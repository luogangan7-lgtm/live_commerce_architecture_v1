// client.go: the HTTP client, response classification and the four Checkout Session
// calls plus VerifyAccount (contracts/stripe-psp-v1.md §5.1, §5.5, §5.6).
// It never retries, never mints a second create key and never infers "unpaid" from
// an error: retry and closure are durable-orchestration decisions (§10).
//
// Ownership: integration_worker. Dependencies: net/http, crypto/tls (TLS ≥1.2 with
// verification); no stripe-go. Callers: the payment worker, cmd/stripe-admin
// (VerifyAccount), and tests. NewWithMockTransport is for _test.go files and the
// PROVIDER_MOCK assembly only (SP20 source guard).

package stripe

import (
	"bytes"
	"context"
	"crypto/tls"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"time"
)

const (
	// baseURL is fixed; there is no caller-supplied host, redirect or client (§5.1).
	baseURL         = "https://api.stripe.com"
	userAgent       = "livecommerce-stripe/1"
	callTimeout     = 10 * time.Second // §5.1 CallTimeout; the caller's context may be shorter
	maxResponseBody = 256 << 10        // §5.1 response body bound
	maxListPages    = 10               // §5.5: more pages is ErrUncertain, never "not found"
	listPageLimit   = 100              // F10: limit 1..100
)

// Client performs the Stripe calls for exactly one account and environment.
type Client struct {
	cfg        Config
	mock       bool
	httpClient *http.Client
}

func (Client) String() string               { return "stripe.Client{redacted}" }
func (c Client) GoString() string           { return c.String() }
func (Client) MarshalJSON() ([]byte, error) { return []byte(`"stripe.Client{redacted}"`), nil }

// New admits cfg under §5.2 and returns a client for https://api.stripe.com with
// TLS ≥1.2, certificate verification, no cookie jar and no redirect following.
// Errors are ErrInvalid or ErrLiveRefused wrapped with a fixed code.
func New(cfg Config) (*Client, error) {
	if err := admit(cfg, false); err != nil {
		return nil, err
	}
	transport := &http.Transport{
		Proxy:               http.ProxyFromEnvironment,
		TLSClientConfig:     &tls.Config{MinVersion: tls.VersionTLS12},
		ForceAttemptHTTP2:   true,
		TLSHandshakeTimeout: callTimeout,
		MaxIdleConnsPerHost: 4,
	}
	return newClient(cfg, false, transport), nil
}

// NewWithMockTransport is the PROVIDER_MOCK / test constructor: every request goes to
// rt instead of the network. It admits only sk_test_/rk_test_ keys in SANDBOX.
func NewWithMockTransport(cfg Config, rt http.RoundTripper) (*Client, error) {
	if rt == nil {
		return nil, ErrInvalid
	}
	if err := admit(cfg, true); err != nil {
		return nil, err
	}
	return newClient(cfg, true, rt), nil
}

func newClient(cfg Config, mock bool, rt http.RoundTripper) *Client {
	return &Client{cfg: cfg, mock: mock, httpClient: &http.Client{
		Timeout:   callTimeout,
		Transport: rt,
		Jar:       nil, // no cookies
		// A 3xx is returned as-is and classified ErrUncertain; redirects are never followed.
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}}
}

type operation int

const (
	opAccount operation = iota
	opCreate
	opRetrieve
	opExpire
	opList
)

