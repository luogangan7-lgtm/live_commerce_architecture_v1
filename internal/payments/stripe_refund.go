// stripe_refund.go owns the lease-fenced Stripe refund lifecycle: payment_refund_v1 on the profile
// queue (stripe-refund-v1 §6). It never sends a second create key, never releases capacity from a
// timeout, 404 or 5xx, and never writes stock, order or fulfilment state (RD6): it only records
// authenticated observations, and payments.apply_stripe_refund decides facts in SQL.
//
// External: api.stripe.com POST/GET /v1/refunds and GET /v1/payment_intents, only through
// internal/integrations/psp/stripe (docs URLs in that package's refund.go).

package payments

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"

	"livecommerce/internal/command"
	"livecommerce/internal/integrations/accounts"
	"livecommerce/internal/integrations/core"
	"livecommerce/internal/integrations/psp/stripe"
	"livecommerce/internal/jobqueue"
	"livecommerce/internal/platform"
)

var (
	errRefundJob      = errors.New("stripe_refund_invalid_job")
	errRefundFamily   = errors.New("stripe_refund_wrong_family")
	errRefundDatabase = errors.New("stripe_refund_database")
	errRefundMismatch = errors.New("stripe_refund_mismatch")
	errRefundBudget   = errors.New("stripe_refund_budget_exhausted")
	errRefundBinding  = errors.New("stripe_refund_binding_changed")
)

// payment_refund_v1 args are frozen by post_river/0013 guard_payment_job_family: exactly
// {operation_id (the refund id), version:1}. Per-package copy, like the other payment job args.
type paymentRefundArgs struct {
	OperationID string `json:"operation_id"`
	Version     int    `json:"version"`
}

func (paymentRefundArgs) Kind() string { return "payment_refund_v1" }

func validRefundArgs(a paymentRefundArgs) bool {
	return a.Version == 1 && command.ValidID(a.OperationID)
}

// validRefundOperation is the family fence of a refund operation (never a checkout or PAYUNi op).
func validRefundOperation(op queryOperation) bool {
	return op.ActorKind == "PAYMENT_REFUND" && op.Provider == "stripe" && op.Action == "stripe.refund" &&
		op.Purpose == "transactional"
}

type refundWorker struct {
	river.WorkerDefaults[paymentRefundArgs]
	pool    *pgxpool.Pool
	stripe  *StripeRuntime
	profile string
	options QueryWorkerOptions
	core    core.Service
	jobs    *river.Client[pgx.Tx]
}

var _ river.Worker[paymentRefundArgs] = (*refundWorker)(nil)

// newRefundWorker builds the payment_refund_v1 worker. A nil StripeRuntime is allowed: the worker then
// leaves the job and operation untouched (snooze) for an enabled worker, as SignalWorker does.
func newRefundWorker(ctx context.Context, pool *pgxpool.Pool, s *StripeRuntime, profile string,
	o QueryWorkerOptions) (*refundWorker, error) {
	if ctx == nil || pool == nil || !validQueryWorkerProfile(profile) || !validQueryWorkerOptions(o) ||
		(s != nil && (s.pool != pool || s.profile != profile)) {
		return nil, errRefundJob
	}
	if err := platform.ValidateWorkerPool(ctx, pool); err != nil {
		return nil, errRefundDatabase
	}
	jobs, err := river.NewClient(riverpgxv5.New(pool), &river.Config{Schema: "river_payment"})
	if err != nil {
		return nil, errRefundDatabase
	}
	return &refundWorker{pool: pool, stripe: s, profile: profile, options: o, jobs: jobs}, nil
}

func (w *refundWorker) Timeout(*river.Job[paymentRefundArgs]) time.Duration {
	if w == nil {
		return 0
	}
	return time.Duration(w.options.LeaseSeconds) * time.Second
}

