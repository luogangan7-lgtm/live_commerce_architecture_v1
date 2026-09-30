package foundation_test

// SL02 (contracts/stripe-live-enable-v1.md §11 SL02, §3.1–§3.4, SP19): the 0077 + post_river/0016 schema, REAL_PG.
// Prefix `sls`. Written from the contract; where the contract names no SQLSTATE only "rejected by the database" is
// asserted, never a message.
//
// Owner-pool disclosure (each logged as `OWNER-POOL:`): approval/qualification rows built through the registrar
// definers of the shared LIVE harness (stripe_live_registrar_test.go); direct INSERT/UPDATE as the migration owner (a
// superuser: RLS does not hide, triggers still fire) to prove CHECKs, FKs and the immutability/revoke-only triggers;
// `SET LOCAL ROLE commerce_payment_registry_writer` transactions to prove the trigger holds for the role that owns the
// UPDATE privilege; replica-mode setup rows (restored) only to reach a revoked SANDBOX/PAYUNi row.
// The "populated-latest" upgrade runs in its own labelled PG container (mciStartPG) seeded through the pre-0077 definers.

import (
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"
	"github.com/riverqueue/river/rivermigrate"

	"livecommerce/migrations"
)

func TestStripeSL02Schema(t *testing.T) {
	t.Run("ledger_checksum_and_repeat", slsLedger)
	t.Run("widened_checks_sp19_and_amount_table", slsChecks)
	t.Run("approval_table_rls_privileges_and_definers", slsCatalog)
	base := pwIsolatedFixture(t) // rows are created directly; keep them out of the shared suite database
	t.Run("approval_checks_fks_immutability_and_one_active", func(t *testing.T) { slsApprovalTable(t, base) })
	t.Run("qualification_revoke_only_trigger", func(t *testing.T) { slsRevokeOnly(t, base) })
	t.Run("upgrade_of_a_populated_database", slsUpgrade)
}

func slsRead(t *testing.T, rel string) []byte {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "migrations", rel))
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func slsLedger(t *testing.T) {
	f := fixture(t)
	ctx := context.Background()
	versions := []string{"0077_stripe_live_enable.sql", "post_river/0016_stripe_live.sql"}
	sums := map[string]string{}
	for _, v := range versions {
		want := fmt.Sprintf("%x", sha256.Sum256(slsRead(t, v)))
		var got string
		var n int
		if err := f.owner.QueryRow(ctx, `SELECT count(*),coalesce(max(checksum),'') FROM public.lc_schema_migrations WHERE version=$1`, v).Scan(&n, &got); err != nil {
			t.Fatal(err)
		}
		if n != 1 || got != want {
			t.Fatalf("ledger %s: rows=%d checksum=%s want one row with sha256 of the file %s", v, n, got, want)
		}
		sums[v] = got
	}
	var before int
	if err := f.owner.QueryRow(ctx, `SELECT count(*) FROM public.lc_schema_migrations`).Scan(&before); err != nil {
		t.Fatal(err)
	}
	// Repeat: Apply is idempotent and never rewrites a recorded checksum (the fixture already applied twice).
	if err := migrations.Apply(ctx, f.owner); err != nil {
		t.Fatalf("repeat Apply on the latest database: %v", err)
	}
	var after int
	if err := f.owner.QueryRow(ctx, `SELECT count(*) FROM public.lc_schema_migrations`).Scan(&after); err != nil {
		t.Fatal(err)
	}
	if after != before {
		t.Fatalf("repeat Apply changed the ledger: %d -> %d", before, after)
	}
	for _, v := range versions {
		var got string
		if err := f.owner.QueryRow(ctx, `SELECT checksum FROM public.lc_schema_migrations WHERE version=$1`, v).Scan(&got); err != nil || got != sums[v] {
			t.Fatalf("checksum of %s changed by the repeat: %s %v", v, got, err)
		}
	}
}

func slsChecks(t *testing.T) {
	f := fixture(t)
	p := f.owner
	// SP19: a REAL_LIVE Stripe qualification without an approval is still refused; an approval id appears only on
	// REAL_LIVE Stripe rows (§3.1 two CHECKs).
	const qualCols = "id,tenant_id,store_id,connection_id,credential_version,environment,code,proof_class,evidence_ref,observed_at,expires_at,live_approval_id"
	const qualHead = "gen_random_uuid(),gen_random_uuid(),gen_random_uuid(),gen_random_uuid(),1,"
	const qualTail = "'x',now()-interval '1 second',now()+interval '1 hour',"
	valid := qualHead + "'LIVE','stripe_checkout','REAL_LIVE'," + qualTail + "gen_random_uuid()"
	for name, invalid := range map[string]string{
		"REAL_LIVE Stripe without approval (SP19)":  qualHead + "'LIVE','stripe_checkout','REAL_LIVE'," + qualTail + "NULL",
		"approval id on a PROVIDER_MOCK Stripe row": qualHead + "'SANDBOX','stripe_checkout','PROVIDER_MOCK'," + qualTail + "gen_random_uuid()",
		"approval id on a REAL_SANDBOX Stripe row":  qualHead + "'SANDBOX','stripe_checkout','REAL_SANDBOX'," + qualTail + "gen_random_uuid()",
		"approval id on a PAYUNi row":               qualHead + "'LIVE','payuni_credit','REAL_LIVE'," + qualTail + "gen_random_uuid()",
	} {
		t.Run("qualification/"+name, func(t *testing.T) {
			stripeExpectCheck(t, p, "payments.account_qualifications", qualCols, valid, invalid)
		})
	}
	// §3.1 endpoint env/profile CHECK: (environment='LIVE')=(execution_profile='LIVE'), both directions.
	const epCols = "endpoint_id,tenant_id,store_id,connection_id,environment,account_id,execution_profile,enabled,key_version,key_id,nonce,ciphertext"
	ep := func(env, profile string) string {
		return fmt.Sprintf("gen_random_uuid(),gen_random_uuid(),gen_random_uuid(),gen_random_uuid(),'%s','acct_x1','%s',true,1,'k',decode(repeat('00',12),'hex'),decode(repeat('00',17),'hex')", env, profile)
	}
	for _, c := range []struct{ env, profile string }{{"LIVE", "SANDBOX"}, {"LIVE", "PROVIDER_MOCK"}, {"SANDBOX", "LIVE"}, {"STAGING", "LIVE"}, {"LIVE", "STAGING"}} {
		t.Run("endpoint/"+c.env+"+"+c.profile, func(t *testing.T) {
			stripeExpectCheck(t, p, "payments.stripe_webhook_endpoints", epCols, ep("LIVE", "LIVE"), ep(c.env, c.profile))
		})
	}
	t.Run("endpoint/SANDBOX and PROVIDER_MOCK still valid", func(t *testing.T) {
		stripeExpectCheck(t, p, "payments.stripe_webhook_endpoints", epCols, ep("SANDBOX", "PROVIDER_MOCK"), ep("SANDBOX", "STAGING"))
		stripeExpectCheck(t, p, "payments.stripe_webhook_endpoints", epCols, ep("SANDBOX", "SANDBOX"), ep("SANDBOX", "LIVE"))
	})
	// §3.1 refund environment widened to SANDBOX and LIVE only.
	const rfCols = "tenant_id,store_id,id,attempt_id,order_id,owner_id,principal_id,environment,account_id,credential_version,payment_intent_id,currency,amount_minor,reason,create_params,requested_at,resend_until"
	rf := func(env string) string {
		return fmt.Sprintf("gen_random_uuid(),gen_random_uuid(),gen_random_uuid(),gen_random_uuid(),gen_random_uuid(),gen_random_uuid(),gen_random_uuid(),'%s','acct_x1',1,'pi_x1','TWD',2500,'requested_by_customer','{}'::jsonb,TIMESTAMPTZ '2026-01-01 00:00:00+00',TIMESTAMPTZ '2026-01-01 20:00:00+00'", env)
	}
	t.Run("refund/LIVE admitted, other environments refused", func(t *testing.T) {
		stripeExpectCheck(t, p, "payments.stripe_refunds", rfCols, rf("LIVE"), rf("STAGING"))
		stripeExpectCheck(t, p, "payments.stripe_refunds", rfCols, rf("SANDBOX"), rf("live"))
	})
	// §3.1: stripe_min_minor is the closed table next to stripe_amount_ok and the two agree per currency.
	for _, c := range []struct {
		code string
		min  int64
	}{{"HKD", 400}, {"USD", 50}, {"SGD", 50}, {"MYR", 200}, {"TWD", 2500}} {
		var got *int64
		var okAt, okBelow bool
		if err := p.QueryRow(context.Background(), `SELECT payments.stripe_min_minor($1),payments.stripe_amount_ok($1,payments.stripe_min_minor($1)),payments.stripe_amount_ok($1,payments.stripe_min_minor($1)-1)`, c.code).Scan(&got, &okAt, &okBelow); err != nil {
			t.Fatal(err)
		}
		if got == nil || *got != c.min || !okAt || okBelow {
			t.Fatalf("%s: stripe_min_minor=%v ok(min)=%v ok(min-1)=%v, want %d/true/false", c.code, got, okAt, okBelow, c.min)
		}
	}
	for _, code := range []string{"JPY", "ISK", "usd", "", "XXX"} {
		var got *int64
		if err := p.QueryRow(context.Background(), `SELECT payments.stripe_min_minor($1)`, code).Scan(&got); err != nil || got != nil {
			t.Fatalf("stripe_min_minor(%q)=%v %v, want NULL", code, got, err)
		}
	}
}

