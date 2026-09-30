// ads.go owns the merchant ads HTTP adapter of meta-ads-v1 §7 under /v1/admin/stores/{store_id}/ads (brief default D1: the repo
// convention, not /v1/merchant/ads; the admin BFF forwards these unchanged and issues the browser redirect itself, D6).
// Routes: POST meta/connect, GET meta/callback, GET meta/states/{state_id}, POST meta/bindings, GET settings, GET/POST
// drafts, GET/PUT drafts/{id}, POST drafts/{id}/approve|publish|pause|end, GET report, PUT capi.
//
// It decides no rule (internal/ads and the ads.* SQL definers do, including every permission after lock waits), never calls
// Meta itself (ads.ConnectFunc is injected in ads.Service), and never returns a driver message, a token, an OAuth code or a
// state value: the callback handler does not log, and no formatter here sees the URL query. Each route runs in one
// commerce_runtime READ COMMITTED transaction (platform.WithScope, opened with the route's permission) whose nil return is the
// COMMIT acknowledgement the 2xx waits for, and keeps the Studio private no-store boundary with strict JSON and query rejection.
// Route -> Go endpoint: these handlers ARE the Go endpoints (apps/admin lib/backend.ts callBackend mirrors them under /api/admin).

package httpapi

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"livecommerce/internal/ads"
	"livecommerce/internal/command"
	"livecommerce/internal/platform"
)

// Exact request bodies; every required key must be present and none may be null (claimsBody).
var (
	adsDraftFields   = []string{"ad_binding_id", "identity_binding_id", "template", "source_ref", "currency", "lifetime_budget_minor", "starts_at", "ends_at", "countries", "age_min", "age_max"}
	adsBindFields    = []string{"state_id", "ad_account_id"}
	adsApproveFields = []string{"revision"}
	adsPublishFields = []string{"publish_attempt"}
	adsCapiFields    = []string{"enabled"}
	adsDatePattern   = regexp.MustCompile(`^[0-9]{4}-[0-9]{2}-[0-9]{2}$`)
	adsCodePattern   = regexp.MustCompile(`^[\x21-\x7e]{1,1000}$`)
	adsStatePattern  = regexp.MustCompile(`^[A-Za-z0-9_-]{43}$`)
)

