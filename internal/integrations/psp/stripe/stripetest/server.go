// Package stripetest owns the independent MOCK Stripe HTTP service: Checkout sessions
// (server.go) and refunds / PaymentIntent+charge reads (refunds.go).
// It never contacts Stripe, decides payment state in PG, or stores real card data.
// Written from https://docs.stripe.com/api/refunds and /api/payment_intents/retrieve
// (retrieved 2026-09-29) plus contracts/stripe-refund-v1.md §1/§3, never from the adapter.
package stripetest

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Fault is consumed by the next create request. RateLimit, Conflict, Validation
// and IdempotencyError run before execution; Cached500 and DropAfterExecute run
// after the cache write. IdempotencyError mimics Stripe's 400 idempotency_error
// (https://docs.stripe.com/error-low-level#idempotency, retrieved 2026-09-29).
type Fault struct {
	RateLimit, Conflict, Validation, IdempotencyError, Cached500, DropAfterExecute bool
	// Cached500NoSession caches a 500 for the key but leaves no session behind: an
	// execution that failed before creating anything (list must then find no match).
	Cached500NoSession bool
	Delay              time.Duration
}

type cached struct {
	params string
	status int
	body   []byte
}

type session struct {
	ID                 string            `json:"id"`
	Object             string            `json:"object"`
	Status             string            `json:"status"`
	PaymentStatus      string            `json:"payment_status"`
	Livemode           bool              `json:"livemode"`
	Currency           string            `json:"currency"`
	AmountTotal        int64             `json:"amount_total"`
	AmountSubtotal     int64             `json:"amount_subtotal"`
	TotalDetails       map[string]int64  `json:"total_details"`
	ClientReferenceID  string            `json:"client_reference_id"`
	Metadata           map[string]string `json:"metadata"`
	ExpiresAt          int64             `json:"expires_at"`
	Created            int64             `json:"created"`
	Mode               string            `json:"mode"`
	PaymentMethodTypes []string          `json:"payment_method_types"`
	PaymentIntent      *paymentIntent    `json:"payment_intent"`
	URL                string            `json:"url,omitempty"`
	owner              string            // account whose key created it; never serialized
	overlay            map[string]any    // Patch: provider-side anomalies merged into every later read
}

type paymentIntent struct {
	ID             string `json:"id"`
	Object         string `json:"object"`
	Status         string `json:"status"`
	AmountReceived int64  `json:"amount_received"`
	Currency       string `json:"currency"`
}

// Server serves only local HTTP and rewrites api.stripe.com requests to itself.
// Each instance has an independent idempotency cache and session collection.
type Server struct {
	mu               sync.Mutex
	http             *httptest.Server
	account          string
	next             int
	sessions         map[string]*session
	cache            map[string]cached
	inflight         map[string]bool
	createKeys       []string
	fault            Fault
	requireKey       bool
	keys             map[[32]byte]string // API key digest -> account (AddAccount / RequireAPIKey)
	failures         map[string][]int    // op -> queued HTTP statuses (FailNext)
	requests         []Request
	accepted, denied int
	rf               *refundState // refunds.go: PaymentIntents, charges, refunds (lazy)
}

// Request is one audited call. It deliberately carries no body, key or header
// other than the Idempotency-Key, so tests can prove key/scope behavior without
// the fake becoming a secret sink.
// KeyFingerprint (first 4 bytes of SHA-256, hex) identifies which credential made a
// call without the fake ever logging the key; tests compute it with KeyFingerprint.
// RawQuery is the URL query of the call (e.g. "expand[]=latest_charge"); it never holds secrets.
type Request struct{ Method, Path, Account, IdempotencyKey, KeyFingerprint, RawQuery string }

// KeyFingerprint is the non-secret label recorded in Request.KeyFingerprint.
func KeyFingerprint(key string) string {
	d := sha256.Sum256([]byte(key))
	return hex.EncodeToString(d[:4])
}

// RevokeKey makes one API key fail with 401 from now on (a rotated-out credential).
func (s *Server) RevokeKey(key string) {
	s.mu.Lock()
	delete(s.keys, sha256.Sum256([]byte(key)))
	s.mu.Unlock()
}

