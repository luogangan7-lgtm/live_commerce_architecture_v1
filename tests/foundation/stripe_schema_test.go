// stripe_schema_test.go owns independent REAL_PG checks for the frozen Stripe schema.
// It never substitutes catalog text for a provider or browser acceptance result.
package foundation_test

import (
	"context"
	"crypto/sha256"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"livecommerce/internal/integrations/psp/stripe"
)

func stripeCatalogBool(t *testing.T, p *pgxpool.Pool, q string, args ...any) bool {
	t.Helper()
	var got bool
	if err := p.QueryRow(context.Background(), q, args...).Scan(&got); err != nil {
		t.Fatal(err)
	}
	return got
}

func stripeMustCatalog(t *testing.T, p *pgxpool.Pool, label, q string, args ...any) {
	t.Helper()
	if !stripeCatalogBool(t, p, q, args...) {
		t.Fatalf("SP06 missing schema requirement: %s", label)
	}
}

func stripeCheckDef(t *testing.T, p *pgxpool.Pool, table string, fragments ...string) {
	t.Helper()
	var defs string
	err := p.QueryRow(context.Background(), `SELECT coalesce(string_agg(pg_get_constraintdef(oid),' | '),'')
	 FROM pg_constraint WHERE conrelid=to_regclass($1) AND contype='c'`, table).Scan(&defs)
	if err != nil {
		t.Fatal(err)
	}
	defs = strings.ToLower(defs)
	for _, frag := range fragments {
		if !strings.Contains(defs, strings.ToLower(frag)) {
			t.Errorf("SP06 %s CHECK missing %q", table, frag)
		}
	}
}

func stripeSQLState(err error, code string) bool {
	var e *pgconn.PgError
	return errors.As(err, &e) && e.Code == code
}

func stripeExpectCheck(t *testing.T, p *pgxpool.Pool, table, columns, validValues, invalidValues string) {
	t.Helper()
	ctx := context.Background()
	tx, err := p.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	// LIKE copies NOT NULL, DEFAULT and CHECK, but not foreign keys or row triggers.
	// The valid row proves the fixture can reach the intended negative CHECK.
	if _, err = tx.Exec(ctx, `CREATE TEMP TABLE stripe_check_case (LIKE `+table+` INCLUDING DEFAULTS INCLUDING CONSTRAINTS)`); err != nil {
		t.Fatal(err)
	}
	if _, err = tx.Exec(ctx, `INSERT INTO stripe_check_case (`+columns+`) VALUES (`+validValues+`)`); err != nil {
		t.Fatalf("SP06 %s positive CHECK baseline failed: %v", table, err)
	}
	_, err = tx.Exec(ctx, `INSERT INTO stripe_check_case (`+columns+`) VALUES (`+invalidValues+`)`)
	if !stripeSQLState(err, "23514") {
		t.Fatalf("SP06 %s negative CHECK accepted or failed for wrong reason: %v", table, err)
	}
}

// TestStripeSP02Currency is the SQL half of SP02. The §0.1 ruling overrides
// stale JPY/one-account draft rows: five currencies, no conversion or rounding.
func TestStripeSP02Currency(t *testing.T) {
	f := fixture(t)
	for _, v := range []struct {
		name, currency string
		amount         int64
		ok             bool
	}{
		{"HKD minimum", "HKD", 400, true}, {"HKD below", "HKD", 399, false},
		{"USD minimum", "USD", 50, true}, {"USD below", "USD", 49, false},
		{"SGD minimum", "SGD", 50, true}, {"SGD below", "SGD", 49, false},
		{"MYR minimum", "MYR", 200, true}, {"MYR below", "MYR", 199, false},
		// TWD min 2500: Stripe SANDBOX rejected 100/1200, accepted 2500 (2026-09-29)
		{"TWD minimum", "TWD", 2500, true}, {"TWD below", "TWD", 2499, false}, {"TWD old min", "TWD", 100, false}, {"TWD step", "TWD", 2501, false},
		{"TWD maximum", "TWD", 99999900, true}, {"TWD overflow", "TWD", 100000000, false},
		{"USD maximum", "USD", 99999999, true}, {"USD overflow", "USD", 100000000, false},
		{"JPY denied", "JPY", 500, false}, {"ISK denied", "ISK", 500, false},
		{"UGX denied", "UGX", 500, false}, {"HUF denied", "HUF", 500, false},
		{"BHD denied", "BHD", 500, false}, {"lowercase denied", "usd", 500, false},
		{"zero denied", "USD", 0, false}, {"negative denied", "USD", -1, false},
	} {
		t.Run(v.name, func(t *testing.T) {
			// I05: SQL admission and Go wire encoding must agree exactly on the charge.
			unit, goErr := stripe.UnitAmount(v.currency, v.amount)
			var sqlOK bool
			var sqlUnit *int64
			err := f.owner.QueryRow(context.Background(), `SELECT payments.stripe_amount_ok($1,$2),payments.stripe_unit_amount($1,$2)`, v.currency, v.amount).Scan(&sqlOK, &sqlUnit)
			if err != nil {
				t.Fatal(err)
			}
			if (goErr == nil) != v.ok || sqlOK != v.ok {
				t.Fatalf("Go/SQL admission: go=%v sql=%v want=%v", goErr, sqlOK, v.ok)
			}
			if v.ok {
				if sqlUnit == nil || *sqlUnit != unit || unit != v.amount {
					t.Fatalf("unit amount: Go=%d SQL=%v", unit, sqlUnit)
				}
			} else if sqlUnit != nil {
				t.Fatalf("rejected amount converted to %d", *sqlUnit)
			}
		})
	}
}

