package foundation_test

// SP07 (contracts/stripe-psp-v1.md §14) plus the harness shared by the other
// Stripe B1 MOCK/HTTP gates in this package (prefix sst).
//
// Tier (docs/delivery/units/stripe-b1-tests-b.md): MOCK = real PG 18 + real
// River + the SAME assembly functions the binaries call (checkout.
// NewHostedPaymentService, stripeadmin.Open) + stripetest. The binaries refuse
// PROVIDER_MOCK by design, so nothing here is a binary-level run.
//
// Every account, credential, qualification and method row is written through
// stripeadmin (registrar login + fake transport), never through a merchant
// runtime role. Fixtures that deliberately mutate rows as the migration owner
// (drift, aging) are named at their call site so the evidence can list them.

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"
	"livecommerce/internal/checkout"
	"livecommerce/internal/command"
	"livecommerce/internal/integrations/accounts"
	"livecommerce/internal/integrations/psp/stripe/stripetest"
	"livecommerce/internal/payments/stripeadmin"
	"livecommerce/internal/platform"
	"livecommerce/internal/pricing"
	"livecommerce/internal/storefront"
)

const sstReturnURL = "https://checkout.example.test/payment/return"

// sstLogin creates a disposable LOGIN member of one NOLOGIN authority group with
// INHERIT and no SET, the shape ValidateStripe{Ingress,Registrar}Pool requires
// (contract §6.3). It returns a DSN; the role is dropped at cleanup, after every
// pool opened later (t.Cleanup is LIFO).
func sstLogin(t *testing.T, f *testFixture, group string) string {
	t.Helper()
	name := "sst_" + t04Tag()
	password := hex.EncodeToString(randomBytes(24))
	quoted := pgx.Identifier{name}.Sanitize()
	mustExec(t, f.owner, fmt.Sprintf(`CREATE ROLE %s LOGIN NOSUPERUSER NOBYPASSRLS NOCREATEDB NOCREATEROLE NOREPLICATION PASSWORD '%s'`, quoted, password))
	t.Cleanup(func() { mustExec(t, f.owner, `DROP ROLE `+quoted) })
	mustExec(t, f.owner, fmt.Sprintf(`GRANT %s TO %s WITH INHERIT TRUE, SET FALSE`, pgx.Identifier{group}.Sanitize(), quoted))
	return roleURL(t, f.databaseURL, name, password)
}

func sstKeyring(t *testing.T, id string, key []byte) *accounts.Keyring {
	t.Helper()
	k, err := accounts.NewKeyring(id, map[string][]byte{id: key}, randomBytes(32))
	if err != nil {
		t.Fatal(err)
	}
	return k
}

// sstEnv is the Stripe side of one fixture database: a fake api.stripe.com, a
// registrar (the only writer of Stripe accounts/methods), separately keyed API
// and webhook-signing custody, and the hosted service the API binary builds.
type sstEnv struct {
	f       *testFixture
	keys    *accounts.Keyring // API-key custody: registrar seals, payment-worker opens
	signing *accounts.Keyring // webhook-signing custody: different material on purpose (§0.2)
	fake    *stripetest.Server
	reg     *stripeadmin.Registrar
	hosted  *pgxpool.Pool
	jobs    *river.Client[pgx.Tx]
	scfg    checkout.StripeHostedConfig
	pcfg    checkout.HostedConfig
	svc     *checkout.HostedPaymentStarter
}

func sstNewEnv(t *testing.T, f *testFixture, keys *accounts.Keyring) *sstEnv {
	t.Helper()
	ctx := context.Background()
	e := &sstEnv{f: f, keys: keys, signing: sstKeyring(t, "stripe_signing_fixture", randomBytes(32)),
		fake: stripetest.New("acct_FakeDefault0001"), scfg: checkout.StripeHostedConfig{ReturnURL: sstReturnURL},
		pcfg: checkout.HostedConfig{ReturnURL: sstReturnURL, NotifyURL: "https://checkout.example.test/payment/notify"}}
	t.Cleanup(e.fake.Close)
	var err error
	// stripeadmin.Open validates the registrar login through platform.OpenStripeRegistrarPool.
	if e.reg, err = stripeadmin.Open(ctx, sstLogin(t, f, "commerce_payment_registrar"), keys, e.signing, e.fake.Transport()); err != nil {
		t.Fatalf("open registrar: %v", err)
	}
	t.Cleanup(e.reg.Close)
	if e.hosted, err = platform.OpenHostedPool(ctx, hpRole(t, f)); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(e.hosted.Close)
	if e.jobs, err = river.NewClient(riverpgxv5.New(e.hosted), &river.Config{Schema: "river_payment"}); err != nil {
		t.Fatal(err)
	}
	e.svc = e.service(t, "PROVIDER_MOCK", e.scfg)
	return e
}

// service builds a hosted service over the shared hosted pool. It is exactly the
// call cmd/api makes (PAYUNi config stays required in B1, contract brief §cmd/api).
func (e *sstEnv) service(t *testing.T, profile string, stripeCfg checkout.StripeHostedConfig) *checkout.HostedPaymentStarter {
	t.Helper()
	s, err := checkout.NewHostedPaymentService(context.Background(), e.hosted, e.jobs, profile, e.keys,
		checkout.HostedProviders{PAYUNi: &e.pcfg, Stripe: &stripeCfg})
	if err != nil {
		t.Fatalf("hosted service (%s): %v", profile, err)
	}
	return s
}

// sstStore is one store with a registered, qualified, enabled Stripe method.
type sstStore struct {
	p                                          psHarness
	scope                                      stripeadmin.Scope
	account, secret, connection, qualification string
	method                                     int64
}

