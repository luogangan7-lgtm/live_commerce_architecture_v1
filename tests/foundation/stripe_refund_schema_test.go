package foundation_test

// RF03 (contracts/stripe-refund-v1.md §4, §9): the refund schema, REAL_PG. Prefix `srs`.
//
// Every privilege, policy and definer property is read from the catalog (pg_policies, ACLs,
// pg_proc, pg_constraint), never inferred from a successful call; behaviour is then proven through
// the real definers (the real refund worker and the merchant HTTP handler, not as superuser), and the
// set-once / fact triggers are exercised with owner-pool statements that are disclosed at their use.
// Policy names follow the contract text and refund-core D5. Evidence: REAL_PG.
//
// NOT_RUN clause (recorded, not hidden): "fresh + populated upgrade from 0061". migrations.Apply has
// no partial-apply hook, so a database frozen at 0061 with rows cannot be built without re-implementing
// the migrator; the subtest below is an explicit SKIP and the constraint re-derivation clause is
// covered from the fresh database instead (every prior value still admitted).

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"testing"
	"time"

	"livecommerce/internal/integrations/psp/stripe/stripetest"
	"livecommerce/internal/platform"
)

func srsBool(t *testing.T, e *rfxEnv, label, q string, args ...any) bool {
	t.Helper()
	var ok bool
	if err := e.f.owner.QueryRow(context.Background(), q, args...).Scan(&ok); err != nil {
		t.Fatalf("%s: %v", label, err)
	}
	return ok
}

func srsMust(t *testing.T, e *rfxEnv, label, q string, args ...any) {
	t.Helper()
	if !srsBool(t, e, label, q, args...) {
		t.Errorf("RF03 missing schema requirement: %s", label)
	}
}

func srsDef(t *testing.T, e *rfxEnv, table string, fragments ...string) {
	t.Helper()
	var defs string
	if err := e.f.owner.QueryRow(context.Background(), `SELECT coalesce(string_agg(pg_get_constraintdef(oid),' | '),'') FROM pg_constraint WHERE conrelid=to_regclass($1) AND contype='c'`, table).Scan(&defs); err != nil {
		t.Fatal(err)
	}
	defs = strings.ToLower(defs)
	for _, f := range fragments {
		if !strings.Contains(defs, strings.ToLower(f)) {
			t.Errorf("RF03 %s CHECKs lack %q", table, f)
		}
	}
}

// srsFK reports whether a foreign key with exactly these column lists exists.
func srsFK(t *testing.T, e *rfxEnv, table string, cols []string, refTable string, refCols []string) {
	t.Helper()
	srsMust(t, e, fmt.Sprintf("FK %s%v -> %s%v", table, cols, refTable, refCols), `SELECT EXISTS(SELECT 1 FROM pg_constraint c WHERE c.contype='f' AND c.conrelid=$1::regclass AND c.confrelid=$2::regclass
	 AND (SELECT array_agg(a.attname::text ORDER BY k.ord) FROM unnest(c.conkey) WITH ORDINALITY k(attnum,ord) JOIN pg_attribute a ON a.attrelid=c.conrelid AND a.attnum=k.attnum)=$3::text[]
	 AND (SELECT array_agg(a.attname::text ORDER BY k.ord) FROM unnest(c.confkey) WITH ORDINALITY k(attnum,ord) JOIN pg_attribute a ON a.attrelid=c.confrelid AND a.attnum=k.attnum)=$4::text[])`,
		table, refTable, cols, refCols)
}

func srsPolicy(t *testing.T, e *rfxEnv, schema, table, name, role, cmd string, permissive bool, fragments ...string) {
	t.Helper()
	kind := "PERMISSIVE"
	if !permissive {
		kind = "RESTRICTIVE"
	}
	var qual, check *string
	err := e.f.owner.QueryRow(context.Background(), `SELECT qual,with_check FROM pg_policies WHERE schemaname=$1 AND tablename=$2 AND policyname=$3 AND $4::name=ANY(roles) AND cmd=$5 AND permissive=$6`,
		schema, table, name, role, cmd, kind).Scan(&qual, &check)
	if err != nil {
		t.Errorf("RF03 policy %s ON %s.%s TO %s FOR %s (%s) missing: %v", name, schema, table, role, cmd, kind, err)
		return
	}
	text := ""
	if qual != nil {
		text += *qual
	}
	if check != nil {
		text += *check
	}
	for _, f := range fragments {
		if !strings.Contains(text, f) {
			t.Errorf("RF03 policy %s qual/check lacks %q: %s", name, f, text)
		}
	}
}

func srsPriv(t *testing.T, e *rfxEnv, role, object, priv string, want bool) {
	t.Helper()
	q := `SELECT has_table_privilege($1,$2,$3)`
	if got := srsBool(t, e, "priv", q, role, object, priv); got != want {
		t.Errorf("RF03 has_table_privilege(%s,%s,%s) = %v want %v", role, object, priv, got, want)
	}
}

func srsColPriv(t *testing.T, e *rfxEnv, role, object, col, priv string, want bool) {
	t.Helper()
	if got := srsBool(t, e, "colpriv", `SELECT has_column_privilege($1,$2,$3,$4)`, role, object, col, priv); got != want {
		t.Errorf("RF03 has_column_privilege(%s,%s.%s,%s) = %v want %v", role, object, col, priv, got, want)
	}
}

