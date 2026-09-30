//go:build browser

package foundation_test

// T12 (docs/delivery/units/t12-e2e.md): the core deal loop in ONE real-browser chain, driven by
// tests/e2e/deal-loop.spec.ts:
//
//	store creation through the settings wizard (create_initial_store, 0065 grants) -> Studio draft scene ->
//	claims window + offer + comment source (claim-source UI) -> SIGNED Meta comment webhook "A1+2" over HTTP into the
//	real meta ingress -> River consumer -> claims intake poller -> claim bundle -> first private reply (fake Graph
//	records exactly one send carrying the claim link) -> buyer opens the link (zh-TW) -> cart -> checkout -> Stripe
//	hosted payment (MOCK: stripetest fake + a Chromium-routed synthetic hosted page) -> CAPTURED + order CONFIRMED +
//	stock allocated -> merchant orders UI ships it (7-ELEVEN + tracking) -> buyer sees the shipment -> merchant issues a
//	partial refund -> the real worker settles it at the fake -> merchant and buyer see it, stock unchanged.
//
// Processes and pools (one isolated PG 18 container, rfxNew): the real payment worker (payments.NewStripeRuntime), the
// merchant admin API (identity + httpapi handlers incl. claims, Studio and refunds) behind the production admin Next
// build with a signed mock OIDC issuer, the buyerhttp handler behind the production storefront Next build behind a
// disposable buyer.example TLS edge + CONNECT proxy, the real meta ingress handler over HTTP, the claims-aware River
// consumer, claimsintake.Poller.Run and the metareply dispatcher against a fake Graph.
//
// Go stays the oracle: the browser only ever calls the runner-only control listener (random key) for actions the
// outside world performs (a Meta comment arrives, Stripe pays, the provider settles a refund) and for checkpoints where
// this file asserts PostgreSQL facts. The control handler never runs assertions itself: it hands each request to the test
// goroutine (like sbEnv.actions), so a failed assertion is an ordinary t.Fatalf.
//
// Evidence labels (AGENTS.md): Meta = MOCK (signed synthetic webhook, fake Graph), Stripe = MOCK (SANDBOX variant is a
// separate NOT_RUN unless STRIPE_SANDBOX=1 and STRIPE_BROWSER=1, see TestBrowserE2EDealLoopSandbox), IdP = signed MOCK.
// Disclosed non-product fixtures: catalog/inventory/pricing/delivery are created through the store creator's own
// domain calls (no merchant UI exists for them in R1), the Meta binding + route rows (owner registrar path, as in the
// MCI gates), the storefront publication/domain rows, the Stripe account registration (cmd/stripe-admin equivalent),
// and the clock nudges of the worker's sleeping refund jobs (wakeAllRefunds, as RF11).
//
// Red runs (PROCESS §2.4): LC_E2E_MUTATE=replay (a NEW comment instead of a replay), negatives (the keyword itself instead of the negation), isolation (the creator's token stands
// in for the foreign tenant) and leak (a claim token lands in a log) each make the corresponding gate fail; the browser
// gates were first red on the real product defect MDEF-1 (defects.json), a Go-side gate never turns a known defect green.
// The PG budget (max_connections=60) is part of the harness: sampleConnections logs the peak.

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"livecommerce/internal/buyer"
	"livecommerce/internal/buyerhttp"
	"livecommerce/internal/catalog"
	"livecommerce/internal/checkout"
	"livecommerce/internal/claims"
	"livecommerce/internal/claimsintake"
	"livecommerce/internal/fulfillment"
	"livecommerce/internal/httpapi"
	"livecommerce/internal/identity"
	"livecommerce/internal/identityhttp"
	"livecommerce/internal/integrations/accounts"
	"livecommerce/internal/integrations/meta"
	"livecommerce/internal/integrations/metareply"
	"livecommerce/internal/inventory"
	"livecommerce/internal/oidclogin"
	"livecommerce/internal/payments"
	"livecommerce/internal/payments/stripeadmin"
	"livecommerce/internal/platform"
	"livecommerce/internal/pricing"
)

// e2eProduct is the one synthetic product; the buyer's total is 2 x 1250 = 2500 minor (NT$25.00 with a zero delivery fee
// and no tax: the smallest TWD amount Stripe accepted in the SANDBOX probe, stripe-psp-v1 §0.1).
const (
	e2eProductName = "Synthetic e2e product"
	e2eCarrier     = "seven_eleven_cvs"
	e2eTracking    = "0012345678"
	e2eRefundMinor = 1000
)

type e2eAction struct {
	name  string
	body  []byte
	reply chan e2eReply
}
type e2eReply struct {
	status int
	body   any
}

type e2eRun struct {
	t        *testing.T
	ctx      context.Context
	e        *rfxEnv
	root     string
	evidence string
	actions  chan e2eAction

	idp           *browserIDP
	adminOrigin   string
	adminAPI      *httptest.Server
	buyerAPI      *httptest.Server
	ingress       *httptest.Server
	bffKey        string
	buyerKeyValue string
	graph         *mciGraph

	// after provision
	f                            testFixture
	principal, tenant, store, wh string
	token                        string
	stock                        t04Stock
	market                       pricing.Market
	scope                        stripeadmin.Scope
	endpoint, webhookSecret      string
	mci                          *mciEnv
	pageAsset, post              string
	firstWebhook                 []byte
	firstComment                 string
	linkToken                    string // secret: only ever compared, never logged
	order, attempt, session      string
	secrets                      map[string]string // label -> value; scanned for in every log and never printed
	peakConnections              atomic.Int64
	a                            buyerTestAuthorities
	opsAtPaid                    int
}

func e2eRequire(t *testing.T) {
	t.Helper()
	if os.Getenv("LC_BROWSER_E2E_ACCEPTANCE") != "1" || os.Getenv("LC_TEST_DATABASE_ALLOWED") != "1" {
		t.Fatal("use scripts/dev/test-local.sh --browser-e2e")
	}
}

func TestBrowserE2EDealLoop(t *testing.T) {
	e2eRequire(t)
	ctx, cancel := context.WithTimeout(context.Background(), 26*time.Minute)
	defer cancel()
	x := &e2eRun{t: t, ctx: ctx, actions: make(chan e2eAction), secrets: map[string]string{}}
	x.root, _ = filepath.Abs("../..")
	x.evidence = brfEvidence(t, x.root, "e2e")
	x.e = rfxNew(t)
	x.startPaymentWorker(t) // the real payment worker: Stripe sessions, capture, refunds
	x.sampleConnections(t)

	x.startAPIs(t)
	control, controlKey := x.startControl(t)
	adminNext := x.startAdminNext(t)
	storefrontPort := x.startStorefrontNext(t)
	_ = adminNext
	target, err := url.Parse("http://127.0.0.1:" + storefrontPort)
	if err != nil {
		t.Fatal(err)
	}
	edge := httptest.NewTLSServer(httputil.NewSingleHostReverseProxy(target))
	t.Cleanup(edge.Close)
	proxy := connectProxy(t, "buyer.example:443", edge.Listener.Addr().String())

	spec := exec.CommandContext(ctx, "pnpm", "exec", "playwright", "test", "tests/e2e/deal-loop.spec.ts", "--config", x.playwrightConfig(t),
		"--reporter=list")
	spec.Dir = x.root
	spec.Env = browserEnvironment(map[string]string{"LC_E2E_ADMIN_ORIGIN": x.adminOrigin, "LC_E2E_BUYER_ORIGIN": sbOrigin, "LC_E2E_PROXY": proxy,
		"LC_E2E_EVIDENCE": x.evidence, "LC_E2E_CONTROL": control, "LC_E2E_CONTROL_KEY": controlKey})
	log := browserLog(t, filepath.Join(x.evidence, "playwright.log"))
	spec.Stdout, spec.Stderr = log, log
	if err := spec.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- spec.Wait() }()
	for running := true; running; {
		select {
		case a := <-x.actions:
			status, body := x.dispatch(t, a)
			a.reply <- e2eReply{status, body}
		case err := <-done:
			running = false
			if err != nil {
				x.finish(t) // secrets/PII scan of what the failed run left behind
				t.Fatalf("T12 browser chain failed: %v; evidence=%s (defects.json lists product defects the chain recorded)", err, x.evidence)
			}
		case <-ctx.Done():
			t.Fatalf("T12 deadline; evidence=%s", x.evidence)
		}
	}
	x.finish(t)
	t.Logf("T12 BROWSER (MOCK Meta, MOCK Stripe): the deal loop passed end to end; evidence=%s", x.evidence)
}

