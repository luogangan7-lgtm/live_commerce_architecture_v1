package foundation_test

// TCV02 TestTaiwanCvsSchema (contracts/taiwan-cvs-logistics-v1.md §10 TCV02, §4.1, §4.2, §4.3 incl. the round-3/4 grant bullets and the
// X9 pool note, §16.1, §16.5, §16.8). Tier REAL_PG. Prefix `tcs`. Written from the contract: catalog reads (pg_constraint, pg_policies,
// pg_proc, ACLs) for structure; LIKE-copies of the real tables (defaults + CHECKs, no FKs/triggers) for column CHECK behaviour; real
// role logins (commerce_runtime, commerce_checkout_runtime, commerce_buyer_runtime) for every privilege negative.
//
// Owner-pool writes are disclosed fixtures: (1) planting `ecpay.*` / `buyer.*` pickup rows so the merchant-plant negatives have a target,
// (2) TEMP LIKE-copy tables that vanish at ROLLBACK. No product row is fabricated.
//
// NOT_RUN clauses (recorded in output/cvs-tests/NOT_RUN.md): "populated upgrade over 0063-0066" and "one begin_hold after an upgrade": the
// migrator has no partial-apply hook (same limit as RF03/MF02). The fresh install + second Apply of the fixture covers "exactly one
// checkout.begin_hold after a fresh install" (C1 ordering).
// Exercised: migrations 0072/0073/post_river 0017 objects, fulfillment.ecpay_trade_no vs ecpay.MerchantTradeNo.

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"livecommerce/internal/integrations/shipping/ecpay"
	"livecommerce/internal/platform"
)

func tcsBool(t *testing.T, f *testFixture, label, q string, args ...any) bool {
	t.Helper()
	var ok bool
	if err := f.owner.QueryRow(context.Background(), q, args...).Scan(&ok); err != nil {
		t.Fatalf("%s: %v", label, err)
	}
	return ok
}

// tcsDefs asserts the CHECK constraints of a table together contain every fragment (case-insensitive) and none of the absent ones.
func tcsDefs(t *testing.T, f *testFixture, table string, fragments ...string) {
	t.Helper()
	var defs string
	if err := f.owner.QueryRow(context.Background(), `SELECT coalesce(string_agg(pg_get_constraintdef(oid),' | '),'') FROM pg_constraint WHERE conrelid=to_regclass($1) AND contype='c'`, table).Scan(&defs); err != nil {
		t.Fatal(err)
	}
	if defs == "" {
		t.Errorf("TCV02 %s has no CHECK constraints (table missing?)", table)
	}
	defs = strings.ToLower(defs)
	for _, fr := range fragments {
		if !strings.Contains(defs, strings.ToLower(fr)) {
			t.Errorf("TCV02 %s CHECKs lack %q", table, fr)
		}
	}
}

func tcsPriv(t *testing.T, f *testFixture, role, object, priv string, want bool) {
	t.Helper()
	if got := tcsBool(t, f, "priv", `SELECT has_table_privilege($1,$2,$3)`, role, object, priv); got != want {
		t.Errorf("TCV02 has_table_privilege(%s,%s,%s)=%v want %v", role, object, priv, got, want)
	}
}

func tcsColPriv(t *testing.T, f *testFixture, role, object, col, priv string, want bool) {
	t.Helper()
	if got := tcsBool(t, f, "colpriv", `SELECT has_column_privilege($1,$2,$3,$4)`, role, object, col, priv); got != want {
		t.Errorf("TCV02 has_column_privilege(%s,%s.%s,%s)=%v want %v", role, object, col, priv, got, want)
	}
}

// tcsPolicy asserts a policy exists with the given command/kind/role and that qual+check contain (and do not contain) fragments.
func tcsPolicy(t *testing.T, f *testFixture, schema, table, name, role, cmd string, permissive bool, has, hasNot []string) {
	t.Helper()
	kind := "PERMISSIVE"
	if !permissive {
		kind = "RESTRICTIVE"
	}
	var qual, check *string
	err := f.owner.QueryRow(context.Background(), `SELECT qual,with_check FROM pg_policies WHERE schemaname=$1 AND tablename=$2 AND policyname=$3 AND $4::name=ANY(roles) AND cmd=$5 AND permissive=$6`,
		schema, table, name, role, cmd, kind).Scan(&qual, &check)
	if err != nil {
		t.Errorf("TCV02 policy %s ON %s.%s TO %s FOR %s (%s) missing: %v", name, schema, table, role, cmd, kind, err)
		return
	}
	text := ""
	if qual != nil {
		text += *qual
	}
	if check != nil {
		text += *check
	}
	for _, fr := range has {
		if !strings.Contains(text, fr) {
			t.Errorf("TCV02 policy %s lacks %q: %s", name, fr, text)
		}
	}
	for _, fr := range hasNot {
		if strings.Contains(text, fr) {
			t.Errorf("TCV02 policy %s must not contain %q: %s", name, fr, text)
		}
	}
}

// tcsRows inserts a valid baseline into a TEMP LIKE-copy of table and then each case; wantErr "" means accepted, else the SQLSTATE.
// keepIndexes copies unique indexes too (LIKE ... INCLUDING ALL) for the uniqueness cases.
func tcsRows(t *testing.T, f *testFixture, table string, keepIndexes bool, base map[string]string, cases map[string]map[string]string, wantErr string) {
	t.Helper()
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
	like := "INCLUDING DEFAULTS INCLUDING CONSTRAINTS"
	if keepIndexes {
		like = "INCLUDING ALL"
	}
	for _, name := range names {
		ctx := context.Background()
		tx, err := f.owner.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = tx.Exec(ctx, `CREATE TEMP TABLE tcs_case (LIKE `+table+` `+like+`) ON COMMIT DROP`); err != nil {
			tx.Rollback(ctx)
			t.Fatalf("TCV02 %s: cannot LIKE-copy (table missing?): %v", table, err)
		}
		if _, err = tx.Exec(ctx, `INSERT INTO tcs_case (`+strings.Join(cols, ",")+`) VALUES (`+values(nil)+`)`); err != nil {
			tx.Rollback(ctx)
			t.Fatalf("TCV02 %s: the all-valid baseline row is refused (schema or fixture error): %v", table, err)
		}
		_, err = tx.Exec(ctx, `INSERT INTO tcs_case (`+strings.Join(cols, ",")+`) VALUES (`+values(cases[name])+`)`)
		tx.Rollback(ctx)
		if got := sqlState(err); got != wantErr {
			t.Errorf("TCV02 %s case %q: want SQLSTATE %q, got %q (%v)", table, name, wantErr, got, err)
		}
	}
}

func tcsU(n int) string { return fmt.Sprintf("'00000000-0000-4000-8000-0000000000%02d'::uuid", n) }

// tcsExec runs the statements as a real role login inside one transaction (rolled back) with the given GUCs.
func tcsAs(t *testing.T, f *testFixture, authority string, gucs map[string]string, fn func(ctx context.Context, tx pgx.Tx)) {
	t.Helper()
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, bcRole(t, f, authority))
	if err != nil {
		t.Fatalf("login as %s: %v", authority, err)
	}
	defer conn.Close(ctx)
	tx, err := conn.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	for k, v := range gucs {
		if _, err := tx.Exec(ctx, `SELECT set_config($1,$2,true)`, k, v); err != nil {
			t.Fatal(err)
		}
	}
	fn(ctx, tx)
}

// tcsSub runs stmt in a savepoint so a refusal does not poison the surrounding tx; it returns the SQLSTATE ("" = success) and rows affected.
func tcsSub(ctx context.Context, tx pgx.Tx, stmt string, args ...any) (string, int64) {
	sp, _ := tx.Begin(ctx)
	tag, err := sp.Exec(ctx, stmt, args...)
	if err != nil {
		sp.Rollback(ctx)
		return sqlState(err), 0
	}
	sp.Rollback(ctx)
	return "", tag.RowsAffected()
}

type tcsFn struct {
	schema, name, owner string
	definer             bool
	exec                []string // commerce_* roles (besides the owner) that may EXECUTE; nobody else, PUBLIC never
	stable              bool
}

