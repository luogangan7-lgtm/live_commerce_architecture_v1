// main_test.go: usage, environment gating and secret-hygiene tests for the registrar CLI (MOCK tier).
// Non-goal: the registry SQL (REAL_PG SP21, blocked on pool-fix) and any real Stripe call.
package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"strings"
	"testing"

	"livecommerce/internal/payments/stripeadmin"
)

// Synthetic DSN sentinels live in their own constants so no source line looks like a
// credential to secret scanners (GitGuardian false positives 2026-09-29); they are test sentinels.
const (
	dsnSentinel1 = "sentinel-operator-9f"
)

func env() map[string]string {
	k := func(b byte) string { return base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{b}, 32)) }
	return map[string]string{
		"COMMERCE_STRIPE_REGISTRAR_DATABASE_URL": "postgres://operator:" + dsnSentinel1 + "@127.0.0.1:1/lc",
		"COMMERCE_ACCOUNT_ACTIVE_KEY_ID":         "api-1",
		"COMMERCE_ACCOUNT_KEYS_JSON":             `[{"id":"api-1","key_base64":"` + k(1) + `"}]`,
		"COMMERCE_ACCOUNT_REPLAY_KEY":            k(2),
		"COMMERCE_STRIPE_WEBHOOK_ACTIVE_KEY_ID":  "sig-1",
		"COMMERCE_STRIPE_WEBHOOK_KEYS_JSON":      `[{"id":"sig-1","key_base64":"` + k(3) + `"}]`,
		"COMMERCE_STRIPE_WEBHOOK_REPLAY_KEY":     k(4),
		"STRIPE_SECRET_KEY":                      "sk_" + "test_CLISENTINELKEY000000000",
		"STRIPE_ACCOUNT_ID":                      "acct_1CliTest000000",
		"STRIPE_WEBHOOK_SECRET":                  "whsec_" + "cliSentinelSecret_0123456789",
	}
}

const ids = "--tenant 11111111-1111-4111-8111-111111111111 --store 22222222-2222-4222-8222-222222222222 --principal 33333333-3333-4333-8333-333333333333"

func do(t *testing.T, values map[string]string, line string) (string, error) {
	t.Helper()
	var out bytes.Buffer
	err := run(context.Background(), strings.Fields(line), func(n string) string { return values[n] }, &out)
	return out.String(), err
}

func TestUsageErrorsAreFixedAndPrintNothing(t *testing.T) {
	for _, line := range []string{"", "bogus", "register extra", "register --nope=sk_test_leak", "rotate --expected-version=notanumber",
		"method --sort=abc"} {
		out, err := do(t, env(), line)
		if !errors.Is(err, errUsage) || out != "" || strings.Contains(err.Error(), "leak") || strings.Contains(err.Error(), "abc") {
			t.Fatalf("%q -> %q %v", line, out, err)
		}
	}
}

func TestEnvironmentGatesBeforeAnyConnection(t *testing.T) {
	cases := []struct {
		name, line string
		mutate     func(map[string]string)
	}{
		{"missing dsn", "register " + ids, func(v map[string]string) { delete(v, "COMMERCE_STRIPE_REGISTRAR_DATABASE_URL") }},
		{"register without api keyring", "register " + ids, func(v map[string]string) { delete(v, "COMMERCE_ACCOUNT_KEYS_JSON") }},
		{"rotate without api keyring", "rotate " + ids + " --connection x --expected-version 1", func(v map[string]string) { delete(v, "COMMERCE_ACCOUNT_REPLAY_KEY") }},
		{"webhook without signing keyring", "webhook " + ids + " --connection x --profile SANDBOX", func(v map[string]string) { delete(v, "COMMERCE_STRIPE_WEBHOOK_KEYS_JSON") }},
		{"webhook with api keyring names only", "webhook " + ids + " --connection x --profile SANDBOX", func(v map[string]string) {
			for k := range v {
				if strings.HasPrefix(k, "COMMERCE_STRIPE_WEBHOOK_") {
					delete(v, k)
				}
			}
		}},
		{"sandbox qualify without opt-in", "qualify " + ids + " --profile SANDBOX --connection x --expected-version 1", func(v map[string]string) {}},
	}
	for _, tc := range cases {
		v := env()
		tc.mutate(v)
		out, err := do(t, v, tc.line)
		if !errors.Is(err, errConfig) || out != "" {
			t.Fatalf("%s: %q %v", tc.name, out, err)
		}
	}
}

