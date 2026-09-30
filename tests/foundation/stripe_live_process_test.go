package foundation_test

// SL06 (contracts/stripe-live-enable-v1.md §11 SL06, §5.2, §7, §9 pair rules, ops brief O1-O3): process + PG + shell.
// Prefix `slx`. Tier labels: PROCESS (built binaries, config refusal proven by whether the database is dialed),
// MOCK+REAL_PG (an in-flight MOCK Stripe attempt closes while the hosted service has no Stripe), SHELL (deploy scripts
// with a stub `docker` that records argv and forwarded NAMES; ops-admin runs inside the pinned PG image because the
// scripts need bash >= 4.4 and GNU stat, exactly like the Linux production host; preflight P06 runs on the host).
//
// How a refusal is told from an acceptance without a database: the built binary is pointed at a test-owned TCP listener.
// A config refusal exits before any pool is opened (zero connections); an accepted config dials it (>= 1 connection) and
// then fails for an unrelated reason. Both directions are asserted for every pair combination.
//
// Fake material: keys are built at run time from split literals ("rk_" + "live_" + ...); DSNs come from net/url with the
// password in a neutrally named constant that leak assertions search for. Every secret-looking value reaches the
// container by env NAME only and never appears in argv, stdout, stderr or the stub log (asserted).

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"livecommerce/internal/checkout"
	"livecommerce/internal/command"
)

const slxDSNSentinel = "slx-dsn-sentinel-7f31c9"
const slxPGImage = "postgres@sha256:4ef4dbc939d61acea57712655ddb4b4ab27419c913f94cca0cd57cb3ea3c2280"
const slxRef = "owner-chat:2026-09-30:stripe-live:slx-fixture"

func slxRepo(t *testing.T) string {
	t.Helper()
	abs, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	return abs
}

func slxBuild(t *testing.T, name string) string {
	t.Helper()
	out := filepath.Join(t.TempDir(), name)
	if b, err := exec.Command("go", "build", "-o", out, "../../cmd/"+name).CombinedOutput(); err != nil {
		t.Fatalf("build cmd/%s: %v %s", name, err, b)
	}
	return out
}

// slxListener counts TCP connections to a loopback port (the "database").
type slxListener struct {
	ln    net.Listener
	count atomic.Int64
	port  string
}

func slxDial(t *testing.T) *slxListener {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	l := &slxListener{ln: ln}
	_, l.port, _ = net.SplitHostPort(ln.Addr().String())
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			l.count.Add(1)
			_ = c.Close()
		}
	}()
	t.Cleanup(func() { _ = ln.Close() })
	return l
}

func (l *slxListener) dsn() string {
	u := url.URL{Scheme: "postgres", User: url.UserPassword("u", slxDSNSentinel), Host: "127.0.0.1:" + l.port, Path: "/d", RawQuery: "sslmode=disable&connect_timeout=2"}
	return u.String()
}

func slxFreePort(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	_, port, _ := net.SplitHostPort(ln.Addr().String())
	return port
}

func slxB64() string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return base64.StdEncoding.EncodeToString(b)
}

// slxRun starts a built binary with exactly env (no inheritance), waits, and returns the exit code and the combined output.
func slxRun(t *testing.T, binary string, env map[string]string) (int, string) {
	t.Helper()
	cmd := exec.Command(binary)
	cmd.Env = []string{"PATH=" + os.Getenv("PATH")}
	for k, v := range env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			return ee.ExitCode(), out.String()
		}
		return 0, out.String()
	case <-time.After(45 * time.Second):
		_ = cmd.Process.Kill()
		<-done
		t.Fatalf("binary did not exit: %s", out.String())
		return -1, ""
	}
}

func TestStripeSL06LiveProcess(t *testing.T) {
	t.Run("worker_refuses_LIVE_without_the_pair", slxWorker)
	t.Run("api_refuses_LIVE_surfaces_without_the_pair", slxAPI)
	t.Run("api_never_reads_STRIPE_secrets", slxAPINeverReadsStripeSecrets)
	t.Run("checkout_switch_removes_stripe_from_hosted_while_an_in_flight_mock_attempt_closes", slxCheckoutSwitch)
	t.Run("compose_wiring", slxCompose)
	t.Run("shell_ops_admin", slxOpsAdmin)
	t.Run("shell_preflight_P06", slxPreflight)
}

// The pair combinations the contract distinguishes (§5.2 "both or neither"); ok = accepted by the config loader.
func slxPairs() []struct {
	name string
	env  map[string]string
	ok   bool
} {
	return []struct {
		name string
		env  map[string]string
		ok   bool
	}{
		{"flag and reference", map[string]string{"COMMERCE_STRIPE_LIVE_ENABLED": "1", "COMMERCE_STRIPE_LIVE_APPROVAL_REF": slxRef}, true},
		{"no pair", map[string]string{}, false},
		{"flag without reference", map[string]string{"COMMERCE_STRIPE_LIVE_ENABLED": "1"}, false},
		{"reference without flag", map[string]string{"COMMERCE_STRIPE_LIVE_APPROVAL_REF": slxRef}, false},
		{"flag 0 with reference", map[string]string{"COMMERCE_STRIPE_LIVE_ENABLED": "0", "COMMERCE_STRIPE_LIVE_APPROVAL_REF": slxRef}, false},
		{"flag true (non-canonical)", map[string]string{"COMMERCE_STRIPE_LIVE_ENABLED": "true", "COMMERCE_STRIPE_LIVE_APPROVAL_REF": slxRef}, false},
		{"malformed reference", map[string]string{"COMMERCE_STRIPE_LIVE_ENABLED": "1", "COMMERCE_STRIPE_LIVE_APPROVAL_REF": "bad ref"}, false},
		{"short reference", map[string]string{"COMMERCE_STRIPE_LIVE_ENABLED": "1", "COMMERCE_STRIPE_LIVE_APPROVAL_REF": "short"}, false},
	}
}

func slxMerge(maps ...map[string]string) map[string]string {
	out := map[string]string{}
	for _, m := range maps {
		for k, v := range m {
			out[k] = v
		}
	}
	return out
}

