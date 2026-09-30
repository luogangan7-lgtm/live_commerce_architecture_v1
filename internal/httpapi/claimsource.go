// claimsource.go owns the merchant comment-source HTTP adapter (meta-claims-intake-v1 §2, ruling h;
// docs/delivery/units/claim-source.md, frozen interface): GET and PUT
// /v1/admin/stores/{store_id}/live-sessions/{session_id}/claim-source (ruling o: the only spelling, matching
// the Studio/claims routes). Both bind a live session to a pasted Facebook post / live video or Instagram media.
//
// Non-goals: no parsing or binding rule (internal/claims.ParseClaimSourceInput, PutClaimSource and the
// live.put_claim_source definer decide every rule and permission), no Graph call, no logging of the
// pasted input, bodies or bearer tokens.
//
// Each route runs in one commerce_runtime READ COMMITTED transaction (platform.WithScope through scopedAs)
// whose nil return is the COMMIT acknowledgement the 200 waits for; responses are private no-store, strict
// JSON, no query, Idempotency-Key only on PUT (helpers in claims.go).
// Route -> Go endpoint: these handlers ARE the Go endpoints; the admin BFF forwards them unchanged.

package httpapi

import (
	"context"
	"errors"
	"net/http"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"livecommerce/internal/claims"
	"livecommerce/internal/platform"
)

// claimSourceBodyFields is the exact PUT body; every key is required and none may be null. "platform"
// is the only optional key (ruling p), also never null.
var claimSourceBodyFields = []string{"input", "private_reply", "reply_locale", "active", "expected_version"}

func registerClaimSourceRoutes(mux *http.ServeMux, pool *pgxpool.Pool) {
	const path = "/v1/admin/stores/{store_id}/live-sessions/{session_id}/claim-source"
	// GET: the session's current source or {"source": null} (live:read).
	mux.HandleFunc("GET "+path, claimsRoute(http.MethodGet, false,
		scopedAs(pool, "live:read", claimSourceClassify, func(ctx context.Context, tx pgx.Tx, s platform.Scope, r *http.Request) (any, error) {
			return claims.GetClaimSource(ctx, tx, s, bearerToken(r), r.PathValue("session_id"))
		})))
	// PUT: bind, re-bind, edit or deactivate (live:manage, integration:execute; version CAS).
	mux.HandleFunc("PUT "+path, claimsRoute(http.MethodPut, false, func(w http.ResponseWriter, r *http.Request) {
		in, ok := claimsBody[claims.ClaimSourceInput](w, r, claimSourceBodyFields, nil, "platform")
		if !ok {
			return
		}
		scopedAs(pool, "live:manage", claimSourceClassify, func(ctx context.Context, tx pgx.Tx, s platform.Scope, r *http.Request) (any, error) {
			return claims.PutClaimSource(ctx, tx, s, bearerToken(r), r.Header.Get("Idempotency-Key"), r.PathValue("session_id"), in)
		})(w, r)
	}))
	mux.HandleFunc(path, studioRoute("", false, nil)) // methodless fallback keeps 405 inside the private boundary
}

// claimSourceClassify maps the claim-source errors to their frozen codes, then the claims table
// (deadlock -> 503 retry_later, unclassified -> 503 unavailable). No driver message is returned.
func claimSourceClassify(err error) (int, string) {
	switch {
	case errors.Is(err, claims.ErrInputInvalid):
		return http.StatusUnprocessableEntity, "input_invalid"
	case errors.Is(err, claims.ErrInputUnresolvable):
		return http.StatusUnprocessableEntity, "input_unresolvable"
	case errors.Is(err, claims.ErrBindingMissing):
		return http.StatusConflict, "binding_missing"
	case errors.Is(err, claims.ErrBindingAmbiguous):
		return http.StatusConflict, "binding_ambiguous"
	case errors.Is(err, claims.ErrSourceConflict):
		return http.StatusConflict, "source_conflict"
	case errors.Is(err, claims.ErrVersionChanged):
		return http.StatusConflict, "version_changed"
	case errors.Is(err, claims.ErrPageTokenMissing):
		return http.StatusConflict, "page_token_missing"
	}
	return claimsClassify(err)
}
