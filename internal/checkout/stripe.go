// stripe.go owns the Stripe branch of the hosted buyer payment service: prepare
// (start), repeatable REDIRECT handoff, and buyer refresh/cancel signals.
// It never performs provider I/O (the API process makes zero Stripe calls, SP07), never
// reads a Stripe key or STRIPE_* variable, never releases stock, and never treats a
// redirect or handoff as proof of payment.
//
// Contract: contracts/stripe-psp-v1.md §0.2, §6.4, §9.2, §9.3, §11.

package checkout

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/url"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"

	"livecommerce/internal/buyer"
	"livecommerce/internal/command"
	"livecommerce/internal/integrations/psp/stripe"
	"livecommerce/internal/jobqueue"
)

const stripeMethodCode = "stripe_checkout"

// StripeHostedConfig is the one neutral return page used as both success_url and cancel_url.
type StripeHostedConfig struct{ ReturnURL string }

// CanonicalDigest binds a Stripe attempt to the config it was started under (§9.2).
// The wire constants are the stripe-psp-v1 §9.2 frozen values: a 40 minute session, a
// 7 minute create-send window and a 5 minute handoff margin.
func (c StripeHostedConfig) CanonicalDigest() (StripeHostedConfig, [32]byte, error) {
	if !validStripeReturnURL(c.ReturnURL) {
		return StripeHostedConfig{}, [32]byte{}, command.ErrInvalid
	}
	body, _ := json.Marshal(struct {
		Version       string `json:"version"`
		ReturnURL     string `json:"return_url"`
		APIVersion    string `json:"api_version"`
		SessionTTL    int    `json:"session_ttl_seconds"`
		SendWindow    int    `json:"send_window_seconds"`
		HandoffMargin int    `json:"handoff_margin_seconds"`
	}{"stripe-hosted-v1", c.ReturnURL, stripe.APIVersion, 2400, 420, 300})
	return c, sha256.Sum256(body), nil
}

// validStripeReturnURL mirrors the unexported psp/stripe validReturnURL (absolute https, no
// userinfo/query/fragment/braces/controls, <=2048) and the start_stripe_payment SQL check, so a
// URL admitted here can never be rejected later by the adapter or SQL.
// §9.3: success_url/cancel_url carry "no query" — not even a bare '?' (ForceQuery).
func validStripeReturnURL(raw string) bool {
	if len(raw) == 0 || len(raw) > 2048 || strings.ContainsAny(raw, "{} \t\r\n") {
		return false
	}
	for i := 0; i < len(raw); i++ {
		if raw[i] < 0x21 || raw[i] > 0x7e {
			return false
		}
	}
	u, err := url.Parse(raw)
	return err == nil && u.Scheme == "https" && u.Host != "" && u.User == nil &&
		u.RawQuery == "" && !u.ForceQuery && u.Fragment == "" && u.Opaque == ""
}

// PaymentSignal is the buyer-safe result of a refresh or cancel request.
type PaymentSignal struct {
	OrderID   string `json:"order_id"`
	Scheduled bool   `json:"scheduled"`
}

// paymentSignalArgs mirrors internal/payments.paymentSignalArgs (the consumer); the exact
// args are fenced by checkout.request_stripe_signal and integration.guard_payment_job_family.
type paymentSignalArgs struct {
	OperationID string `json:"operation_id"`
	SignalID    string `json:"signal_id"`
	Version     int    `json:"version"`
}

func (paymentSignalArgs) Kind() string { return "payment_signal_v1" }

// errNoSignal aborts the buyer transaction so a job inserted for a signal the database
// refused (throttled, terminal, already cancelled) never becomes an orphan.
var errNoSignal = errors.New("stripe signal not scheduled")

const stripeRedirectPrefix = "https://checkout.stripe.com/"

// validStripeRedirect is the §9.2 shape ^https://checkout\.stripe\.com/[!-~]{1,4000}$ in every
// profile. It is spelled out because Go's regexp caps a repeat count at 1000.
func validStripeRedirect(raw string) bool {
	rest, ok := strings.CutPrefix(raw, stripeRedirectPrefix)
	if !ok || len(rest) < 1 || len(rest) > 4000 {
		return false
	}
	for i := 0; i < len(rest); i++ {
		if rest[i] < '!' || rest[i] > '~' {
			return false
		}
	}
	return true
}

