// merchant_refund.go builds the insert-only River client the merchant refund routes use. It never
// starts a worker, never reads a Stripe secret and never contacts a provider from the API process.

package main

import (
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"
)

var errMerchantRefundJobs = errors.New("merchant refund jobs unavailable")

// newMerchantRefundJobs returns an insert-only client on schema river_payment (no Workers, no Queues,
// never Start()ed). Jobs go to the default queue; the deferred route_payment_queue_v1 trigger moves them
// to the attempt's profile queue at COMMIT.
func newMerchantRefundJobs(pool *pgxpool.Pool) (*river.Client[pgx.Tx], error) {
	if pool == nil {
		return nil, errMerchantRefundJobs
	}
	jobs, err := river.NewClient(riverpgxv5.New(pool), &river.Config{Schema: "river_payment"})
	if err != nil {
		return nil, errMerchantRefundJobs
	}
	return jobs, nil
}
