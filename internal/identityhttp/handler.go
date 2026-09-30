// Package identityhttp owns the trusted identity HTTP surface (/v1/identity/*) that only the local
// BFF may call, authenticated by a fixed BFF key: OIDC login start and complete, first store
// bootstrap and logout (handler.go), and the merchant password routes /v1/identity/password/*
// (password.go, contracts/merchant-password-auth-v1.md §7.1).
//
// It never handles cookies, Origin or CSRF (those belong to Next), never issues a business API
// token, never lets the business runtime pool sign in, and never trusts X-Commerce-Client-IP before
// the BFF-key check: it wraps internal/identity only.
package identityhttp

import (
	"context"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"strings"
	"time"

	"livecommerce/internal/httperror"
	"livecommerce/internal/identity"
)

// A narrow seam lets transport tests prove rejection before any identity I/O.
type service interface {
	Start(context.Context) (identity.Flow, error)
	Complete(context.Context, string, string, string) (identity.Session, error)
	CreateInitialStore(context.Context, string, string, identity.StoreRequest) (identity.Store, error)
	Logout(context.Context, string) error
}

func ValidSecret(value string) bool {
	decoded, err := base64.RawURLEncoding.Strict().DecodeString(value)
	return len(value) == 43 && err == nil && len(decoded) == 32
}

func NewHandler(s service, bffKey string) (http.Handler, error) {
	if s == nil || !ValidSecret(bffKey) {
		return nil, errors.New("invalid identity transport configuration")
	}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/identity/login/start", func(w http.ResponseWriter, r *http.Request) {
		var in struct{}
		if !body(w, r, &in) {
			return
		}
		flow, err := s.Start(r.Context())
		if err != nil {
			failure(w, err)
			return
		}
		respond(w, map[string]any{"authorization_url": flow.URL, "binding": flow.Binding, "expires_at": flow.ExpiresAt})
	})
	mux.HandleFunc("POST /v1/identity/login/complete", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			State   string `json:"state"`
			Binding string `json:"binding"`
			Code    string `json:"code"`
		}
		if !body(w, r, &in) {
			return
		}
		session, err := s.Complete(r.Context(), in.State, in.Binding, in.Code)
		if err != nil {
			failure(w, err)
			return
		}
		respond(w, map[string]any{"token": session.Token, "expires_at": session.ExpiresAt})
	})
	mux.HandleFunc("POST /v1/identity/initial-store", func(w http.ResponseWriter, r *http.Request) {
		var in identity.StoreRequest
		if !body(w, r, &in) {
			return
		}
		token := bearer(r)
		if token == "" {
			failure(w, identity.ErrUnauthorized)
			return
		}
		if len(r.Header.Values("Idempotency-Key")) != 1 {
			failure(w, identity.ErrInvalid)
			return
		}
		result, err := s.CreateInitialStore(r.Context(), token, r.Header.Get("Idempotency-Key"), in)
		if err != nil {
			failure(w, err)
			return
		}
		respond(w, result)
	})
	mux.HandleFunc("POST /v1/identity/logout", func(w http.ResponseWriter, r *http.Request) {
		var in struct{}
		if !body(w, r, &in) {
			return
		}
		token := bearer(r)
		if token == "" {
			failure(w, identity.ErrUnauthorized)
			return
		}
		if err := s.Logout(r.Context(), token); err != nil {
			failure(w, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	return httperror.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if len(r.Header.Values("X-Commerce-BFF-Key")) != 1 || subtle.ConstantTimeCompare([]byte(r.Header.Get("X-Commerce-BFF-Key")), []byte(bffKey)) != 1 {
			failure(w, identity.ErrUnauthorized)
			return
		}
		// Browser headers are not forwarded by the BFF. Refuse accidental direct
		// browser exposure even when an operator has leaked a transport credential.
		if len(r.Header.Values("Origin")) > 0 || len(r.Header.Values("Cookie")) > 0 {
			httperror.Write(w, http.StatusForbidden, "forbidden")
			return
		}
		if r.URL.RawQuery != "" {
			failure(w, identity.ErrInvalid)
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
		defer cancel()
		mux.ServeHTTP(w, r.WithContext(ctx))
	})), nil
}

func body[T any](w http.ResponseWriter, r *http.Request, out *T) bool {
	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/json" {
		httperror.Write(w, http.StatusUnsupportedMediaType, "json_required")
		return false
	}
	r.Body = http.MaxBytesReader(w, r.Body, 65536)
	defer r.Body.Close()
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	var pointer *T
	if err := decoder.Decode(&pointer); err != nil || pointer == nil {
		httperror.Write(w, http.StatusBadRequest, "invalid_json")
		return false
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		httperror.Write(w, http.StatusBadRequest, "invalid_json")
		return false
	}
	*out = *pointer
	return true
}

func bearer(r *http.Request) string {
	if len(r.Header.Values("Authorization")) != 1 {
		return ""
	}
	value := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	if value == r.Header.Get("Authorization") || !ValidSecret(value) {
		return ""
	}
	return value
}

func respond(w http.ResponseWriter, value any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(value)
}

func failure(w http.ResponseWriter, err error) {
	status, code := http.StatusServiceUnavailable, "unavailable"
	switch {
	case errors.Is(err, identity.ErrInvalid):
		status, code = http.StatusUnprocessableEntity, "invalid_request"
	case errors.Is(err, identity.ErrUnauthorized):
		status, code = http.StatusUnauthorized, "unauthorized"
	case errors.Is(err, identity.ErrDisabled):
		status, code = http.StatusForbidden, "forbidden"
	case errors.Is(err, identity.ErrConflict):
		status, code = http.StatusConflict, "conflict"
	}
	httperror.Write(w, status, code)
}
