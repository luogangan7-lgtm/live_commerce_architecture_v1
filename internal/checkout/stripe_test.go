package checkout

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"

	"livecommerce/internal/command"
)

// UNIT only: SQL/River behavior of these paths is covered by the REAL_PG gates of
// stripe-b1-tests-b; nothing here claims SP07/SP14.

func TestStripeHostedConfigDigestIsFrozenShape(t *testing.T) {
	config := StripeHostedConfig{ReturnURL: "https://pay.example.com/payment/return"}
	canonical, digest, err := config.CanonicalDigest()
	if err != nil || canonical != config {
		t.Fatalf("valid config rejected: %v", err)
	}
	// Independent spelling of contracts/stripe-psp-v1.md §9.2 (key order is part of the contract).
	want := sha256.Sum256([]byte(`{"version":"stripe-hosted-v1","return_url":"https://pay.example.com/payment/return",` +
		`"api_version":"2026-08-26.dahlia","session_ttl_seconds":2400,"send_window_seconds":420,"handoff_margin_seconds":300}`))
	if digest != want {
		t.Fatal("stripe config digest drifted from the frozen JSON shape")
	}
	_, other, _ := StripeHostedConfig{ReturnURL: "https://pay.example.com/other"}.CanonicalDigest()
	if other == digest {
		t.Fatal("return url is not bound into the digest")
	}
	for _, bad := range []string{"", "http://pay.example.com/r", "https://user@pay.example.com/r", "https://pay.example.com/r#f",
		"https://pay.example.com/{CHECKOUT_SESSION_ID}", "https://pay.example.com/a b", "/relative",
		"https://pay.example.com/" + strings.Repeat("a", 2048)} {
		if _, _, err := (StripeHostedConfig{ReturnURL: bad}).CanonicalDigest(); !errors.Is(err, command.ErrInvalid) {
			t.Fatalf("unsafe return url %q accepted: %v", bad, err)
		}
	}
}

func TestHostedProvidersConstructorBoundary(t *testing.T) {
	ctx := context.Background()
	jobs := new(river.Client[pgx.Tx])
	payuni := HostedConfig{ReturnURL: "https://pay.example.com/return", NotifyURL: "https://pay.example.com/notify"}
	stripeConfig := StripeHostedConfig{ReturnURL: "https://pay.example.com/return"}
	build := func(c context.Context, j *river.Client[pgx.Tx], profile string, p HostedProviders) error {
		_, err := NewHostedPaymentService(c, nil, j, profile, nil, p)
		return err
	}
	for name, err := range map[string]error{
		"no provider":         build(ctx, jobs, "SANDBOX", HostedProviders{}),
		"nil ctx":             build(nil, jobs, "SANDBOX", HostedProviders{Stripe: &stripeConfig}), //nolint:staticcheck // nil ctx is the case under test
		"nil jobs":            build(ctx, nil, "SANDBOX", HostedProviders{Stripe: &stripeConfig}),
		"payuni without keys": build(ctx, jobs, "SANDBOX", HostedProviders{PAYUNi: &payuni}),
		"bad stripe url":      build(ctx, jobs, "SANDBOX", HostedProviders{Stripe: &StripeHostedConfig{ReturnURL: "http://x.example.com/r"}}),
		"bad profile":         build(ctx, jobs, "PROD", HostedProviders{Stripe: &stripeConfig}),
	} {
		if !errors.Is(err, command.ErrInvalid) {
			t.Fatalf("%s: want ErrInvalid, got %v", name, err)
		}
	}
	// A valid Stripe-only request passes argument validation and stops at the pool authority check.
	if err := build(ctx, jobs, "PROVIDER_MOCK", HostedProviders{Stripe: &stripeConfig}); err == nil || errors.Is(err, command.ErrInvalid) {
		t.Fatalf("valid Stripe config should reach pool validation, got %v", err)
	}
}