// beginStripe prepares a Stripe attempt: receipt replay or new attempt + River query job +
// checkout.start_stripe_payment, all in one buyer transaction. No provider call, no key use.
func (s *HostedPaymentStarter) beginStripe(ctx context.Context, token, storeID, key string, in HostedInput) (PaymentResult, error) {
	if s.stripe == nil {
		return PaymentResult{}, command.ErrInvalid
	}
	request, _ := json.Marshal(struct {
		Input        HostedInput `json:"input"`
		Profile      string      `json:"profile"`
		ConfigDigest string      `json:"config_digest"`
	}{in, s.starter.profile, hex.EncodeToString(s.stripeDigest[:])})
	digest := sha256.Sum256(request)
	tokenHash := sha256.Sum256([]byte(token))
	var out PaymentResult
	err := buyer.WithScope(ctx, s.starter.pool, token, storeID, func(callCtx context.Context, tx pgx.Tx, scope buyer.Scope) error {
		if err := lockPaymentKey(callCtx, tx, scope, key); err != nil {
			return err
		}
		saved, found, err := readStripeReceipt(callCtx, tx, scope, key, digest, in.OrderID)
		if err != nil {
			return err
		}
		if found {
			out = saved
			return checkCapability(callCtx, tx, tokenHash[:], storeID, scope)
		}
		var attemptID string
		if err = tx.QueryRow(callCtx, `SELECT gen_random_uuid()::text`).Scan(&attemptID); err != nil {
			return err
		}
		// §8 "ScheduledAt=now": unlike PAYUNi's 5 s delay, the create must go out inside the
		// 7 minute send window. Same payment_query_v1 kind; the worker branches on provider.
		job, err := s.starter.jobs.InsertTx(callCtx, tx, paymentQueryArgs{OperationID: attemptID, Version: 1},
			&river.InsertOpts{Queue: jobqueue.ForProfile(s.starter.profile)})
		if err != nil {
			return err
		}
		// checkout.start_stripe_payment: attempt + op + session + reservation move + receipt in one
		// SQL tx; it verifies this exact job. It is the only writer of the receipt (key shared with PAYUNi, I02).
		var response []byte
		err = tx.QueryRow(callCtx, `SELECT checkout.start_stripe_payment($1::bytea,$2::uuid,$3::text,$4::bytea,$5::uuid,$6::text,$7::bigint,$8::text,$9::uuid,$10::bigint,$11::text,$12::bytea,$13::text)`,
			tokenHash[:], storeID, key, digest[:], in.OrderID, in.MethodCode, in.MethodVersion,
			s.starter.profile, attemptID, job.Job.ID, in.Locale, s.stripeDigest[:], s.stripe.ReturnURL).Scan(&response)
		if err != nil {
			return err
		}
		if err = checkCapability(callCtx, tx, tokenHash[:], storeID, scope); err != nil {
			return err
		}
		if err = json.Unmarshal(response, &out); err != nil || !validStripePaymentResult(out, in.OrderID) ||
			out.AttemptID != attemptID || out.JobID != job.Job.ID {
			return command.ErrConflict
		}
		command.InLocalTime(&out) // JSON-built result; same location as replays
		return nil
	})
	if err != nil {
		return PaymentResult{}, safeError(ctx, err)
	}
	return out, nil
}

// readStripeReceipt is readPaymentReceipt with the Stripe result rules: the shared
// validPaymentResult is TWD-only, so it cannot validate an HKD/USD/SGD/MYR receipt.
func readStripeReceipt(ctx context.Context, tx pgx.Tx, scope buyer.Scope, key string, digest [32]byte, orderID string) (PaymentResult, bool, error) {
	var savedHash, response []byte
	err := tx.QueryRow(ctx, `SELECT request_hash,response FROM checkout.command_results
		WHERE tenant_id=$1 AND store_id=$2 AND owner_id=$3 AND operation=$4 AND idempotency_key=$5`,
		scope.TenantID, scope.StoreID, scope.OwnerID, paymentOperation, key).Scan(&savedHash, &response)
	if errors.Is(err, pgx.ErrNoRows) {
		return PaymentResult{}, false, nil
	}
	if err != nil {
		return PaymentResult{}, false, err
	}
	if len(savedHash) != len(digest) || subtle.ConstantTimeCompare(savedHash, digest[:]) != 1 {
		return PaymentResult{}, false, command.ErrConflict
	}
	var out PaymentResult
	if err = json.Unmarshal(response, &out); err != nil || !validStripePaymentResult(out, orderID) {
		return PaymentResult{}, false, command.ErrConflict
	}
	command.InLocalTime(&out)
	return out, true, nil
}

