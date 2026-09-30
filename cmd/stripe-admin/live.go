// live.go: the LIVE subcommands of stripe-admin (contracts/stripe-live-enable-v1.md §3.4, §5.2, ruling S4):
//
//	live-approve --approval --connection --expected-version --currency --approval-ref --approved-at
//	             --canary-max --max --attest   (Stripe readiness read with the STORED key; no key input)
//	live-canary  --approval --attempt --refund (SQL verifies the owner's canary)
//	live-revoke  --approval --revoke-ref       (per-store kill switch)
//
// live-approve and live-canary need the owner's COMMERCE_STRIPE_LIVE_ENABLED + _APPROVAL_REF pair; live-revoke
// never reads it (S8, LD6). Each prints one JSON line of ids and one RFC3339 timestamp; failures print one
// fixed code on stderr. Logic lives in internal/payments/stripeadmin.
//
// Ownership: commerce_worker (carried by stripe-live-core). It never reads STRIPE_* variables (the
// stored key is unsealed with the API keyring), never opens a mock transport and never prints a value it was given.

package main

import (
	"context"
	"io"
	"strings"
	"time"

	"livecommerce/internal/integrations/accounts"
	"livecommerce/internal/integrations/psp/stripe"
	"livecommerce/internal/payments/stripeadmin"
)

func runLive(ctx context.Context, name string, args []string, getenv func(string) string, stdout io.Writer) error {
	c := newCommand(name)
	var approval, connection, currency, approvalRef, approvedAt, attest, attempt, refund, revokeRef string
	var expected, canaryMax, maxMinor int64
	switch name {
	case "live-approve":
		c.fs.StringVar(&approval, "approval", "", "")
		c.fs.StringVar(&connection, "connection", "", "")
		c.fs.Int64Var(&expected, "expected-version", 0, "")
		c.fs.StringVar(&currency, "currency", "", "")
		c.fs.StringVar(&approvalRef, "approval-ref", "", "")
		c.fs.StringVar(&approvedAt, "approved-at", "", "")
		c.fs.Int64Var(&canaryMax, "canary-max", 0, "")
		c.fs.Int64Var(&maxMinor, "max", 0, "")
		c.fs.StringVar(&attest, "attest", "", "")
	case "live-canary":
		c.fs.StringVar(&approval, "approval", "", "")
		c.fs.StringVar(&attempt, "attempt", "", "")
		c.fs.StringVar(&refund, "refund", "", "")
	case "live-revoke":
		c.fs.StringVar(&approval, "approval", "", "")
		c.fs.StringVar(&revokeRef, "revoke-ref", "", "")
	default:
		return errUsage
	}
	if err := c.parse(args); err != nil {
		return err
	}
	var approvedTime time.Time
	if name == "live-approve" {
		var err error
		if approvedTime, err = time.Parse(time.RFC3339, approvedAt); err != nil {
			return errUsage // the message would echo the value; the fixed code does not
		}
	}
	dsn := getenv("COMMERCE_STRIPE_REGISTRAR_DATABASE_URL")
	if strings.TrimSpace(dsn) == "" {
		return errConfig
	}
	// Validate everything the operation needs from the environment before any connection opens.
	var live stripe.LiveApproval
	var apiKeys *accounts.Keyring
	var err error
	if name != "live-revoke" { // live-revoke never reads the pair or any keyring (kill switch, LD6)
		if live, err = requireLivePair(getenv); err != nil {
			return err
		}
	}
	if name == "live-approve" {
		// The stored rk_live_ key is unsealed with the API keyring, exactly like SANDBOX qualify (S4).
		if apiKeys, err = apiKeyring(getenv); err != nil {
			return err
		}
	}
	reg, err := openRegistrar(ctx, dsn, apiKeys, nil, live)
	if err != nil {
		return err
	}
	defer reg.Close()
	switch name {
	case "live-approve":
		id, err := reg.LiveApprove(ctx, c.scope, stripeadmin.LiveApproveInput{ApprovalID: approval,
			ConnectionID: connection, Currency: currency, ApprovalRef: approvalRef, ApprovedAt: approvedTime,
			ExpectedVersion: expected, CanaryMaxMinor: canaryMax, MaxMinor: maxMinor, Checklist: splitList(attest)})
		if err != nil {
			return err
		}
		return emit(stdout, map[string]any{"approval_id": id})
	case "live-canary":
		at, err := reg.LiveCanary(ctx, c.scope, approval, attempt, refund)
		if err != nil {
			return err
		}
		return emit(stdout, map[string]any{"approval_id": approval, "canary_verified_at": at.UTC().Format(time.RFC3339)})
	default: // live-revoke
		at, err := reg.LiveRevoke(ctx, c.scope, approval, revokeRef)
		if err != nil {
			return err
		}
		return emit(stdout, map[string]any{"approval_id": approval, "revoked_at": at.UTC().Format(time.RFC3339)})
	}
}

// splitList turns a comma list into codes; empty input is no codes (the registrar then refuses the checklist).
func splitList(s string) []string {
	if s == "" {
		return nil
	}
	return strings.Split(s, ",")
}
