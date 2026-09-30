// finance_test.go covers the finance routes' transport rules before any database work and the range grammar.
// Day boundaries, refund rules and the export audit row are CB03/CB09 (real PG), not here.

package httpapi

import (
	"net/http"
	"net/url"
	"testing"
)

func TestFinanceTransportRulesBeforeDatabase(t *testing.T) {
	h := customerHandler()
	base := "/v1/admin/stores/" + custStore + "/finance/summary"
	for _, tc := range []struct {
		name, method, path string
		edit               func(*http.Request)
		status             int
		code               string
	}{
		{"summary", "GET", base + "?from=2026-09-01&to=2026-09-30", nil, 401, "unauthorized"},
		{"csv", "GET", base + ".csv?from=2026-09-01&to=2026-09-30", nil, 401, "unauthorized"},
		{"same day", "GET", base + "?from=2026-09-01&to=2026-09-01", nil, 401, "unauthorized"},
		{"max range", "GET", base + "?from=2026-01-01&to=2026-04-02", nil, 401, "unauthorized"},
		{"range too long", "GET", base + "?from=2026-01-01&to=2026-04-03", nil, 422, "invalid_request"},
		{"reversed", "GET", base + "?from=2026-09-02&to=2026-09-01", nil, 422, "invalid_request"},
		{"no query", "GET", base, nil, 422, "invalid_request"},
		{"missing to", "GET", base + "?from=2026-09-01", nil, 422, "invalid_request"},
		{"bad date", "GET", base + "?from=2026-02-30&to=2026-03-01", nil, 422, "invalid_request"},
		{"timestamp", "GET", base + "?from=2026-09-01T00:00:00Z&to=2026-09-02", nil, 422, "invalid_request"},
		{"extra key", "GET", base + "?from=2026-09-01&to=2026-09-02&tz=UTC", nil, 422, "invalid_request"},
		{"duplicate from", "GET", base + "?from=2026-09-01&from=2026-09-02&to=2026-09-03", nil, 422, "invalid_request"},
		{"csv bad", "GET", base + ".csv?from=x&to=y", nil, 422, "invalid_request"},
		{"post", "POST", base + "?from=2026-09-01&to=2026-09-02", nil, 405, ""},
		{"head", "HEAD", base + "?from=2026-09-01&to=2026-09-02", nil, 405, ""},
		{"csv head", "HEAD", base + ".csv?from=2026-09-01&to=2026-09-02", nil, 405, ""},
		{"with key", "GET", base + "?from=2026-09-01&to=2026-09-02", func(r *http.Request) { r.Header.Set("Idempotency-Key", "cust-key-0001") }, 422, "invalid_request"},
		{"no bearer", "GET", base + "?from=2026-09-01&to=2026-09-02", func(r *http.Request) { r.Header.Del("Authorization") }, 401, "unauthorized"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := customerCall(h, tc.method, tc.path, "", tc.edit)
			if w.Code != tc.status {
				t.Fatalf("status=%d want=%d body=%s", w.Code, tc.status, w.Body.String())
			}
		})
	}
}

func TestParseFinanceQuery(t *testing.T) {
	u, _ := url.Parse("http://x/?from=2026-09-01&to=2026-09-30")
	if from, to, err := parseFinanceQuery(u); err != nil || from != "2026-09-01" || to != "2026-09-30" {
		t.Fatalf("%s %s %v", from, to, err)
	}
}
