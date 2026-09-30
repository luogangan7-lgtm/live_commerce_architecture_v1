package foundation_test

// Shared MOCK/HTTP_PG environment of the refund and manual-fulfilment gates (RF04–RF09,
// MF03–MF06). Prefix `rfx`.
//
// It extends the SP08 harness (`sflEnv`: an isolated PG 18 container, real River, the same
// assembly functions the binaries call, the independent stripetest fake) with:
//   - orders paid through the REAL capture path (fake pays the session, a signed webhook wakes the
//     real signal worker, the real capture worker applies it): no fabricated facts;
//   - merchant principals: the store's creator (fixture principal) plus extra members, each with an
//     explicit grant set (owner-pool INSERT into identity.store_grants, disclosed: the fixture
//     stores are not created through create_initial_store, so 0065 grants do not apply to them;
//     OP01 covers the onboarding grant itself);
//   - the merchant HTTP handler over the runtime pool with an insert-only River client for
//     river_payment (the same shape cmd/api builds), and small request/await helpers.
//
// Disclosed owner-pool fixtures (evidence "controlled SQL faults and aged timestamps"): ageRefund
// (session_replication_role=replica on payments.stripe_refunds only, all CHECK-linked timestamps
// move together) and grants/sessions inserts. Nothing else is fabricated.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"
	"livecommerce/internal/httpapi"
	"livecommerce/internal/integrations/psp/stripe/stripetest"
	"livecommerce/internal/inventory"
	"livecommerce/internal/platform"
)

type rfxEnv struct {
	*sflEnv
	jobs    *river.Client[pgx.Tx]
	handler http.Handler
	mu      sync.Mutex
	// requested lists every refund id an accepted request created (any test of the env).
	requested []string
}

func rfxNew(t *testing.T, opts ...func(*rfxEnv)) *rfxEnv {
	t.Helper()
	e := &rfxEnv{sflEnv: sflNew(t, nil)}
	jobs, err := river.NewClient(riverpgxv5.New(e.f.runtime), &river.Config{Schema: "river_payment"})
	if err != nil {
		t.Fatalf("merchant refund jobs client: %v", err)
	}
	e.jobs = jobs
	e.handler = httpapi.NewHandler(e.f.runtime, httpapi.Options{RefundJobs: jobs})
	return e
}

// rfxOrder is one order paid through the real capture path.
type rfxOrder struct {
	s        sstStore
	attempt  string
	session  string
	pi       string
	order    string
	endpoint string
	secret   string
	captured int64
}

func (o rfxOrder) store() string { return o.s.p.f.storeA1 }
func (o rfxOrder) token() string { return o.s.p.f.tokens["a"] } // the store creator's merchant session

// pay pins a Stripe session, has the fake pay it, delivers the signed webhook and waits for the
// CAPTURED fact and the pinned PaymentIntent (the refund contract's precondition).
func (e *rfxEnv) pay(t *testing.T, s sstStore, endpoint, secret string) rfxOrder {
	t.Helper()
	res, session := e.pinned(t, s)
	if !e.fake.SetState(session, "complete", "paid") {
		t.Fatal("fake pay")
	}
	if status := e.deliver(t, endpoint, secret, sflEvent(res.AttemptID, session)); status != 200 {
		t.Fatalf("checkout webhook answered %d", status)
	}
	e.awaitFact(t, res.AttemptID, "CAPTURED")
	e.await(t, "payment intent pinned", res.AttemptID, 30*time.Second, `SELECT payment_intent_id IS NOT NULL FROM payments.stripe_sessions WHERE attempt_id=$1`)
	var captured int64
	if err := e.f.owner.QueryRow(context.Background(), `SELECT amount_minor FROM payments.facts WHERE attempt_id=$1 AND kind='CAPTURED'`, res.AttemptID).Scan(&captured); err != nil {
		t.Fatal(err)
	}
	return rfxOrder{s: s, attempt: res.AttemptID, session: session, pi: e.fake.PaymentIntentForSession(session), order: s.p.hold.OrderID, endpoint: endpoint, secret: secret, captured: captured}
}

