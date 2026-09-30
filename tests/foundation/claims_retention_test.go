// claims_retention_test.go holds the independent CRP02-CRP08 gates of U08 (contract
// contracts/claims-retention-purge-v1.md §7; brief docs/delivery/units/retention-tests.md). Written from the
// FROZEN contract, D1-D10 and the frozen Go interface only. Evidence label: REAL_PG (PG 18 via
// scripts/dev/test-focused.sh); no network, no LIVE, no production DSN, no real buyer data.
//
// Conventions of this file:
//   - Retention logins are real LOGIN roles the test creates and grants exactly commerce_retention_job /
//     commerce_retention_operator (GRANT ... WITH INHERIT TRUE, SET FALSE); the definers are only ever
//     reached through them (crEnv.job / crEnv.op), never through the owner pool.
//   - Aged timestamps, held row locks and pg_stat_activity/pg_locks-gated interleavings use the owner pool
//     (the fixture superuser) and are disclosed at each test ("owner-seeded").
//   - Every sender id, comment id, label, Page token and actor key is a synthetic sentinel.
//   - Thresholds are seeded +/- crMargin around now() (wall clock; the contract's "-1 s / +1 s" cannot be exact
//     against clock_timestamp()); crMargin is checked against the measured seed-to-run latency.
//   - Every test removes what it seeded and restores the policy row (enforced=false, default periods).
package foundation_test

import (
	"bytes"
	"context"
	"crypto/sha256"
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
	"sort"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"livecommerce/internal/buyer"
	"livecommerce/internal/claims"
	"livecommerce/internal/command"
	"livecommerce/internal/httpapi"
	"livecommerce/internal/integrations/meta"
	"livecommerce/internal/platform"
	"livecommerce/internal/retention"
	"livecommerce/migrations"
)

// crMargin is how far from the exact threshold the +/- rows are seeded. A definer runs some ms
// after the seed; crMaxLatency guards that assumption so a slow host fails loudly, not flakily.
const (
	crMargin     = 4 * time.Second
	crMaxLatency = 2 * time.Second
)

// crAgo is the SQL text of "clock_timestamp() - <days> days + <offset seconds>" (integers only, no user text).
func crAgo(days int, offsetSec int) string {
	return fmt.Sprintf("(clock_timestamp() - interval '%d days' + interval '%d seconds')", days, offsetSec)
}

// crOld / crNew are the SQL texts of the two sides of a threshold: just past it (eligible) and just inside (kept).
func crOld(days int) string { return crAgo(days, -int(crMargin/time.Second)) }
func crNew(days int) string { return crAgo(days, int(crMargin/time.Second)) }

// ---------------------------------------------------------------------------------------
// World: tenant/store/principal the seeds attach to. The shared fixture (tenant A) or a throw-away cluster.
// ---------------------------------------------------------------------------------------

type crWorld struct {
	owner                       *pgxpool.Pool
	f                           *testFixture // nil in a throw-away cluster (no catalog stock helpers)
	tenant, store, store2, prin string
	sessions                    []string
	operations                  []string
	events                      []string // meta_inbox.events ids whose social rows this world created
	conversations               []string
	owners                      []string
	logs                        []string
	bindings                    []string
	sources                     []string
}

func crSharedWorld(f *testFixture) *crWorld {
	return &crWorld{owner: f.owner, f: f, tenant: f.tenantA, store: f.storeA1, store2: f.storeA2, prin: f.principalA}
}

// crFreshWorld seeds a tenant with two stores and a principal (owner SQL, synthetic) into a throw-away cluster.
func crFreshWorld(t *testing.T, owner *pgxpool.Pool) *crWorld {
	t.Helper()
	w := &crWorld{owner: owner, tenant: randomUUID(), store: randomUUID(), store2: randomUUID(), prin: randomUUID()}
	mustExec(t, owner, `INSERT INTO control.tenants(id,name) VALUES($1,'retention-gate-tenant')`, w.tenant)
	mustExec(t, owner, `INSERT INTO control.stores(tenant_id,id,name,currency) VALUES($1,$2,'rg-store-1','USD'),($1,$3,'rg-store-2','USD')`, w.tenant, w.store, w.store2)
	mustExec(t, owner, `INSERT INTO identity.principals(id) VALUES($1)`, w.prin)
	mustExec(t, owner, `INSERT INTO identity.memberships(tenant_id,principal_id) VALUES($1,$2)`, w.tenant, w.prin)
	return w
}

// cleanup removes everything this world seeded (owner SQL, FK order); safe to call twice.
func (w *crWorld) cleanup() {
	ctx := context.Background()
	S := w.sessions
	run := func(q string, args ...any) { _, _ = w.owner.Exec(ctx, q, args...) }
	if len(w.events) > 0 {
		run(`DELETE FROM social.messages WHERE event_id=ANY($1::uuid[])`, w.events)
		run(`DELETE FROM social.comment_events WHERE event_id=ANY($1::uuid[])`, w.events)
	}
	if len(w.conversations) > 0 {
		run(`DELETE FROM social.messages WHERE conversation_id=ANY($1::uuid[])`, w.conversations)
		run(`DELETE FROM social.conversations WHERE id=ANY($1::uuid[])`, w.conversations)
	}
	if len(w.operations) > 0 {
		run(`DELETE FROM integration.operation_events WHERE operation_id=ANY($1::uuid[])`, w.operations)
		run(`DELETE FROM integration.operations WHERE id=ANY($1::uuid[])`, w.operations)
	}
	if len(S) > 0 {
		run(`DELETE FROM claims.meta_intake WHERE session_id=ANY($1::uuid[])`, S)
		run(`DELETE FROM live.claim_sources WHERE session_id=ANY($1::uuid[])`, S)
		run(`DELETE FROM claims.links WHERE bundle_id IN (SELECT id FROM claims.bundles WHERE session_id=ANY($1::uuid[]))`, S)
		run(`DELETE FROM claims.events WHERE session_id=ANY($1::uuid[])`, S)
		run(`DELETE FROM claims.lines WHERE session_id=ANY($1::uuid[])`, S)
		run(`DELETE FROM claims.bundles WHERE session_id=ANY($1::uuid[])`, S)
		run(`DELETE FROM live.offers WHERE session_id=ANY($1::uuid[])`, S)
		run(`DELETE FROM live.claim_windows WHERE session_id=ANY($1::uuid[])`, S)
		run(`DELETE FROM live.sessions WHERE id=ANY($1::uuid[])`, S)
	}
	if len(w.owners) > 0 {
		run(`DELETE FROM buyer.owners WHERE id=ANY($1::uuid[])`, w.owners)
	}
	if len(w.logs) > 0 {
		run(`DELETE FROM claims.retention_log WHERE id=ANY($1::uuid[])`, w.logs)
	}
	w.sessions, w.operations, w.events, w.conversations, w.owners, w.logs = nil, nil, nil, nil, nil, nil
}

// crSess is one live session with its claim window.
type crSess struct{ id, store string }

// session inserts live.sessions + a never-opened CLOSED window (owner-seeded: sessions and windows are created
// by the merchant APIs in production; retention only reads windows).
func (w *crWorld) session(t *testing.T, store string) crSess {
	t.Helper()
	id := randomUUID()
	mustExec(t, w.owner, `INSERT INTO live.sessions(id,tenant_id,store_id,principal_id,title) VALUES($1,$2,$3,$4,$5)`, id, w.tenant, store, w.prin, "retention gate "+t04Tag())
	mustExec(t, w.owner, `INSERT INTO live.claim_windows(tenant_id,store_id,session_id,state,match_mode,generation,principal_id) VALUES($1,$2,$3,'CLOSED','EXACT',0,$4)`, w.tenant, store, id, w.prin)
	w.sessions = append(w.sessions, id)
	return crSess{id, store}
}

// closedAt makes the window CLOSED with closed_at = the SQL timestamp expression (opened one hour earlier).
func (w *crWorld) closedAt(t *testing.T, s crSess, expr string) {
	t.Helper()
	mustExec(t, w.owner, `UPDATE live.claim_windows SET state='CLOSED',generation=greatest(generation,1),opened_at=`+expr+`-interval '1 hour',closed_at=`+expr+`
		WHERE tenant_id=$1 AND store_id=$2 AND session_id=$3`, w.tenant, s.store, s.id)
}

// reopen moves a CLOSED window to OPEN (new generation), the "reopened window" and OPEN cases.
func (w *crWorld) reopen(t *testing.T, s crSess) {
	t.Helper()
	mustExec(t, w.owner, `UPDATE live.claim_windows SET state='OPEN',generation=generation+1,opened_at=clock_timestamp(),closed_at=NULL,updated_at=clock_timestamp()
		WHERE tenant_id=$1 AND store_id=$2 AND session_id=$3`, w.tenant, s.store, s.id)
}

// crBundle is one claims.bundles row.
type crBundle struct{ id, platform, actor, label string }

func crHex64() string { return hex.EncodeToString(randomBytes(32)) }

// bundle inserts a bundle (owner-seeded; production writes it through claims ingest). manual: label set;
// facebook/instagram: label NULL (0060/0064 CHECK).
func (w *crWorld) bundle(t *testing.T, s crSess, platform, actor, label string) crBundle {
	t.Helper()
	if actor == "" {
		actor = crHex64()
	}
	b := crBundle{id: randomUUID(), platform: platform, actor: actor, label: label}
	var lbl any
	if label != "" {
		lbl = label
	}
	mustExec(t, w.owner, `INSERT INTO claims.bundles(tenant_id,store_id,id,session_id,platform,actor_key,label) VALUES($1,$2,$3,$4,$5,$6,$7)`,
		w.tenant, s.store, b.id, s.id, platform, actor, lbl)
	return b
}

// bind attaches a buyer owner to the bundle (owner_id/bound_at, the state C2 must clear).
func (w *crWorld) bind(t *testing.T, s crSess, b crBundle) string {
	t.Helper()
	owner := randomUUID()
	mustExec(t, w.owner, `INSERT INTO buyer.owners(id,tenant_id,store_id) VALUES($1,$2,$3)`, owner, w.tenant, s.store)
	w.owners = append(w.owners, owner)
	mustExec(t, w.owner, `UPDATE claims.bundles SET owner_id=$1,bound_at=clock_timestamp() WHERE tenant_id=$2 AND store_id=$3 AND id=$4`, owner, w.tenant, s.store, b.id)
	return owner
}

// link inserts the bundle's buyer link with expires_at = the SQL expression (issued 1 h earlier; 0060 TTL CHECK).
func (w *crWorld) link(t *testing.T, s crSess, b crBundle, expiresExpr string) []byte {
	t.Helper()
	hash := sha256.Sum256(randomBytes(32))
	mustExec(t, w.owner, `INSERT INTO claims.links(tenant_id,store_id,bundle_id,token_hash,generation,issued_at,expires_at,principal_id)
		VALUES($1,$2,$3,$4,1,`+expiresExpr+`-interval '1 hour',`+expiresExpr+`,$5)`, w.tenant, s.store, b.id, hash[:], w.prin)
	return hash[:]
}

// offerSKU creates one offer for the session on a fresh SKU (owner-seeded; needs the shared fixture catalog helper).
func (w *crWorld) offer(t *testing.T, s crSess) (offer, sku string) {
	t.Helper()
	if w.f == nil {
		t.Fatal("offer seeding needs the shared fixture")
	}
	sku = lcSKUs(t, w.f, w.tenant, s.store, "USD", 1)[0]
	offer = randomUUID()
	mustExec(t, w.owner, `INSERT INTO live.offers(tenant_id,store_id,id,session_id,keyword,sku_id,max_quantity_per_claim,principal_id) VALUES($1,$2,$3,$4,$5,$6,5,$7)`,
		w.tenant, s.store, offer, s.id, "K"+strings.ToUpper(t04Tag()[:8]), sku, w.prin)
	return offer, sku
}

// line inserts claims.lines with applied_version set (the column C2 must clear) and one ACCEPTED manual event
// (append-only fact that must stay byte-identical). Owner-seeded.
func (w *crWorld) line(t *testing.T, s crSess, b crBundle, offer, sku string, qty int) {
	t.Helper()
	mustExec(t, w.owner, `INSERT INTO claims.lines(tenant_id,store_id,session_id,bundle_id,offer_id,sku_id,quantity,version,applied_version) VALUES($1,$2,$3,$4,$5,$6,$7,1,1)`,
		w.tenant, s.store, s.id, b.id, offer, sku, qty)
	mustExec(t, w.owner, `INSERT INTO claims.events(tenant_id,store_id,session_id,window_generation,source_kind,source_event_id,platform,occurred_at,grammar_version,grammar_kind,match_mode,outcome,offer_id,quantity,explicit_quantity,bundle_id,line_version,bundle_version,principal_id)
		VALUES($1,$2,$3,1,'manual',$4,'manual',clock_timestamp(),'kw-v1','MATCH','EXACT','ACCEPTED',$5,$6,true,$7,1,1,$8)`,
		w.tenant, s.store, s.id, randomUUID(), offer, qty, b.id, w.prin)
}

// crMeta is a synthetic Meta lane: binding + claim source in one session.
type crMeta struct {
	binding, source, object, asset, platform string
	sess                                     crSess
}

// source inserts integration.bindings + live.claim_sources so meta_intake rows have a valid source (owner-seeded).
func (w *crWorld) source(t *testing.T, s crSess, object string) crMeta {
	t.Helper()
	return w.sourceOn(t, s, object, miAsset())
}

// sourceOn is source for a given asset id (one actor across two stores needs the same asset in both).
func (w *crWorld) sourceOn(t *testing.T, s crSess, object, asset string) crMeta {
	t.Helper()
	m := crMeta{binding: randomUUID(), source: randomUUID(), object: object, asset: asset, sess: s, platform: "facebook"}
	if object == "instagram" {
		m.platform = "instagram"
	}
	mustExec(t, w.owner, `INSERT INTO integration.bindings(id,tenant_id,store_id,principal_id,provider,external_asset_id) VALUES($1,$2,$3,$4,$5,$6)`, m.binding, w.tenant, s.store, w.prin, m.platform, m.asset)
	w.bindings = append(w.bindings, m.binding)
	mustExec(t, w.owner, `INSERT INTO live.claim_sources(tenant_id,store_id,id,session_id,platform,binding_id,binding_version,object,asset_id,source_object_id,principal_id)
		VALUES($1,$2,$3,$4,$5,$6,1,$7,$8,$9,$10)`, w.tenant, s.store, m.source, s.id, m.platform, m.binding, object, m.asset, m.asset+"_"+crDigits(9), w.prin)
	return m
}

func crDigits(n int) string { return mciDigits(n) }

// intake inserts a claims.meta_intake row (owner-seeded, aged via receivedExpr). state PENDING/APPLIED/DROPPED/FAILED.
func (w *crWorld) intake(t *testing.T, m crMeta, comment, actor, state, receivedExpr string, inboxEvent string) {
	t.Helper()
	if inboxEvent == "" {
		inboxEvent = randomUUID()
	}
	var applied, drop, fail any
	switch state {
	case "APPLIED":
		applied = randomUUID()
	case "DROPPED":
		drop = "window_closed"
	case "FAILED":
		fail = "synthetic_fail"
	}
	mustExec(t, w.owner, `INSERT INTO claims.meta_intake(tenant_id,store_id,inbox_event_id,source_id,session_id,platform,app_id,object,asset_id,comment_ref,live_media,actor_key,
		occurred_at,received_at,grammar_version,grammar_kind,state,drop_reason,fail_code,applied_event_id,not_before)
		VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,false,$11,`+receivedExpr+`,`+receivedExpr+`,'kw-v1','NO_MATCH',$12,$13,$14,$15,`+receivedExpr+`)`,
		w.tenant, m.sess.store, inboxEvent, m.source, m.sess.id, m.platform, miApp, m.object, m.asset, comment, actor, state, drop, fail, applied)
}

// operation inserts a meta.private_reply ledger row in the state (owner-seeded; production plans it in
// integration.plan_claim_reply). Fields satisfy the 0008 lease/generation CHECKs.
func (w *crWorld) operation(t *testing.T, m crMeta, b crBundle, comment, state, createdExpr string) string {
	t.Helper()
	return w.operationAs(t, m, b, comment, state, createdExpr, "meta.private_reply")
}

// operationAs is operation with another action (rows the retention role must not even read).
func (w *crWorld) operationAs(t *testing.T, m crMeta, b crBundle, comment, state, createdExpr, action string) string {
	t.Helper()
	id := randomUUID()
	req := map[string]any{"v": 1, "platform": m.platform, "source_id": m.source, "asset_id": m.asset, "comment_ref": comment, "bundle_id": b.id,
		"session_id": m.sess.id, "link_generation": 1, "locale": "zh-TW", "message_type": "first_private_reply"}
	raw, _ := json.Marshal(req)
	hash := sha256.Sum256(raw)
	key := "mpr:" + hex.EncodeToString(func() []byte { s := sha256.Sum256([]byte(m.object + "|" + m.asset + "|" + comment)); return s[:] }())[:48]
	gen, mode, until, tok := 1, "", any(nil), any(nil)
	switch state {
	case "READY":
		gen = 0
	case "DISPATCHING":
		mode, until, tok = "dispatch", time.Now().Add(time.Hour), make([]byte, 32)
	}
	mustExec(t, w.owner, `INSERT INTO integration.operations(tenant_id,store_id,id,principal_id,binding_id,binding_version,provider,external_asset_id,purpose,action,semantic_key,request_hash,request,job_id,state,generation,lease_mode,lease_until,lease_token_hash,created_at,updated_at)
		VALUES($1,$2,$3,$4,$5,1,$6,$7,'service',$17,$8,$9,$10::jsonb,$11,$12,$13,$14,$15,$16,`+createdExpr+`,`+createdExpr+`)`,
		w.tenant, m.sess.store, id, w.prin, m.binding, m.platform, m.asset, key, hash[:], string(raw), 1+int(time.Now().UnixNano()%1000000), state, gen, mode, until, tok, action)
	w.operations = append(w.operations, id)
	return id
}

// ---------------------------------------------------------------------------------------
// Environment: retention pools + policy handling
// ---------------------------------------------------------------------------------------

type crEnv struct {
	t         *testing.T
	f         *testFixture
	w         *crWorld
	job, op   *pgxpool.Pool
	jobDSN    string
	opDSN     string
	actorRaw  []byte
	actor     meta.ClaimsActorKey
	seedStart time.Time
	holds     []func() // open transactions of this scenario; rolled back before its seeds are removed
}

// crSetup opens the two retention logins through the platform admission functions (they validate the login),
// registers cleanup (seeds removed, policy restored) and leaves the database with no purge-eligible rows.
func crSetup(t *testing.T) *crEnv {
	t.Helper()
	f := fixture(t)
	ctx := context.Background()
	e := &crEnv{t: t, f: f, w: crSharedWorld(f)}
	e.jobDSN = miRole(t, f, "commerce_retention_job")
	e.opDSN = miRole(t, f, "commerce_retention_operator")
	var err error
	if e.job, err = platform.OpenRetentionJobPool(ctx, e.jobDSN); err != nil {
		t.Fatalf("dedicated retention job login refused: %v", err)
	}
	if e.op, err = platform.OpenRetentionOperatorPool(ctx, e.opDSN); err != nil {
		t.Fatalf("dedicated retention operator login refused: %v", err)
	}
	t.Cleanup(e.job.Close)
	t.Cleanup(e.op.Close)
	e.actorRaw = randomBytes(32)
	if e.actor, err = meta.NewClaimsActorKey(e.actorRaw); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		e.w.cleanup()
		e.resetPolicy()
	})
	e.resetPolicy()
	e.quiesce()
	return e
}

// resetPolicy restores the migration defaults (owner SQL; the row is shared, global state).
func (e *crEnv) resetPolicy() {
	_, _ = e.f.owner.Exec(context.Background(), `UPDATE claims.retention_policy SET enforced=false,link_days=7,intake_days=30,claims_days=90,social_days=30,updated_at=clock_timestamp()`)
}

// policy sets the retention policy through the operator API (CAS on the current version).
func (e *crEnv) policy(enforced bool, link, intake, claims, social int) {
	e.t.Helper()
	st, err := retention.GetStatus(context.Background(), e.op)
	if err != nil {
		e.t.Fatalf("GetStatus: %v", err)
	}
	if _, err := retention.SetPolicy(context.Background(), e.op, st.Version, retention.Policy{Enforced: enforced, LinkDays: link, IntakeDays: intake, ClaimsDays: claims, SocialDays: social}); err != nil {
		e.t.Fatalf("SetPolicy(enforced=%t %d/%d/%d/%d): %v", enforced, link, intake, claims, social, err)
	}
}

func (e *crEnv) enforceDefaults() { e.policy(true, 7, 30, 90, 30) }

// quiesce purges whatever earlier tests left eligible in the shared database (aged rows only can be
// eligible), then leaves the policy unenforced. It fails if the database cannot be brought to a clean state.
func (e *crEnv) quiesce() {
	e.t.Helper()
	e.policy(true, 7, 30, 90, 30)
	for i := 0; i < 50; i++ {
		c, err := retention.RunOnce(context.Background(), e.job, 1000)
		if err != nil {
			e.t.Fatalf("quiesce run: %v", err)
		}
		if c["more"] == 0 && c["busy"] == 0 {
			break
		}
	}
	e.policy(false, 7, 30, 90, 30)
}

// run runs one enforced-or-not batch on the retention job login and checks the seed-to-run latency.
func (e *crEnv) run(limit int) retention.Counts {
	e.t.Helper()
	if !e.seedStart.IsZero() && time.Since(e.seedStart) > crMaxLatency*4 {
		// Rows were seeded crMargin around now(); a run this late would cross the boundary on a slow host.
		e.t.Logf("note: %v elapsed between seeding and the run (margin %v)", time.Since(e.seedStart), crMargin)
	}
	c, err := retention.RunOnce(context.Background(), e.job, limit)
	if err != nil {
		e.t.Fatalf("RunOnce(%d): %v", limit, err)
	}
	return c
}

func (e *crEnv) markSeed() { e.seedStart = time.Now() }

// releaseHolds rolls back every transaction the scenario still holds open (idempotent).
func (e *crEnv) releaseHolds() {
	for _, f := range e.holds {
		f()
	}
	e.holds = nil
}

// ---------------------------------------------------------------------------------------
// Assertions and digests
// ---------------------------------------------------------------------------------------

// crDigest fingerprints a table restricted by where (owner SQL) so "byte-identical" is checked on row text.
func crDigest(t *testing.T, pool *pgxpool.Pool, table, where string, args ...any) string {
	t.Helper()
	if where == "" {
		where = "true"
	}
	var d string
	if err := pool.QueryRow(context.Background(), `SELECT count(*)::text||':'||coalesce(md5(string_agg(x::text,E'\n' ORDER BY x::text)),'') FROM `+table+` x WHERE `+where, args...).Scan(&d); err != nil {
		t.Fatalf("digest %s: %v", table, err)
	}
	return d
}

func crCount(t *testing.T, pool *pgxpool.Pool, q string, args ...any) int {
	t.Helper()
	return countRows(t, pool, q, args...)
}

func crWant(t *testing.T, label string, got retention.Counts, want map[string]int64) {
	t.Helper()
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s: %s=%d, want %d (counts: %v)", label, k, got[k], v, got)
		}
	}
}

// crSQLState is the SQLSTATE of a pgx error ("" for none).
func crSQLState(err error) string {
	var pg *pgconn.PgError
	if errors.As(err, &pg) {
		return pg.Code
	}
	return ""
}

// crTables are the purge-target and dependent tables whose content the report-mode and "untouched" checks fingerprint.
var crTables = []string{"claims.links", "claims.bundles", "claims.lines", "claims.events", "claims.meta_intake", "integration.operations",
	"social.conversations", "social.messages", "social.comment_events", "live.claim_windows", "claims.retention_policy"}

func crDigests(t *testing.T, pool *pgxpool.Pool, tables ...string) map[string]string {
	t.Helper()
	if len(tables) == 0 {
		tables = crTables
	}
	out := map[string]string{}
	for _, tb := range tables {
		out[tb] = crDigest(t, pool, tb, "")
	}
	return out
}

func crSameDigests(t *testing.T, label string, before, after map[string]string, except ...string) {
	t.Helper()
	skip := map[string]bool{}
	for _, x := range except {
		skip[x] = true
	}
	keys := make([]string, 0, len(before))
	for k := range before {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if !skip[k] && before[k] != after[k] {
			t.Errorf("%s: %s changed (%s -> %s)", label, k, before[k], after[k])
		}
	}
}

// crLog collects (kind, counts) of retention_log rows created after mark (owner SQL).
type crLogRow struct {
	kind, by string
	counts   map[string]any
	actor    []byte
	request  *string
}

