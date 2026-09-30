package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"livecommerce/internal/command"
	"livecommerce/internal/live"
	"livecommerce/internal/pagination"
	"livecommerce/internal/platform"
)

// registerStudioRoutes mounts planning (list/create/detail/edit) when studio is on, the MOCK rehearsal
// routes only with a media planner, and the input routes only with planner and browserInput. With
// media off those routes are absent (404), never a disabled stub (R1 ruling G2).
func registerStudioRoutes(mux *http.ServeMux, pool *pgxpool.Pool, studio bool, planner *live.MediaPlanner, browserInput *live.BrowserInputRuntime) {
	if !studio {
		return
	}
	if planner == nil {
		browserInput = nil // input routes need the planner
	}
	const base = "/v1/admin/stores/{store_id}/live-sessions"
	mux.HandleFunc("GET "+base, studioRoute(http.MethodGet, true, func(w http.ResponseWriter, r *http.Request) {
		page, err := studioPage(r.URL)
		if err != nil {
			respondError(w, http.StatusUnprocessableEntity, "invalid_request")
			return
		}
		scoped(pool, "live:read", func(ctx context.Context, tx pgx.Tx, s platform.Scope, r *http.Request) (any, error) {
			return live.ListDrafts(ctx, tx, s, bearerToken(r), page)
		})(w, r)
	}))
	mux.HandleFunc("POST "+base, studioRoute(http.MethodPost, false, studioBodyRoute(pool, "live:manage", []string{"title", "scheduled_at", "aspect_ratio"}, func(ctx context.Context, tx pgx.Tx, s platform.Scope, r *http.Request, in live.DraftInput) (any, error) {
		return live.CreateDraft(ctx, tx, s, bearerToken(r), r.Header.Get("Idempotency-Key"), in)
	})))
	mux.HandleFunc("GET "+base+"/{session_id}", studioRoute(http.MethodGet, false, func(w http.ResponseWriter, r *http.Request) {
		if !command.ValidID(r.PathValue("session_id")) {
			respondError(w, http.StatusUnprocessableEntity, "invalid_request")
			return
		}
		scoped(pool, "live:read", func(ctx context.Context, tx pgx.Tx, s platform.Scope, r *http.Request) (any, error) {
			out, err := live.GetStudio(ctx, tx, s, bearerToken(r), r.PathValue("session_id"))
			if err != nil {
				return nil, err
			}
			return studioDetail{Studio: out, MediaEnabled: planner != nil}, nil
		})(w, r)
	}))
	type edit struct {
		live.DraftInput
		ExpectedVersion int64 `json:"expected_version"`
	}
	mux.HandleFunc("PATCH "+base+"/{session_id}", studioRoute(http.MethodPatch, false, studioBodyRoute(pool, "live:manage", []string{"title", "scheduled_at", "aspect_ratio", "expected_version"}, func(ctx context.Context, tx pgx.Tx, s platform.Scope, r *http.Request, in edit) (any, error) {
		return live.UpdateDraft(ctx, tx, s, bearerToken(r), r.Header.Get("Idempotency-Key"), r.PathValue("session_id"), in.ExpectedVersion, in.DraftInput)
	})))
	for _, path := range []string{base, base + "/{session_id}"} {
		mux.HandleFunc(path, studioRoute("", false, nil))
	}
	if planner == nil {
		return
	}
	type start struct {
		AuthorizationID        string `json:"authorization_id"`
		ExpectedSessionVersion int64  `json:"expected_session_version"`
	}
	mux.HandleFunc("POST "+base+"/{session_id}/rehearsal/start", studioRoute(http.MethodPost, false, studioBodyRoute(pool, "live:manage", []string{"authorization_id", "expected_session_version"}, func(ctx context.Context, tx pgx.Tx, s platform.Scope, r *http.Request, in start) (any, error) {
		out, err := planner.PlanStart(ctx, tx, s, bearerToken(r), r.Header.Get("Idempotency-Key"), live.MediaStartInput{SessionID: r.PathValue("session_id"), AuthorizationID: in.AuthorizationID, ExpectedSessionVersion: in.ExpectedSessionVersion})
		if err != nil {
			return nil, err
		}
		return studioReceipt{SessionID: out.SessionID, AttemptID: out.AttemptID, State: out.State}, nil
	})))
	type stop struct {
		AttemptID string `json:"attempt_id"`
	}
	mux.HandleFunc("POST "+base+"/{session_id}/rehearsal/stop", studioRoute(http.MethodPost, false, studioBodyRoute(pool, "live:manage", []string{"attempt_id"}, func(ctx context.Context, tx pgx.Tx, s platform.Scope, r *http.Request, in stop) (any, error) {
		out, err := planner.RequestStop(ctx, tx, s, bearerToken(r), r.Header.Get("Idempotency-Key"), live.MediaStopInput{SessionID: r.PathValue("session_id"), AttemptID: in.AttemptID})
		if err != nil {
			return nil, err
		}
		return studioReceipt{SessionID: out.SessionID, AttemptID: out.AttemptID, State: out.State}, nil
	})))
	if browserInput != nil {
		mux.HandleFunc("GET "+base+"/{session_id}/input", studioRoute(http.MethodGet, false, func(w http.ResponseWriter, r *http.Request) {
			if !command.ValidID(r.PathValue("session_id")) {
				respondError(w, http.StatusUnprocessableEntity, "invalid_request")
				return
			}
			scoped(pool, "live:read", func(ctx context.Context, tx pgx.Tx, s platform.Scope, r *http.Request) (any, error) {
				return live.GetStudioInput(ctx, tx, s, bearerToken(r), r.PathValue("session_id"))
			})(w, r)
		}))
		mux.HandleFunc("GET "+base+"/{session_id}/input/prepared", studioRoute(http.MethodGet, false, func(w http.ResponseWriter, r *http.Request) {
			if !command.ValidID(r.PathValue("session_id")) {
				respondError(w, http.StatusUnprocessableEntity, "invalid_request")
				return
			}
			scoped(pool, "live:read", func(ctx context.Context, tx pgx.Tx, s platform.Scope, r *http.Request) (any, error) {
				return live.GetStudioInputPrepared(ctx, tx, s, bearerToken(r), r.PathValue("session_id"), browserInput)
			})(w, r)
		}))
		mux.HandleFunc("POST "+base+"/{session_id}/input/start", studioRoute(http.MethodPost, false, studioBodyRoute(pool, "live:manage", []string{"authorization_id", "expected_session_version"}, func(ctx context.Context, tx pgx.Tx, s platform.Scope, r *http.Request, in start) (any, error) {
			out, err := planner.PlanBrowserInputStart(ctx, tx, s, bearerToken(r), r.Header.Get("Idempotency-Key"), live.MediaStartInput{SessionID: r.PathValue("session_id"), AuthorizationID: in.AuthorizationID, ExpectedSessionVersion: in.ExpectedSessionVersion}, browserInput)
			if err != nil {
				return nil, err
			}
			return studioReceipt{SessionID: out.SessionID, AttemptID: out.AttemptID, State: out.State}, nil
		})))
		type reserve struct {
			AttemptID              string `json:"attempt_id"`
			ExpectedSessionVersion int64  `json:"expected_session_version"`
		}
		mux.HandleFunc("POST "+base+"/{session_id}/input/token", studioRoute(http.MethodPost, false, func(w http.ResponseWriter, r *http.Request) {
			in, ok := studioDecodeBody[reserve](w, r, []string{"attempt_id", "expected_session_version"})
			if !ok {
				return
			}
			header := r.Header.Get("Authorization")
			if !strings.HasPrefix(header, "Bearer ") || strings.ContainsAny(strings.TrimPrefix(header, "Bearer "), " \t\r\n") {
				respondError(w, http.StatusUnauthorized, "unauthorized")
				return
			}
			var grant live.MediaInputGrant
			ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
			defer cancel()
			err := platform.WithScope(ctx, pool, bearerToken(r), r.PathValue("store_id"), "live:manage", func(tx pgx.Tx, s platform.Scope) error {
				var reserveErr error
				grant, reserveErr = planner.ReserveBrowserInput(ctx, tx, s, bearerToken(r), r.Header.Get("Idempotency-Key"), live.MediaInputReserveInput{SessionID: r.PathValue("session_id"), AttemptID: in.AttemptID, ExpectedSessionVersion: in.ExpectedSessionVersion}, browserInput)
				return reserveErr
			})
			if err != nil {
				status, code := classify(err)
				respondError(w, status, code)
				return
			}
			// WithScope returning nil is the COMMIT acknowledgement; no token exists before it.
			url, token, err := browserInput.MintPublisher(grant)
			if err != nil {
				status, code := classify(err)
				respondError(w, status, code)
				return
			}
			body, err := json.Marshal(struct {
				AttemptID         string `json:"attempt_id"`
				RoomName          string `json:"room_name"`
				PublisherIdentity string `json:"publisher_identity"`
				URL               string `json:"url"`
				Token             string `json:"token"`
				ExpiresAt         int64  `json:"expires_at"`
			}{grant.AttemptID, grant.RoomName, grant.PublisherIdentity, url, token.Bearer(), grant.ExpiresAt})
			if err != nil || len(body) > 8192 {
				respondError(w, http.StatusInternalServerError, "internal")
				return
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(body)
		}))
	}
	// Methodless fallbacks keep method errors inside the same private response
	// boundary. Optional input routes are absent until explicitly configured.
	for _, path := range []string{base + "/{session_id}/rehearsal/start", base + "/{session_id}/rehearsal/stop"} {
		mux.HandleFunc(path, studioRoute("", false, nil))
	}
	if browserInput != nil {
		for _, path := range []string{base + "/{session_id}/input", base + "/{session_id}/input/prepared", base + "/{session_id}/input/start", base + "/{session_id}/input/token"} {
			mux.HandleFunc(path, studioRoute("", false, nil))
		}
	}
}

// studioDetail is the GET /{session_id} body: the studio-v1 projection plus media_enabled, the
// read-only capability the admin Studio uses to hide rehearsal controls (R1 ruling G2).
type studioDetail struct {
	live.Studio
	MediaEnabled bool `json:"media_enabled"`
}

type studioReceipt struct {
	SessionID string `json:"session_id"`
	AttemptID string `json:"attempt_id"`
	State     string `json:"state"`
}

func studioRoute(method string, query bool, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "private, no-store")
		if r.Method != method {
			respondError(w, http.StatusMethodNotAllowed, "method_not_allowed")
			return
		}
		if !query && (r.URL.RawQuery != "" || r.URL.ForceQuery) {
			respondError(w, http.StatusUnprocessableEntity, "invalid_request")
			return
		}
		if method == http.MethodGet && (r.Body != nil && r.Body != http.NoBody || r.ContentLength != 0 || len(r.TransferEncoding) != 0 || r.Header.Get("Idempotency-Key") != "") {
			respondError(w, http.StatusUnprocessableEntity, "invalid_request")
			return
		}
		next(w, r)
	}
}