// TestStripeSP06Schema checks the migration's structural and privilege gates.
// The fixture applies migrations twice, catching repeat/checksum drift.
func TestStripeSP06Schema(t *testing.T) {
	f := fixture(t)
	p := f.owner
	for _, v := range []string{"0061_stripe_psp.sql", "post_river/0012_stripe_payment.sql"} {
		stripeMustCatalog(t, p, "migration "+v, `SELECT EXISTS(SELECT 1 FROM public.lc_schema_migrations WHERE version=$1 AND checksum<>'')`, v)
	}
	for _, table := range []string{"payments.stripe_sessions", "payments.stripe_webhook_receipts", "payments.stripe_signals", "payments.stripe_webhook_endpoints"} {
		stripeMustCatalog(t, p, table+" FORCE RLS", `SELECT coalesce((SELECT relrowsecurity AND relforcerowsecurity FROM pg_class WHERE oid=to_regclass($1)),false)`, table)
		stripeMustCatalog(t, p, table+" direct privilege refused to PUBLIC", `SELECT NOT EXISTS (SELECT 1 FROM pg_class c CROSS JOIN LATERAL aclexplode(COALESCE(c.relacl,acldefault('r',c.relowner))) acl WHERE c.oid=to_regclass($1) AND acl.grantee=0 AND acl.privilege_type IN ('SELECT','INSERT','UPDATE'))`, table)
		for _, role := range []string{"commerce_runtime", "commerce_worker"} {
			stripeMustCatalog(t, p, table+" direct privilege refused to "+role, `SELECT NOT has_table_privilege($1,$2,'SELECT') AND NOT has_table_privilege($1,$2,'INSERT') AND NOT has_table_privilege($1,$2,'UPDATE')`, role, table)
		}
	}
	stripeCheckDef(t, p, "integration.merchant_accounts", "stripe", "acct_")
	stripeMustCatalog(t, p, "PAYUNi credential ciphertext remains non-null", `SELECT bool_and(attnotnull) FROM pg_attribute WHERE attrelid='integration.account_credentials'::regclass AND attname IN ('nonce','ciphertext')`)
	stripeMustCatalog(t, p, "no obsolete ENV_PLATFORM custody", `SELECT NOT EXISTS(SELECT 1 FROM pg_attribute WHERE attrelid='integration.account_credentials'::regclass AND attname IN ('custody','key_fingerprint') AND NOT attisdropped)`)
	stripeCheckDef(t, p, "payments.method_versions", "stripe_checkout", "SGD", "MYR", "TWD")
	stripeCheckDef(t, p, "payments.account_qualifications", "stripe_checkout", "REAL_LIVE")
	stripeCheckDef(t, p, "checkout.payment_attempts", "stripe_checkout", "stripe_amount_ok")
	stripeCheckDef(t, p, "integration.operations", "BUYER_PAYMENT_QUERY", "stripe.checkout_session", "MEDIA_ATTEMPT")
	stripeCheckDef(t, p, "payments.provider_observations", "LOCAL", "QUERY", "stripe")
	stripeCheckDef(t, p, "payments.facts", "CLOSED_UNPAID", "amount_minor", "currency", "provider_reference")
	stripeCheckDef(t, p, "payments.review_cases", "PROVIDER_AMOUNT_MISMATCH", "CLOSURE_CONTRADICTED")
	stripeCheckDef(t, p, "inventory.ledger", "CLOSED_UNPAID", "SYSTEM_PAYMENT", "RELEASE")
	stripeCheckDef(t, p, "checkout.events", "checkout.payment_closed")
	stripeCheckDef(t, p, "payments.stripe_sessions", "session_id", "create_body_sha256")
	stripeCheckDef(t, p, "payments.stripe_webhook_receipts", "MALFORMED", "ACCEPTED", "signal_id")
	stripeCheckDef(t, p, "payments.stripe_signals", "STRIPE_WEBHOOK", "BUYER_REFRESH", "BUYER_CANCEL", "consumed_at")
	stripeCheckDef(t, p, "payments.stripe_webhook_endpoints", "key_version", "nonce", "ciphertext")
	for _, v := range []struct{ table, cols, valid, invalid string }{
		{"integration.merchant_accounts", "id,tenant_id,store_id,principal_id,provider,environment,account_id,binding_id,credential_version", "gen_random_uuid(),gen_random_uuid(),gen_random_uuid(),gen_random_uuid(),'stripe','SANDBOX','acct_TestStoreA1',gen_random_uuid(),1", "gen_random_uuid(),gen_random_uuid(),gen_random_uuid(),gen_random_uuid(),'stripe','SANDBOX','bad',gen_random_uuid(),1"},
		{"integration.account_credentials", "tenant_id,store_id,connection_id,version,key_id,principal_id,nonce,ciphertext", "gen_random_uuid(),gen_random_uuid(),gen_random_uuid(),1,'test',gen_random_uuid(),decode(repeat('aa',12),'hex'),decode(repeat('aa',48),'hex')", "gen_random_uuid(),gen_random_uuid(),gen_random_uuid(),1,'test',gen_random_uuid(),decode('aa','hex'),decode(repeat('aa',48),'hex')"},
		{"payments.account_qualifications", "id,tenant_id,store_id,connection_id,credential_version,environment,code,proof_class,evidence_ref,observed_at,expires_at", "gen_random_uuid(),gen_random_uuid(),gen_random_uuid(),gen_random_uuid(),1,'SANDBOX','stripe_checkout','PROVIDER_MOCK','test',now()-interval '1 second',now()+interval '1 hour'", "gen_random_uuid(),gen_random_uuid(),gen_random_uuid(),gen_random_uuid(),1,'SANDBOX','stripe_checkout','REAL_LIVE','test',now()-interval '1 second',now()+interval '1 hour'"},
		{"payments.facts", "tenant_id,store_id,attempt_id,kind,amount_minor,currency,provider_reference,connection_id,execution_profile,environment,source_report_hash", "gen_random_uuid(),gen_random_uuid(),gen_random_uuid(),'CLOSED_UNPAID',0,'USD','cs_test_fake',gen_random_uuid(),'PROVIDER_MOCK','SANDBOX',decode(repeat('aa',32),'hex')", "gen_random_uuid(),gen_random_uuid(),gen_random_uuid(),'CLOSED_UNPAID',1,'USD','cs_test_fake',gen_random_uuid(),'PROVIDER_MOCK','SANDBOX',decode(repeat('aa',32),'hex')"},
		{"payments.review_cases", "tenant_id,store_id,attempt_id,reason,source_report_hash", "gen_random_uuid(),gen_random_uuid(),gen_random_uuid(),'PROVIDER_AMOUNT_MISMATCH',decode(repeat('aa',32),'hex')", "gen_random_uuid(),gen_random_uuid(),gen_random_uuid(),'UNLISTED',decode(repeat('aa',32),'hex')"},
	} {
		t.Run("negative/"+v.table, func(t *testing.T) { stripeExpectCheck(t, p, v.table, v.cols, v.valid, v.invalid) })
	}
	// PostgreSQL normalizes interval literals in pg_get_constraintdef. Exercise
	// the three frozen deadline equations rather than matching their SQL spelling.
	at := "'2026-01-01 00:00:00.456+00'::timestamptz"
	expires := "date_trunc('second'," + at + ")+interval '40 minutes'"
	send := at + "+interval '7 minutes'"
	cutoff := "(" + expires + ")-interval '5 minutes'"
	deadlineColumns := "tenant_id,store_id,owner_id,attempt_id,environment,account_id,locale,config_digest,unit_amount,create_params,attempt_created_at,expires_at,send_deadline,handoff_cutoff"
	deadlineValues := func(expireValue, sendValue, cutoffValue string) string {
		return "gen_random_uuid(),gen_random_uuid(),gen_random_uuid(),gen_random_uuid(),'SANDBOX','acct_TestStoreA1','en',decode(repeat('aa',32),'hex'),50,'{}'::jsonb," + at + "," + expireValue + "," + sendValue + "," + cutoffValue
	}
	for _, v := range []struct{ name, expire, send, cutoff string }{
		{"expires_at 40 minutes", "(" + expires + ")+interval '1 second'", send, cutoff},
		{"send_deadline 7 minutes", expires, "(" + send + ")+interval '1 second'", cutoff},
		{"handoff_cutoff 5 minutes", expires, send, "(" + cutoff + ")+interval '1 second'"},
	} {
		t.Run("deadline/"+v.name, func(t *testing.T) {
			stripeExpectCheck(t, p, "payments.stripe_sessions", deadlineColumns,
				deadlineValues(expires, send, cutoff), deadlineValues(v.expire, v.send, v.cutoff))
		})
	}
	// The revised §0.1 index must allow separate accounts in one environment,
	// while prohibiting one account being bound to two stores.
	stripeMustCatalog(t, p, "one Stripe account per store/environment", `SELECT EXISTS(SELECT 1 FROM pg_indexes WHERE schemaname='integration' AND tablename='merchant_accounts' AND indexdef ILIKE '%UNIQUE%' AND indexdef ILIKE '%(tenant_id, store_id, environment)%' AND indexdef ILIKE '%provider%stripe%')`)
	stripeMustCatalog(t, p, "one store per Stripe account/environment", `SELECT EXISTS(SELECT 1 FROM pg_indexes WHERE schemaname='integration' AND tablename='merchant_accounts' AND indexdef ILIKE '%UNIQUE%' AND indexdef ILIKE '%(environment, account_id)%' AND indexdef ILIKE '%provider%stripe%')`)
	stripeMustCatalog(t, p, "merchant Stripe account policy restrictive", `SELECT EXISTS(SELECT 1 FROM pg_policy WHERE polrelid='integration.merchant_accounts'::regclass AND polpermissive=false AND (pg_get_expr(polwithcheck,polrelid) ILIKE '%payuni%' OR pg_get_expr(polqual,polrelid) ILIKE '%payuni%'))`)
	stripeMustCatalog(t, p, "merchant credential insert restricted to PAYUNi", `SELECT EXISTS(SELECT 1 FROM pg_policy WHERE polrelid='integration.account_credentials'::regclass AND polpermissive=false AND pg_get_expr(polwithcheck,polrelid) ILIKE '%payuni%')`)
	for _, c := range []string{"create_first_sent_at", "create_last_sent_at", "create_send_count", "create_body_sha256", "create_suppressed_at", "session_id", "session_url", "payment_intent_id", "pinned_at", "url_purged_at", "expire_calls", "last_expire_at"} {
		stripeMustCatalog(t, p, "integration writer stripe_sessions UPDATE("+c+")", `SELECT has_column_privilege('commerce_integration_writer','payments.stripe_sessions',$1,'UPDATE') AND NOT has_column_privilege('commerce_runtime','payments.stripe_sessions',$1,'UPDATE')`, c)
	}
	for _, c := range []string{"first_handed_out_at", "cancel_requested_at", "refresh_count", "last_refresh_at", "signal_count"} {
		stripeMustCatalog(t, p, "checkout writer stripe_sessions UPDATE("+c+")", `SELECT has_column_privilege('commerce_checkout_writer','payments.stripe_sessions',$1,'UPDATE') AND NOT has_column_privilege('commerce_runtime','payments.stripe_sessions',$1,'UPDATE')`, c)
	}
	for _, sig := range []string{"integration.load_stripe_session(uuid,bigint,bytea,text)", "integration.mark_stripe_create_sent(uuid,bigint,bytea,text,bytea)", "integration.note_stripe_expire(uuid,bigint,bytea,text)", "integration.record_stripe_observation(uuid,bigint,bytea,text,jsonb,bigint,text)", "integration.finish_stripe_query(uuid,bigint,bytea,text,text)", "integration.consume_stripe_signal(uuid,uuid,bigint,bytea,text,text)", "payments.apply_capture(uuid,bytea)", "payments.apply_capture_payuni_v1(uuid,bytea)", "payments.apply_stripe_observation(uuid,bytea)"} {
		stripeMustCatalog(t, p, "definer/ACL "+sig, `SELECT EXISTS(SELECT 1 FROM pg_proc p JOIN pg_roles r ON r.oid=p.proowner WHERE p.oid=to_regprocedure($1) AND p.prosecdef AND p.proconfig @> ARRAY['search_path=pg_catalog']::text[] AND NOT r.rolcanlogin AND NOT EXISTS (SELECT 1 FROM aclexplode(COALESCE(p.proacl,acldefault('f',p.proowner))) acl WHERE acl.grantee=0 AND acl.privilege_type='EXECUTE'))`, sig)
	}
	for _, name := range []string{"guard_payment_job_family", "payment_job_queue", "route_payment_queue_v1", "payment_queue_ready", "reject_legacy_family_job"} {
		stripeMustCatalog(t, p, "post-River "+name+" signal family", `SELECT EXISTS(SELECT 1 FROM pg_proc WHERE pronamespace='integration'::regnamespace AND proname=$1 AND pg_get_functiondef(oid) LIKE '%payment_signal_v1%')`, name)
	}
	stripeMustCatalog(t, p, "payment queue readiness", `SELECT integration.payment_queue_ready()`)
	// Body identity, not merely a renamed symbol: the 0018 PAYUNi function must
	// retain its original normalized PL/pgSQL body after dispatch is installed.
	var oldBody, newBody string
	if err := p.QueryRow(context.Background(), `SELECT prosrc FROM pg_proc WHERE oid='payments.apply_capture_payuni_v1(uuid,bytea)'::regprocedure`).Scan(&oldBody); err != nil {
		t.Fatal(err)
	}
	if err := p.QueryRow(context.Background(), `SELECT prosrc FROM pg_proc WHERE oid='payments.apply_capture(uuid,bytea)'::regprocedure`).Scan(&newBody); err != nil {
		t.Fatal(err)
	}
	source, err := os.ReadFile("../../migrations/0018_payment_capture.sql")
	if err != nil {
		t.Fatal(err)
	}
	begin := strings.Index(string(source), "CREATE FUNCTION payments.apply_capture(")
	if begin < 0 {
		t.Fatal("0018 PAYUNi source body absent")
	}
	fragment := string(source)[begin:]
	start := strings.Index(fragment, "AS $$")
	if start < 0 {
		t.Fatal("0018 PAYUNi source body opener absent")
	}
	fragment = fragment[start+len("AS $$"):]
	end := strings.Index(fragment, "$$;")
	if end < 0 {
		t.Fatal("0018 PAYUNi source body terminator absent")
	}
	wantHash, gotHash := sha256.Sum256([]byte(fragment[:end])), sha256.Sum256([]byte(oldBody))
	if oldBody == "" || wantHash != gotHash || oldBody == newBody || !strings.Contains(newBody, "apply_capture_payuni_v1") {
		t.Fatal("PAYUNi body was not preserved behind Stripe dispatcher")
	}
}

