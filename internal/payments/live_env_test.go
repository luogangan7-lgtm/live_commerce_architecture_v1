// live_env_test.go: package-internal unit tests of the LIVE environment rules of
// contracts/stripe-live-enable-v1.md §5.2 (S5/S6/S7): ProfileEnvironment, the snapshot and refund-loader
// environment guards, the session-identity livemode rule, and the LIVE runtime constructors' argument gates.
// Non-goals: no database (NewLiveStripeRuntime stops at argument validation with a nil pool), no network, no real key.
// Callers: go test ./internal/payments (names deliberately do not start with TestStripeSL: those belong to
// the independent stripe-live-tests unit).

package payments

import (
	"context"
	"errors"
	"testing"

	"livecommerce/internal/integrations/accounts"
	"livecommerce/internal/integrations/psp/stripe"
)

func TestProfileEnvironmentMapping(t *testing.T) {
	for profile, want := range map[string]string{"PROVIDER_MOCK": "SANDBOX", "SANDBOX": "SANDBOX", "LIVE": "LIVE"} {
		if got, ok := ProfileEnvironment(profile); !ok || got != want {
			t.Fatalf("%s -> %q %v", profile, got, ok)
		}
	}
	for _, bad := range []string{"", "live", "sandbox", "PRODUCTION", "LIVE ", "REAL_LIVE"} {
		if got, ok := ProfileEnvironment(bad); ok || got != "" {
			t.Fatalf("%q accepted as %q", bad, got)
		}
	}
}

func TestSnapshotEnvironmentMustEqualProfileEnvironment(t *testing.T) {
	live := testStripeSnapshot()
	live.Environment, live.Profile = "LIVE", "LIVE"
	live.CreateParams["metadata[lc_profile]"] = "LIVE"
	if !validStripeSnapshot(live, stripeTestAttempt, "LIVE") {
		t.Fatal("valid LIVE snapshot refused")
	}
	if validStripeSnapshot(live, stripeTestAttempt, "SANDBOX") {
		t.Fatal("LIVE snapshot accepted by a SANDBOX runtime")
	}
	crossed := live
	crossed.Environment = "SANDBOX" // a SANDBOX row under a LIVE profile
	if validStripeSnapshot(crossed, stripeTestAttempt, "LIVE") {
		t.Fatal("SANDBOX-environment snapshot accepted by a LIVE runtime")
	}
	sandbox := testStripeSnapshot()
	if !validStripeSnapshot(sandbox, stripeTestAttempt, "SANDBOX") || validStripeSnapshot(sandbox, stripeTestAttempt, "LIVE") {
		t.Fatal("SANDBOX rules changed")
	}
	// PROVIDER_MOCK keeps the SANDBOX environment.
	mock := testStripeSnapshot()
	mock.Profile = "PROVIDER_MOCK"
	mock.CreateParams["metadata[lc_profile]"] = "PROVIDER_MOCK"
	if !validStripeSnapshot(mock, stripeTestAttempt, "PROVIDER_MOCK") {
		t.Fatal("PROVIDER_MOCK snapshot refused")
	}
	if validStripeSnapshot(sandbox, stripeTestAttempt, "PRODUCTION") {
		t.Fatal("unknown profile accepted")
	}
}

func TestRefundLoaderEnvironmentMustEqualProfileEnvironment(t *testing.T) {
	live := testRefundSnapshot()
	live.Environment = "LIVE"
	if !validRefundSnapshot(live, stripeTestOrder, "LIVE") {
		t.Fatal("valid LIVE refund snapshot refused")
	}
	for _, profile := range []string{"SANDBOX", "PROVIDER_MOCK"} {
		if validRefundSnapshot(live, stripeTestOrder, profile) {
			t.Fatalf("LIVE refund accepted by %s", profile)
		}
	}
	sandbox := testRefundSnapshot()
	if validRefundSnapshot(sandbox, stripeTestOrder, "LIVE") || !validRefundSnapshot(sandbox, stripeTestOrder, "SANDBOX") ||
		!validRefundSnapshot(sandbox, stripeTestOrder, "PROVIDER_MOCK") {
		t.Fatal("SANDBOX refund rules changed")
	}
	if validRefundSnapshot(sandbox, stripeTestOrder, "PRODUCTION") {
		t.Fatal("unknown profile accepted")
	}
}

