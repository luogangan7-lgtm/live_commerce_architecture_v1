package foundation_test

// PA03 TestPasswordPA03Schema (REAL_PG) — contracts/merchant-password-auth-v1.md §4.1 tables and
// CHECKs, §4.2 definer properties (owner, SECURITY DEFINER, proconfig, ACL), §4.4 grants and the §9
// PA03 row: table owners are not the writer, column-level UPDATE denial, TRUNCATE denial, privilege
// matrix, no plaintext-code column, `operator.unlocked` accepted by the action CHECK, and an upgrade
// of a DB populated through 0066. Touches identity.{password_credentials,email_challenges,
// auth_throttle,auth_events} and the eight identity.* definers named in §4.2, plus platform.OpenIdentityPool.
// Owner-pool use (disclosed): CHECK negatives insert rows directly as the migration owner because the
// CHECKs are the property under test; privilege denials use SET LOCAL ROLE from the owner pool so no
// extra login role is created; the upgrade test drops the 0070 objects in a throw-away container to
// reproduce the 0066 state and then runs the real migrations.Apply.

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"livecommerce/internal/platform"
	"livecommerce/migrations"
)

var pwa03Tables = []string{"password_credentials", "email_challenges", "auth_throttle", "auth_events"}

// §4.2 signatures (name -> identity arguments as pg_get_function_identity_arguments prints them).
var pwa03Funcs = map[string]string{
	"auth_throttle_hit":        "p_bucket bytea, p_window_seconds integer, p_offset_seconds integer",
	"start_signup_challenge":   "p_id uuid, p_email text, p_hash text, p_binding bytea, p_code bytea, p_locale text, p_ip bytea",
	"password_login_material":  "p_email text",
	"record_password_failure":  "p_email text",
	"start_login_challenge":    "p_id uuid, p_email text, p_version bigint, p_binding bytea, p_code bytea, p_locale text, p_ip bytea",
	"start_reset_challenge":    "p_id uuid, p_email text, p_binding bytea, p_code bytea, p_locale text, p_ip bytea",
	"record_challenge_mail":    "p_id uuid, p_state text, p_message_id text",
	"complete_email_challenge": "p_binding bytea, p_purpose text, p_code bytea, p_new_hash text, p_session bytea, p_ttl bigint",
}

var pwa03Columns = map[string][]string{
	"password_credentials": {"principal_id", "email", "password_hash", "password_version", "failed_count", "disabled_at", "email_verified_at", "created_at", "password_changed_at"},
	"email_challenges": {"id", "purpose", "email", "locale", "principal_id", "password_version", "pending_password_hash", "binding_hash", "code_hmac", "ip_hmac",
		"attempts", "created_at", "expires_at", "consumed_at", "consumed_reason", "mail_state", "provider_message_id"},
	"auth_throttle": {"bucket", "window_start", "hits"},
	"auth_events":   {"id", "principal_id", "email_hmac", "ip_hmac", "challenge_id", "action", "created_at"},
}

// §4.4 writer UPDATE column grants, exactly.
var pwa03WriterUpdate = map[string][]string{
	"password_credentials": {"password_hash", "password_version", "failed_count", "disabled_at", "password_changed_at"},
	"email_challenges":     {"attempts", "consumed_at", "consumed_reason", "mail_state", "provider_message_id", "pending_password_hash"},
	"auth_throttle":        {"hits"},
	"auth_events":          {},
}

var pwa03Actions = []string{"signup.requested", "signup.exists_notified", "signup.verified", "login.password_failed", "login.password_ok",
	"login.disabled", "login.verified", "reset.requested", "reset.completed", "code.failed", "code.exhausted", "throttled", "mail.sent",
	"mail.failed", "mail.unknown", "password.breach_check_unavailable", "operator.unlocked"}

func pwaAsRole(t *testing.T, owner *pgxpool.Pool, role, sql string, args ...any) error {
	t.Helper()
	ctx := context.Background()
	tx, err := owner.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, "SET LOCAL ROLE "+pgx.Identifier{role}.Sanitize()); err != nil {
		t.Fatalf("SET ROLE %s: %v", role, err)
	}
	_, err = tx.Exec(ctx, sql, args...)
	return err
}

