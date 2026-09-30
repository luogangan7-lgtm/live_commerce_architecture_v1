package httpapi

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"livecommerce/internal/command"
	"livecommerce/internal/httperror"
	"livecommerce/internal/merchantorders"
	"livecommerce/internal/platform"
)

const (
	shipStore   = "11111111-1111-4111-8111-111111111111"
	shipOrder   = "33333333-3333-4333-8333-333333333333"
	shipBearer  = "Bearer AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
	shipBodyOK  = `{"expected_version":0,"status":"SHIPPED","carrier_code":"seven_eleven_cvs","carrier_name":null,"tracking_number":"0012345678","tracking_url":null,"note":null,"void_reason":null}`
	shipVoidOK  = `{"expected_version":1,"status":"VOIDED","carrier_code":null,"carrier_name":null,"tracking_number":null,"tracking_url":null,"note":null,"void_reason":"wrong_order"}`
	shipOrders  = "/v1/admin/stores/" + shipStore + "/orders"
	shipmentURL = shipOrders + "/" + shipOrder + "/shipment"
)

// shipmentHandler mounts the order and shipment routes on one mux exactly as NewHandler will, so a
// pattern conflict (unshipped.csv vs {order_id}) would panic here.
func shipmentHandler() http.Handler {
	mux := http.NewServeMux()
	registerOrderRoutes(mux, nil)
	registerShipmentRoutes(mux, nil)
	return httperror.Middleware(mux)
}