func tcsFunctions() []tcsFn {
	const cw, iw = "commerce_checkout_writer", "commerce_integration_writer"
	const rt, cr, wk = "commerce_runtime", "commerce_checkout_runtime", "commerce_worker"
	return []tcsFn{
		{"integration", "register_ecpay_logistics", iw, true, []string{rt}, false},
		{"integration", "set_ecpay_logistics_enabled", iw, true, []string{rt}, false},
		{"fulfillment", "open_cvs_selection", cw, true, []string{cr}, false},
		{"fulfillment", "record_cvs_map_return", cw, true, []string{rt}, false},
		{"fulfillment", "verify_cvs_selection", cw, true, []string{rt, cr}, false},
		{"fulfillment", "read_cvs_selection", cw, true, []string{cr}, false},
		// contract 4.3 lists runtime, checkout_writer, worker; load_cvs_create (owner commerce_integration_writer) calls it, so that owner
		// needs EXECUTE too: recorded as a contract gap, accepted here.
		{"fulfillment", "ecpay_recipient_ok", "", false, []string{rt, cw, wk, iw}, false},
		{"fulfillment", "request_cvs_shipment", cw, true, []string{rt}, false},
		{"integration", "plan_cvs_create", iw, true, []string{cw}, false},
		{"fulfillment", "read_cvs_shipment_command", cw, true, []string{rt}, false},
		{"fulfillment", "cvs_order_payable", cw, true, []string{iw}, false},
		{"fulfillment", "settle_cvs_attempt", cw, false, nil, false},
		{"integration", "load_cvs_create", iw, true, []string{wk}, false},
		{"integration", "finish_cvs_create", iw, true, []string{wk}, false},
		{"fulfillment", "apply_cvs_create_result", cw, true, []string{iw}, false},
		{"fulfillment", "ingest_ecpay_status", cw, true, []string{rt}, false},
		{"fulfillment", "abandon_cvs_shipment", cw, true, []string{rt}, false},
		{"integration", "load_ecpay_key_for_status", iw, true, []string{rt}, false},
		{"integration", "load_ecpay_key_for_selection", iw, true, []string{rt, cr}, false},
		{"integration", "load_ecpay_key_for_merchant", iw, true, []string{rt}, false},
		{"fulfillment", "read_cvs_shipment", cw, true, []string{rt}, false},
		{"fulfillment", "read_buyer_cvs_shipment", cw, true, []string{cr}, true},
		{"fulfillment", "read_cvs_offer", cw, true, []string{cr}, true},
		{"fulfillment", "record_buyer_cvs_store", cw, true, []string{cr}, false},
		{"fulfillment", "record_collection", cw, true, []string{rt}, false},
		{"fulfillment", "set_cvs_store_settings", cw, true, []string{rt}, false},
		{"inventory", "release_pay_at_pickup", cw, true, []string{rt}, false},
	}
}

