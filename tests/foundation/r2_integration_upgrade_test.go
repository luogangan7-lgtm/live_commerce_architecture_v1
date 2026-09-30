package foundation_test

import (
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"testing"

	"livecommerce/migrations"
)

// TestR2IntegrationUpgradeFromReleaseHead is the R2 integration gate for the merged migration set: the six R2
// lanes each proved their own migration on their own branch; this proves all of them together, in ledger
// (numeric) order, as an UPGRADE of the release-head schema (numbered <= 0066, post-River <= 0014, commit 10c43ad)
// and that the result is the same schema a fresh database gets.
//
// It never asserts any lane's objects (those gates stay with the lanes); it fails when an R2 migration depends on
// something a later-numbered file creates, or is not re-runnable/idempotent through migrations.Apply.
func TestR2IntegrationUpgradeFromReleaseHead(t *testing.T) {
	ctx := context.Background()
	var r2 []string
	numbered, _ := filepath.Glob("../../migrations/[0-9][0-9][0-9][0-9]_*.sql")
	post, _ := filepath.Glob("../../migrations/post_river/[0-9][0-9][0-9][0-9]_*.sql")
	for _, path := range numbered {
		if base := filepath.Base(path); base >= "0070" {
			r2 = append(r2, base)
		}
	}
	for _, path := range post {
		if base := filepath.Base(path); base >= "0015" {
			r2 = append(r2, "post_river/"+base)
		}
	}
	sort.Strings(r2)
	// 0070..0080 without 0076 (never allocated) + post-River 0015..0017: a lane that drops or adds a file must update this.
	if len(r2) != 13 {
		t.Fatalf("R2 migration set = %d files %v, want 13", len(r2), r2)
	}

	upgraded := mciStartPG(t)
	mustExec(t, upgraded, `CREATE TABLE public.lc_schema_migrations (version text PRIMARY KEY, checksum text NOT NULL, applied_at timestamptz NOT NULL DEFAULT now())`)
	for _, version := range r2 {
		body, err := os.ReadFile(filepath.Join("../../migrations", version))
		if err != nil {
			t.Fatal(err)
		}
		mustExec(t, upgraded, `INSERT INTO public.lc_schema_migrations(version,checksum) VALUES($1,$2)`, version, fmt.Sprintf("%x", sha256.Sum256(body)))
	}
	if err := migrations.Apply(ctx, upgraded); err != nil {
		t.Fatalf("release-head schema (everything but the R2 files): %v", err)
	}
	if n := countRows(t, upgraded, `SELECT count(*) FROM pg_namespace WHERE nspname IN ('ads','billing','customers')`); n != 0 {
		t.Fatalf("release-head database already has %d R2 schemas", n)
	}
	mustExec(t, upgraded, `DELETE FROM public.lc_schema_migrations WHERE version=ANY($1)`, r2)
	for i := 0; i < 2; i++ { // the second Apply must be a no-op
		if err := migrations.Apply(ctx, upgraded); err != nil {
			t.Fatalf("R2 upgrade apply %d: %v", i+1, err)
		}
	}
	if n := countRows(t, upgraded, `SELECT count(*) FROM public.lc_schema_migrations WHERE version=ANY($1)`, r2); n != len(r2) {
		t.Fatalf("ledger has %d of %d R2 files after the upgrade", n, len(r2))
	}

	fresh := mciStartPG(t)
	if err := migrations.Apply(ctx, fresh); err != nil {
		t.Fatalf("fresh apply: %v", err)
	}
	// ACL items are compared as sets: an upgrade grants in a different order than a fresh apply (post-River before the R2 files).
	// Same objects either way: relations+columns, functions (with body hash), policies, triggers, table/function ACLs, roles.
	const catalog = `SELECT x FROM (
		SELECT 'col '||c.oid::regclass::text||' '||a.attname||' '||format_type(a.atttypid,a.atttypmod)||' '||a.attnotnull::text||' '||coalesce((SELECT string_agg(i::text, ',' ORDER BY i::text) FROM unnest(c.relacl) i),'')||' '||coalesce((SELECT string_agg(i::text, ',' ORDER BY i::text) FROM unnest(a.attacl) i),'')||' '||c.relrowsecurity::text||c.relforcerowsecurity::text AS x
		  FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace JOIN pg_attribute a ON a.attrelid=c.oid AND a.attnum>0 AND NOT a.attisdropped
		  WHERE n.nspname NOT IN ('pg_catalog','information_schema','pg_toast') AND c.relkind IN ('r','p','v','m')
		UNION ALL SELECT 'fn '||p.oid::regprocedure::text||' '||md5(p.prosrc)||' '||p.prosecdef::text||' '||pg_get_userbyid(p.proowner)||' '||coalesce((SELECT string_agg(i::text, ',' ORDER BY i::text) FROM unnest(p.proacl) i),'')||' '||coalesce(p.proconfig::text,'')
		  FROM pg_proc p JOIN pg_namespace n ON n.oid=p.pronamespace WHERE n.nspname NOT IN ('pg_catalog','information_schema')
		UNION ALL SELECT 'pol '||polrelid::regclass::text||' '||polname||' '||polcmd::text||' '||coalesce(pg_get_expr(polqual,polrelid),'')||' '||coalesce(pg_get_expr(polwithcheck,polrelid),'')||' '||polroles::regrole[]::text FROM pg_policy
		UNION ALL SELECT 'trg '||tgrelid::regclass::text||' '||tgname||' '||tgfoid::regprocedure::text||' '||tgtype::text||' '||tgenabled::text FROM pg_trigger WHERE NOT tgisinternal
		UNION ALL SELECT 'con '||conrelid::regclass::text||' '||conname||' '||pg_get_constraintdef(oid) FROM pg_constraint WHERE conrelid<>0
		UNION ALL SELECT 'idx '||indexrelid::regclass::text||' '||pg_get_indexdef(indexrelid) FROM pg_index i JOIN pg_class c ON c.oid=i.indrelid JOIN pg_namespace n ON n.oid=c.relnamespace WHERE n.nspname NOT IN ('pg_catalog','information_schema','pg_toast')
		UNION ALL SELECT 'role '||rolname||' '||rolcanlogin::text||rolinherit::text||rolbypassrls::text FROM pg_roles WHERE rolname LIKE 'commerce\_%'
		UNION ALL SELECT 'member '||roleid::regrole::text||' '||member::regrole::text FROM pg_auth_members WHERE roleid::regrole::text LIKE 'commerce\_%'
		UNION ALL SELECT 'ns '||nspname||' '||coalesce((SELECT string_agg(i::text, ',' ORDER BY i::text) FROM unnest(nspacl) i),'') FROM pg_namespace WHERE nspname NOT LIKE 'pg\_%' AND nspname<>'information_schema'
	) s ORDER BY x`
	a, b := lcStrings(t, upgraded, catalog), lcStrings(t, fresh, catalog)
	seen := map[string]int{}
	for _, x := range a {
		seen[x]++
	}
	for _, x := range b {
		seen[x]--
	}
	var diff []string
	for x, n := range seen {
		if n != 0 {
			diff = append(diff, fmt.Sprintf("%+d %s", n, x)) // +1 only after the upgrade, -1 only on fresh
		}
	}
	sort.Strings(diff)
	if len(diff) != 0 || len(a) < 1000 {
		t.Fatalf("upgraded catalog (%d rows) differs from fresh (%d rows) in %d rows; first: %.2000v", len(a), len(b), len(diff), diff[:min(len(diff), 12)])
	}
}
