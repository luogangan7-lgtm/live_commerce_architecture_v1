package foundation_test

// SP21 (contracts/stripe-psp-v1.md §13, §0.2, §14): the operator registrar through
// stripeadmin.Open with a real registrar login. The registrar is the only writer
// of Stripe accounts, webhook endpoints, qualifications and methods.
//
// Cited, not repeated (existing SQL-level gates): TestStripeSP06RegistrarIsolation
// (merchant runtime cannot create/rebind Stripe accounts or methods, stale CAS),
// TestStripeSP06EndpointCustodyAndRLS, TestStripeSP06Schema (REAL_LIVE CHECK) and
// TestStripeSP21PinnedHandoffAfterKeyRotation (pinned handoff after rotation).
//
// The SANDBOX probe against the real api.stripe.com runs only with STRIPE_SANDBOX=1
// (test key + STRIPE_ACCOUNT_ID); otherwise it is reported NOT_RUN via t.Skip, never
// PASS. The MOCK tier below exercises the same code path against the fake.

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"regexp"
	"testing"
	"time"

	"livecommerce/internal/integrations/accounts"
	"livecommerce/internal/integrations/psp/stripe/stripetest"
	"livecommerce/internal/payments/stripeadmin"
	"livecommerce/internal/platform"
)

func srgRejected(t *testing.T, what string, err error) {
	t.Helper()
	if !errors.Is(err, stripeadmin.ErrRejected) {
		t.Fatalf("%s: want ErrRejected, got %v", what, err)
	}
}