// ensureStock tops up the store's single SKU through the real inventory adjustment API (merchant
// scope, ledger row) when fewer than 4 units are available: every order holds 2 of the 10 units
// psSetup provisions, so a gate with many orders in one store needs more stock.
func (e *rfxEnv) ensureStock(t *testing.T, o rfxOrder) {
	t.Helper()
	p := o.s.p
	var available, version int64
	if err := e.f.owner.QueryRow(context.Background(), `SELECT on_hand-reserved-allocated-unavailable,version FROM inventory.balances WHERE tenant_id=$1 AND store_id=$2 AND sku_id=$3`,
		p.f.tenantA, p.f.storeA1, p.stock.skus[0].ID).Scan(&available, &version); err != nil {
		t.Fatalf("read balance: %v", err)
	}
	if available >= 4 {
		return
	}
	_, err := t04Scoped(context.Background(), p.f, p.f.tokens["a"], p.f.storeA1, "inventory:write", func(tx pgx.Tx, scope platform.Scope) (inventory.Balance, error) {
		return inventory.AdjustOnHand(context.Background(), tx, scope, t04Key("rfx-restock"), inventory.Adjustment{WarehouseID: p.stock.warehouse.ID, SKUID: p.stock.skus[0].ID, Delta: 200, ExpectedVersion: version, Reason: "rfx fixture restock"})
	})
	if err != nil {
		t.Fatalf("restock: %v", err)
	}
}

// payMore adds another buyer/order to the same store and pays it (same account, same endpoint).
func (e *rfxEnv) payMore(t *testing.T, o rfxOrder) rfxOrder {
	t.Helper()
	e.ensureStock(t, o)
	s := o.s
	s.p = sstMoreHold(t, o.s.p)
	return e.pay(t, s, o.endpoint, o.secret)
}

// grant gives the store creator extra permissions (owner-pool fixture).
func (e *rfxEnv) grant(t *testing.T, o rfxOrder, perms ...string) {
	t.Helper()
	for _, p := range perms {
		mustExec(t, e.f.owner, `INSERT INTO identity.store_grants(tenant_id,store_id,principal_id,permission) VALUES($1,$2,$3,$4) ON CONFLICT DO NOTHING`,
			o.s.p.f.tenantA, o.store(), o.s.p.f.principalA, p)
	}
}

// member creates one more merchant principal of the same tenant with exactly perms on the
// store (plus store:read) and a live merchant session; it returns the bearer token and the principal id.
func (e *rfxEnv) member(t *testing.T, o rfxOrder, perms ...string) (token, principal string) {
	t.Helper()
	ctx := context.Background()
	token, principal = randomToken(), randomUUID()
	mustExec(t, e.f.owner, `INSERT INTO identity.principals(id) VALUES($1)`, principal)
	mustExec(t, e.f.owner, `INSERT INTO identity.memberships(tenant_id,principal_id) VALUES($1,$2)`, o.s.p.f.tenantA, principal)
	// Every store member holds store:read: admin-transport-v1 answers 404 without it (no readable
	// store) and 403 only for a readable store missing the route's permission.
	for _, p := range append([]string{"store:read"}, perms...) {
		mustExec(t, e.f.owner, `INSERT INTO identity.store_grants(tenant_id,store_id,principal_id,permission) VALUES($1,$2,$3,$4)`, o.s.p.f.tenantA, o.store(), principal, p)
	}
	tx, err := e.f.owner.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := insertSession(ctx, tx, token, principal, "merchant", time.Now().Add(time.Hour), nil); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	return token, principal
}

