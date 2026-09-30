// stripe_query.go owns the lease-fenced Stripe Checkout query lifecycle.
// It never infers unpaid from timeout, retries create with a new key, or updates stock directly.

package payments

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"math/rand"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"

	"livecommerce/internal/integrations/core"
	"livecommerce/internal/integrations/psp/stripe"
	"livecommerce/internal/jobqueue"
)

var errStripeBudget = errors.New("stripe_budget_exhausted")

type stripeStepResult struct {
	report *stripe.Observation
	url    string
	code   string
	delay  time.Duration
}

func (w *QueryWorker) stripeStep(ctx context.Context, id string, op queryOperation) (result error) {
	if op.ResultCode == "stripe_budget_exhausted" && op.LeaseUntil == nil {
		return river.JobCancel(errStripeBudget)
	}
	claim, _, err := w.claim(ctx, id)
	if err != nil {
		return w.dbError(ctx)
	}
	switch claim.Disposition {
	case "busy":
		return river.JobSnooze(2 * time.Second)
	case "terminal":
		return nil
	case "blocked_binding":
		// An identity drift needs review; a pending Stripe payment is never discarded.
		return river.JobSnooze(w.options.RetryDelay)
	case "claimed":
		if claim.Mode != "reconcile" {
			return errPaymentQueryDatabase
		}
	default:
		return errPaymentQueryDatabase
	}
	defer func() {
		if recover() != nil {
			if ctx.Err() != nil {
				result = river.JobSnooze(5 * time.Second)
			} else {
				result = w.finishStripe(ctx, id, claim, "stripe_panic", 5*time.Second)
			}
		}
	}()
	if claim.Generation > w.options.MaxGenerations {
		if err := w.finishStripe(ctx, id, claim, "stripe_budget_exhausted", 0); err != nil {
			return err
		}
		return river.JobCancel(errStripeBudget)
	}
	// One deadline spans both fenced SQL reads, GET /v1/account and every checkout call.
	callCtx, cancel := context.WithTimeout(ctx, w.options.CallTimeout)
	defer cancel()
	snapshot, err := w.stripe.loadSession(callCtx, id, claim, w.options.DBTimeout)
	if ctx.Err() != nil {
		return river.JobSnooze(5 * time.Second)
	}
	if callCtx.Err() != nil {
		return w.finishStripe(ctx, id, claim, "stripe_timeout", 5*time.Second)
	}
	if err != nil {
		return w.finishStripe(ctx, id, claim, "stripe_unavailable", 5*time.Second)
	}
	if snapshot.Captured || snapshot.ClosedUnpaid {
		return w.finishStripe(ctx, id, claim, "stripe_terminal_observed", 0)
	}
	if snapshot.DBNow.Sub(snapshot.ExpiresAt.Add(-40*time.Minute)) >= w.options.MaxAge {
		if err := w.finishStripe(ctx, id, claim, "stripe_budget_exhausted", 0); err != nil {
			return err
		}
		return river.JobCancel(errStripeBudget)
	}
	step := w.stripeExecute(callCtx, snapshot, claim)
	if ctx.Err() != nil {
		return river.JobSnooze(5 * time.Second)
	}
	if callCtx.Err() != nil {
		return w.finishStripe(ctx, id, claim, "stripe_timeout", 5*time.Second)
	}
	if step.code != "" {
		return w.finishStripe(ctx, id, claim, step.code, step.delay)
	}
	if step.report == nil {
		return w.finishStripe(ctx, id, claim, "stripe_record_failed", 5*time.Second)
	}
	if err := w.recordStripe(ctx, id, claim, *step.report, step.url); err != nil {
		if ctx.Err() != nil {
			return river.JobSnooze(5 * time.Second)
		}
		return w.finishStripe(ctx, id, claim, "stripe_record_failed", 5*time.Second)
	}
	return river.JobSnooze(step.delay)
}

