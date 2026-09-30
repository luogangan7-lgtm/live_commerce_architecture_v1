package foundation_test

import (
	"context"
	"os"
	"strings"
	"testing"
)

// TestMetaAdsGrantScript runs the operator script scripts/ops/grant-ads-permissions.sql (REAL_PG; psql meta-commands stripped,
// :'vars' substituted, the rest executed as written by the migration-owner connection). Gate for the r3 review P2 "nothing
// provisions ads:read / ads:manage / ads:approve": the script must grant exactly the three permissions to a real store
// creator, be idempotent, audit only when it inserted, and refuse anyone who is not the creator or not a member.
func TestMetaAdsGrantScript(t *testing.T) {
	f := fixture(t)
	ctx := context.Background()
	raw, err := os.ReadFile("../../scripts/ops/grant-ads-permissions.sql")
	if err != nil {
		t.Fatal(err)
	}
	var keep []string
	for _, line := range strings.Split(string(raw), "\n") {
		if !strings.HasPrefix(strings.TrimSpace(line), `\`) {
			keep = append(keep, line)
		}
	}
	script := strings.Join(keep, "\n")
	run := func(store, principal string) error {
		conn, err := f.owner.Acquire(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Release()
		sql := strings.NewReplacer(":'store_id'", "'"+store+"'", ":'principal_id'", "'"+principal+"'").Replace(script)
		_, err = conn.Exec(ctx, sql)
		if err != nil {
			_, _ = conn.Exec(ctx, "ROLLBACK") // the script's BEGIN was left open by the failed simple query
		}
		return err
	}
	count := func(q string, args ...any) int {
		var n int
		if err := f.owner.QueryRow(ctx, q, args...).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	// The 0065 store-creator set (identity.create_initial_store); keep in step with the script.
	creator := []string{"store:read", "audit:read", "audit:write", "catalog:read", "catalog:write", "inventory:read", "inventory:write",
		"inventory:reserve", "pricing:read", "pricing:write", "integration:read", "integration:manage", "orders:read", "live:read",
		"live:manage", "payments:refund", "fulfillment:write", "orders:export", "integration:execute"}
	adsGrants := func(store, principal string) int {
		return count(`SELECT count(*) FROM identity.store_grants WHERE store_id=$1 AND principal_id=$2 AND permission IN ('ads:read','ads:manage','ads:approve')`, store, principal)
	}
	audits := func(store, principal string) int {
		return count(`SELECT count(*) FROM ops.audit_events WHERE store_id=$1 AND principal_id=$2 AND action='store.permissions_granted'`, store, principal)
	}

	store := cbxStore(t, f, f.tenantA)
	member, _ := lcPrincipal(t, f, f.tenantA, []string{store}, "store:read", "catalog:read")
	if err := run(store, member); err == nil {
		t.Fatal("a member without the creator grant set was given ads permissions")
	}
	if n := adsGrants(store, member); n != 0 {
		t.Fatalf("%d ads grants written by a refused run (must be atomic)", n)
	}

	creatorID, _ := lcPrincipal(t, f, f.tenantA, []string{store}, creator...)
	if err := run(store, creatorID); err != nil {
		t.Fatalf("store creator: %v", err)
	}
	if n := adsGrants(store, creatorID); n != 3 || audits(store, creatorID) != 1 {
		t.Fatalf("after first run: %d ads grants, %d audit rows, want 3 and 1", n, audits(store, creatorID))
	}
	if err := run(store, creatorID); err != nil {
		t.Fatalf("rerun: %v", err)
	}
	if n := adsGrants(store, creatorID); n != 3 || audits(store, creatorID) != 1 {
		t.Fatalf("rerun not idempotent: %d ads grants, %d audit rows", n, audits(store, creatorID))
	}
	if n := count(`SELECT count(*) FROM identity.store_grants WHERE store_id=$1 AND principal_id=$2`, store, creatorID); n != len(creator)+3 {
		t.Fatalf("creator holds %d grants, want %d (nothing else touched)", n, len(creator)+3)
	}

	// Not a member of the store's tenant: refused.
	outsider := randomUUID()
	mustExec(t, f.owner, `INSERT INTO identity.principals(id) VALUES($1)`, outsider)
	if err := run(store, outsider); err == nil {
		t.Fatal("a principal outside the tenant was accepted")
	}
	// Unknown store: refused.
	if err := run(randomUUID(), creatorID); err == nil {
		t.Fatal("an unknown store was accepted")
	}
}