func studioPage(u *url.URL) (pagination.Request, error) {
	var out pagination.Request
	if u.ForceQuery || len(u.RawQuery) > 4096 {
		return out, command.ErrInvalid
	}
	if u.RawQuery != "" {
		for _, field := range strings.Split(u.RawQuery, "&") {
			name, value, ok := strings.Cut(field, "=")
			if !ok || value == "" || (name != "limit" && name != "cursor") || strings.ContainsAny(value, "%+=") {
				return out, command.ErrInvalid
			}
		}
	}
	values, err := url.ParseQuery(u.RawQuery)
	if err != nil {
		return out, command.ErrInvalid
	}
	for name, list := range values {
		if len(list) != 1 || list[0] == "" {
			return out, command.ErrInvalid
		}
		switch name {
		case "limit":
			n, err := strconv.Atoi(list[0])
			if err != nil || n < 1 || n > 100 || strconv.Itoa(n) != list[0] {
				return out, command.ErrInvalid
			}
			out.Limit = n
		case "cursor":
			if len(list[0]) > 1024 {
				return out, command.ErrInvalid
			}
			out.Cursor = list[0]
		default:
			return out, command.ErrInvalid
		}
	}
	return out, nil
}

// The generic bodyRoute does not reject duplicate JSON keys; Studio does.
func studioBodyRoute[T any](pool *pgxpool.Pool, permission string, fields []string, fn func(context.Context, pgx.Tx, platform.Scope, *http.Request, T) (any, error)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		in, ok := studioDecodeBody[T](w, r, fields)
		if !ok {
			return
		}
		scoped(pool, permission, func(ctx context.Context, tx pgx.Tx, s platform.Scope, r *http.Request) (any, error) {
			return fn(ctx, tx, s, r, in)
		})(w, r)
	}
}

