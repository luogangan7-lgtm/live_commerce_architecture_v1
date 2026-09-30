// claims.go owns the merchant keyword-claims HTTP adapter M1–M7 (contract
// contracts/live-keyword-claims-v1.md §7.1): the Studio › Claims routes under
// /v1/admin/stores/{store_id}/live-sessions/{session_id}/claims.
//
// Non-goals: no claims rule (internal/claims decides every invariant, CAS, bound and
// permission re-check inside its own receipt), no buyer route (internal/buyerhttp owns
// B1–B2), no key loading (cmd/api decodes COMMERCE_CLAIMS_LABEL_KEY), and no logging of
// bodies, labels, comment text, bearer or link tokens.
//
// Each route runs in one commerce_runtime READ COMMITTED transaction (platform.WithScope, opened with
// the route's permission) whose nil return is the COMMIT acknowledgement M7 waits for, and keeps the
// Studio private-response boundary (studio.go helpers): Cache-Control private, no-store, strict JSON,
// query rejection and methodless 405 fallbacks.

package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"livecommerce/internal/claims"
	"livecommerce/internal/command"
	"livecommerce/internal/platform"
)

// claimsKey mirrors command.Run's receipt key grammar so a malformed key is rejected
// before a transaction opens (command.Run would reject it again inside).
var claimsKey = regexp.MustCompile(`^[A-Za-z0-9_.:-]{8,128}$`)

// claimLinkResponse is the only M7 body (§7.1). Token is the one-time link token on the
// first execution and null on a replay; it is built only after COMMIT is acknowledged.
type claimLinkResponse struct {
	Token      *string   `json:"token"`
	Generation int64     `json:"generation"`
	ExpiresAt  time.Time `json:"expires_at"`
	Released   bool      `json:"released"`
	Replayed   bool      `json:"replayed"`
}

// registerClaimRoutes mounts M1–M7 on the admin mux when labels is non-nil (cmd/api sets it
// only when COMMERCE_CLAIMS_ENABLED=1 with Studio enabled). Called once by NewHandler.
// Every route opens its own platform.WithScope transaction; reads use live:read and writes
// live:manage at scope open, and the claims functions check again after lock waits.
func registerClaimRoutes(mux *http.ServeMux, pool *pgxpool.Pool, labels *claims.LabelKey) {
	if labels == nil {
		return
	}
	const base = "/v1/admin/stores/{store_id}/live-sessions/{session_id}/claims"
	// M1 board: window, offers and per-reason stats of the current window generation.
	mux.HandleFunc("GET "+base, claimsRoute(http.MethodGet, false, claimsScoped(pool, "live:read",
		func(ctx context.Context, tx pgx.Tx, s platform.Scope, r *http.Request) (any, error) {
			return claims.GetBoard(ctx, tx, s, bearerToken(r), r.PathValue("session_id"))
		})))
	// M2 open, close or re-mode the session's claim window (version CAS).
	mux.HandleFunc("POST "+base+"/window", claimsRoute(http.MethodPost, false, claimsBodyRoute(pool,
		[]string{"expected_version", "state", "match_mode"}, nil,
		func(ctx context.Context, tx pgx.Tx, s platform.Scope, r *http.Request, in claims.WindowInput) (any, error) {
			return claims.SetWindow(ctx, tx, s, bearerToken(r), r.Header.Get("Idempotency-Key"), r.PathValue("session_id"), in)
		})))
	// M3 bind a keyword to a SKU inside this session.
	mux.HandleFunc("POST "+base+"/offers", claimsRoute(http.MethodPost, false, claimsBodyRoute(pool,
		[]string{"keyword", "sku_id", "max_quantity_per_claim"}, nil,
		func(ctx context.Context, tx pgx.Tx, s platform.Scope, r *http.Request, in claims.OfferInput) (any, error) {
			return claims.CreateOffer(ctx, tx, s, bearerToken(r), r.Header.Get("Idempotency-Key"), r.PathValue("session_id"), in)
		})))
	// M4 change max quantity / active of one offer (version CAS). "active" is required:
	// a missing boolean must never decode to a silent deactivation.
	mux.HandleFunc("PATCH "+base+"/offers/{offer_id}", claimsRoute(http.MethodPatch, false, claimsBodyRoute(pool,
		[]string{"expected_version", "max_quantity_per_claim", "active"}, nil,
		func(ctx context.Context, tx pgx.Tx, s platform.Scope, r *http.Request, in claims.OfferUpdate) (any, error) {
			return claims.UpdateOffer(ctx, tx, s, bearerToken(r), r.Header.Get("Idempotency-Key"), r.PathValue("session_id"), r.PathValue("offer_id"), in)
		})))
	// M5 MOCK operator ingress: text plus exactly one of bundle_id / actor_label. REJECTED
	// and WINDOW_CLOSED are 200 results (data, not errors).
	mux.HandleFunc("POST "+base+"/manual", claimsRoute(http.MethodPost, false, claimsBodyRoute(pool,
		[]string{"text"}, []string{"bundle_id", "actor_label"},
		func(ctx context.Context, tx pgx.Tx, s platform.Scope, r *http.Request, in claims.ManualClaimInput) (any, error) {
			return claims.RecordManualClaim(ctx, tx, s, *labels, bearerToken(r), r.Header.Get("Idempotency-Key"), r.PathValue("session_id"), in)
		})))
	// M6 bundle list, newest first; only limit and cursor are accepted (no label filter).
	mux.HandleFunc("GET "+base+"/bundles", claimsRoute(http.MethodGet, true, func(w http.ResponseWriter, r *http.Request) {
		page, err := studioPage(r.URL)
		if err != nil {
			respondError(w, http.StatusUnprocessableEntity, "invalid_request")
			return
		}
		claimsScoped(pool, "live:read", func(ctx context.Context, tx pgx.Tx, s platform.Scope, r *http.Request) (any, error) {
			return claims.ListBundles(ctx, tx, s, bearerToken(r), r.PathValue("session_id"), page)
		})(w, r)
	}))
	// M7 issue / rotate / release+rotate a bundle's buyer link. Every M7 response, including
	// transport errors and the 405 fallback, forbids a Referer.
	mux.HandleFunc("POST "+base+"/bundles/{bundle_id}/link", noReferrer(claimsRoute(http.MethodPost, false, claimLinkRoute(pool))))
	mux.HandleFunc(base+"/bundles/{bundle_id}/link", noReferrer(studioRoute("", false, nil)))
	// Comment source (meta-claims-intake-v1 §2, claimsource.go): GET/PUT claim-source of a session.
	registerClaimSourceRoutes(mux, pool)
	// Methodless fallbacks keep 405 inside the same private response boundary.
	for _, path := range []string{base, base + "/window", base + "/offers", base + "/offers/{offer_id}", base + "/manual", base + "/bundles"} {
		mux.HandleFunc(path, studioRoute("", false, nil))
	}
}

