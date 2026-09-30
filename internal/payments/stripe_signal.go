// stripe_signal.go owns River's Stripe signal kind on the existing payment queue.
// It never treats an unauthenticated job argument as a payment or stock decision. A signal is a wake-up:
// checkout signals retrieve the Checkout Session, refund signals (refund_id set) retrieve that refund,
// and charge signals (receipt object_type charge, stripe-refund-v1 §6) record a charge snapshot.

package payments

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"

	"livecommerce/internal/command"
	"livecommerce/internal/integrations/core"
	"livecommerce/internal/integrations/psp/stripe"
	"livecommerce/internal/jobqueue"
	"livecommerce/internal/platform"
)

var (
	errStripeSignalJob      = errors.New("stripe_signal_invalid_job")
	errStripeSignalFamily   = errors.New("stripe_signal_wrong_family")
	errStripeSignalDatabase = errors.New("stripe_signal_database")
)

type paymentSignalArgs struct {
	OperationID string `json:"operation_id"`
	SignalID    string `json:"signal_id"`
	Version     int    `json:"version"`
}

func (paymentSignalArgs) Kind() string { return "payment_signal_v1" }

func validStripeSignalArgs(a paymentSignalArgs) bool {
	return a.Version == 1 && command.ValidID(a.OperationID) && command.ValidID(a.SignalID)
}

type SignalWorker struct {
	river.WorkerDefaults[paymentSignalArgs]
	pool    *pgxpool.Pool
	stripe  *StripeRuntime
	profile string
	options QueryWorkerOptions
	core    core.Service
	jobs    *river.Client[pgx.Tx]
}

var _ river.Worker[paymentSignalArgs] = (*SignalWorker)(nil)

func NewSignalWorker(ctx context.Context, pool *pgxpool.Pool, s *StripeRuntime,
	profile string, o QueryWorkerOptions) (*SignalWorker, error) {
	if ctx == nil || pool == nil || !validQueryWorkerProfile(profile) || !validQueryWorkerOptions(o) ||
		(s != nil && (s.pool != pool || s.profile != profile)) {
		return nil, errStripeSignalJob
	}
	if err := platform.ValidateWorkerPool(ctx, pool); err != nil {
		return nil, errStripeSignalDatabase
	}
	jobs, err := river.NewClient(riverpgxv5.New(pool), &river.Config{Schema: "river_payment"})
	if err != nil {
		return nil, errStripeSignalDatabase
	}
	return &SignalWorker{pool: pool, stripe: s, profile: profile, options: o, jobs: jobs}, nil
}

func (w *SignalWorker) Timeout(*river.Job[paymentSignalArgs]) time.Duration {
	if w == nil {
		return 0
	}
	return time.Duration(w.options.LeaseSeconds) * time.Second
}

