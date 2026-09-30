package stripetest

// Refund half of the fake: POST/GET /v1/refunds, GET /v1/refunds/{id}, and
// GET /v1/payment_intents/{id}[?expand[]=latest_charge] (contracts/stripe-refund-v1.md §1, §3).
//
// Facts modelled (https://docs.stripe.com/api/refunds/create and /object, retrieved 2026-09-29):
// amount is a positive integer and cannot exceed the unrefunded remainder of the charge;
// reason is duplicate | fraudulent | requested_by_customer; a card refund is normally
// `succeeded`, may be held `pending`, and may later become `failed` or `canceled` (which
// frees the amount again); the list is newest first with limit 1..100 and starting_after.
// Idempotency behaves as for checkout creates (params compare, cached 500, replay header,
// per-account scope). Unknown form keys are refused like Stripe's parameter_unknown, which is
// what makes the adapter's exact-key-set rule observable from the outside.
//
// The refund/charge objects carry sentinel ARN / receipt URL / billing e-mail values on
// purpose (SentinelARN...): the product must never persist them, and tests scan for them.

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Sentinels the fake puts into refund/charge objects and webhook payloads. Contract §3.1:
// "Never stored: destination_details (ARN), card data, receipt_url, billing details".
const (
	SentinelARN        = "ARN_SENTINEL_74839201"
	SentinelReceiptURL = "https://pay.stripe.com/receipts/SENTINEL_RECEIPT_5521"
	SentinelEmail      = "sentinel.buyer.5521@example.invalid"
)

type paymentIntentRec struct {
	id, owner, currency, chargeID, session string
	captured                               int64
	patch                                  map[string]any // Patch-style overlay merged into the charge view
}

type refundRec struct {
	id, pi, owner, status, failure, pending, reason, currency string
	amount                                                    int64
	metadata                                                  map[string]string
	seq                                                       int
	created                                                   int64
	overlay                                                   map[string]any
	dashboard                                                 bool // created outside our API (no metadata, no idempotency key)
}

type refundState struct {
	pis        map[string]*paymentIntentRec
	refunds    map[string]*refundRec
	seq        int
	keys       []string // every Idempotency-Key seen on POST /v1/refunds, in order (with repeats)
	bodies     []string // raw form bodies of every POST /v1/refunds
	posts      int
	nextStatus string
	nextPend   string
}

func (s *Server) rfLocked() *refundState {
	if s.rf == nil {
		s.rf = &refundState{pis: map[string]*paymentIntentRec{}, refunds: map[string]*refundRec{}}
	}
	return s.rf
}

func (s *Server) registerPaymentIntentLocked(id, owner string, amount int64, currency, session string) {
	rf := s.rfLocked()
	if _, ok := rf.pis[id]; ok {
		return
	}
	rf.pis[id] = &paymentIntentRec{id: id, owner: owner, currency: currency, captured: amount, chargeID: "ch_test_" + session, session: session}
}

// AddPaidPaymentIntent registers a captured PaymentIntent directly (adapter-level tests that
// need no checkout session). currency is lower case as Stripe returns it.
func (s *Server) AddPaidPaymentIntent(account, id string, amount int64, currency string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.registerPaymentIntentLocked(id, account, amount, strings.ToLower(currency), id)
}

// PatchCharge merges provider-side anomalies (disputed, amount_captured drift, ...) into every
// later charge read of the PaymentIntent; a nil value deletes the key.
func (s *Server) PatchCharge(pi string, fields map[string]any) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	p := s.rfLocked().pis[pi]
	if p == nil {
		return false
	}
	if p.patch == nil {
		p.patch = map[string]any{}
	}
	for k, v := range fields {
		p.patch[k] = v
	}
	return true
}

// HoldNextRefund makes the next refund create answer with status (pending | requires_action |
// failed | canceled | succeeded) instead of succeeded; pendingReason is echoed for pending.
func (s *Server) HoldNextRefund(status, pendingReason string) {
	s.mu.Lock()
	rf := s.rfLocked()
	rf.nextStatus, rf.nextPend = status, pendingReason
	s.mu.Unlock()
}

// SetRefundStatus moves one refund to a provider status (Stripe only ever moves pending ->
// succeeded/failed/canceled and succeeded -> failed, but tests may also force stale orders).
// failureReason is echoed for failed/canceled.
func (s *Server) SetRefundStatus(id, status, failureReason string) bool {
	switch status {
	case "pending", "requires_action", "succeeded", "failed", "canceled":
	default:
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	r := s.rfLocked().refunds[id]
	if r == nil {
		return false
	}
	r.status = status
	r.pending = ""
	if status == "pending" {
		r.pending = "processing"
	}
	r.failure = ""
	if status == "failed" || status == "canceled" {
		r.failure = failureReason
		if r.failure == "" {
			r.failure = "unknown"
		}
	}
	return true
}

// PatchRefund merges anomalies (amount/currency drift, a foreign payment_intent) into every
// later read of the refund; nil deletes the key.
func (s *Server) PatchRefund(id string, fields map[string]any) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	r := s.rfLocked().refunds[id]
	if r == nil {
		return false
	}
	if r.overlay == nil {
		r.overlay = map[string]any{}
	}
	for k, v := range fields {
		r.overlay[k] = v
	}
	return true
}

