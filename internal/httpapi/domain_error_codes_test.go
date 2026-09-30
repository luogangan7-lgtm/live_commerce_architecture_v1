package httpapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"livecommerce/internal/billing"
	"livecommerce/internal/claims"
	"livecommerce/internal/customers"
	"livecommerce/internal/merchantorders"
)

// Ruling 15 (refund-fulfilment-rulings.md): every §7.1 refund and §5.1 shipment code must reach the
// JSON body. The route families classify with refundClassify/shipmentClassify and write through
// respondError (httperror.Write), whose closed message table used to rewrite unknown codes to
// "internal" — this test drives that exact path per sentinel.
func TestDomainErrorCodesReachJSONBody(t *testing.T) {
	families := map[string]struct {
		classify func(error) (int, string)
		cases    map[error]string
	}{
		"refunds": {refundClassify, map[error]string{
			merchantorders.ErrRefundableChanged:   "refundable_changed",
			merchantorders.ErrExceedsRefundable:   "exceeds_refundable",
			merchantorders.ErrAmountStep:          "amount_step",
			merchantorders.ErrNotRefundable:       "not_refundable",
			merchantorders.ErrRefundBlockedReview: "refund_blocked_review",
			merchantorders.ErrRefundLimit:         "refund_limit",
		}},
		"customers": {customersClassify, map[error]string{
			customers.ErrIdempotencyConflict: "idempotency_conflict",
			customers.ErrErasureBlocked:      "erasure_blocked",
			customers.ErrExportTooLarge:      "export_too_large",
			customers.ErrErased:              "erased",
		}},
		"shipments": {shipmentClassify, map[error]string{
			merchantorders.ErrVersionChanged:      "version_changed",
			merchantorders.ErrNotShippable:        "not_shippable",
			merchantorders.ErrInvalidCarrier:      "invalid_carrier",
			merchantorders.ErrInvalidTracking:     "invalid_tracking",
			merchantorders.ErrInvalidURL:          "invalid_url",
			merchantorders.ErrVoidRequiresShipped: "void_requires_shipped",
			merchantorders.ErrInvalidVoid:         "invalid_void",
		}},
		"billing": {billingClassify, map[error]string{
			billing.ErrSubscriptionExists: "subscription_exists",
			billing.ErrNoCustomer:         "no_billing_customer",
		}},
	}
	for family, f := range families {
		for sentinel, want := range f.cases {
			t.Run(family+"/"+want, func(t *testing.T) {
				status, code := f.classify(sentinel)
				rec := httptest.NewRecorder()
				respondError(rec, status, code)
				var body struct {
					Code    string `json:"code"`
					Message string `json:"message"`
				}
				if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
					t.Fatalf("body is not JSON: %v", err)
				}
				if rec.Code != status || status < http.StatusBadRequest || status >= http.StatusInternalServerError {
					t.Fatalf("status=%d classified=%d, want a 4xx", rec.Code, status)
				}
				if body.Code != want || body.Message == "" || body.Message == "Request could not be completed." {
					t.Fatalf("body code=%q message=%q, want code %q with its own message", body.Code, body.Message, want)
				}
			})
		}
	}
}

// billing-core B2/B12: the two billing codes outside the 4xx family loop above must also reach the body
// (503 billing_unavailable is retryable, 402 billing_restricted is the claim-window guard).
func TestBillingStatusCodesReachJSONBody(t *testing.T) {
	for _, tc := range []struct {
		err    error
		status int
		code   string
	}{
		{billing.ErrUnavailable, http.StatusServiceUnavailable, "billing_unavailable"},
		{claims.ErrBillingRestricted, http.StatusPaymentRequired, "billing_restricted"},
	} {
		status, code := billingClassify(tc.err)
		if tc.status == http.StatusPaymentRequired {
			status, code = claimsClassify(tc.err)
		}
		rec := httptest.NewRecorder()
		respondError(rec, status, code)
		var body struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil || rec.Code != tc.status ||
			body.Code != tc.code || body.Message == "" || body.Message == "Request could not be completed." {
			t.Fatalf("%v: status=%d body=%+v err=%v, want %d %s", tc.err, rec.Code, body, err, tc.status, tc.code)
		}
	}
}
