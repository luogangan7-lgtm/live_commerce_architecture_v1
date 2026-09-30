//go:build browser

package foundation_test

// TCV08 (contracts/taiwan-cvs-logistics-v1.md §10 TCV08) and the browser half of TCV14: real browsers. `TestBrowserTaiwanCvs`, prefix `brc`.
// Run through `bash scripts/dev/test-local.sh --browser-cvs` (isolated PG 18, production storefront + admin Next builds).
//
// Stack (MOCK variant, always): three stores on three ACTIVE hosts served by one private buyerhttp handler behind the production storefront
// Next build; the ecpaytest fake answers the stage map (over real HTTP, behind a synthetic edge) and the Go hooks handler answers the provider
// return; the merchant admin half runs the production admin Next build against the private Go API with the CVS routes and a signed mock OIDC issuer.
// Evidence labels: MOCK (fake map/Create, signed MOCK status posts). SANDBOX variant (stage map + Stripe 4242) needs ECPAY_LOGISTICS_SANDBOX=1
// STRIPE_SANDBOX=1 STRIPE_BROWSER=1 and the SP18 harness: otherwise NOT_RUN. The WebKit subtest re-runs the MOCK stack under Playwright WebKit (iPhone 15 / Desktop Safari) when it is installed, NOT_RUN otherwise.
// Owner-pool writes (disclosed fixtures): the registrar-only ok/hilife flags are not touched; product names for screenshots only.

import (
	"context"
	"encoding/base64"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"livecommerce/internal/buyerhttp"
	"livecommerce/internal/fulfillment"
	"livecommerce/internal/httpapi"
	"livecommerce/internal/identity"
	"livecommerce/internal/identityhttp"
	"livecommerce/internal/integrations/shipping/ecpay/ecpaytest"
	"livecommerce/internal/oidclogin"
	"livecommerce/internal/platform"
)

func brcRequire(t *testing.T) {
	t.Helper()
	if os.Getenv("LC_BROWSER_CVS_ACCEPTANCE") != "1" || os.Getenv("LC_TEST_DATABASE_ALLOWED") != "1" {
		t.Fatal("use scripts/dev/test-local.sh --browser-cvs")
	}
}

func TestBrowserTaiwanCvs(t *testing.T) {
	brcRequire(t)
	t.Run("MOCK", brcMock)
	t.Run("SANDBOX", func(t *testing.T) {
		if os.Getenv("ECPAY_LOGISTICS_SANDBOX") != "1" || os.Getenv("STRIPE_SANDBOX") != "1" || os.Getenv("STRIPE_BROWSER") != "1" {
			t.Skip("NOT_RUN: the SANDBOX variant (stage map + Stripe 4242) needs ECPAY_LOGISTICS_SANDBOX=1 STRIPE_SANDBOX=1 STRIPE_BROWSER=1 and the owner's test keys")
		}
		// Probed 2026-10-01: POST https://logistics-stage.ecpay.com.tw/Express/map (MerchantID 2000933, no CheckMacValue) answers 200 with an
		// auto-post form into the multi-hop 7-ELEVEN e-map page, so the stage map itself needs no keys. What blocks this variant is the rest of
		// the chain: connecting the store runs the signed GetStoreList probe, and verifying the returned store id runs it again, both with the
		// stage HashKey/HashIV of merchant 2000933 (ECPAY_STAGE_C2C_* as in tests/integrations/ecpay), which are not provisioned in
		// ~/.config/livecommerce/secrets.env; and the tcvEnv fixtures are built on the ecpaytest fake transport, not the real stage host.
		// The Stripe 4242 half exists already (LC_BROWSER_ENGINE=webkit|chromium ... --stripe-browser, SP18) and is not repeated here.
		t.Skip("NOT_RUN: the SANDBOX variant needs ECPAY_STAGE_C2C_{MERCHANT_ID,HASH_KEY,HASH_IV} for the stage GetStoreList probe and directory verification (not provisioned) and a stage-transport tcvEnv; the stage map itself is keyless but hands off to an interactive e-map page that is not scripted")
	})
	t.Run("WebKit", func(t *testing.T) {
		// MOCK stack again (fresh stores, fresh evidence dir) with LC_BROWSER_ENGINE=webkit: the buyer half runs in Playwright's iPhone 15
		// (phone matrix) / Desktop Safari (desktop matrix) profiles and the merchant half in Desktop Safari. This is where the iOS same-tab map
		// round trip and Device=1 for an iPhone user agent (B20) are proven on the real Safari engine, not on Chromium's Pixel profile.
		root, _ := filepath.Abs("../..")
		if !brcWebKitInstalled(root) {
			t.Skip("NOT_RUN: Playwright WebKit is not installed (pnpm exec playwright install webkit)")
		}
		t.Setenv("LC_BROWSER_ENGINE", "webkit")
		brcMock(t)
	})
}