// stripeRefundSnapshot is the decoded integration.load_stripe_refund row.
type stripeRefundSnapshot struct {
	TenantID, StoreID, AttemptID, OrderID, ConnectionID, Environment, AccountID string
	CredentialVersion                                                           int64
	KeyID                                                                       string
	Nonce, Ciphertext                                                           []byte
	PaymentIntentID, Currency, Reason                                           string
	AmountMinor                                                                 int64
	RequestedAt, ResendUntil, DBNow                                             time.Time
	FirstSentAt, LastSentAt, SuppressedAt, PinnedAt                             *time.Time
	SendCount                                                                   int
	BodySHA256                                                                  []byte
	StripeRefundID, LatestStatus                                                string
	HasSucceeded, HasTerminal, HasRejected                                      bool
}

func validRefundSnapshot(s stripeRefundSnapshot, id, profile string) bool {
	// LD1: the refund row's environment must be the runtime profile's environment (refund loader rule).
	env, known := ProfileEnvironment(profile)
	return known && s.Environment == env && command.ValidID(s.TenantID) && command.ValidID(s.StoreID) && command.ValidID(s.AttemptID) &&
		command.ValidID(s.OrderID) && command.ValidID(s.ConnectionID) &&
		s.AccountID != "" && s.CredentialVersion > 0 && s.KeyID != "" && len(s.Nonce) == 12 &&
		len(s.Ciphertext) >= 17 && s.PaymentIntentID != "" && s.AmountMinor > 0 &&
		stripe.RefundAmountOK(s.Currency, s.AmountMinor) && !s.RequestedAt.IsZero() && !s.ResendUntil.IsZero() &&
		s.ResendUntil.Equal(s.RequestedAt.Add(20*time.Hour)) && !s.DBNow.IsZero() && s.SendCount >= 0 &&
		(s.FirstSentAt == nil) == (s.SendCount == 0) && (s.StripeRefundID == "") == (s.PinnedAt == nil) &&
		command.ValidID(id)
}

// loadRefund reads the frozen refund and the account's current-head credential under the refund lease
// (integration.load_stripe_refund, RD13). The id is the refund operation id.
func (s *StripeRuntime) loadRefund(ctx context.Context, id string, claim core.ClaimResult,
	dbTimeout time.Duration) (stripeRefundSnapshot, error) {
	bounded, cancel := context.WithTimeout(ctx, dbTimeout)
	defer cancel()
	var out stripeRefundSnapshot
	var refundID *string
	// integration.load_stripe_refund: lease/profile fenced by require_stripe_refund; returns the account's
	// CURRENT head key material, never another account's.
	if err := s.pool.QueryRow(bounded, `SELECT tenant_id::text,store_id::text,attempt_id::text,order_id::text,
		connection_id::text,environment,account_id,credential_version,key_id,nonce,ciphertext,payment_intent_id,
		currency,amount_minor,reason,requested_at,resend_until,first_sent_at,last_sent_at,send_count,body_sha256,
		suppressed_at,stripe_refund_id,pinned_at,has_succeeded,has_terminal,has_rejected,latest_status,db_now
		FROM integration.load_stripe_refund($1::uuid,$2::bigint,$3::bytea,$4::text)`,
		id, claim.Generation, claim.LeaseToken, s.profile).
		Scan(&out.TenantID, &out.StoreID, &out.AttemptID, &out.OrderID, &out.ConnectionID, &out.Environment,
			&out.AccountID, &out.CredentialVersion, &out.KeyID, &out.Nonce, &out.Ciphertext, &out.PaymentIntentID,
			&out.Currency, &out.AmountMinor, &out.Reason, &out.RequestedAt, &out.ResendUntil, &out.FirstSentAt,
			&out.LastSentAt, &out.SendCount, &out.BodySHA256, &out.SuppressedAt, &refundID, &out.PinnedAt,
			&out.HasSucceeded, &out.HasTerminal, &out.HasRejected, &out.LatestStatus, &out.DBNow); err != nil {
		return stripeRefundSnapshot{}, errStripeMaterial
	}
	if refundID != nil {
		out.StripeRefundID = *refundID
	}
	if !validRefundSnapshot(out, id, s.profile) {
		return stripeRefundSnapshot{}, errStripeMaterial
	}
	return out, nil
}

