// T10c meta claims intake gates MCI02 (migration 0064 + post-River 0014: upgrade, exact privilege
// delta, RLS, definers, pool validator) and MCI03 (live.put_claim_source), contracts/
// meta-claims-intake-v1.md §2, §4, §4.3, §4.4 and §12, written by the independent test_worker from
// the FROZEN contract, not from the migration SQL.
//
// Owns: the pre/post-migration catalog diff. A throw-away labelled PostgreSQL container gets every
// migration EXCEPT the meta-claims-intake pair (mirroring migrations.Apply phase by phase, with a
// populated OPEN claim window), the ACL/RLS/definer catalog is snapshotted, migrations.Apply installs
// the pair (twice), and the difference must equal the §4.3 table row for row: post - pre ==
// expected - pre, and pre - post is empty. The migration files are found by glob
// (*_meta_claims_intake.sql, post_river/*_meta_claims_intake_river.sql) because the integrator may
// renumber 0064/0014 at merge.
//
// Non-goals: no expectation copied from the migration bodies. Where §4.3 says only "fixed columns"
// (commerce_integration_writer SELECT on three tables) the gate proves column-level SELECT and
// nothing else instead of inventing a list. Evidence label: MOCK (REAL_PG).
package foundation_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"
	"github.com/riverqueue/river/rivermigrate"

	"livecommerce/internal/claims"
	"livecommerce/internal/platform"
	"livecommerce/migrations"
)

// ---------------------------------------------------------------------------------------
// Throw-away pre-migration database
// ---------------------------------------------------------------------------------------

