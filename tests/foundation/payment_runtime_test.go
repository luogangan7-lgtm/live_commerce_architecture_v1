package foundation_test

import (
	"bufio"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"
	"github.com/riverqueue/river/rivertype"
	"livecommerce/internal/checkout"
	"livecommerce/internal/integrations/accounts"
	integration "livecommerce/internal/integrations/core"
	"livecommerce/internal/payments"
	"livecommerce/internal/platform"
	"livecommerce/migrations"
)

// The business baseline provisions cluster-scoped roles, so a second database
// on the suite's server cannot be migrated fresh. Each test owns a labelled,
// loopback-only temporary PG container instead.
func pwIsolatedFixture(t *testing.T) *testFixture {
	t.Helper()
	fixture(t) // enforces the existing explicit local-PG test permission gate
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	name := "lc-payment-worker-" + t04Tag()
	password := hex.EncodeToString(randomBytes(24))
	_, err := exec.CommandContext(ctx, "docker", "run", "-d", "--pull=never", "--name", name,
		"--label", "livecommerce.fixture="+name, "--memory=512m", "--cpus=1", "--pids-limit=128",
		"--tmpfs", "/var/lib/postgresql:rw,size=268435456", "-e", "POSTGRES_PASSWORD="+password,
		"-e", "POSTGRES_DB=lc_foundation_test", "-p", "127.0.0.1::5432",
		"postgres@sha256:4ef4dbc939d61acea57712655ddb4b4ab27419c913f94cca0cd57cb3ea3c2280",
		"-c", "shared_buffers=32MB", "-c", "max_connections=60").CombinedOutput()
	if err != nil {
		t.Fatalf("start labelled local PG container: %v", err)
	}
	t.Cleanup(func() {
		cleanup, stop := context.WithTimeout(context.Background(), 15*time.Second)
		defer stop()
		label, err := exec.CommandContext(cleanup, "docker", "inspect", "-f", `{{index .Config.Labels "livecommerce.fixture"}}`, name).Output()
		if err != nil || strings.TrimSpace(string(label)) != name {
			t.Errorf("refusing unverified PG container cleanup %s: %v", name, err)
			return
		}
		if _, err := exec.CommandContext(cleanup, "docker", "rm", "-f", name).CombinedOutput(); err != nil {
			t.Errorf("remove test container %s: %v", name, err)
		}
	})
	portOutput, err := exec.CommandContext(ctx, "docker", "port", name, "5432/tcp").Output()
	if err != nil || !strings.HasPrefix(strings.TrimSpace(string(portOutput)), "127.0.0.1:") {
		t.Fatal("test PG did not bind loopback")
	}
	port := strings.TrimPrefix(strings.TrimSpace(string(portOutput)), "127.0.0.1:")
	if _, err := strconv.Atoi(port); err != nil {
		t.Fatal("test PG returned invalid port")
	}
	u := &url.URL{Scheme: "postgres", User: url.UserPassword("postgres", password), Host: "127.0.0.1:" + port, Path: "/lc_foundation_test", RawQuery: "sslmode=disable"}
	owner, err := pgxpool.New(ctx, u.String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(owner.Close)
	ready := false
	for i := 0; i < 50; i++ {
		probe, stop := context.WithTimeout(ctx, 500*time.Millisecond)
		err = owner.Ping(probe)
		stop()
		if err == nil {
			ready = true
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	if !ready {
		t.Fatal("test PG did not become ready")
	}
	if err := migrations.Apply(ctx, owner); err != nil {
		t.Fatal(err)
	}
	if err := migrations.Apply(ctx, owner); err != nil {
		t.Fatal(err)
	}
	f := &testFixture{owner: owner, databaseURL: u.String(), tenantA: randomUUID(), tenantB: randomUUID(), storeA1: randomUUID(), storeA2: randomUUID(), storeB: randomUUID(), principalA: randomUUID(), tokens: map[string]string{"a": randomToken(), "a2": randomToken(), "b": randomToken(), "expired": randomToken(), "revoked": randomToken(), "buyer": randomToken(), "revoked_grant": randomToken()}}
	if err := f.seed(ctx); err != nil {
		t.Fatal(fmt.Errorf("seed isolated worker database: %w", err))
	}
	runtimeURL := bcRole(t, f, "commerce_runtime")
	runtime, err := platform.OpenPool(ctx, runtimeURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(runtime.Close)
	f.runtime = runtime
	return f
}

func pwKeys(t *testing.T) *accounts.Keyring {
	t.Helper()
	keys, err := accounts.NewKeyring("query_test", map[string][]byte{"query_test": randomBytes(32)}, randomBytes(32))
	if err != nil {
		t.Fatal(err)
	}
	return keys
}

func pwWorkerPool(t *testing.T, f *testFixture) *pgxpool.Pool {
	t.Helper()
	pool, err := platform.OpenWorkerPool(context.Background(), bcRole(t, f, "commerce_worker"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// Force the pre-upgrade producer's requested default queue before River's
// actual InsertTx. The starter still inserts the job before its attempt in the
// same transaction; only the post-River deferred trigger can resolve it.
func pwOldQuerySetupOn(t *testing.T, f *testFixture, keys *accounts.Keyring, historical ...string) pqFixture {
	t.Helper()
	p := psSetupItemsOn(t, f, 1, historical...)
	service, err := accounts.New(keys, &integration.Service{})
	if err != nil {
		t.Fatal(err)
	}
	schema := "river_payment"
	if len(historical) == 1 && historical[0] == "river" {
		schema = "river"
	}
	q := pqFixture{psHarness: p, keys: keys, accountService: service, schema: schema}
	q.rotate(t, 1, pqOldSecret)
	qualExec(t, p.f.owner, `UPDATE payments.account_qualifications SET credential_version=2 WHERE id=$1`, p.proof)
	middleware := river.JobInsertMiddlewareFunc(func(ctx context.Context, params []*rivertype.JobInsertParams, next func(context.Context) ([]*rivertype.JobInsertResult, error)) ([]*rivertype.JobInsertResult, error) {
		for _, row := range params {
			if row.Kind == "payment_query_v1" {
				row.Queue = "default"
			}
		}
		return next(ctx)
	})
	jobs, err := river.NewClient(riverpgxv5.New(p.pool), &river.Config{Schema: schema, JobInsertMiddleware: []rivertype.JobInsertMiddleware{middleware}})
	if err != nil {
		t.Fatal(err)
	}
	q.starter, err = checkout.NewPaymentStarter(context.Background(), p.pool, jobs, "PROVIDER_MOCK")
	if err != nil {
		t.Fatal(err)
	}
	q.result, err = q.start(t04Key("pw-old-start"))
	if err != nil {
		t.Fatal(err)
	}
	return q
}

func pwDefaultDomainJobs(t *testing.T, q pqFixture) (expiryID, externalID int64) {
	t.Helper()
	ctx := context.Background()
	if err := q.f.owner.QueryRow(ctx, `SELECT job_id FROM checkout.orders WHERE id=$1`, q.hold.OrderID).Scan(&expiryID); err != nil {
		t.Fatal(err)
	}
	jobs, err := river.NewClient(riverpgxv5.New(q.f.runtime), &river.Config{Schema: "river"})
	if err != nil {
		t.Fatal(err)
	}
	service, err := integration.New(jobs)
	if err != nil {
		t.Fatal(err)
	}
	err = platform.WithScope(ctx, q.f.runtime, q.f.tokens["a"], q.f.storeA1, "store:read", func(tx pgx.Tx, scope platform.Scope) error {
		planned, err := service.Plan(ctx, tx, scope, q.f.tokens["a"], t04Key("pw-external"), integration.PlanInput{
			BindingID: q.binding, ExpectedBindingVersion: 1, Purpose: "transactional", Action: "payment.authorize", Request: json.RawMessage(`{"amount":1}`),
		})
		if err == nil {
			externalID = planned.JobID
		}
		return err
	})
	if err != nil || externalID < 1 || pwQueueIn(t, q.f.owner, "river_expiry.river_job", expiryID) != "checkout_expiry_v1" || pwQueueIn(t, q.f.owner, "river.river_job", externalID) != "default" {
		t.Fatalf("actual non-payment domain jobs unavailable: expiry=%d external=%d err=%v", expiryID, externalID, err)
	}
	return expiryID, externalID
}

func pwAwaitJob(t *testing.T, q pqFixture, id int64, state string) {
	t.Helper()
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		var got string
		if err := q.f.owner.QueryRow(context.Background(), `SELECT state FROM river_payment.river_job WHERE id=$1`, id).Scan(&got); err != nil {
			t.Fatal(err)
		}
		if got == state {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("payment job %d did not reach %s", id, state)
}

func pwStopClient(t *testing.T, client *river.Client[pgx.Tx]) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := client.StopAndCancel(ctx); err != nil {
		t.Fatal(err)
	}
}

func pwQueryTrade(values url.Values) (string, error) {
	wire, err := hex.DecodeString(values.Get("EncryptInfo"))
	if err != nil {
		return "", err
	}
	parts := strings.Split(string(wire), ":::")
	if len(parts) != 2 {
		return "", fmt.Errorf("bad query envelope")
	}
	data, err := base64.StdEncoding.DecodeString(parts[0])
	if err != nil {
		return "", err
	}
	tag, err := base64.StdEncoding.DecodeString(parts[1])
	if err != nil {
		return "", err
	}
	block, err := aes.NewCipher([]byte(pqOldSecret.HashKey))
	if err != nil {
		return "", err
	}
	gcm, err := cipher.NewGCMWithNonceSize(block, 16)
	if err != nil {
		return "", err
	}
	plain, err := gcm.Open(nil, []byte(pqOldSecret.HashIV), append(data, tag...), nil)
	if err != nil {
		return "", err
	}
	fields, err := url.ParseQuery(string(plain))
	if err != nil {
		return "", err
	}
	return fields.Get("MerTradeNo"), nil
}

func TestBuyerPaymentWorkerRealRiverTwoTenantCaptureAndRestart(t *testing.T) {
	f := pwIsolatedFixture(t)
	keys := pwKeys(t)
	first := pqSetupItemsOn(t, f, keys, false, 1)
	second := pqSetupItemsOn(t, f, keys, false, 1)
	expiryID, externalID := pwDefaultDomainJobs(t, first)
	expiryBefore, externalBefore := pwJobExceptQueueIn(t, f.owner, "river_expiry.river_job", expiryID), pwJobExceptQueueIn(t, f.owner, "river.river_job", externalID)
	if first.result.AttemptID == second.result.AttemptID || first.f.tenantA == second.f.tenantA {
		t.Fatal("two-tenant worker fixture collapsed")
	}
	var calls atomic.Int32
	bodies := map[string]string{first.result.MerchantTradeNo: pqSignedResponse(pcFull(first)), second.result.MerchantTradeNo: pqSignedResponse(pcFull(second))}
	opts := payments.DefaultQueryWorkerOptions()
	opts.MockTransport = pqTransport(func(r *http.Request) (*http.Response, error) {
		if r.URL.String() != "https://sandbox-api.payuni.com.tw/api/trade/query" || r.Method != http.MethodPost {
			return nil, fmt.Errorf("unexpected query destination")
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			return nil, err
		}
		values, err := url.ParseQuery(string(body))
		if err != nil {
			return nil, err
		}
		trade, err := pwQueryTrade(values)
		if err != nil {
			return nil, err
		}
		response := bodies[trade]
		if values.Get("MerID") != "mock-account" || response == "" {
			return nil, fmt.Errorf("unexpected signed query request")
		}
		calls.Add(1)
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(response)), Header: make(http.Header)}, nil
	})
	for _, q := range []pqFixture{first, second} {
		mustExec(t, q.f.owner, `UPDATE river_payment.river_job SET scheduled_at=clock_timestamp() WHERE id=$1`, q.result.JobID)
	}
	client, err := payments.NewWorkerClient(context.Background(), first.worker, keys, "PROVIDER_MOCK", 2, opts)
	if err != nil {
		t.Fatal(err)
	}
	if err := client.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	running := true
	defer func() {
		if running {
			pwStopClient(t, client)
		}
	}()
	for _, q := range []pqFixture{first, second} {
		pqAwait(t, q, q.result.JobID, "payment_report_observed", "scheduled")
		if q.reportCount(t) != 1 {
			t.Fatal("query stage did not persist observation")
		}
	}
	// Reconcile jobs become available only after the real query commits.
	var reconcileIDs [2]int64
	for i, q := range []pqFixture{first, second} {
		if err := q.f.owner.QueryRow(context.Background(), `SELECT id FROM river_payment.river_job WHERE kind='payment_reconcile_v1' AND args->>'operation_id'=$1`, q.result.OperationID).Scan(&reconcileIDs[i]); err != nil {
			t.Fatal(err)
		}
		pwAwaitJob(t, q, reconcileIDs[i], "completed")
		pcAssertStock(t, q, 0, 2, "CONFIRMED", "COMMITTED")
		pcAssertWork(t, q, "READY")
		if pcCount(t, q, "payments.facts", " AND kind='CAPTURED'") != 1 {
			t.Fatal("capture fact not unique")
		}
	}
	if calls.Load() != 2 {
		t.Fatalf("signed transport calls=%d want 2", calls.Load())
	}
	pwStopClient(t, client)
	running = false
	// Redeliver only this fixture's durable reconcile jobs across a client
	// restart. The second real worker pass must be observable in River attempts.
	var beforeAttempts [2]int
	for i, id := range reconcileIDs {
		if err := f.owner.QueryRow(context.Background(), `SELECT attempt FROM river_payment.river_job WHERE id=$1`, id).Scan(&beforeAttempts[i]); err != nil {
			t.Fatal(err)
		}
		mustExec(t, f.owner, `UPDATE river_payment.river_job SET state='available',finalized_at=NULL,scheduled_at=clock_timestamp() WHERE id=$1`, id)
	}
	restarted, err := payments.NewWorkerClient(context.Background(), first.worker, keys, "PROVIDER_MOCK", 2, opts)
	if err != nil {
		t.Fatal(err)
	}
	if err := restarted.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	for i, q := range []pqFixture{first, second} {
		pwAwaitJob(t, q, reconcileIDs[i], "completed")
		var attempts int
		if err := f.owner.QueryRow(context.Background(), `SELECT attempt FROM river_payment.river_job WHERE id=$1`, reconcileIDs[i]).Scan(&attempts); err != nil || attempts <= beforeAttempts[i] {
			t.Fatalf("restarted client did not replay reconcile: before=%d after=%d err=%v", beforeAttempts[i], attempts, err)
		}
	}
	pwStopClient(t, restarted)
	for _, q := range []pqFixture{first, second} {
		if pcCount(t, q, "payments.facts", " AND kind='CAPTURED'") != 1 || pcCount(t, q, "fulfillment.payment_work_items", "") != 1 {
			t.Fatal("restart duplicated money or fulfillment")
		}
	}
	if pwQueueIn(t, f.owner, "river_expiry.river_job", expiryID) != "checkout_expiry_v1" || pwQueueIn(t, f.owner, "river.river_job", externalID) != "default" || pwJobExceptQueueIn(t, f.owner, "river_expiry.river_job", expiryID) != expiryBefore || pwJobExceptQueueIn(t, f.owner, "river.river_job", externalID) != externalBefore {
		t.Fatal("payment client changed actual checkout expiry or external operation jobs")
	}
}

func TestBuyerPaymentWorkerLateDefaultInsertPollAndProfileIsolation(t *testing.T) {
	f := pwIsolatedFixture(t)
	keys := pwKeys(t)
	var unrelated int64
	if err := f.owner.QueryRow(context.Background(), `INSERT INTO river.river_job(kind,args,max_attempts,queue) VALUES('pw_unrelated_v1','{}',2,'default') RETURNING id`).Scan(&unrelated); err != nil {
		t.Fatal(err)
	}
	unrelatedBefore := pwJobExceptQueue(t, f.owner, unrelated)
	response := make(chan string, 1)
	opts := payments.DefaultQueryWorkerOptions()
	opts.MockTransport = pqTransport(func(r *http.Request) (*http.Response, error) {
		var body string
		select {
		case body = <-response:
		case <-r.Context().Done():
			return nil, r.Context().Err()
		}
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header)}, nil
	})
	client, err := payments.NewWorkerClient(context.Background(), pwWorkerPool(t, f), keys, "PROVIDER_MOCK", 2, opts)
	if err != nil {
		t.Fatal(err)
	}
	if err := client.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer pwStopClient(t, client)
	// The old producer requests default before it inserts the attempt; its
	// deferred route must commit before this already-running client can claim it.
	q := pwOldQuerySetupOn(t, f, keys)
	response <- pqSignedResponse(pcFull(q))
	if pwQueueIn(t, f.owner, "river_payment.river_job", q.result.JobID) != "payment_mock_v1" {
		t.Fatal("deferred late insert not routed")
	}
	mustExec(t, f.owner, `UPDATE river_payment.river_job SET scheduled_at=clock_timestamp() WHERE id=$1`, q.result.JobID)
	pqAwait(t, q, q.result.JobID, "payment_report_observed", "scheduled")
	var reconcile int64
	if err := f.owner.QueryRow(context.Background(), `SELECT id FROM river_payment.river_job WHERE kind='payment_reconcile_v1' AND args->>'operation_id'=$1`, q.result.OperationID).Scan(&reconcile); err != nil {
		t.Fatal(err)
	}
	pwAwaitJob(t, q, reconcile, "completed")
	pcAssertStock(t, q, 0, 2, "CONFIRMED", "COMMITTED")
	// The old observation producer requests default for an available reconcile
	// job after client Start. River's original NOTIFY names default; the worker
	// must still poll its profile queue and perform the real capture.
	notified := pqSetupItemsOn(t, f, keys, false, 1)
	if err := notified.record(notified.claim(t), pcFull(notified)); err != nil {
		t.Fatal(err)
	}
	var notifiedID int64
	if err := f.owner.QueryRow(context.Background(), `SELECT id FROM river_payment.river_job WHERE kind='payment_reconcile_v1' AND args->>'operation_id'=$1`, notified.result.OperationID).Scan(&notifiedID); err != nil {
		t.Fatal(err)
	}
	if pwQueueIn(t, f.owner, "river_payment.river_job", notifiedID) != "payment_mock_v1" {
		t.Fatal("old default-notified reconcile was not routed")
	}
	mustExec(t, f.owner, `UPDATE river_payment.river_job SET scheduled_at=clock_timestamp()+interval '1 hour' WHERE id=$1`, notified.result.JobID)
	pwAwaitJob(t, notified, notifiedID, "completed")
	pcAssertStock(t, notified, 0, 2, "CONFIRMED", "COMMITTED")
	foreign := pqSetupItemsOn(t, f, keys, false, 1)
	// Owner-only fixture mutation creates a due SANDBOX job without contacting a
	// provider. Bypass then restore the immutable family guard solely for this
	// fixture poison; the MOCK client must never claim the separate fixed queue.
	mustExec(t, f.owner, `ALTER TABLE river_payment.river_job DISABLE TRIGGER payment_job_family`)
	defer mustExec(t, f.owner, `ALTER TABLE river_payment.river_job ENABLE TRIGGER payment_job_family`)
	mustExec(t, f.owner, `UPDATE checkout.payment_attempts SET execution_profile='SANDBOX' WHERE id=$1`, foreign.result.AttemptID)
	mustExec(t, f.owner, `UPDATE river_payment.river_job SET queue='payment_sandbox_v1',scheduled_at=clock_timestamp() WHERE id=$1`, foreign.result.JobID)
	mustExec(t, f.owner, `ALTER TABLE river_payment.river_job ENABLE TRIGGER payment_job_family`)
	var generationBefore, generationAfter int64
	if err := f.owner.QueryRow(context.Background(), `SELECT generation FROM integration.operations WHERE id=$1`, foreign.result.OperationID).Scan(&generationBefore); err != nil {
		t.Fatal(err)
	}
	time.Sleep(2 * time.Second)
	if err := f.owner.QueryRow(context.Background(), `SELECT generation FROM integration.operations WHERE id=$1`, foreign.result.OperationID).Scan(&generationAfter); err != nil {
		t.Fatal(err)
	}
	if generationAfter != generationBefore || pwQueueIn(t, f.owner, "river_payment.river_job", foreign.result.JobID) != "payment_sandbox_v1" {
		t.Fatal("MOCK consumer claimed a SANDBOX attempt")
	}
	var foreignAttempts, foreignObservations, foreignFacts int
	var multiReady bool
	if err := f.owner.QueryRow(context.Background(), `SELECT j.attempt,
	 (SELECT count(*) FROM payments.provider_observations WHERE attempt_id=$1),
	 (SELECT count(*) FROM payments.facts WHERE attempt_id=$1),
	 integration.payment_queue_ready()
	 FROM river_payment.river_job j WHERE j.id=$2`, foreign.result.AttemptID, foreign.result.JobID).Scan(&foreignAttempts, &foreignObservations, &foreignFacts, &multiReady); err != nil || foreignAttempts != 0 || foreignObservations != 0 || foreignFacts != 0 || !multiReady {
		t.Fatalf("cross-profile isolation/admission: attempts=%d observations=%d facts=%d ready=%t err=%v", foreignAttempts, foreignObservations, foreignFacts, multiReady, err)
	}
	if pwJobExceptQueue(t, f.owner, unrelated) != unrelatedBefore || pwQueue(t, f.owner, unrelated) != "default" {
		t.Fatal("ordinary default River job changed")
	}
}

func TestBuyerPaymentWorkerProcessSignalAndPoolCleanup(t *testing.T) {
	f := pwIsolatedFixture(t)
	binary := filepath.Join(t.TempDir(), "payment-worker")
	build := exec.Command("go", "build", "-o", binary, "../../cmd/payment-worker")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build real payment-worker binary: %v %s", err, out)
	}
	disabled := exec.Command(binary)
	disabled.Env = []string{"PATH=" + os.Getenv("PATH"), "COMMERCE_PAYMENT_WORKER_ENABLED=0", "COMMERCE_PAYMENT_WORKER_DATABASE_URL=invalid", "COMMERCE_ACCOUNT_KEYS_JSON=invalid"}
	if out, err := disabled.CombinedOutput(); err != nil || len(out) != 0 {
		t.Fatalf("disabled process opened resources or failed: %v %q", err, out)
	}
	invalid := exec.Command(binary)
	invalid.Env = []string{"PATH=" + os.Getenv("PATH"), "COMMERCE_PAYMENT_WORKER_ENABLED=1", "COMMERCE_PAYMENT_WORKER_PROFILE=PROVIDER_MOCK", "COMMERCE_PAYMENT_WORKER_DATABASE_URL=invalid"}
	if out, err := invalid.CombinedOutput(); err == nil || !strings.Contains(string(out), "payment_worker_invalid_config") || strings.Contains(string(out), "invalid\n") {
		t.Fatalf("production binary accepted MOCK or leaked config: %v %q", err, out)
	}
	role := bcRole(t, f, "commerce_worker")
	u, err := url.Parse(role)
	if err != nil {
		t.Fatal(err)
	}
	app := "pw_binary_" + t04Tag()
	params := u.Query()
	params.Set("application_name", app)
	u.RawQuery = params.Encode()
	key := base64.StdEncoding.EncodeToString(randomBytes(32))
	keysJSON, err := json.Marshal([]map[string]string{{"id": "query_test", "key_base64": key}})
	if err != nil {
		t.Fatal(err)
	}
	env := []string{"PATH=" + os.Getenv("PATH"), "COMMERCE_PAYMENT_WORKER_ENABLED=1", "COMMERCE_PAYMENT_WORKER_PROFILE=SANDBOX", "COMMERCE_PAYMENT_WORKER_CONCURRENCY=1", "COMMERCE_PAYMENT_WORKER_DATABASE_URL=" + u.String(), "COMMERCE_ACCOUNT_ACTIVE_KEY_ID=query_test", "COMMERCE_ACCOUNT_KEYS_JSON=" + string(keysJSON), "COMMERCE_ACCOUNT_REPLAY_KEY=" + base64.StdEncoding.EncodeToString(randomBytes(32))}
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
	readerDone := make(chan struct{})
	go func() {
		defer close(readerDone)
		scan := bufio.NewScanner(stderr)
		for scan.Scan() {
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
			select {
			case <-readerDone:
			case <-time.After(3 * time.Second):
			}
			_ = cmd.Wait()
		}
	}()
	select {
	case <-ready:
	case <-readerDone:
		err := cmd.Wait()
		waited = true
		t.Fatalf("payment-worker exited before ready marker: %v", err)
	case <-time.After(8 * time.Second):
		t.Fatal("payment-worker did not report ready after River start")
	}
	deadline := time.Now().Add(8 * time.Second)
	seen := false
	for time.Now().Before(deadline) {
		var count int
		var queueStarted bool
		if err := f.owner.QueryRow(context.Background(), `SELECT
		 (SELECT count(*) FROM pg_stat_activity WHERE datname='lc_foundation_test' AND application_name=$1),
			 EXISTS(SELECT 1 FROM river_payment.river_queue WHERE name='payment_sandbox_v1')`, app).Scan(&count, &queueStarted); err != nil {
			t.Fatal(err)
		}
		if count > 0 && queueStarted {
			seen = true
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if !seen {
		t.Fatal("ready process has no SANDBOX River producer or worker pool")
	}
	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	select {
	case <-readerDone:
		err := cmd.Wait()
		waited = true
		if err != nil {
			t.Fatalf("SIGTERM worker exit: %v", err)
		}
	case <-time.After(8 * time.Second):
		t.Fatal("payment-worker did not stop after SIGTERM with no due jobs")
	}
	waitPoolsGone(t, f, "worker pool leaked connections", app)
}

func TestBuyerPaymentWorkerCrashChild(t *testing.T) {
	if os.Getenv("LC_PW_CHILD") != "1" {
		return
	}
	ctx := context.Background()
	pool, err := platform.OpenWorkerPool(ctx, os.Getenv("LC_PW_CHILD_DSN"))
	if err != nil {
		t.Fatal("child worker authority")
	}
	defer pool.Close()
	key, err := base64.StdEncoding.DecodeString(os.Getenv("LC_PW_CHILD_KEY"))
	if err != nil {
		t.Fatal("child key format")
	}
	replay, err := base64.StdEncoding.DecodeString(os.Getenv("LC_PW_CHILD_REPLAY"))
	if err != nil {
		t.Fatal("child replay key format")
	}
	keys, err := accounts.NewKeyring("query_test", map[string][]byte{"query_test": key}, replay)
	if err != nil {
		t.Fatal("child keyring")
	}
	opts := payments.DefaultQueryWorkerOptions()
	opts.MockTransport = pqTransport(func(r *http.Request) (*http.Response, error) {
		req, err := http.NewRequestWithContext(r.Context(), http.MethodGet, os.Getenv("LC_PW_CHILD_MOCK_URL"), nil)
		if err != nil {
			return nil, err
		}
		return http.DefaultClient.Do(req)
	})
	client, err := payments.NewWorkerClient(ctx, pool, keys, "PROVIDER_MOCK", 1, opts)
	if err != nil {
		t.Fatal("child client admission")
	}
	if err := client.Start(ctx); err != nil {
		t.Fatal("child client start")
	}
	select {} // parent kills this process; no Go cleanup runs
}

func TestBuyerPaymentWorkerRealCrashAndRiverLeaseRescue(t *testing.T) {
	f := pwIsolatedFixture(t)
	key, replay := randomBytes(32), randomBytes(32)
	keys, err := accounts.NewKeyring("query_test", map[string][]byte{"query_test": key}, replay)
	if err != nil {
		t.Fatal(err)
	}
	q := pqSetupItemsOn(t, f, keys, false, 1)
	q.pending(t)
	entered := make(chan struct{}, 1)
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case entered <- struct{}{}:
		default:
		}
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	defer server.Close()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	child := exec.Command(executable, "-test.run=^TestBuyerPaymentWorkerCrashChild$", "-test.timeout=60s")
	child.Env = append(os.Environ(), "LC_PW_CHILD=1", "LC_PW_CHILD_DSN="+q.worker.Config().ConnString(),
		"LC_PW_CHILD_KEY="+base64.StdEncoding.EncodeToString(key), "LC_PW_CHILD_REPLAY="+base64.StdEncoding.EncodeToString(replay), "LC_PW_CHILD_MOCK_URL="+server.URL)
	child.Stdout, child.Stderr = io.Discard, io.Discard
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- child.Wait() }()
	defer func() { _ = child.Process.Kill() }()
	mustExec(t, f.owner, `UPDATE river_payment.river_job SET scheduled_at=clock_timestamp() WHERE id=$1`, q.result.JobID)
	select {
	case <-entered:
	case <-done:
		t.Fatal("child exited before provider query boundary")
	case <-time.After(10 * time.Second):
		t.Fatal("child did not claim due payment query")
	}
	var state string
	var generation int64
	if err := f.owner.QueryRow(context.Background(), `SELECT j.state,o.generation FROM river_payment.river_job j JOIN integration.operations o ON o.id=$2 WHERE j.id=$1`, q.result.JobID, q.result.OperationID).Scan(&state, &generation); err != nil || state != "running" || generation < 1 {
		t.Fatalf("crash boundary was not leased: state=%s generation=%d err=%v", state, generation, err)
	}
	if err := child.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("killed child exited successfully")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("killed child not reaped")
	}
	close(release)
	q.pending(t)
	if pcCount(t, q, "payments.facts", "") != 0 {
		t.Fatal("crashed query created money")
	}
	// Age only this owned lease and River running timestamp; River itself must
	// rescue the same row. No state rewrite or duplicate enqueue is used.
	mustExec(t, f.owner, `UPDATE integration.operations SET lease_until=clock_timestamp()-interval '1 second' WHERE id=$1`, q.result.OperationID)
	mustExec(t, f.owner, `UPDATE river_payment.river_job SET attempted_at=clock_timestamp()-interval '2 hours' WHERE id=$1 AND state='running'`, q.result.JobID)
	var calls atomic.Int32
	opts := payments.DefaultQueryWorkerOptions()
	opts.RetryDelay = time.Second // bounded fixture rescue, using River's own scheduler
	body := pqSignedResponse(pcFull(q))
	opts.MockTransport = pqTransport(func(*http.Request) (*http.Response, error) {
		calls.Add(1)
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header)}, nil
	})
	client, err := payments.NewWorkerClient(context.Background(), q.worker, keys, "PROVIDER_MOCK", 1, opts)
	if err != nil {
		t.Fatal(err)
	}
	if err := client.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer pwStopClient(t, client)
	// Observe River's durable rescue marker: retryable is transient and may
	// already have advanced before this poll. No job state or due time is
	// manually rewritten after the crash.
	rescueDeadline := time.Now().Add(40 * time.Second)
	rescued := false
	for time.Now().Before(rescueDeadline) {
		if err := f.owner.QueryRow(context.Background(), `SELECT coalesce(errors::text LIKE '%Stuck job rescued by JobRescuer%',false) FROM river_payment.river_job WHERE id=$1`, q.result.JobID).Scan(&rescued); err != nil {
			t.Fatal(err)
		}
		if rescued {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if !rescued {
		t.Fatal("River did not rescue the crashed running job")
	}
	deadline := time.Now().Add(12 * time.Second)
	captured, work := 0, 0
	for time.Now().Before(deadline) {
		if err := f.owner.QueryRow(context.Background(), `SELECT (SELECT count(*) FROM payments.facts WHERE attempt_id=$1 AND kind='CAPTURED'),(SELECT count(*) FROM fulfillment.payment_work_items WHERE attempt_id=$1)`, q.result.AttemptID).Scan(&captured, &work); err != nil {
			t.Fatal(err)
		}
		if captured == 1 && work == 1 {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if captured != 1 || work != 1 {
		var jobState string
		var jobAttempt int
		var scheduled time.Time
		var rescued bool
		var finalGeneration int64
		if err := f.owner.QueryRow(context.Background(), `SELECT j.state,j.attempt,j.scheduled_at,j.errors::text LIKE '%Stuck job rescued by JobRescuer%',o.generation FROM river_payment.river_job j JOIN integration.operations o ON o.id=$2 WHERE j.id=$1`, q.result.JobID, q.result.OperationID).Scan(&jobState, &jobAttempt, &scheduled, &rescued, &finalGeneration); err != nil {
			t.Fatal(err)
		}
		t.Fatalf("rescue stopped before capture: job=%s attempt=%d scheduled_in=%s rescued=%t operation_generation=%d calls=%d observations=%d", jobState, jobAttempt, time.Until(scheduled), rescued, finalGeneration, calls.Load(), q.reportCount(t))
	}
	// Freeze the accepted recovery before checking counts; this worker keeps
	// polling after a successful observation using the fixture's short retry.
	pwStopClient(t, client)
	pcAssertStock(t, q, 0, 2, "CONFIRMED", "COMMITTED")
	pcAssertWork(t, q, "READY")
	if calls.Load() != 1 || q.reportCount(t) != 1 || pcCount(t, q, "payments.facts", " AND kind='CAPTURED'") != 1 {
		t.Fatalf("rescue query/report/capture counts: %d/%d/%d", calls.Load(), q.reportCount(t), pcCount(t, q, "payments.facts", " AND kind='CAPTURED'"))
	}
	var finalGeneration int64
	var finalAttempt int
	if err := f.owner.QueryRow(context.Background(), `SELECT o.generation,j.attempt FROM river_payment.river_job j JOIN integration.operations o ON o.id=$2 WHERE j.id=$1`, q.result.JobID, q.result.OperationID).Scan(&finalGeneration, &finalAttempt); err != nil || finalGeneration <= generation || finalAttempt < 1 {
		t.Fatalf("same job was not retried after lease recovery: generation=%d→%d attempt=%d err=%v", generation, finalGeneration, finalAttempt, err)
	}
}
