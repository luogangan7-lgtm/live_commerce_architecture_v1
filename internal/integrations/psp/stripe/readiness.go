// readiness.go: the LIVE account-readiness projection (contracts/stripe-live-enable-v1.md
// LD8, §5.2, ruling S3). AccountReadiness reads GET /v1/account and keeps ONLY the L12
// fields listed below; business profile, email, addresses, phone and every other field are
// never decoded into memory beyond the strict JSON tree and are never stored or returned.
//
// Ownership: integration_worker (carried by stripe-live-core). Dependencies: client.go call,
// strictjson.go. Callers: internal/payments/stripeadmin (LiveApprove) only.
// Why every field is a pointer: an absent or null field must never be read as false/0 and
// pass a gate (I17); nil maps to `stripe_live_readiness_unknown` in Readiness.JSON and in the
// SQL definer payments.approve_stripe_live (22023).

package stripe

import (
	"context"
	"encoding/json"
	"net/http"
	"unicode/utf8"
)

// Readiness is the tri-state projection of the Stripe Account object (docs URL for every
// field: https://docs.stripe.com/api/accounts/object, retrieved 2026-09-29, L12):
//
//	ChargesEnabled    account.charges_enabled
//	PayoutsEnabled    account.payouts_enabled
//	DetailsSubmitted  account.details_submitted
//	CurrentlyDueCount len(account.requirements.currently_due)   (requirements object must be present)
//	DescriptorLength  runes of account.settings.payments.statement_descriptor
//	PrefixLength      runes of account.settings.card_payments.statement_descriptor_prefix; a null
//	                  or "" prefix inside a present card_payments object is 0 (Stripe then uses the
//	                  static descriptor truncated to 10 chars, L7)
//	CVCRule, AVSRule  account.settings.card_payments.decline_on.{cvc_failure,avs_failure}
//	                  (an account setting, not a Radar rule; recorded as-is, L5/L12)
//
// nil means "absent or JSON null". Lengths count runes (Stripe limits are in characters).
type Readiness struct {
	ChargesEnabled, PayoutsEnabled, DetailsSubmitted, CVCRule, AVSRule *bool
	CurrentlyDueCount, DescriptorLength, PrefixLength                  *int
}

// The descriptor lengths are account data about the merchant's business; keep them out of
// every formatting path anyway (a %v of a struct full of pointers prints addresses).
func (Readiness) String() string               { return "stripe.Readiness{redacted}" }
func (r Readiness) GoString() string           { return r.String() }
func (Readiness) MarshalJSON() ([]byte, error) { return []byte(`"stripe.Readiness{redacted}"`), nil }

// JSON returns the exact eight-key object stored in payments.stripe_live_approvals
// .account_readiness (keys sorted, bools and ints only). Any nil field refuses with the fixed
// code so an absent Stripe field can never be turned into a stored false/0 (LD8).
func (r Readiness) JSON() ([]byte, error) {
	if r.ChargesEnabled == nil || r.PayoutsEnabled == nil || r.DetailsSubmitted == nil ||
		r.CVCRule == nil || r.AVSRule == nil || r.CurrentlyDueCount == nil ||
		r.DescriptorLength == nil || r.PrefixLength == nil {
		return nil, refuse(ErrUncertain, "stripe_live_readiness_unknown")
	}
	// encoding/json sorts map keys, which is the "sorted" contract.
	return json.Marshal(map[string]any{
		"ChargesEnabled": *r.ChargesEnabled, "PayoutsEnabled": *r.PayoutsEnabled,
		"DetailsSubmitted": *r.DetailsSubmitted, "CVCRule": *r.CVCRule, "AVSRule": *r.AVSRule,
		"CurrentlyDueCount": *r.CurrentlyDueCount, "DescriptorLength": *r.DescriptorLength,
		"PrefixLength": *r.PrefixLength,
	})
}

