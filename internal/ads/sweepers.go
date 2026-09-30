package ads

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

// sweepers.go registers the three periodic jobs of cmd/ads-worker on queue ads (contract 6.2/6.3, D9): the 30 s advance
// sweeper, the hourly insights planner and the 5 min OAuth-state purge. They plan operations and book results; they never call
// Meta (the dispatcher runs the operations), never hold a lock across I/O (each draft is one short transaction), and never
// plan an activate (ads.advance_decide does, only through the Check-gated path).
// SQL touched: ads.advance_candidates/advance_next/advance_plan, ads.pending_insight_reads/put_insights_day,
// ads.insights_candidates/insights_days/insights_plan, ads.purge_oauth_states (EXECUTE commerce_worker).
// The capi_purchase_sweep_v1 periodic kind is admitted by post_river/0015 but registered by ads-capi, not here.

const (
	queueAds      = "ads"
	advancePage   = 50  // candidate page size; every page is listed and advanced each run (review R1: a fixed first page starved drafts 51+)
	insightsLimit = 200 // drafts per hourly run; bounded because each plans up to 4 reads
	ingestLimit   = 200
	sweepTimeout  = 90 * time.Second
	txTimeout     = 10 * time.Second
	kindAdvance   = "ads_publish_advance_v1"
	kindInsights  = "ads_insights_plan_v1"
	kindPurge     = "ads_oauth_purge_v1"
)

type advanceArgs struct{}

func (advanceArgs) Kind() string { return kindAdvance }

type insightsArgs struct{}

func (insightsArgs) Kind() string { return kindInsights }

type purgeArgs struct{}

func (purgeArgs) Kind() string { return kindPurge }

// PeriodicJobs are the three periodic jobs, all on queue ads (30 s / hourly / 5 min). Duplicate runs from several workers
// are harmless: every planner is idempotent by semantic key and serialized by the store settings and draft row locks.
func PeriodicJobs() []*river.PeriodicJob {
	mk := func(every time.Duration, args river.JobArgs, id string) *river.PeriodicJob {
		return river.NewPeriodicJob(river.PeriodicInterval(every),
			func() (river.JobArgs, *river.InsertOpts) {
				return args, &river.InsertOpts{Queue: queueAds, Priority: 3}
			},
			&river.PeriodicJobOpts{ID: id, RunOnStart: true})
	}
	return []*river.PeriodicJob{
		mk(30*time.Second, advanceArgs{}, kindAdvance),
		mk(time.Hour, insightsArgs{}, kindInsights),
		mk(5*time.Minute, purgeArgs{}, kindPurge),
	}
}

// AddWorkers registers the three sweeper workers on w over the commerce_worker pool.
func AddWorkers(w *river.Workers, pool *pgxpool.Pool) error {
	if w == nil || pool == nil {
		return command.ErrInvalid
	}
	if err := river.AddWorkerSafely(w, &advanceWorker{pool: pool}); err != nil {
		return err
	}
	if err := river.AddWorkerSafely(w, &insightsWorker{pool: pool}); err != nil {
		return err
	}
	return river.AddWorkerSafely(w, &purgeWorker{pool: pool})
}

type advanceWorker struct {
	river.WorkerDefaults[advanceArgs]
	pool *pgxpool.Pool
}

func (*advanceWorker) Timeout(*river.Job[advanceArgs]) time.Duration { return sweepTimeout }

// Work ingests finished insight reads, then advances every candidate draft in approved_at order (paged). One draft's failure is
// logged (draft id only, never a payload) and does not stop the others; nothing here retries a remote effect.
func (w *advanceWorker) Work(ctx context.Context, _ *river.Job[advanceArgs]) error {
	client := river.ClientFromContext[pgx.Tx](ctx)
	rows, err := w.pool.Query(ctx, `SELECT o FROM ads.pending_insight_reads($1) o`, ingestLimit)
	if err != nil {
		return err
	}
	var reads []string
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		reads = append(reads, id)
	}
	rows.Close()
	if err = rows.Err(); err != nil {
		return err
	}
	for _, id := range reads {
		// ads.put_insights_day: parses the read's provider_reference, upserts insights_daily; idempotent per read.
		if _, err = w.pool.Exec(ctx, `SELECT ads.put_insights_day($1::uuid)`, id); err != nil {
			slog.Warn("ads insights ingest failed", "operation", id)
		}
	}
	return w.advanceAll(ctx, client)
}

// advanceAll lists every page of ads.advance_candidates first, then advances the drafts in that order: advancing pins ids and
// ends drafts, which would shift a later page's offset and skip rows if the two were interleaved. Stops at the job deadline
// (sweepTimeout); the next run starts again from the oldest approval, so no draft is starved by a fixed first page.
func (w *advanceWorker) advanceAll(ctx context.Context, client *river.Client[pgx.Tx]) error {
	var drafts []string
	for offset := 0; ; offset += advancePage {
		// ads.advance_candidates: published drafts in approved_at order; (limit, offset) is one stable page of that order.
		page, err := w.pool.Query(ctx, `SELECT d FROM ads.advance_candidates($1,$2) d`, advancePage, offset)
		if err != nil {
			return err
		}
		n := 0
		for page.Next() {
			var id string
			if err = page.Scan(&id); err != nil {
				page.Close()
				return err
			}
			drafts = append(drafts, id)
			n++
		}
		page.Close()
		if err = page.Err(); err != nil {
			return err
		}
		if n < advancePage {
			break
		}
	}
	for _, id := range drafts {
		if ctx.Err() != nil {
			return nil // deadline: the rest is picked up by the next run
		}
		if err := w.advanceOne(ctx, client, id); err != nil {
			slog.Warn("ads advance failed", "draft", id)
		}
	}
	return nil
}