// clientForRefund opens the head credential returned by loadRefund and proves the account with
// GET /v1/account before any refund call (RD13: VerifyAccount must pass, else no call).
func (s *StripeRuntime) clientForRefund(ctx context.Context, snap stripeRefundSnapshot) (*stripe.Client, error) {
	scope := accounts.StripeAPIScope{TenantID: snap.TenantID, StoreID: snap.StoreID, ConnectionID: snap.ConnectionID,
		Environment: snap.Environment, AccountID: snap.AccountID, CredentialVersion: snap.CredentialVersion}
	credentials, err := s.keys.OpenStripeAPI(scope, snap.KeyID, snap.Nonce, snap.Ciphertext)
	if err != nil {
		return nil, errStripeMaterial
	}
	config := s.stripeClientConfig(credentials.SecretKey, snap.AccountID, snap.Environment)
	var client *stripe.Client
	if s.transport == nil {
		client, err = stripe.New(config)
	} else {
		client, err = stripe.NewWithMockTransport(config, s.transport)
	}
	if err != nil {
		return nil, errStripeMaterial
	}
	if _, err := client.VerifyAccount(ctx); err != nil {
		return nil, errStripeMaterial
	}
	return client, nil
}

func (w *refundWorker) Work(ctx context.Context, job *river.Job[paymentRefundArgs]) (result error) {
	if job == nil || !validRefundArgs(job.Args) {
		return river.JobCancel(errRefundJob)
	}
	if ctx == nil || w == nil || w.pool == nil {
		return errRefundDatabase
	}
	id := job.Args.OperationID
	bounded, cancel := context.WithTimeout(ctx, w.options.DBTimeout)
	defer cancel()
	var op queryOperation
	// The family is checked before a disabled runtime can snooze the job.
	if err := w.pool.QueryRow(bounded, `SELECT actor_kind,provider,action,purpose,state
		FROM integration.operations WHERE id=$1::uuid`, id).
		Scan(&op.ActorKind, &op.Provider, &op.Action, &op.Purpose, &op.State); err != nil {
		return errRefundDatabase
	}
	if !validRefundOperation(op) {
		return river.JobCancel(errRefundFamily)
	}
	if w.stripe == nil {
		// Missing runtime is transient (stripe_unavailable before Claim): nothing is claimed or finished.
		return river.JobSnooze(5 * time.Second)
	}
	claim, err := w.claim(ctx, id)
	if err != nil {
		if ctx.Err() != nil {
			return river.JobSnooze(5 * time.Second)
		}
		return errRefundDatabase
	}
	switch claim.Disposition {
	case "busy":
		return river.JobSnooze(2 * time.Second)
	case "terminal":
		return nil
	case "blocked_binding":
		// The provider or asset binding changed: capacity stays held and a review is opened (LOCAL escalate).
		return w.escalateBinding(ctx, id, claim)
	case "claimed":
		if claim.Mode != "reconcile" {
			return errRefundDatabase
		}
	default:
		return errRefundDatabase
	}
	defer func() {
		if recover() != nil {
			if ctx.Err() != nil {
				result = river.JobSnooze(5 * time.Second)
			} else {
				result = w.finish(ctx, id, claim, "stripe_panic", 5*time.Second)
			}
		}
	}()
	if claim.Generation > w.options.MaxGenerations {
		if err := w.finish(ctx, id, claim, "stripe_budget_exhausted", 0); err != nil {
			return err
		}
		return river.JobCancel(errRefundBudget)
	}
	// One deadline spans the fenced load, GET /v1/account and every refund call of this claim.
	callCtx, stop := context.WithTimeout(ctx, w.options.CallTimeout)
	defer stop()
	snap, err := w.stripe.loadRefund(callCtx, id, claim, w.options.DBTimeout)
	if ctx.Err() != nil {
		return river.JobSnooze(5 * time.Second)
	}
	if callCtx.Err() != nil {
		return w.finish(ctx, id, claim, "stripe_timeout", 5*time.Second)
	}
	if err != nil {
		return w.finish(ctx, id, claim, "stripe_unavailable", 5*time.Second)
	}
	result = w.step(ctx, callCtx, id, claim, snap)
	if ctx.Err() != nil {
		return river.JobSnooze(5 * time.Second)
	}
	return result
}

