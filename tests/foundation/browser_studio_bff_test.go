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
)

// Studio BFF transport is separate from the unapproved Studio page/UI gate.
// The IdP is a signed local mock; Next, Go and PG execute their real paths.
func TestBrowserStudioBFFRealChain(t *testing.T) {
	if os.Getenv("LC_BROWSER_STUDIO_BFF_ACCEPTANCE") != "1" || os.Getenv("LC_TEST_DATABASE_ALLOWED") != "1" {
		t.Fatal("use scripts/dev/test-local.sh --browser-studio-bff")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 210*time.Second)
	defer cancel()
	h := lmpSetup(t, true)
	mustExec(t, h.lp.f.owner, `INSERT INTO identity.store_grants(tenant_id,store_id,principal_id,permission) VALUES($1,$2,$3,'inventory:read')`, h.lp.f.tenantA, h.lp.f.storeA1, h.lp.actor)
	mustExec(t, h.lp.f.owner, `INSERT INTO identity.store_grants(tenant_id,store_id,principal_id,permission) VALUES($1,$2,$3,'live:read')`, h.lp.f.tenantA, h.lp.f.storeA1, h.lp.limited)
	expiredToken, revokedToken := randomToken(), randomToken()
	tx, err := h.lp.f.owner.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	for _, item := range []struct {
		token   string
		expiry  time.Time
		revoked *time.Time
	}{
		{expiredToken, now.Add(-time.Minute), nil}, {revokedToken, now.Add(time.Hour), &now},
	} {
		if err := insertSession(ctx, tx, item.token, h.lp.actor, "merchant", item.expiry, item.revoked); err != nil {
			_ = tx.Rollback(ctx)
			t.Fatal(err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	// Stop may create an execution row even before provider dispatch; LMP's
	// generic cleanup removes the attempt afterwards.
	t.Cleanup(func() {
		for _, item := range []struct{ query, arg string }{
			{`DELETE FROM live.media_execution_state WHERE attempt_id IN (SELECT id FROM live.media_attempts WHERE session_id=$1)`, h.session},
			{`DELETE FROM ops.command_results WHERE principal_id=$1 AND operation='live.media.stop'`, h.lp.actor},
			{`DELETE FROM ops.audit_events WHERE principal_id=$1 AND action='live.media.stop.requested'`, h.lp.actor},
		} {
			if _, err := h.lp.f.owner.Exec(context.Background(), item.query, item.arg); err != nil {
				t.Errorf("Studio BFF cleanup: %v", err)
			}
		}
	})
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
	mustExec(t, h.lp.f.owner, `INSERT INTO identity.external_identities(issuer,subject,principal_id) VALUES($1,'browser-subject',$2)`, idp.server.URL, h.lp.actor)
	t.Cleanup(func() {
		_, _ = h.lp.f.owner.Exec(context.Background(), `DELETE FROM identity.external_identities WHERE issuer=$1 AND subject='browser-subject'`, idp.server.URL)
		if _, err := h.lp.f.owner.Exec(context.Background(), `DELETE FROM identity.session_events WHERE session_id IN (SELECT id FROM identity.sessions WHERE principal_id=$1)`, h.lp.actor); err != nil {
			t.Errorf("signed session event cleanup: %v", err)
		}
	})
	_, _, authority := identityFixture(t)
	provider, err := oidclogin.New(ctx, oidclogin.Config{Issuer: idp.server.URL, ClientID: browserClientID, RedirectURL: idp.redirect, AllowLoopbackForTests: true})
	if err != nil {
		t.Fatal(err)
	}
	service, err := identity.New(authority, observedBrowserProvider{Provider: provider, t: t}, identity.Policy{
		ProviderKey: "browser-studio-signed-mock-v1", SessionTTL: time.Hour, OnboardingEnabled: true, Currencies: []string{"TWD", "USD"},
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
	mux.Handle("/", httpapi.NewHandler(h.lp.f.runtime, httpapi.Options{SessionStoreList: true, Live: h.planner}))
	var studioCalls, strippedFailures atomic.Int64
	var lastURI, lastMethod atomic.Value
	lastURI.Store("")
	lastMethod.Store("")
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/__test/studio-observation" {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{"count": studioCalls.Load(), "stripped_failures": strippedFailures.Load(), "last_uri": lastURI.Load(), "last_method": lastMethod.Load()})
			return
		}
		if strings.HasPrefix(r.URL.Path, "/v1/admin/stores/") && strings.Contains(r.URL.Path, "/live-sessions") {
			studioCalls.Add(1)
			lastURI.Store(r.RequestURI)
			lastMethod.Store(r.Method)
			if r.Header.Get("Cookie") != "" || r.Header.Get("X-Tenant-ID") != "" || r.Header.Get("X-Forwarded-Host") != "" || r.Header.Get("X-BFF-Test") != "" ||
				r.Header.Get("Authorization") == "Bearer browser-bogus-token" || r.Header.Get("Authorization") == "Bearer "+h.lp.token {
				strippedFailures.Add(1)
			}
			switch r.URL.RawQuery {
			case "cursor=NONJSON":
				w.Header().Set("Content-Type", "text/html")
				w.Header().Set("X-Backend-Secret", "backend-secret")
				w.WriteHeader(200)
				_, _ = w.Write([]byte("backend-secret"))
				return
			case "cursor=OVERSIZE":
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(200)
				_, _ = w.Write([]byte(`{"payload":"` + strings.Repeat("x", 260<<10) + `"}`))
				return
			case "cursor=ERROR":
				w.Header().Set("Content-Type", "application/json")
				w.Header().Set("X-Backend-Secret", "backend-secret")
				w.Header().Set("Set-Cookie", "upstream=backend-secret")
				w.WriteHeader(503)
				_, _ = w.Write([]byte(`{"code":"retry_later","message":"SQLSTATE backend-secret"}`))
				return
			}
		}
		mux.ServeHTTP(w, r)
	}))
	t.Cleanup(api.Close)
	evidence := filepath.Join(root, "output", "playwright", "studio-bff-"+time.Now().UTC().Format("20060102T150405.000000000"))
	if err := os.MkdirAll(evidence, 0700); err != nil {
		t.Fatal(err)
	}
	// Existing dev fixture must work for a normal resource, while Studio is
	// unavailable even with the same bearer and loopback Host.
	fixtureListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	fixtureAddr := fixtureListener.Addr().String()
	_ = fixtureListener.Close()
	_, fixturePort, _ := net.SplitHostPort(fixtureAddr)
	nextEnvPath := filepath.Join(root, "apps/admin/next-env.d.ts")
	nextEnvBefore, err := os.ReadFile(nextEnvPath)
	if err != nil {
		t.Fatal(err)
	}
	devNextEnv := bytes.ReplaceAll(nextEnvBefore, []byte("./.next/types/"), []byte("./.next/dev/types/"))
	t.Cleanup(func() {
		current, err := os.ReadFile(nextEnvPath)
		if err != nil {
			t.Error("Next generated types unreadable")
			return
		}
		if bytes.Equal(current, nextEnvBefore) {
			return
		}
		if !bytes.Equal(current, devNextEnv) {
			t.Error("unexpected Next generated type change; preserving")
			return
		}
		if err := os.WriteFile(nextEnvPath, nextEnvBefore, 0644); err != nil {
			t.Error(err)
		}
	})
	fixtureLog := browserLog(t, filepath.Join(evidence, "fixture-next.log"))
	fixture := exec.CommandContext(ctx, "node", filepath.Join(root, "apps/admin/node_modules/next/dist/bin/next"), "dev", "--hostname", "127.0.0.1", "--port", fixturePort)
	fixture.Dir = filepath.Join(root, "apps/admin")
	fixture.Env = browserEnvironment(map[string]string{"NODE_ENV": "development", "COMMERCE_IDENTITY_ENABLED": "0", "COMMERCE_FIXTURE_ENABLED": "1", "COMMERCE_FIXTURE_ALLOWED": "1", "COMMERCE_FIXTURE_TOKEN": h.lp.token, "COMMERCE_FIXTURE_STORE_ID": h.lp.f.storeA1, "COMMERCE_API_ORIGIN": api.URL})
	fixture.Stdout, fixture.Stderr = fixtureLog, fixtureLog
	// F6: `next dev` forks a next-server child that outlives its parent and holds the port; own a process
	// group and signal the whole group (this PID's group only, never by port or name).
	fixture.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	fixture.Cancel = func() error { return syscall.Kill(-fixture.Process.Pid, syscall.SIGKILL) }
	if err := fixture.Start(); err != nil {
		t.Fatal(err)
	}
	fixtureDone := make(chan error, 1)
	go func() { fixtureDone <- fixture.Wait() }()
	var stopFixtureOnce sync.Once
	stopFixture := func() {
		stopFixtureOnce.Do(func() {
			_ = syscall.Kill(-fixture.Process.Pid, syscall.SIGINT)
			select {
			case <-fixtureDone:
			case <-time.After(5 * time.Second):
				_ = syscall.Kill(-fixture.Process.Pid, syscall.SIGKILL)
				<-fixtureDone
			}
			stopProcessGroup(t, fixture.Process.Pid) // leaked next-server children hold the port
		})
	}
	t.Cleanup(stopFixture)
	fixtureClient := &http.Client{Timeout: 2 * time.Second}
	fixtureRead := func(resource string) (*http.Response, error) {
		req, err := http.NewRequestWithContext(ctx, "GET", "http://"+fixtureAddr+"/api/stores/"+h.lp.f.storeA1+"/"+resource, nil)
		if err != nil {
			return nil, err
		}
		req.Host = "127.0.0.1:3100"
		return fixtureClient.Do(req)
	}
	ready := false
	for i := 0; i < 100; i++ {
		resp, err := fixtureRead("warehouses")
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == 200 {
				ready = true
				break
			}
		}
		select {
		case <-ctx.Done():
			t.Fatal("fixture Next deadline")
		case <-time.After(100 * time.Millisecond):
		}
	}
	if !ready {
		t.Fatalf("fixture positive control failed: %s", evidence)
	}
	beforeStudio := studioCalls.Load()
	for _, resource := range []string{"live-sessions", "live-sessions/" + h.session} {
		resp, err := fixtureRead(resource)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != 404 || !strings.Contains(resp.Header.Get("Cache-Control"), "no-store") || studioCalls.Load() != beforeStudio {
			t.Fatalf("fixture Studio escape: %s status=%d calls=%d evidence=%s", resource, resp.StatusCode, studioCalls.Load()-beforeStudio, evidence)
		}
	}
	stopFixture()
	serverLog := browserLog(t, filepath.Join(evidence, "next.log"))
	server := exec.CommandContext(ctx, "node", filepath.Join(root, "apps/admin/.next/standalone/apps/admin/server.js"))
	server.Dir = root
	server.Env = browserEnvironment(map[string]string{"HOSTNAME": "127.0.0.1", "PORT": port, "NODE_ENV": "production", "COMMERCE_IDENTITY_ENABLED": "1", "COMMERCE_IDENTITY_ALLOW_LOOPBACK_TESTS": "1", "COMMERCE_PUBLIC_ORIGIN": origin, "COMMERCE_API_ORIGIN": api.URL, "COMMERCE_OIDC_ISSUER": idp.server.URL, "COMMERCE_BFF_KEY": bffKey, "COMMERCE_ONBOARDING_ENABLED": "1", "COMMERCE_ONBOARDING_CURRENCIES": "TWD,USD"})
	server.Stdout, server.Stderr = serverLog, serverLog
	if err := server.Start(); err != nil {
		t.Fatal(err)
	}
	serverDone := make(chan error, 1)
	go func() { serverDone <- server.Wait() }()
	t.Cleanup(func() {
		_ = server.Process.Kill()
		select {
		case <-serverDone:
		case <-time.After(5 * time.Second):
			t.Error("Next process did not stop")
		}
	})
	client := &http.Client{Timeout: time.Second}
	ready = false
	for i := 0; i < 100; i++ {
		resp, err := client.Get(origin + "/api/stores")
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == 401 {
				ready = true
				break
			}
		}
		select {
		case <-ctx.Done():
			t.Fatal("Next readiness deadline")
		case <-time.After(100 * time.Millisecond):
		}
	}
	if !ready {
		t.Fatalf("Next standalone not ready: %s", evidence)
	}
	browserLogFile := browserLog(t, filepath.Join(evidence, "browser.log"))
	browser := exec.CommandContext(ctx, "node", "--test", "--experimental-strip-types", "tests/admin/studio-bff.spec.ts")
	browser.Dir = root
	browser.Env = browserEnvironment(map[string]string{
		"LC_BROWSER_PUBLIC_ORIGIN": origin, "LC_BROWSER_API_ORIGIN": api.URL,
		"LC_BROWSER_STUDIO_STORE": h.lp.f.storeA1, "LC_BROWSER_STUDIO_FOREIGN_STORE": h.lp.f.storeA2,
		"LC_BROWSER_STUDIO_UNLISTED_STORE": h.lp.f.storeB, "LC_BROWSER_STUDIO_SESSION": h.session,
		"LC_BROWSER_STUDIO_AUTHORIZATION":  h.input.AuthorizationID,
		"LC_BROWSER_STUDIO_READONLY_TOKEN": h.lp.limitedToken,
		"LC_BROWSER_STUDIO_EXPIRED_TOKEN":  expiredToken, "LC_BROWSER_STUDIO_REVOKED_TOKEN": revokedToken,
	})
	browser.Stdout, browser.Stderr = browserLogFile, browserLogFile
	if err := browser.Run(); err != nil {
		t.Fatalf("Studio BFF browser transport failed: %s", evidence)
	}
	if strippedFailures.Load() != 0 || studioCalls.Load() < 8 {
		t.Fatalf("BFF did not strip browser authority or exercise backend: stripped=%d calls=%d evidence=%s", strippedFailures.Load(), studioCalls.Load(), evidence)
	}
	var issued int
	if err := h.lp.f.owner.QueryRow(ctx, `SELECT count(*) FROM identity.sessions s JOIN identity.session_events ev ON ev.session_id=s.id AND ev.action='session.issued' JOIN identity.external_identities e ON e.principal_id=s.principal_id WHERE e.issuer=$1 AND e.subject='browser-subject' AND s.token_hash<>$2`, idp.server.URL, tokenHash(h.lp.token)).Scan(&issued); err != nil || issued != 1 {
		t.Fatalf("signed browser session missing: %d %v", issued, err)
	}
	t.Logf("PASS: signed cookie -> packaged Next -> Go -> task-owned PG; evidence=%s", evidence)
}
