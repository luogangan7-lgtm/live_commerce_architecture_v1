package checkout

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"

	"livecommerce/internal/buyer"
	"livecommerce/internal/command"
	"livecommerce/internal/jobqueue"
)

const paymentOperation = "checkout.payment.start"

type PaymentStarter struct {
	pool    *pgxpool.Pool
	jobs    *river.Client[pgx.Tx]
	profile string
}

type PaymentInput struct {
	OrderID       string `json:"order_id"`
	MethodCode    string `json:"method_code"`
	MethodVersion int64  `json:"method_version"`
}

type PaymentResult struct {
	OrderID         string `json:"order_id"`
	AttemptID       string `json:"attempt_id"`
	OperationID     string `json:"operation_id"`
	JobID           int64  `json:"job_id"`
	Generation      int64  `json:"generation"`
	MerchantTradeNo string `json:"merchant_trade_no"`
	Currency        string `json:"currency"`
	AmountMinor     int64  `json:"amount_minor"`
	State           string `json:"state"`
}

type paymentQueryArgs struct {
	OperationID string `json:"operation_id"`
	Version     int    `json:"version"`
}

func (paymentQueryArgs) Kind() string { return "payment_query_v1" }

func NewPaymentStarter(ctx context.Context, checkoutPool *pgxpool.Pool, jobs *river.Client[pgx.Tx], profile string) (*PaymentStarter, error) {
	if !validPaymentProfile(profile) {
		return nil, command.ErrInvalid
	}
	s, err := New(ctx, checkoutPool, jobs)
	if err != nil {
		return nil, err
	}
	return &PaymentStarter{pool: s.pool, jobs: s.jobs, profile: profile}, nil
}

func validPaymentProfile(profile string) bool {
	return profile == "PROVIDER_MOCK" || profile == "SANDBOX" || profile == "LIVE"
}

func validPaymentInput(in PaymentInput) bool {
	return command.ValidID(in.OrderID) && in.MethodCode == "payuni_credit" && in.MethodVersion > 0
}

// StartPayment creates a query-only payment intent; it never contacts a provider.
func (s *PaymentStarter) StartPayment(ctx context.Context, token, storeID, key string, in PaymentInput) (PaymentResult, error) {
	if ctx == nil || s == nil || s.pool == nil || s.jobs == nil ||
		!validPaymentProfile(s.profile) || !checkoutKey.MatchString(key) || !validPaymentInput(in) {
		return PaymentResult{}, command.ErrInvalid
	}
	request, err := json.Marshal(struct {
		Input   PaymentInput `json:"input"`
		Profile string       `json:"profile"`
	}{in, s.profile})
	if err != nil {
		return PaymentResult{}, command.ErrInvalid
	}
	digest := sha256.Sum256(request)
	tokenHash := sha256.Sum256([]byte(token))
	var out PaymentResult
	err = buyer.WithScope(ctx, s.pool, token, storeID, func(callCtx context.Context, tx pgx.Tx, scope buyer.Scope) error {
		var err error
		out, _, err = s.startPaymentTx(callCtx, tx, scope, tokenHash[:], storeID, key, in, digest)
		return err
	})
	if err != nil {
		return PaymentResult{}, safeError(ctx, err)
	}
	return out, nil
}

// startPaymentTx owns the one payment-start path for legacy and hosted callers.
// found means an authenticated durable receipt was replayed, so callers must
// not regenerate provider material or renew its deadline.
func (s *PaymentStarter) startPaymentTx(ctx context.Context, tx pgx.Tx, scope buyer.Scope, tokenHash []byte,
	storeID, key string, in PaymentInput, digest [32]byte) (PaymentResult, bool, error) {
	if err := lockPaymentKey(ctx, tx, scope, key); err != nil {
		return PaymentResult{}, false, err
	}
	saved, found, err := readPaymentReceipt(ctx, tx, scope, key, digest, in.OrderID)
	if err != nil {
		return PaymentResult{}, false, err
	}
	if found {
		if err = checkCapability(ctx, tx, tokenHash, storeID, scope); err != nil {
			return PaymentResult{}, false, err
		}
		return saved, true, nil
	}
	var attemptID string
	var now time.Time
	if err = tx.QueryRow(ctx, `SELECT gen_random_uuid()::text,clock_timestamp()`).Scan(&attemptID, &now); err != nil {
		return PaymentResult{}, false, err
	}
	job, err := s.jobs.InsertTx(ctx, tx, paymentQueryArgs{OperationID: attemptID, Version: 1},
		&river.InsertOpts{Queue: jobqueue.ForProfile(s.profile), ScheduledAt: now.Add(5 * time.Second)})
	if err != nil {
		return PaymentResult{}, false, err
	}
	var response []byte
	err = tx.QueryRow(ctx, `SELECT checkout.start_payment($1,$2::uuid,$3,$4,$5::uuid,$6,$7::bigint,$8,$9::uuid,$10::bigint)`,
		tokenHash, storeID, key, digest[:], in.OrderID, in.MethodCode, in.MethodVersion,
		s.profile, attemptID, job.Job.ID).Scan(&response)
	if err != nil {
		return PaymentResult{}, false, err
	}
	if err = checkCapability(ctx, tx, tokenHash, storeID, scope); err != nil {
		return PaymentResult{}, false, err
	}
	var out PaymentResult
	if err = json.Unmarshal(response, &out); err != nil || !validPaymentResult(out, in.OrderID) ||
		out.AttemptID != attemptID || out.JobID != job.Job.ID {
		return PaymentResult{}, false, command.ErrConflict
	}
	command.InLocalTime(&out) // JSON-built result; same location as replays
	return out, false, nil
}

func lockPaymentKey(ctx context.Context, tx pgx.Tx, scope buyer.Scope, key string) error {
	var timeout string
	if err := tx.QueryRow(ctx, `SHOW lock_timeout`).Scan(&timeout); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `SELECT set_config('lock_timeout','0',true)`); err != nil {
		return err
	}
	if err := advisory(ctx, tx, "checkout.payment.start|"+scope.TenantID+"|"+scope.StoreID+"|"+scope.OwnerID+"|"+key); err != nil {
		return err
	}
	_, err := tx.Exec(ctx, `SELECT set_config('lock_timeout',$1,true)`, timeout)
	return err
}

func readPaymentReceipt(ctx context.Context, tx pgx.Tx, scope buyer.Scope, key string, digest [32]byte, orderID string) (PaymentResult, bool, error) {
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
	if err = json.Unmarshal(response, &out); err != nil || !validPaymentResult(out, orderID) {
		return PaymentResult{}, false, command.ErrConflict
	}
	command.InLocalTime(&out) // replay equals the first result on any host TZ
	return out, true, nil
}

func validPaymentResult(out PaymentResult, orderID string) bool {
	return out.OrderID == orderID && command.ValidID(out.AttemptID) && out.OperationID == out.AttemptID &&
		out.JobID > 0 && out.Generation == 2 && out.MerchantTradeNo != "" &&
		out.Currency == "TWD" && out.AmountMinor >= 100 && out.AmountMinor <= 19999900 &&
		out.AmountMinor%100 == 0 && out.State == "PAYMENT_PENDING"
}
