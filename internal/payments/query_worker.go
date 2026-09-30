package payments

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"

	"livecommerce/internal/command"
	"livecommerce/internal/integrations/accounts"
	"livecommerce/internal/integrations/core"
	"livecommerce/internal/integrations/psp/payuni"
	"livecommerce/internal/jobqueue"
	"livecommerce/internal/platform"
)

var (
	errPaymentQueryJob      = errors.New("payment_query_invalid_job")
	errPaymentQueryDatabase = errors.New("payment_query_database")
	errPaymentQueryBudget   = errors.New("payment_query_budget_exhausted")
	errPaymentQueryFamily   = errors.New("payment_query_wrong_family")
)

type paymentQueryArgs struct {
	OperationID string `json:"operation_id"`
	Version     int    `json:"version"`
}

func (paymentQueryArgs) Kind() string { return "payment_query_v1" }

type QueryWorkerOptions struct {
	LeaseSeconds   int
	DBTimeout      time.Duration
	CallTimeout    time.Duration
	RetryDelay     time.Duration
	MaxAge         time.Duration
	MaxGenerations int64
	MockTransport  http.RoundTripper
}

func DefaultQueryWorkerOptions() QueryWorkerOptions {
	return QueryWorkerOptions{LeaseSeconds: 30, DBTimeout: 2 * time.Second,
		CallTimeout: 10 * time.Second, RetryDelay: 2 * time.Minute,
		MaxAge: 24 * time.Hour, MaxGenerations: 720}
}

func validQueryWorkerOptions(o QueryWorkerOptions) bool {
	if o.LeaseSeconds < 5 || o.LeaseSeconds > 300 ||
		o.DBTimeout < 100*time.Millisecond || o.DBTimeout > 5*time.Second ||
		o.CallTimeout < 100*time.Millisecond || o.CallTimeout > 60*time.Second ||
		o.RetryDelay < time.Second || o.RetryDelay > time.Hour ||
		o.MaxAge < time.Minute || o.MaxAge > 7*24*time.Hour ||
		o.MaxGenerations < 2 || o.MaxGenerations > 10000 {
		return false
	}
	return o.CallTimeout+2*o.DBTimeout+time.Second < time.Duration(o.LeaseSeconds)*time.Second
}

// QueryWorker only queries a frozen buyer attempt. The caller owns the pool
// and River lifecycle.
type QueryWorker struct {
	river.WorkerDefaults[paymentQueryArgs]
	pool    *pgxpool.Pool
	keys    *accounts.Keyring
	profile string
	options QueryWorkerOptions
	core    core.Service
	jobs    *river.Client[pgx.Tx]
	stripe  *StripeRuntime
}

var _ river.Worker[paymentQueryArgs] = (*QueryWorker)(nil)

func NewQueryWorker(ctx context.Context, pool *pgxpool.Pool, keys *accounts.Keyring,
	profile string, options QueryWorkerOptions) (*QueryWorker, error) {
	if ctx == nil || keys == nil || !validQueryWorkerProfile(profile) || !validQueryWorkerOptions(options) ||
		(profile == "PROVIDER_MOCK") != (options.MockTransport != nil) {
		return nil, errPaymentQueryJob
	}
	if err := platform.ValidateWorkerPool(ctx, pool); err != nil {
		return nil, errPaymentQueryDatabase
	}
	// Reconciliation jobs enter the same schema that payment consumers maintain.
	jobs, err := river.NewClient(riverpgxv5.New(pool), &river.Config{Schema: "river_payment"})
	if err != nil {
		return nil, errPaymentQueryDatabase
	}
	return &QueryWorker{pool: pool, keys: keys, profile: profile, options: options, jobs: jobs}, nil
}

func validQueryWorkerProfile(profile string) bool {
	return profile == "PROVIDER_MOCK" || profile == "SANDBOX" || profile == "LIVE"
}

func (w *QueryWorker) Timeout(*river.Job[paymentQueryArgs]) time.Duration {
	if w == nil {
		return 0
	}
	return time.Duration(w.options.LeaseSeconds) * time.Second
}

func (w *QueryWorker) NextRetry(*river.Job[paymentQueryArgs]) time.Time {
	if w == nil {
		return time.Time{}
	}
	return time.Now().UTC().Add(w.options.RetryDelay)
}

type queryOperation struct {
	ActorKind, Provider, Action, Purpose, State, ResultCode string
	Generation                                              int64
	LeaseUntil                                              *time.Time
}