func slxWorker(t *testing.T) {
	binary := slxBuild(t, "payment-worker")
	db := slxDial(t)
	keys, _ := json.Marshal([]map[string]string{{"id": "k1", "key_base64": slxB64()}})
	sentinels := []string{"sk_" + "test_slx" + t04Tag(), "whsec_" + "slx" + t04Tag(), "acct_slx" + t04Tag(), "rk_" + "live_slx" + t04Tag()}
	base := func(profile, stripe string) map[string]string {
		return map[string]string{"COMMERCE_PAYMENT_WORKER_ENABLED": "1", "COMMERCE_PAYMENT_WORKER_PROFILE": profile, "COMMERCE_PAYMENT_WORKER_CONCURRENCY": "1",
			"COMMERCE_PAYMENT_WORKER_DATABASE_URL": db.dsn(), "COMMERCE_ACCOUNT_ACTIVE_KEY_ID": "k1", "COMMERCE_ACCOUNT_KEYS_JSON": string(keys),
			"COMMERCE_ACCOUNT_REPLAY_KEY": slxB64(), "COMMERCE_STRIPE_ENABLED": stripe,
			// Global Stripe credentials the worker must never read (§0.2): they would leak into output if it did.
			"STRIPE_SECRET_KEY": sentinels[0], "STRIPE_WEBHOOK_SECRET": sentinels[1], "STRIPE_ACCOUNT_ID": sentinels[2], "STRIPE_RESTRICTED_KEY": sentinels[3]}
	}
	for _, p := range slxPairs() {
		before := db.count.Load()
		code, out := slxRun(t, binary, slxMerge(base("LIVE", "1"), p.env))
		dialed := db.count.Load() - before
		if p.ok {
			if dialed == 0 || strings.Contains(out, "payment_worker_invalid_config") {
				t.Errorf("worker LIVE + %s: config refused (dialed=%d) %q", p.name, dialed, out)
			}
		} else if dialed != 0 || code == 0 || !strings.Contains(out, "payment_worker_invalid_config") {
			t.Errorf("worker LIVE + %s: exit=%d dialed=%d out=%q, want a config refusal before any database access", p.name, code, dialed, out)
		}
		for _, s := range append(sentinels, slxDSNSentinel) {
			if strings.Contains(out, s) {
				t.Errorf("worker output echoes a secret (%s)", p.name)
			}
		}
	}
	// The pair is only ever read for LIVE: SANDBOX with Stripe keeps its old behaviour (accepted) and LIVE without Stripe needs
	// no pair (the pair is the owner's approval for Stripe, not for PAYUNi).
	before := db.count.Load()
	if _, out := slxRun(t, binary, base("SANDBOX", "1")); db.count.Load() == before || strings.Contains(out, "invalid_config") {
		t.Errorf("SANDBOX + Stripe was refused: %q", out)
	}
	before = db.count.Load()
	if _, out := slxRun(t, binary, base("LIVE", "0")); db.count.Load() == before || strings.Contains(out, "invalid_config") {
		t.Errorf("LIVE with Stripe disabled was refused (no pair needed): %q", out)
	}
	// Non-canonical Stripe flag is refused whatever the profile.
	before = db.count.Load()
	if code, out := slxRun(t, binary, base("SANDBOX", "true")); code == 0 || db.count.Load() != before || !strings.Contains(out, "invalid_config") {
		t.Errorf("non-canonical COMMERCE_STRIPE_ENABLED accepted: %d %q", code, out)
	}
}

func slxAPI(t *testing.T) {
	binary := slxBuild(t, "api")
	db := slxDial(t)
	port := slxFreePort(t)
	keys, _ := json.Marshal([]map[string]string{{"id": "k1", "key_base64": slxB64()}})
	bff := strings.TrimRight(base64.URLEncoding.EncodeToString([]byte(slxB64()[:32])), "=")
	bff = bff[:43]
	webhook := map[string]string{"DATABASE_URL": db.dsn(), "LISTEN_ADDR": "127.0.0.1:" + port, "COMMERCE_STRIPE_WEBHOOK_ENABLED": "1",
		"COMMERCE_STRIPE_INGRESS_DATABASE_URL": db.dsn(), "COMMERCE_STRIPE_WEBHOOK_ACTIVE_KEY_ID": "k1", "COMMERCE_STRIPE_WEBHOOK_KEYS_JSON": string(keys),
		"COMMERCE_STRIPE_WEBHOOK_REPLAY_KEY": slxB64()}
	buyer := map[string]string{"DATABASE_URL": db.dsn(), "LISTEN_ADDR": "127.0.0.1:" + port, "COMMERCE_BUYER_ENABLED": "1", "COMMERCE_BUYER_ISSUER_DATABASE_URL": db.dsn(),
		"COMMERCE_BUYER_DATABASE_URL": db.dsn(), "COMMERCE_CHECKOUT_DATABASE_URL": db.dsn(), "COMMERCE_BUYER_BFF_KEY": bff, "COMMERCE_BUYER_SESSION_TTL": "1h",
		"COMMERCE_BUYER_PAYMENT_ENABLED": "1", "COMMERCE_HOSTED_DATABASE_URL": db.dsn(), "COMMERCE_PAYMENT_RETURN_URL": "https://shop.example.test/payment/return",
		"COMMERCE_PAYMENT_NOTIFY_URL": "https://api.example.test/v1/payment/notify", "COMMERCE_ACCOUNT_ACTIVE_KEY_ID": "k1", "COMMERCE_ACCOUNT_KEYS_JSON": string(keys),
		"COMMERCE_ACCOUNT_REPLAY_KEY": slxB64()}
	check := func(what string, env map[string]string, wantAccepted bool) {
		t.Helper()
		before := db.count.Load()
		code, out := slxRun(t, binary, env)
		dialed := db.count.Load() - before
		if code == 0 {
			t.Errorf("%s: the API exited 0 without a database", what)
		}
		if wantAccepted && dialed == 0 {
			t.Errorf("%s: config refused (no database dial), want accepted: %q", what, out)
		}
		if !wantAccepted && dialed != 0 {
			t.Errorf("%s: config accepted (%d dials), want a refusal before any database access: %q", what, dialed, out)
		}
		if strings.Contains(out, slxDSNSentinel) || strings.Contains(out, slxRef) {
			t.Errorf("%s: the API output contains the DSN or the approval reference", what)
		}
	}
	for _, p := range slxPairs() {
		check("webhook ingress on LIVE, "+p.name, slxMerge(webhook, map[string]string{"COMMERCE_PAYMENT_PROFILE": "LIVE"}, p.env), p.ok)
		check("buyer Stripe checkout on LIVE, "+p.name, slxMerge(buyer, map[string]string{"COMMERCE_PAYMENT_PROFILE": "LIVE", "COMMERCE_STRIPE_CHECKOUT_ENABLED": "1"}, p.env), p.ok)
	}
	// SANDBOX behaviour is unchanged (accepted without any pair).
	check("webhook ingress on SANDBOX", slxMerge(webhook, map[string]string{"COMMERCE_PAYMENT_PROFILE": "SANDBOX"}), true)
	check("buyer Stripe checkout on SANDBOX", slxMerge(buyer, map[string]string{"COMMERCE_PAYMENT_PROFILE": "SANDBOX", "COMMERCE_STRIPE_CHECKOUT_ENABLED": "1"}), true)
	// LD6: the platform switch. With COMMERCE_STRIPE_CHECKOUT_ENABLED=0 (compose LC_STRIPE_CHECKOUT_ENABLED=0) a LIVE
	// deployment needs no pair for the hosted service (Stripe is simply absent from it); unset behaves the same.
	check("buyer LIVE, Stripe checkout switched off, no pair", slxMerge(buyer, map[string]string{"COMMERCE_PAYMENT_PROFILE": "LIVE", "COMMERCE_STRIPE_CHECKOUT_ENABLED": "0"}), true)
	check("buyer LIVE, Stripe checkout flag unset, no pair", slxMerge(buyer, map[string]string{"COMMERCE_PAYMENT_PROFILE": "LIVE"}), true)
	// An unknown payment profile is refused by the webhook ingress and the hosted service.
	check("webhook ingress, unknown profile", slxMerge(webhook, map[string]string{"COMMERCE_PAYMENT_PROFILE": "PRODUCTION"}, slxPairs()[0].env), false)
	check("buyer payment, unknown profile", slxMerge(buyer, map[string]string{"COMMERCE_PAYMENT_PROFILE": "PRODUCTION", "COMMERCE_STRIPE_CHECKOUT_ENABLED": "1"}, slxPairs()[0].env), false)
}

