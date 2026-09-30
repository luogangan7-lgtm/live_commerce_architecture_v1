// cvs.go owns the merchant ECPay CVS routes of contracts/taiwan-cvs-logistics-v1.md §8 (FROZEN): the ECPay connection card, chain and
// pay-at-pickup settings, one label request per order, shipment read, print form, abandon, collection record and pay-at-pickup
// cancel/restock. Each route is a thin transport gate (exact method, no query, canonical ids, strict bodies with every key present,
// Idempotency-Key exactly where §8 says) around one internal/fulfillment.CVS method; the SQL definers decide every rule and every
// permission. The admin BFF mirrors these under /api/admin/. Route -> Go endpoint: these handlers ARE the Go endpoints.
//
// Non-goals: no business rule, no provider call (fulfillment.CVS makes the only ones), no secret, recipient field, trade number or
// driver message in a response or log (a coded refusal returns only its contract code). The error codes of §8 live in
// internal/httperror's table (ruling 15: an unknown code would be rewritten to "internal").
// Mounted by NewHandler through Options.CVS (integrator hook): a nil CVS leaves the whole surface unmounted.

package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"livecommerce/internal/command"
	"livecommerce/internal/fulfillment"
)

// Body field sets of §8 / unit default C11: every key must be present (only the settings cap may be an explicit null).
var (
	cvsConnectFields    = []string{"expected_version", "environment", "mode", "merchant_id", "hash_key", "hash_iv", "sender_name", "sender_cell_phone"}
	cvsEnableFields     = []string{"expected_version", "enabled"}
	cvsSettingsFields   = []string{"expected_version", "enabled_chains", "pay_at_pickup_enabled", "pay_at_pickup_max_twd", "pay_at_pickup_max_open"}
	cvsRequestFields    = []string{"expected_version"}
	cvsPrintFields      = []string{"thermal"}
	cvsAbandonFields    = []string{"expected_version", "i_checked_ecpay_backend"}
	cvsCollectionFields = []string{"expected_state", "state"}
	cvsReleaseFields    = []string{"action", "expected_state"}
)

