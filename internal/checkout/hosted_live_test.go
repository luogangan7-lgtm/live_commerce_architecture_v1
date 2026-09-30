// hosted_live_test.go: the checkout-package unit part of stripe-live-enable-v1 §5.2 for the hosted service
// constructor: the Stripe branch admits profile LIVE (it was refused in B1). The pair gate is cmd/api's and the
// per-store gate is SQL, so neither is asserted here. Non-goals: no database, no network, no LIVE call.
// Callers: go test ./internal/checkout.

package checkout

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"

	"livecommerce/internal/command"
)

func TestHostedStripeAdmitsLiveProfile(t *testing.T) {
	jobs := new(river.Client[pgx.Tx])
	stripeConfig := StripeHostedConfig{ReturnURL: "https://pay.example.com/return"}
	for _, profile := range []string{"PROVIDER_MOCK", "SANDBOX", "LIVE"} {
		// A valid Stripe-only request passes argument validation and stops at the pool authority check.
		_, err := NewHostedPaymentService(context.Background(), nil, jobs, profile, nil, HostedProviders{Stripe: &stripeConfig})
		if err == nil || errors.Is(err, command.ErrInvalid) {
			t.Fatalf("%s: valid Stripe config should reach pool validation, got %v", profile, err)
		}
	}
	// Everything else about the constructor boundary still refuses (unknown profile, bad URL).
	for name, err := range map[string]error{
		"unknown profile": func() error {
			_, err := NewHostedPaymentService(context.Background(), nil, jobs, "PROD", nil, HostedProviders{Stripe: &stripeConfig})
			return err
		}(),
		"live with an http return url": func() error {
			_, err := NewHostedPaymentService(context.Background(), nil, jobs, "LIVE", nil,
				HostedProviders{Stripe: &StripeHostedConfig{ReturnURL: "http://x.example.com/r"}})
			return err
		}(),
	} {
		if !errors.Is(err, command.ErrInvalid) {
			t.Fatalf("%s: want ErrInvalid, got %v", name, err)
		}
	}
}
