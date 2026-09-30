// stripe_live_test.go: unit tests of the API-side LIVE gate (contracts/stripe-live-enable-v1.md §5.1, §5.2):
// loadStripeLiveApproval (both-or-neither, shape), LIVE admission of the webhook and buyer-Stripe configs only
// with the owner's pair, and that a SANDBOX/MOCK deployment reads no LIVE variable (SP15 read-set invariant).
// Non-goals: the ingress or hosted service assembly (needs PG) and any Stripe call; the API never reads STRIPE_*.
// Callers: go test ./cmd/api.

package main

import (
	"errors"
	"strings"
	"testing"
)

const liveTestRef = "owner-chat:2026-09-30:stripe-live:shop"

func liveNames(read []string) []string {
	var out []string
	for _, n := range read {
		if strings.HasPrefix(n, "COMMERCE_STRIPE_LIVE_") {
			out = append(out, n)
		}
	}
	return out
}

func TestLoadStripeLiveApprovalIsBothOrNeither(t *testing.T) {
	get := func(v map[string]string) func(string) string { return func(n string) string { return v[n] } }
	live, err := loadStripeLiveApproval(get(map[string]string{"COMMERCE_STRIPE_LIVE_ENABLED": "1", "COMMERCE_STRIPE_LIVE_APPROVAL_REF": liveTestRef}))
	if err != nil || !live.Enabled || live.Reference != liveTestRef {
		t.Fatalf("valid pair: %+v %v", live, err)
	}
	for name, v := range map[string]map[string]string{
		"neither (LIVE asked, none given)": {},
		"flag off with ref":                {"COMMERCE_STRIPE_LIVE_ENABLED": "0", "COMMERCE_STRIPE_LIVE_APPROVAL_REF": liveTestRef},
		"flag empty with ref":              {"COMMERCE_STRIPE_LIVE_APPROVAL_REF": liveTestRef},
		"flag without ref":                 {"COMMERCE_STRIPE_LIVE_ENABLED": "1"},
		"noncanonical flag true":           {"COMMERCE_STRIPE_LIVE_ENABLED": "true", "COMMERCE_STRIPE_LIVE_APPROVAL_REF": liveTestRef},
		"noncanonical flag 2":              {"COMMERCE_STRIPE_LIVE_ENABLED": "2", "COMMERCE_STRIPE_LIVE_APPROVAL_REF": liveTestRef},
		"flag with space":                  {"COMMERCE_STRIPE_LIVE_ENABLED": " 1", "COMMERCE_STRIPE_LIVE_APPROVAL_REF": liveTestRef},
		"short ref":                        {"COMMERCE_STRIPE_LIVE_ENABLED": "1", "COMMERCE_STRIPE_LIVE_APPROVAL_REF": "short"},
		"ref with spaces":                  {"COMMERCE_STRIPE_LIVE_ENABLED": "1", "COMMERCE_STRIPE_LIVE_APPROVAL_REF": "owner approval ok"},
		"ref too long":                     {"COMMERCE_STRIPE_LIVE_ENABLED": "1", "COMMERCE_STRIPE_LIVE_APPROVAL_REF": strings.Repeat("a", 129)},
	} {
		_, err := loadStripeLiveApproval(get(v))
		if err == nil {
			t.Fatalf("%s accepted", name)
		}
		if ref := v["COMMERCE_STRIPE_LIVE_APPROVAL_REF"]; ref != "" && strings.Contains(err.Error(), ref) {
			t.Fatalf("%s leaked the reference", name)
		}
	}
	if _, err := loadStripeLiveApproval(nil); err == nil {
		t.Fatal("nil getenv accepted")
	}
}

