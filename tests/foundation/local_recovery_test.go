package foundation_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"
	"livecommerce/internal/platform"
	"livecommerce/migrations"
)

const lrImage = "postgres@sha256:4ef4dbc939d61acea57712655ddb4b4ab27419c913f94cca0cd57cb3ea3c2280"

type lrCluster struct {
	id, name, role, password, port, dsn string
	owner                               *pgxpool.Pool
}

func lrCommand(t *testing.T, timeout time.Duration, env []string, input io.Reader, output io.Writer, name string, args ...string) []byte {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Env = append(os.Environ(), env...)
	cmd.Stdin, cmd.Stdout = input, output
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if output == nil {
		var stdout bytes.Buffer
		cmd.Stdout = &stdout
		if err := cmd.Run(); err != nil {
			lrCommandFailure(t, name, args, err, stderr.Bytes())
		}
		return stdout.Bytes()
	}
	if err := cmd.Run(); err != nil {
		lrCommandFailure(t, name, args, err, stderr.Bytes())
	}
	return nil
}

func lrCommandFailure(t *testing.T, name string, args []string, err error, stderr []byte) {
	t.Helper()
	if name == "docker" && len(args) > 2 && args[0] == "exec" && (args[2] == "pg_dumpall" || args[2] == "pg_dump") {
		t.Fatalf("native %s failed: %v: %s", args[2], err, strings.TrimSpace(string(stderr)))
	}
	if name == "docker" && len(args) > 3 && args[0] == "exec" && args[1] == "-i" && (args[3] == "psql" || args[3] == "pg_restore") {
		f, saveErr := os.CreateTemp("", "lc-recovery-native-stderr-*.log") // 0600; retained for root's bounded diagnosis.
		if saveErr == nil {
			_, saveErr = f.Write(stderr)
			if closeErr := f.Close(); saveErr == nil {
				saveErr = closeErr
			}
		}
		if saveErr != nil {
			t.Fatalf("native %s failed: %v (stderr bytes=%d; evidence write failed: %v)", args[3], err, len(stderr), saveErr)
		}
		t.Fatalf("native %s failed: %v (restricted stderr=%s sha256=%s bytes=%d)", args[3], err, f.Name(), lrDigest(string(stderr)), len(stderr))
	}
	t.Fatalf("%s failed: %v (stderr bytes=%d)", name, err, len(stderr))
}

func lrDocker(t *testing.T, args ...string) string {
	t.Helper()
	return strings.TrimSpace(string(lrCommand(t, 20*time.Second, nil, nil, nil, "docker", args...)))
}

func lrPort(t *testing.T, id string) string {
	t.Helper()
	got := lrDocker(t, "port", id, "5432/tcp")
	if !strings.HasPrefix(got, "127.0.0.1:") {
		t.Fatal("fixture port is not loopback-only")
	}
	port := strings.TrimPrefix(got, "127.0.0.1:")
	if n, err := strconv.Atoi(port); err != nil || n < 1 || n > 65535 {
		t.Fatal("invalid fixture port")
	}
	return port
}

func lrInspect(t *testing.T, c lrCluster) {
	t.Helper()
	if len(c.id) != 64 {
		t.Fatal("fixture immutable ID missing")
	}
	if _, err := hex.DecodeString(c.id); err != nil {
		t.Fatal("fixture immutable ID malformed")
	}
	parts := strings.Split(lrDocker(t, "inspect", "--format", `{{.Id}}{{println}}{{.Name}}{{println}}{{index .Config.Labels "livecommerce.fixture"}}{{println}}{{.Config.Image}}{{println}}{{json .Mounts}}`, c.id), "\n")
	if len(parts) != 5 || parts[0] != c.id || parts[1] != "/"+c.name || parts[2] != c.name || parts[3] != lrImage || parts[4] != "[]" || lrPort(t, c.id) != c.port {
		t.Fatal("fixture identity/label/image/mount/binding guard failed")
	}
}

