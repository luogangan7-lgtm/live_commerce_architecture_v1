package foundation_test

// CB02 (contracts/customers-billing-v1.md §3.1, §3.2, §8; tier REAL_PG in an isolated PG 18 database).
// Written from the contract text and the FROZEN blocks of customers-core.md / billing-core.md only. Helper
// prefix `cbs`. What it proves:
//   - roles, schemas, owners, SECURITY DEFINER + search_path, EXECUTE ACLs (per function, no PUBLIC),
//     schema USAGE = owners + EXECUTE grantees only;
//   - FORCE RLS on the four new tables, policies from pg_policies, table ACLs, the effective column-privilege
//     matrix of commerce_privacy_writer / commerce_billing_writer from information_schema, and that no
//     PUBLIC/buyer/worker/runtime role can read customers.* / billing.*;
//   - the CHECK / UNIQUE / FK / set-once / terminal rules of the four tables, by inserting violating rows;
//   - the permission CHECK holds each of the three new values exactly once (re-derivation idempotent);
//   - the claim-window guard trigger shape; no R3 role is referenced by 0078/0079;
//   - a fresh store creator receives the three permissions once, a replay adds nothing, nobody else gains
//     anything, and the create_initial_store body differs from its 0065 definition only by the array.
// Disclosed deviations found while writing it (escalated in the unit return, not "fixed" here):
//   D-S1 §3.1 lists `GRANT USAGE ON SCHEMA customers TO commerce_privacy_writer, commerce_auth` but the
//        runtime roles must also hold USAGE to EXECUTE the granted functions; the gate therefore asserts
//        USAGE == owners + EXECUTE grantees, which is what PostgreSQL requires (not the shorter list).
//   D-S2 privacy_writer effective privileges are compared against the frozen §3.1 list plus the four
//        column reads cbsPrivacyExtra names (control.stores(name) for "store name" in the export §5 and
//        claims.bundles(platform,session_id,line_count) for the claims summary §5); anything else fails.

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"livecommerce/internal/identity"
	"livecommerce/internal/platform"
)

type cbsEnv struct {
	t *testing.T
	f *testFixture
}

func (e cbsEnv) list(q string, args ...any) []string {
	e.t.Helper()
	rows, err := e.f.owner.Query(context.Background(), q, args...)
	if err != nil {
		e.t.Fatalf("query %q: %v", q, err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s *string
		if err := rows.Scan(&s); err != nil {
			e.t.Fatal(err)
		}
		if s != nil {
			out = append(out, *s)
		}
	}
	if err := rows.Err(); err != nil {
		e.t.Fatal(err)
	}
	sort.Strings(out)
	return out
}

func (e cbsEnv) one(q string, args ...any) string {
	e.t.Helper()
	var s *string
	if err := e.f.owner.QueryRow(context.Background(), q, args...).Scan(&s); err != nil {
		e.t.Fatalf("query %q: %v", q, err)
	}
	if s == nil {
		return ""
	}
	return *s
}

func (e cbsEnv) bool(q string, args ...any) bool {
	e.t.Helper()
	var b bool
	if err := e.f.owner.QueryRow(context.Background(), q, args...).Scan(&b); err != nil {
		e.t.Fatalf("query %q: %v", q, err)
	}
	return b
}

// state runs one statement through the owner pool and returns its SQLSTATE ("" = success).
func (e cbsEnv) state(q string, args ...any) string {
	_, err := e.f.owner.Exec(context.Background(), q, args...)
	var pg *pgconn.PgError
	if errors.As(err, &pg) {
		return pg.Code
	}
	if err != nil {
		e.t.Fatalf("non-SQL failure for %q: %v", q, err)
	}
	return ""
}

func cbsEq(t *testing.T, label string, got, want []string) {
	t.Helper()
	sort.Strings(want)
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("%s\n got  %v\n want %v", label, got, want)
	}
}

// cbsNoLogin is the role attribute set §3.1/§3.2 give both new roles.
func cbsNoLogin(t *testing.T, e cbsEnv, role string) {
	t.Helper()
	var login, super, bypass, createDB, createRole, repl bool
	if err := e.f.owner.QueryRow(context.Background(), `SELECT rolcanlogin,rolsuper,rolbypassrls,rolcreatedb,rolcreaterole,rolreplication FROM pg_roles WHERE rolname=$1`, role).
		Scan(&login, &super, &bypass, &createDB, &createRole, &repl); err != nil {
		t.Fatalf("role %s: %v", role, err)
	}
	if login || super || bypass || createDB || createRole || repl {
		t.Errorf("%s must be NOLOGIN NOSUPERUSER NOBYPASSRLS NOCREATEDB NOCREATEROLE NOREPLICATION: login=%v super=%v bypass=%v createdb=%v createrole=%v repl=%v", role, login, super, bypass, createDB, createRole, repl)
	}
}

type cbsFn struct {
	sig, owner string
	exec       []string // exact EXECUTE grantees besides the owner; never PUBLIC
	stable     bool
}

const (
	cbsApplyArgs   = "text,text,text,text,text,timestamp with time zone,timestamp with time zone,boolean,timestamp with time zone,timestamp with time zone,uuid,boolean"
	cbsRefreshArgs = "bytea,uuid," + cbsApplyArgs
)

// The two commerce_ads_writer grantees are R3's, not 0078/0079's: meta-ads-v1 §4.3/§4.4 rows of
// 0080_meta_capi.sql (A-3 consent, §5.2 standing), placed after 0079 by C-7; the C-7 subtest proves where they live.
var cbsFunctions = []cbsFn{
	{"customers.consent_allows(uuid,uuid,uuid,text,text)", "commerce_privacy_writer", []string{"commerce_auth", "commerce_ads_writer"}, true},
	{"customers.buyer_set_consent(bytea,uuid,text,text,boolean,text,text,uuid)", "commerce_privacy_writer", []string{"commerce_buyer_runtime"}, false},
	{"customers.merchant_withdraw_consent(bytea,uuid,uuid,text,text,uuid)", "commerce_privacy_writer", []string{"commerce_runtime"}, false},
	{"customers.record_export(bytea,uuid,uuid,text,uuid,jsonb)", "commerce_privacy_writer", []string{"commerce_runtime", "commerce_buyer_runtime"}, false},
	{"customers.erase_owner(bytea,uuid,uuid,text,uuid)", "commerce_privacy_writer", []string{"commerce_runtime", "commerce_buyer_runtime"}, false},
	{"customers.apply_erasure(uuid,uuid,uuid)", "commerce_privacy_writer", nil, false},
	{"customers.replay_erasures(uuid[])", "commerce_privacy_writer", nil, false},
	{"identity.read_merchant_customers(bytea,uuid,uuid,integer,timestamp with time zone,uuid,text)", "commerce_auth", []string{"commerce_runtime"}, false},
	{"identity.read_finance_summary(bytea,uuid,date,date)", "commerce_auth", []string{"commerce_runtime"}, false},
	{"identity.export_finance_summary(bytea,uuid,date,date)", "commerce_auth", []string{"commerce_runtime"}, false},
	{"identity.read_billing(bytea,uuid)", "commerce_auth", []string{"commerce_runtime"}, false},
	{"identity.read_billing_standing(bytea,uuid)", "commerce_auth", []string{"commerce_runtime"}, false},
	{"billing.store_standing(uuid,uuid)", "commerce_billing_writer", []string{"commerce_auth", "commerce_ads_writer"}, true},
	{"billing.platform_account_conflict(text)", "commerce_billing_writer", []string{"commerce_runtime", "commerce_stripe_ingress"}, false},
	{"billing.pin_customer(bytea,uuid,text,text,text)", "commerce_billing_writer", []string{"commerce_runtime"}, false},
	{"billing.apply_subscription(" + cbsApplyArgs + ")", "commerce_billing_writer", []string{"commerce_stripe_ingress"}, false},
	{"billing.refresh_subscription(" + cbsRefreshArgs + ")", "commerce_billing_writer", []string{"commerce_runtime"}, false},
	{"billing.record_checkout_session(bytea,uuid,text,text,timestamp with time zone)", "commerce_billing_writer", []string{"commerce_runtime"}, false},
	{"billing.guard_window_open()", "commerce_billing_writer", nil, false},
}