// DashboardRefund models a merchant refunding in the Stripe Dashboard: it consumes charge
// capacity but carries no lc_refund metadata and is not a POST /v1/refunds call, so
// RefundPosts stays unchanged (RD9 zero-POST proof).
func (s *Server) DashboardRefund(pi string, amount int64) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	rf := s.rfLocked()
	p := rf.pis[pi]
	if p == nil {
		return ""
	}
	rf.seq++
	id := fmt.Sprintf("re_test_dash_%d", rf.seq)
	rf.refunds[id] = &refundRec{id: id, pi: pi, owner: p.owner, status: "succeeded", reason: "requested_by_customer", currency: p.currency,
		amount: amount, metadata: map[string]string{}, seq: rf.seq, created: time.Now().Unix(), dashboard: true}
	return id
}

// RefundPosts counts every POST /v1/refunds that reached the fake (replays and faults included).
func (s *Server) RefundPosts() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.rfLocked().posts
}

// RefundCreateKeys lists the Idempotency-Key of every POST /v1/refunds in arrival order.
func (s *Server) RefundCreateKeys() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.rfLocked().keys...)
}

// RefundBodies lists the raw form bodies of every POST /v1/refunds (no secrets travel in them).
func (s *Server) RefundBodies() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.rfLocked().bodies...)
}

// RefundByRef returns the id of the refund whose metadata[lc_refund] is ref ("" if none), so a
// test can find what the provider executed even when the worker never pinned it.
func (s *Server) RefundByRef(ref string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, r := range s.rfLocked().refunds {
		if ref != "" && r.metadata["lc_refund"] == ref {
			return r.id
		}
	}
	return ""
}

// RefundIDs lists refund ids of one PaymentIntent, oldest first.
func (s *Server) RefundIDs(pi string) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var rs []*refundRec
	for _, r := range s.rfLocked().refunds {
		if r.pi == pi {
			rs = append(rs, r)
		}
	}
	sort.Slice(rs, func(i, j int) bool { return rs[i].seq < rs[j].seq })
	out := make([]string, len(rs))
	for i, r := range rs {
		out[i] = r.id
	}
	return out
}

// PaymentIntentForSession returns the PaymentIntent id of a paid session ("" while unpaid).
func (s *Server) PaymentIntentForSession(session string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	for id, p := range s.rfLocked().pis {
		if p.session == session {
			return id
		}
	}
	return ""
}

// counted reports whether a refund still consumes charge capacity (Stripe frees failed and
// canceled refunds; pending and requires_action already reserve the amount).
func (r *refundRec) counted() bool { return r.status != "failed" && r.status != "canceled" }

func (rf *refundState) refundedLocked(pi string) (sum int64) {
	for _, r := range rf.refunds {
		if r.pi == pi && r.counted() {
			sum += r.amount
		}
	}
	return
}

func (rf *refundState) chargeView(p *paymentIntentRec) map[string]any {
	refunded := rf.refundedLocked(p.id)
	out := map[string]any{"id": p.chargeID, "object": "charge", "amount": p.captured, "amount_captured": p.captured, "amount_refunded": refunded,
		"refunded": refunded == p.captured, "disputed": false, "currency": p.currency, "livemode": false, "payment_intent": p.id,
		"receipt_url": SentinelReceiptURL, "billing_details": map[string]any{"email": SentinelEmail, "name": "Sentinel Buyer"}}
	for k, v := range p.patch {
		if v == nil {
			delete(out, k)
		} else {
			out[k] = v
		}
	}
	return out
}

func (rf *refundState) refundView(r *refundRec) map[string]any {
	p := rf.pis[r.pi]
	charge := ""
	if p != nil {
		charge = p.chargeID
	}
	out := map[string]any{"id": r.id, "object": "refund", "amount": r.amount, "currency": r.currency, "charge": charge, "payment_intent": r.pi,
		"status": r.status, "reason": r.reason, "created": r.created, "metadata": r.metadata, "livemode": false,
		"destination_details": map[string]any{"type": "card", "card": map[string]any{"type": "pending", "reference": SentinelARN, "reference_type": "acquirer_reference_number", "reference_status": "pending"}}}
	if r.status == "failed" || r.status == "canceled" {
		out["failure_reason"] = r.failure
	}
	if r.pending != "" && r.status == "pending" {
		out["pending_reason"] = r.pending
	}
	for k, v := range r.overlay {
		if v == nil {
			delete(out, k)
		} else {
			out[k] = v
		}
	}
	return out
}