// registerAdsRoutes mounts the ads surface; a nil service leaves it unmounted (cmd/api sets it only when the ads worker's
// configuration and migration 0080 are present, contract 4.3).
func registerAdsRoutes(mux *http.ServeMux, pool *pgxpool.Pool, svc *ads.Service) {
	if svc == nil {
		return
	}
	const base = "/v1/admin/stores/{store_id}/ads"
	fallbacks := []string{"/meta/connect", "/meta/callback", "/meta/states/{state_id}", "/meta/bindings", "/settings", "/drafts",
		"/drafts/{draft_id}", "/drafts/{draft_id}/approve", "/drafts/{draft_id}/publish", "/drafts/{draft_id}/pause",
		"/drafts/{draft_id}/end", "/report", "/capi"}

	mux.HandleFunc("POST "+base+"/meta/connect", adsRoute(http.MethodPost, true, false, func(w http.ResponseWriter, r *http.Request) {
		if !adsNoBody(w, r) {
			return
		}
		adsScope(w, r, pool, "ads:manage", http.StatusCreated, func(ctx context.Context, tx pgx.Tx, s platform.Scope) (any, error) {
			return svc.Connect(ctx, tx, s, bearerToken(r), r.Header.Get("Idempotency-Key"))
		})
	}))
	mux.HandleFunc("GET "+base+"/meta/callback", adsRoute(http.MethodGet, false, true, func(w http.ResponseWriter, r *http.Request) {
		adsCallback(w, r, pool, svc)
	}))
	mux.HandleFunc("GET "+base+"/meta/states/{state_id}", adsRoute(http.MethodGet, false, false, func(w http.ResponseWriter, r *http.Request) {
		adsScope(w, r, pool, "ads:manage", http.StatusOK, func(ctx context.Context, tx pgx.Tx, s platform.Scope) (any, error) {
			return svc.GetState(ctx, tx, s, bearerToken(r), r.PathValue("state_id"))
		})
	}))
	mux.HandleFunc("POST "+base+"/meta/bindings", adsRoute(http.MethodPost, true, false, func(w http.ResponseWriter, r *http.Request) {
		in, ok := claimsBody[ads.BindInput](w, r, adsBindFields, nil, "dataset_id")
		if !ok {
			return
		}
		adsScope(w, r, pool, "ads:manage", http.StatusCreated, func(ctx context.Context, tx pgx.Tx, s platform.Scope) (any, error) {
			return svc.Bind(ctx, tx, s, bearerToken(r), r.Header.Get("Idempotency-Key"), in)
		})
	}))
	mux.HandleFunc("GET "+base+"/settings", adsRoute(http.MethodGet, false, false, func(w http.ResponseWriter, r *http.Request) {
		adsScope(w, r, pool, "ads:read", http.StatusOK, func(ctx context.Context, tx pgx.Tx, s platform.Scope) (any, error) {
			return svc.GetSettings(ctx, tx, s, bearerToken(r))
		})
	}))
	mux.HandleFunc("GET "+base+"/drafts", adsRoute(http.MethodGet, false, false, func(w http.ResponseWriter, r *http.Request) {
		adsScope(w, r, pool, "ads:read", http.StatusOK, func(ctx context.Context, tx pgx.Tx, s platform.Scope) (any, error) {
			return svc.ListDrafts(ctx, tx, s, bearerToken(r))
		})
	}))
	mux.HandleFunc("GET "+base+"/drafts/{draft_id}", adsRoute(http.MethodGet, false, false, func(w http.ResponseWriter, r *http.Request) {
		adsScope(w, r, pool, "ads:read", http.StatusOK, func(ctx context.Context, tx pgx.Tx, s platform.Scope) (any, error) {
			return svc.GetDraft(ctx, tx, s, bearerToken(r), r.PathValue("draft_id"))
		})
	}))
	mux.HandleFunc("POST "+base+"/drafts", adsRoute(http.MethodPost, true, false, func(w http.ResponseWriter, r *http.Request) {
		in, ok := claimsBody[ads.DraftInput](w, r, adsDraftFields, nil)
		if !ok {
			return
		}
		adsScope(w, r, pool, "ads:manage", http.StatusCreated, func(ctx context.Context, tx pgx.Tx, s platform.Scope) (any, error) {
			return svc.CreateDraft(ctx, tx, s, bearerToken(r), r.Header.Get("Idempotency-Key"), in)
		})
	}))
	mux.HandleFunc("PUT "+base+"/drafts/{draft_id}", adsRoute(http.MethodPut, true, false, func(w http.ResponseWriter, r *http.Request) {
		revision, ok := adsIfMatch(w, r)
		if !ok {
			return
		}
		in, ok := claimsBody[ads.DraftInput](w, r, adsDraftFields, nil)
		if !ok {
			return
		}
		adsScope(w, r, pool, "ads:manage", http.StatusOK, func(ctx context.Context, tx pgx.Tx, s platform.Scope) (any, error) {
			return svc.UpdateDraft(ctx, tx, s, bearerToken(r), r.Header.Get("Idempotency-Key"), r.PathValue("draft_id"), revision, in)
		})
	}))
	mux.HandleFunc("POST "+base+"/drafts/{draft_id}/approve", adsRoute(http.MethodPost, false, false, func(w http.ResponseWriter, r *http.Request) {
		in, ok := claimsBody[struct {
			Revision int `json:"revision"`
		}](w, r, adsApproveFields, nil)
		if !ok {
			return
		}
		adsScope(w, r, pool, "ads:approve", http.StatusOK, func(ctx context.Context, tx pgx.Tx, s platform.Scope) (any, error) {
			return svc.Approve(ctx, tx, s, bearerToken(r), r.PathValue("draft_id"), in.Revision)
		})
	}))
	mux.HandleFunc("POST "+base+"/drafts/{draft_id}/publish", adsRoute(http.MethodPost, true, false, func(w http.ResponseWriter, r *http.Request) {
		in, ok := claimsBody[struct {
			PublishAttempt int `json:"publish_attempt"`
		}](w, r, adsPublishFields, nil)
		if !ok {
			return
		}
		adsScope(w, r, pool, "ads:approve", http.StatusOK, func(ctx context.Context, tx pgx.Tx, s platform.Scope) (any, error) {
			return svc.Publish(ctx, tx, s, bearerToken(r), r.Header.Get("Idempotency-Key"), r.PathValue("draft_id"), in.PublishAttempt)
		})
	}))
	mux.HandleFunc("POST "+base+"/drafts/{draft_id}/pause", adsRoute(http.MethodPost, true, false, func(w http.ResponseWriter, r *http.Request) {
		if !adsNoBody(w, r) {
			return
		}
		adsScope(w, r, pool, "ads:manage", http.StatusOK, func(ctx context.Context, tx pgx.Tx, s platform.Scope) (any, error) {
			return svc.Pause(ctx, tx, s, bearerToken(r), r.Header.Get("Idempotency-Key"), r.PathValue("draft_id"))
		})
	}))
	mux.HandleFunc("POST "+base+"/drafts/{draft_id}/end", adsRoute(http.MethodPost, true, false, func(w http.ResponseWriter, r *http.Request) {
		if !adsNoBody(w, r) {
			return
		}
		adsScope(w, r, pool, "ads:approve", http.StatusOK, func(ctx context.Context, tx pgx.Tx, s platform.Scope) (any, error) {
			return svc.End(ctx, tx, s, bearerToken(r), r.Header.Get("Idempotency-Key"), r.PathValue("draft_id"))
		})
	}))
	mux.HandleFunc("GET "+base+"/report", adsRoute(http.MethodGet, false, true, func(w http.ResponseWriter, r *http.Request) {
		from, to, ok := adsReportWindow(r.URL.Query(), r.URL.RawQuery)
		if !ok {
			respondError(w, http.StatusUnprocessableEntity, "invalid_request")
			return
		}
		adsScope(w, r, pool, "ads:read", http.StatusOK, func(ctx context.Context, tx pgx.Tx, s platform.Scope) (any, error) {
			return svc.Report(ctx, tx, s, bearerToken(r), from, to)
		})
	}))
	mux.HandleFunc("PUT "+base+"/capi", adsRoute(http.MethodPut, true, false, func(w http.ResponseWriter, r *http.Request) {
		in, ok := claimsBody[ads.CapiInput](w, r, adsCapiFields, nil, "dataset_binding_id", "test_event_code")
		if !ok {
			return
		}
		adsScope(w, r, pool, "ads:manage", http.StatusOK, func(ctx context.Context, tx pgx.Tx, s platform.Scope) (any, error) {
			return svc.SetCapi(ctx, tx, s, bearerToken(r), r.Header.Get("Idempotency-Key"), in)
		})
	}))
	// Methodless fallbacks keep 405 inside the same private response boundary.
	for _, suffix := range fallbacks {
		mux.HandleFunc(base+suffix, studioRoute("", false, nil))
	}
}