func (w *refundWorker) step(ctx, callCtx context.Context, id string, claim core.ClaimResult, s stripeRefundSnapshot) error {
	if s.HasTerminal || s.HasRejected || s.HasSucceeded {
		// A terminal fact ends the poll. A SUCCEEDED refund that later fails arrives by webhook or a
		// merchant refresh signal (R-8: no 30-day poller), never by this job.
		return w.finish(ctx, id, claim, "stripe_refund_terminal", 0)
	}
	if s.DBNow.Sub(s.RequestedAt) >= w.options.MaxAge {
		// A pinned PENDING refund is kept, and an unresolved one keeps its review and held capacity: a
		// merchant refresh or webhook wakes either again with a new job.
		if err := w.finish(ctx, id, claim, "stripe_budget_exhausted", 0); err != nil {
			return err
		}
		return river.JobCancel(errRefundBudget)
	}
	if s.StripeRefundID == "" && s.FirstSentAt == nil && s.SuppressedAt != nil {
		// Suppressed and never sent (a crash after the synchronous RD9 suppression): record the LOCAL closure.
		return w.recordUnsent(ctx, id, claim, s, suppressionReason(s))
	}
	if s.StripeRefundID == "" && s.FirstSentAt == nil && !s.DBNow.Before(s.ResendUntil.Add(-time.Hour)) {
		// A first send would fall inside the last hour, which is reserved for the list closure (A1).
		return w.recordUnsent(ctx, id, claim, s, "send_window_closed")
	}
	client, err := w.stripe.clientForRefund(callCtx, s)
	if ctx.Err() != nil {
		return river.JobSnooze(5 * time.Second)
	}
	if callCtx.Err() != nil {
		return w.finish(ctx, id, claim, "stripe_timeout", 5*time.Second)
	}
	if err != nil {
		return w.finish(ctx, id, claim, "stripe_unavailable", 5*time.Second)
	}
	if s.StripeRefundID != "" {
		return w.retrieve(ctx, callCtx, client, id, claim, s)
	}
	if s.FirstSentAt == nil {
		return w.firstSend(ctx, callCtx, client, id, claim, s)
	}
	if s.DBNow.Before(s.ResendUntil) {
		return w.send(ctx, callCtx, client, id, claim, s, false)
	}
	return w.list(ctx, callCtx, client, id, claim, s)
}

// suppressionReason names the LOCAL reason for an already suppressed, never-sent refund. Only the RD9
// external-refund check (or the closed send window itself) can suppress one.
func suppressionReason(s stripeRefundSnapshot) string {
	if !s.DBNow.Before(s.ResendUntil.Add(-time.Hour)) {
		return "send_window_closed"
	}
	return "external_refund"
}

func (w *refundWorker) refundParams(s stripeRefundSnapshot, id string) stripe.RefundParams {
	return stripe.RefundParams{PaymentIntentID: s.PaymentIntentID, Currency: s.Currency, Reason: s.Reason,
		RefundRef: id, AttemptRef: s.AttemptID, AmountMinor: s.AmountMinor}
}

// firstSend performs RD9: read the PaymentIntent's charge and record it (Via=presend) in the same
// transaction that may suppress the refund, and only then let mark_sent decide SEND.
func (w *refundWorker) firstSend(ctx, callCtx context.Context, client *stripe.Client, id string,
	claim core.ClaimResult, s stripeRefundSnapshot) error {
	charge, meta, err := client.RetrievePaymentCharge(callCtx, s.PaymentIntentID)
	if ctx.Err() != nil {
		return river.JobSnooze(5 * time.Second)
	}
	if callCtx.Err() != nil {
		return w.finish(ctx, id, claim, "stripe_timeout", 5*time.Second)
	}
	if err != nil {
		// Nothing was sent: retrying the pre-send read is always safe.
		return w.finishForError(ctx, id, claim, err, "stripe_retrieve_failed", claim.Generation)
	}
	suppressed, err := w.recordCharge(ctx, id, claim, charge.Observation("presend", meta, s.AccountID, s.CredentialVersion, id), s.AttemptID)
	if err != nil {
		return w.finish(ctx, id, claim, "stripe_record_failed", 5*time.Second)
	}
	if suppressed {
		// An external (Dashboard) refund already exists: nothing is sent, capacity is released by REJECTED.
		return w.recordUnsent(ctx, id, claim, s, "external_refund")
	}
	return w.send(ctx, callCtx, client, id, claim, s, true)
}