func TestStripeSP19LiveRefusal(t *testing.T) {
	f := fixture(t)
	// SQL tier only: configuration and CLI refusal are separate UNIT gates.
	stripeExpectCheck(t, f.owner, "payments.account_qualifications",
		"id,tenant_id,store_id,connection_id,credential_version,environment,code,proof_class,evidence_ref,observed_at,expires_at",
		"gen_random_uuid(),gen_random_uuid(),gen_random_uuid(),gen_random_uuid(),1,'SANDBOX','stripe_checkout','PROVIDER_MOCK','test',now()-interval '1 second',now()+interval '1 hour'",
		"gen_random_uuid(),gen_random_uuid(),gen_random_uuid(),gen_random_uuid(),1,'LIVE','stripe_checkout','REAL_LIVE','test',now(),now()+interval '1 hour'")
}

// stripeRegistrarFixture exercises the operator SQL API with real NOLOGIN
// membership, not the migration owner. The existing T06 fixture owns synthetic
// tenants and tears them down; Stripe additions are removed first.
type stripeRegistrarFixture struct {
	seed                                 *t06GoFixture
	other                                *t06GoFixture
	registrar, ingress, writer, merchant *pgxpool.Pool
	accountA, accountB, endpointA        string
}

func stripeRegistrarSetup(t *testing.T) *stripeRegistrarFixture {
	t.Helper()
	a := newT06GoFixture(t)
	b := newT06GoFixture(t)
	h := &stripeRegistrarFixture{seed: a, other: b, accountA: randomUUID(), accountB: randomUUID(), endpointA: randomUUID()}
	// Registrar-created rows can reference the T06 tenant; remove them before
	// the seed fixture's tenant/membership cleanup. Every query is scoped to the
	// synthetic tenant IDs, never to the shared suite as a whole.
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		tx, err := a.base.owner.Begin(ctx)
		if err != nil {
			t.Error(err)
			return
		}
		defer tx.Rollback(ctx)
		for _, q := range []string{
			`DELETE FROM payments.stripe_webhook_receipts WHERE tenant_id IN ($1,$2)`,
			`DELETE FROM payments.stripe_webhook_endpoints WHERE tenant_id IN ($1,$2)`,
			`DELETE FROM integration.account_credentials WHERE tenant_id IN ($1,$2) AND connection_id IN (SELECT id FROM integration.merchant_accounts WHERE provider='stripe')`,
			`DELETE FROM integration.merchant_accounts WHERE tenant_id IN ($1,$2) AND provider='stripe'`,
			`DELETE FROM integration.bindings WHERE tenant_id IN ($1,$2) AND provider='stripe'`,
		} {
			if _, err = tx.Exec(ctx, q, a.tenant, b.tenant); err != nil {
				t.Errorf("Stripe fixture teardown: %v", err)
				return
			}
		}
		if err = tx.Commit(ctx); err != nil {
			t.Errorf("Stripe fixture teardown commit: %v", err)
		}
	})
	for _, v := range []struct {
		group string
		dst   **pgxpool.Pool
	}{
		{"commerce_payment_registrar", &h.registrar},
		{"commerce_stripe_ingress", &h.ingress},
		{"commerce_integration_writer", &h.writer},
		{"commerce_runtime", &h.merchant},
	} {
		_, *v.dst = lmaLogin(t, a.base, v.group)
	}
	return h
}

