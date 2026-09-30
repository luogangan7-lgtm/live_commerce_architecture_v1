//go:build browser

package foundation_test

import (
	"bytes"
	"context"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// ALG01 wires the admin ledger Playwright suite (ledger.spec.ts, production.spec.ts,
// visual-states.spec.ts) into a gate: real cmd/admin-fixture (disposable PG + Go API on
// 127.0.0.1:18081, 9 sample SKUs), the dev Next fixture adapter on :3100 (the specs hard-code
// that origin) and the packaged production build with identity AND fixture disabled on :3101.
// Evidence label: BROWSER (fixture bearer, no IdP); not a signed-identity or production claim.
func TestBrowserAdminLedgerFixtureChain(t *testing.T) {
	guard := os.Getenv("LC_ADMIN_GUARD_DSN")
	if os.Getenv("LC_BROWSER_ADMIN_LEGACY_ACCEPTANCE") != "1" || os.Getenv("LC_TEST_DATABASE_ALLOWED") != "1" || guard == "" {
		t.Fatal("use scripts/dev/test-local.sh --browser-admin-legacy")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 240*time.Second)
	defer cancel()
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	// Fixed ports: the specs and cmd/admin-fixture hard-code them. Refuse rather than talk to a
	// stranger's server that happens to own one.
	for _, addr := range []string{"127.0.0.1:18081", "127.0.0.1:3100", "127.0.0.1:3101"} {
		if !portFree(addr) {
			t.Fatalf("port %s is busy; stop the other process (this gate never kills by port)", addr)
		}
	}
	evidence := adminEvidence(t, root, "admin-ledger")

	// cmd/admin-fixture wants: an empty 0600 env file and a one-use owner.dsn beside it.
	private := t.TempDir()
	binary := filepath.Join(private, "admin-fixture")
	build := exec.CommandContext(ctx, "go", "build", "-o", binary, "./cmd/admin-fixture")
	build.Dir = root
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("admin-fixture build failed: %s", out)
	}
	envFile := filepath.Join(private, "session.env")
	if err := os.WriteFile(envFile, nil, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(private, "owner.dsn"), []byte(guard), 0600); err != nil {
		t.Fatal(err)
	}
	fixtureLog := browserLog(t, filepath.Join(evidence, "admin-fixture.log"))
	fixture := exec.CommandContext(ctx, binary)
	fixture.Env = browserEnvironment(map[string]string{"COMMERCE_FIXTURE_ALLOWED": "1", "FIXTURE_OWNER_FILE": filepath.Join(private, "owner.dsn"), "FIXTURE_ENV_PATH": envFile, "PATH": os.Getenv("PATH")})
	fixture.Stdout, fixture.Stderr = fixtureLog, fixtureLog
	if err := fixture.Start(); err != nil {
		t.Fatal(err)
	}
	fixtureDone := make(chan error, 1)
	go func() { fixtureDone <- fixture.Wait() }()
	t.Cleanup(func() {
		_ = fixture.Process.Signal(os.Interrupt)
		select {
		case <-fixtureDone:
		case <-time.After(10 * time.Second):
			_ = fixture.Process.Kill()
			<-fixtureDone
		}
	})
	var fixtureEnv map[string]string
	for attempt := 0; attempt < 300 && fixtureEnv == nil; attempt++ {
		if data, err := os.ReadFile(envFile); err == nil && bytes.HasSuffix(data, []byte("\n")) {
			fixtureEnv = map[string]string{}
			for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
				k, v, _ := strings.Cut(line, "=")
				fixtureEnv[k] = v
			}
			break
		}
		select {
		case err := <-fixtureDone:
			t.Fatalf("admin-fixture exited early (%v); evidence: %s", err, evidence)
		case <-ctx.Done():
			t.Fatal("admin-fixture readiness deadline")
		case <-time.After(200 * time.Millisecond):
		}
	}
	if fixtureEnv == nil || fixtureEnv["COMMERCE_FIXTURE_TOKEN"] == "" || fixtureEnv["COMMERCE_FIXTURE_STORE_ID"] == "" {
		t.Fatalf("admin-fixture wrote no session; evidence: %s", evidence)
	}

	// Dev Next with the fixture bearer (NODE_ENV must be development: apps/admin/lib/backend.ts).
	nextEnvPath := filepath.Join(root, "apps/admin/next-env.d.ts")
	nextEnvBefore, err := os.ReadFile(nextEnvPath)
	if err != nil {
		t.Fatal(err)
	}
	devNextEnv := bytes.ReplaceAll(nextEnvBefore, []byte("./.next/types/"), []byte("./.next/dev/types/"))
	t.Cleanup(func() { // same restore rule as the orders/studio BFF gates
		current, err := os.ReadFile(nextEnvPath)
		if err != nil || bytes.Equal(current, nextEnvBefore) {
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
	startBrowserNode(t, ctx, evidence, "dev-next", filepath.Join(root, "apps/admin"),
		[]string{filepath.Join(root, "apps/admin/node_modules/next/dist/bin/next"), "dev", "--hostname", "127.0.0.1", "--port", "3100"},
		map[string]string{"NODE_ENV": "development", "COMMERCE_IDENTITY_ENABLED": "0", "COMMERCE_FIXTURE_ENABLED": "1", "COMMERCE_FIXTURE_ALLOWED": "1",
			"COMMERCE_FIXTURE_TOKEN": fixtureEnv["COMMERCE_FIXTURE_TOKEN"], "COMMERCE_FIXTURE_STORE_ID": fixtureEnv["COMMERCE_FIXTURE_STORE_ID"], "COMMERCE_API_ORIGIN": fixtureEnv["COMMERCE_API_ORIGIN"]})
	// production.spec.ts: packaged build, no identity, no fixture -> fail-closed "not configured".
	startBrowserNode(t, ctx, evidence, "production-next", root, []string{filepath.Join(root, "apps/admin/.next/standalone/apps/admin/server.js")},
		map[string]string{"HOSTNAME": "127.0.0.1", "PORT": "3101", "NODE_ENV": "production"})

	storeURL := "/api/stores/" + fixtureEnv["COMMERCE_FIXTURE_STORE_ID"] + "/warehouses"
	waitHTTP(t, ctx, evidence, "http://127.0.0.1:3100"+storeURL, "127.0.0.1:3100", http.StatusOK)
	waitHTTP(t, ctx, evidence, "http://127.0.0.1:3101/api/stores/00000000-0000-0000-0000-000000000001/products", "", http.StatusUnauthorized)

	runPlaywright(t, ctx, root, evidence, "ledger", map[string]string{
		"COMMERCE_FIXTURE_TOKEN": fixtureEnv["COMMERCE_FIXTURE_TOKEN"], "COMMERCE_FIXTURE_STORE_ID": fixtureEnv["COMMERCE_FIXTURE_STORE_ID"]})
	t.Logf("PASS: admin-fixture PG + Go + dev Next (:3100) + packaged production Next (:3101); evidence=%s", evidence)
}

// startBrowserNode runs one owned Node child (exact PID, never a port/name kill) with the
// scrubbed browserEnvironment, logging to <evidence>/<name>.log.
func startBrowserNode(t *testing.T, ctx context.Context, evidence, name, dir string, args []string, env map[string]string) {
	t.Helper()
	log := browserLog(t, filepath.Join(evidence, name+".log"))
	cmd := exec.CommandContext(ctx, "node", args...)
	cmd.Dir, cmd.Env = dir, browserEnvironment(env)
	// `next dev` forks a next-server child that outlives a signal to its parent and keeps the fixed
	// port; own a process group and signal the whole group (this PID's group only, never by port).
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	cmd.Stdout, cmd.Stderr = log, log
	if err := cmd.Start(); err != nil {
		t.Fatalf("could not start %s", name)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	var once sync.Once
	t.Cleanup(func() {
		once.Do(func() {
			_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM)
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
				<-done
			}
			stopProcessGroup(t, cmd.Process.Pid)
		})
	})
}

// portFree waits up to 5 s for addr to be bindable: the previous gate of this process may have just
// killed its server, and the kernel releases the listening socket asynchronously after the exit.
func portFree(addr string) bool {
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(50 * time.Millisecond) {
		if l, err := net.Listen("tcp", addr); err == nil {
			_ = l.Close()
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
	}
}

// stopProcessGroup (F6) returns only when no process of the group is left: the parent's exit does not
// mean its next-server child has released the fixed port, so the next gate's port check would race it.
func stopProcessGroup(t *testing.T, pgid int) {
	t.Helper()
	_ = syscall.Kill(-pgid, syscall.SIGKILL)
	for deadline := time.Now().Add(5 * time.Second); syscall.Kill(-pgid, 0) == nil; time.Sleep(20 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Errorf("process group %d still alive 5 s after SIGKILL", pgid)
			return
		}
	}
}

// waitHTTP polls url (optionally with a forced Host) until it answers want.
func waitHTTP(t *testing.T, ctx context.Context, evidence, url, host string, want int) {
	t.Helper()
	client := &http.Client{Timeout: 3 * time.Second}
	for attempt := 0; attempt < 300; attempt++ {
		req, _ := http.NewRequestWithContext(ctx, "GET", url, nil)
		if host != "" {
			req.Host = host
		}
		if resp, err := client.Do(req); err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == want {
				return
			}
		}
		select {
		case <-ctx.Done():
			t.Fatalf("readiness deadline for %s; evidence: %s", url, evidence)
		case <-time.After(200 * time.Millisecond):
		}
	}
	t.Fatalf("not ready: %s; evidence: %s", url, evidence)
}

