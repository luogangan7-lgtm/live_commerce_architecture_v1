package foundation_test

// CB08 (contracts/customers-billing-v1.md §1 F-B*, §2, §5 checkout steps + Platform paragraph, §6, §7, §8 and
// billing-core.md B3-B10; tier MOCK = real PG + billing.New(..., fake.Transport()) + billing.WebhookHandler
// against the independent fake internal/billing/billingtest). Written from the contract and the FROZEN block
// of billing-core.md only. Helper prefix `cbm`. What it proves, per contract clause:
//   - config admission: disabled reads no secret, test keys only, 1..10 price ids, https origin, no value
//     ever echoed, Config formats redacted;
//   - startup (BD1/B4): the account id equal to a registered PSP account, or unreadable, disables billing
//     for the process with one `billing_account_conflict` log line and exactly one Stripe call; the pin path
//     re-checks it;
//   - customer create: body `metadata[lc_store]` only, key lc:billing:customer:v1:<store>:SANDBOX, stable and
//     byte-identical on retry, also after a lost response;
//   - checkout: exact sorted body (B6), pinned API version, no Idempotency-Key, 30 min expiry, trial only for a
//     never-subscribed store (BD2), subscription_exists (409) mirrors before refusing, parallel and retried
//     calls leave one open session known to the database (older one expired under key lc:billing:expire:v1:<id>);
//   - portal: no customer -> 409; return URL; no key; URLs never in logs, DB or errors (I11);
//   - webhook: forged/old/missing signature, livemode, oversize, malformed -> 400 with no Stripe call; the event
//     is a wake-up (state comes from the retrieved subscription, D8); invoice.* resolves through
//     parent.subscription_details only; unhandled types ignored; retrieve failure/timeout -> 503; mismatch,
//     duplicate, unknown customer -> 200 + billing_ops_alert without ids beyond the subscription id; multi-item
//     -> logged, not applied; period fields from the items (F-B10) on API 2026-08-26.dahlia;
//   - status: refresh-on-read after 10 min (list body), Stripe error -> DB state with stale=true, plan cache,
//     plan filtering (active, recurring, test mode).
// Disclosed owner-pool fixtures: extra stores of the tenant, aged billing.subscriptions.retrieved_at, a
// registered stripe merchant account row. Fake keys are written split ("sk_"+"test_..."), no real secret.

import (
	"bytes"
	"context"
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
	"regexp"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"livecommerce/internal/billing"
	"livecommerce/internal/billing/billingtest"
	"livecommerce/internal/platform"
)

type cbmLogBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *cbmLogBuf) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}
func (l *cbmLogBuf) String() string { l.mu.Lock(); defer l.mu.Unlock(); return l.b.String() }
func (l *cbmLogBuf) Lines(sub string) (out []string) {
	for _, line := range strings.Split(l.String(), "\n") {
		if strings.Contains(line, sub) {
			out = append(out, line)
		}
	}
	return out
}

type cbmEnv struct {
	p       psHarness
	t       *testing.T
	ctx     context.Context
	f       *testFixture
	fake    *billingtest.Server
	svc     *billing.Service
	cfg     billing.Config
	ingress *pgxpool.Pool
	hook    http.Handler
	logs    *cbmLogBuf
	tenant  string
	store   string
	token   string
	scope   platform.Scope
}

const cbmOrigin = "https://admin.example.test"

func cbmConfig() billing.Config {
	return billing.Config{SecretKey: "sk_" + "test_" + hex.EncodeToString(randomBytes(12)), WebhookSecret: "whsec_" + hex.EncodeToString(randomBytes(16)),
		ReturnOrigin: cbmOrigin, PriceIDs: []string{"price_Cb08Month", "price_Cb08Year", "price_Cb08Live", "price_Cb08One"}}
}

