// billing_test.go covers the pure and wire-level behaviour of internal/billing without PG or a network:
// BD4 standing twin, configuration admission and redaction, the exact form bytes of every Stripe request
// (via an in-test RoundTripper), subscription projection, webhook subscription-id resolution and status
// codes before any database work, plan filtering, and that no bearer URL reaches the logs.
// Real-PG behaviour (CB06-CB09) belongs to the independent test unit customers-billing-tests.

package billing

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"livecommerce/internal/command"
	"livecommerce/internal/integrations/psp/stripe"
	"livecommerce/internal/platform"
)

// Synthetic values: keys are assembled from parts so no key-shaped literal exists (PROCESS §6).
var (
	fakeSecretKey     = "sk_" + "test_" + strings.Repeat("A", 24)
	fakeWebhookSecret = "whsec" + "_" + strings.Repeat("B", 32)
)

const (
	testStore   = "11111111-1111-4111-8111-111111111111"
	testAccount = "acct_1PlatformTest"
	bearerSent  = "https://checkout.example.test/c/pay/cs_test_SENTINEL_BEARER"
)

type roundTrip func(*http.Request) (*http.Response, error)

func (f roundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func reply(status int, body string) *http.Response {
	return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(body)), Header: http.Header{}}
}

// recorder captures every request a client sends and answers from a queue of canned responses.
type recorder struct {
	mu      sync.Mutex
	reqs    []*http.Request
	bodies  []string
	answers []*http.Response
}

func (r *recorder) RoundTrip(req *http.Request) (*http.Response, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var body []byte
	if req.Body != nil {
		body, _ = io.ReadAll(req.Body)
	}
	r.reqs, r.bodies = append(r.reqs, req), append(r.bodies, string(body))
	if len(r.answers) == 0 {
		return reply(500, `{}`), nil
	}
	a := r.answers[0]
	r.answers = r.answers[1:]
	return a, nil
}

func TestStandingOfMatchesBD4(t *testing.T) {
	for _, tc := range []struct {
		name     string
		statuses []string
		want     Standing
	}{
		{"none", nil, Unbilled},
		{"empty", []string{}, Unbilled},
		{"trialing", []string{"trialing"}, Good},
		{"active", []string{"active"}, Good},
		{"past_due", []string{"past_due"}, Grace},
		{"unpaid", []string{"unpaid"}, Restricted},
		{"canceled", []string{"canceled"}, Restricted},
		{"paused", []string{"paused"}, Restricted},
		{"incomplete alone never counts", []string{"incomplete"}, Unbilled},
		{"incomplete_expired alone never counts", []string{"incomplete_expired"}, Unbilled},
		{"canceled plus incomplete stays restricted (F-B1)", []string{"canceled", "incomplete"}, Restricted},
		{"incomplete cannot lift a restriction", []string{"incomplete", "unpaid", "incomplete_expired"}, Restricted},
		{"best wins: active over canceled", []string{"canceled", "active"}, Good},
		{"best wins: past_due over canceled", []string{"canceled", "past_due", "incomplete"}, Grace},
		{"best wins: trialing over past_due", []string{"past_due", "trialing"}, Good},
		{"unknown status fails closed", []string{"brand_new"}, Restricted},
	} {
		if got := StandingOf(tc.statuses); got != tc.want {
			t.Errorf("%s: StandingOf(%v)=%s want %s", tc.name, tc.statuses, got, tc.want)
		}
	}
}

func env(values map[string]string) func(string) string {
	return func(name string) string { return values[name] }
}

func goodEnv() map[string]string {
	return map[string]string{
		"LC_BILLING_ENABLED":                "1",
		"LC_PLATFORM_STRIPE_SECRET_KEY":     fakeSecretKey,
		"LC_PLATFORM_STRIPE_WEBHOOK_SECRET": fakeWebhookSecret,
		"LC_BILLING_PRICE_IDS":              "price_1Abc, price_2Def",
		"LC_BILLING_RETURN_ORIGIN":          "https://admin.example.test/",
	}
}

func TestLoadConfigDisabledReadsNothingElse(t *testing.T) {
	for _, enabled := range []string{""} {
		cfg, on, err := LoadConfig(func(name string) string {
			if name != "LC_BILLING_ENABLED" {
				t.Fatalf("disabled billing read %s", name)
			}
			return enabled
		})
		if err != nil || on || cfg.SecretKey != "" || cfg.PriceIDs != nil {
			t.Fatalf("cfg=%v on=%v err=%v", cfg, on, err)
		}
	}
	if _, _, err := LoadConfig(nil); !errors.Is(err, ErrConfig) {
		t.Fatalf("nil getenv err=%v", err)
	}
}

