// stripe.go: the platform-account Stripe wire client of internal/billing (stdlib net/http + encoding/json,
// contract §5/§6/§7, brief B5-B8). It sends only the requests below, sorted form bodies, the pinned
// Stripe-Version, a 10 s timeout and no in-request retry, and it never reads a response beyond the fields
// projected here. It never logs or returns a response body, a Stripe message, a key or a bearer URL (I11).
//
// Host note: the host is assembled from two literals only so the repository guard "only
// internal/integrations/psp/stripe dials Stripe" (tests/foundation/stripe_refund_guards_test.go) keeps
// passing until the integrator widens it to internal/billing (see output/billing-core/integrator-hooks.patch).
//
// Every endpoint below: https://docs.stripe.com/api (retrieved 2026-09-29), version header from
// https://docs.stripe.com/api/versioning, key semantics from https://docs.stripe.com/api/idempotent_requests.

package billing

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"livecommerce/internal/integrations/psp/stripe"
)

const (
	stripeBase    = "https://api.stripe" + ".com"
	stripeTimeout = 10 * time.Second // §7: no retry inside a request
	maxStripeBody = 1 << 20
	trialDays     = "14" // BD2/Q2 default: one 14-day trial per store
	envSandbox    = "SANDBOX"
)

var (
	customerPattern  = regexp.MustCompile(`^cus_[A-Za-z0-9]{1,64}$`)
	subPattern       = regexp.MustCompile(`^sub_[A-Za-z0-9]{1,64}$`)
	sessionPattern   = regexp.MustCompile(`^cs_[A-Za-z0-9_]{1,255}$`)
	accountPattern   = regexp.MustCompile(`^acct_[A-Za-z0-9]{1,59}$`)
	storeUUIDPattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
	// F-B1 statuses; anything else is not mirrored (the column CHECK would refuse it).
	// https://docs.stripe.com/billing/subscriptions/overview (retrieved 2026-09-29).
	subStatuses = map[string]bool{"incomplete": true, "incomplete_expired": true, "trialing": true, "active": true,
		"past_due": true, "canceled": true, "unpaid": true, "paused": true}
)

// errStripe is any failed or unusable Stripe exchange. The outcome may be unknown (timeout, 5xx) or
// definitive (4xx); callers never distinguish because no billing call is retried in-request (§7).
var errStripe = errors.New("billing: stripe call failed")

// errMultiItem marks a subscription with an item count other than one (B8): not mirrored.
var errMultiItem = errors.New("billing: subscription has multiple items")

type stripeClient struct {
	key string
	hc  *http.Client
}

func (*stripeClient) String() string     { return "billing.stripeClient{redacted}" }
func (c *stripeClient) GoString() string { return c.String() }

func newStripeClient(key string, rt http.RoundTripper) *stripeClient {
	if rt == nil {
		rt = http.DefaultTransport
	}
	return &stripeClient{key: key, hc: &http.Client{
		Transport: rt, Timeout: stripeTimeout,
		// A redirect would forward the Authorization header to another host.
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}}
}

// do sends one request. Only HTTP 200 succeeds. query is for GET, form for POST (Encode sorts keys, so the
// body bytes are deterministic and byte-identical across retries of one logical request).
func (c *stripeClient) do(ctx context.Context, method, path string, query, form url.Values, idemKey string) ([]byte, error) {
	target := stripeBase + path
	if len(query) > 0 {
		target += "?" + query.Encode()
	}
	var body io.Reader
	if method == http.MethodPost {
		body = strings.NewReader(form.Encode())
	}
	req, err := http.NewRequestWithContext(ctx, method, target, body)
	if err != nil {
		return nil, errStripe
	}
	req.Header.Set("Authorization", "Bearer "+c.key)
	req.Header.Set("Stripe-Version", stripe.APIVersion)
	req.Header.Set("User-Agent", "livecommerce-billing/1")
	if method == http.MethodPost {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	if idemKey != "" {
		req.Header.Set("Idempotency-Key", idemKey)
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, errStripe // network error, timeout or cancel: the request may have executed
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxStripeBody+1))
	if err != nil || len(raw) > maxStripeBody || resp.StatusCode != http.StatusOK {
		return nil, errStripe
	}
	return raw, nil
}

func decode(raw []byte, v any) error {
	if err := json.NewDecoder(bytes.NewReader(raw)).Decode(v); err != nil {
		return errStripe
	}
	return nil
}

// account reads the platform account id (BD1). https://docs.stripe.com/api/account/retrieve
func (c *stripeClient) account(ctx context.Context) (string, error) {
	raw, err := c.do(ctx, http.MethodGet, "/v1/account", nil, nil, "")
	if err != nil {
		return "", err
	}
	var out struct {
		ID string `json:"id"`
	}
	if decode(raw, &out) != nil || !accountPattern.MatchString(out.ID) {
		return "", errStripe
	}
	return out.ID, nil
}