// call performs one merchant HTTP request against the real handler.
func (e *rfxEnv) call(method, path, token string, headers map[string]string, body string) (int, []byte, http.Header) {
	var r io.Reader
	if body != "" {
		r = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, path, r)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	e.handler.ServeHTTP(w, req)
	return w.Code, w.Body.Bytes(), w.Header()
}

func rfxRefundsPath(o rfxOrder) string {
	return "/v1/admin/stores/" + o.store() + "/orders/" + o.order + "/refunds"
}

func rfxBody(amount int64, reason string, expected int64) string {
	return fmt.Sprintf(`{"amount_minor":%d,"reason":%q,"expected_refundable_minor":%d}`, amount, reason, expected)
}

// request POSTs a refund with an Idempotency-Key and returns the status and decoded JSON.
func (e *rfxEnv) request(o rfxOrder, token, key, body string) (int, map[string]any) {
	hdr := map[string]string{}
	if key != "" {
		hdr["Idempotency-Key"] = key
	}
	status, raw, _ := e.call("POST", rfxRefundsPath(o), token, hdr, body)
	var out map[string]any
	_ = json.Unmarshal(raw, &out)
	if status == 201 {
		if id, _ := out["refund_id"].(string); id != "" {
			e.mu.Lock()
			e.requested = append(e.requested, id)
			e.mu.Unlock()
		}
	}
	return status, out
}

// mustRefund requests a refund and returns its id; it fails the test on anything but 201.
func (e *rfxEnv) mustRefund(t *testing.T, o rfxOrder, amount int64, reason string) string {
	t.Helper()
	expected := e.refundable(t, o)
	status, out := e.request(o, o.token(), "rfx-"+t04Tag(), rfxBody(amount, reason, expected))
	if status != 201 {
		t.Fatalf("refund request answered %d: %v", status, out)
	}
	id, _ := out["refund_id"].(string)
	if id == "" || out["state"] != "REQUESTED" {
		t.Fatalf("refund request body: %v", out)
	}
	return id
}

// list reads the merchant refund list (GET .../refunds) as decoded JSON with the raw bytes.
func (e *rfxEnv) list(t *testing.T, o rfxOrder, token string) (map[string]any, []byte) {
	t.Helper()
	status, raw, _ := e.call("GET", rfxRefundsPath(o), token, nil, "")
	if status != 200 {
		t.Fatalf("refund list answered %d: %s", status, raw)
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	return out, raw
}

func (e *rfxEnv) refundable(t *testing.T, o rfxOrder) int64 {
	t.Helper()
	l, _ := e.list(t, o, o.token())
	return int64(l["refundable_minor"].(float64))
}

// itemState returns the merchant-visible state of one refund ("" if absent).
func (e *rfxEnv) itemState(t *testing.T, o rfxOrder, refund string) string {
	t.Helper()
	l, _ := e.list(t, o, o.token())
	for _, it := range l["items"].([]any) {
		m := it.(map[string]any)
		if m["refund_id"] == refund {
			return m["state"].(string)
		}
	}
	return ""
}

// wakeRefund makes sleeping refund/signal/reconcile jobs of one refund (and its attempt) due now.
// River schedules snoozes minutes ahead; tests move the schedule, never the code under test.
func (e *rfxEnv) wakeRefund(t *testing.T, refund, attempt string) {
	t.Helper()
	mustExec(t, e.f.owner, `UPDATE river_payment.river_job SET state='available',scheduled_at=clock_timestamp()
	 WHERE kind IN ('payment_refund_v1','payment_signal_v1','payment_reconcile_v1','payment_query_v1')
	 AND args->>'operation_id'=ANY($1) AND state IN ('scheduled','retryable')`, []string{refund, attempt})
}

// awaitRefund polls cond (a bool query with $1 = refund id) making sleeping jobs due each round.
func (e *rfxEnv) awaitRefund(t *testing.T, what, refund, attempt string, timeout time.Duration, cond string, args ...any) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		var ok bool
		if err := e.f.owner.QueryRow(context.Background(), cond, append([]any{refund}, args...)...).Scan(&ok); err == nil && ok {
			return
		}
		if time.Now().After(deadline) {
			var state string
			var generation int64
			var codes []string
			_ = e.f.owner.QueryRow(context.Background(), `SELECT state,generation FROM integration.operations WHERE id=$1`, refund).Scan(&state, &generation)
			rows, _ := e.f.owner.Query(context.Background(), `SELECT reason_code FROM integration.operation_events WHERE operation_id=$1 ORDER BY id DESC LIMIT 5`, refund)
			for rows != nil && rows.Next() {
				var code string
				_ = rows.Scan(&code)
				codes = append(codes, code)
			}
			t.Fatalf("timeout waiting for %s: refund op state=%s generation=%d last codes=%v refund POSTs=%d", what, state, generation, codes, e.fake.RefundPosts())
		}
		e.wakeRefund(t, refund, attempt)
		time.Sleep(400 * time.Millisecond)
	}
}