var cbsTables = []string{"customers.consent_events", "customers.privacy_actions", "billing.store_customers", "billing.subscriptions"}

// cbsPriv is one entry of an expected effective column-privilege set.
type cbsPriv struct {
	table, priv string
	cols        []string // nil = every column
}

// cbsPrivacyFrozen is the §3.1 grant list of commerce_privacy_writer. required: entries whose columns
// must all be held; SELECT without columns on destination_snapshots is "SELECT" in the contract and is
// held at least on the key columns (the implementation may narrow it, never widen it, see D-S2).
var cbsPrivacyFrozen = []cbsPriv{
	{"customers.consent_events", "SELECT", nil}, {"customers.consent_events", "INSERT", nil},
	{"customers.privacy_actions", "SELECT", nil}, {"customers.privacy_actions", "INSERT", nil},
	{"buyer.owners", "SELECT", nil}, {"buyer.owners", "UPDATE", []string{"active"}},
	{"buyer.capability_sessions", "SELECT", nil}, {"buyer.capability_sessions", "UPDATE", []string{"revoked_at"}},
	{"buyer.capability_events", "INSERT", nil},
	{"storefront.destination_snapshots", "SELECT", nil},
	{"storefront.destination_snapshots", "UPDATE", []string{"recipient_name", "phone", "region", "city", "postal_code", "line1", "line2"}},
	{"checkout.orders", "SELECT", []string{"tenant_id", "store_id", "owner_id", "id", "destination_id", "commercial_state", "expires_at"}},
	{"claims.bundles", "SELECT", []string{"tenant_id", "store_id", "id", "owner_id", "bound_at", "label"}},
	{"claims.bundles", "UPDATE", []string{"label"}},
	{"payments.stripe_sessions", "SELECT", []string{"tenant_id", "store_id", "owner_id", "expires_at"}},
	{"payments.stripe_refunds", "SELECT", []string{"tenant_id", "store_id", "id", "owner_id"}},
	{"payments.refund_facts", "SELECT", []string{"tenant_id", "store_id", "refund_id", "kind"}},
	{"ops.audit_events", "INSERT", nil},
}

var cbsPrivacyRequiredNarrow = map[string][]string{"storefront.destination_snapshots:SELECT": {"tenant_id", "store_id", "id", "owner_id"}}

// D-S2: reads the contract text elsewhere justifies (export content, claims summary).
var cbsPrivacyExtra = []cbsPriv{
	{"control.stores", "SELECT", []string{"id", "name", "tenant_id"}},
	{"claims.bundles", "SELECT", []string{"platform", "session_id", "line_count"}},
}

var cbsBillingFrozen = []cbsPriv{
	{"billing.store_customers", "SELECT", nil}, {"billing.store_customers", "INSERT", nil},
	{"billing.subscriptions", "SELECT", nil}, {"billing.subscriptions", "INSERT", nil},
	{"integration.merchant_accounts", "SELECT", []string{"provider", "account_id"}},
	{"ops.audit_events", "INSERT", nil}, // pin_customer audit `billing.customer_pinned` (§3.2)
}

// cbsBillingUpdate: UPDATE is allowed on the mutable columns only (set-once columns never).
var cbsBillingUpdateAllowed = map[string][]string{
	"billing.store_customers": {"open_checkout_session_id", "open_checkout_expires_at"},
	"billing.subscriptions":   {"status", "price_id", "current_period_start", "current_period_end", "cancel_at_period_end", "retrieved_at", "updated_at"},
}

func (e cbsEnv) columns(table string) []string {
	return e.list(`SELECT attname::text FROM pg_attribute WHERE attrelid=$1::regclass AND attnum>0 AND NOT attisdropped`, table)
}

// effective returns the directly granted column privileges of role as "table.column:PRIV".
func (e cbsEnv) effective(role string) map[string]bool {
	out := map[string]bool{}
	for _, s := range e.list(`SELECT table_schema||'.'||table_name||'.'||column_name||':'||privilege_type FROM information_schema.column_privileges
	 WHERE grantee=$1 AND table_schema NOT IN ('pg_catalog','information_schema')`, role) {
		out[s] = true
	}
	return out
}

func (e cbsEnv) expand(p cbsPriv) []string {
	cols := p.cols
	if cols == nil {
		cols = e.columns(p.table)
	}
	out := make([]string, len(cols))
	for i, c := range cols {
		out[i] = p.table + "." + c + ":" + p.priv
	}
	return out
}

