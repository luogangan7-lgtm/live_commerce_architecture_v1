package checkout

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"time"

	"github.com/jackc/pgx/v5"
	"livecommerce/internal/buyer"
	"livecommerce/internal/command"
)

type PaymentMethodOption struct {
	Code     string `json:"code"`
	Version  int64  `json:"version"`
	NameHans string `json:"name_hans"`
	NameHant string `json:"name_hant"`
	NameEN   string `json:"name_en"`
}

// OrderRefund is the buyer-safe refund total of a Stripe order (stripe-refund-v1 §7.2, D8): no refund
// id, reason, provider string or failure detail. Pending covers REQUESTED..UNKNOWN refunds that still
// hold the money; Refunded counts only succeeded, not-failed ones.
type OrderRefund struct {
	RefundedMinor int64 `json:"refunded_minor"`
	PendingMinor  int64 `json:"pending_minor"`
}

// OrderPayment is already buyer-safe. The SQL projection deliberately excludes
// attempt/account IDs, credentials, stored form bytes and provider references.
type OrderPayment struct {
	OrderID          string                `json:"order_id"`
	Currency         string                `json:"currency"`
	TotalMinor       int64                 `json:"total_minor"`
	CommercialState  string                `json:"commercial_state"`
	TestMode         bool                  `json:"test_mode"`
	PaymentState     string                `json:"payment_state"`
	HandoffState     string                `json:"handoff_state"`
	HandoffExpiresAt *time.Time            `json:"handoff_expires_at"`
	Methods          []PaymentMethodOption `json:"methods"`
	// CancelRequested is emitted only for Stripe-attempt orders (view_v2, then cleared by
	// PaymentView otherwise); nil is omitted so PAYUNi responses stay byte-identical (rulings §4).
	CancelRequested *bool `json:"cancel_requested,omitempty"`
	// Refund is emitted only for Stripe-attempt orders with refund activity (view_v2, D8); an absent
	// field is the same as null, so PAYUNi and refund-free responses keep byte-identical output.
	Refund *OrderRefund `json:"refund,omitempty"`
}

// PaymentView is an informational snapshot. BeginHosted and TakeHosted retain
// admission authority; calling this method never prepares or releases a form.
func (s *HostedPaymentStarter) PaymentView(ctx context.Context, token, storeID, orderID string) (OrderPayment, error) {
	if ctx == nil || s == nil || s.starter.pool == nil || !validPaymentProfile(s.starter.profile) || !command.ValidID(orderID) {
		return OrderPayment{}, command.ErrInvalid
	}
	tokenHash := sha256.Sum256([]byte(token))
	var out OrderPayment
	err := buyer.WithScope(ctx, s.starter.pool, token, storeID, func(callCtx context.Context, tx pgx.Tx, scope buyer.Scope) error {
		var body []byte
		if s.stripe == nil {
			if err := tx.QueryRow(callCtx, `SELECT checkout.hosted_payment_view($1,$2::uuid,$3::uuid,$4,$5)`,
				tokenHash[:], storeID, orderID, s.starter.profile, s.configDigest[:]).Scan(&body); err != nil {
				return err
			}
		} else {
			// checkout.hosted_payment_view_v2: superset of v1 that adds the Stripe method,
			// CLOSED_UNPAID and cancel_requested; a NULL PAYUNi digest hides the PAYUNi method.
			var payuni []byte
			if s.payuni {
				payuni = s.configDigest[:]
			}
			if err := tx.QueryRow(callCtx, `SELECT checkout.hosted_payment_view_v2($1,$2::uuid,$3::uuid,$4,$5::bytea,$6::bytea)`,
				tokenHash[:], storeID, orderID, s.starter.profile, payuni, s.stripeDigest[:]).Scan(&body); err != nil {
				return err
			}
		}
		if len(body) == 0 || len(body) > 64<<10 {
			return command.ErrConflict
		}
		decoder := json.NewDecoder(bytes.NewReader(body))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&out); err != nil || !validPaymentViewFor(out, orderID, s.stripe != nil) {
			return command.ErrConflict
		}
		if s.stripe != nil {
			// Same tx, so the provider cannot change between projection and check.
			isStripe, err := s.isStripeOrder(callCtx, tx, tokenHash[:], storeID, orderID)
			if err != nil {
				return err
			}
			dropCancelUnlessStripe(&out, isStripe)
		}
		if out.HandoffExpiresAt != nil {
			utc := out.HandoffExpiresAt.UTC()
			out.HandoffExpiresAt = &utc
		}
		return checkCapability(callCtx, tx, tokenHash[:], storeID, scope)
	})
	if err != nil {
		return OrderPayment{}, safeError(ctx, err)
	}
	return out, nil
}

