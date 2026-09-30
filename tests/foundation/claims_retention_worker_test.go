// claims_retention_worker_test.go is the independent CRP09 gate of U08 (contract claims-retention-purge-v1 §5 + §7
// row CRP09): the real cmd/claims-worker binary, built and run against the disposable PG with real River. Evidence
// label: MOCK (River is real, the clock is not: the hourly cadence itself is NOT observed; RunOnStart, the
// one-job-per-hour uniqueness and the batch loop are). Pattern of expiry_runtime_test.go / mrLaunch: build once, run
// with an explicit environment, watch the process log and the database, stop with a signal.
package foundation_test

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

// crp09Sentinel is a synthetic password; leak assertions search for it (PROCESS §6).
const crp09Sentinel = "sentinel-crp09-pw"

// crWorkerEnv is the complete environment of a real claims-worker (names of meta-intake-reply.md + §5). The three
// logins are real LOGIN roles granted exactly their authority; retentionDSN overrides the retention-job login.
type crWorkerEnv struct {
	env                               []string
	jobUser, workerUser, jobDSNSecret string
	linkRaw, pageKeyRaw               []byte
}

func crLoginDSN(t *testing.T, f *testFixture, setRole bool, authorities ...string) (dsn, user, password string) {
	t.Helper()
	user = "cr_w_" + strings.ReplaceAll(randomUUID(), "-", "")
	password = hex.EncodeToString(randomBytes(24)) // random per run, never a stored secret
	id := pgx.Identifier{user}.Sanitize()
	mustExec(t, f.owner, `CREATE ROLE `+id+` LOGIN NOSUPERUSER NOBYPASSRLS NOCREATEDB NOCREATEROLE NOREPLICATION PASSWORD '`+password+`'`)
	for _, a := range authorities {
		mustExec(t, f.owner, `GRANT `+pgx.Identifier{a}.Sanitize()+` TO `+id+` WITH INHERIT TRUE, SET `+map[bool]string{true: "TRUE", false: "FALSE"}[setRole])
	}
	t.Cleanup(func() {
		for _, a := range authorities {
			_, _ = f.owner.Exec(context.Background(), `REVOKE `+pgx.Identifier{a}.Sanitize()+` FROM `+id)
		}
		_, _ = f.owner.Exec(context.Background(), `DROP ROLE `+id)
	})
	return roleURL(t, f.databaseURL, user, password), user, password
}

func crNewWorkerEnv(t *testing.T, f *testFixture) *crWorkerEnv {
	t.Helper()
	w := &crWorkerEnv{linkRaw: randomBytes(32), pageKeyRaw: randomBytes(32)}
	intake, _, _ := crLoginDSN(t, f, false, "commerce_claims_intake")
	worker, workerUser, _ := crLoginDSN(t, f, false, "commerce_worker")
	job, jobUser, jobPassword := crLoginDSN(t, f, false, "commerce_retention_job")
	w.workerUser, w.jobUser, w.jobDSNSecret = workerUser, jobUser, jobPassword
	w.env = []string{
		"COMMERCE_CLAIMS_WORKER_ENABLED=1",
		"COMMERCE_CLAIMS_INTAKE_DATABASE_URL=" + intake,
		"COMMERCE_WORKER_DATABASE_URL=" + worker,
		"COMMERCE_RETENTION_JOB_DATABASE_URL=" + job,
		"COMMERCE_CLAIMS_REPLY_LINK_KEY=" + base64.StdEncoding.EncodeToString(w.linkRaw),
		"COMMERCE_META_PAGE_TOKEN_ACTIVE_KEY_ID=pt_key_1",
		`COMMERCE_META_PAGE_TOKEN_KEYS_JSON={"keys":[{"id":"pt_key_1","key_base64":"` + base64.StdEncoding.EncodeToString(w.pageKeyRaw) + `"}]}`,
		"COMMERCE_META_GRAPH_VERSION=v99.0",
	}
	return w
}

// with returns a copy of the environment with name set to value ("" value and unset=true removes it).
func (w *crWorkerEnv) with(name, value string, unset bool) []string {
	var out []string
	for _, kv := range w.env {
		if !strings.HasPrefix(kv, name+"=") {
			out = append(out, kv)
		}
	}
	if !unset {
		out = append(out, name+"="+value)
	}
	return out
}

