package checkout

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"livecommerce/internal/command"
)

func TestPaymentViewBoundedShape(t *testing.T) {
	const order = "00000000-0000-4000-8000-000000000001"
	base := OrderPayment{OrderID: order, Currency: "TWD", TotalMinor: 100,
		CommercialState: "DRAFT", TestMode: true, PaymentState: "NOT_STARTED", HandoffState: "NONE",
		Methods: []PaymentMethodOption{{Code: "payuni_credit", Version: 1, NameHans: "卡", NameHant: "卡", NameEN: "Card"}}}
	if !validPaymentView(base, order) {
		t.Fatal("valid bounded view rejected")
	}
	for name, change := range map[string]func(*OrderPayment){
		"owner-order":      func(v *OrderPayment) { v.OrderID = "different" },
		"currency":         func(v *OrderPayment) { v.Currency = "twd" },
		"amount":           func(v *OrderPayment) { v.TotalMinor = -1 },
		"null-methods":     func(v *OrderPayment) { v.Methods = nil },
		"second-method":    func(v *OrderPayment) { v.Methods = append(v.Methods, v.Methods[0]) },
		"unknown-state":    func(v *OrderPayment) { v.PaymentState = "PAID_BY_REDIRECT" },
		"unknown-handoff":  func(v *OrderPayment) { v.HandoffState = "RETRYABLE" },
		"missing-deadline": func(v *OrderPayment) { v.HandoffState = "PREPARED" },
		"pending-option":   func(v *OrderPayment) { v.PaymentState = "PENDING" },
		"cancelled-option": func(v *OrderPayment) { v.CommercialState = "CANCELLED" },
	} {
		t.Run(name, func(t *testing.T) {
			v := base
			change(&v)
			if validPaymentView(v, order) {
				t.Fatal("invalid view accepted")
			}
		})
	}
	base.Methods = []PaymentMethodOption{}
	base.PaymentState, base.HandoffState = "CAPTURED", "UNAVAILABLE"
	if !validPaymentView(base, order) {
		t.Fatal("historical different-profile view must remain readable")
	}
	now := time.Now().UTC()
	base.HandoffState, base.HandoffExpiresAt = "ISSUED", &now
	if !validPaymentView(base, order) {
		t.Fatal("issued captured view rejected")
	}
}

func TestPaymentViewNotFoundMapping(t *testing.T) {
	if !errors.Is(safeError(context.Background(), &pgconn.PgError{Code: "PT404"}), command.ErrNotFound) {
		t.Fatal("private missing/foreign order must not become database unavailable")
	}
}

func TestPaymentViewRefundStatesAreStripeOnlyAndConsistent(t *testing.T) {
	const order = "00000000-0000-4000-8000-000000000001"
	cancel := false
	base := OrderPayment{OrderID: order, Currency: "HKD", TotalMinor: 10000, CommercialState: "CONFIRMED", TestMode: true,
		PaymentState: "PARTIALLY_REFUNDED", HandoffState: "CLOSED", Methods: []PaymentMethodOption{}, CancelRequested: &cancel,
		Refund: &OrderRefund{RefundedMinor: 4000, PendingMinor: 1000}}
	expires := time.Now().UTC()
	base.HandoffExpiresAt = &expires
	if !validPaymentViewFor(base, order, true) {
		t.Fatal("valid partial refund view rejected")
	}
	if validPaymentViewFor(base, order, false) {
		t.Fatal("v1 (PAYUNi) must never accept a refund state")
	}
	full := base
	full.PaymentState, full.Refund = "REFUNDED", &OrderRefund{RefundedMinor: 10000}
	if !validPaymentViewFor(full, order, true) {
		t.Fatal("valid full refund view rejected")
	}
	for name, change := range map[string]func(*OrderPayment){
		"refunded but total not covered": func(v *OrderPayment) { v.PaymentState = "REFUNDED" },
		"partial but fully refunded":     func(v *OrderPayment) { v.Refund = &OrderRefund{RefundedMinor: 10000} },
		"refund state without totals":    func(v *OrderPayment) { v.Refund = nil },
		"nothing refunded yet":           func(v *OrderPayment) { v.Refund = &OrderRefund{PendingMinor: 100} },
		"over total":                     func(v *OrderPayment) { v.Refund = &OrderRefund{RefundedMinor: 9000, PendingMinor: 2000} },
		"negative pending":               func(v *OrderPayment) { v.Refund = &OrderRefund{RefundedMinor: 4000, PendingMinor: -1} },
	} {
		v := base
		change(&v)
		if validPaymentViewFor(v, order, true) {
			t.Errorf("%s accepted", name)
		}
	}
	// A pending-only refund keeps the base state and still shows the processing amount.
	pending := base
	pending.PaymentState, pending.Refund = "CAPTURED", &OrderRefund{PendingMinor: 2500}
	if !validPaymentViewFor(pending, order, true) {
		t.Fatal("pending refund on a captured order rejected")
	}
	// dropCancelUnlessStripe (D8): only Stripe-attempt orders may expose the refund totals.
	dropCancelUnlessStripe(&pending, false)
	if pending.Refund != nil || pending.CancelRequested != nil {
		t.Fatal("non-Stripe order kept refund or cancel_requested")
	}
	kept := base
	dropCancelUnlessStripe(&kept, true)
	if kept.Refund == nil || kept.CancelRequested == nil {
		t.Fatal("Stripe order lost its projection")
	}
}

func TestPaymentViewRefundJSONIsOmittedWhenAbsent(t *testing.T) {
	raw, err := json.Marshal(OrderPayment{OrderID: "x", Methods: []PaymentMethodOption{}})
	if err != nil || strings.Contains(string(raw), "refund") {
		t.Fatalf("PAYUNi bytes must not gain a refund key: %s %v", raw, err)
	}
	raw, _ = json.Marshal(OrderPayment{OrderID: "x", Refund: &OrderRefund{RefundedMinor: 1}})
	if !strings.Contains(string(raw), `"refund":{"refunded_minor":1,"pending_minor":0}`) {
		t.Fatalf("refund shape: %s", raw)
	}
}