// adsRoute is studioRoute plus the ads transport rules that need no database: Idempotency-Key exactly once (receipt grammar)
// where the route is keyed and forbidden elsewhere, and canonical UUID path ids (422 before any transaction).
func adsRoute(method string, keyed, query bool, next http.HandlerFunc) http.HandlerFunc {
	return studioRoute(method, query, func(w http.ResponseWriter, r *http.Request) {
		keys := r.Header.Values("Idempotency-Key")
		if (keyed && (len(keys) != 1 || !claimsKey.MatchString(keys[0]))) || (!keyed && len(keys) != 0) {
			respondError(w, http.StatusUnprocessableEntity, "invalid_request")
			return
		}
		for _, name := range []string{"store_id", "draft_id", "state_id"} {
			if value := r.PathValue(name); value != "" && !command.ValidID(value) {
				respondError(w, http.StatusUnprocessableEntity, "invalid_request")
				return
			}
		}
		next(w, r)
	})
}

// adsNoBody rejects any request body on a bodiless POST (pause, end, connect).
func adsNoBody(w http.ResponseWriter, r *http.Request) bool {
	if r.ContentLength != 0 || len(r.TransferEncoding) != 0 || (r.Body != nil && r.Body != http.NoBody) {
		respondError(w, http.StatusUnprocessableEntity, "invalid_request")
		return false
	}
	return true
}

