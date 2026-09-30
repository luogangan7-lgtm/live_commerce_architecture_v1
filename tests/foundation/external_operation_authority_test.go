package foundation_test

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"
	"livecommerce/internal/platform"
)

// These tests intentionally bypass the Go integration service. Fixed SQL and
// ordinary login grants must enforce leases even if a caller sends raw SQL.
func t06AuthorityLogin(t *testing.T, memberships string) (string, *pgxpool.Pool) {
	t.Helper()
	f := fixture(t)
	name := "t06_" + strings.ReplaceAll(randomUUID(), "-", "")
	password := randomToken()
	_, err := f.owner.Exec(context.Background(), fmt.Sprintf(`CREATE ROLE %s LOGIN NOSUPERUSER NOBYPASSRLS NOCREATEDB NOCREATEROLE NOREPLICATION IN ROLE %s PASSWORD '%s'`, pgx.Identifier{name}.Sanitize(), memberships, password))
	if err != nil {
		t.Fatal(err)
	}
	u, err := url.Parse(f.databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	u.User = url.UserPassword(name, password)
	p, err := pgxpool.New(context.Background(), u.String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		p.Close()
		if _, err := f.owner.Exec(context.Background(), "DROP ROLE "+pgx.Identifier{name}.Sanitize()); err != nil {
			t.Error(err)
		}
	})
	return u.String(), p
}

func t06AuthorityOperation(t *testing.T) (string, string) {
	t.Helper()
	f := fixture(t)
	ctx := context.Background()
	tenant, store, principal, binding, operation := randomUUID(), randomUUID(), randomUUID(), randomUUID(), randomUUID()
	tx, err := f.owner.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	for _, statement := range []struct {
		q    string
		args []any
	}{
		{`INSERT INTO control.tenants(id,name) VALUES($1,'T06 synthetic authority')`, []any{tenant}},
		{`INSERT INTO control.stores(tenant_id,id,name,currency) VALUES($1,$2,'T06 synthetic store','TWD')`, []any{tenant, store}},
		{`INSERT INTO identity.principals(id) VALUES($1)`, []any{principal}},
		{`INSERT INTO identity.memberships(tenant_id,principal_id) VALUES($1,$2)`, []any{tenant, principal}},
		{`INSERT INTO integration.bindings(id,tenant_id,store_id,principal_id,provider,external_asset_id) VALUES($1,$2,$3,$4,'mock','synthetic-asset')`, []any{binding, tenant, store, principal}},
		{`INSERT INTO integration.operations(id,tenant_id,store_id,principal_id,binding_id,binding_version,provider,external_asset_id,purpose,action,semantic_key,request_hash,request,job_id) VALUES($1,$2,$3,$4,$5,1,'mock','synthetic-asset','transactional','mock.authority','authority:test',decode(repeat('01',32),'hex'),'{}',1)`, []any{operation, tenant, store, principal, binding}},
	} {
		if _, err = tx.Exec(ctx, statement.q, statement.args...); err != nil {
			t.Fatal(err)
		}
	}
	if err = tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	return operation, binding
}

type t06AuthorityClaim struct {
	disposition string
	generation  int64
	mode        string
}

func t06AuthorityTake(t *testing.T, p *pgxpool.Pool, id string, token []byte) t06AuthorityClaim {
	t.Helper()
	var c t06AuthorityClaim
	if err := p.QueryRow(context.Background(), `SELECT * FROM integration.claim_operation($1,30,$2)`, id, token).Scan(&c.disposition, &c.generation, &c.mode); err != nil {
		t.Fatal(err)
	}
	return c
}

func t06AuthorityFinish(p *pgxpool.Pool, id string, gen int64, token []byte, state string) error {
	_, err := p.Exec(context.Background(), `SELECT integration.complete_operation($1,$2,$3,$4,'observed','synthetic-ref')`, id, gen, token, state)
	return err
}

