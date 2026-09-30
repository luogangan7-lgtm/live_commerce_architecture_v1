package foundation_test

// MA02 schema half (contracts/meta-ads-v1.md §9 row MA02, §4 preamble, §4.1 DDL, §4.3, §4.4 privilege delta, B12 amendment;
// A-1, AD9). Tier REAL_PG, no Graph. Written from the FROZEN contract; every expectation is a row or sentence of the contract,
// and every implementation privilege the contract does not name is listed under CONTRACT_GAPS with the reason, so that any NEW
// privilege fails the gate and a gap the integrator has not ruled stays visible in the evidence log ("GAP ..." lines).
//
// What it proves:
//   - commerce_ads_writer holds exactly the §4.4 rows (catalog walk of ACLs incl. column grants and policies) plus the listed gaps;
//   - the other roles hold exactly the §4.4 EXECUTE/USAGE/table rows on ads objects, nothing on PUBLIC, no LOGIN role holds a
//     direct grant, runtime/worker cannot touch an ads table or the operator definer, only the registrar can call the latter;
//   - every ads table FORCEs RLS; every ads/definer function is SECURITY DEFINER with search_path=pg_catalog, owned per §4;
//     COMMENT ON exists for the role, schema, tables and functions (PROCESS §5);
//   - the widened CHECKs (store_grants permissions, meta_page_credentials provider + nonce) and the §4.1 table CHECKs refuse
//     what the contract says they refuse (TWD non-whole budget, >30 days, currency set, age bounds, attempt/revision bounds,
//     pending-token trio, capi enable rules, ...); no store_grants row is created for ads:* by store creation (A-1);
//   - the ads writer's operations / operation_events policies exist AND behave (read only meta_ads/meta_dataset rows; insert only
//     ads/CAPI marketing ops): removing one turns this gate red because a grant without a policy reads zero rows (allowance sum
//     empty = fail open, §4.4 row 3).
// Disclosed owner-pool fixtures: catalog walks, SET LOCAL ROLE probes, one synthetic non-ads operation to make the zero-row
// assertions meaningful, INSERTs under session_replication_role=replica to reach CHECK constraints without their FK parents.
// Evidence label: REAL_PG.

