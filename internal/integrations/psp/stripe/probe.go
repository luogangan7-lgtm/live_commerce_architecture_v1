// probe.go: the registrar's SANDBOX qualification probe (contracts/stripe-psp-v1.md §13,
// integrator ruling 7 of docs/delivery/units/stripe-b1-rulings.md). It creates one throwaway
// Checkout Session tagged metadata[lc_probe]=1, expires it at once and requires the
// retrieved copy to be expired + unpaid + livemode equal to the client's environment (false in
// SANDBOX, true on a LIVE-admitted client; stripe-live-enable-v1 §5.1). It never reads PG and
// never touches an order. A LIVE probe creates and expires a session that nobody completes,
// so it cannot charge anyone.
//
// Ownership: integration_worker. Dependencies: client.go call/classify, params.go patterns.
// Callers: internal/payments/stripeadmin (Qualify) only. The webhook handler ignores the
// resulting checkout.session.* events because metadata.lc_probe marks them (§9.1 probe_session).

package stripe

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

// probeLifetime: expires_at must be 30 min..24 h after creation, so 31 min is the smallest
// safe value with one minute of clock slack.
// https://docs.stripe.com/api/checkout/sessions/create#create_checkout_session-expires_at
// (retrieved 2026-09-28, F1).
const probeLifetime = 31 * time.Minute

// ProbeCheckout creates, expires and retrieves one probe session for the qualification and
// returns its id. The create key is "lc:stripe:probe:v1:<qualification>" and the expire key
// "lc:stripe:probe-expire:v1:<qualification>". Why a retry cannot reuse the create key: the
// body embeds expires_at (now+31m), so a second run with the same qualification id would fail
// Stripe's parameter comparison with ErrIdempotency; the operator must mint a new
// qualification id instead (nothing was qualified by a failed probe).
// https://docs.stripe.com/api/checkout/sessions/expire (retrieved 2026-09-28, F2).
func (c *Client) ProbeCheckout(ctx context.Context, qualificationID, currency string,
	amountMinor int64, returnURL string) (string, CallMeta, error) {
	return c.probeCheckout(ctx, time.Now(), qualificationID, currency, amountMinor, returnURL)
}

func (c *Client) probeCheckout(ctx context.Context, now time.Time, qualificationID, currency string,
	amountMinor int64, returnURL string) (string, CallMeta, error) {
	var meta CallMeta
	// A LIVE client only exists if admit() accepted the flag+ref pair (New), so no extra LIVE
	// refusal is needed here; the livemode check below binds the evidence to the environment.
	if c == nil || ctx == nil {
		return "", meta, ErrInvalid
	}
	if !uuidPattern.MatchString(qualificationID) || !validReturnURL(returnURL) {
		return "", meta, ErrInvalid
	}
	// I05: the probe amount must be an exactly admissible amount for the store currency.
	if unit, err := UnitAmount(strings.ToUpper(currency), amountMinor); err != nil || unit != amountMinor {
		return "", meta, ErrInvalid
	}
	body := encodeProbeBody(qualificationID, strings.ToLower(currency), amountMinor, returnURL,
		now.Add(probeLifetime).Unix())
	raw, meta, err := c.call(ctx, opCreate, http.MethodPost, "/v1/checkout/sessions", body,
		"lc:stripe:probe:v1:"+qualificationID)
	if err != nil {
		return "", meta, err
	}
	created, err := c.decodeSession(raw)
	if err != nil {
		return "", meta, err
	}
	// Expire always follows a successful create; ErrNotOpen is tolerated because the retrieve
	// below is the only evidence we accept.
	if _, meta, err = c.call(ctx, opExpire, http.MethodPost, "/v1/checkout/sessions/"+created.ID+"/expire",
		nil, "lc:stripe:probe-expire:v1:"+qualificationID); err != nil && !errors.Is(err, ErrNotOpen) {
		return "", meta, err
	}
	s, meta, err := c.RetrieveCheckoutSession(ctx, created.ID)
	if err != nil {
		return "", meta, err
	}
	// LD1: a LIVE probe must come back livemode=true, a SANDBOX one livemode=false.
	if s.Status != "expired" || s.PaymentStatus != "unpaid" || s.Livemode != c.wantLivemode() {
		return "", meta, refuse(ErrRejected, "stripe_probe_not_expired")
	}
	return s.ID, meta, nil
}

// encodeProbeBody keeps the production flags (§5.4: card only, hosted page, no adaptive
// pricing/managed payments/tax) so the probe exercises the same account configuration.
// client_reference_id carries the qualification id (never an attempt id, so the webhook
// mapper cannot confuse it; metadata.lc_probe short-circuits it first anyway).
func encodeProbeBody(qualificationID, currency string, amountMinor int64, returnURL string, expiresAt int64) []byte {
	f := map[string]string{
		pAdaptivePricing: "false", pManagedPayments: "false", pAutomaticTax: "false",
		pCancelURL: returnURL, pSuccessURL: returnURL, pClientRef: qualificationID,
		pExpiresAt: strconv.FormatInt(expiresAt, 10), pCurrency: currency,
		pProductName: "Stripe probe", pUnitAmount: strconv.FormatInt(amountMinor, 10),
		pQuantity: "1", "metadata[lc_probe]": "1", pMode: "payment", pMethodTypes: "card",
		pSubmitType: "pay", pUIMode: "hosted_page",
	}
	keys := make([]string, 0, len(f))
	for k := range f {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	for i, k := range keys {
		if i > 0 {
			b.WriteByte('&')
		}
		b.WriteString(url.QueryEscape(k) + "=" + url.QueryEscape(f[k]))
	}
	return []byte(b.String())
}
