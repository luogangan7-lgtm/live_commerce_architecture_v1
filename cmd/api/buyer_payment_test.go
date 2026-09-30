package main

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func buyerPaymentTestEnv() map[string]string {
	values := accountTestEnv()
	values["COMMERCE_ACCOUNTS_ENABLED"] = "0" // hosted signing does not enable merchant account HTTP
	values["COMMERCE_BUYER_ENABLED"] = "1"
	values["COMMERCE_BUYER_BFF_KEY"] = strings.Repeat("A", 43)
	values["COMMERCE_BUYER_SESSION_TTL"] = "1h"
	values["COMMERCE_BUYER_DATABASE_URL"] = "postgres://fixture@127.0.0.1:1/buyer"
	values["COMMERCE_BUYER_ISSUER_DATABASE_URL"] = "postgres://fixture@127.0.0.1:1/issuer"
	values["COMMERCE_CHECKOUT_DATABASE_URL"] = "postgres://fixture@127.0.0.1:1/checkout"
	values["COMMERCE_BUYER_PAYMENT_ENABLED"] = "1"
	values["COMMERCE_HOSTED_DATABASE_URL"] = "postgres://fixture@127.0.0.1:1/hosted"
	values["COMMERCE_PAYMENT_PROFILE"] = "PROVIDER_MOCK"
	values["COMMERCE_PAYMENT_RETURN_URL"] = "https://PAY.example.com:443/return"
	values["COMMERCE_PAYMENT_NOTIFY_URL"] = "https://pay.example.com/notify"
	return values
}

func TestBuyerPaymentNestedFlagsReadOnlyWhenEnabled(t *testing.T) {
	values := buyerPaymentTestEnv()
	values["COMMERCE_BUYER_ENABLED"] = "0"
	reads := []string{}
	root, err := loadBuyerConfig(func(name string) string {
		reads = append(reads, name)
		return values[name]
	}, "0.0.0.0:8080")
	if err != nil || root.enabled || len(reads) != 1 || reads[0] != "COMMERCE_BUYER_ENABLED" {
		t.Fatalf("disabled root read nested configuration: %v, %v", reads, err)
	}
	values["COMMERCE_BUYER_ENABLED"] = "1"
	values["COMMERCE_BUYER_PAYMENT_ENABLED"] = "0"
	reads = nil
	root, err = loadBuyerConfig(func(name string) string {
		reads = append(reads, name)
		return values[name]
	}, "127.0.0.1:8080")
	if err != nil || !root.enabled || root.payment.enabled {
		t.Fatalf("disabled payment changed buyer configuration: %v", err)
	}
	for _, name := range reads {
		if strings.HasPrefix(name, "COMMERCE_ACCOUNT_") || strings.HasPrefix(name, "COMMERCE_PAYMENT_") || name == "COMMERCE_HOSTED_DATABASE_URL" {
			t.Fatalf("disabled payment read secret or hosted setting: %s", name)
		}
	}
	if service, pool, err := buildBuyerPayment(context.Background(), root.payment); err != nil || service != nil || pool != nil {
		t.Fatalf("disabled payment opened authority: %v", err)
	}
}

func TestBuyerPaymentConfigUsesHistoricalKeyParserWithoutMerchantHTTP(t *testing.T) {
	values := buyerPaymentTestEnv()
	get := func(name string) string { return values[name] }
	root, err := loadBuyerConfig(get, "127.0.0.1:8080")
	if err != nil || !root.payment.enabled || root.payment.keys == nil ||
		root.payment.endpoints.ReturnURL != "https://pay.example.com/return" ||
		values["COMMERCE_ACCOUNTS_ENABLED"] != "0" {
		t.Fatalf("valid isolated hosted configuration rejected: %v", err)
	}
	for name, bad := range map[string]string{
		"COMMERCE_BUYER_PAYMENT_ENABLED": "true",
		"COMMERCE_HOSTED_DATABASE_URL":   " ",
		"COMMERCE_PAYMENT_PROFILE":       "LIVE ",
		"COMMERCE_PAYMENT_RETURN_URL":    "http://pay.example.com/return",
		"COMMERCE_PAYMENT_NOTIFY_URL":    "https://pay.example.com/notify?buyer=1",
		"COMMERCE_ACCOUNT_KEYS_JSON":     "[]",
	} {
		original := values[name]
		values[name] = bad
		if _, err := loadBuyerConfig(get, "127.0.0.1:8080"); !errors.Is(err, errBuyerConfig) || err.Error() != "invalid buyer configuration" {
			t.Fatalf("unsafe %s accepted or leaked: %v", name, err)
		}
		values[name] = original
	}
	if _, _, err := buildBuyerPayment(context.Background(), buyerPaymentConfig{enabled: true}); !errors.Is(err, errBuyerConfig) {
		t.Fatalf("unconfigured signer reached database: %v", err)
	}
}

