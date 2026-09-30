// registrar_test.go: MOCK-tier tests of the registrar's Go side (verification-before-write, AAD
// sealing, version arithmetic, error vocabulary) against stripetest and a recording DB fake.
// Non-goal: the SQL definers, roles and RLS (REAL_PG SP21, blocked on pool-fix) and any real Stripe call.
package stripeadmin

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"livecommerce/internal/integrations/accounts"
	"livecommerce/internal/integrations/psp/stripe/stripetest"
)

const (
	tenant    = "11111111-1111-4111-8111-111111111111"
	store     = "22222222-2222-4222-8222-222222222222"
	principal = "33333333-3333-4333-8333-333333333333"
	conn      = "44444444-4444-4444-8444-444444444444"
	acct      = "acct_1RegistrarTest0"
	testKey   = "sk_" + "test_REGISTRARSENTINELKEY00000"
	whsecA    = "whsec_" + "registrarSecretA_0123456789"
	returnURL = "https://shop.example.test/payment/return"
)

type call struct {
	sql  string
	args []any
}

type fakeDB struct {
	calls []call
	reply any   // value Scan writes: string or int64
	err   error // returned by Scan
}

type fakeRow struct{ db *fakeDB }

func (r fakeRow) Scan(dest ...any) error {
	if r.db.err != nil {
		return r.db.err
	}
	reply := r.db.reply
	if f, ok := reply.(func([]any) any); ok {
		reply = f(r.db.calls[len(r.db.calls)-1].args)
	}
	if row, ok := reply.([]any); ok { // multi-column reply: string and []byte columns
		for i, d := range dest {
			switch d := d.(type) {
			case *string:
				*d = row[i].(string)
			case *[]byte:
				*d = row[i].([]byte)
			}
		}
		return nil
	}
	switch d := dest[0].(type) {
	case *string:
		*d = reply.(string)
	case *int64:
		*d = reply.(int64)
	}
	return nil
}

func (f *fakeDB) QueryRow(_ context.Context, sql string, args ...any) pgx.Row {
	f.calls = append(f.calls, call{sql, args})
	return fakeRow{f}
}

func ring(t *testing.T, seed byte) *accounts.Keyring {
	t.Helper()
	k, err := accounts.NewKeyring("k1", map[string][]byte{"k1": bytes.Repeat([]byte{seed}, 32)}, bytes.Repeat([]byte{seed + 50}, 32))
	if err != nil {
		t.Fatal(err)
	}
	return k
}

func fixture(t *testing.T, providerAccount string) (*Registrar, *fakeDB, *accounts.Keyring, *accounts.Keyring, *stripetest.Server) {
	t.Helper()
	srv := stripetest.New(providerAccount)
	t.Cleanup(srv.Close)
	db := &fakeDB{}
	api, sign := ring(t, 1), ring(t, 2)
	return newRegistrar(db, api, sign, srv.Transport()), db, api, sign, srv
}

var scope = Scope{TenantID: tenant, StoreID: store, PrincipalID: principal}

func TestRegisterVerifiesBeforeWriteAndSealsVersionOne(t *testing.T) {
	r, db, api, _, _ := fixture(t, acct)
	db.reply = func(args []any) any { return args[3].(string) } // echo the connection id
	id, err := r.Register(context.Background(), scope, acct, testKey)
	if err != nil || len(db.calls) != 1 {
		t.Fatalf("register: %v calls=%d", err, len(db.calls))
	}
	a := db.calls[0].args
	if a[3] != id || a[5] != "SANDBOX" || a[6] != acct {
		t.Fatalf("unexpected register args")
	}
	got, err := api.OpenStripeAPI(accounts.StripeAPIScope{TenantID: tenant, StoreID: store, ConnectionID: id,
		Environment: "SANDBOX", AccountID: acct, CredentialVersion: 1}, a[7].(string), a[8].([]byte), a[9].([]byte))
	if err != nil || got.SecretKey != testKey {
		t.Fatal("registered ciphertext does not open under version 1 scope")
	}
	if _, err := api.OpenStripeAPI(accounts.StripeAPIScope{TenantID: tenant, StoreID: store, ConnectionID: id,
		Environment: "SANDBOX", AccountID: acct, CredentialVersion: 2}, a[7].(string), a[8].([]byte), a[9].([]byte)); err == nil {
		t.Fatal("AAD does not bind credential version")
	}
	for _, v := range a {
		if b, ok := v.([]byte); ok && bytes.Contains(b, []byte(testKey)) {
			t.Fatal("plaintext key reached SQL args")
		}
	}
}