func (h *stripeRegistrarFixture) register(t *testing.T, tenant, store, principal, connection, binding, account string) error {
	t.Helper()
	var got string
	// SQL owns only ciphertext. These random bytes are disposable fixtures and
	// are never decrypted or treated as a real Stripe credential.
	err := h.registrar.QueryRow(context.Background(), `SELECT integration.register_stripe_account(
	 $1::uuid,$2::uuid,$3::uuid,$4::uuid,$5::uuid,'SANDBOX',$6,'fixture_key',$7::bytea,$8::bytea)`,
		tenant, store, principal, connection, binding, account, randomBytes(12), randomBytes(48)).Scan(&got)
	if err == nil && got == "" {
		t.Fatal("registrar returned empty UUID")
	}
	return err
}

func (h *stripeRegistrarFixture) endpoint(t *testing.T, tenant, store, principal, connection, endpoint, profile string, expected int64, enabled bool) (int64, error) {
	t.Helper()
	var got int64
	err := h.registrar.QueryRow(context.Background(), `SELECT payments.set_stripe_webhook_endpoint(
	 $1::uuid,$2::uuid,$3::uuid,$4::uuid,$5::uuid,$6,$7,$8,'fixture_signing_key',$9::bytea,$10::bytea)`,
		tenant, store, principal, connection, endpoint, profile, expected, enabled, randomBytes(12), randomBytes(48)).Scan(&got)
	return got, err
}

