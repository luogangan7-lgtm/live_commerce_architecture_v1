package foundation_test

// MA03 / MA08 (contracts/meta-ads-v1.md §9 rows MA03, MA08; AD8, AD10, §3 CAPI bullet, §4.3, §6.1 CAPI Check, §6.4 sweeper, §7 feed;
// F14-F17; customers-billing-v1 CD5 and ads-capi C2..C7). Tier MOCK + REAL_PG: the CAPTURED fact comes from the real payments
// capture path (payments.apply_capture over a recorded observation), consent through the real buyer HTTP handler
// (buyerhttp -> customers.BuyerSetConsent -> attribution.PutCAPIContext), the operation from the real 5-minute sweeper, and the
// event through the real dispatcher + capiroute + metaads.Client against the fake Graph.
// Disclosed owner-pool fixtures: the attempt's execution_profile/environment before the observation is recorded (the fixture
// order is a PROVIDER_MOCK/SANDBOX attempt; SANDBOX and LIVE profiles need the flag), aged fact time (replica mode), erased-owner
// flip through the buyer erasure route (real), grant revocation, SET LOCAL ROLE probes.
//
// Contract vs CD5 (recorded, not hidden): §6.4 says user_data carries `ph` when normalizable; customers-billing CD5 (frozen, "to
// mirror in meta-ads") says the destination phone may be used only when checkout records recipient = buyer, else CAPI omits `ph`.
// Nothing records that, so this gate asserts `ph` is ABSENT and that neither the raw nor the hashed phone reaches PG (outside
// storefront.destination_snapshots), logs or the wire.

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/csv"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"regexp"
	"sort"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"livecommerce/internal/attribution"
	"livecommerce/internal/buyerhttp"
	integration "livecommerce/internal/integrations/core"
	"livecommerce/tests/ads/fakegraph"
)

type capiEnv struct {
	*adsEnv
	p      pqFixture
	bh     bhHarness
	ua     string
	origin string
}

// newCapiEnv builds the ads surface on the payment fixture's private store (one paid-order candidate, owner O, attempt A).
func newCapiEnv(t *testing.T, o adsOpts) *capiEnv {
	t.Helper()
	p := pqSetupItems(t, false, 1)
	o.fx = p.f
	o.dataset = true
	e := newAdsEnv(t, o)
	c := &capiEnv{adsEnv: e, p: p, ua: "Mozilla/5.0 (synthetic-ads-tests) SENTINEL-UA-" + t04Tag(), origin: "https://capi-" + t04Tag() + ".example.test"}
	if o.origin != "" {
		c.origin = o.origin
	}
	bhPublish(t, p.bcHarness, c.origin, p.f.tenantA, p.f.storeA1)
	key := base64.RawURLEncoding.EncodeToString(randomBytes(32))
	handler, err := buyerhttp.New(context.Background(), p.a.issuer, p.a.runtime, p.bcHarness.service, key, time.Hour)
	if err != nil {
		t.Fatalf("buyerhttp.New: %v", err)
	}
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	c.bh = bhHarness{bcHarness: p.bcHarness, key: key, origin: c.origin, server: srv}
	return c
}

// setCapi enables/disables CAPI through the merchant API (ads.set_capi).
func (c *capiEnv) setCapi(enabled bool) adsResp {
	body := map[string]any{"enabled": enabled}
	if enabled {
		body["dataset_binding_id"] = c.dsBinding
		if !c.opts.live {
			body["test_event_code"] = "TEST12345"
		}
	}
	return c.api("PUT", "/capi", c.token, adsKey(), body)
}

// consent PUTs the buyer's ads_personalization consent (the real route, with the browser user agent).
func (c *capiEnv) consent(granted bool) bhResponse {
	c.t.Helper()
	body := map[string]any{"purpose": "ads_personalization", "channel": "meta_ads", "granted": granted, "context": "settings"}
	return c.bh.request(c.t, "PUT", "/v1/buyer/consents", c.p.cap.Token, t04Key("capi-consent"), body, func(r *http.Request) { r.Header.Set("User-Agent", c.ua) })
}

// capture runs the REAL capture path for the fixture order: profile/environment set on the attempt (owner, disclosed), one claimed
// query observation, payments.apply_capture. It returns the fact row's environment/profile.
func (c *capiEnv) capture(profile, environment string) {
	c.t.Helper()
	q := c.p
	mustExec(c.t, q.f.owner, `UPDATE checkout.payment_attempts SET execution_profile=$2,environment=$3 WHERE id=$1`, q.result.AttemptID, profile, environment)
	// The attempt's payment_query_v1 job was routed by the OLD profile. Move it with the profile (owner, replica: the family
	// guard forbids a queue rewrite), as payment_runtime_test does; otherwise integration.payment_queue_ready() stays false for
	// every later test in the shared fixture DB (SP06 "payment queue readiness" failed after this file, 2026-09-30).
	c.ownerReplica(`UPDATE river_payment.river_job j SET queue=integration.payment_job_queue(j.id)
		FROM checkout.payment_attempts a WHERE a.id=$1 AND j.id=a.job_id AND j.queue IS DISTINCT FROM integration.payment_job_queue(j.id)`, q.result.AttemptID)
	var aligned bool
	if err := q.f.owner.QueryRow(c.ctx, `SELECT NOT EXISTS(SELECT 1 FROM river_payment.river_job j WHERE j.args->>'operation_id'=$1
		AND j.state NOT IN ('completed','cancelled','discarded') AND j.queue IS DISTINCT FROM integration.payment_job_queue(j.id))`, q.result.AttemptID).Scan(&aligned); err != nil || !aligned {
		c.t.Fatalf("fixture attempt %s left a payment job off its profile queue (aligned=%v err=%v)", q.result.AttemptID, aligned, err)
	}
	claim := q.claim(c.t)
	report := pcFull(q)
	if err := pqRecord(q.worker, q.result.OperationID, claim, profile, report); err != nil {
		c.t.Fatalf("record observation (%s/%s): %v", profile, environment, err)
	}
	hash := pcHash(c.t, q, report)
	if err := pcApply(q.worker, q.result.AttemptID, hash); err != nil {
		c.t.Fatalf("payments.apply_capture: %v", err)
	}
	var kind, env, prof string
	if err := q.f.owner.QueryRow(c.ctx, `SELECT kind,environment,execution_profile FROM payments.facts WHERE attempt_id=$1 AND kind='CAPTURED'`, q.result.AttemptID).Scan(&kind, &env, &prof); err != nil {
		c.t.Fatalf("no CAPTURED fact after apply_capture: %v", err)
	}
	if env != environment || prof != profile {
		c.t.Fatalf("fact %s/%s, want %s/%s", env, prof, environment, profile)
	}
}

