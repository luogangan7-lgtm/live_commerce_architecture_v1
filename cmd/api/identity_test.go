package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestIdentityDisabledDoesNotReadAuthorityConfiguration(t *testing.T) {
	for _, value := range []string{"", "0"} {
		c, err := loadIdentityConfig(func(key string) string {
			if key != "COMMERCE_IDENTITY_ENABLED" {
				t.Fatal("disabled identity read another configuration key")
			}
			return value
		})
		if err != nil || c.enabled {
			t.Fatal("disabled config was not disabled")
		}
		h, closeFn, err := buildIdentityHandler(context.Background(), c)
		if err != nil || h != nil || closeFn == nil {
			t.Fatal("disabled identity was constructed")
		}
		closeFn()
	}
}

func TestIdentityEnabledConfigurationFailClosed(t *testing.T) {
	base := map[string]string{
		"COMMERCE_IDENTITY_ENABLED": "1", "COMMERCE_PUBLIC_ORIGIN": "https://merchant.example",
		"COMMERCE_IDENTITY_DATABASE_URL": "postgres://config-not-connected.invalid/identity", "COMMERCE_BFF_KEY": strings.Repeat("A", 43),
		"COMMERCE_OIDC_ISSUER": "https://idp.example", "COMMERCE_OIDC_CLIENT_ID": "test-client",
		"COMMERCE_IDENTITY_PROVIDER_KEY": "test-provider", "COMMERCE_SESSION_TTL": "1h",
		"COMMERCE_ONBOARDING_ENABLED": "1", "COMMERCE_ONBOARDING_CURRENCIES": "TWD,USD",
	}
	c, err := loadIdentityConfig(func(key string) string { return base[key] })
	if err != nil || !c.enabled || c.provider.RedirectURL != "https://merchant.example/api/auth/callback" || len(c.policy.Currencies) != 2 {
		t.Fatal("valid configuration rejected")
	}
	for _, key := range []string{"COMMERCE_PUBLIC_ORIGIN", "COMMERCE_IDENTITY_DATABASE_URL", "COMMERCE_BFF_KEY", "COMMERCE_OIDC_ISSUER", "COMMERCE_OIDC_CLIENT_ID", "COMMERCE_IDENTITY_PROVIDER_KEY", "COMMERCE_SESSION_TTL", "COMMERCE_ONBOARDING_CURRENCIES"} {
		t.Run("missing_"+key, func(t *testing.T) {
			if _, err := loadIdentityConfig(func(k string) string {
				if k == key {
					return ""
				}
				return base[k]
			}); err == nil {
				t.Fatal("missing required configuration accepted")
			}
		})
	}
	for key, values := range map[string][]string{
		"COMMERCE_IDENTITY_ENABLED": {"true", "2"}, "COMMERCE_SESSION_TTL": {"1m", "25h", "forever"},
		"COMMERCE_BFF_KEY": {"short", strings.Repeat("A", 42) + "B"}, "COMMERCE_FIXTURE_ENABLED": {"1"},
		"COMMERCE_ONBOARDING_ENABLED": {"true"}, "COMMERCE_ONBOARDING_CURRENCIES": {"USD,", "twd", "TWD, US1"},
	} {
		for _, value := range values {
			t.Run("invalid_"+key+"_"+value, func(t *testing.T) {
				if _, err := loadIdentityConfig(func(k string) string {
					if k == key {
						return value
					}
					return base[k]
				}); err == nil {
					t.Fatal("invalid configuration accepted")
				}
			})
		}
	}
}

func TestIdentityPublicOriginAndPrivateListener(t *testing.T) {
	for _, raw := range []string{"", "https://u:p@merchant.example", "https://merchant.example/path", "https://merchant.example?x=1", "https://merchant.example#", "http://merchant.example", "http://127.0.0.1:3100", "javascript:alert(1)", "//merchant.example", "https://merchant.example:99999"} {
		if _, err := publicOrigin(raw, false); err == nil {
			t.Errorf("unsafe public origin accepted: %q", raw)
		}
	}
	for _, raw := range []string{"http://localhost:3100", "http://127.0.0.1:3100", "http://[::1]:3100"} {
		if _, err := publicOrigin(raw, true); err != nil {
			t.Fatal("explicit loopback test origin rejected")
		}
	}
	if _, err := publicOrigin("http://127.0.0.1.attacker.example", true); err == nil {
		t.Fatal("loopback prefix spoof accepted")
	}
	if got, err := publicOrigin("https://Merchant.Example:443/", false); err != nil || got != "https://merchant.example" {
		t.Fatal("canonical origin mismatch")
	}
	for _, addr := range []string{"127.0.0.1:8080", "[::1]:8080"} {
		if !privateIdentityAddress(addr) {
			t.Fatal("literal loopback listener rejected")
		}
	}
	for _, addr := range []string{":8080", "0.0.0.0:8080", "[::]:8080", "localhost:8080", "127.0.0.1:0", "127.0.0.1:65536", "10.0.0.1:8080"} {
		if privateIdentityAddress(addr) {
			t.Errorf("unsafe listener accepted: %s", addr)
		}
	}
}