func TestStripeSP06RegistrarIsolation(t *testing.T) {
	h := stripeRegistrarSetup(t)
	a, b := h.seed, h.other
	thirdStore := randomUUID()
	mustExec(t, a.base.owner, `INSERT INTO control.stores(tenant_id,id,name,currency) VALUES($1,$2,'Stripe forged-owner target','USD')`, a.tenant, thirdStore)
	if err := h.register(t, a.tenant, a.store, a.principal, h.accountA, randomUUID(), "acct_TestStoreA1"); err != nil {
		t.Fatal(err)
	}
	if err := h.register(t, a.tenant, a.otherStore, a.principal, h.accountB, randomUUID(), "acct_TestStoreB1"); err != nil {
		t.Fatalf("distinct accounts in same environment: %v", err)
	}
	for _, v := range []struct{ name, tenant, store, principal, account string }{
		{"second_account_same_store", a.tenant, a.store, a.principal, "acct_TestStoreC1"},
		{"same_account_other_store", b.tenant, b.store, b.principal, "acct_TestStoreA1"},
		{"foreign_principal", a.tenant, thirdStore, b.principal, "acct_TestStoreD1"},
		{"foreign_tenant_store_pair", b.tenant, thirdStore, b.principal, "acct_TestStoreE1"},
	} {
		t.Run(v.name, func(t *testing.T) {
			if err := h.register(t, v.tenant, v.store, v.principal, randomUUID(), randomUUID(), v.account); err == nil {
				t.Fatal("registrar accepted forbidden account/scope")
			}
		})
	}
	var rows int
	if err := a.base.owner.QueryRow(context.Background(), `SELECT count(*) FROM integration.merchant_accounts WHERE tenant_id=$1 AND provider='stripe'`, a.tenant).Scan(&rows); err != nil || rows != 2 {
		t.Fatalf("failed registration partially persisted: rows=%d err=%v", rows, err)
	}
	// Frozen version 1 remains byte-identical after rotation; a stale CAS
	// cannot append version 3. The head changes, not historical attempt data.
	var oldNonce, oldCipher []byte
	if err := a.base.owner.QueryRow(context.Background(), `SELECT nonce,ciphertext FROM integration.account_credentials WHERE connection_id=$1 AND version=1`, h.accountA).Scan(&oldNonce, &oldCipher); err != nil {
		t.Fatal(err)
	}
	var version int64
	err := h.registrar.QueryRow(context.Background(), `SELECT integration.rotate_stripe_key($1::uuid,$2::uuid,$3::uuid,$4::uuid,1,'fixture_key_v2',$5::bytea,$6::bytea)`, a.tenant, a.store, a.principal, h.accountA, randomBytes(12), randomBytes(48)).Scan(&version)
	if err != nil || version != 2 {
		t.Fatalf("rotation: version=%d err=%v", version, err)
	}
	var gotNonce, gotCipher []byte
	if err := a.base.owner.QueryRow(context.Background(), `SELECT nonce,ciphertext FROM integration.account_credentials WHERE connection_id=$1 AND version=1`, h.accountA).Scan(&gotNonce, &gotCipher); err != nil {
		t.Fatal(err)
	}
	if string(gotNonce) != string(oldNonce) || string(gotCipher) != string(oldCipher) {
		t.Fatal("rotation rewrote historical credential version")
	}
	if err := h.registrar.QueryRow(context.Background(), `SELECT integration.rotate_stripe_key($1::uuid,$2::uuid,$3::uuid,$4::uuid,1,'stale',$5::bytea,$6::bytea)`, a.tenant, a.store, a.principal, h.accountA, randomBytes(12), randomBytes(48)).Scan(&version); err == nil {
		t.Fatal("stale rotation CAS appended a version")
	}
	if err := a.base.owner.QueryRow(context.Background(), `SELECT count(*) FROM integration.account_credentials WHERE connection_id=$1`, h.accountA).Scan(&rows); err != nil || rows != 2 {
		t.Fatalf("rotation row count=%d err=%v", rows, err)
	}
	// A probe started under version 1 cannot qualify the rotated version 2.
	proof := randomUUID()
	observed := time.Now().UTC().Add(-time.Minute)
	var qualified string
	if err := h.registrar.QueryRow(context.Background(), `SELECT payments.qualify_stripe_method(
	 $1::uuid,$2::uuid,$3::uuid,$4::uuid,$5::uuid,1,'PROVIDER_MOCK',
	 'synthetic-stale-key-probe',$6::timestamptz,$7::timestamptz)`,
		a.tenant, a.store, a.principal, proof, h.accountA, observed, observed.Add(time.Hour)).Scan(&qualified); err == nil {
		t.Fatal("stale-key provider probe qualified rotated credential")
	}
	if err := a.base.owner.QueryRow(context.Background(), `SELECT count(*) FROM payments.account_qualifications WHERE id=$1`, proof).Scan(&rows); err != nil || rows != 0 {
		t.Fatalf("stale probe partially persisted: rows=%d err=%v", rows, err)
	}
}

