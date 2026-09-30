// platform_billing.go owns the API process wiring of platform billing (customers-billing-v1 T17,
// brief billing-core B1-B4): LC_BILLING_* configuration, the commerce_stripe_ingress pool for the
// platform webhook, the billing.Service and the exact-path mount of POST /v1/platform/stripe/webhook.
// It never reads a variable while LC_BILLING_ENABLED is unset, never opens a pool for disabled billing,
// never reuses the merchant pool for the webhook and never logs a key, secret or DSN.
//
// External host: api.stripe.com (platform account), only through internal/billing.

package main

import (
	"context"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"livecommerce/internal/billing"

	"livecommerce/internal/platform"
)

// platformWebhookPath is the only path the platform webhook answers (contract §5 Platform).
const platformWebhookPath = "/v1/platform/stripe/webhook"

// buildPlatformBilling is called once by main.go with the startup context, the merchant pool and
// os.Getenv (the getenv seam proves disabled billing reads nothing else, like loadStripeWebhookConfig).
// Disabled (LC_BILLING_ENABLED unset): svc nil and webhook nil; standing and GET billing still work
// through a nil Service. Enabled: the ingress pool is opened with platform.OpenStripeIngressPool
// (COMMERCE_STRIPE_INGRESS_DATABASE_URL, the commerce_stripe_ingress login, C-4), proven to point at the
// same database as mainPool, and billing.New runs the B4 account check. A BD1 conflict or an unreadable
// account yields a DISABLED service (webhook nil, ingress pool closed): checkout/portal answer 503 for the
// process lifetime. closeFn is never nil.
func buildPlatformBilling(ctx context.Context, mainPool *pgxpool.Pool, getenv func(string) string) (svc *billing.Service, webhook http.Handler, closeFn func(), err error) {
	noop := func() {}
	cfg, enabled, err := billing.LoadConfig(getenv)
	if err != nil {
		return nil, nil, noop, err
	}
	if !enabled {
		return nil, nil, noop, nil
	}
	dsn := getenv("COMMERCE_STRIPE_INGRESS_DATABASE_URL")
	if ctx == nil || mainPool == nil || len(dsn) < 1 || len(dsn) > 8192 {
		return nil, nil, noop, billing.ErrConfig
	}
	startup, stop := context.WithTimeout(ctx, 30*time.Second)
	defer stop()
	ingress, err := platform.OpenStripeIngressPool(startup, dsn)
	if err != nil {
		return nil, nil, noop, errStripeDatabase
	}
	if err := platform.ValidateSameDatabase(startup, mainPool, ingress); err != nil {
		ingress.Close()
		return nil, nil, noop, errStripeDatabase
	}
	svc, err = billing.New(startup, mainPool, cfg, nil)
	if err != nil {
		ingress.Close()
		return nil, nil, noop, err
	}
	if !svc.Enabled() { // B4: disabled for the process lifetime, no webhook, no ingress pool
		ingress.Close()
		return svc, nil, noop, nil
	}
	return svc, svc.WebhookHandler(ingress), ingress.Close, nil
}

// mountPlatformBilling routes exactly platformWebhookPath to the webhook (the handler itself answers 405 to
// non-POST) and everything else to fallback. A nil webhook returns fallback unchanged. Aliases such as
// //v1/... or /v1/platform/stripe/../stripe/webhook are not rewritten here: they fall through to
// the ServeMux, which redirects to the clean path instead of feeding Stripe's signed body to the handler
// under another name.
func mountPlatformBilling(fallback, webhook http.Handler) http.Handler {
	if webhook == nil {
		return fallback
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == platformWebhookPath {
			webhook.ServeHTTP(w, r)
			return
		}
		fallback.ServeHTTP(w, r)
	})
}