func TestLoadConfigEnabled(t *testing.T) {
	cfg, on, err := LoadConfig(env(goodEnv()))
	if err != nil || !on {
		t.Fatalf("on=%v err=%v", on, err)
	}
	if cfg.ReturnOrigin != "https://admin.example.test" || len(cfg.PriceIDs) != 2 || cfg.PriceIDs[1] != "price_2Def" {
		t.Fatalf("cfg fields wrong: origin=%q prices=%v", cfg.ReturnOrigin, cfg.PriceIDs)
	}
	mutate := func(name, value string) map[string]string {
		m := goodEnv()
		m[name] = value
		return m
	}
	many := make([]string, 11)
	for i := range many {
		many[i] = "price_" + strconv.Itoa(i+1)
	}
	for name, m := range map[string]map[string]string{
		"flag other than 1":        mutate("LC_BILLING_ENABLED", "true"),
		"flag 0":                   mutate("LC_BILLING_ENABLED", "0"),
		"live key refused":         mutate("LC_PLATFORM_STRIPE_SECRET_KEY", "sk_"+"live_"+strings.Repeat("A", 24)),
		"restricted live refused":  mutate("LC_PLATFORM_STRIPE_SECRET_KEY", "rk_"+"live_"+strings.Repeat("A", 24)),
		"missing key":              mutate("LC_PLATFORM_STRIPE_SECRET_KEY", ""),
		"short webhook secret":     mutate("LC_PLATFORM_STRIPE_WEBHOOK_SECRET", "whsec"+"_short"),
		"no prices":                mutate("LC_BILLING_PRICE_IDS", ""),
		"duplicate price":          mutate("LC_BILLING_PRICE_IDS", "price_1,price_1"),
		"bad price":                mutate("LC_BILLING_PRICE_IDS", "prod_1"),
		"eleven prices":            mutate("LC_BILLING_PRICE_IDS", strings.Join(many, ",")),
		"http origin":              mutate("LC_BILLING_RETURN_ORIGIN", "http://admin.example.test"),
		"origin with path":         mutate("LC_BILLING_RETURN_ORIGIN", "https://admin.example.test/app"),
		"origin with query":        mutate("LC_BILLING_RETURN_ORIGIN", "https://admin.example.test?x=1"),
		"origin with userinfo":     mutate("LC_BILLING_RETURN_ORIGIN", "https://user@admin.example.test"),
		"origin with fragment":     mutate("LC_BILLING_RETURN_ORIGIN", "https://admin.example.test#f"),
		"origin missing":           mutate("LC_BILLING_RETURN_ORIGIN", ""),
		"origin scheme not http/s": mutate("LC_BILLING_RETURN_ORIGIN", "ftp://admin.example.test"),
	} {
		if _, on, err := LoadConfig(env(m)); !errors.Is(err, ErrConfig) || on {
			t.Errorf("%s: on=%v err=%v", name, on, err)
		}
	}
	// A loopback http origin is the one http admission (local SANDBOX run).
	if cfg, _, err := LoadConfig(env(mutate("LC_BILLING_RETURN_ORIGIN", "http://localhost:3000"))); err != nil || cfg.ReturnOrigin != "http://localhost:3000" {
		t.Fatalf("loopback origin: %v %v", cfg.ReturnOrigin, err)
	}
}

func TestConfigAndClientAreRedactedInEveryFormat(t *testing.T) {
	cfg, _, _ := LoadConfig(env(goodEnv()))
	client := newStripeClient(cfg.SecretKey, nil)
	for _, v := range []any{cfg, &cfg, client} {
		text := fmt.Sprintf("%v|%+v|%#v|%s", v, v, v, v)
		blob, _ := json.Marshal(v)
		for _, leak := range []string{fakeSecretKey, fakeWebhookSecret, strings.Repeat("A", 24), strings.Repeat("B", 32)} {
			if strings.Contains(text, leak) || bytes.Contains(blob, []byte(leak)) {
				t.Fatalf("%T leaks a secret: %q", v, text)
			}
		}
	}
}

func decodeForm(t *testing.T, body string) url.Values {
	t.Helper()
	values, err := url.ParseQuery(body)
	if err != nil {
		t.Fatal(err)
	}
	return values
}