func lrTarget(t *testing.T, kind, bootstrap string) lrCluster {
	t.Helper()
	fixture(t) // Existing explicit local-PG permission gate.
	name := "lc-local-recovery-" + kind + "-" + t04Tag()
	role := bootstrap
	if role == "" {
		role = "lr_boot_" + t04Tag()
	}
	password := hex.EncodeToString(randomBytes(24))
	if out := lrDocker(t, "ps", "-aq", "--filter", "name=^/"+name+"$"); out != "" {
		t.Fatal("target name already exists")
	}
	id := lrCommand(t, 60*time.Second, []string{"POSTGRES_USER=" + role, "POSTGRES_PASSWORD=" + password, "POSTGRES_DB=lc_foundation_test"}, nil, nil,
		"docker", "run", "-d", "--pull=never", "--name", name, "--label", "livecommerce.fixture="+name,
		"--memory=512m", "--cpus=1", "--pids-limit=128", "--tmpfs", "/var/lib/postgresql:rw,size=268435456",
		"-e", "POSTGRES_USER", "-e", "POSTGRES_PASSWORD", "-e", "POSTGRES_DB", "-p", "127.0.0.1::5432",
		lrImage, "-c", "shared_buffers=32MB", "-c", "max_connections=60")
	c := lrCluster{id: strings.TrimSpace(string(id)), name: name, role: role, password: password}
	// Cleanup only the recorded immutable ID after rechecking every ownership field.
	t.Cleanup(func() {
		if c.port == "" { // Creation failed before an identity proof; never delete by name.
			return
		}
		lrInspect(t, c)
		lrDocker(t, "rm", "-f", c.id)
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if exec.CommandContext(ctx, "docker", "inspect", c.id).Run() == nil {
			t.Error("owned target container still exists")
		}
	})
	c.port = lrPort(t, c.id)
	lrInspect(t, c)
	u := &url.URL{Scheme: "postgres", User: url.UserPassword(role, password), Host: "127.0.0.1:" + c.port, Path: "/lc_foundation_test", RawQuery: "sslmode=disable"}
	c.dsn = u.String()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	for i := 0; i < 50; i++ {
		pool, err := pgxpool.New(ctx, c.dsn)
		if err == nil {
			probe, stop := context.WithTimeout(ctx, 500*time.Millisecond)
			err = pool.Ping(probe)
			stop()
			if err == nil {
				c.owner = pool
				t.Cleanup(pool.Close)
				return c
			}
			pool.Close()
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatal("fresh target did not become ready")
	return lrCluster{}
}

func lrSourceFixture(t *testing.T) (lrCluster, *testFixture) {
	t.Helper()
	c := lrTarget(t, "source", "")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := migrations.Apply(ctx, c.owner); err != nil {
		t.Fatal(err)
	}
	f := &testFixture{owner: c.owner, databaseURL: c.dsn,
		tenantA: randomUUID(), tenantB: randomUUID(), storeA1: randomUUID(), storeA2: randomUUID(), storeB: randomUUID(), principalA: randomUUID(),
		tokens: map[string]string{"a": randomToken(), "a2": randomToken(), "b": randomToken(), "expired": randomToken(), "revoked": randomToken(), "buyer": randomToken(), "revoked_grant": randomToken()},
	}
	if err := f.seed(ctx); err != nil {
		t.Fatal(err)
	}
	runtimeURL := bcRole(t, f, "commerce_runtime")
	runtime, err := platform.OpenPool(ctx, runtimeURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(runtime.Close)
	f.runtime = runtime
	return c, f
}

func lrFileHash(t *testing.T, path string) (string, int64) {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(h, f)
	if err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(h.Sum(nil)), n
}

func lrSafeBootstrap(role string) bool {
	if len(role) != len("lr_boot_")+12 || !strings.HasPrefix(role, "lr_boot_") {
		return false
	}
	_, err := hex.DecodeString(strings.TrimPrefix(role, "lr_boot_"))
	return err == nil
}

func lrRequireBootIdentity(sourceName, targetName string, sourceOID, targetOID int64) error {
	if !lrSafeBootstrap(sourceName) || sourceName != targetName || sourceOID != 10 || targetOID != 10 {
		return errors.New("bootstrap name/OID10 mismatch")
	}
	return nil
}

func lrDeriveRoles(raw []byte, bootstrap string) ([]byte, error) {
	if !lrSafeBootstrap(bootstrap) {
		return nil, errors.New("unsafe bootstrap role name")
	}
	statement := []byte("CREATE ROLE " + bootstrap + ";\n")
	if bytes.Count(raw, []byte("CREATE ROLE "+bootstrap)) != 1 || bytes.Count(raw, statement) != 1 {
		return nil, errors.New("bootstrap CREATE ROLE missing, malformed or duplicate")
	}
	start := bytes.Index(raw, statement)
	if start < 0 || (start > 0 && raw[start-1] != '\n') {
		return nil, errors.New("bootstrap CREATE ROLE is not one full line")
	}
	derived := make([]byte, 0, len(raw)-len(statement))
	derived = append(derived, raw[:start]...)
	derived = append(derived, raw[start+len(statement):]...)
	if len(derived)+len(statement) != len(raw) || !bytes.Equal(raw[:start], derived[:start]) || !bytes.Equal(raw[start+len(statement):], derived[start:]) {
		return nil, errors.New("roles transform changed unrelated bytes")
	}
	return derived, nil
}

func lrHasPasswordClause(raw []byte) bool {
	for _, line := range bytes.Split(raw, []byte{'\n'}) {
		line = bytes.TrimSpace(line)
		if len(line) == 0 || bytes.HasPrefix(line, []byte("--")) {
			continue
		}
		if bytes.Contains(bytes.ToUpper(line), []byte("PASSWORD")) {
			return true
		}
	}
	return false
}

func TestLocalRecoveryBootstrapRolesTransform(t *testing.T) {
	role := "lr_boot_123456abcdef"
	raw := []byte("-- roles\nCREATE ROLE " + role + ";\nALTER ROLE " + role + " WITH SUPERUSER LOGIN;\nGRANT a TO b GRANTED BY " + role + ";\n")
	want := []byte("-- roles\nALTER ROLE " + role + " WITH SUPERUSER LOGIN;\nGRANT a TO b GRANTED BY " + role + ";\n")
	got, err := lrDeriveRoles(raw, role)
	if err != nil || !bytes.Equal(got, want) {
		t.Fatal("exact bootstrap CREATE transform failed", err)
	}
	for _, invalid := range [][]byte{
		bytes.Replace(raw, []byte("CREATE ROLE "+role+";\n"), nil, 1),
		append(append([]byte(nil), raw...), []byte("CREATE ROLE "+role+";\n")...),
		bytes.Replace(raw, []byte("CREATE ROLE "+role+";\n"), []byte("CREATE ROLE other_boot;\n"), 1),
		bytes.Replace(raw, []byte("CREATE ROLE "+role+";\n"), []byte("CREATE ROLE "+role+"; -- altered\n"), 1),
	} {
		if _, err := lrDeriveRoles(invalid, role); err == nil {
			t.Fatal("unsafe roles artifact accepted")
		}
	}
	for _, test := range []struct {
		source, target string
		a, b           int64
	}{{role, role, 10, 11}, {role, "lr_boot_000000000000", 10, 10}, {"other_boot", role, 10, 10}} {
		if lrRequireBootIdentity(test.source, test.target, test.a, test.b) == nil {
			t.Fatal("wrong bootstrap name/OID accepted")
		}
	}
	if lrHasPasswordClause(raw) || !lrHasPasswordClause(append(append([]byte(nil), raw...), []byte("ALTER ROLE x PASSWORD 'redacted';\n")...)) {
		t.Fatal("native password-clause guard drift")
	}
}

func lrDump(t *testing.T, c lrCluster, path string, args ...string) (string, int64, time.Duration) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	lrCommand(t, 90*time.Second, nil, nil, f, "docker", append([]string{"exec", c.id}, args...)...)
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	hash, size := lrFileHash(t, path)
	if size == 0 {
		t.Fatal("empty native backup artifact")
	}
	return hash, size, time.Since(start)
}

func lrRestoreInput(t *testing.T, c lrCluster, path string, args ...string) time.Duration {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	start := time.Now()
	lrCommand(t, 90*time.Second, nil, f, io.Discard, "docker", append([]string{"exec", "-i", c.id}, args...)...)
	return time.Since(start)
}

func lrRows(t *testing.T, pool *pgxpool.Pool, query string, args ...any) string {
	t.Helper()
	var rows string
	stmt := `SELECT coalesce(jsonb_agg(to_jsonb(x) ORDER BY to_jsonb(x)::text),'[]'::jsonb)::text FROM (` + query + `) x`
	if err := lrScan(pool, stmt, []any{&rows}, args...); err != nil {
		t.Fatal("snapshot query failed: ", err)
	}
	return rows
}

func lrScan(pool *pgxpool.Pool, query string, dest []any, args ...any) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return pool.QueryRow(ctx, query, args...).Scan(dest...)
}

func lrScanBefore(parent context.Context, deadline time.Time, pool *pgxpool.Pool, query string, dest []any, args ...any) error {
	ctx, cancel := context.WithDeadline(parent, deadline)
	defer cancel()
	probe, stop := context.WithTimeout(ctx, 5*time.Second)
	defer stop()
	return pool.QueryRow(probe, query, args...).Scan(dest...)
}

func lrCount(t *testing.T, pool *pgxpool.Pool, query string, args ...any) int64 {
	t.Helper()
	var n int64
	if err := lrScan(pool, query, []any{&n}, args...); err != nil {
		t.Fatal(err)
	}
	return n
}

func lrExec(t *testing.T, pool *pgxpool.Pool, query string, args ...any) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := pool.Exec(ctx, query, args...); err != nil {
		t.Fatal(err)
	}
}

func lrDigest(s string) string { h := sha256.Sum256([]byte(s)); return hex.EncodeToString(h[:]) }

