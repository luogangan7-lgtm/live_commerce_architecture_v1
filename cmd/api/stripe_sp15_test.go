package main

// SP15 no-PG half for cmd/api (contracts/stripe-psp-v1.md §12, §0.2, rulings 5-6 in
// docs/delivery/units/stripe-b1-rulings.md). Written from the contract and the
// FROZEN seams of stripe-b1-ingress-assembly / stripe-b1-start-http; it never
// reads those units' implementation. The PG half is TestStripeSP15Process in
// tests/foundation/stripe_process_test.go.

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sort"
	"strings"
	"testing"
)

// Synthetic DSN sentinels live in their own constants so no source line looks like a
// credential to secret scanners (GitGuardian false positives 2026-09-29); they are test sentinels.
const (
	dsnSentinel1 = "sentinel-pass"
)

func sp15WebhookEnv() map[string]string {
	key := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{5}, 32))
	replay := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{6}, 32))
	return map[string]string{
		"COMMERCE_STRIPE_WEBHOOK_ENABLED":       "1",
		"COMMERCE_STRIPE_INGRESS_DATABASE_URL":  "postgres://sentinel-user:" + dsnSentinel1 + "@127.0.0.1:1/stripe_ingress",
		"COMMERCE_PAYMENT_PROFILE":              "PROVIDER_MOCK",
		"COMMERCE_STRIPE_WEBHOOK_ACTIVE_KEY_ID": "signing-1",
		"COMMERCE_STRIPE_WEBHOOK_KEYS_JSON":     `[{"id":"signing-1","key_base64":"` + key + `"}]`,
		"COMMERCE_STRIPE_WEBHOOK_REPLAY_KEY":    replay,
	}
}

// sp15Recorder returns a getenv over values that records every name asked for.
func sp15Recorder(values map[string]string, read *[]string) func(string) string {
	return func(name string) string {
		*read = append(*read, name)
		return values[name]
	}
}

func sp15AssertNoName(t *testing.T, read []string, what string, forbidden func(string) bool) {
	t.Helper()
	for _, name := range read {
		if forbidden(name) {
			t.Fatalf("%s read %q", what, name)
		}
	}
}