// pwaAssertSchema is the structural + privilege half of PA03; it runs against the shared fixture
// and again against the upgraded database.
func pwaAssertSchema(t *testing.T, owner *pgxpool.Pool) {
	t.Helper()
	ctx := context.Background()

	// R-3 / round-1 P1 #4: the four tables belong to the migration role, never the writer or the login role.
	for _, tbl := range pwa03Tables {
		var ownerName string
		var rls bool
		if err := owner.QueryRow(ctx, `SELECT t.tableowner, c.relrowsecurity FROM pg_tables t JOIN pg_class c ON c.oid=(quote_ident(t.schemaname)||'.'||quote_ident(t.tablename))::regclass WHERE t.schemaname='identity' AND t.tablename=$1`, tbl).Scan(&ownerName, &rls); err != nil {
			t.Fatalf("table identity.%s missing: %v", tbl, err)
		}
		if ownerName == "commerce_identity_writer" || ownerName == "commerce_identity" {
			t.Errorf("identity.%s is owned by %s (must be the migration role)", tbl, ownerName)
		}
		if rls {
			t.Errorf("identity.%s has RLS enabled; ruling R-3 says privilege isolation only", tbl)
		}
	}

	// Columns present; the only column that mentions a code is the HMAC; no plaintext secret columns.
	for tbl, want := range pwa03Columns {
		rows, err := owner.Query(ctx, `SELECT column_name FROM information_schema.columns WHERE table_schema='identity' AND table_name=$1`, tbl)
		if err != nil {
			t.Fatal(err)
		}
		have := map[string]bool{}
		for rows.Next() {
			var c string
			if err := rows.Scan(&c); err != nil {
				t.Fatal(err)
			}
			have[c] = true
		}
		rows.Close()
		for _, c := range want {
			if !have[c] {
				t.Errorf("identity.%s lacks column %s", tbl, c)
			}
			delete(have, c)
		}
		for extra := range have {
			t.Errorf("identity.%s has undeclared column %s", tbl, extra)
		}
	}
	rows, err := owner.Query(ctx, `SELECT table_name||'.'||column_name FROM information_schema.columns WHERE table_schema='identity' AND table_name=ANY($1) AND (column_name ~* '(code|otp|plain|secret|token|raw)' OR (column_name ~* 'password' AND column_name NOT IN ('password_hash','password_version','password_changed_at','pending_password_hash')))`, pwa03Tables)
	if err != nil {
		t.Fatal(err)
	}
	var codeCols []string
	for rows.Next() {
		var c string
		_ = rows.Scan(&c)
		codeCols = append(codeCols, c)
	}
	rows.Close()
	if strings.Join(codeCols, ",") != "email_challenges.code_hmac" {
		t.Errorf("code/secret-like columns = %v, want only email_challenges.code_hmac (no plaintext-code column)", codeCols)
	}

	// Indexes and keys named by §4.1.
	for name, must := range map[string][]string{
		"email_challenges": {"(email, purpose)", "consumed_at IS NULL", "(expires_at)", "(principal_id, created_at)", "purpose = 'reset'"},
		"auth_throttle":    {"(bucket, window_start)", "(window_start)"},
		"auth_events":      {"(created_at)"},
	} {
		var defs string
		if err := owner.QueryRow(ctx, `SELECT coalesce(string_agg(indexdef,' ; '),'') FROM pg_indexes WHERE schemaname='identity' AND tablename=$1`, name).Scan(&defs); err != nil {
			t.Fatal(err)
		}
		for _, frag := range must {
			if name == "auth_events" {
				// the contract names no auth_events index; (created_at) serves the 180 day purge, tolerate absence
				continue
			}
			if !strings.Contains(defs, frag) {
				t.Errorf("identity.%s index definitions %q lack %q", name, defs, frag)
			}
		}
	}
	var uniques int
	if err := owner.QueryRow(ctx, `SELECT count(*) FROM pg_indexes WHERE schemaname='identity' AND indexdef LIKE 'CREATE UNIQUE INDEX%' AND ((tablename='password_credentials' AND indexdef LIKE '%(email)%') OR (tablename='email_challenges' AND indexdef LIKE '%(binding_hash)%'))`).Scan(&uniques); err != nil || uniques != 2 {
		t.Errorf("UNIQUE(email) and UNIQUE(binding_hash) indexes = %d (%v), want 2", uniques, err)
	}

	// §4.2: eight definers, owner commerce_identity_writer, SECURITY DEFINER, search_path=pg_catalog,
	// PUBLIC revoked, EXECUTE for commerce_identity only.
	var pubFuncs int
	for name, args := range pwa03Funcs {
		var oid uint32
		var identArgs, ownerName string
		var secdef bool
		var config []string
		var src string
		err := owner.QueryRow(ctx, `SELECT p.oid, pg_get_function_identity_arguments(p.oid), pg_get_userbyid(p.proowner), p.prosecdef, coalesce(p.proconfig,'{}'), p.prosrc FROM pg_proc p JOIN pg_namespace n ON n.oid=p.pronamespace WHERE n.nspname='identity' AND p.proname=$1`, name).Scan(&oid, &identArgs, &ownerName, &secdef, &config, &src)
		if err != nil {
			t.Errorf("function identity.%s: %v", name, err)
			continue
		}
		if identArgs != args {
			t.Errorf("identity.%s arguments = %q, want %q", name, identArgs, args)
		}
		if ownerName != "commerce_identity_writer" || !secdef {
			t.Errorf("identity.%s owner=%s secdef=%v", name, ownerName, secdef)
		}
		if strings.Join(config, ",") != "search_path=pg_catalog" {
			t.Errorf("identity.%s proconfig = %v, want exactly search_path=pg_catalog", name, config)
		}
		if strings.Contains(src, "operator.unlocked") {
			t.Errorf("identity.%s writes operator.unlocked; §4.1 says only the runbook SQL may", name)
		}
		var public bool
		if err := owner.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM pg_proc p, aclexplode(coalesce(p.proacl, acldefault('f', p.proowner))) a WHERE p.oid=$1 AND a.grantee=0 AND a.privilege_type='EXECUTE')`, oid).Scan(&public); err != nil {
			t.Fatal(err)
		}
		if public {
			pubFuncs++
			t.Errorf("identity.%s is executable by PUBLIC", name)
		}
		for role, want := range map[string]bool{"commerce_identity": true, "commerce_runtime": false, "commerce_auth": false, "commerce_worker": false} {
			var got bool
			if err := owner.QueryRow(ctx, `SELECT has_function_privilege($1, $2::oid, 'EXECUTE')`, role, oid).Scan(&got); err != nil {
				t.Fatal(err)
			}
			if got != want {
				t.Errorf("EXECUTE on identity.%s for %s = %v, want %v", name, role, got, want)
			}
		}
		// Every other commerce_* role: no EXECUTE except the identity login role.
		var extra []string
		xr, err := owner.Query(ctx, `SELECT rolname FROM pg_roles WHERE rolname LIKE 'commerce\_%' AND rolname NOT IN ('commerce_identity','commerce_identity_writer') AND has_function_privilege(rolname, $1::oid, 'EXECUTE')`, oid)
		if err != nil {
			t.Fatal(err)
		}
		for xr.Next() {
			var r string
			_ = xr.Scan(&r)
			extra = append(extra, r)
		}
		xr.Close()
		if len(extra) > 0 {
			t.Errorf("identity.%s executable by %v", name, extra)
		}
	}

	// §4.4 privilege matrix on the four tables.
	privs := []string{"SELECT", "INSERT", "UPDATE", "DELETE", "TRUNCATE", "REFERENCES", "TRIGGER"}
	for _, tbl := range pwa03Tables {
		rel := "identity." + tbl
		var others []string
		orows, err := owner.Query(ctx, `SELECT rolname FROM pg_roles WHERE rolname <> 'commerce_identity_writer' AND NOT rolsuper AND left(rolname,3)<>'pg_'`)
		if err != nil {
			t.Fatal(err)
		}
		for orows.Next() {
			var r string
			_ = orows.Scan(&r)
			others = append(others, r)
		}
		orows.Close()
		for _, role := range others {
			for _, p := range privs {
				var has, anyCol bool
				if err := owner.QueryRow(ctx, `SELECT has_table_privilege($1,$2,$3)`, role, rel, p).Scan(&has); err != nil {
					t.Fatal(err)
				}
				if p == "SELECT" || p == "INSERT" || p == "UPDATE" || p == "REFERENCES" {
					if err := owner.QueryRow(ctx, `SELECT has_any_column_privilege($1,$2,$3)`, role, rel, p).Scan(&anyCol); err != nil {
						t.Fatal(err)
					}
				}
				if has || anyCol {
					t.Errorf("role %s holds %s on %s (table=%v column=%v); only commerce_identity_writer may hold anything", role, p, rel, has, anyCol)
				}
			}
		}
		// The writer: SELECT+INSERT on all four, DELETE on three (purge), never TRUNCATE, table-level UPDATE never.
		for p, want := range map[string]bool{"SELECT": true, "INSERT": true, "UPDATE": false, "TRUNCATE": false, "REFERENCES": false, "TRIGGER": false,
			"DELETE": tbl != "password_credentials"} {
			var got bool
			if err := owner.QueryRow(ctx, `SELECT has_table_privilege('commerce_identity_writer',$1,$2)`, rel, p).Scan(&got); err != nil {
				t.Fatal(err)
			}
			if got != want {
				t.Errorf("commerce_identity_writer %s on %s = %v, want %v", p, rel, got, want)
			}
		}
		granted := map[string]bool{}
		for _, c := range pwa03WriterUpdate[tbl] {
			granted[c] = true
		}
		for _, c := range pwa03Columns[tbl] {
			var got bool
			if err := owner.QueryRow(ctx, `SELECT has_column_privilege('commerce_identity_writer',$1,$2,'UPDATE')`, rel, c).Scan(&got); err != nil {
				t.Fatal(err)
			}
			if got != granted[c] {
				t.Errorf("commerce_identity_writer UPDATE(%s) on %s = %v, want %v", c, rel, got, granted[c])
			}
		}
	}

	// Real denials, not just catalog reads: SET ROLE and try (SQLSTATE 42501 insufficient_privilege).
	deny := func(role, sql, what string) {
		t.Helper()
		err := pwaAsRole(t, owner, role, sql)
		if sqlState(err) != "42501" {
			t.Errorf("%s as %s: got %v, want 42501", what, role, err)
		}
	}
	deny("commerce_identity_writer", `UPDATE identity.email_challenges SET code_hmac = code_hmac`, "UPDATE email_challenges.code_hmac")
	deny("commerce_identity_writer", `UPDATE identity.auth_events SET action = action`, "UPDATE auth_events.action")
	deny("commerce_identity_writer", `UPDATE identity.password_credentials SET email = email`, "UPDATE password_credentials.email")
	for _, tbl := range pwa03Tables {
		deny("commerce_identity_writer", "TRUNCATE identity."+tbl, "TRUNCATE "+tbl)
		for _, role := range []string{"commerce_identity", "commerce_runtime", "commerce_auth"} {
			deny(role, "SELECT count(*) FROM identity."+tbl, "SELECT "+tbl)
			deny(role, fmt.Sprintf("DELETE FROM identity.%s", tbl), "DELETE "+tbl)
		}
	}
}

// pwaValidPHC matches the §4.1 password_hash CHECK (salt 22 + hash 43 unpadded base64 characters).
var pwaValidPHC = "$argon2id$v=19$m=19456,t=2,p=1$" + strings.Repeat("A", 22) + "$" + strings.Repeat("B", 43)

func pwa03Check(t *testing.T, owner *pgxpool.Pool, name, sql string, args ...any) {
	t.Helper()
	_, err := owner.Exec(context.Background(), sql, args...)
	if sqlState(err) != "23514" {
		t.Errorf("%s: SQLSTATE %q (err %v), want 23514 check_violation", name, sqlState(err), err)
	}
}

func TestPasswordPA03Schema(t *testing.T) {
	f := fixture(t)
	ctx := context.Background()
	owner := f.owner

	t.Run("structure_and_privileges", func(t *testing.T) { pwaAssertSchema(t, owner) })

	t.Run("checks", func(t *testing.T) {
		principal := randomUUID()
		mustExec(t, owner, `INSERT INTO identity.principals(id) VALUES ($1::uuid)`, principal)
		bytes32 := func(b byte) []byte { return []byte(strings.Repeat(string([]byte{b}), 32)) }
		email := "pwa03." + pwaLetters(10) + "@example.test"

		credInsert := `INSERT INTO identity.password_credentials(principal_id,email,password_hash,password_version,failed_count,email_verified_at) VALUES ($1::uuid,$2,$3,$4,$5,now())`
		// Baseline row is valid (proves each negative below fails on its own field only).
		mustExec(t, owner, credInsert, principal, email, pwaValidPHC, 1, 0)
		for _, c := range []struct {
			name, email, hash string
			version, failed   int
		}{
			{"failed_count 101", "b.pwa03@example.test", pwaValidPHC, 1, 101},
			{"failed_count -1", "c.pwa03@example.test", pwaValidPHC, 1, -1},
			{"password_version 0", "d.pwa03@example.test", pwaValidPHC, 0, 0},
			{"email too short", "a@", pwaValidPHC, 1, 0},
			{"email non-ASCII", "é.pwa03@example.test", pwaValidPHC, 1, 0},
			{"email uppercase", "E.PWA03@example.test", pwaValidPHC, 1, 0},
			{"email with space", "e f.pwa03@example.test", pwaValidPHC, 1, 0},
			{"email no at", "nodomain.pwa03", pwaValidPHC, 1, 0},
			{"email two at", "x@y@z.pwa03", pwaValidPHC, 1, 0},
			{"email over 254", strings.Repeat("a", 250) + "@b.cd", pwaValidPHC, 1, 0},
			{"hash argon2i", "f.pwa03@example.test", strings.Replace(pwaValidPHC, "argon2id", "argon2i", 1), 1, 0},
			{"hash argon2d", "g.pwa03@example.test", strings.Replace(pwaValidPHC, "argon2id", "argon2d", 1), 1, 0},
			{"hash bcrypt-shaped", "h.pwa03@example.test", "$2b$12$" + strings.Repeat("A", 53), 1, 0},
			{"hash bad version", "i.pwa03@example.test", strings.Replace(pwaValidPHC, "v=19", "v=16", 1), 1, 0},
			{"hash short salt", "j.pwa03@example.test", strings.Replace(pwaValidPHC, strings.Repeat("A", 22), strings.Repeat("A", 21), 1), 1, 0},
			{"hash empty", "k.pwa03@example.test", "", 1, 0},
		} {
			p := randomUUID()
			mustExec(t, owner, `INSERT INTO identity.principals(id) VALUES ($1::uuid)`, p)
			pwa03Check(t, owner, "password_credentials "+c.name, credInsert, p, c.email, c.hash, c.version, c.failed)
		}
		if _, err := owner.Exec(ctx, credInsert, principal, "dup.pwa03@example.test", pwaValidPHC, 1, 0); sqlState(err) != "23505" {
			t.Errorf("second credential for one principal: %v, want 23505 (PK principal_id)", err)
		}
		p2 := randomUUID()
		mustExec(t, owner, `INSERT INTO identity.principals(id) VALUES ($1::uuid)`, p2)
		if _, err := owner.Exec(ctx, credInsert, p2, email, pwaValidPHC, 1, 0); sqlState(err) != "23505" {
			t.Errorf("second credential for one email: %v, want 23505 (UNIQUE email)", err)
		}
		if _, err := owner.Exec(ctx, `INSERT INTO identity.password_credentials(principal_id,email,password_hash,email_verified_at) VALUES ($1::uuid,$2,$3,now())`, p2, "nover.pwa03@example.test", pwaValidPHC); err != nil {
			t.Errorf("credential defaults (version 1, failed 0) rejected: %v", err)
		}
		var v, failed int
		var disabled any
		if err := owner.QueryRow(ctx, `SELECT password_version, failed_count, disabled_at FROM identity.password_credentials WHERE email='nover.pwa03@example.test'`).Scan(&v, &failed, &disabled); err != nil || v != 1 || failed != 0 || disabled != nil {
			t.Errorf("defaults: version=%d failed=%d disabled=%v err=%v", v, failed, disabled, err)
		}

		// email_challenges: purpose shape (signup <=> no principal + pending hash; login <=> principal + version;
		// reset <=> principal only), lengths, enums.
		chIns := `INSERT INTO identity.email_challenges(id,purpose,email,locale,principal_id,password_version,pending_password_hash,binding_hash,code_hmac,ip_hmac,attempts,expires_at,consumed_reason,mail_state,provider_message_id)
		          VALUES ($1::uuid,$2,$3,$4,$5::uuid,$6,$7,$8,$9,$10,$11,now()+interval '10 minutes',$12,$13,$14)`
		type ch struct {
			purpose, email, locale string
			principal              *string
			version                *int64
			pending                *string
			binding, code, ip      []byte
			attempts               int
			reason                 *string
			mailState              string
			msgID                  *string
		}
		one := int64(1)
		pid := principal
		hash := pwaValidPHC
		good := map[string]ch{
			"signup": {purpose: "signup", email: "s.pwa03@example.test", locale: "en", pending: &hash, mailState: "PENDING"},
			"login":  {purpose: "login", email: "l.pwa03@example.test", locale: "zh-CN", principal: &pid, version: &one, mailState: "PENDING"},
			"reset":  {purpose: "reset", email: "r.pwa03@example.test", locale: "zh-TW", principal: &pid, mailState: "PENDING"},
		}
		insert := func(c ch) error {
			if c.binding == nil {
				c.binding = randomBytes(32)
			}
			if c.code == nil {
				c.code = bytes32('c')
			}
			if c.ip == nil {
				c.ip = bytes32('i')
			}
			_, err := owner.Exec(ctx, chIns, randomUUID(), c.purpose, c.email, c.locale, c.principal, c.version, c.pending, c.binding, c.code, c.ip, c.attempts, c.reason, c.mailState, c.msgID)
			return err
		}
		for name, c := range good {
			if err := insert(c); err != nil {
				t.Fatalf("valid %s challenge rejected: %v", name, err)
			}
		}
		bad := func(name string, mut func(*ch), base string) {
			c := good[base]
			mut(&c)
			if err := insert(c); sqlState(err) != "23514" {
				t.Errorf("email_challenges %s: SQLSTATE %q (err %v), want 23514", name, sqlState(err), err)
			}
		}
		empty := ""
		longID := strings.Repeat("m", 129)
		wrong := "invalid"
		bad("purpose outside enum", func(c *ch) { c.purpose = "other" }, "signup")
		bad("locale outside enum", func(c *ch) { c.locale = "fr" }, "signup")
		bad("signup with principal", func(c *ch) { c.principal = &pid }, "signup")
		bad("signup without pending hash", func(c *ch) { c.pending = nil }, "signup")
		bad("signup pending hash not PHC", func(c *ch) { c.pending = &empty }, "signup")
		bad("login without principal", func(c *ch) { c.principal = nil }, "login")
		bad("login without version", func(c *ch) { c.version = nil }, "login")
		bad("login with pending hash", func(c *ch) { c.pending = &hash }, "login")
		bad("reset with version", func(c *ch) { c.version = &one }, "reset")
		bad("reset with pending hash", func(c *ch) { c.pending = &hash }, "reset")
		bad("reset without principal", func(c *ch) { c.principal = nil }, "reset")
		bad("binding_hash 31 bytes", func(c *ch) { c.binding = randomBytes(31) }, "signup")
		bad("code_hmac 31 bytes", func(c *ch) { c.code = randomBytes(31) }, "signup")
		bad("ip_hmac 31 bytes", func(c *ch) { c.ip = randomBytes(31) }, "signup")
		bad("ip_hmac 33 bytes", func(c *ch) { c.ip = randomBytes(33) }, "signup")
		bad("attempts 6", func(c *ch) { c.attempts = 6 }, "signup")
		bad("consumed_reason outside enum", func(c *ch) { c.reason = &wrong }, "signup")
		bad("mail_state outside enum", func(c *ch) { c.mailState = "QUEUED" }, "signup")
		bad("provider_message_id 129", func(c *ch) { c.msgID = &longID }, "signup")
		bad("recipient non-ASCII", func(c *ch) { c.email = "é@example.test" }, "signup")
		bad("recipient uppercase", func(c *ch) { c.email = "UP@example.test" }, "signup")
		bad("recipient two at", func(c *ch) { c.email = "a@b@c.test" }, "signup")
		dupBinding := randomBytes(32)
		if err := insert(ch{purpose: "signup", email: "u1.pwa03@example.test", locale: "en", pending: &hash, mailState: "PENDING", binding: dupBinding}); err != nil {
			t.Fatal(err)
		}
		if err := insert(ch{purpose: "signup", email: "u2.pwa03@example.test", locale: "en", pending: &hash, mailState: "PENDING", binding: dupBinding}); sqlState(err) != "23505" {
			t.Errorf("duplicate binding_hash: %v, want 23505", err)
		}
		// defaults: attempts 0, mail_state PENDING, expires 10 min from creation is set by the caller/SQL (row supplied).

		// auth_throttle: 32-byte bucket, hits default/positive, PK (bucket, window_start).
		bucket := randomBytes(32)
		mustExec(t, owner, `INSERT INTO identity.auth_throttle(bucket,window_start,hits) VALUES ($1,now(),1)`, bucket)
		if _, err := owner.Exec(ctx, `INSERT INTO identity.auth_throttle(bucket,window_start,hits) VALUES ($1,now()+interval '1 day',1)`, randomBytes(31)); err == nil {
			t.Error("auth_throttle accepted a 31-byte bucket")
		}

		// auth_events: every listed action accepted (operator.unlocked included), unknown rejected, no raw data columns.
		for _, a := range pwa03Actions {
			if _, err := owner.Exec(ctx, `INSERT INTO identity.auth_events(action) VALUES ($1)`, a); err != nil {
				t.Errorf("auth_events action %q rejected: %v", a, err)
			}
		}
		pwa03Check(t, owner, "auth_events unknown action", `INSERT INTO identity.auth_events(action) VALUES ('operator.deactivated')`)
		pwa03Check(t, owner, "auth_events email_hmac 31 bytes", `INSERT INTO identity.auth_events(action,email_hmac) VALUES ('throttled',$1)`, randomBytes(31))
		pwa03Check(t, owner, "auth_events ip_hmac 31 bytes", `INSERT INTO identity.auth_events(action,ip_hmac) VALUES ('throttled',$1)`, randomBytes(31))
	})

	t.Run("comments_on_every_object", func(t *testing.T) {
		// PROCESS §5 SQL rule: COMMENT ON every new table, column, function.
		for _, tbl := range pwa03Tables {
			var tableComment *string
			if err := f.owner.QueryRow(ctx, `SELECT obj_description(('identity.'||$1)::regclass,'pg_class')`, tbl).Scan(&tableComment); err != nil || tableComment == nil || len(strings.TrimSpace(*tableComment)) < 20 {
				t.Errorf("identity.%s has no table comment", tbl)
			}
			rows, err := f.owner.Query(ctx, `SELECT a.attname FROM pg_attribute a WHERE a.attrelid=('identity.'||$1)::regclass AND a.attnum>0 AND NOT a.attisdropped AND col_description(a.attrelid,a.attnum) IS NULL`, tbl)
			if err != nil {
				t.Fatal(err)
			}
			for rows.Next() {
				var c string
				_ = rows.Scan(&c)
				t.Errorf("identity.%s.%s has no column comment", tbl, c)
			}
			rows.Close()
		}
		for name := range pwa03Funcs {
			var c *string
			if err := f.owner.QueryRow(ctx, `SELECT obj_description(p.oid,'pg_proc') FROM pg_proc p JOIN pg_namespace n ON n.oid=p.pronamespace WHERE n.nspname='identity' AND p.proname=$1`, name).Scan(&c); err != nil || c == nil || len(strings.TrimSpace(*c)) < 20 {
				t.Errorf("identity.%s has no function comment", name)
			}
		}
	})

	t.Run("upgrade_from_populated_0066", func(t *testing.T) {
		// Throw-away container (pwIsolatedFixture pattern): the shared DB is not touched.
		iso := pwIsolatedFixture(t)
		o := iso.owner
		// Rebuild the 0066 state: remove exactly what 0070 created and its ledger row (disclosed).
		pwaDisclose(t, "drop the eight 0070 functions, the four tables and the 0070 ledger row in a throw-away container to reproduce the 0066 state before the real migrations.Apply")
		for name, args := range pwa03Funcs {
			mustExec(t, o, fmt.Sprintf(`DROP FUNCTION identity.%s(%s)`, name, args))
		}
		mustExec(t, o, `DROP TABLE identity.auth_events, identity.auth_throttle, identity.email_challenges, identity.password_credentials CASCADE`)
		mustExec(t, o, `DELETE FROM public.lc_schema_migrations WHERE version='0070_merchant_password_auth.sql'`)
		var newest string
		// R2 integration: the other R2 lanes' migrations (0071..0080) are applied here too, so "the 0066 state" is
		// asserted as: 0070 itself is not in the ledger and its predecessor in ledger order is 0066. The whole R2 set
		// as one upgrade of the release head is TestR2IntegrationUpgradeFromReleaseHead.
		if err := o.QueryRow(ctx, `SELECT max(version) FROM public.lc_schema_migrations WHERE version ~ '^[0-9]{4}_' AND version < '0071'`).Scan(&newest); err != nil || !strings.HasPrefix(newest, "0066_") {
			t.Fatalf("newest applied migration below 0071 = %q (%v), want 0066_*", newest, err)
		}
		// Populate: a store owner through the real definers already exists from the seed; add sessions/principals.
		for range 3 {
			p := randomUUID()
			mustExec(t, o, `INSERT INTO identity.principals(id) VALUES ($1::uuid)`, p)
			mustExec(t, o, `INSERT INTO identity.sessions(id,token_hash,principal_id,audience,expires_at) VALUES ($1::uuid,$2,$3::uuid,'merchant',now()+interval '1 hour')`, randomUUID(), tokenHash(randomToken()), p)
		}
		count := func(q string) int64 {
			var n int64
			if err := o.QueryRow(ctx, q).Scan(&n); err != nil {
				t.Fatal(err)
			}
			return n
		}
		before := []int64{count(`SELECT count(*) FROM identity.principals`), count(`SELECT count(*) FROM identity.sessions`), count(`SELECT count(*) FROM identity.memberships`), count(`SELECT count(*) FROM control.tenants`)}
		if before[0] < 4 || before[1] < 4 {
			t.Fatalf("upgrade DB is not populated: %v", before)
		}
		if err := migrations.Apply(ctx, o); err != nil {
			t.Fatalf("Apply over populated 0066 DB: %v", err)
		}
		if err := migrations.Apply(ctx, o); err != nil {
			t.Fatalf("second Apply (idempotent): %v", err)
		}
		after := []int64{count(`SELECT count(*) FROM identity.principals`), count(`SELECT count(*) FROM identity.sessions`), count(`SELECT count(*) FROM identity.memberships`), count(`SELECT count(*) FROM control.tenants`)}
		for i := range before {
			if before[i] != after[i] {
				t.Errorf("upgrade changed pre-existing row counts: before %v after %v", before, after)
			}
		}
		if n := count(`SELECT count(*) FROM public.lc_schema_migrations WHERE version='0070_merchant_password_auth.sql'`); n != 1 {
			t.Errorf("0070 ledger rows = %d", n)
		}
		for _, tbl := range pwa03Tables {
			if n := count("SELECT count(*) FROM identity." + tbl); n != 0 {
				t.Errorf("identity.%s has %d rows after upgrade, want 0", tbl, n)
			}
		}
		pwaAssertSchema(t, o)

		// OpenIdentityPool still admits an identity login after 0070.
		role := "pwa03_" + strings.ReplaceAll(randomUUID(), "-", "")
		secret := randomToken()
		mustExec(t, o, `CREATE ROLE `+pgx.Identifier{role}.Sanitize()+` LOGIN NOSUPERUSER NOBYPASSRLS NOCREATEDB NOCREATEROLE IN ROLE commerce_identity PASSWORD '`+secret+`'`)
		u, err := url.Parse(iso.databaseURL)
		if err != nil {
			t.Fatal(err)
		}
		u.User = url.UserPassword(role, secret)
		pool, err := platform.OpenIdentityPool(ctx, u.String())
		if err != nil {
			t.Fatalf("OpenIdentityPool rejects the identity login after 0070: %v", err)
		}
		defer pool.Close()
		var rowsSeen int
		rows, err := pool.Query(ctx, `SELECT * FROM identity.password_login_material('nobody.pwa03@example.test')`)
		if err != nil {
			t.Fatalf("identity login cannot execute the definers: %v", err)
		}
		for rows.Next() {
			rowsSeen++
		}
		rows.Close()
		if rowsSeen != 0 {
			t.Errorf("password_login_material for an unknown email returned %d rows", rowsSeen)
		}
		var denied error
		_, denied = pool.Exec(ctx, `SELECT count(*) FROM identity.password_credentials`)
		var pgErr interface{ SQLState() string }
		if !errors.As(denied, &pgErr) || pgErr.SQLState() != "42501" {
			t.Errorf("identity login read a table directly: %v, want 42501", denied)
		}
	})
}