func TestCustomersBillingCB02Schema(t *testing.T) {
	f := pwIsolatedFixture(t)
	e := cbsEnv{t: t, f: f}
	ctx := context.Background()

	t.Run("roles and schemas", func(t *testing.T) {
		cbsNoLogin(t, e, "commerce_privacy_writer")
		cbsNoLogin(t, e, "commerce_billing_writer")
		for schema, base := range map[string][]string{"customers": {"commerce_privacy_writer", "commerce_auth"}, "billing": {"commerce_billing_writer", "commerce_auth"}} {
			want := map[string]bool{}
			for _, r := range base {
				want[r] = true
			}
			for _, fn := range cbsFunctions {
				if strings.HasPrefix(fn.sig, schema+".") {
					for _, r := range fn.exec {
						want[r] = true // D-S1: EXECUTE needs schema USAGE
					}
				}
			}
			var roles []string
			for r := range want {
				roles = append(roles, r)
			}
			got := e.list(`SELECT coalesce(nullif(pg_get_userbyid(a.grantee),''),'PUBLIC') FROM pg_namespace n, aclexplode(n.nspacl) a
			 WHERE n.nspname=$1 AND a.privilege_type='USAGE' AND a.grantee<>n.nspowner`, schema)
			cbsEq(t, "schema "+schema+" USAGE grantees (owners + EXECUTE grantees, never PUBLIC)", got, roles)
			if e.bool(`SELECT has_schema_privilege('commerce_buyer_writer',$1,'USAGE') OR has_schema_privilege('commerce_worker',$1,'USAGE')`, schema) {
				t.Errorf("schema %s must not be usable by the buyer writer / worker roles", schema)
			}
		}
	})

	t.Run("tables: owner, FORCE RLS, ACL, no public/buyer/worker read", func(t *testing.T) {
		owner := e.one(`SELECT pg_get_userbyid(relowner) FROM pg_class WHERE oid='checkout.orders'::regclass`)
		for _, tb := range cbsTables {
			var rls, force bool
			var o string
			if err := f.owner.QueryRow(ctx, `SELECT relrowsecurity,relforcerowsecurity,pg_get_userbyid(relowner) FROM pg_class WHERE oid=$1::regclass`, tb).Scan(&rls, &force, &o); err != nil {
				t.Fatal(err)
			}
			if !rls || !force || o != owner {
				t.Errorf("%s: rowsecurity=%v force=%v owner=%s (want true,true,%s)", tb, rls, force, o, owner)
			}
			if e.bool(`SELECT EXISTS(SELECT 1 FROM pg_class c, aclexplode(coalesce(c.relacl,acldefault('r',c.relowner))) a WHERE c.oid=$1::regclass AND a.grantee=0)`, tb) {
				t.Errorf("%s grants a privilege to PUBLIC", tb)
			}
			got := e.list(`SELECT pg_get_userbyid(a.grantee)||':'||a.privilege_type FROM pg_class c, aclexplode(c.relacl) a WHERE c.oid=$1::regclass AND a.grantee<>c.relowner`, tb)
			var want []string
			switch tb {
			case "customers.consent_events", "customers.privacy_actions":
				want = []string{"commerce_privacy_writer:SELECT", "commerce_privacy_writer:INSERT", "commerce_auth:SELECT"} // append-only: nobody gets UPDATE/DELETE
			default:
				want = []string{"commerce_billing_writer:SELECT", "commerce_billing_writer:INSERT", "commerce_auth:SELECT"}
			}
			// billing UPDATE is a column privilege (checked below); a table-level UPDATE would be wider than the contract
			cbsEq(t, tb+" table ACL", got, want)
		}
		allowed := map[string][]string{"customers": {"commerce_privacy_writer", "commerce_auth"}, "billing": {"commerce_billing_writer", "commerce_auth"}}
		roles := e.list(`SELECT rolname::text FROM pg_roles WHERE NOT rolsuper AND rolname !~ '^pg_'`)
		for _, tb := range cbsTables {
			schema := strings.SplitN(tb, ".", 2)[0]
			for _, r := range roles {
				inherits := false
				for _, a := range allowed[schema] {
					if e.bool(`SELECT pg_has_role($1,$2,'USAGE')`, r, a) {
						inherits = true
					}
				}
				if got := e.bool(`SELECT has_table_privilege($1,$2,'SELECT')`, r, tb); got && !inherits {
					t.Errorf("role %s can read %s but is neither an allowed reader nor inherits one", r, tb)
				}
			}
		}
		for _, r := range []string{"commerce_buyer_runtime", "commerce_buyer_writer", "commerce_worker", "commerce_runtime", "commerce_checkout_runtime", "commerce_checkout_writer", "commerce_stripe_ingress", "commerce_integration_writer"} {
			for _, tb := range cbsTables {
				if e.bool(`SELECT has_table_privilege($1,$2,'SELECT') OR has_table_privilege($1,$2,'INSERT') OR has_table_privilege($1,$2,'UPDATE') OR has_table_privilege($1,$2,'DELETE')`, r, tb) {
					t.Errorf("%s holds a direct privilege on %s (contract: no login role reads customers.* / billing.* directly)", r, tb)
				}
			}
		}
		// commerce_auth reads only: no write privilege on any of the four tables
		for _, tb := range cbsTables {
			for _, p := range []string{"INSERT", "UPDATE", "DELETE", "TRUNCATE"} {
				if e.bool(`SELECT has_table_privilege('commerce_auth',$1,$2)`, tb, p) {
					t.Errorf("commerce_auth can %s %s", p, tb)
				}
			}
		}
	})

	t.Run("effective column privileges of the two writers", func(t *testing.T) {
		want := map[string]bool{}
		for _, p := range cbsPrivacyFrozen {
			for _, k := range e.expand(p) {
				want[k] = true
			}
		}
		allowed := map[string]bool{}
		for k := range want {
			allowed[k] = true
		}
		for _, p := range cbsPrivacyExtra {
			for _, k := range e.expand(p) {
				allowed[k] = true
			}
		}
		got := e.effective("commerce_privacy_writer")
		for k := range got {
			if !allowed[k] {
				t.Errorf("commerce_privacy_writer holds %s beyond the frozen §3.1 list (D-S2 allows only %v)", k, cbsPrivacyExtra)
			}
		}
		for _, p := range cbsPrivacyFrozen {
			need := e.expand(p)
			if n, ok := cbsPrivacyRequiredNarrow[p.table+":"+p.priv]; ok {
				need = e.expand(cbsPriv{p.table, p.priv, n})
			}
			for _, k := range need {
				if !got[k] {
					t.Errorf("commerce_privacy_writer lacks the frozen privilege %s", k)
				}
			}
		}
		for k := range got {
			if strings.HasPrefix(k, "claims.bundles.actor_key") || strings.HasPrefix(k, "claims.meta_intake") || strings.HasPrefix(k, "claims.links") ||
				strings.HasPrefix(k, "live.claim_sources") || strings.HasPrefix(k, "social.") {
				t.Errorf("privacy writer holds %s (CD7 never touches actor_key, claims.meta_intake, live.claim_sources, social.*)", k)
			}
		}

		bAllowed := map[string]bool{}
		for _, p := range cbsBillingFrozen {
			for _, k := range e.expand(p) {
				bAllowed[k] = true
			}
		}
		for tb, cols := range cbsBillingUpdateAllowed {
			for _, k := range e.expand(cbsPriv{tb, "UPDATE", cols}) {
				bAllowed[k] = true
			}
		}
		bGot := e.effective("commerce_billing_writer")
		for k := range bGot {
			if !bAllowed[k] {
				t.Errorf("commerce_billing_writer holds %s beyond §3.2 (billing.*, merchant_accounts(provider,account_id), audit insert; UPDATE never on set-once columns)", k)
			}
		}
		for _, p := range cbsBillingFrozen {
			for _, k := range e.expand(p) {
				if !bGot[k] {
					t.Errorf("commerce_billing_writer lacks %s", k)
				}
			}
		}
		for k := range bGot {
			if strings.HasPrefix(k, "buyer.") || strings.HasPrefix(k, "storefront.") || strings.HasPrefix(k, "checkout.") || strings.HasPrefix(k, "payments.") || strings.HasPrefix(k, "customers.") {
				t.Errorf("commerce_billing_writer must not read buyer/PII/payment data: %s", k)
			}
		}
		for _, r := range []string{"commerce_privacy_writer", "commerce_billing_writer"} {
			if e.bool(`SELECT EXISTS(SELECT 1 FROM information_schema.table_privileges WHERE grantee=$1 AND privilege_type IN ('DELETE','TRUNCATE','TRIGGER','REFERENCES'))`, r) {
				t.Errorf("%s holds DELETE/TRUNCATE/TRIGGER/REFERENCES somewhere", r)
			}
		}
		if !e.bool(`SELECT bool_and(has_function_privilege('commerce_privacy_writer',p.oid,'EXECUTE')) FROM pg_proc p JOIN pg_namespace n ON n.oid=p.pronamespace WHERE (n.nspname,p.proname) IN (('identity','resolve_access'),('buyer','resolve_scope'))`) {
			t.Error("commerce_privacy_writer lacks EXECUTE on identity.resolve_access / buyer.resolve_scope (§3.1)")
		}
	})

	t.Run("policies", func(t *testing.T) {
		type pol struct{ table, name, role, cmd, qual, check string }
		var all []pol
		rows, err := f.owner.Query(ctx, `SELECT schemaname||'.'||tablename,policyname,unnest(roles)::text,cmd,coalesce(qual,''),coalesce(with_check,'') FROM pg_policies WHERE permissive='PERMISSIVE'`)
		if err != nil {
			t.Fatal(err)
		}
		for rows.Next() {
			var p pol
			if err := rows.Scan(&p.table, &p.name, &p.role, &p.cmd, &p.qual, &p.check); err != nil {
				t.Fatal(err)
			}
			all = append(all, p)
		}
		rows.Close()
		scoped := func(s string) bool {
			return strings.Contains(s, "app.tenant_id") && (strings.Contains(s, "app.store_id"))
		}
		covers := func(table, role, cmd string) []pol {
			var out []pol
			for _, p := range all {
				if p.table == table && p.role == role && (p.cmd == cmd || p.cmd == "ALL") {
					out = append(out, p)
				}
			}
			return out
		}
		// every privilege that FORCE RLS would otherwise deny has a policy
		for _, role := range []string{"commerce_privacy_writer", "commerce_billing_writer"} {
			for k := range e.effective(role) {
				i := strings.LastIndex(k, ".")
				table, rest := k[:strings.Index(k, ":")], k
				table = table[:strings.LastIndex(table, ".")]
				_ = i
				cmd := rest[strings.Index(rest, ":")+1:]
				relForced := e.bool(`SELECT relforcerowsecurity FROM pg_class WHERE oid=$1::regclass`, table)
				if relForced && len(covers(table, role, cmd)) == 0 {
					t.Errorf("FORCE RLS on %s but %s has %s with no policy", table, role, cmd)
				}
			}
		}
		for _, tb := range cbsTables[:2] {
			if len(covers(tb, "commerce_auth", "SELECT")) == 0 {
				t.Errorf("commerce_auth has SELECT on %s and no policy (\"policies for its reads\", §3.1)", tb)
			}
		}
		for _, tb := range cbsTables[2:] {
			if len(covers(tb, "commerce_auth", "SELECT")) == 0 {
				t.Errorf("commerce_auth has SELECT on %s and no explicit GUC-scoped policy (§3.2)", tb)
			}
			for _, p := range covers(tb, "commerce_auth", "SELECT") {
				if !scoped(p.qual) {
					t.Errorf("%s: commerce_auth policy %s is not GUC-scoped: %s", tb, p.name, p.qual)
				}
			}
			for _, cmd := range []string{"SELECT", "INSERT", "UPDATE"} {
				ps := covers(tb, "commerce_billing_writer", cmd)
				if len(ps) == 0 {
					t.Errorf("%s: no commerce_billing_writer policy for %s", tb, cmd)
				}
				for _, p := range ps {
					if (cmd != "INSERT" && p.qual != "true") || (cmd != "SELECT" && p.check != "true" && p.cmd != "SELECT") {
						t.Errorf("%s: billing writer policy %s must be USING(true) (§3.2: it reads by ids it resolved itself): qual=%q check=%q", tb, p.name, p.qual, p.check)
					}
				}
			}
		}
		// consent_allows reads by its own arguments: a USING(true) SELECT policy for the owner role (frozen block)
		hasTrue := false
		for _, p := range covers("customers.consent_events", "commerce_privacy_writer", "SELECT") {
			hasTrue = hasTrue || p.qual == "true"
		}
		if !hasTrue {
			t.Error("customers.consent_events needs a USING(true) SELECT policy for commerce_privacy_writer (consent_allows works from any GUC state)")
		}
		// writes and every read of money/PII tables are GUC-scoped to the store
		for _, p := range all {
			if p.role != "commerce_privacy_writer" {
				continue
			}
			isWrite := p.cmd == "INSERT" || p.cmd == "UPDATE" || p.cmd == "ALL"
			pii := map[string]bool{"checkout.orders": true, "claims.bundles": true, "storefront.destination_snapshots": true, "payments.stripe_sessions": true,
				"payments.stripe_refunds": true, "payments.refund_facts": true, "ops.audit_events": true, "control.stores": true}[p.table]
			if p.cmd == "ALL" && p.table != "" {
				t.Errorf("privacy writer policy %s on %s is FOR ALL (a policy must name its command)", p.name, p.table)
			}
			if isWrite && !scoped(p.check) {
				t.Errorf("privacy writer write policy %s on %s is not GUC-scoped: %s", p.name, p.table, p.check)
			}
			if p.cmd == "SELECT" && pii && !scoped(p.qual) {
				t.Errorf("privacy writer read policy %s on %s must be GUC-scoped (CD7 scope): %s", p.name, p.table, p.qual)
			}
			if p.cmd == "UPDATE" && !scoped(p.qual) {
				t.Errorf("privacy writer update policy %s on %s USING is not GUC-scoped: %s", p.name, p.table, p.qual)
			}
		}
		// audit inserts are limited to the contract's actions
		auditRe := map[string][]string{"commerce_privacy_writer": {"customers.consent_withdrawn", "customers.exported", "customers.erased"}, "commerce_billing_writer": {"billing.customer_pinned"}}
		for role, actions := range auditRe {
			ps := covers("ops.audit_events", role, "INSERT")
			if len(ps) == 0 {
				t.Errorf("%s has no ops.audit_events INSERT policy", role)
			}
			for _, p := range ps {
				for _, a := range actions {
					if !strings.Contains(p.check, "'"+a+"'") {
						t.Errorf("%s audit policy lacks action %s: %s", role, a, p.check)
					}
				}
				if n := strings.Count(p.check, "::text"); n < len(actions) || regexp.MustCompile(`'[a-z_]+\.[a-z_]+'`).FindAllString(p.check, -1) == nil {
					t.Errorf("%s audit policy shape: %s", role, p.check)
				}
				var extra []string
				for _, lit := range regexp.MustCompile(`'([a-z_]+\.[a-z_]+)'`).FindAllStringSubmatch(p.check, -1) {
					if strings.HasPrefix(lit[1], "app.") { // the GUC names of the scope check, not audit actions
						continue
					}
					ok := false
					for _, a := range actions {
						ok = ok || a == lit[1]
					}
					if !ok {
						extra = append(extra, lit[1])
					}
				}
				if len(extra) > 0 {
					t.Errorf("%s audit policy admits actions beyond the contract's: %v", role, extra)
				}
			}
		}
		// platform_account_conflict reads merchant_accounts through its own read policy
		if len(covers("integration.merchant_accounts", "commerce_billing_writer", "SELECT")) == 0 {
			t.Error("commerce_billing_writer has SELECT(provider,account_id) on integration.merchant_accounts and no read policy (§3.2)")
		}
		// the dropped buyer/worker roles have no policy on the new tables
		for _, p := range all {
			if strings.HasPrefix(p.table, "customers.") || strings.HasPrefix(p.table, "billing.") {
				switch p.role {
				case "commerce_privacy_writer", "commerce_billing_writer", "commerce_auth":
				default:
					t.Errorf("unexpected policy %s on %s for role %s", p.name, p.table, p.role)
				}
			}
		}
	})

	t.Run("functions: owner, SECURITY DEFINER, search_path, EXECUTE, volatility", func(t *testing.T) {
		for _, fn := range cbsFunctions {
			var owner, config string
			var definer, stable bool
			err := f.owner.QueryRow(ctx, `SELECT pg_get_userbyid(p.proowner),coalesce(p.proconfig::text,''),p.prosecdef,p.provolatile='s' FROM pg_proc p WHERE p.oid=$1::regprocedure`, fn.sig).Scan(&owner, &config, &definer, &stable)
			if err != nil {
				t.Errorf("%s does not exist: %v", fn.sig, err)
				continue
			}
			if owner != fn.owner || !definer || !strings.Contains(config, "search_path=pg_catalog") {
				t.Errorf("%s: owner=%s (want %s) definer=%v config=%s (want SECURITY DEFINER SET search_path=pg_catalog)", fn.sig, owner, fn.owner, definer, config)
			}
			if fn.stable && !stable {
				t.Errorf("%s must be STABLE (contract)", fn.sig)
			}
			got := e.list(`SELECT coalesce(nullif(pg_get_userbyid(a.grantee),''),'PUBLIC') FROM pg_proc p, aclexplode(coalesce(p.proacl,acldefault('f',p.proowner))) a
			 WHERE p.oid=$1::regprocedure AND a.privilege_type='EXECUTE' AND a.grantee<>p.proowner`, fn.sig)
			cbsEq(t, fn.sig+" EXECUTE grantees", got, append([]string(nil), fn.exec...))
		}
		// every function of the two new schemas (contract-listed or not): no PUBLIC EXECUTE, pinned search_path, definer unless trigger
		for _, s := range e.list(`SELECT p.oid::regprocedure::text||'|'||coalesce(p.proconfig::text,'')||'|'||p.prosecdef::text||'|'||(p.prorettype='trigger'::regtype)::text
		 FROM pg_proc p JOIN pg_namespace n ON n.oid=p.pronamespace WHERE n.nspname IN ('customers','billing')`) {
			parts := strings.Split(s, "|")
			if !strings.Contains(parts[1], "search_path=pg_catalog") || (parts[2] != "true" && parts[3] != "true") {
				t.Errorf("function %s: config=%s definer=%s trigger=%s", parts[0], parts[1], parts[2], parts[3])
			}
			if e.bool(`SELECT EXISTS(SELECT 1 FROM pg_proc p, aclexplode(coalesce(p.proacl,acldefault('f',p.proowner))) a WHERE p.oid=$1::regprocedure AND a.grantee=0)`, parts[0]) {
				t.Errorf("function %s is executable by PUBLIC", parts[0])
			}
		}
		// runtime lacks the ingress/read-side-only functions (also CB06)
		for _, sig := range []string{"billing.apply_subscription(" + cbsApplyArgs + ")", "billing.store_standing(uuid,uuid)", "customers.consent_allows(uuid,uuid,uuid,text,text)"} {
			for _, r := range []string{"commerce_runtime", "commerce_buyer_runtime", "commerce_worker"} {
				if e.bool(`SELECT has_function_privilege($1,$2::regprocedure,'EXECUTE')`, r, sig) {
					t.Errorf("%s can execute %s", r, sig)
				}
			}
		}
	})

	t.Run("claim-window guard trigger shape (BD5)", func(t *testing.T) {
		defs := e.list(`SELECT pg_get_triggerdef(t.oid) FROM pg_trigger t WHERE t.tgrelid='live.claim_windows'::regclass AND NOT t.tgisinternal AND t.tgname='billing_guard_window_open'`)
		if len(defs) != 1 {
			t.Fatalf("want exactly one trigger billing_guard_window_open on live.claim_windows, got %d", len(defs))
		}
		d := defs[0]
		for _, frag := range []string{"BEFORE INSERT OR UPDATE OF state ON live.claim_windows", "FOR EACH ROW", "billing.guard_window_open()"} {
			if !strings.Contains(d, frag) {
				t.Errorf("trigger definition lacks %q: %s", frag, d)
			}
		}
		if !regexp.MustCompile(`WHEN \(\(?new\.state = 'OPEN'`).MatchString(d) {
			t.Errorf("trigger WHEN clause must be NEW.state='OPEN': %s", d)
		}
		if e.one(`SELECT tgenabled::text FROM pg_trigger WHERE tgrelid='live.claim_windows'::regclass AND tgname='billing_guard_window_open'`) != "O" {
			t.Error("guard trigger is not enabled")
		}
		if e.bool(`SELECT has_function_privilege('commerce_runtime','billing.guard_window_open()','EXECUTE')`) {
			t.Error("commerce_runtime can call the guard function directly")
		}
	})

	t.Run("permission CHECK holds each new value exactly once and every old one", func(t *testing.T) {
		def := e.one(`SELECT pg_get_constraintdef(oid) FROM pg_constraint WHERE conrelid='identity.store_grants'::regclass AND conname='store_grants_permission_check'`)
		for _, p := range []string{"customers:read", "customers:privacy", "billing:manage"} {
			if n := strings.Count(def, "'"+p+"'"); n != 1 {
				t.Errorf("store_grants_permission_check holds %s %d times (want exactly 1; the re-derivation must be idempotent)", p, n)
			}
		}
		for _, p := range append(append([]string{}, op01Base...), "live:read", "live:manage", "payments:refund", "fulfillment:write", "orders:export", "integration:execute") {
			if strings.Count(def, "'"+p+"'") != 1 {
				t.Errorf("store_grants_permission_check lost or duplicated %s", p)
			}
		}
	})

	t.Run("no R3 role is referenced by 0078/0079 (C-7)", func(t *testing.T) {
		re := regexp.MustCompile(`(?is)\b(GRANT|REVOKE|CREATE\s+POLICY|ALTER\s+DEFAULT\s+PRIVILEGES)\b[^;]*?\bcommerce_(ads|capi|meta_ads|cvs|retention)[a-z_]*`)
		for _, name := range []string{"0078_customers_privacy.sql", "0079_platform_billing.sql"} {
			raw, err := os.ReadFile(filepath.Join("../../migrations", name))
			if err != nil {
				t.Fatal(err)
			}
			var stripped []string
			for _, l := range strings.Split(string(raw), "\n") {
				if i := strings.Index(l, "--"); i >= 0 && !strings.Contains(l[:i], "'") {
					l = l[:i]
				}
				stripped = append(stripped, l)
			}
			if m := re.FindString(strings.Join(stripped, "\n")); m != "" {
				t.Errorf("%s grants/policies to a role created by a later-shipping file: %q", name, m)
			}
		}
		// The release tree now also ships R3 (0074 creates commerce_ads_writer), so the role legitimately exists here.
		// C-7's real rule is where grants to it live: every GRANT/REVOKE/POLICY naming an R3 role on a customers.* or
		// billing.* object (or those schemas) must be in a migration numbered after 0079, so an R2 database that already
		// has 0078/0079 gets them only when R3 lands (migrate.go applies missing versions in lexical order).
		stmt := regexp.MustCompile(`(?is)\b(GRANT|REVOKE|CREATE\s+POLICY)\b[^;]*;`)
		r3 := regexp.MustCompile(`(?i)\bcommerce_(ads|capi|meta_ads|cvs|retention)[a-z_]*`)
		r2obj := regexp.MustCompile(`(?i)\b(customers|billing)\.|\bSCHEMA\b[^;]*\b(customers|billing)\b`)
		files, _ := filepath.Glob("../../migrations/[0-9][0-9][0-9][0-9]_*.sql")
		found := 0
		for _, path := range files {
			raw, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			var stripped []string
			for _, l := range strings.Split(string(raw), "\n") {
				if i := strings.Index(l, "--"); i >= 0 && !strings.Contains(l[:i], "'") {
					l = l[:i]
				}
				stripped = append(stripped, l)
			}
			for _, m := range stmt.FindAllString(strings.Join(stripped, "\n"), -1) {
				if r3.MatchString(m) && r2obj.MatchString(m) {
					found++
					if base := filepath.Base(path); base <= "0079_platform_billing.sql" {
						t.Errorf("%s grants an R3 role on a customers/billing object before 0080 (C-7): %q", base, m)
					}
				}
			}
		}
		if found == 0 && len(e.list(`SELECT rolname::text FROM pg_roles WHERE rolname='commerce_ads_writer'`)) == 1 {
			t.Error("commerce_ads_writer exists but no migration grants it the 0080 customers/billing rows (meta-ads-v1 §4.4)")
		}
	})

	cbsTableRules(t, e)
	cbsCreatorGrants(t, e, f)
}