// ---------------------------------------------------------------------------------------------------------------
// processes
// ---------------------------------------------------------------------------------------------------------------

// The fixture PG has max_connections=60 (pwIsolatedFixture, test-focused.sh): one process here holds every pool that
// production spreads over separate processes. Every platform.Open* pool caps itself at 8 (openPool), so the budget is
// kept by (a) one worker pool shared by the payment worker and the Meta dispatcher, (b) low River concurrency, (c) closing
// the setup-only pools right after provisioning, (d) never opening the unused buyer-identity authority. sampleConnections
// records the peak so the evidence shows the margin.
func (x *e2eRun) sampleConnections(t *testing.T) {
	go func() {
		for x.ctx.Err() == nil {
			if n := x.connections(); n > int(x.peakConnections.Load()) {
				x.peakConnections.Store(int64(n))
			}
			select {
			case <-x.ctx.Done():
			case <-time.After(150 * time.Millisecond):
			}
		}
	}()
}

// startPaymentWorker is sflEnv.startWith(stripeEnabled=true) with Concurrency 2 (the connection budget above).
func (x *e2eRun) startPaymentWorker(t *testing.T) {
	e := x.e
	rt, err := payments.NewStripeRuntime(x.ctx, e.pool, e.keys, "PROVIDER_MOCK", e.fake.Transport())
	if err != nil {
		t.Fatalf("stripe runtime: %v", err)
	}
	client, err := payments.NewPaymentWorkerClient(x.ctx, e.pool, payments.WorkerConfig{Profile: "PROVIDER_MOCK", Concurrency: 2, Query: e.opts, Keys: e.keys, Stripe: rt})
	if err != nil {
		t.Fatalf("worker client: %v", err)
	}
	if err := client.Start(x.ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		graceful, cancel := context.WithTimeout(context.Background(), 8*time.Second)
		defer cancel()
		if client.Stop(graceful) != nil {
			hard, stop := context.WithTimeout(context.Background(), 5*time.Second)
			defer stop()
			_ = client.StopAndCancel(hard)
		}
	})
}

// startAPIs builds the admin API (identity + merchant handlers), the buyer API and the meta ingress. Nothing here needs
// the store: it is created by the browser afterwards and everything resolves it per request.
func (x *e2eRun) startAPIs(t *testing.T) {
	ctx := x.ctx
	e := x.e
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	x.adminOrigin = "http://" + listener.Addr().String()
	_ = listener.Close()
	x.idp = newBrowserIDP(t, x.adminOrigin+"/api/auth/callback")

	role := "e2e_" + strings.ReplaceAll(randomUUID(), "-", "")
	password := randomToken()
	mustExec(t, e.f.owner, `CREATE ROLE `+pgx.Identifier{role}.Sanitize()+` LOGIN NOSUPERUSER NOBYPASSRLS NOCREATEDB NOCREATEROLE IN ROLE commerce_identity PASSWORD '`+password+`'`)
	t.Cleanup(func() { mustExec(t, e.f.owner, `DROP ROLE `+pgx.Identifier{role}.Sanitize()) })
	u, err := url.Parse(e.f.databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	u.User = url.UserPassword(role, password)
	authority, err := platform.OpenIdentityPool(ctx, u.String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(authority.Close)
	provider, err := oidclogin.New(ctx, oidclogin.Config{Issuer: x.idp.server.URL, ClientID: browserClientID, RedirectURL: x.idp.redirect, AllowLoopbackForTests: true})
	if err != nil {
		t.Fatal(err)
	}
	service, err := identity.New(authority, observedBrowserProvider{Provider: provider, t: t}, identity.Policy{ProviderKey: "browser-e2e-v1", SessionTTL: time.Hour, OnboardingEnabled: true, Currencies: []string{"TWD", "USD"}})
	if err != nil {
		t.Fatal(err)
	}
	adminKey := randomToken()
	x.secrets["admin BFF key"] = adminKey
	private, err := identityhttp.NewHandler(service, adminKey)
	if err != nil {
		t.Fatal(err)
	}
	labels, err := claims.NewLabelKey(randomBytes(32))
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	mux.Handle("/v1/identity/", private)
	// R1 deploy shape (ruling G2): planning-only Studio + claims, no media planner (media routes 404).
	mux.Handle("/", httpapi.NewHandler(e.f.runtime, httpapi.Options{SessionStoreList: true, Studio: true, ClaimLabels: &labels, RefundJobs: e.jobs}))
	x.adminAPI = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Diagnosis aid: every 5xx of the merchant API is logged (method, path, status, the Go error CODE only).
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, r)
		if rec.Code >= 500 {
			var out struct {
				Code string `json:"code"`
			}
			_ = json.Unmarshal(rec.Body.Bytes(), &out)
			t.Logf("T12 admin API %s %s -> %d code=%q connections=%d", r.Method, r.URL.Path, rec.Code, out.Code, x.connections())
		}
		for k, v := range rec.Header() {
			w.Header()[k] = v
		}
		w.WriteHeader(rec.Code)
		_, _ = w.Write(rec.Body.Bytes())
	}))
	t.Cleanup(x.adminAPI.Close)
	x.bffKey = adminKey

	// Buyer API: the same assembly as browser_stripe_test.go's sbNew, with the buyer.example return URL.
	a := openBuyerTestPools(t, e.f)
	a.identity.Close() // unused here (the merchant identity pool is the admin one); frees a connection of the 60
	x.a = a
	checkoutPool, err := platform.OpenCheckoutPool(ctx, bcRole(t, e.f, "commerce_checkout_runtime"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(checkoutPool.Close)
	pcfg := checkout.HostedConfig{ReturnURL: sbReturnURL, NotifyURL: sbOrigin + "/payment/notify"}
	scfg := checkout.StripeHostedConfig{ReturnURL: sbReturnURL}
	hosted, err := checkout.NewHostedPaymentService(ctx, e.hosted, e.sstEnv.jobs, "PROVIDER_MOCK", e.keys, checkout.HostedProviders{PAYUNi: &pcfg, Stripe: &scfg})
	if err != nil {
		t.Fatal(err)
	}
	buyerKey := base64.RawURLEncoding.EncodeToString(randomBytes(32))
	handler, err := buyerhttp.New(ctx, a.issuer, a.runtime, bcServiceIn(t, checkoutPool, "river_expiry"), buyerKey, time.Hour, hosted)
	if err != nil {
		t.Fatalf("buyer HTTP constructor: %v", err)
	}
	x.buyerAPI = httptest.NewServer(handler)
	t.Cleanup(x.buyerAPI.Close)
	x.secrets["buyer BFF key"] = buyerKey
	x.buyerKeyValue = buyerKey
}

func (x *e2eRun) startAdminNext(t *testing.T) *exec.Cmd {
	_, port, _ := net.SplitHostPort(strings.TrimPrefix(x.adminOrigin, "http://"))
	logFile := browserLog(t, filepath.Join(x.evidence, "admin-next.log"))
	cmd := exec.CommandContext(x.ctx, "node", filepath.Join(x.root, "apps/admin/.next/standalone/apps/admin/server.js"))
	cmd.Dir = x.root
	cmd.Env = browserEnvironment(map[string]string{"HOSTNAME": "127.0.0.1", "PORT": port, "NODE_ENV": "production", "NEXT_TELEMETRY_DISABLED": "1",
		"COMMERCE_IDENTITY_ENABLED": "1", "COMMERCE_IDENTITY_ALLOW_LOOPBACK_TESTS": "1", "COMMERCE_PUBLIC_ORIGIN": x.adminOrigin,
		"COMMERCE_API_ORIGIN": x.adminAPI.URL, "COMMERCE_OIDC_ISSUER": x.idp.server.URL, "COMMERCE_BFF_KEY": x.bffKey,
		"COMMERCE_ONBOARDING_ENABLED": "1", "COMMERCE_ONBOARDING_CURRENCIES": "TWD,USD"})
	cmd.Stdout, cmd.Stderr = logFile, logFile
	x.spawn(t, cmd)
	waitReady(x.ctx, t, x.adminOrigin+"/api/stores", "", http.StatusUnauthorized, x.evidence)
	return cmd
}

func (x *e2eRun) startStorefrontNext(t *testing.T) string {
	port := freeLoopbackPort(t)
	logFile := browserLog(t, filepath.Join(x.evidence, "storefront-next.log"))
	cookieKey := brToken()
	x.secrets["buyer cookie key"] = cookieKey
	cmd := exec.CommandContext(x.ctx, "node", filepath.Join(x.root, "apps/storefront/node_modules/next/dist/bin/next"), "start", "--hostname", "127.0.0.1", "--port", port)
	cmd.Dir = filepath.Join(x.root, "apps/storefront")
	cmd.Env = browserEnvironment(map[string]string{"NODE_ENV": "production", "NEXT_TELEMETRY_DISABLED": "1", "COMMERCE_BUYER_WEB_ENABLED": "1",
		"COMMERCE_BUYER_DEMO_LABEL": "1", "COMMERCE_BUYER_API_ORIGIN": x.buyerAPI.URL, "COMMERCE_BUYER_BFF_KEY": x.buyerKeyValue,
		"COMMERCE_BUYER_COOKIE_KEY": cookieKey, "COMMERCE_BUYER_SESSION_TTL": "3600"})
	cmd.Stdout, cmd.Stderr = logFile, logFile
	x.spawn(t, cmd)
	waitReady(x.ctx, t, "http://127.0.0.1:"+port+"/api/buyer/session", "buyer.example", http.StatusOK, x.evidence)
	return port
}

func (x *e2eRun) spawn(t *testing.T, cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); _ = cmd.Wait() })
}

