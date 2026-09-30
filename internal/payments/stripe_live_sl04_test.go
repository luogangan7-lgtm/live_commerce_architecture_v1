// stripe_live_sl04_test.go is the Go half of gate SL04 (contracts/stripe-live-enable-v1.md §11 SL04, §5.2): the worker's
// environment rules for LIVE, package-internal because the guards are unexported. UNIT tier, no database, no network,
// no key. Written from the contract text (validStripeSnapshot requires Environment = the profile's environment;
// stripeSessionIdentity requires session.Livemode == (Environment=="LIVE"); the refund loader applies the same rule) and
// independent of the implementer's own tests (live_env_test.go). The REAL_PG half of the same top-level name is
// tests/foundation/stripe_live_shape_test.go.
// Callers: go test ./internal/payments -run '^TestStripeSL04LiveShape$'.

package payments

import (
	"strconv"
	"testing"
	"time"

	"livecommerce/internal/integrations/psp/stripe"
)

const (
	slpAttempt = "0b6a9d2e-7a55-4c53-9c07-0a8f1e0c5a01"
	slpID      = "6f1c2a0e-3b5d-4e77-8a11-9d0c7b3e2f10"
)

// slpParams is the frozen §9.2 create_params key set for one attempt and profile.
func slpParams(profile string, expires time.Time) map[string]string {
	return map[string]string{
		"adaptive_pricing[enabled]": "false", "managed_payments[enabled]": "false", "automatic_tax[enabled]": "false",
		"metadata[lc_attempt]": slpAttempt, "cancel_url": "https://shop.example.test/return", "metadata[lc_order]": slpID,
		"client_reference_id": slpAttempt, "metadata[lc_profile]": profile, "expires_at": strconv.FormatInt(expires.Unix(), 10),
		"metadata[lc_v]": "1", "line_items[0][price_data][currency]": "twd", "mode": "payment",
		"line_items[0][price_data][product_data][name]": "Order 6F1C2A0E", "line_items[0][price_data][unit_amount]": "2500",
		"line_items[0][quantity]": "1", "payment_intent_data[metadata][lc_attempt]": slpAttempt, "locale": "zh",
		"payment_intent_data[metadata][lc_order]": slpID, "payment_method_types[0]": "card", "submit_type": "pay",
		"success_url": "https://shop.example.test/return", "ui_mode": "hosted_page",
	}
}

func slpSnapshot(profile, environment string) stripeSessionSnapshot {
	expires := time.Unix(1790000000, 0).UTC()
	return stripeSessionSnapshot{
		TenantID: slpID, StoreID: slpID, AttemptID: slpAttempt, ConnectionID: slpID, CredentialVersion: 1,
		Environment: environment, AccountID: "acct_SlpFixture01", Currency: "TWD", Profile: profile,
		AmountMinor: 2500, UnitAmount: 2500, CreateParams: slpParams(profile, expires),
		ExpiresAt: expires, SendDeadline: expires.Add(-33 * time.Minute), HandoffCutoff: expires.Add(-5 * time.Minute),
		DBNow: expires.Add(-39 * time.Minute),
	}
}

func slpRefundSnapshot(environment string) stripeRefundSnapshot {
	requested := time.Unix(1790000000, 0).UTC()
	return stripeRefundSnapshot{
		TenantID: slpID, StoreID: slpID, AttemptID: slpAttempt, OrderID: slpID, ConnectionID: slpID, Environment: environment,
		AccountID: "acct_SlpFixture01", CredentialVersion: 1, KeyID: "k1", Nonce: make([]byte, 12), Ciphertext: make([]byte, 32),
		PaymentIntentID: "pi_slp1", Currency: "TWD", Reason: "requested_by_customer", AmountMinor: 2500,
		RequestedAt: requested, ResendUntil: requested.Add(20 * time.Hour), DBNow: requested,
	}
}