func TestCheckoutBodyBytesAndHeaders(t *testing.T) {
	rec := &recorder{answers: []*http.Response{
		reply(200, `{"id":"cs_test_a1","url":"`+bearerSent+`"}`),
		reply(200, `{"id":"cs_test_a2","url":"`+bearerSent+`"}`),
	}}
	c := newStripeClient(fakeSecretKey, rec)
	expires := time.Unix(1_900_000_000, 0)
	p := checkoutParams{Customer: "cus_1", Store: testStore, PriceID: "price_1Abc", ExpiresAt: expires,
		SuccessURL: "https://admin.example.test/billing?store=" + testStore + "&checkout=done",
		CancelURL:  "https://admin.example.test/billing?store=" + testStore + "&checkout=cancel"}
	if id, u, err := c.createCheckout(context.Background(), p); err != nil || id != "cs_test_a1" || u != bearerSent {
		t.Fatalf("id=%q err=%v", id, err)
	}
	p.Trial = true
	if _, _, err := c.createCheckout(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	// B6: sorted keys, no trial by default, trial_period_days=14 only under BD2.
	noTrial := "cancel_url=https%3A%2F%2Fadmin.example.test%2Fbilling%3Fstore%3D" + testStore + "%26checkout%3Dcancel" +
		"&client_reference_id=" + testStore + "&customer=cus_1&expires_at=1900000000" +
		"&line_items%5B0%5D%5Bprice%5D=price_1Abc&line_items%5B0%5D%5Bquantity%5D=1&mode=subscription" +
		"&subscription_data%5Bmetadata%5D%5Blc_store%5D=" + testStore +
		"&success_url=https%3A%2F%2Fadmin.example.test%2Fbilling%3Fstore%3D" + testStore + "%26checkout%3Ddone"
	if rec.bodies[0] != noTrial {
		t.Fatalf("checkout body bytes:\n got %s\nwant %s", rec.bodies[0], noTrial)
	}
	withTrial := strings.Replace(noTrial, "&subscription_data%5Bmetadata%5D", "&subscription_data%5Bmetadata%5D", 1)
	withTrial = strings.Replace(withTrial, "&success_url=", "&subscription_data%5Btrial_period_days%5D=14&success_url=", 1)
	if rec.bodies[1] != withTrial {
		t.Fatalf("trial body bytes:\n got %s\nwant %s", rec.bodies[1], withTrial)
	}
	for _, req := range rec.reqs {
		if req.Method != http.MethodPost || req.URL.String() != stripeBase+"/v1/checkout/sessions" {
			t.Fatalf("request %s %s", req.Method, req.URL)
		}
		if req.Header.Get("Idempotency-Key") != "" { // §5 step 3: expires_at is wall clock, no key
			t.Fatal("checkout create must not send an Idempotency-Key")
		}
		if req.Header.Get("Stripe-Version") != stripe.APIVersion || req.Header.Get("Authorization") != "Bearer "+fakeSecretKey ||
			req.Header.Get("Content-Type") != "application/x-www-form-urlencoded" {
			t.Fatalf("headers %v", req.Header)
		}
	}
}

func TestCustomerAndExpireKeysAreStableAndBodiesIdentical(t *testing.T) {
	rec := &recorder{answers: []*http.Response{
		reply(200, `{"id":"cus_9"}`), reply(200, `{"id":"cus_9"}`), reply(200, `{}`), reply(200, `{}`)}}
	c := newStripeClient(fakeSecretKey, rec)
	for i := 0; i < 2; i++ {
		if id, err := c.createCustomer(context.Background(), testStore); err != nil || id != "cus_9" {
			t.Fatalf("id=%q err=%v", id, err)
		}
	}
	for i := 0; i < 2; i++ {
		if err := c.expireCheckout(context.Background(), "cs_test_old1"); err != nil {
			t.Fatal(err)
		}
	}
	wantKey := "lc:billing:customer:v1:" + testStore + ":SANDBOX"
	if rec.bodies[0] != rec.bodies[1] || rec.bodies[0] != "metadata%5Blc_store%5D="+testStore ||
		rec.reqs[0].Header.Get("Idempotency-Key") != wantKey || rec.reqs[1].Header.Get("Idempotency-Key") != wantKey {
		t.Fatalf("customer retries differ: %q %q keys %q", rec.bodies[0], rec.bodies[1], rec.reqs[0].Header.Get("Idempotency-Key"))
	}
	if rec.reqs[2].URL.Path != "/v1/checkout/sessions/cs_test_old1/expire" ||
		rec.reqs[2].Header.Get("Idempotency-Key") != "lc:billing:expire:v1:cs_test_old1" ||
		rec.reqs[3].Header.Get("Idempotency-Key") != rec.reqs[2].Header.Get("Idempotency-Key") || rec.bodies[2] != rec.bodies[3] {
		t.Fatal("expire key or body is not stable")
	}
}

func TestPortalPricesAndSubscriptionRequests(t *testing.T) {
	rec := &recorder{answers: []*http.Response{
		reply(200, `{"url":"`+bearerSent+`"}`),
		reply(200, `{"data":[],"has_more":false}`),
		reply(200, `{"id":"sub_1","customer":"cus_1","status":"active","created":1,"livemode":false,"items":{"data":[]}}`),
	}}
	c := newStripeClient(fakeSecretKey, rec)
	if u, err := c.createPortal(context.Background(), "cus_1", "https://admin.example.test/billing?store="+testStore); err != nil || u != bearerSent {
		t.Fatalf("portal %q %v", u, err)
	}
	if rec.bodies[0] != "customer=cus_1&return_url=https%3A%2F%2Fadmin.example.test%2Fbilling%3Fstore%3D"+testStore ||
		rec.reqs[0].Header.Get("Idempotency-Key") != "" {
		t.Fatalf("portal body %q", rec.bodies[0])
	}
	if subs, err := c.listSubscriptions(context.Background(), "cus_1"); err != nil || len(subs) != 0 {
		t.Fatalf("list %v %v", subs, err)
	}
	if got := rec.reqs[1].URL.RawQuery; got != "customer=cus_1&limit=100&status=all" || rec.reqs[1].Method != http.MethodGet {
		t.Fatalf("list query %q", got)
	}
	if _, err := c.retrieveSubscription(context.Background(), "sub_1"); err != nil || rec.reqs[2].URL.Path != "/v1/subscriptions/sub_1" {
		t.Fatalf("retrieve %v %s", err, rec.reqs[2].URL)
	}
	if _, err := c.retrieveSubscription(context.Background(), "sub_1/../x"); !errors.Is(err, errStripe) {
		t.Fatalf("unsafe id must be refused, got %v", err)
	}
}

func TestStripeFailuresAreOpaque(t *testing.T) {
	for _, status := range []int{400, 401, 404, 429, 500, 503} {
		c := newStripeClient(fakeSecretKey, roundTrip(func(*http.Request) (*http.Response, error) {
			return reply(status, `{"error":{"message":"card 4242 `+bearerSent+` sk_secret_detail"}}`), nil
		}))
		_, _, err := c.createCheckout(context.Background(), checkoutParams{Customer: "cus_1", Store: testStore, PriceID: "price_1", ExpiresAt: time.Now()})
		if !errors.Is(err, errStripe) || strings.Contains(err.Error(), "4242") || strings.Contains(err.Error(), "checkout.example") {
			t.Fatalf("status %d error %v", status, err)
		}
	}
	// A transport error and a response above the body limit are failures too.
	c := newStripeClient(fakeSecretKey, roundTrip(func(*http.Request) (*http.Response, error) { return nil, errors.New("dial: " + bearerSent) }))
	if _, err := c.account(context.Background()); !errors.Is(err, errStripe) || strings.Contains(err.Error(), "example") {
		t.Fatalf("transport error %v", err)
	}
	big := newStripeClient(fakeSecretKey, roundTrip(func(*http.Request) (*http.Response, error) {
		return reply(200, `{"id":"`+strings.Repeat("a", maxStripeBody)+`"}`), nil
	}))
	if _, err := big.account(context.Background()); !errors.Is(err, errStripe) {
		t.Fatalf("oversized body %v", err)
	}
	// A redirect is never followed (it would replay the Authorization header elsewhere).
	redirect := newStripeClient(fakeSecretKey, roundTrip(func(*http.Request) (*http.Response, error) {
		r := reply(302, ``)
		r.Header.Set("Location", "https://elsewhere.example.test/")
		return r, nil
	}))
	if _, err := redirect.account(context.Background()); !errors.Is(err, errStripe) {
		t.Fatalf("redirect %v", err)
	}
}

func TestSubscriptionProjection(t *testing.T) {
	good := `{"id":"sub_A1","customer":"cus_A1","status":"trialing","created":1700000000,"livemode":false,
	 "cancel_at_period_end":true,"metadata":{"lc_store":"` + testStore + `"},
	 "items":{"data":[{"price":{"id":"price_A1"},"current_period_start":1700000100,"current_period_end":1702592100}]}}`
	var w wireSubscription
	if err := json.Unmarshal([]byte(good), &w); err != nil {
		t.Fatal(err)
	}
	row, err := w.project()
	if err != nil || row.ID != "sub_A1" || row.PriceID != "price_A1" || row.Status != "trialing" || !row.CancelAtPeriodEnd ||
		row.MetaStore != testStore || row.PeriodStart == nil || row.PeriodStart.Unix() != 1700000100 || row.PeriodEnd.Unix() != 1702592100 ||
		row.Created.Unix() != 1700000000 || row.Livemode {
		t.Fatalf("row %+v err %v", row, err)
	}
	cases := map[string]struct {
		mutate func(*wireSubscription)
		want   error
	}{
		"two items":     {func(w *wireSubscription) { w.Items.Data = append(w.Items.Data, w.Items.Data[0]) }, errMultiItem},
		"no items":      {func(w *wireSubscription) { w.Items.Data = nil }, errMultiItem},
		"unknown state": {func(w *wireSubscription) { w.Status = "brand_new" }, errStripe},
		"bad customer":  {func(w *wireSubscription) { w.Customer = "acct_x" }, errStripe},
		"bad price":     {func(w *wireSubscription) { w.Items.Data[0].Price.ID = "prod_1" }, errStripe},
		"no created":    {func(w *wireSubscription) { w.Created = 0 }, errStripe},
	}
	for name, tc := range cases {
		var v wireSubscription
		_ = json.Unmarshal([]byte(good), &v)
		tc.mutate(&v)
		if _, err := v.project(); !errors.Is(err, tc.want) {
			t.Errorf("%s: err=%v want %v", name, err, tc.want)
		}
	}
	// Missing metadata or a non-uuid lc_store projects to NULL (mismatch in SQL); a bad period pair is dropped.
	var v wireSubscription
	_ = json.Unmarshal([]byte(good), &v)
	v.Metadata = map[string]string{"lc_store": "not-a-uuid"}
	v.Items.Data[0].CurrentPeriodEnd = v.Items.Data[0].CurrentPeriodStart
	row, err = v.project()
	if err != nil || row.MetaStore != nil || row.PeriodStart != nil || row.PeriodEnd != nil {
		t.Fatalf("row %+v err %v", row, err)
	}
}

func TestSubscriptionIDResolution(t *testing.T) {
	sub := stripe.Event{Type: "customer.subscription.updated", ObjectType: "subscription", SessionID: "sub_1Abc"}
	if id, ok := subscriptionID(sub, nil); !ok || id != "sub_1Abc" {
		t.Fatalf("subscription event: %q %v", id, ok)
	}
	if _, ok := subscriptionID(stripe.Event{Type: "customer.subscription.updated", ObjectType: "invoice", SessionID: "sub_1Abc"}, nil); ok {
		t.Fatal("wrong object type must not resolve")
	}
	if _, ok := subscriptionID(stripe.Event{Type: "customer.subscription.updated", ObjectType: "subscription", SessionID: "in_1"}, nil); ok {
		t.Fatal("non-sub id must not resolve")
	}
	inv := func(object string) []byte {
		return []byte(`{"data":{"object":` + object + `}}`)
	}
	paid := stripe.Event{Type: "invoice.paid", ObjectType: "invoice", SessionID: "in_1"}
	if id, ok := subscriptionID(paid, inv(`{"id":"in_1","parent":{"type":"subscription_details","subscription_details":{"subscription":"sub_9Z"}}}`)); !ok || id != "sub_9Z" {
		t.Fatalf("invoice event: %q %v", id, ok)
	}
	for name, object := range map[string]string{
		"one-off invoice":   `{"id":"in_1","parent":null}`,
		"no parent":         `{"id":"in_1"}`,
		"other parent kind": `{"id":"in_1","parent":{"type":"quote_details","quote_details":{"quote":"qt_1"}}}`,
		"junk subscription": `{"id":"in_1","parent":{"subscription_details":{"subscription":"sub_9Z/../x"}}}`,
	} {
		if id, ok := subscriptionID(paid, inv(object)); ok {
			t.Errorf("%s resolved %q", name, id)
		}
	}
	if _, ok := subscriptionID(paid, []byte(`{not json`)); ok {
		t.Fatal("malformed raw body must not resolve")
	}
}

// signed builds a Stripe-Signature header for body at time at (https://docs.stripe.com/webhooks/signature).
func signed(secret string, at time.Time, body string) string {
	ts := strconv.FormatInt(at.Unix(), 10)
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(ts + "." + body))
	return "t=" + ts + ",v1=" + hex.EncodeToString(mac.Sum(nil))
}