// seed registers a Stripe account for the store of p through the registrar:
// Register (VerifyAccount against the fake) -> Qualify (PROVIDER_MOCK, no network)
// -> SetMethod (TWD whole-dollar bounds, version 1).
func (e *sstEnv) seed(t *testing.T, p psHarness) sstStore {
	t.Helper()
	ctx := context.Background()
	s := sstStore{p: p, scope: stripeadmin.Scope{TenantID: p.f.tenantA, StoreID: p.f.storeA1, PrincipalID: p.f.principalA},
		account: "acct_T" + t04Tag(), secret: "sk_test_" + hex.EncodeToString(randomBytes(12))}
	if err := e.fake.AddAccount(s.account, s.secret); err != nil {
		t.Fatal(err)
	}
	var err error
	if s.connection, err = e.reg.Register(ctx, s.scope, s.account, s.secret); err != nil {
		t.Fatalf("register Stripe account: %v", err)
	}
	s.requalify(t, e, 1)
	if s.method, err = e.reg.SetMethod(ctx, s.scope, s.methodInput(0, true, true, 2500, 99999900)); err != nil || s.method != 1 {
		t.Fatalf("enable Stripe method: version=%d err=%v", s.method, err)
	}
	return s
}

func (s *sstStore) requalify(t *testing.T, e *sstEnv, credentialVersion int64) {
	t.Helper()
	var err error
	if s.qualification, err = e.reg.Qualify(context.Background(), s.scope, stripeadmin.QualifyInput{
		ConnectionID: s.connection, AccountID: s.account, SecretKey: s.secret, Profile: "PROVIDER_MOCK",
		Currency: "TWD", ReturnURL: sstReturnURL, ExpectedVersion: credentialVersion, AmountMinor: 2500}); err != nil {
		t.Fatalf("qualify Stripe method: %v", err)
	}
}

func (s sstStore) methodInput(expected int64, enabled, visible bool, min, max int64) stripeadmin.MethodInput {
	return stripeadmin.MethodInput{MarketID: s.p.market.ID, Country: "TW", ConnectionID: s.connection,
		QualificationID: s.qualification, ExpectedVersion: expected, Enabled: enabled, Visible: visible,
		Sort: 1, MinMinor: min, MaxMinor: max, NameHans: "Stripe", NameHant: "Stripe", NameEN: "Stripe"}
}

func (s sstStore) input(locale string) checkout.HostedInput {
	return checkout.HostedInput{OrderID: s.p.hold.OrderID, MethodCode: "stripe_checkout", MethodVersion: s.method, Locale: locale}
}

func (s sstStore) begin(svc *checkout.HostedPaymentStarter, key string, in checkout.HostedInput) (checkout.PaymentResult, error) {
	return svc.BeginHosted(context.Background(), s.p.cap.Token, s.p.f.storeA1, key, in)
}

// sstMoreHold adds another buyer/order/hold to the store of p (2 of its 10
// units each), so a workload can race several orders of one store.
func sstMoreHold(t *testing.T, p psHarness) psHarness {
	t.Helper()
	q := p
	q.bcHarness.prepare(t, mustIssue(t, p.cqHarness.service, p.f.storeA1), []storefront.Item{{SKUID: p.stock.skus[0].ID, Quantity: 2}})
	hold, err := q.bcHarness.begin(t04Key("sst-hold"))
	if err != nil {
		t.Fatal(err)
	}
	q.hold = hold
	return q
}

