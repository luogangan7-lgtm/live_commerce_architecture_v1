//go:build browser

package foundation_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"livecommerce/internal/httpapi"
	"livecommerce/internal/identity"
	"livecommerce/internal/identityhttp"
	"livecommerce/internal/oidclogin"
	"livecommerce/internal/storefront"
)

// MBT04 uses signed browser login, the real Next route, Go HTTP, and disposable PG.
// The only mock is the external OIDC issuer; checkout orders come from Begin.
func TestBrowserMerchantOrdersBFFRealChain(t *testing.T) {
	if os.Getenv("LC_BROWSER_MERCHANT_ORDERS_BFF_ACCEPTANCE") != "1" || os.Getenv("LC_TEST_DATABASE_ALLOWED") != "1" {
		t.Fatal("use scripts/dev/test-local.sh --browser-merchant-orders-bff")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 160*time.Second)
	defer cancel()
	q := pqSetup(t)
	moGrant(t, q.f, q.f.tenantA, q.f.storeA1, q.f.principalA)
	secondBuyer := mustIssue(t, q.cqHarness.service, q.f.storeA1)
	secondHarness := q.bcHarness
	secondHarness.prepare(t, secondBuyer, []storefront.Item{{SKUID: q.stock.skus[0].ID, Quantity: 1}})
	second, err := secondHarness.begin(t04Key("mbt-second-order"))
	if err != nil {
		t.Fatal(err)
	}
	foreignStore, foreignOrder := moBeginInOtherStore(t, q.f, true)
	for _, permission := range []string{"store:read", "orders:read"} {
		mustExec(t, q.f.owner, `INSERT INTO identity.store_grants(tenant_id,store_id,principal_id,permission)
			VALUES($1,$2,$3,$4)`, q.f.tenantA, foreignStore, q.f.principalA, permission)
	}
	noOrdersPrincipal, noOrdersToken := randomUUID(), randomToken()
	expiredToken, revokedToken := randomToken(), randomToken()
	mustExec(t, q.f.owner, `INSERT INTO identity.principals(id) VALUES($1)`, noOrdersPrincipal)
	mustExec(t, q.f.owner, `INSERT INTO identity.memberships(tenant_id,principal_id) VALUES($1,$2)`, q.f.tenantA, noOrdersPrincipal)
	mustExec(t, q.f.owner, `INSERT INTO identity.store_grants(tenant_id,store_id,principal_id,permission)
		VALUES($1,$2,$3,'store:read')`, q.f.tenantA, q.f.storeA1, noOrdersPrincipal)
	tx, err := q.f.owner.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	if err := insertSession(ctx, tx, noOrdersToken, noOrdersPrincipal, "merchant", now.Add(time.Hour), nil); err != nil {
		_ = tx.Rollback(ctx)
		t.Fatal(err)
	}
	if err := insertSession(ctx, tx, expiredToken, q.f.principalA, "merchant", now.Add(-time.Minute), nil); err != nil {
		_ = tx.Rollback(ctx)
		t.Fatal(err)
	}
	if err := insertSession(ctx, tx, revokedToken, q.f.principalA, "merchant", now.Add(time.Hour), now); err != nil {
		_ = tx.Rollback(ctx)
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	uiAddress := listener.Addr().String()
	_ = listener.Close()
	_, uiPort, _ := net.SplitHostPort(uiAddress)
	publicOrigin := "http://" + uiAddress
	idp := newBrowserIDP(t, publicOrigin+"/api/auth/callback")
	// Attach the signed mock subject to the existing merchant fixture principal.
	// The login itself still issues a new session through the real identity API.
	mustExec(t, q.f.owner, `INSERT INTO identity.external_identities(issuer,subject,principal_id)
		VALUES($1,'browser-subject',$2)`, idp.server.URL, q.f.principalA)
	_, _, authority := identityFixture(t)
	provider, err := oidclogin.New(ctx, oidclogin.Config{
		Issuer: idp.server.URL, ClientID: browserClientID,
		RedirectURL: idp.redirect, AllowLoopbackForTests: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	service, err := identity.New(authority, observedBrowserProvider{Provider: provider, t: t}, identity.Policy{
		ProviderKey: "browser-orders-signed-mock-v1", SessionTTL: time.Hour,
		OnboardingEnabled: true, Currencies: []string{"TWD", "USD"},
	})
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
	mux.Handle("/", httpapi.NewHandler(q.f.runtime, httpapi.Options{SessionStoreList: true}))
	var orderCalls, strippedFailures atomic.Int64
	var lastURI, lastMethod atomic.Value
	lastURI.Store("")
	lastMethod.Store("")
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/__test/order-observation" {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{"count": orderCalls.Load(), "last_method": lastMethod.Load(), "last_uri": lastURI.Load()})
			return
		}
		if strings.HasPrefix(r.URL.Path, "/v1/admin/stores/") && strings.Contains(r.URL.Path, "/orders") {
			orderCalls.Add(1)
			lastMethod.Store(r.Method)
			lastURI.Store(r.RequestURI)
			if r.Header.Get("Cookie") != "" || r.Header.Get("X-Tenant-ID") != "" ||
				r.Header.Get("X-BFF-Test") != "" || r.Header.Get("X-Forwarded-Host") != "" ||
				r.Header.Get("Authorization") == "Bearer "+q.f.tokens["a"] ||
				r.Header.Get("Authorization") == "Bearer browser-bogus-token" {
				strippedFailures.Add(1)
			}
			if r.URL.RawQuery == "state=CANCELLED" {
				w.Header().Set("X-Backend-Secret", "backend-secret")
				w.Header().Set("Set-Cookie", "upstream=backend-secret")
				w.Header().Set("Cache-Control", "public")
				w.WriteHeader(http.StatusServiceUnavailable)
				_, _ = w.Write([]byte(`{"code":"retry_later","message":"SQLSTATE backend-secret"}`))
				return
			}
		}
		mux.ServeHTTP(w, r)
	}))
	t.Cleanup(api.Close)

	tables := []string{"checkout.orders", "checkout.payment_attempts", "payments.facts", "inventory.ledger", "checkout.command_results", "checkout.events", "fulfillment.payment_work_items", "river.river_job", "river_payment.river_job", "river_expiry.river_job"}
	before := map[string]int{}
	for _, table := range tables {
		before[table] = countRows(t, q.f.owner, "SELECT count(*) FROM "+table)
	}
	var orderHash string
	if err := q.f.owner.QueryRow(ctx, `SELECT md5(o::text) FROM checkout.orders o WHERE id=$1`, q.hold.OrderID).Scan(&orderHash); err != nil {
		t.Fatal(err)
	}

	evidence := filepath.Join(root, "output", "playwright", "merchant-orders-bff-"+time.Now().UTC().Format("20060102T150405.000000000"))
	if err := os.MkdirAll(evidence, 0700); err != nil {
		t.Fatal(err)
	}
	// Prove the fixture actually works on a second, local Next process, then
	// require orders to remain hidden when real identity transport is disabled.
	fixtureListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	fixtureAddress := fixtureListener.Addr().String()
	_ = fixtureListener.Close()
	_, fixturePort, _ := net.SplitHostPort(fixtureAddress)
	nextEnvPath := filepath.Join(root, "apps/admin/next-env.d.ts")
	nextEnvBefore, err := os.ReadFile(nextEnvPath)
	if err != nil {
		t.Fatal(err)
	}
	devNextEnv := bytes.ReplaceAll(nextEnvBefore, []byte("./.next/types/"), []byte("./.next/dev/types/"))
	t.Cleanup(func() {
		current, err := os.ReadFile(nextEnvPath)
		if err != nil {
			t.Error("could not read Next generated type file during cleanup")
			return
		}
		if bytes.Equal(current, nextEnvBefore) {
			return
		}
		if !bytes.Equal(current, devNextEnv) {
			t.Error("Next type file changed outside the expected dev rewrite; preserving it for review")
			return
		}
		if err := os.WriteFile(nextEnvPath, nextEnvBefore, 0644); err != nil {
			t.Error("could not restore Next generated type file")
		}
	})
	fixtureLog := browserLog(t, filepath.Join(evidence, "fixture-next.log"))
	fixtureNext := exec.CommandContext(ctx, "node", filepath.Join(root, "apps/admin/node_modules/next/dist/bin/next"), "dev", "--hostname", "127.0.0.1", "--port", fixturePort)
	fixtureNext.Dir = filepath.Join(root, "apps/admin")
	fixtureNext.Env = browserEnvironment(map[string]string{
		"NODE_ENV": "development", "COMMERCE_IDENTITY_ENABLED": "0",
		"COMMERCE_FIXTURE_ENABLED": "1", "COMMERCE_FIXTURE_ALLOWED": "1",
		"COMMERCE_FIXTURE_TOKEN": q.f.tokens["a"], "COMMERCE_FIXTURE_STORE_ID": q.f.storeA1,
		"COMMERCE_API_ORIGIN": api.URL,
	})
	fixtureNext.Stdout, fixtureNext.Stderr = fixtureLog, fixtureLog
	// F6: `next dev` forks a next-server child that outlives its parent and holds the port; own a process
	// group and signal the whole group (this PID's group only, never by port or name).
	fixtureNext.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	fixtureNext.Cancel = func() error { return syscall.Kill(-fixtureNext.Process.Pid, syscall.SIGKILL) }
	if err := fixtureNext.Start(); err != nil {
		t.Fatal("could not start local fixture Next")
	}
	fixtureDone := make(chan error, 1)
	go func() { fixtureDone <- fixtureNext.Wait() }()
	var stopFixtureOnce sync.Once
	stopFixture := func() {
		stopFixtureOnce.Do(func() {
			_ = syscall.Kill(-fixtureNext.Process.Pid, syscall.SIGINT)
			select {
			case <-fixtureDone:
			case <-time.After(5 * time.Second):
				_ = syscall.Kill(-fixtureNext.Process.Pid, syscall.SIGKILL)
				<-fixtureDone
			}
			stopProcessGroup(t, fixtureNext.Process.Pid) // leaked next-server children hold the port
		})
	}
	t.Cleanup(stopFixture)
	fixtureClient := &http.Client{Timeout: 2 * time.Second}
	fixtureRead := func(resource string) (*http.Response, error) {
		req, err := http.NewRequestWithContext(ctx, "GET", "http://"+fixtureAddress+"/api/stores/"+q.f.storeA1+"/"+resource, nil)
		if err != nil {
			return nil, err
		}
		req.Host = "127.0.0.1:3100" // The existing fixture adapter has this exact loopback Host guard.
		return fixtureClient.Do(req)
	}
	fixtureReady := false
	for attempt := 0; attempt < 100; attempt++ {
		response, err := fixtureRead("warehouses")
		if err == nil {
			_ = response.Body.Close()
			fixtureReady = response.StatusCode == http.StatusOK
		}
		if fixtureReady {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatal("fixture Next readiness deadline")
		case <-time.After(100 * time.Millisecond):
		}
	}
	if !fixtureReady {
		t.Fatalf("fixture Next did not expose warehouses; local evidence: %s", evidence)
	}
	beforeFixtureOrders := orderCalls.Load()
	fixtureOrder, err := fixtureRead("orders")
	if err != nil {
		t.Fatal("fixture-only orders request failed")
	}
	_ = fixtureOrder.Body.Close()
	if fixtureOrder.StatusCode != http.StatusNotFound || fixtureOrder.Header.Get("Cache-Control") != "no-store" || orderCalls.Load() != beforeFixtureOrders {
		t.Fatalf("fixture-only orders not isolated: status=%d upstream_delta=%d evidence=%s", fixtureOrder.StatusCode, orderCalls.Load()-beforeFixtureOrders, evidence)
	}
	stopFixture()
	serverLog := browserLog(t, filepath.Join(evidence, "next.log"))
	server := exec.CommandContext(ctx, "node", filepath.Join(root, "apps/admin/.next/standalone/apps/admin/server.js"))
	server.Dir = root
	server.Env = browserEnvironment(map[string]string{
		"HOSTNAME": "127.0.0.1", "PORT": uiPort, "NODE_ENV": "production",
		"COMMERCE_IDENTITY_ENABLED": "1", "COMMERCE_IDENTITY_ALLOW_LOOPBACK_TESTS": "1",
		"COMMERCE_PUBLIC_ORIGIN": publicOrigin, "COMMERCE_API_ORIGIN": api.URL,
		"COMMERCE_OIDC_ISSUER": idp.server.URL, "COMMERCE_BFF_KEY": bffKey,
		"COMMERCE_ONBOARDING_ENABLED": "1", "COMMERCE_ONBOARDING_CURRENCIES": "TWD,USD",
	})
	server.Stdout, server.Stderr = serverLog, serverLog
	if err := server.Start(); err != nil {
		t.Fatal("could not start packaged Next server")
	}
	serverDone := make(chan error, 1)
	go func() { serverDone <- server.Wait() }()
	t.Cleanup(func() {
		_ = server.Process.Kill()
		select {
		case <-serverDone:
		case <-time.After(5 * time.Second):
			t.Error("owned Next process did not stop")
		}
	})
	client := &http.Client{Timeout: time.Second}
	ready := false
	for attempt := 0; attempt < 100; attempt++ {
		response, err := client.Get(publicOrigin + "/api/stores")
		if err == nil {
			_ = response.Body.Close()
			ready = response.StatusCode == http.StatusUnauthorized
		}
		if ready {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatal("Next readiness deadline")
		case <-time.After(100 * time.Millisecond):
		}
	}
	if !ready {
		t.Fatalf("Next readiness failed; local evidence: %s", evidence)
	}
	browserLogFile := browserLog(t, filepath.Join(evidence, "playwright.log"))
	browser := exec.CommandContext(ctx, "pnpm", "exec", "playwright", "test", "tests/admin/orders-bff.spec.ts", "--reporter=list", "--output="+filepath.Join(evidence, "results"))
	browser.Dir = root
	browser.Env = browserEnvironment(map[string]string{
		"LC_BROWSER_SUITE": "merchant-orders-bff", "LC_BROWSER_PUBLIC_ORIGIN": publicOrigin,
		"LC_BROWSER_API_ORIGIN": api.URL, "LC_BROWSER_ORDER_STORE": q.f.storeA1,
		"LC_BROWSER_ORDER_ID": q.hold.OrderID, "LC_BROWSER_SECOND_ORDER_ID": second.OrderID,
		"LC_BROWSER_FOREIGN_STORE":    foreignStore,
		"LC_BROWSER_FOREIGN_ORDER_ID": foreignOrder, "LC_BROWSER_UNLISTED_STORE": q.f.storeB,
		"LC_BROWSER_NO_ORDERS_TOKEN": noOrdersToken,
		"LC_BROWSER_EXPIRED_TOKEN":   expiredToken, "LC_BROWSER_REVOKED_TOKEN": revokedToken,
	})
	browser.Stdout, browser.Stderr = browserLogFile, browserLogFile
	if err := browser.Run(); err != nil {
		t.Fatalf("merchant orders browser chain failed; local evidence: %s", evidence)
	}
	if strippedFailures.Load() != 0 || orderCalls.Load() < 5 {
		t.Fatalf("authority stripping/order read proof failed: stripped=%d calls=%d evidence=%s", strippedFailures.Load(), orderCalls.Load(), evidence)
	}
	var issued int
	if err := q.f.owner.QueryRow(ctx, `SELECT count(*) FROM identity.sessions s
		JOIN identity.session_events ev ON ev.session_id=s.id AND ev.action='session.issued'
		JOIN identity.external_identities e ON e.principal_id=s.principal_id
		WHERE e.issuer=$1 AND e.subject='browser-subject' AND s.token_hash<>$2`, idp.server.URL, tokenHash(q.f.tokens["a"])).Scan(&issued); err != nil || issued != 1 {
		t.Fatalf("signed browser session persistence proof: count=%d err=%v", issued, err)
	}
	for _, table := range tables {
		if after := countRows(t, q.f.owner, "SELECT count(*) FROM "+table); after != before[table] {
			t.Fatalf("read mutated %s: %d -> %d", table, before[table], after)
		}
	}
	var afterHash string
	if err := q.f.owner.QueryRow(ctx, `SELECT md5(o::text) FROM checkout.orders o WHERE id=$1`, q.hold.OrderID).Scan(&afterHash); err != nil || afterHash != orderHash {
		t.Fatalf("read changed persisted order: match=%t err=%v", afterHash == orderHash, err)
	}
	t.Logf("PASS: signed browser cookie -> Next -> Go -> disposable PG; evidence=%s", evidence)
}
