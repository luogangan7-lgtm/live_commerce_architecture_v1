package core

import (
	"context"
	"regexp"

	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"

	"livecommerce/internal/command"
)

// InsertOperationJob inserts the external_operation_v1 River job for an operation UUID the
// caller minted, inside the caller's transaction. It uses exactly the Plan shape (args
// {operation_id, version:1}, default queue, no InsertOpts) that the meta-claims-intake §5.4
// river_job guard admits from the claims-intake login. The caller inserts the matching
// integration.operations row (integration.plan_claim_reply) in the same transaction.
func InsertOperationJob(ctx context.Context, jobs *river.Client[pgx.Tx], tx pgx.Tx, operationID string) (int64, error) {
	if ctx == nil || jobs == nil || tx == nil || !command.ValidID(operationID) {
		return 0, command.ErrInvalid
	}
	job, err := jobs.InsertTx(ctx, tx, externalOperationArgs{OperationID: operationID, Version: 1}, nil)
	if err != nil {
		return 0, err
	}
	return job.Job.ID, nil
}

// jobQueuePattern is the River queue-name shape the ads guard admits (lowercase, <= 40 chars).
var jobQueuePattern = regexp.MustCompile(`^[a-z][a-z0-9_]{0,39}$`)

// validJobLane reports whether queue/priority are usable: River accepts priority 1 (highest) to 4.
func validJobLane(queue string, priority int) bool {
	return jobQueuePattern.MatchString(queue) && priority >= 1 && priority <= 4
}

// InsertOperationJobOn is InsertOperationJob with an explicit River queue and priority (A10-D3):
// same args and kind, no other InsertOpts (default MaxAttempts, no uniqueness). Ads operations use
// queue "ads" (priority 1 for pause) so cmd/claims-worker, which works only "default", never
// claims them into errRouteMissing. It touches river_job in the caller's transaction; the
// river_job guard that must admit this queue for the ads login is ads-core post_river/0015. An
// insert failure rolls back with the caller's plan row, so there is no retry decision to make here.
func InsertOperationJobOn(ctx context.Context, jobs *river.Client[pgx.Tx], tx pgx.Tx, operationID, queue string, priority int) (int64, error) {
	if ctx == nil || jobs == nil || tx == nil || !command.ValidID(operationID) || !validJobLane(queue, priority) {
		return 0, command.ErrInvalid
	}
	job, err := jobs.InsertTx(ctx, tx, externalOperationArgs{OperationID: operationID, Version: 1},
		&river.InsertOpts{Queue: queue, Priority: priority})
	if err != nil {
		return 0, err
	}
	return job.Job.ID, nil
}
