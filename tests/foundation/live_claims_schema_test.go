// T10 claims schema and isolation gates (independent test_worker; contract
// contracts/live-keyword-claims-v1.md §3, §3.1-§3.3, §9 KC03/KC12, §0.1 P2(a)/(g)).
//
// Owns: KC03 (migration 0060 on a fresh and on a populated pre-0060 database, FORCE RLS,
// the §3.2 privilege matrix compared for equality, definer ownership/config/ACLs, every
// CHECK/FK in both directions, foreign-role denial, "no release without rotation" and
// the pool validator), KC12 (tenant/store isolation of buyer definers, merchant RLS,
// issue_link authority, no 42P17), P2(a) (OR-ed permissive policies cannot mix buyer and
// merchant contexts) and P2(g) (link TTL enforced by the database).
//
// Non-goals: no claims business flow beyond what the probes need (those are KC04-KC11),
// no HTTP, and no reading of internal/claims bodies. Probes that bypass Go run as the
// superuser owner, sometimes after SET LOCAL ROLE commerce_claims_writer or a role under
// test, always inside transactions that roll back, so no synthetic row survives.
package foundation_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"livecommerce/internal/buyer"
	"livecommerce/internal/claims"
	"livecommerce/internal/command"
	"livecommerce/internal/pagination"
	"livecommerce/internal/platform"
	"livecommerce/migrations"
)

var lcTables = []string{"live.offers", "live.claim_windows", "claims.bundles", "claims.lines", "claims.events", "claims.links"}

// lcProbe runs one statement under a savepoint of tx and always rolls it back, so probes
// cannot affect each other. want "" means success; otherwise the exact SQLSTATE. A
// mismatch is reported with t.Errorf so one run lists every CHECK/FK finding.
func lcProbe(t *testing.T, tx pgx.Tx, want, label, sql string, args ...any) {
	t.Helper()
	ctx := context.Background()
	if _, err := tx.Exec(ctx, `SAVEPOINT lc_probe`); err != nil {
		t.Fatalf("%s: savepoint: %v", label, err)
	}
	_, err := tx.Exec(ctx, sql, args...)
	if _, rerr := tx.Exec(ctx, `ROLLBACK TO SAVEPOINT lc_probe`); rerr != nil {
		t.Fatalf("%s: rollback to savepoint: %v", label, rerr)
	}
	if want == "" {
		if err != nil {
			t.Errorf("%s: unexpected error %v", label, err)
		}
		return
	}
	if got := sqlState(err); got != want {
		t.Errorf("%s: sqlstate %q (%v), want %s", label, got, err, want)
	}
}

func lcStrings(t *testing.T, pool *pgxpool.Pool, query string, args ...any) []string {
	t.Helper()
	rows, err := pool.Query(context.Background(), query, args...)
	if err != nil {
		t.Fatal(err)
	}
	out, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(out)
	return out
}

func lcSet(values ...string) []string {
	out := append([]string(nil), values...)
	sort.Strings(out)
	return out
}

func lcSameSet(t *testing.T, label string, got, want []string) {
	t.Helper()
	g, w := lcSet(got...), lcSet(want...)
	if strings.Join(g, "\n") != strings.Join(w, "\n") {
		missing, extra := []string{}, []string{}
		seen := map[string]bool{}
		for _, v := range g {
			seen[v] = true
		}
		for _, v := range w {
			if !seen[v] {
				missing = append(missing, v)
			}
			delete(seen, v)
		}
		for v := range seen {
			extra = append(extra, v)
		}
		sort.Strings(extra)
		t.Fatalf("%s mismatch\nmissing: %v\nextra:   %v", label, missing, extra)
	}
}

// lcDirectMerchant runs fn in a merchant transaction with app.authz_revision set the way
// IssueLink sets it (§4.2), so claims.issue_link can be called directly.
func (h *lcHarness) directMerchant(token, store string, fn func(pgx.Tx, platform.Scope) error) error {
	return h.do(token, store, func(tx pgx.Tx, s platform.Scope) error {
		if _, err := tx.Exec(h.ctx, `SELECT set_config('app.authz_revision',$1,true)`, strconv.FormatInt(s.Revision, 10)); err != nil {
			return err
		}
		return fn(tx, s)
	})
}

// lcCountRows runs a counting query in a buyer transaction (definer calls).
func (h *lcHarness) buyerCount(c buyer.Capability, query string, args ...any) (n int, err error) {
	err = buyer.WithScope(h.ctx, h.a.runtime, c.Token, c.Scope.StoreID, func(ctx context.Context, tx pgx.Tx, _ buyer.Scope) error {
		return tx.QueryRow(ctx, query, args...).Scan(&n)
	})
	return n, err
}

// lcClaimSetup opens a session in store A1 with offer A1 (sku0, max 5) and one ACCEPTED
// bundle ("A1+2") with an issued link; returns session, offer, claim and link.
func (h *lcHarness) claimSetup(t *testing.T, label string) (string, claims.Offer, claims.ManualClaimResult, claims.IssuedLink) {
	t.Helper()
	s := h.draft(t, h.f.storeA1)
	h.open(t, s, claims.MatchExact)
	o := h.offer(t, s, "A1", h.stock.skus[0].ID, 5)
	r := h.accepted(t, s, "", label, "A1+2")
	return s, o, r, h.link(t, s, r.BundleID, 0, false)
}