func (e *rfxEnv) awaitRefundFact(t *testing.T, refund, attempt, kind string) {
	t.Helper()
	e.awaitRefund(t, kind+" refund fact", refund, attempt, 45*time.Second, `SELECT EXISTS(SELECT 1 FROM payments.refund_facts WHERE refund_id=$1 AND kind=$2)`, kind)
}

func (e *rfxEnv) awaitRefundCode(t *testing.T, refund, attempt, code string) {
	t.Helper()
	e.awaitRefund(t, "operation event "+code, refund, attempt, 45*time.Second, `SELECT EXISTS(SELECT 1 FROM integration.operation_events WHERE operation_id=$1 AND reason_code=$2)`, code)
}

func (e *rfxEnv) hasRefundFact(t *testing.T, refund, kind string) bool {
	return e.count(t, `SELECT count(*) FROM payments.refund_facts WHERE refund_id=$1 AND kind=$2`, refund, kind) == 1
}

func (e *rfxEnv) hasAttemptReview(t *testing.T, attempt, reason string) bool {
	return e.count(t, `SELECT count(*) FROM payments.review_cases WHERE attempt_id=$1 AND reason=$2`, attempt, reason) >= 1
}

// ageRefund moves every stored timestamp of one refund back by d, as the migration owner with
// session_replication_role=replica. Disclosed evidence: this bypasses payments.guard_stripe_refund
// (set-once) on purpose; requested_at and resend_until move together (CHECK resend_until =
// requested_at + 20 h) with the send/pin markers. Facts, operations and River rows are untouched.
func (e *rfxEnv) ageRefund(t *testing.T, refund string, d time.Duration) {
	t.Helper()
	ctx := context.Background()
	tx, err := e.f.owner.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `SET LOCAL session_replication_role = replica`); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `UPDATE payments.stripe_refunds SET requested_at=requested_at-$2::interval, resend_until=resend_until-$2::interval,
	 first_sent_at=first_sent_at-$2::interval, last_sent_at=last_sent_at-$2::interval, pinned_at=pinned_at-$2::interval,
	 last_refresh_at=last_refresh_at-$2::interval WHERE id=$1`, refund, fmt.Sprintf("%d seconds", int64(d.Seconds()))); err != nil {
		t.Fatalf("age refund: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
}

// rfxLogEntry classifies the fake's audit log for refund traffic.
type rfxCall struct {
	kind string // "create", "pi-read", "retrieve", "list", "other"
	key  string
	pi   string
}

func (e *rfxEnv) refundCalls() (out []rfxCall) {
	for _, r := range e.fake.Requests() {
		switch {
		case r.Method == http.MethodPost && r.Path == "/v1/refunds":
			out = append(out, rfxCall{kind: "create", key: r.IdempotencyKey})
		case r.Method == http.MethodGet && strings.HasPrefix(r.Path, "/v1/payment_intents/"):
			out = append(out, rfxCall{kind: "pi-read", pi: strings.TrimPrefix(r.Path, "/v1/payment_intents/")})
		case r.Method == http.MethodGet && r.Path == "/v1/refunds":
			out = append(out, rfxCall{kind: "list"})
		case r.Method == http.MethodGet && strings.HasPrefix(r.Path, "/v1/refunds/"):
			out = append(out, rfxCall{kind: "retrieve"})
		}
	}
	return
}

// assertOnePreSendReadPerFirstCreate proves "the pre-send charge read occurs before every first POST"
// (RD9): between two distinct create keys (and before the first) there is at least one
// expand=latest_charge read of the PaymentIntent.
func (e *rfxEnv) assertPreSendReads(t *testing.T) {
	t.Helper()
	seen := map[string]bool{}
	read := false
	for _, c := range e.refundCalls() {
		switch c.kind {
		case "pi-read":
			read = true
		case "create":
			if !seen[c.key] {
				if !read {
					t.Fatalf("first POST of key %s had no PaymentIntent read before it (RD9)", c.key)
				}
				seen[c.key] = true
				read = false
			}
		}
	}
}

// distinctCreateKeys groups the fake's POST /v1/refunds keys; the caller asserts <=1 per refund.
func (e *rfxEnv) distinctCreateKeys() map[string]int {
	out := map[string]int{}
	for _, k := range e.fake.RefundCreateKeys() {
		out[k]++
	}
	return out
}

func (e *rfxEnv) startWorker(t *testing.T) func() { return e.start(t, true) }

// wakeAllRefunds makes every sleeping refund/signal/reconcile job of the order's refunds due now (no *testing.T:
// used from HTTP control handlers of the browser gates).
func (e *rfxEnv) wakeAllRefunds(o rfxOrder) {
	_, _ = e.f.owner.Exec(context.Background(), `UPDATE river_payment.river_job SET state='available',scheduled_at=clock_timestamp()
	 WHERE kind IN ('payment_refund_v1','payment_signal_v1','payment_reconcile_v1') AND state IN ('scheduled','retryable')
	 AND (args->>'operation_id'=$1 OR args->>'operation_id' IN (SELECT id::text FROM payments.stripe_refunds WHERE attempt_id=$1::uuid))`, o.attempt)
}

// stopAllWorkers stops every payment worker this env started (each stop func is idempotent).
func (e *rfxEnv) stopAllWorkers() {
	for _, stop := range e.stops {
		stop()
	}
}

// pollRefund is awaitRefund WITHOUT making snoozed jobs due: for tests whose assertion is the
// natural backoff itself.
func (e *rfxEnv) pollRefund(t *testing.T, what, refund string, timeout time.Duration, cond string, args ...any) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		var ok bool
		if err := e.f.owner.QueryRow(context.Background(), cond, append([]any{refund}, args...)...).Scan(&ok); err == nil && ok {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("timeout waiting for %s (refund POSTs=%d)", what, e.fake.RefundPosts())
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// awaitSend waits until the fake has seen n POSTs carrying this refund's key.
func (e *rfxEnv) awaitSend(t *testing.T, refund, attempt string, n int) {
	t.Helper()
	deadline := time.Now().Add(45 * time.Second)
	for e.keysOfRefund(refund) < n {
		if time.Now().After(deadline) {
			t.Fatalf("refund %s: %d of %d expected POSTs reached the provider", refund, e.keysOfRefund(refund), n)
		}
		e.wakeRefund(t, refund, attempt)
		time.Sleep(250 * time.Millisecond)
	}
}

func (e *rfxEnv) keysOfRefund(refund string) (n int) {
	for _, k := range e.fake.RefundCreateKeys() {
		if k == "lc:stripe:refund:v1:"+refund {
			n++
		}
	}
	return
}

// rfxPost / rfxDeliverRaw send a pre-rendered signed webhook body to the real handler.
func (e *rfxEnv) deliverRaw(t *testing.T, endpoint, secret string, body []byte) int {
	t.Helper()
	status := 0
	for try := 0; try < 6; try++ {
		req, err := http.NewRequest(http.MethodPost, e.hook.URL+"/v1/stripe/webhook/"+endpoint, bytes.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Stripe-Signature", stripetest.SignWebhook(secret, body, time.Now()))
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		if status = res.StatusCode; status != 503 {
			return status
		}
		time.Sleep(500 * time.Millisecond)
	}
	return status
}

// rfxTables lists every table whose row counts prove "zero ledger/reservation/order/work-item deltas".
var rfxNoEffectTables = []string{"inventory.ledger", "inventory.reservations", "inventory.balances", "checkout.orders", "fulfillment.payment_work_items", "payments.facts"}

func (e *rfxEnv) fingerprint(t *testing.T, o rfxOrder) map[string]string {
	t.Helper()
	out := map[string]string{}
	q := map[string]string{
		"ledger":       `SELECT md5(coalesce(string_agg(l::text,'|' ORDER BY l::text),'')) FROM inventory.ledger l WHERE l.tenant_id=$1 AND l.store_id=$2`,
		"balances":     `SELECT md5(coalesce(string_agg(b::text,'|' ORDER BY b::text),'')) FROM inventory.balances b WHERE b.tenant_id=$1 AND b.store_id=$2`,
		"reservations": `SELECT md5(coalesce(string_agg(r::text,'|' ORDER BY r::text),'')) FROM inventory.reservations r WHERE r.tenant_id=$1 AND r.store_id=$2`,
		"orders":       `SELECT md5(coalesce(string_agg(o::text,'|' ORDER BY o::text),'')) FROM checkout.orders o WHERE o.tenant_id=$1 AND o.store_id=$2`,
		"work_items":   `SELECT md5(coalesce(string_agg(w::text,'|' ORDER BY w::text),'')) FROM fulfillment.payment_work_items w WHERE w.tenant_id=$1 AND w.store_id=$2`,
		"facts":        `SELECT md5(coalesce(string_agg(f::text,'|' ORDER BY f::text),'')) FROM payments.facts f WHERE f.tenant_id=$1 AND f.store_id=$2`,
	}
	for k, sql := range q {
		var h string
		if err := e.f.owner.QueryRow(context.Background(), sql, o.s.p.f.tenantA, o.store()).Scan(&h); err != nil {
			t.Fatalf("fingerprint %s: %v", k, err)
		}
		out[k] = h
	}
	return out
}

func (e *rfxEnv) assertUnchanged(t *testing.T, o rfxOrder, before map[string]string, what string) {
	t.Helper()
	after := e.fingerprint(t, o)
	for k, v := range before {
		if after[k] != v {
			t.Fatalf("%s changed %s (RD6: a refund never writes ledger, reservations, order state, facts or work items)", what, k)
		}
	}
}

// queuedBehind counts backends whose lock wait chain reaches holder. PostgreSQL queues the second
// waiter on a row on the TUPLE lock held by the first waiter (pg_blocking_pids = the first waiter), so
// a direct "holder = ANY(pg_blocking_pids(pid))" count never exceeds 1 for one row (observed in
// output/r1-integration/wave3/debug-race.log). The platform lock_timeout is 1 s, so callers must
// release the holder promptly once the count is reached.
func (e *rfxEnv) queuedBehind(t *testing.T, holder int) int {
	t.Helper()
	var n int
	if err := e.f.owner.QueryRow(context.Background(), `WITH RECURSIVE w(pid) AS (
	  SELECT a.pid FROM pg_stat_activity a WHERE $1::int = ANY(pg_blocking_pids(a.pid))
	  UNION SELECT a.pid FROM pg_stat_activity a JOIN w ON w.pid = ANY(pg_blocking_pids(a.pid)))
	 SELECT count(*) FROM w`, holder).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}