// RefundEventBody renders the signed-webhook payload of one refund.* event from the fake's
// current refund state (event envelope: https://docs.stripe.com/api/events/object, retrieved
// 2026-09-29). eventType is refund.created | refund.updated | refund.failed.
func (s *Server) RefundEventBody(eventID, eventType, refundID string, livemode bool) []byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	rf := s.rfLocked()
	r := rf.refunds[refundID]
	if r == nil {
		return nil
	}
	return marshalEvent(eventID, eventType, livemode, rf.refundView(r))
}

// ChargeEventBody renders a charge.refunded event for the PaymentIntent's current charge.
func (s *Server) ChargeEventBody(eventID, pi string, livemode bool) []byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	rf := s.rfLocked()
	p := rf.pis[pi]
	if p == nil {
		return nil
	}
	return marshalEvent(eventID, "charge.refunded", livemode, rf.chargeView(p))
}

func marshalEvent(id, typ string, livemode bool, obj map[string]any) []byte {
	ev := map[string]any{"id": id, "object": "event", "api_version": "2026-08-26.dahlia", "created": time.Now().Unix(),
		"livemode": livemode, "pending_webhooks": 1, "type": typ, "data": map[string]any{"object": obj}}
	out, _ := json.Marshal(ev)
	return out
}

// serveRefunds handles the refund/payment-intent routes; it returns false for any other path.
func (s *Server) serveRefunds(w http.ResponseWriter, r *http.Request, account string) bool {
	path := r.URL.Path
	switch {
	case path == "/v1/refunds" && r.Method == http.MethodPost:
		s.refundCreate(w, r, account)
	case path == "/v1/refunds" && r.Method == http.MethodGet:
		if st := s.failed("refund_list"); st != 0 {
			errorJSON(w, st, "api_error", "api_error")
			return true
		}
		s.refundList(w, r, account)
	case strings.HasPrefix(path, "/v1/refunds/") && r.Method == http.MethodGet:
		if st := s.failed("refund_retrieve"); st != 0 {
			errorJSON(w, st, "api_error", "api_error")
			return true
		}
		s.refundRetrieve(w, strings.TrimPrefix(path, "/v1/refunds/"), account)
	case strings.HasPrefix(path, "/v1/payment_intents/") && r.Method == http.MethodGet:
		if st := s.failed("pi_retrieve"); st != 0 {
			errorJSON(w, st, "api_error", "api_error")
			return true
		}
		s.piRetrieve(w, r, strings.TrimPrefix(path, "/v1/payment_intents/"), account)
	default:
		return false
	}
	return true
}

var refundKeys = map[string]bool{"payment_intent": true, "amount": true, "reason": true, "metadata[lc_refund]": true, "metadata[lc_attempt]": true}

