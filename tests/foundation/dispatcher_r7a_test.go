// R-7a dispatcher extension (taiwan-cvs-logistics-v1 §0.2 R-7a, cvs-ecpay E1/E3, rulings B17),
// tier REAL_PG. Owns: DispatchRoute.Finish runs in the completion transaction before
// Service.Complete on every completion path, receives the adapter's Outcome.Detail and a
// SecretClaim carrying the claim mode, and a Finish error rolls the whole transaction back so the
// next claim reconciles instead of re-dispatching. SecretClaim.Mode reaches LoadSecret.
// Non-goals: the ECPay route's own Finish SQL (cvs-core/cvs-tests TCV05).
package foundation_test

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	integration "livecommerce/internal/integrations/core"
)

type r7aFinishCall struct {
	mode, state, detail, rowState, rowMode, xid string
}

// r7aFinishRecorder records what Finish saw inside its tx: the operation row must still be
// claimed (DISPATCHING, or UNKNOWN on a reconcile claim) under the same claim mode (Complete runs
// after Finish), and the tx id is kept to
// prove Complete committed in that same transaction.
func r7aFinishRecorder(calls *[]r7aFinishCall, mu *sync.Mutex, fail func(context.Context, pgx.Tx, integration.SecretClaim) error) func(context.Context, pgx.Tx, integration.SecretClaim, integration.Outcome) error {
	return func(ctx context.Context, tx pgx.Tx, claim integration.SecretClaim, outcome integration.Outcome) error {
		c := r7aFinishCall{mode: claim.Mode, state: outcome.State}
		c.detail, _ = outcome.Detail.(string)
		if err := tx.QueryRow(ctx, `SELECT state, coalesce(lease_mode,''), (txid_current() % 4294967296)::text FROM integration.operations WHERE id=$1`, claim.OperationID).Scan(&c.rowState, &c.rowMode, &c.xid); err != nil {
			return err
		}
		mu.Lock()
		*calls = append(*calls, c)
		mu.Unlock()
		if fail != nil {
			return fail(ctx, tx, claim)
		}
		return nil
	}
}

func TestT06DispatcherR7aFinishCommitsWithCompletionAndSeesMode(t *testing.T) {
	f := newT06GoFixture(t)
	b := f.register(t, f.store, "r7a-finish-binding")
	p := f.plan(t, "r7a-finish-op", b, `{"amount":1}`)
	queue := t06Queue(t, f, p)
	var mu sync.Mutex
	var calls []r7aFinishCall
	var loaderMode atomic.Value
	route := t06Route()
	route.Dispatch = nil
	route.LoadSecret = func(ctx context.Context, tx pgx.Tx, claim integration.SecretClaim) (integration.Secret, error) {
		var rowMode string
		if err := tx.QueryRow(ctx, `SELECT coalesce(lease_mode,'') FROM integration.operations WHERE id=$1`, claim.OperationID).Scan(&rowMode); err != nil {
			return integration.Secret{}, err
		}
		loaderMode.Store(claim.Mode + "/" + rowMode)
		return integration.NewSecret([]byte("synthetic-key")), nil
	}
	route.DispatchWithSecret = func(context.Context, integration.DispatchRequest, integration.Secret) (integration.Outcome, error) {
		return integration.Outcome{State: "SUCCEEDED", Code: "mock_observed", ProviderReference: "mock-1", Detail: "label-detail"}, nil
	}
	route.Finish = r7aFinishRecorder(&calls, &mu, nil)
	t06StartDispatcher(t, f.worker, queue, []integration.DispatchRoute{route}, t06DispatchOptions())
	t06Await(t, f, p, "SUCCEEDED", "completed", 5*time.Second)
	if got, _ := loaderMode.Load().(string); got != "dispatch/dispatch" {
		t.Fatalf("LoadSecret claim/row mode = %q", got)
	}
	var xmin string
	if err := f.worker.QueryRow(context.Background(), `SELECT xmin::text FROM integration.operations WHERE id=$1`, p.OperationID).Scan(&xmin); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	want := r7aFinishCall{mode: "dispatch", state: "SUCCEEDED", detail: "label-detail", rowState: "DISPATCHING", rowMode: "dispatch", xid: xmin}
	if len(calls) != 1 || calls[0] != want {
		t.Fatalf("finish calls = %+v, want one %+v", calls, want)
	}
}

