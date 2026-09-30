// session.go: Checkout Session parsing, CallMeta and the observation projection
// (contracts/stripe-psp-v1.md §5.6, §5.7). It never decides paid/unpaid or compares
// money against the order; payments.apply_stripe_observation does that in SQL.
//
// Ownership: integration_worker. Dependencies: strictjson.go only.
// Callers: Client methods here; the payment worker writes Observation into
// payments.provider_observations.report (provider 'stripe').

package stripe

import (
	"regexp"
	"strings"
)

// Session object fields: https://docs.stripe.com/api/checkout/sessions/object
// (retrieved 2026-09-28, F3, F8).
var (
	stripeIDPattern  = regexp.MustCompile(`^[A-Za-z0-9_]{1,255}$`) // F9: prefixes are not authority
	requestIDPattern = regexp.MustCompile(`^[A-Za-z0-9_]{0,64}$`)
	errorWordPattern = regexp.MustCompile(`^[a-z_]{0,64}$`)
	refValuePattern  = regexp.MustCompile(`^[A-Za-z0-9_-]{0,64}$`)
	methodPattern    = regexp.MustCompile(`^[a-z_]{1,32}$`)
	currencyPattern  = regexp.MustCompile(`^[a-z]{3}$`)
	sessionStatuses  = map[string]bool{"open": true, "complete": true, "expired": true}
	paymentStatuses  = map[string]bool{"paid": true, "unpaid": true, "no_payment_required": true}
	observationVias  = map[string]bool{"create": true, "retrieve": true, "expire": true, "list": true, "unsent": true, "escalate": true}
)

// invalidRef replaces a client_reference_id or metadata value that is not a bounded
// token. It can never equal an attempt uuid, so it never matches, and it keeps the
// projection within its 2048-byte CHECK.
const invalidRef = "?"

const maxPaymentMethodTypes = 8

// CallMeta is the bounded, non-secret evidence of one HTTP exchange.
type CallMeta struct {
	HTTPStatus         int
	RequestID          string // Request-Id header when it matches ^[A-Za-z0-9_]{0,64}$
	IdempotentReplayed bool   // Idempotent-Replayed: true (F4)
	ShouldRetry        *bool  // Stripe-Should-Retry hint; never implies a closure
	ErrorType          string // bounded ^[a-z_]{0,64}$; never the message
	ErrorCode          string // bounded ^[a-z_]{0,64}$; never the message
}

func (CallMeta) String() string               { return "stripe.CallMeta{redacted}" }
func (m CallMeta) GoString() string           { return m.String() }
func (CallMeta) MarshalJSON() ([]byte, error) { return []byte(`"stripe.CallMeta{redacted}"`), nil }

// Session is the validated §5.7 projection of a Checkout Session. The hosted URL is
// private: it is readable only through URL() for the pin write and never formatted.
type Session struct {
	ID, Status, PaymentStatus   string
	Livemode                    bool
	Currency                    string // uppercase, or "" when absent
	AmountTotal                 *int64
	AmountSubtotal              *int64
	AmountDiscount              *int64 // total_details.amount_discount
	AmountTax                   *int64 // total_details.amount_tax
	AmountShipping              *int64 // total_details.amount_shipping
	PresentmentCurrency         string // presentment_details.presentment_currency, uppercase (F8)
	PresentmentAmount           *int64 // presentment_details.presentment_amount
	CurrencyConversion          bool   // legacy currency_conversion present (F8)
	ClientReferenceID           string
	MetadataAttempt             string
	MetadataProfile             string
	ExpiresAt, Created          *int64
	Mode                        string
	PaymentMethodTypes          []string
	PaymentIntentID             string
	PaymentIntentStatus         string // only when payment_intent was expanded
	PaymentIntentAmountReceived *int64 // only when payment_intent was expanded
	PaymentIntentCurrency       string // uppercase; only when payment_intent was expanded
	url                         string
}

func (Session) String() string               { return "stripe.Session{redacted}" }
func (s Session) GoString() string           { return s.String() }
func (Session) MarshalJSON() ([]byte, error) { return []byte(`"stripe.Session{redacted}"`), nil }

// URL returns the hosted Checkout URL (present only while the session is active, F3).
// It exists only for the stripe_sessions pin write; callers must never log it.
func (s Session) URL() string { return s.url }

