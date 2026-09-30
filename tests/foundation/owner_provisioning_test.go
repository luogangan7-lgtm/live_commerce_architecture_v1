package foundation_test

// OP01 (contracts/stripe-refund-v1.md §12 and rulings): 0065_owner_provisioning.sql, REAL_PG in an isolated
// database. Written from the contract text only. What it proves:
//   - a store creator onboarded through identity.create_initial_store receives exactly the 0027 set plus
//     live:read, live:manage, payments:refund, fulfillment:write, orders:export, integration:execute on the new
//     store (ruling 24: six, spec change of §12);
//   - an onboarding replay (same key) adds nothing and does not restore a removed grant;
//   - other members of the tenant, principals created before 0065 and other stores get none of the six;
//   - the function body differs from its last pre-0065 definition ONLY by the permission array.

import (
	"context"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"livecommerce/internal/identity"
	"livecommerce/internal/platform"
)

var op01Base = []string{"store:read", "audit:read", "audit:write", "catalog:read", "catalog:write", "inventory:read", "inventory:write", "inventory:reserve", "pricing:read", "pricing:write", "integration:read", "integration:manage", "orders:read"}
var op01New = []string{"live:read", "live:manage", "payments:refund", "fulfillment:write", "orders:export", "integration:execute", "customers:read", "customers:privacy", "billing:manage"} // 0079 sixth version (customers-billing-v1 C-5)

func op01Sorted(lists ...[]string) []string {
	var all []string
	for _, l := range lists {
		all = append(all, l...)
	}
	sort.Strings(all)
	return all
}

