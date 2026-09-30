// Package httperror owns transport-safe error envelopes, never domain policy.
package httperror

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strings"
)

type Envelope struct {
	Code      string         `json:"code"`
	Message   string         `json:"message"`
	RequestID string         `json:"request_id"`
	Retryable bool           `json:"retryable"`
	Details   map[string]any `json:"details"`
}

type requestKey struct{}

// Middleware assigns a server-owned correlation ID. Nested platform handlers
// reuse the context value, never an untrusted inbound X-Request-ID header.
func Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id, _ := r.Context().Value(requestKey{}).(string)
		if id == "" {
			var value [16]byte
			_, _ = rand.Read(value[:]) // Go's crypto/rand terminates on entropy failure.
			id = hex.EncodeToString(value[:])
			r = r.WithContext(context.WithValue(r.Context(), requestKey{}, id))
		}
		w.Header().Set("X-Request-ID", id)
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		next.ServeHTTP(&errorWriter{ResponseWriter: w}, r)
	})
}

func Write(w http.ResponseWriter, status int, code string) {
	write(w, status, code, status == http.StatusServiceUnavailable || status == http.StatusTooManyRequests)
}

// WriteNonRetryable is for non-idempotent operations such as issuing a new
// anonymous owner. A 503 does not prove that the database failed to commit.
func WriteNonRetryable(w http.ResponseWriter, status int, code string) {
	write(w, status, code, false)
}

