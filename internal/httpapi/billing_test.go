// billing_test.go covers the customers-billing-v1 §5 billing routes' transport rules and error
// classification without a database. With a nil pool, a request that passes every transport rule reaches
// platform.WithScope and is answered 401, the "admitted to authentication" marker (refunds_test.go
// pattern). Real-PG and Stripe-fake behaviour (CB08, CB09) belongs to customers-billing-tests.
// Names deliberately avoid the TestCustomersBillingCB prefix owned by that unit.

package httpapi

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"

	"livecommerce/internal/billing"
	"livecommerce/internal/claims"
	"livecommerce/internal/command"
	"livecommerce/internal/platform"
)

const (
	billingStore  = "11111111-1111-4111-8111-111111111111"
	billingBearer = "Bearer AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
	billingBase   = "/v1/admin/stores/" + billingStore + "/billing"
)

func billingHandler(svc *billing.Service) http.Handler {
	mux := http.NewServeMux()
	registerBillingRoutes(mux, nil, svc)
	return mux
}

func billingCall(h http.Handler, method, path, body string, edit func(*http.Request)) *httptest.ResponseRecorder {
	var r *http.Request
	if body == "" {
		r = httptest.NewRequest(method, path, nil)
	} else {
		r = httptest.NewRequest(method, path, strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
	}
	r.Header.Set("Authorization", billingBearer)
	if edit != nil {
		edit(r)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func TestBillingGetsAreMountedWithoutAService(t *testing.T) {
	// LC_BILLING_ENABLED unset: a nil Service still mounts standing and GET billing (they answer from PG).
	h := billingHandler(nil)
	for _, path := range []string{billingBase, billingBase + "/standing"} {
		if w := billingCall(h, http.MethodGet, path, "", nil); w.Code != http.StatusUnauthorized {
			t.Fatalf("GET %s: status=%d, want 401 (admitted to authentication)", path, w.Code)
		}
	}
	// POSTs are mounted too and pass their transport rules; they answer after authentication.
	if w := billingCall(h, http.MethodPost, billingBase+"/checkout", `{"price_id":"price_1Abc"}`, nil); w.Code != http.StatusUnauthorized {
		t.Fatalf("POST checkout status=%d", w.Code)
	}
	if w := billingCall(h, http.MethodPost, billingBase+"/portal", "", nil); w.Code != http.StatusUnauthorized {
		t.Fatalf("POST portal status=%d", w.Code)
	}
}

func TestBillingTransportRules(t *testing.T) {
	h := billingHandler(nil)
	type tc struct {
		name, method, path, body string
		edit                     func(*http.Request)
		want                     int
	}
	key := func(r *http.Request) { r.Header.Set("Idempotency-Key", "billing-key-0001") }
	noAuth := func(r *http.Request) { r.Header.Del("Authorization") }
	for _, c := range []tc{
		{"GET billing ok", "GET", billingBase, "", nil, 401},
		{"GET without bearer", "GET", billingBase, "", noAuth, 401},
		{"GET with query", "GET", billingBase + "?x=1", "", nil, 422},
		{"GET with Idempotency-Key", "GET", billingBase, "", key, 422},
		{"GET with body", "GET", billingBase, `{}`, nil, 422},
		{"GET standing with Idempotency-Key", "GET", billingBase + "/standing", "", key, 422},
		{"POST billing is not a route", "POST", billingBase, `{}`, nil, 405},
		{"DELETE standing", "DELETE", billingBase + "/standing", "", nil, 405},
		{"PUT checkout", "PUT", billingBase + "/checkout", `{"price_id":"price_1"}`, nil, 405},
		{"checkout with Idempotency-Key (Stripe checkout takes none, §6)", "POST", billingBase + "/checkout", `{"price_id":"price_1"}`, key, 422},
		{"checkout with query", "POST", billingBase + "/checkout?x=1", `{"price_id":"price_1"}`, nil, 422},
		{"checkout not json", "POST", billingBase + "/checkout", `{"price_id":"price_1"}`, func(r *http.Request) { r.Header.Set("Content-Type", "text/plain") }, 415},
		{"checkout unknown field", "POST", billingBase + "/checkout", `{"price_id":"price_1","customer":"cus_1"}`, nil, 400},
		{"checkout duplicate key", "POST", billingBase + "/checkout", `{"price_id":"price_1","price_id":"price_2"}`, nil, 400},
		{"checkout null", "POST", billingBase + "/checkout", `{"price_id":null}`, nil, 400},
		{"checkout missing price_id", "POST", billingBase + "/checkout", `{}`, nil, 422},
		{"checkout ok", "POST", billingBase + "/checkout", `{"price_id":"price_1"}`, nil, 401},
		{"portal with body", "POST", billingBase + "/portal", `{}`, nil, 422},
		{"portal with Idempotency-Key", "POST", billingBase + "/portal", "", key, 422},
		{"portal ok", "POST", billingBase + "/portal", "", nil, 401},
		{"store id not a uuid", "GET", "/v1/admin/stores/not-a-uuid/billing", "", nil, 422},
	} {
		if w := billingCall(h, c.method, c.path, c.body, c.edit); w.Code != c.want {
			t.Errorf("%s: status=%d body=%s, want %d", c.name, w.Code, w.Body.String(), c.want)
		}
	}
}

func TestBillingResponsesArePrivateAndNoReferrer(t *testing.T) {
	h := billingHandler(nil)
	for _, c := range []struct{ method, path string }{
		{"GET", billingBase}, {"GET", billingBase + "/standing"}, {"POST", billingBase + "/checkout"}, {"POST", billingBase + "/portal"}} {
		w := billingCall(h, c.method, c.path, "", nil)
		if got := w.Header().Get("Cache-Control"); got != "private, no-store" {
			t.Errorf("%s %s: Cache-Control=%q", c.method, c.path, got)
		}
	}
	// URL-bearing routes forbid a Referer on every response, including transport errors and the 405 fallback.
	for _, c := range []struct{ method, path string }{{"POST", billingBase + "/checkout"}, {"POST", billingBase + "/portal"}, {"GET", billingBase + "/checkout"}} {
		if w := billingCall(h, c.method, c.path, "", nil); w.Header().Get("Referrer-Policy") != "no-referrer" {
			t.Errorf("%s %s: Referrer-Policy=%q", c.method, c.path, w.Header().Get("Referrer-Policy"))
		}
	}
}

func TestBillingClassifierMapping(t *testing.T) {
	pgErr := func(code string) error { return &pgconn.PgError{Code: code, Message: "driver text with a store id"} }
	for _, tc := range []struct {
		err    error
		status int
		code   string
	}{
		{billing.ErrUnavailable, 503, "billing_unavailable"},
		{fmt.Errorf("wrapped: %w", billing.ErrUnavailable), 503, "billing_unavailable"},
		{billing.ErrSubscriptionExists, 409, "subscription_exists"},
		{billing.ErrNoCustomer, 409, "no_billing_customer"},
		{billing.ErrUnknownPrice, 422, "invalid_request"},
		{command.ErrInvalid, 422, "invalid_request"},
		{command.ErrConflict, 409, "conflict"},
		{platform.ErrUnauthorized, 401, "unauthorized"},
		{platform.ErrForbidden, 403, "forbidden"},
		{platform.ErrScopeNotFound, 404, "not_found"},
		{pgErr("40P01"), 503, "retry_later"},
		{context.DeadlineExceeded, 503, "retry_later"},
		{errors.New("stripe said card 4242"), 503, "unavailable"},
	} {
		if status, code := billingClassify(tc.err); status != tc.status || code != tc.code {
			t.Errorf("%v: got %d/%s want %d/%s", tc.err, status, code, tc.status, tc.code)
		}
	}
}

// PT412 (0079 guard trigger) reaches the merchant as 402 billing_restricted through the claims classifier
// (brief B12); PT402 keeps meaning insufficient stock and is not touched.
func TestClaimsClassifyMapsBillingRestrictedTo402(t *testing.T) {
	for _, err := range []error{claims.ErrBillingRestricted, fmt.Errorf("window open: %w", claims.ErrBillingRestricted)} {
		if status, code := claimsClassify(err); status != http.StatusPaymentRequired || code != "billing_restricted" {
			t.Fatalf("%v: %d/%s", err, status, code)
		}
	}
	if status, code := claimsClassify(command.ErrInsufficient); status != http.StatusConflict || code != "insufficient_inventory" {
		t.Fatalf("insufficient stock changed: %d/%s", status, code)
	}
}
