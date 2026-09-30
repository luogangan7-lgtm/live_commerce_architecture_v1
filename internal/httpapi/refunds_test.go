// refunds_test.go covers the stripe-refund-v1 §7.1 transport rules that hold before any database work.
// With a nil pool, a request that passes every transport rule reaches platform.WithScope and is answered
// 401, which these tests use as the "admitted to the transaction" marker. Real-PG behaviour (RF04, RF09)
// belongs to the independent test unit.

package httpapi

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"

	"livecommerce/internal/command"
	"livecommerce/internal/merchantorders"
	"livecommerce/internal/platform"
)

const (
	refundStore  = "11111111-1111-4111-8111-111111111111"
	refundOrder  = "22222222-2222-4222-8222-222222222222"
	refundID     = "33333333-3333-4333-8333-333333333333"
	refundBearer = "Bearer AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
	refundBase   = "/v1/admin/stores/" + refundStore + "/orders/" + refundOrder + "/refunds"
	refundBody   = `{"amount_minor":100,"reason":"duplicate","expected_refundable_minor":100}`
)

func refundHandler(t *testing.T) http.Handler {
	t.Helper()
	jobs, err := river.NewClient(riverpgxv5.New(nil), &river.Config{Schema: "river_payment"})
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	registerRefundRoutes(mux, nil, jobs)
	return mux
}