// adsIfMatch parses the draft revision from If-Match: exactly one value, digits only, 1..1000.
func adsIfMatch(w http.ResponseWriter, r *http.Request) (int, bool) {
	values := r.Header.Values("If-Match")
	if len(values) == 1 {
		if n, err := strconv.Atoi(values[0]); err == nil && n >= 1 && n <= 1000 && strconv.Itoa(n) == values[0] {
			return n, true
		}
	}
	respondError(w, http.StatusUnprocessableEntity, "invalid_request")
	return 0, false
}

// adsReportWindow accepts exactly ?from=YYYY-MM-DD&to=YYYY-MM-DD (each once, nothing else, <= 92 days: SQL and the service
// re-check the span).
func adsReportWindow(values url.Values, raw string) (string, string, bool) {
	if len(raw) > 64 || len(values) != 2 || len(values["from"]) != 1 || len(values["to"]) != 1 ||
		!adsDatePattern.MatchString(values["from"][0]) || !adsDatePattern.MatchString(values["to"][0]) {
		return "", "", false
	}
	return values["from"][0], values["to"][0], true
}

// adsScope runs fn in a platform.WithScope transaction opened with permission and writes the response: status on success
// (only after COMMIT is acknowledged), the classified error otherwise.
func adsScope(w http.ResponseWriter, r *http.Request, pool *pgxpool.Pool, permission string, status int,
	fn func(context.Context, pgx.Tx, platform.Scope) (any, error)) {
	if !canonicalBearer(r) {
		respondError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	var result any
	err := platform.WithScope(ctx, pool, bearerToken(r), r.PathValue("store_id"), permission, func(tx pgx.Tx, s platform.Scope) error {
		var inner error
		result, inner = fn(ctx, tx, s)
		return inner
	})
	if err != nil {
		code, name := adsClassify(err)
		respondError(w, code, name)
		return
	}
	respond(w, status, result)
}

// adsCallback is GET meta/callback?code&state (D6): 200 {"state_id"}; the admin BFF issues the 303. The URL and query are never
// logged or echoed; the codes are the fixed state_mismatch 409, state_expired 410 and meta_connect_failed 502.
func adsCallback(w http.ResponseWriter, r *http.Request, pool *pgxpool.Pool, svc *ads.Service) {
	if !canonicalBearer(r) {
		respondError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	values := r.URL.Query()
	if len(r.URL.RawQuery) > 2200 || len(values) != 2 || len(values["code"]) != 1 || len(values["state"]) != 1 ||
		!adsCodePattern.MatchString(values["code"][0]) {
		respondError(w, http.StatusUnprocessableEntity, "invalid_request")
		return
	}
	if !adsStatePattern.MatchString(values["state"][0]) {
		respondError(w, http.StatusConflict, "state_mismatch")
		return
	}
	// The callback holds no request-wide deadline of 5 s: the code exchange is a network call bounded by the ConnectFunc's own timeout.
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	stateID, err := svc.Callback(ctx, pool, bearerToken(r), r.PathValue("store_id"), values["code"][0], values["state"][0])
	if err != nil {
		code, name := adsClassify(err)
		respondError(w, code, name)
		return
	}
	respond(w, http.StatusOK, map[string]string{"state_id": stateID})
}

// adsClassify maps ads refusals and ErrConnectFailed to their frozen statuses and codes, then the claims table (deadlock and
// unknown errors become a retryable 503). No driver message is ever returned.
func adsClassify(err error) (int, string) {
	var refused *ads.Refusal
	switch {
	case errors.As(err, &refused):
		return refused.Status, refused.Code
	case errors.Is(err, ads.ErrConnectFailed):
		return http.StatusBadGateway, "meta_connect_failed"
	}
	return claimsClassify(err)
}
