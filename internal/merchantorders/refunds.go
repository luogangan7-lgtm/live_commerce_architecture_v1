// refunds.go is the merchant refund service (stripe-refund-v1 §7.1): request, list and refresh one
// order's Stripe refunds. It never decides capacity, review or provider state (payments.request_stripe_refund
// and identity.read_merchant_refunds do, in SQL), never calls Stripe, and never writes stock, order or
// fulfilment state (RD6). Authority is decided in the database (payments:refund on POSTs, orders:read on
// GET) and re-checked here with platform.RequirePermission after the SQL call.

package merchantorders

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"regexp"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/riverqueue/river"

	"livecommerce/internal/command"
	"livecommerce/internal/platform"
)

// RefundRequest is the exact POST body; Currency is never accepted from the client.
type RefundRequest struct {
	AmountMinor             int64  `json:"amount_minor"`
	Reason                  string `json:"reason"`
	ExpectedRefundableMinor int64  `json:"expected_refundable_minor"`
}

type RefundResult struct {
	RefundID        string `json:"refund_id"`
	State           string `json:"state"`
	AmountMinor     int64  `json:"amount_minor"`
	Currency        string `json:"currency"`
	RefundableMinor int64  `json:"refundable_minor"`
}

type RefundItem struct {
	RefundID       string  `json:"refund_id"`
	AmountMinor    int64   `json:"amount_minor"`
	Reason         string  `json:"reason"`
	State          string  `json:"state"`
	RequestedAt    string  `json:"requested_at"`
	UpdatedAt      string  `json:"updated_at"`
	FailureReason  *string `json:"failure_reason,omitempty"`
	StripeRefundID *string `json:"stripe_refund_id"`
}

type RefundList struct {
	CapturedMinor   int64        `json:"captured_minor"`
	RefundedMinor   int64        `json:"refunded_minor"`
	PendingMinor    int64        `json:"pending_minor"`
	RefundableMinor int64        `json:"refundable_minor"`
	Currency        string       `json:"currency"`
	Items           []RefundItem `json:"items"`
}

type RefundSignal struct {
	RefundID  string `json:"refund_id"`
	Scheduled bool   `json:"scheduled"`
}

// §7.1 fixed error codes. The HTTP layer maps each to one status and code; none carries a SQL message.
var (
	ErrRefundableChanged   = errors.New("refundable_changed")
	ErrExceedsRefundable   = errors.New("exceeds_refundable")
	ErrAmountStep          = errors.New("amount_step")
	ErrNotRefundable       = errors.New("not_refundable")
	ErrRefundBlockedReview = errors.New("refund_blocked_review")
	ErrRefundLimit         = errors.New("refund_limit")
)

const refundOperation = "payments.refund.request"

var (
	refundKey     = regexp.MustCompile(`^[A-Za-z0-9_.:-]{8,128}$`)
	stripeRefundP = regexp.MustCompile(`^[A-Za-z0-9_]{1,255}$`)
)

// River job args are per-package copies of the frozen post_river/0013 shapes (repo pattern).
type paymentRefundArgs struct {
	OperationID string `json:"operation_id"`
	Version     int    `json:"version"`
}

func (paymentRefundArgs) Kind() string { return "payment_refund_v1" }

type paymentSignalArgs struct {
	OperationID string `json:"operation_id"`
	SignalID    string `json:"signal_id"`
	Version     int    `json:"version"`
}

func (paymentSignalArgs) Kind() string { return "payment_signal_v1" }

func validRefundRequest(in RefundRequest) bool {
	return in.AmountMinor >= 1 && in.AmountMinor <= 999999999999 && in.ExpectedRefundableMinor >= 0 &&
		in.ExpectedRefundableMinor <= command.MaxMoney && (in.Reason == "requested_by_customer" || in.Reason == "duplicate")
}

