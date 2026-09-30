package foundation_test

// G2 (docs/delivery/units/r1-final-rulings.md): the real cmd/api binary against real PostgreSQL in the
// R1 deploy shape COMMERCE_STUDIO_ENABLED=1 + COMMERCE_CLAIMS_ENABLED=1 + COMMERCE_STUDIO_MEDIA_ENABLED=0
// mounts live-session planning, keyword claims and claim-source, and leaves every LiveKit media route
// unmounted (404). MEDIA=1 mounts the media routes again; MEDIA=1 without Studio refuses startup.

import (
	"encoding/base64"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestStudioPlanningOnlyG2APIProcess(t *testing.T) {
	h := lpSetup(t)
	_, _, authority := identityFixture(t)
	// Discovery only: the API resolves its OIDC provider at startup; no login happens here.
	var issuer string
	idp := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/.well-known/openid-configuration" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"issuer": issuer, "authorization_endpoint": issuer + "/authorize",
			"token_endpoint": issuer + "/token", "jwks_uri": issuer + "/jwks", "id_token_signing_alg_values_supported": []string{"RS256"},
			"token_endpoint_auth_methods_supported": []string{"none"}})
	}))
	issuer = "http://" + idp.Listener.Addr().String() // set before Start: the handler goroutines only read it
	idp.Start()
	t.Cleanup(idp.Close)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := listener.Addr().String()
	_ = listener.Close()
	origin := "http://" + addr
	binary := mrBuild(t, "../../cmd/api", "g2-api")
	env := func(name string, extra ...string) []string {
		return append([]string{"LISTEN_ADDR=" + addr, "DATABASE_URL=" + mrNamedDSN(t, h.f.runtime.Config().ConnString(), name),
			"COMMERCE_IDENTITY_ENABLED=1", "COMMERCE_IDENTITY_ALLOW_LOOPBACK_TESTS=1", "COMMERCE_PUBLIC_ORIGIN=" + origin,
			"COMMERCE_IDENTITY_DATABASE_URL=" + authority.Config().ConnString(), "COMMERCE_BFF_KEY=" + randomToken(),
			"COMMERCE_OIDC_ISSUER=" + issuer, "COMMERCE_OIDC_CLIENT_ID=g2-client", "COMMERCE_IDENTITY_PROVIDER_KEY=g2-mock-v1",
			"COMMERCE_SESSION_TTL=1h", "COMMERCE_ONBOARDING_ENABLED=0"}, extra...)
	}
	client := &http.Client{Timeout: 5 * time.Second}
	ready := func(p *mrProcess) {
		t.Helper()
		deadline := time.Now().Add(12 * time.Second)
		for time.Now().Before(deadline) {
			select {
			case err := <-p.done:
				p.exited = true
				t.Fatalf("API exited before readiness: %v log=%s", err, p.logPath)
			default:
			}
			if resp, err := client.Get(origin + "/healthz"); err == nil {
				_ = resp.Body.Close()
				if resp.StatusCode == 200 {
					return
				}
			}
			time.Sleep(25 * time.Millisecond)
		}
		t.Fatalf("API not ready: %s", p.logPath)
	}
	call := func(method, path, token, body string) (int, map[string]any) {
		t.Helper()
		var reader io.Reader
		if body != "" {
			reader = strings.NewReader(body)
		}
		r, err := http.NewRequest(method, origin+path, reader)
		if err != nil {
			t.Fatal(err)
		}
		if token != "" {
			r.Header.Set("Authorization", "Bearer "+token)
		}
		if body != "" {
			r.Header.Set("Content-Type", "application/json")
			r.Header.Set("Idempotency-Key", t04Key("g2-"+method))
		}
		resp, err := client.Do(r)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		var out map[string]any
		_ = json.Unmarshal(raw, &out)
		return resp.StatusCode, out
	}
	sessions := "/v1/admin/stores/" + h.f.storeA1 + "/live-sessions"
	labelKey := base64.RawURLEncoding.EncodeToString(randomBytes(32))

	// 1. R1 deploy shape: planning + claims + claim-source mounted, media routes 404.
	name := "g2_planning_" + t04Tag()
	api := mrLaunch(t, binary, "g2-planning-only", env(name, "COMMERCE_STUDIO_ENABLED=1", "COMMERCE_STUDIO_MEDIA_ENABLED=0",
		"COMMERCE_CLAIMS_ENABLED=1", "COMMERCE_CLAIMS_LABEL_KEY="+labelKey))
	ready(api)
	status, created := call("POST", sessions, h.token, `{"title":"G2 planning only","aspect_ratio":"9:16"}`)
	session, _ := created["session_id"].(string)
	if status != 200 || session == "" {
		t.Fatalf("planning create status=%d body=%v", status, created)
	}
	base := sessions + "/" + session
	if status, detail := call("GET", base, h.token, ""); status != 200 || detail["media_enabled"] != false {
		t.Fatalf("planning detail must say media_enabled=false: status=%d body=%v", status, detail)
	}
	if status, _ := call("GET", sessions, h.token, ""); status != 200 {
		t.Fatalf("planning list status=%d", status)
	}
	if status, source := call("GET", base+"/claim-source", h.token, ""); status != 200 || source["source"] != nil {
		t.Fatalf("claim-source GET status=%d body=%v", status, source)
	}
	if status, board := call("GET", base+"/claims", h.token, ""); status != 200 {
		t.Fatalf("claims board GET status=%d body=%v", status, board)
	}
	// The smoke assertion (deploy/scripts/smoke.sh S45): unauthenticated claims routes answer 401, not 404.
	for _, path := range []string{base + "/claims", base + "/claim-source"} {
		if status, _ := call("GET", path, "", ""); status != http.StatusUnauthorized {
			t.Fatalf("unauthenticated %s status=%d, want 401 (mounted)", path, status)
		}
	}
	// The exact probes of smoke S45 (fake ids, no token, GET): 401/403 x3 and media 404.
	fake := "/v1/admin/stores/00000000-0000-4000-8000-000000000001/live-sessions"
	for path, want := range map[string]int{fake: 401, fake + "/00000000-0000-4000-8000-000000000002/claims": 401,
		fake + "/00000000-0000-4000-8000-000000000002/claim-source": 401, fake + "/00000000-0000-4000-8000-000000000002/rehearsal/start": 404} {
		if status, _ := call("GET", path, "", ""); status != want {
			t.Fatalf("smoke S45 probe %s status=%d want %d", path, status, want)
		}
	}
	for _, route := range []struct{ method, path string }{
		{"POST", base + "/rehearsal/start"}, {"POST", base + "/rehearsal/stop"}, {"GET", base + "/input"},
		{"GET", base + "/input/prepared"}, {"POST", base + "/input/start"}, {"POST", base + "/input/token"},
	} {
		if status, _ := call(route.method, route.path, h.token, ""); status != http.StatusNotFound {
			t.Fatalf("media route %s %s status=%d, want 404 with media off", route.method, route.path, status)
		}
	}
	mrStop(t, api, syscall.SIGTERM, true)
	waitPoolsGone(t, h.f, "planning-only API pool remained", name)

	// 2. MEDIA=1 (with Studio) mounts the rehearsal routes again and reports media_enabled=true.
	name = "g2_media_" + t04Tag()
	api = mrLaunch(t, binary, "g2-media", env(name, "COMMERCE_STUDIO_ENABLED=1", "COMMERCE_STUDIO_MEDIA_ENABLED=1"))
	ready(api)
	if status, detail := call("GET", base, h.token, ""); status != 200 || detail["media_enabled"] != true {
		t.Fatalf("media detail must say media_enabled=true: status=%d body=%v", status, detail)
	}
	if status, _ := call("POST", base+"/rehearsal/start", "", ""); status == http.StatusNotFound {
		t.Fatal("rehearsal/start unmounted with media on")
	}
	mrStop(t, api, syscall.SIGTERM, true)
	waitPoolsGone(t, h.f, "media API pool remained", name)

	// 3. MEDIA=1 without Studio is a configuration error: the process exits before listening.
	refused := mrLaunch(t, binary, "g2-media-without-studio", env("g2_refused_"+t04Tag(), "COMMERCE_STUDIO_MEDIA_ENABLED=1"))
	select {
	case err := <-refused.done:
		refused.exited = true
		if err == nil {
			t.Fatal("MEDIA=1 without Studio started")
		}
	case <-time.After(12 * time.Second):
		t.Fatalf("MEDIA=1 without Studio kept running: %s", refused.logPath)
	}
}
