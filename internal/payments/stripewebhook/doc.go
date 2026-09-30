// Package stripewebhook owns admission of signed Stripe webhook deliveries for one registered
// endpoint: it verifies the signature with that endpoint's decrypted signing secrets and then
// commits receipt, River signal job and reciprocal signal row in one PG transaction before any ACK.
// It never reads a Stripe API key, decides payment/stock state, trusts unsigned event fields for
// tenant/account selection, ACKs early, or retries a provider call (it makes none).
//
// It admits profiles PROVIDER_MOCK, SANDBOX and LIVE; whether a process may run LIVE (flag+reference pair) is
// decided by cmd/api, and an endpoint of another profile is answered with the fixed 404.
//
// Contract: contracts/stripe-psp-v1.md §0.2, §6.4; contracts/stripe-live-enable-v1.md §5.2, §5.4.
package stripewebhook