func TestStripeSP06EndpointCustodyAndRLS(t *testing.T) {
	h := stripeRegistrarSetup(t)
	a := h.seed
	if err := h.register(t, a.tenant, a.store, a.principal, h.accountA, randomUUID(), "acct_TestStoreA1"); err != nil {
		t.Fatal(err)
	}
	if err := h.register(t, a.tenant, a.otherStore, a.principal, h.accountB, randomUUID(), "acct_TestStoreB1"); err != nil {
		t.Fatal(err)
	}
	if version, err := h.endpoint(t, a.tenant, a.store, a.principal, h.accountA, h.endpointA, "PROVIDER_MOCK", 0, true); err != nil || version != 1 {
		t.Fatalf("endpoint create=%d err=%v", version, err)
	}
	var tenant, store, account, profile string
	var keyVersion int64
	read := func() error {
		return h.ingress.QueryRow(context.Background(), `SELECT tenant_id::text,store_id::text,account_id,execution_profile,key_version FROM payments.stripe_webhook_material($1::uuid)`, h.endpointA).Scan(&tenant, &store, &account, &profile, &keyVersion)
	}
	if err := read(); err != nil || tenant != a.tenant || store != a.store || account != "acct_TestStoreA1" || profile != "PROVIDER_MOCK" || keyVersion != 1 {
		t.Fatalf("ingress material scope mismatch: %v", err)
	}
	if _, err := h.endpoint(t, a.tenant, a.otherStore, a.principal, h.accountB, h.endpointA, "PROVIDER_MOCK", 1, true); err == nil {
		t.Fatal("endpoint identity rebound to another store/account")
	}
	// A binding disable stops new sales, not historical webhook material.
	mustExec(t, a.base.owner, `UPDATE integration.bindings SET enabled=false WHERE id=(SELECT binding_id FROM integration.merchant_accounts WHERE id=$1)`, h.accountA)
	if err := read(); err != nil || account != "acct_TestStoreA1" {
		t.Fatalf("binding disable suppressed historical material: %v", err)
	}
	if version, err := h.endpoint(t, a.tenant, a.store, a.principal, h.accountA, h.endpointA, "PROVIDER_MOCK", 1, false); err != nil || version != 2 {
		t.Fatalf("endpoint disable=%d err=%v", version, err)
	}
	if err := read(); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("disabled endpoint yielded material: %v", err)
	}
	if version, err := h.endpoint(t, a.tenant, a.store, a.principal, h.accountA, h.endpointA, "PROVIDER_MOCK", 2, true); err != nil || version != 3 {
		t.Fatalf("endpoint re-enable=%d err=%v", version, err)
	}
	if err := read(); err != nil || keyVersion != 3 {
		t.Fatalf("historical endpoint version: %v", err)
	}
	// Effective authority: the integration writer may acquire the FOR SHARE
	// lock required by prepare, but RLS WITH CHECK(false) blocks even a no-op
	// endpoint mutation. An ordinary merchant cannot read signing ciphertext.
	ctx := context.Background()
	tx, err := h.writer.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	var seen string
	if err := tx.QueryRow(ctx, `SELECT endpoint_id::text FROM payments.stripe_webhook_endpoints WHERE endpoint_id=$1::uuid FOR SHARE`, h.endpointA).Scan(&seen); err != nil || seen != h.endpointA {
		t.Fatalf("writer FOR SHARE denied: %v", err)
	}
	if _, err := tx.Exec(ctx, `UPDATE payments.stripe_webhook_endpoints SET endpoint_id=endpoint_id WHERE endpoint_id=$1::uuid`, h.endpointA); !stripeSQLState(err, "42501") {
		t.Fatalf("writer endpoint UPDATE not blocked by RLS: %v", err)
	}
	if _, err := h.merchant.Exec(ctx, `SELECT ciphertext FROM payments.stripe_webhook_endpoints WHERE endpoint_id=$1::uuid`, h.endpointA); !stripeSQLState(err, "42501") {
		t.Fatalf("merchant read signing material: %v", err)
	}
	if _, err := h.registrar.Exec(ctx, `UPDATE integration.account_credentials SET ciphertext=$2 WHERE connection_id=$1::uuid AND version=1`, h.accountA, randomBytes(48)); !stripeSQLState(err, "42501") {
		t.Fatalf("registrar direct historical credential UPDATE: %v", err)
	}
}