func TestT06DispatcherR7aFinishErrorRollsBackAndReconciles(t *testing.T) {
	f := newT06GoFixture(t)
	b := f.register(t, f.store, "r7a-rollback-binding")
	p := f.plan(t, "r7a-rollback-op", b, `{"amount":1}`)
	queue := t06Queue(t, f, p)
	var mu sync.Mutex
	var calls []r7aFinishCall
	var dispatches, queries, finishes atomic.Int64
	var leakErr atomic.Value
	route := t06Route()
	route.Dispatch = func(context.Context, integration.DispatchRequest) (integration.Outcome, error) {
		dispatches.Add(1)
		return integration.Outcome{State: "SUCCEEDED", Code: "mock_observed", ProviderReference: "mock-1", Detail: "d1"}, nil
	}
	route.Reconcile = func(context.Context, integration.DispatchRequest) (integration.Outcome, error) {
		queries.Add(1)
		return integration.Outcome{State: "SUCCEEDED", Code: "queried", ProviderReference: "mock-1", Detail: "r1"}, nil
	}
	route.Finish = r7aFinishRecorder(&calls, &mu, func(ctx context.Context, tx pgx.Tx, claim integration.SecretClaim) error {
		if finishes.Add(1) > 1 {
			return nil
		}
		// A write made in the completion tx (here: a lease-fenced terminal completion) must vanish
		// with the Finish error; it also proves the SecretClaim carries a valid lease fence.
		if _, err := tx.Exec(ctx, `SELECT integration.complete_operation($1,$2,$3,'FAILED_FINAL','finish_leak','')`, claim.OperationID, claim.Generation, claim.LeaseToken); err != nil {
			leakErr.Store(err.Error())
		}
		return errors.New("synthetic finish db fault")
	})
	opts := t06DispatchOptions()
	opts.LeaseSeconds = 5
	opts.CallTimeout = 100 * time.Millisecond
	opts.DBTimeout = 500 * time.Millisecond
	t06StartDispatcher(t, f.worker, queue, []integration.DispatchRoute{route}, opts)
	code := t06Await(t, f, p, "SUCCEEDED", "completed", 20*time.Second)
	if v, _ := leakErr.Load().(string); v != "" {
		t.Fatalf("finish write setup failed: %s", v)
	}
	if code != "queried" || dispatches.Load() != 1 || queries.Load() != 1 {
		t.Fatalf("code=%s dispatches=%d queries=%d", code, dispatches.Load(), queries.Load())
	}
	var leaked bool
	if err := f.worker.QueryRow(context.Background(), `SELECT EXISTS(SELECT 1 FROM integration.operation_events WHERE operation_id=$1 AND reason_code='finish_leak')`, p.OperationID).Scan(&leaked); err != nil {
		t.Fatal(err)
	}
	if leaked {
		t.Fatal("write from a failed Finish committed")
	}
	mu.Lock()
	defer mu.Unlock()
	if len(calls) != 2 || calls[0].mode != "dispatch" || calls[0].detail != "d1" || calls[1].mode != "reconcile" || calls[1].detail != "r1" ||
		calls[1].rowState != "UNKNOWN" || calls[1].rowMode != "reconcile" {
		t.Fatalf("finish calls = %+v", calls)
	}
}

func TestT06DispatcherR7aFinishRunsOnPolicyBlock(t *testing.T) {
	f := newT06GoFixture(t)
	b := f.register(t, f.store, "r7a-blocked-binding")
	p := f.plan(t, "r7a-blocked-op", b, `{"amount":1}`)
	queue := t06Queue(t, f, p)
	var mu sync.Mutex
	var calls []r7aFinishCall
	var dispatches atomic.Int64
	route := t06Route()
	route.Check = func(context.Context, integration.DispatchRequest) error { return integration.ErrPolicyDenied }
	route.Dispatch = func(context.Context, integration.DispatchRequest) (integration.Outcome, error) {
		dispatches.Add(1)
		return integration.Outcome{State: "SUCCEEDED", Code: "mock_observed"}, nil
	}
	route.Finish = r7aFinishRecorder(&calls, &mu, nil)
	t06StartDispatcher(t, f.worker, queue, []integration.DispatchRoute{route}, t06DispatchOptions())
	t06Await(t, f, p, "BLOCKED_POLICY", "completed", 5*time.Second)
	mu.Lock()
	defer mu.Unlock()
	if dispatches.Load() != 0 || len(calls) != 1 || calls[0].state != "BLOCKED_POLICY" || calls[0].mode != "dispatch" || calls[0].detail != "" {
		t.Fatalf("dispatches=%d finish calls=%+v", dispatches.Load(), calls)
	}
}