// runPlaywright runs one suite and fails unless the list reporter shows passes and no skip/fail.
// Playwright exits non-zero on zero matched tests; the counts below guard a silent skip.
func runPlaywright(t *testing.T, ctx context.Context, root, evidence, suite string, env map[string]string) {
	t.Helper()
	path := filepath.Join(evidence, "playwright.log")
	logFile := browserLog(t, path)
	env["LC_BROWSER_SUITE"] = suite
	browser := exec.CommandContext(ctx, "pnpm", "exec", "playwright", "test", "--reporter=list", "--output="+filepath.Join(evidence, "results"))
	browser.Dir, browser.Env = root, browserEnvironment(env)
	browser.Stdout, browser.Stderr = logFile, logFile
	if err := browser.Run(); err != nil {
		t.Fatalf("%s browser suite failed; local evidence: %s", suite, evidence)
	}
	out, _ := os.ReadFile(path)
	if !strings.Contains(string(out), " passed") || strings.Contains(string(out), " skipped") || strings.Contains(string(out), " failed") {
		t.Fatalf("%s playwright summary is not all-passed; local evidence: %s", suite, evidence)
	}
}

func adminEvidence(t *testing.T, root, name string) string {
	t.Helper()
	dir := filepath.Join(root, "output", "playwright", name+"-"+time.Now().UTC().Format("20060102T150405.000000000"))
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	return dir
}

