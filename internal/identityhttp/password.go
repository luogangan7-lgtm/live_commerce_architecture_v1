package identityhttp

// Private password-auth routes /v1/identity/password/{signup,login,reset,complete}
// (contracts/merchant-password-auth-v1.md §7.1). Called only by the admin BFF
// (apps/admin/app/api/auth/password/*), authenticated by the fixed BFF key; the Go endpoints behind
// them are identity.Passwords.{Signup,Login,Reset,Complete} in internal/identity/challenge.go.
//
// Same transport rules as the OIDC routes (handler.go): BFF key first, no Origin/Cookie, no query,
// strict JSON <= 64 KiB, no-store; plus exactly one valid X-Commerce-Client-IP, read only after the
// key check (ruling R-5), because that header is only trustworthy on the BFF-authenticated call.

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"net/http"
	"net/netip"
	"strconv"
	"time"

	"livecommerce/internal/httperror"
	"livecommerce/internal/identity"
)

// passwordService mirrors *identity.Passwords; a narrow seam lets transport tests prove rejection
// before any identity I/O.
type passwordService interface {
	Signup(ctx context.Context, ip netip.Addr, email, password, locale string) (identity.Challenge, error)
	Login(ctx context.Context, ip netip.Addr, email, password, locale string) (identity.Challenge, error)
	Reset(ctx context.Context, ip netip.Addr, email, locale string) (identity.Challenge, error)
	Complete(ctx context.Context, ip netip.Addr, binding, purpose, code, newPassword string) (identity.Session, error)
}

// passwordRequestTimeout keeps a login (Argon2 + 10 s synchronous mail) inside http.Server.WriteTimeout
// (15 s) and just above the BFF's 14 s login timeout (A6).
const passwordRequestTimeout = 14 * time.Second

// ClientIP returns the single valid X-Commerce-Client-IP (IPv4-mapped IPv6 unmapped). Missing,
// duplicate, zoned or unparsable values are an error (HTTP 400). Call it only after the BFF-key check.
func ClientIP(r *http.Request) (netip.Addr, error) {
	v := r.Header.Values("X-Commerce-Client-IP")
	if len(v) != 1 {
		return netip.Addr{}, errors.New("client ip header must appear exactly once")
	}
	ip, err := netip.ParseAddr(v[0])
	if err != nil || ip.Zone() != "" {
		return netip.Addr{}, errors.New("invalid client ip")
	}
	return ip.Unmap(), nil
}

// NewPasswordHandler serves the four password routes (§7.1).
func NewPasswordHandler(p passwordService, bffKey string) (http.Handler, error) {
	if p == nil || !ValidSecret(bffKey) {
		return nil, errors.New("invalid password transport configuration")
	}
	mux := http.NewServeMux()
	type credentials struct {
		Email    string `json:"email"`
		Password string `json:"password"`
		Locale   string `json:"locale"`
	}
	mux.HandleFunc("POST /v1/identity/password/signup", func(w http.ResponseWriter, r *http.Request) {
		var in credentials
		ip, ok := prepare(w, r, &in)
		if !ok {
			return
		}
		c, err := p.Signup(r.Context(), ip, in.Email, in.Password, in.Locale)
		challenge(w, c, err)
	})
	mux.HandleFunc("POST /v1/identity/password/login", func(w http.ResponseWriter, r *http.Request) {
		var in credentials
		ip, ok := prepare(w, r, &in)
		if !ok {
			return
		}
		c, err := p.Login(r.Context(), ip, in.Email, in.Password, in.Locale)
		challenge(w, c, err)
	})
	mux.HandleFunc("POST /v1/identity/password/reset", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Email  string `json:"email"`
			Locale string `json:"locale"`
		}
		ip, ok := prepare(w, r, &in)
		if !ok {
			return
		}
		c, err := p.Reset(r.Context(), ip, in.Email, in.Locale)
		challenge(w, c, err)
	})
	mux.HandleFunc("POST /v1/identity/password/complete", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Binding     string `json:"binding"`
			Purpose     string `json:"purpose"`
			Code        string `json:"code"`
			NewPassword string `json:"new_password"`
		}
		ip, ok := prepare(w, r, &in)
		if !ok {
			return
		}
		// A code that is not exactly six ASCII digits cannot be right: reject before any other work (§7.1).
		if !sixDigits(in.Code) {
			passwordFailure(w, identity.ErrInvalidCode)
			return
		}
		s, err := p.Complete(r.Context(), ip, in.Binding, in.Purpose, in.Code, in.NewPassword)
		if err != nil {
			passwordFailure(w, err)
			return
		}
		respond(w, map[string]any{"token": s.Token, "expires_at": s.ExpiresAt})
	})
	return httperror.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if len(r.Header.Values("X-Commerce-BFF-Key")) != 1 || subtle.ConstantTimeCompare([]byte(r.Header.Get("X-Commerce-BFF-Key")), []byte(bffKey)) != 1 {
			failure(w, identity.ErrUnauthorized)
			return
		}
		if len(r.Header.Values("Origin")) > 0 || len(r.Header.Values("Cookie")) > 0 {
			httperror.Write(w, http.StatusForbidden, "forbidden")
			return
		}
		if r.URL.RawQuery != "" {
			failure(w, identity.ErrInvalid)
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), passwordRequestTimeout)
		defer cancel()
		mux.ServeHTTP(w, r.WithContext(ctx))
	})), nil
}

