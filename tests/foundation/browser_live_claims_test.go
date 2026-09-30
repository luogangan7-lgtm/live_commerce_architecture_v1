//go:build browser

// browser_live_claims_test.go owns KC16 (contracts/live-keyword-claims-v1.md §11.1): the
// real Studio › Claims → buyer claim link → cart chain across packaged admin Next, the
// storefront Next production server, the ordinary Go admin and buyer transports, and
// task-owned PostgreSQL, driven by Chromium through tests/admin/claims-ui.spec.ts.
//
// Non-goals: no provider (ingress is MOCK/manual, nothing is sent), no deployment DNS/TLS
// proof (buyer.example is a disposable local TLS edge behind a CONNECT proxy), and no
// substitute for KC01–KC15 (independent test_worker gates).
//
// Fixtures (the only non-product parts): bhSetup (published synthetic storefront origin,
// t04 stock with two SKUs, buyer HTTP), the signed MOCK IdP mapped to a dedicated
// principal, one Studio draft created through live.CreateDraft, and a runner-only control
// listener (random key) that reports side-effect row counts and checks a token the spec
// saw against the recorded transport. The browser never talks to Go directly.

package foundation_test

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"livecommerce/internal/claims"
	"livecommerce/internal/httpapi"
	"livecommerce/internal/identity"
	"livecommerce/internal/identityhttp"
	"livecommerce/internal/live"
	"livecommerce/internal/oidclogin"
	"livecommerce/internal/platform"
)

// claimsTransportLog records every request that reached the Go admin or buyer transport,
// so the gate can prove afterwards where a link token did (and did not) travel.
type claimsTransportLog struct {
	mu       sync.Mutex
	requests []claimsSeen
}

type claimsSeen struct {
	target      string // path + raw query
	claimHeader string // X-Commerce-Claim-Token as received (buyer transport only)
}

func (l *claimsTransportLog) wrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		l.mu.Lock()
		l.requests = append(l.requests, claimsSeen{target: r.URL.RequestURI(), claimHeader: r.Header.Get("X-Commerce-Claim-Token")})
		l.mu.Unlock()
		next.ServeHTTP(w, r)
	})
}

// audit returns how often token appeared in a URL, and how often the claim header reached
// a route other than B1/B2.
func (l *claimsTransportLog) audit(token string) (urlHits, misplacedHeaders, claimCalls int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, seen := range l.requests {
		if strings.Contains(seen.target, token) {
			urlHits++
		}
		claimRoute := seen.target == "/v1/buyer/claim-link" || seen.target == "/v1/buyer/claim-link/redeem"
		if claimRoute {
			claimCalls++
		}
		if seen.claimHeader != "" && !claimRoute {
			misplacedHeaders++
		}
	}
	return urlHits, misplacedHeaders, claimCalls
}

