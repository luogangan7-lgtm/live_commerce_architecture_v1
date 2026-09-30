//go:build browser

package foundation_test

// CB11 (contracts/customers-billing-v1.md §8 CB11; customers-billing-ui.md U2/U4/U6/U7/U8): real browsers.
//
// Stack: an isolated PG 18 container (rfx harness), the real payment worker and independent Stripe fake (the paid
// order goes through the real capture path, labelled MOCK), the real private Go API (identity + admin routes incl.
// customers, finance, billing and the Studio claims routes) with the platform-fee service pointed at the independent
// billingtest fake (checkout.stripe.com / billing.stripe.com are answered inside the browser by page.route: the
// Stripe redirect is MOCK), the production admin Next build with a signed mock IdP, and, for the buyer half, the real
// buyerhttp handler behind the production storefront Next build.
//
// Admin half: tests/admin/customers-billing.spec.ts (Playwright, en + zh-TW, desktop 1586x992 + 390 px, screenshots
// hashed). Buyer half: tests/storefront/privacy-buyer.mjs (checkout consent boxes, privacy page, public data-deletion
// page; en + zh-TW, desktop + 390 px). The runner-only control listener changes the store's billing standing through
// the real webhook path (fake subscription + signed event) or a disclosed owner-pool reset; Node never receives a
// database credential. Evidence: output/playwright/customers-billing/<timestamp>/.

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
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
	"livecommerce/internal/billing"
	"livecommerce/internal/billing/billingtest"
	"livecommerce/internal/buyer"
	"livecommerce/internal/buyerhttp"
	"livecommerce/internal/claims"
	"livecommerce/internal/customers"
	"livecommerce/internal/httpapi"
	"livecommerce/internal/identity"
	"livecommerce/internal/identityhttp"
	"livecommerce/internal/oidclogin"
	"livecommerce/internal/platform"
	"livecommerce/internal/storefront"
)

func cbbrRequire(t *testing.T) {
	t.Helper()
	if os.Getenv("LC_BROWSER_CUSTOMERS_BILLING_ACCEPTANCE") != "1" || os.Getenv("LC_TEST_DATABASE_ALLOWED") != "1" {
		t.Fatal("use scripts/dev/test-local.sh --browser-customers-billing (mode shipped as output/customers-billing-tests/test-local-mode.patch)")
	}
}

// cbbrStartAdmin is brfStartAdmin with the API options of this gate (billing service, Studio claims routes).
func cbbrStartAdmin(t *testing.T, ctx context.Context, e *rfxEnv, principal, evidence string, options func(origin string) httpapi.Options) *brfStack {
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
	origin := "http://" + address
	idp := newBrowserIDP(t, origin+"/api/auth/callback")
	mustExec(t, e.f.owner, `INSERT INTO identity.external_identities(issuer,subject,principal_id) VALUES($1,'browser-subject',$2)`, idp.server.URL, principal)
	role := "cbbr_" + strings.ReplaceAll(randomUUID(), "-", "")
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
	provider, err := oidclogin.New(ctx, oidclogin.Config{Issuer: idp.server.URL, ClientID: browserClientID, RedirectURL: idp.redirect, AllowLoopbackForTests: true})
	if err != nil {
		t.Fatal(err)
	}
	service, err := identity.New(authority, observedBrowserProvider{Provider: provider, t: t}, identity.Policy{ProviderKey: "browser-customers-billing-v1", SessionTTL: time.Hour, OnboardingEnabled: true, Currencies: []string{"TWD", "USD"}})
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
	mux.Handle("/", httpapi.NewHandler(e.f.runtime, options(origin)))
	api := httptest.NewServer(mux)
	t.Cleanup(api.Close)
	nextLog := browserLog(t, filepath.Join(evidence, "next.log"))
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
		if response, err := client.Get(origin + "/api/stores"); err == nil {
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
	return &brfStack{e: e, root: root, evidence: evidence, origin: origin, api: api}
}

// cbbrShots verifies the hashed screenshot manifest: every requested locale and both viewports are present.
func cbbrShots(t *testing.T, evidence string, min int, locales ...string) {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(evidence, "screenshots.json"))
	if err != nil {
		t.Fatalf("screenshot hash manifest missing: %v; evidence=%s", err, evidence)
	}
	var shots []struct{ File, Sha256, Locale, Viewport string }
	if err := json.Unmarshal(raw, &shots); err != nil || len(shots) < min {
		t.Fatalf("screenshot manifest has %d entries (want >= %d): %v", len(shots), min, err)
	}
	seen := map[string]bool{}
	for _, s := range shots {
		if len(s.Sha256) != 64 {
			t.Fatalf("screenshot %s is not hashed", s.File)
		}
		seen[s.Locale], seen[s.Viewport] = true, true
	}
	for _, l := range append(locales, "desktop", "mobile") {
		if !seen[l] {
			t.Fatalf("no screenshot for %s", l)
		}
	}
}