func validStripePaymentResult(out PaymentResult, orderID string) bool {
	// I05: the amount must satisfy the same closed currency table as the SQL/adapter.
	_, err := stripe.UnitAmount(out.Currency, out.AmountMinor)
	return out.OrderID == orderID && command.ValidID(out.AttemptID) && out.OperationID == out.AttemptID &&
		out.JobID > 0 && out.Generation == 2 && out.MerchantTradeNo != "" &&
		err == nil && out.State == "PAYMENT_PENDING"
}

// isStripeOrder asks the database which provider owns the order's attempt, so TakeHosted
// routes without matching an error message (rulings §2). NULL (no attempt) and PAYUNi both
// take the unchanged PAYUNi path and its existing errors.
func (s *HostedPaymentStarter) isStripeOrder(ctx context.Context, tx pgx.Tx, tokenHash []byte, storeID, orderID string) (bool, error) {
	var provider *string
	// checkout.hosted_payment_provider: read-only, scoped by the buyer capability, no lock.
	if err := tx.QueryRow(ctx, `SELECT checkout.hosted_payment_provider($1::bytea,$2::uuid,$3::uuid)`,
		tokenHash, storeID, orderID).Scan(&provider); err != nil {
		return false, err
	}
	return provider != nil && *provider == "stripe", nil
}