// send commits SEND/RESEND for the exact body, then POSTs it. The key is RefundIdempotencyKey(id) on every
// send: an uncertain outcome is resolved by the same key (a replay returns the saved result) or, after the
// 20 h window, by list — never by a new key.
func (w *refundWorker) send(ctx, callCtx context.Context, client *stripe.Client, id string,
	claim core.ClaimResult, s stripeRefundSnapshot, first bool) error {
	params := w.refundParams(s, id)
	body, err := stripe.EncodeRefundBody(params)
	if err != nil {
		return w.finish(ctx, id, claim, "stripe_unavailable", 5*time.Second)
	}
	hash := sha256.Sum256(body)
	mark, err := w.markSent(ctx, id, claim, hash[:])
	if err != nil {
		return w.finish(ctx, id, claim, "stripe_refund_uncertain", 5*time.Second)
	}
	if mark == "CLOSED" {
		// A closed send window is handled by step() from the DB clock before any call. Anything else
		// (a stale presend proof, a sticky review, the resend cap) means: never send without the
		// proof and never release capacity; the job retries and the window closes at 19 h. A recorded
		// first-send rejection also lands here until its REJECTED fact is applied (no resend, ever).
		// 2 min, not 1: MaxGenerations (720) must outlast the 19 h window closure, or the job is cancelled
		// at ~12 h and a review-held refund stays REQUESTED until a merchant refresh (review P2).
		return w.finish(ctx, id, claim, "stripe_refund_uncertain", 2*time.Minute)
	}
	if mark != "SEND" && mark != "RESEND" {
		return w.finish(ctx, id, claim, "stripe_refund_uncertain", 5*time.Second)
	}
	sendCount := s.SendCount + 1
	if first {
		sendCount = 1
	}
	refund, meta, err := client.CreateRefund(callCtx, params)
	if ctx.Err() != nil {
		return river.JobSnooze(5 * time.Second)
	}
	if err != nil {
		switch {
		case errors.Is(err, stripe.ErrIdempotency):
			// Frozen params changed under one key: a bug, never a closure.
			return w.finish(ctx, id, claim, "stripe_idempotency_alarm", 2*time.Minute)
		case errors.Is(err, stripe.ErrRateLimited):
			return w.finish(ctx, id, claim, "stripe_rate_limited", stripeRateDelay(claim.Generation))
		case mark == "SEND" && (errors.Is(err, stripe.ErrRejected) || errors.Is(err, stripe.ErrAuthentication)):
			// Definitive only on the FIRST send: a 400/401/403 before any byte could have executed proves the
			// refund does not exist. Auth failures are classified before Stripe's idempotency layer (F4).
			// Once recorded the rejection is final: mark_stripe_refund_sent answers CLOSED for every later
			// claim (RD4), so a delayed payment_reconcile_v1 can never lead to a second POST of this key.
			o := (stripe.Refund{}).Observation("create", meta, s.AccountID, s.CredentialVersion, 1, id, s.AttemptID)
			o.ErrorClass = "rejected"
			return w.recordAndSnooze(ctx, id, claim, s.Currency, o, 5*time.Second)
		}
		// Uncertain, 409 or a rejection after an earlier send: the SAME key is retried (a replay returns the
		// saved result); nothing here releases capacity.
		return w.finish(ctx, id, claim, "stripe_refund_uncertain", refundBackoff(claim.Generation))
	}
	o := refund.Observation("create", meta, s.AccountID, s.CredentialVersion, sendCount, id, s.AttemptID)
	return w.recordAndSnooze(ctx, id, claim, s.Currency, o, refundPollDelay(refund.Status, s.DBNow, s.RequestedAt))
}

