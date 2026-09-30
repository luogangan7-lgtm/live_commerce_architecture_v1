package attribution

import (
	"context"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"

	"livecommerce/internal/command"
	"livecommerce/internal/integrations/core"
)

// sweeper.go is capi_purchase_sweep_v1 (contract 6.4): every 5 minutes it plans one meta.capi.purchase operation for
// each CAPTURED payment attempt that passed consent, environment and freshness rules, and purges stale browser contexts.
// It plans only: the dispatcher (cmd/ads-worker) sends, and an UNKNOWN send is never re-planned (ads.capi_events is one
// row per attempt, ever), because Meta documents no server-to-server dedup (F15) and a resend could double-count a sale.
// SQL touched (EXECUTE commerce_worker): ads.plan_capi_candidates, ads.plan_capi_eligible, ads.plan_capi, ads.plan_capi_purge.
// River: inserts the external_operation_v1 job on queue ads priority 3 (core.InsertOperationJobOn; post_river/0015 admits it).

const (
	queueAds   = "ads"
	kindSweep  = "capi_purchase_sweep_v1"
	sweepLimit = 200 // contract 6.4: at most 200 attempts per run
	sweepEvery = 5 * time.Minute
	sweepMax   = 90 * time.Second
	txTimeout  = 10 * time.Second
)

type sweepArgs struct{}

func (sweepArgs) Kind() string { return kindSweep }

// PeriodicJobs is the 5-minute CAPI sweep on queue ads. Duplicate runs from several workers are harmless: a second
// planner of the same attempt loses on the ads.capi_events primary key and its transaction (job included) rolls back.
func PeriodicJobs() []*river.PeriodicJob {
	return []*river.PeriodicJob{river.NewPeriodicJob(river.PeriodicInterval(sweepEvery),
		func() (river.JobArgs, *river.InsertOpts) {
			return sweepArgs{}, &river.InsertOpts{Queue: queueAds, Priority: 3}
		},
		&river.PeriodicJobOpts{ID: kindSweep, RunOnStart: true})}
}

// AddWorkers registers the sweeper on w over the commerce_worker pool (also deletes stale capi_contexts).
func AddWorkers(w *river.Workers, pool *pgxpool.Pool) error {
	if w == nil || pool == nil {
		return command.ErrInvalid
	}
	return river.AddWorkerSafely(w, &sweepWorker{pool: pool})
}

type sweepWorker struct {
	river.WorkerDefaults[sweepArgs]
	pool *pgxpool.Pool
}

func (*sweepWorker) Timeout(*river.Job[sweepArgs]) time.Duration { return sweepMax }

// Work purges stale contexts, then plans up to sweepLimit attempts. One attempt's failure is logged (attempt id only,
// never a payload) and does not stop the others; the next run re-selects it because no capi_events row was committed.
func (w *sweepWorker) Work(ctx context.Context, _ *river.Job[sweepArgs]) error {
	client := river.ClientFromContext[pgx.Tx](ctx)
	// ads.plan_capi_purge: deletes contexts older than 8 days or whose owner withdrew or was erased (consent_allows false).
	if _, err := w.pool.Exec(ctx, `SELECT ads.plan_capi_purge()`); err != nil {
		slog.Error("capi context purge failed") // planning below re-checks consent itself, so a failed purge leaks nothing
	}
	// ads.plan_capi_candidates: CAPTURED facts <= 6 days old that ads.plan_capi_eligible accepts, oldest first.
	rows, err := w.pool.Query(ctx, `SELECT a::text FROM ads.plan_capi_candidates($1) a`, sweepLimit)
	if err != nil {
		return err
	}
	var attempts []string
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		attempts = append(attempts, id)
	}
	rows.Close()
	if err = rows.Err(); err != nil {
		return err
	}
	for _, id := range attempts {
		if err = w.planOne(ctx, client, id); err != nil {
			slog.Warn("capi plan failed", "attempt", id)
		}
	}
	return nil
}

// planOne is one attempt's short transaction: eligibility (consent included, CD5) is decided in it, the ads-lane job is
// inserted first (a rolled-back transaction drops it), and ads.plan_capi re-decides under a lock on the store settings.
func (w *sweepWorker) planOne(ctx context.Context, client *river.Client[pgx.Tx], attempt string) (err error) {
	ctx, cancel := context.WithTimeout(ctx, txTimeout)
	defer cancel()
	tx, err := w.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			rollbackCtx, stop := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
			defer stop()
			_ = tx.Rollback(rollbackCtx)
		}
	}()
	if _, err = tx.Exec(ctx, `SELECT set_config('statement_timeout','8000',true),set_config('lock_timeout','2000',true)`); err != nil {
		return err
	}
	var ok bool
	// ads.plan_capi_eligible: CD5 consent_allows, environment, dataset binding, ads:manage of capi_enabled_by, freshness.
	if err = tx.QueryRow(ctx, `SELECT ads.plan_capi_eligible($1::uuid)`, attempt).Scan(&ok); err != nil {
		return err
	}
	if !ok {
		return tx.Commit(ctx) // nothing planned: consent withdrawn or settings changed since the candidate query
	}
	var op string
	if err = tx.QueryRow(ctx, `SELECT gen_random_uuid()::text`).Scan(&op); err != nil {
		return err
	}
	job, err := core.InsertOperationJobOn(ctx, client, tx, op, queueAds, 3)
	if err != nil {
		return err
	}
	// ads.plan_capi: inserts the READY meta.capi.purchase operation + ads.capi_events row (one per attempt, ever).
	if _, err = tx.Exec(ctx, `SELECT ads.plan_capi($1::uuid,$2::uuid,$3)`, attempt, op, job); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
