// customers.go owns the merchant customer HTTP adapter of contracts/customers-billing-v1.md §5 (FROZEN):
// GET customers (list), GET customers/{id} (detail), POST customers/{id}/consent-withdrawals,
// POST customers/{id}/exports and POST customers/{id}/erasure. Each handler is the Go endpoint; the admin BFF
// mirrors them under /api/admin/.
//
// Non-goals: no privacy or consent rule (internal/customers and migrations/0078 decide every rule, permission
// and idempotency outcome), no billing, no logging of bodies, customer names, phones or bearer tokens.
//
// Each route runs in one commerce_runtime READ COMMITTED transaction (platform.WithScope opened with the
// route's permission: customers:read for reads, customers:privacy for writes) whose nil return is the COMMIT
// acknowledgement every 2xx waits for. Every response is private and non-cacheable. The POST routes need exactly
// one Idempotency-Key. Error codes idempotency_conflict, erasure_blocked, erased and export_too_large live in
// internal/httperror's table (ruling 15; integrator hook).

package httpapi

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"livecommerce/internal/command"
	"livecommerce/internal/customers"
	"livecommerce/internal/platform"
	"livecommerce/internal/reporting"
)

const customerBase = "/v1/admin/stores/{store_id}/customers"

// registerCustomerRoutes mounts the five §5 customer rows; NewHandler calls it unconditionally (integrator).
func registerCustomerRoutes(mux *http.ServeMux, pool *pgxpool.Pool) {
	mux.HandleFunc("GET "+customerBase, customerRoute(http.MethodGet, true, func(w http.ResponseWriter, r *http.Request) {
		in, err := parseCustomersQuery(r.URL)
		if err != nil {
			respondError(w, http.StatusUnprocessableEntity, "invalid_request")
			return
		}
		if result, ok := customersScope(w, r, pool, "customers:read", func(ctx context.Context, tx pgx.Tx, s platform.Scope, r *http.Request) (any, error) {
			return customers.List(ctx, tx, s, bearerToken(r), in)
		}); ok {
			respond(w, http.StatusOK, result)
		}
	}))
	mux.HandleFunc("GET "+customerBase+"/{customer_id}", customerRoute(http.MethodGet, false, func(w http.ResponseWriter, r *http.Request) {
		if result, ok := customersScope(w, r, pool, "customers:read", func(ctx context.Context, tx pgx.Tx, s platform.Scope, r *http.Request) (any, error) {
			return customers.Get(ctx, tx, s, bearerToken(r), r.PathValue("customer_id"))
		}); ok {
			respond(w, http.StatusOK, result)
		}
	}))
	mux.HandleFunc("POST "+customerBase+"/{customer_id}/consent-withdrawals", customerRoute(http.MethodPost, false, func(w http.ResponseWriter, r *http.Request) {
		in, ok := claimsBody[customers.WithdrawInput](w, r, []string{"purpose", "channel"}, nil)
		if !ok {
			return
		}
		key := r.Header.Get("Idempotency-Key")
		if result, ok := customersScope(w, r, pool, "customers:privacy", func(ctx context.Context, tx pgx.Tx, s platform.Scope, r *http.Request) (any, error) {
			return customers.WithdrawConsent(ctx, tx, s, bearerToken(r), key, r.PathValue("customer_id"), in)
		}); ok {
			respond(w, http.StatusCreated, result) // COMMIT acknowledged
		}
	}))
	mux.HandleFunc("POST "+customerBase+"/{customer_id}/exports", customerRoute(http.MethodPost, false, func(w http.ResponseWriter, r *http.Request) {
		if hasBody(r) {
			respondError(w, http.StatusUnprocessableEntity, "invalid_request")
			return
		}
		key := r.Header.Get("Idempotency-Key")
		result, ok := customersScope(w, r, pool, "customers:privacy", func(ctx context.Context, tx pgx.Tx, s platform.Scope, r *http.Request) (any, error) {
			return customers.Export(ctx, tx, s, bearerToken(r), key, r.PathValue("customer_id"))
		})
		if !ok {
			return
		}
		writeAttachment(w, "application/json", `attachment; filename="customer-`+r.PathValue("customer_id")+`.json"`, result.([]byte))
	}))
	mux.HandleFunc("POST "+customerBase+"/{customer_id}/erasure", customerRoute(http.MethodPost, false, func(w http.ResponseWriter, r *http.Request) {
		in, ok := claimsBody[erasureBody](w, r, []string{"confirm"}, nil)
		if !ok {
			return
		}
		if in.Confirm != "ERASE" { // typed confirmation, exactly (contract §5)
			respondError(w, http.StatusUnprocessableEntity, "invalid_request")
			return
		}
		key := r.Header.Get("Idempotency-Key")
		if result, ok := customersScope(w, r, pool, "customers:privacy", func(ctx context.Context, tx pgx.Tx, s platform.Scope, r *http.Request) (any, error) {
			return customers.Erase(ctx, tx, s, bearerToken(r), key, r.PathValue("customer_id"))
		}); ok {
			respond(w, http.StatusOK, result)
		}
	}))
}

