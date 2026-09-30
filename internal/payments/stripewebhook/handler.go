// handler.go: POST /v1/stripe/webhook/{endpoint_id} (contracts/stripe-psp-v1.md §0.2, §9.1, brief
// stripe-b1-ingress-assembly). The order of checks below is part of the contract: cheap
// request-shape refusals first, then bounded body under a read deadline, then the admission slot
// (S2: never held while a body is read), then per-endpoint material, then signature,
// then one admission transaction. Nothing is ACKed before COMMIT.

package stripewebhook

import (
	"context"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"regexp"
	"strings"
	"time"

	"livecommerce/internal/integrations/accounts"
	"livecommerce/internal/integrations/psp/stripe"
	"livecommerce/internal/jobqueue"
)

const (
	routePrefix   = "/v1/stripe/webhook/"
	maxBody       = 256 << 10 // §9.1 body bound (read limit + 1 byte detects overflow)
	maxInFlight   = 32        // §9.1 non-blocking admission semaphore
	requestBudget = 5 * time.Second
	// maxTypeLen mirrors payments.stripe_webhook_prepare's event-type regex ({1,100}); the Go
	// projection admits 128. A longer signed type is quarantined as malformed rather than looping
	// on a SQL 22023 that Stripe would retry forever.
	maxTypeLen = 100
)

var endpointPattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

type handler struct {
	in     *Inbox
	sem    chan struct{}
	budget time.Duration // per-request deadline for body read plus lookup/verify/admit (requestBudget)
}

// NewHandler is the only way to obtain a webhook entry point; no exported method accepts an
// event that has not passed the endpoint's signature check.
func NewHandler(inbox *Inbox) (http.Handler, error) {
	// jobqueue.ForProfile is the closed profile list (PROVIDER_MOCK|SANDBOX|LIVE); LIVE is admitted (§5.2).
	if inbox == nil || inbox.store == nil || inbox.keys == nil || inbox.now == nil ||
		jobqueue.ForProfile(inbox.profile) == "" {
		return nil, ErrConfig
	}
	return &handler{in: inbox, sem: make(chan struct{}, maxInFlight), budget: requestBudget}, nil
}

func reply(w http.ResponseWriter, status int, body string) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(status)
	_, _ = io.WriteString(w, body)
}

func fail(w http.ResponseWriter, status int, code string) {
	reply(w, status, `{"error":"`+code+`"}`)
}

// logReject records only a fixed code and the endpoint UUID (§12): never body, header or secret.
func logReject(endpoint, code string) {
	slog.Warn("stripe_webhook_rejected", "code", code, "endpoint", endpoint)
}

func (h *handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		fail(w, http.StatusMethodNotAllowed, "method_not_allowed")
		return
	}
	// Exact literal path; RawPath is set when the client used a non-canonical escape (%61 etc.).
	endpoint, ok := strings.CutPrefix(r.URL.Path, routePrefix)
	if !ok || r.URL.RawPath != "" || !endpointPattern.MatchString(endpoint) {
		fail(w, http.StatusNotFound, "not_found")
		return
	}
	if r.URL.RawQuery != "" || r.URL.ForceQuery {
		fail(w, http.StatusBadRequest, "invalid_request")
		return
	}
	if !jsonContentType(r.Header.Values("Content-Type")) || !identityEncoding(r.Header.Values("Content-Encoding")) {
		fail(w, http.StatusUnsupportedMediaType, "unsupported_media_type")
		return
	}
	if r.Body == nil {
		fail(w, http.StatusBadRequest, "invalid_request")
		return
	}
	// S2: the body is read BEFORE an admission slot is taken and under its own read deadline, so an
	// unauthenticated client trickling a body (any UUID-shaped path passes the checks above) cannot
	// hold slots: the slot only covers material lookup, verification and admit. The deadline also
	// bounds the read itself (io.ReadAll ignores ctx); ErrNotSupported (test recorders) is ignored.
	_ = http.NewResponseController(w).SetReadDeadline(time.Now().Add(h.budget))
	raw, err := io.ReadAll(io.LimitReader(r.Body, maxBody+1))
	if err != nil {
		fail(w, http.StatusBadRequest, "invalid_request")
		return
	}
	if len(raw) > maxBody {
		fail(w, http.StatusRequestEntityTooLarge, "payload_too_large")
		return
	}
	select {
	case h.sem <- struct{}{}:
		defer func() { <-h.sem }()
	default:
		w.Header().Set("Retry-After", "5")
		fail(w, http.StatusServiceUnavailable, "busy")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), h.budget)
	defer cancel()

	m, found, err := h.in.store.material(ctx, endpoint)
	if err != nil {
		// Why 503: the lookup failed, not the delivery; Stripe retries with the same event.
		logReject(endpoint, "unavailable")
		fail(w, http.StatusServiceUnavailable, "unavailable")
		return
	}
	// A wrong-profile endpoint is invisible here (404) and, in SQL, cannot consume delivery
	// belonging to the correct-profile endpoint (ruling 6).
	if !found || m.Profile != h.in.profile {
		fail(w, http.StatusNotFound, "not_found")
		return
	}
	secrets, err := h.in.keys.OpenStripeWebhook(accounts.StripeWebhookScope{
		TenantID: m.TenantID, StoreID: m.StoreID, ConnectionID: m.ConnectionID, EndpointID: endpoint,
		Environment: m.Environment, AccountID: m.AccountID, Profile: m.Profile, KeyVersion: m.KeyVersion,
	}, m.KeyID, m.Nonce, m.Ciphertext)
	if err != nil {
		// Why 503: the envelope cannot be opened (missing keyring entry, AAD mismatch); Stripe
		// retries until an operator repairs custody. Never a 4xx that would make Stripe give up.
		logReject(endpoint, "signing_unavailable")
		fail(w, http.StatusServiceUnavailable, "signing_unavailable")
		return
	}
	values := []string{secrets.CurrentSecret}
	if secrets.NextSecret != "" {
		values = append(values, secrets.NextSecret)
	}
	verifier, err := stripe.NewWebhookVerifier(stripe.WebhookConfig{Secrets: values,
		AccountID: m.AccountID, Environment: m.Environment})
	if err != nil {
		logReject(endpoint, "signing_unavailable")
		fail(w, http.StatusServiceUnavailable, "signing_unavailable")
		return
	}
	signatures := r.Header.Values("Stripe-Signature")
	if len(signatures) != 1 {
		logReject(endpoint, "invalid_signature")
		fail(w, http.StatusBadRequest, "invalid_signature")
		return
	}
	ev, err := verifier.Verify(raw, signatures[0], h.in.now())
	if err != nil {
		logReject(endpoint, "invalid_signature")
		fail(w, http.StatusBadRequest, "invalid_signature")
		return
	}
	if !ev.Malformed && len(ev.Type) > maxTypeLen {
		ev = stripe.Event{Malformed: true, BodySHA256: ev.BodySHA256, SignedAt: ev.SignedAt}
	}
	if err := h.in.store.admit(ctx, endpoint, m, ev); err != nil {
		// Why 503: DB error, stale key_version (rotation raced us) or cancel. No receipt was
		// committed, so Stripe's retry is safe and is deduplicated per (endpoint,event).
		logReject(endpoint, "unavailable")
		fail(w, http.StatusServiceUnavailable, "unavailable")
		return
	}
	reply(w, http.StatusOK, `{"received":true}`)
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
		return present && strings.EqualFold(charset, "utf-8")
	}
	return true
}

func identityEncoding(values []string) bool {
	return len(values) == 0 || (len(values) == 1 && strings.EqualFold(strings.TrimSpace(values[0]), "identity"))
}