// playwrightConfig writes a config next to the evidence (inside the repo so @playwright/test resolves).
func (x *e2eRun) playwrightConfig(t *testing.T) string {
	config := filepath.Join(x.evidence, "playwright.e2e.config.ts")
	body := fmt.Sprintf(`import { defineConfig } from "@playwright/test";
export default defineConfig({
  testDir: %q, testMatch: ["deal-loop.spec.ts"],
  fullyParallel: false, workers: 1, retries: 0, timeout: 1500000,
  expect: { timeout: 15000 },
  reporter: [["list"]], outputDir: %q,
  use: { headless: true, viewport: { width: 1586, height: 992 }, trace: "off", screenshot: "only-on-failure", actionTimeout: 20000, navigationTimeout: 30000 },
});
`, filepath.Join(x.root, "tests/e2e"), filepath.Join(x.evidence, "results"))
	if err := os.WriteFile(config, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return config
}

// startControl serves the runner-only control listener: POST /act?name=<action> with a JSON body. Requests are handed to
// the test goroutine and answered from there.
func (x *e2eRun) startControl(t *testing.T) (origin, key string) {
	key = randomToken()
	x.secrets["control key"] = key
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Gate-Key") != key || r.Method != http.MethodPost || r.URL.Path != "/act" {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		raw, _ := io.ReadAll(io.LimitReader(r.Body, 1<<16))
		reply := make(chan e2eReply, 1)
		select {
		case x.actions <- e2eAction{name: r.URL.Query().Get("name"), body: raw, reply: reply}:
		case <-x.ctx.Done():
			http.Error(w, "closed", http.StatusServiceUnavailable)
			return
		}
		var out e2eReply
		select {
		case out = <-reply:
		case <-x.ctx.Done():
			http.Error(w, "closed", http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(out.status)
		_ = json.NewEncoder(w).Encode(out.body)
	}))
	t.Cleanup(server.Close)
	return server.URL, key
}

// ---------------------------------------------------------------------------------------------------------------
// actions (run on the test goroutine)
// ---------------------------------------------------------------------------------------------------------------

func (x *e2eRun) dispatch(t *testing.T, a e2eAction) (int, any) {
	t.Helper()
	var in map[string]any
	if len(a.body) > 0 {
		if err := json.Unmarshal(a.body, &in); err != nil {
			return http.StatusBadRequest, map[string]string{"error": "bad json"}
		}
	}
	str := func(k string) string { s, _ := in[k].(string); return s }
	var out any
	switch a.name {
	case "provision":
		out = x.provision(t)
	case "comment":
		out = x.comment(t, str("text"), str("kind"))
	case "await-reply":
		out = x.awaitReply(t)
	case "replay":
		out = x.replay(t)
	case "negatives":
		out = x.negatives(t)
	case "check":
		out = x.check(t, str("name"), in)
	case "pay":
		out = x.pay(t, str("order"))
	case "settle-refund":
		out = x.settleRefund(t)
	case "isolation":
		out = x.isolation(t)
	default:
		return http.StatusNotFound, map[string]string{"error": "unknown action"}
	}
	t.Logf("T12 step %s ok", a.name)
	return http.StatusOK, out
}

// connections is the number of server connections on the isolated PG (max_connections=60 is a hard limit of the fixture).
func (x *e2eRun) connections() int {
	var n int
	_ = x.e.f.owner.QueryRow(context.Background(), `SELECT count(*) FROM pg_stat_activity WHERE datname=current_database()`).Scan(&n)
	return n
}

// connectionMap groups the connections by role membership (diagnosis of the max_connections=60 budget).
func (x *e2eRun) connectionMap() string {
	rows, err := x.e.f.owner.Query(context.Background(), `SELECT coalesce((SELECT string_agg(g.rolname,'+') FROM pg_auth_members m JOIN pg_roles g ON g.oid=m.roleid WHERE m.member=r.oid),a.usename) AS grp,count(*)
		FROM pg_stat_activity a LEFT JOIN pg_roles r ON r.rolname=a.usename WHERE a.datname=current_database() GROUP BY 1 ORDER BY 2 DESC`)
	if err != nil {
		return err.Error()
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var g string
		var n int
		if rows.Scan(&g, &n) == nil {
			out = append(out, fmt.Sprintf("%s=%d", g, n))
		}
	}
	return strings.Join(out, " ")
}

func (x *e2eRun) count(q string, args ...any) int { return countRows(x.t, x.e.f.owner, q, args...) }

