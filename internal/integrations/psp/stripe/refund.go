// refund.go: Stripe refund wire calls and their observation projections
// (contracts/stripe-refund-v1.md §3, §3.1). It never decides refund state, capacity or review
// (payments.apply_stripe_refund does that in SQL), never retries and never mints a second create key.
//
// Ownership: integration_worker. External: api.stripe.com only through Client.call (docs URLs below).
//
// Wire facts (docs.stripe.com, retrieved 2026-09-28, F-R1..F-R7 of stripe-refund-v1 §1):
//   - POST /v1/refunds        https://docs.stripe.com/api/refunds/create  (payment_intent, amount, reason,
//     metadata; `amount` is always sent, never "omit for full"; `fraudulent` is never offered)
//   - GET  /v1/refunds/{id}   https://docs.stripe.com/api/refunds/retrieve
//   - GET  /v1/refunds        https://docs.stripe.com/api/refunds/list     (payment_intent, limit 1..100, starting_after)
//   - GET  /v1/payment_intents/{id}?expand[]=latest_charge  https://docs.stripe.com/api/payment_intents/retrieve
//   - Refund statuses / failure and pending reasons: https://docs.stripe.com/api/refunds/object
//   - Charge amount_captured / amount_refunded / refunded / disputed: https://docs.stripe.com/api/charges/object

package stripe

import (
	"context"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
)

// RefundParams is the frozen create input. Currency is never sent to Stripe (a refund inherits the
// charge currency); it exists so RefundAmountOK can enforce the local step rule before any send.
type RefundParams struct {
	PaymentIntentID, Currency, Reason, RefundRef, AttemptRef string
	AmountMinor                                              int64
}

// Refund is the validated projection of one refund object. Livemode is the environment of the client
// that read it: Stripe's refund object carries no livemode field, and a test key can only see test data.
type Refund struct {
	ID, Status, FailureReason, PendingReason, Currency, PaymentIntentID, MetadataRefund, MetadataAttempt string
	Amount                                                                                               int64
	Livemode                                                                                             bool
}

// PaymentCharge is the projection of a PaymentIntent's latest charge (expand[]=latest_charge).
type PaymentCharge struct {
	PaymentIntentID, ChargeID, Currency string
	AmountCaptured, AmountRefunded      int64
	Refunded, Disputed, Livemode        bool
}

func (Refund) String() string               { return "stripe.Refund{redacted}" }
func (r Refund) GoString() string           { return r.String() }
func (Refund) MarshalJSON() ([]byte, error) { return []byte(`"stripe.Refund{redacted}"`), nil }

func (PaymentCharge) String() string     { return "stripe.PaymentCharge{redacted}" }
func (c PaymentCharge) GoString() string { return c.String() }
func (PaymentCharge) MarshalJSON() ([]byte, error) {
	return []byte(`"stripe.PaymentCharge{redacted}"`), nil
}

var (
	refundReasons  = map[string]bool{"requested_by_customer": true, "duplicate": true}
	refundStatuses = map[string]bool{"pending": true, "requires_action": true, "succeeded": true, "failed": true, "canceled": true}
	failureReasons = map[string]bool{"lost_or_stolen_card": true, "expired_or_canceled_card": true,
		"charge_for_pending_refund_disputed": true, "insufficient_funds": true, "declined": true,
		"merchant_request": true, "unknown": true}
	pendingReasons = map[string]bool{"processing": true, "insufficient_funds": true, "charge_pending": true}
)

// refundRules is the step table of payments.stripe_refund_amount_ok: the §0.2 currency table
// without its minimum (no refund minimum is documented; Stripe's own 400 on the first send is
// definitive). TWD refunds are whole NT$ (Stripe pays TWD out as zero-decimal).
var refundRules = map[string]currencyRule{
	"HKD": {min: 1, max: 99_999_999, step: 1},
	"USD": {min: 1, max: 99_999_999, step: 1},
	"SGD": {min: 1, max: 99_999_999, step: 1},
	"MYR": {min: 1, max: 99_999_999, step: 1},
	"TWD": {min: 100, max: 99_999_900, step: 100},
}