func TestTaiwanCvsSchema(t *testing.T) {
	f := fixture(t)
	ctx := context.Background()

	newTables := []string{"integration.ecpay_logistics_profiles", "fulfillment.cvs_selections", "fulfillment.cvs_shipments", "fulfillment.cvs_shipment_events", "fulfillment.cvs_store_settings"}

	t.Run("objects: migrations, FORCE RLS, no PUBLIC grant, COMMENT ON", func(t *testing.T) {
		for _, v := range []string{"0072_taiwan_cvs_logistics.sql", "0073_taiwan_cvs_functions.sql"} {
			if !tcsBool(t, f, v, `SELECT EXISTS(SELECT 1 FROM public.lc_schema_migrations WHERE version=$1 AND checksum<>'')`, v) {
				t.Errorf("migration %s is not recorded", v)
			}
		}
		for _, tbl := range newTables {
			if !tcsBool(t, f, tbl, `SELECT c.relrowsecurity AND c.relforcerowsecurity FROM pg_class c WHERE c.oid=to_regclass($1)`, tbl) {
				t.Errorf("%s must exist with ENABLE + FORCE ROW LEVEL SECURITY", tbl)
			}
			if tcsBool(t, f, tbl, `SELECT EXISTS(SELECT 1 FROM aclexplode(coalesce((SELECT relacl FROM pg_class WHERE oid=to_regclass($1)),'{}'::aclitem[])) a WHERE a.grantee=0)`, tbl) {
				t.Errorf("%s has a PUBLIC privilege", tbl)
			}
			if !tcsBool(t, f, tbl, `SELECT obj_description(to_regclass($1),'pg_class') IS NOT NULL`, tbl) {
				t.Errorf("PROCESS section 5: %s lacks COMMENT ON", tbl)
			}
		}
		for _, fn := range tcsFunctions() {
			if !tcsBool(t, f, fn.name, `SELECT count(*)>0 AND bool_and(obj_description(p.oid,'pg_proc') IS NOT NULL) FROM pg_proc p WHERE p.pronamespace=$1::regnamespace AND p.proname=$2`, fn.schema, fn.name) {
				t.Errorf("PROCESS section 5: %s.%s missing or without COMMENT ON", fn.schema, fn.name)
			}
		}
	})

	t.Run("exactly one checkout.begin_hold with 10 arguments (post_river ordering, C1)", func(t *testing.T) {
		var n, args int
		if err := f.owner.QueryRow(ctx, `SELECT count(*),coalesce(max(pronargs),0) FROM pg_proc WHERE pronamespace='checkout'::regnamespace AND proname='begin_hold'`).Scan(&n, &args); err != nil {
			t.Fatal(err)
		}
		if n != 1 || args != 10 {
			t.Fatalf("checkout.begin_hold: %d function(s), max %d args; want exactly one with 10 args (the 8-arg signature must be dropped)", n, args)
		}
		if !tcsBool(t, f, "begin_hold grant", `SELECT has_function_privilege('commerce_checkout_runtime',p.oid,'EXECUTE') AND NOT has_function_privilege('commerce_runtime',p.oid,'EXECUTE') AND NOT has_function_privilege('commerce_buyer_runtime',p.oid,'EXECUTE') AND p.prosecdef
		  AND pg_get_userbyid(p.proowner)=(SELECT pg_get_userbyid(proowner) FROM pg_proc WHERE proname='expire_held' AND pronamespace='checkout'::regnamespace)
		  FROM pg_proc p WHERE p.pronamespace='checkout'::regnamespace AND p.proname='begin_hold'`) {
			t.Error("begin_hold keeps its owner, SECURITY DEFINER and EXECUTE only for commerce_checkout_runtime")
		}
	})

	t.Run("widened CHECKs keep every prior value", func(t *testing.T) {
		tcsDefs(t, f, "fulfillment.pickup_versions", "cvs_711", "cvs_familymart", "cvs_hilife", "cvs_okmart", "MANUAL_ATTESTED", "PROVIDER_DIRECTORY_VERIFIED", "BUYER_ENTERED")
		tcsDefs(t, f, "storefront.destination_snapshots", "home", "cvs_711", "cvs_familymart", "cvs_hilife", "cvs_okmart")
		tcsDefs(t, f, "fulfillment.service_versions", "home", "cvs_711", "cvs_familymart", "cvs_hilife", "cvs_okmart", "binding_id")
		tcsDefs(t, f, "checkout.orders", "MANUAL_UNASSIGNED", "CANCELLED", "PAID_ALLOCATION_FAILED", "MERCHANT_SHIPPED", "PROVIDER_LABEL_CREATED",
			"payment_mode", "pay_at_pickup", "collection_state", "PENDING", "COLLECTED", "RETURNED", "REFUNDED_OFFLINE", "RESTOCKED")
		tcsDefs(t, f, "integration.merchant_accounts", "payuni", "stripe", "ecpay_logistics")
		tcsDefs(t, f, "integration.operations", "ecpay_logistics", "ecpay.cvs_create", "transactional", "MERCHANT")
		tcsDefs(t, f, "inventory.ledger", "ADJUST", "RESERVE", "RELEASE", "ALLOCATE", "DEALLOCATE", "SYSTEM_EXPIRY", "SYSTEM_PAYMENT", "checkout.payment.capture",
			"checkout.payment.close", "checkout.pay_at_pickup.commit", "fulfillment.pay_at_pickup.cancel", "fulfillment.pay_at_pickup.restock")
		tcsDefs(t, f, "checkout.events", "checkout.held", "checkout.expired", "checkout.payment_started", "checkout.payment_captured", "checkout.payment_closed", "checkout.pay_at_pickup_placed")
		if !tcsBool(t, f, "payment_mode default", `SELECT column_default LIKE '%card%' AND is_nullable='NO' FROM information_schema.columns WHERE table_schema='checkout' AND table_name='orders' AND column_name='payment_mode'`) {
			t.Error("checkout.orders.payment_mode must be NOT NULL DEFAULT 'card'")
		}
		if !tcsBool(t, f, "collection_state nullable", `SELECT is_nullable='YES' FROM information_schema.columns WHERE table_schema='checkout' AND table_name='orders' AND column_name='collection_state'`) {
			t.Error("checkout.orders.collection_state must be nullable (NULL for card)")
		}
		if !tcsBool(t, f, "principal nullable", `SELECT is_nullable='YES' FROM information_schema.columns WHERE table_schema='fulfillment' AND table_name='pickup_versions' AND column_name='principal_id'`) {
			t.Error("pickup_versions.principal_id must be nullable (BUYER_ENTERED)")
		}
		// all existing orders backfilled: no row without a payment_mode, no card row with a collection state
		if n := countRows(t, f.owner, `SELECT count(*) FROM checkout.orders WHERE payment_mode IS NULL OR (payment_mode='card' AND collection_state IS NOT NULL)`); n != 0 {
			t.Errorf("%d orders violate the payment_mode/collection_state pairing", n)
		}
	})

	t.Run("column CHECK behaviour on LIKE-copies", func(t *testing.T) {
		pv := map[string]string{"tenant_id": tcsU(1), "store_id": tcsU(2), "kind": "'cvs_711'", "namespace": "'buyer.x'", "code": "'123456'", "version": "1", "name": "'S'", "address": "'A'",
			"verification_kind": "'BUYER_ENTERED'", "evidence_ref": "'e'", "principal_id": "NULL", "attested_at": "'2026-01-01 00:00:00+00'", "valid_until": "'2026-01-02 00:00:00+00'"}
		tcsRows(t, f, "fulfillment.pickup_versions", false, pv, map[string]map[string]string{
			"BUYER_ENTERED with a principal":          {"principal_id": tcsU(5)},
			"MANUAL_ATTESTED without a principal":     {"verification_kind": "'MANUAL_ATTESTED'"},
			"PROVIDER_DIRECTORY_VERIFIED, no princip": {"verification_kind": "'PROVIDER_DIRECTORY_VERIFIED'"},
			"unknown verification kind":               {"verification_kind": "'SELF_VERIFIED'", "principal_id": tcsU(5)},
			"unknown chain":                           {"kind": "'cvs_bogus'"},
			"valid_until beyond 7 days":               {"valid_until": "'2026-01-09 00:00:00+00'"},
		}, "23514")
		tcsRows(t, f, "fulfillment.pickup_versions", false, pv, map[string]map[string]string{
			"control BUYER_ENTERED":       {},
			"hilife":                      {"kind": "'cvs_hilife'"},
			"okmart":                      {"kind": "'cvs_okmart'"},
			"familymart":                  {"kind": "'cvs_familymart'"},
			"provider directory verified": {"verification_kind": "'PROVIDER_DIRECTORY_VERIFIED'", "principal_id": tcsU(5)},
			"manual attested":             {"verification_kind": "'MANUAL_ATTESTED'", "principal_id": tcsU(5)},
			"24 h validity":               {"valid_until": "'2026-01-02 00:00:00+00'"},
		}, "")

		profile := map[string]string{"tenant_id": tcsU(1), "store_id": tcsU(2), "connection_id": tcsU(3), "mode": "'C2C'", "endpoint_id": tcsU(4), "enabled": "true",
			"qualified_credential_version": "1", "qualified_at": "now()", "version": "1", "updated_at": "now()"}
		tcsRows(t, f, "integration.ecpay_logistics_profiles", false, profile, map[string]map[string]string{
			"mode DHL":                       {"mode": "'DHL'"},
			"qualified version without time": {"qualified_at": "NULL"},
			"qualified time without version": {"qualified_credential_version": "NULL"},
			"version 0":                      {"version": "0"},
		}, "23514")
		tcsRows(t, f, "integration.ecpay_logistics_profiles", false, profile, map[string]map[string]string{
			"B2C":                        {"mode": "'B2C'"},
			"never qualified":            {"qualified_at": "NULL", "qualified_credential_version": "NULL"},
			"ok_verified defaults false": {},
		}, "")

		sel := map[string]string{"tenant_id": tcsU(1), "store_id": tcsU(2), "owner_id": tcsU(3), "id": tcsU(4), "session_id": tcsU(5), "cart_id": tcsU(6), "cart_version": "1", "kind": "'cvs_711'",
			"connection_id": tcsU(7), "credential_version": "1", "logistics_subtype": "'UNIMARTC2C'", "nonce_sha256": "decode(repeat('ab',32),'hex')", "state": "'OPEN'",
			"return_origin": "'https://a.example.com'", "return_path": "'/zh-TW/products/abc'", "created_at": "'2026-01-01 00:00:00+00'", "expires_at": "'2026-01-01 00:10:00+00'",
			"updated_at": "'2026-01-01 00:00:00+00'", "version": "1"}
		tcsRows(t, f, "fulfillment.cvs_selections", false, sel, map[string]map[string]string{
			"unknown chain":                {"kind": "'cvs_bogus'"},
			"unknown subtype":              {"logistics_subtype": "'POST'"},
			"nonce 31 bytes":               {"nonce_sha256": "decode(repeat('ab',31),'hex')"},
			"state unknown":                {"state": "'DONE'"},
			"returned store id 10 chars":   {"returned_store_id": "'1234567890'"},
			"returned store id empty":      {"returned_store_id": "''"},
			"returned store id symbol":     {"returned_store_id": "'12-45'"},
			"reject code uppercase":        {"state": "'REJECTED'", "reject_code": "'Bad'"},
			"reject code 41 chars":         {"state": "'REJECTED'", "reject_code": "repeat('a',41)"},
			"VERIFIED without pickup":      {"state": "'VERIFIED'"},
			"pickup with OPEN":             {"pickup_id": tcsU(9)},
			"REJECTED without reject_code": {"state": "'REJECTED'"},
			"reject_code with OPEN":        {"reject_code": "'expired'"},
			"lifetime 16 minutes":          {"expires_at": "'2026-01-01 00:16:00+00'"},
			"lifetime zero":                {"expires_at": "'2026-01-01 00:00:00+00'"},
			"country not TW":               {"country": "'JP'"},
			"return path claim page":       {"return_path": "'/zh-TW/claim'"},
			"return path unknown locale":   {"return_path": "'/fr/products/x'"},
			"return path empty product":    {"return_path": "'/en/products/'"},
			"return path 65-char product":  {"return_path": "'/en/products/'||repeat('a',65)"},
			"return path with query":       {"return_path": "'/en/products/a?x=1'"},
			"version 0":                    {"version": "0"},
			"cart version 0":               {"cart_version": "0"},
		}, "23514")
		tcsRows(t, f, "fulfillment.cvs_selections", false, sel, map[string]map[string]string{
			"control": {}, "15 minute lifetime": {"expires_at": "'2026-01-01 00:15:00+00'"}, "hilife": {"kind": "'cvs_hilife'", "logistics_subtype": "'HILIFEC2C'"},
			"okmart": {"kind": "'cvs_okmart'", "logistics_subtype": "'OKMARTC2C'"}, "B2C subtype": {"logistics_subtype": "'FAMI'"},
			"VERIFIED with pickup": {"state": "'VERIFIED'", "pickup_id": tcsU(9)}, "REJECTED with code": {"state": "'REJECTED'", "reject_code": "'store_not_in_directory'"},
			"9-char store id": {"returned_store_id": "'123456789'"}, "zh-CN": {"return_path": "'/zh-CN/products/A_b-1'"},
		}, "")

		ship := map[string]string{"tenant_id": tcsU(1), "store_id": tcsU(2), "owner_id": tcsU(3), "order_id": tcsU(4), "attempt": "1", "connection_id": tcsU(5), "credential_version": "1",
			"environment": "'SANDBOX'", "logistics_subtype": "'UNIMARTC2C'", "receiver_store_id": "'131386'", "pickup_id": tcsU(6), "goods_amount": "1000",
			"merchant_trade_no": "'LCABCDEFGHIJKLMNOPQR'", "operation_id": tcsU(7), "state": "'REQUESTED'", "principal_id": tcsU(8),
			"created_at": "now()", "updated_at": "now()", "version": "1"}
		tcsRows(t, f, "fulfillment.cvs_shipments", false, ship, map[string]map[string]string{
			"attempt 0":                         {"attempt": "0"},
			"attempt 6":                         {"attempt": "6"},
			"environment TEST":                  {"environment": "'TEST'"},
			"receiver store id 7 chars":         {"receiver_store_id": "'1234567'"},
			"receiver store id empty":           {"receiver_store_id": "''"},
			"receiver store id with space":      {"receiver_store_id": "'12 34'"},
			"goods 0":                           {"goods_amount": "0"},
			"goods 20001":                       {"goods_amount": "20001"},
			"collection differs from goods":     {"collection_amount": "999"},
			"collection 0":                      {"collection_amount": "0", "goods_amount": "1"},
			"collection 20001":                  {"collection_amount": "20001", "goods_amount": "20001"},
			"trade no lower case":               {"merchant_trade_no": "'LCabcdefghijklmnopqr'"},
			"trade no 19 chars":                 {"merchant_trade_no": "'LCABCDEFGHIJKLMNOPQ'"},
			"trade no digit 1 (outside base32)": {"merchant_trade_no": "'LC1BCDEFGHIJKLMNOPQR'"},
			"trade no wrong prefix":             {"merchant_trade_no": "'XXABCDEFGHIJKLMNOPQR'"},
			"state unknown":                     {"state": "'DELIVERED'"},
			"CREATED without logistics id":      {"state": "'CREATED'"},
			"AT_DC without logistics id":        {"state": "'AT_DC'"},
			"AT_STORE without logistics id":     {"state": "'AT_STORE'"},
			"PICKED_UP without logistics id":    {"state": "'PICKED_UP'"},
			"UNCLAIMED without logistics id":    {"state": "'UNCLAIMED'"},
			"logistics id empty":                {"provider_logistics_id": "''"},
			"logistics id 41 chars":             {"provider_logistics_id": "repeat('1',41)"},
			"logistics id with space":           {"provider_logistics_id": "'1 2'"},
			"payment no 41 chars":               {"cvs_payment_no": "repeat('1',41)"},
			"validation no 41 chars":            {"cvs_validation_no": "repeat('1',41)"},
			"shipment no 41 chars":              {"shipment_no": "repeat('1',41)"},
			"status code letters":               {"last_status_code": "'abc'"},
			"status code 9 digits":              {"last_status_code": "'123456789'"},
			"result code upper case":            {"result_code": "'Ecpay.Rejected'"},
			"version 0":                         {"version": "0"},
		}, "23514")
		tcsRows(t, f, "fulfillment.cvs_shipments", false, ship, map[string]map[string]string{
			"control": {}, "4-char store id 1328 (F17)": {"receiver_store_id": "'1328'"}, "6-char store": {"receiver_store_id": "'006598'"},
			"goods 20000 with matching collection": {"goods_amount": "20000", "collection_amount": "20000"},
			"collection equals goods":              {"collection_amount": "1000"},
			"FAILED without logistics id":          {"state": "'FAILED'"}, "UNKNOWN without id": {"state": "'UNKNOWN'"}, "ABANDONED without id": {"state": "'ABANDONED'"},
			"CREATED with id":            {"state": "'CREATED'", "provider_logistics_id": "'1000001'"},
			"codes at the 40-char bound": {"provider_logistics_id": "repeat('1',40)", "cvs_payment_no": "repeat('a',40)", "cvs_validation_no": "'a-_'||repeat('1',37)", "shipment_no": "repeat('9',40)"},
			"attempt 5":                  {"attempt": "5"}, "LIVE": {"environment": "'LIVE'"}, "B2C subtype": {"logistics_subtype": "'FAMI'"},
			"result code": {"state": "'FAILED'", "result_code": "'ecpay.not_sent.credential_unavailable'"}, "status code": {"last_status_code": "'2030'"},
		}, "")

		ev := map[string]string{"tenant_id": tcsU(1), "store_id": tcsU(2), "order_id": tcsU(3), "attempt": "1", "source": "'ecpay_status'", "body_sha256": "decode(repeat('cd',32),'hex')",
			"provider_code": "'2030'", "provider_message": "'ok'", "provider_updated_at": "'2026/09/30 10:00:00'"}
		tcsRows(t, f, "fulfillment.cvs_shipment_events", false, ev, map[string]map[string]string{
			"source unknown":            {"source": "'webhook'"},
			"event code upper case":     {"event_code": "'Ecpay.Code'"},
			"body hash 31 bytes":        {"body_sha256": "decode(repeat('cd',31),'hex')"},
			"provider code letters":     {"provider_code": "'2O30'"},
			"provider code 9 digits":    {"provider_code": "'123456789'"},
			"message 201 chars":         {"provider_message": "repeat('m',201)"},
			"message control char":      {"provider_message": "'a'||chr(7)"},
			"update date ISO format":    {"provider_updated_at": "'2026-09-30 10:00:00'"},
			"update date slash no time": {"provider_updated_at": "'2026/09/30'"},
		}, "23514")
		tcsRows(t, f, "fulfillment.cvs_shipment_events", false, ev, map[string]map[string]string{
			"control": {}, "local event": {"source": "'local'", "event_code": "'ecpay.code_nonconforming'", "body_sha256": "NULL", "provider_code": "NULL", "provider_message": "NULL", "provider_updated_at": "NULL"},
			"query source": {"source": "'ecpay_query'"}, "200-char message": {"provider_message": "repeat('m',200)"},
		}, "")

		st := map[string]string{"tenant_id": tcsU(1), "store_id": tcsU(2), "pay_at_pickup_max_open": "20", "version": "1", "updated_at": "now()"}
		tcsRows(t, f, "fulfillment.cvs_store_settings", false, st, map[string]map[string]string{
			"unknown chain":                {"enabled_chains": "ARRAY['cvs_bogus']::text[]"},
			"cap 0":                        {"pay_at_pickup_max_twd": "0"},
			"cap 20001":                    {"pay_at_pickup_max_twd": "20001"},
			"max_open 0":                   {"pay_at_pickup_max_open": "0"},
			"max_open 501":                 {"pay_at_pickup_max_open": "501"},
			"pay-at-pickup on without cap": {"pay_at_pickup_enabled": "true"},
			"version 0":                    {"version": "0"},
		}, "23514")
		tcsRows(t, f, "fulfillment.cvs_store_settings", false, st, map[string]map[string]string{
			"defaults (all four chains, off)": {}, "on with a 20000 cap": {"pay_at_pickup_enabled": "true", "pay_at_pickup_max_twd": "20000"},
			"cap set while off": {"pay_at_pickup_max_twd": "1"}, "max_open 500": {"pay_at_pickup_max_open": "500"}, "no chain": {"enabled_chains": "'{}'::text[]"},
			"one chain": {"enabled_chains": "ARRAY['cvs_okmart']::text[]"},
		}, "")
		if !tcsBool(t, f, "settings default chains", `SELECT (SELECT column_default FROM information_schema.columns WHERE table_schema='fulfillment' AND table_name='cvs_store_settings' AND column_name='enabled_chains') LIKE '%cvs_711%cvs_familymart%cvs_hilife%cvs_okmart%'`) {
			t.Error("enabled_chains defaults to all four chains (§16.5)")
		}
	})

	t.Run("uniqueness and indexes", func(t *testing.T) {
		ship := map[string]string{"tenant_id": tcsU(1), "store_id": tcsU(2), "owner_id": tcsU(3), "order_id": tcsU(4), "attempt": "1", "connection_id": tcsU(5), "credential_version": "1",
			"environment": "'SANDBOX'", "logistics_subtype": "'UNIMARTC2C'", "receiver_store_id": "'131386'", "pickup_id": tcsU(6), "goods_amount": "1000",
			"merchant_trade_no": "'LCABCDEFGHIJKLMNOPQR'", "operation_id": tcsU(7), "state": "'REQUESTED'", "principal_id": tcsU(8), "created_at": "now()", "updated_at": "now()", "version": "1"}
		// a second attempt of the same order: refused while the first is live (one live shipment per order), accepted once it is FAILED/ABANDONED
		tcsRows(t, f, "fulfillment.cvs_shipments", true, ship, map[string]map[string]string{
			"second live attempt (REQUESTED)": {"attempt": "2", "merchant_trade_no": "'LCBBBBBBBBBBBBBBBBBB'", "operation_id": tcsU(17)},
			"second live attempt (UNKNOWN)":   {"attempt": "2", "merchant_trade_no": "'LCBBBBBBBBBBBBBBBBBB'", "operation_id": tcsU(17), "state": "'UNKNOWN'"},
			"same trade no on the connection": {"order_id": tcsU(14), "operation_id": tcsU(17)},
			"same operation id":               {"order_id": tcsU(14), "merchant_trade_no": "'LCBBBBBBBBBBBBBBBBBB'"},
			"same attempt key":                {"merchant_trade_no": "'LCBBBBBBBBBBBBBBBBBB'", "operation_id": tcsU(17)},
		}, "23505")
		// ...but a FAILED or ABANDONED predecessor frees the slot: baseline row FAILED, second REQUESTED
		base2 := map[string]string{}
		for k, v := range ship {
			base2[k] = v
		}
		base2["state"] = "'FAILED'"
		tcsRows(t, f, "fulfillment.cvs_shipments", true, base2, map[string]map[string]string{
			"live attempt after FAILED": {"attempt": "2", "merchant_trade_no": "'LCBBBBBBBBBBBBBBBBBB'", "operation_id": tcsU(17), "state": "'REQUESTED'"},
			"FAILED again":              {"attempt": "2", "merchant_trade_no": "'LCBBBBBBBBBBBBBBBBBB'", "operation_id": tcsU(17), "state": "'FAILED'"},
		}, "")
		base2["state"] = "'ABANDONED'"
		tcsRows(t, f, "fulfillment.cvs_shipments", true, base2, map[string]map[string]string{
			"live attempt after ABANDONED": {"attempt": "2", "merchant_trade_no": "'LCBBBBBBBBBBBBBBBBBB'", "operation_id": tcsU(17), "state": "'REQUESTED'"},
		}, "")
		ev := map[string]string{"tenant_id": tcsU(1), "store_id": tcsU(2), "order_id": tcsU(3), "attempt": "1", "source": "'ecpay_status'", "body_sha256": "decode(repeat('cd',32),'hex')"}
		tcsRows(t, f, "fulfillment.cvs_shipment_events", true, ev, map[string]map[string]string{
			"duplicate notification body": {},
		}, "23505")

		for name, frags := range map[string][]string{
			"ecpay_one_enabled_profile":         {"ecpay_logistics_profiles", "unique", "enabled"},
			"ecpay_logistics_identity_unique":   {"merchant_accounts", "unique", "environment", "account_id", "ecpay_logistics"},
			"cvs_shipments_one_live":            {"cvs_shipments", "unique", "FAILED", "ABANDONED"},
			"orders_pay_at_pickup_open":         {"orders", "pay_at_pickup", "PENDING"},
			"ledger_pay_at_pickup_release_once": {"ledger", "unique", "DEALLOCATE"},
		} {
			var def string
			if err := f.owner.QueryRow(ctx, `SELECT coalesce((SELECT indexdef FROM pg_indexes WHERE indexname=$1),'')`, name).Scan(&def); err != nil || def == "" {
				t.Errorf("index %s missing", name)
				continue
			}
			for _, fr := range frags {
				if !strings.Contains(strings.ToLower(def), strings.ToLower(fr)) {
					t.Errorf("index %s lacks %q: %s", name, fr, def)
				}
			}
		}
		if !tcsBool(t, f, "one profile per store+environment", `SELECT EXISTS(SELECT 1 FROM pg_indexes WHERE tablename='merchant_accounts' AND schemaname='integration' AND indexdef ILIKE '%unique%' AND indexdef ILIKE '%ecpay_logistics%' AND indexdef ILIKE '%tenant_id%' AND indexdef ILIKE '%store_id%' AND indexdef ILIKE '%environment%')`) {
			t.Error("a partial unique (tenant_id, store_id, environment) WHERE provider='ecpay_logistics' must exist (TD2)")
		}
	})

	t.Run("triggers: order<->shipment guard from both sides, immutability, append-only, ledger guards", func(t *testing.T) {
		count := func(q string, args ...any) int { return countRows(t, f.owner, q, args...) }
		if count(`SELECT count(*) FROM pg_trigger WHERE tgrelid='checkout.orders'::regclass AND NOT tgisinternal AND tgconstraint<>0 AND tgdeferrable AND tginitdeferred AND pg_get_triggerdef(oid) ILIKE '%guard_cvs_shipment_state%'`) != 1 {
			t.Error("a DEFERRED constraint trigger guard_cvs_shipment_state() on checkout.orders is required")
		}
		if count(`SELECT count(*) FROM pg_trigger WHERE tgrelid='fulfillment.cvs_shipments'::regclass AND NOT tgisinternal AND tgconstraint<>0 AND tgdeferrable AND tginitdeferred AND pg_get_triggerdef(oid) ILIKE '%guard_cvs_shipment_state%'`) != 1 {
			t.Error("a DEFERRED constraint trigger guard_cvs_shipment_state() on fulfillment.cvs_shipments is required")
		}
		if count(`SELECT count(*) FROM pg_trigger WHERE tgrelid='fulfillment.cvs_shipments'::regclass AND NOT tgisinternal AND (tgtype & 2)=2 AND (tgtype & 16)=16`) < 1 {
			t.Error("a BEFORE UPDATE immutability trigger on cvs_shipments is required (identity columns, +1 version CAS)")
		}
		if count(`SELECT count(*) FROM pg_trigger WHERE tgrelid='fulfillment.cvs_shipment_events'::regclass AND NOT tgisinternal AND (tgtype & 2)=2 AND ((tgtype & 16)=16 OR (tgtype & 8)=8)`) < 1 {
			t.Error("cvs_shipment_events is append-only: a BEFORE UPDATE/DELETE trigger is required")
		}
		if count(`SELECT count(*) FROM pg_trigger WHERE tgrelid='fulfillment.service_versions'::regclass AND NOT tgisinternal AND pg_get_triggerdef(oid) ILIKE '%guard_api_service_binding%' AND (tgtype & 4)=4`) != 1 {
			t.Error("guard_api_service_binding() AFTER INSERT on service_versions is required")
		}
		if count(`SELECT count(*) FROM pg_trigger WHERE tgrelid='inventory.ledger'::regclass AND NOT tgisinternal AND pg_get_triggerdef(oid) ILIKE '%guard_pay_at_pickup_ledger%'`) < 1 {
			t.Error("inventory.guard_pay_at_pickup_ledger() must be attached to inventory.ledger")
		}
		// the ledger guard is the DEALLOCATE authority: both its body and guard_checkout_ledger mention DEALLOCATE
		for _, fn := range []string{"guard_pay_at_pickup_ledger", "guard_checkout_ledger"} {
			if !tcsBool(t, f, fn, `SELECT p.prosrc ILIKE '%DEALLOCATE%' AND pg_get_userbyid(p.proowner) IS NOT NULL FROM pg_proc p WHERE p.pronamespace='inventory'::regnamespace AND p.proname=$1`, fn) {
				t.Errorf("inventory.%s must handle DEALLOCATE rows", fn)
			}
		}
		if !tcsBool(t, f, "guard_checkout_ledger owner", `SELECT pg_get_userbyid(proowner)='commerce_inventory_writer' AND prosecdef AND NOT EXISTS(SELECT 1 FROM aclexplode(coalesce(proacl,acldefault('f',proowner))) a WHERE a.grantee=0) FROM pg_proc WHERE pronamespace='inventory'::regnamespace AND proname='guard_checkout_ledger'`) {
			t.Error("inventory.guard_checkout_ledger keeps owner commerce_inventory_writer, SECURITY DEFINER, PUBLIC revoked (0013)")
		}
	})

	t.Run("ledger CHECK branches: BUYER ALLOCATE and MERCHANT DEALLOCATE", func(t *testing.T) {
		buyerAlloc := map[string]string{"tenant_id": tcsU(1), "store_id": tcsU(2), "warehouse_id": tcsU(3), "sku_id": tcsU(4), "kind": "'ALLOCATE'", "delta_reserved": "-2", "delta_allocated": "2",
			"operation": "'checkout.pay_at_pickup.commit'", "command_key": tcsU(5) + "::text", "reservation_id": tcsU(5), "actor_kind": "'BUYER'", "checkout_id": tcsU(5),
			"buyer_owner_id": tcsU(6), "buyer_session_id": tcsU(7), "principal_id": "NULL"}
		tcsRows(t, f, "inventory.ledger", false, buyerAlloc, map[string]map[string]string{
			"NULL checkout_id":            {"checkout_id": "NULL"},
			"NULL buyer_owner_id":         {"buyer_owner_id": "NULL"},
			"NULL buyer_session_id":       {"buyer_session_id": "NULL"},
			"reservation differs":         {"reservation_id": tcsU(15)},
			"payment attempt present":     {"payment_attempt_id": tcsU(8)},
			"payment fact kind present":   {"payment_fact_kind": "'CAPTURED'"},
			"other operation":             {"operation": "'checkout.payment.capture'"},
			"command key is not order id": {"command_key": "'x'"},
			"principal present":           {"principal_id": tcsU(9)},
		}, "23514")
		tcsRows(t, f, "inventory.ledger", false, buyerAlloc, map[string]map[string]string{"control BUYER ALLOCATE (pay_at_pickup commit)": {}}, "")

		dealloc := map[string]string{"tenant_id": tcsU(1), "store_id": tcsU(2), "warehouse_id": tcsU(3), "sku_id": tcsU(4), "kind": "'DEALLOCATE'", "delta_allocated": "-2",
			"operation": "'fulfillment.pay_at_pickup.cancel'", "command_key": tcsU(5) + "::text", "reservation_id": tcsU(5), "actor_kind": "'MERCHANT'", "checkout_id": tcsU(5),
			"buyer_owner_id": tcsU(6), "buyer_session_id": tcsU(7), "principal_id": tcsU(9), "reason": "'PENDING'"}
		tcsRows(t, f, "inventory.ledger", false, dealloc, map[string]map[string]string{
			"no principal":                     {"principal_id": "NULL"},
			"positive delta_allocated":         {"delta_allocated": "2"},
			"zero delta_allocated":             {"delta_allocated": "0"},
			"delta_on_hand moves":              {"delta_on_hand": "-2"},
			"delta_reserved moves":             {"delta_reserved": "-2"},
			"delta_unavailable moves":          {"delta_unavailable": "-2"},
			"no reservation":                   {"reservation_id": "NULL"},
			"reservation differs":              {"reservation_id": tcsU(15)},
			"other operation":                  {"operation": "'checkout.payment.capture'"},
			"command key is not order id":      {"command_key": "'x'"},
			"payment attempt present":          {"payment_attempt_id": tcsU(8)},
			"no buyer owner":                   {"buyer_owner_id": "NULL"},
			"no buyer session":                 {"buyer_session_id": "NULL"},
			"no checkout id":                   {"checkout_id": "NULL"},
			"SYSTEM_EXPIRY may not DEALLOCATE": {"actor_kind": "'SYSTEM_EXPIRY'", "principal_id": "NULL"},
		}, "23514")
		restock := map[string]string{}
		for k, v := range dealloc {
			restock[k] = v
		}
		restock["operation"] = "'fulfillment.pay_at_pickup.restock'"
		tcsRows(t, f, "inventory.ledger", false, dealloc, map[string]map[string]string{"control cancel DEALLOCATE": {}, "restock operation": {"operation": "'fulfillment.pay_at_pickup.restock'"}}, "")
		// the 0013 rule stays: an ordinary MERCHANT row carries no checkout/buyer columns
		merchantAdj := map[string]string{"tenant_id": tcsU(1), "store_id": tcsU(2), "warehouse_id": tcsU(3), "sku_id": tcsU(4), "kind": "'ADJUST'", "delta_on_hand": "1", "operation": "'x'",
			"command_key": "'k'", "actor_kind": "'MERCHANT'", "principal_id": tcsU(9), "reason": "'r'"}
		tcsRows(t, f, "inventory.ledger", false, merchantAdj, map[string]map[string]string{
			"MERCHANT ADJUST with checkout_id": {"checkout_id": tcsU(5)}, "MERCHANT ADJUST with buyer owner": {"buyer_owner_id": tcsU(6)},
			"MERCHANT ADJUST with buyer session": {"buyer_session_id": tcsU(7)},
		}, "23514")
		tcsRows(t, f, "inventory.ledger", false, merchantAdj, map[string]map[string]string{"control MERCHANT ADJUST": {}}, "")
	})

	t.Run("definers: owner, SECURITY DEFINER, search_path, ACL matrix (PUBLIC revoked, no other commerce_* role)", func(t *testing.T) {
		var roles []string
		rows, err := f.owner.Query(ctx, `SELECT rolname FROM pg_roles WHERE rolname LIKE 'commerce\_%' ORDER BY 1`)
		if err != nil {
			t.Fatal(err)
		}
		for rows.Next() {
			var r string
			_ = rows.Scan(&r)
			roles = append(roles, r)
		}
		rows.Close()
		if len(roles) < 8 {
			t.Fatalf("role universe suspiciously small: %v", roles)
		}
		for _, fn := range tcsFunctions() {
			label := fn.schema + "." + fn.name
			var oids []int64
			or, err := f.owner.Query(ctx, `SELECT oid::bigint FROM pg_proc WHERE pronamespace=$1::regnamespace AND proname=$2`, fn.schema, fn.name)
			if err != nil {
				t.Fatal(err)
			}
			for or.Next() {
				var o int64
				_ = or.Scan(&o)
				oids = append(oids, o)
			}
			or.Close()
			if len(oids) != 1 {
				t.Errorf("%s: %d overloads, want exactly 1", label, len(oids))
				continue
			}
			var owner string
			var definer, fixed, publicExec, stable bool
			if err := f.owner.QueryRow(ctx, `SELECT pg_get_userbyid(p.proowner),p.prosecdef,coalesce(p.proconfig,'{}'::text[])=ARRAY['search_path=pg_catalog']::text[],
			  EXISTS(SELECT 1 FROM aclexplode(coalesce(p.proacl,acldefault('f',p.proowner))) a WHERE a.grantee=0 AND a.privilege_type='EXECUTE'),p.provolatile='s'
			  FROM pg_proc p WHERE p.oid=$1::oid`, oids[0]).Scan(&owner, &definer, &fixed, &publicExec, &stable); err != nil {
				t.Fatal(err)
			}
			if fn.owner != "" && owner != fn.owner {
				t.Errorf("%s owner %s want %s", label, owner, fn.owner)
			}
			if definer != fn.definer {
				t.Errorf("%s SECURITY DEFINER=%v want %v", label, definer, fn.definer)
			}
			if fn.definer && !fixed {
				t.Errorf("%s must pin proconfig search_path=pg_catalog", label)
			}
			if publicExec {
				t.Errorf("%s is executable by PUBLIC", label)
			}
			if fn.stable && !stable {
				t.Errorf("%s must be STABLE", label)
			}
			want := map[string]bool{}
			for _, r := range fn.exec {
				want[r] = true
			}
			for _, r := range roles {
				if r == owner {
					continue
				}
				if r == "commerce_hosted_runtime" {
					// 0025: commerce_hosted_runtime is granted commerce_checkout_runtime WITH INHERIT TRUE, so it holds exactly what the
					// checkout pool holds; that inheritance predates the CVS contract and is not a CVS grant.
					continue
				}
				got := tcsBool(t, f, label, `SELECT has_function_privilege($1,$2::oid,'EXECUTE')`, r, oids[0])
				if got != want[r] {
					t.Errorf("%s: EXECUTE for %s = %v, contract says %v", label, r, got, want[r])
				}
			}
		}
		// invoker helper of the eligibility predicate keeps its owner/grants (0063) after the re-derivation
		for _, fn := range []string{"manual_shipment_eligible", "order_money_shippable"} {
			if !tcsBool(t, f, fn, `SELECT count(*)=1 AND bool_and(NOT prosecdef) AND bool_and(pg_get_userbyid(proowner)='commerce_checkout_writer') FROM pg_proc WHERE pronamespace='fulfillment'::regnamespace AND proname=$1`, fn) {
				t.Errorf("fulfillment.%s must exist once, INVOKER, owner commerce_checkout_writer", fn)
			}
		}
		if !tcsBool(t, f, "same grants", `SELECT (SELECT array_agg(a.grantee::regrole::text ORDER BY 1) FROM pg_proc p, aclexplode(coalesce(p.proacl,acldefault('f',p.proowner))) a WHERE p.proname='manual_shipment_eligible' AND p.pronamespace='fulfillment'::regnamespace)
		  IS NOT DISTINCT FROM (SELECT array_agg(a.grantee::regrole::text ORDER BY 1) FROM pg_proc p, aclexplode(coalesce(p.proacl,acldefault('f',p.proowner))) a WHERE p.proname='order_money_shippable' AND p.pronamespace='fulfillment'::regnamespace)`) {
			t.Error("order_money_shippable has the same owner/grants as manual_shipment_eligible")
		}
	})

	t.Run("grant matrix and policies (one negative per role)", func(t *testing.T) {
		const cw, iw, au = "commerce_checkout_writer", "commerce_integration_writer", "commerce_auth"
		blind := []string{"commerce_runtime", "commerce_worker", "commerce_buyer_runtime", "commerce_buyer_issuer", "commerce_checkout_runtime"}
		for _, tbl := range newTables {
			for _, r := range blind {
				for _, p := range []string{"SELECT", "INSERT", "UPDATE", "DELETE", "TRUNCATE", "REFERENCES", "TRIGGER"} {
					tcsPriv(t, f, r, tbl, p, false)
				}
				if tcsBool(t, f, tbl, `SELECT has_any_column_privilege($1,$2,'SELECT') OR has_any_column_privilege($1,$2,'UPDATE') OR has_any_column_privilege($1,$2,'INSERT')`, r, tbl) {
					t.Errorf("%s has a column privilege on %s (no direct table grant to runtime/worker/buyer roles)", r, tbl)
				}
			}
		}
		// commerce_checkout_writer
		for _, tbl := range []string{"fulfillment.cvs_selections", "fulfillment.cvs_shipments", "fulfillment.cvs_shipment_events", "fulfillment.cvs_store_settings"} {
			tcsPriv(t, f, cw, tbl, "SELECT", true)
			tcsPriv(t, f, cw, tbl, "INSERT", true)
			tcsPriv(t, f, cw, tbl, "DELETE", false)
			tcsPriv(t, f, cw, tbl, "TRUNCATE", false)
		}
		tcsPriv(t, f, cw, "integration.ecpay_logistics_profiles", "SELECT", true)
		tcsPriv(t, f, cw, "integration.ecpay_logistics_profiles", "INSERT", false)
		tcsPriv(t, f, cw, "integration.ecpay_logistics_profiles", "UPDATE", false)
		tcsPriv(t, f, cw, "fulfillment.cvs_shipment_events", "UPDATE", false)
		tcsColPriv(t, f, cw, "fulfillment.cvs_shipments", "state", "UPDATE", true)
		tcsColPriv(t, f, cw, "fulfillment.cvs_shipments", "tenant_id", "UPDATE", false)
		tcsColPriv(t, f, cw, "fulfillment.cvs_shipments", "merchant_trade_no", "UPDATE", false)
		tcsColPriv(t, f, cw, "fulfillment.cvs_shipments", "operation_id", "UPDATE", false)
		tcsColPriv(t, f, cw, "fulfillment.cvs_shipments", "collection_amount", "UPDATE", false)
		for _, c := range []string{"enabled_chains", "pay_at_pickup_enabled", "pay_at_pickup_max_twd", "pay_at_pickup_max_open", "version", "updated_at"} {
			tcsColPriv(t, f, cw, "fulfillment.cvs_store_settings", c, "UPDATE", true)
		}
		tcsColPriv(t, f, cw, "fulfillment.cvs_store_settings", "tenant_id", "UPDATE", false)
		tcsColPriv(t, f, cw, "fulfillment.cvs_store_settings", "store_id", "UPDATE", false)
		tcsColPriv(t, f, cw, "checkout.orders", "collection_state", "UPDATE", true)
		tcsColPriv(t, f, cw, "checkout.orders", "payment_mode", "UPDATE", false)
		tcsColPriv(t, f, cw, "checkout.orders", "snapshot", "UPDATE", false)
		tcsPriv(t, f, cw, "fulfillment.pickup_versions", "INSERT", true)
		tcsPriv(t, f, cw, "fulfillment.pickup_heads", "INSERT", true)
		tcsColPriv(t, f, cw, "fulfillment.pickup_heads", "current_version", "UPDATE", true)
		tcsColPriv(t, f, cw, "fulfillment.pickup_heads", "pickup_id", "UPDATE", true)
		tcsColPriv(t, f, cw, "fulfillment.pickup_heads", "namespace", "UPDATE", false)
		tcsColPriv(t, f, cw, "fulfillment.pickup_heads", "enabled", "UPDATE", false)
		// river: X9, revoked at the end of every Apply
		if tcsBool(t, f, "river", `SELECT has_table_privilege('commerce_checkout_writer','river.river_job','SELECT') OR has_table_privilege('commerce_checkout_writer','river.river_job','INSERT') OR has_schema_privilege('commerce_checkout_writer','river','USAGE')`) {
			t.Error("X9: after a full Apply commerce_checkout_writer must have no privilege on river.river_job or schema river")
		}
		// commerce_integration_writer: reads shipments/profiles, never writes any fulfillment.* table
		tcsPriv(t, f, iw, "fulfillment.cvs_shipments", "SELECT", true)
		tcsPriv(t, f, iw, "integration.ecpay_logistics_profiles", "SELECT", true)
		if n := countRows(t, f.owner, `SELECT count(*) FROM pg_class c WHERE c.relnamespace='fulfillment'::regnamespace AND c.relkind IN ('r','p','v')
		   AND (has_table_privilege('commerce_integration_writer',c.oid,'INSERT') OR has_table_privilege('commerce_integration_writer',c.oid,'UPDATE') OR has_table_privilege('commerce_integration_writer',c.oid,'DELETE')
		     OR has_any_column_privilege('commerce_integration_writer',c.oid,'INSERT') OR has_any_column_privilege('commerce_integration_writer',c.oid,'UPDATE'))`); n != 0 {
			t.Errorf("commerce_integration_writer holds INSERT/UPDATE/DELETE on %d fulfillment.* table(s); the contract says none", n)
		}
		for _, c := range []string{"tenant_id", "store_id", "owner_id", "id", "currency", "snapshot"} {
			tcsColPriv(t, f, iw, "checkout.orders", c, "SELECT", true)
		}
		// commerce_auth: exactly the projection columns
		for _, c := range []string{"tenant_id", "store_id", "order_id", "state"} {
			tcsColPriv(t, f, au, "fulfillment.cvs_shipments", c, "SELECT", true)
		}
		for _, c := range []string{"merchant_trade_no", "provider_logistics_id", "cvs_payment_no", "receiver_store_id", "operation_id", "principal_id"} {
			tcsColPriv(t, f, au, "fulfillment.cvs_shipments", c, "SELECT", false)
		}
		tcsPriv(t, f, au, "fulfillment.cvs_shipments", "INSERT", false)
		tcsPriv(t, f, au, "fulfillment.cvs_shipments", "UPDATE", false)
		tcsColPriv(t, f, au, "checkout.orders", "payment_mode", "SELECT", true)
		tcsColPriv(t, f, au, "checkout.orders", "collection_state", "SELECT", true)

		tcsPolicy(t, f, "integration", "operations", "cvs_create_insert", iw, "INSERT", true, []string{"ecpay_logistics", "ecpay.cvs_create", "transactional", "MERCHANT"}, nil)
		tcsPolicy(t, f, "integration", "operations", "checkout_cvs_operation_read", cw, "SELECT", true, []string{"ecpay_logistics", "ecpay.cvs_create"}, []string{"principal"})
		tcsPolicy(t, f, "inventory", "ledger", "checkout_writer_pay_at_pickup_release", cw, "INSERT", true, []string{"DEALLOCATE", "MERCHANT"}, nil)
		for _, tbl := range []string{"pickup_versions", "pickup_heads"} {
			for _, r := range []string{"commerce_buyer_runtime", "commerce_checkout_runtime"} {
				tcsPolicy(t, f, "fulfillment", tbl, "buyer_entered_own", r, "SELECT", false, []string{"buyer."}, nil)
			}
		}
		if n := countRows(t, f.owner, `SELECT count(*) FROM pg_policies WHERE schemaname='fulfillment' AND tablename='cvs_store_settings' AND 'commerce_checkout_writer'=ANY(roles) AND cmd IN ('SELECT','INSERT','UPDATE','ALL')`); n < 1 {
			t.Error("cvs_store_settings needs policies for commerce_checkout_writer (SELECT/INSERT/UPDATE)")
		}
		for _, cmd := range []string{"SELECT", "INSERT", "UPDATE"} {
			if !tcsBool(t, f, cmd, `SELECT EXISTS(SELECT 1 FROM pg_policies WHERE schemaname='fulfillment' AND tablename='cvs_store_settings' AND 'commerce_checkout_writer'=ANY(roles) AND cmd IN ($1,'ALL'))`, cmd) {
				t.Errorf("cvs_store_settings: no %s policy for commerce_checkout_writer", cmd)
			}
		}
		// no policy of the new tables grants a role the contract keeps out
		if n := countRows(t, f.owner, `SELECT count(*) FROM pg_policies WHERE schemaname IN ('fulfillment','integration') AND tablename IN ('cvs_selections','cvs_shipments','cvs_shipment_events','cvs_store_settings','ecpay_logistics_profiles')
		   AND roles && ARRAY['commerce_runtime','commerce_worker','commerce_buyer_runtime','commerce_buyer_issuer','commerce_checkout_runtime','public']::name[]`); n != 0 {
			t.Errorf("%d policies on the new tables target runtime/worker/buyer roles or PUBLIC", n)
		}
	})

	t.Run("no PII columns (catalog scan)", func(t *testing.T) {
		rows, err := f.owner.Query(ctx, `SELECT c.relname||'.'||a.attname FROM pg_attribute a JOIN pg_class c ON c.oid=a.attrelid
		 WHERE a.attnum>0 AND NOT a.attisdropped AND c.oid = ANY(ARRAY['fulfillment.cvs_selections','fulfillment.cvs_shipments','fulfillment.cvs_shipment_events','fulfillment.cvs_store_settings','integration.ecpay_logistics_profiles']::regclass[])
		   AND a.attname ~* '(name|phone|cell|mobile|email|mail|address|addr|recipient|street|line1|line2|city|postal|zip|receiver_(name|phone|cell|email|address))' AND a.attname NOT IN ('receiver_store_id')`)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		for rows.Next() {
			var c string
			_ = rows.Scan(&c)
			t.Errorf("TD8: %s looks like a PII column (recipient data lives only in the frozen order snapshot)", c)
		}
	})

	t.Run("Go and SQL trade number agree over generated UUIDs (C2/E2)", func(t *testing.T) {
		for i := 0; i < 200; i++ {
			id := randomUUID()
			var sqlNo string
			if err := f.owner.QueryRow(ctx, `SELECT fulfillment.ecpay_trade_no($1::uuid)`, id).Scan(&sqlNo); err != nil {
				t.Fatalf("fulfillment.ecpay_trade_no: %v", err)
			}
			if goNo := ecpay.MerchantTradeNo(id); goNo != sqlNo {
				t.Fatalf("uuid %s: Go %s != SQL %s", id, goNo, sqlNo)
			}
		}
	})

	t.Run("merchant-plant negatives under a real commerce_runtime login", func(t *testing.T) {
		tenant, store, principal := f.tenantA, f.storeA1, f.principalA
		// disclosed owner-pool fixture: one provider-namespace version+head and one buyer-namespace version+head to aim at
		plant := func(kind, namespace, code, vk string, principalID any) (versionID string) {
			versionID = randomUUID()
			mustExec(t, f.owner, `INSERT INTO fulfillment.pickup_versions(tenant_id,store_id,id,kind,namespace,code,version,name,address,verification_kind,evidence_ref,principal_id,attested_at,valid_until)
			  VALUES($1,$2,$3,$4,$5,$6,1,'Synthetic store','Synthetic address',$7,'tcs-fixture',$8,now(),now()+interval '24 hours')`, tenant, store, versionID, kind, namespace, code, vk, principalID)
			mustExec(t, f.owner, `INSERT INTO fulfillment.pickup_heads(tenant_id,store_id,kind,namespace,code,current_version,pickup_id,enabled) VALUES($1,$2,$3,$4,$5,1,$6,true)`, tenant, store, kind, namespace, code, versionID)
			return
		}
		tag := t04Tag()
		ecpayNS := "ecpay.sandbox.tcs" + tag
		buyerNS := "buyer." + randomUUID()
		plant("cvs_711", ecpayNS, "131386", "PROVIDER_DIRECTORY_VERIFIED", principal)
		plant("cvs_711", buyerNS, "123456", "BUYER_ENTERED", nil)
		plant("cvs_711", "fixture.tcs"+tag, "000123", "MANUAL_ATTESTED", principal)
		guc := map[string]string{"app.tenant_id": tenant, "app.store_id": store, "app.principal_id": principal}
		ins := func(ns, code, vk, principalSQL string) string {
			return fmt.Sprintf(`INSERT INTO fulfillment.pickup_versions(tenant_id,store_id,kind,namespace,code,version,name,address,verification_kind,evidence_ref,principal_id,attested_at,valid_until)
			  VALUES('%s','%s','cvs_711','%s','%s',9,'n','a','%s','e',%s,now(),now()+interval '1 hour')`, tenant, store, ns, code, vk, principalSQL)
		}
		tcsAs(t, f, "commerce_runtime", guc, func(ctx context.Context, tx pgx.Tx) {
			if st, _ := tcsSub(ctx, tx, ins("fixture.ctrl"+tag, "555555", "MANUAL_ATTESTED", "'"+principal+"'")); st != "" {
				t.Fatalf("control: an ordinary merchant attestation must still be accepted, got %s", st)
			}
			for name, stmt := range map[string]string{
				"INSERT ecpay.* namespace version":             ins(ecpayNS, "777777", "MANUAL_ATTESTED", "'"+principal+"'"),
				"INSERT buyer.* namespace version":             ins(buyerNS, "777777", "MANUAL_ATTESTED", "'"+principal+"'"),
				"INSERT PROVIDER_DIRECTORY_VERIFIED, plain ns": ins("fixture.v"+tag, "777777", "PROVIDER_DIRECTORY_VERIFIED", "'"+principal+"'"),
				"INSERT BUYER_ENTERED, plain ns":               ins("fixture.b"+tag, "777777", "BUYER_ENTERED", "NULL"),
				"INSERT BUYER_ENTERED into buyer.* ns":         ins(buyerNS, "777778", "BUYER_ENTERED", "NULL"),
				"INSERT ecpay.* head":                          fmt.Sprintf(`INSERT INTO fulfillment.pickup_heads(tenant_id,store_id,kind,namespace,code,current_version,pickup_id,enabled) VALUES('%s','%s','cvs_711','ecpay.sandbox.x%s','1',1,gen_random_uuid(),true)`, tenant, store, tag),
				"INSERT buyer.* head":                          fmt.Sprintf(`INSERT INTO fulfillment.pickup_heads(tenant_id,store_id,kind,namespace,code,current_version,pickup_id,enabled) VALUES('%s','%s','cvs_711','buyer.%s','1',1,gen_random_uuid(),true)`, tenant, store, randomUUID()),
			} {
				if st, _ := tcsSub(ctx, tx, stmt); st != "42501" {
					t.Errorf("%s: want 42501 (row-level security), got %q", name, st)
				}
			}
			for name, stmt := range map[string]string{
				"UPDATE ecpay.* head": fmt.Sprintf(`UPDATE fulfillment.pickup_heads SET enabled=false WHERE tenant_id='%s' AND store_id='%s' AND namespace='%s'`, tenant, store, ecpayNS),
				"UPDATE buyer.* head": fmt.Sprintf(`UPDATE fulfillment.pickup_heads SET enabled=false WHERE tenant_id='%s' AND store_id='%s' AND namespace='%s'`, tenant, store, buyerNS),
			} {
				st, n := tcsSub(ctx, tx, stmt)
				if st == "" && n != 0 {
					t.Errorf("%s changed %d row(s); the RESTRICTIVE namespace policy must hide it", name, n)
				}
			}
			if st, n := tcsSub(ctx, tx, fmt.Sprintf(`UPDATE fulfillment.pickup_heads SET enabled=false WHERE tenant_id='%s' AND store_id='%s' AND namespace='fixture.tcs%s'`, tenant, store, tag)); st != "" || n != 1 {
				t.Errorf("control: a merchant head UPDATE must still work (state %q rows %d)", st, n)
			}
			// the merchant cannot read or touch the tables of the ECPay / settings surface directly
			for _, tbl := range []string{"fulfillment.cvs_store_settings", "integration.ecpay_logistics_profiles", "fulfillment.cvs_shipments", "fulfillment.cvs_selections"} {
				if st, _ := tcsSub(ctx, tx, `SELECT 1 FROM `+tbl+` LIMIT 1`); st != "42501" {
					t.Errorf("commerce_runtime SELECT on %s: want 42501, got %q", tbl, st)
				}
			}
			// ...and no direct DEALLOCATE / checkout ledger write (only the definer may)
			if st, _ := tcsSub(ctx, tx, fmt.Sprintf(`INSERT INTO inventory.ledger(tenant_id,store_id,warehouse_id,sku_id,kind,delta_allocated,operation,command_key,reservation_id,actor_kind,checkout_id,buyer_owner_id,buyer_session_id,principal_id,reason)
			  VALUES('%s','%s',gen_random_uuid(),gen_random_uuid(),'DEALLOCATE',-1,'fulfillment.pay_at_pickup.cancel','k',gen_random_uuid(),'MERCHANT',gen_random_uuid(),gen_random_uuid(),gen_random_uuid(),'%s','PENDING')`, tenant, store, principal)); st == "" {
				t.Error("commerce_runtime must not be able to INSERT a DEALLOCATE ledger row directly")
			}
		})
	})

	t.Run("buyer_entered_own hides buyer A's rows from buyer B under both buyer roles", func(t *testing.T) {
		tenant, store := f.tenantA, f.storeA1
		buyerA, buyerB := randomUUID(), randomUUID()
		ns := "buyer." + buyerA
		vid := randomUUID()
		code := "9" + strings.ReplaceAll(randomUUID(), "-", "")[:5]
		mustExec(t, f.owner, `INSERT INTO fulfillment.pickup_versions(tenant_id,store_id,id,kind,namespace,code,version,name,address,verification_kind,evidence_ref,principal_id,attested_at,valid_until)
		  VALUES($1,$2,$3,'cvs_711',$4,$5,1,'Synthetic store','Synthetic address','BUYER_ENTERED','tcs-fixture',NULL,now(),now()+interval '24 hours')`, tenant, store, vid, ns, code)
		mustExec(t, f.owner, `INSERT INTO fulfillment.pickup_heads(tenant_id,store_id,kind,namespace,code,current_version,pickup_id,enabled) VALUES($1,$2,'cvs_711',$3,$4,1,$5,true)`, tenant, store, ns, code, vid)
		for _, role := range []string{"commerce_buyer_runtime", "commerce_checkout_runtime"} {
			count := func(buyer, table string) (n int) {
				tcsAs(t, f, role, map[string]string{"app.tenant_id": tenant, "app.store_id": store, "app.buyer_id": buyer}, func(ctx context.Context, tx pgx.Tx) {
					q := `SELECT count(*) FROM fulfillment.` + table + ` WHERE namespace=$1`
					if err := tx.QueryRow(ctx, q, ns).Scan(&n); err != nil {
						t.Fatalf("%s reading %s: %v", role, table, err)
					}
				})
				return
			}
			for _, table := range []string{"pickup_versions", "pickup_heads"} {
				if n := count(buyerB, table); n != 0 {
					t.Errorf("%s as buyer B sees %d of buyer A's %s rows", role, n, table)
				}
				if n := count(buyerA, table); n != 1 {
					t.Errorf("%s as buyer A sees %d of its own %s rows, want 1", role, n, table)
				}
			}
		}
		// merchants (commerce_runtime) still see the row: the label must reach the merchant (§16.1)
		tcsAs(t, f, "commerce_runtime", map[string]string{"app.tenant_id": tenant, "app.store_id": store}, func(ctx context.Context, tx pgx.Tx) {
			var n int
			if err := tx.QueryRow(ctx, `SELECT count(*) FROM fulfillment.pickup_versions WHERE namespace=$1`, ns).Scan(&n); err != nil || n != 1 {
				t.Errorf("commerce_runtime must still read buyer_entered rows: n=%d err=%v", n, err)
			}
		})
	})

	t.Run("pool note: buyer definers run from a real checkout-pool login and nowhere else", func(t *testing.T) {
		call := func(dsn, schema, name string) string {
			var argTypes []string
			rows, err := f.owner.Query(ctx, `SELECT format_type(t,NULL) FROM pg_proc p, unnest(p.proargtypes::oid[]) WITH ORDINALITY u(t,i) WHERE p.pronamespace=$1::regnamespace AND p.proname=$2 ORDER BY u.i`, schema, name)
			if err != nil {
				t.Fatal(err)
			}
			for rows.Next() {
				var s string
				_ = rows.Scan(&s)
				argTypes = append(argTypes, "NULL::"+s)
			}
			rows.Close()
			conn, err := pgx.Connect(ctx, dsn)
			if err != nil {
				t.Fatalf("login: %v", err)
			}
			defer conn.Close(ctx)
			tx, _ := conn.Begin(ctx)
			defer tx.Rollback(ctx)
			var sink any
			err = tx.QueryRow(ctx, fmt.Sprintf(`SELECT to_jsonb(x) FROM (SELECT %s.%s(%s)) x`, schema, name, strings.Join(argTypes, ","))).Scan(&sink)
			return sqlState(err)
		}
		// a checkout-pool login exactly as cmd/api opens it (platform.OpenCheckoutPool refuses any other authority mix)
		checkoutDSN := bcRole(t, f, "commerce_checkout_runtime")
		pool, err := platform.OpenCheckoutPool(ctx, checkoutDSN)
		if err != nil {
			t.Fatalf("checkout pool login: %v", err)
		}
		pool.Close()
		buyerDSN, runtimeDSN := bcRole(t, f, "commerce_buyer_runtime"), bcRole(t, f, "commerce_runtime")
		for _, fn := range tcsFunctions() {
			if len(fn.exec) != 1 || fn.exec[0] != "commerce_checkout_runtime" {
				continue
			}
			if st := call(checkoutDSN, fn.schema, fn.name); st == "42501" {
				t.Errorf("%s.%s from the checkout pool: SQLSTATE %s (the contract grants EXECUTE to commerce_checkout_runtime)", fn.schema, fn.name, st)
			}
			for role, dsn := range map[string]string{"commerce_buyer_runtime": buyerDSN, "commerce_runtime": runtimeDSN} {
				if st := call(dsn, fn.schema, fn.name); st != "42501" {
					t.Errorf("%s.%s as %s: want 42501, got %q (buyer definers are checkout-pool only)", fn.schema, fn.name, role, st)
				}
			}
		}
	})
}