// TestCustomersBillingCB02Comments is the PROCESS §5 half of the SQL rules (contract §3.1: "COMMENT ON each
// table/column/function per PROCESS §5"): every new table, column, function and role has a COMMENT that
// names its owning package. It is a separate top-level test so a documentation gap cannot hide a security
// regression in CB02Schema (nor the reverse).
func TestCustomersBillingCB02Comments(t *testing.T) {
	f := pwIsolatedFixture(t)
	e := cbsEnv{t: t, f: f}
	t.Run("every new object names its owning package", func(t *testing.T) {
		check := func(kind, name, comment, word string) {
			if len(strings.TrimSpace(comment)) < 20 || !strings.Contains(strings.ToLower(comment), word) {
				t.Errorf("%s %s: COMMENT missing, too short, or not naming %q: %q", kind, name, word, comment)
			}
		}
		for _, tb := range cbsTables {
			word := strings.SplitN(tb, ".", 2)[0]
			check("table", tb, e.one(`SELECT coalesce(obj_description($1::regclass,'pg_class'),'')`, tb), word)
			for _, c := range e.columns(tb) {
				if len(e.one(`SELECT coalesce(col_description($1::regclass,a.attnum),'') FROM pg_attribute a WHERE a.attrelid=$1::regclass AND a.attname=$2`, tb, c)) < 5 {
					t.Errorf("column %s.%s has no COMMENT", tb, c)
				}
			}
		}
		for _, fn := range cbsFunctions {
			word := strings.SplitN(fn.sig, ".", 2)[0]
			switch {
			case strings.Contains(fn.sig, "finance"):
				word = "reporting"
			case word == "identity" && strings.Contains(fn.sig, "customers"):
				word = "customers"
			case word == "identity":
				word = "billing"
			}
			check("function", fn.sig, e.one(`SELECT coalesce(obj_description($1::regprocedure,'pg_proc'),'')`, fn.sig), word)
		}
		for r, word := range map[string]string{"commerce_privacy_writer": "customers", "commerce_billing_writer": "billing"} {
			check("role", r, e.one(`SELECT coalesce(shobj_description(oid,'pg_authid'),'') FROM pg_roles WHERE rolname=$1`, r), word)
		}
	})

}