func TestStripeHandoffValidatorAndRedirectShape(t *testing.T) {
	cutoff := time.Now().Add(time.Minute)
	redirect := HostedHandoff{OrderID: testID, Disposition: "REDIRECT", ExpiresAt: cutoff,
		RedirectURL: "https://checkout.stripe.com/c/pay/cs_test_a1B2#fid"}
	if !validStripeHandoff(redirect, testID) {
		t.Fatal("valid redirect rejected")
	}
	for _, edit := range []func(*HostedHandoff){
		func(h *HostedHandoff) { h.RedirectURL = "" },
		func(h *HostedHandoff) { h.RedirectURL = "http://checkout.stripe.com/c/pay/x" },
		func(h *HostedHandoff) { h.RedirectURL = "https://checkout.stripe.com.evil.example/c/pay/x" },
		func(h *HostedHandoff) { h.RedirectURL = "https://evil.example/https://checkout.stripe.com/x" },
		func(h *HostedHandoff) { h.RedirectURL = "https://checkout.stripe.com/" },
		func(h *HostedHandoff) { h.RedirectURL = "https://checkout.stripe.com/a b" },
		func(h *HostedHandoff) { h.RedirectURL = "https://checkout.stripe.com/" + strings.Repeat("a", 4001) },
		func(h *HostedHandoff) { h.Form = &HostedForm{} },
		func(h *HostedHandoff) { h.OrderID = "00000000-0000-0000-0000-000000000002" },
		func(h *HostedHandoff) { h.ExpiresAt = time.Time{} },
		func(h *HostedHandoff) { h.Disposition = "ISSUED" },
	} {
		bad := redirect
		edit(&bad)
		if validStripeHandoff(bad, testID) {
			t.Fatalf("invalid stripe handoff accepted: %+v", bad)
		}
	}
	for _, disposition := range []string{"CREATING", "CLOSED", "UNAVAILABLE"} {
		none := HostedHandoff{OrderID: testID, Disposition: disposition, ExpiresAt: cutoff}
		if !validStripeHandoff(none, testID) {
			t.Fatalf("%s without url rejected", disposition)
		}
		none.RedirectURL = redirect.RedirectURL
		if validStripeHandoff(none, testID) {
			t.Fatalf("%s carried a url", disposition)
		}
	}
	// PAYUNi handoffs may never carry a Stripe URL, and their bytes stay free of the new key.
	payuni := HostedHandoff{OrderID: testID, Disposition: "ALREADY_ISSUED", ExpiresAt: cutoff, RedirectURL: redirect.RedirectURL}
	if validHostedHandoff(payuni, testID, "SANDBOX") {
		t.Fatal("payuni handoff accepted a redirect url")
	}
	payuni.RedirectURL = ""
	body, err := json.Marshal(payuni)
	if err != nil || strings.Contains(string(body), "redirect_url") {
		t.Fatalf("payuni handoff bytes changed: %s %v", body, err)
	}
}

func TestStripePaymentResultUsesClosedCurrencyTable(t *testing.T) {
	ok := PaymentResult{OrderID: testID, AttemptID: testID, OperationID: testID, JobID: 7, Generation: 2,
		MerchantTradeNo: "Pabc", Currency: "HKD", AmountMinor: 400, State: "PAYMENT_PENDING"}
	if !validStripePaymentResult(ok, testID) {
		t.Fatal("valid HKD result rejected")
	}
	for _, edit := range []func(*PaymentResult){
		func(r *PaymentResult) { r.Currency = "JPY" },
		func(r *PaymentResult) { r.Currency = "hkd" },
		func(r *PaymentResult) { r.AmountMinor = 399 },
		func(r *PaymentResult) { r.Currency, r.AmountMinor = "TWD", 150 },
		func(r *PaymentResult) { r.State = "PAID" },
		func(r *PaymentResult) { r.OperationID = "00000000-0000-0000-0000-000000000009" },
		func(r *PaymentResult) { r.Generation = 3 },
		func(r *PaymentResult) { r.JobID = 0 },
	} {
		bad := ok
		edit(&bad)
		if validStripePaymentResult(bad, testID) {
			t.Fatalf("invalid stripe result accepted: %+v", bad)
		}
	}
	if validPaymentResult(ok, testID) {
		t.Fatal("PAYUNi receipt validator started admitting non-TWD results")
	}
	for _, in := range []HostedInput{
		{OrderID: testID, MethodCode: "stripe_checkout", MethodVersion: 1, Locale: "en"},
		{OrderID: testID, MethodCode: "payuni_credit", MethodVersion: 1, Locale: "zh-TW"},
	} {
		if !validHostedInput(in) {
			t.Fatalf("valid hosted input rejected: %+v", in)
		}
	}
	for _, in := range []HostedInput{
		{OrderID: testID, MethodCode: "stripe", MethodVersion: 1, Locale: "en"},
		{OrderID: testID, MethodCode: "stripe_checkout", MethodVersion: 0, Locale: "en"},
		{OrderID: testID, MethodCode: "stripe_checkout", MethodVersion: 1, Locale: "fr"},
	} {
		if validHostedInput(in) {
			t.Fatalf("invalid hosted input accepted: %+v", in)
		}
	}
}