func (w *QueryWorker) Work(ctx context.Context, job *river.Job[paymentQueryArgs]) error {
	if ctx == nil || w == nil || w.pool == nil || w.keys == nil || job == nil ||
		job.Args.Version != 1 || !command.ValidID(job.Args.OperationID) {
		return errPaymentQueryJob
	}
	id := job.Args.OperationID
	op, err := w.read(ctx, id)
	if err != nil {
		return w.dbError(ctx)
	}
	if !validQueryOperation(op) {
		return river.JobCancel(errPaymentQueryFamily)
	}
	if op.Provider == "stripe" {
		// A disabled Stripe worker leaves the job and operation untouched for an enabled worker.
		if w.stripe == nil {
			return river.JobSnooze(5 * time.Second)
		}
		return w.stripeStep(ctx, id, op)
	}
	if op.ResultCode == "payment_query_budget_exhausted" && op.LeaseUntil == nil {
		return river.JobCancel(errPaymentQueryBudget)
	}
	if terminalQueryState(op.State) {
		return nil
	}
	claim, providerReference, err := w.claim(ctx, id)
	if err != nil {
		return w.dbError(ctx)
	}
	switch claim.Disposition {
	case "busy":
		return river.JobSnooze(w.options.RetryDelay)
	case "terminal":
		return nil
	case "blocked_binding":
		return river.JobCancel(errPaymentQueryFamily)
	case "claimed":
		if claim.Mode != "reconcile" {
			return errPaymentQueryDatabase
		}
	default:
		return errPaymentQueryDatabase
	}
	if claim.Generation > w.options.MaxGenerations {
		return w.finish(ctx, id, claim, providerReference, "payment_query_budget_exhausted", true)
	}

	// The load and the one wire query share one deadline. The load commits
	// before any provider I/O, and the query uses the parent's cancellation.
	callCtx, cancel := context.WithTimeout(ctx, w.options.CallTimeout)
	defer cancel()
	material, err := w.load(callCtx, id, claim)
	if ctx.Err() != nil {
		return river.JobSnooze(w.options.RetryDelay)
	}
	if callCtx.Err() != nil {
		return w.finish(ctx, id, claim, providerReference, "payment_query_timeout", false)
	}
	if material.Age >= w.options.MaxAge {
		return w.finish(ctx, id, claim, providerReference, "payment_query_budget_exhausted", true)
	}
	if err != nil {
		return w.finish(ctx, id, claim, providerReference, "payment_query_material_unavailable", false)
	}
	observation, wireErr, panicked := queryOnce(callCtx, material)
	if ctx.Err() != nil {
		return river.JobSnooze(w.options.RetryDelay)
	}
	if callCtx.Err() != nil {
		return w.finish(ctx, id, claim, providerReference, "payment_query_timeout", false)
	}
	if panicked {
		return w.finish(ctx, id, claim, providerReference, "payment_query_panic", false)
	}
	if wireErr != nil {
		return w.finish(ctx, id, claim, providerReference, "payment_query_wire_failed", false)
	}
	if err := w.record(ctx, id, claim, observation); err != nil {
		if ctx.Err() != nil {
			return river.JobSnooze(w.options.RetryDelay)
		}
		return w.finish(ctx, id, claim, providerReference, "payment_query_record_failed", false)
	}
	return river.JobSnooze(queryRetryDelay(w.options.RetryDelay, observation.DataSource))
}

func queryRetryDelay(configured time.Duration, source string) time.Duration {
	if source == "B" && configured < 10*time.Minute {
		return 10 * time.Minute
	}
	return configured
}

func validQueryOperation(op queryOperation) bool {
	return op.ActorKind == "BUYER_PAYMENT_QUERY" && op.Purpose == "transactional" &&
		((op.Provider == "payuni" && op.Action == "payuni.query") ||
			(op.Provider == "stripe" && op.Action == "stripe.checkout_session"))
}

func terminalQueryState(state string) bool {
	return state == "SUCCEEDED" || state == "FAILED_FINAL" || state == "CANCELLED" ||
		state == "BLOCKED_POLICY" || state == "STALE_BINDING"
}

func (w *QueryWorker) read(ctx context.Context, id string) (queryOperation, error) {
	bounded, cancel := context.WithTimeout(ctx, w.options.DBTimeout)
	defer cancel()
	var op queryOperation
	err := w.pool.QueryRow(bounded, `SELECT actor_kind,provider,action,purpose,state,generation,
		lease_until,result_code FROM integration.operations WHERE id=$1::uuid`, id).
		Scan(&op.ActorKind, &op.Provider, &op.Action, &op.Purpose, &op.State,
			&op.Generation, &op.LeaseUntil, &op.ResultCode)
	return op, err
}

func (w *QueryWorker) claim(ctx context.Context, id string) (core.ClaimResult, string, error) {
	bounded, cancel := context.WithTimeout(ctx, w.options.DBTimeout)
	defer cancel()
	tx, err := w.pool.BeginTx(bounded, pgx.TxOptions{})
	if err != nil {
		return core.ClaimResult{}, "", err
	}
	defer w.rollback(tx)
	claim, err := w.core.Claim(bounded, tx, id, w.options.LeaseSeconds)
	if err != nil {
		return core.ClaimResult{}, "", err
	}
	var providerReference string
	if err := tx.QueryRow(bounded, `SELECT provider_reference FROM integration.operations WHERE id=$1::uuid`, id).
		Scan(&providerReference); err != nil {
		return core.ClaimResult{}, "", err
	}
	if err := tx.Commit(bounded); err != nil {
		return core.ClaimResult{}, "", err
	}
	return claim, providerReference, nil
}

