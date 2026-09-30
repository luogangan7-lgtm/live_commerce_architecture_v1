package foundation_test

// SP15 PG/process half (contracts/stripe-psp-v1.md §14, §0.2 SP15 delta). The no-PG
// half lives in cmd/{api,payment-worker,stripe-admin}/stripe_sp15_test.go under the
// same top-level name. Ingress-role admission in both directions (ingress may not
// hold merchant/worker/registrar authority and vice versa) is cited, not repeated:
// see TestStripeAuthority* in tests/foundation/stripe_authority_test.go.
//
// One isolated database (the payment-queue audit refuses the shared fixture):
//  1. a Stripe-DISABLED worker assembly serves PAYUNi (signed mock) while a Stripe
//     job is retained without a claim;
//  2. a Stripe-enabled assembly with the wrong custody keyring cannot open the
//     scoped credential and sends no provider request;
//  3. the correct assembly then serves the very same job (fake Stripe) on the
//     same profile queue;
//  4. GET /v1/account mismatch fails before any checkout request;
//  5. exact-version credentials: a rotated-out historical key is never replaced;
//  6. the built cmd/payment-worker binary (SANDBOX, COMMERCE_STRIPE_ENABLED=1)
//     reaches payment_worker_ready and exits 0 on SIGTERM.
// MOCK tier note: the binary refuses PROVIDER_MOCK by design, so 1-5 run the same
// assembly functions in-process; only 6 is binary-level.

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"livecommerce/internal/integrations/psp/stripe/stripetest"
)