// provision runs after the wizard created the store. It reads what create_initial_store produced, then adds the
// fixtures that have no merchant UI in R1 (see the header) for THAT store, with the creator's own session.
func (x *e2eRun) provision(t *testing.T) map[string]any {
	ctx := x.ctx
	e := x.e
	if err := e.f.owner.QueryRow(ctx, `SELECT s.principal_id::text,s.tenant_id::text,s.store_id::text,s.warehouse_id::text FROM identity.initial_stores s
		JOIN identity.external_identities x ON x.principal_id=s.principal_id WHERE x.issuer=$1 AND x.subject='browser-subject'`, x.idp.server.URL).
		Scan(&x.principal, &x.tenant, &x.store, &x.wh); err != nil {
		t.Fatalf("the wizard did not create an initial store for the signed-in merchant: %v", err)
	}
	if n := x.count(`SELECT count(*) FROM identity.initial_stores`); n != 1 {
		t.Fatalf("%d initial stores exist, want exactly the merchant's", n)
	}
	// 0065: the creator holds exactly the documented set; assert the ones this chain depends on (no extra grants are added).
	for _, p := range []string{"live:read", "live:manage", "payments:refund", "fulfillment:write", "orders:export", "integration:execute", "integration:manage", "orders:read", "catalog:write", "inventory:write", "pricing:write"} {
		if x.count(`SELECT count(*) FROM identity.store_grants WHERE tenant_id=$1 AND store_id=$2 AND principal_id=$3 AND permission=$4`, x.tenant, x.store, x.principal, p) != 1 {
			t.Fatalf("0065: the store creator lacks %s", p)
		}
	}
	f := *e.f
	f.tenantA, f.storeA1, f.principalA = x.tenant, x.store, x.principal
	x.token = randomToken()
	x.secrets["fixture session token"] = x.token
	f.tokens = map[string]string{"a": x.token}
	x.f = f
	tx, err := f.owner.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := insertSession(ctx, tx, x.token, x.principal, "merchant", time.Now().Add(2*time.Hour), nil); err != nil {
		_ = tx.Rollback(ctx)
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}

	// catalog + stock in the wizard's own warehouse
	f0 := &x.f
	tag := t04Tag()
	product, err := t04Scoped(ctx, f0, x.token, x.store, "catalog:write", func(tx pgx.Tx, s platform.Scope) (catalog.Product, error) {
		return catalog.CreateProduct(ctx, tx, s, t04Key("e2e-product"), catalog.ProductInput{Name: e2eProductName, Description: "Synthetic acceptance fixture"})
	})
	if err != nil {
		t.Fatalf("create product: %v", err)
	}
	sku, err := t04Scoped(ctx, f0, x.token, x.store, "catalog:write", func(tx pgx.Tx, s platform.Scope) (catalog.SKU, error) {
		return catalog.CreateSKU(ctx, tx, s, t04Key("e2e-sku"), catalog.SKUInput{ProductID: product.ID, Code: "E2E-" + strings.ToUpper(tag[:6]), PriceMinor: 1250,
			WeightGrams: 100, LengthMM: 10, WidthMM: 20, HeightMM: 30, OriginCountry: "TW", CustomsName: "test item", HSCandidate: "851840"})
	})
	if err != nil {
		t.Fatalf("create SKU: %v", err)
	}
	balance, err := t04Scoped(ctx, f0, x.token, x.store, "inventory:write", func(tx pgx.Tx, s platform.Scope) (inventory.Balance, error) {
		return inventory.AdjustOnHand(ctx, tx, s, t04Key("e2e-stock"), inventory.Adjustment{WarehouseID: x.wh, SKUID: sku.ID, Delta: 10, ExpectedVersion: 0, Reason: "T12 e2e opening stock"})
	})
	if err != nil {
		t.Fatalf("opening stock: %v", err)
	}
	x.stock = t04Stock{product: product, warehouse: inventory.Warehouse{ID: x.wh}, skus: []catalog.SKU{sku}, balances: []inventory.Balance{balance}}

	// market, policy (zero delivery fee, no tax), home delivery service and its allocation (see psSetupItemsOn)
	h := cqHarness{f: f0, a: x.a, stock: x.stock}
	if h.service, err = buyer.New(h.a.issuer, time.Hour); err != nil {
		t.Fatal(err)
	}
	if x.market, err = pricingScoped(ctx, f0, x.token, x.store, "pricing:write", func(tx pgx.Tx, s platform.Scope) (pricing.Market, error) {
		return pricing.CreateMarket(ctx, tx, s, t04Key("e2e-market"), pricing.MarketInput{Code: "tw", Name: "Synthetic TW", Currency: "TWD"})
	}); err != nil {
		t.Fatal(err)
	}
	h.market = x.market
	zero := int64(0)
	h.policy = pricing.PolicyInput{MarketID: x.market.ID, Country: "TW", Currency: "TWD", ShippingMode: "country_flat", ShippingMinor: &zero, TaxMode: "none", TaxBasis: "goods", TaxRateBPS: &zero, QuoteTTLSeconds: 300, Enabled: true, ConfigurationRef: "synthetic only"}
	delivery := fulfillment.ServiceInput{MarketID: x.market.ID, Country: "TW", Code: "home", PolicyVersion: 1, NameHans: "测试配送", NameHant: "測試配送", NameEN: "Mock delivery", DeliveryKind: "home", Mode: "MANUAL", Enabled: true, Visible: true}
	dsPolicy(t, h, delivery, 0, 0, true)
	if _, err = dsSet(h, t04Key("e2e-delivery"), delivery); err != nil {
		t.Fatal(err)
	}
	if _, err = daSet(h, t04Key("e2e-allocation"), fulfillment.AllocationInput{MarketID: x.market.ID, Country: "TW", Code: "home", ExpectedServiceVersion: 1, WarehouseIDs: []string{x.wh}}); err != nil {
		t.Fatal(err)
	}

	x.provisionStripe(t)
	x.provisionMeta(t, h)
	// The buyer origin: published + an ACTIVE verified domain row (bhPublish's rows; owner fixture, disclosed).
	mustExec(t, e.f.owner, `INSERT INTO control.storefront_publications(tenant_id,store_id,published) VALUES($1,$2,true)`, x.tenant, x.store)
	mustExec(t, e.f.owner, `INSERT INTO control.storefront_domains(tenant_id,store_id,origin,state,ownership_verified_at,tls_verified_at,valid_until,evidence_ref)
		VALUES($1,$2,$3,'ACTIVE',clock_timestamp()-interval '1 hour',clock_timestamp()-interval '1 hour',clock_timestamp()+interval '4 hours','SYNTHETIC e2e gate only')`, x.tenant, x.store, sbOrigin)
	t.Logf("T12 provisioned; PG connections=%d of 60: %s", x.connections(), x.connectionMap())
	return map[string]any{"store": x.store, "product_id": product.ID, "product_name": e2eProductName, "sku_id": sku.ID, "sku_code": sku.Code,
		"page_asset": x.pageAsset, "post_url": "https://www.facebook.com/" + x.pageAsset + "/posts/" + x.post, "carrier": e2eCarrier, "tracking": e2eTracking}
}

// provisionStripe is cmd/stripe-admin's path for the fake: register the account, qualify TWD 2500, enable the method,
// register the webhook endpoint (sstEnv.seed + sflEnv.endpoint, with the buyer.example return URL of sbNew).
func (x *e2eRun) provisionStripe(t *testing.T) {
	ctx := x.ctx
	e := x.e
	x.scope = stripeadmin.Scope{TenantID: x.tenant, StoreID: x.store, PrincipalID: x.principal}
	account, secret := "acct_T"+t04Tag(), "sk_"+"test_"+hex.EncodeToString(randomBytes(12))
	x.secrets["stripe test key"] = secret
	if err := e.fake.AddAccount(account, secret); err != nil {
		t.Fatal(err)
	}
	conn, err := e.reg.Register(ctx, x.scope, account, secret)
	if err != nil {
		t.Fatalf("register Stripe account: %v", err)
	}
	qual, err := e.reg.Qualify(ctx, x.scope, stripeadmin.QualifyInput{ConnectionID: conn, AccountID: account, SecretKey: secret, Profile: "PROVIDER_MOCK",
		Currency: "TWD", ReturnURL: sbReturnURL, ExpectedVersion: 1, AmountMinor: 2500})
	if err != nil {
		t.Fatalf("qualify Stripe method: %v", err)
	}
	if _, err = e.reg.SetMethod(ctx, x.scope, stripeadmin.MethodInput{MarketID: x.market.ID, Country: "TW", ConnectionID: conn, QualificationID: qual, ExpectedVersion: 0,
		Enabled: true, Visible: true, Sort: 1, MinMinor: 2500, MaxMinor: 99999900, NameHans: "Stripe 测试卡付款", NameHant: "Stripe 測試卡付款", NameEN: "Stripe test card payment"}); err != nil {
		t.Fatalf("set Stripe method: %v", err)
	}
	defer e.reg.Close() // setup-only (idempotent: the fixture's own cleanup closes it again)
	x.webhookSecret = swhSecret()
	x.secrets["stripe webhook secret"] = x.webhookSecret
	if x.endpoint, _, err = e.reg.SetWebhookEndpoint(ctx, x.scope, stripeadmin.EndpointInput{ConnectionID: conn, AccountID: account, Profile: "PROVIDER_MOCK", Enabled: true,
		Secrets: accounts.StripeWebhookSecrets{CurrentSecret: x.webhookSecret}}); err != nil {
		t.Fatalf("register webhook endpoint: %v", err)
	}
}

