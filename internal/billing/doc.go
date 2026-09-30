// Package billing owns the platform-fee subscription mirror and the derived store standing
// (contracts/customers-billing-v1.md, T17): pinning a Stripe customer to a store, starting a
// Stripe Checkout subscription (one open session, trial once), opening the Stripe customer portal,
// turning a verified Stripe webhook into a retrieved Subscription mirrored by billing.apply_subscription,
// and reading standing/usage. The SQL side (migrations/0079_platform_billing.sql) owns every table and
// the guard that refuses new claim-window opens under RESTRICTED; this package only calls its definers.
//
// It never charges buyers, touches merchant PSP accounts, blocks refunds, fulfilment or checkout,
// stores invoices, holds a DB transaction open across a Stripe call, retries a request on its own or
// logs a bearer URL (Checkout and portal URLs are bearer links, I11). It reads no environment itself:
// cmd/api hands it a Config from LoadConfig.
//
// External service: api.stripe.com, the platform's own Stripe account (never a merchant's), for
// customers, checkout sessions, billing portal sessions, prices and subscriptions. Every wire constant
// carries its docs URL and retrieval date in stripe.go. Webhook signatures are verified by the existing
// internal/integrations/psp/stripe WebhookVerifier; this package adds no second verifier.
package billing