func slxAPINeverReadsStripeSecrets(t *testing.T) {
	// SP15 carried into LIVE: cmd/api never reads STRIPE_* variables (keys live in PG under the operator registrar).
	// (1) static: no non-test source of cmd/api reads a STRIPE_ variable.
	read := regexp.MustCompile(`(?i)(getenv|lookupenv|environ|expandenv)\s*\(\s*"STRIPE_`)
	files, err := filepath.Glob(filepath.Join("..", "..", "cmd", "api", "*.go"))
	if err != nil || len(files) == 0 {
		t.Fatalf("cmd/api sources: %v %v", files, err)
	}
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		raw, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		if read.Match(raw) {
			t.Errorf("%s reads a STRIPE_ variable", f)
		}
	}
	// (2) process: a LIVE API configured with the pair and holding STRIPE_* sentinels never echoes them.
	binary := slxBuild(t, "api")
	db := slxDial(t)
	keys, _ := json.Marshal([]map[string]string{{"id": "k1", "key_base64": slxB64()}})
	sentinels := []string{"sk_" + "live_slx" + t04Tag(), "rk_" + "live_slx" + t04Tag(), "whsec_" + "slx" + t04Tag()}
	env := map[string]string{"DATABASE_URL": db.dsn(), "LISTEN_ADDR": "127.0.0.1:" + slxFreePort(t), "COMMERCE_STRIPE_WEBHOOK_ENABLED": "1",
		"COMMERCE_STRIPE_INGRESS_DATABASE_URL": db.dsn(), "COMMERCE_PAYMENT_PROFILE": "LIVE", "COMMERCE_STRIPE_WEBHOOK_ACTIVE_KEY_ID": "k1",
		"COMMERCE_STRIPE_WEBHOOK_KEYS_JSON": string(keys), "COMMERCE_STRIPE_WEBHOOK_REPLAY_KEY": slxB64(),
		"COMMERCE_STRIPE_LIVE_ENABLED": "1", "COMMERCE_STRIPE_LIVE_APPROVAL_REF": slxRef,
		"STRIPE_SECRET_KEY": sentinels[0], "STRIPE_RESTRICTED_KEY": sentinels[1], "STRIPE_WEBHOOK_SECRET": sentinels[2]}
	_, out := slxRun(t, binary, env)
	for _, s := range append(sentinels, slxDSNSentinel) {
		if strings.Contains(out, s) {
			t.Fatal("the API output contains a STRIPE_* sentinel or the DSN")
		}
	}
	if db.count.Load() == 0 {
		t.Fatal("control failed: the LIVE API with the pair never reached the database dial")
	}
}

func slxCheckoutSwitch(t *testing.T) {
	// LD6: removing Stripe from the hosted service (COMMERCE_STRIPE_CHECKOUT_ENABLED=0 builds HostedProviders without Stripe)
	// refuses new Stripe starts and leaves the worker + webhook path untouched: an attempt started before the switch
	// (a MOCK in-flight attempt) still pins, is paid and closes CAPTURED through worker + signed webhook.
	e := sflNew(t, nil)
	s := e.stripeStore(t)
	endpoint, secret := e.endpoint(t, s)
	e.start(t, true)
	res, session := e.pinned(t, s)
	noStripe, err := checkout.NewHostedPaymentService(context.Background(), e.hosted, e.jobs, "PROVIDER_MOCK", e.keys, checkout.HostedProviders{PAYUNi: &e.pcfg})
	if err != nil {
		t.Fatalf("hosted service without Stripe: %v", err)
	}
	other := s
	other.p = sstMoreHold(t, s.p)
	before := countRows(t, e.f.owner, `SELECT count(*) FROM checkout.payment_attempts WHERE owner_id=$1`, other.p.cap.Scope.OwnerID)
	if _, err := other.begin(noStripe, t04Key("slx-nostripe"), other.input("zh-TW")); err == nil {
		t.Fatal("a Stripe start was accepted by a hosted service built without Stripe")
	} else if !errors.Is(err, command.ErrInvalid) && !errors.Is(err, command.ErrConflict) {
		t.Fatalf("Stripe start without Stripe: %v", err)
	}
	if n := countRows(t, e.f.owner, `SELECT count(*) FROM checkout.payment_attempts WHERE owner_id=$1`, other.p.cap.Scope.OwnerID); n != before {
		t.Fatalf("the refused start left %d attempts", n-before)
	}
	// The in-flight attempt still closes.
	if !e.fake.SetState(session, "complete", "paid") {
		t.Fatal("fake pay")
	}
	if status := e.deliver(t, endpoint, secret, sflEvent(res.AttemptID, session)); status != 200 {
		t.Fatalf("webhook answered %d", status)
	}
	e.awaitFact(t, res.AttemptID, "CAPTURED")
}

// slxServices splits compose.yml into its top-level services (2-space keys under `services:`).
func slxServices(t *testing.T) map[string]string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "deploy", "compose.yml"))
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]string{}
	var name string
	inServices := false
	head := regexp.MustCompile(`^  ([a-z0-9-]+):\s*$`)
	for _, line := range strings.Split(string(raw), "\n") {
		if strings.HasPrefix(line, "services:") {
			inServices = true
			continue
		}
		if inServices && line != "" && !strings.HasPrefix(line, " ") && !strings.HasPrefix(line, "#") {
			inServices = false
		}
		if !inServices {
			continue
		}
		if m := head.FindStringSubmatch(line); m != nil {
			name = m[1]
			continue
		}
		if name != "" {
			out[name] += line + "\n"
		}
	}
	return out
}