// list resolves a refund whose create key can no longer be replayed (>= resend_until): match
// metadata.lc_refund. Zero matches inside 15 min of the last send are recorded and re-listed (D3),
// since Stripe's list is eventually consistent; after that SQL opens REFUND_UNRESOLVED and the
// refund stays UNKNOWN with capacity held. There is no second POST, ever.
func (w *refundWorker) list(ctx, callCtx context.Context, client *stripe.Client, id string,
	claim core.ClaimResult, s stripeRefundSnapshot) error {
	refunds, meta, err := client.ListRefunds(callCtx, s.PaymentIntentID, "")
	if ctx.Err() != nil {
		return river.JobSnooze(5 * time.Second)
	}
	if callCtx.Err() != nil {
		return w.finish(ctx, id, claim, "stripe_timeout", 5*time.Second)
	}
	if err != nil {
		return w.finishForError(ctx, id, claim, err, "stripe_refund_uncertain", claim.Generation)
	}
	var match stripe.Refund
	matches := 0
	for _, r := range refunds {
		if r.MetadataRefund == id && r.MetadataAttempt == s.AttemptID && r.PaymentIntentID == s.PaymentIntentID {
			match = r
			matches++
		}
	}
	if matches > 1 {
		return w.finish(ctx, id, claim, "stripe_refund_mismatch", 2*time.Minute)
	}
	o := match.Observation("list", meta, s.AccountID, s.CredentialVersion, 0, id, s.AttemptID)
	delay := 5 * time.Second
	if matches == 0 {
		delay = 15 * time.Minute
		if s.LastSentAt != nil {
			if until := s.LastSentAt.Add(15*time.Minute + 5*time.Second).Sub(s.DBNow); until > 0 && until < delay {
				delay = until // D3: snooze to last_sent_at+15 min 5 s, then list again
			}
		}
	}
	return w.recordAndSnooze(ctx, id, claim, s.Currency, o, delay)
}

func (w *refundWorker) retrieve(ctx, callCtx context.Context, client *stripe.Client, id string,
	claim core.ClaimResult, s stripeRefundSnapshot) error {
	refund, meta, err := client.RetrieveRefund(callCtx, s.StripeRefundID)
	if ctx.Err() != nil {
		return river.JobSnooze(5 * time.Second)
	}
	if callCtx.Err() != nil {
		return w.finish(ctx, id, claim, "stripe_timeout", 5*time.Second)
	}
	if err != nil {
		return w.finishForError(ctx, id, claim, err, "stripe_retrieve_failed", claim.Generation)
	}
	o := refund.Observation("retrieve", meta, s.AccountID, s.CredentialVersion, 0, id, s.AttemptID)
	return w.recordAndSnooze(ctx, id, claim, s.Currency, o, refundPollDelay(refund.Status, s.DBNow, s.RequestedAt))
}

// refundPollDelay: pending (or requires_action) polls every 60 s for the first hour, then every 15 min
// (bounded by the op MaxAge check in step); a terminal status re-runs soon so the fact is seen and the
// job completes.
func refundPollDelay(status string, now, requested time.Time) time.Duration {
	if status == "pending" || status == "requires_action" {
		if now.Sub(requested) < time.Hour {
			return time.Minute
		}
		return 15 * time.Minute
	}
	return 5 * time.Second
}

// refundBackoff is the uncertain-send schedule 5, 15, 45, 120 s (cap 120), by claim generation.
func refundBackoff(generation int64) time.Duration {
	steps := []time.Duration{5 * time.Second, 15 * time.Second, 45 * time.Second, 120 * time.Second}
	i := int(generation) - 2
	if i < 0 {
		i = 0
	}
	if i >= len(steps) {
		i = len(steps) - 1
	}
	return steps[i]
}

func (w *refundWorker) finishForError(ctx context.Context, id string, claim core.ClaimResult, err error,
	code string, generation int64) error {
	if errors.Is(err, stripe.ErrRateLimited) {
		return w.finish(ctx, id, claim, "stripe_rate_limited", stripeRateDelay(claim.Generation))
	}
	if errors.Is(err, stripe.ErrIdempotency) {
		return w.finish(ctx, id, claim, "stripe_idempotency_alarm", 2*time.Minute)
	}
	return w.finish(ctx, id, claim, code, refundBackoff(generation))
}

func (w *refundWorker) recordUnsent(ctx context.Context, id string, claim core.ClaimResult,
	s stripeRefundSnapshot, reason string) error {
	o := (stripe.Refund{}).Observation("unsent", stripe.CallMeta{}, s.AccountID, 0, 0, id, s.AttemptID)
	o.LocalReason = reason
	return w.recordAndSnooze(ctx, id, claim, s.Currency, o, 5*time.Second)
}