// --- merchant password login configuration (merchant-password-auth-v1 §5) ---

const canarySMTPSecret = "canary-smtp-authorization-code" // written to a temp file only; must never appear in errors

func passwordEnv(t *testing.T) map[string]string {
	t.Helper()
	dir := t.TempDir()
	write := func(name, content string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	return map[string]string{
		"COMMERCE_IDENTITY_ENABLED": "1", "COMMERCE_PUBLIC_ORIGIN": "https://merchant.example",
		"COMMERCE_IDENTITY_DATABASE_URL": "postgres://config-not-connected.invalid/identity", "COMMERCE_BFF_KEY": strings.Repeat("A", 43),
		"COMMERCE_SESSION_TTL": "1h", "COMMERCE_PASSWORD_LOGIN_ENABLED": "1",
		"COMMERCE_AUTH_PEPPER_FILE":   write("pepper", base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{7}, 32))+"\n"),
		"COMMERCE_SMTP_PASSWORD_FILE": write("smtp", canarySMTPSecret+"\n"),
		"COMMERCE_SMTP_HOST":          "smtp.mail.example",
		"COMMERCE_SMTP_USERNAME":      "sender@mail.example",
		"COMMERCE_MAIL_FROM":          "xgdwm <sender@mail.example>",
		"COMMERCE_ONBOARDING_ENABLED": "1", "COMMERCE_ONBOARDING_CURRENCIES": "TWD",
	}
}

func TestIdentityPasswordOnlyConfigurationNeedsNoOIDC(t *testing.T) {
	base := passwordEnv(t)
	c, err := loadIdentityConfig(func(k string) string { return base[k] })
	if err != nil || !c.passwordLogin || !c.policy.PasswordLogin || c.mailer == nil || c.provider.Issuer != "" ||
		c.passwords.MailDailyCap != 200 || c.passwords.BreachCheck != "hibp" || len(c.passwords.Pepper) != 32 || c.passwords.SessionTTL != time.Hour {
		t.Fatalf("valid password-only configuration rejected or misread: %v %+v", err, c.passwords)
	}
	// OIDC alongside is still accepted, and then still needs all three values.
	with := passwordEnv(t)
	with["COMMERCE_OIDC_ISSUER"], with["COMMERCE_OIDC_CLIENT_ID"], with["COMMERCE_IDENTITY_PROVIDER_KEY"] = "https://idp.example", "client", "provider"
	if c, err := loadIdentityConfig(func(k string) string { return with[k] }); err != nil || c.provider.Issuer == "" {
		t.Fatalf("password + OIDC rejected: %v", err)
	}
	for _, drop := range []string{"COMMERCE_OIDC_CLIENT_ID", "COMMERCE_IDENTITY_PROVIDER_KEY", "COMMERCE_OIDC_ISSUER"} {
		partial := passwordEnv(t)
		partial["COMMERCE_OIDC_ISSUER"], partial["COMMERCE_OIDC_CLIENT_ID"], partial["COMMERCE_IDENTITY_PROVIDER_KEY"] = "https://idp.example", "client", "provider"
		delete(partial, drop)
		if _, err := loadIdentityConfig(func(k string) string { return partial[k] }); err == nil {
			t.Fatalf("partial OIDC configuration (missing %s) accepted", drop)
		}
	}
	// Password login OFF: the password variables are not even read, and OIDC is required again.
	off := passwordEnv(t)
	off["COMMERCE_PASSWORD_LOGIN_ENABLED"] = "0"
	read := map[string]bool{}
	if _, err := loadIdentityConfig(func(k string) string { read[k] = true; return off[k] }); err == nil {
		t.Fatal("no OIDC and no password login must be rejected")
	}
	for _, k := range []string{"COMMERCE_AUTH_PEPPER_FILE", "COMMERCE_SMTP_PASSWORD_FILE", "COMMERCE_SMTP_HOST", "COMMERCE_MAIL_FROM"} {
		if read[k] {
			t.Fatalf("password variable %s read while password login is off", k)
		}
	}
}

