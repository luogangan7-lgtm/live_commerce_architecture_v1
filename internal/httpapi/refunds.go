// refunds.go owns the merchant refund routes of stripe-refund-v1 §7.1 under
// /v1/admin/stores/{store_id}/orders/{order_id}/refunds. It decides no refund rule (SQL and
// internal/merchantorders do), never calls Stripe, and never returns a driver message.
// Each route runs in one commerce_runtime READ COMMITTED transaction (platform.WithScope, opened with the
// route's permission) whose nil return is the COMMIT acknowledgement the 201 waits for, and keeps the Studio
// private no-store boundary with strict JSON and query rejection.

package httpapi

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"

	"livecommerce/internal/command"
	"livecommerce/internal/merchantorders"
	"livecommerce/internal/platform"
)

// errRefundNotScheduled aborts the refresh transaction so a River job inserted for a signal the database
// refused (throttled, terminal refund) never commits as an orphan.
var errRefundNotScheduled = errors.New("refund refresh not scheduled")

// registerRefundRoutes mounts the three §7.1 routes for a SANDBOX deployment; a nil client leaves the
// surface unmounted. It is registerRefundRoutesIn(…, "SANDBOX") so existing callers keep their behavior.
func registerRefundRoutes(mux *http.ServeMux, pool *pgxpool.Pool, jobs *river.Client[pgx.Tx]) {
	registerRefundRoutesIn(mux, pool, jobs, "SANDBOX")
}

// registerRefundRoutesIn mounts the routes for the deployment environment (payments.ProfileEnvironment of
// COMMERCE_PAYMENT_PROFILE, chosen by cmd/api). stripe-live-enable-v1 §5.2: the refund POST refuses any
// attempt of another environment with not_refundable (merchantorders.RequestRefundIn); the refresh route applies the
// same guard (merchantorders.RefreshRefundIn). An environment
// outside {SANDBOX, LIVE} mounts nothing, exactly like a nil client, so a misconfigured deployment cannot
// accept refunds for an unknown environment.
func registerRefundRoutesIn(mux *http.ServeMux, pool *pgxpool.Pool, jobs *river.Client[pgx.Tx], environment string) {
	if jobs == nil || (environment != "SANDBOX" && environment != "LIVE") {
		return
	}
	const base = "/v1/admin/stores/{store_id}/orders/{order_id}/refunds"
	mux.HandleFunc("POST "+base, refundRoute(http.MethodPost, true, func(w http.ResponseWriter, r *http.Request) {
		in, ok := claimsBody[merchantorders.RefundRequest](w, r,
			[]string{"amount_minor", "reason", "expected_refundable_minor"}, nil)
		if !ok {
			return
		}
		key := r.Header.Get("Idempotency-Key")
		var out merchantorders.RefundResult
		if !refundScope(w, r, pool, "payments:refund", func(ctx context.Context, tx pgx.Tx, s platform.Scope) error {
			var err error
			out, err = merchantorders.RequestRefundIn(ctx, tx, jobs, s, environment, bearerToken(r), key, r.PathValue("order_id"), in)
			return err
		}) {
			return
		}
		// platform.WithScope returned nil: COMMIT is acknowledged, so 201 is honest.
		respond(w, http.StatusCreated, out)
	}))
	mux.HandleFunc("GET "+base, refundRoute(http.MethodGet, false, func(w http.ResponseWriter, r *http.Request) {
		var out merchantorders.RefundList
		if !refundScope(w, r, pool, "orders:read", func(ctx context.Context, tx pgx.Tx, s platform.Scope) error {
			var err error
			out, err = merchantorders.ListRefunds(ctx, tx, s, bearerToken(r), r.PathValue("order_id"))
			return err
		}) {
			return
		}
		respond(w, http.StatusOK, out)
	}))
	mux.HandleFunc("POST "+base+"/{refund_id}/refresh", refundRoute(http.MethodPost, false, func(w http.ResponseWriter, r *http.Request) {
		if r.ContentLength != 0 || len(r.TransferEncoding) != 0 || (r.Body != nil && r.Body != http.NoBody) {
			respondError(w, http.StatusUnprocessableEntity, "invalid_request")
			return
		}
		var out merchantorders.RefundSignal
		if !refundScope(w, r, pool, "payments:refund", func(ctx context.Context, tx pgx.Tx, s platform.Scope) error {
			var err error
			out, err = merchantorders.RefreshRefundIn(ctx, tx, jobs, s, environment, bearerToken(r), r.PathValue("order_id"), r.PathValue("refund_id"))
			if err == nil && !out.Scheduled {
				return errRefundNotScheduled // roll back: the inserted job has no signal row
			}
			return err
		}) {
			return
		}
		respond(w, http.StatusOK, out)
	}))
	// Methodless fallbacks keep 405 inside the same private response boundary.
	mux.HandleFunc(base, studioRoute("", false, nil))
	mux.HandleFunc(base+"/{refund_id}/refresh", studioRoute("", false, nil))
}