func TestBrowserCustomersBilling(t *testing.T) {
	cbbrRequire(t)
	ctx, cancel := context.WithTimeout(context.Background(), 24*time.Minute)
	defer cancel()
	e := rfxNew(t)
	e.startWorker(t)
	base := e.storeFor(t)
	main := e.payInStore(t, base) // the paid order (real capture path) of the main customer
	e.grant(t, main, "customers:read", "customers:privacy", "billing:manage", "live:read", "live:manage")
	store, tenant := main.store(), main.s.p.f.tenantA
	principal := main.s.p.f.principalA
	mainOwner := main.s.p.cap.Scope.OwnerID
	mainToken := main.token()
	restricted, _ := e.member(t, main, "orders:read") // store:read + orders:read only (finance reads under orders:read, contract 6)
	nofinance, _ := e.member(t, main)                 // store:read only: no customers, billing or finance
	// a second customer without any payment (erasable), and the claims of both through the real claims path
	h := cblClaims(t, main)
	skus := h.stock.skus
	claimSession := h.draft(t, store)
	h.offer(t, claimSession, "A1", skus[0].ID, 5)
	h.open(t, claimSession, claims.MatchExact)
	erasableCap := mustIssue(t, main.s.p.cqHarness.service, store)
	for label, cp := range map[string]buyer.Capability{"browser-main": main.s.p.cap, "browser-erasable": erasableCap} {
		r := h.accepted(t, claimSession, "", label, "A1+1")
		if _, err := h.redeem(cp, t04Key("cbbr-redeem"), h.link(t, claimSession, r.BundleID, 0, false).Token, r.BundleVersion); err != nil {
			t.Fatalf("redeem %s: %v", label, err)
		}
	}
	h.closeWindow(t, claimSession)
	studioSession := h.draft(t, store) // the scene of the Studio refusal case: its window was never opened
	erasable := erasableCap.Scope.OwnerID
	consent := func(cp buyer.Capability, purpose, channel string) {
		t.Helper()
		if err := buyer.WithScope(ctx, main.s.p.a.runtime, cp.Token, store, func(c context.Context, tx pgx.Tx, s buyer.Scope) error {
			_, err := customers.BuyerSetConsent(c, tx, s.StoreID, cp.Token, t04Key("cbbr-consent"), customers.ConsentInput{Purpose: purpose, Channel: channel, Granted: true, Context: "settings"})
			return err
		}); err != nil {
			t.Fatalf("consent: %v", err)
		}
	}
	consent(main.s.p.cap, "marketing_messages", "meta_dm")
	consent(main.s.p.cap, "ads_personalization", "meta_ads")
	consent(erasableCap, "marketing_messages", "meta_dm")
	actorKey := cbxOne(t, e.f, `SELECT actor_key FROM claims.bundles WHERE owner_id=$1`, mainOwner)
	if len(actorKey) != 64 {
		t.Fatal("fixture: no actor key to prove absence of")
	}

	// billing: the independent fake behind the real service; the customer is pinned through the real definer
	fake := billingtest.New("acct_CbBrowser0001")
	t.Cleanup(fake.Close)
	cfg := billing.Config{SecretKey: "sk_" + "test_" + fmt.Sprintf("%x", randomBytes(12)), WebhookSecret: "whsec_" + fmt.Sprintf("%x", randomBytes(16)),
		ReturnOrigin: "https://admin.example.test", PriceIDs: []string{"price_Cb11Month"}}
	fake.RequireKey(cfg.SecretKey)
	fake.AddPrice("price_Cb11Month", 30000, "twd", "month", "Pro Monthly", true, false)
	svc, err := billing.New(ctx, e.f.runtime, cfg, fake.Transport())
	if err != nil || !svc.Enabled() {
		t.Fatalf("billing service: enabled=%v err=%v", svc.Enabled(), err)
	}
	ingressPool, err := platform.OpenStripeIngressPool(ctx, sstLogin(t, e.f, "commerce_stripe_ingress"))
	if err != nil {
		t.Fatalf("ingress pool: %v", err)
	}
	t.Cleanup(ingressPool.Close)
	hook := svc.WebhookHandler(ingressPool)
	customer := fake.AddCustomer(map[string]string{"lc_store": store})
	hash := sha256.Sum256([]byte(mainToken))
	if err := platform.WithScope(ctx, e.f.runtime, mainToken, store, "store:read", func(tx pgx.Tx, _ platform.Scope) error {
		var out any
		return tx.QueryRow(ctx, `SELECT billing.pin_customer($1,$2,'SANDBOX',$3,$4)`, hash[:], store, customer, fake.Account()).Scan(&out)
	}); err != nil {
		t.Fatalf("pin customer: %v", err)
	}
	deliver := func(body []byte) {
		t.Helper()
		req := httptest.NewRequest(http.MethodPost, "/v1/platform/stripe/webhook", strings.NewReader(string(body)))
		req.Header.Set("Stripe-Signature", billingtest.Sign(cfg.WebhookSecret, body, time.Now()))
		w := httptest.NewRecorder()
		hook.ServeHTTP(w, req)
		if w.Code != 200 {
			t.Fatalf("platform webhook answered %d", w.Code)
		}
	}
	subscribe := func(status string) {
		t.Helper()
		sub := fake.AddSubscription(billingtest.Sub{Customer: customer, Status: status, StoreMeta: store, PriceID: "price_Cb11Month"})
		deliver(fake.SubscriptionEvent("customer.subscription.updated", sub.ID, billingtest.EventOpts{}))
	}
	reset := func() { // disclosed fixture: back to "never subscribed" at Stripe and in the mirror
		fake.ClearSubscriptions()
		mustExec(t, e.f.owner, `DELETE FROM billing.subscriptions WHERE store_id=$1`, store)
	}
	controlKey := randomToken()
	control := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Gate-Key") != controlKey || r.Method != http.MethodPost {
			http.Error(w, "forbidden", 403)
			return
		}
		switch {
		case r.URL.Path == "/standing/unbilled":
			reset()
		case r.URL.Path == "/standing/good":
			reset()
			subscribe("active")
		case r.URL.Path == "/standing/grace":
			reset()
			subscribe("past_due")
		case r.URL.Path == "/standing/restricted":
			reset()
			subscribe("canceled")
		case r.URL.Path == "/checkout-complete":
			subscribe("active")
		default:
			http.NotFound(w, r)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(control.Close)

	root, _ := filepath.Abs("../..")
	evidence := brfEvidence(t, root, "customers-billing")
	labels, err := claims.NewLabelKey(randomBytes(32))
	if err != nil {
		t.Fatal(err)
	}
	stack := cbbrStartAdmin(t, ctx, e, principal, evidence, func(string) httpapi.Options {
		return httpapi.Options{SessionStoreList: true, RefundJobs: e.jobs, Studio: true, ClaimLabels: &labels, Billing: svc}
	})
	env := map[string]string{
		"LC_BROWSER_STORE": store, "LC_BROWSER_CUSTOMER": mainOwner, "LC_BROWSER_CUSTOMER_ERASE": erasable, "LC_BROWSER_ORDER": main.order,
		"LC_BROWSER_SESSION": studioSession, "LC_BROWSER_PRICE": "price_Cb11Month", "LC_BROWSER_CONTROL": control.URL, "LC_BROWSER_CONTROL_KEY": controlKey,
		"LC_BROWSER_PHONE_TAIL": "001", "LC_BROWSER_PHONE_FULL": "886900000001", "LC_BROWSER_ACTOR_KEY": actorKey, "LC_BROWSER_RESTRICTED_TOKEN": restricted, "LC_BROWSER_NOFIN_TOKEN": nofinance,
	}
	brfPlaywright(t, ctx, stack, []string{"customers-billing.spec.ts"}, env)

	// ---- PG facts after the admin half: the UI drove real commands ----
	count := func(q string, args ...any) int { return countRows(t, e.f.owner, q, args...) }
	if n := count(`SELECT count(*) FROM customers.consent_events WHERE owner_id=$1 AND source='merchant_recorded' AND NOT granted AND purpose='marketing_messages'`, mainOwner); n != 1 {
		t.Errorf("CB11: %d merchant withdrawals of the main customer, want exactly 1 (one keyed request); evidence=%s", n, evidence)
	}
	if n := count(`SELECT count(*) FROM customers.consent_events WHERE owner_id=$1 AND source='merchant_recorded'`, mainOwner); n != 1 {
		t.Errorf("CB11: the merchant recorded %d consent rows for the main customer (the ads consent must stay)", n)
	}
	if !cbxAllows(t, e.f, tenant, store, mainOwner, "ads_personalization", "meta_ads") || cbxAllows(t, e.f, tenant, store, mainOwner, "marketing_messages", "meta_dm") {
		t.Errorf("CB11: consent_allows after the withdrawal: ads must stay granted, marketing withdrawn")
	}
	if n := count(`SELECT count(*) FROM customers.privacy_actions WHERE owner_id=$1 AND kind='EXPORT' AND via='merchant'`, mainOwner); n != 1 {
		t.Errorf("CB11: %d EXPORT rows of the main customer, want 1", n)
	}
	if n := count(`SELECT count(*) FROM customers.privacy_actions WHERE owner_id=$1 AND kind='ERASURE'`, mainOwner); n != 0 {
		t.Errorf("CB11: the main customer was erased although a payment session was open (%d ERASURE rows)", n)
	}
	var via string
	var active bool
	if err := e.f.owner.QueryRow(ctx, `SELECT a.via,o.active FROM customers.privacy_actions a JOIN buyer.owners o ON o.id=a.owner_id WHERE a.owner_id=$1 AND a.kind='ERASURE'`, erasable).Scan(&via, &active); err != nil || via != "merchant" || active {
		t.Errorf("CB11: erasable customer via=%q active=%v err=%v; evidence=%s", via, active, err, evidence)
	}
	if n := count(`SELECT count(*) FROM buyer.capability_sessions WHERE owner_id=$1 AND revoked_at IS NULL`, erasable); n != 0 {
		t.Errorf("CB11: %d live capability sessions remain after the erasure", n)
	}
	for action, min := range map[string]int{"customers.consent_withdrawn": 1, "customers.exported": 1, "customers.erased": 1, "finance.exported": 1} {
		if n := count(`SELECT count(*) FROM ops.audit_events WHERE store_id=$1 AND action=$2 AND principal_id=$3`, store, action, principal); n < min {
			t.Errorf("CB11: audit %s missing (%d)", action, n)
		}
	}
	created := fake.CallsTo("POST", "/v1/checkout/sessions")
	if len(created) != 1 || created[0].IdempotencyKey != "" || len(fake.CallsTo("POST", "/v1/billing_portal/sessions")) != 1 {
		t.Errorf("CB11: billing calls at Stripe: %d checkout creates (want 1, no key), %d portal sessions (want 1)", len(created), len(fake.CallsTo("POST", "/v1/billing_portal/sessions")))
	}
	if n := count(`SELECT count(*) FROM live.claim_windows WHERE session_id=$1 AND state='OPEN'`, studioSession); n != 0 {
		t.Errorf("CB11: the Studio window is left open")
	}
	cbbrShots(t, evidence, 20, "en", "zh-TW")
	t.Logf("CB11 BROWSER (MOCK Stripe): admin half passed; screenshots hashed; evidence=%s", evidence)

	// ---- buyer half ----
	e.ensureStock(t, main)
	settingsBefore := count(`SELECT count(*) FROM customers.consent_events WHERE store_id=$1 AND source='buyer_settings'`, store)
	cbbrBuyerNode(t, ctx, e, main, evidence, map[string]string{})
	var res struct{ Order1, Order2, Order3 string }
	raw, err := os.ReadFile(filepath.Join(evidence, "privacy-buyer-result.json"))
	if err != nil || json.Unmarshal(raw, &res) != nil || res.Order1 == "" || res.Order2 == "" || res.Order3 == "" {
		t.Fatalf("CB11 buyer: result file unreadable: %v", err)
	}
	ownerOf := func(order string) string {
		return cbxOne(t, e.f, `SELECT owner_id::text FROM checkout.orders WHERE id=$1`, order)
	}
	g1, g2, g3 := ownerOf(res.Order1), ownerOf(res.Order2), ownerOf(res.Order3)
	if g1 == "" || g2 == "" || g3 == "" || g1 == g2 || g2 == g3 {
		t.Fatalf("CB11 buyer: guest owners %q %q %q", g1, g2, g3)
	}
	consentRows := func(owner string) int {
		return count(`SELECT count(*) FROM customers.consent_events WHERE owner_id=$1`, owner)
	}
	if n := count(`SELECT count(*) FROM customers.consent_events WHERE owner_id=$1 AND purpose='marketing_messages' AND granted AND source='buyer_checkout' AND policy_version=$2`, g1, customers.PrivacyPolicyVersion); n != 1 || consentRows(g1) != 1 {
		t.Errorf("CB11 buyer: guest 1 has %d matching / %d total consent rows, want exactly one server-set buyer_checkout grant of marketing_messages", n, consentRows(g1))
	}
	if consentRows(g2) != 0 || consentRows(g3) != 0 {
		t.Errorf("CB11 buyer: guest 2 has %d and guest 3 has %d consent rows, want 0 (nothing ticked / requests blocked)", consentRows(g2), consentRows(g3))
	}
	for _, g := range []string{g1, g2, g3} {
		if n := count(`SELECT count(*) FROM checkout.orders WHERE owner_id=$1`, g); n != 1 {
			t.Errorf("CB11 buyer: owner %s has %d orders, want 1 (the order is never blocked or undone by consent)", g, n)
		}
	}
	if n := count(`SELECT count(*) FROM customers.privacy_actions WHERE store_id=$1 AND kind='ERASURE' AND via='buyer'`, store); n != 1 {
		t.Errorf("CB11 buyer: %d buyer erasures in the store, want exactly 1 (the privacy-page guest; the guest with an open order was refused)", n)
	}
	if n := count(`SELECT count(*) FROM customers.privacy_actions WHERE owner_id=$1 AND kind='ERASURE'`, g1); n != 0 {
		t.Errorf("CB11 buyer: guest 1 was erased although an order was open")
	}
	if n := count(`SELECT count(*) FROM customers.privacy_actions WHERE store_id=$1 AND kind='EXPORT' AND via='buyer'`, store); n != 1 {
		t.Errorf("CB11 buyer: %d buyer exports, want 1", n)
	}
	if n := count(`SELECT count(*) FROM customers.consent_events WHERE store_id=$1 AND source='buyer_settings'`, store) - settingsBefore; n != 3 {
		t.Errorf("CB11 buyer: %d privacy-page consent rows, want exactly the 3 toggles (marketing on, ads on, ads off)", n)
	}
	cbbrShots(t, evidence, 28, "en", "zh-TW")
	t.Logf("CB11 BROWSER buyer half passed; evidence=%s", evidence)
}

