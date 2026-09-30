// Package billingtest owns the independent MOCK of the Stripe Billing endpoints the platform-fee client
// calls: GET /v1/account, POST /v1/customers (idempotency cache), POST /v1/checkout/sessions and
// /{id}/expire, POST /v1/billing_portal/sessions, GET /v1/subscriptions/{id} and the list by customer,
// GET /v1/prices/{id}, plus a signed-event builder.
// It never contacts Stripe (its Transport refuses every host but api.stripe.com and rewrites that one to
// the local listener), never decides billing state in PG, never stores card data and never reads the
// client under test: it is written from the Stripe docs cited per behaviour below (retrieved 2026-09-29),
// so a client that drifts from the docs fails here instead of being mirrored by its own fake.
// Every call is captured (method, path, query, sorted form body, kept headers) so tests can assert the
// exact bytes sent; captured Authorization values are reduced to a non-secret fingerprint.
package billingtest

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"livecommerce/internal/integrations/psp/stripe/stripetest"
)

// APIVersion is the Stripe-Version the platform client must pin (stripe-psp, F-B10). The fake does not
// derive it from the adapter; CB08 asserts the captured header equals this literal.
const APIVersion = "2026-08-26.dahlia"

// Call is one captured request. Body is the raw form body; Form is its parsed, sorted view.
type Call struct {
	Method, Path, RawQuery string
	Body                   string
	Form                   url.Values
	IdempotencyKey         string
	StripeVersion          string
	ContentType            string
	KeyFingerprint         string
}

// SortedBody returns the form body as sorted k=v pairs joined by "&" (encoded), the byte form CB08 pins.
func (c Call) SortedBody() string {
	keys := make([]string, 0, len(c.Form))
	for k := range c.Form {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var parts []string
	for _, k := range keys {
		for _, v := range c.Form[k] {
			parts = append(parts, url.QueryEscape(k)+"="+url.QueryEscape(v))
		}
	}
	return strings.Join(parts, "&")
}

type cachedResponse struct {
	fingerprint string
	status      int
	body        []byte
}

// Session is the fake's Checkout Session state (F-B4).
type Session struct {
	ID, Customer, Status, ClientReferenceID, URL string
	ExpiresAt                                    int64
	Params                                       url.Values
}

// Sub describes one Subscription for AddSubscription (F-B1 statuses, F-B10 item-level period fields).
type Sub struct {
	ID, Customer, Status, PriceID, StoreMeta string
	Created, PeriodStart, PeriodEnd          int64
	CancelAtPeriodEnd, Livemode              bool
	Items                                    int // 0 or 1 → one item; n>1 → n items (billing_multi_item)
	NoMetadata                               bool
}

// Server is the fake. Safe for concurrent use.
type Server struct {
	mu        sync.Mutex
	http      *httptest.Server
	account   string
	key       string
	seq       int
	tag       string                       // unique per Server: ids of two fakes never collide in one shared database
	customers map[string]map[string]string // id -> metadata
	sessions  map[string]*Session
	subs      map[string]Sub
	prices    map[string]map[string]any
	idem      map[string]cachedResponse // method+path+key
	calls     []Call
	failures  map[string][]int
	hangs     map[string]int
	lose      map[string]int
	delay     map[string]time.Duration
	events    int
}

// New starts the fake for one platform account id.
func New(accountID string) *Server {
	tag := make([]byte, 4)
	_, _ = rand.Read(tag)
	s := &Server{tag: hex.EncodeToString(tag), account: accountID, customers: map[string]map[string]string{}, sessions: map[string]*Session{},
		subs: map[string]Sub{}, prices: map[string]map[string]any{}, idem: map[string]cachedResponse{},
		failures: map[string][]int{}, hangs: map[string]int{}, lose: map[string]int{}, delay: map[string]time.Duration{}}
	s.http = httptest.NewServer(http.HandlerFunc(s.serve))
	return s
}

// Close stops the listener.
func (s *Server) Close() { s.http.Close() }

// URL is the local listener (tests normally use Transport).
func (s *Server) URL() string { return s.http.URL }

// Account is the account id GET /v1/account reports; SetAccount changes it (BD1 conflict cases).
func (s *Server) Account() string { s.mu.Lock(); defer s.mu.Unlock(); return s.account }

// SetAccount changes the id GET /v1/account reports.
func (s *Server) SetAccount(id string) { s.mu.Lock(); s.account = id; s.mu.Unlock() }

// RequireKey makes every call need `Authorization: Bearer <key>` (401 otherwise, F-B4 auth).
func (s *Server) RequireKey(key string) { s.mu.Lock(); s.key = key; s.mu.Unlock() }

// Transport intercepts https://api.stripe.com only; any other destination is refused.
func (s *Server) Transport() http.RoundTripper {
	return roundTripper(func(req *http.Request) (*http.Response, error) {
		if req.URL.Scheme != "https" || req.URL.Host != "api.stripe.com" {
			return nil, fmt.Errorf("billingtest: refused non-Stripe destination")
		}
		c := req.Clone(req.Context())
		c.URL.Scheme, c.URL.Host = "http", strings.TrimPrefix(s.http.URL, "http://")
		c.Host = c.URL.Host
		return s.http.Client().Transport.RoundTrip(c)
	})
}

type roundTripper func(*http.Request) (*http.Response, error)

func (f roundTripper) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// FailNext queues one HTTP status for the next call of op: "account", "customer", "checkout", "expire",
// "portal", "retrieve" (GET subscription), "list" (GET subscriptions), "price". A 5xx models an outage.
func (s *Server) FailNext(op string, status int) {
	s.mu.Lock()
	s.failures[op] = append(s.failures[op], status)
	s.mu.Unlock()
}

// HangNext makes the next call of op never answer until the client gives up (a timeout).
func (s *Server) HangNext(op string) { s.mu.Lock(); s.hangs[op]++; s.mu.Unlock() }

// LoseNext makes the next call of op execute normally but never deliver its response (the connection is
// dropped after the effect): the "response lost" case of a create call.
func (s *Server) LoseNext(op string) { s.mu.Lock(); s.lose[op]++; s.mu.Unlock() }

// DelayNext delays the next call of op by d before it executes (parallel-request races).
func (s *Server) DelayNext(op string, d time.Duration) { s.mu.Lock(); s.delay[op] = d; s.mu.Unlock() }

// Calls returns every captured call in arrival order.
func (s *Server) Calls() []Call {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Call(nil), s.calls...)
}

