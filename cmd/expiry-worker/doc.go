// Command expiry-worker owns the process that runs the checkout-expiry River queue
// (jobqueue.CheckoutExpiry): it turns due unpaid checkout holds into released stock through
// internal/checkout.ExpiryWorker, using its own worker DB login.
//
// It never accepts or reconciles payment, never calls a provider, never serves HTTP, and never
// starts unless COMMERCE_EXPIRY_WORKER_ENABLED=1 (COMMERCE_EXPIRY_WORKER_DATABASE_URL required,
// COMMERCE_EXPIRY_WORKER_CONCURRENCY 1..16). It calls no external host.
package main
