package foundation_test

// SP13 (contracts/stripe-psp-v1.md §9.1, §0.2 webhook clauses, §12 log-forbidden
// list): the real stripewebhook handler over HTTP against real PG with the real
// ingress role. The API binary mounts this same handler (mountStripe is covered
// by cmd/api/stripe_sp15_test.go); PG-side authority is covered by the existing
// SQL-level gates TestStripeSP06EndpointCustodyAndRLS,
// TestStripeSP13EndpointPrepareLocksRotation and
// TestStripeSP13AcceptedReceiptNeedsCommittedLink, which are cited, not repeated.
//
// Owner-injected hooks (disclosed for the evidence file): a FOR UPDATE row lock
// on the endpoint, a deferred constraint trigger on stripe_webhook_receipts
// (sleep / raise at COMMIT), a BEFORE INSERT trigger on stripe_signals that
// swallows the row, and one signal_count UPDATE. All are removed in cleanup.

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"livecommerce/internal/integrations/accounts"
	"livecommerce/internal/integrations/psp/stripe/stripetest"
	"livecommerce/internal/payments/stripeadmin"
	"livecommerce/internal/payments/stripewebhook"
	"livecommerce/internal/platform"
)

type swhLogBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *swhLogBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}
func (b *swhLogBuffer) String() string { b.mu.Lock(); defer b.mu.Unlock(); return b.buf.String() }

type swhHarness struct {
	e        *sstEnv
	s        sstStore
	ingress  *pgxpool.Pool
	server   *httptest.Server
	endpoint string
	secret   string
	next     string
	logs     *swhLogBuffer
}

type swhResult struct {
	status int
	header http.Header
	body   string
}

func swhSecret() string { return "whsec_" + hex.EncodeToString(randomBytes(20)) }