func TestStripeSP15Process(t *testing.T) {
	e := sflNew(t, nil)
	ctx := context.Background()
	payuni := e.addPAYUNi(t)
	stopDisabled := e.start(t, false) // no StripeRuntime: never claims Stripe operations

	t.Run("disabled_assembly_serves_payuni_and_retains_the_stripe_job", func(t *testing.T) {
		s := e.stripeStore(t)
		seeded := len(e.fake.Requests())
		res := e.attempt(t, s)
		payuni.pcAwaitCapture(t) // the PAYUNi provider path is untouched by Stripe support
		time.Sleep(4 * time.Second)
		var state string
		var generation int64
		if err := e.f.owner.QueryRow(ctx, `SELECT j.state,o.generation FROM river_payment.river_job j JOIN integration.operations o ON o.id=$1::uuid WHERE j.id=$2`, res.AttemptID, res.JobID).Scan(&state, &generation); err != nil {
			t.Fatal(err)
		}
		if state == "completed" || state == "discarded" || state == "cancelled" || generation != 1 {
			t.Fatalf("Stripe job not retained or claim burned: state=%s generation=%d", state, generation)
		}
		if e.count(t, `SELECT count(*) FROM payments.stripe_sessions WHERE attempt_id=$1 AND (create_first_sent_at IS NOT NULL OR session_id IS NOT NULL)`, res.AttemptID) != 0 {
			t.Fatal("disabled assembly touched the Stripe session")
		}
		if got := len(e.fake.Requests()); got != seeded {
			t.Fatalf("disabled assembly called Stripe: %d -> %d requests", seeded, got)
		}
		e.wantStock(t, s, sflPending)
		// Retire the disabled assembly before the enabled ones start: while both compete, it fails
		// the job on every wake-up and burns River attempts (the job would be discarded after 25).
		stopDisabled()

		// (2) wrong custody keyring: same queue, Stripe enabled, but the scoped credential cannot be
		// opened, so the job fails closed before any provider request.
		wrong := e.startWith(t, true, pwKeys(t))
		e.await(t, "unavailable code", res.AttemptID, 40*time.Second, `SELECT EXISTS(SELECT 1 FROM integration.operation_events WHERE operation_id=$1 AND reason_code='stripe_unavailable')`)
		if got := len(e.fake.Requests()); got != seeded {
			t.Fatalf("worker with unopenable material contacted Stripe: %d -> %d requests", seeded, got)
		}
		e.wantStock(t, s, sflPending)
		wrong()

		// (3) the correct assembly consumes the same retained job on the same queue.
		e.startWith(t, true, e.keys)
		e.await(t, "session pinned by the enabled assembly", res.AttemptID, 60*time.Second, `SELECT session_id IS NOT NULL FROM payments.stripe_sessions WHERE attempt_id=$1`)
		if len(sflCreateKeysPerAttempt(e)[res.AttemptID]) != 1 || e.sends(res.AttemptID) != 1 {
			t.Fatalf("one create, one key expected: %v", sflCreateKeysPerAttempt(e)[res.AttemptID])
		}
	})

	t.Run("account_mismatch_fails_before_any_checkout_request", func(t *testing.T) {
		s := e.stripeStore(t)
		// The registered account is s.account; Stripe now says this key belongs to another account.
		if err := e.fake.AddAccount("acct_MismatchOther1", s.secret); err != nil {
			t.Fatal(err)
		}
		res := e.attempt(t, s)
		e.await(t, "unavailable code", res.AttemptID, 40*time.Second, `SELECT EXISTS(SELECT 1 FROM integration.operation_events WHERE operation_id=$1 AND reason_code='stripe_unavailable')`)
		creates, verifies := 0, 0
		for _, r := range e.fake.Requests() {
			if r.KeyFingerprint != stripetest.KeyFingerprint(s.secret) {
				continue
			}
			if r.Method == http.MethodPost && r.Path == "/v1/checkout/sessions" {
				creates++
			}
			if r.Method == http.MethodGet && r.Path == "/v1/account" {
				verifies++
			}
		}
		if creates != 0 || verifies < 1 {
			t.Fatalf("GET /v1/account must run (%d) and fail closed before any create (%d)", verifies, creates)
		}
		if e.count(t, `SELECT count(*) FROM payments.stripe_sessions WHERE attempt_id=$1 AND (create_first_sent_at IS NOT NULL OR session_id IS NOT NULL)`, res.AttemptID) != 0 {
			t.Fatal("a create was marked sent although verification failed")
		}
		e.wantStock(t, s, sflPending)
	})

	t.Run("exact_version_credentials_survive_rotation_and_never_substitute", func(t *testing.T) {
		s := e.stripeStore(t)
		oldKey := s.secret
		res, session := e.pinned(t, s) // created with v1
		if fp := sflCreateFingerprint(e, res.AttemptID); fp != stripetest.KeyFingerprint(oldKey) {
			t.Fatalf("create used key %s, not the frozen v1 key", fp)
		}
		newKey := "sk_test_" + hex.EncodeToString(randomBytes(12))
		if err := e.fake.AddAccount(s.account, newKey); err != nil {
			t.Fatal(err)
		}
		if head, err := e.reg.Rotate(ctx, s.scope, s.connection, 1, s.account, newKey); err != nil || head != 2 {
			t.Fatalf("rotate: %d %v", head, err)
		}
		var pinned int64
		if err := e.f.owner.QueryRow(ctx, `SELECT credential_version FROM checkout.payment_attempts WHERE id=$1`, res.AttemptID).Scan(&pinned); err != nil || pinned != 1 {
			t.Fatalf("historical attempt credential version=%d", pinned)
		}
		// Later polls of the historical attempt still use v1 (the old key is still valid at Stripe).
		mark := len(e.fake.Requests())
		sstWake(t, e.f, res.AttemptID)
		e.awaitRequestFrom(t, session, mark, oldKey)
		for _, r := range e.fake.Requests()[mark:] {
			if strings.Contains(r.Path, session) && r.KeyFingerprint == stripetest.KeyFingerprint(newKey) {
				t.Fatal("historical attempt used the rotated head credential")
			}
		}
		// If the historical credential stops working, the attempt stays UNKNOWN with stock kept and
		// never silently switches to the new key or a new create identity.
		e.fake.RevokeKey(oldKey)
		mark = len(e.fake.Requests())
		for i := 0; i < 3; i++ {
			sstWake(t, e.f, res.AttemptID)
			time.Sleep(1200 * time.Millisecond)
		}
		for _, r := range e.fake.Requests()[mark:] {
			if r.KeyFingerprint == stripetest.KeyFingerprint(newKey) && (strings.Contains(r.Path, session) || r.Path == "/v1/checkout/sessions") {
				t.Fatal("a revoked historical credential was replaced by the new head")
			}
		}
		if e.has(t, res.AttemptID, "CLOSED_UNPAID") || e.has(t, res.AttemptID, "CAPTURED") {
			t.Fatal("auth failure on a pinned session produced a financial fact")
		}
		e.wantStock(t, s, sflPending)
		if e.sends(res.AttemptID) != 1 {
			t.Fatalf("create sends=%d after credential failure", e.sends(res.AttemptID))
		}
		// A NEW attempt after requalification freezes v2 and its create uses the new key.
		s.secret = newKey
		s.requalify(t, e.sstEnv, 2)
		v, err := e.reg.SetMethod(ctx, s.scope, s.methodInput(1, true, true, 2500, 99999900))
		if err != nil {
			t.Fatal(err)
		}
		s.method = v
		second := sstMoreHold(t, s.p)
		s.p = second
		res2, _ := e.pinned(t, s)
		var v2 int64
		if err := e.f.owner.QueryRow(ctx, `SELECT credential_version FROM checkout.payment_attempts WHERE id=$1`, res2.AttemptID).Scan(&v2); err != nil || v2 != 2 {
			t.Fatalf("new attempt froze version %d", v2)
		}
		if fp := sflCreateFingerprint(e, res2.AttemptID); fp != stripetest.KeyFingerprint(newKey) {
			t.Fatalf("new attempt created with %s, not the v2 key", fp)
		}
	})

	t.Run("built_payment_worker_binary_sandbox_stripe_enabled_ready_and_sigterm", func(t *testing.T) {
		sflWorkerBinary(t, e)
	})
}