type lrEvidence struct {
	tables, sequences, catalog map[string]string
}

// NULL ACL means PostgreSQL's object-type default; explicit empty ACL does not.
// Compare the complete effective grant tuples by names, not array order or OID.
const lrRelationsQuery = `SELECT n.nspname,c.relname,c.relkind,pg_get_userbyid(c.relowner) owner_name,
 (c.relacl IS NOT NULL AND cardinality(c.relacl)=0) explicit_empty_acl,
 (SELECT coalesce(jsonb_agg(jsonb_build_object('grantee',a.grantee_name,'grantor',a.grantor_name,'privilege',a.privilege_type,'grantable',a.is_grantable)
   ORDER BY a.grantee_name,a.grantor_name,a.privilege_type,a.is_grantable),'[]'::jsonb)
  FROM (SELECT CASE WHEN g.grantee=0 THEN 'PUBLIC' ELSE pg_get_userbyid(g.grantee) END grantee_name,
    pg_get_userbyid(g.grantor) grantor_name,g.privilege_type,g.is_grantable
    FROM aclexplode(coalesce(c.relacl,acldefault(CASE WHEN c.relkind='S' THEN 's'::"char" ELSE 'r'::"char" END,c.relowner))) g) a) effective_acl,
 c.relrowsecurity,c.relforcerowsecurity
 FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace
 WHERE n.nspname NOT IN ('pg_catalog','information_schema') AND n.nspname NOT LIKE 'pg_toast%' AND c.relkind IN ('r','p','S','v','m')`

