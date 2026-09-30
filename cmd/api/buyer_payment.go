// buyer_payment.go owns the API-side configuration and assembly of the hosted buyer payment
// service (PAYUNi always, Stripe Checkout when COMMERCE_STRIPE_CHECKOUT_ENABLED=1). It never
// reads STRIPE_* secrets, starts a worker, or contacts a provider from the API process.

package main

import (
	"context"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"

	"livecommerce/internal/checkout"
	"livecommerce/internal/integrations/accounts"
	"livecommerce/internal/platform"
)

type buyerPaymentConfig struct {
	enabled   bool
	hostedDSN string
	profile   string
	endpoints checkout.HostedConfig
	stripe    *checkout.StripeHostedConfig // nil unless COMMERCE_STRIPE_CHECKOUT_ENABLED=1
	keys      *accounts.Keyring
}

// A disabled nested feature reads only its flag. The root buyer loader calls
// this only after the buyer listener and its own configuration are admitted.
func loadBuyerPaymentConfig(getenv func(string) string, addr string) (buyerPaymentConfig, error) {
	var c buyerPaymentConfig
	enabled, err := flag(getenv("COMMERCE_BUYER_PAYMENT_ENABLED"))
	if err != nil {
		return c, errBuyerConfig
	}
	if !enabled {
		return c, nil
	}
	if !privateIdentityAddress(addr) {
		return c, errBuyerConfig
	}
	c.hostedDSN = getenv("COMMERCE_HOSTED_DATABASE_URL")
	c.profile = getenv("COMMERCE_PAYMENT_PROFILE")
	c.endpoints = checkout.HostedConfig{ReturnURL: getenv("COMMERCE_PAYMENT_RETURN_URL"),
		NotifyURL: getenv("COMMERCE_PAYMENT_NOTIFY_URL")}
	if strings.TrimSpace(c.hostedDSN) == "" ||
		(c.profile != "PROVIDER_MOCK" && c.profile != "SANDBOX" && c.profile != "LIVE") {
		return buyerPaymentConfig{}, errBuyerConfig
	}
	canonical, _, err := c.endpoints.CanonicalDigest()
	if err != nil {
		return buyerPaymentConfig{}, errBuyerConfig
	}
	c.endpoints = canonical
	c.keys, err = loadAccountKeys(getenv)
	if err != nil {
		return buyerPaymentConfig{}, errBuyerConfig
	}
	// Nested flag, read only once buyer payment itself is admitted. Stripe rides on the PAYUNi
	// hosted service in B1 (no Stripe-only deployment) and never reads STRIPE_* variables:
	// API keys live in PG under the operator registrar. The return URL is the same neutral
	// COMMERCE_PAYMENT_RETURN_URL, already canonicalised above.
	// LD6: this flag is the platform-wide kill switch (LC_STRIPE_CHECKOUT_ENABLED in compose). On LIVE it also
	// needs the owner's flag+reference pair (stripe-live-enable-v1 §5.2); the pair is read only in that case.
	stripeEnabled, err := flag(getenv("COMMERCE_STRIPE_CHECKOUT_ENABLED"))
	if err != nil {
		return buyerPaymentConfig{}, errBuyerConfig
	}
	if stripeEnabled && c.profile == "LIVE" {
		if _, err := loadStripeLiveApproval(getenv); err != nil {
			return buyerPaymentConfig{}, errBuyerConfig
		}
	}
	if stripeEnabled {
		stripeConfig := checkout.StripeHostedConfig{ReturnURL: c.endpoints.ReturnURL}
		if _, _, err := stripeConfig.CanonicalDigest(); err != nil {
			return buyerPaymentConfig{}, errBuyerConfig
		}
		c.stripe = &stripeConfig
	}
	c.enabled = true
	return c, nil
}

func buildBuyerPayment(ctx context.Context, c buyerPaymentConfig) (*checkout.HostedPaymentStarter, *pgxpool.Pool, error) {
	if !c.enabled {
		return nil, nil, nil
	}
	if ctx == nil || c.keys == nil {
		return nil, nil, errBuyerConfig
	}
	pool, err := platform.OpenHostedPool(ctx, c.hostedDSN)
	if err != nil {
		return nil, nil, errBuyerConfig
	}
	// This client inserts payment query/signal jobs in the payment transaction. It
	// does not start a worker or make a provider request in the API process.
	// Place it in the family schema maintained only by payment workers.
	jobs, err := river.NewClient(riverpgxv5.New(pool), &river.Config{Schema: "river_payment"})
	if err != nil {
		pool.Close()
		return nil, nil, errBuyerConfig
	}
	service, err := checkout.NewHostedPaymentService(ctx, pool, jobs, c.profile, c.keys,
		checkout.HostedProviders{PAYUNi: &c.endpoints, Stripe: c.stripe})
	if err != nil {
		pool.Close()
		return nil, nil, errBuyerConfig
	}
	return service, pool, nil
}
