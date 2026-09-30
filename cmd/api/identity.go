package main

import (
	"context"
	"encoding/base64"
	"errors"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"livecommerce/internal/identity"
	"livecommerce/internal/identityhttp"
	"livecommerce/internal/mail"
	"livecommerce/internal/oidclogin"
	"livecommerce/internal/platform"
)

type identityConfig struct {
	enabled                   bool
	dsn, bffKey, publicOrigin string
	provider                  oidclogin.Config
	policy                    identity.Policy
	// Password login (merchant-password-auth-v1 §5). passwordLogin=false leaves everything below zero.
	passwordLogin bool
	passwords     identity.PasswordPolicy
	mailer        *mail.SMTP // built (never dialled) at load so an invalid From/host fails startup
}

func flag(value string) (bool, error) {
	switch value {
	case "", "0":
		return false, nil
	case "1":
		return true, nil
	default:
		return false, errors.New("invalid boolean configuration")
	}
}

// The getenv seam proves disabled identity cannot accidentally read secrets,
// open its authority pool or perform provider discovery. No config is logged.
func loadIdentityConfig(getenv func(string) string) (identityConfig, error) {
	var c identityConfig
	var err error
	c.enabled, err = flag(getenv("COMMERCE_IDENTITY_ENABLED"))
	if err != nil || !c.enabled {
		return c, err
	}
	allowLoopback, err := flag(getenv("COMMERCE_IDENTITY_ALLOW_LOOPBACK_TESTS"))
	if err != nil {
		return c, err
	}
	c.publicOrigin, err = publicOrigin(getenv("COMMERCE_PUBLIC_ORIGIN"), allowLoopback)
	if err != nil {
		return c, err
	}
	c.dsn = getenv("COMMERCE_IDENTITY_DATABASE_URL")
	c.bffKey = getenv("COMMERCE_BFF_KEY")
	c.passwordLogin, err = flag(getenv("COMMERCE_PASSWORD_LOGIN_ENABLED"))
	if err != nil {
		return c, err
	}
	c.provider = oidclogin.Config{
		Issuer: getenv("COMMERCE_OIDC_ISSUER"), ClientID: getenv("COMMERCE_OIDC_CLIENT_ID"),
		ClientSecret: getenv("COMMERCE_OIDC_CLIENT_SECRET"), RedirectURL: c.publicOrigin + "/api/auth/callback",
		AllowLoopbackForTests: allowLoopback,
	}
	c.policy.PasswordLogin = c.passwordLogin
	c.policy.ProviderKey = getenv("COMMERCE_IDENTITY_PROVIDER_KEY")
	c.policy.SessionTTL, err = time.ParseDuration(getenv("COMMERCE_SESSION_TTL"))
	if err != nil || c.policy.SessionTTL < 5*time.Minute || c.policy.SessionTTL > 24*time.Hour {
		return c, errors.New("invalid session duration")
	}
	c.policy.OnboardingEnabled, err = flag(getenv("COMMERCE_ONBOARDING_ENABLED"))
	if err != nil {
		return c, err
	}
	if currencies := getenv("COMMERCE_ONBOARDING_CURRENCIES"); currencies != "" {
		for _, currency := range strings.Split(currencies, ",") {
			currency = strings.TrimSpace(currency)
			if len(currency) != 3 || strings.IndexFunc(currency, func(r rune) bool { return r < 'A' || r > 'Z' }) != -1 {
				return c, errors.New("invalid onboarding currency")
			}
			c.policy.Currencies = append(c.policy.Currencies, currency)
		}
	}
	if c.dsn == "" || !identityhttp.ValidSecret(c.bffKey) || len(c.policy.ProviderKey) > 128 || (c.policy.OnboardingEnabled && len(c.policy.Currencies) == 0) {
		return c, errors.New("incomplete identity configuration")
	}
	// OIDC is required unless password login is on (ruling R-4); when on, OIDC is all-or-nothing so a
	// half-typed issuer cannot silently disable the OIDC button.
	oidcAny := c.provider.Issuer != "" || c.provider.ClientID != "" || c.provider.ClientSecret != "" || c.policy.ProviderKey != ""
	oidcAll := c.provider.Issuer != "" && c.provider.ClientID != "" && len(c.policy.ProviderKey) > 0
	if (!c.passwordLogin && !oidcAll) || (c.passwordLogin && oidcAny && !oidcAll) {
		return c, errors.New("incomplete identity configuration")
	}
	if c.passwordLogin {
		if err := loadPasswordConfig(getenv, allowLoopback, &c); err != nil {
			return c, err
		}
	}
	if getenv("COMMERCE_FIXTURE_ENABLED") != "" && getenv("COMMERCE_FIXTURE_ENABLED") != "0" {
		return c, errors.New("identity and fixture modes are mutually exclusive")
	}
	return c, nil
}

