// config.go: environment-to-Config admission for the platform billing service (brief B3). It reads only
// through the getenv seam it is given, so disabled billing provably reads no secret, and it never echoes
// a value in an error or a formatted Config (redacted in every formatting path).

package billing

import (
	"encoding/json"
	"net/url"
	"regexp"
	"strings"
)

// Environment names (contract §5, brief B3).
const (
	envEnabled       = "LC_BILLING_ENABLED"
	envSecretKey     = "LC_PLATFORM_STRIPE_SECRET_KEY"
	envWebhookSecret = "LC_PLATFORM_STRIPE_WEBHOOK_SECRET"
	envPriceIDs      = "LC_BILLING_PRICE_IDS"
	envReturnOrigin  = "LC_BILLING_RETURN_ORIGIN"
)

var (
	// BD8: SANDBOX only in v1, so only test keys are admitted (same key syntax as psp/stripe).
	// https://docs.stripe.com/keys (retrieved 2026-09-29).
	secretKeyPattern = regexp.MustCompile(`^(sk|rk)_test_[A-Za-z0-9]{16,240}$`)
	// Same webhook secret syntax the shared WebhookVerifier admits.
	// https://docs.stripe.com/webhooks/signature (retrieved 2026-09-29).
	webhookSecretPattern = regexp.MustCompile(`^whsec_[!-~]{16,249}$`)
	pricePattern         = regexp.MustCompile(`^price_[A-Za-z0-9]{1,64}$`)
)

const maxPriceIDs = 10 // I23 / contract §7

// Config is the only way to hand platform Stripe credentials to this package.
type Config struct {
	SecretKey     string   // sk_test_ or rk_test_ only
	WebhookSecret string   // whsec_...
	ReturnOrigin  string   // https origin of the admin app, no path
	PriceIDs      []string // 1..10 configured plan prices
}

func (Config) String() string               { return "billing.Config{redacted}" }
func (c Config) GoString() string           { return c.String() }
func (Config) MarshalJSON() ([]byte, error) { return json.Marshal("billing.Config{redacted}") }

// LoadConfig reads the billing environment. LC_BILLING_ENABLED unset means disabled and nothing else is
// read; "1" enables and then every other variable is required and validated; any other value is an error.
// Errors are ErrConfig only, never the offending value.
func LoadConfig(getenv func(string) string) (cfg Config, enabled bool, err error) {
	if getenv == nil {
		return Config{}, false, ErrConfig
	}
	switch getenv(envEnabled) {
	case "":
		return Config{}, false, nil
	case "1":
	default:
		return Config{}, false, ErrConfig
	}
	cfg.SecretKey = getenv(envSecretKey)
	cfg.WebhookSecret = getenv(envWebhookSecret)
	if !secretKeyPattern.MatchString(cfg.SecretKey) || !webhookSecretPattern.MatchString(cfg.WebhookSecret) {
		return Config{}, false, ErrConfig
	}
	seen := map[string]bool{}
	for _, id := range strings.Split(getenv(envPriceIDs), ",") {
		id = strings.TrimSpace(id)
		if !pricePattern.MatchString(id) || seen[id] {
			return Config{}, false, ErrConfig
		}
		seen[id] = true
		cfg.PriceIDs = append(cfg.PriceIDs, id)
	}
	if len(cfg.PriceIDs) < 1 || len(cfg.PriceIDs) > maxPriceIDs {
		return Config{}, false, ErrConfig
	}
	origin, ok := normalizeOrigin(getenv(envReturnOrigin))
	if !ok {
		return Config{}, false, ErrConfig
	}
	cfg.ReturnOrigin = origin
	return cfg, true, nil
}

// normalizeOrigin admits an https origin (scheme://host[:port]) without userinfo, path, query or fragment.
// http is admitted only for a loopback host so a local SANDBOX run (CB10) works; production uses https.
func normalizeOrigin(raw string) (string, bool) {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Opaque != "" ||
		(u.Path != "" && u.Path != "/") || strings.Contains(raw, "#") || strings.Contains(raw, "?") {
		return "", false
	}
	switch u.Scheme {
	case "https":
	case "http":
		if h := u.Hostname(); h != "localhost" && h != "127.0.0.1" && h != "::1" {
			return "", false
		}
	default:
		return "", false
	}
	return u.Scheme + "://" + u.Host, true
}