// escalateBinding records the lease-less LOCAL escalate report of a changed binding. The account and
// attempt come from the operation's own frozen columns (the worker holds no lease to load anything else).
func (w *refundWorker) escalateBinding(ctx context.Context, id string, claim core.ClaimResult) error {
	bounded, cancel := context.WithTimeout(ctx, w.options.DBTimeout)
	defer cancel()
	var asset, attempt string
	if err := w.pool.QueryRow(bounded, `SELECT external_asset_id,payment_attempt_id::text
		FROM integration.operations WHERE id=$1::uuid`, id).Scan(&asset, &attempt); err != nil {
		return errRefundDatabase
	}
	account := strings.TrimPrefix(asset, "SANDBOX:")
	if !command.ValidID(attempt) || account == asset {
		return river.JobCancel(errRefundFamily)
	}
	o := (stripe.Refund{}).Observation("escalate", stripe.CallMeta{}, account, 0, 0, id, attempt)
	o.LocalReason = "binding_changed"
	noLease := core.ClaimResult{Disposition: claim.Disposition, Generation: claim.Generation, LeaseToken: make([]byte, 32)}
	if err := w.record(ctx, id, noLease, o, attempt); err != nil {
		if ctx.Err() != nil {
			return river.JobSnooze(5 * time.Second)
		}
		return errRefundDatabase
	}
	// The review is open and capacity stays held; a merchant refresh or webhook opens a new job.
	return river.JobCancel(errRefundBinding)
}

// recordAndSnooze records the report and snoozes. Ruling 23: a report whose Currency differs from the
// request (wantCurrency) is still recorded (SQL opens REFUND_AMOUNT_MISMATCH and completes the op
// stripe_refund_mismatch; capacity stays held, no fact) but the job ENDS here: no poll, no resend.
// A merchant refresh or webhook opens a new job if a human resolves the review.
func (w *refundWorker) recordAndSnooze(ctx context.Context, id string, claim core.ClaimResult, wantCurrency string,
	report stripe.RefundObservation, delay time.Duration) error {
	if err := w.record(ctx, id, claim, report, report.AttemptRef); err != nil {
		if ctx.Err() != nil {
			return river.JobSnooze(5 * time.Second)
		}
		// §4.4: an identity refusal (PT409) finishes UNKNOWN stripe_refund_mismatch. It backs off 15 min, not
		// 5 s: on the create path every retry re-POSTs the same key, and a drifted report will not heal.
		if errors.Is(err, errRefundMismatch) {
			return w.finish(ctx, id, claim, "stripe_refund_mismatch", 15*time.Minute)
		}
		// Anything else is a record failure; it also ends UNKNOWN.
		return w.finish(ctx, id, claim, "stripe_record_failed", 5*time.Second)
	}
	if report.Currency != "" && report.Currency != wantCurrency {
		return river.JobCancel(errRefundMismatch)
	}
	return river.JobSnooze(delay)
}

func (w *refundWorker) claim(ctx context.Context, id string) (core.ClaimResult, error) {
	bounded, cancel := context.WithTimeout(ctx, w.options.DBTimeout)
	defer cancel()
	tx, err := w.pool.BeginTx(bounded, pgx.TxOptions{})
	if err != nil {
		return core.ClaimResult{}, err
	}
	defer w.rollback(tx)
	claim, err := w.core.Claim(bounded, tx, id, w.options.LeaseSeconds)
	if err != nil {
		return core.ClaimResult{}, err
	}
	return claim, tx.Commit(bounded)
}

func (w *refundWorker) markSent(ctx context.Context, id string, claim core.ClaimResult, hash []byte) (string, error) {
	bounded, cancel := context.WithTimeout(ctx, w.options.DBTimeout)
	defer cancel()
	var result string
	// integration.mark_stripe_refund_sent commits SEND/RESEND for the exact body BEFORE any POST.
	err := w.pool.QueryRow(bounded, `SELECT integration.mark_stripe_refund_sent($1::uuid,$2::bigint,$3::bytea,$4::text,$5::bytea)`,
		id, claim.Generation, claim.LeaseToken, w.profile, hash).Scan(&result)
	return result, err
}

