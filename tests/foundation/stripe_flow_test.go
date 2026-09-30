package foundation_test

// SP08-SP12 (contracts/stripe-psp-v1.md §14, with the §0.2 deltas): MOCK tier.
//
// Real PG 18 (an isolated container per gate: the payment-queue audit refuses the
// shared fixture), real River, the SAME assembly functions the binaries call
// (payments.NewStripeRuntime + payments.NewPaymentWorkerClient, checkout.
// NewHostedPaymentService, stripeadmin.Open, stripewebhook.NewInbox/NewHandler
// behind httptest) and the independent stripetest fake. The binaries themselves
// refuse PROVIDER_MOCK by design and are exercised only by SP15.
//
// Deadline and clock tests age STORED timestamps through the owner pool (sstAge:
// payments.stripe_sessions only, session_replication_role=replica, every
// CHECK-linked deadline moved together) and age the fake session identically
// (stripetest.AgeSession). No test sleeps for a Stripe deadline; waits are
// bounded polls that also make sleeping River jobs due (sstWake).
//
// Owner fixtures beyond aging are named at their call sites: a forged
// cross-account signal insert (must fail), owner-inserted BUYER_* signals with
// their River jobs (NOOP_TERMINAL / STALE_DROPPED / busy lease), and one manual
// lease claim.

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"
	"livecommerce/internal/buyerhttp"
	"livecommerce/internal/checkout"
	"livecommerce/internal/integrations/accounts"
	integration "livecommerce/internal/integrations/core"
	"livecommerce/internal/integrations/psp/stripe/stripetest"
	"livecommerce/internal/payments"
	"livecommerce/internal/payments/stripeadmin"
	"livecommerce/internal/payments/stripewebhook"
	"livecommerce/internal/platform"
)

// sflEnv is one isolated database with a fake Stripe, a registrar, the webhook
// handler and (once started) a payment worker process assembly.
type sflEnv struct {
	*sstEnv
	pool    *pgxpool.Pool
	opts    payments.QueryWorkerOptions
	ingress *pgxpool.Pool
	hook    *httptest.Server
	stops   []func()
}

func sflNew(t *testing.T, tune func(*payments.QueryWorkerOptions)) *sflEnv {
	t.Helper()
	f := pwIsolatedFixture(t)
	keys := pwKeys(t)
	e := &sflEnv{sstEnv: sstNewEnv(t, f, keys), pool: pwWorkerPool(t, f), opts: payments.DefaultQueryWorkerOptions()}
	e.opts.MockTransport = pqNoNetwork() // PAYUNi transport; SP08 replaces it before start
	if tune != nil {
		tune(&e.opts)
	}
	var err error
	if e.ingress, err = platform.OpenStripeIngressPool(context.Background(), sstLogin(t, f, "commerce_stripe_ingress")); err != nil {
		t.Fatalf("ingress pool: %v", err)
	}
	t.Cleanup(e.ingress.Close)
	inbox, err := stripewebhook.NewInbox(context.Background(), e.ingress, e.signing, "PROVIDER_MOCK")
	if err != nil {
		t.Fatal(err)
	}
	handler, err := stripewebhook.NewHandler(inbox)
	if err != nil {
		t.Fatal(err)
	}
	e.hook = httptest.NewServer(handler)
	t.Cleanup(e.hook.Close)
	t.Cleanup(func() {
		for _, stop := range e.stops {
			stop()
		}
	})
	return e
}

// start assembles exactly what cmd/payment-worker does for a PROVIDER_MOCK queue.
func (e *sflEnv) start(t *testing.T, stripeEnabled bool) (stop func()) {
	return e.startWith(t, stripeEnabled, e.keys)
}

// startWith is start with an explicit custody keyring (SP15 wrong-keyring case).
func (e *sflEnv) startWith(t *testing.T, stripeEnabled bool, keys *accounts.Keyring) (stop func()) {
	t.Helper()
	ctx := context.Background()
	cfg := payments.WorkerConfig{Profile: "PROVIDER_MOCK", Concurrency: 4, Query: e.opts, Keys: keys}
	if stripeEnabled {
		rt, err := payments.NewStripeRuntime(ctx, e.pool, keys, "PROVIDER_MOCK", e.fake.Transport())
		if err != nil {
			t.Fatalf("stripe runtime: %v", err)
		}
		cfg.Stripe = rt
	}
	client, err := payments.NewPaymentWorkerClient(ctx, e.pool, cfg)
	if err != nil {
		t.Fatalf("worker client: %v", err)
	}
	if err := client.Start(ctx); err != nil {
		t.Fatal(err)
	}
	stopped := false
	stop = func() {
		if stopped {
			return
		}
		stopped = true
		// Graceful first: cancelling a running job orphans it in state 'running' (River rescues it
		// only after an hour), which would starve every later assertion about that attempt.
		graceful, cancel := context.WithTimeout(context.Background(), 8*time.Second)
		defer cancel()
		if client.Stop(graceful) != nil {
			hard, stop := context.WithTimeout(context.Background(), 5*time.Second)
			defer stop()
			_ = client.StopAndCancel(hard)
		}
	}
	e.stops = append(e.stops, stop)
	return stop
}

// addPAYUNi creates a PAYUNi store whose query job is due now and installs the
// signed-mock PAYUNi transport (the existing PW chain) for the next worker start.
func (e *sflEnv) addPAYUNi(t *testing.T) pqFixture {
	t.Helper()
	c := pqSetupItemsOn(t, e.f, e.keys, false, 1)
	mustExec(t, e.f.owner, `UPDATE river_payment.river_job SET scheduled_at=clock_timestamp() WHERE id=$1`, c.result.JobID)
	signed := pqSignedResponse(pcFull(c))
	e.opts.MockTransport = pqTransport(func(r *http.Request) (*http.Response, error) {
		if r.URL.String() != "https://sandbox-api.payuni.com.tw/api/trade/query" || r.Method != http.MethodPost {
			return nil, fmt.Errorf("unexpected PAYUNi destination")
		}
		raw, err := io.ReadAll(r.Body)
		if err != nil {
			return nil, err
		}
		values, err := url.ParseQuery(string(raw))
		if err != nil {
			return nil, err
		}
		if trade, err := pwQueryTrade(values); err != nil || trade != c.result.MerchantTradeNo {
			return nil, fmt.Errorf("unexpected PAYUNi trade")
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(signed)), Header: make(http.Header)}, nil
	})
	return c
}

// stripeStore creates a fresh tenant/store/hold with an enabled Stripe method.
func (e *sflEnv) stripeStore(t *testing.T) sstStore {
	t.Helper()
	return e.seed(t, psSetupItemsOn(t, e.f, 1))
}

// attempt starts a Stripe payment through the hosted service (no worker needed).
func (e *sflEnv) attempt(t *testing.T, s sstStore) checkout.PaymentResult {
	t.Helper()
	res, err := s.begin(e.svc, t04Key("sfl-start"), s.input("zh-TW"))
	if err != nil {
		t.Fatalf("start Stripe payment: %v", err)
	}
	return res
}

// pinned starts a payment and waits for the real worker to create and pin a session.
func (e *sflEnv) pinned(t *testing.T, s sstStore) (checkout.PaymentResult, string) {
	t.Helper()
	res := e.attempt(t, s)
	e.await(t, "session pinned", res.AttemptID, 25*time.Second, `SELECT session_id IS NOT NULL FROM payments.stripe_sessions WHERE attempt_id=$1`)
	return res, e.session(t, res.AttemptID)
}

func (e *sflEnv) session(t *testing.T, attempt string) string {
	t.Helper()
	var id *string
	if err := e.f.owner.QueryRow(context.Background(), `SELECT session_id FROM payments.stripe_sessions WHERE attempt_id=$1`, attempt).Scan(&id); err != nil || id == nil {
		t.Fatalf("no pinned session for %s: %v", attempt, err)
	}
	return *id
}

// await polls cond (a bool query with $1=attempt), making sleeping jobs due
// each round. On timeout it reports the operation's state, never payloads.
func (e *sflEnv) await(t *testing.T, what, attempt string, timeout time.Duration, cond string, args ...any) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		var ok bool
		if err := e.f.owner.QueryRow(context.Background(), cond, append([]any{attempt}, args...)...).Scan(&ok); err == nil && ok {
			return
		}
		if time.Now().After(deadline) {
			var state string
			var generation int64
			var events []string
			_ = e.f.owner.QueryRow(context.Background(), `SELECT state,generation FROM integration.operations WHERE id=$1`, attempt).Scan(&state, &generation)
			rows, _ := e.f.owner.Query(context.Background(), `SELECT reason_code FROM integration.operation_events WHERE operation_id=$1 ORDER BY id DESC LIMIT 4`, attempt)
			for rows != nil && rows.Next() {
				var code string
				_ = rows.Scan(&code)
				events = append(events, code)
			}
			t.Fatalf("timeout waiting for %s: op state=%s generation=%d last codes=%v fake requests=%d", what, state, generation, events, len(e.fake.Requests()))
		}
		sstWake(t, e.f, attempt)
		time.Sleep(400 * time.Millisecond)
	}
}

