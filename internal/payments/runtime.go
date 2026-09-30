package payments

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"

	"livecommerce/internal/integrations/accounts"
	"livecommerce/internal/jobqueue"
	"livecommerce/internal/platform"
)

var (
	errPaymentWorkerConfig       = errors.New("payment_worker_invalid_config")
	errPaymentWorkerDatabase     = errors.New("payment_worker_database")
	errPaymentWorkerQueueUnready = errors.New("payment_worker_queue_unready")
)

type WorkerConfig struct {
	Profile     string
	Concurrency int
	Query       QueryWorkerOptions
	Keys        *accounts.Keyring
	Stripe      *StripeRuntime
}

// NewWorkerClient keeps the existing PAYUNi assembly signature.
func NewWorkerClient(ctx context.Context, pool *pgxpool.Pool, keys *accounts.Keyring,
	profile string, concurrency int, queryOptions QueryWorkerOptions) (*river.Client[pgx.Tx], error) {
	return NewPaymentWorkerClient(ctx, pool, WorkerConfig{Profile: profile, Concurrency: concurrency,
		Query: queryOptions, Keys: keys})
}

// NewPaymentWorkerClient adds Stripe dispatch to the existing profile queue.
// It registers non-claiming signal and refund workers even when Stripe is disabled.
func NewPaymentWorkerClient(ctx context.Context, pool *pgxpool.Pool,
	c WorkerConfig) (*river.Client[pgx.Tx], error) {
	queue := jobqueue.ForProfile(c.Profile)
	if ctx == nil || pool == nil || c.Keys == nil || queue == "" || c.Concurrency < 1 || c.Concurrency > 16 ||
		!validQueryWorkerOptions(c.Query) || (c.Profile == "PROVIDER_MOCK") != (c.Query.MockTransport != nil) ||
		(c.Stripe != nil && (c.Stripe.pool != pool || c.Stripe.profile != c.Profile || c.Stripe.keys == nil)) {
		return nil, errPaymentWorkerConfig
	}
	if err := platform.ValidateWorkerPool(ctx, pool); err != nil {
		return nil, errPaymentWorkerDatabase
	}
	// The privileged SQL predicate checks the installed deferred router and all
	// active payment and reserved-queue rows. Nothing is fetched on false/error.
	preflight, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	var ready bool
	if err := pool.QueryRow(preflight, `SELECT integration.payment_queue_ready()`).Scan(&ready); err != nil || !ready {
		return nil, errPaymentWorkerQueueUnready
	}
	query, err := NewQueryWorker(ctx, pool, c.Keys, c.Profile, c.Query)
	if err != nil {
		return nil, errPaymentWorkerDatabase
	}
	query.stripe = c.Stripe
	capture, err := NewCaptureWorker(ctx, pool)
	if err != nil {
		return nil, errPaymentWorkerDatabase
	}
	signal, err := NewSignalWorker(ctx, pool, c.Stripe, c.Profile, c.Query)
	if err != nil {
		return nil, errPaymentWorkerDatabase
	}
	// payment_refund_v1 (stripe-refund-v1 §6): like the signal worker it is registered even when Stripe
	// is disabled and then only snoozes, never claiming an operation.
	refund, err := newRefundWorker(ctx, pool, c.Stripe, c.Profile, c.Query)
	if err != nil {
		return nil, errPaymentWorkerDatabase
	}
	workers := river.NewWorkers()
	river.AddWorker(workers, query)
	river.AddWorker(workers, capture)
	river.AddWorker(workers, signal)
	river.AddWorker(workers, refund)
	// River can include job errors in logs; keep the separate process silent
	// until diagnostics have an explicit redaction contract. The payment schema
	// confines leader maintenance; the profile queue still limits fetch.
	client, err := river.NewClient(riverpgxv5.New(pool), &river.Config{
		Schema: "river_payment", Workers: workers,
		Queues: map[string]river.QueueConfig{queue: {MaxWorkers: c.Concurrency}},
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		return nil, errPaymentWorkerDatabase
	}
	return client, nil
}