func slsCatalog(t *testing.T) {
	f := fixture(t)
	ctx := context.Background()
	p := f.owner
	must := func(label, q string, args ...any) {
		t.Helper()
		if !stripeCatalogBool(t, p, q, args...) {
			t.Errorf("SL02 catalog: %s", label)
		}
	}
	const tbl = "payments.stripe_live_approvals"
	must("approvals FORCE RLS", `SELECT c.relrowsecurity AND c.relforcerowsecurity FROM pg_class c WHERE c.oid=to_regclass($1)`, tbl)
	must("approvals: PUBLIC holds no privilege", `SELECT NOT EXISTS(SELECT 1 FROM pg_class c CROSS JOIN LATERAL aclexplode(COALESCE(c.relacl,acldefault('r',c.relowner))) a WHERE c.oid=to_regclass($1) AND a.grantee=0)`, tbl)
	must("approvals: no DELETE/TRUNCATE for anyone but the table owner", `SELECT NOT EXISTS(SELECT 1 FROM pg_class c CROSS JOIN LATERAL aclexplode(COALESCE(c.relacl,acldefault('r',c.relowner))) a
	  WHERE c.oid=to_regclass($1) AND a.grantee<>c.relowner AND a.privilege_type IN ('DELETE','TRUNCATE'))`, tbl)
	must("approvals: environment and provider are generated constants", `SELECT (SELECT count(*) FROM pg_attribute WHERE attrelid=to_regclass($1) AND attname IN ('environment','provider') AND attgenerated='s')=2`, tbl)
	must("approvals table has a COMMENT naming the owner package, no money movement and that it is not proof of consent",
		`SELECT obj_description(to_regclass($1),'pg_class') ILIKE '%payments/stripeadmin%' AND obj_description(to_regclass($1),'pg_class') ILIKE '%money%' AND obj_description(to_regclass($1),'pg_class') ILIKE '%consent%'`, tbl)
	must("every approvals column carries a COMMENT", `SELECT bool_and(col_description(a.attrelid,a.attnum) IS NOT NULL) FROM pg_attribute a WHERE a.attrelid=to_regclass($1) AND a.attnum>0 AND NOT a.attisdropped`, tbl)
	// Column-privilege matrix from information_schema: only the registry writer holds anything (SELECT, INSERT, and
	// UPDATE on exactly the six canary/revoke columns); the table owner's implicit grants are excluded.
	rows, err := p.Query(ctx, `SELECT cp.grantee,cp.column_name,cp.privilege_type FROM information_schema.column_privileges cp
	 WHERE cp.table_schema='payments' AND cp.table_name='stripe_live_approvals'
	   AND cp.grantee<>(SELECT pg_get_userbyid(relowner) FROM pg_class WHERE oid=to_regclass($1))`, tbl)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for rows.Next() {
		var grantee, col, priv string
		if err := rows.Scan(&grantee, &col, &priv); err != nil {
			t.Fatal(err)
		}
		got[grantee+"|"+col+"|"+priv] = true
	}
	rows.Close()
	cols := []string{}
	crow, err := p.Query(ctx, `SELECT column_name FROM information_schema.columns WHERE table_schema='payments' AND table_name='stripe_live_approvals'`)
	if err != nil {
		t.Fatal(err)
	}
	for crow.Next() {
		var c string
		if err := crow.Scan(&c); err != nil {
			t.Fatal(err)
		}
		cols = append(cols, c)
	}
	crow.Close()
	want := map[string]bool{}
	for _, c := range cols {
		want["commerce_payment_registry_writer|"+c+"|SELECT"] = true
		want["commerce_payment_registry_writer|"+c+"|INSERT"] = true
	}
	for _, c := range []string{"canary_attempt_id", "canary_refund_id", "canary_verified_at", "revoked_at", "revoked_by", "revoke_ref"} {
		want["commerce_payment_registry_writer|"+c+"|UPDATE"] = true
	}
	for _, k := range mciDiff(got, want) {
		t.Errorf("SL02 approvals: unexpected privilege %s", k)
	}
	for _, k := range mciDiff(want, got) {
		t.Errorf("SL02 approvals: missing privilege %s", k)
	}
	// No runtime/worker/ingress/checkout/integration/registrar privilege on approvals (table or any column).
	for _, role := range []string{"commerce_runtime", "commerce_worker", "commerce_stripe_ingress", "commerce_checkout_writer", "commerce_integration_writer",
		"commerce_checkout_runtime", "commerce_hosted_runtime", "commerce_payment_registrar", "commerce_auth"} {
		for _, priv := range []string{"SELECT", "INSERT", "UPDATE", "DELETE", "TRUNCATE", "REFERENCES"} {
			var ok bool
			if err := p.QueryRow(ctx, `SELECT to_regrole($1) IS NOT NULL AND has_table_privilege($1,$2,$3)`, role, tbl, priv).Scan(&ok); err != nil {
				t.Fatal(err)
			}
			if ok {
				t.Errorf("SL02: %s holds table %s on approvals", role, priv)
			}
			for _, c := range cols {
				if priv == "DELETE" || priv == "TRUNCATE" {
					continue
				}
				if err := p.QueryRow(ctx, `SELECT to_regrole($1) IS NOT NULL AND has_column_privilege($1,$2,$3,$4)`, role, tbl, c, priv).Scan(&ok); err != nil {
					t.Fatal(err)
				}
				if ok {
					t.Errorf("SL02: %s holds %s(%s) on approvals", role, priv, c)
				}
			}
		}
	}
	// qualifications: nobody but the registry writer may set revoked_at, and nobody holds table-level UPDATE. (UPDATE(id) row-lock
	// grants of 0016/0061 to other authorities predate 0077; the revoke-only trigger makes them inert and is tested below.)
	for _, role := range []string{"commerce_runtime", "commerce_worker", "commerce_stripe_ingress", "commerce_checkout_writer", "commerce_integration_writer", "commerce_payment_registrar"} {
		var ok bool
		if err := p.QueryRow(ctx, `SELECT has_table_privilege($1,'payments.account_qualifications','UPDATE') OR has_column_privilege($1,'payments.account_qualifications','revoked_at','UPDATE')`, role).Scan(&ok); err != nil {
			t.Fatal(err)
		}
		if ok {
			t.Errorf("SL02: %s may UPDATE payments.account_qualifications table-wide or revoked_at", role)
		}
	}
	must("registry writer has UPDATE(revoked_at) but not table-level UPDATE on qualifications",
		`SELECT has_column_privilege('commerce_payment_registry_writer','payments.account_qualifications','revoked_at','UPDATE') AND NOT has_table_privilege('commerce_payment_registry_writer','payments.account_qualifications','UPDATE')
		  AND NOT has_column_privilege('commerce_payment_registry_writer','payments.account_qualifications','evidence_ref','UPDATE')`)
	must("qualifications still FORCE RLS", `SELECT relrowsecurity AND relforcerowsecurity FROM pg_class WHERE oid='payments.account_qualifications'::regclass`)
	must("the lock policy stays and the revoke policy is a separate permissive policy for the registry writer",
		`SELECT EXISTS(SELECT 1 FROM pg_policy WHERE polrelid='payments.account_qualifications'::regclass AND polname='stripe_registry_qualification_lock' AND polwithcheck IS NOT NULL AND pg_get_expr(polwithcheck,polrelid)='false')
		 AND EXISTS(SELECT 1 FROM pg_policy WHERE polrelid='payments.account_qualifications'::regclass AND polname='stripe_registry_qualification_revoke' AND polpermissive AND polcmd='w')`)
	// Definers: owner, search_path, ACL. EXECUTE holders excluding the function owner.
	type fn struct {
		sig, owner string
		execute    []string
	}
	for _, c := range []fn{
		{"payments.approve_stripe_live(uuid,uuid,uuid,uuid,uuid,text,text,timestamptz,bigint,bigint,text[],jsonb)", "commerce_payment_registry_writer", []string{"commerce_payment_registrar"}},
		{"payments.record_stripe_live_canary(uuid,uuid,uuid,uuid,uuid,uuid)", "commerce_payment_registry_writer", []string{"commerce_payment_registrar"}},
		{"payments.revoke_stripe_live(uuid,uuid,uuid,uuid,text)", "commerce_payment_registry_writer", []string{"commerce_payment_registrar"}},
		{"identity.merchant_refund_environment(bytea,uuid,uuid)", "commerce_auth", []string{"commerce_runtime"}}, // S5 / brief ruling B5
	} {
		var owner string
		var secdef bool
		var config []string
		var holders []string
		if err := p.QueryRow(ctx, `SELECT pg_get_userbyid(p.proowner),p.prosecdef,coalesce(p.proconfig,'{}'),
		  coalesce((SELECT array_agg(CASE WHEN a.grantee=0 THEN 'PUBLIC' ELSE pg_get_userbyid(a.grantee) END ORDER BY 1) FROM aclexplode(coalesce(p.proacl,acldefault('f',p.proowner))) a
		   WHERE a.privilege_type='EXECUTE' AND a.grantee<>p.proowner),'{}')
		  FROM pg_proc p WHERE p.oid=to_regprocedure($1)`, c.sig).Scan(&owner, &secdef, &config, &holders); err != nil {
			t.Fatalf("%s: %v", c.sig, err)
		}
		if owner != c.owner || !secdef || strings.Join(config, ",") != "search_path=pg_catalog" || strings.Join(holders, ",") != strings.Join(c.execute, ",") {
			t.Errorf("SL02 definer %s: owner=%s secdef=%v config=%v EXECUTE=%v; want %s/true/[search_path=pg_catalog]/%v", c.sig, owner, secdef, config, holders, c.owner, c.execute)
		}
		for _, role := range []string{"commerce_worker", "commerce_stripe_ingress", "commerce_checkout_writer", "commerce_integration_writer", "commerce_hosted_runtime"} {
			if c.owner == "commerce_auth" {
				break
			}
			var ok bool
			if err := p.QueryRow(ctx, `SELECT has_function_privilege($1,to_regprocedure($2),'EXECUTE')`, role, c.sig).Scan(&ok); err != nil || ok {
				t.Errorf("SL02: %s may EXECUTE %s (%v)", role, c.sig, err)
			}
		}
	}
	must("stripe_min_minor is IMMUTABLE with search_path=pg_catalog, EXECUTE registry writer, not PUBLIC",
		`SELECT p.provolatile='i' AND p.proconfig=ARRAY['search_path=pg_catalog'] AND has_function_privilege('commerce_payment_registry_writer',p.oid,'EXECUTE')
		  AND NOT EXISTS(SELECT 1 FROM aclexplode(coalesce(p.proacl,acldefault('f',p.proowner))) a WHERE a.grantee=0)
		 FROM pg_proc p WHERE p.oid=to_regprocedure('payments.stripe_min_minor(text)')`)
	// Every function created by 0077/0016 or re-created there stays closed to PUBLIC.
	must("no 0077 object is executable by PUBLIC",
		`SELECT NOT EXISTS(SELECT 1 FROM pg_proc p JOIN pg_namespace n ON n.oid=p.pronamespace CROSS JOIN LATERAL aclexplode(coalesce(p.proacl,acldefault('f',p.proowner))) a
		  WHERE n.nspname IN ('payments','integration','checkout') AND p.proname IN ('approve_stripe_live','record_stripe_live_canary','revoke_stripe_live','stripe_min_minor',
		   'guard_stripe_live_approval','account_qualification_revoke_only','register_stripe_account','rotate_stripe_key','set_stripe_webhook_endpoint','stripe_endpoint_account',
		   'stripe_registrar_credential','qualify_stripe_method','set_stripe_method','apply_stripe_refund','hosted_payment_view_v2','start_stripe_payment','request_stripe_signal',
		   'record_stripe_refund_observation','record_stripe_charge_observation','request_stripe_refund') AND a.grantee=0 AND a.privilege_type='EXECUTE')`)
	// The two new triggers exist, are BEFORE UPDATE FOR EACH ROW and are enabled.
	must("revoke-only and approval-guard triggers are enabled BEFORE UPDATE row triggers",
		`SELECT (SELECT count(*) FROM pg_trigger WHERE tgname IN ('account_qualification_revoke_only','guard_stripe_live_approval') AND tgenabled='O' AND (tgtype & 1)=1 AND (tgtype & 2)=2 AND (tgtype & 16)=16)=2`)
}