func TestStripeSP13EndpointPrepareLocksRotation(t *testing.T) {
	h := stripeRegistrarSetup(t)
	a := h.seed
	if err := h.register(t, a.tenant, a.store, a.principal, h.accountA, randomUUID(), "acct_TestStoreA1"); err != nil {
		t.Fatal(err)
	}
	if version, err := h.endpoint(t, a.tenant, a.store, a.principal, h.accountA, h.endpointA, "PROVIDER_MOCK", 0, true); err != nil || version != 1 {
		t.Fatalf("endpoint create=%d err=%v", version, err)
	}
	ctx := context.Background()
	tx, err := h.ingress.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	var disposition string
	attempt := randomUUID()
	err = tx.QueryRow(ctx, `SELECT disposition FROM payments.stripe_webhook_prepare(
	 $1::uuid,1,$2,'checkout.session.completed',$3::bigint,'2026-08-26.dahlia',
	 'checkout.session','cs_test_missing',$4,$4,false,false,false,false,$5::bytea,$3::bigint)`,
		h.endpointA, "evt_stripe_lock_"+t04Tag(), time.Now().Unix(), attempt, randomBytes(32)).Scan(&disposition)
	if err != nil {
		t.Fatalf("prepare failed before endpoint lock: %v", err)
	}
	if disposition == "ACCEPT_PENDING" {
		t.Fatal("unknown session admitted as accepted")
	}
	rotationTx, err := h.registrar.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer rotationTx.Rollback(ctx)
	if _, err := rotationTx.Exec(ctx, `SET LOCAL lock_timeout='250ms'`); err != nil {
		t.Fatal(err)
	}
	var version int64
	err = rotationTx.QueryRow(ctx, `SELECT payments.set_stripe_webhook_endpoint(
	 $1::uuid,$2::uuid,$3::uuid,$4::uuid,$5::uuid,'PROVIDER_MOCK',1,false,
	 'fixture_signing_key_v2',$6::bytea,$7::bytea)`, a.tenant, a.store, a.principal, h.accountA, h.endpointA, randomBytes(12), randomBytes(48)).Scan(&version)
	if !stripeSQLState(err, "55P03") {
		t.Fatalf("endpoint rotation did not hit server lock timeout: %v", err)
	}
	if err := rotationTx.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	var current int64
	if err := a.base.owner.QueryRow(ctx, `SELECT key_version FROM payments.stripe_webhook_endpoints WHERE endpoint_id=$1::uuid`, h.endpointA).Scan(&current); err != nil || current != 1 {
		t.Fatalf("timed-out rotation changed endpoint version=%d err=%v", current, err)
	}
	if err := tx.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	if version, err = h.endpoint(t, a.tenant, a.store, a.principal, h.accountA, h.endpointA, "PROVIDER_MOCK", 1, false); err != nil || version != 2 {
		t.Fatalf("rotation after prepare commit=%d err=%v", version, err)
	}
}

