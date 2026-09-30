// privacy_test.go covers the buyer privacy transport rules that hold BEFORE the buyer transaction (strict
// bodies, typed confirmation, key mapping) and the customers-error to HTTP mapping. Real-PG consent, export
// and erasure behaviour is CB04/CB05/CB09 (customers-billing-tests), not here.

package buyerhttp

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"livecommerce/internal/buyer"
	"livecommerce/internal/command"
	"livecommerce/internal/customers"
)

const (
	privacyToken = "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
	privacyStore = "11111111-1111-4111-8111-111111111111"
	privacyKey   = "privacy-key-0001"
)

func privacyRequest(method, body string) *http.Request {
	r := httptest.NewRequest(method, "http://internal/v1/buyer/consents", strings.NewReader(body))
	if body != "" {
		r.Header.Set("Content-Type", "application/json")
	}
	return r
}

func statusOf(t *testing.T, err error) (int, string) {
	t.Helper()
	var response responseError
	if !errors.As(err, &response) {
		t.Fatalf("not a responseError: %v", err)
	}
	return response.status, response.code
}

func TestConsentPutBodyIsStrictBeforeDatabase(t *testing.T) {
	h := &handler{} // nil pool: a request that passes the body gate reaches buyer.WithScope and is unauthorized
	ok := `{"purpose":"ads_personalization","channel":"meta_ads","granted":true,"context":"checkout"}`
	if err := h.consentPut(context.Background(), httptest.NewRecorder(), privacyRequest("PUT", ok), privacyStore, privacyToken, privacyKey); !errors.Is(err, buyer.ErrUnauthorized) {
		t.Fatalf("valid body must reach the buyer transaction: %v", err)
	}
	for name, tc := range map[string]struct {
		body   string
		status int
		code   string
	}{
		"source key":         {`{"purpose":"ads_personalization","channel":"meta_ads","granted":true,"context":"checkout","source":"buyer_checkout"}`, 422, "invalid_request"},
		"policy_version key": {`{"purpose":"ads_personalization","channel":"meta_ads","granted":true,"context":"checkout","policy_version":"lc-2026-10"}`, 422, "invalid_request"},
		"tenant key":         {`{"purpose":"ads_personalization","channel":"meta_ads","granted":true,"context":"checkout","tenant_id":"x"}`, 422, "invalid_request"},
		"missing context":    {`{"purpose":"ads_personalization","channel":"meta_ads","granted":true}`, 422, "invalid_request"},
		"missing granted":    {`{"purpose":"ads_personalization","channel":"meta_ads","context":"checkout"}`, 422, "invalid_request"},
		"empty object":       {`{}`, 422, "invalid_request"},
		"granted as string":  {`{"purpose":"ads_personalization","channel":"meta_ads","granted":"yes","context":"checkout"}`, 422, "invalid_request"},
		"purpose as number":  {`{"purpose":1,"channel":"meta_ads","granted":true,"context":"checkout"}`, 422, "invalid_request"},
		"null value":         {`{"purpose":null,"channel":"meta_ads","granted":true,"context":"checkout"}`, 400, "invalid_json"},
		"not json":           {`purpose=x`, 400, "invalid_json"},
		"array":              {`[]`, 400, "invalid_json"},
	} {
		err := h.consentPut(context.Background(), httptest.NewRecorder(), privacyRequest("PUT", tc.body), privacyStore, privacyToken, privacyKey)
		if status, code := statusOf(t, err); status != tc.status || code != tc.code {
			t.Fatalf("%s: %d %s want %d %s", name, status, code, tc.status, tc.code)
		}
	}
	r := privacyRequest("PUT", ok)
	r.Header.Set("Content-Type", "text/plain")
	if status, code := statusOf(t, h.consentPut(context.Background(), httptest.NewRecorder(), r, privacyStore, privacyToken, privacyKey)); status != 415 || code != "json_required" {
		t.Fatalf("media type: %d %s", status, code)
	}
}