func mciStartPG(t *testing.T) *pgxpool.Pool {
	t.Helper()
	// The same explicit real-PG permission gate as every other fixture, without needing the shared
	// database: this test provisions its own labelled container (like mcPre0029Fixture).
	if os.Getenv("LC_TEST_DATABASE_ALLOWED") != "1" {
		t.Skip("LC_TEST_DATABASE_ALLOWED=1 is required; real-PG upgrade test NOT_RUN")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	name := "lc-mci-upgrade-" + t04Tag()
	password := hex.EncodeToString(randomBytes(24))
	if out, err := exec.CommandContext(ctx, "docker", "run", "-d", "--pull=never", "--name", name,
		"--label", "livecommerce.fixture="+name, "--memory=512m", "--cpus=1", "--pids-limit=128",
		"--tmpfs", "/var/lib/postgresql:rw,size=268435456", "-e", "POSTGRES_PASSWORD="+password,
		"-e", "POSTGRES_DB=lc_foundation_test", "-p", "127.0.0.1::5432",
		"postgres@sha256:4ef4dbc939d61acea57712655ddb4b4ab27419c913f94cca0cd57cb3ea3c2280",
		"-c", "shared_buffers=32MB", "-c", "max_connections=60").CombinedOutput(); err != nil {
		t.Fatalf("start labelled upgrade PG: %v: %s", err, out)
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
			t.Errorf("remove upgrade PG %s: %v: %s", name, err, out)
		}
	})
	portOut, err := exec.CommandContext(ctx, "docker", "port", name, "5432/tcp").Output()
	if err != nil || !strings.HasPrefix(strings.TrimSpace(string(portOut)), "127.0.0.1:") {
		t.Fatal("upgrade PG did not bind loopback")
	}
	port := strings.TrimPrefix(strings.TrimSpace(string(portOut)), "127.0.0.1:")
	if _, err := strconv.Atoi(port); err != nil {
		t.Fatal("upgrade PG returned an invalid port")
	}
	u := &url.URL{Scheme: "postgres", User: url.UserPassword("postgres", password), Host: "127.0.0.1:" + port, Path: "/lc_foundation_test", RawQuery: "sslmode=disable"}
	owner, err := pgxpool.New(ctx, u.String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(owner.Close)
	for i := 0; i < 60; i++ {
		probe, stop := context.WithTimeout(ctx, 500*time.Millisecond)
		err = owner.Ping(probe)
		stop()
		if err == nil {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	if err != nil {
		t.Fatalf("upgrade PG did not become ready: %v", err)
	}
	return owner
}

// mciIntakeFiles returns the two migration paths (glob, not a fixed number).
func mciIntakeFiles(t *testing.T) (numbered, post string) {
	t.Helper()
	a, _ := filepath.Glob("../../migrations/[0-9][0-9][0-9][0-9]_meta_claims_intake.sql")
	b, _ := filepath.Glob("../../migrations/post_river/[0-9][0-9][0-9][0-9]_meta_claims_intake_river.sql")
	if len(a) != 1 || len(b) != 1 {
		t.Fatalf("expected exactly one migrations/NNNN_meta_claims_intake.sql (got %v) and one post_river/NNNN_meta_claims_intake_river.sql (got %v)", a, b)
	}
	return a[0], b[0]
}

// mciRetentionFile is claims-retention-purge-v1's numbered migration (0071): it requires 0064 (55000
// precondition), so MCI02 holds it back with the intake files and its §4 rows join the delta (§6 clause 5).
func mciRetentionFile(t *testing.T) string {
	t.Helper()
	a, _ := filepath.Glob("../../migrations/[0-9][0-9][0-9][0-9]_claims_retention.sql")
	if len(a) != 1 {
		t.Fatalf("expected exactly one migrations/NNNN_claims_retention.sql (got %v)", a)
	}
	return a[0]
}

// mciRetentionRoles are created by 0071; outside the three §6-clause-5 tables their privileges are CRP02's.
var mciRetentionRoles = map[string]bool{"commerce_retention_writer": true, "commerce_retention_job": true, "commerce_retention_operator": true}

// mciApplyWithout mirrors migrations.Apply (same phase order, ledger and River grants) but skips
// the two meta-claims-intake files (and the dependent 0071), producing the "populated 0063" database of §12 MCI02.
// mciHeldBack lists the numbered migrations after 0064 that depend on it (0078 customers-core, 0079 billing-core).
var mciHeldBack = []string{"0078_customers_privacy.sql", "0079_platform_billing.sql"}

func mciApplyWithout(t *testing.T, owner *pgxpool.Pool) {
	t.Helper()
	skipA, skipB := mciIntakeFiles(t)
	skipR := mciRetentionFile(t)
	// The meta-ads migrations (unit ads-core: 0074/0075, post-River 0015) build on the intake's Page-token custody tables, so
	// they cannot run before it; they are ledger-marked applied below and never executed by this gate.
	adsNumbered, _ := filepath.Glob("../../migrations/[0-9][0-9][0-9][0-9]_meta_ads*.sql")
	capiNumbered, _ := filepath.Glob("../../migrations/[0-9][0-9][0-9][0-9]_meta_capi.sql") // ads-capi 0080 builds on 0074's schema
	adsNumbered = append(adsNumbered, capiNumbered...)
	adsPost, _ := filepath.Glob("../../migrations/post_river/[0-9][0-9][0-9][0-9]_meta_ads_river.sql")
	adsSkip := map[string]bool{}
	for _, path := range append(adsNumbered, adsPost...) {
		adsSkip[path] = true
	}
	ctx := context.Background()
	numbered, _ := filepath.Glob("../../migrations/[0-9][0-9][0-9][0-9]_*.sql")
	post, _ := filepath.Glob("../../migrations/post_river/[0-9][0-9][0-9][0-9]_*.sql")
	sort.Strings(numbered)
	sort.Strings(post)
	apply := func(tx pgx.Tx, paths []string, prefix string, skip string) {
		for _, path := range paths {
			if path == skip || path == skipR {
				continue
			}
			body, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if adsSkip[path] {
				// Recorded as applied WITHOUT running: the ads objects exist in neither the pre nor the post state of this
				// gate, so its exact-delta assertions stay about the intake migrations only (unit ads-core).
				if _, err := tx.Exec(ctx, `INSERT INTO public.lc_schema_migrations(version,checksum) VALUES($1,$2)`, prefix+filepath.Base(path), fmt.Sprintf("%x", sha256.Sum256(body))); err != nil {
					t.Fatal(err)
				}
				continue
			}
			// Held-back migrations build on 0064's objects (customers-billing-v1: 0079 reads
			// live.claim_window_intervals): recorded in the ledger without running, so the intake Apply below
			// yields exactly the intake delta; the test's Cleanup un-records and applies them on top.
			if !slices.Contains(mciHeldBack, filepath.Base(path)) {
				if _, err := tx.Exec(ctx, string(body)); err != nil {
					t.Fatalf("historical migration %s: %v", path, err)
				}
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
	// A Fatalf inside apply leaves the tx open; pool.Close (t.Cleanup) would then wait forever for the
	// acquired connection and leak the labelled container. Rollback after Commit is a no-op.
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `CREATE TABLE IF NOT EXISTS public.lc_schema_migrations (version text PRIMARY KEY, checksum text NOT NULL, applied_at timestamptz NOT NULL DEFAULT now())`); err != nil {
		t.Fatal(err)
	}
	apply(tx, numbered, "", skipA)
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	for _, schema := range []string{"river", "river_meta", "river_payment", "river_expiry", "river_media"} {
		up, err := rivermigrate.New(riverpgxv5.New(owner), &rivermigrate.Config{Schema: schema, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := up.Migrate(ctx, rivermigrate.DirectionUp, nil); err != nil {
			t.Fatal(err)
		}
	}
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
	defer func() { _ = tx.Rollback(ctx) }()
	for _, stmt := range []string{
		`GRANT SELECT, INSERT, UPDATE(kind) ON river.river_job TO commerce_checkout_runtime; GRANT USAGE ON SEQUENCE river.river_job_id_seq TO commerce_checkout_runtime;
		 GRANT SELECT ON river.river_job TO commerce_checkout_writer,commerce_integration_writer`,
	} {
		if _, err := tx.Exec(ctx, stmt); err != nil {
			t.Fatal(err)
		}
	}
	apply(tx, post, "post_river/", skipB)
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

// ---------------------------------------------------------------------------------------
// Catalog snapshot
// ---------------------------------------------------------------------------------------

const mciRoles = `(SELECT rolname::text rn FROM pg_roles WHERE rolname LIKE 'commerce\_%' UNION ALL SELECT 'public')`
const mciSchemaFilter = `n.nspname !~ '^(pg_|information_schema)'`

type mciCatalog struct {
	acl      map[string]bool // role|kind|object|priv
	policies map[string]string
	members  map[string]bool
	rls      map[string]string // table -> enable/force
	fnOwner  map[string]string // definer schema.name -> owner
}

func mciSet(t *testing.T, pool *pgxpool.Pool, query string) map[string]bool {
	t.Helper()
	rows, err := pool.Query(context.Background(), query)
	if err != nil {
		t.Fatalf("catalog query: %v", err)
	}
	items, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatal(err)
	}
	out := make(map[string]bool, len(items))
	for _, item := range items {
		out[item] = true
	}
	return out
}

func mciSnapshot(t *testing.T, pool *pgxpool.Pool) mciCatalog {
	t.Helper()
	c := mciCatalog{acl: map[string]bool{}, policies: map[string]string{}, rls: map[string]string{}, fnOwner: map[string]string{}}
	add := func(m map[string]bool) {
		for k := range m {
			c.acl[k] = true
		}
	}
	add(mciSet(t, pool, `SELECT r.rn||'|table|'||n.nspname||'.'||c.relname||'|'||p.priv FROM `+mciRoles+` r CROSS JOIN pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace
		CROSS JOIN (VALUES ('SELECT'),('INSERT'),('UPDATE'),('DELETE'),('TRUNCATE'),('REFERENCES'),('TRIGGER')) p(priv)
		WHERE c.relkind IN ('r','p') AND `+mciSchemaFilter+` AND has_table_privilege(r.rn,c.oid,p.priv)`))
	add(mciSet(t, pool, `SELECT r.rn||'|column|'||n.nspname||'.'||c.relname||'.'||a.attname||'|'||p.priv FROM `+mciRoles+` r CROSS JOIN pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace
		JOIN pg_attribute a ON a.attrelid=c.oid AND a.attnum>0 AND NOT a.attisdropped
		CROSS JOIN (VALUES ('SELECT'),('INSERT'),('UPDATE'),('REFERENCES')) p(priv)
		WHERE c.relkind IN ('r','p') AND `+mciSchemaFilter+` AND has_column_privilege(r.rn,c.oid,a.attnum,p.priv) AND NOT has_table_privilege(r.rn,c.oid,p.priv)`))
	add(mciSet(t, pool, `SELECT r.rn||'|exec|'||n.nspname||'.'||p.proname||'|EXECUTE' FROM `+mciRoles+` r CROSS JOIN pg_proc p JOIN pg_namespace n ON n.oid=p.pronamespace
		WHERE p.prosecdef AND `+mciSchemaFilter+` AND has_function_privilege(r.rn,p.oid,'EXECUTE')`))
	add(mciSet(t, pool, `SELECT r.rn||'|schema|'||n.nspname||'|'||p.priv FROM `+mciRoles+` r CROSS JOIN pg_namespace n CROSS JOIN (VALUES ('USAGE'),('CREATE')) p(priv)
		WHERE `+mciSchemaFilter+` AND has_schema_privilege(r.rn,n.oid,p.priv)`))
	add(mciSet(t, pool, `SELECT r.rn||'|sequence|'||n.nspname||'.'||c.relname||'|'||p.priv FROM `+mciRoles+` r CROSS JOIN pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace
		CROSS JOIN (VALUES ('USAGE'),('SELECT'),('UPDATE')) p(priv) WHERE c.relkind='S' AND `+mciSchemaFilter+` AND has_sequence_privilege(r.rn,c.oid,p.priv)`))
	c.members = mciSet(t, pool, `SELECT roleid::regrole::text||'>'||member::regrole::text||'|inherit='||inherit_option||'|set='||set_option||'|admin='||admin_option FROM pg_auth_members
		WHERE roleid::regrole::text LIKE 'commerce\_%' OR member::regrole::text LIKE 'commerce\_%'`)
	rows, err := pool.Query(context.Background(), `SELECT p.polrelid::regclass::text||'|'||p.polname,
		p.polcmd::text||'|'||p.polpermissive::text||'|'||coalesce((SELECT string_agg(r.rolname,',' ORDER BY r.rolname) FROM pg_roles r WHERE r.oid=ANY(p.polroles)),'public')||'|'||coalesce(pg_get_expr(p.polqual,p.polrelid),'')||'|'||coalesce(pg_get_expr(p.polwithcheck,p.polrelid),'')
		FROM pg_policy p`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			t.Fatal(err)
		}
		c.policies[k] = v
	}
	rows.Close()
	rows, err = pool.Query(context.Background(), `SELECT n.nspname||'.'||c.relname, CASE WHEN c.relrowsecurity THEN 'enabled' ELSE 'off' END||'/'||CASE WHEN c.relforcerowsecurity THEN 'forced' ELSE 'not-forced' END
		FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace WHERE c.relkind IN ('r','p') AND `+mciSchemaFilter)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			t.Fatal(err)
		}
		c.rls[k] = v
	}
	rows.Close()
	rows, err = pool.Query(context.Background(), `SELECT n.nspname||'.'||p.proname, pg_get_userbyid(p.proowner) FROM pg_proc p JOIN pg_namespace n ON n.oid=p.pronamespace WHERE p.prosecdef AND `+mciSchemaFilter)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			t.Fatal(err)
		}
		c.fnOwner[k] = v
	}
	rows.Close()
	return c
}

// ---------------------------------------------------------------------------------------
// Expected §4.3 delta
// ---------------------------------------------------------------------------------------

type mciWant map[string]bool

func (w mciWant) table(role, table string, privs ...string) {
	for _, p := range privs {
		w[role+"|table|"+table+"|"+p] = true
	}
}
func (w mciWant) cols(role, table string, cols []string, privs ...string) {
	for _, c := range cols {
		for _, p := range privs {
			w[role+"|column|"+table+"."+c+"|"+p] = true
		}
	}
}
func (w mciWant) exec(role string, fns ...string) {
	for _, f := range fns {
		w[role+"|exec|"+f+"|EXECUTE"] = true
	}
}
func (w mciWant) schema(role string, schemas ...string) {
	for _, s := range schemas {
		w[role+"|schema|"+s+"|USAGE"] = true
	}
}

func mciColumns(t *testing.T, pool *pgxpool.Pool, table string, except ...string) []string {
	t.Helper()
	parts := strings.SplitN(table, ".", 2)
	rows, err := pool.Query(context.Background(), `SELECT a.attname FROM pg_attribute a JOIN pg_class c ON c.oid=a.attrelid JOIN pg_namespace n ON n.oid=c.relnamespace
		WHERE n.nspname=$1 AND c.relname=$2 AND a.attnum>0 AND NOT a.attisdropped AND a.attname<>ALL($3) ORDER BY a.attnum`, parts[0], parts[1], except)
	if err != nil {
		t.Fatal(err)
	}
	out, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil || len(out) == 0 {
		t.Fatalf("columns of %s: %v %v", table, out, err)
	}
	return out
}

func mciExpected(t *testing.T, pool *pgxpool.Pool) (want mciWant, definers map[string]string) {
	t.Helper()
	const (
		intake = "commerce_claims_intake"
		cw     = "commerce_claims_writer"
		iw     = "commerce_integration_writer"
		mw     = "commerce_meta_writer"
		mc     = "commerce_meta_consumer"
		rt     = "commerce_runtime"
		wk     = "commerce_worker"
		reg    = "commerce_meta_registrar"
		au     = "commerce_auth"
	)
	w := mciWant{}
	list := func(s ...string) []string { return s }
	// commerce_claims_intake
	w.table(intake, "claims.meta_intake", "SELECT")
	w.cols(intake, "claims.meta_intake", list("state", "drop_reason", "applied_event_id", "lease_xid", "updated_at"), "UPDATE")
	w.table(intake, "live.claim_windows", "SELECT")
	w.cols(intake, "live.claim_windows", list("updated_at"), "UPDATE")
	w.table(intake, "live.claim_window_intervals", "SELECT")
	w.cols(intake, "live.claim_sources", list("tenant_id", "store_id", "id", "session_id", "active", "private_reply"), "SELECT")
	w.table(intake, "live.offers", "SELECT")
	w.cols(intake, "live.offers", list("updated_at"), "UPDATE")
	// purged_at (0071) is readable by commerce_retention_writer only (claims-retention-purge-v1 §4).
	w.cols(intake, "claims.bundles", mciColumns(t, pool, "claims.bundles", "owner_id", "purged_at"), "SELECT")
	w.cols(intake, "claims.bundles", list("tenant_id", "store_id", "session_id", "platform", "actor_key", "label"), "INSERT")
	w.cols(intake, "claims.bundles", list("line_count", "version", "updated_at"), "UPDATE")
	w.table(intake, "claims.lines", "SELECT")
	w.cols(intake, "claims.lines", mciColumns(t, pool, "claims.lines", "applied_version"), "INSERT")
	w.cols(intake, "claims.lines", list("quantity", "version", "updated_at"), "UPDATE")
	w.table(intake, "claims.events", "SELECT", "INSERT")
	w.table(intake, "river.river_job", "SELECT", "INSERT")
	w.cols(intake, "river.river_job", list("kind"), "UPDATE")
	w["commerce_claims_intake|sequence|river.river_job_id_seq|USAGE"] = true
	w.exec(intake, "claims.lease_meta_intake", "claims.fail_meta_intake", "claims.intake_scope", "integration.claim_reply_plannable", "integration.plan_claim_reply")
	w.schema(intake, "claims", "live", "integration", "river")
	// Not a 0064 grant: the snapshot reports effective privileges, and a role created by 0064 inherits
	// PUBLIC's default USAGE on schema public (0001 revokes only CREATE). Pre-existing roles already had it.
	w.schema(intake, "public")
	// commerce_claims_writer (owner of its definers)
	w.table(cw, "claims.meta_intake", "SELECT", "INSERT")
	w.cols(cw, "claims.meta_intake", list("state", "fail_code", "attempts", "not_before", "lease_xid", "updated_at"), "UPDATE")
	w.table(cw, "live.claim_sources", "SELECT")
	w.cols(cw, "live.claim_sources", list("tenant_id", "store_id", "session_id", "platform", "binding_id", "binding_version", "object", "asset_id", "source_object_id", "private_reply", "reply_locale", "active", "version", "principal_id"), "INSERT")
	w.cols(cw, "live.claim_sources", list("intake_count", "intake_capped", "binding_id", "binding_version", "private_reply", "reply_locale", "active", "version", "principal_id", "updated_at"), "UPDATE")
	w.table(cw, "live.claim_window_intervals", "SELECT", "INSERT")
	w.cols(cw, "live.claim_window_intervals", list("closed_at"), "UPDATE")
	w.cols(cw, "integration.operations", list("id", "tenant_id", "store_id", "action", "state", "request"), "SELECT")
	w.cols(cw, "live.claim_windows", list("tenant_id", "store_id", "session_id", "state", "generation"), "SELECT")
	w.cols(cw, "claims.events", list("tenant_id", "store_id", "id", "session_id", "source_kind", "source_event_id", "outcome", "bundle_id"), "SELECT")
	w.cols(cw, "integration.bindings", list("id", "tenant_id", "store_id", "provider", "external_asset_id", "semantic_version", "enabled"), "SELECT")
	w.cols(cw, "integration.meta_page_heads", list("tenant_id", "store_id", "binding_id", "current_version"), "SELECT")
	w.cols(cw, "meta_inbox.routes", list("tenant_id", "store_id", "object", "asset_id", "binding_id", "enabled"), "SELECT")
	w.exec(cw, "identity.principal_holds")
	w.schema(cw, "integration", "meta_inbox")
	// Other roles
	w.exec(mc, "meta_inbox.stage_claim_intake")
	w.exec(mw, "claims.insert_meta_intake")
	w.schema(mw, "claims")
	w.exec(iw, "claims.issue_system_link", "claims.intake_scope")
	w.table(iw, "integration.operations", "INSERT")
	w.table(iw, "river.river_job", "SELECT")
	w.cols(iw, "live.claim_sources", list("updated_at"), "UPDATE")
	w.cols(iw, "control.storefront_publications", list("tenant_id", "store_id", "published"), "SELECT")
	w.cols(iw, "control.storefront_domains", list("id", "tenant_id", "store_id", "origin", "state"), "SELECT")
	w.table(iw, "ops.audit_events", "INSERT")
	w.schema(iw, "claims", "live", "control", "ops")
	w.table(iw, "integration.meta_page_credentials", "SELECT", "INSERT")
	w.table(iw, "integration.meta_page_heads", "SELECT", "INSERT")
	// Contract §15 wave-3 amendment (ruling i): UPDATE(current_version, updated_at) replaces UPDATE(current_version);
	// + EXECUTE identity.principal_holds and USAGE identity for commerce_integration_writer; + USAGE integration for
	// commerce_meta_registrar.
	w.cols(iw, "integration.meta_page_heads", list("current_version", "updated_at"), "UPDATE")
	w.exec(iw, "identity.principal_holds")
	w.schema(iw, "identity")
	w.schema(reg, "integration")
	w.table(rt, "live.claim_sources", "SELECT")
	w.exec(rt, "live.put_claim_source")
	w.exec(wk, "claims.check_meta_reply", "integration.load_meta_page_token")
	w.schema(wk, "claims")
	w.exec(reg, "integration.register_meta_page_token")
	// claims-retention-purge-v1 §4 rows on the §4.3 tables (§6 clause 5); "lock-only" UPDATE is a real column grant.
	const rw = "commerce_retention_writer"
	w.cols(rw, "claims.meta_intake", list("tenant_id", "store_id", "id", "inbox_event_id", "session_id", "object", "asset_id", "comment_ref", "actor_key", "state", "received_at"), "SELECT")
	w.table(rw, "claims.meta_intake", "DELETE")
	w.cols(rw, "claims.meta_intake", list("updated_at"), "UPDATE")
	w.cols(rw, "integration.operations", list("id", "tenant_id", "store_id", "action", "state", "request", "created_at"), "SELECT")
	w.cols(rw, "integration.operations", list("request", "semantic_key", "updated_at"), "UPDATE")
	w.cols(rw, "live.claim_windows", list("tenant_id", "store_id", "session_id", "state", "closed_at"), "SELECT")
	w.cols(rw, "live.claim_windows", list("updated_at"), "UPDATE")
	// Definer ownership: the owner's implicit EXECUTE is part of the effective privilege set.
	definers = map[string]string{
		"claims.insert_meta_intake": cw, "claims.lease_meta_intake": cw, "claims.fail_meta_intake": cw, "claims.intake_scope": cw, "claims.issue_system_link": cw,
		"claims.check_meta_reply": cw, "live.put_claim_source": cw, "live.track_claim_window_interval": cw,
		"meta_inbox.stage_claim_intake": mw,
		"integration.plan_claim_reply":  iw, "integration.claim_reply_plannable": iw, "integration.load_meta_page_token": iw, "integration.register_meta_page_token": iw,
		"identity.principal_holds": au,
	}
	for fn, owner := range definers {
		w[owner+"|exec|"+fn+"|EXECUTE"] = true
	}
	return w, definers
}

func mciDiff(a, b map[string]bool) (only []string) {
	for k := range a {
		if !b[k] {
			only = append(only, k)
		}
	}
	sort.Strings(only)
	return only
}

func mciHead(items []string) string {
	if len(items) > 40 {
		return strings.Join(items[:40], "\n  ") + fmt.Sprintf("\n  ... and %d more", len(items)-40)
	}
	return strings.Join(items, "\n  ")
}

// ---------------------------------------------------------------------------------------
// MCI02: upgrade + exact delta
// ---------------------------------------------------------------------------------------

// TestMetaClaimsMCI02UpgradeAndExactPrivilegeDelta is the migration gate: a populated database
// without the intake migrations upgrades (twice) to exactly the §4.3 privilege set, all new tables
// are FORCE-RLS, ownership/search_path/COMMENT rules hold, existing rows and policies are untouched
// and the OPEN window gets its backfilled interval.
func TestMetaClaimsMCI02UpgradeAndExactPrivilegeDelta(t *testing.T) {
	owner := mciStartPG(t)
	ctx := context.Background()
	mciApplyWithout(t, owner)
	t.Cleanup(func() { // the held-back migrations must also apply over the upgraded, populated database
		if _, err := owner.Exec(ctx, `DELETE FROM public.lc_schema_migrations WHERE version=ANY($1)`, mciHeldBack); err != nil {
			t.Errorf("un-record held-back migrations: %v", err)
			return
		}
		if err := migrations.Apply(ctx, owner); err != nil {
			t.Errorf("held-back migrations over the upgraded database: %v", err)
		}
	})

	// Populated database: one tenant/store/session with an OPEN generation-1 window, one CLOSED
	// generation-2 window (closed_at set), one CLOSED generation-0 window and a manual bundle.
	tenant, store, principal := randomUUID(), randomUUID(), randomUUID()
	sess := []string{randomUUID(), randomUUID(), randomUUID()}
	mustExec(t, owner, `INSERT INTO control.tenants(id,name) VALUES($1,'mci-upgrade')`, tenant)
	mustExec(t, owner, `INSERT INTO control.stores(tenant_id,id,name,currency) VALUES($1,$2,'mci-store','USD')`, tenant, store)
	mustExec(t, owner, `INSERT INTO identity.principals(id) VALUES($1)`, principal)
	mustExec(t, owner, `INSERT INTO identity.memberships(tenant_id,principal_id) VALUES($1,$2)`, tenant, principal)
	for i, s := range sess {
		mustExec(t, owner, `INSERT INTO live.sessions(id,tenant_id,store_id,principal_id,title) VALUES($1,$2,$3,$4,$5)`, s, tenant, store, principal, fmt.Sprintf("mci upgrade %d", i))
	}
	mustExec(t, owner, `INSERT INTO live.claim_windows(tenant_id,store_id,session_id,state,match_mode,generation,opened_at,principal_id) VALUES($1,$2,$3,'OPEN','EXACT',1,clock_timestamp()-interval '10 minutes',$4)`, tenant, store, sess[0], principal)
	mustExec(t, owner, `INSERT INTO live.claim_windows(tenant_id,store_id,session_id,state,match_mode,generation,opened_at,closed_at,principal_id) VALUES($1,$2,$3,'CLOSED','EXACT',2,clock_timestamp()-interval '2 hours',clock_timestamp()-interval '1 hour',$4)`, tenant, store, sess[1], principal)
	mustExec(t, owner, `INSERT INTO live.claim_windows(tenant_id,store_id,session_id,state,match_mode,generation,principal_id) VALUES($1,$2,$3,'CLOSED','EXACT',0,$4)`, tenant, store, sess[2], principal)
	mustExec(t, owner, `INSERT INTO claims.bundles(tenant_id,store_id,session_id,platform,actor_key,label) VALUES($1,$2,$3,'manual',$4,'mci label')`, tenant, store, sess[0], strings.Repeat("ab", 32))

	digest := func() map[string]string {
		return lcDigest(t, &testFixture{owner: owner}, "claims", "live", "identity", "control")
	}
	before, preCatalog := digest(), mciSnapshot(t, owner)
	// 0071 adds claims.bundles.purged_at (NULL on every existing row), which changes t::text; bundles are
	// compared on their pre-upgrade columns plus "no purged_at set" (claims-retention-purge-v1 §6 clause 5).
	bundleCols := mciColumns(t, owner, "claims.bundles", "purged_at") // pre-upgrade: no purged_at yet (non-empty except list)
	bundlesDigest := func() string {
		var d string
		if err := owner.QueryRow(ctx, `SELECT count(*)::text||':'||coalesce(md5(string_agg(r::text,E'\n' ORDER BY r::text)),'') FROM (SELECT `+
			strings.Join(bundleCols, ",")+` FROM claims.bundles) r`).Scan(&d); err != nil {
			t.Fatal(err)
		}
		return d
	}
	bundlesBefore := bundlesDigest()
	var ledgerBefore int
	if err := owner.QueryRow(ctx, `SELECT count(*) FROM public.lc_schema_migrations`).Scan(&ledgerBefore); err != nil {
		t.Fatal(err)
	}

	if err := migrations.Apply(ctx, owner); err != nil {
		t.Fatalf("first Apply of the intake migrations over a populated database: %v", err)
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
	if ledgerAfter != ledgerBefore+3 {
		t.Fatalf("ledger grew by %d, want exactly 3 (one numbered + one post-River file + the dependent 0071, once)", ledgerAfter-ledgerBefore)
	}
	if extra := append(mciDiff(post1.acl, post2.acl), mciDiff(post2.acl, post1.acl)...); len(extra) != 0 {
		t.Fatalf("the second Apply changed privileges:\n  %s", mciHead(extra))
	}

	t.Run("existing data untouched", func(t *testing.T) {
		after := digest()
		if got := bundlesDigest(); got != bundlesBefore {
			t.Errorf("claims.bundles pre-upgrade columns changed by the upgrade")
		}
		if n := countRows(t, owner, `SELECT count(*) FROM claims.bundles WHERE purged_at IS NOT NULL`); n != 0 {
			t.Errorf("the upgrade set purged_at on %d existing bundles", n)
		}
		for table, was := range before {
			if table == "claims.bundles" {
				continue // compared above on its pre-upgrade columns
			}
			if after[table] != was {
				t.Errorf("table %s changed by the upgrade", table)
			}
		}
	})

	t.Run("privilege delta equals section 4.3", func(t *testing.T) {
		want, definers := mciExpected(t, owner)
		// Named definers must have the contract owner; definers created by 0064/0014 that §4.3 does not
		// name (the deferred trigger function) may exist only with an in-scope owner and owner-only EXECUTE.
		for fn, own := range post1.fnOwner {
			if _, existed := preCatalog.fnOwner[fn]; existed {
				continue
			}
			if own == "commerce_retention_writer" {
				continue // claims-retention-purge-v1 §3 definers (0071): shape and ACL are CRP02's
			}
			if named, ok := definers[fn]; ok {
				if own != named {
					t.Errorf("definer %s is owned by %s, contract says %s", fn, own, named)
				}
				continue
			}
			switch own {
			case "commerce_integration_writer", "commerce_claims_writer", "commerce_meta_writer":
				want[own+"|exec|"+fn+"|EXECUTE"] = true
			default:
				t.Errorf("unnamed new SECURITY DEFINER %s is owned by %q", fn, own)
			}
		}
		for fn := range definers {
			if _, ok := post1.fnOwner[fn]; !ok {
				t.Errorf("contract definer %s does not exist as SECURITY DEFINER", fn)
			}
		}
		// §4.3 "fixed columns" rows: column-level SELECT only, on exactly these three tables.
		fixed := map[string]bool{"claims.meta_intake": false, "claims.events": false, "live.claim_sources": false}
		delta := mciDiff(post1.acl, preCatalog.acl)
		var filtered []string
		clause5 := map[string]bool{"claims.meta_intake": true, "integration.operations": true, "live.claim_windows": true}
		for _, fact := range delta {
			parts := strings.Split(fact, "|")
			if mciRetentionRoles[parts[0]] {
				// Only the §6 clause 5 tables belong to this delta; every other 0071 privilege is CRP02's.
				table := parts[2]
				if parts[1] == "column" {
					table = table[:strings.LastIndex(table, ".")]
				}
				if (parts[1] != "table" && parts[1] != "column") || !clause5[table] {
					continue
				}
			}
			if parts[0] == "commerce_integration_writer" && parts[1] == "column" && strings.HasSuffix(fact, "|SELECT") {
				table := parts[2][:strings.LastIndex(parts[2], ".")]
				if _, ok := fixed[table]; ok {
					fixed[table] = true
					continue
				}
			}
			filtered = append(filtered, fact)
		}
		for table, seen := range fixed {
			if !seen {
				t.Errorf("commerce_integration_writer has no new column-level SELECT on %s (contract §4.3 row)", table)
			}
		}
		for _, table := range []string{"claims.meta_intake", "claims.events", "live.claim_sources"} {
			if post1.acl["commerce_integration_writer|table|"+table+"|SELECT"] {
				t.Errorf("commerce_integration_writer holds table-level SELECT on %s (contract: fixed columns only)", table)
			}
		}
		expected := map[string]bool{}
		for k := range want {
			parts := strings.Split(k, "|")
			if parts[1] == "column" {
				// A column-level requirement is met by an equal table-level privilege (the snapshot
				// reports column facts only where the table-level privilege is absent).
				table := parts[2][:strings.LastIndex(parts[2], ".")]
				if post1.acl[parts[0]+"|table|"+table+"|"+parts[3]] {
					continue
				}
			}
			if !preCatalog.acl[k] {
				expected[k] = true
			}
		}
		gotDelta := map[string]bool{}
		for _, f := range filtered {
			gotDelta[f] = true
		}
		if missing := mciDiff(expected, gotDelta); len(missing) != 0 {
			t.Errorf("privileges required by §4.3 that the migration did not grant (%d):\n  %s", len(missing), mciHead(missing))
		}
		if extra := mciDiff(gotDelta, expected); len(extra) != 0 {
			t.Errorf("privileges granted beyond §4.3 (%d):\n  %s", len(extra), mciHead(extra))
		}
		if removed := mciDiff(preCatalog.acl, post1.acl); len(removed) != 0 {
			t.Errorf("the migration removed existing privileges (§4.3: all rows are additions):\n  %s", mciHead(removed))
		}
		if added, gone := mciDiff(post1.members, preCatalog.members), mciDiff(preCatalog.members, post1.members); len(added)+len(gone) != 0 {
			t.Errorf("role memberships changed: +%v -%v (§4.3 grants no membership)", added, gone)
		}
	})

	t.Run("RLS policies", func(t *testing.T) {
		for key, def := range preCatalog.policies {
			if post1.policies[key] != def {
				t.Errorf("existing policy %s changed or removed:\n  was %s\n  now %s", key, def, post1.policies[key])
			}
		}
		named := map[string][2]string{ // table|policy -> {command, role}; command char as pg_policy.polcmd ('*' = ALL)
			"claims.meta_intake|intake_owner_read": {"r", "commerce_claims_writer"}, "claims.links|link_system_issue": {"a", "commerce_claims_writer"},
			"live.offers|offer_system_read": {"r", "commerce_claims_writer"}, "claims.links|link_system_read": {"r", "commerce_claims_writer"},
			"live.claim_windows|window_system_read": {"r", "commerce_claims_writer"}, "claims.events|event_system_read": {"r", "commerce_claims_writer"},
			"live.claim_sources|claim_source_put": {"a", "commerce_claims_writer"}, "integration.bindings|binding_claims_read": {"r", "commerce_claims_writer"},
			"integration.operations|claim_reply_insert": {"a", "commerce_integration_writer"}, "ops.audit_events|claim_reply_audit": {"a", "commerce_integration_writer"},
			"integration.operations|no_merchant_claim_reply": {"a", "commerce_runtime"},
		}
		for key, want := range named {
			def, ok := post1.policies[key]
			if !ok {
				t.Errorf("policy %s missing", key)
				continue
			}
			parts := strings.SplitN(def, "|", 5)
			if parts[0] != want[0] || !strings.Contains(","+parts[2]+",", ","+want[1]+",") {
				t.Errorf("policy %s: command=%s roles=%s want command=%s role=%s", key, parts[0], parts[2], want[0], want[1])
			}
		}
		if def := post1.policies["integration.operations|no_merchant_claim_reply"]; !strings.Contains(def, "|false|") {
			t.Errorf("no_merchant_claim_reply must be RESTRICTIVE: %s", def)
		}
		allowed := map[string]bool{"commerce_claims_intake": true, "commerce_claims_writer": true, "commerce_integration_writer": true, "commerce_runtime": true,
			"commerce_meta_writer": true, "commerce_meta_consumer": true, "commerce_worker": true, "commerce_meta_registrar": true,
			// claims-retention-purge-v1 §4 (0071, held back with 0064): every U08 policy is TO commerce_retention_writer only.
			"commerce_retention_writer": true}
		for key, def := range post1.policies {
			if _, existed := preCatalog.policies[key]; existed {
				continue
			}
			for _, role := range strings.Split(strings.SplitN(def, "|", 5)[2], ",") {
				if !allowed[role] {
					t.Errorf("new policy %s applies to %q (§4.3 names no such role)", key, role)
				}
			}
		}
		for _, table := range []string{"claims.meta_intake", "live.claim_sources", "live.claim_window_intervals", "integration.meta_page_credentials", "integration.meta_page_heads"} {
			if got := post1.rls[table]; got != "enabled/forced" {
				t.Errorf("%s RLS = %s, want enabled/forced (§4, §7)", table, got)
			}
		}
	})

	t.Run("definer hygiene and COMMENT ON", func(t *testing.T) {
		rows, err := owner.Query(ctx, `SELECT n.nspname||'.'||p.proname,
			coalesce((SELECT bool_or(c ~ '^search_path=pg_catalog$') FROM unnest(p.proconfig) c),false),
			has_function_privilege('public',p.oid,'EXECUTE'), coalesce(obj_description(p.oid,'pg_proc'),'')
			FROM pg_proc p JOIN pg_namespace n ON n.oid=p.pronamespace WHERE p.prosecdef AND p.proowner IN
			 (SELECT oid FROM pg_roles WHERE rolname IN ('commerce_claims_writer','commerce_integration_writer','commerce_meta_writer'))`)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		seen := 0
		for rows.Next() {
			var name, comment string
			var path, public bool
			if err := rows.Scan(&name, &path, &public, &comment); err != nil {
				t.Fatal(err)
			}
			if _, existed := preCatalog.fnOwner[name]; existed {
				continue
			}
			seen++
			if !path {
				t.Errorf("%s: search_path is not fixed to pg_catalog", name)
			}
			if public {
				t.Errorf("%s: EXECUTE not revoked from PUBLIC", name)
			}
			if len(comment) < 20 {
				t.Errorf("%s: missing COMMENT ON naming the owning package and the only caller role (PROCESS.md §5)", name)
			}
		}
		if seen < 12 {
			t.Errorf("only %d new definers found, expected at least the 13 named by §4.3", seen)
		}
		var login, super, bypass bool
		if err := owner.QueryRow(ctx, `SELECT rolcanlogin,rolsuper,rolbypassrls FROM pg_roles WHERE rolname='commerce_claims_intake'`).Scan(&login, &super, &bypass); err != nil {
			t.Fatalf("role commerce_claims_intake missing: %v", err)
		}
		if login || super || bypass {
			t.Error("commerce_claims_intake must be NOLOGIN NOSUPERUSER NOBYPASSRLS")
		}
		for _, table := range []string{"claims.meta_intake", "live.claim_sources", "live.claim_window_intervals", "integration.meta_page_credentials", "integration.meta_page_heads"} {
			var tableComment string
			var undocumented int
			if err := owner.QueryRow(ctx, `SELECT coalesce(obj_description($1::regclass,'pg_class'),''),
				(SELECT count(*) FROM pg_attribute a WHERE a.attrelid=$1::regclass AND a.attnum>0 AND NOT a.attisdropped AND col_description(a.attrelid,a.attnum) IS NULL)`, table).Scan(&tableComment, &undocumented); err != nil {
				t.Fatal(err)
			}
			if tableComment == "" || undocumented != 0 {
				t.Errorf("%s: table comment %q, %d columns without COMMENT ON (PROCESS.md §5)", table, tableComment, undocumented)
			}
		}
	})

	t.Run("interval backfill and widened CHECKs", func(t *testing.T) {
		var open, closed, zero, total int
		if err := owner.QueryRow(ctx, `SELECT
			count(*) FILTER (WHERE session_id=$1 AND generation=1 AND closed_at IS NULL AND opened_at=(SELECT opened_at FROM live.claim_windows WHERE session_id=$1)),
			count(*) FILTER (WHERE session_id=$2 AND generation=2 AND closed_at=(SELECT closed_at FROM live.claim_windows WHERE session_id=$2) AND opened_at=(SELECT opened_at FROM live.claim_windows WHERE session_id=$2)),
			count(*) FILTER (WHERE session_id=$3), count(*) FROM live.claim_window_intervals WHERE tenant_id=$4`, sess[0], sess[1], sess[2], tenant).Scan(&open, &closed, &zero, &total); err != nil {
			t.Fatal(err)
		}
		if open != 1 || closed != 1 || zero != 0 || total != 2 {
			t.Fatalf("interval backfill: open=%d closed=%d generation0=%d total=%d, want 1,1,0,2", open, closed, zero, total)
		}
		exec := func(q string, args ...any) error { _, err := owner.Exec(ctx, q, args...); return err }
		bundle := func(platform string, label any) error {
			return exec(`INSERT INTO claims.bundles(tenant_id,store_id,session_id,platform,actor_key,label) VALUES($1,$2,$3,$4,$5,$6)`, tenant, store, sess[0], platform, hex.EncodeToString(randomBytes(32)), label)
		}
		if err := bundle("facebook", nil); err != nil {
			t.Errorf("platform facebook without label refused: %v", err)
		}
		if err := bundle("instagram", nil); err != nil {
			t.Errorf("platform instagram without label refused: %v", err)
		}
		if err := bundle("threads", nil); !mciSQLIs(err, "23514") {
			t.Errorf("platform threads: SQLSTATE=%s want 23514", miSQLState(err))
		}
		if err := bundle("facebook", "a label"); !mciSQLIs(err, "23514") {
			t.Errorf("a meta bundle with a label: SQLSTATE=%s want 23514 ((platform='manual')=(label IS NOT NULL))", miSQLState(err))
		}
		if err := bundle("manual", nil); !mciSQLIs(err, "23514") {
			t.Errorf("a manual bundle without a label: SQLSTATE=%s want 23514", miSQLState(err))
		}
	})
}

// ---------------------------------------------------------------------------------------
// MCI02: behaviour of policies and definers on the shared fixture
// ---------------------------------------------------------------------------------------

func mciMixedPool(t *testing.T, f *testFixture, setRole bool, roles ...string) *pgxpool.Pool {
	t.Helper()
	login := "mci_mixed_" + strings.ReplaceAll(randomUUID(), "-", "")
	password := hex.EncodeToString(randomBytes(24))
	id := pgx.Identifier{login}.Sanitize()
	mustExec(t, f.owner, `CREATE ROLE `+id+` LOGIN NOSUPERUSER NOBYPASSRLS NOCREATEDB NOCREATEROLE NOREPLICATION PASSWORD '`+password+`'`)
	for _, r := range roles {
		mustExec(t, f.owner, `GRANT `+pgx.Identifier{r}.Sanitize()+` TO `+id+` WITH INHERIT TRUE, SET `+map[bool]string{true: "TRUE", false: "FALSE"}[setRole])
	}
	t.Cleanup(func() {
		for _, r := range roles {
			_, _ = f.owner.Exec(context.Background(), `REVOKE `+pgx.Identifier{r}.Sanitize()+` FROM `+id)
		}
		_, _ = f.owner.Exec(context.Background(), `DROP ROLE `+id)
	})
	pool, err := pgxpool.New(context.Background(), roleURL(t, f.databaseURL, login, password))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// TestMetaClaimsMCI02PoolValidator: the dedicated intake login passes; mixed logins, SET ROLE and
// runtime logins are rejected, and every other pool validator rejects a login that can reach the
// intake role (both directions, §4.1).
func TestMetaClaimsMCI02PoolValidator(t *testing.T) {
	f := fixture(t)
	ctx := context.Background()
	dedicated, err := platform.OpenClaimsIntakePool(ctx, miRole(t, f, "commerce_claims_intake"))
	if err != nil {
		t.Fatalf("dedicated intake login refused: %v", err)
	}
	defer dedicated.Close()
	if err := platform.ValidateClaimsIntakePool(ctx, dedicated); err != nil {
		t.Fatalf("ValidateClaimsIntakePool refused the dedicated login: %v", err)
	}
	for name, pool := range map[string]*pgxpool.Pool{
		"intake + worker":             mciMixedPool(t, f, false, "commerce_claims_intake", "commerce_worker"),
		"intake + meta consumer":      mciMixedPool(t, f, false, "commerce_claims_intake", "commerce_meta_consumer"),
		"intake + runtime":            mciMixedPool(t, f, false, "commerce_claims_intake", "commerce_runtime"),
		"intake + integration writer": mciMixedPool(t, f, false, "commerce_claims_intake", "commerce_integration_writer"),
		"intake with SET ROLE":        mciMixedPool(t, f, true, "commerce_claims_intake"),
		"another authority (worker)":  mciMixedPool(t, f, false, "commerce_worker"),
		"the runtime pool":            f.runtime,
		"the owner pool":              f.owner,
	} {
		if err := platform.ValidateClaimsIntakePool(ctx, pool); err == nil {
			t.Errorf("ValidateClaimsIntakePool accepted: %s", name)
		}
	}
	// Reverse direction: a login that can also reach the intake role fails every other validator.
	workerMix := mciMixedPool(t, f, false, "commerce_worker", "commerce_claims_intake")
	if err := platform.ValidateWorkerPool(ctx, workerMix); err == nil {
		t.Error("ValidateWorkerPool accepted a worker login that also holds commerce_claims_intake")
	}
	consumerMix := mciMixedPool(t, f, false, "commerce_meta_consumer", "commerce_claims_intake")
	if err := platform.ValidateMetaConsumerPool(ctx, consumerMix); err == nil {
		t.Error("ValidateMetaConsumerPool accepted a consumer login that also holds commerce_claims_intake")
	}
	checkoutMix := mciMixedPool(t, f, false, "commerce_checkout_runtime", "commerce_claims_intake")
	if err := platform.ValidateCheckoutPool(ctx, checkoutMix); err == nil {
		t.Error("ValidateCheckoutPool accepted a checkout login that also holds commerce_claims_intake")
	}
	if _, err := platform.OpenClaimsIntakePool(ctx, roleURL(t, f.databaseURL, "no_such_login", "x")); err == nil {
		t.Error("OpenClaimsIntakePool opened a nonexistent login")
	}
}

// TestMetaClaimsMCI02PolicyBehaviour exercises the RLS rows of §4.3 from their real roles and GUC
// states: the intake login sees and writes nothing without a leased row even with forged GUCs; the
// NOGUC system policies of commerce_claims_writer return zero rows in any tenant-scoped transaction;
// link_system_issue is INSERT-only, generation 1, system-only; commerce_runtime cannot insert a
// meta.private_reply operation; commerce_meta_consumer holds no claims/live/river table privilege.
func TestMetaClaimsMCI02PolicyBehaviour(t *testing.T) {
	e := mciSetup(t, mciOpts{private: true})
	f := e.h.f
	ctx := context.Background()
	first := e.planReply(t, false, "", "A1") // one applied bundle with a system link and an operation
	if first.op == "" {
		t.Fatal("no reply planned")
	}
	pending := e.postFB(t, "", "", "A1", mciAt(3*time.Second), nil) // one PENDING row to lease
	pendingRow := e.fbIntake(t, pending)

	t.Run("intake login without a leased row sees and writes nothing, even with forged GUCs", func(t *testing.T) {
		tx, err := e.intakePool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback(ctx)
		if _, err := tx.Exec(ctx, `SELECT set_config('app.tenant_id',$1,true),set_config('app.store_id',$2,true)`, f.tenantA, f.storeA1); err != nil {
			t.Fatal(err)
		}
		for table, col := range map[string]string{"claims.meta_intake": "id", "live.claim_sources": "id", "live.claim_windows": "session_id", "live.claim_window_intervals": "session_id",
			"live.offers": "id", "claims.bundles": "id", "claims.lines": "bundle_id", "claims.events": "id"} {
			var n int
			if err := tx.QueryRow(ctx, `SELECT count(`+col+`) FROM `+table).Scan(&n); err != nil {
				t.Fatalf("read %s: %v", table, err)
			}
			if n != 0 {
				t.Errorf("intake login read %d rows of %s without a leased row", n, table)
			}
		}
		tag, err := tx.Exec(ctx, `UPDATE claims.meta_intake SET updated_at=clock_timestamp()`)
		if err != nil || tag.RowsAffected() != 0 {
			t.Errorf("intake login updated %d meta_intake rows without a lease (err=%v)", tag.RowsAffected(), err)
		}
		if _, err := tx.Exec(ctx, `SAVEPOINT b`); err != nil {
			t.Fatal(err)
		}
		if _, err := tx.Exec(ctx, `INSERT INTO claims.bundles(tenant_id,store_id,session_id,platform,actor_key) VALUES($1,$2,$3,'facebook',$4)`, f.tenantA, f.storeA1, e.session, strings.Repeat("c", 64)); !mciSQLIs(err, "42501") {
			t.Errorf("forged-GUC bundle insert SQLSTATE=%s want 42501", miSQLState(err))
		}
	})

	t.Run("with a lease the login sees exactly the leased scope", func(t *testing.T) {
		tx, id := e.leaseTx(t, pgx.ReadCommitted, true)
		if id != pendingRow.ID {
			t.Fatalf("leased %s, expected the staged row %s", id, pendingRow.ID)
		}
		var windows, offers, sources, intakes int
		if err := tx.QueryRow(ctx, `SELECT (SELECT count(session_id) FROM live.claim_windows),(SELECT count(id) FROM live.offers),(SELECT count(id) FROM live.claim_sources),(SELECT count(id) FROM claims.meta_intake)`).Scan(&windows, &offers, &sources, &intakes); err != nil {
			t.Fatal(err)
		}
		if windows != 1 || offers < 1 || sources < 1 || intakes != 1 {
			t.Errorf("leased scope: windows=%d offers=%d sources=%d intakes=%d", windows, offers, sources, intakes)
		}
		// live.claim_sources column grant: private_reply readable, principal_id is not.
		if _, err := tx.Exec(ctx, `SAVEPOINT c`); err != nil {
			t.Fatal(err)
		}
		if _, err := tx.Exec(ctx, `SELECT principal_id FROM live.claim_sources`); !mciSQLIs(err, "42501") {
			t.Errorf("intake login read live.claim_sources.principal_id: SQLSTATE=%s want 42501", miSQLState(err))
		}
	})

	t.Run("NOGUC system policies never widen a tenant-scoped transaction", func(t *testing.T) {
		count := func(guc bool, tenant string, table, col string) int {
			tx, err := f.owner.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback(ctx)
			if _, err := tx.Exec(ctx, `SET LOCAL ROLE commerce_claims_writer`); err != nil {
				t.Fatal(err)
			}
			if guc {
				if _, err := tx.Exec(ctx, `SELECT set_config('app.tenant_id',$1,true),set_config('app.store_id',$2,true)`, tenant, f.storeA1); err != nil {
					t.Fatal(err)
				}
			}
			var n int
			if err := tx.QueryRow(ctx, `SELECT count(`+col+`) FROM `+table).Scan(&n); err != nil {
				t.Fatalf("%s as commerce_claims_writer: %v", table, err)
			}
			return n
		}
		for table, col := range map[string]string{"claims.links": "bundle_id", "live.offers": "id", "live.claim_windows": "session_id"} {
			if count(false, "", table, col) < 1 {
				t.Errorf("%s: the NOGUC policy shows no rows to commerce_claims_writer with all GUCs unset", table)
			}
			if n := count(true, randomUUID(), table, col); n != 0 {
				t.Errorf("%s: %d rows visible in a transaction scoped to a foreign tenant (NOGUC policy must not apply)", table, n)
			}
		}
	})

	t.Run("claims.links: system issue is INSERT-only, generation 1, no principal", func(t *testing.T) {
		tx, err := f.owner.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback(ctx)
		// Lease the pending row inside this transaction (owner SQL), then act as the definer's owner.
		if _, err := tx.Exec(ctx, `UPDATE claims.meta_intake SET lease_xid=pg_current_xact_id() WHERE id=$1`, pendingRow.ID); err != nil {
			t.Fatal(err)
		}
		var bundle string
		if err := tx.QueryRow(ctx, `INSERT INTO claims.bundles(tenant_id,store_id,session_id,platform,actor_key) VALUES($1,$2,$3,'facebook',$4) RETURNING id::text`, f.tenantA, f.storeA1, e.session, strings.Repeat("9", 64)).Scan(&bundle); err != nil {
			t.Fatal(err)
		}
		if _, err := tx.Exec(ctx, `SET LOCAL ROLE commerce_claims_writer`); err != nil {
			t.Fatal(err)
		}
		set := func(principal string) {
			if _, err := tx.Exec(ctx, `SELECT set_config('app.tenant_id',$1,true),set_config('app.store_id',$2,true),set_config('app.principal_id',$3,true),set_config('app.buyer_id','',true)`, f.tenantA, f.storeA1, principal); err != nil {
				t.Fatal(err)
			}
		}
		insert := func(generation int) error {
			if _, err := tx.Exec(ctx, `SAVEPOINT s`); err != nil {
				t.Fatal(err)
			}
			_, err := tx.Exec(ctx, `INSERT INTO claims.links(tenant_id,store_id,bundle_id,token_hash,generation,issued_at,expires_at,principal_id) VALUES($1,$2,$3,decode(repeat('ab',32),'hex'),$4,clock_timestamp(),clock_timestamp()+interval '1 hour',$5)`,
				f.tenantA, f.storeA1, bundle, generation, e.h.actor)
			if err != nil {
				if _, e2 := tx.Exec(ctx, `ROLLBACK TO s`); e2 != nil {
					t.Fatal(e2)
				}
			}
			return err
		}
		set("")
		if err := insert(2); !mciSQLIs(err, "42501") {
			t.Errorf("system insert of generation 2: SQLSTATE=%s want 42501 (WITH CHECK generation=1)", miSQLState(err))
		}
		// A principal GUC that is not the row's principal: the pre-existing merchant policy link_issue (0060,
		// principal_id = app.principal_id) cannot admit the row, so only link_system_issue could, and it must
		// not while app.principal_id is set.
		set(randomUUID())
		if err := insert(1); !mciSQLIs(err, "42501") {
			t.Errorf("insert with app.principal_id set: SQLSTATE=%s want 42501 (SYSTEM only)", miSQLState(err))
		}
		set("")
		if err := insert(1); err != nil {
			t.Fatalf("the sanctioned system insert of generation 1 failed: %v", err)
		}
		tag, err := tx.Exec(ctx, `UPDATE claims.links SET generation=generation+1 WHERE bundle_id=$1`, bundle)
		if err == nil && tag.RowsAffected() != 0 {
			t.Errorf("an intake-context UPDATE of claims.links affected %d rows: no rotate/release path may exist for SYSTEM", tag.RowsAffected())
		}
	})

	t.Run("commerce_runtime cannot insert a meta.private_reply operation", func(t *testing.T) {
		cols := lcStrings(t, f.owner, `SELECT column_name FROM information_schema.columns WHERE table_schema='integration' AND table_name='operations' AND is_generated='NEVER' ORDER BY ordinal_position`)
		var sel []string
		for _, c := range cols {
			switch c {
			case "id":
				sel = append(sel, "gen_random_uuid()")
			case "semantic_key":
				sel = append(sel, "'mpr:'||substr(md5(random()::text),1,20)")
			default:
				sel = append(sel, c)
			}
		}
		err := platform.WithScope(ctx, f.runtime, e.h.token, f.storeA1, "store:read", func(tx pgx.Tx, _ platform.Scope) error {
			_, err := tx.Exec(ctx, `INSERT INTO integration.operations(`+strings.Join(cols, ",")+`) SELECT `+strings.Join(sel, ",")+` FROM integration.operations WHERE id=$1`, first.op)
			return err
		})
		if err == nil || !strings.Contains(err.Error(), "row-level security") {
			t.Fatalf("a merchant transaction inserted (or failed differently on) a meta.private_reply operation: %v", err)
		}
	})

	t.Run("commerce_meta_consumer holds no claims, live or river table privilege", func(t *testing.T) {
		rows := lcStrings(t, f.owner, `SELECT n.nspname||'.'||c.relname||':'||p.priv FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace
			CROSS JOIN (VALUES ('SELECT'),('INSERT'),('UPDATE'),('DELETE'),('TRUNCATE'),('REFERENCES'),('TRIGGER')) p(priv)
			WHERE c.relkind IN ('r','p') AND n.nspname IN ('claims','live','river') AND has_table_privilege('commerce_meta_consumer',c.oid,p.priv)`)
		if len(rows) != 0 {
			t.Fatalf("commerce_meta_consumer holds table privileges: %v", rows)
		}
		cols := lcStrings(t, f.owner, `SELECT n.nspname||'.'||c.relname||'.'||a.attname FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace JOIN pg_attribute a ON a.attrelid=c.oid AND a.attnum>0 AND NOT a.attisdropped
			WHERE c.relkind IN ('r','p') AND n.nspname IN ('claims','live','river') AND (has_column_privilege('commerce_meta_consumer',c.oid,a.attnum,'SELECT') OR has_column_privilege('commerce_meta_consumer',c.oid,a.attnum,'UPDATE') OR has_column_privilege('commerce_meta_consumer',c.oid,a.attnum,'INSERT'))`)
		if len(cols) != 0 {
			t.Fatalf("commerce_meta_consumer holds column privileges: %v", cols)
		}
	})

	t.Run("check_meta_reply from the worker pool (no GUCs) sees the link and the window", func(t *testing.T) {
		pool := miPool(t, f, "commerce_worker")
		token := e.token(t, first)
		hash := sha256.Sum256([]byte(token))
		var code string
		if err := pool.QueryRow(ctx, `SELECT claims.check_meta_reply($1::uuid,$2)`, first.op, hash[:]).Scan(&code); err != nil || code != "OK" {
			t.Fatalf("check_meta_reply = %q err=%v, want OK", code, err)
		}
		bad := sha256.Sum256([]byte("not the token"))
		if err := pool.QueryRow(ctx, `SELECT claims.check_meta_reply($1::uuid,$2)`, first.op, bad[:]).Scan(&code); err != nil || code != "link_invalid" {
			t.Fatalf("wrong hash: %q err=%v, want link_invalid", code, err)
		}
		// Buyer- or merchant-shaped GUC state is refused by the definer (asserts NOGUC).
		tx, err := pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback(ctx)
		if _, err := tx.Exec(ctx, `SELECT set_config('app.tenant_id',$1,true)`, f.tenantA); err != nil {
			t.Fatal(err)
		}
		if err := tx.QueryRow(ctx, `SELECT claims.check_meta_reply($1::uuid,$2)`, first.op, hash[:]).Scan(&code); !mciSQLIs(err, "22023") {
			t.Errorf("check_meta_reply with app.tenant_id set: SQLSTATE=%s want 22023 (asserts NOGUC)", miSQLState(err))
		}
	})

	t.Run("CHECK widening: meta events carry no principal, RATE_LIMITED is meta-only", func(t *testing.T) {
		insert := func(kind, platform string, principal any) error {
			_, err := f.owner.Exec(ctx, `INSERT INTO claims.events(tenant_id,store_id,session_id,window_generation,source_kind,source_event_id,platform,occurred_at,grammar_version,grammar_kind,match_mode,outcome,reason,offer_id,quantity,explicit_quantity,principal_id)
				VALUES($1,$2,$3,1,$4,$5,$6,clock_timestamp(),'kw-v1','MATCH','EXACT','REJECTED','RATE_LIMITED',$7,1,false,$8)`, f.tenantA, f.storeA1, e.session, kind, randomUUID(), platform, e.offer.ID, principal)
			return err
		}
		if err := insert("meta", "facebook", nil); err != nil {
			t.Errorf("a meta RATE_LIMITED event without principal refused: %v", err)
		}
		if err := insert("manual", "manual", e.h.actor); !mciSQLIs(err, "23514") {
			t.Errorf("a manual RATE_LIMITED event: SQLSTATE=%s want 23514", miSQLState(err))
		}
		if err := insert("meta", "facebook", e.h.actor); !mciSQLIs(err, "23514") {
			t.Errorf("a meta event with a principal: SQLSTATE=%s want 23514", miSQLState(err))
		}
	})
}

// ---------------------------------------------------------------------------------------
// MCI03: live.put_claim_source
// ---------------------------------------------------------------------------------------

func mciForbidden(err error) bool { return mciSQLIs(err, "PT403", "42501") }

// TestMetaClaimsMCI03PutClaimSource: route binding, provider mapping, single-owner-of-an-object under
// concurrency, permissions, credential requirement for private_reply, CAS and validation.
func TestMetaClaimsMCI03PutClaimSource(t *testing.T) {
	e := mciSetup(t, mciOpts{})
	f := e.h.f
	ctx := context.Background()
	row := func(id string) (platform, object, asset, oid, locale string, private, active bool, version int64, binding string, bindingVersion int64, principal string) {
		if err := f.owner.QueryRow(ctx, `SELECT platform,object,asset_id,source_object_id,reply_locale,private_reply,active,version,binding_id::text,binding_version,principal_id::text FROM live.claim_sources WHERE id=$1`, id).
			Scan(&platform, &object, &asset, &oid, &locale, &private, &active, &version, &binding, &bindingVersion, &principal); err != nil {
			t.Fatal(err)
		}
		return
	}

	t.Run("provider and binding mapping", func(t *testing.T) {
		for _, c := range []struct{ id, platform, object, asset, binding string }{
			{e.srcFB, "facebook", "page", e.pageAsset, e.pageBinding}, {e.srcIG, "instagram", "instagram", e.igAsset, e.igBinding}} {
			platform, object, asset, _, locale, private, active, version, binding, bindingVersion, principal := row(c.id)
			var current int64
			if err := f.owner.QueryRow(ctx, `SELECT semantic_version FROM integration.bindings WHERE id=$1`, c.binding).Scan(&current); err != nil {
				t.Fatal(err)
			}
			if platform != c.platform || object != c.object || asset != c.asset || binding != c.binding || bindingVersion != current || locale != "zh-TW" || private || !active || version < 1 || principal != e.h.actor {
				t.Fatalf("source row: platform=%s object=%s asset=%s binding=%s bv=%d/%d locale=%s private=%t active=%t version=%d principal=%s", platform, object, asset, binding, bindingVersion, current, locale, private, active, version, principal)
			}
		}
	})

	t.Run("route mismatch is a conflict", func(t *testing.T) {
		oid := e.pageAsset + "_" + mciDigits(10)
		for name, args := range map[string][3]string{
			"an asset with no route":          {"page", mciDigits(14), oid},
			"a Page asset presented as IG":    {"instagram", e.pageAsset, oid},
			"an IG asset presented as a Page": {"page", e.igAsset, oid},
		} {
			if _, err := e.putSource(e.session, args[0], args[1], args[2], false, "zh-TW", true, 0); !mciConflict(err) {
				t.Errorf("%s: SQLSTATE=%s err=%v want conflict", name, miSQLState(err), err)
			}
		}
		// A disabled route is not a route.
		var route string
		if err := f.owner.QueryRow(ctx, `SELECT id::text FROM meta_inbox.routes WHERE app_id=$1 AND object='page' AND asset_id=$2`, miApp, e.pageAsset).Scan(&route); err != nil {
			t.Fatal(err)
		}
		mustExec(t, f.owner, `UPDATE meta_inbox.routes SET enabled=false WHERE id=$1`, route)
		_, err := e.putSource(e.session, "page", e.pageAsset, e.pageAsset+"_"+mciDigits(10), false, "zh-TW", true, 0)
		mustExec(t, f.owner, `UPDATE meta_inbox.routes SET enabled=true WHERE id=$1`, route)
		if !mciConflict(err) {
			t.Errorf("a disabled route accepted a source: %v", err)
		}
	})

	t.Run("validation", func(t *testing.T) {
		valid := e.pageAsset + "_" + mciDigits(10)
		for name, c := range map[string]struct {
			oid, locale string
		}{"non-numeric object id": {"abc_def", "zh-TW"}, "empty object id": {"", "zh-TW"}, "81-char object id": {strings.Repeat("1", 81), "zh-TW"}, "unknown locale": {valid, "fr"}} {
			if _, err := e.putSource(e.session, "page", e.pageAsset, c.oid, false, c.locale, true, 0); err == nil || mciForbidden(err) {
				t.Errorf("%s accepted or misclassified: %v", name, err)
			}
		}
		if _, err := e.putSource(e.session, "threads", e.pageAsset, valid, false, "zh-TW", true, 0); err == nil {
			t.Error("object 'threads' accepted")
		}
		for _, locale := range []string{"zh-TW", "zh-CN", "en"} {
			if _, err := e.putSource(e.session, "page", e.pageAsset, e.pageAsset+"_"+mciDigits(10), false, locale, true, 0); err != nil {
				t.Errorf("locale %s refused: %v", locale, err)
			}
		}
	})

	t.Run("permissions live:manage and integration:execute", func(t *testing.T) {
		oid := e.pageAsset + "_" + mciDigits(10)
		for name, perms := range map[string][]string{
			"live:manage without integration:execute": {"store:read", "live:read", "live:manage"},
			"integration:execute without live:manage": {"store:read", "live:read", "integration:execute"},
			"neither": {"store:read"},
		} {
			_, token := lcPrincipal(t, f, f.tenantA, []string{f.storeA1}, perms...)
			err := platform.WithScope(ctx, f.runtime, token, f.storeA1, "store:read", func(tx pgx.Tx, _ platform.Scope) error {
				var id string
				return tx.QueryRow(ctx, `SELECT live.put_claim_source($1::uuid,'page',$2,$3,false,'zh-TW',true,0)::text`, e.session, e.pageAsset, oid).Scan(&id)
			})
			if !mciForbidden(err) {
				t.Errorf("%s: SQLSTATE=%s err=%v want forbidden", name, miSQLState(err), err)
			}
		}
		if n := miCount(t, f.owner, `SELECT count(*) FROM live.claim_sources WHERE object='page' AND asset_id=$1 AND source_object_id=$2`, e.pageAsset, oid); n != 0 {
			t.Fatal("a forbidden call created a source row")
		}
	})

	t.Run("private_reply requires a current Page-token credential", func(t *testing.T) {
		oid := e.pageAsset + "_" + mciDigits(10)
		if _, err := e.putSource(e.session, "page", e.pageAsset, oid, true, "zh-TW", true, 0); err == nil {
			t.Fatal("private_reply=true accepted for a binding without a credential")
		}
		if n := miCount(t, f.owner, `SELECT count(*) FROM live.claim_sources WHERE object='page' AND asset_id=$1 AND source_object_id=$2`, e.pageAsset, oid); n != 0 {
			t.Fatal("a refused private_reply source left a row")
		}
		if _, err := e.putSource(e.session, "page", e.pageAsset, oid, false, "zh-TW", true, 0); err != nil {
			t.Fatalf("private_reply=false needs no credential: %v", err)
		}
	})

	t.Run("CAS on version, principal follows the caller, deactivation frees the object", func(t *testing.T) {
		oid := e.pageAsset + "_" + mciDigits(10)
		id, err := e.putSource(e.session, "page", e.pageAsset, oid, false, "zh-TW", true, 0)
		if err != nil {
			t.Fatal(err)
		}
		_, _, _, _, _, _, _, v1, _, _, _ := row(id)
		if _, err := e.putSource(e.session, "page", e.pageAsset, oid, false, "en", true, v1+5); !mciConflict(err) {
			t.Errorf("stale expected_version: SQLSTATE=%s want conflict", miSQLState(err))
		}
		if _, err := e.putSource(e.session, "page", e.pageAsset, oid, false, "en", true, 0); !mciConflict(err) {
			t.Errorf("expected_version 0 on an existing source: SQLSTATE=%s want conflict", miSQLState(err))
		}
		same, err := e.putSource(e.session, "page", e.pageAsset, oid, false, "en", true, v1)
		if err != nil || same != id {
			t.Fatalf("update with the current version: id=%s err=%v (must update the same source)", same, err)
		}
		_, _, _, _, locale, _, _, v2, _, _, principal := row(id)
		if locale != "en" || v2 != v1+1 || principal != e.h.actor {
			t.Fatalf("after update: locale=%s version=%d (was %d) principal=%s", locale, v2, v1, principal)
		}
		if _, err := e.putSource(e.session, "page", e.pageAsset, oid, false, "en", false, v2); err != nil {
			t.Fatalf("deactivate: %v", err)
		}
		if n := miCount(t, f.owner, `SELECT count(*) FROM live.claim_sources WHERE object='page' AND asset_id=$1 AND source_object_id=$2 AND active`, e.pageAsset, oid); n != 0 {
			t.Fatal("deactivation left an active source")
		}
	})

	t.Run("one object maps to one session under concurrency", func(t *testing.T) {
		// A second session with its own window row (one OPEN window per store: close the first).
		e.h.closeWindow(t, e.session)
		second := e.h.draft(t, f.storeA1)
		e.h.open(t, second, claims.MatchExact)
		t.Cleanup(func() {
			_, _ = f.owner.Exec(context.Background(), `DELETE FROM live.claim_sources WHERE session_id=$1`, second)
			_, _ = f.owner.Exec(context.Background(), `DELETE FROM live.claim_window_intervals WHERE session_id=$1`, second)
		})
		oid := e.pageAsset + "_" + mciDigits(10)
		results := make(chan error, 2)
		var wg sync.WaitGroup
		for _, s := range []string{e.session, second} {
			wg.Add(1)
			go func(session string) {
				defer wg.Done()
				_, err := e.putSource(session, "page", e.pageAsset, oid, false, "zh-TW", true, 0)
				results <- err
			}(s)
		}
		wg.Wait()
		close(results)
		ok, conflicts := 0, 0
		for err := range results {
			switch {
			case err == nil:
				ok++
			case mciConflict(err):
				conflicts++
			default:
				t.Errorf("unexpected error: SQLSTATE=%s %v", miSQLState(err), err)
			}
		}
		if ok != 1 || conflicts != 1 {
			t.Fatalf("concurrent claim of one object: %d succeeded, %d conflicted; want 1 and 1", ok, conflicts)
		}
		if n := miCount(t, f.owner, `SELECT count(*) FROM live.claim_sources WHERE object='page' AND asset_id=$1 AND source_object_id=$2 AND active`, e.pageAsset, oid); n != 1 {
			t.Fatalf("%d active sources for one object", n)
		}
	})
}
