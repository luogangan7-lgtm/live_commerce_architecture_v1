package foundation_test

// TCV05 real child-process kill (contracts/taiwan-cvs-logistics-v1.md §10 TCV05: "real child-process kill after the fake records the create,
// restart -> one create, query recovers CREATED"; §7.4 reconcile, R-7b). Prefix `tsh`.
// The dispatcher runs in a CHILD PROCESS (this test binary re-executed on TestCvsChildWorker) against the ecpaytest fake served over real HTTP by
// the parent. The fake records the Create and then never answers; the parent SIGKILLs the child; a restarted dispatcher must reconcile with a signed
// Query V5 and find the trade: exactly one Create ever reached ECPay and the shipment becomes CREATED.
// Disclosed owner-pool write: River's rescuer would return the killed worker's `running` job to `retryable` after RescueStuckJobsAfter; the test applies
// exactly that transition at once instead of waiting an hour.

import (
	"context"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"

	integration "livecommerce/internal/integrations/core"
	"livecommerce/internal/integrations/shipping/ecpay"
	"livecommerce/internal/integrations/shipping/ecpay/ecpayroute"
	"livecommerce/internal/integrations/shipping/ecpay/ecpaytest"
	"livecommerce/internal/platform"
)

// TestCvsChildWorker is the child of the kill gate; it returns at once in a normal run (a helper, not a gate: a SKIP
// would read as an unexplained skip to the release gate, same pattern as TestT06DispatcherCrashChild).
func TestCvsChildWorker(t *testing.T) {
	dsn := os.Getenv("TCV_CHILD_DSN")
	if dsn == "" {
		return
	}
	ctx := context.Background()
	keys, err := ecpay.LoadKeyring(func(n string) string {
		if n == "ECPAY_LOGISTICS_KEYRING" {
			return os.Getenv("TCV_CHILD_KEYRING")
		}
		return ""
	})
	if err != nil {
		t.Fatal(err)
	}
	pool, err := platform.OpenWorkerPool(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	client, err := ecpay.NewClient(ecpay.EnvSandbox, ecpaytest.ForwardTransport(os.Getenv("TCV_CHILD_FAKE")))
	if err != nil {
		t.Fatal(err)
	}
	routes, err := ecpayroute.Routes(pool, keys, client, ecpay.Config{Enabled: true, HooksOrigin: tcvHooksOrigin})
	if err != nil {
		t.Fatal(err)
	}
	opts := integration.DefaultDispatcherOptions()
	opts.RetryDelay, opts.LeaseSeconds, opts.CallTimeout, opts.DBTimeout = 200*time.Millisecond, 8, 2*time.Second, time.Second
	disp, err := integration.NewDispatcher(ctx, pool, routes, opts)
	if err != nil {
		t.Fatal(err)
	}
	workers := river.NewWorkers()
	river.AddWorker(workers, disp)
	rc, err := river.NewClient(riverpgxv5.New(pool), &river.Config{Schema: "river", Workers: workers, Queues: map[string]river.QueueConfig{os.Getenv("TCV_CHILD_QUEUE"): {MaxWorkers: 1}},
		JobTimeout: time.Minute, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err != nil {
		t.Fatal(err)
	}
	if err := rc.Start(ctx); err != nil {
		t.Fatal(err)
	}
	os.Stdout.WriteString("TCV_CHILD_READY\n")
	select {} // until killed
}

func tshKillRestart(t *testing.T, e *tcvEnv, api string) {
	f := e.p.f
	ctx := context.Background()
	order, _ := e.cvsOrder(tcvOrderSpec{kind: "cvs_711", code: api, paymentMode: "pay_at_pickup"})
	st, out, raw := e.ship(e.token(), order, 0, "", false)
	if st != 202 {
		t.Fatalf("request: %d %s", st, raw)
	}
	opID := tcvStr(out, "operation_id")
	queue := "cvskill_" + strings.ReplaceAll(randomUUID(), "-", "")
	e.queue = queue
	e.route(opID) // the job now waits on the child's queue
	tn := e.tradeNo(order)

	srv := newFakeServer(e.fake)
	defer srv.Close()
	e.fake.SetCreateMode(ecpaytest.CreateTimeoutLost, "") // ECPay records the trade and never answers
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(self, "-test.run=^TestCvsChildWorker$", "-test.v", "-test.timeout=10m")
	cmd.Env = append(os.Environ(), "TCV_CHILD_DSN="+bcRole(t, f, "commerce_worker"), "TCV_CHILD_KEYRING="+e.keyringJSON, "TCV_CHILD_FAKE="+srv.URL, "TCV_CHILD_QUEUE="+queue)
	var childOut, childErr syncBuffer
	cmd.Stdout, cmd.Stderr = &childOut, &childErr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	exited := make(chan struct{})
	go func() {
		_ = cmd.Wait()
		close(exited)
	}()
	killed := false
	defer func() {
		if !killed {
			_ = cmd.Process.Kill()
			<-exited
		}
	}()
	readyBy := time.Now().Add(60 * time.Second)
	for !strings.Contains(childOut.String(), "TCV_CHILD_READY") {
		if time.Now().After(readyBy) {
			t.Fatalf("the child worker never became ready: stdout=%q stderr=%q", childOut.String(), childErr.String())
		}
		select {
		case <-exited:
			t.Fatalf("the child worker exited early: stdout=%q stderr=%q", childOut.String(), childErr.String())
		case <-time.After(100 * time.Millisecond):
		}
	}
	// wait until the fake has recorded the create, then kill the child hard (no shutdown hook can run)
	deadline := time.Now().Add(30 * time.Second)
	for e.fake.CreateCalls(tn) < 1 {
		if time.Now().After(deadline) {
			t.Fatal("the child never sent the Create")
		}
		time.Sleep(50 * time.Millisecond)
	}
	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	killed = true
	<-exited
	if s, _, _ := e.shipState(order); s != "REQUESTED" {
		t.Fatalf("after the kill the shipment is %s, want REQUESTED (Finish never ran)", s)
	}
	// River's rescuer transition, applied at once (disclosed): the killed worker's running job goes back to retryable
	var jobID int64
	_ = f.owner.QueryRow(ctx, `SELECT job_id FROM integration.operations WHERE id=$1`, opID).Scan(&jobID)
	tag, err := f.owner.Exec(ctx, `UPDATE river.river_job SET state='retryable',scheduled_at=clock_timestamp() WHERE id=$1 AND state='running'`, jobID)
	if err != nil || tag.RowsAffected() != 1 {
		t.Fatalf("rescue the killed worker's job: rows=%d err=%v", tag.RowsAffected(), err)
	}
	// restart: a fresh in-process dispatcher on the same queue; the lease of the dead claim expires, the reconcile signs a Query V5 and finds the trade
	e.fake.SetCreateMode(ecpaytest.CreateOK, "")
	queries := e.fake.CountCalls("QueryLogisticsTradeInfo")
	e.queueOver = queue
	e.startDispatcherWith(func(o *integration.DispatcherOptions) {
		o.LeaseSeconds, o.CallTimeout, o.DBTimeout = 8, 2*time.Second, time.Second
	})
	e.queueOver = ""
	e.awaitShipWithin(order, "CREATED", 90*time.Second)
	if got := e.fake.CreateCalls(tn); got != 1 {
		t.Errorf("%d Create calls reached ECPay for one MerchantTradeNo across the kill and restart, want exactly 1", got)
	}
	if e.fake.CountCalls("QueryLogisticsTradeInfo") <= queries {
		t.Error("the restarted dispatcher must recover through a signed Query V5")
	}
	if n := e.count(`SELECT count(*) FROM integration.operations WHERE id=$1 AND state='SUCCEEDED'`, opID); n != 1 {
		t.Error("the recovered operation must end SUCCEEDED")
	}
	_ = pgx.ErrNoRows
}

type syncBuffer struct {
	mu sync.Mutex
	b  []byte
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	s.b = append(s.b, p...)
	s.mu.Unlock()
	return len(p), nil
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return string(s.b)
}
