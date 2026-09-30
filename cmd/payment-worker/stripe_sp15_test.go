package main

// SP15 no-PG half for cmd/payment-worker (contracts/stripe-psp-v1.md §12, §0.2;
// docs/delivery/units/stripe-b1-ingress-assembly.md FROZEN worker seam:
// workerConfig.stripe bool and errWorkerStripe). The PG/process half, including
// the built binary reaching payment_worker_ready on SANDBOX with
// COMMERCE_STRIPE_ENABLED=1 and exiting 0 on SIGTERM, is TestStripeSP15Process in
// tests/foundation/stripe_process_test.go.

import (
	"context"
	"errors"
	"sort"
	"strings"
	"testing"
)

// Synthetic DSN sentinels live in their own constants so no source line looks like a
// credential to secret scanners (GitGuardian false positives 2026-09-29); they are test sentinels.
const (
	dsnSentinel1 = "pw-sentinel"
)

func TestStripeSP15Process(t *testing.T) {
	t.Run("frozen_error_code", func(t *testing.T) {
		if errWorkerStripe.Error() != "payment_worker_stripe_unavailable" {
			t.Fatalf("code drifted: %q", errWorkerStripe)
		}
	})

	t.Run("disabled_worker_reads_only_its_flag_whatever_stripe_says", func(t *testing.T) {
		for _, flag := range []string{"", "0"} {
			read := []string{}
			env := func(name string) string {
				read = append(read, name)
				switch name {
				case "COMMERCE_PAYMENT_WORKER_ENABLED":
					return flag
				case "COMMERCE_STRIPE_ENABLED":
					return "1"
				case "STRIPE_SECRET_KEY":
					return "sk_" + "test_sentinel0123456789abcdef"
				}
				return ""
			}
			if err := run(context.Background(), env); err != nil || len(read) != 1 || read[0] != "COMMERCE_PAYMENT_WORKER_ENABLED" {
				t.Fatalf("disabled worker (flag %q): reads=%v err=%v", flag, read, err)
			}
		}
	})

	t.Run("stripe_flag_is_read_only_when_enabled_and_is_strict", func(t *testing.T) {
		values := func(extra map[string]string) func(string) string {
			m := testEnvironment()
			for k, v := range extra {
				m[k] = v
			}
			return func(n string) string { return m[n] }
		}
		for _, flag := range []string{"", "0"} {
			c, err := loadConfig(values(map[string]string{"COMMERCE_STRIPE_ENABLED": flag}))
			if err != nil || c.stripe {
				t.Fatalf("stripe flag %q: stripe=%v err=%v", flag, c.stripe, err)
			}
		}
		if c, err := loadConfig(values(map[string]string{"COMMERCE_STRIPE_ENABLED": "1"})); err != nil || !c.stripe || c.profile != "SANDBOX" {
			t.Fatalf("stripe enabled on SANDBOX: %+v %v", c, err)
		}
		for name, extra := range map[string]map[string]string{
			"live_profile_with_stripe": {"COMMERCE_PAYMENT_WORKER_PROFILE": "LIVE", "COMMERCE_STRIPE_ENABLED": "1"},
			"true":                     {"COMMERCE_STRIPE_ENABLED": "true"},
			"space_one":                {"COMMERCE_STRIPE_ENABLED": " 1"},
			"two":                      {"COMMERCE_STRIPE_ENABLED": "2"},
			"word":                     {"COMMERCE_STRIPE_ENABLED": "sentinel-stripe-flag"},
		} {
			_, err := loadConfig(values(extra))
			if !errors.Is(err, errWorkerConfig) {
				t.Fatalf("%s accepted: %v", name, err)
			}
			for _, v := range extra {
				if strings.Contains(err.Error(), v) {
					t.Fatalf("%s leaked input %q", name, v)
				}
			}
		}
	})

	t.Run("worker_never_reads_global_stripe_or_webhook_secrets", func(t *testing.T) {
		m := testEnvironment()
		m["COMMERCE_STRIPE_ENABLED"] = "1"
		m["STRIPE_SECRET_KEY"], m["STRIPE_ACCOUNT_ID"] = "sk_"+"test_sentinel0123456789abcdef", "acct_SentinelAcct1"
		m["STRIPE_WEBHOOK_SECRET"], m["STRIPE_WEBHOOK_SECRET_NEXT"] = "whsec_"+"sentinel0123456789abcdef", "whsec_"+"sentinel_next_0123456789"
		m["COMMERCE_STRIPE_WEBHOOK_KEYS_JSON"], m["COMMERCE_STRIPE_INGRESS_DATABASE_URL"] = "signing-sentinel", "postgres://ingress-sentinel"
		var read []string
		if _, err := loadConfig(func(n string) string { read = append(read, n); return m[n] }); err != nil {
			t.Fatal(err)
		}
		allowed := map[string]bool{"COMMERCE_PAYMENT_WORKER_ENABLED": true, "COMMERCE_PAYMENT_WORKER_DATABASE_URL": true, "COMMERCE_PAYMENT_WORKER_PROFILE": true,
			"COMMERCE_PAYMENT_WORKER_CONCURRENCY": true, "COMMERCE_ACCOUNT_ACTIVE_KEY_ID": true, "COMMERCE_ACCOUNT_KEYS_JSON": true,
			"COMMERCE_ACCOUNT_REPLAY_KEY": true, "COMMERCE_STRIPE_ENABLED": true}
		sort.Strings(read)
		for _, name := range read {
			if !allowed[name] {
				t.Fatalf("worker config read %q; allowed %v", name, allowed)
			}
		}
	})

	t.Run("unreachable_database_reports_only_the_fixed_code", func(t *testing.T) {
		m := testEnvironment()
		m["COMMERCE_STRIPE_ENABLED"] = "1"
		m["COMMERCE_PAYMENT_WORKER_DATABASE_URL"] = "postgres://worker-sentinel:" + dsnSentinel1 + "@127.0.0.1:1/x"
		err := run(context.Background(), func(n string) string { return m[n] })
		if err == nil || strings.Contains(err.Error(), "sentinel") || strings.Contains(err.Error(), "postgres://") {
			t.Fatalf("run with an unreachable database: %v", err)
		}
	})
}