// registerCVSRoutes mounts the §8 table; the integrator calls it only when a *fulfillment.CVS exists. The methods of cvs use the
// CVS service's own main pool (the same commerce_runtime pool the other admin routes run on), so pool is accepted for symmetry
// with registerShipmentRoutes and is not used.
func registerCVSRoutes(mux *http.ServeMux, pool *pgxpool.Pool, cvs *fulfillment.CVS) {
	_ = pool
	if cvs == nil {
		return
	}
	const base = "/v1/admin/stores/{store_id}"
	const order = base + "/orders/{order_id}"

	mux.HandleFunc("GET "+base+"/logistics/ecpay", cvsRoute(http.MethodGet, false, func(w http.ResponseWriter, r *http.Request) {
		cvsServe(w, r, 5*time.Second, http.StatusOK, func(ctx context.Context) (any, error) {
			return cvs.Profile(ctx, bearerToken(r), r.PathValue("store_id"))
		})
	}))
	mux.HandleFunc("PUT "+base+"/logistics/ecpay", cvsRoute(http.MethodPut, true, func(w http.ResponseWriter, r *http.Request) {
		in, ok := cvsStrictBody[fulfillment.ConnectInput](w, r, cvsConnectFields, nil)
		if !ok {
			return
		}
		cvsServe(w, r, 40*time.Second, http.StatusOK, func(ctx context.Context) (any, error) {
			return cvs.Connect(ctx, bearerToken(r), r.PathValue("store_id"), r.Header.Get("Idempotency-Key"), in)
		})
	}))
	mux.HandleFunc("POST "+base+"/logistics/ecpay/enabled", cvsRoute(http.MethodPost, true, func(w http.ResponseWriter, r *http.Request) {
		in, ok := cvsStrictBody[fulfillment.EnableInput](w, r, cvsEnableFields, nil)
		if !ok {
			return
		}
		cvsServe(w, r, 5*time.Second, http.StatusOK, func(ctx context.Context) (any, error) {
			return cvs.Enable(ctx, bearerToken(r), r.PathValue("store_id"), r.Header.Get("Idempotency-Key"), in)
		})
	}))
	mux.HandleFunc("GET "+base+"/logistics/cvs-settings", cvsRoute(http.MethodGet, false, func(w http.ResponseWriter, r *http.Request) {
		cvsServe(w, r, 5*time.Second, http.StatusOK, func(ctx context.Context) (any, error) {
			return cvs.Settings(ctx, bearerToken(r), r.PathValue("store_id"))
		})
	}))
	mux.HandleFunc("PUT "+base+"/logistics/cvs-settings", cvsRoute(http.MethodPut, true, func(w http.ResponseWriter, r *http.Request) {
		in, ok := cvsStrictBody[fulfillment.SettingsInput](w, r, cvsSettingsFields, []string{"pay_at_pickup_max_twd"})
		if !ok {
			return
		}
		cvsServe(w, r, 5*time.Second, http.StatusOK, func(ctx context.Context) (any, error) {
			return cvs.SetSettings(ctx, bearerToken(r), r.PathValue("store_id"), r.Header.Get("Idempotency-Key"), in)
		})
	}))
	mux.HandleFunc("POST "+order+"/cvs-shipment", cvsRoute(http.MethodPost, true, func(w http.ResponseWriter, r *http.Request) {
		in, ok := cvsStrictBody[fulfillment.RequestInput](w, r, cvsRequestFields, nil)
		if !ok {
			return
		}
		// 202: the operation is planned and the shipment REQUESTED; ECPay is called later by the worker, never here.
		cvsServe(w, r, 8*time.Second, http.StatusAccepted, func(ctx context.Context) (any, error) {
			return cvs.Request(ctx, bearerToken(r), r.PathValue("store_id"), r.Header.Get("Idempotency-Key"), r.PathValue("order_id"), in)
		})
	}))
	mux.HandleFunc("GET "+order+"/cvs-shipment", cvsRoute(http.MethodGet, false, func(w http.ResponseWriter, r *http.Request) {
		cvsServe(w, r, 5*time.Second, http.StatusOK, func(ctx context.Context) (any, error) {
			return cvs.Shipment(ctx, bearerToken(r), r.PathValue("store_id"), r.PathValue("order_id"))
		})
	}))
	mux.HandleFunc("POST "+order+"/cvs-shipment/print-form", cvsRoute(http.MethodPost, false, func(w http.ResponseWriter, r *http.Request) {
		in, ok := cvsStrictBody[struct {
			Thermal bool `json:"thermal"`
		}](w, r, cvsPrintFields, nil)
		if !ok {
			return
		}
		cvsServe(w, r, 5*time.Second, http.StatusOK, func(ctx context.Context) (any, error) {
			return cvs.PrintForm(ctx, bearerToken(r), r.PathValue("store_id"), r.PathValue("order_id"), in.Thermal)
		})
	}))
	mux.HandleFunc("POST "+order+"/cvs-shipment/abandon", cvsRoute(http.MethodPost, true, func(w http.ResponseWriter, r *http.Request) {
		in, ok := cvsStrictBody[fulfillment.AbandonInput](w, r, cvsAbandonFields, nil)
		if !ok {
			return
		}
		cvsServe(w, r, 40*time.Second, http.StatusOK, func(ctx context.Context) (any, error) {
			return cvs.Abandon(ctx, bearerToken(r), r.PathValue("store_id"), r.Header.Get("Idempotency-Key"), r.PathValue("order_id"), in)
		})
	}))
	mux.HandleFunc("POST "+order+"/collection", cvsRoute(http.MethodPost, true, func(w http.ResponseWriter, r *http.Request) {
		in, ok := cvsStrictBody[fulfillment.CollectionInput](w, r, cvsCollectionFields, nil)
		if !ok {
			return
		}
		cvsServe(w, r, 5*time.Second, http.StatusOK, func(ctx context.Context) (any, error) {
			return cvs.RecordCollection(ctx, bearerToken(r), r.PathValue("store_id"), r.Header.Get("Idempotency-Key"), r.PathValue("order_id"), in)
		})
	}))
	mux.HandleFunc("POST "+order+"/pay-at-pickup-release", cvsRoute(http.MethodPost, true, func(w http.ResponseWriter, r *http.Request) {
		in, ok := cvsStrictBody[fulfillment.ReleaseInput](w, r, cvsReleaseFields, nil)
		if !ok {
			return
		}
		cvsServe(w, r, 5*time.Second, http.StatusOK, func(ctx context.Context) (any, error) {
			return cvs.Release(ctx, bearerToken(r), r.PathValue("store_id"), r.Header.Get("Idempotency-Key"), r.PathValue("order_id"), in)
		})
	}))
}