func (w *SignalWorker) Work(ctx context.Context, job *river.Job[paymentSignalArgs]) (result error) {
	if job == nil || !validStripeSignalArgs(job.Args) {
		return river.JobCancel(errStripeSignalJob)
	}
	if ctx == nil || w == nil || w.pool == nil {
		return errStripeSignalDatabase
	}
	bounded, cancel := context.WithTimeout(ctx, w.options.DBTimeout)
	defer cancel()
	var op queryOperation
	// The operation family is checked before a disabled runtime can snooze the job.
	if err := w.pool.QueryRow(bounded, `SELECT actor_kind,provider,action,purpose,state
		FROM integration.operations WHERE id=$1::uuid`, job.Args.OperationID).
		Scan(&op.ActorKind, &op.Provider, &op.Action, &op.Purpose, &op.State); err != nil {
		return errStripeSignalDatabase
	}
	if (!validQueryOperation(op) && !validRefundOperation(op)) || op.Provider != "stripe" {
		return river.JobCancel(errStripeSignalFamily)
	}
	if w.stripe == nil {
		return river.JobSnooze(5 * time.Second)
	}
	claim, err := w.claimSignal(ctx, job.Args.OperationID)
	if err != nil {
		return errStripeSignalDatabase
	}
	// finish routes a fixed completion code to the fence of the operation kind this job claimed.
	finish := func(code string, delay time.Duration) error {
		if op.ActorKind == "PAYMENT_REFUND" {
			return w.finishRefundSignal(ctx, job.Args.OperationID, claim, code, delay)
		}
		return w.finishSignal(ctx, job.Args.OperationID, claim, code, delay)
	}
	switch claim.Disposition {
	case "busy":
		return river.JobSnooze(2 * time.Second)
	case "terminal":
		return nil
	case "blocked_binding":
		return river.JobSnooze(w.options.RetryDelay)
	case "claimed":
		if claim.Mode != "reconcile" {
			return errStripeSignalDatabase
		}
	default:
		return errStripeSignalDatabase
	}
	defer func() {
		if recover() != nil {
			if ctx.Err() != nil {
				result = river.JobSnooze(5 * time.Second)
			} else {
				result = finish("stripe_panic", 5*time.Second)
			}
		}
	}()
	callCtx, stop := context.WithTimeout(ctx, w.options.CallTimeout)
	defer stop()
	sig, err := w.loadSignal(callCtx, job.Args, job.ID, claim, nil)
	if ctx.Err() != nil {
		return river.JobSnooze(5 * time.Second)
	}
	if callCtx.Err() != nil {
		return finish("stripe_timeout", 5*time.Second)
	}
	if err != nil {
		return finish("stripe_unavailable", 5*time.Second)
	}
	if sig.ConsumedAt != nil {
		return w.consumeSignal(ctx, job.Args, job.ID, claim, sig, "", "stripe_signal_consumed")
	}
	if sig.DBNow.Sub(sig.CreatedAt) > 10*time.Minute {
		return w.consumeSignal(ctx, job.Args, job.ID, claim, sig, "STALE_DROPPED", "stripe_signal_stale")
	}
	if sig.RefundID != "" {
		// A refund wake-up (webhook or MERCHANT_REFRESH) retrieves that refund under the refund lease.
		return w.refundSignal(ctx, callCtx, job, claim, sig, finish)
	}
	snapshot, err := w.stripe.loadSession(callCtx, job.Args.OperationID, claim, w.options.DBTimeout)
	if ctx.Err() != nil {
		return river.JobSnooze(5 * time.Second)
	}
	if callCtx.Err() != nil {
		return finish("stripe_timeout", 5*time.Second)
	}
	if err != nil {
		return finish("stripe_unavailable", 5*time.Second)
	}
	if sig.ObjectType == "charge" {
		// charge.refunded: never NOOP_TERMINAL; the checkout op records a charge snapshot (Via=retrieve).
		return w.chargeSignal(ctx, callCtx, job, claim, sig, snapshot, finish)
	}
	target := snapshot.SessionID
	if sig.SessionID != "" {
		target = sig.SessionID
	}
	if (snapshot.Captured || snapshot.ClosedUnpaid) && sig.SessionID == "" {
		return w.consumeSignal(ctx, job.Args, job.ID, claim, sig, "NOOP_TERMINAL", "stripe_terminal_observed")
	}
	if target == "" {
		return finish("stripe_unavailable", 5*time.Second)
	}
	client, err := w.stripe.clientForClaim(callCtx, snapshot, claim, w.options.DBTimeout)
	if ctx.Err() != nil {
		return river.JobSnooze(5 * time.Second)
	}
	if callCtx.Err() != nil {
		return finish("stripe_timeout", 5*time.Second)
	}
	if err != nil {
		return finish("stripe_unavailable", 5*time.Second)
	}
	session, meta, err := client.RetrieveCheckoutSession(callCtx, target)
	if err != nil {
		return finish("stripe_retrieve_failed", 15*time.Second)
	}
	if !stripeSessionIdentity(session, snapshot) {
		return finish("stripe_session_mismatch", 2*time.Minute)
	}
	via, outcome := "retrieve", "OBSERVED"
	if sig.Source == "BUYER_CANCEL" && session.Status == "open" {
		if err := w.noteSignalExpire(callCtx, job.Args.OperationID, claim); err != nil {
			return finish("stripe_retrieve_failed", 15*time.Second)
		}
		_, _, _ = client.ExpireCheckoutSession(callCtx, target,
			stripe.ExpireIdempotencyKey(snapshot.AttemptID, claim.Generation))
		session, meta, err = client.RetrieveCheckoutSession(callCtx, target)
		if err != nil {
			return finish("stripe_retrieve_failed", 15*time.Second)
		}
		if !stripeSessionIdentity(session, snapshot) {
			return finish("stripe_session_mismatch", 2*time.Minute)
		}
		via, outcome = "expire", "EXPIRE_REQUESTED"
	}
	if ctx.Err() != nil {
		return river.JobSnooze(5 * time.Second)
	}
	if callCtx.Err() != nil {
		return finish("stripe_timeout", 5*time.Second)
	}
	report := session.Observation(via, meta, snapshot.AccountID, snapshot.CredentialVersion, 0)
	if err := w.recordSignal(ctx, job.Args, job.ID, claim, sig, report, session.URL(), outcome); err != nil {
		return finish("stripe_record_failed", 5*time.Second)
	}
	return nil
}

