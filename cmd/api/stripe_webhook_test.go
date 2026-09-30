// stripe_webhook_test.go: MOCK-tier unit tests for the API's Stripe webhook config, redaction and
// mount seams. Non-goal: pool admission and SQL (REAL_PG gates SP13/SP15, blocked on pool-fix).
package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func stripeTestEnv() map[string]string {
	key := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{4}, 32))
	replay := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{5}, 32))
	return map[string]string{
		"COMMERCE_STRIPE_WEBHOOK_ENABLED":       "1",
		"COMMERCE_STRIPE_INGRESS_DATABASE_URL":  "postgres://synthetic-secret@synthetic.invalid/stripe",
		"COMMERCE_PAYMENT_PROFILE":              "SANDBOX",
		"COMMERCE_STRIPE_WEBHOOK_ACTIVE_KEY_ID": "sig-1",
		"COMMERCE_STRIPE_WEBHOOK_KEYS_JSON":     `[{"id":"sig-1","key_base64":"` + key + `"}]`,
		"COMMERCE_STRIPE_WEBHOOK_REPLAY_KEY":    replay,
	}
}

func TestStripeWebhookDisabledReadsOnlyFlag(t *testing.T) {
	for _, value := range []string{"", "0"} {
		c, err := loadStripeWebhookConfig(func(name string) string {
			if name != "COMMERCE_STRIPE_WEBHOOK_ENABLED" {
				t.Fatalf("disabled Stripe webhook read %s", name)
			}
			return value
		}, "0.0.0.0:8080")
		if err != nil || c.enabled {
			t.Fatal("disabled config changed")
		}
		h, closePools, err := buildStripeWebhookHandler(context.Background(), nil, c)
		if err != nil || h != nil || closePools == nil {
			t.Fatal("disabled webhook opened authority")
		}
		closePools()
	}
	for _, bad := range []string{"true", "2", " 1"} {
		if _, err := loadStripeWebhookConfig(func(string) string { return bad }, "127.0.0.1:8080"); !errors.Is(err, errStripeConfig) {
			t.Fatalf("noncanonical flag %q accepted", bad)
		}
	}
}

func TestStripeWebhookConfigStrictAndRedacted(t *testing.T) {
	values := stripeTestEnv()
	read := []string{}
	get := func(name string) string { read = append(read, name); return values[name] }
	for _, addr := range []string{"0.0.0.0:8080", "localhost:8080", ":8080", "127.0.0.1:0"} {
		if _, err := loadStripeWebhookConfig(get, addr); !errors.Is(err, errStripeConfig) {
			t.Fatalf("listener %s accepted", addr)
		}
	}
	for _, addr := range []string{"127.0.0.1:8080", "[::1]:8080"} {
		read = read[:0]
		c, err := loadStripeWebhookConfig(get, addr)
		if err != nil || !c.enabled || c.keys == nil || c.profile != "SANDBOX" {
			t.Fatal("valid private configuration rejected")
		}
		for _, name := range read {
			if strings.HasPrefix(name, "STRIPE_") || strings.HasPrefix(name, "COMMERCE_ACCOUNT_") {
				t.Fatalf("API read forbidden variable %s", name)
			}
		}
		for _, rendered := range []string{fmt.Sprint(c), fmt.Sprintf("%+v", c), fmt.Sprintf("%#v", c)} {
			if strings.Contains(rendered, "synthetic-secret") || !strings.Contains(rendered, "redacted") {
				t.Fatal("config formatting leaked")
			}
		}
		encoded, err := json.Marshal(c)
		if err != nil || strings.Contains(string(encoded), "synthetic-secret") {
			t.Fatal("config JSON leaked")
		}
	}
	for _, tc := range []struct{ name, value string }{
		{"COMMERCE_STRIPE_INGRESS_DATABASE_URL", ""}, {"COMMERCE_STRIPE_INGRESS_DATABASE_URL", " "},
		{"COMMERCE_STRIPE_INGRESS_DATABASE_URL", strings.Repeat("x", 8193)},
		{"COMMERCE_PAYMENT_PROFILE", "LIVE"}, {"COMMERCE_PAYMENT_PROFILE", ""}, {"COMMERCE_PAYMENT_PROFILE", "sandbox"},
		{"COMMERCE_STRIPE_WEBHOOK_KEYS_JSON", "not-json"}, {"COMMERCE_STRIPE_WEBHOOK_REPLAY_KEY", ""},
	} {
		v := stripeTestEnv()
		v[tc.name] = tc.value
		_, err := loadStripeWebhookConfig(func(n string) string { return v[n] }, "127.0.0.1:8080")
		if !errors.Is(err, errStripeConfig) || (tc.value != "" && strings.Contains(err.Error(), tc.value)) {
			t.Fatalf("%s=%q accepted or leaked: %v", tc.name, tc.value, err)
		}
	}
	// The payment API-key keyring names must not satisfy the signing keyring.
	acct := stripeTestEnv()
	for k, v := range acct {
		if strings.HasPrefix(k, "COMMERCE_STRIPE_WEBHOOK_") && k != "COMMERCE_STRIPE_WEBHOOK_ENABLED" {
			acct["COMMERCE_ACCOUNT_"+strings.TrimPrefix(k, "COMMERCE_STRIPE_WEBHOOK_")] = v
			delete(acct, k)
		}
	}
	if _, err := loadStripeWebhookConfig(func(n string) string { return acct[n] }, "127.0.0.1:8080"); !errors.Is(err, errStripeConfig) {
		t.Fatal("COMMERCE_ACCOUNT_* keyring accepted as webhook signing custody")
	}
	if _, closePools, err := buildStripeWebhookHandler(context.Background(), nil, stripeWebhookConfig{enabled: true}); !errors.Is(err, errStripeConfig) || closePools != nil {
		t.Fatal("enabled build without pool/keys accepted")
	}
}

func TestMountStripeReservesNamespace(t *testing.T) {
	hits := ""
	fallback := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hits += "F" })
	stripe := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hits += "S" })
	mountStripe(fallback, nil).ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("POST", "/v1/stripe/webhook/x", nil))
	if hits != "F" {
		t.Fatal("disabled mount did not fall through")
	}
	hits = ""
	h := mountStripe(fallback, stripe)
	for _, p := range []string{"/v1/stripe/webhook/7a1d2c3b-4e5f-4a6b-8c7d-9e0f1a2b3c4d", "/v1/stripe",
		"/v1//stripe/webhook/x", "/v1/x/../stripe/webhook/x", "/v1/stripex"} {
		req := httptest.NewRequest("POST", "/", nil)
		req.URL.Path = p
		h.ServeHTTP(httptest.NewRecorder(), req)
	}
	if hits != "SSSSS" {
		t.Fatalf("namespace leaked to fallback: %s", hits)
	}
	hits = ""
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/v1/meta/x", nil))
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/v1/streams", nil))
	if hits != "FF" {
		t.Fatalf("unrelated routes captured: %s", hits)
	}
}