// parseSession validates one checkout.session object. Any structural doubt returns
// ErrUncertain (§5.6): a response we cannot fully trust is not evidence.
func parseSession(o jsonObject, wantLivemode bool) (Session, error) {
	var s Session
	obj, _, ok1 := o.str("object")
	id, _, ok2 := o.str("id")
	live, livePresent, ok3 := o.boolean("livemode")
	status, _, ok4 := o.str("status")
	payStatus, _, ok5 := o.str("payment_status")
	if !ok1 || !ok2 || !ok3 || !ok4 || !ok5 || obj != "checkout.session" ||
		!stripeIDPattern.MatchString(id) || !livePresent || live != wantLivemode ||
		!sessionStatuses[status] || !paymentStatuses[payStatus] {
		return Session{}, ErrUncertain
	}
	s.ID, s.Livemode, s.Status, s.PaymentStatus = id, live, status, payStatus

	var ok bool
	if s.Currency, ok = currencyField(o, "currency"); !ok {
		return Session{}, ErrUncertain
	}
	if s.AmountTotal, ok = nonNegative(o, "amount_total"); !ok {
		return Session{}, ErrUncertain
	}
	if s.AmountSubtotal, ok = nonNegative(o, "amount_subtotal"); !ok {
		return Session{}, ErrUncertain
	}
	if s.ExpiresAt, ok = nonNegative(o, "expires_at"); !ok {
		return Session{}, ErrUncertain
	}
	if s.Created, ok = nonNegative(o, "created"); !ok {
		return Session{}, ErrUncertain
	}
	td, _, ok := o.object("total_details")
	if !ok {
		return Session{}, ErrUncertain
	}
	if td != nil {
		var okD, okT, okS bool
		s.AmountDiscount, okD = nonNegative(td, "amount_discount")
		s.AmountTax, okT = nonNegative(td, "amount_tax")
		s.AmountShipping, okS = nonNegative(td, "amount_shipping")
		if !okD || !okT || !okS {
			return Session{}, ErrUncertain
		}
	}
	pd, _, ok := o.object("presentment_details")
	if !ok {
		return Session{}, ErrUncertain
	}
	if pd != nil {
		var okA bool
		if s.PresentmentCurrency, ok = currencyField(pd, "presentment_currency"); !ok {
			return Session{}, ErrUncertain
		}
		if s.PresentmentAmount, okA = nonNegative(pd, "presentment_amount"); !okA {
			return Session{}, ErrUncertain
		}
	}
	// Any non-null legacy currency_conversion is flagged for review (F8, §6.5).
	if v, exists := o["currency_conversion"]; exists && v != nil {
		s.CurrencyConversion = true
	}
	ref, _, ok := o.str("client_reference_id")
	if !ok {
		return Session{}, ErrUncertain
	}
	s.ClientReferenceID = boundedRef(ref)
	md, _, ok := o.object("metadata")
	if !ok {
		return Session{}, ErrUncertain
	}
	if md != nil {
		a, _, okA := md.str("lc_attempt")
		p, _, okP := md.str("lc_profile")
		if !okA || !okP {
			return Session{}, ErrUncertain
		}
		s.MetadataAttempt, s.MetadataProfile = boundedRef(a), boundedRef(p)
	}
	mode, _, ok := o.str("mode")
	if !ok || (mode != "" && !methodPattern.MatchString(mode)) {
		return Session{}, ErrUncertain
	}
	s.Mode = mode
	if s.PaymentMethodTypes, ok = methodTypes(o["payment_method_types"]); !ok {
		return Session{}, ErrUncertain
	}
	if !parsePaymentIntent(o["payment_intent"], &s) {
		return Session{}, ErrUncertain
	}
	u, _, ok := o.str("url")
	if !ok {
		return Session{}, ErrUncertain
	}
	s.url = u
	return s, nil
}

func currencyField(o jsonObject, key string) (string, bool) {
	v, present, ok := o.str(key)
	if !ok || (present && !currencyPattern.MatchString(v)) {
		return "", false
	}
	return strings.ToUpper(v), true
}

func nonNegative(o jsonObject, key string) (*int64, bool) {
	v, ok := o.integer(key)
	if !ok || (v != nil && *v < 0) {
		return nil, false
	}
	return v, true
}

func boundedRef(v string) string {
	if refValuePattern.MatchString(v) {
		return v
	}
	return invalidRef
}

func methodTypes(raw any) ([]string, bool) {
	if raw == nil {
		return []string{}, true
	}
	arr, ok := raw.([]any)
	if !ok || len(arr) > maxPaymentMethodTypes {
		return nil, false
	}
	out := make([]string, 0, len(arr))
	for _, v := range arr {
		s, ok := v.(string)
		if !ok || !methodPattern.MatchString(s) {
			return nil, false
		}
		out = append(out, s)
	}
	return out, true
}

// parsePaymentIntent accepts either an id string or the expanded object; only the
// §5.7 fields are kept, and client_secret and everything else are discarded.
func parsePaymentIntent(raw any, s *Session) bool {
	switch v := raw.(type) {
	case nil:
		return true
	case string:
		if !stripeIDPattern.MatchString(v) {
			return false
		}
		s.PaymentIntentID = v
		return true
	case map[string]any:
		pi := jsonObject(v)
		id, _, ok1 := pi.str("id")
		status, _, ok2 := pi.str("status")
		recv, ok3 := nonNegative(pi, "amount_received")
		cur, ok4 := currencyField(pi, "currency")
		if !ok1 || !ok2 || !ok3 || !ok4 || !stripeIDPattern.MatchString(id) ||
			(status != "" && !methodPattern.MatchString(status)) {
			return false
		}
		s.PaymentIntentID, s.PaymentIntentStatus, s.PaymentIntentAmountReceived, s.PaymentIntentCurrency = id, status, recv, cur
		return true
	}
	return false
}