func TestErasureNeedsTheTypedConfirmation(t *testing.T) {
	h := &handler{}
	if err := h.privacyErase(context.Background(), httptest.NewRecorder(), privacyRequest("POST", `{"confirm":"ERASE"}`), privacyStore, privacyToken, privacyKey); !errors.Is(err, buyer.ErrUnauthorized) {
		t.Fatalf("typed confirmation must reach the buyer transaction: %v", err)
	}
	for name, body := range map[string]string{
		"lower case": `{"confirm":"erase"}`, "blank": `{"confirm":""}`, "yes": `{"confirm":"yes"}`,
		"trailing space": `{"confirm":"ERASE "}`, "extra key": `{"confirm":"ERASE","force":true}`, "missing": `{}`, "boolean": `{"confirm":true}`,
	} {
		err := h.privacyErase(context.Background(), httptest.NewRecorder(), privacyRequest("POST", body), privacyStore, privacyToken, privacyKey)
		if status, code := statusOf(t, err); status != 422 || code != "invalid_request" {
			t.Fatalf("%s: %d %s", name, status, code)
		}
	}
}

func TestExportRejectsABodyAndReadIsNotBodyBound(t *testing.T) {
	h := &handler{}
	err := h.privacyExport(context.Background(), httptest.NewRecorder(), privacyRequest("POST", `{}`), privacyStore, privacyToken, privacyKey)
	if status, code := statusOf(t, err); status != 422 || code != "invalid_request" {
		t.Fatalf("%d %s", status, code)
	}
	r := httptest.NewRequest("POST", "http://internal/v1/buyer/privacy/export", nil)
	if err := h.privacyExport(context.Background(), httptest.NewRecorder(), r, privacyStore, privacyToken, privacyKey); !errors.Is(err, buyer.ErrUnauthorized) {
		t.Fatalf("bodyless export must reach the buyer transaction: %v", err)
	}
	if err := h.privacyGet(context.Background(), httptest.NewRecorder(), httptest.NewRequest("GET", "http://internal/v1/buyer/privacy", nil), privacyStore, privacyToken, ""); !errors.Is(err, buyer.ErrUnauthorized) {
		t.Fatalf("read must reach the buyer transaction: %v", err)
	}
}

func TestPrivacyErrorMapping(t *testing.T) {
	for _, tc := range []struct {
		err    error
		status int
		code   string
	}{
		{customers.ErrIdempotencyConflict, 409, "idempotency_conflict"},
		{customers.ErrErasureBlocked, 409, "erasure_blocked"},
		{customers.ErrExportTooLarge, 409, "export_too_large"},
		{customers.ErrErased, 410, "erased"},
	} {
		if status, code := statusOf(t, privacyError(tc.err)); status != tc.status || code != tc.code {
			t.Fatalf("%v: %d %s", tc.err, status, code)
		}
	}
	// Everything else is left to classify (no edit to it, brief): 401 / 422 / 503.
	for err, want := range map[error]int{buyer.ErrUnauthorized: 401, command.ErrInvalid: 422, customers.ErrUnavailable: 503, errors.New("boom"): 503} {
		mapped := privacyError(err)
		if !errors.Is(mapped, err) {
			t.Fatalf("%v rewritten to %v", err, mapped)
		}
		if status, _ := classify(mapped); status != want {
			t.Fatalf("%v: classify %d want %d", err, status, want)
		}
	}
}

func TestBuyerTransactionRefusesWithoutAPool(t *testing.T) {
	if _, err := buyerTransaction(context.Background(), nil, func(context.Context, pgx.Tx) (int, error) { return 1, nil }); !errors.Is(err, buyer.ErrUnauthorized) {
		t.Fatalf("%v", err)
	}
}

func TestPrivacyPaths(t *testing.T) {
	for _, p := range []string{privacyPath, consentsPath, privacyExportPath, privacyErasurePath} {
		if !strings.HasPrefix(p, "/v1/buyer/") {
			t.Fatal(p)
		}
	}
}