func sflCreateFingerprint(e *sflEnv, attempt string) string {
	for _, r := range e.fake.Requests() {
		if r.Method == http.MethodPost && r.Path == "/v1/checkout/sessions" && strings.HasSuffix(r.IdempotencyKey, attempt) {
			return r.KeyFingerprint
		}
	}
	return ""
}

// awaitRequestFrom waits until a fake call for the session made with key appears after mark.
func (e *sflEnv) awaitRequestFrom(t *testing.T, session string, mark int, key string) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		for _, r := range e.fake.Requests()[mark:] {
			if strings.Contains(r.Path, session) && r.KeyFingerprint == stripetest.KeyFingerprint(key) {
				return
			}
		}
		time.Sleep(300 * time.Millisecond)
	}
	t.Fatal("no historical-key request observed")
}

// sflWorkerBinary builds cmd/payment-worker and runs it against the isolated DB with
// SANDBOX + COMMERCE_STRIPE_ENABLED=1 and no Stripe secret in its environment.
func sflWorkerBinary(t *testing.T, e *sflEnv) {
	binary := filepath.Join(t.TempDir(), "payment-worker")
	if out, err := exec.Command("go", "build", "-o", binary, "../../cmd/payment-worker").CombinedOutput(); err != nil {
		t.Fatalf("build cmd/payment-worker: %v %s", err, out)
	}
	key := base64.StdEncoding.EncodeToString(randomBytes(32))
	keysJSON, _ := json.Marshal([]map[string]string{{"id": "query_test", "key_base64": key}})
	base := func(profile, stripe string) []string {
		env := []string{"PATH=" + os.Getenv("PATH"), "COMMERCE_PAYMENT_WORKER_ENABLED=1", "COMMERCE_PAYMENT_WORKER_PROFILE=" + profile,
			"COMMERCE_PAYMENT_WORKER_CONCURRENCY=1", "COMMERCE_PAYMENT_WORKER_DATABASE_URL=" + bcRole(t, e.f, "commerce_worker"),
			"COMMERCE_ACCOUNT_ACTIVE_KEY_ID=query_test", "COMMERCE_ACCOUNT_KEYS_JSON=" + string(keysJSON),
			"COMMERCE_ACCOUNT_REPLAY_KEY=" + base64.StdEncoding.EncodeToString(randomBytes(32)),
			// Global Stripe credentials the worker must never read (§0.2): sentinels would leak if it did.
			"STRIPE_SECRET_KEY=sk_" + "test_sentinel" + t04Tag(), "STRIPE_WEBHOOK_SECRET=whsec_" + "sentinel" + t04Tag(), "STRIPE_ACCOUNT_ID=acct_sentinel" + t04Tag()}
		if stripe != "" {
			env = append(env, "COMMERCE_STRIPE_ENABLED="+stripe)
		}
		return env
	}
	secrets := []string{}
	for _, kv := range base("SANDBOX", "") {
		if strings.HasPrefix(kv, "STRIPE_") {
			secrets = append(secrets, strings.SplitN(kv, "=", 2)[1])
		}
	}
	// Config negatives exit non-zero before touching the database and never echo input.
	for name, env := range map[string][]string{
		"stripe_with_live_profile": base("LIVE", "1"),
		"stripe_flag_noncanonical": base("SANDBOX", "true"),
	} {
		out, err := sprRunWorkerOnce(binary, env)
		if err == nil || !strings.Contains(out, "payment_worker_invalid_config") || strings.Contains(out, "postgres://") || sprAnyContains(out, secrets) {
			t.Fatalf("%s: err=%v out=%q", name, err, out)
		}
	}
	u, err := url.Parse(bcRole(t, e.f, "commerce_worker"))
	if err != nil {
		t.Fatal(err)
	}
	app := "sp15_binary_" + t04Tag()
	q := u.Query()
	q.Set("application_name", app)
	u.RawQuery = q.Encode()
	env := base("SANDBOX", "1")
	for i, kv := range env {
		if strings.HasPrefix(kv, "COMMERCE_PAYMENT_WORKER_DATABASE_URL=") {
			env[i] = "COMMERCE_PAYMENT_WORKER_DATABASE_URL=" + u.String()
		}
	}
	cmd := exec.Command(binary)
	cmd.Env = env
	cmd.Stdout = io.Discard
	stderr, err := cmd.StderrPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	ready := make(chan struct{}, 1)
	done := make(chan struct{})
	var captured strings.Builder
	go func() {
		defer close(done)
		scan := bufio.NewScanner(stderr)
		for scan.Scan() {
			captured.WriteString(scan.Text() + "\n")
			if strings.HasSuffix(scan.Text(), " INFO payment_worker_ready") {
				select {
				case ready <- struct{}{}:
				default:
				}
			}
		}
	}()
	waited := false
	defer func() {
		if !waited {
			_ = cmd.Process.Kill()
			<-done
			_ = cmd.Wait()
		}
	}()
	select {
	case <-ready:
	case <-done:
		err := cmd.Wait()
		waited = true
		t.Fatalf("worker exited before ready: %v %s", err, captured.String())
	case <-time.After(15 * time.Second):
		t.Fatal("worker did not print payment_worker_ready")
	}
	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	select {
	case <-done:
		if err := cmd.Wait(); err != nil {
			waited = true
			t.Fatalf("SIGTERM exit: %v", err)
		}
		waited = true
	case <-time.After(10 * time.Second):
		t.Fatal("worker did not exit on SIGTERM")
	}
	if sprAnyContains(captured.String(), secrets) || strings.Contains(captured.String(), "postgres://") {
		t.Fatal("worker output contains an environment secret")
	}
	waitPoolsGone(t, e.f, "worker binary leaked pool connections", app)
}

func sprRunWorkerOnce(binary string, env []string) (string, error) {
	cmd := exec.Command(binary)
	cmd.Env = env
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func sprAnyContains(s string, needles []string) bool {
	for _, n := range needles {
		if n != "" && strings.Contains(s, n) {
			return true
		}
	}
	return false
}