// requireRefundEnvironment is the S5 deployment-environment guard shared by the refund POST and the refresh
// route. identity.merchant_refund_environment (commerce_auth definer, payments:refund with a fresh final fence):
// commerce_runtime cannot read checkout.payment_attempts.environment itself, and the payments definers have no
// profile input. NULL (order without an attempt) or another environment is ErrNotRefundable, the same error the
// definer gives an order without a captured attempt; a missing order is PT404 from the function. It runs before
// any River insert, so a refused call leaves no orphan job.
func requireRefundEnvironment(ctx context.Context, tx pgx.Tx, scope platform.Scope, tokenHash []byte, orderID, environment string) error {
	var attemptEnvironment *string
	if err := tx.QueryRow(ctx, `SELECT identity.merchant_refund_environment($1::bytea,$2::uuid,$3::uuid)`,
		tokenHash, scope.StoreID, orderID).Scan(&attemptEnvironment); err != nil {
		return mapRefundError(err)
	}
	if attemptEnvironment == nil || *attemptEnvironment != environment {
		return ErrNotRefundable
	}
	return nil
}

// RequestRefund is RequestRefundIn for a SANDBOX deployment (the pre-LIVE signature, unchanged).
func RequestRefund(ctx context.Context, tx pgx.Tx, jobs *river.Client[pgx.Tx], scope platform.Scope,
	token, key, orderID string, in RefundRequest) (RefundResult, error) {
	return RequestRefundIn(ctx, tx, jobs, scope, "SANDBOX", token, key, orderID, in)
}

// RequestRefundIn records one refund request and its payment_refund_v1 job in the caller's scoped
// transaction. environment is the deployment's payment environment (SANDBOX or LIVE): an order whose
// attempt was made in another environment (or has none) is ErrNotRefundable BEFORE any River insert, so a
// pre-cutover SANDBOX capture can never create a refund the LIVE worker refuses (validRefundSnapshot) and
// the LIVE monitor never sees (stripe-live-enable-v1 §5.2, ruling S5). A replay (same key and body) returns
// the stored result and inserts nothing; the same key with another body is command.ErrConflict. No provider
// I/O happens here or in the SQL definer.
func RequestRefundIn(ctx context.Context, tx pgx.Tx, jobs *river.Client[pgx.Tx], scope platform.Scope,
	environment, token, key, orderID string, in RefundRequest) (RefundResult, error) {
	if tx == nil || jobs == nil || !validAuthorityInput(scope, token) || !refundKey.MatchString(key) ||
		!command.ValidID(orderID) || !validRefundRequest(in) ||
		(environment != "SANDBOX" && environment != "LIVE") {
		return RefundResult{}, command.ErrInvalid
	}
	body, err := json.Marshal(struct {
		OrderID                 string `json:"order_id"`
		AmountMinor             int64  `json:"amount_minor"`
		Reason                  string `json:"reason"`
		ExpectedRefundableMinor int64  `json:"expected_refundable_minor"`
	}{orderID, in.AmountMinor, in.Reason, in.ExpectedRefundableMinor})
	if err != nil {
		return RefundResult{}, command.ErrInvalid
	}
	digest := sha256.Sum256(body)
	// Replay pre-check: read-only, so a replay never inserts a River job that no signal row would own.
	// Same rule as the SQL definer (post_river 0013): another principal's key is a conflict, never a replay.
	var prevHash, prevResponse []byte
	var prevPrincipal string
	err = tx.QueryRow(ctx, `SELECT request_hash,response,coalesce(principal_id::text,'') FROM ops.command_results
		WHERE tenant_id=$1 AND store_id=$2 AND operation=$3 AND idempotency_key=$4`,
		scope.TenantID, scope.StoreID, refundOperation, key).Scan(&prevHash, &prevResponse, &prevPrincipal)
	if err == nil {
		if !bytes.Equal(prevHash, digest[:]) || prevPrincipal != scope.PrincipalID {
			return RefundResult{}, command.ErrConflict
		}
		replayed, derr := decodeRefundResult(prevResponse)
		if derr != nil {
			return RefundResult{}, derr
		}
		if err = platform.RequirePermission(ctx, tx, scope, token, "payments:refund"); err != nil {
			return RefundResult{}, mapRefundError(err)
		}
		return replayed, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return RefundResult{}, mapRefundError(err)
	}
	hash := sha256.Sum256([]byte(token))
	if err = requireRefundEnvironment(ctx, tx, scope, hash[:], orderID, environment); err != nil {
		return RefundResult{}, err
	}
	var refundID string
	if err = tx.QueryRow(ctx, `SELECT gen_random_uuid()::text`).Scan(&refundID); err != nil {
		return RefundResult{}, mapRefundError(err)
	}
	// No ScheduledAt: SQL requires the job state available with attempt 0; the default queue is routed to
	// the attempt's profile queue by the deferred route_payment_queue_v1 trigger.
	job, err := jobs.InsertTx(ctx, tx, paymentRefundArgs{OperationID: refundID, Version: 1}, nil)
	if err != nil || job == nil || job.Job == nil {
		return RefundResult{}, ErrUnavailable
	}
	var raw []byte
	// payments.request_stripe_refund: auth, order lock, capacity CAS, op+refund+event+audit+receipt rows.
	if err = tx.QueryRow(ctx, `SELECT payments.request_stripe_refund($1::bytea,$2::uuid,$3::uuid,$4::text,$5::bytea,
		$6::bigint,$7::text,$8::bigint,$9::uuid,$10::bigint)`, hash[:], scope.StoreID, orderID, key, digest[:],
		in.AmountMinor, in.Reason, in.ExpectedRefundableMinor, refundID, job.Job.ID).Scan(&raw); err != nil {
		return RefundResult{}, mapRefundError(err)
	}
	out, err := decodeRefundResult(raw)
	if err != nil {
		return RefundResult{}, err
	}
	if out.RefundID != refundID || out.AmountMinor != in.AmountMinor {
		// A concurrent duplicate committed first: our job would be an orphan, so fail and roll back.
		return RefundResult{}, command.ErrConflict
	}
	if err = platform.RequirePermission(ctx, tx, scope, token, "payments:refund"); err != nil {
		return RefundResult{}, mapRefundError(err)
	}
	return out, nil
}