// New starts a local-only Stripe fake. The account ID is deliberately synthetic.
func New(accountID string) *Server {
	s := &Server{account: accountID, sessions: make(map[string]*session), cache: make(map[string]cached), inflight: make(map[string]bool),
		keys: make(map[[32]byte]string), failures: make(map[string][]int)}
	s.http = httptest.NewServer(http.HandlerFunc(s.serveHTTP))
	return s
}

func (s *Server) Close()                { s.http.Close() }
func (s *Server) URL() string           { return s.http.URL }
func (s *Server) Handler() http.Handler { return http.HandlerFunc(s.serveHTTP) }

// Transport intercepts the documented Stripe host and never forwards any
// other request. Tests may pass it to stripe.NewWithMockTransport.
func (s *Server) Transport() http.RoundTripper {
	return &transport{server: s.http.URL, client: s.http.Client()}
}

type transport struct {
	server string
	client *http.Client
}

func (t *transport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.URL.Scheme != "https" || req.URL.Host != "api.stripe.com" {
		return nil, fmt.Errorf("stripetest: refused non-Stripe destination")
	}
	copy := req.Clone(req.Context())
	copy.URL.Scheme = "http"
	copy.URL.Host = strings.TrimPrefix(t.server, "http://")
	copy.Host = copy.URL.Host
	return t.client.Transport.RoundTrip(copy)
}

func (s *Server) SetNextFault(f Fault) { s.mu.Lock(); s.fault = f; s.mu.Unlock() }

// FaultPending reports whether a SetNextFault fault has not been consumed yet, so a
// test never overwrites the one-shot slot before the request it was meant for.
func (s *Server) FaultPending() bool { s.mu.Lock(); defer s.mu.Unlock(); return s.fault != (Fault{}) }

// ClearFailures drops every queued FailNext status (scenario hygiene).
func (s *Server) ClearFailures() {
	s.mu.Lock()
	s.failures = make(map[string][]int)
	s.mu.Unlock()
}

// RequireAPIKey enables exact Bearer admission for all fake endpoints for the
// constructor's account. Only a digest is retained; neither request counts nor
// failures expose the key. https://docs.stripe.com/api/authentication
// (retrieved 2026-09-28).
func (s *Server) RequireAPIKey(key string) error { return s.AddAccount(s.account, key) }

// AddAccount registers one more account behind its own key. Sessions created
// with a key are visible only to that key's account, so a cross-account key
// swap fails with 401 (wrong key) or 404 (foreign session), never a leak.
func (s *Server) AddAccount(accountID, key string) error {
	if key == "" || accountID == "" || strings.ContainsAny(key, " \t\r\n") {
		return fmt.Errorf("stripetest: invalid API key fixture")
	}
	s.mu.Lock()
	s.keys[sha256.Sum256([]byte(key))] = accountID
	s.requireKey = true
	s.mu.Unlock()
	return nil
}

// FailNext queues one HTTP status for the next call of op ("retrieve",
// "expire", "list" or "account"); a 5xx models a Stripe outage without executing.
func (s *Server) FailNext(op string, status int) {
	s.mu.Lock()
	s.failures[op] = append(s.failures[op], status)
	s.mu.Unlock()
}

func (s *Server) failed(op string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	q := s.failures[op]
	if len(q) == 0 {
		return 0
	}
	s.failures[op] = q[1:]
	return q[0]
}

// Requests returns the ordered call log (method, path, account, create/expire key).
func (s *Server) Requests() []Request {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Request(nil), s.requests...)
}

type RequestCounts struct{ Accepted, Denied int }

func (s *Server) Counts() RequestCounts {
	s.mu.Lock()
	defer s.mu.Unlock()
	return RequestCounts{Accepted: s.accepted, Denied: s.denied}
}
func (s *Server) CreateKeys() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.createKeys...)
}
func (s *Server) SessionIDs() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	ids := make([]string, 0, len(s.sessions))
	for id := range s.sessions {
		ids = append(ids, id)
	}
	return ids
}

