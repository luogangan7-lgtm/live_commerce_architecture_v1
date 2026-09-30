// stripe_live.go owns the API-side gate for Stripe LIVE (contracts/stripe-live-enable-v1.md §5.1, §5.2).
// The API process never dials Stripe and never reads STRIPE_* keys; it only refuses to start a LIVE
// Stripe surface (webhook ingress, buyer Stripe checkout) unless the owner's flag+reference pair is
// present and well-formed. The per-store gate is SQL (approval, REAL_LIVE qualification, method), not this file.

package main

import (
	"livecommerce/internal/integrations/psp/stripe"
	"livecommerce/internal/platform"
)

// loadStripeLiveApproval returns the complete COMMERCE_STRIPE_LIVE_ENABLED=1 +
// COMMERCE_STRIPE_LIVE_APPROVAL_REF pair, or an error when either half is missing, malformed or
// contradictory (both or neither; here "neither" is also an error because callers ask only for LIVE).
// Callers map the error to their own fixed sentinel; the reference is never part of it.
func loadStripeLiveApproval(getenv func(string) string) (stripe.LiveApproval, error) {
	live, err := platform.LoadStripeLiveApproval(getenv)
	if err != nil {
		return stripe.LiveApproval{}, err
	}
	if !live.Valid() {
		return stripe.LiveApproval{}, errStripeConfig
	}
	return live, nil
}