func TestRegisterRejectsBeforeAnySQL(t *testing.T) {
	r, db, _, _, _ := fixture(t, "acct_1SomebodyElse0") // key belongs to another account
	if _, err := r.Register(context.Background(), scope, acct, testKey); !errors.Is(err, ErrRejected) {
		t.Fatalf("account mismatch: %v", err)
	}
	live := "sk_" + "live_REGISTRARSENTINELKEY00000"
	if _, err := r.Register(context.Background(), scope, acct, live); !errors.Is(err, ErrRejected) {
		t.Fatalf("live key: %v", err)
	}
	if _, err := r.Register(context.Background(), scope, "acct_x y", testKey); !errors.Is(err, ErrConfig) {
		t.Fatalf("bad account id: %v", err)
	}
	if _, err := r.Register(context.Background(), Scope{}, acct, testKey); !errors.Is(err, ErrConfig) {
		t.Fatalf("bad scope: %v", err)
	}
	if len(db.calls) != 0 {
		t.Fatal("SQL ran although verification failed")
	}
}

func TestRotateSealsNextVersionAndChecksReturn(t *testing.T) {
	r, db, api, _, _ := fixture(t, acct)
	rotateReply := func(version int64) func([]any) any { // 4 args = registered-account read (S5)
		return func(args []any) any {
			if len(args) == 4 {
				return acct
			}
			return version
		}
	}
	db.reply = rotateReply(5)
	v, err := r.Rotate(context.Background(), scope, conn, 4, acct, testKey)
	if err != nil || v != 5 {
		t.Fatalf("rotate: %d %v", v, err)
	}
	a := db.calls[len(db.calls)-1].args
	if a[4] != int64(4) {
		t.Fatal("expected version not passed")
	}
	if _, err := api.OpenStripeAPI(accounts.StripeAPIScope{TenantID: tenant, StoreID: store, ConnectionID: conn,
		Environment: "SANDBOX", AccountID: acct, CredentialVersion: 5}, a[5].(string), a[6].([]byte), a[7].([]byte)); err != nil {
		t.Fatal("rotation ciphertext not bound to expected+1")
	}
	db.reply = rotateReply(9)
	if _, err := r.Rotate(context.Background(), scope, conn, 4, acct, testKey); !errors.Is(err, ErrDatabase) {
		t.Fatalf("unexpected version accepted: %v", err)
	}
	if _, err := r.Rotate(context.Background(), scope, conn, 0, acct, testKey); !errors.Is(err, ErrConfig) {
		t.Fatal("expected version 0 accepted")
	}
}

func TestSetWebhookEndpointVersionsAndSeparateCustody(t *testing.T) {
	r, db, api, sign, _ := fixture(t, acct)
	// stripe_endpoint_account (4 args) answers the registered account; the endpoint write answers version.
	endpointReply := func(version int64) func([]any) any {
		return func(args []any) any {
			if len(args) == 4 {
				return acct
			}
			return version
		}
	}
	db.reply = endpointReply(1)
	// AccountID omitted: the account is derived from the registered connection (§0.2).
	in := EndpointInput{ConnectionID: conn, Profile: "SANDBOX", Enabled: true,
		Secrets: accounts.StripeWebhookSecrets{CurrentSecret: whsecA}}
	endpoint, v, err := r.SetWebhookEndpoint(context.Background(), scope, in)
	if err != nil || v != 1 || endpoint == "" || len(db.calls) != 2 || !strings.Contains(db.calls[0].sql, "payments.stripe_endpoint_account(") {
		t.Fatalf("create: %v", err)
	}
	a := db.calls[1].args
	ws := accounts.StripeWebhookScope{TenantID: tenant, StoreID: store, ConnectionID: conn, EndpointID: endpoint,
		Environment: "SANDBOX", AccountID: acct, Profile: "SANDBOX", KeyVersion: 1}
	if s, err := sign.OpenStripeWebhook(ws, a[8].(string), a[9].([]byte), a[10].([]byte)); err != nil || s.CurrentSecret != whsecA {
		t.Fatal("endpoint envelope does not open under the signing keyring")
	}
	if _, err := api.OpenStripeWebhook(ws, a[8].(string), a[9].([]byte), a[10].([]byte)); err == nil {
		t.Fatal("payment API keyring can open webhook signing custody")
	}
	// Update: EndpointID required, version arithmetic, disabled still re-seals.
	db.reply = endpointReply(3)
	in.EndpointID, in.ExpectedVersion, in.Enabled = endpoint, 2, false
	if _, v, err = r.SetWebhookEndpoint(context.Background(), scope, in); err != nil || v != 3 {
		t.Fatalf("update: %d %v", v, err)
	}
	// An operator-supplied account that differs from the registered one is refused before any write.
	n := len(db.calls)
	wrong := in
	wrong.AccountID = "acct_1SomeoneElse000"
	if _, _, err := r.SetWebhookEndpoint(context.Background(), scope, wrong); !errors.Is(err, ErrRejected) || len(db.calls) != n+1 {
		t.Fatalf("mismatched account: %v calls=%d", err, len(db.calls)-n)
	}
	for _, bad := range []EndpointInput{
		{ConnectionID: conn, AccountID: acct, Profile: "SANDBOX", ExpectedVersion: 2, Secrets: in.Secrets},                    // update without id
		{ConnectionID: conn, AccountID: acct, Profile: "SANDBOX", EndpointID: endpoint, Secrets: in.Secrets},                  // id without version
		{ConnectionID: conn, AccountID: acct, Profile: "LIVE", Secrets: in.Secrets},                                           // LIVE
		{ConnectionID: conn, AccountID: acct, Profile: "SANDBOX", Secrets: accounts.StripeWebhookSecrets{CurrentSecret: "x"}}, // bad secret
	} {
		// Nothing is written: at most the read-only account lookup precedes secret validation.
		n := len(db.calls)
		if _, _, err := r.SetWebhookEndpoint(context.Background(), scope, bad); err == nil {
			t.Fatalf("bad endpoint input accepted: %+v", bad)
		}
		for _, c := range db.calls[n:] {
			if strings.Contains(c.sql, "set_stripe_webhook_endpoint") {
				t.Fatalf("bad endpoint input reached the write: %+v", bad)
			}
		}
	}
}

