//go:build browser

package foundation_test

// PA11 TestBrowserPasswordAuth (BROWSER) — contracts/merchant-password-auth-v1.md §7.2/§7.3/§9 PA11.
// Real chain: Chromium (Playwright, tests/admin/password-auth.spec.ts) -> packaged Next admin BFF -> in-process
// Go api (identityhttp.NewPasswordHandler + the existing identity/http handlers) -> real PG, with the REAL
// *mail.SMTP adapter against the loopback mailtest server. Only external things are faked: the mailbox
// (loopback SMTP fake) and HIBP (loopback range fake). Password login is on and OIDC is NOT configured
// (ruling R-4), so this is also the OIDC-less startup proof.
// The inspection endpoint that hands the spec the emailed codes (GET /mail?to=<address>) exists only in this
// test binary, on a separate loopback listener, and never on the api origin. Codes are never read from SQL.
// After the spec the Go side (a) proves in PG that the browser flows really created principals, credentials,
// sessions, revocations and a store, and (b) scans the admin server log, the browser log and the Go log capture
// for every sentinel the spec recorded (passwords, emailed codes, email addresses): none may appear (I11).
// Owner-pool use: reads only. Evidence: output/playwright/password-auth-<ts>/ (or LC_BROWSER_EVIDENCE_ROOT).

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"
	"livecommerce/internal/httpapi"
	"livecommerce/internal/identityhttp"
	"livecommerce/internal/integrations/accounts"
	"livecommerce/internal/integrations/core"
)