// RefundAmountOK is the Go twin of payments.stripe_refund_amount_ok (RF01 asserts parity).
func RefundAmountOK(currency string, amountMinor int64) bool {
	rule, ok := refundRules[currency]
	return ok && amountMinor >= rule.min && amountMinor <= rule.max && amountMinor%rule.step == 0
}

// RefundIdempotencyKey is the single create key of one refund: "lc:stripe:refund:v1:<refund uuid>".
// Every send of the refund uses it; a new key is never minted because an uncertain send may have
// created the refund (I06/I20, RD5). Stripe prunes keys after >=24 h (F4); SQL stops resending at 20 h.
// https://docs.stripe.com/api/idempotent_requests (retrieved 2026-09-28).
func RefundIdempotencyKey(refundID string) string { return "lc:stripe:refund:v1:" + refundID }

// EncodeRefundBody validates p and returns the deterministic form body: exactly the keys
// amount, metadata[lc_attempt], metadata[lc_refund], payment_intent and reason, sorted bytewise and
// escaped with url.QueryEscape. Any invalid value (including reason "fraudulent") is ErrInvalid; the
// forbidden keys (charge, reverse_transfer, refund_application_fee, instructions_email, origin) can
// never appear because the key set is fixed here. Not part of the frozen surface: the worker needs the
// exact bytes to pin body_sha256 before the send, as it does with EncodeCreateBody.
func EncodeRefundBody(p RefundParams) ([]byte, error) {
	if !stripeIDPattern.MatchString(p.PaymentIntentID) || !uuidPattern.MatchString(p.RefundRef) ||
		!uuidPattern.MatchString(p.AttemptRef) || !refundReasons[p.Reason] ||
		!RefundAmountOK(p.Currency, p.AmountMinor) {
		return nil, ErrInvalid
	}
	fields := map[string]string{
		"amount":               strconv.FormatInt(p.AmountMinor, 10),
		"metadata[lc_attempt]": p.AttemptRef,
		"metadata[lc_refund]":  p.RefundRef,
		"payment_intent":       p.PaymentIntentID,
		"reason":               p.Reason,
	}
	keys := make([]string, 0, len(fields))
	for k := range fields {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	for i, k := range keys {
		if i > 0 {
			b.WriteByte('&')
		}
		b.WriteString(url.QueryEscape(k))
		b.WriteByte('=')
		b.WriteString(url.QueryEscape(fields[k]))
	}
	return []byte(b.String()), nil
}

// CreateRefund posts the frozen body with Idempotency-Key = RefundIdempotencyKey(RefundRef). Every send
// of one refund posts byte-identical bytes under the same key. On 200 the refund must carry our own
// metadata (lc_refund, lc_attempt) and the same PaymentIntent, else ErrUncertain. Classification is
// §5.6 unchanged: a 400 invalid_request_error or 404 is ErrRejected, definitive only on the FIRST send.
func (c *Client) CreateRefund(ctx context.Context, p RefundParams) (Refund, CallMeta, error) {
	body, err := EncodeRefundBody(p)
	if err != nil {
		return Refund{}, CallMeta{}, err
	}
	raw, meta, err := c.call(ctx, opCreate, http.MethodPost, "/v1/refunds", body, RefundIdempotencyKey(p.RefundRef))
	if err != nil {
		return Refund{}, meta, err
	}
	r, err := c.decodeRefund(raw)
	if err != nil || r.MetadataRefund != p.RefundRef || r.MetadataAttempt != p.AttemptRef ||
		r.PaymentIntentID != p.PaymentIntentID {
		return Refund{}, meta, ErrUncertain
	}
	return r, meta, nil
}

// RetrieveRefund calls GET /v1/refunds/{id}. A 404 is ErrUncertain: absence is never inferred for a
// pinned refund.
func (c *Client) RetrieveRefund(ctx context.Context, id string) (Refund, CallMeta, error) {
	if !stripeIDPattern.MatchString(id) {
		return Refund{}, CallMeta{}, ErrInvalid
	}
	raw, meta, err := c.call(ctx, opRetrieve, http.MethodGet, "/v1/refunds/"+id, nil, "")
	if err != nil {
		return Refund{}, meta, err
	}
	r, err := c.decodeRefund(raw)
	if err != nil || r.ID != id {
		return Refund{}, meta, ErrUncertain
	}
	return r, meta, nil
}

// ListRefunds returns every refund of the PaymentIntent, newest first, reading at most 10 pages of 100
// from startingAfter ("" = the first page). More pages is ErrUncertain, never "not found": the caller
// matches metadata.lc_refund itself.
func (c *Client) ListRefunds(ctx context.Context, paymentIntentID, startingAfter string) ([]Refund, CallMeta, error) {
	if !stripeIDPattern.MatchString(paymentIntentID) || (startingAfter != "" && !stripeIDPattern.MatchString(startingAfter)) {
		return nil, CallMeta{}, ErrInvalid
	}
	out := []Refund{}
	var meta CallMeta
	after := startingAfter
	for page := 0; page < maxListPages; page++ {
		q := "payment_intent=" + url.QueryEscape(paymentIntentID) + "&limit=" + strconv.Itoa(listPageLimit)
		if after != "" {
			q += "&starting_after=" + url.QueryEscape(after)
		}
		raw, m, err := c.call(ctx, opList, http.MethodGet, "/v1/refunds?"+q, nil, "")
		meta = m
		if err != nil {
			return nil, meta, err
		}
		refunds, hasMore, err := c.decodeRefundList(raw)
		if err != nil {
			return nil, meta, ErrUncertain
		}
		out = append(out, refunds...)
		if !hasMore {
			return out, meta, nil
		}
		if len(refunds) == 0 {
			return nil, meta, ErrUncertain // has_more with an empty page cannot advance
		}
		after = refunds[len(refunds)-1].ID
	}
	return nil, meta, ErrUncertain
}

// RetrievePaymentCharge calls GET /v1/payment_intents/{pi}?expand[]=latest_charge and returns only the
// §3.1 charge fields. An unexpanded or missing latest_charge is ErrUncertain.
func (c *Client) RetrievePaymentCharge(ctx context.Context, paymentIntentID string) (PaymentCharge, CallMeta, error) {
	if !stripeIDPattern.MatchString(paymentIntentID) {
		return PaymentCharge{}, CallMeta{}, ErrInvalid
	}
	raw, meta, err := c.call(ctx, opRetrieve, http.MethodGet,
		"/v1/payment_intents/"+paymentIntentID+"?expand%5B%5D=latest_charge", nil, "")
	if err != nil {
		return PaymentCharge{}, meta, err
	}
	root, err := decodeStrict(raw)
	if err != nil {
		return PaymentCharge{}, meta, ErrUncertain
	}
	obj, ok := root.(map[string]any)
	if !ok {
		return PaymentCharge{}, meta, ErrUncertain
	}
	pi := jsonObject(obj)
	kind, _, ok1 := pi.str("object")
	id, _, ok2 := pi.str("id")
	charge, present, ok3 := pi.object("latest_charge")
	if !ok1 || !ok2 || !ok3 || !present || kind != "payment_intent" || id != paymentIntentID {
		return PaymentCharge{}, meta, ErrUncertain
	}
	chargeKind, _, okK := charge.str("object")
	chargeID, _, okI := charge.str("id")
	currency, currencyPresent, okC := charge.str("currency")
	captured, okA := nonNegative(charge, "amount_captured")
	refundedAmount, okR := nonNegative(charge, "amount_refunded")
	refunded, _, okF := charge.boolean("refunded")
	disputed, _, okD := charge.boolean("disputed")
	live, livePresent, okL := charge.boolean("livemode")
	if !okK || !okI || !okC || !okA || !okR || !okF || !okD || !okL || chargeKind != "charge" ||
		!stripeIDPattern.MatchString(chargeID) || !currencyPresent || !currencyPattern.MatchString(currency) ||
		captured == nil || refundedAmount == nil || !livePresent || live != c.wantLivemode() {
		return PaymentCharge{}, meta, ErrUncertain
	}
	return PaymentCharge{PaymentIntentID: id, ChargeID: chargeID, Currency: strings.ToUpper(currency),
		AmountCaptured: *captured, AmountRefunded: *refundedAmount, Refunded: refunded, Disputed: disputed,
		Livemode: live}, meta, nil
}

func (c *Client) decodeRefund(raw []byte) (Refund, error) {
	root, err := decodeStrict(raw)
	if err != nil {
		return Refund{}, ErrUncertain
	}
	obj, ok := root.(map[string]any)
	if !ok {
		return Refund{}, ErrUncertain
	}
	return c.parseRefund(jsonObject(obj))
}

func (c *Client) decodeRefundList(raw []byte) ([]Refund, bool, error) {
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
	out := make([]Refund, 0, len(data))
	for _, item := range data {
		m, ok := item.(map[string]any)
		if !ok {
			return nil, false, ErrUncertain
		}
		r, err := c.parseRefund(jsonObject(m))
		if err != nil {
			return nil, false, ErrUncertain
		}
		out = append(out, r)
	}
	return out, hasMore, nil
}

// parseRefund validates one refund object. Any structural doubt is ErrUncertain (§5.6). destination_details
// (ARN), balance transactions, receipt numbers and every other field are never read.
func (c *Client) parseRefund(o jsonObject) (Refund, error) {
	kind, _, ok1 := o.str("object")
	id, _, ok2 := o.str("id")
	status, _, ok3 := o.str("status")
	failure, _, ok4 := o.str("failure_reason")
	pending, _, ok5 := o.str("pending_reason")
	currency, currencyPresent, ok6 := o.str("currency")
	amount, ok7 := nonNegative(o, "amount")
	pi, _, ok8 := o.str("payment_intent")
	live, livePresent, ok9 := o.boolean("livemode")
	md, _, ok10 := o.object("metadata")
	if !ok1 || !ok2 || !ok3 || !ok4 || !ok5 || !ok6 || !ok7 || !ok8 || !ok9 || !ok10 || kind != "refund" ||
		!stripeIDPattern.MatchString(id) || !refundStatuses[status] ||
		(failure != "" && !failureReasons[failure]) || (pending != "" && !pendingReasons[pending]) ||
		!currencyPresent || !currencyPattern.MatchString(currency) || amount == nil ||
		(pi != "" && !stripeIDPattern.MatchString(pi)) || (livePresent && live != c.wantLivemode()) {
		return Refund{}, ErrUncertain
	}
	r := Refund{ID: id, Status: status, FailureReason: failure, PendingReason: pending,
		Currency: strings.ToUpper(currency), PaymentIntentID: pi, Amount: *amount, Livemode: c.wantLivemode()}
	if md != nil {
		refundRef, _, okA := md.str("lc_refund")
		attemptRef, _, okB := md.str("lc_attempt")
		if !okA || !okB {
			return Refund{}, ErrUncertain
		}
		r.MetadataRefund, r.MetadataAttempt = boundedRef(refundRef), boundedRef(attemptRef)
	}
	return r, nil
}

// RefundObservation is the exact §3.1 refund report: these 25 keys, always present (nullable ones as
// null), never destination_details/ARN, card data, receipt URL, emails, error messages or raw JSON.
type RefundObservation struct {
	Provider        string `json:"Provider"`
	Version         int    `json:"Version"`
	Object          string `json:"Object"`
	Via             string `json:"Via"`
	AccountID       string `json:"AccountID"`
	KeyVersion      int64  `json:"KeyVersion"`
	RequestID       string `json:"RequestID"`
	SendCount       int    `json:"SendCount"`
	RefundRef       string `json:"RefundRef"`
	AttemptRef      string `json:"AttemptRef"`
	RefundID        string `json:"RefundID"`
	Status          string `json:"Status"`
	FailureReason   string `json:"FailureReason"`
	PendingReason   string `json:"PendingReason"`
	Amount          *int64 `json:"Amount"`
	Currency        string `json:"Currency"`
	PaymentIntentID string `json:"PaymentIntentID"`
	Livemode        bool   `json:"Livemode"`
	MetadataRefund  string `json:"MetadataRefund"`
	MetadataAttempt string `json:"MetadataAttempt"`
	ErrorClass      string `json:"ErrorClass"`
	ErrorCode       string `json:"ErrorCode"`
	HTTPStatus      int    `json:"HTTPStatus"`
	ListMatchCount  *int   `json:"ListMatchCount"`
	LocalReason     string `json:"LocalReason"`
}

var refundObservationVias = map[string]bool{"create": true, "retrieve": true, "list": true, "unsent": true, "escalate": true}

// Observation projects r plus call evidence into the §3.1 refund report. An empty r (ID "") yields an
// identity-less report (rejected create, list with no match, LOCAL codes). via outside
// create|retrieve|list|unsent|escalate yields Via="" so the SQL validation rejects it. sendCount is kept
// only for via=create. ErrorClass is "rejected" for a 400 invalid_request_error or 404; whether that
// closes the refund is the caller's first-send rule (RD4/RD5), and SQL re-checks it.
func (r Refund) Observation(via string, meta CallMeta, account string, keyVersion int64, sendCount int,
	refundRef, attemptRef string) RefundObservation {
	if !refundObservationVias[via] {
		via = ""
	}
	if via != "create" {
		sendCount = 0
	}
	o := RefundObservation{Provider: "stripe", Version: 1, Object: "refund", Via: via, AccountID: account,
		KeyVersion: keyVersion, SendCount: sendCount, RefundRef: refundRef, AttemptRef: attemptRef,
		RefundID: r.ID, Status: r.Status, FailureReason: r.FailureReason, PendingReason: r.PendingReason,
		Currency: r.Currency, PaymentIntentID: r.PaymentIntentID, Livemode: r.Livemode,
		MetadataRefund: r.MetadataRefund, MetadataAttempt: r.MetadataAttempt, HTTPStatus: meta.HTTPStatus}
	if r.ID != "" {
		amount := r.Amount
		o.Amount = &amount
	}
	if requestIDPattern.MatchString(meta.RequestID) {
		o.RequestID = meta.RequestID
	}
	if errorWordPattern.MatchString(meta.ErrorCode) {
		o.ErrorCode = meta.ErrorCode
	}
	if (meta.HTTPStatus == 400 && meta.ErrorType == "invalid_request_error") || meta.HTTPStatus == 404 {
		o.ErrorClass = "rejected"
	}
	if via == "list" {
		n := 0
		if r.ID != "" {
			n = 1
		}
		o.ListMatchCount = &n
	}
	return o
}

// ChargeObservation is the exact §3.1 charge report (17 keys).
type ChargeObservation struct {
	Provider       string `json:"Provider"`
	Version        int    `json:"Version"`
	Object         string `json:"Object"`
	Via            string `json:"Via"`
	AccountID      string `json:"AccountID"`
	KeyVersion     int64  `json:"KeyVersion"`
	RequestID      string `json:"RequestID"`
	RefundRef      string `json:"RefundRef"`
	PaymentIntent  string `json:"PaymentIntentID"`
	ChargeID       string `json:"ChargeID"`
	Currency       string `json:"Currency"`
	AmountCaptured *int64 `json:"AmountCaptured"`
	AmountRefunded *int64 `json:"AmountRefunded"`
	Refunded       bool   `json:"Refunded"`
	Disputed       bool   `json:"Disputed"`
	Livemode       bool   `json:"Livemode"`
	LocalReason    string `json:"LocalReason"`
}

// Observation projects the charge into the §3.1 charge report. via must be presend or retrieve
// (anything else yields Via="" so SQL rejects it); refundRef is our refund uuid only for presend.
func (c PaymentCharge) Observation(via string, meta CallMeta, account string, keyVersion int64, refundRef string) ChargeObservation {
	if via != "presend" && via != "retrieve" {
		via = ""
	}
	if via != "presend" {
		refundRef = ""
	}
	captured, refunded := c.AmountCaptured, c.AmountRefunded
	o := ChargeObservation{Provider: "stripe", Version: 1, Object: "charge", Via: via, AccountID: account,
		KeyVersion: keyVersion, RefundRef: refundRef, PaymentIntent: c.PaymentIntentID, ChargeID: c.ChargeID,
		Currency: c.Currency, AmountCaptured: &captured, AmountRefunded: &refunded, Refunded: c.Refunded,
		Disputed: c.Disputed, Livemode: c.Livemode}
	if requestIDPattern.MatchString(meta.RequestID) {
		o.RequestID = meta.RequestID
	}
	return o
}