func TestStripeSP15Process(t *testing.T) {
	t.Run("frozen_error_codes", func(t *testing.T) {
		if errStripeConfig.Error() != "stripe_api_invalid_config" || errStripeDatabase.Error() != "stripe_api_database_unavailable" {
			t.Fatalf("fixed codes drifted: %q %q", errStripeConfig, errStripeDatabase)
		}
	})

	t.Run("disabled_webhook_reads_only_its_flag_and_opens_nothing", func(t *testing.T) {
		for _, flagValue := range []string{"", "0"} {
			var read []string
			values := sp15WebhookEnv()
			values["COMMERCE_STRIPE_WEBHOOK_ENABLED"] = flagValue
			c, err := loadStripeWebhookConfig(sp15Recorder(values, &read), "0.0.0.0:8080") // public address is irrelevant when disabled
			if err != nil || len(read) != 1 || read[0] != "COMMERCE_STRIPE_WEBHOOK_ENABLED" {
				t.Fatalf("flag %q: err=%v read=%v", flagValue, err, read)
			}
			h, closePools, err := buildStripeWebhookHandler(context.Background(), nil, c)
			if err != nil || h != nil {
				t.Fatalf("disabled webhook built a handler: %v", err)
			}
			if closePools != nil {
				closePools()
			}
		}
	})

	t.Run("enabled_webhook_needs_a_private_listener_before_reading_secrets", func(t *testing.T) {
		for _, addr := range []string{"0.0.0.0:8080", "localhost:8080", ":8080", "127.0.0.1:0", "10.0.0.5:8080", "[::]:8080", "example.com:8080"} {
			var read []string
			values := sp15WebhookEnv()
			if _, err := loadStripeWebhookConfig(sp15Recorder(values, &read), addr); !errors.Is(err, errStripeConfig) {
				t.Fatalf("%s accepted: %v", addr, err)
			}
			if len(read) != 1 {
				t.Fatalf("%s: a public listener read %v", addr, read)
			}
		}
		for _, enabled := range []string{"true", "2", " 1", "1 ", "yes"} {
			values := sp15WebhookEnv()
			values["COMMERCE_STRIPE_WEBHOOK_ENABLED"] = enabled
			if _, err := loadStripeWebhookConfig(func(n string) string { return values[n] }, "127.0.0.1:8080"); !errors.Is(err, errStripeConfig) {
				t.Fatalf("noncanonical flag %q accepted", enabled)
			}
		}
	})

	t.Run("webhook_config_every_negative_and_no_leak", func(t *testing.T) {
		get := func(v map[string]string) func(string) string { return func(n string) string { return v[n] } }
		for _, addr := range []string{"127.0.0.1:8080", "[::1]:8080"} {
			if _, err := loadStripeWebhookConfig(get(sp15WebhookEnv()), addr); err != nil {
				t.Fatalf("valid private config rejected on %s: %v", addr, err)
			}
		}
		for name, mutate := range map[string]func(map[string]string){
			"dsn_missing":       func(v map[string]string) { delete(v, "COMMERCE_STRIPE_INGRESS_DATABASE_URL") },
			"dsn_blank":         func(v map[string]string) { v["COMMERCE_STRIPE_INGRESS_DATABASE_URL"] = "  " },
			"dsn_too_long":      func(v map[string]string) { v["COMMERCE_STRIPE_INGRESS_DATABASE_URL"] = strings.Repeat("x", 8193) },
			"profile_missing":   func(v map[string]string) { delete(v, "COMMERCE_PAYMENT_PROFILE") },
			"profile_live":      func(v map[string]string) { v["COMMERCE_PAYMENT_PROFILE"] = "LIVE" },
			"profile_unknown":   func(v map[string]string) { v["COMMERCE_PAYMENT_PROFILE"] = "PRODUCTION" },
			"profile_lowercase": func(v map[string]string) { v["COMMERCE_PAYMENT_PROFILE"] = "sandbox" },
			"keyring_json":      func(v map[string]string) { v["COMMERCE_STRIPE_WEBHOOK_KEYS_JSON"] = "not-json-sentinel" },
			"keyring_active":    func(v map[string]string) { v["COMMERCE_STRIPE_WEBHOOK_ACTIVE_KEY_ID"] = "absent" },
			"keyring_replay":    func(v map[string]string) { v["COMMERCE_STRIPE_WEBHOOK_REPLAY_KEY"] = "short" },
			"keyring_missing":   func(v map[string]string) { delete(v, "COMMERCE_STRIPE_WEBHOOK_KEYS_JSON") },
			// The API-key custody keyring must NOT stand in for the signing keyring.
			"only_account_keyring": func(v map[string]string) {
				a := accountTestEnv()
				for _, k := range []string{"COMMERCE_STRIPE_WEBHOOK_ACTIVE_KEY_ID", "COMMERCE_STRIPE_WEBHOOK_KEYS_JSON", "COMMERCE_STRIPE_WEBHOOK_REPLAY_KEY"} {
					delete(v, k)
				}
				v["COMMERCE_ACCOUNT_ACTIVE_KEY_ID"], v["COMMERCE_ACCOUNT_KEYS_JSON"], v["COMMERCE_ACCOUNT_REPLAY_KEY"] = a["COMMERCE_ACCOUNT_ACTIVE_KEY_ID"], a["COMMERCE_ACCOUNT_KEYS_JSON"], a["COMMERCE_ACCOUNT_REPLAY_KEY"]
			},
		} {
			values := sp15WebhookEnv()
			mutate(values)
			_, err := loadStripeWebhookConfig(get(values), "127.0.0.1:8080")
			if !errors.Is(err, errStripeConfig) {
				t.Fatalf("%s accepted: %v", name, err)
			}
			for _, secret := range []string{"sentinel-pass", "sentinel-user", "not-json-sentinel", values["COMMERCE_STRIPE_WEBHOOK_KEYS_JSON"], values["COMMERCE_STRIPE_WEBHOOK_REPLAY_KEY"]} {
				if secret != "" && strings.Contains(err.Error(), secret) {
					t.Fatalf("%s leaked input in its error", name)
				}
			}
		}
	})

	t.Run("api_never_reads_stripe_secrets_and_webhook_reads_only_its_own_names", func(t *testing.T) {
		var read []string
		values := sp15WebhookEnv()
		// Global Stripe credentials that must never be consulted (contract §0.2, ruling 5).
		values["STRIPE_SECRET_KEY"], values["STRIPE_ACCOUNT_ID"] = "sk_"+"test_sentinel0123456789abcdef", "acct_SentinelAcct1"
		values["STRIPE_WEBHOOK_SECRET"], values["STRIPE_WEBHOOK_SECRET_NEXT"] = "whsec_"+"sentinel0123456789abcdef", "whsec_"+"sentinel_next_0123456789"
		c, err := loadStripeWebhookConfig(sp15Recorder(values, &read), "127.0.0.1:8080")
		if err != nil {
			t.Fatal(err)
		}
		allowed := map[string]bool{"COMMERCE_STRIPE_WEBHOOK_ENABLED": true, "COMMERCE_STRIPE_INGRESS_DATABASE_URL": true, "COMMERCE_PAYMENT_PROFILE": true,
			"COMMERCE_STRIPE_WEBHOOK_ACTIVE_KEY_ID": true, "COMMERCE_STRIPE_WEBHOOK_KEYS_JSON": true, "COMMERCE_STRIPE_WEBHOOK_REPLAY_KEY": true}
		sort.Strings(read)
		for _, name := range read {
			if !allowed[name] {
				t.Fatalf("webhook config read %q (allowed: %v)", name, allowed)
			}
		}
		sp15AssertNoName(t, read, "webhook config", func(n string) bool {
			return strings.HasPrefix(n, "STRIPE_") || strings.HasPrefix(n, "COMMERCE_ACCOUNT_")
		})
		// Redaction under every formatting path and JSON.
		encoded, _ := json.Marshal(c)
		for _, rendered := range []string{fmt.Sprint(c), fmt.Sprintf("%+v", c), fmt.Sprintf("%#v", c), string(encoded)} {
			for _, secret := range []string{"sentinel-pass", "sentinel-user", values["COMMERCE_STRIPE_WEBHOOK_KEYS_JSON"], values["COMMERCE_STRIPE_WEBHOOK_REPLAY_KEY"]} {
				if strings.Contains(rendered, secret) {
					t.Fatal("webhook config formatting leaked a secret")
				}
			}
		}
		// Building against an unreachable database reports only the fixed code, never the DSN.
		_, closePools, err := buildStripeWebhookHandler(context.Background(), nil, c)
		if !errors.Is(err, errStripeDatabase) && !errors.Is(err, errStripeConfig) {
			t.Fatalf("build with unreachable ingress database: %v", err)
		}
		if err != nil && (strings.Contains(err.Error(), "sentinel") || strings.Contains(err.Error(), "postgres://")) {
			t.Fatal("build error leaked the DSN")
		}
		if closePools != nil {
			closePools()
		}
	})

	t.Run("buyer_payment_stripe_flag_is_nested_and_never_reads_stripe_secrets", func(t *testing.T) {
		// Disabled buyer payment: only its own flag is read, even with the Stripe flag set.
		values := buyerPaymentTestEnv()
		values["COMMERCE_BUYER_PAYMENT_ENABLED"] = "0"
		values["COMMERCE_STRIPE_CHECKOUT_ENABLED"] = "1"
		var read []string
		if _, err := loadBuyerConfig(sp15Recorder(values, &read), "127.0.0.1:8080"); err != nil {
			t.Fatal(err)
		}
		sp15AssertNoName(t, read, "disabled buyer payment", func(n string) bool { return strings.Contains(n, "STRIPE") })

		for _, flagValue := range []string{"", "0"} {
			values = buyerPaymentTestEnv()
			values["COMMERCE_STRIPE_CHECKOUT_ENABLED"] = flagValue
			read = nil
			if _, err := loadBuyerConfig(sp15Recorder(values, &read), "127.0.0.1:8080"); err != nil {
				t.Fatalf("stripe flag %q: %v", flagValue, err)
			}
			for _, n := range read {
				if strings.Contains(n, "STRIPE") && n != "COMMERCE_STRIPE_CHECKOUT_ENABLED" {
					t.Fatalf("Stripe checkout disabled but %q was read", n)
				}
			}
		}
		for _, profile := range []string{"PROVIDER_MOCK", "SANDBOX"} {
			values = buyerPaymentTestEnv()
			values["COMMERCE_STRIPE_CHECKOUT_ENABLED"], values["COMMERCE_PAYMENT_PROFILE"] = "1", profile
			values["STRIPE_SECRET_KEY"], values["STRIPE_WEBHOOK_SECRET"] = "sk_"+"test_sentinel0123456789abcdef", "whsec_"+"sentinel0123456789abcdef"
			read = nil
			if _, err := loadBuyerConfig(sp15Recorder(values, &read), "127.0.0.1:8080"); err != nil {
				t.Fatalf("stripe checkout on %s: %v", profile, err)
			}
			sp15AssertNoName(t, read, "stripe checkout config", func(n string) bool {
				return strings.HasPrefix(n, "STRIPE_") || strings.HasPrefix(n, "COMMERCE_STRIPE_WEBHOOK_") || strings.HasPrefix(n, "COMMERCE_STRIPE_INGRESS_")
			})
		}
		for name, mutate := range map[string]func(map[string]string){
			"live_profile":         func(v map[string]string) { v["COMMERCE_PAYMENT_PROFILE"] = "LIVE" },
			"flag_true":            func(v map[string]string) { v["COMMERCE_STRIPE_CHECKOUT_ENABLED"] = "true" },
			"flag_space":           func(v map[string]string) { v["COMMERCE_STRIPE_CHECKOUT_ENABLED"] = " 1" },
			"return_url_http":      func(v map[string]string) { v["COMMERCE_PAYMENT_RETURN_URL"] = "http://pay.example.com/return" },
			"return_url_query":     func(v map[string]string) { v["COMMERCE_PAYMENT_RETURN_URL"] = "https://pay.example.com/return?x=1" },
			"payuni_notify_absent": func(v map[string]string) { v["COMMERCE_PAYMENT_NOTIFY_URL"] = "" }, // no Stripe-only deployment in B1
		} {
			values = buyerPaymentTestEnv()
			values["COMMERCE_STRIPE_CHECKOUT_ENABLED"], values["COMMERCE_PAYMENT_PROFILE"] = "1", "PROVIDER_MOCK"
			mutate(values)
			if _, err := loadBuyerConfig(func(n string) string { return values[n] }, "127.0.0.1:8080"); !errors.Is(err, errBuyerConfig) || err.Error() != "invalid buyer configuration" {
				t.Fatalf("%s accepted or leaked: %v", name, err)
			}
		}
	})

	t.Run("mountStripe_reserves_the_whole_namespace", func(t *testing.T) {
		fallback := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte("fallback")) })
		stripe := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte("stripe")) })
		h := mountStripe(fallback, stripe)
		serve := func(path string) string {
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, &http.Request{Method: http.MethodPost, URL: &url.URL{Path: path}, Header: http.Header{}})
			return rec.Body.String()
		}
		id := "3f2b8c1e-0d4a-4b6f-9a7e-5c1d2e3f4a5b"
		for _, path := range []string{"/v1/stripe", "/v1/stripe/", "/v1/stripe/webhook", "/v1/stripe/webhook/" + id, "/v1/stripe//webhook/" + id,
			"/v1/stripe/../stripe/webhook/" + id, "/v1//stripe/webhook/" + id, "/v1/./stripe/webhook/" + id, "/v1/other/../stripe/webhook/" + id} {
			if got := serve(path); got != "stripe" {
				t.Fatalf("%s reached %q, not the Stripe handler (alias bypass)", path, got)
			}
		}
		for _, path := range []string{"/", "/healthz", "/v1/stores", "/v1/meta/webhook/x", "/v1/strip"} {
			if got := serve(path); got != "fallback" {
				t.Fatalf("%s reached %q, not the fallback", path, got)
			}
		}
		if got := mountStripe(fallback, nil); got == nil {
			t.Fatal("a disabled Stripe mount must keep serving the fallback")
		}
	})
}