// srsCheck inserts one all-valid row into a LIKE copy (defaults + CHECKs, no FKs/triggers), then
// asserts every mutation of one or more columns is refused with a CHECK violation (23514).
func srsCheck(t *testing.T, e *rfxEnv, table string, base map[string]string, cases map[string]map[string]string) {
	t.Helper()
	// Columns are base ∪ every case's keys: a case column absent from base (e.g. stripe_refund_id,
	// refresh_count) must still reach the INSERT, or the "negative" silently equals the valid baseline.
	// The baseline row uses DEFAULT for such columns.
	colSet := map[string]bool{}
	for c := range base {
		colSet[c] = true
	}
	for _, over := range cases {
		for c := range over {
			colSet[c] = true
		}
	}
	cols := make([]string, 0, len(colSet))
	for c := range colSet {
		cols = append(cols, c)
	}
	sort.Strings(cols)
	values := func(over map[string]string) string {
		parts := make([]string, len(cols))
		for i, c := range cols {
			v, ok := base[c]
			if !ok {
				v = "DEFAULT"
			}
			if o, ok := over[c]; ok {
				v = o
			}
			parts[i] = v
		}
		return strings.Join(parts, ",")
	}
	names := make([]string, 0, len(cases))
	for n := range cases {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, name := range names {
		ctx := context.Background()
		tx, err := e.f.owner.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = tx.Exec(ctx, `CREATE TEMP TABLE srs_check_case (LIKE `+table+` INCLUDING DEFAULTS INCLUDING CONSTRAINTS) ON COMMIT DROP`); err != nil {
			tx.Rollback(ctx)
			t.Fatal(err)
		}
		if _, err = tx.Exec(ctx, `INSERT INTO srs_check_case (`+strings.Join(cols, ",")+`) VALUES (`+values(nil)+`)`); err != nil {
			tx.Rollback(ctx)
			t.Fatalf("RF03 %s: the all-valid baseline row is refused (fixture or schema error): %v", table, err)
		}
		_, err = tx.Exec(ctx, `INSERT INTO srs_check_case (`+strings.Join(cols, ",")+`) VALUES (`+values(cases[name])+`)`)
		tx.Rollback(ctx)
		if sqlState(err) != "23514" {
			t.Errorf("RF03 %s CHECK case %q: want 23514, got %v", table, name, err)
		}
	}
}

const srsUUID = "'00000000-0000-4000-8000-0000000000%02d'::uuid"

func srsRefundBase() map[string]string {
	u := func(n int) string { return fmt.Sprintf(srsUUID, n) }
	return map[string]string{
		"tenant_id": u(1), "store_id": u(2), "id": u(3), "attempt_id": u(4), "order_id": u(5), "owner_id": u(6), "principal_id": u(7),
		"environment": "'SANDBOX'", "account_id": "'acct_SrsTest1'", "credential_version": "1", "payment_intent_id": "'pi_srs1'", "currency": "'TWD'",
		"amount_minor": "1000", "reason": "'requested_by_customer'", "create_params": "'{}'::jsonb",
		"requested_at": "TIMESTAMPTZ '2026-01-01 00:00:00+00'", "resend_until": "TIMESTAMPTZ '2026-01-01 20:00:00+00'",
	}
}

func srsSent(m map[string]string) map[string]string {
	out := map[string]string{}
	for k, v := range m {
		out[k] = v
	}
	out["first_sent_at"], out["last_sent_at"] = "TIMESTAMPTZ '2026-01-01 01:00:00+00'", "TIMESTAMPTZ '2026-01-01 01:00:00+00'"
	out["send_count"], out["body_sha256"] = "1", "decode(repeat('ab',32),'hex')"
	return out
}

func TestStripeRF03Schema(t *testing.T) {
	e := rfxNew(t)
	ctx := context.Background()

	// (A1) 0062 adds the permission and NO grant row (the onboarding grant exists only via 0065, OP01).
	if n := e.count(t, `SELECT count(*) FROM identity.store_grants WHERE permission='payments:refund'`); n != 0 {
		t.Fatalf("0062 (or the fixture) wrote %d payments:refund grant rows before any test grant", n)
	}

	t.Run("migrations, tables, RLS, PUBLIC and direct-role privileges", func(t *testing.T) {
		for _, v := range []string{"0062_stripe_refund.sql", "post_river/0013_stripe_refund.sql"} {
			srsMust(t, e, "migration "+v, `SELECT EXISTS(SELECT 1 FROM public.lc_schema_migrations WHERE version=$1 AND checksum<>'')`, v)
		}
		for _, table := range []string{"payments.stripe_refunds", "payments.refund_facts"} {
			srsMust(t, e, table+" FORCE RLS", `SELECT coalesce((SELECT relrowsecurity AND relforcerowsecurity FROM pg_class WHERE oid=to_regclass($1)),false)`, table)
			srsMust(t, e, table+" refused to PUBLIC", `SELECT NOT EXISTS (SELECT 1 FROM pg_class c CROSS JOIN LATERAL aclexplode(COALESCE(c.relacl,acldefault('r',c.relowner))) acl WHERE c.oid=to_regclass($1) AND acl.grantee=0)`, table)
			for _, role := range []string{"commerce_runtime", "commerce_worker", "commerce_checkout_runtime", "commerce_stripe_ingress"} {
				for _, p := range []string{"SELECT", "INSERT", "UPDATE", "DELETE"} {
					srsPriv(t, e, role, table, p, false)
				}
				srsMust(t, e, table+" no column privilege for "+role, `SELECT NOT has_any_column_privilege($1,$2,'SELECT') AND NOT has_any_column_privilege($1,$2,'UPDATE')`, role, table)
			}
		}
		srsMust(t, e, "payments.comment", `SELECT obj_description(to_regclass('payments.stripe_refunds'),'pg_class') IS NOT NULL AND obj_description(to_regclass('payments.refund_facts'),'pg_class') IS NOT NULL`)
	})

	t.Run("widened constraints keep every prior value", func(t *testing.T) {
		srsDef(t, e, "identity.store_grants", "payments:refund", "store:read", "audit:read", "audit:write", "catalog:read", "catalog:write", "inventory:read", "inventory:write",
			"inventory:reserve", "pricing:read", "pricing:write", "integration:manage", "integration:execute", "integration:read", "orders:read", "live:read", "live:manage")
		srsDef(t, e, "integration.operations", "PAYMENT_REFUND", "stripe.refund", "MERCHANT", "BUYER_PAYMENT_QUERY", "MEDIA_ATTEMPT", "stripe")
		srsDef(t, e, "payments.review_cases", "REFUND_UNRESOLVED", "REFUND_AMOUNT_MISMATCH", "REFUND_CONFLICTING", "REFUND_HISTORY", "CAPTURE_EVIDENCE_INCOMPLETE", "CONFLICTING_REPORT",
			"PAID_ALLOCATION_FAILED", "PROVIDER_AMOUNT_MISMATCH", "PROVIDER_PRESENTMENT_DRIFT", "PROVIDER_SESSION_DUPLICATE", "PROVIDER_IDENTITY_MISMATCH", "PROVIDER_EXPIRY_UNCONFIRMED",
			"PROVIDER_ASYNC_PENDING", "CLOSURE_CONTRADICTED")
		srsDef(t, e, "payments.stripe_signals", "MERCHANT_REFRESH", "STRIPE_WEBHOOK", "BUYER_REFRESH", "BUYER_CANCEL")
		srsDef(t, e, "payments.stripe_webhook_receipts", "unknown_refund", "unknown_charge", "unknown_session", "signal_cap")
		// §4.1 "object_type admits refund, charge": 0061 constrains object_type by a shape regex, not an
		// enumeration, so admission is proved by behaviour on a LIKE copy (NOT NULLs dropped so only the
		// CHECKs decide), not by searching the constraint text for the literal names.
		func() {
			ctx := context.Background()
			tx, err := e.f.owner.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback(ctx)
			for _, q := range []string{
				`CREATE TEMP TABLE srs_rcpt (LIKE payments.stripe_webhook_receipts INCLUDING CONSTRAINTS) ON COMMIT DROP`,
				`DO $$ DECLARE c text; BEGIN FOR c IN SELECT attname FROM pg_attribute WHERE attrelid='srs_rcpt'::regclass AND attnum>0 AND attnotnull AND NOT attisdropped
				 LOOP EXECUTE format('ALTER TABLE srs_rcpt ALTER COLUMN %I DROP NOT NULL', c); END LOOP; END $$`,
				`INSERT INTO srs_rcpt(object_type) VALUES('refund'),('charge'),('checkout.session')`,
			} {
				if _, err := tx.Exec(ctx, q); err != nil {
					t.Errorf("RF03 stripe_webhook_receipts.object_type must admit refund, charge, checkout.session: %v", err)
					return
				}
			}
		}()
		srsMust(t, e, "stripe_signals.refund_id uuid NULL", `SELECT EXISTS(SELECT 1 FROM information_schema.columns WHERE table_schema='payments' AND table_name='stripe_signals' AND column_name='refund_id' AND data_type='uuid' AND is_nullable='YES')`)
		srsMust(t, e, "receipts.refund_id uuid NULL", `SELECT EXISTS(SELECT 1 FROM information_schema.columns WHERE table_schema='payments' AND table_name='stripe_webhook_receipts' AND column_name='refund_id' AND data_type='uuid' AND is_nullable='YES')`)
		srsMust(t, e, "stripe_sessions.charge_signal_count", `SELECT EXISTS(SELECT 1 FROM information_schema.columns WHERE table_schema='payments' AND table_name='stripe_sessions' AND column_name='charge_signal_count' AND is_nullable='NO')`)
		srsMust(t, e, "payment_attempts UNIQUE(tenant,store,owner,order,id)", `SELECT EXISTS(SELECT 1 FROM pg_constraint c WHERE c.contype IN ('u','p') AND c.conrelid='checkout.payment_attempts'::regclass
		 AND (SELECT array_agg(a.attname::text ORDER BY k.ord) FROM unnest(c.conkey) WITH ORDINALITY k(attnum,ord) JOIN pg_attribute a ON a.attrelid=c.conrelid AND a.attnum=k.attnum)=ARRAY['tenant_id','store_id','owner_id','order_id','id'])`)
		// behaviour of the new signal / session CHECKs on LIKE copies
		signalBase := map[string]string{"id": "gen_random_uuid()", "tenant_id": "gen_random_uuid()", "store_id": "gen_random_uuid()", "attempt_id": "gen_random_uuid()", "source": "'MERCHANT_REFRESH'", "job_id": "1", "refund_id": "gen_random_uuid()"}
		srsCheck(t, e, "payments.stripe_signals", signalBase, map[string]map[string]string{"MERCHANT_REFRESH without refund_id": {"refund_id": "NULL"}})
		sessionCheck := func(name, v string) {
			ctx := context.Background()
			tx, err := e.f.owner.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback(ctx)
			if _, err := tx.Exec(ctx, `CREATE TEMP TABLE srs_sess (LIKE payments.stripe_sessions INCLUDING DEFAULTS INCLUDING CONSTRAINTS) ON COMMIT DROP`); err != nil {
				t.Fatal(err)
			}
			_, err = tx.Exec(ctx, `INSERT INTO srs_sess(tenant_id,store_id,owner_id,attempt_id,environment,account_id,locale,config_digest,unit_amount,create_params,attempt_created_at,expires_at,send_deadline,handoff_cutoff,charge_signal_count)
			 VALUES(gen_random_uuid(),gen_random_uuid(),gen_random_uuid(),gen_random_uuid(),'SANDBOX','acct_x','en',decode(repeat('ab',32),'hex'),100,'{}',TIMESTAMPTZ '2026-01-01 00:00:00+00',TIMESTAMPTZ '2026-01-01 00:40:00+00',TIMESTAMPTZ '2026-01-01 00:07:00+00',TIMESTAMPTZ '2026-01-01 00:35:00+00',`+v+`)`)
			if name == "ok" && err != nil {
				t.Fatalf("baseline session row refused: %v", err)
			}
			if name != "ok" && sqlState(err) != "23514" {
				t.Errorf("charge_signal_count %s: want 23514, got %v", v, err)
			}
		}
		sessionCheck("ok", "64")
		sessionCheck("over", "65")
		sessionCheck("negative", "-1")
	})

	t.Run("stripe_refunds and refund_facts CHECKs", func(t *testing.T) {
		u := func(n int) string { return fmt.Sprintf(srsUUID, n) }
		base := srsRefundBase()
		srsCheck(t, e, "payments.stripe_refunds", base, map[string]map[string]string{
			"unknown environment":             {"environment": "'PROD'"}, // was "LIVE environment": 0077 widens the CHECK to SANDBOX|LIVE (SL02 covers it)
			"account id shape":                {"account_id": "'acct_'"},
			"credential_version 0":            {"credential_version": "0"},
			"payment_intent shape":            {"payment_intent_id": "'pi bad'"},
			"currency lower case":             {"currency": "'twd'"},
			"amount 0":                        {"amount_minor": "0"},
			"amount over 999999999999":        {"amount_minor": "1000000000000"},
			"TWD step (150)":                  {"amount_minor": "150"},
			"reason fraudulent":               {"reason": "'fraudulent'"},
			"create_params not an object":     {"create_params": "'[]'::jsonb"},
			"create_params over 2048 bytes":   {"create_params": "jsonb_build_object('k',repeat('x',2100))"},
			"resend_until != requested+20h":   {"resend_until": "TIMESTAMPTZ '2026-01-01 19:00:00+00'"},
			"pin without pinned_at":           {"stripe_refund_id": "'re_x'"},
			"pinned_at without pin":           {"pinned_at": "TIMESTAMPTZ '2026-01-01 01:00:00+00'"},
			"first_sent_at with send_count 0": {"first_sent_at": "TIMESTAMPTZ '2026-01-01 01:00:00+00'", "last_sent_at": "TIMESTAMPTZ '2026-01-01 01:00:00+00'", "body_sha256": "decode(repeat('ab',32),'hex')"},
			"send_count without first_sent":   {"send_count": "1"},
			"refresh_count 31":                {"refresh_count": "31"},
			"signal_count 65":                 {"signal_count": "65"},
			"send_count 201":                  {"send_count": "201"},
		})
		sent := srsSent(base)
		srsCheck(t, e, "payments.stripe_refunds", sent, map[string]map[string]string{
			"body hash of 31 bytes":                   {"body_sha256": "decode(repeat('ab',31),'hex')"},
			"suppressed after first send":             {"suppressed_at": "TIMESTAMPTZ '2026-01-01 02:00:00+00'"},
			"first send after resend_until - 1h":      {"first_sent_at": "TIMESTAMPTZ '2026-01-01 19:30:00+00'", "last_sent_at": "TIMESTAMPTZ '2026-01-01 19:30:00+00'"},
			"first send exactly at resend_until-1h":   {"first_sent_at": "TIMESTAMPTZ '2026-01-01 19:00:00+00'", "last_sent_at": "TIMESTAMPTZ '2026-01-01 19:00:00+00'"},
			"missing last_sent_at with first_sent_at": {"last_sent_at": "NULL"},
		})
		factBase := map[string]string{"tenant_id": u(1), "store_id": u(2), "refund_id": u(3), "attempt_id": u(4), "kind": "'FAILED'", "amount_minor": "1000", "currency": "'TWD'",
			"stripe_refund_id": "'re_x'", "failure_reason": "'declined'", "source_report_hash": "decode(repeat('ab',32),'hex')"}
		srsCheck(t, e, "payments.refund_facts", factBase, map[string]map[string]string{
			"kind":                          {"kind": "'DONE'"},
			"negative amount":               {"amount_minor": "-1"},
			"currency shape":                {"currency": "'tw'"},
			"stripe id shape":               {"stripe_refund_id": "'re bad'"},
			"failure_reason outside enum":   {"failure_reason": "'because'"},
			"hash length":                   {"source_report_hash": "decode(repeat('ab',31),'hex')"},
			"REJECTED with a stripe id":     {"kind": "'REJECTED'"},
			"SUCCEEDED without a stripe id": {"kind": "'SUCCEEDED'", "stripe_refund_id": "NULL"},
		})
		for _, reason := range []string{"lost_or_stolen_card", "expired_or_canceled_card", "charge_for_pending_refund_disputed", "insufficient_funds", "declined", "merchant_request", "unknown",
			"first_send_rejected", "external_refund_detected", "send_window_closed"} {
			ok := map[string]string{}
			for k, v := range factBase {
				ok[k] = v
			}
			ok["failure_reason"] = "'" + reason + "'"
			srsCheck(t, e, "payments.refund_facts", ok, map[string]map[string]string{"control: still-invalid hash": {"source_report_hash": "decode('ab','hex')"}})
		}
	})

	t.Run("foreign keys, definers, ACLs and the private matrix", func(t *testing.T) {
		srsFK(t, e, "payments.stripe_refunds", []string{"tenant_id", "store_id", "owner_id", "order_id", "attempt_id"}, "checkout.payment_attempts", []string{"tenant_id", "store_id", "owner_id", "order_id", "id"})
		srsFK(t, e, "payments.stripe_refunds", []string{"tenant_id", "store_id", "id"}, "integration.operations", []string{"tenant_id", "store_id", "id"})
		srsFK(t, e, "payments.stripe_refunds", []string{"tenant_id", "principal_id"}, "identity.memberships", []string{"tenant_id", "principal_id"})
		srsFK(t, e, "payments.refund_facts", []string{"tenant_id", "store_id", "attempt_id", "refund_id"}, "payments.stripe_refunds", []string{"tenant_id", "store_id", "attempt_id", "id"})
		srsFK(t, e, "payments.refund_facts", []string{"tenant_id", "store_id", "attempt_id", "source_report_hash"}, "payments.provider_observations", []string{"tenant_id", "store_id", "attempt_id", "report_hash"})
		srsFK(t, e, "payments.stripe_signals", []string{"tenant_id", "store_id", "attempt_id", "refund_id"}, "payments.stripe_refunds", []string{"tenant_id", "store_id", "attempt_id", "id"})
		srsMust(t, e, "stripe_refunds.stripe_refund_id UNIQUE", `SELECT EXISTS(SELECT 1 FROM pg_constraint c WHERE c.contype='u' AND c.conrelid='payments.stripe_refunds'::regclass AND pg_get_constraintdef(c.oid) ILIKE '%(stripe_refund_id)%')`)
		srsMust(t, e, "refund_facts PK (tenant,store,refund,kind)", `SELECT EXISTS(SELECT 1 FROM pg_constraint c WHERE c.contype='p' AND c.conrelid='payments.refund_facts'::regclass
		 AND (SELECT array_agg(a.attname::text ORDER BY k.ord) FROM unnest(c.conkey) WITH ORDINALITY k(attnum,ord) JOIN pg_attribute a ON a.attrelid=c.conrelid AND a.attnum=k.attnum)=ARRAY['tenant_id','store_id','refund_id','kind'])`)
		srsMust(t, e, "amount step function is IMMUTABLE", `SELECT p.provolatile='i' FROM pg_proc p WHERE p.oid='payments.stripe_refund_amount_ok(text,bigint)'::regprocedure`)
		for _, trg := range []string{"guard_stripe_refund", "guard_refund_fact"} {
			srsMust(t, e, "trigger function payments."+trg, `SELECT EXISTS(SELECT 1 FROM pg_trigger tg JOIN pg_proc p ON p.oid=tg.tgfoid JOIN pg_namespace n ON n.oid=p.pronamespace WHERE n.nspname='payments' AND p.proname=$1 AND NOT tg.tgisinternal)`, trg)
		}
		type fn struct {
			sig, owner string
			exec, deny []string
		}
		for _, f := range []fn{
			{"payments.request_stripe_refund(bytea,uuid,uuid,text,bytea,bigint,text,bigint,uuid,bigint)", "commerce_checkout_writer", []string{"commerce_runtime"}, []string{"commerce_worker", "commerce_checkout_runtime"}},
			{"payments.request_stripe_refund_refresh(bytea,uuid,uuid,uuid,uuid,bigint)", "commerce_checkout_writer", []string{"commerce_runtime"}, []string{"commerce_worker", "commerce_checkout_runtime"}},
			{"integration.require_stripe_refund(uuid,bigint,bytea,text)", "commerce_integration_writer", nil, []string{"commerce_runtime", "commerce_worker"}},
			{"integration.load_stripe_refund(uuid,bigint,bytea,text)", "commerce_integration_writer", []string{"commerce_worker"}, []string{"commerce_runtime", "commerce_checkout_runtime"}},
			{"integration.mark_stripe_refund_sent(uuid,bigint,bytea,text,bytea)", "commerce_integration_writer", []string{"commerce_worker"}, []string{"commerce_runtime"}},
			{"integration.record_stripe_refund_observation(uuid,bigint,bytea,text,jsonb,bigint)", "commerce_integration_writer", []string{"commerce_worker"}, []string{"commerce_runtime"}},
			{"integration.record_stripe_charge_observation(uuid,bigint,bytea,text,jsonb,bigint)", "commerce_integration_writer", []string{"commerce_worker"}, []string{"commerce_runtime"}},
			{"integration.finish_stripe_refund(uuid,bigint,bytea,text,text)", "commerce_integration_writer", []string{"commerce_worker"}, []string{"commerce_runtime"}},
			{"payments.apply_stripe_refund(uuid,bytea)", "commerce_checkout_writer", nil, []string{"commerce_worker", "commerce_runtime"}},
			{"payments.apply_stripe_charge(uuid,bytea)", "commerce_checkout_writer", nil, []string{"commerce_worker", "commerce_runtime"}},
			{"payments.apply_stripe_observation(uuid,bytea)", "commerce_checkout_writer", nil, []string{"commerce_worker", "commerce_runtime"}},
			{"payments.apply_capture(uuid,bytea)", "commerce_checkout_writer", []string{"commerce_worker"}, []string{"commerce_runtime"}},
			{"identity.read_merchant_refunds(bytea,uuid,uuid)", "commerce_auth", []string{"commerce_runtime"}, []string{"commerce_worker"}},
		} {
			var ownerName string
			var definer, fixed, publicExec bool
			err := e.f.owner.QueryRow(ctx, `SELECT pg_get_userbyid(p.proowner),p.prosecdef,p.proconfig=ARRAY['search_path=pg_catalog']::text[],
			 EXISTS(SELECT 1 FROM aclexplode(coalesce(p.proacl,acldefault('f',p.proowner))) a WHERE a.grantee=0 AND a.privilege_type='EXECUTE')
			 FROM pg_proc p WHERE p.oid=to_regprocedure($1)`, f.sig).Scan(&ownerName, &definer, &fixed, &publicExec)
			if err != nil {
				t.Errorf("RF03 function %s missing: %v", f.sig, err)
				continue
			}
			if ownerName != f.owner || !definer || !fixed || publicExec {
				t.Errorf("RF03 %s: owner=%s definer=%v search_path=pg_catalog:%v public-execute=%v", f.sig, ownerName, definer, fixed, publicExec)
			}
			for _, role := range f.exec {
				if !srsBool(t, e, "exec", `SELECT has_function_privilege($1,to_regprocedure($2),'EXECUTE')`, role, f.sig) {
					t.Errorf("RF03 %s: EXECUTE missing for %s", f.sig, role)
				}
			}
			for _, role := range f.deny {
				if srsBool(t, e, "deny", `SELECT has_function_privilege($1,to_regprocedure($2),'EXECUTE')`, role, f.sig) {
					t.Errorf("RF03 %s: %s must not EXECUTE", f.sig, role)
				}
			}
			if !srsBool(t, e, "comment", `SELECT obj_description(to_regprocedure($1),'pg_proc') IS NOT NULL`, f.sig) {
				t.Errorf("RF03 %s has no COMMENT ON (PROCESS §5)", f.sig)
			}
		}
		// §4.5 privilege matrix: one positive and one negative per role.
		for _, table := range []string{"payments.stripe_refunds", "payments.refund_facts"} {
			srsPriv(t, e, "commerce_checkout_writer", table, "SELECT", true)
			srsPriv(t, e, "commerce_checkout_writer", table, "INSERT", true)
			srsPriv(t, e, "commerce_checkout_writer", table, "DELETE", false)
			srsPriv(t, e, "commerce_integration_writer", table, "SELECT", true)
			srsPriv(t, e, "commerce_integration_writer", table, "DELETE", false)
		}
		for _, c := range []string{"refresh_count", "last_refresh_at", "signal_count"} {
			srsColPriv(t, e, "commerce_checkout_writer", "payments.stripe_refunds", c, "UPDATE", true)
		}
		for _, c := range []string{"amount_minor", "stripe_refund_id", "first_sent_at", "suppressed_at", "reason", "currency", "principal_id"} {
			srsColPriv(t, e, "commerce_checkout_writer", "payments.stripe_refunds", c, "UPDATE", false)
		}
		for _, c := range []string{"first_sent_at", "last_sent_at", "send_count", "body_sha256", "suppressed_at", "stripe_refund_id", "pinned_at", "signal_count"} {
			srsColPriv(t, e, "commerce_integration_writer", "payments.stripe_refunds", c, "UPDATE", true)
		}
		for _, c := range []string{"amount_minor", "currency", "reason", "requested_at", "resend_until", "refresh_count", "account_id", "create_params"} {
			srsColPriv(t, e, "commerce_integration_writer", "payments.stripe_refunds", c, "UPDATE", false)
		}
		srsPriv(t, e, "commerce_integration_writer", "payments.stripe_refunds", "INSERT", false)
		srsPriv(t, e, "commerce_integration_writer", "payments.refund_facts", "INSERT", false)
		srsPriv(t, e, "commerce_integration_writer", "payments.review_cases", "SELECT", true)
		srsPriv(t, e, "commerce_integration_writer", "payments.review_cases", "INSERT", true)
		srsPriv(t, e, "commerce_integration_writer", "payments.review_cases", "UPDATE", false)
		srsColPriv(t, e, "commerce_integration_writer", "payments.stripe_sessions", "charge_signal_count", "UPDATE", true)
		srsColPriv(t, e, "commerce_checkout_writer", "payments.stripe_sessions", "charge_signal_count", "UPDATE", false)
		for _, c := range []string{"tenant_id", "store_id", "attempt_id", "id", "amount_minor", "currency", "reason", "requested_at", "stripe_refund_id", "first_sent_at", "suppressed_at", "last_sent_at", "pinned_at"} {
			srsColPriv(t, e, "commerce_auth", "payments.stripe_refunds", c, "SELECT", true)
		}
		for _, c := range []string{"create_params", "account_id", "payment_intent_id", "principal_id", "credential_version", "body_sha256", "send_count", "owner_id", "order_id"} {
			srsColPriv(t, e, "commerce_auth", "payments.stripe_refunds", c, "SELECT", false)
		}
		for _, c := range []string{"tenant_id", "store_id", "refund_id", "kind", "received_at", "failure_reason"} {
			srsColPriv(t, e, "commerce_auth", "payments.refund_facts", c, "SELECT", true)
		}
		for _, c := range []string{"amount_minor", "source_report_hash", "stripe_refund_id", "attempt_id"} {
			srsColPriv(t, e, "commerce_auth", "payments.refund_facts", c, "SELECT", false)
		}
		// (A2) schema USAGE the merchant definers need.
		for _, schema := range []string{"identity", "ops"} {
			if !srsBool(t, e, "usage", `SELECT has_schema_privilege('commerce_checkout_writer',$1,'USAGE')`, schema) {
				t.Errorf("RF03 commerce_checkout_writer lacks USAGE on schema %s", schema)
			}
		}
		// checkout_writer merchant-auth set.
		for _, c := range []string{"token_hash", "principal_id", "audience", "revoked_at", "expires_at"} {
			srsColPriv(t, e, "commerce_checkout_writer", "identity.sessions", c, "SELECT", true)
		}
		rows, err := e.f.owner.Query(ctx, `SELECT attname FROM pg_attribute WHERE attrelid='identity.sessions'::regclass AND attnum>0 AND NOT attisdropped
		 AND attname NOT IN ('token_hash','principal_id','audience','revoked_at','expires_at')`)
		if err != nil {
			t.Fatal(err)
		}
		for rows.Next() {
			var c string
			_ = rows.Scan(&c)
			srsColPriv(t, e, "commerce_checkout_writer", "identity.sessions", c, "SELECT", false)
		}
		rows.Close()
		if !srsBool(t, e, "resolve_access", `SELECT has_function_privilege('commerce_checkout_writer','identity.resolve_access(bytea,uuid,text)','EXECUTE')`) {
			t.Error("RF03 commerce_checkout_writer cannot EXECUTE identity.resolve_access")
		}
		srsPriv(t, e, "commerce_checkout_writer", "ops.command_results", "INSERT", true)
		srsPriv(t, e, "commerce_checkout_writer", "ops.command_results", "SELECT", true)
		srsPriv(t, e, "commerce_checkout_writer", "ops.audit_events", "INSERT", true)
		srsPriv(t, e, "commerce_checkout_writer", "ops.command_results", "DELETE", false)
		srsPriv(t, e, "commerce_checkout_writer", "ops.audit_events", "UPDATE", false)
		// (A2) policies by name, role, command and qual.
		g := []string{"app.tenant_id", "app.store_id"}
		srsPolicy(t, e, "payments", "stripe_refunds", "stripe_refund_checkout", "commerce_checkout_writer", "ALL", true, g...)
		srsPolicy(t, e, "payments", "refund_facts", "refund_fact_checkout", "commerce_checkout_writer", "ALL", true, g...)
		srsPolicy(t, e, "payments", "stripe_refunds", "stripe_refund_integration", "commerce_integration_writer", "ALL", true)
		srsPolicy(t, e, "payments", "refund_facts", "refund_fact_integration", "commerce_integration_writer", "SELECT", true)
		srsPolicy(t, e, "payments", "review_cases", "review_integration_read", "commerce_integration_writer", "SELECT", true)
		srsMust(t, e, "integration_writer INSERT policy on review_cases limited to REFUND_HISTORY", `SELECT EXISTS(SELECT 1 FROM pg_policies WHERE schemaname='payments' AND tablename='review_cases'
		 AND 'commerce_integration_writer'::name=ANY(roles) AND cmd IN ('INSERT','ALL') AND with_check LIKE '%REFUND_HISTORY%')`)
		srsPolicy(t, e, "payments", "stripe_refunds", "auth_refund_read", "commerce_auth", "SELECT", true)
		srsPolicy(t, e, "payments", "refund_facts", "auth_refund_fact_read", "commerce_auth", "SELECT", true)
		srsPolicy(t, e, "integration", "operations", "checkout_refund_operation", "commerce_checkout_writer", "ALL", true, "PAYMENT_REFUND", "app.principal_id")
		srsPolicy(t, e, "integration", "operation_events", "checkout_refund_event", "commerce_checkout_writer", "INSERT", true, "PAYMENT_REFUND")
		srsPolicy(t, e, "ops", "command_results", "checkout_command_result_insert", "commerce_checkout_writer", "INSERT", true, "app.principal_id")
		// SELECT is tenant/store scoped: the principal term is the 0002 command_actor shape, which is
		// INSERT-only, and the definers must read another principal's row to answer 409 key conflict (§7.1).
		srsPolicy(t, e, "ops", "command_results", "checkout_command_result_select", "commerce_checkout_writer", "SELECT", true, g...)
		srsPolicy(t, e, "ops", "audit_events", "checkout_audit_insert", "commerce_checkout_writer", "INSERT", true, "app.principal_id")
		srsMust(t, e, "RESTRICTIVE guards on ops.command_results and ops.audit_events for commerce_checkout_writer", `SELECT
		 EXISTS(SELECT 1 FROM pg_policies WHERE schemaname='ops' AND tablename='command_results' AND permissive='RESTRICTIVE' AND 'commerce_checkout_writer'::name=ANY(roles))
		 AND EXISTS(SELECT 1 FROM pg_policies WHERE schemaname='ops' AND tablename='audit_events' AND permissive='RESTRICTIVE' AND 'commerce_checkout_writer'::name=ANY(roles))`)
		// commerce_runtime River grants (post_river) and the platform runtime-pool validator.
		if !srsBool(t, e, "river usage", `SELECT has_schema_privilege('commerce_runtime','river_payment','USAGE') AND has_sequence_privilege('commerce_runtime','river_payment.river_job_id_seq','USAGE')`) {
			t.Error("RF03 commerce_runtime lacks river_payment USAGE / sequence USAGE")
		}
		srsPriv(t, e, "commerce_runtime", "river_payment.river_job", "SELECT", true)
		srsPriv(t, e, "commerce_runtime", "river_payment.river_job", "INSERT", true)
		srsColPriv(t, e, "commerce_runtime", "river_payment.river_job", "kind", "UPDATE", true)
		for _, priv := range []string{"DELETE", "TRUNCATE"} {
			srsPriv(t, e, "commerce_runtime", "river_payment.river_job", priv, false)
		}
		for _, c := range []string{"state", "args", "attempt", "errors", "finalized_at", "scheduled_at", "queue", "priority", "max_attempts"} {
			srsColPriv(t, e, "commerce_runtime", "river_payment.river_job", c, "UPDATE", false)
		}
		// Ruling 19: the runtime login keeps river_media (post_river 0006, live media planner); only
		// river_expiry and river_meta are foreign to it.
		for _, other := range []string{"river_expiry", "river_meta"} {
			if srsBool(t, e, "other river", `SELECT has_schema_privilege('commerce_runtime',$1,'USAGE')`, other) && srsBool(t, e, "other river", `SELECT has_table_privilege('commerce_runtime',$1||'.river_job','INSERT')`, other) {
				t.Errorf("RF03 commerce_runtime gained INSERT on %s.river_job", other)
			}
		}
	})

	t.Run("runtime pool validator accepts the new grants and still rejects River lifecycle privileges", func(t *testing.T) {
		if _, err := platform.OpenPool(ctx, bcRole(t, e.f, "commerce_runtime")); err != nil {
			t.Fatalf("validator rejects the frozen commerce_runtime privilege set: %v", err)
		}
		for _, bad := range []struct{ name, grant, revoke string }{
			{"DELETE on river_payment.river_job", `GRANT DELETE ON river_payment.river_job TO commerce_runtime`, `REVOKE DELETE ON river_payment.river_job FROM commerce_runtime`},
			{"UPDATE(state) on river_payment.river_job", `GRANT UPDATE(state) ON river_payment.river_job TO commerce_runtime`, `REVOKE UPDATE(state) ON river_payment.river_job FROM commerce_runtime`},
			{"UPDATE(args) on river_payment.river_job", `GRANT UPDATE(args) ON river_payment.river_job TO commerce_runtime`, `REVOKE UPDATE(args) ON river_payment.river_job FROM commerce_runtime`},
			{"TRUNCATE on river_payment.river_job", `GRANT TRUNCATE ON river_payment.river_job TO commerce_runtime`, `REVOKE TRUNCATE ON river_payment.river_job FROM commerce_runtime`},
			// Ruling 19 admits river_media for the runtime; river_meta stays foreign.
			{"INSERT on river_meta.river_job", `GRANT INSERT ON river_meta.river_job TO commerce_runtime`, `REVOKE INSERT ON river_meta.river_job FROM commerce_runtime`},
		} {
			mustExec(t, e.f.owner, bad.grant)
			_, err := platform.OpenPool(ctx, bcRole(t, e.f, "commerce_runtime"))
			mustExec(t, e.f.owner, bad.revoke)
			if err == nil {
				t.Errorf("validator accepted commerce_runtime with %s (a negative was loosened)", bad.name)
			}
		}
		if _, err := platform.OpenPool(ctx, bcRole(t, e.f, "commerce_runtime")); err != nil {
			t.Fatalf("validator rejects the privilege set after the negatives were reverted: %v", err)
		}
	})

	t.Run("post-River routing and job guards include payment_refund_v1", func(t *testing.T) {
		for _, name := range []string{"guard_payment_job_family", "payment_job_queue", "route_payment_queue_v1", "payment_queue_ready", "reject_legacy_family_job"} {
			srsMust(t, e, name+" mentions payment_refund_v1", `SELECT EXISTS(SELECT 1 FROM pg_proc p WHERE p.proname=$1 AND p.prosrc LIKE '%payment_refund_v1%')`, name)
		}
		// The legacy river schema still refuses payment kinds (reject_legacy_family_job).
		_, err := e.f.owner.Exec(ctx, `INSERT INTO river.river_job(kind,args,queue,max_attempts,state) VALUES('payment_refund_v1','{"operation_id":"00000000-0000-4000-8000-000000000001","version":1}','default',3,'available')`)
		if err == nil {
			mustExec(t, e.f.owner, `DELETE FROM river.river_job WHERE kind='payment_refund_v1'`)
			t.Error("river.river_job accepted a payment_refund_v1 job (reject_legacy_family_job)")
		}
		// guard_payment_job_family: exact args {operation_id, version:1} only.
		for name, args := range map[string]string{
			"extra key":        `{"operation_id":"00000000-0000-4000-8000-000000000001","version":1,"x":1}`,
			"wrong version":    `{"operation_id":"00000000-0000-4000-8000-000000000001","version":2}`,
			"missing version":  `{"operation_id":"00000000-0000-4000-8000-000000000001"}`,
			"unknown refund":   `{"operation_id":"00000000-0000-4000-8000-000000000001","version":1}`,
			"operation_id nan": `{"operation_id":"not-a-uuid","version":1}`,
		} {
			_, err := e.f.owner.Exec(ctx, `INSERT INTO river_payment.river_job(kind,args,queue,max_attempts,state) VALUES('payment_refund_v1',$1::jsonb,'default',3,'available')`, args)
			if err == nil {
				mustExec(t, e.f.owner, `DELETE FROM river_payment.river_job WHERE kind='payment_refund_v1' AND args=$1::jsonb`, args)
				t.Errorf("guard_payment_job_family accepted a payment_refund_v1 job with %s", name)
			}
		}
	})

	// ---- behaviour through the real definers ---------------------------------------------
	a := e.stripeStore(t)
	endpoint, secret := e.endpoint(t, a)
	e.startWorker(t)
	o1 := e.pay(t, a, endpoint, secret)
	o2 := e.payMore(t, o1)
	for _, o := range []rfxOrder{o1, o2} {
		e.grant(t, o, "orders:read", "payments:refund")
	}

	r1 := e.mustRefund(t, o1, 1000, "requested_by_customer")
	e.awaitRefundFact(t, r1, o1.attempt, "SUCCEEDED")
	e.fake.HoldNextRefund("pending", "processing")
	r2 := e.mustRefund(t, o1, 500, "requested_by_customer")
	e.awaitRefund(t, "r2 pinned", r2, o1.attempt, 45*time.Second, `SELECT stripe_refund_id IS NOT NULL FROM payments.stripe_refunds WHERE id=$1`)
	e.fake.SetNextFault(stripetest.Fault{Validation: true})
	r3 := e.mustRefund(t, o1, 300, "duplicate")
	e.awaitRefundFact(t, r3, o1.attempt, "REJECTED")
	e.fake.HoldNextRefund("pending", "processing")
	r5 := e.mustRefund(t, o1, 200, "duplicate")
	e.awaitRefund(t, "r5 pinned", r5, o1.attempt, 45*time.Second, `SELECT stripe_refund_id IS NOT NULL FROM payments.stripe_refunds WHERE id=$1`)
	e.fake.SetRefundStatus(e.fake.RefundByRef(r5), "failed", "declined")
	e.awaitRefundFact(t, r5, o1.attempt, "FAILED")
	r4 := e.mustRefund(t, o2, 1000, "requested_by_customer")
	e.awaitRefundFact(t, r4, o2.attempt, "SUCCEEDED")

	refuse := func(name, sql string, args ...any) {
		t.Helper()
		tx, err := e.f.owner.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback(ctx)
		if _, err := tx.Exec(ctx, sql, args...); err == nil {
			t.Errorf("RF03 %s was accepted", name)
		}
	}
	hashOf := func(refund, kind string) []byte {
		var h []byte
		if err := e.f.owner.QueryRow(ctx, `SELECT source_report_hash FROM payments.refund_facts WHERE refund_id=$1 AND kind=$2`, refund, kind).Scan(&h); err != nil {
			t.Fatalf("hash of %s/%s: %v", refund, kind, err)
		}
		return h
	}
	insertFact := `INSERT INTO payments.refund_facts(tenant_id,store_id,refund_id,attempt_id,kind,amount_minor,currency,stripe_refund_id,failure_reason,source_report_hash)
	 SELECT r.tenant_id,r.store_id,r.id,$2::uuid,$3,$4::bigint,'TWD',$5,$6,$7::bytea FROM payments.stripe_refunds r WHERE r.id=$1`
	// guard_refund_fact runs as commerce_checkout_writer under FORCE RLS; the product's definers set the
	// app.tenant_id/app.store_id GUCs before touching stripe_refunds/refund_facts (§4.5), so the fixture
	// does the same, or every insert is refused as "not found" for the wrong reason.
	factTx := func(commit bool, args ...any) error {
		tx, err := e.f.owner.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback(ctx)
		if _, err := tx.Exec(ctx, `SELECT set_config('app.tenant_id',tenant_id::text,true),set_config('app.store_id',store_id::text,true)
		 FROM payments.stripe_refunds WHERE id=$1`, args[0]); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, insertFact, args...); err != nil {
			return err
		}
		if commit {
			return tx.Commit(ctx)
		}
		return nil
	}
	refuseFact := func(name string, args ...any) {
		t.Helper()
		if err := factTx(false, args...); err == nil {
			t.Errorf("RF03 %s was accepted", name)
		}
	}

	t.Run("set-once trigger", func(t *testing.T) {
		for name, col := range map[string]string{
			"amount_minor change":   `amount_minor=amount_minor+100`,
			"account change":        `account_id='acct_Other0001'`,
			"reason change":         `reason='duplicate'`,
			"requested_at change":   `requested_at=requested_at+interval '1 second'`,
			"first_sent_at unset":   `first_sent_at=NULL`,
			"send_count decrease":   `send_count=send_count-1`,
			"pin changed":           `stripe_refund_id='re_srs_other'`,
			"pin unset":             `stripe_refund_id=NULL,pinned_at=NULL`,
			"principal change":      `principal_id=gen_random_uuid()`,
			"payment_intent change": `payment_intent_id='pi_other'`,
		} {
			refuse("stripe_refunds "+name, `UPDATE payments.stripe_refunds SET `+col+` WHERE id=$1`, r1)
		}
		var amount int64
		if err := e.f.owner.QueryRow(ctx, `SELECT amount_minor FROM payments.stripe_refunds WHERE id=$1`, r1).Scan(&amount); err != nil || amount != 1000 {
			t.Fatalf("a refused update changed the row: %d %v", amount, err)
		}
		// the guarded columns as NULL -> value are exercised by the real worker (pin, sent markers).
		var pin, opRef string
		if err := e.f.owner.QueryRow(ctx, `SELECT r.stripe_refund_id,op.provider_reference FROM payments.stripe_refunds r JOIN integration.operations op ON op.id=r.id WHERE r.id=$1`, r1).Scan(&pin, &opRef); err != nil || pin == "" || pin != opRef {
			t.Fatalf("pin %q op.provider_reference %q: stripe_refund_id must also become the op reference", pin, opRef)
		}
	})

	t.Run("fact trigger and append-only facts", func(t *testing.T) {
		okHash := hashOf(r1, "SUCCEEDED")
		pin1 := e.fake.RefundByRef(r1)
		// SUCCEEDED amount must equal the refund amount (I05): r2 is pinned and pending.
		refuseFact("SUCCEEDED with the wrong amount", r2, o1.attempt, "SUCCEEDED", 400, e.fake.RefundByRef(r2), nil, okHash)
		// REJECTED requires no pin and excludes every other kind.
		refuseFact("REJECTED for a pinned refund", r2, o1.attempt, "REJECTED", 500, nil, "first_send_rejected", okHash)
		refuseFact("REJECTED next to a FAILED fact", r5, o1.attempt, "REJECTED", 200, nil, "first_send_rejected", okHash)
		refuseFact("FAILED next to a REJECTED fact", r3, o1.attempt, "FAILED", 300, "re_srs_x", "declined", okHash)
		// SUCCEEDED after FAILED is refused (the product would raise REFUND_CONFLICTING instead).
		refuseFact("SUCCEEDED after FAILED", r5, o1.attempt, "SUCCEEDED", 200, e.fake.RefundByRef(r5), nil, okHash)
		// FAILED may follow SUCCEEDED (late failure).
		if err := factTx(true, r1, o1.attempt, "FAILED", 1000, pin1, "declined", okHash); err != nil {
			t.Fatalf("FAILED after SUCCEEDED (late failure, F-R3) refused: %v", err)
		}
		// Composite FK: a fact of attempt X referencing a refund of attempt Y is refused.
		// The BEFORE INSERT guard (refund looked up by tenant, store, attempt AND id) refuses first with 23514;
		// the composite FK itself is proved by catalog (srsFK). Either refusal is the contract behaviour.
		if err := factTx(false, r4, o1.attempt, "FAILED", 1000, e.fake.RefundByRef(r4), "declined", okHash); sqlState(err) != "23503" && sqlState(err) != "23514" {
			t.Errorf("fact of attempt X for a refund of attempt Y: want 23503/23514, got %v", err)
		}
		// Append-only for the writer roles (checked as those roles, not superuser).
		for role, sql := range map[string]string{
			"commerce_checkout_writer":    `UPDATE payments.refund_facts SET amount_minor=1`,
			"commerce_integration_writer": `INSERT INTO payments.refund_facts(tenant_id,store_id,refund_id,attempt_id,kind,amount_minor,currency,source_report_hash) SELECT tenant_id,store_id,refund_id,attempt_id,'REJECTED',0,'TWD',source_report_hash FROM payments.refund_facts LIMIT 1`,
		} {
			tx, err := e.f.owner.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if _, err = tx.Exec(ctx, `SET LOCAL ROLE `+role); err != nil {
				t.Fatal(err)
			}
			if _, err = tx.Exec(ctx, sql); err == nil {
				t.Errorf("%s changed facts", role)
			} else if sqlState(err) != "42501" && !strings.Contains(err.Error(), "permission") {
				t.Errorf("%s: want a permission failure, got %v", role, err)
			}
			tx.Rollback(ctx)
		}
		for role, sql := range map[string]string{"commerce_checkout_writer": `DELETE FROM payments.refund_facts`, "commerce_integration_writer": `DELETE FROM payments.refund_facts`} {
			tx, _ := e.f.owner.Begin(ctx)
			_, _ = tx.Exec(ctx, `SET LOCAL ROLE `+role)
			if _, err := tx.Exec(ctx, sql); err == nil {
				t.Errorf("%s deleted facts", role)
			}
			tx.Rollback(ctx)
		}
		// Row Level Security: the checkout writer sees no facts without its GUC scope.
		tx, _ := e.f.owner.Begin(ctx)
		_, _ = tx.Exec(ctx, `SET LOCAL ROLE commerce_checkout_writer`)
		var n int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM payments.refund_facts`).Scan(&n); err != nil || n != 0 {
			t.Errorf("checkout_writer read %d facts without app.tenant_id/app.store_id: %v", n, err)
		}
		tx.Rollback(ctx)
	})

	t.Run("mark_stripe_refund_sent returns CLOSED, not SEND, while REFUND_HISTORY or CONFLICTING_REPORT exists", func(t *testing.T) {
		// §4.4 (mark_stripe_refund_sent row): only a never-sent refund CLOSED "because the send window passed"
		// becomes REJECTED; a review-caused CLOSED keeps the refund REQUESTED with its capacity held (the
		// worker records stripe_refund_uncertain and retries). So each round waits for that processed
		// claim, not for job completion, and uses a fresh order whose review stays in place (an unreviewed
		// leftover would be sent during the next round).
		for _, reason := range []string{"REFUND_HISTORY", "CONFLICTING_REPORT"} {
			o := e.payMore(t, o1) // needs the running worker (capture), so before the stop
			e.grant(t, o, "orders:read", "payments:refund")
			e.stopAllWorkers()
			restarted := false
			id := func() string {
				defer func() {
					if !restarted && t.Failed() {
						e.startWorker(t) // keep later subtests runnable after an early failure
					}
				}()
				id := e.mustRefund(t, o, 300, "requested_by_customer")
				srqReview(t, e, o.attempt, reason)
				return id
			}()
			posts := e.fake.RefundPosts()
			restarted = true
			e.startWorker(t)
			e.awaitRefund(t, "a claim that met CLOSED", id, o.attempt, 45*time.Second, `SELECT EXISTS(SELECT 1 FROM integration.operation_events WHERE operation_id=$1 AND reason_code='stripe_refund_uncertain')`)
			if e.fake.RefundPosts() != posts {
				t.Fatalf("%s on the attempt: the worker sent the refund (fail-open review policy, A2)", reason)
			}
			if n := e.count(t, `SELECT count(*) FROM payments.stripe_refunds WHERE id=$1 AND first_sent_at IS NULL`, id); n != 1 {
				t.Fatalf("%s: refund was marked sent", reason)
			}
			if n := e.count(t, `SELECT count(*) FROM payments.refund_facts WHERE refund_id=$1`, id); n != 0 {
				t.Fatalf("%s: a review-caused CLOSED released capacity (%d facts)", reason, n)
			}
		}
	})

	t.Run("the pre-send check of refund B does not suppress after refund A was sent", func(t *testing.T) {
		o := e.payMore(t, o1)
		e.grant(t, o, "orders:read", "payments:refund")
		a1 := e.mustRefund(t, o, 1000, "requested_by_customer")
		e.awaitRefundFact(t, a1, o.attempt, "SUCCEEDED")
		b1 := e.mustRefund(t, o, 500, "requested_by_customer")
		e.awaitRefundFact(t, b1, o.attempt, "SUCCEEDED")
		if n := e.count(t, `SELECT count(*) FROM payments.stripe_refunds WHERE id=$1 AND suppressed_at IS NULL AND first_sent_at IS NOT NULL`, b1); n != 1 || e.hasAttemptReview(t, o.attempt, "REFUND_HISTORY") {
			t.Fatal("refund B was suppressed although Stripe's amount_refunded equals what was sent (integration_writer cannot read refunds/facts: RLS policies missing)")
		}
	})

	t.Run("the webhook commit increments stripe_refunds.signal_count", func(t *testing.T) {
		before := e.count(t, `SELECT signal_count FROM payments.stripe_refunds WHERE id=$1`, r1)
		body := e.fake.RefundEventBody(srhEvent("srs"), "refund.updated", e.fake.RefundByRef(r1), false)
		if status := e.deliverRaw(t, o1.endpoint, o1.secret, body); status != 200 {
			t.Fatalf("webhook answered %d", status)
		}
		if after := e.count(t, `SELECT signal_count FROM payments.stripe_refunds WHERE id=$1`, r1); after != before+1 {
			t.Fatalf("signal_count %d -> %d", before, after)
		}
	})

	t.Run("the request definer sees existing refunds in the capacity sum (not as superuser)", func(t *testing.T) {
		var want int64
		if err := e.f.owner.QueryRow(ctx, `SELECT $2::bigint-coalesce((SELECT sum(r.amount_minor) FROM payments.stripe_refunds r WHERE r.attempt_id=$1
		 AND NOT EXISTS(SELECT 1 FROM payments.refund_facts f WHERE f.refund_id=r.id AND f.kind IN ('FAILED','CANCELED','REJECTED'))),0)::bigint`, o1.attempt, o1.captured).Scan(&want); err != nil {
			t.Fatal(err)
		}
		if got := e.refundable(t, o1); got != want {
			t.Fatalf("merchant refundable %d, RD3 capacity says %d (the definer reads zero rows under FORCE RLS without its policy)", got, want)
		}
		if want >= o1.captured {
			t.Fatal("fixture: nothing is held on o1")
		}
		// a request for exactly the remaining amount is accepted, one unit of 100 more is not
		if status, out := e.request(o1, o1.token(), t04Key("srs-cap"), rfxBody(want+100, "requested_by_customer", want)); status != 422 || srqCode(out) != "exceeds_refundable" {
			t.Fatalf("over the sum: %d %v", status, out)
		}
	})

	t.Run("populated upgrade from 0061", func(t *testing.T) {
		t.Skip("NOT_RUN: migrations.Apply has no partial-apply hook; a database frozen at 0061 with rows cannot be built without re-implementing the migrator")
	})
}