func crReadLog(t *testing.T, p *mrProcess) string {
	t.Helper()
	b, err := os.ReadFile(p.logPath)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// crLaunch starts the worker and waits for its ready witness (claims_worker_ready).
func crLaunch(t *testing.T, binary string, env []string) *mrProcess {
	t.Helper()
	p := mrLaunch(t, binary, "claims-worker", env)
	deadline := time.Now().Add(25 * time.Second)
	for time.Now().Before(deadline) {
		if strings.Contains(crReadLog(t, p), "claims_worker_ready") {
			return p
		}
		select {
		case err := <-p.done:
			p.exited = true
			t.Fatalf("claims-worker exited before ready: %v log=%s", err, crReadLog(t, p))
		default:
		}
		time.Sleep(25 * time.Millisecond) // observes the process log only
	}
	t.Fatalf("claims-worker never became ready: %s", crReadLog(t, p))
	return nil
}

type crJob struct {
	id             int64
	state          string
	attempt        int
	queue, args    string
	errors         string
	createdSeconds float64
}

func (e *crEnv) retentionJobs() []crJob {
	e.t.Helper()
	rows, err := e.f.owner.Query(context.Background(), `SELECT id,state::text,attempt,queue,args::text,coalesce(errors::text,'') FROM river.river_job WHERE kind='claims_retention_v1' ORDER BY id`)
	if err != nil {
		e.t.Fatal(err)
	}
	defer rows.Close()
	var out []crJob
	for rows.Next() {
		var j crJob
		if err := rows.Scan(&j.id, &j.state, &j.attempt, &j.queue, &j.args, &j.errors); err != nil {
			e.t.Fatal(err)
		}
		out = append(out, j)
	}
	return out
}

func (e *crEnv) clearJobs() {
	e.t.Helper()
	mustExec(e.t, e.f.owner, `DELETE FROM river.river_job WHERE kind='claims_retention_v1'`)
}

// awaitJob polls the River table (observation only) until the single retention job reaches state.
func (e *crEnv) awaitJob(state string, timeout time.Duration) crJob {
	e.t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if jobs := e.retentionJobs(); len(jobs) == 1 && jobs[0].state == state {
			return jobs[0]
		}
		time.Sleep(50 * time.Millisecond) // polls River's table
	}
	e.t.Fatalf("no claims_retention_v1 job reached %s: %+v", state, e.retentionJobs())
	return crJob{}
}

// runRows returns the `run` log rows written by login since the DB timestamp since, oldest first, as their counts.
func (e *crEnv) runRows(login string, since time.Time) []map[string]float64 {
	e.t.Helper()
	rows, err := e.f.owner.Query(context.Background(), `SELECT counts::text FROM claims.retention_log WHERE kind='run' AND executed_by=$1 AND created_at>=$2 ORDER BY created_at,id`, login, since)
	if err != nil {
		e.t.Fatal(err)
	}
	defer rows.Close()
	var out []map[string]float64
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			e.t.Fatal(err)
		}
		m := map[string]float64{}
		if err := crUnmarshalNumbers(raw, m); err != nil {
			e.t.Fatal(err)
		}
		out = append(out, m)
	}
	return out
}

func (e *crEnv) dbNow() time.Time {
	e.t.Helper()
	var now time.Time
	if err := e.f.owner.QueryRow(context.Background(), `SELECT clock_timestamp()`).Scan(&now); err != nil {
		e.t.Fatal(err)
	}
	return now
}

// seedIntake bulk-inserts n terminal (DROPPED) intake rows received at the SQL expression (owner-seeded, synthetic).
func (e *crEnv) seedIntake(m crMeta, n int, expr string) {
	e.t.Helper()
	mustExec(e.t, e.f.owner, `INSERT INTO claims.meta_intake(tenant_id,store_id,inbox_event_id,source_id,session_id,platform,app_id,object,asset_id,comment_ref,live_media,actor_key,
		occurred_at,received_at,grammar_version,grammar_kind,state,drop_reason,not_before)
		SELECT $1,$2,gen_random_uuid(),$3,$4,'facebook',$5,'page',$6,$6||'_'||g,false,repeat('a',64),`+expr+`,`+expr+`,'kw-v1','NO_MATCH','DROPPED','window_closed',`+expr+`
		FROM generate_series(1,$7::int) g`, e.w.tenant, m.sess.store, m.source, m.sess.id, miApp, m.asset, n)
}