func TestT06WorkerAuthorityAndFunctionACL(t *testing.T) {
	f := fixture(t)
	ctx := context.Background()
	dsn, p := t06AuthorityLogin(t, "commerce_worker")
	checked, err := platform.OpenWorkerPool(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	checked.Close()
	if err := platform.ValidateWorkerPool(ctx, p); err != nil {
		t.Fatalf("ordinary worker pool rejected: %v", err)
	}
	for _, unsafe := range []*pgxpool.Pool{nil, f.owner, f.runtime} {
		if err := platform.ValidateWorkerPool(ctx, unsafe); err == nil {
			t.Fatal("unsafe existing worker pool accepted")
		}
	}
	// The validator does not own or close supplied pools on failure.
	if err := f.runtime.Ping(ctx); err != nil {
		t.Fatal("validation closed caller-owned pool")
	}
	if unsafe, err := platform.OpenPool(ctx, dsn); err == nil {
		unsafe.Close()
		t.Fatal("worker admitted as merchant")
	}
	for _, roles := range []string{"commerce_worker,commerce_runtime", "commerce_worker,commerce_buyer_runtime", "commerce_worker,commerce_integration_writer"} {
		mixed, mixedPool := t06AuthorityLogin(t, roles)
		if err := platform.ValidateWorkerPool(ctx, mixedPool); err == nil {
			t.Fatalf("mixed existing worker pool accepted: %s", roles)
		}
		if unsafe, err := platform.OpenWorkerPool(ctx, mixed); err == nil {
			unsafe.Close()
			t.Fatalf("mixed worker accepted: %s", roles)
		}
	}
	if unsafe, err := platform.OpenWorkerPool(ctx, f.databaseURL); err == nil {
		unsafe.Close()
		t.Fatal("owner admitted as worker")
	}
	for _, q := range []string{
		`SELECT * FROM identity.sessions`, `SELECT * FROM buyer.capability_sessions`,
		`UPDATE integration.operations SET generation=generation+1 WHERE false`,
		`UPDATE integration.operations SET request='{}' WHERE false`,
		`UPDATE integration.bindings SET enabled=false WHERE false`,
		`DELETE FROM integration.operation_events WHERE false`,
		`INSERT INTO integration.operation_events(tenant_id,store_id,operation_id,generation,state,mode,reason_code) SELECT tenant_id,store_id,operation_id,generation,state,mode,reason_code FROM integration.operation_events WHERE false`,
		`SELECT * FROM river.river_migration`,
		`INSERT INTO river.river_migration SELECT * FROM river.river_migration WHERE false`,
		`UPDATE river.river_migration SET version=version WHERE false`,
		`DELETE FROM river.river_migration WHERE false`,
	} {
		if _, err := p.Exec(ctx, q); sqlState(err) != "42501" {
			t.Fatalf("worker privilege unexpectedly allowed: %s: %v", q, err)
		}
	}
	if _, err := p.Exec(ctx, `UPDATE river.river_job SET state=state WHERE false`); err != nil {
		t.Fatalf("queue lifecycle denied: %v", err)
	}
	if _, err := f.runtime.Exec(ctx, `SELECT * FROM integration.claim_operation($1,30,$2)`, randomUUID(), randomBytes(32)); sqlState(err) != "42501" {
		t.Fatalf("merchant claim allowed: %v", err)
	}
	var functions int
	var safe bool
	// Enumerate exact signatures, not just a count: an added overload must fail
	// closed, and the shared private guard must never be callable by workers.
	err = f.owner.QueryRow(ctx, `WITH approved(oid,worker_execute,owner,registrar_execute,runtime_execute,checkout_writer_execute,checkout_runtime_execute) AS (VALUES
	 ('integration.claim_operation(uuid,integer,bytea)'::regprocedure::oid,true,'commerce_integration_writer',false,false,false,false),
	 ('integration.complete_operation(uuid,bigint,bytea,text,text,text)'::regprocedure::oid,true,'commerce_integration_writer',false,false,false,false),
	 ('integration.require_payment_query(uuid,bigint,bytea,text)'::regprocedure::oid,false,'commerce_integration_writer',false,false,false,false),
	 ('integration.load_payment_query(uuid,bigint,bytea,text)'::regprocedure::oid,true,'commerce_integration_writer',false,false,false,false),
	 ('integration.record_payment_query(uuid,bigint,bytea,text,jsonb,bigint)'::regprocedure::oid,true,'commerce_integration_writer',false,false,false,false),
	 ('integration.finish_payment_query(uuid,bigint,bytea,text,text,text)'::regprocedure::oid,true,'commerce_integration_writer',false,false,false,false),
	 ('integration.payment_job_queue(bigint)'::regprocedure::oid,false,'commerce_integration_writer',false,false,false,false),
	 ('integration.route_payment_queue_v1()'::regprocedure::oid,false,'commerce_integration_writer',false,false,false,false),
	 ('integration.payment_queue_ready()'::regprocedure::oid,true,'commerce_integration_writer',false,false,false,false),
	 ('integration.guard_payment_job_family()'::regprocedure::oid,false,'commerce_integration_writer',false,false,false,false),
	 ('integration.reject_legacy_family_job()'::regprocedure::oid,false,'commerce_integration_writer',false,false,false,false),
	 ('integration.require_stripe_query(uuid,bigint,bytea,text)'::regprocedure::oid,false,'commerce_integration_writer',false,false,false,false),
	 ('integration.load_stripe_credential(uuid,bigint,bytea,text)'::regprocedure::oid,true,'commerce_integration_writer',false,false,false,false),
	 ('integration.load_stripe_session(uuid,bigint,bytea,text)'::regprocedure::oid,true,'commerce_integration_writer',false,false,false,false),
	 ('integration.load_stripe_signal(uuid,bigint,bytea,text,bigint,uuid)'::regprocedure::oid,true,'commerce_integration_writer',false,false,false,false),
	 ('integration.mark_stripe_create_sent(uuid,bigint,bytea,text,bytea)'::regprocedure::oid,true,'commerce_integration_writer',false,false,false,false),
	 ('integration.note_stripe_expire(uuid,bigint,bytea,text)'::regprocedure::oid,true,'commerce_integration_writer',false,false,false,false),
	 ('integration.record_stripe_observation(uuid,bigint,bytea,text,jsonb,bigint,text)'::regprocedure::oid,true,'commerce_integration_writer',false,false,false,false),
	 ('integration.consume_stripe_signal(uuid,uuid,bigint,bytea,text,text)'::regprocedure::oid,true,'commerce_integration_writer',false,false,false,false),
	 ('integration.finish_stripe_query(uuid,bigint,bytea,text,text)'::regprocedure::oid,true,'commerce_integration_writer',false,false,false,false),
	 ('integration.require_stripe_registrar_scope(uuid,uuid,uuid)'::regprocedure::oid,false,'commerce_payment_registry_writer',false,false,false,false),
	 ('integration.register_stripe_account(uuid,uuid,uuid,uuid,uuid,text,text,text,bytea,bytea)'::regprocedure::oid,false,'commerce_payment_registry_writer',true,false,false,false),
	 ('integration.rotate_stripe_key(uuid,uuid,uuid,uuid,bigint,text,bytea,bytea)'::regprocedure::oid,false,'commerce_payment_registry_writer',true,false,false,false),
	 ('integration.require_stripe_refund(uuid,bigint,bytea,text)'::regprocedure::oid,false,'commerce_integration_writer',false,false,false,false),
	 ('integration.load_stripe_refund(uuid,bigint,bytea,text)'::regprocedure::oid,true,'commerce_integration_writer',false,false,false,false),
	 ('integration.mark_stripe_refund_sent(uuid,bigint,bytea,text,bytea)'::regprocedure::oid,true,'commerce_integration_writer',false,false,false,false),
	 ('integration.record_stripe_refund_observation(uuid,bigint,bytea,text,jsonb,bigint)'::regprocedure::oid,true,'commerce_integration_writer',false,false,false,false),
	 ('integration.record_stripe_charge_observation(uuid,bigint,bytea,text,jsonb,bigint)'::regprocedure::oid,true,'commerce_integration_writer',false,false,false,false),
	 ('integration.finish_stripe_refund(uuid,bigint,bytea,text,text)'::regprocedure::oid,true,'commerce_integration_writer',false,false,false,false),
	 -- meta-claims-intake-v1 (migration 0064 and post-River 0014): reply planning is intake-only, the Page-token
	 -- loader is the dispatcher's only credential read, the registrar has its own role (not the payment registrar).
	 ('integration.claim_reply_plannable(uuid)'::regprocedure::oid,false,'commerce_integration_writer',false,false,false,false),
	 ('integration.plan_claim_reply(uuid,uuid,bytea,text,bigint)'::regprocedure::oid,false,'commerce_integration_writer',false,false,false,false),
	 ('integration.load_meta_page_token(uuid,bigint,bytea)'::regprocedure::oid,true,'commerce_integration_writer',false,false,false,false),
	 ('integration.register_meta_page_token(uuid,uuid,uuid,uuid,text,text,bigint,text,bytea,bytea,text[])'::regprocedure::oid,false,'commerce_integration_writer',false,false,false,false),
	 -- R1 ruling F2 (migration 0066): the Meta registrar's binding definer, same owner/grant shape as the page-token one.
	 ('integration.register_meta_binding(uuid,uuid,uuid,text,text)'::regprocedure::oid,false,'commerce_integration_writer',false,false,false,false),
	 ('integration.guard_claims_intake_job()'::regprocedure::oid,false,'commerce_integration_writer',false,false,false,false),
	 ('integration.guard_external_operation_job_link()'::regprocedure::oid,false,'commerce_integration_writer',false,false,false,false),
	 -- meta-ads-v1 (migration 0074, post-River 0015; unit ads-core): the ads token registrar is hash-authenticated and
	 -- callable by the merchant runtime (runtime_execute), the loader is the dispatcher's only
	 -- ads credential read, and the two River guards have no caller EXECUTE.
	 ('integration.register_meta_ads_token(bytea,uuid,uuid,uuid,bigint)'::regprocedure::oid,false,'commerce_integration_writer',false,true,false,false),
	 ('integration.load_meta_ads_token(uuid,bigint,bytea)'::regprocedure::oid,true,'commerce_integration_writer',false,false,false,false),
	 ('integration.guard_ads_job()'::regprocedure::oid,false,'commerce_integration_writer',false,false,false,false),
	 ('integration.guard_ads_job_link()'::regprocedure::oid,false,'commerce_integration_writer',false,false,false,false),
	 -- taiwan-cvs-logistics-v1 (migrations 0072/0073, unit cvs-core): +8 approved integration functions. Each declares its exact EXECUTE set; every other
	 -- role stays refused by the equality below (the registrar, ingress and buyer roles never gain anything).
	 ('integration.register_ecpay_logistics(bytea,uuid,text,bytea,bigint,text,text,text,text,bytea,bytea,boolean,text)'::regprocedure::oid,false,'commerce_integration_writer',false,true,false,false),
	 ('integration.set_ecpay_logistics_enabled(bytea,uuid,text,bytea,bigint,boolean)'::regprocedure::oid,false,'commerce_integration_writer',false,true,false,false),
	 ('integration.plan_cvs_create(uuid,uuid,uuid,smallint,uuid,uuid,uuid,bigint)'::regprocedure::oid,false,'commerce_integration_writer',false,false,true,false),
	 ('integration.load_cvs_create(uuid,bigint,bytea,text)'::regprocedure::oid,true,'commerce_integration_writer',false,false,false,false),
	 ('integration.finish_cvs_create(uuid,bigint,bytea,text,text,text,text,text,text,text)'::regprocedure::oid,true,'commerce_integration_writer',false,false,false,false),
	 ('integration.load_ecpay_key_for_status(uuid)'::regprocedure::oid,false,'commerce_integration_writer',false,true,false,false),
	 ('integration.load_ecpay_key_for_selection(uuid)'::regprocedure::oid,false,'commerce_integration_writer',false,true,false,true),
	 ('integration.load_ecpay_key_for_merchant(bytea,uuid)'::regprocedure::oid,false,'commerce_integration_writer',false,true,false,false))
	 SELECT count(*),bool_and(a.oid IS NOT NULL AND p.prosecdef AND p.proconfig = ARRAY['search_path=pg_catalog']
	 AND pg_get_userbyid(p.proowner)=a.owner
	 AND has_function_privilege('commerce_worker',p.oid,'EXECUTE')=a.worker_execute
	 AND has_function_privilege('commerce_payment_registrar',p.oid,'EXECUTE')=a.registrar_execute
	 AND NOT has_function_privilege('commerce_stripe_ingress',p.oid,'EXECUTE')
	 AND has_function_privilege('commerce_runtime',p.oid,'EXECUTE')=a.runtime_execute
	 AND NOT has_function_privilege('commerce_buyer_runtime',p.oid,'EXECUTE')
	 AND NOT has_function_privilege('commerce_buyer_issuer',p.oid,'EXECUTE')
	 AND has_function_privilege('commerce_checkout_runtime',p.oid,'EXECUTE')=a.checkout_runtime_execute
	 AND has_function_privilege('commerce_hosted_runtime',p.oid,'EXECUTE')=a.checkout_runtime_execute -- 0025: hosted_runtime inherits commerce_checkout_runtime (INHERIT TRUE), so it never has more than the checkout runtime
	 AND has_function_privilege('commerce_checkout_writer',p.oid,'EXECUTE')=a.checkout_writer_execute
	 AND NOT EXISTS(SELECT 1 FROM aclexplode(coalesce(p.proacl,acldefault('f',p.proowner))) a WHERE a.grantee=0 AND a.privilege_type='EXECUTE'))
	 FROM pg_proc p JOIN pg_namespace n ON n.oid=p.pronamespace
	 LEFT JOIN approved a ON a.oid=p.oid WHERE n.nspname='integration'`).Scan(&functions, &safe)
	if err != nil || functions != 48 || !safe {
		t.Fatalf("fixed function ACL: count=%d safe=%v err=%v", functions, safe, err)
	}
}

func TestPoolAuthorityCannotBeDisguisedWithStartupRole(t *testing.T) {
	f := fixture(t)
	ctx := context.Background()
	for _, tc := range []struct {
		role string
		open func(context.Context, string) (*pgxpool.Pool, error)
	}{
		{"commerce_runtime", platform.OpenPool},
		{"commerce_identity", platform.OpenIdentityPool},
		{"commerce_buyer_runtime", platform.OpenBuyerPool},
		{"commerce_buyer_issuer", platform.OpenBuyerIssuerPool},
		{"commerce_worker", platform.OpenWorkerPool},
	} {
		t.Run(tc.role, func(t *testing.T) {
			masked, err := url.Parse(f.databaseURL)
			if err != nil {
				t.Fatal("parse fixture DSN")
			}
			query := masked.Query()
			query.Set("options", "-c role="+tc.role)
			masked.RawQuery = query.Encode()
			if pool, err := tc.open(ctx, masked.String()); err == nil {
				pool.Close()
				t.Fatal("owner admitted under startup role")
			}
			if tc.role != "commerce_worker" {
				return
			}
			// Independently prove the dangerous current_user/session_user split
			// with SET ROLE after authentication. Startup option rejection alone
			// could be a parser/auth failure rather than our authority gate.
			cfg, err := pgxpool.ParseConfig(f.databaseURL)
			if err != nil {
				t.Fatal("parse supplied fixture pool")
			}
			cfg.AfterConnect = func(ctx context.Context, conn *pgx.Conn) error {
				_, err := conn.Exec(ctx, `SET ROLE commerce_worker`)
				return err
			}
			pool, err := pgxpool.NewWithConfig(ctx, cfg)
			if err != nil {
				t.Fatal("open masked fixture pool")
			}
			defer pool.Close()
			var role string
			var same bool
			if err := pool.QueryRow(ctx, `SELECT current_user, session_user=current_user`).Scan(&role, &same); err != nil {
				t.Fatal("read masked fixture identity")
			}
			if same || role != tc.role {
				t.Fatal("fixture did not disguise owner")
			}
			if err := platform.ValidateWorkerPool(ctx, pool); err == nil {
				t.Fatal("supplied owner pool admitted under SET ROLE")
			}
			if err := pool.Ping(ctx); err != nil {
				t.Fatal("validator closed supplied pool")
			}
		})
	}
}

func TestT06PolicyOutcomeCannotErasePossibleRemoteEffect(t *testing.T) {
	_, pool := t06AuthorityLogin(t, "commerce_worker")
	ctx := context.Background()
	id, _ := t06AuthorityOperation(t)
	token := []byte(strings.Repeat("p", 32))
	claim := t06AuthorityTake(t, pool, id, token)
	_, err := pool.Exec(ctx, `SELECT integration.complete_operation($1,$2,$3,'BLOCKED_POLICY','policy_denied','')`, id, claim.generation, token)
	if err != nil {
		t.Fatal(err)
	}
	var state, code string
	if err := pool.QueryRow(ctx, `SELECT state,result_code FROM integration.operations WHERE id=$1`, id).Scan(&state, &code); err != nil {
		t.Fatal(err)
	}
	if state != "BLOCKED_POLICY" || code != "policy_denied" {
		t.Fatalf("unexpected policy fact %s/%s", state, code)
	}
	uncertain, _ := t06AuthorityOperation(t)
	claim = t06AuthorityTake(t, pool, uncertain, token)
	if err := t06AuthorityFinish(pool, uncertain, claim.generation, token, "UNKNOWN"); err != nil {
		t.Fatal(err)
	}
	claim = t06AuthorityTake(t, pool, uncertain, token)
	_, err = pool.Exec(ctx, `SELECT integration.complete_operation($1,$2,$3,'BLOCKED_POLICY','policy_denied','')`, uncertain, claim.generation, token)
	if err == nil {
		t.Fatal("reconcile relabelled possible side effect as policy denied")
	}
	if err := pool.QueryRow(ctx, `SELECT state FROM integration.operations WHERE id=$1`, uncertain).Scan(&state); err != nil {
		t.Fatal(err)
	}
	if state != "UNKNOWN" {
		t.Fatalf("lost possible effect: %s", state)
	}
}

func TestT06SQLLeaseOwnershipExpiryAndNoRedispatch(t *testing.T) {
	f := fixture(t)
	ctx := context.Background()
	_, p := t06AuthorityLogin(t, "commerce_worker")
	id, _ := t06AuthorityOperation(t)
	a, b := randomBytes(32), randomBytes(32)
	c := t06AuthorityTake(t, p, id, a)
	if c.disposition != "claimed" || c.mode != "dispatch" || c.generation != 1 {
		t.Fatalf("initial claim: %+v", c)
	}
	if busy := t06AuthorityTake(t, p, id, b); busy.disposition != "busy" {
		t.Fatalf("duplicate: %+v", busy)
	}
	if err := t06AuthorityFinish(p, id, 1, b, "SUCCEEDED"); sqlState(err) != "40001" {
		t.Fatalf("wrong owner: %v", err)
	}
	if _, err := f.owner.Exec(ctx, `UPDATE integration.operations SET lease_until=clock_timestamp()-interval '1 second' WHERE id=$1`, id); err != nil {
		t.Fatal(err)
	}
	if err := t06AuthorityFinish(p, id, 1, a, "SUCCEEDED"); sqlState(err) != "40001" {
		t.Fatalf("expired owner: %v", err)
	}
	c = t06AuthorityTake(t, p, id, b)
	if c.disposition != "claimed" || c.mode != "reconcile" || c.generation != 2 {
		t.Fatalf("expired dispatch was resent: %+v", c)
	}
	if err := t06AuthorityFinish(p, id, 1, a, "SUCCEEDED"); sqlState(err) != "40001" {
		t.Fatalf("stale generation: %v", err)
	}
	if err := t06AuthorityFinish(p, id, 2, b, "ACKNOWLEDGED"); err != nil {
		t.Fatal(err)
	}
	c = t06AuthorityTake(t, p, id, a)
	if c.mode != "reconcile" || c.generation != 3 {
		t.Fatalf("ACK was resent: %+v", c)
	}
	if err := t06AuthorityFinish(p, id, 3, a, "UNKNOWN"); err != nil {
		t.Fatal(err)
	}
	c = t06AuthorityTake(t, p, id, b)
	if c.mode != "reconcile" || c.generation != 4 {
		t.Fatalf("UNKNOWN was resent: %+v", c)
	}
	if err := t06AuthorityFinish(p, id, 4, b, "SUCCEEDED"); err != nil {
		t.Fatal(err)
	}
	if c = t06AuthorityTake(t, p, id, a); c.disposition != "terminal" || c.generation != 4 {
		t.Fatalf("terminal re-executed: %+v", c)
	}
	var events int
	if err := f.owner.QueryRow(ctx, `SELECT count(*) FROM integration.operation_events WHERE operation_id=$1`, id).Scan(&events); err != nil || events != 7 {
		t.Fatalf("rejected/replayed lease wrote events: count=%d err=%v", events, err)
	}
}

func TestT06SQLConcurrentClaimAndBindingOutcomeFacts(t *testing.T) {
	f := fixture(t)
	ctx := context.Background()
	_, p := t06AuthorityLogin(t, "commerce_worker")
	id, binding := t06AuthorityOperation(t)
	tokens := [][]byte{randomBytes(32), randomBytes(32)}
	results := make([]t06AuthorityClaim, 2)
	errs := make([]error, 2)
	var wg sync.WaitGroup
	for i := range 2 {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			errs[i] = p.QueryRow(ctx, `SELECT * FROM integration.claim_operation($1,30,$2)`, id, tokens[i]).Scan(&results[i].disposition, &results[i].generation, &results[i].mode)
		}(i)
	}
	wg.Wait()
	winner := -1
	for i, c := range results {
		if errs[i] != nil {
			t.Fatal(errs[i])
		}
		if c.disposition == "claimed" {
			if winner != -1 {
				t.Fatal("two dispatch winners")
			}
			winner = i
		} else if c.disposition != "busy" {
			t.Fatalf("unexpected claim: %+v", c)
		}
	}
	if winner < 0 {
		t.Fatal("no dispatch winner")
	}
	if _, err := f.owner.Exec(ctx, `UPDATE integration.bindings SET enabled=false,semantic_version=2 WHERE id=$1`, binding); err != nil {
		t.Fatal(err)
	}
	if err := t06AuthorityFinish(p, id, 1, tokens[winner], "SUCCEEDED"); err != nil {
		t.Fatal(err)
	}
	var state, reason string
	if err := f.owner.QueryRow(ctx, `SELECT state FROM integration.operations WHERE id=$1`, id).Scan(&state); err != nil || state != "SUCCEEDED" {
		t.Fatalf("remote fact lost: state=%s err=%v", state, err)
	}
	if err := f.owner.QueryRow(ctx, `SELECT reason_code FROM integration.operation_events WHERE operation_id=$1 ORDER BY id DESC LIMIT 1`, id).Scan(&reason); err != nil || reason != "completed_binding_changed" {
		t.Fatalf("binding change not recorded: %s %v", reason, err)
	}
	for _, dispatched := range []bool{false, true} {
		id, binding = t06AuthorityOperation(t)
		token := randomBytes(32)
		if dispatched {
			t06AuthorityTake(t, p, id, token)
			if _, err := f.owner.Exec(ctx, `UPDATE integration.operations SET lease_until=clock_timestamp()-interval '1 second' WHERE id=$1`, id); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := f.owner.Exec(ctx, `UPDATE integration.bindings SET enabled=false,semantic_version=2 WHERE id=$1`, binding); err != nil {
			t.Fatal(err)
		}
		c := t06AuthorityTake(t, p, id, randomBytes(32))
		want := "terminal"
		if dispatched {
			want = "blocked_binding"
		}
		if c.disposition != want || c.mode != "" {
			t.Fatalf("binding fence dispatched=%v: %+v", dispatched, c)
		}
		if dispatched {
			if next := t06AuthorityTake(t, p, id, randomBytes(32)); next.generation != c.generation || next.disposition != "blocked_binding" {
				t.Fatalf("unchanged block churn: %+v", next)
			}
		}
	}
}

func TestT06SQLEventFailureRollsBackClaimAndCompletion(t *testing.T) {
	f := fixture(t)
	ctx := context.Background()
	_, p := t06AuthorityLogin(t, "commerce_worker")
	id, _ := t06AuthorityOperation(t)
	token := randomBytes(32)
	name := "t06_block_" + strings.ReplaceAll(randomUUID(), "-", "")
	function := pgx.Identifier{"integration", name}.Sanitize()
	trigger := pgx.Identifier{name}.Sanitize()
	if _, err := f.owner.Exec(ctx, fmt.Sprintf(`CREATE FUNCTION %s() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'synthetic event failure'; END $$; CREATE TRIGGER %s BEFORE INSERT ON integration.operation_events FOR EACH ROW WHEN (NEW.operation_id='%s') EXECUTE FUNCTION %s()`, function, trigger, id, function)); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if _, err := f.owner.Exec(ctx, fmt.Sprintf(`DROP TRIGGER %s ON integration.operation_events; DROP FUNCTION %s()`, trigger, function)); err != nil {
			t.Error(err)
		}
	})
	if _, err := p.Exec(ctx, `SELECT * FROM integration.claim_operation($1,30,$2)`, id, token); sqlState(err) != "P0001" {
		t.Fatalf("fault not exercised: %v", err)
	}
	var state string
	var gen int64
	if err := f.owner.QueryRow(ctx, `SELECT state,generation FROM integration.operations WHERE id=$1`, id).Scan(&state, &gen); err != nil || state != "READY" || gen != 0 {
		t.Fatalf("claim partial write %s/%d: %v", state, gen, err)
	}
	if _, err := f.owner.Exec(ctx, fmt.Sprintf(`ALTER TABLE integration.operation_events DISABLE TRIGGER %s`, trigger)); err != nil {
		t.Fatal(err)
	}
	t06AuthorityTake(t, p, id, token)
	if _, err := f.owner.Exec(ctx, fmt.Sprintf(`ALTER TABLE integration.operation_events ENABLE TRIGGER %s`, trigger)); err != nil {
		t.Fatal(err)
	}
	if err := t06AuthorityFinish(p, id, 1, token, "SUCCEEDED"); sqlState(err) != "P0001" {
		t.Fatalf("completion fault not exercised: %v", err)
	}
	if err := f.owner.QueryRow(ctx, `SELECT state,generation FROM integration.operations WHERE id=$1`, id).Scan(&state, &gen); err != nil || state != "DISPATCHING" || gen != 1 {
		t.Fatalf("completion partial write %s/%d: %v", state, gen, err)
	}
}