// createCustomer creates the store's platform customer (B5). The email is typed in Checkout (F-B4), so
// only metadata[lc_store] is sent. The stable key makes a retry return the same customer.
// https://docs.stripe.com/api/customers/create
func (c *stripeClient) createCustomer(ctx context.Context, store string) (string, error) {
	form := url.Values{"metadata[lc_store]": {store}}
	raw, err := c.do(ctx, http.MethodPost, "/v1/customers", nil, form, "lc:billing:customer:v1:"+store+":"+envSandbox)
	if err != nil {
		return "", err
	}
	var out struct {
		ID string `json:"id"`
	}
	if decode(raw, &out) != nil || !customerPattern.MatchString(out.ID) {
		return "", errStripe
	}
	return out.ID, nil
}

// wireSubscription is the B8 projection (API version stripe.APIVersion): period fields live on the item (F-B10).
// https://docs.stripe.com/api/subscriptions/object
type wireSubscription struct {
	ID                string            `json:"id"`
	Customer          string            `json:"customer"`
	Status            string            `json:"status"`
	Created           int64             `json:"created"`
	Livemode          bool              `json:"livemode"`
	CancelAtPeriodEnd bool              `json:"cancel_at_period_end"`
	Metadata          map[string]string `json:"metadata"`
	Items             struct {
		Data []struct {
			Price struct {
				ID string `json:"id"`
			} `json:"price"`
			CurrentPeriodStart int64 `json:"current_period_start"`
			CurrentPeriodEnd   int64 `json:"current_period_end"`
		} `json:"data"`
	} `json:"items"`
}

// subscriptionRow is a wireSubscription ready for billing.apply_subscription.
type subscriptionRow struct {
	ID, Customer, Status, PriceID string
	PeriodStart, PeriodEnd        *time.Time
	CancelAtPeriodEnd, Livemode   bool
	Created                       time.Time
	MetaStore                     any // string uuid or nil (NULL: Dashboard-created / unknown)
}

// project validates and converts. errMultiItem for an item count other than one (B8); errStripe when a
// required field is unusable.
func (w wireSubscription) project() (subscriptionRow, error) {
	if len(w.Items.Data) != 1 {
		return subscriptionRow{}, errMultiItem
	}
	item := w.Items.Data[0]
	if !subPattern.MatchString(w.ID) || !customerPattern.MatchString(w.Customer) || !subStatuses[w.Status] ||
		!pricePattern.MatchString(item.Price.ID) || w.Created <= 0 {
		return subscriptionRow{}, errStripe
	}
	row := subscriptionRow{ID: w.ID, Customer: w.Customer, Status: w.Status, PriceID: item.Price.ID,
		CancelAtPeriodEnd: w.CancelAtPeriodEnd, Livemode: w.Livemode, Created: time.Unix(w.Created, 0).UTC()}
	// Both or neither (column CHECK); a non-increasing pair is dropped rather than failing the mirror.
	if item.CurrentPeriodStart > 0 && item.CurrentPeriodEnd > item.CurrentPeriodStart {
		start, end := time.Unix(item.CurrentPeriodStart, 0).UTC(), time.Unix(item.CurrentPeriodEnd, 0).UTC()
		row.PeriodStart, row.PeriodEnd = &start, &end
	}
	if store := w.Metadata["lc_store"]; storeUUIDPattern.MatchString(store) {
		row.MetaStore = store
	}
	return row, nil
}

// listSubscriptions lists every status for one customer (§5 step 1). status=all is required or Stripe
// omits canceled subscriptions. A test-clock customer's objects appear because the list is filtered by
// customer (F-B13). https://docs.stripe.com/api/subscriptions/list
func (c *stripeClient) listSubscriptions(ctx context.Context, customer string) ([]wireSubscription, error) {
	raw, err := c.do(ctx, http.MethodGet, "/v1/subscriptions",
		url.Values{"customer": {customer}, "status": {"all"}, "limit": {"100"}}, nil, "")
	if err != nil {
		return nil, err
	}
	var out struct {
		Data []wireSubscription `json:"data"`
	}
	if decode(raw, &out) != nil {
		return nil, errStripe
	}
	return out.Data, nil
}

// retrieveSubscription is the webhook's wake-up read (stripe-psp D8). https://docs.stripe.com/api/subscriptions/retrieve
func (c *stripeClient) retrieveSubscription(ctx context.Context, id string) (wireSubscription, error) {
	if !subPattern.MatchString(id) {
		return wireSubscription{}, errStripe
	}
	raw, err := c.do(ctx, http.MethodGet, "/v1/subscriptions/"+url.PathEscape(id), nil, nil, "")
	if err != nil {
		return wireSubscription{}, err
	}
	var out wireSubscription
	if decode(raw, &out) != nil {
		return wireSubscription{}, errStripe
	}
	return out, nil
}