func TestPaymentViewV2Validator(t *testing.T) {
	yes := true
	future := time.Now().Add(time.Minute)
	method := func(code string) PaymentMethodOption {
		return PaymentMethodOption{Code: code, Version: 1, NameHans: "a", NameHant: "b", NameEN: "c"}
	}
	base := OrderPayment{OrderID: testID, Currency: "HKD", TotalMinor: 500, CommercialState: "DRAFT",
		PaymentState: "NOT_STARTED", HandoffState: "NONE", Methods: []PaymentMethodOption{method("payuni_credit"), method("stripe_checkout")}}
	base.CancelRequested = &yes
	if !validPaymentViewFor(base, testID, true) {
		t.Fatal("two-method v2 view rejected")
	}
	if validPaymentViewFor(base, testID, false) {
		t.Fatal("v1 accepted cancel_requested and two methods")
	}
	v1 := base
	v1.CancelRequested = nil
	v1.Methods = v1.Methods[:1]
	if !validPaymentView(v1, testID) || validPaymentViewFor(v1, testID, true) {
		t.Fatal("v1 view must stay valid without cancel_requested and be refused by v2")
	}
	dup := base
	dup.Methods = []PaymentMethodOption{method("stripe_checkout"), method("stripe_checkout")}
	unknown := base
	unknown.Methods = []PaymentMethodOption{method("payuni_credit"), method("payuni_atm")}
	three := base
	three.Methods = append(append([]PaymentMethodOption{}, base.Methods...), method("payuni_credit"))
	for name, bad := range map[string]OrderPayment{"duplicate method": dup, "unknown method": unknown, "three methods": three} {
		if validPaymentViewFor(bad, testID, true) {
			t.Fatalf("%s accepted", name)
		}
	}
	for _, state := range []string{"CREATING", "READY", "CLOSED"} {
		view := base
		view.Methods = []PaymentMethodOption{}
		view.CommercialState, view.PaymentState, view.HandoffState, view.HandoffExpiresAt = "AWAITING_PAYMENT", "PENDING", state, &future
		if !validPaymentViewFor(view, testID, true) {
			t.Fatalf("stripe handoff state %s rejected by v2", state)
		}
		v1state := view
		v1state.CancelRequested = nil
		if validPaymentViewFor(v1state, testID, false) {
			t.Fatalf("stripe handoff state %s accepted by v1", state)
		}
		view.HandoffExpiresAt = nil
		if validPaymentViewFor(view, testID, true) {
			t.Fatalf("stripe handoff state %s without cutoff accepted", state)
		}
	}
	closed := base
	closed.Methods, closed.CommercialState, closed.PaymentState = []PaymentMethodOption{}, "CANCELLED", "CLOSED_UNPAID"
	if !validPaymentViewFor(closed, testID, true) {
		t.Fatal("CLOSED_UNPAID rejected by v2")
	}
	closed.CancelRequested = nil
	if validPaymentViewFor(closed, testID, false) {
		t.Fatal("CLOSED_UNPAID accepted by v1")
	}
	// PAYUNi-only bytes: the new field is omitted while nil.
	body, _ := json.Marshal(v1)
	if strings.Contains(string(body), "cancel_requested") {
		t.Fatalf("nil cancel_requested leaked into PAYUNi bytes: %s", body)
	}
}

