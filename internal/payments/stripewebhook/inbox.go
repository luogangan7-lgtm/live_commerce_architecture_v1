// inbox.go: the PG side of Stripe webhook admission (contracts/stripe-psp-v1.md §0.2, §9.1).
// It never sees an unverified event: admit takes only a stripe.Event that the handler already
// authenticated with the endpoint's own signing secrets.

package stripewebhook

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"

	"livecommerce/internal/integrations/accounts"
	"livecommerce/internal/integrations/psp/stripe"
	"livecommerce/internal/jobqueue"
	"livecommerce/internal/platform"
)

// ErrConfig: constructor input is invalid. ErrDatabase: PG or River refused; nothing was ACKed.
var (
	ErrConfig   = errors.New("stripewebhook: invalid config")
	ErrDatabase = errors.New("stripewebhook: database unavailable")
)

// dbTimeout is the per-transaction budget (§9.1); the whole request has 5 s.
const dbTimeout = 2 * time.Second

// material is what payments.stripe_webhook_material returns for one enabled endpoint.
type material struct {
	TenantID, StoreID, ConnectionID, Environment, AccountID, Profile, KeyID string
	KeyVersion                                                              int64
	Nonce, Ciphertext                                                       []byte
}

// store is the PG seam. pgStore is the only production implementation; the interface exists so
// handler logic (ordering, status codes, no-ACK rules) is unit-testable without a database.
type store interface {
	material(ctx context.Context, endpointID string) (m material, found bool, err error)
	admit(ctx context.Context, endpointID string, m material, ev stripe.Event) error
}

// Inbox borrows the ingress pool; closing it stays the caller's job.
type Inbox struct {
	store   store
	keys    *accounts.Keyring // signing keyring only; never the payment API-key keyring's plaintext
	profile string
	now     func() time.Time
}

func (Inbox) String() string               { return "stripewebhook.Inbox{redacted}" }
func (i Inbox) GoString() string           { return i.String() }
func (Inbox) MarshalJSON() ([]byte, error) { return []byte(`"stripewebhook.Inbox{redacted}"`), nil }

// NewInbox validates the borrowed ingress pool (platform.ValidateStripeIngressPool: no merchant or
// worker pool may be substituted) and builds an insert-only River client on schema river_payment.
// profile is the only profile this process admits (PROVIDER_MOCK|SANDBOX|LIVE). LIVE (stripe-live-enable-v1
// §5.2) is admitted here because the deployment gate is the caller's: cmd/api builds a LIVE inbox only with the
// owner's flag+reference pair, and an endpoint of another profile is invisible (404) in the handler.
func NewInbox(ctx context.Context, ingressPool *pgxpool.Pool, signingKeys *accounts.Keyring,
	profile string) (*Inbox, error) {
	if ctx == nil || ingressPool == nil || signingKeys == nil || jobqueue.ForProfile(profile) == "" {
		return nil, ErrConfig
	}
	if err := platform.ValidateStripeIngressPool(ctx, ingressPool); err != nil {
		return nil, ErrDatabase
	}
	jobs, err := river.NewClient(riverpgxv5.New(ingressPool), &river.Config{Schema: "river_payment"})
	if err != nil {
		return nil, ErrDatabase
	}
	return &Inbox{store: &pgStore{pool: ingressPool, jobs: jobs, queue: jobqueue.ForProfile(profile)},
		keys: signingKeys, profile: profile, now: time.Now}, nil
}

type pgStore struct {
	pool  *pgxpool.Pool
	jobs  *river.Client[pgx.Tx]
	queue string
}

func (s *pgStore) material(ctx context.Context, endpointID string) (material, bool, error) {
	bounded, cancel := context.WithTimeout(ctx, dbTimeout)
	defer cancel()
	var m material
	// payments.stripe_webhook_material (integration_writer definer, EXECUTE ingress only): returns
	// one enabled endpoint's signing envelope; the caller-supplied UUID is not tenant authority.
	rows, err := s.pool.Query(bounded, `SELECT tenant_id::text,store_id::text,connection_id::text,environment,
		account_id,execution_profile,key_version,key_id,nonce,ciphertext
		FROM payments.stripe_webhook_material($1::uuid)`, endpointID)
	if err != nil {
		return m, false, ErrDatabase
	}
	defer rows.Close()
	if !rows.Next() {
		return m, false, errOrNil(rows.Err())
	}
	if err := rows.Scan(&m.TenantID, &m.StoreID, &m.ConnectionID, &m.Environment, &m.AccountID,
		&m.Profile, &m.KeyVersion, &m.KeyID, &m.Nonce, &m.Ciphertext); err != nil {
		return m, false, ErrDatabase
	}
	rows.Close()
	return m, true, errOrNil(rows.Err())
}

