package foundation_test

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/jackc/pgx/v5"

	"livecommerce/internal/platform"
)

func TestNewInitialStoreOwnerGetsAccountSettingsAndExecution(t *testing.T) {
	s, _, _ := identityFixture(t)
	f := fixture(t)
	ctx := context.Background()
	session := identityLogin(t, s)
	request := firstStoreRequest()
	const key = "initial-account-settings-v1"
	store, err := s.CreateInitialStore(ctx, session.Token, key, request)
	if err != nil {
		t.Fatal(err)
	}

	var grants []string
	if err := f.owner.QueryRow(ctx, `SELECT ARRAY(SELECT permission FROM identity.store_grants
		WHERE tenant_id=$1::uuid AND store_id=$2::uuid AND principal_id=$3::uuid
		AND permission LIKE 'integration:%' ORDER BY permission)`, store.TenantID, store.StoreID, session.PrincipalID).Scan(&grants); err != nil {
		t.Fatal(err)
	}
	// Ruling 24 (spec change of stripe-refund-v1 §12): 0065 also grants integration:execute, which the
	// claim-source definer requires with live:manage so the owner can bind their own posts.
	if !slices.Equal(grants, []string{"integration:execute", "integration:manage", "integration:read"}) {
		t.Fatalf("new owner integration grants = %v", grants)
	}
	for _, permission := range []string{"integration:read", "integration:manage", "integration:execute"} {
		if err := platform.WithScope(ctx, f.runtime, session.Token, store.StoreID, permission,
			func(_ pgx.Tx, scope platform.Scope) error {
				if scope.TenantID != store.TenantID || scope.PrincipalID != session.PrincipalID {
					return errors.New("wrong authenticated account scope")
				}
				return nil
			}); err != nil {
			t.Fatalf("new owner missing %s: %v", permission, err)
		}
	}

	var owner string
	var definer, publicExec, identityExec bool
	var config []string
	if err := f.owner.QueryRow(ctx, `SELECT pg_get_userbyid(proowner),prosecdef,proconfig,
		EXISTS(SELECT 1 FROM aclexplode(COALESCE(proacl,acldefault('f',proowner))) a
		 WHERE a.grantee=0 AND a.privilege_type='EXECUTE'),
		has_function_privilege('commerce_identity','identity.create_initial_store(bytea,text,bytea,text,text,text,text)'::regprocedure,'EXECUTE')
		FROM pg_proc WHERE oid='identity.create_initial_store(bytea,text,bytea,text,text,text,text)'::regprocedure`).
		Scan(&owner, &definer, &config, &publicExec, &identityExec); err != nil {
		t.Fatal(err)
	}
	if owner != "commerce_identity_writer" || !definer || publicExec || !identityExec ||
		!slices.Equal(config, []string{"search_path=pg_catalog"}) {
		t.Fatal("initial-store function authority changed")
	}

	if _, err := f.owner.Exec(ctx, `DELETE FROM identity.store_grants
		WHERE tenant_id=$1::uuid AND store_id=$2::uuid AND principal_id=$3::uuid
		AND permission IN ('integration:read','integration:manage','integration:execute')`, store.TenantID, store.StoreID, session.PrincipalID); err != nil {
		t.Fatal(err)
	}
	for _, permission := range []string{"integration:read", "integration:manage", "integration:execute"} {
		if err := platform.WithScope(ctx, f.runtime, session.Token, store.StoreID, permission,
			func(pgx.Tx, platform.Scope) error { return nil }); !errors.Is(err, platform.ErrForbidden) {
			t.Fatalf("revoked %s remained usable: %v", permission, err)
		}
	}
	replay, err := s.CreateInitialStore(ctx, session.Token, key, request)
	if err != nil || replay != store {
		t.Fatalf("idempotent replay after revocation: store=%+v err=%v", replay, err)
	}
	if err := f.owner.QueryRow(ctx, `SELECT ARRAY(SELECT permission FROM identity.store_grants
		WHERE tenant_id=$1::uuid AND store_id=$2::uuid AND principal_id=$3::uuid
		AND permission LIKE 'integration:%' ORDER BY permission)`, store.TenantID, store.StoreID, session.PrincipalID).Scan(&grants); err != nil {
		t.Fatal(err)
	}
	if len(grants) != 0 {
		t.Fatalf("replay restored revoked grants: %v", grants)
	}
}
