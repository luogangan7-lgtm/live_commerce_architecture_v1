package foundation_test

// Shared helpers of the customers-billing gates (CB02-CB09). Prefix `cbx`. No test lives here.

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// cbxLogin opens a plain (non-strict) login that is a member of exactly the given roles. It is used only
// for calls the gate makes AS a role (consent_allows as commerce_auth, EXECUTE probes), never as a production pool.
func cbxLogin(t *testing.T, f *testFixture, roles ...string) *pgxpool.Pool {
	t.Helper()
	role := "cbx_" + t04Tag()
	password := randomToken()
	membership := ""
	for i, r := range roles {
		if i > 0 {
			membership += ","
		}
		membership += pgx.Identifier{r}.Sanitize()
	}
	mustExec(t, f.owner, `CREATE ROLE `+pgx.Identifier{role}.Sanitize()+` LOGIN NOSUPERUSER NOBYPASSRLS NOCREATEDB NOCREATEROLE NOREPLICATION IN ROLE `+membership+` PASSWORD '`+password+`'`)
	pool, err := pgxpool.New(context.Background(), roleURL(t, f.databaseURL, role, password))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		pool.Close()
		mustExec(t, f.owner, `DROP OWNED BY `+pgx.Identifier{role}.Sanitize())
		mustExec(t, f.owner, `DROP ROLE `+pgx.Identifier{role}.Sanitize())
	})
	return pool
}

// cbxOne returns the first column of the first row as text ("" for NULL or no row).
func cbxOne(t *testing.T, f *testFixture, q string, args ...any) string {
	t.Helper()
	var s *string
	if err := f.owner.QueryRow(context.Background(), q, args...).Scan(&s); err != nil && err != pgx.ErrNoRows {
		t.Fatalf("query %q: %v", q, err)
	}
	if s == nil {
		return ""
	}
	return *s
}

// cbxAllows calls the consent gate as the owner role (a fixture superuser: the gate's own behaviour with real
// logins and GUC states is CB04's).
func cbxAllows(t *testing.T, f *testFixture, tenant, store, owner, purpose, channel string) bool {
	t.Helper()
	var ok bool
	if err := f.owner.QueryRow(context.Background(), `SELECT customers.consent_allows($1,$2,$3,$4,$5)`, tenant, store, owner, purpose, channel).Scan(&ok); err != nil {
		t.Fatalf("consent_allows: %v", err)
	}
	return ok
}

// cbxSub is one apply_subscription call (12 arguments, contracts/customers-billing-v1.md §3.2).
type cbxSub struct {
	Env, Customer, ID, Status, Price string
	PS, PE                           *time.Time
	CancelEnd, Livemode              bool
	Created, Retrieved               time.Time
	MetaStore                        *string
}

// cbxSubFor is a well-formed subscription of customer/store with the given status.
func cbxSubFor(customer, store, status string) cbxSub {
	now := time.Now().UTC()
	metaStore := store
	start, end := now.Add(-24*time.Hour), now.Add(29*24*time.Hour)
	return cbxSub{Env: "SANDBOX", Customer: customer, ID: "sub_" + t04Tag(), Status: status, Price: "price_Test0001", PS: &start, PE: &end,
		Created: now.Add(-time.Hour), Retrieved: now, MetaStore: &metaStore}
}

// cbxApply calls billing.apply_subscription; only the commerce_stripe_ingress role may (C-4).
func cbxApply(pool *pgxpool.Pool, s cbxSub) (string, error) {
	var out string
	err := pool.QueryRow(context.Background(), `SELECT billing.apply_subscription($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11::uuid,$12)`,
		s.Env, s.Customer, s.ID, s.Status, s.Price, s.PS, s.PE, s.CancelEnd, s.Created, s.Retrieved, s.MetaStore, s.Livemode).Scan(&out)
	return out, err
}

// cbxStore creates an extra store of tenant (owner pool fixture).
func cbxStore(t *testing.T, f *testFixture, tenant string) string {
	t.Helper()
	id := randomUUID()
	mustExec(t, f.owner, `INSERT INTO control.stores(tenant_id,id,name,currency) VALUES($1,$2,'cbx store','TWD')`, tenant, id)
	return id
}

// cbxPin inserts the store's Stripe customer row directly (disclosed fixture: stores that do not go through the
// merchant pin_customer path) and returns the customer id.
func cbxPin(t *testing.T, f *testFixture, tenant, store string) string {
	t.Helper()
	customer := "cus_" + t04Tag()
	mustExec(t, f.owner, `INSERT INTO billing.store_customers(tenant_id,store_id,environment,stripe_customer_id,platform_account_id) VALUES($1,$2,'SANDBOX',$3,'acct_Platform0001')`, tenant, store, customer)
	return customer
}

// cbxRestrict makes the store RESTRICTED through the real ingress definer: a pinned customer with a canceled
// subscription. It returns the customer and the subscription id.
func cbxRestrict(t *testing.T, f *testFixture, ingress *pgxpool.Pool, tenant, store string) (customer, sub string) {
	t.Helper()
	customer = cbxPin(t, f, tenant, store)
	s := cbxSubFor(customer, store, "canceled")
	if res, err := cbxApply(ingress, s); err != nil || res != "applied" {
		t.Fatalf("restrict: %q %v", res, err)
	}
	return customer, s.ID
}

// cbxRegisterPSP registers a Stripe merchant account row (the registrar is not run; disclosed fixture) in ONE
// transaction, as psSetup does for its mock account: binding, merchant account, credential row (the current-credential
// FK is deferred). It is what platform_account_conflict reads (BD1).
func cbxRegisterPSP(t *testing.T, f *testFixture, tenant, store, principal, environment, account string) {
	t.Helper()
	ctx := context.Background()
	tx, err := f.owner.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	binding, connection := randomUUID(), randomUUID()
	for _, st := range []struct {
		q    string
		args []any
	}{
		{`INSERT INTO integration.bindings(id,tenant_id,store_id,principal_id,provider,external_asset_id) VALUES($1,$2,$3,$4,'stripe',$5)`, []any{binding, tenant, store, principal, environment + ":" + account}},
		{`INSERT INTO integration.merchant_accounts(id,tenant_id,store_id,principal_id,provider,environment,account_id,binding_id,credential_version) VALUES($1,$2,$3,$4,'stripe',$5,$6,$7,1)`, []any{connection, tenant, store, principal, environment, account, binding}},
		{`INSERT INTO integration.account_credentials(tenant_id,store_id,connection_id,version,key_id,nonce,ciphertext,principal_id) VALUES($1,$2,$3,1,'mock_key',decode(repeat('00',12),'hex'),decode(repeat('00',17),'hex'),$4)`, []any{tenant, store, connection, principal}},
	} {
		if _, err := tx.Exec(ctx, st.q, st.args...); err != nil {
			t.Fatalf("register PSP account %s: %v", account, err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("register PSP account %s: %v", account, err)
	}
}