func TestStripeSL04LiveShape(t *testing.T) {
	// Environment of a profile (one helper, S6): mock and sandbox are SANDBOX, LIVE is LIVE, nothing else exists.
	for profile, want := range map[string]string{"PROVIDER_MOCK": "SANDBOX", "SANDBOX": "SANDBOX", "LIVE": "LIVE"} {
		if got, ok := ProfileEnvironment(profile); !ok || got != want {
			t.Fatalf("ProfileEnvironment(%s)=%q,%v want %q", profile, got, ok, want)
		}
	}
	for _, bad := range []string{"", "live", "Live", "sandbox", "PRODUCTION", "LIVE ", "REAL_LIVE", "STAGING"} {
		if got, ok := ProfileEnvironment(bad); ok || got != "" {
			t.Fatalf("ProfileEnvironment(%q)=%q,%v: unknown profiles have no environment", bad, got, ok)
		}
	}

	// validStripeSnapshot: Environment must equal the profile's environment, in every combination.
	for _, c := range []struct {
		profile, environment string
		valid                bool
	}{
		{"LIVE", "LIVE", true}, {"SANDBOX", "SANDBOX", true}, {"PROVIDER_MOCK", "SANDBOX", true},
		{"LIVE", "SANDBOX", false}, {"SANDBOX", "LIVE", false}, {"PROVIDER_MOCK", "LIVE", false},
		{"LIVE", "", false}, {"LIVE", "live", false}, {"PRODUCTION", "LIVE", false}, {"", "LIVE", false},
	} {
		s := slpSnapshot(c.profile, c.environment)
		if got := validStripeSnapshot(s, slpAttempt, c.profile); got != c.valid {
			t.Errorf("validStripeSnapshot(profile=%q environment=%q)=%v want %v", c.profile, c.environment, got, c.valid)
		}
	}
	// A snapshot valid for one profile is refused by a runtime of the other environment.
	live := slpSnapshot("LIVE", "LIVE")
	if validStripeSnapshot(live, slpAttempt, "SANDBOX") || validStripeSnapshot(live, slpAttempt, "PROVIDER_MOCK") {
		t.Fatal("a LIVE snapshot was accepted by a SANDBOX/mock runtime")
	}
	sandbox := slpSnapshot("SANDBOX", "SANDBOX")
	if validStripeSnapshot(sandbox, slpAttempt, "LIVE") {
		t.Fatal("a SANDBOX snapshot was accepted by a LIVE runtime")
	}
	// The other frozen snapshot rules still hold under LIVE (mutation-sensitive sanity: not everything is accepted).
	broken := slpSnapshot("LIVE", "LIVE")
	broken.AttemptID = slpID
	if validStripeSnapshot(broken, slpAttempt, "LIVE") {
		t.Fatal("a snapshot of another attempt was accepted")
	}
	broken = slpSnapshot("LIVE", "LIVE")
	broken.CreateParams["metadata[lc_profile]"] = "SANDBOX"
	if validStripeSnapshot(broken, slpAttempt, "LIVE") {
		t.Fatal("create_params profile differing from the runtime profile was accepted")
	}

	// stripeSessionIdentity: session.Livemode == (snapshot environment == LIVE), both directions.
	for _, c := range []struct {
		environment string
		livemode    bool
		want        bool
	}{{"LIVE", true, true}, {"LIVE", false, false}, {"SANDBOX", false, true}, {"SANDBOX", true, false}} {
		profile := "SANDBOX"
		if c.environment == "LIVE" {
			profile = "LIVE"
		}
		snap := slpSnapshot(profile, c.environment)
		expires := snap.ExpiresAt.Unix()
		session := stripe.Session{ID: "cs_slp1", ClientReferenceID: slpAttempt, MetadataAttempt: slpAttempt, MetadataProfile: profile,
			ExpiresAt: &expires, Mode: "payment", Livemode: c.livemode, PaymentMethodTypes: []string{"card"}}
		if got := stripeSessionIdentity(session, snap); got != c.want {
			t.Errorf("stripeSessionIdentity(environment=%s livemode=%v)=%v want %v", c.environment, c.livemode, got, c.want)
		}
	}
	// The identity check is not a livemode-only predicate: any other drift still refuses a matching livemode.
	snap := slpSnapshot("LIVE", "LIVE")
	expires := snap.ExpiresAt.Unix()
	drifted := stripe.Session{ID: "cs_slp1", ClientReferenceID: slpID, MetadataAttempt: slpAttempt, MetadataProfile: "LIVE",
		ExpiresAt: &expires, Mode: "payment", Livemode: true, PaymentMethodTypes: []string{"card"}}
	if stripeSessionIdentity(drifted, snap) {
		t.Fatal("a session with another client_reference_id was accepted")
	}

	// Refund loader: the refund row's environment must be the runtime profile's environment.
	for _, c := range []struct {
		profile, environment string
		valid                bool
	}{
		{"LIVE", "LIVE", true}, {"SANDBOX", "SANDBOX", true}, {"PROVIDER_MOCK", "SANDBOX", true},
		{"LIVE", "SANDBOX", false}, {"SANDBOX", "LIVE", false}, {"PROVIDER_MOCK", "LIVE", false}, {"PRODUCTION", "LIVE", false},
	} {
		if got := validRefundSnapshot(slpRefundSnapshot(c.environment), slpID, c.profile); got != c.valid {
			t.Errorf("refund loader(profile=%q environment=%q)=%v want %v", c.profile, c.environment, got, c.valid)
		}
	}
	badRefund := slpRefundSnapshot("LIVE")
	badRefund.PaymentIntentID = ""
	if validRefundSnapshot(badRefund, slpID, "LIVE") {
		t.Fatal("a refund snapshot without a payment intent was accepted")
	}
}