func TestBrowserPasswordAuth(t *testing.T) {
	if os.Getenv("LC_BROWSER_PASSWORD_AUTH_ACCEPTANCE") != "1" || os.Getenv("LC_TEST_DATABASE_ALLOWED") != "1" {
		t.Fatal("use scripts/dev/test-local.sh --browser-password-auth; isolated fixtures are required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 14*time.Minute)
	defer cancel()
	logs := pwaCaptureLogs(t)
	// Daily mail cap 5000: the deployment-wide mail shares (§6) are not what this gate measures, and the
	// matrix + BFF checks send well over the default 200/day. Per-source limits stay at their contract values.
	e := newPwa(t, pwaCap(5000))
	f := e.f
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
	publicOrigin := browserFront(t, uiAddress) // https TLS front under LC_BROWSER_ENGINE=webkit, else http://uiAddress

	private, err := identityhttp.NewHandler(e.oidc, e.bffKey)
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	mux.Handle("/v1/identity/password/", e.handler) // the same handler cmd/api mounts (auth-core build step 4)
	mux.Handle("/v1/identity/", private)
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
		for name, values := range recorded.Header() {
			w.Header()[name] = values
		}
		w.WriteHeader(recorded.Code)
		_, _ = w.Write(recorded.Body.Bytes())
		// Path and status only: never the query, cookie, header or body.
		t.Logf("Go transport %s %s -> %d", r.Method, r.URL.Path, recorded.Code)
	}))
	t.Cleanup(api.Close)

	// Inspection endpoint: test binary only, separate loopback listener, read-only view of the mailtest mailbox.
	inspect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/mail" {
			http.NotFound(w, r)
			return
		}
		to := strings.ToLower(r.URL.Query().Get("to"))
		type item struct {
			Subject string `json:"subject"`
			Text    string `json:"text"`
			HTML    string `json:"html"`
		}
		out := []item{}
		for _, m := range e.smtp.Messages() {
			if strings.EqualFold(m.To, to) {
				out = append(out, item{m.Subject, m.Text, m.HTML})
			}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(out)
	}))
	t.Cleanup(inspect.Close)

	evidenceRoot := os.Getenv("LC_BROWSER_EVIDENCE_ROOT")
	if evidenceRoot == "" {
		evidenceRoot = filepath.Join(root, "output", "playwright")
	}
	evidence := filepath.Join(evidenceRoot, "password-auth-"+time.Now().UTC().Format("20060102T150405.000000000"))
	if err := os.MkdirAll(evidence, 0700); err != nil {
		t.Fatal(err)
	}
	serverLog := browserLog(t, filepath.Join(evidence, "next.log"))
	server := exec.CommandContext(ctx, "node", filepath.Join(root, "apps/admin/.next/standalone/apps/admin/server.js"))
	server.Dir = root
	server.Env = browserEnvironment(map[string]string{
		"HOSTNAME": "127.0.0.1", "PORT": uiPort, "NODE_ENV": "production",
		"COMMERCE_IDENTITY_ENABLED": "1", "COMMERCE_IDENTITY_ALLOW_LOOPBACK_TESTS": "1",
		"COMMERCE_PASSWORD_LOGIN_ENABLED": "1", // no COMMERCE_OIDC_ISSUER: password-only (ruling R-4)
		"COMMERCE_PUBLIC_ORIGIN":          publicOrigin, "COMMERCE_API_ORIGIN": api.URL,
		"COMMERCE_BFF_KEY":            e.bffKey,
		"COMMERCE_ONBOARDING_ENABLED": "1", "COMMERCE_ONBOARDING_CURRENCIES": "TWD,USD",
	})
	server.Stdout, server.Stderr = serverLog, serverLog
	if err := server.Start(); err != nil {
		t.Fatal("could not start packaged Next server")
	}
	serverDone := make(chan error, 1)
	go func() { serverDone <- server.Wait() }()
	t.Cleanup(func() {
		_ = server.Process.Kill() // exact child PID only
		select {
		case <-serverDone:
		case <-time.After(5 * time.Second):
			t.Error("owned Next process did not stop")
		}
	})
	client := &http.Client{Timeout: time.Second}
	ready := false
	for attempt := 0; attempt < 100 && !ready; attempt++ {
		if response, err := client.Get("http://" + uiAddress + "/api/stores"); err == nil {
			_ = response.Body.Close()
			ready = response.StatusCode == http.StatusUnauthorized
		}
		if !ready {
			select {
			case <-ctx.Done():
				t.Fatal("Next readiness deadline")
			case <-time.After(100 * time.Millisecond):
			}
		}
	}
	if !ready {
		t.Fatalf("Next readiness failed; local evidence: %s", evidence)
	}
	browserLogFile := browserLog(t, filepath.Join(evidence, "playwright.log"))
	browser := exec.CommandContext(ctx, "pnpm", "exec", "playwright", "test", "tests/admin/password-auth.spec.ts", "--reporter=list", "--output="+filepath.Join(evidence, "results"))
	browser.Dir = root
	browser.Env = browserEnvironment(map[string]string{
		"LC_BROWSER_SUITE":         "password-auth",
		"LC_BROWSER_PUBLIC_ORIGIN": publicOrigin, "LC_BROWSER_API_ORIGIN": api.URL,
		"LC_BROWSER_INSPECT_ORIGIN": inspect.URL, "LC_BROWSER_EVIDENCE_DIR": evidence,
	})
	browser.Stdout, browser.Stderr = browserLogFile, browserLogFile
	specErr := browser.Run()
	if specErr != nil {
		t.Errorf("browser spec failed; local evidence: %s", evidence)
	}

	// (a) Independent PG proof that the browser flows were real.
	var principals, credentials, sessions, revoked, stores, issued, codes int
	err = f.owner.QueryRow(ctx, `SELECT count(*), count(*) FILTER (WHERE c.principal_id IS NOT NULL) FROM identity.principals p LEFT JOIN identity.password_credentials c ON c.principal_id=p.id WHERE c.email LIKE 'pwa.%@example.test'`).Scan(&principals, &credentials)
	if err != nil || principals < 4 || credentials != principals {
		t.Errorf("password principals created through the browser: %d (credentials %d) err=%v; want at least 4 (chain + BFF checks)", principals, credentials, err)
	}
	if err := f.owner.QueryRow(ctx, `SELECT count(*), count(*) FILTER (WHERE s.revoked_at IS NOT NULL) FROM identity.sessions s JOIN identity.password_credentials c ON c.principal_id=s.principal_id WHERE s.audience='merchant'`).Scan(&sessions, &revoked); err != nil || sessions < 5 || revoked < 2 {
		t.Errorf("merchant sessions=%d revoked=%d err=%v; want >= 5 issued and >= 2 revoked (logout + password reset)", sessions, revoked, err)
	}
	if err := f.owner.QueryRow(ctx, `SELECT count(*) FROM identity.initial_stores i JOIN identity.password_credentials c ON c.principal_id=i.principal_id`).Scan(&stores); err != nil || stores < 1 {
		t.Errorf("onboarding through a password principal created %d stores: %v", stores, err)
	}
	if err := f.owner.QueryRow(ctx, `SELECT count(*) FROM identity.session_events se JOIN identity.password_credentials c ON c.principal_id=se.principal_id WHERE se.action='session.issued'`).Scan(&issued); err != nil || issued < 5 {
		t.Errorf("session.issued events %d: %v", issued, err)
	}
	if err := f.owner.QueryRow(ctx, `SELECT count(*) FROM identity.email_challenges WHERE mail_state='SENT'`).Scan(&codes); err != nil || codes < 12 {
		t.Errorf("SENT challenge mails %d: %v", codes, err)
	}
	if n := e.smtp.DataCount(); n < 12 {
		t.Errorf("SMTP DATA commands %d: every browser step-1 must have sent one mail", n)
	}

	// (b) Canary scan: nothing the spec recorded may appear in any server-side or process log.
	raw, err := os.ReadFile(filepath.Join(evidence, "canaries.txt"))
	if err != nil {
		t.Fatalf("the spec recorded no canaries: %v", err)
	}
	var canaries []string
	for _, line := range strings.Split(string(raw), "\n") {
		if len(strings.TrimSpace(line)) >= 6 {
			canaries = append(canaries, strings.TrimSpace(line))
		}
	}
	if len(canaries) < 20 {
		t.Fatalf("only %d canaries recorded; the scan would be vacuous", len(canaries))
	}
	for name, path := range map[string]string{"admin server log": filepath.Join(evidence, "next.log"), "browser process log": filepath.Join(evidence, "playwright.log")} {
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		for _, c := range canaries {
			if strings.Contains(string(b), c) {
				t.Errorf("%s contains a recorded sentinel (password, code or address)", name)
				break
			}
		}
	}
	captured := logs.String()
	for _, c := range canaries {
		if strings.Contains(captured, c) {
			t.Error("Go log capture contains a recorded sentinel (password, code or address)")
			break
		}
	}
	if strings.Contains(captured, e.smtp.Password) {
		t.Error("Go log capture contains the SMTP secret")
	}
	if specErr == nil {
		t.Logf("PASS: browser -> Next -> Go -> real PG, real SMTP adapter against loopback fake; %d canaries not found in any log; evidence=%s", len(canaries), evidence)
	}
}