func (w *QueryWorker) stripeExecute(ctx context.Context, s stripeSessionSnapshot,
	claim core.ClaimResult) stripeStepResult {
	if s.SessionID == "" && s.CreateFirstSentAt == nil &&
		(s.CancelRequestedAt != nil || !s.DBNow.Before(s.SendDeadline)) {
		// §11.5: only a never-sent create may close locally; SQL rechecks the proof.
		o := (stripe.Session{}).Observation("unsent", stripe.CallMeta{}, s.AccountID, 0, 0)
		o.LocalReason = "SEND_DEADLINE"
		return stripeStepResult{report: &o, delay: 5 * time.Second}
	}
	client, err := w.stripe.clientForClaim(ctx, s, claim, w.options.DBTimeout)
	if err != nil {
		return stripeStepResult{code: "stripe_unavailable", delay: 5 * time.Second}
	}
	if s.SessionID == "" {
		return w.stripeCreate(ctx, client, s, claim)
	}
	return w.stripeRetrieve(ctx, client, s, claim)
}

func (w *QueryWorker) stripeCreate(ctx context.Context, client *stripe.Client,
	s stripeSessionSnapshot, claim core.ClaimResult) stripeStepResult {
	params := stripe.CreateParams{Fields: s.CreateParams}
	if s.CreateFirstSentAt != nil && !s.DBNow.Before(s.ExpiresAt.Add(15*time.Minute)) {
		// A cached uncertain create is resolved by list, never a fresh create key.
		from := s.ExpiresAt.Add(-40*time.Minute - time.Minute).Unix()
		to := s.SendDeadline.Add(time.Minute).Unix()
		matches, meta, err := client.FindCheckoutSessions(ctx, s.AttemptID, from, to)
		if err != nil {
			return stripeStepResult{code: "stripe_create_uncertain", delay: 15 * time.Second}
		}
		if len(matches) > 1 {
			return stripeStepResult{code: "stripe_session_mismatch", delay: 2 * time.Minute}
		}
		var session stripe.Session
		if len(matches) == 1 {
			session = matches[0]
			if !stripeSessionIdentity(session, s) {
				return stripeStepResult{code: "stripe_session_mismatch", delay: 2 * time.Minute}
			}
		}
		o := session.Observation("list", meta, s.AccountID, s.CredentialVersion, 0)
		return stripeStepResult{report: &o, url: session.URL(), delay: 5 * time.Second}
	}
	body, err := stripe.EncodeCreateBody(params)
	if err != nil {
		return stripeStepResult{code: "stripe_unavailable", delay: 5 * time.Second}
	}
	hash := sha256.Sum256(body)
	mark, err := w.markStripeCreate(ctx, s.AttemptID, claim, hash[:])
	if err != nil || (mark != "SEND" && mark != "RESEND") {
		return stripeStepResult{code: "stripe_create_uncertain", delay: 5 * time.Second}
	}
	// CreateCheckoutSession always reuses the attempt-derived key and canonical body.
	session, meta, err := client.CreateCheckoutSession(ctx, params)
	if err != nil {
		if errors.Is(err, stripe.ErrIdempotency) {
			return stripeStepResult{code: "stripe_idempotency_alarm", delay: 2 * time.Minute}
		}
		if errors.Is(err, stripe.ErrRateLimited) {
			return stripeStepResult{code: "stripe_rate_limited", delay: stripeRateDelay(claim.Generation)}
		}
		if mark == "SEND" && (errors.Is(err, stripe.ErrRejected) || errors.Is(err, stripe.ErrAuthentication)) {
			// First-send definitive rejection proves no payable Checkout Session exists.
			o := (stripe.Session{}).Observation("create", meta, s.AccountID, s.CredentialVersion, 1)
			o.ErrorClass = "rejected"
			return stripeStepResult{report: &o, delay: 5 * time.Second}
		}
		// A rejection after any uncertain send cannot close; list after the cutoff.
		return stripeStepResult{code: "stripe_create_uncertain", delay: 15 * time.Second}
	}
	if !stripeSessionIdentity(session, s) {
		return stripeStepResult{code: "stripe_session_mismatch", delay: 2 * time.Minute}
	}
	o := session.Observation("create", meta, s.AccountID, s.CredentialVersion, s.CreateSendCount+1)
	return stripeStepResult{report: &o, url: session.URL(), delay: 5 * time.Second}
}

