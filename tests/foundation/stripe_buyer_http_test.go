package foundation_test

// SP14 (contracts/stripe-psp-v1.md §9.2, §14): the private Go buyer routes over
// HTTP against real PG. Prepare/handoff/refresh/cancel/view with the frozen keys
// and enums, and PAYUNi responses byte-identical to the golden files that were
// captured on base b563bb5 BEFORE the start-http unit merged (see
// output/stripe-b1-tests-b/golden-capture.md). Never regenerate a golden to make
// this pass: a diff means the PAYUNi wire changed.
//
// BFF Node tests (apps/storefront/lib/payment-contract.ts validators) are
// NOT_RUN until the buyer-payment-ui amendment; nothing here claims them.
//
// No worker runs (shared fixture DB), so the session pin is an owner fixture
// (sstPin) and the cutoff an aged column (sstAge); both are disclosed.
// Used with: existing TestBuyerPayment* (PAYUNi behavior), which must stay green.

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"regexp"
	"sort"
	"testing"
	"time"

	"livecommerce/internal/buyerhttp"
	"livecommerce/internal/checkout"
)

var (
	sbhUUID = regexp.MustCompile(`[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}`)
	sbhTime = regexp.MustCompile(`"\d{4}-\d\d-\d\dT\d\d:\d\d:\d\d(\.\d+)?Z"`)
	sbhHex  = regexp.MustCompile(`[0-9A-Fa-f]{32,}`)
	// The public validator pattern of contract §9.2 is ^https://checkout\.stripe\.com/[!-~]{1,4000}$.
	// Go's RE2 refuses repeat counts above 1000, so the bound is a separate length check
	// (sbhRedirectOK); the character class and host are identical.
	sbhRedirect = regexp.MustCompile(`^https://checkout\.stripe\.com/[!-~]+$`)
)

func sbhRedirectOK(u string) bool {
	const prefix = "https://checkout.stripe.com/"
	return sbhRedirect.MatchString(u) && len(u)-len(prefix) >= 1 && len(u)-len(prefix) <= 4000
}

// sbhNormalize masks exactly what varies per run (order UUID, quoted timestamps,
// ciphertext/hash hex) and nothing else; it must match the normalizer that
// produced the goldens.
func sbhNormalize(b []byte) []byte {
	b = sbhUUID.ReplaceAll(b, []byte("00000000-0000-4000-8000-000000000000"))
	b = sbhTime.ReplaceAll(b, []byte(`"2000-01-01T00:00:00Z"`))
	return sbhHex.ReplaceAll(b, []byte("HEX"))
}

func sbhGolden(t *testing.T, name string, r bhResponse) {
	t.Helper()
	want, err := os.ReadFile("../payments/stripe-sp14-payuni-" + name + ".golden.json")
	if err != nil {
		t.Fatalf("golden %s: %v", name, err)
	}
	if got := sbhNormalize(r.body); !bytes.Equal(got, want) || r.status != 200 {
		t.Fatalf("PAYUNi %s changed on the wire (status %d):\n got  %s\n want %s", name, r.status, got, want)
	}
}

func sbhServe(t *testing.T, h hpHarness, starter *checkout.HostedPaymentStarter, publish bool) bhHarness {
	t.Helper()
	b := bhHarness{bcHarness: h.bcHarness, key: base64.RawURLEncoding.EncodeToString(randomBytes(32)), origin: "https://buyer-payment.example"}
	if publish {
		bhPublish(t, h.bcHarness, b.origin, h.f.tenantA, h.f.storeA1)
	}
	handler, err := buyerhttp.New(context.Background(), h.a.issuer, h.a.runtime, h.service, b.key, time.Hour, starter)
	if err != nil {
		t.Fatalf("buyer HTTP constructor: %v", err)
	}
	b.server = httptest.NewServer(handler)
	t.Cleanup(b.server.Close)
	return b
}