func TestIdentityPasswordConfigurationFailClosed(t *testing.T) {
	cases := map[string]func(map[string]string, string){
		"pepper file missing":  func(e map[string]string, dir string) { e["COMMERCE_AUTH_PEPPER_FILE"] = filepath.Join(dir, "nope") },
		"pepper path relative": func(e map[string]string, dir string) { e["COMMERCE_AUTH_PEPPER_FILE"] = "pepper" },
		"pepper path unset":    func(e map[string]string, dir string) { delete(e, "COMMERCE_AUTH_PEPPER_FILE") },
		"pepper not base64url": func(e map[string]string, dir string) {
			e["COMMERCE_AUTH_PEPPER_FILE"] = writeFile(t, dir, "p1", "not base64 !!!")
		},
		"pepper wrong length": func(e map[string]string, dir string) {
			e["COMMERCE_AUTH_PEPPER_FILE"] = writeFile(t, dir, "p2", base64.RawURLEncoding.EncodeToString(make([]byte, 31)))
		},
		"pepper empty":        func(e map[string]string, dir string) { e["COMMERCE_AUTH_PEPPER_FILE"] = writeFile(t, dir, "p3", "\n") },
		"smtp secret missing": func(e map[string]string, dir string) { delete(e, "COMMERCE_SMTP_PASSWORD_FILE") },
		"smtp secret empty":   func(e map[string]string, dir string) { e["COMMERCE_SMTP_PASSWORD_FILE"] = writeFile(t, dir, "s1", "") },
		"smtp secret too large": func(e map[string]string, dir string) {
			e["COMMERCE_SMTP_PASSWORD_FILE"] = writeFile(t, dir, "s2", strings.Repeat("x", 2000))
		},
		"smtp secret control": func(e map[string]string, dir string) {
			e["COMMERCE_SMTP_PASSWORD_FILE"] = writeFile(t, dir, "s3", "ab\x01cd")
		},
		"host unset":         func(e map[string]string, dir string) { delete(e, "COMMERCE_SMTP_HOST") },
		"host with port":     func(e map[string]string, dir string) { e["COMMERCE_SMTP_HOST"] = "smtp.mail.example:465" },
		"host with scheme":   func(e map[string]string, dir string) { e["COMMERCE_SMTP_HOST"] = "smtps://smtp.mail.example" },
		"host single label":  func(e map[string]string, dir string) { e["COMMERCE_SMTP_HOST"] = "mail" },
		"host localhost":     func(e map[string]string, dir string) { e["COMMERCE_SMTP_HOST"] = "localhost" },
		"host loopback ip":   func(e map[string]string, dir string) { e["COMMERCE_SMTP_HOST"] = "127.0.0.1" },
		"host public ip":     func(e map[string]string, dir string) { e["COMMERCE_SMTP_HOST"] = "203.0.113.5" },
		"username unset":     func(e map[string]string, dir string) { delete(e, "COMMERCE_SMTP_USERNAME") },
		"from unset":         func(e map[string]string, dir string) { delete(e, "COMMERCE_MAIL_FROM") },
		"from differs":       func(e map[string]string, dir string) { e["COMMERCE_MAIL_FROM"] = "xgdwm <other@mail.example>" },
		"cap below range":    func(e map[string]string, dir string) { e["COMMERCE_MAIL_DAILY_CAP"] = "19" },
		"cap above range":    func(e map[string]string, dir string) { e["COMMERCE_MAIL_DAILY_CAP"] = "100001" },
		"cap not a number":   func(e map[string]string, dir string) { e["COMMERCE_MAIL_DAILY_CAP"] = "many" },
		"breach unknown":     func(e map[string]string, dir string) { e["COMMERCE_BREACH_CHECK"] = "maybe" },
		"breach off in prod": func(e map[string]string, dir string) { e["COMMERCE_BREACH_CHECK"] = "off" },
		"hibp url in prod":   func(e map[string]string, dir string) { e["COMMERCE_HIBP_BASE_URL"] = "http://127.0.0.1:9" },
		"flag not boolean":   func(e map[string]string, dir string) { e["COMMERCE_PASSWORD_LOGIN_ENABLED"] = "true" },
		"fixture mode":       func(e map[string]string, dir string) { e["COMMERCE_FIXTURE_ENABLED"] = "1" },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			env := passwordEnv(t)
			mutate(env, t.TempDir())
			_, err := loadIdentityConfig(func(k string) string { return env[k] })
			if err == nil {
				t.Fatal("invalid password configuration accepted")
			}
			if strings.Contains(err.Error(), canarySMTPSecret) {
				t.Fatal("error message leaks a secret")
			}
		})
	}
}