// cbsTableRules inserts one valid row per table (controls) and then every violation the contract's DDL
// forbids. Rows go through the owner pool: it bypasses grants and RLS but not CHECK/UNIQUE/FK/triggers.
func cbsTableRules(t *testing.T, e cbsEnv) {
	f := e.f
	newOwner := func() string {
		id := randomUUID()
		mustExec(t, f.owner, `INSERT INTO buyer.owners(id,tenant_id,store_id) VALUES($1,$2,$3)`, id, f.tenantA, f.storeA1)
		return id
	}
	ce := func(owner string, over map[string]string) string {
		base := map[string]string{"purpose": "'marketing_messages'", "channel": "'meta_dm'", "granted": "true", "source": "'buyer_checkout'",
			"policy_version": "'lc-2026-10'", "principal_id": "NULL", "request_key": "'" + randomUUID() + "'"}
		for k, v := range over {
			base[k] = v
		}
		var cols, vals []string
		for k, v := range base {
			cols, vals = append(cols, k), append(vals, v)
		}
		return e.state(fmt.Sprintf(`INSERT INTO customers.consent_events(tenant_id,store_id,owner_id,%s) VALUES('%s','%s','%s',%s)`,
			strings.Join(cols, ","), f.tenantA, f.storeA1, owner, strings.Join(vals, ",")))
	}
	principal := "'" + f.principalA + "'"
	t.Run("consent_events rules", func(t *testing.T) {
		o := newOwner()
		if s := ce(o, nil); s != "" {
			t.Fatalf("control insert failed: %s", s)
		}
		want := map[string]struct {
			over  map[string]string
			state string
		}{
			"unknown purpose":                {map[string]string{"purpose": "'sms'"}, "23514"},
			"marketing on the ads channel":   {map[string]string{"channel": "'meta_ads'"}, "23514"},
			"ads on the DM channel":          {map[string]string{"purpose": "'ads_personalization'"}, "23514"},
			"unknown channel":                {map[string]string{"channel": "'email'"}, "23514"},
			"unknown source":                 {map[string]string{"source": "'api'"}, "23514"},
			"merchant grant (only buyer)":    {map[string]string{"source": "'merchant_recorded'", "principal_id": principal}, "23514"},
			"erasure grant (only buyer)":     {map[string]string{"source": "'erasure'"}, "23514"},
			"merchant withdrawal, no member": {map[string]string{"source": "'merchant_recorded'", "granted": "false", "principal_id": "NULL"}, "23514"},
			"buyer row with a principal":     {map[string]string{"principal_id": principal}, "23514"},
			"principal is not a member":      {map[string]string{"source": "'merchant_recorded'", "granted": "false", "principal_id": "'" + randomUUID() + "'"}, "23503"},
			"policy version upper case":      {map[string]string{"policy_version": "'LC-2026'"}, "23514"},
			"policy version empty":           {map[string]string{"policy_version": "''"}, "23514"},
			"policy version leading dash":    {map[string]string{"policy_version": "'-a'"}, "23514"},
			"policy version 41 chars":        {map[string]string{"policy_version": "'" + strings.Repeat("a", 41) + "'"}, "23514"},
			"unknown owner":                  {nil, ""},
		}
		delete(want, "unknown owner")
		for name, c := range want {
			if s := ce(newOwner(), c.over); s != c.state {
				t.Errorf("%s: SQLSTATE %q, want %s", name, s, c.state)
			}
		}
		if s := ce(newOwner(), map[string]string{"policy_version": "'" + strings.Repeat("a", 40) + "'"}); s != "" {
			t.Errorf("40-char policy version must be accepted: %s", s)
		}
		if s := ce(newOwner(), map[string]string{"source": "'merchant_recorded'", "granted": "false", "principal_id": principal}); s != "" {
			t.Errorf("merchant withdrawal with a member principal must be accepted: %s", s)
		}
		if s := ce(randomUUID(), nil); s != "23503" {
			t.Errorf("unknown owner: %q, want 23503", s)
		}
		// one buyer/merchant request = one row (consent_one_key), whatever the pair
		k := "'" + randomUUID() + "'"
		o2 := newOwner()
		if s := ce(o2, map[string]string{"request_key": k}); s != "" {
			t.Fatal(s)
		}
		if s := ce(o2, map[string]string{"request_key": k, "purpose": "'ads_personalization'", "channel": "'meta_ads'"}); s != "23505" {
			t.Errorf("same (owner,key) for another pair must violate consent_one_key: %q", s)
		}
		if s := ce(newOwner(), map[string]string{"request_key": k}); s != "" {
			t.Errorf("the same key for another owner is independent: %q", s)
		}
		// erasure writes one row per pair under its key (UNIQUE owner,key,purpose,channel)
		o3, ek := newOwner(), "'"+randomUUID()+"'"
		er := map[string]string{"source": "'erasure'", "granted": "false", "policy_version": "'erasure'", "request_key": ek}
		if s := ce(o3, er); s != "" {
			t.Fatalf("erasure row: %s", s)
		}
		er2 := map[string]string{"source": "'erasure'", "granted": "false", "policy_version": "'erasure'", "request_key": ek, "purpose": "'ads_personalization'", "channel": "'meta_ads'"}
		if s := ce(o3, er2); s != "" {
			t.Errorf("erasure rows for two pairs under one key: %s", s)
		}
		if s := ce(o3, er); s != "23505" {
			t.Errorf("the same erasure (owner,key,pair) twice: %q, want 23505", s)
		}
	})

	pa := func(owner string, over map[string]string) string {
		base := map[string]string{"kind": "'EXPORT'", "via": "'buyer'", "principal_id": "NULL", "request_key": "'" + randomUUID() + "'", "summary": "'{}'::jsonb"}
		for k, v := range over {
			base[k] = v
		}
		var cols, vals []string
		for k, v := range base {
			cols, vals = append(cols, k), append(vals, v)
		}
		return e.state(fmt.Sprintf(`INSERT INTO customers.privacy_actions(tenant_id,store_id,owner_id,%s) VALUES('%s','%s','%s',%s)`,
			strings.Join(cols, ","), f.tenantA, f.storeA1, owner, strings.Join(vals, ",")))
	}
	t.Run("privacy_actions rules", func(t *testing.T) {
		if s := pa(newOwner(), nil); s != "" {
			t.Fatalf("control: %s", s)
		}
		for name, c := range map[string]struct {
			over  map[string]string
			state string
		}{
			"unknown kind":          {map[string]string{"kind": "'DELETE'"}, "23514"},
			"unknown via":           {map[string]string{"via": "'admin'"}, "23514"},
			"merchant needs member": {map[string]string{"via": "'merchant'"}, "23514"},
			"buyer with a member":   {map[string]string{"principal_id": principal}, "23514"},
			"restore with a member": {map[string]string{"via": "'restore'", "principal_id": principal}, "23514"},
			"summary not an object": {map[string]string{"summary": "'[]'::jsonb"}, "23514"},
			"summary over 1024":     {map[string]string{"summary": "jsonb_build_object('x',repeat('a',1100))"}, "23514"},
			"summary null":          {map[string]string{"summary": "NULL"}, "23502"},
		} {
			if s := pa(newOwner(), c.over); s != c.state {
				t.Errorf("%s: SQLSTATE %q, want %s", name, s, c.state)
			}
		}
		if s := pa(newOwner(), map[string]string{"via": "'merchant'", "principal_id": principal}); s != "" {
			t.Errorf("merchant with a member: %s", s)
		}
		if s := pa(newOwner(), map[string]string{"via": "'restore'", "kind": "'ERASURE'"}); s != "" {
			t.Errorf("restore erasure tombstone: %s", s)
		}
		o := newOwner()
		k := "'" + randomUUID() + "'"
		pa(o, map[string]string{"request_key": k})
		if s := pa(o, map[string]string{"request_key": k, "kind": "'ERASURE'"}); s != "23505" {
			t.Errorf("(owner,key) is unique across kinds: %q", s)
		}
		if s := pa(o, nil); s != "" {
			t.Errorf("many EXPORT rows per owner are allowed: %s", s)
		}
		pa(o, map[string]string{"kind": "'ERASURE'"})
		if s := pa(o, map[string]string{"kind": "'ERASURE'"}); s != "23505" {
			t.Errorf("a second ERASURE per owner (privacy_one_erasure): %q, want 23505", s)
		}
	})

	sc := func(store string, over map[string]string) string {
		base := map[string]string{"environment": "'SANDBOX'", "stripe_customer_id": "'cus_" + strings.ReplaceAll(randomUUID(), "-", "")[:20] + "'",
			"platform_account_id": "'acct_Platform0001'", "open_checkout_session_id": "NULL", "open_checkout_expires_at": "NULL"}
		for k, v := range over {
			base[k] = v
		}
		var cols, vals []string
		for k, v := range base {
			cols, vals = append(cols, k), append(vals, v)
		}
		return e.state(fmt.Sprintf(`INSERT INTO billing.store_customers(tenant_id,store_id,%s) VALUES('%s','%s',%s)`,
			strings.Join(cols, ","), f.tenantA, store, strings.Join(vals, ",")))
	}
	newStore := func() string {
		id := randomUUID()
		mustExec(t, f.owner, `INSERT INTO control.stores(tenant_id,id,name,currency) VALUES($1,$2,'cb02 store','TWD')`, f.tenantA, id)
		return id
	}
	t.Run("store_customers rules (BD1, BD8)", func(t *testing.T) {
		if s := sc(newStore(), nil); s != "" {
			t.Fatalf("control: %s", s)
		}
		for name, c := range map[string]struct {
			over  map[string]string
			state string
		}{
			"LIVE environment (BD8)":            {map[string]string{"environment": "'LIVE'"}, "23514"},
			"customer id shape":                 {map[string]string{"stripe_customer_id": "'xyz'"}, "23514"},
			"customer id empty tail":            {map[string]string{"stripe_customer_id": "'cus_'"}, "23514"},
			"platform account shape":            {map[string]string{"platform_account_id": "'bad'"}, "23514"},
			"open session id shape":             {map[string]string{"open_checkout_session_id": "'nope'", "open_checkout_expires_at": "now()"}, "23514"},
			"session id without expiry":         {map[string]string{"open_checkout_session_id": "'cs_test_x1'"}, "23514"},
			"expiry without session id":         {map[string]string{"open_checkout_expires_at": "now()"}, "23514"},
			"platform account id 60 chars tail": {map[string]string{"platform_account_id": "'acct_" + strings.Repeat("a", 60) + "'"}, "23514"},
		} {
			if s := sc(newStore(), c.over); s != c.state {
				t.Errorf("%s: SQLSTATE %q, want %s", name, s, c.state)
			}
		}
		if s := sc(newStore(), map[string]string{"open_checkout_session_id": "'cs_test_a1B2'", "open_checkout_expires_at": "now()+interval '30 min'"}); s != "" {
			t.Errorf("session id + expiry together: %s", s)
		}
		if s := sc(randomUUID(), nil); s != "23503" {
			t.Errorf("unknown store: %q, want 23503", s)
		}
		st := newStore()
		sc(st, map[string]string{"stripe_customer_id": "'cus_dupSource001'"})
		if s := sc(st, nil); s != "23505" {
			t.Errorf("one customer per (store, environment): %q, want 23505", s)
		}
		if s := sc(newStore(), map[string]string{"stripe_customer_id": "'cus_dupSource001'"}); s != "23505" {
			t.Errorf("stripe_customer_id is globally unique: %q, want 23505", s)
		}
	})

	subBase := func(store, cust string, over map[string]string) string {
		base := map[string]string{"stripe_subscription_id": "'sub_" + strings.ReplaceAll(randomUUID(), "-", "")[:20] + "'", "stripe_customer_id": "'" + cust + "'",
			"status": "'active'", "price_id": "'price_Test0001'", "current_period_start": "now()", "current_period_end": "now()+interval '30 days'",
			"cancel_at_period_end": "false", "stripe_created_at": "now()", "retrieved_at": "now()"}
		for k, v := range over {
			base[k] = v
		}
		var cols, vals []string
		for k, v := range base {
			cols, vals = append(cols, k), append(vals, v)
		}
		return e.state(fmt.Sprintf(`INSERT INTO billing.subscriptions(tenant_id,store_id,%s) VALUES('%s','%s',%s)`,
			strings.Join(cols, ","), f.tenantA, store, strings.Join(vals, ",")))
	}
	t.Run("subscriptions rules and the set-once/terminal trigger (F-B1, §3.2)", func(t *testing.T) {
		st := newStore()
		cust := "cus_Sub" + strings.ReplaceAll(randomUUID(), "-", "")[:18]
		sc(st, map[string]string{"stripe_customer_id": "'" + cust + "'"})
		if s := subBase(st, cust, nil); s != "" {
			t.Fatalf("control: %s", s)
		}
		for name, c := range map[string]struct {
			over  map[string]string
			state string
		}{
			"unknown status":            {map[string]string{"status": "'weird'"}, "23514"},
			"subscription id shape":     {map[string]string{"stripe_subscription_id": "'x'"}, "23514"},
			"price id shape":            {map[string]string{"price_id": "'p'"}, "23514"},
			"start without end":         {map[string]string{"current_period_end": "NULL"}, "23514"},
			"end without start":         {map[string]string{"current_period_start": "NULL"}, "23514"},
			"end not after start":       {map[string]string{"current_period_start": "timestamptz '2026-01-01'", "current_period_end": "timestamptz '2026-01-01'"}, "23514"},
			"cancel flag null":          {map[string]string{"cancel_at_period_end": "NULL"}, "23502"},
			"customer of another store": {map[string]string{"stripe_customer_id": "'cus_NoSuchOne1'"}, "23503"},
		} {
			if s := subBase(st, cust, c.over); s != c.state {
				t.Errorf("%s: SQLSTATE %q, want %s", name, s, c.state)
			}
		}
		for _, status := range []string{"incomplete", "incomplete_expired", "trialing", "active", "past_due", "canceled", "unpaid", "paused"} {
			if s := subBase(st, cust, map[string]string{"status": "'" + status + "'"}); s != "" {
				t.Errorf("status %s must be accepted: %s", status, s)
			}
		}
		if s := subBase(st, cust, map[string]string{"current_period_start": "NULL", "current_period_end": "NULL"}); s != "" {
			t.Errorf("a subscription without period fields (incomplete) is valid: %s", s)
		}
		// set-once columns
		sub := "sub_Once" + strings.ReplaceAll(randomUUID(), "-", "")[:16]
		subBase(st, cust, map[string]string{"stripe_subscription_id": "'" + sub + "'"})
		other := newStore()
		otherCust := "cus_Oth" + strings.ReplaceAll(randomUUID(), "-", "")[:18]
		sc(other, map[string]string{"stripe_customer_id": "'" + otherCust + "'"})
		for name, q := range map[string]string{
			"tenant_id":          fmt.Sprintf(`UPDATE billing.subscriptions SET tenant_id='%s' WHERE stripe_subscription_id='%s'`, f.tenantB, sub),
			"store_id":           fmt.Sprintf(`UPDATE billing.subscriptions SET store_id='%s',stripe_customer_id='%s' WHERE stripe_subscription_id='%s'`, other, otherCust, sub),
			"stripe_customer_id": fmt.Sprintf(`UPDATE billing.subscriptions SET stripe_customer_id='%s' WHERE stripe_subscription_id='%s'`, otherCust, sub),
			"stripe_created_at":  fmt.Sprintf(`UPDATE billing.subscriptions SET stripe_created_at=now()-interval '1 day' WHERE stripe_subscription_id='%s'`, sub),
		} {
			if s := e.state(q); s == "" {
				t.Errorf("%s is set-once: the UPDATE succeeded", name)
			}
		}
		if s := e.state(fmt.Sprintf(`UPDATE billing.subscriptions SET status='past_due',retrieved_at=now(),updated_at=now() WHERE stripe_subscription_id='%s'`, sub)); s != "" {
			t.Errorf("mutable columns update: %s", s)
		}
		if s := e.state(fmt.Sprintf(`UPDATE billing.subscriptions SET status='canceled' WHERE stripe_subscription_id='%s'`, sub)); s != "" {
			t.Errorf("active -> canceled: %s", s)
		}
		for _, to := range []string{"active", "trialing", "past_due", "unpaid", "incomplete"} {
			if s := e.state(fmt.Sprintf(`UPDATE billing.subscriptions SET status='%s' WHERE stripe_subscription_id='%s'`, to, sub)); s == "" {
				t.Errorf("canceled is terminal (F-B1): status %s was accepted", to)
			}
		}
	})
}