func (w *QueryWorker) stripeRetrieve(ctx context.Context, client *stripe.Client,
	s stripeSessionSnapshot, claim core.ClaimResult) stripeStepResult {
	session, meta, err := client.RetrieveCheckoutSession(ctx, s.SessionID)
	if err != nil {
		return stripeStepResult{code: "stripe_retrieve_failed", delay: 15 * time.Second}
	}
	if !stripeSessionIdentity(session, s) {
		return stripeStepResult{code: "stripe_session_mismatch", delay: 2 * time.Minute}
	}
	via := "retrieve"
	if session.Status == "open" && (!s.DBNow.Before(s.ExpiresAt) || s.CancelRequestedAt != nil) {
		if err := w.noteStripeExpire(ctx, s.AttemptID, claim); err != nil {
			return stripeStepResult{code: "stripe_retrieve_failed", delay: 15 * time.Second}
		}
		// Expire is an auditable, non-money-moving request. Every outcome is followed by retrieve.
		_, _, _ = client.ExpireCheckoutSession(ctx, s.SessionID,
			stripe.ExpireIdempotencyKey(s.AttemptID, claim.Generation))
		session, meta, err = client.RetrieveCheckoutSession(ctx, s.SessionID)
		if err != nil {
			return stripeStepResult{code: "stripe_retrieve_failed", delay: 15 * time.Second}
		}
		if !stripeSessionIdentity(session, s) {
			return stripeStepResult{code: "stripe_session_mismatch", delay: 2 * time.Minute}
		}
		via = "expire"
	}
	if session.Status == "open" && !s.DBNow.Before(s.ExpiresAt.Add(time.Hour)) &&
		s.LatestStatus == "open" {
		o := (stripe.Session{}).Observation("escalate", stripe.CallMeta{}, s.AccountID, 0, 0)
		o.LocalReason = "EXPIRY_UNCONFIRMED"
		return stripeStepResult{report: &o, delay: 5 * time.Minute}
	}
	if session.Status == "complete" && session.PaymentStatus == "unpaid" &&
		s.FirstCompleteUnpaidAt != nil && !s.DBNow.Before(s.FirstCompleteUnpaidAt.Add(time.Hour)) {
		o := (stripe.Session{}).Observation("escalate", stripe.CallMeta{}, s.AccountID, 0, 0)
		o.LocalReason = "ASYNC_PENDING"
		return stripeStepResult{report: &o, delay: 5 * time.Minute}
	}
	o := session.Observation(via, meta, s.AccountID, s.CredentialVersion, 0)
	return stripeStepResult{report: &o, delay: stripePollDelay(session, s.DBNow, s.ExpiresAt)}
}

func stripeSessionIdentity(session stripe.Session, s stripeSessionSnapshot) bool {
	return session.ID != "" && session.ClientReferenceID == s.AttemptID &&
		session.MetadataAttempt == s.AttemptID && session.MetadataProfile == s.Profile &&
		session.ExpiresAt != nil && *session.ExpiresAt == s.ExpiresAt.Unix() &&
		session.Mode == "payment" && session.Livemode == (s.Environment == "LIVE") && // LD1: livemode = attempt environment

		len(session.PaymentMethodTypes) == 1 && session.PaymentMethodTypes[0] == "card"
}

func stripePollDelay(session stripe.Session, now, expires time.Time) time.Duration {
	if session.Status == "complete" && session.PaymentStatus == "unpaid" {
		return 5 * time.Minute
	}
	if session.Status == "open" {
		if !now.Before(expires) {
			return 30 * time.Second
		}
		delay := expires.Sub(now) + time.Second
		if delay < time.Second {
			return time.Second
		}
		if delay < time.Minute {
			return delay
		}
		return time.Minute
	}
	return 5 * time.Second
}

