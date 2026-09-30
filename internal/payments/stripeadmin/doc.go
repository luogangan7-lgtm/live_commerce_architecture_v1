// Package stripeadmin owns the operator-only Stripe registrar: provisioning a store's Stripe account
// and encrypted API key, webhook endpoint signing secrets, method qualification and method rows, and
// the LIVE steps (approval with a Stripe readiness read, canary verification, per-store revoke), each as
// one call to a scoped registry SQL definer. It is the only writer of those rows.
// It never runs inside the API or the payment worker, never serves merchants, never prints or
// persists plaintext keys/secrets (only AEAD ciphertext reaches PG), never moves money (the LIVE probe
// creates and expires one session that nobody pays), never talks to LIVE unless opened with OpenLive and
// the owner's flag+reference pair, and never reads the account row back (the registrar role cannot).
//
// External: api.stripe.com through internal/integrations/psp/stripe: VerifyAccount, ProbeCheckout and (LIVE
// approval only) AccountReadiness, with the operator's key (sk_test_/rk_test_ via Open; rk_live_ via OpenLive;
// NewWithMockTransport only when a test injects a transport, never for LIVE).
// Contract: contracts/stripe-psp-v1.md §0.2, §13; contracts/stripe-live-enable-v1.md §3.4, §5.2.
package stripeadmin