// sstFacts fingerprints every fact one payment start writes (or must not write).
// The River count is global: the shared fixture runs no worker, so an orphan job
// from a rolled-back start would show up here.
func sstFacts(t *testing.T, p psHarness) [9]int64 {
	t.Helper()
	var out [9]int64
	err := p.f.owner.QueryRow(context.Background(), `SELECT
	 (SELECT count(*) FROM checkout.payment_attempts WHERE owner_id=$1),
	 (SELECT count(*) FROM integration.operations WHERE buyer_owner_id=$1),
	 (SELECT count(*) FROM checkout.command_results WHERE owner_id=$1 AND operation='checkout.payment.start'),
	 (SELECT count(*) FROM checkout.events WHERE owner_id=$1 AND action='checkout.payment_started'),
	 (SELECT count(*) FROM payments.stripe_sessions WHERE owner_id=$1),
	 (SELECT count(*) FROM river_payment.river_job WHERE kind='payment_query_v1'),
	 (SELECT count(*) FROM checkout.orders WHERE owner_id=$1 AND commercial_state='AWAITING_PAYMENT'),
	 (SELECT count(*) FROM inventory.reservations WHERE buyer_owner_id=$1 AND state='PAYMENT_PENDING'),
	 (SELECT count(*) FROM checkout.hosted_payment_pages g JOIN checkout.payment_attempts a ON a.id=g.attempt_id WHERE a.owner_id=$1)`,
		p.cap.Scope.OwnerID).Scan(&out[0], &out[1], &out[2], &out[3], &out[4], &out[5], &out[6], &out[7], &out[8])
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// sstExpectedParams is the complete §9.2/§6.4 create_params key set frozen by
// start_stripe_payment; anything else is a contract change.
func sstExpectedParams(attempt, order, profile, currency, amount, locale, ret string) map[string]string {
	return map[string]string{
		"adaptive_pricing[enabled]": "false", "managed_payments[enabled]": "false", "automatic_tax[enabled]": "false",
		"metadata[lc_attempt]": attempt, "cancel_url": ret, "metadata[lc_order]": order, "client_reference_id": attempt,
		"metadata[lc_profile]": profile, "metadata[lc_v]": "1", "line_items[0][price_data][currency]": currency,
		"mode": "payment", "line_items[0][price_data][unit_amount]": amount, "line_items[0][quantity]": "1",
		"payment_intent_data[metadata][lc_attempt]": attempt, "locale": locale,
		"payment_intent_data[metadata][lc_order]": order, "payment_method_types[0]": "card", "submit_type": "pay",
		"success_url": ret, "ui_mode": "hosted_page",
	}
}

func sstAdmissionDrift(t *testing.T, name string, mutate func(*testing.T, *sstEnv, *sstStore), begin func(*testing.T, *sstEnv, sstStore) (checkout.PaymentResult, error)) {
	t.Helper()
	t.Run(name, func(t *testing.T) {
		p := psSetup(t)
		e := sstNewEnv(t, p.f, sstKeyring(t, "sst_api", randomBytes(32)))
		s := e.seed(t, p)
		before, seeded := sstFacts(t, p), len(e.fake.Requests()) // registrar verification is the only provider traffic
		mutate(t, e, &s)
		if _, err := begin(t, e, s); !errors.Is(err, command.ErrConflict) {
			t.Fatalf("admission drift %q was not a 409 conflict: %v", name, err)
		}
		if after := sstFacts(t, p); after != before {
			t.Fatalf("admission drift %q left partial facts: %v -> %v", name, before, after)
		}
		if got := len(e.fake.Requests()); got != seeded {
			t.Fatalf("API process made provider requests: %d -> %d", seeded, got)
		}
	})
}

// TestStripeSP07Start covers every SP07 clause of contract §14 (with the §0.2
// deltas). No worker runs here: the gate proves the API-side start writes one
// atomic fact set and never performs provider I/O (fake request count 0).
func TestStripeSP07Start(t *testing.T) {
	t.Run("atomic_rows_and_zero_provider_calls", func(t *testing.T) {
		p := psSetup(t)
		e := sstNewEnv(t, p.f, sstKeyring(t, "sst_api", randomBytes(32)))
		s := e.seed(t, p)
		before, seeded := sstFacts(t, p), len(e.fake.Requests()) // registrar verification is the only provider traffic
		res, err := s.begin(e.svc, t04Key("sst-start"), s.input("zh-TW"))
		if err != nil || res.AttemptID == "" || res.OperationID != res.AttemptID || res.JobID < 1 ||
			res.State != "PAYMENT_PENDING" || res.Currency != "TWD" || res.AmountMinor != 2500 || res.OrderID != p.hold.OrderID {
			t.Fatalf("stripe start: %+v err=%v", res, err)
		}
		after := sstFacts(t, p)
		want := [9]int64{1, 1, 1, 1, 1, before[5] + 1, 1, 1, 0}
		if after != want {
			t.Fatalf("atomic fact counts %v want %v (hosted_payment_pages must stay 0: no PAYUNi form)", after, want)
		}
		ctx := context.Background()
		var (
			method, provider, state, profile, account, action, semantic, actor string
			connection                                                         string
			credentialVersion                                                  int64
			providerRef                                                        string
			locale, digest                                                     string
			unit                                                               int64
			params                                                             []byte
			created, expires, send, cutoff                                     time.Time
			sessionID, sessionURL                                              *string
			sends                                                              int
		)
		if err := p.f.owner.QueryRow(ctx, `SELECT a.method_code,o.provider,a.state,a.execution_profile,ss.account_id,o.action,
		  o.semantic_key,o.actor_kind,a.connection_id::text,a.credential_version,coalesce(o.provider_reference,''),ss.locale,
		  encode(ss.config_digest,'hex'),ss.unit_amount,ss.create_params,ss.attempt_created_at,ss.expires_at,
		  ss.send_deadline,ss.handoff_cutoff,ss.session_id,ss.session_url,ss.create_send_count
		  FROM checkout.payment_attempts a JOIN integration.operations o ON o.id=a.id
		  JOIN payments.stripe_sessions ss ON ss.attempt_id=a.id WHERE a.id=$1`, res.AttemptID).
			Scan(&method, &provider, &state, &profile, &account, &action, &semantic, &actor, &connection,
				&credentialVersion, &providerRef, &locale, &digest, &unit, &params, &created, &expires, &send, &cutoff,
				&sessionID, &sessionURL, &sends); err != nil {
			t.Fatal(err)
		}
		_, wantDigest, err := e.scfg.CanonicalDigest()
		if err != nil {
			t.Fatal(err)
		}
		if method != "stripe_checkout" || provider != "stripe" || state != "PAYMENT_PENDING" || profile != "PROVIDER_MOCK" ||
			account != s.account || action != "stripe.checkout_session" || semantic != "payment.stripe:"+res.AttemptID ||
			actor != "BUYER_PAYMENT_QUERY" || connection != s.connection || credentialVersion != 1 || providerRef != "" ||
			locale != "zh-TW" || digest != hex.EncodeToString(wantDigest[:]) || unit != 2500 ||
			sessionID != nil || sessionURL != nil || sends != 0 {
			t.Fatalf("frozen attempt/op/session identity mismatch: method=%s provider=%s state=%s profile=%s account_ok=%t action=%s semantic_ok=%t actor=%s connection_ok=%t credential=%d provider_ref_nil=%t locale=%s digest_ok=%t unit=%d session_nil=%t url_nil=%t sends=%d",
				method, provider, state, profile, account == s.account, action, semantic == "payment.stripe:"+res.AttemptID, actor, connection == s.connection,
				credentialVersion, providerRef == "", locale, digest == hex.EncodeToString(wantDigest[:]), unit, sessionID == nil, sessionURL == nil, sends)
		}
		// §0.1/D4: expires = date_trunc(second, created)+40 min, send window 7 min, handoff stops 5 min early.
		if !expires.Equal(created.Truncate(time.Second).Add(40*time.Minute)) || !send.Equal(created.Add(7*time.Minute)) ||
			!cutoff.Equal(expires.Add(-5*time.Minute)) {
			t.Fatalf("frozen deadlines drifted: created=%s expires=%s send=%s cutoff=%s", created, expires, send, cutoff)
		}
		var got map[string]string
		if err := json.Unmarshal(params, &got); err != nil {
			t.Fatal(err)
		}
		exp := sstExpectedParams(res.AttemptID, p.hold.OrderID, "PROVIDER_MOCK", "twd", "2500", "zh-TW", sstReturnURL)
		exp["line_items[0][price_data][product_data][name]"] = got["line_items[0][price_data][product_data][name]"]
		exp["expires_at"] = got["expires_at"]
		if len(got) != len(exp) {
			keys := make([]string, 0, len(got))
			for k := range got {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			t.Fatalf("create_params key set drifted: %v", keys)
		}
		for k, v := range exp {
			if got[k] != v {
				t.Fatalf("create_params[%s] = %q want %q", k, got[k], v)
			}
		}
		if got["expires_at"] != fmt.Sprint(expires.Unix()) || !regexp.MustCompile(`^Order [0-9A-F]{8}$`).MatchString(got["line_items[0][price_data][product_data][name]"]) {
			t.Fatal("create_params expires_at/product name not the frozen values")
		}
		// job: same tx, ScheduledAt=now (§8), routed to the attempt profile queue, exact args.
		var kind, queue, jobState string
		var args []byte
		var scheduled, dbNow time.Time
		if err := p.f.owner.QueryRow(ctx, `SELECT kind,queue,state,args,scheduled_at,clock_timestamp() FROM river_payment.river_job WHERE id=$1`, res.JobID).
			Scan(&kind, &queue, &jobState, &args, &scheduled, &dbNow); err != nil {
			t.Fatal(err)
		}
		var jobArgs map[string]any
		_ = json.Unmarshal(args, &jobArgs)
		if kind != "payment_query_v1" || queue != "payment_mock_v1" || jobState != "available" || len(jobArgs) != 2 ||
			jobArgs["operation_id"] != res.AttemptID || jobArgs["version"] != float64(1) || scheduled.After(dbNow) || dbNow.Sub(scheduled) > 30*time.Second {
			t.Fatalf("query job not at now on the profile queue: %s %s %s %s", kind, queue, jobState, args)
		}
		// the receipt shares the PAYUNi namespace (I02); the event and operation event exist once.
		var receiptOp string
		var receipt []byte
		if err := p.f.owner.QueryRow(ctx, `SELECT operation,response FROM checkout.command_results WHERE order_id=$1 AND operation='checkout.payment.start'`, p.hold.OrderID).Scan(&receiptOp, &receipt); err != nil || receiptOp != "checkout.payment.start" {
			t.Fatalf("receipt operation %q: %v", receiptOp, err)
		}
		var receiptKeys map[string]any
		_ = json.Unmarshal(receipt, &receiptKeys)
		if len(receiptKeys) != 9 || receiptKeys["attempt_id"] != res.AttemptID || receiptKeys["state"] != "PAYMENT_PENDING" {
			t.Fatalf("receipt keys differ from start_payment's: %s", receipt)
		}
		if n := countRows(t, p.f.owner, `SELECT count(*) FROM integration.operation_events WHERE operation_id=$1 AND reason_code='buyer_payment_started'`, res.AttemptID); n != 1 {
			t.Fatalf("operation event count %d", n)
		}
		if got := len(e.fake.Requests()); got != seeded || e.fake.Counts().Denied != 0 || len(e.fake.CreateKeys()) != 0 {
			t.Fatalf("SP07 zero calls: fake requests %d -> %d, create keys %d", seeded, got, len(e.fake.CreateKeys()))
		}
	})

	t.Run("replay_and_frozen_input_conflicts", func(t *testing.T) {
		p := psSetup(t)
		e := sstNewEnv(t, p.f, sstKeyring(t, "sst_api", randomBytes(32)))
		s := e.seed(t, p)
		key := t04Key("sst-replay")
		first, err := s.begin(e.svc, key, s.input("zh-TW"))
		if err != nil {
			t.Fatal(err)
		}
		stable := sstFacts(t, p)
		again, err := s.begin(e.svc, key, s.input("zh-TW"))
		if err != nil || again != first || sstFacts(t, p) != stable {
			t.Fatalf("replay changed receipt or facts: %+v %v", again, err)
		}
		otherReturn := e.service(t, "PROVIDER_MOCK", checkout.StripeHostedConfig{ReturnURL: "https://checkout.example.test/payment/other"})
		otherProfile := e.service(t, "SANDBOX", e.scfg)
		payuni := s.input("zh-TW")
		payuni.MethodCode, payuni.MethodVersion = "payuni_credit", 1
		for _, v := range []struct {
			name string
			svc  *checkout.HostedPaymentStarter
			in   checkout.HostedInput
		}{
			{"changed_locale", e.svc, s.input("en")},
			{"changed_config_digest", otherReturn, s.input("zh-TW")},
			{"changed_profile", otherProfile, s.input("zh-TW")},
			{"changed_method", e.svc, payuni},
		} {
			if _, err := s.begin(v.svc, key, v.in); !errors.Is(err, command.ErrConflict) {
				t.Fatalf("%s: same key with changed frozen input was not 409: %v", v.name, err)
			}
			if sstFacts(t, p) != stable {
				t.Fatalf("%s changed facts", v.name)
			}
		}
		// A different key never yields a second attempt: the order is already AWAITING_PAYMENT.
		for i := 0; i < 3; i++ {
			if _, err := s.begin(e.svc, t04Key("sst-second"), s.input("zh-TW")); !errors.Is(err, command.ErrConflict) {
				t.Fatalf("second key was not 409: %v", err)
			}
		}
		if sstFacts(t, p) != stable {
			t.Fatal("different keys created a second attempt or job")
		}
	})

	t.Run("concurrent_payuni_vs_stripe_one_winner", func(t *testing.T) {
		h := hpSetup(t)
		e := sstNewEnv(t, h.f, sstKeyring(t, "hosted_fixture", h.key))
		s := e.seed(t, h.psHarness)
		before := sstFacts(t, h.psHarness)
		const n = 8
		errs := make([]error, n)
		var wg sync.WaitGroup
		start := make(chan struct{})
		for i := 0; i < n; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				<-start
				in := s.input("zh-TW")
				if i%2 == 0 { // PAYUNi prepares the same order concurrently
					in.MethodCode, in.MethodVersion = "payuni_credit", 1
				}
				_, errs[i] = s.begin(e.svc, t04Key(fmt.Sprintf("sst-race-%d", i)), in)
			}(i)
		}
		close(start)
		wg.Wait()
		winners := 0
		for i, err := range errs {
			switch {
			case err == nil:
				winners++
			case !errors.Is(err, command.ErrConflict):
				t.Fatalf("racer %d failed with %v (want 409 for losers)", i, err)
			}
		}
		after := sstFacts(t, h.psHarness)
		if winners != 1 || after[0] != 1 || after[1] != 1 || after[6] != 1 || after[7] != 1 || after[5] != before[5]+1 {
			t.Fatalf("winners=%d facts %v -> %v", winners, before, after)
		}
		var method string
		if err := h.f.owner.QueryRow(context.Background(), `SELECT method_code FROM checkout.payment_attempts WHERE owner_id=$1`, h.cap.Scope.OwnerID).Scan(&method); err != nil {
			t.Fatal(err)
		}
		if (method == "stripe_checkout") != (after[4] == 1) || (method == "payuni_credit") != (after[8] == 1) {
			t.Fatalf("winner %s left the other provider's page/session rows: %v", method, after)
		}
	})

	t.Run("mixed_provider_lock_workload_no_deadlock", func(t *testing.T) {
		h := hpSetup(t)
		e := sstNewEnv(t, h.f, sstKeyring(t, "hosted_fixture", h.key))
		s := e.seed(t, h.psHarness)
		holds := []psHarness{h.psHarness, sstMoreHold(t, h.psHarness), sstMoreHold(t, h.psHarness), sstMoreHold(t, h.psHarness)}
		var deadlocksBefore int64
		deadlocks := func() (n int64) {
			if err := h.f.owner.QueryRow(context.Background(), `SELECT deadlocks FROM pg_stat_database WHERE datname=current_database()`).Scan(&n); err != nil {
				t.Fatal(err)
			}
			return
		}
		deadlocksBefore = deadlocks()
		var wg sync.WaitGroup
		results := make([]error, len(holds)*4)
		start := make(chan struct{})
		for i, hold := range holds {
			store := s
			store.p = hold
			for j := 0; j < 4; j++ {
				wg.Add(1)
				go func(slot int, st sstStore, stripeFirst bool) {
					defer wg.Done()
					<-start
					in := st.input("zh-TW")
					if !stripeFirst {
						in.MethodCode, in.MethodVersion = "payuni_credit", 1
					}
					_, results[slot] = st.begin(e.svc, t04Key(fmt.Sprintf("sst-lock-%d", slot)), in)
				}(i*4+j, store, j%2 == 0)
			}
		}
		close(start)
		wg.Wait()
		wins := 0
		for _, err := range results {
			if err == nil {
				wins++
			} else if !errors.Is(err, command.ErrConflict) {
				t.Fatalf("mixed workload error is not a clean 409 (40P01 would surface here): %v", err)
			}
		}
		if wins != len(holds) {
			t.Fatalf("exactly one winner per order expected, got %d for %d orders", wins, len(holds))
		}
		if after := deadlocks(); after != deadlocksBefore {
			t.Fatalf("PG reported %d deadlock(s) during the mixed-provider workload", after-deadlocksBefore)
		}
	})

	t.Run("admission_drift", func(t *testing.T) {
		rebegin := func(t *testing.T, e *sstEnv, s sstStore) (checkout.PaymentResult, error) {
			return s.begin(e.svc, t04Key("sst-drift"), s.input("zh-TW"))
		}
		sstAdmissionDrift(t, "method_stale_version", func(t *testing.T, e *sstEnv, s *sstStore) {
			if v, err := e.reg.SetMethod(context.Background(), s.scope, s.methodInput(1, true, true, 2500, 99999900)); err != nil || v != 2 {
				t.Fatalf("new method version: %d %v", v, err)
			}
		}, rebegin)
		sstAdmissionDrift(t, "method_disabled", func(t *testing.T, e *sstEnv, s *sstStore) {
			v, err := e.reg.SetMethod(context.Background(), s.scope, s.methodInput(1, false, true, 2500, 99999900))
			if err != nil {
				t.Fatal(err)
			}
			s.method = v
		}, rebegin)
		sstAdmissionDrift(t, "method_hidden", func(t *testing.T, e *sstEnv, s *sstStore) {
			v, err := e.reg.SetMethod(context.Background(), s.scope, s.methodInput(1, true, false, 2500, 99999900))
			if err != nil {
				t.Fatal(err)
			}
			s.method = v
		}, rebegin)
		sstAdmissionDrift(t, "amount_below_method_minimum", func(t *testing.T, e *sstEnv, s *sstStore) {
			v, err := e.reg.SetMethod(context.Background(), s.scope, s.methodInput(1, true, true, 5000, 99999900))
			if err != nil {
				t.Fatal(err)
			}
			s.method = v
		}, rebegin)
		sstAdmissionDrift(t, "amount_above_method_maximum", func(t *testing.T, e *sstEnv, s *sstStore) {
			// TWD min 2500: Stripe SANDBOX rejected 100/1200, accepted 2500 (2026-09-29). The registrar can no
			// longer admit a TWD max below the 2500 order, so the drift is applied to the new head as an owner fixture.
			v, err := e.reg.SetMethod(context.Background(), s.scope, s.methodInput(1, true, true, 2500, 99999900))
			if err != nil {
				t.Fatal(err)
			}
			mustExec(t, e.f.owner, `UPDATE payments.method_versions SET min_amount_minor=100,max_amount_minor=1000 WHERE tenant_id=$1 AND store_id=$2 AND code='stripe_checkout' AND version=$3`, s.scope.TenantID, s.scope.StoreID, v)
			s.method = v
		}, rebegin)
		sstAdmissionDrift(t, "qualification_revoked", func(t *testing.T, e *sstEnv, s *sstStore) {
			// owner fixture: no registrar operation revokes.
			qualExec(t, e.f.owner, `UPDATE payments.account_qualifications SET revoked_at=clock_timestamp() WHERE id=$1`, s.qualification)
		}, rebegin)
		sstAdmissionDrift(t, "qualification_expired", func(t *testing.T, e *sstEnv, s *sstStore) {
			qualExec(t, e.f.owner, `UPDATE payments.account_qualifications SET observed_at=clock_timestamp()-interval '2 hours',expires_at=clock_timestamp()-interval '1 hour' WHERE id=$1`, s.qualification)
		}, rebegin)
		sstAdmissionDrift(t, "binding_disabled", func(t *testing.T, e *sstEnv, s *sstStore) {
			mustExec(t, e.f.owner, `UPDATE integration.bindings SET enabled=false WHERE id=(SELECT binding_id FROM integration.merchant_accounts WHERE id=$1)`, s.connection)
		}, rebegin)
		sstAdmissionDrift(t, "wrong_environment_on_method", func(t *testing.T, e *sstEnv, s *sstStore) {
			// owner fixture (replica role: bypasses method_account_target_fk, which pins the
			// environment): the method row claims LIVE while the account and profile are SANDBOX/MOCK.
			tx, err := e.f.owner.Begin(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback(context.Background())
			if _, err := tx.Exec(context.Background(), `SET LOCAL session_replication_role=replica`); err != nil {
				t.Fatal(err)
			}
			if _, err := tx.Exec(context.Background(), `UPDATE payments.method_versions SET environment='LIVE' WHERE connection_id=$1`, s.connection); err != nil {
				t.Fatal(err)
			}
			if err := tx.Commit(context.Background()); err != nil {
				t.Fatal(err)
			}
		}, rebegin)
		sstAdmissionDrift(t, "mock_evidence_on_sandbox_profile", func(t *testing.T, e *sstEnv, s *sstStore) {}, func(t *testing.T, e *sstEnv, s sstStore) (checkout.PaymentResult, error) {
			return s.begin(e.service(t, "SANDBOX", e.scfg), t04Key("sst-sandbox"), s.input("zh-TW"))
		})
		sstAdmissionDrift(t, "hold_expired_before_start", func(t *testing.T, e *sstEnv, s *sstStore) {
			bcDue(t, s.p.bcHarness, s.p.hold)
		}, rebegin)
	})

	t.Run("currency_not_admitted_JPY", func(t *testing.T) {
		// §0.2: JPY and every currency outside HKD/USD/SGD/MYR/TWD is rejected. A JPY store cannot
		// obtain a Stripe method, so no JPY order can ever reach start_stripe_payment.
		p := psSetup(t)
		e := sstNewEnv(t, p.f, sstKeyring(t, "sst_api", randomBytes(32)))
		ctx := context.Background()
		jpStore := randomUUID()
		mustExec(t, p.f.owner, `INSERT INTO control.stores(tenant_id,id,name,currency) VALUES($1,$2,'SP07 JPY store','JPY')`, p.f.tenantA, jpStore)
		mustExec(t, p.f.owner, `INSERT INTO identity.store_grants(tenant_id,store_id,principal_id,permission)
		 SELECT $1,$2,$3,x FROM unnest(ARRAY['store:read','pricing:read','pricing:write','integration:manage','integration:read']) x`, p.f.tenantA, jpStore, p.f.principalA)
		jp, err := pricingScoped(ctx, p.f, p.f.tokens["a"], jpStore, "pricing:write", func(tx pgx.Tx, sc platform.Scope) (pricing.Market, error) {
			return pricing.CreateMarket(ctx, tx, sc, t04Key("sst-jp"), pricing.MarketInput{Code: "jp", Name: "Synthetic JP", Currency: "JPY"})
		})
		if err != nil {
			t.Fatalf("JPY market fixture: %v", err)
		}
		scope := stripeadmin.Scope{TenantID: p.f.tenantA, StoreID: jpStore, PrincipalID: p.f.principalA}
		account, secret := "acct_T"+t04Tag(), "sk_test_"+hex.EncodeToString(randomBytes(12))
		if err := e.fake.AddAccount(account, secret); err != nil {
			t.Fatal(err)
		}
		conn, err := e.reg.Register(ctx, scope, account, secret)
		if err != nil {
			t.Fatalf("register JPY-store account: %v", err)
		}
		qual, err := e.reg.Qualify(ctx, scope, stripeadmin.QualifyInput{ConnectionID: conn, AccountID: account, SecretKey: secret, Profile: "PROVIDER_MOCK",
			Currency: "JPY", ReturnURL: sstReturnURL, ExpectedVersion: 1, AmountMinor: 50})
		if err != nil {
			t.Fatalf("qualify: %v", err)
		}
		_, err = e.reg.SetMethod(ctx, scope, stripeadmin.MethodInput{MarketID: jp.ID, Country: "JP", ConnectionID: conn, QualificationID: qual, ExpectedVersion: 0,
			Enabled: true, Visible: true, Sort: 1, MinMinor: 50, MaxMinor: 99999999, NameHans: "Stripe", NameHant: "Stripe", NameEN: "Stripe"})
		if !errors.Is(err, stripeadmin.ErrRejected) {
			t.Fatalf("JPY Stripe method accepted: %v", err)
		}
		if n := countRows(t, p.f.owner, `SELECT count(*) FROM payments.method_versions WHERE market_id=$1`, jp.ID); n != 0 {
			t.Fatalf("rejected JPY method left %d rows", n)
		}
	})

	t.Run("hold_expires_during_lock_wait", func(t *testing.T) {
		// Same technique as TestBuyerPaymentFinalWaitGate: an owner transaction holds the
		// order row, the hold expires while start waits, start must re-check after the wait.
		p := psSetup(t)
		e := sstNewEnv(t, p.f, sstKeyring(t, "sst_api", randomBytes(32)))
		s := e.seed(t, p)
		name := "sst-wait-" + t04Tag()
		pool, err := platform.OpenHostedPool(context.Background(), withApplicationName(t, hpRole(t, p.f), name))
		if err != nil {
			t.Fatal(err)
		}
		defer pool.Close()
		jobs, err := river.NewClient(riverpgxv5.New(pool), &river.Config{Schema: "river_payment"})
		if err != nil {
			t.Fatal(err)
		}
		svc, err := checkout.NewHostedPaymentService(context.Background(), pool, jobs, "PROVIDER_MOCK", e.keys, checkout.HostedProviders{PAYUNi: &e.pcfg, Stripe: &e.scfg})
		if err != nil {
			t.Fatal(err)
		}
		holder, err := p.f.owner.Begin(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		defer holder.Rollback(context.Background())
		if _, err := holder.Exec(context.Background(), `SELECT 1 FROM checkout.orders WHERE id=$1 FOR UPDATE`, p.hold.OrderID); err != nil {
			t.Fatal(err)
		}
		before := sstFacts(t, p)
		done := make(chan error, 1)
		go func() { _, err := s.begin(svc, t04Key("sst-wait"), s.input("zh-TW")); done <- err }()
		waitForDatabaseLock(t, p.f.owner, name)
		// The order row is held by the owner tx above, so only the reservation can be aged
		// (owner fixture); start re-reads r.expires_at after every wait.
		mustExec(t, p.f.owner, `UPDATE inventory.reservations SET expires_at=clock_timestamp()-interval '1 second',created_at=clock_timestamp()-interval '901 seconds' WHERE id=$1`, p.hold.OrderID)
		if err := holder.Commit(context.Background()); err != nil {
			t.Fatal(err)
		}
		if err := waitError(t, done); !errors.Is(err, command.ErrConflict) {
			t.Fatalf("start after expired-during-wait: %v", err)
		}
		if after := sstFacts(t, p); after != before {
			t.Fatalf("expired-during-wait left facts %v -> %v", before, after)
		}
	})

	t.Run("rotation_fences_new_starts_until_requalified", func(t *testing.T) {
		p := psSetup(t)
		e := sstNewEnv(t, p.f, sstKeyring(t, "sst_api", randomBytes(32)))
		s := e.seed(t, p)
		secondKey := "sk_test_" + hex.EncodeToString(randomBytes(12))
		if err := e.fake.AddAccount(s.account, secondKey); err != nil {
			t.Fatal(err)
		}
		if head, err := e.reg.Rotate(context.Background(), s.scope, s.connection, 1, s.account, secondKey); err != nil || head != 2 {
			t.Fatalf("rotate: %d %v", head, err)
		}
		s.secret = secondKey
		before := sstFacts(t, p)
		if _, err := s.begin(e.svc, t04Key("sst-rotated"), s.input("zh-TW")); !errors.Is(err, command.ErrConflict) {
			t.Fatalf("start under an unqualified rotated head was not 409: %v", err)
		}
		if sstFacts(t, p) != before {
			t.Fatal("fenced start left facts")
		}
		// A qualification captured under the OLD head cannot qualify the new one (registrar CAS)...
		if _, err := e.reg.Qualify(context.Background(), s.scope, stripeadmin.QualifyInput{ConnectionID: s.connection, AccountID: s.account,
			SecretKey: s.secret, Profile: "PROVIDER_MOCK", Currency: "TWD", ReturnURL: sstReturnURL, ExpectedVersion: 1, AmountMinor: 2500}); !errors.Is(err, stripeadmin.ErrRejected) {
			t.Fatalf("stale expected version qualified the rotated head: %v", err)
		}
		// ...requalify at version 2, re-point the method (CAS 1 -> 2), and new starts work again.
		s.requalify(t, e, 2)
		v, err := e.reg.SetMethod(context.Background(), s.scope, s.methodInput(1, true, true, 2500, 99999900))
		if err != nil || v != 2 {
			t.Fatalf("re-point method: %d %v", v, err)
		}
		s.method = v
		res, err := s.begin(e.svc, t04Key("sst-requalified"), s.input("zh-TW"))
		if err != nil {
			t.Fatalf("start after requalification: %v", err)
		}
		var version int64
		if err := p.f.owner.QueryRow(context.Background(), `SELECT credential_version FROM checkout.payment_attempts WHERE id=$1`, res.AttemptID).Scan(&version); err != nil || version != 2 {
			t.Fatalf("new attempt froze credential version %d: %v", version, err)
		}
	})

	t.Run("constructor_negatives", func(t *testing.T) {
		p := psSetup(t)
		e := sstNewEnv(t, p.f, sstKeyring(t, "sst_api", randomBytes(32)))
		ctx := context.Background()
		good := checkout.HostedProviders{PAYUNi: &e.pcfg, Stripe: &e.scfg}
		for _, v := range []struct {
			name    string
			ctx     context.Context
			jobs    *river.Client[pgx.Tx]
			profile string
			keys    *accounts.Keyring
			p       checkout.HostedProviders
		}{
			{"nil_ctx", nil, e.jobs, "PROVIDER_MOCK", e.keys, good},
			{"nil_jobs", ctx, nil, "PROVIDER_MOCK", e.keys, good},
			{"no_provider", ctx, e.jobs, "PROVIDER_MOCK", e.keys, checkout.HostedProviders{}},
			{"payuni_without_keys", ctx, e.jobs, "PROVIDER_MOCK", nil, good},
			// "stripe_on_live_profile" removed: the Stripe branch admits LIVE since 0077 (stripe-live-enable-v1 §5.2); see hosted_live_test.go.
			{"stripe_on_unknown_profile", ctx, e.jobs, "PRODUCTION", e.keys, good},
			{"stripe_http_return_url", ctx, e.jobs, "PROVIDER_MOCK", e.keys, checkout.HostedProviders{Stripe: &checkout.StripeHostedConfig{ReturnURL: "http://checkout.example.test/return"}}},
			{"stripe_query_in_return_url", ctx, e.jobs, "PROVIDER_MOCK", e.keys, checkout.HostedProviders{Stripe: &checkout.StripeHostedConfig{ReturnURL: sstReturnURL + "?x=1"}}},
		} {
			if s, err := checkout.NewHostedPaymentService(v.ctx, e.hosted, v.jobs, v.profile, v.keys, v.p); !errors.Is(err, command.ErrInvalid) || s != nil {
				t.Fatalf("%s: err=%v", v.name, err)
			}
		}
		// Stripe-only is a valid constructor input (keys are only needed for PAYUNi).
		if s, err := checkout.NewHostedPaymentService(ctx, e.hosted, e.jobs, "PROVIDER_MOCK", nil, checkout.HostedProviders{Stripe: &e.scfg}); err != nil || s == nil {
			t.Fatalf("stripe-only service: %v", err)
		}
		// The old constructor keeps its signature and stays PAYUNi-only: Refresh/Cancel are NotFound.
		old, err := checkout.NewHostedPaymentStarter(ctx, e.hosted, e.jobs, "PROVIDER_MOCK", e.keys, e.pcfg)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := old.RefreshPayment(ctx, p.cap.Token, p.f.storeA1, p.hold.OrderID); !errors.Is(err, command.ErrNotFound) {
			t.Fatalf("refresh on a PAYUNi-only service: %v", err)
		}
		if _, err := old.CancelPayment(ctx, p.cap.Token, p.f.storeA1, p.hold.OrderID); !errors.Is(err, command.ErrNotFound) {
			t.Fatalf("cancel on a PAYUNi-only service: %v", err)
		}
		// digest golden shape: version, return_url, api_version, ttl 2400, send 420, margin 300.
		canonical, digest, err := e.scfg.CanonicalDigest()
		if err != nil || canonical.ReturnURL != sstReturnURL || digest == ([32]byte{}) {
			t.Fatalf("canonical digest: %v", err)
		}
		_, digest2, _ := checkout.StripeHostedConfig{ReturnURL: sstReturnURL + "/x"}.CanonicalDigest()
		if digest == digest2 {
			t.Fatal("digest ignores the return URL")
		}
	})
}

// sstPin plays the worker's pin step with an owner fixture (no worker runs in the
// shared fixture database): provider_reference plus the set-once session pins,
// exactly what integration.record_stripe_observation writes for via=create.
func sstPin(t *testing.T, f *testFixture, attempt string) (sessionID, sessionURL string) {
	t.Helper()
	sessionID = "cs_test_" + hex.EncodeToString(randomBytes(12))
	sessionURL = "https://checkout.stripe.com/c/pay/" + sessionID
	mustExec(t, f.owner, `UPDATE integration.operations SET provider_reference=$2 WHERE id=$1`, attempt, sessionID)
	mustExec(t, f.owner, `UPDATE payments.stripe_sessions SET create_first_sent_at=clock_timestamp(),
	 create_last_sent_at=clock_timestamp(),create_send_count=1,create_body_sha256=$2,session_id=$3,session_url=$4,
	 pinned_at=clock_timestamp() WHERE attempt_id=$1`, attempt, randomBytes(32), sessionID, sessionURL)
	return
}

// sstAge shifts every stored timestamp of one Stripe session back by d, as the
// migration owner in session_replication_role=replica. Disclosed evidence: this
// bypasses payments.guard_stripe_session (set-once/frozen columns) on purpose;
// all CHECK-linked deadlines (expires_at = created + 40 min, send_deadline,
// handoff_cutoff) and the frozen create_params expires_at move together, because the
// worker refuses a snapshot whose body disagrees with its deadlines. Nothing in
// checkout.payment_attempts or integration.operations is touched.
func sstAge(t *testing.T, f *testFixture, attempt string, d time.Duration) {
	t.Helper()
	ctx := context.Background()
	tx, err := f.owner.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `SET LOCAL session_replication_role = replica`); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `UPDATE payments.stripe_sessions SET
	 attempt_created_at=attempt_created_at-$2::interval, expires_at=expires_at-$2::interval,
	 send_deadline=send_deadline-$2::interval, handoff_cutoff=handoff_cutoff-$2::interval,
	 create_first_sent_at=create_first_sent_at-$2::interval, create_last_sent_at=create_last_sent_at-$2::interval,
	 pinned_at=pinned_at-$2::interval, first_handed_out_at=first_handed_out_at-$2::interval,
	 cancel_requested_at=cancel_requested_at-$2::interval, last_refresh_at=last_refresh_at-$2::interval,
	 last_expire_at=last_expire_at-$2::interval, url_purged_at=url_purged_at-$2::interval,
	 create_params=jsonb_set(create_params,'{expires_at}',to_jsonb(((create_params->>'expires_at')::bigint-$3::bigint)::text))
	 WHERE attempt_id=$1`, attempt, fmt.Sprintf("%d seconds", int64(d.Seconds())), int64(d.Seconds())); err != nil {
		t.Fatalf("age session: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
}

// sstWake makes an attempt's sleeping River jobs due now. River schedules snoozes
// minutes ahead; tests move the schedule, never the code under test.
func sstWake(t *testing.T, f *testFixture, attempt string) {
	t.Helper()
	mustExec(t, f.owner, `UPDATE river_payment.river_job SET state='available',scheduled_at=clock_timestamp()
	 WHERE kind IN ('payment_query_v1','payment_signal_v1','payment_reconcile_v1')
	 AND args->>'operation_id'=$1 AND state IN ('scheduled','retryable')`, attempt)
}