// TestLiveClaimsKC03Schema is KC03 (§3, §3.1, §3.2 matrix equality, §3.3 definers).
func TestLiveClaimsKC03Schema(t *testing.T) {
	h := lcSetup(t)
	f, ctx := h.f, h.ctx

	t.Run("fresh-migrated-twice", func(t *testing.T) {
		// The shared fixture ran migrations.Apply twice on a fresh database.
		if n := countRows(t, f.owner, `SELECT count(*) FROM public.lc_schema_migrations WHERE version='0060_live_claims.sql'`); n != 1 {
			t.Fatalf("0060 ledger rows=%d", n)
		}
		if err := migrations.Apply(ctx, f.owner); err != nil {
			t.Fatalf("third Apply: %v", err)
		}
		for _, table := range lcTables {
			var rls, force bool
			var comment string
			if err := f.owner.QueryRow(ctx, `SELECT relrowsecurity,relforcerowsecurity,coalesce(obj_description(oid,'pg_class'),'') FROM pg_class WHERE oid=$1::regclass`, table).Scan(&rls, &force, &comment); err != nil {
				t.Fatal(err)
			}
			if !rls || !force || !strings.Contains(comment, "internal/claims") {
				t.Fatalf("%s rls=%t force=%t comment=%q (ENABLE+FORCE RLS and COMMENT ON naming the owner package, §3/§10)", table, rls, force, comment)
			}
		}
		var login, super, bypass, createdb, createrole, replication bool
		if err := f.owner.QueryRow(ctx, `SELECT rolcanlogin,rolsuper,rolbypassrls,rolcreatedb,rolcreaterole,rolreplication FROM pg_roles WHERE rolname='commerce_claims_writer'`).Scan(&login, &super, &bypass, &createdb, &createrole, &replication); err != nil {
			t.Fatal(err)
		}
		if login || super || bypass || createdb || createrole || replication {
			t.Fatal("commerce_claims_writer must be NOLOGIN without elevated attributes")
		}
		if n := countRows(t, f.owner, `SELECT count(*) FROM pg_auth_members WHERE roleid='commerce_claims_writer'::regrole OR member='commerce_claims_writer'::regrole`); n != 0 {
			t.Fatalf("commerce_claims_writer has %d role memberships (no membership backfill, §3.2)", n)
		}
		var issuedDefault, expiresDefault *string
		if err := f.owner.QueryRow(ctx, `SELECT max(column_default) FILTER (WHERE column_name='issued_at'),max(column_default) FILTER (WHERE column_name='expires_at')
			FROM information_schema.columns WHERE table_schema='claims' AND table_name='links'`).Scan(&issuedDefault, &expiresDefault); err != nil {
			t.Fatal(err)
		}
		if issuedDefault != nil || expiresDefault != nil {
			t.Fatalf("claims.links issued_at/expires_at must have no DEFAULT (§3): %v %v", issuedDefault, expiresDefault)
		}
		// meta-claims-intake-v1 §4: claims.meta_intake joins the four T10 tables; claims-retention-purge-v1 §2
		// (§6 clause 1) adds retention_policy and retention_log.
		if n := countRows(t, f.owner, `SELECT count(*) FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace WHERE n.nspname='claims' AND c.relkind IN ('r','p') `); n != 7 {
			t.Fatalf("schema claims has %d tables, want bundles/lines/events/links/meta_intake/retention_policy/retention_log", n)
		}
	})

	t.Run("privilege-matrix", func(t *testing.T) {
		cols := func(table string, except ...string) []string {
			parts := strings.SplitN(table, ".", 2)
			all := lcStrings(t, f.owner, `SELECT column_name::text FROM information_schema.columns WHERE table_schema=$1 AND table_name=$2`, parts[0], parts[1])
			out := []string{}
			for _, c := range all {
				skip := false
				for _, e := range except {
					skip = skip || c == e
				}
				if !skip {
					out = append(out, c)
				}
			}
			return out
		}
		var want []string
		add := func(grantee, table, privilege string, columns ...string) {
			for _, c := range columns {
				want = append(want, grantee+" "+table+"."+c+" "+privilege)
			}
		}
		const rt, wr = "commerce_runtime", "commerce_claims_writer"
		add(rt, "live.offers", "SELECT", cols("live.offers")...)
		add(rt, "live.offers", "INSERT", cols("live.offers")...)
		add(rt, "live.offers", "UPDATE", "max_quantity_per_claim", "active", "activated_at", "version", "updated_at")
		add(rt, "live.claim_windows", "SELECT", cols("live.claim_windows")...)
		add(rt, "live.claim_windows", "INSERT", cols("live.claim_windows")...)
		add(rt, "live.claim_windows", "UPDATE", "state", "match_mode", "generation", "opened_at", "closed_at", "version", "principal_id", "updated_at")
		// claims-retention-purge-v1 §4 (§6 clause 1): purged_at is readable by commerce_retention_writer only.
		add(rt, "claims.bundles", "SELECT", cols("claims.bundles", "owner_id", "purged_at")...)
		add(rt, "claims.bundles", "INSERT", "tenant_id", "store_id", "session_id", "platform", "actor_key", "label")
		add(rt, "claims.bundles", "UPDATE", "line_count", "version", "updated_at")
		add(rt, "claims.lines", "SELECT", cols("claims.lines")...)
		add(rt, "claims.lines", "INSERT", cols("claims.lines", "applied_version")...)
		add(rt, "claims.lines", "UPDATE", "quantity", "version", "updated_at")
		add(rt, "claims.events", "SELECT", cols("claims.events")...)
		add(rt, "claims.events", "INSERT", cols("claims.events")...)
		add(rt, "claims.links", "SELECT", "tenant_id", "store_id", "bundle_id", "generation", "issued_at", "expires_at")
		add(wr, "claims.links", "SELECT", "tenant_id", "store_id", "bundle_id", "token_hash", "generation", "expires_at")
		add(wr, "claims.links", "INSERT", "tenant_id", "store_id", "bundle_id", "token_hash", "generation", "issued_at", "expires_at", "principal_id")
		add(wr, "claims.links", "UPDATE", "token_hash", "generation", "issued_at", "expires_at", "principal_id")
		add(wr, "claims.bundles", "SELECT", "tenant_id", "store_id", "id", "session_id", "owner_id", "bound_at", "version")
		add(wr, "claims.bundles", "UPDATE", "owner_id", "bound_at")
		add(wr, "claims.lines", "SELECT", "tenant_id", "store_id", "bundle_id", "offer_id", "sku_id", "quantity", "version", "applied_version")
		add(wr, "claims.lines", "UPDATE", "applied_version")
		add(wr, "live.offers", "SELECT", "tenant_id", "store_id", "id", "session_id", "keyword", "active")
		add(wr, "identity.sessions", "SELECT", "token_hash", "principal_id", "audience", "revoked_at", "expires_at")
		// meta-claims-intake-v1 §4.3 rows (exactly; the contract is the source, not the migration).
		const ci, iw = "commerce_claims_intake", "commerce_integration_writer"
		add(ci, "live.offers", "SELECT", cols("live.offers")...)
		add(ci, "live.offers", "UPDATE", "updated_at")
		add(ci, "live.claim_windows", "SELECT", cols("live.claim_windows")...)
		add(ci, "live.claim_windows", "UPDATE", "updated_at")
		add(ci, "claims.bundles", "SELECT", cols("claims.bundles", "owner_id", "purged_at")...)
		add(ci, "claims.bundles", "INSERT", "tenant_id", "store_id", "session_id", "platform", "actor_key", "label")
		add(ci, "claims.bundles", "UPDATE", "line_count", "version", "updated_at")
		add(ci, "claims.lines", "SELECT", cols("claims.lines")...)
		add(ci, "claims.lines", "INSERT", cols("claims.lines", "applied_version")...)
		add(ci, "claims.lines", "UPDATE", "quantity", "version", "updated_at")
		add(ci, "claims.events", "SELECT", cols("claims.events")...)
		add(ci, "claims.events", "INSERT", cols("claims.events")...)
		add(iw, "claims.events", "SELECT", "tenant_id", "store_id", "id", "session_id", "source_event_id", "outcome", "bundle_id", "bundle_version")
		add(wr, "claims.events", "SELECT", "tenant_id", "store_id", "id", "session_id", "source_kind", "source_event_id", "outcome", "bundle_id")
		add(wr, "live.claim_windows", "SELECT", "tenant_id", "store_id", "session_id", "state", "generation")
		// customers-billing-v1 §3.1 (0078): the customer projection reads counts/platform/time of bound bundles and the
		// privacy writer relabels manual labels; neither ever reads actor_key or link hashes.
		add("commerce_auth", "claims.bundles", "SELECT", "tenant_id", "store_id", "id", "session_id", "platform", "owner_id", "bound_at", "line_count")
		add("commerce_privacy_writer", "claims.bundles", "SELECT", "tenant_id", "store_id", "id", "session_id", "platform", "owner_id", "bound_at", "label", "line_count")
		add("commerce_privacy_writer", "claims.bundles", "UPDATE", "label")
		add(wr, "claims.meta_intake", "SELECT", cols("claims.meta_intake")...)
		add(wr, "claims.meta_intake", "INSERT", cols("claims.meta_intake")...)
		add(wr, "claims.meta_intake", "UPDATE", "state", "fail_code", "attempts", "not_before", "lease_xid", "updated_at")
		add(wr, "live.claim_sources", "SELECT", cols("live.claim_sources")...)
		add(wr, "live.claim_sources", "INSERT", "tenant_id", "store_id", "session_id", "platform", "binding_id", "binding_version", "object", "asset_id",
			"source_object_id", "private_reply", "reply_locale", "active", "version", "principal_id")
		add(wr, "live.claim_sources", "UPDATE", "intake_count", "intake_capped", "binding_id", "binding_version", "private_reply", "reply_locale", "active", "version", "principal_id", "updated_at")
		add(wr, "live.claim_window_intervals", "SELECT", cols("live.claim_window_intervals")...)
		add(wr, "live.claim_window_intervals", "INSERT", cols("live.claim_window_intervals")...)
		add(wr, "live.claim_window_intervals", "UPDATE", "closed_at")
		add(wr, "integration.operations", "SELECT", "id", "tenant_id", "store_id", "action", "state", "request")
		add(wr, "meta_inbox.routes", "SELECT", "tenant_id", "store_id", "object", "asset_id", "binding_id", "enabled")
		add(wr, "integration.bindings", "SELECT", "id", "tenant_id", "store_id", "provider", "external_asset_id", "semantic_version", "enabled")
		add(wr, "integration.meta_page_heads", "SELECT", "tenant_id", "store_id", "binding_id", "current_version")
		// claims-retention-purge-v1 §4 rows on the six tables (§6 clause 1; the contract is the source, CRP02 owns the rest).
		const rw = "commerce_retention_writer"
		add(rw, "claims.bundles", "SELECT", "tenant_id", "store_id", "id", "session_id", "platform", "actor_key", "label", "owner_id", "purged_at")
		add(rw, "claims.bundles", "UPDATE", "actor_key", "label", "owner_id", "bound_at", "purged_at", "updated_at")
		add(rw, "claims.lines", "SELECT", "tenant_id", "store_id", "bundle_id")
		add(rw, "claims.lines", "UPDATE", "applied_version")
		add(rw, "claims.links", "SELECT", "tenant_id", "store_id", "bundle_id", "expires_at")
		add(rw, "claims.links", "UPDATE", "expires_at")
		add(rw, "live.claim_windows", "SELECT", "tenant_id", "store_id", "session_id", "state", "closed_at")
		add(rw, "live.claim_windows", "UPDATE", "updated_at")
		got := lcStrings(t, f.owner, `SELECT DISTINCT p.grantee::text||' '||p.table_schema||'.'||p.table_name||'.'||p.column_name||' '||p.privilege_type
			FROM information_schema.column_privileges p
			JOIN pg_class c ON c.oid=format('%I.%I',p.table_schema,p.table_name)::regclass
			WHERE p.grantee::text<>pg_get_userbyid(c.relowner)
			  AND (format('%s.%s',p.table_schema,p.table_name)=ANY($1) OR p.grantee='commerce_claims_writer')`, lcTables)
		lcSameSet(t, "§3.2 column privilege matrix", got, want)
		tableWant := []string{rt + " live.offers SELECT", rt + " live.offers INSERT", rt + " live.claim_windows SELECT", rt + " live.claim_windows INSERT",
			rt + " claims.lines SELECT", rt + " claims.events SELECT", rt + " claims.events INSERT",
			// meta-claims-intake-v1 §4.3 table-level rows
			ci + " live.offers SELECT", ci + " live.claim_windows SELECT", ci + " claims.lines SELECT", ci + " claims.events SELECT", ci + " claims.events INSERT",
			wr + " claims.meta_intake SELECT", wr + " claims.meta_intake INSERT", wr + " live.claim_sources SELECT",
			wr + " live.claim_window_intervals SELECT", wr + " live.claim_window_intervals INSERT",
			// claims-retention-purge-v1 §4 (§6 clause 1): the only DELETE on the six tables
			rw + " claims.links DELETE"}
		tableGot := lcStrings(t, f.owner, `SELECT p.grantee::text||' '||p.table_schema||'.'||p.table_name||' '||p.privilege_type
			FROM information_schema.table_privileges p JOIN pg_class c ON c.oid=format('%I.%I',p.table_schema,p.table_name)::regclass
			WHERE p.grantee::text<>pg_get_userbyid(c.relowner)
			  AND (format('%s.%s',p.table_schema,p.table_name)=ANY($1) OR p.grantee='commerce_claims_writer')`, lcTables)
		lcSameSet(t, "§3.2 table-level privileges", tableGot, tableWant)
		lcSameSet(t, "commerce_claims_writer EXECUTE", lcStrings(t, f.owner, `SELECT DISTINCT routine_schema||'.'||routine_name FROM information_schema.routine_privileges WHERE grantee='commerce_claims_writer'`),
			[]string{"claims.issue_link", "claims.mark_applied", "claims.preview_link", "claims.redeem_link", "identity.resolve_access",
				// meta-claims-intake-v1 §4.3: owned definers + principal_holds
				"claims.check_meta_reply", "claims.fail_meta_intake", "claims.insert_meta_intake", "claims.intake_scope", "claims.issue_system_link",
				"claims.lease_meta_intake", "identity.principal_holds", "live.put_claim_source", "live.track_claim_window_interval"})
		lcSameSet(t, "schema claims ACL", lcStrings(t, f.owner, `SELECT CASE WHEN a.grantee=0 THEN 'PUBLIC' ELSE pg_get_userbyid(a.grantee) END||' '||a.privilege_type
			FROM pg_namespace n CROSS JOIN LATERAL aclexplode(n.nspacl) a WHERE n.nspname='claims' AND a.grantee<>n.nspowner`),
			[]string{"commerce_buyer_runtime USAGE", "commerce_claims_writer USAGE", "commerce_runtime USAGE",
				// meta-claims-intake-v1 §4.3 schema USAGE rows
				"commerce_claims_intake USAGE", "commerce_integration_writer USAGE", "commerce_meta_writer USAGE", "commerce_worker USAGE",
				// customers-billing-v1 §3.1 (0078): the customer projection and the privacy writer read/relabel bound bundles
				"commerce_auth USAGE", "commerce_privacy_writer USAGE",
				// claims-retention-purge-v1 §4 schema USAGE rows
				"commerce_retention_writer USAGE", "commerce_retention_job USAGE", "commerce_retention_operator USAGE"})
		var usage []bool
		if err := f.owner.QueryRow(ctx, `SELECT ARRAY[has_schema_privilege('commerce_claims_writer','live','USAGE'),has_schema_privilege('commerce_claims_writer','identity','USAGE'),
			has_schema_privilege('commerce_claims_writer','claims','USAGE')]`).Scan(&usage); err != nil || !usage[0] || !usage[1] || !usage[2] {
			t.Fatalf("writer schema USAGE live/identity/claims: %v %v", usage, err)
		}
		if n := countRows(t, f.owner, `SELECT count(*) FROM pg_namespace WHERE nspname NOT LIKE 'pg\_%' AND nspname<>'information_schema' AND has_schema_privilege('commerce_claims_writer',oid,'CREATE')`); n != 0 {
			t.Fatalf("writer can CREATE in %d schemas", n)
		}
		// No other commerce_* role (nor PUBLIC) holds any privilege on the six tables; the
		// buyer runtime has none on any claims.* or live.* table (§3.2).
		denied := lcStrings(t, f.owner, `SELECT r.rolname||' '||c.oid::regclass::text FROM pg_roles r CROSS JOIN pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace
			WHERE r.rolname LIKE 'commerce\_%' AND r.rolname NOT IN ('commerce_runtime','commerce_claims_writer','commerce_claims_intake','commerce_retention_writer') -- retention_writer: §4 rows asserted above
			  AND NOT (r.rolname='commerce_integration_writer' AND c.oid::regclass::text='claims.events') -- §4.3 column SELECT, asserted above
			  AND NOT (r.rolname IN ('commerce_auth','commerce_privacy_writer') AND c.oid::regclass::text='claims.bundles') -- 0078 column grants, asserted above
			  AND c.relkind IN ('r','p','v','m') AND (c.oid::regclass::text=ANY($1) OR (r.rolname='commerce_buyer_runtime' AND n.nspname IN ('claims','live')))
			  AND (has_table_privilege(r.oid,c.oid,'SELECT,INSERT,UPDATE,DELETE,TRUNCATE,REFERENCES,TRIGGER') OR has_any_column_privilege(r.oid,c.oid,'SELECT,INSERT,UPDATE,REFERENCES'))`, lcTables)
		if len(denied) != 0 {
			t.Fatalf("foreign roles hold claims table privileges: %v", denied)
		}
		if n := countRows(t, f.owner, `SELECT count(*) FROM pg_class c CROSS JOIN LATERAL aclexplode(c.relacl) a WHERE c.oid::regclass::text=ANY($1) AND a.grantee=0`, lcTables); n != 0 {
			t.Fatal("PUBLIC holds a claims table privilege")
		}
		for _, table := range lcTables {
			var del, trunc bool
			if err := f.owner.QueryRow(ctx, `SELECT has_table_privilege('commerce_runtime',$1,'DELETE'),has_table_privilege('commerce_runtime',$1,'TRUNCATE')`, table).Scan(&del, &trunc); err != nil || del || trunc {
				t.Fatalf("%s runtime DELETE=%t TRUNCATE=%t %v (no DELETE grant, §3)", table, del, trunc, err)
			}
		}
	})

	t.Run("definers", func(t *testing.T) {
		type fnRow struct{ name, args, result, owner, config, volatility, comment, acl, caller string }
		want := map[string]fnRow{
			"issue_link": {args: "p_auth_hash bytea, p_store uuid, p_session uuid, p_bundle uuid, p_expected_generation bigint, p_new_hash bytea, p_release boolean",
				result: "TABLE(generation bigint, expires_at timestamp with time zone, released boolean)", volatility: "v", acl: "commerce_claims_writer:EXECUTE,commerce_runtime:EXECUTE", caller: "commerce_runtime"},
			"mark_applied": {args: "p_bundle uuid, p_offers uuid[], p_versions bigint[]", result: "integer", volatility: "v", acl: "commerce_buyer_runtime:EXECUTE,commerce_claims_writer:EXECUTE", caller: "commerce_buyer_runtime"},
			"preview_link": {args: "p_hash bytea", result: "TABLE(bundle_version bigint, bound boolean, expires_at timestamp with time zone, offer_id uuid, keyword text, sku_id uuid, quantity integer, pending boolean, offer_active boolean)",
				volatility: "s", acl: "commerce_buyer_runtime:EXECUTE,commerce_claims_writer:EXECUTE", caller: "commerce_buyer_runtime"},
			"redeem_link": {args: "p_hash bytea, p_expected_version bigint", result: "TABLE(bundle_id uuid, bundle_version bigint, offer_id uuid, sku_id uuid, quantity integer, line_version bigint, pending boolean, offer_active boolean)",
				volatility: "v", acl: "commerce_buyer_runtime:EXECUTE,commerce_claims_writer:EXECUTE", caller: "commerce_buyer_runtime"},
			// meta-claims-intake-v1 §4.3 / §5 / §6.3: the six new claims-schema definers (all owned by commerce_claims_writer).
			"intake_scope": {args: "", result: "TABLE(tenant_id uuid, store_id uuid, session_id uuid)", volatility: "s",
				acl: "commerce_claims_intake:EXECUTE,commerce_claims_writer:EXECUTE,commerce_integration_writer:EXECUTE", caller: "commerce_claims_intake"},
			"insert_meta_intake": {args: "p_tenant uuid, p_store uuid, p_event uuid, p_received timestamp with time zone, p_app text, p_object text, p_asset text, p_object_id text, p_comment_ref text, p_actor_key text, p_occurred timestamp with time zone, p_kind text, p_keyword text, p_quantity integer, p_explicit boolean, p_live_media boolean",
				result: "uuid", volatility: "v", acl: "commerce_claims_writer:EXECUTE,commerce_meta_writer:EXECUTE", caller: "commerce_meta_writer"},
			"lease_meta_intake": {args: "", result: "SETOF claims.meta_intake", volatility: "v", acl: "commerce_claims_intake:EXECUTE,commerce_claims_writer:EXECUTE", caller: "commerce_claims_intake"},
			"fail_meta_intake":  {args: "p_intake uuid, p_code text, p_final boolean", result: "void", volatility: "v", acl: "commerce_claims_intake:EXECUTE,commerce_claims_writer:EXECUTE", caller: "commerce_claims_intake"},
			"issue_system_link": {args: "p_intake uuid, p_hash bytea", result: "timestamp with time zone", volatility: "v", acl: "commerce_claims_writer:EXECUTE,commerce_integration_writer:EXECUTE", caller: "commerce_integration_writer"},
			"check_meta_reply":  {args: "p_operation uuid, p_hash bytea", result: "text", volatility: "s", acl: "commerce_claims_writer:EXECUTE,commerce_worker:EXECUTE", caller: "commerce_worker"},
		}
		rows, err := f.owner.Query(ctx, `SELECT p.proname::text,pg_get_function_identity_arguments(p.oid),pg_get_function_result(p.oid),p.prosecdef,pg_get_userbyid(p.proowner)::text,
			coalesce(array_to_string(p.proconfig,','),''),p.provolatile::text,coalesce(obj_description(p.oid,'pg_proc'),''),
			coalesce((SELECT string_agg(x.acl,',' ORDER BY x.acl) FROM (SELECT CASE WHEN a.grantee=0 THEN 'PUBLIC' ELSE pg_get_userbyid(a.grantee)::text END||':'||a.privilege_type AS acl
				FROM aclexplode(p.proacl) a) x),'')
			FROM pg_proc p JOIN pg_namespace n ON n.oid=p.pronamespace WHERE n.nspname='claims'`)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		// claims-retention-purge-v1 §3 (§6 clause 1): the U08 functions are owned by commerce_retention_writer;
		// their shape and ACL are CRP02's, here only the exact name set and owner.
		retention := map[string]bool{"run_retention": false, "erase_actor": false, "apply_actor_erasure": false, "set_retention_policy": false,
			"retention_status": false, "replay_actor_erasures": false, "links_not_purged": false}
		seen := 0
		for rows.Next() {
			var r fnRow
			var definer bool
			if err := rows.Scan(&r.name, &r.args, &r.result, &definer, &r.owner, &r.config, &r.volatility, &r.comment, &r.acl); err != nil {
				t.Fatal(err)
			}
			if done, ok := retention[r.name]; ok && !done && r.owner == "commerce_retention_writer" {
				retention[r.name] = true
				continue
			}
			w, ok := want[r.name]
			if !ok {
				t.Fatalf("unexpected function claims.%s", r.name)
			}
			seen++
			if !definer || r.owner != "commerce_claims_writer" || r.config != "search_path=pg_catalog" || r.args != w.args || r.result != w.result ||
				r.volatility != w.volatility || r.acl != w.acl || !strings.Contains(r.comment, "internal/claims") || !strings.Contains(r.comment, w.caller) {
				t.Fatalf("claims.%s definer shape %+v (definer=%t), want %+v", r.name, r, definer, w)
			}
		}
		if rows.Err() != nil || seen != len(want) {
			t.Fatalf("claims functions seen=%d want %d err=%v", seen, len(want), rows.Err())
		}
		for fn, ok := range retention {
			if !ok {
				t.Fatalf("claims.%s missing or not owned by commerce_retention_writer (claims-retention-purge-v1 §3)", fn)
			}
		}
		// Four T10 definers + six meta-claims-intake-v1 claims definers + live.put_claim_source and the
		// live.claim_windows interval trigger function (§2, §4).
		if n := countRows(t, f.owner, `SELECT (SELECT count(*) FROM pg_proc WHERE proowner='commerce_claims_writer'::regrole)+(SELECT count(*) FROM pg_class WHERE relowner='commerce_claims_writer'::regrole)+(SELECT count(*) FROM pg_namespace WHERE nspowner='commerce_claims_writer'::regrole)`); n != 12 {
			t.Fatalf("commerce_claims_writer owns %d objects, want exactly its twelve functions", n)
		}
		denied := lcStrings(t, f.owner, `SELECT r.rolname||' '||p.proname FROM pg_roles r CROSS JOIN pg_proc p JOIN pg_namespace n ON n.oid=p.pronamespace
			WHERE n.nspname='claims' AND r.rolname LIKE 'commerce\_%' AND has_function_privilege(r.oid,p.oid,'EXECUTE')
			  AND NOT (r.rolname='commerce_claims_writer' OR (r.rolname='commerce_runtime' AND p.proname='issue_link')
			       OR (r.rolname='commerce_buyer_runtime' AND p.proname IN ('preview_link','redeem_link','mark_applied'))
			       OR (r.rolname='commerce_claims_intake' AND p.proname IN ('intake_scope','lease_meta_intake','fail_meta_intake'))
			       OR (r.rolname='commerce_integration_writer' AND p.proname IN ('intake_scope','issue_system_link'))
			       OR (r.rolname='commerce_meta_writer' AND p.proname='insert_meta_intake')
			       OR (r.rolname='commerce_worker' AND p.proname='check_meta_reply')
			       -- claims-retention-purge-v1 §4: owner, job and operator rows (§6 clause 1)
			       OR (r.rolname='commerce_retention_writer' AND p.proname IN ('run_retention','erase_actor','apply_actor_erasure','set_retention_policy','retention_status','replay_actor_erasures','links_not_purged'))
			       OR (r.rolname='commerce_retention_job' AND p.proname IN ('run_retention','retention_status'))
			       OR (r.rolname='commerce_retention_operator' AND p.proname IN ('run_retention','retention_status','erase_actor','set_retention_policy','replay_actor_erasures')))`)
		if len(denied) != 0 {
			t.Fatalf("unexpected EXECUTE on claims definers: %v", denied)
		}
	})

	t.Run("checks-and-keys", func(t *testing.T) {
		s, s2 := h.draft(t, f.storeA1), h.draft(t, f.storeA1)
		sku0, sku1 := h.stock.skus[0].ID, h.stock.skus[1].ID
		foreignSKU := lcSKUs(t, f, f.tenantA, f.storeA2, "USD", 1)[0]
		tx, err := f.owner.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback(ctx)
		exec := func(sql string, args ...any) {
			t.Helper()
			if _, err := tx.Exec(ctx, sql, args...); err != nil {
				t.Fatalf("base row: %v", err)
			}
		}
		now := time.Now().UTC().Truncate(time.Microsecond)
		a, st, actor := f.tenantA, f.storeA1, h.actor
		// live.claim_windows (probed on s2, which never keeps a window row).
		win := `INSERT INTO live.claim_windows(tenant_id,store_id,session_id,state,match_mode,generation,opened_at,closed_at,principal_id,version) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)`
		later, earlier := now.Add(time.Second), now.Add(-time.Second)
		for _, c := range []struct {
			want, state, mode string
			gen               int64
			opened, closed    any
			version           int64
		}{
			{"", "OPEN", "EXACT", 1, now, nil, 1}, {"", "CLOSED", "EXACT", 0, nil, nil, 1}, {"", "CLOSED", "KEYWORD_QTY_ONLY", 2, now, now, 1}, {"", "CLOSED", "EXACT", 2, now, later, 7},
			{"23514", "OPEN", "EXACT", 0, now, nil, 1}, {"23514", "OPEN", "EXACT", 1, nil, nil, 1}, {"23514", "OPEN", "EXACT", 1, now, later, 1},
			{"23514", "CLOSED", "EXACT", 0, now, nil, 1}, {"23514", "CLOSED", "EXACT", 0, nil, now, 1}, {"23514", "CLOSED", "EXACT", 1, nil, nil, 1},
			{"23514", "CLOSED", "EXACT", 1, now, nil, 1}, {"23514", "CLOSED", "EXACT", 1, now, earlier, 1}, {"23514", "PAUSED", "EXACT", 0, nil, nil, 1},
			{"23514", "CLOSED", "CONTAINS", 0, nil, nil, 1}, {"23514", "CLOSED", "EXACT", -1, nil, nil, 1}, {"23514", "CLOSED", "EXACT", 0, nil, nil, 0},
		} {
			lcProbe(t, tx, c.want, fmt.Sprintf("window %+v", c), win, a, st, s2, c.state, c.mode, c.gen, c.opened, c.closed, actor, c.version)
		}
		lcProbe(t, tx, "23503", "window without session", win, a, st, randomUUID(), "CLOSED", "EXACT", 0, nil, nil, actor, 1)
		lcProbe(t, tx, "23503", "window principal outside tenant", win, a, st, s2, "CLOSED", "EXACT", 0, nil, nil, randomUUID(), 1)
		exec(`INSERT INTO live.claim_windows(tenant_id,store_id,session_id,state,match_mode,generation,principal_id) VALUES($1,$2,$3,'CLOSED','EXACT',0,$4)`, a, st, s, actor)
		// live.offers.
		offerA, offerB, offerOther := randomUUID(), randomUUID(), randomUUID()
		off := `INSERT INTO live.offers(tenant_id,store_id,id,session_id,keyword,sku_id,max_quantity_per_claim,active,version,principal_id) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)`
		exec(off, a, st, offerA, s, "A1", sku0, 3, true, 1, actor)
		exec(off, a, st, offerB, s, "B2", sku1, 999, true, 1, actor)
		exec(off, a, st, offerOther, s2, "C3", sku0, 3, true, 1, actor)
		for _, c := range []struct {
			want, keyword, sku string
			max                int
			active             bool
			version            int
		}{
			{"", "P1", sku0, 1, false, 1}, {"", "P2", sku0, 999, false, 1}, {"", "ABCDEFGHIJKLMNOP", sku0, 1, false, 1},
			{"23514", "a1", sku0, 1, false, 1}, {"23514", "A1+2", sku0, 1, false, 1}, {"23514", "", sku0, 1, false, 1}, {"23514", "ABCDEFGHIJKLMNOPQ", sku0, 1, false, 1},
			{"23514", "Ａ1", sku0, 1, false, 1}, {"23514", "A 1", sku0, 1, false, 1}, {"23514", "P3", sku0, 0, false, 1}, {"23514", "P3", sku0, 1000, false, 1},
			{"23514", "P3", sku0, 1, false, 0}, {"23505", "A1", sku1, 1, false, 1}, {"23505", "P4", sku0, 1, true, 1}, {"23503", "P5", foreignSKU, 1, false, 1},
			{"23503", "P6", randomUUID(), 1, false, 1},
		} {
			lcProbe(t, tx, c.want, fmt.Sprintf("offer %+v", c), off, a, st, randomUUID(), s, c.keyword, c.sku, c.max, c.active, c.version, actor)
		}
		lcProbe(t, tx, "", "two inactive offers on one SKU (partial unique index)", `INSERT INTO live.offers(tenant_id,store_id,session_id,keyword,sku_id,max_quantity_per_claim,active,principal_id)
			VALUES($1,$2,$3,'I1',$4,1,false,$5),($1,$2,$3,'I2',$4,1,false,$5)`, a, st, s, sku0, actor)
		lcProbe(t, tx, "23503", "offer without session", off, a, st, randomUUID(), randomUUID(), "P7", sku0, 1, false, 1, actor)
		// claims.bundles.
		bundle, bundle2 := randomUUID(), randomUUID()
		actorKey := hex.EncodeToString(randomBytes(32))
		bun := `INSERT INTO claims.bundles(tenant_id,store_id,id,session_id,platform,actor_key,label,owner_id,bound_at,line_count,version) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)`
		exec(bun, a, st, bundle, s, "manual", actorKey, "amy", nil, nil, 1, 1)
		exec(bun, a, st, bundle2, s, "manual", hex.EncodeToString(randomBytes(32)), "bob", nil, nil, 0, 0)
		otherOwner := mustIssue(t, h.service, f.storeA2).Scope.OwnerID
		for _, c := range []struct {
			want, platform, key string
			label, owner, bound any
			lines, version      int
		}{
			{"", "manual", "", strings.Repeat("x", 60), nil, nil, 0, 0}, {"", "manual", "", "陳小美", h.cap.Scope.OwnerID, now, 50, 3},
			{"23514", "facebook", "", "fb", nil, nil, 0, 0}, {"23514", "manual", strings.ToUpper(hex.EncodeToString(randomBytes(32))), "u1", nil, nil, 0, 0},
			{"23514", "manual", hex.EncodeToString(randomBytes(32))[:63], "u2", nil, nil, 0, 0}, {"23514", "manual", "", nil, nil, nil, 0, 0},
			{"23514", "manual", "", " amy2", nil, nil, 0, 0}, {"23514", "manual", "", "", nil, nil, 0, 0}, {"23514", "manual", "", strings.Repeat("x", 61), nil, nil, 0, 0},
			{"23514", "manual", "", "a\x07b", nil, nil, 0, 0}, {"23514", "manual", "", "u3", nil, nil, 51, 0}, {"23514", "manual", "", "u4", nil, nil, -1, 0},
			{"23514", "manual", "", "u5", nil, nil, 0, -1}, {"23514", "manual", "", "u6", h.cap.Scope.OwnerID, nil, 0, 0}, {"23514", "manual", "", "u7", nil, now, 0, 0},
			{"23505", "manual", actorKey, "u8", nil, nil, 0, 0}, {"23505", "manual", "", "amy", nil, nil, 0, 0},
			{"23503", "manual", "", "u9", randomUUID(), now, 0, 0}, {"23503", "manual", "", "u10", otherOwner, now, 0, 0},
		} {
			key := c.key
			if key == "" {
				key = hex.EncodeToString(randomBytes(32))
			}
			lcProbe(t, tx, c.want, fmt.Sprintf("bundle %+v", c), bun, a, st, randomUUID(), s, c.platform, key, c.label, c.owner, c.bound, c.lines, c.version)
		}
		lcProbe(t, tx, "23503", "bundle of a session without window", bun, a, st, randomUUID(), s2, "manual", hex.EncodeToString(randomBytes(32)), "w1", nil, nil, 0, 0)
		// claims.lines.
		line := `INSERT INTO claims.lines(tenant_id,store_id,session_id,bundle_id,offer_id,sku_id,quantity,version,applied_version) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9)`
		exec(line, a, st, s, bundle, offerA, sku0, 1, 1, nil)
		for _, c := range []struct {
			want, bundle, offer, sku string
			qty, version             int
			applied                  any
		}{
			{"", bundle, offerB, sku1, 999, 2, 2}, {"", bundle, offerB, sku1, 1, 1, nil},
			{"23514", bundle, offerB, sku1, 0, 1, nil}, {"23514", bundle, offerB, sku1, 1000, 1, nil}, {"23514", bundle, offerB, sku1, 1, 0, nil},
			{"23514", bundle, offerB, sku1, 1, 2, 0}, {"23514", bundle, offerB, sku1, 1, 2, 3},
			{"23503", bundle, offerB, sku0, 1, 1, nil}, {"23503", bundle, offerOther, sku0, 1, 1, nil}, {"23503", randomUUID(), offerB, sku1, 1, 1, nil},
			{"23505", bundle, offerA, sku0, 2, 2, nil},
		} {
			lcProbe(t, tx, c.want, fmt.Sprintf("line %+v", c), line, a, st, s, c.bundle, c.offer, c.sku, c.qty, c.version, c.applied)
		}
		// claims.events: every §3.1 row positive, then each CHECK negated.
		var hasBundleVersion bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM information_schema.columns WHERE table_schema='claims' AND table_name='events' AND column_name='bundle_version')`).Scan(&hasBundleVersion); err != nil {
			t.Fatal(err)
		}
		type ev map[string]any
		event := func(e ev) (string, []any) {
			row := ev{"tenant_id": a, "store_id": st, "session_id": s, "window_generation": 1, "source_kind": "manual", "source_event_id": randomUUID(), "platform": "manual",
				"occurred_at": time.Now(), "grammar_version": "kw-v1", "match_mode": "EXACT", "principal_id": actor}
			for k, v := range e {
				row[k] = v
			}
			if _, set := e["bundle_version"]; hasBundleVersion && !set && row["outcome"] == "ACCEPTED" {
				row["bundle_version"] = 1 // §0.1 P2(c) persisted column: required iff ACCEPTED
			}
			names := make([]string, 0, len(row))
			for k := range row {
				names = append(names, k)
			}
			sort.Strings(names)
			args, marks := make([]any, len(names)), make([]string, len(names))
			for i, n := range names {
				args[i], marks[i] = row[n], "$"+strconv.Itoa(i+1)
			}
			return `INSERT INTO claims.events(` + strings.Join(names, ",") + `) VALUES(` + strings.Join(marks, ",") + `)`, args
		}
		probeEvent := func(want, label string, e ev) {
			t.Helper()
			sql, args := event(e)
			lcProbe(t, tx, want, "event "+label, sql, args...)
		}
		rej := func(kind, reason string, extra ev) ev {
			e := ev{"grammar_kind": kind, "outcome": "REJECTED", "reason": reason}
			for k, v := range extra {
				e[k] = v
			}
			return e
		}
		acc := func(extra ev) ev {
			e := ev{"grammar_kind": "MATCH", "outcome": "ACCEPTED", "offer_id": offerA, "quantity": 1, "explicit_quantity": false, "bundle_id": bundle, "line_version": 1}
			for k, v := range extra {
				e[k] = v
			}
			return e
		}
		withOffer := func(q any, explicit any) ev {
			return ev{"offer_id": offerA, "quantity": q, "explicit_quantity": explicit}
		}
		for label, e := range map[string]ev{
			"NO_MATCH":                    rej("NO_MATCH", "NO_MATCH", nil),
			"UNKNOWN_KEYWORD/MATCH":       rej("MATCH", "UNKNOWN_KEYWORD", nil),
			"UNKNOWN_KEYWORD/INVALID":     rej("INVALID_QUANTITY", "UNKNOWN_KEYWORD", nil),
			"OFFER_INACTIVE/MATCH":        rej("MATCH", "OFFER_INACTIVE", withOffer(1, false)),
			"OFFER_INACTIVE/INVALID":      rej("INVALID_QUANTITY", "OFFER_INACTIVE", ev{"offer_id": offerA}),
			"INVALID_QUANTITY":            rej("INVALID_QUANTITY", "INVALID_QUANTITY", ev{"offer_id": offerA}),
			"QUANTITY_REQUIRED":           rej("MATCH", "QUANTITY_REQUIRED", ev{"offer_id": offerA, "quantity": 1, "explicit_quantity": false, "match_mode": "KEYWORD_QTY_ONLY"}),
			"QUANTITY_OVER_MAX":           rej("MATCH", "QUANTITY_OVER_MAX", withOffer(5, true)),
			"BUNDLE_LIMIT":                rej("MATCH", "BUNDLE_LIMIT", withOffer(2, true)),
			"ACCEPTED new line":           acc(nil),
			"ACCEPTED update":             acc(ev{"quantity": 3, "explicit_quantity": true, "line_version": 2, "previous_quantity": 1}),
			"ACCEPTED KEYWORD_QTY_ONLY":   acc(ev{"quantity": 999, "explicit_quantity": true, "match_mode": "KEYWORD_QTY_ONLY"}),
			"occurred 119s in the future": rej("NO_MATCH", "NO_MATCH", ev{"occurred_at": time.Now().Add(110 * time.Second)}),
		} {
			probeEvent("", label, e)
		}
		for label, e := range map[string]ev{
			"NO_MATCH reason, MATCH kind":            rej("MATCH", "NO_MATCH", nil),
			"NO_MATCH kind, UNKNOWN reason":          rej("NO_MATCH", "UNKNOWN_KEYWORD", nil),
			"NO_MATCH kind accepted":                 acc(ev{"grammar_kind": "NO_MATCH"}),
			"NO_MATCH with offer":                    rej("NO_MATCH", "NO_MATCH", ev{"offer_id": offerA}),
			"UNKNOWN_KEYWORD with offer":             rej("MATCH", "UNKNOWN_KEYWORD", withOffer(1, false)),
			"UNKNOWN_KEYWORD with quantity":          rej("MATCH", "UNKNOWN_KEYWORD", ev{"quantity": 1, "explicit_quantity": false}),
			"OFFER_INACTIVE without offer":           rej("MATCH", "OFFER_INACTIVE", nil),
			"OFFER_INACTIVE/MATCH without quantity":  rej("MATCH", "OFFER_INACTIVE", ev{"offer_id": offerA}),
			"OFFER_INACTIVE/INVALID with quantity":   rej("INVALID_QUANTITY", "OFFER_INACTIVE", withOffer(1, false)),
			"INVALID_QUANTITY reason, MATCH kind":    rej("MATCH", "INVALID_QUANTITY", withOffer(1, false)),
			"INVALID_QUANTITY with quantity":         rej("INVALID_QUANTITY", "INVALID_QUANTITY", withOffer(1, true)),
			"INVALID_QUANTITY kind, OVER_MAX reason": rej("INVALID_QUANTITY", "QUANTITY_OVER_MAX", ev{"offer_id": offerA}),
			"INVALID_QUANTITY kind, BUNDLE_LIMIT":    rej("INVALID_QUANTITY", "BUNDLE_LIMIT", ev{"offer_id": offerA}),
			"QUANTITY_REQUIRED in EXACT":             rej("MATCH", "QUANTITY_REQUIRED", withOffer(1, false)),
			"QUANTITY_REQUIRED explicit":             rej("MATCH", "QUANTITY_REQUIRED", ev{"offer_id": offerA, "quantity": 1, "explicit_quantity": true, "match_mode": "KEYWORD_QTY_ONLY"}),
			"quantity without explicit":              rej("MATCH", "QUANTITY_OVER_MAX", withOffer(5, nil)),
			"explicit without quantity":              rej("INVALID_QUANTITY", "INVALID_QUANTITY", ev{"offer_id": offerA, "explicit_quantity": true}),
			"ACCEPTED with reason":                   acc(ev{"reason": "QUANTITY_OVER_MAX"}),
			"ACCEPTED without bundle":                acc(ev{"bundle_id": nil}),
			"ACCEPTED without line_version":          acc(ev{"line_version": nil}),
			"ACCEPTED without quantity":              acc(ev{"quantity": nil, "explicit_quantity": nil}),
			"ACCEPTED without offer":                 acc(ev{"offer_id": nil, "quantity": nil, "explicit_quantity": nil}),
			"REJECTED with bundle":                   rej("MATCH", "QUANTITY_OVER_MAX", ev{"offer_id": offerA, "quantity": 5, "explicit_quantity": true, "bundle_id": bundle}),
			"REJECTED with line_version":             rej("MATCH", "QUANTITY_OVER_MAX", ev{"offer_id": offerA, "quantity": 5, "explicit_quantity": true, "line_version": 1}),
			"REJECTED with previous":                 rej("MATCH", "QUANTITY_OVER_MAX", ev{"offer_id": offerA, "quantity": 5, "explicit_quantity": true, "previous_quantity": 1}),
			"REJECTED without reason":                rej("MATCH", "", ev{"reason": nil, "offer_id": offerA, "quantity": 5, "explicit_quantity": true}),
			"source meta":                            rej("NO_MATCH", "NO_MATCH", ev{"source_kind": "meta"}),
			"platform facebook":                      rej("NO_MATCH", "NO_MATCH", ev{"platform": "facebook"}),
			"grammar kw-v2":                          rej("NO_MATCH", "NO_MATCH", ev{"grammar_version": "kw-v2"}),
			"manual without principal":               rej("NO_MATCH", "NO_MATCH", ev{"principal_id": nil}),
			"occurred beyond +120s":                  rej("NO_MATCH", "NO_MATCH", ev{"occurred_at": time.Now().Add(130 * time.Second)}),
			"generation 0":                           rej("NO_MATCH", "NO_MATCH", ev{"window_generation": 0}),
			"unknown reason":                         rej("MATCH", "CONTAINS", nil),
			"unknown outcome":                        ev{"grammar_kind": "NO_MATCH", "outcome": "PENDING", "reason": "NO_MATCH"},
			"unknown kind":                           rej("PARTIAL", "NO_MATCH", nil),
			"unknown mode":                           rej("NO_MATCH", "NO_MATCH", ev{"match_mode": "CONTAINS"}),
			"quantity 0":                             rej("MATCH", "QUANTITY_OVER_MAX", withOffer(0, true)),
			"quantity 1000":                          rej("MATCH", "QUANTITY_OVER_MAX", withOffer(1000, true)),
			"line_version 0":                         acc(ev{"line_version": 0}),
			"previous 1000":                          acc(ev{"line_version": 2, "previous_quantity": 1000}),
		} {
			probeEvent("23514", label, e)
		}
		if hasBundleVersion {
			probeEvent("23514", "bundle_version on REJECTED", rej("MATCH", "QUANTITY_OVER_MAX", ev{"offer_id": offerA, "quantity": 5, "explicit_quantity": true, "bundle_version": 1}))
			probeEvent("23514", "ACCEPTED without bundle_version", acc(ev{"bundle_version": nil}))
		}
		probeEvent("23503", "offer of another session", rej("MATCH", "QUANTITY_OVER_MAX", ev{"offer_id": offerOther, "quantity": 5, "explicit_quantity": true}))
		probeEvent("23503", "ACCEPTED without its line", acc(ev{"offer_id": offerB}))
		probeEvent("23503", "session without window", rej("NO_MATCH", "NO_MATCH", ev{"session_id": s2}))
		probeEvent("23503", "principal outside tenant", rej("NO_MATCH", "NO_MATCH", ev{"principal_id": randomUUID()}))
		source := randomUUID()
		sql, args := event(rej("NO_MATCH", "NO_MATCH", ev{"source_event_id": source}))
		exec(sql, args...)
		probeEvent("23505", "duplicate source_event_id", rej("MATCH", "UNKNOWN_KEYWORD", ev{"source_event_id": source}))
		sql, args = event(acc(nil))
		exec(sql, args...)
		probeEvent("23505", "duplicate ACCEPTED line_version", acc(ev{"quantity": 2, "explicit_quantity": true}))
		probeEvent("", "REJECTED rows share no line_version index", rej("MATCH", "QUANTITY_OVER_MAX", withOffer(5, true)))
		// claims.links: exact 72 h TTL window in the database (§3, P2(g)).
		link := `INSERT INTO claims.links(tenant_id,store_id,bundle_id,token_hash,generation,issued_at,expires_at,principal_id) VALUES($1,$2,$3,$4,$5,$6,$7,$8)`
		hash := randomBytes(32)
		for _, c := range []struct {
			want    string
			hash    []byte
			gen     int
			expires time.Time
			bundle  string
			member  string
		}{
			{"", randomBytes(32), 1, now.Add(72 * time.Hour), bundle, actor}, {"", randomBytes(32), 9, now.Add(time.Microsecond), bundle, actor},
			{"23514", randomBytes(31), 1, now.Add(time.Hour), bundle, actor}, {"23514", randomBytes(33), 1, now.Add(time.Hour), bundle, actor},
			{"23514", randomBytes(32), 0, now.Add(time.Hour), bundle, actor}, {"23514", randomBytes(32), 1, now, bundle, actor},
			{"23514", randomBytes(32), 1, now.Add(-time.Second), bundle, actor}, {"23514", randomBytes(32), 1, now.Add(72*time.Hour + time.Microsecond), bundle, actor},
			{"23503", randomBytes(32), 1, now.Add(time.Hour), randomUUID(), actor}, {"23503", randomBytes(32), 1, now.Add(time.Hour), bundle, randomUUID()},
		} {
			lcProbe(t, tx, c.want, fmt.Sprintf("link gen=%d len=%d ttl=%v", c.gen, len(c.hash), c.expires.Sub(now)), link, a, st, c.bundle, c.hash, c.gen, now, c.expires, c.member)
		}
		lcProbe(t, tx, "23502", "link without issued_at (no DEFAULT)", `INSERT INTO claims.links(tenant_id,store_id,bundle_id,token_hash,generation,expires_at,principal_id) VALUES($1,$2,$3,$4,1,$5,$6)`,
			a, st, bundle, randomBytes(32), now.Add(time.Hour), actor)
		exec(link, a, st, bundle, hash, 1, now, now.Add(72*time.Hour), actor)
		lcProbe(t, tx, "23505", "token_hash reused by another bundle", link, a, st, bundle2, hash, 1, now, now.Add(time.Hour), actor)
		lcProbe(t, tx, "23505", "second link row for one bundle", link, a, st, bundle, randomBytes(32), 2, now, now.Add(time.Hour), actor)
		lcProbe(t, tx, "23514", "rotation beyond 72h", `UPDATE claims.links SET expires_at=issued_at+interval '72 hours 1 microsecond' WHERE bundle_id=$1`, bundle)
		lcProbe(t, tx, "", "rotation exactly 72h", `UPDATE claims.links SET issued_at=$2,expires_at=$2::timestamptz+interval '72 hours' WHERE bundle_id=$1`, bundle, now.Add(time.Minute))
	})

	t.Run("foreign-roles-denied", func(t *testing.T) {
		roles := []string{"commerce_buyer_runtime", "commerce_buyer_issuer", "commerce_buyer_writer", "commerce_identity", "commerce_identity_writer", "commerce_auth",
			"commerce_worker", "commerce_checkout_runtime", "commerce_checkout_writer", "commerce_hosted_runtime", "commerce_integration_writer", "commerce_inventory_writer",
			"commerce_meta_ingress", "commerce_meta_registrar", "commerce_meta_curator", "commerce_meta_consumer", "commerce_meta_writer", "commerce_meta_worker",
			"commerce_media_registrar", "commerce_media_writer", "commerce_media_worker", "commerce_media_executor", "commerce_media_recovery"}
		for _, role := range roles {
			for _, table := range lcTables {
				for _, q := range []string{`SELECT 1 FROM ` + table + ` LIMIT 1`, `INSERT INTO ` + table + ` DEFAULT VALUES`, `UPDATE ` + table + ` SET tenant_id=tenant_id`, `DELETE FROM ` + table} {
					// The single exemption of meta-claims-intake-v1 §4.4 clause 10: commerce_integration_writer holds
					// column SELECT on claims.events (§4.3), so `SELECT 1 FROM claims.events` succeeds. Every other
					// role/table/statement pair stays 42501.
					if role == "commerce_integration_writer" && table == "claims.events" && strings.HasPrefix(q, "SELECT") {
						continue
					}
					// customers-billing-v1 §3.1 (0078): commerce_auth holds column SELECT on claims.bundles (customer list
					// claims_count/platforms), so `SELECT 1 FROM claims.bundles` succeeds; its write statements stay 42501 and
					// the matrix above pins the exact columns (no actor_key, label or link hash).
					if role == "commerce_auth" && table == "claims.bundles" && strings.HasPrefix(q, "SELECT") {
						continue
					}
					tx, err := f.owner.Begin(ctx)
					if err != nil {
						t.Fatal(err)
					}
					_, err = tx.Exec(ctx, `SET LOCAL ROLE `+pgx.Identifier{role}.Sanitize())
					if err == nil {
						_, err = tx.Exec(ctx, q)
					}
					_ = tx.Rollback(ctx)
					if sqlState(err) != "42501" {
						t.Fatalf("%s: %q sqlstate %q (%v), want 42501", role, q, sqlState(err), err)
					}
				}
			}
		}
	})

	t.Run("release-only-with-rotation", func(t *testing.T) {
		// Catalog enumeration: every function a runtime or buyer-runtime login can execute
		// that references claims.bundles or claims.links is one of the four definers.
		lcSameSet(t, "functions reaching claims bindings/links", lcStrings(t, f.owner, `SELECT p.oid::regprocedure::text FROM pg_proc p JOIN pg_namespace n ON n.oid=p.pronamespace
			WHERE n.nspname NOT IN ('pg_catalog','information_schema') AND p.prosrc ~* 'claims\.(bundles|links)'
			  AND (has_function_privilege('commerce_runtime',p.oid,'EXECUTE') OR has_function_privilege('commerce_buyer_runtime',p.oid,'EXECUTE'))`),
			[]string{"claims.issue_link(bytea,uuid,uuid,uuid,bigint,bytea,boolean)", "claims.mark_applied(uuid,uuid[],bigint[])", "claims.preview_link(bytea)", "claims.redeem_link(bytea,bigint)",
				// customers-billing-v1 §3.1 (0078): read-only projections of bound-bundle counts/time, no binding write.
				"identity.read_merchant_customers(bytea,uuid,uuid,integer,timestamp with time zone,uuid,text)", "customers.buyer_read_privacy(bytea,uuid,boolean)"})
		lcSameSet(t, "roles able to write owner_id", lcStrings(t, f.owner, `SELECT DISTINCT p.grantee::text FROM information_schema.column_privileges p
			WHERE p.table_schema='claims' AND p.table_name='bundles' AND p.column_name IN ('owner_id','bound_at') AND p.privilege_type='UPDATE'
			  AND p.grantee::text<>(SELECT pg_get_userbyid(relowner) FROM pg_class WHERE oid='claims.bundles'::regclass)`),
			// claims-retention-purge-v1 §6 clause 2: U08 clears owner_id only with the link delete + purged_at (RD8).
			[]string{"commerce_claims_writer", "commerce_retention_writer"})
		// Direct call of each definer on a bound bundle.
		s, o, r, l := h.claimSetup(t, "rotation-proof")
		owner1, owner2 := h.cap, mustIssue(t, h.service, f.storeA1)
		red, err := h.redeem(owner1, t04Key("lc-redeem"), l.Token, r.BundleVersion)
		if err != nil || lcOwner(t, f, r.BundleID) != owner1.Scope.OwnerID {
			t.Fatalf("bind: %+v %v", red, err)
		}
		hash := tokenHash(string(l.Token))
		for _, q := range []struct {
			c     buyer.Capability
			query string
			args  []any
			want  int
		}{
			{owner1, `SELECT count(*) FROM claims.preview_link($1)`, []any{hash}, 1},
			{owner1, `SELECT count(*) FROM claims.redeem_link($1,$2)`, []any{hash, r.BundleVersion}, 1},
			{owner1, `SELECT claims.mark_applied($1,ARRAY[$2]::uuid[],ARRAY[$3]::bigint[])`, []any{r.BundleID, o.ID, r.LineVersion}, 1},
			{owner2, `SELECT count(*) FROM claims.redeem_link($1,$2)`, []any{hash, r.BundleVersion}, 0},
			{owner2, `SELECT count(*) FROM claims.preview_link($1)`, []any{hash}, 0},
		} {
			if n, err := h.buyerCount(q.c, q.query, q.args...); err != nil || n != q.want {
				t.Fatalf("%s: n=%d err=%v want %d", q.query, n, err, q.want)
			}
			if lcOwner(t, f, r.BundleID) != owner1.Scope.OwnerID || !bytesEqual(lcLinkState(t, f, r.BundleID).hash, hash) {
				t.Fatalf("%s changed the binding or the token hash", q.query)
			}
		}
		for _, q := range []string{`SELECT count(*) FROM claims.preview_link($1)`, `SELECT count(*) FROM claims.redeem_link($1,1)`} {
			err := h.directMerchant(h.token, f.storeA1, func(tx pgx.Tx, _ platform.Scope) error { var n int; return tx.QueryRow(h.ctx, q, hash).Scan(&n) })
			requirePGCode(t, err, "42501", "runtime "+q)
		}
		err = buyer.WithScope(h.ctx, h.a.runtime, owner1.Token, f.storeA1, func(ctx context.Context, tx pgx.Tx, _ buyer.Scope) error {
			_, e := tx.Exec(ctx, `SELECT * FROM claims.issue_link($1,$2,$3,$4,1,$5,true)`, tokenHash(h.token), f.storeA1, s, r.BundleID, randomBytes(32))
			return e
		})
		requirePGCode(t, err, "42501", "buyer issue_link")
		issue := func(generation int64, release bool) ([]byte, bool) {
			t.Helper()
			next := randomBytes(32)
			var gen int64
			var released bool
			err := h.directMerchant(h.token, f.storeA1, func(tx pgx.Tx, _ platform.Scope) error {
				return tx.QueryRow(h.ctx, `SELECT generation,released FROM claims.issue_link($1,$2,$3,$4,$5,$6,$7)`, tokenHash(h.token), f.storeA1, s, r.BundleID, generation, next, release).Scan(&gen, &released)
			})
			if err != nil || gen != generation+1 {
				t.Fatalf("direct issue_link gen=%d release=%t: %d %v", generation, release, gen, err)
			}
			return next, released
		}
		rotated, released := issue(1, false)
		if released || lcOwner(t, f, r.BundleID) != owner1.Scope.OwnerID || !bytesEqual(lcLinkState(t, f, r.BundleID).hash, rotated) {
			t.Fatal("rotation without release must keep the binding and replace the hash")
		}
		releasedHash, released := issue(2, true)
		if !released || lcOwner(t, f, r.BundleID) != "" || !bytesEqual(lcLinkState(t, f, r.BundleID).hash, releasedHash) || bytesEqual(releasedHash, rotated) {
			t.Fatal("release must clear the binding and replace the hash in the same call")
		}
		if n := countRows(t, f.owner, `SELECT count(*) FROM claims.lines WHERE bundle_id=$1 AND applied_version IS NOT NULL`, r.BundleID); n != 0 {
			t.Fatalf("release must reset applied state, %d lines still applied", n)
		}
	})

	t.Run("pool-validator", func(t *testing.T) {
		login := func(inRoles string, extra string) string {
			role := "lc_reach_" + hex.EncodeToString(randomBytes(5))
			password := hex.EncodeToString(randomBytes(24))
			mustExec(t, f.owner, fmt.Sprintf(`CREATE ROLE %s LOGIN NOSUPERUSER NOBYPASSRLS NOCREATEDB NOCREATEROLE NOREPLICATION IN ROLE %s PASSWORD '%s'`, role, inRoles, password))
			if extra != "" {
				mustExec(t, f.owner, fmt.Sprintf(extra, role))
			}
			t.Cleanup(func() { mustExec(t, f.owner, `DROP ROLE `+role) })
			return roleURL(t, f.databaseURL, role, password)
		}
		pool, err := platform.OpenPool(ctx, login("commerce_runtime", ""))
		if err != nil {
			t.Fatalf("positive control: plain runtime login rejected: %v", err)
		}
		pool.Close()
		assertPoolRejected(t, "runtime+claims writer", platform.OpenPool, login("commerce_runtime,commerce_claims_writer", ""))
		assertPoolRejected(t, "runtime SET claims writer", platform.OpenPool, login("commerce_runtime", "GRANT commerce_claims_writer TO %s WITH INHERIT FALSE, SET TRUE"))
		assertPoolRejected(t, "buyer+claims writer", platform.OpenBuyerPool, login("commerce_buyer_runtime,commerce_claims_writer", ""))
		assertPoolRejected(t, "buyer SET claims writer", platform.OpenBuyerPool, login("commerce_buyer_runtime", "GRANT commerce_claims_writer TO %s WITH INHERIT FALSE, SET TRUE"))
	})

	t.Run("populated-pre-0060-upgrade", func(t *testing.T) {
		lcPopulatedUpgrade(t)
	})
}

func bytesEqual(a, b []byte) bool { return hex.EncodeToString(a) == hex.EncodeToString(b) }

// lcPopulatedUpgrade starts a separate labelled PG (pinned image, never pulled), applies
// every migration except 0060 through the real runner (0060 is pre-registered in the
// ledger with its true checksum, then un-registered), populates live/catalog/identity
// rows, and then applies 0060 twice on top of that data.
func lcPopulatedUpgrade(t *testing.T) {
	fixture(t) // same explicit disposable-PG consent gate as every foundation test
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Second)
	defer cancel()
	name := "lc-claims-upgrade-" + t04Tag()
	password := hex.EncodeToString(randomBytes(24))
	if out, err := exec.CommandContext(ctx, "docker", "run", "-d", "--pull=never", "--name", name, "--label", "livecommerce.fixture="+name,
		"--memory=1g", "--cpus=1", "--pids-limit=128", "--tmpfs", "/var/lib/postgresql:rw,size=268435456", "-e", "POSTGRES_PASSWORD="+password,
		"-e", "POSTGRES_DB=lc_foundation_test", "-p", "127.0.0.1::5432", "postgres@sha256:4ef4dbc939d61acea57712655ddb4b4ab27419c913f94cca0cd57cb3ea3c2280",
		"-c", "shared_buffers=32MB", "-c", "max_connections=60").CombinedOutput(); err != nil {
		t.Fatalf("start labelled upgrade PG: %v %s", err, out)
	}
	t.Cleanup(func() {
		cleanup, stop := context.WithTimeout(context.Background(), 15*time.Second)
		defer stop()
		label, err := exec.CommandContext(cleanup, "docker", "inspect", "-f", `{{index .Config.Labels "livecommerce.fixture"}}`, name).Output()
		if err != nil || strings.TrimSpace(string(label)) != name {
			t.Errorf("refusing unverified PG cleanup %s: %v", name, err)
			return
		}
		if out, err := exec.CommandContext(cleanup, "docker", "rm", "-f", name).CombinedOutput(); err != nil {
			t.Errorf("remove upgrade PG %s: %v %s", name, err, out)
		}
	})
	portOut, err := exec.CommandContext(ctx, "docker", "port", name, "5432/tcp").Output()
	if err != nil || !strings.HasPrefix(strings.TrimSpace(string(portOut)), "127.0.0.1:") {
		t.Fatal("upgrade PG did not bind loopback")
	}
	// password is random per run (hex of randomBytes above), never a stored secret.
	u := &url.URL{Scheme: "postgres", User: url.UserPassword("postgres", password), Host: strings.TrimSpace(string(portOut)), Path: "/lc_foundation_test", RawQuery: "sslmode=disable"} // ggignore
	owner, err := pgxpool.New(ctx, u.String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(owner.Close)
	ready := false
	for i := 0; i < 100 && !ready; i++ {
		probe, stop := context.WithTimeout(ctx, 500*time.Millisecond)
		var one int
		ready = owner.QueryRow(probe, `SELECT 1`).Scan(&one) == nil
		stop()
		if !ready {
			time.Sleep(200 * time.Millisecond)
		}
	}
	if !ready {
		t.Fatal("upgrade PG did not become ready")
	}
	body, err := os.ReadFile(filepath.Join("../../migrations", "0060_live_claims.sql"))
	if err != nil {
		t.Fatal(err)
	}
	mustExec(t, owner, `CREATE TABLE public.lc_schema_migrations (version text PRIMARY KEY, checksum text NOT NULL, applied_at timestamptz NOT NULL DEFAULT now())`)
	mustExec(t, owner, `INSERT INTO public.lc_schema_migrations(version,checksum) VALUES('0060_live_claims.sql',$1)`, fmt.Sprintf("%x", sha256.Sum256(body)))
	// 0064 and post-River 0014 (meta-claims-intake-v1) build on 0060's tables and roles, so they are
	// held back with it and applied on top of the populated data in the second phase below. The meta-ads
	// migrations (0074/0075, post-River 0015) build on 0064's Page-token custody tables and 0074's role, so they
	// are held back too (unit ads-core; nothing else about this gate changes).
	// 0078 (customers-billing-v1) reads claims.bundles and 0079 reads live.claim_windows / claim_window_intervals,
	// so both are held back with 0060 as well (0079 after 0078: it needs 0078's permission values). 0080 (ads-capi)
	// builds on 0074 and 0078/0079, so it is held back with them.
	dependents := []string{"0064_meta_claims_intake.sql", "0074_meta_ads.sql", "0075_meta_ads_insights.sql",
		"0078_customers_privacy.sql", "0079_platform_billing.sql", "0080_meta_capi.sql",
		"post_river/0014_meta_claims_intake_river.sql", "post_river/0015_meta_ads_river.sql",
		// 0071 (claims-retention-purge-v1) requires 0060+0064 (55000 precondition), so it is held back as well.
		"0071_claims_retention.sql"}
	for _, version := range dependents {
		dependent, err := os.ReadFile(filepath.Join("../../migrations", version))
		if err != nil {
			t.Fatal(err)
		}
		mustExec(t, owner, `INSERT INTO public.lc_schema_migrations(version,checksum) VALUES($1,$2)`, version, fmt.Sprintf("%x", sha256.Sum256(dependent)))
	}
	if err := migrations.Apply(ctx, owner); err != nil {
		t.Fatalf("apply everything before 0060: %v", err)
	}
	if n := countRows(t, owner, `SELECT count(*) FROM pg_namespace WHERE nspname='claims'`) + countRows(t, owner, `SELECT count(*) FROM pg_class WHERE oid=to_regclass('live.offers')`); n != 0 {
		t.Fatal("pre-0060 database already has claims objects")
	}
	tenant, store, principal, session, product, sku := randomUUID(), randomUUID(), randomUUID(), randomUUID(), randomUUID(), randomUUID()
	for _, q := range []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO control.tenants(id,name) VALUES($1,'upgrade-tenant')`, []any{tenant}},
		{`INSERT INTO control.stores(tenant_id,id,name,currency) VALUES($1,$2,'upgrade-store','USD')`, []any{tenant, store}},
		{`INSERT INTO identity.principals(id) VALUES($1)`, []any{principal}},
		{`INSERT INTO identity.memberships(tenant_id,principal_id) VALUES($1,$2)`, []any{tenant, principal}},
		{`INSERT INTO identity.store_grants(tenant_id,store_id,principal_id,permission) VALUES($1,$2,$3,'live:manage')`, []any{tenant, store, principal}},
		{`INSERT INTO live.sessions(id,tenant_id,store_id,principal_id,title) VALUES($1,$2,$3,$4,'pre-0060 session')`, []any{session, tenant, store, principal}},
		{`INSERT INTO live.programs(tenant_id,store_id,session_id,principal_id,aspect_ratio) VALUES($1,$2,$3,$4,'9:16')`, []any{tenant, store, session, principal}},
		{`INSERT INTO catalog.products(tenant_id,store_id,id,name) VALUES($1,$2,$3,'pre-0060 product')`, []any{tenant, store, product}},
		{`INSERT INTO catalog.skus(tenant_id,store_id,id,product_id,code,currency,price_minor) VALUES($1,$2,$3,$4,'PRE-0060','USD',100)`, []any{tenant, store, sku, product}},
	} {
		mustExec(t, owner, q.sql, q.args...)
	}
	before := map[string]int{}
	for _, table := range []string{"live.sessions", "live.programs", "catalog.skus", "identity.store_grants"} {
		before[table] = countRows(t, owner, `SELECT count(*) FROM `+table)
	}
	mustExec(t, owner, `DELETE FROM public.lc_schema_migrations WHERE version='0060_live_claims.sql' OR version=ANY($1)`, dependents)
	for i := 0; i < 2; i++ {
		if err := migrations.Apply(ctx, owner); err != nil {
			t.Fatalf("0060 upgrade apply %d on populated data: %v", i+1, err)
		}
	}
	if n := countRows(t, owner, `SELECT count(*) FROM public.lc_schema_migrations WHERE version='0060_live_claims.sql' AND checksum=$1`, fmt.Sprintf("%x", sha256.Sum256(body))); n != 1 {
		t.Fatalf("0060 ledger rows after upgrade=%d", n)
	}
	for table, n := range before {
		if got := countRows(t, owner, `SELECT count(*) FROM `+table); got != n {
			t.Fatalf("upgrade changed %s rows %d -> %d", table, n, got)
		}
	}
	var title string
	if err := owner.QueryRow(ctx, `SELECT title FROM live.sessions WHERE id=$1`, session).Scan(&title); err != nil || title != "pre-0060 session" {
		t.Fatalf("pre-existing session changed: %q %v", title, err)
	}
	for _, table := range lcTables {
		var force bool
		if err := owner.QueryRow(ctx, `SELECT relforcerowsecurity FROM pg_class WHERE oid=$1::regclass`, table).Scan(&force); err != nil || !force {
			t.Fatalf("upgraded %s FORCE RLS=%t %v", table, force, err)
		}
		if n := countRows(t, owner, `SELECT count(*) FROM `+table); n != 0 {
			t.Fatalf("upgrade backfilled %d rows into %s", n, table)
		}
	}
	if n := countRows(t, owner, `SELECT count(*) FROM identity.store_grants WHERE permission LIKE 'live:%'`); n != 1 {
		t.Fatalf("upgrade changed live grants (no backfill, §3): %d", n)
	}
	// New claim configuration can reference the pre-existing session and SKU.
	mustExec(t, owner, `INSERT INTO live.claim_windows(tenant_id,store_id,session_id,state,match_mode,generation,principal_id) VALUES($1,$2,$3,'CLOSED','EXACT',0,$4)`, tenant, store, session, principal)
	mustExec(t, owner, `INSERT INTO live.offers(tenant_id,store_id,session_id,keyword,sku_id,max_quantity_per_claim,principal_id) VALUES($1,$2,$3,'A1',$4,3,$5)`, tenant, store, session, sku, principal)
}

