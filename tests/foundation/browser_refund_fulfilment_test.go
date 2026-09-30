//go:build browser

package foundation_test

// MF07 and RF11 (contracts/manual-fulfilment-v1.md §6, contracts/stripe-refund-v1.md §9): real browsers.
//
// Stack per test: an isolated PG 18 container (rfx harness), the real payment worker + independent Stripe fake
// (orders are paid through the real capture path, labeled MOCK), the real private Go API (identity + admin
// handlers incl. refunds/shipments) behind the production admin Next build with a signed mock OIDC issuer, and,
// for the buyer half, the real buyerhttp handler behind the production storefront Next build.
//
// Evidence labels: RF11 (a) BROWSER(MOCK): refunds go through the fake; RF11 (b) BROWSER(SANDBOX) needs the SP18
// harness of stripe-b2-browser-tests (not merged) plus STRIPE_BROWSER=1 and STRIPE_SANDBOX=1, otherwise NOT_RUN.
// Playwright runs with a config generated into the evidence directory (playwright.config.ts is not this unit's
// path); screenshots (desktop 1586x992 and 390 px, three locales) are hashed into screenshots.json.
//
// NOT_RUN until the UI unit lands: the admin refund/shipment sections and the storefront refund/shipment display
// do not exist in this base. These gates are authored from the contracts and the UI brief and compile; they cannot
// pass until refund-fulfilment-ui merges (recorded, not hidden).

import (
	"context"
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
	"livecommerce/internal/buyerhttp"
	"livecommerce/internal/httpapi"
	"livecommerce/internal/identity"
	"livecommerce/internal/identityhttp"
	"livecommerce/internal/oidclogin"
	"livecommerce/internal/platform"
)

type brfStack struct {
	e        *rfxEnv
	root     string
	evidence string
	origin   string
	api      *httptest.Server
	env      map[string]string // shared browser environment for the admin app
}

func brfRequire(t *testing.T) {
	t.Helper()
	if os.Getenv("LC_BROWSER_REFUND_FULFILMENT_ACCEPTANCE") != "1" || os.Getenv("LC_TEST_DATABASE_ALLOWED") != "1" {
		t.Fatal("use scripts/dev/test-local.sh --browser-refund-fulfilment")
	}
}