// provisionMeta builds the MCI harness pieces for this store: Facebook Page binding + route (synthetic asset, exactly like
// the MCI gates), a Page token so private replies may be enabled, the claims-aware River consumer, the intake poller
// running on its own timer (as cmd/claims-worker does) and the metareply dispatcher against a fake Graph.
func (x *e2eRun) provisionMeta(t *testing.T, h cqHarness) {
	ctx := x.ctx
	f := &x.f
	m := e2eMetaSetup(t, x.e.f)
	labels, err := claims.NewLabelKey(randomBytes(32))
	if err != nil {
		t.Fatal(err)
	}
	e := &mciEnv{t: t, h: &lcHarness{cqHarness: h, ctx: ctx, actor: x.principal, token: x.token, labels: labels}, page: m, consumerWorkers: 1, stopConsumer: mciNoop}
	e.actorRaw, e.linkRaw, e.pageKeyRaw = randomBytes(32), randomBytes(32), randomBytes(32)
	if e.actor, err = meta.NewClaimsActorKey(e.actorRaw); err != nil {
		t.Fatal(err)
	}
	if e.link, err = claims.NewReplyLinkKey(e.linkRaw); err != nil {
		t.Fatal(err)
	}
	if e.pageKeys, err = metareply.NewPageTokenKeyring("pt_key_1", map[string][]byte{"pt_key_1": e.pageKeyRaw}); err != nil {
		t.Fatal(err)
	}
	e.pageAsset = miAsset()
	e.pageBinding = miBinding(t, m, e.pageAsset, "facebook", x.tenant, x.store, x.principal)
	miRoute(t, m, e.pageAsset, x.tenant, x.store, e.pageBinding)
	e.pageToken = "SENTINEL-EAAG-PAGE-" + t04Tag() + t04Tag()
	x.secrets["page token"] = e.pageToken
	e.registerToken(t, "facebook", e.pageBinding, e.pageAsset, []string{"pages_messaging"}, e.pageToken)
	x.pageAsset, x.post = e.pageAsset, mciDigits(15)
	x.mci = e
	e.intakeLogin = miRole(t, f, "commerce_claims_intake")
	if e.intakePool, err = platform.OpenClaimsIntakePool(ctx, e.intakeLogin); err != nil {
		t.Fatalf("claims intake login: %v", err)
	}
	t.Cleanup(e.intakePool.Close)
	if e.poller, err = claimsintake.New(ctx, e.intakePool, e.link, claimsintake.Config{Workers: 1, IdleSleep: 200 * time.Millisecond}); err != nil {
		t.Fatal(err)
	}
	pollCtx, stopPoll := context.WithCancel(ctx)
	pollDone := make(chan struct{})
	go func() { _ = e.poller.Run(pollCtx); close(pollDone) }()
	t.Cleanup(func() { stopPoll(); <-pollDone })
	e.startConsumer(t)
	t.Cleanup(func() { e.stopConsumer() })
	x.graph = newMciGraph(t)
	// The dispatcher owns the default queue of the main river schema (where the intake commit inserts the reply job).
	pool := x.e.pool // the payment worker's commerce_worker pool (one shared worker authority; see sampleConnections)
	routes, err := metareply.Routes(pool, e.link, e.pageKeys, metareply.Config{GraphBaseURL: x.graph.srv.URL, GraphVersion: "v99.0", HTTPClient: x.graph.srv.Client()})
	if err != nil {
		t.Fatalf("metareply.Routes: %v", err)
	}
	t06StartDispatcher(t, pool, "default", routes, mciDispatchOptions())
	x.ingress = httptest.NewServer(m.handler)
	t.Cleanup(x.ingress.Close)
	m.registrar.Close() // setup-only: route activation and Page-token registration are done
	m.curator.Close()
}

// e2eMetaSetup is miSetup on the isolated fixture instead of the shared one.
func e2eMetaSetup(t *testing.T, f *testFixture) miTest {
	t.Helper()
	ctx := context.Background()
	ingress := miPool(t, f, "commerce_meta_ingress")
	registrar := miPool(t, f, "commerce_meta_registrar")
	curator := miPool(t, f, "commerce_meta_curator")
	key := randomBytes(32)
	keys, err := meta.NewPayloadKeyring(miKeyID, map[string][]byte{miKeyID: key})
	if err != nil {
		t.Fatal(err)
	}
	inbox, err := meta.NewInbox(ctx, ingress, keys)
	if err != nil {
		t.Fatal("dedicated ingress rejected", err)
	}
	v, err := meta.NewVerifier(meta.Config{AppID: miApp, Object: "page", AppSecret: miSecret, VerifyToken: "meta-inbox-verify-token"})
	if err != nil {
		t.Fatal(err)
	}
	h, err := meta.NewInboxHandler(v, inbox)
	if err != nil {
		t.Fatal(err)
	}
	return miTest{f, ingress, registrar, curator, v, h, key}
}

// postWebhook signs raw and POSTs it over HTTP to the real ingress handler.
func (x *e2eRun) postWebhook(t *testing.T, raw []byte) (int, string) {
	t.Helper()
	req, err := http.NewRequestWithContext(x.ctx, http.MethodPost, x.ingress.URL+"/meta-webhook", bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Hub-Signature-256", miSignature(raw))
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("webhook POST: %v", err)
	}
	defer res.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(res.Body, 4096))
	return res.StatusCode, string(body)
}

// comment posts one signed comment on the bound post and waits for the consumer's `processed` fact.
func (x *e2eRun) comment(t *testing.T, text, kind string) map[string]any {
	t.Helper()
	comment := mciDigits(15) + "_" + mciDigits(10)
	from := "100" + mciDigits(12)
	at := time.Now().Add(3 * time.Second)
	raw := mciFBBody(x.pageAsset, x.pageAsset+"_"+x.post, comment, from, "Amy Synthetic", text, &at, nil)
	batch, err := x.mci.page.verifier.Verify(raw, miSignature(raw))
	if err != nil || len(batch.Events) != 1 || batch.Events[0].QuarantineReason != "" {
		t.Fatalf("the synthetic comment is not one clean event: %v", err)
	}
	status, body := x.postWebhook(t, raw)
	if status != 200 || body != "EVENT_RECEIVED" {
		t.Fatalf("real ingress answered %d %q", status, body)
	}
	ev := mcEvent{key: batch.Events[0].Key, app: miApp, object: batch.Object, asset: x.pageAsset}
	if err := x.e.f.owner.QueryRow(x.ctx, `SELECT id::text,route_id::text,route_epoch,job_id FROM meta_inbox.events WHERE app_id=$1 AND object=$2 AND event_key=$3 AND is_primary`,
		miApp, ev.object, ev.key).Scan(&ev.id, &ev.route, &ev.epoch, &ev.job); err != nil {
		t.Fatalf("the ingress admitted no primary event: %v", err)
	}
	mcAwait(t, x.mci.page, ev)
	if kind == "first" {
		x.firstWebhook, x.firstComment = raw, comment
	}
	return map[string]any{"comment_id": comment, "ingress_status": status}
}

var e2eLink = regexp.MustCompile(`https://buyer\.example/zh-TW/claim#t=([A-Za-z0-9_-]{43})`)

// awaitReply waits for the (single) private reply the intake planned to reach the fake Graph.
func (x *e2eRun) awaitReply(t *testing.T) map[string]any {
	t.Helper()
	deadline := time.Now().Add(60 * time.Second)
	for x.graph.posts(x.firstComment) == 0 {
		if time.Now().After(deadline) {
			t.Fatalf("no private reply reached the fake Graph within 60s (operations: %d)", x.count(`SELECT count(*) FROM integration.operations WHERE action='meta.private_reply'`))
		}
		time.Sleep(200 * time.Millisecond)
	}
	time.Sleep(1500 * time.Millisecond) // a second, wrong send would land inside this window
	posts := x.graph.all()
	if len(posts) != 1 || posts[0].method != http.MethodPost || posts[0].comment != x.firstComment {
		t.Fatalf("the fake Graph saw %d request(s), want exactly one POST for the comment", len(posts))
	}
	match := e2eLink.FindStringSubmatch(posts[0].text)
	if match == nil {
		t.Fatalf("the private reply text carries no zh-TW claim link on the published origin")
	}
	if !strings.HasPrefix(posts[0].text, "感謝留言") { // metareply/render.go zh-TW sentence: the source's reply_locale is honoured
		t.Fatalf("the private reply is not the zh-TW template")
	}
	x.linkToken = match[1]
	x.secrets["claim link token"] = x.linkToken
	if !strings.HasPrefix(posts[0].path, "/v99.0/") || posts[0].bodyToken != "" && posts[0].bodyToken != x.mci.pageToken {
		t.Fatalf("Graph call shape: path=%s", posts[0].path)
	}
	return map[string]any{"sends": len(posts), "link": match[0], "text_locale": "zh-TW"}
}

