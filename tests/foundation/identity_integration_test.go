package foundation_test

import (
	"context"
	"errors"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"livecommerce/internal/identity"
	"livecommerce/internal/oidclogin"
	"livecommerce/internal/platform"
)

// This provider tests database state transitions, not OIDC verification. Signed
// protocol/PKCE/nonce negative tests live in internal/oidclogin instead.
type identityProvider struct {
	subject   string
	fail      bool
	exchanges atomic.Int32
}

func (p *identityProvider) AuthorizationURL(state, nonce, verifier string) (string, error) {
	return "https://idp.example/authorize?state=" + state, nil
}
func (p *identityProvider) Exchange(ctx context.Context, code, nonce, verifier string) (oidclogin.Identity, error) {
	p.exchanges.Add(1)
	if p.fail {
		return oidclogin.Identity{}, errors.New("synthetic IdP denial")
	}
	return oidclogin.Identity{Issuer: "https://idp.example", Subject: p.subject}, nil
}

func identityFixture(t *testing.T) (*identity.Service, *identityProvider, *pgxpool.Pool) {
	t.Helper()
	f := fixture(t)
	role := "identity_" + strings.ReplaceAll(randomUUID(), "-", "")
	password := randomToken()
	_, err := f.owner.Exec(context.Background(), `CREATE ROLE `+pgx.Identifier{role}.Sanitize()+` LOGIN NOSUPERUSER NOBYPASSRLS NOCREATEDB NOCREATEROLE IN ROLE commerce_identity PASSWORD '`+password+`'`)
	if err != nil {
		t.Fatal(err)
	}
	// The shared fixture outlives this test. Tests hand this login objects
	// (TestIdentityPoolRejectsPrivilegeAndObjectOwnership creates identity.<role>()
	// with default PUBLIC EXECUTE); leaving them makes every later strict pool
	// gate (e.g. platform.OpenStripeIngressPool) correctly reject correct logins.
	// Registered before pool.Close, so it runs after the pool is closed.
	t.Cleanup(func() {
		for _, q := range []string{`DROP OWNED BY `, `DROP ROLE `} {
			if _, err := f.owner.Exec(context.Background(), q+pgx.Identifier{role}.Sanitize()); err != nil {
				t.Errorf("identity fixture cleanup %s%s: %v", q, role, err)
			}
		}
	})
	u, err := url.Parse(f.databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	u.User = url.UserPassword(role, password)
	pool, err := platform.OpenIdentityPool(context.Background(), u.String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	p := &identityProvider{subject: randomUUID()}
	s, err := identity.New(pool, p, identity.Policy{ProviderKey: "isolated-test-provider-v1", SessionTTL: time.Hour, OnboardingEnabled: true, Currencies: []string{"TWD", "USD"}})
	if err != nil {
		t.Fatal(err)
	}
	return s, p, pool
}

func identityLogin(t *testing.T, s *identity.Service) identity.Session {
	t.Helper()
	flow, err := s.Start(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	session, err := s.Complete(context.Background(), flow.State, flow.Binding, "test-code")
	if err != nil {
		t.Fatal(err)
	}
	return session
}

func firstStoreRequest() identity.StoreRequest {
	return identity.StoreRequest{TenantName: "Test merchant", StoreName: "測試商店", WarehouseName: "Supplier warehouse", Currency: "TWD"}
}

func TestIdentityFlowBindingExpiryReplayAndMapping(t *testing.T) {
	s, p, _ := identityFixture(t)
	ctx := context.Background()
	f := fixture(t)
	flow, err := s.Start(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.Complete(ctx, flow.State, randomToken(), "code"); !errors.Is(err, identity.ErrUnauthorized) {
		t.Fatalf("wrong binding: %v", err)
	}
	if p.exchanges.Load() != 0 {
		t.Fatal("bad binding reached IdP")
	}
	session, err := s.Complete(ctx, flow.State, flow.Binding, "code")
	if err != nil {
		t.Fatal(err)
	}
	if session.Token == flow.State || session.Token == flow.Binding {
		t.Fatal("tokens are not independent")
	}
	if _, err = s.Complete(ctx, flow.State, flow.Binding, "code"); !errors.Is(err, identity.ErrUnauthorized) {
		t.Fatalf("replay: %v", err)
	}
	if p.exchanges.Load() != 1 {
		t.Fatal("replay reached IdP")
	}
	again := identityLogin(t, s)
	if again.PrincipalID != session.PrincipalID || again.Token == session.Token {
		t.Fatal("identity mapping/session rotation")
	}
	other, _, _ := identityFixture(t)
	if identityLogin(t, other).PrincipalID == session.PrincipalID {
		t.Fatal("different subject merged")
	}
	flow, err = s.Start(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.owner.Exec(ctx, `UPDATE identity.login_flows SET expires_at=clock_timestamp()-interval '1 second' WHERE state_hash=$1`, tokenHash(flow.State)); err != nil {
		t.Fatal(err)
	}
	if _, err = s.Complete(ctx, flow.State, flow.Binding, "code"); !errors.Is(err, identity.ErrUnauthorized) {
		t.Fatalf("expired flow: %v", err)
	}
	var count int
	if err = f.owner.QueryRow(ctx, `SELECT count(*) FROM identity.session_events WHERE principal_id=$1::uuid AND action='session.issued'`, session.PrincipalID).Scan(&count); err != nil || count != 2 {
		t.Fatalf("session audit: %d %v", count, err)
	}
	if _, err = f.owner.Exec(ctx, `UPDATE identity.principals SET active=false WHERE id=$1::uuid`, session.PrincipalID); err != nil {
		t.Fatal(err)
	}
	flow, err = s.Start(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.Complete(ctx, flow.State, flow.Binding, "code"); !errors.Is(err, identity.ErrUnauthorized) {
		t.Fatalf("inactive principal: %v", err)
	}
}

func TestIdentityConcurrentConsumeAndProviderFailure(t *testing.T) {
	s, p, _ := identityFixture(t)
	ctx := context.Background()
	flow, err := s.Start(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	results := make(chan error, 2)
	for range 2 {
		wg.Go(func() { _, e := s.Complete(ctx, flow.State, flow.Binding, "code"); results <- e })
	}
	wg.Wait()
	close(results)
	var ok, denied int
	for err := range results {
		if err == nil {
			ok++
		} else if errors.Is(err, identity.ErrUnauthorized) {
			denied++
		} else {
			t.Fatal(err)
		}
	}
	if ok != 1 || denied != 1 || p.exchanges.Load() != 1 {
		t.Fatalf("consume: ok=%d denied=%d exchanges=%d", ok, denied, p.exchanges.Load())
	}
	p.fail = true
	flow, err = s.Start(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.Complete(ctx, flow.State, flow.Binding, "denied"); !errors.Is(err, identity.ErrUnauthorized) {
		t.Fatal(err)
	}
	p.fail = false
	if _, err = s.Complete(ctx, flow.State, flow.Binding, "denied"); !errors.Is(err, identity.ErrUnauthorized) {
		t.Fatalf("failed exchange reused: %v", err)
	}
}

func TestIdentityInitialStoreAtomicIdempotentAndScoped(t *testing.T) {
	s, _, _ := identityFixture(t)
	ctx := context.Background()
	f := fixture(t)
	session := identityLogin(t, s)
	request := firstStoreRequest()
	key := "first-store-" + randomUUID()
	var wg sync.WaitGroup
	out := make(chan identity.Store, 2)
	fail := make(chan error, 2)
	for range 2 {
		wg.Go(func() { store, e := s.CreateInitialStore(ctx, session.Token, key, request); out <- store; fail <- e })
	}
	wg.Wait()
	for range 2 {
		if err := <-fail; err != nil {
			t.Fatal(err)
		}
	}
	a, b := <-out, <-out
	if a != b || a.StoreID == "" {
		t.Fatalf("idempotency: %+v %+v", a, b)
	}
	if _, err := s.CreateInitialStore(ctx, session.Token, key+"new", request); !errors.Is(err, identity.ErrConflict) {
		t.Fatalf("changed key: %v", err)
	}
	request.StoreName = "Changed"
	if _, err := s.CreateInitialStore(ctx, session.Token, key, request); !errors.Is(err, identity.ErrConflict) {
		t.Fatalf("changed body: %v", err)
	}
	var grants []string
	var warehouses, audits int
	if err := f.owner.QueryRow(ctx, `SELECT ARRAY(SELECT permission FROM identity.store_grants WHERE tenant_id=$1::uuid ORDER BY permission),(SELECT count(*) FROM inventory.warehouses WHERE tenant_id=$1::uuid),(SELECT count(*) FROM ops.audit_events WHERE tenant_id=$1::uuid AND action='merchant.store_created')`, a.TenantID).Scan(&grants, &warehouses, &audits); err != nil {
		t.Fatal(err)
	}
	if strings.Join(grants, ",") != initialStoreGrants || warehouses != 1 || audits != 1 {
		t.Fatalf("bootstrap grants=%v warehouses=%d audits=%d", grants, warehouses, audits)
	}
	if err := platform.WithScope(ctx, f.runtime, session.Token, a.StoreID, "catalog:write", func(tx pgx.Tx, scope platform.Scope) error {
		if scope.PrincipalID != session.PrincipalID || scope.TenantID != a.TenantID {
			t.Fatal("bad resolved scope")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := platform.WithScope(ctx, f.runtime, session.Token, f.storeB, "store:read", func(pgx.Tx, platform.Scope) error { return nil }); !errors.Is(err, platform.ErrScopeNotFound) {
		t.Fatalf("cross tenant: %v", err)
	}
	for range 2 {
		if err := s.Logout(ctx, session.Token); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.CreateInitialStore(ctx, session.Token, key, firstStoreRequest()); !errors.Is(err, identity.ErrUnauthorized) {
		t.Fatalf("revoked onboarding: %v", err)
	}
	if err := platform.WithScope(ctx, f.runtime, session.Token, a.StoreID, "store:read", func(pgx.Tx, platform.Scope) error { return nil }); !errors.Is(err, platform.ErrUnauthorized) {
		t.Fatalf("revoked domain: %v", err)
	}
	if err := f.owner.QueryRow(ctx, `SELECT count(*) FROM identity.session_events WHERE principal_id=$1::uuid AND action='session.revoked'`, session.PrincipalID).Scan(&audits); err != nil || audits != 1 {
		t.Fatalf("logout audit: %d %v", audits, err)
	}
}

func TestIdentityRollbackWhenLastAuditInsertFails(t *testing.T) {
	s, _, _ := identityFixture(t)
	ctx := context.Background()
	f := fixture(t)
	session := identityLogin(t, s)
	// A temporary failing trigger in this explicitly disposable test database
	// proves the LAST statement failure rolls back all earlier bootstrap writes.
	name := "identity_fail_" + strings.ReplaceAll(randomUUID(), "-", "")
	qname := pgx.Identifier{"ops", name}.Sanitize()
	trigger := pgx.Identifier{name}.Sanitize()
	_, err := f.owner.Exec(ctx, `CREATE FUNCTION `+qname+`() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF NEW.principal_id='`+session.PrincipalID+`'::uuid THEN RAISE EXCEPTION 'synthetic final audit failure'; END IF; RETURN NEW; END $$; CREATE TRIGGER `+trigger+` BEFORE INSERT ON ops.audit_events FOR EACH ROW EXECUTE FUNCTION `+qname+`()`)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = f.owner.Exec(context.Background(), `DROP TRIGGER IF EXISTS `+trigger+` ON ops.audit_events; DROP FUNCTION IF EXISTS `+qname+`()`)
	})
	request := firstStoreRequest()
	request.TenantName = "Rollback-" + randomUUID()
	if _, err = s.CreateInitialStore(ctx, session.Token, "rollback-first-store", request); !errors.Is(err, identity.ErrUnavailable) {
		t.Fatalf("fault injection: %v", err)
	}
	var count int
	if err = f.owner.QueryRow(ctx, `SELECT (SELECT count(*) FROM control.tenants WHERE name=$1)+(SELECT count(*) FROM identity.memberships WHERE principal_id=$2::uuid)+(SELECT count(*) FROM identity.initial_stores WHERE principal_id=$2::uuid)`, request.TenantName, session.PrincipalID).Scan(&count); err != nil || count != 0 {
		t.Fatalf("partial state count=%d error=%v", count, err)
	}
	if _, err = f.owner.Exec(ctx, `DROP TRIGGER `+trigger+` ON ops.audit_events; DROP FUNCTION `+qname+`() `); err != nil {
		t.Fatal(err)
	}
	if _, err = s.CreateInitialStore(ctx, session.Token, "rollback-first-store", request); err != nil {
		t.Fatalf("retry after rollback: %v", err)
	}
}

func TestIdentityDatabaseAuthoritySeparation(t *testing.T) {
	_, _, pool := identityFixture(t)
	ctx := context.Background()
	f := fixture(t)
	for _, query := range []string{`SELECT * FROM identity.external_identities`, `SELECT * FROM identity.login_flows`, `INSERT INTO identity.principals(id) VALUES(gen_random_uuid())`, `UPDATE identity.sessions SET revoked_at=now()`, `INSERT INTO identity.initial_stores DEFAULT VALUES`} {
		if _, err := f.runtime.Exec(ctx, query); sqlState(err) != "42501" {
			t.Fatalf("runtime unexpectedly allowed %s: %v", query, err)
		}
	}
	for _, query := range []string{`CREATE TABLE identity.denied(id int)`, `DELETE FROM identity.sessions`, `UPDATE identity.memberships SET active=false`, `DELETE FROM identity.session_events`, `SELECT * FROM catalog.products`} {
		if _, err := pool.Exec(ctx, query); sqlState(err) != "42501" {
			t.Fatalf("identity unexpectedly allowed %s: %v", query, err)
		}
	}
	if unexpected, err := platform.OpenPool(ctx, pool.Config().ConnString()); err == nil {
		unexpected.Close()
		t.Fatal("business pool accepted identity login")
	}
	if unexpected, err := platform.OpenIdentityPool(ctx, f.runtime.Config().ConnString()); err == nil {
		unexpected.Close()
		t.Fatal("identity pool accepted business login")
	}
	if unexpected, err := platform.OpenIdentityPool(ctx, f.databaseURL); err == nil {
		unexpected.Close()
		t.Fatal("identity pool accepted owner")
	}
	var role string
	if err := pool.QueryRow(ctx, `SELECT current_user`).Scan(&role); err != nil {
		t.Fatal(err)
	}
	if _, err := f.owner.Exec(ctx, `GRANT commerce_runtime TO `+pgx.Identifier{role}.Sanitize()); err != nil {
		t.Fatal(err)
	}
	if unexpected, err := platform.OpenIdentityPool(ctx, pool.Config().ConnString()); err == nil {
		unexpected.Close()
		t.Fatal("cross-member role accepted")
	}
}

func TestIdentityPolicyValidationAndExpiredAudience(t *testing.T) {
	s, p, pool := identityFixture(t)
	ctx := context.Background()
	f := fixture(t)
	if _, err := identity.New(pool, p, identity.Policy{ProviderKey: "test", SessionTTL: time.Hour, OnboardingEnabled: true}); !errors.Is(err, identity.ErrInvalid) {
		t.Fatal("enabled without currency policy")
	}
	disabled, err := identity.New(pool, p, identity.Policy{ProviderKey: "test", SessionTTL: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = disabled.CreateInitialStore(ctx, randomToken(), "first-store-key", firstStoreRequest()); !errors.Is(err, identity.ErrDisabled) {
		t.Fatal("default onboarding enabled")
	}
	for _, audience := range []string{"buyer", "support", "merchant"} {
		session := identityLogin(t, s)
		if _, err = f.owner.Exec(ctx, `UPDATE identity.sessions SET audience=$2,expires_at=CASE WHEN $2='merchant' THEN clock_timestamp()-interval '1 second' ELSE expires_at END WHERE token_hash=$1`, tokenHash(session.Token), audience); err != nil {
			t.Fatal(err)
		}
		if _, err = s.CreateInitialStore(ctx, session.Token, "invalid-session-key", firstStoreRequest()); !errors.Is(err, identity.ErrUnauthorized) {
			t.Fatalf("audience/expiry %s: %v", audience, err)
		}
	}
	session := identityLogin(t, s)
	for _, bad := range []identity.StoreRequest{{TenantName: "ok", StoreName: "ok", WarehouseName: "ok", Currency: "EUR"}, {TenantName: "ok", StoreName: "bad\x00", WarehouseName: "ok", Currency: "TWD"}, {TenantName: "", StoreName: "ok", WarehouseName: "ok", Currency: "TWD"}} {
		if _, err = s.CreateInitialStore(ctx, session.Token, "invalid-input-key", bad); !errors.Is(err, identity.ErrInvalid) {
			t.Fatalf("invalid request: %v", err)
		}
	}
}

func TestIdentityPoolRejectsPrivilegeAndObjectOwnership(t *testing.T) {
	for _, kind := range []string{"CREATEROLE", "CREATEDB", "REPLICATION", "relation", "function", "writer_membership"} {
		t.Run(kind, func(t *testing.T) {
			_, _, pool := identityFixture(t)
			f, ctx := fixture(t), context.Background()
			var role string
			if err := pool.QueryRow(ctx, `SELECT current_user`).Scan(&role); err != nil {
				t.Fatal(err)
			}
			roleSQL := pgx.Identifier{role}.Sanitize()
			var err error
			switch kind {
			case "relation", "function":
				// Own an object without owning its schema; neither login may pass.
				object := pgx.Identifier{"identity", role}.Sanitize()
				if _, err = f.owner.Exec(ctx, `GRANT CREATE ON SCHEMA identity TO `+roleSQL); err != nil {
					t.Fatal(err)
				}
				if kind == "relation" {
					_, err = f.owner.Exec(ctx, `CREATE TABLE `+object+`(id int); ALTER TABLE `+object+` OWNER TO `+roleSQL)
				} else {
					_, err = f.owner.Exec(ctx, `CREATE FUNCTION `+object+`() RETURNS int LANGUAGE sql AS 'SELECT 1'; ALTER FUNCTION `+object+`() OWNER TO `+roleSQL)
				}
				if err != nil {
					t.Fatal(err)
				}
				_, err = f.owner.Exec(ctx, `REVOKE CREATE ON SCHEMA identity FROM `+roleSQL)
			case "writer_membership":
				_, err = f.owner.Exec(ctx, `GRANT commerce_identity_writer TO `+roleSQL)
			default:
				_, err = f.owner.Exec(ctx, `ALTER ROLE `+roleSQL+` `+kind)
			}
			if err != nil {
				t.Fatal(err)
			}
			if unexpected, err := platform.OpenIdentityPool(ctx, pool.Config().ConnString()); err == nil {
				unexpected.Close()
				t.Fatalf("identity pool accepted %s", kind)
			}
		})
	}
}

// initialStoreGrants is the exact, sorted permission set that
// identity.create_initial_store (latest definition: migrations/0079_platform_billing.sql)
// grants the creating principal. Every test asserting the initial owner's
// grants compares against this one list so a migration that changes the set
// fails loudly in one place instead of leaving stale per-test counts.
const initialStoreGrants = "audit:read,audit:write,billing:manage,catalog:read,catalog:write,customers:privacy,customers:read,fulfillment:write,integration:execute,integration:manage,integration:read,inventory:read,inventory:reserve,inventory:write,live:manage,live:read,orders:export,orders:read,payments:refund,pricing:read,pricing:write,store:read"