func (s *Server) refundCreate(w http.ResponseWriter, r *http.Request, account string) {
	raw := r.Header.Get("Idempotency-Key")
	key := account + "|refund|" + raw
	body, err := io.ReadAll(io.LimitReader(r.Body, 8192))
	if err != nil {
		errorJSON(w, 400, "invalid_request_error", "invalid_request")
		return
	}
	s.mu.Lock()
	rf := s.rfLocked()
	rf.posts++
	rf.keys = append(rf.keys, raw)
	rf.bodies = append(rf.bodies, string(body))
	s.mu.Unlock()
	if raw == "" || len(raw) > 255 {
		errorJSON(w, 400, "invalid_request_error", "parameter_invalid_empty")
		return
	}
	vals, err := url.ParseQuery(string(body))
	if err != nil {
		errorJSON(w, 400, "invalid_request_error", "invalid_request")
		return
	}
	params := vals.Encode()
	s.mu.Lock()
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
	for k := range vals {
		if !refundKeys[k] { // Stripe: parameter_unknown. Also refuses charge/fraudulent-only shapes below.
			errorJSON(w, 400, "invalid_request_error", "parameter_unknown")
			return
		}
	}
	reason := vals.Get("reason")
	if reason != "duplicate" && reason != "fraudulent" && reason != "requested_by_customer" {
		errorJSON(w, 400, "invalid_request_error", "parameter_invalid_enum")
		return
	}
	amount, aerr := strconv.ParseInt(vals.Get("amount"), 10, 64)
	if aerr != nil || amount <= 0 {
		errorJSON(w, 400, "invalid_request_error", "parameter_invalid_integer")
		return
	}
	s.mu.Lock()
	p := rf.pis[vals.Get("payment_intent")]
	if p == nil || p.owner != account {
		s.mu.Unlock()
		errorJSON(w, 404, "invalid_request_error", "resource_missing")
		return
	}
	remaining := p.captured - rf.refundedLocked(p.id)
	if remaining <= 0 {
		s.mu.Unlock()
		errorJSON(w, 400, "invalid_request_error", "charge_already_refunded")
		return
	}
	if amount > remaining {
		s.mu.Unlock()
		errorJSON(w, 400, "invalid_request_error", "amount_too_large")
		return
	}
	rf.seq++
	status, pend := "succeeded", ""
	if rf.nextStatus != "" {
		status, pend = rf.nextStatus, rf.nextPend
		rf.nextStatus, rf.nextPend = "", ""
	}
	rec := &refundRec{id: fmt.Sprintf("re_test_fake_%d", rf.seq), pi: p.id, owner: account, status: status, pending: pend, reason: reason,
		currency: p.currency, amount: amount, metadata: metadataOf(vals), seq: rf.seq, created: time.Now().Unix()}
	if status == "failed" || status == "canceled" {
		rec.failure = "unknown"
	}
	rf.refunds[rec.id] = rec
	code := 200
	var reply []byte
	if f.Cached500 || f.Cached500NoSession {
		code = 500
		reply = []byte(`{"error":{"type":"api_error","code":"api_error"}}`)
		if f.Cached500NoSession {
			delete(rf.refunds, rec.id) // executed nothing: a later list must find no match
		}
	} else {
		reply, _ = json.Marshal(rf.refundView(rec))
	}
	s.cache[key] = cached{params: params, status: code, body: reply}
	s.mu.Unlock()
	if f.DropAfterExecute {
		if hijack, ok := w.(http.Hijacker); ok {
			if conn, _, e := hijack.Hijack(); e == nil {
				_ = conn.Close()
			}
		}
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_, _ = w.Write(reply)
}

func (s *Server) refundRetrieve(w http.ResponseWriter, id, account string) {
	s.mu.Lock()
	rf := s.rfLocked()
	rec := rf.refunds[id]
	var view map[string]any
	if rec != nil && rec.owner == account {
		view = rf.refundView(rec)
	}
	s.mu.Unlock()
	if view == nil {
		errorJSON(w, 404, "invalid_request_error", "resource_missing")
		return
	}
	writeJSON(w, 200, view)
}

func (s *Server) refundList(w http.ResponseWriter, r *http.Request, account string) {
	q := r.URL.Query()
	limit, _ := strconv.Atoi(q.Get("limit"))
	pi := q.Get("payment_intent")
	if limit < 1 || limit > 100 || pi == "" {
		errorJSON(w, 400, "invalid_request_error", "invalid_request")
		return
	}
	s.mu.Lock()
	rf := s.rfLocked()
	var rs []*refundRec
	for _, rec := range rf.refunds {
		if rec.owner == account && rec.pi == pi {
			rs = append(rs, rec)
		}
	}
	sort.Slice(rs, func(i, j int) bool { return rs[i].seq > rs[j].seq }) // newest first (F-R6)
	start := 0
	if after := q.Get("starting_after"); after != "" {
		for i, rec := range rs {
			if rec.id == after {
				start = i + 1
				break
			}
		}
	}
	rs = rs[start:]
	more := len(rs) > limit
	if more {
		rs = rs[:limit]
	}
	views := make([]map[string]any, len(rs))
	for i, rec := range rs {
		views[i] = rf.refundView(rec)
	}
	s.mu.Unlock()
	writeJSON(w, 200, map[string]any{"object": "list", "data": views, "has_more": more})
}

func (s *Server) piRetrieve(w http.ResponseWriter, r *http.Request, id, account string) {
	s.mu.Lock()
	rf := s.rfLocked()
	p := rf.pis[id]
	if p == nil || p.owner != account {
		s.mu.Unlock()
		errorJSON(w, 404, "invalid_request_error", "resource_missing")
		return
	}
	var latest any = p.chargeID
	if exp := r.URL.Query()["expand[]"]; len(exp) == 1 && exp[0] == "latest_charge" {
		latest = rf.chargeView(p)
	}
	out := map[string]any{"id": p.id, "object": "payment_intent", "status": "succeeded", "amount": p.captured, "amount_received": p.captured,
		"currency": p.currency, "livemode": false, "latest_charge": latest}
	s.mu.Unlock()
	writeJSON(w, 200, out)
}

// RawEvent renders an arbitrary event envelope around obj (forged/foreign-account cases in
// tests: wrong lc_refund, wrong payment_intent, unknown object).
func RawEvent(eventID, eventType string, livemode bool, obj map[string]any) []byte {
	return marshalEvent(eventID, eventType, livemode, obj)
}
