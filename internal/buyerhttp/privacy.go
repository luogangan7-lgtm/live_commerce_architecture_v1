// privacy.go owns the buyer privacy transport of contracts/customers-billing-v1.md §5 (FROZEN):
// GET /v1/buyer/privacy, PUT /v1/buyer/consents, POST /v1/buyer/privacy/export and POST /v1/buyer/privacy/erasure.
// The integrator mounts them in handler.go (routeKind, matchRoute, allowed, dispatch: integrator-hooks.patch);
// this file holds the paths and the four handlers.
//
// Non-goals: no consent, export or erasure rule (internal/customers and migrations/0078 decide them), no
// merchant route, no logging of bodies, consent state or the capability token, and no change to classify:
// privacy errors map through responseError{status,code} here.
//
// Store, tenant and owner come from the origin the request resolved and the capability the database checks,
// never from input. Every route runs in one commerce_buyer_runtime READ COMMITTED transaction. GET uses
// buyer.WithScope. Consent, export and erasure do NOT use buyer.WithScope: consent and export
// because WithScope's resolve_scope takes FOR SHARE on the owner row and their definers then take FOR UPDATE on it,
// so two concurrent requests of one buyer would deadlock on the upgrade (they lock first, inside the definer);
// erasure (D11) because a revoked capability must still reach customers.erase_owner to answer 410 erased on a
// retry after success. All three use buyerTransaction.

package buyerhttp

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"livecommerce/internal/attribution"
	"livecommerce/internal/buyer"
	"livecommerce/internal/customers"
)

const (
	privacyPath        = "/v1/buyer/privacy"
	consentsPath       = "/v1/buyer/consents"
	privacyExportPath  = "/v1/buyer/privacy/export"
	privacyErasurePath = "/v1/buyer/privacy/erasure"
)