// Rulings §4 / SP14: a PAYUNi order projected by v2 must marshal to the same bytes as v1.
func TestPaymentViewV2PAYUNiBytesMatchV1(t *testing.T) {
	const v1JSON = `{"order_id":"` + testID + `","currency":"TWD","total_minor":500,"commercial_state":"AWAITING_PAYMENT","test_mode":true,` +
		`"payment_state":"PENDING","handoff_state":"NONE","handoff_expires_at":null,"methods":[]}`
	var v1, v2 OrderPayment
	if err := json.Unmarshal([]byte(v1JSON), &v1); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(strings.Replace(v1JSON, `"methods":[]`, `"methods":[],"cancel_requested":false`, 1)), &v2); err != nil {
		t.Fatal(err)
	}
	if !validPaymentViewFor(v2, testID, true) {
		t.Fatal("v2 projection with cancel_requested rejected")
	}
	dropCancelUnlessStripe(&v2, false)
	want, _ := json.Marshal(v1)
	got, _ := json.Marshal(v2)
	if !bytes.Equal(want, got) {
		t.Fatalf("PAYUNi bytes differ under Stripe: %s vs %s", got, want)
	}
	no := false
	stripeView := OrderPayment{CancelRequested: &no}
	dropCancelUnlessStripe(&stripeView, true)
	if stripeView.CancelRequested == nil {
		t.Fatal("Stripe order lost cancel_requested")
	}
}

func TestSignalRequestsFailClosedWithoutStripe(t *testing.T) {
	ctx := context.Background()
	for name, call := range map[string]func(*HostedPaymentStarter) (PaymentSignal, error){
		"refresh": func(s *HostedPaymentStarter) (PaymentSignal, error) {
			return s.RefreshPayment(ctx, "t", testID, testID)
		},
		"cancel": func(s *HostedPaymentStarter) (PaymentSignal, error) { return s.CancelPayment(ctx, "t", testID, testID) },
	} {
		if _, err := call(&HostedPaymentStarter{}); !errors.Is(err, command.ErrNotFound) {
			t.Fatalf("%s on a PAYUNi-only service: want ErrNotFound, got %v", name, err)
		}
		if _, err := call(nil); !errors.Is(err, command.ErrInvalid) {
			t.Fatalf("%s on nil service: want ErrInvalid, got %v", name, err)
		}
		if _, err := call(&HostedPaymentStarter{stripe: &StripeHostedConfig{}}); !errors.Is(err, command.ErrInvalid) {
			t.Fatalf("%s without pool: want ErrInvalid, got %v", name, err)
		}
	}
	if b, _ := json.Marshal(PaymentSignal{OrderID: testID, Scheduled: false}); string(b) != `{"order_id":"`+testID+`","scheduled":false}` {
		t.Fatalf("signal response shape drifted: %s", b)
	}
	if b, _ := json.Marshal(paymentSignalArgs{OperationID: testID, SignalID: testID, Version: 1}); string(b) !=
		`{"operation_id":"`+testID+`","signal_id":"`+testID+`","version":1}` || (paymentSignalArgs{}).Kind() != "payment_signal_v1" {
		t.Fatalf("signal job args drifted: %s", b)
	}
	// Stripe begin on a service without Stripe must not fall through to the PAYUNi path.
	if _, err := (&HostedPaymentStarter{}).beginStripe(ctx, "t", testID, "key-000001", HostedInput{}); !errors.Is(err, command.ErrInvalid) {
		t.Fatalf("beginStripe without Stripe: %v", err)
	}
}