// SetState controls provider observations without relying on webhook delivery.
// It also permits expired+paid and complete+unpaid anomaly fixtures.
func (s *Server) SetState(id, status, paymentStatus string) bool {
	if status != "open" && status != "complete" && status != "expired" {
		return false
	}
	if paymentStatus != "paid" && paymentStatus != "unpaid" && paymentStatus != "no_payment_required" {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	v := s.sessions[id]
	if v == nil {
		return false
	}
	v.Status, v.PaymentStatus = status, paymentStatus
	if status != "open" {
		v.URL = ""
	}
	if paymentStatus == "paid" {
		v.PaymentIntent = &paymentIntent{ID: "pi_test_" + id, Object: "payment_intent", Status: "succeeded", AmountReceived: v.AmountTotal, Currency: v.Currency}
		s.registerPaymentIntentLocked(v.PaymentIntent.ID, v.owner, v.AmountTotal, v.Currency, id)
	}
	return true
}

// SignWebhook uses Stripe's t.raw-body HMAC-SHA256 v1 wire format.
// https://docs.stripe.com/webhooks/signature (retrieved 2026-09-28).
func SignWebhook(secret string, body []byte, at time.Time) string {
	t := strconv.FormatInt(at.Unix(), 10)
	h := hmac.New(sha256.New, []byte(secret))
	h.Write([]byte(t + "."))
	h.Write(body)
	return "t=" + t + ",v1=" + hex.EncodeToString(h.Sum(nil))
}

// EventOpts describes one webhook payload for EventBody. Zero values give a
// well-formed, livemode=false checkout.session.completed event.
type EventOpts struct {
	ID, Type, ObjectType, SessionID, ClientRef, Attempt string
	Created                                             int64
	Livemode, Connect, Probe                            bool
	PendingWebhooks                                     int
	Extra                                               map[string]any // merged into data.object (e.g. customer_details PII sentinels)
}

// EventBody renders the event JSON that Stripe signs (event envelope + the
// projected checkout.session fields, https://docs.stripe.com/api/events/object
// retrieved 2026-09-29). Tests sign it with SignWebhook.
func EventBody(o EventOpts) []byte {
	if o.Type == "" {
		o.Type = "checkout.session.completed"
	}
	if o.ObjectType == "" {
		o.ObjectType = "checkout.session"
	}
	if o.Created == 0 {
		o.Created = time.Now().Unix()
	}
	md := map[string]any{"lc_attempt": o.Attempt}
	if o.Probe {
		md["lc_probe"] = "1"
	}
	obj := map[string]any{"id": o.SessionID, "object": o.ObjectType, "client_reference_id": o.ClientRef, "metadata": md}
	for k, v := range o.Extra {
		obj[k] = v
	}
	ev := map[string]any{"id": o.ID, "object": "event", "api_version": "2026-08-26.dahlia", "created": o.Created,
		"livemode": o.Livemode, "pending_webhooks": o.PendingWebhooks, "type": o.Type,
		"data": map[string]any{"object": obj}}
	if o.Connect {
		ev["account"] = "acct_ConnectedElsewhere1"
	}
	out, _ := json.Marshal(ev)
	return out
}

func (s *Server) serveHTTP(w http.ResponseWriter, r *http.Request) {
	account, ok := s.authorized(r)
	if !ok {
		errorJSON(w, http.StatusUnauthorized, "authentication_error", "invalid_api_key")
		return
	}
	s.mu.Lock()
	fp := ""
	if token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer "); ok {
		fp = KeyFingerprint(token)
	}
	s.requests = append(s.requests, Request{Method: r.Method, Path: r.URL.Path, Account: account, IdempotencyKey: r.Header.Get("Idempotency-Key"), KeyFingerprint: fp, RawQuery: r.URL.RawQuery})
	s.mu.Unlock()
	if r.URL.Path == "/v1/account" && r.Method == http.MethodGet {
		if st := s.failed("account"); st != 0 {
			errorJSON(w, st, "api_error", "api_error")
			return
		}
		writeJSON(w, 200, map[string]any{"id": account, "object": "account", "livemode": false})
		return
	}
	if s.serveRefunds(w, r, account) {
		return
	}
	base := "/v1/checkout/sessions"
	if r.URL.Path == base && r.Method == http.MethodPost {
		s.create(w, r, account)
		return
	}
	if r.URL.Path == base && r.Method == http.MethodGet {
		if st := s.failed("list"); st != 0 {
			errorJSON(w, st, "api_error", "api_error")
			return
		}
		s.list(w, r, account)
		return
	}
	if strings.HasPrefix(r.URL.Path, base+"/") {
		id := strings.TrimPrefix(r.URL.Path, base+"/")
		if strings.HasSuffix(id, "/expire") && r.Method == http.MethodPost {
			if st := s.failed("expire"); st != 0 {
				errorJSON(w, st, "api_error", "api_error")
				return
			}
			s.expire(w, strings.TrimSuffix(id, "/expire"), account)
			return
		}
		if r.Method == http.MethodGet {
			if st := s.failed("retrieve"); st != 0 {
				errorJSON(w, st, "api_error", "api_error")
				return
			}
			s.retrieve(w, id, account)
			return
		}
	}
	errorJSON(w, 404, "invalid_request_error", "resource_missing")
}