func TestOwnerProvisioningOP01(t *testing.T) {
	f := pwIsolatedFixture(t) // clean grants: only what this test creates
	ctx := context.Background()
	role := "op01_" + strings.ReplaceAll(randomUUID(), "-", "")
	password := randomToken()
	mustExec(t, f.owner, `CREATE ROLE `+pgx.Identifier{role}.Sanitize()+` LOGIN NOSUPERUSER NOBYPASSRLS NOCREATEDB NOCREATEROLE IN ROLE commerce_identity PASSWORD '`+password+`'`)
	u, err := url.Parse(f.databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	u.User = url.UserPassword(role, password)
	pool, err := platform.OpenIdentityPool(ctx, u.String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	provider := &identityProvider{subject: randomUUID()}
	svc, err := identity.New(pool, provider, identity.Policy{ProviderKey: "op01-provider-v1", SessionTTL: time.Hour, OnboardingEnabled: true, Currencies: []string{"TWD"}})
	if err != nil {
		t.Fatal(err)
	}
	grantsOf := func(tenant, store, principal string) []string {
		t.Helper()
		var got []string
		if err := f.owner.QueryRow(ctx, `SELECT coalesce(array_agg(permission ORDER BY permission),'{}') FROM identity.store_grants WHERE tenant_id=$1 AND store_id=$2 AND principal_id=$3`, tenant, store, principal).Scan(&got); err != nil {
			t.Fatal(err)
		}
		return got
	}

	// a principal that exists BEFORE the migration's effect is observed: it must not be backfilled
	old := struct{ tenant, store, principal string }{f.tenantA, f.storeA1, f.principalA}
	oldBefore := grantsOf(old.tenant, old.store, old.principal)

	session := identityLogin(t, svc)
	key := "op01-store-" + randomUUID()
	first, err := svc.CreateInitialStore(ctx, session.Token, key, firstStoreRequest())
	if err != nil {
		t.Fatal(err)
	}
	want := op01Sorted(op01Base, op01New)
	if got := grantsOf(first.TenantID, first.StoreID, session.PrincipalID); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("creator grants\n got  %v\n want %v", got, want)
	}
	// exactly one audit row and one warehouse (behaviour of the body is otherwise unchanged)
	var audits, warehouses int
	if err := f.owner.QueryRow(ctx, `SELECT (SELECT count(*) FROM ops.audit_events WHERE tenant_id=$1 AND action='merchant.store_created'),(SELECT count(*) FROM inventory.warehouses WHERE tenant_id=$1)`, first.TenantID).Scan(&audits, &warehouses); err != nil || audits != 1 || warehouses != 1 {
		t.Fatalf("audit=%d warehouses=%d err=%v", audits, warehouses, err)
	}

	t.Run("replay adds nothing and restores nothing", func(t *testing.T) {
		total := func() int {
			var n int
			if err := f.owner.QueryRow(ctx, `SELECT count(*) FROM identity.store_grants WHERE tenant_id=$1`, first.TenantID).Scan(&n); err != nil {
				t.Fatal(err)
			}
			return n
		}
		before := total()
		again, err := svc.CreateInitialStore(ctx, session.Token, key, firstStoreRequest())
		if err != nil || again != first || total() != before {
			t.Fatalf("replay: %+v err=%v grants %d -> %d", again, err, before, total())
		}
		// a grant the merchant later lost is NOT restored by a replay
		mustExec(t, f.owner, `DELETE FROM identity.store_grants WHERE tenant_id=$1 AND store_id=$2 AND principal_id=$3 AND permission='payments:refund'`, first.TenantID, first.StoreID, session.PrincipalID)
		if _, err := svc.CreateInitialStore(ctx, session.Token, key, firstStoreRequest()); err != nil {
			t.Fatal(err)
		}
		for _, p := range grantsOf(first.TenantID, first.StoreID, session.PrincipalID) {
			if p == "payments:refund" {
				t.Fatal("an onboarding replay restored a removed payments:refund grant")
			}
		}
		if got := len(grantsOf(first.TenantID, first.StoreID, session.PrincipalID)); got != len(want)-1 {
			t.Fatalf("grants after replay: %d", got)
		}
	})

	t.Run("nobody else gains anything", func(t *testing.T) {
		newSet := strings.Join(op01New, "','")
		// (1) another member of the creator's tenant, created after onboarding, has only what it was given
		other := randomUUID()
		mustExec(t, f.owner, `INSERT INTO identity.principals(id) VALUES($1)`, other)
		mustExec(t, f.owner, `INSERT INTO identity.memberships(tenant_id,principal_id) VALUES($1,$2)`, first.TenantID, other)
		mustExec(t, f.owner, `INSERT INTO identity.store_grants(tenant_id,store_id,principal_id,permission) VALUES($1,$2,$3,'store:read')`, first.TenantID, first.StoreID, other)
		if _, err := svc.CreateInitialStore(ctx, session.Token, key, firstStoreRequest()); err != nil { // replay again: must not touch others
			t.Fatal(err)
		}
		if got := grantsOf(first.TenantID, first.StoreID, other); strings.Join(got, ",") != "store:read" {
			t.Fatalf("another member of the tenant has %v", got)
		}
		// (2) principals that existed before (the fixture principals) hold none of the six newer permissions
		var leaked int
		if err := f.owner.QueryRow(ctx, `SELECT count(*) FROM identity.store_grants WHERE principal_id<>$1 AND permission IN ('`+newSet+`')`, session.PrincipalID).Scan(&leaked); err != nil || leaked != 0 {
			t.Fatalf("%d grants of the new permissions exist for principals other than the creator (backfill): %v", leaked, err)
		}
		if got := grantsOf(old.tenant, old.store, old.principal); strings.Join(got, ",") != strings.Join(oldBefore, ",") {
			t.Fatalf("a pre-existing principal's grants changed: %v -> %v", oldBefore, got)
		}
		// (3) the creator has them ONLY on the store it created
		var elsewhere int
		if err := f.owner.QueryRow(ctx, `SELECT count(*) FROM identity.store_grants WHERE principal_id=$1 AND store_id<>$2`, session.PrincipalID, first.StoreID).Scan(&elsewhere); err != nil || elsewhere != 0 {
			t.Fatalf("the creator holds %d grants outside the new store: %v", elsewhere, err)
		}
	})

	t.Run("the permission CHECK admits all six (0065 needs 0062-0064 first)", func(t *testing.T) {
		var def string
		if err := f.owner.QueryRow(ctx, `SELECT pg_get_constraintdef(oid) FROM pg_constraint WHERE conrelid='identity.store_grants'::regclass AND conname='store_grants_permission_check'`).Scan(&def); err != nil {
			t.Fatal(err)
		}
		for _, p := range op01Sorted(op01Base, op01New) {
			if !strings.Contains(def, "'"+p+"'") {
				t.Errorf("store_grants_permission_check lacks %s", p)
			}
		}
	})

	t.Run("function body differs from its last pre-0065 definition only by the permission array", func(t *testing.T) {
		oldBody, file := srgOldUntil(t, "identity.create_initial_store", func(base string, post bool) bool { return !post && base >= "0065_" })
		var newBody, owner, config string
		var definer, publicExec bool
		var grantee string
		if err := f.owner.QueryRow(ctx, `SELECT p.prosrc,pg_get_userbyid(p.proowner),p.proconfig::text,p.prosecdef,
		 EXISTS(SELECT 1 FROM aclexplode(coalesce(p.proacl,acldefault('f',p.proowner))) a WHERE a.grantee=0 AND a.privilege_type='EXECUTE'),
		 coalesce((SELECT string_agg(pg_get_userbyid(a.grantee),',' ORDER BY 1) FROM aclexplode(p.proacl) a WHERE a.privilege_type='EXECUTE' AND a.grantee<>p.proowner),'')
		 FROM pg_proc p WHERE p.oid='identity.create_initial_store(bytea,text,bytea,text,text,text,text)'::regprocedure`).Scan(&newBody, &owner, &config, &definer, &publicExec, &grantee); err != nil {
			t.Fatal(err)
		}
		literal := regexp.MustCompile(`'[a-z_]+:[a-z_]+'`)
		strip := func(s string) string {
			s = literal.ReplaceAllString(s, "")
			s = strings.NewReplacer(",", " ", "\n", " ", "\t", " ").Replace(s)
			return strings.Join(strings.Fields(s), " ")
		}
		if strip(oldBody) != strip(newBody) {
			removed, added, unified := srgDiff(srgLines(strip(oldBody)), srgLines(strip(newBody)))
			t.Fatalf("the body differs beyond the permission literals (old from %s): removed %d added %d\n%s", file, len(removed), len(added), unified)
		}
		oldSet, newSet := map[string]bool{}, map[string]bool{}
		for _, l := range literal.FindAllString(oldBody, -1) {
			oldSet[strings.Trim(l, "'")] = true
		}
		for _, l := range literal.FindAllString(newBody, -1) {
			newSet[strings.Trim(l, "'")] = true
		}
		// the literal set may also contain non-permission tokens of the body (none today): compare as sets of differences
		var added []string
		for l := range newSet {
			if !oldSet[l] {
				added = append(added, l)
			}
		}
		sort.Strings(added)
		if strings.Join(added, ",") != strings.Join(op01Sorted(op01New), ",") {
			t.Fatalf("new permission literals %v, want exactly %v", added, op01Sorted(op01New))
		}
		for l := range oldSet {
			if !newSet[l] {
				t.Fatalf("the permission %s was dropped from the array", l)
			}
		}
		if owner != "commerce_identity_writer" || !definer || publicExec || !strings.Contains(config, "search_path=pg_catalog") || grantee != "commerce_identity" {
			t.Fatalf("owner=%s definer=%v public-exec=%v config=%s grantees=%q (owner/ACL/search_path must be unchanged)", owner, definer, publicExec, config, grantee)
		}
		var comment *string
		if err := f.owner.QueryRow(ctx, `SELECT obj_description('identity.create_initial_store(bytea,text,bytea,text,text,text,text)'::regprocedure,'pg_proc')`).Scan(&comment); err != nil || comment == nil || !strings.Contains(*comment, "0065") {
			t.Fatalf("COMMENT ON FUNCTION must name 0065 and 'no backfill': %v %v", comment, err)
		}
		if !strings.Contains(strings.ToLower(*comment), "backfill") {
			t.Fatalf("COMMENT ON FUNCTION must say there is no backfill: %s", *comment)
		}
	})
}