func TestValidEnvironmentReachesMaskedDatabaseError(t *testing.T) {
	for _, line := range []string{"register " + ids, "rotate " + ids + " --connection c --expected-version 1",
		"webhook " + ids + " --connection c --profile SANDBOX --enabled", "qualify " + ids + " --profile PROVIDER_MOCK --connection c --expected-version 1",
		"method " + ids + " --market m --country HK"} {
		out, err := do(t, env(), line)
		if !errors.Is(err, stripeadmin.ErrDatabase) || out != "" {
			t.Fatalf("%q: %q %v", line, out, err)
		}
		for _, banned := range []string{dsnSentinel1, "127.0.0.1", "sk_", "whsec"} {
			if strings.Contains(err.Error(), banned) {
				t.Fatalf("error leaked %s", banned)
			}
		}
	}
}

// ---- stripe-live-core: LIVE subcommands and gates (contracts/stripe-live-enable-v1.md §5.2, S4, S8) ----

const liveRef = "owner-chat:2026-09-30:stripe-live:shop"

func liveEnv() map[string]string {
	v := env()
	v["COMMERCE_STRIPE_LIVE_ENABLED"] = "1"
	v["COMMERCE_STRIPE_LIVE_APPROVAL_REF"] = liveRef
	// LIVE never needs the sandbox opt-in or an operator key: the stored key is unsealed with the API keyring.
	delete(v, "STRIPE_SECRET_KEY")
	delete(v, "STRIPE_ACCOUNT_ID")
	return v
}

const (
	approveLine = "live-approve " + ids + " --approval 55555555-5555-4555-8555-555555555555 --connection 44444444-4444-4444-8444-444444444444" +
		" --expected-version 2 --currency TWD --approval-ref " + liveRef + " --approved-at 2026-09-30T08:30:00Z --canary-max 5000 --max 2000000" +
		" --attest account_active,canary_private,descriptor,dispute_notice,managed_off,payout_bank,policy_pages,radar_default,rak_live,three_ds,webhook_live"
	canaryLine = "live-canary " + ids + " --approval 55555555-5555-4555-8555-555555555555 --attempt 66666666-6666-4666-8666-666666666666 --refund 77777777-7777-4777-8777-777777777777"
	revokeLine = "live-revoke " + ids + " --approval 55555555-5555-4555-8555-555555555555 --revoke-ref owner-chat:2026-09-30:revoke"
)

func TestLiveSubcommandsUsageErrorsAreFixed(t *testing.T) {
	for _, line := range []string{
		"live-bogus " + ids,
		"live-approve " + ids + " --nope=leak",
		strings.Replace(approveLine, "2026-09-30T08:30:00Z", "yesterday-leak", 1),
		"live-canary " + ids + " extra",
		"live-revoke " + ids + " --secret sentinel-flag-value",
		"register " + ids + " --environment PRODUCTION",
		"rotate " + ids + " --connection c --expected-version 1 --environment live",
	} {
		out, err := do(t, liveEnv(), line)
		if !errors.Is(err, errUsage) || out != "" || strings.Contains(err.Error(), "leak") || strings.Contains(err.Error(), "sentinel") {
			t.Fatalf("%q -> %q %v", line, out, err)
		}
	}
}