func TestStripeWebhookLiveNeedsThePairAndOthersReadNoLiveName(t *testing.T) {
	load := func(profile string, extra map[string]string) (stripeWebhookConfig, []string, error) {
		values := stripeTestEnv()
		values["COMMERCE_PAYMENT_PROFILE"] = profile
		for k, v := range extra {
			values[k] = v
		}
		var read []string
		c, err := loadStripeWebhookConfig(func(n string) string { read = append(read, n); return values[n] }, "127.0.0.1:8080")
		return c, read, err
	}
	pair := map[string]string{"COMMERCE_STRIPE_LIVE_ENABLED": "1", "COMMERCE_STRIPE_LIVE_APPROVAL_REF": liveTestRef}
	c, read, err := load("LIVE", pair)
	if err != nil || !c.enabled || c.profile != "LIVE" {
		t.Fatalf("LIVE with the pair: %+v %v", c, err)
	}
	if len(liveNames(read)) != 2 {
		t.Fatalf("LIVE read set: %v", liveNames(read))
	}
	for name, extra := range map[string]map[string]string{
		"no pair":         nil,
		"flag only":       {"COMMERCE_STRIPE_LIVE_ENABLED": "1"},
		"ref only":        {"COMMERCE_STRIPE_LIVE_APPROVAL_REF": liveTestRef},
		"flag 0 with ref": {"COMMERCE_STRIPE_LIVE_ENABLED": "0", "COMMERCE_STRIPE_LIVE_APPROVAL_REF": liveTestRef},
		"bad ref":         {"COMMERCE_STRIPE_LIVE_ENABLED": "1", "COMMERCE_STRIPE_LIVE_APPROVAL_REF": "short"},
	} {
		if _, _, err := load("LIVE", extra); !errors.Is(err, errStripeConfig) {
			t.Fatalf("LIVE %s accepted: %v", name, err)
		}
	}
	// SANDBOX and PROVIDER_MOCK deployments never read (and so never depend on) a LIVE variable, even when set.
	for _, profile := range []string{"SANDBOX", "PROVIDER_MOCK"} {
		c, read, err := load(profile, pair)
		if err != nil || !c.enabled || len(liveNames(read)) != 0 {
			t.Fatalf("%s: %v read=%v", profile, err, liveNames(read))
		}
	}
	if _, _, err := load("PRODUCTION", pair); !errors.Is(err, errStripeConfig) {
		t.Fatalf("unknown profile: %v", err)
	}
}

func TestBuyerStripeCheckoutLiveNeedsThePair(t *testing.T) {
	load := func(profile, stripeFlag string, extra map[string]string) (buyerPaymentConfig, []string, error) {
		values := buyerPaymentTestEnv()
		values["COMMERCE_PAYMENT_PROFILE"] = profile
		values["COMMERCE_STRIPE_CHECKOUT_ENABLED"] = stripeFlag
		for k, v := range extra {
			values[k] = v
		}
		var read []string
		c, err := loadBuyerPaymentConfig(func(n string) string { read = append(read, n); return values[n] }, "127.0.0.1:8080")
		return c, read, err
	}
	pair := map[string]string{"COMMERCE_STRIPE_LIVE_ENABLED": "1", "COMMERCE_STRIPE_LIVE_APPROVAL_REF": liveTestRef}
	c, read, err := load("LIVE", "1", pair)
	if err != nil || !c.enabled || c.stripe == nil || len(liveNames(read)) != 2 {
		t.Fatalf("LIVE Stripe with the pair: %v stripe=%v read=%v", err, c.stripe != nil, liveNames(read))
	}
	for name, extra := range map[string]map[string]string{
		"no pair":   nil,
		"flag only": {"COMMERCE_STRIPE_LIVE_ENABLED": "1"},
		"ref only":  {"COMMERCE_STRIPE_LIVE_APPROVAL_REF": liveTestRef},
		"bad ref":   {"COMMERCE_STRIPE_LIVE_ENABLED": "1", "COMMERCE_STRIPE_LIVE_APPROVAL_REF": "short"},
	} {
		if _, _, err := load("LIVE", "1", extra); !errors.Is(err, errBuyerConfig) {
			t.Fatalf("LIVE Stripe %s accepted: %v", name, err)
		}
	}
	// LD6: the platform kill switch (flag off) needs no pair and reads none; PAYUNi on LIVE is untouched.
	c, read, err = load("LIVE", "0", nil)
	if err != nil || c.stripe != nil || len(liveNames(read)) != 0 {
		t.Fatalf("LIVE with Stripe checkout off: %v stripe=%v read=%v", err, c.stripe != nil, liveNames(read))
	}
	for _, profile := range []string{"SANDBOX", "PROVIDER_MOCK"} {
		c, read, err := load(profile, "1", pair)
		if err != nil || c.stripe == nil || len(liveNames(read)) != 0 {
			t.Fatalf("%s Stripe: %v read=%v", profile, err, liveNames(read))
		}
	}
}
