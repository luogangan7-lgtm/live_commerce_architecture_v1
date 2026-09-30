// stripe_webhook.go owns the API process wiring of POST /v1/stripe/webhook/{endpoint_id}
// (contracts/stripe-psp-v1.md §0.2, §12; brief stripe-b1-ingress-assembly).
// It never reads STRIPE_* variables or the payment API-key keyring, and never opens a pool
// unless the flag is on.

package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"path"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"livecommerce/internal/integrations/accounts"
	"livecommerce/internal/payments/stripewebhook"
	"livecommerce/internal/platform"
)

var errStripeConfig = errors.New("stripe_api_invalid_config")
var errStripeDatabase = errors.New("stripe_api_database_unavailable")

type stripeWebhookConfig struct {
	enabled bool
	dsn     string
	profile string
	keys    *accounts.Keyring
}

func (stripeWebhookConfig) String() string     { return "stripeWebhookConfig{redacted}" }
func (c stripeWebhookConfig) GoString() string { return c.String() }
func (stripeWebhookConfig) MarshalJSON() ([]byte, error) {
	return json.Marshal("stripeWebhookConfig{redacted}")
}

// remapSigningNames lets the existing accounts.LoadKeyring parse a distinct keyring: the
// webhook signing custody (stripe-webhook-v1) must not share env names, hence keys, with the
// payment API-key keyring that only the worker holds.
func remapSigningNames(getenv func(string) string) func(string) string {
	return func(name string) string {
		if suffix, ok := strings.CutPrefix(name, "COMMERCE_ACCOUNT_"); ok {
			return getenv("COMMERCE_STRIPE_WEBHOOK_" + suffix)
		}
		return ""
	}
}

func loadStripeWebhookConfig(getenv func(string) string, addr string) (stripeWebhookConfig, error) {
	var c stripeWebhookConfig
	if getenv == nil {
		return c, errStripeConfig
	}
	enabled, err := flag(getenv("COMMERCE_STRIPE_WEBHOOK_ENABLED"))
	if err != nil {
		return c, errStripeConfig
	}
	if !enabled { // disabled reads nothing else (SP15)
		return c, nil
	}
	if !privateIdentityAddress(addr) {
		return stripeWebhookConfig{}, errStripeConfig
	}
	c.dsn = getenv("COMMERCE_STRIPE_INGRESS_DATABASE_URL")
	if len(c.dsn) < 1 || len(c.dsn) > 8192 || strings.TrimSpace(c.dsn) == "" {
		return stripeWebhookConfig{}, errStripeConfig
	}
	// stripe-live-enable-v1 §5.2: LIVE ingress is admitted only with the owner's flag+reference pair. The pair
	// is read only for LIVE, so a SANDBOX/MOCK deployment reads exactly the names it always did (SP15).
	c.profile = getenv("COMMERCE_PAYMENT_PROFILE")
	if c.profile != "PROVIDER_MOCK" && c.profile != "SANDBOX" && c.profile != "LIVE" {
		return stripeWebhookConfig{}, errStripeConfig
	}
	if c.profile == "LIVE" {
		if _, err := loadStripeLiveApproval(getenv); err != nil {
			return stripeWebhookConfig{}, errStripeConfig
		}
	}
	if c.keys, err = accounts.LoadKeyring(remapSigningNames(getenv)); err != nil {
		return stripeWebhookConfig{}, errStripeConfig
	}
	c.enabled = true
	return c, nil
}

// buildStripeWebhookHandler opens the dedicated ingress pool (never the merchant pool),
// proves it points at the same database as mainPool, and builds the handler. The returned
// close func releases the ingress pool.
func buildStripeWebhookHandler(ctx context.Context, mainPool *pgxpool.Pool, c stripeWebhookConfig) (http.Handler, func(), error) {
	if !c.enabled {
		return nil, func() {}, nil
	}
	if ctx == nil || mainPool == nil || c.keys == nil {
		return nil, nil, errStripeConfig
	}
	startup, stop := context.WithTimeout(ctx, 10*time.Second)
	defer stop()
	ingressPool, err := platform.OpenStripeIngressPool(startup, c.dsn)
	if err != nil {
		return nil, nil, errStripeDatabase
	}
	preflight, done := context.WithTimeout(startup, 5*time.Second)
	defer done()
	if err := platform.ValidateSameDatabase(preflight, mainPool, ingressPool); err != nil {
		ingressPool.Close()
		return nil, nil, errStripeDatabase
	}
	inbox, err := stripewebhook.NewInbox(preflight, ingressPool, c.keys, c.profile)
	if err != nil {
		ingressPool.Close()
		return nil, nil, errStripeDatabase
	}
	h, err := stripewebhook.NewHandler(inbox)
	if err != nil {
		ingressPool.Close()
		return nil, nil, errStripeDatabase
	}
	return h, ingressPool.Close, nil
}

// mountStripe reserves the whole /v1/stripe namespace, raw and path.Clean'ed, before ServeMux
// can rewrite aliases (as mountMeta does); the handler itself rejects every non-literal path.
func mountStripe(fallback, stripeHandler http.Handler) http.Handler {
	if stripeHandler == nil {
		return fallback
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/v1/stripe") ||
			strings.HasPrefix(path.Clean(r.URL.Path), "/v1/stripe") {
			stripeHandler.ServeHTTP(w, r)
			return
		}
		fallback.ServeHTTP(w, r)
	})
}