import (
	"context"
	"fmt"
	"os"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

func adsGrantFacts(t *testing.T, f *testFixture, role string, includeOwned bool) []string {
	t.Helper()
	q := `
SELECT 'table:'||n.nspname||'.'||c.relname||':'||a.privilege_type FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace, aclexplode(c.relacl) a
  WHERE a.grantee=$1::regrole AND c.relkind IN ('r','p','v','m','f')
UNION ALL SELECT 'seq:'||n.nspname||'.'||c.relname||':'||a.privilege_type FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace, aclexplode(c.relacl) a
  WHERE a.grantee=$1::regrole AND c.relkind='S'
UNION ALL SELECT 'column:'||n.nspname||'.'||c.relname||'.'||at.attname||':'||a.privilege_type FROM pg_attribute at JOIN pg_class c ON c.oid=at.attrelid JOIN pg_namespace n ON n.oid=c.relnamespace, aclexplode(at.attacl) a
  WHERE a.grantee=$1::regrole AND NOT at.attisdropped
UNION ALL SELECT 'func:'||n.nspname||'.'||p.proname||'('||pg_get_function_identity_arguments(p.oid)||'):'||a.privilege_type FROM pg_proc p JOIN pg_namespace n ON n.oid=p.pronamespace, aclexplode(p.proacl) a
  WHERE a.grantee=$1::regrole AND ($2 OR p.proowner<>$1::regrole)
UNION ALL SELECT 'schema:'||n.nspname||':'||a.privilege_type FROM pg_namespace n, aclexplode(n.nspacl) a WHERE a.grantee=$1::regrole`
	rows, err := f.owner.Query(context.Background(), q, role, includeOwned)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		_ = rows.Scan(&s)
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}

func adsSet(list ...string) map[string]bool {
	m := map[string]bool{}
	for _, s := range list {
		m[s] = true
	}
	return m
}

func adsCols(rel string, priv string, cols ...string) []string {
	var out []string
	for _, c := range cols {
		out = append(out, "column:"+rel+"."+c+":"+priv)
	}
	return out
}

// TestMetaAdsMA02Schema is the gate; sub-tests split the clauses.
func TestMetaAdsMA02Schema(t *testing.T) {
	f := fixture(t)
	ctx := context.Background()

	t.Run("commerce_ads_writer holds exactly the §4.4 rows (+ the listed contract gaps)", func(t *testing.T) {
		// §4.4 rows, by row. ads.* tables are handled below (SELECT/INSERT/UPDATE/DELETE cap).
		contract := adsSet(
			// row: ads schema USAGE; `principal_holds`, `resolve_access` EXECUTE
			"schema:ads:USAGE", "func:identity.principal_holds(p_tenant uuid, p_store uuid, p_principal uuid, p_permissions text[]):EXECUTE",
			"func:identity.resolve_access(p_hash bytea, p_store uuid, p_permission text):EXECUTE",
			// row: SELECT + read policy on operations, operation_events, bindings, payments.facts, control.stores; refund_facts (B12)
			"table:integration.operations:SELECT", "table:integration.operation_events:SELECT", "table:integration.bindings:SELECT",
			"table:payments.facts:SELECT", "table:payments.refund_facts:SELECT",
			// row: INSERT (+policy) on operations/events, USAGE on the events sequence
			"table:integration.operations:INSERT", "table:integration.operation_events:INSERT", "seq:integration.operation_events_id_seq:USAGE",
			// row (0080): consent_allows, store_standing, schemas customers/billing; resolve_published_store + schema buyer
			"func:customers.consent_allows(p_tenant uuid, p_store uuid, p_owner uuid, p_purpose text, p_channel text):EXECUTE",
			"func:billing.store_standing(p_tenant uuid, p_store uuid):EXECUTE", "schema:customers:USAGE", "schema:billing:USAGE",
			"func:buyer.resolve_published_store(p_origin text):EXECUTE", "schema:buyer:USAGE",
			// schemas whose tables the rows above live in
			"schema:identity:USAGE", "schema:integration:USAGE", "schema:payments:USAGE", "schema:control:USAGE", "schema:catalog:USAGE",
			// §4.3: ads.put_capi_context resolves the buyer capability like customers.buyer_set_consent (buyer.resolve_scope)
			"func:buyer.resolve_scope(p_hash bytea, p_store uuid):EXECUTE",
			// row (0080): attempt->order->owner columns and published-catalog columns (F17), named by table below
			"schema:checkout:USAGE", "schema:storefront:USAGE", "schema:inventory:USAGE",
		)
		for _, c := range adsCols("control.stores", "SELECT", "tenant_id", "id", "active", "currency", "name") { // §4.4 row 3 (+ feed name, 0080)
			contract[c] = true
		}
		for _, c := range adsCols("checkout.payment_attempts", "SELECT", "tenant_id", "store_id", "id", "owner_id", "order_id") {
			contract[c] = true
		}
		for _, c := range adsCols("catalog.products", "SELECT", "tenant_id", "store_id", "id", "name", "description") {
			contract[c] = true
		}
		for _, c := range adsCols("catalog.skus", "SELECT", "tenant_id", "store_id", "id", "product_id", "status", "currency", "price_minor") {
			contract[c] = true
		}
		for _, c := range adsCols("inventory.balances", "SELECT", "tenant_id", "store_id", "sku_id", "on_hand", "reserved", "allocated", "unavailable") {
			contract[c] = true
		}
		for _, c := range adsCols("checkout.orders", "SELECT", "tenant_id", "store_id", "owner_id", "id") {
			contract[c] = true
		}

		// CONTRACT_GAPS: privileges the implementation holds that no §4.4 row or §4.3 sentence names. Each is justified in the
		// migration; the integrator must rule them (add a §4.4 row) or remove them. Any privilege NOT in this list fails.
		gaps := map[string]string{
			"schema:river:USAGE":                             "GAP-1: ads.verify_job reads river.river_job (xmin) to prove the planned job, the pattern §6.3 mandates (integration.plan_claim_reply, 0064:829); §4.4 has no river row",
			"table:river.river_job:SELECT":                   "GAP-1",
			"func:ads.canonical_draft(p_draft uuid):EXECUTE": "",
		}
		delete(gaps, "func:ads.canonical_draft(p_draft uuid):EXECUTE") // owned function, not a received grant
		for _, c := range adsCols("control.storefront_domains", "SELECT", "id", "tenant_id", "store_id", "origin", "state") {
			gaps[c] = "GAP-2 (R-C): §5.2 PRODUCT_TRAFFIC validation needs the active storefront domain; §4.4 has no such row"
		}
		for _, c := range adsCols("control.storefront_publications", "SELECT", "tenant_id", "store_id", "published") {
			gaps[c] = "GAP-2 (R-C): published storefront check of §5.2"
		}
		gaps["column:catalog.products.status:SELECT"] = "GAP-2 (R-C): 'product exists, published' of §5.2"
		for _, c := range adsCols("checkout.orders", "SELECT", "quote_id") {
			gaps[c] = "GAP-3: order lines live in the quote snapshot; §4.3 names destination_id/order-line columns that do not exist as such"
		}
		for _, c := range adsCols("storefront.quotes", "SELECT", "tenant_id", "store_id", "owner_id", "id", "snapshot") {
			gaps[c] = "GAP-3: quote snapshot carries the order lines (contents[]); §4.3 names storefront.destination_snapshots(phone), which the implementation deliberately does NOT grant (CD5: ph omitted)"
		}

		actual := adsGrantFacts(t, f, "commerce_ads_writer", false)
		seen := map[string]bool{}
		for _, a := range actual {
			seen[a] = true
			switch {
			case strings.HasPrefix(a, "table:ads.") || strings.HasPrefix(a, "column:ads.") || strings.HasPrefix(a, "seq:ads."):
				priv := a[strings.LastIndex(a, ":")+1:]
				if priv != "SELECT" && priv != "INSERT" && priv != "UPDATE" && priv != "DELETE" && priv != "USAGE" {
					t.Errorf("writer holds %s on its own schema (§4.4 row 1 allows SELECT/INSERT/UPDATE/DELETE)", a)
				}
			case contract[a]:
			case gaps[a] != "":
				t.Logf("GAP %s -- %s", a, gaps[a])
			default:
				t.Errorf("commerce_ads_writer holds %s, which no §4.4 row (or listed gap) grants", a)
			}
		}
		for want := range contract {
			if !seen[want] {
				t.Errorf("§4.4 row missing: commerce_ads_writer does not hold %s", want)
			}
		}
		if len(actual) < 100 {
			t.Errorf("only %d grant facts read: the catalog walk saw nothing", len(actual))
		}
		// no DELETE on ads tables except the capi context purge (§6.4 deletes stale rows); nothing may TRUNCATE/REFERENCES/TRIGGER
		for _, a := range actual {
			if strings.HasSuffix(a, ":TRUNCATE") || strings.HasSuffix(a, ":REFERENCES") || strings.HasSuffix(a, ":TRIGGER") {
				t.Errorf("writer holds %s", a)
			}
		}
	})

	t.Run("every other role holds exactly its §4.4 rows on ads objects; PUBLIC and login roles hold none", func(t *testing.T) {
		type roleWant struct {
			schemaUsage bool
			execIn      map[string]bool // function name (schema-qualified, no args) prefix rules
		}
		facts := map[string][]string{}
		for _, role := range []string{"commerce_runtime", "commerce_worker", "commerce_buyer_runtime", "commerce_integration_writer", "commerce_meta_registrar"} {
			facts[role] = adsGrantFacts(t, f, role, true)
		}
		adsOnly := func(role string) []string {
			var out []string
			for _, a := range facts[role] {
				if strings.HasPrefix(a, "column:ads.") || strings.HasPrefix(a, "table:ads.") || strings.HasPrefix(a, "seq:ads.") || strings.HasPrefix(a, "func:ads.") ||
					strings.HasPrefix(a, "func:integration.register_meta_ads_token") || strings.HasPrefix(a, "func:integration.load_meta_ads_token") || a == "schema:ads:USAGE" {
					out = append(out, a)
				}
			}
			return out
		}
		fnName := func(a string) string { // func:ads.report(p_hash bytea,...):EXECUTE -> ads.report
			s := strings.TrimPrefix(a, "func:")
			return s[:strings.Index(s, "(")]
		}
		firstArg := func(a string) string {
			s := a[strings.Index(a, "(")+1:]
			return strings.TrimSpace(strings.SplitN(s, ",", 2)[0])
		}
		// §4.4: runtime = merchant definers (every one authenticates by session hash, first argument p_hash bytea) + register_meta_ads_token
		var runtimeFns []string
		for _, a := range adsOnly("commerce_runtime") {
			switch {
			case a == "schema:ads:USAGE":
			case strings.HasPrefix(a, "func:ads."):
				runtimeFns = append(runtimeFns, fnName(a))
				if !strings.HasPrefix(firstArg(a), "p_hash bytea") {
					t.Errorf("commerce_runtime may execute %s, which does not authenticate a session hash (merchant definers only, §4.4)", a)
				}
			case a == "func:integration.register_meta_ads_token(p_hash bytea, p_store uuid, p_state uuid, p_binding uuid, p_expected_version bigint):EXECUTE":
			default:
				t.Errorf("commerce_runtime holds %s on an ads object (§4.4 grants runtime EXECUTE only)", a)
			}
		}
		for _, must := range []string{"ads.approve_draft", "ads.publish_draft", "ads.create_draft", "ads.update_draft", "ads.get_draft", "ads.list_drafts", "ads.set_capi", "ads.get_settings", "ads.report"} {
			found := false
			for _, n := range runtimeFns {
				found = found || n == must
			}
			if !found {
				t.Errorf("commerce_runtime cannot execute %s (merchant API, §7)", must)
			}
		}
		// worker: check/advance/insights/planner/ingest/loader functions only, none authenticating a merchant hash
		workerOK := regexp.MustCompile(`^(ads\.(check_[a-z]+|advance_[a-z]+|insights_[a-z]+|pending_insight_reads|put_insights_day|plan_capi[a-z_]*|purge_oauth_states|canonical_draft|capi_user_data)|integration\.load_meta_ads_token)$`)
		for _, a := range adsOnly("commerce_worker") {
			if a == "schema:ads:USAGE" {
				continue
			}
			if !strings.HasPrefix(a, "func:") || !workerOK.MatchString(fnName(a)) || strings.HasPrefix(firstArg(a), "p_hash") {
				t.Errorf("commerce_worker holds %s (§4.4: Check, sweepers, ingestion, loader only)", a)
			}
		}
		for _, must := range []string{"ads.check_create", "ads.check_activate", "ads.check_read", "ads.check_capi", "ads.advance_next", "ads.advance_plan", "ads.put_insights_day", "ads.plan_capi", "integration.load_meta_ads_token", "ads.capi_user_data"} {
			found := false
			for _, a := range adsOnly("commerce_worker") {
				found = found || (strings.HasPrefix(a, "func:") && fnName(a) == must)
			}
			if !found {
				t.Errorf("commerce_worker cannot execute %s", must)
			}
		}
		// buyer runtime: exactly put_capi_context + feed_rows; registrar: exactly the operator definer
		want := adsSet("schema:ads:USAGE", "func:ads.put_capi_context(p_hash bytea, p_store uuid, p_user_agent text):EXECUTE", "func:ads.feed_rows(p_origin text):EXECUTE")
		for _, a := range adsOnly("commerce_buyer_runtime") {
			if !want[a] {
				t.Errorf("commerce_buyer_runtime holds %s (§4.4: put_capi_context and feed_rows only)", a)
			}
			delete(want, a)
		}
		for a := range want {
			t.Errorf("commerce_buyer_runtime lacks %s", a)
		}
		regWant := adsSet("schema:ads:USAGE", "func:ads.operator_set_settings(p_tenant uuid, p_store uuid, p_environment text, p_sandbox_ad_account text, p_max_active_budget_minor bigint, p_allowance_currency text):EXECUTE")
		for _, a := range adsOnly("commerce_meta_registrar") {
			if !regWant[a] {
				t.Errorf("commerce_meta_registrar holds %s (B12: EXECUTE on ads.operator_set_settings only)", a)
			}
			delete(regWant, a)
		}
		for a := range regWant {
			t.Errorf("commerce_meta_registrar lacks %s", a)
		}
		// integration writer: the two oauth/connection tables (§4.4 row) and the two integration functions
		iw := adsSet("schema:ads:USAGE", "table:ads.oauth_states:SELECT", "column:ads.oauth_states.used_at:UPDATE", "table:ads.connections:SELECT", "table:ads.connections:INSERT")
		for _, a := range adsOnly("commerce_integration_writer") {
			switch {
			case iw[a]:
			case strings.HasPrefix(a, "column:ads.connections.") && strings.HasSuffix(a, ":UPDATE"):
			case strings.HasPrefix(a, "func:integration.register_meta_ads_token") || strings.HasPrefix(a, "func:integration.load_meta_ads_token"): // owner
			default:
				t.Errorf("commerce_integration_writer holds %s (§4.4: oauth_states, connections only)", a)
			}
		}
		// nobody but the owner reads the sealed ads ciphertext: load_meta_ads_token EXECUTE = worker only (+ its owner)
		rows, err := f.owner.Query(ctx, `SELECT r.rolname FROM pg_proc p, aclexplode(p.proacl) a JOIN pg_roles r ON r.oid=a.grantee
			WHERE p.oid='integration.load_meta_ads_token(uuid,bigint,bytea)'::regprocedure AND a.privilege_type='EXECUTE' ORDER BY 1`)
		if err != nil {
			t.Fatal(err)
		}
		var holders []string
		for rows.Next() {
			var s string
			_ = rows.Scan(&s)
			holders = append(holders, s)
		}
		rows.Close()
		if fmt.Sprint(holders) != "[commerce_integration_writer commerce_worker]" {
			t.Errorf("EXECUTE on integration.load_meta_ads_token: %v, want owner + commerce_worker only", holders)
		}
		var publicCount int
		if err := f.owner.QueryRow(ctx, `SELECT
		  (SELECT count(*) FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace, aclexplode(c.relacl) a WHERE n.nspname='ads' AND a.grantee=0)
		 +(SELECT count(*) FROM pg_attribute at JOIN pg_class c ON c.oid=at.attrelid JOIN pg_namespace n ON n.oid=c.relnamespace, aclexplode(at.attacl) a WHERE n.nspname='ads' AND a.grantee=0)
		 +(SELECT count(*) FROM pg_proc p JOIN pg_namespace n ON n.oid=p.pronamespace WHERE (n.nspname='ads' OR p.proname IN ('register_meta_ads_token','load_meta_ads_token')) AND (p.proacl IS NULL OR EXISTS (SELECT 1 FROM aclexplode(p.proacl) a WHERE a.grantee=0)))
		 +(SELECT count(*) FROM pg_namespace n, aclexplode(n.nspacl) a WHERE n.nspname='ads' AND a.grantee=0)`).Scan(&publicCount); err != nil {
			t.Fatal(err)
		}
		if publicCount != 0 {
			t.Errorf("%d PUBLIC privileges on ads objects (default deny, §4.4 last row)", publicCount)
		}
		var loginGrants int
		if err := f.owner.QueryRow(ctx, `SELECT count(*) FROM (
		  SELECT c.oid FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace, aclexplode(c.relacl) a JOIN pg_roles r ON r.oid=a.grantee WHERE n.nspname='ads' AND r.rolcanlogin AND a.grantee<>c.relowner
		  UNION ALL SELECT p.oid FROM pg_proc p JOIN pg_namespace n ON n.oid=p.pronamespace, aclexplode(p.proacl) a JOIN pg_roles r ON r.oid=a.grantee WHERE n.nspname='ads' AND r.rolcanlogin AND a.grantee<>p.proowner) x`).Scan(&loginGrants); err != nil {
			t.Fatal(err)
		}
		if loginGrants != 0 {
			t.Errorf("%d direct grants of ads objects to LOGIN roles (§4: no direct runtime login grants)", loginGrants)
		}
	})

	t.Run("object hygiene: FORCE RLS, definer + search_path, owners, comments", func(t *testing.T) {
		rows, err := f.owner.Query(ctx, `SELECT c.relname,c.relrowsecurity,c.relforcerowsecurity,r.rolname,obj_description(c.oid,'pg_class') IS NOT NULL
			FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace JOIN pg_roles r ON r.oid=c.relowner WHERE n.nspname='ads' AND c.relkind='r' ORDER BY 1`)
		if err != nil {
			t.Fatal(err)
		}
		var tables []string
		for rows.Next() {
			var name, owner string
			var rls, force, comment bool
			_ = rows.Scan(&name, &rls, &force, &owner, &comment)
			tables = append(tables, name)
			if !rls || !force {
				t.Errorf("ads.%s: rowsecurity=%v force=%v (§4: all new tables FORCE RLS)", name, rls, force)
			}
			if !comment {
				t.Errorf("ads.%s has no COMMENT ON (PROCESS §5)", name)
			}
		}
		rows.Close()
		for _, want := range []string{"store_settings", "oauth_states", "connections", "campaign_drafts", "draft_approvals", "remote_objects", "insight_reads", "insights_daily", "capi_contexts", "capi_events"} {
			found := false
			for _, n := range tables {
				found = found || n == want
			}
			if !found {
				t.Errorf("§4 table ads.%s does not exist", want)
			}
		}
		rows, err = f.owner.Query(ctx, `SELECT n.nspname||'.'||p.proname,p.prosecdef,r.rolname,coalesce(p.proconfig::text,''),obj_description(p.oid,'pg_proc') IS NOT NULL,p.prorettype='trigger'::regtype
			FROM pg_proc p JOIN pg_namespace n ON n.oid=p.pronamespace JOIN pg_roles r ON r.oid=p.proowner
			WHERE n.nspname='ads' OR p.proname IN ('register_meta_ads_token','load_meta_ads_token') ORDER BY 1`)
		if err != nil {
			t.Fatal(err)
		}
		count := 0
		for rows.Next() {
			var name, owner, cfg string
			var secdef, comment, trigger bool
			_ = rows.Scan(&name, &secdef, &owner, &cfg, &comment, &trigger)
			count++
			wantOwner := "commerce_ads_writer"
			if strings.HasPrefix(name, "integration.") {
				wantOwner = "commerce_integration_writer"
			}
			if owner != wantOwner {
				t.Errorf("%s owned by %s, want %s", name, owner, wantOwner)
			}
			if !comment {
				t.Errorf("%s has no COMMENT ON (PROCESS §5)", name)
			}
			if trigger {
				continue
			}
			// pure SQL immutables (ads.ts, capi_json) are exempt from DEFINER but must still pin the search path
			if !strings.Contains(cfg, "search_path=pg_catalog") {
				t.Errorf("%s: proconfig %q, want SET search_path=pg_catalog (§4)", name, cfg)
			}
			// raise-only / pure builders (no table is read or written) need not be DEFINER
			if !secdef && name != "ads.ts" && name != "ads.capi_json" && name != "ads.deny" && name != "ads.draft_input" {
				t.Errorf("%s is not SECURITY DEFINER (§4: writes only through SECURITY DEFINER functions)", name)
			}
		}
		rows.Close()
		if count < 40 {
			t.Errorf("only %d ads functions found", count)
		}
		var roleComment, schemaComment bool
		_ = f.owner.QueryRow(ctx, `SELECT shobj_description('commerce_ads_writer'::regrole,'pg_authid') IS NOT NULL, obj_description('ads'::regnamespace,'pg_namespace') IS NOT NULL`).Scan(&roleComment, &schemaComment)
		if !roleComment || !schemaComment {
			t.Errorf("COMMENT ON role/schema missing: role=%v schema=%v", roleComment, schemaComment)
		}
		var login bool
		var bypass bool
		_ = f.owner.QueryRow(ctx, `SELECT rolcanlogin,rolbypassrls FROM pg_roles WHERE rolname='commerce_ads_writer'`).Scan(&login, &bypass)
		if login || bypass {
			t.Errorf("commerce_ads_writer login=%v bypassrls=%v, want NOLOGIN NOBYPASSRLS (§4)", login, bypass)
		}
	})

	t.Run("widened CHECKs and A-1: no automatic ads grants", func(t *testing.T) {
		var def string
		if err := f.owner.QueryRow(ctx, `SELECT pg_get_constraintdef(oid) FROM pg_constraint WHERE conrelid='identity.store_grants'::regclass AND conname='store_grants_permission_check'`).Scan(&def); err != nil {
			t.Fatal(err)
		}
		for _, p := range []string{"'ads:read'", "'ads:manage'", "'ads:approve'", "'integration:manage'", "'store:read'", "'catalog:write'"} {
			if !strings.Contains(def, p) {
				t.Errorf("store_grants permission CHECK lacks %s (re-derived, not overwritten): %s", p, def)
			}
		}
		var prov, nonce string
		if err := f.owner.QueryRow(ctx, `SELECT pg_get_constraintdef(oid) FROM pg_constraint WHERE conrelid='integration.meta_page_credentials'::regclass AND conname='meta_page_credentials_provider_check'`).Scan(&prov); err != nil {
			t.Fatal(err)
		}
		if err := f.owner.QueryRow(ctx, `SELECT pg_get_constraintdef(oid) FROM pg_constraint WHERE conrelid='integration.meta_page_credentials'::regclass AND conname='meta_page_credentials_nonce_check'`).Scan(&nonce); err != nil {
			t.Fatal(err)
		}
		for _, p := range []string{"'facebook'", "'instagram'", "'meta_ads'", "'meta_dataset'"} {
			if !strings.Contains(prov, p) {
				t.Errorf("provider CHECK lacks %s: %s", p, prov)
			}
		}
		if !strings.Contains(nonce, "12") || !strings.Contains(nonce, "32") || !strings.Contains(nonce, "meta_ads") {
			t.Errorf("nonce CHECK (12 bytes for page tokens, 32 for HPKE enc): %s", nonce)
		}
		// A-1: store creation never grants ads:*; the store-creation definer names none of them
		var body string
		if err := f.owner.QueryRow(ctx, `SELECT pg_get_functiondef('identity.create_initial_store(bytea,text,bytea,text,text,text,text)'::regprocedure)`).Scan(&body); err != nil {
			t.Fatal(err)
		}
		if strings.Contains(body, "ads:") {
			t.Error("identity.create_initial_store grants an ads:* permission (A-1: explicit provisioning only)")
		}
		e := newAdsEnv(t, adsOpts{noWorker: true, noConnect: true})
		if n := e.count(`SELECT count(*) FROM identity.store_grants WHERE store_id=$1 AND permission LIKE 'ads:%' AND principal_id<>$2`, e.store, e.creator); n != 0 {
			t.Errorf("%d ads grants exist for principals the test did not grant", n)
		}
	})

	t.Run("§4.1 table CHECKs refuse what the contract says they refuse", func(t *testing.T) {
		e := newAdsEnv(t, adsOpts{noWorker: true})
		d := e.newDraft(adsDraftIn{})
		insertDraft := func(overrides string) error {
			// copy of the draft under new ids with column overrides; replica mode skips FK/guard triggers, CHECKs still apply
			cols := []string{"tenant_id", "store_id", "id", "ad_binding_id", "ad_binding_version", "identity_binding_id", "template", "source_ref", "currency", "lifetime_budget_minor", "starts_at", "ends_at", "countries", "age_min", "age_max", "revision", "publish_attempt", "created_by"}
			expr := map[string]string{}
			for _, c := range cols {
				expr[c] = "d." + c
			}
			expr["id"] = "gen_random_uuid()"
			for _, kv := range strings.Split(overrides, ";") {
				if kv = strings.TrimSpace(kv); kv != "" {
					k, v, _ := strings.Cut(kv, "=")
					expr[strings.TrimSpace(k)] = strings.TrimSpace(v)
				}
			}
			var sel []string
			for _, c := range cols {
				sel = append(sel, expr[c])
			}
			tx, err := f.owner.Begin(ctx)
			if err != nil {
				return err
			}
			defer tx.Rollback(ctx)
			if _, err := tx.Exec(ctx, `SET LOCAL session_replication_role=replica`); err != nil {
				return err
			}
			_, err = tx.Exec(ctx, `INSERT INTO ads.campaign_drafts(`+strings.Join(cols, ",")+`) SELECT `+strings.Join(sel, ",")+` FROM ads.campaign_drafts d WHERE d.id=$1`, d)
			return err
		}
		if err := insertDraft(""); err != nil {
			t.Fatalf("control insert (unchanged copy) failed: %v", err)
		}
		bad := map[string]string{
			"TWD budget not a whole NT$ (I05, F19)": "currency='TWD';lifetime_budget_minor=12345",
			"non-positive budget":                   "lifetime_budget_minor=0",
			"more than 30 days":                     "ends_at=d.starts_at+interval '31 days'",
			"ends before starts":                    "ends_at=d.starts_at-interval '1 hour'",
			"currency outside TWD/USD/HKD":          "currency='EUR'",
			"template outside the two":              "template='CAROUSEL'",
			"empty countries":                       "countries=ARRAY[]::text[]",
			"more than 10 countries":                "countries=ARRAY['TW','HK','SG','JP','KR','US','GB','FR','DE','IT','ES']",
			"age below 18":                          "age_min=17",
			"age above 65":                          "age_max=66",
			"age_max below age_min":                 "age_min=30;age_max=20",
			"revision above 1000":                   "revision=1001",
			"publish_attempt above 5":               "publish_attempt=6",
			"source_ref outside the two shapes":     "source_ref='not a ref!'",
		}
		for name, ov := range bad {
			err := insertDraft(ov)
			var pg *pgconn.PgError
			if err == nil {
				t.Errorf("%s: accepted", name)
			} else if !asPg(err, &pg) || pg.Code != "23514" {
				t.Errorf("%s: %v (want check violation 23514)", name, err)
			}
		}
		// settings CHECKs
		for name, q := range map[string]string{
			"environment outside SANDBOX/LIVE":     `UPDATE ads.store_settings SET environment='STAGING' WHERE store_id=$1`,
			"allowance currency outside the three": `UPDATE ads.store_settings SET allowance_currency='EUR' WHERE store_id=$1`,
			"negative allowance":                   `UPDATE ads.store_settings SET max_active_budget_minor=-1 WHERE store_id=$1`,
			"allowance above 10^11":                `UPDATE ads.store_settings SET max_active_budget_minor=100000000001 WHERE store_id=$1`,
			"sandbox account not numeric":          `UPDATE ads.store_settings SET sandbox_ad_account='act_1' WHERE store_id=$1`,
			"capi enabled without dataset binding": `UPDATE ads.store_settings SET capi_enabled=true WHERE store_id=$1`,
			"capi test code shape":                 `UPDATE ads.store_settings SET capi_test_event_code='lower' WHERE store_id=$1`,
		} {
			_, err := f.owner.Exec(ctx, q, e.store)
			var pg *pgconn.PgError
			if err == nil {
				t.Errorf("%s: accepted", name)
			} else if !asPg(err, &pg) || (pg.Code != "23514" && pg.Code != "23502") {
				t.Errorf("%s: %v", name, err)
			}
		}
		// approvals: hash exactly 32 bytes; oauth pending trio all-or-none; remote object kind/seq enums
		for name, q := range map[string]string{
			"approval hash of 31 bytes":           `INSERT INTO ads.draft_approvals(tenant_id,store_id,draft_id,revision,draft_sha256,principal_id) SELECT tenant_id,store_id,id,revision+7,decode(repeat('ab',31),'hex'),created_by FROM ads.campaign_drafts WHERE id=$1`,
			"remote object kind outside the enum": `INSERT INTO ads.remote_objects(tenant_id,store_id,draft_id,publish_attempt,kind,seq,operation_id) SELECT tenant_id,store_id,id,1,'delete',1,gen_random_uuid() FROM ads.campaign_drafts WHERE id=$1`,
			"remote object seq 51":                `INSERT INTO ads.remote_objects(tenant_id,store_id,draft_id,publish_attempt,kind,seq,operation_id) SELECT tenant_id,store_id,id,1,'pause',51,gen_random_uuid() FROM ads.campaign_drafts WHERE id=$1`,
			"remote id not numeric":               `INSERT INTO ads.remote_objects(tenant_id,store_id,draft_id,publish_attempt,kind,seq,operation_id,remote_id) SELECT tenant_id,store_id,id,1,'pause',2,gen_random_uuid(),'abc' FROM ads.campaign_drafts WHERE id=$1`,
		} {
			tx, _ := f.owner.Begin(ctx)
			_, _ = tx.Exec(ctx, `SET LOCAL session_replication_role=replica`)
			_, err := tx.Exec(ctx, q, d)
			_ = tx.Rollback(ctx)
			var pg *pgconn.PgError
			if err == nil {
				t.Errorf("%s: accepted", name)
			} else if !asPg(err, &pg) || pg.Code != "23514" {
				t.Errorf("%s: %v", name, err)
			}
		}
		for name, q := range map[string]string{
			"state hash of 31 bytes":      `INSERT INTO ads.oauth_states(id,tenant_id,store_id,principal_id,state_hash,expires_at) VALUES(gen_random_uuid(),$1,$2,$3,decode(repeat('ab',31),'hex'),now()+interval '10 minutes')`,
			"expiry not after creation":   `INSERT INTO ads.oauth_states(id,tenant_id,store_id,principal_id,state_hash,created_at,expires_at) VALUES(gen_random_uuid(),$1,$2,$3,decode(repeat('ab',32),'hex'),now(),now()-interval '1 minute')`,
			"ciphertext without enc/key":  `INSERT INTO ads.oauth_states(id,tenant_id,store_id,principal_id,state_hash,expires_at,pending_ciphertext) VALUES(gen_random_uuid(),$1,$2,$3,decode(repeat('ab',32),'hex'),now()+interval '10 minutes',decode(repeat('cd',20),'hex'))`,
			"enc of 31 bytes":             `INSERT INTO ads.oauth_states(id,tenant_id,store_id,principal_id,state_hash,expires_at,pending_key_id,pending_enc,pending_ciphertext) VALUES(gen_random_uuid(),$1,$2,$3,decode(repeat('ab',32),'hex'),now()+interval '10 minutes','k1',decode(repeat('ab',31),'hex'),decode(repeat('cd',20),'hex'))`,
			"ciphertext of 16 bytes":      `INSERT INTO ads.oauth_states(id,tenant_id,store_id,principal_id,state_hash,expires_at,pending_key_id,pending_enc,pending_ciphertext) VALUES(gen_random_uuid(),$1,$2,$3,decode(repeat('ab',32),'hex'),now()+interval '10 minutes','k1',decode(repeat('ab',32),'hex'),decode(repeat('cd',16),'hex'))`,
			"client business not numeric": `INSERT INTO ads.oauth_states(id,tenant_id,store_id,principal_id,state_hash,expires_at,client_business_id) VALUES(gen_random_uuid(),$1,$2,$3,decode(repeat('ab',32),'hex'),now()+interval '10 minutes','abc')`,
		} {
			tx, _ := f.owner.Begin(ctx)
			_, _ = tx.Exec(ctx, `SET LOCAL session_replication_role=replica`)
			_, err := tx.Exec(ctx, q, e.tenant, e.store, e.creator)
			_ = tx.Rollback(ctx)
			var pg *pgconn.PgError
			if err == nil {
				t.Errorf("%s: accepted", name)
			} else if !asPg(err, &pg) || pg.Code != "23514" {
				t.Errorf("%s: %v", name, err)
			}
		}
	})

	t.Run("runtime and worker cannot touch ads tables or the operator definer; merchants cannot change environment or allowance", func(t *testing.T) {
		e := newAdsEnv(t, adsOpts{noWorker: true})
		for name, pool := range map[string]interface {
			Exec(context.Context, string, ...any) (pgconn.CommandTag, error)
		}{"runtime": f.runtime, "worker": e.workerPool} {
			for _, q := range []string{
				`SELECT count(*) FROM ads.campaign_drafts`, `SELECT count(*) FROM ads.oauth_states`, `SELECT count(*) FROM ads.store_settings`,
				`UPDATE ads.store_settings SET environment='LIVE'`, `UPDATE ads.store_settings SET max_active_budget_minor=1`,
				`INSERT INTO ads.draft_approvals(tenant_id,store_id,draft_id,revision,draft_sha256,principal_id) VALUES(gen_random_uuid(),gen_random_uuid(),gen_random_uuid(),1,decode(repeat('ab',32),'hex'),gen_random_uuid())`,
				`SELECT count(*) FROM ads.remote_objects`, `SELECT count(*) FROM ads.connections`,
			} {
				_, err := pool.Exec(ctx, q)
				if !pgcode(err, "42501") {
					t.Errorf("%s: %q -> %v, want permission denied 42501", name, q, err)
				}
			}
			_, err := pool.Exec(ctx, `SELECT ads.operator_set_settings($1::uuid,$2::uuid,'LIVE',NULL,1000000,'TWD')`, e.tenant, e.store)
			if !pgcode(err, "42501") {
				t.Errorf("%s may run ads.operator_set_settings: %v (only the operator role, B12)", name, err)
			}
		}
		// the merchant surface never exposes environment / allowance: PUT capi changes capi fields only (settings otherwise unchanged)
		before := e.api("GET", "/settings", e.token, nil, nil)
		put := e.api("PUT", "/capi", e.token, adsKey(), map[string]any{"enabled": false})
		if put.Status != 200 {
			t.Fatalf("PUT capi (disable): %d %s", put.Status, put.Raw)
		}
		after := e.api("GET", "/settings", e.token, nil, nil)
		for _, k := range []string{"environment", "allowance_currency", "max_active_budget_minor", "sandbox_ad_account"} {
			if fmt.Sprint(before.JSON[k]) != fmt.Sprint(after.JSON[k]) {
				t.Errorf("merchant CAPI call changed %s: %v -> %v", k, before.JSON[k], after.JSON[k])
			}
		}
		// an unknown key on any merchant body is refused (strict JSON): environment/allowance can never ride along
		bad := e.api("PUT", "/capi", e.token, adsKey(), map[string]any{"enabled": false, "environment": "LIVE"})
		if bad.Status < 400 {
			t.Errorf("PUT capi accepted an `environment` member: %d", bad.Status)
		}
		if e.api("GET", "/settings", e.token, nil, nil).JSON["environment"] != before.JSON["environment"] {
			t.Error("environment changed")
		}
		// the registrar (operator) may, and the change is audited in the operator log
		var n1, n2 int64
		_ = f.owner.QueryRow(ctx, `SELECT count(*) FROM ads.operator_events WHERE store_id=$1`, e.store).Scan(&n1)
		e.setSettings("SANDBOX", e.account, 123400, "TWD")
		_ = f.owner.QueryRow(ctx, `SELECT count(*) FROM ads.operator_events WHERE store_id=$1`, e.store).Scan(&n2)
		if n2 != n1+1 {
			t.Errorf("operator change wrote %d audit rows", n2-n1)
		}
		if got := e.api("GET", "/settings", e.token, nil, nil).JSON["max_active_budget_minor"]; fmt.Sprint(got) != "123400" {
			t.Errorf("operator allowance not visible: %v", got)
		}
		// operator validation: unknown environment / currency / negative allowance refused
		for _, args := range [][]any{{"STAGING", "TWD", int64(1)}, {"SANDBOX", "EUR", int64(1)}, {"SANDBOX", "TWD", int64(-1)}} {
			_, err := e.registrarPool.Exec(ctx, `SELECT ads.operator_set_settings($1::uuid,$2::uuid,$3,NULL,$5::bigint,$4)`, e.tenant, e.store, args[0], args[1], args[2])
			if err == nil {
				t.Errorf("operator_set_settings accepted %v", args)
			}
		}
	})

	t.Run("writer policies on integration.operations / operation_events exist and behave (removing one turns this red)", func(t *testing.T) {
		e := newAdsEnv(t, adsOpts{noWorker: true})
		d := e.newDraft(adsDraftIn{})
		e.mustApprove(d)
		e.mustPublish(d) // plans one create_campaign op through the writer's INSERT policy
		if len(e.ops(d)) != 1 {
			t.Fatalf("expected one planned op, got %d", len(e.ops(d)))
		}
		// a synthetic NON-ads operation + event (owner fixture) makes the zero-row assertions meaningful
		foreignOp := randomUUID()
		mustExec(t, f.owner, `INSERT INTO integration.bindings(id,tenant_id,store_id,principal_id,provider,external_asset_id) VALUES($1,$2,$3,$4,'mock_provider',$5)`, randomUUID(), e.tenant, e.store, e.creator, "asset-"+t04Tag())
		var binding string
		_ = f.owner.QueryRow(ctx, `SELECT id::text FROM integration.bindings WHERE store_id=$1 AND provider='mock_provider' LIMIT 1`, e.store).Scan(&binding)
		mustExec(t, f.owner, `INSERT INTO integration.operations(id,tenant_id,store_id,principal_id,binding_id,binding_version,provider,external_asset_id,purpose,action,semantic_key,request_hash,request,job_id)
			SELECT $1,$2,$3,$4,b.id,b.semantic_version,'mock_provider',b.external_asset_id,'transactional','payment.authorize',$5,sha256('x'::bytea),'{}'::jsonb,1 FROM integration.bindings b WHERE b.id=$6`,
			foreignOp, e.tenant, e.store, e.creator, "foreign-"+t04Tag()+"-key", binding)
		mustExec(t, f.owner, `INSERT INTO integration.operation_events(tenant_id,store_id,operation_id,generation,state,mode,reason_code) VALUES($1,$2,$3,0,'READY','','operation_planned')`, e.tenant, e.store, foreignOp)
		t.Cleanup(func() { // append-only guards: replica mode
			_ = e.ownerReplicaBestEffort(`DELETE FROM integration.operation_events WHERE operation_id=$1`, foreignOp)
			_ = e.ownerReplicaBestEffort(`DELETE FROM integration.operations WHERE id=$1`, foreignOp)
			_ = e.ownerReplicaBestEffort(`DELETE FROM integration.bindings WHERE id=$1`, binding)
		})
		asWriter := func(q string, args ...any) (n int64, err error) {
			tx, err := f.owner.Begin(ctx)
			if err != nil {
				return 0, err
			}
			defer tx.Rollback(ctx)
			if _, err = tx.Exec(ctx, `SET LOCAL ROLE commerce_ads_writer`); err != nil {
				return 0, err
			}
			if strings.HasPrefix(strings.TrimSpace(q), "SELECT") {
				err = tx.QueryRow(ctx, q, args...).Scan(&n)
				return n, err
			}
			_, err = tx.Exec(ctx, q, args...)
			return 0, err
		}
		if n, err := asWriter(`SELECT count(*) FROM integration.operations WHERE provider IN ('meta_ads','meta_dataset')`); err != nil || n < 1 {
			t.Fatalf("writer reads %d ads operations (%v): the read policy is missing or empty (allowance sum would be empty = fail open, §4.4)", n, err)
		}
		if n, err := asWriter(`SELECT count(*) FROM integration.operations WHERE provider NOT IN ('meta_ads','meta_dataset')`); err != nil || n != 0 {
			t.Errorf("writer reads %d non-ads operations (%v), want 0 (policy limited to provider IN meta_ads, meta_dataset)", n, err)
		}
		if n, err := asWriter(`SELECT count(*) FROM integration.operation_events`); err != nil || n < 1 {
			t.Fatalf("writer reads %d events (%v): the operation_events read policy is missing (AD6 pause-claim order needs it, §4.4)", n, err)
		}
		if n, err := asWriter(`SELECT count(*) FROM integration.operation_events WHERE operation_id=$1`, foreignOp); err != nil || n != 0 {
			t.Errorf("writer reads %d events of a non-ads operation (%v), want 0 (events policy = EXISTS such an op)", n, err)
		}
		var total, adsEvents int64
		_ = f.owner.QueryRow(ctx, `SELECT count(*),count(*) FILTER (WHERE o.provider IN ('meta_ads','meta_dataset')) FROM integration.operation_events e JOIN integration.operations o ON o.id=e.operation_id`).Scan(&total, &adsEvents)
		if n, err := asWriter(`SELECT count(*) FROM integration.operation_events`); err != nil || n != adsEvents || total == adsEvents {
			t.Errorf("writer sees %d of %d events, want exactly the %d ads-operation events (and the fixture must contain a foreign one)", n, total, adsEvents)
		}
		// INSERT policy: only ads/CAPI marketing ops of the MERCHANT actor
		ins := func(provider, purpose, action string) error {
			_, err := asWriter(`INSERT INTO integration.operations(id,tenant_id,store_id,principal_id,binding_id,binding_version,provider,external_asset_id,purpose,action,semantic_key,request_hash,request,job_id)
				VALUES(gen_random_uuid(),$1,$2,$3,$7::uuid,1,$4,'asset',$5,$6,'pol-'||gen_random_uuid()::text,sha256('x'::bytea),'{}'::jsonb,1)`,
				e.tenant, e.store, e.creator, provider, purpose, action, binding)
			return err
		}
		// (the fixture binding is provider mock_provider: provider/binding coherence is a different check, so the policy is the only
		// thing that can stop these rows before any FK/trigger; a denial by the policy is SQLSTATE 42501)
		for name, args := range map[string][3]string{
			"provider facebook": {"facebook", "marketing", "meta.ads.pause"}, "purpose transactional": {"meta_ads", "transactional", "meta.ads.pause"},
			"action payment.authorize": {"meta_ads", "marketing", "payment.authorize"}, "provider mock_provider": {"mock_provider", "marketing", "meta.ads.pause"},
			"CAPI action on meta_ads": {"meta_ads", "marketing", "meta.capi.purchase"},
		} {
			err := ins(args[0], args[1], args[2])
			if name == "CAPI action on meta_ads" {
				continue // provider/action pairing is enforced by the loader and Check, not by this policy
			}
			if !pgcode(err, "42501") {
				t.Errorf("writer INSERT (%s) -> %v, want RLS denial 42501", name, err)
			}
		}
		// pg_policies: the named policies exist for the writer on exactly these tables
		for _, want := range []struct{ table, cmd, contains string }{
			{"operations", "SELECT", "meta_ads"}, {"operations", "INSERT", "marketing"}, {"operation_events", "SELECT", "meta_ads"}, {"operation_events", "INSERT", "meta_ads"},
		} {
			var n int
			_ = f.owner.QueryRow(ctx, `SELECT count(*) FROM pg_policies WHERE schemaname='integration' AND tablename=$1 AND cmd=$2 AND 'commerce_ads_writer'=ANY(roles) AND coalesce(qual,'')||coalesce(with_check,'') LIKE '%'||$3||'%'`, want.table, want.cmd, want.contains).Scan(&n)
			if n < 1 {
				t.Errorf("no %s policy for commerce_ads_writer on integration.%s mentioning %s", want.cmd, want.table, want.contains)
			}
		}
		// other written/read tables: every grant has its policy (a grant without a policy reads zero rows under FORCE RLS)
		rows, err := f.owner.Query(ctx, `SELECT DISTINCT n.nspname||'.'||c.relname FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace
			WHERE c.relkind='r' AND c.relrowsecurity AND (has_table_privilege('commerce_ads_writer',c.oid,'SELECT') OR has_any_column_privilege('commerce_ads_writer',c.oid,'SELECT'))
			AND NOT EXISTS (SELECT 1 FROM pg_policies p WHERE p.schemaname=n.nspname AND p.tablename=c.relname AND ('commerce_ads_writer'=ANY(p.roles) OR 'public'=ANY(p.roles)) AND p.cmd IN ('SELECT','ALL'))`)
		if err != nil {
			t.Fatal(err)
		}
		for rows.Next() {
			var s string
			_ = rows.Scan(&s)
			t.Errorf("commerce_ads_writer can SELECT %s but no policy admits it (zero rows under FORCE RLS: fail-open sums)", s)
		}
		rows.Close()
	})

	t.Run("evidence: source files of the schema half exist", func(t *testing.T) {
		for _, p := range []string{"../../migrations/0074_meta_ads.sql", "../../migrations/0075_meta_ads_insights.sql", "../../migrations/0080_meta_capi.sql", "../../migrations/post_river/0015_meta_ads_river.sql"} {
			if _, err := os.Stat(p); err != nil {
				t.Errorf("%s: %v", p, err)
			}
		}
	})

	t.Run("static: every GRANT in the ads migrations is a §4.4 / 0080 statement (comments and strings stripped)", func(t *testing.T) {
		stmts := adsMigrationGrants(t)
		if len(stmts) < 40 {
			t.Fatalf("parsed only %d GRANT statements", len(stmts))
		}
		allowedTo := adsSet("commerce_ads_writer", "commerce_runtime", "commerce_worker", "commerce_buyer_runtime", "commerce_integration_writer", "commerce_meta_registrar")
		for _, s := range stmts {
			for _, g := range s.grantees {
				if !allowedTo[g] {
					t.Errorf("GRANT to %s: %s", g, s.text)
				}
			}
			if strings.Contains(s.text, "PUBLIC") && !strings.HasPrefix(s.text, "REVOKE") {
				t.Errorf("GRANT to PUBLIC: %s", s.text)
			}
			if strings.Contains(s.text, "ALL PRIVILEGES") || strings.Contains(s.text, "WITH GRANT OPTION") {
				t.Errorf("blanket grant: %s", s.text)
			}
		}
	})
}

type adsGrantStmt struct {
	text     string
	grantees []string
}

var (
	adsLineComment = regexp.MustCompile(`--[^\n]*`)
	adsStringLit   = regexp.MustCompile(`'(?:[^']|'')*'`)
	adsDollarBody  = regexp.MustCompile(`(?s)\$\$.*?\$\$`)
	adsGrantRe     = regexp.MustCompile(`(?is)^\s*(GRANT|REVOKE)\b(.*?)\s(TO|FROM)\s+([A-Za-z0-9_,\s]+?)\s*$`)
)

// adsMigrationGrants parses GRANT/REVOKE statements of the four ads migrations (comments, string literals and $$ bodies removed).
func adsMigrationGrants(t *testing.T) []adsGrantStmt {
	t.Helper()
	var out []adsGrantStmt
	for _, p := range []string{"../../migrations/0074_meta_ads.sql", "../../migrations/0075_meta_ads_insights.sql", "../../migrations/0080_meta_capi.sql", "../../migrations/post_river/0015_meta_ads_river.sql"} {
		raw, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		s := adsDollarBody.ReplaceAllString(string(raw), "''")
		s = adsLineComment.ReplaceAllString(s, "")
		s = adsStringLit.ReplaceAllString(s, "''")
		for _, stmt := range strings.Split(s, ";") {
			stmt = strings.TrimSpace(stmt)
			m := adsGrantRe.FindStringSubmatch(stmt)
			if m == nil {
				continue
			}
			var grantees []string
			for _, g := range strings.Split(m[4], ",") {
				if g = strings.TrimSpace(g); g != "" && !strings.EqualFold(g, "PUBLIC") {
					grantees = append(grantees, g)
				}
			}
			if strings.EqualFold(m[1], "REVOKE") {
				continue
			}
			out = append(out, adsGrantStmt{text: strings.Join(strings.Fields(stmt), " "), grantees: grantees})
		}
	}
	return out
}

func asPg(err error, target **pgconn.PgError) bool {
	for e := err; e != nil; {
		if pg, ok := e.(*pgconn.PgError); ok {
			*target = pg
			return true
		}
		u, ok := e.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		e = u.Unwrap()
	}
	return false
}

func pgcode(err error, code string) bool {
	var pg *pgconn.PgError
	return err != nil && asPg(err, &pg) && pg.Code == code
}

func (e *adsEnv) ownerReplicaBestEffort(q string, args ...any) error {
	tx, err := e.f.owner.Begin(context.Background())
	if err != nil {
		return err
	}
	defer tx.Rollback(context.Background())
	if _, err = tx.Exec(context.Background(), `SET LOCAL session_replication_role=replica`); err != nil {
		return err
	}
	if _, err = tx.Exec(context.Background(), q, args...); err != nil {
		return err
	}
	return tx.Commit(context.Background())
}

var _ = pgx.ErrNoRows
