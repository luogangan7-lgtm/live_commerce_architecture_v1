//go:build browser

package foundation_test

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math/big"
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
	"testing"
	"time"

	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"
	"livecommerce/internal/httpapi"
	"livecommerce/internal/identity"
	"livecommerce/internal/identityhttp"
	"livecommerce/internal/integrations/accounts"
	"livecommerce/internal/integrations/core"
	"livecommerce/internal/oidclogin"
)

// This optional gate connects the actual Next BFF and Go authority to a real,
// task-owned PG. Only the external IdP is mocked. It is not a real-provider or
// provider acceptance. The settings suite below exercises the actual approved UI.
// Explicit build tag keeps ordinary Go tests independent of Node/Chromium.
func TestBrowserIdentityRealChain(t *testing.T) {
	runBrowserIdentityChain(t, false)
}

func TestBrowserSettingsWizardRealChain(t *testing.T) {
	runBrowserIdentityChain(t, true)
}

// The same signed IdP/BFF/PG harness is shared, but the wizard has no market seed.
func runBrowserIdentityChain(t *testing.T, wizard bool) {
	t.Helper()
	if os.Getenv("LC_BROWSER_IDENTITY_ACCEPTANCE") != "1" || os.Getenv("LC_TEST_DATABASE_ALLOWED") != "1" {
		t.Fatal("use scripts/dev/test-local.sh --browser-identity; isolated fixtures are required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Second)
	defer cancel()
	f := fixture(t)
	_, _, authority := identityFixture(t)
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	// Reserve a fresh loopback port, then hand it to Node. If another process
	// wins the small release/start race, readiness or process exit fails closed.
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	uiAddress := listener.Addr().String()
	_ = listener.Close()
	_, uiPort, _ := net.SplitHostPort(uiAddress)
	publicOrigin := "http://" + uiAddress
	idp := newBrowserIDP(t, publicOrigin+"/api/auth/callback")
	provider, err := oidclogin.New(ctx, oidclogin.Config{
		Issuer: idp.server.URL, ClientID: browserClientID,
		RedirectURL: idp.redirect, AllowLoopbackForTests: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	service, err := identity.New(authority, observedBrowserProvider{Provider: provider, t: t}, identity.Policy{
		ProviderKey: "browser-signed-mock-v1", SessionTTL: time.Hour,
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
	// Insert-only dependency, no worker/provider is started. Random fixture keys
	// are held only in Go memory, never in the Next environment or browser.
	jobs, err := river.NewClient(riverpgxv5.New(f.runtime), &river.Config{Schema: "river"})
	if err != nil {
		t.Fatal(err)
	}
	bindings, err := core.New(jobs)
	if err != nil {
		t.Fatal(err)
	}
	accountKeys, err := accounts.NewKeyring("browser_fixture", map[string][]byte{"browser_fixture": randomBytes(32)}, randomBytes(32))
	if err != nil {
		t.Fatal(err)
	}
	accountService, err := accounts.New(accountKeys, bindings)
	if err != nil {
		t.Fatal(err)
	}
	mux.Handle("/", httpapi.NewHandler(f.runtime, httpapi.Options{SessionStoreList: true, Accounts: accountService}))
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		recorded := httptest.NewRecorder()
		mux.ServeHTTP(recorded, r)
		// Historical transport suite keeps its isolated fixture. The new settings
		// suite MUST create markets through the actual UI and HTTP command instead.
		if !wizard && r.Method == "POST" && r.URL.Path == "/v1/identity/initial-store" && recorded.Code == http.StatusOK {
			var created identity.Store
			if err := json.Unmarshal(recorded.Body.Bytes(), &created); err != nil {
				t.Error("fixture could not decode successful store receipt")
				w.WriteHeader(500)
				return
			}
			_, err := f.owner.Exec(r.Context(), `INSERT INTO pricing.markets(tenant_id,store_id,id,code,name,currency,principal_id)
				SELECT i.tenant_id,i.store_id,$1,'browser_tw','Browser fixture Taiwan','TWD',i.principal_id
				FROM identity.initial_stores i WHERE i.tenant_id=$2 AND i.store_id=$3
				ON CONFLICT(tenant_id,store_id,id) DO NOTHING`, browserMarketID, created.TenantID, created.StoreID)
			if err != nil {
				t.Error("isolated browser market fixture failed")
				w.WriteHeader(500)
				return
			}
		}
		for name, values := range recorded.Header() {
			w.Header()[name] = values
		}
		w.WriteHeader(recorded.Code)
		_, _ = w.Write(recorded.Body.Bytes())
		// Path and status only: never record query, cookie, auth header or body.
		t.Logf("Go transport %s %s -> %d", r.Method, r.URL.Path, recorded.Code)
	}))
	t.Cleanup(api.Close)

	// Per-run evidence, no tokens/DB credentials in console or process args.
	suite, spec := "identity-real", "auth-real.spec.ts"
	if wizard {
		suite, spec = "settings-real", "settings-real.spec.ts"
	}
	evidence := filepath.Join(root, "output", "playwright", suite+"-"+time.Now().UTC().Format("20060102T150405.000000000"))
	if err := os.MkdirAll(evidence, 0700); err != nil {
		t.Fatal(err)
	}
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
		_ = server.Process.Kill() // exact child PID only, never a port/process-name kill
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
	browser := exec.CommandContext(ctx, "pnpm", "exec", "playwright", "test", "tests/admin/"+spec, "--reporter=list", "--output="+filepath.Join(evidence, "results"))
	browser.Dir = root
	browser.Env = browserEnvironment(map[string]string{
		"LC_BROWSER_SUITE":         suite,
		"LC_BROWSER_PUBLIC_ORIGIN": publicOrigin, "LC_BROWSER_API_ORIGIN": api.URL,
		"LC_BROWSER_ISSUER":    idp.server.URL,
		"LC_BROWSER_MARKET_ID": browserMarketID,
	})
	browser.Stdout, browser.Stderr = browserLogFile, browserLogFile
	if err := browser.Run(); err != nil {
		t.Fatalf("browser chain failed; local evidence: %s", evidence)
	}
	// Independently prove the browser did not merely accept a mock HTTP receipt.
	var sessions, revoked, stores int
	err = f.owner.QueryRow(ctx, `SELECT count(*), count(*) FILTER (WHERE s.revoked_at IS NOT NULL)
		FROM identity.sessions s JOIN identity.external_identities e ON e.principal_id=s.principal_id
		WHERE e.issuer=$1 AND e.subject='browser-subject'`, idp.server.URL).Scan(&sessions, &revoked)
	if err != nil || sessions != 1 || revoked != 1 {
		t.Fatalf("database session/revocation proof: sessions=%d revoked=%d err=%v", sessions, revoked, err)
	}
	err = f.owner.QueryRow(ctx, `SELECT count(*) FROM identity.initial_stores s
		JOIN identity.external_identities e ON e.principal_id=s.principal_id
		WHERE e.issuer=$1 AND e.subject='browser-subject'`, idp.server.URL).Scan(&stores)
	if err != nil || stores != 1 {
		t.Fatalf("database bootstrap receipt proof: stores=%d err=%v", stores, err)
	}
	var accountCount, credentialCount, qualifications, credentialVersion, operations int
	err = f.owner.QueryRow(ctx, `SELECT count(DISTINCT a.id), count(c.version),
		(SELECT count(*) FROM payments.account_qualifications q WHERE q.tenant_id=s.tenant_id),
		coalesce(max(a.credential_version),0),
		(SELECT count(*) FROM integration.operations o WHERE o.tenant_id=s.tenant_id)
		FROM identity.initial_stores s
		JOIN identity.external_identities e ON e.principal_id=s.principal_id
		LEFT JOIN integration.merchant_accounts a ON a.tenant_id=s.tenant_id AND a.store_id=s.store_id
		LEFT JOIN integration.account_credentials c ON c.tenant_id=a.tenant_id AND c.store_id=a.store_id AND c.connection_id=a.id
		WHERE e.issuer=$1 AND e.subject='browser-subject' GROUP BY s.tenant_id`, idp.server.URL).
		Scan(&accountCount, &credentialCount, &qualifications, &credentialVersion, &operations)
	wantAccounts, wantCredentials, wantCredentialVersion := 1, 2, 2
	if wizard {
		// One initial credential despite crash/retries, plus one independent
		// account proving fresh UI hydration follows the saved method binding.
		wantAccounts, wantCredentialVersion = 2, 1
	}
	if err != nil || accountCount != wantAccounts || credentialCount != wantCredentials || qualifications != 0 || credentialVersion != wantCredentialVersion || operations != 0 {
		t.Fatalf("browser account persistence proof failed: accounts=%d credentials=%d qualifications=%d version=%d operations=%d err=%v", accountCount, credentialCount, qualifications, credentialVersion, operations, err)
	}
	var methodVersions, methodHeads, enabledMethods, inspectCommands int
	err = f.owner.QueryRow(ctx, `SELECT
		(SELECT count(*) FROM payments.method_versions m WHERE m.tenant_id=s.tenant_id),
		(SELECT count(*) FROM payments.method_heads m WHERE m.tenant_id=s.tenant_id),
		(SELECT count(*) FROM payments.method_versions m WHERE m.tenant_id=s.tenant_id AND m.enabled),
		(SELECT count(*) FROM ops.command_results c WHERE c.tenant_id=s.tenant_id AND c.operation LIKE '%inspect%')
		FROM identity.initial_stores s JOIN identity.external_identities e ON e.principal_id=s.principal_id
		WHERE e.issuer=$1 AND e.subject='browser-subject'`, idp.server.URL).
		Scan(&methodVersions, &methodHeads, &enabledMethods, &inspectCommands)
	// Wizard revision 2 is an independent concurrent editor; the stale UI must
	// not overwrite it with an implicitly upgraded expected_version.
	wantMethodVersions := 2
	if err != nil || methodVersions != wantMethodVersions || methodHeads != 1 || enabledMethods != 0 || inspectCommands != 0 {
		t.Fatalf("browser method/inspection persistence proof: versions=%d heads=%d enabled=%d inspect_commands=%d err=%v", methodVersions, methodHeads, enabledMethods, inspectCommands, err)
	}
	if wizard {
		var markets, policies, services, commands int
		err = f.owner.QueryRow(ctx, `SELECT
		 (SELECT count(*) FROM pricing.markets m WHERE m.tenant_id=s.tenant_id),
		 (SELECT count(*) FROM pricing.policy_versions p WHERE p.tenant_id=s.tenant_id),
		 (SELECT count(*) FROM fulfillment.service_versions f WHERE f.tenant_id=s.tenant_id),
		 (SELECT count(*) FROM ops.command_results c WHERE c.tenant_id=s.tenant_id AND c.operation='pricing.market.create')
		 FROM identity.initial_stores s JOIN identity.external_identities e ON e.principal_id=s.principal_id
		 WHERE e.issuer=$1 AND e.subject='browser-subject'`, idp.server.URL).Scan(&markets, &policies, &services, &commands)
		if err != nil || markets != 1 || policies != 1 || services != 1 || commands != 1 {
			t.Fatalf("wizard fresh setup persistence: markets=%d policies=%d services=%d market_commands=%d err=%v", markets, policies, services, commands, err)
		}
		var methodName string
		err = f.owner.QueryRow(ctx, `SELECT m.name_en FROM payments.method_versions m
		 JOIN identity.initial_stores s ON s.tenant_id=m.tenant_id AND s.store_id=m.store_id
		 JOIN identity.external_identities e ON e.principal_id=s.principal_id
		 WHERE e.issuer=$1 AND e.subject='browser-subject' AND m.version=2`, idp.server.URL).Scan(&methodName)
		if err != nil || methodName != "Remote saved name" {
			t.Fatalf("wizard stale draft overwrote concurrent editor or failed readback: err=%v", err)
		}
	}
	idp.mu.Lock()
	exchanges, keys := idp.exchanges, idp.keyReads
	idp.mu.Unlock()
	if exchanges != 1 || keys < 1 {
		t.Fatalf("signed IdP proof: exchanges=%d JWKS reads=%d", exchanges, keys)
	}
	t.Logf("PASS: browser -> Next -> Go -> real PG, signed MOCK IdP; evidence=%s", evidence)
}

func browserLog(t *testing.T, path string) *os.File {
	t.Helper()
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = file.Close() })
	return file
}

// Do not give Node the database owner DSN or ambient commerce credentials.
// browserFront returns the public origin of an admin Next server that listens on the plain-HTTP loopback address `upstream`.
// Chromium (the default engine) accepts the admin's Secure `__Host-` cookies over http://127.0.0.1, so the origin stays http://<upstream>.
// WebKit does not: Safari enforces the `__Host-` prefix rule (https origin required) even on loopback, so under LC_BROWSER_ENGINE=webkit
// the admin would never hold its login/session/CSRF cookies. That is a harness limit, not a product defect (production is https behind the
// edge, which is what this front imitates): the admin is then reached through a TLS listener (httptest self-signed certificate; the
// specs run with ignoreHTTPSErrors) whose HTTP reverse proxy adds X-Forwarded-Proto: https, keeps Host, and forwards the inbound
// X-Forwarded-For untouched (the password BFF tests assert client-IP handling). Used by the password-auth, CVS and merchant-buyer gates.
func browserFront(t *testing.T, upstream string) string {
	t.Helper()
	if os.Getenv("LC_BROWSER_ENGINE") != "webkit" {
		return "http://" + upstream
	}
	target, err := url.Parse("http://" + upstream)
	if err != nil {
		t.Fatal(err)
	}
	front := httptest.NewUnstartedServer(&httputil.ReverseProxy{Rewrite: func(r *httputil.ProxyRequest) {
		r.SetURL(target)
		r.Out.Host = r.In.Host
		if v, ok := r.In.Header["X-Forwarded-For"]; ok {
			r.Out.Header["X-Forwarded-For"] = v
		}
		r.Out.Header.Set("X-Forwarded-Proto", "https")
	}})
	front.StartTLS()
	t.Cleanup(front.Close)
	return front.URL
}

func browserEnvironment(values map[string]string) []string {
	env := []string{}
	for _, entry := range os.Environ() {
		name, _, _ := strings.Cut(entry, "=")
		_, replaced := values[name]
		// LC_BROWSER_ENGINE (chromium|webkit) is the one LC_* variable the Node/Playwright drivers must see: tests/storefront/browser-engine.mjs
		// and playwright.config.ts read it. It carries no secret and cannot widen any fixture's authority.
		if name == "LC_BROWSER_ENGINE" && !replaced {
			env = append(env, entry)
			continue
		}
		if replaced || strings.HasPrefix(name, "COMMERCE_") || strings.HasPrefix(name, "LC_") || name == "DATABASE_URL" || name == "POSTGRES_PASSWORD" {
			continue
		}
		env = append(env, entry)
	}
	for name, value := range values {
		env = append(env, name+"="+value)
	}
	return env
}

const browserClientID = "isolated-browser-client"
const browserMarketID = "99999999-9999-4999-8999-999999999999"

type observedBrowserProvider struct {
	*oidclogin.Provider
	t *testing.T
}

func (p observedBrowserProvider) Exchange(ctx context.Context, code, nonce, verifier string) (oidclogin.Identity, error) {
	result, err := p.Provider.Exchange(ctx, code, nonce, verifier)
	if err != nil {
		// The production provider deliberately returns only fixed, sanitized errors.
		p.t.Logf("signed mock exchange failure: %v", err)
	}
	return result, err
}

type browserCode struct{ nonce, challenge string }
type browserIDP struct {
	server              *httptest.Server
	key                 *rsa.PrivateKey
	redirect            string
	mu                  sync.Mutex
	codes               map[string]browserCode
	exchanges, keyReads int
}

func newBrowserIDP(t *testing.T, redirect string) *browserIDP {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	idp := &browserIDP{key: key, redirect: redirect, codes: map[string]browserCode{}}
	idp.server = httptest.NewServer(http.HandlerFunc(idp.serve))
	t.Cleanup(idp.server.Close)
	return idp
}

func (idp *browserIDP) serve(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	jsonReply := func(status int, value any) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(value)
	}
	switch r.URL.Path {
	case "/.well-known/openid-configuration":
		jsonReply(200, map[string]any{"issuer": idp.server.URL, "authorization_endpoint": idp.server.URL + "/authorize", "token_endpoint": idp.server.URL + "/token", "jwks_uri": idp.server.URL + "/jwks", "id_token_signing_alg_values_supported": []string{"RS256"}, "token_endpoint_auth_methods_supported": []string{"none"}})
	case "/jwks":
		idp.mu.Lock()
		idp.keyReads++
		idp.mu.Unlock()
		jsonReply(200, map[string]any{"keys": []map[string]string{{"kty": "RSA", "kid": "browser-test-key", "use": "sig", "alg": "RS256", "n": base64.RawURLEncoding.EncodeToString(idp.key.N.Bytes()), "e": base64.RawURLEncoding.EncodeToString(big.NewInt(int64(idp.key.E)).Bytes())}}})
	case "/authorize":
		q := r.URL.Query()
		if r.Method != "GET" || q.Get("redirect_uri") != idp.redirect || q.Get("client_id") != browserClientID || q.Get("response_type") != "code" || q.Get("scope") != "openid" || q.Get("code_challenge_method") != "S256" || !identityhttp.ValidSecret(q.Get("state")) || !identityhttp.ValidSecret(q.Get("nonce")) || !identityhttp.ValidSecret(q.Get("code_challenge")) {
			jsonReply(400, map[string]string{"error": "invalid_request"})
			return
		}
		code := randomToken()
		idp.mu.Lock()
		idp.codes[code] = browserCode{nonce: q.Get("nonce"), challenge: q.Get("code_challenge")}
		idp.mu.Unlock()
		destination, _ := url.Parse(idp.redirect)
		destination.RawQuery = url.Values{"state": {q.Get("state")}, "code": {code}, "iss": {idp.server.URL}}.Encode()
		http.Redirect(w, r, destination.String(), http.StatusSeeOther)
	case "/token":
		r.Body = http.MaxBytesReader(w, r.Body, 8192)
		if r.Method != "POST" || r.ParseForm() != nil || r.Form.Get("grant_type") != "authorization_code" || r.Form.Get("client_id") != browserClientID || r.Form.Get("redirect_uri") != idp.redirect {
			jsonReply(400, map[string]string{"error": "invalid_request"})
			return
		}
		idp.mu.Lock()
		code, ok := idp.codes[r.Form.Get("code")]
		delete(idp.codes, r.Form.Get("code"))
		idp.exchanges++
		idp.mu.Unlock()
		digest := sha256.Sum256([]byte(r.Form.Get("code_verifier")))
		if !ok || base64.RawURLEncoding.EncodeToString(digest[:]) != code.challenge {
			jsonReply(400, map[string]string{"error": "invalid_grant"})
			return
		}
		header, _ := json.Marshal(map[string]string{"alg": "RS256", "kid": "browser-test-key", "typ": "JWT"})
		claims, _ := json.Marshal(map[string]any{"iss": idp.server.URL, "sub": "browser-subject", "aud": browserClientID, "nonce": code.nonce, "exp": time.Now().Add(time.Minute).Unix(), "iat": time.Now().Add(-time.Second).Unix()})
		unsigned := base64.RawURLEncoding.EncodeToString(header) + "." + base64.RawURLEncoding.EncodeToString(claims)
		digest = sha256.Sum256([]byte(unsigned))
		signature, err := rsa.SignPKCS1v15(rand.Reader, idp.key, crypto.SHA256, digest[:])
		if err != nil {
			jsonReply(500, map[string]string{"error": "server_error"})
			return
		}
		jsonReply(200, map[string]any{"access_token": "unused-local-test-token", "token_type": "Bearer", "expires_in": 300, "id_token": fmt.Sprintf("%s.%s", unsigned, base64.RawURLEncoding.EncodeToString(signature))})
	default:
		http.NotFound(w, r)
	}
}
