// platform_billing_test.go covers the platform billing wiring that needs no database: disabled billing
// reads nothing and opens nothing, invalid configuration is refused without echoing a value, and
// mountPlatformBilling routes exactly the one webhook path. Names avoid the TestCustomersBillingCB prefix.

package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"livecommerce/internal/billing"
)

func TestBuildPlatformBillingDisabledReadsAndOpensNothing(t *testing.T) {
	read := map[string]bool{}
	svc, webhook, closeFn, err := buildPlatformBilling(context.Background(), nil, func(name string) string {
		read[name] = true
		return ""
	})
	if err != nil || svc != nil || webhook != nil || closeFn == nil {
		t.Fatalf("svc=%v webhook=%v closeFn nil=%v err=%v", svc, webhook, closeFn == nil, err)
	}
	closeFn() // a disabled build still hands back a callable no-op
	if len(read) != 1 || !read["LC_BILLING_ENABLED"] {
		t.Fatalf("disabled billing read %v, want only LC_BILLING_ENABLED", read)
	}
	var nilService *billing.Service
	if nilService.Enabled() { // the value main.go stores in Options.Billing
		t.Fatal("nil service reports enabled")
	}
}

func TestBuildPlatformBillingRefusesBadConfigurationWithoutEchoingValues(t *testing.T) {
	secret := "sk_" + "live_" + strings.Repeat("Z", 24)
	good := map[string]string{
		"LC_BILLING_ENABLED":                "1",
		"LC_PLATFORM_STRIPE_SECRET_KEY":     "sk_" + "test_" + strings.Repeat("A", 24),
		"LC_PLATFORM_STRIPE_WEBHOOK_SECRET": "whsec" + "_" + strings.Repeat("B", 32),
		"LC_BILLING_PRICE_IDS":              "price_1Abc",
		"LC_BILLING_RETURN_ORIGIN":          "https://admin.example.test",
	}
	with := func(name, value string) func(string) string {
		m := map[string]string{}
		for k, v := range good {
			m[k] = v
		}
		m[name] = value
		return func(n string) string { return m[n] }
	}
	for name, getenv := range map[string]func(string) string{
		"live key":             with("LC_PLATFORM_STRIPE_SECRET_KEY", secret),
		"flag typo":            with("LC_BILLING_ENABLED", "yes"),
		"missing ingress DSN":  with("COMMERCE_STRIPE_INGRESS_DATABASE_URL", ""), // valid config, no DSN, no pool
		"blank webhook secret": with("LC_PLATFORM_STRIPE_WEBHOOK_SECRET", ""),
	} {
		svc, webhook, closeFn, err := buildPlatformBilling(context.Background(), nil, getenv)
		if err == nil || svc != nil || webhook != nil || closeFn == nil {
			t.Errorf("%s: svc=%v webhook=%v err=%v", name, svc, webhook, err)
			continue
		}
		if text := fmt.Sprint(err); strings.Contains(text, secret) || strings.Contains(text, "Zzzz") {
			t.Errorf("%s: error echoes a value: %q", name, text)
		}
		closeFn()
	}
	if _, _, _, err := buildPlatformBilling(context.Background(), nil, with("LC_BILLING_ENABLED", "yes")); !errors.Is(err, billing.ErrConfig) {
		t.Fatalf("flag typo err=%v, want billing.ErrConfig", err)
	}
}

func TestMountPlatformBillingRoutesExactlyTheWebhookPath(t *testing.T) {
	hit := func(name string) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(name)) })
	}
	fallback := hit("fallback")
	if got := mountPlatformBilling(fallback, nil); fmt.Sprintf("%p", got) != fmt.Sprintf("%p", fallback) {
		// http.HandlerFunc values compare by func pointer through %p; a nil webhook must return fallback itself.
		t.Fatal("nil webhook must return the fallback unchanged")
	}
	h := mountPlatformBilling(fallback, hit("webhook"))
	for path, want := range map[string]string{
		"/v1/platform/stripe/webhook":           "webhook",
		"/v1/platform/stripe/webhook/":          "fallback",
		"/v1/platform/stripe":                   "fallback",
		"/v1/platform/stripe/webhook/extra":     "fallback",
		"//v1/platform/stripe/webhook":          "fallback",
		"/v1/platform/stripe/../stripe/webhook": "fallback",
		"/v1/stripe/webhook/x":                  "fallback",
		"/v1/admin/stores":                      "fallback",
	} {
		req := httptest.NewRequest(http.MethodPost, "http://api.example.test/", nil)
		req.URL.Path = path
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Body.String() != want {
			t.Errorf("%s -> %q, want %q", path, rec.Body.String(), want)
		}
	}
}
