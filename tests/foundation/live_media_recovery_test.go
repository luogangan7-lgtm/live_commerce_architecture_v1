package foundation_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"livecommerce/internal/platform"
	"livecommerce/migrations"
)

// MRR tests use the existing real-PG18 media fixture and its original River
// job. Elapsed arguments below exercise the SQL boundary; only the process
// test exercises the parent's real monotonic 90-second clock.
func mrrRecoveryPool(t *testing.T, h *lmeHarness) (*pgxpool.Pool, string) {
	t.Helper()
	login, pool := lmaLogin(t, h.lp.f, "commerce_media_recovery")
	name := pgx.Identifier{login}.Sanitize()
	mustExec(t, h.lp.f.owner, "REVOKE commerce_media_recovery FROM "+name)
	mustExec(t, h.lp.f.owner, "GRANT commerce_media_recovery TO "+name+" WITH INHERIT TRUE, SET FALSE")
	return pool, login
}

type mrrMember struct {
	disposition string
	episode     string
	operation   *string
	job         *int64
	baseline    *int64
	candidates  int
	known       bool
	blocked     *string
}

func mrrBegin(t *testing.T, pool *pgxpool.Pool, episode string, elapsed int64, capacity int, known bool) []mrrMember {
	t.Helper()
	rows, err := pool.Query(context.Background(), `SELECT disposition,episode_id::text,operation_id::text,job_id,
	 baseline_generation,candidate_count,coverage_known,blocked_by_episode_id::text
	 FROM live.begin_media_recovery_episode($1::uuid,$2::bigint,$3::integer,$4::boolean)`, episode, elapsed, capacity, known)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []mrrMember
	for rows.Next() {
		var row mrrMember
		if err := rows.Scan(&row.disposition, &row.episode, &row.operation, &row.job,
			&row.baseline, &row.candidates, &row.known, &row.blocked); err != nil {
			t.Fatal(err)
		}
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if len(out) == 0 {
		t.Fatal("begin returned no scope/member row")
	}
	return out
}

type mrrReadback struct {
	scope, disposition string
	known              bool
	candidates         int
	operation, obs     *string
	source             *string
	generation         *int64
	witnessElapsed     *int64
	timeoutAt          *time.Time
	cleanup            *bool
}

func mrrRead(t *testing.T, pool *pgxpool.Pool, episode string) []mrrReadback {
	t.Helper()
	rows, err := pool.Query(context.Background(), `SELECT scope_status,coverage_known,candidate_count,operation_id::text,
	 coalesce(disposition,''),observation_id::text,observation_source,observation_generation,witness_elapsed_ms,timeout_at,cleanup_required
	 FROM live.read_media_recovery_episode($1::uuid)`, episode)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []mrrReadback
	for rows.Next() {
		var row mrrReadback
		if err := rows.Scan(&row.scope, &row.known, &row.candidates, &row.operation,
			&row.disposition, &row.obs, &row.source, &row.generation, &row.witnessElapsed,
			&row.timeoutAt, &row.cleanup); err != nil {
			t.Fatal(err)
		}
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if len(out) == 0 {
		t.Fatal("committed recovery readback returned no row")
	}
	return out
}

func mrrReserveUnansweredStart(t *testing.T, h *lmeHarness) lmeLease {
	t.Helper()
	lease := h.claim(t, 30)
	if lease.disposition != "claimed" || lease.mode != "dispatch" {
		t.Fatalf("original dispatch claim: %+v", lease)
	}
	if err := h.reserve(t, lease); err != nil {
		t.Fatal(err)
	}
	if !h.facts(t).reserved {
		t.Fatal("original Start wire reservation did not commit")
	}
	return lease
}

func mrrClaim(t *testing.T, pool *pgxpool.Pool, episode string, h *lmeHarness, token []byte) (string, int64, *string) {
	t.Helper()
	var disposition string
	var generation *int64
	var project, endpoint, room, egress *string
	var credential *int64
	err := pool.QueryRow(context.Background(), `SELECT disposition,generation,project_id,credential_version,endpoint_identity,room_name,egress_id
	 FROM live.claim_recovery_observation($1::uuid,$2::uuid,$3::bigint,$4::bytea)`,
		episode, h.plan.OperationID, h.plan.JobID, token).Scan(&disposition, &generation, &project, &credential, &endpoint, &room, &egress)
	if err != nil {
		t.Fatal(err)
	}
	if disposition == "claimed" {
		if generation == nil || project == nil || *project != "project_lma" || credential == nil || *credential != 1 ||
			endpoint == nil || *endpoint != "https://unit.livekit.cloud" || room == nil || *room != h.plan.RoomName {
			t.Fatalf("wrong frozen target or generation: %s %v %v %v %v %v", disposition, generation, project, credential, endpoint, room)
		}
		return disposition, *generation, egress
	}
	if project != nil || credential != nil || endpoint != nil || room != nil || egress != nil {
		t.Fatalf("nonclaim disclosed target: %s", disposition)
	}
	return disposition, 0, nil
}

func mrrTimeout(t *testing.T, pool *pgxpool.Pool, episode string, elapsed int64) (string, int) {
	t.Helper()
	var disposition string
	var affected int
	if err := pool.QueryRow(context.Background(), `SELECT disposition,affected_count FROM live.timeout_media_recovery_episode($1::uuid,$2::bigint)`,
		episode, elapsed).Scan(&disposition, &affected); err != nil {
		t.Fatal(err)
	}
	return disposition, affected
}

func TestLiveMediaRecoveryMRR02ScopeClockAndWitness(t *testing.T) {
	h := lmeSetup(t, func(w http.ResponseWriter, _ *http.Request) { http.Error(w, "SQL only", 500) })
	recovery, _ := mrrRecoveryPool(t, h)
	var ready bool
	if err := recovery.QueryRow(context.Background(), `SELECT live.media_recovery_ready()`).Scan(&ready); err != nil || !ready {
		t.Fatalf("fixed PG18 observer readiness failed before process gates: %v %v", ready, err)
	}
	knownEmpty := randomUUID()
	empty := mrrBegin(t, recovery, knownEmpty, 0, 1, true)
	if len(empty) != 1 || empty[0].disposition != "empty" || empty[0].candidates != 0 || !empty[0].known {
		t.Fatalf("known empty must be NO_WORK: %+v", empty)
	}
	if disposition, count := mrrTimeout(t, recovery, knownEmpty, 90000); disposition != "already_finished" || count != 0 {
		t.Fatalf("known empty emitted alarm: %s %d", disposition, count)
	}
	unknownEmpty := randomUUID()
	if rows := mrrBegin(t, recovery, unknownEmpty, 91000, 1, false); len(rows) != 1 || rows[0].known {
		t.Fatalf("unknown coverage upgraded: %+v", rows)
	}
	if disposition, count := mrrTimeout(t, recovery, unknownEmpty, 91000); disposition != "already_timed_out" || count != 0 {
		t.Fatalf("unknown empty must retain scope miss: %s %d", disposition, count)
	}
	if got := mrrRead(t, recovery, unknownEmpty); got[0].timeoutAt == nil || got[0].known {
		t.Fatalf("unknown coverage miss lost: %+v", got)
	}

	old := mrrReserveUnansweredStart(t, h)
	episode := randomUUID()
	first := mrrBegin(t, recovery, episode, 1000, 1, true)
	if len(first) != 1 || first[0].disposition != "pending" || first[0].operation == nil || *first[0].operation != h.plan.OperationID ||
		first[0].job == nil || *first[0].job != h.plan.JobID || first[0].baseline == nil {
		t.Fatalf("original unresolved member not captured: %+v", first)
	}
	if replay := mrrBegin(t, recovery, episode, 80000, 1, false); len(replay) != 1 || !replay[0].known || replay[0].candidates != first[0].candidates {
		t.Fatalf("same-episode replay changed immutable coverage: %+v", replay)
	}
	if disposition, _, _ := mrrClaim(t, recovery, episode, h, randomBytes(32)); disposition != "busy" {
		t.Fatalf("active old lease stolen: %s", disposition)
	}
	// Native finalization legally clears the complete lease tuple. This is the
	// NULL-lease branch; the other SQL counterexamples use intact expired leases.
	var finished string
	if err := h.executor.QueryRow(context.Background(), `SELECT live.finish_media_uncertain($1::uuid,$2::bigint,$3::bytea,'remote_unknown')`,
		h.plan.OperationID, old.generation, old.token).Scan(&finished); err != nil || finished != "observe" {
		t.Fatalf("native uncertain finish: %s %v", finished, err)
	}
	var state, mode string
	var until *time.Time
	var tokenHash []byte
	if err := h.lp.f.owner.QueryRow(context.Background(), `SELECT state,lease_mode,lease_until,lease_token_hash FROM integration.operations WHERE id=$1::uuid`,
		h.plan.OperationID).Scan(&state, &mode, &until, &tokenHash); err != nil || state != "UNKNOWN" || mode != "" || until != nil || tokenHash != nil {
		t.Fatalf("native finish did not clear lease tuple: state=%s mode=%s until=%v token=%v err=%v", state, mode, until, tokenHash != nil, err)
	}
	token := randomBytes(32)
	if disposition, generation, egress := mrrClaim(t, recovery, episode, h, token); disposition != "claimed" || generation <= *first[0].baseline || egress != nil {
		t.Fatalf("ROOM recovery claim: %s gen=%d egress=%v", disposition, generation, egress)
	} else {
		var state string
		var obs string
		err := recovery.QueryRow(context.Background(), `SELECT disposition,observation_id::text FROM live.record_recovery_observation(
		 $1::uuid,$2::uuid,$3::bigint,$4::bytea,'ROOM',$5::text,$6::text,'EGRESS_ACTIVE',100,120,0)`,
			episode, h.plan.OperationID, generation, token, "EG_mrr_room", h.plan.RoomName).Scan(&state, &obs)
		if err != nil || state != "checked" || obs == "" {
			t.Fatalf("ROOM record: %s %s %v", state, obs, err)
		}
		read := mrrRead(t, recovery, episode)
		if len(read) != 1 || read[0].obs == nil || *read[0].obs != obs || read[0].source == nil || *read[0].source != "ROOM" ||
			read[0].generation == nil || *read[0].generation != generation {
			t.Fatalf("committed correlation missing: %+v", read)
		}
		var readDBClock time.Time
		if err := h.lp.f.owner.QueryRow(context.Background(), `SELECT clock_timestamp()`).Scan(&readDBClock); err != nil {
			t.Fatal(err)
		}
		time.Sleep(25 * time.Millisecond) // Delayed persistence relative to committed readback, not a virtual 90s clock.
		var witnessed string
		if err := recovery.QueryRow(context.Background(), `SELECT live.witness_media_recovery_episode($1::uuid,$2::uuid,$3::uuid,89999)`,
			episode, h.plan.OperationID, obs).Scan(&witnessed); err != nil || witnessed != "witnessed" {
			t.Fatalf("delayed witness for timely readback: %s %v", witnessed, err)
		}
		var witnessDBClock time.Time
		if err := h.lp.f.owner.QueryRow(context.Background(), `SELECT created_at FROM integration.operation_events
		 WHERE operation_id=$1::uuid AND episode_id=$2::uuid AND episode_event_kind='witnessed'`,
			h.plan.OperationID, episode).Scan(&witnessDBClock); err != nil || !witnessDBClock.After(readDBClock) {
			t.Fatalf("witness was not persisted after committed readback: read=%v witness=%v err=%v", readDBClock, witnessDBClock, err)
		}
		if disposition, count := mrrTimeout(t, recovery, episode, 90000); disposition != "already_finished" || count != 0 {
			t.Fatalf("witnessed member timed out: %s %d", disposition, count)
		}
	}
	if h.starts.Load()+h.lists.Load()+h.queries.Load()+h.stops.Load() != 0 {
		t.Fatal("SQL-only observer made provider call")
	}
}

func TestLiveMediaRecoveryMRR03NegativeAuthorityAndFences(t *testing.T) {
	h := lmeSetup(t, func(w http.ResponseWriter, _ *http.Request) { http.Error(w, "no provider", 500) })
	recovery, login := mrrRecoveryPool(t, h)
	ctx := context.Background()
	signatures := map[string]string{
		"live.begin_media_recovery_episode(uuid,bigint,integer,boolean)":                                    "TABLE(disposition text, episode_id uuid, operation_id uuid, job_id bigint, baseline_generation bigint, deadline_at timestamp with time zone, candidate_count integer, coverage_known boolean, blocked_by_episode_id uuid)",
		"live.claim_recovery_observation(uuid,uuid,bigint,bytea)":                                           "TABLE(disposition text, generation bigint, project_id text, credential_version bigint, endpoint_identity text, room_name text, egress_id text)",
		"live.record_recovery_observation(uuid,uuid,bigint,bytea,text,text,text,text,bigint,bigint,bigint)": "TABLE(disposition text, observation_id uuid)",
		"live.finish_recovery_observation(uuid,uuid,bigint,bytea,text)":                                     "text",
		"live.read_media_recovery_episode(uuid)":                                                            "TABLE(episode_id uuid, scope_status text, coverage_known boolean, candidate_count integer, blocked_by_episode_id uuid, operation_id uuid, disposition text, baseline_generation bigint, observation_id uuid, observation_source text, observation_generation bigint, witness_elapsed_ms bigint, timeout_at timestamp with time zone, cleanup_required boolean)",
		"live.witness_media_recovery_episode(uuid,uuid,uuid,bigint)":                                        "text",
		"live.timeout_media_recovery_episode(uuid,bigint)":                                                  "TABLE(disposition text, affected_count integer)",
	}
	for signature, wantResult := range signatures {
		var owner, result string
		var securityDefiner, fixedPath, allowed bool
		var excessACL int
		if err := h.lp.f.owner.QueryRow(ctx, `SELECT pg_get_userbyid(p.proowner),pg_get_function_result(p.oid),p.prosecdef,
		 'search_path=pg_catalog'=ANY(p.proconfig),
		 (SELECT count(*) FROM aclexplode(coalesce(p.proacl,acldefault('f',p.proowner))) acl
		  WHERE acl.privilege_type='EXECUTE' AND acl.grantee NOT IN (p.proowner,'commerce_media_recovery'::regrole))
		 FROM pg_proc p WHERE p.oid=to_regprocedure($1)`, signature).Scan(&owner, &result, &securityDefiner, &fixedPath, &excessACL); err != nil ||
			owner != "commerce_media_writer" || result != wantResult || !securityDefiner || !fixedPath || excessACL != 0 {
			t.Fatalf("ABI/ACL drift %s: owner=%s result=%s secdef=%v path=%v excess=%d err=%v", signature, owner, result, securityDefiner, fixedPath, excessACL, err)
		}
		if err := recovery.QueryRow(ctx, `SELECT has_function_privilege(current_user,$1,'EXECUTE')`, signature).Scan(&allowed); err != nil || !allowed {
			t.Fatalf("recovery role denied %s: %v %v", signature, allowed, err)
		}
		for label, old := range map[string]*pgxpool.Pool{"worker": h.worker, "executor": h.executor, "runtime": h.lp.f.runtime, "registrar": h.registrar} {
			if err := old.QueryRow(ctx, `SELECT has_function_privilege(current_user,$1,'EXECUTE')`, signature).Scan(&allowed); err != nil || allowed {
				t.Fatalf("%s reaches observer %s: %v %v", label, signature, allowed, err)
			}
		}
	}
	var ready bool
	if err := recovery.QueryRow(ctx, `SELECT live.media_recovery_ready()`).Scan(&ready); err != nil || !ready {
		t.Fatalf("recovery readiness: %v %v", ready, err)
	}
	for _, table := range []string{"live.media_execution_state", "live.media_observations", "integration.operations", "river_media.river_job"} {
		var count int
		if err := recovery.QueryRow(ctx, "SELECT count(*) FROM "+table).Scan(&count); err == nil {
			t.Fatalf("recovery role directly read %s", table)
		}
	}
	var allowed bool
	if err := recovery.QueryRow(ctx, `SELECT has_function_privilege(current_user,'live.claim_media_operation(uuid,bigint,integer,bytea)','EXECUTE')`).Scan(&allowed); err != nil || allowed {
		t.Fatalf("observer reaches native executor: %v %v", allowed, err)
	}
	if err := platform.ValidateMediaRecoveryPool(ctx, recovery); err != nil {
		t.Fatalf("clean observer pool: %v", err)
	}
	name := pgx.Identifier{login}.Sanitize()
	mustExec(t, h.lp.f.owner, "GRANT commerce_media_executor TO "+name+" WITH INHERIT TRUE, SET FALSE")
	if err := platform.ValidateMediaRecoveryPool(ctx, recovery); err == nil {
		t.Fatal("mixed observer/executor role admitted")
	}
	mustExec(t, h.lp.f.owner, "REVOKE commerce_media_executor FROM "+name)
	if err := platform.ValidateMediaRecoveryPool(ctx, recovery); err != nil {
		t.Fatalf("clean observer role not restored: %v", err)
	}
	mrrReserveUnansweredStart(t, h)
	episode := randomUUID()
	if got := mrrBegin(t, recovery, episode, 0, 1, true); len(got) != 1 || got[0].operation == nil {
		t.Fatalf("missing member: %+v", got)
	}
	mustExec(t, h.lp.f.owner, `UPDATE integration.operations SET lease_until=clock_timestamp()-interval '1 second' WHERE id=$1`, h.plan.OperationID)
	token := randomBytes(32)
	disposition, generation, _ := mrrClaim(t, recovery, episode, h, token)
	if disposition != "claimed" {
		t.Fatalf("fresh claim: %s", disposition)
	}
	for _, bad := range []struct {
		name  string
		gen   int64
		token []byte
		room  string
	}{
		{"old generation", generation - 1, token, h.plan.RoomName},
		{"wrong token", generation, randomBytes(32), h.plan.RoomName},
		{"wrong target", generation, token, "wrong_room"},
	} {
		var result, id string
		err := recovery.QueryRow(ctx, `SELECT disposition,observation_id::text FROM live.record_recovery_observation(
		 $1::uuid,$2::uuid,$3::bigint,$4::bytea,'ROOM','EG_bad',$5::text,'EGRESS_ACTIVE',100,120,0)`,
			episode, h.plan.OperationID, bad.gen, bad.token, bad.room).Scan(&result, &id)
		if err == nil {
			t.Fatalf("%s recorded observer result %s/%s", bad.name, result, id)
		}
	}
	if f := h.facts(t); f.observations != 0 || f.cleanup || h.stops.Load() != 0 {
		t.Fatalf("negative fences changed custody: %+v", f)
	}
	if err := migrations.Apply(ctx, h.lp.f.owner); err != nil {
		t.Fatalf("idempotent additive migration: %v", err)
	}
	if err := platform.ValidateMediaRecoveryPool(ctx, recovery); err != nil {
		t.Fatalf("migration drifted readiness: %v", err)
	}
	if h.starts.Load()+h.lists.Load()+h.queries.Load()+h.stops.Load() != 0 {
		t.Fatal("negative SQL gate made provider call")
	}
	if !strings.HasPrefix(h.plan.RoomName, "lc_") {
		t.Fatal("fixture target not frozen room")
	}
}

func TestLiveMediaRecoveryMRR03OldPoolsRejectDirectObserverGrant(t *testing.T) {
	h := lmeSetup(t, func(w http.ResponseWriter, _ *http.Request) { http.Error(w, "no provider", 500) })
	ctx := context.Background()
	const observerFunction = "live.read_media_recovery_episode(uuid)"
	var observerOID uint32
	if err := h.lp.f.owner.QueryRow(ctx, `SELECT pg_catalog.to_regprocedure($1)::oid`, observerFunction).Scan(&observerOID); err != nil || observerOID == 0 {
		t.Fatalf("resolve observer function OID as owner: %d %v", observerOID, err)
	}
	for _, role := range []struct {
		name     string
		member   string
		validate func(*pgxpool.Pool) error
	}{
		{"runtime", "commerce_runtime", func(p *pgxpool.Pool) error {
			admitted, err := platform.OpenPool(ctx, p.Config().ConnString())
			if admitted != nil {
				admitted.Close()
			}
			return err
		}},
		{"buyer", "commerce_buyer_runtime", func(p *pgxpool.Pool) error { return platform.ValidateBuyerPool(ctx, p) }},
		{"meta", "commerce_meta_worker", func(p *pgxpool.Pool) error { return platform.ValidateMetaWorkerPool(ctx, p) }},
	} {
		t.Run(role.name, func(t *testing.T) {
			login, pool := lmaLogin(t, h.lp.f, role.member)
			if role.name == "meta" {
				name := pgx.Identifier{login}.Sanitize()
				mustExec(t, h.lp.f.owner, "REVOKE commerce_meta_worker FROM "+name)
				mustExec(t, h.lp.f.owner, "GRANT commerce_meta_worker TO "+name+" WITH INHERIT TRUE, SET FALSE")
			}
			if err := role.validate(pool); err != nil {
				t.Fatalf("clean old pool rejected: %v", err)
			}
			grant := "GRANT EXECUTE ON FUNCTION " + observerFunction + " TO " + pgx.Identifier{login}.Sanitize()
			revoke := "REVOKE EXECUTE ON FUNCTION " + observerFunction + " FROM " + pgx.Identifier{login}.Sanitize()
			mustExec(t, h.lp.f.owner, grant)
			t.Cleanup(func() { _, _ = h.lp.f.owner.Exec(context.Background(), revoke) })
			var granted bool
			if err := pool.QueryRow(ctx, `SELECT has_function_privilege(current_user,$1::oid,'EXECUTE')`, observerOID).Scan(&granted); err != nil || !granted {
				t.Fatalf("direct observer grant not installed: %v %v", granted, err)
			}
			if err := role.validate(pool); err == nil {
				t.Fatal("old pool admitted direct observer EXECUTE")
			}
			mustExec(t, h.lp.f.owner, revoke)
			if err := role.validate(pool); err != nil {
				t.Fatalf("old pool not restored after revoke: %v", err)
			}
		})
	}
}

func TestLiveMediaRecoveryMRR03WrongPhysicalDatabase(t *testing.T) {
	h := lmeSetup(t, func(w http.ResponseWriter, _ *http.Request) { http.Error(w, "no provider", 500) })
	recovery, _ := mrrRecoveryPool(t, h)
	ctx := context.Background()
	config, err := pgxpool.ParseConfig(recovery.Config().ConnString())
	if err != nil {
		t.Fatal(err)
	}
	config.ConnConfig.Database = "postgres" // The other DB in this task-owned PG18 container.
	wrong, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		t.Fatalf("open wrong physical DB: %v", err)
	}
	defer wrong.Close()
	var name string
	if err := wrong.QueryRow(ctx, `SELECT current_database()`).Scan(&name); err != nil || name != "postgres" {
		t.Fatalf("wrong physical DB fixture not connected: %s %v", name, err)
	}
	if err := platform.ValidateMediaRecoveryPool(ctx, wrong); err == nil {
		t.Fatal("observer admitted wrong physical DB")
	}
}

func TestLiveMediaRecoveryMRR02PoolConfigFailures(t *testing.T) {
	ctx := context.Background()
	if pool, err := platform.OpenMediaRecoveryPool(ctx, "postgres://%zz"); err == nil {
		pool.Close()
		t.Fatal("malformed recovery DSN admitted")
	}
	f := fixture(t)
	_, worker := lmaLogin(t, f, "commerce_media_worker")
	if pool, err := platform.OpenMediaRecoveryPool(ctx, worker.Config().ConnString()); err == nil {
		pool.Close()
		t.Fatal("native worker role admitted as recovery pool")
	}
}

func TestLiveMediaRecoveryMRR02AdmittedEventRLSAndPartialVisibility(t *testing.T) {
	ctx := context.Background()
	h1 := lmeSetup(t, func(w http.ResponseWriter, _ *http.Request) { http.Error(w, "SQL only", 500) })
	h2 := lmeSetup(t, func(w http.ResponseWriter, _ *http.Request) { http.Error(w, "SQL only", 500) })
	recovery, _ := mrrRecoveryPool(t, h1)
	_, writer := lmaLogin(t, h1.lp.f, "commerce_media_writer")
	mrrReserveUnansweredStart(t, h1)
	mrrReserveUnansweredStart(t, h2)
	episode := randomUUID()
	rows := mrrBegin(t, recovery, episode, 0, 2, true)
	if len(rows) != 2 || rows[0].candidates != 2 || rows[1].candidates != 2 {
		t.Fatalf("two original members were not captured: %+v", rows)
	}
	count := func(pool *pgxpool.Pool) int {
		t.Helper()
		var n int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM integration.operation_events WHERE episode_id=$1::uuid AND episode_event_kind='admitted'`, episode).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	if owner, mediaWriter := count(h1.lp.f.owner), count(writer); owner != 2 || mediaWriter != owner {
		t.Fatalf("admitted events hidden from SECURITY DEFINER writer: owner=%d media_writer=%d", owner, mediaWriter)
	}
	var predicate string
	if err := h1.lp.f.owner.QueryRow(ctx, `SELECT pg_catalog.pg_get_expr(p.polqual,p.polrelid)
	 FROM pg_catalog.pg_policy p WHERE p.polrelid='integration.operation_events'::regclass
	 AND p.polname='media_writer_event_read'`).Scan(&predicate); err != nil || predicate == "" {
		t.Fatalf("missing media-writer admitted-event SELECT policy: %v", err)
	}
	const drop = `DROP POLICY IF EXISTS media_writer_event_read ON integration.operation_events`
	create := `CREATE POLICY media_writer_event_read ON integration.operation_events FOR SELECT TO commerce_media_writer USING (` + predicate + `)`
	restore := func() {
		t.Helper()
		mustExec(t, h1.lp.f.owner, drop)
		mustExec(t, h1.lp.f.owner, create)
	}
	t.Cleanup(restore)
	assertHiddenFailsClosed := func(label string) {
		t.Helper()
		for _, query := range []string{
			`SELECT disposition FROM live.begin_media_recovery_episode($1::uuid,0,2,true) LIMIT 1`,
			`SELECT scope_status FROM live.read_media_recovery_episode($1::uuid) LIMIT 1`,
			`SELECT disposition FROM live.timeout_media_recovery_episode($1::uuid,90000)`,
		} {
			var result string
			if err := recovery.QueryRow(ctx, query, episode).Scan(&result); sqlState(err) != "ME409" {
				t.Fatalf("%s: incomplete admitted set was treated as %s, err=%v", label, result, err)
			}
		}
		var timeoutAt *time.Time
		if err := h1.lp.f.owner.QueryRow(ctx, `SELECT timeout_at FROM live.media_recovery_episode_scope WHERE episode_id=$1::uuid`, episode).Scan(&timeoutAt); err != nil || timeoutAt != nil {
			t.Fatalf("%s: false completion/timeout persisted: %v %v", label, timeoutAt, err)
		}
	}
	mustExec(t, h1.lp.f.owner, drop)
	if n := count(writer); n != 0 {
		t.Fatalf("dropped policy still exposed %d admitted events", n)
	}
	assertHiddenFailsClosed("dropped-policy")
	restore()
	if n := count(writer); n != 2 {
		t.Fatalf("restored policy failed readback: %d", n)
	}
	if !regexp.MustCompile(`^[0-9a-f]{8}(-[0-9a-f]{4}){3}-[0-9a-f]{12}$`).MatchString(h1.plan.OperationID) {
		t.Fatalf("invalid fixture UUID: %q", h1.plan.OperationID)
	}
	partial := fmt.Sprintf(`ALTER POLICY media_writer_event_read ON integration.operation_events USING (operation_id <> '%s'::uuid AND (%s))`, h1.plan.OperationID, predicate)
	mustExec(t, h1.lp.f.owner, partial)
	if n := count(writer); n != 1 {
		t.Fatalf("partial policy did not hide exactly one member: %d", n)
	}
	assertHiddenFailsClosed("partial-policy")
	var oldDeadline time.Time
	if err := h1.lp.f.owner.QueryRow(ctx, `SELECT deadline_at FROM live.media_recovery_episode_scope WHERE episode_id=$1::uuid`, episode).Scan(&oldDeadline); err != nil {
		t.Fatal(err)
	}
	blockedEpisode := randomUUID()
	if got := mrrBegin(t, recovery, blockedEpisode, 1000, 2, true); len(got) != 1 || got[0].disposition != "prior_unfinished" || got[0].blocked == nil || *got[0].blocked != episode {
		t.Fatalf("partially hidden previous episode was displaced: %+v", got)
	}
	var afterDeadline time.Time
	if err := h1.lp.f.owner.QueryRow(ctx, `SELECT deadline_at FROM live.media_recovery_episode_scope WHERE episode_id=$1::uuid`, episode).Scan(&afterDeadline); err != nil || !afterDeadline.Equal(oldDeadline) {
		t.Fatalf("prior scope deadline was changed: %v -> %v %v", oldDeadline, afterDeadline, err)
	}
	restore()
	if got := mrrRead(t, recovery, episode); len(got) != 2 {
		t.Fatalf("restored policy lost full committed member set: %+v", got)
	}
	const extra = `DROP POLICY IF EXISTS mrr_test_permissive_event_read ON integration.operation_events`
	t.Cleanup(func() { _, _ = h1.lp.f.owner.Exec(context.Background(), extra) })
	mustExec(t, h1.lp.f.owner, `CREATE POLICY mrr_test_permissive_event_read ON integration.operation_events FOR SELECT TO PUBLIC USING (true)`)
	var ready bool
	if err := recovery.QueryRow(ctx, `SELECT live.media_recovery_ready()`).Scan(&ready); err != nil || ready {
		t.Fatalf("extra PUBLIC SELECT policy was admitted: ready=%v err=%v", ready, err)
	}
	mustExec(t, h1.lp.f.owner, extra)
	if err := recovery.QueryRow(ctx, `SELECT live.media_recovery_ready()`).Scan(&ready); err != nil || !ready {
		t.Fatalf("exact policy readiness not restored: ready=%v err=%v", ready, err)
	}
	mustExec(t, h1.lp.f.owner, `CREATE POLICY mrr_test_permissive_event_read ON integration.operation_events FOR SELECT TO commerce_media_writer USING (true)`)
	if err := recovery.QueryRow(ctx, `SELECT live.media_recovery_ready()`).Scan(&ready); err != nil || ready {
		t.Fatalf("extra writer SELECT policy was admitted: ready=%v err=%v", ready, err)
	}
	mustExec(t, h1.lp.f.owner, extra)
	if err := recovery.QueryRow(ctx, `SELECT live.media_recovery_ready()`).Scan(&ready); err != nil || !ready {
		t.Fatalf("readiness not restored after writer policy: ready=%v err=%v", ready, err)
	}
	t.Cleanup(func() {
		_, _ = h1.lp.f.owner.Exec(context.Background(), `ALTER ROLE commerce_media_writer NOBYPASSRLS`)
	})
	mustExec(t, h1.lp.f.owner, `ALTER ROLE commerce_media_writer BYPASSRLS`)
	if err := recovery.QueryRow(ctx, `SELECT live.media_recovery_ready()`).Scan(&ready); err != nil || ready {
		t.Fatalf("BYPASSRLS definer writer was admitted: ready=%v err=%v", ready, err)
	}
	mustExec(t, h1.lp.f.owner, `ALTER ROLE commerce_media_writer NOBYPASSRLS`)
	if err := recovery.QueryRow(ctx, `SELECT live.media_recovery_ready()`).Scan(&ready); err != nil || !ready {
		t.Fatalf("readiness not restored after writer role reset: ready=%v err=%v", ready, err)
	}
	if h1.starts.Load()+h1.lists.Load()+h1.queries.Load()+h1.stops.Load()+
		h2.starts.Load()+h2.lists.Load()+h2.queries.Load()+h2.stops.Load() != 0 {
		t.Fatal("SQL-only RLS counterexample made provider call")
	}
}

func TestLiveMediaRecoveryMRR02TimeoutWinsAndCoverage(t *testing.T) {
	h := lmeSetup(t, func(w http.ResponseWriter, _ *http.Request) { http.Error(w, "SQL only", 500) })
	recovery, _ := mrrRecoveryPool(t, h)
	mrrReserveUnansweredStart(t, h)
	episode := randomUUID()
	if rows := mrrBegin(t, recovery, episode, 0, 1, false); len(rows) != 1 || rows[0].operation == nil || rows[0].known {
		t.Fatalf("unknown coverage member: %+v", rows)
	}
	blocked := randomUUID()
	if rows := mrrBegin(t, recovery, blocked, 1000, 1, true); len(rows) != 1 || rows[0].disposition != "prior_unfinished" || rows[0].blocked == nil || *rows[0].blocked != episode {
		t.Fatalf("unfinished original episode was displaced: %+v", rows)
	}
	if disposition, count := mrrTimeout(t, recovery, blocked, 90000); disposition != "timed_out" || count != 0 {
		t.Fatalf("prior-unfinished scope miss: %s %d", disposition, count)
	}
	mustExec(t, h.lp.f.owner, `UPDATE integration.operations SET lease_until=clock_timestamp()-interval '1 second' WHERE id=$1`, h.plan.OperationID)
	token := randomBytes(32)
	if disposition, generation, _ := mrrClaim(t, recovery, episode, h, token); disposition != "claimed" {
		t.Fatalf("unknown-coverage claim: %s", disposition)
	} else {
		var state, obs string
		err := recovery.QueryRow(context.Background(), `SELECT disposition,observation_id::text FROM live.record_recovery_observation(
		 $1::uuid,$2::uuid,$3::bigint,$4::bytea,'ROOM','EG_mrr_unknown',$5::text,'EGRESS_ACTIVE',100,120,0)`,
			episode, h.plan.OperationID, generation, token, h.plan.RoomName).Scan(&state, &obs)
		if err != nil || state != "checked" {
			t.Fatalf("unknown-coverage observation: %s %v", state, err)
		}
		if read := mrrRead(t, recovery, episode); read[0].obs == nil || *read[0].obs != obs {
			t.Fatalf("committed observation absent: %+v", read)
		}
		var witnessed string
		if err := recovery.QueryRow(context.Background(), `SELECT live.witness_media_recovery_episode($1::uuid,$2::uuid,$3::uuid,1000)`,
			episode, h.plan.OperationID, obs).Scan(&witnessed); err != nil || witnessed != "witnessed" {
			t.Fatalf("member witness: %s %v", witnessed, err)
		}
		if disposition, count := mrrTimeout(t, recovery, episode, 90000); disposition != "timed_out" || count != 0 {
			t.Fatalf("unknown coverage all-witnessed must still scope-miss: %s %d", disposition, count)
		}
		if read := mrrRead(t, recovery, episode); read[0].scope != "overdue" || read[0].disposition != "witnessed" || read[0].known {
			t.Fatalf("scope timeout rewrote member witness or coverage: %+v", read)
		}
	}
	if h.starts.Load()+h.lists.Load()+h.queries.Load()+h.stops.Load() != 0 {
		t.Fatal("SQL gate made provider call")
	}
}

func TestLiveMediaRecoveryMRR02TimeoutFirstAndCapacity(t *testing.T) {
	h := lmeSetup(t, func(w http.ResponseWriter, _ *http.Request) { http.Error(w, "SQL only", 500) })
	recovery, _ := mrrRecoveryPool(t, h)
	mrrReserveUnansweredStart(t, h)
	episode := randomUUID()
	if rows := mrrBegin(t, recovery, episode, 0, 1, true); len(rows) != 1 || rows[0].operation == nil {
		t.Fatalf("member absent: %+v", rows)
	}
	mustExec(t, h.lp.f.owner, `UPDATE integration.operations SET lease_until=clock_timestamp()-interval '1 second' WHERE id=$1`, h.plan.OperationID)
	token := randomBytes(32)
	disposition, generation, _ := mrrClaim(t, recovery, episode, h, token)
	if disposition != "claimed" {
		t.Fatalf("claim: %s", disposition)
	}
	var state, obs string
	if err := recovery.QueryRow(context.Background(), `SELECT disposition,observation_id::text FROM live.record_recovery_observation(
	 $1::uuid,$2::uuid,$3::bigint,$4::bytea,'ROOM','EG_mrr_late',$5::text,'EGRESS_ACTIVE',100,120,0)`,
		episode, h.plan.OperationID, generation, token, h.plan.RoomName).Scan(&state, &obs); err != nil || state != "checked" {
		t.Fatalf("qualified before timeout: %s %v", state, err)
	}
	if disposition, count := mrrTimeout(t, recovery, episode, 90000); disposition != "timed_out" || count != 1 {
		t.Fatalf("timeout-first: %s %d", disposition, count)
	}
	var lateState, lateObs string
	if err := recovery.QueryRow(context.Background(), `SELECT disposition,observation_id::text FROM live.record_recovery_observation(
	 $1::uuid,$2::uuid,$3::bigint,$4::bytea,'ROOM','EG_mrr_late',$5::text,'EGRESS_COMPLETE',100,140,130)`,
		episode, h.plan.OperationID, generation, token, h.plan.RoomName).Scan(&lateState, &lateObs); sqlState(err) != "ME409" {
		t.Fatalf("late observation reversed deadline: %s %s %v", lateState, lateObs, err)
	}
	var witness string
	if err := recovery.QueryRow(context.Background(), `SELECT live.witness_media_recovery_episode($1::uuid,$2::uuid,$3::uuid,1000)`,
		episode, h.plan.OperationID, obs).Scan(&witness); err != nil || witness != "timeout_wins" {
		t.Fatalf("late witness reversed sticky timeout: %s %v", witness, err)
	}
	if disposition, count := mrrTimeout(t, recovery, episode, 91000); disposition != "already_timed_out" || count != 0 {
		t.Fatalf("timeout replay: %s %d", disposition, count)
	}
	if read := mrrRead(t, recovery, episode); read[0].disposition != "timeout" || read[0].timeoutAt == nil {
		t.Fatalf("sticky timeout lost: %+v", read)
	}

	// Separate fixture within the same isolated database gives the capacity+1
	// counterexample without manufacturing 33 rows or changing queue clocks.
	h2 := lmeSetup(t, func(w http.ResponseWriter, _ *http.Request) { http.Error(w, "SQL only", 500) })
	mrrReserveUnansweredStart(t, h2)
	overflow := randomUUID()
	rows := mrrBegin(t, recovery, overflow, 0, 1, true)
	if len(rows) != 1 || rows[0].disposition != "capacity_exceeded" || rows[0].operation != nil || rows[0].candidates < 2 {
		t.Fatalf("overflow admitted partial set: %+v", rows)
	}
	if disposition, count := mrrTimeout(t, recovery, overflow, 90000); disposition != "timed_out" || count != 0 {
		t.Fatalf("capacity scope alarm: %s %d", disposition, count)
	}
	if h2.starts.Load()+h2.lists.Load()+h2.queries.Load()+h2.stops.Load() != 0 {
		t.Fatal("capacity gate made provider call")
	}
}

func TestLiveMediaRecoveryMRR02WitnessCommitBeatsWaitingTimeout(t *testing.T) {
	h := lmeSetup(t, func(w http.ResponseWriter, _ *http.Request) { http.Error(w, "SQL only", 500) })
	recovery, _ := mrrRecoveryPool(t, h)
	mrrReserveUnansweredStart(t, h)
	episode := randomUUID()
	mrrBegin(t, recovery, episode, 0, 1, true)
	mustExec(t, h.lp.f.owner, `UPDATE integration.operations SET lease_until=clock_timestamp()-interval '1 second' WHERE id=$1`, h.plan.OperationID)
	token := randomBytes(32)
	disposition, generation, _ := mrrClaim(t, recovery, episode, h, token)
	if disposition != "claimed" {
		t.Fatalf("claim: %s", disposition)
	}
	var state, obs string
	if err := recovery.QueryRow(context.Background(), `SELECT disposition,observation_id::text FROM live.record_recovery_observation(
	 $1::uuid,$2::uuid,$3::bigint,$4::bytea,'ROOM','EG_mrr_race',$5::text,'EGRESS_ACTIVE',100,120,0)`,
		episode, h.plan.OperationID, generation, token, h.plan.RoomName).Scan(&state, &obs); err != nil || state != "checked" {
		t.Fatalf("record: %s %v", state, err)
	}
	if read := mrrRead(t, recovery, episode); read[0].obs == nil || *read[0].obs != obs {
		t.Fatalf("read before witness: %+v", read)
	}
	ctx := context.Background()
	witnessTx, err := recovery.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer witnessTx.Rollback(ctx)
	var holderPID int
	if err := witnessTx.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&holderPID); err != nil {
		t.Fatal(err)
	}
	var witness string
	if err := witnessTx.QueryRow(ctx, `SELECT live.witness_media_recovery_episode($1::uuid,$2::uuid,$3::uuid,1000)`,
		episode, h.plan.OperationID, obs).Scan(&witness); err != nil || witness != "witnessed" {
		t.Fatalf("witness before commit: %s %v", witness, err)
	}
	conn, err := recovery.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Release()
	var waiterPID int
	if err := conn.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&waiterPID); err != nil {
		t.Fatal(err)
	}
	type timeoutResult struct {
		disposition string
		affected    int
		err         error
	}
	done := make(chan timeoutResult, 1)
	go func() {
		var result timeoutResult
		result.err = conn.QueryRow(ctx, `SELECT disposition,affected_count FROM live.timeout_media_recovery_episode($1::uuid,90000)`, episode).
			Scan(&result.disposition, &result.affected)
		done <- result
	}()
	lmaObserveBlock(t, h.lp.f.owner, waiterPID, holderPID, false)
	if err := witnessTx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case result := <-done:
		if result.err != nil || result.disposition != "already_finished" || result.affected != 0 {
			t.Fatalf("waiting timeout ignored committed final witness: %+v", result)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timeout waiter did not finish")
	}
	var timeoutAt *time.Time
	if err := h.lp.f.owner.QueryRow(ctx, `SELECT timeout_at FROM live.media_recovery_episode_scope WHERE episode_id=$1::uuid`, episode).Scan(&timeoutAt); err != nil || timeoutAt != nil {
		t.Fatalf("known complete episode left false scope timeout: %v %v", timeoutAt, err)
	}
}

func TestLiveMediaRecoveryMRR02CapacityOverflowReleasesNativeCleanup(t *testing.T) {
	h := lmeSetup(t, func(w http.ResponseWriter, _ *http.Request) { http.Error(w, "unused", 500) })
	h2 := lmeSetup(t, func(w http.ResponseWriter, _ *http.Request) { http.Error(w, "unused", 500) })
	recovery, _ := mrrRecoveryPool(t, h)
	for _, item := range []*lmeHarness{h, h2} {
		lease := mrrReserveUnansweredStart(t, item)
		var finished string
		if err := item.executor.QueryRow(context.Background(), `SELECT live.finish_media_uncertain($1::uuid,$2::bigint,$3::bytea,'remote_unknown')`,
			item.plan.OperationID, lease.generation, lease.token).Scan(&finished); err != nil || finished != "observe" {
			t.Fatalf("original native lease did not release: %s %v", finished, err)
		}
	}
	var nativeRequests atomic.Int32
	tlsServer, ca := lmwTLS(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/twirp/livekit.Egress/ListEgress" {
			t.Errorf("capacity path issued unexpected method %s", r.URL.Path)
			http.Error(w, "unexpected method", 500)
			return
		}
		var target struct {
			RoomName string `json:"room_name"`
			EgressID string `json:"egress_id"`
		}
		if err := json.NewDecoder(r.Body).Decode(&target); err != nil || target.EgressID != "" {
			t.Errorf("capacity path changed native ROOM target: %+v %v", target, err)
			http.Error(w, "wrong target", 400)
			return
		}
		var original *lmeHarness
		for _, item := range []*lmeHarness{h, h2} {
			if target.RoomName == item.plan.RoomName {
				original = item
			}
		}
		if original == nil {
			t.Errorf("capacity path queried foreign room %q", target.RoomName)
			http.Error(w, "foreign room", 400)
			return
		}
		nativeRequests.Add(1)
		lmeReply(w, `{"items":[`+original.observation("EG_mrr_native_overflow", "EGRESS_COMPLETE", 100, 150, 140)+`]}`)
	}))
	binary := mrBuild(t, "../../cmd/media-worker", "mrr-capacity-native")
	env := append(lmwEnvironment(h, tlsServer.Listener.Addr().String(), ca),
		"COMMERCE_MEDIA_RECOVERY_SUPERVISED=1", "COMMERCE_MEDIA_RECOVERY_DATABASE_URL="+recovery.Config().ConnString())
	launched := time.Now()
	parent := lmwProcess(t, binary, "mrr-capacity-native", env)
	episode := mrrWaitEpisode(t, h.lp.f.owner, launched)
	rows := mrrRead(t, recovery, episode)
	if len(rows) != 1 || rows[0].scope != "capacity_exceeded" || rows[0].known != true || rows[0].candidates < 2 || rows[0].operation != nil {
		t.Fatalf("capacity admission was not fail-whole: %+v", rows)
	}
	mrReadyLog(t, parent, "media_native_child_released")
	mrReadyLog(t, parent, "media_worker_ready")
	deadline := time.Now().Add(40 * time.Second)
	progress := false
	for time.Now().Before(deadline) {
		for _, item := range []*lmeHarness{h, h2} {
			facts := item.facts(t)
			if facts.observations > 0 && facts.resource == "TERMINAL" {
				progress = true
			}
		}
		if progress {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !progress || nativeRequests.Load() == 0 {
		t.Fatalf("overflow blocked original native cleanup: progress=%v requests=%d", progress, nativeRequests.Load())
	}
	var claims int
	if err := h.lp.f.owner.QueryRow(context.Background(), `SELECT count(*) FROM integration.operation_events WHERE episode_id=$1::uuid AND episode_event_kind IN ('admitted','qualified','witnessed')`, episode).
		Scan(&claims); err != nil || claims != 0 {
		t.Fatalf("overflow admitted observer member: claims=%d err=%v", claims, err)
	}
	mrStop(t, parent, syscall.SIGTERM, true)
}

func TestLiveMediaRecoveryMRR03CleanupGuardAndStopBudget(t *testing.T) {
	h := lmrSetup(t, func(w http.ResponseWriter, _ *http.Request) { http.Error(w, "SQL only", 500) })
	id := "EG_mrr_cleanup"
	lmrStarted(t, h, id)
	lmrStop(t, h, t04Key("mrr-cleanup"))
	old := h.claim(t, 30)
	before := h.facts(t)
	if _, err := h.record(t, old, "QUERY", id, "EGRESS_ACTIVE", 100, 130, 0); sqlState(err) != "ME409" {
		t.Fatalf("original public QUERY cleanup guard changed: %v", err)
	}
	if after := h.facts(t); after != before {
		t.Fatalf("old QUERY wrote despite rejection: %+v -> %+v", before, after)
	}
	// Isolated SQL counterexample: expire the intact old lease only. The
	// real-clock process gate never rewrites a lease or River timestamp.
	mustExec(t, h.lp.f.owner, `UPDATE integration.operations SET lease_until=clock_timestamp()-interval '1 second' WHERE id=$1`, h.plan.OperationID)
	recovery, _ := mrrRecoveryPool(t, h)
	episode := randomUUID()
	if rows := mrrBegin(t, recovery, episode, 0, 1, true); len(rows) != 1 || rows[0].operation == nil {
		t.Fatalf("cleanup member absent: %+v", rows)
	}
	token := randomBytes(32)
	disposition, generation, egress := mrrClaim(t, recovery, episode, h, token)
	if disposition != "claimed" || egress == nil || *egress != id {
		t.Fatalf("cleanup Query target: %s %v", disposition, egress)
	}
	var state, obs string
	if err := recovery.QueryRow(context.Background(), `SELECT disposition,observation_id::text FROM live.record_recovery_observation(
	 $1::uuid,$2::uuid,$3::bigint,$4::bytea,'QUERY',$5::text,$6::text,'EGRESS_ACTIVE',100,130,0)`,
		episode, h.plan.OperationID, generation, token, id, h.plan.RoomName).Scan(&state, &obs); err != nil || state != "checked" || obs == "" {
		t.Fatalf("observer-only Query: %s %s %v", state, obs, err)
	}
	if f := lmrRead(t, h); f.count != 0 || !f.cleanup || f.operation != "UNKNOWN" || f.resource == "TERMINAL" {
		t.Fatalf("observer spent Stop or closed liability: %+v", f)
	}
	if read := mrrRead(t, recovery, episode); len(read) != 1 || read[0].obs == nil || *read[0].obs != obs || read[0].cleanup == nil || !*read[0].cleanup {
		t.Fatalf("cleanup-required readback lost: %+v", read)
	}
	if h.starts.Load()+h.lists.Load()+h.queries.Load()+h.stops.Load() != 0 {
		t.Fatal("SQL-only guard test made provider call")
	}
}

func TestLiveMediaRecoveryMRR03StaleTerminalObservationDoesNotClose(t *testing.T) {
	h := lmeSetup(t, func(w http.ResponseWriter, _ *http.Request) { http.Error(w, "SQL only", 500) })
	id := "EG_mrr_stale"
	old := h.claim(t, 30)
	if old.disposition != "claimed" || old.mode != "dispatch" {
		t.Fatalf("original claim: %+v", old)
	}
	if err := h.reserve(t, old); err != nil {
		t.Fatal(err)
	}
	if state, err := h.record(t, old, "START", id, "EGRESS_ACTIVE", 200, 220, 0); err != nil || state != "observe" {
		t.Fatalf("newer baseline: %s %v", state, err)
	}
	recovery, _ := mrrRecoveryPool(t, h)
	episode := randomUUID()
	if rows := mrrBegin(t, recovery, episode, 0, 1, true); len(rows) != 1 || rows[0].operation == nil {
		t.Fatalf("original member: %+v", rows)
	}
	token := randomBytes(32)
	disposition, generation, egress := mrrClaim(t, recovery, episode, h, token)
	if disposition != "claimed" || egress == nil || *egress != id {
		t.Fatalf("Query claim: %s %v", disposition, egress)
	}
	var state, obs string
	if err := recovery.QueryRow(context.Background(), `SELECT disposition,observation_id::text FROM live.record_recovery_observation(
	 $1::uuid,$2::uuid,$3::bigint,$4::bytea,'QUERY',$5::text,$6::text,'EGRESS_COMPLETE',0,150,150)`,
		episode, h.plan.OperationID, generation, token, id, h.plan.RoomName).Scan(&state, &obs); err != nil || state != "checked" || obs == "" {
		t.Fatalf("stale terminal overrode newer active fact: %s %s %v", state, obs, err)
	}
	if f := h.facts(t); f.resource != "OBSERVED" || f.operation != "UNKNOWN" || f.startedNS != 200 || f.updatedNS != 220 || f.endedNS != 0 {
		t.Fatalf("stale QUERY closed original resource: %+v", f)
	}
	if h.starts.Load()+h.lists.Load()+h.queries.Load()+h.stops.Load() != 0 {
		t.Fatal("SQL stale fact test made provider call")
	}
}

func TestLiveMediaRecoveryMRR03PrewireAndNativeIneligible(t *testing.T) {
	t.Run("prewire", func(t *testing.T) {
		h := lmeSetup(t, func(w http.ResponseWriter, _ *http.Request) { http.Error(w, "no provider", 500) })
		recovery, _ := mrrRecoveryPool(t, h)
		episode := randomUUID()
		if rows := mrrBegin(t, recovery, episode, 0, 1, true); len(rows) != 1 || rows[0].disposition != "empty" || rows[0].operation != nil {
			t.Fatalf("prewire admitted: %+v", rows)
		}
		var disposition string
		if err := recovery.QueryRow(context.Background(), `SELECT disposition FROM live.claim_recovery_observation($1::uuid,$2::uuid,$3::bigint,$4::bytea)`,
			episode, h.plan.OperationID, h.plan.JobID, randomBytes(32)).Scan(&disposition); sqlState(err) != "ME409" {
			t.Fatalf("prewire claim admitted: %s %v", disposition, err)
		}
		if h.starts.Load()+h.lists.Load()+h.queries.Load()+h.stops.Load() != 0 {
			t.Fatal("prewire observer called provider")
		}
	})
	t.Run("exhausted-original-job", func(t *testing.T) {
		h := lmeSetup(t, func(w http.ResponseWriter, _ *http.Request) { http.Error(w, "no provider", 500) })
		recovery, _ := mrrRecoveryPool(t, h)
		mrrReserveUnansweredStart(t, h)
		mustExec(t, h.lp.f.owner, `UPDATE river_media.river_job SET attempt=max_attempts WHERE id=$1`, h.plan.JobID)
		episode := randomUUID()
		if rows := mrrBegin(t, recovery, episode, 0, 1, true); len(rows) != 1 || rows[0].disposition != "native_ineligible" || rows[0].job == nil || *rows[0].job != h.plan.JobID {
			t.Fatalf("exhausted original job classified as recovery: %+v", rows)
		}
		if disposition, _, _ := mrrClaim(t, recovery, episode, h, randomBytes(32)); disposition != "native_ineligible" {
			t.Fatalf("exhausted original obtained observer lease: %s", disposition)
		}
		if h.starts.Load()+h.lists.Load()+h.queries.Load()+h.stops.Load() != 0 {
			t.Fatal("ineligible observer called provider")
		}
	})
	t.Run("escalated-still-in-scope", func(t *testing.T) {
		h := lmeSetup(t, func(w http.ResponseWriter, _ *http.Request) { http.Error(w, "no provider", 500) })
		recovery, _ := mrrRecoveryPool(t, h)
		mrrReserveUnansweredStart(t, h)
		mustExec(t, h.lp.f.owner, `UPDATE live.media_execution_state
		 SET escalated_at=clock_timestamp(),escalation_code='reconcile_exhausted'
		 WHERE attempt_id=$1::uuid`, h.plan.AttemptID)
		var attempt, maxAttempts int
		if err := h.lp.f.owner.QueryRow(context.Background(), `SELECT attempt,max_attempts FROM river_media.river_job WHERE id=$1`, h.plan.JobID).
			Scan(&attempt, &maxAttempts); err != nil || attempt >= maxAttempts {
			t.Fatalf("ceiling counterexample lost eligible original job: %d/%d %v", attempt, maxAttempts, err)
		}
		episode := randomUUID()
		if rows := mrrBegin(t, recovery, episode, 0, 1, true); len(rows) != 1 || rows[0].disposition != "ceiling" ||
			rows[0].operation == nil || *rows[0].operation != h.plan.OperationID || rows[0].job == nil || *rows[0].job != h.plan.JobID {
			t.Fatalf("escalated original silently vanished from scope: %+v", rows)
		}
		if disposition, _, _ := mrrClaim(t, recovery, episode, h, randomBytes(32)); disposition != "ceiling" {
			t.Fatalf("ceiling obtained observer target: %s", disposition)
		}
		if disposition, _ := mrrTimeout(t, recovery, episode, 90000); disposition != "timed_out" {
			t.Fatalf("ceiling liability did not scope-miss: %s", disposition)
		}
		if rows := mrrRead(t, recovery, episode); len(rows) != 1 || rows[0].timeoutAt == nil || rows[0].operation == nil || *rows[0].operation != h.plan.OperationID {
			t.Fatalf("ceiling liability lost from durable readback: %+v", rows)
		}
		if h.starts.Load()+h.lists.Load()+h.queries.Load()+h.stops.Load() != 0 {
			t.Fatal("ceiling gate made provider call")
		}
	})
}

func TestLiveMediaRecoveryMRR03InputProfileAndWrongOriginalJob(t *testing.T) {
	ctx := context.Background()
	t.Run("input-profile-even-with-synthetic-wire-flag", func(t *testing.T) {
		h, plan, _ := bicStarted(t)
		login, recovery := lmaLogin(t, h.lp.f, "commerce_media_recovery")
		name := pgx.Identifier{login}.Sanitize()
		mustExec(t, h.lp.f.owner, "REVOKE commerce_media_recovery FROM "+name)
		mustExec(t, h.lp.f.owner, "GRANT commerce_media_recovery TO "+name+" WITH INHERIT TRUE, SET FALSE")
		var profile, queue string
		if err := h.lp.f.owner.QueryRow(ctx, `SELECT a.execution_profile,j.queue FROM live.media_attempts a
		 JOIN integration.operations o ON o.media_attempt_id=a.id JOIN river_media.river_job j ON j.id=o.job_id
		 WHERE a.id=$1::uuid`, plan.AttemptID).Scan(&profile, &queue); err != nil || profile != "LOCAL_SFU_MOCK_EGRESS" || queue != "media_input_mock_v1" {
			t.Fatalf("not an original INPUT job: %s %s %v", profile, queue, err)
		}
		if disposition, _, _, err := bicClaim(ctx, h, plan, randomBytes(32)); err != nil || disposition != "await_admission" {
			t.Fatalf("INPUT projection not initialized: %s %v", disposition, err)
		}
		// Owner-only SQL counterexample: even misleading nonterminal/wire flags
		// cannot turn immutable INPUT work into observer/provider authority.
		mustExec(t, h.lp.f.owner, `UPDATE live.media_execution_state SET wire_reserved_at=clock_timestamp(),wire_generation=1 WHERE attempt_id=$1::uuid`, plan.AttemptID)
		mustExec(t, h.lp.f.owner, `UPDATE integration.operations SET state='UNKNOWN',generation=greatest(generation,1),result_code='remote_unknown' WHERE id=$1::uuid`, plan.OperationID)
		episode := randomUUID()
		if rows := mrrBegin(t, recovery, episode, 0, 1, true); len(rows) != 1 || rows[0].disposition != "empty" || rows[0].operation != nil {
			t.Fatalf("INPUT job entered provider observer set: %+v", rows)
		}
		var disposition string
		if err := recovery.QueryRow(ctx, `SELECT disposition FROM live.claim_recovery_observation($1::uuid,$2::uuid,$3::bigint,$4::bytea)`,
			episode, plan.OperationID, plan.JobID, randomBytes(32)).Scan(&disposition); sqlState(err) != "ME409" {
			t.Fatalf("INPUT obtained observer claim: %s %v", disposition, err)
		}
	})
	t.Run("wrong-original-job", func(t *testing.T) {
		h := lmeSetup(t, func(w http.ResponseWriter, _ *http.Request) { http.Error(w, "SQL only", 500) })
		recovery, _ := mrrRecoveryPool(t, h)
		mrrReserveUnansweredStart(t, h)
		episode := randomUUID()
		if rows := mrrBegin(t, recovery, episode, 0, 1, true); len(rows) != 1 || rows[0].job == nil || *rows[0].job != h.plan.JobID {
			t.Fatalf("original job not captured: %+v", rows)
		}
		var disposition string
		if err := recovery.QueryRow(ctx, `SELECT disposition FROM live.claim_recovery_observation($1::uuid,$2::uuid,$3::bigint,$4::bytea)`,
			episode, h.plan.OperationID, h.plan.JobID+1, randomBytes(32)).Scan(&disposition); sqlState(err) != "ME409" {
			t.Fatalf("substituted job obtained observer claim: %s %v", disposition, err)
		}
	})
	t.Run("missing-original-job", func(t *testing.T) {
		h := lmeSetup(t, func(w http.ResponseWriter, _ *http.Request) { http.Error(w, "SQL only", 500) })
		recovery, _ := mrrRecoveryPool(t, h)
		mrrReserveUnansweredStart(t, h)
		deleted, err := h.lp.f.owner.Exec(ctx, `DELETE FROM river_media.river_job WHERE id=$1`, h.plan.JobID)
		if err != nil || deleted.RowsAffected() != 1 {
			t.Fatalf("task-owned original River job was not deleted: rows=%d err=%v", deleted.RowsAffected(), err)
		}
		var operationJob int64
		if err := h.lp.f.owner.QueryRow(ctx, `SELECT job_id FROM integration.operations WHERE id=$1::uuid`, h.plan.OperationID).Scan(&operationJob); err != nil || operationJob != h.plan.JobID {
			t.Fatalf("missing job erased original operation responsibility: job=%d err=%v", operationJob, err)
		}
		episode := randomUUID()
		rows := mrrBegin(t, recovery, episode, 0, 1, true)
		if len(rows) != 1 || rows[0].disposition != "native_ineligible" || rows[0].operation == nil ||
			*rows[0].operation != h.plan.OperationID || rows[0].job == nil || *rows[0].job != h.plan.JobID {
			t.Fatalf("missing original job escaped native-ineligible liability: %+v", rows)
		}
		if disposition, _, _ := mrrClaim(t, recovery, episode, h, randomBytes(32)); disposition != "native_ineligible" {
			t.Fatalf("missing original job disclosed observer target: %s", disposition)
		}
		if h.starts.Load()+h.lists.Load()+h.queries.Load()+h.stops.Load() != 0 {
			t.Fatal("missing original job made provider call")
		}
	})
}

func TestLiveMediaRecoveryMRR04ModeAndConfigurationMatrix(t *testing.T) {
	h := lmeSetup(t, func(w http.ResponseWriter, _ *http.Request) { http.Error(w, "unused", 500) })
	recovery, _ := mrrRecoveryPool(t, h)
	var requests atomic.Int32
	tlsServer, ca := lmwTLS(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		http.Error(w, "unexpected provider request", 500)
	}))
	binary := mrBuild(t, "../../cmd/media-worker", "mrr-mode-matrix")
	base := lmwEnvironment(h, tlsServer.Listener.Addr().String(), ca)
	disabled := lmwReplace(base, "COMMERCE_MEDIA_WORKER_ENABLED", "0")
	for _, mode := range []struct{ name, flag string }{{"unset", ""}, {"zero", "0"}} {
		t.Run("legacy-"+mode.name, func(t *testing.T) {
			env := append([]string(nil), disabled...)
			if mode.flag != "" {
				env = append(env, "COMMERCE_MEDIA_RECOVERY_SUPERVISED="+mode.flag)
			}
			p := lmwProcess(t, binary, "mrr-legacy-"+mode.name, env)
			select {
			case err := <-p.done:
				p.exited = true
				if err != nil {
					t.Fatalf("legacy disabled worker changed behavior: %v", err)
				}
			case <-time.After(8 * time.Second):
				t.Fatal("legacy disabled worker did not exit")
			}
		})
	}
	for _, item := range []struct {
		name   string
		env    []string
		marker string
	}{
		{"invalid-mode", append(append([]string(nil), disabled...), "COMMERCE_MEDIA_RECOVERY_SUPERVISED=bogus"), "media_worker_invalid_config"},
		{"missing-recovery-dsn", append(append([]string(nil), disabled...), "COMMERCE_MEDIA_RECOVERY_SUPERVISED=1"), "media_recovery_dsn_invalid"},
		{"malformed-recovery-dsn", append(append([]string(nil), disabled...), "COMMERCE_MEDIA_RECOVERY_SUPERVISED=1", "COMMERCE_MEDIA_RECOVERY_DATABASE_URL=postgres://%"), "media_recovery_dsn_invalid"},
	} {
		t.Run(item.name, func(t *testing.T) {
			lmwFail(t, lmwProcess(t, binary, "mrr-"+item.name, item.env), item.marker)
		})
	}
	for _, item := range []struct {
		name   string
		env    []string
		marker string
	}{
		{"worker-disabled", append(append([]string(nil), disabled...), "COMMERCE_MEDIA_RECOVERY_SUPERVISED=1", "COMMERCE_MEDIA_RECOVERY_DATABASE_URL="+recovery.Config().ConnString()), "media_recovery_native_worker_disabled"},
		{"native-config-invalid", append(lmwReplace(base, "COMMERCE_MEDIA_MATERIAL_KEYS_JSON", "not-json"), "COMMERCE_MEDIA_RECOVERY_SUPERVISED=1", "COMMERCE_MEDIA_RECOVERY_DATABASE_URL="+recovery.Config().ConnString()), "media_recovery_native_config_invalid"},
	} {
		t.Run(item.name, func(t *testing.T) {
			p := lmwProcess(t, binary, "mrr-"+item.name, item.env)
			mrReadyLog(t, p, item.marker)
			if err := syscall.Kill(p.cmd.Process.Pid, 0); err != nil {
				t.Fatalf("degraded supervisor exited: %v", err)
			}
			mrStop(t, p, syscall.SIGTERM, true)
		})
	}
	if requests.Load() != 0 {
		t.Fatalf("mode/config failure reached provider: requests=%d", requests.Load())
	}
}

func TestLiveMediaRecoveryMRR04InternalChildEOF(t *testing.T) {
	h := lmeSetup(t, func(w http.ResponseWriter, _ *http.Request) { http.Error(w, "no provider", 500) })
	var configuredRequests atomic.Int32
	tlsServer, ca := lmwTLS(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		configuredRequests.Add(1)
		http.Error(w, "no provider", 500)
	}))
	binary := mrBuild(t, "../../cmd/media-worker", "mrr-internal-child")
	env := append(lmwEnvironment(h, tlsServer.Listener.Addr().String(), ca),
		"COMMERCE_MEDIA_RECOVERY_SUPERVISED=1", "COMMERCE_MEDIA_RECOVERY_INTERNAL_CHILD=1",
		"HTTPS_PROXY=http://127.0.0.1:1", "HTTP_PROXY=http://127.0.0.1:1")
	readEnd, writeEnd, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer readEnd.Close()
	defer writeEnd.Close()
	logFile, err := os.CreateTemp(t.TempDir(), "mrr-internal-child-*.log")
	if err != nil {
		t.Fatal(err)
	}
	defer logFile.Close()
	cmd := exec.Command(binary)
	cmd.Env = append([]string{"PATH=" + os.Getenv("PATH")}, env...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = readEnd, logFile, logFile
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	_ = readEnd.Close()
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	reaped := false
	t.Cleanup(func() {
		if !reaped {
			_ = cmd.Process.Kill()
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				t.Errorf("internal child PID %d not reaped", cmd.Process.Pid)
			}
		}
	})
	if _, err := writeEnd.Write([]byte{1}); err != nil {
		t.Fatal(err)
	}
	ready := false
	for deadline := time.Now().Add(8 * time.Second); time.Now().Before(deadline); {
		log, err := os.ReadFile(logFile.Name())
		if err != nil {
			t.Fatal(err)
		}
		if bytes.Contains(log, []byte("media_worker_ready")) {
			ready = true
			break
		}
		select {
		case err := <-done:
			reaped = true
			t.Fatalf("internal child exited before release-backed readiness: %v", err)
		default:
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !ready {
		t.Fatal("internal child never became ready after release byte")
	}
	_ = writeEnd.Close() // EOF after release cancels the native child context.
	select {
	case <-done:
		reaped = true
	case <-time.After(8 * time.Second):
		t.Fatal("internal child ignored post-release EOF")
	}
	t.Logf("native child configured TLS requests before EOF/reap: %d", configuredRequests.Load())
}

func mrrChildPID(parent, exclude int) (int, error) {
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		out, err := exec.Command("pgrep", "-P", strconv.Itoa(parent)).Output()
		if err == nil {
			for _, field := range strings.Fields(string(out)) {
				pid, parseErr := strconv.Atoi(field)
				if parseErr == nil && pid != exclude {
					return pid, nil
				}
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	return 0, context.DeadlineExceeded
}

func mrrWaitEpisode(t *testing.T, owner *pgxpool.Pool, launched time.Time) string {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		var episode string
		err := owner.QueryRow(context.Background(), `SELECT episode_id::text FROM live.media_recovery_episode_scope
		 WHERE created_at >= $1 ORDER BY created_at DESC LIMIT 1`, launched).Scan(&episode)
		if err == nil {
			return episode
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("supervisor did not persist a recovery episode")
	return ""
}

func mrrWaitReadback(t *testing.T, recovery *pgxpool.Pool, episode string, deadline time.Time, accept func(mrrReadback) bool) mrrReadback {
	t.Helper()
	for time.Now().Before(deadline) {
		rows := mrrRead(t, recovery, episode)
		if len(rows) == 1 && accept(rows[0]) {
			return rows[0]
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("recovery readback not reached by deadline; episode=%s", episode)
	return mrrReadback{}
}

func TestLiveMediaRecoveryMRR01SupervisorCrashAndRealClock(t *testing.T) {
	accepted := make(chan struct{}, 1)
	holdStart := make(chan struct{})
	t.Cleanup(func() {
		select {
		case <-holdStart:
		default:
			close(holdStart)
		}
	})
	h := lmeSetup(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/twirp/livekit.Egress/StartEgress" {
			t.Errorf("initial child called %s", r.URL.Path)
			http.Error(w, "wrong method", 500)
			return
		}
		_, _ = io.Copy(io.Discard, r.Body)
		select {
		case accepted <- struct{}{}:
		default:
		}
		select {
		case <-holdStart:
		case <-r.Context().Done():
			return
		}
		// The provider accepted Start but its reply must never reach that child.
	})
	recovery, _ := mrrRecoveryPool(t, h)
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	old := exec.Command(self, "-test.run=^TestLiveMediaExecutionCrashChild$", "-test.timeout=75s")
	old.Env = append(os.Environ(), "LC_LME_CHILD=1", "LC_LME_WORKER_DSN="+h.worker.Config().ConnString(),
		"LC_LME_EXECUTOR_DSN="+h.executor.Config().ConnString(), "LC_LME_TLS_ADDR="+h.server.Listener.Addr().String())
	old.Stdout, old.Stderr = io.Discard, io.Discard
	if err := old.Start(); err != nil {
		t.Fatal(err)
	}
	oldDone := make(chan error, 1)
	go func() { oldDone <- old.Wait() }()
	t.Cleanup(func() { _ = old.Process.Kill() })
	select {
	case <-accepted:
	case err := <-oldDone:
		t.Fatalf("old worker exited before escaped Start: %v", err)
	case <-time.After(20 * time.Second):
		t.Fatal("old worker never issued Start")
	}
	if !h.facts(t).reserved || h.starts.Load() != 1 {
		t.Fatal("provider Start lacked committed original reservation")
	}
	if err := old.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-oldDone:
		if err == nil {
			t.Fatal("SIGKILL returned cleanly")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("old worker not reaped")
	}
	close(holdStart)
	var initialAttempt, maxAttempts int
	if err := h.lp.f.owner.QueryRow(context.Background(), `SELECT attempt,max_attempts FROM river_media.river_job WHERE id=$1`, h.plan.JobID).
		Scan(&initialAttempt, &maxAttempts); err != nil {
		t.Fatal(err)
	}
	wire := make(chan struct{}, 1)
	release := make(chan struct{})
	t.Cleanup(func() {
		select {
		case <-release:
		default:
			close(release)
		}
	})
	tlsServer, ca := lmwTLS(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/twirp/livekit.Egress/ListEgress" {
			t.Errorf("observer issued %s", r.URL.Path)
			http.Error(w, "wrong method", 500)
			return
		}
		var query struct {
			RoomName string `json:"room_name"`
			EgressID string `json:"egress_id"`
			Active   bool   `json:"active"`
		}
		if err := json.NewDecoder(r.Body).Decode(&query); err != nil || query.RoomName != h.plan.RoomName || query.EgressID != "" || query.Active {
			t.Errorf("ROOM selector changed: %+v %v", query, err)
			http.Error(w, "wrong target", 400)
			return
		}
		select {
		case wire <- struct{}{}:
		default:
		}
		select {
		case <-release:
		case <-r.Context().Done():
			return
		}
		lmeReply(w, `{"items":[`+h.observation("EG_mrr_process", "EGRESS_ACTIVE", 100, 120, 0)+`]}`)
	}))
	binary := mrBuild(t, "../../cmd/media-worker", "media-recovery")
	env := lmwEnvironment(h, tlsServer.Listener.Addr().String(), ca)
	env = append(env, "COMMERCE_MEDIA_RECOVERY_SUPERVISED=1", "COMMERCE_MEDIA_RECOVERY_DATABASE_URL="+recovery.Config().ConnString())
	launched := time.Now()
	parent := lmwProcess(t, binary, "mrr-positive", env)
	episode := mrrWaitEpisode(t, h.lp.f.owner, launched)
	var firstCapture int64
	var firstDeadline time.Time
	if err := h.lp.f.owner.QueryRow(context.Background(), `SELECT capture_elapsed_ms,deadline_at FROM live.media_recovery_episode_scope WHERE episode_id=$1::uuid`, episode).
		Scan(&firstCapture, &firstDeadline); err != nil {
		t.Fatal(err)
	}
	select {
	case <-wire:
	case <-time.After(40 * time.Second):
		t.Fatal("observer never queried committed room")
	}
	child, err := mrrChildPID(parent.cmd.Process.Pid, 0)
	if err != nil {
		t.Fatalf("supervisor child absent: %v", err)
	}
	for i := 0; i < 2; i++ {
		if err := syscall.Kill(child, syscall.SIGKILL); err != nil {
			t.Fatal(err)
		}
		next, err := mrrChildPID(parent.cmd.Process.Pid, child)
		if err != nil {
			t.Fatalf("killed child %d not reaped/restarted: %v", i+1, err)
		}
		child = next
	}
	close(release)
	read := mrrWaitReadback(t, recovery, episode, launched.Add(90*time.Second), func(r mrrReadback) bool { return r.disposition == "witnessed" })
	if read.obs == nil || read.source == nil || *read.source != "ROOM" || read.witnessElapsed == nil || *read.witnessElapsed > 90000 ||
		read.operation == nil || *read.operation != h.plan.OperationID {
		t.Fatalf("not committed original ROOM readback: %+v", read)
	}
	if time.Since(launched) > 90*time.Second {
		t.Fatal("readback exceeded launch-based 90s clock")
	}
	var afterCapture int64
	var afterDeadline time.Time
	var episodeCount int
	if err := h.lp.f.owner.QueryRow(context.Background(), `SELECT capture_elapsed_ms,deadline_at FROM live.media_recovery_episode_scope WHERE episode_id=$1::uuid`, episode).
		Scan(&afterCapture, &afterDeadline); err != nil {
		t.Fatal(err)
	}
	if err := h.lp.f.owner.QueryRow(context.Background(), `SELECT count(DISTINCT s.episode_id) FROM live.media_recovery_episode_scope s
	 JOIN integration.operation_events e ON e.episode_id=s.episode_id AND e.episode_event_kind='admitted'
	 WHERE e.operation_id=$1::uuid AND s.created_at>=$2`, h.plan.OperationID, launched).Scan(&episodeCount); err != nil {
		t.Fatal(err)
	}
	if episodeCount != 1 || firstCapture != afterCapture || !firstDeadline.Equal(afterDeadline) {
		t.Fatalf("child restart changed original episode/t0/deadline: count=%d capture=%d/%d deadline=%v/%v", episodeCount, firstCapture, afterCapture, firstDeadline, afterDeadline)
	}
	var jobID int64
	var finalAttempt, finalMax int
	if err := h.lp.f.owner.QueryRow(context.Background(), `SELECT id,attempt,max_attempts FROM river_media.river_job WHERE id=$1`, h.plan.JobID).
		Scan(&jobID, &finalAttempt, &finalMax); err != nil || jobID != h.plan.JobID || finalMax != maxAttempts || finalAttempt < initialAttempt {
		t.Fatalf("observer replaced or retuned original River job: id=%d attempt=%d max=%d err=%v initial_attempt=%d", jobID, finalAttempt, finalMax, err, initialAttempt)
	}
	t.Logf("MRR01 original River attempt: before=%d after=%d; native scheduling may advance it", initialAttempt, finalAttempt)
	if h.starts.Load() != 1 || h.stops.Load() != 0 {
		t.Fatal("recovery duplicated escaped Start or called Stop")
	}
	mrStop(t, parent, syscall.SIGTERM, true)
}

func TestLiveMediaRecoveryMRR01KnownIDQuery(t *testing.T) {
	h := lmrSetup(t, func(w http.ResponseWriter, _ *http.Request) { http.Error(w, "unused provider", 500) })
	lmrStarted(t, h, "EG_mrr_known")
	recovery, _ := mrrRecoveryPool(t, h)
	var queryCount atomic.Int32
	tlsServer, ca := lmwTLS(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/twirp/livekit.Egress/ListEgress" {
			t.Errorf("observer issued %s", r.URL.Path)
			http.Error(w, "wrong method", 500)
			return
		}
		var query struct {
			RoomName string `json:"room_name"`
			EgressID string `json:"egress_id"`
			Active   bool   `json:"active"`
		}
		if err := json.NewDecoder(r.Body).Decode(&query); err != nil || query.EgressID != "EG_mrr_known" || query.RoomName != h.plan.RoomName || query.Active {
			t.Errorf("known-ID Query target changed: %+v %v", query, err)
			http.Error(w, "wrong target", 400)
			return
		}
		queryCount.Add(1)
		lmeReply(w, `{"items":[`+h.observation("EG_mrr_known", "EGRESS_ACTIVE", 100, 120, 0)+`]}`)
	}))
	binary := mrBuild(t, "../../cmd/media-worker", "media-recovery-query")
	env := append(lmwEnvironment(h, tlsServer.Listener.Addr().String(), ca),
		"COMMERCE_MEDIA_RECOVERY_SUPERVISED=1", "COMMERCE_MEDIA_RECOVERY_DATABASE_URL="+recovery.Config().ConnString())
	launched := time.Now()
	parent := lmwProcess(t, binary, "mrr-known-query", env)
	episode := mrrWaitEpisode(t, h.lp.f.owner, launched)
	read := mrrWaitReadback(t, recovery, episode, launched.Add(90*time.Second), func(r mrrReadback) bool { return r.disposition == "witnessed" })
	if read.source == nil || *read.source != "QUERY" || read.obs == nil || read.witnessElapsed == nil || *read.witnessElapsed > 90000 || queryCount.Load() == 0 {
		t.Fatalf("known-ID Query not witnessed: %+v calls=%d", read, queryCount.Load())
	}
	if h.starts.Load()+h.stops.Load() != 0 {
		t.Fatal("observer issued Start/Stop")
	}
	mrStop(t, parent, syscall.SIGTERM, true)
}

func TestLiveMediaRecoveryMRR02RealNinetySecondMiss(t *testing.T) {
	h := lmeSetup(t, func(w http.ResponseWriter, _ *http.Request) { http.Error(w, "unused provider", 500) })
	recovery, _ := mrrRecoveryPool(t, h)
	mrrReserveUnansweredStart(t, h)
	var queryCount, lateCalls atomic.Int32
	var launched time.Time
	var launchedAt atomic.Value
	tlsServer, ca := lmwTLS(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/twirp/livekit.Egress/ListEgress" {
			t.Errorf("unexpected provider method %s", r.URL.Path)
		}
		queryCount.Add(1)
		if stamp := launchedAt.Load(); stamp != nil && time.Since(stamp.(time.Time)) >= 90*time.Second {
			lateCalls.Add(1)
		}
		http.Error(w, "local provider fault", http.StatusServiceUnavailable)
	}))
	binary := mrBuild(t, "../../cmd/media-worker", "media-recovery-timeout")
	env := lmwEnvironment(h, tlsServer.Listener.Addr().String(), ca)
	env = append(env, "COMMERCE_MEDIA_RECOVERY_SUPERVISED=1", "COMMERCE_MEDIA_RECOVERY_DATABASE_URL="+recovery.Config().ConnString())
	launched = time.Now()
	launchedAt.Store(launched)
	parent := lmwProcess(t, binary, "mrr-real-timeout", env)
	episode := mrrWaitEpisode(t, h.lp.f.owner, launched)
	read := mrrWaitReadback(t, recovery, episode, launched.Add(105*time.Second), func(r mrrReadback) bool { return r.timeoutAt != nil })
	if time.Since(launched) < 90*time.Second || read.disposition != "timeout" || read.obs != nil || read.operation == nil || *read.operation != h.plan.OperationID {
		t.Fatalf("false 90s negative: elapsed=%s read=%+v", time.Since(launched), read)
	}
	if err := syscall.Kill(parent.cmd.Process.Pid, 0); err != nil {
		t.Fatalf("supervisor exited before miss: %v", err)
	}
	if queryCount.Load() == 0 || lateCalls.Load() != 0 {
		t.Fatalf("provider fault not exercised or I/O scheduled after t0+90s: calls=%d late=%d", queryCount.Load(), lateCalls.Load())
	}
	log, err := os.ReadFile(parent.logPath)
	if err != nil {
		t.Fatal(err)
	}
	parsedDSN, err := url.Parse(recovery.Config().ConnString())
	if err != nil {
		t.Fatal(err)
	}
	password, _ := parsedDSN.User.Password()
	for _, forbidden := range []string{recovery.Config().ConnString(), password, h.project.Config.APISecret, lmwEnvValue(env, "COMMERCE_MEDIA_MATERIAL_KEYS_JSON")} {
		if forbidden != "" && bytes.Contains(log, []byte(forbidden)) {
			t.Fatal("supervisor deadline log exposed material")
		}
	}
	if !bytes.Contains(log, []byte("media_recovery_deadline_missed")) {
		t.Fatalf("missing redacted 90s alert: %s", parent.logPath)
	}
	if h.starts.Load()+h.stops.Load() != 0 {
		t.Fatal("fault path called Start or Stop")
	}
	mrStop(t, parent, syscall.SIGTERM, true)
}

func TestLiveMediaRecoveryMRR02TimelyReadbackWitnessCommitsAfterNinety(t *testing.T) {
	h := lmeSetup(t, func(w http.ResponseWriter, _ *http.Request) { http.Error(w, "unused", 500) })
	recovery, _ := mrrRecoveryPool(t, h)
	old := mrrReserveUnansweredStart(t, h)
	var finished string
	if err := h.executor.QueryRow(context.Background(), `SELECT live.finish_media_uncertain($1::uuid,$2::bigint,$3::bytea,'remote_unknown')`,
		h.plan.OperationID, old.generation, old.token).Scan(&finished); err != nil || finished != "observe" {
		t.Fatalf("old native lease did not release: %s %v", finished, err)
	}
	tlsServer, ca := lmwTLS(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/twirp/livekit.Egress/ListEgress" {
			t.Errorf("readback path issued %s", r.URL.Path)
			http.Error(w, "wrong method", 500)
			return
		}
		lmeReply(w, `{"items":[`+h.observation("EG_mrr_witness_delay", "EGRESS_ACTIVE", 100, 120, 0)+`]}`)
	}))
	// Fixture-only trigger rejects early attestations, then waits after taking
	// the original operation lock. Its advisory wait is released just after t0+90;
	// Timeout cannot commit first while this bounded Witness call holds that lock.
	function := fmt.Sprintf(`CREATE FUNCTION live.mrr_test_block_witness() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE gate_at timestamptz;
BEGIN
 IF NEW.operation_id='%s'::uuid AND NEW.episode_event_kind='witnessed' THEN
  SELECT deadline_at-interval '3 seconds' INTO gate_at FROM live.media_recovery_episode_scope WHERE episode_id=NEW.episode_id;
  IF clock_timestamp()<gate_at THEN
   RAISE EXCEPTION 'fixture attestation not yet released' USING ERRCODE='P0001';
  END IF;
  PERFORM pg_advisory_xact_lock(824,501234);
 END IF;
 RETURN NEW;
END $$`, h.plan.OperationID)
	mustExec(t, h.lp.f.owner, function)
	mustExec(t, h.lp.f.owner, `CREATE TRIGGER mrr_test_witness_lock BEFORE INSERT ON integration.operation_events
 FOR EACH ROW WHEN (NEW.episode_event_kind='witnessed') EXECUTE FUNCTION live.mrr_test_block_witness()`)
	t.Cleanup(func() {
		mustExec(t, h.lp.f.owner, `DROP TRIGGER IF EXISTS mrr_test_witness_lock ON integration.operation_events`)
		mustExec(t, h.lp.f.owner, `DROP FUNCTION IF EXISTS live.mrr_test_block_witness()`)
	})
	ctx := context.Background()
	holder, err := h.lp.f.owner.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer holder.Release()
	var holderPID int
	if err := holder.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&holderPID); err != nil {
		t.Fatal(err)
	}
	if _, err := holder.Exec(ctx, `SELECT pg_advisory_lock(824,501234)`); err != nil {
		t.Fatal(err)
	}
	locked := true
	defer func() {
		if locked {
			_, _ = holder.Exec(ctx, `SELECT pg_advisory_unlock(824,501234)`)
		}
	}()
	binary := mrBuild(t, "../../cmd/media-worker", "mrr-witness-after-ninety")
	env := append(lmwEnvironment(h, tlsServer.Listener.Addr().String(), ca),
		"COMMERCE_MEDIA_RECOVERY_SUPERVISED=1", "COMMERCE_MEDIA_RECOVERY_DATABASE_URL="+recovery.Config().ConnString())
	launched := time.Now()
	parent := lmwProcess(t, binary, "mrr-witness-after-ninety", env)
	episode := mrrWaitEpisode(t, h.lp.f.owner, launched)
	qualified := mrrWaitReadback(t, recovery, episode, launched.Add(60*time.Second), func(r mrrReadback) bool { return r.obs != nil })
	if qualified.operation == nil || *qualified.operation != h.plan.OperationID || qualified.source == nil || *qualified.source != "ROOM" ||
		qualified.generation == nil || qualified.obs == nil || qualified.timeoutAt != nil {
		t.Fatalf("no committed fresh ROOM readback before deadline: %+v", qualified)
	}
	var baseline int64
	if err := h.lp.f.owner.QueryRow(ctx, `SELECT recovery_baseline_generation FROM live.media_execution_state WHERE operation_id=$1::uuid`,
		h.plan.OperationID).Scan(&baseline); err != nil || *qualified.generation <= baseline {
		t.Fatalf("pre-admission or stale observation accepted: generation=%v baseline=%d err=%v", qualified.generation, baseline, err)
	}
	// The 90 s clock is the episode's own deadline_at (set by the supervisor from its episode start, DB clock), not
	// the test's launch instant: under a loaded full-suite run the episode starts seconds after launch, and the
	// fixture gate (deadline_at-3s) plus the supervisor's 1 s tick then fell past launched+89s (R2 release gate
	// 2026-09-30: blocked=false elapsed=1m29s). The DB-side remaining time is mapped onto the local monotonic clock.
	var remainingMS int64
	if err := h.lp.f.owner.QueryRow(ctx, `SELECT (extract(epoch FROM deadline_at-clock_timestamp())*1000)::bigint
	 FROM live.media_recovery_episode_scope WHERE episode_id=$1::uuid`, episode).Scan(&remainingMS); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(time.Duration(remainingMS) * time.Millisecond)
	var blocked bool
	for time.Now().Before(deadline.Add(-300 * time.Millisecond)) {
		if time.Until(deadline) > 3*time.Second {
			time.Sleep(100 * time.Millisecond)
			continue
		}
		if err := h.lp.f.owner.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_stat_activity a, live.media_recovery_episode_scope s
          WHERE s.episode_id=$2::uuid AND a.state='active' AND a.query LIKE '%witness_media_recovery_episode%'
           AND a.query_start>=s.deadline_at-interval '4 seconds' AND $1=ANY(pg_blocking_pids(a.pid)))`, holderPID, episode).Scan(&blocked); err != nil {
			t.Fatal(err)
		}
		if blocked {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !blocked || !time.Now().Before(deadline) {
		t.Fatalf("fixture did not place a timely parent Witness behind original-operation lock: blocked=%v to_deadline=%s since_launch=%s",
			blocked, time.Until(deadline), time.Since(launched))
	}
	if wait := time.Until(deadline.Add(200 * time.Millisecond)); wait > 0 {
		time.Sleep(wait)
	}
	if _, err := holder.Exec(ctx, `SELECT pg_advisory_unlock(824,501234)`); err != nil {
		t.Fatal(err)
	}
	locked = false
	read := mrrWaitReadback(t, recovery, episode, deadline.Add(8*time.Second), func(r mrrReadback) bool { return r.disposition == "witnessed" })
	if time.Now().Before(deadline) || read.scope != "finished" || read.timeoutAt != nil ||
		read.obs == nil || *read.obs != *qualified.obs || read.witnessElapsed == nil || *read.witnessElapsed >= 90000 {
		t.Fatalf("timely parent readback did not retain post90 Witness: %+v past_deadline=%s", read, -time.Until(deadline))
	}
	log, err := os.ReadFile(parent.logPath)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(log, []byte("media_recovery_deadline_missed")) {
		t.Fatalf("Witness-first completion emitted false miss: %s", parent.logPath)
	}
	mrStop(t, parent, syscall.SIGTERM, true)
}

func TestLiveMediaRecoveryMRR02LateDatabaseRecoveryKeepsOriginalDeadline(t *testing.T) {
	h := lmeSetup(t, func(w http.ResponseWriter, _ *http.Request) { http.Error(w, "unused provider", 500) })
	recovery, login := mrrRecoveryPool(t, h)
	mrrReserveUnansweredStart(t, h)
	var requests atomic.Int32
	tlsServer, ca := lmwTLS(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		http.Error(w, "unexpected provider request", 500)
	}))
	binary := mrBuild(t, "../../cmd/media-worker", "mrr-late-db")
	env := lmwEnvironment(h, tlsServer.Listener.Addr().String(), ca)
	env = lmwReplace(env, "COMMERCE_MEDIA_MATERIAL_KEYS_JSON", "not-json")
	env = append(env, "COMMERCE_MEDIA_RECOVERY_SUPERVISED=1", "COMMERCE_MEDIA_RECOVERY_DATABASE_URL="+recovery.Config().ConnString())
	quotedLogin := pgx.Identifier{login}.Sanitize()
	mustExec(t, h.lp.f.owner, "ALTER ROLE "+quotedLogin+" NOLOGIN")
	restored := false
	t.Cleanup(func() {
		if !restored {
			mustExec(t, h.lp.f.owner, "ALTER ROLE "+quotedLogin+" LOGIN")
		}
	})
	launched := time.Now()
	parent := lmwProcess(t, binary, "mrr-late-db", env)
	mrReadyLog(t, parent, "media_recovery_native_config_invalid")
	if wait := time.Until(launched.Add(90 * time.Second)); wait > 0 {
		time.Sleep(wait)
	}
	var alert []byte
	for time.Now().Before(launched.Add(97 * time.Second)) {
		var err error
		alert, err = os.ReadFile(parent.logPath)
		if err != nil {
			t.Fatal(err)
		}
		if bytes.Contains(alert, []byte("media_recovery_deadline_missed")) {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if elapsed := time.Since(launched); elapsed < 90*time.Second || elapsed >= 97*time.Second ||
		!bytes.Contains(alert, []byte("media_recovery_deadline_missed")) {
		t.Fatalf("missing prompt local deadline signal with DB unavailable: elapsed=%s log=%s", elapsed, parent.logPath)
	}
	match := regexp.MustCompile(`media_recovery_deadline_missed[^\n]*episode_id=([0-9a-f-]{36})`).FindSubmatch(alert)
	if len(match) != 2 {
		t.Fatalf("deadline alert lacked original episode identity: %s", parent.logPath)
	}
	if err := syscall.Kill(parent.cmd.Process.Pid, 0); err != nil {
		t.Fatalf("parent died during initial DB outage: %v", err)
	}
	mustExec(t, h.lp.f.owner, "ALTER ROLE "+quotedLogin+" LOGIN")
	restored = true
	episode := mrrWaitEpisode(t, h.lp.f.owner, launched)
	if episode != string(match[1]) {
		t.Fatalf("late DB access started a new clock/episode: signal=%s durable=%s", match[1], episode)
	}
	read := mrrRead(t, recovery, episode)
	if len(read) != 1 || read[0].scope != "overdue" || read[0].known || read[0].candidates != 1 ||
		read[0].disposition != "timeout" || read[0].timeoutAt == nil || read[0].operation == nil || *read[0].operation != h.plan.OperationID {
		t.Fatalf("late DB recovery invented coverage or PASS: %+v", read)
	}
	var captureElapsed int64
	if err := h.lp.f.owner.QueryRow(context.Background(), `SELECT capture_elapsed_ms FROM live.media_recovery_episode_scope WHERE episode_id=$1::uuid`, episode).
		Scan(&captureElapsed); err != nil || captureElapsed < 90000 {
		t.Fatalf("late capture reset original monotonic deadline: elapsed=%d err=%v", captureElapsed, err)
	}
	time.Sleep(time.Second)
	if requests.Load() != 0 {
		t.Fatalf("DB outage/recovery reached provider after deadline: requests=%d", requests.Load())
	}
	mrStop(t, parent, syscall.SIGTERM, true)
}