// TestTaiwanCvsSchemaColumnComments is the PROCESS section 5 column half of "every new table, column, function and role gets COMMENT ON".
// It is advisory (a P2 documentation finding, not a contract section 10 clause), so it is a separate test from the TCV02 gate.
func TestTaiwanCvsSchemaColumnComments(t *testing.T) {
	f := fixture(t)
	ctx := context.Background()
	for _, tbl := range []string{"integration.ecpay_logistics_profiles", "fulfillment.cvs_selections", "fulfillment.cvs_shipments", "fulfillment.cvs_shipment_events", "fulfillment.cvs_store_settings"} {
		var missing []string
		rows, err := f.owner.Query(ctx, `SELECT a.attname FROM pg_attribute a WHERE a.attrelid=to_regclass($1) AND a.attnum>0 AND NOT a.attisdropped AND col_description(a.attrelid,a.attnum) IS NULL ORDER BY a.attnum`, tbl)
		if err != nil {
			t.Fatal(err)
		}
		for rows.Next() {
			var c string
			_ = rows.Scan(&c)
			missing = append(missing, c)
		}
		rows.Close()
		if len(missing) > 0 {
			t.Errorf("PROCESS section 5: %s columns without COMMENT ON: %v", tbl, missing)
		}
	}
	for _, c := range []string{"payment_mode", "collection_state"} {
		if !tcsBool(t, f, c, `SELECT col_description('checkout.orders'::regclass,(SELECT attnum FROM pg_attribute WHERE attrelid='checkout.orders'::regclass AND attname=$1)) IS NOT NULL`, c) {
			t.Errorf("PROCESS section 5: checkout.orders.%s lacks COMMENT ON", c)
		}
	}
}