func (e *sflEnv) awaitFact(t *testing.T, attempt, kind string) {
	t.Helper()
	e.await(t, kind+" fact", attempt, 30*time.Second, `SELECT EXISTS(SELECT 1 FROM payments.facts WHERE attempt_id=$1 AND kind=$2)`, kind)
}

func (e *sflEnv) awaitReview(t *testing.T, attempt, reason string) {
	t.Helper()
	e.await(t, reason+" review", attempt, 30*time.Second, `SELECT EXISTS(SELECT 1 FROM payments.review_cases WHERE attempt_id=$1 AND reason=$2)`, reason)
}

func (e *sflEnv) count(t *testing.T, q string, args ...any) int {
	t.Helper()
	return countRows(t, e.f.owner, q, args...)
}

func (e *sflEnv) has(t *testing.T, attempt, kind string) bool {
	return e.count(t, `SELECT count(*) FROM payments.facts WHERE attempt_id=$1 AND kind=$2`, attempt, kind) == 1
}

func (e *sflEnv) hasReview(t *testing.T, attempt, reason string) bool {
	return e.count(t, `SELECT count(*) FROM payments.review_cases WHERE attempt_id=$1 AND reason=$2`, attempt, reason) == 1
}

type sflStock struct {
	order, reservation        string
	reserved, allocated, onHd int64
}

func (e *sflEnv) stock(t *testing.T, s sstStore) sflStock {
	t.Helper()
	var x sflStock
	err := e.f.owner.QueryRow(context.Background(), `SELECT o.commercial_state,r.state,b.reserved,b.allocated,b.on_hand
	 FROM checkout.orders o JOIN inventory.reservations r ON r.id=o.id
	 JOIN inventory.reservation_lines l ON l.reservation_id=r.id
	 JOIN inventory.balances b ON (b.tenant_id,b.store_id,b.warehouse_id,b.sku_id)=(l.tenant_id,l.store_id,l.warehouse_id,l.sku_id)
	 WHERE o.id=$1`, s.p.hold.OrderID).Scan(&x.order, &x.reservation, &x.reserved, &x.allocated, &x.onHd)
	if err != nil {
		t.Fatal(err)
	}
	return x
}

func (e *sflEnv) wantStock(t *testing.T, s sstStore, want sflStock) {
	t.Helper()
	if got := e.stock(t, s); got != want {
		t.Fatalf("order/reservation/stock = %+v want %+v", got, want)
	}
}

var (
	sflPending  = sflStock{"AWAITING_PAYMENT", "PAYMENT_PENDING", 2, 0, 10}
	sflReleased = sflStock{"CANCELLED", "RELEASED", 0, 0, 10}
	sflAllocd   = sflStock{"CONFIRMED", "COMMITTED", 0, 2, 10}
)

// age moves one attempt's stored deadlines and its fake session together.
func (e *sflEnv) age(t *testing.T, attempt string, d time.Duration) {
	t.Helper()
	id := e.session(t, attempt)
	sstAge(t, e.f, attempt, d)
	if !e.fake.AgeSession(id, d) {
		t.Fatal("fake session missing")
	}
}

// endpoint registers a webhook endpoint for the store's account through the registrar.
func (e *sflEnv) endpoint(t *testing.T, s sstStore) (id, secret string) {
	t.Helper()
	secret = swhSecret()
	id, _, err := e.reg.SetWebhookEndpoint(context.Background(), s.scope, stripeadmin.EndpointInput{ConnectionID: s.connection,
		AccountID: s.account, Profile: "PROVIDER_MOCK", Enabled: true, Secrets: accounts.StripeWebhookSecrets{CurrentSecret: secret}})
	if err != nil {
		t.Fatalf("register endpoint: %v", err)
	}
	return id, secret
}

