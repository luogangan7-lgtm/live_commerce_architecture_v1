// customers_test.go covers the customer routes' transport rules that hold BEFORE any database work (methods,
// queries, Idempotency-Key, strict bodies, path ids), the list-query grammar and the privacy error table.
// Real-PG permissions, tenancy, keyset stability and export/erasure outcomes are CB03-CB05/CB09
// (customers-billing-tests), not here.

package httpapi

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
	"livecommerce/internal/command"
	"livecommerce/internal/customers"
	"livecommerce/internal/httperror"
	"livecommerce/internal/platform"
	"livecommerce/internal/reporting"
)

const (
	custStore    = "11111111-1111-4111-8111-111111111111"
	custID       = "44444444-4444-4444-8444-444444444444"
	custBearer   = "Bearer AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
	custBase     = "/v1/admin/stores/" + custStore + "/customers"
	custWithdraw = `{"purpose":"marketing_messages","channel":"meta_dm"}`
)

// customerHandler mounts both route families on one mux exactly as NewHandler will, so a pattern conflict panics here.
func customerHandler() http.Handler {
	mux := http.NewServeMux()
	registerCustomerRoutes(mux, nil)
	registerFinanceRoutes(mux, nil)
	return httperror.Middleware(mux)
}

func customerCall(h http.Handler, method, path, body string, edit func(*http.Request)) *httptest.ResponseRecorder {
	var r *http.Request
	if body == "" {
		r = httptest.NewRequest(method, path, nil)
	} else {
		r = httptest.NewRequest(method, path, strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
	}
	r.Header.Set("Authorization", custBearer)
	if method == http.MethodPost {
		r.Header.Set("Idempotency-Key", "cust-key-0001")
	}
	if edit != nil {
		edit(r)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func TestCustomerTransportRulesBeforeDatabase(t *testing.T) {
	h := customerHandler()
	detail := custBase + "/" + custID
	withdraw, export, erase := detail+"/consent-withdrawals", detail+"/exports", detail+"/erasure"
	noKey := func(r *http.Request) { r.Header.Del("Idempotency-Key") }
	for _, tc := range []struct {
		name, method, path, body string
		edit                     func(*http.Request)
		status                   int
		code                     string
	}{
		// Admitted requests reach platform.WithScope (nil pool -> 401 unauthorized).
		{"list", "GET", custBase, "", nil, 401, "unauthorized"},
		{"list paged", "GET", custBase + "?limit=10&after=abc&q=Alice", "", nil, 401, "unauthorized"},
		{"detail", "GET", detail, "", nil, 401, "unauthorized"},
		{"withdraw", "POST", withdraw, custWithdraw, nil, 401, "unauthorized"},
		{"export", "POST", export, "", nil, 401, "unauthorized"},
		{"erase", "POST", erase, `{"confirm":"ERASE"}`, nil, 401, "unauthorized"},
		{"no bearer", "GET", custBase, "", func(r *http.Request) { r.Header.Del("Authorization") }, 401, "unauthorized"},
		{"spaced bearer", "GET", detail, "", func(r *http.Request) { r.Header.Set("Authorization", "Bearer a b") }, 401, "unauthorized"},
		// Methods: HEAD and every other verb are rejected.
		{"list head", "HEAD", custBase, "", nil, 405, ""},
		{"detail head", "HEAD", detail, "", nil, 405, ""},
		{"post list", "POST", custBase, "", nil, 405, ""},
		{"put detail", "PUT", detail, "", nil, 405, ""},
		{"delete detail", "DELETE", detail, "", nil, 405, ""},
		{"get withdraw", "GET", withdraw, "", nil, 405, ""},
		{"get export", "GET", export, "", nil, 405, ""},
		{"get erase", "GET", erase, "", nil, 405, ""},
		// Query: only the list route takes one (limit, after, q).
		{"detail query", "GET", detail + "?x=1", "", nil, 422, "invalid_request"},
		{"withdraw query", "POST", withdraw + "?x=1", custWithdraw, nil, 422, "invalid_request"},
		{"export bare query", "POST", export + "?", "", nil, 422, "invalid_request"},
		{"list unknown key", "GET", custBase + "?cursor=x", "", nil, 422, "invalid_request"},
		{"list limit 0", "GET", custBase + "?limit=0", "", nil, 422, "invalid_request"},
		{"list limit 101", "GET", custBase + "?limit=101", "", nil, 422, "invalid_request"},
		{"list limit zero padded", "GET", custBase + "?limit=010", "", nil, 422, "invalid_request"},
		{"list limit twice", "GET", custBase + "?limit=1&limit=2", "", nil, 422, "invalid_request"},
		{"list empty q", "GET", custBase + "?q=", "", nil, 422, "invalid_request"},
		{"list blank q", "GET", custBase + "?q=%20%20", "", nil, 422, "invalid_request"},
		{"list long q", "GET", custBase + "?q=" + strings.Repeat("a", 41), "", nil, 422, "invalid_request"},
		{"list control q", "GET", custBase + "?q=a%00b", "", nil, 422, "invalid_request"},
		{"list long after", "GET", custBase + "?after=" + strings.Repeat("a", 1025), "", nil, 422, "invalid_request"},
		{"list ampersand", "GET", custBase + "?limit=1&", "", nil, 422, "invalid_request"},
		// Idempotency-Key: exactly one well-formed key on POST, forbidden (even empty) on GET.
		{"withdraw no key", "POST", withdraw, custWithdraw, noKey, 422, "invalid_request"},
		{"erase short key", "POST", erase, `{"confirm":"ERASE"}`, func(r *http.Request) { r.Header.Set("Idempotency-Key", "short") }, 422, "invalid_request"},
		{"export two keys", "POST", export, "", func(r *http.Request) { r.Header.Add("Idempotency-Key", "cust-key-0002") }, 422, "invalid_request"},
		{"get with key", "GET", detail, "", func(r *http.Request) { r.Header.Set("Idempotency-Key", "cust-key-0001") }, 422, "invalid_request"},
		{"get with empty key", "GET", custBase, "", func(r *http.Request) { r.Header["Idempotency-Key"] = []string{""} }, 422, "invalid_request"},
		{"get with body", "GET", detail, "{}", nil, 422, "invalid_request"},
		// Path id.
		{"bad customer id", "GET", custBase + "/NOT-A-UUID", "", nil, 422, "invalid_request"},
		{"upper-case customer id", "POST", custBase + "/44444444-4444-4444-8444-44444444444A/erasure", `{"confirm":"ERASE"}`, nil, 422, "invalid_request"},
		// Strict bodies. Withdrawal: purpose + channel, nothing else (a merchant can never grant).
		{"withdraw media", "POST", withdraw, custWithdraw, func(r *http.Request) { r.Header.Set("Content-Type", "text/plain") }, 415, "json_required"},
		{"withdraw empty body", "POST", withdraw, "", func(r *http.Request) { r.Header.Set("Content-Type", "application/json") }, 400, "invalid_json"},
		{"withdraw granted key", "POST", withdraw, `{"purpose":"marketing_messages","channel":"meta_dm","granted":true}`, nil, 400, "invalid_json"},
		{"withdraw source key", "POST", withdraw, `{"purpose":"marketing_messages","channel":"meta_dm","source":"merchant_recorded"}`, nil, 400, "invalid_json"},
		{"withdraw missing channel", "POST", withdraw, `{"purpose":"marketing_messages"}`, nil, 422, "invalid_request"},
		{"withdraw null", "POST", withdraw, `{"purpose":null,"channel":"meta_dm"}`, nil, 400, "invalid_json"},
		{"withdraw duplicate key", "POST", withdraw, `{"purpose":"marketing_messages","purpose":"ads_personalization","channel":"meta_dm"}`, nil, 400, "invalid_json"},
		{"withdraw trailing", "POST", withdraw, custWithdraw + ` {}`, nil, 400, "invalid_json"},
		// Erasure body is exactly {"confirm":"ERASE"}.
		{"erase empty object", "POST", erase, `{}`, nil, 422, "invalid_request"},
		{"erase wrong confirm", "POST", erase, `{"confirm":"erase"}`, nil, 422, "invalid_request"},
		{"erase blank confirm", "POST", erase, `{"confirm":""}`, nil, 422, "invalid_request"},
		{"erase extra key", "POST", erase, `{"confirm":"ERASE","force":true}`, nil, 400, "invalid_json"},
		{"erase no body", "POST", erase, "", func(r *http.Request) { r.Header.Set("Content-Type", "application/json") }, 400, "invalid_json"},
		// Export takes no body.
		{"export with body", "POST", export, `{}`, nil, 422, "invalid_request"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := customerCall(h, tc.method, tc.path, tc.body, tc.edit)
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
			if tc.status != 405 && w.Header().Get("Cache-Control") != "no-store, private" {
				t.Fatalf("Cache-Control=%q", w.Header().Get("Cache-Control"))
			}
		})
	}
}

func TestParseCustomersQuery(t *testing.T) {
	parse := func(raw string) (customers.ListRequest, error) {
		u := &url.URL{Scheme: "http", Host: "x", Path: "/", RawQuery: raw}
		return parseCustomersQuery(u)
	}
	in, err := parse("")
	if err != nil || in.Page.Limit != 0 || in.Page.Cursor != "" || in.Q != "" {
		t.Fatalf("%+v %v", in, err)
	}
	in, err = parse("limit=100&after=abc_-&q=%20Alice%20")
	if err != nil || in.Page.Limit != 100 || in.Page.Cursor != "abc_-" || in.Q != " Alice " {
		t.Fatalf("%+v %v", in, err)
	}
	for _, bad := range []string{"limit=abc", "limit=-1", "limit=1.5", "after=", "q=", "x=1", "limit=1&limit=2", "limit", "&", "q=%ff"} {
		if _, err := parse(bad); !errors.Is(err, command.ErrInvalid) {
			t.Fatalf("%q accepted: %v", bad, err)
		}
	}
}

func TestCustomersClassify(t *testing.T) {
	pg := func(code string) error { return &pgconn.PgError{Code: code} }
	for _, tc := range []struct {
		name   string
		err    error
		status int
		code   string
	}{
		{"idempotency", customers.ErrIdempotencyConflict, 409, "idempotency_conflict"},
		{"blocked", customers.ErrErasureBlocked, 409, "erasure_blocked"},
		{"too large", customers.ErrExportTooLarge, 409, "export_too_large"},
		{"erased", customers.ErrErased, 410, "erased"},
		{"unavailable", customers.ErrUnavailable, 503, "unavailable"},
		{"finance unavailable", reporting.ErrUnavailable, 503, "unavailable"},
		{"invalid", command.ErrInvalid, 422, "invalid_request"},
		{"not found", platform.ErrScopeNotFound, 404, "not_found"},
		{"forbidden", platform.ErrForbidden, 403, "forbidden"},
		{"unauthorized", platform.ErrUnauthorized, 401, "unauthorized"},
		{"deadlock", pg("40P01"), 503, "retry_later"},
		{"lock timeout", pg("55P03"), 503, "retry_later"},
		{"unique race", pg("23505"), 409, "conflict"},
		{"unknown", errors.New("Alice 0912345678"), 503, "unavailable"},
		{"wrapped", errors.Join(errors.New("ctx"), customers.ErrErased), 410, "erased"},
	} {
		if status, code := customersClassify(tc.err); status != tc.status || code != tc.code {
			t.Fatalf("%s: %d %s want %d %s", tc.name, status, code, tc.status, tc.code)
		}
	}
}

func TestWriteAttachment(t *testing.T) {
	w := httptest.NewRecorder()
	writeAttachment(w, "application/json", `attachment; filename="customer-`+custID+`.json"`, []byte(`{"a":1}`))
	if w.Code != 200 || w.Header().Get("Cache-Control") != "no-store" || w.Header().Get("Content-Length") != "7" ||
		!strings.HasPrefix(w.Header().Get("Content-Disposition"), "attachment;") || w.Header().Get("Content-Type") != "application/json" {
		t.Fatalf("%d %v", w.Code, w.Header())
	}
}
