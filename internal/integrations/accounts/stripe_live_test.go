// stripe_live_test.go: SL01 (contracts/stripe-live-enable-v1.md §11), the accounts-package
// half: validStripeAPIScope / validStripeWebhookScope LIVE rules (§5.2) and the LIVE key
// grammar at seal/open time. Tier: UNIT. Non-goals: no network, no PG, no real key (all keys
// are split sentinels so no live-shaped literal exists in the tree, PROCESS §6).
// Callers: go test ./internal/integrations/accounts (SL01 red-then-green log in
// output/stripe-live-core/).

package accounts

import (
	"errors"
	"testing"
)

const (
	stripeLiveRAK = "rk_" + "live_ABCDEFGHIJKLMNOPQR"
	stripeLiveSK  = "sk_" + "live_ABCDEFGHIJKLMNOPQR"
)

func stripeLiveAPIScope() StripeAPIScope {
	s := stripeAPITestScope()
	s.Environment = "LIVE"
	return s
}

func stripeLiveWebhookScope() StripeWebhookScope {
	w := stripeWebhookTestScope()
	w.Environment, w.Profile = "LIVE", "LIVE"
	return w
}

func TestStripeSL01LiveConfig(t *testing.T) {
	k := testKeyring(t)

	t.Run("api scope and key grammar", func(t *testing.T) {
		live := stripeLiveAPIScope()
		id, nonce, ct, err := k.SealStripeAPI(live, StripeAPICredentials{stripeLiveRAK})
		if err != nil {
			t.Fatalf("rk_live in a LIVE scope refused: %v", err)
		}
		got, err := k.OpenStripeAPI(live, id, nonce, ct)
		if err != nil || got.SecretKey != stripeLiveRAK {
			t.Fatalf("LIVE round trip failed: %v", err)
		}
		// The AAD binds the environment: a LIVE ciphertext never opens as SANDBOX.
		sandbox := live
		sandbox.Environment = "SANDBOX"
		if _, err := k.OpenStripeAPI(sandbox, id, nonce, ct); !errors.Is(err, errStripeMaterial) {
			t.Fatal("LIVE material opened under a SANDBOX scope")
		}
		for name, c := range map[string]struct {
			scope StripeAPIScope
			key   string
		}{
			"sk_live in LIVE (LD3: unrestricted key)": {live, stripeLiveSK},
			"test key in LIVE":                        {live, stripeTestKey},
			"rk_test in LIVE":                         {live, stripeTestNextKey},
			"rk_live in SANDBOX":                      {stripeAPITestScope(), stripeLiveRAK},
			"sk_live in SANDBOX":                      {stripeAPITestScope(), stripeLiveSK},
			"unknown environment":                     {func() StripeAPIScope { s := live; s.Environment = "PROVIDER_MOCK"; return s }(), stripeLiveRAK},
			"lower-case live":                         {func() StripeAPIScope { s := live; s.Environment = "live"; return s }(), stripeLiveRAK},
		} {
			if _, _, _, err := k.SealStripeAPI(c.scope, StripeAPICredentials{c.key}); !errors.Is(err, errStripeMaterial) {
				t.Fatalf("%s: sealed", name)
			}
		}
	})

	t.Run("webhook scope", func(t *testing.T) {
		live := stripeLiveWebhookScope()
		id, nonce, ct, err := k.SealStripeWebhook(live, StripeWebhookSecrets{CurrentSecret: stripeTestWhsec})
		if err != nil {
			t.Fatalf("LIVE endpoint scope refused: %v", err)
		}
		if _, err := k.OpenStripeWebhook(live, id, nonce, ct); err != nil {
			t.Fatalf("LIVE webhook round trip: %v", err)
		}
		// Same endpoint, other environment/profile: AAD mismatch or scope refusal.
		other := live
		other.Environment, other.Profile = "SANDBOX", "SANDBOX"
		if _, err := k.OpenStripeWebhook(other, id, nonce, ct); !errors.Is(err, errStripeMaterial) {
			t.Fatal("LIVE webhook material opened under SANDBOX")
		}
		for name, mutate := range map[string]func(*StripeWebhookScope){
			"LIVE env with SANDBOX profile":       func(s *StripeWebhookScope) { s.Profile = "SANDBOX" },
			"LIVE env with PROVIDER_MOCK profile": func(s *StripeWebhookScope) { s.Profile = "PROVIDER_MOCK" },
			"SANDBOX env with LIVE profile":       func(s *StripeWebhookScope) { s.Environment = "SANDBOX" },
			"unknown environment":                 func(s *StripeWebhookScope) { s.Environment = "STAGING" },
			"empty profile":                       func(s *StripeWebhookScope) { s.Profile = "" },
		} {
			bad := live
			mutate(&bad)
			if _, _, _, err := k.SealStripeWebhook(bad, StripeWebhookSecrets{CurrentSecret: stripeTestWhsec}); !errors.Is(err, errStripeMaterial) {
				t.Fatalf("%s: sealed", name)
			}
		}
		// SANDBOX profiles are unchanged.
		for _, p := range []string{"PROVIDER_MOCK", "SANDBOX"} {
			ok := stripeWebhookTestScope()
			ok.Profile = p
			if _, _, _, err := k.SealStripeWebhook(ok, StripeWebhookSecrets{CurrentSecret: stripeTestWhsec}); err != nil {
				t.Fatalf("SANDBOX profile %s refused: %v", p, err)
			}
		}
	})
}