// cbmSetup builds the fake, the service and the webhook handler over a fresh store of the shared fixture.
// registerPSP registers the fake platform account id as a merchant PSP account before startup (BD1 conflict); failAccount
// makes GET /v1/account fail. Every call gets its own account id: the shared database keeps registered accounts.
func cbmSetup(t *testing.T, registerPSP bool, failAccount bool) *cbmEnv {
	t.Helper()
	p := psSetup(t)
	f := p.f
	m := &cbmEnv{p: p, t: t, ctx: context.Background(), f: f, tenant: f.tenantA, store: f.storeA1, logs: &cbmLogBuf{}, cfg: cbmConfig()}
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(m.logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	m.fake = billingtest.New("acct_Cb" + t04Tag())
	t.Cleanup(m.fake.Close)
	m.fake.RequireKey(m.cfg.SecretKey)
	m.fake.AddPrice("price_Cb08Month", 30000, "twd", "month", "Pro Monthly", true, false)
	m.fake.AddPrice("price_Cb08Year", 300000, "twd", "year", "Pro Yearly", false, false) // inactive
	m.fake.AddPrice("price_Cb08Live", 30000, "twd", "month", "Live mode price", true, true)
	m.fake.AddPriceRaw("price_Cb08One", map[string]any{"active": true, "livemode": false, "currency": "twd", "unit_amount": 500, "product": map[string]any{"id": "prod_x", "name": "One time"}})
	if registerPSP {
		cbxRegisterPSP(t, f, f.tenantA, f.storeA1, f.principalA, "SANDBOX", m.fake.Account())
	}
	if failAccount {
		m.fake.FailNext("account", 503)
	}
	var err error
	if m.svc, err = billing.New(m.ctx, f.runtime, m.cfg, m.fake.Transport()); err != nil {
		t.Fatalf("billing.New: %v", err)
	}
	m.ingress, err = platform.OpenStripeIngressPool(m.ctx, sstLogin(t, f, "commerce_stripe_ingress"))
	if err != nil {
		t.Fatalf("ingress pool: %v", err)
	}
	t.Cleanup(m.ingress.Close)
	if m.svc != nil {
		m.hook = m.svc.WebhookHandler(m.ingress)
	}
	_, m.token = lcPrincipal(t, f, f.tenantA, []string{f.storeA1}, "store:read", "billing:manage")
	m.scope = m.scopeOf(m.store, m.token)
	return m
}

func (m *cbmEnv) scopeOf(store, token string) (sc platform.Scope) {
	m.t.Helper()
	if err := platform.WithScope(m.ctx, m.f.runtime, token, store, "store:read", func(_ pgx.Tx, s platform.Scope) error { sc = s; return nil }); err != nil {
		m.t.Fatalf("scope: %v", err)
	}
	return sc
}

func (m *cbmEnv) checkout(price string) (string, error) {
	return m.svc.StartCheckout(m.ctx, m.f.runtime, m.scope, m.token, price)
}

// extraStore is a fresh store with its own billing:manage member: the per-status cases need clean stores.
func (m *cbmEnv) extraStore() (store, token string, sc platform.Scope) {
	m.t.Helper()
	store = cbxStore(m.t, m.f, m.tenant)
	token = lcTokenFor(m.t, m.f, m.tenant, store, "billing:manage")
	return store, token, m.scopeOf(store, token)
}

func cbmForm(c billingtest.Call) map[string]string {
	out := map[string]string{}
	for k, v := range c.Form {
		out[k] = strings.Join(v, ",")
	}
	return out
}

func cbmKeys(m map[string]string) string {
	var ks []string
	for k := range m {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return strings.Join(ks, ",")
}

func (m *cbmEnv) post(body []byte, sig string) *httptest.ResponseRecorder {
	m.t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/platform/stripe/webhook", bytes.NewReader(body))
	if sig != "" {
		req.Header.Set("Stripe-Signature", sig)
	}
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	m.hook.ServeHTTP(w, req)
	return w
}

func (m *cbmEnv) signed(body []byte) *httptest.ResponseRecorder {
	return m.post(body, billingtest.Sign(m.cfg.WebhookSecret, body, time.Now()))
}

// pinned pins a fresh fake customer to store through the real definer path and returns its id.
func (m *cbmEnv) pinned(store, token string) string {
	m.t.Helper()
	cus := m.fake.AddCustomer(map[string]string{"lc_store": store})
	h := sha256.Sum256([]byte(token))
	if err := platform.WithScope(m.ctx, m.f.runtime, token, store, "store:read", func(tx pgx.Tx, _ platform.Scope) error {
		var out any
		return tx.QueryRow(m.ctx, `SELECT billing.pin_customer($1,$2,'SANDBOX',$3,$4)`, h[:], store, cus, m.fake.Account()).Scan(&out)
	}); err != nil {
		m.t.Fatalf("pin: %v", err)
	}
	return cus
}

// age moves a mirrored row's retrieved_at back by d (disclosed fixture: the monotone trigger forbids it, so triggers are
// off for this one statement; the service's own clock cannot be skewed from outside).
func (m *cbmEnv) age(sub string, d time.Duration) {
	m.t.Helper()
	tx, err := m.f.owner.Begin(m.ctx)
	if err != nil {
		m.t.Fatal(err)
	}
	defer tx.Rollback(m.ctx)
	if _, err := tx.Exec(m.ctx, `SET LOCAL session_replication_role = replica`); err != nil {
		m.t.Fatal(err)
	}
	if _, err := tx.Exec(m.ctx, `UPDATE billing.subscriptions SET retrieved_at=retrieved_at-$2::interval WHERE stripe_subscription_id=$1`, sub, fmt.Sprintf("%d seconds", int64(d.Seconds()))); err != nil {
		m.t.Fatal(err)
	}
	if err := tx.Commit(m.ctx); err != nil {
		m.t.Fatal(err)
	}
}

func (m *cbmEnv) rowStatus(sub string) string {
	return cbxOne(m.t, m.f, `SELECT status FROM billing.subscriptions WHERE stripe_subscription_id=$1`, sub)
}

func TestCustomersBillingCB08Mock(t *testing.T) {
	t.Run("config admission (B3)", func(t *testing.T) {
		good := map[string]string{"LC_BILLING_ENABLED": "1", "LC_PLATFORM_STRIPE_SECRET_KEY": "sk_" + "test_" + strings.Repeat("a1", 12),
			"LC_PLATFORM_STRIPE_WEBHOOK_SECRET": "whsec_" + strings.Repeat("b2", 16), "LC_BILLING_PRICE_IDS": "price_A1,price_B2", "LC_BILLING_RETURN_ORIGIN": cbmOrigin}
		load := func(over map[string]string) (billing.Config, bool, error, []string) {
			env := map[string]string{}
			for k, v := range good {
				env[k] = v
			}
			for k, v := range over {
				if v == "\x00unset" {
					delete(env, k)
				} else {
					env[k] = v
				}
			}
			var read []string
			cfg, on, err := billing.LoadConfig(func(k string) string { read = append(read, k); return env[k] })
			return cfg, on, err, read
		}
		if cfg, on, err, read := load(map[string]string{"LC_BILLING_ENABLED": "\x00unset"}); on || err != nil || len(read) != 1 || read[0] != "LC_BILLING_ENABLED" || cfg.SecretKey != "" {
			t.Errorf("disabled: on=%v err=%v read=%v cfg=%+v (billing off must read no secret)", on, err, read, cfg)
		}
		cfg, on, err, _ := load(nil)
		if !on || err != nil || cfg.SecretKey != good["LC_PLATFORM_STRIPE_SECRET_KEY"] || len(cfg.PriceIDs) != 2 || cfg.ReturnOrigin != cbmOrigin {
			t.Fatalf("valid: on=%v err=%v", on, err)
		}
		raw, _ := json.Marshal(cfg)
		for _, s := range []string{fmt.Sprintf("%v", cfg), fmt.Sprintf("%+v", cfg), fmt.Sprintf("%#v", cfg), fmt.Sprintf("%s", cfg), string(raw)} {
			for _, secret := range []string{cfg.SecretKey, cfg.WebhookSecret} {
				if strings.Contains(s, secret) {
					t.Errorf("a formatted Config leaks a secret: %s", s)
				}
			}
		}
		if _, ok, err, _ := load(map[string]string{"LC_PLATFORM_STRIPE_SECRET_KEY": "rk_" + "test_" + strings.Repeat("c3", 12)}); !ok || err != nil {
			t.Errorf("a restricted test key must be accepted: %v", err)
		}
		bad := map[string]map[string]string{
			"live secret key":      {"LC_PLATFORM_STRIPE_SECRET_KEY": "sk_" + "live_" + strings.Repeat("a1", 12)},
			"restricted live key":  {"LC_PLATFORM_STRIPE_SECRET_KEY": "rk_" + "live_" + strings.Repeat("a1", 12)},
			"publishable key":      {"LC_PLATFORM_STRIPE_SECRET_KEY": "pk_" + "test_" + strings.Repeat("a1", 12)},
			"missing secret key":   {"LC_PLATFORM_STRIPE_SECRET_KEY": "\x00unset"},
			"missing webhook":      {"LC_PLATFORM_STRIPE_WEBHOOK_SECRET": "\x00unset"},
			"webhook wrong prefix": {"LC_PLATFORM_STRIPE_WEBHOOK_SECRET": "whk_" + strings.Repeat("b2", 16)},
			"no price ids":         {"LC_BILLING_PRICE_IDS": ""},
			"eleven price ids":     {"LC_BILLING_PRICE_IDS": strings.TrimSuffix(strings.Repeat("price_X1,", 11), ",")},
			"malformed price id":   {"LC_BILLING_PRICE_IDS": "price_A1,plan_B2"},
			"http origin":          {"LC_BILLING_RETURN_ORIGIN": "http://admin.example.test"},
			"origin with a path":   {"LC_BILLING_RETURN_ORIGIN": cbmOrigin + "/billing"},
			"origin with userinfo": {"LC_BILLING_RETURN_ORIGIN": "https://u:p@admin.example.test"},
			"origin with a query":  {"LC_BILLING_RETURN_ORIGIN": cbmOrigin + "?x=1"},
			"missing origin":       {"LC_BILLING_RETURN_ORIGIN": "\x00unset"},
			"enabled is not 1":     {"LC_BILLING_ENABLED": "yes"},
			"enabled true":         {"LC_BILLING_ENABLED": "true"},
		}
		for name, over := range bad {
			_, on, err, _ := load(over)
			if err == nil {
				t.Errorf("%s: accepted (on=%v)", name, on)
				continue
			}
			for k, v := range over {
				if v != "\x00unset" && len(v) > 6 && strings.Contains(err.Error(), v) {
					t.Errorf("%s: the error echoes %s: %v", name, k, err)
				}
			}
		}
		if _, err := billing.New(context.Background(), nil, cfg, nil); err == nil {
			t.Error("billing.New with no runtime pool must refuse")
		}
	})

	t.Run("billing off: standing and the read still work, the POSTs are 503 unavailable, no Stripe traffic", func(t *testing.T) {
		m := cbmSetup(t, false, false)
		var off *billing.Service
		if off.Enabled() {
			t.Fatal("a nil service must not be enabled")
		}
		if _, err := off.StartCheckout(m.ctx, m.f.runtime, m.scope, m.token, "price_Cb08Month"); !errors.Is(err, billing.ErrUnavailable) {
			t.Errorf("checkout with billing off: %v, want ErrUnavailable", err)
		}
		if _, err := off.OpenPortal(m.ctx, m.f.runtime, m.scope, m.token); !errors.Is(err, billing.ErrUnavailable) {
			t.Errorf("portal with billing off: %v, want ErrUnavailable", err)
		}
		st, err := off.Status(m.ctx, m.f.runtime, m.scope, m.token)
		raw, _ := json.Marshal(st)
		if err != nil || st.Standing != billing.Unbilled || len(st.Plans) != 0 || !strings.Contains(string(raw), `"plans":[]`) || !strings.Contains(string(raw), `"subscriptions":[]`) {
			t.Errorf("status with billing off: %s %v (arrays must marshal as [], never null)", raw, err)
		}
		before := len(m.fake.Calls())
		if _, err := off.Status(m.ctx, m.f.runtime, m.scope, m.token); err != nil || len(m.fake.Calls()) != before {
			t.Error("a disabled service called Stripe")
		}
	})

	t.Run("startup: a registered PSP account id or an unreadable account disables billing once (BD1, B4)", func(t *testing.T) {
		for name, tc := range map[string]struct {
			psp  bool
			fail bool
		}{"account equals a registered PSP account": {true, false}, "account unreadable": {false, true}} {
			m := cbmSetup(t, tc.psp, tc.fail)
			if m.svc.Enabled() {
				t.Errorf("%s: billing stayed enabled", name)
			}
			if n := len(m.logs.Lines("billing_account_conflict")); n != 1 {
				t.Errorf("%s: %d billing_account_conflict log lines, want exactly one", name, n)
			}
			if got := len(m.fake.CallsTo("GET", "/v1/account")); got != 1 || len(m.fake.Calls()) != 1 {
				t.Errorf("%s: %d calls (%d account reads), want exactly one account read and nothing else (no retry loop)", name, len(m.fake.Calls()), got)
			}
			if _, err := m.checkout("price_Cb08Month"); !errors.Is(err, billing.ErrUnavailable) {
				t.Errorf("%s: checkout %v, want ErrUnavailable (503 billing_unavailable)", name, err)
			}
			if _, err := m.svc.OpenPortal(m.ctx, m.f.runtime, m.scope, m.token); !errors.Is(err, billing.ErrUnavailable) {
				t.Errorf("%s: portal %v, want ErrUnavailable", name, err)
			}
			if st, err := m.svc.Status(m.ctx, m.f.runtime, m.scope, m.token); err != nil || st.Standing != billing.Unbilled {
				t.Errorf("%s: status %+v %v (the read side works while billing is disabled)", name, st, err)
			}
			if w := m.hook; w != nil {
				body := m.fake.RawEvent("customer.subscription.updated", billingtest.EventOpts{}, map[string]any{"id": "sub_x"})
				if rec := m.signed(body); rec.Code < 400 {
					t.Errorf("%s: the webhook of a disabled service answered %d", name, rec.Code)
				}
			}
			if len(m.fake.Customers()) != 0 {
				t.Errorf("%s: a customer was created", name)
			}
		}
	})

	t.Run("customer create: key, body, retry and lost response; pin re-checks the conflict", func(t *testing.T) {
		m := cbmSetup(t, false, false)
		m.fake.FailNext("customer", 500)
		if _, err := m.checkout("price_Cb08Month"); err == nil || errors.Is(err, billing.ErrSubscriptionExists) {
			t.Fatalf("customer create failing: %v, want a retryable error", err)
		}
		if cbxOne(t, m.f, `SELECT count(*)::text FROM billing.store_customers WHERE store_id=$1`, m.store) != "0" {
			t.Fatal("a failed customer create left a pinned row")
		}
		m.fake.LoseNext("customer") // Stripe creates the customer, the response never arrives; net/http itself replays a POST that carries an Idempotency-Key
		if _, err := m.checkout("price_Cb08Month"); err != nil {
			if _, err := m.checkout("price_Cb08Month"); err != nil { // the stable key makes the caller's retry a replay too
				t.Fatalf("retry: %v", err)
			}
		}
		calls := m.fake.CallsExact("POST", "/v1/customers")
		if len(calls) < 3 {
			t.Fatalf("%d customer-create calls, want >= 3 (failed, lost, replayed)", len(calls))
		}
		wantKey := "lc:billing:customer:v1:" + m.store + ":SANDBOX"
		for i, c := range calls {
			if c.IdempotencyKey != wantKey || c.Body != calls[0].Body || cbmKeys(cbmForm(c)) != "metadata[lc_store]" || cbmForm(c)["metadata[lc_store]"] != m.store {
				t.Errorf("customer-create call %d: key=%q body=%q (want key %q, body identical and only metadata[lc_store]=<store>)", i, c.IdempotencyKey, c.Body, wantKey)
			}
			if c.StripeVersion != billingtest.APIVersion || !strings.HasPrefix(c.ContentType, "application/x-www-form-urlencoded") {
				t.Errorf("call %d: Stripe-Version=%q Content-Type=%q", i, c.StripeVersion, c.ContentType)
			}
		}
		if len(m.fake.Customers()) != 1 {
			t.Errorf("%d Stripe customers for one store after retries (the stable key must dedupe)", len(m.fake.Customers()))
		}
		if got := cbxOne(t, m.f, `SELECT stripe_customer_id FROM billing.store_customers WHERE store_id=$1`, m.store); got != m.fake.Customers()[0] {
			t.Errorf("pinned %q, Stripe has %v", got, m.fake.Customers())
		}
		// BD1 re-check at pin time: the platform account becomes a registered PSP account after startup
		s2, tok2, sc2 := m.extraStore()
		cbxRegisterPSP(t, m.f, m.tenant, s2, m.f.principalA, "SANDBOX", m.fake.Account())
		if _, err := m.svc.StartCheckout(m.ctx, m.f.runtime, sc2, tok2, "price_Cb08Month"); err == nil {
			t.Error("checkout pinned a customer although the platform account is now a registered PSP account")
		}
		if cbxOne(t, m.f, `SELECT count(*)::text FROM billing.store_customers WHERE store_id=$1`, s2) != "0" {
			t.Error("pin_customer accepted a conflicting platform account")
		}
		if _, err := m.svc.StartCheckout(m.ctx, m.f.runtime, sc2, tok2, "price_Cb08Nope"); !errors.Is(err, billing.ErrUnknownPrice) {
			t.Errorf("a price outside the configured set: %v, want ErrUnknownPrice (422)", err)
		}
	})

	t.Run("checkout: exact body, no Idempotency-Key, trial once, subscription_exists, one open session", func(t *testing.T) {
		m := cbmSetup(t, false, false)
		before := time.Now()
		url1, err := m.checkout("price_Cb08Month")
		if err != nil || !strings.HasPrefix(url1, "https://checkout.stripe.com/") {
			t.Fatalf("checkout: %q %v", url1, err)
		}
		calls := m.fake.Calls()
		var order []string
		for _, c := range calls {
			order = append(order, c.Method+" "+c.Path)
		}
		wantOrder := []string{"POST /v1/customers", "GET /v1/subscriptions", "POST /v1/checkout/sessions"}
		if strings.Join(order, "|") != strings.Join(append([]string{"GET /v1/account"}, wantOrder...), "|") {
			t.Fatalf("Stripe call order %v, want account, customer create, list by customer, checkout create (steps 1-4)", order)
		}
		list := m.fake.CallsTo("GET", "/v1/subscriptions")[0]
		if lq, _ := url.ParseQuery(list.RawQuery); lq.Get("customer") != m.fake.Customers()[0] || lq.Get("status") != "all" {
			t.Errorf("step 1 list query %q, want customer=<pinned>&status=all", list.RawQuery)
		}
		create := m.fake.CallsExact("POST", "/v1/checkout/sessions")[0]
		form := cbmForm(create)
		wantKeys := "cancel_url,client_reference_id,customer,expires_at,line_items[0][price],line_items[0][quantity],mode,subscription_data[metadata][lc_store],subscription_data[trial_period_days],success_url"
		if cbmKeys(form) != wantKeys {
			t.Fatalf("checkout form keys\n got  %s\n want %s", cbmKeys(form), wantKeys)
		}
		var expires int64
		fmt.Sscan(form["expires_at"], &expires)
		if form["mode"] != "subscription" || form["customer"] != m.fake.Customers()[0] || form["client_reference_id"] != m.store ||
			form["line_items[0][price]"] != "price_Cb08Month" || form["line_items[0][quantity]"] != "1" ||
			form["subscription_data[metadata][lc_store]"] != m.store || form["subscription_data[trial_period_days]"] != "14" ||
			form["success_url"] != cbmOrigin+"/billing?store="+m.store+"&checkout=done" || form["cancel_url"] != cbmOrigin+"/billing?store="+m.store+"&checkout=cancel" {
			t.Errorf("checkout form values: %v", form)
		}
		if d := time.Unix(expires, 0).Sub(before); d < 29*time.Minute+50*time.Second || d > 31*time.Minute {
			t.Errorf("expires_at is %s after the call, want the F-B4 minimum of 30 min (record_checkout_session allows <= 31)", d)
		}
		if create.IdempotencyKey != "" {
			t.Errorf("checkout create sent Idempotency-Key %q: a wall-clock expires_at changes the params on retry and Stripe rejects the reuse (§5 step 3)", create.IdempotencyKey)
		}
		if create.StripeVersion != billingtest.APIVersion {
			t.Errorf("Stripe-Version %q", create.StripeVersion)
		}
		if got := cbxOne(t, m.f, `SELECT open_checkout_session_id FROM billing.store_customers WHERE store_id=$1`, m.store); got != m.fake.Sessions()[0].ID {
			t.Errorf("recorded open session %q, Stripe has %v", got, m.fake.Sessions())
		}
		// a second click: a new session, the older one is expired with a stable per-session key and an empty body
		first := m.fake.Sessions()[0].ID
		if _, err := m.checkout("price_Cb08Month"); err != nil {
			t.Fatal(err)
		}
		if m.fake.OpenSessions() != 1 {
			t.Fatalf("%d open sessions after two clicks, want 1", m.fake.OpenSessions())
		}
		exp2 := m.fake.CallsTo("POST", "/v1/checkout/sessions/")
		var expires2 []billingtest.Call
		for _, c := range exp2 {
			if strings.HasSuffix(c.Path, "/expire") {
				expires2 = append(expires2, c)
			}
		}
		if len(expires2) != 1 || expires2[0].Path != "/v1/checkout/sessions/"+first+"/expire" || expires2[0].IdempotencyKey != "lc:billing:expire:v1:"+first || expires2[0].Body != "" {
			t.Errorf("expire calls %+v, want one for %s with key lc:billing:expire:v1:<id> and no body", expires2, first)
		}
		// the response of a create is lost: nothing is recorded, the retry records its own, one is known to the database
		m.fake.LoseNext("checkout")
		if _, err := m.checkout("price_Cb08Month"); err == nil {
			t.Fatal("a lost checkout response must surface as an error")
		}
		if _, err := m.checkout("price_Cb08Month"); err != nil {
			t.Fatal(err)
		}
		known := cbxOne(t, m.f, `SELECT open_checkout_session_id FROM billing.store_customers WHERE store_id=$1`, m.store)
		for _, s := range m.fake.Sessions() {
			if s.ID == known && s.Status != "open" {
				t.Errorf("the recorded session %s is not open", known)
			}
		}
		if len(m.fake.CallsExact("POST", "/v1/checkout/sessions")) != 4 {
			t.Errorf("%d create calls, want 4 (first, second, lost response, retry: never an idempotent replay)", len(m.fake.CallsExact("POST", "/v1/checkout/sessions")))
		}
		for _, c := range m.fake.CallsExact("POST", "/v1/checkout/sessions") {
			if c.IdempotencyKey != "" {
				t.Errorf("a checkout create carried the key %q", c.IdempotencyKey)
			}
		}
	})

	t.Run("checkout trial only once (BD2) and subscription_exists (409) mirrors before refusing", func(t *testing.T) {
		m := cbmSetup(t, false, false)
		store, token, scope := m.extraStore()
		cus := m.pinned(store, token)
		// a canceled prior subscription at Stripe: mirrored by step 1, so no trial on the new checkout
		prior := m.fake.AddSubscription(billingtest.Sub{Customer: cus, Status: "canceled", StoreMeta: store})
		if _, err := m.svc.StartCheckout(m.ctx, m.f.runtime, scope, token, "price_Cb08Month"); err != nil {
			t.Fatalf("checkout after a canceled subscription: %v", err)
		}
		c := m.fake.CallsExact("POST", "/v1/checkout/sessions")
		if len(c) != 1 {
			t.Fatalf("%d create calls", len(c))
		}
		if _, has := cbmForm(c[0])["subscription_data[trial_period_days]"]; has {
			t.Error("a store with a prior subscription got a trial (BD2: trial only once)")
		}
		if m.rowStatus(prior.ID) != "canceled" {
			t.Errorf("step 1 must mirror the Stripe-side subscription: row status %q", m.rowStatus(prior.ID))
		}
		// every status outside {canceled, incomplete_expired} refuses; those two allow
		for status, exists := range map[string]bool{"trialing": true, "active": true, "past_due": true, "unpaid": true, "paused": true, "incomplete": true, "canceled": false, "incomplete_expired": false} {
			st, tok, sc := m.extraStore()
			cu := m.pinned(st, tok)
			sub := m.fake.AddSubscription(billingtest.Sub{Customer: cu, Status: status, StoreMeta: st})
			created := len(m.fake.CallsExact("POST", "/v1/checkout/sessions"))
			_, err := m.svc.StartCheckout(m.ctx, m.f.runtime, sc, tok, "price_Cb08Month")
			if exists {
				if !errors.Is(err, billing.ErrSubscriptionExists) {
					t.Errorf("%s: %v, want ErrSubscriptionExists (409)", status, err)
				}
				if len(m.fake.CallsExact("POST", "/v1/checkout/sessions")) != created {
					t.Errorf("%s: a session was created despite the existing subscription", status)
				}
			} else if err != nil {
				t.Errorf("%s: %v, want a session", status, err)
			}
			if m.rowStatus(sub.ID) != status {
				t.Errorf("%s: the refusal did not mirror the subscription (row %q)", status, m.rowStatus(sub.ID))
			}
		}
	})

	t.Run("parallel checkouts leave one open session known to the database", func(t *testing.T) {
		m := cbmSetup(t, false, false)
		m.pinned(m.store, m.token)
		const n = 6
		m.fake.DelayNext("checkout", 300*time.Millisecond)
		var wg sync.WaitGroup
		errs := make([]error, n)
		for i := 0; i < n; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				_, errs[i] = m.checkout("price_Cb08Month")
			}()
		}
		wg.Wait()
		for i, err := range errs {
			if err != nil {
				t.Errorf("parallel call %d: %v", i, err)
			}
		}
		created := len(m.fake.CallsExact("POST", "/v1/checkout/sessions"))
		if created != n || m.fake.OpenSessions() != 1 {
			t.Errorf("created %d sessions, %d open; want %d created and exactly one open (older ones expired)", created, m.fake.OpenSessions(), n)
		}
		var expires int
		for _, c := range m.fake.CallsTo("POST", "/v1/checkout/sessions/") {
			if strings.HasSuffix(c.Path, "/expire") {
				expires++
				id := strings.TrimSuffix(strings.TrimPrefix(c.Path, "/v1/checkout/sessions/"), "/expire")
				if c.IdempotencyKey != "lc:billing:expire:v1:"+id {
					t.Errorf("expire key %q for %s", c.IdempotencyKey, id)
				}
			}
		}
		if expires != n-1 {
			t.Errorf("%d expire calls, want %d", expires, n-1)
		}
		open := ""
		for _, s := range m.fake.Sessions() {
			if s.Status == "open" {
				open = s.ID
			}
		}
		if got := cbxOne(t, m.f, `SELECT open_checkout_session_id FROM billing.store_customers WHERE store_id=$1`, m.store); got != open {
			t.Errorf("the database knows %q, the open session is %q", got, open)
		}
	})

	t.Run("portal: no customer is 409, the body, no key", func(t *testing.T) {
		m := cbmSetup(t, false, false)
		if _, err := m.svc.OpenPortal(m.ctx, m.f.runtime, m.scope, m.token); !errors.Is(err, billing.ErrNoCustomer) {
			t.Fatalf("portal without a customer: %v, want ErrNoCustomer (409 no_billing_customer)", err)
		}
		if len(m.fake.CallsTo("POST", "/v1/billing_portal")) != 0 || len(m.fake.Customers()) != 0 {
			t.Error("the portal path called Stripe or created a customer without one being pinned")
		}
		cus := m.pinned(m.store, m.token)
		u, err := m.svc.OpenPortal(m.ctx, m.f.runtime, m.scope, m.token)
		if err != nil || !strings.HasPrefix(u, "https://billing.stripe.com/") {
			t.Fatalf("portal: %q %v", u, err)
		}
		c := m.fake.CallsTo("POST", "/v1/billing_portal/sessions")
		if len(c) != 1 || cbmKeys(cbmForm(c[0])) != "customer,return_url" || cbmForm(c[0])["customer"] != cus ||
			cbmForm(c[0])["return_url"] != cbmOrigin+"/billing?store="+m.store || c[0].IdempotencyKey != "" {
			t.Errorf("portal call: %+v", c)
		}
		plain := lcTokenFor(t, m.f, m.tenant, m.store)
		if _, err := m.svc.OpenPortal(m.ctx, m.f.runtime, m.scopeOf(m.store, plain), plain); err == nil {
			t.Error("a member without billing:manage opened the portal")
		}
	})

	t.Run("webhook: rejected inputs cause no Stripe call and no write", func(t *testing.T) {
		m := cbmSetup(t, false, false)
		cus := m.pinned(m.store, m.token)
		sub := m.fake.AddSubscription(billingtest.Sub{Customer: cus, Status: "active", StoreMeta: m.store})
		body := m.fake.SubscriptionEvent("customer.subscription.updated", sub.ID, billingtest.EventOpts{})
		calls := len(m.fake.Calls())
		big := append([]byte(`{"pad":"`), bytes.Repeat([]byte("a"), 70*1024)...)
		big = append(big, []byte(`"}`)...)
		cases := map[string]*httptest.ResponseRecorder{
			"no signature header":       m.post(body, ""),
			"forged signature":          m.post(body, billingtest.Sign("whsec_"+strings.Repeat("f", 32), body, time.Now())),
			"old signature":             m.post(body, billingtest.Sign(m.cfg.WebhookSecret, body, time.Now().Add(-10*time.Minute))),
			"future signature":          m.post(body, billingtest.Sign(m.cfg.WebhookSecret, body, time.Now().Add(10*time.Minute))),
			"signature of another body": m.post(body, billingtest.Sign(m.cfg.WebhookSecret, []byte("{}"), time.Now())),
			"garbage body":              m.signed([]byte("not json")),
			"livemode event":            m.signed(m.fake.SubscriptionEvent("customer.subscription.updated", sub.ID, billingtest.EventOpts{Livemode: true})),
			"body over 64 KiB":          m.signed(big),
			"empty body":                m.signed(nil),
		}
		for name, rec := range cases {
			if rec.Code != 400 && rec.Code != 413 {
				t.Errorf("%s: HTTP %d, want 400 (or 413 for the oversize body)", name, rec.Code)
			}
		}
		if len(m.fake.Calls()) != calls {
			t.Errorf("a rejected delivery called Stripe (%d -> %d calls)", calls, len(m.fake.Calls()))
		}
		if cbxOne(t, m.f, `SELECT count(*)::text FROM billing.subscriptions WHERE store_id=$1`, m.store) != "0" {
			t.Error("a rejected delivery wrote a row")
		}
		// GET is not a delivery
		req := httptest.NewRequest(http.MethodGet, "/v1/platform/stripe/webhook", nil)
		w := httptest.NewRecorder()
		m.hook.ServeHTTP(w, req)
		if w.Code < 400 {
			t.Errorf("GET on the webhook answered %d", w.Code)
		}
	})

	t.Run("webhook: the event is a wake-up; state comes from the retrieved subscription (D8, F-B10)", func(t *testing.T) {
		m := cbmSetup(t, false, false)
		cus := m.pinned(m.store, m.token)
		start, end := time.Now().Add(-48*time.Hour).Truncate(time.Second), time.Now().Add(27*24*time.Hour).Truncate(time.Second)
		sub := m.fake.AddSubscription(billingtest.Sub{Customer: cus, Status: "active", StoreMeta: m.store, PriceID: "price_Cb08Month",
			PeriodStart: start.Unix(), PeriodEnd: end.Unix(), CancelAtPeriodEnd: true})
		// the event claims canceled; the subscription at Stripe is active: the row must say active
		lie := m.fake.RawEvent("customer.subscription.updated", billingtest.EventOpts{}, func() map[string]any {
			o := m.fake.SubObject(sub.ID)
			o["status"] = "canceled"
			return o
		}())
		before := len(m.fake.CallsTo("GET", "/v1/subscriptions/"))
		rec := m.signed(lie)
		if rec.Code != 200 {
			t.Fatalf("delivery: HTTP %d %s", rec.Code, rec.Body)
		}
		if got := m.fake.CallsTo("GET", "/v1/subscriptions/"); len(got) != before+1 || got[len(got)-1].Path != "/v1/subscriptions/"+sub.ID {
			t.Fatalf("the handler must retrieve the subscription once: %+v", got)
		}
		var status, price string
		var ps, pe time.Time
		var cancel bool
		if err := m.f.owner.QueryRow(m.ctx, `SELECT status,price_id,current_period_start,current_period_end,cancel_at_period_end FROM billing.subscriptions WHERE stripe_subscription_id=$1`, sub.ID).Scan(&status, &price, &ps, &pe, &cancel); err != nil {
			t.Fatalf("mirrored row: %v", err)
		}
		if status != "active" || price != "price_Cb08Month" || !ps.Equal(start) || !pe.Equal(end) || !cancel {
			t.Errorf("row: status=%s price=%s period=%s..%s cancel=%v; want the RETRIEVED values (active, item-level period fields, cancel flag)", status, price, ps, pe, cancel)
		}
		if got := cbxOne(t, m.f, `SELECT stripe_created_at::text FROM billing.subscriptions WHERE stripe_subscription_id=$1`, sub.ID); got == "" {
			t.Error("stripe_created_at missing")
		}
		// a later state change at Stripe follows on the next wake-up
		m.fake.SetSubStatus(sub.ID, "past_due")
		if rec := m.signed(m.fake.SubscriptionEvent("customer.subscription.updated", sub.ID, billingtest.EventOpts{})); rec.Code != 200 || m.rowStatus(sub.ID) != "past_due" {
			t.Errorf("past_due: HTTP %d row %s", rec.Code, m.rowStatus(sub.ID))
		}
		// every handled type wakes a retrieve
		for _, typ := range []string{"customer.subscription.created", "customer.subscription.deleted", "customer.subscription.paused", "customer.subscription.resumed"} {
			n := len(m.fake.CallsTo("GET", "/v1/subscriptions/"))
			if rec := m.signed(m.fake.SubscriptionEvent(typ, sub.ID, billingtest.EventOpts{})); rec.Code != 200 || len(m.fake.CallsTo("GET", "/v1/subscriptions/")) != n+1 {
				t.Errorf("%s: HTTP %d, retrieves %d -> %d", typ, rec.Code, n, len(m.fake.CallsTo("GET", "/v1/subscriptions/")))
			}
		}
		// invoice.* resolve the subscription through parent.subscription_details only (F-B3, API >= basil)
		for _, typ := range []string{"invoice.paid", "invoice.payment_failed"} {
			m.fake.SetSubStatus(sub.ID, "active")
			n := len(m.fake.CallsTo("GET", "/v1/subscriptions/"))
			if rec := m.signed(m.fake.InvoiceEvent(typ, sub.ID, billingtest.EventOpts{})); rec.Code != 200 || len(m.fake.CallsTo("GET", "/v1/subscriptions/")) != n+1 || m.rowStatus(sub.ID) != "active" {
				t.Errorf("%s: HTTP %d, retrieves %d -> %d, row %s", typ, rec.Code, n, len(m.fake.CallsTo("GET", "/v1/subscriptions/")), m.rowStatus(sub.ID))
			}
		}
		n := len(m.fake.Calls())
		for name, body := range map[string][]byte{
			"invoice without a subscription parent":          m.fake.InvoiceEvent("invoice.paid", "", billingtest.EventOpts{}),
			"invoice with only the pre-basil top-level id":   m.fake.RawEvent("invoice.paid", billingtest.EventOpts{}, map[string]any{"id": "in_x", "object": "invoice", "subscription": sub.ID}),
			"charge.succeeded":                               m.fake.RawEvent("charge.succeeded", billingtest.EventOpts{}, map[string]any{"id": "ch_x", "object": "charge"}),
			"customer.created":                               m.fake.RawEvent("customer.created", billingtest.EventOpts{}, map[string]any{"id": "cus_x", "object": "customer"}),
			"customer.subscription.trial_will_end":           m.fake.RawEvent("customer.subscription.trial_will_end", billingtest.EventOpts{}, m.fake.SubObject(sub.ID)),
			"checkout.session.completed (not handled in v1)": m.fake.RawEvent("checkout.session.completed", billingtest.EventOpts{}, map[string]any{"id": "cs_x", "object": "checkout.session"}),
		} {
			if rec := m.signed(body); rec.Code != 200 {
				t.Errorf("%s: HTTP %d, want 200 ignored", name, rec.Code)
			}
		}
		if len(m.fake.Calls()) != n {
			t.Errorf("ignored events called Stripe (%d -> %d)", n, len(m.fake.Calls()))
		}
	})

	t.Run("webhook: retrieve failure and timeout are 503 so Stripe retries (F-B9); redelivery applies", func(t *testing.T) {
		m := cbmSetup(t, false, false)
		cus := m.pinned(m.store, m.token)
		sub := m.fake.AddSubscription(billingtest.Sub{Customer: cus, Status: "active", StoreMeta: m.store})
		body := m.fake.SubscriptionEvent("customer.subscription.created", sub.ID, billingtest.EventOpts{})
		m.fake.FailNext("retrieve", 500)
		if rec := m.signed(body); rec.Code != 503 {
			t.Fatalf("retrieve 500: HTTP %d, want 503", rec.Code)
		}
		if m.rowStatus(sub.ID) != "" {
			t.Fatal("a failed retrieve wrote a row")
		}
		m.fake.FailNext("retrieve", 404)
		if rec := m.signed(body); rec.Code != 503 && rec.Code != 200 {
			t.Errorf("retrieve 404: HTTP %d", rec.Code)
		}
		if rec := m.signed(body); rec.Code != 200 || m.rowStatus(sub.ID) != "active" {
			t.Fatalf("redelivery: HTTP %d row %q", rec.Code, m.rowStatus(sub.ID))
		}
		// a hung Stripe: the 10 s client timeout, no retry inside the request, then 503
		m.fake.HangNext("retrieve")
		started := time.Now()
		rec := m.signed(body)
		if rec.Code != 503 || time.Since(started) > 25*time.Second {
			t.Errorf("hung retrieve: HTTP %d after %s, want 503 within the 10 s client timeout", rec.Code, time.Since(started))
		}
		if n := len(m.fake.CallsTo("GET", "/v1/subscriptions/")); n != 4 && n != 3 {
			t.Errorf("%d retrieve calls: none of them may be retried inside a request", n)
		}
	})

	t.Run("webhook: mismatch, duplicate, unknown customer -> 200 + one billing_ops_alert without extra ids; multi-item not applied", func(t *testing.T) {
		m := cbmSetup(t, false, false)
		cus := m.pinned(m.store, m.token)
		other, otherTok, _ := m.extraStore()
		m.pinned(other, otherTok)
		alertLines := func(sub string) []string {
			var out []string
			for _, l := range m.logs.Lines("billing_ops_alert") {
				if strings.Contains(l, sub) {
					out = append(out, l)
				}
			}
			return out
		}
		check := func(name, code string, sub billingtest.Sub, ids ...string) {
			t.Helper()
			s := m.fake.AddSubscription(sub)
			if rec := m.signed(m.fake.SubscriptionEvent("customer.subscription.updated", s.ID, billingtest.EventOpts{})); rec.Code != 200 {
				t.Errorf("%s: HTTP %d, want 200", name, rec.Code)
			}
			lines := alertLines(s.ID)
			if len(lines) != 1 || !strings.Contains(lines[0], "code="+code) {
				t.Errorf("%s: alert lines %v, want exactly one with code=%s", name, lines, code)
				return
			}
			for _, id := range ids {
				if strings.Contains(lines[0], id) {
					t.Errorf("%s: the alert line carries %q (no ids beyond the subscription id)", name, id)
				}
			}
		}
		check("metadata of another store", "mismatch", billingtest.Sub{Customer: cus, Status: "active", StoreMeta: other}, m.store, cus, other)
		check("NULL metadata (Dashboard-created)", "mismatch", billingtest.Sub{Customer: cus, Status: "active", NoMetadata: true}, m.store, cus)
		check("unknown customer", "unknown_customer", billingtest.Sub{Customer: "cus_NotOurs0001", Status: "active", StoreMeta: m.store}, m.store, "cus_NotOurs0001")
		good := m.fake.AddSubscription(billingtest.Sub{Customer: cus, Status: "active", StoreMeta: m.store})
		if rec := m.signed(m.fake.SubscriptionEvent("customer.subscription.created", good.ID, billingtest.EventOpts{})); rec.Code != 200 || m.rowStatus(good.ID) != "active" {
			t.Fatalf("first subscription: HTTP %d row %q", rec.Code, m.rowStatus(good.ID))
		}
		check("a second non-terminal subscription", "duplicate", billingtest.Sub{Customer: cus, Status: "active", StoreMeta: m.store}, m.store, cus)
		if cbxOne(t, m.f, `SELECT count(*)::text FROM billing.subscriptions WHERE store_id=$1`, m.store) != "2" {
			t.Error("the duplicate must still be mirrored (no auto-cancel)")
		}
		for _, calls := range m.fake.CallsTo("POST", "/v1/") {
			if strings.Contains(calls.Path, "cancel") || strings.HasPrefix(calls.Path, "/v1/subscriptions") {
				t.Errorf("billing called %s: it must never cancel a subscription", calls.Path)
			}
		}
		if cbxOne(t, m.f, `SELECT count(*)::text FROM billing.subscriptions WHERE store_id=$1`, other) != "0" {
			t.Error("a mismatching subscription was mirrored under the other store")
		}
		multi := m.fake.AddSubscription(billingtest.Sub{Customer: cus, Status: "active", StoreMeta: m.store, Items: 2})
		if rec := m.signed(m.fake.SubscriptionEvent("customer.subscription.updated", multi.ID, billingtest.EventOpts{})); rec.Code != 200 {
			t.Errorf("multi-item: HTTP %d, want 200", rec.Code)
		}
		if m.rowStatus(multi.ID) != "" || len(m.logs.Lines("billing_multi_item")) < 1 {
			t.Errorf("multi-item: row %q, multi-item log lines %d (want not applied and logged)", m.rowStatus(multi.ID), len(m.logs.Lines("billing_multi_item")))
		}
	})

	t.Run("status: refresh-on-read after 10 min, stale flag on a Stripe error, plan cache and filtering", func(t *testing.T) {
		m := cbmSetup(t, false, false)
		cus := m.pinned(m.store, m.token)
		sub := m.fake.AddSubscription(billingtest.Sub{Customer: cus, Status: "active", StoreMeta: m.store})
		// B10: refresh-on-read applies only when a stored row is older than 10 min, so the first row arrives by webhook
		if rec := m.signed(m.fake.SubscriptionEvent("customer.subscription.created", sub.ID, billingtest.EventOpts{})); rec.Code != 200 || m.rowStatus(sub.ID) != "active" {
			t.Fatalf("first mirror by webhook: HTTP %d row %q", rec.Code, m.rowStatus(sub.ID))
		}
		lists := func() []billingtest.Call { return m.fake.CallsTo("GET", "/v1/subscriptions") }
		n := len(lists())
		st, err := m.svc.Status(m.ctx, m.f.runtime, m.scope, m.token)
		if err != nil || len(lists()) != n || st.Stale || st.Standing != billing.Good || !st.CustomerPinned || len(st.Subscriptions) != 1 || st.Subscriptions[0].Status != "active" {
			t.Fatalf("fresh row: no refresh expected: %+v %v lists %d -> %d", st, err, n, len(lists()))
		}
		// age the row past 10 minutes and change the truth at Stripe
		m.age(sub.ID, 11*time.Minute)
		m.fake.SetSubStatus(sub.ID, "past_due")
		st, err = m.svc.Status(m.ctx, m.f.runtime, m.scope, m.token)
		if err != nil || st.Stale || st.Standing != billing.Grace || st.Subscriptions[0].Status != "past_due" {
			t.Fatalf("after refresh: %+v %v", st, err)
		}
		got := lists()
		lq, _ := url.ParseQuery(got[len(got)-1].RawQuery)
		if len(got) != n+1 || lq.Get("customer") != cus || lq.Get("status") != "all" || lq.Get("limit") != "100" {
			t.Errorf("refresh list query %q, want customer=<pinned>&status=all&limit=100", got[len(got)-1].RawQuery)
		}
		// Stripe down: the database state with stale=true, no error
		m.age(sub.ID, 11*time.Minute)
		m.fake.SetSubStatus(sub.ID, "canceled")
		m.fake.FailNext("list", 500)
		st, err = m.svc.Status(m.ctx, m.f.runtime, m.scope, m.token)
		if err != nil || !st.Stale || st.Standing != billing.Grace || st.Subscriptions[0].Status != "past_due" {
			t.Errorf("Stripe error on refresh: %+v %v, want the stored past_due state with stale=true", st, err)
		}
		// plans: only active, recurring, test-mode prices; cached 10 minutes; a failed fetch omits that plan
		plans := st.Plans
		if len(plans) != 1 || plans[0].PriceID != "price_Cb08Month" || plans[0].Name != "Pro Monthly" || plans[0].AmountMinor != 30000 || !strings.EqualFold(plans[0].Currency, "twd") || plans[0].Interval != "month" {
			t.Fatalf("plans %+v, want only price_Cb08Month (inactive, live-mode and one-time prices dropped)", plans)
		}
		priceCalls := len(m.fake.CallsTo("GET", "/v1/prices/"))
		if _, err := m.svc.Status(m.ctx, m.f.runtime, m.scope, m.token); err != nil || len(m.fake.CallsTo("GET", "/v1/prices/")) != priceCalls {
			t.Errorf("the plan list must be cached for 10 minutes (price calls %d -> %d)", priceCalls, len(m.fake.CallsTo("GET", "/v1/prices/")))
		}
		for _, c := range m.fake.CallsTo("GET", "/v1/prices/") {
			if q, _ := url.ParseQuery(c.RawQuery); len(q["expand[]"]) != 1 || q["expand[]"][0] != "product" {
				t.Errorf("price fetch %q must expand[]=product", c.RawQuery)
			}
		}
		if len(m.fake.CallsTo("GET", "/v1/prices/")) != len(m.cfg.PriceIDs) {
			t.Errorf("%d price fetches for %d configured ids", len(m.fake.CallsTo("GET", "/v1/prices/")), len(m.cfg.PriceIDs))
		}
	})

	t.Run("plans: a failed price fetch omits that plan and logs; the next read tries again", func(t *testing.T) {
		m := cbmSetup(t, false, false)
		m.fake.FailNext("price", 500)
		st, err := m.svc.Status(m.ctx, m.f.runtime, m.scope, m.token)
		if err != nil || len(st.Plans) != 0 {
			t.Fatalf("with the only sellable price failing: plans=%+v err=%v, want none and no error", st.Plans, err)
		}
		if len(m.logs.Lines("billing_stripe_error")) < 1 || !strings.Contains(strings.Join(m.logs.Lines("billing_stripe_error"), "\n"), "price_retrieve") {
			t.Errorf("a failed price fetch must be logged: %q", m.logs.String())
		}
		if st, err := m.svc.Status(m.ctx, m.f.runtime, m.scope, m.token); err != nil || len(st.Plans) != 1 || st.Plans[0].PriceID != "price_Cb08Month" {
			t.Errorf("the next read after Stripe recovered: plans=%+v err=%v, want the plan back", st.Plans, err)
		}
	})

	t.Run("bearer URLs never reach logs, the database or errors (I11)", func(t *testing.T) {
		m := cbmSetup(t, false, false)
		url1, err := m.checkout("price_Cb08Month")
		if err != nil {
			t.Fatal(err)
		}
		portal, err := m.svc.OpenPortal(m.ctx, m.f.runtime, m.scope, m.token)
		if err != nil {
			t.Fatal(err)
		}
		// error paths: a failing portal and checkout
		m.fake.FailNext("portal", 500)
		_, perr := m.svc.OpenPortal(m.ctx, m.f.runtime, m.scope, m.token)
		m.fake.FailNext("checkout", 500)
		_, cerr := m.checkout("price_Cb08Month")
		for _, secret := range []string{url1, portal, "fakebearer", "checkout.stripe.com", "billing.stripe.com/p/session", m.cfg.SecretKey, m.cfg.WebhookSecret, "Bearer "} {
			if strings.Contains(m.logs.String(), secret) {
				t.Errorf("the log output contains %q", secret)
			}
			for _, err := range []error{perr, cerr} {
				if err != nil && strings.Contains(err.Error(), secret) {
					t.Errorf("an error contains %q: %v", secret, err)
				}
			}
		}
		// nothing bearer-shaped is persisted anywhere in the billing or audit tables
		for _, q := range []string{
			`SELECT count(*) FROM billing.store_customers t WHERE t::text ~* '(fakebearer|checkout\.stripe\.com|billing\.stripe\.com)'`,
			`SELECT count(*) FROM billing.subscriptions t WHERE t::text ~* '(fakebearer|checkout\.stripe\.com|billing\.stripe\.com)'`,
			`SELECT count(*) FROM ops.audit_events t WHERE t::text ~* '(fakebearer|checkout\.stripe\.com|billing\.stripe\.com)'`,
		} {
			if n := countRows(t, m.f.owner, q); n != 0 {
				t.Errorf("%s -> %d rows hold a bearer URL", q, n)
			}
		}
		if len(m.logs.String()) == 0 {
			t.Error("no log output captured: the absence checks above would be vacuous")
		}
		// every call of the client pins the API version (F-B10) and uses the configured key (fingerprint only), and a GET never carries a key
		sum := sha256.Sum256([]byte(m.cfg.SecretKey))
		wantFP := hex.EncodeToString(sum[:4])
		for _, call := range m.fake.Calls() {
			if call.StripeVersion != billingtest.APIVersion || call.KeyFingerprint != wantFP || (call.Method == "GET" && call.IdempotencyKey != "") {
				t.Errorf("call %s %s: Stripe-Version=%q keyFP=%q idempotency=%q", call.Method, call.Path, call.StripeVersion, call.KeyFingerprint, call.IdempotencyKey)
			}
		}
		body, _ := io.ReadAll(io.LimitReader(strings.NewReader(m.logs.String()), 1<<20))
		if !regexp.MustCompile(`billing_stripe_error`).Match(body) {
			t.Error("the failure paths produced no billing_stripe_error line: the error branches were not exercised")
		}
	})
}