func TestLiveGatesNeedTheOwnersPairBeforeAnyConnection(t *testing.T) {
	registerLive := "register " + ids + " --environment LIVE"
	rotateLive := "rotate " + ids + " --connection c --expected-version 1 --environment LIVE"
	webhookLive := "webhook " + ids + " --connection c --profile LIVE --enabled"
	qualifyLive := "qualify " + ids + " --profile LIVE --connection c --expected-version 1"
	gated := map[string]string{"register": registerLive, "rotate": rotateLive, "webhook": webhookLive, "qualify": qualifyLive,
		"live-approve": approveLine, "live-canary": canaryLine}
	halves := map[string]func(map[string]string){
		"no pair": func(v map[string]string) {
			delete(v, "COMMERCE_STRIPE_LIVE_ENABLED")
			delete(v, "COMMERCE_STRIPE_LIVE_APPROVAL_REF")
		},
		"flag only":       func(v map[string]string) { delete(v, "COMMERCE_STRIPE_LIVE_APPROVAL_REF") },
		"ref only":        func(v map[string]string) { delete(v, "COMMERCE_STRIPE_LIVE_ENABLED") },
		"flag 0 with ref": func(v map[string]string) { v["COMMERCE_STRIPE_LIVE_ENABLED"] = "0" },
		"flag true":       func(v map[string]string) { v["COMMERCE_STRIPE_LIVE_ENABLED"] = "true" },
		"short ref":       func(v map[string]string) { v["COMMERCE_STRIPE_LIVE_APPROVAL_REF"] = "short" },
	}
	for name, line := range gated {
		for half, mutate := range halves {
			v := liveEnv()
			mutate(v)
			out, err := do(t, v, line)
			if !errors.Is(err, errConfig) || out != "" || strings.Contains(err.Error(), liveRef) {
				t.Fatalf("%s with %s: %q %v", name, half, out, err)
			}
		}
		// The complete pair passes the gates and reaches the (closed) database step, masked.
		out, err := do(t, liveEnv(), line)
		if !errors.Is(err, stripeadmin.ErrDatabase) || out != "" {
			t.Fatalf("%s with the pair: %q %v", name, out, err)
		}
	}
	// live-approve additionally needs the API keyring (it unseals the stored key) and never a STRIPE_* key.
	v := liveEnv()
	delete(v, "COMMERCE_ACCOUNT_KEYS_JSON")
	if out, err := do(t, v, approveLine); !errors.Is(err, errConfig) || out != "" {
		t.Fatalf("live-approve without the API keyring: %q %v", out, err)
	}
	// LIVE qualify needs no STRIPE_SANDBOX opt-in (the pair is the opt-in) but still the API keyring.
	v = liveEnv()
	delete(v, "COMMERCE_ACCOUNT_KEYS_JSON")
	if out, err := do(t, v, qualifyLive); !errors.Is(err, errConfig) || out != "" {
		t.Fatalf("live qualify without the API keyring: %q %v", out, err)
	}
}

// S8 / LD6: live-revoke and method never read the pair or a keyring, so the per-store kill switch cannot be
// blocked by deploy env. A recorder proves the read set.
func TestLiveRevokeAndMethodAreAdmittedWithoutThePair(t *testing.T) {
	for name, line := range map[string]string{
		"live-revoke":            revokeLine,
		"method --enabled=false": "method " + ids + " --market m --country HK --enabled=false",
	} {
		values := map[string]string{"COMMERCE_STRIPE_REGISTRAR_DATABASE_URL": env()["COMMERCE_STRIPE_REGISTRAR_DATABASE_URL"]}
		var read []string
		var out bytes.Buffer
		err := run(context.Background(), strings.Fields(line), func(n string) string { read = append(read, n); return values[n] }, &out)
		if !errors.Is(err, stripeadmin.ErrDatabase) || out.Len() != 0 {
			t.Fatalf("%s without any pair or keyring: %q %v", name, out.String(), err)
		}
		for _, n := range read {
			if n != "COMMERCE_STRIPE_REGISTRAR_DATABASE_URL" {
				t.Fatalf("%s read %s", name, n)
			}
		}
		// Even with a pair present in the environment, they never look at it.
		values["COMMERCE_STRIPE_LIVE_ENABLED"], values["COMMERCE_STRIPE_LIVE_APPROVAL_REF"] = "1", liveRef
		read = nil
		_ = run(context.Background(), strings.Fields(line), func(n string) string { read = append(read, n); return values[n] }, &out)
		for _, n := range read {
			if strings.HasPrefix(n, "COMMERCE_STRIPE_LIVE_") {
				t.Fatalf("%s read %s", name, n)
			}
		}
	}
}

// SANDBOX paths keep their read set: they never look at the LIVE pair (SP15 read-set invariant).
func TestSandboxPathsReadNoLiveVariable(t *testing.T) {
	for _, line := range []string{
		"register " + ids, "register " + ids + " --environment SANDBOX",
		"rotate " + ids + " --connection c --expected-version 1",
		"webhook " + ids + " --connection c --profile SANDBOX --enabled",
		"qualify " + ids + " --profile PROVIDER_MOCK --connection c --expected-version 1",
	} {
		v := env()
		v["COMMERCE_STRIPE_LIVE_ENABLED"], v["COMMERCE_STRIPE_LIVE_APPROVAL_REF"] = "1", liveRef
		var read []string
		_ = run(context.Background(), strings.Fields(line), func(n string) string { read = append(read, n); return v[n] }, &bytes.Buffer{})
		for _, n := range read {
			if strings.HasPrefix(n, "COMMERCE_STRIPE_LIVE_") {
				t.Fatalf("%q read %s", line, n)
			}
		}
	}
}