type stripeSignalSnapshot struct {
	Source, SessionID string
	CreatedAt, DBNow  time.Time
	ConsumedAt        *time.Time
	// ObjectType is the linked receipt's Stripe object ("" for buyer and merchant refresh signals);
	// RefundID is the refund a refund-op signal belongs to ("" for checkout signals).
	ObjectType, RefundID string
}

func (w *SignalWorker) claimSignal(ctx context.Context, id string) (core.ClaimResult, error) {
	bounded, cancel := context.WithTimeout(ctx, w.options.DBTimeout)
	defer cancel()
	tx, err := w.pool.BeginTx(bounded, pgx.TxOptions{})
	if err != nil {
		return core.ClaimResult{}, err
	}
	defer w.rollbackSignal(tx)
	claim, err := w.core.Claim(bounded, tx, id, w.options.LeaseSeconds)
	if err != nil {
		return core.ClaimResult{}, err
	}
	return claim, tx.Commit(bounded)
}

func (w *SignalWorker) loadSignal(ctx context.Context, args paymentSignalArgs, jobID int64,
	claim core.ClaimResult, tx pgx.Tx) (stripeSignalSnapshot, error) {
	bounded, cancel := context.WithTimeout(ctx, w.options.DBTimeout)
	defer cancel()
	var out stripeSignalSnapshot
	query := `SELECT source,session_id,created_at,consumed_at,db_now,object_type,refund_id::text
		FROM integration.load_stripe_signal($1::uuid,$2::bigint,$3::bytea,$4::text,$5::bigint,$6::uuid)`
	var sessionID, objectType, refundID *string
	var err error
	if tx == nil {
		err = w.pool.QueryRow(bounded, query, args.OperationID, claim.Generation, claim.LeaseToken,
			w.profile, jobID, args.SignalID).Scan(&out.Source, &sessionID, &out.CreatedAt, &out.ConsumedAt, &out.DBNow, &objectType, &refundID)
	} else {
		err = tx.QueryRow(bounded, query, args.OperationID, claim.Generation, claim.LeaseToken,
			w.profile, jobID, args.SignalID).Scan(&out.Source, &sessionID, &out.CreatedAt, &out.ConsumedAt, &out.DBNow, &objectType, &refundID)
	}
	if sessionID != nil {
		out.SessionID = *sessionID
	}
	if objectType != nil {
		out.ObjectType = *objectType
	}
	if refundID != nil {
		out.RefundID = *refundID
	}
	return out, err
}

func (w *SignalWorker) consumeSignal(ctx context.Context, args paymentSignalArgs, jobID int64,
	claim core.ClaimResult, initial stripeSignalSnapshot, outcome, code string) error {
	bounded, cancel := context.WithTimeout(ctx, w.options.DBTimeout)
	defer cancel()
	tx, err := w.pool.BeginTx(bounded, pgx.TxOptions{})
	if err != nil {
		return errStripeSignalDatabase
	}
	defer w.rollbackSignal(tx)
	current, err := w.loadSignal(bounded, args, jobID, claim, tx)
	if err != nil || (current.ConsumedAt == nil) != (initial.ConsumedAt == nil) ||
		current.Source != initial.Source || current.SessionID != initial.SessionID ||
		current.ObjectType != initial.ObjectType || current.RefundID != initial.RefundID {
		return errStripeSignalDatabase
	}
	if outcome != "" {
		if current.ConsumedAt != nil {
			return errStripeSignalDatabase
		}
		if _, err = tx.Exec(bounded, `SELECT integration.consume_stripe_signal($1::uuid,$2::uuid,$3::bigint,$4::bytea,$5::text,$6::text)`,
			args.SignalID, args.OperationID, claim.Generation, claim.LeaseToken, w.profile, outcome); err != nil {
			return errStripeSignalDatabase
		}
	}
	var providerReference string
	if err = tx.QueryRow(bounded, `SELECT provider_reference FROM integration.operations WHERE id=$1::uuid`,
		args.OperationID).Scan(&providerReference); err != nil {
		return errStripeSignalDatabase
	}
	if err = w.core.Complete(bounded, tx, args.OperationID, claim.Generation, claim.LeaseToken,
		core.Outcome{State: "UNKNOWN", Code: code, ProviderReference: providerReference}); err != nil {
		return errStripeSignalDatabase
	}
	if err = tx.Commit(bounded); err != nil {
		return errStripeSignalDatabase
	}
	return nil
}