func (s *Server) authorized(r *http.Request) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.requireKey {
		s.accepted++
		return s.account, true
	}
	values := r.Header.Values("Authorization")
	if len(values) != 1 {
		s.denied++
		return "", false
	}
	token, ok := strings.CutPrefix(values[0], "Bearer ")
	if !ok || token == "" || strings.ContainsAny(token, " \t\r\n") {
		s.denied++
		return "", false
	}
	account, known := s.keys[sha256.Sum256([]byte(token))]
	if !known {
		s.denied++
		return "", false
	}
	s.accepted++
	return account, true
}

func (s *Server) create(w http.ResponseWriter, r *http.Request, account string) {
	raw := r.Header.Get("Idempotency-Key")
	key := account + "|" + raw // Stripe scopes idempotency keys per account
	if raw == "" || len(raw) > 255 {
		errorJSON(w, 400, "invalid_request_error", "parameter_invalid_empty")
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 8192))
	if err != nil {
		errorJSON(w, 400, "invalid_request_error", "invalid_request")
		return
	}
	vals, err := url.ParseQuery(string(body))
	if err != nil {
		errorJSON(w, 400, "invalid_request_error", "invalid_request")
		return
	}
	params := vals.Encode() // Stripe compares parameters, not byte formatting.
	s.mu.Lock()
	s.createKeys = append(s.createKeys, raw)
	if prior, ok := s.cache[key]; ok {
		s.mu.Unlock()
		if prior.params != params {
			errorJSON(w, 400, "idempotency_error", "idempotency_key_in_use")
			return
		}
		w.Header().Set("Idempotent-Replayed", "true")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(prior.status)
		_, _ = w.Write(prior.body)
		return
	}
	if s.inflight[key] {
		s.mu.Unlock()
		errorJSON(w, 409, "invalid_request_error", "lock_timeout")
		return
	}
	f := s.fault
	s.fault = Fault{}
	if f.RateLimit || f.Conflict || f.Validation || f.IdempotencyError {
		s.mu.Unlock()
		switch {
		case f.IdempotencyError:
			errorJSON(w, 400, "idempotency_error", "idempotency_key_in_use")
		case f.RateLimit:
			errorJSON(w, 429, "invalid_request_error", "rate_limit")
		case f.Conflict:
			errorJSON(w, 409, "invalid_request_error", "lock_timeout")
		default:
			errorJSON(w, 400, "invalid_request_error", "invalid_request")
		}
		return
	}
	s.inflight[key] = true
	s.mu.Unlock()
	defer func() { s.mu.Lock(); delete(s.inflight, key); s.mu.Unlock() }()
	if f.Delay > 0 {
		select {
		case <-time.After(f.Delay):
		case <-r.Context().Done():
			return
		}
	}
	// Registrar probes (metadata[lc_probe]) carry no attempt reference by design
	// (contract §13); every other session must reference its attempt.
	probe := vals.Get("metadata[lc_probe]") != ""
	if vals.Get("mode") != "payment" || vals.Get("payment_method_types[0]") != "card" || (vals.Get("client_reference_id") == "" && !probe) || vals.Get("line_items[0][price_data][currency]") == "" {
		errorJSON(w, 400, "invalid_request_error", "invalid_request")
		return // validation never enters cache
	}
	amount, err := strconv.ParseInt(vals.Get("line_items[0][price_data][unit_amount]"), 10, 64)
	if err != nil || amount <= 0 {
		errorJSON(w, 400, "invalid_request_error", "amount_too_small")
		return
	}
	expires, err := strconv.ParseInt(vals.Get("expires_at"), 10, 64)
	if err != nil || expires <= 0 {
		errorJSON(w, 400, "invalid_request_error", "invalid_expiry")
		return
	}
	s.mu.Lock()
	s.next++
	id := fmt.Sprintf("cs_test_fake_%d", s.next)
	v := &session{ID: id, Object: "checkout.session", Status: "open", PaymentStatus: "unpaid", Currency: strings.ToLower(vals.Get("line_items[0][price_data][currency]")), AmountTotal: amount, AmountSubtotal: amount, TotalDetails: map[string]int64{"amount_discount": 0, "amount_tax": 0, "amount_shipping": 0}, ClientReferenceID: vals.Get("client_reference_id"), Metadata: metadataOf(vals), owner: account, ExpiresAt: expires, Created: time.Now().Unix(), Mode: "payment", PaymentMethodTypes: []string{"card"}, URL: "https://checkout.stripe.com/c/pay/" + id}
	s.sessions[id] = v
	if f.Cached500NoSession {
		delete(s.sessions, id)
	}
	status := 200
	var reply []byte
	if f.Cached500 || f.Cached500NoSession {
		status = 500
		reply = []byte(`{"error":{"type":"api_error","code":"api_error"}}`)
	} else {
		reply, _ = json.Marshal(v)
	}
	s.cache[key] = cached{params: params, status: status, body: reply}
	s.mu.Unlock()
	if f.DropAfterExecute {
		hijack, ok := w.(http.Hijacker)
		if ok {
			conn, _, e := hijack.Hijack()
			if e == nil {
				_ = conn.Close()
				return
			}
		}
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(reply)
}