// privacyGet returns the caller's consents and whether an erasure exists.
func (h *handler) privacyGet(ctx context.Context, w http.ResponseWriter, r *http.Request, storeID, token, key string) error {
	out, err := scoped(ctx, h.pool, token, storeID, func(c context.Context, tx pgx.Tx, s buyer.Scope) (customers.BuyerPrivacy, error) {
		return customers.ReadBuyerPrivacy(c, tx, s, token)
	})
	if err != nil {
		return privacyError(err)
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	writeOK(w, out)
	return nil
}

// consentPut records the buyer's own consent. The body is exactly {purpose,channel,granted,context}: any other
// key (source, policy_version, ...) is 422 (D12); the server derives source and policy version.
func (h *handler) consentPut(ctx context.Context, w http.ResponseWriter, r *http.Request, storeID, token, key string) error {
	var in customers.ConsentInput
	if err := decodeExact(r, &in, "purpose", "channel", "granted", "context"); err != nil {
		return err
	}
	out, err := buyerTransaction(ctx, h.pool, func(c context.Context, tx pgx.Tx) (customers.ConsentResult, error) {
		result, err := customers.BuyerSetConsent(c, tx, storeID, token, key, in)
		if err != nil || in.Purpose != customers.PurposeAdsPersonalization || !in.Granted {
			return result, err
		}
		// ads.put_capi_context (meta-ads-v1 A-3, 0080): same tx, grant only; the definer upserts the CAPI browser
		// context only while customers.consent_allows(ads_personalization, meta_ads) holds. User-Agent is the
		// browser's, forwarded by the storefront BFF on this route only.
		hash := sha256.Sum256([]byte(token))
		return result, attribution.PutCAPIContext(c, tx, hash[:], storeID, r.UserAgent())
	})
	if err != nil {
		return privacyError(err)
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	writeOK(w, out)
	return nil
}

// privacyExport streams the buyer's own data as one JSON attachment (<= 200 orders and 1 MiB, else 409
// export_too_large with no EXPORT row). It takes no body.
func (h *handler) privacyExport(ctx context.Context, w http.ResponseWriter, r *http.Request, storeID, token, key string) error {
	if err := noBody(r); err != nil {
		return err
	}
	body, err := buyerTransaction(ctx, h.pool, func(c context.Context, tx pgx.Tx) ([]byte, error) {
		return customers.BuyerExport(c, tx, storeID, token, key)
	})
	if err != nil {
		return privacyError(err)
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Disposition", `attachment; filename="my-data.json"`)
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body)
	return nil
}

// erasureResponse tells the buyer what happened and what is kept: order records stay for legal retention and
// the store can still contact them about existing orders (contract §5).
type erasureResponse struct {
	Erased         bool                     `json:"erased"`
	OrdersRetained bool                     `json:"orders_retained"`
	Summary        customers.ErasureSummary `json:"summary"`
}

// privacyErase erases the caller's owner after the typed confirmation {"confirm":"ERASE"}.
func (h *handler) privacyErase(ctx context.Context, w http.ResponseWriter, r *http.Request, storeID, token, key string) error {
	var in struct {
		Confirm string `json:"confirm"`
	}
	if err := decodeExact(r, &in, "confirm"); err != nil {
		return err
	}
	if in.Confirm != "ERASE" {
		return responseError{http.StatusUnprocessableEntity, "invalid_request"}
	}
	summary, err := buyerTransaction(ctx, h.pool, func(c context.Context, tx pgx.Tx) (customers.ErasureSummary, error) {
		return customers.BuyerErase(c, tx, token, storeID, key)
	})
	if err != nil {
		return privacyError(err)
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	writeOK(w, erasureResponse{Erased: true, OrdersRetained: true, Summary: summary})
	return nil
}

// decodeExact reads a strict JSON object that has exactly the given keys (D12). Media type, size and JSON
// validity errors keep decodeJSON's 415/400; a missing, extra or mistyped key is 422 invalid_request.
func decodeExact(r *http.Request, out any, keys ...string) error {
	var fields map[string]json.RawMessage
	if err := decodeJSON(r, &fields); err != nil {
		return err
	}
	if len(fields) != len(keys) {
		return responseError{http.StatusUnprocessableEntity, "invalid_request"}
	}
	for _, key := range keys {
		if _, found := fields[key]; !found {
			return responseError{http.StatusUnprocessableEntity, "invalid_request"}
		}
	}
	raw, err := json.Marshal(fields)
	if err != nil {
		return responseError{http.StatusUnprocessableEntity, "invalid_request"}
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if dec.Decode(out) != nil {
		return responseError{http.StatusUnprocessableEntity, "invalid_request"}
	}
	return nil
}

// privacyError maps the customers sentinels to the stable buyer codes (D3); everything else falls through to
// classify (buyer.ErrUnauthorized -> 401, command.ErrInvalid -> 422, unknown -> 503).
func privacyError(err error) error {
	switch {
	case errors.Is(err, customers.ErrIdempotencyConflict):
		return responseError{http.StatusConflict, "idempotency_conflict"}
	case errors.Is(err, customers.ErrErasureBlocked):
		return responseError{http.StatusConflict, "erasure_blocked"}
	case errors.Is(err, customers.ErrExportTooLarge):
		return responseError{http.StatusConflict, "export_too_large"}
	case errors.Is(err, customers.ErrErased):
		return responseError{http.StatusGone, "erased"}
	}
	return err
}

// buyerTransaction runs fn in one buyer-pool READ COMMITTED transaction with the same bounded timeouts as
// buyer.WithScope but WITHOUT resolving a capability first (D11). fn authenticates through the token hash inside
// the database. The transaction commits only when fn returns nil.
func buyerTransaction[T any](ctx context.Context, pool *pgxpool.Pool, fn func(context.Context, pgx.Tx) (T, error)) (out T, err error) {
	if pool == nil {
		return out, buyer.ErrUnauthorized
	}
	txCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	tx, err := pool.BeginTx(txCtx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return out, err
	}
	defer func() {
		if err != nil {
			cleanup, stop := context.WithTimeout(context.Background(), 2*time.Second)
			defer stop()
			_ = tx.Rollback(cleanup)
		}
	}()
	if _, err = tx.Exec(txCtx, `SELECT set_config('statement_timeout',$1,true),set_config('lock_timeout',$2,true),
		set_config('idle_in_transaction_session_timeout',$1,true)`, (5 * time.Second).String(), time.Second.String()); err != nil {
		return out, err
	}
	if out, err = fn(txCtx, tx); err != nil {
		return out, err
	}
	if err = tx.Commit(txCtx); err != nil {
		return out, err
	}
	return out, nil
}
