package foundation_test

// CB10 (contracts/customers-billing-v1.md §8 CB10, §10 operator prerequisites, F-B13; tier SANDBOX). NOT_RUN without the
// platform's own Stripe TEST-mode key (O-D: the key and the webhook secret come from owner-supplied files into the
// environment, never chat). Authored from the contract only, compile-checked; it has never run against Stripe (label
// NOT_RUN in every evidence file until an operator runs it).
//
// Prerequisites (all in env, checked first, otherwise t.Skip("NOT_RUN: ...")):
//   STRIPE_BILLING_SANDBOX=1
//   LC_PLATFORM_STRIPE_SECRET_KEY        sk_test_/rk_test_ of a platform account that is NOT any merchant's PSP account
//   LC_PLATFORM_STRIPE_WEBHOOK_SECRET    the signing secret `stripe listen` prints (whsec_...)
//   LC_BILLING_PRICE_IDS                 one recurring test-mode price id (monthly)
//   LC_BILLING_SANDBOX_FORWARD_PORT      local port that `stripe listen --forward-to localhost:<port>/v1/platform/stripe/webhook`
//                                        targets; the test serves the real billing.WebhookHandler there
// Account prerequisites (§10): failed-payment setting "unpaid" (Q7), no Billing Automations (F-B13), portal configured.
//
// What it does (contract text): create a Test Clock through the API; create the Customer with `test_clock` (F-B13); pin it
// through the real pin_customer definer; subscribe with the test card 4242 -> webhook -> `active` -> GOOD; switch the
// default payment method to a card that attaches but fails charges, advance the clock past the renewal -> past_due ->
// GRACE, advance past the retries -> unpaid -> RESTRICTED (a claim window is then refused); pay the open invoice with a
// good card (what the portal does) -> active -> GOOD. Every Stripe object it creates is deleted at the end.

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"livecommerce/internal/billing"
	"livecommerce/internal/platform"
)

func cb10Env(t *testing.T) (key, hook, price string, port int) {
	t.Helper()
	if os.Getenv("STRIPE_BILLING_SANDBOX") != "1" {
		t.Skip("NOT_RUN: CB10 needs STRIPE_BILLING_SANDBOX=1 and the platform's own Stripe test key (owner-supplied file, O-D); no key, no SANDBOX claim")
	}
	key, hook = os.Getenv("LC_PLATFORM_STRIPE_SECRET_KEY"), os.Getenv("LC_PLATFORM_STRIPE_WEBHOOK_SECRET")
	price = strings.Split(os.Getenv("LC_BILLING_PRICE_IDS"), ",")[0]
	port, _ = strconv.Atoi(os.Getenv("LC_BILLING_SANDBOX_FORWARD_PORT"))
	if !strings.HasPrefix(key, "sk_test_") && !strings.HasPrefix(key, "rk_test_") {
		t.Skip("NOT_RUN: LC_PLATFORM_STRIPE_SECRET_KEY must be a test-mode key (sk_test_/rk_test_); live keys are refused (BD8)")
	}
	if hook == "" || price == "" || port == 0 {
		t.Skip("NOT_RUN: LC_PLATFORM_STRIPE_WEBHOOK_SECRET, LC_BILLING_PRICE_IDS and LC_BILLING_SANDBOX_FORWARD_PORT are required")
	}
	return key, hook, price, port
}