// ListRefunds reads the order's refunds and money totals under orders:read.
func ListRefunds(ctx context.Context, tx pgx.Tx, scope platform.Scope, token, orderID string) (RefundList, error) {
	if tx == nil || !validAuthorityInput(scope, token) || !command.ValidID(orderID) {
		return RefundList{}, command.ErrInvalid
	}
	hash := sha256.Sum256([]byte(token))
	var raw []byte
	if err := tx.QueryRow(ctx, `SELECT identity.read_merchant_refunds($1::bytea,$2::uuid,$3::uuid)`,
		hash[:], scope.StoreID, orderID).Scan(&raw); err != nil {
		return RefundList{}, mapRefundError(err)
	}
	// The SQL function has its own fresh final fence; keep the Go Scope as a second fence.
	if err := platform.RequirePermission(ctx, tx, scope, token, "orders:read"); err != nil {
		return RefundList{}, mapRefundError(err)
	}
	if len(raw) == 0 || len(raw) > 64<<10 {
		return RefundList{}, ErrUnavailable
	}
	return decodeRefundList(raw)
}

// RefreshRefund is RefreshRefundIn for a SANDBOX deployment (the pre-LIVE signature, unchanged).
func RefreshRefund(ctx context.Context, tx pgx.Tx, jobs *river.Client[pgx.Tx], scope platform.Scope,
	token, orderID, refundID string) (RefundSignal, error) {
	return RefreshRefundIn(ctx, tx, jobs, scope, "SANDBOX", token, orderID, refundID)
}