// record inserts the same-tx payment_reconcile_v1 job (args.operation_id = the ATTEMPT, as for checkout
// reports) and calls integration.record_stripe_refund_observation, which validates the exact §3.1
// projection, pins the refund set-once and completes the operation UNKNOWN under the lease.
func (w *refundWorker) record(ctx context.Context, id string, claim core.ClaimResult,
	report stripe.RefundObservation, attemptID string) error {
	return w.recordJSON(ctx, id, claim, report, attemptID,
		`SELECT integration.record_stripe_refund_observation($1::uuid,$2::bigint,$3::bytea,$4::text,$5::jsonb,$6::bigint)`, nil)
}

// recordCharge records a Via=presend charge report under the refund lease; true means the refund was
// suppressed in the same transaction (an external refund exists).
func (w *refundWorker) recordCharge(ctx context.Context, id string, claim core.ClaimResult,
	report stripe.ChargeObservation, attemptID string) (bool, error) {
	var suppressed bool
	err := w.recordJSON(ctx, id, claim, report, attemptID,
		`SELECT integration.record_stripe_charge_observation($1::uuid,$2::bigint,$3::bytea,$4::text,$5::jsonb,$6::bigint)`, &suppressed)
	return suppressed, err
}

func (w *refundWorker) recordJSON(ctx context.Context, id string, claim core.ClaimResult, report any,
	attemptID, query string, boolOut *bool) error {
	encoded, err := json.Marshal(report)
	if err != nil {
		return errRefundDatabase
	}
	bounded, cancel := context.WithTimeout(ctx, w.options.DBTimeout)
	defer cancel()
	tx, err := w.pool.BeginTx(bounded, pgx.TxOptions{})
	if err != nil {
		return errRefundDatabase
	}
	defer w.rollback(tx)
	var hash string
	if err := tx.QueryRow(bounded, `SELECT encode(sha256(convert_to($1::jsonb::text,'UTF8')),'hex')`,
		encoded).Scan(&hash); err != nil {
		return errRefundDatabase
	}
	job, err := w.jobs.InsertTx(bounded, tx, paymentReconcileArgs{OperationID: attemptID, ReportHash: hash, Version: 1},
		&river.InsertOpts{Queue: jobqueue.ForProfile(w.profile)})
	if err != nil {
		return errRefundDatabase
	}
	if boolOut != nil {
		if err := tx.QueryRow(bounded, query, id, claim.Generation, claim.LeaseToken, w.profile, encoded,
			job.Job.ID).Scan(boolOut); err != nil {
			return errRefundDatabase
		}
	} else if _, err := tx.Exec(bounded, query, id, claim.Generation, claim.LeaseToken, w.profile, encoded,
		job.Job.ID); err != nil {
		var pg *pgconn.PgError
		if errors.As(err, &pg) && pg.Code == "PT409" {
			return errRefundMismatch // §4.4: identity/pin/account refusal of the report
		}
		return errRefundDatabase
	}
	if err := tx.Commit(bounded); err != nil {
		return errRefundDatabase
	}
	return nil
}

func (w *refundWorker) finish(ctx context.Context, id string, claim core.ClaimResult, code string, delay time.Duration) error {
	if ctx.Err() != nil {
		return river.JobSnooze(5 * time.Second)
	}
	bounded, cancel := context.WithTimeout(ctx, w.options.DBTimeout)
	defer cancel()
	// integration.finish_stripe_refund records only UNKNOWN and fixed codes under the claim lease.
	if _, err := w.pool.Exec(bounded, `SELECT integration.finish_stripe_refund($1::uuid,$2::bigint,$3::bytea,$4::text,$5::text)`,
		id, claim.Generation, claim.LeaseToken, w.profile, code); err != nil {
		return errRefundDatabase
	}
	if delay <= 0 {
		return nil
	}
	return river.JobSnooze(delay)
}

func (w *refundWorker) rollback(tx pgx.Tx) {
	cleanup, cancel := context.WithTimeout(context.Background(), w.options.DBTimeout)
	defer cancel()
	_ = tx.Rollback(cleanup)
}