const ingressDSNSentinel = "ingress-dsn-sentinel"

// lazyPool is a pool that never connects: every webhook path tested here ends before the database.
func lazyPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	u := url.URL{Scheme: "postgres", User: url.UserPassword("ingress", ingressDSNSentinel), Host: "127.0.0.1:1", Path: "/none"}
	pool, err := pgxpool.New(context.Background(), u.String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func webhookService(t *testing.T, rt http.RoundTripper) *Service {
	t.Helper()
	verifier, err := stripe.NewWebhookVerifier(stripe.WebhookConfig{Secrets: []string{fakeWebhookSecret}, AccountID: testAccount, Environment: "SANDBOX"})
	if err != nil {
		t.Fatal(err)
	}
	return &Service{cfg: Config{SecretKey: fakeSecretKey, PriceIDs: []string{"price_1Abc"}, ReturnOrigin: "https://admin.example.test"},
		stripe: newStripeClient(fakeSecretKey, rt), account: testAccount, verifier: verifier, enabled: true, now: time.Now}
}

func eventBody(typ string, livemode bool, object string) string {
	return fmt.Sprintf(`{"id":"evt_1Abc","object":"event","api_version":%q,"created":1700000000,"livemode":%t,"type":%q,"data":{"object":%s}}`,
		stripe.APIVersion, livemode, typ, object)
}

func postWebhook(h http.Handler, method, body string, headers map[string]string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, "/v1/platform/stripe/webhook", strings.NewReader(body))
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestWebhookStatusCodesBeforeAnyDatabaseWork(t *testing.T) {
	var retrieves int
	svc := webhookService(t, roundTrip(func(r *http.Request) (*http.Response, error) {
		retrieves++
		return reply(500, `{}`), nil // every retrieve fails: only handled events may reach it
	}))
	h := svc.WebhookHandler(lazyPool(t))
	now := time.Now()
	subObject := `{"object":"subscription","id":"sub_1Abc"}`
	sign := func(body string) map[string]string {
		return map[string]string{"Stripe-Signature": signed(fakeWebhookSecret, now, body)}
	}

	body := eventBody("customer.subscription.updated", false, subObject)
	for name, tc := range map[string]struct {
		method  string
		body    string
		headers map[string]string
		want    int
		fetches int
	}{
		"no signature":      {"POST", body, nil, 400, 0},
		"forged signature":  {"POST", body, map[string]string{"Stripe-Signature": signed(strings.Repeat("C", 32), now, body)}, 400, 0},
		"old signature":     {"POST", body, map[string]string{"Stripe-Signature": signed(fakeWebhookSecret, now.Add(-time.Hour), body)}, 400, 0},
		"tampered body":     {"POST", body + " ", sign(body), 400, 0},
		"livemode event":    {"POST", eventBody("customer.subscription.updated", true, subObject), sign(eventBody("customer.subscription.updated", true, subObject)), 400, 0},
		"malformed signed":  {"POST", `{"id":"evt_1"}`, sign(`{"id":"evt_1"}`), 400, 0},
		"ignored type":      {"POST", eventBody("charge.succeeded", false, `{"object":"charge","id":"ch_1"}`), sign(eventBody("charge.succeeded", false, `{"object":"charge","id":"ch_1"}`)), 200, 0},
		"invoice w/o sub":   {"POST", eventBody("invoice.paid", false, `{"object":"invoice","id":"in_1","parent":null}`), sign(eventBody("invoice.paid", false, `{"object":"invoice","id":"in_1","parent":null}`)), 200, 0},
		"retrieve fails":    {"POST", body, sign(body), 503, 1},
		"invoice retrieves": {"POST", eventBody("invoice.payment_failed", false, `{"object":"invoice","id":"in_1","parent":{"subscription_details":{"subscription":"sub_9Z"}}}`), sign(eventBody("invoice.payment_failed", false, `{"object":"invoice","id":"in_1","parent":{"subscription_details":{"subscription":"sub_9Z"}}}`)), 503, 1},
		"GET is refused":    {"GET", "", nil, 405, 0},
		"oversized body":    {"POST", strings.Repeat("x", maxWebhookBody+1), nil, 400, 0},
	} {
		retrieves = 0
		rec := postWebhook(h, tc.method, tc.body, tc.headers)
		if rec.Code != tc.want || retrieves != tc.fetches {
			t.Errorf("%s: status=%d retrieves=%d, want %d/%d", name, rec.Code, retrieves, tc.want, tc.fetches)
		}
		if strings.Contains(rec.Body.String(), fakeWebhookSecret) || strings.Contains(rec.Body.String(), "sub_1Abc") {
			t.Errorf("%s: response echoes request material: %s", name, rec.Body.String())
		}
	}
}

// An endpoint on a pre-basil API version delivers invoice.* without parent.subscription_details: the event is
// acknowledged (nothing to retry) but must leave an operator-visible alert, never a silent 200.
func TestWebhookInvoiceWithoutSubscriptionAlerts(t *testing.T) {
	var logs bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	defer slog.SetDefault(previous)
	svc := webhookService(t, roundTrip(func(r *http.Request) (*http.Response, error) {
		t.Fatalf("no subscription id must not reach Stripe: %s", r.URL.Path)
		return nil, nil
	}))
	body := eventBody("invoice.paid", false, `{"object":"invoice","id":"in_1","subscription":"sub_1Abc"}`) // pre-basil shape
	rec := postWebhook(svc.WebhookHandler(lazyPool(t)), "POST", body, map[string]string{"Stripe-Signature": signed(fakeWebhookSecret, time.Now(), body)})
	if rec.Code != 200 || strings.Count(logs.String(), "invoice_without_subscription") != 1 {
		t.Fatalf("status=%d logs=%q, want 200 and one invoice_without_subscription alert", rec.Code, logs.String())
	}
	logs.Reset()
	body = eventBody("customer.subscription.updated", false, `{"object":"customer","id":"cus_1"}`)
	postWebhook(svc.WebhookHandler(lazyPool(t)), "POST", body, map[string]string{"Stripe-Signature": signed(fakeWebhookSecret, time.Now(), body)})
	if strings.Contains(logs.String(), "invoice_without_subscription") {
		t.Fatalf("a non-invoice miss must not raise the invoice alert: %q", logs.String())
	}
}

func TestWebhookDisabledAnswers503(t *testing.T) {
	for name, h := range map[string]http.Handler{
		"nil service":     (*Service)(nil).WebhookHandler(nil),
		"disabled":        (&Service{}).WebhookHandler(nil),
		"enabled no pool": webhookService(t, nil).WebhookHandler(nil),
	} {
		if rec := postWebhook(h, "POST", "{}", nil); rec.Code != http.StatusServiceUnavailable {
			t.Errorf("%s: %d", name, rec.Code)
		}
	}
}

func TestNilAndDisabledServiceRefuseStripeActions(t *testing.T) {
	scope := platform.Scope{TenantID: "t", StoreID: testStore, PrincipalID: "p"}
	for name, svc := range map[string]*Service{"nil": nil, "disabled": {}} {
		if svc.Enabled() {
			t.Fatalf("%s enabled", name)
		}
		if _, err := svc.StartCheckout(context.Background(), nil, scope, "token", "price_1Abc"); !errors.Is(err, ErrUnavailable) {
			t.Errorf("%s checkout err=%v", name, err)
		}
		if _, err := svc.OpenPortal(context.Background(), nil, scope, "token"); !errors.Is(err, ErrUnavailable) {
			t.Errorf("%s portal err=%v", name, err)
		}
		if plans := svc.planList(context.Background()); plans == nil || len(plans) != 0 {
			t.Errorf("%s plans=%#v want empty non-nil", name, plans)
		}
	}
	// Enabled, but price_id outside the configured set: 422 before any database or Stripe work.
	if _, err := webhookService(t, nil).StartCheckout(context.Background(), nil, scope, "token", "price_other"); !errors.Is(err, ErrUnknownPrice) {
		t.Fatalf("unknown price err=%v", err)
	}
}

func TestNewStaysDisabledWhenAccountUnreadable(t *testing.T) {
	var logs bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	defer slog.SetDefault(previous)
	rt := roundTrip(func(r *http.Request) (*http.Response, error) {
		if r.URL.Path != "/v1/account" {
			t.Fatalf("startup called %s before proving the account", r.URL.Path)
		}
		return reply(401, `{"error":{"message":"bad key"}}`), nil
	})
	cfg, _, _ := LoadConfig(env(goodEnv()))
	svc, err := New(context.Background(), lazyPool(t), cfg, rt)
	if err != nil || svc == nil || svc.Enabled() {
		t.Fatalf("svc=%v err=%v: an unreadable account must yield a disabled service", svc, err)
	}
	if strings.Count(logs.String(), "billing_account_conflict") != 1 {
		t.Fatalf("want exactly one billing_account_conflict line, got %q", logs.String())
	}
	if strings.Contains(logs.String(), fakeSecretKey) {
		t.Fatal("key in log")
	}
	if _, err := New(context.Background(), nil, cfg, rt); !errors.Is(err, ErrConfig) {
		t.Fatalf("nil pool err=%v", err)
	}
	bad := cfg
	bad.SecretKey = "sk_" + "live_" + strings.Repeat("A", 24)
	if _, err := New(context.Background(), lazyPool(t), bad, rt); !errors.Is(err, ErrConfig) {
		t.Fatalf("live key err=%v", err)
	}
}

func TestPlansFilterCacheAndFailure(t *testing.T) {
	price := func(id string, active, live bool, recurring string, amount string) string {
		rec := "null"
		if recurring != "" {
			rec = `{"interval":"` + recurring + `"}`
		}
		return `{"id":"` + id + `","active":` + strconv.FormatBool(active) + `,"livemode":` + strconv.FormatBool(live) +
			`,"currency":"twd","unit_amount":` + amount + `,"nickname":"nick","recurring":` + rec + `,"product":{"name":"Standard"}}`
	}
	answers := map[string]string{
		"price_1Abc":  price("price_1Abc", true, false, "month", "99000"),
		"price_2Off":  price("price_2Off", false, false, "month", "1"),
		"price_3Liv":  price("price_3Liv", true, true, "month", "1"),
		"price_4One":  price("price_4One", true, false, "", "1"),
		"price_5Tier": price("price_5Tier", true, false, "month", "null"),
	}
	var calls int
	fail := "price_6Bad"
	svc := webhookService(t, roundTrip(func(r *http.Request) (*http.Response, error) {
		calls++
		id := strings.TrimPrefix(r.URL.Path, "/v1/prices/")
		if r.URL.RawQuery != "expand%5B%5D=product" {
			t.Errorf("query %q", r.URL.RawQuery)
		}
		if id == fail {
			return reply(500, `{}`), nil
		}
		return reply(200, answers[id]), nil
	}))
	svc.cfg.PriceIDs = []string{"price_1Abc", "price_2Off", "price_3Liv", "price_4One", "price_5Tier"}
	got := svc.planList(context.Background())
	if len(got) != 1 || got[0] != (Plan{PriceID: "price_1Abc", Name: "Standard", AmountMinor: 99000, Currency: "TWD", Interval: "month"}) {
		t.Fatalf("plans %+v", got)
	}
	first := calls
	svc.planList(context.Background())
	if calls != first {
		t.Fatalf("second read within TTL fetched again (%d vs %d)", calls, first)
	}
	svc.now = func() time.Time { return time.Now().Add(11 * time.Minute) }
	svc.planList(context.Background())
	if calls != first*2 {
		t.Fatalf("read after TTL did not refetch (%d)", calls)
	}
	// A failed fetch omits that plan, is not cached, and the next read retries it.
	svc.cfg.PriceIDs = []string{"price_1Abc", fail}
	svc.plans, svc.plansAt = nil, time.Time{}
	svc.now = time.Now
	before := calls
	if got := svc.planList(context.Background()); len(got) != 1 || got[0].PriceID != "price_1Abc" {
		t.Fatalf("partial plans %+v", got)
	}
	svc.planList(context.Background())
	if calls-before != 4 {
		t.Fatalf("failed fetch was cached: %d calls", calls-before)
	}
}

func TestNeedsRefreshUsesTenMinuteRule(t *testing.T) {
	svc := &Service{now: func() time.Time { return time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC) }}
	at := func(age time.Duration) string {
		return svc.now().Add(-age).Format("2006-01-02T15:04:05.000000Z")
	}
	if svc.needsRefresh(nil) {
		t.Fatal("no rows: nothing to refresh (spec: any ROW older than 10 min)")
	}
	if svc.needsRefresh([]Subscription{{RetrievedAt: at(9 * time.Minute)}}) {
		t.Fatal("9 min old row must not refresh")
	}
	if !svc.needsRefresh([]Subscription{{RetrievedAt: at(time.Minute)}, {RetrievedAt: at(11 * time.Minute)}}) {
		t.Fatal("one 11 min old row must refresh")
	}
	if !svc.needsRefresh([]Subscription{{RetrievedAt: "garbage"}}) {
		t.Fatal("an unparsable timestamp must refresh, not hide staleness")
	}
}