func slxCompose(t *testing.T) {
	services := slxServices(t)
	line := func(service, key string) string {
		re := regexp.MustCompile(`(?m)^\s+` + regexp.QuoteMeta(key) + `:\s*(.+?)\s*$`)
		if m := re.FindStringSubmatch(services[service]); m != nil {
			return strings.Trim(m[1], `"'`)
		}
		return ""
	}
	// §5.2/LD6: two switches. The buyer checkout switch has its own compose source, defaulting to LC_STRIPE_ENABLED.
	if got := line("api", "COMMERCE_STRIPE_CHECKOUT_ENABLED"); got != "${LC_STRIPE_CHECKOUT_ENABLED:-${LC_STRIPE_ENABLED:-0}}" {
		t.Errorf("api COMMERCE_STRIPE_CHECKOUT_ENABLED=%q, want ${LC_STRIPE_CHECKOUT_ENABLED:-${LC_STRIPE_ENABLED:-0}}", got)
	}
	// The pair reaches api and payment-worker-live from the compose.env keys.
	for _, svc := range []string{"api", "payment-worker-live"} {
		if got := line(svc, "COMMERCE_STRIPE_LIVE_ENABLED"); got != "${LC_STRIPE_LIVE_ENABLED:-0}" {
			t.Errorf("%s COMMERCE_STRIPE_LIVE_ENABLED=%q, want ${LC_STRIPE_LIVE_ENABLED:-0}", svc, got)
		}
		if got := line(svc, "COMMERCE_STRIPE_LIVE_APPROVAL_REF"); got != "${LC_STRIPE_LIVE_APPROVAL_REF:-}" {
			t.Errorf("%s COMMERCE_STRIPE_LIVE_APPROVAL_REF=%q, want ${LC_STRIPE_LIVE_APPROVAL_REF:-}", svc, got)
		}
	}
	// payment-worker-live now receives COMMERCE_STRIPE_ENABLED (today it never did); worker + webhook keep following
	// LC_STRIPE_ENABLED, never the checkout switch, so LC_STRIPE_CHECKOUT_ENABLED=0 leaves them running.
	if got := line("payment-worker-live", "COMMERCE_STRIPE_ENABLED"); got != "${LC_STRIPE_ENABLED:-0}" {
		t.Errorf("payment-worker-live COMMERCE_STRIPE_ENABLED=%q, want ${LC_STRIPE_ENABLED:-0}", got)
	}
	if got := line("api", "COMMERCE_STRIPE_WEBHOOK_ENABLED"); !strings.Contains(got, "LC_STRIPE_ENABLED") || strings.Contains(got, "LC_STRIPE_CHECKOUT_ENABLED") {
		t.Errorf("api COMMERCE_STRIPE_WEBHOOK_ENABLED=%q must follow LC_STRIPE_ENABLED only", got)
	}
	for _, svc := range []string{"payment-worker-live", "payment-worker-sandbox"} {
		if services[svc] == "" {
			continue
		}
		if strings.Contains(services[svc], "LC_STRIPE_CHECKOUT_ENABLED") {
			t.Errorf("%s reads the platform checkout switch: stopping checkout must not stop the worker", svc)
		}
	}
	// The stripe-admin service does not get the pair from compose (ops-admin forwards it by name).
	if strings.Contains(services["stripe-admin"], "COMMERCE_STRIPE_LIVE") {
		t.Error("stripe-admin wires the LIVE pair in compose; ops-admin forwards it by name")
	}
	// PAYUNi stays off in a LIVE deployment: no LIVE key or Stripe secret is ever a compose value.
	raw, _ := os.ReadFile(filepath.Join("..", "..", "deploy", "compose.yml"))
	if regexp.MustCompile(`(?i)(sk|rk)_live_[A-Za-z0-9]`).Match(raw) {
		t.Error("compose.yml contains a live-key-shaped value")
	}
	names := make([]string, 0, len(services))
	for n := range services {
		names = append(names, n)
	}
	sort.Strings(names)
	if len(names) < 5 {
		t.Fatalf("compose service parsing failed: %v", names)
	}
}

// ---------------------------------------------------------------------------------------------------------------------
// Shell half: ops-admin.sh in the pinned Linux image
// ---------------------------------------------------------------------------------------------------------------------

// slxContainer runs a bash script (stdin) in the pinned PG image with the repo's deploy dir read-only at /repo/deploy.
// envNames are forwarded by NAME only. network "" = none, else container:<name> (shares that container's netns).
func slxContainer(t *testing.T, script string, envNames []string, extra []string, network string) (string, string, int) {
	t.Helper()
	repo := slxRepo(t)
	args := []string{"run", "--rm", "--pull=never", "-i", "-v", filepath.Join(repo, "deploy") + ":/repo/deploy:ro"}
	if network == "" {
		args = append(args, "--network", "none")
	} else {
		args = append(args, "--network", "container:"+network)
	}
	for _, n := range envNames {
		args = append(args, "-e", n)
	}
	args = append(args, "--entrypoint", "bash", slxPGImage, "-s")
	cmd := exec.Command("docker", args...)
	cmd.Env = append(os.Environ(), extra...)
	cmd.Stdin = strings.NewReader(script)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	rc := 0
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		rc = ee.ExitCode()
	} else if err != nil {
		t.Fatalf("docker run: %v %s", err, stderr.String())
	}
	return stdout.String(), stderr.String(), rc
}

// slxCase is one ops-admin invocation: the compose.env pair variant, the caller env, files to prepare and the argv.
type slxCase struct {
	name string
	pair string   // none | ok | flag | ref | badref | zero | env-only
	env  []string // caller env assignments (inside the container, NAME=value with value from forwarded vars)
	prep string   // bash run before the case (files)
	args string
}

type slxResult struct {
	rc               int
	stdout, stderr   string
	stub             []string
	matchKey, matchW bool
}

func slxParse(t *testing.T, out string) map[string]*slxResult {
	t.Helper()
	res := map[string]*slxResult{}
	var cur *slxResult
	for _, line := range strings.Split(out, "\n") {
		switch {
		case strings.HasPrefix(line, "@@CASE "):
			var name string
			var rc int
			rest := strings.TrimPrefix(line, "@@CASE ")
			i := strings.LastIndex(rest, " rc=")
			name = rest[:i]
			fmt.Sscanf(rest[i+4:], "%d", &rc)
			cur = &slxResult{rc: rc}
			res[name] = cur
		case cur != nil && strings.HasPrefix(line, "@@ERR "):
			cur.stderr += strings.TrimPrefix(line, "@@ERR ") + "\n"
		case cur != nil && strings.HasPrefix(line, "@@OUT "):
			cur.stdout += strings.TrimPrefix(line, "@@OUT ") + "\n"
		case cur != nil && strings.HasPrefix(line, "@@STUB "):
			cur.stub = append(cur.stub, strings.TrimPrefix(line, "@@STUB "))
		}
	}
	return res
}

