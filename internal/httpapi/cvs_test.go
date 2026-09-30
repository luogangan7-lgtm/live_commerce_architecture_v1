package httpapi

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"

	"livecommerce/internal/command"
	"livecommerce/internal/fulfillment"
	"livecommerce/internal/platform"
)

const (
	cvsStore = "22222222-2222-4222-8222-222222222222"
	cvsOrder = "33333333-3333-4333-8333-333333333333"
)

// cvsMux registers the routes over a CVS whose pool is never reached: every case below is refused by the transport gate or by
// Go-side validation before any SQL.
func cvsMux(t *testing.T) *http.ServeMux {
	t.Helper()
	jobs, err := river.NewClient(riverpgxv5.New(&pgxpool.Pool{}), &river.Config{Schema: "river"})
	if err != nil {
		t.Fatal(err)
	}
	cvs, err := fulfillment.NewCVS(&pgxpool.Pool{}, jobs, nil, nil, fulfillment.CVSConfig{PaymentEnvironment: "SANDBOX"})
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	registerCVSRoutes(mux, nil, cvs)
	return mux
}

func cvsCall(mux *http.ServeMux, method, path string, headers map[string]string, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+strings.Repeat("t", 43))
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return rec
}

func TestCVSRoutesUnmountedWithoutService(t *testing.T) {
	mux := http.NewServeMux()
	registerCVSRoutes(mux, nil, nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/admin/stores/"+cvsStore+"/logistics/ecpay", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("unmounted surface answered %d", rec.Code)
	}
}

func TestCVSRouteTransportGates(t *testing.T) {
	mux := cvsMux(t)
	base := "/v1/admin/stores/" + cvsStore
	key := map[string]string{"Idempotency-Key": "cvs-route-key-0001", "Content-Type": "application/json"}
	json := map[string]string{"Content-Type": "application/json"}
	type tc struct {
		name, method, path string
		headers            map[string]string
		body               string
		want               int
	}
	for _, c := range []tc{
		{"GET card with a key", http.MethodGet, base + "/logistics/ecpay", key, "", 422},
		{"GET card with a query", http.MethodGet, base + "/logistics/ecpay?x=1", nil, "", 422},
		{"GET card with a body", http.MethodGet, base + "/logistics/ecpay", nil, "{}", 422},
		{"PUT card without key", http.MethodPut, base + "/logistics/ecpay", json, `{}`, 422},
		{"PUT card with a bad key", http.MethodPut, base + "/logistics/ecpay", map[string]string{"Idempotency-Key": "short", "Content-Type": "application/json"}, `{}`, 422},
		{"PUT card without JSON type", http.MethodPut, base + "/logistics/ecpay", map[string]string{"Idempotency-Key": "cvs-route-key-0001"}, `{}`, 415},
		{"PUT card with missing keys", http.MethodPut, base + "/logistics/ecpay", key, `{"expected_version":0}`, 422},
		{"PUT card with an unknown key", http.MethodPut, base + "/logistics/ecpay", key, `{"expected_version":0,"environment":"SANDBOX","mode":"C2C","merchant_id":"2000933","hash_key":"a","hash_iv":"b","sender_name":"x","sender_cell_phone":"y","extra":1}`, 400},
		{"PUT card with a null secret", http.MethodPut, base + "/logistics/ecpay", key, `{"expected_version":0,"environment":"SANDBOX","mode":"C2C","merchant_id":"2000933","hash_key":null,"hash_iv":"b","sender_name":"x","sender_cell_phone":"y"}`, 422},
		{"POST card on the wrong method", http.MethodPost, base + "/logistics/ecpay", key, `{}`, 405},
		{"enabled without version", http.MethodPost, base + "/logistics/ecpay/enabled", key, `{"enabled":true}`, 422},
		{"settings duplicate key", http.MethodPut, base + "/logistics/cvs-settings", key, `{"expected_version":0,"expected_version":1,"enabled_chains":[],"pay_at_pickup_enabled":false,"pay_at_pickup_max_twd":null,"pay_at_pickup_max_open":5}`, 400},
		{"settings null open", http.MethodPut, base + "/logistics/cvs-settings", key, `{"expected_version":0,"enabled_chains":[],"pay_at_pickup_enabled":false,"pay_at_pickup_max_twd":null,"pay_at_pickup_max_open":null}`, 422},
		{"settings duplicate chain", http.MethodPut, base + "/logistics/cvs-settings", key, `{"expected_version":0,"enabled_chains":["cvs_711","cvs_711"],"pay_at_pickup_enabled":false,"pay_at_pickup_max_twd":null,"pay_at_pickup_max_open":5}`, 422},
		{"request with a bad order id", http.MethodPost, base + "/orders/not-a-uuid/cvs-shipment", key, `{"expected_version":0}`, 422},
		{"request with an extra field", http.MethodPost, base + "/orders/" + cvsOrder + "/cvs-shipment", key, `{"expected_version":0,"attempt":2}`, 400},
		{"request without key", http.MethodPost, base + "/orders/" + cvsOrder + "/cvs-shipment", json, `{"expected_version":0}`, 422},
		{"shipment GET with a key", http.MethodGet, base + "/orders/" + cvsOrder + "/cvs-shipment", key, "", 422},
		{"print-form with a key", http.MethodPost, base + "/orders/" + cvsOrder + "/cvs-shipment/print-form", key, `{"thermal":true}`, 422},
		{"print-form with an unknown field", http.MethodPost, base + "/orders/" + cvsOrder + "/cvs-shipment/print-form", json, `{"thermal":true,"copies":2}`, 400},
		{"print-form without thermal", http.MethodPost, base + "/orders/" + cvsOrder + "/cvs-shipment/print-form", json, `{}`, 422},
		{"abandon without acknowledgement", http.MethodPost, base + "/orders/" + cvsOrder + "/cvs-shipment/abandon", key, `{"expected_version":1}`, 422},
		{"collection with a state alias", http.MethodPost, base + "/orders/" + cvsOrder + "/collection", key, `{"expected_state":"PENDING","state":"collected","note":"x"}`, 400},
		{"collection unknown expected state", http.MethodPost, base + "/orders/" + cvsOrder + "/collection", key, `{"expected_state":"PAID","state":"collected"}`, 422},
		{"release bad action", http.MethodPost, base + "/orders/" + cvsOrder + "/pay-at-pickup-release", key, `{"action":"refund","expected_state":"PENDING"}`, 422},
		{"release cancel of RETURNED", http.MethodPost, base + "/orders/" + cvsOrder + "/pay-at-pickup-release", key, `{"action":"cancel","expected_state":"RETURNED"}`, 422},
	} {
		rec := cvsCall(mux, c.method, c.path, c.headers, c.body)
		if rec.Code != c.want {
			t.Errorf("%s: %d want %d (%s)", c.name, rec.Code, c.want, rec.Body.String())
		}
		if got := rec.Header().Get("Cache-Control"); got != "private, no-store" && rec.Code != 405 {
			t.Errorf("%s: cache-control %q", c.name, got)
		}
	}
	// A missing or malformed bearer never reaches the domain.
	req := httptest.NewRequest(http.MethodGet, base+"/logistics/ecpay", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("no bearer: %d", rec.Code)
	}
}