// cbbrBuyerNode runs tests/storefront/privacy-buyer.mjs against the production storefront Next build and the real
// buyerhttp handler of the store of o (the same sealed-cookie / TLS-edge technique as the refund and shipment gates).
func cbbrBuyerNode(t *testing.T, ctx context.Context, e *rfxEnv, o rfxOrder, evidence string, env map[string]string) {
	t.Helper()
	root, _ := filepath.Abs("../..")
	const storefrontOrigin = "https://buyer.example"
	if countRows(t, e.f.owner, `SELECT count(*) FROM control.storefront_publications WHERE tenant_id=$1 AND store_id=$2`, o.s.p.f.tenantA, o.store()) == 0 {
		bhPublish(t, o.s.p.bcHarness, storefrontOrigin, o.s.p.f.tenantA, o.store())
	}
	bffKey := base64.RawURLEncoding.EncodeToString(randomBytes(32))
	handler, err := buyerhttp.New(ctx, o.s.p.a.issuer, o.s.p.a.runtime, o.s.p.bcHarness.service, bffKey, time.Hour, e.svc)
	if err != nil {
		t.Fatalf("buyer HTTP constructor: %v", err)
	}
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	mustExec(t, e.f.owner, `UPDATE catalog.products SET name='Synthetic browser privacy product',description='Synthetic acceptance fixture' WHERE id=$1`, o.s.p.stock.product.ID)
	values := map[string]string{"COMMERCE_BUYER_WEB_ENABLED": "1", "COMMERCE_BUYER_API_ORIGIN": server.URL, "COMMERCE_BUYER_DEMO_LABEL": "1", "COMMERCE_BUYER_BFF_KEY": bffKey,
		"COMMERCE_BUYER_COOKIE_KEY": base64.RawURLEncoding.EncodeToString(randomBytes(32)), "COMMERCE_BUYER_SESSION_TTL": "3600",
		"LC_PB_EVIDENCE": evidence, "LC_PB_PRODUCT": o.s.p.stock.product.ID, "LC_PB_ORIGIN": storefrontOrigin, "LC_PB_POLICY": customers.PrivacyPolicyVersion}
	for k, v := range env {
		values[k] = v
	}
	cmd := exec.CommandContext(ctx, "node", "tests/storefront/privacy-buyer.mjs")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	defer func() {
		if cmd.Process != nil {
			_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		}
	}()
	cmd.Dir = root
	cmd.Env = browserEnvironment(values)
	log := browserLog(t, filepath.Join(evidence, "privacy-buyer.mjs.log"))
	cmd.Stdout, cmd.Stderr = log, log
	if err := cmd.Run(); err != nil {
		t.Fatalf("storefront browser gate failed: %v; evidence=%s", err, evidence)
	}
}

var _ = storefront.Item{}