func writeFile(t *testing.T, dir, name, content string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// In containers lcentry turns COMMERCE_X_FILE into COMMERCE_X=<contents>; the config must accept that form.
func TestIdentityPasswordSecretsAcceptLcentryExpandedValues(t *testing.T) {
	env := passwordEnv(t)
	pepper, _ := os.ReadFile(env["COMMERCE_AUTH_PEPPER_FILE"])
	delete(env, "COMMERCE_AUTH_PEPPER_FILE")
	delete(env, "COMMERCE_SMTP_PASSWORD_FILE")
	env["COMMERCE_AUTH_PEPPER"] = strings.TrimSpace(string(pepper))
	env["COMMERCE_SMTP_PASSWORD"] = canarySMTPSecret
	c, err := loadIdentityConfig(func(k string) string { return env[k] })
	if err != nil || len(c.passwords.Pepper) != 32 || c.mailer == nil {
		t.Fatalf("expanded-value form rejected: %v", err)
	}
	both := passwordEnv(t)
	both["COMMERCE_AUTH_PEPPER"] = env["COMMERCE_AUTH_PEPPER"]
	if _, err := loadIdentityConfig(func(k string) string { return both[k] }); err == nil {
		t.Fatal("NAME and NAME_FILE both set must be rejected")
	}
	bad := passwordEnv(t)
	delete(bad, "COMMERCE_AUTH_PEPPER_FILE")
	bad["COMMERCE_AUTH_PEPPER"] = "not-a-valid-pepper"
	if _, err := loadIdentityConfig(func(k string) string { return bad[k] }); err == nil {
		t.Fatal("malformed expanded pepper accepted")
	}
}

func TestIdentityPasswordLoopbackOverridesOnlyWithTestFlag(t *testing.T) {
	env := passwordEnv(t)
	env["COMMERCE_IDENTITY_ALLOW_LOOPBACK_TESTS"] = "1"
	env["COMMERCE_SMTP_HOST"] = "127.0.0.1"
	env["COMMERCE_BREACH_CHECK"] = "off"
	env["COMMERCE_HIBP_BASE_URL"] = "http://127.0.0.1:9"
	env["COMMERCE_MAIL_DAILY_CAP"] = "20"
	c, err := loadIdentityConfig(func(k string) string { return env[k] })
	if err != nil || c.passwords.BreachCheck != "off" || c.passwords.MailDailyCap != 20 || !c.passwords.AllowLoopback {
		t.Fatalf("loopback test configuration rejected: %v", err)
	}
	// Still no public non-DNS host even with the flag.
	env["COMMERCE_SMTP_HOST"] = "203.0.113.5"
	if _, err := loadIdentityConfig(func(k string) string { return env[k] }); err == nil {
		t.Fatal("public IP literal accepted as SMTP host")
	}
}

func TestIdentityMuxRoutesPasswordSeparatelyFromOIDC(t *testing.T) {
	label := func(name string) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(name)) })
	}
	h := identityMux(label("oidc"), label("password"))
	for path, want := range map[string]string{
		"/v1/identity/password/signup":   "password",
		"/v1/identity/password/complete": "password",
		"/v1/identity/login/start":       "oidc",
		"/v1/identity/login/complete":    "oidc",
		"/v1/identity/initial-store":     "oidc",
		"/v1/identity/logout":            "oidc",
	} {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest(http.MethodPost, path, nil))
		if w.Body.String() != want {
			t.Fatalf("%s served by %q, want %q", path, w.Body.String(), want)
		}
	}
}
