// live.go: the three LIVE registrar steps of contracts/stripe-live-enable-v1.md §3.4 / §5.2:
// LiveApprove (Stripe readiness read + payments.approve_stripe_live), LiveCanary
// (payments.record_stripe_live_canary) and LiveRevoke (payments.revoke_stripe_live, the per-store kill switch).
// Ordering rule as registrar.go: validate input, then provider read, then exactly one SQL call.
//
// Ownership: commerce_worker (carried by stripe-live-core). Touches: api.stripe.com GET /v1/account
// (LiveApprove only, through stripe.Client.AccountReadiness) and the three registry definers. None of the
// three moves money or takes a key as input: LiveApprove unseals the STORED rk_live_ key like SANDBOX
// Qualify does (S4 pattern). Errors stay ErrConfig / ErrDatabase / ErrRejected / ErrProvider.

package stripeadmin

import (
	"context"
	"errors"
	"slices"
	"time"

	"livecommerce/internal/command"
	"livecommerce/internal/integrations/psp/stripe"
)

// LiveChecklist is the exact §9 attestation code set stored in payments.stripe_live_approvals.checklist,
// sorted in byte order. live-approve refuses a missing, extra or duplicate code.
var LiveChecklist = []string{"account_active", "canary_private", "descriptor", "dispute_notice", "managed_off",
	"payout_bank", "policy_pages", "radar_default", "rak_live", "three_ds", "webhook_live"}

// LiveApproveInput is the operator's attested approval. ApprovedBy is the Scope principal (--principal).
// ExpectedVersion is the credential version whose STORED key reads the account (no key input).
type LiveApproveInput struct {
	ApprovalID, ConnectionID, Currency, ApprovalRef string
	ApprovedAt                                      time.Time
	ExpectedVersion, CanaryMaxMinor, MaxMinor       int64
	Checklist                                       []string
}

func validChecklist(codes []string) bool {
	sorted := slices.Clone(codes)
	slices.Sort(sorted)
	return slices.Equal(sorted, LiveChecklist) // sorted copy equal => no missing, extra or duplicate code
}

// LiveApprove reads GET /v1/account with the stored key (OpenLive only), projects the L12 readiness fields and
// calls payments.approve_stripe_live, which re-checks everything in SQL (grant predicate, readiness gate, caps,
// checklist). A missing or null Stripe field fails closed as ErrRejected (LD8), never as 0/false.
func (r *Registrar) LiveApprove(ctx context.Context, s Scope, in LiveApproveInput) (string, error) {
	if r == nil || r.db == nil || ctx == nil || r.apiKeys == nil || !r.live.Enabled || !validScope(s) ||
		!command.ValidID(in.ApprovalID) || !command.ValidID(in.ConnectionID) || in.ExpectedVersion < 1 {
		return "", ErrConfig
	}
	// Operator content the definer would also refuse (22023): refuse early with the same class (SetMethod rule).
	// I05: the caps are minor-unit integers ordered canary <= max; the closed minimum table, the 2x-minimum canary
	// bound and the TWD 2,000,000 per-order max (LQ3) are enforced only in SQL (stripe_min_minor, approve_stripe_live).
	if !currencyPattern.MatchString(in.Currency) || !refPattern.MatchString(in.ApprovalRef) || in.ApprovedAt.IsZero() ||
		in.CanaryMaxMinor < 1 || in.MaxMinor < in.CanaryMaxMinor || !validChecklist(in.Checklist) {
		return "", ErrRejected
	}
	bounded, cancel := context.WithTimeout(ctx, qualifyBudget)
	defer cancel()
	account, secret, err := r.storedCredential(bounded, s, in.ConnectionID, in.ExpectedVersion)
	if err != nil {
		return "", err
	}
	client, err := r.providerClient(secret, account)
	if err != nil {
		return "", err
	}
	readiness, _, err := client.AccountReadiness(bounded)
	switch {
	case errors.Is(err, stripe.ErrAuthentication):
		return "", ErrRejected // the key belongs to another account: nothing may be written
	case err != nil:
		// A transport failure or malformed body proves nothing; the operator simply reruns (a read, no state).
		return "", ErrProvider
	}
	readinessJSON, err := readiness.JSON()
	if err != nil {
		return "", ErrRejected // stripe_live_readiness_unknown: an absent field never passes (LD8)
	}
	var out string
	// payments.approve_stripe_live: LD2 owner grant predicate + LD8 readiness + LQ3 caps + §9 checklist, one audit row.
	if err := r.scan(bounded, &out, `SELECT payments.approve_stripe_live($1::uuid,$2::uuid,$3::uuid,$4::uuid,$5::uuid,
		$6::text,$7::text,$8::timestamptz,$9::bigint,$10::bigint,$11::text[],$12::jsonb)::text`,
		s.TenantID, s.StoreID, s.PrincipalID, in.ApprovalID, in.ConnectionID, in.Currency, in.ApprovalRef,
		in.ApprovedAt.UTC(), in.CanaryMaxMinor, in.MaxMinor, in.Checklist, string(readinessJSON)); err != nil {
		return "", err
	}
	if out != in.ApprovalID {
		return "", ErrDatabase
	}
	return out, nil
}

// LiveCanary asks SQL to verify the owner's canary (a captured LIVE payment, fully refunded, with webhook
// receipts) and to set the canary triple once (payments.record_stripe_live_canary). OpenLive only: it is part of
// the owner-approved LIVE flow. It returns the stored verification time; an identical replay returns the same time.
func (r *Registrar) LiveCanary(ctx context.Context, s Scope, approvalID, attemptID, refundID string) (time.Time, error) {
	if r == nil || r.db == nil || ctx == nil || !r.live.Enabled || !validScope(s) || !command.ValidID(approvalID) ||
		!command.ValidID(attemptID) || !command.ValidID(refundID) {
		return time.Time{}, ErrConfig
	}
	var verified time.Time
	// payments.record_stripe_live_canary: read-only checks on facts/refunds/receipts, then the set-once triple.
	if err := r.scan(ctx, &verified, `SELECT payments.record_stripe_live_canary($1::uuid,$2::uuid,$3::uuid,$4::uuid,
		$5::uuid,$6::uuid)`, s.TenantID, s.StoreID, s.PrincipalID, approvalID, attemptID, refundID); err != nil {
		return time.Time{}, err
	}
	return verified, nil
}

// LiveRevoke is the per-store kill switch (LD6, S8): revoke the approval and its LIVE qualifications so the next
// payment start is PT409. It works on a registrar from Open OR OpenLive and never needs the flag+reference pair:
// stopping new starts must not depend on deploy env or on the owner. In-flight reconcile and refunds are unaffected.
func (r *Registrar) LiveRevoke(ctx context.Context, s Scope, approvalID, revokeRef string) (time.Time, error) {
	if r == nil || r.db == nil || ctx == nil || !validScope(s) || !command.ValidID(approvalID) {
		return time.Time{}, ErrConfig
	}
	if !refPattern.MatchString(revokeRef) {
		return time.Time{}, ErrRejected // the definer answers 22023; keep the class without the round trip
	}
	var revoked time.Time
	// payments.revoke_stripe_live: approval FOR UPDATE, then revoke its qualifications in a separate statement.
	if err := r.scan(ctx, &revoked, `SELECT payments.revoke_stripe_live($1::uuid,$2::uuid,$3::uuid,$4::uuid,$5::text)`,
		s.TenantID, s.StoreID, s.PrincipalID, approvalID, revokeRef); err != nil {
		return time.Time{}, err
	}
	return revoked, nil
}