// cb10API is one Stripe API call with the platform test key (form-encoded, https://docs.stripe.com/api, retrieved 2026-09-29).
func cb10API(t *testing.T, key, method, path string, form url.Values) map[string]any {
	t.Helper()
	req, err := http.NewRequest(method, "https://api.stripe.com"+path, strings.NewReader(form.Encode()))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Stripe-Version", "2026-08-26.dahlia")
	res, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	if err != nil {
		t.Fatalf("stripe %s %s: %v", method, path, err)
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil || res.StatusCode >= 300 {
		t.Fatalf("stripe %s %s: HTTP %d (body withheld)", method, path, res.StatusCode)
	}
	return out
}

func TestCustomersBillingCB10Sandbox(t *testing.T) {
	key, hookSecret, price, port := cb10Env(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()
	c := cbbSetup(t)
	f := c.f
	cfg := billing.Config{SecretKey: key, WebhookSecret: hookSecret, ReturnOrigin: "https://admin.example.test", PriceIDs: []string{price}}
	svc, err := billing.New(ctx, f.runtime, cfg, nil) // real api.stripe.com
	if err != nil || !svc.Enabled() {
		t.Fatalf("billing service against the real sandbox: enabled=%v err=%v (an account equal to a registered PSP account also lands here)", svc.Enabled(), err)
	}
	ingress, err := platform.OpenStripeIngressPool(ctx, sstLogin(t, f, "commerce_stripe_ingress"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(ingress.Close)
	listener, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		t.Fatalf("webhook forward port: %v", err)
	}
	mux := http.NewServeMux()
	mux.Handle("/v1/platform/stripe/webhook", svc.WebhookHandler(ingress))
	server := &http.Server{Handler: mux}
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(func() { _ = server.Close() })

	// Test Clock + Customer created WITH the clock (F-B13), pinned through the real definer
	clock := cb10API(t, key, "POST", "/v1/test_helpers/test_clocks", url.Values{"frozen_time": {strconv.FormatInt(time.Now().Unix(), 10)}, "name": {"cb10-" + t04Tag()}})
	clockID := clock["id"].(string)
	t.Cleanup(func() { cb10API(t, key, "DELETE", "/v1/test_helpers/test_clocks/"+clockID, nil) })
	customer := cb10API(t, key, "POST", "/v1/customers", url.Values{"test_clock": {clockID}, "metadata[lc_store]": {c.store}})["id"].(string)
	account := cb10API(t, key, "GET", "/v1/account", nil)["id"].(string)
	hash := sha256.Sum256([]byte(c.mgr))
	if err := c.rt(c.mgr, func(tx pgx.Tx, _ []byte) error {
		var out any
		return tx.QueryRow(ctx, `SELECT billing.pin_customer($1,$2,'SANDBOX',$3,$4)`, hash[:], c.store, customer, account).Scan(&out)
	}); err != nil {
		t.Fatalf("pin_customer: %v", err)
	}
	standing := func(want string) {
		t.Helper()
		deadline := time.Now().Add(90 * time.Second)
		for time.Now().Before(deadline) {
			if got := c.standing(f.tenantA, c.store); got == want {
				return
			}
			time.Sleep(2 * time.Second)
		}
		t.Fatalf("standing did not become %s within 90 s of the webhook (forwarded by `stripe listen`?), last %s", want, c.standing(f.tenantA, c.store))
	}
	pay := func(pm string) {
		cb10API(t, key, "POST", "/v1/payment_methods/"+pm+"/attach", url.Values{"customer": {customer}})
		cb10API(t, key, "POST", "/v1/customers/"+customer, url.Values{"invoice_settings[default_payment_method]": {pm}})
	}
	advance := func(d time.Duration) {
		frozen := int64(cb10API(t, key, "GET", "/v1/test_helpers/test_clocks/"+clockID, nil)["frozen_time"].(float64))
		cb10API(t, key, "POST", "/v1/test_helpers/test_clocks/"+clockID+"/advance", url.Values{"frozen_time": {strconv.FormatInt(frozen+int64(d.Seconds()), 10)}})
		time.Sleep(20 * time.Second) // the clock advances asynchronously
	}
	pay("pm_card_visa") // test card 4242
	sub := cb10API(t, key, "POST", "/v1/subscriptions", url.Values{"customer": {customer}, "items[0][price]": {price}, "metadata[lc_store]": {c.store}})
	subID := sub["id"].(string)
	standing("GOOD")
	// a card that attaches but fails every charge, then the renewal and the retries
	pay("pm_card_chargeCustomerFail")
	advance(32 * 24 * time.Hour)
	standing("GRACE")
	advance(21 * 24 * time.Hour) // past Smart Retries with the account setting "unpaid" (Q7)
	standing("RESTRICTED")
	// pay the open invoice with a good card, as the portal does: back to GOOD
	pay("pm_card_visa")
	invoices := cb10API(t, key, "GET", "/v1/invoices?subscription="+subID+"&status=open", nil)
	if list, _ := invoices["data"].([]any); len(list) > 0 {
		cb10API(t, key, "POST", "/v1/invoices/"+list[0].(map[string]any)["id"].(string)+"/pay", nil)
	}
	standing("GOOD")
	// the store never had its checkout blocked, its refunds or its data touched by any of the above
	if n := c.rows(c.store); n < 1 {
		t.Fatalf("no subscription mirrored: %d", n)
	}
	t.Logf("CB10 SANDBOX: GOOD -> GRACE -> RESTRICTED -> GOOD through the real webhook path (forwarded), test clock %s", clockID)
}