// advanceOne is one draft's short transaction: advance_next decides and books (pins, ends), the ads-lane job is inserted first
// (a rolled-back transaction drops it, so no orphan job commits), advance_plan re-decides under the same locks and plans.
func (w *advanceWorker) advanceOne(ctx context.Context, client *river.Client[pgx.Tx], draft string) error {
	return inTx(ctx, w.pool, func(tx pgx.Tx) error {
		var kind string
		// ads.advance_next: takes the store settings lock then the draft lock, pins remote ids, ends drafts past ends_at.
		if err := tx.QueryRow(ctx, `SELECT ads.advance_next($1::uuid)`, draft).Scan(&kind); err != nil {
			return err
		}
		if kind == "" {
			return nil // commit: the pins and ended_at written above are the run's result
		}
		prio := 3
		if kind == "pause" || kind == "pause_retry" {
			prio = 1 // pause lane
		}
		var op string
		if err := tx.QueryRow(ctx, `SELECT gen_random_uuid()::text`).Scan(&op); err != nil {
			return err
		}
		// core.InsertOperationJobOn: queue ads; post_river/0015 admits it for the worker login.
		job, err := core.InsertOperationJobOn(ctx, client, tx, op, queueAds, prio)
		if err != nil {
			return err
		}
		if kind == "pause_retry" {
			// Contract 5.3: a pause not SUCCEEDED within 15 minutes raises an ops alert (a fresh seq is planned, never the same key).
			slog.Error("ads pause retry planned", "draft", draft)
		}
		_, err = tx.Exec(ctx, `SELECT ads.advance_plan($1::uuid,$2,$3::uuid,$4)`, draft, kind, op, job)
		return err
	})
}

type insightsWorker struct {
	river.WorkerDefaults[insightsArgs]
	pool *pgxpool.Pool
}

func (*insightsWorker) Timeout(*river.Job[insightsArgs]) time.Duration { return sweepTimeout }

// Work plans the read_insights operations of every draft with a pinned campaign (any derived status). It never reads Meta.
func (w *insightsWorker) Work(ctx context.Context, _ *river.Job[insightsArgs]) error {
	client := river.ClientFromContext[pgx.Tx](ctx)
	drafts, err := candidates(ctx, w.pool, `SELECT d FROM ads.insights_candidates($1) d`, insightsLimit)
	if err != nil {
		return err
	}
	for _, id := range drafts {
		if err = w.planOne(ctx, client, id); err != nil {
			slog.Warn("ads insights plan failed", "draft", id)
		}
	}
	return nil
}

func (w *insightsWorker) planOne(ctx context.Context, client *river.Client[pgx.Tx], draft string) error {
	return inTx(ctx, w.pool, func(tx pgx.Tx) error {
		var days []time.Time
		// ads.insights_days: every hour while AD6 counts the draft, else only in the 04:00 Asia/Taipei hour (D9).
		if err := tx.QueryRow(ctx, `SELECT ads.insights_days($1::uuid)`, draft).Scan(&days); err != nil {
			return err
		}
		for _, day := range days {
			var op string
			if err := tx.QueryRow(ctx, `SELECT gen_random_uuid()::text`).Scan(&op); err != nil {
				return err
			}
			job, err := core.InsertOperationJobOn(ctx, client, tx, op, queueAds, 3)
			if err != nil {
				return err
			}
			if _, err = tx.Exec(ctx, `SELECT ads.insights_plan($1::uuid,$2::date,$3::uuid,$4)`, draft, day.Format("2006-01-02"), op, job); err != nil {
				return err
			}
		}
		return nil
	})
}

type purgeWorker struct {
	river.WorkerDefaults[purgeArgs]
	pool *pgxpool.Pool
}

func (*purgeWorker) Timeout(*river.Job[purgeArgs]) time.Duration { return sweepTimeout }

// Work deletes pending sealed token material at or after each state's expires_at (ads.purge_oauth_states).
func (w *purgeWorker) Work(ctx context.Context, _ *river.Job[purgeArgs]) error {
	_, err := w.pool.Exec(ctx, `SELECT ads.purge_oauth_states()`)
	return err
}

// candidates runs a `SELECT d FROM ads.<lister>($1) d` query returning draft ids.
func candidates(ctx context.Context, pool *pgxpool.Pool, sql string, limit int) ([]string, error) {
	rows, err := pool.Query(ctx, sql, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// inTx runs fn in one READ COMMITTED transaction with short statement and lock timeouts; any error rolls back (including the
// River job inserted inside), a nil return commits.
func inTx(ctx context.Context, pool *pgxpool.Pool, fn func(pgx.Tx) error) (err error) {
	ctx, cancel := context.WithTimeout(ctx, txTimeout)
	defer cancel()
	tx, err := pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
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
	if err = fn(tx); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