func (w *SignalWorker) recordSignal(ctx context.Context, args paymentSignalArgs, jobID int64,
	claim core.ClaimResult, initial stripeSignalSnapshot, report stripe.Observation, url, outcome string) error {
	encoded, err := json.Marshal(report)
	if err != nil {
		return errStripeSignalDatabase
	}
	bounded, cancel := context.WithTimeout(ctx, w.options.DBTimeout)
	defer cancel()
	tx, err := w.pool.BeginTx(bounded, pgx.TxOptions{})
	if err != nil {
		return errStripeSignalDatabase
	}
	defer w.rollbackSignal(tx)
	current, err := w.loadSignal(bounded, args, jobID, claim, tx)
	if err != nil || current.ConsumedAt != nil || current.Source != initial.Source || current.SessionID != initial.SessionID ||
		current.ObjectType != initial.ObjectType || current.RefundID != initial.RefundID {
		return errStripeSignalDatabase
	}
	var hash string
	if err = tx.QueryRow(bounded, `SELECT encode(sha256(convert_to($1::jsonb::text,'UTF8')),'hex')`, encoded).Scan(&hash); err != nil {
		return errStripeSignalDatabase
	}
	job, err := w.jobs.InsertTx(bounded, tx, paymentReconcileArgs{OperationID: args.OperationID,
		ReportHash: hash, Version: 1}, &river.InsertOpts{Queue: jobqueue.ForProfile(w.profile)})
	if err != nil {
		return errStripeSignalDatabase
	}
	if _, err = tx.Exec(bounded, `SELECT integration.consume_stripe_signal($1::uuid,$2::uuid,$3::bigint,$4::bytea,$5::text,$6::text)`,
		args.SignalID, args.OperationID, claim.Generation, claim.LeaseToken, w.profile, outcome); err != nil {
		return errStripeSignalDatabase
	}
	var sessionURL any
	if url != "" {
		sessionURL = url
	}
	if _, err = tx.Exec(bounded, `SELECT integration.record_stripe_observation($1::uuid,$2::bigint,$3::bytea,$4::text,$5::jsonb,$6::bigint,$7::text)`,
		args.OperationID, claim.Generation, claim.LeaseToken, w.profile, encoded, job.Job.ID, sessionURL); err != nil {
		return errStripeSignalDatabase
	}
	if err = tx.Commit(bounded); err != nil {
		return errStripeSignalDatabase
	}
	return nil
}

func (w *SignalWorker) noteSignalExpire(ctx context.Context, id string, claim core.ClaimResult) error {
	bounded, cancel := context.WithTimeout(ctx, w.options.DBTimeout)
	defer cancel()
	_, err := w.pool.Exec(bounded, `SELECT integration.note_stripe_expire($1::uuid,$2::bigint,$3::bytea,$4::text)`,
		id, claim.Generation, claim.LeaseToken, w.profile)
	return err
}

func (w *SignalWorker) finishSignal(ctx context.Context, id string, claim core.ClaimResult,
	code string, delay time.Duration) error {
	if ctx.Err() != nil {
		return river.JobSnooze(5 * time.Second)
	}
	bounded, cancel := context.WithTimeout(ctx, w.options.DBTimeout)
	defer cancel()
	_, err := w.pool.Exec(bounded, `SELECT integration.finish_stripe_query($1::uuid,$2::bigint,$3::bytea,$4::text,$5::text)`,
		id, claim.Generation, claim.LeaseToken, w.profile, code)
	if err != nil {
		return errStripeSignalDatabase
	}
	if delay > 0 {
		return river.JobSnooze(delay)
	}
	return nil
}