// RefreshRefundIn records one throttled MERCHANT_REFRESH wake-up. Scheduled=false means the SQL definer
// refused (throttle, terminal refund): the caller must roll back the transaction, because the River job
// inserted here would have no signal row. Like RequestRefundIn it refuses an order whose attempt is in
// another environment than the deployment's (ErrNotRefundable, before any River insert): the other
// environment's worker would refuse the work and the LIVE monitor would never see it (stripe-live-enable-v1 §5.2).
func RefreshRefundIn(ctx context.Context, tx pgx.Tx, jobs *river.Client[pgx.Tx], scope platform.Scope,
	environment, token, orderID, refundID string) (RefundSignal, error) {
	if tx == nil || jobs == nil || !validAuthorityInput(scope, token) || !command.ValidID(orderID) ||
		!command.ValidID(refundID) || (environment != "SANDBOX" && environment != "LIVE") {
		return RefundSignal{}, command.ErrInvalid
	}
	hash := sha256.Sum256([]byte(token))
	if err := requireRefundEnvironment(ctx, tx, scope, hash[:], orderID, environment); err != nil {
		return RefundSignal{}, err
	}
	var signalID string
	if err := tx.QueryRow(ctx, `SELECT gen_random_uuid()::text`).Scan(&signalID); err != nil {
		return RefundSignal{}, mapRefundError(err)
	}
	job, err := jobs.InsertTx(ctx, tx, paymentSignalArgs{OperationID: refundID, SignalID: signalID, Version: 1}, nil)
	if err != nil || job == nil || job.Job == nil {
		return RefundSignal{}, ErrUnavailable
	}
	var raw []byte
	if err = tx.QueryRow(ctx, `SELECT payments.request_stripe_refund_refresh($1::bytea,$2::uuid,$3::uuid,$4::uuid,$5::uuid,$6::bigint)`,
		hash[:], scope.StoreID, orderID, refundID, signalID, job.Job.ID).Scan(&raw); err != nil {
		return RefundSignal{}, mapRefundError(err)
	}
	if _, err = exactRefund(raw, "refund_id", "scheduled"); err != nil {
		return RefundSignal{}, err
	}
	var out RefundSignal
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err = dec.Decode(&out); err != nil || out.RefundID != refundID {
		return RefundSignal{}, ErrUnavailable
	}
	if err = platform.RequirePermission(ctx, tx, scope, token, "payments:refund"); err != nil {
		return RefundSignal{}, mapRefundError(err)
	}
	return out, nil
}

// mapRefundError turns a definer error into a fixed sentinel. Only the SQLSTATE and OUR fixed message
// are read; no driver detail, customer value or provider string ever crosses this boundary.
func mapRefundError(err error) error {
	var pg *pgconn.PgError
	if errors.As(err, &pg) {
		switch pg.Code {
		case "PT400":
			return command.ErrInvalid
		case "PT409":
			if pg.Message == "refundable_changed" {
				return ErrRefundableChanged
			}
			return command.ErrConflict
		case "PT422":
			switch pg.Message {
			case "exceeds_refundable":
				return ErrExceedsRefundable
			case "amount_step":
				return ErrAmountStep
			case "not_refundable":
				return ErrNotRefundable
			case "refund_blocked_review":
				return ErrRefundBlockedReview
			case "refund_limit":
				return ErrRefundLimit
			}
			return command.ErrInvalid
		case "23505":
			return command.ErrConflict
		}
	}
	return mapError(err)
}

func exactRefund(raw []byte, names ...string) (map[string]json.RawMessage, error) {
	var fields map[string]json.RawMessage
	if !bytes.HasPrefix(bytes.TrimSpace(raw), []byte("{")) || json.Unmarshal(raw, &fields) != nil || len(fields) != len(names) {
		return nil, ErrUnavailable
	}
	for _, name := range names {
		if _, ok := fields[name]; !ok {
			return nil, ErrUnavailable
		}
	}
	return fields, nil
}