// sbhPayuniFlow replays the capture sequence byte for byte.
func sbhPayuniFlow(t *testing.T, h hpHarness, srv bhHarness) {
	t.Helper()
	path := "/v1/buyer/orders/" + h.hold.OrderID + "/payment"
	sbhGolden(t, "view-not-started", srv.request(t, "GET", path, h.cap.Token, "", nil, nil))
	in := map[string]any{"method_code": "payuni_credit", "method_version": 1, "locale": "zh-TW"}
	sbhGolden(t, "prepare", srv.request(t, "POST", path+"/prepare", h.cap.Token, t04Key("sbh-prepare"), in, nil))
	sbhGolden(t, "view-pending", srv.request(t, "GET", path, h.cap.Token, "", nil, nil))
	sbhGolden(t, "handoff-issued", srv.request(t, "POST", path+"/handoff", h.cap.Token, "", nil, nil))
	sbhGolden(t, "handoff-already-issued", srv.request(t, "POST", path+"/handoff", h.cap.Token, "", nil, nil))
	sbhGolden(t, "view-issued", srv.request(t, "GET", path, h.cap.Token, "", nil, nil))
}

var sbhViewKeys = func() []string {
	k := append(append([]string{}, bpViewKeys...), "cancel_requested")
	sort.Strings(k)
	return k
}()

type sbhStripe struct {
	h       hpHarness
	e       *sstEnv
	s       sstStore
	srv     bhHarness
	path    string
	attempt string
}

// sbhNew builds a store with PAYUNi and Stripe methods behind the Stripe-enabled
// service. prepare=true also starts the Stripe attempt through HTTP.
func sbhNew(t *testing.T, prepare bool) *sbhStripe {
	t.Helper()
	h := hpSetup(t)
	e := sstNewEnv(t, h.f, sstKeyring(t, "hosted_fixture", h.key))
	x := &sbhStripe{h: h, e: e, s: e.seed(t, h.psHarness), srv: sbhServe(t, h, e.svc, true), path: "/v1/buyer/orders/" + h.hold.OrderID + "/payment"}
	if prepare {
		in := map[string]any{"method_code": "stripe_checkout", "method_version": x.s.method, "locale": "zh-TW"}
		bphRaw(t, x.srv.request(t, "POST", x.path+"/prepare", h.cap.Token, t04Key("sbh-stripe"), in, nil), 200, []string{"amount_minor", "currency", "order_id", "state"})
		if err := h.f.owner.QueryRow(context.Background(), `SELECT id::text FROM checkout.payment_attempts WHERE order_id=$1`, h.hold.OrderID).Scan(&x.attempt); err != nil {
			t.Fatal(err)
		}
	}
	return x
}

func (x *sbhStripe) post(t *testing.T, suffix string) bhResponse {
	t.Helper()
	return x.srv.request(t, "POST", x.path+suffix, x.h.cap.Token, "", nil, nil)
}

func (x *sbhStripe) str(t *testing.T, raw map[string]json.RawMessage, key string) string {
	t.Helper()
	var v string
	if err := json.Unmarshal(raw[key], &v); err != nil {
		t.Fatalf("field %s is not a string: %s", key, raw[key])
	}
	return v
}

func (x *sbhStripe) boolField(t *testing.T, raw map[string]json.RawMessage, key string) bool {
	t.Helper()
	var v bool
	if err := json.Unmarshal(raw[key], &v); err != nil {
		t.Fatalf("field %s is not a bool: %s", key, raw[key])
	}
	return v
}

func (x *sbhStripe) cutoff(t *testing.T) time.Time {
	t.Helper()
	var c time.Time
	if err := x.h.f.owner.QueryRow(context.Background(), `SELECT handoff_cutoff FROM payments.stripe_sessions WHERE attempt_id=$1`, x.attempt).Scan(&c); err != nil {
		t.Fatal(err)
	}
	return c
}

func (x *sbhStripe) sameTime(t *testing.T, raw json.RawMessage, want time.Time) {
	t.Helper()
	var got time.Time
	if err := json.Unmarshal(raw, &got); err != nil || !got.Equal(want) {
		t.Fatalf("time %s != DB cutoff %s (%v)", raw, want, err)
	}
}

func (x *sbhStripe) view(t *testing.T) map[string]json.RawMessage {
	t.Helper()
	return bphRaw(t, x.srv.request(t, "GET", x.path, x.h.cap.Token, "", nil, nil), 200, sbhViewKeys)
}

func (x *sbhStripe) signals(t *testing.T) (n int, jobs int) {
	t.Helper()
	if err := x.h.f.owner.QueryRow(context.Background(), `SELECT
	 (SELECT count(*) FROM payments.stripe_signals WHERE attempt_id=$1::uuid),
	 (SELECT count(*) FROM river_payment.river_job WHERE kind='payment_signal_v1' AND args->>'operation_id'=$1::text)`, x.attempt).Scan(&n, &jobs); err != nil {
		t.Fatal(err)
	}
	return
}