func (c *capiEnv) capiOps() []adsOp {
	c.t.Helper()
	rows, err := c.f.owner.Query(c.ctx, `SELECT o.id::text,'capi',o.action,o.state,o.result_code,o.provider_reference,coalesce(j.state::text,''),coalesce(j.queue,''),1,1,coalesce(j.priority,0),o.job_id,o.generation
		FROM ads.capi_events c JOIN integration.operations o ON o.id=c.operation_id LEFT JOIN river.river_job j ON j.id=o.job_id WHERE c.store_id=$1 ORDER BY c.planned_at`, c.store)
	if err != nil {
		c.t.Fatal(err)
	}
	defer rows.Close()
	var out []adsOp
	for rows.Next() {
		var o adsOp
		if err := rows.Scan(&o.ID, &o.Kind, &o.Action, &o.State, &o.Code, &o.Ref, &o.JobState, &o.Queue, &o.Seq, &o.Attempt, &o.Priority, &o.Job, &o.Generation); err != nil {
			c.t.Fatal(err)
		}
		out = append(out, o)
	}
	return out
}

func (c *capiEnv) contextRows() int64 {
	return c.count(`SELECT count(*) FROM ads.capi_contexts WHERE store_id=$1`, c.store)
}

// events are the POST /{pixel}/events requests the fake received.
func (c *capiEnv) events() []fakegraph.Req {
	var out []fakegraph.Req
	for _, r := range c.g.Requests() {
		if r.Route == fakegraph.RouteEvents {
			out = append(out, r)
		}
	}
	return out
}

// primed = capture + consent + CAPI on, ready for the sweeper.
func (c *capiEnv) prime(profile, environment string) {
	c.t.Helper()
	if r := c.setCapi(true); r.Status != 200 {
		c.t.Fatalf("PUT capi: %d %s", r.Status, r.Raw)
	}
	c.capture(profile, environment)
	if r := c.consent(true); r.status != 200 {
		c.t.Fatalf("buyer consent: %d %s", r.status, r.body)
	}
}

// ---------------------------------------------------------------------------------------------------------------------
// MA03
// ---------------------------------------------------------------------------------------------------------------------