func studioDecodeBody[T any](w http.ResponseWriter, r *http.Request, fields []string) (T, bool) {
	in, _, ok := studioDecodeRaw[T](w, r, fields)
	return in, ok
}

// studioDecodeRaw is studioDecodeBody that also returns the validated body bytes, so
// claims.go can add key-presence rules without reading the request twice. It has
// already written the 415/400 response whenever it returns false.
func studioDecodeRaw[T any](w http.ResponseWriter, r *http.Request, fields []string) (T, []byte, bool) {
	var in T
	media, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || media != "application/json" {
		respondError(w, http.StatusUnsupportedMediaType, "json_required")
		return in, nil, false
	}
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	defer r.Body.Close()
	raw, err := io.ReadAll(r.Body)
	if err != nil || !utf8.Valid(raw) || !studioUniqueJSON(raw, fields) {
		respondError(w, http.StatusBadRequest, "invalid_json")
		return in, nil, false
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&in); err != nil {
		respondError(w, http.StatusBadRequest, "invalid_json")
		return in, nil, false
	}
	var extra any
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		respondError(w, http.StatusBadRequest, "invalid_json")
		return in, nil, false
	}
	return in, raw, true
}

func studioUniqueJSON(raw []byte, fields []string) bool {
	d := json.NewDecoder(bytes.NewReader(raw))
	allowed := make(map[string]bool, len(fields))
	for _, field := range fields {
		allowed[field] = true
	}
	if !studioValue(d, 0, allowed) {
		return false
	}
	_, err := d.Token()
	return errors.Is(err, io.EOF)
}

func studioValue(d *json.Decoder, depth int, allowed map[string]bool) bool {
	if depth > 16 {
		return false
	}
	token, err := d.Token()
	if err != nil {
		return false
	}
	switch token {
	case json.Delim('{'):
		seen := map[string]bool{}
		for d.More() {
			keyToken, err := d.Token()
			if err != nil {
				return false
			}
			key, ok := keyToken.(string)
			if !ok || seen[key] || (depth == 0 && !allowed[key]) {
				return false
			}
			seen[key] = true
			if !studioValue(d, depth+1, nil) {
				return false
			}
		}
		end, err := d.Token()
		return err == nil && end == json.Delim('}')
	case json.Delim('['):
		for d.More() {
			if !studioValue(d, depth+1, nil) {
				return false
			}
		}
		end, err := d.Token()
		return err == nil && end == json.Delim(']')
	case json.Delim('}'), json.Delim(']'):
		return false
	default:
		return true
	}
}