// TestLiveClaimsKC12Isolation is KC12 (§3.2 RLS/grants, §3.3 guards, 2 tenants x 2 stores).
func TestLiveClaimsKC12Isolation(t *testing.T) {
	h := lcSetup(t)
	f, ctx := h.f, h.ctx
	storeB2 := randomUUID()
	mustExec(t, f.owner, `INSERT INTO control.stores(tenant_id,id,name,currency) VALUES($1,$2,'claims-isolation-b2','TWD')`, f.tenantB, storeB2)
	principalB, tokenB := lcPrincipal(t, f, f.tenantB, []string{f.storeB, storeB2}, "store:read", "live:read", "live:manage")
	t.Cleanup(func() {
		mustExec(t, f.owner, `UPDATE live.claim_windows w SET state='CLOSED',closed_at=clock_timestamp(),version=w.version+1
			WHERE w.state='OPEN' AND w.session_id IN (SELECT id FROM live.sessions WHERE principal_id=$1)`, principalB)
	})
	type claimData struct {
		store, session, bundle, offer string
		version                       int64
		token                         claims.LinkToken
	}
	setup := func(token, tenant, store, currency string) claimData {
		t.Helper()
		session := h.draftAs(t, token, store)
		sku := lcSKUs(t, f, tenant, store, currency, 1)[0]
		w, err := h.setWindow(token, store, session, claims.WindowInput{State: claims.WindowOpen, MatchMode: claims.MatchExact})
		if err != nil || w.State != claims.WindowOpen {
			t.Fatalf("open %s: %v", store, err)
		}
		o, err := h.createOffer(token, store, t04Key("lc-offer"), session, claims.OfferInput{Keyword: "A1", SKUID: sku, MaxQuantityPerClaim: 5})
		if err != nil {
			t.Fatal(err)
		}
		r, err := h.manual(token, store, t04Key("lc-manual"), session, claims.ManualClaimInput{ActorLabel: "iso", Text: "A1+2"})
		if err != nil || r.Outcome != claims.OutcomeAccepted {
			t.Fatalf("claim in %s: %+v %v", store, r, err)
		}
		l, err := h.issue(token, store, t04Key("lc-link"), session, r.BundleID, claims.LinkInput{})
		if err != nil {
			t.Fatal(err)
		}
		return claimData{store: store, session: session, bundle: r.BundleID, offer: o.ID, version: r.BundleVersion, token: l.Token}
	}
	a1 := setup(h.token, f.tenantA, f.storeA1, "USD")
	a2 := setup(h.token, f.tenantA, f.storeA2, "USD")
	b := setup(tokenB, f.tenantB, f.storeB, "TWD")
	caps := map[string]buyer.Capability{f.storeA1: h.cap, f.storeA2: mustIssue(t, h.service, f.storeA2), f.storeB: mustIssue(t, h.service, f.storeB), storeB2: mustIssue(t, h.service, storeB2)}
	links := lcDigest(t, f, "claims")

	// Buyers of every other store (same tenant, other tenant, empty store) get the uniform not-found.
	for _, d := range []claimData{a1, a2, b} {
		for store, c := range caps {
			if store == d.store {
				continue
			}
			if _, err := h.preview(c, d.token); !errors.Is(err, command.ErrNotFound) {
				t.Fatalf("preview of %s link from %s: %v", d.store, store, err)
			}
			if _, err := h.redeem(c, t04Key("lc-redeem"), d.token, d.version); !errors.Is(err, command.ErrNotFound) {
				t.Fatalf("redeem of %s link from %s: %v", d.store, store, err)
			}
		}
	}
	lcSameDigest(t, "cross-store buyer attempts", links, lcDigest(t, f, "claims"))

	// Forged buyer GUCs + a foreign token hash: zero rows, or 22023 for malformed context.
	buyerForged := func(c buyer.Capability, forge []string, query string, args ...any) (int, error) {
		n := 0
		err := buyer.WithScope(ctx, h.a.runtime, c.Token, c.Scope.StoreID, func(ctx context.Context, tx pgx.Tx, _ buyer.Scope) error {
			for i := 0; i+1 < len(forge); i += 2 {
				if _, err := tx.Exec(ctx, `SELECT set_config($1,$2,true)`, forge[i], forge[i+1]); err != nil {
					return err
				}
			}
			return tx.QueryRow(ctx, query, args...).Scan(&n)
		})
		return n, err
	}
	hashA1 := tokenHash(string(a1.token))
	for _, tc := range []struct {
		name  string
		c     buyer.Capability
		forge []string
	}{
		{"A2 buyer", caps[f.storeA2], nil}, {"B buyer", caps[f.storeB], nil},
		{"A1 buyer with tenant B", h.cap, []string{"app.tenant_id", f.tenantB}},
		{"A1 buyer with store A2", h.cap, []string{"app.store_id", f.storeA2}},
		{"B buyer with tenant A", caps[f.storeB], []string{"app.tenant_id", f.tenantA}},
	} {
		for _, q := range []string{`SELECT count(*) FROM claims.preview_link($1)`, `SELECT count(*) FROM claims.redeem_link($1,` + strconv.FormatInt(a1.version, 10) + `)`} {
			if n, err := buyerForged(tc.c, tc.forge, q, hashA1); err != nil || n != 0 {
				t.Fatalf("%s %s: rows=%d err=%v", tc.name, q, n, err)
			}
		}
	}
	for _, tc := range []struct {
		name  string
		forge []string
		query string
		args  []any
	}{
		{"principal set", []string{"app.principal_id", h.actor}, `SELECT count(*) FROM claims.preview_link($1)`, []any{hashA1}},
		{"buyer empty", []string{"app.buyer_id", ""}, `SELECT count(*) FROM claims.preview_link($1)`, []any{hashA1}},
		{"buyer malformed", []string{"app.buyer_id", "not-a-uuid"}, `SELECT count(*) FROM claims.redeem_link($1,1)`, []any{hashA1}},
		{"session empty", []string{"app.buyer_session_id", ""}, `SELECT count(*) FROM claims.preview_link($1)`, []any{hashA1}},
		{"tenant upper", []string{"app.tenant_id", strings.ToUpper(f.tenantA)}, `SELECT count(*) FROM claims.preview_link($1)`, []any{hashA1}},
		{"store empty", []string{"app.store_id", ""}, `SELECT claims.mark_applied($1,ARRAY[$2]::uuid[],ARRAY[1]::bigint[])`, []any{a1.bundle, a1.offer}},
		{"short hash", nil, `SELECT count(*) FROM claims.preview_link($1)`, []any{randomBytes(31)}},
		{"null hash", nil, `SELECT count(*) FROM claims.redeem_link(NULL,1)`, nil},
		{"null version", nil, `SELECT count(*) FROM claims.redeem_link($1,NULL)`, []any{hashA1}},
		{"empty arrays", nil, `SELECT claims.mark_applied($1,ARRAY[]::uuid[],ARRAY[]::bigint[])`, []any{a1.bundle}},
		{"unequal arrays", nil, `SELECT claims.mark_applied($1,ARRAY[$2]::uuid[],ARRAY[1,2]::bigint[])`, []any{a1.bundle, a1.offer}},
		{"duplicate offers", nil, `SELECT claims.mark_applied($1,ARRAY[$2,$2]::uuid[],ARRAY[1,1]::bigint[])`, []any{a1.bundle, a1.offer}},
		{"51 lines", nil, `SELECT claims.mark_applied($1,array_fill($2::uuid,ARRAY[51]),array_fill(1::bigint,ARRAY[51]))`, []any{a1.bundle, a1.offer}},
	} {
		_, err := buyerForged(h.cap, tc.forge, tc.query, tc.args...)
		requirePGCode(t, err, "22023", "buyer guard "+tc.name)
	}
	for _, tc := range []struct {
		name   string
		c      buyer.Capability
		bundle string
		offer  string
	}{{"foreign-store bundle", h.cap, a2.bundle, a2.offer}, {"unbound own-store bundle", h.cap, a1.bundle, a1.offer}} {
		_, err := buyerForged(tc.c, nil, `SELECT claims.mark_applied($1,ARRAY[$2]::uuid[],ARRAY[1]::bigint[])`, tc.bundle, tc.offer)
		requirePGCode(t, err, "PT409", "mark_applied "+tc.name)
	}
	rr, err := f.owner.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead})
	if err != nil {
		t.Fatal(err)
	}
	// Always end the transaction: a t.Fatal with it open leaks the shared owner
	// pool connection and TestMain's pool Close then waits forever.
	defer func() { _ = rr.Rollback(context.Background()) }()
	if _, err := rr.Exec(ctx, `SELECT set_config('app.tenant_id',$1,true), set_config('app.store_id',$2,true),
		set_config('app.buyer_id',$3,true), set_config('app.buyer_session_id',$4,true), set_config('app.principal_id','',true)`,
		f.tenantA, f.storeA1, h.cap.Scope.OwnerID, h.cap.Scope.SessionID); err != nil {
		t.Fatal(err)
	}
	_, err = rr.Exec(ctx, `SET LOCAL ROLE commerce_buyer_runtime`)
	if err == nil {
		_, err = rr.Exec(ctx, `SELECT count(*) FROM claims.preview_link($1)`, hashA1)
	}
	_ = rr.Rollback(ctx)
	requirePGCode(t, err, "22023", "REPEATABLE READ buyer definer")
	lcSameDigest(t, "forged buyer contexts", links, lcDigest(t, f, "claims"))

	// Merchants: another store's scope (legitimate or forged GUC) sees nothing of A1.
	countA1 := func(tx pgx.Tx) (int, error) {
		var n int
		err := tx.QueryRow(ctx, `SELECT (SELECT count(*) FROM live.offers WHERE session_id=$1)+(SELECT count(*) FROM live.claim_windows WHERE session_id=$1)
			+(SELECT count(*) FROM claims.bundles WHERE session_id=$1)+(SELECT count(*) FROM claims.lines WHERE session_id=$1)
			+(SELECT count(*) FROM claims.events WHERE session_id=$1)+(SELECT count(*) FROM claims.links WHERE bundle_id=$2)`, a1.session, a1.bundle).Scan(&n)
		return n, err
	}
	if err := h.do(h.token, f.storeA1, func(tx pgx.Tx, _ platform.Scope) error {
		n, err := countA1(tx)
		if err == nil && n != 6 {
			err = fmt.Errorf("positive control saw %d A1 rows", n)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, token, store string
		forge              []string
	}{
		{"A2 merchant", h.token, f.storeA2, nil}, {"B merchant", tokenB, f.storeB, nil}, {"B2 merchant", tokenB, storeB2, nil},
		{"A1 merchant forging store B", h.token, f.storeA1, []string{"app.store_id", f.storeB}},
		{"A1 merchant forging tenant B", h.token, f.storeA1, []string{"app.tenant_id", f.tenantB}},
	} {
		if err := h.do(tc.token, tc.store, func(tx pgx.Tx, _ platform.Scope) error {
			for i := 0; i+1 < len(tc.forge); i += 2 {
				if _, err := tx.Exec(ctx, `SELECT set_config($1,$2,true)`, tc.forge[i], tc.forge[i+1]); err != nil {
					return err
				}
			}
			n, err := countA1(tx)
			if err == nil && n != 0 {
				err = fmt.Errorf("saw %d A1 rows", n)
			}
			return err
		}); err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
	}
	for _, tc := range []struct {
		name, token, store string
	}{{"A2", h.token, f.storeA2}, {"B", tokenB, f.storeB}} {
		lcIs(t, lcErr(h.getBoard(tc.token, tc.store, a1.session)), command.ErrNotFound, tc.name+" GetBoard")
		lcIs(t, lcErr(h.bundles(tc.token, tc.store, a1.session, pagination.Request{})), command.ErrNotFound, tc.name+" ListBundles")
		lcIs(t, lcErr(h.setWindow(tc.token, tc.store, a1.session, claims.WindowInput{ExpectedVersion: 1, State: claims.WindowClosed, MatchMode: claims.MatchExact})), command.ErrNotFound, tc.name+" SetWindow")
		lcIs(t, lcErr(h.createOffer(tc.token, tc.store, t04Key("lc-offer"), a1.session, claims.OfferInput{Keyword: "Z1", SKUID: h.stock.skus[1].ID, MaxQuantityPerClaim: 1})), command.ErrNotFound, tc.name+" CreateOffer")
		lcIs(t, lcErr(h.updateOffer(tc.token, tc.store, t04Key("lc-offer"), a1.session, a1.offer, claims.OfferUpdate{ExpectedVersion: 1, MaxQuantityPerClaim: 1, Active: true})), command.ErrNotFound, tc.name+" UpdateOffer")
		lcIs(t, lcErr(h.manual(tc.token, tc.store, t04Key("lc-manual"), a1.session, claims.ManualClaimInput{BundleID: a1.bundle, Text: "A1"})), command.ErrNotFound, tc.name+" RecordManualClaim")
		lcIs(t, lcErr(h.issue(tc.token, tc.store, t04Key("lc-link"), a1.session, a1.bundle, claims.LinkInput{ExpectedGeneration: 1})), command.ErrNotFound, tc.name+" IssueLink")
	}
	// The merchant runtime can neither read credentials/bindings nor write them (42501).
	for _, q := range []string{
		`SELECT token_hash FROM claims.links`, `SELECT principal_id FROM claims.links`, `SELECT * FROM claims.links`, `SELECT owner_id FROM claims.bundles`, `SELECT * FROM claims.bundles`,
		`UPDATE claims.bundles SET owner_id=NULL`, `UPDATE claims.bundles SET bound_at=NULL`, `UPDATE claims.lines SET applied_version=NULL`,
		`INSERT INTO claims.links(tenant_id,store_id,bundle_id,token_hash,generation,issued_at,expires_at,principal_id) SELECT tenant_id,store_id,id,'\x00'::bytea,1,now(),now(),'` + h.actor + `' FROM claims.bundles`,
		`UPDATE claims.links SET generation=generation`, `DELETE FROM claims.links`, `DELETE FROM claims.bundles`, `DELETE FROM claims.lines`, `DELETE FROM claims.events`,
		`UPDATE claims.events SET quantity=quantity`, `UPDATE claims.bundles SET label=label`, `UPDATE claims.bundles SET actor_key=actor_key`, `UPDATE claims.lines SET offer_id=offer_id`,
		`TRUNCATE claims.events`, `DELETE FROM live.claim_windows`,
	} {
		err := h.do(h.token, f.storeA1, func(tx pgx.Tx, _ platform.Scope) error { _, e := tx.Exec(ctx, q); return e })
		requirePGCode(t, err, "42501", "runtime "+q)
	}

	// claims.issue_link decides authority in the database: live:read-only principal,
	// foreign p_store, forged GUCs -> PT403/PT404/22023 and nothing written.
	_, readerToken := lcPrincipal(t, f, f.tenantA, []string{f.storeA1}, "store:read", "live:read")
	peer, _ := lcPrincipal(t, f, f.tenantA, []string{f.storeA1}, "store:read", "live:read", "live:manage")
	before := lcLinkState(t, f, a1.bundle)
	for _, tc := range []struct {
		name, want, token, store, pStore string
		forge                            []string
	}{
		{"live:read only", "PT403", readerToken, f.storeA1, f.storeA1, nil},
		{"foreign p_store (other tenant)", "PT404", h.token, f.storeA1, f.storeB, nil},
		{"p_store differs from GUC store", "PT403", h.token, f.storeA1, f.storeA2, nil},
		{"forged principal GUC", "PT403", h.token, f.storeA1, f.storeA1, []string{"app.principal_id", peer}},
		{"forged revision GUC", "PT403", h.token, f.storeA1, f.storeA1, []string{"app.authz_revision", "999"}},
		{"buyer GUC present", "22023", h.token, f.storeA1, f.storeA1, []string{"app.buyer_id", h.cap.Scope.OwnerID}},
		{"revision unset", "22023", h.token, f.storeA1, f.storeA1, []string{"app.authz_revision", ""}},
	} {
		err := h.directMerchant(tc.token, tc.store, func(tx pgx.Tx, _ platform.Scope) error {
			for i := 0; i+1 < len(tc.forge); i += 2 {
				if _, err := tx.Exec(ctx, `SELECT set_config($1,$2,true)`, tc.forge[i], tc.forge[i+1]); err != nil {
					return err
				}
			}
			_, err := tx.Exec(ctx, `SELECT * FROM claims.issue_link($1,$2,$3,$4,1,$5,true)`, tokenHash(tc.token), tc.pStore, a1.session, a1.bundle, randomBytes(32))
			return err
		})
		requirePGCode(t, err, tc.want, "issue_link "+tc.name)
	}
	if after := lcLinkState(t, f, a1.bundle); after.generation != before.generation || !bytesEqual(after.hash, before.hash) {
		t.Fatal("refused issue_link calls changed the link")
	}
	lcIs(t, lcErr(h.issue(readerToken, f.storeA1, t04Key("lc-link"), a1.session, a1.bundle, claims.LinkInput{ExpectedGeneration: 1})), platform.ErrForbidden, "IssueLink live:read only")

	// Every policy evaluated as every role/context raises no 42P17 (no recursive policy).
	contexts := map[string][]string{
		"merchant": {"app.tenant_id", f.tenantA, "app.store_id", f.storeA1, "app.principal_id", h.actor, "app.buyer_id", ""},
		"buyer":    {"app.tenant_id", f.tenantA, "app.store_id", f.storeA1, "app.principal_id", "", "app.buyer_id", h.cap.Scope.OwnerID, "app.buyer_session_id", h.cap.Scope.SessionID},
		"mixed":    {"app.tenant_id", f.tenantA, "app.store_id", f.storeA1, "app.principal_id", h.actor, "app.buyer_id", h.cap.Scope.OwnerID},
	}
	statements := []string{
		`SELECT count(*) FROM (SELECT tenant_id,bundle_id,generation,expires_at FROM claims.links) x`,
		`SELECT count(*) FROM (SELECT id,session_id,bound_at,version FROM claims.bundles) x`,
		`SELECT count(*) FROM (SELECT bundle_id,offer_id,quantity,version,applied_version FROM claims.lines) x`,
		`SELECT count(*) FROM (SELECT id,session_id,keyword,active FROM live.offers) x`,
		`SELECT count(*) FROM claims.events`, `SELECT count(*) FROM live.claim_windows`,
		`UPDATE claims.bundles SET bound_at=bound_at WHERE session_id='` + a1.session + `'`,
		`UPDATE claims.lines SET applied_version=applied_version WHERE session_id='` + a1.session + `'`,
		`UPDATE claims.links SET generation=generation WHERE bundle_id='` + a1.bundle + `'`,
		`UPDATE live.offers SET version=version WHERE session_id='` + a1.session + `'`,
		`SELECT count(*) FROM claims.preview_link('\x` + hex.EncodeToString(hashA1) + `'::bytea)`,
	}
	for _, role := range []string{"commerce_runtime", "commerce_claims_writer", "commerce_buyer_runtime"} {
		for name, gucs := range contexts {
			for _, q := range statements {
				tx, err := f.owner.Begin(ctx)
				if err != nil {
					t.Fatal(err)
				}
				for i := 0; i+1 < len(gucs); i += 2 {
					if _, err := tx.Exec(ctx, `SELECT set_config($1,$2,true)`, gucs[i], gucs[i+1]); err != nil {
						t.Fatal(err)
					}
				}
				_, err = tx.Exec(ctx, `SET LOCAL ROLE `+role)
				if err == nil {
					_, err = tx.Exec(ctx, q)
				}
				_ = tx.Rollback(ctx)
				if sqlState(err) == "42P17" {
					t.Fatalf("%s/%s %q raised 42P17 (recursive policy)", role, name, q)
				}
			}
		}
	}
	lcSameDigest(t, "isolation probes", links, lcDigest(t, f, "claims"))
}

// TestLiveClaimsP2aPolicyContexts closes §0.1 P2(a): permissive UPDATE policies are
// OR-ed, so the writer's bind/release/rotate policies must each pin their own context. A
// buyer transaction cannot release or rotate; a merchant transaction cannot bind; a mixed
// context can do neither. The probes run as commerce_claims_writer directly (as if a
// definer body were buggy), inside rolled-back transactions.
func TestLiveClaimsP2aPolicyContexts(t *testing.T) {
	h := lcSetup(t)
	f, ctx := h.f, h.ctx
	s, _, bound, l := h.claimSetup(t, "p2a-bound")
	if _, err := h.redeem(h.cap, t04Key("lc-redeem"), l.Token, bound.BundleVersion); err != nil {
		t.Fatal(err)
	}
	unbound := h.accepted(t, s, "", "p2a-unbound", "A1")
	owner2 := mustIssue(t, h.service, f.storeA1)
	buyerCtx := []string{"app.tenant_id", f.tenantA, "app.store_id", f.storeA1, "app.principal_id", "", "app.buyer_id", h.cap.Scope.OwnerID, "app.buyer_session_id", h.cap.Scope.SessionID}
	merchantCtx := []string{"app.tenant_id", f.tenantA, "app.store_id", f.storeA1, "app.principal_id", h.actor, "app.buyer_id", ""}
	mixedCtx := []string{"app.tenant_id", f.tenantA, "app.store_id", f.storeA1, "app.principal_id", h.actor, "app.buyer_id", h.cap.Scope.OwnerID}
	asWriter := func(gucs []string, q string, args ...any) (int64, error) {
		tx, err := f.owner.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback(ctx)
		for i := 0; i+1 < len(gucs); i += 2 {
			if _, err := tx.Exec(ctx, `SELECT set_config($1,$2,true)`, gucs[i], gucs[i+1]); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := tx.Exec(ctx, `SET LOCAL ROLE commerce_claims_writer`); err != nil {
			t.Fatal(err)
		}
		tag, err := tx.Exec(ctx, q, args...)
		return tag.RowsAffected(), err
	}
	release := `UPDATE claims.bundles SET owner_id=NULL,bound_at=NULL WHERE id=$1`
	bind := `UPDATE claims.bundles SET owner_id=$2,bound_at=clock_timestamp() WHERE id=$1`
	rotate := `UPDATE claims.links SET token_hash=$2 WHERE bundle_id=$1`
	issue := `INSERT INTO claims.links(tenant_id,store_id,bundle_id,token_hash,generation,issued_at,expires_at,principal_id) SELECT $2,$3,$1,$4,1,v,v+interval '1 hour',$5 FROM (SELECT clock_timestamp() v) x`
	denied := func(label string, n int64, err error) {
		t.Helper()
		if err == nil && n != 0 {
			t.Fatalf("%s: %d rows changed", label, n)
		}
		if err != nil && sqlState(err) != "42501" {
			t.Fatalf("%s: sqlstate %s (%v), want 0 rows or 42501", label, sqlState(err), err)
		}
	}
	// Positive controls: each policy works in its own context.
	if n, err := asWriter(buyerCtx, bind, unbound.BundleID, h.cap.Scope.OwnerID); err != nil || n != 1 {
		t.Fatalf("control: buyer binds an unbound bundle: %d %v", n, err)
	}
	if n, err := asWriter(merchantCtx, release, bound.BundleID); err != nil || n != 1 {
		t.Fatalf("control: merchant releases: %d %v", n, err)
	}
	if n, err := asWriter(merchantCtx, rotate, bound.BundleID, randomBytes(32)); err != nil || n != 1 {
		t.Fatalf("control: merchant rotates: %d %v", n, err)
	}
	// A buyer transaction can neither release nor rotate nor issue.
	n, err := asWriter(buyerCtx, release, bound.BundleID)
	denied("buyer releases its own binding", n, err)
	n, err = asWriter(buyerCtx, rotate, bound.BundleID, randomBytes(32))
	denied("buyer rotates", n, err)
	n, err = asWriter(buyerCtx, issue, unbound.BundleID, f.tenantA, f.storeA1, randomBytes(32), h.actor)
	denied("buyer issues a link", n, err)
	n, err = asWriter(buyerCtx, bind, bound.BundleID, owner2.Scope.OwnerID)
	denied("buyer re-binds to another owner", n, err)
	// A merchant transaction cannot bind (nor re-bind) any bundle.
	n, err = asWriter(merchantCtx, bind, unbound.BundleID, owner2.Scope.OwnerID)
	denied("merchant binds an unbound bundle", n, err)
	n, err = asWriter(merchantCtx, bind, bound.BundleID, owner2.Scope.OwnerID)
	denied("merchant re-binds a bound bundle", n, err)
	// A mixed context satisfies neither side.
	for label, q := range map[string][]any{"mixed bind": {bind, unbound.BundleID, h.cap.Scope.OwnerID}, "mixed release": {release, bound.BundleID}, "mixed rotate": {rotate, bound.BundleID, randomBytes(32)}} {
		n, err = asWriter(mixedCtx, q[0].(string), q[1:]...)
		denied(label, n, err)
	}
	if lcOwner(t, f, bound.BundleID) != h.cap.Scope.OwnerID || lcOwner(t, f, unbound.BundleID) != "" {
		t.Fatal("rolled-back probes changed a binding")
	}
}

// TestLiveClaimsP2gLinkTTLInDatabase closes §0.1 P2(g): the 72 h TTL and expiry are
// enforced by the database (CHECK + definer clock), not only in Go.
func TestLiveClaimsP2gLinkTTLInDatabase(t *testing.T) {
	h := lcSetup(t)
	f, ctx := h.f, h.ctx
	s, _, r, l := h.claimSetup(t, "p2g")
	row := lcLinkState(t, f, r.BundleID)
	if !row.found || !row.exactly72 || !row.expires.Equal(l.ExpiresAt) || row.generation != 1 {
		t.Fatalf("issued link row %+v (IssuedLink.ExpiresAt=%v)", row, l.ExpiresAt)
	}
	for label, q := range map[string]string{
		"TTL above 72h":  `UPDATE claims.links SET expires_at=issued_at+interval '72 hours 1 microsecond' WHERE bundle_id=$1`,
		"zero TTL":       `UPDATE claims.links SET expires_at=issued_at WHERE bundle_id=$1`,
		"negative TTL":   `UPDATE claims.links SET expires_at=issued_at-interval '1 second' WHERE bundle_id=$1`,
		"issued forward": `UPDATE claims.links SET issued_at=expires_at WHERE bundle_id=$1`,
	} {
		_, err := f.owner.Exec(ctx, q, r.BundleID)
		requirePGCode(t, err, "23514", label)
	}
	// Move the whole 72 h window so it ends ~1.5 s from now (one clock read keeps the CHECK).
	var expires time.Time
	if err := f.owner.QueryRow(ctx, `UPDATE claims.links SET issued_at=x.v,expires_at=x.v+interval '72 hours'
		FROM (SELECT clock_timestamp()-interval '72 hours'+interval '1500 milliseconds' AS v) x WHERE bundle_id=$1 RETURNING expires_at`, r.BundleID).Scan(&expires); err != nil {
		t.Fatal(err)
	}
	hash := tokenHash(string(l.Token))
	if p, err := h.preview(h.cap, l.Token); err != nil || !p.ExpiresAt.Equal(expires) {
		t.Fatalf("preview before expiry: %+v %v", p, err)
	}
	if n, err := h.buyerCount(h.cap, `SELECT count(*) FROM claims.preview_link($1)`, hash); err != nil || n != 1 {
		t.Fatalf("definer before expiry rows=%d %v", n, err)
	}
	mustExec(t, f.owner, `SELECT pg_sleep(GREATEST(0,extract(epoch FROM $1::timestamptz-clock_timestamp()))+0.05)`, expires)
	if n, err := h.buyerCount(h.cap, `SELECT count(*) FROM claims.preview_link($1)`, hash); err != nil || n != 0 {
		t.Fatalf("definer preview after DB expiry rows=%d %v", n, err)
	}
	if n, err := h.buyerCount(h.cap, `SELECT count(*) FROM claims.redeem_link($1,$2)`, hash, r.BundleVersion); err != nil || n != 0 {
		t.Fatalf("definer redeem after DB expiry rows=%d %v", n, err)
	}
	lcIs(t, lcErr(h.preview(h.cap, l.Token)), command.ErrNotFound, "PreviewLink after expiry")
	lcIs(t, lcErr(h.redeem(h.cap, t04Key("lc-redeem"), l.Token, r.BundleVersion)), command.ErrNotFound, "RedeemLink after expiry")
	if lcOwner(t, f, r.BundleID) != "" {
		t.Fatal("expired link bound the bundle")
	}
	page, err := h.bundles(h.token, f.storeA1, s, pagination.Request{})
	if err != nil || len(page.Items) != 1 || page.Items[0].Link.State != "EXPIRED" || page.Items[0].Link.Generation != 1 {
		t.Fatalf("merchant link state after expiry: %+v %v", page.Items, err)
	}
	// Rotation after expiry resets both timestamps from one clock read.
	next := h.link(t, s, r.BundleID, 1, false)
	if row := lcLinkState(t, f, r.BundleID); !row.exactly72 || !row.expires.Equal(next.ExpiresAt) || !row.issued.After(expires) {
		t.Fatalf("rotated link row %+v", row)
	}
	if _, err := h.preview(h.cap, next.Token); err != nil {
		t.Fatalf("rotated token after expiry: %v", err)
	}
}