func TestQualifySandboxProbeAndMock(t *testing.T) {
	r, db, api, _, srv := fixture(t, acct)
	stored := storedCredential(t, api, acct, 2, testKey)
	db.reply = func(args []any) any { // 5 args = registrar credential read (S4); else echo qualification
		if len(args) == 5 {
			return stored
		}
		return args[3].(string)
	}
	in := QualifyInput{ConnectionID: conn, AccountID: acct, SecretKey: testKey, Profile: "SANDBOX",
		Currency: "HKD", ReturnURL: returnURL, ExpectedVersion: 2, AmountMinor: 400}
	q, err := r.Qualify(context.Background(), scope, in)
	if err != nil || q == "" {
		t.Fatalf("sandbox qualify: %v", err)
	}
	a := db.calls[len(db.calls)-1].args
	if a[5] != int64(2) || a[6] != "SANDBOX" || !strings.HasPrefix(a[7].(string), "stripe-probe:cs_test_fake_") {
		t.Fatalf("unexpected qualify args: %v", a[5:])
	}
	if !strings.Contains(db.calls[len(db.calls)-1].sql, "now(),now()+interval '30 days'") {
		t.Fatal("qualify must use the DB transaction clock")
	}
	keys := srv.CreateKeys()
	if len(keys) != 1 || keys[0] != "lc:stripe:probe:v1:"+q {
		t.Fatalf("probe create key: %v", keys)
	}
	// PROVIDER_MOCK: no provider traffic at all.
	before := srv.Counts()
	mock := in
	mock.Profile, mock.SecretKey = "PROVIDER_MOCK", ""
	if _, err := r.Qualify(context.Background(), scope, mock); err != nil {
		t.Fatalf("mock qualify: %v", err)
	}
	if srv.Counts() != before {
		t.Fatal("PROVIDER_MOCK qualification touched the network transport")
	}
	if got := db.calls[len(db.calls)-1].args[7].(string); !strings.HasPrefix(got, "provider-mock:") {
		t.Fatalf("mock evidence %q", got)
	}
	// Probe input error is a rejection and writes nothing.
	n := len(db.calls)
	in.AmountMinor = 399
	// Only the read-only credential lookup may precede the probe; nothing is written.
	if _, err := r.Qualify(context.Background(), scope, in); !errors.Is(err, ErrRejected) || wrote(&fakeDB{calls: db.calls[n:]}, "qualify_stripe_method") {
		t.Fatalf("invalid probe amount: %v", err)
	}
}

