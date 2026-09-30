//go:build browser

package foundation_test

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"livecommerce/internal/httpapi"
	"livecommerce/internal/identity"
	"livecommerce/internal/identityhttp"
	"livecommerce/internal/oidclogin"
)

// Both Next applications use production builds. Only the signed MOCK IdP,
// pre-authorized merchant, publication evidence and TLS/CONNECT edge are fixtures.
// Product/SKU creation and the purchase-entry projection are never mocked.
func TestBrowserMerchantBuyerRealChain(t *testing.T) {
	if os.Getenv("LC_BROWSER_MERCHANT_BUYER_ACCEPTANCE") != "1" || os.Getenv("LC_TEST_DATABASE_ALLOWED") != "1" {
		t.Fatal("use scripts/dev/test-local.sh --browser-merchant-buyer")
	}
	h := bhSetup(t)
	ctx, cancel := context.WithTimeout(context.Background(), 140*time.Second)
	defer cancel()
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	evidence, err := os.MkdirTemp(filepath.Join(root, "output/playwright"), "merchant-buyer-real-")
	if err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	adminOrigin := browserFront(t, listener.Addr().String()) // https TLS front under LC_BROWSER_ENGINE=webkit, else http://addr
	_, adminPort, _ := net.SplitHostPort(listener.Addr().String())
	_ = listener.Close()
	idp := newBrowserIDP(t, adminOrigin+"/api/auth/callback")
	_, _, authority := identityFixture(t)
	provider, err := oidclogin.New(ctx, oidclogin.Config{Issuer: idp.server.URL, ClientID: browserClientID, RedirectURL: idp.redirect, AllowLoopbackForTests: true})
	if err != nil {
		t.Fatal(err)
	}
	service, err := identity.New(authority, observedBrowserProvider{Provider: provider, t: t}, identity.Policy{ProviderKey: "browser-merchant-buyer-mock-v1", SessionTTL: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	// A dedicated fixture principal can see only this store. Production session
	// issuance, membership checks and catalog:write checks remain in the path.
	principal := randomUUID()
	mustExec(t, h.f.owner, `INSERT INTO identity.principals(id) VALUES($1)`, principal)
	mustExec(t, h.f.owner, `INSERT INTO identity.memberships(tenant_id,principal_id) VALUES($1,$2)`, h.f.tenantA, principal)
	mustExec(t, h.f.owner, `INSERT INTO identity.external_identities(issuer,subject,principal_id) VALUES($1,'browser-subject',$2)`, idp.server.URL, principal)
	mustExec(t, h.f.owner, `INSERT INTO identity.store_grants(tenant_id,store_id,principal_id,permission)
		SELECT $1,$2,$3,p FROM unnest(ARRAY['store:read','catalog:read','catalog:write','inventory:read']) p`, h.f.tenantA, h.f.storeA1, principal)
	_, foreignProduct := bcatForeignStore(t, h, h.f.tenantB)
	adminKey := randomToken()
	private, err := identityhttp.NewHandler(service, adminKey)
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	mux.Handle("/v1/identity/", private)
	mux.Handle("/", httpapi.NewHandler(h.f.runtime, httpapi.Options{SessionStoreList: true}))
	var productWrites, skuWrites atomic.Int32
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "POST" && r.URL.Path == "/v1/admin/stores/"+h.f.storeA1+"/products" {
			productWrites.Add(1)
		}
		if r.Method == "POST" && r.URL.Path == "/v1/admin/stores/"+h.f.storeA1+"/skus" {
			skuWrites.Add(1)
		}
		mux.ServeHTTP(w, r)
	}))
	t.Cleanup(api.Close)
	// Privileged test controls stay on an ephemeral loopback listener and require
	// a random runner-only key. They are not application routes or browser APIs.
	tables := []string{"buyer.owners", "buyer.capability_sessions", "buyer.capability_events", "buyer.command_results", "storefront.carts", "storefront.cart_lines", "storefront.quotes", "storefront.events", "checkout.orders", "checkout.command_results", "checkout.payment_attempts", "integration.operations", "integration.operation_events", "inventory.reservations", "inventory.ledger", "ops.command_results"}
	facts := func() map[string]int {
		out := make(map[string]int, len(tables))
		for _, table := range tables {
			out[table] = countRows(t, h.f.owner, "SELECT count(*) FROM "+table)
		}
		return out
	}
	before := facts()
	controlKey := randomToken()
	control := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Gate-Key") != controlKey {
			http.Error(w, "forbidden", 403)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		switch {
		case r.Method == "GET" && r.URL.Path == "/facts":
			_ = json.NewEncoder(w).Encode(facts())
		case r.Method == "POST" && r.URL.Path == "/unpublish":
			_, err := h.f.owner.Exec(r.Context(), `UPDATE control.storefront_publications SET published=false WHERE tenant_id=$1 AND store_id=$2`, h.f.tenantA, h.f.storeA1)
			if err != nil {
				http.Error(w, "fixture mutation failed", 500)
				return
			}
			_, _ = w.Write([]byte(`{"unpublished":true}`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(control.Close)
	cmd := exec.CommandContext(ctx, "node", "tests/storefront/merchant-buyer-gate.mjs")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	defer func() {
		if cmd.Process != nil {
			_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		}
	}()
	cmd.Dir = root
	cmd.Env = browserEnvironment(map[string]string{
		"COMMERCE_IDENTITY_ENABLED": "1", "COMMERCE_IDENTITY_ALLOW_LOOPBACK_TESTS": "1",
		"COMMERCE_PUBLIC_ORIGIN": adminOrigin, "COMMERCE_API_ORIGIN": api.URL,
		"COMMERCE_OIDC_ISSUER": idp.server.URL, "COMMERCE_BFF_KEY": adminKey,
		"COMMERCE_BUYER_WEB_ENABLED": "1", "COMMERCE_BUYER_DEMO_LABEL": "1",
		"COMMERCE_BUYER_API_ORIGIN": h.server.URL, "COMMERCE_BUYER_BFF_KEY": h.key,
		"COMMERCE_BUYER_COOKIE_KEY": brToken(), "COMMERCE_BUYER_SESSION_TTL": "3600",
		"LC_JOINT_EVIDENCE": evidence, "LC_JOINT_ADMIN_PORT": adminPort,
		"LC_JOINT_STORE": h.f.storeA1, "LC_JOINT_FOREIGN_PRODUCT": foreignProduct,
		"LC_JOINT_CONTROL": control.URL, "LC_JOINT_CONTROL_KEY": controlKey,
	})
	log := browserLog(t, filepath.Join(evidence, "browser.log"))
	cmd.Stdout, cmd.Stderr = log, log
	if err := cmd.Run(); err != nil {
		t.Fatalf("merchant/buyer browser gate failed; evidence=%s", evidence)
	}
	data, err := os.ReadFile(filepath.Join(evidence, "result.json"))
	if err != nil {
		t.Fatal(err)
	}
	var result struct {
		ProductID string   `json:"product_id"`
		SKUID     string   `json:"sku_id"`
		Name      string   `json:"name"`
		Code      string   `json:"code"`
		Locales   []string `json:"locales"`
		Cases     int      `json:"cases"`
	}
	if json.Unmarshal(data, &result) != nil || result.Cases != 8 || !reflect.DeepEqual(result.Locales, []string{"en", "zh-CN", "zh-TW"}) {
		t.Fatal("missing exact joint browser gate results")
	}
	var product, store, tenant, name, code, currency string
	var price int64
	err = h.f.owner.QueryRow(ctx, `SELECT p.id::text,p.store_id::text,p.tenant_id::text,p.name,s.code,s.currency,s.price_minor
		FROM catalog.products p JOIN catalog.skus s ON s.product_id=p.id AND s.store_id=p.store_id AND s.tenant_id=p.tenant_id
		WHERE p.id=$1 AND s.id=$2`, result.ProductID, result.SKUID).Scan(&product, &store, &tenant, &name, &code, &currency, &price)
	if err != nil || product != result.ProductID || store != h.f.storeA1 || tenant != h.f.tenantA || name != result.Name || code != result.Code || currency != h.stock.skus[0].Currency || price != 12345 {
		t.Fatal("independent PostgreSQL product/SKU scope or amount readback failed")
	}
	if productWrites.Load() != 1 || skuWrites.Load() != 1 {
		t.Fatalf("expected exactly one real product/SKU request, got %d/%d", productWrites.Load(), skuWrites.Load())
	}
	after := facts()
	for _, table := range []string{"storefront.cart_lines", "storefront.quotes", "storefront.events", "checkout.orders", "checkout.command_results", "checkout.payment_attempts", "integration.operations", "integration.operation_events", "inventory.reservations", "inventory.ledger"} {
		if before[table] != after[table] {
			t.Fatalf("unexpected purchase side effect in %s", table)
		}
	}
	if after["ops.command_results"]-before["ops.command_results"] != 2 {
		t.Fatal("expected exactly two merchant command receipts")
	}
	if countRows(t, h.f.owner, `SELECT count(*) FROM identity.sessions WHERE principal_id=$1 AND audience='merchant'`, principal) != 1 || countRows(t, h.f.owner, `SELECT count(*) FROM control.storefront_publications WHERE store_id=$1 AND published`, h.f.storeA1) != 0 {
		t.Fatal("real merchant session or unpublish readback failed")
	}
	idp.mu.Lock()
	exchanges := idp.exchanges
	idp.mu.Unlock()
	if exchanges != 1 {
		t.Fatal("expected one real signed MOCK IdP exchange")
	}
	t.Logf("PASS: production admin/Next + signed MOCK IdP + real catalog BFF/Go/PG -> exact buyer URL; cases=%d; 3 locales; zero purchase effects; evidence=%s", result.Cases, evidence)
}