func srgRows(t *testing.T, f *testFixture, tenant string) [4]int {
	t.Helper()
	var out [4]int
	if err := f.owner.QueryRow(context.Background(), `SELECT
	 (SELECT count(*) FROM integration.merchant_accounts WHERE tenant_id=$1 AND provider='stripe'),
	 (SELECT count(*) FROM integration.account_credentials WHERE tenant_id=$1 AND connection_id IN (SELECT id FROM integration.merchant_accounts WHERE provider='stripe')),
	 (SELECT count(*) FROM integration.bindings WHERE tenant_id=$1 AND provider='stripe'),
	 (SELECT count(*) FROM payments.stripe_webhook_endpoints WHERE tenant_id=$1)`, tenant).Scan(&out[0], &out[1], &out[2], &out[3]); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestStripeSP21Registrar(t *testing.T) {
	p1 := psSetup(t)
	p2 := psSetup(t)
	e := sstNewEnv(t, p1.f, sstKeyring(t, "sst_api", randomBytes(32)))
	ctx := context.Background()
	// A second store in tenant 1 (same tenant, different store) for the same-tenant rules.
	storeB := randomUUID()
	mustExec(t, p1.f.owner, `INSERT INTO control.stores(tenant_id,id,name,currency) VALUES($1,$2,'SP21 second store','TWD')`, p1.f.tenantA, storeB)
	mustExec(t, p1.f.owner, `INSERT INTO identity.store_grants(tenant_id,store_id,principal_id,permission)
	 SELECT $1,$2,$3,p FROM unnest(ARRAY['store:read','integration:manage','integration:read']) p`, p1.f.tenantA, storeB, p1.f.principalA)
	scope1 := stripeadmin.Scope{TenantID: p1.f.tenantA, StoreID: p1.f.storeA1, PrincipalID: p1.f.principalA}
	scopeB := stripeadmin.Scope{TenantID: p1.f.tenantA, StoreID: storeB, PrincipalID: p1.f.principalA}
	scope2 := stripeadmin.Scope{TenantID: p2.f.tenantA, StoreID: p2.f.storeA1, PrincipalID: p2.f.principalA}
	key := func() string { return "sk_test_" + t04Tag() + t04Tag() }
	register := func(scope stripeadmin.Scope, account, secret string) (string, error) {
		if err := e.fake.AddAccount(account, secret); err != nil {
			t.Fatal(err)
		}
		return e.reg.Register(ctx, scope, account, secret)
	}

	var s1 sstStore
	t.Run("two_stores_two_accounts_register_and_first_rotation_is_exact_version", func(t *testing.T) {
		s1 = e.seed(t, p1)
		if _, err := register(scope2, "acct_T"+t04Tag(), key()); err != nil {
			t.Fatalf("second tenant's own account: %v", err)
		}
		if got := srgRows(t, p1.f, p1.f.tenantA); got != [4]int{1, 1, 1, 0} {
			t.Fatalf("tenant 1 rows %v", got)
		}
		// Rotation appends exactly expected+1 and never rewrites a historical version.
		var nonce, ciphertext []byte
		if err := p1.f.owner.QueryRow(ctx, `SELECT nonce,ciphertext FROM integration.account_credentials WHERE connection_id=$1 AND version=1`, s1.connection).Scan(&nonce, &ciphertext); err != nil {
			t.Fatal(err)
		}
		next := key()
		if err := e.fake.AddAccount(s1.account, next); err != nil {
			t.Fatal(err)
		}
		srgRejected(t, "stale rotation expected version", func() error { _, err := e.reg.Rotate(ctx, scope1, s1.connection, 7, s1.account, next); return err }())
		if head, err := e.reg.Rotate(ctx, scope1, s1.connection, 1, s1.account, next); err != nil || head != 2 {
			t.Fatalf("rotate: %d %v", head, err)
		}
		var n2, c2 []byte
		if err := p1.f.owner.QueryRow(ctx, `SELECT nonce,ciphertext FROM integration.account_credentials WHERE connection_id=$1 AND version=1`, s1.connection).Scan(&n2, &c2); err != nil || string(n2) != string(nonce) || string(c2) != string(ciphertext) {
			t.Fatalf("rotation rewrote historical credential version 1 (%v)", err)
		}
		if n := countRows(t, p1.f.owner, `SELECT count(*) FROM integration.account_credentials WHERE connection_id=$1`, s1.connection); n != 2 {
			t.Fatalf("credential versions=%d", n)
		}
		s1.secret = next
		s1.requalify(t, e, 2) // new starts need a qualification of the new head
	})

	t.Run("second_account_per_store_same_account_across_stores_and_cross_store_rebinding_are_rejected", func(t *testing.T) {
		before := srgRows(t, p1.f, p1.f.tenantA)
		_, err := register(scope1, "acct_T"+t04Tag(), key())
		srgRejected(t, "second account for the same store", err)
		_, err = register(scopeB, s1.account, key())
		srgRejected(t, "same account on another store of the tenant", err)
		_, err = register(scope2, s1.account, key())
		srgRejected(t, "same account on another tenant's store", err)
		if got := srgRows(t, p1.f, p1.f.tenantA); got != before {
			t.Fatalf("rejected registrations left rows: %v -> %v", before, got)
		}
		// Cross-store rebinding through every other writer: a connection is only usable in its own scope.
		secrets := accounts.StripeWebhookSecrets{CurrentSecret: swhSecret()}
		_, _, err = e.reg.SetWebhookEndpoint(ctx, scopeB, stripeadmin.EndpointInput{ConnectionID: s1.connection, AccountID: s1.account, Profile: "PROVIDER_MOCK", Enabled: true, Secrets: secrets})
		srgRejected(t, "endpoint bound to another store's connection", err)
		_, _, err = e.reg.SetWebhookEndpoint(ctx, scope2, stripeadmin.EndpointInput{ConnectionID: s1.connection, AccountID: s1.account, Profile: "PROVIDER_MOCK", Enabled: true, Secrets: secrets})
		srgRejected(t, "endpoint bound to another tenant's connection", err)
		_, err = e.reg.Qualify(ctx, scope2, stripeadmin.QualifyInput{ConnectionID: s1.connection, AccountID: s1.account, SecretKey: s1.secret, Profile: "PROVIDER_MOCK",
			Currency: "TWD", ReturnURL: sstReturnURL, ExpectedVersion: 2, AmountMinor: 2500})
		srgRejected(t, "qualification of another tenant's connection", err)
		in := s1.methodInput(1, true, true, 2500, 99999900)
		in.MarketID = p2.market.ID
		_, err = e.reg.SetMethod(ctx, scope2, in)
		srgRejected(t, "method pointing at another tenant's connection", err)
		if _, err := e.reg.Rotate(ctx, scope2, s1.connection, 2, s1.account, s1.secret); !errors.Is(err, stripeadmin.ErrRejected) {
			t.Fatalf("rotation of another tenant's connection: %v", err)
		}
		if got := srgRows(t, p1.f, p1.f.tenantA); got != before {
			t.Fatalf("cross-store attempts left rows: %v -> %v", before, got)
		}
	})

	t.Run("non_owner_principal_is_rejected_everywhere", func(t *testing.T) {
		stranger := randomUUID()
		mustExec(t, p1.f.owner, `INSERT INTO identity.principals(id) VALUES($1)`, stranger)
		foreign := stripeadmin.Scope{TenantID: p1.f.tenantA, StoreID: storeB, PrincipalID: stranger}            // known principal, no membership
		unknown := stripeadmin.Scope{TenantID: p1.f.tenantA, StoreID: storeB, PrincipalID: randomUUID()}        // no such principal
		otherTenant := stripeadmin.Scope{TenantID: p1.f.tenantA, StoreID: storeB, PrincipalID: p2.f.principalA} // owner of another tenant
		before := srgRows(t, p1.f, p1.f.tenantA)
		for name, scope := range map[string]stripeadmin.Scope{"no_membership": foreign, "unknown_principal": unknown, "owner_of_another_tenant": otherTenant} {
			_, err := register(scope, "acct_T"+t04Tag(), key())
			srgRejected(t, name+" register", err)
			if _, _, err := e.reg.SetWebhookEndpoint(ctx, scope, stripeadmin.EndpointInput{ConnectionID: s1.connection, AccountID: s1.account, Profile: "PROVIDER_MOCK", Enabled: true,
				Secrets: accounts.StripeWebhookSecrets{CurrentSecret: swhSecret()}}); !errors.Is(err, stripeadmin.ErrRejected) {
				t.Fatalf("%s endpoint: %v", name, err)
			}
			if _, err := e.reg.Rotate(ctx, scope, s1.connection, 2, s1.account, s1.secret); !errors.Is(err, stripeadmin.ErrRejected) {
				t.Fatalf("%s rotate: %v", name, err)
			}
		}
		if got := srgRows(t, p1.f, p1.f.tenantA); got != before {
			t.Fatalf("non-owner attempts left rows: %v -> %v", before, got)
		}
	})

	t.Run("register_verifies_the_account_before_any_write", func(t *testing.T) {
		// Ruling 8: the operator-supplied account id must equal GET /v1/account for that key.
		before := srgRows(t, p1.f, p1.f.tenantA)
		secret := key()
		if err := e.fake.AddAccount("acct_ActuallyThisOne1", secret); err != nil {
			t.Fatal(err)
		}
		_, err := e.reg.Register(ctx, scopeB, "acct_ClaimedButWrong1", secret)
		srgRejected(t, "claimed account differs from the key's account", err)
		// A key Stripe does not know at all is a provider failure or rejection, never a write.
		if _, err := e.reg.Register(ctx, scopeB, "acct_T"+t04Tag(), key()); err == nil {
			t.Fatal("an unauthenticated key registered an account")
		}
		if got := srgRows(t, p1.f, p1.f.tenantA); got != before {
			t.Fatalf("failed verification wrote rows: %v -> %v", before, got)
		}
	})

	t.Run("ciphertext_purpose_and_aad_swaps_fail_to_open", func(t *testing.T) {
		secrets := accounts.StripeWebhookSecrets{CurrentSecret: swhSecret(), NextSecret: swhSecret()}
		endpoint, version, err := e.reg.SetWebhookEndpoint(ctx, scope1, stripeadmin.EndpointInput{ConnectionID: s1.connection, AccountID: s1.account, Profile: "PROVIDER_MOCK", Enabled: true, Secrets: secrets})
		if err != nil || version != 1 {
			t.Fatalf("endpoint with rotation pair: %d %v", version, err)
		}
		type envelope struct {
			keyID             string
			nonce, ciphertext []byte
		}
		var api, hook envelope
		var apiVersion int64
		if err := p1.f.owner.QueryRow(ctx, `SELECT c.key_id,c.nonce,c.ciphertext,c.version FROM integration.account_credentials c WHERE c.connection_id=$1 ORDER BY c.version DESC LIMIT 1`, s1.connection).
			Scan(&api.keyID, &api.nonce, &api.ciphertext, &apiVersion); err != nil {
			t.Fatal(err)
		}
		if err := p1.f.owner.QueryRow(ctx, `SELECT key_id,nonce,ciphertext FROM payments.stripe_webhook_endpoints WHERE endpoint_id=$1::uuid`, endpoint).Scan(&hook.keyID, &hook.nonce, &hook.ciphertext); err != nil {
			t.Fatal(err)
		}
		apiScope := accounts.StripeAPIScope{TenantID: scope1.TenantID, StoreID: scope1.StoreID, ConnectionID: s1.connection, Environment: "SANDBOX", AccountID: s1.account, CredentialVersion: apiVersion}
		hookScope := accounts.StripeWebhookScope{TenantID: scope1.TenantID, StoreID: scope1.StoreID, ConnectionID: s1.connection, EndpointID: endpoint, Environment: "SANDBOX", AccountID: s1.account, Profile: "PROVIDER_MOCK", KeyVersion: 1}
		// Positive controls: the exact scope opens each envelope to exactly what the operator supplied.
		if got, err := e.keys.OpenStripeAPI(apiScope, api.keyID, api.nonce, api.ciphertext); err != nil || got.SecretKey != s1.secret {
			t.Fatalf("API envelope does not open with its own scope: %v", err)
		}
		if got, err := e.signing.OpenStripeWebhook(hookScope, hook.keyID, hook.nonce, hook.ciphertext); err != nil || got.CurrentSecret != secrets.CurrentSecret || got.NextSecret != secrets.NextSecret {
			t.Fatalf("webhook envelope does not open with its own scope: %v", err)
		}
		fail := func(name string, _ any, err error) {
			t.Helper()
			if err == nil {
				t.Fatalf("%s: opened", name)
			}
		}
		// purpose swaps (same keyring material, other purpose) and custody swaps (other keyring).
		_, err = e.keys.OpenStripeAPI(accounts.StripeAPIScope{TenantID: hookScope.TenantID, StoreID: hookScope.StoreID, ConnectionID: hookScope.ConnectionID, Environment: "SANDBOX", AccountID: hookScope.AccountID, CredentialVersion: 1}, hook.keyID, hook.nonce, hook.ciphertext)
		fail("webhook ciphertext as API credential", nil, err)
		_, err = e.signing.OpenStripeWebhook(hookScope, api.keyID, api.nonce, api.ciphertext)
		fail("API ciphertext as webhook secrets", nil, err)
		_, err = e.signing.OpenStripeAPI(apiScope, api.keyID, api.nonce, api.ciphertext)
		fail("API ciphertext under the signing keyring", nil, err)
		_, err = e.keys.OpenStripeWebhook(hookScope, hook.keyID, hook.nonce, hook.ciphertext)
		fail("webhook ciphertext under the API keyring", nil, err)
		// AAD swaps: every bound field of both envelopes.
		for name, mutate := range map[string]func(*accounts.StripeAPIScope){
			"api_version":    func(s *accounts.StripeAPIScope) { s.CredentialVersion++ },
			"api_connection": func(s *accounts.StripeAPIScope) { s.ConnectionID = randomUUID() },
			"api_tenant":     func(s *accounts.StripeAPIScope) { s.TenantID = p2.f.tenantA },
			"api_store":      func(s *accounts.StripeAPIScope) { s.StoreID = storeB },
			"api_account":    func(s *accounts.StripeAPIScope) { s.AccountID = "acct_SwappedAccount1" },
		} {
			c := apiScope
			mutate(&c)
			_, err := e.keys.OpenStripeAPI(c, api.keyID, api.nonce, api.ciphertext)
			fail(name, nil, err)
		}
		for name, mutate := range map[string]func(*accounts.StripeWebhookScope){
			"hook_endpoint":   func(s *accounts.StripeWebhookScope) { s.EndpointID = randomUUID() },
			"hook_version":    func(s *accounts.StripeWebhookScope) { s.KeyVersion++ },
			"hook_profile":    func(s *accounts.StripeWebhookScope) { s.Profile = "SANDBOX" },
			"hook_connection": func(s *accounts.StripeWebhookScope) { s.ConnectionID = randomUUID() },
			"hook_account":    func(s *accounts.StripeWebhookScope) { s.AccountID = "acct_SwappedAccount1" },
			"hook_store":      func(s *accounts.StripeWebhookScope) { s.StoreID = storeB },
		} {
			c := hookScope
			mutate(&c)
			_, err := e.signing.OpenStripeWebhook(c, hook.keyID, hook.nonce, hook.ciphertext)
			fail(name, nil, err)
		}
		// Endpoint rules: exact CAS, distinct current/next, LIVE profile refused.
		_, _, err = e.reg.SetWebhookEndpoint(ctx, scope1, stripeadmin.EndpointInput{ConnectionID: s1.connection, EndpointID: endpoint, AccountID: s1.account, Profile: "PROVIDER_MOCK",
			ExpectedVersion: 9, Enabled: true, Secrets: secrets})
		srgRejected(t, "stale endpoint version", err)
		if _, _, err := e.reg.SetWebhookEndpoint(ctx, scope1, stripeadmin.EndpointInput{ConnectionID: s1.connection, EndpointID: endpoint, AccountID: s1.account, Profile: "PROVIDER_MOCK",
			ExpectedVersion: 1, Enabled: true, Secrets: accounts.StripeWebhookSecrets{CurrentSecret: secrets.CurrentSecret, NextSecret: secrets.CurrentSecret}}); err == nil {
			t.Fatal("identical current/next signing secrets accepted")
		}
	})

	t.Run("rotation_blocks_new_starts_until_requalified_and_historical_attempts_keep_their_version", func(t *testing.T) {
		p := psSetup(t)
		st := e.seed(t, p)
		res, err := st.begin(e.svc, t04Key("srg-hist"), st.input("zh-TW"))
		if err != nil {
			t.Fatal(err)
		}
		next := key()
		if err := e.fake.AddAccount(st.account, next); err != nil {
			t.Fatal(err)
		}
		if head, err := e.reg.Rotate(ctx, st.scope, st.connection, 1, st.account, next); err != nil || head != 2 {
			t.Fatalf("rotate: %d %v", head, err)
		}
		var attemptVersion, head int64
		if err := p.f.owner.QueryRow(ctx, `SELECT a.credential_version,m.credential_version FROM checkout.payment_attempts a JOIN integration.merchant_accounts m ON m.id=a.connection_id WHERE a.id=$1`, res.AttemptID).Scan(&attemptVersion, &head); err != nil || attemptVersion != 1 || head != 2 {
			t.Fatalf("historical attempt pin=%d head=%d (%v)", attemptVersion, head, err)
		}
		// The old qualification is bound to version 1; a stale expected version cannot requalify version 2.
		st.secret = next
		_, err = e.reg.Qualify(ctx, st.scope, stripeadmin.QualifyInput{ConnectionID: st.connection, AccountID: st.account, SecretKey: next, Profile: "PROVIDER_MOCK",
			Currency: "TWD", ReturnURL: sstReturnURL, ExpectedVersion: 1, AmountMinor: 2500})
		srgRejected(t, "qualify with a stale expected version after rotation", err)
		q := sstMoreHold(t, p)
		st.p = q
		if _, err := st.begin(e.svc, t04Key("srg-fenced"), st.input("zh-TW")); err == nil {
			t.Fatal("a new start under an unqualified rotated head succeeded")
		}
		st.requalify(t, e, 2)
		v, err := e.reg.SetMethod(ctx, st.scope, st.methodInput(1, true, true, 2500, 99999900))
		if err != nil {
			t.Fatal(err)
		}
		st.method = v
		if _, err := st.begin(e.svc, t04Key("srg-requalified"), st.input("zh-TW")); err != nil {
			t.Fatalf("start after requalification: %v", err)
		}
	})

	t.Run("method_bounds_currency_and_cas", func(t *testing.T) {
		for name, in := range map[string]stripeadmin.MethodInput{
			"min_below_currency_floor": s1.methodInput(1, true, true, 1, 99999900),
			// TWD min 2500: Stripe SANDBOX rejected 100/1200, accepted 2500 (2026-09-29)
			"twd_min_below_2500":        s1.methodInput(1, true, true, 2400, 99999900),
			"twd_old_min_100":           s1.methodInput(1, true, true, 100, 99999900),
			"max_above_currency_cap":    s1.methodInput(1, true, true, 2500, 100000000),
			"twd_step_not_whole_dollar": s1.methodInput(1, true, true, 2501, 99999900),
			"min_above_max":             s1.methodInput(1, true, true, 5000, 100),
		} {
			if _, err := e.reg.SetMethod(ctx, scope1, in); !errors.Is(err, stripeadmin.ErrRejected) {
				t.Fatalf("%s: %v", name, err)
			}
		}
		if _, err := e.reg.SetMethod(ctx, scope1, s1.methodInput(42, true, true, 2500, 99999900)); !errors.Is(err, stripeadmin.ErrRejected) {
			t.Fatalf("stale method version accepted: %v", err)
		}
	})

	t.Run("qualification_evidence_formats_mock_and_probe", func(t *testing.T) {
		// PROVIDER_MOCK: no network, evidence provider-mock:<qualification>.
		requestsBefore := len(e.fake.Requests())
		mockID, err := e.reg.Qualify(ctx, scope1, stripeadmin.QualifyInput{ConnectionID: s1.connection, AccountID: s1.account, SecretKey: s1.secret, Profile: "PROVIDER_MOCK",
			Currency: "TWD", ReturnURL: sstReturnURL, ExpectedVersion: 2, AmountMinor: 2500})
		if err != nil {
			t.Fatal(err)
		}
		var evidence, class string
		var observed, expires time.Time
		read := func(id string) {
			if err := p1.f.owner.QueryRow(ctx, `SELECT evidence_ref,proof_class,observed_at,expires_at FROM payments.account_qualifications WHERE id=$1`, id).Scan(&evidence, &class, &observed, &expires); err != nil {
				t.Fatal(err)
			}
		}
		read(mockID)
		if evidence != "provider-mock:"+mockID || class != "PROVIDER_MOCK" || len(e.fake.Requests()) != requestsBefore {
			t.Fatalf("mock qualification evidence=%q class=%s requests+=%d", evidence, class, len(e.fake.Requests())-requestsBefore)
		}
		// SANDBOX through the fake transport: the probe creates one lc_probe session at the
		// requested minimum, expires it and requires expired+unpaid+!livemode; evidence is stripe-probe:<id>.
		sbID, err := e.reg.Qualify(ctx, scope1, stripeadmin.QualifyInput{ConnectionID: s1.connection, AccountID: s1.account, SecretKey: s1.secret, Profile: "SANDBOX",
			Currency: "TWD", ReturnURL: sstReturnURL, ExpectedVersion: 2, AmountMinor: 2500})
		if err != nil {
			t.Fatalf("SANDBOX probe against the fake: %v", err)
		}
		read(sbID)
		m := regexp.MustCompile(`^stripe-probe:(cs_test_[A-Za-z0-9_]+)$`).FindStringSubmatch(evidence)
		if m == nil || class != "REAL_SANDBOX" || !expires.After(observed) || expires.Sub(observed) != 30*24*time.Hour || observed.After(time.Now()) {
			t.Fatalf("probe qualification: evidence=%q class=%s observed=%s expires=%s", evidence, class, observed, expires)
		}
		var status, currency string
		var amount int64
		var meta map[string]string
		sflFakeSession(t, e.fake, s1.secret, m[1], &status, &currency, &amount, &meta)
		if status != "expired" || currency != "twd" || amount != 2500 || meta["lc_probe"] == "" {
			t.Fatalf("probe session %s status=%s currency=%s amount=%d probe=%q", m[1], status, currency, amount, meta["lc_probe"])
		}
		// Probe and rotation: a probe started under the old head cannot qualify the new one.
		if _, err := e.reg.Qualify(ctx, scope1, stripeadmin.QualifyInput{ConnectionID: s1.connection, AccountID: s1.account, SecretKey: s1.secret, Profile: "SANDBOX",
			Currency: "TWD", ReturnURL: sstReturnURL, ExpectedVersion: 1, AmountMinor: 2500}); !errors.Is(err, stripeadmin.ErrRejected) {
			t.Fatalf("stale-version probe qualified: %v", err)
		}
	})

	t.Run("live_is_refused_by_the_registrar_and_by_sql", func(t *testing.T) {
		fakeCalls := len(e.fake.Requests())
		before := srgRows(t, p1.f, p1.f.tenantA)
		liveKey := "sk_live_" + t04Tag() + t04Tag()
		if _, err := e.reg.Register(ctx, scopeB, "acct_T"+t04Tag(), liveKey); err == nil {
			t.Fatal("a live key registered")
		}
		if _, err := e.reg.Qualify(ctx, scope1, stripeadmin.QualifyInput{ConnectionID: s1.connection, AccountID: s1.account, SecretKey: s1.secret, Profile: "LIVE",
			Currency: "TWD", ReturnURL: sstReturnURL, ExpectedVersion: 2, AmountMinor: 2500}); err == nil {
			t.Fatal("LIVE qualification accepted")
		}
		if _, _, err := e.reg.SetWebhookEndpoint(ctx, scope1, stripeadmin.EndpointInput{ConnectionID: s1.connection, AccountID: s1.account, Profile: "LIVE", Enabled: true,
			Secrets: accounts.StripeWebhookSecrets{CurrentSecret: swhSecret()}}); err == nil {
			t.Fatal("LIVE webhook endpoint accepted")
		}
		if len(e.fake.Requests()) != fakeCalls {
			t.Fatal("a LIVE request reached the network layer")
		}
		if got := srgRows(t, p1.f, p1.f.tenantA); got != before {
			t.Fatalf("LIVE attempts left rows: %v -> %v", before, got)
		}
		// SQL layer, independent of the Go registrar: the registrar login calls the definer directly.
		dsn := sstLogin(t, p1.f, "commerce_payment_registrar")
		pool, err := platform.OpenStripeRegistrarPool(ctx, dsn)
		if err != nil {
			t.Fatalf("registrar pool: %v", err)
		}
		defer pool.Close()
		observedAt := time.Now().UTC().Add(-time.Minute)
		var id string
		err = pool.QueryRow(ctx, `SELECT payments.qualify_stripe_method($1::uuid,$2::uuid,$3::uuid,$4::uuid,$5::uuid,2,'LIVE','live evidence',$6,$7)`,
			scope1.TenantID, scope1.StoreID, scope1.PrincipalID, randomUUID(), s1.connection, observedAt, observedAt.Add(time.Hour)).Scan(&id)
		if err == nil {
			t.Fatal("SQL accepted a LIVE qualification")
		}
		if n := countRows(t, p1.f.owner, `SELECT count(*) FROM payments.account_qualifications WHERE tenant_id=$1 AND environment='LIVE'`, scope1.TenantID); n != 0 {
			t.Fatalf("LIVE qualification rows: %d", n)
		}
	})

	t.Run("sandbox_probe_against_real_stripe", func(t *testing.T) {
		if os.Getenv("STRIPE_SANDBOX") != "1" || os.Getenv("STRIPE_SECRET_KEY") == "" || os.Getenv("STRIPE_ACCOUNT_ID") == "" {
			t.Skip("NOT_RUN: set STRIPE_SANDBOX=1 with a test STRIPE_SECRET_KEY and STRIPE_ACCOUNT_ID to probe the real sandbox (creates and expires one test session, no charge)")
		}
		p := psSetup(t)
		real, err := stripeadmin.Open(ctx, sstLogin(t, p.f, "commerce_payment_registrar"), e.keys, e.signing) // no mock transport: api.stripe.com
		if err != nil {
			t.Fatal(err)
		}
		defer real.Close()
		scope := stripeadmin.Scope{TenantID: p.f.tenantA, StoreID: p.f.storeA1, PrincipalID: p.f.principalA}
		account, secret := os.Getenv("STRIPE_ACCOUNT_ID"), os.Getenv("STRIPE_SECRET_KEY")
		// Same default as SP16 (psp/stripe sandbox_test.go): ProbeCheckout requires an https return URL,
		// and no harness sets COMMERCE_PAYMENT_RETURN_URL, so an empty value was refused locally (ErrRejected).
		returnURL := os.Getenv("COMMERCE_PAYMENT_RETURN_URL")
		if returnURL == "" {
			returnURL = "https://example.com/livecommerce/payment/return"
		}
		conn, err := real.Register(ctx, scope, account, secret)
		if err != nil {
			t.Fatalf("register sandbox account: %v", err)
		}
		id, err := real.Qualify(ctx, scope, stripeadmin.QualifyInput{ConnectionID: conn, AccountID: account, SecretKey: secret, Profile: "SANDBOX",
			Currency: "HKD", ReturnURL: returnURL, ExpectedVersion: 1, AmountMinor: 400})
		if err != nil {
			t.Fatalf("sandbox probe: %v", err)
		}
		var evidence string
		if err := p.f.owner.QueryRow(ctx, `SELECT evidence_ref FROM payments.account_qualifications WHERE id=$1`, id).Scan(&evidence); err != nil || !regexp.MustCompile(`^stripe-probe:cs_test_[A-Za-z0-9_]+$`).MatchString(evidence) {
			t.Fatalf("evidence format: %v", err)
		}
	})
}

// sflFakeSession reads one session from the fake through the API key of its account.
func sflFakeSession(t *testing.T, f *stripetest.Server, key, id string, status, currency *string, amount *int64, meta *map[string]string) {
	t.Helper()
	req, err := srgNewRequest(f.URL()+"/v1/checkout/sessions/"+id, key)
	if err != nil {
		t.Fatal(err)
	}
	var out struct {
		Status      string            `json:"status"`
		Currency    string            `json:"currency"`
		AmountTotal int64             `json:"amount_total"`
		Metadata    map[string]string `json:"metadata"`
	}
	if err := srgDoJSON(req, &out); err != nil {
		t.Fatal(err)
	}
	*status, *currency, *amount, *meta = out.Status, out.Currency, out.AmountTotal, out.Metadata
}

func srgNewRequest(url, key string) (*http.Request, error) {
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err == nil {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	return req, err
}

func srgDoJSON(req *http.Request, out any) error {
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	return json.NewDecoder(res.Body).Decode(out)
}
