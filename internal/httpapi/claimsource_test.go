// claimsource_test.go covers the claim-source transport rules that hold before any database work
// and the frozen error-code mapping (docs/delivery/units/claim-source.md). A request that passes
// every transport rule reaches platform.WithScope with a nil pool and is answered 401, which these
// tests use as the "admitted to the transaction" marker. Real-PG behaviour: tests/foundation
// claim_source_http_test.go.

package httpapi

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"livecommerce/internal/claims"
	"livecommerce/internal/command"
	"livecommerce/internal/httperror"
	"livecommerce/internal/platform"
)

const claimSourceBody = `{"input":"https://www.facebook.com/somepage/posts/123","private_reply":true,"reply_locale":"zh-TW","active":true,"expected_version":0}`

func TestClaimSourceTransportRulesBeforeDatabase(t *testing.T) {
	h := claimsHandler(t)
	for _, spelling := range []string{"live-sessions"} { // ruling o: the only spelling
		path := "/v1/admin/stores/" + claimsStore + "/" + spelling + "/" + claimsSession + "/claim-source"
		noKey := func(r *http.Request) { r.Header.Del("Idempotency-Key") }
		for _, tc := range []struct {
			name, method, path, body string
			edit                     func(*http.Request)
			status                   int
			code                     string
		}{
			{"GET admitted", "GET", path, "", nil, 401, "unauthorized"},
			{"PUT admitted", "PUT", path, claimSourceBody, nil, 401, "unauthorized"},
			{"HEAD never mirrors GET", "HEAD", path, "", nil, 405, ""},
			{"POST", "POST", path, claimSourceBody, nil, 405, "method_not_allowed"},
			{"PATCH", "PATCH", path, claimSourceBody, nil, 405, "method_not_allowed"},
			{"DELETE", "DELETE", path, "", nil, 405, "method_not_allowed"},
			{"GET query", "GET", path + "?x=1", "", nil, 422, "invalid_request"},
			{"PUT query", "PUT", path + "?x=1", claimSourceBody, nil, 422, "invalid_request"},
			{"GET key", "GET", path, "", func(r *http.Request) { r.Header.Set("Idempotency-Key", "claims-key-0001") }, 422, "invalid_request"},
			{"PUT missing key", "PUT", path, claimSourceBody, noKey, 422, "invalid_request"},
			{"PUT short key", "PUT", path, claimSourceBody, func(r *http.Request) { r.Header.Set("Idempotency-Key", "short") }, 422, "invalid_request"},
			{"bad session id", "GET", "/v1/admin/stores/" + claimsStore + "/" + spelling + "/NOT-A-UUID/claim-source", "", nil, 422, "invalid_request"},
			{"media type", "PUT", path, claimSourceBody, func(r *http.Request) { r.Header.Set("Content-Type", "text/plain") }, 415, "json_required"},
			{"unknown key", "PUT", path, `{"input":"1","private_reply":true,"reply_locale":"en","active":true,"expected_version":0,"asset_id":"9"}`, nil, 400, "invalid_json"},
			{"null input", "PUT", path, `{"input":null,"private_reply":true,"reply_locale":"en","active":true,"expected_version":0}`, nil, 400, "invalid_json"},
			{"null bool", "PUT", path, `{"input":"1","private_reply":null,"reply_locale":"en","active":true,"expected_version":0}`, nil, 400, "invalid_json"},
			{"duplicate key", "PUT", path, `{"input":"1","input":"2","private_reply":true,"reply_locale":"en","active":true,"expected_version":0}`, nil, 400, "invalid_json"},
			{"trailing", "PUT", path, claimSourceBody + ` {}`, nil, 400, "invalid_json"},
			{"missing active", "PUT", path, `{"input":"1","private_reply":true,"reply_locale":"en","expected_version":0}`, nil, 422, "invalid_request"},
			{"missing version", "PUT", path, `{"input":"1","private_reply":true,"reply_locale":"en","active":true}`, nil, 422, "invalid_request"},
			{"no bearer", "PUT", path, claimSourceBody, func(r *http.Request) { r.Header.Del("Authorization") }, 401, "unauthorized"},
			{"platform admitted (ruling p)", "PUT", path, `{"input":"1","private_reply":false,"reply_locale":"en","active":true,"expected_version":0,"platform":"instagram"}`, nil, 401, "unauthorized"},
			{"null platform", "PUT", path, `{"input":"1","private_reply":false,"reply_locale":"en","active":true,"expected_version":0,"platform":null}`, nil, 400, "invalid_json"},
			{"platform not a string", "PUT", path, `{"input":"1","private_reply":false,"reply_locale":"en","active":true,"expected_version":0,"platform":1}`, nil, 400, "invalid_json"},
		} {
			t.Run(spelling+"/"+tc.name, func(t *testing.T) {
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
				if w.Header().Get("Cache-Control") != "private, no-store" {
					t.Fatalf("Cache-Control=%q", w.Header().Get("Cache-Control"))
				}
			})
		}
	}
}

// Ruling o: the /live/sessions/ alias is gone; only live-sessions serves the resource.
func TestClaimSourceAliasRemoved(t *testing.T) {
	h := claimsHandler(t)
	for _, method := range []string{"GET", "PUT"} {
		w := claimsCall(h, method, "/v1/admin/stores/"+claimsStore+"/live/sessions/"+claimsSession+"/claim-source", claimSourceBody, nil)
		if w.Code != http.StatusNotFound {
			t.Fatalf("%s alias: status=%d body=%s", method, w.Code, w.Body.String())
		}
	}
}

// Every claim-source error must reach the JSON body under its own code and message (the httperror table
// rewrites unknown codes to "internal": ruling 15 pattern).
func TestClaimSourceErrorCodesReachJSONBody(t *testing.T) {
	for sentinel, want := range map[error]struct {
		status int
		code   string
	}{
		claims.ErrInputInvalid:      {422, "input_invalid"},
		claims.ErrInputUnresolvable: {422, "input_unresolvable"},
		claims.ErrBindingMissing:    {409, "binding_missing"},
		claims.ErrBindingAmbiguous:  {409, "binding_ambiguous"},
		claims.ErrSourceConflict:    {409, "source_conflict"},
		claims.ErrVersionChanged:    {409, "version_changed"},
		claims.ErrPageTokenMissing:  {409, "page_token_missing"},
		platform.ErrForbidden:       {403, "forbidden"},
		command.ErrNotFound:         {404, "not_found"},
		errors.New("driver text"):   {503, "unavailable"},
	} {
		status, code := claimSourceClassify(sentinel)
		rec := httptest.NewRecorder()
		respondError(rec, status, code)
		var body struct{ Code, Message string }
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("%v: %v", sentinel, err)
		}
		if status != want.status || body.Code != want.code || body.Message == "" || body.Message == "Request could not be completed." && want.code != "internal" {
			t.Errorf("%v: %d/%q/%q, want %d/%s with its own message", sentinel, status, body.Code, body.Message, want.status, want.code)
		}
	}
}