// slsRegistryWriter runs one statement as commerce_payment_registry_writer (SET LOCAL ROLE from the owner) with the pinned
// GUC scope the definers set, in its own transaction.
func slsRegistryWriter(t *testing.T, s *slrEnv, sql string, args ...any) error {
	t.Helper()
	ctx := context.Background()
	tx, err := s.f.owner.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	if _, err = tx.Exec(ctx, `SET LOCAL ROLE commerce_payment_registry_writer`); err != nil {
		t.Fatal(err)
	}
	if _, err = tx.Exec(ctx, `SELECT set_config('app.tenant_id',$1,true),set_config('app.store_id',$2,true)`, s.scope.TenantID, s.scope.StoreID); err != nil {
		t.Fatal(err)
	}
	if _, err = tx.Exec(ctx, sql, args...); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func slsApprovalTable(t *testing.T, base *testFixture) {
	ctx := context.Background()
	// A: registered LIVE account, no approval: CHECK and FK negatives on the real table (each in a rolled-back tx).
	a := slrNew(t, base)
	a.registerLive(t)
	slrDisclose(t, "direct INSERTs into payments.stripe_live_approvals (rolled back)")
	insert := func(mods map[string]any) error {
		cols := map[string]any{"id": randomUUID(), "tenant_id": a.scope.TenantID, "store_id": a.scope.StoreID, "connection_id": a.conn, "account_id": a.account,
			"currency": "TWD", "approved_by": a.scope.PrincipalID, "approval_ref": "owner-chat:2026-09-30:stripe-live:sls", "approved_at": time.Now().Add(-time.Hour).UTC(),
			"canary_max_minor": int64(5000), "max_minor": int64(2000000), "checklist": slrChecklist, "account_readiness": `{"a":true}`}
		for k, v := range mods {
			cols[k] = v
		}
		var names, marks []string
		var vals []any
		for k, v := range cols {
			names, vals = append(names, k), append(vals, v)
			marks = append(marks, fmt.Sprintf("$%d", len(vals)))
		}
		tx, err := a.f.owner.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback(ctx)
		_, err = tx.Exec(ctx, `INSERT INTO payments.stripe_live_approvals(`+strings.Join(names, ",")+`) VALUES(`+strings.Join(marks, ",")+`)`, vals...)
		return err
	}
	if err := insert(nil); err != nil {
		t.Fatalf("baseline approval row must be insertable: %v", err)
	}
	wrongMember := randomUUID()
	mustExec(t, a.f.owner, `INSERT INTO identity.principals(id) VALUES($1)`, wrongMember)
	now := time.Now().UTC()
	for name, c := range map[string]struct {
		mods map[string]any
		code string
	}{
		"account_id shape":                {map[string]any{"account_id": "acct_"}, "23514"},
		"currency shape":                  {map[string]any{"currency": "twd"}, "23514"},
		"approval_ref shape":              {map[string]any{"approval_ref": "short"}, "23514"},
		"canary cap must be positive":     {map[string]any{"canary_max_minor": int64(0), "max_minor": int64(2000000)}, "23514"},
		"max below canary":                {map[string]any{"canary_max_minor": int64(5000), "max_minor": int64(4900)}, "23514"},
		"approved_at after recorded_at":   {map[string]any{"approved_at": now.Add(time.Hour)}, "23514"},
		"canary triple partial":           {map[string]any{"canary_attempt_id": randomUUID()}, "23514"},
		"canary triple without time":      {map[string]any{"canary_attempt_id": randomUUID(), "canary_refund_id": randomUUID()}, "23514"},
		"revoke triple partial":           {map[string]any{"revoked_at": now}, "23514"},
		"revoke_ref shape":                {map[string]any{"revoked_at": now, "revoked_by": a.scope.PrincipalID, "revoke_ref": "x"}, "23514"},
		"approved_by is not a member":     {map[string]any{"approved_by": wrongMember}, "23503"},
		"revoked_by is not a member":      {map[string]any{"revoked_at": now, "revoked_by": wrongMember, "revoke_ref": "owner-chat:revoke-x"}, "23503"},
		"connection unknown":              {map[string]any{"connection_id": randomUUID()}, "23503"},
		"account_id differs from account": {map[string]any{"account_id": "acct_Other" + t04Tag()}, "23503"},
	} {
		t.Run("approval/"+name, func(t *testing.T) { slrWant(t, name, insert(c.mods), c.code, "") })
	}
	// FK approval -> LIVE account: a SANDBOX account cannot carry an approval.
	b := slrNew(t, base)
	if err := b.registerAccount(t, "SANDBOX"); err != nil {
		t.Fatal(err)
	}
	tx, err := b.f.owner.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	_, err = tx.Exec(ctx, `INSERT INTO payments.stripe_live_approvals(id,tenant_id,store_id,connection_id,account_id,currency,approved_by,approval_ref,approved_at,canary_max_minor,max_minor,checklist,account_readiness)
	 VALUES(gen_random_uuid(),$1,$2,$3,$4,'TWD',$5,'owner-chat:2026-09-30:stripe-live:sls',now()-interval '1 hour',5000,2000000,$6,'{}')`,
		b.scope.TenantID, b.scope.StoreID, b.conn, b.account, b.scope.PrincipalID, slrChecklist)
	_ = tx.Rollback(ctx)
	slrWant(t, "approval on a SANDBOX account", err, "23503", "")

	// D: immutability and set-once triples on a definer-created approval.
	d := slrNew(t, base)
	d.registerLive(t)
	ap := d.mustApprove(t)
	row := func() string {
		var out string
		if err := d.f.owner.QueryRow(ctx, `SELECT x::text FROM payments.stripe_live_approvals x WHERE id=$1`, ap.ID).Scan(&out); err != nil {
			t.Fatal(err)
		}
		return out
	}
	frozen := row()
	other := d.member(t, "integration:manage")
	for col, val := range map[string]any{
		"approval_ref": "owner-chat:2026-09-30:changed", "approved_at": time.Now().Add(-2 * time.Hour).UTC(), "currency": "HKD", "max_minor": int64(4000000),
		"canary_max_minor": int64(2500), "checklist": []string{"account_active"}, "account_readiness": `{"changed":true}`, "approved_by": other,
		"recorded_at": time.Now().Add(-3 * time.Hour).UTC(), "id": randomUUID(), "tenant_id": randomUUID(), "store_id": randomUUID(), "connection_id": randomUUID(), "account_id": "acct_Changed1",
	} {
		_, err := d.f.owner.Exec(ctx, `UPDATE payments.stripe_live_approvals SET `+col+`=$2 WHERE id=$1`, ap.ID, val)
		slrRejected(t, "immutable column "+col, err)
		if row() != frozen {
			t.Fatalf("a rejected update of %s changed the row", col)
		}
	}
	// Canary triple: partial refused by CHECK, complete accepted once, then frozen (change and NULL both refused).
	_, err = d.f.owner.Exec(ctx, `UPDATE payments.stripe_live_approvals SET canary_attempt_id=$2 WHERE id=$1`, ap.ID, randomUUID())
	slrWant(t, "partial canary triple", err, "23514", "")
	att, ref := randomUUID(), randomUUID()
	slrDisclose(t, "payments.stripe_live_approvals canary triple set directly (set-once trigger check)")
	if _, err = d.f.owner.Exec(ctx, `UPDATE payments.stripe_live_approvals SET canary_attempt_id=$2,canary_refund_id=$3,canary_verified_at=clock_timestamp() WHERE id=$1`, ap.ID, att, ref); err != nil {
		t.Fatalf("first canary triple write: %v", err)
	}
	after := row()
	for name, sql := range map[string]string{
		"change the canary attempt": `UPDATE payments.stripe_live_approvals SET canary_attempt_id=gen_random_uuid() WHERE id=$1`,
		"change the canary refund":  `UPDATE payments.stripe_live_approvals SET canary_refund_id=gen_random_uuid() WHERE id=$1`,
		"change the verified time":  `UPDATE payments.stripe_live_approvals SET canary_verified_at=canary_verified_at+interval '1 minute' WHERE id=$1`,
		"clear the canary triple":   `UPDATE payments.stripe_live_approvals SET canary_attempt_id=NULL,canary_refund_id=NULL,canary_verified_at=NULL WHERE id=$1`,
	} {
		_, err := d.f.owner.Exec(ctx, sql, ap.ID)
		slrRejected(t, name, err)
		if row() != after {
			t.Fatalf("%s: a rejected update changed the row", name)
		}
	}
	// Revoke triple: same discipline, and the FK on revoked_by.
	_, err = d.f.owner.Exec(ctx, `UPDATE payments.stripe_live_approvals SET revoked_at=clock_timestamp() WHERE id=$1`, ap.ID)
	slrWant(t, "partial revoke triple", err, "23514", "")
	_, err = d.f.owner.Exec(ctx, `UPDATE payments.stripe_live_approvals SET revoked_at=clock_timestamp(),revoked_by=$2,revoke_ref='owner-chat:revoke-x' WHERE id=$1`, ap.ID, wrongMember)
	slrWant(t, "revoked_by not a member", err, "23503", "")
	if _, err = d.f.owner.Exec(ctx, `UPDATE payments.stripe_live_approvals SET revoked_at=clock_timestamp(),revoked_by=$2,revoke_ref='owner-chat:revoke-x' WHERE id=$1`, ap.ID, d.scope.PrincipalID); err != nil {
		t.Fatalf("first revoke triple write: %v", err)
	}
	revoked := row()
	for name, sql := range map[string]string{
		"change the revoke ref":   `UPDATE payments.stripe_live_approvals SET revoke_ref='owner-chat:revoke-y' WHERE id=$1`,
		"clear the revoke triple": `UPDATE payments.stripe_live_approvals SET revoked_at=NULL,revoked_by=NULL,revoke_ref=NULL WHERE id=$1`,
		"change the revoke time":  `UPDATE payments.stripe_live_approvals SET revoked_at=revoked_at+interval '1 minute' WHERE id=$1`,
	} {
		_, err := d.f.owner.Exec(ctx, sql, ap.ID)
		slrRejected(t, name, err)
		if row() != revoked {
			t.Fatalf("%s: a rejected update changed the row", name)
		}
	}
	// One active approval per connection (revoked ones do not count); the registry writer cannot DELETE.
	_, err = d.f.owner.Exec(ctx, `INSERT INTO payments.stripe_live_approvals(id,tenant_id,store_id,connection_id,account_id,currency,approved_by,approval_ref,approved_at,canary_max_minor,max_minor,checklist,account_readiness)
	 SELECT gen_random_uuid(),tenant_id,store_id,connection_id,account_id,currency,approved_by,'owner-chat:second-active',approved_at,canary_max_minor,max_minor,checklist,account_readiness FROM payments.stripe_live_approvals WHERE id=$1`, ap.ID)
	if err != nil {
		t.Fatalf("a second approval after the first was revoked: %v", err)
	}
	_, err = d.f.owner.Exec(ctx, `INSERT INTO payments.stripe_live_approvals(id,tenant_id,store_id,connection_id,account_id,currency,approved_by,approval_ref,approved_at,canary_max_minor,max_minor,checklist,account_readiness)
	 SELECT gen_random_uuid(),tenant_id,store_id,connection_id,account_id,currency,approved_by,'owner-chat:third-active',approved_at,canary_max_minor,max_minor,checklist,account_readiness FROM payments.stripe_live_approvals WHERE id=$1`, ap.ID)
	slrWant(t, "two active approvals of one connection", err, "23505", "")
	err = slsRegistryWriter(t, d, `DELETE FROM payments.stripe_live_approvals WHERE id=$1`, ap.ID)
	slrRejected(t, "registry writer DELETE of an approval", err)
	// Qualification -> approval FK: a REAL_LIVE qualification cannot name another connection's approval.
	q := slrNew(t, base)
	q.registerLive(t)
	qap := q.mustApprove(t)
	_, err = q.f.owner.Exec(ctx, `INSERT INTO payments.account_qualifications(id,tenant_id,store_id,connection_id,credential_version,environment,code,proof_class,evidence_ref,observed_at,expires_at,live_approval_id)
	 VALUES(gen_random_uuid(),$1,$2,$3,1,'LIVE','stripe_checkout','REAL_LIVE','x',now()-interval '1 second',now()+interval '1 hour',$4)`, a.scope.TenantID, a.scope.StoreID, a.conn, qap.ID)
	slrWant(t, "qualification naming another connection's approval", err, "23503", "")
	// A qualification of the own connection naming the own approval is fine (baseline of the FK).
	if _, err = q.f.owner.Exec(ctx, `INSERT INTO payments.account_qualifications(id,tenant_id,store_id,connection_id,credential_version,environment,code,proof_class,evidence_ref,observed_at,expires_at,live_approval_id)
	 VALUES(gen_random_uuid(),$1,$2,$3,1,'LIVE','stripe_checkout','REAL_LIVE','x',now()-interval '1 second',now()+interval '1 hour',$4)`, q.scope.TenantID, q.scope.StoreID, q.conn, qap.ID); err != nil {
		t.Fatalf("baseline own-approval qualification: %v", err)
	}
}

func slsRevokeOnly(t *testing.T, base *testFixture) {
	ctx := context.Background()
	// LIVE-approved Stripe row, SANDBOX Stripe row and PAYUNi row, each in its own tenant.
	l := slrNew(t, base).live(t)
	sb := slrNew(t, base)
	if err := sb.registerAccount(t, "SANDBOX"); err != nil {
		t.Fatal(err)
	}
	sbQ, err := sb.qualify(t, "PROVIDER_MOCK", "provider-mock:"+t04Tag(), time.Now().Add(-time.Second), time.Now().Add(time.Hour), 1)
	if err != nil {
		t.Fatal(err)
	}
	px := slrNew(t, base)
	pxQ := px.p.proof // the PAYUNi PROVIDER_MOCK qualification psSetup inserted
	rowOf := func(f *testFixture, id string) string {
		var out string
		if err := f.owner.QueryRow(ctx, `SELECT x::text FROM payments.account_qualifications x WHERE id=$1`, id).Scan(&out); err != nil {
			t.Fatal(err)
		}
		return out
	}
	// Every disallowed change is rejected for every kind of row (owner: superuser, triggers still fire).
	changes := map[string]string{
		"id change":           `UPDATE payments.account_qualifications SET id=gen_random_uuid() WHERE id=$1`,
		"evidence_ref change": `UPDATE payments.account_qualifications SET evidence_ref='changed' WHERE id=$1`,
		"expires_at change":   `UPDATE payments.account_qualifications SET expires_at=expires_at+interval '1 hour' WHERE id=$1`,
		"observed_at change":  `UPDATE payments.account_qualifications SET observed_at=observed_at-interval '1 minute' WHERE id=$1`,
		"credential_version":  `UPDATE payments.account_qualifications SET credential_version=credential_version WHERE id=$1 AND false`,
	}
	delete(changes, "credential_version")
	kinds := []struct {
		name string
		f    *testFixture
		id   string
	}{{"Stripe LIVE", l.f, l.qual}, {"Stripe SANDBOX", sb.f, sbQ}, {"PAYUNi", px.f, pxQ}}
	for _, k := range kinds {
		for name, sql := range changes {
			t.Run(k.name+"/"+name, func(t *testing.T) {
				before := rowOf(k.f, k.id)
				_, err := k.f.owner.Exec(ctx, sql, k.id)
				slrWant(t, k.name+" "+name, err, "PT409", "")
				if rowOf(k.f, k.id) != before {
					t.Fatal("a rejected update changed the row")
				}
			})
		}
	}
	// revoked_at NULL -> now is allowed only on a LIVE-approved Stripe row; SANDBOX Stripe and PAYUNi rows refuse it.
	for _, k := range kinds[1:] {
		t.Run(k.name+"/revoke refused", func(t *testing.T) {
			before := rowOf(k.f, k.id)
			_, err := k.f.owner.Exec(ctx, `UPDATE payments.account_qualifications SET revoked_at=clock_timestamp() WHERE id=$1`, k.id)
			slrWant(t, k.name+" revoke", err, "PT409", "")
			if rowOf(k.f, k.id) != before {
				t.Fatal("a rejected revoke changed the row")
			}
		})
	}
	// Reach a revoked SANDBOX/PAYUNi row (replica-mode setup) to prove revoked_at cannot be cleared either.
	for _, k := range kinds[1:] {
		slrReplica(t, k.f, `UPDATE payments.account_qualifications SET revoked_at=clock_timestamp() WHERE id=$1`, k.id)
		before := rowOf(k.f, k.id)
		_, err := k.f.owner.Exec(ctx, `UPDATE payments.account_qualifications SET revoked_at=NULL WHERE id=$1`, k.id)
		slrWant(t, k.name+" clear revoked_at", err, "PT409", "")
		if rowOf(k.f, k.id) != before {
			t.Fatalf("%s: clearing revoked_at changed the row", k.name)
		}
	}
	// The role that owns the UPDATE privilege obeys the trigger as well: id change refused, revoke of a SANDBOX row refused.
	slrRejected(t, "registry writer id change (LIVE row)", slsRegistryWriter(t, l.slrEnv, `UPDATE payments.account_qualifications SET id=gen_random_uuid() WHERE id=$1`, l.qual))
	slrRejected(t, "registry writer evidence change (LIVE row)", slsRegistryWriter(t, l.slrEnv, `UPDATE payments.account_qualifications SET evidence_ref='x' WHERE id=$1`, l.qual))
	slrRejected(t, "registry writer revoke of a SANDBOX row", slsRegistryWriter(t, sb, `UPDATE payments.account_qualifications SET revoked_at=clock_timestamp() WHERE id=$1`, sbQ))
	// The revoke of a LIVE-approved row ignores the supplied timestamp and stamps clock_timestamp() (§3.1).
	windowStart := time.Now().Add(-2 * time.Second)
	if err := slsRegistryWriter(t, l.slrEnv, `UPDATE payments.account_qualifications SET revoked_at=TIMESTAMPTZ '2001-01-01 00:00:00+00' WHERE id=$1`, l.qual); err != nil {
		t.Fatalf("registry writer revoke of a LIVE row: %v", err)
	}
	var stored time.Time
	if err := l.f.owner.QueryRow(ctx, `SELECT revoked_at FROM payments.account_qualifications WHERE id=$1`, l.qual).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if stored.Before(windowStart) || stored.After(time.Now().Add(2*time.Second)) {
		t.Fatalf("stored revoked_at %v is not the trigger's clock (supplied 2001-01-01)", stored)
	}
	// LIVE row now revoked: clearing revoked_at, or moving it, is refused for every role.
	for name, sql := range map[string]string{
		"clear":                           `UPDATE payments.account_qualifications SET revoked_at=NULL WHERE id=$1`,
		"move":                            `UPDATE payments.account_qualifications SET revoked_at=revoked_at+interval '1 hour' WHERE id=$1`,
		"revoke again with another value": `UPDATE payments.account_qualifications SET revoked_at=TIMESTAMPTZ '2001-01-01 00:00:00+00' WHERE id=$1`,
	} {
		_, err := l.f.owner.Exec(ctx, sql, l.qual)
		slrWant(t, "owner "+name, err, "PT409", "")
		slrRejected(t, "registry writer "+name, slsRegistryWriter(t, l.slrEnv, sql, l.qual))
	}
	var again time.Time
	if err := l.f.owner.QueryRow(ctx, `SELECT revoked_at FROM payments.account_qualifications WHERE id=$1`, l.qual).Scan(&again); err != nil || !again.Equal(stored) {
		t.Fatalf("revoked_at moved after refused updates: %v -> %v (%v)", stored, again, err)
	}
	// A revoked qualification is refused by start (the kill-switch hook is unchanged): covered by SL03; here the
	// registrar login itself has no direct UPDATE at all.
	_, err = l.pool.Exec(ctx, `UPDATE payments.account_qualifications SET revoked_at=clock_timestamp() WHERE id=$1`, sbQ)
	slrWant(t, "registrar login direct UPDATE", err, "42501", "")
}

// ---------------------------------------------------------------------------------------------------------------------
// Populated-latest upgrade
// ---------------------------------------------------------------------------------------------------------------------

// slsApplyWithout mirrors migrations.Apply (phase order, ledger, River grants: same as mciApplyWithout) but skips the two
// stripe-live files, producing the populated pre-0077 database.
func slsApplyWithout(t *testing.T, owner *pgxpool.Pool, skipNumbered, skipPost string) {
	t.Helper()
	ctx := context.Background()
	numbered, _ := filepath.Glob("../../migrations/[0-9][0-9][0-9][0-9]_*.sql")
	post, _ := filepath.Glob("../../migrations/post_river/[0-9][0-9][0-9][0-9]_*.sql")
	sort.Strings(numbered)
	sort.Strings(post)
	apply := func(tx pgx.Tx, paths []string, prefix, skip string) {
		for _, path := range paths {
			if filepath.Base(path) == skip {
				continue
			}
			body, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := tx.Exec(ctx, string(body)); err != nil {
				t.Fatalf("historical migration %s: %v", path, err)
			}
			if _, err := tx.Exec(ctx, `INSERT INTO public.lc_schema_migrations(version,checksum) VALUES($1,$2)`, prefix+filepath.Base(path), fmt.Sprintf("%x", sha256.Sum256(body))); err != nil {
				t.Fatal(err)
			}
		}
	}
	tx, err := owner.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `CREATE TABLE IF NOT EXISTS public.lc_schema_migrations (version text PRIMARY KEY, checksum text NOT NULL, applied_at timestamptz NOT NULL DEFAULT now())`); err != nil {
		t.Fatal(err)
	}
	apply(tx, numbered, "", skipNumbered)
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	for _, schema := range []string{"river", "river_meta", "river_payment", "river_expiry", "river_media"} {
		up, err := rivermigrate.New(riverpgxv5.New(owner), &rivermigrate.Config{Schema: schema})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := up.Migrate(ctx, rivermigrate.DirectionUp, nil); err != nil {
			t.Fatal(err)
		}
	}
	// The same River/queue grants migrations.Apply issues between the phases (copied from mciApplyWithout).
	for _, stmt := range []string{
		`GRANT SELECT, INSERT, UPDATE(kind) ON river.river_job TO commerce_runtime; GRANT USAGE ON SEQUENCE river.river_job_id_seq TO commerce_runtime`,
		`GRANT USAGE ON SCHEMA river TO commerce_worker; GRANT SELECT,INSERT,UPDATE,DELETE ON ALL TABLES IN SCHEMA river TO commerce_worker;
		 REVOKE ALL ON river.river_migration FROM commerce_worker; GRANT USAGE,SELECT ON ALL SEQUENCES IN SCHEMA river TO commerce_worker`,
	} {
		if _, err := owner.Exec(ctx, stmt); err != nil {
			t.Fatal(err)
		}
	}
	tx, err = owner.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `GRANT SELECT, INSERT, UPDATE(kind) ON river.river_job TO commerce_checkout_runtime; GRANT USAGE ON SEQUENCE river.river_job_id_seq TO commerce_checkout_runtime;
		 GRANT SELECT ON river.river_job TO commerce_checkout_writer,commerce_integration_writer`); err != nil {
		t.Fatal(err)
	}
	apply(tx, post, "post_river/", skipPost)
	for _, stmt := range []string{
		`GRANT UPDATE(kind) ON river_payment.river_job TO commerce_stripe_ingress`,
		`GRANT USAGE ON SCHEMA river_meta TO commerce_meta_worker; GRANT SELECT,INSERT,UPDATE,DELETE ON ALL TABLES IN SCHEMA river_meta TO commerce_meta_worker;
		 REVOKE ALL ON river_meta.river_migration FROM commerce_meta_worker; GRANT USAGE,SELECT ON ALL SEQUENCES IN SCHEMA river_meta TO commerce_meta_worker`,
		`REVOKE ALL ON river.river_job FROM commerce_checkout_runtime,commerce_checkout_writer; REVOKE UPDATE(kind) ON river.river_job FROM commerce_checkout_runtime;
		 REVOKE UPDATE(queue) ON river.river_job FROM commerce_checkout_writer; REVOKE ALL ON river.river_job_id_seq FROM commerce_checkout_runtime;
		 REVOKE ALL ON SCHEMA river FROM commerce_checkout_runtime,commerce_checkout_writer;
		 GRANT USAGE ON SCHEMA river_payment,river_expiry TO commerce_worker;
		 GRANT SELECT,INSERT,UPDATE,DELETE ON ALL TABLES IN SCHEMA river_payment,river_expiry TO commerce_worker;
		 REVOKE ALL ON river_payment.river_migration,river_expiry.river_migration FROM commerce_worker;
		 GRANT USAGE,SELECT ON ALL SEQUENCES IN SCHEMA river_payment,river_expiry TO commerce_worker`,
	} {
		if _, err := tx.Exec(ctx, stmt); err != nil {
			t.Fatal(err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
}

func slsUpgrade(t *testing.T) {
	owner := mciStartPG(t)
	ctx := context.Background()
	slsApplyWithout(t, owner, "0077_stripe_live_enable.sql", "0016_stripe_live.sql")
	for _, v := range []string{"0077_stripe_live_enable.sql", "post_river/0016_stripe_live.sql"} {
		var n int
		if err := owner.QueryRow(ctx, `SELECT count(*) FROM public.lc_schema_migrations WHERE version=$1`, v).Scan(&n); err != nil || n != 0 {
			t.Fatalf("fixture is not pre-0077: %s recorded (%d, %v)", v, n, err)
		}
	}
	// Populate: a SANDBOX Stripe account, endpoint and PROVIDER_MOCK qualification through the pre-0077 definers.
	tenant, store, principal := randomUUID(), randomUUID(), randomUUID()
	mustExec(t, owner, `INSERT INTO control.tenants(id,name) VALUES($1,'sls-upgrade')`, tenant)
	mustExec(t, owner, `INSERT INTO control.stores(tenant_id,id,name,currency) VALUES($1,$2,'sls-store','TWD')`, tenant, store)
	mustExec(t, owner, `INSERT INTO identity.principals(id) VALUES($1)`, principal)
	mustExec(t, owner, `INSERT INTO identity.memberships(tenant_id,principal_id) VALUES($1,$2)`, tenant, principal)
	mustExec(t, owner, `INSERT INTO identity.store_grants(tenant_id,store_id,principal_id,permission) SELECT $1,$2,$3,p FROM unnest(ARRAY['store:read','integration:manage','integration:read']) p`, tenant, store, principal)
	conn, binding, endpoint, qual := randomUUID(), randomUUID(), randomUUID(), randomUUID()
	mustExec(t, owner, `SELECT integration.register_stripe_account($1::uuid,$2::uuid,$3::uuid,$4::uuid,$5::uuid,'SANDBOX','acct_SlsUpg1','k1',$6::bytea,$7::bytea)`, tenant, store, principal, conn, binding, randomBytes(12), randomBytes(48))
	mustExec(t, owner, `SELECT payments.set_stripe_webhook_endpoint($1::uuid,$2::uuid,$3::uuid,$4::uuid,$5::uuid,'SANDBOX',0,true,'ks',$6::bytea,$7::bytea)`, tenant, store, principal, conn, endpoint, randomBytes(12), randomBytes(48))
	mustExec(t, owner, `SELECT payments.qualify_stripe_method($1::uuid,$2::uuid,$3::uuid,$4::uuid,$5::uuid,1,'PROVIDER_MOCK','provider-mock:sls',clock_timestamp()-interval '1 second',clock_timestamp()+interval '1 hour')`, tenant, store, principal, qual, conn)
	// Old-shape row digests (the new live_approval_id column is excluded from the qualification digest).
	digest := func() map[string]string {
		out := map[string]string{}
		for name, q := range map[string]string{
			"merchant_accounts": `SELECT count(*)||':'||coalesce(md5(string_agg(x::text,E'\n' ORDER BY x::text)),'') FROM integration.merchant_accounts x`,
			"credentials":       `SELECT count(*)||':'||coalesce(md5(string_agg(x::text,E'\n' ORDER BY x::text)),'') FROM integration.account_credentials x`,
			"endpoints":         `SELECT count(*)||':'||coalesce(md5(string_agg(x::text,E'\n' ORDER BY x::text)),'') FROM payments.stripe_webhook_endpoints x`,
			"qualifications":    `SELECT count(*)||':'||coalesce(md5(string_agg((to_jsonb(x)-'live_approval_id')::text,E'\n' ORDER BY (to_jsonb(x)-'live_approval_id')::text)),'') FROM payments.account_qualifications x`,
			"audit":             `SELECT count(*)||':'||coalesce(md5(string_agg(x::text,E'\n' ORDER BY x::text)),'') FROM ops.audit_events x`,
		} {
			var d string
			if err := owner.QueryRow(ctx, q).Scan(&d); err != nil {
				t.Fatal(err)
			}
			out[name] = d
		}
		return out
	}
	functions := func() map[string]string {
		rows, err := owner.Query(ctx, `SELECT n.nspname||'.'||p.proname||'('||pg_get_function_identity_arguments(p.oid)||')',
		  pg_get_userbyid(p.proowner)||'|'||p.prosecdef::text||'|'||coalesce(p.proconfig::text,'')||'|'||
		  coalesce((SELECT string_agg(CASE WHEN a.grantee=0 THEN 'PUBLIC' ELSE pg_get_userbyid(a.grantee) END||':'||a.privilege_type,',' ORDER BY CASE WHEN a.grantee=0 THEN 'PUBLIC' ELSE pg_get_userbyid(a.grantee) END,a.privilege_type)
		    FROM aclexplode(coalesce(p.proacl,acldefault('f',p.proowner))) a),'')
		  FROM pg_proc p JOIN pg_namespace n ON n.oid=p.pronamespace WHERE n.nspname IN ('payments','integration','checkout','identity','inventory','fulfillment')`)
		if err != nil {
			t.Fatal(err)
		}
		out := map[string]string{}
		for rows.Next() {
			var k, v string
			if err := rows.Scan(&k, &v); err != nil {
				t.Fatal(err)
			}
			out[k] = v
		}
		rows.Close()
		return out
	}
	before, beforeFn, pre := digest(), functions(), mciSnapshot(t, owner)
	var ledgerBefore int
	if err := owner.QueryRow(ctx, `SELECT count(*) FROM public.lc_schema_migrations`).Scan(&ledgerBefore); err != nil {
		t.Fatal(err)
	}
	if err := migrations.Apply(ctx, owner); err != nil {
		t.Fatalf("first Apply of the stripe-live migrations over the populated database: %v", err)
	}
	post1 := mciSnapshot(t, owner)
	if err := migrations.Apply(ctx, owner); err != nil {
		t.Fatalf("second Apply: %v", err)
	}
	post2 := mciSnapshot(t, owner)
	var ledgerAfter int
	if err := owner.QueryRow(ctx, `SELECT count(*) FROM public.lc_schema_migrations`).Scan(&ledgerAfter); err != nil {
		t.Fatal(err)
	}
	if ledgerAfter != ledgerBefore+2 {
		t.Fatalf("ledger grew by %d, want exactly 2 (0077 and post_river/0016, once)", ledgerAfter-ledgerBefore)
	}
	if extra := append(mciDiff(post1.acl, post2.acl), mciDiff(post2.acl, post1.acl)...); len(extra) != 0 {
		t.Fatalf("the second Apply changed privileges:\n  %s", mciHead(extra))
	}
	t.Run("existing rows are untouched", func(t *testing.T) {
		after := digest()
		for k, was := range before {
			if after[k] != was {
				t.Errorf("%s changed by the upgrade: %s -> %s", k, was, after[k])
			}
		}
	})
	t.Run("re-created functions keep signature, owner, search_path and ACL", func(t *testing.T) {
		after := functions()
		recreated := 0
		for k, was := range beforeFn {
			got, ok := after[k]
			if !ok {
				t.Errorf("function %s disappeared", k)
				continue
			}
			if got != was {
				t.Errorf("function %s changed owner/config/ACL:\n  before %s\n  after  %s", k, was, got)
			}
			recreated++
		}
		if recreated == 0 {
			t.Fatal("no function compared")
		}
		for _, name := range []string{"payments.approve_stripe_live(", "payments.record_stripe_live_canary(", "payments.revoke_stripe_live(", "payments.stripe_min_minor(", "identity.merchant_refund_environment("} {
			found := false
			for k := range after {
				if strings.HasPrefix(k, name) {
					found = true
				}
			}
			if !found {
				t.Errorf("new function %s missing after the upgrade", name)
			}
		}
	})
	t.Run("privilege delta equals section 3.3", func(t *testing.T) {
		if lost := mciDiff(pre.acl, post1.acl); len(lost) != 0 {
			t.Errorf("the upgrade removed privileges:\n  %s", mciHead(lost))
		}
		delta := mciDiff(post1.acl, pre.acl)
		allowedUpdate := map[string]bool{"canary_attempt_id": true, "canary_refund_id": true, "canary_verified_at": true, "revoked_at": true, "revoked_by": true, "revoke_ref": true}
		var bad []string
		sawApprovals, sawRevoke, sawFn := false, false, map[string]bool{}
		for _, k := range delta {
			parts := strings.Split(k, "|")
			role, kind, obj, priv := parts[0], parts[1], parts[2], parts[3]
			ok := false
			switch {
			case role == "commerce_payment_registry_writer" && kind == "table" && obj == "payments.stripe_live_approvals" && (priv == "SELECT" || priv == "INSERT"):
				ok, sawApprovals = true, true
			case role == "commerce_payment_registry_writer" && kind == "column" && strings.HasPrefix(obj, "payments.stripe_live_approvals.") && priv == "UPDATE" && allowedUpdate[strings.TrimPrefix(obj, "payments.stripe_live_approvals.")]:
				ok = true
			case role == "commerce_payment_registry_writer" && kind == "column" && obj == "payments.account_qualifications.revoked_at" && priv == "UPDATE":
				ok, sawRevoke = true, true
			case role == "commerce_payment_registry_writer" && priv == "SELECT" && (kind == "table" || kind == "column") &&
				(strings.HasPrefix(obj, "checkout.payment_attempts") || strings.HasPrefix(obj, "payments.facts") || strings.HasPrefix(obj, "payments.stripe_refunds") ||
					strings.HasPrefix(obj, "payments.refund_facts") || strings.HasPrefix(obj, "payments.review_cases") || strings.HasPrefix(obj, "payments.stripe_webhook_receipts")):
				ok = true // canary check read only; column-limited is tighter than the contract's table SELECT
			case role == "commerce_payment_registry_writer" && kind == "schema" && obj == "checkout" && priv == "USAGE":
				ok = true
			case kind == "exec" && role == "commerce_payment_registry_writer" && (obj == "payments.approve_stripe_live" || obj == "payments.record_stripe_live_canary" ||
				obj == "payments.revoke_stripe_live" || obj == "payments.guard_stripe_live_approval" || obj == "payments.account_qualification_revoke_only") && priv == "EXECUTE":
				ok = true // the function owner's implicit EXECUTE, not a grant
			case kind == "exec" && role == "commerce_auth" && obj == "identity.merchant_refund_environment" && priv == "EXECUTE":
				ok = true // owner (S5 definer)
			case role == "commerce_payment_registrar" && kind == "exec" && (obj == "payments.approve_stripe_live" || obj == "payments.record_stripe_live_canary" || obj == "payments.revoke_stripe_live") && priv == "EXECUTE":
				ok = true
				sawFn[obj] = true
			case role == "commerce_runtime" && kind == "exec" && obj == "identity.merchant_refund_environment" && priv == "EXECUTE":
				ok = true
				sawFn[obj] = true
			}
			if !ok {
				bad = append(bad, k)
			}
		}
		if len(bad) != 0 {
			t.Errorf("privileges gained beyond contract §3.3:\n  %s", mciHead(bad))
		}
		if !sawApprovals || !sawRevoke || len(sawFn) != 4 {
			t.Errorf("expected grants missing: approvals=%v revoke=%v functions=%v", sawApprovals, sawRevoke, sawFn)
		}
	})
	t.Run("the upgraded database behaves", func(t *testing.T) {
		// SANDBOX still qualifies; a LIVE qualification without approval is still impossible (SP19); the new table is empty.
		if _, err := owner.Exec(ctx, `SELECT payments.qualify_stripe_method($1::uuid,$2::uuid,$3::uuid,$4::uuid,$5::uuid,1,'PROVIDER_MOCK','provider-mock:sls2',clock_timestamp()-interval '1 second',clock_timestamp()+interval '1 hour')`,
			tenant, store, principal, randomUUID(), conn); err != nil {
			t.Fatalf("SANDBOX/PROVIDER_MOCK qualification after the upgrade: %v", err)
		}
		_, err := owner.Exec(ctx, `INSERT INTO payments.account_qualifications(id,tenant_id,store_id,connection_id,credential_version,environment,code,proof_class,evidence_ref,observed_at,expires_at)
		 VALUES(gen_random_uuid(),$1,$2,$3,1,'LIVE','stripe_checkout','REAL_LIVE','x',now()-interval '1 second',now()+interval '1 hour')`, tenant, store, conn)
		slrRejected(t, "REAL_LIVE without approval after the upgrade", err)
		var n int
		if err := owner.QueryRow(ctx, `SELECT count(*) FROM payments.stripe_live_approvals`).Scan(&n); err != nil || n != 0 {
			t.Fatalf("approvals after upgrade: %d %v", n, err)
		}
		if got := func() int {
			var c int
			_ = owner.QueryRow(ctx, `SELECT count(*) FROM public.lc_schema_migrations WHERE version IN ('0077_stripe_live_enable.sql','post_river/0016_stripe_live.sql') AND checksum<>''`).Scan(&c)
			return c
		}(); got != 2 {
			t.Fatalf("ledger rows with a checksum: %d", got)
		}
	})
}