func TestMetaAdsMA03Consent(t *testing.T) {
	t.Run("no consent: no op, no row, no event", func(t *testing.T) {
		c := newCapiEnv(t, adsOpts{})
		if r := c.setCapi(true); r.Status != 200 {
			t.Fatalf("PUT capi %d %s", r.Status, r.Raw)
		}
		c.capture("SANDBOX", "SANDBOX")
		c.sweep("capi")
		c.settle()
		if n := len(c.capiOps()); n != 0 || len(c.events()) != 0 || c.contextRows() != 0 {
			t.Fatalf("ops=%d events=%d contexts=%d without consent (G09/I07)", n, len(c.events()), c.contextRows())
		}
	})

	t.Run("granted: exactly one op per attempt, stable event_id and key, merchant actor + capi_enabled_by, queue ads, one event", func(t *testing.T) {
		c := newCapiEnv(t, adsOpts{})
		c.prime("SANDBOX", "SANDBOX")
		for i := 0; i < 3; i++ {
			c.sweep("capi")
			c.settle()
		}
		ops := c.capiOps()
		if len(ops) != 1 || ops[0].State != "SUCCEEDED" {
			t.Fatalf("capi ops %+v%s", ops, c.dump())
		}
		var key, provider, purpose, actor, principal, eventID, reqJSON string
		if err := c.f.owner.QueryRow(c.ctx, `SELECT o.semantic_key,o.provider,o.purpose,o.actor_kind,o.principal_id::text,e.event_id,o.request::text
			FROM ads.capi_events e JOIN integration.operations o ON o.id=e.operation_id WHERE e.store_id=$1`, c.store).Scan(&key, &provider, &purpose, &actor, &principal, &eventID, &reqJSON); err != nil {
			t.Fatal(err)
		}
		attempt := c.p.result.AttemptID
		if key != "ads:capi:"+attempt || provider != "meta_dataset" || purpose != "marketing" || actor != "MERCHANT" || principal != c.creator || eventID != "lc-purchase-"+attempt {
			t.Errorf("op shape: key=%s provider=%s purpose=%s actor=%s principal=%s event_id=%s", key, provider, purpose, actor, principal, eventID)
		}
		if ops[0].Queue != "ads" || ops[0].Priority != 3 || ops[0].Action != "meta.capi.purchase" {
			t.Errorf("queue/priority/action: %s/%d/%s", ops[0].Queue, ops[0].Priority, ops[0].Action)
		}
		var req map[string]any
		_ = json.Unmarshal([]byte(reqJSON), &req)
		for k := range req {
			if k != "v" && k != "attempt_id" && k != "event_id" && k != "event_time" && k != "test_event_code" {
				t.Errorf("frozen request carries key %q (no PII, no tokens: §6.1)", k)
			}
		}
		if strings.Contains(reqJSON, c.ua) || strings.Contains(reqJSON, "@") {
			t.Errorf("frozen request carries user data: %s", reqJSON)
		}
		if n := len(c.events()); n != 1 {
			t.Fatalf("%d events sent for one attempt", n)
		}
		if n := c.count(`SELECT count(*) FROM ads.capi_events WHERE store_id=$1`, c.store); n != 1 {
			t.Errorf("%d capi_events rows (one per attempt, ever)", n)
		}
	})

	t.Run("consent withdrawn before the sweeper: no op and the context row is deleted", func(t *testing.T) {
		c := newCapiEnv(t, adsOpts{})
		c.prime("SANDBOX", "SANDBOX")
		if c.contextRows() != 1 {
			t.Fatal("no context after grant")
		}
		if r := c.consent(false); r.status != 200 {
			t.Fatalf("withdraw: %d %s", r.status, r.body)
		}
		c.sweep("capi")
		c.settle()
		if len(c.capiOps()) != 0 || len(c.events()) != 0 {
			t.Fatalf("withdrawn consent produced ops=%d events=%d", len(c.capiOps()), len(c.events()))
		}
		if c.contextRows() != 0 {
			t.Fatal("the sweeper did not delete the context row of a withdrawn consent (§4.3)")
		}
	})

	t.Run("owner erased: no op and the context row is deleted", func(t *testing.T) {
		c := newCapiEnv(t, adsOpts{})
		c.prime("SANDBOX", "SANDBOX")
		r := c.bh.request(t, "POST", "/v1/buyer/privacy/erasure", c.p.cap.Token, t04Key("capi-erase"), map[string]any{"confirm": "ERASE"}, nil)
		if r.status != 200 {
			t.Fatalf("erasure: %d %s", r.status, r.body)
		}
		c.sweep("capi")
		c.settle()
		if len(c.capiOps()) != 0 || len(c.events()) != 0 || c.contextRows() != 0 {
			t.Fatalf("erased owner: ops=%d events=%d contexts=%d", len(c.capiOps()), len(c.events()), c.contextRows())
		}
	})

	t.Run("withdraw after planning: Check answers BLOCKED_POLICY consent_withdrawn with zero HTTP (G09)", func(t *testing.T) {
		c := newCapiEnv(t, adsOpts{})
		c.prime("SANDBOX", "SANDBOX")
		c.pauseDispatch()
		c.sweep("capi")
		ops := c.capiOps()
		if len(ops) != 1 || ops[0].State != "READY" {
			t.Fatalf("expected one READY CAPI op: %+v", ops)
		}
		if r := c.consent(false); r.status != 200 {
			t.Fatalf("withdraw %d", r.status)
		}
		mark := c.g.Mark()
		c.resumeDispatch()
		c.settle()
		c.wantBlocked(c.capiOps()[0], "consent_withdrawn")
		if n := len(c.g.RequestsSince(mark)); n != 0 {
			t.Fatalf("%d Graph requests after the withdrawal", n)
		}
		if len(c.events()) != 0 {
			t.Fatal("an event was sent after the withdrawal")
		}
	})

	t.Run("fact environment must equal the store environment and PROVIDER_MOCK facts are never sent", func(t *testing.T) {
		live := newCapiEnv(t, adsOpts{live: true})
		live.prime("SANDBOX", "SANDBOX") // a SANDBOX fact in a LIVE store
		live.sweep("capi")
		live.settle()
		if len(live.capiOps()) != 0 || len(live.events()) != 0 {
			t.Fatalf("SANDBOX fact in a LIVE store produced ops=%d events=%d (F17-ish: §6.4 environment filter)", len(live.capiOps()), len(live.events()))
		}
	})
	t.Run("PROVIDER_MOCK fact in a matching environment is skipped", func(t *testing.T) {
		mock := newCapiEnv(t, adsOpts{})
		mock.prime("PROVIDER_MOCK", "SANDBOX")
		mock.sweep("capi")
		mock.settle()
		if len(mock.capiOps()) != 0 || len(mock.events()) != 0 {
			t.Fatalf("PROVIDER_MOCK fact produced ops=%d events=%d (§6.4: execution_profile <> PROVIDER_MOCK)", len(mock.capiOps()), len(mock.events()))
		}
	})
	// NOT_RUN: a LIVE-profile captured fact (the "no test_event_code in LIVE" half of AD9) needs a LIVE PSP qualification the
	// disposable fixture cannot mint (payment_attempts FK to a LIVE qualification); fabricating a LIVE facts row is forbidden.
	// The SANDBOX half (`test_event_code` present) is asserted in MA08.

	checkDenial := func(name, code string, mutate func(c *capiEnv), zeroHTTP bool, states ...string) {
		t.Run(name, func(t *testing.T) {
			c := newCapiEnv(t, adsOpts{})
			c.prime("SANDBOX", "SANDBOX")
			c.pauseDispatch()
			c.sweep("capi")
			if ops := c.capiOps(); len(ops) != 1 || ops[0].State != "READY" {
				t.Fatalf("expected one READY CAPI op: %+v", ops)
			}
			mutate(c)
			mark := c.g.Mark()
			c.resumeDispatch()
			c.settle()
			o := c.capiOps()[0]
			if len(states) > 0 {
				ok := false
				for _, s := range states {
					ok = ok || o.State == s
				}
				if !ok {
					t.Fatalf("op %s/%s, want one of %v", o.State, o.Code, states)
				}
			} else {
				c.wantBlocked(o, code)
			}
			if zeroHTTP && len(c.g.RequestsSince(mark)) != 0 {
				t.Fatalf("%d Graph requests for a denied CAPI op", len(c.g.RequestsSince(mark)))
			}
		})
	}
	checkDenial("capi_enabled_by lost ads:manage -> capi_principal_revoked", "capi_principal_revoked", func(c *capiEnv) { c.revoke(c.creator, "ads:manage") }, true)
	checkDenial("CAPI switched off after planning -> capi_disabled", "capi_disabled", func(c *capiEnv) {
		if r := c.api("PUT", "/capi", c.token, adsKey(), map[string]any{"enabled": false}); r.Status != 200 {
			c.t.Fatalf("disable: %d %s", r.Status, r.Raw)
		}
	}, true)
	checkDenial("op that sat READY for 7 days (frozen event_time aged, hash kept consistent) -> event_too_old", "event_too_old", func(c *capiEnv) {
		old := time.Now().Add(-7 * 24 * time.Hour).Unix()
		c.ownerReplica(`UPDATE integration.operations SET request=jsonb_set(request,'{event_time}',to_jsonb($2::bigint)),
			request_hash=sha256(convert_to(jsonb_set(request,'{event_time}',to_jsonb($2::bigint))::text,'UTF8')) WHERE id=$1`, c.capiOps()[0].ID, old)
	}, true)
	checkDenial("dataset binding re-pointed after planning: STALE_BINDING, zero HTTP", "", func(c *capiEnv) { c.disableBinding(c.dsBinding) }, true, "STALE_BINDING", "BLOCKED_POLICY")

	t.Run("billing RESTRICTED never blocks CAPI (BD5)", func(t *testing.T) {
		c := newCapiEnv(t, adsOpts{})
		c.prime("SANDBOX", "SANDBOX")
		cbxRestrict(t, c.f, mustStripeIngress(t, c.f), c.tenant, c.store)
		c.sweep("capi")
		c.settle()
		if ops := c.capiOps(); len(ops) != 1 || ops[0].State != "SUCCEEDED" {
			t.Fatalf("RESTRICTED store: %+v%s", ops, c.dump())
		}
	})

	t.Run("a fact older than 6 days is never planned; a context older than 7 days does not make an owner eligible; 8-day contexts are purged", func(t *testing.T) {
		c := newCapiEnv(t, adsOpts{})
		c.prime("SANDBOX", "SANDBOX")
		c.ownerReplica(`UPDATE payments.facts SET received_at=received_at-interval '7 days' WHERE attempt_id=$1`, c.p.result.AttemptID)
		c.sweep("capi")
		c.settle()
		if len(c.capiOps()) != 0 {
			t.Fatal("a 7-day-old fact was planned (event_time > 6 days is rejected by Meta, F14)")
		}
		c2 := newCapiEnv(t, adsOpts{})
		c2.prime("SANDBOX", "SANDBOX")
		c2.ownerReplica(`UPDATE ads.capi_contexts SET captured_at=captured_at-interval '7 days 1 hour' WHERE store_id=$1`, c2.store)
		c2.sweep("capi")
		c2.settle()
		if len(c2.capiOps()) != 0 {
			t.Fatal("a context captured more than 7 days ago still made the owner eligible (§6.4)")
		}
		c2.ownerReplica(`UPDATE ads.capi_contexts SET captured_at=captured_at-interval '2 days' WHERE store_id=$1`, c2.store)
		c2.sweep("capi")
		if c2.contextRows() != 0 {
			t.Fatal("contexts older than 8 days were not purged")
		}
	})

	t.Run("consent without a browser context (no user agent): not eligible", func(t *testing.T) {
		c := newCapiEnv(t, adsOpts{})
		c.prime("SANDBOX", "SANDBOX")
		mustExec(t, c.f.owner, `DELETE FROM ads.capi_contexts WHERE store_id=$1`, c.store)
		c.sweep("capi")
		c.settle()
		if len(c.capiOps()) != 0 || len(c.events()) != 0 {
			t.Fatalf("event sent without a client_user_agent (F14: website events need one): ops=%d", len(c.capiOps()))
		}
	})

	t.Run("CAPI off or no dataset: nothing is planned", func(t *testing.T) {
		c := newCapiEnv(t, adsOpts{})
		c.capture("SANDBOX", "SANDBOX")
		if r := c.consent(true); r.status != 200 {
			t.Fatalf("consent %d", r.status)
		}
		c.sweep("capi")
		c.settle()
		if len(c.capiOps()) != 0 {
			t.Fatal("planned although CAPI is not enabled for the store")
		}
		// the merchant cannot enable CAPI for a dataset binding that is not one of the store's meta_dataset bindings
		if r := c.api("PUT", "/capi", c.token, adsKey(), map[string]any{"enabled": true, "dataset_binding_id": c.adBinding, "test_event_code": "TEST12345"}); r.Status < 400 {
			t.Errorf("CAPI enabled on an ad-account binding: %d", r.Status)
		}
		if r := c.api("PUT", "/capi", c.token, adsKey(), map[string]any{"enabled": true, "dataset_binding_id": randomUUID(), "test_event_code": "TEST12345"}); r.Status < 400 {
			t.Errorf("CAPI enabled on an unknown binding: %d", r.Status)
		}
		if r := c.api("PUT", "/capi", c.token, adsKey(), map[string]any{"enabled": true, "dataset_binding_id": c.dsBinding}); r.Status < 400 {
			t.Errorf("SANDBOX store enabled CAPI without a test_event_code (§4.1 CHECK): %d", r.Status)
		}
	})
}

