// params.go: canonical Checkout Session create parameters and idempotency keys
// (contracts/stripe-psp-v1.md §5.4, §11). It never builds the key set itself: the
// set is frozen in SQL by checkout.start_stripe_payment and only validated/encoded here.
//
// Ownership: integration_worker. Dependencies: net/url for form encoding, regexp.
// Callers: Client.CreateCheckoutSession, the payment worker (to pin
// create_body_sha256 from the first encoded body) and SP03.

package stripe

import (
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// CreateParams is exactly the §5.4 key set, loaded from payments.stripe_sessions.create_params.
type CreateParams struct{ Fields map[string]string }

// Each key below is a Checkout Session create parameter.
// https://docs.stripe.com/api/checkout/sessions/create (retrieved 2026-09-28, F1, F8).
const (
	pAdaptivePricing = "adaptive_pricing[enabled]" // F8: disabled per session (Q6)
	pManagedPayments = "managed_payments[enabled]" // F13: on by default on the sandbox; disabled (Q6)
	pAutomaticTax    = "automatic_tax[enabled]"    // no Stripe Tax in v1
	pCancelURL       = "cancel_url"                // neutral return page, never a session id
	pClientRef       = "client_reference_id"       // F1: ≤200 chars; our attempt uuid
	pExpiresAt       = "expires_at"                // F1: 30 min..24 h after creation; D4 fixes 40 min
	pCurrency        = "line_items[0][price_data][currency]"
	pProductName     = "line_items[0][price_data][product_data][name]"
	pUnitAmount      = "line_items[0][price_data][unit_amount]" // F7: minor unit
	pQuantity        = "line_items[0][quantity]"
	pLocale          = "locale" // F1: zh-TW, zh, en
	pMetaAttempt     = "metadata[lc_attempt]"
	pMetaOrder       = "metadata[lc_order]"
	pMetaProfile     = "metadata[lc_profile]"
	pMetaVersion     = "metadata[lc_v]"
	pMode            = "mode"
	pPIMetaAttempt   = "payment_intent_data[metadata][lc_attempt]"
	pPIMetaOrder     = "payment_intent_data[metadata][lc_order]"
	pMethodTypes     = "payment_method_types[0]" // Q3: card only
	pSubmitType      = "submit_type"
	pSuccessURL      = "success_url"
	pUIMode          = "ui_mode" // F1: hosted_page
)

// fixedValues are the keys whose value is a constant of the contract.
var fixedValues = map[string]string{
	pAdaptivePricing: "false",
	pManagedPayments: "false",
	pAutomaticTax:    "false",
	pQuantity:        "1",
	pMetaVersion:     "1",
	pMode:            "payment",
	pMethodTypes:     "card",
	pSubmitType:      "pay",
	pUIMode:          "hosted_page",
}

// variableKeys are validated individually in validateCreate.
var variableKeys = []string{
	pCancelURL, pClientRef, pExpiresAt, pCurrency, pProductName, pUnitAmount, pLocale,
	pMetaAttempt, pMetaOrder, pMetaProfile, pPIMetaAttempt, pPIMetaOrder, pSuccessURL,
}

const maxUnixSeconds = 253402300799

var (
	uuidPattern    = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
	decimalPattern = regexp.MustCompile(`^[1-9][0-9]{0,18}$`)
	expireKeyGen   = regexp.MustCompile(`^lc:stripe:cs-expire:v1:[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}:(0|[1-9][0-9]{0,18})$`)
	allowedLocales = map[string]bool{"zh-TW": true, "zh": true, "en": true}
	allowedProfile = map[string]bool{"PROVIDER_MOCK": true, "SANDBOX": true, "LIVE": true}
)

// validateCreate enforces the exact §5.4 key set and every value rule. Forbidden
// parameters (§5.4 "never sent") fail simply because they are not in the set.
func validateCreate(p CreateParams) error {
	f := p.Fields
	if len(f) != len(fixedValues)+len(variableKeys) {
		return ErrInvalid
	}
	for k, v := range fixedValues {
		if f[k] != v {
			return ErrInvalid
		}
	}
	for _, k := range variableKeys {
		if _, ok := f[k]; !ok {
			return ErrInvalid
		}
	}
	attempt, order := f[pMetaAttempt], f[pMetaOrder]
	if !uuidPattern.MatchString(attempt) || !uuidPattern.MatchString(order) ||
		f[pClientRef] != attempt || f[pPIMetaAttempt] != attempt || f[pPIMetaOrder] != order {
		return ErrInvalid
	}
	if !allowedProfile[f[pMetaProfile]] || !allowedLocales[f[pLocale]] {
		return ErrInvalid
	}
	// No catalog text or PII: the name is derived from the order id only.
	if f[pProductName] != "Order "+strings.ToUpper(order[:8]) {
		return ErrInvalid
	}
	if !decimalPattern.MatchString(f[pExpiresAt]) {
		return ErrInvalid
	}
	if exp, err := strconv.ParseInt(f[pExpiresAt], 10, 64); err != nil || exp > maxUnixSeconds {
		return ErrInvalid
	}
	// Stripe expects lowercase currency on the wire; PG stores uppercase (§4).
	cur := f[pCurrency]
	if cur != strings.ToLower(cur) || !decimalPattern.MatchString(f[pUnitAmount]) {
		return ErrInvalid
	}
	amount, err := strconv.ParseInt(f[pUnitAmount], 10, 64)
	if err != nil {
		return ErrInvalid
	}
	// I05: unit_amount must be exactly the admitted order amount; UnitAmount never rounds.
	if unit, err := UnitAmount(strings.ToUpper(cur), amount); err != nil || unit != amount {
		return ErrInvalid
	}
	if f[pSuccessURL] != f[pCancelURL] || !validReturnURL(f[pSuccessURL]) {
		return ErrInvalid
	}
	return nil
}

// validReturnURL admits an absolute https URL without userinfo, query (§9.3: "There is
// no query"), fragment, template braces ({CHECKOUT_SESSION_ID} is never sent) or control
// characters.
func validReturnURL(raw string) bool {
	if len(raw) == 0 || len(raw) > 2048 || strings.ContainsAny(raw, "{} \t\r\n") {
		return false
	}
	for i := 0; i < len(raw); i++ {
		if raw[i] < 0x21 || raw[i] > 0x7e {
			return false
		}
	}
	u, err := url.Parse(raw)
	return err == nil && u.Scheme == "https" && u.Host != "" && u.User == nil &&
		u.RawQuery == "" && !u.ForceQuery && u.Fragment == "" && u.Opaque == ""
}

// EncodeCreateBody validates p against §5.4 and returns the
// application/x-www-form-urlencoded body: keys sorted bytewise, each key and value
// escaped with url.QueryEscape, joined by '&'. The output is deterministic, so
// every resend of one attempt is byte-identical (SP03). Any extra, missing or
// out-of-rule key returns ErrInvalid.
func EncodeCreateBody(p CreateParams) ([]byte, error) {
	if err := validateCreate(p); err != nil {
		return nil, err
	}
	keys := make([]string, 0, len(p.Fields))
	for k := range p.Fields {
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
		b.WriteString(url.QueryEscape(p.Fields[k]))
	}
	return []byte(b.String()), nil
}

// CreateIdempotencyKey returns the single create key for an attempt:
// "lc:stripe:cs-create:v1:<attempt>". Every send of the attempt uses it; a new key is
// never minted because a cached 500 is indeterminate (F4, I06).
// https://docs.stripe.com/api/idempotent_requests (retrieved 2026-09-28, F4: ≤255 chars).
func CreateIdempotencyKey(attemptID string) string {
	return "lc:stripe:cs-create:v1:" + attemptID
}

// ExpireIdempotencyKey returns "lc:stripe:cs-expire:v1:<attempt>:<generation>". A fresh
// key per claim generation is safe: expire moves no money and a retrieve always
// follows, while a fixed key would pin a cached 500 forever (§11).
func ExpireIdempotencyKey(attemptID string, generation int64) string {
	return "lc:stripe:cs-expire:v1:" + attemptID + ":" + strconv.FormatInt(generation, 10)
}
