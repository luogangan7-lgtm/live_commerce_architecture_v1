// Package stripe owns Stripe Checkout Session wire calls and webhook signature verification.
// It never reads PG, decides payment state, releases stock or retries on its own.
//
// Ownership: integration_worker (contracts/stripe-psp-v1.md §5, §15).
// Non-goals: order/payment state, SQL, River jobs, Connect, PaymentIntents outside
// Checkout, Stripe CLI support, and deciding whether a store may go LIVE (that is SQL:
// contracts/stripe-live-enable-v1.md §3; this package only admits a key/environment pair).
// Dependencies: standard library only (§5.1). stripe-go is deliberately not used so the
// exact wire bytes, headers and classification stay reviewable in this package.
// Callers: the payment worker (create/retrieve/expire/find), the API webhook
// handler (WebhookVerifier only) and cmd/stripe-admin (VerifyAccount, AccountReadiness).
// This is the only package in the repository allowed to dial api.stripe.com (SP20);
// GET /v1/account is also read for LIVE readiness (readiness.go, stripe-live-enable-v1 LD8).
//
// This file holds the error vocabulary, Config and key admission (§5.2).
package stripe

import (
	"errors"
	"regexp"
	"strings"
)

// APIVersion is sent as Stripe-Version on every request. Changing it requires a
// contract amendment and a re-run of SP16/SP18.
// https://docs.stripe.com/api-versions (retrieved 2026-09-28, F9).
const APIVersion = "2026-08-26.dahlia"

// Errors are fixed sentinels. Wrapped errors add only a fixed refusal code; they
// never carry Stripe error messages, raw bodies or key material (§12).
var (
	// ErrInvalid: caller input or configuration violates the contract; nothing was sent.
	ErrInvalid = errors.New("stripe: invalid input")
	// ErrLiveRefused: a live key or LIVE environment lacks the explicit owner approval.
	ErrLiveRefused = errors.New("stripe: live refused")
	// ErrSignature: a webhook signature header failed any §5.8 rule.
	ErrSignature = errors.New("stripe: signature rejected")
	// ErrRejected: Stripe definitively rejected the request (400 invalid_request_error or 404 on create).
	ErrRejected = errors.New("stripe: request rejected")
	// ErrAuthentication: 401/403, or GET /v1/account returned another account.
	ErrAuthentication = errors.New("stripe: authentication failure")
	// ErrIdempotency: 400 idempotency_error; frozen params changed under one key (a bug).
	ErrIdempotency = errors.New("stripe: idempotency mismatch")
	// ErrConflict: 409, a concurrent request with the same key; retry the same key later.
	ErrConflict = errors.New("stripe: concurrent conflict")
	// ErrRateLimited: 429; retry the same key with backoff.
	ErrRateLimited = errors.New("stripe: rate limited")
	// ErrNotOpen: expire returned 400 invalid_request_error; the caller must retrieve.
	ErrNotOpen = errors.New("stripe: session not open")
	// ErrUncertain: the outcome is unknown; never evidence of "not paid" or "not created".
	ErrUncertain = errors.New("stripe: outcome uncertain")
)

// coded wraps a sentinel with a fixed, grep-able refusal code.
type coded struct {
	code string
	err  error
}

func (e *coded) Error() string { return "stripe: " + e.code }
func (e *coded) Unwrap() error { return e.err }

func refuse(sentinel error, code string) error { return &coded{code: code, err: sentinel} }

const (
	envSandbox = "SANDBOX"
	envLive    = "LIVE"
)

var (
	// §5.2 key syntax; restricted keys (rk_) are recommended.
	// https://docs.stripe.com/keys/restricted-api-keys (retrieved 2026-09-28, F11).
	keyPattern = regexp.MustCompile(`^(sk|rk)_(test|live)_[A-Za-z0-9]{16,240}$`)
	// §12 STRIPE_ACCOUNT_ID.
	accountPattern = regexp.MustCompile(`^acct_[A-Za-z0-9]{1,59}$`)
	// §5.2 COMMERCE_STRIPE_LIVE_APPROVAL_REF.
	approvalPattern = regexp.MustCompile(`^[A-Za-z0-9._:-]{8,128}$`)
)

// Config is the only way to hand a Stripe key to this package. It is redacted in
// every formatting path so it can never leak through logs or JSON.
type Config struct {
	SecretKey   string       // sk_/rk_ test or live key; never logged or serialized
	AccountID   string       // acct_…; must equal GET /v1/account id
	Environment string       // "SANDBOX" | "LIVE" (PROVIDER_MOCK uses SANDBOX)
	Live        LiveApproval // zero value unless Environment=="LIVE"
}

// LiveApproval mirrors COMMERCE_STRIPE_LIVE_ENABLED and COMMERCE_STRIPE_LIVE_APPROVAL_REF.
// Both or neither must be set (§12).
type LiveApproval struct {
	Enabled   bool
	Reference string
}

// Valid reports whether the pair is a complete owner approval: the flag is on and the reference has
// the §5.2 shape. Constructors of LIVE-capable components (payments.NewLiveStripeRuntime,
// stripeadmin.OpenLive) call it to fail fast; admit() re-checks it on every client, so this is never
// the only gate.
func (a LiveApproval) Valid() bool { return a.Enabled && approvalPattern.MatchString(a.Reference) }

func (Config) String() string               { return "stripe.Config{redacted}" }
func (c Config) GoString() string           { return c.String() }
func (Config) MarshalJSON() ([]byte, error) { return []byte(`"stripe.Config{redacted}"`), nil }

// admit applies the §5.2 matrix. mock=true is the PROVIDER_MOCK path, which only
// ever admits test keys. Returned errors carry only a fixed code.
func admit(cfg Config, mock bool) error {
	if cfg.Environment != envSandbox && cfg.Environment != envLive {
		return refuse(ErrInvalid, "stripe_environment_invalid")
	}
	if !accountPattern.MatchString(cfg.AccountID) {
		return refuse(ErrInvalid, "stripe_account_invalid")
	}
	// No surrounding whitespace: the regex is anchored and has no space class.
	if !keyPattern.MatchString(cfg.SecretKey) {
		return refuse(ErrInvalid, "stripe_key_invalid")
	}
	liveKey := strings.HasPrefix(cfg.SecretKey, "sk_live_") || strings.HasPrefix(cfg.SecretKey, "rk_live_")
	approvalPresent := cfg.Live.Enabled || cfg.Live.Reference != ""
	if !liveKey {
		// Test key: only SANDBOX, and a live flag or reference is itself a mismatch.
		if cfg.Environment != envSandbox || approvalPresent {
			return refuse(ErrLiveRefused, "stripe_key_mode_mismatch")
		}
		return nil
	}
	// Live key: never on the mock transport and never in SANDBOX.
	if mock || cfg.Environment != envLive {
		return refuse(ErrLiveRefused, "stripe_key_mode_mismatch")
	}
	// LD3/LR-3: only a restricted key (rk_live_) is admitted; an unrestricted secret key is
	// refused before the flag+ref pair is even looked at, so no approval can legitimise it.
	// https://docs.stripe.com/keys/restricted-api-keys (retrieved 2026-09-29, L3).
	if strings.HasPrefix(cfg.SecretKey, "sk_live_") {
		return refuse(ErrLiveRefused, "stripe_live_key_unrestricted")
	}
	if !cfg.Live.Valid() {
		return refuse(ErrLiveRefused, "stripe_live_refused")
	}
	return nil
}