func TestBrowserLiveClaimsRealChain(t *testing.T) {
	if os.Getenv("LC_BROWSER_LIVE_CLAIMS_ACCEPTANCE") != "1" || os.Getenv("LC_TEST_DATABASE_ALLOWED") != "1" {
		t.Fatal("use scripts/dev/test-local.sh --browser-live-claims")
	}
	h := bhSetup(t)
	ctx, cancel := context.WithTimeout(context.Background(), 330*time.Second)
	defer cancel()
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "output", "playwright"), 0o755); err != nil {
		t.Fatal(err)
	}
	evidence, err := os.MkdirTemp(filepath.Join(root, "output", "playwright"), "live-claims-")
	if err != nil {
		t.Fatal(err)
	}

	// Merchant identity: a dedicated principal reachable only through the signed MOCK IdP
	// (browser) and one fixture token used solely to create the draft scene below.
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	adminOrigin := "http://" + listener.Addr().String()
	_, adminPort, _ := net.SplitHostPort(listener.Addr().String())
	_ = listener.Close()
	idp := newBrowserIDP(t, adminOrigin+"/api/auth/callback")
	_, _, authority := identityFixture(t)
	provider, err := oidclogin.New(ctx, oidclogin.Config{Issuer: idp.server.URL, ClientID: browserClientID, RedirectURL: idp.redirect, AllowLoopbackForTests: true})
	if err != nil {
		t.Fatal(err)
	}
	service, err := identity.New(authority, observedBrowserProvider{Provider: provider, t: t}, identity.Policy{ProviderKey: "browser-live-claims-mock-v1", SessionTTL: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	principal := randomUUID()
	mustExec(t, h.f.owner, `INSERT INTO identity.principals(id) VALUES($1)`, principal)
	mustExec(t, h.f.owner, `INSERT INTO identity.memberships(tenant_id,principal_id) VALUES($1,$2)`, h.f.tenantA, principal)
	mustExec(t, h.f.owner, `INSERT INTO identity.external_identities(issuer,subject,principal_id) VALUES($1,'browser-subject',$2)`, idp.server.URL, principal)
	// live:read/live:manage are not provisioned for existing memberships (§12); grant explicitly.
	// integration:execute: the claim-source definer requires it with live:manage (ruling 24).
	mustExec(t, h.f.owner, `INSERT INTO identity.store_grants(tenant_id,store_id,principal_id,permission)
		SELECT $1,$2,$3,p FROM unnest(ARRAY['store:read','catalog:read','inventory:read','live:read','live:manage','integration:execute']) p`, h.f.tenantA, h.f.storeA1, principal)
	// Comment source (claim-source, rulings o/p/s/t) runs against Go + PG: the store gets exactly one
	// enabled, routed Facebook Page and Instagram account binding (synthetic ids, no Page token), so the
	// UI shows its platform select, a bare id needs the hint and private_reply is refused page_token_missing.
	if n := countRows(t, h.f.owner, `SELECT count(*) FROM integration.bindings WHERE tenant_id=$1 AND store_id=$2 AND enabled
		AND provider IN ('facebook','instagram')`, h.f.tenantA, h.f.storeA1); n != 0 {
		t.Fatalf("store A1 already has %d enabled Meta bindings; the claim-source phase needs a clean store", n)
	}
	m := miSetup(t)
	fbAsset, igAsset := miAsset(), miAsset()
	miRoute(t, m, fbAsset, h.f.tenantA, h.f.storeA1, miBinding(t, m, fbAsset, "facebook", h.f.tenantA, h.f.storeA1, principal))
	igBinding := miBinding(t, m, igAsset, "instagram", h.f.tenantA, h.f.storeA1, principal)
	var igRoute string
	var igEpoch int64
	if err := m.registrar.QueryRow(ctx, `SELECT * FROM meta_inbox.activate_route($1,'instagram',$2,$3,$4,$5,1,$6,$7,0)`,
		miApp, igAsset, h.f.tenantA, h.f.storeA1, igBinding, strings.Repeat("c", 64), time.Now().Add(time.Hour)).Scan(&igRoute, &igEpoch); err != nil {
		t.Fatal("instagram route", err)
	}
	fixtureToken := randomToken()
	tx, err := h.f.owner.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := insertSession(ctx, tx, fixtureToken, principal, "merchant", time.Now().Add(time.Hour), nil); err != nil {
		_ = tx.Rollback(ctx)
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	sceneTitle := "KC16 live claims " + t04Tag()
	var draft live.Draft
	if err := platform.WithScope(ctx, h.f.runtime, fixtureToken, h.f.storeA1, "live:manage", func(tx pgx.Tx, s platform.Scope) (err error) {
		draft, err = live.CreateDraft(ctx, tx, s, fixtureToken, t04Key("kc16-draft"), live.DraftInput{Title: sceneTitle, AspectRatio: "9:16"})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	// The store-wide one-OPEN-window rule would leak into later suites; close it whatever happens.
	t.Cleanup(func() {
		mustExec(t, h.f.owner, `UPDATE live.claim_windows SET state='CLOSED',closed_at=clock_timestamp() WHERE session_id=$1 AND state='OPEN'`, draft.ID)
	})

	labels, err := claims.NewLabelKey(randomBytes(32))
	if err != nil {
		t.Fatal(err)
	}
	adminKey := randomToken()
	private, err := identityhttp.NewHandler(service, adminKey)
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	mux.Handle("/v1/identity/", private)
	// R1 deploy shape (ruling G2): planning-only Studio + claims, no media planner (media routes 404).
	mux.Handle("/", httpapi.NewHandler(h.f.runtime, httpapi.Options{SessionStoreList: true, Studio: true, ClaimLabels: &labels}))
	transport := &claimsTransportLog{}
	adminAPI := httptest.NewServer(transport.wrap(mux))
	t.Cleanup(adminAPI.Close)
	buyerAPI := httptest.NewServer(transport.wrap(h.server.Config.Handler))
	t.Cleanup(buyerAPI.Close)

	// Runner-only controls on their own loopback listener behind a random key; never an
	// application route. /observe-token keeps the token in memory for one comparison.
	tables := []string{"inventory.reservations", "inventory.ledger", "storefront.quotes", "checkout.orders",
		"checkout.payment_attempts", "integration.operations"}
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
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/facts":
			_ = json.NewEncoder(w).Encode(facts())
		case r.Method == http.MethodPost && r.URL.Path == "/observe-token":
			raw, err := io.ReadAll(io.LimitReader(r.Body, 128))
			token := strings.TrimSpace(string(raw))
			if err != nil || len(token) != 43 {
				http.Error(w, "bad token", http.StatusBadRequest)
				return
			}
			urlHits, misplaced, claimCalls := transport.audit(token)
			_ = json.NewEncoder(w).Encode(map[string]int{"url_hits": urlHits, "misplaced_headers": misplaced, "claim_calls": claimCalls})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(control.Close)

	adminLog := browserLog(t, filepath.Join(evidence, "admin-next.log"))
	admin := exec.CommandContext(ctx, "node", filepath.Join(root, "apps/admin/.next/standalone/apps/admin/server.js"))
	admin.Dir = root
	admin.Env = browserEnvironment(map[string]string{"HOSTNAME": "127.0.0.1", "PORT": adminPort, "NODE_ENV": "production",
		"NEXT_TELEMETRY_DISABLED": "1", "COMMERCE_IDENTITY_ENABLED": "1", "COMMERCE_IDENTITY_ALLOW_LOOPBACK_TESTS": "1",
		"COMMERCE_PUBLIC_ORIGIN": adminOrigin, "COMMERCE_API_ORIGIN": adminAPI.URL, "COMMERCE_OIDC_ISSUER": idp.server.URL,
		"COMMERCE_BFF_KEY": adminKey})
	admin.Stdout, admin.Stderr = adminLog, adminLog
	storefrontPort := freeLoopbackPort(t)
	storefrontLog := browserLog(t, filepath.Join(evidence, "storefront-next.log"))
	storefront := exec.CommandContext(ctx, "node", filepath.Join(root, "apps/storefront/node_modules/next/dist/bin/next"),
		"start", "--hostname", "127.0.0.1", "--port", storefrontPort)
	storefront.Dir = filepath.Join(root, "apps/storefront")
	storefront.Env = browserEnvironment(map[string]string{"NODE_ENV": "production", "NEXT_TELEMETRY_DISABLED": "1",
		"COMMERCE_BUYER_WEB_ENABLED": "1", "COMMERCE_BUYER_DEMO_LABEL": "1", "COMMERCE_BUYER_API_ORIGIN": buyerAPI.URL,
		"COMMERCE_BUYER_BFF_KEY": h.key, "COMMERCE_BUYER_COOKIE_KEY": brToken(), "COMMERCE_BUYER_SESSION_TTL": "3600"})
	storefront.Stdout, storefront.Stderr = storefrontLog, storefrontLog
	for _, process := range []*exec.Cmd{admin, storefront} {
		process.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		if err := process.Start(); err != nil {
			t.Fatal(err)
		}
		owned := process
		t.Cleanup(func() {
			_ = syscall.Kill(-owned.Process.Pid, syscall.SIGKILL)
			_ = owned.Wait()
		})
	}
	waitReady(ctx, t, adminOrigin+"/api/stores", "", http.StatusUnauthorized, evidence)
	waitReady(ctx, t, "http://127.0.0.1:"+storefrontPort+"/api/buyer/session", "buyer.example", http.StatusOK, evidence)

	// buyer.example: a disposable TLS edge in front of the storefront server, reachable only
	// through this CONNECT proxy (Chromium bypasses it for the loopback admin origin).
	target, err := url.Parse("http://127.0.0.1:" + storefrontPort)
	if err != nil {
		t.Fatal(err)
	}
	edge := httptest.NewTLSServer(httputil.NewSingleHostReverseProxy(target))
	t.Cleanup(edge.Close)
	proxy := connectProxy(t, "buyer.example:443", edge.Listener.Addr().String())

	sku0, sku1 := h.stock.skus[0], h.stock.skus[1]
	playwrightLog := browserLog(t, filepath.Join(evidence, "playwright.log"))
	browser := exec.CommandContext(ctx, "pnpm", "exec", "playwright", "test", "tests/admin/claims-ui.spec.ts",
		"--reporter=list", "--output="+filepath.Join(evidence, "results"))
	browser.Dir = root
	browser.Env = browserEnvironment(map[string]string{
		"LC_BROWSER_SUITE": "live-claims", "LC_BROWSER_PUBLIC_ORIGIN": adminOrigin, "LC_BROWSER_EVIDENCE": evidence,
		"LC_CLAIMS_PROXY": proxy, "LC_CLAIMS_BUYER_ORIGIN": "https://buyer.example",
		"LC_CLAIMS_STORE": h.f.storeA1, "LC_CLAIMS_SESSION": draft.ID, "LC_CLAIMS_SCENE": sceneTitle,
		"LC_CLAIMS_PRODUCT": h.stock.product.Name, "LC_CLAIMS_SKU_A": sku0.Code, "LC_CLAIMS_SKU_B": sku1.Code,
		"LC_CLAIMS_SKU_A_ID": sku0.ID, "LC_CLAIMS_SKU_B_ID": sku1.ID,
		"LC_CLAIMS_CONTROL": control.URL, "LC_CLAIMS_CONTROL_KEY": controlKey,
		"LC_CLAIMS_FB_ASSET": fbAsset, "LC_CLAIMS_IG_ASSET": igAsset,
	})
	browser.Stdout, browser.Stderr = playwrightLog, playwrightLog
	if err := browser.Run(); err != nil {
		t.Fatalf("KC16 browser chain failed: %v; evidence=%s", err, evidence)
	}

	// Independent PostgreSQL readback of what the browser caused.
	var result struct {
		Cases       int      `json:"cases"`
		Locales     []string `json:"locales"`
		LinkSHA256  string   `json:"link_sha256"`
		Generations int64    `json:"generation"`
	}
	raw, err := os.ReadFile(filepath.Join(evidence, "result.json"))
	if err != nil || json.Unmarshal(raw, &result) != nil || result.Cases < 8 || strings.Join(result.Locales, ",") != "en,zh-CN,zh-TW" {
		t.Fatalf("missing KC16 browser result: %s evidence=%s", raw, evidence)
	}
	var label string
	var bound bool
	var bundleID string
	if err := h.f.owner.QueryRow(ctx, `SELECT id::text,label,owner_id IS NOT NULL FROM claims.bundles WHERE session_id=$1`, draft.ID).Scan(&bundleID, &label, &bound); err != nil || label != "amy" || !bound {
		t.Fatalf("bundle readback label=%q bound=%t err=%v", label, bound, err)
	}
	var linkHash []byte
	var generation int64
	if err := h.f.owner.QueryRow(ctx, `SELECT token_hash,generation FROM claims.links WHERE bundle_id=$1`, bundleID).Scan(&linkHash, &generation); err != nil ||
		hex.EncodeToString(linkHash) != result.LinkSHA256 || generation != result.Generations {
		t.Fatalf("browser-seen link is not the stored link: generation=%d/%d err=%v", generation, result.Generations, err)
	}
	for sku, want := range map[string]int64{sku0.ID: 3, sku1.ID: 1} {
		var claimed, applied, carted int64
		if err := h.f.owner.QueryRow(ctx, `SELECT l.quantity,coalesce(l.applied_version,0)-l.version,c.quantity
			FROM claims.lines l JOIN claims.bundles b ON b.tenant_id=l.tenant_id AND b.store_id=l.store_id AND b.id=l.bundle_id
			JOIN storefront.cart_lines c ON c.tenant_id=b.tenant_id AND c.store_id=b.store_id AND c.owner_id=b.owner_id AND c.sku_id=l.sku_id
			WHERE l.bundle_id=$1 AND l.sku_id=$2`, bundleID, sku).Scan(&claimed, &applied, &carted); err != nil || claimed != want || applied != 0 || carted != want {
			t.Fatalf("sku %s claimed=%d applied_delta=%d cart=%d want=%d err=%v", sku, claimed, applied, carted, want, err)
		}
	}
	var accepted, notUnderstood int
	if err := h.f.owner.QueryRow(ctx, `SELECT count(*) FILTER (WHERE outcome='ACCEPTED'),count(*) FILTER (WHERE reason='NO_MATCH')
		FROM claims.events WHERE session_id=$1`, draft.ID).Scan(&accepted, &notUnderstood); err != nil || accepted != 4 || notUnderstood != 1 {
		t.Fatalf("events accepted=%d no_match=%d err=%v", accepted, notUnderstood, err)
	}
	// Comment source readback: the browser's last save re-bound the scene to the Instagram media (platform
	// hint), the Facebook post row stays inactive, and no scene ever held two active sources.
	var igActive, fbInactive, active int
	if err := h.f.owner.QueryRow(ctx, `SELECT count(*) FILTER (WHERE active AND platform='instagram' AND asset_id=$2 AND source_object_id='17900000000000001'),
		count(*) FILTER (WHERE NOT active AND platform='facebook' AND source_object_id=$3), count(*) FILTER (WHERE active)
		FROM live.claim_sources WHERE session_id=$1`, draft.ID, igAsset, fbAsset+"_123456789012345").Scan(&igActive, &fbInactive, &active); err != nil ||
		igActive != 1 || fbInactive != 1 || active != 1 {
		t.Fatalf("claim-source readback ig_active=%d fb_inactive=%d active=%d err=%v", igActive, fbInactive, active, err)
	}
	after := facts()
	for _, table := range tables {
		if before[table] != after[table] {
			t.Fatalf("claims caused a %s effect (%d → %d)", table, before[table], after[table])
		}
	}
	idp.mu.Lock()
	exchanges := idp.exchanges
	idp.mu.Unlock()
	if exchanges != 1 {
		t.Fatalf("expected one signed MOCK IdP exchange, got %d", exchanges)
	}
	t.Logf("PASS KC16: admin Next + storefront Next + Go + PG (incl. claim-source), signed MOCK IdP, MOCK manual ingress; cases=%d locales=%v; evidence=%s", result.Cases, result.Locales, evidence)
}

// freeLoopbackPort reserves and releases one loopback port for a child server.
func freeLoopbackPort(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	_, port, _ := net.SplitHostPort(listener.Addr().String())
	return port
}

// waitReady polls a child server until it answers the expected status (Host override for
// the storefront's published-origin check) or the gate deadline passes.
func waitReady(ctx context.Context, t *testing.T, target, host string, status int, evidence string) {
	t.Helper()
	client := &http.Client{Timeout: time.Second}
	for i := 0; i < 200; i++ {
		request, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
		if err != nil {
			t.Fatal(err)
		}
		if host != "" {
			request.Host = host
		}
		if response, err := client.Do(request); err == nil {
			_ = response.Body.Close()
			if response.StatusCode == status {
				return
			}
		}
		select {
		case <-ctx.Done():
			t.Fatalf("%s readiness deadline; evidence=%s", target, evidence)
		case <-time.After(100 * time.Millisecond):
		}
	}
	t.Fatalf("%s not ready; evidence=%s", target, evidence)
}

// connectProxy is an HTTP proxy that tunnels exactly one CONNECT authority to one local
// address and refuses everything else. It returns the proxy URL for Chromium.
func connectProxy(t *testing.T, authority, upstream string) string {
	t.Helper()
	var mu sync.Mutex
	var tunnels []net.Conn
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodConnect || r.Host != authority {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		remote, err := net.Dial("tcp", upstream)
		if err != nil {
			http.Error(w, "unavailable", http.StatusBadGateway)
			return
		}
		client, buffered, err := w.(http.Hijacker).Hijack()
		if err != nil {
			_ = remote.Close()
			return
		}
		mu.Lock()
		tunnels = append(tunnels, client, remote)
		mu.Unlock()
		_, _ = client.Write([]byte("HTTP/1.1 200 Connection Established\r\n\r\n"))
		go func() { _, _ = io.Copy(remote, buffered); _ = remote.Close() }()
		go func() { _, _ = io.Copy(client, remote); _ = client.Close() }()
	}))
	t.Cleanup(func() {
		server.Close()
		mu.Lock()
		defer mu.Unlock()
		for _, conn := range tunnels {
			_ = conn.Close()
		}
	})
	return server.URL
}