func lrSnapshot(t *testing.T, pool *pgxpool.Pool, excludedRole string) lrEvidence {
	t.Helper()
	e := lrEvidence{tables: map[string]string{}, sequences: map[string]string{}, catalog: map[string]string{}}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	rows, err := pool.Query(ctx, `SELECT n.nspname,c.relname,c.relkind FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace
		WHERE n.nspname NOT IN ('pg_catalog','information_schema') AND n.nspname NOT LIKE 'pg_toast%' AND c.relkind IN ('r','p','S') ORDER BY 1,2`)
	if err != nil {
		t.Fatal(err)
	}
	type relation struct{ schema, name, kind string }
	var relations []relation
	for rows.Next() {
		var r relation
		if err := rows.Scan(&r.schema, &r.name, &r.kind); err != nil {
			t.Fatal(err)
		}
		relations = append(relations, r)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	rows.Close()
	for _, r := range relations {
		name := r.schema + "." + r.name
		ident := pgx.Identifier{r.schema, r.name}.Sanitize()
		if r.kind == "S" {
			var last int64
			var called bool
			if err := lrScan(pool, `SELECT last_value,is_called FROM `+ident, []any{&last, &called}); err != nil {
				t.Fatal(err)
			}
			e.sequences[name] = fmt.Sprintf("%d/%t", last, called)
			continue
		}
		e.tables[name] = lrDigest(lrRows(t, pool, `SELECT * FROM `+ident))
	}
	catalog := map[string]string{
		"roles":       `SELECT rolname,rolsuper,rolinherit,rolcreaterole,rolcreatedb,rolcanlogin,rolreplication,rolbypassrls,rolconnlimit,rolvaliduntil,rolconfig FROM pg_roles WHERE left(rolname,3)<>'pg_' AND rolname <> $1`,
		"members":     `SELECT r.rolname role_name,m.rolname member_name,g.rolname grantor_name,a.admin_option,a.inherit_option,a.set_option FROM pg_auth_members a JOIN pg_roles r ON r.oid=a.roleid JOIN pg_roles m ON m.oid=a.member JOIN pg_roles g ON g.oid=a.grantor WHERE m.rolname <> $1 AND r.rolname <> $1 AND NOT (left(r.rolname,3)='pg_' AND left(m.rolname,3)='pg_')`,
		"schemas":     `SELECT n.nspname,pg_get_userbyid(n.nspowner) owner_name,n.nspacl::text acl FROM pg_namespace n WHERE n.nspname NOT IN ('pg_catalog','information_schema') AND n.nspname NOT LIKE 'pg_toast%'`,
		"relations":   lrRelationsQuery,
		"columns":     `SELECT n.nspname,c.relname,a.attname,a.attacl::text acl FROM pg_attribute a JOIN pg_class c ON c.oid=a.attrelid JOIN pg_namespace n ON n.oid=c.relnamespace WHERE n.nspname NOT IN ('pg_catalog','information_schema') AND n.nspname NOT LIKE 'pg_toast%' AND a.attnum>0 AND NOT a.attisdropped`,
		"functions":   `SELECT n.nspname,p.proname,pg_get_function_identity_arguments(p.oid) args,pg_get_userbyid(p.proowner) owner_name,p.proacl::text acl,p.prosecdef,p.proconfig,p.prosrc FROM pg_proc p JOIN pg_namespace n ON n.oid=p.pronamespace WHERE n.nspname NOT IN ('pg_catalog','information_schema') AND n.nspname NOT LIKE 'pg_toast%'`,
		"policies":    `SELECT n.nspname,c.relname,p.polname,p.polcmd,p.polpermissive,pg_get_expr(p.polqual,p.polrelid) qual,pg_get_expr(p.polwithcheck,p.polrelid) check_expr,(SELECT array_agg(r.rolname ORDER BY r.rolname) FROM pg_roles r WHERE r.oid=ANY(p.polroles)) role_names FROM pg_policy p JOIN pg_class c ON c.oid=p.polrelid JOIN pg_namespace n ON n.oid=c.relnamespace`,
		"triggers":    `SELECT n.nspname,c.relname,t.tgname,t.tgenabled,pg_get_triggerdef(t.oid) definition FROM pg_trigger t JOIN pg_class c ON c.oid=t.tgrelid JOIN pg_namespace n ON n.oid=c.relnamespace WHERE NOT t.tgisinternal AND n.nspname NOT LIKE 'pg_%'`,
		"default_acl": `SELECT pg_get_userbyid(d.defaclrole) owner_name,n.nspname,d.defaclobjtype,d.defaclacl::text acl FROM pg_default_acl d LEFT JOIN pg_namespace n ON n.oid=d.defaclnamespace`,
		"database":    `SELECT datname,pg_get_userbyid(datdba) owner_name,encoding,datcollate,datctype,datlocprovider,datlocale,datacl IS NULL default_acl,(SELECT count(*) FROM pg_db_role_setting s WHERE s.setdatabase=d.oid) settings_count FROM pg_database d WHERE datname='lc_foundation_test'`,
	}
	for key, query := range catalog {
		if key == "roles" || key == "members" {
			e.catalog[key] = lrDigest(lrRows(t, pool, query, excludedRole))
		} else {
			e.catalog[key] = lrDigest(lrRows(t, pool, query))
		}
	}
	return e
}

func lrAssertEqual(t *testing.T, left, right lrEvidence, scope string) {
	t.Helper()
	for _, pair := range []struct {
		name string
		a, b map[string]string
	}{{"tables", left.tables, right.tables}, {"sequences", left.sequences, right.sequences}, {"catalog", left.catalog, right.catalog}} {
		keys := make([]string, 0, len(pair.a)+len(pair.b))
		seen := map[string]bool{}
		for k := range pair.a {
			seen[k] = true
			keys = append(keys, k)
		}
		for k := range pair.b {
			if !seen[k] {
				keys = append(keys, k)
			}
		}
		sort.Strings(keys)
		for _, k := range keys {
			if pair.a[k] != pair.b[k] || (pair.a[k] == "") != (pair.b[k] == "") {
				t.Fatalf("%s %s differs: %s", scope, pair.name, k)
			}
		}
	}
}

func lrRelationsDiagnostic(t *testing.T, label string, pool *pgxpool.Pool) {
	t.Helper()
	canonical := lrRows(t, pool, lrRelationsQuery)
	f, err := os.CreateTemp("", "lc-recovery-relations-"+label+"-*.json") // 0600; catalog only, no application rows.
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(canonical); err != nil {
		_ = f.Close()
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	t.Logf("restricted relations catalog %s=%s sha256=%s bytes=%d", label, f.Name(), lrDigest(canonical), len(canonical))
}

func lrAssertForeignUnchanged(t *testing.T, before, after lrEvidence) {
	t.Helper()
	mutable := map[string]bool{
		"river_meta.river_job": true, "river_meta.river_queue": true,
		"meta_inbox.events": true, "meta_inbox.audit_events": true,
		"social.conversations": true, "social.messages": true,
	}
	for name, hash := range before.tables {
		if !mutable[name] && after.tables[name] != hash {
			t.Fatalf("cold-start changed foreign table %s", name)
		}
	}
	if len(before.tables) != len(after.tables) {
		t.Fatal("cold-start changed table inventory")
	}
	for name, value := range before.sequences {
		if name != "meta_inbox.audit_events_id_seq" && after.sequences[name] != value {
			t.Fatalf("cold-start changed foreign sequence %s", name)
		}
	}
	if len(before.sequences) != len(after.sequences) || !reflect.DeepEqual(before.catalog, after.catalog) {
		t.Fatal("cold-start changed sequence inventory or catalog")
	}
}

func lrMetaUnrelated(t *testing.T, pool *pgxpool.Pool, eventID string, jobID int64) map[string]string {
	t.Helper()
	checks := map[string]struct {
		query string
		args  []any
	}{
		"job":           {`SELECT * FROM river_meta.river_job WHERE id<>$1`, []any{jobID}},
		"event":         {`SELECT * FROM meta_inbox.events WHERE id<>$1`, []any{eventID}},
		"audit":         {`SELECT * FROM meta_inbox.audit_events WHERE event_id IS DISTINCT FROM $1::uuid`, []any{eventID}},
		"messages":      {`SELECT * FROM social.messages WHERE event_id<>$1`, []any{eventID}},
		"conversations": {`SELECT * FROM social.conversations WHERE id NOT IN (SELECT conversation_id FROM social.messages WHERE event_id=$1)`, []any{eventID}},
	}
	out := make(map[string]string, len(checks))
	for name, check := range checks {
		out[name] = lrDigest(lrRows(t, pool, check.query, check.args...))
	}
	return out
}

func lrDatabaseBoundary(t *testing.T, source, target lrCluster) {
	t.Helper()
	var sourceOID, targetOID int64
	if err := lrScan(source.owner, `SELECT oid::bigint FROM pg_roles WHERE rolname=$1`, []any{&sourceOID}, source.role); err != nil {
		t.Fatal(err)
	}
	if err := lrScan(target.owner, `SELECT oid::bigint FROM pg_roles WHERE rolname=$1`, []any{&targetOID}, target.role); err != nil {
		t.Fatal(err)
	}
	if err := lrRequireBootIdentity(source.role, target.role, sourceOID, targetOID); err != nil {
		t.Fatal(err)
	}
	query := `SELECT system_identifier::text FROM pg_control_system()`
	var sourceID, targetID string
	if err := lrScan(source.owner, query, []any{&sourceID}); err != nil {
		t.Fatal(err)
	}
	if err := lrScan(target.owner, query, []any{&targetID}); err != nil {
		t.Fatal(err)
	}
	if source.id == target.id || source.port == target.port || sourceID == targetID {
		t.Fatal("source and target are not independent PostgreSQL clusters")
	}
	metadata := `SELECT encoding,datcollate,datctype,datlocprovider,to_jsonb(d)->>'datlocale',datacl IS NULL,pg_get_userbyid(datdba),
		(SELECT count(*) FROM pg_db_role_setting WHERE setdatabase=d.oid) FROM pg_database d WHERE datname='lc_foundation_test'`
	var sEnc, bEnc int
	var sColl, bColl, sType, bType, sProvider, bProvider string
	var sLocale, bLocale *string
	var sACL, bACL bool
	var sOwner, bOwner string
	var sSettings, bSettings int64
	if err := lrScan(source.owner, metadata, []any{&sEnc, &sColl, &sType, &sProvider, &sLocale, &sACL, &sOwner, &sSettings}); err != nil {
		t.Fatal(err)
	}
	if err := lrScan(target.owner, metadata, []any{&bEnc, &bColl, &bType, &bProvider, &bLocale, &bACL, &bOwner, &bSettings}); err != nil {
		t.Fatal(err)
	}
	if !sACL || !bACL || sSettings != 0 || bSettings != 0 || sEnc != bEnc || sColl != bColl || sType != bType || sProvider != bProvider || !reflect.DeepEqual(sLocale, bLocale) || sOwner != source.role || bOwner != target.role || sOwner != bOwner {
		t.Fatal("database provisioning exception exceeded default ACL/settings or locale boundary")
	}
}

func lrEmpty(t *testing.T, c lrCluster) bool {
	t.Helper()
	var n int
	if err := lrScan(c.owner, `SELECT count(*) FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace
		WHERE n.nspname NOT IN ('pg_catalog','information_schema') AND n.nspname NOT LIKE 'pg_toast%' AND c.relkind IN ('r','p','S')`, []any{&n}); err != nil {
		t.Fatal(err)
	}
	return n == 0
}

func lrCanConnect(dsn string) bool {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return false
	}
	defer pool.Close()
	return pool.Ping(ctx) == nil
}

func lrPassword(t *testing.T, target lrCluster, sourceDSN string) string {
	t.Helper()
	u, err := url.Parse(sourceDSN)
	if err != nil {
		t.Fatal(err)
	}
	role := u.User.Username()
	var absent bool
	if err := lrScan(target.owner, `SELECT rolpassword IS NULL FROM pg_authid WHERE rolname=$1`, []any{&absent}, role); err != nil || !absent {
		t.Fatal("restored LOGIN retained a password")
	}
	old := roleURL(t, target.dsn, role, func() string { p, _ := u.User.Password(); return p }())
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	probe, err := pgxpool.New(ctx, old)
	if err == nil {
		err = probe.Ping(ctx)
		probe.Close()
	}
	if err == nil {
		t.Fatal("source password authenticated on recovered cluster")
	}
	password := hex.EncodeToString(randomBytes(24))
	if _, err := target.owner.Exec(ctx, `ALTER ROLE `+pgx.Identifier{role}.Sanitize()+` PASSWORD '`+password+`'`); err != nil {
		t.Fatal("new scoped test password failed")
	}
	return roleURL(t, target.dsn, role, password)
}

func lrHTTPReady(t *testing.T, p *mrProcess, address string) {
	t.Helper()
	client := &http.Client{Timeout: time.Second}
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := client.Get("http://" + address + "/healthz")
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return
			}
		}
		select {
		case err := <-p.done:
			p.exited = true
			t.Fatalf("API exited before ready: %v", err)
		default:
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("API did not become healthy")
}