// CRP09 (MOCK River): the real claims-worker registers the periodic job, runs it on the retention-job login, stops its
// batch loop on more=0 / busy / 20 batches, retries after a database fault with the earlier batches committed, refuses
// a missing/invalid/mixed retention DSN with a fixed code, and the deploy config carries the job DSN only.
func TestClaimsRetentionCRP09Worker(t *testing.T) {
	e := crSetup(t)
	f := e.f
	ctx := context.Background()
	binary := mrBuild(t, "../../cmd/claims-worker", "claims-worker")
	we := crNewWorkerEnv(t, f)
	// a synthetic session/source the bulk rows hang on
	sessionOf := func() crMeta {
		s := e.w.session(t, e.w.store)
		e.w.closedAt(t, s, crAgo(1, 0))
		return e.w.source(t, s, "page")
	}

	e.sub(t, "start-registers-one-job-and-runs-on-the-job-login", func(t *testing.T) {
		e.clearJobs()
		defer e.clearJobs()
		s := e.w.session(t, e.w.store)
		e.w.closedAt(t, s, crAgo(1, 0))
		b := e.w.bundle(t, s, "manual", "", "w09-"+t04Tag())
		e.w.link(t, s, b, crOld(7)) // one expired link: the run has something to purge
		mark := e.dbNow()
		var stdoutBefore = len(e.retentionJobs())
		if stdoutBefore != 0 {
			t.Fatalf("stale claims_retention_v1 jobs before the start: %d", stdoutBefore)
		}
		p := crLaunch(t, binary, we.env)
		job := e.awaitJob("completed", 20*time.Second)
		if job.queue != "default" || job.args != "{}" || job.attempt != 1 {
			t.Errorf("job shape: queue=%q args=%q attempt=%d, want default / {} / 1", job.queue, job.args, job.attempt)
		}
		if n := len(e.retentionJobs()); n != 1 {
			t.Errorf("RunOnStart inserted %d claims_retention_v1 jobs, want exactly 1", n)
		}
		rows := e.runRows(we.jobUser, mark)
		if len(rows) != 1 || rows[0]["enforced"] != 1 || rows[0]["links"] != 1 {
			t.Errorf("run rows by the retention-job login: %v, want one enforced run purging the expired link", rows)
		}
		if n := crCount(t, f.owner, `SELECT count(*) FROM claims.retention_log WHERE kind='run' AND executed_by=$1 AND created_at>=$2`, we.workerUser, mark); n != 0 {
			t.Errorf("%d run rows were written by the commerce_worker login (the purge must run on lc_retention_job)", n)
		}
		if n := crCount(t, f.owner, `SELECT count(*) FROM claims.links WHERE bundle_id=$1`, b.id); n != 0 {
			t.Error("the job did not purge the expired link")
		}
		// the job row itself was inserted by the worker login and executed on the default queue of the main schema
		if n := crCount(t, f.owner, `SELECT count(*) FROM river.river_job WHERE kind='claims_retention_v1' AND state='completed' AND finalized_at IS NOT NULL`); n != 1 {
			t.Error("job not finalized")
		}
		log := crReadLog(t, p)
		if !strings.Contains(log, "claims_retention_run") {
			t.Errorf("no claims_retention_run line in the worker log: %s", log)
		}
		if strings.Contains(log, we.jobDSNSecret) || regexp.MustCompile(`[0-9a-f]{64}`).MatchString(log) {
			t.Error("the worker log holds a credential or a 64-hex key")
		}

		// restart inside the hour: no second job, no second run
		mrStop(t, p, syscall.SIGTERM, true)
		hour := time.Now().Truncate(time.Hour)
		p2 := crLaunch(t, binary, we.env)
		time.Sleep(3 * time.Second) // bounded NEGATIVE observation: RunOnStart fires at client start, well inside this window
		jobs := e.retentionJobs()
		if time.Now().Truncate(time.Hour) != hour {
			t.Skip("the hour boundary passed during the restart window (uniqueness is per hour); rerun")
		}
		if len(jobs) != 1 || jobs[0].id != job.id || jobs[0].state != "completed" {
			t.Errorf("a restart within the hour changed the job set: %+v (want the one original completed job)", jobs)
		}
		if rows := e.runRows(we.jobUser, mark); len(rows) != 1 {
			t.Errorf("a restart within the hour ran the purge again: %d run rows", len(rows))
		}
		mrStop(t, p2, syscall.SIGTERM, true)
	})

	e.sub(t, "batch-loop-stops", func(t *testing.T) {
		e.clearJobs()
		defer e.clearJobs()
		m := sessionOf()
		// more=0: 1200 terminal intake rows -> batches of 500, 500, 200 and then the job stops.
		e.seedIntake(m, 1200, crOld(30))
		mark := e.dbNow()
		p := crLaunch(t, binary, we.env)
		e.awaitJob("completed", 40*time.Second)
		rows := e.runRows(we.jobUser, mark)
		if len(rows) != 3 || rows[0]["intake"] != 500 || rows[1]["intake"] != 500 || rows[2]["intake"] != 200 || rows[0]["more"] != 1 || rows[1]["more"] != 1 || rows[2]["more"] != 0 {
			t.Errorf("batches %v, want intake 500/500/200 with more 1/1/0 (stop on more=0)", rows)
		}
		if n := crCount(t, f.owner, `SELECT count(*) FROM claims.meta_intake WHERE asset_id=$1`, m.asset); n != 0 {
			t.Errorf("%d intake rows left after more=0", n)
		}
		mrStop(t, p, syscall.SIGTERM, true)
		e.clearJobs()

		// 20 batches: 10001 rows -> exactly 20 batches of 500, one row left, the job still completes (next hour continues).
		e.seedIntake(m, 10001, crOld(30))
		mark = e.dbNow()
		p = crLaunch(t, binary, we.env)
		job := e.awaitJob("completed", 90*time.Second)
		rows = e.runRows(we.jobUser, mark)
		if len(rows) != 20 || job.attempt != 1 {
			t.Errorf("%d batches (attempt %d), want exactly 20 and one attempt", len(rows), job.attempt)
		}
		if n := crCount(t, f.owner, `SELECT count(*) FROM claims.meta_intake WHERE asset_id=$1`, m.asset); n != 1 {
			t.Errorf("%d intake rows left after the 20-batch cap, want 1", n)
		}
		if len(rows) == 20 && rows[19]["more"] != 1 {
			t.Errorf("the 20th batch reports more=%v, want 1 (work remains for the next hour)", rows[19]["more"])
		}
		mrStop(t, p, syscall.SIGTERM, true)
		e.clearJobs()
		mustExec(t, f.owner, `DELETE FROM claims.meta_intake WHERE asset_id=$1`, m.asset)

		// busy: another holder of the advisory key -> the batch reports busy, writes nothing, the job stops and succeeds.
		e.seedIntake(m, 10, crOld(30))
		hold, err := f.owner.Acquire(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := hold.Exec(ctx, `SELECT pg_advisory_lock(hashtextextended('claims-retention',0))`); err != nil {
			t.Fatal(err)
		}
		released := false
		release := func() {
			if !released {
				released = true
				_, _ = hold.Exec(ctx, `SELECT pg_advisory_unlock(hashtextextended('claims-retention',0))`)
				hold.Release()
			}
		}
		t.Cleanup(release)
		mark = e.dbNow()
		p = crLaunch(t, binary, we.env)
		job = e.awaitJob("completed", 30*time.Second)
		if job.attempt != 1 {
			t.Errorf("busy run attempt=%d, want 1 (busy is not an error)", job.attempt)
		}
		if rows := e.runRows(we.jobUser, mark); len(rows) != 0 {
			t.Errorf("a busy run wrote %d log rows", len(rows))
		}
		if n := crCount(t, f.owner, `SELECT count(*) FROM claims.meta_intake WHERE asset_id=$1`, m.asset); n != 10 {
			t.Errorf("a busy run changed the data: %d rows", n)
		}
		release()
		mrStop(t, p, syscall.SIGTERM, true)
	})

	e.sub(t, "database-fault-is-a-river-retry-with-earlier-batches-committed", func(t *testing.T) {
		e.clearJobs()
		defer e.clearJobs()
		m := sessionOf()
		e.seedIntake(m, 1200, crOld(30))
		mark := e.dbNow()
		// Injected fault (owner-created, removed below): the third batch's run-log insert fails, so that batch's whole
		// transaction rolls back while batches one and two, each its own transaction, stay committed.
		mustExec(t, f.owner, fmt.Sprintf(`CREATE FUNCTION public.crp09_fail() RETURNS trigger LANGUAGE plpgsql AS $$
			BEGIN IF NEW.kind='run' AND (SELECT count(*) FROM claims.retention_log WHERE kind='run' AND created_at >= '%s') >= 2 THEN
			 RAISE EXCEPTION 'crp09 injected fault' USING ERRCODE='XX001'; END IF; RETURN NEW; END $$`, mark.UTC().Format("2006-01-02 15:04:05.000000+00")))
		mustExec(t, f.owner, `CREATE TRIGGER crp09_fail BEFORE INSERT ON claims.retention_log FOR EACH ROW EXECUTE FUNCTION public.crp09_fail()`)
		dropped := false
		drop := func() {
			if !dropped {
				dropped = true
				_, _ = f.owner.Exec(ctx, `DROP TRIGGER IF EXISTS crp09_fail ON claims.retention_log`)
				_, _ = f.owner.Exec(ctx, `DROP FUNCTION IF EXISTS public.crp09_fail()`)
			}
		}
		t.Cleanup(drop)
		p := crLaunch(t, binary, we.env)
		deadline := time.Now().Add(30 * time.Second)
		var job crJob
		for time.Now().Before(deadline) {
			if jobs := e.retentionJobs(); len(jobs) == 1 && jobs[0].state == "retryable" {
				job = jobs[0]
				break
			}
			time.Sleep(50 * time.Millisecond)
		}
		if job.id == 0 {
			t.Fatalf("the faulted job never became retryable: %+v", e.retentionJobs())
		}
		if rows := e.runRows(we.jobUser, mark); len(rows) != 2 {
			t.Errorf("%d committed batches before the fault, want 2 (the failed batch rolls back completely)", len(rows))
		}
		if n := crCount(t, f.owner, `SELECT count(*) FROM claims.meta_intake WHERE asset_id=$1`, m.asset); n != 200 {
			t.Errorf("%d intake rows remain after the fault, want 200 (two committed batches of 500, the third rolled back)", n)
		}
		if strings.Contains(job.errors, "crp09 injected fault") || strings.Contains(job.errors, we.jobDSNSecret) {
			t.Errorf("the River job error carries the driver message or a credential: %s", job.errors)
		}
		drop()
		job = e.awaitJob("completed", 60*time.Second)
		if job.attempt < 2 {
			t.Errorf("job completed on attempt %d, want a River retry (attempt >= 2)", job.attempt)
		}
		if n := crCount(t, f.owner, `SELECT count(*) FROM claims.meta_intake WHERE asset_id=$1`, m.asset); n != 0 {
			t.Errorf("%d rows left after the retry", n)
		}
		mrStop(t, p, syscall.SIGTERM, true)
	})

	e.sub(t, "refuses-to-start-with-a-bad-retention-dsn", func(t *testing.T) {
		e.clearJobs()
		defer e.clearJobs()
		mixed, _, mixedPassword := crLoginDSN(t, f, false, "commerce_retention_job", "commerce_worker")
		setRole, _, setPassword := crLoginDSN(t, f, true, "commerce_retention_job")
		operator, _, operatorPassword := crLoginDSN(t, f, false, "commerce_retention_operator")
		plainWorker, _, workerPassword := crLoginDSN(t, f, false, "commerce_worker")
		mustExec(t, f.owner, `CREATE DATABASE lc_crp09_other`)
		t.Cleanup(func() { _, _ = f.owner.Exec(ctx, `DROP DATABASE IF EXISTS lc_crp09_other WITH (FORCE)`) })
		u, err := url.Parse(strings.TrimPrefix(we.env[3], "COMMERCE_RETENTION_JOB_DATABASE_URL="))
		if err != nil {
			t.Fatal(err)
		}
		u.Path = "/lc_crp09_other"
		crp09Unreachable := (&url.URL{Scheme: "postgres", User: url.UserPassword("nobody", crp09Sentinel), Host: "127.0.0.1:1", Path: "/x", RawQuery: "connect_timeout=2"}).String()
		cases := []struct {
			name, code string
			env        []string
			secret     string
		}{
			{"missing", "claims_worker_invalid_config", we.with("COMMERCE_RETENTION_JOB_DATABASE_URL", "", true), ""},
			{"empty", "claims_worker_invalid_config", we.with("COMMERCE_RETENTION_JOB_DATABASE_URL", "", false), ""},
			{"whitespace", "claims_worker_invalid_config", we.with("COMMERCE_RETENTION_JOB_DATABASE_URL", "   ", false), ""},
			{"not a DSN", "claims_worker_database_unavailable", we.with("COMMERCE_RETENTION_JOB_DATABASE_URL", "not a dsn %%", false), ""},
			{"unreachable host", "claims_worker_database_unavailable", we.with("COMMERCE_RETENTION_JOB_DATABASE_URL", crp09Unreachable, false), crp09Sentinel},
			{"login reaching two authorities (job + worker)", "claims_worker_database_unavailable", we.with("COMMERCE_RETENTION_JOB_DATABASE_URL", mixed, false), mixedPassword},
			{"job login with SET ROLE", "claims_worker_database_unavailable", we.with("COMMERCE_RETENTION_JOB_DATABASE_URL", setRole, false), setPassword},
			{"the operator login", "claims_worker_database_unavailable", we.with("COMMERCE_RETENTION_JOB_DATABASE_URL", operator, false), operatorPassword},
			{"another authority (worker)", "claims_worker_database_unavailable", we.with("COMMERCE_RETENTION_JOB_DATABASE_URL", plainWorker, false), workerPassword},
			{"a different database than the worker pool", "claims_worker_database_unavailable", we.with("COMMERCE_RETENTION_JOB_DATABASE_URL", u.String(), false), we.jobDSNSecret},
		}
		for _, c := range cases {
			cmd := exec.Command(binary)
			cmd.Env = append([]string{"PATH=" + os.Getenv("PATH")}, c.env...)
			done := make(chan struct{})
			var out []byte
			var runErr error
			go func() {
				out, runErr = cmd.CombinedOutput()
				close(done)
			}()
			select {
			case <-done:
			case <-time.After(30 * time.Second):
				_ = cmd.Process.Kill()
				<-done
				t.Errorf("%s: the worker kept running (or hung) instead of refusing", c.name)
				continue
			}
			var exit *exec.ExitError
			if !errors.As(runErr, &exit) || exit.ExitCode() != 1 {
				t.Errorf("%s: exit %v, want a non-zero exit 1", c.name, runErr)
			}
			text := string(out)
			if !strings.Contains(text, c.code) {
				t.Errorf("%s: output %q lacks the fixed code %s", c.name, text, c.code)
			}
			for _, secret := range []string{c.secret, "postgres://", "password", "SQLSTATE"} {
				if secret != "" && strings.Contains(strings.ToLower(text), strings.ToLower(secret)) {
					t.Errorf("%s: output leaks %q: %q", c.name, secret, text)
				}
			}
			if strings.Contains(text, "claims_worker_ready") {
				t.Errorf("%s: the worker reported ready", c.name)
			}
		}
		if n := len(e.retentionJobs()); n != 0 {
			t.Errorf("a refused start inserted %d retention jobs", n)
		}
	})

	e.sub(t, "deploy-config-carries-the-job-dsn-only", func(t *testing.T) {
		read := func(rel string) string {
			b, err := os.ReadFile(filepath.Join("../..", rel))
			if err != nil {
				t.Fatalf("read %s: %v", rel, err)
			}
			return string(b)
		}
		compose := read("deploy/compose.yml")
		if !strings.Contains(compose, "COMMERCE_RETENTION_JOB_DATABASE_URL") {
			t.Error("deploy/compose.yml does not give claims-worker COMMERCE_RETENTION_JOB_DATABASE_URL")
		}
		logins := read("deploy/postgres/logins.tsv")
		if !strings.Contains(logins, "lc_retention_job\tcommerce_retention_job") || strings.Contains(logins, "lc_retention_operator") {
			t.Error("deploy/postgres/logins.tsv must provision lc_retention_job and never lc_retention_operator (ruling B25)")
		}
		manifest := read("deploy/secrets.manifest.tsv")
		if !strings.Contains(manifest, "dsn_lc_retention_job") || strings.Contains(manifest, "lc_retention_operator") {
			t.Error("deploy/secrets.manifest.tsv must carry dsn_lc_retention_job only")
		}
		smoke := read("deploy/scripts/smoke.sh")
		if !strings.Contains(smoke, "retention-admin status") || strings.Contains(smoke, "COMMERCE_RETENTION_OPERATOR_DATABASE_URL") {
			t.Error("deploy/scripts/smoke.sh must run `retention-admin status` on the job login and never reference the operator DSN")
		}
	})
}

// crUnmarshalNumbers decodes a JSON object of numbers into m.
func crUnmarshalNumbers(raw string, m map[string]float64) error {
	var generic map[string]any
	if err := json.Unmarshal([]byte(raw), &generic); err != nil {
		return err
	}
	for k, v := range generic {
		n, ok := v.(float64)
		if !ok {
			return fmt.Errorf("counts[%s] is %T, not a number", k, v)
		}
		m[k] = n
	}
	return nil
}