// A committed ACCEPTED receipt without its exact signal and River job would
// let ingress ACK an event that no worker can observe. The deferred constraint
// must reject this at COMMIT, even when the owner fixture bypasses the API.
func TestStripeSP13AcceptedReceiptNeedsCommittedLink(t *testing.T) {
	h := stripeRegistrarSetup(t)
	a := h.seed
	if err := h.register(t, a.tenant, a.store, a.principal, h.accountA, randomUUID(), "acct_TestStoreA1"); err != nil {
		t.Fatal(err)
	}
	if version, err := h.endpoint(t, a.tenant, a.store, a.principal, h.accountA, h.endpointA, "PROVIDER_MOCK", 0, true); err != nil || version != 1 {
		t.Fatalf("endpoint create=%d err=%v", version, err)
	}
	ctx := context.Background()
	tx, err := a.base.owner.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	_, err = tx.Exec(ctx, `INSERT INTO payments.stripe_webhook_receipts
	 (id,endpoint_id,environment,account_id,event_id,event_type,event_created,api_version,
	  object_type,session_id,body_sha256,signed_at,disposition,reason,attempt_id,tenant_id,store_id,signal_id)
	 VALUES ($1::uuid,$2::uuid,'SANDBOX','acct_TestStoreA1',$3,'checkout.session.completed',$4,
	  '2026-08-26.dahlia','checkout.session','cs_test_orphan',$5::bytea,$4,'ACCEPTED','accepted',
	  $6::uuid,$7::uuid,$8::uuid,$9::uuid)`,
		randomUUID(), h.endpointA, "evt_stripe_orphan_"+t04Tag(), time.Now().Unix(), randomBytes(32),
		randomUUID(), a.tenant, a.store, randomUUID())
	if err != nil {
		t.Fatalf("ACCEPTED fixture rejected before deferred COMMIT check: %v", err)
	}
	if err := tx.Commit(ctx); err == nil {
		t.Fatal("ACCEPTED receipt committed without reciprocal signal and River job")
	}
}