func (x *e2eRun) intakeQuiet(t *testing.T, why string) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for x.count(`SELECT count(*) FROM claims.meta_intake WHERE state='PENDING'`) != 0 {
		if time.Now().After(deadline) {
			t.Fatalf("%s: intake rows still PENDING after 20s", why)
		}
		time.Sleep(200 * time.Millisecond)
	}
	time.Sleep(1200 * time.Millisecond)
}

type e2eClaimFacts struct{ events, accepted, bundles, ops, sends int }

func (x *e2eRun) claimFacts() e2eClaimFacts {
	var c e2eClaimFacts
	c.events = x.count(`SELECT count(*) FROM claims.events WHERE tenant_id=$1 AND store_id=$2`, x.tenant, x.store)
	c.accepted = x.count(`SELECT count(*) FROM claims.events WHERE tenant_id=$1 AND store_id=$2 AND outcome='ACCEPTED'`, x.tenant, x.store)
	c.bundles = x.count(`SELECT count(*) FROM claims.bundles WHERE tenant_id=$1 AND store_id=$2`, x.tenant, x.store)
	c.ops = x.count(`SELECT count(*) FROM integration.operations WHERE action='meta.private_reply'`)
	c.sends = len(x.graph.all())
	return c
}

// replay re-delivers the identical signed body: Meta retries deliveries, so nothing new may be created.
func (x *e2eRun) replay(t *testing.T) map[string]any {
	t.Helper()
	x.intakeQuiet(t, "before replay")
	before := x.claimFacts()
	inbox := x.count(`SELECT count(*) FROM meta_inbox.events`)
	if os.Getenv("LC_E2E_MUTATE") == "replay" { // red-run mutation (PROCESS §2.4): a NEW comment instead of a replay must trip the gate below
		x.comment(t, "A1+2", "")
		x.intakeQuiet(t, "mutation")
	}
	status, body := x.postWebhook(t, x.firstWebhook)
	if status != 200 || body != "EVENT_RECEIVED" {
		t.Fatalf("replayed delivery answered %d %q", status, body)
	}
	x.intakeQuiet(t, "after replay")
	after := x.claimFacts()
	if before != after || x.count(`SELECT count(*) FROM meta_inbox.events`) != inbox {
		t.Fatalf("a replayed webhook changed state: before=%+v after=%+v inbox %d->%d", before, after, inbox, x.count(`SELECT count(*) FROM meta_inbox.events`))
	}
	if after.bundles != 1 || after.ops != 1 || after.sends != 1 {
		t.Fatalf("after replay: %+v, want one bundle, one reply operation and one Graph send", after)
	}
	return map[string]any{"replay_status": status, "facts": fmt.Sprintf("%+v", after)}
}

// negatives: a refusal and a question containing the keyword must not claim anything or send anything, AND must have
// actually reached the grammar: each comment's intake row is APPLIED with grammar NO_MATCH, and the claims ledger grew
// by exactly those two REJECTED/NO_MATCH events (contract R01/I07). Without that, a dropped or FAILED comment would
// pass this gate untested.
func (x *e2eRun) negatives(t *testing.T) map[string]any {
	t.Helper()
	x.intakeQuiet(t, "before negatives")
	before := x.claimFacts()
	noMatchBefore := x.count(`SELECT count(*) FROM claims.events WHERE tenant_id=$1 AND store_id=$2 AND outcome='REJECTED' AND reason='NO_MATCH'`, x.tenant, x.store)
	lines := x.count(`SELECT coalesce(sum(quantity),0)::int FROM claims.lines WHERE tenant_id=$1 AND store_id=$2`, x.tenant, x.store)
	texts := []string{"不要A1", "A1是不是红色"}
	if os.Getenv("LC_E2E_MUTATE") == "negatives" { // red-run mutation (PROCESS §2.4): a real claim keyword instead of the negation must trip the gates below
		texts = []string{"A1", "A1"}
	}
	var refs []string
	for _, text := range texts {
		refs = append(refs, x.comment(t, text, "")["comment_id"].(string))
	}
	x.intakeQuiet(t, "after negatives")
	for i, ref := range refs {
		var state, kind, outcome, reason string
		if err := x.e.f.owner.QueryRow(x.ctx, `SELECT i.state,i.grammar_kind,coalesce(e.outcome,''),coalesce(e.reason,'')
			FROM claims.meta_intake i LEFT JOIN claims.events e ON e.tenant_id=i.tenant_id AND e.store_id=i.store_id AND e.id=i.applied_event_id
			WHERE i.tenant_id=$1 AND i.store_id=$2 AND i.comment_ref=$3`, x.tenant, x.store, ref).Scan(&state, &kind, &outcome, &reason); err != nil {
			t.Fatalf("negative comment %d (%q) has no intake row: %v", i, texts[i], err)
		}
		if state != "APPLIED" || kind != "NO_MATCH" || outcome != "REJECTED" || reason != "NO_MATCH" {
			t.Fatalf("negative comment %d (%q) did not reach the grammar as NO_MATCH: intake=%s grammar=%s event=%s/%s", i, texts[i], state, kind, outcome, reason)
		}
	}
	after := x.claimFacts()
	if got := x.count(`SELECT count(*) FROM claims.events WHERE tenant_id=$1 AND store_id=$2 AND outcome='REJECTED' AND reason='NO_MATCH'`, x.tenant, x.store); got != noMatchBefore+len(texts) || after.events != before.events+len(texts) {
		t.Fatalf("claims.events must grow by exactly %d REJECTED/NO_MATCH rows: total %d->%d, NO_MATCH %d->%d", len(texts), before.events, after.events, noMatchBefore, got)
	}
	if after.accepted != before.accepted || after.bundles != before.bundles || after.ops != before.ops || after.sends != before.sends {
		t.Fatalf("a refusal/question comment created a claim or a send: before=%+v after=%+v", before, after)
	}
	if got := x.count(`SELECT coalesce(sum(quantity),0)::int FROM claims.lines WHERE tenant_id=$1 AND store_id=$2`, x.tenant, x.store); got != lines {
		t.Fatalf("claimed quantity moved %d -> %d", lines, got)
	}
	return map[string]any{"events_before": before.events, "events_after": after.events, "no_match_events": noMatchBefore + len(texts), "accepted": after.accepted}
}

// ---------------------------------------------------------------------------------------------------------------
// checkpoints, payment, refund, isolation
// ---------------------------------------------------------------------------------------------------------------

type e2eStock struct{ onHand, reserved, allocated int64 }

func (x *e2eRun) stockNow(t *testing.T) e2eStock {
	t.Helper()
	var s e2eStock
	if err := x.e.f.owner.QueryRow(x.ctx, `SELECT on_hand,reserved,allocated FROM inventory.balances WHERE tenant_id=$1 AND store_id=$2 AND sku_id=$3`,
		x.tenant, x.store, x.stock.skus[0].ID).Scan(&s.onHand, &s.reserved, &s.allocated); err != nil {
		t.Fatalf("read stock: %v", err)
	}
	return s
}

func (x *e2eRun) orderState(t *testing.T) (id, commercial, fulfilment string, total int64) {
	t.Helper()
	if err := x.e.f.owner.QueryRow(x.ctx, `SELECT id::text,commercial_state,fulfillment_state,total_minor FROM checkout.orders WHERE tenant_id=$1 AND store_id=$2`, x.tenant, x.store).
		Scan(&id, &commercial, &fulfilment, &total); err != nil {
		t.Fatalf("exactly one order was expected: %v", err)
	}
	return
}

func (x *e2eRun) need(t *testing.T, ok bool, format string, args ...any) {
	t.Helper()
	if !ok {
		t.Fatalf("T12 checkpoint failed: "+format+"; evidence="+x.evidence, args...)
	}
}