func TestBuyerPaymentStripeFlagIsNestedAndNeverReadsStripeSecrets(t *testing.T) {
	values := buyerPaymentTestEnv()
	var reads []string
	get := func(name string) string { reads = append(reads, name); return values[name] }
	load := func() (buyerPaymentConfig, error) { return loadBuyerPaymentConfig(get, "127.0.0.1:8080") }

	values["COMMERCE_STRIPE_CHECKOUT_ENABLED"] = "1"
	values["STRIPE_SECRET_KEY"] = "sentinel-must-not-be-read"
	values["STRIPE_ACCOUNT_ID"] = "sentinel-must-not-be-read"
	c, err := load()
	if err != nil || !c.enabled || c.stripe == nil || c.stripe.ReturnURL != "https://pay.example.com/return" {
		t.Fatalf("stripe checkout not admitted with the canonical neutral return url: %+v %v", c, err)
	}
	for _, name := range reads {
		if strings.HasPrefix(name, "STRIPE_") {
			t.Fatalf("API read global Stripe variable %s", name)
		}
	}
	for _, off := range []string{"", "0"} {
		values["COMMERCE_STRIPE_CHECKOUT_ENABLED"] = off
		if c, err = load(); err != nil || c.stripe != nil {
			t.Fatalf("flag %q enabled Stripe: %v", off, err)
		}
	}
	// PAYUNi config stays required: there is no Stripe-only B1 deployment.
	values["COMMERCE_STRIPE_CHECKOUT_ENABLED"] = "1"
	values["COMMERCE_PAYMENT_NOTIFY_URL"] = ""
	if _, err = load(); !errors.Is(err, errBuyerConfig) {
		t.Fatalf("stripe without PAYUNi config accepted: %v", err)
	}
	values["COMMERCE_PAYMENT_NOTIFY_URL"] = "https://pay.example.com/notify"
	for name, bad := range map[string]string{"COMMERCE_STRIPE_CHECKOUT_ENABLED": "true", "COMMERCE_PAYMENT_PROFILE": "LIVE"} {
		original := values[name]
		values[name] = bad
		if _, err = load(); !errors.Is(err, errBuyerConfig) {
			t.Fatalf("unsafe %s=%s accepted: %v", name, bad, err)
		}
		values[name] = original
	}
	// LIVE remains valid for PAYUNi alone; only Stripe on LIVE is refused.
	values["COMMERCE_PAYMENT_PROFILE"] = "LIVE"
	values["COMMERCE_STRIPE_CHECKOUT_ENABLED"] = ""
	if c, err = load(); err != nil || c.stripe != nil {
		t.Fatalf("PAYUNi LIVE config regressed: %v", err)
	}

	// Disabled buyer payment must not even read the nested Stripe flag.
	values["COMMERCE_STRIPE_CHECKOUT_ENABLED"] = "1"
	values["COMMERCE_BUYER_PAYMENT_ENABLED"] = "0"
	reads = nil
	if c, err = load(); err != nil || c.enabled || c.stripe != nil || len(reads) != 1 || reads[0] != "COMMERCE_BUYER_PAYMENT_ENABLED" {
		t.Fatalf("disabled payment read nested Stripe configuration: %v %v", reads, err)
	}
}
