// config.go: the CVS_ECPAY_* environment switches (§12). Fail-closed: every switch defaults to off,
// and an enabled deployment must name a valid https hooks origin, because ECPay reaches the status
// route only through it (F11, §0.3 P1).

package ecpay

import (
	"net/url"
	"strings"
)

// Config is the parsed ECPay switch set. Enabled is CVS_ECPAY_ENABLED, LiveCreate is
// CVS_ECPAY_LIVE_CREATE (owner-only, §0.3 P4), HooksOrigin is COMMERCE_CVS_HOOKS_ORIGIN normalised to
// "https://host[:port]" with no trailing slash.
type Config struct {
	Enabled, LiveCreate bool
	HooksOrigin         string
}

func flag(v string) (bool, bool) {
	switch v {
	case "", "0":
		return false, true
	case "1":
		return true, true
	}
	return false, false
}

// httpsOrigin accepts only an ASCII https origin (no userinfo, path, query or fragment; F11: the
// ServerReplyURL must be ASCII/punycode) and returns it without a trailing slash.
func httpsOrigin(raw string) (string, bool) {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" ||
		u.ForceQuery || (u.Path != "" && u.Path != "/") || u.Opaque != "" || !isASCII(raw) || strings.ContainsAny(raw, " \t\r\n") {
		return "", false
	}
	return "https://" + u.Host, true
}

// LoadConfig reads CVS_ECPAY_ENABLED, CVS_ECPAY_LIVE_CREATE ("0"/"1"/unset) and
// COMMERCE_CVS_HOOKS_ORIGIN. Any other flag value, or Enabled with a hooks origin that is not an https
// origin, is ErrConfig. The error never echoes a value.
func LoadConfig(getenv func(string) string) (Config, error) {
	if getenv == nil {
		return Config{}, ErrConfig
	}
	enabled, ok1 := flag(getenv("CVS_ECPAY_ENABLED"))
	live, ok2 := flag(getenv("CVS_ECPAY_LIVE_CREATE"))
	if !ok1 || !ok2 {
		return Config{}, ErrConfig
	}
	cfg := Config{Enabled: enabled, LiveCreate: live}
	if origin, ok := httpsOrigin(getenv("COMMERCE_CVS_HOOKS_ORIGIN")); ok {
		cfg.HooksOrigin = origin
	} else if enabled {
		return Config{}, ErrConfig
	}
	return cfg, nil
}