// brfStartAdmin starts the private Go API (identity + admin routes over the ISOLATED database) and the production
// admin Next build against it. principal is the store creator that the mock IdP subject maps to.
func brfStartAdmin(t *testing.T, ctx context.Context, e *rfxEnv, principal, evidence string) *brfStack {
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
	role := "brf_" + strings.ReplaceAll(randomUUID(), "-", "")
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
	service, err := identity.New(authority, observedBrowserProvider{Provider: provider, t: t}, identity.Policy{ProviderKey: "browser-refund-fulfilment-v1", SessionTTL: time.Hour, OnboardingEnabled: true, Currencies: []string{"TWD", "USD"}})
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
	mux.Handle("/", httpapi.NewHandler(e.f.runtime, httpapi.Options{SessionStoreList: true, RefundJobs: e.jobs}))
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

// brfPlaywright runs the given admin specs with a config generated next to the evidence (inside the repo so that
// @playwright/test resolves).
func brfPlaywright(t *testing.T, ctx context.Context, s *brfStack, specs []string, env map[string]string) {
	t.Helper()
	config := filepath.Join(s.evidence, "playwright.rf.config.ts")
	body := fmt.Sprintf(`import { defineConfig, devices } from "@playwright/test";
// LC_BROWSER_ENGINE=webkit: Desktop Safari profile for the admin pages (forwarded by browserEnvironment); chromium otherwise.
const safari = process.env.LC_BROWSER_ENGINE === "webkit" ? { ...devices["Desktop Safari"], ignoreHTTPSErrors: true } : {};
export default defineConfig({
  testDir: %q,
  testMatch: %s,
  fullyParallel: false, workers: 1, retries: 0, timeout: 180000,
  expect: { timeout: 10000 },
  reporter: [["list"]],
  outputDir: %q,
  use: { ...safari, baseURL: %q, headless: true, viewport: { width: 1586, height: 992 }, trace: "retain-on-failure", screenshot: "only-on-failure" },
});
`, filepath.Join(s.root, "tests/admin"), mustJSON(t, specs), filepath.Join(s.evidence, "results"), s.origin)
	if err := os.WriteFile(config, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	// browserLog is O_EXCL: a gate that runs Playwright once per phase (RF11 partial, full) needs one log per phase.
	logName := "playwright.log"
	if phase := env["LC_BROWSER_PHASE"]; phase != "" {
		logName = "playwright-" + phase + ".log"
	}
	log := browserLog(t, filepath.Join(s.evidence, logName))
	cmd := exec.CommandContext(ctx, "pnpm", "exec", "playwright", "test", "--config", config)
	cmd.Dir = s.root
	base := map[string]string{"LC_BROWSER_PUBLIC_ORIGIN": s.origin, "LC_BROWSER_API_ORIGIN": s.api.URL, "LC_BROWSER_EVIDENCE": s.evidence}
	for k, v := range env {
		base[k] = v
	}
	cmd.Env = browserEnvironment(base)
	cmd.Stdout, cmd.Stderr = log, log
	if err := cmd.Run(); err != nil {
		t.Fatalf("admin browser gate failed: %v; evidence=%s", err, s.evidence)
	}
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

// brfBuyerNode starts the real buyerhttp handler for one store and runs a storefront script against the
// production storefront Next build. The buyer capability token stays inside Go and Node's env: the script forges
// the sealed buyer cookie exactly as apps/storefront/lib/buyer-server.ts verifies it (HMAC over the envelope).
func brfBuyerNode(t *testing.T, ctx context.Context, e *rfxEnv, o rfxOrder, script, evidence string, env map[string]string) {
	t.Helper()
	root, _ := filepath.Abs("../..")
	const storefrontOrigin = "https://buyer.example"
	// RF11 runs the buyer script twice (processing, final) on one store: publish once.
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
	mustExec(t, e.f.owner, `UPDATE catalog.products SET name='Synthetic browser fulfilment product',description='Synthetic acceptance fixture' WHERE id=$1`, o.s.p.stock.product.ID)
	values := map[string]string{"COMMERCE_BUYER_WEB_ENABLED": "1", "COMMERCE_BUYER_API_ORIGIN": server.URL, "COMMERCE_BUYER_DEMO_LABEL": "1", "COMMERCE_BUYER_BFF_KEY": bffKey,
		"COMMERCE_BUYER_COOKIE_KEY": base64.RawURLEncoding.EncodeToString(randomBytes(32)), "COMMERCE_BUYER_SESSION_TTL": "3600",
		"LC_RF_EVIDENCE": evidence, "LC_RF_PRODUCT": o.s.p.stock.product.ID, "LC_RF_ORDER": o.order, "LC_RF_BUYER_TOKEN": o.s.p.cap.Token, "LC_RF_ORIGIN": storefrontOrigin}
	for k, v := range env {
		values[k] = v
	}
	cmd := exec.CommandContext(ctx, "node", script)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	defer func() {
		if cmd.Process != nil {
			_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		}
	}()
	cmd.Dir = root
	cmd.Env = browserEnvironment(values)
	logName := filepath.Base(script) + ".log"
	if phase := env["LC_RF_PHASE"]; phase != "" { // the RF11 buyer script runs twice (processing, final)
		logName = filepath.Base(script) + "-" + phase + ".log"
	}
	log := browserLog(t, filepath.Join(evidence, logName))
	cmd.Stdout, cmd.Stderr = log, log
	if err := cmd.Run(); err != nil {
		t.Fatalf("storefront browser gate %s failed: %v; evidence=%s", script, err, evidence)
	}
}

func brfEvidence(t *testing.T, root, name string) string {
	t.Helper()
	dir := filepath.Join(root, "output/playwright", name, time.Now().UTC().Format("20060102T150405.000000000"))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	return dir
}

// brfShots verifies the hashed screenshot manifest both halves append to.
func brfShots(t *testing.T, evidence string, min int) {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(evidence, "screenshots.json"))
	if err != nil {
		t.Fatalf("screenshot hash manifest missing: %v; evidence=%s", err, evidence)
	}
	var shots []struct {
		File, Sha256, Locale, Viewport string
	}
	if err := json.Unmarshal(raw, &shots); err != nil || len(shots) < min {
		t.Fatalf("screenshot manifest has %d entries (want >= %d): %v", len(shots), min, err)
	}
	locales, viewports := map[string]bool{}, map[string]bool{}
	for _, s := range shots {
		if len(s.Sha256) != 64 {
			t.Fatalf("screenshot %s is not hashed", s.File)
		}
		locales[s.Locale], viewports[s.Viewport] = true, true
	}
	for _, l := range []string{"en", "zh-CN", "zh-TW"} {
		if !locales[l] {
			t.Fatalf("no screenshot for locale %s", l)
		}
	}
	if !viewports["desktop"] || !viewports["mobile"] {
		t.Fatalf("screenshots lack a desktop or mobile viewport: %v", viewports)
	}
}

func TestBrowserManualFulfilment(t *testing.T) {
	brfRequire(t)
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Minute)
	defer cancel()
	e := rfxNew(t)
	e.startWorker(t)
	ship := e.payInStore(t, e.storeFor(t))
	reship := e.payMore(t, ship)
	e.ensureStock(t, ship)
	drafts := sstMoreHold(t, ship.s.p) // a DRAFT order: never eligible, must be absent from the export
	restricted, _ := e.member(t, ship, "orders:read")
	root, _ := filepath.Abs("../..")
	evidence := brfEvidence(t, root, "manual-fulfilment")
	stack := brfStartAdmin(t, ctx, e, ship.s.p.f.principalA, evidence)
	env := map[string]string{"LC_BROWSER_STORE": ship.store(), "LC_BROWSER_SHIP_ORDER": ship.order, "LC_BROWSER_RESHIP_ORDER": reship.order, "LC_BROWSER_DRAFT_ORDER": drafts.hold.OrderID,
		"LC_BROWSER_RESTRICTED_TOKEN": restricted, "LC_BROWSER_TRACKING": "0012345678"}
	brfPlaywright(t, ctx, stack, []string{"manual-fulfilment.spec.ts"}, env)
	// PG facts after the admin half: the UI drove real commands (record v1, correct v2, void v3, re-record v4 of `ship`).
	if n := e.mfxVersions(t, ship); n != 4 {
		t.Fatalf("MF07: order %s has %d shipment versions, want 4 (record, correct, void, re-record); evidence=%s", ship.order, n, evidence)
	}
	if got := e.mfxFulfilmentState(t, ship); got != "MERCHANT_SHIPPED" {
		t.Fatalf("MF07: final fulfilment state %s", got)
	}
	if n := e.count(t, `SELECT count(*) FROM integration.operations WHERE actor_kind NOT IN ('BUYER_PAYMENT_QUERY')`); n != 0 {
		t.Fatalf("MF07: %d provider operations besides the payment queries: a shipment must never create one", n)
	}
	if e.mfxAudit(t, ship, "orders.export_unshipped") < 1 {
		t.Fatalf("MF07: the export produced no audit row; evidence=%s", evidence)
	}
	var draftState string
	if err := e.f.owner.QueryRow(ctx, `SELECT fulfillment_state FROM checkout.orders WHERE id=$1`, drafts.hold.OrderID).Scan(&draftState); err != nil || draftState == "MERCHANT_SHIPPED" {
		t.Fatalf("MF07: the DRAFT order is %s (err %v): drafts are never shippable", draftState, err)
	}
	// Buyer half: the shipped order's buyer sees carrier, tracking and the link host, never "delivered".
	brfBuyerNode(t, ctx, e, ship, "tests/storefront/shipment-buyer.mjs", evidence, map[string]string{"LC_RF_TRACKING": "0012345678", "LC_RF_CARRIER": "7-ELEVEN", "LC_RF_URL_HOST": "track.example.com"})
	brfShots(t, evidence, 12)
	t.Logf("MF07 BROWSER (MOCK Stripe capture): admin + buyer halves passed; screenshots hashed; evidence=%s", evidence)
}

func TestBrowserRefund(t *testing.T) {
	brfRequire(t)
	t.Run("MOCK: refunds through the fake Stripe, real worker, real admin and storefront", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 14*time.Minute)
		defer cancel()
		e := rfxNew(t)
		e.startWorker(t)
		order := e.payInStore(t, e.storeFor(t))
		restricted, _ := e.member(t, order, "orders:read") // no payments:refund
		root, _ := filepath.Abs("../..")
		evidence := brfEvidence(t, root, "refund")
		// The buyer script settles the held-pending partial refund through this scalar control endpoint.
		control := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/settle" || r.Method != http.MethodPost {
				http.NotFound(w, r)
				return
			}
			for _, id := range e.fake.RefundIDs(order.pi) {
				e.fake.SetRefundStatus(id, "succeeded", "")
			}
			// The fake sends no webhook (R-8: webhook + merchant refresh only), so stand in for it the way
			// awaitRefund does: keep the attempt's refund jobs due until the SUCCEEDED fact is committed
			// (the observation and the apply are separate job runs). Bounded; the page assertion still decides.
			deadline := time.Now().Add(45 * time.Second)
			for {
				e.wakeAllRefunds(order)
				var done bool
				if err := e.f.owner.QueryRow(r.Context(), `SELECT EXISTS(SELECT 1 FROM payments.refund_facts f JOIN payments.stripe_refunds x ON x.id=f.refund_id
					WHERE x.attempt_id=$1::uuid AND f.kind='SUCCEEDED')`, order.attempt).Scan(&done); err == nil && done {
					break
				}
				if time.Now().After(deadline) {
					http.Error(w, "refund did not settle", http.StatusGatewayTimeout)
					return
				}
				time.Sleep(100 * time.Millisecond)
			}
			w.WriteHeader(http.StatusNoContent)
		}))
		t.Cleanup(control.Close)
		stack := brfStartAdmin(t, ctx, e, order.s.p.f.principalA, evidence)
		env := func(phase string) map[string]string {
			return map[string]string{"LC_BROWSER_STORE": order.store(), "LC_BROWSER_ORDER": order.order, "LC_BROWSER_RESTRICTED_TOKEN": restricted,
				"LC_BROWSER_CAPTURED": fmt.Sprint(order.captured), "LC_BROWSER_PARTIAL": "1000", "LC_BROWSER_PHASE": phase}
		}
		buyer := func(phase string) {
			brfBuyerNode(t, ctx, e, order, "tests/storefront/refund-buyer.mjs", evidence, map[string]string{"LC_RF_CONTROL": control.URL, "LC_RF_CAPTURED": fmt.Sprint(order.captured), "LC_RF_PARTIAL": "1000", "LC_RF_PHASE": phase})
		}
		// phase 1: the merchant refunds NT$10 (held pending at the provider); the buyer sees "processing", then it settles
		e.fake.HoldNextRefund("pending", "processing")
		brfPlaywright(t, ctx, stack, []string{"refund.spec.ts"}, env("partial"))
		buyer("processing")
		// phase 2: the merchant refunds the remainder; the buyer sees the order fully refunded
		brfPlaywright(t, ctx, stack, []string{"refund.spec.ts"}, env("full"))
		buyer("final")
		var succeeded int
		var refunded int64
		if err := e.f.owner.QueryRow(ctx, `SELECT count(*),coalesce(sum(r.amount_minor),0)::bigint FROM payments.refund_facts f JOIN payments.stripe_refunds r ON r.id=f.refund_id WHERE f.attempt_id=$1 AND f.kind='SUCCEEDED'`, order.attempt).Scan(&succeeded, &refunded); err != nil {
			t.Fatal(err)
		}
		if succeeded != 2 || refunded != order.captured {
			t.Fatalf("RF11: %d SUCCEEDED refunds totalling %d of %d (a partial then a full refund of the remainder were required); evidence=%s", succeeded, refunded, order.captured, evidence)
		}
		if got := len(e.fake.RefundIDs(order.pi)); got != 2 || e.count(t, `SELECT count(*) FROM checkout.orders WHERE id=$1 AND commercial_state='CONFIRMED' AND fulfillment_state='MANUAL_UNASSIGNED'`, order.order) != 1 {
			t.Fatalf("RF11: provider refunds=%d or the order state moved (a refund never changes fulfilment)", got)
		}
		if n := e.count(t, `SELECT count(*) FROM payments.stripe_refunds WHERE attempt_id=$1 AND principal_id<>$2`, order.attempt, order.s.p.f.principalA); n != 0 {
			t.Fatalf("RF11: %d refunds were requested by someone other than the permitted merchant (the restricted member must have no action)", n)
		}
		e.assertPreSendReads(t)
		brfShots(t, evidence, 12)
		t.Logf("RF11 BROWSER(MOCK): admin + buyer halves passed; screenshots hashed; evidence=%s", evidence)
	})
	t.Run("SANDBOX: real Stripe test-mode card 4242 through the SP18 harness", func(t *testing.T) {
		if os.Getenv("STRIPE_BROWSER") != "1" || os.Getenv("STRIPE_SANDBOX") != "1" {
			t.Skip("NOT_RUN: RF11(b) needs STRIPE_BROWSER=1 and STRIPE_SANDBOX=1 with the owner's test keys")
		}
		t.Skip("NOT_RUN: RF11(b) reuses the SP18 browser harness of the stripe-b2-browser-tests unit, which is not merged in this base; wire it here once it is")
	})
}