func TestSessionIdentityLivemodeMustEqualEnvironment(t *testing.T) {
	expires := testStripeSnapshot().ExpiresAt.Unix()
	identity := func(env string, livemode bool) bool {
		s := testStripeSnapshot()
		s.Environment = env
		session := stripe.Session{ID: "cs_x", ClientReferenceID: s.AttemptID, MetadataAttempt: s.AttemptID,
			MetadataProfile: s.Profile, ExpiresAt: &expires, Mode: "payment", PaymentMethodTypes: []string{"card"},
			Livemode: livemode}
		return stripeSessionIdentity(session, s)
	}
	if !identity("SANDBOX", false) || identity("SANDBOX", true) || !identity("LIVE", true) || identity("LIVE", false) {
		t.Fatal("livemode must equal (environment == LIVE) in both directions")
	}
}

func TestLiveRuntimeConstructorsAreGated(t *testing.T) {
	ctx := context.Background()
	keys := &accounts.Keyring{}
	pair := stripe.LiveApproval{Enabled: true, Reference: "owner-chat:2026-09-30:stripe-live:shop"}
	// S7: the SANDBOX constructor keeps refusing LIVE, with or without a transport.
	if _, err := NewStripeRuntime(ctx, nil, keys, "LIVE"); !errors.Is(err, errStripeRuntimeConfig) {
		t.Fatalf("NewStripeRuntime admitted LIVE: %v", err)
	}
	for name, tc := range map[string]struct {
		ctx  context.Context
		keys *accounts.Keyring
		live stripe.LiveApproval
	}{
		"nil ctx":             {nil, keys, pair}, //nolint:staticcheck // nil ctx is the case under test
		"nil keys":            {ctx, nil, pair},
		"zero pair":           {ctx, keys, stripe.LiveApproval{}},
		"flag without ref":    {ctx, keys, stripe.LiveApproval{Enabled: true}},
		"ref without flag":    {ctx, keys, stripe.LiveApproval{Reference: pair.Reference}},
		"short ref":           {ctx, keys, stripe.LiveApproval{Enabled: true, Reference: "short"}},
		"ref with space":      {ctx, keys, stripe.LiveApproval{Enabled: true, Reference: "owner approval ok"}},
		"nil pool, good pair": {ctx, keys, pair}, // pool is nil in every row
	} {
		if _, err := NewLiveStripeRuntime(tc.ctx, nil, tc.keys, tc.live); !errors.Is(err, errStripeRuntimeConfig) {
			t.Fatalf("%s: want config error, got %v", name, err)
		}
	}
}

func TestStripeClientConfigCarriesThePairOnlyForLive(t *testing.T) {
	pair := stripe.LiveApproval{Enabled: true, Reference: "owner-chat:2026-09-30:stripe-live:shop"}
	live := &StripeRuntime{profile: "LIVE", live: pair}
	if cfg := live.stripeClientConfig("k", "acct_1FakeAccount000", "LIVE"); cfg.Live != pair || cfg.Environment != "LIVE" {
		t.Fatalf("LIVE config lost the pair: %+v", cfg.Live)
	}
	// A SANDBOX row through a LIVE runtime (a snapshot guard bug) would still never carry the pair.
	if cfg := live.stripeClientConfig("k", "acct_1FakeAccount000", "SANDBOX"); cfg.Live != (stripe.LiveApproval{}) {
		t.Fatal("pair attached to a SANDBOX client")
	}
	sandbox := &StripeRuntime{profile: "SANDBOX"}
	if cfg := sandbox.stripeClientConfig("k", "acct_1FakeAccount000", "SANDBOX"); cfg.Live != (stripe.LiveApproval{}) {
		t.Fatal("SANDBOX runtime carries a pair")
	}
}