func (w *SignalWorker) rollbackSignal(tx pgx.Tx) {
	cleanup, cancel := context.WithTimeout(context.Background(), w.options.DBTimeout)
	defer cancel()
	_ = tx.Rollback(cleanup)
}

// refundSignal retrieves the refund a wake-up names and records it (Via=retrieve) under the refund lease.
// The target is the pinned Stripe refund id; before the pin, the id carried by the webhook receipt (its
// identity is proved in SQL by our own metadata). A terminal refund needs no retrieve.
func (w *SignalWorker) refundSignal(ctx, callCtx context.Context, job *river.Job[paymentSignalArgs],
	claim core.ClaimResult, sig stripeSignalSnapshot, finish func(string, time.Duration) error) error {
	id := job.Args.OperationID
	snap, err := w.stripe.loadRefund(callCtx, id, claim, w.options.DBTimeout)
	if ctx.Err() != nil {
		return river.JobSnooze(5 * time.Second)
	}
	if callCtx.Err() != nil {
		return finish("stripe_timeout", 5*time.Second)
	}
	if err != nil {
		return finish("stripe_unavailable", 5*time.Second)
	}
	if snap.HasTerminal || snap.HasRejected {
		return w.consumeSignal(ctx, job.Args, job.ID, claim, sig, "NOOP_TERMINAL", "stripe_refund_terminal")
	}
	target := snap.StripeRefundID
	if target == "" {
		target = sig.SessionID
	}
	if target == "" {
		// Nothing to retrieve before the send worker pins the refund; that worker owns the send.
		return w.consumeSignal(ctx, job.Args, job.ID, claim, sig, "NOOP_TERMINAL", "stripe_refund_no_target")
	}
	client, err := w.stripe.clientForRefund(callCtx, snap)
	if ctx.Err() != nil {
		return river.JobSnooze(5 * time.Second)
	}
	if callCtx.Err() != nil {
		return finish("stripe_timeout", 5*time.Second)
	}
	if err != nil {
		return finish("stripe_unavailable", 5*time.Second)
	}
	refund, meta, err := client.RetrieveRefund(callCtx, target)
	if err != nil {
		if errors.Is(err, stripe.ErrRateLimited) {
			return finish("stripe_rate_limited", stripeRateDelay(claim.Generation))
		}
		return finish("stripe_retrieve_failed", 15*time.Second)
	}
	if ctx.Err() != nil {
		return river.JobSnooze(5 * time.Second)
	}
	if callCtx.Err() != nil {
		return finish("stripe_timeout", 5*time.Second)
	}
	report := refund.Observation("retrieve", meta, snap.AccountID, snap.CredentialVersion, 0, id, snap.AttemptID)
	if err := w.recordSignalObservation(ctx, job.Args, job.ID, claim, sig, report, snap.AttemptID,
		`SELECT integration.record_stripe_refund_observation($1::uuid,$2::bigint,$3::bytea,$4::text,$5::jsonb,$6::bigint)`); err != nil {
		return finish("stripe_record_failed", 5*time.Second)
	}
	return nil
}

// chargeSignal records a charge snapshot (Via=retrieve) under the checkout operation lease with the
// attempt's frozen key version (D7). It only runs after a CAPTURED fact; SQL only opens review evidence.
func (w *SignalWorker) chargeSignal(ctx, callCtx context.Context, job *river.Job[paymentSignalArgs],
	claim core.ClaimResult, sig stripeSignalSnapshot, snapshot stripeSessionSnapshot,
	finish func(string, time.Duration) error) error {
	if !snapshot.Captured || snapshot.PaymentIntentID == "" {
		return w.consumeSignal(ctx, job.Args, job.ID, claim, sig, "STALE_DROPPED", "stripe_charge_not_captured")
	}
	client, err := w.stripe.clientForClaim(callCtx, snapshot, claim, w.options.DBTimeout)
	if ctx.Err() != nil {
		return river.JobSnooze(5 * time.Second)
	}
	if callCtx.Err() != nil {
		return finish("stripe_timeout", 5*time.Second)
	}
	if err != nil {
		return finish("stripe_unavailable", 5*time.Second)
	}
	charge, meta, err := client.RetrievePaymentCharge(callCtx, snapshot.PaymentIntentID)
	if err != nil {
		if errors.Is(err, stripe.ErrRateLimited) {
			return finish("stripe_rate_limited", stripeRateDelay(claim.Generation))
		}
		return finish("stripe_retrieve_failed", 15*time.Second)
	}
	if ctx.Err() != nil {
		return river.JobSnooze(5 * time.Second)
	}
	if callCtx.Err() != nil {
		return finish("stripe_timeout", 5*time.Second)
	}
	report := charge.Observation("retrieve", meta, snapshot.AccountID, snapshot.CredentialVersion, "")
	if err := w.recordSignalObservation(ctx, job.Args, job.ID, claim, sig, report, snapshot.AttemptID,
		`SELECT integration.record_stripe_charge_observation($1::uuid,$2::bigint,$3::bytea,$4::text,$5::jsonb,$6::bigint)`); err != nil {
		return finish("stripe_record_failed", 5*time.Second)
	}
	return nil
}