// ---------------------------------------------------------------------------------------------------------------------
// MA08
// ---------------------------------------------------------------------------------------------------------------------

func keysOf(m map[string]any) string {
	var ks []string
	for k := range m {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return strings.Join(ks, ",")
}

// phoneOfOrder reads the order's recipient phone (owner read-back of a column no ads role may read).
func (c *capiEnv) phoneOfOrder() string {
	var phone string
	err := c.f.owner.QueryRow(c.ctx, `SELECT d.phone FROM checkout.orders o JOIN storefront.destination_snapshots d ON d.tenant_id=o.tenant_id AND d.store_id=o.store_id AND d.id=o.destination_id
		JOIN checkout.payment_attempts a ON a.order_id=o.id WHERE a.id=$1`, c.p.result.AttemptID).Scan(&phone)
	if err != nil {
		c.t.Fatalf("recipient phone of the fixture order: %v", err)
	}
	return phone
}

func TestMetaAdsMA08CAPI(t *testing.T) {
	t.Run("wire body: exact keys, values, hashing, SANDBOX test_event_code, no phone (CD5)", func(t *testing.T) {
		c := newCapiEnv(t, adsOpts{})
		c.prime("SANDBOX", "SANDBOX")
		// Deterministic rounding probe (r3 P2): a fraction >= .5 s makes a rounding cast land one second ahead of the fact.
		c.ownerReplica(`UPDATE payments.facts SET received_at=date_trunc('second',received_at)+interval '900 milliseconds' WHERE attempt_id=$1 AND kind='CAPTURED'`, c.p.result.AttemptID)
		var received time.Time
		var amount int64
		var owner string
		if err := c.f.owner.QueryRow(c.ctx, `SELECT f.received_at,f.amount_minor,a.owner_id::text FROM payments.facts f JOIN checkout.payment_attempts a ON a.id=f.attempt_id WHERE f.attempt_id=$1 AND f.kind='CAPTURED'`, c.p.result.AttemptID).Scan(&received, &amount, &owner); err != nil {
			t.Fatal(err)
		}
		c.sweep("capi")
		c.settle()
		evs := c.events()
		if len(evs) != 1 {
			t.Fatalf("%d events%s", len(evs), c.dump())
		}
		ev := evs[0]
		if !strings.HasSuffix(ev.Path, "/events") || ev.Path != c.pixel+"/events" || ev.Method != http.MethodPost {
			t.Errorf("request %s %s, want POST /{pixel_id}/events of the store's dataset binding", ev.Method, ev.Path)
		}
		if ev.TokenSource == "query" || strings.Contains(ev.RawQuery, "access_token") {
			t.Errorf("token in the URL")
		}
		var body map[string]any
		if err := json.Unmarshal(ev.Body, &body); err != nil {
			t.Fatal(err)
		}
		delete(body, "access_token") // how Graph takes the token in a JSON POST; asserted not to be in the URL above
		if got := keysOf(body); got != "data,partner_agent,test_event_code" {
			t.Errorf("top-level keys %s, want data,partner_agent,test_event_code (SANDBOX)", got)
		}
		if body["partner_agent"] != adsPartner || body["test_event_code"] != "TEST12345" {
			t.Errorf("partner_agent %v test_event_code %v", body["partner_agent"], body["test_event_code"])
		}
		data, _ := body["data"].([]any)
		if len(data) != 1 {
			t.Fatalf("data has %d events, want exactly one", len(data))
		}
		e0, _ := data[0].(map[string]any)
		if got := keysOf(e0); got != "action_source,custom_data,event_id,event_name,event_source_url,event_time,user_data" {
			t.Errorf("event keys %s", got)
		}
		if e0["event_name"] != "Purchase" || e0["action_source"] != "website" || e0["event_id"] != "lc-purchase-"+c.p.result.AttemptID {
			t.Errorf("event_name/action_source/event_id: %v %v %v", e0["event_name"], e0["action_source"], e0["event_id"])
		}
		// Contract: event_time = the fact's received_at in unix seconds, floored (0080 floors; a rounding cast was 0.5s early-ahead, r3 P2).
		if tm, _ := e0["event_time"].(float64); int64(tm) != received.Unix() {
			t.Errorf("event_time %v, want the fact's received_at %d exactly", e0["event_time"], received.Unix())
		}
		if e0["event_source_url"] != c.origin+"/orders" {
			t.Errorf("event_source_url %v, want %s/orders", e0["event_source_url"], c.origin)
		}
		ud, _ := e0["user_data"].(map[string]any)
		if got := keysOf(ud); got != "client_user_agent,external_id" {
			t.Errorf("user_data keys %s: `ph` must be omitted while checkout does not record recipient = buyer (CD5); no fbc/fbp/email", got)
		}
		if ud["client_user_agent"] != c.ua {
			t.Errorf("client_user_agent %v, want the UA stored by the consent hook", ud["client_user_agent"])
		}
		// C3: external_id = hex(SHA-256(hex(HMAC-SHA256(K, "capi-external-id/v1|tenant|store|owner")))), K = worker key; Graph takes an array
		mac := hmac.New(sha256.New, []byte(adsExternalKey))
		mac.Write([]byte("capi-external-id/v1|" + c.tenant + "|" + c.store + "|" + owner))
		inner := sha256.Sum256([]byte(hex.EncodeToString(mac.Sum(nil))))
		wantExt := hex.EncodeToString(inner[:])
		ext, _ := ud["external_id"].([]any)
		if len(ext) != 1 || ext[0] != wantExt {
			t.Errorf("external_id %v, want [%s]", ud["external_id"], wantExt)
		}
		if strings.Contains(string(ev.Body), owner) {
			t.Errorf("the internal owner uuid is on the wire")
		}
		cd, _ := e0["custom_data"].(map[string]any)
		if got := keysOf(cd); got != "content_type,contents,currency,value" {
			t.Errorf("custom_data keys %s", got)
		}
		if cd["currency"] != "TWD" || cd["content_type"] != "product" {
			t.Errorf("currency/content_type %v %v", cd["currency"], cd["content_type"])
		}
		// I05: value = amount_minor/100 as an exact decimal (no float artefacts)
		wantValue := fmt.Sprintf("%d.%02d", amount/100, amount%100)
		rawValue := regexp.MustCompile(`"value":([0-9]+(?:\.[0-9]+)?)`).FindSubmatch(ev.Body)
		if len(rawValue) != 2 || strings.TrimRight(strings.TrimRight(string(rawValue[1]), "0"), ".") != strings.TrimRight(strings.TrimRight(wantValue, "0"), ".") {
			t.Errorf("value on the wire %q, want decimal %s", rawValue, wantValue)
		}
		contents, _ := cd["contents"].([]any)
		if len(contents) != 1 {
			t.Fatalf("contents %v", cd["contents"])
		}
		c0, _ := contents[0].(map[string]any)
		if keysOf(c0) != "id,quantity" || c0["id"] != c.p.stock.skus[0].ID || c0["quantity"] != float64(2) {
			t.Errorf("contents[0] %v, want id = the catalog id the feed publishes (%s), quantity 2", c0, c.p.stock.skus[0].ID)
		}
		// the phone: neither raw nor hashed on the wire
		phone := c.phoneOfOrder()
		digits := strings.TrimPrefix(phone, "+")
		h, _ := attribution.HashPhone(phone)
		if strings.Contains(string(ev.Body), digits) || (h != "" && strings.Contains(string(ev.Body), h)) {
			t.Errorf("recipient phone (raw or hashed) on the wire")
		}
	})

	t.Run("user data only through the lease-fenced definer ads.capi_user_data; commerce_worker cannot read the source tables", func(t *testing.T) {
		c := newCapiEnv(t, adsOpts{})
		c.prime("SANDBOX", "SANDBOX")
		var results []string
		var once atomic.Bool
		foreign := c.foreignOp()
		c.onLoad = func(cl integration.SecretClaim) {
			if !once.CompareAndSwap(false, true) {
				return
			}
			probe := func(name, op string, gen int64, token []byte) {
				var ph *string
				var owner *string
				var n int
				rows, err := c.workerPool.Query(c.ctx, `SELECT ph_e164,owner_id::text FROM ads.capi_user_data($1::uuid,$2,$3)`, op, gen, token)
				if err == nil {
					for rows.Next() {
						n++
						_ = rows.Scan(&ph, &owner)
					}
					err = rows.Err()
					rows.Close()
				}
				results = append(results, fmt.Sprintf("%s|%v|%d|%v", name, sqlStateOf(err), n, ph != nil))
			}
			wrong := append([]byte(nil), cl.LeaseToken...)
			wrong[0] ^= 0xff
			probe("valid", cl.OperationID, cl.Generation, cl.LeaseToken)
			probe("stale generation", cl.OperationID, cl.Generation+1, cl.LeaseToken)
			probe("wrong token", cl.OperationID, cl.Generation, wrong)
			probe("non-CAPI operation", foreign, cl.Generation, cl.LeaseToken)
			probe("unknown operation", randomUUID(), cl.Generation, cl.LeaseToken)
			tx, _ := c.f.owner.Begin(c.ctx)
			_, _ = tx.Exec(c.ctx, `SET LOCAL session_replication_role=replica`)
			var until time.Time
			_ = tx.QueryRow(c.ctx, `SELECT lease_until FROM integration.operations WHERE id=$1`, cl.OperationID).Scan(&until)
			_, _ = tx.Exec(c.ctx, `UPDATE integration.operations SET lease_until=clock_timestamp()-interval '1 second' WHERE id=$1`, cl.OperationID)
			_ = tx.Commit(c.ctx)
			probe("expired lease", cl.OperationID, cl.Generation, cl.LeaseToken)
			tx, _ = c.f.owner.Begin(c.ctx)
			_, _ = tx.Exec(c.ctx, `SET LOCAL session_replication_role=replica`)
			_, _ = tx.Exec(c.ctx, `UPDATE integration.operations SET lease_until=$2 WHERE id=$1`, cl.OperationID, until)
			_ = tx.Commit(c.ctx)
		}
		c.sweep("capi")
		c.settle()
		want := map[string]string{
			"valid": "<nil>|1|false", "stale generation": "40001|0|false", "wrong token": "40001|0|false", "non-CAPI operation": "P0002|0|false",
			"unknown operation": "P0002|0|false", "expired lease": "40001|0|false",
		}
		got := map[string]string{}
		for _, r := range results {
			parts := strings.SplitN(r, "|", 2)
			got[parts[0]] = parts[1]
		}
		for name, w := range want {
			if got[name] != w {
				t.Errorf("ads.capi_user_data %s -> %q, want %q (ph_e164 is always NULL: CD5)", name, got[name], w)
			}
		}
		// after the operation completed, the same claim is refused (no longer DISPATCHING) — the definer is not a standing read path
		var stale int
		var err error
		if ld := c.probe.Loads(); len(ld) > 0 {
			cl := ld[len(ld)-1].Claim
			err = c.workerPool.QueryRow(c.ctx, `SELECT count(*) FROM ads.capi_user_data($1::uuid,$2,$3)`, cl.OperationID, cl.Generation, cl.LeaseToken).Scan(&stale)
			if err == nil {
				t.Errorf("a finished operation's claim still reads user data (%d rows)", stale)
			}
		}
		// worker has no table access to the user-data sources
		for _, rel := range []string{"storefront.destination_snapshots", "ads.capi_contexts", "ads.capi_events", "checkout.orders", "checkout.payment_attempts", "storefront.quotes", "customers.consent_events", "buyer.owners"} {
			var tbl, col bool
			if err := c.f.owner.QueryRow(c.ctx, `SELECT has_table_privilege('commerce_worker',$1::regclass,'SELECT'),has_any_column_privilege('commerce_worker',$1::regclass,'SELECT')`, rel).Scan(&tbl, &col); err != nil {
				t.Fatal(err)
			}
			if tbl || col {
				t.Errorf("commerce_worker can SELECT %s (C2: user data only through the lease-fenced definer)", rel)
			}
		}
		for _, q := range []string{`SELECT count(*) FROM storefront.destination_snapshots`, `SELECT count(*) FROM ads.capi_contexts`, `SELECT count(*) FROM checkout.orders`} {
			if _, err := c.workerPool.Exec(c.ctx, q); !pgcode(err, "42501") {
				t.Errorf("worker %q -> %v, want 42501", q, err)
			}
		}
	})

	t.Run("hashed fields never reach PG or logs; raw phone only where the order snapshot lives (sentinel scan)", func(t *testing.T) {
		logs := captureSlog(t)
		c := newCapiEnv(t, adsOpts{})
		c.prime("SANDBOX", "SANDBOX")
		c.sweep("capi")
		c.settle()
		evs := c.events()
		if len(evs) != 1 {
			t.Fatalf("%d events", len(evs))
		}
		var owner string
		_ = c.f.owner.QueryRow(c.ctx, `SELECT owner_id::text FROM checkout.payment_attempts WHERE id=$1`, c.p.result.AttemptID).Scan(&owner)
		phone := c.phoneOfOrder()
		digits := strings.TrimPrefix(phone, "+")
		hashedPhone, _ := attribution.HashPhone(phone)
		mac := hmac.New(sha256.New, []byte(adsExternalKey))
		mac.Write([]byte("capi-external-id/v1|" + c.tenant + "|" + c.store + "|" + owner))
		inner := sha256.Sum256([]byte(hex.EncodeToString(mac.Sum(nil))))
		extHash := hex.EncodeToString(inner[:])
		hmacHex := hex.EncodeToString(mac.Sum(nil))
		needles := map[string]string{"hashed phone": hashedPhone, "external_id hash": extHash, "HMAC inner hex": hmacHex, "BISU token plaintext": c.botToken, "app secret": adsAppSecret, "external-id key": adsExternalKey}
		found := map[string][]string{}
		rows, err := c.f.owner.Query(c.ctx, `SELECT n.nspname,c.relname FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace WHERE c.relkind='r' AND n.nspname NOT IN ('pg_catalog','information_schema') AND n.nspname NOT LIKE 'pg_toast%' ORDER BY 1,2`)
		if err != nil {
			t.Fatal(err)
		}
		var tables [][2]string
		for rows.Next() {
			var s, r string
			_ = rows.Scan(&s, &r)
			tables = append(tables, [2]string{s, r})
		}
		rows.Close()
		scanned := 0
		for _, tb := range tables {
			id := pgx.Identifier{tb[0], tb[1]}.Sanitize()
			for name, needle := range needles {
				if needle == "" {
					continue
				}
				var n int
				if err := c.f.owner.QueryRow(c.ctx, `SELECT count(*) FROM `+id+` t WHERE t::text LIKE '%'||$1||'%'`, needle).Scan(&n); err != nil {
					t.Fatalf("scan %s: %v", id, err)
				}
				if n > 0 {
					found[name] = append(found[name], tb[0]+"."+tb[1])
				}
			}
			scanned++
		}
		if scanned < 100 {
			t.Fatalf("only %d tables scanned: the scan is not DB-wide", scanned)
		}
		for name, where := range found {
			if name == "app secret" || name == "external-id key" || name == "hashed phone" || name == "external_id hash" || name == "HMAC inner hex" || name == "BISU token plaintext" {
				t.Errorf("%s found in PG: %v (never persisted, hashed in memory only / sealed)", name, where)
			}
		}
		// raw phone + UA: may live only in their designated homes
		for _, tb := range tables {
			id := pgx.Identifier{tb[0], tb[1]}.Sanitize()
			var n int
			if digits != "" && len(digits) >= 8 {
				_ = c.f.owner.QueryRow(c.ctx, `SELECT count(*) FROM `+id+` t WHERE t::text LIKE '%'||$1||'%'`, digits).Scan(&n)
				if n > 0 && (tb[0] == "ads" || tb[0] == "integration" || tb[0] == "ops" || strings.HasPrefix(tb[0], "river") || tb[0] == "customers" || tb[0] == "billing") {
					t.Errorf("raw recipient phone found in %s.%s", tb[0], tb[1])
				}
			}
			_ = c.f.owner.QueryRow(c.ctx, `SELECT count(*) FROM `+id+` t WHERE t::text LIKE '%'||$1||'%'`, c.ua).Scan(&n)
			if n > 0 && !(tb[0] == "ads" && tb[1] == "capi_contexts") {
				t.Errorf("browser user agent found in %s.%s (only ads.capi_contexts may hold it)", tb[0], tb[1])
			}
		}
		text := logs.String()
		for name, needle := range needles {
			if needle != "" && strings.Contains(text, needle) {
				t.Errorf("%s in the process log", name)
			}
		}
		for name, needle := range map[string]string{"raw phone": digits, "user agent": c.ua, "owner id": owner} {
			if needle != "" && strings.Contains(text, needle) {
				t.Errorf("%s in the process log", name)
			}
		}
	})

	for name, fl := range map[string]fakegraph.Fault{
		"timeout after the event was received": {Route: fakegraph.RouteEvents, Kind: fakegraph.FaultTimeout, Effect: true, Times: 30},
		"503":                                  {Route: fakegraph.RouteEvents, Kind: fakegraph.Fault5xx, Times: 30},
		"unparseable 200":                      {Route: fakegraph.RouteEvents, Kind: fakegraph.FaultGarbled, Times: 30},
		"200 without events_received":          {Route: fakegraph.RouteEvents, Kind: fakegraph.FaultShapeless, Times: 30},
	} {
		t.Run("UNKNOWN is never resent: "+name, func(t *testing.T) {
			c := newCapiEnv(t, adsOpts{maxGen: 4})
			c.g.Inject(fl)
			c.prime("SANDBOX", "SANDBOX")
			for i := 0; i < 3; i++ {
				c.sweep("capi")
				c.settle()
			}
			ops := c.capiOps()
			if len(ops) != 1 || ops[0].State != "UNKNOWN" {
				t.Fatalf("CAPI op %+v, want one UNKNOWN%s", ops, c.dump())
			}
			if n := len(c.events()); n != 1 {
				t.Fatalf("%d event POSTs after UNKNOWN + reconcile cycles, want exactly 1 (F15: server-to-server dedup undocumented)", n)
			}
			// reconcile of a CAPI op needs no secret: the loader ran once (the dispatch), never for a reconcile claim
			loads := 0
			for _, l := range c.probe.Loads() {
				if l.Op == ops[0].ID {
					loads++
					if l.Lease == "reconcile" {
						t.Errorf("a CAPI reconcile loaded a secret")
					}
				}
			}
			if loads != 1 {
				t.Errorf("%d loader calls for the CAPI op, want 1", loads)
			}
			if n := c.count(`SELECT count(*) FROM ads.capi_events WHERE store_id=$1`, c.store); n != 1 {
				t.Errorf("%d capi_events rows", n)
			}
		})
	}

	t.Run("a Graph 4xx error body ends FAILED_FINAL and is not resent either", func(t *testing.T) {
		c := newCapiEnv(t, adsOpts{})
		c.g.Inject(fakegraph.Fault{Route: fakegraph.RouteEvents, Kind: fakegraph.FaultGraphError, Code: 100, Times: 10})
		c.prime("SANDBOX", "SANDBOX")
		for i := 0; i < 3; i++ {
			c.sweep("capi")
			c.settle()
		}
		ops := c.capiOps()
		if len(ops) != 1 || ops[0].State != "FAILED_FINAL" || len(c.events()) != 1 {
			t.Fatalf("ops %+v events %d", ops, len(c.events()))
		}
	})
}

// ---------------------------------------------------------------------------------------------------------------------
// Feed (AD10, §7, F17)
// ---------------------------------------------------------------------------------------------------------------------

func TestMetaAdsFeed(t *testing.T) {
	c := newCapiEnv(t, adsOpts{noWorker: true})
	handler := attribution.FeedHandler(c.p.a.runtime)
	get := func(method, path string, origins ...string) (int, http.Header, string) {
		req := httptest.NewRequest(method, path, nil)
		for _, o := range origins {
			req.Header.Add("X-Commerce-Storefront-Origin", o)
		}
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, req)
		return w.Code, w.Header(), w.Body.String()
	}
	code, hdr, body := get(http.MethodGet, "/v1/buyer/feeds/meta.csv", c.origin)
	if code != 200 {
		t.Fatalf("feed: %d %s", code, body)
	}
	if !strings.HasPrefix(hdr.Get("Content-Type"), "text/csv") || !strings.Contains(hdr.Get("Cache-Control"), "public") {
		t.Errorf("headers: %v", hdr)
	}
	recs, err := csv.NewReader(strings.NewReader(body)).ReadAll()
	if err != nil || len(recs) < 2 {
		t.Fatalf("csv: %v (%d records)", err, len(recs))
	}
	header := strings.Join(recs[0], ",")
	for _, col := range []string{"id", "title", "description", "availability", "condition", "price", "link"} {
		if !strings.Contains(","+header+",", ","+col+",") {
			t.Errorf("feed lacks column %s: %s (F17 required columns)", col, header)
		}
	}
	idx := func(name string) int {
		for i, h := range recs[0] {
			if h == name {
				return i
			}
		}
		return -1
	}
	sku := c.p.stock.skus[0].ID
	found := false
	for _, r := range recs[1:] {
		if r[idx("id")] == sku {
			found = true
			if !regexp.MustCompile(`^[0-9]+\.[0-9]{2} TWD$`).MatchString(r[idx("price")]) {
				t.Errorf("price %q, want \"<amount> <ISO>\" (exact decimal of minor units)", r[idx("price")])
			}
			if r[idx("condition")] != "new" {
				t.Errorf("condition %q", r[idx("condition")])
			}
			if !strings.HasPrefix(r[idx("link")], c.origin) {
				t.Errorf("link %q not on the verified storefront host %s", r[idx("link")], c.origin)
			}
		}
	}
	if !found {
		t.Errorf("the published SKU %s (the id CAPI contents[].id uses, AD10) is not in the feed", sku)
	}
	// no store parameter, no query string, GET/HEAD only, exactly one origin header, unverified host 404
	if code, _, _ := get(http.MethodGet, "/v1/buyer/feeds/meta.csv?store_id="+c.store, c.origin); code < 400 || code >= 500 {
		t.Errorf("query string accepted: %d", code)
	}
	if code, _, _ := get(http.MethodPost, "/v1/buyer/feeds/meta.csv", c.origin); code != http.StatusMethodNotAllowed {
		t.Errorf("POST: %d", code)
	}
	if code, _, _ := get(http.MethodGet, "/v1/buyer/feeds/meta.csv", c.origin, "https://other.example.test"); code < 400 || code >= 500 {
		t.Errorf("two origin headers: %d", code)
	}
	if code, _, _ := get(http.MethodGet, "/v1/buyer/feeds/meta.csv"); code < 400 || code >= 500 {
		t.Errorf("no origin header: %d", code)
	}
	if code, _, _ := get(http.MethodGet, "/v1/buyer/feeds/meta.csv", "https://unverified-"+t04Tag()+".example.test"); code != 404 {
		t.Errorf("unverified host: %d, want 404", code)
	}
	// an archived SKU and another store's products never appear
	mustExec(t, c.f.owner, `UPDATE catalog.skus SET status='archived' WHERE id=$1`, sku)
	_, _, body2 := get(http.MethodGet, "/v1/buyer/feeds/meta.csv", c.origin)
	if strings.Contains(body2, sku) {
		t.Error("an archived SKU is still in the feed")
	}
	mustExec(t, c.f.owner, `UPDATE catalog.skus SET status='active' WHERE id=$1`, sku)
}