// brcWebKitInstalled asks Playwright itself where its WebKit binary lives (PLAYWRIGHT_BROWSERS_PATH, per-OS cache, revision pinned by the
// installed @playwright/test) and reports whether that file exists. A missing Node/Playwright is "not installed" too: the subtest then says NOT_RUN.
func brcWebKitInstalled(root string) bool {
	cmd := exec.Command("node", "-e", `import("@playwright/test").then(m=>console.log(m.webkit.executablePath()))`)
	cmd.Dir = root
	out, err := cmd.Output()
	if err != nil {
		return false
	}
	_, err = os.Stat(strings.TrimSpace(string(out)))
	return err == nil
}

func brcMock(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()
	root, _ := filepath.Abs("../..")
	evidence := brfEvidence(t, root, "taiwan-cvs")

	const o1, o2, o3 = "https://buyer.example", "https://buyer2.example", "https://buyer3.example"
	e1 := tcvNew(t, tcvOpts{origin: o1})
	e2 := tcvNew(t, tcvOpts{origin: o2, share: e1})
	e3 := tcvNew(t, tcvOpts{origin: o3, share: e1}) // no ECPay profile: buyer_entered mode
	for _, e := range []*tcvEnv{e1, e2, e3} {
		e.grantCreator("orders:read", "fulfillment:write", "integration:manage", "integration:read")
	}
	e1.startDispatcher()
	e1.cvsSettings(tcvAllChains, true, "20000", 500)
	// a buyer_entered pay-at-pickup order first (the store has no ECPay profile yet), then the connection and the API services
	manual, _, _ := e1.service("cvs_711", "MANUAL", 0)
	eb := e1.newBuyer()
	entered, err := e1.tppPlace(eb, manual, e1.tppEntered(eb, manual), tppName, tppPhone)
	if err != nil {
		t.Fatalf("buyer_entered order: %v", err)
	}
	enteredOrder := entered.OrderID
	e1.updateService(manual, func(in *fulfillment.ServiceInput) { in.Enabled = false })
	e1.connect("C2C")
	e2.connect("C2C")
	for _, k := range []struct{ kind, mode string }{{"cvs_711", "API"}, {"cvs_familymart", "API"}, {"cvs_hilife", "MANUAL"}, {"cvs_okmart", "MANUAL"}} {
		code, _, _ := e1.service(k.kind, k.mode, 0)
		if k.kind == "cvs_711" {
			e1.apiCode = code
		}
	}
	e2.service("cvs_711", "API", 0)
	e2.cvsSettings(tcvAllChains, true, "20000", 500)
	for _, kind := range []string{"cvs_711", "cvs_familymart", "cvs_hilife", "cvs_okmart"} {
		e3.service(kind, "MANUAL", 0)
	}
	for _, e := range []*tcvEnv{e1, e2, e3} {
		mustExec(t, e.p.f.owner, `UPDATE catalog.products SET name='Synthetic CVS browser product',description='Synthetic acceptance fixture' WHERE id=$1`, e.p.stock.product.ID)
	}

	bffKey := base64.RawURLEncoding.EncodeToString(randomBytes(32))
	handler, err := buyerhttp.New(ctx, e1.p.a.issuer, e1.p.a.runtime, e1.svc, bffKey, time.Hour)
	if err != nil {
		t.Fatalf("buyer HTTP constructor: %v", err)
	}
	buyerAPI := newHTTPServer(t, handler)
	hooks := newHTTPServer(t, e1.hooks)
	fake := newFakeServer(e1.fake)
	t.Cleanup(fake.Close)

	values := map[string]string{"COMMERCE_BUYER_WEB_ENABLED": "1", "COMMERCE_BUYER_API_ORIGIN": buyerAPI.URL, "COMMERCE_BUYER_DEMO_LABEL": "1", "COMMERCE_BUYER_BFF_KEY": bffKey,
		"COMMERCE_BUYER_COOKIE_KEY": base64.RawURLEncoding.EncodeToString(randomBytes(32)), "COMMERCE_BUYER_SESSION_TTL": "3600",
		"LC_CVS_EVIDENCE": evidence, "LC_CVS_FAKE_URL": fake.URL, "LC_CVS_HOOKS_URL": hooks.URL,
		"LC_CVS_ORIGIN1": o1, "LC_CVS_ORIGIN2": o2, "LC_CVS_ORIGIN3": o3,
		"LC_CVS_PRODUCT1": e1.p.stock.product.ID, "LC_CVS_PRODUCT2": e2.p.stock.product.ID, "LC_CVS_PRODUCT3": e3.p.stock.product.ID}
	cmd := exec.CommandContext(ctx, "node", "tests/storefront/cvs-buyer.mjs")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	defer func() {
		if cmd.Process != nil {
			_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		}
	}()
	cmd.Dir = root
	cmd.Env = browserEnvironment(values)
	log := browserLog(t, filepath.Join(evidence, "cvs-buyer.mjs.log"))
	cmd.Stdout, cmd.Stderr = log, log
	if err := cmd.Run(); err != nil {
		t.Fatalf("storefront browser gate failed: %v; evidence=%s", err, evidence)
	}
	brfShots(t, evidence, 8)

	// ---- merchant half (production admin Next build, signed mock OIDC, private Go API with the CVS routes) --------------------------
	e1.startDispatcherWith(nil)
	e1.routeNewJobs()
	pap := func() string {
		order, _ := e1.cvsOrder(tcvOrderSpec{kind: "cvs_711", code: e1.apiCode, paymentMode: "pay_at_pickup"})
		return order
	}
	createOrder, cancelOrder, collectOrder := pap(), pap(), pap()
	if st, _, raw := e1.mcall(e1.token(), "PUT", "/v1/admin/stores/"+e1.store()+"/orders/"+collectOrder+"/shipment", t04Key("brc-ms"), mfxShip(0, "seven_eleven_cvs", "0012345678")); st != 200 {
		t.Fatalf("manual shipment of the collect fixture: %d %s", st, raw)
	}
	e1.fake.SetCreateMode(ecpaytest.Create403, "")
	unknownOrder := pap()
	if st, _, raw := e1.ship(e1.token(), unknownOrder, 0, "", true); st != 202 {
		t.Fatalf("request: %d %s", st, raw)
	}
	e1.awaitShip(unknownOrder, "UNKNOWN")
	e1.fake.SetCreateMode(ecpaytest.CreateOK, "")
	stack := brcStartAdmin(t, ctx, e1, evidence)
	env := map[string]string{"LC_BROWSER_STORE": e1.store(), "LC_BROWSER_CVS_CREATE_ORDER": createOrder, "LC_BROWSER_CVS_CANCEL_ORDER": cancelOrder,
		"LC_BROWSER_CVS_COLLECT_ORDER": collectOrder, "LC_BROWSER_CVS_UNKNOWN_ORDER": unknownOrder, "LC_BROWSER_CVS_ENTERED_ORDER": enteredOrder}
	brfPlaywright(t, ctx, stack, []string{"taiwan-cvs.spec.ts"}, env)
	if s, _, _ := e1.shipState(createOrder); s != "CREATED" {
		t.Errorf("the UI-created label is %s, want CREATED", s)
	}
	if got := e1.collectionState(collectOrder); got != "COLLECTED" {
		t.Errorf("collect fixture is %s, want COLLECTED", got)
	}
	if got := e1.collectionState(cancelOrder); got != "CANCELLED" || len(e1.deallocRows(cancelOrder)) != 1 {
		t.Errorf("cancel fixture is %s with %d DEALLOCATE rows", got, len(e1.deallocRows(cancelOrder)))
	}
	if e1.fake.TotalCreates() < 1 {
		t.Error("the fake ECPay never received the Create the merchant UI requested")
	}

	// PG facts after the buyer half: every map round trip was a real selection, verified against the directory, on its own store
	if n := e1.count(`SELECT count(*) FROM fulfillment.cvs_selections WHERE store_id=$1 AND state='VERIFIED' AND return_origin=$2`, e1.store(), o1); n < 5 {
		t.Errorf("store 1: %d VERIFIED selections with return_origin %s, want at least 5 (4 locale/viewport runs + the change-store run)", n, o1)
	}
	if n := e2.count(`SELECT count(*) FROM fulfillment.cvs_selections WHERE store_id=$1 AND state='VERIFIED' AND return_origin=$2`, e2.store(), o2); n != 1 {
		t.Errorf("store 2: %d VERIFIED selections with return_origin %s, want 1", n, o2)
	}
	if n := e1.count(`SELECT count(*) FROM fulfillment.cvs_selections WHERE store_id=$1 AND return_origin<>$2`, e1.store(), o1); n != 0 {
		t.Errorf("store 1 has %d selections returning to another origin", n)
	}
	if n := e2.count(`SELECT count(*) FROM checkout.orders WHERE store_id=$1 AND payment_mode='pay_at_pickup' AND collection_state='PENDING' AND commercial_state='CONFIRMED'`, e2.store()); n != 1 {
		t.Errorf("store 2: %d CONFIRMED pay-at-pickup orders placed through the UI, want 1", n)
	}
	if n := e2.count(`SELECT count(*) FROM checkout.payment_attempts a JOIN checkout.orders o ON o.id=a.order_id WHERE o.store_id=$1`, e2.store()); n != 0 {
		t.Errorf("store 2: %d payment attempts for a pay-at-pickup order", n)
	}
	if n := e3.count(`SELECT count(*) FROM fulfillment.cvs_selections WHERE store_id=$1`, e3.store()); n != 0 {
		t.Errorf("the buyer_entered store opened %d ECPay map selections", n)
	}
	if n := e1.count(`SELECT count(*) FROM fulfillment.pickup_versions WHERE tenant_id=$1 AND store_id=$2 AND verification_kind='PROVIDER_DIRECTORY_VERIFIED' AND namespace='ecpay.sandbox.unimartc2c'`, e1.tenant(), e1.store()); n < 1 {
		t.Error("no directory-verified pickup row was written for store 1")
	}
	t.Logf("TCV08 BROWSER (MOCK) buyer half + TCV14 buyer_entered UI half passed; screenshots hashed; evidence=%s", evidence)
}