// prepare reads the client ip (400 when absent/duplicate/invalid) and the strict JSON body.
func prepare[T any](w http.ResponseWriter, r *http.Request, in *T) (netip.Addr, bool) {
	ip, err := ClientIP(r)
	if err != nil {
		writePasswordError(w, http.StatusBadRequest, "invalid_client_ip", false, "")
		return netip.Addr{}, false
	}
	if !body(w, r, in) {
		return netip.Addr{}, false
	}
	return ip, true
}

func challenge(w http.ResponseWriter, c identity.Challenge, err error) {
	if err != nil {
		passwordFailure(w, err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	_ = jsonEncode(w, c)
}

func sixDigits(s string) bool {
	if len(s) != 6 {
		return false
	}
	for i := 0; i < 6; i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

// passwordFailure maps service errors to A10 (status, code). Anything else falls through to the
// shared mapping (invalid_request 422, unauthorized, ..., unavailable 503). Wrong password, unknown
// email and disabled credential are the same error before they get here (PD6).
func passwordFailure(w http.ResponseWriter, err error) {
	var pe identity.PolicyError
	var te identity.ThrottleError
	switch {
	case errors.As(err, &pe):
		writePasswordError(w, http.StatusUnprocessableEntity, "password_policy", false, pe.Reason)
	case errors.Is(err, identity.ErrInvalidEmail):
		writePasswordError(w, http.StatusUnprocessableEntity, "invalid_email", false, "")
	case errors.Is(err, identity.ErrInvalidCredentials):
		writePasswordError(w, http.StatusUnauthorized, "invalid_credentials", false, "")
	case errors.Is(err, identity.ErrInvalidCode):
		writePasswordError(w, http.StatusUnauthorized, "invalid_code", false, "")
	case errors.Is(err, identity.ErrAccountExists):
		writePasswordError(w, http.StatusConflict, "account_exists", false, "")
	case errors.As(err, &te):
		secs := int((te.RetryAfter + time.Second - 1) / time.Second)
		if secs < 1 {
			secs = 1
		}
		w.Header().Set("Retry-After", strconv.Itoa(secs))
		writePasswordError(w, http.StatusTooManyRequests, "throttled", true, "")
	case errors.Is(err, identity.ErrBusy):
		writePasswordError(w, http.StatusServiceUnavailable, "busy", true, "")
	case errors.Is(err, identity.ErrMailUnavailable):
		writePasswordError(w, http.StatusServiceUnavailable, "mail_unavailable", true, "")
	default:
		failure(w, err)
	}
}

// passwordMessages are the user-safe texts of the password-auth codes. internal/httperror rewrites
// unknown codes to "internal", so these codes use the same envelope written here (A10) instead of
// httperror.Write.
var passwordMessages = map[string]string{
	"invalid_client_ip": "Client address is missing or invalid.", "password_policy": "Password does not meet the policy.",
	"invalid_email": "Email address is not valid.", "invalid_credentials": "Email or password is incorrect.",
	"invalid_code": "The code is incorrect or expired.", "account_exists": "An account already exists for this email.",
	"throttled": "Too many attempts. Try again later.", "busy": "The service is busy. Try again shortly.",
	"mail_unavailable": "Email cannot be sent right now. Try again later.",
}

// passwordEnvelope is httperror.Envelope plus the A10 `reason` key (only for password_policy).
type passwordEnvelope struct {
	httperror.Envelope
	Reason string `json:"reason,omitempty"`
}

func writePasswordError(w http.ResponseWriter, status int, code string, retryable bool, reason string) {
	details := map[string]any{}
	if reason != "" {
		details["reason"] = reason
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = jsonEncode(w, passwordEnvelope{Envelope: httperror.Envelope{Code: code, Message: passwordMessages[code],
		RequestID: w.Header().Get("X-Request-ID"), Retryable: retryable, Details: details}, Reason: reason})
}

func jsonEncode(w http.ResponseWriter, v any) error { return json.NewEncoder(w).Encode(v) }
