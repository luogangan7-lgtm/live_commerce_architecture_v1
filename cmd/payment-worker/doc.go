// Command payment-worker owns the process that runs one payment River queue, chosen by profile
// (SANDBOX or LIVE, jobqueue.ForProfile): PAYUNi query and capture reconciliation and, with
// COMMERCE_STRIPE_ENABLED=1 on SANDBOX only, the Stripe query, signal and refund workers of
// internal/payments.
//
// It never reads STRIPE_* or whsec variables (API keys come per attempt from PG, sealed under the
// accounts keyring), never accepts buyer HTTP, and never runs live Stripe without owner approval
// (stripe-psp-v1 §5.2, §16). External hosts, only through internal/integrations/psp: api.stripe.com
// and the PAYUNi query host.
package main