// AccountReadiness calls GET /v1/account (any environment) and projects the L12 fields.
// Like VerifyAccount the returned id must equal Config.AccountID, else ErrAuthentication.
// It is a read: no idempotency key, and a transport failure is ErrUncertain (the caller
// simply reruns; nothing was changed at Stripe). A wrong JSON type for a listed field is
// ErrUncertain, never a partial projection.
// https://docs.stripe.com/api/accounts/retrieve (retrieved 2026-09-29).
func (c *Client) AccountReadiness(ctx context.Context) (Readiness, CallMeta, error) {
	var zero Readiness
	if c == nil {
		return zero, CallMeta{}, ErrInvalid
	}
	raw, meta, err := c.call(ctx, opAccount, http.MethodGet, "/v1/account", nil, "")
	if err != nil {
		return zero, meta, err
	}
	root, err := decodeStrict(raw)
	if err != nil {
		return zero, meta, ErrUncertain
	}
	obj, ok := root.(map[string]any)
	if !ok {
		return zero, meta, ErrUncertain
	}
	o := jsonObject(obj)
	id, _, okID := o.str("id")
	kind, _, okKind := o.str("object")
	if !okID || !okKind || kind != "account" || !accountPattern.MatchString(id) {
		return zero, meta, ErrUncertain
	}
	if id != c.cfg.AccountID {
		return zero, meta, refuse(ErrAuthentication, "stripe_account_mismatch")
	}
	r, ok := projectReadiness(o)
	if !ok {
		return zero, meta, ErrUncertain
	}
	return r, meta, nil
}

// projectReadiness reads only the listed paths. ok=false means a listed field had the wrong
// JSON type (a malformed or hostile body), which is never a readiness answer.
func projectReadiness(o jsonObject) (Readiness, bool) {
	var r Readiness
	boolField := func(parent jsonObject, key string) (*bool, bool) {
		v, present, ok := parent.boolean(key)
		if !ok {
			return nil, false
		}
		if !present {
			return nil, true
		}
		return &v, true
	}
	var ok bool
	if r.ChargesEnabled, ok = boolField(o, "charges_enabled"); !ok {
		return Readiness{}, false
	}
	if r.PayoutsEnabled, ok = boolField(o, "payouts_enabled"); !ok {
		return Readiness{}, false
	}
	if r.DetailsSubmitted, ok = boolField(o, "details_submitted"); !ok {
		return Readiness{}, false
	}

	// requirements.currently_due: `[]` is a real zero; a missing/null requirements object or
	// currently_due stays nil (fails closed downstream).
	reqs, present, ok := o.object("requirements")
	if !ok {
		return Readiness{}, false
	}
	if present {
		if raw, exists := reqs["currently_due"]; exists && raw != nil {
			arr, isArr := raw.([]any)
			if !isArr {
				return Readiness{}, false
			}
			n := len(arr)
			r.CurrentlyDueCount = &n
		}
	}

	settings, present, ok := o.object("settings")
	if !ok {
		return Readiness{}, false
	}
	if !present {
		return r, true
	}
	if pay, present, ok := settings.object("payments"); !ok {
		return Readiness{}, false
	} else if present {
		desc, present, ok := pay.str("statement_descriptor")
		if !ok {
			return Readiness{}, false
		}
		if present {
			n := utf8.RuneCountInString(desc)
			r.DescriptorLength = &n
		}
	}
	card, present, ok := settings.object("card_payments")
	if !ok {
		return Readiness{}, false
	}
	if present {
		// L7: no prefix set (null or "") is 0, but only when card_payments itself was returned.
		prefix, _, ok := card.str("statement_descriptor_prefix")
		if !ok {
			return Readiness{}, false
		}
		n := utf8.RuneCountInString(prefix)
		r.PrefixLength = &n
		decline, present, ok := card.object("decline_on")
		if !ok {
			return Readiness{}, false
		}
		if present {
			if r.CVCRule, ok = boolField(decline, "cvc_failure"); !ok {
				return Readiness{}, false
			}
			if r.AVSRule, ok = boolField(decline, "avs_failure"); !ok {
				return Readiness{}, false
			}
		}
	}
	return r, true
}
