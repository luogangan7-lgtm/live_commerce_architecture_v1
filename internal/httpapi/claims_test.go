// claims_test.go covers the M1–M7 transport rules that hold before any database work
// (contract live-keyword-claims-v1 §7.1). With a nil pool, a request that passes every
// transport rule reaches platform.WithScope and is answered 401, which these tests use as
// the "admitted to the transaction" marker. Real-PG behaviour is KC13 (test_worker).

package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"

	"livecommerce/internal/claims"
	"livecommerce/internal/command"
	"livecommerce/internal/httperror"
	"livecommerce/internal/platform"
)

const (
	claimsStore   = "11111111-1111-4111-8111-111111111111"
	claimsSession = "22222222-2222-4222-8222-222222222222"
	claimsOther   = "33333333-3333-4333-8333-333333333333"
	claimsBearer  = "Bearer AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
)

func claimsHandler(t *testing.T) http.Handler {
	t.Helper()
	labels, err := claims.NewLabelKey(make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	return NewHandler(nil, Options{ClaimLabels: &labels})
}

func claimsCall(h http.Handler, method, path, body string, edit func(*http.Request)) *httptest.ResponseRecorder {
	var r *http.Request
	if body == "" {
		r = httptest.NewRequest(method, path, nil)
	} else {
		r = httptest.NewRequest(method, path, strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
	}
	r.Header.Set("Authorization", claimsBearer)
	if method != http.MethodGet && method != http.MethodHead {
		r.Header.Set("Idempotency-Key", "claims-key-0001")
	}
	if edit != nil {
		edit(r)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func TestClaimsRoutesUnmountedWithoutLabelKey(t *testing.T) {
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/v1/admin/stores/"+claimsStore+"/live-sessions/"+claimsSession+"/claims", nil)
	r.Header.Set("Authorization", claimsBearer)
	NewHandler(nil).ServeHTTP(w, r)
	if w.Code != http.StatusNotFound {
		t.Fatalf("claims mounted without a label key: %d", w.Code)
	}
}

func TestClaimsTransportRulesBeforeDatabase(t *testing.T) {
	h := claimsHandler(t)
	base := "/v1/admin/stores/" + claimsStore + "/live-sessions/" + claimsSession + "/claims"
	noKey := func(r *http.Request) { r.Header.Del("Idempotency-Key") }
	for _, tc := range []struct {
		name, method, path, body string
		edit                     func(*http.Request)
		status                   int
		code                     string
	}{
		// Admitted requests (nil pool → 401 at platform.WithScope).
		{"M1 board", "GET", base, "", nil, 401, "unauthorized"},
		{"M2 window", "POST", base + "/window", `{"expected_version":0,"state":"OPEN","match_mode":"EXACT"}`, nil, 401, "unauthorized"},
		{"M3 offer", "POST", base + "/offers", `{"keyword":"A1","sku_id":"` + claimsOther + `","max_quantity_per_claim":3}`, nil, 401, "unauthorized"},
		{"M4 offer update", "PATCH", base + "/offers/" + claimsOther, `{"expected_version":1,"max_quantity_per_claim":3,"active":false}`, nil, 401, "unauthorized"},
		{"M5 new actor", "POST", base + "/manual", `{"actor_label":"amy","text":"A1"}`, nil, 401, "unauthorized"},
		{"M5 bundle", "POST", base + "/manual", `{"bundle_id":"` + claimsOther + `","text":"A1+2"}`, nil, 401, "unauthorized"},
		{"M6 bundles", "GET", base + "/bundles", "", nil, 401, "unauthorized"},
		{"M6 bundles paged", "GET", base + "/bundles?limit=20&cursor=abc", "", nil, 401, "unauthorized"},
		{"M7 link", "POST", base + "/bundles/" + claimsOther + "/link", `{"expected_generation":0,"release_binding":false}`, nil, 401, "unauthorized"},
		// Methods: only GET/POST/PATCH on their exact routes; HEAD never mirrors GET.
		{"M1 head", "HEAD", base, "", nil, 405, ""},
		{"M1 post", "POST", base, `{}`, nil, 405, "method_not_allowed"},
		{"M2 get", "GET", base + "/window", "", nil, 405, "method_not_allowed"},
		{"M4 delete", "DELETE", base + "/offers/" + claimsOther, "", nil, 405, "method_not_allowed"},
		{"M6 post", "POST", base + "/bundles", `{}`, nil, 405, "method_not_allowed"},
		{"M7 put", "PUT", base + "/bundles/" + claimsOther + "/link", `{}`, nil, 405, "method_not_allowed"},
		{"unknown child", "GET", base + "/labels", "", nil, 404, "not_found"},
		// Query strings: rejected everywhere except M6, which takes only limit and cursor.
		{"M1 query", "GET", base + "?x=1", "", nil, 422, "invalid_request"},
		{"M1 bare query", "GET", base + "?", "", nil, 422, "invalid_request"},
		{"M5 query", "POST", base + "/manual?label=amy", `{"actor_label":"amy","text":"A1"}`, nil, 422, "invalid_request"},
		{"M6 label filter", "GET", base + "/bundles?label=amy", "", nil, 422, "invalid_request"},
		{"M6 limit bound", "GET", base + "/bundles?limit=101", "", nil, 422, "invalid_request"},
		{"M6 duplicate", "GET", base + "/bundles?limit=1&limit=2", "", nil, 422, "invalid_request"},
		// Idempotency-Key: forbidden on reads (even empty), required once and well formed on writes.
		{"M1 key", "GET", base, "", func(r *http.Request) { r.Header["Idempotency-Key"] = []string{""} }, 422, "invalid_request"},
		{"M6 key", "GET", base + "/bundles", "", func(r *http.Request) { r.Header.Set("Idempotency-Key", "claims-key-0001") }, 422, "invalid_request"},
		{"M2 missing key", "POST", base + "/window", `{"expected_version":0,"state":"OPEN","match_mode":"EXACT"}`, noKey, 422, "invalid_request"},
		{"M7 short key", "POST", base + "/bundles/" + claimsOther + "/link", `{"expected_generation":0,"release_binding":false}`, func(r *http.Request) { r.Header.Set("Idempotency-Key", "short") }, 422, "invalid_request"},
		{"M3 duplicate key", "POST", base + "/offers", `{"keyword":"A1","sku_id":"` + claimsOther + `","max_quantity_per_claim":3}`, func(r *http.Request) { r.Header.Add("Idempotency-Key", "claims-key-0002") }, 422, "invalid_request"},
		// Path ids are canonical UUIDs.
		{"bad session", "GET", "/v1/admin/stores/" + claimsStore + "/live-sessions/NOT-A-UUID/claims", "", nil, 422, "invalid_request"},
		{"bad offer", "PATCH", base + "/offers/ABC", `{"expected_version":1,"max_quantity_per_claim":3,"active":true}`, nil, 422, "invalid_request"},
		{"bad bundle", "POST", base + "/bundles/AAAAAAAA-3333-4333-8333-333333333333/link", `{"expected_generation":0,"release_binding":false}`, nil, 422, "invalid_request"},
		// Strict JSON bodies.
		{"media", "POST", base + "/window", `{"expected_version":0,"state":"OPEN","match_mode":"EXACT"}`, func(r *http.Request) { r.Header.Set("Content-Type", "text/plain") }, 415, "json_required"},
		{"unknown key", "POST", base + "/window", `{"expected_version":0,"state":"OPEN","match_mode":"EXACT","tenant_id":"x"}`, nil, 400, "invalid_json"},
		{"duplicate json key", "POST", base + "/offers", `{"keyword":"A1","keyword":"B2","sku_id":"` + claimsOther + `","max_quantity_per_claim":3}`, nil, 400, "invalid_json"},
		{"trailing", "POST", base + "/window", `{"expected_version":0,"state":"OPEN","match_mode":"EXACT"} {}`, nil, 400, "invalid_json"},
		{"null boolean", "PATCH", base + "/offers/" + claimsOther, `{"expected_version":1,"max_quantity_per_claim":3,"active":null}`, nil, 400, "invalid_json"},
		{"null generation", "POST", base + "/bundles/" + claimsOther + "/link", `{"expected_generation":null,"release_binding":false}`, nil, 400, "invalid_json"},
		{"missing active", "PATCH", base + "/offers/" + claimsOther, `{"expected_version":1,"max_quantity_per_claim":3}`, nil, 422, "invalid_request"},
		{"missing release", "POST", base + "/bundles/" + claimsOther + "/link", `{"expected_generation":0}`, nil, 422, "invalid_request"},
		{"manual both", "POST", base + "/manual", `{"bundle_id":"` + claimsOther + `","actor_label":"amy","text":"A1"}`, nil, 422, "invalid_request"},
		{"manual neither", "POST", base + "/manual", `{"text":"A1"}`, nil, 422, "invalid_request"},
		{"manual no text", "POST", base + "/manual", `{"actor_label":"amy"}`, nil, 422, "invalid_request"},
		// Authorization shape is checked before a transaction opens.
		{"M7 no bearer", "POST", base + "/bundles/" + claimsOther + "/link", `{"expected_generation":0,"release_binding":false}`, func(r *http.Request) { r.Header.Del("Authorization") }, 401, "unauthorized"},
		{"M1 spaced bearer", "GET", base, "", func(r *http.Request) { r.Header.Set("Authorization", "Bearer a b") }, 401, "unauthorized"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := claimsCall(h, tc.method, tc.path, tc.body, tc.edit)
			if w.Code != tc.status {
				t.Fatalf("status=%d want=%d body=%s", w.Code, tc.status, w.Body.String())
			}
			if tc.code != "" {
				var envelope httperror.Envelope
				if err := json.Unmarshal(w.Body.Bytes(), &envelope); err != nil || envelope.Code != tc.code {
					t.Fatalf("code=%q want=%q", envelope.Code, tc.code)
				}
			}
			if tc.status != 404 && w.Header().Get("Cache-Control") != "private, no-store" {
				t.Fatalf("Cache-Control=%q", w.Header().Get("Cache-Control"))
			}
			if w.Header().Get("X-Content-Type-Options") != "nosniff" {
				t.Fatal("missing nosniff")
			}
			if strings.HasSuffix(strings.SplitN(tc.path, "?", 2)[0], "/link") && tc.method == "POST" && w.Header().Get("Referrer-Policy") != "no-referrer" {
				t.Fatal("M7 must forbid a Referer on every response")
			}
		})
	}
}

func TestClaimsClassifyContractMapping(t *testing.T) {
	for _, tc := range []struct {
		err    error
		status int
		code   string
	}{
		{command.ErrInvalid, 422, "invalid_request"},
		{command.ErrConflict, 409, "conflict"},
		{command.ErrNotFound, 404, "not_found"},
		{platform.ErrUnauthorized, 401, "unauthorized"},
		{platform.ErrForbidden, 403, "forbidden"},
		{platform.ErrScopeNotFound, 404, "not_found"},
		{&pgconn.PgError{Code: "40P01", Message: "deadlock label=amy"}, 503, "retry_later"},
		{&pgconn.PgError{Code: "55P03"}, 503, "retry_later"},
		{&pgconn.PgError{Code: "57014"}, 503, "retry_later"},
		{context.DeadlineExceeded, 503, "retry_later"},
		{errors.New("driver text with a label"), 503, "unavailable"},
	} {
		if status, code := claimsClassify(tc.err); status != tc.status || code != tc.code {
			t.Fatalf("%v: got %d/%s want %d/%s", tc.err, status, code, tc.status, tc.code)
		}
	}
}

func TestClaimLinkResponseShape(t *testing.T) {
	token := "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
	for _, tc := range []struct {
		out  claimLinkResponse
		want string
	}{
		{claimLinkResponse{Token: &token, Generation: 1}, `{"token":"` + token + `","generation":1,"expires_at":"0001-01-01T00:00:00Z","released":false,"replayed":false}`},
		{claimLinkResponse{Generation: 1, Replayed: true}, `{"token":null,"generation":1,"expires_at":"0001-01-01T00:00:00Z","released":false,"replayed":true}`},
	} {
		raw, err := json.Marshal(tc.out)
		if err != nil || string(raw) != tc.want {
			t.Fatalf("M7 body %s want %s (%v)", raw, tc.want, err)
		}
	}
}
