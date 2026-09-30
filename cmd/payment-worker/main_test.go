package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"strings"
	"testing"
)

func testEnvironment() map[string]string {
	key := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{1}, 32))
	replay := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{2}, 32))
	return map[string]string{
		"COMMERCE_PAYMENT_WORKER_ENABLED":      "1",
		"COMMERCE_PAYMENT_WORKER_DATABASE_URL": "postgres://secret@localhost/test",
		"COMMERCE_PAYMENT_WORKER_PROFILE":      "SANDBOX",
		"COMMERCE_ACCOUNT_ACTIVE_KEY_ID":       "key-1",
		"COMMERCE_ACCOUNT_KEYS_JSON":           `[{"id":"key-1","key_base64":"` + key + `"}]`,
		"COMMERCE_ACCOUNT_REPLAY_KEY":          replay,
	}
}

func TestDisabledWorkerReadsOnlyFlag(t *testing.T) {
	for _, flag := range []string{"", "0"} {
		read := []string{}
		env := func(name string) string {
			read = append(read, name)
			if name == "COMMERCE_PAYMENT_WORKER_ENABLED" {
				return flag
			}
			t.Fatal("disabled worker inspected another variable")
			return ""
		}
		if err := run(context.Background(), env); err != nil || len(read) != 1 ||
			read[0] != "COMMERCE_PAYMENT_WORKER_ENABLED" {
			t.Fatalf("disabled worker: reads=%v err=%v", read, err)
		}
	}
}

func TestWorkerConfigStrictAndRedacted(t *testing.T) {
	base := testEnvironment()
	read := func(values map[string]string) func(string) string {
		return func(name string) string { return values[name] }
	}
	config, err := loadConfig(read(base))
	if err != nil || !config.enabled || config.profile != "SANDBOX" ||
		config.concurrency != 4 || config.keys == nil {
		t.Fatal("valid default configuration rejected")
	}
	for _, tc := range []struct{ field, value string }{
		{"COMMERCE_PAYMENT_WORKER_ENABLED", "true"},
		{"COMMERCE_PAYMENT_WORKER_DATABASE_URL", ""},
		{"COMMERCE_PAYMENT_WORKER_PROFILE", "PROVIDER_MOCK"},
		{"COMMERCE_PAYMENT_WORKER_PROFILE", "LIVE "},
		{"COMMERCE_PAYMENT_WORKER_CONCURRENCY", "0"},
		{"COMMERCE_PAYMENT_WORKER_CONCURRENCY", "17"},
		{"COMMERCE_PAYMENT_WORKER_CONCURRENCY", "04"},
		{"COMMERCE_PAYMENT_WORKER_CONCURRENCY", "+4"},
		{"COMMERCE_ACCOUNT_KEYS_JSON", "secret-invalid-json"},
	} {
		values := testEnvironment()
		values[tc.field] = tc.value
		_, err := loadConfig(read(values))
		if !errors.Is(err, errWorkerConfig) && !errors.Is(err, errWorkerKeyring) {
			t.Fatalf("%s accepted", tc.field)
		}
		if strings.Contains(err.Error(), tc.value) && tc.value != "" {
			t.Fatalf("%s leaked input in error", tc.field)
		}
	}
}

// ---- stripe-live-core: LIVE Stripe needs the owner's pair (contracts/stripe-live-enable-v1.md §5.2) ----

const workerLiveRef = "owner-chat:2026-09-30:stripe-live:shop"

func TestWorkerLiveStripeNeedsThePairAndOthersReadNoLiveName(t *testing.T) {
	load := func(profile, stripeFlag string, extra map[string]string) (workerConfig, []string, error) {
		values := testEnvironment()
		values["COMMERCE_PAYMENT_WORKER_PROFILE"] = profile
		values["COMMERCE_STRIPE_ENABLED"] = stripeFlag
		for k, v := range extra {
			values[k] = v
		}
		var read []string
		c, err := loadConfig(func(n string) string { read = append(read, n); return values[n] })
		return c, read, err
	}
	liveNames := func(read []string) (out []string) {
		for _, n := range read {
			if strings.HasPrefix(n, "COMMERCE_STRIPE_LIVE_") {
				out = append(out, n)
			}
		}
		return out
	}
	pair := map[string]string{"COMMERCE_STRIPE_LIVE_ENABLED": "1", "COMMERCE_STRIPE_LIVE_APPROVAL_REF": workerLiveRef}
	c, read, err := load("LIVE", "1", pair)
	if err != nil || !c.stripe || !c.live.Enabled || c.live.Reference != workerLiveRef || len(liveNames(read)) != 2 {
		t.Fatalf("LIVE Stripe with the pair: %+v %v", c.live.Enabled, err)
	}
	for name, extra := range map[string]map[string]string{
		"no pair":         nil,
		"flag only":       {"COMMERCE_STRIPE_LIVE_ENABLED": "1"},
		"ref only":        {"COMMERCE_STRIPE_LIVE_APPROVAL_REF": workerLiveRef},
		"flag 0 with ref": {"COMMERCE_STRIPE_LIVE_ENABLED": "0", "COMMERCE_STRIPE_LIVE_APPROVAL_REF": workerLiveRef},
		"short ref":       {"COMMERCE_STRIPE_LIVE_ENABLED": "1", "COMMERCE_STRIPE_LIVE_APPROVAL_REF": "short"},
	} {
		_, _, err := load("LIVE", "1", extra)
		if !errors.Is(err, errWorkerConfig) || strings.Contains(err.Error(), workerLiveRef) {
			t.Fatalf("LIVE Stripe %s accepted or leaked: %v", name, err)
		}
	}
	// PAYUNi on LIVE (Stripe off) and every SANDBOX worker never read the LIVE names, even when they are set.
	for _, tc := range []struct{ profile, flag string }{{"LIVE", ""}, {"LIVE", "0"}, {"SANDBOX", "1"}, {"SANDBOX", ""}} {
		c, read, err := load(tc.profile, tc.flag, pair)
		if err != nil || len(liveNames(read)) != 0 || c.live.Enabled {
			t.Fatalf("%s stripe=%q: %v read=%v", tc.profile, tc.flag, err, liveNames(read))
		}
	}
	if _, _, err := load("LIVE", "true", pair); !errors.Is(err, errWorkerConfig) {
		t.Fatalf("noncanonical Stripe flag accepted: %v", err)
	}
}
