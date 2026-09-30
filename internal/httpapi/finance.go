// finance.go owns the merchant finance HTTP adapter of contracts/customers-billing-v1.md §5 (BD7, FROZEN):
// GET finance/summary?from&to (orders:read) and GET finance/summary.csv?from&to (orders:export, audited).
// Each handler is the Go endpoint; the admin BFF mirrors them under /api/admin/.
//
// Non-goals: no money rule (internal/reporting and identity.read_finance_summary in migrations/0078 decide
// the rows), no currency conversion, no storage of the CSV (generated per request, MD9 pattern), no logging.
//
// Each route runs in one commerce_runtime READ COMMITTED transaction (platform.WithScope opened with the route's
// permission) whose nil return is the COMMIT acknowledgement; from and to are required YYYY-MM-DD dates with
// 0 <= to-from <= 91 (D13); every response is private and non-cacheable.

package httpapi

import (
	"context"
	"net/http"
	"net/url"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"livecommerce/internal/command"
	"livecommerce/internal/platform"
	"livecommerce/internal/reporting"
)

// registerFinanceRoutes mounts the two §5 finance rows; NewHandler calls it unconditionally (integrator).
func registerFinanceRoutes(mux *http.ServeMux, pool *pgxpool.Pool) {
	const base = "/v1/admin/stores/{store_id}/finance"
	mux.HandleFunc("GET "+base+"/summary", customerRoute(http.MethodGet, true, func(w http.ResponseWriter, r *http.Request) {
		from, to, err := parseFinanceQuery(r.URL)
		if err != nil {
			respondError(w, http.StatusUnprocessableEntity, "invalid_request")
			return
		}
		if result, ok := customersScope(w, r, pool, "orders:read", func(ctx context.Context, tx pgx.Tx, s platform.Scope, r *http.Request) (any, error) {
			return reporting.Finance(ctx, tx, s, bearerToken(r), from, to)
		}); ok {
			respond(w, http.StatusOK, result)
		}
	}))
	mux.HandleFunc("GET "+base+"/summary.csv", customerRoute(http.MethodGet, true, func(w http.ResponseWriter, r *http.Request) {
		from, to, err := parseFinanceQuery(r.URL)
		if err != nil {
			respondError(w, http.StatusUnprocessableEntity, "invalid_request")
			return
		}
		result, ok := customersScope(w, r, pool, "orders:export", func(ctx context.Context, tx pgx.Tx, s platform.Scope, r *http.Request) (any, error) {
			return reporting.FinanceCSV(ctx, tx, s, bearerToken(r), from, to)
		})
		if !ok {
			return
		}
		// only after COMMIT was acknowledged (the export audit row is durable)
		writeAttachment(w, "text/csv; charset=utf-8", `attachment; filename="finance-`+from+`-`+to+`.csv"`, result.([]byte))
	}))
}

// parseFinanceQuery requires exactly from and to, once each, as valid dates within the range bound.
func parseFinanceQuery(u *url.URL) (string, string, error) {
	if u.ForceQuery || len(u.RawQuery) > 256 || u.RawQuery == "" {
		return "", "", command.ErrInvalid
	}
	for _, field := range strings.Split(u.RawQuery, "&") {
		if field == "" || !strings.Contains(field, "=") {
			return "", "", command.ErrInvalid
		}
	}
	values, err := url.ParseQuery(u.RawQuery)
	if err != nil || len(values) != 2 || len(values["from"]) != 1 || len(values["to"]) != 1 {
		return "", "", command.ErrInvalid
	}
	from, to := values["from"][0], values["to"][0]
	if _, _, err := reporting.ParseRange(from, to); err != nil {
		return "", "", err
	}
	return from, to, nil
}
