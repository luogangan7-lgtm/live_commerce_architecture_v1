//go:build browser

package foundation_test

// SP18 / SU06-SU09 (contracts/stripe-buyer-ui-v1.md §9, contracts/stripe-psp-v1.md §14 SP18):
// production Next -> private Go -> task-owned PG 18 -> payment worker, driven by Playwright
// Chromium (tests/storefront/stripe-browser.mjs).
//
// Tiers (labelled in every result.json):
//   SANDBOX  SP18-*    real api.stripe.com test mode + real checkout.stripe.com hosted page
//                      (only the harness's CONNECT tunnel reaches Stripe hosts). Never LIVE.
//   MOCK     SU07/SU09 stripetest fake + a Chromium-routed synthetic hosted page; deadline rows
//                      use the SP10 owner-fixture clock aging (sstAge + AgeSession), disclosed.
//   OBS-*    developer-only observation of the real hosted page WITHOUT the B2 buyer UI (the Go
//                      side drives the frozen API); not part of the gate, never counted as PASS.
//
// Secrets never cross to Node: stripeNodeEnvironment strips every STRIPE_* name, every name
// defined in secrets.env and every value shaped like sk_test_/rk_test_ (TestBrowserStripeNodeEnv
// is the unit proof; runNode re-asserts before every spawn). Node receives counts through the
// /facts control server, never a URL, session id, key or email.

import (
	"bufio"
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"
	"livecommerce/internal/buyerhttp"
	"livecommerce/internal/checkout"
	"livecommerce/internal/integrations/accounts"
	stripe "livecommerce/internal/integrations/psp/stripe"
	"livecommerce/internal/integrations/psp/stripe/stripetest"
	"livecommerce/internal/inventory"
	"livecommerce/internal/payments"
	"livecommerce/internal/payments/stripeadmin"
	"livecommerce/internal/platform"
)

const (
	sbSandboxAccount = "acct_1UJDb0RusP6Wwj7e" // contracts/stripe-psp-v1.md §0.1 (not a secret)
	sbOrigin         = "https://buyer.example"
	sbReturnURL      = sbOrigin + "/payment/return"
)

var (
	sbKeyShape  = regexp.MustCompile(`^(sk|rk)_test_`)
	sbValueLeak = regexp.MustCompile(`(sk|rk)_test_`)
	// SU08 sentinels for logs (the browser side scans storage/console with the same set).
	sbLogLeaks = []*regexp.Regexp{regexp.MustCompile(`checkout\.stripe\.com/`), regexp.MustCompile(`cs_test_`),
		regexp.MustCompile(`sk_test_`), regexp.MustCompile(`rk_test_`), regexp.MustCompile(`gate\+[0-9]+@example\.com`)}
)