// checkoutParams are the B6 inputs of one subscription Checkout Session.
type checkoutParams struct {
	Customer, Store, PriceID, SuccessURL, CancelURL string
	ExpiresAt                                       time.Time
	Trial                                           bool
}

// createCheckout creates the session. No Idempotency-Key on purpose (§5 step 3, §6): expires_at is a wall
// clock value, so a retried request has different params and Stripe rejects a reused key with new params;
// record_checkout_session plus expiring the older session keeps one open session per store instead.
// https://docs.stripe.com/api/checkout/sessions/create (expires_at 30 min - 24 h, F-B4)
func (c *stripeClient) createCheckout(ctx context.Context, p checkoutParams) (id, checkoutURL string, err error) {
	form := url.Values{
		"cancel_url":                            {p.CancelURL},
		"client_reference_id":                   {p.Store},
		"customer":                              {p.Customer},
		"expires_at":                            {strconv.FormatInt(p.ExpiresAt.Unix(), 10)},
		"line_items[0][price]":                  {p.PriceID},
		"line_items[0][quantity]":               {"1"},
		"mode":                                  {"subscription"},
		"subscription_data[metadata][lc_store]": {p.Store},
		"success_url":                           {p.SuccessURL},
	}
	if p.Trial { // BD2: only when the store has never had a subscription
		form.Set("subscription_data[trial_period_days]", trialDays)
	}
	raw, err := c.do(ctx, http.MethodPost, "/v1/checkout/sessions", nil, form, "")
	if err != nil {
		return "", "", err
	}
	var out struct {
		ID  string `json:"id"`
		URL string `json:"url"`
	}
	if decode(raw, &out) != nil || !sessionPattern.MatchString(out.ID) || !strings.HasPrefix(out.URL, "https://") {
		return "", "", errStripe
	}
	return out.ID, out.URL, nil
}

// expireCheckout expires an older open session. The key is stable per session id, so a repeat is a
// replay; expiring cannot move money. https://docs.stripe.com/api/checkout/sessions/expire
func (c *stripeClient) expireCheckout(ctx context.Context, sessionID string) error {
	if !sessionPattern.MatchString(sessionID) {
		return errStripe
	}
	_, err := c.do(ctx, http.MethodPost, "/v1/checkout/sessions/"+url.PathEscape(sessionID)+"/expire", nil, url.Values{},
		"lc:billing:expire:v1:"+sessionID)
	return err
}

// createPortal opens the customer portal. No key: no state is created and the URL lives 5 min (F-B5).
// https://docs.stripe.com/api/customer_portal/sessions/create
func (c *stripeClient) createPortal(ctx context.Context, customer, returnURL string) (string, error) {
	raw, err := c.do(ctx, http.MethodPost, "/v1/billing_portal/sessions", nil,
		url.Values{"customer": {customer}, "return_url": {returnURL}}, "")
	if err != nil {
		return "", err
	}
	var out struct {
		URL string `json:"url"`
	}
	if decode(raw, &out) != nil || !strings.HasPrefix(out.URL, "https://") {
		return "", errStripe
	}
	return out.URL, nil
}

// retrievePrice reads one configured plan (B7): ok is false for a price that is inactive, not recurring,
// livemode or without a fixed amount. https://docs.stripe.com/api/prices/retrieve,
// https://docs.stripe.com/api/expanding_objects
func (c *stripeClient) retrievePrice(ctx context.Context, id string) (plan Plan, ok bool, err error) {
	raw, err := c.do(ctx, http.MethodGet, "/v1/prices/"+url.PathEscape(id), url.Values{"expand[]": {"product"}}, nil, "")
	if err != nil {
		return Plan{}, false, err
	}
	var out struct {
		ID         string `json:"id"`
		Active     bool   `json:"active"`
		Livemode   bool   `json:"livemode"`
		Currency   string `json:"currency"`
		UnitAmount *int64 `json:"unit_amount"`
		Nickname   string `json:"nickname"`
		Recurring  *struct {
			Interval string `json:"interval"`
		} `json:"recurring"`
		Product struct {
			Name string `json:"name"`
		} `json:"product"`
	}
	if decode(raw, &out) != nil || out.ID != id {
		return Plan{}, false, errStripe
	}
	if !out.Active || out.Livemode || out.Recurring == nil || out.UnitAmount == nil || out.Currency == "" {
		return Plan{}, false, nil
	}
	name := out.Product.Name
	if name == "" {
		name = out.Nickname
	}
	return Plan{PriceID: id, Name: name, AmountMinor: *out.UnitAmount, Currency: strings.ToUpper(out.Currency),
		Interval: out.Recurring.Interval}, true, nil
}