func errOrNil(err error) error {
	if err != nil {
		return ErrDatabase
	}
	return nil
}

// payment_signal_v1 args are frozen by post_river/0012 guard_payment_job_family: exactly
// {operation_id, signal_id, version:1} with canonical lowercase UUIDs.
type signalArgs struct {
	OperationID string `json:"operation_id"`
	SignalID    string `json:"signal_id"`
	Version     int    `json:"version"`
}

func (signalArgs) Kind() string { return "payment_signal_v1" }

func (s *pgStore) admit(ctx context.Context, endpointID string, m material, ev stripe.Event) (err error) {
	bounded, cancel := context.WithTimeout(ctx, dbTimeout)
	defer cancel()
	// ReadCommitted: prepare re-reads endpoint key_version after its FOR SHARE lock wait.
	tx, err := s.pool.BeginTx(bounded, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return ErrDatabase
	}
	done := false
	defer func() {
		if !done {
			cleanup, stop := context.WithTimeout(context.Background(), 2*time.Second)
			defer stop()
			_ = tx.Rollback(cleanup)
		}
	}()
	// §9.1: the DB transaction budget is 2 s (dbTimeout). A registrar rotation holding the endpoint
	// row lock is waited out within that budget, then 503 so Stripe retries against the new
	// key_version. No shorter lock_timeout: a sub-budget would 503 deliveries the contract admits.
	// statement_timeout is only the server-side backstop inside the same 2 s.
	if _, err = tx.Exec(bounded, `SELECT set_config('statement_timeout','1800ms',true),
		set_config('idle_in_transaction_session_timeout','3s',true)`); err != nil {
		return ErrDatabase
	}
	var disposition string
	var receipt, attempt, signal, refund *string
	var session, objectType *string
	// payments.stripe_webhook_prepare (integration_writer definer): locks the endpoint FOR SHARE,
	// rejects a stale key_version with 40001 (no receipt, no ACK), dedupes per endpoint and
	// preallocates the signal id for ACCEPT_PENDING. The last two arguments (payment_intent and
	// metadata.lc_refund) exist for refund/charge objects (stripe-refund-v1 §4.6, D2); refund_id is the
	// refund a refund object mapped to, NULL for checkout and charge receipts.
	if err = tx.QueryRow(bounded, `SELECT disposition,receipt_id::text,attempt_id::text,session_id,signal_id::text,
		refund_id::text,object_type
		FROM payments.stripe_webhook_prepare($1::uuid,$2::bigint,$3::text,$4::text,$5::bigint,$6::text,$7::text,
		$8::text,$9::text,$10::text,$11::boolean,$12::boolean,$13::boolean,$14::boolean,$15::bytea,$16::bigint,
		$17::text,$18::text)`,
		endpointID, m.KeyVersion, ev.ID, ev.Type, ev.Created, ev.APIVersion, ev.ObjectType, ev.SessionID,
		ev.ClientReferenceID, ev.MetadataAttempt, ev.AccountPresent, ev.Livemode, ev.ProbeSession,
		ev.Malformed, ev.BodySHA256[:], ev.SignedAt, ev.PaymentIntentID, ev.MetadataRefund).
		Scan(&disposition, &receipt, &attempt, &session, &signal, &refund, &objectType); err != nil {
		return ErrDatabase
	}
	switch disposition {
	case "DUPLICATE", "IGNORED", "QUARANTINED", "MALFORMED":
	case "ACCEPT_PENDING":
		if receipt == nil || attempt == nil || signal == nil {
			return ErrDatabase
		}
		// InsertTx shares the admission transaction: the job exists iff the receipt commits. A refund
		// object's signal job carries the REFUND operation id; every other receipt keeps the attempt id
		// (post_river/0013 guard_stripe_receipt_link: operation_id = coalesce(refund_id, attempt_id)).
		operation := *attempt
		if refund != nil {
			operation = *refund
		}
		job, insertErr := s.jobs.InsertTx(bounded, tx, signalArgs{OperationID: operation, SignalID: *signal, Version: 1},
			&river.InsertOpts{Queue: s.queue})
		if insertErr != nil || job == nil || job.Job == nil {
			return ErrDatabase
		}
		// payments.stripe_webhook_commit (integration_writer definer): validates the exact job
		// kind/args/queue/scope and inserts the reciprocal signal; the deferred receipt trigger
		// rejects COMMIT without it.
		if _, err = tx.Exec(bounded, `SELECT payments.stripe_webhook_commit($1::uuid,$2::uuid,$3::bigint)`,
			*receipt, *signal, job.Job.ID); err != nil {
			return ErrDatabase
		}
	default:
		return ErrDatabase
	}
	if err = tx.Commit(bounded); err != nil {
		return ErrDatabase
	}
	done = true
	return nil
}