const slxOpsDriver = `set -u
W=$(mktemp -d)
mkdir -p "$W/bin" "$W/cfg" "$W/state" "$W/files"
STUBLOG="$W/stub.log"
cat >"$W/bin/docker" <<'STUB'
#!/usr/bin/env bash
# Test stub: records argv (names only, never values) and whether the forwarded variables carry the expected values.
{ printf 'ARGV'; for a in "$@"; do printf ' %s' "$a"; done; printf '\n'; } >>"$STUBLOG"
[ -n "${STRIPE_SECRET_KEY:-}" ] && { [ "${STRIPE_SECRET_KEY}" = "${EXPECT_KEY:-x}" ] && echo 'ENVMATCH STRIPE_SECRET_KEY=1' || echo 'ENVMATCH STRIPE_SECRET_KEY=0'; } >>"$STUBLOG"
[ -n "${STRIPE_WEBHOOK_SECRET:-}" ] && { [ "${STRIPE_WEBHOOK_SECRET}" = "${EXPECT_WHSEC:-x}" ] && echo 'ENVMATCH STRIPE_WEBHOOK_SECRET=1' || echo 'ENVMATCH STRIPE_WEBHOOK_SECRET=0'; } >>"$STUBLOG"
[ -n "${COMMERCE_STRIPE_LIVE_ENABLED:-}" ] && echo "ENVVALUE COMMERCE_STRIPE_LIVE_ENABLED=${COMMERCE_STRIPE_LIVE_ENABLED}" >>"$STUBLOG"
[ -n "${COMMERCE_STRIPE_LIVE_APPROVAL_REF:-}" ] && echo "ENVVALUE COMMERCE_STRIPE_LIVE_APPROVAL_REF=${COMMERCE_STRIPE_LIVE_APPROVAL_REF}" >>"$STUBLOG"
exit 0
STUB
chmod +x "$W/bin/docker"
REF=$SLX_REF
write_compose() {
  { echo 'COMPOSE_PROJECT_NAME=slx'; echo 'COMPOSE_PROFILES=db,app,payments-live'; echo 'LC_ENVIRONMENT=smoke'
    case "$1" in
      ok) echo 'LC_STRIPE_LIVE_ENABLED=1'; echo "LC_STRIPE_LIVE_APPROVAL_REF=$REF" ;;
      flag) echo 'LC_STRIPE_LIVE_ENABLED=1' ;;
      ref) echo "LC_STRIPE_LIVE_APPROVAL_REF=$REF" ;;
      zero) echo 'LC_STRIPE_LIVE_ENABLED=0'; echo "LC_STRIPE_LIVE_APPROVAL_REF=$REF" ;;
      badref) echo 'LC_STRIPE_LIVE_ENABLED=1'; echo 'LC_STRIPE_LIVE_APPROVAL_REF=bad ref' ;;
      shortref) echo 'LC_STRIPE_LIVE_ENABLED=1'; echo 'LC_STRIPE_LIVE_APPROVAL_REF=short' ;;
    esac; } >"$W/cfg/compose.env"
}
mkfile() { # name mode contentvar
  local f="$W/files/$1"; rm -f "$f"; printf '%s\n' "${!3}" >"$f"; chmod "$2" "$f"
}
run() { # name pair envassignments... -- args...
  local name=$1 pair=$2; shift 2
  local envs=()
  while [ "$1" != "--" ]; do envs+=("$1"); shift; done; shift
  write_compose "$pair"
  : >"$STUBLOG"
  ( env -i PATH="$W/bin:/usr/bin:/bin" HOME="$W" LC_COMPOSE_ENV="$W/cfg/compose.env" LC_CONFIG_DIR="$W/cfg" LC_STATE_DIR="$W/state" \
      EXPECT_KEY="${RKLIVE:-}" EXPECT_WHSEC="${WHSEC:-}" STUBLOG="$STUBLOG" ${envs[@]+"${envs[@]}"} bash /repo/deploy/scripts/ops-admin.sh "$@" ) >"$W/out" 2>"$W/err"
  local rc=$?
  printf '@@CASE %s rc=%s\n' "$name" "$rc"
  sed 's/^/@@ERR /' "$W/err"; sed 's/^/@@OUT /' "$W/out"; sed 's/^/@@STUB /' "$STUBLOG"
}
`