func (w *QueryWorker) load(ctx context.Context, id string, claim core.ClaimResult) (accounts.PaymentQueryMaterial, error) {
	bounded, cancel := context.WithTimeout(ctx, w.options.DBTimeout)
	defer cancel()
	tx, err := w.pool.BeginTx(bounded, pgx.TxOptions{})
	if err != nil {
		return accounts.PaymentQueryMaterial{}, err
	}
	defer w.rollback(tx)
	var material accounts.PaymentQueryMaterial
	if w.options.MockTransport != nil {
		material, err = w.keys.LoadPaymentQuery(bounded, tx, id, claim.Generation, claim.LeaseToken,
			w.profile, w.options.MockTransport)
	} else {
		material, err = w.keys.LoadPaymentQuery(bounded, tx, id, claim.Generation, claim.LeaseToken, w.profile)
	}
	if err != nil {
		return material, err
	}
	if err = tx.Commit(bounded); err != nil {
		return accounts.PaymentQueryMaterial{}, err
	}
	return material, nil
}

func queryOnce(ctx context.Context, material accounts.PaymentQueryMaterial) (observation payuni.Observation, err error, panicked bool) {
	defer func() {
		if recover() != nil {
			observation, err, panicked = payuni.Observation{}, nil, true
		}
	}()
	observation, err = material.Client.Query(ctx, material.Expected, time.Now().Unix())
	return observation, err, false
}

func (w *QueryWorker) record(ctx context.Context, id string, claim core.ClaimResult, report payuni.Observation) error {
	if w == nil || w.jobs == nil {
		return errPaymentQueryDatabase
	}
	encoded, err := json.Marshal(report)
	if err != nil {
		return errPaymentQueryDatabase
	}
	bounded, cancel := context.WithTimeout(ctx, w.options.DBTimeout)
	defer cancel()
	tx, err := w.pool.BeginTx(bounded, pgx.TxOptions{})
	if err != nil {
		return err
	}
	defer w.rollback(tx)
	var reportHash string
	if err = tx.QueryRow(bounded, `SELECT encode(sha256(convert_to($1::jsonb::text,'UTF8')),'hex')`,
		encoded).Scan(&reportHash); err != nil {
		return err
	}
	job, err := w.jobs.InsertTx(bounded, tx, paymentReconcileArgs{
		OperationID: id, ReportHash: reportHash, Version: 1}, &river.InsertOpts{Queue: jobqueue.ForProfile(w.profile)})
	if err != nil {
		return err
	}
	if _, err = tx.Exec(bounded, `SELECT integration.record_payment_query($1::uuid,$2::bigint,$3::bytea,$4::text,$5::jsonb,$6::bigint)`,
		id, claim.Generation, claim.LeaseToken, w.profile, encoded, job.Job.ID); err != nil {
		return err
	}
	return tx.Commit(bounded)
}

func (w *QueryWorker) finish(ctx context.Context, id string, claim core.ClaimResult,
	providerReference, code string, exhausted bool) error {
	if ctx.Err() != nil {
		return river.JobSnooze(w.options.RetryDelay)
	}
	bounded, cancel := context.WithTimeout(ctx, w.options.DBTimeout)
	defer cancel()
	tx, err := w.pool.BeginTx(bounded, pgx.TxOptions{})
	if err != nil {
		return errPaymentQueryDatabase
	}
	defer w.rollback(tx)
	if _, err = tx.Exec(bounded, `SELECT integration.finish_payment_query($1::uuid,$2::bigint,$3::bytea,$4::text,$5::text,$6::text)`,
		id, claim.Generation, claim.LeaseToken, w.profile, code, providerReference); err != nil {
		return errPaymentQueryDatabase
	}
	if err = tx.Commit(bounded); err != nil {
		return errPaymentQueryDatabase
	}
	if exhausted {
		return river.JobCancel(errPaymentQueryBudget)
	}
	return river.JobSnooze(w.options.RetryDelay)
}

func (w *QueryWorker) dbError(ctx context.Context) error {
	if ctx.Err() != nil {
		return river.JobSnooze(w.options.RetryDelay)
	}
	return errPaymentQueryDatabase
}

func (w *QueryWorker) rollback(tx pgx.Tx) {
	cleanup, cancel := context.WithTimeout(context.Background(), w.options.DBTimeout)
	defer cancel()
	_ = tx.Rollback(cleanup)
}