// sbSecretNames returns only the variable NAMES defined in secrets.env (never a value).
func sbSecretNames(path string) map[string]bool {
	names := map[string]bool{}
	file, err := os.Open(path)
	if err != nil {
		return names
	}
	defer file.Close()
	scan := bufio.NewScanner(file)
	scan.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for scan.Scan() {
		line := strings.TrimSpace(scan.Text())
		if name, _, ok := strings.Cut(line, "="); ok && regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`).MatchString(name) {
			names[name] = true
		}
	}
	return names
}

func sbSecretsPath() string {
	if p := os.Getenv("LC_SECRETS_FILE"); p != "" {
		return p
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".config", "livecommerce", "secrets.env")
}

// stripeNodeEnvironment builds the Node/Next environment from environ. browserEnvironment copies
// os.Environ(), so the stripping has to happen here (a shell unset before spawning is not enough).
func stripeNodeEnvironment(environ []string, secretNames map[string]bool, values map[string]string) []string {
	env := []string{}
	for _, entry := range environ {
		name, value, _ := strings.Cut(entry, "=")
		// LC_BROWSER_ENGINE (chromium|webkit) is the single LC_* name Node must keep: tests/storefront/browser-engine.mjs reads it (the SP18
		// hosted-checkout run on real Safari). It is not a secret; the value is still screened by sbValueLeak like every other value.
		if _, replaced := values[name]; name == "LC_BROWSER_ENGINE" && !replaced && !sbValueLeak.MatchString(value) {
			env = append(env, entry)
			continue
		}
		if _, replaced := values[name]; replaced || strings.HasPrefix(name, "COMMERCE_") || strings.HasPrefix(name, "LC_") ||
			strings.HasPrefix(name, "STRIPE_") || secretNames[name] || name == "DATABASE_URL" || name == "POSTGRES_PASSWORD" ||
			sbValueLeak.MatchString(value) {
			continue
		}
		env = append(env, entry)
	}
	for name, value := range values {
		env = append(env, name+"="+value)
	}
	return env
}

// sbEnvLeak reports the first STRIPE_-prefixed name or sk_test_/rk_test_ value in env (name only).
func sbEnvLeak(env []string) string {
	for _, entry := range env {
		name, value, _ := strings.Cut(entry, "=")
		if strings.HasPrefix(name, "STRIPE_") || sbValueLeak.MatchString(value) {
			return name
		}
	}
	return ""
}

// TestBrowserStripeNodeEnv is the unit assertion SU08 requires: no Stripe key can reach Node.
func TestBrowserStripeNodeEnv(t *testing.T) {
	dir := t.TempDir()
	secrets := filepath.Join(dir, "secrets.env")
	if err := os.WriteFile(secrets, []byte("# names only\nSTRIPE_SECRET_KEY=x\nMETA_APP_SECRET=y\nPLAIN_NAME_SECRET=z\n"), 0600); err != nil {
		t.Fatal(err)
	}
	environ := []string{"PATH=/usr/bin", "HOME=/h", "STRIPE_SECRET_KEY=sk_test_" + strings.Repeat("a", 24), "STRIPE_ACCOUNT_ID=acct_1",
		"META_APP_SECRET=abc", "PLAIN_NAME_SECRET=abc", "RENAMED_KEY=rk_test_" + strings.Repeat("b", 24), "COMMERCE_X=1", "LC_Y=1", "KEEP_ME=ok"}
	env := stripeNodeEnvironment(environ, sbSecretNames(secrets), map[string]string{"LC_STRIPE_CASE": "{}"})
	if leak := sbEnvLeak(env); leak != "" {
		t.Fatalf("Node environment leaks %s", leak)
	}
	joined := strings.Join(env, "\n")
	for _, gone := range []string{"META_APP_SECRET=", "PLAIN_NAME_SECRET=", "RENAMED_KEY=", "COMMERCE_X=", "LC_Y=", "STRIPE_SECRET_KEY=", "STRIPE_ACCOUNT_ID="} {
		for _, entry := range env {
			if strings.HasPrefix(entry, gone) {
				t.Fatalf("Node environment still contains %s", gone)
			}
		}
	}
	for _, kept := range []string{"PATH=/usr/bin", "HOME=/h", "KEEP_ME=ok", "LC_STRIPE_CASE={}"} {
		if !strings.Contains(joined, kept) {
			t.Fatalf("Node environment lost %s", kept)
		}
	}
	// The detector must be able to fail: a deliberately leaking environment is flagged.
	if sbEnvLeak([]string{"OK=1", "SNEAKY=sk_test_" + strings.Repeat("c", 24)}) != "SNEAKY" || sbEnvLeak([]string{"STRIPE_SECRET_KEY=x"}) != "STRIPE_SECRET_KEY" {
		t.Fatal("leak detector cannot fail")
	}
}

type sbLogBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *sbLogBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}
func (b *sbLogBuffer) String() string { b.mu.Lock(); defer b.mu.Unlock(); return b.buf.String() }

// sbCase is the JSON the driver receives in LC_STRIPE_CASE (no secret, URL or key).
type sbCase struct {
	Kind     string `json:"kind"` // SP18 | SU07 | SU09 | OBS
	Mode     string `json:"mode"` // SANDBOX | MOCK
	Card     string `json:"card,omitempty"`
	Mobile   bool   `json:"mobile"`
	Locale   string `json:"locale"`
	Scenario string `json:"scenario,omitempty"`
	BudgetMS int    `json:"budgetMs"`         // driver watchdog: fail with a snapshot before the Go deadline kills it
	Mutate   string `json:"mutate,omitempty"` // red-run mutation (STRIPE_BROWSER_MUTATE=su08), harness-only
}

type sbAction struct {
	name, order string
	reply       chan error
}

// sbEnv is one isolated assembly: PG, registrar, worker, hosted service, buyer HTTP, control.
type sbEnv struct {
	t        *testing.T
	mode     string
	profile  string
	f        *testFixture
	p        psHarness
	keys     *accounts.Keyring
	signing  *accounts.Keyring
	fake     *stripetest.Server
	reg      *stripeadmin.Registrar
	scope    stripeadmin.Scope
	account  string
	secret   string
	conn     string
	qual     string
	method   int64
	pool     interface{ Close() }
	svc      *checkout.HostedPaymentStarter
	api      *httptest.Server
	control  *httptest.Server
	bffKey   string
	ctlKey   string
	actions  chan sbAction
	mu       sync.Mutex
	stopper  func()
	observe  string // OBS only: the redirect URL for the driver (never logged)
	baseline sbStock
	logs     *sbLogBuffer
	workerDB interface{}
}

type sbStock struct{ Reserved, Allocated, OnHand int64 }

func sbSandboxCredentials(t *testing.T) (account, secret string) {
	t.Helper()
	account, secret = os.Getenv("STRIPE_ACCOUNT_ID"), os.Getenv("STRIPE_SECRET_KEY")
	if account != sbSandboxAccount {
		t.Fatalf("refused: STRIPE_ACCOUNT_ID must be %s", sbSandboxAccount)
	}
	if !sbKeyShape.MatchString(secret) {
		t.Fatal("refused: STRIPE_SECRET_KEY is not a test key")
	}
	return account, secret
}

// sbNew builds the assembly. SANDBOX: no mock transport anywhere (registrar, worker, API).
func sbNew(t *testing.T, mode string) *sbEnv {
	t.Helper()
	ctx := context.Background()
	e := &sbEnv{t: t, mode: mode, profile: "SANDBOX", keys: pwKeys(t), signing: sstKeyring(t, "sb_signing", randomBytes(32)),
		bffKey: randomToken(), ctlKey: randomToken(), actions: make(chan sbAction), logs: &sbLogBuffer{}}
	if mode == "MOCK" {
		e.profile = "PROVIDER_MOCK"
	}
	e.f = pwIsolatedFixture(t)
	e.p = psSetupItemsOn(t, e.f, 1)
	e.scope = stripeadmin.Scope{TenantID: e.p.f.tenantA, StoreID: e.p.f.storeA1, PrincipalID: e.p.f.principalA}
	var err error
	if mode == "SANDBOX" {
		e.account, e.secret = sbSandboxCredentials(t)
		e.reg, err = stripeadmin.Open(ctx, sstLogin(t, e.f, "commerce_payment_registrar"), e.keys, e.signing) // real api.stripe.com
	} else {
		e.fake = stripetest.New("acct_FakeBrowser0001")
		t.Cleanup(e.fake.Close)
		e.account, e.secret = "acct_T"+t04Tag(), "sk_test_"+hex.EncodeToString(randomBytes(12))
		if err = e.fake.AddAccount(e.account, e.secret); err != nil {
			t.Fatal(err)
		}
		e.reg, err = stripeadmin.Open(ctx, sstLogin(t, e.f, "commerce_payment_registrar"), e.keys, e.signing, e.fake.Transport())
	}
	if err != nil {
		t.Fatalf("open registrar: %v", err)
	}
	t.Cleanup(e.reg.Close)
	if e.conn, err = e.reg.Register(ctx, e.scope, e.account, e.secret); err != nil {
		t.Fatalf("register Stripe account: %v", err)
	}
	// TWD, not HKD: psSetupItemsOn fixes store/market/price currency to TWD (2500 = NT$25.00 for the
	// buyer's quantity 2). Real Stripe accepted the TWD probe at 2500 but rejected 100 and 1200
	// (SANDBOX finding, 2026-09-29), so the qualification probe uses the order amount; the method
	// bounds stay the contract's local prefilter (TWD 2500..99999900).
	if e.qual, err = e.reg.Qualify(ctx, e.scope, stripeadmin.QualifyInput{ConnectionID: e.conn, AccountID: e.account, SecretKey: e.secret,
		Profile: e.profile, Currency: "TWD", ReturnURL: sbReturnURL, ExpectedVersion: 1, AmountMinor: 2500}); err != nil {
		t.Fatalf("qualify Stripe method: %v", err)
	}
	e.setMethod(t, true)
	// Single-method UI (Q3): remove the fixture's PAYUNi method head; owner fixture, disclosed.
	mustExec(t, e.f.owner, `DELETE FROM payments.method_heads WHERE tenant_id=$1 AND store_id=$2 AND code='payuni_credit'`, e.p.f.tenantA, e.p.f.storeA1)
	e.topUpStock(t)
	mustExec(t, e.f.owner, `UPDATE catalog.products SET name='Synthetic browser payment product',description='Synthetic acceptance fixture' WHERE id=$1`, e.p.stock.product.ID)

	hosted, err := platform.OpenHostedPool(ctx, hpRole(t, e.f))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(hosted.Close)
	jobs, err := river.NewClient(riverpgxv5.New(hosted), &river.Config{Schema: "river_payment"})
	if err != nil {
		t.Fatal(err)
	}
	pcfg := checkout.HostedConfig{ReturnURL: sbReturnURL, NotifyURL: sbOrigin + "/payment/notify"}
	scfg := checkout.StripeHostedConfig{ReturnURL: sbReturnURL}
	if e.svc, err = checkout.NewHostedPaymentService(ctx, hosted, jobs, e.profile, e.keys, checkout.HostedProviders{PAYUNi: &pcfg, Stripe: &scfg}); err != nil {
		t.Fatalf("hosted service: %v", err)
	}
	bhPublish(t, e.p.bcHarness, sbOrigin, e.p.f.tenantA, e.p.f.storeA1)
	handler, err := buyerhttp.New(ctx, e.p.a.issuer, e.p.a.runtime, e.p.bcHarness.service, e.bffKey, time.Hour, e.svc)
	if err != nil {
		t.Fatalf("buyer HTTP constructor: %v", err)
	}
	e.api = httptest.NewServer(handler)
	t.Cleanup(e.api.Close)
	e.workerStart(t)
	// Registered after the worker so it runs BEFORE the worker stops (t.Cleanup is LIFO).
	t.Cleanup(func() { e.drain(t) })
	e.startControl()
	e.baseline = e.stock(t)
	return e
}

func (e *sbEnv) setMethod(t *testing.T, enabled bool) {
	t.Helper()
	expected := e.method
	v, err := e.reg.SetMethod(context.Background(), e.scope, stripeadmin.MethodInput{MarketID: e.p.market.ID, Country: "TW", ConnectionID: e.conn,
		QualificationID: e.qual, ExpectedVersion: expected, Enabled: enabled, Visible: true, Sort: 1, MinMinor: 2500, MaxMinor: 99999900,
		NameHans: "Stripe 测试卡付款", NameHant: "Stripe 測試卡付款", NameEN: "Stripe test card payment"})
	if err != nil {
		t.Fatalf("set Stripe method (enabled=%v): %v", enabled, err)
	}
	e.method = v
}

// topUpStock adds units through the merchant inventory API so ~15 browser orders can reserve 2 each.
func (e *sbEnv) topUpStock(t *testing.T) {
	t.Helper()
	var version int64
	if err := e.f.owner.QueryRow(context.Background(), `SELECT version FROM inventory.balances WHERE tenant_id=$1 AND store_id=$2 AND sku_id=$3`,
		e.p.f.tenantA, e.p.f.storeA1, e.p.stock.skus[0].ID).Scan(&version); err != nil {
		t.Fatal(err)
	}
	if _, err := t04Scoped(context.Background(), e.p.f, e.p.f.tokens["a"], e.p.f.storeA1, "inventory:write", func(tx pgx.Tx, scope platform.Scope) (inventory.Balance, error) {
		return inventory.AdjustOnHand(context.Background(), tx, scope, t04Key("browser-topup"), inventory.Adjustment{WarehouseID: e.p.stock.warehouse.ID,
			SKUID: e.p.stock.skus[0].ID, Delta: 100, ExpectedVersion: version, Reason: "browser gate stock top-up"})
	}); err != nil {
		t.Fatalf("stock top-up: %v", err)
	}
}

// workerStart assembles exactly what cmd/payment-worker does (SANDBOX: no mock transport at all).
func (e *sbEnv) workerStart(t *testing.T) {
	t.Helper()
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.stopper != nil {
		return
	}
	ctx := context.Background()
	pool := pwWorkerPool(t, e.f)
	opts := payments.DefaultQueryWorkerOptions()
	var rt *payments.StripeRuntime
	var err error
	if e.mode == "MOCK" {
		opts.MockTransport = pqNoNetwork() // PAYUNi transport only; Stripe goes to the fake
		rt, err = payments.NewStripeRuntime(ctx, pool, e.keys, e.profile, e.fake.Transport())
	} else {
		rt, err = payments.NewStripeRuntime(ctx, pool, e.keys, e.profile)
	}
	if err != nil {
		t.Fatalf("stripe runtime: %v", err)
	}
	client, err := payments.NewPaymentWorkerClient(ctx, pool, payments.WorkerConfig{Profile: e.profile, Concurrency: 4, Query: opts, Keys: e.keys, Stripe: rt})
	if err != nil {
		t.Fatalf("worker client: %v", err)
	}
	if err = client.Start(ctx); err != nil {
		t.Fatal(err)
	}
	e.stopper = func() {
		graceful, cancel := context.WithTimeout(context.Background(), 8*time.Second)
		defer cancel()
		if client.Stop(graceful) != nil {
			hard, stop := context.WithTimeout(context.Background(), 5*time.Second)
			defer stop()
			_ = client.StopAndCancel(hard)
		}
		pool.Close()
	}
}

func (e *sbEnv) workerStop() {
	e.mu.Lock()
	stop := e.stopper
	e.stopper = nil
	e.mu.Unlock()
	if stop != nil {
		stop()
	}
}

// drain leaves no open SANDBOX test session behind. The driver's own finally-block clicks Cancel
// (the worker's cancel path); this fallback expires whatever a failed run left open, through the
// adapter that is the only api.stripe.com dialer, and records the count.
func (e *sbEnv) drain(t *testing.T) {
	if e.mode != "SANDBOX" {
		e.workerStop()
		return
	}
	rows, err := e.f.owner.Query(context.Background(), `SELECT s.attempt_id::text, s.session_id FROM payments.stripe_sessions s
	 WHERE s.session_id IS NOT NULL AND NOT EXISTS (SELECT 1 FROM payments.facts f WHERE f.attempt_id=s.attempt_id AND f.kind IN ('CAPTURED','CLOSED_UNPAID'))`)
	if err != nil {
		t.Logf("drain: query failed")
		e.workerStop()
		return
	}
	type open struct{ attempt, session string }
	var todo []open
	for rows.Next() {
		var o open
		if rows.Scan(&o.attempt, &o.session) == nil {
			todo = append(todo, o)
		}
	}
	rows.Close()
	client, err := stripe.New(stripe.Config{SecretKey: e.secret, AccountID: e.account, Environment: "SANDBOX"})
	expired := 0
	for _, o := range todo {
		if err != nil {
			break
		}
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		if _, _, e2 := client.ExpireCheckoutSession(ctx, o.session, stripe.ExpireIdempotencyKey(o.attempt, 9000)); e2 == nil {
			expired++
		}
		cancel()
	}
	t.Logf("drain: %d open SANDBOX test session(s) found, %d expired directly (fallback; driver Cancel is the primary path)", len(todo), expired)
	e.workerStop()
}

type sbOrderFacts struct {
	Attempts      int    `json:"attempts"`
	Sessions      int    `json:"sessions"`
	Pinned        int    `json:"pinned"`
	Captured      int    `json:"captured"`
	ClosedUnpaid  int    `json:"closed_unpaid"`
	Commercial    string `json:"commercial"`
	Reservation   string `json:"reservation"`
	CancelPending bool   `json:"cancel_requested"`
}

type sbFacts struct {
	Orders       int            `json:"orders"`
	Attempts     int            `json:"attempts"`
	Sessions     int            `json:"sessions"`
	Captured     int            `json:"captured"`
	ClosedUnpaid int            `json:"closed_unpaid"`
	Reviews      int            `json:"reviews"`
	Signals      int            `json:"signals"`
	Reserved     int64          `json:"reserved"`
	Allocated    int64          `json:"allocated"`
	OnHand       int64          `json:"on_hand"`
	Reservations map[string]int `json:"reservations"`
	Order        *sbOrderFacts  `json:"order,omitempty"`
}

func (e *sbEnv) stock(t testing.TB) sbStock {
	var s sbStock
	if err := e.f.owner.QueryRow(context.Background(), `SELECT coalesce(sum(reserved),0),coalesce(sum(allocated),0),coalesce(sum(on_hand),0)
	 FROM inventory.balances WHERE tenant_id=$1 AND store_id=$2`, e.p.f.tenantA, e.p.f.storeA1).Scan(&s.Reserved, &s.Allocated, &s.OnHand); err != nil {
		t.Fatal(err)
	}
	return s
}

// facts returns counts and states only; never a URL, session id, key or email.
func (e *sbEnv) facts(ctx context.Context, order string) (sbFacts, error) {
	var out sbFacts
	store := e.p.f.storeA1
	err := e.f.owner.QueryRow(ctx, `SELECT
	 (SELECT count(*) FROM checkout.orders WHERE store_id=$1),
	 (SELECT count(*) FROM checkout.payment_attempts WHERE store_id=$1),
	 (SELECT count(*) FROM payments.stripe_sessions s JOIN checkout.payment_attempts a ON a.id=s.attempt_id WHERE a.store_id=$1),
	 (SELECT count(*) FROM payments.facts f JOIN checkout.payment_attempts a ON a.id=f.attempt_id WHERE a.store_id=$1 AND f.kind='CAPTURED'),
	 (SELECT count(*) FROM payments.facts f JOIN checkout.payment_attempts a ON a.id=f.attempt_id WHERE a.store_id=$1 AND f.kind='CLOSED_UNPAID'),
	 (SELECT count(*) FROM payments.review_cases WHERE store_id=$1),
	 (SELECT count(*) FROM payments.stripe_signals WHERE store_id=$1),
	 (SELECT coalesce(sum(reserved),0) FROM inventory.balances WHERE store_id=$1),
	 (SELECT coalesce(sum(allocated),0) FROM inventory.balances WHERE store_id=$1),
	 (SELECT coalesce(sum(on_hand),0) FROM inventory.balances WHERE store_id=$1)`, store).
		Scan(&out.Orders, &out.Attempts, &out.Sessions, &out.Captured, &out.ClosedUnpaid, &out.Reviews, &out.Signals, &out.Reserved, &out.Allocated, &out.OnHand)
	if err != nil {
		return out, err
	}
	out.Reservations = map[string]int{}
	rows, err := e.f.owner.Query(ctx, `SELECT r.state, count(*) FROM checkout.orders o JOIN inventory.reservations r ON r.id=o.id WHERE o.store_id=$1 GROUP BY 1`, store)
	if err != nil {
		return out, err
	}
	for rows.Next() {
		var state string
		var n int
		if err = rows.Scan(&state, &n); err != nil {
			rows.Close()
			return out, err
		}
		out.Reservations[state] = n
	}
	rows.Close()
	if order != "" {
		var o sbOrderFacts
		err = e.f.owner.QueryRow(ctx, `SELECT
		 (SELECT count(*) FROM checkout.payment_attempts WHERE order_id=$1::uuid AND store_id=$2),
		 (SELECT count(*) FROM payments.stripe_sessions s JOIN checkout.payment_attempts a ON a.id=s.attempt_id WHERE a.order_id=$1::uuid AND a.store_id=$2),
		 (SELECT count(*) FROM payments.stripe_sessions s JOIN checkout.payment_attempts a ON a.id=s.attempt_id WHERE a.order_id=$1::uuid AND a.store_id=$2 AND s.session_id IS NOT NULL),
		 (SELECT count(*) FROM payments.facts f JOIN checkout.payment_attempts a ON a.id=f.attempt_id WHERE a.order_id=$1::uuid AND a.store_id=$2 AND f.kind='CAPTURED'),
		 (SELECT count(*) FROM payments.facts f JOIN checkout.payment_attempts a ON a.id=f.attempt_id WHERE a.order_id=$1::uuid AND a.store_id=$2 AND f.kind='CLOSED_UNPAID'),
		 coalesce((SELECT commercial_state FROM checkout.orders WHERE id=$1::uuid AND store_id=$2),''),
		 coalesce((SELECT r.state FROM inventory.reservations r WHERE r.id=$1::uuid),''),
		 coalesce((SELECT s.cancel_requested_at IS NOT NULL FROM payments.stripe_sessions s JOIN checkout.payment_attempts a ON a.id=s.attempt_id WHERE a.order_id=$1::uuid AND a.store_id=$2 LIMIT 1),false)`,
			order, store).Scan(&o.Attempts, &o.Sessions, &o.Pinned, &o.Captured, &o.ClosedUnpaid, &o.Commercial, &o.Reservation, &o.CancelPending)
		if err != nil {
			return out, err
		}
		out.Order = &o
	}
	return out, nil
}

func (e *sbEnv) sessionOf(order string) (attempt, session string, err error) {
	var id *string
	err = e.f.owner.QueryRow(context.Background(), `SELECT s.attempt_id::text, s.session_id FROM payments.stripe_sessions s
	 JOIN checkout.payment_attempts a ON a.id=s.attempt_id WHERE a.order_id=$1::uuid AND a.store_id=$2`, order, e.p.f.storeA1).Scan(&attempt, &id)
	if err == nil && id == nil {
		err = fmt.Errorf("session not pinned yet")
	}
	if id != nil {
		session = *id
	}
	return
}

// startControl serves /facts and POST /act. Actions are executed on the TEST goroutine
// (sstAge and friends call t.Fatal), which drains e.actions while Node runs.
func (e *sbEnv) startControl() {
	e.control = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Gate-Key") != e.ctlKey {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/facts":
			facts, err := e.facts(r.Context(), r.URL.Query().Get("order"))
			if err != nil {
				http.Error(w, "fixture readback failed", 500)
				return
			}
			_ = json.NewEncoder(w).Encode(facts)
		case r.Method == http.MethodPost && r.URL.Path == "/act" && e.mode == "MOCK":
			a := sbAction{name: r.URL.Query().Get("name"), order: r.URL.Query().Get("order"), reply: make(chan error, 1)}
			select {
			case e.actions <- a:
			case <-time.After(20 * time.Second):
				http.Error(w, "busy", 503)
				return
			}
			if err := <-a.reply; err != nil {
				http.Error(w, "action failed", 409)
				return
			}
			_, _ = w.Write([]byte(`{"ok":true}`))
		case r.Method == http.MethodGet && r.URL.Path == "/observe" && e.observe != "":
			_ = json.NewEncoder(w).Encode(map[string]string{"url": e.observe}) // OBS only
		default:
			http.NotFound(w, r)
		}
	}))
	e.t.Cleanup(e.control.Close)
}

// act runs one MOCK fixture action on the test goroutine.
func (e *sbEnv) act(t *testing.T, a sbAction) error {
	t.Helper()
	switch a.name {
	case "worker-stop":
		e.workerStop()
	case "worker-start":
		e.workerStart(t)
	case "method-disable":
		e.setMethod(t, false)
	case "method-enable":
		e.setMethod(t, true)
	case "qualification-revoke": // owner fixture: the operator revokes the method qualification (SP07 drift pattern)
		qualExec(t, e.f.owner, `UPDATE payments.account_qualifications SET revoked_at=clock_timestamp() WHERE id=$1`, e.qual)
	case "binding-disable": // owner fixture: the store's Stripe binding is disabled (PAYUNi drift pattern)
		mustExec(t, e.f.owner, `UPDATE integration.bindings SET enabled=false,semantic_version=semantic_version+1
		 WHERE id=(SELECT binding_id FROM integration.merchant_accounts WHERE id=$1)`, e.conn)
	case "pay", "review", "age-cutoff":
		attempt, session, err := e.sessionOf(a.order)
		if err != nil {
			return err
		}
		switch a.name {
		case "pay":
			e.fake.SetState(session, "complete", "paid")
		case "review": // SP12 presentment drift: CAPTURED + review work, no allocation
			e.fake.Patch(session, map[string]any{"presentment_details": map[string]any{"presentment_currency": "twd", "presentment_amount": 2499}})
			e.fake.SetState(session, "complete", "paid")
		case "age-cutoff": // SP10 owner-fixture clock aging: past the handoff cutoff, before expires_at
			sstAge(t, e.f, attempt, 36*time.Minute)
			e.fake.AgeSession(session, 36*time.Minute)
		}
	default:
		return fmt.Errorf("unknown action")
	}
	return nil
}

func sbEvidenceRoot(t *testing.T) string {
	t.Helper()
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	base := os.Getenv("LC_STRIPE_EVIDENCE_ROOT")
	if base == "" {
		base = filepath.Join(root, "output", "stripe-b2-browser-tests")
	}
	if err = os.MkdirAll(base, 0o700); err != nil {
		t.Fatal(err)
	}
	return base
}

// runNode spawns the driver for one case, serving MOCK actions until it exits, and returns the
// evidence directory. The Go-side log capture and browser.log are scanned for SU08 sentinels.
func (e *sbEnv) runNode(t *testing.T, c sbCase, budget time.Duration) string {
	t.Helper()
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	evidence, err := os.MkdirTemp(sbEvidenceRoot(t), c.Mode+"-"+strings.ReplaceAll(t.Name()[strings.Index(t.Name(), "/")+1:], "/", "_")+"-")
	if err != nil {
		t.Fatal(err)
	}
	c.Mutate = os.Getenv("STRIPE_BROWSER_MUTATE")
	c.BudgetMS = int((budget - 20*time.Second).Milliseconds())
	caseJSON, _ := json.Marshal(c)
	env := stripeNodeEnvironment(os.Environ(), sbSecretNames(sbSecretsPath()), map[string]string{
		"COMMERCE_BUYER_WEB_ENABLED": "1", "COMMERCE_BUYER_API_ORIGIN": e.api.URL, "COMMERCE_BUYER_DEMO_LABEL": "1",
		"COMMERCE_BUYER_BFF_KEY": e.bffKey, "COMMERCE_BUYER_COOKIE_KEY": randomToken(), "COMMERCE_BUYER_SESSION_TTL": "3600",
		"LC_STRIPE_CASE": string(caseJSON), "LC_STRIPE_EVIDENCE": evidence, "LC_STRIPE_PRODUCT": e.p.stock.product.ID,
		"LC_STRIPE_CONTROL": e.control.URL, "LC_STRIPE_CONTROL_KEY": e.ctlKey,
	})
	if leak := sbEnvLeak(env); leak != "" { // SU08: re-asserted before every spawn
		t.Fatalf("refusing to spawn Node: environment leaks %s", leak)
	}
	ctx, cancel := context.WithTimeout(context.Background(), budget)
	defer cancel()
	cmd := exec.CommandContext(ctx, "node", "tests/storefront/stripe-browser.mjs")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Dir = root
	cmd.Env = env
	logf := browserLog(t, filepath.Join(evidence, "browser.log"))
	cmd.Stdout, cmd.Stderr = logf, logf
	prev := log.Writer()
	log.SetOutput(io.MultiWriter(prev, e.logs))
	defer log.SetOutput(prev)
	if err = cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if cmd.Process != nil {
			_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		}
	}()
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	var waitErr error
loop:
	for {
		select {
		case a := <-e.actions:
			a.reply <- e.act(t, a)
		case waitErr = <-done:
			break loop
		}
	}
	e.scanLogs(t, evidence)
	if waitErr != nil {
		_ = logf.Sync()
		if data, _ := os.ReadFile(filepath.Join(evidence, "browser.log")); bytes.Contains(data, []byte("BLOCKED: captcha")) {
			t.Fatalf("BLOCKED: captcha (recorded NOT_RUN(BLOCKED), never solved); evidence=%s", evidence)
		}
		tail := sbTail(filepath.Join(evidence, "browser.log"), 30)
		t.Fatalf("browser gate failed (%v); evidence=%s\n--- browser.log tail (redacted by the driver) ---\n%s", waitErr, evidence, tail)
	}
	return evidence
}

func sbTail(path string, lines int) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	parts := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	if len(parts) > lines {
		parts = parts[len(parts)-lines:]
	}
	return strings.Join(parts, "\n")
}

// scanLogs is the Go half of SU08: browser.log, next.log and the in-process Go log.
func (e *sbEnv) scanLogs(t *testing.T, evidence string) {
	t.Helper()
	surfaces := map[string]string{"go.log": e.logs.String()}
	for _, name := range []string{"browser.log", "next.log"} {
		data, _ := os.ReadFile(filepath.Join(evidence, name))
		surfaces[name] = string(data)
	}
	var hits []string
	for name, text := range surfaces {
		for _, re := range sbLogLeaks {
			if re.MatchString(text) {
				hits = append(hits, name+":"+re.String())
			}
		}
	}
	sort.Strings(hits)
	if len(hits) > 0 {
		t.Errorf("SU08: leak sentinel(s) in logs: %v", hits)
	}
}

type sbResult struct {
	Kind, Mode string
	Case       string          `json:"case"`
	Pass       bool            `json:"pass"`
	Orders     []string        `json:"orders"`
	Cases      []string        `json:"cases"`
	Scan       json.RawMessage `json:"scan"`
	Shots      []struct {
		Name   string `json:"name"`
		SHA256 string `json:"sha256"`
		Tier   string `json:"tier"`
	} `json:"screenshots"`
	Selectors map[string]string `json:"selectors"`
	Refused   []string          `json:"refused_hosts"`
	Unreached []struct {
		Row    string `json:"row"`
		Reason string `json:"reason"`
	} `json:"unreached_rows"`
}

func (e *sbEnv) result(t *testing.T, evidence string) sbResult {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(evidence, "result.json"))
	if err != nil {
		t.Fatalf("missing result.json in %s", evidence)
	}
	var r sbResult
	if json.Unmarshal(data, &r) != nil || !r.Pass || len(r.Cases) == 0 {
		t.Fatalf("incomplete result.json in %s", evidence)
	}
	var scan struct {
		Clean    bool     `json:"clean"`
		Surfaces []string `json:"surfaces"`
	}
	if json.Unmarshal(r.Scan, &scan) != nil || !scan.Clean || len(scan.Surfaces) < 5 {
		t.Fatalf("SU08 browser-side scan missing or dirty in %s", evidence)
	}
	return r
}

func (e *sbEnv) wantOrder(t *testing.T, order string, captured, closed int, commercial, reservation string) {
	t.Helper()
	f, err := e.facts(context.Background(), order)
	if err != nil || f.Order == nil {
		t.Fatalf("facts for order: %v", err)
	}
	o := f.Order
	if o.Attempts != 1 || o.Sessions != 1 || o.Captured != captured || o.ClosedUnpaid != closed || o.Commercial != commercial || o.Reservation != reservation {
		t.Fatalf("order facts %+v, want attempts=1 sessions=1 captured=%d closed_unpaid=%d %s/%s", *o, captured, closed, commercial, reservation)
	}
}

func TestBrowserStripeCheckout(t *testing.T) {
	if os.Getenv("STRIPE_BROWSER") != "1" || os.Getenv("STRIPE_SANDBOX") != "1" {
		t.Skip("NOT_RUN: SP18 requires STRIPE_BROWSER=1 and STRIPE_SANDBOX=1 (bash scripts/dev/test-local.sh --stripe-browser)")
	}
	if os.Getenv("LC_STRIPE_BROWSER_ACCEPTANCE") != "1" || os.Getenv("LC_TEST_DATABASE_ALLOWED") != "1" {
		t.Fatal("use scripts/dev/test-local.sh --stripe-browser")
	}
	type sp18 struct {
		name, card, scenario, locale string
		mobile                       bool
	}
	for _, c := range []sp18{
		{"SP18A_4242_desktop_en", "4242", "A", "en", false},
		{"SP18A_4242_mobile_zh-TW", "4242", "A", "zh-TW", true},
		{"SP18B_decline_cancel_desktop_zh-CN", "decline", "B", "zh-CN", false},
		{"SP18B_decline_cancel_mobile_en", "decline", "B", "en", true},
		{"SP18C_3ds_desktop_zh-TW", "3ds", "C", "zh-TW", false},
		{"SP18C_3ds_mobile_zh-CN", "3ds", "C", "zh-CN", true},
	} {
		c := c
		t.Run(c.name, func(t *testing.T) {
			e := sbNew(t, "SANDBOX")
			t.Logf("SANDBOX checkout.stripe.com test mode; no live charge; store currency TWD (fixture), qualification probe at 2500 minor")
			evidence := e.runNode(t, sbCase{Kind: "SP18", Mode: "SANDBOX", Card: c.card, Mobile: c.mobile, Locale: c.locale, Scenario: c.scenario}, 300*time.Second)
			r := e.result(t, evidence)
			if len(r.Orders) != 1 {
				t.Fatalf("expected one order, got %d", len(r.Orders))
			}
			after := e.stock(t)
			switch c.scenario {
			case "A", "C": // captured, committed, allocated
				e.wantOrder(t, r.Orders[0], 1, 0, "CONFIRMED", "COMMITTED")
				if after.Reserved != e.baseline.Reserved || after.Allocated != e.baseline.Allocated+2 || after.OnHand != e.baseline.OnHand {
					t.Fatalf("stock after capture %+v from %+v", after, e.baseline)
				}
			case "B": // closed without charge, stock restored exactly
				e.wantOrder(t, r.Orders[0], 0, 1, "CANCELLED", "RELEASED")
				if after != e.baseline {
					t.Fatalf("stock not restored exactly: %+v vs %+v", after, e.baseline)
				}
			}
			t.Logf("PASS SANDBOX %s: cases=%d shots=%d evidence=%s", c.name, len(r.Cases), len(r.Shots), evidence)
		})
	}
	t.Run("SU07_popup_reuse_dedupe_reload_return", func(t *testing.T) {
		e := sbNew(t, "MOCK")
		evidence := e.runNode(t, sbCase{Kind: "SU07", Mode: "MOCK", Locale: "en"}, 420*time.Second)
		r := e.result(t, evidence)
		t.Logf("PASS MOCK SU07: cases=%d orders=%d evidence=%s", len(r.Cases), len(r.Orders), evidence)
	})
	for _, c := range []struct {
		name   string
		mobile bool
	}{{"SU09_desktop", false}, {"SU09_mobile", true}} {
		c := c
		t.Run(c.name, func(t *testing.T) {
			e := sbNew(t, "MOCK")
			evidence := e.runNode(t, sbCase{Kind: "SU09", Mode: "MOCK", Mobile: c.mobile, Locale: "en"}, 600*time.Second)
			r := e.result(t, evidence)
			if len(r.Shots) < 3*8 {
				t.Fatalf("SU09 produced %d screenshots, want >= 24", len(r.Shots))
			}
			for _, s := range r.Shots {
				if s.Tier != "MOCK" || !regexp.MustCompile(`^[0-9a-f]{64}$`).MatchString(s.SHA256) {
					t.Fatalf("SU09 screenshot %s is not a labelled MOCK hash", s.Name)
				}
			}
			for _, u := range r.Unreached {
				t.Logf("UNREACHED ROW (Go finding, needs an integrator ruling): %s: %s", u.Row, u.Reason)
			}
			t.Logf("PASS MOCK SU09: cases=%d shots=%d unreached=%d evidence=%s", len(r.Cases), len(r.Shots), len(r.Unreached), evidence)
		})
	}
	// Developer-only: exercises every MOCK fixture action the SU07/SU09 driver calls, and the Go states the
	// contract's rows assume (cutoff -> UNAVAILABLE, method disabled, worker restart, paid + refresh), with no browser.
	t.Run("OBS_mock_actions", func(t *testing.T) {
		if os.Getenv("STRIPE_BROWSER_OBSERVE") != "1" {
			t.Skip("NOT_RUN: developer-only fixture-action smoke (STRIPE_BROWSER_OBSERVE=1 with -run '/^OBS')")
		}
		e := sbNew(t, "MOCK")
		ctx, order := context.Background(), e.p.hold.OrderID
		view := func() checkout.OrderPayment {
			v, err := e.svc.PaymentView(ctx, e.p.cap.Token, e.p.f.storeA1, order)
			if err != nil {
				t.Fatalf("view: %v", err)
			}
			return v
		}
		if v := view(); len(v.Methods) != 1 || v.Methods[0].Code != "stripe_checkout" || v.PaymentState != "NOT_STARTED" {
			t.Fatalf("fresh view %+v", v)
		}
		in := checkout.HostedInput{OrderID: order, MethodCode: "stripe_checkout", MethodVersion: e.method, Locale: "en"}
		if _, err := e.svc.BeginHosted(ctx, e.p.cap.Token, e.p.f.storeA1, t04Key("mock-actions"), in); err != nil {
			t.Fatal(err)
		}
		deadline := time.Now().Add(60 * time.Second)
		for {
			if h, err := e.svc.TakeHosted(ctx, e.p.cap.Token, e.p.f.storeA1, order); err == nil && h.Disposition == "REDIRECT" {
				break
			}
			if time.Now().After(deadline) {
				t.Fatal("no REDIRECT")
			}
			time.Sleep(time.Second)
		}
		if v := view(); v.PaymentState != "PENDING" || v.HandoffState != "READY" || v.CancelRequested == nil || *v.CancelRequested {
			t.Fatalf("READY view %+v", v)
		}
		// worker stop/start round trip
		for _, name := range []string{"worker-stop", "worker-start", "method-disable"} {
			if err := e.act(t, sbAction{name: name, order: order}); err != nil {
				t.Fatalf("%s: %v", name, err)
			}
		}
		if v := view(); len(v.Methods) != 0 {
			t.Fatalf("disabled-method view still lists a method: %+v", v)
		} else {
			t.Logf("FINDING: method disabled -> view handoff_state=%s (methods empty, cancel_requested=%v)", v.HandoffState, *v.CancelRequested)
		}
		if h, err := e.svc.TakeHosted(ctx, e.p.cap.Token, e.p.f.storeA1, order); err != nil {
			t.Logf("FINDING: handoff POST with the method disabled -> error %v", err)
		} else {
			t.Logf("FINDING: handoff POST with the method disabled -> disposition=%s", h.Disposition)
		}
		v := view()
		t.Logf("FINDING: after that handoff POST the view says handoff_state=%s payment_state=%s", v.HandoffState, v.PaymentState)
		if err := e.act(t, sbAction{name: "method-enable", order: order}); err != nil {
			t.Fatal(err)
		}
		if err := e.act(t, sbAction{name: "age-cutoff", order: order}); err != nil {
			t.Fatal(err)
		}
		if v := view(); v.HandoffState != "UNAVAILABLE" || v.HandoffExpiresAt == nil || !v.HandoffExpiresAt.Before(time.Now()) || v.PaymentState != "PENDING" {
			t.Fatalf("cutoff view %+v", v)
		}
		if err := e.act(t, sbAction{name: "review", order: order}); err != nil {
			t.Fatal(err)
		}
		end := time.Now().Add(60 * time.Second)
		for time.Now().Before(end) {
			_, _ = e.svc.RefreshPayment(ctx, e.p.cap.Token, e.p.f.storeA1, order)
			if v := view(); v.PaymentState == "REVIEW_REQUIRED" || v.PaymentState == "CAPTURED" {
				t.Logf("OBSERVED after review action: payment_state=%s commercial=%s", v.PaymentState, v.CommercialState)
				return
			}
			time.Sleep(5 * time.Second)
		}
		t.Fatal("review action never reached a terminal payment state")
	})
	t.Run("OBS_mock_unavailable", func(t *testing.T) {
		if os.Getenv("STRIPE_BROWSER_OBSERVE") != "1" {
			t.Skip("NOT_RUN: developer-only (STRIPE_BROWSER_OBSERVE=1)")
		}
		for _, drift := range []string{"qualification-revoke", "binding-disable"} {
			e := sbNew(t, "MOCK")
			ctx, order := context.Background(), e.p.hold.OrderID
			in := checkout.HostedInput{OrderID: order, MethodCode: "stripe_checkout", MethodVersion: e.method, Locale: "en"}
			if _, err := e.svc.BeginHosted(ctx, e.p.cap.Token, e.p.f.storeA1, t04Key("mock-unavail"), in); err != nil {
				t.Fatal(err)
			}
			deadline := time.Now().Add(60 * time.Second)
			for {
				if h, err := e.svc.TakeHosted(ctx, e.p.cap.Token, e.p.f.storeA1, order); err == nil && h.Disposition == "REDIRECT" {
					break
				}
				if time.Now().After(deadline) {
					t.Fatal("no REDIRECT")
				}
				time.Sleep(time.Second)
			}
			if err := e.act(t, sbAction{name: drift, order: order}); err != nil {
				t.Fatal(err)
			}
			v, err := e.svc.PaymentView(ctx, e.p.cap.Token, e.p.f.storeA1, order)
			h, herr := e.svc.TakeHosted(ctx, e.p.cap.Token, e.p.f.storeA1, order)
			t.Logf("FINDING %s: view handoff_state=%s methods=%d err=%v; handoff POST disposition=%s err=%v", drift, v.HandoffState, len(v.Methods), err, h.Disposition, herr)
			v2, _ := e.svc.PaymentView(ctx, e.p.cap.Token, e.p.f.storeA1, order)
			t.Logf("FINDING %s: view after that POST handoff_state=%s expires_in=%s", drift, v2.HandoffState, time.Until(*v2.HandoffExpiresAt).Round(time.Second))
		}
	})
	// Developer-only: observe the REAL hosted page without the B2 buyer UI (never part of the gate).
	for _, c := range []struct{ name, card string }{{"OBS_4242", "4242"}, {"OBS_decline", "decline"}, {"OBS_3ds", "3ds"}} {
		c := c
		t.Run(c.name, func(t *testing.T) {
			if os.Getenv("STRIPE_BROWSER_OBSERVE") != "1" {
				t.Skip("NOT_RUN: developer-only observation (STRIPE_BROWSER_OBSERVE=1 with -run '/^OBS')")
			}
			e := sbNew(t, "SANDBOX")
			ctx := context.Background()
			order := e.p.hold.OrderID
			in := checkout.HostedInput{OrderID: order, MethodCode: "stripe_checkout", MethodVersion: e.method, Locale: "en"}
			if _, err := e.svc.BeginHosted(ctx, e.p.cap.Token, e.p.f.storeA1, t04Key("obs"), in); err != nil {
				t.Fatalf("begin: %v", err)
			}
			deadline := time.Now().Add(60 * time.Second)
			for e.observe == "" {
				h, err := e.svc.TakeHosted(ctx, e.p.cap.Token, e.p.f.storeA1, order)
				if err == nil && h.Disposition == "REDIRECT" && h.RedirectURL != "" {
					e.observe = h.RedirectURL // held in memory for the driver only
				} else if time.Now().After(deadline) {
					t.Fatalf("no REDIRECT within 60s (err=%v)", err)
				} else {
					time.Sleep(time.Second)
				}
			}
			evidence := e.runNode(t, sbCase{Kind: "OBS", Mode: "SANDBOX", Card: c.card, Locale: "en"}, 330*time.Second)
			// Drive the frozen refresh/cancel API as the buyer would through the UI, then read facts.
			end := time.Now().Add(120 * time.Second)
			if c.card == "decline" {
				if _, err := e.svc.CancelPayment(ctx, e.p.cap.Token, e.p.f.storeA1, order); err != nil {
					t.Fatalf("cancel: %v", err)
				}
			}
			for time.Now().Before(end) {
				if c.card != "decline" {
					_, _ = e.svc.RefreshPayment(ctx, e.p.cap.Token, e.p.f.storeA1, order)
				}
				f, err := e.facts(ctx, order)
				if err == nil && f.Order != nil && (f.Order.Captured == 1 || f.Order.ClosedUnpaid == 1) {
					t.Logf("OBSERVED %s: captured=%d closed_unpaid=%d commercial=%s reservation=%s evidence=%s", c.name, f.Order.Captured, f.Order.ClosedUnpaid, f.Order.Commercial, f.Order.Reservation, evidence)
					return
				}
				time.Sleep(12 * time.Second)
			}
			t.Fatalf("OBSERVE %s: no terminal fact within 120s; evidence=%s", c.name, evidence)
		})
	}
}

// TestBrowserPayuniBaseline (SU05 capture) runs the existing BPU02 chain (hpSetup + local mock PSP)
// and captures the PAYUNi order-payment element for a SHA-to-SHA comparison
// (node tests/storefront/payuni-ui-baseline.mjs --compare base.json candidate.json). No Stripe.
func TestBrowserPayuniBaseline(t *testing.T) {
	if os.Getenv("LC_STRIPE_BROWSER_ACCEPTANCE") != "1" || os.Getenv("LC_TEST_DATABASE_ALLOWED") != "1" {
		t.Fatal("use scripts/dev/test-local.sh --stripe-browser")
	}
	h := hpSetup(t)
	bhPublish(t, h.bcHarness, sbOrigin, h.f.tenantA, h.f.storeA1)
	mustExec(t, h.f.owner, `UPDATE catalog.products SET name='Synthetic browser payment product',description='Synthetic acceptance fixture' WHERE id=$1`, h.stock.product.ID)
	bffKey := randomToken()
	handler, err := buyerhttp.New(context.Background(), h.a.issuer, h.a.runtime, h.service, bffKey, time.Hour, h.api.(*checkout.HostedPaymentStarter))
	if err != nil {
		t.Fatal("private payment HTTP fixture")
	}
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	dir := os.Getenv("LC_BASELINE_OUT_DIR")
	if dir == "" {
		dir = filepath.Join(root, "output", "stripe-b2-browser-tests")
	}
	if err = os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	sha, mutant := os.Getenv("LC_BASELINE_SHA"), os.Getenv("LC_BASELINE_MUTATE") == "1"
	name := "payuni-baseline-" + sha
	if mutant {
		name += "-MUTANT"
	}
	out := filepath.Join(dir, name+".json")
	_ = os.Remove(out)
	ctx, cancel := context.WithTimeout(context.Background(), 240*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "node", "tests/storefront/payuni-ui-baseline.mjs")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	defer func() {
		if cmd.Process != nil {
			_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		}
	}()
	cmd.Dir = root
	cmd.Env = stripeNodeEnvironment(os.Environ(), sbSecretNames(sbSecretsPath()), map[string]string{
		"COMMERCE_BUYER_WEB_ENABLED": "1", "COMMERCE_BUYER_API_ORIGIN": server.URL, "COMMERCE_BUYER_DEMO_LABEL": "1",
		"COMMERCE_BUYER_BFF_KEY": bffKey, "COMMERCE_BUYER_COOKIE_KEY": randomToken(), "COMMERCE_BUYER_SESSION_TTL": "3600",
		"LC_BASELINE_OUT": out, "LC_BASELINE_PRODUCT": h.stock.product.ID, "LC_BASELINE_SHA": sha, "LC_BASELINE_MUTATE": os.Getenv("LC_BASELINE_MUTATE"),
	})
	_ = os.Remove(filepath.Join(dir, name+".log"))
	logf := browserLog(t, filepath.Join(dir, name+".log"))
	cmd.Stdout, cmd.Stderr = logf, logf
	if err = cmd.Run(); err != nil {
		t.Fatalf("PAYUNi baseline capture failed; log=%s.log\n%s", filepath.Join(dir, name), sbTail(filepath.Join(dir, name+".log"), 25))
	}
	t.Logf("PASS: PAYUNi order-payment baseline captured (%s) mutant=%v file=%s", sha, mutant, out)
}