func slxOpsAdmin(t *testing.T) {
	tag := t04Tag() + t04Tag()
	rkLive := "rk_" + "live_" + tag
	skLive := "sk_" + "live_" + tag
	rkTest := "rk_" + "test_" + tag
	skTest := "sk_" + "test_" + tag
	whsec := "whsec_" + tag
	acct := "acct_SlxFixture01"
	env := []string{"SLX_REF=" + slxRef, "RKLIVE=" + rkLive, "SKLIVE=" + skLive, "RKTEST=" + rkTest, "SKTEST=" + skTest, "WHSEC=" + whsec}
	names := []string{"SLX_REF", "RKLIVE", "SKLIVE", "RKTEST", "SKTEST", "WHSEC"}

	var b strings.Builder
	b.WriteString(slxOpsDriver)
	add := func(prep, call string) { b.WriteString(prep + "\n" + call + "\n") }
	live := `STRIPE_SECRET_KEY_FILE="$W/files/%s" STRIPE_ACCOUNT_ID=` + acct
	reg := func(name, file string) {
		add("", fmt.Sprintf(`run %s ok `+live+` -- stripe-admin register --environment LIVE`, name, file))
	}
	// --- _FILE (O1) with the LIVE pair set --------------------------------------------------------------------------------
	add(`mkfile ok600 600 RKLIVE; mkfile ok400 400 RKLIVE`, ``)
	reg("file_valid_0600", "ok600")
	reg("file_valid_0400", "ok400")
	add(`mkfile g640 640 RKLIVE; mkfile w644 644 RKLIVE; mkfile g660 660 RKLIVE; mkfile x700 700 RKLIVE; mkfile x000 000 RKLIVE; ln -sf "$W/files/ok600" "$W/files/link"`, ``)
	reg("file_group_readable_0640", "g640")
	reg("file_world_readable_0644", "w644")
	reg("file_group_writable_0660", "g660")
	reg("file_mode_0700", "x700")
	reg("file_symlink", "link")
	add(`mkfile chown 600 RKLIVE; chown 1234 "$W/files/chown"`, ``)
	reg("file_wrong_owner", "chown")
	add(`printf '%s\n%s\n' "$RKLIVE" "$RKLIVE" >"$W/files/two"; chmod 600 "$W/files/two"; printf '%s\n\n' "$RKLIVE" >"$W/files/blank2"; chmod 600 "$W/files/blank2"; : >"$W/files/empty"; chmod 600 "$W/files/empty"; printf '%s' "$RKLIVE" >"$W/files/nonl"; chmod 600 "$W/files/nonl"`, ``)
	reg("file_multi_line", "two")
	reg("file_trailing_blank_line", "blank2")
	reg("file_empty", "empty")
	reg("file_no_trailing_newline_is_one_line", "nonl")
	add(`mkfile sk600 600 SKLIVE; mkfile rkt600 600 RKTEST`, ``)
	reg("file_sk_live_key", "sk600")
	reg("file_test_key_under_LIVE_pair", "rkt600")
	add("", `run file_relative_path ok STRIPE_SECRET_KEY_FILE=files/ok600 STRIPE_ACCOUNT_ID=`+acct+` -- stripe-admin register --environment LIVE`)
	add("", `run file_missing ok STRIPE_SECRET_KEY_FILE="$W/files/none" STRIPE_ACCOUNT_ID=`+acct+` -- stripe-admin register --environment LIVE`)
	add("", `run file_directory ok STRIPE_SECRET_KEY_FILE="$W/files" STRIPE_ACCOUNT_ID=`+acct+` -- stripe-admin register --environment LIVE`)
	add("", `run file_and_value_both ok STRIPE_SECRET_KEY_FILE="$W/files/ok600" STRIPE_SECRET_KEY="$RKLIVE" STRIPE_ACCOUNT_ID=`+acct+` -- stripe-admin register --environment LIVE`)
	add(`mkfile w600 600 WHSEC`, `run webhook_file_valid ok STRIPE_WEBHOOK_SECRET_FILE="$W/files/w600" -- stripe-admin webhook --profile LIVE`)
	add(`mkfile w644b 644 WHSEC`, `run webhook_file_world_readable ok STRIPE_WEBHOOK_SECRET_FILE="$W/files/w644b" -- stripe-admin webhook --profile LIVE`)
	add("", `run xtrace_does_not_trace_the_secret ok SHELLOPTS=xtrace STRIPE_SECRET_KEY_FILE="$W/files/ok600" STRIPE_ACCOUNT_ID=`+acct+` -- stripe-admin register --environment LIVE`)
	// --- LIVE refused without the pair, every spelling ----------------------------------------------------------------------
	for _, c := range []struct{ pair, label string }{{"none", "no_pair"}, {"flag", "flag_only"}, {"ref", "ref_only"}, {"zero", "flag_zero"}, {"badref", "bad_ref"}, {"shortref", "short_ref"}} {
		for _, a := range []struct{ name, args string }{
			{"register_environment_LIVE", "stripe-admin register --environment LIVE"},
			{"register_environment_eq_LIVE", "stripe-admin register --environment=LIVE"},
			{"register_environment_lowercase", "stripe-admin register --environment live"},
			{"rotate_environment_LIVE", "stripe-admin rotate --environment LIVE"},
			{"webhook_profile_LIVE", "stripe-admin webhook --profile LIVE"},
			{"qualify_profile_eq_LIVE", "stripe-admin qualify --profile=LIVE"},
			{"live_approve", "stripe-admin live-approve --approval x"},
			{"live_canary", "stripe-admin live-canary --approval x"},
		} {
			add("", fmt.Sprintf(`run refused_%s_%s %s STRIPE_SECRET_KEY="$RKLIVE" STRIPE_ACCOUNT_ID=%s STRIPE_WEBHOOK_SECRET="$WHSEC" -- %s`, c.label, a.name, c.pair, acct, a.args))
		}
	}
	// --- admitted with the pair ---------------------------------------------------------------------------------------------
	for _, a := range []struct{ name, args string }{
		{"live_approve", "stripe-admin live-approve --approval x --currency TWD"},
		{"live_canary", "stripe-admin live-canary --approval x --attempt y --refund z"},
		{"live_revoke", "stripe-admin live-revoke --approval x --revoke-ref r"},
		{"method_disable", "stripe-admin method --enabled=false"},
		{"method_enable", "stripe-admin method --enabled=true"},
		{"qualify_live", "stripe-admin qualify --profile LIVE"},
	} {
		add("", fmt.Sprintf(`run pair_%s ok -- %s`, a.name, a.args))
	}
	// --- the kill switch never needs the pair (LD6) -------------------------------------------------------------------------
	for _, c := range []string{"none", "flag", "ref", "badref"} {
		add("", fmt.Sprintf(`run killswitch_%s_live_revoke %s -- stripe-admin live-revoke --approval x --revoke-ref r`, c, c))
		add("", fmt.Sprintf(`run killswitch_%s_method_disable %s -- stripe-admin method --enabled=false`, c, c))
		add("", fmt.Sprintf(`run killswitch_%s_method_any_flags %s -- stripe-admin method --enabled=true --max 5000`, c, c))
	}
	// --- allowlist: an unknown subcommand is still refused by usage() ------------------------------------------------------
	add("", `run unknown_subcommand ok -- stripe-admin frobnicate --x`)
	add("", `run unknown_live_subcommand ok -- stripe-admin live-frobnicate`)
	add("", `run unknown_tool ok -- other-admin register`)
	add("", `run missing_subcommand ok -- stripe-admin`)
	add("", `run meta_admin_unchanged none META_PAGE_ACCESS_TOKEN=abcdefghijklmnopqrstuvwxyz -- meta-admin page-token`)
	// --- SANDBOX behaviour unchanged, and the caller's COMMERCE_* pair is never trusted ------------------------------------------
	add("", `run sandbox_register_value none STRIPE_SECRET_KEY="$SKTEST" STRIPE_ACCOUNT_ID=`+acct+` -- stripe-admin register --environment SANDBOX`)
	add("", `run sandbox_register_default_env none STRIPE_SECRET_KEY="$RKTEST" STRIPE_ACCOUNT_ID=`+acct+` -- stripe-admin register`)
	add("", `run sandbox_rejects_live_key_value none STRIPE_SECRET_KEY="$RKLIVE" STRIPE_ACCOUNT_ID=`+acct+` -- stripe-admin register --environment SANDBOX`)
	add("", `run sandbox_rejects_sk_live_value none STRIPE_SECRET_KEY="$SKLIVE" STRIPE_ACCOUNT_ID=`+acct+` -- stripe-admin register --environment SANDBOX`)
	add("", `run live_rejects_sk_live_value ok STRIPE_SECRET_KEY="$SKLIVE" STRIPE_ACCOUNT_ID=`+acct+` -- stripe-admin register --environment LIVE`)
	add("", `run live_rejects_test_key_value ok STRIPE_SECRET_KEY="$SKTEST" STRIPE_ACCOUNT_ID=`+acct+` -- stripe-admin register --environment LIVE`)
	// r2 close: on a pair host a register/rotate without --environment LIVE defaulted to SANDBOX and failed mid-incident
	// with a mismatch inside the container; ops-admin now names the missing flag before any container starts.
	add("", `run pair_rotate_without_environment ok STRIPE_SECRET_KEY="$RKLIVE" STRIPE_ACCOUNT_ID=`+acct+` -- stripe-admin rotate --tenant t --connection c --expected-version 1`)
	add("", `run pair_register_without_environment ok STRIPE_SECRET_KEY="$RKLIVE" STRIPE_ACCOUNT_ID=`+acct+` -- stripe-admin register --tenant t`)
	add("", `run pair_rotate_environment_LIVE ok STRIPE_SECRET_KEY="$RKLIVE" STRIPE_ACCOUNT_ID=`+acct+` -- stripe-admin rotate --environment LIVE --tenant t --connection c --expected-version 1`)
	add("", `run caller_env_pair_is_not_trusted env-only COMMERCE_STRIPE_LIVE_ENABLED=1 COMMERCE_STRIPE_LIVE_APPROVAL_REF="$REF" -- stripe-admin live-approve --approval x`)
	add("", `run caller_env_pair_is_not_forwarded none COMMERCE_STRIPE_LIVE_ENABLED=1 COMMERCE_STRIPE_LIVE_APPROVAL_REF="$REF" -- stripe-admin method --enabled=false`)

	stdout, stderr, rc := slxContainer(t, b.String(), names, env, "")
	if rc != 0 {
		t.Fatalf("driver failed: rc=%d %s", rc, stderr)
	}
	res := slxParse(t, stdout)
	get := func(name string) *slxResult {
		r := res[name]
		if r == nil {
			t.Fatalf("case %s did not run (driver output %d bytes)", name, len(stdout))
		}
		return r
	}
	stubHas := func(r *slxResult, sub string) bool {
		for _, l := range r.stub {
			if strings.Contains(l, sub) {
				return true
			}
		}
		return false
	}
	admitted := func(name string) *slxResult {
		t.Helper()
		r := get(name)
		if r.rc != 0 || len(r.stub) == 0 || !stubHas(r, "ARGV compose") || !stubHas(r, " run --rm --no-deps -T ") {
			t.Errorf("%s: rc=%d stderr=%q stub=%v, want admitted (one-shot container started)", name, r.rc, r.stderr, r.stub)
		}
		return r
	}
	refused := func(name, fragment string) {
		t.Helper()
		r := get(name)
		if r.rc == 0 || len(r.stub) != 0 {
			t.Errorf("%s: rc=%d stub=%v, want refused before any container", name, r.rc, r.stub)
		}
		if fragment != "" && !strings.Contains(r.stderr, fragment) {
			t.Errorf("%s: stderr %q lacks %q", name, r.stderr, fragment)
		}
	}
	// files (O1)
	r := admitted("file_valid_0600")
	if !stubHas(r, "ENVMATCH STRIPE_SECRET_KEY=1") || !stubHas(r, "-e STRIPE_SECRET_KEY") || !stubHas(r, "-e STRIPE_ACCOUNT_ID") {
		t.Errorf("file_valid_0600: the file value must reach the one-shot container by env name: %v", r.stub)
	}
	admitted("file_valid_0400")
	admitted("file_no_trailing_newline_is_one_line")
	for _, name := range []string{"file_group_readable_0640", "file_world_readable_0644", "file_group_writable_0660", "file_mode_0700", "file_symlink", "file_wrong_owner",
		"file_multi_line", "file_trailing_blank_line", "file_empty", "file_relative_path", "file_missing", "file_directory", "file_and_value_both", "file_test_key_under_LIVE_pair",
		"webhook_file_world_readable"} {
		refused(name, "STRIPE_")
	}
	refused("file_sk_live_key", "stripe_live_key_unrestricted")
	refused("live_rejects_sk_live_value", "stripe_live_key_unrestricted")
	refused("live_rejects_test_key_value", "")
	refused("pair_rotate_without_environment", "stripe_live_environment_required")
	refused("pair_register_without_environment", "stripe_live_environment_required")
	admitted("pair_rotate_environment_LIVE")
	rw := admitted("webhook_file_valid")
	if !stubHas(rw, "ENVMATCH STRIPE_WEBHOOK_SECRET=1") || !stubHas(rw, "-e STRIPE_WEBHOOK_SECRET") {
		t.Errorf("webhook_file_valid: %v", rw.stub)
	}
	admitted("xtrace_does_not_trace_the_secret")
	// LIVE without the full pair, every spelling
	for _, c := range []string{"no_pair", "flag_only", "ref_only", "flag_zero", "bad_ref", "short_ref"} {
		for _, a := range []string{"register_environment_LIVE", "register_environment_eq_LIVE", "register_environment_lowercase", "rotate_environment_LIVE", "webhook_profile_LIVE",
			"qualify_profile_eq_LIVE", "live_approve", "live_canary"} {
			refused("refused_"+c+"_"+a, "stripe_live_refused")
		}
	}
	// admitted with the pair; argv carries the subcommand and only names
	for _, a := range []string{"live_approve", "live_canary", "live_revoke", "method_disable", "method_enable", "qualify_live"} {
		r := admitted("pair_" + a)
		if !stubHas(r, "-e COMMERCE_STRIPE_LIVE_ENABLED") || !stubHas(r, "-e COMMERCE_STRIPE_LIVE_APPROVAL_REF") || !stubHas(r, "ENVVALUE COMMERCE_STRIPE_LIVE_ENABLED=1") ||
			!stubHas(r, "ENVVALUE COMMERCE_STRIPE_LIVE_APPROVAL_REF="+slxRef) {
			t.Errorf("pair_%s: the compose.env pair must be mapped to COMMERCE_STRIPE_LIVE_* and forwarded by name: %v", a, r.stub)
		}
	}
	for _, sub := range []struct{ name, needle string }{{"live_approve", "stripe-admin /app/bin/stripe-admin live-approve"}, {"live_canary", "stripe-admin /app/bin/stripe-admin live-canary"},
		{"live_revoke", "stripe-admin /app/bin/stripe-admin live-revoke"}} {
		if !stubHas(get("pair_"+sub.name), sub.needle) {
			t.Errorf("pair_%s: argv lacks %q", sub.name, sub.needle)
		}
	}
	// the kill switch never depends on the pair, whatever state the deploy env is in
	for _, c := range []string{"none", "flag", "ref", "badref"} {
		for _, a := range []string{"live_revoke", "method_disable", "method_any_flags"} {
			r := admitted("killswitch_" + c + "_" + a)
			if c != "ok" && stubHas(r, "-e COMMERCE_STRIPE_LIVE_ENABLED") {
				t.Errorf("killswitch_%s_%s: an invalid or absent pair was forwarded: %v", c, a, r.stub)
			}
		}
	}
	// usage() still refuses everything else (exit 2, the usage line)
	for _, name := range []string{"unknown_subcommand", "unknown_live_subcommand", "unknown_tool", "missing_subcommand"} {
		r := get(name)
		if r.rc != 2 || len(r.stub) != 0 || !strings.Contains(r.stderr, "usage:") {
			t.Errorf("%s: rc=%d stderr=%q stub=%v, want usage() (exit 2)", name, r.rc, r.stderr, r.stub)
		}
	}
	admitted("meta_admin_unchanged")
	// SANDBOX keeps working and never forwards the pair; a caller's own COMMERCE_* pair is neither trusted nor forwarded
	for _, name := range []string{"sandbox_register_value", "sandbox_register_default_env"} {
		r := admitted(name)
		if stubHas(r, "COMMERCE_STRIPE_LIVE") {
			t.Errorf("%s forwarded the LIVE pair: %v", name, r.stub)
		}
	}
	refused("sandbox_rejects_live_key_value", "")
	refused("sandbox_rejects_sk_live_value", "")
	refused("caller_env_pair_is_not_trusted", "stripe_live_refused")
	if r := admitted("caller_env_pair_is_not_forwarded"); stubHas(r, "COMMERCE_STRIPE_LIVE") {
		t.Errorf("a caller-supplied COMMERCE_STRIPE_LIVE_* value was forwarded: %v", r.stub)
	}
	// The secret values never appear on any stream or in the stub log (LD3/O-D): checked over every case.
	for name, r := range res {
		all := r.stdout + r.stderr + strings.Join(r.stub, "\n")
		for _, secret := range []string{rkLive, skLive, rkTest, skTest, whsec, tag} {
			if strings.Contains(all, secret) {
				t.Errorf("%s: a secret value or its tag appears in stdout/stderr/argv", name)
			}
		}
	}
	// The raw driver output (which includes any tracing) must not contain them either.
	for _, secret := range []string{rkLive, skLive, rkTest, skTest, whsec} {
		if strings.Contains(stdout+stderr, secret) {
			t.Error("a secret value appears in the raw container output")
		}
	}
}