// check asserts PostgreSQL facts at one step of the chain and returns them for the evidence log.
func (x *e2eRun) check(t *testing.T, name string, in map[string]any) map[string]any {
	t.Helper()
	ctx := x.ctx
	owner := x.e.f.owner
	facts := map[string]any{"checkpoint": name}
	switch name {
	case "source": // the claim-source UI bound the scene to the Facebook post with private replies on
		var sources int
		x.need(t, owner.QueryRow(ctx, `SELECT count(*) FROM live.claim_sources WHERE tenant_id=$1 AND store_id=$2 AND active AND platform='facebook' AND asset_id=$3 AND source_object_id=$4 AND private_reply AND reply_locale='zh-TW'`,
			x.tenant, x.store, x.pageAsset, x.pageAsset+"_"+x.post).Scan(&sources) == nil && sources == 1, "claim source bound to the post: %d rows", sources)
		x.need(t, x.count(`SELECT count(*) FROM live.claim_windows WHERE tenant_id=$1 AND store_id=$2 AND state='OPEN'`, x.tenant, x.store) == 1, "exactly one OPEN claim window")
		x.need(t, x.count(`SELECT count(*) FROM live.offers WHERE tenant_id=$1 AND store_id=$2 AND sku_id=$3`, x.tenant, x.store, x.stock.skus[0].ID) == 1, "one offer for the SKU")
	case "claimed":
		c := x.claimFacts()
		x.need(t, c.bundles == 1 && c.accepted == 1 && c.ops == 1 && c.sends == 1, "claim facts %+v want one bundle, one accepted event, one reply op, one send", c)
		var qty int
		var ownerBound bool
		x.need(t, owner.QueryRow(ctx, `SELECT l.quantity,b.owner_id IS NOT NULL FROM claims.lines l JOIN claims.bundles b ON b.tenant_id=l.tenant_id AND b.store_id=l.store_id AND b.id=l.bundle_id
			WHERE l.tenant_id=$1 AND l.store_id=$2 AND l.sku_id=$3`, x.tenant, x.store, x.stock.skus[0].ID).Scan(&qty, &ownerBound) == nil && qty == 2 && !ownerBound, "claimed line qty=%d bound=%v", qty, ownerBound)
		var state string
		x.need(t, owner.QueryRow(ctx, `SELECT state FROM integration.operations WHERE action='meta.private_reply'`).Scan(&state) == nil, "reply operation readable")
		facts["reply_operation_state"] = state
		s := x.stockNow(t)
		x.need(t, s.reserved == 0 && s.allocated == 0, "a claim must not touch stock: %+v", s)
		x.need(t, x.count(`SELECT count(*) FROM checkout.orders WHERE store_id=$1`, x.store) == 0, "a claim must not create an order")
	case "cart":
		var qty int
		x.need(t, owner.QueryRow(ctx, `SELECT c.quantity FROM claims.bundles b JOIN storefront.cart_lines c ON c.tenant_id=b.tenant_id AND c.store_id=b.store_id AND c.owner_id=b.owner_id AND c.sku_id=$3
			WHERE b.tenant_id=$1 AND b.store_id=$2`, x.tenant, x.store, x.stock.skus[0].ID).Scan(&qty) == nil && qty == 2, "buyer cart qty=%d want 2", qty)
		x.need(t, x.count(`SELECT count(*) FROM claims.bundles WHERE store_id=$1 AND owner_id IS NOT NULL`, x.store) == 1, "the bundle is bound to the buyer")
		s := x.stockNow(t)
		x.need(t, s.reserved == 0 && s.allocated == 0, "the cart holds no stock: %+v", s)
	case "ordered":
		id, commercial, fulfilment, total := x.orderState(t)
		x.order = id
		x.need(t, commercial == "DRAFT" && fulfilment == "MANUAL_UNASSIGNED" && total == 2500, "order %s/%s total=%d", commercial, fulfilment, total)
		if want, _ := in["order"].(string); want != "" {
			x.need(t, want == id, "the order the buyer sees is not the stored one")
		}
		s := x.stockNow(t)
		x.need(t, s.reserved == 2 && s.allocated == 0 && s.onHand == 10, "an unpaid order holds 2 units: %+v", s)
		facts["total_minor"] = total
	case "paid":
		id, commercial, fulfilment, total := x.orderState(t)
		x.need(t, id == x.order && commercial == "CONFIRMED" && fulfilment == "MANUAL_UNASSIGNED" && total == 2500, "order %s/%s total=%d", commercial, fulfilment, total)
		var captured int64
		var pi *string
		x.need(t, owner.QueryRow(ctx, `SELECT f.amount_minor,s.payment_intent_id FROM payments.facts f JOIN payments.stripe_sessions s ON s.attempt_id=f.attempt_id WHERE f.attempt_id=$1 AND f.kind='CAPTURED'`, x.attempt).Scan(&captured, &pi) == nil &&
			captured == 2500 && pi != nil, "CAPTURED fact of the paid attempt")
		s := x.stockNow(t)
		x.need(t, s.onHand == 10 && s.reserved == 0 && s.allocated == 2, "stock after capture: %+v want on_hand 10, reserved 0, allocated 2", s)
		x.need(t, x.count(`SELECT count(*) FROM payments.facts WHERE attempt_id=$1 AND kind='CAPTURED'`, x.attempt) == 1, "exactly one CAPTURED fact")
		x.opsAtPaid = x.count(`SELECT count(*) FROM integration.operations WHERE action<>'meta.private_reply'`)
		facts["captured_minor"] = captured
	case "shipped":
		var version int
		var status, carrier, tracking string
		x.need(t, owner.QueryRow(ctx, `SELECT version,status,carrier_code,tracking_number FROM fulfillment.manual_shipment_versions WHERE order_id=$1 ORDER BY version DESC LIMIT 1`, x.order).
			Scan(&version, &status, &carrier, &tracking) == nil && version == 1 && status == "SHIPPED" && carrier == e2eCarrier && tracking == e2eTracking, "shipment v%d %s %s (tracking checked separately)", version, status, carrier)
		var fulfilment string
		x.need(t, owner.QueryRow(ctx, `SELECT fulfillment_state FROM checkout.orders WHERE id=$1`, x.order).Scan(&fulfilment) == nil && fulfilment == "MERCHANT_SHIPPED", "fulfilment state %s", fulfilment)
		x.need(t, x.count(`SELECT count(*) FROM integration.operations WHERE action<>'meta.private_reply'`) == x.opsAtPaid, "a manual shipment must not create a provider operation")
		var audit int
		x.need(t, owner.QueryRow(ctx, `SELECT count(*) FROM ops.audit_events WHERE tenant_id=$1 AND store_id=$2 AND action ILIKE '%ship%'`, x.tenant, x.store).Scan(&audit) == nil && audit >= 1, "shipment audit rows: %d", audit)
		s := x.stockNow(t)
		x.need(t, s.onHand == 10 && s.reserved == 0 && s.allocated == 2, "shipping moves no stock: %+v", s)
	case "refunded":
		var refunds int
		var amount int64
		var stripeID *string
		x.need(t, owner.QueryRow(ctx, `SELECT count(*),coalesce(sum(r.amount_minor),0)::bigint,max(f.stripe_refund_id) FROM payments.stripe_refunds r
			JOIN payments.refund_facts f ON f.refund_id=r.id AND f.kind='SUCCEEDED' WHERE r.attempt_id=$1`, x.attempt).Scan(&refunds, &amount, &stripeID) == nil &&
			refunds == 1 && amount == e2eRefundMinor && stripeID != nil && strings.HasPrefix(*stripeID, "re_"), "SUCCEEDED refunds=%d amount=%d", refunds, amount)
		var pi string
		x.need(t, owner.QueryRow(ctx, `SELECT payment_intent_id FROM payments.stripe_sessions WHERE attempt_id=$1`, x.attempt).Scan(&pi) == nil && len(x.e.fake.RefundIDs(pi)) == 1, "the fake Stripe holds exactly one refund")
		id, commercial, fulfilment, _ := x.orderState(t)
		x.need(t, id == x.order && commercial == "CONFIRMED" && fulfilment == "MERCHANT_SHIPPED", "a refund never changes the order: %s/%s", commercial, fulfilment)
		s := x.stockNow(t)
		x.need(t, s.onHand == 10 && s.reserved == 0 && s.allocated == 2, "a refund never restocks: %+v", s)
		facts["refunded_minor"] = amount
	default:
		t.Fatalf("unknown checkpoint %q", name)
	}
	return facts
}

