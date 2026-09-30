// shipments.go owns the merchant manual-fulfilment HTTP adapter of contracts/manual-fulfilment-v1.md
// §5.1 (FROZEN): PUT shipment, GET shipment history, GET orders/unshipped.csv, plus the read-only
// permission probe GET order-actions (ruling E2).
//
// Non-goals: no fulfilment rule (internal/merchantorders and migrations/0063 decide every invariant,
// CAS and permission), no buyer route (the buyer sees shipments through GET /v1/buyer/orders/{id}),
// no bulk tracking import (M-7), no logging of bodies, tracking numbers, recipients or bearer tokens.
//
// Each route runs in one commerce_runtime READ COMMITTED transaction (platform.WithScope) whose nil return is
// the COMMIT acknowledgement every 2xx waits for. Route → Go endpoint: this file's handlers ARE the Go
// endpoints; the admin BFF mirrors them under /api/admin/.
//
// The §5.1 error codes (version_changed, not_shippable, ...) live in internal/httperror's table (ruling 15).

package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"livecommerce/internal/command"
	"livecommerce/internal/merchantorders"
	"livecommerce/internal/platform"
)

// shipmentBodyFields are the eight keys the PUT body must carry (nullable ones as explicit null).
var shipmentBodyFields = []string{"expected_version", "status", "carrier_code", "carrier_name", "tracking_number", "tracking_url", "note", "void_reason"}

// registerShipmentRoutes mounts the four routes; NewHandler calls it unconditionally (integrator).
// PUT needs an Idempotency-Key; HEAD is rejected everywhere (mux 405 on the PUT-only path, the
// method check below on the GET routes).
func registerShipmentRoutes(mux *http.ServeMux, pool *pgxpool.Pool) {
	const base = "/v1/admin/stores/{store_id}"
	// PUT: record, correct or void (status + expected_version select the transition).
	mux.HandleFunc("PUT "+base+"/orders/{order_id}/shipment", shipmentRoute(http.MethodPut, func(w http.ResponseWriter, r *http.Request) {
		keys := r.Header.Values("Idempotency-Key")
		if len(keys) != 1 || !claimsKey.MatchString(keys[0]) {
			respondError(w, http.StatusUnprocessableEntity, "invalid_request")
			return
		}
		in, raw, ok := studioDecodeRaw[merchantorders.ShipmentInput](w, r, shipmentBodyFields)
		if !ok {
			return
		}
		var present map[string]json.RawMessage
		if json.Unmarshal(raw, &present) != nil || len(present) != len(shipmentBodyFields) ||
			string(present["expected_version"]) == "null" || string(present["status"]) == "null" {
			// Explicit null is the only way to omit a nullable field; a missing key or a null
			// expected_version/status must never decode to a silent zero value.
			respondError(w, http.StatusUnprocessableEntity, "invalid_request")
			return
		}
		result, ok := shipmentScope(w, r, pool, "fulfillment:write", func(ctx context.Context, tx pgx.Tx, s platform.Scope, r *http.Request) (any, error) {
			return merchantorders.RecordShipment(ctx, tx, s, bearerToken(r), keys[0], r.PathValue("order_id"), in)
		})
		if ok {
			respond(w, http.StatusOK, result)
		}
	}))
	// GET history: every version, merchant-only fields included (orders:read).
	mux.HandleFunc("GET "+base+"/orders/{order_id}/shipment/history", shipmentRoute(http.MethodGet, func(w http.ResponseWriter, r *http.Request) {
		result, ok := shipmentScope(w, r, pool, "orders:read", func(ctx context.Context, tx pgx.Tx, s platform.Scope, r *http.Request) (any, error) {
			items, err := merchantorders.ShipmentHistory(ctx, tx, s, bearerToken(r), r.PathValue("order_id"))
			return struct {
				Items []merchantorders.ShipmentVersion `json:"items"`
			}{items}, err
		})
		if ok {
			respond(w, http.StatusOK, result)
		}
	}))
	// GET unshipped.csv: bulk recipient PII, generated per request, never stored (MD9).
	mux.HandleFunc("GET "+base+"/orders/unshipped.csv", shipmentRoute(http.MethodGet, func(w http.ResponseWriter, r *http.Request) {
		result, ok := shipmentScope(w, r, pool, "orders:export", func(ctx context.Context, tx pgx.Tx, s platform.Scope, r *http.Request) (any, error) {
			return merchantorders.ExportUnshipped(ctx, tx, s, bearerToken(r))
		})
		if !ok {
			return
		}
		writeExport(w, r.PathValue("store_id"), result.(merchantorders.Export), time.Now()) // only after COMMIT was acknowledged
	}))
	// GET order-actions: three booleans for the UI; the server stays the authority on every write.
	mux.HandleFunc("GET "+base+"/order-actions", shipmentRoute(http.MethodGet, func(w http.ResponseWriter, r *http.Request) {
		result, ok := shipmentScope(w, r, pool, "orders:read", func(ctx context.Context, tx pgx.Tx, s platform.Scope, r *http.Request) (any, error) {
			return merchantorders.Actions(ctx, tx, s, bearerToken(r))
		})
		if ok {
			respond(w, http.StatusOK, result)
		}
	}))
}