// noReferrer sets Referrer-Policy: no-referrer before any M7 response is written.
func noReferrer(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Referrer-Policy", "no-referrer")
		next(w, r)
	}
}

// claimLinkRoute is M7. The token exists only inside claims.IssueLink's receipt closure
// until platform.WithScope returns nil (COMMIT acknowledged, studio input/token pattern);
// only then is it written, once, into this response. A replay (same key and body) returns
// token null with replayed true. registerClaimRoutes wraps it in noReferrer.
func claimLinkRoute(pool *pgxpool.Pool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		in, ok := claimsBody[claims.LinkInput](w, r, []string{"expected_generation", "release_binding"}, nil)
		if !ok {
			return
		}
		if !canonicalBearer(r) {
			respondError(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()
		var issued claims.IssuedLink
		err := platform.WithScope(ctx, pool, bearerToken(r), r.PathValue("store_id"), "live:manage", func(tx pgx.Tx, s platform.Scope) error {
			var issueErr error
			issued, issueErr = claims.IssueLink(ctx, tx, s, bearerToken(r), r.Header.Get("Idempotency-Key"),
				r.PathValue("session_id"), r.PathValue("bundle_id"), in)
			return issueErr
		})
		if err != nil {
			status, code := claimsClassify(err)
			respondError(w, status, code)
			return
		}
		// WithScope returning nil is the COMMIT acknowledgement; no token left the process before it.
		out := claimLinkResponse{Generation: issued.Generation, ExpiresAt: issued.ExpiresAt.UTC(),
			Released: issued.Released, Replayed: issued.Replayed}
		if !issued.Replayed {
			token := string(issued.Token) // the only read of the raw token value
			out.Token = &token
		}
		respond(w, http.StatusOK, out)
	}
}

// claimsRoute is studioRoute (private no-store, method, query and GET-body rules) plus the
// claims transport rules that need no database: an Idempotency-Key header is forbidden on
// reads (even when empty) and required exactly once in the receipt grammar on writes, and
// every path id is a canonical UUID (422 before any transaction).
func claimsRoute(method string, query bool, next http.HandlerFunc) http.HandlerFunc {
	return studioRoute(method, query, func(w http.ResponseWriter, r *http.Request) {
		keys := r.Header.Values("Idempotency-Key")
		if (method == http.MethodGet && len(keys) != 0) ||
			(method != http.MethodGet && (len(keys) != 1 || !claimsKey.MatchString(keys[0]))) {
			respondError(w, http.StatusUnprocessableEntity, "invalid_request")
			return
		}
		for _, name := range []string{"session_id", "offer_id", "bundle_id"} {
			if value := r.PathValue(name); value != "" && !command.ValidID(value) {
				respondError(w, http.StatusUnprocessableEntity, "invalid_request")
				return
			}
		}
		next(w, r)
	})
}

// claimsBodyRoute decodes a strict claims body (claimsBody) and runs fn in a live:manage
// platform.WithScope transaction. Used by M2–M5.
func claimsBodyRoute[T any](pool *pgxpool.Pool, required, oneOf []string, fn func(context.Context, pgx.Tx, platform.Scope, *http.Request, T) (any, error)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		in, ok := claimsBody[T](w, r, required, oneOf)
		if !ok {
			return
		}
		claimsScoped(pool, "live:manage", func(ctx context.Context, tx pgx.Tx, s platform.Scope, r *http.Request) (any, error) {
			return fn(ctx, tx, s, r, in)
		})(w, r)
	}
}