func TestSetMethodPassesEveryField(t *testing.T) {
	r, db, _, _, _ := fixture(t, acct)
	db.reply = int64(1)
	in := MethodInput{MarketID: conn, Country: "HK", ConnectionID: conn, QualificationID: conn, Enabled: true,
		Visible: true, Sort: 10, MinMinor: 400, MaxMinor: 99999, NameHans: "a", NameHant: "b", NameEN: "c"}
	if v, err := r.SetMethod(context.Background(), scope, in); err != nil || v != 1 || len(db.calls[0].args) != 16 {
		t.Fatalf("set method: %v", err)
	}
	for _, mutate := range []func(*MethodInput){
		func(m *MethodInput) { m.Country = "hk" },
		func(m *MethodInput) { m.Sort = 1001 }, func(m *MethodInput) { m.QualificationID = "x" },
	} {
		bad := in
		mutate(&bad)
		if _, err := r.SetMethod(context.Background(), scope, bad); !errors.Is(err, ErrConfig) {
			t.Fatal("bad method input accepted")
		}
	}
	// Amount bounds are operator input: same class as the definer's 22023/PT409 (ErrRejected), no SQL.
	for _, mutate := range []func(*MethodInput){func(m *MethodInput) { m.MaxMinor = 1 }, func(m *MethodInput) { m.MinMinor = 0 }} {
		bad := in
		mutate(&bad)
		n := len(db.calls)
		if _, err := r.SetMethod(context.Background(), scope, bad); !errors.Is(err, ErrRejected) || len(db.calls) != n {
			t.Fatalf("bad method bounds: %v", err)
		}
	}
}

func TestSQLErrorVocabularyAndRedaction(t *testing.T) {
	for code, want := range map[string]error{"22023": ErrRejected, "PT409": ErrRejected, "42501": ErrRejected,
		"23505": ErrRejected, "23514": ErrRejected, "40001": ErrDatabase, "57014": ErrDatabase} {
		r, db, _, _, _ := fixture(t, acct)
		db.err = &pgconn.PgError{Code: code, Message: "secret detail " + testKey}
		_, err := r.SetMethod(context.Background(), scope, MethodInput{MarketID: conn, Country: "HK", ConnectionID: conn,
			QualificationID: conn, MinMinor: 1, MaxMinor: 1})
		if !errors.Is(err, want) || strings.Contains(err.Error(), "secret") {
			t.Fatalf("code %s -> %v", code, err)
		}
	}
	for _, rendered := range []string{fmt.Sprint(Registrar{}), fmt.Sprintf("%+v", Registrar{}), fmt.Sprintf("%#v", Registrar{})} {
		if !strings.Contains(rendered, "redacted") {
			t.Fatal("Registrar formatting not redacted")
		}
	}
}

func TestOpenValidatesTransportsAndNeedsKeys(t *testing.T) {
	rt := http.RoundTripper(nil)
	if _, err := Open(context.Background(), "postgres://x", nil, nil, rt); !errors.Is(err, ErrConfig) {
		t.Fatal("nil transport accepted")
	}
	srv := stripetest.New(acct)
	defer srv.Close()
	if _, err := Open(context.Background(), "postgres://x", nil, nil, srv.Transport(), srv.Transport()); !errors.Is(err, ErrConfig) {
		t.Fatal("two transports accepted")
	}
	r := newRegistrar(&fakeDB{}, nil, nil)
	if _, err := r.Register(context.Background(), scope, acct, testKey); !errors.Is(err, ErrConfig) {
		t.Fatal("register without api keyring")
	}
	if _, _, err := r.SetWebhookEndpoint(context.Background(), scope, EndpointInput{ConnectionID: conn, AccountID: acct, Profile: "SANDBOX"}); !errors.Is(err, ErrConfig) {
		t.Fatal("endpoint without signing keyring")
	}
	r.Close()
}

// storedCredential is what payments.stripe_registrar_credential returns for one connection version.
func storedCredential(t *testing.T, api *accounts.Keyring, account string, version int64, key string) []any {
	t.Helper()
	keyID, nonce, ciphertext, err := api.SealStripeAPI(accounts.StripeAPIScope{TenantID: tenant, StoreID: store,
		ConnectionID: conn, Environment: "SANDBOX", AccountID: account, CredentialVersion: version},
		accounts.StripeAPICredentials{SecretKey: key})
	if err != nil {
		t.Fatal(err)
	}
	return []any{account, keyID, nonce, ciphertext}
}

func wrote(db *fakeDB, fn string) bool {
	for _, c := range db.calls {
		if strings.Contains(c.sql, fn) {
			return true
		}
	}
	return false
}