func TestLocalRecoveryLogicalRestoreAndColdStart(t *testing.T) {
	if os.Getenv("LC_TEST_DATABASE_ALLOWED") != "1" {
		t.Skip("explicit isolated PG permission required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Second)
	defer cancel()
	start := time.Now()
	source, sourceFixture := lrSourceFixture(t)
	q := pqSetupItemsOn(t, sourceFixture, nil, false, 1)
	if err := q.record(q.claim(t), pqReport(q)); err != nil {
		t.Fatal("synthetic payment observation", err)
	}
	_, _ = pwDefaultDomainJobs(t, q)
	m := mrSetup(t, sourceFixture)
	asset := miAsset()
	binding := miBinding(t, m, asset, "facebook", sourceFixture.tenantA, sourceFixture.storeA1, sourceFixture.principalA)
	miRoute(t, m, asset, sourceFixture.tenantA, sourceFixture.storeA1, binding)
	pending := mcPost(t, m, asset, miMessage(asset, "m."+randomUUID(), "local-recovery-synthetic"))
	var principalB string
	if err := lrScan(source.owner, `SELECT principal_id::text FROM identity.memberships WHERE tenant_id=$1 LIMIT 1`, []any{&principalB}, sourceFixture.tenantB); err != nil {
		t.Fatal(err)
	}
	otherAsset := miAsset()
	otherBinding := miBinding(t, m, otherAsset, "facebook", sourceFixture.tenantB, sourceFixture.storeB, principalB)
	miRoute(t, m, otherAsset, sourceFixture.tenantB, sourceFixture.storeB, otherBinding)
	other := mcPost(t, m, otherAsset, miMessage(otherAsset, "m."+randomUUID(), "unrelated-tenant-sentinel"))
	lrExec(t, source.owner, `UPDATE river_meta.river_job SET state='scheduled',scheduled_at=clock_timestamp()+interval '1 hour' WHERE id=$1 AND state='available'`, other.job)
	workerSource := miRole(t, sourceFixture, "commerce_meta_worker")
	consumerSource := miRole(t, sourceFixture, "commerce_meta_consumer")
	for schema, queue := range map[string]string{"river": "default", "river_meta": "meta_inbox", "river_payment": "payment_mock_v1", "river_expiry": "checkout_expiry_v1"} {
		lrExec(t, source.owner, `INSERT INTO `+pgx.Identifier{schema, "river_queue"}.Sanitize()+`(name) VALUES($1) ON CONFLICT (name) DO NOTHING`, queue)
	}
	var high int64
	if err := lrScan(source.owner, `SELECT setval('river_payment.river_job_id_seq', (SELECT max(id)+100 FROM river_payment.river_job), true)`, []any{&high}); err != nil || high < 101 {
		t.Fatal("source pruned high-water fixture failed")
	}
	if err := lrScan(source.owner, `SELECT setval('river_expiry.river_job_id_seq', (SELECT max(id)+50 FROM river_expiry.river_job), false)`, []any{&high}); err != nil || high < 51 {
		t.Fatal("source is_called=false high-water fixture failed")
	}
	for _, schema := range []string{"river", "river_meta", "river_payment", "river_expiry"} {
		var jobs, queues int
		if err := lrScan(source.owner, `SELECT (SELECT count(*) FROM `+pgx.Identifier{schema, "river_job"}.Sanitize()+`),(SELECT count(*) FROM `+pgx.Identifier{schema, "river_queue"}.Sanitize()+`)`, []any{&jobs, &queues}); err != nil || jobs < 1 || queues < 1 {
			t.Fatalf("source %s missing linked job/queue", schema)
		}
	}
	if lrCount(t, source.owner, `SELECT count(*) FROM meta_private.event_bodies WHERE event_id=$1`, pending.id) != 1 || lrCount(t, source.owner, `SELECT count(*) FROM social.messages WHERE event_id=$1`, pending.id) != 0 || lrCount(t, source.owner, `SELECT count(*) FROM identity.sessions WHERE revoked_at IS NOT NULL`) == 0 {
		t.Fatal("source pending ciphertext or revoked authorization missing")
	}
	sourceRows := lrSnapshot(t, source.owner, "")
	if len(sourceRows.tables) < 30 || len(sourceRows.sequences) < 4 {
		t.Fatal("source snapshot coverage too narrow")
	}
	artifactDir := t.TempDir()
	rolesPath, archivePath := filepath.Join(artifactDir, "roles.sql"), filepath.Join(artifactDir, "database.dump")
	for _, tool := range []string{"pg_dumpall", "pg_dump", "pg_restore", "psql"} {
		t.Logf("native %s: %s", tool, lrDocker(t, "exec", source.id, tool, "--version"))
	}
	rolesHash, rolesBytes, rolesElapsed := lrDump(t, source, rolesPath, "pg_dumpall", "--roles-only", "--no-role-passwords", "-U", source.role, "-l", "postgres")
	archiveHash, archiveBytes, archiveElapsed := lrDump(t, source, archivePath, "pg_dump", "-Fc", "-U", source.role, "-d", "lc_foundation_test")
	rawRoles, err := os.ReadFile(rolesPath)
	if err != nil {
		t.Fatal(err)
	}
	derivedRoles, err := lrDeriveRoles(rawRoles, source.role)
	if err != nil {
		t.Fatal(err)
	}
	derivedPath := filepath.Join(artifactDir, "roles-bootstrap-existing.sql")
	if err := os.WriteFile(derivedPath, derivedRoles, 0600); err != nil {
		t.Fatal(err)
	}
	derivedHash, derivedBytes := lrFileHash(t, derivedPath)
	var bootstrapAlterSeen bool
	for _, line := range bytes.Split(rawRoles, []byte{'\n'}) {
		if bytes.HasPrefix(line, []byte("ALTER ROLE "+source.role+" WITH ")) {
			bootstrapAlterSeen = true
		}
	}
	if !bootstrapAlterSeen {
		t.Fatal("native roles dump omitted bootstrap attributes")
	}
	passwordClauseSeen := lrHasPasswordClause(rawRoles)
	t.Logf("native roles password-clause-present=%t; derived roles bytes=%d sha256=%s", passwordClauseSeen, derivedBytes, derivedHash)
	if passwordClauseSeen {
		t.Fatal("native passwordless roles dump unexpectedly contains a PASSWORD clause")
	}
	archiveFile, err := os.Open(archivePath)
	if err != nil {
		t.Fatal(err)
	}
	toc := lrCommand(t, 20*time.Second, nil, archiveFile, nil, "docker", "exec", "-i", source.id, "pg_restore", "--list")
	_ = archiveFile.Close()
	if !bytes.Contains(toc, []byte("TABLE DATA")) || !bytes.Contains(toc, []byte("SEQUENCE SET")) {
		t.Fatal("native archive TOC incomplete")
	}
	t.Logf("native dump roles bytes=%d sha256=%s elapsed=%s; archive bytes=%d sha256=%s elapsed=%s", rolesBytes, rolesHash, rolesElapsed, archiveBytes, archiveHash, archiveElapsed)
	target := lrTarget(t, "target", source.role)
	lrDatabaseBoundary(t, source, target)
	if !lrEmpty(t, target) {
		t.Fatal("target was not fresh before restore")
	}
	sourceBeforeGuards, targetBeforeGuards := lrSnapshot(t, source.owner, ""), lrSnapshot(t, target.owner, "")
	if err := func() error { wrong := target; wrong.id = source.id; return lrCheckTarget(wrong, target) }(); err == nil {
		t.Fatal("wrong-target guard accepted source container")
	}
	if lrFileHashMust(t, rolesPath) != rolesHash || lrFileHashMust(t, archivePath) != archiveHash {
		t.Fatal("new backup artifacts changed before restore")
	}
	changed := filepath.Join(artifactDir, "changed.dump")
	if err := lrCopyAndChange(archivePath, changed); err != nil {
		t.Fatal(err)
	}
	if lrArtifactOK(changed, archiveHash) {
		t.Fatal("changed archive hash accepted")
	}
	changedRoles := filepath.Join(artifactDir, "changed-roles.sql")
	if err := lrCopyAndChange(derivedPath, changedRoles); err != nil {
		t.Fatal(err)
	}
	if lrArtifactOK(changedRoles, derivedHash) {
		t.Fatal("changed derived roles hash accepted")
	}
	lrAssertEqual(t, sourceBeforeGuards, lrSnapshot(t, source.owner, ""), "negative source guards")
	lrAssertEqual(t, targetBeforeGuards, lrSnapshot(t, target.owner, ""), "negative target guards")
	lrInspect(t, target)
	if err := lrCheckTarget(target, target); err != nil {
		t.Fatal(err)
	}
	if !lrArtifactOK(rolesPath, rolesHash) || !lrArtifactOK(derivedPath, derivedHash) || !lrArtifactOK(archivePath, archiveHash) {
		t.Fatal("native backup artifact changed before restore")
	}
	currentRaw, err := os.ReadFile(rolesPath)
	if err != nil {
		t.Fatal(err)
	}
	rederived, err := lrDeriveRoles(currentRaw, source.role)
	if err != nil || !bytes.Equal(rederived, derivedRoles) {
		t.Fatal("derived roles provenance changed before restore", err)
	}
	rolesRestore := lrRestoreInput(t, target, derivedPath, "psql", "-X", "--set", "ON_ERROR_STOP=1", "-U", target.role, "-d", "lc_foundation_test")
	if !lrCanConnect(target.dsn) || lrCanConnect(roleURL(t, target.dsn, target.role, source.password)) {
		t.Fatal("native roles restore drifted independent target password or accepted source password")
	}
	archiveRestore := lrRestoreInput(t, target, archivePath, "pg_restore", "--single-transaction", "--exit-on-error", "-U", target.role, "-d", "lc_foundation_test")
	t.Logf("native restore roles=%s archive=%s", rolesRestore, archiveRestore)
	if lrEmpty(t, target) {
		t.Fatal("strict restore returned success but target remained empty")
	}
	if err := lrCheckTarget(target, target); err == nil {
		t.Fatal("nonempty target accepted for a second restore")
	}
	targetRows := lrSnapshot(t, target.owner, "")
	lrAssertEqual(t, targetRows, lrSnapshot(t, target.owner, ""), "nonempty target guard")
	if sourceRows.catalog["relations"] != targetRows.catalog["relations"] {
		lrRelationsDiagnostic(t, "source", source.owner)
		lrRelationsDiagnostic(t, "target", target.owner)
	}
	lrAssertEqual(t, sourceRows, targetRows, "raw restore")
	if err := migrations.Apply(ctx, target.owner); err != nil {
		t.Fatal("first restored Apply", err)
	}
	if err := migrations.Apply(ctx, target.owner); err != nil {
		t.Fatal("second restored Apply", err)
	}
	lrAssertEqual(t, targetRows, lrSnapshot(t, target.owner, ""), "idempotent Apply")
	var tenant, store, keyID string
	var nonce, ciphertext []byte
	if err := lrScan(target.owner, `SELECT e.tenant_id::text,e.store_id::text,b.key_id,b.nonce,b.ciphertext FROM meta_inbox.events e JOIN meta_private.event_bodies b ON b.event_id=e.id WHERE e.id=$1`, []any{&tenant, &store, &keyID, &nonce, &ciphertext}, pending.id); err != nil {
		t.Fatal(err)
	}
	if tenant != sourceFixture.tenantA || store != sourceFixture.storeA1 || keyID != miKeyID || !bytes.Equal(miDecrypt(t, m.key, "event", pending.id, pending.app, pending.object, "", pending.key, pending.hash, tenant, store, pending.route, pending.epoch, keyID, nonce, ciphertext), pending.plain) {
		t.Fatal("restored original encrypted event context/key failed")
	}
	mainDSN := lrPassword(t, target, sourceFixture.runtime.Config().ConnString())
	workerDSN := lrPassword(t, target, workerSource)
	consumerDSN := lrPassword(t, target, consumerSource)
	mainPool, err := platform.OpenPool(ctx, mainDSN)
	if err != nil {
		t.Fatal("restored ordinary runtime rejected", err)
	}
	defer mainPool.Close()
	workerPool, err := platform.OpenMetaWorkerPool(ctx, workerDSN)
	if err != nil {
		t.Fatal("restored Meta worker rejected", err)
	}
	defer workerPool.Close()
	consumerPool, err := platform.OpenMetaConsumerPool(ctx, consumerDSN)
	if err != nil {
		t.Fatal("restored Meta consumer rejected", err)
	}
	defer consumerPool.Close()
	if err := platform.ValidateSameDatabase(ctx, workerPool, consumerPool); err != nil {
		t.Fatal("same recovered database identity rejected", err)
	}
	sourceWorker, err := platform.OpenMetaWorkerPool(ctx, workerSource)
	if err != nil {
		t.Fatal(err)
	}
	if err := platform.ValidateSameDatabase(ctx, sourceWorker, consumerPool); err == nil {
		t.Fatal("cloned source/target databases accepted as identical")
	}
	sourceWorker.Close()
	if err := platform.WithScope(ctx, mainPool, sourceFixture.tokens["a"], sourceFixture.storeA1, "store:read", func(tx pgx.Tx, scope platform.Scope) error {
		if scope.TenantID != sourceFixture.tenantA || scope.StoreID != sourceFixture.storeA1 {
			return errors.New("restored scope drift")
		}
		var visible, foreign int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM control.stores`).Scan(&visible); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM control.stores WHERE id=$1`, sourceFixture.storeB).Scan(&foreign); err != nil {
			return err
		}
		if visible != 1 || foreign != 0 {
			return errors.New("restored scoped SELECT leaked another store")
		}
		return nil
	}); err != nil {
		t.Fatal("authorized restored read failed", err)
	}
	for _, check := range []struct {
		token, store string
		want         error
	}{{sourceFixture.tokens["a"], sourceFixture.storeB, platform.ErrScopeNotFound}, {sourceFixture.tokens["revoked"], sourceFixture.storeA1, platform.ErrUnauthorized}} {
		err := platform.WithScope(ctx, mainPool, check.token, check.store, "store:read", func(pgx.Tx, platform.Scope) error { return errors.New("unauthorized callback") })
		if !errors.Is(err, check.want) {
			t.Fatalf("restored authorization negative drift: %v", err)
		}
	}
	if err := platform.ValidateMetaWorkerPool(ctx, consumerPool); err == nil || platform.ValidateMetaConsumerPool(ctx, workerPool) == nil {
		t.Fatal("wrong restored lifecycle authority admitted")
	}
	var readyMeta, readyPayment, readyExpiry bool
	if err := lrScan(target.owner, `SELECT meta_inbox.runtime_ready(),integration.payment_queue_ready(),checkout.expiry_queue_ready()`, []any{&readyMeta, &readyPayment, &readyExpiry}); err != nil || !readyMeta || !readyPayment || !readyExpiry {
		t.Fatal("restored runtime readiness failed")
	}
	if err := lrScan(workerPool, `SELECT meta_inbox.runtime_ready()`, []any{&readyMeta}); err != nil || !readyMeta {
		t.Fatal("nonowner Meta worker readiness failed")
	}
	lrExec(t, target.owner, `ALTER TABLE river_meta.river_job DISABLE TRIGGER meta_job_family`)
	if err := lrScan(workerPool, `SELECT meta_inbox.runtime_ready()`, []any{&readyMeta}); err != nil || readyMeta {
		t.Fatal("tampered restored guard did not fail closed")
	}
	lrExec(t, target.owner, `ALTER TABLE river_meta.river_job ENABLE TRIGGER meta_job_family`)
	lrAssertEqual(t, targetRows, lrSnapshot(t, target.owner, ""), "guard reenabled")
	var dbOwner, dbCreate, dbConnect, canAssume bool
	mainURL, err := url.Parse(mainDSN)
	if err != nil {
		t.Fatal(err)
	}
	if err := lrScan(target.owner, `SELECT pg_has_role($1,$2,'MEMBER'),has_database_privilege($1,'lc_foundation_test','CREATE'),has_database_privilege($1,'lc_foundation_test','CONNECT'),pg_has_role($1,$2,'SET')`, []any{&dbOwner, &dbCreate, &dbConnect, &canAssume}, mainURL.User.Username(), target.role); err != nil || dbOwner || dbCreate || !dbConnect || canAssume {
		t.Fatal("restored runtime database/bootstrapping authority drift")
	}
	apiBinary := mrBuild(t, "../../cmd/api", "recovered-api")
	metaBinary := mrBuild(t, "../../cmd/meta-worker", "recovered-meta-worker")
	for _, item := range []struct{ packagePath, enabled string }{{"../../cmd/payment-worker", "COMMERCE_PAYMENT_WORKER_ENABLED"}, {"../../cmd/expiry-worker", "COMMERCE_EXPIRY_WORKER_ENABLED"}} {
		binary := mrBuild(t, item.packagePath, "recovered-"+filepath.Base(item.packagePath))
		out := lrCommand(t, 15*time.Second, []string{item.enabled + "=0"}, nil, nil, binary)
		if len(out) != 0 {
			t.Fatal("disabled worker emitted output")
		}
	}
	if out := lrCommand(t, 15*time.Second, []string{"COMMERCE_META_WORKER_ENABLED=0", "COMMERCE_META_WORKER_DATABASE_URL=invalid", "COMMERCE_META_CONSUMER_DATABASE_URL=invalid", "COMMERCE_META_PAYLOAD_KEYS_JSON=invalid"}, nil, nil, metaBinary); len(out) != 0 {
		t.Fatal("disabled Meta worker opened secrets or emitted output")
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := listener.Addr().String()
	_ = listener.Close()
	api := mrLaunch(t, apiBinary, "local-recovery-api", []string{"LISTEN_ADDR=" + address, "DATABASE_URL=" + mainDSN, "COMMERCE_META_WEBHOOK_ENABLED=0", "COMMERCE_BUYER_ENABLED=0", "COMMERCE_IDENTITY_ENABLED=0"})
	lrHTTPReady(t, api, address)
	lrAssertEqual(t, targetRows, lrSnapshot(t, target.owner, ""), "disabled cold start")
	mrStop(t, api, syscall.SIGTERM, true)
	mrLogNoSecrets(t, api, mainDSN, workerDSN, consumerDSN)
	for _, schema := range []string{"river", "river_meta", "river_payment", "river_expiry"} {
		var next, maximum int64
		if err := lrScan(target.owner, `SELECT nextval($1::regclass)`, []any{&next}, schema+".river_job_id_seq"); err != nil {
			t.Fatal(err)
		}
		if err := lrScan(target.owner, `SELECT max(id) FROM `+pgx.Identifier{schema, "river_job"}.Sanitize(), []any{&maximum}); err != nil || next <= maximum {
			t.Fatalf("restored %s sequence reused an admitted job ID", schema)
		}
	}
	foreignBeforeMeta := lrSnapshot(t, target.owner, "")
	unrelatedBeforeMeta := lrMetaUnrelated(t, target.owner, pending.id, pending.job)
	// The same restored ciphertext is attempted with the right ID/wrong bytes,
	// then with the separately retained correct key. No new webhook is posted.
	wrongKey := randomBytes(32)
	if bytes.Equal(wrongKey, m.key) {
		t.Fatal("wrong-key fixture collision")
	}
	workerEnv := func(key []byte) []string {
		jsonKeys := fmt.Sprintf(`{"keys":[{"id":%q,"key_base64":%q}]}`, miKeyID, base64.StdEncoding.EncodeToString(key))
		return []string{"COMMERCE_META_WORKER_ENABLED=1", "COMMERCE_META_WORKER_DATABASE_URL=" + workerDSN, "COMMERCE_META_CONSUMER_DATABASE_URL=" + consumerDSN, "COMMERCE_META_WORKER_CONCURRENCY=1", "COMMERCE_META_PAYLOAD_ACTIVE_KEY_ID=" + miKeyID, "COMMERCE_META_PAYLOAD_KEYS_JSON=" + jsonKeys}
	}
	wrong := mrLaunch(t, metaBinary, "local-recovery-wrong-key", workerEnv(wrongKey))
	mrReadyLog(t, wrong, "meta_worker_ready")
	deadline := time.Now().Add(10 * time.Second)
	var state string
	var attempt int
	for time.Now().Before(deadline) {
		if err := lrScanBefore(ctx, deadline, target.owner, `SELECT state,attempt FROM river_meta.river_job WHERE id=$1`, []any{&state, &attempt}, pending.job); err != nil {
			t.Fatal(err)
		}
		if state == "retryable" && attempt > 0 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if state != "retryable" || attempt == 0 || lrCount(t, target.owner, `SELECT count(*) FROM meta_private.event_bodies WHERE event_id=$1`, pending.id) != 1 || lrCount(t, target.owner, `SELECT count(*) FROM social.messages WHERE event_id=$1`, pending.id) != 0 || lrCount(t, target.owner, `SELECT count(*) FROM meta_inbox.events WHERE id=$1 AND terminal_reason IS NOT NULL`, pending.id) != 0 {
		t.Fatal("same-ID wrong-key attempt did not retain retryable encrypted event")
	}
	mrStop(t, wrong, syscall.SIGTERM, true)
	mrLogNoSecrets(t, wrong, workerDSN, consumerDSN)
	lrExec(t, target.owner, `UPDATE river_meta.river_job SET scheduled_at=clock_timestamp()-interval '1 second' WHERE id=$1 AND state='retryable'`, pending.job)
	correct := mrLaunch(t, metaBinary, "local-recovery-correct-key", workerEnv(m.key))
	mrReadyLog(t, correct, "meta_worker_ready")
	deadline = time.Now().Add(10 * time.Second)
	processed := false
	for time.Now().Before(deadline) {
		var reason *string
		if err := lrScanBefore(ctx, deadline, target.owner, `SELECT j.state,e.terminal_reason FROM river_meta.river_job j JOIN meta_inbox.events e ON e.job_id=j.id WHERE j.id=$1`, []any{&state, &reason}, pending.job); err != nil {
			t.Fatal(err)
		}
		if state == "completed" && reason != nil && *reason == "processed" {
			processed = true
			break
		}
		if state == "cancelled" || state == "discarded" {
			t.Fatalf("restored Meta job ended %s without processed fact", state)
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !processed {
		t.Fatal("correct-key restored Meta job did not complete")
	}
	var completedAttempt int
	if err := lrScan(target.owner, `SELECT attempt FROM river_meta.river_job WHERE id=$1 AND state='completed'`, []any{&completedAttempt}, pending.job); err != nil || completedAttempt <= attempt {
		t.Fatal("correct-key job did not advance native attempt")
	}
	if lrCount(t, target.owner, `SELECT count(*) FROM social.messages WHERE event_id=$1`, pending.id) != 1 || lrCount(t, target.owner, `SELECT count(*) FROM meta_inbox.audit_events WHERE event_id=$1 AND action='processed'`, pending.id) != 1 {
		t.Fatal("restored event not projected exactly once")
	}
	if lrCount(t, target.owner, `SELECT count(*) FROM social.messages WHERE event_id=$1 AND tenant_id=$2 AND store_id=$3`, pending.id, sourceFixture.tenantA, sourceFixture.storeA1) != 1 {
		t.Fatal("restored projection lost selected tenant/store context")
	}
	mrStop(t, correct, syscall.SIGTERM, true)
	restart := mrLaunch(t, metaBinary, "local-recovery-restart", workerEnv(m.key))
	mrReadyLog(t, restart, "meta_worker_ready")
	retryClient, err := river.NewClient(riverpgxv5.New(workerPool), &river.Config{Schema: "river_meta"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := retryClient.JobRetry(ctx, pending.job); err != nil {
		t.Fatal("native River retry of completed restored job", err)
	}
	replayed := false
	deadline = time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if err := lrScanBefore(ctx, deadline, target.owner, `SELECT state,attempt FROM river_meta.river_job WHERE id=$1`, []any{&state, &attempt}, pending.job); err != nil {
			t.Fatal(err)
		}
		if state == "completed" && attempt > completedAttempt {
			replayed = true
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	mrStop(t, restart, syscall.SIGTERM, true)
	if !replayed {
		t.Fatal("native same-job retry did not complete after restart")
	}
	if lrCount(t, target.owner, `SELECT count(*) FROM social.messages WHERE event_id=$1`, pending.id) != 1 || lrCount(t, target.owner, `SELECT count(*) FROM meta_inbox.audit_events WHERE event_id=$1 AND action='processed'`, pending.id) != 1 {
		t.Fatal("restart replayed restored event")
	}
	lrAssertForeignUnchanged(t, foreignBeforeMeta, lrSnapshot(t, target.owner, ""))
	if !reflect.DeepEqual(unrelatedBeforeMeta, lrMetaUnrelated(t, target.owner, pending.id, pending.job)) || lrCount(t, target.owner, `SELECT count(*) FROM meta_inbox.events WHERE id=$1 AND tenant_id=$2 AND store_id=$3`, other.id, sourceFixture.tenantB, sourceFixture.storeB) != 1 {
		t.Fatal("unrelated tenant Meta state changed during selected replay")
	}
	lrAssertEqual(t, sourceRows, lrSnapshot(t, source.owner, ""), "source after recovery")
	t.Logf("LOCAL recovery gate completed in %s; no provider or production access", time.Since(start))
}

func lrFileHashMust(t *testing.T, path string) string { h, _ := lrFileHash(t, path); return h }

func lrArtifactOK(path, want string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return false
	}
	return hex.EncodeToString(h.Sum(nil)) == want
}

func lrCopyAndChange(source, target string) error {
	in, err := os.Open(source)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	if _, err = io.Copy(out, in); err == nil {
		_, err = out.Write([]byte{0})
	}
	if closeErr := out.Close(); err == nil {
		err = closeErr
	}
	return err
}

func lrCheckTarget(actual, expected lrCluster) error {
	if actual.id != expected.id || actual.name != expected.name || actual.port != expected.port || actual.role != expected.role {
		return errors.New("target identity mismatch")
	}
	var n int
	if err := lrScan(actual.owner, `SELECT count(*) FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace WHERE n.nspname NOT IN ('pg_catalog','information_schema') AND n.nspname NOT LIKE 'pg_toast%' AND c.relkind IN ('r','p','S')`, []any{&n}); err != nil {
		return errors.New("target catalog unavailable")
	}
	if n != 0 {
		return errors.New("target is not empty")
	}
	return nil
}
