package foundation_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"livecommerce/internal/identity"
	"livecommerce/internal/oidclogin"
)

func TestIdentityFunctionsAreTheOnlyLoginAuthority(t *testing.T) {
	s, _, pool := identityFixture(t)
	ctx := context.Background()
	f := fixture(t)
	_ = identityLogin(t, s) // The fixed function surface remains usable.

	for _, query := range []string{
		`SELECT * FROM identity.login_flows`,
		`INSERT INTO identity.login_flows(state_hash,binding_hash,provider_key,nonce,verifier,expires_at) VALUES(decode(repeat('00',32),'hex'),decode(repeat('11',32),'hex'),'provider','nonce','verifier',clock_timestamp())`,
		`UPDATE identity.sessions SET revoked_at=clock_timestamp() WHERE false`,
		`DELETE FROM identity.session_events WHERE false`,
	} {
		if _, err := pool.Exec(ctx, query); sqlState(err) != "42501" {
			t.Fatalf("identity login retained direct table authority for %q: %v", query, err)
		}
	}
	if _, err := f.runtime.Exec(ctx, `SELECT identity.revoke_merchant_session(decode(repeat('00',32),'hex'))`); sqlState(err) != "42501" {
		t.Fatalf("runtime executed identity authority function: %v", err)
	}

	functions := []string{"start_login_flow", "consume_login_flow", "issue_merchant_session", "create_initial_store", "revoke_merchant_session"}
	var count int
	var securityDefiner, safeSearchPath, identityExecute, runtimeDenied, publicDenied bool
	err := f.owner.QueryRow(ctx, `
		SELECT count(*),bool_and(p.prosecdef),
		       bool_and(coalesce(p.proconfig,ARRAY[]::text[]) @> ARRAY['search_path=pg_catalog']),
		       bool_and(has_function_privilege('commerce_identity',p.oid,'EXECUTE')),
		       bool_and(NOT has_function_privilege('commerce_runtime',p.oid,'EXECUTE')),
		       bool_and(NOT EXISTS (
		           SELECT 1 FROM aclexplode(coalesce(p.proacl,acldefault('f',p.proowner))) acl
		           WHERE acl.grantee=0 AND acl.privilege_type='EXECUTE'
		       ))
		FROM pg_proc p JOIN pg_namespace n ON n.oid=p.pronamespace
		WHERE n.nspname='identity' AND p.proname=ANY($1)`, functions).
		Scan(&count, &securityDefiner, &safeSearchPath, &identityExecute, &runtimeDenied, &publicDenied)
	if err != nil {
		t.Fatal(err)
	}
	if count != len(functions) || !securityDefiner || !safeSearchPath || !identityExecute || !runtimeDenied || !publicDenied {
		t.Fatalf("unsafe identity function surface: count=%d definer=%t search_path=%t identity_execute=%t runtime_denied=%t public_denied=%t",
			count, securityDefiner, safeSearchPath, identityExecute, runtimeDenied, publicDenied)
	}
}

func TestIdentityConcurrentFlowsShareOneLiteralIdentity(t *testing.T) {
	s, p, _ := identityFixture(t)
	ctx := context.Background()
	f := fixture(t)
	flows := make([]identity.Flow, 2)
	for index := range flows {
		flow, err := s.Start(ctx)
		if err != nil {
			t.Fatal(err)
		}
		flows[index] = flow
	}

	type result struct {
		session identity.Session
		err     error
	}
	results := make(chan result, len(flows))
	var wait sync.WaitGroup
	for _, flow := range flows {
		wait.Go(func() {
			session, err := s.Complete(ctx, flow.State, flow.Binding, "code")
			results <- result{session: session, err: err}
		})
	}
	wait.Wait()
	close(results)
	var sessions []identity.Session
	for result := range results {
		if result.err != nil {
			t.Fatal(result.err)
		}
		sessions = append(sessions, result.session)
	}
	if len(sessions) != 2 || sessions[0].PrincipalID != sessions[1].PrincipalID || sessions[0].Token == sessions[1].Token {
		t.Fatal("concurrent identity mapping did not share one principal with distinct opaque sessions")
	}

	var identities, principals, storedSessions, issuedEvents int
	if err := f.owner.QueryRow(ctx, `
		SELECT (SELECT count(*) FROM identity.external_identities WHERE issuer='https://idp.example' AND subject=$1),
		       (SELECT count(DISTINCT principal_id) FROM identity.external_identities WHERE issuer='https://idp.example' AND subject=$1),
		       (SELECT count(*) FROM identity.sessions WHERE principal_id=$2::uuid),
		       (SELECT count(*) FROM identity.session_events WHERE principal_id=$2::uuid AND action='session.issued')`,
		p.subject, sessions[0].PrincipalID).Scan(&identities, &principals, &storedSessions, &issuedEvents); err != nil {
		t.Fatal(err)
	}
	if identities != 1 || principals != 1 || storedSessions != 2 || issuedEvents != 2 {
		t.Fatalf("identity rows identities=%d principals=%d sessions=%d events=%d", identities, principals, storedSessions, issuedEvents)
	}
}