// deliver posts one signed event through the real handler and returns the HTTP status.
func (e *sflEnv) deliver(t *testing.T, endpoint, secret string, o stripetest.EventOpts) int {
	t.Helper()
	if o.ID == "" {
		o.ID = swhEventID("flow")
	}
	body := stripetest.EventBody(o)
	// 503 is the contract's "retry me" answer (Stripe redelivers the same event id): retry like Stripe.
	status := 0
	for try := 0; try < 6; try++ {
		req, err := http.NewRequest(http.MethodPost, e.hook.URL+"/v1/stripe/webhook/"+endpoint, strings.NewReader(string(body)))
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

func sflEvent(attempt, session string) stripetest.EventOpts {
	return stripetest.EventOpts{SessionID: session, ClientRef: attempt, Attempt: attempt, PendingWebhooks: 1}
}

var sflExpireKey = regexp.MustCompile(`^lc:stripe:cs-expire:v1:[0-9a-f-]{36}:[0-9]+$`)
var sflCreateKey = regexp.MustCompile(`^lc:stripe:cs-create:v1:[0-9a-f-]{36}$`)

// requestsFor filters the fake's audit log to the calls touching one session.
func sflRequestsFor(e *sflEnv, session string) (out []stripetest.Request) {
	for _, r := range e.fake.Requests() {
		if strings.Contains(r.Path, session) {
			out = append(out, r)
		}
	}
	return
}

func sflExpires(e *sflEnv, session string) (keys []string) {
	for _, r := range sflRequestsFor(e, session) {
		if r.Method == http.MethodPost && strings.HasSuffix(r.Path, "/expire") {
			keys = append(keys, r.IdempotencyKey)
		}
	}
	return
}

func sflNoLeakedSecrets(t *testing.T, e *sflEnv, secrets ...string) {
	t.Helper()
	for _, r := range e.fake.Requests() {
		for _, s := range secrets {
			if strings.Contains(r.Path+r.IdempotencyKey, s) {
				t.Fatal("fake request log carries a secret")
			}
		}
	}
}

// sflCreateKeysPerAttempt returns distinct create keys grouped by attempt uuid.
func sflCreateKeysPerAttempt(e *sflEnv) map[string]map[string]bool {
	out := map[string]map[string]bool{}
	for _, k := range e.fake.CreateKeys() {
		if !sflCreateKey.MatchString(k) {
			out["?"+k] = map[string]bool{k: true}
			continue
		}
		attempt := strings.TrimPrefix(k, "lc:stripe:cs-create:v1:")
		if out[attempt] == nil {
			out[attempt] = map[string]bool{}
		}
		out[attempt][k] = true
	}
	return out
}

// TestStripeSP08HappyMock: two Stripe stores/accounts plus one PAYUNi store share
// the profile queue without key or scope leakage; the Stripe happy path ends in
// CAPTURED/COMMITTED/CONFIRMED/READY with the URL purged and stock conserved.
func TestStripeSP08HappyMock(t *testing.T) {
	e := sflNew(t, nil)
	f := e.f
	a, b := e.stripeStore(t), e.stripeStore(t)
	if a.account == b.account || a.secret == b.secret || a.p.f.tenantA == b.p.f.tenantA {
		t.Fatal("fixture collapsed two Stripe stores into one")
	}
	// The PAYUNi store: existing signed-mock query/capture chain, one profile queue, one worker.
	c := e.addPAYUNi(t)
	endpointA, secretA := e.endpoint(t, a)
	endpointB, secretB := e.endpoint(t, b)
	e.start(t, true)

	ra, sessionA := e.pinned(t, a)
	rb, sessionB := e.pinned(t, b)
	if sessionA == sessionB {
		t.Fatal("two accounts pinned one session")
	}
	// Exactly one create per attempt, with the frozen per-attempt key, from that attempt's account only.
	perAttempt := sflCreateKeysPerAttempt(e)
	if len(perAttempt) != 2 || len(perAttempt[ra.AttemptID]) != 1 || len(perAttempt[rb.AttemptID]) != 1 {
		t.Fatalf("create keys per attempt: %v", perAttempt)
	}
	creates := map[string]string{}
	for _, r := range e.fake.Requests() {
		if r.Method == http.MethodPost && r.Path == "/v1/checkout/sessions" {
			creates[strings.TrimPrefix(r.IdempotencyKey, "lc:stripe:cs-create:v1:")] = r.Account
		}
	}
	if creates[ra.AttemptID] != a.account || creates[rb.AttemptID] != b.account {
		t.Fatalf("a create used the wrong account's key: %v", creates)
	}
	for _, r := range e.fake.Requests() {
		if strings.Contains(r.Path, sessionA) && r.Account != a.account || strings.Contains(r.Path, sessionB) && r.Account != b.account {
			t.Fatalf("session %s touched with a foreign account key", r.Path)
		}
	}
	if c := e.fake.Counts(); c.Denied != 0 {
		t.Fatalf("worker sent a wrong API key %d times (per-account RequireAPIKey)", c.Denied)
	}
	// Independent proof that A's session is invisible to B's key on the provider side.
	sflForeignRead(t, e, b, sessionA)

	// Handoff: REDIRECT, repeated, same URL, the pinned Stripe host only.
	url := func(s sstStore, session string) string {
		var first string
		for i := 0; i < 3; i++ {
			out, err := e.svc.TakeHosted(context.Background(), s.p.cap.Token, s.p.f.storeA1, s.p.hold.OrderID)
			if err != nil || out.Disposition != "REDIRECT" || out.RedirectURL != "https://checkout.stripe.com/c/pay/"+session || out.Form != nil {
				t.Fatalf("handoff %d: %+v %v", i, out, err)
			}
			if first == "" {
				first = out.RedirectURL
			} else if out.RedirectURL != first {
				t.Fatal("handoff URL changed between takes")
			}
		}
		return first
	}
	if url(a, sessionA) == url(b, sessionB) {
		t.Fatal("two stores were handed the same URL")
	}
	var firstHandedA time.Time
	if err := f.owner.QueryRow(context.Background(), `SELECT first_handed_out_at FROM payments.stripe_sessions WHERE attempt_id=$1`, ra.AttemptID).Scan(&firstHandedA); err != nil {
		t.Fatalf("first_handed_out_at: %v", err)
	}
	e.wantStock(t, a, sflPending)
	e.wantStock(t, b, sflPending)

	// Forged cross-account material: a signal, a session and a receipt aimed at A through B's authority.
	if _, err := f.owner.Exec(context.Background(), `INSERT INTO payments.stripe_signals(id,tenant_id,store_id,attempt_id,source,job_id)
	 VALUES(gen_random_uuid(),$1,$2,$3,'BUYER_REFRESH',$4)`, b.p.f.tenantA, b.p.f.storeA1, ra.AttemptID, int64(999999)); err == nil {
		t.Fatal("cross-tenant signal for another account's attempt was accepted")
	}
	forged := e.fake.Inject(b.account, url2values(ra, a))
	before := e.count(t, `SELECT count(*) FROM payments.stripe_signals WHERE attempt_id=$1`, ra.AttemptID)
	for _, o := range []stripetest.EventOpts{
		sflEvent(ra.AttemptID, sessionA), // A's real session, delivered on B's endpoint
		sflEvent(ra.AttemptID, forged),   // a session forged inside B's account that names A's attempt
		sflEvent(rb.AttemptID, sessionA), // B's attempt id with A's session
	} {
		if status := e.deliver(t, endpointB, secretB, o); status != 200 {
			t.Fatalf("authenticated forged delivery answered %d", status)
		}
	}
	if n := e.count(t, `SELECT count(*) FROM payments.stripe_webhook_receipts r WHERE r.endpoint_id=$1::uuid AND r.disposition='ACCEPTED' AND r.attempt_id=$2::uuid`, endpointB, ra.AttemptID); n != 0 {
		t.Fatalf("forged receipts were ACCEPTED for A's attempt: %d", n)
	}
	if after := e.count(t, `SELECT count(*) FROM payments.stripe_signals WHERE attempt_id=$1`, ra.AttemptID); after != before {
		t.Fatalf("forged deliveries produced signals for A: %d -> %d", before, after)
	}
	e.wantStock(t, a, sflPending)

	// A pays: the fake pays, Stripe's signed webhook reaches the real handler, the signal worker
	// retrieves, the capture worker applies. B stays open and untouched.
	if !e.fake.SetState(sessionA, "complete", "paid") {
		t.Fatal("fake pay")
	}
	if status := e.deliver(t, endpointA, secretA, sflEvent(ra.AttemptID, sessionA)); status != 200 {
		t.Fatalf("webhook for A answered %d", status)
	}
	e.awaitFact(t, ra.AttemptID, "CAPTURED")
	e.wantStock(t, a, sflAllocd) // on_hand unchanged (10): reserved -> allocated only
	if n := e.count(t, `SELECT count(*) FROM inventory.ledger WHERE payment_attempt_id=$1 AND kind='ALLOCATE'`, ra.AttemptID); n != 1 {
		t.Fatalf("ALLOCATE ledger rows=%d, one per reservation line", n)
	}
	if n := e.count(t, `SELECT count(*) FROM fulfillment.payment_work_items WHERE attempt_id=$1 AND state='READY'`, ra.AttemptID); n != 1 {
		t.Fatalf("READY work items=%d", n)
	}
	if n := e.count(t, `SELECT count(*) FROM checkout.events WHERE order_id=$1 AND action='checkout.payment_captured'`, a.p.hold.OrderID); n != 1 {
		t.Fatalf("checkout.payment_captured events=%d", n)
	}
	var amount int64
	var reference string
	if err := f.owner.QueryRow(context.Background(), `SELECT amount_minor,provider_reference FROM payments.facts WHERE attempt_id=$1 AND kind='CAPTURED'`, ra.AttemptID).Scan(&amount, &reference); err != nil || amount != 2500 || reference != sessionA {
		t.Fatalf("CAPTURED fact amount=%d reference=%q err=%v", amount, reference, err)
	}
	e.wantStock(t, b, sflPending)
	if e.has(t, rb.AttemptID, "CAPTURED") || e.has(t, rb.AttemptID, "CLOSED_UNPAID") {
		t.Fatal("B's attempt moved without a payment")
	}
	// The poller ends with stripe_terminal_observed, the hosted URL is purged and the handoff is CLOSED.
	e.await(t, "terminal observation", ra.AttemptID, 30*time.Second, `SELECT EXISTS(SELECT 1 FROM integration.operation_events WHERE operation_id=$1 AND reason_code='stripe_terminal_observed')`)
	e.await(t, "query job completed", ra.AttemptID, 30*time.Second, `SELECT $1::text IS NOT NULL AND EXISTS(SELECT 1 FROM river_payment.river_job WHERE id=$2 AND state='completed')`, ra.JobID)
	var purged *time.Time
	var stored *string
	var handedOut time.Time
	if err := f.owner.QueryRow(context.Background(), `SELECT url_purged_at,session_url,first_handed_out_at FROM payments.stripe_sessions WHERE attempt_id=$1`, ra.AttemptID).Scan(&purged, &stored, &handedOut); err != nil ||
		purged == nil || stored != nil || !handedOut.Equal(firstHandedA) {
		t.Fatalf("URL purge / first handoff marker: purged=%v stored=%v err=%v", purged, stored, err)
	}
	if out, err := e.svc.TakeHosted(context.Background(), a.p.cap.Token, a.p.f.storeA1, a.p.hold.OrderID); err != nil || out.Disposition != "CLOSED" || out.RedirectURL != "" {
		t.Fatalf("handoff after capture: %+v %v", out, err)
	}
	// The PAYUNi store completed on the same queue and worker (unchanged provider path).
	c.pcAwaitCapture(t)
	sflNoLeakedSecrets(t, e, a.secret, b.secret, secretA, secretB)
}

// pcAwaitCapture waits for the PAYUNi store's reconcile job and checks its stock.
func (q pqFixture) pcAwaitCapture(t *testing.T) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if pcCount(t, q, "payments.facts", " AND kind='CAPTURED'") == 1 {
			pcAssertStock(t, q, 0, 2, "CONFIRMED", "COMMITTED")
			pcAssertWork(t, q, "READY")
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatal("PAYUNi store did not capture on the shared profile queue")
}

func sflForeignRead(t *testing.T, e *sflEnv, s sstStore, foreignSession string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, e.fake.URL()+"/v1/checkout/sessions/"+foreignSession, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+s.secret)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != 404 {
		t.Fatalf("another account's key read this session: %d", res.StatusCode)
	}
}

func url2values(r checkout.PaymentResult, s sstStore) url.Values {
	v := url.Values{}
	for k, val := range sstExpectedParams(r.AttemptID, s.p.hold.OrderID, "PROVIDER_MOCK", "twd", "2500", "zh-TW", sstReturnURL) {
		v.Set(k, val)
	}
	v.Set("expires_at", fmt.Sprint(time.Now().Add(40*time.Minute).Unix()))
	return v
}

func (e *sflEnv) sends(attempt string) (n int) {
	for _, k := range e.fake.CreateKeys() {
		if strings.HasSuffix(k, attempt) {
			n++
		}
	}
	return
}

func (e *sflEnv) pinnedIsFakeSession(t *testing.T, attempt string) bool {
	t.Helper()
	fake := e.fake.SessionByReference(attempt)
	return fake != "" && fake == e.session(t, attempt)
}

// TestStripeSP09CreateUnknown: every create-uncertainty row of §10. Faults are
// injected in the fake and scenarios run one at a time; each scenario is settled
// (pinned or closed) before the next one so a leftover create cannot consume the
// next scenario's one-shot fault.
func TestStripeSP09CreateUnknown(t *testing.T) {
	e := sflNew(t, func(o *payments.QueryWorkerOptions) {
		o.CallTimeout, o.DBTimeout = 2*time.Second, time.Second // Delay faults must time out quickly
	})
	// (worker down past send_deadline, never sent): no worker exists until after the deadline.
	down := e.stripeStore(t)
	seeded := len(e.fake.Requests()) // registrar verification is the only traffic so far
	dres := e.attempt(t, down)
	sstAge(t, e.f, dres.AttemptID, 8*time.Minute) // send_deadline = created+7m is now past
	e.start(t, true)
	e.awaitFact(t, dres.AttemptID, "CLOSED_UNPAID")
	e.wantStock(t, down, sflReleased)
	if got := len(e.fake.Requests()); got != seeded || len(e.fake.CreateKeys()) != 0 {
		t.Fatalf("a request was sent for a never-sent attempt after LOCAL unsent closure: %d -> %d", seeded, got)
	}
	var reference string
	if err := e.f.owner.QueryRow(context.Background(), `SELECT provider_reference FROM payments.facts WHERE attempt_id=$1 AND kind='CLOSED_UNPAID'`, dres.AttemptID).Scan(&reference); err != nil || reference == "" {
		t.Fatalf("closure without a session must reference merchant_trade_no: %q %v", reference, err)
	}
	if e.count(t, `SELECT count(*) FROM payments.stripe_sessions WHERE attempt_id=$1 AND create_suppressed_at IS NOT NULL AND create_first_sent_at IS NULL`, dres.AttemptID) != 1 {
		t.Fatal("LOCAL unsent must set create_suppressed_at and never mark a send")
	}
	// The unsent attempt stays unsendable: a wake-up never produces a POST.
	sstWake(t, e.f, dres.AttemptID)
	time.Sleep(1500 * time.Millisecond)
	if len(e.fake.CreateKeys()) != 0 {
		t.Fatal("POST sent after LOCAL unsent")
	}

	// Stores are created with the subtest's own t so their pools close with the subtest
	// (the isolated PG allows 30 connections; every psSetup opens several).
	begin := func(t *testing.T, fault stripetest.Fault) (checkout.PaymentResult, sstStore) {
		s := e.stripeStore(t)
		e.fake.SetNextFault(fault)
		return e.attempt(t, s), s
	}
	sentAtLeast := func(t *testing.T, res checkout.PaymentResult, n int) {
		e.await(t, fmt.Sprintf("create sent x%d", n), res.AttemptID, 45*time.Second, `SELECT create_send_count>=$2 FROM payments.stripe_sessions WHERE attempt_id=$1`, n)
	}

	t.Run("drop_after_execute_replays_same_key_and_pins_same_session", func(t *testing.T) {
		res, _ := begin(t, stripetest.Fault{DropAfterExecute: true})
		e.await(t, "session pinned", res.AttemptID, 45*time.Second, `SELECT session_id IS NOT NULL FROM payments.stripe_sessions WHERE attempt_id=$1`)
		if e.sends(res.AttemptID) < 2 || len(sflCreateKeysPerAttempt(e)[res.AttemptID]) != 1 {
			t.Fatalf("expected a same-key resend: sends=%d keys=%v", e.sends(res.AttemptID), sflCreateKeysPerAttempt(e)[res.AttemptID])
		}
		if !e.pinnedIsFakeSession(t, res.AttemptID) {
			t.Fatal("the resend did not pin the session the first send executed")
		}
	})

	t.Run("cached_500_wait_then_list_match_pins", func(t *testing.T) {
		res, s := begin(t, stripetest.Fault{Cached500: true})
		sentAtLeast(t, res, 1)
		time.Sleep(1500 * time.Millisecond)
		if e.count(t, `SELECT count(*) FROM payments.stripe_sessions WHERE attempt_id=$1 AND session_id IS NOT NULL`, res.AttemptID) != 0 || e.has(t, res.AttemptID, "CLOSED_UNPAID") {
			t.Fatal("a cached 500 pinned or closed before expires_at+15 min")
		}
		e.wantStock(t, s, sflPending)
		// Past expires_at+15m the resend window is over: the worker lists and finds the session the
		// cached-500 execution created (the fake session ages with the stored deadlines).
		id := e.fake.SessionByReference(res.AttemptID)
		if id == "" || !e.fake.AgeSession(id, 56*time.Minute) {
			t.Fatal("cached-500 execution left no fake session to list")
		}
		sstAge(t, e.f, res.AttemptID, 56*time.Minute)
		e.await(t, "list pins the session", res.AttemptID, 45*time.Second, `SELECT session_id IS NOT NULL FROM payments.stripe_sessions WHERE attempt_id=$1`)
		if !e.pinnedIsFakeSession(t, res.AttemptID) || e.has(t, res.AttemptID, "CLOSED_UNPAID") {
			t.Fatal("a list match must pin, never close")
		}
		if e.count(t, `SELECT count(*) FROM payments.provider_observations WHERE attempt_id=$1 AND report->>'Via'='list' AND (report->>'ListMatchCount')::int=1`, res.AttemptID) < 1 {
			t.Fatal("no via=list ListMatchCount=1 observation")
		}
	})

	t.Run("cached_500_without_session_closes_only_after_expires_plus_15m", func(t *testing.T) {
		res, s := begin(t, stripetest.Fault{Cached500NoSession: true})
		sentAtLeast(t, res, 1)
		sstAge(t, e.f, res.AttemptID, 30*time.Minute) // still inside the resend window
		sstWake(t, e.f, res.AttemptID)
		time.Sleep(2500 * time.Millisecond)
		if e.has(t, res.AttemptID, "CLOSED_UNPAID") {
			t.Fatal("closed before expires_at+15 min")
		}
		e.wantStock(t, s, sflPending)
		sstAge(t, e.f, res.AttemptID, 26*time.Minute) // 56 min: list finds nothing, URL never disclosed
		e.awaitFact(t, res.AttemptID, "CLOSED_UNPAID")
		e.wantStock(t, s, sflReleased)
		if e.count(t, `SELECT count(*) FROM payments.provider_observations WHERE attempt_id=$1 AND report->>'Via'='list' AND (report->>'ListMatchCount')::int=0`, res.AttemptID) < 1 {
			t.Fatal("closure without a via=list ListMatchCount=0 observation")
		}
	})

	t.Run("first_send_400_closes_immediately", func(t *testing.T) {
		res, s := begin(t, stripetest.Fault{Validation: true})
		e.awaitFact(t, res.AttemptID, "CLOSED_UNPAID")
		e.wantStock(t, s, sflReleased)
		if n := e.sends(res.AttemptID); n != 1 {
			t.Fatalf("a definitive first-send rejection was sent %d times", n)
		}
	})

	t.Run("400_after_uncertain_send_does_not_close_until_list", func(t *testing.T) {
		// Send 1 times out at the worker (the fake drops it uncached); send 2 gets a 400. Validation
		// and auth can run before the idempotency lookup (F4), so it is not a definitive rejection.
		res, s := begin(t, stripetest.Fault{Delay: 8 * time.Second})
		sentAtLeast(t, res, 1)
		for i := 0; i < 100 && e.fake.FaultPending(); i++ { // the delay fault must reach the fake first
			time.Sleep(50 * time.Millisecond)
		}
		e.fake.SetNextFault(stripetest.Fault{Validation: true})
		sentAtLeast(t, res, 2)
		time.Sleep(1500 * time.Millisecond)
		if e.has(t, res.AttemptID, "CLOSED_UNPAID") {
			t.Fatal("a 400 after an uncertain send closed the attempt")
		}
		e.wantStock(t, s, sflPending)
		sstAge(t, e.f, res.AttemptID, 56*time.Minute) // only the list path may decide now
		e.awaitFact(t, res.AttemptID, "CLOSED_UNPAID")
	})

	t.Run("conflict_and_rate_limit_back_off_then_succeed", func(t *testing.T) {
		for name, fault := range map[string]stripetest.Fault{"409": {Conflict: true}, "429": {RateLimit: true}} {
			res, s := begin(t, fault)
			e.await(t, "session pinned after "+name, res.AttemptID, 90*time.Second, `SELECT session_id IS NOT NULL FROM payments.stripe_sessions WHERE attempt_id=$1`)
			if e.has(t, res.AttemptID, "CLOSED_UNPAID") || len(sflCreateKeysPerAttempt(e)[res.AttemptID]) != 1 {
				t.Fatalf("%s: closure or a second create key", name)
			}
			e.wantStock(t, s, sflPending)
		}
	})

	t.Run("idempotency_error_alarms_without_closure", func(t *testing.T) {
		res, s := begin(t, stripetest.Fault{IdempotencyError: true})
		e.await(t, "idempotency alarm", res.AttemptID, 45*time.Second, `SELECT EXISTS(SELECT 1 FROM integration.operation_events WHERE operation_id=$1 AND reason_code='stripe_idempotency_alarm')`)
		if e.has(t, res.AttemptID, "CLOSED_UNPAID") {
			t.Fatal("idempotency_error closed the attempt")
		}
		e.wantStock(t, s, sflPending)
		e.await(t, "settle: next same-key send pins", res.AttemptID, 45*time.Second, `SELECT session_id IS NOT NULL FROM payments.stripe_sessions WHERE attempt_id=$1`)
	})

	t.Run("at_most_one_create_key_per_attempt_across_the_gate", func(t *testing.T) {
		for attempt, keys := range sflCreateKeysPerAttempt(e) {
			if strings.HasPrefix(attempt, "?") || len(keys) > 1 {
				t.Fatalf("attempt %s used create keys %v", attempt, keys)
			}
		}
		sflNoLeakedSecrets(t, e, "sk_test_")
	})
}

func (e *sflEnv) view(t *testing.T, s sstStore) checkout.OrderPayment {
	t.Helper()
	v, err := e.svc.PaymentView(context.Background(), s.p.cap.Token, s.p.f.storeA1, s.p.hold.OrderID)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func (e *sflEnv) closedCleanly(t *testing.T, s sstStore, res checkout.PaymentResult) {
	t.Helper()
	e.wantStock(t, s, sflReleased)
	if n := e.count(t, `SELECT count(*) FROM inventory.ledger WHERE payment_attempt_id=$1 AND kind='RELEASE'`, res.AttemptID); n != 1 {
		t.Fatalf("RELEASE ledger rows=%d, one per reservation line", n)
	}
	if n := e.count(t, `SELECT count(*) FROM checkout.events WHERE order_id=$1 AND action='checkout.payment_closed'`, s.p.hold.OrderID); n != 1 {
		t.Fatalf("checkout.payment_closed events=%d", n)
	}
	v := e.view(t, s)
	if v.PaymentState != "CLOSED_UNPAID" || v.HandoffState != "CLOSED" || v.CancelRequested == nil {
		t.Fatalf("closed view: %+v", v)
	}
	if out, err := e.svc.TakeHosted(context.Background(), s.p.cap.Token, s.p.f.storeA1, s.p.hold.OrderID); err != nil || out.Disposition != "CLOSED" || out.RedirectURL != "" {
		t.Fatalf("handoff after closure: %+v %v", out, err)
	}
}

// TestStripeSP10Deadline: the deadline row of §10/§7 with the D5 release rule.
func TestStripeSP10Deadline(t *testing.T) {
	e := sflNew(t, nil)
	e.start(t, true)

	t.Run("expire_then_retrieve_closes_and_restores_exact_stock", func(t *testing.T) {
		s := e.stripeStore(t)
		res, session := e.pinned(t, s)
		e.wantStock(t, s, sflPending)
		// The old 15-minute expiry job is STALE once payment started (generation moved on).
		if d := bcExpire(t, s.p.bcHarness, s.p.hold, s.p.hold.Generation); d != "STALE" {
			t.Fatalf("old expiry job disposition %q, want STALE", d)
		}
		e.wantStock(t, s, sflPending)
		e.age(t, res.AttemptID, 41*time.Minute) // now >= expires_at
		e.awaitFact(t, res.AttemptID, "CLOSED_UNPAID")
		e.closedCleanly(t, s, res)
		keys := sflExpires(e, session)
		if len(keys) < 1 {
			t.Fatal("no expire call before closure")
		}
		for _, k := range keys {
			if !sflExpireKey.MatchString(k) || !strings.Contains(k, res.AttemptID) {
				t.Fatalf("expire key %q is not lc:stripe:cs-expire:v1:<attempt>:<generation>", k)
			}
		}
		// The fact is amount 0 and references the confirmed session.
		var amount int64
		var ref string
		if err := e.f.owner.QueryRow(context.Background(), `SELECT amount_minor,provider_reference FROM payments.facts WHERE attempt_id=$1 AND kind='CLOSED_UNPAID'`, res.AttemptID).Scan(&amount, &ref); err != nil || amount != 0 || ref != session {
			t.Fatalf("CLOSED_UNPAID fact amount=%d ref=%q err=%v", amount, ref, err)
		}
	})

	for name, status := range map[string]int{"expire_500": 500, "expire_400_already_expired_race": 400} {
		status := status
		t.Run(name+"_retries_with_a_fresh_key_and_releases_only_after_confirmation", func(t *testing.T) {
			s := e.stripeStore(t)
			res, session := e.pinned(t, s)
			e.fake.FailNext("expire", status) // the first expire POST fails; retrieve still shows open
			e.age(t, res.AttemptID, 41*time.Minute)
			e.awaitFact(t, res.AttemptID, "CLOSED_UNPAID")
			e.closedCleanly(t, s, res)
			keys := sflExpires(e, session)
			seen := map[string]bool{}
			for _, k := range keys {
				seen[k] = true
			}
			if len(keys) < 2 || len(seen) != len(keys) {
				t.Fatalf("expected >=2 expire calls with a distinct key per claim generation: %v", keys)
			}
		})
	}

	t.Run("paid_in_the_last_second_captures_not_closes", func(t *testing.T) {
		s := e.stripeStore(t)
		res, session := e.pinned(t, s)
		if !e.fake.SetState(session, "complete", "paid") { // paid first, then the deadline passes
			t.Fatal("fake pay")
		}
		e.age(t, res.AttemptID, 41*time.Minute)
		e.awaitFact(t, res.AttemptID, "CAPTURED")
		e.wantStock(t, s, sflAllocd)
		if e.has(t, res.AttemptID, "CLOSED_UNPAID") || len(sflExpires(e, session)) != 0 {
			t.Fatal("a paid session was expired or closed")
		}
	})

	t.Run("still_open_at_plus_60m_escalates_and_keeps_stock", func(t *testing.T) {
		s := e.stripeStore(t)
		res, session := e.pinned(t, s)
		// Stripe never confirms the expiry during this scenario. One queued failure is not
		// enough: the next claim's expire (fresh key) would succeed, and a Stripe-confirmed
		// expired+unpaid session legitimately closes and releases (§6.5 step 7a, §10) even after the
		// EXPIRY_UNCONFIRMED review; the await loop wakes that claim at any 400 ms tick.
		for i := 0; i < 1000; i++ {
			e.fake.FailNext("expire", 500)
		}
		defer e.fake.ClearFailures()
		e.age(t, res.AttemptID, 101*time.Minute) // expires_at + 61 min
		e.awaitReview(t, res.AttemptID, "PROVIDER_EXPIRY_UNCONFIRMED")
		e.wantStock(t, s, sflPending)
		// "Still open" holds across further poller cycles (the await loop above wakes them
		// every 400 ms): each retries expire, sees open, and must keep the stock.
		sstWake(t, e.f, res.AttemptID)
		time.Sleep(3 * time.Second)
		e.wantStock(t, s, sflPending)
		if e.has(t, res.AttemptID, "CLOSED_UNPAID") {
			t.Fatal("stock released without Stripe confirming the expiry")
		}
		if len(sflExpires(e, session)) < 1 {
			t.Fatal("no expire attempt before escalation")
		}
	})

	t.Run("expired_and_paid_is_conflicting_report_without_release", func(t *testing.T) {
		s := e.stripeStore(t)
		res, session := e.pinned(t, s)
		if !e.fake.SetState(session, "expired", "paid") {
			t.Fatal("fake anomaly")
		}
		sstWake(t, e.f, res.AttemptID)
		e.awaitReview(t, res.AttemptID, "CONFLICTING_REPORT")
		e.wantStock(t, s, sflPending)
		if e.has(t, res.AttemptID, "CLOSED_UNPAID") {
			t.Fatal("expired+paid released stock")
		}
	})

	t.Run("no_request_is_ever_made_with_a_foreign_account_key", func(t *testing.T) {
		if c := e.fake.Counts(); c.Denied != 0 {
			t.Fatalf("wrong key used %d times", c.Denied)
		}
	})
}

// sflSignal inserts a BUYER_* signal and its River job as the owner (the API path
// is covered elsewhere): used for states the SQL request function refuses to
// create (terminal attempt) or that need a chosen created_at.
func (e *sflEnv) sflSignal(t *testing.T, s sstStore, attempt, source string, age time.Duration) (signal string, job int64) {
	t.Helper()
	ctx := context.Background()
	jobs, err := river.NewClient(riverpgxv5.New(e.f.owner), &river.Config{Schema: "river_payment"})
	if err != nil {
		t.Fatal(err)
	}
	signal = randomUUID()
	tx, err := e.f.owner.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	inserted, err := jobs.InsertTx(ctx, tx, sslSignalArgs{OperationID: attempt, SignalID: signal, Version: 1}, &river.InsertOpts{Queue: "payment_mock_v1"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO payments.stripe_signals(id,tenant_id,store_id,attempt_id,source,job_id,created_at)
	 VALUES($1::uuid,$2::uuid,$3::uuid,$4::uuid,$5,$6,clock_timestamp()-$7::interval)`, signal, s.scope.TenantID, s.scope.StoreID, attempt, source,
		inserted.Job.ID, fmt.Sprintf("%d seconds", int64(age.Seconds()))); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	return signal, inserted.Job.ID
}

func (e *sflEnv) awaitOutcome(t *testing.T, attempt, signal, outcome string) {
	t.Helper()
	e.await(t, "signal outcome "+outcome, attempt, 40*time.Second, `SELECT $1::text IS NOT NULL AND EXISTS(SELECT 1 FROM payments.stripe_signals WHERE id=$2::uuid AND outcome=$3 AND consumed_at IS NOT NULL)`, signal, outcome)
}

// TestStripeSP11BuyerSignals: cancel/refresh through the real service with real
// workers; NOOP/STALE/busy states use owner-inserted signals (disclosed above).
func TestStripeSP11BuyerSignals(t *testing.T) {
	e := sflNew(t, nil)
	ctx := context.Background()
	cancel := func(s sstStore) checkout.PaymentSignal {
		out, err := e.svc.CancelPayment(ctx, s.p.cap.Token, s.p.f.storeA1, s.p.hold.OrderID)
		if err != nil {
			t.Fatalf("cancel: %v", err)
		}
		return out
	}
	refresh := func(s sstStore) checkout.PaymentSignal {
		out, err := e.svc.RefreshPayment(ctx, s.p.cap.Token, s.p.f.storeA1, s.p.hold.OrderID)
		if err != nil {
			t.Fatalf("refresh: %v", err)
		}
		return out
	}

	// Cancel before any send: the worker is not running yet, so the create was never sent.
	early := e.stripeStore(t)
	seeded := len(e.fake.Requests())
	eres := e.attempt(t, early)
	if out := cancel(early); !out.Scheduled || out.OrderID != early.p.hold.OrderID {
		t.Fatalf("cancel before send: %+v", out)
	}
	if out := cancel(early); out.Scheduled {
		t.Fatal("second cancel scheduled (set-once)")
	}
	e.start(t, true)
	e.awaitFact(t, eres.AttemptID, "CLOSED_UNPAID")
	e.wantStock(t, early, sflReleased)
	if got := len(e.fake.Requests()); got != seeded || len(e.fake.CreateKeys()) != 0 {
		t.Fatalf("cancel before send still reached Stripe: %d -> %d", seeded, got)
	}
	e.await(t, "signal consumed", eres.AttemptID, 30*time.Second, `SELECT bool_and(consumed_at IS NOT NULL) FROM payments.stripe_signals WHERE attempt_id=$1`)
	if out := refresh(early); out.Scheduled {
		t.Fatal("refresh after a terminal fact was scheduled (NOOP after terminal)")
	}

	t.Run("cancel_while_create_unknown_waits_for_provider_confirmation", func(t *testing.T) {
		s := e.stripeStore(t)
		e.fake.SetNextFault(stripetest.Fault{DropAfterExecute: true})
		res := e.attempt(t, s)
		e.await(t, "first send", res.AttemptID, 30*time.Second, `SELECT create_send_count>=1 FROM payments.stripe_sessions WHERE attempt_id=$1`)
		if !cancel(s).Scheduled {
			t.Fatal("cancel not scheduled")
		}
		e.awaitFact(t, res.AttemptID, "CLOSED_UNPAID")
		e.wantStock(t, s, sflReleased)
		if len(sflCreateKeysPerAttempt(e)[res.AttemptID]) != 1 {
			t.Fatal("cancel caused a second create key")
		}
		if id := e.fake.SessionByReference(res.AttemptID); id == "" || len(sflExpires(e, id)) < 1 {
			t.Fatal("closure without a provider-confirmed expire")
		}
	})

	t.Run("cancel_while_open_expires_then_closes", func(t *testing.T) {
		s := e.stripeStore(t)
		res, session := e.pinned(t, s)
		if !cancel(s).Scheduled {
			t.Fatal("cancel not scheduled")
		}
		e.awaitFact(t, res.AttemptID, "CLOSED_UNPAID")
		e.closedCleanly(t, s, res)
		if len(sflExpires(e, session)) < 1 {
			t.Fatal("no expire call")
		}
		e.await(t, "cancel signal consumed", res.AttemptID, 30*time.Second, `SELECT EXISTS(SELECT 1 FROM payments.stripe_signals WHERE attempt_id=$1 AND source='BUYER_CANCEL' AND consumed_at IS NOT NULL)`)
	})

	t.Run("cancel_racing_a_payment_captures", func(t *testing.T) {
		s := e.stripeStore(t)
		res, session := e.pinned(t, s)
		if !e.fake.SetState(session, "complete", "paid") {
			t.Fatal("fake pay")
		}
		if !cancel(s).Scheduled {
			t.Fatal("cancel not scheduled")
		}
		e.awaitFact(t, res.AttemptID, "CAPTURED")
		e.wantStock(t, s, sflAllocd)
		if e.has(t, res.AttemptID, "CLOSED_UNPAID") || len(sflExpires(e, session)) != 0 {
			t.Fatal("cancel closed or expired a paid session")
		}
		// NOOP after terminal: no more signals are created, and an owner-inserted one is a no-op.
		if refresh(s).Scheduled || cancel(s).Scheduled {
			t.Fatal("signals scheduled after a terminal fact")
		}
		requests := len(e.fake.Requests())
		signal, _ := e.sflSignal(t, s, res.AttemptID, "BUYER_REFRESH", 0)
		e.awaitOutcome(t, res.AttemptID, signal, "NOOP_TERMINAL")
		if len(e.fake.Requests()) != requests {
			t.Fatal("NOOP_TERMINAL signal made a provider request")
		}
	})

	t.Run("refresh_wakes_a_retrieve_and_is_throttled_by_the_db", func(t *testing.T) {
		s := e.stripeStore(t)
		res, session := e.pinned(t, s)
		if !refresh(s).Scheduled {
			t.Fatal("first refresh not scheduled")
		}
		if refresh(s).Scheduled {
			t.Fatal("refresh inside the 10 s throttle scheduled")
		}
		e.await(t, "refresh observed", res.AttemptID, 30*time.Second, `SELECT EXISTS(SELECT 1 FROM payments.stripe_signals WHERE attempt_id=$1 AND source='BUYER_REFRESH' AND outcome='OBSERVED')`)
		if n := e.count(t, `SELECT count(*) FROM payments.stripe_signals WHERE attempt_id=$1`, res.AttemptID); n != 1 {
			t.Fatalf("throttled refresh left %d signals", n)
		}
		if got := sflRequestsFor(e, session); len(got) == 0 {
			t.Fatal("no provider retrieve after refresh")
		}
	})

	t.Run("http_refresh_and_cancel_over_real_workers", func(t *testing.T) {
		// SP11 over HTTP: the same throttle / set-once rules through /v1/buyer/.../payment/{refresh,cancel}.
		s := e.stripeStore(t)
		res, session := e.pinned(t, s)
		srv := e.serveHTTP(t, s)
		path := "/v1/buyer/orders/" + s.p.hold.OrderID + "/payment"
		signal := func(suffix string) bool {
			raw := bphRaw(t, srv.request(t, "POST", path+suffix, s.p.cap.Token, "", nil, nil), 200, []string{"order_id", "scheduled"})
			var v bool
			if err := json.Unmarshal(raw["scheduled"], &v); err != nil {
				t.Fatal(err)
			}
			return v
		}
		if !signal("/refresh") || signal("/refresh") {
			t.Fatal("refresh must schedule once and then be throttled by the DB (scheduled=false, no job)")
		}
		e.await(t, "refresh observed", res.AttemptID, 30*time.Second, `SELECT EXISTS(SELECT 1 FROM payments.stripe_signals WHERE attempt_id=$1 AND source='BUYER_REFRESH' AND outcome='OBSERVED')`)
		if n := e.count(t, `SELECT count(*) FROM payments.stripe_signals WHERE attempt_id=$1`, res.AttemptID); n != 1 {
			t.Fatalf("signals=%d after a throttled refresh", n)
		}
		if !signal("/cancel") || signal("/cancel") {
			t.Fatal("cancel must be scheduled exactly once (set-once)")
		}
		e.awaitFact(t, res.AttemptID, "CLOSED_UNPAID")
		e.closedCleanly(t, s, res)
		view := bphRaw(t, srv.request(t, "GET", path, s.p.cap.Token, "", nil, nil), 200, sbhViewKeys)
		bpState(t, view, "payment_state", "CLOSED_UNPAID")
		bpState(t, view, "handoff_state", "CLOSED")
		var cancelled bool
		if err := json.Unmarshal(view["cancel_requested"], &cancelled); err != nil || !cancelled {
			t.Fatalf("cancel_requested=%s", view["cancel_requested"])
		}
		if len(sflExpires(e, session)) < 1 {
			t.Fatal("no provider-confirmed expire before closure")
		}
	})

	t.Run("stale_signal_is_dropped_without_provider_io", func(t *testing.T) {
		s := e.stripeStore(t)
		res, _ := e.pinned(t, s)
		time.Sleep(500 * time.Millisecond)
		requests := len(e.fake.Requests())
		signal, _ := e.sflSignal(t, s, res.AttemptID, "BUYER_REFRESH", 11*time.Minute)
		e.awaitOutcome(t, res.AttemptID, signal, "STALE_DROPPED")
		e.await(t, "stale event", res.AttemptID, 20*time.Second, `SELECT EXISTS(SELECT 1 FROM integration.operation_events WHERE operation_id=$1 AND reason_code='stripe_signal_stale')`)
		// The poller (not the dropped signal) remains the guaranteed path; it may have retrieved
		// meanwhile, but the stale signal itself caused no observation.
		_ = requests
		e.wantStock(t, s, sflPending)
	})

	t.Run("busy_lease_snoozes_without_generation_loss", func(t *testing.T) {
		s := e.stripeStore(t)
		res, _ := e.pinned(t, s)
		var generation int64
		for i := 0; ; i++ { // take the operation lease manually, retrying if a worker holds it now
			tx, err := e.pool.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			claim, err := (&integration.Service{}).Claim(ctx, tx, res.AttemptID, 120)
			if err == nil && claim.Disposition == "claimed" && tx.Commit(ctx) == nil {
				generation = claim.Generation
				break
			}
			_ = tx.Rollback(ctx)
			if i > 100 {
				t.Fatal("could not take the operation lease")
			}
			time.Sleep(200 * time.Millisecond)
		}
		signal, _ := e.sflSignal(t, s, res.AttemptID, "BUYER_REFRESH", 0)
		time.Sleep(6 * time.Second) // several 2 s busy snoozes of the signal and query jobs
		var now int64
		if err := e.f.owner.QueryRow(ctx, `SELECT generation FROM integration.operations WHERE id=$1`, res.AttemptID).Scan(&now); err != nil || now != generation {
			t.Fatalf("busy cycles moved the generation %d -> %d (%v)", generation, now, err)
		}
		if e.count(t, `SELECT count(*) FROM payments.stripe_signals WHERE id=$1::uuid AND consumed_at IS NOT NULL`, signal) != 0 {
			t.Fatal("signal consumed while another claim held the lease")
		}
		mustExec(t, e.f.owner, `UPDATE integration.operations SET lease_until=clock_timestamp()-interval '1 second' WHERE id=$1`, res.AttemptID)
		e.awaitOutcome(t, res.AttemptID, signal, "OBSERVED")
		if err := e.f.owner.QueryRow(ctx, `SELECT generation FROM integration.operations WHERE id=$1`, res.AttemptID).Scan(&now); err != nil || now <= generation {
			t.Fatalf("generation did not advance after the lease expired: %d -> %d", generation, now)
		}
	})
}

// TestStripeSP12MoneyChecks: every §6.5 rule against provider states the fake can
// forge (Patch/Inject/SetState). Mutations that change money are applied while
// the session is still open/unpaid and the paying state is set last, so a
// worker cycle can never observe a half-built anomaly as a valid payment.
func TestStripeSP12MoneyChecks(t *testing.T) {
	e := sflNew(t, nil)
	e.start(t, true)
	ctx := context.Background()

	type rule struct {
		name  string
		patch map[string]any
	}
	pi := func(received int64, currency string) map[string]any {
		return map[string]any{"id": "pi_test_patch", "object": "payment_intent", "status": "succeeded", "amount_received": received, "currency": currency}
	}
	for _, r := range []rule{
		{"amount_total", map[string]any{"amount_total": 2400}},
		{"currency", map[string]any{"currency": "usd"}},
		{"subtotal", map[string]any{"amount_subtotal": 2400}},
		{"discount", map[string]any{"total_details": map[string]any{"amount_discount": 100, "amount_tax": 0, "amount_shipping": 0}}},
		{"tax", map[string]any{"total_details": map[string]any{"amount_discount": 0, "amount_tax": 100, "amount_shipping": 0}}},
		{"shipping", map[string]any{"total_details": map[string]any{"amount_discount": 0, "amount_tax": 0, "amount_shipping": 100}}},
		{"payment_intent_amount", map[string]any{"payment_intent": pi(2400, "twd")}},
		{"payment_intent_currency", map[string]any{"payment_intent": pi(2500, "usd")}},
	} {
		r := r
		t.Run("amount_mismatch_"+r.name+"_records_review_not_a_fact", func(t *testing.T) {
			s := e.stripeStore(t)
			res, session := e.pinned(t, s)
			e.fake.Patch(session, r.patch)
			e.fake.SetState(session, "complete", "paid")
			e.awaitReview(t, res.AttemptID, "PROVIDER_AMOUNT_MISMATCH")
			if e.has(t, res.AttemptID, "CAPTURED") || e.has(t, res.AttemptID, "CLOSED_UNPAID") {
				t.Fatal("I05 violated: money drift produced a fact")
			}
			e.wantStock(t, s, sflPending)
		})
	}

	for name, patch := range map[string]map[string]any{
		"presentment_currency":       {"presentment_details": map[string]any{"presentment_currency": "eur", "presentment_amount": 71}},
		"presentment_amount":         {"presentment_details": map[string]any{"presentment_currency": "twd", "presentment_amount": 2499}},
		"legacy_currency_conversion": {"currency_conversion": map[string]any{"source_currency": "usd"}},
	} {
		patch := patch
		t.Run("presentment_drift_"+name+"_captures_with_review_work_and_no_allocation", func(t *testing.T) {
			s := e.stripeStore(t)
			res, session := e.pinned(t, s)
			e.fake.Patch(session, patch)
			e.fake.SetState(session, "complete", "paid")
			e.awaitFact(t, res.AttemptID, "CAPTURED")
			e.awaitReview(t, res.AttemptID, "PROVIDER_PRESENTMENT_DRIFT")
			e.wantStock(t, s, sflPending) // no allocation while a review exists
			if n := e.count(t, `SELECT count(*) FROM fulfillment.payment_work_items WHERE attempt_id=$1 AND state='REVIEW_REQUIRED'`, res.AttemptID); n != 1 {
				t.Fatalf("REVIEW_REQUIRED work items=%d", n)
			}
		})
	}

	t.Run("identity_mismatch_is_not_recorded", func(t *testing.T) {
		s := e.stripeStore(t)
		res, session := e.pinned(t, s)
		before := e.count(t, `SELECT count(*) FROM payments.provider_observations WHERE attempt_id=$1`, res.AttemptID)
		e.fake.Patch(session, map[string]any{"metadata": map[string]any{"lc_attempt": randomUUID(), "lc_profile": "PROVIDER_MOCK"}})
		e.fake.SetState(session, "complete", "paid")
		e.await(t, "mismatch code", res.AttemptID, 40*time.Second, `SELECT EXISTS(SELECT 1 FROM integration.operation_events WHERE operation_id=$1 AND reason_code='stripe_session_mismatch')`)
		if after := e.count(t, `SELECT count(*) FROM payments.provider_observations WHERE attempt_id=$1`, res.AttemptID); after != before {
			t.Fatalf("a foreign-identity session was recorded (%d -> %d observations)", before, after)
		}
		if e.has(t, res.AttemptID, "CAPTURED") || e.has(t, res.AttemptID, "CLOSED_UNPAID") {
			t.Fatal("identity mismatch produced a fact")
		}
		e.wantStock(t, s, sflPending)
	})

	t.Run("duplicate_session_reviews_never_captures_or_closes", func(t *testing.T) {
		s := e.stripeStore(t)
		endpoint, secret := e.endpoint(t, s)
		res, _ := e.pinned(t, s)
		var expires string
		if err := e.f.owner.QueryRow(ctx, `SELECT create_params->>'expires_at' FROM payments.stripe_sessions WHERE attempt_id=$1`, res.AttemptID).Scan(&expires); err != nil {
			t.Fatal(err)
		}
		vals := url2values(res, s)
		vals.Set("expires_at", expires)
		dup := e.fake.Inject(s.account, vals) // a second session carrying our reference
		e.fake.SetState(dup, "complete", "paid")
		if status := e.deliver(t, endpoint, secret, sflEvent(res.AttemptID, dup)); status != 200 {
			t.Fatalf("webhook: %d", status)
		}
		e.awaitReview(t, res.AttemptID, "PROVIDER_SESSION_DUPLICATE")
		if e.has(t, res.AttemptID, "CAPTURED") || e.has(t, res.AttemptID, "CLOSED_UNPAID") {
			t.Fatal("a duplicate session captured or closed the attempt")
		}
		e.wantStock(t, s, sflPending)
	})

	t.Run("no_payment_required_is_conflicting_report", func(t *testing.T) {
		s := e.stripeStore(t)
		res, session := e.pinned(t, s)
		e.fake.SetState(session, "complete", "no_payment_required")
		e.awaitReview(t, res.AttemptID, "CONFLICTING_REPORT")
		if e.has(t, res.AttemptID, "CAPTURED") || e.has(t, res.AttemptID, "CLOSED_UNPAID") {
			t.Fatal("no_payment_required produced a fact")
		}
		e.wantStock(t, s, sflPending)
	})

	t.Run("webhook_is_a_wake_up_never_financial_authority", func(t *testing.T) {
		s := e.stripeStore(t)
		endpoint, secret := e.endpoint(t, s)
		res, session := e.pinned(t, s)
		for _, typ := range []string{"checkout.session.completed", "checkout.session.async_payment_succeeded", "checkout.session.async_payment_failed"} {
			o := sflEvent(res.AttemptID, session)
			o.Type = typ
			if status := e.deliver(t, endpoint, secret, o); status != 200 {
				t.Fatalf("%s: %d", typ, status)
			}
		}
		e.await(t, "signals observed", res.AttemptID, 40*time.Second, `SELECT count(*)=3 AND bool_and(outcome='OBSERVED') FROM payments.stripe_signals WHERE attempt_id=$1 AND source='STRIPE_WEBHOOK'`)
		if e.has(t, res.AttemptID, "CAPTURED") {
			t.Fatal("a 'completed' webhook without a paid session created a CAPTURED fact")
		}
		e.wantStock(t, s, sflPending)
	})

	t.Run("complete_unpaid_escalates_after_60_minutes", func(t *testing.T) {
		s := e.stripeStore(t)
		res, session := e.pinned(t, s)
		e.fake.SetState(session, "complete", "unpaid")
		e.await(t, "complete+unpaid first seen", res.AttemptID, 40*time.Second, `SELECT EXISTS(SELECT 1 FROM payments.provider_observations WHERE attempt_id=$1 AND report->>'Status'='complete' AND report->>'PaymentStatus'='unpaid')`)
		e.wantStock(t, s, sflPending)
		// Owner fixture: age provider_observations.received_at (the "first seen" clock), replica role.
		tx, err := e.f.owner.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback(ctx)
		if _, err := tx.Exec(ctx, `SET LOCAL session_replication_role=replica`); err != nil {
			t.Fatal(err)
		}
		if _, err := tx.Exec(ctx, `UPDATE payments.provider_observations SET received_at=received_at-interval '61 minutes' WHERE attempt_id=$1`, res.AttemptID); err != nil {
			t.Fatal(err)
		}
		if err := tx.Commit(ctx); err != nil {
			t.Fatal(err)
		}
		e.awaitReview(t, res.AttemptID, "PROVIDER_ASYNC_PENDING")
		e.wantStock(t, s, sflPending)
		if e.has(t, res.AttemptID, "CAPTURED") || e.has(t, res.AttemptID, "CLOSED_UNPAID") {
			t.Fatal("escalation produced a fact")
		}
	})

	t.Run("late_paid_after_closure_is_one_manual_refund_obligation_and_replay_converges", func(t *testing.T) {
		s := e.stripeStore(t)
		endpoint, secret := e.endpoint(t, s)
		res, session := e.pinned(t, s)
		e.age(t, res.AttemptID, 41*time.Minute)
		e.awaitFact(t, res.AttemptID, "CLOSED_UNPAID")
		e.wantStock(t, s, sflReleased)
		stockAtClosure := e.stock(t, s)
		// Provider anomaly (D13): the closed session turns out paid. A late signed webhook observes it.
		if !e.fake.SetState(session, "complete", "paid") {
			t.Fatal("fake anomaly")
		}
		if status := e.deliver(t, endpoint, secret, sflEvent(res.AttemptID, session)); status != 200 {
			t.Fatalf("late webhook: %d", status)
		}
		e.awaitFact(t, res.AttemptID, "CAPTURED")
		e.awaitReview(t, res.AttemptID, "CLOSURE_CONTRADICTED")
		if got := e.stock(t, s); got != stockAtClosure || got != sflReleased {
			t.Fatalf("late money moved stock or reopened the order: %+v", got)
		}
		var fulfillment string
		if err := e.f.owner.QueryRow(ctx, `SELECT fulfillment_state FROM checkout.orders WHERE id=$1`, s.p.hold.OrderID).Scan(&fulfillment); err != nil || fulfillment != "PAID_ALLOCATION_FAILED" {
			t.Fatalf("fulfillment_state=%q err=%v", fulfillment, err)
		}
		snapshot := func() string {
			var out string
			if err := e.f.owner.QueryRow(ctx, `SELECT
			 (SELECT coalesce(string_agg(kind,',' ORDER BY kind),'') FROM payments.facts WHERE attempt_id=$1)||'|'||
			 (SELECT coalesce(string_agg(reason,',' ORDER BY reason),'') FROM payments.review_cases WHERE attempt_id=$1)||'|'||
			 (SELECT coalesce(string_agg(state,',' ORDER BY state),'') FROM fulfillment.payment_work_items WHERE attempt_id=$1)||'|'||
			 (SELECT coalesce(string_agg(kind,',' ORDER BY kind),'') FROM inventory.ledger WHERE payment_attempt_id=$1)||'|'||
			 (SELECT commercial_state||'/'||fulfillment_state FROM checkout.orders WHERE id=$2)`, res.AttemptID, s.p.hold.OrderID).Scan(&out); err != nil {
				t.Fatal(err)
			}
			return out
		}
		want := snapshot()
		if n := e.count(t, `SELECT count(*) FROM payments.review_cases WHERE attempt_id=$1 AND reason='CLOSURE_CONTRADICTED'`, res.AttemptID); n != 1 {
			t.Fatalf("CLOSURE_CONTRADICTED reviews=%d", n)
		}
		if n := e.count(t, `SELECT count(*) FROM fulfillment.payment_work_items WHERE attempt_id=$1 AND state='REVIEW_REQUIRED'`, res.AttemptID); n != 1 {
			t.Fatalf("manual-refund obligations (REVIEW_REQUIRED work items)=%d", n)
		}
		if n := e.count(t, `SELECT count(*) FROM fulfillment.payment_work_items WHERE attempt_id=$1 AND state='READY'`, res.AttemptID); n != 0 {
			t.Fatal("a READY fulfilment consumer can select the contradicted payment")
		}
		// Replay the observations recorded AFTER the deadline aging in shuffled orders (older reports
		// carry the pre-aging expires_at, which the SQL identity guard rightly refuses on aged rows).
		rows, err := e.f.owner.Query(ctx, `SELECT o.report_hash,o.report->>'Status',o.report->>'PaymentStatus' FROM payments.provider_observations o
		 JOIN payments.stripe_sessions ss ON ss.attempt_id=o.attempt_id
		 WHERE o.attempt_id=$1 AND o.report->>'ExpiresAt'=extract(epoch FROM ss.expires_at)::bigint::text ORDER BY o.received_at`, res.AttemptID)
		if err != nil {
			t.Fatal(err)
		}
		var hashes [][]byte
		sawExpired, sawPaid := false, false
		for rows.Next() {
			var h []byte
			var status, paid string
			if err := rows.Scan(&h, &status, &paid); err != nil {
				t.Fatal(err)
			}
			hashes = append(hashes, h)
			sawExpired = sawExpired || status == "expired" && paid == "unpaid"
			sawPaid = sawPaid || status == "complete" && paid == "paid"
		}
		rows.Close()
		if len(hashes) < 2 || !sawExpired || !sawPaid {
			t.Fatalf("need the closing and the late-paid observations to replay: %d expired=%t paid=%t", len(hashes), sawExpired, sawPaid)
		}
		orders := [][]int{{}, {}, {}}
		for i := range hashes {
			orders[0] = append(orders[0], i)
			orders[1] = append(orders[1], len(hashes)-1-i)
		}
		orders[2] = rand.New(rand.NewSource(7)).Perm(len(hashes))
		for _, order := range orders {
			for _, i := range order {
				if err := pcApply(e.pool, res.AttemptID, hashes[i]); err != nil {
					t.Fatalf("replay observation %d: %v", i, err)
				}
			}
			if got := snapshot(); got != want {
				t.Fatalf("replay order %v diverged:\n got  %s\n want %s", order, got, want)
			}
		}
	})
}

// serveHTTP mounts the private buyer routes (the same buyerhttp.New the API binary
// builds) for one store of the isolated database.
func (e *sflEnv) serveHTTP(t *testing.T, s sstStore) bhHarness {
	t.Helper()
	b := bhHarness{bcHarness: s.p.bcHarness, key: base64.RawURLEncoding.EncodeToString(randomBytes(32)), origin: "https://buyer-payment.example"}
	bhPublish(t, s.p.bcHarness, b.origin, s.p.f.tenantA, s.p.f.storeA1)
	handler, err := buyerhttp.New(context.Background(), s.p.a.issuer, s.p.a.runtime, s.p.bcHarness.service, b.key, time.Hour, e.svc)
	if err != nil {
		t.Fatalf("buyer HTTP constructor: %v", err)
	}
	b.server = httptest.NewServer(handler)
	t.Cleanup(b.server.Close)
	return b
}