// cbsCreatorGrants is the OP01-shaped half of CB02 for the three new permissions (§0.1 C-5).
func cbsCreatorGrants(t *testing.T, e cbsEnv, f *testFixture) {
	ctx := context.Background()
	newPerms := []string{"customers:read", "customers:privacy", "billing:manage"}
	t.Run("a fresh creator gets the three permissions once; nobody else gains any (C-5)", func(t *testing.T) {
		role := "cbs_" + strings.ReplaceAll(randomUUID(), "-", "")
		password := randomToken()
		mustExec(t, f.owner, `CREATE ROLE `+pgx.Identifier{role}.Sanitize()+` LOGIN NOSUPERUSER NOBYPASSRLS NOCREATEDB NOCREATEROLE IN ROLE commerce_identity PASSWORD '`+password+`'`)
		u, err := url.Parse(f.databaseURL)
		if err != nil {
			t.Fatal(err)
		}
		u.User = url.UserPassword(role, password)
		pool, err := platform.OpenIdentityPool(ctx, u.String())
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(pool.Close)
		svc, err := identity.New(pool, &identityProvider{subject: randomUUID()}, identity.Policy{ProviderKey: "cbs-provider-v1", SessionTTL: time.Hour, OnboardingEnabled: true, Currencies: []string{"TWD"}})
		if err != nil {
			t.Fatal(err)
		}
		grants := func(tenant, store, principal string) []string {
			return e.list(`SELECT permission FROM identity.store_grants WHERE tenant_id=$1 AND store_id=$2 AND principal_id=$3`, tenant, store, principal)
		}
		oldBefore := grants(f.tenantA, f.storeA1, f.principalA)
		session := identityLogin(t, svc)
		key := "cbs-store-" + randomUUID()
		first, err := svc.CreateInitialStore(ctx, session.Token, key, firstStoreRequest())
		if err != nil {
			t.Fatal(err)
		}
		got := grants(first.TenantID, first.StoreID, session.PrincipalID)
		for _, p := range newPerms {
			n := 0
			for _, g := range got {
				if g == p {
					n++
				}
			}
			if n != 1 {
				t.Errorf("creator holds %s %d times, want once", p, n)
			}
		}
		if len(got) != len(op01Base)+9 {
			t.Errorf("creator holds %d grants %v, want the 0065 set plus the three (%d)", len(got), got, len(op01Base)+9)
		}
		total := func() string {
			return e.one(`SELECT count(*)::text FROM identity.store_grants WHERE tenant_id=$1`, first.TenantID)
		}
		before := total()
		if _, err := svc.CreateInitialStore(ctx, session.Token, key, firstStoreRequest()); err != nil || total() != before {
			t.Errorf("replay changed the grant count %s -> %s (%v)", before, total(), err)
		}
		mustExec(t, f.owner, `DELETE FROM identity.store_grants WHERE tenant_id=$1 AND store_id=$2 AND principal_id=$3 AND permission='billing:manage'`, first.TenantID, first.StoreID, session.PrincipalID)
		if _, err := svc.CreateInitialStore(ctx, session.Token, key, firstStoreRequest()); err != nil {
			t.Fatal(err)
		}
		for _, g := range grants(first.TenantID, first.StoreID, session.PrincipalID) {
			if g == "billing:manage" {
				t.Error("an onboarding replay restored a revoked billing:manage")
			}
		}
		other := randomUUID()
		mustExec(t, f.owner, `INSERT INTO identity.principals(id) VALUES($1)`, other)
		mustExec(t, f.owner, `INSERT INTO identity.memberships(tenant_id,principal_id) VALUES($1,$2)`, first.TenantID, other)
		mustExec(t, f.owner, `INSERT INTO identity.store_grants(tenant_id,store_id,principal_id,permission) VALUES($1,$2,$3,'store:read')`, first.TenantID, first.StoreID, other)
		if _, err := svc.CreateInitialStore(ctx, session.Token, key, firstStoreRequest()); err != nil {
			t.Fatal(err)
		}
		if g := grants(first.TenantID, first.StoreID, other); strings.Join(g, ",") != "store:read" {
			t.Errorf("another member of the tenant has %v", g)
		}
		leaked := e.one(`SELECT count(*)::text FROM identity.store_grants WHERE principal_id<>$1 AND permission IN ('customers:read','customers:privacy','billing:manage')`, session.PrincipalID)
		if leaked != "0" {
			t.Errorf("%s grants of the new permissions exist for principals other than the creator (no backfill, C-5)", leaked)
		}
		if g := grants(f.tenantA, f.storeA1, f.principalA); strings.Join(g, ",") != strings.Join(oldBefore, ",") {
			t.Errorf("a pre-existing principal's grants changed: %v -> %v", oldBefore, g)
		}
		if e.one(`SELECT count(*)::text FROM identity.store_grants WHERE principal_id=$1 AND store_id<>$2`, session.PrincipalID, first.StoreID) != "0" {
			t.Error("the creator holds grants outside the store it created")
		}
	})

	t.Run("create_initial_store body differs from its 0065 definition only by the permission array", func(t *testing.T) {
		oldBody, file := srgOldUntil(t, "identity.create_initial_store", func(base string, post bool) bool { return !post && base >= "0079_" })
		if !strings.Contains(file, "0065_") {
			t.Fatalf("the last definition before 0079 must be 0065's, got %s", file)
		}
		var newBody, owner, config string
		var definer, publicExec bool
		var grantees string
		if err := f.owner.QueryRow(ctx, `SELECT p.prosrc,pg_get_userbyid(p.proowner),p.proconfig::text,p.prosecdef,
		 EXISTS(SELECT 1 FROM aclexplode(coalesce(p.proacl,acldefault('f',p.proowner))) a WHERE a.grantee=0 AND a.privilege_type='EXECUTE'),
		 coalesce((SELECT string_agg(pg_get_userbyid(a.grantee),',' ORDER BY 1) FROM aclexplode(p.proacl) a WHERE a.privilege_type='EXECUTE' AND a.grantee<>p.proowner),'')
		 FROM pg_proc p WHERE p.oid='identity.create_initial_store(bytea,text,bytea,text,text,text,text)'::regprocedure`).Scan(&newBody, &owner, &config, &definer, &publicExec, &grantees); err != nil {
			t.Fatal(err)
		}
		literal := regexp.MustCompile(`'[a-z_]+:[a-z_]+'`)
		strip := func(s string) string {
			s = literal.ReplaceAllString(s, "")
			s = strings.NewReplacer(",", " ", "\n", " ", "\t", " ").Replace(s)
			return strings.Join(strings.Fields(s), " ")
		}
		if strip(oldBody) != strip(newBody) {
			_, _, unified := srgDiff(srgLines(strip(oldBody)), srgLines(strip(newBody)))
			t.Fatalf("the body differs beyond the permission literals (old from %s):\n%s", file, unified)
		}
		oldSet, newSet := map[string]bool{}, map[string]bool{}
		for _, l := range literal.FindAllString(oldBody, -1) {
			oldSet[strings.Trim(l, "'")] = true
		}
		for _, l := range literal.FindAllString(newBody, -1) {
			newSet[strings.Trim(l, "'")] = true
		}
		var added []string
		for l := range newSet {
			if !oldSet[l] {
				added = append(added, l)
			}
		}
		sort.Strings(added)
		cbsEq(t, "permission literals added by 0079", added, append([]string(nil), newPerms...))
		for l := range oldSet {
			if !newSet[l] {
				t.Errorf("the permission %s was dropped from the array", l)
			}
		}
		if owner != "commerce_identity_writer" || !definer || publicExec || !strings.Contains(config, "search_path=pg_catalog") || grantees != "commerce_identity" {
			t.Errorf("owner=%s definer=%v public-exec=%v config=%s grantees=%q (owner/ACL/search_path must be unchanged)", owner, definer, publicExec, config, grantees)
		}
	})
}