func stripeRateDelay(generation int64) time.Duration {
	n := generation
	if n < 1 {
		n = 1
	}
	if n > 6 {
		n = 6
	}
	base := time.Duration(1<<uint(n)) * time.Second
	delay := base + time.Duration(rand.Int63n(int64(base/4)+1))
	if delay > time.Minute {
		return time.Minute
	}
	return delay
}

func (w *QueryWorker) markStripeCreate(ctx context.Context, id string,
	claim core.ClaimResult, hash []byte) (string, error) {
	bounded, cancel := context.WithTimeout(ctx, w.options.DBTimeout)
	defer cancel()
	var result string
	// integration.mark_stripe_create_sent commits SEND/RESEND before any POST.
	err := w.pool.QueryRow(bounded, `SELECT integration.mark_stripe_create_sent($1::uuid,$2::bigint,$3::bytea,$4::text,$5::bytea)`,
		id, claim.Generation, claim.LeaseToken, w.profile, hash).Scan(&result)
	return result, err
}

func (w *QueryWorker) noteStripeExpire(ctx context.Context, id string, claim core.ClaimResult) error {
	bounded, cancel := context.WithTimeout(ctx, w.options.DBTimeout)
	defer cancel()
	// integration.note_stripe_expire records the attempt before the provider POST.
	_, err := w.pool.Exec(bounded, `SELECT integration.note_stripe_expire($1::uuid,$2::bigint,$3::bytea,$4::text)`,
		id, claim.Generation, claim.LeaseToken, w.profile)
	return err
}

func (w *QueryWorker) recordStripe(ctx context.Context, id string, claim core.ClaimResult,
	report stripe.Observation, url string) error {
	encoded, err := json.Marshal(report)
	if err != nil {
		return errPaymentQueryDatabase
	}
	bounded, cancel := context.WithTimeout(ctx, w.options.DBTimeout)
	defer cancel()
	tx, err := w.pool.BeginTx(bounded, pgx.TxOptions{})
	if err != nil {
		return errPaymentQueryDatabase
	}
	defer w.rollback(tx)
	var hash string
	if err := tx.QueryRow(bounded, `SELECT encode(sha256(convert_to($1::jsonb::text,'UTF8')),'hex')`,
		encoded).Scan(&hash); err != nil {
		return errPaymentQueryDatabase
	}
	job, err := w.jobs.InsertTx(bounded, tx, paymentReconcileArgs{
		OperationID: id, ReportHash: hash, Version: 1}, &river.InsertOpts{Queue: jobqueue.ForProfile(w.profile)})
	if err != nil {
		return errPaymentQueryDatabase
	}
	var sessionURL any
	if url != "" {
		sessionURL = url
	}
	// integration.record_stripe_observation authenticates identity, pins once, and links the same-tx reconcile job.
	if _, err := tx.Exec(bounded, `SELECT integration.record_stripe_observation($1::uuid,$2::bigint,$3::bytea,$4::text,$5::jsonb,$6::bigint,$7::text)`,
		id, claim.Generation, claim.LeaseToken, w.profile, encoded, job.Job.ID, sessionURL); err != nil {
		return errPaymentQueryDatabase
	}
	if err := tx.Commit(bounded); err != nil {
		return errPaymentQueryDatabase
	}
	return nil
}

func (w *QueryWorker) finishStripe(ctx context.Context, id string, claim core.ClaimResult,
	code string, delay time.Duration) error {
	if ctx.Err() != nil {
		return river.JobSnooze(5 * time.Second)
	}
	bounded, cancel := context.WithTimeout(ctx, w.options.DBTimeout)
	defer cancel()
	// integration.finish_stripe_query records only UNKNOWN and fixed codes under the claim lease.
	if _, err := w.pool.Exec(bounded, `SELECT integration.finish_stripe_query($1::uuid,$2::bigint,$3::bytea,$4::text,$5::text)`,
		id, claim.Generation, claim.LeaseToken, w.profile, code); err != nil {
		return errPaymentQueryDatabase
	}
	if delay <= 0 {
		return nil
	}
	return river.JobSnooze(delay)
}