type issuerOverrideProvider struct {
	delegate *identityProvider
	issuer   string
}

func (provider *issuerOverrideProvider) AuthorizationURL(state, nonce, verifier string) (string, error) {
	return provider.delegate.AuthorizationURL(state, nonce, verifier)
}

func (provider *issuerOverrideProvider) Exchange(ctx context.Context, code, nonce, verifier string) (oidclogin.Identity, error) {
	verified, err := provider.delegate.Exchange(ctx, code, nonce, verifier)
	verified.Issuer = provider.issuer
	return verified, err
}

func TestIdentitySameSubjectFromDifferentIssuerDoesNotMerge(t *testing.T) {
	base, provider, pool := identityFixture(t)
	ctx := context.Background()
	f := fixture(t)
	first := identityLogin(t, base)
	secondService, err := identity.New(pool, &issuerOverrideProvider{delegate: provider, issuer: "https://second-idp.example"}, identity.Policy{
		ProviderKey: "second-isolated-provider-v1", SessionTTL: time.Hour, OnboardingEnabled: true, Currencies: []string{"TWD", "USD"},
	})
	if err != nil {
		t.Fatal(err)
	}
	second := identityLogin(t, secondService)
	if first.PrincipalID == second.PrincipalID {
		t.Fatal("same subject from different issuers merged")
	}
	var identities, principals, issuers int
	if err := f.owner.QueryRow(ctx, `SELECT count(*),count(DISTINCT principal_id),count(DISTINCT issuer) FROM identity.external_identities WHERE subject=$1`, provider.subject).
		Scan(&identities, &principals, &issuers); err != nil {
		t.Fatal(err)
	}
	if identities != 2 || principals != 2 || issuers != 2 {
		t.Fatalf("literal identity rows identities=%d principals=%d issuers=%d", identities, principals, issuers)
	}
}

func TestIdentitySessionIssueAuditFailureRollsBackButConsumesFlow(t *testing.T) {
	s, provider, _ := identityFixture(t)
	ctx := context.Background()
	f := fixture(t)
	name := "identity_session_fail_" + strings.ReplaceAll(randomUUID(), "-", "")
	function := pgx.Identifier{"identity", name}.Sanitize()
	trigger := pgx.Identifier{name}.Sanitize()
	_, err := f.owner.Exec(ctx, `CREATE FUNCTION `+function+`() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF NEW.action='session.issued' THEN RAISE EXCEPTION 'synthetic session audit failure'; END IF; RETURN NEW; END $$; CREATE TRIGGER `+trigger+` BEFORE INSERT ON identity.session_events FOR EACH ROW EXECUTE FUNCTION `+function+`()`)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = f.owner.Exec(context.Background(), `DROP TRIGGER IF EXISTS `+trigger+` ON identity.session_events; DROP FUNCTION IF EXISTS `+function+`()`)
	})

	var principalsBefore, identitiesBefore, sessionsBefore, eventsBefore int
	if err = f.owner.QueryRow(ctx, `SELECT (SELECT count(*) FROM identity.principals),(SELECT count(*) FROM identity.external_identities),(SELECT count(*) FROM identity.sessions),(SELECT count(*) FROM identity.session_events)`).
		Scan(&principalsBefore, &identitiesBefore, &sessionsBefore, &eventsBefore); err != nil {
		t.Fatal(err)
	}
	flow, err := s.Start(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.Complete(ctx, flow.State, flow.Binding, "code"); !errors.Is(err, identity.ErrUnavailable) {
		t.Fatalf("session audit fault: %v", err)
	}
	var principalsAfter, identitiesAfter, sessionsAfter, eventsAfter int
	if err = f.owner.QueryRow(ctx, `SELECT (SELECT count(*) FROM identity.principals),(SELECT count(*) FROM identity.external_identities),(SELECT count(*) FROM identity.sessions),(SELECT count(*) FROM identity.session_events)`).
		Scan(&principalsAfter, &identitiesAfter, &sessionsAfter, &eventsAfter); err != nil {
		t.Fatal(err)
	}
	if principalsAfter != principalsBefore || identitiesAfter != identitiesBefore || sessionsAfter != sessionsBefore || eventsAfter != eventsBefore {
		t.Fatalf("partial identity state after audit failure: before=%d/%d/%d/%d after=%d/%d/%d/%d",
			principalsBefore, identitiesBefore, sessionsBefore, eventsBefore, principalsAfter, identitiesAfter, sessionsAfter, eventsAfter)
	}
	if _, err = s.Complete(ctx, flow.State, flow.Binding, "code"); !errors.Is(err, identity.ErrUnauthorized) {
		t.Fatalf("consumed flow retried after issue rollback: %v", err)
	}
	if provider.exchanges.Load() != 1 {
		t.Fatalf("consumed flow reached provider %d times", provider.exchanges.Load())
	}
}

