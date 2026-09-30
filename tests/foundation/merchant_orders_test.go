package foundation_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"reflect"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"livecommerce/internal/buyer"
	"livecommerce/internal/fulfillment"
	"livecommerce/internal/httpapi"
	"livecommerce/internal/merchantorders"
	"livecommerce/internal/pagination"
	"livecommerce/internal/platform"
	"livecommerce/internal/pricing"
	"livecommerce/internal/storefront"
	"livecommerce/migrations"
)

const moFunction = "identity.read_merchant_orders(bytea,uuid,uuid,integer,timestamptz,uuid,text)"

// moRead invokes the SQL boundary itself so these gates do not depend on the
// separately authored Go transport or its final permission check.
func moRead(ctx context.Context, pool *pgxpool.Pool, token, tenant, store, principal, order, state string, limit int) ([]map[string]any, error) {
	tx, err := pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(context.Background())
	if _, err = tx.Exec(ctx, `SELECT set_config('app.tenant_id',$1,true),set_config('app.store_id',$2,true),set_config('app.principal_id',$3,true)`, tenant, store, principal); err != nil {
		return nil, err
	}
	var raw []byte
	var orderArg any
	if order != "" {
		orderArg = order
	}
	err = tx.QueryRow(ctx, `SELECT identity.read_merchant_orders($1::bytea,$2::uuid,$3::uuid,$4::integer,NULL::timestamptz,NULL::uuid,$5::text)`, tokenHash(token), store, orderArg, limit, state).Scan(&raw)
	if err != nil {
		return nil, err
	}
	var rows []map[string]any
	if err = json.Unmarshal(raw, &rows); err != nil {
		return nil, err
	}
	return rows, tx.Commit(ctx)
}

func moGrant(t *testing.T, f *testFixture, tenant, store, principal string) {
	t.Helper()
	mustExec(t, f.owner, `INSERT INTO identity.store_grants(tenant_id,store_id,principal_id,permission)
		VALUES($1,$2,$3,'orders:read') ON CONFLICT DO NOTHING`, tenant, store, principal)
}