func decodeRefundResult(raw []byte) (RefundResult, error) {
	if _, err := exactRefund(raw, "refund_id", "state", "amount_minor", "currency", "refundable_minor"); err != nil {
		return RefundResult{}, err
	}
	var out RefundResult
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if dec.Decode(&out) != nil || !command.ValidID(out.RefundID) || out.State != "REQUESTED" ||
		out.AmountMinor < 1 || !currency(out.Currency) || !money(out.RefundableMinor) {
		return RefundResult{}, ErrUnavailable
	}
	return out, nil
}

var refundStates = map[string]bool{"REQUESTED": true, "SUBMITTING": true, "PENDING": true, "SUCCEEDED": true,
	"FAILED": true, "CANCELED": true, "REJECTED": true, "UNKNOWN": true}

func decodeRefundList(raw []byte) (RefundList, error) {
	fields, err := exactRefund(raw, "captured_minor", "refunded_minor", "pending_minor", "refundable_minor", "currency", "items")
	if err != nil {
		return RefundList{}, err
	}
	var list RefundList
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if dec.Decode(&list) != nil || !currency(list.Currency) || list.Items == nil || len(list.Items) > 20 ||
		bytes.Equal(bytes.TrimSpace(fields["items"]), []byte("null")) {
		return RefundList{}, ErrUnavailable
	}
	var rawItems []json.RawMessage
	if json.Unmarshal(fields["items"], &rawItems) != nil || len(rawItems) != len(list.Items) {
		return RefundList{}, ErrUnavailable
	}
	var refunded, pending int64
	for i, item := range list.Items {
		itemFields := map[string]json.RawMessage{}
		if json.Unmarshal(rawItems[i], &itemFields) != nil {
			return RefundList{}, ErrUnavailable
		}
		names := 7
		if _, has := itemFields["failure_reason"]; has {
			names = 8
		}
		if len(itemFields) != names {
			return RefundList{}, ErrUnavailable
		}
		if !validRefundItem(item) {
			return RefundList{}, ErrUnavailable
		}
		switch item.State {
		case "SUCCEEDED":
			refunded += item.AmountMinor
		case "REQUESTED", "SUBMITTING", "PENDING", "UNKNOWN":
			pending += item.AmountMinor
		}
	}
	if !money(list.CapturedMinor) || list.RefundedMinor != refunded || list.PendingMinor != pending ||
		list.RefundableMinor != list.CapturedMinor-refunded-pending || list.RefundableMinor < 0 {
		return RefundList{}, ErrUnavailable
	}
	return list, nil
}

func validRefundItem(v RefundItem) bool {
	requested, e1 := canonicalTime(v.RequestedAt)
	updated, e2 := canonicalTime(v.UpdatedAt)
	if !command.ValidID(v.RefundID) || v.AmountMinor < 1 || !money(v.AmountMinor) ||
		(v.Reason != "requested_by_customer" && v.Reason != "duplicate") || !refundStates[v.State] ||
		e1 != nil || e2 != nil || updated.Before(requested) {
		return false
	}
	if v.StripeRefundID != nil && !stripeRefundP.MatchString(*v.StripeRefundID) {
		return false
	}
	if v.FailureReason != nil && (*v.FailureReason == "" || len(*v.FailureReason) > 64) {
		return false
	}
	pinned := v.StripeRefundID != nil
	switch v.State {
	case "REQUESTED", "SUBMITTING", "UNKNOWN", "REJECTED":
		if pinned {
			return false
		}
	default: // PENDING, SUCCEEDED, FAILED, CANCELED all follow a pin
		if !pinned {
			return false
		}
	}
	failed := v.State == "FAILED" || v.State == "CANCELED" || v.State == "REJECTED"
	return failed == (v.FailureReason != nil)
}