type t06AuthorityArgs struct {
	ProbeID string `json:"probe_id"`
}

func (t06AuthorityArgs) Kind() string { return "t06_authority_probe" }

type t06AuthorityWorker struct {
	river.WorkerDefaults[t06AuthorityArgs]
	seen chan int64
}

func (w *t06AuthorityWorker) Work(ctx context.Context, job *river.Job[t06AuthorityArgs]) error {
	select {
	case w.seen <- job.ID:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func TestT06OrdinaryWorkerStartsProcessesAndStops(t *testing.T) {
	ctx := context.Background()
	_, pool := t06AuthorityLogin(t, "commerce_worker")
	queue := "t06_" + strings.ReplaceAll(randomUUID(), "-", "")
	seen := make(chan int64, 2)
	workers := river.NewWorkers()
	river.AddWorker(workers, &t06AuthorityWorker{seen: seen})
	client, err := river.NewClient(riverpgxv5.New(pool), &river.Config{
		Schema: "river", Workers: workers,
		Queues: map[string]river.QueueConfig{queue: {MaxWorkers: 1}},
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatal(err)
	}
	job, err := client.Insert(ctx, t06AuthorityArgs{ProbeID: randomUUID()}, &river.InsertOpts{Queue: queue})
	if err != nil {
		t.Fatal(err)
	}
	if err = client.Start(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		stop, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := client.StopAndCancel(stop); err != nil {
			t.Error(err)
		}
	})
	select {
	case id := <-seen:
		if id != job.Job.ID {
			t.Fatalf("wrong job executed: %d", id)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("ordinary worker did not execute isolated probe")
	}
	// Wait for River's persisted completion, not merely the Work callback.
	deadline := time.Now().Add(5 * time.Second)
	for {
		var state string
		if err = pool.QueryRow(ctx, `SELECT state FROM river.river_job WHERE id=$1`, job.Job.ID).Scan(&state); err != nil {
			t.Fatal(err)
		}
		if state == "completed" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("worker callback not persisted: %s", state)
		}
		time.Sleep(20 * time.Millisecond)
	}
}