// S4: the SANDBOX probe runs with the STORED credential at expected_version against the connection's
// registered account. The env key/account are only an operator assertion that must match it.
func TestQualifyUsesStoredCredentialAndRegisteredAccount(t *testing.T) {
	const (
		acctB = "acct_1ForeignAccount0"
		oldK  = "sk_" + "test_REGISTRAROLDKEY0000000"
		foreK = "sk_" + "test_REGISTRARFOREIGNKEY0000"
	)
	r, db, api, _, srv := fixture(t, acct)
	for account, key := range map[string]string{acct: testKey, acctB: foreK} {
		if err := srv.AddAccount(account, key); err != nil {
			t.Fatal(err)
		}
	}
	if err := srv.AddAccount(acct, oldK); err != nil { // the pre-rotation key still verifies for acct
		t.Fatal(err)
	}
	stored := storedCredential(t, api, acct, 2, testKey)
	db.reply = func(args []any) any {
		if len(args) == 5 {
			return stored
		}
		return args[3].(string)
	}
	base := QualifyInput{ConnectionID: conn, Profile: "SANDBOX", Currency: "HKD", ReturnURL: returnURL,
		ExpectedVersion: 2, AmountMinor: 400}
	for name, in := range map[string]QualifyInput{
		"foreign account and key": {AccountID: acctB, SecretKey: foreK},
		"old key of the account":  {AccountID: acct, SecretKey: oldK},
	} {
		in.ConnectionID, in.Profile, in.Currency, in.ReturnURL = base.ConnectionID, base.Profile, base.Currency, base.ReturnURL
		in.ExpectedVersion, in.AmountMinor = base.ExpectedVersion, base.AmountMinor
		if _, err := r.Qualify(context.Background(), scope, in); !errors.Is(err, ErrRejected) {
			t.Fatalf("%s qualified version 2: %v", name, err)
		}
		if wrote(db, "qualify_stripe_method") || len(srv.CreateKeys()) != 0 {
			t.Fatalf("%s: evidence written or a probe was created", name)
		}
	}
	// Env credentials omitted: the stored credential alone drives the probe.
	q, err := r.Qualify(context.Background(), scope, base)
	if err != nil || q == "" {
		t.Fatalf("stored-credential qualify: %v", err)
	}
	want := stripetest.KeyFingerprint(testKey)
	for _, req := range srv.Requests() {
		if req.Path != "" && req.KeyFingerprint != want && req.KeyFingerprint != "" {
			t.Fatalf("a probe request used a key other than the stored one: %s", req.Path)
		}
	}
	if len(srv.CreateKeys()) != 1 {
		t.Fatalf("probe creates: %v", srv.CreateKeys())
	}
	// No keyring for the API custody: SANDBOX qualification cannot open the stored key.
	nokeys := newRegistrar(db, nil, nil, srv.Transport())
	if _, err := nokeys.Qualify(context.Background(), scope, base); !errors.Is(err, ErrConfig) {
		t.Fatalf("qualify without an API keyring: %v", err)
	}
}

// S5: rotation seals the AAD with the REGISTERED account; a foreign account's key is refused.
func TestRotateBindsRegisteredAccountNotEnv(t *testing.T) {
	const acctB = "acct_1ForeignAccount0"
	r, db, api, _, _ := fixture(t, acctB) // the supplied key genuinely belongs to account B
	db.reply = func(args []any) any {
		if len(args) == 4 {
			return acct // registered account of the connection
		}
		return int64(5)
	}
	if _, err := r.Rotate(context.Background(), scope, conn, 4, acctB, testKey); !errors.Is(err, ErrRejected) {
		t.Fatalf("rotation with another account's key: %v", err)
	}
	if wrote(db, "rotate_stripe_key") {
		t.Fatal("a foreign-account envelope reached rotate_stripe_key")
	}
	// A matching account seals under the database value.
	r2, db2, _, _, _ := fixture(t, acct)
	db2.reply = db.reply
	if v, err := r2.Rotate(context.Background(), scope, conn, 4, acct, testKey); err != nil || v != 5 {
		t.Fatalf("matching rotation: %d %v", v, err)
	}
	a := db2.calls[len(db2.calls)-1].args
	if _, err := api.OpenStripeAPI(accounts.StripeAPIScope{TenantID: tenant, StoreID: store, ConnectionID: conn,
		Environment: "SANDBOX", AccountID: acct, CredentialVersion: 5}, a[5].(string), a[6].([]byte), a[7].([]byte)); err != nil {
		t.Fatal("rotation envelope is not bound to the registered account")
	}
}