// Create a real checkout order in a fresh store while retaining the supplied
// tenant when requested. All product, buyer, quote, delivery and stock setup
// uses the ordinary APIs; no order row or financial fact is fabricated.
func moBeginInOtherStore(t *testing.T, base *testFixture, sameTenant bool) (string, string) {
	t.Helper()
	ctx := context.Background()
	f := *base
	if !sameTenant {
		f.tenantA = randomUUID()
		mustExec(t, f.owner, `INSERT INTO control.tenants(id,name) VALUES($1,'merchant order foreign tenant')`, f.tenantA)
	}
	f.storeA1, f.principalA = randomUUID(), randomUUID()
	f.tokens = map[string]string{"a": randomToken()}
	mustExec(t, f.owner, `INSERT INTO control.stores(tenant_id,id,name,currency) VALUES($1,$2,'merchant order foreign store','TWD')`, f.tenantA, f.storeA1)
	mustExec(t, f.owner, `INSERT INTO identity.principals(id) VALUES($1)`, f.principalA)
	mustExec(t, f.owner, `INSERT INTO identity.memberships(tenant_id,principal_id) VALUES($1,$2)`, f.tenantA, f.principalA)
	mustExec(t, f.owner, `INSERT INTO identity.store_grants(tenant_id,store_id,principal_id,permission)
		SELECT $1,$2,$3,p FROM unnest(ARRAY['store:read','catalog:read','catalog:write',
		'inventory:read','inventory:write','inventory:reserve','pricing:read','pricing:write',
		'integration:manage','integration:read','orders:read']) p`, f.tenantA, f.storeA1, f.principalA)
	tx, err := f.owner.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := insertSession(ctx, tx, f.tokens["a"], f.principalA, "merchant", time.Now().Add(time.Hour), nil); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	h := cqHarness{f: &f, a: openBuyerTestPools(t, &f), stock: t04CreateStock(t, &f, f.tokens["a"], f.storeA1, 10)}
	h.service, err = buyer.New(h.a.issuer, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	h.cap = mustIssue(t, h.service, f.storeA1)
	h.market, err = pricingScoped(ctx, &f, f.tokens["a"], f.storeA1, "pricing:write", func(tx pgx.Tx, scope platform.Scope) (pricing.Market, error) {
		return pricing.CreateMarket(ctx, tx, scope, t04Key("mo-other-market"), pricing.MarketInput{Code: "tw", Name: "Other store market", Currency: "TWD"})
	})
	if err != nil {
		t.Fatal(err)
	}
	zero := int64(0)
	h.policy = pricing.PolicyInput{MarketID: h.market.ID, Country: "TW", Currency: "TWD", ShippingMode: "country_flat", ShippingMinor: &zero, TaxMode: "none", TaxBasis: "goods", TaxRateBPS: &zero, QuoteTTLSeconds: 300, Enabled: true, ConfigurationRef: "synthetic merchant order fixture"}
	delivery := fulfillment.ServiceInput{MarketID: h.market.ID, Country: "TW", Code: "home", PolicyVersion: 1, NameHans: "测试配送", NameHant: "測試配送", NameEN: "Mock delivery", DeliveryKind: "home", Mode: "MANUAL", Enabled: true, Visible: true}
	dsPolicy(t, h, delivery, 0, 0, true)
	if _, err = dsSet(h, t04Key("mo-other-delivery"), delivery); err != nil {
		t.Fatal(err)
	}
	allocation := fulfillment.AllocationInput{MarketID: h.market.ID, Country: "TW", Code: "home", ExpectedServiceVersion: 1, WarehouseIDs: []string{h.stock.warehouse.ID}}
	if _, err = daSet(h, t04Key("mo-other-allocation"), allocation); err != nil {
		t.Fatal(err)
	}
	pool, err := platform.OpenCheckoutPool(ctx, bcRole(t, &f, "commerce_checkout_runtime"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	b := bcHarness{cqHarness: h, pool: pool, service: bcService(t, pool), delivery: delivery, allocation: allocation}
	b.prepare(t, h.cap, []storefront.Item{{SKUID: h.stock.skus[0].ID, Quantity: 1}})
	order, err := b.begin(t04Key("mo-other-begin"))
	if err != nil {
		t.Fatal(err)
	}
	return f.storeA1, order.OrderID
}

func TestMerchantOrdersAuthorityAndOnboarding(t *testing.T) {
	f := fixture(t)
	ctx := context.Background()
	var owner, volatility string
	var definer, noLogin, noBypass, fixedPath, publicExec bool
	if err := f.owner.QueryRow(ctx, `SELECT pg_get_userbyid(p.proowner),p.provolatile::text,p.prosecdef,
		NOT r.rolcanlogin,NOT r.rolbypassrls,p.proconfig=ARRAY['search_path=pg_catalog']::text[],
		EXISTS(SELECT 1 FROM aclexplode(p.proacl) a WHERE a.grantee=0 AND a.privilege_type='EXECUTE')
		FROM pg_proc p JOIN pg_roles r ON r.oid=p.proowner WHERE p.oid=$1::regprocedure`, moFunction).
		Scan(&owner, &volatility, &definer, &noLogin, &noBypass, &fixedPath, &publicExec); err != nil {
		t.Fatal(err)
	}
	if owner != "commerce_auth" || volatility != "v" || !definer || !noLogin || !noBypass || !fixedPath || publicExec {
		t.Fatalf("private read definer drift: owner=%s volatility=%s definer=%v no_login=%v no_bypass=%v path=%v public=%v", owner, volatility, definer, noLogin, noBypass, fixedPath, publicExec)
	}
	var execPrincipals []string
	if err := f.owner.QueryRow(ctx, `SELECT ARRAY(SELECT CASE WHEN a.grantee=0 THEN 'PUBLIC' ELSE pg_get_userbyid(a.grantee) END
		FROM pg_proc p CROSS JOIN LATERAL aclexplode(coalesce(p.proacl,acldefault('f',p.proowner))) a
		WHERE p.oid=$1::regprocedure AND a.privilege_type='EXECUTE' ORDER BY 1)`, moFunction).Scan(&execPrincipals); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(execPrincipals, []string{"commerce_auth", "commerce_runtime"}) {
		t.Fatalf("merchant read EXECUTE ACL principals=%v", execPrincipals)
	}
	for _, role := range []string{"commerce_runtime", "commerce_hosted_runtime", "commerce_buyer_runtime", "commerce_buyer_issuer", "commerce_checkout_runtime", "commerce_worker", "commerce_identity"} {
		var execute, checkoutUsage, orderRead, attemptRead, authMember bool
		if err := f.owner.QueryRow(ctx, `SELECT has_function_privilege($1,$2,'EXECUTE'),
			has_schema_privilege($1,'checkout','USAGE'),has_any_column_privilege($1,'checkout.orders','SELECT'),
			has_any_column_privilege($1,'checkout.payment_attempts','SELECT'),pg_has_role($1,'commerce_auth','MEMBER')`, role, moFunction).
			Scan(&execute, &checkoutUsage, &orderRead, &attemptRead, &authMember); err != nil {
			t.Fatal(err)
		}
		if execute != (role == "commerce_runtime") || authMember || (role == "commerce_runtime" && (checkoutUsage || orderRead || attemptRead)) {
			t.Fatalf("role %s direct read boundary: execute=%v schema=%v order=%v attempt=%v member=%v", role, execute, checkoutUsage, orderRead, attemptRead, authMember)
		}
	}
	for table, want := range map[string][]string{
		"checkout.orders":                {"collection_state", "commercial_state", "country", "created_at", "currency", "fulfillment_state", "id", "owner_id", "payment_mode", "service_code", "snapshot", "store_id", "tenant_id", "total_minor", "updated_at"}, // 0073 (taiwan-cvs C4): +collection_state, payment_mode for the merchant projection
		"checkout.payment_attempts":      {"amount_minor", "connection_id", "currency", "environment", "execution_profile", "id", "order_id", "owner_id", "store_id", "tenant_id"},
		"payments.facts":                 {"amount_minor", "attempt_id", "connection_id", "currency", "environment", "execution_profile", "kind", "received_at", "store_id", "tenant_id"}, // 0078 adds received_at (BD7 finance day)
		"payments.review_cases":          {"attempt_id", "reason", "store_id", "tenant_id"},                                                                                               // 0063 adds reason (MD6 review predicate)
		"fulfillment.payment_work_items": {"attempt_id", "order_id", "owner_id", "state", "store_id", "tenant_id"},
	} {
		var got []string
		if err := f.owner.QueryRow(ctx, `SELECT ARRAY(SELECT a.attname FROM pg_attribute a
			WHERE a.attrelid=$1::regclass AND a.attnum>0 AND NOT a.attisdropped
			AND has_column_privilege('commerce_auth',$1::regclass,a.attname,'SELECT') ORDER BY a.attname)`, table).Scan(&got); err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(got, want) {
			t.Fatalf("%s commerce_auth SELECT columns=%v want=%v", table, got, want)
		}
		var forced, write bool
		if err := f.owner.QueryRow(ctx, `SELECT relrowsecurity AND relforcerowsecurity,
			has_table_privilege('commerce_auth',oid,'INSERT,UPDATE,DELETE,TRUNCATE') FROM pg_class WHERE oid=$1::regclass`, table).Scan(&forced, &write); err != nil || !forced || write {
			t.Fatalf("%s RLS/write drift: forced=%v write=%v err=%v", table, forced, write, err)
		}
	}
	for _, schema := range []string{"checkout", "payments", "fulfillment"} {
		var usage bool
		if err := f.owner.QueryRow(ctx, `SELECT has_schema_privilege('commerce_auth',$1,'USAGE')`, schema).Scan(&usage); err != nil || !usage {
			t.Fatalf("commerce_auth lacks %s usage: %v", schema, err)
		}
	}
	// Existing members are not elevated by the migration.
	var oldCount int
	if err := f.owner.QueryRow(ctx, `SELECT count(*) FROM identity.store_grants WHERE tenant_id=$1 AND store_id=$2 AND principal_id=$3 AND permission='orders:read'`, f.tenantA, f.storeA1, f.principalA).Scan(&oldCount); err != nil || oldCount != 0 {
		t.Fatalf("existing owner auto-elevated: count=%d err=%v", oldCount, err)
	}
	s, _, _ := identityFixture(t)
	session := identityLogin(t, s)
	request := firstStoreRequest()
	key := t04Key("mo-onboard")
	store, err := s.CreateInitialStore(ctx, session.Token, key, request)
	if err != nil {
		t.Fatal(err)
	}
	var grants []string
	if err := f.owner.QueryRow(ctx, `SELECT ARRAY(SELECT permission FROM identity.store_grants WHERE tenant_id=$1 AND store_id=$2 AND principal_id=$3 AND permission='orders:read')`, store.TenantID, store.StoreID, session.PrincipalID).Scan(&grants); err != nil || !slices.Equal(grants, []string{"orders:read"}) {
		t.Fatalf("new owner orders grant=%v err=%v", grants, err)
	}
	mustExec(t, f.owner, `DELETE FROM identity.store_grants WHERE tenant_id=$1 AND store_id=$2 AND principal_id=$3 AND permission='orders:read'`, store.TenantID, store.StoreID, session.PrincipalID)
	replay, err := s.CreateInitialStore(ctx, session.Token, key, request)
	if err != nil || replay != store {
		t.Fatalf("same-key onboarding replay after removal: store=%+v err=%v", replay, err)
	}
	if err := f.owner.QueryRow(ctx, `SELECT ARRAY(SELECT permission FROM identity.store_grants WHERE tenant_id=$1 AND store_id=$2 AND principal_id=$3 AND permission='orders:read')`, store.TenantID, store.StoreID, session.PrincipalID).Scan(&grants); err != nil || len(grants) != 0 {
		t.Fatalf("onboarding replay restored revoked orders grant=%v err=%v", grants, err)
	}
}

func TestMerchantOrdersUpgradeDoesNotBackfillOldMembership(t *testing.T) {
	// Own container: this test rewinds only 0027's additions, creates a member
	// under the old vocabulary/function, then applies the actual forward 0027.
	f := pwIsolatedFixture(t)
	ctx := context.Background()
	oldFunction, err := os.ReadFile("../../migrations/0019_owner_integration_settings.sql")
	if err != nil {
		t.Fatal(err)
	}
	tx, err := f.owner.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	for _, statement := range []string{
		`DROP FUNCTION identity.read_merchant_orders(bytea,uuid,uuid,integer,timestamptz,uuid,text)`,
		`DROP POLICY merchant_order_projection ON checkout.orders`,
		`DROP POLICY merchant_order_projection ON checkout.payment_attempts`,
		`DROP POLICY merchant_order_projection ON payments.facts`,
		`DROP POLICY merchant_order_projection ON payments.review_cases`,
		`DROP POLICY merchant_order_projection ON fulfillment.payment_work_items`,
		`DROP INDEX checkout.merchant_orders_history`,
		`ALTER TABLE identity.store_grants DROP CONSTRAINT store_grants_permission_check`,
		`ALTER TABLE identity.store_grants ADD CONSTRAINT store_grants_permission_check CHECK
			(permission IN ('store:read','audit:read','audit:write','catalog:read','catalog:write',
			'inventory:read','inventory:write','inventory:reserve','pricing:read','pricing:write',
			'integration:manage','integration:execute','integration:read'))`,
		string(oldFunction),
		`DELETE FROM public.lc_schema_migrations WHERE version='0027_merchant_orders.sql'`,
	} {
		if _, err := tx.Exec(ctx, statement); err != nil {
			t.Fatalf("restore pre-0027 isolated schema: %v", err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	oldTenant, oldStore, oldPrincipal := randomUUID(), randomUUID(), randomUUID()
	mustExec(t, f.owner, `INSERT INTO control.tenants(id,name) VALUES($1,'pre-0027 tenant')`, oldTenant)
	mustExec(t, f.owner, `INSERT INTO control.stores(tenant_id,id,name,currency) VALUES($1,$2,'pre-0027 store','TWD')`, oldTenant, oldStore)
	mustExec(t, f.owner, `INSERT INTO identity.principals(id) VALUES($1)`, oldPrincipal)
	mustExec(t, f.owner, `INSERT INTO identity.memberships(tenant_id,principal_id) VALUES($1,$2)`, oldTenant, oldPrincipal)
	mustExec(t, f.owner, `INSERT INTO identity.store_grants(tenant_id,store_id,principal_id,permission) VALUES($1,$2,$3,'store:read')`, oldTenant, oldStore, oldPrincipal)
	if _, err := f.owner.Exec(ctx, `INSERT INTO identity.store_grants(tenant_id,store_id,principal_id,permission) VALUES($1,$2,$3,'orders:read')`, oldTenant, oldStore, oldPrincipal); sqlState(err) != "23514" {
		t.Fatalf("old permission vocabulary accepted orders:read: %v", err)
	}
	if err := migrations.Apply(ctx, f.owner); err != nil {
		t.Fatalf("forward 0027 against old membership: %v", err)
	}
	var oldGrants int
	if err := f.owner.QueryRow(ctx, `SELECT count(*) FROM identity.store_grants WHERE tenant_id=$1 AND store_id=$2 AND principal_id=$3 AND permission='orders:read'`, oldTenant, oldStore, oldPrincipal).Scan(&oldGrants); err != nil || oldGrants != 0 {
		t.Fatalf("0027 backfilled existing member: count=%d err=%v", oldGrants, err)
	}
	newPrincipal, newToken, key := randomUUID(), randomToken(), t04Key("mo-upgrade-new")
	mustExec(t, f.owner, `INSERT INTO identity.principals(id) VALUES($1)`, newPrincipal)
	sessionTx, err := f.owner.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := insertSession(ctx, sessionTx, newToken, newPrincipal, "merchant", time.Now().Add(time.Hour), nil); err != nil {
		t.Fatal(err)
	}
	if err := sessionTx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	create := func() (string, string) {
		t.Helper()
		var tenant, store, warehouse string
		if err := f.owner.QueryRow(ctx, `SELECT tenant_id,store_id,warehouse_id FROM identity.create_initial_store($1,$2,$3,'post-upgrade tenant','post-upgrade store','warehouse','TWD')`, tokenHash(newToken), key, tokenHash(key)).Scan(&tenant, &store, &warehouse); err != nil {
			t.Fatal(err)
		}
		return tenant, store
	}
	newTenant, newStore := create()
	if tenant, store := create(); tenant != newTenant || store != newStore {
		t.Fatal("onboarding replay changed created store")
	}
	var freshGrants int
	if err := f.owner.QueryRow(ctx, `SELECT count(*) FROM identity.store_grants WHERE tenant_id=$1 AND store_id=$2 AND principal_id=$3 AND permission='orders:read'`, newTenant, newStore, newPrincipal).Scan(&freshGrants); err != nil || freshGrants != 1 {
		t.Fatalf("fresh onboarding orders grant=%d err=%v", freshGrants, err)
	}
	mustExec(t, f.owner, `DELETE FROM identity.store_grants WHERE tenant_id=$1 AND store_id=$2 AND principal_id=$3 AND permission='orders:read'`, newTenant, newStore, newPrincipal)
	create()
	if err := f.owner.QueryRow(ctx, `SELECT count(*) FROM identity.store_grants WHERE tenant_id=$1 AND store_id=$2 AND principal_id=$3 AND permission='orders:read'`, newTenant, newStore, newPrincipal).Scan(&freshGrants); err != nil || freshGrants != 0 {
		t.Fatalf("onboarding replay restored removed grant=%d err=%v", freshGrants, err)
	}
}

func TestMerchantOrdersSQLProjectionAndIsolation(t *testing.T) {
	b := bcSetup(t)
	ctx := context.Background()
	first, err := b.begin(t04Key("mo-buyer-one"))
	if err != nil {
		t.Fatal(err)
	}
	secondBuyer := mustIssue(t, b.cqHarness.service, b.f.storeA1)
	secondHarness := b
	secondHarness.prepare(t, secondBuyer, []storefront.Item{{SKUID: b.stock.skus[0].ID, Quantity: 1}})
	second, err := secondHarness.begin(t04Key("mo-buyer-two"))
	if err != nil {
		t.Fatal(err)
	}
	sameTenantStore, sameTenantOrder := moBeginInOtherStore(t, b.f, true)
	otherTenantStore, otherTenantOrder := moBeginInOtherStore(t, b.f, false)
	for _, foreignOrder := range []string{sameTenantOrder, otherTenantOrder} {
		if got := countRows(t, b.f.owner, `SELECT count(*) FROM checkout.orders WHERE id=$1`, foreignOrder); got != 1 {
			t.Fatalf("foreign order witness %s not created through checkout", foreignOrder)
		}
	}
	moGrant(t, b.f, b.f.tenantA, b.f.storeA1, b.f.principalA)
	rows, err := moRead(ctx, b.f.runtime, b.f.tokens["a"], b.f.tenantA, b.f.storeA1, b.f.principalA, "", "all", 101)
	if err != nil {
		t.Fatal(err)
	}
	wantKeys := []string{"collection_state", "commercial_state", "created_at", "currency", "fulfillment_state", "order_id", "payment_mode", "payment_state", "pickup_source", "refund_pending_minor", "refunded_minor", "test_mode", "total_minor", "updated_at", "work_state"} // 0063: stripe-refund-v1 §7.1 amounts
	ids := map[string]bool{first.OrderID: false, second.OrderID: false}
	for _, row := range rows {
		id, _ := row["order_id"].(string)
		if id == sameTenantOrder || id == otherTenantOrder {
			t.Fatalf("authorized A1 list leaked actual foreign order %s", id)
		}
		if _, ok := ids[id]; !ok {
			continue // Other serial foundation cases may have created this base-store order.
		}
		keys := make([]string, 0, len(row))
		for k := range row {
			keys = append(keys, k)
		}
		slices.Sort(keys)
		if !slices.Equal(keys, wantKeys) || row["commercial_state"] != "DRAFT" || row["payment_state"] != "NOT_STARTED" || row["work_state"] != "NONE" || row["test_mode"] != false {
			t.Fatalf("summary of %s violated exact safe projection: %+v", id, row)
		}
		ids[id] = true
	}
	if !ids[first.OrderID] || !ids[second.OrderID] {
		t.Fatalf("merchant missed independent buyers: %v", ids)
	}
	detail, err := moRead(ctx, b.f.runtime, b.f.tokens["a"], b.f.tenantA, b.f.storeA1, b.f.principalA, first.OrderID, "all", 1)
	if err != nil || len(detail) != 1 {
		t.Fatalf("owned detail: %v rows=%d", err, len(detail))
	}
	for _, key := range []string{"country", "service_code", "items", "totals", "destination", "shipment"} { // 0063: manual-fulfilment-v1 §4.1
		if _, ok := detail[0][key]; !ok {
			t.Fatalf("detail missing %s", key)
		}
	}
	encoded, _ := json.Marshal(rows)
	for _, secret := range []string{b.cap.Scope.OwnerID, b.cap.Scope.SessionID, b.destination.RecipientName, b.destination.Phone, b.destination.HomeAddress.Line1, "snapshot", "cart_id", "psp_reference"} {
		if strings.Contains(string(encoded), secret) {
			t.Fatalf("summary disclosed %s", secret)
		}
	}
	// A same-tenant second store and a second tenant remain invisible even to
	// a caller holding orders:read on the first store.
	for _, foreignStore := range []string{sameTenantStore, otherTenantStore} {
		_, err := moRead(ctx, b.f.runtime, b.f.tokens["a"], b.f.tenantA, foreignStore, b.f.principalA, first.OrderID, "all", 1)
		if sqlState(err) != "PT404" {
			t.Fatalf("foreign store %s disclosed detail: %v", foreignStore, err)
		}
	}
	moGrant(t, b.f, b.f.tenantA, sameTenantStore, b.f.principalA)
	mustExec(t, b.f.owner, `INSERT INTO identity.store_grants(tenant_id,store_id,principal_id,permission) VALUES($1,$2,$3,'store:read') ON CONFLICT DO NOTHING`, b.f.tenantA, sameTenantStore, b.f.principalA)
	foreignRows, err := moRead(ctx, b.f.runtime, b.f.tokens["a"], b.f.tenantA, sameTenantStore, b.f.principalA, "", "all", 101)
	if err != nil || len(foreignRows) != 1 || foreignRows[0]["order_id"] != sameTenantOrder {
		t.Fatalf("authorized same-tenant second store row=%+v err=%v", foreignRows, err)
	}
	_, err = moRead(ctx, b.f.runtime, b.f.tokens["buyer"], b.f.tenantA, b.f.storeA1, b.f.principalA, first.OrderID, "all", 1)
	if sqlState(err) != "PT401" {
		t.Fatalf("buyer audience gained merchant detail: %v", err)
	}
	_, err = moRead(ctx, b.f.runtime, b.f.tokens["a"], b.f.tenantA, b.f.storeA1, randomUUID(), first.OrderID, "all", 1)
	if sqlState(err) != "PT403" {
		t.Fatalf("forged GUC gained detail: %v", err)
	}
}

func TestMerchantOrdersFinalSQLFenceAfterObservedDataLock(t *testing.T) {
	b := bcSetup(t)
	order, err := b.begin(t04Key("mo-fence-order"))
	if err != nil {
		t.Fatal(err)
	}
	_, foreignOrder := moBeginInOtherStore(t, b.f, true)
	ctx := context.Background()
	// Each case gets its own principal/session, so revocation remains durable and
	// a later case cannot accidentally inherit a repaired grant.
	for _, tc := range []struct {
		name, code string
		change     string
		order      string
	}{
		{"revoked session", "PT401", "UPDATE identity.sessions SET revoked_at=clock_timestamp() WHERE token_hash=$1", order.OrderID},
		{"expired session", "PT401", "", order.OrderID},
		{"missing orders grant", "PT403", "DELETE FROM identity.store_grants WHERE principal_id=$1 AND permission='orders:read'", order.OrderID},
		{"missing store grant", "PT404", "DELETE FROM identity.store_grants WHERE principal_id=$1 AND permission='store:read'", order.OrderID},
		{"membership disabled", "PT404", "UPDATE identity.memberships SET active=false WHERE principal_id=$1", order.OrderID},
		{"revision changed", "PT403", "UPDATE identity.memberships SET authz_revision=authz_revision+1 WHERE principal_id=$1", order.OrderID},
		{"principal disabled", "PT401", "UPDATE identity.principals SET active=false WHERE id=$1", order.OrderID},
		{"tenant disabled", "PT404", "UPDATE control.tenants SET active=false WHERE id=$1", order.OrderID},
		{"store disabled", "PT404", "UPDATE control.stores SET active=false WHERE id=$1", order.OrderID},
		{"missing detail after revoke", "PT401", "UPDATE identity.sessions SET revoked_at=clock_timestamp() WHERE token_hash=$1", randomUUID()},
		{"foreign detail after revoke", "PT401", "UPDATE identity.sessions SET revoked_at=clock_timestamp() WHERE token_hash=$1", foreignOrder},
		{"empty list after revoke", "PT401", "UPDATE identity.sessions SET revoked_at=clock_timestamp() WHERE token_hash=$1", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			principal, token := randomUUID(), randomToken()
			store := b.f.storeA1
			if tc.name == "empty list after revoke" {
				store = b.f.storeA2
			}
			mustExec(t, b.f.owner, `INSERT INTO identity.principals(id) VALUES($1)`, principal)
			mustExec(t, b.f.owner, `INSERT INTO identity.memberships(tenant_id,principal_id) VALUES($1,$2)`, b.f.tenantA, principal)
			mustExec(t, b.f.owner, `INSERT INTO identity.store_grants(tenant_id,store_id,principal_id,permission)
				VALUES($1,$2,$3,'store:read'),($1,$2,$3,'orders:read')`, b.f.tenantA, store, principal)
			tx, err := b.f.owner.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if err = insertSession(ctx, tx, token, principal, "merchant", time.Now().Add(time.Hour), nil); err != nil {
				t.Fatal(err)
			}
			if err = tx.Commit(ctx); err != nil {
				t.Fatal(err)
			}
			_, err = moRead(ctx, b.f.runtime, token, b.f.tenantA, store, principal, tc.order, "all", 1)
			if tc.name == "missing detail after revoke" || tc.name == "foreign detail after revoke" {
				if sqlState(err) != "PT404" {
					t.Fatalf("pre-wait missing detail=%v", err)
				}
			} else if err != nil {
				t.Fatalf("pre-wait authorization failed: %v", err)
			}
			var expiry time.Time
			if tc.name == "expired session" {
				if err := b.f.owner.QueryRow(ctx, `UPDATE identity.sessions SET expires_at=clock_timestamp()+interval '1200 milliseconds' WHERE token_hash=$1 RETURNING expires_at`, tokenHash(token)).Scan(&expiry); err != nil {
					t.Fatal(err)
				}
			}
			name := "mo-reader-" + t04Tag()
			cfg := b.f.runtime.Config().Copy()
			cfg.ConnConfig.RuntimeParams["application_name"] = name
			reader, err := pgxpool.NewWithConfig(ctx, cfg)
			if err != nil {
				t.Fatal(err)
			}
			defer reader.Close()
			holder, err := b.f.owner.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer holder.Rollback(context.Background())
			var holderPID int
			if err = holder.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&holderPID); err != nil {
				t.Fatal(err)
			}
			if _, err = holder.Exec(ctx, `LOCK TABLE checkout.orders IN ACCESS EXCLUSIVE MODE`); err != nil {
				t.Fatal(err)
			}
			done := make(chan error, 1)
			go func() {
				_, e := moRead(ctx, reader, token, b.f.tenantA, store, principal, tc.order, "all", 1)
				done <- e
			}()
			waitForDatabaseLock(t, b.f.owner, name)
			var witnessed bool
			if err = b.f.owner.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_stat_activity a
				WHERE a.application_name=$1 AND $2=ANY(pg_blocking_pids(a.pid)))`, name, holderPID).Scan(&witnessed); err != nil || !witnessed {
				t.Fatalf("data-read blocker not causally witnessed: %v witnessed=%v", err, witnessed)
			}
			if tc.name == "expired session" {
				var stillValid bool
				if err := b.f.owner.QueryRow(ctx, `SELECT clock_timestamp()<$1`, expiry).Scan(&stillValid); err != nil || !stillValid {
					t.Fatalf("reader did not block before actual expiry: %v valid=%v", err, stillValid)
				}
				mustExec(t, b.f.owner, `SELECT pg_sleep(GREATEST(0,extract(epoch FROM $1::timestamptz-clock_timestamp()))+0.02)`, expiry)
			} else if strings.Contains(tc.change, "token_hash") {
				mustExec(t, b.f.owner, tc.change, tokenHash(token))
			} else if strings.Contains(tc.change, "control.tenants") {
				mustExec(t, b.f.owner, tc.change, b.f.tenantA)
				t.Cleanup(func() { mustExec(t, b.f.owner, `UPDATE control.tenants SET active=true WHERE id=$1`, b.f.tenantA) })
			} else if strings.Contains(tc.change, "control.stores") {
				mustExec(t, b.f.owner, tc.change, store)
				t.Cleanup(func() { mustExec(t, b.f.owner, `UPDATE control.stores SET active=true WHERE id=$1`, store) })
			} else {
				mustExec(t, b.f.owner, tc.change, principal)
			}
			if err = holder.Commit(ctx); err != nil {
				t.Fatal(err)
			}
			if got := sqlState(waitError(t, done)); got != tc.code {
				t.Fatalf("post-wait direct SQL code=%s want=%s", got, tc.code)
			}
		})
	}
}

func TestMerchantOrdersActualPaymentAndExpiryStates(t *testing.T) {
	ctx := context.Background()
	for _, mode := range []string{"pending", "authorized", "captured", "review"} {
		t.Run(mode, func(t *testing.T) {
			q := pqSetup(t)
			moGrant(t, q.f, q.f.tenantA, q.f.storeA1, q.f.principalA)
			read := func() map[string]any {
				t.Helper()
				rows, err := moRead(ctx, q.f.runtime, q.f.tokens["a"], q.f.tenantA, q.f.storeA1, q.f.principalA, q.hold.OrderID, "all", 1)
				if err != nil || len(rows) != 1 {
					t.Fatalf("merchant payment detail: %v rows=%d", err, len(rows))
				}
				return rows[0]
			}
			before := read()
			if before["commercial_state"] != "AWAITING_PAYMENT" || before["payment_state"] != "PENDING" || before["work_state"] != "NONE" || before["test_mode"] != true {
				t.Fatalf("real payment initiation projection: %+v", before)
			}
			if mode == "pending" {
				return
			}
			report := pcFull(q)
			switch mode {
			case "authorized":
				report["CloseStatus"] = "9"
				delete(report, "CloseAmountTWD")
			case "review":
				report["CardRefundStatus"], report["CardRefundAmountTWD"] = "2", int64(1)
			}
			hash := pcRecord(t, q, report)
			if err := pcApply(q.worker, q.result.AttemptID, hash); err != nil {
				t.Fatal(err)
			}
			wantPayment, wantWork, wantCommercial := "CAPTURED", "READY", "CONFIRMED"
			if mode == "authorized" {
				wantPayment, wantWork, wantCommercial = "AUTHORIZED", "NONE", "AWAITING_PAYMENT"
			} else if mode == "review" {
				wantPayment, wantWork, wantCommercial = "REVIEW_REQUIRED", "REVIEW_REQUIRED", "AWAITING_PAYMENT"
			}
			after := read()
			if after["payment_state"] != wantPayment || after["work_state"] != wantWork || after["commercial_state"] != wantCommercial {
				t.Fatalf("%s actual worker projection: payment=%v work=%v commercial=%v", mode, after["payment_state"], after["work_state"], after["commercial_state"])
			}
		})
	}
	t.Run("expired draft", func(t *testing.T) {
		b := bcSetup(t)
		order, err := b.begin(t04Key("mo-draft-expire"))
		if err != nil {
			t.Fatal(err)
		}
		moGrant(t, b.f, b.f.tenantA, b.f.storeA1, b.f.principalA)
		bcDue(t, b, order)
		if disposition := bcExpire(t, b, order, 1); disposition != "EXPIRED" {
			t.Fatalf("real checkout expiry=%s", disposition)
		}
		rows, err := moRead(ctx, b.f.runtime, b.f.tokens["a"], b.f.tenantA, b.f.storeA1, b.f.principalA, order.OrderID, "all", 1)
		if err != nil || len(rows) != 1 || rows[0]["commercial_state"] != "CANCELLED" || rows[0]["fulfillment_state"] != "CANCELLED" || rows[0]["payment_state"] != "NOT_STARTED" {
			t.Fatalf("expired order projection=%+v err=%v", rows, err)
		}
	})
	t.Run("paid allocation failure", func(t *testing.T) {
		q := pqSetup(t)
		moGrant(t, q.f, q.f.tenantA, q.f.storeA1, q.f.principalA)
		hash := pcRecord(t, q, pcFull(q))
		// Fault injection matches the existing capture gate: release a pending
		// reservation in the disposable database before the real capture worker
		// runs. Financial facts and review work are created only by pcApply.
		tx, err := q.f.owner.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback(ctx)
		if _, err = tx.Exec(ctx, `SELECT set_config('app.tenant_id',$1,true),set_config('app.store_id',$2,true),set_config('app.principal_id','',true),set_config('app.buyer_id',$3,true),set_config('app.buyer_session_id',$4,true)`, q.f.tenantA, q.f.storeA1, q.cap.Scope.OwnerID, q.cap.Scope.SessionID); err != nil {
			t.Fatal(err)
		}
		if _, err = tx.Exec(ctx, `INSERT INTO inventory.ledger(tenant_id,store_id,warehouse_id,sku_id,kind,delta_reserved,operation,command_key,reservation_id,principal_id,checkout_id,buyer_owner_id,buyer_session_id,actor_kind)
			SELECT l.tenant_id,l.store_id,l.warehouse_id,l.sku_id,'RELEASE',-l.quantity,'test.merchant_orders.release',$1,r.id,NULL,r.id,r.buyer_owner_id,r.buyer_session_id,'SYSTEM_EXPIRY'
			FROM inventory.reservations r JOIN inventory.reservation_lines l ON (l.tenant_id,l.store_id,l.reservation_id)=(r.tenant_id,r.store_id,r.id)
			WHERE r.id=$2`, q.result.AttemptID, q.hold.OrderID); err != nil {
			t.Fatal(err)
		}
		if _, err = tx.Exec(ctx, `UPDATE inventory.reservations SET state='RELEASED' WHERE id=$1`, q.hold.OrderID); err != nil {
			t.Fatal(err)
		}
		if _, err = tx.Exec(ctx, `UPDATE checkout.orders SET commercial_state='CANCELLED',fulfillment_state='CANCELLED' WHERE id=$1`, q.hold.OrderID); err != nil {
			t.Fatal(err)
		}
		if err = tx.Commit(ctx); err != nil {
			t.Fatal(err)
		}
		if err = pcApply(q.worker, q.result.AttemptID, hash); err != nil {
			t.Fatal(err)
		}
		if pcCount(t, q, "payments.facts", " AND kind='CAPTURED'") != 1 || pcCount(t, q, "payments.review_cases", " AND reason='PAID_ALLOCATION_FAILED'") != 1 {
			t.Fatal("capture worker did not persist money and allocation review")
		}
		rows, err := moRead(ctx, q.f.runtime, q.f.tokens["a"], q.f.tenantA, q.f.storeA1, q.f.principalA, q.hold.OrderID, "all", 1)
		if err != nil || len(rows) != 1 || rows[0]["commercial_state"] != "CANCELLED" || rows[0]["fulfillment_state"] != "PAID_ALLOCATION_FAILED" || rows[0]["payment_state"] != "REVIEW_REQUIRED" || rows[0]["work_state"] != "REVIEW_REQUIRED" {
			t.Fatalf("paid allocation failure projection=%+v err=%v", rows, err)
		}
	})
}

func TestMerchantOrdersHTTPPaginationPrivacyAndNoEffects(t *testing.T) {
	q := pqSetup(t)
	moGrant(t, q.f, q.f.tenantA, q.f.storeA1, q.f.principalA)
	secondBuyer := mustIssue(t, q.cqHarness.service, q.f.storeA1)
	secondHarness := q.bcHarness
	secondHarness.prepare(t, secondBuyer, []storefront.Item{{SKUID: q.stock.skus[0].ID, Quantity: 1}})
	second, err := secondHarness.begin(t04Key("mo-http-second-buyer"))
	if err != nil {
		t.Fatal(err)
	}
	// Tie both checkout-created orders at a microsecond boundary: only the UUID
	// can separate the keyset positions. Their state transitions still came from
	// the existing Begin/StartPayment APIs.
	mustExec(t, q.f.owner, `UPDATE checkout.orders SET created_at=(SELECT created_at FROM checkout.orders WHERE id=$1) WHERE id=$2`, q.hold.OrderID, second.OrderID)
	handler := httpapi.NewHandler(q.f.runtime)
	base := "/v1/admin/stores/" + q.f.storeA1 + "/orders"
	request := func(method, path, token string, body []byte, edit func(*http.Request)) (int, []byte) {
		t.Helper()
		var req *http.Request
		if body == nil {
			req = httptest.NewRequest(method, path, nil)
		} else {
			req = httptest.NewRequest(method, path, strings.NewReader(string(body)))
		}
		req.Header.Set("Authorization", "Bearer "+token)
		req.Host = "attacker.invalid"
		req.Header.Set("X-Forwarded-Host", "attacker.invalid")
		if edit != nil {
			edit(req)
		}
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, req)
		if w.Header().Get("Cache-Control") != "no-store" || strings.Contains(w.Body.String(), token) || strings.Contains(w.Body.String(), "SQLSTATE") {
			t.Fatalf("unsafe HTTP response status=%d body=%s", w.Code, w.Body.String())
		}
		return w.Code, w.Body.Bytes()
	}
	readPage := func(path string) pagination.Page[map[string]any] {
		t.Helper()
		status, raw := request("GET", path, q.f.tokens["a"], nil, nil)
		if status != 200 {
			t.Fatalf("GET %s status=%d body=%s", path, status, raw)
		}
		var page pagination.Page[map[string]any]
		if err := json.Unmarshal(raw, &page); err != nil || page.Items == nil {
			t.Fatalf("page decode: %v raw=%s", err, raw)
		}
		return page
	}
	before := map[string]int{}
	for _, table := range []string{"checkout.orders", "checkout.payment_attempts", "payments.facts", "inventory.ledger", "checkout.command_results", "checkout.events", "fulfillment.payment_work_items", "river.river_job", "river_payment.river_job", "river_expiry.river_job"} {
		before[table] = countRows(t, q.f.owner, "SELECT count(*) FROM "+table)
	}
	fingerprint := func() [2]string {
		t.Helper()
		var hashes [2]string
		if err := q.f.owner.QueryRow(context.Background(), `SELECT
			(SELECT md5(coalesce(string_agg(o::text,'|' ORDER BY o.id),'')) FROM checkout.orders o WHERE o.tenant_id=$1 AND o.store_id=$2),
			(SELECT md5(coalesce(string_agg(b::text,'|' ORDER BY b.warehouse_id,b.sku_id),'')) FROM inventory.balances b WHERE b.tenant_id=$1 AND b.store_id=$2)`, q.f.tenantA, q.f.storeA1).Scan(&hashes[0], &hashes[1]); err != nil {
			t.Fatal(err)
		}
		return hashes
	}
	beforeRows := fingerprint()
	full := readPage(base)
	if len(full.Items) != 2 || full.NextCursor != "" {
		t.Fatalf("all-buyers store page=%+v", full)
	}
	for _, item := range full.Items {
		if len(item) != 15 { // 10 + refunded_minor, refund_pending_minor (0063) + pickup_source, payment_mode, collection_state (0073, taiwan-cvs C4)
			t.Fatalf("summary has extra keys: %+v", item)
		}
	}
	first := readPage(base + "?limit=1")
	if len(first.Items) != 1 || first.NextCursor == "" || len(first.NextCursor) > 1024 {
		t.Fatalf("first keyset page=%+v", first)
	}
	last := readPage(base + "?limit=1&cursor=" + url.QueryEscape(first.NextCursor))
	ids := []string{q.hold.OrderID, second.OrderID}
	sort.Sort(sort.Reverse(sort.StringSlice(ids)))
	if len(last.Items) != 1 || last.NextCursor != "" || first.Items[0]["order_id"] != ids[0] || last.Items[0]["order_id"] != ids[1] {
		t.Fatalf("tied timestamp UUID keyset: first=%+v last=%+v want=%v", first, last, ids)
	}
	if page := readPage(base + "?state=DRAFT"); len(page.Items) != 1 || page.Items[0]["order_id"] != second.OrderID {
		t.Fatalf("state filter=%+v", page)
	}
	if page := readPage(base + "?state=CONFIRMED"); len(page.Items) != 0 || page.NextCursor != "" {
		t.Fatalf("empty state page=%+v", page)
	}
	if status, _ := request("GET", base+"?limit=1&state=DRAFT&cursor="+url.QueryEscape(first.NextCursor), q.f.tokens["a"], nil, nil); status != 422 {
		t.Fatal("cross-state cursor accepted")
	}
	otherStore := randomUUID()
	mustExec(t, q.f.owner, `INSERT INTO control.stores(tenant_id,id,name,currency) VALUES($1,$2,'merchant-order-other-store','TWD')`, q.f.tenantA, otherStore)
	mustExec(t, q.f.owner, `INSERT INTO identity.store_grants(tenant_id,store_id,principal_id,permission) VALUES($1,$2,$3,'store:read'),($1,$2,$3,'orders:read')`, q.f.tenantA, otherStore, q.f.principalA)
	otherBase := "/v1/admin/stores/" + otherStore + "/orders"
	if page := readPage(otherBase); len(page.Items) != 0 {
		t.Fatalf("same tenant other store saw orders: %+v", page)
	}
	if status, _ := request("GET", otherBase+"?cursor="+url.QueryEscape(first.NextCursor), q.f.tokens["a"], nil, nil); status != 422 {
		t.Fatal("cross-store cursor accepted")
	}
	for _, orderID := range []string{q.hold.OrderID, second.OrderID} {
		status, raw := request("GET", base+"/"+orderID, q.f.tokens["a"], nil, nil)
		var detail map[string]any
		if status != 200 || json.Unmarshal(raw, &detail) != nil || len(detail) != 21 { // 15 + refunded_minor, refund_pending_minor, shipment (0063) + the three CVS keys (0073)
			t.Fatalf("detail %s status=%d body=%s", orderID, status, raw)
		}
		if _, leaked := detail["owner_id"]; leaked {
			t.Fatal("owner_id leaked")
		}
	}
	missingStatus, missing := request("GET", base+"/"+randomUUID(), q.f.tokens["a"], nil, nil)
	foreignStatus, foreign := request("GET", otherBase+"/"+q.hold.OrderID, q.f.tokens["a"], nil, nil)
	var missingEnvelope, foreignEnvelope struct {
		Code, Message string
		Retryable     bool
	}
	if err := json.Unmarshal(missing, &missingEnvelope); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(foreign, &foreignEnvelope); err != nil {
		t.Fatal(err)
	}
	if missingStatus != 404 || foreignStatus != 404 || missingEnvelope != foreignEnvelope || missingEnvelope.Code != "not_found" {
		t.Fatalf("missing/other-store distinguishable: missing=%d %s foreign=%d %s", missingStatus, missing, foreignStatus, foreign)
	}
	noOrdersPrincipal, noOrdersToken := randomUUID(), randomToken()
	mustExec(t, q.f.owner, `INSERT INTO identity.principals(id) VALUES($1)`, noOrdersPrincipal)
	mustExec(t, q.f.owner, `INSERT INTO identity.memberships(tenant_id,principal_id) VALUES($1,$2)`, q.f.tenantA, noOrdersPrincipal)
	mustExec(t, q.f.owner, `INSERT INTO identity.store_grants(tenant_id,store_id,principal_id,permission) VALUES($1,$2,$3,'store:read')`, q.f.tenantA, q.f.storeA1, noOrdersPrincipal)
	buyerToken, expiredToken := randomToken(), randomToken()
	authTx, err := q.f.owner.Begin(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, session := range []struct {
		token, principal, audience string
		expires                    time.Time
	}{
		{noOrdersToken, noOrdersPrincipal, "merchant", time.Now().Add(time.Hour)},
		{buyerToken, q.f.principalA, "buyer", time.Now().Add(time.Hour)},
		{expiredToken, q.f.principalA, "merchant", time.Now().Add(-time.Minute)},
	} {
		if err := insertSession(context.Background(), authTx, session.token, session.principal, session.audience, session.expires, nil); err != nil {
			t.Fatal(err)
		}
	}
	if err := authTx.Commit(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		path, token string
		status      int
	}{
		{base, randomToken(), 401},
		{base, buyerToken, 401},
		{base, expiredToken, 401},
		{base, noOrdersToken, 403},
		{base + "?limit=0", q.f.tokens["a"], 422},
		{base + "?limit=101", q.f.tokens["a"], 422},
		{base + "?limit=1&limit=2", q.f.tokens["a"], 422},
		{base + "?buyer_id=" + secondBuyer.Scope.OwnerID, q.f.tokens["a"], 422},
		{base + "?cursor=bad", q.f.tokens["a"], 422},
		{base + "?state=draft", q.f.tokens["a"], 422},
		{base + "?", q.f.tokens["a"], 422},
		{base + "/" + q.hold.OrderID + "?state=all", q.f.tokens["a"], 422},
	} {
		if status, raw := request("GET", tc.path, tc.token, nil, nil); status != tc.status {
			t.Fatalf("HTTP %s status=%d want=%d body=%s", tc.path, status, tc.status, raw)
		}
	}
	for _, path := range []string{base, base + "/" + q.hold.OrderID} {
		if status, _ := request("HEAD", path, q.f.tokens["a"], nil, nil); status != 405 {
			t.Fatal("HEAD accepted")
		}
		if status, _ := request("GET", path, q.f.tokens["a"], []byte(`{}`), nil); status != 422 {
			t.Fatal("GET body accepted")
		}
		if status, _ := request("GET", path, q.f.tokens["a"], nil, func(req *http.Request) { req.Header.Set("Idempotency-Key", "read-key") }); status != 422 {
			t.Fatal("GET idempotency key accepted")
		}
	}
	for table, count := range before {
		if after := countRows(t, q.f.owner, "SELECT count(*) FROM "+table); after != count {
			t.Fatalf("read mutated %s: %d -> %d", table, count, after)
		}
	}
	if afterRows := fingerprint(); afterRows != beforeRows {
		t.Fatalf("read changed existing order/balance rows: before=%v after=%v", beforeRows, afterRows)
	}
}

func TestMerchantOrdersFrozenHistoryAfterMutableEdits(t *testing.T) {
	b := bcCVS(t)
	order, err := b.begin(t04Key("mo-frozen-pickup"))
	if err != nil {
		t.Fatal(err)
	}
	moGrant(t, b.f, b.f.tenantA, b.f.storeA1, b.f.principalA)
	handler := httpapi.NewHandler(b.f.runtime)
	path := "/v1/admin/stores/" + b.f.storeA1 + "/orders/" + order.OrderID
	read := func() (merchantorders.Detail, []byte) {
		t.Helper()
		r := httptest.NewRequest("GET", path, nil)
		r.Header.Set("Authorization", "Bearer "+b.f.tokens["a"])
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		if w.Code != 200 || w.Header().Get("Cache-Control") != "no-store" {
			t.Fatalf("history HTTP status=%d body=%s", w.Code, w.Body.String())
		}
		var detail merchantorders.Detail
		if err := json.Unmarshal(w.Body.Bytes(), &detail); err != nil {
			t.Fatal(err)
		}
		return detail, slices.Clone(w.Body.Bytes())
	}
	before, beforeRaw := read()
	if before.OrderID != order.OrderID || len(before.Items) != len(b.quote.Lines) || before.Destination.Pickup == nil || before.Destination.Pickup.Code != "017888" || before.Destination.RecipientName != "Synthetic Recipient" || before.TotalMinor != b.quote.Amount.TotalMinor || before.Totals.TotalMinor != b.quote.Amount.TotalMinor {
		t.Fatalf("initial frozen pickup projection=%+v", before)
	}
	for i, item := range before.Items {
		line := b.quote.Lines[i]
		if item.SKUID != line.SKUID || item.Code != line.Code || item.Name != line.Name || item.Quantity != line.Quantity || item.UnitPriceMinor != line.UnitPriceMinor || item.Amount != line.Amount {
			t.Fatalf("line[%d] lost quote order or money: got=%+v want=%+v", i, item, line)
		}
	}
	for _, forbidden := range []string{b.cap.Scope.OwnerID, b.cap.Scope.SessionID, "synthetic only", "evidence_ref", "credential", "cart_id", "product_id", "snapshot"} {
		if strings.Contains(string(beforeRaw), forbidden) {
			t.Fatalf("detail disclosed %s", forbidden)
		}
	}
	// Current mutable sources change through their normal owner/buyer fixture
	// paths; the checkout-created order remains an immutable read snapshot.
	mustExec(t, b.f.owner, `UPDATE catalog.products SET name='Renamed after checkout' WHERE id=$1`, b.stock.product.ID)
	mustExec(t, b.f.owner, `UPDATE catalog.skus SET code='NEW-CODE',price_minor=99999 WHERE id=$1`, b.stock.skus[0].ID)
	mustExec(t, b.f.owner, `UPDATE control.stores SET name='Renamed store after checkout' WHERE id=$1`, b.f.storeA1)
	newDestination, err := bdSet(b.cqHarness, t04Key("mo-new-current-destination"), storefront.DestinationInput{
		ExpectedVersion: b.destination.Version,
		CartVersion:     b.input.CartVersion,
		Kind:            "home",
		Country:         "TW",
		RecipientName:   "New current recipient",
		Phone:           "+886900000099",
		HomeAddress:     storefront.HomeAddress{City: "Other city", Line1: "Other current address"},
	})
	if err != nil || newDestination.ID == b.destination.ID {
		t.Fatalf("current destination change failed: %+v %v", newDestination, err)
	}
	if err = bdRevoke(b.cqHarness, t04Key("mo-pickup-revoke"), b.destination.Pickup.ID); err != nil {
		t.Fatal(err)
	}
	after, afterRaw := read()
	if !reflect.DeepEqual(before, after) || !slices.Equal(beforeRaw, afterRaw) {
		t.Fatalf("mutable catalog/destination/pickup rewrote order history: before=%+v after=%+v", before, after)
	}
}