// refundRoute is studioRoute plus the refund transport rules that need no database: the Idempotency-Key
// header is required exactly once (receipt grammar) on the creating POST and forbidden everywhere else,
// and every path id is a canonical UUID (422 before any transaction).
func refundRoute(method string, keyed bool, next http.HandlerFunc) http.HandlerFunc {
	return studioRoute(method, false, func(w http.ResponseWriter, r *http.Request) {
		keys := r.Header.Values("Idempotency-Key")
		if (keyed && (len(keys) != 1 || !claimsKey.MatchString(keys[0]))) || (!keyed && len(keys) != 0) {
			respondError(w, http.StatusUnprocessableEntity, "invalid_request")
			return
		}
		for _, name := range []string{"order_id", "refund_id"} {
			if value := r.PathValue(name); value != "" && !command.ValidID(value) {
				respondError(w, http.StatusUnprocessableEntity, "invalid_request")
				return
			}
		}
		next(w, r)
	})
}

// refundScope runs fn in a platform.WithScope transaction opened with permission and writes the error
// response itself. errRefundNotScheduled is the one expected abort: it answers 200 {refund_id,scheduled:false}.
// It reports whether fn committed.
func refundScope(w http.ResponseWriter, r *http.Request, pool *pgxpool.Pool, permission string,
	fn func(context.Context, pgx.Tx, platform.Scope) error) bool {
	if !canonicalBearer(r) {
		respondError(w, http.StatusUnauthorized, "unauthorized")
		return false
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	err := platform.WithScope(ctx, pool, bearerToken(r), r.PathValue("store_id"), permission,
		func(tx pgx.Tx, s platform.Scope) error { return fn(ctx, tx, s) })
	if errors.Is(err, errRefundNotScheduled) {
		respond(w, http.StatusOK, merchantorders.RefundSignal{RefundID: r.PathValue("refund_id"), Scheduled: false})
		return false
	}
	if err != nil {
		status, code := refundClassify(err)
		respondError(w, status, code)
		return false
	}
	return true
}

// refundClassify is the §7.1 mapping: the refund sentinels first, then claimsClassify (shared table,
// deadlock and unknown errors become a retryable 503). No driver message is ever returned.
func refundClassify(err error) (int, string) {
	switch {
	case errors.Is(err, merchantorders.ErrRefundableChanged):
		return http.StatusConflict, "refundable_changed"
	case errors.Is(err, merchantorders.ErrExceedsRefundable):
		return http.StatusUnprocessableEntity, "exceeds_refundable"
	case errors.Is(err, merchantorders.ErrAmountStep):
		return http.StatusUnprocessableEntity, "amount_step"
	case errors.Is(err, merchantorders.ErrNotRefundable):
		return http.StatusUnprocessableEntity, "not_refundable"
	case errors.Is(err, merchantorders.ErrRefundBlockedReview):
		return http.StatusUnprocessableEntity, "refund_blocked_review"
	case errors.Is(err, merchantorders.ErrRefundLimit):
		return http.StatusUnprocessableEntity, "refund_limit"
	}
	return claimsClassify(err)
}
