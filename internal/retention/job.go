// job.go is the River side of package retention: the hourly job claims_retention_v1 that
// cmd/claims-worker registers (NewWorker + PeriodicJob) and that runs claims.run_retention on the
// dedicated retention-job pool. Non-goals: no choice of rows, no policy change (report-only vs
// enforced is claims.retention_policy, read by the definer), no scheduler of its own.

package retention

import (
	"context"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"
)

// JobKind is the River kind of the periodic retention job (main `river` schema, queue default).
const JobKind = "claims_retention_v1"

const (
	batchLimit = 500 // rows per class per batch (D9)
	maxBatches = 20  // batches per job: <= 20 x 500 rows per class per hour (contract §9)
)

// JobArgs carries nothing: the policy and the rows live in the database.
type JobArgs struct{}

// Kind returns JobKind.
func (JobArgs) Kind() string { return JobKind }

// InsertOpts makes the job unique per hour, so a restart within the hour inserts no second one
// (CRP09) and RunOnStart on two replicas still yields one job. The queue is River's default,
// the only queue cmd/claims-worker works.
func (JobArgs) InsertOpts() river.InsertOpts {
	return river.InsertOpts{UniqueOpts: river.UniqueOpts{ByPeriod: time.Hour}}
}

// Worker runs the batches. batch is the unit-test seam; NewWorker wires it to RunOnce.
type Worker struct {
	river.WorkerDefaults[JobArgs]
	pool  *pgxpool.Pool
	batch func(context.Context) (Counts, error)
	// budget is the unit-test seam of the time bound; zero means jobBudget.
	budget time.Duration
}

// NewWorker returns the job worker over the retention-job pool the caller already validated
// (platform.ValidateRetentionJobPool). A nil pool is an error.
func NewWorker(jobPool *pgxpool.Pool) (*Worker, error) {
	if jobPool == nil {
		return nil, ErrUsage
	}
	w := &Worker{pool: jobPool}
	w.batch = func(ctx context.Context) (Counts, error) { return RunOnce(ctx, w.pool, batchLimit) }
	return w, nil
}

// Work runs up to maxBatches batches, each its own transaction, and stops when more=0, busy=1 or the time budget is spent.
// A database error is returned so River retries the job; batches already committed stay committed.
func (w *Worker) Work(ctx context.Context, _ *river.Job[JobArgs]) error {
	if w == nil || w.batch == nil {
		return ErrUsage
	}
	total := Counts{}
	batches := 0
	budget := w.budget
	if budget == 0 {
		budget = jobBudget
	}
	start := time.Now()
	for batches < maxBatches {
		if err := ctx.Err(); err != nil {
			return err
		}
		if batches > 0 && time.Since(start) >= budget {
			break // the rest waits for the next hour; the job must end inside RescueWindow
		}
		c, err := w.batch(ctx)
		if err != nil {
			// retry: the definer is idempotent (rows already purged no longer match), so the same job may run again.
			return err
		}
		batches++
		if c["busy"] == 1 {
			// busy: another run holds hashtextextended('claims-retention',0); next hour retries.
			break
		}
		for k, v := range c {
			if k != "enforced" && k != "more" {
				total[k] += v
			}
		}
		total["enforced"], total["more"] = c["enforced"], c["more"]
		if c["more"] == 0 {
			break
		}
	}
	total["batches"] = int64(batches)
	// One fixed line, numbers only (contract §5).
	slog.Info("claims_retention_run", "counts", total.String())
	return nil
}

// RescueWindow is River's RescueStuckJobsAfter in cmd/claims-worker. A job running longer is rescued and a second
// runner starts beside the first, so the job bounds itself below it.
const RescueWindow = time.Minute

// jobBudget: no new batch starts after this; Timeout leaves the rest of the window for the batch in flight.
// What is left waits for the next hourly run (more stays 1), so a long backlog never outlives the rescue window.
const jobBudget = RescueWindow * 3 / 4

// Timeout bounds one job below RescueWindow (see jobBudget).
func (w *Worker) Timeout(*river.Job[JobArgs]) time.Duration { return RescueWindow - 5*time.Second }

// PeriodicJob is the hourly schedule, also run once at claims-worker start. The constructor
// returns no InsertOpts so JobArgs.InsertOpts (hourly uniqueness, queue default) applies.
func PeriodicJob() *river.PeriodicJob {
	return river.NewPeriodicJob(river.PeriodicInterval(time.Hour),
		func() (river.JobArgs, *river.InsertOpts) { return JobArgs{}, nil },
		&river.PeriodicJobOpts{ID: JobKind, RunOnStart: true})
}