// smtpHostPattern is a DNS name with at least one dot (LDH labels); IP literals and "localhost" are
// handled separately and only allowed with the loopback test flag.
var smtpHostPattern = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]*[a-z0-9])?(\.[a-z0-9]([a-z0-9-]*[a-z0-9])?)+$`)

// loadPasswordConfig reads the §5 password-login variables. Secrets come from NAME (lcentry-expanded
// *_FILE) or a NAME_FILE path, are read once, and are never logged or echoed in an error (I11, O-D).
func loadPasswordConfig(getenv func(string) string, allowLoopback bool, c *identityConfig) error {
	pepper, err := secretEnv(getenv, "COMMERCE_AUTH_PEPPER")
	if err != nil {
		return errors.New("invalid auth pepper configuration")
	}
	raw, err := base64.RawURLEncoding.Strict().DecodeString(pepper)
	if err != nil || len(raw) != 32 {
		return errors.New("invalid auth pepper configuration")
	}
	smtpPassword, err := secretEnv(getenv, "COMMERCE_SMTP_PASSWORD")
	if err != nil || len(smtpPassword) > 256 || strings.IndexFunc(smtpPassword, func(r rune) bool { return r < 32 || r == 127 }) != -1 {
		return errors.New("invalid smtp password configuration")
	}
	host := strings.ToLower(getenv("COMMERCE_SMTP_HOST"))
	if loopbackHost(host) {
		if !allowLoopback {
			return errors.New("smtp host must be a DNS name")
		}
	} else if !smtpHostPattern.MatchString(host) || len(host) > 253 {
		return errors.New("smtp host must be a DNS name")
	}
	dailyCap := 200
	if v := getenv("COMMERCE_MAIL_DAILY_CAP"); v != "" {
		if dailyCap, err = strconv.Atoi(v); err != nil || dailyCap < 20 || dailyCap > 100000 {
			return errors.New("invalid mail daily cap")
		}
	}
	breach := getenv("COMMERCE_BREACH_CHECK")
	if breach == "" {
		breach = "hibp"
	}
	if (breach != "hibp" && breach != "off") || (breach == "off" && !allowLoopback) {
		return errors.New("invalid breach check configuration")
	}
	if getenv("COMMERCE_HIBP_BASE_URL") != "" && !allowLoopback {
		return errors.New("hibp base url override requires loopback tests")
	}
	c.passwords = identity.PasswordPolicy{Pepper: raw, SessionTTL: c.policy.SessionTTL, MailDailyCap: dailyCap,
		BreachCheck: breach, HIBPBaseURL: getenv("COMMERCE_HIBP_BASE_URL"), AllowLoopback: allowLoopback}
	// Port stays 0 (= 465, implicit TLS) and RootCAs nil (system roots): only in-process tests override them.
	c.mailer, err = mail.NewSMTP(mail.Config{Host: host, Username: getenv("COMMERCE_SMTP_USERNAME"), Password: smtpPassword,
		From: getenv("COMMERCE_MAIL_FROM"), AllowLoopback: allowLoopback})
	if err != nil {
		return errors.New("invalid smtp configuration") // NewSMTP enforces From == Username (M2)
	}
	return nil
}

// secretEnv resolves a secret that deploy delivers as NAME_FILE (contract §5). In containers
// deploy/tools/lcentry expands NAME_FILE into NAME=<contents> and drops NAME_FILE before exec, so the
// process normally sees NAME; a directly started binary (tests, local runs) may instead pass NAME_FILE
// with an absolute path. Both set is an error, exactly like lcentry's rule. Errors never carry values.
func secretEnv(getenv func(string) string, name string) (string, error) {
	value, path := getenv(name), getenv(name+"_FILE")
	switch {
	case value != "" && path != "":
		return "", errors.New(name + " and " + name + "_FILE both set")
	case value != "":
		return strings.TrimRight(value, "\r\n"), nil
	case path != "":
		return readSecretFile(path)
	}
	return "", errors.New(name + " is not set")
}

// readSecretFile returns the first line of an absolute-path secret file (<= 1 KiB, trailing CR/LF
// trimmed). Errors never include the content.
func readSecretFile(path string) (string, error) {
	if !filepath.IsAbs(path) {
		return "", errors.New("secret file path must be absolute")
	}
	f, err := os.Open(path)
	if err != nil {
		return "", errors.New("secret file unreadable")
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, 1025))
	if err != nil || len(b) == 0 || len(b) > 1024 {
		return "", errors.New("secret file empty or too large")
	}
	v := strings.TrimRight(string(b), "\r\n")
	if v == "" {
		return "", errors.New("secret file empty")
	}
	return v, nil
}

func publicOrigin(raw string, allowLoopback bool) (string, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || u.Opaque != "" || u.User != nil || (u.Path != "" && u.Path != "/") || strings.ContainsAny(raw, "?#") || strings.IndexFunc(raw, func(r rune) bool { return r > 127 || r <= 32 }) != -1 {
		return "", errors.New("invalid public origin")
	}
	if u.Scheme != "https" && !(u.Scheme == "http" && allowLoopback && loopbackHost(u.Hostname())) {
		return "", errors.New("public origin requires HTTPS")
	}
	host := strings.ToLower(u.Host)
	if port := u.Port(); port != "" {
		n, err := strconv.Atoi(port)
		if err != nil || n < 1 || n > 65535 {
			return "", errors.New("invalid public origin port")
		}
		if (u.Scheme == "https" && n == 443) || (u.Scheme == "http" && n == 80) {
			host = strings.TrimSuffix(host, ":"+port)
		}
	}
	return u.Scheme + "://" + host, nil
}

func loopbackHost(host string) bool {
	if host == "localhost" {
		return true
	}
	addr, err := netip.ParseAddr(host)
	return err == nil && addr.Zone() == "" && addr.IsLoopback()
}

func privateIdentityAddress(addr string) bool {
	host, port, err := net.SplitHostPort(addr)
	if err != nil || host == "localhost" || !loopbackHost(host) {
		return false
	}
	n, err := strconv.Atoi(port)
	return err == nil && n > 0 && n <= 65535
}

// identityMux serves the password routes and, for everything else under /v1/identity/, the OIDC/store
// handler. cmd/api/main.go mounts the result at /v1/identity/ and stays unchanged: the longer
// /v1/identity/password/ pattern wins.
func identityMux(oidc, password http.Handler) http.Handler {
	mux := http.NewServeMux()
	mux.Handle("/v1/identity/password/", password)
	mux.Handle("/v1/identity/", oidc)
	return mux
}

func buildIdentityHandler(ctx context.Context, c identityConfig) (http.Handler, func(), error) {
	if !c.enabled {
		return nil, func() {}, nil
	}
	// This pool is never supplied to platform/httpapi. It can execute only the
	// bounded identity functions; the existing provider library validates tokens.
	pool, err := platform.OpenIdentityPool(ctx, c.dsn)
	if err != nil {
		return nil, nil, err
	}
	var provider identity.Provider // stays a nil interface when only password login is configured (A8)
	if c.provider.Issuer != "" {
		p, err := oidclogin.New(ctx, c.provider)
		if err != nil {
			pool.Close()
			return nil, nil, err
		}
		provider = p
	}
	s, err := identity.New(pool, provider, c.policy)
	if err != nil {
		pool.Close()
		return nil, nil, err
	}
	handler, err := identityhttp.NewHandler(s, c.bffKey)
	if err != nil {
		pool.Close()
		return nil, nil, err
	}
	if !c.passwordLogin {
		return handler, pool.Close, nil
	}
	passwords, err := identity.NewPasswords(pool, c.mailer, c.passwords)
	if err != nil {
		pool.Close()
		return nil, nil, err
	}
	passwordHandler, err := identityhttp.NewPasswordHandler(passwords, c.bffKey)
	if err != nil {
		pool.Close()
		return nil, nil, err
	}
	return identityMux(handler, passwordHandler), func() {
		// A12: stop background mail sends and wait for in-flight ones (<= 25 s) BEFORE the pool closes,
		// because each finished send records its outcome through the pool.
		drain, cancel := context.WithTimeout(context.Background(), 25*time.Second)
		defer cancel()
		_ = passwords.Close(drain)
		pool.Close()
	}, nil
}