func TestBearerURLsNeverReachLogsOrErrors(t *testing.T) {
	var logs bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	defer slog.SetDefault(previous)
	rec := &recorder{answers: []*http.Response{
		reply(200, `{"id":"cs_test_z1","url":"`+bearerSent+`"}`),
		reply(200, `{"url":"`+bearerSent+`"}`),
		reply(500, `{"error":{"message":"`+bearerSent+`"}}`),
	}}
	c := newStripeClient(fakeSecretKey, rec)
	_, u1, err1 := c.createCheckout(context.Background(), checkoutParams{Customer: "cus_1", Store: testStore, PriceID: "price_1", ExpiresAt: time.Now()})
	u2, err2 := c.createPortal(context.Background(), "cus_1", "https://admin.example.test/billing")
	_, _, err3 := c.createCheckout(context.Background(), checkoutParams{Customer: "cus_1", Store: testStore, PriceID: "price_1", ExpiresAt: time.Now()})
	if err1 != nil || err2 != nil || u1 != bearerSent || u2 != bearerSent || err3 == nil {
		t.Fatalf("setup: %v %v %v", err1, err2, err3)
	}
	if strings.Contains(logs.String(), "SENTINEL_BEARER") || strings.Contains(err3.Error(), "SENTINEL_BEARER") {
		t.Fatalf("bearer URL leaked: logs=%q err=%v", logs.String(), err3)
	}
}

func TestMapErrorTable(t *testing.T) {
	pg := func(code, msg string) error { return &pgconn.PgError{Code: code, Message: msg} }
	for _, tc := range []struct {
		err  error
		want error
	}{
		{pg("PT400", "invalid"), command.ErrInvalid},
		{pg("23514", "x"), command.ErrInvalid},
		{pg("PT409", "billing_account_conflict"), ErrUnavailable},
		{pg("PT409", "billing customer already pinned"), command.ErrConflict},
		{pg("23505", "dup"), command.ErrConflict},
		{pg("PT401", "u"), platform.ErrUnauthorized},
		{pg("PT403", "f"), platform.ErrForbidden},
		{pg("PT404", "n"), platform.ErrScopeNotFound},
	} {
		if got := mapError(tc.err); !errors.Is(got, tc.want) {
			t.Errorf("%v -> %v want %v", tc.err, got, tc.want)
		}
	}
	lock := pg("55P03", "lock")
	if mapError(lock) != lock || mapError(nil) != nil {
		t.Fatal("lock timeouts and nil must pass through unchanged")
	}
}