func write(w http.ResponseWriter, status int, code string, retryable bool) {
	messages := map[string]string{
		"unauthorized": "Sign-in required.", "forbidden": "Operation not permitted.",
		"not_found": "Resource not found.", "method_not_allowed": "Method not allowed.",
		"invalid_request": "Request validation failed.", "invalid_json": "Malformed JSON body.",
		"json_required": "JSON content type required.", "conflict": "Request conflicts with current state.",
		"rate_limited":           "Too many requests.",
		"insufficient_inventory": "Insufficient available inventory.",
		"retry_later":            "Temporarily unavailable.", "unavailable": "Temporarily unavailable.",
		"internal": "Request could not be completed.",
		// meta-ads-v1 §7 / internal/ads frozenStatus: every frozen ads refusal code must be listed here or the
		// merchant sees "internal" (found by MA04; internal/ads TestFrozenCodesSurviveHTTPError guards the drift).
		"state_mismatch":           "This Meta connection attempt does not belong to this session.",
		"state_expired":            "This Meta connection attempt expired.",
		"meta_connect_failed":      "Meta could not complete the connection.",
		"not_in_pick_list":         "That ad account or dataset was not offered by Meta for this login.",
		"client_business_changed":  "This ad account now belongs to a different Meta business.",
		"revision_changed":         "The draft changed since it was loaded.",
		"draft_approved":           "An approved draft cannot be edited.",
		"over_allowance":           "The budget exceeds the store's ads allowance.",
		"attempt_changed":          "The publish attempt changed since it was loaded.",
		"prior_attempt_not_paused": "An earlier attempt is not confirmed paused.",
		"budget_below_minimum":     "The budget is below the minimum.",
		"not_whole_unit":           "The budget must be a whole currency unit.",
		"currency_mismatch":        "The currency does not match the ad account.",
		"starts_too_soon":          "The start time is too soon.",
		"binding_disabled":         "The Meta ads connection is not enabled.",
		"source_not_owned":         "That post does not belong to this store's connection.",
		"product_not_published":    "The product is not published.",
		// stripe-refund-v1 §7.1 (ruling 15: unknown codes were rewritten to "internal").
		"refundable_changed":    "Refundable amount changed since it was loaded.",
		"exceeds_refundable":    "Amount exceeds the refundable amount.",
		"amount_step":           "Amount is not a valid step for this currency.",
		"not_refundable":        "Order cannot be refunded.",
		"refund_blocked_review": "Refund is blocked by an open payment review.",
		"refund_limit":          "Refund limit reached for this payment.",
		// manual-fulfilment-v1 §5.1.
		"version_changed":       "This item changed since it was loaded.",
		"not_shippable":         "Order cannot be shipped in its current state.",
		"invalid_carrier":       "Carrier is not valid.",
		"invalid_tracking":      "Tracking number is not valid.",
		"invalid_url":           "Tracking URL is not valid.",
		"void_requires_shipped": "Only a shipped record can be voided.",
		"invalid_void":          "Void request is not valid.",
		// taiwan-cvs-logistics-v1 §8 / §5.2 / §16 (unit cvs-core). Ruling 15: an unknown code would be rewritten to "internal".
		"ecpay_probe_failed": "ECPay rejected the keys or could not be reached.", "invalid_sender": "Sender name or mobile number is not valid.",
		"ecpay_environment_not_allowed": "This ECPay environment is not allowed on this deployment.",
		"not_qualified":                 "The ECPay connection has not passed its check with the current keys.",
		"another_profile_enabled":       "Another ECPay connection of this store is enabled.", "merchant_id_changed": "The ECPay merchant id cannot change on rotation.",
		"connection_unavailable": "No usable ECPay connection for this store.", "no_cvs_destination": "The order has no ECPay-verified pickup store.",
		"cvs_recipient_rejected":   "The recipient name or mobile number does not meet the ECPay rules.",
		"cvs_environment_mismatch": "The pickup store or connection belongs to another ECPay environment.",
		"cvs_amount_exceeds":       "The amount is outside the ECPay convenience-store limits.", "cvs_source_mismatch": "This store source cannot be used with this delivery service.",
		"print_unsupported": "This label cannot be printed here.", "not_created": "No label has been created for this order yet.",
		"ecpay_shows_movement": "ECPay shows the parcel is not unmoved; it cannot be abandoned.", "ecpay_trade_found": "ECPay has this shipment; it was recorded as created.",
		"not_lapsed": "The ECPay order has not lapsed yet.", "reconcile_in_progress": "The shipment is still being reconciled with ECPay.",
		"acknowledgement_required": "Confirm that you checked the ECPay back office.", "attempt_in_flight": "A label request is still in progress.",
		"cvs_attempt_in_flight": "A label request may already be at ECPay.", "not_abandonable": "This shipment can no longer be abandoned.",
		"invalid_settings": "The convenience-store settings are not valid.", "not_pay_at_pickup": "The order is not a pay-at-pickup order.",
		"not_shipped": "The order has not been shipped.", "collection_state_changed": "The collection state changed since it was loaded.",
		"parcel_not_returned": "The parcel has not been returned yet.", "not_cancellable": "The order can no longer be cancelled.",
		"idempotency_conflict": "This request key was already used for a different request.",
		"bad_return_path":      "The return page is not allowed.", "bad_return_origin": "The storefront origin is not allowed.",
		"service_unavailable": "This delivery service is not available.", "selection_replay_new_key": "Start the store selection again.",
		"bad_store_code": "The store number is not valid for this chain.", "bad_store_name": "The store name is not valid.",
		"bad_store_address": "The store address is not valid.", "pay_at_pickup_unavailable": "Pay at pickup is not available for this order.",
		"pay_at_pickup_amount_exceeds": "The amount is outside the pay-at-pickup limit.", "pay_at_pickup_limit": "Too many pay-at-pickup orders are open.",
		// meta-claims-intake-v1 §2 / claim-source unit: comment source binding (version_changed above is shared).
		"input_invalid":      "The pasted link or id is not a supported Facebook or Instagram post.",
		"input_unresolvable": "This link cannot be resolved without Meta; paste the numeric post or media id.",
		"binding_missing":    "No enabled Meta connection is ready for this post.",
		"binding_ambiguous":  "Several Meta connections are enabled; the post cannot be assigned to one.",
		"source_conflict":    "This post already feeds another live session.",
		"page_token_missing": "Private replies need a registered Page token for this connection.",
		// customers-billing-v1 §5/§6 (customers-core): consent, export and erasure.
		// idempotency_conflict: shared, declared with the CVS codes above.
		"erasure_blocked":  "Erasure is blocked while a hold, payment or refund is in progress.",
		"erased":           "This data has been erased.",
		"export_too_large": "The export is too large to generate.",
		// customers-billing-v1 §5 / billing-core B2, B12: platform billing.
		"billing_unavailable": "Billing is temporarily unavailable.",
		"billing_restricted":  "New claim windows are paused until billing is up to date.",
		"subscription_exists": "This store already has a subscription.",
		"no_billing_customer": "No billing account exists for this store yet.",
	}
	message, ok := messages[code]
	if !ok {
		code, message = "internal", messages["internal"]
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(Envelope{Code: code, Message: message,
		RequestID: w.Header().Get("X-Request-ID"), Retryable: retryable,
		Details: map[string]any{}})
}

// ServeMux generates plain-text 404/405 responses. Translate only non-JSON
// errors at the boundary; never buffer application responses or expose text.
type errorWriter struct {
	http.ResponseWriter
	wrote, suppressed bool
}

func (w *errorWriter) WriteHeader(status int) {
	if w.wrote {
		return
	}
	w.wrote = true
	if status >= 400 && !strings.HasPrefix(w.Header().Get("Content-Type"), "application/json") {
		w.suppressed = true
		code := "internal"
		if status == 404 {
			code = "not_found"
		}
		if status == 405 {
			code = "method_not_allowed"
		}
		w.Header().Del("Content-Length")
		Write(w.ResponseWriter, status, code)
		return
	}
	w.ResponseWriter.WriteHeader(status)
}

func (w *errorWriter) Write(p []byte) (int, error) {
	if !w.wrote {
		w.WriteHeader(http.StatusOK)
	}
	if w.suppressed {
		return len(p), nil
	}
	return w.ResponseWriter.Write(p)
}

func (w *errorWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }
