// ads_test.go covers the meta-ads-v1 §7 (brief D1/D5/D6) transport rules that hold before any database work. With a nil pool, a
// request that passes every transport rule reaches platform.WithScope (or ads.Service.Callback) and is answered 401, which these
// tests use as the "admitted to the transaction" marker. Real-PG behaviour (MA02, MA09) belongs to the independent test unit.

package httpapi

import (
	"bytes"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"

	"livecommerce/internal/ads"
	"livecommerce/internal/command"
)

const (
	adsStore  = "11111111-1111-4111-8111-111111111111"
	adsDraft  = "22222222-2222-4222-8222-222222222222"
	adsBase   = "/v1/admin/stores/" + adsStore + "/ads"
	adsBearer = "Bearer AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
	adsDraftB = `{"ad_binding_id":"11111111-1111-4111-8111-111111111111","identity_binding_id":"33333333-3333-4333-8333-333333333333",` +
		`"template":"BOOST_POST","source_ref":"111_222","currency":"TWD","lifetime_budget_minor":300000,` +
		`"starts_at":"2030-01-01T00:00:00Z","ends_at":"2030-01-03T00:00:00Z","countries":["TW"],"age_min":18,"age_max":65}`
	adsState = "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
)

func adsHandler(t *testing.T) http.Handler {
	t.Helper()
	jobs, err := river.NewClient(riverpgxv5.New(nil), &river.Config{Schema: "river"})
	if err != nil {
		t.Fatal(err)
	}
	svc, err := ads.NewService(jobs, nil, ads.DialogConfig{AppID: "1", ConfigID: "2", RedirectURI: "https://a.example.test/cb", GraphVersion: "v26.0", StateKey: bytes.Repeat([]byte{7}, 32)})
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	registerAdsRoutes(mux, nil, svc)
	return mux
}

func adsCall(h http.Handler, method, path, body string, edit func(*http.Request)) *httptest.ResponseRecorder {
	var r *http.Request
	if body == "" {
		r = httptest.NewRequest(method, path, nil)
	} else {
		r = httptest.NewRequest(method, path, strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
	}
	r.Header.Set("Authorization", adsBearer)
	if edit != nil {
		edit(r)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func adsKey(r *http.Request) { r.Header.Set("Idempotency-Key", "ads-key-000001") }

func adsKeyMatch(r *http.Request) {
	adsKey(r)
	r.Header.Set("If-Match", "3")
}

func TestAdsRoutesAreUnmountedWithoutAService(t *testing.T) {
	mux := http.NewServeMux()
	registerAdsRoutes(mux, nil, nil)
	if w := adsCall(mux, http.MethodGet, adsBase+"/settings", "", nil); w.Code != http.StatusNotFound {
		t.Fatalf("status=%d", w.Code)
	}
}

func TestAdsTransportRules(t *testing.T) {
	h := adsHandler(t)
	admitted := http.StatusUnauthorized // nil pool: reached platform.WithScope
	cases := []struct {
		name         string
		method, path string
		body         string
		edit         func(*http.Request)
		want         int
	}{
		{"connect ok", "POST", "/meta/connect", "", adsKey, admitted},
		{"connect needs key", "POST", "/meta/connect", "", nil, 422},
		{"connect rejects body", "POST", "/meta/connect", `{}`, adsKey, 422},
		{"connect rejects query", "POST", "/meta/connect?x=1", "", adsKey, 422},
		{"connect bad key", "POST", "/meta/connect", "", func(r *http.Request) { r.Header.Set("Idempotency-Key", "short") }, 422},
		{"connect two keys", "POST", "/meta/connect", "", func(r *http.Request) {
			r.Header.Add("Idempotency-Key", "ads-key-000001")
			r.Header.Add("Idempotency-Key", "ads-key-000002")
		}, 422},
		{"connect wrong method", "GET", "/meta/connect", "", nil, 405},
		{"states get", "GET", "/meta/states/" + adsDraft, "", nil, admitted},
		{"states bad id", "GET", "/meta/states/nope", "", nil, 422},
		{"states with key", "GET", "/meta/states/" + adsDraft, "", adsKey, 422},
		{"settings", "GET", "/settings", "", nil, admitted},
		{"settings query", "GET", "/settings?x=1", "", nil, 422},
		{"settings post", "POST", "/settings", "", adsKey, 405},
		{"drafts list", "GET", "/drafts", "", nil, admitted},
		{"draft get", "GET", "/drafts/" + adsDraft, "", nil, admitted},
		{"draft get bad id", "GET", "/drafts/nope", "", nil, 422},
		{"draft create ok", "POST", "/drafts", adsDraftB, adsKey, admitted},
		{"draft create no key", "POST", "/drafts", adsDraftB, nil, 422},
		{"draft create wrong content type", "POST", "/drafts", adsDraftB, func(r *http.Request) { adsKey(r); r.Header.Set("Content-Type", "text/plain") }, 415},
		{"draft create unknown field", "POST", "/drafts", strings.Replace(adsDraftB, `"age_max":65`, `"age_max":65,"admin":true`, 1), adsKey, 400},
		{"draft create null", "POST", "/drafts", strings.Replace(adsDraftB, `"source_ref":"111_222"`, `"source_ref":null`, 1), adsKey, 400},
		{"draft create duplicate key", "POST", "/drafts", strings.Replace(adsDraftB, `"age_max":65`, `"age_max":65,"age_max":66`, 1), adsKey, 400},
		{"draft create missing field", "POST", "/drafts", strings.Replace(adsDraftB, `"age_max":65`, ``, 1), adsKey, 400},
		{"draft create bad time", "POST", "/drafts", strings.Replace(adsDraftB, `2030-01-01T00:00:00Z`, `tomorrow`, 1), adsKey, 400},
		{"draft create trailing data", "POST", "/drafts", adsDraftB + `{}`, adsKey, 400},
		{"draft update ok", "PUT", "/drafts/" + adsDraft, adsDraftB, adsKeyMatch, admitted},
		{"draft update needs If-Match", "PUT", "/drafts/" + adsDraft, adsDraftB, adsKey, 422},
		{"draft update If-Match junk", "PUT", "/drafts/" + adsDraft, adsDraftB, func(r *http.Request) { adsKey(r); r.Header.Set("If-Match", "\"3\"") }, 422},
		{"draft update If-Match zero", "PUT", "/drafts/" + adsDraft, adsDraftB, func(r *http.Request) { adsKey(r); r.Header.Set("If-Match", "0") }, 422},
		{"approve ok", "POST", "/drafts/" + adsDraft + "/approve", `{"revision":1}`, nil, admitted},
		{"approve takes no key", "POST", "/drafts/" + adsDraft + "/approve", `{"revision":1}`, adsKey, 422},
		{"approve missing revision", "POST", "/drafts/" + adsDraft + "/approve", `{}`, nil, 422},
		{"approve unknown field", "POST", "/drafts/" + adsDraft + "/approve", `{"revision":1,"force":true}`, nil, 400},
		{"publish ok", "POST", "/drafts/" + adsDraft + "/publish", `{"publish_attempt":0}`, adsKey, admitted},
		{"publish needs key", "POST", "/drafts/" + adsDraft + "/publish", `{"publish_attempt":0}`, nil, 422},
		{"publish missing attempt", "POST", "/drafts/" + adsDraft + "/publish", `{}`, adsKey, 422},
		{"pause ok", "POST", "/drafts/" + adsDraft + "/pause", "", adsKey, admitted},
		{"pause rejects body", "POST", "/drafts/" + adsDraft + "/pause", `{}`, adsKey, 422},
		{"end ok", "POST", "/drafts/" + adsDraft + "/end", "", adsKey, admitted},
		{"end bad id", "POST", "/drafts/nope/end", "", adsKey, 422},
		{"report ok", "GET", "/report?from=2026-09-01&to=2026-09-30", "", nil, admitted},
		{"report no query", "GET", "/report", "", nil, 422},
		{"report extra param", "GET", "/report?from=2026-09-01&to=2026-09-30&x=1", "", nil, 422},
		{"report duplicate", "GET", "/report?from=2026-09-01&from=2026-09-02&to=2026-09-30", "", nil, 422},
		{"report bad date", "GET", "/report?from=09-01&to=2026-09-30", "", nil, 422},
		{"capi ok", "PUT", "/capi", `{"enabled":false}`, adsKey, admitted},
		{"capi full", "PUT", "/capi", `{"enabled":true,"dataset_binding_id":"33333333-3333-4333-8333-333333333333","test_event_code":"TEST1234"}`, adsKey, admitted},
		{"capi needs key", "PUT", "/capi", `{"enabled":false}`, nil, 422},
		{"capi missing enabled", "PUT", "/capi", `{}`, adsKey, 422},
		{"capi null optional", "PUT", "/capi", `{"enabled":true,"test_event_code":null}`, adsKey, 400},
		{"bindings ok", "POST", "/meta/bindings", `{"state_id":"` + adsDraft + `","ad_account_id":"9001"}`, adsKey, admitted},
		{"bindings with dataset", "POST", "/meta/bindings", `{"state_id":"` + adsDraft + `","ad_account_id":"9001","dataset_id":"7001"}`, adsKey, admitted},
		{"bindings missing account", "POST", "/meta/bindings", `{"state_id":"` + adsDraft + `"}`, adsKey, 422},
		{"bindings unknown field", "POST", "/meta/bindings", `{"state_id":"x","ad_account_id":"1","token":"t"}`, adsKey, 400},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := adsCall(h, tc.method, adsBase+tc.path, tc.body, tc.edit)
			if w.Code != tc.want {
				t.Fatalf("status=%d want %d body=%s", w.Code, tc.want, w.Body.String())
			}
			if cc := w.Header().Get("Cache-Control"); cc != "private, no-store" {
				t.Fatalf("Cache-Control=%q", cc)
			}
		})
	}
	if w := adsCall(h, http.MethodGet, adsBase+"/settings", "", func(r *http.Request) { r.Header.Del("Authorization") }); w.Code != http.StatusUnauthorized {
		t.Fatalf("no bearer: %d", w.Code)
	}
	if w := adsCall(h, http.MethodGet, "/v1/admin/stores/nope/ads/settings", "", nil); w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("bad store id: %d", w.Code)
	}
}

func TestAdsCallbackTransportAndSecrecy(t *testing.T) {
	h := adsHandler(t)
	const secretCode = "SECRETCODE-9f3a"
	q := func(s string) string { return adsBase + "/meta/callback" + s }
	cases := []struct {
		name string
		path string
		want int
	}{
		{"valid shape reaches the service", q("?code=" + secretCode + "&state=" + adsState), http.StatusUnauthorized},
		{"no query", q(""), 422},
		{"missing state", q("?code=" + secretCode), 422},
		{"extra parameter", q("?code=" + secretCode + "&state=" + adsState + "&error=x"), 422},
		{"duplicate code", q("?code=a&code=b&state=" + adsState), 422},
		{"code with space", q("?code=a%20b&state=" + adsState), 422},
		{"state wrong shape is a mismatch", q("?code=" + secretCode + "&state=short"), 409},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := adsCall(h, http.MethodGet, tc.path, "", nil)
			if w.Code != tc.want {
				t.Fatalf("status=%d want %d body=%s", w.Code, tc.want, w.Body.String())
			}
			if strings.Contains(w.Body.String(), secretCode) || strings.Contains(w.Body.String(), adsState) {
				t.Fatalf("response echoed the OAuth code or state: %s", w.Body.String())
			}
		})
	}
	if w := adsCall(h, http.MethodGet, q("?code=a&state="+adsState), "", adsKey); w.Code != 422 {
		t.Fatalf("callback with key: %d", w.Code)
	}
}

func TestAdsClassify(t *testing.T) {
	cases := []struct {
		err    error
		status int
		code   string
	}{
		{errors.New("x"), 503, "unavailable"},
		{ads.ErrConnectFailed, 502, "meta_connect_failed"},
		{command.ErrConflict, 409, "conflict"},
		{command.ErrInvalid, 422, "invalid_request"},
		{pgx.ErrNoRows, 404, "not_found"},
	}
	for _, tc := range cases {
		if status, code := adsClassify(tc.err); status != tc.status || code != tc.code {
			t.Fatalf("%v -> %d %s", tc.err, status, code)
		}
	}
}