// Patch merges provider-side anomalies (amount drift, presentment details, a
// legacy currency_conversion, a foreign payment_intent) into every later read of
// the session; a nil value deletes the key. Stripe never lets a merchant do
// this, which is exactly why the reconciliation SQL must catch it (§6.5).
func (s *Server) Patch(id string, fields map[string]any) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	v := s.sessions[id]
	if v == nil {
		return false
	}
	if v.overlay == nil {
		v.overlay = map[string]any{}
	}
	for k, x := range fields {
		v.overlay[k] = x
	}
	return true
}

// AgeSession moves one session's created/expires_at back by d, in step with a
// test that aged the same attempt's stored deadlines (sstAge); otherwise the
// worker's identity check (expires_at must match the frozen value) would fail.
func (s *Server) AgeSession(id string, d time.Duration) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	v := s.sessions[id]
	if v == nil {
		return false
	}
	v.Created -= int64(d.Seconds())
	v.ExpiresAt -= int64(d.Seconds())
	return true
}

// SessionByReference returns the first session whose client_reference_id is ref
// ("" if none): tests use it to find the session an attempt executed at the
// provider even when the worker never pinned it (cached 500, dropped response).
func (s *Server) SessionByReference(ref string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	best := ""
	for id, v := range s.sessions {
		if v.ClientReferenceID == ref && (best == "" || id < best) {
			best = id
		}
	}
	return best
}

// Inject creates a second session for account without an idempotency key,
// modelling a duplicate/late session that carries our reference (contract
// §10 "Second session with our reference").
func (s *Server) Inject(account string, vals url.Values) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.next++
	id := fmt.Sprintf("cs_test_fake_%d", s.next)
	amount, _ := strconv.ParseInt(vals.Get("line_items[0][price_data][unit_amount]"), 10, 64)
	expires, _ := strconv.ParseInt(vals.Get("expires_at"), 10, 64)
	s.sessions[id] = &session{ID: id, Object: "checkout.session", Status: "open", PaymentStatus: "unpaid", Currency: strings.ToLower(vals.Get("line_items[0][price_data][currency]")),
		AmountTotal: amount, AmountSubtotal: amount, TotalDetails: map[string]int64{"amount_discount": 0, "amount_tax": 0, "amount_shipping": 0},
		ClientReferenceID: vals.Get("client_reference_id"), Metadata: metadataOf(vals), owner: account, ExpiresAt: expires, Created: time.Now().Unix(),
		Mode: "payment", PaymentMethodTypes: []string{"card"}, URL: "https://checkout.stripe.com/c/pay/" + id}
	return id
}