// dropCancelUnlessStripe enforces rulings §4: v2 always emits cancel_requested (the validator
// requires it), but only Stripe-attempt orders may expose it, so PAYUNi and not-started orders
// serialize byte-identically whether or not Stripe is enabled. The refund totals follow the same rule
// (D8): only Stripe-attempt orders can carry them.
func dropCancelUnlessStripe(out *OrderPayment, isStripe bool) {
	if !isStripe {
		out.CancelRequested = nil
		out.Refund = nil
	}
}

func validPaymentView(out OrderPayment, orderID string) bool {
	return validPaymentViewFor(out, orderID, false)
}

// validPaymentViewFor checks one projection. v2 (Stripe-enabled service) allows two
// methods, CLOSED_UNPAID, the Stripe handoff states and requires cancel_requested;
// v1 rejects all of them so PAYUNi-only bytes and validation stay unchanged.
func validPaymentViewFor(out OrderPayment, orderID string, v2 bool) bool {
	maxMethods := 1
	if v2 {
		maxMethods = 2
	}
	if (out.CancelRequested != nil) != v2 {
		return false
	}
	if out.OrderID != orderID || len(out.Currency) != 3 || out.TotalMinor < 0 || out.TotalMinor > 1000000000000 ||
		out.Methods == nil || len(out.Methods) > maxMethods {
		return false
	}
	for _, ch := range out.Currency {
		if ch < 'A' || ch > 'Z' {
			return false
		}
	}
	switch out.CommercialState {
	case "DRAFT", "AWAITING_PAYMENT", "CONFIRMED", "CANCELLED":
	default:
		return false
	}
	switch out.PaymentState {
	case "NOT_STARTED", "PENDING", "AUTHORIZED", "CAPTURED", "REVIEW_REQUIRED":
	case "CLOSED_UNPAID":
		if !v2 {
			return false
		}
	case "PARTIALLY_REFUNDED", "REFUNDED":
		// Derived from succeeded refund facts (RD7): only the Stripe projection can say so, and the totals
		// must agree with the state.
		if !v2 || out.Refund == nil || out.Refund.RefundedMinor < 1 ||
			(out.PaymentState == "REFUNDED") != (out.Refund.RefundedMinor >= out.TotalMinor) {
			return false
		}
	default:
		return false
	}
	if out.Refund != nil && (!v2 || out.Refund.RefundedMinor < 0 || out.Refund.PendingMinor < 0 ||
		out.Refund.RefundedMinor > out.TotalMinor || out.Refund.PendingMinor > out.TotalMinor-out.Refund.RefundedMinor) {
		return false
	}
	switch out.HandoffState {
	case "NONE":
		if out.HandoffExpiresAt != nil {
			return false
		}
	case "UNAVAILABLE": // Historical profile may differ even without a page.
	case "PREPARED", "ISSUED", "EXPIRED", "CREATING", "READY", "CLOSED":
		// CREATING/READY/CLOSED are Stripe-only and need the v2 projection.
		if out.HandoffExpiresAt == nil || out.HandoffExpiresAt.IsZero() ||
			(!v2 && (out.HandoffState == "CREATING" || out.HandoffState == "READY" || out.HandoffState == "CLOSED")) {
			return false
		}
	default:
		return false
	}
	if len(out.Methods) > 0 && (out.PaymentState != "NOT_STARTED" || out.CommercialState != "DRAFT" || out.HandoffState != "NONE") {
		return false
	}
	seen := map[string]bool{}
	for _, method := range out.Methods {
		if (method.Code != "payuni_credit" && !(v2 && method.Code == stripeMethodCode)) || seen[method.Code] ||
			method.Version < 1 || method.NameHans == "" || method.NameHant == "" || method.NameEN == "" {
			return false
		}
		seen[method.Code] = true
	}
	return true
}