// recordSignalObservation is recordSignal for the refund and charge report shapes: in one transaction it
// re-reads the exact signal, inserts the same-tx reconcile job (args.operation_id = the attempt),
// consumes the signal OBSERVED and calls the record definer, which completes the operation UNKNOWN.
func (w *SignalWorker) recordSignalObservation(ctx context.Context, args paymentSignalArgs, jobID int64,
	claim core.ClaimResult, initial stripeSignalSnapshot, report any, attemptID, recordSQL string) error {
	encoded, err := json.Marshal(report)
	if err != nil {
		return errStripeSignalDatabase
	}
	bounded, cancel := context.WithTimeout(ctx, w.options.DBTimeout)
	defer cancel()
	tx, err := w.pool.BeginTx(bounded, pgx.TxOptions{})
	if err != nil {
		return errStripeSignalDatabase
	}
	defer w.rollbackSignal(tx)
	current, err := w.loadSignal(bounded, args, jobID, claim, tx)
	if err != nil || current.ConsumedAt != nil || current.Source != initial.Source || current.SessionID != initial.SessionID ||
		current.ObjectType != initial.ObjectType || current.RefundID != initial.RefundID {
		return errStripeSignalDatabase
	}
	var hash string
	if err = tx.QueryRow(bounded, `SELECT encode(sha256(convert_to($1::jsonb::text,'UTF8')),'hex')`, encoded).Scan(&hash); err != nil {
		return errStripeSignalDatabase
	}
	job, err := w.jobs.InsertTx(bounded, tx, paymentReconcileArgs{OperationID: attemptID,
		ReportHash: hash, Version: 1}, &river.InsertOpts{Queue: jobqueue.ForProfile(w.profile)})
	if err != nil {
		return errStripeSignalDatabase
	}
	if _, err = tx.Exec(bounded, `SELECT integration.consume_stripe_signal($1::uuid,$2::uuid,$3::bigint,$4::bytea,$5::text,$6::text)`,
		args.SignalID, args.OperationID, claim.Generation, claim.LeaseToken, w.profile, "OBSERVED"); err != nil {
		return errStripeSignalDatabase
	}
	// The record definers return void (refund) or boolean (charge); the result is not needed here.
	if _, err = tx.Exec(bounded, recordSQL, args.OperationID, claim.Generation, claim.LeaseToken, w.profile,
		encoded, job.Job.ID); err != nil {
		return errStripeSignalDatabase
	}
	if err = tx.Commit(bounded); err != nil {
		return errStripeSignalDatabase
	}
	return nil
}

// finishRefundSignal completes the claimed REFUND operation UNKNOWN with a fixed code
// (integration.finish_stripe_refund), then snoozes.
func (w *SignalWorker) finishRefundSignal(ctx context.Context, id string, claim core.ClaimResult,
	code string, delay time.Duration) error {
	if ctx.Err() != nil {
		return river.JobSnooze(5 * time.Second)
	}
	bounded, cancel := context.WithTimeout(ctx, w.options.DBTimeout)
	defer cancel()
	if _, err := w.pool.Exec(bounded, `SELECT integration.finish_stripe_refund($1::uuid,$2::bigint,$3::bytea,$4::text,$5::text)`,
		id, claim.Generation, claim.LeaseToken, w.profile, code); err != nil {
		return errStripeSignalDatabase
	}
	if delay > 0 {
		return river.JobSnooze(delay)
	}
	return nil
}