func TestStripeSP14BuyerHTTP(t *testing.T) {
	t.Run("payuni_bytes_identical_stripe_disabled", func(t *testing.T) {
		h := hpSetup(t)
		sbhPayuniFlow(t, h, sbhServe(t, h, h.api.(*checkout.HostedPaymentStarter), true))
	})

	t.Run("payuni_bytes_identical_stripe_enabled", func(t *testing.T) {
		// Same goldens through the Stripe-enabled service. The store has no Stripe method, so the
		// projection may not grow a method, a cancel_requested field or another handoff shape.
		h := hpSetup(t)
		e := sstNewEnv(t, h.f, sstKeyring(t, "hosted_fixture", h.key))
		sbhPayuniFlow(t, h, sbhServe(t, h, e.svc, true))
	})

	t.Run("stripe_view_prepare_handoff_enums_and_keys", func(t *testing.T) {
		x := sbhNew(t, false)
		before := sstFacts(t, x.h.psHarness)
		// Before start: no attempt => no cancel_requested (PAYUNi-identical projection), <=2 methods.
		view := bphRaw(t, x.srv.request(t, "GET", x.path, x.h.cap.Token, "", nil, nil), 200, bpViewKeys)
		bpState(t, view, "payment_state", "NOT_STARTED")
		bpState(t, view, "handoff_state", "NONE")
		var methods []map[string]json.RawMessage
		if err := json.Unmarshal(view["methods"], &methods); err != nil || len(methods) != 2 {
			t.Fatalf("methods=%s err=%v (payuni_credit + stripe_checkout)", view["methods"], err)
		}
		codes := map[string]bool{}
		for _, m := range methods {
			if !reflect.DeepEqual(bpKeys(m), bpMethodKeys) {
				t.Fatalf("method keys %v", bpKeys(m))
			}
			var code string
			_ = json.Unmarshal(m["code"], &code)
			codes[code] = true
		}
		if !codes["payuni_credit"] || !codes["stripe_checkout"] {
			t.Fatalf("method codes %v", codes)
		}
		in := map[string]any{"method_code": "stripe_checkout", "method_version": x.s.method, "locale": "zh-TW"}
		key := t04Key("sbh-prepare")
		first := x.srv.request(t, "POST", x.path+"/prepare", x.h.cap.Token, key, in, nil)
		prepared := bphRaw(t, first, 200, []string{"amount_minor", "currency", "order_id", "state"})
		bpState(t, prepared, "state", "PAYMENT_PENDING")
		bpState(t, prepared, "currency", "TWD")
		var amount int64
		if err := json.Unmarshal(prepared["amount_minor"], &amount); err != nil || amount != 2500 {
			t.Fatalf("prepare amount %s", prepared["amount_minor"])
		}
		stable := sstFacts(t, x.h.psHarness)
		if stable == before {
			t.Fatal("prepare wrote no facts")
		}
		if replay := x.srv.request(t, "POST", x.path+"/prepare", x.h.cap.Token, key, in, nil); replay.status != 200 || !bytes.Equal(replay.body, first.body) || sstFacts(t, x.h.psHarness) != stable {
			t.Fatal("prepare replay changed the receipt bytes or the facts")
		}
		if err := x.h.f.owner.QueryRow(context.Background(), `SELECT id::text FROM checkout.payment_attempts WHERE order_id=$1`, x.h.hold.OrderID).Scan(&x.attempt); err != nil {
			t.Fatal(err)
		}
		// CREATING: attempt exists, worker has not pinned a session yet.
		v := x.view(t)
		bpState(t, v, "payment_state", "PENDING")
		bpState(t, v, "handoff_state", "CREATING")
		if string(v["methods"]) != "[]" || x.boolField(t, v, "cancel_requested") {
			t.Fatalf("pending view: methods=%s cancel=%s", v["methods"], v["cancel_requested"])
		}
		x.sameTime(t, v["handoff_expires_at"], x.cutoff(t)) // handoff cutoff, not the session expiry
		creating := bphRaw(t, x.post(t, "/handoff"), 200, []string{"disposition", "expires_at", "order_id"})
		bpState(t, creating, "disposition", "CREATING")
		x.sameTime(t, creating["expires_at"], x.cutoff(t))
		// REDIRECT: the pinned URL is returned, repeatably, and first_handed_out_at is set once.
		_, url := sstPin(t, x.h.f, x.attempt)
		bpState(t, x.view(t), "handoff_state", "READY")
		var firstHandoff *time.Time
		for i := 0; i < 3; i++ {
			out := bphRaw(t, x.post(t, "/handoff"), 200, []string{"disposition", "expires_at", "order_id", "redirect_url"})
			bpState(t, out, "disposition", "REDIRECT")
			if got := x.str(t, out, "redirect_url"); got != url || !sbhRedirectOK(got) {
				t.Fatalf("redirect url %q", got)
			}
			x.sameTime(t, out["expires_at"], x.cutoff(t))
			var stamp *time.Time
			if err := x.h.f.owner.QueryRow(context.Background(), `SELECT first_handed_out_at FROM payments.stripe_sessions WHERE attempt_id=$1`, x.attempt).Scan(&stamp); err != nil || stamp == nil {
				t.Fatalf("first_handed_out_at: %v", err)
			}
			if firstHandoff == nil {
				firstHandoff = stamp
			} else if !stamp.Equal(*firstHandoff) {
				t.Fatal("repeat handoff moved first_handed_out_at")
			}
		}
		if got := sstFacts(t, x.h.psHarness); got != stable {
			t.Fatalf("GET/handoff changed payment facts: %v -> %v", stable, got)
		}
		// strictness of the existing prepare validator with the widened method enum.
		for _, bad := range []map[string]any{
			{"method_code": "stripe_checkout", "method_version": x.s.method, "locale": "fr"},
			{"method_code": "stripe_card", "method_version": 1, "locale": "en"},
			{"method_code": "stripe_checkout", "method_version": x.s.method, "locale": "en", "provider": "stripe"},
		} {
			r := x.srv.request(t, "POST", x.path+"/prepare", x.h.cap.Token, t04Key("sbh-bad"), bad, nil)
			if r.status != 400 && r.status != 422 {
				t.Fatalf("invalid prepare accepted: %d %s", r.status, r.body)
			}
		}
		if got := sstFacts(t, x.h.psHarness); got != stable {
			t.Fatal("rejected prepare wrote facts")
		}
	})

	t.Run("stripe_refresh_cancel_exact_keys_throttle_and_setonce", func(t *testing.T) {
		x := sbhNew(t, true)
		sstPin(t, x.h.f, x.attempt)
		signal := func(suffix string) (scheduled bool) {
			raw := bphRaw(t, x.post(t, suffix), 200, []string{"order_id", "scheduled"})
			bpState(t, raw, "order_id", x.h.hold.OrderID)
			return x.boolField(t, raw, "scheduled")
		}
		n0, j0 := x.signals(t)
		if !signal("/refresh") {
			t.Fatal("first refresh not scheduled")
		}
		if n, j := x.signals(t); n != n0+1 || j != j0+1 {
			t.Fatalf("scheduled refresh: signals %d->%d jobs %d->%d", n0, n, j0, j)
		}
		var source, kind, state string
		if err := x.h.f.owner.QueryRow(context.Background(), `SELECT s.source,j.kind,j.state FROM payments.stripe_signals s
		 JOIN river_payment.river_job j ON j.id=s.job_id WHERE s.attempt_id=$1`, x.attempt).Scan(&source, &kind, &state); err != nil ||
			source != "BUYER_REFRESH" || kind != "payment_signal_v1" || state != "available" {
			t.Fatalf("refresh signal/job: %s %s %s %v", source, kind, state, err)
		}
		// 10 s DB throttle: scheduled=false and NO orphan job (the InsertTx is rolled back).
		for i := 0; i < 3; i++ {
			if signal("/refresh") {
				t.Fatal("refresh inside the 10 s throttle was scheduled")
			}
		}
		if n, j := x.signals(t); n != n0+1 || j != j0+1 {
			t.Fatalf("throttled refresh left rows: signals %d jobs %d", n, j)
		}
		sstAge(t, x.h.f, x.attempt, 11*time.Second)
		if !signal("/refresh") {
			t.Fatal("refresh after 10 s not scheduled")
		}
		// <=30 refreshes: owner fixture advances the counter to 29 (monotone, CHECK<=30).
		mustExec(t, x.h.f.owner, `UPDATE payments.stripe_sessions SET refresh_count=29 WHERE attempt_id=$1`, x.attempt)
		sstAge(t, x.h.f, x.attempt, 11*time.Second)
		if !signal("/refresh") {
			t.Fatal("30th refresh not scheduled")
		}
		_, jobsBefore := x.signals(t)
		sstAge(t, x.h.f, x.attempt, 11*time.Second)
		if signal("/refresh") {
			t.Fatal("31st refresh was scheduled")
		}
		if _, j := x.signals(t); j != jobsBefore {
			t.Fatal("capped refresh left an orphan job")
		}
		// cancel is set-once; its effect is asynchronous (expire then closure after Stripe confirms).
		if x.boolField(t, x.view(t), "cancel_requested") {
			t.Fatal("cancel_requested before cancel")
		}
		if !signal("/cancel") {
			t.Fatal("first cancel not scheduled")
		}
		if signal("/cancel") {
			t.Fatal("second cancel scheduled (set-once)")
		}
		v := x.view(t)
		if !x.boolField(t, v, "cancel_requested") {
			t.Fatal("cancel_requested not projected")
		}
		bpState(t, v, "payment_state", "PENDING") // stock is not released by the API
		var cancels int
		if err := x.h.f.owner.QueryRow(context.Background(), `SELECT count(*) FROM payments.stripe_signals WHERE attempt_id=$1 AND source='BUYER_CANCEL'`, x.attempt).Scan(&cancels); err != nil || cancels != 1 {
			t.Fatalf("cancel signals=%d %v", cancels, err)
		}
		var state2 string
		if err := x.h.f.owner.QueryRow(context.Background(), `SELECT reservation.state FROM inventory.reservations reservation WHERE reservation.id=$1`, x.h.hold.OrderID).Scan(&state2); err != nil || state2 != "PAYMENT_PENDING" {
			t.Fatalf("cancel released stock in the API: %s %v", state2, err)
		}
	})

	t.Run("stripe_handoff_unavailable_variants", func(t *testing.T) {
		type variant struct {
			name    string
			prepare func(*testing.T, *sbhStripe) bhHarness
			view    string // expected handoff_state ("" = do not assert)
		}
		self := func(t *testing.T, x *sbhStripe) bhHarness { return x.srv }
		for _, v := range []variant{
			{"binding_disabled", func(t *testing.T, x *sbhStripe) bhHarness {
				mustExec(t, x.h.f.owner, `UPDATE integration.bindings SET enabled=false WHERE id=(SELECT binding_id FROM integration.merchant_accounts WHERE id=$1)`, x.s.connection)
				return x.srv
			}, ""},
			{"qualification_revoked", func(t *testing.T, x *sbhStripe) bhHarness {
				qualExec(t, x.h.f.owner, `UPDATE payments.account_qualifications SET revoked_at=clock_timestamp() WHERE id=$1`, x.s.qualification)
				return x.srv
			}, ""},
			{"after_cutoff", func(t *testing.T, x *sbhStripe) bhHarness {
				sstAge(t, x.h.f, x.attempt, 36*time.Minute)
				return x.srv
			}, "UNAVAILABLE"},
			{"config_digest_mismatch", func(t *testing.T, x *sbhStripe) bhHarness {
				other := x.e.service(t, "PROVIDER_MOCK", checkout.StripeHostedConfig{ReturnURL: "https://checkout.example.test/payment/elsewhere"})
				return sbhServe(t, x.h, other, false)
			}, "UNAVAILABLE"},
			{"profile_mismatch", func(t *testing.T, x *sbhStripe) bhHarness {
				return sbhServe(t, x.h, x.e.service(t, "SANDBOX", x.e.scfg), false)
			}, "UNAVAILABLE"},
		} {
			t.Run(v.name, func(t *testing.T) {
				x := sbhNew(t, true)
				sstPin(t, x.h.f, x.attempt)
				srv := v.prepare(t, x)
				out := bphRaw(t, srv.request(t, "POST", x.path+"/handoff", x.h.cap.Token, "", nil, nil), 200, []string{"disposition", "expires_at", "order_id"})
				bpState(t, out, "disposition", "UNAVAILABLE") // never a redirect_url
				if v.view != "" {
					got := bphRaw(t, srv.request(t, "GET", x.path, x.h.cap.Token, "", nil, nil), 200, sbhViewKeys)
					bpState(t, got, "handoff_state", v.view)
				}
			})
		}
		_ = self
	})

	t.Run("refresh_cancel_config_mismatch_is_409_and_no_job", func(t *testing.T) {
		x := sbhNew(t, true)
		sstPin(t, x.h.f, x.attempt)
		other := sbhServe(t, x.h, x.e.service(t, "PROVIDER_MOCK", checkout.StripeHostedConfig{ReturnURL: "https://checkout.example.test/payment/elsewhere"}), false)
		n0, j0 := x.signals(t)
		for _, suffix := range []string{"/refresh", "/cancel"} {
			bphFail(t, other.request(t, "POST", x.path+suffix, x.h.cap.Token, "", nil, nil), 409, "conflict")
		}
		if n, j := x.signals(t); n != n0 || j != j0 {
			t.Fatalf("409 left signal/job rows: %d/%d -> %d/%d", n0, j0, n, j)
		}
	})

	t.Run("scope_and_strictness", func(t *testing.T) {
		x := sbhNew(t, true)
		sstPin(t, x.h.f, x.attempt)
		n0, j0 := x.signals(t)
		other := mustIssue(t, x.h.cqHarness.service, x.h.f.storeA1) // a different buyer of the same store
		missing := "/v1/buyer/orders/" + randomUUID() + "/payment"
		for _, suffix := range []string{"/refresh", "/cancel"} {
			bhError(t, x.srv.request(t, "POST", x.path+suffix, other.Token, "", nil, nil), 404, "not_found")
			bhError(t, x.srv.request(t, "POST", missing+suffix, x.h.cap.Token, "", nil, nil), 404, "not_found")
		}
		bhError(t, x.srv.request(t, "GET", x.path, other.Token, "", nil, nil), 404, "not_found")
		bhError(t, x.srv.request(t, "GET", missing, x.h.cap.Token, "", nil, nil), 404, "not_found")
		// A foreign buyer must never obtain the redirect URL, whatever the status.
		for _, r := range []bhResponse{x.srv.request(t, "POST", x.path+"/handoff", other.Token, "", nil, nil),
			x.srv.request(t, "POST", missing+"/handoff", x.h.cap.Token, "", nil, nil)} {
			if (r.status != 404 && r.status != 409) || bytes.Contains(r.body, []byte("checkout.stripe.com")) {
				t.Fatalf("foreign handoff: %d %s", r.status, r.body)
			}
		}
		// strict: no body, no Idempotency-Key, no query, one bearer token, valid path id.
		for _, suffix := range []string{"/refresh", "/cancel", "/handoff"} {
			for _, trial := range []struct {
				name, path, token, key string
				input                  any
				edit                   func(*http.Request)
				status                 int
				code                   string
			}{
				{"body", x.path + suffix, x.h.cap.Token, "", struct{}{}, nil, 422, "invalid_request"},
				{"key", x.path + suffix, x.h.cap.Token, t04Key("sbh-forbidden"), nil, nil, 422, "invalid_request"},
				{"query", x.path + suffix + "?x=1", x.h.cap.Token, "", nil, nil, 403, "forbidden"},
				{"missing_token", x.path + suffix, "", "", nil, nil, 401, "unauthorized"},
				{"duplicate_token", x.path + suffix, x.h.cap.Token, "", nil, func(r *http.Request) { r.Header.Add("Authorization", "Bearer "+x.h.cap.Token) }, 401, "unauthorized"},
				{"bad_bff_key", x.path + suffix, x.h.cap.Token, "", nil, func(r *http.Request) { r.Header.Set("X-Commerce-Buyer-BFF-Key", "bad") }, 401, "unauthorized"},
				{"cookie", x.path + suffix, x.h.cap.Token, "", nil, func(r *http.Request) { r.Header.Set("Cookie", "synthetic=1") }, 403, "forbidden"},
				{"bad_order_id", "/v1/buyer/orders/not-a-uuid/payment" + suffix, x.h.cap.Token, "", nil, nil, 422, "invalid_request"},
			} {
				bphFail(t, x.srv.request(t, "POST", trial.path, trial.token, trial.key, trial.input, trial.edit), trial.status, trial.code)
			}
		}
		if n, j := x.signals(t); n != n0 || j != j0 {
			t.Fatalf("denied requests created signals/jobs: %d/%d -> %d/%d", n0, j0, n, j)
		}
		// A revoked capability is 401 for the new routes exactly like handoff.
		mustExec(t, x.h.f.owner, `UPDATE buyer.capability_sessions SET revoked_at=clock_timestamp() WHERE id=$1`, x.h.cap.Scope.SessionID)
		for _, suffix := range []string{"/refresh", "/cancel"} {
			bphFail(t, x.srv.request(t, "POST", x.path+suffix, x.h.cap.Token, "", nil, nil), 401, "unauthorized")
		}
	})
}