func crLogSince(t *testing.T, pool *pgxpool.Pool, since time.Time) []crLogRow {
	t.Helper()
	rows, err := pool.Query(context.Background(), `SELECT kind,executed_by,counts::text,actor_digest,request_id::text FROM claims.retention_log WHERE created_at>=$1 ORDER BY created_at,id`, since)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []crLogRow
	for rows.Next() {
		var r crLogRow
		var counts string
		if err := rows.Scan(&r.kind, &r.by, &counts, &r.actor, &r.request); err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal([]byte(counts), &r.counts); err != nil {
			t.Fatal(err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

// crNoDeadlocks reads pg_stat_database.deadlocks for the current database.
func crDeadlocks(t *testing.T, pool *pgxpool.Pool) int64 {
	t.Helper()
	var n int64
	if err := pool.QueryRow(context.Background(), `SELECT deadlocks FROM pg_stat_database WHERE datname=current_database()`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// crWaitBlocked waits (no sleeps for ordering: it polls pg_stat_activity) until backend pid waits on a lock.
func crWaitBlocked(t *testing.T, pool *pgxpool.Pool, pid int, what string) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		var waiting bool
		if err := pool.QueryRow(context.Background(), `SELECT coalesce((SELECT wait_event_type='Lock' FROM pg_stat_activity WHERE pid=$1),false)`, pid).Scan(&waiting); err != nil {
			t.Fatal(err)
		}
		if waiting {
			return
		}
		time.Sleep(10 * time.Millisecond) // polls pg_stat_activity; no ordering conclusion depends on the delay
	}
	t.Fatalf("backend %d never blocked on a lock: %s", pid, what)
}

// ---------------------------------------------------------------------------------------
// Social lane: real webhook -> inbox -> consumer function, so social rows carry real events and River jobs
// ---------------------------------------------------------------------------------------

// crSocial is one Page asset routed to a store, with the consumer login of the meta-consumer gates.
type crSocial struct {
	m        miTest
	consumer *pgxpool.Pool
	asset    string
	store    string
}

// socialLane routes a fresh synthetic Page asset to store (owner-seeded binding, registrar-activated route).
func (e *crEnv) socialLane(store string) *crSocial {
	e.t.Helper()
	m := miSetup(e.t)
	s := &crSocial{m: m, consumer: mcConsumer(e.t, m), asset: miAsset(), store: store}
	binding := miBinding(e.t, m, s.asset, "facebook", e.w.tenant, store, e.w.prin)
	miRoute(e.t, m, s.asset, e.w.tenant, store, binding)
	return s
}

// crCommentKey is the comment identity of the consumer projection (contract §0 facts: tupleHash of
// "meta-social-comment/v1", app, object, asset, comment id), computed here independently.
func crCommentKey(app, object, asset, comment string) string {
	raw, _ := json.Marshal([]string{"meta-social-comment/v1", app, object, asset, comment})
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

// terminal makes the event's River job terminal (completed), the state in which C5 may delete its social row.
func (e *crEnv) terminal(ev mcEvent) {
	e.t.Helper()
	mustExec(e.t, e.f.owner, `UPDATE river_meta.river_job SET state='completed',finalized_at=clock_timestamp() WHERE id=$1`, ev.job)
}

// message projects one Page DM of peerKey through the real consumer function (finish_social_event) and ages
// it. jobState "completed" makes the inbox job terminal; "running" leaves it in flight (the F5 shape).
func (e *crEnv) message(s *crSocial, peerKey, receivedExpr, jobState string) mcEvent {
	e.t.Helper()
	ev := mcPost(e.t, s.m, s.asset, miMessage(s.asset, "m."+randomUUID(), "retention gate dm "+t04Tag()))
	mcRunning(e.t, s.m, ev, 1)
	mcFinish(e.t, s.consumer, ev, 1, peerKey)
	e.w.events = append(e.w.events, ev.id)
	if jobState == "completed" {
		e.terminal(ev)
	}
	mustExec(e.t, e.f.owner, `UPDATE social.messages SET received_at=`+receivedExpr+` WHERE event_id=$1`, ev.id)
	mustExec(e.t, e.f.owner, `UPDATE social.conversations SET created_at=`+receivedExpr+` WHERE id=(SELECT conversation_id FROM social.messages WHERE event_id=$1)`, ev.id)
	var conv string
	if err := e.f.owner.QueryRow(context.Background(), `SELECT conversation_id::text FROM social.messages WHERE event_id=$1`, ev.id).Scan(&conv); err != nil {
		e.t.Fatal(err)
	}
	e.w.conversations = append(e.w.conversations, conv)
	return ev
}

// commentEvent projects one Page comment event (verb add|edit|remove) of commentID by sender via the real
// consumer function, with occurred/receive times distinct per call, and ages it.
func (e *crEnv) commentEvent(s *crSocial, commentID, sender, verb, receivedExpr, jobState string, seq int) mcEvent {
	e.t.Helper()
	at := time.Now().Add(-time.Duration(seq+1) * time.Second)
	extra := map[string]any{}
	if verb != "add" {
		extra["verb"] = verb
	}
	ev := mcPost(e.t, s.m, s.asset, mciFBBody(s.asset, s.asset+"_"+crDigits(10), commentID, sender, "Synthetic Name", "retention gate comment "+t04Tag(), &at, extra))
	mcRunning(e.t, s.m, ev, 1)
	if _, err := s.consumer.Exec(context.Background(), `SELECT meta_inbox.finish_social_event($1::uuid,$2::bigint,$3::integer,'comment',$4)`, ev.id, ev.job, 1, crCommentKey(miApp, "page", s.asset, commentID)); err != nil {
		e.t.Fatalf("finish comment event: %v", err)
	}
	e.w.events = append(e.w.events, ev.id)
	if jobState == "completed" {
		e.terminal(ev)
	}
	mustExec(e.t, e.f.owner, `UPDATE social.comment_events SET received_at=`+receivedExpr+` WHERE event_id=$1`, ev.id)
	return ev
}

// crClasses is the independent eligibility count of contract §1 at the default periods (owner SQL over the
// whole database; the definers are never consulted). Conversation eligibility = no messages and aged.
func crEligible(t *testing.T, pool *pgxpool.Pool, link, intake, claims, social int) map[string]int64 {
	t.Helper()
	q := map[string]string{
		"links": fmt.Sprintf(`SELECT count(*) FROM claims.links WHERE expires_at < clock_timestamp() - interval '%d days'`, link),
		"bundles": fmt.Sprintf(`SELECT count(*) FROM claims.bundles b JOIN live.claim_windows w ON (w.tenant_id,w.store_id,w.session_id)=(b.tenant_id,b.store_id,b.session_id)
			WHERE b.purged_at IS NULL AND w.state='CLOSED' AND w.closed_at < clock_timestamp() - interval '%d days'`, claims),
		"intake": fmt.Sprintf(`SELECT count(*) FROM claims.meta_intake WHERE state IN ('APPLIED','DROPPED','FAILED') AND received_at < clock_timestamp() - interval '%d days'`, intake),
		"operations": fmt.Sprintf(`SELECT count(*) FROM integration.operations WHERE action='meta.private_reply'
			AND state IN ('SUCCEEDED','FAILED_FINAL','CANCELLED','BLOCKED_POLICY','STALE_BINDING') AND created_at < clock_timestamp() - interval '%d days' AND request ? 'comment_ref'`, intake),
		"comment_events": fmt.Sprintf(`SELECT count(*) FROM social.comment_events WHERE received_at < clock_timestamp() - interval '%d days' AND meta_inbox.purgeable(event_id)`, social),
		"messages":       fmt.Sprintf(`SELECT count(*) FROM social.messages WHERE received_at < clock_timestamp() - interval '%d days' AND meta_inbox.purgeable(event_id)`, social),
		"conversations": fmt.Sprintf(`SELECT count(*) FROM social.conversations c WHERE c.created_at < clock_timestamp() - interval '%d days'
			AND NOT EXISTS (SELECT 1 FROM social.messages m WHERE m.conversation_id=c.id)`, social),
	}
	out := map[string]int64{}
	for k, sql := range q {
		var n int64
		if err := pool.QueryRow(context.Background(), sql).Scan(&n); err != nil {
			t.Fatalf("eligibility %s: %v", k, err)
		}
		out[k] = n
	}
	return out
}

// ---------------------------------------------------------------------------------------
// CRP03 report-only
// ---------------------------------------------------------------------------------------

// crSeedOnePerClass seeds exactly one purge-eligible row per class C1-C5b at the default periods (owner-seeded,
// aged), plus rows that must NOT be eligible. It returns the ids the assertions need.
type crClassSeed struct {
	recent, old    crSess
	linkBundle     crBundle
	oldBundle      crBundle
	intakeAt       crMeta
	op             string
	msg, cmt       mcEvent
	convOnly       string
	social         *crSocial
	oldLinkExpires string
}

func (e *crEnv) seedOnePerClass() *crClassSeed {
	t, w := e.t, e.w
	x := &crClassSeed{}
	x.recent = w.session(t, w.store)
	w.closedAt(t, x.recent, crAgo(1, 0))
	// C1: a link expired just past link_days on a bundle of a recently closed session.
	x.linkBundle = w.bundle(t, x.recent, "manual", "", "rg-c1-"+t04Tag())
	w.link(t, x.recent, x.linkBundle, crOld(7))
	// not eligible: a link expired just inside link_days
	keep := w.bundle(t, x.recent, "manual", "", "rg-c1-keep-"+t04Tag())
	w.link(t, x.recent, keep, crNew(7))
	// C2: a session whose window closed just past claims_days: manual bundle bound to a buyer, with a line, an event, a live link.
	x.old = w.session(t, w.store)
	w.closedAt(t, x.old, crOld(90))
	x.oldBundle = w.bundle(t, x.old, "manual", "", "rg-c2-"+t04Tag())
	w.bind(t, x.old, x.oldBundle)
	offer, sku := w.offer(t, x.old)
	w.line(t, x.old, x.oldBundle, offer, sku, 2)
	w.link(t, x.old, x.oldBundle, `(clock_timestamp() + interval '1 hour')`)
	// C3 intake and C4 operation on the recent session's Meta lane.
	x.intakeAt = w.source(t, x.recent, "page")
	w.intake(t, x.intakeAt, x.intakeAt.asset+"_"+crDigits(9), crHex64(), "APPLIED", crOld(30), "")
	w.intake(t, x.intakeAt, x.intakeAt.asset+"_"+crDigits(9), crHex64(), "APPLIED", crNew(30), "") // just inside: kept
	x.op = w.operation(t, x.intakeAt, x.linkBundle, x.intakeAt.asset+"_"+crDigits(9), "SUCCEEDED", crOld(30))
	// C5: one DM (its conversation keeps a message, so only the message counts) and one comment event, both terminal and aged;
	// C5b: one empty conversation created just past social_days.
	x.social = e.socialLane(w.store)
	x.msg = e.message(x.social, crHex64(), crOld(30), "completed")
	x.cmt = e.commentEvent(x.social, x.social.asset+"_"+crDigits(10), crDigits(9), "add", crOld(30), "completed", 0)
	x.convOnly = randomUUID()
	mustExec(t, w.owner, `INSERT INTO social.conversations(id,tenant_id,store_id,app_id,object,asset_id,peer_key,created_at) VALUES($1,$2,$3,$4,'page',$5,$6,`+crOld(30)+`)`,
		x.convOnly, w.tenant, w.store, miApp, x.social.asset, crHex64())
	w.conversations = append(w.conversations, x.convOnly)
	return x
}

// CRP03 + RD2 + §3 run_retention: with enforced=false every class reports its eligible rows and NOTHING is written
// but one `run` log row. Expected counts come from an independent SQL reading of contract §1; every purge-target
// table (and the append-only facts) has an identical fingerprint before and after; the run row holds numbers only.
func TestClaimsRetentionCRP03ReportOnly(t *testing.T) {
	e := crSetup(t)
	ctx := context.Background()
	x := e.seedOnePerClass()
	// C6: an aged `run` row (must survive a report-only run: C6 is a DELETE, a write).
	var oldRun string
	if err := e.f.owner.QueryRow(ctx, `INSERT INTO claims.retention_log(kind,counts,created_at) VALUES('run','{"links":0}',`+crOld(400)+`) RETURNING id::text`).Scan(&oldRun); err != nil {
		t.Fatal(err)
	}
	e.w.logs = append(e.w.logs, oldRun)

	want := crEligible(t, e.f.owner, 7, 30, 90, 30)
	for _, class := range []string{"links", "bundles", "intake", "operations", "comment_events", "messages", "conversations"} {
		if want[class] != 1 {
			t.Fatalf("independent eligibility of %s = %d, want exactly 1 (seed or cross-test pollution)", class, want[class])
		}
	}
	before := crDigests(t, e.f.owner)
	beforeMeta := crDigest(t, e.f.owner, "meta_inbox.events", "")
	mark := time.Now().Add(-time.Second)
	// owner-side clock skew is nil (same server); the log filter uses the DB clock.
	var dbNow time.Time
	if err := e.f.owner.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&dbNow); err != nil {
		t.Fatal(err)
	}
	mark = dbNow
	e.markSeed()
	c := e.run(500)
	crWant(t, "report-only run", c, map[string]int64{"enforced": 0, "links": 1, "bundles": 1, "intake": 1, "operations": 1,
		"comment_events": 1, "messages": 1, "conversations": 1, "more": 0})
	after := crDigests(t, e.f.owner)
	crSameDigests(t, "report-only run", before, after)
	if got := crDigest(t, e.f.owner, "meta_inbox.events", ""); got != beforeMeta {
		t.Error("report-only run changed meta_inbox.events")
	}
	if n := crCount(t, e.f.owner, `SELECT count(*) FROM claims.retention_log WHERE id=$1`, oldRun); n != 1 {
		t.Error("report-only run deleted an aged run row (C6 is a write)")
	}
	rows := crLogSince(t, e.f.owner, mark)
	if len(rows) != 1 || rows[0].kind != "run" {
		t.Fatalf("report-only run wrote %d log rows %+v, want exactly one `run` row", len(rows), rows)
	}
	var jobLogin string
	if err := e.job.QueryRow(ctx, `SELECT session_user`).Scan(&jobLogin); err != nil {
		t.Fatal(err)
	}
	if rows[0].by != jobLogin {
		t.Errorf("run row executed_by=%q, want the retention job login %q", rows[0].by, jobLogin)
	}
	for _, k := range []string{"enforced", "links", "bundles", "intake", "operations", "comment_events", "messages", "conversations", "more"} {
		v, ok := rows[0].counts[k]
		if _, isNum := v.(float64); !ok || !isNum {
			t.Errorf("run row counts[%s]=%v (%T), want a number", k, v, v)
		}
	}
	if rows[0].counts["links"] != float64(1) || rows[0].counts["enforced"] != float64(0) {
		t.Errorf("run row counts %v disagree with the returned counts", rows[0].counts)
	}
	// The purge targets are all still there.
	if crCount(t, e.f.owner, `SELECT count(*) FROM claims.bundles WHERE id=$1 AND purged_at IS NULL AND actor_key IS NOT NULL AND owner_id IS NOT NULL`, x.oldBundle.id) != 1 {
		t.Error("report-only run de-identified a bundle")
	}
	// retention_status reflects the run (job login may call it: EXECUTE retention_status only).
	st, err := retention.GetStatus(ctx, e.job)
	if err != nil {
		t.Fatal(err)
	}
	if st.Enforced || st.LastRunMore || time.Since(time.Unix(st.LastRunUnix, 0)) > time.Minute || st.LastRunUnix <= 0 {
		t.Errorf("status after a report-only run: %+v", st)
	}
	// The cap: two eligible links with p_limit=1 count one (still nothing written). r2-close-retention: report-only
	// never says more=1 (nothing is removed, so the backlog cannot shrink; more=1 would make the job run 20 batches an
	// hour and trip the runbook's last_run_more escalation falsely) and writes exactly one run row.
	extra := e.w.bundle(t, x.recent, "manual", "", "rg-c1-extra-"+t04Tag())
	e.w.link(t, x.recent, extra, crOld(7))
	before = crDigests(t, e.f.owner)
	var markCap time.Time
	if err := e.f.owner.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&markCap); err != nil {
		t.Fatal(err)
	}
	capped := e.run(1)
	crWant(t, "report-only capped run", capped, map[string]int64{"enforced": 0, "links": 1, "more": 0})
	crSameDigests(t, "capped report-only run", before, crDigests(t, e.f.owner))
	if rows := crLogSince(t, e.f.owner, markCap); len(rows) != 1 || rows[0].counts["more"] != float64(0) {
		t.Errorf("capped report-only run log rows %+v, want exactly one run row with more=0", rows)
	}
	if st, err := retention.GetStatus(ctx, e.job); err != nil || st.LastRunMore {
		t.Errorf("status after a capped report-only run: %+v %v, want last_run_more=false", st, err)
	}
	// The operator may run the same batch (EXECUTE run_retention) and is recorded under its own login.
	mark2 := dbNow
	if err := e.f.owner.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&mark2); err != nil {
		t.Fatal(err)
	}
	oc, err := retention.RunOnce(ctx, e.op, 500)
	if err != nil || oc["enforced"] != 0 || oc["links"] != 2 {
		t.Errorf("operator report-only run: %v %v", oc, err)
	}
	var opLogin string
	if err := e.op.QueryRow(ctx, `SELECT session_user`).Scan(&opLogin); err != nil {
		t.Fatal(err)
	}
	if rows := crLogSince(t, e.f.owner, mark2); len(rows) != 1 || rows[0].by != opLogin {
		t.Errorf("operator run log rows: %+v (want one row by %s)", rows, opLogin)
	}
}

// ---------------------------------------------------------------------------------------
// CRP04 enforced purge
// ---------------------------------------------------------------------------------------

var (
	crPurgedLabel = `^purged-[0-9a-f]{32}$`
	crHex64Re     = `^[0-9a-f]{64}$`
)

// crSub runs one CRP04/CRP05 scenario with the seeds of the previous scenario removed and the policy at the
// enforced defaults, and removes its own seeds afterwards (kept rows sit crMargin from a threshold and would
// become eligible for the next scenario otherwise).
func (e *crEnv) sub(t *testing.T, name string, fn func(t *testing.T)) {
	t.Run(name, func(t *testing.T) {
		e.t = t
		e.w.cleanup()
		e.enforceDefaults()
		defer func() {
			e.releaseHolds() // an open blocker would make the seed cleanup wait on its own row locks
			e.w.cleanup()
			e.resetPolicy()
		}()
		fn(t)
	})
}

type crBundleRow struct {
	actor, label          string
	owner, bound, purged  *string
	version, lineCount    int64
	platform, session     string
	createdAt, purgedTime *time.Time
}

func (e *crEnv) bundleRow(t *testing.T, id string) (r crBundleRow) {
	t.Helper()
	err := e.f.owner.QueryRow(context.Background(), `SELECT actor_key,coalesce(label,''),owner_id::text,bound_at::text,purged_at::text,version,line_count,platform,session_id::text,created_at,purged_at
		FROM claims.bundles WHERE id=$1`, id).Scan(&r.actor, &r.label, &r.owner, &r.bound, &r.purged, &r.version, &r.lineCount, &r.platform, &r.session, &r.createdAt, &r.purgedTime)
	if err != nil {
		t.Fatalf("read bundle %s: %v", id, err)
	}
	return r
}

// CRP04 + §1 C1-C6 + RD1/RD8: each retention class at threshold -/+ margin, untouched sets, token and link
// consequences, second run no-op, more=1 at p_limit. Every subtest is owner-seeded except "buyer token / issue_link",
// which uses a real manual claim, a real issued link token and the real merchant HTTP handler.
func TestClaimsRetentionCRP04EnforcedPurge(t *testing.T) {
	e := crSetup(t)
	ctx := context.Background()
	w := e.w

	e.sub(t, "C1-links", func(t *testing.T) {
		s := w.session(t, w.store)
		w.closedAt(t, s, crAgo(1, 0))
		oldB, newB := w.bundle(t, s, "manual", "", "c1-old-"+t04Tag()), w.bundle(t, s, "manual", "", "c1-new-"+t04Tag())
		w.link(t, s, oldB, crOld(7))
		w.link(t, s, newB, crNew(7))
		bundlesBefore := crDigest(t, w.owner, "claims.bundles", "session_id=$1", s.id)
		e.markSeed()
		c := e.run(500)
		crWant(t, "C1", c, map[string]int64{"enforced": 1, "links": 1, "bundles": 0, "intake": 0, "operations": 0, "comment_events": 0, "messages": 0, "conversations": 0, "more": 0})
		if crCount(t, w.owner, `SELECT count(*) FROM claims.links WHERE bundle_id=$1`, oldB.id) != 0 || crCount(t, w.owner, `SELECT count(*) FROM claims.links WHERE bundle_id=$1`, newB.id) != 1 {
			t.Error("expired link not deleted or unexpired link deleted")
		}
		if crDigest(t, w.owner, "claims.bundles", "session_id=$1", s.id) != bundlesBefore {
			t.Error("C1 changed a bundle")
		}
		again := e.run(500)
		crWant(t, "C1 second run", again, map[string]int64{"links": 0, "bundles": 0, "more": 0})
	})

	e.sub(t, "C2-claim-identity", func(t *testing.T) {
		old := w.session(t, w.store)
		w.closedAt(t, old, crOld(90))
		mFB := w.source(t, old, "page")
		man1 := w.bundle(t, old, "manual", "", "c2-a-"+t04Tag())
		man2 := w.bundle(t, old, "manual", "", "purged-1a2b3c4d") // merchant label outside the reserved pattern (F1): must not block C2
		meta1 := w.bundle(t, old, "facebook", crHex64(), "")
		owner1 := w.bind(t, old, man1)
		w.bind(t, old, meta1)
		offer, sku := w.offer(t, old)
		w.line(t, old, man1, offer, sku, 3)
		w.link(t, old, man1, `(clock_timestamp() + interval '1 hour')`)
		w.link(t, old, meta1, `(clock_timestamp() + interval '1 hour')`)
		_ = mFB
		// sessions that must stay: closed just inside claims_days, recently closed, OPEN, reopened after a long close.
		keep := map[string]crBundle{}
		mk := func(name string, s crSess) {
			keep[name] = w.bundle(t, s, "manual", "", "keep-"+name+"-"+t04Tag())
			w.bind(t, s, keep[name])
		}
		just := w.session(t, w.store)
		w.closedAt(t, just, crNew(90))
		mk("just-inside", just)
		recent := w.session(t, w.store)
		w.closedAt(t, recent, crAgo(1, 0))
		mk("recent", recent)
		reopened := w.session(t, w.store)
		w.closedAt(t, reopened, crAgo(100, 0))
		w.reopen(t, reopened)
		mk("reopened", reopened)
		open := w.session(t, w.store2)
		w.reopen(t, open)
		mk("open", open)
		neverOpened := w.session(t, w.store) // CLOSED generation 0: never opened, closed_at NULL
		mk("never-opened", neverOpened)

		eventsBefore := crDigest(t, w.owner, "claims.events", "session_id=$1", old.id)
		linesBefore := crDigest(t, w.owner, "claims.lines", "session_id=$1", old.id) // applied_version changes, so compare the parts below
		var qtyBefore string
		if err := w.owner.QueryRow(ctx, `SELECT string_agg(quantity::text||'/'||version||'/'||offer_id||'/'||sku_id,',' ORDER BY offer_id) FROM claims.lines WHERE session_id=$1`, old.id).Scan(&qtyBefore); err != nil {
			t.Fatal(err)
		}
		keepBefore := map[string]crBundleRow{}
		keepDigest := map[string]string{}
		for name, b := range keep {
			keepBefore[name] = e.bundleRow(t, b.id)
			keepDigest[name] = crDigest(t, w.owner, "claims.bundles", "id=$1", b.id)
		}
		before := map[string]crBundleRow{}
		for _, b := range []crBundle{man1, man2, meta1} {
			before[b.id] = e.bundleRow(t, b.id)
		}
		e.markSeed()
		c := e.run(500)
		crWant(t, "C2", c, map[string]int64{"enforced": 1, "bundles": 3, "links": 0, "more": 0})
		seenActor, seenLabel := map[string]bool{}, map[string]bool{}
		for _, b := range []crBundle{man1, man2, meta1} {
			r, was := e.bundleRow(t, b.id), before[b.id]
			if r.purged == nil || r.owner != nil || r.bound != nil {
				t.Errorf("bundle %s (%s): purged_at=%v owner_id=%v bound_at=%v", b.id, b.platform, r.purged, r.owner, r.bound)
			}
			if !regexpMatch(crHex64Re, r.actor) || r.actor == was.actor || seenActor[r.actor] {
				t.Errorf("bundle %s actor_key not a fresh random 64-hex: changed=%t", b.id, r.actor != was.actor)
			}
			seenActor[r.actor] = true
			if b.platform == "manual" {
				plain := "purged-" + strings.ReplaceAll(b.id, "-", "")
				if !regexpMatch(crPurgedLabel, r.label) || r.label == plain || r.label == was.label || seenLabel[r.label] {
					t.Errorf("manual bundle %s label %q: not a random purged-<32 hex> (derived-from-id=%t)", b.id, r.label, r.label == plain)
				}
				seenLabel[r.label] = true
			} else if r.label != "" {
				t.Errorf("meta bundle %s got a label %q", b.id, r.label)
			}
			if r.session != was.session || r.platform != was.platform || r.version != was.version || r.lineCount != was.lineCount || !r.createdAt.Equal(*was.createdAt) {
				t.Errorf("bundle %s: a non-identity column changed: %+v -> %+v", b.id, was, r)
			}
		}
		if crCount(t, w.owner, `SELECT count(*) FROM claims.links WHERE bundle_id=ANY($1::uuid[])`, []string{man1.id, man2.id, meta1.id}) != 0 {
			t.Error("links of purged bundles survive")
		}
		var applied *int64
		if err := w.owner.QueryRow(ctx, `SELECT applied_version FROM claims.lines WHERE bundle_id=$1`, man1.id).Scan(&applied); err != nil || applied != nil {
			t.Errorf("lines.applied_version after C2 = %v (%v), want NULL", applied, err)
		}
		var qtyAfter string
		if err := w.owner.QueryRow(ctx, `SELECT string_agg(quantity::text||'/'||version||'/'||offer_id||'/'||sku_id,',' ORDER BY offer_id) FROM claims.lines WHERE session_id=$1`, old.id).Scan(&qtyAfter); err != nil || qtyAfter != qtyBefore {
			t.Errorf("lines quantity/version/offer/sku changed: %q -> %q (%v)", qtyBefore, qtyAfter, err)
		}
		_ = linesBefore
		if crDigest(t, w.owner, "claims.events", "session_id=$1", old.id) != eventsBefore {
			t.Error("claims.events changed")
		}
		if crCount(t, w.owner, `SELECT count(*) FROM buyer.owners WHERE id=$1`, owner1) != 1 {
			t.Error("C2 deleted the buyer owner row (only the binding is cleared)")
		}
		for name, b := range keep {
			if crDigest(t, w.owner, "claims.bundles", "id=$1", b.id) != keepDigest[name] {
				t.Errorf("bundle of the %s session was touched (must stay: %+v)", name, keepBefore[name].purged)
			}
			if r := e.bundleRow(t, b.id); r.purged != nil || r.owner == nil {
				t.Errorf("%s: purged=%v owner=%v", name, r.purged, r.owner)
			}
		}
		afterDigest := crDigest(t, w.owner, "claims.bundles", "id=ANY($1::uuid[])", []string{man1.id, man2.id, meta1.id})
		second := e.run(500)
		crWant(t, "C2 second run", second, map[string]int64{"bundles": 0, "links": 0, "more": 0})
		if crDigest(t, w.owner, "claims.bundles", "id=ANY($1::uuid[])", []string{man1.id, man2.id, meta1.id}) != afterDigest {
			t.Error("the second run changed purged bundles (a purged bundle must never be re-randomised)")
		}
	})

	e.sub(t, "C2-more-at-limit", func(t *testing.T) {
		old := w.session(t, w.store)
		w.closedAt(t, old, crOld(90))
		ids := []string{}
		for i := 0; i < 3; i++ {
			ids = append(ids, w.bundle(t, old, "manual", "", fmt.Sprintf("more-%d-%s", i, t04Tag())).id)
		}
		e.markSeed()
		first := e.run(2)
		crWant(t, "limit 2 first", first, map[string]int64{"bundles": 2, "more": 1})
		second := e.run(2)
		crWant(t, "limit 2 second", second, map[string]int64{"bundles": 1, "more": 0})
		if n := crCount(t, w.owner, `SELECT count(*) FROM claims.bundles WHERE id=ANY($1::uuid[]) AND purged_at IS NOT NULL`, ids); n != 3 {
			t.Errorf("purged bundles after two batches: %d, want 3", n)
		}
		third := e.run(2)
		crWant(t, "limit 2 third", third, map[string]int64{"bundles": 0, "more": 0})
	})

	e.sub(t, "C3-intake", func(t *testing.T) {
		s := w.session(t, w.store)
		w.closedAt(t, s, crAgo(1, 0))
		m := w.source(t, s, "page")
		ref := func() string { return m.asset + "_" + crDigits(9) }
		var oldIDs, keepIDs, pending []string
		for _, st := range []string{"APPLIED", "DROPPED", "FAILED"} {
			r := ref()
			w.intake(t, m, r, crHex64(), st, crOld(30), "")
			oldIDs = append(oldIDs, r)
			r2 := ref()
			w.intake(t, m, r2, crHex64(), st, crNew(30), "")
			keepIDs = append(keepIDs, r2)
		}
		rp := ref()
		w.intake(t, m, rp, crHex64(), "PENDING", crAgo(100, 0), "")
		pending = append(pending, rp)
		before := crDigest(t, w.owner, "claims.meta_intake", "comment_ref=ANY($1::text[])", append(append([]string{}, keepIDs...), pending...))
		e.markSeed()
		c := e.run(500)
		crWant(t, "C3", c, map[string]int64{"enforced": 1, "intake": 3, "links": 0, "bundles": 0, "more": 0})
		if crCount(t, w.owner, `SELECT count(*) FROM claims.meta_intake WHERE comment_ref=ANY($1::text[])`, oldIDs) != 0 {
			t.Error("aged terminal intake rows survive")
		}
		if crDigest(t, w.owner, "claims.meta_intake", "comment_ref=ANY($1::text[])", append(append([]string{}, keepIDs...), pending...)) != before {
			t.Error("kept or PENDING intake rows changed")
		}
		crWant(t, "C3 second", e.run(500), map[string]int64{"intake": 0})
	})

	e.sub(t, "C4-reply-ledger", func(t *testing.T) {
		s := w.session(t, w.store)
		w.closedAt(t, s, crAgo(1, 0))
		m := w.source(t, s, "page")
		b := w.bundle(t, s, "manual", "", "c4-"+t04Tag())
		ref := func() string { return m.asset + "_" + crDigits(9) }
		terminal := []string{"SUCCEEDED", "FAILED_FINAL", "CANCELLED", "BLOCKED_POLICY", "STALE_BINDING"}
		type opSeed struct{ id, ref, state string }
		var redact, keepNew, keepOpen []opSeed
		for _, st := range terminal {
			r := ref()
			redact = append(redact, opSeed{w.operation(t, m, b, r, st, crOld(30)), r, st})
			r2 := ref()
			keepNew = append(keepNew, opSeed{w.operation(t, m, b, r2, st, crNew(30)), r2, st})
		}
		for _, st := range []string{"READY", "DISPATCHING", "UNKNOWN", "ACKNOWLEDGED"} {
			r := ref()
			keepOpen = append(keepOpen, opSeed{w.operation(t, m, b, r, st, crAgo(120, 0)), r, st})
		}
		type opRow struct {
			key, state string
			hash       []byte
			req        map[string]any
		}
		read := func(id string) (o opRow) {
			var raw string
			if err := w.owner.QueryRow(ctx, `SELECT semantic_key,state,request_hash,request::text FROM integration.operations WHERE id=$1`, id).Scan(&o.key, &o.state, &o.hash, &raw); err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal([]byte(raw), &o.req); err != nil {
				t.Fatal(err)
			}
			return o
		}
		before := map[string]opRow{}
		for _, list := range [][]opSeed{redact, keepNew, keepOpen} {
			for _, o := range list {
				before[o.id] = read(o.id)
			}
		}
		e.markSeed()
		c := e.run(500)
		crWant(t, "C4", c, map[string]int64{"enforced": 1, "operations": 5, "intake": 0, "links": 0, "more": 0})
		for _, o := range redact {
			after, was := read(o.id), before[o.id]
			if _, has := after.req["comment_ref"]; has || after.req["redacted"] != true {
				t.Errorf("%s op: request %v not redacted (comment_ref must go, redacted:true)", o.state, after.req)
			}
			for k, v := range was.req {
				if k != "comment_ref" && fmt.Sprint(after.req[k]) != fmt.Sprint(v) {
					t.Errorf("%s op: request key %q changed %v -> %v", o.state, k, v, after.req[k])
				}
			}
			if len(after.req) != len(was.req) { // comment_ref removed, redacted added
				t.Errorf("%s op: request keys %d -> %d", o.state, len(was.req), len(after.req))
			}
			if after.key != "mpr-purged:"+o.id || string(after.hash) != string(was.hash) || after.state != o.state {
				t.Errorf("%s op: semantic_key=%q (want mpr-purged:<id>) hash kept=%t state=%s", o.state, after.key, string(after.hash) == string(was.hash), after.state)
			}
		}
		for _, list := range [][]opSeed{keepNew, keepOpen} {
			for _, o := range list {
				after := read(o.id)
				if fmt.Sprint(after) != fmt.Sprint(before[o.id]) || after.req["comment_ref"] != o.ref {
					t.Errorf("%s op (%s) was touched", o.state, o.id)
				}
			}
		}
		crWant(t, "C4 second", e.run(500), map[string]int64{"operations": 0})
	})

	e.sub(t, "C5-social", func(t *testing.T) {
		lane := e.socialLane(w.store)
		peerOld, peerNew, peerHeld := crHex64(), crHex64(), crHex64()
		oldMsg := e.message(lane, peerOld, crOld(30), "completed")
		newMsg := e.message(lane, peerNew, crNew(30), "completed")
		heldMsg := e.message(lane, peerHeld, crOld(30), "running") // inbox job still in flight: kept (F5)
		commentOld := lane.asset + "_" + crDigits(10)
		oldCmt := e.commentEvent(lane, commentOld, crDigits(9), "add", crOld(30), "completed", 1)
		newCmt := e.commentEvent(lane, lane.asset+"_"+crDigits(10), crDigits(9), "add", crNew(30), "completed", 2)
		heldCmt := e.commentEvent(lane, lane.asset+"_"+crDigits(10), crDigits(9), "add", crOld(30), "running", 3)
		emptyOld, emptyNew := randomUUID(), randomUUID()
		for id, expr := range map[string]string{emptyOld: crOld(30), emptyNew: crNew(30)} {
			mustExec(t, w.owner, `INSERT INTO social.conversations(id,tenant_id,store_id,app_id,object,asset_id,peer_key,created_at) VALUES($1,$2,$3,$4,'page',$5,$6,`+expr+`)`, id, w.tenant, w.store, miApp, lane.asset, crHex64())
			w.conversations = append(w.conversations, id)
		}
		var oldConv, newConv, heldConv string
		for ev, dst := range map[string]*string{oldMsg.id: &oldConv, newMsg.id: &newConv, heldMsg.id: &heldConv} {
			if err := w.owner.QueryRow(ctx, `SELECT conversation_id::text FROM social.messages WHERE event_id=$1`, ev).Scan(dst); err != nil {
				t.Fatal(err)
			}
		}
		keepDigest := crDigest(t, w.owner, "social.messages", "event_id=ANY($1::uuid[])", []string{newMsg.id, heldMsg.id}) +
			crDigest(t, w.owner, "social.comment_events", "event_id=ANY($1::uuid[])", []string{newCmt.id, heldCmt.id}) +
			crDigest(t, w.owner, "social.conversations", "id=ANY($1::uuid[])", []string{newConv, heldConv, emptyNew})
		e.markSeed()
		c := e.run(500)
		// oldConv loses its only message in C5 and is removed by C5b in the same run (class order C5 -> C5b).
		crWant(t, "C5", c, map[string]int64{"enforced": 1, "comment_events": 1, "messages": 1, "conversations": 2, "more": 0})
		for name, q := range map[string]struct {
			table, key, id string
			gone           bool
		}{
			"old message":      {"social.messages", "event_id", oldMsg.id, true},
			"old comment":      {"social.comment_events", "event_id", oldCmt.id, true},
			"old conversation": {"social.conversations", "id", oldConv, true},
			"empty old conv":   {"social.conversations", "id", emptyOld, true},
			"new message":      {"social.messages", "event_id", newMsg.id, false},
			"held message":     {"social.messages", "event_id", heldMsg.id, false},
			"new comment":      {"social.comment_events", "event_id", newCmt.id, false},
			"held comment":     {"social.comment_events", "event_id", heldCmt.id, false},
			"new conv":         {"social.conversations", "id", newConv, false},
			"held conv":        {"social.conversations", "id", heldConv, false},
			"empty new conv":   {"social.conversations", "id", emptyNew, false},
		} {
			n := crCount(t, w.owner, `SELECT count(*) FROM `+q.table+` WHERE `+q.key+`=$1`, q.id)
			if (n == 0) != q.gone {
				t.Errorf("%s: rows=%d, want gone=%t", name, n, q.gone)
			}
		}
		if got := crDigest(t, w.owner, "social.messages", "event_id=ANY($1::uuid[])", []string{newMsg.id, heldMsg.id}) +
			crDigest(t, w.owner, "social.comment_events", "event_id=ANY($1::uuid[])", []string{newCmt.id, heldCmt.id}) +
			crDigest(t, w.owner, "social.conversations", "id=ANY($1::uuid[])", []string{newConv, heldConv, emptyNew}); got != keepDigest {
			t.Error("kept social rows changed")
		}
		crWant(t, "C5 second", e.run(500), map[string]int64{"comment_events": 0, "messages": 0, "conversations": 0})
	})

	e.sub(t, "C6-run-log", func(t *testing.T) {
		ins := func(kind, expr string) string {
			var id string
			q := `INSERT INTO claims.retention_log(kind,counts,created_at) VALUES('run','{"links":0}',` + expr + `) RETURNING id::text`
			switch kind {
			case "policy_set":
				q = `INSERT INTO claims.retention_log(kind,counts,created_at) VALUES('policy_set','{"enforced":0}',` + expr + `) RETURNING id::text`
			case "actor_erased":
				q = `INSERT INTO claims.retention_log(kind,request_id,selector_digest,actor_digest,counts,created_at) VALUES('actor_erased',gen_random_uuid(),sha256('a'),sha256('b'),'{"bundles":1}',` + expr + `) RETURNING id::text`
			case "replay":
				q = `INSERT INTO claims.retention_log(kind,counts,created_at) VALUES('replay','{"tombstones":1}',` + expr + `) RETURNING id::text`
			}
			if err := w.owner.QueryRow(ctx, q).Scan(&id); err != nil {
				t.Fatalf("seed %s log row: %v", kind, err)
			}
			w.logs = append(w.logs, id)
			return id
		}
		oldRun, newRun := ins("run", crOld(400)), ins("run", crNew(400))
		keepers := []string{ins("policy_set", crAgo(500, 0)), ins("actor_erased", crAgo(500, 0)), ins("replay", crAgo(500, 0))}
		e.markSeed()
		c := e.run(500)
		if c["enforced"] != 1 {
			t.Errorf("C6 run counts %v", c)
		}
		if crCount(t, w.owner, `SELECT count(*) FROM claims.retention_log WHERE id=$1`, oldRun) != 0 {
			t.Error("run row older than 400 days survives")
		}
		if crCount(t, w.owner, `SELECT count(*) FROM claims.retention_log WHERE id=ANY($1::uuid[])`, append([]string{newRun}, keepers...)) != 4 {
			t.Error("a run row inside 400 days, or a policy_set/actor_erased/replay row, was deleted (only kind=run is C6)")
		}
	})

	e.sub(t, "meta-actor-new-session-gets-a-new-bundle", func(t *testing.T) {
		// Real webhook -> consumer -> intake -> apply. After C2 de-identified the actor's bundle, the same sender commenting in a
		// new session gets a NEW bundle whose (deterministic) key is not the purged bundle's key.
		m := mciSetup(t, mciOpts{})
		f := m.h.f
		sender := mciDigits(15)
		m.postFB(t, "", sender, "A1", mciAt(3*time.Second), nil)
		if n := m.apply(t); n != 1 {
			t.Fatalf("apply processed %d rows", n)
		}
		key := meta.ClaimActorKey(m.actor, "page", m.pageAsset, sender)
		first := m.bundleOfActor(t, key)
		m.h.closeWindow(t, m.session)
		mustExec(t, f.owner, `UPDATE live.claim_windows SET opened_at=`+crOld(90)+`-interval '1 hour',closed_at=`+crOld(90)+` WHERE session_id=$1`, m.session)
		c := e.run(500)
		if c["bundles"] < 1 {
			t.Fatalf("the real bundle was not purged: %v", c)
		}
		purged := e.bundleRow(t, first)
		if purged.purged == nil || purged.actor == key || purged.owner != nil {
			t.Fatalf("purged real bundle: purged=%v key kept=%t", purged.purged, purged.actor == key)
		}
		s2 := m.h.draft(t, f.storeA1)
		m.h.open(t, s2, claims.MatchExact)
		m.h.offer(t, s2, "A1", m.sku, 5)
		post2 := m.pageAsset + "_" + mciDigits(10)
		if _, err := m.putSource(s2, "page", m.pageAsset, post2, false, "zh-TW", true, 0); err != nil {
			t.Fatalf("claim source for the new session: %v", err)
		}
		t.Cleanup(func() { // runs before the shared session purge (LIFO): intake -> sources -> windows
			_, _ = f.owner.Exec(ctx, `DELETE FROM claims.meta_intake WHERE session_id=$1`, s2)
			_, _ = f.owner.Exec(ctx, `DELETE FROM live.claim_sources WHERE session_id=$1`, s2)
		})
		m.postFBTo(t, post2, "", sender, "A1", mciAt(3*time.Second), nil, true)
		if n := m.apply(t); n != 1 {
			t.Fatalf("apply of the new session's comment processed %d rows", n)
		}
		var second, secondKey string
		if err := f.owner.QueryRow(ctx, `SELECT id::text,actor_key FROM claims.bundles WHERE session_id=$1 AND platform='facebook'`, s2).Scan(&second, &secondKey); err != nil {
			t.Fatalf("no bundle in the new session: %v", err)
		}
		if second == first || secondKey != key || secondKey == purged.actor {
			t.Errorf("new-session bundle: same id as the purged one=%t, key is the deterministic actor key=%t, equals the purged key=%t", second == first, secondKey == key, secondKey == purged.actor)
		}
		if again := e.bundleRow(t, first); again.purged == nil || again.actor != purged.actor {
			t.Error("the purged bundle changed when the actor came back")
		}
		m.h.closeWindow(t, s2)
	})

	e.sub(t, "buyer-token-and-issue-link", func(t *testing.T) {
		// Real manual claim + real link token + real buyer capability + real merchant HTTP handler (not owner-seeded except aging).
		h := lcSetup(t)
		f := h.f
		session := h.draft(t, f.storeA1)
		h.open(t, session, "EXACT")
		h.offer(t, session, "A1", h.stock.skus[0].ID, 5)
		claim := h.accepted(t, session, "", "retention-real", "A1")
		issued := h.link(t, session, claim.BundleID, 0, false)
		owner := mustIssue(t, h.service, f.storeA1)
		pv, err := h.preview(owner, issued.Token)
		if err != nil {
			t.Fatalf("preview of a live token before the purge: %v", err)
		}
		if _, err := h.redeem(owner, t04Key("crp04-redeem"), issued.Token, pv.BundleVersion); err != nil {
			t.Fatalf("redeem before the purge: %v", err)
		}
		if lcOwner(t, f, claim.BundleID) == "" {
			t.Fatal("redeem did not bind the bundle")
		}
		h.closeWindow(t, session)
		// owner-seeded aging: the window closed just past claims_days ago (the merchant API cannot back-date it)
		mustExec(t, f.owner, `UPDATE live.claim_windows SET opened_at=`+crOld(90)+`-interval '1 hour',closed_at=`+crOld(90)+` WHERE session_id=$1`, session)
		e.markSeed()
		c := e.run(500)
		if c["bundles"] < 1 {
			t.Fatalf("real bundle not purged: %v", c)
		}
		if crCount(t, f.owner, `SELECT count(*) FROM claims.links WHERE bundle_id=$1`, claim.BundleID) != 0 || lcOwner(t, f, claim.BundleID) != "" {
			t.Error("link or binding of the purged bundle survives")
		}
		if _, err := h.preview(owner, issued.Token); !errors.Is(err, command.ErrNotFound) {
			t.Errorf("old token preview after purge: %v, want not found", err)
		}
		other := mustIssue(t, h.service, f.storeA1)
		if _, err := h.redeem(other, t04Key("crp04-redeem"), issued.Token, pv.BundleVersion); !errors.Is(err, command.ErrNotFound) {
			t.Errorf("old token redeem after purge: %v, want not found", err)
		}
		// issue_link on the purged bundle: RD8 trigger PT404 at SQL level ...
		_, sqlErr := f.owner.Exec(ctx, `INSERT INTO claims.links(tenant_id,store_id,bundle_id,token_hash,generation,issued_at,expires_at,principal_id)
			VALUES($1,$2,$3,sha256('x'),1,clock_timestamp(),clock_timestamp()+interval '1 hour',$4)`, f.tenantA, f.storeA1, claim.BundleID, f.principalA)
		if crSQLState(sqlErr) != "PT404" {
			t.Errorf("link INSERT for a purged bundle: %v, want SQLSTATE PT404", sqlErr)
		}
		// ... through the merchant API function (mapped, not a raw PostgreSQL error) ...
		_, apiErr := h.issue(h.token, f.storeA1, t04Key("crp04-link"), session, claim.BundleID, claims.LinkInput{ExpectedGeneration: 0})
		if apiErr == nil || crSQLState(apiErr) != "" {
			t.Errorf("IssueLink on a purged bundle: %v, want a mapped not-found error", apiErr)
		}
		// ... and through the real HTTP handler: 404.
		handler := httpapi.NewHandler(f.runtime, httpapi.Options{ClaimLabels: &h.labels})
		path := "/v1/admin/stores/" + f.storeA1 + "/live-sessions/" + session + "/claims/bundles/" + claim.BundleID + "/link"
		resp := adminRequest(handler, "POST", path, h.token, []byte(`{"expected_generation":0,"release_binding":false}`), "application/json", map[string]string{"Idempotency-Key": t04Key("crp04-http")})
		if resp.Code != 404 {
			t.Errorf("POST issue-link for a purged bundle: HTTP %d %s, want 404", resp.Code, resp.Body.String())
		}
		if strings.Contains(resp.Body.String(), "PT404") || strings.Contains(resp.Body.String(), "SQLSTATE") {
			t.Error("HTTP 404 body echoes a database error")
		}
		// the merchant sees the purged bundle with a purged- label and no link (contract §9)
		var label string
		if err := f.owner.QueryRow(ctx, `SELECT label FROM claims.bundles WHERE id=$1`, claim.BundleID).Scan(&label); err != nil || !regexpMatch(crPurgedLabel, label) {
			t.Errorf("purged manual bundle label %q (%v)", label, err)
		}
	})
}

func regexpMatch(pattern, s string) bool { return regexp.MustCompile(pattern).MatchString(s) }

// ---------------------------------------------------------------------------------------
// CRP05 concurrency
// ---------------------------------------------------------------------------------------

func (e *crEnv) user(pool *pgxpool.Pool) string {
	e.t.Helper()
	var u string
	if err := pool.QueryRow(context.Background(), `SELECT session_user`).Scan(&u); err != nil {
		e.t.Fatal(err)
	}
	return u
}

// crBackend polls pg_stat_activity for the backend of login whose statement contains like and which waits on a
// lock of waitType ("" = any Lock wait; "advisory" = advisory lock wait). No sleeps order anything: the
// poll only observes a state the database reports.
func crBackend(t *testing.T, pool *pgxpool.Pool, login, like string, advisory bool) int {
	t.Helper()
	deadline := time.Now().Add(6 * time.Second)
	for time.Now().Before(deadline) {
		var pid int
		err := pool.QueryRow(context.Background(), `SELECT coalesce((SELECT pid FROM pg_stat_activity WHERE usename=$1 AND state='active' AND wait_event_type='Lock'
			AND query LIKE $2 AND (($3 AND wait_event='advisory') OR (NOT $3 AND wait_event<>'advisory')) ORDER BY query_start DESC LIMIT 1),0)`, login, "%"+like+"%", advisory).Scan(&pid)
		if err != nil {
			t.Fatal(err)
		}
		if pid != 0 {
			return pid
		}
		time.Sleep(5 * time.Millisecond) // observation poll only
	}
	t.Fatalf("no %s backend of %s reached a lock wait (statement like %q)", map[bool]string{true: "advisory", false: "row"}[advisory], login, like)
	return 0
}

// crAdvisoryKey is hashtextextended('claims-retention',0) split as pg_locks reports a bigint advisory key.
func crAdvisoryKey(t *testing.T, pool *pgxpool.Pool) (classid, objid int64) {
	t.Helper()
	if err := pool.QueryRow(context.Background(), `SELECT ((k>>32) & 4294967295), (k & 4294967295) FROM (SELECT hashtextextended('claims-retention',0) k) x`).Scan(&classid, &objid); err != nil {
		t.Fatal(err)
	}
	return classid, objid
}

type crAdvisory struct {
	classid, objid, objsubid int64
	granted                  bool
}

func crAdvisoryOf(t *testing.T, pool *pgxpool.Pool, pid int) []crAdvisory {
	t.Helper()
	rows, err := pool.Query(context.Background(), `SELECT classid::bigint,objid::bigint,objsubid::bigint,granted FROM pg_locks WHERE locktype='advisory' AND pid=$1 ORDER BY granted DESC`, pid)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []crAdvisory
	for rows.Next() {
		var a crAdvisory
		if err := rows.Scan(&a.classid, &a.objid, &a.objsubid, &a.granted); err != nil {
			t.Fatal(err)
		}
		out = append(out, a)
	}
	return out
}

type crRunResult struct {
	c   retention.Counts
	err error
}

// crPaused is a run_retention call parked inside its C5 loop: an owner transaction holds FOR UPDATE on the
// only eligible comment_events row, so the definer's DELETE waits (lock_timeout 2 s) while holding the
// advisory key and every lock of C1-C4. A paused run has at most ~2 s to live: keep the paused section short.
type crPaused struct {
	e       *crEnv
	blocker pgx.Tx
	res     chan crRunResult
	pid     int
	ev      mcEvent
	closed  bool
}

func (e *crEnv) pauseRun(limit int) *crPaused {
	e.t.Helper()
	ctx := context.Background()
	lane := e.socialLane(e.w.store)
	ev := e.commentEvent(lane, lane.asset+"_"+crDigits(10), crDigits(9), "add", crOld(30), "completed", 0)
	tx, err := e.f.owner.Begin(ctx)
	if err != nil {
		e.t.Fatal(err)
	}
	p := &crPaused{e: e, blocker: tx, res: make(chan crRunResult, 1), ev: ev}
	release := func() {
		if !p.closed {
			_ = tx.Rollback(ctx)
		}
	}
	e.t.Cleanup(release)
	e.holds = append(e.holds, release)
	if _, err := tx.Exec(ctx, `SELECT 1 FROM social.comment_events WHERE event_id=$1 FOR UPDATE`, ev.id); err != nil {
		e.t.Fatal(err)
	}
	go func() {
		c, err := retention.RunOnce(ctx, e.job, limit)
		p.res <- crRunResult{c, err}
	}()
	p.pid = crBackend(e.t, e.f.owner, e.user(e.job), "run_retention", false)
	return p
}

// release commits the blocker (the parked DELETE proceeds) and returns the run's outcome.
func (p *crPaused) release() crRunResult {
	p.e.t.Helper()
	if err := p.blocker.Commit(context.Background()); err != nil {
		p.e.t.Fatalf("release blocker: %v", err)
	}
	p.closed = true
	select {
	case r := <-p.res:
		return r
	case <-time.After(10 * time.Second):
		p.e.t.Fatal("paused run did not finish after the blocker committed")
	}
	return crRunResult{}
}

// crHold is an owner transaction holding a row lock.
type crHold struct{ tx pgx.Tx }

func (e *crEnv) holdRow(query string, args ...any) *crHold {
	e.t.Helper()
	ctx := context.Background()
	tx, err := e.f.owner.Begin(ctx)
	if err != nil {
		e.t.Fatal(err)
	}
	e.t.Cleanup(func() { _ = tx.Rollback(ctx) })
	e.holds = append(e.holds, func() { _ = tx.Rollback(ctx) })
	if _, err := tx.Exec(ctx, query, args...); err != nil {
		e.t.Fatalf("hold %q: %v", query, err)
	}
	return &crHold{tx}
}

func (h *crHold) commit(t *testing.T)   { t.Helper(); mustNil(t, h.tx.Commit(context.Background())) }
func (h *crHold) rollback(t *testing.T) { t.Helper(); mustNil(t, h.tx.Rollback(context.Background())) }

func mustNil(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

// CRP05 + §3/§7 (interleavings): all by held locks and pg_stat_activity/pg_locks observation, never by sleeping.
//   - held-row-locks-skip: every class skips a row another transaction holds; the next run purges it.
//   - reopen-in-flight: C2's recheck under the window lock skips a bundle whose window is being reopened.
//   - advisory-key: run_retention and erase_actor use the one key hashtextextended('claims-retention',0) (pg_locks);
//     two runs -> one `busy`; erase blocks on a running run; a run is `busy` while an erasure runs.
//   - lock-timeout: a wait beyond 2 s ends with 55P03 -> ErrBusy, nothing committed.
//   - parked-run-vs-real-operations: issue_link, redeem, window reopen, manual ingest and the consumer's message
//     write, all real code paths, against a run parked mid-flight: zero 40P01, no lost claim, message exactly once.
//   - F5: a social row whose inbox job is non-terminal is kept, its consumer retry returns ALREADY, not XX000.
func TestClaimsRetentionCRP05Concurrency(t *testing.T) {
	e := crSetup(t)
	ctx := context.Background()
	w := e.w
	deadlocksAtStart := crDeadlocks(t, e.f.owner)

	e.sub(t, "held-row-locks-skip", func(t *testing.T) {
		// One class at a time: seed one eligible row, let another transaction hold it, run (must skip it and
		// report 0 for the class), release, run again (must purge it and report 1).
		type step struct {
			name, class string
			seed        func() (hold func() *crHold, purged func() bool)
		}
		recent := w.session(t, w.store)
		w.closedAt(t, recent, crAgo(1, 0))
		steps := []step{
			{"C1 link", "links", func() (func() *crHold, func() bool) {
				b := w.bundle(t, recent, "manual", "", "hl-link-"+t04Tag())
				w.link(t, recent, b, crOld(7))
				return func() *crHold { return e.holdRow(`SELECT 1 FROM claims.links WHERE bundle_id=$1 FOR UPDATE`, b.id) },
					func() bool {
						return crCount(t, w.owner, `SELECT count(*) FROM claims.links WHERE bundle_id=$1`, b.id) == 0
					}
			}},
			{"C2 bundle row", "bundles", func() (func() *crHold, func() bool) {
				old := w.session(t, w.store)
				w.closedAt(t, old, crOld(90))
				b := w.bundle(t, old, "manual", "", "hl-bundle-"+t04Tag())
				return func() *crHold { return e.holdRow(`SELECT 1 FROM claims.bundles WHERE id=$1 FOR UPDATE`, b.id) },
					func() bool {
						return crCount(t, w.owner, `SELECT count(*) FROM claims.bundles WHERE id=$1 AND purged_at IS NOT NULL`, b.id) == 1
					}
			}},
			{"C2 window row", "bundles", func() (func() *crHold, func() bool) {
				old := w.session(t, w.store)
				w.closedAt(t, old, crOld(90))
				b := w.bundle(t, old, "manual", "", "hl-window-"+t04Tag())
				return func() *crHold {
						return e.holdRow(`SELECT 1 FROM live.claim_windows WHERE session_id=$1 FOR UPDATE`, old.id)
					}, func() bool {
						return crCount(t, w.owner, `SELECT count(*) FROM claims.bundles WHERE id=$1 AND purged_at IS NOT NULL`, b.id) == 1
					}
			}},
			{"C3 intake", "intake", func() (func() *crHold, func() bool) {
				m := w.source(t, recent, "page")
				ref := m.asset + "_" + crDigits(9)
				w.intake(t, m, ref, crHex64(), "APPLIED", crOld(30), "")
				return func() *crHold {
						return e.holdRow(`SELECT 1 FROM claims.meta_intake WHERE comment_ref=$1 FOR UPDATE`, ref)
					},
					func() bool {
						return crCount(t, w.owner, `SELECT count(*) FROM claims.meta_intake WHERE comment_ref=$1`, ref) == 0
					}
			}},
			{"C4 operation", "operations", func() (func() *crHold, func() bool) {
				m := w.source(t, recent, "page")
				b := w.bundle(t, recent, "manual", "", "hl-op-"+t04Tag())
				op := w.operation(t, m, b, m.asset+"_"+crDigits(9), "SUCCEEDED", crOld(30))
				return func() *crHold {
						return e.holdRow(`SELECT 1 FROM integration.operations WHERE id=$1::uuid FOR UPDATE`, op)
					},
					func() bool {
						return crCount(t, w.owner, `SELECT count(*) FROM integration.operations WHERE id=$1::uuid AND NOT request ? 'comment_ref'`, op) == 1
					}
			}},
			{"C5 comment (River job row held by a worker)", "comment_events", func() (func() *crHold, func() bool) {
				lane := e.socialLane(w.store)
				cmt := e.commentEvent(lane, lane.asset+"_"+crDigits(10), crDigits(9), "add", crOld(30), "completed", 0)
				return func() *crHold {
						return e.holdRow(`SELECT 1 FROM river_meta.river_job WHERE id=$1::bigint FOR UPDATE`, cmt.job)
					},
					func() bool {
						return crCount(t, w.owner, `SELECT count(*) FROM social.comment_events WHERE event_id=$1`, cmt.id) == 0
					}
			}},
			{"C5b conversation", "conversations", func() (func() *crHold, func() bool) {
				lane := e.socialLane(w.store)
				conv := randomUUID()
				mustExec(t, w.owner, `INSERT INTO social.conversations(id,tenant_id,store_id,app_id,object,asset_id,peer_key,created_at) VALUES($1,$2,$3,$4,'page',$5,$6,`+crOld(30)+`)`, conv, w.tenant, w.store, miApp, lane.asset, crHex64())
				w.conversations = append(w.conversations, conv)
				return func() *crHold { return e.holdRow(`SELECT 1 FROM social.conversations WHERE id=$1 FOR UPDATE`, conv) },
					func() bool {
						return crCount(t, w.owner, `SELECT count(*) FROM social.conversations WHERE id=$1`, conv) == 0
					}
			}},
		}
		for _, st := range steps {
			hold, purged := st.seed()
			locked := hold()
			got := e.run(500)
			if got[st.class] != 0 || purged() {
				t.Errorf("%s: the locked row was not skipped (counts %v, purged=%t)", st.name, got, purged())
			}
			locked.rollback(t)
			got = e.run(500)
			if got[st.class] != 1 || !purged() {
				t.Errorf("%s: the next run did not purge the skipped row (counts %v, purged=%t)", st.name, got, purged())
			}
			if again := e.run(500); again[st.class] != 0 {
				t.Errorf("%s: a third run still finds work: %v", st.name, again)
			}
		}
	})

	e.sub(t, "reopen-in-flight", func(t *testing.T) {
		old := w.session(t, w.store)
		w.closedAt(t, old, crOld(90))
		b := w.bundle(t, old, "manual", "", "reopen-"+t04Tag())
		w.bind(t, old, b)
		before := crDigest(t, w.owner, "claims.bundles", "id=$1", b.id)
		// A reopen is in flight: the window row is locked and its new state is uncommitted.
		hold := e.holdRow(`UPDATE live.claim_windows SET state='OPEN',generation=generation+1,opened_at=clock_timestamp(),closed_at=NULL,updated_at=clock_timestamp() WHERE session_id=$1`, old.id)
		c := e.run(500)
		if c["bundles"] != 0 || crDigest(t, w.owner, "claims.bundles", "id=$1", b.id) != before {
			t.Errorf("run purged a bundle whose window reopen was in flight (counts %v)", c)
		}
		hold.commit(t)
		c = e.run(500)
		if c["bundles"] != 0 || crDigest(t, w.owner, "claims.bundles", "id=$1", b.id) != before {
			t.Errorf("run purged a bundle of a reopened (OPEN) window (counts %v)", c)
		}
	})

	e.sub(t, "advisory-key-busy-and-erase-blocking", func(t *testing.T) {
		classid, objid := crAdvisoryKey(t, e.f.owner)
		// a bundle for the erasure to work on (recent window: not a purge candidate)
		s := w.session(t, w.store)
		w.closedAt(t, s, crAgo(1, 0))
		mb := w.bundle(t, s, "manual", "", "erase-"+t04Tag())
		p := e.pauseRun(500)
		logsBefore := crCount(t, e.f.owner, `SELECT count(*) FROM claims.retention_log WHERE kind='run'`)
		// the parked run holds the one advisory key
		held := crAdvisoryOf(t, e.f.owner, p.pid)
		if len(held) != 1 || !held[0].granted || held[0].classid != classid || held[0].objid != objid || held[0].objsubid != 1 {
			t.Errorf("run_retention advisory locks %+v, want exactly one granted key (%d,%d,1) = hashtextextended('claims-retention',0)", held, classid, objid)
		}
		// two runs -> one busy: the second returns at once without writing a log row
		second, err := retention.RunOnce(ctx, e.op, 500)
		if err != nil || second["busy"] != 1 || len(second) != 1 {
			t.Errorf("second run while one is parked: %v %v, want {busy:1}", second, err)
		}
		if n := crCount(t, e.f.owner, `SELECT count(*) FROM claims.retention_log WHERE kind='run'`); n != logsBefore {
			t.Error("a busy run wrote a log row")
		}
		// erase_actor blocks on the same key (advisory wait), it does not skip
		req := randomUUID()
		erased := make(chan struct {
			c   retention.Counts
			h   retention.Held
			err error
		}, 1)
		go func() {
			c, h, err := retention.Erase(ctx, e.op, retention.Selector{Request: req, Tenant: w.tenant, Store: s.store, Bundle: mb.id})
			erased <- struct {
				c   retention.Counts
				h   retention.Held
				err error
			}{c, h, err}
		}()
		epid := crBackend(t, e.f.owner, e.user(e.op), "erase_actor", true)
		waiting := crAdvisoryOf(t, e.f.owner, epid)
		if len(waiting) != 1 || waiting[0].granted || waiting[0].classid != classid || waiting[0].objid != objid || waiting[0].objsubid != 1 {
			t.Errorf("erase_actor advisory lock %+v, want one UNgranted request for the same key (%d,%d,1)", waiting, classid, objid)
		}
		res := p.release()
		if res.err != nil || res.c["comment_events"] != 1 || res.c["enforced"] != 1 {
			t.Errorf("parked run outcome: %v %v", res.c, res.err)
		}
		select {
		case r := <-erased:
			if r.err != nil || r.h.IsHeld() || r.c["bundles"] != 1 {
				t.Errorf("erase after the run finished: %v held=%v err=%v", r.c, r.h, r.err)
			}
		case <-time.After(8 * time.Second):
			t.Fatal("erase_actor stayed blocked after the run committed")
		}
		if n := crCount(t, e.f.owner, `SELECT count(*) FROM claims.bundles WHERE id=$1 AND purged_at IS NOT NULL AND label ~ '^erased-[0-9a-f]{32}$'`, mb.id); n != 1 {
			t.Error("the blocked erasure did not de-identify its bundle after acquiring the key")
		}

		// vice versa: a parked erasure makes a run `busy`, and holds the same key while it waits on a row.
		s2 := w.session(t, w.store)
		w.closedAt(t, s2, crAgo(1, 0))
		mb2 := w.bundle(t, s2, "manual", "", "erase2-"+t04Tag())
		hold := e.holdRow(`SELECT 1 FROM claims.bundles WHERE id=$1 FOR UPDATE`, mb2.id)
		req2 := randomUUID()
		erased2 := make(chan error, 1)
		go func() {
			_, _, err := retention.Erase(ctx, e.op, retention.Selector{Request: req2, Tenant: w.tenant, Store: s2.store, Bundle: mb2.id})
			erased2 <- err
		}()
		e2 := crBackend(t, e.f.owner, e.user(e.op), "erase_actor", false)
		heldKey := crAdvisoryOf(t, e.f.owner, e2)
		if len(heldKey) != 1 || !heldKey[0].granted || heldKey[0].classid != classid || heldKey[0].objid != objid || heldKey[0].objsubid != 1 {
			t.Errorf("erase_actor advisory locks %+v, want one granted key (%d,%d,1)", heldKey, classid, objid)
		}
		busy, err := retention.RunOnce(ctx, e.job, 500)
		if err != nil || busy["busy"] != 1 || len(busy) != 1 {
			t.Errorf("run while an erasure is in flight: %v %v, want {busy:1}", busy, err)
		}
		hold.commit(t)
		select {
		case err := <-erased2:
			if err != nil {
				t.Errorf("erasure after its row lock cleared: %v", err)
			}
		case <-time.After(8 * time.Second):
			t.Fatal("erasure did not finish")
		}
	})

	e.sub(t, "lock-timeout-is-busy-and-commits-nothing", func(t *testing.T) {
		s := w.session(t, w.store)
		w.closedAt(t, s, crAgo(1, 0))
		mb := w.bundle(t, s, "manual", "", "lt-"+t04Tag())
		w.bind(t, s, mb)
		before := crDigests(t, e.f.owner)
		logsBefore := crCount(t, e.f.owner, `SELECT count(*) FROM claims.retention_log`)
		hold := e.holdRow(`SELECT 1 FROM claims.bundles WHERE id=$1 FOR UPDATE`, mb.id)
		start := time.Now()
		_, _, err := retention.Erase(ctx, e.op, retention.Selector{Request: randomUUID(), Tenant: w.tenant, Store: s.store, Bundle: mb.id})
		took := time.Since(start)
		hold.rollback(t)
		if !errors.Is(err, retention.ErrBusy) {
			t.Errorf("erase blocked on a row lock: %v, want ErrBusy (55P03 from lock_timeout)", err)
		}
		if took < 1500*time.Millisecond || took > 5*time.Second {
			t.Errorf("erase gave up after %v, want about the 2 s lock_timeout of the definer", took)
		}
		crSameDigests(t, "timed-out erase", before, crDigests(t, e.f.owner))
		if n := crCount(t, e.f.owner, `SELECT count(*) FROM claims.retention_log`); n != logsBefore {
			t.Error("a timed-out erase left a log row")
		}
		// the same for a run parked on a row longer than the timeout
		lane := e.socialLane(w.store)
		cmt := e.commentEvent(lane, lane.asset+"_"+crDigits(10), crDigits(9), "add", crOld(30), "completed", 0)
		hold = e.holdRow(`SELECT 1 FROM social.comment_events WHERE event_id=$1 FOR UPDATE`, cmt.id)
		start = time.Now()
		_, rerr := retention.RunOnce(ctx, e.job, 500)
		took = time.Since(start)
		hold.rollback(t)
		if !errors.Is(rerr, retention.ErrBusy) || took < 1500*time.Millisecond || took > 5*time.Second {
			t.Errorf("run parked past lock_timeout: err=%v after %v, want ErrBusy after about 2 s", rerr, took)
		}
		if n := crCount(t, e.f.owner, `SELECT count(*) FROM claims.retention_log`); n != logsBefore {
			t.Error("a run that failed on lock_timeout left a log row (its whole transaction must roll back)")
		}
		if crCount(t, e.f.owner, `SELECT count(*) FROM social.comment_events WHERE event_id=$1`, cmt.id) != 1 {
			t.Error("the timed-out run deleted a row")
		}
		// idempotent retry: with the lock gone the same calls succeed.
		if c := e.run(500); c["comment_events"] != 1 {
			t.Errorf("retry after the lock cleared: %v", c)
		}
	})

	e.sub(t, "F5-non-terminal-inbox-job-keeps-row", func(t *testing.T) {
		lane := e.socialLane(w.store)
		peer := crHex64()
		msg := e.message(lane, peer, crOld(30), "running") // consumer job still running (non-terminal)
		cmtID := lane.asset + "_" + crDigits(10)
		cmt := e.commentEvent(lane, cmtID, crDigits(9), "add", crOld(30), "running", 1)
		c := e.run(500)
		if c["messages"] != 0 || c["comment_events"] != 0 || c["conversations"] != 0 {
			t.Errorf("rows of non-terminal inbox jobs were counted or deleted: %v", c)
		}
		if crCount(t, w.owner, `SELECT count(*) FROM social.messages WHERE event_id=$1`, msg.id)+crCount(t, w.owner, `SELECT count(*) FROM social.comment_events WHERE event_id=$1`, cmt.id) != 2 {
			t.Fatal("a social row with a non-terminal inbox job was deleted (its retry would hit social_terminal XX000)")
		}
		// the consumer's retry of the same attempt: ALREADY, no error (not XX000)
		var outcome string
		if err := lane.consumer.QueryRow(ctx, `SELECT outcome FROM meta_inbox.load_social_event($1::uuid,$2::bigint,1)`, msg.id, msg.job).Scan(&outcome); err != nil || outcome != "ALREADY" {
			t.Errorf("consumer retry after the run: outcome=%q err=%v (SQLSTATE %s), want ALREADY", outcome, err, crSQLState(err))
		}
		if _, err := lane.consumer.Exec(ctx, `SELECT meta_inbox.finish_social_event($1::uuid,$2::bigint,1,'message',$3)`, msg.id, msg.job, peer); err != nil {
			t.Errorf("consumer re-finish after the run: %v (SQLSTATE %s), want the ALREADY no-op", err, crSQLState(err))
		}
		if _, err := lane.consumer.Exec(ctx, `SELECT meta_inbox.finish_social_event($1::uuid,$2::bigint,1,'comment',$3)`, cmt.id, cmt.job, crCommentKey(miApp, "page", lane.asset, cmtID)); err != nil {
			t.Errorf("consumer comment re-finish after the run: %v (SQLSTATE %s)", err, crSQLState(err))
		}
		// once the job is terminal the next run removes them.
		e.terminal(msg)
		e.terminal(cmt)
		c = e.run(500)
		crWant(t, "after the inbox jobs completed", c, map[string]int64{"messages": 1, "comment_events": 1, "conversations": 1})
	})

	e.sub(t, "parked-run-vs-real-operations", func(t *testing.T) {
		h := lcSetup(t)
		f := h.f
		// X: a real session with a real claim, an issued (unbound) link and a window closed long ago; Y: a live session.
		xs := h.draft(t, f.storeA1)
		h.open(t, xs, "EXACT")
		h.offer(t, xs, "A1", h.stock.skus[0].ID, 5)
		claimX := h.accepted(t, xs, "", "parked-x", "A1")
		linkX := h.link(t, xs, claimX.BundleID, 0, false)
		h.closeWindow(t, xs)
		mustExec(t, f.owner, `UPDATE live.claim_windows SET opened_at=`+crOld(90)+`-interval '1 hour',closed_at=`+crOld(90)+` WHERE session_id=$1`, xs)
		// Y lives in the other store (one OPEN window per store: X's reopen must not collide with it)
		ys := h.draftAs(t, h.token, f.storeA2)
		if _, err := h.setWindow(h.token, f.storeA2, ys, claims.WindowInput{ExpectedVersion: 0, State: claims.WindowOpen, MatchMode: claims.MatchExact}); err != nil {
			t.Fatalf("open Y: %v", err)
		}
		skuY := lcSKUs(t, f, f.tenantA, f.storeA2, "USD", 1)[0]
		if _, err := h.createOffer(h.token, f.storeA2, t04Key("crp05-offer"), ys, claims.OfferInput{Keyword: "B1", SKUID: skuY, MaxQuantityPerClaim: 5}); err != nil {
			t.Fatalf("offer Y: %v", err)
		}
		winX := h.board(t, xs).Window
		// the consumer's message for a peer whose (aged, empty) conversation is a C5b candidate
		lane := e.socialLane(w.store)
		peer := crHex64()
		conv := randomUUID()
		mustExec(t, w.owner, `INSERT INTO social.conversations(id,tenant_id,store_id,app_id,object,asset_id,peer_key,created_at) VALUES($1,$2,$3,$4,'page',$5,$6,`+crOld(30)+`)`, conv, w.tenant, w.store, miApp, lane.asset, peer)
		w.conversations = append(w.conversations, conv)
		dm := mcPost(t, lane.m, lane.asset, miMessage(lane.asset, "m."+randomUUID(), "parked "+t04Tag()))
		mcRunning(t, lane.m, dm, 1)
		w.events = append(w.events, dm.id)

		owner := mustIssue(t, h.service, f.storeA1)
		before := crDeadlocks(t, f.owner)
		p := e.pauseRun(500)

		// (1) manual ingest into the live session does not wait for the run.
		ingest := make(chan error, 1)
		go func() {
			_, err := h.manual(h.token, f.storeA2, t04Key("crp05-ingest"), ys, claims.ManualClaimInput{ActorLabel: "parked-y", Text: "B1"})
			ingest <- err
		}()
		select {
		case err := <-ingest:
			if err != nil {
				t.Errorf("manual ingest while a run is parked: %v", err)
			}
		case <-time.After(1500 * time.Millisecond):
			t.Error("manual ingest into a live session waited for the parked run")
		}
		// (2) the consumer's message write does not wait either, and is committed exactly once.
		if _, err := lane.consumer.Exec(ctx, `SELECT meta_inbox.finish_social_event($1::uuid,$2::bigint,1,'message',$3)`, dm.id, dm.job, peer); err != nil {
			t.Errorf("consumer message write while a run is parked: %v", err)
		}
		// (3) real operations on the bundle the run has locked wait for it.
		pidIssue, doneIssue := h.bgMerchant(t, h.token, f.storeA1, func(tx pgx.Tx, sc platform.Scope) error {
			_, err := claims.IssueLink(h.ctx, tx, sc, h.token, t04Key("crp05-link"), xs, claimX.BundleID, claims.LinkInput{ExpectedGeneration: linkX.Generation})
			return err
		})
		pidRedeem, doneRedeem := h.bgBuyer(t, owner, func(c context.Context, tx pgx.Tx, sc buyer.Scope) error {
			_, err := claims.RedeemLink(c, tx, sc, t04Key("crp05-redeem"), linkX.Token, claims.RedeemInput{ExpectedBundleVersion: claimX.BundleVersion})
			return err
		})
		pidReopen, doneReopen := h.bgMerchant(t, h.token, f.storeA1, func(tx pgx.Tx, sc platform.Scope) error {
			_, err := claims.SetWindow(h.ctx, tx, sc, h.token, t04Key("crp05-reopen"), xs, claims.WindowInput{ExpectedVersion: winX.Version, State: claims.WindowOpen, MatchMode: winX.MatchMode})
			return err
		})
		for name, pid := range map[string]int{"issue_link": pidIssue, "redeem": pidRedeem, "window reopen": pidReopen} {
			lpWaitLock(t, f, pid, false)
			_ = name
		}
		res := p.release()
		if res.err != nil {
			t.Fatalf("parked run failed: %v", res.err)
		}
		crWant(t, "parked run", res.c, map[string]int64{"bundles": 1, "comment_events": 1, "conversations": 0})
		// outcomes of the operations that waited: consistent with "the purge happened first", never a raw database error
		for name, done := range map[string]<-chan error{"issue_link": doneIssue, "redeem": doneRedeem} {
			err := waitError(t, done)
			if err == nil || crSQLState(err) != "" || (!errors.Is(err, command.ErrNotFound) && !errors.Is(err, command.ErrConflict) && !errors.Is(err, platform.ErrScopeNotFound)) {
				t.Errorf("%s after the purge: %v, want a mapped not-found/conflict error", name, err)
			}
		}
		if err := waitError(t, doneReopen); err != nil {
			t.Errorf("window reopen after the run committed: %v", err)
		}
		// the purge stands (the run locked the bundle while the window was still CLOSED), the window is OPEN, nothing was lost:
		bx := e.bundleRow(t, claimX.BundleID)
		if bx.purged == nil || bx.owner != nil {
			t.Errorf("bundle X after the parked run: purged=%v owner=%v", bx.purged, bx.owner)
		}
		if crCount(t, f.owner, `SELECT count(*) FROM live.claim_windows WHERE session_id=$1 AND state='OPEN'`, xs) != 1 {
			t.Error("the reopen did not commit")
		}
		if crCount(t, f.owner, `SELECT count(*) FROM claims.events WHERE bundle_id=$1 AND outcome='ACCEPTED'`, claimX.BundleID) != 1 ||
			crCount(t, f.owner, `SELECT count(*) FROM claims.lines WHERE bundle_id=$1 AND quantity=1`, claimX.BundleID) != 1 {
			t.Error("claim events/lines of the purged bundle changed")
		}
		// the live session's claim (ingested during the park) is intact and unpurged; the DM is stored exactly once; its conversation survived.
		if crCount(t, f.owner, `SELECT count(*) FROM claims.bundles WHERE session_id=$1 AND purged_at IS NULL`, ys) != 1 {
			t.Error("the claim ingested during the parked run is missing or purged")
		}
		if crCount(t, f.owner, `SELECT count(*) FROM social.messages WHERE event_id=$1`, dm.id) != 1 || crCount(t, f.owner, `SELECT count(*) FROM social.conversations WHERE id=$1`, conv) != 1 {
			t.Error("the message written during the parked run is not stored exactly once in its surviving conversation")
		}
		if d := crDeadlocks(t, f.owner) - before; d != 0 {
			t.Errorf("%d deadlock(s) (40P01) while a run was parked against real operations", d)
		}
		// the same operations after the run: issue_link on the purged bundle is not found (RD8) even though the window is open.
		_, err := h.issue(h.token, f.storeA1, t04Key("crp05-link2"), xs, claimX.BundleID, claims.LinkInput{ExpectedGeneration: 0})
		if err == nil || crSQLState(err) != "" {
			t.Errorf("issue_link on a purged bundle in an OPEN window: %v, want mapped not-found", err)
		}
		h.closeWindow(t, xs)
		if _, err := h.setWindow(h.token, f.storeA2, ys, claims.WindowInput{ExpectedVersion: h.getWindowVersion(t, f.storeA2, ys), State: claims.WindowClosed, MatchMode: claims.MatchExact}); err != nil {
			t.Errorf("close Y: %v", err)
		}
	})

	e.sub(t, "parked-run-vs-intake-apply", func(t *testing.T) {
		// The Meta intake poller applies a PENDING row (real claim path) while a run is parked: the apply never waits for
		// retention and the claim it creates is neither lost nor purged.
		m := mciSetup(t, mciOpts{})
		f := m.h.f
		sent := m.postFB(t, "", mciDigits(15), "A1", mciAt(3*time.Second), nil)
		if row := m.fbIntake(t, sent); row.State != "PENDING" {
			t.Fatalf("staged intake row is %s, want PENDING", row.State)
		}
		before := crDeadlocks(t, f.owner)
		p := e.pauseRun(500)
		applied := make(chan struct {
			leased bool
			err    error
		}, 1)
		go func() {
			leased, err := m.poller.ApplyOne(ctx)
			applied <- struct {
				leased bool
				err    error
			}{leased, err}
		}()
		select {
		case r := <-applied:
			if r.err != nil || !r.leased {
				t.Errorf("intake apply while a run is parked: leased=%t err=%v", r.leased, r.err)
			}
		case <-time.After(1500 * time.Millisecond):
			t.Error("the intake apply waited for the parked run")
		}
		res := p.release()
		if res.err != nil {
			t.Fatalf("parked run: %v", res.err)
		}
		if row := m.fbIntake(t, sent); row.State != "APPLIED" {
			t.Errorf("intake after the apply: %s, want APPLIED", row.State)
		}
		if m.bundleCount(t) != 1 || crCount(t, f.owner, `SELECT count(*) FROM claims.bundles WHERE session_id=$1 AND purged_at IS NULL`, m.session) != 1 {
			t.Error("the claim applied during the parked run is missing or purged")
		}
		if d := crDeadlocks(t, f.owner) - before; d != 0 {
			t.Errorf("%d deadlock(s) between the intake apply and the parked run", d)
		}
	})

	if d := crDeadlocks(t, e.f.owner) - deadlocksAtStart; d != 0 {
		t.Errorf("CRP05 caused %d deadlock(s) in total", d)
	}
}

func (h *lcHarness) getWindowVersion(t *testing.T, store, session string) int64 {
	t.Helper()
	b, err := h.getBoard(h.token, store, session)
	if err != nil {
		t.Fatalf("board: %v", err)
	}
	return b.Window.Version
}

// ---------------------------------------------------------------------------------------
// CRP06 erasure
// ---------------------------------------------------------------------------------------

// crActor is one synthetic Meta actor with the footprint an erasure must remove: two bundles (two stores, two
// sessions) with binding/link/line, intake rows, comment events (add + edit + remove of one comment), reply
// operations and a DM conversation. Owner-seeded, except that the social rows are projected by the real
// consumer function.
type crActor struct {
	sender, asset, key, peer string
	bundles                  []crBundle
	sessions                 []crSess
	comments                 []string // comment refs with intake rows (the last one has no social rows)
	events                   []mcEvent
	ops                      []string
	msgs                     []mcEvent
	intake                   int
	lane                     *crSocial
}

// actorWorld builds the footprint of sender on lane's asset. intakeExpr ages every intake row; twoStores adds the
// second store/session (same actor key, same asset).
func (e *crEnv) actorWorld(lane *crSocial, sender string, twoStores bool, intakeExpr string) *crActor {
	t, w := e.t, e.w
	a := &crActor{sender: sender, asset: lane.asset, lane: lane}
	a.key = meta.ClaimActorKey(e.actor, "page", lane.asset, sender)
	a.peer = meta.SocialPeerKey(miApp, "page", lane.asset, sender)
	future := `(clock_timestamp() + interval '1 hour')`
	s1 := w.session(t, w.store)
	w.closedAt(t, s1, crAgo(10, 0))
	m1 := w.sourceOn(t, s1, "page", lane.asset)
	b1 := w.bundle(t, s1, "facebook", a.key, "")
	w.bind(t, s1, b1)
	w.link(t, s1, b1, future)
	offer, sku := w.offer(t, s1)
	w.line(t, s1, b1, offer, sku, 2)
	a.bundles, a.sessions = append(a.bundles, b1), append(a.sessions, s1)
	c1, c2 := lane.asset+"_"+crDigits(10), lane.asset+"_"+crDigits(10)
	add := e.commentEvent(lane, c1, sender, "add", crAgo(20, 0), "completed", 1)
	edit := e.commentEvent(lane, c1, sender, "edit", crAgo(20, 0), "completed", 2)
	rem := e.commentEvent(lane, c1, sender, "remove", crAgo(20, 0), "completed", 3)
	add2 := e.commentEvent(lane, c2, sender, "add", crAgo(20, 0), "completed", 4)
	a.events = []mcEvent{add, edit, rem, add2}
	w.intake(t, m1, c1, a.key, "APPLIED", intakeExpr, add.id)
	w.intake(t, m1, c2, a.key, "DROPPED", intakeExpr, add2.id)
	a.comments = []string{c1, c2}
	a.intake = 2
	a.ops = append(a.ops, w.operation(t, m1, b1, c1, "SUCCEEDED", crAgo(1, 0)))
	if twoStores {
		s2 := w.session(t, w.store2)
		w.closedAt(t, s2, crAgo(10, 0))
		m2 := w.sourceOn(t, s2, "page", lane.asset)
		b2 := w.bundle(t, s2, "facebook", a.key, "")
		w.bind(t, s2, b2)
		w.link(t, s2, b2, future)
		c3 := lane.asset + "_" + crDigits(10)
		w.intake(t, m2, c3, a.key, "APPLIED", intakeExpr, "")
		a.comments = append(a.comments, c3)
		a.intake++
		a.ops = append(a.ops, w.operation(t, m2, b2, c3, "SUCCEEDED", crAgo(1, 0)))
		a.bundles, a.sessions = append(a.bundles, b2), append(a.sessions, s2)
	}
	a.msgs = []mcEvent{e.message(lane, a.peer, crAgo(20, 0), "completed"), e.message(lane, a.peer, crAgo(20, 0), "completed")}
	return a
}

func (a *crActor) commentKeys() []string {
	var out []string
	for _, c := range a.comments {
		out = append(out, crCommentKey(miApp, "page", a.asset, c))
	}
	return out
}

func (a *crActor) bundleIDs() []string {
	var out []string
	for _, b := range a.bundles {
		out = append(out, b.id)
	}
	return out
}

// footprint fingerprints every row the actor owns (owner SQL), for "byte-identical" checks.
func (e *crEnv) footprint(a *crActor) string {
	t, p := e.t, e.f.owner
	return strings.Join([]string{
		crDigest(t, p, "claims.bundles", "id=ANY($1::uuid[])", a.bundleIDs()),
		crDigest(t, p, "claims.links", "bundle_id=ANY($1::uuid[])", a.bundleIDs()),
		crDigest(t, p, "claims.lines", "bundle_id=ANY($1::uuid[])", a.bundleIDs()),
		crDigest(t, p, "claims.events", "bundle_id=ANY($1::uuid[])", a.bundleIDs()),
		crDigest(t, p, "claims.meta_intake", "actor_key=$1", a.key),
		crDigest(t, p, "social.comment_events", "comment_key=ANY($1::text[])", a.commentKeys()),
		crDigest(t, p, "integration.operations", "id=ANY($1::uuid[])", a.ops),
		crDigest(t, p, "social.conversations", "peer_key=$1", a.peer),
		crDigest(t, p, "social.messages", "conversation_id IN (SELECT id FROM social.conversations WHERE peer_key=$1)", a.peer),
	}, "|")
}

func (e *crEnv) wantErasure(a *crActor, peers bool, comments int) map[string]int64 {
	m := map[string]int64{"bundles": int64(len(a.bundles)), "lines": 1, "links": int64(len(a.bundles)), "intake": int64(a.intake),
		"comment_events": int64(comments), "operations": int64(len(a.ops)), "messages": 0, "conversations": 0}
	if peers {
		m["messages"], m["conversations"] = int64(len(a.msgs)), 1
	}
	return m
}

// crErased asserts the state after an erasure of a (owner SQL).
func (e *crEnv) assertErased(a *crActor, peers, commentsGone bool) {
	t, p := e.t, e.f.owner
	ctx := context.Background()
	for _, b := range a.bundles {
		r := e.bundleRow(t, b.id)
		if r.purged == nil || r.owner != nil || r.bound != nil || r.actor == a.key || !regexpMatch(crHex64Re, r.actor) || r.label != "" {
			t.Errorf("bundle %s after erasure: purged=%v owner=%v actor_key changed=%t label=%q", b.id, r.purged, r.owner, r.actor != a.key, r.label)
		}
		if crCount(t, p, `SELECT count(*) FROM claims.links WHERE bundle_id=$1`, b.id) != 0 {
			t.Errorf("link of bundle %s survives", b.id)
		}
	}
	if n := crCount(t, p, `SELECT count(*) FROM claims.lines WHERE bundle_id=ANY($1::uuid[]) AND applied_version IS NOT NULL`, a.bundleIDs()); n != 0 {
		t.Errorf("%d lines keep applied_version", n)
	}
	if n := crCount(t, p, `SELECT count(*) FROM claims.meta_intake WHERE actor_key=$1 OR comment_ref=ANY($2::text[])`, a.key, a.comments); n != 0 {
		t.Errorf("%d intake rows of the actor survive", n)
	}
	if commentsGone {
		if n := crCount(t, p, `SELECT count(*) FROM social.comment_events WHERE comment_key=ANY($1::text[])`, a.commentKeys()); n != 0 {
			t.Errorf("%d social comment rows (add/edit/remove) of the actor's comments survive", n)
		}
	}
	if peers {
		if n := crCount(t, p, `SELECT count(*) FROM social.conversations WHERE peer_key=$1`, a.peer); n != 0 {
			t.Errorf("peer conversation survives")
		}
		for _, m := range a.msgs {
			if crCount(t, p, `SELECT count(*) FROM social.messages WHERE event_id=$1`, m.id) != 0 {
				t.Errorf("DM %s survives", m.id)
			}
		}
	} else if crCount(t, p, `SELECT count(*) FROM social.conversations WHERE peer_key=$1`, a.peer) != 1 {
		t.Errorf("a selector without peer keys removed the peer conversation")
	}
	for _, id := range a.ops {
		var raw, key string
		if err := p.QueryRow(ctx, `SELECT request::text,semantic_key FROM integration.operations WHERE id=$1`, id).Scan(&raw, &key); err != nil {
			t.Fatal(err)
		}
		if strings.Contains(raw, `"comment_ref"`) || !strings.Contains(raw, `"redacted": true`) || key != "mpr-purged:"+id {
			t.Errorf("operation %s not redacted: request=%s key=%s", id, raw, key)
		}
	}
}

func crSHA256Hex(s string) string { sum := sha256.Sum256([]byte(s)); return hex.EncodeToString(sum[:]) }

// crLogEntry reads the actor_erased row of request (owner SQL).
type crErasedRow struct {
	selector, actor []byte
	bundleRef       *string
	by              string
	counts          map[string]any
}

func (e *crEnv) erasedRow(request string) (r crErasedRow, found bool) {
	t := e.t
	var counts string
	err := e.f.owner.QueryRow(context.Background(), `SELECT selector_digest,actor_digest,bundle_ref::text,executed_by,counts::text FROM claims.retention_log WHERE kind='actor_erased' AND request_id=$1`, request).
		Scan(&r.selector, &r.actor, &r.bundleRef, &r.by, &counts)
	if errors.Is(err, pgx.ErrNoRows) {
		return r, false
	}
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(counts), &r.counts); err != nil {
		t.Fatal(err)
	}
	return r, true
}

// CRP06 + §3 erase_actor + RD4/RD5/RD6/RD8: the four selectors, one actor across two stores and sessions, untouched
// actors and peers byte-identical, the four holds (+ the 8-day boundary), replay/conflict/not-found, the reserved
// label pattern, and one real webhook -> consumer -> intake -> apply chain (peer/comment keys of the projection).
func TestClaimsRetentionCRP06Erasure(t *testing.T) {
	e := crSetup(t)
	ctx := context.Background()
	w := e.w
	lane := e.socialLane(w.store)
	lane2 := e.socialLane(w.store)

	e.sub(t, "selector-a-across-two-stores", func(t *testing.T) {
		p := e.actorWorld(lane, "918273645", true, crAgo(20, 0))
		q := e.actorWorld(lane, "918273699", false, crAgo(20, 0))  // another actor on the same asset
		r := e.actorWorld(lane2, "918273645", false, crAgo(20, 0)) // the same sender id on another asset
		manual := w.bundle(t, p.sessions[0], "manual", "", "keep-manual-"+t04Tag())
		manualBefore := crDigest(t, w.owner, "claims.bundles", "id=$1", manual.id)
		qBefore, rBefore := e.footprint(q), e.footprint(r)
		request := randomUUID()
		sel := retention.Selector{Request: request, Object: "page", Asset: p.asset, ActorKey: p.key, PeerKeys: []string{p.peer}}
		mark := time.Now()
		c, held, err := retention.Erase(ctx, e.op, sel)
		if err != nil || held.IsHeld() {
			t.Fatalf("Erase(a): counts=%v held=%v err=%v", c, held.IsHeld(), err)
		}
		crWant(t, "erase (a)", c, e.wantErasure(p, true, 4))
		e.assertErased(p, true, true)
		if e.footprint(q) != qBefore || e.footprint(r) != rBefore {
			t.Error("another actor / the same sender on another asset changed")
		}
		if crDigest(t, w.owner, "claims.bundles", "id=$1", manual.id) != manualBefore {
			t.Error("a manual bundle of the same session changed")
		}
		// the log row: digests only, counts only, by the operator login
		row, ok := e.erasedRow(request)
		if !ok {
			t.Fatal("no actor_erased log row")
		}
		if hex.EncodeToString(row.actor) != crSHA256Hex(p.key) || row.bundleRef != nil {
			t.Errorf("actor_digest %x is not sha256(actor_key) (bundle_ref=%v)", row.actor, row.bundleRef)
		}
		hexKey := crSHA256Hex(p.key)
		rawKey := sha256.Sum256([]byte(p.key))
		variants := map[string]string{
			"a|object|asset|hex(sha256(key))": crSHA256Hex("a|page|" + p.asset + "|" + hexKey),
			"a|object|asset|raw sha256(key)":  crSHA256Hex("a|page|" + p.asset + "|" + string(rawKey[:])),
		}
		matched := ""
		for name, want := range variants {
			if hex.EncodeToString(row.selector) == want {
				matched = name
			}
		}
		if matched == "" {
			t.Errorf("selector_digest %x matches neither reading of the contract's canonical selector a|object|asset|sha256(key)", row.selector)
		} else {
			t.Logf("selector_digest = sha256(%s)", matched)
		}
		if row.by != e.user(e.op) {
			t.Errorf("executed_by=%q, want the operator login", row.by)
		}
		for k, v := range row.counts {
			if _, isNum := v.(float64); !isNum {
				t.Errorf("log counts[%s]=%v is not a number", k, v)
			}
		}
		if row.counts["bundles"] != float64(2) || row.counts["comment_events"] != float64(4) {
			t.Errorf("log counts %v disagree with the returned counts", row.counts)
		}
		if n := crCount(t, w.owner, `SELECT count(*) FROM claims.retention_log WHERE kind='actor_erased' AND request_id=$1`, request); n != 1 {
			t.Errorf("%d actor_erased rows for one request", n)
		}
		_ = mark
		// replay of the same request and selector: same counts + replayed=1, nothing more written, no second log row
		before := crDigests(t, w.owner)
		logs := crCount(t, w.owner, `SELECT count(*) FROM claims.retention_log`)
		c2, held2, err := retention.Erase(ctx, e.op, sel)
		if err != nil || held2.IsHeld() || c2["replayed"] != 1 {
			t.Fatalf("replay: %v held=%v err=%v", c2, held2.IsHeld(), err)
		}
		for k, v := range c {
			if c2[k] != v {
				t.Errorf("replay counts[%s]=%d, want the stored %d", k, c2[k], v)
			}
		}
		crSameDigests(t, "replay", before, crDigests(t, w.owner))
		if crCount(t, w.owner, `SELECT count(*) FROM claims.retention_log`) != logs {
			t.Error("a replay wrote a log row")
		}
		// same request, another selector: PT409 -> ErrConflict, nothing written
		_, _, err = retention.Erase(ctx, e.op, retention.Selector{Request: request, Object: "page", Asset: p.asset, CommentRef: p.comments[0]})
		if !errors.Is(err, retention.ErrConflict) {
			t.Errorf("same request with another selector: %v, want ErrConflict", err)
		}
		_, _, err = retention.Erase(ctx, e.op, retention.Selector{Request: request, Object: "page", Asset: p.asset, ActorKey: q.key})
		if !errors.Is(err, retention.ErrConflict) {
			t.Errorf("same request with another actor key: %v, want ErrConflict", err)
		}
		crSameDigests(t, "conflicting request", before, crDigests(t, w.owner))
		// unknown selectors: PT404 -> ErrNotFound, nothing written (the request id stays free)
		for name, s := range map[string]retention.Selector{
			"unknown actor key":  {Request: randomUUID(), Object: "page", Asset: p.asset, ActorKey: crHex64()},
			"unknown comment":    {Request: randomUUID(), Object: "page", Asset: p.asset, CommentRef: p.asset + "_" + crDigits(10)},
			"unknown bundle":     {Request: randomUUID(), Tenant: w.tenant, Store: w.store, Bundle: randomUUID()},
			"already erased key": {Request: randomUUID(), Object: "page", Asset: p.asset, ActorKey: p.key}, // its rows are de-identified now
		} {
			_, _, err := retention.Erase(ctx, e.op, s)
			if !errors.Is(err, retention.ErrNotFound) {
				t.Errorf("%s: %v, want ErrNotFound", name, err)
			}
			if _, found := e.erasedRow(s.Request); found {
				t.Errorf("%s: a not-found erasure left a log row", name)
			}
		}
		crSameDigests(t, "not found", before, crDigests(t, w.owner))
		// The retry with the used request id but different subject was refused above; a new request id for the same
		// (already erased) actor is not found rather than silently succeeding twice.
	})

	e.sub(t, "selector-b-comment-id", func(t *testing.T) {
		p := e.actorWorld(lane, "918273611", true, crAgo(20, 0))
		q := e.actorWorld(lane, "918273622", false, crAgo(20, 0))
		qBefore := e.footprint(q)
		sel := retention.Selector{Request: randomUUID(), Object: "page", Asset: p.asset, CommentRef: p.comments[1]} // any comment of the person
		c, held, err := retention.Erase(ctx, e.op, sel)
		if err != nil || held.IsHeld() {
			t.Fatalf("Erase(b): %v %v %v", c, held.IsHeld(), err)
		}
		crWant(t, "erase (b)", c, e.wantErasure(p, false, 4)) // no peer keys: the DM conversation stays
		e.assertErased(p, false, true)
		if e.footprint(q) != qBefore {
			t.Error("another actor changed")
		}
		row, ok := e.erasedRow(sel.Request)
		if !ok || hex.EncodeToString(row.actor) != crSHA256Hex(p.key) {
			t.Error("selector (b) log row lacks the resolved actor's digest")
		}
	})

	e.sub(t, "selector-c-meta-bundle", func(t *testing.T) {
		p := e.actorWorld(lane, "918273633", true, crAgo(20, 0))
		q := e.actorWorld(lane, "918273644", false, crAgo(20, 0))
		qBefore := e.footprint(q)
		sel := retention.Selector{Request: randomUUID(), Tenant: w.tenant, Store: p.sessions[1].store, Bundle: p.bundles[1].id}
		c, held, err := retention.Erase(ctx, e.op, sel)
		if err != nil || held.IsHeld() {
			t.Fatalf("Erase(c meta): %v %v %v", c, held.IsHeld(), err)
		}
		crWant(t, "erase (c, meta bundle)", c, e.wantErasure(p, false, 4)) // the whole actor, both stores
		e.assertErased(p, false, true)
		if e.footprint(q) != qBefore {
			t.Error("another actor changed")
		}
		row, ok := e.erasedRow(sel.Request)
		if !ok || row.actor == nil || hex.EncodeToString(row.actor) != crSHA256Hex(p.key) || row.bundleRef != nil {
			t.Error("meta bundle selector must log the actor digest (replayable across restore), not the bundle")
		}
	})

	e.sub(t, "selector-c-manual-bundle", func(t *testing.T) {
		s := w.session(t, w.store)
		w.closedAt(t, s, crAgo(5, 0))
		target := w.bundle(t, s, "manual", "", "erase-target-"+t04Tag())
		sibling := w.bundle(t, s, "manual", "", "erase-sibling-"+t04Tag())
		reserved8 := w.bundle(t, s, "manual", "", "erased-1a2b3c4d") // merchant label outside the reserved 32-hex pattern
		w.bind(t, s, target)
		w.link(t, s, target, `(clock_timestamp() + interval '1 hour')`)
		offer, sku := w.offer(t, s)
		w.line(t, s, target, offer, sku, 1)
		w.link(t, s, sibling, `(clock_timestamp() + interval '1 hour')`)
		otherBefore := crDigest(t, w.owner, "claims.bundles", "id=ANY($1::uuid[])", []string{sibling.id, reserved8.id}) + crDigest(t, w.owner, "claims.links", "bundle_id=$1", sibling.id)
		sel := retention.Selector{Request: randomUUID(), Tenant: w.tenant, Store: s.store, Bundle: target.id}
		c, held, err := retention.Erase(ctx, e.op, sel)
		if err != nil || held.IsHeld() {
			t.Fatalf("Erase(c manual): %v %v %v", c, held.IsHeld(), err)
		}
		crWant(t, "erase (c, manual bundle)", c, map[string]int64{"bundles": 1, "lines": 1, "links": 1, "intake": 0, "comment_events": 0, "operations": 0, "messages": 0, "conversations": 0})
		r := e.bundleRow(t, target.id)
		plain := "erased-" + strings.ReplaceAll(target.id, "-", "")
		if r.purged == nil || r.owner != nil || !regexpMatch(`^erased-[0-9a-f]{32}$`, r.label) || r.label == plain || r.label == target.label || !regexpMatch(crHex64Re, r.actor) {
			t.Errorf("manual bundle after erasure: purged=%v owner=%v label=%q (derived-from-id=%t)", r.purged, r.owner, r.label, r.label == plain)
		}
		if crDigest(t, w.owner, "claims.bundles", "id=ANY($1::uuid[])", []string{sibling.id, reserved8.id})+crDigest(t, w.owner, "claims.links", "bundle_id=$1", sibling.id) != otherBefore {
			t.Error("a sibling manual bundle (or the merchant's erased-<8 hex> label) changed")
		}
		row, ok := e.erasedRow(sel.Request)
		if !ok || row.actor != nil || row.bundleRef == nil || *row.bundleRef != target.id {
			t.Errorf("manual bundle selector must log bundle_tenant/store/ref (no actor digest): %+v", row)
		}
		// a second manual erasure in the same session picks a different random label and does not collide
		sel2 := retention.Selector{Request: randomUUID(), Tenant: w.tenant, Store: s.store, Bundle: sibling.id}
		if _, _, err := retention.Erase(ctx, e.op, sel2); err != nil {
			t.Errorf("second manual erasure in the session: %v", err)
		}
		if a, b := e.bundleRow(t, target.id).label, e.bundleRow(t, sibling.id).label; a == b {
			t.Errorf("two erased bundles share label %q", a)
		}
	})

	e.sub(t, "holds", func(t *testing.T) {
		type hold struct {
			name    string
			mutate  func(a *crActor)
			retryAt func(a *crActor) time.Time
		}
		hourOut := func(*crActor) time.Time { return time.Now().Add(time.Hour) }
		intakePlus8 := func(a *crActor) time.Time {
			var at time.Time
			if err := w.owner.QueryRow(ctx, `SELECT max(received_at)+interval '8 days' FROM claims.meta_intake WHERE actor_key=$1`, a.key).Scan(&at); err != nil {
				t.Fatal(err)
			}
			return at
		}
		holds := []hold{
			{"OPEN window of an affected bundle", func(a *crActor) { w.reopen(t, a.sessions[0]) }, hourOut},
			{"PENDING intake", func(a *crActor) {
				mustExec(t, w.owner, `UPDATE claims.meta_intake SET state='PENDING',applied_event_id=NULL,drop_reason=NULL,received_at=`+crAgo(1, 0)+` WHERE comment_ref=$1`, a.comments[0])
			}, intakePlus8},
			{"intake 7 d 23 h old", func(a *crActor) {
				mustExec(t, w.owner, `UPDATE claims.meta_intake SET received_at=`+crAgo(8, 3600)+` WHERE actor_key=$1`, a.key)
			}, intakePlus8},
			{"intake 8 d minus a margin old", func(a *crActor) {
				mustExec(t, w.owner, `UPDATE claims.meta_intake SET received_at=`+crNew(8)+` WHERE actor_key=$1`, a.key)
			}, intakePlus8},
			{"non-terminal operation (READY)", func(a *crActor) {
				mustExec(t, w.owner, `UPDATE integration.operations SET state='READY',generation=0 WHERE id=$1`, a.ops[0])
			}, hourOut},
			{"non-terminal operation (UNKNOWN)", func(a *crActor) {
				mustExec(t, w.owner, `UPDATE integration.operations SET state='UNKNOWN' WHERE id=$1`, a.ops[0])
			}, hourOut},
			{"social row whose inbox job is non-terminal (F5)", func(a *crActor) {
				mustExec(t, w.owner, `UPDATE river_meta.river_job SET state='running',finalized_at=NULL WHERE id=$1`, a.events[1].job)
			}, nil},
		}
		for i, h := range holds {
			a := e.actorWorld(lane, fmt.Sprintf("9182750%02d", i), false, crAgo(20, 0))
			h.mutate(a)
			before := crDigests(t, w.owner, append(append([]string{}, crTables...), "meta_inbox.events", "claims.retention_log")...)
			foot := e.footprint(a)
			req := randomUUID()
			c, held, err := retention.Erase(ctx, e.op, retention.Selector{Request: req, Object: "page", Asset: a.asset, ActorKey: a.key, PeerKeys: []string{a.peer}})
			if err != nil || !held.IsHeld() || len(c) != 0 {
				t.Errorf("%s: counts=%v held=%v err=%v, want held with retry_after", h.name, c, held.IsHeld(), err)
				continue
			}
			if h.retryAt != nil {
				if want := h.retryAt(a); held.RetryAfter.Sub(want) > 90*time.Second || want.Sub(held.RetryAfter) > 90*time.Second {
					t.Errorf("%s: retry_after=%s, want about %s", h.name, held.RetryAfter.Format(time.RFC3339), want.Format(time.RFC3339))
				}
			} else if !held.RetryAfter.After(time.Now()) {
				t.Errorf("%s: retry_after %s is not in the future", h.name, held.RetryAfter)
			}
			crSameDigests(t, h.name+" (held writes nothing)", before, crDigests(t, w.owner, append(append([]string{}, crTables...), "meta_inbox.events", "claims.retention_log")...))
			if e.footprint(a) != foot {
				t.Errorf("%s: the actor's rows changed although the erasure was held", h.name)
			}
			if _, found := e.erasedRow(req); found {
				t.Errorf("%s: a held erasure left an actor_erased row", h.name)
			}
		}
		// the boundary: 8 d + margin old intake, closed window, terminal ops: NOT held (RD5 "received < 8 days ago").
		ok := e.actorWorld(lane, "918275099", false, crAgo(8, -int(crMargin/time.Second)))
		c, held, err := retention.Erase(ctx, e.op, retention.Selector{Request: randomUUID(), Object: "page", Asset: ok.asset, ActorKey: ok.key, PeerKeys: []string{ok.peer}})
		if err != nil || held.IsHeld() || c["bundles"] != 1 {
			t.Errorf("intake older than 8 d was held or failed: %v held=%v err=%v", c, held.IsHeld(), err)
		}
		// a held request id stays free: once the hold clears, the same id erases.
		a := e.actorWorld(lane, "918275098", false, crAgo(20, 0))
		mustExec(t, w.owner, `UPDATE integration.operations SET state='READY',generation=0 WHERE id=$1`, a.ops[0])
		req := randomUUID()
		sel := retention.Selector{Request: req, Object: "page", Asset: a.asset, ActorKey: a.key}
		if _, held, err := retention.Erase(ctx, e.op, sel); err != nil || !held.IsHeld() {
			t.Fatalf("expected a hold: held=%v err=%v", held.IsHeld(), err)
		}
		mustExec(t, w.owner, `UPDATE integration.operations SET state='SUCCEEDED',generation=1 WHERE id=$1`, a.ops[0])
		if c, held, err := retention.Erase(ctx, e.op, sel); err != nil || held.IsHeld() || c["bundles"] != 1 {
			t.Errorf("same request id after the hold cleared: %v held=%v err=%v", c, held.IsHeld(), err)
		}
	})

	e.sub(t, "reserved-labels", func(t *testing.T) {
		s := w.session(t, w.store2) // store 2: store 1's single OPEN window slot is used by the merchant-path check below
		w.reopen(t, s)              // window OPEN: a merchant can create bundles
		x := w.bundle(t, s, "manual", "", "res-x-"+t04Tag())
		hexX := strings.ReplaceAll(x.id, "-", "")
		reserved := []string{"purged-" + crHex64()[:32], "erased-" + crHex64()[:32], "purged-" + hexX, "erased-" + strings.ReplaceAll(randomUUID(), "-", "")}
		for _, label := range reserved {
			_, err := w.owner.Exec(ctx, `INSERT INTO claims.bundles(tenant_id,store_id,session_id,platform,actor_key,label) VALUES($1,$2,$3,'manual',$4,$5)`, w.tenant, s.store, s.id, crHex64(), label)
			if crSQLState(err) != "23514" {
				t.Errorf("INSERT of reserved label %q: %v, want 23514", label, err)
			}
			_, err = w.owner.Exec(ctx, `UPDATE claims.bundles SET label=$1 WHERE id=$2`, label, x.id)
			if crSQLState(err) != "23514" {
				t.Errorf("UPDATE to reserved label %q: %v, want 23514", label, err)
			}
		}
		// labels outside the reserved pattern are ordinary merchant text
		for _, label := range []string{"purged-1a2b3c4d", "erased-1a2b3c4d", "purged-" + crHex64()[:31], "Purged-" + crHex64()[:32], "erased-" + crHex64()[:33]} {
			if _, err := w.owner.Exec(ctx, `INSERT INTO claims.bundles(tenant_id,store_id,session_id,platform,actor_key,label) VALUES($1,$2,$3,'manual',$4,$5)`, w.tenant, s.store, s.id, crHex64(), label); err != nil {
				t.Errorf("INSERT of ordinary label %q: %v", label, err)
			}
		}
		// the real merchant path: RecordManualClaim with a reserved actor label is refused, no bundle is created.
		h := lcSetup(t)
		f := h.f
		sess := h.draft(t, f.storeA1)
		h.open(t, sess, "EXACT")
		h.offer(t, sess, "A1", h.stock.skus[0].ID, 5)
		for _, label := range []string{"purged-" + crHex64()[:32], "erased-" + crHex64()[:32]} {
			r, err := h.manual(h.token, f.storeA1, t04Key("crp06-label"), sess, claims.ManualClaimInput{ActorLabel: label, Text: "A1"})
			if err == nil && r.Outcome == claims.OutcomeAccepted {
				t.Errorf("merchant claim with reserved label %q was accepted", label)
			}
		}
		if n := crCount(t, f.owner, `SELECT count(*) FROM claims.bundles WHERE session_id=$1`, sess); n != 0 {
			t.Errorf("%d bundles were created by refused reserved-label claims", n)
		}
		h.closeWindow(t, sess)
		// a label purged-<hex of X> cannot exist while the window is OPEN (above); after the window closes and ages, C2 and an
		// erasure of X both complete.
		w.closedAt(t, s, "clock_timestamp()") // close properly (the interval trigger closes the reopened generation) ...
		w.closedAt(t, s, crOld(90))           // ... then age the closed window (CLOSED -> CLOSED, no interval change)
		e.enforceDefaults()
		c := e.run(500)
		if c["bundles"] < 1 {
			t.Fatalf("C2 did not purge the aged session: %v", c)
		}
		if r := e.bundleRow(t, x.id); r.purged == nil || !regexpMatch(crPurgedLabel, r.label) || r.label == "purged-"+hexX {
			t.Errorf("C2 of bundle X: purged=%v label=%q", r.purged, r.label)
		}
		y := w.bundle(t, w.session2(t, crOld(90)), "manual", "", "res-y-"+t04Tag())
		if _, _, err := retention.Erase(ctx, e.op, retention.Selector{Request: randomUUID(), Tenant: w.tenant, Store: w.store, Bundle: y.id}); err != nil {
			t.Errorf("erasure of a bundle in an aged session: %v", err)
		}
	})

	e.sub(t, "real-pipeline-peer-and-comment-keys", func(t *testing.T) {
		m := mciSetup(t, mciOpts{})
		sender := mciDigits(15)
		fb := m.postFB(t, "", sender, "A1", mciAt(3*time.Second), nil)
		dmSender := "4" // miMessage's fixed sender id
		dm := mcPost(t, m.page, m.pageAsset, miMessage(m.pageAsset, "m."+randomUUID(), "real dm "+t04Tag()))
		mcAwait(t, m.page, dm)
		if n := m.apply(t); n != 1 {
			t.Fatalf("poller applied %d rows, want 1", n)
		}
		// CRP01's projection clause: the peer key the real consumer stored equals meta.SocialPeerKey, and the comment
		// key equals the contract's tupleHash of (app, object, asset, comment id).
		var peer string
		if err := m.h.f.owner.QueryRow(ctx, `SELECT c.peer_key FROM social.messages x JOIN social.conversations c ON c.id=x.conversation_id WHERE x.event_id=$1`, dm.id).Scan(&peer); err != nil {
			t.Fatalf("real DM conversation: %v", err)
		}
		if want := meta.SocialPeerKey(miApp, "page", m.pageAsset, dmSender); peer != want {
			t.Errorf("meta.SocialPeerKey %s != the peer_key the consumer projection stored %s", want, peer)
		}
		var ckey string
		if err := m.h.f.owner.QueryRow(ctx, `SELECT comment_key FROM social.comment_events WHERE event_id=$1`, fb.ev.id).Scan(&ckey); err != nil {
			t.Fatalf("real comment event: %v", err)
		}
		if want := crCommentKey(miApp, "page", m.pageAsset, fb.comment); ckey != want {
			t.Errorf("comment_key stored by the consumer %s != tupleHash(meta-social-comment/v1, app, object, asset, comment) %s", ckey, want)
		}
		bundle := m.bundleOfActor(t, meta.ClaimActorKey(m.actor, "page", m.pageAsset, sender))
		// preconditions of an erasure: window closed, intake old enough, no live reply operation.
		m.h.closeWindow(t, m.session)
		mustExec(t, m.h.f.owner, `UPDATE claims.meta_intake SET received_at=`+crAgo(20, 0)+` WHERE object='page' AND asset_id=$1`, m.pageAsset)
		// (b): the real comment id resolves the real actor; the real bundle, intake, comment row are erased.
		c, held, err := retention.Erase(ctx, e.op, retention.Selector{Request: randomUUID(), Object: "page", Asset: m.pageAsset, CommentRef: fb.comment})
		if err != nil || held.IsHeld() {
			t.Fatalf("Erase(b) of a real comment: %v held=%v err=%v", c, held.IsHeld(), err)
		}
		crWant(t, "real erase (b)", c, map[string]int64{"bundles": 1, "lines": 1, "intake": 1, "comment_events": 1})
		if r := e.bundleRow(t, bundle); r.purged == nil || r.actor == meta.ClaimActorKey(m.actor, "page", m.pageAsset, sender) {
			t.Errorf("real bundle after erasure: purged=%v", r.purged)
		}
		if crCount(t, m.h.f.owner, `SELECT count(*) FROM social.comment_events WHERE event_id=$1`, fb.ev.id) != 0 {
			t.Error("the real comment event survives")
		}
		if crCount(t, m.h.f.owner, `SELECT count(*) FROM social.conversations WHERE peer_key=$1`, peer) != 1 {
			t.Error("selector (b) removed the DM conversation")
		}
		// (a) with the peer key derived exactly as the CLI does: the real DM conversation goes (the actor key of that sender has no rows).
		c, held, err = retention.Erase(ctx, e.op, retention.Selector{Request: randomUUID(), Object: "page", Asset: m.pageAsset,
			ActorKey: meta.ClaimActorKey(m.actor, "page", m.pageAsset, dmSender), PeerKeys: []string{meta.SocialPeerKey(miApp, "page", m.pageAsset, dmSender)}})
		if err != nil || held.IsHeld() {
			t.Fatalf("Erase(a) of the real DM peer: %v held=%v err=%v", c, held.IsHeld(), err)
		}
		crWant(t, "real erase (a) peer", c, map[string]int64{"messages": 1, "conversations": 1, "bundles": 0})
		if crCount(t, m.h.f.owner, `SELECT count(*) FROM social.conversations WHERE peer_key=$1`, peer) != 0 {
			t.Error("the real DM conversation survives an erasure with the derived peer key")
		}
	})
}

// session2 is a CLOSED session (store 1) whose window closed at the SQL expression.
func (w *crWorld) session2(t *testing.T, closedExpr string) crSess {
	t.Helper()
	s := w.session(t, w.store)
	w.closedAt(t, s, closedExpr)
	return s
}

// bundleOfActor returns the id of the (single) non-manual bundle with actor key in the harness session.
func (e *mciEnv) bundleOfActor(t *testing.T, key string) string {
	t.Helper()
	var id string
	if err := e.h.f.owner.QueryRow(context.Background(), `SELECT id::text FROM claims.bundles WHERE session_id=$1 AND actor_key=$2`, e.session, key).Scan(&id); err != nil {
		t.Fatalf("real bundle of the actor: %v", err)
	}
	return id
}

// ---------------------------------------------------------------------------------------
// CRP02 schema
// ---------------------------------------------------------------------------------------

var crRoles = []string{"commerce_retention_writer", "commerce_retention_job", "commerce_retention_operator"}

// crWantMatrix is contract §4 as a set of "role|kind|object|privilege" strings: the ONLY privileges the three
// retention roles may hold anywhere in the database (schemas other than pg_*, information_schema and public).
// kind: table (table-level), col (column-level, no table-level grant), schema, func (EXECUTE on functions the role
// does not own). Written from the §4 table, not from the migration.
func crWantMatrix() map[string]bool {
	w := map[string]bool{}
	add := func(role, kind, object string, privs ...string) {
		for _, p := range privs {
			w[role+"|"+kind+"|"+object+"|"+p] = true
		}
	}
	const W = "commerce_retention_writer"
	cols := func(table string, privs string, names ...string) {
		for _, n := range names {
			add(W, "col", table+"."+n, privs)
		}
	}
	// commerce_retention_writer
	cols("claims.bundles", "SELECT", "tenant_id", "store_id", "id", "session_id", "platform", "actor_key", "label", "owner_id", "purged_at")
	cols("claims.bundles", "UPDATE", "actor_key", "label", "owner_id", "bound_at", "purged_at", "updated_at")
	cols("claims.lines", "SELECT", "tenant_id", "store_id", "bundle_id")
	cols("claims.lines", "UPDATE", "applied_version")
	cols("claims.links", "SELECT", "tenant_id", "store_id", "bundle_id", "expires_at")
	add(W, "table", "claims.links", "DELETE")
	cols("claims.links", "UPDATE", "expires_at")
	cols("claims.meta_intake", "SELECT", "tenant_id", "store_id", "id", "inbox_event_id", "session_id", "object", "asset_id", "comment_ref", "actor_key", "state", "received_at")
	add(W, "table", "claims.meta_intake", "DELETE")
	cols("claims.meta_intake", "UPDATE", "updated_at")
	cols("live.claim_windows", "SELECT", "tenant_id", "store_id", "session_id", "state", "closed_at")
	cols("live.claim_windows", "UPDATE", "updated_at")
	cols("integration.operations", "SELECT", "id", "tenant_id", "store_id", "action", "state", "request", "created_at")
	cols("integration.operations", "UPDATE", "request", "semantic_key", "updated_at")
	add(W, "table", "social.conversations", "SELECT", "DELETE")
	add(W, "table", "social.messages", "SELECT", "DELETE")
	add(W, "table", "social.comment_events", "SELECT", "DELETE")
	cols("social.conversations", "UPDATE", "next_seq")
	cols("social.messages", "UPDATE", "received_at")
	cols("social.comment_events", "UPDATE", "received_at")
	add(W, "table", "claims.retention_policy", "SELECT")
	cols("claims.retention_policy", "UPDATE", "enforced", "link_days", "intake_days", "claims_days", "social_days", "version", "updated_at", "updated_by")
	add(W, "table", "claims.retention_log", "SELECT", "INSERT", "DELETE")
	for _, s := range []string{"claims", "live", "integration", "social", "meta_inbox"} {
		add(W, "schema", s, "USAGE")
	}
	add(W, "func", "meta_inbox.lock_purgeable", "EXECUTE")
	// commerce_retention_job / commerce_retention_operator
	add("commerce_retention_job", "schema", "claims", "USAGE")
	add("commerce_retention_job", "func", "claims.run_retention", "EXECUTE")
	add("commerce_retention_job", "func", "claims.retention_status", "EXECUTE")
	add("commerce_retention_operator", "schema", "claims", "USAGE")
	for _, fn := range []string{"run_retention", "retention_status", "erase_actor", "set_retention_policy", "replay_actor_erasures"} {
		add("commerce_retention_operator", "func", "claims."+fn, "EXECUTE")
	}
	return w
}

// crMatrixOf returns the effective privileges of role (a role or a login; direct, PUBLIC and inherited, exactly as
// has_*_privilege evaluates them) over every object outside pg_*, information_schema and public.
func crMatrixOf(t *testing.T, pool *pgxpool.Pool, role string) map[string]bool {
	t.Helper()
	const rel = `c.relkind IN ('r','p','v','m','f') AND n.nspname !~ '^(pg_|information_schema$)' AND n.nspname<>'public'`
	queries := []string{
		`SELECT $1||'|table|'||n.nspname||'.'||c.relname||'|'||p FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace
		  CROSS JOIN unnest(ARRAY['SELECT','INSERT','UPDATE','DELETE','TRUNCATE','REFERENCES','TRIGGER']) p WHERE ` + rel + ` AND has_table_privilege($1,c.oid,p)`,
		`SELECT $1||'|col|'||n.nspname||'.'||c.relname||'.'||a.attname||'|'||p FROM pg_attribute a JOIN pg_class c ON c.oid=a.attrelid JOIN pg_namespace n ON n.oid=c.relnamespace
		  CROSS JOIN unnest(ARRAY['SELECT','INSERT','UPDATE','REFERENCES']) p WHERE a.attnum>0 AND NOT a.attisdropped AND ` + rel + `
		  AND has_column_privilege($1,c.oid,a.attnum,p) AND NOT has_table_privilege($1,c.oid,p)`,
		`SELECT $1||'|seq|'||n.nspname||'.'||c.relname||'|'||p FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace
		  CROSS JOIN unnest(ARRAY['USAGE','SELECT','UPDATE']) p WHERE c.relkind='S' AND n.nspname !~ '^(pg_|information_schema$)' AND n.nspname<>'public' AND has_sequence_privilege($1,c.oid,p)`,
		`SELECT $1||'|schema|'||n.nspname||'|'||p FROM pg_namespace n CROSS JOIN unnest(ARRAY['USAGE','CREATE']) p
		  WHERE n.nspname !~ '^(pg_|information_schema$)' AND n.nspname<>'public' AND has_schema_privilege($1,n.oid,p)`,
		// EXECUTE only where the role can reach the schema (USAGE), on functions it does not own.
		`SELECT $1||'|func|'||n.nspname||'.'||p.proname||'|EXECUTE' FROM pg_proc p JOIN pg_namespace n ON n.oid=p.pronamespace
		  WHERE n.nspname !~ '^(pg_|information_schema$)' AND n.nspname<>'public' AND has_schema_privilege($1,n.oid,'USAGE')
		  AND has_function_privilege($1,p.oid,'EXECUTE') AND pg_get_userbyid(p.proowner)<>$1`,
	}
	out := map[string]bool{}
	for _, q := range queries {
		rows, err := pool.Query(context.Background(), q, role)
		if err != nil {
			t.Fatalf("matrix query for %s: %v", role, err)
		}
		for rows.Next() {
			var k string
			if err := rows.Scan(&k); err != nil {
				t.Fatal(err)
			}
			out[k] = true
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		rows.Close()
	}
	return out
}

func crDiff(a, b map[string]bool) (only []string) {
	for k := range a {
		if !b[k] {
			only = append(only, k)
		}
	}
	sort.Strings(only)
	return only
}

func crHead(items []string) string {
	if len(items) > 12 {
		return strings.Join(items[:12], "; ") + fmt.Sprintf("; ... (%d in total)", len(items))
	}
	return strings.Join(items, "; ")
}

// crAssertMatrix compares the effective privileges of the three roles with §4, in both directions.
func crAssertMatrix(t *testing.T, pool *pgxpool.Pool, label string) {
	t.Helper()
	got := map[string]bool{}
	for _, r := range crRoles {
		for k := range crMatrixOf(t, pool, r) {
			got[k] = true
		}
	}
	want := crWantMatrix()
	if extra := crDiff(got, want); len(extra) > 0 {
		t.Errorf("%s: retention roles hold privileges outside §4 (%d): %s", label, len(extra), crHead(extra))
	}
	if missing := crDiff(want, got); len(missing) > 0 {
		t.Errorf("%s: §4 privileges the retention roles lack (%d): %s", label, len(missing), crHead(missing))
	}
}

// crAssertRoles checks contract §2 role attributes and that no role is a member of another or has members
// other than logins (the test logins are excluded by name prefix mi_test_).
func crAssertRoles(t *testing.T, pool *pgxpool.Pool, label string) {
	t.Helper()
	ctx := context.Background()
	for _, r := range crRoles {
		var login, super, bypass, createdb, createrole, repl bool
		if err := pool.QueryRow(ctx, `SELECT rolcanlogin,rolsuper,rolbypassrls,rolcreatedb,rolcreaterole,rolreplication FROM pg_roles WHERE rolname=$1`, r).Scan(&login, &super, &bypass, &createdb, &createrole, &repl); err != nil {
			t.Errorf("%s: role %s missing: %v", label, r, err)
			continue
		}
		if login || super || bypass || createdb || createrole || repl {
			t.Errorf("%s: role %s attributes login=%t super=%t bypassrls=%t createdb=%t createrole=%t replication=%t, want all false", label, r, login, super, bypass, createdb, createrole, repl)
		}
		if n := crCount(t, pool, `SELECT count(*) FROM pg_auth_members m JOIN pg_roles g ON g.oid=m.roleid JOIN pg_roles r ON r.oid=m.member WHERE g.rolname=$1 AND r.rolname !~ '^(mi_test_|mci_mixed_)'`, r); n != 0 {
			t.Errorf("%s: %d non-test roles are members of %s", label, n, r)
		}
		if n := crCount(t, pool, `SELECT count(*) FROM pg_auth_members m JOIN pg_roles r ON r.oid=m.member WHERE r.rolname=$1`, r); n != 0 {
			t.Errorf("%s: %s is a member of %d other roles (privileges would inherit)", label, r, n)
		}
		var comment *string
		if err := pool.QueryRow(ctx, `SELECT shobj_description(oid,'pg_authid') FROM pg_roles WHERE rolname=$1`, r).Scan(&comment); err != nil || comment == nil || strings.TrimSpace(*comment) == "" {
			t.Errorf("%s: role %s has no COMMENT (PROCESS §5)", label, r)
		}
	}
}

// crSevenFunctions are the definers/trigger function of contract §2/§3.
var crDefiners = map[string]string{
	"run_retention": "claims", "apply_actor_erasure": "claims", "erase_actor": "claims", "set_retention_policy": "claims",
	"retention_status": "claims", "replay_actor_erasures": "claims", "links_not_purged": "claims",
}

func crAssertDefiners(t *testing.T, pool *pgxpool.Pool, label string) {
	t.Helper()
	ctx := context.Background()
	for fn, schema := range crDefiners {
		var owner string
		var secdef bool
		var config []string
		var comment *string
		err := pool.QueryRow(ctx, `SELECT pg_get_userbyid(p.proowner),p.prosecdef,coalesce(p.proconfig,'{}'),obj_description(p.oid,'pg_proc') FROM pg_proc p JOIN pg_namespace n ON n.oid=p.pronamespace WHERE n.nspname=$1 AND p.proname=$2`, schema, fn).
			Scan(&owner, &secdef, &config, &comment)
		if err != nil {
			t.Errorf("%s: function %s.%s: %v", label, schema, fn, err)
			continue
		}
		if owner != "commerce_retention_writer" || !secdef {
			t.Errorf("%s: %s owner=%s security definer=%t, want commerce_retention_writer and SECURITY DEFINER", label, fn, owner, secdef)
		}
		joined := strings.Join(config, ",")
		if !strings.Contains(joined, "search_path=pg_catalog") {
			t.Errorf("%s: %s proconfig %v lacks search_path=pg_catalog", label, fn, config)
		}
		if fn != "links_not_purged" && !strings.Contains(joined, "lock_timeout=2s") {
			t.Errorf("%s: %s proconfig %v lacks lock_timeout=2s (IR-5)", label, fn, config)
		}
		if comment == nil || !strings.Contains(*comment, "internal/retention") && fn != "links_not_purged" {
			t.Errorf("%s: %s COMMENT does not name internal/retention: %v", label, fn, comment)
		}
		if comment == nil || strings.TrimSpace(*comment) == "" {
			t.Errorf("%s: %s has no COMMENT", label, fn)
		}
	}
	// exactly these seven functions live in claims that are owned by the definer role: no undeclared definer.
	if n := crCount(t, pool, `SELECT count(*) FROM pg_proc p WHERE pg_get_userbyid(p.proowner)='commerce_retention_writer'`); n != len(crDefiners) {
		t.Errorf("%s: commerce_retention_writer owns %d functions, want %d", label, n, len(crDefiners))
	}
	if n := crCount(t, pool, `SELECT count(*) FROM pg_class c WHERE pg_get_userbyid(c.relowner)='commerce_retention_writer'`); n != 0 {
		t.Errorf("%s: the definer role owns %d relations (it must own only functions)", label, n)
	}
}

func crAssertRLS(t *testing.T, pool *pgxpool.Pool, label string) {
	t.Helper()
	for _, table := range []string{"claims.retention_policy", "claims.retention_log", "claims.bundles", "claims.lines", "claims.links", "claims.meta_intake",
		"live.claim_windows", "integration.operations", "social.conversations", "social.messages", "social.comment_events"} {
		var enabled, force bool
		if err := pool.QueryRow(context.Background(), `SELECT relrowsecurity,relforcerowsecurity FROM pg_class WHERE oid=$1::regclass`, table).Scan(&enabled, &force); err != nil || !enabled || !force {
			t.Errorf("%s: %s row security enabled=%t forced=%t (%v), want both", label, table, enabled, force, err)
		}
	}
	// every policy written for the retention roles: (table, command) multiset of contract §4
	want := map[string]int{
		"claims.bundles|SELECT": 1, "claims.bundles|UPDATE": 1, "claims.lines|SELECT": 1, "claims.lines|UPDATE": 1,
		"claims.links|SELECT": 1, "claims.links|DELETE": 1, "claims.links|UPDATE": 1,
		"claims.meta_intake|SELECT": 1, "claims.meta_intake|DELETE": 1, "claims.meta_intake|UPDATE": 1,
		"live.claim_windows|SELECT": 1, "live.claim_windows|UPDATE": 1,
		"integration.operations|SELECT": 1, "integration.operations|UPDATE": 1,
		"social.conversations|SELECT": 1, "social.conversations|DELETE": 1, "social.conversations|UPDATE": 1,
		"social.messages|SELECT": 1, "social.messages|DELETE": 1, "social.messages|UPDATE": 1,
		"social.comment_events|SELECT": 1, "social.comment_events|DELETE": 1, "social.comment_events|UPDATE": 1,
		"claims.retention_policy|SELECT": 1, "claims.retention_policy|UPDATE": 1,
		"claims.retention_log|SELECT": 1, "claims.retention_log|INSERT": 1, "claims.retention_log|DELETE": 1,
	}
	rows, err := pool.Query(context.Background(), `SELECT schemaname||'.'||tablename||'|'||cmd,roles::text,permissive FROM pg_policies WHERE roles::text LIKE '%retention%'`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	got := map[string]int{}
	for rows.Next() {
		var key, roles, permissive string
		if err := rows.Scan(&key, &roles, &permissive); err != nil {
			t.Fatal(err)
		}
		if roles != "{commerce_retention_writer}" || permissive != "PERMISSIVE" {
			t.Errorf("%s: policy %s roles=%s permissive=%s, want {commerce_retention_writer} PERMISSIVE", label, key, roles, permissive)
		}
		got[key]++
	}
	for k, n := range want {
		if got[k] != n {
			t.Errorf("%s: %d policies for %s, want %d", label, got[k], k, n)
		}
	}
	for k := range got {
		if _, ok := want[k]; !ok {
			t.Errorf("%s: undeclared retention policy on %s", label, k)
		}
	}
	// no policy of any other table names a retention role
	if n := crCount(t, pool, `SELECT count(*) FROM pg_policies WHERE roles::text LIKE '%retention%' AND schemaname||'.'||tablename NOT IN (`+crQuoteList(want)+`)`); n != 0 {
		t.Errorf("%s: %d retention policies on undeclared tables", label, n)
	}
}

func crQuoteList(want map[string]int) string {
	seen := map[string]bool{}
	var out []string
	for k := range want {
		tb := strings.SplitN(k, "|", 2)[0]
		if !seen[tb] {
			seen[tb] = true
			out = append(out, "'"+tb+"'")
		}
	}
	sort.Strings(out)
	return strings.Join(out, ",")
}

// crAssertObjects: column, constraints, indexes, trigger, comments of contract §2.
func crAssertObjects(t *testing.T, pool *pgxpool.Pool, label string) {
	t.Helper()
	ctx := context.Background()
	var typ string
	if err := pool.QueryRow(ctx, `SELECT format_type(atttypid,atttypmod) FROM pg_attribute WHERE attrelid='claims.bundles'::regclass AND attname='purged_at' AND NOT attisdropped`).Scan(&typ); err != nil || typ != "timestamp with time zone" {
		t.Errorf("%s: claims.bundles.purged_at: %q %v", label, typ, err)
	}
	for _, c := range []string{"bundle_purged_unbound", "bundle_label_reserved"} {
		var valid bool
		if err := pool.QueryRow(ctx, `SELECT convalidated FROM pg_constraint WHERE conrelid='claims.bundles'::regclass AND conname=$1`, c).Scan(&valid); err != nil || !valid {
			t.Errorf("%s: constraint %s validated=%t (%v)", label, c, valid, err)
		}
	}
	for _, ix := range []string{"claims_bundle_actor", "claims_bundle_purge", "meta_intake_actor", "meta_intake_retention", "claims_link_expiry",
		"social_comment_retention", "social_message_retention", "retention_one_request", "retention_recent_runs"} {
		if crCount(t, pool, `SELECT count(*) FROM pg_indexes WHERE indexname=$1`, ix) != 1 {
			t.Errorf("%s: index %s missing", label, ix)
		}
	}
	var unique bool
	if err := pool.QueryRow(ctx, `SELECT indisunique FROM pg_index WHERE indexrelid='claims.retention_one_request'::regclass`).Scan(&unique); err != nil || !unique {
		t.Errorf("%s: retention_one_request unique=%t %v", label, unique, err)
	}
	var trig string
	if err := pool.QueryRow(ctx, `SELECT pg_get_triggerdef(oid) FROM pg_trigger WHERE tgrelid='claims.links'::regclass AND tgname='links_not_purged'`).Scan(&trig); err != nil ||
		!strings.Contains(trig, "BEFORE INSERT OR UPDATE") || !strings.Contains(trig, "FOR EACH ROW") || !strings.Contains(trig, "links_not_purged") {
		t.Errorf("%s: claims.links trigger links_not_purged: %q %v", label, trig, err)
	}
	// COMMENT ON every table and column of the two new tables (PROCESS §5)
	for _, table := range []string{"claims.retention_policy", "claims.retention_log"} {
		var tc *string
		if err := pool.QueryRow(ctx, `SELECT obj_description($1::regclass,'pg_class')`, table).Scan(&tc); err != nil || tc == nil || strings.TrimSpace(*tc) == "" {
			t.Errorf("%s: %s has no table COMMENT", label, table)
		}
		rows, err := pool.Query(ctx, `SELECT a.attname,col_description(a.attrelid,a.attnum) FROM pg_attribute a WHERE a.attrelid=$1::regclass AND a.attnum>0 AND NOT a.attisdropped`, table)
		if err != nil {
			t.Fatal(err)
		}
		for rows.Next() {
			var col string
			var d *string
			if err := rows.Scan(&col, &d); err != nil {
				t.Fatal(err)
			}
			if d == nil || strings.TrimSpace(*d) == "" {
				t.Errorf("%s: %s.%s has no COMMENT", label, table, col)
			}
		}
		rows.Close()
	}
}

// crStrip removes the role prefix of matrix keys so a login's set can be compared with its role's.
func crStrip(m map[string]bool) map[string]bool {
	out := map[string]bool{}
	for k := range m {
		out[k[strings.Index(k, "|")+1:]] = true
	}
	return out
}

// crAs runs one statement as a role (SET LOCAL ROLE from the owner connection), in its own transaction that is always
// rolled back, and returns rows affected (or the error). It is how the RLS policies of §4 are exercised without a login.
func crAs(t *testing.T, pool *pgxpool.Pool, role, query string, args ...any) (int64, error) {
	t.Helper()
	ctx := context.Background()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `SET LOCAL ROLE `+pgx.Identifier{role}.Sanitize()); err != nil {
		t.Fatalf("SET LOCAL ROLE %s: %v", role, err)
	}
	tag, err := tx.Exec(ctx, query, args...)
	return tag.RowsAffected(), err
}

// crWindowsAndPolicyChecks exercises every RLS policy of contract §4 as commerce_retention_writer.
func (e *crEnv) writerPolicyBehaviour(t *testing.T) {
	w := e.w
	ctx := context.Background()
	const W = "commerce_retention_writer"
	s := w.session(t, w.store)
	w.closedAt(t, s, crAgo(1, 0))
	m := w.source(t, s, "page")
	live := w.bundle(t, s, "manual", "", "wp-live-"+t04Tag())
	w.bind(t, s, live)
	offer, sku := w.offer(t, s)
	w.line(t, s, live, offer, sku, 1)
	w.link(t, s, live, `(clock_timestamp() + interval '1 hour')`)
	done := w.bundle(t, s, "manual", "", "wp-done-"+t04Tag())
	mustExec(t, w.owner, `UPDATE claims.bundles SET purged_at=clock_timestamp(),updated_at=clock_timestamp() WHERE id=$1`, done.id)
	applied, pending := m.asset+"_"+crDigits(9), m.asset+"_"+crDigits(9)
	w.intake(t, m, applied, crHex64(), "APPLIED", crAgo(40, 0), "")
	w.intake(t, m, pending, crHex64(), "PENDING", crAgo(40, 0), "")
	opTerminal := w.operation(t, m, live, m.asset+"_"+crDigits(9), "SUCCEEDED", crAgo(40, 0))
	opReady := w.operation(t, m, live, m.asset+"_"+crDigits(9), "READY", crAgo(40, 0))
	opOther := w.operationAs(t, m, live, m.asset+"_"+crDigits(9), "SUCCEEDED", crAgo(40, 0), "meta.other_action")
	lane := e.socialLane(w.store)
	cmt := e.commentEvent(lane, lane.asset+"_"+crDigits(10), crDigits(9), "add", crAgo(40, 0), "completed", 0)
	msg := e.message(lane, crHex64(), crAgo(40, 0), "completed")
	var conv string
	if err := w.owner.QueryRow(ctx, `SELECT conversation_id::text FROM social.messages WHERE event_id=$1`, msg.id).Scan(&conv); err != nil {
		t.Fatal(err)
	}
	mustExec(t, w.owner, `INSERT INTO claims.retention_log(kind,counts) VALUES('run','{"links":0}')`)
	var policyLog string
	if err := w.owner.QueryRow(ctx, `INSERT INTO claims.retention_log(kind,counts) VALUES('policy_set','{"enforced":0}') RETURNING id::text`).Scan(&policyLog); err != nil {
		t.Fatal(err)
	}
	w.logs = append(w.logs, policyLog)

	type c struct {
		name  string
		sql   string
		args  []any
		rows  int64  // expected rows affected when state == ""
		state string // expected SQLSTATE ("" = success)
	}
	purge := `UPDATE claims.bundles SET actor_key=$2,label='purged-'||replace(gen_random_uuid()::text,'-',''),owner_id=NULL,bound_at=NULL,purged_at=clock_timestamp(),updated_at=clock_timestamp() WHERE id=$1`
	cases := []c{
		// claims.bundles: column grants + read true + update USING purged_at IS NULL WITH CHECK purged_at IS NOT NULL AND owner_id IS NULL
		{"bundles: granted columns readable", `SELECT tenant_id,store_id,id,session_id,platform,actor_key,label,owner_id,purged_at FROM claims.bundles WHERE id=$1`, []any{live.id}, 1, ""},
		{"bundles: ungranted column line_count", `SELECT line_count FROM claims.bundles WHERE id=$1`, []any{live.id}, 0, "42501"},
		{"bundles: SELECT * needs every column", `SELECT * FROM claims.bundles WHERE id=$1`, []any{live.id}, 0, "42501"},
		{"bundles: de-identify (RD1 shape) allowed", purge, []any{live.id, crHex64()}, 1, ""},
		{"bundles: purge that keeps the binding", `UPDATE claims.bundles SET purged_at=clock_timestamp(),label='purged-'||replace(gen_random_uuid()::text,'-','') WHERE id=$1`, []any{live.id}, 0, "42501"},
		{"bundles: relabel without purging (WITH CHECK purged_at)", `UPDATE claims.bundles SET label='renamed' WHERE id=$1`, []any{live.id}, 0, "42501"},
		{"bundles: purged row is invisible to UPDATE (USING purged_at IS NULL)", purge, []any{done.id, crHex64()}, 0, ""},
		{"bundles: ungranted column write", `UPDATE claims.bundles SET line_count=3 WHERE id=$1`, []any{live.id}, 0, "42501"},
		{"bundles: no DELETE", `DELETE FROM claims.bundles WHERE id=$1`, []any{live.id}, 0, "42501"},
		{"bundles: no INSERT", `INSERT INTO claims.bundles(tenant_id,store_id,session_id,platform,actor_key,label) VALUES($1,$2,$3,'manual',$4,'x')`, []any{w.tenant, w.store, s.id, crHex64()}, 0, "42501"},
		// claims.lines: update WITH CHECK applied_version IS NULL
		{"lines: applied_version := NULL", `UPDATE claims.lines SET applied_version=NULL WHERE bundle_id=$1`, []any{live.id}, 1, ""},
		{"lines: applied_version := 1 refused", `UPDATE claims.lines SET applied_version=1 WHERE bundle_id=$1`, []any{live.id}, 0, "42501"},
		{"lines: quantity not writable", `UPDATE claims.lines SET quantity=2 WHERE bundle_id=$1`, []any{live.id}, 0, "42501"},
		{"lines: quantity not readable", `SELECT quantity FROM claims.lines WHERE bundle_id=$1`, []any{live.id}, 0, "42501"},
		// claims.links: read/delete true, lock-only update
		{"links: FOR UPDATE lock works", `SELECT bundle_id FROM claims.links WHERE bundle_id=$1 FOR UPDATE`, []any{live.id}, 1, ""},
		{"links: lock-only UPDATE cannot change a value", `UPDATE claims.links SET expires_at=expires_at+interval '1 day' WHERE bundle_id=$1`, []any{live.id}, 0, "42501"},
		{"links: lock-only UPDATE cannot even rewrite the same value", `UPDATE claims.links SET expires_at=expires_at WHERE bundle_id=$1`, []any{live.id}, 0, "42501"},
		{"links: DELETE", `DELETE FROM claims.links WHERE bundle_id=$1`, []any{live.id}, 1, ""},
		{"links: token_hash unreadable", `SELECT token_hash FROM claims.links WHERE bundle_id=$1`, []any{live.id}, 0, "42501"},
		// claims.meta_intake: delete USING state<>'PENDING', lock-only update
		{"intake: delete terminal row", `DELETE FROM claims.meta_intake WHERE comment_ref=$1`, []any{applied}, 1, ""},
		{"intake: PENDING row is undeletable (USING state<>'PENDING')", `DELETE FROM claims.meta_intake WHERE comment_ref=$1`, []any{pending}, 0, ""},
		{"intake: FOR UPDATE lock works", `SELECT id FROM claims.meta_intake WHERE comment_ref=$1 FOR UPDATE`, []any{applied}, 1, ""},
		{"intake: lock-only UPDATE", `UPDATE claims.meta_intake SET updated_at=clock_timestamp() WHERE comment_ref=$1`, []any{applied}, 0, "42501"},
		{"intake: ungranted column", `SELECT attempts FROM claims.meta_intake WHERE comment_ref=$1`, []any{applied}, 0, "42501"},
		// live.claim_windows: read true, lock-only update
		{"windows: FOR SHARE lock works", `SELECT state,closed_at FROM live.claim_windows WHERE session_id=$1 FOR SHARE`, []any{s.id}, 1, ""},
		{"windows: lock-only UPDATE", `UPDATE live.claim_windows SET updated_at=clock_timestamp() WHERE session_id=$1`, []any{s.id}, 0, "42501"},
		{"windows: state not writable", `UPDATE live.claim_windows SET state='OPEN' WHERE session_id=$1`, []any{s.id}, 0, "42501"},
		// integration.operations: read USING action='meta.private_reply'; update terminal-only with redaction WITH CHECK
		{"operations: private-reply ledger readable", `SELECT id,request,state FROM integration.operations WHERE id=$1`, []any{opTerminal}, 1, ""},
		{"operations: other actions invisible (USING action=meta.private_reply)", `UPDATE integration.operations SET updated_at=clock_timestamp() WHERE id=$1`, []any{opOther}, 0, ""},
		{"operations: other actions unreadable", `SELECT id FROM integration.operations WHERE id=$1`, []any{opOther}, 0, ""},
		{"operations: redact a terminal op", `UPDATE integration.operations SET request=(request-'comment_ref')||'{"redacted":true}'::jsonb,semantic_key='mpr-purged:'||id::text WHERE id=$1`, []any{opTerminal}, 1, ""},
		{"operations: non-terminal op is not updatable", `UPDATE integration.operations SET request=(request-'comment_ref')||'{"redacted":true}'::jsonb,semantic_key='mpr-purged:'||id::text WHERE id=$1`, []any{opReady}, 0, ""},
		{"operations: update that keeps comment_ref refused", `UPDATE integration.operations SET semantic_key='mpr-purged:'||id::text WHERE id=$1`, []any{opTerminal}, 0, "42501"},
		{"operations: update with a foreign semantic_key refused", `UPDATE integration.operations SET request=(request-'comment_ref')||'{"redacted":true}'::jsonb,semantic_key='other-key-'||id::text WHERE id=$1`, []any{opTerminal}, 0, "42501"},
		{"operations: state not writable", `UPDATE integration.operations SET state='CANCELLED' WHERE id=$1`, []any{opTerminal}, 0, "42501"},
		// social.*: delete true, lock-only update
		{"comment_events: DELETE", `DELETE FROM social.comment_events WHERE event_id=$1`, []any{cmt.id}, 1, ""},
		{"comment_events: lock-only UPDATE", `UPDATE social.comment_events SET received_at=received_at WHERE event_id=$1`, []any{cmt.id}, 0, "42501"},
		{"messages: DELETE", `DELETE FROM social.messages WHERE event_id=$1`, []any{msg.id}, 1, ""},
		{"messages: lock-only UPDATE", `UPDATE social.messages SET received_at=received_at WHERE event_id=$1`, []any{msg.id}, 0, "42501"},
		{"conversations: FOR UPDATE lock works", `SELECT id FROM social.conversations WHERE id=$1 FOR UPDATE`, []any{conv}, 1, ""},
		{"conversations: lock-only UPDATE", `UPDATE social.conversations SET next_seq=next_seq WHERE id=$1`, []any{conv}, 0, "42501"},
		{"conversations: DELETE", `DELETE FROM social.conversations WHERE id=$1`, []any{randomUUID()}, 0, ""},
		{"messages: ciphertext readable only as part of a table SELECT grant", `SELECT ciphertext FROM social.messages WHERE event_id=$1`, []any{msg.id}, 1, ""},
		// retention tables
		{"policy: update the granted columns", `UPDATE claims.retention_policy SET enforced=enforced,version=version`, nil, 1, ""},
		{"retention_log: INSERT", `INSERT INTO claims.retention_log(kind,counts) VALUES('run','{}')`, nil, 1, ""},
		{"retention_log: DELETE a run row", `DELETE FROM claims.retention_log WHERE kind='run' AND counts='{"links":0}'::jsonb`, nil, 1, ""},
		{"retention_log: a policy_set row is undeletable (USING kind='run')", `DELETE FROM claims.retention_log WHERE id=$1`, []any{policyLog}, 0, ""},
		{"retention_log: UPDATE refused (no privilege)", `UPDATE claims.retention_log SET counts='{}' WHERE id=$1`, []any{policyLog}, 0, "42501"},
		// everything else stays closed
		{"claims.events unreadable", `SELECT count(*) FROM claims.events`, nil, 0, "42501"},
		{"live.offers unreadable", `SELECT count(*) FROM live.offers`, nil, 0, "42501"},
		{"river.river_job unreadable", `SELECT count(*) FROM river.river_job`, nil, 0, "42501"},
		{"meta_inbox.events unreadable", `SELECT count(*) FROM meta_inbox.events`, nil, 0, "42501"},
		{"buyer.owners unreadable", `SELECT count(*) FROM buyer.owners`, nil, 0, "42501"},
		{"meta_inbox.lock_purgeable callable", `SELECT meta_inbox.lock_purgeable($1)`, []any{cmt.id}, 1, ""},
		{"meta_inbox.purgeable not callable", `SELECT meta_inbox.purgeable($1)`, []any{cmt.id}, 0, "42501"},
		{"meta_inbox.purge_expired not callable", `SELECT meta_inbox.purge_expired(1)`, nil, 0, "42501"},
	}
	for _, x := range cases {
		n, err := crAs(t, w.owner, W, x.sql, x.args...)
		switch {
		case x.state != "" && crSQLState(err) != x.state:
			t.Errorf("writer %s: err=%v (SQLSTATE %q), want SQLSTATE %s", x.name, err, crSQLState(err), x.state)
		case x.state == "" && err != nil:
			t.Errorf("writer %s: %v", x.name, err)
		case x.state == "" && strings.HasPrefix(x.sql, "SELECT") && n != x.rows:
			t.Errorf("writer %s: %d rows, want %d", x.name, n, x.rows)
		case x.state == "" && !strings.HasPrefix(x.sql, "SELECT") && n != x.rows:
			t.Errorf("writer %s: %d rows affected, want %d", x.name, n, x.rows)
		}
	}
}

// crValidators pairs every platform pool validator with the authority role it admits, for the "every other
// validator rejects a login that reaches a retention role" clause.
var crValidators = []struct {
	name, role string
	validate   func(context.Context, *pgxpool.Pool) error
}{
	{"worker", "commerce_worker", platform.ValidateWorkerPool},
	{"checkout", "commerce_checkout_runtime", platform.ValidateCheckoutPool},
	{"buyer", "commerce_buyer_runtime", platform.ValidateBuyerPool},
	{"buyer issuer", "commerce_buyer_issuer", platform.ValidateBuyerIssuerPool},
	{"meta ingress", "commerce_meta_ingress", platform.ValidateMetaIngressPool},
	{"meta consumer", "commerce_meta_consumer", platform.ValidateMetaConsumerPool},
	{"meta worker", "commerce_meta_worker", platform.ValidateMetaWorkerPool},
	{"claims intake", "commerce_claims_intake", platform.ValidateClaimsIntakePool},
	{"media worker", "commerce_media_worker", platform.ValidateMediaWorkerPool},
	{"media executor", "commerce_media_executor", platform.ValidateMediaExecutorPool},
	{"media recovery", "commerce_media_recovery", platform.ValidateMediaRecoveryPool},
	{"stripe ingress", "commerce_stripe_ingress", platform.ValidateStripeIngressPool},
}

// CRP02 (REAL_PG): contract §2/§3/§4 and the schema rows of §7. Subtests:
//   - fresh-database: the shared fixture is a fresh database migrated twice; role attributes, the §4 matrix in both
//     directions (column/table privileges, schema USAGE, EXECUTE, direct + inherited, for the roles and for their logins),
//     FORCE RLS + policy set, definer owner/prosecdef/proconfig, COMMENT ON, objects, D4 label CHECK.
//   - non-retention-roles-denied: 42501 for every other role on the new tables and functions (catalog and behaviour).
//   - writer-policy-behaviour: every RLS policy of §4 exercised as commerce_retention_writer (lock-only UPDATE cannot
//     change a value, PENDING intake undeletable, redaction shape, ...).
//   - pool-validators: dedicated logins pass; mixed / SET ROLE / owner-reachable logins are rejected by both retention
//     validators; every other validator rejects a login that reaches a retention role.
//   - constraints-and-policy-api: retention_log/retention_policy CHECKs, set_retention_policy/retention_status.
//   - populated-upgrade: a separate PG cluster at the pre-0071 schema with populated claims rows: preconditions (55000
//     without 0029/0060/0064 objects), a non-purged reserved label stops the migration (55000, nothing applied), then
//     0071 applies, twice, without changing populated data, and the same matrix holds.
func TestClaimsRetentionCRP02Schema(t *testing.T) {
	f := fixture(t)
	ctx := context.Background()

	t.Run("fresh-database", func(t *testing.T) {
		e := crSetup(t)
		if n := crCount(t, f.owner, `SELECT count(*) FROM public.lc_schema_migrations WHERE version='0071_claims_retention.sql'`); n != 1 {
			t.Fatalf("0071 in the migration ledger: %d rows (the fixture applied every migration twice)", n)
		}
		crAssertRoles(t, f.owner, "fresh")
		crAssertMatrix(t, f.owner, "fresh")
		crAssertDefiners(t, f.owner, "fresh")
		crAssertRLS(t, f.owner, "fresh")
		crAssertObjects(t, f.owner, "fresh")
		// the policy row: exactly one, unenforced defaults (RD2)
		var rows, link, intake, claimsD, social int
		var enforced bool
		if err := f.owner.QueryRow(ctx, `SELECT count(*),bool_or(enforced),max(link_days),max(intake_days),max(claims_days),max(social_days) FROM claims.retention_policy`).Scan(&rows, &enforced, &link, &intake, &claimsD, &social); err != nil {
			t.Fatal(err)
		}
		if rows != 1 || enforced || link != 7 || intake != 30 || claimsD != 90 || social != 30 {
			t.Errorf("policy row after reset: rows=%d enforced=%t %d/%d/%d/%d, want one unenforced 7/30/90/30", rows, enforced, link, intake, claimsD, social)
		}
		// logins hold exactly their role's authority, nothing else (direct + inherited)
		for pool, role := range map[*pgxpool.Pool]string{e.job: "commerce_retention_job", e.op: "commerce_retention_operator"} {
			login := e.user(pool)
			if got, want := crStrip(crMatrixOf(t, f.owner, login)), crStrip(crMatrixOf(t, f.owner, role)); len(crDiff(got, want))+len(crDiff(want, got)) != 0 {
				t.Errorf("login of %s differs from its role: extra=%s missing=%s", role, crHead(crDiff(got, want)), crHead(crDiff(want, got)))
			}
		}
		// D4 / B23: the reserved-label CHECK admits a bundle's OWN erased-<id hex> and nothing else of the reserved form
		s := e.w.session(t, f.storeA1)
		x, y := e.w.bundle(t, s, "manual", "", "d4-x-"+t04Tag()), e.w.bundle(t, s, "manual", "", "d4-y-"+t04Tag())
		own := "erased-" + strings.ReplaceAll(x.id, "-", "")
		if _, err := f.owner.Exec(ctx, `UPDATE claims.bundles SET label=$1 WHERE id=$2`, own, x.id); err != nil {
			t.Errorf("own-id erased label (customers.apply_erasure's form, ruling B23): %v", err)
		}
		other := "erased-" + strings.ReplaceAll(y.id, "-", "")
		if _, err := f.owner.Exec(ctx, `UPDATE claims.bundles SET label=$1 WHERE id=$2`, other, x.id); crSQLState(err) != "23514" {
			t.Errorf("erased-<other bundle id hex> on X: %v, want 23514", err)
		}
		if _, err := f.owner.Exec(ctx, `UPDATE claims.bundles SET label=$1 WHERE id=$2`, "purged-"+strings.ReplaceAll(x.id, "-", ""), x.id); crSQLState(err) != "23514" {
			t.Errorf("purged-<own id hex> (only erased- has the own-id exception): %v, want 23514", err)
		}
		var apply bool
		if err := f.owner.QueryRow(ctx, `SELECT to_regproc('customers.apply_erasure') IS NOT NULL`).Scan(&apply); err != nil {
			t.Fatal(err)
		}
		t.Logf("customers.apply_erasure (0078) present in this lane: %t; CB05 is exercised by the customers-billing lane (NOT_RUN here)", apply)
	})

	t.Run("non-retention-roles-denied", func(t *testing.T) {
		e := crSetup(t)
		empty := "crp_empty_" + t04Tag()
		mustExec(t, f.owner, `CREATE ROLE `+empty+` NOLOGIN`)
		t.Cleanup(func() { _, _ = f.owner.Exec(ctx, `DROP ROLE `+empty) })
		rows, err := f.owner.Query(ctx, `SELECT rolname FROM pg_roles WHERE rolname LIKE 'commerce\_%' AND rolname NOT LIKE 'commerce\_retention\_%' ORDER BY 1`)
		if err != nil {
			t.Fatal(err)
		}
		others, err := pgx.CollectRows(rows, pgx.RowTo[string])
		if err != nil || len(others) < 10 {
			t.Fatalf("other commerce roles: %v %v", others, err)
		}
		others = append(others, empty, "commerce_runtime")
		fns := []string{"run_retention", "retention_status", "erase_actor", "set_retention_policy", "replay_actor_erasures", "apply_actor_erasure", "links_not_purged"}
		stmts := []string{
			`SELECT claims.run_retention(1)`,
			`SELECT claims.retention_status()`,
			`SELECT claims.erase_actor(gen_random_uuid(),NULL,NULL,NULL,NULL,gen_random_uuid(),gen_random_uuid(),gen_random_uuid(),NULL)`,
			`SELECT claims.set_retention_policy(1,false,7,30,90,30)`,
			`SELECT claims.replay_actor_erasures(NULL)`,
			`SELECT claims.apply_actor_erasure('facebook',repeat('a',64),NULL,NULL,NULL,NULL)`,
			`SELECT * FROM claims.retention_policy`,
			`SELECT * FROM claims.retention_log`,
			`INSERT INTO claims.retention_log(kind,counts) VALUES('run','{}')`,
			`UPDATE claims.retention_policy SET enforced=true`,
			`DELETE FROM claims.retention_log`,
		}
		for _, role := range others {
			for _, fn := range fns {
				if crCount(t, f.owner, `SELECT count(*) FROM pg_proc p JOIN pg_namespace n ON n.oid=p.pronamespace WHERE n.nspname='claims' AND p.proname=$1 AND has_function_privilege($2,p.oid,'EXECUTE') AND pg_get_userbyid(p.proowner)<>$2`, fn, role) != 0 {
					t.Errorf("role %s has EXECUTE on claims.%s (catalog)", role, fn)
				}
			}
			for _, tb := range []string{"claims.retention_policy", "claims.retention_log"} {
				for _, priv := range []string{"SELECT", "INSERT", "UPDATE", "DELETE", "TRUNCATE", "REFERENCES", "TRIGGER"} {
					if crCount(t, f.owner, `SELECT CASE WHEN has_table_privilege($1,$2::regclass,$3) THEN 1 ELSE 0 END`, role, tb, priv) != 0 {
						t.Errorf("role %s has %s on %s (catalog)", role, priv, tb)
					}
				}
			}
			for _, q := range stmts {
				if _, err := crAs(t, f.owner, role, q); crSQLState(err) != "42501" {
					t.Errorf("role %s: %.60s -> %v (SQLSTATE %q), want 42501", role, q, err, crSQLState(err))
				}
			}
		}
		// real logins of the two retention roles cannot use each other's or the writer's authority
		for _, q := range []string{`SELECT claims.erase_actor(gen_random_uuid(),NULL,NULL,NULL,NULL,gen_random_uuid(),gen_random_uuid(),gen_random_uuid(),NULL)`,
			`SELECT claims.set_retention_policy(1,false,7,30,90,30)`, `SELECT claims.replay_actor_erasures(NULL)`,
			`SELECT claims.apply_actor_erasure('facebook',repeat('a',64),NULL,NULL,NULL,NULL)`, `SELECT * FROM claims.retention_policy`, `SELECT * FROM claims.retention_log`,
			`SELECT count(*) FROM claims.bundles`, `SELECT count(*) FROM claims.meta_intake`, `SELECT count(*) FROM social.messages`} {
			if _, err := e.job.Exec(ctx, q); crSQLState(err) != "42501" {
				t.Errorf("job login: %.60s -> %v, want 42501", q, err)
			}
		}
		for _, q := range []string{`SELECT * FROM claims.retention_policy`, `SELECT * FROM claims.retention_log`, `UPDATE claims.retention_policy SET enforced=true`,
			`INSERT INTO claims.retention_log(kind,counts) VALUES('run','{}')`, `SELECT claims.apply_actor_erasure('facebook',repeat('a',64),NULL,NULL,NULL,NULL)`,
			`SELECT count(*) FROM claims.bundles`, `SELECT count(*) FROM claims.meta_intake`, `SELECT count(*) FROM social.messages`, `SELECT count(*) FROM integration.operations`} {
			if _, err := e.op.Exec(ctx, q); crSQLState(err) != "42501" {
				t.Errorf("operator login: %.60s -> %v, want 42501", q, err)
			}
		}
		// the runtime pool of the fixture (a real commerce_runtime login) as well
		for _, q := range stmts {
			if _, err := f.runtime.Exec(ctx, q); crSQLState(err) != "42501" {
				t.Errorf("runtime login: %.60s -> %v, want 42501", q, err)
			}
		}
	})

	t.Run("writer-policy-behaviour", func(t *testing.T) {
		e := crSetup(t)
		e.writerPolicyBehaviour(t)
	})

	t.Run("pool-validators", func(t *testing.T) {
		e := crSetup(t)
		if err := platform.ValidateRetentionJobPool(ctx, e.job); err != nil {
			t.Errorf("dedicated job login rejected: %v", err)
		}
		if err := platform.ValidateRetentionOperatorPool(ctx, e.op); err != nil {
			t.Errorf("dedicated operator login rejected: %v", err)
		}
		if err := platform.ValidateRetentionOperatorPool(ctx, e.job); err == nil {
			t.Error("operator validator accepted the job login")
		}
		if err := platform.ValidateRetentionJobPool(ctx, e.op); err == nil {
			t.Error("job validator accepted the operator login")
		}
		type poolCase struct {
			name string
			mk   func(t *testing.T) *pgxpool.Pool
		}
		mixed := func(setRole bool, roles ...string) func(t *testing.T) *pgxpool.Pool {
			return func(t *testing.T) *pgxpool.Pool { return mciMixedPool(t, f, setRole, roles...) }
		}
		shared := func(p *pgxpool.Pool) func(t *testing.T) *pgxpool.Pool {
			return func(*testing.T) *pgxpool.Pool { return p }
		}
		jobRejects := []poolCase{
			{"job + operator (two retention authorities)", mixed(false, "commerce_retention_job", "commerce_retention_operator")},
			{"job + worker", mixed(false, "commerce_retention_job", "commerce_worker")},
			{"job + runtime", mixed(false, "commerce_retention_job", "commerce_runtime")},
			{"job + claims intake", mixed(false, "commerce_retention_job", "commerce_claims_intake")},
			{"job + meta consumer", mixed(false, "commerce_retention_job", "commerce_meta_consumer")},
			{"job + integration writer", mixed(false, "commerce_retention_job", "commerce_integration_writer")},
			{"job + definer owner", mixed(false, "commerce_retention_job", "commerce_retention_writer")},
			{"job with SET ROLE", mixed(true, "commerce_retention_job")},
			{"definer owner alone", mixed(false, "commerce_retention_writer")},
			{"definer owner with SET", mixed(true, "commerce_retention_writer")},
			{"another authority (worker)", mixed(false, "commerce_worker")},
			{"the runtime pool", shared(f.runtime)},
			{"the owner pool", shared(f.owner)},
		}
		for _, c := range jobRejects {
			t.Run("job validator rejects "+c.name, func(t *testing.T) {
				if err := platform.ValidateRetentionJobPool(ctx, c.mk(t)); err == nil {
					t.Errorf("ValidateRetentionJobPool accepted: %s", c.name)
				}
			})
		}
		operatorRejects := []poolCase{
			{"operator + job", mixed(false, "commerce_retention_operator", "commerce_retention_job")},
			{"operator + worker", mixed(false, "commerce_retention_operator", "commerce_worker")},
			{"operator + runtime", mixed(false, "commerce_retention_operator", "commerce_runtime")},
			{"operator + meta worker", mixed(false, "commerce_retention_operator", "commerce_meta_worker")},
			{"operator + definer owner", mixed(false, "commerce_retention_operator", "commerce_retention_writer")},
			{"operator with SET ROLE", mixed(true, "commerce_retention_operator")},
			{"definer owner alone", mixed(false, "commerce_retention_writer")},
			{"another authority (worker)", mixed(false, "commerce_worker")},
			{"the runtime pool", shared(f.runtime)},
			{"the owner pool", shared(f.owner)},
		}
		for _, c := range operatorRejects {
			t.Run("operator validator rejects "+c.name, func(t *testing.T) {
				if err := platform.ValidateRetentionOperatorPool(ctx, c.mk(t)); err == nil {
					t.Errorf("ValidateRetentionOperatorPool accepted: %s", c.name)
				}
			})
		}
		// Every other validator rejects a login that can also reach a retention role. The baseline (the login with only its own
		// authority) must pass first, otherwise the rejection would prove nothing.
		for _, v := range crValidators {
			t.Run("validator "+v.name+" rejects retention reach", func(t *testing.T) {
				if err := v.validate(ctx, mciMixedPool(t, f, false, v.role)); err != nil {
					t.Fatalf("validator %s does not admit the plain %s login on this database (%v): the rejection below would prove nothing", v.name, v.role, err)
				}
				for _, r := range []string{"commerce_retention_job", "commerce_retention_operator", "commerce_retention_writer"} {
					if err := v.validate(ctx, mciMixedPool(t, f, false, v.role, r)); err == nil {
						t.Errorf("%s validator accepted a %s login that also reaches %s", v.name, v.role, r)
					}
				}
			})
		}
	})

	t.Run("constraints-and-policy-api", func(t *testing.T) {
		e := crSetup(t)
		ins := func(q string, args ...any) error { _, err := f.owner.Exec(ctx, q, args...); return err }
		digest := sha256.Sum256([]byte("x"))
		// retention_log: counts must be an object of numbers only; erasure rows carry request + selector digest and
		// exactly one of actor_digest / bundle_ref; run/policy/replay rows carry neither.
		for name, q := range map[string]struct {
			sql  string
			args []any
		}{
			"string count":                  {`INSERT INTO claims.retention_log(kind,counts) VALUES('run','{"links":"1"}')`, nil},
			"nested object count":           {`INSERT INTO claims.retention_log(kind,counts) VALUES('run','{"links":{"a":1}}')`, nil},
			"array of strings count":        {`INSERT INTO claims.retention_log(kind,counts) VALUES('run','{"links":["x"]}')`, nil},
			"null count":                    {`INSERT INTO claims.retention_log(kind,counts) VALUES('run','{"links":null}')`, nil},
			"boolean count":                 {`INSERT INTO claims.retention_log(kind,counts) VALUES('run','{"more":true}')`, nil},
			"counts not an object":          {`INSERT INTO claims.retention_log(kind,counts) VALUES('run','[1]')`, nil},
			"counts over 1024 bytes":        {`INSERT INTO claims.retention_log(kind,counts) SELECT 'run',jsonb_object_agg('k'||g,g) FROM generate_series(1,300) g`, nil},
			"unknown kind":                  {`INSERT INTO claims.retention_log(kind,counts) VALUES('purge','{}')`, nil},
			"erasure without request":       {`INSERT INTO claims.retention_log(kind,selector_digest,actor_digest,counts) VALUES('actor_erased',$1,$2,'{}')`, []any{digest[:], digest[:]}},
			"erasure without selector":      {`INSERT INTO claims.retention_log(kind,request_id,actor_digest,counts) VALUES('actor_erased',gen_random_uuid(),$1,'{}')`, []any{digest[:]}},
			"erasure with two subjects":     {`INSERT INTO claims.retention_log(kind,request_id,selector_digest,actor_digest,bundle_tenant,bundle_store,bundle_ref,counts) VALUES('actor_erased',gen_random_uuid(),$1,$1,gen_random_uuid(),gen_random_uuid(),gen_random_uuid(),'{}')`, []any{digest[:]}},
			"erasure with no subject":       {`INSERT INTO claims.retention_log(kind,request_id,selector_digest,counts) VALUES('actor_erased',gen_random_uuid(),$1,'{}')`, []any{digest[:]}},
			"erasure bundle triple partial": {`INSERT INTO claims.retention_log(kind,request_id,selector_digest,bundle_ref,counts) VALUES('actor_erased',gen_random_uuid(),$1,gen_random_uuid(),'{}')`, []any{digest[:]}},
			"actor_digest 31 bytes":         {`INSERT INTO claims.retention_log(kind,request_id,selector_digest,actor_digest,counts) VALUES('actor_erased',gen_random_uuid(),$1,$2,'{}')`, []any{digest[:], digest[:31]}},
			"run row with an actor digest":  {`INSERT INTO claims.retention_log(kind,actor_digest,counts) VALUES('run',$1,'{}')`, []any{digest[:]}},
			"array of numbers count":        {`INSERT INTO claims.retention_log(kind,counts) VALUES('run','{"links":[1]}')`, nil},
			"run row with a request id":     {`INSERT INTO claims.retention_log(kind,request_id,counts) VALUES('run',gen_random_uuid(),'{}')`, nil},
			"replay row with a selector":    {`INSERT INTO claims.retention_log(kind,selector_digest,counts) VALUES('replay',$1,'{}')`, []any{digest[:]}},
			"replay row with a bundle ref":  {`INSERT INTO claims.retention_log(kind,bundle_tenant,bundle_store,bundle_ref,counts) VALUES('replay',gen_random_uuid(),gen_random_uuid(),gen_random_uuid(),'{}')`, nil},
		} {
			if err := ins(q.sql, q.args...); crSQLState(err) != "23514" {
				t.Errorf("retention_log %s: %v, want 23514", name, err)
			}
		}
		req := randomUUID()
		if err := ins(`INSERT INTO claims.retention_log(kind,request_id,selector_digest,actor_digest,counts) VALUES('actor_erased',$1,$2,$2,'{"bundles":1}')`, req, digest[:]); err != nil {
			t.Fatalf("a well-formed actor_erased row: %v", err)
		}
		e.w.logs = append(e.w.logs, "")
		defer func() { _, _ = f.owner.Exec(ctx, `DELETE FROM claims.retention_log WHERE request_id=$1`, req) }()
		if err := ins(`INSERT INTO claims.retention_log(kind,request_id,selector_digest,actor_digest,counts) VALUES('actor_erased',$1,$2,$2,'{}')`, req, digest[:]); crSQLState(err) != "23505" {
			t.Errorf("two actor_erased rows for one request id: %v, want 23505", err)
		}
		// retention_policy: one row only; bounds; social_days <= intake_days (F4)
		for name, q := range map[string]string{
			"second row":                `INSERT INTO claims.retention_policy(id) VALUES(false)`,
			"link_days 0":               `UPDATE claims.retention_policy SET link_days=0`,
			"link_days 366":             `UPDATE claims.retention_policy SET link_days=366`,
			"intake_days 7 (RD5 min 8)": `UPDATE claims.retention_policy SET intake_days=7,social_days=7`,
			"intake_days 3651":          `UPDATE claims.retention_policy SET intake_days=3651`,
			"claims_days 7":             `UPDATE claims.retention_policy SET claims_days=7`,
			"social_days 7":             `UPDATE claims.retention_policy SET social_days=7`,
			"social_days > intake_days": `UPDATE claims.retention_policy SET intake_days=20,social_days=21`,
			"version 0":                 `UPDATE claims.retention_policy SET version=0`,
			"NULL enforced":             `UPDATE claims.retention_policy SET enforced=NULL`,
		} {
			err := ins(q)
			if st := crSQLState(err); st != "23514" && st != "23505" && st != "23502" {
				t.Errorf("retention_policy %s: %v, want a constraint violation", name, err)
			}
		}
		if n := crCount(t, f.owner, `SELECT count(*) FROM claims.retention_policy`); n != 1 {
			t.Errorf("retention_policy has %d rows", n)
		}
		// set_retention_policy: 22023 -> ErrUsage, CAS conflict PT409 -> ErrConflict, +1 version, audited as numbers
		st, err := retention.GetStatus(ctx, e.op)
		if err != nil {
			t.Fatal(err)
		}
		good := retention.Policy{Enforced: false, LinkDays: 10, IntakeDays: 40, ClaimsDays: 100, SocialDays: 40}
		for name, p := range map[string]retention.Policy{
			"link 0": {LinkDays: 0, IntakeDays: 40, ClaimsDays: 100, SocialDays: 40}, "link 366": {LinkDays: 366, IntakeDays: 40, ClaimsDays: 100, SocialDays: 40},
			"intake 7": {LinkDays: 7, IntakeDays: 7, ClaimsDays: 100, SocialDays: 7}, "claims 7": {LinkDays: 7, IntakeDays: 40, ClaimsDays: 7, SocialDays: 40},
			"social 41 > intake 40": {LinkDays: 7, IntakeDays: 40, ClaimsDays: 100, SocialDays: 41}, "intake 3651": {LinkDays: 7, IntakeDays: 3651, ClaimsDays: 100, SocialDays: 40},
			"all zero": {},
		} {
			if _, err := retention.SetPolicy(ctx, e.op, st.Version, p); !errors.Is(err, retention.ErrUsage) {
				t.Errorf("SetPolicy %s: %v, want ErrUsage (22023)", name, err)
			}
		}
		before := crLogCount(t, f.owner, "policy_set")
		v2, err := retention.SetPolicy(ctx, e.op, st.Version, good)
		if err != nil || v2 != st.Version+1 {
			t.Fatalf("SetPolicy: version %d -> %d err=%v", st.Version, v2, err)
		}
		if _, err := retention.SetPolicy(ctx, e.op, st.Version, good); !errors.Is(err, retention.ErrConflict) {
			t.Errorf("SetPolicy with a stale version: %v, want ErrConflict (PT409)", err)
		}
		if crLogCount(t, f.owner, "policy_set") != before+1 {
			t.Error("policy change is not audited exactly once")
		}
		var counts string
		var by string
		if err := f.owner.QueryRow(ctx, `SELECT counts::text,executed_by FROM claims.retention_log WHERE kind='policy_set' ORDER BY created_at DESC,id LIMIT 1`).Scan(&counts, &by); err != nil {
			t.Fatal(err)
		}
		var parsed map[string]any
		if err := json.Unmarshal([]byte(counts), &parsed); err != nil || len(parsed) < 5 {
			t.Errorf("policy_set counts %s (%v), want the six values as numbers", counts, err)
		}
		for k, v := range parsed {
			if _, num := v.(float64); !num {
				t.Errorf("policy_set counts[%s]=%v is not a number", k, v)
			}
		}
		if by != e.user(e.op) {
			t.Errorf("policy_set executed_by=%s, want the operator login", by)
		}
		now, err := retention.GetStatus(ctx, e.job) // the job login reads status (EXECUTE retention_status only)
		if err != nil {
			t.Fatal(err)
		}
		if now.Version != v2 || now.LinkDays != 10 || now.IntakeDays != 40 || now.ClaimsDays != 100 || now.SocialDays != 40 || now.Enforced {
			t.Errorf("status after SetPolicy: %+v", now)
		}
		var updatedBy string
		if err := f.owner.QueryRow(ctx, `SELECT updated_by FROM claims.retention_policy`).Scan(&updatedBy); err != nil || updatedBy != e.user(e.op) {
			t.Errorf("updated_by=%q (%v), want the operator login (session_user)", updatedBy, err)
		}
		// definer argument checks reachable only by raw SQL (Go validates first): run_retention range, NULL policy args
		for _, q := range []string{`SELECT claims.run_retention(0)`, `SELECT claims.run_retention(1001)`, `SELECT claims.run_retention(NULL)`} {
			if _, err := e.op.Exec(ctx, q); crSQLState(err) != "22023" {
				t.Errorf("%s: %v, want 22023", q, err)
			}
		}
		for _, q := range []string{`SELECT claims.set_retention_policy(NULL,false,7,30,90,30)`, `SELECT claims.set_retention_policy(1,NULL,7,30,90,30)`, `SELECT claims.set_retention_policy(1,false,NULL,30,90,30)`} {
			if _, err := e.op.Exec(ctx, q); crSQLState(err) != "22023" {
				t.Errorf("%s: %v, want 22023", q, err)
			}
		}
		// erase_actor: exactly one selector, well-formed values, else 22023 (raw SQL bypasses the Go shape checks)
		u := randomUUID()
		for name, q := range map[string]string{
			"no selector":       `SELECT claims.erase_actor('` + u + `',NULL,NULL,NULL,NULL,NULL,NULL,NULL,NULL)`,
			"a and b":           `SELECT claims.erase_actor('` + u + `','page','1','` + crpHex64ForSQL + `','1_2',NULL,NULL,NULL,NULL)`,
			"a and c":           `SELECT claims.erase_actor('` + u + `','page','1','` + crpHex64ForSQL + `',NULL,gen_random_uuid(),gen_random_uuid(),gen_random_uuid(),NULL)`,
			"b and c":           `SELECT claims.erase_actor('` + u + `','page','1',NULL,'1_2',gen_random_uuid(),gen_random_uuid(),gen_random_uuid(),NULL)`,
			"key not 64 hex":    `SELECT claims.erase_actor('` + u + `','page','1','abc',NULL,NULL,NULL,NULL,NULL)`,
			"9 peer keys":       `SELECT claims.erase_actor('` + u + `','page','1','` + crpHex64ForSQL + `',NULL,NULL,NULL,NULL,ARRAY(SELECT repeat('a',64) FROM generate_series(1,9)))`,
			"peer key bad":      `SELECT claims.erase_actor('` + u + `','page','1','` + crpHex64ForSQL + `',NULL,NULL,NULL,NULL,ARRAY['zz'])`,
			"comment ref bad":   `SELECT claims.erase_actor('` + u + `','page','1',NULL,'abc',NULL,NULL,NULL,NULL)`,
			"null request":      `SELECT claims.erase_actor(NULL,'page','1',NULL,'1_2',NULL,NULL,NULL,NULL)`,
			"bundle w/o tenant": `SELECT claims.erase_actor('` + u + `',NULL,NULL,NULL,NULL,NULL,gen_random_uuid(),gen_random_uuid(),NULL)`,
		} {
			if _, err := e.op.Exec(ctx, q); crSQLState(err) != "22023" {
				t.Errorf("erase_actor %s: %v (SQLSTATE %q), want 22023", name, err, crSQLState(err))
			}
		}
	})

	t.Run("populated-upgrade", func(t *testing.T) {
		crUpgradeAndPreconditions(t)
	})
}

// crp07Sentinel is a synthetic password; leak assertions search for it (PROCESS §6).
const crp07Sentinel = "sentinel-crp07-password"

const crpHex64ForSQL = "abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789"

func crLogCount(t *testing.T, pool *pgxpool.Pool, kind string) int {
	t.Helper()
	return crCount(t, pool, `SELECT count(*) FROM claims.retention_log WHERE kind=$1`, kind)
}

// crExplicitDigest fingerprints a table by explicit JSON of each row minus the columns 0071 adds, so a populated
// upgrade can be compared even though ALTER TABLE ADD COLUMN changes t::text.
func crExplicitDigest(t *testing.T, pool *pgxpool.Pool, table string, minus ...string) string {
	t.Helper()
	expr := `(to_jsonb(x)`
	for _, c := range minus {
		expr += ` - '` + c + `'`
	}
	expr += `)`
	var d string
	if err := pool.QueryRow(context.Background(), `SELECT count(*)::text||':'||coalesce(md5(string_agg(`+expr+`::text,E'\n' ORDER BY `+expr+`::text)),'') FROM `+table+` x`).Scan(&d); err != nil {
		t.Fatalf("digest %s: %v", table, err)
	}
	return d
}

// crClusterLogin creates a LOGIN role granted exactly authority in the throw-away cluster and returns its pool.
func crClusterLogin(t *testing.T, owner *pgxpool.Pool, authority string) *pgxpool.Pool {
	t.Helper()
	ctx := context.Background()
	login := "cr_up_" + strings.ReplaceAll(randomUUID(), "-", "")
	password := hex.EncodeToString(randomBytes(24)) // random per run, never a stored secret
	id := pgx.Identifier{login}.Sanitize()
	mustExec(t, owner, `CREATE ROLE `+id+` LOGIN NOSUPERUSER NOBYPASSRLS NOCREATEDB NOCREATEROLE NOREPLICATION PASSWORD '`+password+`'`)
	mustExec(t, owner, `GRANT `+pgx.Identifier{authority}.Sanitize()+` TO `+id+` WITH INHERIT TRUE, SET FALSE`)
	cc := owner.Config().ConnConfig
	u := &url.URL{Scheme: "postgres", User: url.UserPassword(login, password), Host: fmt.Sprintf("%s:%d", cc.Host, cc.Port), Path: "/" + cc.Database, RawQuery: "sslmode=disable"}
	var pool *pgxpool.Pool
	var err error
	switch authority {
	case "commerce_retention_job":
		pool, err = platform.OpenRetentionJobPool(ctx, u.String())
	default:
		pool, err = platform.OpenRetentionOperatorPool(ctx, u.String())
	}
	if err != nil {
		t.Fatalf("open %s login on the upgraded cluster: %v", authority, err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// crUpgradeAndPreconditions is CRP02's populated-upgrade clause on a throw-away PG cluster: everything up to and
// including 0070 (and every migration except 0071) is applied, claims rows are populated, then 0071 is applied.
func crUpgradeAndPreconditions(t *testing.T) {
	ctx := context.Background()
	owner := mciStartPG(t)
	body, err := os.ReadFile("../../migrations/0071_claims_retention.sql")
	if err != nil {
		t.Fatal(err)
	}
	sum := fmt.Sprintf("%x", sha256.Sum256(body))
	mustExec(t, owner, `CREATE TABLE public.lc_schema_migrations (version text PRIMARY KEY, checksum text NOT NULL, applied_at timestamptz NOT NULL DEFAULT now())`)
	mustExec(t, owner, `INSERT INTO public.lc_schema_migrations(version,checksum) VALUES('0071_claims_retention.sql',$1)`, sum) // hold 0071 back
	if err := migrations.Apply(ctx, owner); err != nil {
		t.Fatalf("apply every migration but 0071: %v", err)
	}
	for _, q := range []string{`SELECT to_regclass('claims.retention_policy')::text`, `SELECT to_regclass('claims.retention_log')::text`} {
		var got *string
		if err := owner.QueryRow(ctx, q).Scan(&got); err != nil || got != nil {
			t.Fatalf("pre-0071 database already has %s: %v %v", q, got, err)
		}
	}
	if n := crCount(t, owner, `SELECT count(*) FROM pg_roles WHERE rolname LIKE 'commerce\_retention\_%'`); n != 0 {
		t.Fatalf("pre-0071 database already has %d retention roles", n)
	}
	// populated claims rows (owner-seeded, synthetic): manual + Meta bundles with binding and link, intake, ledger, conversation
	w := crFreshWorld(t, owner)
	s := w.session(t, w.store)
	w.closedAt(t, s, crAgo(120, 0))
	m := w.source(t, s, "page")
	man := w.bundle(t, s, "manual", "", "up-manual-"+t04Tag())
	w.bind(t, s, man)
	w.link(t, s, man, crOld(7))
	facebook := w.bundle(t, s, "facebook", crHex64(), "")
	oddLabel := w.bundle(t, s, "manual", "", "purged-1a2b3c4d") // outside the reserved pattern: must survive the upgrade untouched
	w.intake(t, m, m.asset+"_"+crDigits(9), crHex64(), "APPLIED", crOld(30), "")
	w.operation(t, m, man, m.asset+"_"+crDigits(9), "SUCCEEDED", crOld(30))
	mustExec(t, owner, `INSERT INTO social.conversations(tenant_id,store_id,app_id,object,asset_id,peer_key,created_at) VALUES($1,$2,$3,'page',$4,$5,`+crOld(30)+`)`, w.tenant, w.store, miApp, m.asset, crHex64())
	_ = facebook
	_ = oddLabel
	tables := map[string][]string{"claims.bundles": {"purged_at"}, "claims.links": nil, "claims.lines": nil, "claims.events": nil, "claims.meta_intake": nil,
		"integration.operations": nil, "social.conversations": nil, "live.claim_windows": nil, "live.claim_sources": nil, "buyer.owners": nil}
	snap := func() map[string]string {
		out := map[string]string{}
		for tb, minus := range tables {
			out[tb] = crExplicitDigest(t, owner, tb, minus...)
		}
		return out
	}
	before := snap()
	// 55000 preconditions: each object of 0029/0060/0064 hidden in turn, the migration body must refuse first.
	for _, hide := range []struct{ table, migration string }{
		{"social.comment_events", "0029"}, {"claims.links", "0060"}, {"claims.meta_intake", "0064"}, {"live.claim_window_intervals", "0064"}} {
		tx, err := owner.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := tx.Exec(ctx, `ALTER TABLE `+hide.table+` RENAME TO crp_hidden_`+t04Tag()); err != nil {
			_ = tx.Rollback(ctx)
			t.Fatalf("hide %s: %v", hide.table, err)
		}
		_, err = tx.Exec(ctx, string(body))
		_ = tx.Rollback(ctx)
		if crSQLState(err) != "55000" {
			t.Errorf("0071 without %s (%s object): %v (SQLSTATE %q), want 55000", hide.table, hide.migration, err, crSQLState(err))
		}
	}
	if crSnap := snap(); fmt.Sprint(crSnap) != fmt.Sprint(before) {
		t.Fatal("a refused 0071 changed the populated data")
	}
	// a non-purged bundle already carrying a reserved label stops 0071 (55000), all of it rolled back
	reservedLabel := "purged-" + crHex64()[:32]
	reserved := w.bundle(t, s, "manual", "", reservedLabel)
	mustExec(t, owner, `DELETE FROM public.lc_schema_migrations WHERE version='0071_claims_retention.sql'`)
	applyErr := migrations.Apply(ctx, owner)
	if applyErr == nil || (crSQLState(applyErr) != "55000" && !strings.Contains(applyErr.Error(), "55000")) {
		t.Fatalf("0071 over a reserved-pattern label: %v, want SQLSTATE 55000", applyErr)
	}
	if n := crCount(t, owner, `SELECT count(*) FROM pg_roles WHERE rolname LIKE 'commerce\_retention\_%'`); n != 0 {
		t.Errorf("a refused 0071 left %d retention roles (it must roll back completely)", n)
	}
	var stray *string
	if err := owner.QueryRow(ctx, `SELECT to_regclass('claims.retention_log')::text`).Scan(&stray); err != nil || stray != nil {
		t.Errorf("a refused 0071 left claims.retention_log: %v %v", stray, err)
	}
	if n := crCount(t, owner, `SELECT count(*) FROM public.lc_schema_migrations WHERE version='0071_claims_retention.sql'`); n != 0 {
		t.Error("a refused 0071 is recorded in the migration ledger")
	}
	// the operator renames the row (no auto-relabel of merchant data), the migration then applies
	mustExec(t, owner, `UPDATE claims.bundles SET label='renamed-by-operator' WHERE id=$1`, reserved.id)
	before = snap()
	if err := migrations.Apply(ctx, owner); err != nil {
		t.Fatalf("0071 on the populated database: %v", err)
	}
	afterFirst := crExplicitDigest(t, owner, "public.lc_schema_migrations", "applied_at")
	if err := migrations.Apply(ctx, owner); err != nil {
		t.Fatalf("second Apply: %v", err)
	}
	if crExplicitDigest(t, owner, "public.lc_schema_migrations", "applied_at") != afterFirst {
		t.Error("the second Apply changed the ledger")
	}
	if n := crCount(t, owner, `SELECT count(*) FROM public.lc_schema_migrations WHERE version='0071_claims_retention.sql' AND checksum=$1`, sum); n != 1 {
		t.Errorf("0071 ledger rows with the file checksum: %d", n)
	}
	after := snap()
	for tb, d := range before {
		if after[tb] != d {
			t.Errorf("the 0071 upgrade changed populated data in %s", tb)
		}
	}
	if n := crCount(t, owner, `SELECT count(*) FROM claims.bundles WHERE purged_at IS NOT NULL`); n != 0 {
		t.Errorf("the upgrade purged %d bundles (report-only until enforced)", n)
	}
	if n := crCount(t, owner, `SELECT count(*) FROM claims.retention_log`); n != 0 {
		t.Errorf("retention_log has %d rows after the upgrade", n)
	}
	var rows int
	var enforced bool
	if err := owner.QueryRow(ctx, `SELECT count(*),bool_or(enforced) FROM claims.retention_policy`).Scan(&rows, &enforced); err != nil || rows != 1 || enforced {
		t.Errorf("policy row after the upgrade: rows=%d enforced=%t %v", rows, enforced, err)
	}
	crAssertRoles(t, owner, "upgraded")
	crAssertMatrix(t, owner, "upgraded")
	crAssertDefiners(t, owner, "upgraded")
	crAssertRLS(t, owner, "upgraded")
	crAssertObjects(t, owner, "upgraded")
	// the upgraded database and the fresh one grant the same privileges
	fresh := map[string]bool{}
	up := map[string]bool{}
	shared := fixture(t)
	for _, r := range crRoles {
		for k := range crMatrixOf(t, shared.owner, r) {
			fresh[k] = true
		}
		for k := range crMatrixOf(t, owner, r) {
			up[k] = true
		}
	}
	if len(crDiff(fresh, up))+len(crDiff(up, fresh)) != 0 {
		t.Errorf("fresh and upgraded privileges differ: fresh-only=%s upgraded-only=%s", crHead(crDiff(fresh, up)), crHead(crDiff(up, fresh)))
	}
	// and the definers work on the upgraded data: a report-only run over the populated rows
	job := crClusterLogin(t, owner, "commerce_retention_job")
	c, err := retention.RunOnce(ctx, job, 500)
	if err != nil {
		t.Fatalf("RunOnce on the upgraded database: %v", err)
	}
	if c["enforced"] != 0 || c["links"] != 1 || c["bundles"] < 4 || c["intake"] != 1 || c["operations"] != 1 || c["conversations"] != 1 {
		t.Errorf("report-only counts over the populated upgrade data: %v (want links=1 bundles>=4 intake=1 operations=1 conversations=1)", c)
	}
	after2 := snap()
	for tb, d := range after {
		if after2[tb] != d {
			t.Errorf("the report-only run changed %s", tb)
		}
	}
}

// ---------------------------------------------------------------------------------------
// CRP07 privacy
// ---------------------------------------------------------------------------------------

// crScan searches every text/varchar/name/json/jsonb/bytea/array column of every base table outside pg_* and
// information_schema (River job args included) for each needle and returns needle -> "schema.table.column" hits.
func crScan(t *testing.T, pool *pgxpool.Pool, needles ...string) map[string][]string {
	t.Helper()
	ctx := context.Background()
	rows, err := pool.Query(ctx, `SELECT c.table_schema,c.table_name,c.column_name,c.data_type FROM information_schema.columns c
		JOIN information_schema.tables t ON t.table_schema=c.table_schema AND t.table_name=c.table_name
		WHERE t.table_type='BASE TABLE' AND c.table_schema !~ '^(pg_|information_schema$)'
		  AND c.data_type IN ('text','character varying','character','name','jsonb','json','bytea','ARRAY')
		ORDER BY 1,2,3`)
	if err != nil {
		t.Fatal(err)
	}
	type col struct{ schema, table, name, typ string }
	var cols []col
	for rows.Next() {
		var c col
		if err := rows.Scan(&c.schema, &c.table, &c.name, &c.typ); err != nil {
			t.Fatal(err)
		}
		cols = append(cols, c)
	}
	rows.Close()
	if len(cols) < 200 {
		t.Fatalf("the privacy scan found only %d text-like columns: the universe is wrong", len(cols))
	}
	hits := map[string][]string{}
	for _, c := range cols {
		table := pgx.Identifier{c.schema, c.table}.Sanitize()
		name := pgx.Identifier{c.name}.Sanitize()
		var cond string
		if c.typ == "bytea" {
			cond = `position(convert_to(n,'UTF8') in x.` + name + `)>0`
		} else {
			cond = `position(n in x.` + name + `::text)>0`
		}
		q := `SELECT n FROM unnest($1::text[]) n WHERE EXISTS (SELECT 1 FROM ` + table + ` x WHERE x.` + name + ` IS NOT NULL AND ` + cond + `)`
		found, err := pool.Query(ctx, q, needles)
		if err != nil {
			t.Fatalf("scan %s.%s: %v", table, name, err)
		}
		for found.Next() {
			var n string
			if err := found.Scan(&n); err != nil {
				t.Fatal(err)
			}
			hits[n] = append(hits[n], c.schema+"."+c.table+"."+c.name)
		}
		found.Close()
	}
	return hits
}

// crBytesScan finds raw byte needles (a digest) in every bytea column.
func crBytesScan(t *testing.T, pool *pgxpool.Pool, needle []byte) []string {
	t.Helper()
	ctx := context.Background()
	rows, err := pool.Query(ctx, `SELECT c.table_schema,c.table_name,c.column_name FROM information_schema.columns c
		JOIN information_schema.tables t ON t.table_schema=c.table_schema AND t.table_name=c.table_name
		WHERE t.table_type='BASE TABLE' AND c.table_schema !~ '^(pg_|information_schema$)' AND c.data_type='bytea' ORDER BY 1,2,3`)
	if err != nil {
		t.Fatal(err)
	}
	var cols [][3]string
	for rows.Next() {
		var c [3]string
		if err := rows.Scan(&c[0], &c[1], &c[2]); err != nil {
			t.Fatal(err)
		}
		cols = append(cols, c)
	}
	rows.Close()
	var out []string
	for _, c := range cols {
		var n int
		q := `SELECT count(*) FROM ` + pgx.Identifier{c[0], c[1]}.Sanitize() + ` x WHERE position($1::bytea in x.` + pgx.Identifier{c[2]}.Sanitize() + `)>0`
		if err := pool.QueryRow(ctx, q, needle).Scan(&n); err != nil {
			t.Fatalf("bytea scan %s: %v", c, err)
		}
		if n > 0 {
			out = append(out, c[0]+"."+c[1]+"."+c[2])
		}
	}
	return out
}

// crCLI runs the real retention-admin binary and returns its exit code and captured streams.
func crCLI(t *testing.T, binary string, env []string, stdin string, args ...string) (code int, stdout, stderr string) {
	t.Helper()
	cmd := exec.Command(binary, args...)
	cmd.Env = append([]string{"PATH=" + os.Getenv("PATH")}, env...)
	cmd.Stdin = strings.NewReader(stdin)
	var so, se bytes.Buffer
	cmd.Stdout, cmd.Stderr = &so, &se
	done := make(chan error, 1)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		var exit *exec.ExitError
		switch {
		case err == nil:
			code = 0
		case errors.As(err, &exit):
			code = exit.ExitCode()
		default:
			t.Fatalf("run %s: %v", args, err)
		}
	case <-time.After(60 * time.Second):
		_ = cmd.Process.Kill()
		t.Fatalf("retention-admin %v did not return", args)
	}
	return code, so.String(), se.String()
}

// CRP07 + RD6/RD7 + §7: after an actor erasure and a C2 purge (owner-seeded actor + a real webhook -> intake chain with
// registered Page tokens), no database column (River args included) holds the sender id, comment ids, manual label,
// Page token, actor key or peer key; the erased actor key's sha256 is only in retention_log.actor_digest; no counts
// row holds a string; and the captured stdout/stderr of the real retention-admin binary (every exit code) and of the
// real claims-worker process hold none of them either. A positive control proves the scan can see each sentinel
// before the erasure wherever the schema stores it in the clear.
func TestClaimsRetentionCRP07Privacy(t *testing.T) {
	e := crSetup(t)
	ctx := context.Background()
	w := e.w
	f := e.f
	lane := e.socialLane(w.store)
	cli := mrBuild(t, "../../cmd/retention-admin", "retention-admin")
	e.enforceDefaults()
	defer e.resetPolicy()

	// --- sentinels ---
	sender := "9" + crDigits(11)
	a := e.actorWorld(lane, sender, true, crAgo(20, 0))
	label := "SENTINEL-LABEL-" + t04Tag()
	old := w.session(t, w.store)
	w.closedAt(t, old, crOld(90))
	labelled := w.bundle(t, old, "manual", "", label)
	// real chain with registered Page tokens (encrypted at rest) and a real intake row
	m := mciSetup(t, mciOpts{private: true})
	realSender := "9" + crDigits(13)
	real := m.postFB(t, "", realSender, "A1", mciAt(3*time.Second), nil)
	m.apply(t)
	m.h.closeWindow(t, m.session)
	mustExec(t, f.owner, `UPDATE claims.meta_intake SET received_at=`+crAgo(20, 0)+` WHERE object='page' AND asset_id=$1`, m.pageAsset)
	mustExec(t, f.owner, `UPDATE integration.operations SET state='CANCELLED',generation=greatest(generation,1),lease_mode='',lease_until=NULL,lease_token_hash=NULL WHERE action='meta.private_reply' AND request->>'asset_id'=$1`, m.pageAsset)
	realKey := meta.ClaimActorKey(m.actor, "page", m.pageAsset, realSender)
	needles := []string{sender, label, a.key, a.peer, real.comment, realSender, realKey, m.pageToken, m.igToken}
	needles = append(needles, a.comments...)
	// (the synthetic sender id lives only in ciphertext, the real chain's too: they are checked for absence throughout)
	// positive control: the scan sees what the schema stores in the clear before the erasure
	pre := crScan(t, f.owner, needles...)
	for _, want := range []struct{ needle, where string }{
		{a.key, "claims.bundles.actor_key"}, {a.key, "claims.meta_intake.actor_key"}, {a.comments[0], "claims.meta_intake.comment_ref"},
		{a.comments[0], "integration.operations.request"}, {label, "claims.bundles.label"}, {a.peer, "social.conversations.peer_key"},
		{real.comment, "claims.meta_intake.comment_ref"}, {realKey, "claims.bundles.actor_key"},
	} {
		if !containsHit(pre[want.needle], want.where) {
			t.Errorf("positive control: the scan did not find the sentinel in %s (hits %v): the scan cannot see it", want.where, pre[want.needle])
		}
	}
	for _, encrypted := range []string{sender, realSender, m.pageToken, m.igToken} {
		if len(pre[encrypted]) != 0 {
			t.Errorf("a sentinel stored only encrypted appears in the clear in %v", pre[encrypted])
		}
	}

	// --- CLI, real binary, real operator login ---
	baseEnv := []string{"COMMERCE_RETENTION_OPERATOR_DATABASE_URL=" + e.opDSN, "COMMERCE_CLAIMS_ACTOR_KEY=" + base64.StdEncoding.EncodeToString(e.actorRaw)}
	var outputs []string
	record := func(label string, code int, so, se string) {
		outputs = append(outputs, label+"\n"+so+"\n"+se)
	}
	req := randomUUID()
	// held first (intake of the real chain too recent? no: make the synthetic actor's op non-terminal for one attempt)
	mustExec(t, f.owner, `UPDATE integration.operations SET state='READY',generation=0 WHERE id=$1`, a.ops[0])
	code, so, se := crCLI(t, cli, baseEnv, `{"sender_id":"`+sender+`"}`, "erase", "--request", req, "--object", "page", "--asset", a.asset, "--app", miApp)
	record("held", code, so, se)
	if code != 3 || !regexp.MustCompile(`(?m)^retry_after=\d{4}-\d\d-\d\dT\d\d:\d\d:\d\dZ$`).MatchString(so) || !strings.HasPrefix(so, "request="+req+"\n") || strings.TrimSpace(se) != "retention_admin_held" {
		t.Errorf("CLI held: exit %d stdout %q stderr %q, want 3 / request=..+retry_after=RFC3339 / retention_admin_held", code, so, se)
	}
	mustExec(t, f.owner, `UPDATE integration.operations SET state='SUCCEEDED',generation=1 WHERE id=$1`, a.ops[0])
	code, so, se = crCLI(t, cli, baseEnv, `{"sender_id":"`+sender+`"}`, "erase", "--request", req, "--object", "page", "--asset", a.asset, "--app", miApp)
	record("done", code, so, se)
	if code != 0 || strings.TrimSpace(se) != "" || !strings.HasPrefix(so, "request="+req+"\n") || !regexp.MustCompile(`(?m)^bundles=2$`).MatchString(so) {
		t.Errorf("CLI erase (a): exit %d stdout %q stderr %q", code, so, se)
	}
	code, so, se = crCLI(t, cli, baseEnv, `{"sender_id":"`+sender+`"}`, "erase", "--request", req, "--object", "page", "--asset", a.asset, "--app", miApp)
	record("replay", code, so, se)
	if code != 0 || !regexp.MustCompile(`(?m)^replayed=1$`).MatchString(so) {
		t.Errorf("CLI replay: exit %d stdout %q", code, so)
	}
	code, so, se = crCLI(t, cli, baseEnv, `{"sender_id":"`+realSender+`x"}`, "erase", "--request", req, "--object", "page", "--asset", a.asset)
	record("usage-bad-sender", code, so, se)
	if code != 2 || strings.TrimSpace(se) != "retention_admin_usage" {
		t.Errorf("CLI bad sender id: exit %d stderr %q, want 2 retention_admin_usage", code, se)
	}
	code, so, se = crCLI(t, cli, baseEnv, `{"sender_id":"`+crDigits(12)+`"}`, "erase", "--request", req, "--object", "page", "--asset", a.asset)
	record("conflict", code, so, se)
	if code != 5 || strings.TrimSpace(se) != "retention_admin_conflict" {
		t.Errorf("CLI same request other selector: exit %d stderr %q, want 5 retention_admin_conflict", code, se)
	}
	code, so, se = crCLI(t, cli, baseEnv, `{"sender_id":"`+crDigits(12)+`"}`, "erase", "--request", randomUUID(), "--object", "page", "--asset", a.asset)
	record("not-found", code, so, se)
	if code != 4 || strings.TrimSpace(se) != "retention_admin_not_found" {
		t.Errorf("CLI unknown actor: exit %d stderr %q, want 4 retention_admin_not_found", code, se)
	}
	// (b) by the real comment id, resolving the real actor of the real chain
	code, so, se = crCLI(t, cli, baseEnv, `{"comment_ref":"`+real.comment+`"}`, "erase", "--request", randomUUID(), "--object", "page", "--asset", m.pageAsset)
	record("real-b", code, so, se)
	if code != 0 {
		t.Errorf("CLI erase (b) of the real chain: exit %d stdout %q stderr %q", code, so, se)
	}
	// (c) manual bundle: busy when a row lock outlasts lock_timeout, done after
	mreq := randomUUID()
	hold := e.holdRow(`SELECT 1 FROM claims.bundles WHERE id=$1 FOR UPDATE`, labelled.id)
	code, so, se = crCLI(t, cli, baseEnv, "", "erase", "--request", mreq, "--tenant", w.tenant, "--store", old.store, "--bundle", labelled.id)
	hold.rollback(t)
	record("busy", code, so, se)
	if code != 5 || strings.TrimSpace(se) != "retention_admin_busy" {
		t.Errorf("CLI busy: exit %d stderr %q, want 5 retention_admin_busy", code, se)
	}
	// C2 purges the labelled bundle's session instead of a manual erasure: the label must vanish either way
	c := e.run(500)
	if c["bundles"] < 1 {
		t.Errorf("the run did not purge the aged session: %v", c)
	}
	// status on the job login, run refused on the job login, policy paths
	jobEnv := []string{"COMMERCE_RETENTION_JOB_DATABASE_URL=" + e.jobDSN}
	code, so, se = crCLI(t, cli, jobEnv, "", "status")
	record("status-job", code, so, se)
	if code != 0 || !strings.Contains(so, "enforced=1\n") || !strings.Contains(so, "last_run_unix=") {
		t.Errorf("CLI status on the job login: exit %d stdout %q", code, so)
	}
	code, so, se = crCLI(t, cli, jobEnv, "", "run")
	record("run-job", code, so, se)
	if code != 2 || strings.TrimSpace(se) != "retention_admin_usage" {
		t.Errorf("CLI run on the job DSN: exit %d stderr %q, want 2 retention_admin_usage", code, se)
	}
	st, err := retention.GetStatus(ctx, e.op)
	if err != nil {
		t.Fatal(err)
	}
	code, so, se = crCLI(t, cli, baseEnv, "", "policy-set", "--expected-version", fmt.Sprint(st.Version+7), "--enforced=true", "--link-days", "7", "--intake-days", "30", "--claims-days", "90", "--social-days", "30")
	record("policy-conflict", code, so, se)
	if code != 5 || strings.TrimSpace(se) != "retention_admin_conflict" {
		t.Errorf("CLI policy-set with a stale version: exit %d stderr %q, want 5 retention_admin_conflict", code, se)
	}
	code, so, se = crCLI(t, cli, baseEnv, "", "policy-set", "--expected-version", fmt.Sprint(st.Version), "--enforced=true", "--link-days", "7", "--intake-days", "30", "--claims-days", "90", "--social-days", "30")
	record("policy-ok", code, so, se)
	if code != 0 || strings.TrimSpace(so) != fmt.Sprintf("version=%d", st.Version+1) {
		t.Errorf("CLI policy-set: exit %d stdout %q", code, so)
	}
	code, so, se = crCLI(t, cli, baseEnv, "", "run", "--limit", "50")
	record("run-op", code, so, se)
	if code != 0 || !regexp.MustCompile(`(?m)^enforced=1$`).MatchString(so) {
		t.Errorf("CLI run: exit %d stdout %q", code, so)
	}
	// an unreachable database: fixed code, the password never printed
	badU := url.URL{Scheme: "postgres", User: url.UserPassword("lc_retention_operator", crp07Sentinel), Host: "127.0.0.1:1", Path: "/x", RawQuery: "sslmode=disable&connect_timeout=2"}
	code, so, se = crCLI(t, cli, []string{"COMMERCE_RETENTION_OPERATOR_DATABASE_URL=" + badU.String()}, "", "status")
	record("db-down", code, so, se)
	if code != 1 || strings.TrimSpace(se) != "retention_admin_database" {
		t.Errorf("CLI with an unreachable database: exit %d stderr %q, want 1 retention_admin_database", code, se)
	}

	// --- the real claims-worker process on the same data ---
	we := crNewWorkerEnv(t, f)
	e.clearJobs()
	worker := crLaunch(t, mrBuild(t, "../../cmd/claims-worker", "claims-worker"), we.env)
	e.awaitJob("completed", 30*time.Second)
	workerLog := crReadLog(t, worker)
	mrStop(t, worker, syscall.SIGTERM, true)
	e.clearJobs()
	outputs = append(outputs, "worker\n"+workerLog)

	// --- assertions ---
	all := strings.Join(outputs, "\n")
	for _, s := range append(append([]string{}, needles...), crp07Sentinel, base64.StdEncoding.EncodeToString(e.actorRaw), we.jobDSNSecret) {
		if s != "" && strings.Contains(all, s) {
			t.Errorf("captured CLI/worker output contains a sentinel (%d chars starting %.6s)", len(s), s)
		}
	}
	post := crScan(t, f.owner, needles...)
	for needle, where := range post {
		t.Errorf("after the erasure and purge a sentinel (%d chars, starting %.6s) is still stored in %v", len(needle), needle, where)
	}
	digest := sha256.Sum256([]byte(a.key))
	if where := crBytesScan(t, f.owner, digest[:]); len(where) != 1 || where[0] != "claims.retention_log.actor_digest" {
		t.Errorf("sha256(actor_key) is stored in %v, want only claims.retention_log.actor_digest", where)
	}
	if hits := crScan(t, f.owner, hex.EncodeToString(digest[:])); len(hits[hex.EncodeToString(digest[:])]) != 0 {
		t.Errorf("the hex digest of the erased key appears in text columns %v", hits)
	}
	if n := crCount(t, f.owner, `SELECT count(*) FROM claims.retention_log WHERE jsonb_path_exists(counts,'$.** ? (@.type() == "string")')`); n != 0 {
		t.Errorf("%d retention_log rows contain a string count", n)
	}
	if n := crCount(t, f.owner, `SELECT count(*) FROM claims.retention_log WHERE counts::text ~ ':\s*"'`); n != 0 {
		t.Errorf("%d retention_log rows contain a quoted value", n)
	}
}

func containsHit(hits []string, where string) bool {
	for _, h := range hits {
		if h == where {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------------------
// CRP08 restore replay (MODEL of a restore until T20 exports tombstones)
// ---------------------------------------------------------------------------------------

// crDBPool opens a pool as a fresh LOGIN role granted exactly authority on database db of the throw-away cluster of owner.
func crDBPool(t *testing.T, owner *pgxpool.Pool, db, authority string) (*pgxpool.Pool, string) {
	t.Helper()
	ctx := context.Background()
	login := "cr_r_" + strings.ReplaceAll(randomUUID(), "-", "")
	password := hex.EncodeToString(randomBytes(24)) // random per run, never a stored secret
	id := pgx.Identifier{login}.Sanitize()
	mustExec(t, owner, `CREATE ROLE `+id+` LOGIN NOSUPERUSER NOBYPASSRLS NOCREATEDB NOCREATEROLE NOREPLICATION PASSWORD '`+password+`'`)
	mustExec(t, owner, `GRANT `+pgx.Identifier{authority}.Sanitize()+` TO `+id+` WITH INHERIT TRUE, SET FALSE`)
	cc := owner.Config().ConnConfig
	u := &url.URL{Scheme: "postgres", User: url.UserPassword(login, password), Host: fmt.Sprintf("%s:%d", cc.Host, cc.Port), Path: "/" + db, RawQuery: "sslmode=disable"}
	var pool *pgxpool.Pool
	var err error
	if authority == "commerce_retention_job" {
		pool, err = platform.OpenRetentionJobPool(ctx, u.String())
	} else {
		pool, err = platform.OpenRetentionOperatorPool(ctx, u.String())
	}
	if err != nil {
		t.Fatalf("open %s on %s: %v", authority, db, err)
	}
	t.Cleanup(pool.Close)
	return pool, u.String()
}

func crOwnerOn(t *testing.T, owner *pgxpool.Pool, db string) *pgxpool.Pool {
	t.Helper()
	cfg := owner.Config().Copy()
	cfg.ConnConfig.Database = db
	p, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(p.Close)
	return p
}

// crActorRows is the footprint of one synthetic actor in a throw-away database (owner SQL, no social rows).
type crPlain struct {
	key       string
	bundles   []crBundle
	sessions  []crSess
	intakeIDs []string
	ops       []string
}

func (w *crWorld) plainActor(t *testing.T, key string, twoStores bool) *crPlain {
	t.Helper()
	a := &crPlain{key: key}
	asset := miAsset()
	stores := []string{w.store}
	if twoStores {
		stores = append(stores, w.store2)
	}
	for _, store := range stores {
		s := w.session(t, store)
		w.closedAt(t, s, crAgo(20, 0))
		m := w.sourceOn(t, s, "page", asset)
		b := w.bundle(t, s, "facebook", key, "")
		w.bind(t, s, b)
		w.link(t, s, b, `(clock_timestamp() + interval '1 hour')`)
		ref := asset + "_" + crDigits(10)
		w.intake(t, m, ref, key, "APPLIED", crAgo(20, 0), "")
		a.intakeIDs = append(a.intakeIDs, ref)
		a.ops = append(a.ops, w.operation(t, m, b, ref, "SUCCEEDED", crAgo(20, 0)))
		a.bundles, a.sessions = append(a.bundles, b), append(a.sessions, s)
	}
	return a
}

func (a *crPlain) fingerprint(t *testing.T, pool *pgxpool.Pool) string {
	ids := []string{}
	for _, b := range a.bundles {
		ids = append(ids, b.id)
	}
	return strings.Join([]string{
		crDigest(t, pool, "claims.bundles", "id=ANY($1::uuid[])", ids),
		crDigest(t, pool, "claims.links", "bundle_id=ANY($1::uuid[])", ids),
		crDigest(t, pool, "claims.meta_intake", "actor_key=$1", a.key),
		crDigest(t, pool, "integration.operations", "id=ANY($1::uuid[])", a.ops),
	}, "|")
}

// assertErased checks a database where the actor must be de-identified.
func (a *crPlain) assertErased(t *testing.T, pool *pgxpool.Pool, label string) {
	t.Helper()
	for _, b := range a.bundles {
		var actor string
		var purged *time.Time
		var owner *string
		if err := pool.QueryRow(context.Background(), `SELECT actor_key,purged_at,owner_id::text FROM claims.bundles WHERE id=$1`, b.id).Scan(&actor, &purged, &owner); err != nil {
			t.Fatal(err)
		}
		if purged == nil || owner != nil || actor == a.key || !regexpMatch(crHex64Re, actor) {
			t.Errorf("%s: bundle %s purged=%v owner=%v key kept=%t", label, b.id, purged, owner, actor == a.key)
		}
		if crCount(t, pool, `SELECT count(*) FROM claims.links WHERE bundle_id=$1`, b.id) != 0 {
			t.Errorf("%s: link of %s survives", label, b.id)
		}
	}
	if n := crCount(t, pool, `SELECT count(*) FROM claims.meta_intake WHERE actor_key=$1`, a.key); n != 0 {
		t.Errorf("%s: %d intake rows of the erased key survive", label, n)
	}
	if n := crCount(t, pool, `SELECT count(*) FROM claims.bundles WHERE actor_key=$1`, a.key); n != 0 {
		t.Errorf("%s: the erased actor key is still in claims.bundles", label)
	}
	for _, id := range a.ops {
		if crCount(t, pool, `SELECT count(*) FROM integration.operations WHERE id=$1 AND request ? 'comment_ref'`, id) != 0 {
			t.Errorf("%s: operation %s keeps its comment_ref", label, id)
		}
	}
}

// CRP08 (REAL_PG, MODEL of restore until T20): a snapshot of a database taken BEFORE an erasure is restored (CREATE
// DATABASE ... TEMPLATE, the physical model of restore-to-a-fresh-database). Red: without a replay the restored bundles
// keep the erased actor key. Green: replay from the log rows (partial restore keeping the log) and from an external
// tombstone-tuple list through the real CLI (a tombstone missing from the log is inserted as actor_erased, an existing
// one is not duplicated), a `replay` row is written, the RD5 hold is ignored, an untouched actor is byte-identical, and a
// second replay changes nothing.
func TestClaimsRetentionCRP08RestoreReplay(t *testing.T) {
	ctx := context.Background()
	owner := mciStartPG(t)
	if err := migrations.Apply(ctx, owner); err != nil {
		t.Fatalf("migrate the throw-away cluster: %v", err)
	}
	w := crFreshWorld(t, owner)
	keyP, keyQ := crHex64(), crHex64()
	p := w.plainActor(t, keyP, true)
	q := w.plainActor(t, keyQ, false)
	ms := w.session(t, w.store)
	w.closedAt(t, ms, crAgo(20, 0))
	manual := w.bundle(t, ms, "manual", "", "restore-manual-"+t04Tag())
	w.bind(t, ms, manual)
	w.link(t, ms, manual, `(clock_timestamp() + interval '1 hour')`)
	qBefore := q.fingerprint(t, owner)

	// snapshot BEFORE any erasure: the "backup" that will be restored
	src := owner.Config().ConnConfig.Database
	owner.Close() // TEMPLATE needs the source database idle
	admin := owner.Config().ConnConfig.Copy()
	admin.Database = "postgres"
	conn, err := pgx.ConnectConfig(ctx, admin)
	if err != nil {
		t.Fatalf("maintenance connection: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close(ctx) })
	if _, err := conn.Exec(ctx, `SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE datname=$1 AND pid<>pg_backend_pid()`, src); err != nil {
		t.Fatal(err)
	}
	snapshot := "lc_crp08_snapshot"
	if _, err := conn.Exec(ctx, `CREATE DATABASE `+snapshot+` TEMPLATE `+src); err != nil {
		t.Fatalf("snapshot the pre-erasure database: %v", err)
	}
	srcOwner := crOwnerOn(t, owner, src)
	w.owner = srcOwner

	// the erasures happen in the source database (operator login, real definer)
	srcOp, _ := crDBPool(t, srcOwner, src, "commerce_retention_operator")
	reqA, reqC := randomUUID(), randomUUID()
	if c, held, err := retention.Erase(ctx, srcOp, retention.Selector{Request: reqA, Object: "page", Asset: "1", ActorKey: keyP}); err != nil || held.IsHeld() || c["bundles"] != 2 {
		// (asset digits are not part of the key derivation the definer checks: the key selects the actor)
		t.Fatalf("erase P in the source: %v held=%v err=%v", c, held.IsHeld(), err)
	}
	if c, held, err := retention.Erase(ctx, srcOp, retention.Selector{Request: reqC, Tenant: w.tenant, Store: ms.store, Bundle: manual.id}); err != nil || held.IsHeld() || c["bundles"] != 1 {
		t.Fatalf("erase the manual bundle in the source: %v held=%v err=%v", c, held.IsHeld(), err)
	}
	rows, err := srcOwner.Query(ctx, `SELECT request_id::text,selector_digest,actor_digest,bundle_tenant::text,bundle_store::text,bundle_ref::text FROM claims.retention_log WHERE kind='actor_erased' AND request_id=ANY($1::uuid[]) ORDER BY request_id`, []string{reqA, reqC})
	if err != nil {
		t.Fatal(err)
	}
	var tombstones []retention.Tombstone
	for rows.Next() {
		var ts retention.Tombstone
		var bt, bs, br *string
		if err := rows.Scan(&ts.RequestID, &ts.SelectorDigest, &ts.ActorDigest, &bt, &bs, &br); err != nil {
			t.Fatal(err)
		}
		if bt != nil {
			ts.BundleTenant, ts.BundleStore, ts.BundleRef = *bt, *bs, *br
		}
		tombstones = append(tombstones, ts)
	}
	rows.Close()
	if len(tombstones) != 2 {
		t.Fatalf("source tombstones: %d, want 2", len(tombstones))
	}
	p.assertErased(t, srcOwner, "source after erasure")

	restore := func(name string) (*pgxpool.Pool, *pgxpool.Pool, string) {
		if _, err := conn.Exec(ctx, `CREATE DATABASE `+name+` TEMPLATE `+snapshot); err != nil {
			t.Fatalf("restore %s: %v", name, err)
		}
		t.Cleanup(func() { _, _ = conn.Exec(ctx, `DROP DATABASE IF EXISTS `+name+` WITH (FORCE)`) })
		ownerR := crOwnerOn(t, owner, name)
		opR, dsn := crDBPool(t, ownerR, name, "commerce_retention_operator")
		return ownerR, opR, dsn
	}
	t.Cleanup(func() { _, _ = conn.Exec(ctx, `DROP DATABASE IF EXISTS `+snapshot+` WITH (FORCE)`) })

	t.Run("red-restore-without-replay-keeps-the-key", func(t *testing.T) {
		ownerR, _, _ := restore("lc_crp08_red")
		if n := crCount(t, ownerR, `SELECT count(*) FROM claims.bundles WHERE actor_key=$1 AND purged_at IS NULL`, keyP); n != 2 {
			t.Fatalf("the restored snapshot has %d bundles with the erased key, want 2 (the RED state: the erasure is lost)", n)
		}
		if n := crCount(t, ownerR, `SELECT count(*) FROM claims.meta_intake WHERE actor_key=$1`, keyP); n != 2 {
			t.Fatalf("restored intake rows of the erased key: %d", n)
		}
		if crCount(t, ownerR, `SELECT count(*) FROM claims.retention_log WHERE kind='actor_erased'`) != 0 {
			t.Fatal("the snapshot already holds the tombstones")
		}
	})

	t.Run("green-replay-from-log-rows-ignoring-the-hold", func(t *testing.T) {
		ownerR, opR, _ := restore("lc_crp08_log")
		// a partial restore that kept the log: the tombstone rows are in retention_log, the data is the old snapshot
		for _, ts := range tombstones {
			var bt, bs, br any
			var actor any
			if ts.ActorDigest != nil {
				actor = ts.ActorDigest
			}
			if ts.BundleRef != "" {
				bt, bs, br = ts.BundleTenant, ts.BundleStore, ts.BundleRef
			}
			mustExec(t, ownerR, `INSERT INTO claims.retention_log(kind,request_id,selector_digest,actor_digest,bundle_tenant,bundle_store,bundle_ref,counts) VALUES('actor_erased',$1,$2,$3,$4,$5,$6,'{"bundles":1}')`,
				ts.RequestID, ts.SelectorDigest, actor, bt, bs, br)
		}
		// hold conditions that would refuse a live erasure (RD5) and that the writer's §4 grants can still act on: an OPEN
		// window and an intake received a moment ago (terminal state). PENDING intake and non-terminal operations are
		// covered separately below: the §4 policies forbid deleting/redacting those rows even for a replay.
		mustExec(t, ownerR, `UPDATE claims.meta_intake SET received_at=clock_timestamp() WHERE actor_key=$1`, keyP)
		mustExec(t, ownerR, `UPDATE live.claim_windows SET state='OPEN',generation=generation+1,opened_at=clock_timestamp(),closed_at=NULL WHERE session_id=$1`, p.sessions[0].id)
		qFP := q.fingerprint(t, ownerR)
		if qFP != qBefore {
			t.Fatal("the restored snapshot differs from the seed for the untouched actor")
		}
		n, err := retention.Replay(ctx, opR, nil)
		if err != nil || n != 2 {
			t.Fatalf("Replay from the log: n=%d err=%v, want 2 tombstones applied", n, err)
		}
		p.assertErased(t, ownerR, "replay from the log")
		if e := crRestoredManual(t, ownerR, manual.id); e == "" {
			t.Error("the manual bundle tombstone was not replayed (no erased- label)")
		}
		if q.fingerprint(t, ownerR) != qFP {
			t.Error("the replay changed an untouched actor")
		}
		if got := crCount(t, ownerR, `SELECT count(*) FROM claims.retention_log WHERE kind='actor_erased'`); got != 2 {
			t.Errorf("actor_erased rows after a replay from the log: %d, want the original 2 (no duplicates)", got)
		}
		if got := crCount(t, ownerR, `SELECT count(*) FROM claims.retention_log WHERE kind='replay'`); got != 1 {
			t.Errorf("replay rows: %d, want exactly 1", got)
		}
		var counts string
		if err := ownerR.QueryRow(ctx, `SELECT counts::text FROM claims.retention_log WHERE kind='replay'`).Scan(&counts); err != nil {
			t.Fatal(err)
		}
		var parsed map[string]any
		if err := json.Unmarshal([]byte(counts), &parsed); err != nil || len(parsed) == 0 {
			t.Fatalf("replay row counts %s: %v", counts, err)
		}
		for k, v := range parsed {
			if _, num := v.(float64); !num {
				t.Errorf("replay counts[%s]=%v is not a number", k, v)
			}
		}
		if parsed["bundles"] != float64(3) {
			t.Errorf("replay counts %v, want bundles=3 (two of P, one manual)", parsed)
		}
		// a second replay changes no data
		fp := crDigests(t, ownerR, "claims.bundles", "claims.links", "claims.meta_intake", "integration.operations")
		if _, err := retention.Replay(ctx, opR, nil); err != nil {
			t.Fatalf("second replay: %v", err)
		}
		crSameDigests(t, "second replay", fp, crDigests(t, ownerR, "claims.bundles", "claims.links", "claims.meta_intake", "integration.operations"))
	})

	t.Run("replay-removes-pending-intake-and-redacts-nonterminal-operations", func(t *testing.T) {
		// Contract §3 (r2-close-retention amendment): a replay ignores the RD5 hold, including the PENDING-intake and
		// non-terminal-reply-operation limits of the §4 policies, which a replay lifts through a transaction-local flag
		// set only inside claims.replay_actor_erasures. A snapshot taken while an intake row of the erased actor was
		// PENDING (or its reply not yet terminal) must not keep the actor key or the comment_ref after the replay.
		ownerR, opR, _ := restore("lc_crp08_residue")
		for _, ts := range tombstones {
			if ts.ActorDigest != nil {
				mustExec(t, ownerR, `INSERT INTO claims.retention_log(kind,request_id,selector_digest,actor_digest,counts) VALUES('actor_erased',$1,$2,$3,'{"bundles":2}')`, ts.RequestID, ts.SelectorDigest, ts.ActorDigest)
			}
		}
		mustExec(t, ownerR, `UPDATE claims.meta_intake SET state='PENDING',applied_event_id=NULL,drop_reason=NULL,received_at=clock_timestamp() WHERE comment_ref=$1`, p.intakeIDs[0])
		mustExec(t, ownerR, `UPDATE integration.operations SET state='READY',generation=0 WHERE id=$1`, p.ops[0])
		other := q.fingerprint(t, ownerR)
		if _, err := retention.Replay(ctx, opR, nil); err != nil {
			t.Fatalf("replay: %v", err)
		}
		for _, b := range p.bundles {
			var purged *time.Time
			var actor string
			if err := ownerR.QueryRow(ctx, `SELECT purged_at,actor_key FROM claims.bundles WHERE id=$1`, b.id).Scan(&purged, &actor); err != nil || purged == nil || actor == keyP {
				t.Errorf("bundle %s not de-identified by the replay: purged=%v key kept=%t", b.id, purged, actor == keyP)
			}
		}
		if n := crCount(t, ownerR, `SELECT count(*) FROM claims.meta_intake WHERE actor_key=$1`, keyP); n != 0 {
			t.Errorf("%d intake row(s) (PENDING included) still carry the erased actor key after the replay", n)
		}
		var state string
		var hasRef bool
		if err := ownerR.QueryRow(ctx, `SELECT state,request ? 'comment_ref' FROM integration.operations WHERE id=$1`, p.ops[0]).Scan(&state, &hasRef); err != nil {
			t.Fatal(err)
		}
		if hasRef || state != "READY" {
			t.Errorf("non-terminal reply operation after the replay: state=%s comment_ref present=%t, want state untouched (READY) and comment_ref redacted", state, hasRef)
		}
		if q.fingerprint(t, ownerR) != other {
			t.Error("the replay changed an untouched actor")
		}
		// the flag is transaction-local: a later erase in the same session must not inherit it (PENDING rows stay protected)
		var flag string
		if err := ownerR.QueryRow(ctx, `SELECT coalesce(current_setting('lc.retention_replay',true),'')`).Scan(&flag); err != nil || flag == "on" {
			t.Errorf("lc.retention_replay leaked past the replay transaction: %q %v", flag, err)
		}
	})

	t.Run("green-replay-from-an-external-tombstone-file-via-the-cli", func(t *testing.T) {
		ownerR, _, dsn := restore("lc_crp08_file")
		// one tombstone is already in the restored log (must not be duplicated), the other one only in the external list
		actorIdx, manualIdx := 0, 1
		if tombstones[0].ActorDigest == nil {
			actorIdx, manualIdx = 1, 0
		}
		mustExec(t, ownerR, `INSERT INTO claims.retention_log(kind,request_id,selector_digest,actor_digest,counts) VALUES('actor_erased',$1,$2,$3,'{"bundles":2}')`,
			tombstones[actorIdx].RequestID, tombstones[actorIdx].SelectorDigest, tombstones[actorIdx].ActorDigest)
		type entry struct {
			RequestID      string  `json:"request_id"`
			SelectorDigest string  `json:"selector_digest"`
			ActorDigest    *string `json:"actor_digest"`
			BundleTenant   *string `json:"bundle_tenant"`
			BundleStore    *string `json:"bundle_store"`
			BundleRef      *string `json:"bundle_ref"`
		}
		var list []entry
		for _, ts := range tombstones {
			en := entry{RequestID: ts.RequestID, SelectorDigest: hex.EncodeToString(ts.SelectorDigest)}
			if ts.ActorDigest != nil {
				s := hex.EncodeToString(ts.ActorDigest)
				en.ActorDigest = &s
			}
			if ts.BundleRef != "" {
				a, b, c := ts.BundleTenant, ts.BundleStore, ts.BundleRef
				en.BundleTenant, en.BundleStore, en.BundleRef = &a, &b, &c
			}
			list = append(list, en)
		}
		raw, err := json.Marshal(list)
		if err != nil {
			t.Fatal(err)
		}
		file := filepath.Join(t.TempDir(), "tombstones.json")
		if err := os.WriteFile(file, raw, 0o600); err != nil {
			t.Fatal(err)
		}
		cli := mrBuild(t, "../../cmd/retention-admin", "retention-admin")
		code, so, se := crCLI(t, cli, []string{"COMMERCE_RETENTION_OPERATOR_DATABASE_URL=" + dsn}, "", "replay", "--tombstones-file", file)
		if code != 0 || strings.TrimSpace(so) != "tombstones=2" || strings.TrimSpace(se) != "" {
			t.Fatalf("retention-admin replay --tombstones-file: exit %d stdout %q stderr %q", code, so, se)
		}
		p.assertErased(t, ownerR, "replay from the external list")
		if crRestoredManual(t, ownerR, manual.id) == "" {
			t.Error("the manual bundle tombstone of the external list was not replayed")
		}
		if got := crCount(t, ownerR, `SELECT count(*) FROM claims.retention_log WHERE kind='actor_erased'`); got != 2 {
			t.Errorf("actor_erased rows: %d, want 2 (the existing tombstone not duplicated, the missing one inserted once)", got)
		}
		for _, ts := range tombstones {
			if n := crCount(t, ownerR, `SELECT count(*) FROM claims.retention_log WHERE kind='actor_erased' AND request_id=$1`, ts.RequestID); n != 1 {
				t.Errorf("request %s has %d actor_erased rows", ts.RequestID, n)
			}
		}
		var inserted int
		if err := ownerR.QueryRow(ctx, `SELECT count(*) FROM claims.retention_log WHERE kind='actor_erased' AND request_id=$1 AND counts='{}'::jsonb`, tombstones[manualIdx].RequestID).Scan(&inserted); err != nil || inserted != 1 {
			t.Errorf("the tombstone missing from the log must be inserted with empty counts: %d %v", inserted, err)
		}
		if got := crCount(t, ownerR, `SELECT count(*) FROM claims.retention_log WHERE kind='replay'`); got != 1 {
			t.Errorf("replay rows: %d, want 1", got)
		}
		if q.fingerprint(t, ownerR) != qBefore {
			t.Error("the untouched actor changed")
		}
		// the same file again: idempotent, one more replay row at most, no duplicated tombstones
		code, so, _ = crCLI(t, cli, []string{"COMMERCE_RETENTION_OPERATOR_DATABASE_URL=" + dsn}, "", "replay", "--tombstones-file", file)
		if code != 0 || crCount(t, ownerR, `SELECT count(*) FROM claims.retention_log WHERE kind='actor_erased'`) != 2 {
			t.Errorf("second CLI replay: exit %d stdout %q", code, so)
		}
	})
}

// crRestoredManual returns the erased-<32 hex> label of a manual bundle ("" if it has none).
func crRestoredManual(t *testing.T, pool *pgxpool.Pool, id string) string {
	t.Helper()
	var label *string
	if err := pool.QueryRow(context.Background(), `SELECT label FROM claims.bundles WHERE id=$1 AND purged_at IS NOT NULL AND label ~ '^erased-[0-9a-f]{32}$'`, id).Scan(&label); err != nil {
		return ""
	}
	return *label
}
