package billingtest

// Self-test of the fake (CB08 prerequisite): each documented behaviour is asserted so a drift of the fake
// cannot silently weaken a CB08 assertion. Named TestFake* (not TestCustomersBillingCB*).

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"
)

type fx struct {
	t *testing.T
	s *Server
	c *http.Client
}

func newFx(t *testing.T) *fx {
	s := New("acct_FakePlatform0001")
	t.Cleanup(s.Close)
	return &fx{t: t, s: s, c: &http.Client{Transport: s.Transport(), Timeout: 5 * time.Second}}
}

func (f *fx) do(method, path string, form url.Values, hdr map[string]string) (int, map[string]any, http.Header) {
	f.t.Helper()
	var body io.Reader
	if form != nil {
		body = strings.NewReader(form.Encode())
	}
	req, err := http.NewRequest(method, "https://api.stripe.com"+path, body)
	if err != nil {
		f.t.Fatal(err)
	}
	if form != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	res, err := f.c.Do(req)
	if err != nil {
		f.t.Fatalf("%s %s: %v", method, path, err)
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(res.Body)
	var out map[string]any
	_ = json.Unmarshal(raw, &out)
	return res.StatusCode, out, res.Header
}

func exp(min int) string {
	return strconv.FormatInt(time.Now().Add(time.Duration(min)*time.Minute).Unix(), 10)
}

func checkoutForm(cust string, expires string) url.Values {
	return url.Values{"mode": {"subscription"}, "customer": {cust}, "line_items[0][price]": {"price_Fake1"}, "line_items[0][quantity]": {"1"},
		"success_url": {"https://admin.example.test/billing"}, "cancel_url": {"https://admin.example.test/billing"},
		"client_reference_id": {"store-1"}, "expires_at": {expires}}
}

func TestFakeAccountAndAuth(t *testing.T) {
	f := newFx(t)
	if st, out, _ := f.do("GET", "/v1/account", nil, nil); st != 200 || out["id"] != "acct_FakePlatform0001" {
		t.Fatalf("account: %d %v", st, out)
	}
	f.s.RequireKey("sk_" + "test_fake")
	if st, _, _ := f.do("GET", "/v1/account", nil, nil); st != 401 {
		t.Fatalf("no key: %d", st)
	}
	if st, _, _ := f.do("GET", "/v1/account", nil, map[string]string{"Authorization": "Bearer sk_" + "test_fake"}); st != 200 {
		t.Fatalf("with key: %d", st)
	}
	f.s.SetAccount("acct_Other")
	if _, out, _ := f.do("GET", "/v1/account", nil, map[string]string{"Authorization": "Bearer sk_" + "test_fake"}); out["id"] != "acct_Other" {
		t.Fatalf("SetAccount: %v", out)
	}
	for _, c := range f.s.Calls() {
		if strings.Contains(c.KeyFingerprint, "test_fake") {
			t.Fatal("captured call leaks the key")
		}
	}
}

func TestFakeCustomerIdempotency(t *testing.T) {
	f := newFx(t)
	form := url.Values{"metadata[lc_store]": {"s1"}}
	st1, a, _ := f.do("POST", "/v1/customers", form, map[string]string{"Idempotency-Key": "k1"})
	st2, b, h := f.do("POST", "/v1/customers", form, map[string]string{"Idempotency-Key": "k1"})
	if st1 != 200 || st2 != 200 || a["id"] != b["id"] || h.Get("Idempotent-Replayed") != "true" || len(f.s.Customers()) != 1 {
		t.Fatalf("replay: %d %d %v %v %v", st1, st2, a, b, f.s.Customers())
	}
	if st, out, _ := f.do("POST", "/v1/customers", url.Values{"metadata[lc_store]": {"s2"}}, map[string]string{"Idempotency-Key": "k1"}); st != 400 || out["error"].(map[string]any)["type"] != "idempotency_error" {
		t.Fatalf("same key different body: %d %v", st, out)
	}
	if st, c, _ := f.do("POST", "/v1/customers", form, nil); st != 200 || c["id"] == a["id"] || len(f.s.Customers()) != 2 {
		t.Fatal("no key must create a second customer")
	}
}

func TestFakeCheckoutValidationAndExpire(t *testing.T) {
	f := newFx(t)
	cus := f.s.AddCustomer(nil)
	for name, form := range map[string]url.Values{
		"one-minute expiry": checkoutForm(cus, exp(1)),
		"past 24h":          checkoutForm(cus, exp(25*60)),
		"unknown customer":  checkoutForm("cus_Nope", exp(31)),
		"payment mode":      func() url.Values { v := checkoutForm(cus, exp(31)); v.Set("mode", "payment"); return v }(),
		"no line items":     func() url.Values { v := checkoutForm(cus, exp(31)); v.Del("line_items[0][price]"); return v }(),
		"reference too long": func() url.Values {
			v := checkoutForm(cus, exp(31))
			v.Set("client_reference_id", strings.Repeat("x", 201))
			return v
		}(),
	} {
		if st, _, _ := f.do("POST", "/v1/checkout/sessions", form, nil); st != 400 {
			t.Errorf("%s: want 400 got %d", name, st)
		}
	}
	st, s1, _ := f.do("POST", "/v1/checkout/sessions", checkoutForm(cus, exp(31)), nil)
	_, s2, _ := f.do("POST", "/v1/checkout/sessions", checkoutForm(cus, exp(31)), nil)
	if st != 200 || s1["id"] == s2["id"] || f.s.OpenSessions() != 2 || !strings.HasPrefix(s1["url"].(string), "https://checkout.stripe.com/") {
		t.Fatalf("no key => a new session each time: %d %v %v open=%d", st, s1, s2, f.s.OpenSessions())
	}
	id := s1["id"].(string)
	if st, out, _ := f.do("POST", "/v1/checkout/sessions/"+id+"/expire", url.Values{}, map[string]string{"Idempotency-Key": "e1"}); st != 200 || out["status"] != "expired" {
		t.Fatalf("expire: %d %v", st, out)
	}
	if _, _, h := f.do("POST", "/v1/checkout/sessions/"+id+"/expire", url.Values{}, map[string]string{"Idempotency-Key": "e1"}); h.Get("Idempotent-Replayed") != "true" {
		t.Fatal("expire with the same key must replay")
	}
	if st, _, _ := f.do("POST", "/v1/checkout/sessions/"+id+"/expire", url.Values{}, map[string]string{"Idempotency-Key": "e2"}); st != 400 {
		t.Fatalf("expiring an expired session: %d", st)
	}
	if f.s.OpenSessions() != 1 {
		t.Fatalf("open=%d", f.s.OpenSessions())
	}
	// a reused Checkout Idempotency-Key with different params is the failure the contract avoids (§5 step 3)
	k := map[string]string{"Idempotency-Key": "c1"}
	f.do("POST", "/v1/checkout/sessions", checkoutForm(cus, exp(31)), k)
	if st, out, _ := f.do("POST", "/v1/checkout/sessions", checkoutForm(cus, exp(32)), k); st != 400 || out["error"].(map[string]any)["type"] != "idempotency_error" {
		t.Fatalf("reused key with a moved expires_at: %d %v", st, out)
	}
}

func TestFakePortalSubscriptionsPrices(t *testing.T) {
	f := newFx(t)
	cus := f.s.AddCustomer(nil)
	if st, out, _ := f.do("POST", "/v1/billing_portal/sessions", url.Values{"customer": {cus}, "return_url": {"https://admin.example.test/billing"}}, nil); st != 200 || !strings.HasPrefix(out["url"].(string), "https://billing.stripe.com/") {
		t.Fatalf("portal: %d %v", st, out)
	}
	if st, _, _ := f.do("POST", "/v1/billing_portal/sessions", url.Values{"customer": {"cus_Nope"}, "return_url": {"x"}}, nil); st != 400 {
		t.Fatalf("portal unknown customer: %d", st)
	}
	live := f.s.AddSubscription(Sub{Customer: cus, Status: "active", StoreMeta: "s1"})
	f.s.AddSubscription(Sub{Customer: cus, Status: "canceled", StoreMeta: "s1"})
	f.s.AddSubscription(Sub{Customer: "cus_Other", Status: "active"})
	_, def, _ := f.do("GET", "/v1/subscriptions?customer="+cus, nil, nil)
	_, all, _ := f.do("GET", "/v1/subscriptions?customer="+cus+"&status=all", nil, nil)
	if len(def["data"].([]any)) != 1 || len(all["data"].([]any)) != 2 {
		t.Fatalf("list default=%d all=%d (canceled hidden unless status=all)", len(def["data"].([]any)), len(all["data"].([]any)))
	}
	_, got, _ := f.do("GET", "/v1/subscriptions/"+live.ID, nil, nil)
	item := got["items"].(map[string]any)["data"].([]any)[0].(map[string]any)
	if _, top := got["current_period_end"]; top || item["current_period_end"] == nil || got["metadata"].(map[string]any)["lc_store"] != "s1" {
		t.Fatalf("F-B10: period fields must be on the item only: %v", got)
	}
	if st, _, _ := f.do("GET", "/v1/subscriptions/sub_Nope", nil, nil); st != 404 {
		t.Fatalf("unknown sub: %d", st)
	}
	f.s.SetSubStatus(live.ID, "past_due")
	if _, got, _ = f.do("GET", "/v1/subscriptions/"+live.ID, nil, nil); got["status"] != "past_due" {
		t.Fatal("SetSubStatus")
	}
	f.s.ClearSubscriptions()
	if _, all, _ = f.do("GET", "/v1/subscriptions?customer="+cus+"&status=all", nil, nil); len(all["data"].([]any)) != 0 {
		t.Fatal("ClearSubscriptions")
	}
	f.s.AddPrice("price_Fake1", 30000, "twd", "month", "Plan", true, false)
	_, p, _ := f.do("GET", "/v1/prices/price_Fake1?expand[]=product", nil, nil)
	_, q, _ := f.do("GET", "/v1/prices/price_Fake1", nil, nil)
	if _, isObj := p["product"].(map[string]any); !isObj || q["product"] != "prod_Fake1" {
		t.Fatalf("expand[]=product: %v / %v", p["product"], q["product"])
	}
}

func TestFakeFaultsAndCapture(t *testing.T) {
	f := newFx(t)
	f.s.FailNext("account", 503)
	if st, _, _ := f.do("GET", "/v1/account", nil, nil); st != 503 {
		t.Fatalf("FailNext: %d", st)
	}
	if st, _, _ := f.do("GET", "/v1/account", nil, nil); st != 200 {
		t.Fatalf("the fault is one-shot: %d", st)
	}
	f.s.HangNext("retrieve")
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", "https://api.stripe.com/v1/subscriptions/sub_x", nil)
	if _, err := f.c.Do(req); err == nil {
		t.Fatal("HangNext must time the client out")
	}
	form := url.Values{"b": {"2"}, "a": {"1", "0"}}
	f.do("POST", "/v1/customers", form, map[string]string{"Idempotency-Key": "kk", "Stripe-Version": APIVersion})
	c := f.s.CallsExact("POST", "/v1/customers")
	if len(c) != 1 || c[0].SortedBody() != "a=1&a=0&b=2" || c[0].IdempotencyKey != "kk" || c[0].StripeVersion != APIVersion {
		t.Fatalf("capture: %+v", c)
	}
	f.s.ResetCalls()
	if len(f.s.Calls()) != 0 {
		t.Fatal("ResetCalls")
	}
	if _, err := (&http.Client{Transport: f.s.Transport()}).Get("https://example.com/"); err == nil {
		t.Fatal("the transport must refuse every host but api.stripe.com")
	}
}

func TestFakeEvents(t *testing.T) {
	f := newFx(t)
	sub := f.s.AddSubscription(Sub{Customer: "cus_x", StoreMeta: "s1"})
	var ev map[string]any
	body := f.s.SubscriptionEvent("customer.subscription.updated", sub.ID, EventOpts{})
	if err := json.Unmarshal(body, &ev); err != nil || ev["type"] != "customer.subscription.updated" || ev["livemode"] != false ||
		ev["data"].(map[string]any)["object"].(map[string]any)["id"] != sub.ID {
		t.Fatalf("subscription event: %v %v", ev, err)
	}
	inv := map[string]any{}
	_ = json.Unmarshal(f.s.InvoiceEvent("invoice.paid", sub.ID, EventOpts{Livemode: true}), &inv)
	obj := inv["data"].(map[string]any)["object"].(map[string]any)
	if _, old := obj["subscription"]; old || inv["livemode"] != true ||
		obj["parent"].(map[string]any)["subscription_details"].(map[string]any)["subscription"] != sub.ID {
		t.Fatalf("invoice event must carry the subscription only under parent.subscription_details: %v", obj)
	}
	at := time.Now()
	sig := Sign("whsec_"+strings.Repeat("a", 32), body, at)
	if !strings.HasPrefix(sig, "t="+strconv.FormatInt(at.Unix(), 10)+",v1=") {
		t.Fatalf("signature shape: %s", sig)
	}
	if a, b := f.s.RawEvent("x.y", EventOpts{}, nil), f.s.RawEvent("x.y", EventOpts{}, nil); string(a) == string(b) {
		t.Fatal("event ids must be unique")
	}
}

func TestFakeLostResponseAndRawPrice(t *testing.T) {
	f := newFx(t)
	f.s.LoseNext("customer")
	form := url.Values{"metadata[lc_store]": {"s1"}}
	req, _ := http.NewRequest("POST", "https://api.stripe.com/v1/customers", strings.NewReader(form.Encode()))
	req.Header.Set("Idempotency-Key", "lost1")
	if _, err := f.c.Do(req); err == nil {
		t.Fatal("LoseNext must drop the response")
	}
	if n := len(f.s.Customers()); n != 1 {
		t.Fatalf("the effect must have happened: %d customers", n)
	}
	st, out, h := f.do("POST", "/v1/customers", form, map[string]string{"Idempotency-Key": "lost1"})
	if st != 200 || h.Get("Idempotent-Replayed") != "true" || out["id"] != f.s.Customers()[0] || len(f.s.Customers()) != 1 {
		t.Fatalf("a retry with the same key must replay the lost effect: %d %v %v", st, out, f.s.Customers())
	}
	f.s.AddPriceRaw("price_OneTime1", map[string]any{"active": true, "livemode": false, "currency": "twd", "unit_amount": 100, "product": "prod_x"})
	if _, p, _ := f.do("GET", "/v1/prices/price_OneTime1", nil, nil); p["recurring"] != nil || p["id"] != "price_OneTime1" {
		t.Fatalf("raw price: %v", p)
	}
}