// ALG02/ALG03 wire auth.spec.ts (identity-mock) and entry.spec.ts (entry-mock). Each spec runs its
// own MOCK Go API on 127.0.0.1:19111; the only real parts are the packaged Next server (identity
// mode, loopback allowed) and Chromium. Evidence label: BROWSER + MOCK API; no PG, no signed IdP.
func TestBrowserAdminIdentityMock(t *testing.T) { adminMockSuite(t, "identity-mock", false) }
func TestBrowserAdminEntryMock(t *testing.T)    { adminMockSuite(t, "entry-mock", true) }

func adminMockSuite(t *testing.T, suite string, onboarding bool) {
	if os.Getenv("LC_BROWSER_ADMIN_LEGACY_ACCEPTANCE") != "1" {
		t.Fatal("use scripts/dev/test-local.sh --browser-admin-legacy")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Second)
	defer cancel()
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	for _, addr := range []string{"127.0.0.1:19111", "127.0.0.1:3100"} {
		if !portFree(addr) {
			t.Fatalf("port %s is busy; stop the other process (this gate never kills by port)", addr)
		}
	}
	evidence := adminEvidence(t, root, "admin-"+suite)
	env := map[string]string{
		"HOSTNAME": "127.0.0.1", "PORT": "3100", "NODE_ENV": "production",
		"COMMERCE_IDENTITY_ENABLED": "1", "COMMERCE_IDENTITY_ALLOW_LOOPBACK_TESTS": "1",
		"COMMERCE_PUBLIC_ORIGIN": "http://127.0.0.1:3100", "COMMERCE_API_ORIGIN": "http://127.0.0.1:19111",
		"COMMERCE_OIDC_ISSUER": "https://provider.example/realm/", "COMMERCE_BFF_KEY": strings.Repeat("A", 43),
	}
	if onboarding {
		env["COMMERCE_ONBOARDING_ENABLED"], env["COMMERCE_ONBOARDING_CURRENCIES"] = "1", "TWD,USD"
	}
	startBrowserNode(t, ctx, evidence, "next", root, []string{filepath.Join(root, "apps/admin/.next/standalone/apps/admin/server.js")}, env)
	waitHTTP(t, ctx, evidence, "http://127.0.0.1:3100/api/stores", "", http.StatusUnauthorized)
	runPlaywright(t, ctx, root, evidence, suite, map[string]string{})
	t.Logf("PASS: %s; evidence=%s", suite, evidence)
}
