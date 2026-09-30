package payments

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"livecommerce/internal/integrations/accounts"
	"livecommerce/internal/integrations/psp/stripe"
)

const (
	stripeTestAttempt = "0b5e3c1a-7d2f-4e8a-9c61-2f4d8e7a1b30"
	stripeTestOrder   = "9f1c2e4d-5a6b-4c7d-8e9f-0a1b2c3d4e5f"
)

func testStripeFields() map[string]string {
	return map[string]string{
		"adaptive_pricing[enabled]": "false", "managed_payments[enabled]": "false",
		"automatic_tax[enabled]": "false", "cancel_url": "https://shop.example.test/payment/return",
		"success_url": "https://shop.example.test/payment/return", "client_reference_id": stripeTestAttempt,
		"expires_at": "1790000000", "line_items[0][price_data][currency]": "hkd",
		"line_items[0][price_data][product_data][name]": "Order 9F1C2E4D",
		"line_items[0][price_data][unit_amount]":        "12345", "line_items[0][quantity]": "1",
		"locale": "zh-TW", "metadata[lc_attempt]": stripeTestAttempt,
		"metadata[lc_order]": stripeTestOrder, "metadata[lc_profile]": "SANDBOX",
		"metadata[lc_v]": "1", "mode": "payment",
		"payment_intent_data[metadata][lc_attempt]": stripeTestAttempt,
		"payment_intent_data[metadata][lc_order]":   stripeTestOrder,
		"payment_method_types[0]":                   "card", "submit_type": "pay", "ui_mode": "hosted_page",
	}
}

func testStripeSnapshot() stripeSessionSnapshot {
	expires := time.Unix(1790000000, 0).UTC()
	return stripeSessionSnapshot{
		TenantID: stripeTestOrder, StoreID: stripeTestOrder, AttemptID: stripeTestAttempt,
		ConnectionID: stripeTestOrder, CredentialVersion: 2, Environment: "SANDBOX",
		AccountID: "acct_1FakeAccount000", Currency: "HKD", Profile: "SANDBOX",
		AmountMinor: 12345, UnitAmount: 12345, CreateParams: testStripeFields(),
		ExpiresAt: expires, SendDeadline: expires.Add(-33 * time.Minute),
		HandoffCutoff: expires.Add(-5 * time.Minute),
		DBNow:         expires.Add(-39 * time.Minute),
	}
}

func TestStripeRuntimeRejectsInvalidConfigurationBeforeDatabase(t *testing.T) {
	keys := &accounts.Keyring{}
	transport := mockQueryTransport(func(*http.Request) (*http.Response, error) {
		t.Fatal("constructor must not call provider")
		return nil, nil
	})
	for _, tc := range []struct {
		name, profile string
		keys          *accounts.Keyring
		transports    []http.RoundTripper
	}{
		{"live", "LIVE", keys, nil},
		{"mock without transport", "PROVIDER_MOCK", keys, nil},
		{"mock nil transport", "PROVIDER_MOCK", keys, []http.RoundTripper{nil}},
		{"mock multiple transports", "PROVIDER_MOCK", keys, []http.RoundTripper{transport, transport}},
		{"sandbox with transport", "SANDBOX", keys, []http.RoundTripper{transport}},
		{"nil keys", "SANDBOX", nil, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := NewStripeRuntime(context.Background(), nil, tc.keys, tc.profile, tc.transports...)
			if !errors.Is(err, errStripeRuntimeConfig) {
				t.Fatalf("error = %v", err)
			}
		})
	}
}

func TestStripeSnapshotRejectsScopeAndBodyDrift(t *testing.T) {
	s := testStripeSnapshot()
	if !validStripeSnapshot(s, stripeTestAttempt, "SANDBOX") {
		t.Fatal("valid frozen snapshot rejected")
	}
	for name, mutate := range map[string]func(*stripeSessionSnapshot){
		"attempt":             func(s *stripeSessionSnapshot) { s.AttemptID = stripeTestOrder },
		"tenant":              func(s *stripeSessionSnapshot) { s.TenantID = "" },
		"store":               func(s *stripeSessionSnapshot) { s.StoreID = "" },
		"connection":          func(s *stripeSessionSnapshot) { s.ConnectionID = "" },
		"credential version":  func(s *stripeSessionSnapshot) { s.CredentialVersion = 0 },
		"environment":         func(s *stripeSessionSnapshot) { s.Environment = "REAL_LIVE" },
		"profile":             func(s *stripeSessionSnapshot) { s.Profile = "PROVIDER_MOCK" },
		"body attempt":        func(s *stripeSessionSnapshot) { s.CreateParams["metadata[lc_attempt]"] = stripeTestOrder },
		"body account amount": func(s *stripeSessionSnapshot) { s.CreateParams["line_items[0][price_data][unit_amount]"] = "-1" },
	} {
		t.Run(name, func(t *testing.T) {
			changed := testStripeSnapshot()
			mutate(&changed)
			if validStripeSnapshot(changed, stripeTestAttempt, "SANDBOX") {
				t.Fatal("drift accepted")
			}
		})
	}
}

func TestStripeSessionIdentityAndBoundedRateDelay(t *testing.T) {
	s := testStripeSnapshot()
	expires := s.ExpiresAt.Unix()
	session := stripe.Session{ID: "cs_test_fake", ClientReferenceID: s.AttemptID,
		MetadataAttempt: s.AttemptID, MetadataProfile: s.Profile, ExpiresAt: &expires,
		Mode: "payment", PaymentMethodTypes: []string{"card"}}
	if !stripeSessionIdentity(session, s) {
		t.Fatal("valid identity rejected")
	}
	session.MetadataAttempt = stripeTestOrder
	if stripeSessionIdentity(session, s) {
		t.Fatal("foreign attempt accepted")
	}
	session.MetadataAttempt = s.AttemptID
	session.Livemode = true
	if stripeSessionIdentity(session, s) {
		t.Fatal("livemode accepted")
	}
	for _, generation := range []int64{-1, 1, 3, 6, 1000} {
		for i := 0; i < 100; i++ {
			d := stripeRateDelay(generation)
			if d < 2*time.Second || d > time.Minute {
				t.Fatalf("generation %d delay %s", generation, d)
			}
		}
	}
}