// brcStartAdmin is brfStartAdmin for the CVS gates: the private Go API (identity + admin routes incl. the CVS routes) over the isolated database,
// and the production admin Next build against it. The store creator is the principal the mock IdP subject maps to.
func brcStartAdmin(t *testing.T, ctx context.Context, e *tcvEnv, evidence string) *brfStack {
	t.Helper()
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := listener.Addr().String()
	_ = listener.Close()
	_, port, _ := net.SplitHostPort(address)
	origin := browserFront(t, address) // https TLS front under LC_BROWSER_ENGINE=webkit, else http://address
	f := e.p.f
	idp := newBrowserIDP(t, origin+"/api/auth/callback")
	mustExec(t, f.owner, `INSERT INTO identity.external_identities(issuer,subject,principal_id) VALUES($1,'browser-subject',$2)`, idp.server.URL, f.principalA)
	role := "brc_" + strings.ReplaceAll(randomUUID(), "-", "")
	password := randomToken()
	mustExec(t, f.owner, `CREATE ROLE `+pgx.Identifier{role}.Sanitize()+` LOGIN NOSUPERUSER NOBYPASSRLS NOCREATEDB NOCREATEROLE IN ROLE commerce_identity PASSWORD '`+password+`'`)
	t.Cleanup(func() { mustExec(t, f.owner, `DROP ROLE `+pgx.Identifier{role}.Sanitize()) })
	u, err := url.Parse(f.databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	u.User = url.UserPassword(role, password)
	authority, err := platform.OpenIdentityPool(ctx, u.String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(authority.Close)
	provider, err := oidclogin.New(ctx, oidclogin.Config{Issuer: idp.server.URL, ClientID: browserClientID, RedirectURL: idp.redirect, AllowLoopbackForTests: true})
	if err != nil {
		t.Fatal(err)
	}
	service, err := identity.New(authority, observedBrowserProvider{Provider: provider, t: t}, identity.Policy{ProviderKey: "browser-taiwan-cvs-v1", SessionTTL: time.Hour, OnboardingEnabled: true, Currencies: []string{"TWD", "USD"}})
	if err != nil {
		t.Fatal(err)
	}
	bffKey := randomToken()
	private, err := identityhttp.NewHandler(service, bffKey)
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	mux.Handle("/v1/identity/", private)
	mux.Handle("/", httpapi.NewHandler(f.runtime, httpapi.Options{SessionStoreList: true, CVS: e.cvs}))
	api := httptest.NewServer(mux)
	t.Cleanup(api.Close)
	nextLog := browserLog(t, filepath.Join(evidence, "admin-next.log"))
	next := exec.CommandContext(ctx, "node", filepath.Join(root, "apps/admin/.next/standalone/apps/admin/server.js"))
	next.Dir = root
	next.Env = browserEnvironment(map[string]string{"HOSTNAME": "127.0.0.1", "PORT": port, "NODE_ENV": "production", "COMMERCE_IDENTITY_ENABLED": "1", "COMMERCE_IDENTITY_ALLOW_LOOPBACK_TESTS": "1",
		"COMMERCE_PUBLIC_ORIGIN": origin, "COMMERCE_API_ORIGIN": api.URL, "COMMERCE_OIDC_ISSUER": idp.server.URL, "COMMERCE_BFF_KEY": bffKey, "COMMERCE_ONBOARDING_ENABLED": "1", "COMMERCE_ONBOARDING_CURRENCIES": "TWD,USD"})
	next.Stdout, next.Stderr = nextLog, nextLog
	next.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := next.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = syscall.Kill(-next.Process.Pid, syscall.SIGKILL); _, _ = next.Process.Wait() })
	client := &http.Client{Timeout: time.Second}
	for attempt := 0; ; attempt++ {
		if response, err := client.Get("http://" + address + "/api/stores"); err == nil {
			_ = response.Body.Close()
			if response.StatusCode == http.StatusUnauthorized {
				break
			}
		}
		if attempt > 100 {
			t.Fatalf("admin Next readiness failed; evidence=%s", evidence)
		}
		time.Sleep(100 * time.Millisecond)
	}
	return &brfStack{root: root, evidence: evidence, origin: origin, api: api}
}

var _ = fmt.Sprintf