// swhSetup builds one Stripe store, an endpoint registered through the registrar
// (PROVIDER_MOCK profile, enabled) and the real handler behind httptest. Log
// capture starts before the handler is built so nothing it writes is missed.
func swhSetup(t *testing.T) *swhHarness {
	t.Helper()
	p := psSetup(t)
	e := sstNewEnv(t, p.f, sstKeyring(t, "sst_api", randomBytes(32)))
	h := &swhHarness{e: e, s: e.seed(t, p), secret: swhSecret(), logs: &swhLogBuffer{}}
	oldSlog, oldLog := slog.Default(), log.Writer()
	slog.SetDefault(slog.New(slog.NewTextHandler(h.logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	log.SetOutput(h.logs)
	t.Cleanup(func() { slog.SetDefault(oldSlog); log.SetOutput(oldLog) })
	var err error
	var version int64
	if h.endpoint, version, err = e.reg.SetWebhookEndpoint(context.Background(), h.s.scope, stripeadmin.EndpointInput{
		ConnectionID: h.s.connection, AccountID: h.s.account, Profile: "PROVIDER_MOCK", ExpectedVersion: 0, Enabled: true,
		Secrets: accounts.StripeWebhookSecrets{CurrentSecret: h.secret}}); err != nil || version != 1 || h.endpoint == "" {
		t.Fatalf("register webhook endpoint: %d %v", version, err)
	}
	h.ingress = h.openIngress(t)
	h.server = h.serve(t, e.signing, "PROVIDER_MOCK")
	return h
}

func (h *swhHarness) openIngress(t *testing.T) *pgxpool.Pool {
	t.Helper()
	pool, err := platform.OpenStripeIngressPool(context.Background(), sstLogin(t, h.e.f, "commerce_stripe_ingress"))
	if err != nil {
		t.Fatalf("open ingress pool: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func (h *swhHarness) serve(t *testing.T, signing *accounts.Keyring, profile string) *httptest.Server {
	t.Helper()
	inbox, err := stripewebhook.NewInbox(context.Background(), h.ingress, signing, profile)
	if err != nil {
		t.Fatalf("new inbox: %v", err)
	}
	handler, err := stripewebhook.NewHandler(inbox)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return srv
}

// attempt starts a Stripe attempt for the harness store and pins a session for it.
func (h *swhHarness) attempt(t *testing.T) (attempt, session string) {
	t.Helper()
	res, err := h.s.begin(h.e.svc, t04Key("swh-start"), h.s.input("zh-TW"))
	if err != nil {
		t.Fatal(err)
	}
	session, _ = sstPin(t, h.e.f, res.AttemptID)
	return res.AttemptID, session
}

var swhEventSeq int

func swhEventID(kind string) string {
	swhEventSeq++
	return fmt.Sprintf("evt_swh%s_%s_%d", kind, t04Tag(), swhEventSeq)
}

func (h *swhHarness) do(t *testing.T, srv *httptest.Server, method, path string, body []byte, edit func(*http.Request)) swhResult {
	t.Helper()
	req, err := http.NewRequest(method, srv.URL+path, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if edit != nil {
		edit(req)
	}
	res, err := (&http.Client{Timeout: 15 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}).Do(req)
	if err != nil {
		t.Fatalf("webhook request failed: %v", err)
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(res.Body, 1<<16))
	return swhResult{res.StatusCode, res.Header, string(raw)}
}

func swhSigned(secret string, body []byte) func(*http.Request) {
	return func(r *http.Request) {
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("Stripe-Signature", stripetest.SignWebhook(secret, body, time.Now()))
	}
}

// post delivers a correctly signed JSON event to endpoint (default: the harness one).
func (h *swhHarness) post(t *testing.T, endpoint string, body []byte, secret string) swhResult {
	t.Helper()
	return h.do(t, h.server, http.MethodPost, "/v1/stripe/webhook/"+endpoint, body, swhSigned(secret, body))
}

func swhExpect(t *testing.T, r swhResult, status int, body string) {
	t.Helper()
	if r.status != status || r.body != body && strings.TrimSpace(r.body) != body {
		t.Fatalf("status=%d body=%q want %d %q", r.status, r.body, status, body)
	}
	if r.header.Get("Cache-Control") != "no-store" {
		t.Fatalf("missing Cache-Control: no-store (%q)", r.header.Get("Cache-Control"))
	}
}

const swhOK = `{"received":true}`

func swhErr(code string) string { return `{"error":"` + code + `"}` }

type swhCounts struct{ receipts, signals, jobs int }

func (h *swhHarness) counts(t *testing.T) swhCounts {
	t.Helper()
	var c swhCounts
	if err := h.e.f.owner.QueryRow(context.Background(), `SELECT
	 (SELECT count(*) FROM payments.stripe_webhook_receipts WHERE tenant_id=$1 OR endpoint_id=$2::uuid),
	 (SELECT count(*) FROM payments.stripe_signals WHERE tenant_id=$1),
	 (SELECT count(*) FROM river_payment.river_job WHERE kind='payment_signal_v1')`, h.s.scope.TenantID, h.endpoint).
		Scan(&c.receipts, &c.signals, &c.jobs); err != nil {
		t.Fatal(err)
	}
	return c
}

type swhReceipt struct {
	disposition, reason string
	redelivery          int
	hasSignal           bool
}

func (h *swhHarness) receipt(t *testing.T, endpoint, eventID string) (swhReceipt, bool) {
	t.Helper()
	var r swhReceipt
	err := h.e.f.owner.QueryRow(context.Background(), `SELECT disposition,reason,redelivery_count,signal_id IS NOT NULL
	 FROM payments.stripe_webhook_receipts WHERE endpoint_id=$1::uuid AND event_id=$2`, endpoint, eventID).
		Scan(&r.disposition, &r.reason, &r.redelivery, &r.hasSignal)
	return r, err == nil
}

// swhTrigger installs an owner-side hook and removes it at cleanup. kind is
// "TRIGGER" or "CONSTRAINT TRIGGER", when e.g. "BEFORE INSERT", deferral is ""
// or "DEFERRABLE INITIALLY DEFERRED". The function is EXECUTE-revoked from PUBLIC
// (trigger creation, not firing, needs EXECUTE) so no pool gate ever sees it.
func swhTrigger(t *testing.T, f *testFixture, kind, name, when, table, deferral, body string) {
	t.Helper()
	mustExec(t, f.owner, fmt.Sprintf(`CREATE FUNCTION public.%s() RETURNS trigger LANGUAGE plpgsql AS $f$ BEGIN %s END $f$`, name, body))
	mustExec(t, f.owner, fmt.Sprintf(`REVOKE ALL ON FUNCTION public.%s() FROM PUBLIC`, name))
	mustExec(t, f.owner, fmt.Sprintf(`CREATE %s %s %s ON %s %s FOR EACH ROW EXECUTE FUNCTION public.%s()`, kind, name, when, table, deferral, name))
	t.Cleanup(func() {
		mustExec(t, f.owner, fmt.Sprintf(`DROP TRIGGER IF EXISTS %s ON %s`, name, table))
		mustExec(t, f.owner, fmt.Sprintf(`DROP FUNCTION IF EXISTS public.%s()`, name))
	})
}

func swhDropTrigger(t *testing.T, f *testFixture, name, table string) {
	t.Helper()
	mustExec(t, f.owner, fmt.Sprintf(`DROP TRIGGER IF EXISTS %s ON %s`, name, table))
}

// swhScan returns every base table with a row whose text contains any needle
// (owner = superuser, so RLS does not hide rows). strpos, not LIKE: needles are
// raw bodies/signatures and must not act as patterns.
func swhScan(t *testing.T, f *testFixture, needles ...string) []string {
	t.Helper()
	ctx := context.Background()
	rows, err := f.owner.Query(ctx, `SELECT format('%I.%I',n.nspname,c.relname) FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace
	 WHERE c.relkind IN ('r','p') AND n.nspname NOT IN ('pg_catalog','information_schema') AND n.nspname NOT LIKE 'pg\_%'`)
	if err != nil {
		t.Fatal(err)
	}
	var tables []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatal(err)
		}
		tables = append(tables, name)
	}
	rows.Close()
	var hits []string
	for _, table := range tables {
		var found bool
		// table comes from pg_class via %I quoting; needles are bound.
		if err := f.owner.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM `+table+` x, unnest($1::text[]) n WHERE strpos(x::text,n)>0)`, needles).Scan(&found); err != nil {
			t.Fatalf("scan %s: %v", table, err)
		}
		if found {
			hits = append(hits, table)
		}
	}
	return hits
}

// TestStripeSP13WebhookHTTP: every §9.1 status and limit with the frozen codes.
func TestStripeSP13WebhookHTTP(t *testing.T) {
	h := swhSetup(t)
	attempt, session := h.attempt(t)
	ev := func(kind string, mutate func(*stripetest.EventOpts)) (id string, body []byte) {
		o := stripetest.EventOpts{ID: swhEventID(kind), SessionID: session, ClientRef: attempt, Attempt: attempt, PendingWebhooks: 1}
		if mutate != nil {
			mutate(&o)
		}
		return o.ID, stripetest.EventBody(o)
	}
	base := "/v1/stripe/webhook/" + h.endpoint
	contentJSON := func(r *http.Request) { r.Header.Set("Content-Type", "application/json") }

	t.Run("rejections_and_frozen_codes", func(t *testing.T) {
		_, body := ev("rej", nil)
		before := h.counts(t)
		// method first, then path, then query, content type, encoding, busy, size.
		for _, m := range []string{http.MethodGet, http.MethodPut, http.MethodDelete, http.MethodHead, http.MethodPatch} {
			r := h.do(t, h.server, m, base, nil, nil)
			if r.status != 405 || r.header.Get("Allow") != http.MethodPost || (m != http.MethodHead && r.body != swhErr("method_not_allowed")) {
				t.Fatalf("%s: %d %q allow=%q", m, r.status, r.body, r.header.Get("Allow"))
			}
		}
		upper := strings.ToUpper(h.endpoint)
		for _, path := range []string{
			"/v1/stripe/webhook", "/v1/stripe/webhook/", "/v1/stripe/webhook/not-a-uuid", "/v1/stripe/webhook/" + upper,
			base + "/", base + "/extra", "/v1/stripe//webhook/" + h.endpoint, "/v1/stripe/webhook/../webhook/" + h.endpoint,
			"/v1/stripe/webhook/" + strings.ReplaceAll(h.endpoint, "-", ""), "/v1/meta/webhook", "/v1/stripe",
		} {
			swhExpect(t, h.do(t, h.server, http.MethodPost, path, body, swhSigned(h.secret, body)), 404, swhErr("not_found"))
		}
		for _, q := range []string{"?", "?a=b", "?x"} {
			swhExpect(t, h.do(t, h.server, http.MethodPost, base+q, body, swhSigned(h.secret, body)), 400, swhErr("invalid_request"))
		}
		for _, ct := range []string{"", "text/plain", "application/x-www-form-urlencoded", "application/jsonx", "application/json; charset=latin1", "application/json, text/plain", "multipart/form-data"} {
			r := h.do(t, h.server, http.MethodPost, base, body, func(r *http.Request) {
				if ct != "" {
					r.Header.Set("Content-Type", ct)
				} else {
					r.Header.Del("Content-Type")
				}
				r.Header.Set("Stripe-Signature", stripetest.SignWebhook(h.secret, body, time.Now()))
			})
			if r.status != 415 || r.body != swhErr("unsupported_media_type") {
				t.Fatalf("content type %q: %d %q", ct, r.status, r.body)
			}
		}
		for _, enc := range []string{"gzip", "br", "deflate"} {
			r := h.do(t, h.server, http.MethodPost, base, body, func(r *http.Request) {
				swhSigned(h.secret, body)(r)
				r.Header.Set("Content-Encoding", enc)
			})
			if r.status != 415 || r.body != swhErr("unsupported_media_type") {
				t.Fatalf("content encoding %q: %d %q", enc, r.status, r.body)
			}
		}
		huge := bytes.Repeat([]byte("x"), 256*1024+1)
		swhExpect(t, h.do(t, h.server, http.MethodPost, base, huge, swhSigned(h.secret, huge)), 413, swhErr("payload_too_large"))
		// exactly 256 KiB is within the limit (then fails signature/JSON rules, not size).
		exact := bytes.Repeat([]byte("x"), 256*1024)
		if r := h.do(t, h.server, http.MethodPost, base, exact, contentJSON); r.status != 400 || r.body != swhErr("invalid_signature") {
			t.Fatalf("256 KiB boundary: %d %q", r.status, r.body)
		}
		// signature rules: missing / wrong secret / altered body / stale / duplicated header.
		for name, edit := range map[string]func(*http.Request){
			"missing":      contentJSON,
			"wrong_secret": swhSigned(swhSecret(), body),
			"altered_body": func(r *http.Request) {
				swhSigned(h.secret, append([]byte("x"), body...))(r)
			},
			"stale_301s": func(r *http.Request) {
				contentJSON(r)
				r.Header.Set("Stripe-Signature", stripetest.SignWebhook(h.secret, body, time.Now().Add(-301*time.Second)))
			},
			"future_301s": func(r *http.Request) {
				contentJSON(r)
				r.Header.Set("Stripe-Signature", stripetest.SignWebhook(h.secret, body, time.Now().Add(301*time.Second)))
			},
			"v0_only": func(r *http.Request) {
				contentJSON(r)
				r.Header.Set("Stripe-Signature", strings.Replace(stripetest.SignWebhook(h.secret, body, time.Now()), ",v1=", ",v0=", 1))
			},
		} {
			if r := h.do(t, h.server, http.MethodPost, base, body, edit); r.status != 400 || r.body != swhErr("invalid_signature") {
				t.Fatalf("signature %s: %d %q", name, r.status, r.body)
			}
		}
		// A syntactically valid but unknown endpoint is 404 before any signature work.
		swhExpect(t, h.post(t, randomUUID(), body, h.secret), 404, swhErr("not_found"))
		if h.counts(t) != before {
			t.Fatalf("rejections created receipts/signals/jobs: %+v -> %+v", before, h.counts(t))
		}
	})

	t.Run("accept_atomically_and_dedupe_changed_body", func(t *testing.T) {
		before := h.counts(t)
		id, body := ev("acc", nil)
		r := h.post(t, h.endpoint, body, h.secret)
		swhExpect(t, r, 200, swhOK)
		if ct := r.header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
			t.Fatalf("content type %q", ct)
		}
		rc, ok := h.receipt(t, h.endpoint, id)
		if !ok || rc.disposition != "ACCEPTED" || rc.reason != "accepted" || !rc.hasSignal || rc.redelivery != 0 {
			t.Fatalf("accepted receipt: %+v ok=%v", rc, ok)
		}
		var kind, queue, source string
		var args []byte
		var signalSession *string
		if err := h.e.f.owner.QueryRow(context.Background(), `SELECT j.kind,j.queue,j.args,s.source,s.session_id
		 FROM payments.stripe_webhook_receipts r JOIN payments.stripe_signals s ON s.id=r.signal_id
		 JOIN river_payment.river_job j ON j.id=s.job_id WHERE r.endpoint_id=$1::uuid AND r.event_id=$2`, h.endpoint, id).
			Scan(&kind, &queue, &args, &source, &signalSession); err != nil {
			t.Fatalf("reciprocal signal/job missing after ACK: %v", err)
		}
		var a map[string]any
		_ = json.Unmarshal(args, &a)
		if kind != "payment_signal_v1" || queue != "payment_mock_v1" || source != "STRIPE_WEBHOOK" || a["operation_id"] != attempt ||
			a["version"] != float64(1) || len(a) != 3 || a["signal_id"] == "" {
			t.Fatalf("signal job identity: %s %s %s %s", kind, queue, source, args)
		}
		after := h.counts(t)
		if after.receipts != before.receipts+1 || after.signals != before.signals+1 || after.jobs != before.jobs+1 {
			t.Fatalf("one accepted event must add exactly one receipt/signal/job: %+v -> %+v", before, after)
		}
		// Redelivery with a changed body (pending_webhooks changes between Stripe attempts):
		// same event id => DUPLICATE, no job, no second receipt; only redelivery_count moves.
		_, changed := ev("acc", func(o *stripetest.EventOpts) { o.ID = id; o.PendingWebhooks = 7 })
		swhExpect(t, h.post(t, h.endpoint, changed, h.secret), 200, swhOK)
		swhExpect(t, h.post(t, h.endpoint, body, h.secret), 200, swhOK)
		if h.counts(t) != after {
			t.Fatalf("duplicate delivery created rows: %+v -> %+v", after, h.counts(t))
		}
		if rc, _ := h.receipt(t, h.endpoint, id); rc.redelivery != 2 || rc.disposition != "ACCEPTED" {
			t.Fatalf("redelivery bookkeeping: %+v", rc)
		}
	})

	t.Run("ignore_and_quarantine_reasons", func(t *testing.T) {
		before := h.counts(t)
		// Dispositions follow prepare's ladder: unsubscribed_type/probe_session/signal_cap are
		// IGNORED, every other reason is QUARANTINED. account_unregistered cannot arise over
		// HTTP any more: the account derives from the immutable endpoint row (contract §0.2).
		cases := []struct {
			name, disposition, reason string
			mutate                    func(*stripetest.EventOpts)
		}{
			{"unsubscribed_type", "IGNORED", "unsubscribed_type", func(o *stripetest.EventOpts) { o.Type = "payment_intent.created" }},
			{"probe_session", "IGNORED", "probe_session", func(o *stripetest.EventOpts) { o.Probe = true }},
			{"connect_event", "QUARANTINED", "connect_event", func(o *stripetest.EventOpts) { o.Connect = true }},
			{"livemode_mismatch", "QUARANTINED", "livemode_mismatch", func(o *stripetest.EventOpts) { o.Livemode = true }},
			{"object_mismatch", "QUARANTINED", "object_mismatch", func(o *stripetest.EventOpts) { o.ObjectType = "payment_intent" }},
			{"unknown_session", "QUARANTINED", "unknown_session", func(o *stripetest.EventOpts) {
				ref := randomUUID()
				o.SessionID, o.ClientRef, o.Attempt = "cs_test_unknown_"+t04Tag(), ref, ref
			}},
			{"reference_mismatch_unpinned", "QUARANTINED", "reference_mismatch", func(o *stripetest.EventOpts) {
				o.SessionID, o.ClientRef, o.Attempt = "cs_test_unknown_"+t04Tag(), randomUUID(), randomUUID()
			}},
			{"reference_mismatch_pinned", "QUARANTINED", "reference_mismatch", func(o *stripetest.EventOpts) { o.ClientRef = randomUUID() }},
		}
		for _, c := range cases {
			id, body := ev(c.name, c.mutate)
			swhExpect(t, h.post(t, h.endpoint, body, h.secret), 200, swhOK)
			rc, ok := h.receipt(t, h.endpoint, id)
			if !ok || rc.disposition != c.disposition || rc.reason != c.reason || rc.hasSignal {
				t.Fatalf("%s: %+v ok=%v", c.name, rc, ok)
			}
			// every non-accepted delivery is stored once and never produces a signal or job.
			swhExpect(t, h.post(t, h.endpoint, body, h.secret), 200, swhOK)
			if rc, _ := h.receipt(t, h.endpoint, id); rc.redelivery != 1 {
				t.Fatalf("%s redelivery: %+v", c.name, rc)
			}
		}
		if after := h.counts(t); after.signals != before.signals || after.jobs != before.jobs || after.receipts != before.receipts+len(cases) {
			t.Fatalf("ignored/quarantined events changed signals or jobs: %+v -> %+v", before, after)
		}
		// signal cap: the 65th signal for an attempt is IGNORED signal_cap (owner fixture sets
		// the counter on a second order of the same store so later subtests keep their attempt).
		other, otherSession := h.attempt2(t)
		mustExec(t, h.e.f.owner, `UPDATE payments.stripe_sessions SET signal_count=64 WHERE attempt_id=$1`, other)
		id, body := ev("cap", func(o *stripetest.EventOpts) { o.SessionID, o.ClientRef, o.Attempt = otherSession, other, other })
		swhExpect(t, h.post(t, h.endpoint, body, h.secret), 200, swhOK)
		if rc, ok := h.receipt(t, h.endpoint, id); !ok || rc.disposition != "IGNORED" || rc.reason != "signal_cap" || rc.hasSignal {
			t.Fatalf("signal cap: %+v", rc)
		}
	})

	t.Run("malformed_after_signature_is_200_once", func(t *testing.T) {
		before := h.counts(t)
		body := []byte(`{"not":"an event","sentinel":"swh-malformed-` + t04Tag() + `"}`)
		for i := 0; i < 3; i++ {
			swhExpect(t, h.post(t, h.endpoint, body, h.secret), 200, swhOK)
		}
		var n int
		if err := h.e.f.owner.QueryRow(context.Background(), `SELECT count(*) FROM payments.stripe_webhook_receipts
		 WHERE endpoint_id=$1::uuid AND event_id IS NULL AND disposition='MALFORMED' AND reason='malformed_json'`, h.endpoint).Scan(&n); err != nil || n != 1 {
			t.Fatalf("malformed receipts=%d err=%v (deduplicated by body hash, once)", n, err)
		}
		if after := h.counts(t); after.receipts != before.receipts+1 || after.signals != before.signals || after.jobs != before.jobs {
			t.Fatalf("malformed changed signals/jobs: %+v -> %+v", before, after)
		}
	})

	t.Run("no_ack_before_commit_and_commit_failure_503", func(t *testing.T) {
		// (1) blocked prepare: the response must not exist until the lock is released.
		holder, err := h.e.f.owner.Begin(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		defer holder.Rollback(context.Background())
		if _, err := holder.Exec(context.Background(), `SELECT 1 FROM payments.stripe_webhook_endpoints WHERE endpoint_id=$1::uuid FOR UPDATE`, h.endpoint); err != nil {
			t.Fatal(err)
		}
		id, body := ev("blocked", nil)
		done := make(chan swhResult, 1)
		go func() { done <- h.post(t, h.endpoint, body, h.secret) }()
		select {
		case r := <-done:
			t.Fatalf("ACK/response %d %q while the admission tx was still blocked", r.status, r.body)
		case <-time.After(600 * time.Millisecond):
		}
		if _, ok := h.receipt(t, h.endpoint, id); ok {
			t.Fatal("receipt visible before COMMIT")
		}
		if err := holder.Commit(context.Background()); err != nil {
			t.Fatal(err)
		}
		select {
		case r := <-done:
			swhExpect(t, r, 200, swhOK)
		case <-time.After(6 * time.Second):
			t.Fatal("no response after unblock")
		}
		if rc, ok := h.receipt(t, h.endpoint, id); !ok || rc.disposition != "ACCEPTED" {
			t.Fatalf("receipt after ACK: %+v ok=%v", rc, ok)
		}
		// (2) the 2 s DB transaction budget: a lock held longer than that is a 503, never a 200.
		holder2, err := h.e.f.owner.Begin(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		defer holder2.Rollback(context.Background())
		if _, err := holder2.Exec(context.Background(), `SELECT 1 FROM payments.stripe_webhook_endpoints WHERE endpoint_id=$1::uuid FOR UPDATE`, h.endpoint); err != nil {
			t.Fatal(err)
		}
		id2, body2 := ev("budget", nil)
		start := time.Now()
		r := h.post(t, h.endpoint, body2, h.secret)
		swhExpect(t, r, 503, swhErr("unavailable"))
		if d := time.Since(start); d < 1500*time.Millisecond || d > 5500*time.Millisecond {
			t.Fatalf("503 after %s: DB tx budget is 2 s and the request deadline 5 s", d)
		}
		if err := holder2.Rollback(context.Background()); err != nil {
			t.Fatal(err)
		}
		if _, ok := h.receipt(t, h.endpoint, id2); ok {
			t.Fatal("503 left a receipt")
		}
		// Stripe retries the same event: now it is admitted exactly once.
		swhExpect(t, h.post(t, h.endpoint, body2, h.secret), 200, swhOK)
		if rc, ok := h.receipt(t, h.endpoint, id2); !ok || rc.disposition != "ACCEPTED" || rc.redelivery != 0 {
			t.Fatalf("retry after 503: %+v ok=%v", rc, ok)
		}
		// (3) a deferred constraint trigger delays COMMIT: no response and no visible row before it.
		swhTrigger(t, h.e.f, "CONSTRAINT TRIGGER", "swh_commit_probe", "AFTER INSERT", "payments.stripe_webhook_receipts", "DEFERRABLE INITIALLY DEFERRED",
			`IF NEW.event_id LIKE 'evt_swhslow_%' THEN PERFORM pg_sleep(1.5); END IF;
			 IF NEW.event_id LIKE 'evt_swhfail_%' THEN RAISE EXCEPTION 'swh injected commit failure' USING ERRCODE='XX000'; END IF;
			 RETURN NULL;`)
		slowID, slowBody := ev("slow", nil)
		slowDone := make(chan swhResult, 1)
		go func() { slowDone <- h.post(t, h.endpoint, slowBody, h.secret) }()
		time.Sleep(700 * time.Millisecond) // inside the deferred trigger's sleep
		select {
		case r := <-slowDone:
			t.Fatalf("ACK %d before COMMIT completed", r.status)
		default:
		}
		if _, ok := h.receipt(t, h.endpoint, slowID); ok {
			t.Fatal("row visible before COMMIT")
		}
		swhExpect(t, <-slowDone, 200, swhOK)
		if _, ok := h.receipt(t, h.endpoint, slowID); !ok {
			t.Fatal("ACKed event has no committed receipt")
		}
		// (4) commit-time failure: 503, nothing persisted, no orphan signal/job.
		before := h.counts(t)
		failID, failBody := ev("fail", nil)
		swhExpect(t, h.post(t, h.endpoint, failBody, h.secret), 503, swhErr("unavailable"))
		if _, ok := h.receipt(t, h.endpoint, failID); ok || h.counts(t) != before {
			t.Fatalf("commit failure persisted rows: %+v -> %+v", before, h.counts(t))
		}
		swhDropTrigger(t, h.e.f, "swh_commit_probe", "payments.stripe_webhook_receipts")
		swhExpect(t, h.post(t, h.endpoint, failBody, h.secret), 200, swhOK)
		if rc, ok := h.receipt(t, h.endpoint, failID); !ok || rc.disposition != "ACCEPTED" {
			t.Fatalf("redelivery after commit failure: %+v", rc)
		}
	})

	t.Run("missing_signal_linkage_cannot_ack", func(t *testing.T) {
		// A BEFORE INSERT trigger swallows the reciprocal signal row: the deferred receipt
		// link constraint must fail COMMIT, so the handler answers 503 and stores nothing.
		swhTrigger(t, h.e.f, "TRIGGER", "swh_swallow_signal", "BEFORE INSERT", "payments.stripe_signals", "", `RETURN NULL;`)
		before := h.counts(t)
		id, body := ev("nolink", nil)
		swhExpect(t, h.post(t, h.endpoint, body, h.secret), 503, swhErr("unavailable"))
		if _, ok := h.receipt(t, h.endpoint, id); ok || h.counts(t) != before {
			t.Fatalf("missing linkage was acknowledged or persisted: %+v -> %+v", before, h.counts(t))
		}
		swhDropTrigger(t, h.e.f, "swh_swallow_signal", "payments.stripe_signals")
		swhExpect(t, h.post(t, h.endpoint, body, h.secret), 200, swhOK)
	})

	t.Run("signature_endpoint_mismatch_and_stale_key_version", func(t *testing.T) {
		ctx := context.Background()
		// A second endpoint (another store/account, its own secret) exists: a body signed
		// for it and posted to ours fails, and vice versa.
		p2 := psSetup(t)
		s2 := h.e.seed(t, p2)
		secret2 := swhSecret()
		endpoint2, _, err := h.e.reg.SetWebhookEndpoint(ctx, s2.scope, stripeadmin.EndpointInput{ConnectionID: s2.connection, AccountID: s2.account,
			Profile: "PROVIDER_MOCK", Enabled: true, Secrets: accounts.StripeWebhookSecrets{CurrentSecret: secret2}})
		if err != nil {
			t.Fatal(err)
		}
		before := h.counts(t)
		id, body := ev("mismatch", nil)
		swhExpect(t, h.post(t, h.endpoint, body, secret2), 400, swhErr("invalid_signature"))
		swhExpect(t, h.post(t, endpoint2, body, h.secret), 400, swhErr("invalid_signature"))
		if _, ok := h.receipt(t, h.endpoint, id); ok || h.counts(t) != before {
			t.Fatal("mismatch created a receipt")
		}
		// An event for OUR attempt delivered (correctly signed) to the other endpoint never
		// maps: account and scope derive from the endpoint row, not from unsigned fields.
		wid, wbody := ev("crossaccount", nil)
		swhExpect(t, h.post(t, endpoint2, wbody, secret2), 200, swhOK)
		if rc, ok := h.receipt(t, endpoint2, wid); !ok || rc.hasSignal || rc.disposition == "ACCEPTED" {
			t.Fatalf("forged cross-account session accepted: %+v ok=%v", rc, ok)
		}
		// Stale key_version: rotate the signing secret while a delivery sits between
		// "material read" and "prepare". The owner tx performs the same registrar SQL and
		// keeps the endpoint row locked, so the in-flight request's prepare must see the new version.
		next := swhSecret()
		keyID, nonce, ciphertext, err := h.e.signing.SealStripeWebhook(accounts.StripeWebhookScope{TenantID: h.s.scope.TenantID, StoreID: h.s.scope.StoreID,
			ConnectionID: h.s.connection, EndpointID: h.endpoint, Environment: "SANDBOX", AccountID: h.s.account, Profile: "PROVIDER_MOCK", KeyVersion: 2},
			accounts.StripeWebhookSecrets{CurrentSecret: next})
		if err != nil {
			t.Fatal(err)
		}
		holder, err := h.e.f.owner.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer holder.Rollback(ctx)
		var v int64
		if err := holder.QueryRow(ctx, `SELECT payments.set_stripe_webhook_endpoint($1::uuid,$2::uuid,$3::uuid,$4::uuid,$5::uuid,'PROVIDER_MOCK',1,true,$6,$7::bytea,$8::bytea)`,
			h.s.scope.TenantID, h.s.scope.StoreID, h.s.scope.PrincipalID, h.s.connection, h.endpoint, keyID, nonce, ciphertext).Scan(&v); err != nil || v != 2 {
			t.Fatalf("owner-side rotation: %d %v", v, err)
		}
		sid, sbody := ev("stale", nil)
		done := make(chan swhResult, 1)
		go func() { done <- h.post(t, h.endpoint, sbody, h.secret) }() // old secret, old key_version 1
		time.Sleep(500 * time.Millisecond)
		if err := holder.Commit(ctx); err != nil {
			t.Fatal(err)
		}
		swhExpect(t, <-done, 503, swhErr("unavailable"))
		if _, ok := h.receipt(t, h.endpoint, sid); ok {
			t.Fatal("stale key_version produced a receipt")
		}
		// After the committed rotation the old secret is dead and the new one admits.
		swhExpect(t, h.post(t, h.endpoint, sbody, h.secret), 400, swhErr("invalid_signature"))
		swhExpect(t, h.post(t, h.endpoint, sbody, next), 200, swhOK)
		h.secret = next
	})

	t.Run("historical_admission_survives_binding_disable_and_api_rotation_but_not_endpoint_disable", func(t *testing.T) {
		ctx := context.Background()
		mustExec(t, h.e.f.owner, `UPDATE integration.bindings SET enabled=false WHERE id=(SELECT binding_id FROM integration.merchant_accounts WHERE id=$1)`, h.s.connection)
		id, body := ev("binding", nil)
		swhExpect(t, h.post(t, h.endpoint, body, h.secret), 200, swhOK)
		if _, ok := h.receipt(t, h.endpoint, id); !ok {
			t.Fatal("binding disable suppressed historical admission")
		}
		newKey := "sk_test_" + hex.EncodeToString(randomBytes(12))
		if err := h.e.fake.AddAccount(h.s.account, newKey); err != nil {
			t.Fatal(err)
		}
		if head, err := h.e.reg.Rotate(ctx, h.s.scope, h.s.connection, 1, h.s.account, newKey); err != nil || head != 2 {
			t.Fatalf("API-key rotation: %d %v", head, err)
		}
		id, body = ev("rotated", nil)
		swhExpect(t, h.post(t, h.endpoint, body, h.secret), 200, swhOK)
		if _, ok := h.receipt(t, h.endpoint, id); !ok {
			t.Fatal("API-key rotation suppressed historical admission")
		}
		var version int64
		if err := h.e.f.owner.QueryRow(ctx, `SELECT key_version FROM payments.stripe_webhook_endpoints WHERE endpoint_id=$1::uuid`, h.endpoint).Scan(&version); err != nil {
			t.Fatal(err)
		}
		_, v, err := h.e.reg.SetWebhookEndpoint(ctx, h.s.scope, stripeadmin.EndpointInput{ConnectionID: h.s.connection, EndpointID: h.endpoint,
			AccountID: h.s.account, Profile: "PROVIDER_MOCK", ExpectedVersion: version, Enabled: false, Secrets: accounts.StripeWebhookSecrets{CurrentSecret: h.secret}})
		if err != nil || v != version+1 {
			t.Fatalf("disable endpoint: %d %v", v, err)
		}
		id, body = ev("disabled", nil)
		before := h.counts(t)
		swhExpect(t, h.post(t, h.endpoint, body, h.secret), 404, swhErr("not_found"))
		if _, ok := h.receipt(t, h.endpoint, id); ok || h.counts(t) != before {
			t.Fatal("disabled endpoint admitted an event")
		}
		if _, _, err := h.e.reg.SetWebhookEndpoint(ctx, h.s.scope, stripeadmin.EndpointInput{ConnectionID: h.s.connection, EndpointID: h.endpoint,
			AccountID: h.s.account, Profile: "PROVIDER_MOCK", ExpectedVersion: v, Enabled: true, Secrets: accounts.StripeWebhookSecrets{CurrentSecret: h.secret}}); err != nil {
			t.Fatal(err)
		}
		swhExpect(t, h.post(t, h.endpoint, body, h.secret), 200, swhOK)
	})

	t.Run("wrong_profile_then_correct_profile_admits_exactly_one_signal", func(t *testing.T) {
		ctx := context.Background()
		// SANDBOX endpoint for the same connection (UNIQUE per connection+profile) served by a
		// SANDBOX-profile handler; the attempt runs on PROVIDER_MOCK, so this delivery is wrong-profile.
		sbSecret := swhSecret()
		sbEndpoint, _, err := h.e.reg.SetWebhookEndpoint(ctx, h.s.scope, stripeadmin.EndpointInput{ConnectionID: h.s.connection, AccountID: h.s.account,
			Profile: "SANDBOX", Enabled: true, Secrets: accounts.StripeWebhookSecrets{CurrentSecret: sbSecret}})
		if err != nil {
			t.Fatal(err)
		}
		sandbox := h.serve(t, h.e.signing, "SANDBOX")
		before := h.counts(t)
		id, body := ev("profile", nil)
		r := h.do(t, sandbox, http.MethodPost, "/v1/stripe/webhook/"+sbEndpoint, body, swhSigned(sbSecret, body))
		swhExpect(t, r, 200, swhOK)
		if rc, ok := h.receipt(t, sbEndpoint, id); !ok || rc.disposition != "QUARANTINED" || rc.reason != "profile_mismatch" || rc.hasSignal {
			t.Fatalf("wrong-profile delivery: %+v ok=%v", rc, ok)
		}
		if h.counts(t).jobs != before.jobs {
			t.Fatal("wrong-profile delivery created a job")
		}
		swhExpect(t, h.post(t, h.endpoint, body, h.secret), 200, swhOK) // same event id, correct profile
		if rc, ok := h.receipt(t, h.endpoint, id); !ok || rc.disposition != "ACCEPTED" {
			t.Fatalf("correct-profile delivery: %+v ok=%v", rc, ok)
		}
		after := h.counts(t)
		if after.jobs != before.jobs+1 || after.signals != before.signals+1 {
			t.Fatalf("exactly one signal expected: %+v -> %+v", before, after)
		}
		// Endpoint profile != API profile is a fixed 404 (integrator ruling 6): the MOCK handler
		// does not serve the SANDBOX endpoint and the SANDBOX handler does not serve the MOCK one.
		swhExpect(t, h.post(t, sbEndpoint, body, sbSecret), 404, swhErr("not_found"))
		swhExpect(t, h.do(t, sandbox, http.MethodPost, base, body, swhSigned(h.secret, body)), 404, swhErr("not_found"))
	})

	t.Run("signing_material_unavailable_is_503", func(t *testing.T) {
		// Ruling 6: a signing key that cannot be decrypted is 503 signing_unavailable (Stripe retries).
		wrong := h.serve(t, sstKeyring(t, "stripe_signing_fixture", randomBytes(32)), "PROVIDER_MOCK")
		before := h.counts(t)
		id, body := ev("nokey", nil)
		swhExpect(t, h.do(t, wrong, http.MethodPost, base, body, swhSigned(h.secret, body)), 503, swhErr("signing_unavailable"))
		if _, ok := h.receipt(t, h.endpoint, id); ok || h.counts(t) != before {
			t.Fatal("undecryptable signing material stored a receipt")
		}
	})

	t.Run("concurrency_cap_is_exactly_32_and_the_rest_are_503_busy", func(t *testing.T) {
		// 40 signed requests hold an in-flight slot while the owner keeps the endpoint row locked
		// (prepare blocks). A cap of 32 means >=8 immediate 503 busy and >=32 admitted requests.
		const total = 40
		holder, err := h.e.f.owner.Begin(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		defer holder.Rollback(context.Background())
		if _, err := holder.Exec(context.Background(), `SELECT 1 FROM payments.stripe_webhook_endpoints WHERE endpoint_id=$1::uuid FOR UPDATE`, h.endpoint); err != nil {
			t.Fatal(err)
		}
		results := make(chan swhResult, total)
		for i := 0; i < total; i++ {
			_, body := ev("cap", func(o *stripetest.EventOpts) {
				o.SessionID, o.ClientRef, o.Attempt = "cs_test_cap_"+t04Tag(), randomUUID(), randomUUID()
			})
			go func() { results <- h.post(t, h.endpoint, body, h.secret) }()
		}
		var busy, admitted int
		var first swhResult
		release := time.After(500 * time.Millisecond) // busy replies are immediate; then let the admitted ones finish
		collected := 0
		for collected < total {
			select {
			case r := <-results:
				collected++
				if r.status == 503 && r.body == swhErr("busy") {
					busy++
					first = r
				} else {
					admitted++
					if r.status != 200 && r.status != 503 {
						t.Fatalf("admitted request answered %d %q", r.status, r.body)
					}
				}
			case <-release:
				release = nil
				if err := holder.Rollback(context.Background()); err != nil {
					t.Fatal(err)
				}
			case <-time.After(15 * time.Second):
				t.Fatal("in-flight admissions did not finish")
			}
		}
		if busy < total-32 || admitted < 32 {
			t.Fatalf("cap is not exactly 32: busy=%d admitted=%d of %d", busy, admitted, total)
		}
		if first.header.Get("Retry-After") != "5" || first.header.Get("Cache-Control") != "no-store" {
			t.Fatalf("busy reply headers: retry-after=%q cache=%q", first.header.Get("Retry-After"), first.header.Get("Cache-Control"))
		}
	})

	t.Run("constructor_negatives", func(t *testing.T) {
		ctx := context.Background()
		for name, call := range map[string]func() error{
			"nil_ctx": func() error {
				_, err := stripewebhook.NewInbox(nil, h.ingress, h.e.signing, "PROVIDER_MOCK")
				return err
			},
			"nil_keyring":     func() error { _, err := stripewebhook.NewInbox(ctx, h.ingress, nil, "PROVIDER_MOCK"); return err },
			"unknown_profile": func() error { _, err := stripewebhook.NewInbox(ctx, h.ingress, h.e.signing, "PROD"); return err },
			"nil_pool":        func() error { _, err := stripewebhook.NewInbox(ctx, nil, h.e.signing, "PROVIDER_MOCK"); return err },
		} {
			if err := call(); err == nil || err.Error() != stripewebhook.ErrConfig.Error() && err.Error() != stripewebhook.ErrDatabase.Error() {
				t.Fatalf("%s: err=%v", name, err)
			}
		}
		// The handler is only reachable through an Inbox: a nil inbox is refused.
		if _, err := stripewebhook.NewHandler(nil); err == nil {
			t.Fatal("NewHandler accepted a nil inbox")
		}
		// Neither the merchant runtime pool nor the worker pool may stand in for the ingress role.
		for name, dsn := range map[string]string{"runtime": bcRole(t, h.e.f, "commerce_runtime"), "worker": bcRole(t, h.e.f, "commerce_worker")} {
			pool, err := platform.OpenPool(ctx, dsn)
			if err != nil {
				continue // the runtime opener may itself refuse; either way it is not an ingress pool
			}
			t.Cleanup(pool.Close)
			if _, err := stripewebhook.NewInbox(ctx, pool, h.e.signing, "PROVIDER_MOCK"); err == nil || err.Error() != stripewebhook.ErrDatabase.Error() {
				t.Fatalf("%s pool accepted as ingress: %v", name, err)
			}
		}
	})

	t.Run("no_forbidden_material_in_logs_or_database", func(t *testing.T) {
		// Sentinels ride in a correctly signed event (customer PII, hosted URL, extra fields).
		tag := t04Tag()
		sentinels := []string{"swh-email-" + tag + "@example.invalid", "Swh Sentinel Buyer " + tag,
			"https://checkout.stripe.com/c/pay/cs_swh_" + tag, "swh-client-secret-" + tag, "swh-card-4242-" + tag}
		id, body := ev("pii", func(o *stripetest.EventOpts) {
			o.Extra = map[string]any{"customer_details": map[string]any{"email": sentinels[0], "name": sentinels[1]},
				"url": sentinels[2], "client_secret": sentinels[3], "payment_method_details": map[string]any{"card": sentinels[4]}}
		})
		sig := stripetest.SignWebhook(h.secret, body, time.Now())
		r := h.do(t, h.server, http.MethodPost, base, body, func(r *http.Request) {
			r.Header.Set("Content-Type", "application/json")
			r.Header.Set("Stripe-Signature", sig)
		})
		swhExpect(t, r, 200, swhOK)
		if _, ok := h.receipt(t, h.endpoint, id); !ok {
			t.Fatal("PII event not admitted")
		}
		// Also provoke the failure paths so their logs are captured.
		h.do(t, h.server, http.MethodPost, base, body, func(r *http.Request) {
			r.Header.Set("Content-Type", "application/json")
			r.Header.Set("Stripe-Signature", strings.Replace(sig, "v1=", "v1=0", 1))
		})
		sentinels = append(sentinels, sig, sig[strings.Index(sig, "v1=")+3:], h.secret, strings.TrimPrefix(h.secret, "whsec_"), h.s.secret, string(body), tag)
		logs := h.logs.String()
		for _, s := range sentinels[:len(sentinels)-1] {
			if strings.Contains(logs, s) {
				t.Fatalf("logs contain a forbidden value (%d bytes checked)", len(s))
			}
		}
		if hits := swhScan(t, h.e.f, sentinels[0], sentinels[1], sentinels[2], sentinels[3], sentinels[4], sig, h.secret, h.s.secret, string(body)); len(hits) != 0 {
			t.Fatalf("database rows store a forbidden value: %v", hits)
		}
		if strings.Contains(r.body, tag) {
			t.Fatal("response echoes input")
		}
	})
}

// attempt2 starts an attempt on a second order of the SAME store/account and pins
// a session for it (signal-cap fixture; the endpoint's account matches).
func (h *swhHarness) attempt2(t *testing.T) (attempt, session string) {
	t.Helper()
	st := h.s
	st.p = sstMoreHold(t, h.s.p)
	res, err := st.begin(h.e.svc, t04Key("swh-other"), st.input("zh-TW"))
	if err != nil {
		t.Fatal(err)
	}
	session, _ = sstPin(t, h.e.f, res.AttemptID)
	return res.AttemptID, session
}
