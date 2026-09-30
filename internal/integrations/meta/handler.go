package meta

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"errors"
	"io"
	"mime"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const (
	// maxInFlight bounds concurrent verify+commit sections per webhook route (S3), the same
	// non-blocking pattern as the Stripe webhook (stripe-psp-v1 §9.1).
	maxInFlight = 32
	// readBudget is the per-request deadline for reading the body (io.ReadAll ignores ctx).
	readBudget = 5 * time.Second
)

// NewHandler only acknowledges after commit returns. A production commit must
// atomically persist every receipt, job and quarantine record before nil.
func NewHandler(v *Verifier, commit func(context.Context, Batch) error) (http.Handler, error) {
	if commit == nil {
		return nil, ErrConfig
	}
	return newRawHandler(v, func(ctx context.Context, batch Batch, _ []byte) error {
		return commit(ctx, batch)
	})
}

// newRawHandler keeps authenticated bytes inside the receiving package. Durable
// admission must encrypt this exact owned body: canonical events cannot recover
// original whitespace/escapes or a mixed-asset envelope. The callback is synchronous
// and receives raw only after every protocol check; it must not log/queue plaintext.
func newRawHandler(v *Verifier, commit func(context.Context, Batch, []byte) error) (http.Handler, error) {
	return newLimitedRawHandler(v, commit, maxInFlight, readBudget)
}

// newLimitedRawHandler is newRawHandler with explicit admission bounds (tests use tiny ones).
func newLimitedRawHandler(v *Verifier, commit func(context.Context, Batch, []byte) error, inflight int, budget time.Duration) (http.Handler, error) {
	if !v.valid() || commit == nil || inflight < 1 || budget <= 0 {
		return nil, ErrConfig
	}
	sem := make(chan struct{}, inflight)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		switch r.Method {
		case http.MethodGet:
			challenge(w, r, v)
		case http.MethodPost:
			receive(w, r, v, commit, sem, budget)
		default:
			w.Header().Set("Allow", "GET, POST")
			writeCode(w, http.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED")
		}
	}), nil
}

func writeCode(w http.ResponseWriter, status int, code string) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(status)
	_, _ = io.WriteString(w, code)
}

func challenge(w http.ResponseWriter, r *http.Request, v *Verifier) {
	q, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil || len(q) != challengeParams(q) || len(q["hub.mode"]) != 1 || len(q["hub.verify_token"]) != 1 || len(q["hub.challenge"]) != 1 || q.Get("hub.mode") != "subscribe" || !validChallenge(q.Get("hub.challenge")) {
		writeCode(w, http.StatusBadRequest, "BAD_CHALLENGE")
		return
	}
	got, want := sha256.Sum256([]byte(q.Get("hub.verify_token"))), sha256.Sum256([]byte(v.verifyToken))
	if subtle.ConstantTimeCompare(got[:], want[:]) != 1 {
		writeCode(w, http.StatusForbidden, "BAD_VERIFY_TOKEN")
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = io.WriteString(w, q.Get("hub.challenge"))
}

// challengeParams returns how many query keys a verification GET may carry: the three dotted
// hub.* keys, plus each underscore twin (hub_mode, hub_verify_token, hub_challenge) that Meta also
// sends (observed from facebookplatform/1.0 on 2026-09-30). A twin is accepted only once and only
// when it equals its dotted key; otherwise -1, so the length check fails and the request is 400.
func challengeParams(q url.Values) int {
	n := 3
	for _, k := range [...]string{"mode", "verify_token", "challenge"} {
		twin, ok := q["hub_"+k]
		if !ok {
			continue
		}
		if len(twin) != 1 || len(q["hub."+k]) != 1 || twin[0] != q["hub."+k][0] {
			return -1
		}
		n++
	}
	return n
}

func validChallenge(s string) bool {
	if len(s) < 1 || len(s) > 200 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !((c >= '0' && c <= '9') || (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') || c == '.' || c == '_' || c == '-') {
			return false
		}
	}
	return true
}

func receive(w http.ResponseWriter, r *http.Request, v *Verifier, commit func(context.Context, Batch, []byte) error, sem chan struct{}, budget time.Duration) {
	if r.URL.RawQuery != "" || r.URL.ForceQuery {
		writeCode(w, http.StatusBadRequest, "BAD_QUERY")
		return
	}
	if !jsonContentType(r.Header.Values("Content-Type")) || !identityEncoding(r.Header.Values("Content-Encoding")) {
		writeCode(w, http.StatusUnsupportedMediaType, "UNSUPPORTED_MEDIA_TYPE")
		return
	}
	signatures := r.Header.Values("X-Hub-Signature-256")
	if len(signatures) != 1 {
		writeCode(w, http.StatusForbidden, "BAD_SIGNATURE")
		return
	}
	if r.Body == nil {
		writeCode(w, http.StatusBadRequest, "BAD_BODY")
		return
	}
	// S3: the body is read under its own deadline and BEFORE a slot is taken, so a client that
	// trickles a body cannot hold slots (the Stripe S2 lesson); the slot covers HMAC verify and the
	// synchronous commit only. Limit: heap during the read is bounded by the connection count and
	// this deadline, not by the semaphore (ponytail: add a MaxBytesReader-style global budget if
	// ingress connection limits at the edge prove insufficient).
	_ = http.NewResponseController(w).SetReadDeadline(time.Now().Add(budget))
	raw, err := io.ReadAll(io.LimitReader(r.Body, maxBody+1))
	if err != nil {
		writeCode(w, http.StatusBadRequest, "BAD_BODY")
		return
	}
	if len(raw) > maxBody {
		writeCode(w, http.StatusRequestEntityTooLarge, "BODY_TOO_LARGE")
		return
	}
	select {
	case sem <- struct{}{}:
		defer func() { <-sem }()
	default:
		w.Header().Set("Retry-After", "5")
		writeCode(w, http.StatusServiceUnavailable, "BUSY")
		return
	}
	batch, err := v.Verify(raw, signatures[0])
	if err != nil {
		switch {
		case errors.Is(err, ErrSignature):
			writeCode(w, http.StatusForbidden, "BAD_SIGNATURE")
		case errors.Is(err, ErrTooLarge):
			writeCode(w, http.StatusRequestEntityTooLarge, "BATCH_TOO_LARGE")
		default:
			writeCode(w, http.StatusBadRequest, "BAD_JSON")
		}
		return
	}
	if r.Context().Err() != nil || commit(r.Context(), batch, raw) != nil || r.Context().Err() != nil {
		writeCode(w, http.StatusServiceUnavailable, "COMMIT_UNAVAILABLE")
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = io.WriteString(w, "EVENT_RECEIVED")
}

func jsonContentType(values []string) bool {
	if len(values) != 1 {
		return false
	}
	media, params, err := mime.ParseMediaType(values[0])
	if err != nil || media != "application/json" || len(params) > 1 {
		return false
	}
	if len(params) == 1 {
		charset, present := params["charset"]
		if !present || !strings.EqualFold(charset, "utf-8") {
			return false
		}
	}
	return true
}

func identityEncoding(values []string) bool {
	return len(values) == 0 || (len(values) == 1 && strings.EqualFold(strings.TrimSpace(values[0]), "identity"))
}