// cvsRoute is the transport gate before any work: exact method, no query string, canonical store/order ids, the Idempotency-Key
// exactly once (receipt grammar) on keyed routes and absent elsewhere, and no body or key on GET. Every response is private no-store.
func cvsRoute(method string, keyed bool, next http.HandlerFunc) http.HandlerFunc {
	return studioRoute(method, false, func(w http.ResponseWriter, r *http.Request) {
		keys := r.Header.Values("Idempotency-Key")
		if (keyed && (len(keys) != 1 || !claimsKey.MatchString(keys[0]))) || (!keyed && len(keys) != 0) {
			respondError(w, http.StatusUnprocessableEntity, "invalid_request")
			return
		}
		for _, name := range []string{"store_id", "order_id"} {
			if value := r.PathValue(name); value != "" && !command.ValidID(value) {
				respondError(w, http.StatusUnprocessableEntity, "invalid_request")
				return
			}
		}
		if !canonicalBearer(r) {
			respondError(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		next(w, r)
	})
}

// cvsStrictBody decodes a body that carries exactly the named keys: JSON media type, <= 64 KiB, no duplicate or unknown keys, no
// trailing data, every key present, and no null except the keys in nullable (which must still be present). It has written the error
// response whenever it returns false.
func cvsStrictBody[T any](w http.ResponseWriter, r *http.Request, fields, nullable []string) (T, bool) {
	in, raw, ok := studioDecodeRaw[T](w, r, fields)
	if !ok {
		return in, false
	}
	var present map[string]json.RawMessage
	if json.Unmarshal(raw, &present) != nil || len(present) != len(fields) {
		respondError(w, http.StatusUnprocessableEntity, "invalid_request")
		return in, false
	}
	allowedNull := map[string]bool{}
	for _, name := range nullable {
		allowedNull[name] = true
	}
	for _, name := range fields {
		value, found := present[name]
		if !found || (bytes.Equal(bytes.TrimSpace(value), []byte("null")) && !allowedNull[name]) {
			respondError(w, http.StatusUnprocessableEntity, "invalid_request")
			return in, false
		}
	}
	return in, true
}

// cvsServe runs fn under a bounded context, writes the coded refusal or the result, and never echoes a driver message. success is the
// HTTP status of a committed result (a nil return of every fulfillment.CVS method is the COMMIT acknowledgement).
func cvsServe(w http.ResponseWriter, r *http.Request, timeout time.Duration, success int, fn func(context.Context) (any, error)) {
	ctx, cancel := context.WithTimeout(r.Context(), timeout)
	defer cancel()
	out, err := fn(ctx)
	if err != nil {
		status, code, retry := cvsClassify(err)
		if retry > 0 {
			w.Header().Set("Retry-After", strconv.Itoa(retry))
		}
		respondError(w, status, code)
		return
	}
	respond(w, success, out)
}

// cvsClassify maps a fulfillment.CVS error: the coded refusal first (unit default C7), ECPay off / projection drift as a retryable 503,
// then claimsClassify (shared table: authority, not found, conflict, deadlock and unknown -> 503). The third result is Retry-After.
func cvsClassify(err error) (int, string, int) {
	var refusal *fulfillment.CVSError
	switch {
	case errors.As(err, &refusal):
		return refusal.Status, refusal.Code, refusal.RetryAfter
	case errors.Is(err, fulfillment.ErrECPayDisabled), errors.Is(err, fulfillment.ErrCVSProjection):
		return http.StatusServiceUnavailable, "unavailable", 0
	}
	status, code := claimsClassify(err)
	return status, code, 0
}