func TestIdentityLogoutAndInitialStoreSerialize(t *testing.T) {
	s, _, _ := identityFixture(t)
	ctx := context.Background()
	f := fixture(t)
	session := identityLogin(t, s)
	request := firstStoreRequest()
	request.TenantName = "Race merchant " + randomUUID()
	key := "race-store-" + randomUUID()
	start := make(chan struct{})
	var store identity.Store
	var createErr, logoutErr error
	var wait sync.WaitGroup
	wait.Go(func() {
		<-start
		store, createErr = s.CreateInitialStore(ctx, session.Token, key, request)
	})
	wait.Go(func() {
		<-start
		logoutErr = s.Logout(ctx, session.Token)
	})
	close(start)
	wait.Wait()
	if logoutErr != nil {
		t.Fatalf("logout race: %v", logoutErr)
	}
	created := createErr == nil
	if !created && !errors.Is(createErr, identity.ErrUnauthorized) {
		t.Fatalf("initial store race: %v", createErr)
	}
	if created && (store.TenantID == "" || store.StoreID == "" || store.WarehouseID == "") {
		t.Fatalf("empty committed store: %+v", store)
	}

	var receipts, memberships, grants, warehouses, audits, revoked, revocations int
	var grantList string
	if err := f.owner.QueryRow(ctx, `
		SELECT (SELECT count(*) FROM identity.initial_stores WHERE principal_id=$1::uuid),
		       (SELECT count(*) FROM identity.memberships WHERE principal_id=$1::uuid),
		       (SELECT count(*) FROM identity.store_grants WHERE principal_id=$1::uuid),
		       (SELECT count(*) FROM inventory.warehouses w JOIN identity.initial_stores i ON i.warehouse_id=w.id WHERE i.principal_id=$1::uuid),
		       (SELECT count(*) FROM ops.audit_events WHERE principal_id=$1::uuid AND action='merchant.store_created'),
		       (SELECT count(*) FROM identity.sessions WHERE token_hash=$2 AND revoked_at IS NOT NULL),
		       (SELECT count(*) FROM identity.session_events WHERE principal_id=$1::uuid AND action='session.revoked'),
		       coalesce((SELECT string_agg(permission, ',' ORDER BY permission) FROM identity.store_grants WHERE principal_id=$1::uuid),'')`,
		session.PrincipalID, tokenHash(session.Token)).Scan(&receipts, &memberships, &grants, &warehouses, &audits, &revoked, &revocations, &grantList); err != nil {
		t.Fatal(err)
	}
	if revoked != 1 || revocations != 1 {
		t.Fatalf("logout did not commit exactly once: revoked=%d events=%d", revoked, revocations)
	}
	if created {
		// The exact owner permission set, not a count: the old literal 8 went stale
		// when later migrations widened create_initial_store (see initialStoreGrants; 0065: 19 grants).
		if receipts != 1 || memberships != 1 || grantList != initialStoreGrants || warehouses != 1 || audits != 1 {
			t.Fatalf("partial committed store: receipts=%d memberships=%d grants=%d [%s] warehouses=%d audits=%d", receipts, memberships, grants, grantList, warehouses, audits)
		}
	} else if receipts != 0 || memberships != 0 || grants != 0 || warehouses != 0 || audits != 0 {
		t.Fatalf("unauthorized store left state: receipts=%d memberships=%d grants=%d warehouses=%d audits=%d", receipts, memberships, grants, warehouses, audits)
	}
}