// writeExport sends the CSV of contract §5.1: attachment, non-cacheable, truncation flag. The body
// exists only in this response; nothing is stored, logged or cached.
func writeExport(w http.ResponseWriter, store string, export merchantorders.Export, now time.Time) {
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="unshipped-`+store[:8]+`-`+now.UTC().Format("200601021504")+`.csv"`)
	w.Header().Set("Cache-Control", "no-store, private")
	w.Header().Set("X-Export-Truncated", strconv.FormatBool(export.Truncated))
	w.Header().Set("Content-Length", strconv.Itoa(len(export.Body)))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(export.Body)
}

// shipmentRoute is the transport gate before any database work: exact method, no query, canonical
// order id, and for GET no body and no Idempotency-Key (even an empty one). Every response is
// private and non-cacheable.
func shipmentRoute(method string, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store, private")
		if r.Method != method {
			respondError(w, http.StatusMethodNotAllowed, "method_not_allowed")
			return
		}
		if r.URL.RawQuery != "" || r.URL.ForceQuery {
			respondError(w, http.StatusUnprocessableEntity, "invalid_request")
			return
		}
		if id := r.PathValue("order_id"); id != "" && !command.ValidID(id) {
			respondError(w, http.StatusUnprocessableEntity, "invalid_request")
			return
		}
		if method == http.MethodGet {
			if _, present := r.Header[http.CanonicalHeaderKey("Idempotency-Key")]; present ||
				r.ContentLength != 0 || len(r.TransferEncoding) != 0 || (r.Body != nil && r.Body != http.NoBody) {
				respondError(w, http.StatusUnprocessableEntity, "invalid_request")
				return
			}
		}
		next(w, r)
	}
}

// shipmentScope runs fn in one platform.WithScope transaction opened with permission and, on failure,
// has already written the classified error. ok=true means COMMIT was acknowledged (result is safe to send).
func shipmentScope(w http.ResponseWriter, r *http.Request, pool *pgxpool.Pool, permission string, fn action) (any, bool) {
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
		status, code := shipmentClassify(err)
		respondError(w, status, code)
		return nil, false
	}
	return result, true
}

// shipmentClassify maps the frozen §5.1 errors, then the claims table (deadlock → 503 retry_later,
// unclassified → 503 unavailable). No driver message or detail is ever returned.
func shipmentClassify(err error) (int, string) {
	switch {
	case errors.Is(err, merchantorders.ErrVersionChanged):
		return http.StatusConflict, "version_changed"
	case errors.Is(err, merchantorders.ErrNotShippable):
		return http.StatusUnprocessableEntity, "not_shippable"
	case errors.Is(err, merchantorders.ErrInvalidCarrier):
		return http.StatusUnprocessableEntity, "invalid_carrier"
	case errors.Is(err, merchantorders.ErrInvalidTracking):
		return http.StatusUnprocessableEntity, "invalid_tracking"
	case errors.Is(err, merchantorders.ErrInvalidURL):
		return http.StatusUnprocessableEntity, "invalid_url"
	case errors.Is(err, merchantorders.ErrVoidRequiresShipped):
		return http.StatusUnprocessableEntity, "void_requires_shipped"
	case errors.Is(err, merchantorders.ErrInvalidVoid):
		return http.StatusUnprocessableEntity, "invalid_void"
	}
	return claimsClassify(err)
}