func refundCall(h http.Handler, method, path, body string, edit func(*http.Request)) *httptest.ResponseRecorder {
	var r *http.Request
	if body == "" {
		r = httptest.NewRequest(method, path, nil)
	} else {
		r = httptest.NewRequest(method, path, strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
	}
	r.Header.Set("Authorization", refundBearer)
	if edit != nil {
		edit(r)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func withKey(r *http.Request) { r.Header.Set("Idempotency-Key", "refund-key-0001") }

func TestRefundRoutesAreUnmountedWithoutAJobClient(t *testing.T) {
	mux := http.NewServeMux()
	registerRefundRoutes(mux, nil, nil)
	if w := refundCall(mux, http.MethodPost, refundBase, refundBody, withKey); w.Code != http.StatusNotFound {
		t.Fatalf("status=%d", w.Code)
	}
}

func TestRefundPostTransportRules(t *testing.T) {
	h := refundHandler(t)
	post := func(body string, edit func(*http.Request)) int {
		return refundCall(h, http.MethodPost, refundBase, body, func(r *http.Request) {
			withKey(r)
			if edit != nil {
				edit(r)
			}
		}).Code
	}
	if code := post(refundBody, nil); code != http.StatusUnauthorized {
		t.Fatalf("a well-formed request must be admitted to the scope transaction, got %d", code)
	}
	for name, c := range map[string]struct {
		body string
		edit func(*http.Request)
		want int
	}{
		"missing key":       {refundBody, func(r *http.Request) { r.Header.Del("Idempotency-Key") }, 422},
		"short key":         {refundBody, func(r *http.Request) { r.Header.Set("Idempotency-Key", "short") }, 422},
		"two keys":          {refundBody, func(r *http.Request) { r.Header.Add("Idempotency-Key", "refund-key-0002") }, 422},
		"query":             {refundBody, func(r *http.Request) { r.URL.RawQuery = "currency=usd" }, 422},
		"force query":       {refundBody, func(r *http.Request) { r.URL.ForceQuery = true }, 422},
		"not json":          {refundBody, func(r *http.Request) { r.Header.Set("Content-Type", "text/plain") }, 415},
		"unknown field":     {`{"amount_minor":100,"reason":"duplicate","expected_refundable_minor":100,"currency":"USD"}`, nil, 400},
		"duplicate field":   {`{"amount_minor":100,"amount_minor":1,"reason":"duplicate","expected_refundable_minor":100}`, nil, 400},
		"null field":        {`{"amount_minor":null,"reason":"duplicate","expected_refundable_minor":100}`, nil, 400},
		"missing field":     {`{"amount_minor":100,"reason":"duplicate"}`, nil, 422},
		"trailing data":     {refundBody + `{}`, nil, 400},
		"fractional":        {`{"amount_minor":1.5,"reason":"duplicate","expected_refundable_minor":100}`, nil, 400},
		"empty body":        {"", func(r *http.Request) { r.Header.Set("Content-Type", "application/json") }, 400},
		"bad order id":      {refundBody, func(r *http.Request) { r.URL.Path = "/v1/admin/stores/" + refundStore + "/orders/nope/refunds" }, 422},
		"bad bearer":        {refundBody, func(r *http.Request) { r.Header.Set("Authorization", "Bearer a b") }, 401},
		"missing bearer":    {refundBody, func(r *http.Request) { r.Header.Del("Authorization") }, 401},
		"unknown reason ok": {`{"amount_minor":100,"reason":"fraudulent","expected_refundable_minor":100}`, nil, 401}, // SQL/service refuse it, transport only decodes
	} {
		if code := post(c.body, c.edit); code != c.want {
			t.Errorf("%s: status=%d want %d", name, code, c.want)
		}
	}
}

func TestRefundGetAndRefreshTransportRules(t *testing.T) {
	h := refundHandler(t)
	call := func(method, path, body string, edit func(*http.Request)) int {
		return refundCall(h, method, path, body, edit).Code
	}
	if code := call(http.MethodGet, refundBase, "", nil); code != http.StatusUnauthorized {
		t.Fatalf("get admitted: %d", code)
	}
	refresh := refundBase + "/" + refundID + "/refresh"
	if code := call(http.MethodPost, refresh, "", nil); code != http.StatusUnauthorized {
		t.Fatalf("refresh admitted: %d", code)
	}
	for name, c := range map[string]struct {
		method, path, body string
		edit               func(*http.Request)
		want               int
	}{
		"get with key":       {http.MethodGet, refundBase, "", withKey, 422},
		"get with query":     {http.MethodGet, refundBase + "?limit=1", "", nil, 422},
		"get with body":      {http.MethodGet, refundBase, "{}", nil, 422},
		"refresh with key":   {http.MethodPost, refresh, "", withKey, 422},
		"refresh with body":  {http.MethodPost, refresh, "{}", nil, 422},
		"refresh with query": {http.MethodPost, refresh + "?x=1", "", nil, 422},
		"refresh bad refund": {http.MethodPost, refundBase + "/nope/refresh", "", nil, 422},
		"put":                {http.MethodPut, refundBase, "", nil, 405},
		"delete refresh":     {http.MethodDelete, refresh, "", nil, 405},
		"get refresh":        {http.MethodGet, refresh, "", nil, 405},
	} {
		if code := call(c.method, c.path, c.body, c.edit); code != c.want {
			t.Errorf("%s: status=%d want %d", name, code, c.want)
		}
	}
	w := refundCall(h, http.MethodPut, refundBase, "", nil)
	if w.Header().Get("Cache-Control") != "private, no-store" {
		t.Fatal("the 405 fallback must stay inside the private response boundary")
	}
}

func TestRefundClassifyMapsFixedCodes(t *testing.T) {
	for _, c := range []struct {
		err    error
		status int
		code   string
	}{
		{merchantorders.ErrRefundableChanged, 409, "refundable_changed"},
		{merchantorders.ErrExceedsRefundable, 422, "exceeds_refundable"},
		{merchantorders.ErrAmountStep, 422, "amount_step"},
		{merchantorders.ErrNotRefundable, 422, "not_refundable"},
		{merchantorders.ErrRefundBlockedReview, 422, "refund_blocked_review"},
		{merchantorders.ErrRefundLimit, 422, "refund_limit"},
		{command.ErrConflict, 409, "conflict"},
		{platform.ErrForbidden, 403, "forbidden"},
		{platform.ErrScopeNotFound, 404, "not_found"},
		{platform.ErrUnauthorized, 401, "unauthorized"},
		{merchantorders.ErrUnavailable, 503, "unavailable"},
		{errors.New("SENTINEL driver text"), 503, "unavailable"},
		{pgx.ErrTxClosed, 503, "unavailable"},
	} {
		status, code := refundClassify(c.err)
		if status != c.status || code != c.code {
			t.Errorf("%v -> %d %s want %d %s", c.err, status, code, c.status, c.code)
		}
	}
}