// Observation is the flat §5.7 report. It has exactly these JSON keys, all always
// present (nullable ones as null), and never the URL, customer data, card data,
// error messages or raw JSON.
type Observation struct {
	Provider                    string   `json:"Provider"`
	Version                     int      `json:"Version"`
	Via                         string   `json:"Via"`
	AccountID                   string   `json:"AccountID"`
	KeyVersion                  int64    `json:"KeyVersion"`
	RequestID                   string   `json:"RequestID"`
	SendCount                   int      `json:"SendCount"`
	SessionID                   string   `json:"SessionID"`
	Status                      string   `json:"Status"`
	PaymentStatus               string   `json:"PaymentStatus"`
	Livemode                    bool     `json:"Livemode"`
	Currency                    string   `json:"Currency"`
	AmountTotal                 *int64   `json:"AmountTotal"`
	AmountSubtotal              *int64   `json:"AmountSubtotal"`
	AmountDiscount              *int64   `json:"AmountDiscount"`
	AmountTax                   *int64   `json:"AmountTax"`
	AmountShipping              *int64   `json:"AmountShipping"`
	PresentmentCurrency         string   `json:"PresentmentCurrency"`
	PresentmentAmount           *int64   `json:"PresentmentAmount"`
	CurrencyConversion          bool     `json:"CurrencyConversion"`
	ClientReferenceID           string   `json:"ClientReferenceID"`
	MetadataAttempt             string   `json:"MetadataAttempt"`
	MetadataProfile             string   `json:"MetadataProfile"`
	ExpiresAt                   *int64   `json:"ExpiresAt"`
	Created                     *int64   `json:"Created"`
	Mode                        string   `json:"Mode"`
	PaymentMethodTypes          []string `json:"PaymentMethodTypes"`
	PaymentIntentID             string   `json:"PaymentIntentID"`
	PaymentIntentStatus         string   `json:"PaymentIntentStatus"`
	PaymentIntentAmountReceived *int64   `json:"PaymentIntentAmountReceived"`
	PaymentIntentCurrency       string   `json:"PaymentIntentCurrency"`
	ErrorClass                  string   `json:"ErrorClass"`
	ErrorCode                   string   `json:"ErrorCode"`
	HTTPStatus                  int      `json:"HTTPStatus"`
	ListMatchCount              *int     `json:"ListMatchCount"`
	LocalReason                 string   `json:"LocalReason"`
}

// Observation projects s plus call evidence into the §5.7 report. via must be one
// of create|retrieve|expire|list|unsent|escalate (anything else yields Via="" so
// the DB CHECK rejects it). sendCount is kept only for via=create. ErrorClass is
// "rejected" when meta is a definitive rejection (400 invalid_request_error or 404);
// whether that rejection closes the attempt is the caller's first-send rule (§10).
// For via=list, ListMatchCount is 1 when s is a match and 0 otherwise; a caller
// with several matches overwrites it. LocalReason is left for LOCAL writers.
func (s Session) Observation(via string, meta CallMeta, account string, keyVersion int64, sendCount int) Observation {
	if !observationVias[via] {
		via = ""
	}
	if via != "create" {
		sendCount = 0
	}
	methods := s.PaymentMethodTypes
	if methods == nil {
		methods = []string{}
	}
	o := Observation{
		Provider: "stripe", Version: 1, Via: via, AccountID: account, KeyVersion: keyVersion,
		SendCount: sendCount, SessionID: s.ID, Status: s.Status, PaymentStatus: s.PaymentStatus,
		Livemode: s.Livemode, Currency: s.Currency, AmountTotal: s.AmountTotal,
		AmountSubtotal: s.AmountSubtotal, AmountDiscount: s.AmountDiscount, AmountTax: s.AmountTax,
		AmountShipping: s.AmountShipping, PresentmentCurrency: s.PresentmentCurrency,
		PresentmentAmount: s.PresentmentAmount, CurrencyConversion: s.CurrencyConversion,
		ClientReferenceID: s.ClientReferenceID, MetadataAttempt: s.MetadataAttempt,
		MetadataProfile: s.MetadataProfile, ExpiresAt: s.ExpiresAt, Created: s.Created,
		Mode: s.Mode, PaymentMethodTypes: methods, PaymentIntentID: s.PaymentIntentID,
		PaymentIntentStatus: s.PaymentIntentStatus, PaymentIntentAmountReceived: s.PaymentIntentAmountReceived,
		PaymentIntentCurrency: s.PaymentIntentCurrency, HTTPStatus: meta.HTTPStatus,
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
		if s.ID != "" {
			n = 1
		}
		o.ListMatchCount = &n
	}
	return o
}