// pay: the fake Stripe pays the pinned session and the signed webhook reaches the real ingress, exactly what Stripe does
// after the buyer completes the hosted page (the synthetic page in the browser is only a stand-in for the card form).
func (x *e2eRun) pay(t *testing.T, order string) map[string]any {
	t.Helper()
	e := x.e
	x.need(t, order == x.order, "pay was asked for order %q, the stored order is %q", order, x.order)
	deadline := time.Now().Add(45 * time.Second)
	for {
		err := e.f.owner.QueryRow(x.ctx, `SELECT a.id::text,s.session_id FROM checkout.payment_attempts a JOIN payments.stripe_sessions s ON s.attempt_id=a.id WHERE a.order_id=$1 AND s.session_id IS NOT NULL`, order).Scan(&x.attempt, &x.session)
		if err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("no pinned Stripe session for the order after the buyer clicked Pay: %v", err)
		}
		time.Sleep(300 * time.Millisecond)
	}
	if !e.fake.SetState(x.session, "complete", "paid") {
		t.Fatal("the fake does not know the buyer's session")
	}
	if status := e.deliver(t, x.endpoint, x.webhookSecret, sflEvent(x.attempt, x.session)); status != 200 {
		t.Fatalf("the signed checkout webhook answered %d", status)
	}
	e.awaitFact(t, x.attempt, "CAPTURED")
	e.await(t, "payment intent pinned", x.attempt, 30*time.Second, `SELECT payment_intent_id IS NOT NULL FROM payments.stripe_sessions WHERE attempt_id=$1`)
	return map[string]any{"captured": true}
}

// settleRefund plays the provider side of R-8 (the fake sends no webhook): keep the worker's sleeping refund jobs due until
// the SUCCEEDED fact is committed. Bounded; the page assertions still decide.
func (x *e2eRun) settleRefund(t *testing.T) map[string]any {
	t.Helper()
	o := rfxOrder{attempt: x.attempt}
	deadline := time.Now().Add(60 * time.Second)
	for {
		x.e.wakeAllRefunds(o)
		if x.count(`SELECT count(*) FROM payments.refund_facts f JOIN payments.stripe_refunds r ON r.id=f.refund_id WHERE r.attempt_id=$1 AND f.kind='SUCCEEDED'`, x.attempt) == 1 {
			return map[string]any{"settled": true}
		}
		if time.Now().After(deadline) {
			t.Fatalf("the refund did not settle within 60s (refund POSTs at the fake: %d)", x.e.fake.RefundPosts())
		}
		time.Sleep(300 * time.Millisecond)
	}
}

// isolation: a merchant of ANOTHER tenant reads nothing of this store through the real admin API.
func (x *e2eRun) isolation(t *testing.T) map[string]any {
	t.Helper()
	other := x.e.f.tokens["b"]
	if os.Getenv("LC_E2E_MUTATE") == "isolation" { // red-run mutation: the store creator's own token stands in for the foreign tenant
		other = x.token
	}
	x.need(t, other != "", "the base fixture has no second-tenant merchant session")
	get := func(token, path string) (int, string) {
		req, _ := http.NewRequestWithContext(x.ctx, http.MethodGet, x.adminAPI.URL+path, nil)
		req.Header.Set("Authorization", "Bearer "+token)
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		raw, _ := io.ReadAll(io.LimitReader(res.Body, 1<<20))
		return res.StatusCode, string(raw)
	}
	base := "/v1/admin/stores/" + x.store
	paths := []string{base + "/orders?limit=10", base + "/orders/" + x.order + "/refunds", base + "/orders/" + x.order + "/shipment/history", base + "/live-sessions",
		base + "/orders/unshipped.csv"}
	results := map[string]int{}
	for _, p := range paths {
		status, body := get(other, p)
		results[p] = status
		if status == 200 || strings.Contains(body, x.order) || strings.Contains(body, e2eTracking) || strings.Contains(body, x.stock.skus[0].Code) {
			t.Fatalf("a foreign tenant reached %s: status %d", p, status)
		}
	}
	// sanity: the owner's own token does read the same route (the check can fail)
	if status, body := get(x.token, base+"/orders?limit=10"); status != 200 || !strings.Contains(body, x.order) {
		t.Fatalf("control read by the store creator failed: %d", status)
	}
	facts := fmt.Sprintf("%v", results)
	return map[string]any{"foreign_statuses": facts}
}

// e2eScan returns the first needle (label, file) found in any .log/.json file of dir.
func e2eScan(dir string, needles map[string]string) (label, file string) {
	entries, _ := os.ReadDir(dir)
	for _, entry := range entries {
		if entry.IsDir() || !(strings.HasSuffix(entry.Name(), ".log") || strings.HasSuffix(entry.Name(), ".json")) {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(dir, entry.Name()))
		if err != nil {
			continue
		}
		for l, value := range needles {
			if value != "" && bytes.Contains(raw, []byte(value)) {
				return l, entry.Name()
			}
		}
	}
	return "", ""
}

// finish scans every log this run produced for secrets and PII and records the summary. The scanner is proven able to
// fail first (PROCESS §2.4): a planted needle in a scratch directory must be found, a clean one must not.
func (x *e2eRun) finish(t *testing.T) {
	t.Helper()
	needles := map[string]string{"recipient": "Synthetic Gate Recipient", "phone": "+886900000091", "address": "Synthetic Address Ninety One"}
	for label, value := range x.secrets {
		needles[label] = value
	}
	scratch := t.TempDir()
	if err := os.WriteFile(filepath.Join(scratch, "clean.log"), []byte("nothing sensitive here"), 0o600); err != nil {
		t.Fatal(err)
	}
	if l, _ := e2eScan(scratch, needles); l != "" {
		t.Fatalf("scanner self-test: a clean file matched %q", l)
	}
	if err := os.WriteFile(filepath.Join(scratch, "leak.log"), []byte("x "+x.secrets["claim link token"]+" y"), 0o600); err != nil {
		t.Fatal(err)
	}
	if l, f := e2eScan(scratch, needles); l != "claim link token" || f != "leak.log" {
		t.Fatalf("scanner self-test: a planted claim token was not found (%q in %q)", l, f)
	}
	if os.Getenv("LC_E2E_MUTATE") == "leak" { // red-run mutation: a claim token lands in a log
		_ = os.WriteFile(filepath.Join(x.evidence, "mutation.log"), []byte("token="+x.linkToken), 0o600)
	}
	if l, f := e2eScan(x.evidence, needles); l != "" {
		t.Fatalf("SECRET/PII LEAK: %q found in %s", l, f)
	}
	// Positive control: the Graph fake legitimately receives the Page token, so it is present THERE (and only there).
	found := false
	for _, r := range x.graph.all() {
		found = found || strings.Contains(string(r.body), x.mci.pageToken) || r.bodyToken == x.mci.pageToken || strings.Contains(r.rawQuery, x.mci.pageToken) || r.header.Get("Authorization") != ""
	}
	x.need(t, found, "the leak scan's positive control (Page token in the Graph request) did not fire")
	t.Logf("T12 PG connection peak: %d of 60 (max_connections)", x.peakConnections.Load())
	t.Logf("T12 evidence logs scanned clean")
}

// TestBrowserE2EDealLoopSandbox is the SANDBOX tier of the same chain (card 4242 on the real hosted page of
// checkout.stripe.com, test mode, owner test keys through the SP18 harness). It is NOT_RUN unless the owner opts in with
// STRIPE_BROWSER=1 and STRIPE_SANDBOX=1, and even then it is NOT wired here: entering a card number on a third-party page
// and using the owner's Stripe test keys need an owner ruling (T12 return, rulings_needed). Never PASS by skipping.
func TestBrowserE2EDealLoopSandbox(t *testing.T) {
	e2eRequire(t)
	if os.Getenv("STRIPE_BROWSER") != "1" || os.Getenv("STRIPE_SANDBOX") != "1" {
		t.Skip("NOT_RUN: SANDBOX tier needs STRIPE_BROWSER=1 and STRIPE_SANDBOX=1 with the owner's test keys")
	}
	t.Skip("NOT_RUN: SANDBOX tier is not wired in T12 (owner ruling needed: card entry on the real hosted page); the SP18 harness of tests/foundation/browser_stripe_test.go remains the only SANDBOX evidence")
}