// claimsBody is the Studio strict decoder (JSON media type, ≤64 KiB, UTF-8, allowed keys
// only, no duplicate keys, no unknown fields, no trailing data) plus: no top-level null
// (400 invalid_json, so null never becomes a zero-value false/0/""), every required key
// present and exactly one oneOf key present (422 invalid_request); optional keys are allowed but
// may be absent. It has written the error response whenever it returns false.
func claimsBody[T any](w http.ResponseWriter, r *http.Request, required, oneOf []string, optional ...string) (T, bool) {
	fields := append(append(append([]string{}, required...), oneOf...), optional...)
	in, raw, ok := studioDecodeRaw[T](w, r, fields)
	if !ok {
		return in, false
	}
	var present map[string]json.RawMessage
	if err := json.Unmarshal(raw, &present); err != nil {
		respondError(w, http.StatusBadRequest, "invalid_json")
		return in, false
	}
	for _, value := range present {
		if bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
			respondError(w, http.StatusBadRequest, "invalid_json")
			return in, false
		}
	}
	missing := false
	for _, name := range required {
		if _, found := present[name]; !found {
			missing = true
		}
	}
	chosen := 0
	for _, name := range oneOf {
		if _, found := present[name]; found {
			chosen++
		}
	}
	if missing || (len(oneOf) > 0 && chosen != 1) {
		respondError(w, http.StatusUnprocessableEntity, "invalid_request")
		return in, false
	}
	return in, true
}

// claimsScoped is scoped with claimsClassify.
func claimsScoped(pool *pgxpool.Pool, permission string, fn action) http.HandlerFunc {
	return scopedAs(pool, permission, claimsClassify, fn)
}

// claimsClassify is the §4.5 HTTP mapping: the shared classify table, except that a
// deadlock (40P01) is a retryable 503 rather than 409, and any unclassified error is 503
// "unavailable" rather than 500. No driver message or detail is ever returned.
func claimsClassify(err error) (int, string) {
	if errors.Is(err, claims.ErrBillingRestricted) { // billing-core B12: new window under RESTRICTED (PT412)
		return http.StatusPaymentRequired, "billing_restricted"
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "40P01" {
		return http.StatusServiceUnavailable, "retry_later"
	}
	status, code := classify(err)
	if status == http.StatusInternalServerError {
		return http.StatusServiceUnavailable, "unavailable"
	}
	return status, code
}

// canonicalBearer is scoped's Authorization shape check, for M7's hand-written scope.
func canonicalBearer(r *http.Request) bool {
	header := r.Header.Get("Authorization")
	return strings.HasPrefix(header, "Bearer ") && !strings.ContainsAny(strings.TrimPrefix(header, "Bearer "), " \t\r\n")
}
