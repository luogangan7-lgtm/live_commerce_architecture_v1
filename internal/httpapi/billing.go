// billing.go owns the merchant platform-billing routes of customers-billing-v1 §5 under
// /v1/admin/stores/{store_id}/billing: GET status, GET standing (banner), POST checkout, POST portal
// (BFF: apps/admin /api/stores/[store]/billing/*; Go: internal/billing.Service). It decides no billing
// rule (SQL 0079 and internal/billing do), never calls Stripe itself, never logs or stores the returned
// Checkout/portal bearer URL and never returns a driver or Stripe message.
//
// Transaction shape: GET standing runs in one scoped transaction (store:read). Status, checkout and portal
// authenticate with an empty billing:manage scoped transaction first, COMMIT it, and only then let the
// service run its own short transactions, because a Stripe call must never wait inside a database
// transaction (contract §5). Routes are strict: no query, no Idempotency-Key (Stripe checkout takes none,
// §6), checkout a strict JSON body, portal an empty body.

package httpapi

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"livecommerce/internal/billing"
	"livecommerce/internal/command"
	"livecommerce/internal/platform"
)

// billingCallBudget bounds one service call: refresh-on-read (a list plus applies) and the plan fetches
// each carry their own Stripe timeouts inside it.
const billingCallBudget = 30 * time.Second

type billingCheckoutInput struct {
	PriceID string `json:"price_id"`
}

type billingURLResponse struct {
	URL string `json:"url"`
}

type billingStandingResponse struct {
	Standing billing.Standing `json:"standing"`
}

// registerBillingRoutes mounts the four routes. A nil svc (LC_BILLING_ENABLED unset) still mounts the
// GETs, which answer from the database, while POSTs answer 503 billing_unavailable after authentication.
func registerBillingRoutes(mux *http.ServeMux, pool *pgxpool.Pool, svc *billing.Service) {
	const base = "/v1/admin/stores/{store_id}/billing"
	// GET billing: standing, subscriptions, usage, plans (billing:manage).
	mux.HandleFunc("GET "+base, billingRoute(http.MethodGet, func(w http.ResponseWriter, r *http.Request) {
		scope, token, ok := billingAuth(w, r, pool)
		if !ok {
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), billingCallBudget)
		defer cancel()
		status, err := svc.Status(ctx, pool, scope, token)
		if err != nil {
			respondBillingError(w, err)
			return
		}
		respond(w, http.StatusOK, status)
	}))
	// GET billing/standing: the banner every member sees (store:read).
	mux.HandleFunc("GET "+base+"/standing", billingRoute(http.MethodGet, scopedAs(pool, "store:read", billingClassify,
		func(ctx context.Context, tx pgx.Tx, s platform.Scope, r *http.Request) (any, error) {
			standing, err := billing.ReadStanding(ctx, tx, s, bearerToken(r))
			return billingStandingResponse{Standing: standing}, err
		})))
	// POST billing/checkout {price_id}: Stripe-hosted Checkout URL (bearer link: no-store, no-referrer).
	mux.HandleFunc("POST "+base+"/checkout", noReferrer(billingRoute(http.MethodPost, func(w http.ResponseWriter, r *http.Request) {
		in, ok := claimsBody[billingCheckoutInput](w, r, []string{"price_id"}, nil)
		if !ok {
			return
		}
		scope, token, ok := billingAuth(w, r, pool)
		if !ok {
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), billingCallBudget)
		defer cancel()
		url, err := svc.StartCheckout(ctx, pool, scope, token, in.PriceID)
		if err != nil {
			respondBillingError(w, err)
			return
		}
		respond(w, http.StatusOK, billingURLResponse{URL: url})
	})))
	// POST billing/portal: Stripe customer-portal URL (5-minute bearer link).
	mux.HandleFunc("POST "+base+"/portal", noReferrer(billingRoute(http.MethodPost, func(w http.ResponseWriter, r *http.Request) {
		if r.ContentLength != 0 || len(r.TransferEncoding) != 0 || (r.Body != nil && r.Body != http.NoBody) {
			respondError(w, http.StatusUnprocessableEntity, "invalid_request")
			return
		}
		scope, token, ok := billingAuth(w, r, pool)
		if !ok {
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), billingCallBudget)
		defer cancel()
		url, err := svc.OpenPortal(ctx, pool, scope, token)
		if err != nil {
			respondBillingError(w, err)
			return
		}
		respond(w, http.StatusOK, billingURLResponse{URL: url})
	})))
	// Methodless fallbacks keep 405 inside the same private response boundary.
	for _, path := range []string{base, base + "/standing", base + "/checkout", base + "/portal"} {
		mux.HandleFunc(path, noReferrer(studioRoute("", false, nil)))
	}
}

// billingRoute is studioRoute (private no-store, method, query and GET-body rules) plus the billing
// transport rules that need no database: no Idempotency-Key on any route and a canonical store id.
func billingRoute(method string, next http.HandlerFunc) http.HandlerFunc {
	return studioRoute(method, false, func(w http.ResponseWriter, r *http.Request) {
		if len(r.Header.Values("Idempotency-Key")) != 0 || !command.ValidID(r.PathValue("store_id")) {
			respondError(w, http.StatusUnprocessableEntity, "invalid_request")
			return
		}
		next(w, r)
	})
}

// billingAuth proves billing:manage in a scoped transaction that commits immediately, and hands back the
// resolved scope. The service re-verifies the caller in every one of its own transactions.
func billingAuth(w http.ResponseWriter, r *http.Request, pool *pgxpool.Pool) (platform.Scope, string, bool) {
	if !canonicalBearer(r) {
		respondError(w, http.StatusUnauthorized, "unauthorized")
		return platform.Scope{}, "", false
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	var scope platform.Scope
	err := platform.WithScope(ctx, pool, bearerToken(r), r.PathValue("store_id"), "billing:manage",
		func(_ pgx.Tx, s platform.Scope) error { scope = s; return nil })
	if err != nil {
		respondBillingError(w, err)
		return platform.Scope{}, "", false
	}
	return scope, bearerToken(r), true
}

func respondBillingError(w http.ResponseWriter, err error) {
	status, code := billingClassify(err)
	respondError(w, status, code)
}

// billingClassify is the billing mapping (contract §5, brief B2): the billing sentinels first, then the
// shared claimsClassify table (deadlock and unknown errors become a retryable 503; PT412 is 402
// billing_restricted there, but no billing route raises it). No driver message is ever returned.
func billingClassify(err error) (int, string) {
	switch {
	case errors.Is(err, billing.ErrUnavailable):
		return http.StatusServiceUnavailable, "billing_unavailable"
	case errors.Is(err, billing.ErrSubscriptionExists):
		return http.StatusConflict, "subscription_exists"
	case errors.Is(err, billing.ErrNoCustomer):
		return http.StatusConflict, "no_billing_customer"
	case errors.Is(err, billing.ErrUnknownPrice):
		return http.StatusUnprocessableEntity, "invalid_request"
	}
	return claimsClassify(err)
}
