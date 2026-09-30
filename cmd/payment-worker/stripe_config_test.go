// stripe_config_test.go: COMMERCE_STRIPE_ENABLED parsing for the payment worker (MOCK tier).
// Non-goal: NewStripeRuntime against PG (SP15, blocked on pool-fix) and any provider call.
package main

import (
	"errors"
	"strings"
	"testing"
)

func TestStripeFlagOnlyReadWhenWorkerEnabled(t *testing.T) {
	get := func(name string) string {
		if name != "COMMERCE_PAYMENT_WORKER_ENABLED" {
			t.Fatalf("disabled worker read %s", name)
		}
		return ""
	}
	if c, err := loadConfig(get); err != nil || c.enabled || c.stripe {
		t.Fatal("disabled worker changed")
	}
}

func TestStripeFlagStrictAndNoStripeSecretsRead(t *testing.T) {
	values := testEnvironment()
	read := []string{}
	get := func(name string) string { read = append(read, name); return values[name] }
	c, err := loadConfig(get)
	if err != nil || c.stripe {
		t.Fatal("stripe must default off")
	}
	for _, off := range []string{"", "0"} {
		values["COMMERCE_STRIPE_ENABLED"] = off
		if c, err = loadConfig(get); err != nil || c.stripe {
			t.Fatalf("flag %q not off", off)
		}
	}
	values["COMMERCE_STRIPE_ENABLED"] = "1"
	read = read[:0]
	if c, err = loadConfig(get); err != nil || !c.stripe || c.profile != "SANDBOX" {
		t.Fatal("valid Stripe worker rejected")
	}
	for _, name := range read {
		if strings.HasPrefix(name, "STRIPE_") || strings.Contains(name, "WHSEC") || strings.Contains(name, "WEBHOOK") {
			t.Fatalf("worker read forbidden variable %s", name)
		}
	}
	for _, bad := range []string{"true", "2", " 1", "01"} {
		values["COMMERCE_STRIPE_ENABLED"] = bad
		if _, err = loadConfig(get); !errors.Is(err, errWorkerConfig) || strings.Contains(err.Error(), bad) {
			t.Fatalf("flag %q accepted or leaked", bad)
		}
	}
	values["COMMERCE_STRIPE_ENABLED"] = "1"
	values["COMMERCE_PAYMENT_WORKER_PROFILE"] = "LIVE"
	if _, err = loadConfig(get); !errors.Is(err, errWorkerConfig) {
		t.Fatal("Stripe with LIVE profile accepted")
	}
}