func shipmentCall(h http.Handler, method, path, body string, edit func(*http.Request)) *httptest.ResponseRecorder {
	var r *http.Request
	if body == "" {
		r = httptest.NewRequest(method, path, nil)
	} else {
		r = httptest.NewRequest(method, path, strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
	}
	r.Header.Set("Authorization", shipBearer)
	if method == http.MethodPut {
		r.Header.Set("Idempotency-Key", "ship-key-0001")
	}
	if edit != nil {
		edit(r)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func TestShipmentTransportRulesBeforeDatabase(t *testing.T) {
	h := shipmentHandler()
	history := shipmentURL + "/history"
	csv := shipOrders + "/unshipped.csv"
	actions := "/v1/admin/stores/" + shipStore + "/order-actions"
	noKey := func(r *http.Request) { r.Header.Del("Idempotency-Key") }
	for _, tc := range []struct {
		name, method, path, body string
		edit                     func(*http.Request)
		status                   int
		code                     string
	}{
		// Admitted requests reach platform.WithScope (nil pool -> 401).
		{"put record", "PUT", shipmentURL, shipBodyOK, nil, 401, "unauthorized"},
		{"put void", "PUT", shipmentURL, shipVoidOK, nil, 401, "unauthorized"},
		{"history", "GET", history, "", nil, 401, "unauthorized"},
		{"csv", "GET", csv, "", nil, 401, "unauthorized"},
		{"actions", "GET", actions, "", nil, 401, "unauthorized"},
		{"no bearer", "PUT", shipmentURL, shipBodyOK, func(r *http.Request) { r.Header.Del("Authorization") }, 401, "unauthorized"},
		{"spaced bearer", "GET", history, "", func(r *http.Request) { r.Header.Set("Authorization", "Bearer a b") }, 401, "unauthorized"},
		// Methods: HEAD and every other verb are rejected, also on the PUT route.
		{"put head", "HEAD", shipmentURL, "", nil, 405, ""},
		{"history head", "HEAD", history, "", nil, 405, ""},
		{"csv head", "HEAD", csv, "", nil, 405, ""},
		{"actions head", "HEAD", actions, "", nil, 405, ""},
		{"post shipment", "POST", shipmentURL, shipBodyOK, nil, 405, "method_not_allowed"},
		{"patch shipment", "PATCH", shipmentURL, shipBodyOK, nil, 405, "method_not_allowed"},
		{"get shipment", "GET", shipmentURL, "", nil, 405, "method_not_allowed"},
		{"put history", "PUT", history, shipBodyOK, nil, 405, "method_not_allowed"},
		{"post csv", "POST", csv, "", nil, 405, "method_not_allowed"},
		// Queries are rejected on all four routes.
		{"put query", "PUT", shipmentURL + "?x=1", shipBodyOK, nil, 422, "invalid_request"},
		{"put bare query", "PUT", shipmentURL + "?", shipBodyOK, nil, 422, "invalid_request"},
		{"history query", "GET", history + "?limit=1", "", nil, 422, "invalid_request"},
		{"csv query", "GET", csv + "?state=all", "", nil, 422, "invalid_request"},
		{"actions query", "GET", actions + "?x=1", "", nil, 422, "invalid_request"},
		// Idempotency-Key: required once and well formed on PUT, forbidden (even empty) on reads.
		{"missing key", "PUT", shipmentURL, shipBodyOK, noKey, 422, "invalid_request"},
		{"short key", "PUT", shipmentURL, shipBodyOK, func(r *http.Request) { r.Header.Set("Idempotency-Key", "short") }, 422, "invalid_request"},
		{"two keys", "PUT", shipmentURL, shipBodyOK, func(r *http.Request) { r.Header.Add("Idempotency-Key", "ship-key-0002") }, 422, "invalid_request"},
		{"read with key", "GET", history, "", func(r *http.Request) { r.Header.Set("Idempotency-Key", "ship-key-0001") }, 422, "invalid_request"},
		{"read with empty key", "GET", csv, "", func(r *http.Request) { r.Header["Idempotency-Key"] = []string{""} }, 422, "invalid_request"},
		{"read with body", "GET", history, "{}", nil, 422, "invalid_request"},
		// Path ids.
		{"bad order id put", "PUT", shipOrders + "/NOT-A-UUID/shipment", shipBodyOK, nil, 422, "invalid_request"},
		{"bad order id history", "GET", shipOrders + "/AAAAAAAA-3333-4333-8333-333333333333/shipment/history", "", nil, 422, "invalid_request"},
		// Strict bodies: eight keys exactly, nullable ones explicit null.
		{"media", "PUT", shipmentURL, shipBodyOK, func(r *http.Request) { r.Header.Set("Content-Type", "text/plain") }, 415, "json_required"},
		{"empty body", "PUT", shipmentURL, "", func(r *http.Request) { r.Header.Set("Content-Type", "application/json") }, 400, "invalid_json"},
		{"unknown key", "PUT", shipmentURL, strings.Replace(shipBodyOK, `"note":null`, `"note":null,"tenant_id":"x"`, 1), nil, 400, "invalid_json"},
		{"duplicate key", "PUT", shipmentURL, strings.Replace(shipBodyOK, `"note":null`, `"note":null,"note":"a"`, 1), nil, 400, "invalid_json"},
		{"trailing data", "PUT", shipmentURL, shipBodyOK + ` {}`, nil, 400, "invalid_json"},
		{"top-level null", "PUT", shipmentURL, `null`, nil, 422, "invalid_request"},
		{"wrong type", "PUT", shipmentURL, strings.Replace(shipBodyOK, `"expected_version":0`, `"expected_version":"0"`, 1), nil, 400, "invalid_json"},
		{"missing nullable key", "PUT", shipmentURL, strings.Replace(shipBodyOK, `,"note":null`, ``, 1), nil, 422, "invalid_request"},
		{"missing void_reason key", "PUT", shipmentURL, strings.Replace(shipBodyOK, `,"void_reason":null`, ``, 1), nil, 422, "invalid_request"},
		{"null expected_version", "PUT", shipmentURL, strings.Replace(shipBodyOK, `"expected_version":0`, `"expected_version":null`, 1), nil, 422, "invalid_request"},
		{"null status", "PUT", shipmentURL, strings.Replace(shipBodyOK, `"status":"SHIPPED"`, `"status":null`, 1), nil, 422, "invalid_request"},
		{"oversized", "PUT", shipmentURL, `{"note":"` + strings.Repeat("a", 70<<10) + `"}`, nil, 400, "invalid_json"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := shipmentCall(h, tc.method, tc.path, tc.body, tc.edit)
			if w.Code != tc.status {
				t.Fatalf("status=%d want=%d body=%s", w.Code, tc.status, w.Body.String())
			}
			if tc.code != "" {
				var envelope httperror.Envelope
				if err := json.Unmarshal(w.Body.Bytes(), &envelope); err != nil || envelope.Code != tc.code {
					t.Fatalf("code=%q want=%q", envelope.Code, tc.code)
				}
			}
			if w.Header().Get("X-Content-Type-Options") != "nosniff" {
				t.Fatal("missing nosniff")
			}
			// Own routes are private and non-cacheable on every response (HEAD/405 fallbacks aside).
			if tc.status != 405 && w.Header().Get("Cache-Control") != "no-store, private" {
				t.Fatalf("Cache-Control=%q", w.Header().Get("Cache-Control"))
			}
		})
	}
}

// The mux must still route the sibling order routes and never let {order_id} swallow the CSV.
func TestShipmentRoutesDoNotShadowOrderRoutes(t *testing.T) {
	h := shipmentHandler()
	for path, want := range map[string]int{
		shipOrders:                        401,
		shipOrders + "/" + shipOrder:      401,
		shipOrders + "/unshipped.csv":     401,
		shipOrders + "/unshipped.csv?x=1": 422,
		shipOrders + "/unshipped":         422, // treated as an order id, not the CSV
	} {
		if w := shipmentCall(h, "GET", path, "", nil); w.Code != want {
			t.Errorf("%s => %d want %d", path, w.Code, want)
		}
	}
}

func TestShipmentClassifyMapping(t *testing.T) {
	for _, tc := range []struct {
		err    error
		status int
		code   string
	}{
		{merchantorders.ErrVersionChanged, 409, "version_changed"}, {merchantorders.ErrNotShippable, 422, "not_shippable"},
		{merchantorders.ErrInvalidCarrier, 422, "invalid_carrier"}, {merchantorders.ErrInvalidTracking, 422, "invalid_tracking"},
		{merchantorders.ErrInvalidURL, 422, "invalid_url"}, {merchantorders.ErrVoidRequiresShipped, 422, "void_requires_shipped"},
		{merchantorders.ErrInvalidVoid, 422, "invalid_void"}, {command.ErrConflict, 409, "conflict"}, {command.ErrInvalid, 422, "invalid_request"},
		{platform.ErrForbidden, 403, "forbidden"}, {platform.ErrScopeNotFound, 404, "not_found"}, {platform.ErrUnauthorized, 401, "unauthorized"},
		{merchantorders.ErrUnavailable, 503, "unavailable"}, {&pgconn.PgError{Code: "40P01", Message: "deadlock with private value"}, 503, "retry_later"},
		{errors.New("sensitive connection string"), 503, "unavailable"},
	} {
		if status, code := shipmentClassify(tc.err); status != tc.status || code != tc.code {
			t.Errorf("%v => %d %s want %d %s", tc.err, status, code, tc.status, tc.code)
		}
	}
}

// The §5.1 codes live in httperror's message table (ruling 15); the envelope keeps request id and details.
func TestShipmentErrorEnvelopeKeepsContractCodes(t *testing.T) {
	for _, code := range []string{"version_changed", "not_shippable", "invalid_carrier", "invalid_tracking", "invalid_url", "void_requires_shipped", "invalid_void"} {
		w := httptest.NewRecorder()
		w.Header().Set("X-Request-ID", "rid-1")
		respondError(w, 422, code)
		var envelope httperror.Envelope
		if err := json.Unmarshal(w.Body.Bytes(), &envelope); err != nil || envelope.Code != code || envelope.Message == "" ||
			envelope.RequestID != "rid-1" || envelope.Retryable || envelope.Details == nil {
			t.Errorf("%s: %+v %v", code, envelope, err)
		}
	}
	w := httptest.NewRecorder()
	respondError(w, 409, "conflict")
	if !strings.Contains(w.Body.String(), `"code":"conflict"`) {
		t.Fatalf("shared code lost: %s", w.Body.String())
	}
	w = httptest.NewRecorder()
	respondError(w, 503, "retry_later")
	if !strings.Contains(w.Body.String(), `"retryable":true`) {
		t.Fatalf("503 must stay retryable: %s", w.Body.String())
	}
}

func TestWriteExportHeaders(t *testing.T) {
	w := httptest.NewRecorder()
	body := []byte("\xEF\xBB\xBForder_id\r\n")
	writeExport(w, shipStore, merchantorders.Export{Body: body, Rows: 0, Truncated: true}, time.Date(2026, 9, 29, 13, 7, 59, 0, time.UTC))
	for name, want := range map[string]string{
		"Content-Type":        "text/csv; charset=utf-8",
		"Content-Disposition": `attachment; filename="unshipped-11111111-202609291307.csv"`,
		"Cache-Control":       "no-store, private",
		"X-Export-Truncated":  "true",
	} {
		if got := w.Header().Get(name); got != want {
			t.Errorf("%s=%q want %q", name, got, want)
		}
	}
	if w.Code != 200 || w.Body.String() != string(body) {
		t.Fatalf("body/status: %d %q", w.Code, w.Body.String())
	}
	w = httptest.NewRecorder()
	writeExport(w, shipStore, merchantorders.Export{Body: body}, time.Now())
	if w.Header().Get("X-Export-Truncated") != "false" {
		t.Fatal("truncation flag must be explicit false")
	}
}

func TestOrdersListAdmitsShipmentFilters(t *testing.T) {
	for _, state := range []string{"shipped", "unshipped"} {
		u := httptest.NewRequest("GET", shipOrders+"?limit=5&state="+state, nil).URL
		in, err := parseOrdersQuery(u)
		if err != nil || in.State != state {
			t.Fatalf("%s: %+v %v", state, in, err)
		}
	}
	if _, err := parseOrdersQuery(httptest.NewRequest("GET", shipOrders+"?state=Shipped", nil).URL); err == nil {
		t.Fatal("filter names are case-sensitive")
	}
}