// view renders a session with its Patch overlay applied. Callers hold no lock.
func view(v session) map[string]any {
	raw, _ := json.Marshal(v)
	out := map[string]any{}
	_ = json.Unmarshal(raw, &out)
	for k, x := range v.overlay {
		if x == nil {
			delete(out, k)
		} else {
			out[k] = x
		}
	}
	return out
}

// metadataOf copies every metadata[k] parameter; Stripe echoes metadata verbatim.
func metadataOf(vals url.Values) map[string]string {
	out := map[string]string{}
	for k := range vals {
		if strings.HasPrefix(k, "metadata[") && strings.HasSuffix(k, "]") {
			out[k[len("metadata["):len(k)-1]] = vals.Get(k)
		}
	}
	return out
}

func (s *Server) retrieve(w http.ResponseWriter, id, account string) {
	s.mu.Lock()
	v := s.sessions[id]
	if v != nil && v.owner != account {
		v = nil // another account's session is invisible, like Stripe's 404
	}
	var snapshot session
	if v != nil {
		snapshot = *v
	}
	s.mu.Unlock()
	if v == nil {
		errorJSON(w, 404, "invalid_request_error", "resource_missing")
		return
	}
	writeJSON(w, 200, view(snapshot))
}
func (s *Server) expire(w http.ResponseWriter, id, account string) {
	s.mu.Lock()
	v := s.sessions[id]
	if v != nil && v.owner != account {
		v = nil
	}
	if v == nil {
		s.mu.Unlock()
		errorJSON(w, 404, "invalid_request_error", "resource_missing")
		return
	}
	if v.Status != "open" {
		s.mu.Unlock()
		errorJSON(w, 400, "invalid_request_error", "checkout_session_not_expirable")
		return
	}
	v.Status = "expired"
	v.URL = ""
	snapshot := *v
	s.mu.Unlock()
	writeJSON(w, 200, view(snapshot))
}
func (s *Server) list(w http.ResponseWriter, r *http.Request, account string) {
	q := r.URL.Query()
	limit, _ := strconv.Atoi(q.Get("limit"))
	if limit < 1 || limit > 100 {
		errorJSON(w, 400, "invalid_request_error", "invalid_limit")
		return
	}
	gte, _ := strconv.ParseInt(q.Get("created[gte]"), 10, 64)
	lte, _ := strconv.ParseInt(q.Get("created[lte]"), 10, 64)
	s.mu.Lock()
	data := make([]*session, 0, len(s.sessions))
	for _, v := range s.sessions {
		if v.owner == account && v.Created >= gte && v.Created <= lte {
			copy := *v
			data = append(data, &copy)
		}
	}
	s.mu.Unlock()
	// Stable order is enough for the bounded lookup protocol.
	for i := 0; i < len(data); i++ {
		for j := i + 1; j < len(data); j++ {
			if data[i].ID < data[j].ID {
				data[i], data[j] = data[j], data[i]
			}
		}
	}
	start := 0
	if after := q.Get("starting_after"); after != "" {
		for i, v := range data {
			if v.ID == after {
				start = i + 1
				break
			}
		}
	}
	if start > len(data) {
		start = len(data)
	}
	data = data[start:]
	hasMore := len(data) > limit
	if hasMore {
		data = data[:limit]
	}
	views := make([]map[string]any, len(data))
	for i, v := range data {
		views[i] = view(*v)
	}
	writeJSON(w, 200, map[string]any{"object": "list", "data": views, "has_more": hasMore})
}
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
func errorJSON(w http.ResponseWriter, status int, typ, code string) {
	writeJSON(w, status, map[string]any{"error": map[string]string{"type": typ, "code": code}})
}