// takeStripeTx releases the repeatable Stripe redirect. It is repeatable until the cutoff
// (D16), so an uncertain response may be re-requested by an explicit buyer click; the URL is
// never logged, stored or put in an error.
func (s *HostedPaymentStarter) takeStripeTx(ctx context.Context, tx pgx.Tx, scope buyer.Scope, tokenHash []byte,
	storeID, orderID string, out *HostedHandoff) error {
	var body []byte
	// checkout.take_stripe_handoff: locks the order first, sets first_handed_out_at once,
	// re-checks capability/qualification/cutoff inside SQL.
	if err := tx.QueryRow(ctx, `SELECT checkout.take_stripe_handoff($1::bytea,$2::uuid,$3::uuid,$4::text,$5::bytea)`,
		tokenHash, storeID, orderID, s.starter.profile, s.stripeDigest[:]).Scan(&body); err != nil {
		return err
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	var got HostedHandoff
	if err := decoder.Decode(&got); err != nil || !validStripeHandoff(got, orderID) {
		return command.ErrConflict
	}
	if err := checkCapability(ctx, tx, tokenHash, storeID, scope); err != nil {
		return err
	}
	got.ExpiresAt = got.ExpiresAt.UTC()
	if got.Disposition == "REDIRECT" {
		// expires_at is the handoff cutoff: never return a URL at or after it.
		if err := checkHostedDeadline(ctx, tx, got.ExpiresAt); err != nil {
			return err
		}
	}
	*out = got
	return nil
}

func validStripeHandoff(out HostedHandoff, orderID string) bool {
	if out.OrderID != orderID || out.ExpiresAt.IsZero() || out.Form != nil {
		return false
	}
	switch out.Disposition {
	case "REDIRECT":
		return validStripeRedirect(out.RedirectURL)
	case "CREATING", "CLOSED", "UNAVAILABLE":
		return out.RedirectURL == ""
	default:
		return false
	}
}

// RefreshPayment asks the worker to re-observe the Stripe session. The database throttles it
// (10 s, 30 total); it never triggers provider I/O inline.
func (s *HostedPaymentStarter) RefreshPayment(ctx context.Context, token, storeID, orderID string) (PaymentSignal, error) {
	return s.requestSignal(ctx, token, storeID, orderID, "REFRESH")
}

// CancelPayment records a set-once buyer cancel. The effect is an expire and then closure
// after Stripe confirms; stock is not released here.
func (s *HostedPaymentStarter) CancelPayment(ctx context.Context, token, storeID, orderID string) (PaymentSignal, error) {
	return s.requestSignal(ctx, token, storeID, orderID, "CANCEL")
}

func (s *HostedPaymentStarter) requestSignal(ctx context.Context, token, storeID, orderID, kind string) (PaymentSignal, error) {
	if ctx == nil || s == nil {
		return PaymentSignal{}, command.ErrInvalid
	}
	if s.stripe == nil {
		return PaymentSignal{}, command.ErrNotFound
	}
	if s.starter.pool == nil || s.starter.jobs == nil || !validPaymentProfile(s.starter.profile) || !command.ValidID(orderID) {
		return PaymentSignal{}, command.ErrInvalid
	}
	tokenHash := sha256.Sum256([]byte(token))
	out := PaymentSignal{OrderID: orderID}
	err := buyer.WithScope(ctx, s.starter.pool, token, storeID, func(callCtx context.Context, tx pgx.Tx, scope buyer.Scope) error {
		// The attempt id is only the River job's operation_id. It comes from the buyer's own
		// start receipt; checkout.request_stripe_signal re-derives the attempt from the scoped
		// order and requires the job args to name exactly that attempt, so a wrong id fails
		// closed (PT409) and can never signal another buyer's attempt.
		var attemptID string
		err := tx.QueryRow(callCtx, `SELECT response->>'attempt_id' FROM checkout.command_results
			WHERE tenant_id=$1 AND store_id=$2 AND owner_id=$3 AND operation=$4 AND order_id=$5
			ORDER BY created_at LIMIT 1`,
			scope.TenantID, scope.StoreID, scope.OwnerID, paymentOperation, orderID).Scan(&attemptID)
		if errors.Is(err, pgx.ErrNoRows) {
			var exists bool
			if err = tx.QueryRow(callCtx, `SELECT EXISTS(SELECT 1 FROM checkout.orders WHERE tenant_id=$1
				AND store_id=$2 AND owner_id=$3 AND id=$4::uuid)`,
				scope.TenantID, scope.StoreID, scope.OwnerID, orderID).Scan(&exists); err != nil {
				return err
			}
			if !exists {
				return command.ErrNotFound
			}
			return command.ErrConflict // order exists but no payment was ever started
		}
		if err != nil {
			return err
		}
		if !command.ValidID(attemptID) {
			return command.ErrConflict
		}
		var signalID string
		if err = tx.QueryRow(callCtx, `SELECT gen_random_uuid()::text`).Scan(&signalID); err != nil {
			return err
		}
		// No ScheduledAt: SQL requires the job to be state available with attempt 0. A retry of
		// this request creates a new signal id, so nothing here needs an idempotency key.
		job, err := s.starter.jobs.InsertTx(callCtx, tx, paymentSignalArgs{OperationID: attemptID, SignalID: signalID, Version: 1},
			&river.InsertOpts{Queue: jobqueue.ForProfile(s.starter.profile)})
		if err != nil {
			return err
		}
		var response []byte
		// checkout.request_stripe_signal: DB throttle + set-once cancel; never provider I/O inline.
		if err = tx.QueryRow(callCtx, `SELECT checkout.request_stripe_signal($1::bytea,$2::uuid,$3::uuid,$4::text,$5::bytea,$6::text,$7::uuid,$8::bigint)`,
			tokenHash[:], storeID, orderID, s.starter.profile, s.stripeDigest[:], kind, signalID, job.Job.ID).Scan(&response); err != nil {
			return err
		}
		decoder := json.NewDecoder(bytes.NewReader(response))
		decoder.DisallowUnknownFields()
		var got PaymentSignal
		if err = decoder.Decode(&got); err != nil || got.OrderID != orderID {
			return command.ErrConflict
		}
		if err = checkCapability(callCtx, tx, tokenHash[:], storeID, scope); err != nil {
			return err
		}
		if !got.Scheduled {
			return errNoSignal // roll back: the inserted job has no signal row
		}
		out = got
		return nil
	})
	if errors.Is(err, errNoSignal) {
		return out, nil // {order_id,false}
	}
	if err != nil {
		return PaymentSignal{}, safeError(ctx, err)
	}
	return out, nil
}