// erasureBody is the only erasure body: {"confirm":"ERASE"}.
type erasureBody struct {
	Confirm string `json:"confirm"`
}

// customerRoute is the transport gate before any database work: exact method (HEAD is rejected), canonical
// customer id, no query except on the list route, no body and no Idempotency-Key on GET, exactly one valid
// Idempotency-Key on POST. Every response is private and non-cacheable.
func customerRoute(method string, allowQuery bool, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store, private")
		if r.Method != method {
			respondError(w, http.StatusMethodNotAllowed, "method_not_allowed")
			return
		}
		if !allowQuery && (r.URL.RawQuery != "" || r.URL.ForceQuery) {
			respondError(w, http.StatusUnprocessableEntity, "invalid_request")
			return
		}
		if id := r.PathValue("customer_id"); id != "" && !command.ValidID(id) {
			respondError(w, http.StatusUnprocessableEntity, "invalid_request")
			return
		}
		if method == http.MethodGet {
			if _, present := r.Header[http.CanonicalHeaderKey("Idempotency-Key")]; present || hasBody(r) {
				respondError(w, http.StatusUnprocessableEntity, "invalid_request")
				return
			}
		} else if keys := r.Header.Values("Idempotency-Key"); len(keys) != 1 || !claimsKey.MatchString(keys[0]) {
			respondError(w, http.StatusUnprocessableEntity, "invalid_request")
			return
		}
		next(w, r)
	}
}

func hasBody(r *http.Request) bool {
	return r.ContentLength != 0 || len(r.TransferEncoding) != 0 || (r.Body != nil && r.Body != http.NoBody)
}

// customersScope runs fn in one platform.WithScope transaction opened with permission; on failure it has
// already written the classified error. ok=true means COMMIT was acknowledged (result is safe to send).
func customersScope(w http.ResponseWriter, r *http.Request, pool *pgxpool.Pool, permission string, fn action) (any, bool) {
	if !canonicalBearer(r) {
		respondError(w, http.StatusUnauthorized, "unauthorized")
		return nil, false
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	var result any
	err := platform.WithScope(ctx, pool, bearerToken(r), r.PathValue("store_id"), permission, func(tx pgx.Tx, s platform.Scope) error {
		var inner error
		result, inner = fn(ctx, tx, s, r)
		return inner
	})
	if err != nil {
		status, code := customersClassify(err)
		respondError(w, status, code)
		return nil, false
	}
	return result, true
}

// customersClassify maps the §5/§6 privacy errors (D3), then the claims table (deadlock -> 503 retry_later,
// unclassified -> 503 unavailable). No driver message or detail is ever returned. Shared with finance.go.
func customersClassify(err error) (int, string) {
	switch {
	case errors.Is(err, customers.ErrIdempotencyConflict):
		return http.StatusConflict, "idempotency_conflict"
	case errors.Is(err, customers.ErrErasureBlocked):
		return http.StatusConflict, "erasure_blocked"
	case errors.Is(err, customers.ErrExportTooLarge):
		return http.StatusConflict, "export_too_large"
	case errors.Is(err, customers.ErrErased):
		return http.StatusGone, "erased"
	case errors.Is(err, customers.ErrUnavailable), errors.Is(err, reporting.ErrUnavailable):
		return http.StatusServiceUnavailable, "unavailable"
	}
	return claimsClassify(err)
}

// writeAttachment sends a generated document: attachment, non-cacheable, never stored or logged (D9).
func writeAttachment(w http.ResponseWriter, contentType, disposition string, body []byte) {
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Content-Disposition", disposition)
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body)
}

// parseCustomersQuery reads limit (1..100, canonical digits), after (opaque cursor, <= 1024) and q (1..40 chars
// after trimming, D6) — each at most once and non-empty; any other key is 422.
func parseCustomersQuery(u *url.URL) (customers.ListRequest, error) {
	var in customers.ListRequest
	if u.ForceQuery || len(u.RawQuery) > 4096 {
		return in, command.ErrInvalid
	}
	if u.RawQuery == "" {
		return in, nil
	}
	for _, field := range strings.Split(u.RawQuery, "&") {
		if field == "" || !strings.Contains(field, "=") {
			return in, command.ErrInvalid
		}
	}
	values, err := url.ParseQuery(u.RawQuery)
	if err != nil {
		return in, command.ErrInvalid
	}
	for name, value := range values {
		if len(value) != 1 || value[0] == "" {
			return in, command.ErrInvalid
		}
		switch name {
		case "limit":
			if strings.Trim(value[0], "0123456789") != "" {
				return in, command.ErrInvalid
			}
			in.Page.Limit, err = strconv.Atoi(value[0])
			if err != nil || in.Page.Limit < 1 || in.Page.Limit > 100 || strconv.Itoa(in.Page.Limit) != value[0] {
				return in, command.ErrInvalid
			}
		case "after":
			if len(value[0]) > 1024 {
				return in, command.ErrInvalid
			}
			in.Page.Cursor = value[0]
		case "q":
			if _, err := customers.NormalizeQuery(value[0]); err != nil {
				return in, err
			}
			in.Q = value[0]
		default:
			return in, command.ErrInvalid
		}
	}
	return in, nil
}