func TestCVSStrictBodyAllowsOnlyDeclaredNulls(t *testing.T) {
	type body struct {
		A int  `json:"a"`
		B *int `json:"b"`
	}
	run := func(raw string, nullable []string) (int, bool) {
		req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(raw))
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		_, ok := cvsStrictBody[body](rec, req, []string{"a", "b"}, nullable)
		return rec.Code, ok
	}
	if code, ok := run(`{"a":1,"b":null}`, []string{"b"}); !ok {
		t.Fatalf("declared null refused: %d", code)
	}
	if code, ok := run(`{"a":1,"b":null}`, nil); ok || code != 422 {
		t.Fatalf("undeclared null accepted: %d %v", code, ok)
	}
	if code, ok := run(`{"a":null,"b":1}`, []string{"b"}); ok || code != 422 {
		t.Fatalf("null on a required key accepted: %d", code)
	}
	if code, ok := run(`{"a":1}`, []string{"b"}); ok || code != 422 {
		t.Fatalf("absent nullable key accepted: %d", code)
	}
}

func TestCVSClassify(t *testing.T) {
	for _, tc := range []struct {
		err    error
		status int
		code   string
		retry  int
	}{
		{&fulfillment.CVSError{Status: 422, Code: "not_shippable"}, 422, "not_shippable", 0},
		{&fulfillment.CVSError{Status: 429, Code: "pay_at_pickup_limit", RetryAfter: 60}, 429, "pay_at_pickup_limit", 60},
		{errors.Join(errors.New("wrapped"), &fulfillment.CVSError{Status: 409, Code: "version_changed"}), 409, "version_changed", 0},
		{fulfillment.ErrECPayDisabled, 503, "unavailable", 0},
		{fulfillment.ErrCVSProjection, 503, "unavailable", 0},
		{command.ErrNotFound, 404, "not_found", 0},
		{platform.ErrForbidden, 403, "forbidden", 0},
		{command.ErrInvalid, 422, "invalid_request", 0},
	} {
		status, code, retry := cvsClassify(tc.err)
		if status != tc.status || code != tc.code || retry != tc.retry {
			t.Errorf("%v -> %d %s %d, want %d %s %d", tc.err, status, code, retry, tc.status, tc.code, tc.retry)
		}
	}
}

func TestCVSServeWritesRetryAfterAndNeverTheDriverText(t *testing.T) {
	rec := httptest.NewRecorder()
	cvsServe(rec, httptest.NewRequest(http.MethodPost, "/", nil), 0, http.StatusOK, func(context.Context) (any, error) {
		return nil, &fulfillment.CVSError{Status: 429, Code: "pay_at_pickup_limit", RetryAfter: 60}
	})
	if rec.Code != 429 || rec.Header().Get("Retry-After") != "60" {
		t.Fatalf("429 answer: %d %q", rec.Code, rec.Header().Get("Retry-After"))
	}
	rec = httptest.NewRecorder()
	cvsServe(rec, httptest.NewRequest(http.MethodPost, "/", nil), 0, http.StatusAccepted, func(context.Context) (any, error) {
		return map[string]string{"state": "REQUESTED"}, nil
	})
	if rec.Code != http.StatusAccepted || !strings.Contains(rec.Body.String(), "REQUESTED") {
		t.Fatalf("success answer: %d %s", rec.Code, rec.Body.String())
	}
	rec = httptest.NewRecorder()
	cvsServe(rec, httptest.NewRequest(http.MethodPost, "/", nil), 0, http.StatusOK, func(context.Context) (any, error) {
		return nil, &pgxErrorLike{}
	})
	if strings.Contains(rec.Body.String(), "secret driver text") {
		t.Fatalf("driver text leaked: %s", rec.Body.String())
	}
}

type pgxErrorLike struct{}

func (*pgxErrorLike) Error() string { return "secret driver text" }

var _ pgx.Tx