// call sends one request and returns the bounded body plus CallMeta. The error is
// nil only for HTTP 200; every other outcome is classified per §5.6.
func (c *Client) call(ctx context.Context, op operation, method, path string, body []byte, idemKey string) ([]byte, CallMeta, error) {
	var meta CallMeta
	if ctx == nil {
		return nil, meta, ErrInvalid
	}
	var reader io.Reader
	if method == http.MethodPost {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, baseURL+path, reader)
	if err != nil {
		return nil, meta, ErrInvalid
	}
	req.Header.Set("Authorization", "Bearer "+c.cfg.SecretKey)
	req.Header.Set("Stripe-Version", APIVersion)
	req.Header.Set("User-Agent", userAgent)
	if method == http.MethodPost {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	if idemKey != "" {
		req.Header.Set("Idempotency-Key", idemKey)
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		// Network error, timeout, cancel or bad TLS: the request may have executed.
		// The caller retries the SAME key later (a Stripe replay returns the saved result).
		return nil, meta, ErrUncertain
	}
	defer resp.Body.Close()
	raw, readErr := io.ReadAll(io.LimitReader(resp.Body, maxResponseBody+1))
	meta = responseMeta(resp, raw)
	if readErr != nil || len(raw) > maxResponseBody {
		return nil, meta, ErrUncertain
	}
	if err := classify(op, resp.StatusCode, meta); err != nil {
		return nil, meta, err
	}
	return raw, meta, nil
}

// responseMeta extracts only bounded, non-secret evidence. The Stripe error message
// is never read into memory beyond the strict decode and never returned.
func responseMeta(resp *http.Response, raw []byte) CallMeta {
	meta := CallMeta{HTTPStatus: resp.StatusCode}
	if id := resp.Header.Get("Request-Id"); requestIDPattern.MatchString(id) {
		meta.RequestID = id
	}
	// F4: Idempotent-Replayed marks a replay; Stripe-Should-Retry is a hint only.
	meta.IdempotentReplayed = resp.Header.Get("Idempotent-Replayed") == "true"
	switch resp.Header.Get("Stripe-Should-Retry") {
	case "true":
		t := true
		meta.ShouldRetry = &t
	case "false":
		f := false
		meta.ShouldRetry = &f
	}
	if resp.StatusCode >= 400 && len(raw) <= maxResponseBody {
		if root, err := decodeStrict(raw); err == nil {
			if obj, ok := root.(map[string]any); ok {
				if e, present, ok := jsonObject(obj).object("error"); ok && present {
					typ, _, _ := e.str("type")
					code, _, _ := e.str("code")
					if errorWordPattern.MatchString(typ) {
						meta.ErrorType = typ
					}
					if errorWordPattern.MatchString(code) {
						meta.ErrorCode = code
					}
				}
			}
		}
	}
	return meta
}

// classify implements the §5.6 table. Error handling: https://docs.stripe.com/error-low-level
// and https://docs.stripe.com/api/idempotent_requests (retrieved 2026-09-28, F4).
func classify(op operation, status int, meta CallMeta) error {
	switch {
	case status == http.StatusOK:
		return nil
	case status == http.StatusBadRequest && meta.ErrorType == "idempotency_error":
		// Frozen params changed under one key: a bug. Never a closure.
		return ErrIdempotency
	case status == http.StatusBadRequest && meta.ErrorType == "invalid_request_error":
		switch op {
		case opCreate:
			// Definitive only on the first send; the caller applies that rule (§10).
			return ErrRejected
		case opExpire:
			// F2: not in an expireable state; the caller always follows with a retrieve.
			return ErrNotOpen
		}
		return ErrUncertain
	case status == http.StatusNotFound && op == opCreate:
		return ErrRejected
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		// F4: auth runs before the idempotency layer; nothing executed on this send.
		return ErrAuthentication
	case status == http.StatusConflict:
		// F4: concurrent request with the same key; retry the SAME key later.
		return ErrConflict
	case status == http.StatusTooManyRequests:
		// F4: 429 runs before idempotency; retry the SAME key with backoff.
		return ErrRateLimited
	}
	// 5xx (including a replayed, cached 500), 3xx, retrieve 404 and anything else:
	// indeterminate. Never switch keys (F4); resolve by same-key retry, then list (§10).
	return ErrUncertain
}

func (c *Client) wantLivemode() bool { return c.cfg.Environment == envLive }

func (c *Client) decodeSession(raw []byte) (Session, error) {
	root, err := decodeStrict(raw)
	if err != nil {
		return Session{}, ErrUncertain
	}
	obj, ok := root.(map[string]any)
	if !ok {
		return Session{}, ErrUncertain
	}
	return parseSession(jsonObject(obj), c.wantLivemode())
}

// VerifyAccount calls GET /v1/account and requires its id to equal Config.AccountID.
// A different account is ErrAuthentication (the key belongs elsewhere).
// https://docs.stripe.com/api/accounts/retrieve (retrieved 2026-09-28).
func (c *Client) VerifyAccount(ctx context.Context) (CallMeta, error) {
	raw, meta, err := c.call(ctx, opAccount, http.MethodGet, "/v1/account", nil, "")
	if err != nil {
		return meta, err
	}
	root, err := decodeStrict(raw)
	if err != nil {
		return meta, ErrUncertain
	}
	obj, ok := root.(map[string]any)
	if !ok {
		return meta, ErrUncertain
	}
	id, _, okID := jsonObject(obj).str("id")
	kind, _, okKind := jsonObject(obj).str("object")
	if !okID || !okKind || kind != "account" || !accountPattern.MatchString(id) {
		return meta, ErrUncertain
	}
	if id != c.cfg.AccountID {
		return meta, refuse(ErrAuthentication, "stripe_account_mismatch")
	}
	return meta, nil
}

// profileAllowed binds metadata[lc_profile] to the client: LIVE only on a LIVE client,
// SANDBOX on the real sandbox, and PROVIDER_MOCK (or SANDBOX fixtures) on the mock transport.
func (c *Client) profileAllowed(profile string) bool {
	switch {
	case c.cfg.Environment == envLive:
		return profile == "LIVE"
	case c.mock:
		return profile == "PROVIDER_MOCK" || profile == "SANDBOX"
	}
	return profile == "SANDBOX"
}

// CreateCheckoutSession posts the frozen §5.4 body with
// Idempotency-Key = CreateIdempotencyKey(attempt). Every send of one attempt posts
// byte-identical bytes under the same key. On 200 the session must validate and
// carry client_reference_id and metadata.lc_attempt equal to the attempt, else
// ErrUncertain. https://docs.stripe.com/api/checkout/sessions/create (retrieved 2026-09-28).
func (c *Client) CreateCheckoutSession(ctx context.Context, p CreateParams) (Session, CallMeta, error) {
	body, err := EncodeCreateBody(p)
	if err != nil {
		return Session{}, CallMeta{}, err
	}
	if !c.profileAllowed(p.Fields[pMetaProfile]) {
		return Session{}, CallMeta{}, ErrInvalid
	}
	attempt := p.Fields[pMetaAttempt]
	raw, meta, err := c.call(ctx, opCreate, http.MethodPost, "/v1/checkout/sessions", body, CreateIdempotencyKey(attempt))
	if err != nil {
		return Session{}, meta, err
	}
	s, err := c.decodeSession(raw)
	if err != nil || s.ClientReferenceID != attempt || s.MetadataAttempt != attempt {
		return Session{}, meta, ErrUncertain
	}
	return s, meta, nil
}

// RetrieveCheckoutSession calls GET /v1/checkout/sessions/{id}?expand[]=payment_intent.
// A 404 is ErrUncertain: absence is never inferred for a pinned session (§10).
// https://docs.stripe.com/api/checkout/sessions/retrieve (retrieved 2026-09-28).
func (c *Client) RetrieveCheckoutSession(ctx context.Context, sessionID string) (Session, CallMeta, error) {
	if !stripeIDPattern.MatchString(sessionID) {
		return Session{}, CallMeta{}, ErrInvalid
	}
	raw, meta, err := c.call(ctx, opRetrieve, http.MethodGet,
		"/v1/checkout/sessions/"+sessionID+"?expand%5B%5D=payment_intent", nil, "")
	if err != nil {
		return Session{}, meta, err
	}
	s, err := c.decodeSession(raw)
	if err != nil || s.ID != sessionID {
		return Session{}, meta, ErrUncertain
	}
	return s, meta, nil
}

// ExpireCheckoutSession posts an empty body to /v1/checkout/sessions/{id}/expire with
// idempotencyKey (which must be an ExpireIdempotencyKey value). A 400
// invalid_request_error is ErrNotOpen; callers always follow with a retrieve.
// https://docs.stripe.com/api/checkout/sessions/expire (retrieved 2026-09-28, F2).
func (c *Client) ExpireCheckoutSession(ctx context.Context, sessionID, idempotencyKey string) (Session, CallMeta, error) {
	if !stripeIDPattern.MatchString(sessionID) || !expireKeyGen.MatchString(idempotencyKey) {
		return Session{}, CallMeta{}, ErrInvalid
	}
	raw, meta, err := c.call(ctx, opExpire, http.MethodPost, "/v1/checkout/sessions/"+sessionID+"/expire", nil, idempotencyKey)
	if err != nil {
		return Session{}, meta, err
	}
	s, err := c.decodeSession(raw)
	if err != nil || s.ID != sessionID {
		return Session{}, meta, ErrUncertain
	}
	return s, meta, nil
}

// FindCheckoutSessions lists sessions created in [createdFrom, createdTo] (unix
// seconds) and returns those whose client_reference_id and metadata.lc_attempt both
// equal attemptID. It reads at most 10 pages of 100; more is ErrUncertain, never
// "not found". The returned CallMeta is the last page's.
// https://docs.stripe.com/api/checkout/sessions/list (retrieved 2026-09-28, F10).
func (c *Client) FindCheckoutSessions(ctx context.Context, attemptID string, createdFrom, createdTo int64) ([]Session, CallMeta, error) {
	if !uuidPattern.MatchString(attemptID) || createdFrom <= 0 || createdTo < createdFrom || createdTo > maxUnixSeconds {
		return nil, CallMeta{}, ErrInvalid
	}
	matches := []Session{}
	var meta CallMeta
	after := ""
	for page := 0; page < maxListPages; page++ {
		q := "created%5Bgte%5D=" + strconv.FormatInt(createdFrom, 10) +
			"&created%5Blte%5D=" + strconv.FormatInt(createdTo, 10) +
			"&limit=" + strconv.Itoa(listPageLimit)
		if after != "" {
			q += "&starting_after=" + url.QueryEscape(after)
		}
		raw, m, err := c.call(ctx, opList, http.MethodGet, "/v1/checkout/sessions?"+q, nil, "")
		meta = m
		if err != nil {
			return nil, meta, err
		}
		sessions, hasMore, err := c.decodeList(raw)
		if err != nil {
			return nil, meta, ErrUncertain
		}
		for _, s := range sessions {
			if s.ClientReferenceID == attemptID && s.MetadataAttempt == attemptID {
				matches = append(matches, s)
			}
		}
		if !hasMore {
			return matches, meta, nil
		}
		if len(sessions) == 0 {
			return nil, meta, ErrUncertain // has_more with an empty page cannot advance
		}
		after = sessions[len(sessions)-1].ID
	}
	return nil, meta, ErrUncertain
}

func (c *Client) decodeList(raw []byte) ([]Session, bool, error) {
	root, err := decodeStrict(raw)
	if err != nil {
		return nil, false, ErrUncertain
	}
	obj, ok := root.(map[string]any)
	if !ok {
		return nil, false, ErrUncertain
	}
	kind, _, okKind := jsonObject(obj).str("object")
	hasMore, hasMorePresent, okMore := jsonObject(obj).boolean("has_more")
	data, okData := obj["data"].([]any)
	if !okKind || kind != "list" || !okMore || !hasMorePresent || !okData || len(data) > listPageLimit {
		return nil, false, ErrUncertain
	}
	out := make([]Session, 0, len(data))
	for _, item := range data {
		m, ok := item.(map[string]any)
		if !ok {
			return nil, false, ErrUncertain
		}
		s, err := parseSession(jsonObject(m), c.wantLivemode())
		if err != nil {
			return nil, false, ErrUncertain
		}
		out = append(out, s)
	}
	return out, hasMore, nil
}