// ---------------------------------------------------------------------------------------------------------------------
// Shell half: preflight P06 both directions (host: python3 + bash 3.2 are enough for this script)
// ---------------------------------------------------------------------------------------------------------------------

func slxP06(t *testing.T, profiles, paymentProfile string, compose map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	envDir := filepath.Join(dir, "env")
	if err := os.MkdirAll(envDir, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"admin", "storefront", "payment-worker", "claims-worker", "ads-worker", "expiry-worker", "meta-worker", "caddy", "postgres"} { // every file of preflight's knob allowlist (ads-worker.env: R2 ads lane)
		if err := os.WriteFile(filepath.Join(envDir, f+".env"), nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(envDir, "api.env"), []byte("COMMERCE_PAYMENT_PROFILE="+paymentProfile+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	lines := []string{"COMPOSE_PROFILES=" + profiles, "LC_ENVIRONMENT=smoke", "LC_ENV_DIR=" + envDir}
	keys := make([]string, 0, len(compose))
	for k := range compose {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		lines = append(lines, k+"="+compose[k])
	}
	composeEnv := filepath.Join(dir, "compose.env")
	if err := os.WriteFile(composeEnv, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("bash", filepath.Join(slxRepo(t), "deploy", "scripts", "preflight.sh"), "--skip-images")
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "LC_CONFIG_DIR=" + dir, "LC_COMPOSE_ENV=" + composeEnv}
	out, _ := cmd.CombinedOutput() // other rules fail on this minimal config; only the P06 summary is read
	summary := ""
	for _, line := range strings.Split(string(out), "\n") {
		if strings.HasPrefix(line, "P06 ") && strings.HasSuffix(line, "(rule summary)") {
			summary = strings.Fields(line)[1]
		}
	}
	if summary == "" {
		t.Fatalf("no P06 summary in the preflight output: %s", out)
	}
	return summary
}

func slxPreflight(t *testing.T) {
	pair := map[string]string{"LC_STRIPE_ENABLED": "1", "LC_STRIPE_LIVE_ENABLED": "1", "LC_STRIPE_LIVE_APPROVAL_REF": slxRef}
	on := map[string]string{"LC_STRIPE_ENABLED": "1"}
	for _, c := range []struct {
		name, profiles, payment string
		compose                 map[string]string
		want                    string // PASS, WARN (pass with the payments-live warning) or FAIL
	}{
		{"SANDBOX + payments-sandbox pass", "db,app,payments-sandbox", "SANDBOX", on, "PASS"},
		{"LIVE + payments-live + pair pass", "db,app,payments-live", "LIVE", pair, "WARN"},
		{"LIVE without the pair fails", "db,app,payments-live", "LIVE", on, "FAIL"},
		{"LIVE with the flag only fails", "db,app,payments-live", "LIVE", map[string]string{"LC_STRIPE_ENABLED": "1", "LC_STRIPE_LIVE_ENABLED": "1"}, "FAIL"},
		{"LIVE with the reference only fails", "db,app,payments-live", "LIVE", map[string]string{"LC_STRIPE_ENABLED": "1", "LC_STRIPE_LIVE_APPROVAL_REF": slxRef}, "FAIL"},
		{"LIVE with a malformed reference fails", "db,app,payments-live", "LIVE", map[string]string{"LC_STRIPE_ENABLED": "1", "LC_STRIPE_LIVE_ENABLED": "1", "LC_STRIPE_LIVE_APPROVAL_REF": "bad ref"}, "FAIL"},
		{"LIVE profile with only payments-sandbox is mixed and fails", "db,app,payments-sandbox", "LIVE", pair, "FAIL"},
		{"SANDBOX profile with only payments-live is mixed and fails", "db,app,payments-live", "SANDBOX", on, "FAIL"},
		{"LIVE without Stripe needs no pair", "db,app,payments-live", "LIVE", map[string]string{}, "WARN"},
		{"SANDBOX with Stripe off passes", "db,app,payments-sandbox", "SANDBOX", map[string]string{}, "PASS"},
		{"the checkout switch on without LC_STRIPE_ENABLED strands stock and fails", "db,app,payments-sandbox", "SANDBOX", map[string]string{"LC_STRIPE_CHECKOUT_ENABLED": "1"}, "FAIL"},
		{"the checkout switch on with LC_STRIPE_ENABLED passes", "db,app,payments-sandbox", "SANDBOX", slxMerge(on, map[string]string{"LC_STRIPE_CHECKOUT_ENABLED": "1"}), "PASS"},
		{"the checkout switch alone (0) keeps a LIVE+pair deployment valid", "db,app,payments-live", "LIVE", slxMerge(pair, map[string]string{"LC_STRIPE_CHECKOUT_ENABLED": "0"}), "WARN"},
	} {
		if got := slxP06(t, c.profiles, c.payment, c.compose); got != c.want {
			t.Errorf("P06 %s: summary %s, want %s", c.name, got, c.want)
		}
	}
}