// CallsTo returns the captured calls of one method whose path starts with prefix.
func (s *Server) CallsTo(method, prefix string) (out []Call) {
	for _, c := range s.Calls() {
		if c.Method == method && strings.HasPrefix(c.Path, prefix) {
			out = append(out, c)
		}
	}
	return out
}

// CallsExact returns the captured calls of one method whose path equals path (CallsTo matches a prefix: the create
// path "/v1/checkout/sessions" is also the prefix of every ".../{id}/expire").
func (s *Server) CallsExact(method, path string) (out []Call) {
	for _, c := range s.Calls() {
		if c.Method == method && c.Path == path {
			out = append(out, c)
		}
	}
	return out
}

// ResetCalls forgets the captured calls (state is kept).
func (s *Server) ResetCalls() { s.mu.Lock(); s.calls = nil; s.mu.Unlock() }

// Sessions returns a snapshot of every Checkout Session.
func (s *Server) Sessions() (out []Session) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, v := range s.sessions {
		out = append(out, *v)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// OpenSessions counts sessions still in status open.
func (s *Server) OpenSessions() (n int) {
	for _, v := range s.Sessions() {
		if v.Status == "open" {
			n++
		}
	}
	return n
}

// AddCustomer registers a Customer (as if created through the API) and returns its id.
func (s *Server) AddCustomer(metadata map[string]string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.seq++
	id := "cus_Fake" + s.tag + strconv.Itoa(s.seq)
	s.customers[id] = metadata
	return id
}

// Customers returns the ids of every Customer created or added.
func (s *Server) Customers() (ids []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for id := range s.customers {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

// AddSubscription stores (or replaces) a Subscription and returns it with defaults filled in.
func (s *Server) AddSubscription(v Sub) Sub {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.seq++
	if v.ID == "" {
		v.ID = "sub_Fake" + s.tag + strconv.Itoa(s.seq)
	}
	if v.Status == "" {
		v.Status = "active"
	}
	if v.PriceID == "" {
		v.PriceID = "price_Fake1"
	}
	if v.Created == 0 {
		v.Created = time.Now().Add(-time.Hour).Unix()
	}
	if v.PeriodStart == 0 {
		v.PeriodStart = time.Now().Add(-24 * time.Hour).Unix()
		v.PeriodEnd = time.Now().Add(29 * 24 * time.Hour).Unix()
	}
	s.subs[v.ID] = v
	return v
}

// ClearSubscriptions forgets every stored Subscription (a store back to "never subscribed" at Stripe).
func (s *Server) ClearSubscriptions() {
	s.mu.Lock()
	s.subs = map[string]Sub{}
	s.mu.Unlock()
}

// SetSubStatus changes the status of a stored Subscription.
func (s *Server) SetSubStatus(id, status string) {
	s.mu.Lock()
	v := s.subs[id]
	v.Status = status
	s.subs[id] = v
	s.mu.Unlock()
}

// AddPrice registers a Price for GET /v1/prices/{id} (F-B4 recurring price, expand[]=product).
func (s *Server) AddPrice(id string, amount int64, currency, interval, product string, active, livemode bool) {
	s.mu.Lock()
	s.prices[id] = map[string]any{"id": id, "object": "price", "active": active, "livemode": livemode, "currency": currency,
		"unit_amount": amount, "recurring": map[string]any{"interval": interval, "interval_count": 1},
		"product": map[string]any{"id": "prod_Fake1", "object": "product", "name": product}}
	s.mu.Unlock()
}

// AddPriceRaw registers a Price object as given (one-time prices, inactive prices, odd shapes).
func (s *Server) AddPriceRaw(id string, obj map[string]any) {
	s.mu.Lock()
	obj["id"], obj["object"] = id, "price"
	s.prices[id] = obj
	s.mu.Unlock()
}

func fingerprint(form url.Values) string {
	c := Call{Form: form}
	d := sha256.Sum256([]byte(c.SortedBody()))
	return hex.EncodeToString(d[:])
}

func (s *Server) serve(w http.ResponseWriter, r *http.Request) {
	raw, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	form, _ := url.ParseQuery(string(raw))
	fp := ""
	if tok, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer "); ok {
		d := sha256.Sum256([]byte(tok))
		fp = hex.EncodeToString(d[:4])
	}
	s.mu.Lock()
	s.calls = append(s.calls, Call{Method: r.Method, Path: r.URL.Path, RawQuery: r.URL.RawQuery, Body: string(raw), Form: form,
		IdempotencyKey: r.Header.Get("Idempotency-Key"), StripeVersion: r.Header.Get("Stripe-Version"),
		ContentType: r.Header.Get("Content-Type"), KeyFingerprint: fp})
	want := s.key
	s.mu.Unlock()
	if want != "" && r.Header.Get("Authorization") != "Bearer "+want {
		errorJSON(w, 401, "authentication_error", "invalid_api_key")
		return
	}
	op, id := route(r.Method, r.URL.Path)
	if op == "" {
		errorJSON(w, 404, "invalid_request_error", "resource_missing")
		return
	}
	s.mu.Lock()
	hang, d := s.hangs[op] > 0, s.delay[op]
	if hang {
		s.hangs[op]--
	}
	delete(s.delay, op)
	var status int
	if q := s.failures[op]; len(q) > 0 {
		status, s.failures[op] = q[0], q[1:]
	}
	s.mu.Unlock()
	if hang {
		<-r.Context().Done()
		return
	}
	if d > 0 {
		time.Sleep(d)
	}
	if status != 0 {
		errorJSON(w, status, "api_error", "api_error")
		return
	}
	s.mu.Lock()
	lose := s.lose[op] > 0
	if lose {
		s.lose[op]--
	}
	s.mu.Unlock()
	if !lose {
		s.dispatch(w, r, op, id, form)
		return
	}
	s.dispatch(httptest.NewRecorder(), r, op, id, form) // the effect happens, the answer is thrown away
	if hj, ok := w.(http.Hijacker); ok {
		if conn, _, err := hj.Hijack(); err == nil {
			_ = conn.Close()
		}
	}
}

func (s *Server) dispatch(w http.ResponseWriter, r *http.Request, op, id string, form url.Values) {
	switch op {
	case "account":
		writeJSON(w, 200, map[string]any{"id": s.Account(), "object": "account", "livemode": false})
	case "customer":
		s.createCustomer(w, r, form)
	case "checkout":
		s.createCheckout(w, r, form)
	case "expire":
		s.expire(w, r, id)
	case "portal":
		s.portal(w, form)
	case "retrieve":
		s.retrieve(w, id)
	case "list":
		s.list(w, r.URL.Query())
	case "price":
		s.price(w, id, r.URL.Query())
	}
}

func route(method, path string) (op, id string) {
	switch {
	case method == "GET" && path == "/v1/account":
		return "account", ""
	case method == "POST" && path == "/v1/customers":
		return "customer", ""
	case method == "POST" && path == "/v1/checkout/sessions":
		return "checkout", ""
	case method == "POST" && strings.HasPrefix(path, "/v1/checkout/sessions/") && strings.HasSuffix(path, "/expire"):
		return "expire", strings.TrimSuffix(strings.TrimPrefix(path, "/v1/checkout/sessions/"), "/expire")
	case method == "POST" && path == "/v1/billing_portal/sessions":
		return "portal", ""
	case method == "GET" && path == "/v1/subscriptions":
		return "list", ""
	case method == "GET" && strings.HasPrefix(path, "/v1/subscriptions/"):
		return "retrieve", strings.TrimPrefix(path, "/v1/subscriptions/")
	case method == "GET" && strings.HasPrefix(path, "/v1/prices/"):
		return "price", strings.TrimPrefix(path, "/v1/prices/")
	}
	return "", ""
}

// idempotent implements https://docs.stripe.com/api/idempotent_requests (retrieved 2026-09-29): the same
// key with the same parameters replays the first response; the same key with different parameters is a
// 400 idempotency_error. It reports whether it answered.
func (s *Server) idempotent(w http.ResponseWriter, r *http.Request, form url.Values) (key string, answered bool) {
	key = r.Header.Get("Idempotency-Key")
	if key == "" {
		return "", false
	}
	slot := r.Method + " " + r.URL.Path + " " + key
	s.mu.Lock()
	c, ok := s.idem[slot]
	s.mu.Unlock()
	if !ok {
		return slot, false
	}
	if c.fingerprint != fingerprint(form) {
		errorJSON(w, 400, "idempotency_error", "Keys for idempotent requests can only be used with the same parameters they were first used with.")
		return slot, true
	}
	w.Header().Set("Idempotent-Replayed", "true")
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(c.status)
	_, _ = w.Write(c.body)
	return slot, true
}

func (s *Server) remember(slot string, form url.Values, status int, v any) {
	if slot == "" {
		return
	}
	b, _ := json.Marshal(v)
	s.mu.Lock()
	s.idem[slot] = cachedResponse{fingerprint: fingerprint(form), status: status, body: b}
	s.mu.Unlock()
}

func (s *Server) createCustomer(w http.ResponseWriter, r *http.Request, form url.Values) {
	slot, done := s.idempotent(w, r, form)
	if done {
		return
	}
	md := map[string]string{}
	for k, v := range form {
		if strings.HasPrefix(k, "metadata[") && strings.HasSuffix(k, "]") && len(v) > 0 {
			md[k[len("metadata["):len(k)-1]] = v[0]
		}
	}
	id := s.AddCustomer(md)
	out := map[string]any{"id": id, "object": "customer", "livemode": false, "metadata": md}
	s.remember(slot, form, 200, out)
	writeJSON(w, 200, out)
}

// createCheckout follows F-B4: mode=subscription needs recurring line_items and a known customer,
// client_reference_id ≤ 200 chars, expires_at between 30 min and 24 h after creation. The 30-minute
// boundary tolerates 5 s (the exact server-side rounding is UNKNOWN; ponytail: tighten if Stripe is
// observed to reject a client that sends now+30min).
func (s *Server) createCheckout(w http.ResponseWriter, r *http.Request, form url.Values) {
	slot, done := s.idempotent(w, r, form)
	if done {
		return
	}
	bad := func(msg string) { errorJSON(w, 400, "invalid_request_error", msg) }
	if form.Get("mode") != "subscription" {
		bad("mode must be subscription")
		return
	}
	if form.Get("line_items[0][price]") == "" || form.Get("line_items[0][quantity]") == "" {
		bad("line_items required")
		return
	}
	if len(form.Get("client_reference_id")) > 200 {
		bad("client_reference_id too long")
		return
	}
	if form.Get("success_url") == "" || form.Get("cancel_url") == "" {
		bad("success_url and cancel_url required")
		return
	}
	cust := form.Get("customer")
	s.mu.Lock()
	_, known := s.customers[cust]
	s.mu.Unlock()
	if cust == "" || !known {
		bad("No such customer")
		return
	}
	exp, err := strconv.ParseInt(form.Get("expires_at"), 10, 64)
	now := time.Now().Unix()
	if err != nil || exp < now+30*60-5 || exp > now+24*3600 {
		bad("expires_at must be 30 minutes to 24 hours after creation")
		return
	}
	s.mu.Lock()
	s.seq++
	id := "cs_test_Fake" + s.tag + strconv.Itoa(s.seq)
	sess := &Session{ID: id, Customer: cust, Status: "open", ClientReferenceID: form.Get("client_reference_id"), ExpiresAt: exp,
		URL: "https://checkout.stripe.com/c/pay/" + id + "#fakebearer", Params: form}
	s.sessions[id] = sess
	s.mu.Unlock()
	out := map[string]any{"id": id, "object": "checkout.session", "status": "open", "mode": "subscription", "customer": cust,
		"client_reference_id": sess.ClientReferenceID, "expires_at": exp, "url": sess.URL, "livemode": false}
	s.remember(slot, form, 200, out)
	writeJSON(w, 200, out)
}

func (s *Server) expire(w http.ResponseWriter, r *http.Request, id string) {
	slot, done := s.idempotent(w, r, nil)
	if done {
		return
	}
	s.mu.Lock()
	sess := s.sessions[id]
	if sess == nil {
		s.mu.Unlock()
		errorJSON(w, 404, "invalid_request_error", "resource_missing")
		return
	}
	if sess.Status != "open" {
		s.mu.Unlock()
		errorJSON(w, 400, "invalid_request_error", "Only Checkout Sessions with a status in open can be expired.")
		return
	}
	sess.Status, sess.URL = "expired", ""
	s.mu.Unlock()
	out := map[string]any{"id": id, "object": "checkout.session", "status": "expired", "livemode": false}
	s.remember(slot, url.Values{}, 200, out)
	writeJSON(w, 200, out)
}

// portal follows F-B6: customer and return_url in, {id,url} out.
func (s *Server) portal(w http.ResponseWriter, form url.Values) {
	cust := form.Get("customer")
	s.mu.Lock()
	_, known := s.customers[cust]
	s.seq++
	id := "bps_Fake" + s.tag + strconv.Itoa(s.seq)
	s.mu.Unlock()
	if !known || form.Get("return_url") == "" {
		errorJSON(w, 400, "invalid_request_error", "customer and return_url required")
		return
	}
	writeJSON(w, 200, map[string]any{"id": id, "object": "billing_portal.session", "customer": cust,
		"return_url": form.Get("return_url"), "url": "https://billing.stripe.com/p/session/" + id + "#fakebearer", "livemode": false})
}

func (v Sub) object() map[string]any {
	md := map[string]string{}
	if !v.NoMetadata && v.StoreMeta != "" {
		md["lc_store"] = v.StoreMeta
	}
	n := v.Items
	if n < 1 {
		n = 1
	}
	items := make([]any, n)
	for i := range items {
		pid := v.PriceID
		if i > 0 {
			pid = v.PriceID + "x" + strconv.Itoa(i)
		}
		// F-B10: period fields live on the item in current API versions, never on the subscription.
		items[i] = map[string]any{"id": "si_Fake" + strconv.Itoa(i), "object": "subscription_item", "price": map[string]any{"id": pid, "object": "price"},
			"quantity": 1, "current_period_start": v.PeriodStart, "current_period_end": v.PeriodEnd}
	}
	return map[string]any{"id": v.ID, "object": "subscription", "customer": v.Customer, "status": v.Status, "created": v.Created,
		"livemode": v.Livemode, "cancel_at_period_end": v.CancelAtPeriodEnd, "metadata": md,
		"items": map[string]any{"object": "list", "data": items, "has_more": false}}
}

// SubObject renders a stored Subscription exactly as the fake serves it (for event payloads).
func (s *Server) SubObject(id string) map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.subs[id].object()
}

func (s *Server) retrieve(w http.ResponseWriter, id string) {
	s.mu.Lock()
	v, ok := s.subs[id]
	s.mu.Unlock()
	if !ok {
		errorJSON(w, 404, "invalid_request_error", "resource_missing")
		return
	}
	writeJSON(w, 200, v.object())
}

// list follows https://docs.stripe.com/api/subscriptions/list: without status=all canceled
// subscriptions are omitted; customer filters; limit caps.
func (s *Server) list(w http.ResponseWriter, q url.Values) {
	s.mu.Lock()
	var ids []string
	for id, v := range s.subs {
		if q.Get("customer") != "" && v.Customer != q.Get("customer") {
			continue
		}
		if st := q.Get("status"); st == "" && v.Status == "canceled" || st != "" && st != "all" && st != v.Status {
			continue
		}
		ids = append(ids, id)
	}
	sort.Strings(ids)
	limit := 10
	if n, err := strconv.Atoi(q.Get("limit")); err == nil && n > 0 && n <= 100 {
		limit = n
	}
	more := len(ids) > limit
	if more {
		ids = ids[:limit]
	}
	data := make([]any, 0, len(ids))
	for _, id := range ids {
		data = append(data, s.subs[id].object())
	}
	s.mu.Unlock()
	writeJSON(w, 200, map[string]any{"object": "list", "data": data, "has_more": more, "url": "/v1/subscriptions"})
}

func (s *Server) price(w http.ResponseWriter, id string, q url.Values) {
	s.mu.Lock()
	p, ok := s.prices[id]
	s.mu.Unlock()
	if !ok {
		errorJSON(w, 404, "invalid_request_error", "resource_missing")
		return
	}
	out := map[string]any{}
	for k, v := range p {
		out[k] = v
	}
	expanded := false
	for _, e := range q["expand[]"] {
		expanded = expanded || e == "product"
	}
	if !expanded {
		out["product"] = "prod_Fake1"
	}
	writeJSON(w, 200, out)
}

// EventOpts describes one webhook payload.
type EventOpts struct {
	ID       string
	Livemode bool
	Created  int64
	Account  string // non-empty adds a Connect "account" field
}

func (s *Server) event(typ string, o EventOpts, obj map[string]any) []byte {
	s.mu.Lock()
	s.events++
	n := s.events
	s.mu.Unlock()
	if o.ID == "" {
		o.ID = "evt_Fake" + s.tag + strconv.Itoa(n)
	}
	if o.Created == 0 {
		o.Created = time.Now().Unix()
	}
	ev := map[string]any{"id": o.ID, "object": "event", "api_version": APIVersion, "created": o.Created, "livemode": o.Livemode,
		"pending_webhooks": 1, "type": typ, "data": map[string]any{"object": obj}}
	if o.Account != "" {
		ev["account"] = o.Account
	}
	b, _ := json.Marshal(ev)
	return b
}

// SubscriptionEvent builds customer.subscription.<x> (F-B3) whose data.object is the stored Subscription.
func (s *Server) SubscriptionEvent(typ, subID string, o EventOpts) []byte {
	return s.event(typ, o, s.SubObject(subID))
}

// InvoiceEvent builds invoice.<x> with the subscription reference at
// data.object.parent.subscription_details.subscription (API ≥ 2025-03-31.basil, F-B3) and NO top-level
// `subscription` field, so a client that reads the old location fails.
func (s *Server) InvoiceEvent(typ, subID string, o EventOpts) []byte {
	obj := map[string]any{"id": "in_Fake1", "object": "invoice", "status": "paid", "livemode": o.Livemode,
		"parent": map[string]any{"type": "subscription_details", "subscription_details": map[string]any{"subscription": subID}}}
	if subID == "" {
		obj["parent"] = nil
	}
	return s.event(typ, o, obj)
}

// RawEvent builds an event of any type with the given data.object.
func (s *Server) RawEvent(typ string, o EventOpts, obj map[string]any) []byte {
	return s.event(typ, o, obj)
}

// Sign returns the Stripe-Signature header for body at time at, using the shared Stripe wire format
// (https://docs.stripe.com/webhooks/signature): stripetest.SignWebhook is the one shared signer.
func Sign(secret string, body []byte, at time.Time) string {
	return stripetest.SignWebhook(secret, body, at)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	b, _ := json.Marshal(v)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(b)
}

func errorJSON(w http.ResponseWriter, status int, typ, msg string) {
	writeJSON(w, status, map[string]any{"error": map[string]any{"type": typ, "message": msg}})
}
