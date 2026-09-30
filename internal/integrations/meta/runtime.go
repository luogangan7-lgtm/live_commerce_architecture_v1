package meta

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"

	"livecommerce/internal/platform"
)

var ErrRuntimeDatabase = errors.New("meta: runtime database unavailable")

// NewWebhookRouter admits only literal, configured callback paths. It borrows
// the ingress pool, starts no River worker and never trusts a forwarded host.
func NewWebhookRouter(ctx context.Context, pool *pgxpool.Pool, keys *PayloadKeyring,
	endpoints []WebhookEndpoint) (http.Handler, error) {
	if ctx == nil || pool == nil || keys == nil || len(endpoints) < 1 || len(endpoints) > 16 {
		return nil, ErrRuntimeConfig
	}
	routes := make(map[string]http.Handler, len(endpoints))
	for _, endpoint := range endpoints {
		_, duplicate := routes[endpoint.Path]
		if endpoint.Verifier == nil || !endpoint.Verifier.valid() ||
			endpoint.Path != "/v1/meta/webhooks/"+endpoint.Verifier.appID+"/"+endpoint.Verifier.object ||
			duplicate {
			return nil, ErrRuntimeConfig
		}
		routes[endpoint.Path] = nil
	}
	preflight, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	inbox, err := NewInbox(preflight, pool, keys)
	if err != nil {
		return nil, ErrRuntimeDatabase
	}
	var ready bool
	if err := pool.QueryRow(preflight, `SELECT meta_inbox.runtime_ready()`).Scan(&ready); err != nil || !ready {
		return nil, ErrRuntimeDatabase
	}
	for _, endpoint := range endpoints {
		h, err := NewInboxHandler(endpoint.Verifier, inbox)
		if err != nil {
			return nil, ErrRuntimeConfig
		}
		routes[endpoint.Path] = h
	}
	return webhookDispatch(routes), nil
}

func webhookDispatch(routes map[string]http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// EscapedPath differs for any percent encoding, including encoded slashes
		// and dot segments. Never let ServeMux clean a callback first.
		if r.URL.RawPath != "" || r.URL.EscapedPath() != r.URL.Path {
			http.NotFound(w, r)
			return
		}
		h := routes[r.URL.Path]
		if h == nil {
			http.NotFound(w, r)
			return
		}
		h.ServeHTTP(w, r)
	})
}

// NewConsumerClient creates only the fixed Meta queue and worker. Its separate
// River schema isolates schema-wide maintenance; the lifecycle pool and the
// consumer's SQL authority remain separate.
func NewConsumerClient(ctx context.Context, workerPool, consumerPool *pgxpool.Pool,
	keys *PayloadKeyring, concurrency int) (*river.Client[pgx.Tx], error) {
	return NewConsumerClientWithClaims(ctx, workerPool, consumerPool, keys, ClaimsActorKey{}, concurrency)
}

// NewConsumerClientWithClaims is NewConsumerClient with claim staging (meta-claims-intake-v1
// §5.1) when actor holds a key; the zero key is exactly NewConsumerClient. It adds no pool, no
// River job kind and no privilege: the consumer's only new authority is EXECUTE on
// meta_inbox.stage_claim_intake.
func NewConsumerClientWithClaims(ctx context.Context, workerPool, consumerPool *pgxpool.Pool,
	keys *PayloadKeyring, actor ClaimsActorKey, concurrency int) (*river.Client[pgx.Tx], error) {
	if ctx == nil || workerPool == nil || consumerPool == nil || keys == nil ||
		concurrency < 1 || concurrency > 16 {
		return nil, ErrRuntimeConfig
	}
	preflight, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := platform.ValidateMetaWorkerPool(preflight, workerPool); err != nil {
		return nil, ErrRuntimeDatabase
	}
	worker, err := NewConsumerWorkerWithClaims(preflight, consumerPool, keys, actor)
	if err != nil {
		return nil, ErrRuntimeDatabase
	}
	if err := platform.ValidateSameDatabase(preflight, workerPool, consumerPool); err != nil {
		return nil, ErrRuntimeDatabase
	}
	var ready bool
	if err := workerPool.QueryRow(preflight, `SELECT meta_inbox.runtime_ready()`).Scan(&ready); err != nil || !ready {
		return nil, ErrRuntimeDatabase
	}
	workers := river.NewWorkers()
	river.AddWorker(workers, worker)
	client, err := river.NewClient(riverpgxv5.New(workerPool), &river.Config{
		Schema: riverSchema, Workers: workers,
		Queues: map[string]river.QueueConfig{inboxQueue: {MaxWorkers: concurrency}},
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		return nil, ErrRuntimeDatabase
	}
	return client, nil
}
