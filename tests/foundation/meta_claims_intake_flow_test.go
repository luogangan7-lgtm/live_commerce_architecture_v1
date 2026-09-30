// T10c meta claims intake gates MCI04, MCI05, MCI06 and MCI09 (contracts/meta-claims-intake-v1.md
// §12), written by the independent test_worker from the FROZEN contract and the frozen Go/SQL
// signatures of docs/delivery/units/meta-intake-{core,reply}.md, not from the implementation.
//
// Owns: the shared MOCK harness mciEnv (also used by meta_claims_intake_reply_test.go and the
// schema gate): signed synthetic Facebook `feed` and Instagram `comments`/`live_comments` bodies
// -> the real ingress handler -> inbox -> meta.NewConsumerWorkerWithClaims on a real River
// `river_meta` client -> claims.meta_intake -> claimsintake.Poller.ApplyOne (driven by the test,
// never on a timer) -> claims. The reply half (fake Graph, dispatcher) lives in the reply file.
//
// The owner pool is used only for synthetic setup, fault injection (throw-away triggers, backdated
// timestamps of staged rows) and read-back of columns no runtime role may read.
//
// Evidence label: MOCK (REAL_PG + River). Every id, name and text is a synthetic sentinel; no
// Meta token, no send, no non-loopback network. Concurrency is proven with pg_stat_activity lock
// waits and the database's own deadlock counter, never with sleeps.
package foundation_test

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"

	"livecommerce/internal/buyer"
	"livecommerce/internal/claims"
	"livecommerce/internal/claimsintake"
	"livecommerce/internal/command"
	integration "livecommerce/internal/integrations/core"
	"livecommerce/internal/integrations/meta"
	"livecommerce/internal/integrations/metareply"
	"livecommerce/internal/platform"
)

// ---------------------------------------------------------------------------------------
// Harness
// ---------------------------------------------------------------------------------------

type mciOpts struct {
	private bool // sources opt into the first private reply (registers Page tokens first)
	noStore bool // do not publish a storefront domain (reply skip `no_storefront`)
}

type mciEnv struct {
	t                                          *testing.T
	h                                          *lcHarness
	page, ig                                   miTest
	opts                                       mciOpts
	actorRaw, linkRaw, pageKeyRaw              []byte
	actor                                      meta.ClaimsActorKey
	link                                       claims.ReplyLinkKey
	pageKeys                                   *metareply.PageTokenKeyring
	session, sku                               string
	offer                                      claims.Offer
	pageAsset, igAsset, pageBinding, igBinding string
	postID, mediaID, srcFB, srcIG              string
	pageToken, igToken, origin                 string
	intakeLogin                                string
	intakePool                                 *pgxpool.Pool
	poller                                     *claimsintake.Poller
	stopConsumer                               func()
	consumerWorkers                            int
}

func mciDigits(n int) string {
	raw := randomBytes(n)
	out := make([]byte, n)
	for i, b := range raw {
		out[i] = '0' + b%10
	}
	if out[0] == '0' {
		out[0] = '1'
	}
	return string(out)
}

func mciNoop() {}

// mciSetup builds one claim-ready live session (OPEN window, offer A1) with a Facebook Page
// route+source and an Instagram route+source, the consumer running on River, and the intake
// poller. Everything is synthetic; per-test cleanup removes the rows the shared purge cannot.
func mciSetup(t *testing.T, o mciOpts) *mciEnv {
	t.Helper()
	h := lcSetup(t)
	f := h.f
	ctx := context.Background()
	e := &mciEnv{t: t, h: h, opts: o, stopConsumer: mciNoop, consumerWorkers: 8}
	e.page = miSetup(t)
	e.ig = miInstagram(t, e.page)
	var err error
	e.actorRaw, e.linkRaw, e.pageKeyRaw = randomBytes(32), randomBytes(32), randomBytes(32)
	if e.actor, err = meta.NewClaimsActorKey(e.actorRaw); err != nil {
		t.Fatal(err)
	}
	if e.link, err = claims.NewReplyLinkKey(e.linkRaw); err != nil {
		t.Fatal(err)
	}
	if e.pageKeys, err = metareply.NewPageTokenKeyring("pt_key_1", map[string][]byte{"pt_key_1": e.pageKeyRaw}); err != nil {
		t.Fatal(err)
	}
	// integration:execute: put_claim_source. integration:manage: register_meta_page_token requires it since the
	// wave-3 amendment (contract §15, ruling i: "integration:manage held; raises 42501 otherwise").
	mustExec(t, f.owner, `INSERT INTO identity.store_grants(tenant_id,store_id,principal_id,permission)
		VALUES($1,$2,$3,'integration:execute'),($1,$2,$3,'integration:manage') ON CONFLICT DO NOTHING`, f.tenantA, f.storeA1, h.actor)
	// Rows staged by an earlier test of this shared database must not be picked up by this
	// test's poller (it leases the oldest PENDING row globally).
	mustExec(t, f.owner, `DELETE FROM claims.meta_intake WHERE state='PENDING'`)

	e.session = h.draft(t, f.storeA1)
	h.open(t, e.session, claims.MatchExact)
	e.sku = h.stock.skus[0].ID
	e.offer = h.offer(t, e.session, "A1", e.sku, 5)

	e.pageAsset, e.igAsset = miAsset(), miAsset()
	e.pageBinding = miBinding(t, e.page, e.pageAsset, "facebook", f.tenantA, f.storeA1, f.principalA)
	miRoute(t, e.page, e.pageAsset, f.tenantA, f.storeA1, e.pageBinding)
	e.igBinding = miBinding(t, e.ig, e.igAsset, "instagram", f.tenantA, f.storeA1, f.principalA)
	miInstagramRoute(t, e.ig, e.igAsset, e.igBinding)

	t.Cleanup(e.cleanup) // runs after the River/pool cleanups registered below (LIFO)
	if !o.noStore {
		e.publish(t)
	}
	if o.private {
		e.pageToken = "SENTINEL-EAAG-PAGE-" + t04Tag() + t04Tag()
		e.igToken = "SENTINEL-EAAG-IGAA-" + t04Tag() + t04Tag()
		e.registerToken(t, "facebook", e.pageBinding, e.pageAsset, []string{"pages_messaging"}, e.pageToken)
		e.registerToken(t, "instagram", e.igBinding, e.igAsset, []string{"instagram_manage_comments", "pages_read_engagement"}, e.igToken)
	}
	e.postID = e.pageAsset + "_" + mciDigits(10)
	e.mediaID = "178" + mciDigits(13)
	e.srcFB = e.mustSource(t, "page", e.pageAsset, e.postID, o.private)
	e.srcIG = e.mustSource(t, "instagram", e.igAsset, e.mediaID, o.private)

	e.intakeLogin = miRole(t, f, "commerce_claims_intake")
	if e.intakePool, err = platform.OpenClaimsIntakePool(ctx, e.intakeLogin); err != nil {
		t.Fatalf("dedicated claims intake login refused: %v", err)
	}
	t.Cleanup(e.intakePool.Close)
	if e.poller, err = claimsintake.New(ctx, e.intakePool, e.link, claimsintake.Config{Workers: 1}); err != nil {
		t.Fatal(err)
	}
	e.startConsumer(t)
	return e
}

func (e *mciEnv) cleanup() {
	t, f := e.t, e.h.f
	e.stopConsumer()
	e.stopConsumer = mciNoop
	ctx := context.Background()
	// Best effort, owner SQL: FK order intake -> sources -> intervals; the shared purge
	// (lcPurgeSessions, registered first so it runs last) then removes windows and sessions.
	for _, q := range []string{
		`UPDATE river.river_job SET queue='mci_dead' WHERE kind='external_operation_v1' AND state IN ('available','retryable','scheduled')
		   AND args->>'operation_id' IN (SELECT id::text FROM integration.operations WHERE tenant_id=$1 AND store_id=$2 AND action='meta.private_reply')`,
		`DELETE FROM claims.meta_intake WHERE tenant_id=$1 AND store_id=$2 AND session_id=$3`,
		`DELETE FROM live.claim_sources WHERE tenant_id=$1 AND store_id=$2 AND session_id=$3`,
		`DELETE FROM live.claim_window_intervals WHERE tenant_id=$1 AND store_id=$2 AND session_id=$3`,
	} {
		args := []any{f.tenantA, f.storeA1}
		if strings.Contains(q, "$3") {
			args = append(args, e.session)
		}
		if _, err := f.owner.Exec(ctx, q, args...); err != nil {
			t.Logf("cleanup (%.40s): %v", q, err)
		}
	}
	if e.origin != "" {
		_, _ = f.owner.Exec(ctx, `DELETE FROM control.storefront_domains WHERE origin=$1`, e.origin)
		_, _ = f.owner.Exec(ctx, `DELETE FROM control.storefront_publications WHERE tenant_id=$1 AND store_id=$2`, f.tenantA, f.storeA1)
	}
}

func (e *mciEnv) publish(t *testing.T) {
	t.Helper()
	f := e.h.f
	e.origin = "https://mci-" + t04Tag() + ".example.test"
	mustExec(t, f.owner, `INSERT INTO control.storefront_publications(tenant_id,store_id,published) VALUES($1,$2,true) ON CONFLICT (tenant_id,store_id) DO UPDATE SET published=true`, f.tenantA, f.storeA1)
	mustExec(t, f.owner, `INSERT INTO control.storefront_domains(tenant_id,store_id,origin,state,ownership_verified_at,tls_verified_at,valid_until,evidence_ref)
		VALUES($1,$2,$3,'ACTIVE',clock_timestamp()-interval '1 hour',clock_timestamp()-interval '1 hour',clock_timestamp()+interval '1 hour','SYNTHETIC intake gate only')`, f.tenantA, f.storeA1, e.origin)
}

func (e *mciEnv) registerToken(t *testing.T, provider, binding, asset string, scopes []string, token string) {
	t.Helper()
	f := e.h.f
	_, err := metareply.RegisterPageToken(context.Background(), e.page.registrar, e.pageKeys, metareply.Registration{
		TenantID: f.tenantA, StoreID: f.storeA1, PrincipalID: e.h.actor, BindingID: binding, Provider: provider,
		AssetID: asset, ExpectedVersion: 0, Scopes: scopes}, token)
	if err != nil {
		t.Fatalf("register synthetic Page token for %s: %v", provider, err)
	}
}

// putSource calls live.put_claim_source as the merchant (commerce_runtime login, real GUC state).
func (e *mciEnv) putSource(session, object, asset, objectID string, private bool, locale string, active bool, expected int64) (id string, err error) {
	err = platform.WithScope(context.Background(), e.h.f.runtime, e.h.token, e.h.f.storeA1, "store:read", func(tx pgx.Tx, _ platform.Scope) error {
		return tx.QueryRow(context.Background(), `SELECT live.put_claim_source($1::uuid,$2,$3,$4,$5,$6,$7,$8::bigint)::text`,
			session, object, asset, objectID, private, locale, active, expected).Scan(&id)
	})
	return id, err
}

func (e *mciEnv) mustSource(t *testing.T, object, asset, objectID string, private bool) string {
	t.Helper()
	id, err := e.putSource(e.session, object, asset, objectID, private, "zh-TW", true, 0)
	if err != nil {
		t.Fatalf("put_claim_source %s: %v", object, err)
	}
	return id
}

// startConsumer starts the claims-aware consumer on a River `river_meta` client. Pools and the
// stop hook belong to the PARENT test (e.t): a caller inside a subtest may restart the consumer, and
// a pool registered on the subtest would be closed when that subtest ends.
func (e *mciEnv) startConsumer(_ *testing.T) {
	t := e.t
	t.Helper()
	f := e.h.f
	ctx := context.Background()
	consumerPool := mcConsumer(t, e.page)
	w, err := meta.NewConsumerWorkerWithClaims(ctx, consumerPool, mcKeys(t, e.page), e.actor)
	if err != nil {
		t.Fatalf("consumer with claims: %v", err)
	}
	workers := river.NewWorkers()
	river.AddWorker(workers, w)
	riverPool := miPool(t, f, "commerce_meta_worker")
	client, err := river.NewClient(riverpgxv5.New(riverPool), &river.Config{Schema: "river_meta", Workers: workers,
		Queues: map[string]river.QueueConfig{"meta_inbox": {MaxWorkers: e.consumerWorkers}}, Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		JobTimeout: 15 * time.Second, RescueStuckJobsAfter: 30 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	if err := client.Start(ctx); err != nil {
		t.Fatal(err)
	}
	var once sync.Once
	e.stopConsumer = func() {
		once.Do(func() {
			stop, cancel := context.WithTimeout(context.Background(), 8*time.Second)
			defer cancel()
			if err := client.StopAndCancel(stop); err != nil {
				t.Errorf("consumer River stop: %v", err)
			}
			// A restart opens fresh pools; closing these now keeps a test that restarts the consumer
			// inside the harness's max_connections=60 (idle connections otherwise live until the parent
			// test ends). pgxpool.Close is idempotent, so the t.Cleanup close stays harmless.
			riverPool.Close()
			consumerPool.Close()
		})
	}
}

// soon is a comment time safely inside an OPEN window opened a moment ago and within the +120 s
// clock allowance (webhook times are whole seconds, so a floor could fall before opened_at).
func mciSoon() time.Time { return time.Now().Add(3 * time.Second) }

func mciBody(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}

// fbBody is a signed-able Page `feed` comment `add`. at==nil omits both the entry time and
// created_time (the inbox event then has a NULL occurred_at).
func mciFBBody(asset, post, comment, from, name, text string, at *time.Time, extra map[string]any) []byte {
	value := map[string]any{"item": "comment", "verb": "add", "post_id": post, "comment_id": comment,
		"from": map[string]any{"id": from, "name": name}, "message": text}
	entry := map[string]any{"id": asset}
	if at != nil {
		value["created_time"] = at.Unix()
		entry["time"] = at.Unix()
	}
	for k, v := range extra {
		if v == nil {
			delete(value, k)
		} else {
			value[k] = v
		}
	}
	entry["changes"] = []any{map[string]any{"field": "feed", "value": value}}
	return mciBody(map[string]any{"object": "page", "entry": []any{entry}})
}

func mciIGBody(asset, field, media, comment, from, username, text string, at *time.Time, extra map[string]any) []byte {
	value := map[string]any{"id": comment, "text": text, "from": map[string]any{"id": from, "username": username},
		"media": map[string]any{"id": media, "media_product_type": "FEED"}}
	entry := map[string]any{"id": asset}
	if at != nil {
		entry["time"] = at.Unix()
	}
	for k, v := range extra {
		if v == nil {
			delete(value, k)
		} else {
			value[k] = v
		}
	}
	entry["changes"] = []any{map[string]any{"field": field, "value": value}}
	return mciBody(map[string]any{"object": "instagram", "entry": []any{entry}})
}

type mciSent struct {
	ev              mcEvent
	comment, from   string
	name, text, raw string
}

func (s mciSent) String() string { return "mciSent{redacted}" }

// postFB signs and posts one Facebook comment through the real handler and waits for the
// consumer's `processed` fact. Zero `comment`/`from` mean "generate".
func (e *mciEnv) postFB(t *testing.T, comment, from, text string, at *time.Time, extra map[string]any) mciSent {
	t.Helper()
	return e.postFBTo(t, e.postID, comment, from, text, at, extra, true)
}

func (e *mciEnv) postFBTo(t *testing.T, post, comment, from, text string, at *time.Time, extra map[string]any, await bool) mciSent {
	t.Helper()
	if comment == "" {
		comment = mciDigits(15) + "_" + mciDigits(10)
	}
	if from == "" {
		from = mciDigits(15)
	}
	s := mciSent{comment: comment, from: from, text: text, name: "SENTINEL-NAME-" + t04Tag()}
	raw := mciFBBody(e.pageAsset, post, comment, from, s.name, text, at, extra)
	s.raw = string(raw)
	s.ev = mcPost(t, e.page, e.pageAsset, raw)
	if await {
		mcAwait(t, e.page, s.ev)
	}
	return s
}

func (e *mciEnv) postIG(t *testing.T, field, comment, from, text string, at *time.Time, extra map[string]any) mciSent {
	t.Helper()
	if comment == "" {
		comment = "179" + mciDigits(13)
	}
	if from == "" {
		from = mciDigits(15)
	}
	s := mciSent{comment: comment, from: from, text: text, name: "sentinel_user_" + t04Tag()}
	raw := mciIGBody(e.igAsset, field, e.mediaID, comment, from, s.name, text, at, extra)
	s.raw = string(raw)
	s.ev = mcPost(t, e.ig, e.igAsset, raw)
	mcAwait(t, e.ig, s.ev)
	return s
}

func mciAt(d time.Duration) *time.Time { v := time.Now().Add(d); return &v }

// mciActorKey is the §3 actor definition computed independently of the implementation.
func mciActorKey(raw []byte, object, asset, from string) string {
	msg, _ := json.Marshal([]string{"meta-claim-actor/v1", object, asset, from})
	mac := hmac.New(sha256.New, raw)
	mac.Write(msg)
	return hex.EncodeToString(mac.Sum(nil))
}

type mciIntake struct {
	ID, InboxEvent, State, DropReason, FailCode, AppID, Platform, ActorKey, GrammarKind, Offer, AppliedEvent string
	Attempts                                                                                                 int
	Occurred, Received, NotBefore                                                                            time.Time
	Unknown, LiveMedia                                                                                       bool
	Quantity                                                                                                 *int
	Explicit                                                                                                 *bool
}

func (e *mciEnv) intake(t *testing.T, object, asset, ref string) (m mciIntake, found bool) {
	t.Helper()
	err := e.h.f.owner.QueryRow(context.Background(), `SELECT id::text,inbox_event_id::text,state,coalesce(drop_reason,''),coalesce(fail_code,''),app_id,platform,actor_key,
		grammar_kind,coalesce(offer_id::text,''),coalesce(applied_event_id::text,''),attempts,occurred_at,received_at,not_before,unknown_keyword,live_media,quantity,explicit_quantity
		FROM claims.meta_intake WHERE object=$1 AND asset_id=$2 AND comment_ref=$3`, object, asset, ref).
		Scan(&m.ID, &m.InboxEvent, &m.State, &m.DropReason, &m.FailCode, &m.AppID, &m.Platform, &m.ActorKey, &m.GrammarKind, &m.Offer, &m.AppliedEvent,
			&m.Attempts, &m.Occurred, &m.Received, &m.NotBefore, &m.Unknown, &m.LiveMedia, &m.Quantity, &m.Explicit)
	if errors.Is(err, pgx.ErrNoRows) {
		return m, false
	}
	if err != nil {
		t.Fatalf("read intake: %v", err)
	}
	return m, true
}

func (e *mciEnv) mustIntake(t *testing.T, object, asset, ref string) mciIntake {
	t.Helper()
	m, ok := e.intake(t, object, asset, ref)
	if !ok {
		t.Fatalf("no claims.meta_intake row for %s comment", object)
	}
	return m
}

func (e *mciEnv) noIntake(t *testing.T, object, asset, ref, why string) {
	t.Helper()
	if _, ok := e.intake(t, object, asset, ref); ok {
		t.Fatalf("intake row exists but must not: %s", why)
	}
}

func (e *mciEnv) fbIntake(t *testing.T, s mciSent) mciIntake {
	t.Helper()
	return e.mustIntake(t, "page", e.pageAsset, s.comment)
}

func (e *mciEnv) igIntake(t *testing.T, s mciSent) mciIntake {
	t.Helper()
	return e.mustIntake(t, "instagram", e.igAsset, s.comment)
}

// apply drives the poller until it leases nothing (no timers, no sleeps) and returns how many
// rows it processed. A recorded failure is (leased, nil); an unrecordable one fails the test.
func (e *mciEnv) apply(t *testing.T) int {
	t.Helper()
	n := 0
	for ; n < 500; n++ {
		leased, err := e.poller.ApplyOne(context.Background())
		if err != nil {
			t.Fatalf("ApplyOne: %v", err)
		}
		if !leased {
			return n
		}
	}
	t.Fatal("ApplyOne never drained")
	return n
}

// applyConcurrently runs n goroutines that each drain the queue and returns the total leased.
func (e *mciEnv) applyConcurrently(t *testing.T, n int) int {
	t.Helper()
	var wg sync.WaitGroup
	var mu sync.Mutex
	total := 0
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 200; j++ {
				leased, err := e.poller.ApplyOne(context.Background())
				if err != nil {
					errs <- err
					return
				}
				if !leased {
					return
				}
				mu.Lock()
				total++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("concurrent ApplyOne: %v", err)
	}
	return total
}

func (e *mciEnv) eventOf(t *testing.T, m mciIntake) lcEventRow {
	t.Helper()
	if m.AppliedEvent == "" {
		t.Fatalf("intake %s has no applied_event_id (state %s)", m.State, m.State)
	}
	return lcEvent(t, e.h.f, m.AppliedEvent)
}

func (e *mciEnv) countEvents(t *testing.T) int64 {
	return miCount(t, e.h.f.owner, `SELECT count(*) FROM claims.events WHERE session_id=$1 AND source_kind='meta'`, e.session)
}

func (e *mciEnv) bundleCount(t *testing.T) int64 {
	return miCount(t, e.h.f.owner, `SELECT count(*) FROM claims.bundles WHERE session_id=$1 AND platform<>'manual'`, e.session)
}

// leaseTx opens an intake-login transaction that leased one row through claims.lease_meta_intake()
// and set the scope GUCs, exactly as ApplyOne does, so a test can call IngestMetaIntake directly.
func (e *mciEnv) leaseTx(t *testing.T, iso pgx.TxIsoLevel, scope bool) (pgx.Tx, string) {
	t.Helper()
	ctx := context.Background()
	tx, err := e.intakePool.BeginTx(ctx, pgx.TxOptions{IsoLevel: iso})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = tx.Rollback(context.Background()) })
	var id, tenant, store string
	if err := tx.QueryRow(ctx, `SELECT id::text,tenant_id::text,store_id::text FROM claims.lease_meta_intake()`).Scan(&id, &tenant, &store); err != nil {
		t.Fatalf("lease_meta_intake returned no row: %v", err)
	}
	if scope {
		if _, err := tx.Exec(ctx, `SELECT set_config('app.tenant_id',$1,true),set_config('app.store_id',$2,true)`, tenant, store); err != nil {
			t.Fatal(err)
		}
	}
	return tx, id
}

// failTrigger installs a throw-away trigger on table that raises errcode; the returned drop is
// idempotent and also runs at cleanup, so a failing test cannot leave the fault behind.
func mciFailTrigger(t *testing.T, f *testFixture, table, when, errcode string, deferred bool) (drop func()) {
	t.Helper()
	ctx := context.Background()
	name := "mci_fault_" + t04Tag()
	fn := pgx.Identifier{name}.Sanitize()
	mustExec(t, f.owner, `CREATE FUNCTION public.`+fn+`() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'mci synthetic fault' USING ERRCODE='`+errcode+`'; END $$`)
	stmt := `CREATE TRIGGER ` + fn + ` BEFORE INSERT ON ` + table + ` FOR EACH ROW `
	if deferred {
		stmt = `CREATE CONSTRAINT TRIGGER ` + fn + ` AFTER INSERT ON ` + table + ` DEFERRABLE INITIALLY DEFERRED FOR EACH ROW `
	}
	if when != "" {
		stmt += `WHEN (` + when + `) `
	}
	mustExec(t, f.owner, stmt+`EXECUTE FUNCTION public.`+fn+`()`)
	var once sync.Once
	drop = func() {
		once.Do(func() {
			_, _ = f.owner.Exec(ctx, `DROP TRIGGER IF EXISTS `+fn+` ON `+table)
			_, _ = f.owner.Exec(ctx, `DROP FUNCTION IF EXISTS public.`+fn+`()`)
		})
	}
	t.Cleanup(drop)
	return drop
}

func mciBackdate(t *testing.T, f *testFixture, id, occurred, received string) {
	t.Helper()
	mustExec(t, f.owner, `UPDATE claims.meta_intake SET occurred_at=`+occurred+`,received_at=`+received+` WHERE id=$1`, id)
}

func mciSQLIs(err error, codes ...string) bool {
	got := miSQLState(err)
	for _, c := range codes {
		if got == c {
			return true
		}
	}
	return false
}

func mciConflict(err error) bool {
	return mciSQLIs(err, "PT409", "23505") || errors.Is(err, command.ErrConflict)
}

// ---------------------------------------------------------------------------------------
// MCI04: signed replay end to end, atomicity, apply failure paths, crash, River guards
// ---------------------------------------------------------------------------------------

// TestMetaClaimsMCI04SignedReplayToAcceptedClaim: a signed Facebook `feed` comment and an
// Instagram live comment on bound objects become one text-free staged row each and, once
// applied, one ACCEPTED claims event with source_kind 'meta' whose source_event_id is the inbox
// event id, a platform bundle without label or principal, and one absolute line (§3, §4, §5).
func TestMetaClaimsMCI04SignedReplayToAcceptedClaim(t *testing.T) {
	e := mciSetup(t, mciOpts{})
	f := e.h.f
	fromFB, fromIG := mciDigits(15), mciDigits(15)
	fb := e.postFB(t, "", fromFB, "A1+2", mciAt(3*time.Second), nil)
	ig := e.postIG(t, "live_comments", "", fromIG, "a1", mciAt(3*time.Second), nil)

	row := e.fbIntake(t, fb)
	if row.State != "PENDING" || row.InboxEvent != fb.ev.id || row.Platform != "facebook" || row.Offer != e.offer.ID || row.Unknown ||
		row.Quantity == nil || *row.Quantity != 2 || row.Explicit == nil || !*row.Explicit || row.GrammarKind != "MATCH" || row.AppID != miApp {
		t.Fatalf("staged FB row wrong: %+v", row)
	}
	if want := mciActorKey(e.actorRaw, "page", e.pageAsset, fromFB); row.ActorKey != want {
		t.Fatal("staged actor_key is not HMAC(K_actor, page|asset|from.id) of §3")
	}
	if !row.Occurred.Truncate(time.Second).Equal(row.Occurred) || row.Received.Before(row.Occurred.Add(-time.Minute)) {
		t.Fatalf("staged times: occurred=%v received=%v", row.Occurred, row.Received)
	}
	igRow := e.igIntake(t, ig)
	if igRow.Platform != "instagram" || !igRow.LiveMedia || igRow.ActorKey != mciActorKey(e.actorRaw, "instagram", e.igAsset, fromIG) || igRow.Quantity == nil || *igRow.Quantity != 1 {
		t.Fatalf("staged IG live row wrong: %+v", igRow)
	}
	if igRow.ActorKey == row.ActorKey {
		t.Fatal("two platforms share one actor key")
	}
	if n := e.apply(t); n != 2 {
		t.Fatalf("poller processed %d rows want 2", n)
	}
	for _, c := range []struct {
		name string
		row  mciIntake
		s    mciSent
		plat string
		qty  int64
		key  string
	}{{"facebook", e.fbIntake(t, fb), fb, "facebook", 2, row.ActorKey}, {"instagram", e.igIntake(t, ig), ig, "instagram", 1, igRow.ActorKey}} {
		if c.row.State != "APPLIED" || c.row.AppliedEvent == "" || c.row.DropReason != "" || c.row.FailCode != "" {
			t.Fatalf("%s intake after apply: %+v", c.name, c.row)
		}
		ev := lcEvent(t, f, c.row.AppliedEvent)
		if ev.sourceKind != "meta" || ev.platform != c.plat || ev.outcome != "ACCEPTED" || ev.offer != e.offer.ID || ev.quantity != c.qty ||
			ev.source != c.row.InboxEvent || ev.generation != 1 || ev.principal != "" || ev.bundle == "" || ev.explicit == nil {
			t.Fatalf("%s claims event wrong: %+v", c.name, ev)
		}
		var platform, actor string
		var noLabel bool
		var lines int
		if err := f.owner.QueryRow(context.Background(), `SELECT platform,actor_key,label IS NULL,line_count FROM claims.bundles WHERE id=$1`, ev.bundle).Scan(&platform, &actor, &noLabel, &lines); err != nil {
			t.Fatal(err)
		}
		if platform != c.plat || actor != c.key || !noLabel || lines != 1 {
			t.Fatalf("%s bundle wrong: %s %t %d", c.name, platform, noLabel, lines)
		}
		if q := miCount(t, f.owner, `SELECT quantity FROM claims.lines WHERE bundle_id=$1 AND offer_id=$2`, ev.bundle, e.offer.ID); q != c.qty {
			t.Fatalf("%s line quantity %d want %d", c.name, q, c.qty)
		}
	}
	// Applying nothing twice: an empty queue is not an error and writes nothing.
	if e.apply(t) != 0 || e.countEvents(t) != 2 {
		t.Fatal("re-drain changed state")
	}
}

// TestMetaClaimsMCI04NonQualifyingKeepsSocialFactOnly: the §3 fail-closed list at the real
// consumer. The social fact commits exactly as before, and no intake row exists.
func TestMetaClaimsMCI04NonQualifyingKeepsSocialFactOnly(t *testing.T) {
	e := mciSetup(t, mciOpts{})
	f := e.h.f
	other := e.pageAsset + "_" + mciDigits(10)
	cases := []struct {
		name string
		s    func() mciSent
	}{
		{"unbound object", func() mciSent { return e.postFBTo(t, other, "", "", "A1", mciAt(3*time.Second), nil, true) }},
		{"reply to a comment (parent_id)", func() mciSent {
			return e.postFB(t, "", "", "A1", mciAt(3*time.Second), map[string]any{"parent_id": e.postID + "9"})
		}},
		{"seller's own comment", func() mciSent { return e.postFB(t, "", e.pageAsset, "A1", mciAt(3*time.Second), nil) }},
		{"edit verb", func() mciSent {
			return e.postFB(t, "", "", "A1", mciAt(3*time.Second), map[string]any{"verb": "edit"})
		}},
		{"missing from", func() mciSent { return e.postFB(t, "", "", "A1", mciAt(3*time.Second), map[string]any{"from": nil}) }},
		{"NULL occurred_at (no entry time, no created_time)", func() mciSent { return e.postFB(t, "", "", "A1", nil, nil) }},
		{"text over 256 bytes", func() mciSent { return e.postFB(t, "", "", strings.Repeat("x", 257), mciAt(3*time.Second), nil) }},
	}
	for _, c := range cases {
		s := c.s()
		e.noIntake(t, "page", e.pageAsset, s.comment, c.name)
		if miCount(t, f.owner, `SELECT count(*) FROM social.comment_events WHERE event_id=$1`, s.ev.id) != 1 ||
			miCount(t, f.owner, `SELECT count(*) FROM meta_inbox.audit_events WHERE event_id=$1 AND action='processed'`, s.ev.id) != 1 {
			t.Fatalf("%s: social fact or processed mark missing (must commit as before)", c.name)
		}
	}
	if e.countEvents(t) != 0 || e.bundleCount(t) != 0 {
		t.Fatal("a non-qualifying comment reached claims")
	}
}

// TestMetaClaimsMCI04NoMatchAndUnknownKeywordShapes: NO_MATCH and unresolved heads are staged
// (they count toward the staging bound) but persist no keyword, offer or bundle (§3, claims R5).
func TestMetaClaimsMCI04NoMatchAndUnknownKeywordShapes(t *testing.T) {
	e := mciSetup(t, mciOpts{})
	unknown := "Q" + strings.ToUpper(t04Tag()[:8])
	nm := e.postFB(t, "", "", "hello, how much is it?", mciAt(3*time.Second), nil)
	uk := e.postFB(t, "", "", unknown, mciAt(3*time.Second), nil)
	a := e.fbIntake(t, nm)
	if a.GrammarKind != "NO_MATCH" || a.Offer != "" || a.Unknown {
		t.Fatalf("NO_MATCH staged wrong: %+v", a)
	}
	b := e.fbIntake(t, uk)
	if b.GrammarKind != "MATCH" || b.Offer != "" || !b.Unknown {
		t.Fatalf("unknown keyword staged wrong: %+v", b)
	}
	e.apply(t)
	for name, row := range map[string]mciIntake{"NO_MATCH": e.fbIntake(t, nm), "UNKNOWN_KEYWORD": e.fbIntake(t, uk)} {
		if row.State != "APPLIED" {
			t.Fatalf("%s row not applied: %+v", name, row)
		}
		ev := e.eventOf(t, row)
		if ev.outcome != "REJECTED" || ev.reason != name || ev.bundle != "" || ev.offer != "" || ev.quantity != 0 || ev.explicit != nil {
			t.Fatalf("%s event shape wrong: %+v", name, ev)
		}
	}
	if e.bundleCount(t) != 0 {
		t.Fatal("a rejected comment created a bundle")
	}
	if hits := lcFind(t, e.h.f, unknown, nil, "claims", "live", "integration", "ops", "river"); len(hits) != 0 {
		t.Fatalf("unresolved keyword persisted in %v", hits)
	}
}

// TestMetaClaimsMCI04ConsumerAtomicity: a forced failure at the social fact, the stage call, the
// processed mark or COMMIT leaves no fact, no intake row, no processed mark and a retryable River
// job; after the fault is removed the same job produces exactly one of each.
func TestMetaClaimsMCI04ConsumerAtomicity(t *testing.T) {
	e := mciSetup(t, mciOpts{})
	f := e.h.f
	ctx := context.Background()
	points := []struct {
		name, table, when string
		deferred          bool
	}{
		{"social fact", "social.comment_events", "", false},
		{"stage", "claims.meta_intake", "", false},
		{"processed mark", "meta_inbox.audit_events", "NEW.action='processed'", false},
		{"COMMIT", "claims.meta_intake", "", true},
	}
	for _, p := range points {
		p := p
		t.Run(p.name, func(t *testing.T) {
			drop := mciFailTrigger(t, f, p.table, p.when, "P0001", p.deferred)
			s := e.postFBTo(t, e.postID, "", "", "A1", mciAt(3*time.Second), nil, false)
			deadline := time.Now().Add(15 * time.Second)
			for {
				var state string
				var attempt int
				if err := f.owner.QueryRow(ctx, `SELECT state,attempt FROM `+mcJobTable(t, f)+` WHERE id=$1`, s.ev.job).Scan(&state, &attempt); err != nil {
					t.Fatal(err)
				}
				if state == "retryable" && attempt >= 1 {
					break
				}
				if state == "completed" {
					t.Fatalf("consumer completed despite the forced %s failure", p.name)
				}
				if time.Now().After(deadline) {
					t.Fatalf("job never became retryable (state=%s attempt=%d)", state, attempt)
				}
				time.Sleep(20 * time.Millisecond) // polls the job state; no race conclusion depends on it
			}
			e.stopConsumer()
			e.stopConsumer = mciNoop
			if miCount(t, f.owner, `SELECT count(*) FROM social.comment_events WHERE event_id=$1`, s.ev.id) != 0 ||
				miCount(t, f.owner, `SELECT count(*) FROM meta_inbox.audit_events WHERE event_id=$1 AND action='processed'`, s.ev.id) != 0 ||
				miCount(t, f.owner, `SELECT count(*) FROM meta_inbox.events WHERE id=$1 AND terminal_reason IS NOT NULL`, s.ev.id) != 0 {
				t.Fatal("failed consumer transaction left a social fact, processed mark or terminal reason")
			}
			e.noIntake(t, "page", e.pageAsset, s.comment, "failed consumer transaction")
			// Fault removed: the same River job now yields exactly one fact, one mark, one intake.
			drop()
			// Make the retry due as River's JobScheduler would (retryable -> available). Only moving scheduled_at left the
			// promotion to the leader-only scheduler (5 s tick, staggered start, 15 s TTL of a leader that did not resign),
			// which can outlast mcAwait's 10 s (R2 release gate 2026-09-30: "actual River consumer did not complete").
			if tag, err := f.owner.Exec(ctx, `UPDATE `+mcJobTable(t, f)+` SET state='available',scheduled_at=clock_timestamp()
			 WHERE id=$1 AND state='retryable'`, s.ev.job); err != nil || tag.RowsAffected() != 1 {
				t.Fatalf("retryable job not made due: rows=%d err=%v", tag.RowsAffected(), err)
			}
			e.startConsumer(t)
			mcAwait(t, e.page, s.ev)
			if miCount(t, f.owner, `SELECT count(*) FROM social.comment_events WHERE event_id=$1`, s.ev.id) != 1 || e.fbIntake(t, s).State != "PENDING" {
				t.Fatal("retry after the fault did not produce exactly one fact and one PENDING intake")
			}
		})
	}
}

// TestMetaClaimsMCI04ApplyFailurePaths: §5.3 error classes. Invalid/CHECK-class failures are
// FAILED after one attempt and never retried; transient ones back off then apply; attempts stop at 10.
func TestMetaClaimsMCI04ApplyFailurePaths(t *testing.T) {
	e := mciSetup(t, mciOpts{})
	f := e.h.f
	ctx := context.Background()
	stage := func(t *testing.T) (mciSent, mciIntake) {
		s := e.postFB(t, "", "", "A1", mciAt(3*time.Second), nil)
		return s, e.fbIntake(t, s)
	}
	for _, c := range []struct {
		name, code string
		failCodes  []string
	}{{"22023 invalid", "22023", []string{"invalid"}}, {"23514 check", "23514", []string{"invalid", "sqlstate_23514"}}} {
		c := c
		t.Run(c.name, func(t *testing.T) {
			s, before := stage(t)
			drop := mciFailTrigger(t, f, "claims.events", "NEW.source_kind='meta'", c.code, false)
			leased, err := e.poller.ApplyOne(ctx)
			drop()
			if err != nil || !leased {
				t.Fatalf("ApplyOne leased=%t err=%v (a recorded failure is (true,nil))", leased, err)
			}
			row := e.fbIntake(t, s)
			// claims.mapError turns 22023/23514 into command.ErrInvalid, so the poller may record either
			// the mapped `invalid` or the raw `sqlstate_23514`; both are final and operator-visible.
			okCode := false
			for _, want := range c.failCodes {
				okCode = okCode || row.FailCode == want
			}
			if row.State != "FAILED" || row.Attempts != 1 || !okCode || row.AppliedEvent != "" {
				t.Fatalf("%s: want FAILED once with one of %v, got %+v", c.name, c.failCodes, row)
			}
			if e.apply(t) != 0 || e.countEvents(t) != 0 || e.bundleCount(t) != 0 || before.ID != row.ID {
				t.Fatal("a final failure was retried or left claims rows behind")
			}
			mustExec(t, f.owner, `DELETE FROM claims.meta_intake WHERE id=$1`, row.ID)
		})
	}
	t.Run("transient 40P01 backs off then applies", func(t *testing.T) {
		s, _ := stage(t)
		drop := mciFailTrigger(t, f, "claims.events", "NEW.source_kind='meta'", "40P01", false)
		leased, err := e.poller.ApplyOne(ctx)
		drop()
		if err != nil || !leased {
			t.Fatalf("ApplyOne leased=%t err=%v", leased, err)
		}
		row := e.fbIntake(t, s)
		if row.State != "PENDING" || row.Attempts != 1 || !row.NotBefore.After(time.Now().Add(500*time.Millisecond)) {
			t.Fatalf("transient failure not backed off: %+v", row)
		}
		if e.apply(t) != 0 {
			t.Fatal("a backed-off row was leased before not_before")
		}
		mustExec(t, f.owner, `UPDATE claims.meta_intake SET not_before=clock_timestamp()-interval '1 second' WHERE id=$1`, row.ID)
		if e.apply(t) != 1 {
			t.Fatal("row not re-polled after its backoff elapsed")
		}
		if got := e.fbIntake(t, s); got.State != "APPLIED" || e.countEvents(t) != 1 {
			t.Fatalf("retry did not apply exactly once: %+v", got)
		}
	})
	t.Run("attempt cap 10 fails the row", func(t *testing.T) {
		s, _ := stage(t)
		drop := mciFailTrigger(t, f, "claims.events", "NEW.source_kind='meta' AND NEW.source_event_id IS NOT NULL", "40P01", false)
		for i := 1; i <= 10; i++ {
			mustExec(t, f.owner, `UPDATE claims.meta_intake SET not_before=clock_timestamp()-interval '1 second' WHERE object='page' AND asset_id=$1 AND comment_ref=$2 AND state='PENDING'`, e.pageAsset, s.comment)
			if leased, err := e.poller.ApplyOne(ctx); err != nil || !leased {
				t.Fatalf("attempt %d: leased=%t err=%v", i, leased, err)
			}
		}
		drop()
		row := e.fbIntake(t, s)
		if row.State != "FAILED" || row.Attempts != 10 || row.FailCode == "" {
			t.Fatalf("attempts cap: %+v", row)
		}
		if e.apply(t) != 0 {
			t.Fatal("a FAILED row was leased again")
		}
	})
}

// TestMetaClaimsMCI04IngestPreconditions: IngestMetaIntake's frozen preconditions all return
// command.ErrInvalid and write nothing (an unleased id, a non-READ-COMMITTED tx, no scope GUCs).
func TestMetaClaimsMCI04IngestPreconditions(t *testing.T) {
	e := mciSetup(t, mciOpts{})
	ctx := context.Background()
	s := e.postFB(t, "", "", "A1", mciAt(3*time.Second), nil)
	row := e.fbIntake(t, s)
	before := miCount(t, e.h.f.owner, `SELECT count(*) FROM claims.events WHERE session_id=$1`, e.session)

	tx, _ := e.leaseTx(t, pgx.ReadCommitted, true)
	if _, err := claims.IngestMetaIntake(ctx, tx, randomUUID()); !errors.Is(err, command.ErrInvalid) {
		t.Fatalf("an intake id that was not leased in this tx: %v", err)
	}
	_ = tx.Rollback(ctx)
	// REPEATABLE READ: no lease can exist there (contract §4.4/§5.3: lease_meta_intake runs only in a READ
	// COMMITTED NOGUC transaction and raises 22023 otherwise), so IngestMetaIntake is given the pending row's id
	// under the right scope GUCs and must still refuse the isolation level.
	rr, err := e.intakePool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead})
	if err != nil {
		t.Fatal(err)
	}
	var pgErr *pgconn.PgError
	if _, err := rr.Exec(ctx, `SELECT * FROM claims.lease_meta_intake()`); !errors.As(err, &pgErr) || pgErr.Code != "22023" {
		t.Fatalf("lease_meta_intake under REPEATABLE READ: %v, want 22023", err)
	}
	_ = rr.Rollback(ctx)
	if rr, err = e.intakePool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead}); err != nil {
		t.Fatal(err)
	}
	if _, err := rr.Exec(ctx, `SELECT set_config('app.tenant_id',$1,true),set_config('app.store_id',$2,true)`, e.h.f.tenantA, e.h.f.storeA1); err != nil {
		t.Fatal(err)
	}
	if _, err := claims.IngestMetaIntake(ctx, rr, row.ID); !errors.Is(err, command.ErrInvalid) {
		t.Fatalf("REPEATABLE READ transaction accepted: %v", err)
	}
	_ = rr.Rollback(ctx)
	tx, id := e.leaseTx(t, pgx.ReadCommitted, false)
	if _, err := claims.IngestMetaIntake(ctx, tx, id); !errors.Is(err, command.ErrInvalid) {
		t.Fatalf("missing scope GUCs accepted: %v", err)
	}
	_ = tx.Rollback(ctx)
	tx, id = e.leaseTx(t, pgx.ReadCommitted, true)
	if _, err := tx.Exec(ctx, `SELECT set_config('app.principal_id',$1,true)`, randomUUID()); err != nil {
		t.Fatal(err)
	}
	if _, err := claims.IngestMetaIntake(ctx, tx, id); !errors.Is(err, command.ErrInvalid) {
		t.Fatalf("app.principal_id set (not a system transaction) accepted: %v", err)
	}
	_ = tx.Rollback(ctx)
	if miCount(t, e.h.f.owner, `SELECT count(*) FROM claims.events WHERE session_id=$1`, e.session) != before || e.fbIntake(t, s).State != "PENDING" || row.ID == "" {
		t.Fatal("a rejected precondition wrote a claims event or changed the intake state (IngestMetaIntake never changes intake state)")
	}
	// Positive control: a proper leased tx applies and reports BundleCreated once.
	tx, id = e.leaseTx(t, pgx.ReadCommitted, true)
	res, err := claims.IngestMetaIntake(ctx, tx, id)
	if err != nil || res.Outcome != claims.OutcomeAccepted || !res.BundleCreated || res.Duplicate || res.EventID == "" {
		t.Fatalf("proper IngestMetaIntake: %+v %v", res, err)
	}
	_ = tx.Rollback(ctx)
}

// TestMetaClaimsMCI04CrashMidApplyIsRepolledAndAppliedOnce: build cmd/claims-worker, let its
// apply block on a window lock the test holds, SIGKILL it, and prove the row is re-polled by
// another poller and applied exactly once (attempts unchanged: the documented crash limit).
func TestMetaClaimsMCI04CrashMidApplyIsRepolledAndAppliedOnce(t *testing.T) {
	e := mciSetup(t, mciOpts{})
	f := e.h.f
	ctx := context.Background()
	s := e.postFB(t, "", "", "A1+2", mciAt(3*time.Second), nil)
	staged := e.fbIntake(t, s)
	binary := mrBuild(t, "../../cmd/claims-worker", "claims-worker")

	// The test holds the claim window row so the child's FOR SHARE (§5.3 step 2) must wait.
	lock, err := f.owner.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Rollback(ctx)
	if _, err := lock.Exec(ctx, `SELECT 1 FROM live.claim_windows WHERE tenant_id=$1 AND store_id=$2 AND session_id=$3 FOR UPDATE`, f.tenantA, f.storeA1, e.session); err != nil {
		t.Fatal(err)
	}
	child := mrLaunch(t, binary, "claims-worker", e.workerEnv(t, ""))
	user := e.intakePool.Config().ConnConfig.User
	deadline := time.Now().Add(20 * time.Second)
	for {
		if miCount(t, f.owner, `SELECT count(*) FROM pg_stat_activity WHERE usename=$1 AND wait_event_type='Lock' AND state='active'`, user) >= 1 {
			break
		}
		select {
		case err := <-child.done:
			child.exited = true
			t.Fatalf("claims-worker exited before blocking on the window lock: %v log=%s", err, child.logPath)
		default:
		}
		if time.Now().After(deadline) {
			t.Fatalf("claims-worker never blocked on the window lock; log=%s", child.logPath)
		}
		time.Sleep(20 * time.Millisecond) // observes a PG lock wait, not an elapsed-time race
	}
	if m := e.fbIntake(t, s); m.State != "PENDING" {
		t.Fatalf("row state changed while the apply was blocked: %s", m.State)
	}
	mrStop(t, child, syscall.SIGKILL, false)
	if err := lock.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	// The dead backend's transaction must abort at its next client round trip; poll until the
	// row is leasable again (SKIP LOCKED hides it meanwhile).
	deadline = time.Now().Add(15 * time.Second)
	for {
		leased, err := e.poller.ApplyOne(ctx)
		if err != nil {
			t.Fatalf("re-poll after crash: %v", err)
		}
		if leased {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("crashed row was never re-polled")
		}
		time.Sleep(50 * time.Millisecond) // waits for PostgreSQL to notice the dead client
	}
	got := e.fbIntake(t, s)
	if got.State != "APPLIED" || got.Attempts != staged.Attempts || e.countEvents(t) != 1 || e.bundleCount(t) != 1 {
		t.Fatalf("after crash: %+v events=%d bundles=%d (want APPLIED, attempts unchanged, one event, one bundle)", got, e.countEvents(t), e.bundleCount(t))
	}
	if e.apply(t) != 0 {
		t.Fatal("applied row leased again")
	}
}

// workerEnv is the complete environment of a real cmd/claims-worker process (names from
// meta-intake-reply.md). graphURL "" leaves the Graph base at its default (never reached: the
// crash test has no private-reply source).
func (e *mciEnv) workerEnv(t *testing.T, graphURL string) []string {
	t.Helper()
	f := e.h.f
	workerLogin := miRole(t, f, "commerce_worker")
	env := []string{
		"COMMERCE_CLAIMS_WORKER_ENABLED=1",
		"COMMERCE_CLAIMS_INTAKE_DATABASE_URL=" + e.intakePool.Config().ConnString(),
		"COMMERCE_WORKER_DATABASE_URL=" + workerLogin,
		// claims-retention-purge-v1 §5 (U08): required whenever the worker is enabled; the job runs report-only here.
		"COMMERCE_RETENTION_JOB_DATABASE_URL=" + miRole(t, f, "commerce_retention_job"),
		"COMMERCE_CLAIMS_REPLY_LINK_KEY=" + base64.StdEncoding.EncodeToString(e.linkRaw),
		"COMMERCE_META_PAGE_TOKEN_ACTIVE_KEY_ID=pt_key_1",
		`COMMERCE_META_PAGE_TOKEN_KEYS_JSON={"keys":[{"id":"pt_key_1","key_base64":"` + base64.StdEncoding.EncodeToString(e.pageKeyRaw) + `"}]}`,
		"COMMERCE_META_GRAPH_VERSION=v99.0",
	}
	if graphURL != "" {
		env = append(env, "COMMERCE_META_GRAPH_BASE_URL="+graphURL)
	}
	return env
}

// TestMetaClaimsMCI04RiverGuards: §5.4 probes from the intake login, real SQL: another job kind,
// a malformed external_operation_v1 job, a job without its operation, kind/args/queue/unique_key
// updates; a real River InsertTx of the allowed shape passes (ON CONFLICT ... SET kind path
// included); commerce_integration_writer can read xmin of the job it must verify.
func TestMetaClaimsMCI04RiverGuards(t *testing.T) {
	e := mciSetup(t, mciOpts{})
	f := e.h.f
	ctx := context.Background()
	pool := e.intakePool

	insert := func(kind, args, queue string, unique any) error {
		_, err := pool.Exec(ctx, `INSERT INTO river.river_job(kind,args,queue,max_attempts,unique_key) VALUES($1,$2::jsonb,$3,3,$4)`, kind, args, queue, unique)
		return err
	}
	op := randomUUID()
	goodArgs := fmt.Sprintf(`{"operation_id":%q,"version":1}`, op)
	cases := []struct {
		name string
		err  error
	}{
		{"other kind", insert("mci_other_kind", `{}`, "default", nil)},
		{"other queue", insert("external_operation_v1", goodArgs, "mci_queue", nil)},
		{"unique_key set", insert("external_operation_v1", goodArgs, "default", []byte("k"))},
		{"extra args", insert("external_operation_v1", fmt.Sprintf(`{"operation_id":%q,"version":1,"x":1}`, op), "default", nil)},
		{"wrong version", insert("external_operation_v1", fmt.Sprintf(`{"operation_id":%q,"version":2}`, op), "default", nil)},
		{"non-uuid operation", insert("external_operation_v1", `{"operation_id":"nope","version":1}`, "default", nil)},
	}
	for _, c := range cases {
		if c.err == nil || !mciSQLIs(c.err, "22023") {
			t.Fatalf("%s from the intake login: SQLSTATE=%s err=%v want 22023", c.name, miSQLState(c.err), c.err)
		}
	}
	if n := miCount(t, f.owner, `SELECT count(*) FROM river.river_job WHERE kind='mci_other_kind' OR args->>'operation_id'=$1`, op); n != 0 {
		t.Fatalf("a guarded insert left %d job rows", n)
	}

	// A job without its operation row: the shape is admitted, the commit is not.
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO river.river_job(kind,args,max_attempts) VALUES('external_operation_v1',$1::jsonb,3)`, goodArgs); err != nil {
		_ = tx.Rollback(ctx)
		t.Fatalf("well-formed job insert refused before commit: %v", err)
	}
	if err := tx.Commit(ctx); err == nil {
		t.Fatal("a job without an operations row committed")
	}
	if n := miCount(t, f.owner, `SELECT count(*) FROM river.river_job WHERE args->>'operation_id'=$1`, op); n != 0 {
		t.Fatal("the orphan job survived the failed commit")
	}

	// A real River InsertTx of external_operation_v1 (the production shape) is not a 42501.
	insertOnly, err := river.NewClient(riverpgxv5.New(pool), &river.Config{Schema: "river", Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err != nil {
		t.Fatal(err)
	}
	tx, err = pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	jobID, err := integration.InsertOperationJob(ctx, insertOnly, tx, randomUUID())
	if err != nil || jobID < 1 {
		_ = tx.Rollback(ctx)
		t.Fatalf("River InsertTx from the intake login: id=%d err=%v (42501=%t)", jobID, err, mciSQLIs(err, "42501"))
	}
	// River's InsertTx emits ON CONFLICT (unique_key) DO UPDATE SET kind=EXCLUDED.kind, which needs
	// UPDATE(kind); reaching here without 42501 covers that statement shape.
	_ = tx.Rollback(ctx)

	// UPDATE guards. The dummy job is inserted by the owner (kind is not external_operation_v1,
	// so the deferred operation check does not apply) and removed at the end.
	var dummy int64
	if err := f.owner.QueryRow(ctx, `INSERT INTO river.river_job(kind,args,queue,max_attempts,state,scheduled_at) VALUES('mci_dummy','{}','mci_dead',3,'scheduled',clock_timestamp()+interval '1 day') RETURNING id`).Scan(&dummy); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = f.owner.Exec(ctx, `DELETE FROM river.river_job WHERE id=$1`, dummy) })
	if _, err := pool.Exec(ctx, `UPDATE river.river_job SET kind='external_operation_v1' WHERE id=$1`, dummy); !mciSQLIs(err, "22023") {
		t.Fatalf("kind change from the intake login: SQLSTATE=%s err=%v want 22023", miSQLState(err), err)
	}
	if _, err := pool.Exec(ctx, `UPDATE river.river_job SET kind=kind WHERE id=$1`, dummy); err != nil {
		t.Fatalf("River's own no-op kind update refused: %v", err)
	}
	for col, val := range map[string]string{"args": `'{"a":1}'::jsonb`, "queue": `'other'`, "unique_key": `decode('01','hex')`} {
		if _, err := pool.Exec(ctx, `UPDATE river.river_job SET `+col+`=`+val+` WHERE id=$1`, dummy); err == nil {
			t.Fatalf("intake login changed river_job.%s", col)
		} else if !mciSQLIs(err, "22023", "42501") {
			t.Fatalf("UPDATE %s SQLSTATE=%s want 22023 or 42501", col, miSQLState(err))
		}
	}
	if _, err := pool.Exec(ctx, `DELETE FROM river.river_job WHERE id=$1`, dummy); !mciSQLIs(err, "42501") {
		t.Fatalf("intake login DELETE on river_job SQLSTATE=%s want 42501", miSQLState(err))
	}
	// plan_claim_reply reads xmin with commerce_integration_writer's table-level SELECT (§4.3, §5.4).
	writer := miPool(t, f, "commerce_integration_writer")
	var xmin string
	if err := writer.QueryRow(ctx, `SELECT xmin::text FROM river.river_job WHERE id=$1`, dummy).Scan(&xmin); err != nil || xmin == "" {
		t.Fatalf("commerce_integration_writer cannot read xmin of river.river_job: %v", err)
	}
	if _, err := writer.Exec(ctx, `UPDATE river.river_job SET queue='x' WHERE id=$1`, dummy); !mciSQLIs(err, "42501") {
		t.Fatalf("commerce_integration_writer holds more than SELECT on river_job: SQLSTATE=%s", miSQLState(err))
	}
}

// ---------------------------------------------------------------------------------------
// MCI05: idempotency, per-comment uniqueness, redelivery, privacy of unresolved keywords
// ---------------------------------------------------------------------------------------

// TestMetaClaimsMCI05OneIntakePerComment: however a comment id arrives more than once (the same
// bytes again, a different payload hash, the other Instagram kind, a second app routed to the
// same asset, 20 concurrent consumer runs), there is one intake row, one claims event and an
// unchanged quantity (§4 UNIQUE(object,asset_id,comment_ref), §5.1).
func TestMetaClaimsMCI05OneIntakePerComment(t *testing.T) {
	e := mciSetup(t, mciOpts{})
	f := e.h.f
	ctx := context.Background()
	count := func(object, asset, ref string) int64 {
		return miCount(t, f.owner, `SELECT count(*) FROM claims.meta_intake WHERE object=$1 AND asset_id=$2 AND comment_ref=$3`, object, asset, ref)
	}

	t.Run("redelivered webhook is a duplicate", func(t *testing.T) {
		s := e.postFB(t, "", "", "A1", mciAt(3*time.Second), nil)
		jobs := miCount(t, f.owner, `SELECT count(*) FROM `+mcJobTable(t, f)+` WHERE kind='meta_inbox_v1'`)
		status, body := miPost(t, e.page, []byte(s.raw))
		miStatus(t, status, body, 200)
		if miCount(t, f.owner, `SELECT count(*) FROM `+mcJobTable(t, f)+` WHERE kind='meta_inbox_v1'`) != jobs || count("page", e.pageAsset, s.comment) != 1 {
			t.Fatal("byte-identical redelivery created a job or a second intake")
		}
	})

	t.Run("same comment id with a different payload hash: late A1 does not reset A1+3", func(t *testing.T) {
		comment, from := mciDigits(15)+"_"+mciDigits(10), mciDigits(15)
		first := e.postFB(t, comment, from, "A1+3", mciAt(3*time.Second), nil)
		second := e.postFB(t, comment, from, "A1", mciAt(4*time.Second), nil)
		if first.ev.id == second.ev.id || first.ev.hash == second.ev.hash {
			t.Fatal("test bug: the two deliveries are the same inbox event")
		}
		row := e.fbIntake(t, first)
		if count("page", e.pageAsset, comment) != 1 || row.InboxEvent != first.ev.id || row.Quantity == nil || *row.Quantity != 3 {
			t.Fatalf("second payload replaced or duplicated the intake: %+v", row)
		}
		e.apply(t)
		ev := e.eventOf(t, e.fbIntake(t, first))
		if ev.quantity != 3 || miCount(t, f.owner, `SELECT count(*) FROM claims.events WHERE source_event_id=ANY($1::uuid[])`, []string{first.ev.id, second.ev.id}) != 1 {
			t.Fatalf("one claims event with quantity 3 expected, got %+v", ev)
		}
		if q := miCount(t, f.owner, `SELECT quantity FROM claims.lines WHERE bundle_id=$1 AND offer_id=$2`, ev.bundle, e.offer.ID); q != 3 {
			t.Fatalf("late A1 reset the line to %d", q)
		}
	})

	t.Run("one Instagram comment as comments and live_comments", func(t *testing.T) {
		comment, from := "179"+mciDigits(13), mciDigits(15)
		a := e.postIG(t, "comments", comment, from, "A1", mciAt(3*time.Second), nil)
		b := e.postIG(t, "live_comments", comment, from, "A1", mciAt(3*time.Second), nil)
		if a.ev.id == b.ev.id || count("instagram", e.igAsset, comment) != 1 {
			t.Fatal("the IG kind pair produced a second intake row")
		}
		if row := e.igIntake(t, a); row.InboxEvent != a.ev.id || row.LiveMedia {
			t.Fatalf("first delivery must win (comments, live_media=false): %+v", row)
		}
		before := e.countEvents(t)
		e.apply(t)
		if e.countEvents(t) != before+1 {
			t.Fatal("the IG kind pair produced two claims events")
		}
	})

	t.Run("one comment through two apps routed to one asset", func(t *testing.T) {
		const app2 = "223456789012345"
		v2, err := meta.NewVerifier(meta.Config{AppID: app2, Object: "page", AppSecret: miSecret, VerifyToken: "meta-inbox-verify-token"})
		if err != nil {
			t.Fatal(err)
		}
		inbox, err := meta.NewInbox(ctx, e.page.ingress, mcKeys(t, e.page))
		if err != nil {
			t.Fatal(err)
		}
		h2, err := meta.NewInboxHandler(v2, inbox)
		if err != nil {
			t.Fatal(err)
		}
		m2 := e.page
		m2.verifier, m2.handler = v2, h2
		var route string
		var epoch int64
		if err := e.page.registrar.QueryRow(ctx, `SELECT * FROM meta_inbox.activate_route($1,'page',$2,$3,$4,$5,1,$6,$7,0)`, app2, e.pageAsset, f.tenantA, f.storeA1, e.pageBinding,
			strings.Repeat("d", 64), time.Now().Add(time.Hour)).Scan(&route, &epoch); err != nil {
			t.Fatalf("a second app cannot be routed to an asset of the same store and binding: %v", err)
		}
		comment, from := mciDigits(15)+"_"+mciDigits(10), mciDigits(15)
		first := e.postFB(t, comment, from, "A1", mciAt(3*time.Second), nil)
		raw := mciFBBody(e.pageAsset, e.postID, comment, from, "SENTINEL-NAME-"+t04Tag(), "A1", mciAt(3*time.Second), nil)
		second := mcPostApp(t, m2, e.pageAsset, app2, raw)
		mcAwait(t, m2, second)
		row := e.fbIntake(t, first)
		if count("page", e.pageAsset, comment) != 1 || row.AppID != miApp || row.InboxEvent != first.ev.id {
			t.Fatalf("two apps split the intake or replaced the first delivering app: %+v", row)
		}
		before := e.countEvents(t)
		e.apply(t)
		if e.countEvents(t) != before+1 {
			t.Fatal("two apps produced two claims events")
		}
	})

	t.Run("20 concurrent consumer runs of one comment", func(t *testing.T) {
		comment, from := mciDigits(15)+"_"+mciDigits(10), mciDigits(15)
		var sent []mciSent
		for i := 0; i < 20; i++ {
			// Same comment id, 20 distinct payload hashes (only the display name differs).
			s := mciSent{comment: comment, from: from, text: "A1+2", name: fmt.Sprintf("SENTINEL-NAME-%d-%s", i, t04Tag())}
			raw := mciFBBody(e.pageAsset, e.postID, comment, from, s.name, s.text, mciAt(3*time.Second), nil)
			s.raw = string(raw)
			s.ev = mcPost(t, e.page, e.pageAsset, raw)
			sent = append(sent, s)
		}
		for _, s := range sent {
			mcAwait(t, e.page, s.ev)
		}
		if count("page", e.pageAsset, comment) != 1 {
			t.Fatalf("20 concurrent consumer runs staged %d intake rows", count("page", e.pageAsset, comment))
		}
		for _, s := range sent {
			if miCount(t, f.owner, `SELECT count(*) FROM social.comment_events WHERE event_id=$1`, s.ev.id) != 1 {
				t.Fatal("every run must still commit its own social fact")
			}
		}
		// Everything is staged: stop the consumer (and close its pools) so the 20 pollers fit the harness's
		// max_connections=60 next to the fixture pools.
		e.stopConsumer()
		e.stopConsumer = mciNoop
		before := e.countEvents(t)
		// 20 pollers race for the one row: exactly one applies it.
		if leased := e.applyConcurrently(t, 20); leased != 1 || e.countEvents(t) != before+1 {
			t.Fatalf("20 concurrent applies leased %d rows and wrote %d events", leased, e.countEvents(t)-before)
		}
	})
}

// TestMetaClaimsMCI05ImmutableFactsAndDuplicates: re-applying an intake whose immutable facts are
// unchanged is a Duplicate that writes nothing; a changed fact is a 409 conflict (claims §4.3
// step 2); an unresolved keyword stays UNKNOWN_KEYWORD after the offer is created later (S03).
func TestMetaClaimsMCI05ImmutableFactsAndDuplicates(t *testing.T) {
	e := mciSetup(t, mciOpts{})
	f := e.h.f
	ctx := context.Background()
	requeue := func(id string, extra string) {
		// applied_event_id is cleared with the state: CHECK (state='APPLIED')=(applied_event_id IS NOT NULL) (0064);
		// the replay must find the stored event through claims.events, not through this column.
		mustExec(t, f.owner, `UPDATE claims.meta_intake SET state='PENDING',applied_event_id=NULL,lease_xid=NULL,not_before=clock_timestamp()-interval '1 second'`+extra+` WHERE id=$1`, id)
	}

	s := e.postFB(t, "", "", "A1+2", mciAt(3*time.Second), nil)
	e.apply(t)
	row := e.fbIntake(t, s)
	events := e.countEvents(t)

	requeue(row.ID, "")
	tx, id := e.leaseTx(t, pgx.ReadCommitted, true)
	res, err := claims.IngestMetaIntake(ctx, tx, id)
	_ = tx.Rollback(ctx)
	if err != nil || !res.Duplicate || res.BundleCreated || res.EventID != row.AppliedEvent || res.Outcome != claims.OutcomeAccepted {
		t.Fatalf("unchanged facts must replay as Duplicate of the stored event: %+v %v", res, err)
	}
	requeue(row.ID, ",quantity=quantity+1")
	tx, id = e.leaseTx(t, pgx.ReadCommitted, true)
	_, err = claims.IngestMetaIntake(ctx, tx, id)
	_ = tx.Rollback(ctx)
	if !mciConflict(err) {
		t.Fatalf("changed immutable fact (quantity) must be a 409 conflict, got %v", err)
	}
	if e.countEvents(t) != events {
		t.Fatal("a duplicate or conflicting replay wrote a claims event")
	}
	mustExec(t, f.owner, `DELETE FROM claims.meta_intake WHERE id=$1`, row.ID)

	// S03: unknown keyword -> create the offer -> re-apply = duplicate UNKNOWN_KEYWORD, no new event.
	kw := "Q" + strings.ToUpper(t04Tag()[:8])
	u := e.postFB(t, "", "", kw, mciAt(3*time.Second), nil)
	e.apply(t)
	urow := e.fbIntake(t, u)
	first := e.eventOf(t, urow)
	if first.reason != "UNKNOWN_KEYWORD" {
		t.Fatalf("expected UNKNOWN_KEYWORD, got %+v", first)
	}
	// Another SKU: offer A1 already holds e.sku and one SKU has at most one active offer (claims KC04).
	e.h.offer(t, e.session, kw, e.h.stock.skus[1].ID, 5)
	requeue(urow.ID, "")
	tx, id = e.leaseTx(t, pgx.ReadCommitted, true)
	res, err = claims.IngestMetaIntake(ctx, tx, id)
	_ = tx.Rollback(ctx)
	if err != nil || !res.Duplicate || res.Outcome != claims.OutcomeRejected || res.Reason != claims.ReasonUnknownKeyword || res.EventID != urow.AppliedEvent {
		t.Fatalf("S03 re-apply after the offer exists must stay UNKNOWN_KEYWORD (duplicate): %+v %v", res, err)
	}
}

// TestMetaClaimsMCI05StagingCap: at intake_count = 50 000 a comment stages nothing and only
// intake_capped moves (§4.2); the social fact still commits.
func TestMetaClaimsMCI05StagingCap(t *testing.T) {
	e := mciSetup(t, mciOpts{})
	f := e.h.f
	mustExec(t, f.owner, `UPDATE live.claim_sources SET intake_count=50000 WHERE id=$1`, e.srcFB)
	var count, capped int64
	read := func() {
		if err := f.owner.QueryRow(context.Background(), `SELECT intake_count,intake_capped FROM live.claim_sources WHERE id=$1`, e.srcFB).Scan(&count, &capped); err != nil {
			t.Fatal(err)
		}
	}
	read()
	before := capped
	s := e.postFB(t, "", "", "A1", mciAt(3*time.Second), nil)
	e.noIntake(t, "page", e.pageAsset, s.comment, "staging cap reached")
	read()
	if count != 50000 || capped != before+1 {
		t.Fatalf("cap accounting: intake_count=%d intake_capped=%d (was %d)", count, capped, before)
	}
	if miCount(t, f.owner, `SELECT count(*) FROM social.comment_events WHERE event_id=$1`, s.ev.id) != 1 {
		t.Fatal("the social fact must still commit when the staging cap is hit")
	}
	// Below the cap the counter moves by one per staged comment (NO_MATCH counts too).
	mustExec(t, f.owner, `UPDATE live.claim_sources SET intake_count=10 WHERE id=$1`, e.srcFB)
	e.postFB(t, "", "", "no keyword here", mciAt(3*time.Second), nil)
	read()
	if count != 11 {
		t.Fatalf("a NO_MATCH comment must count toward the staging bound: intake_count=%d want 11", count)
	}
}

// ---------------------------------------------------------------------------------------
// MCI06: window intervals, grace, delivery time, rate bounds
// ---------------------------------------------------------------------------------------

func (e *mciEnv) setInterval(t *testing.T, generation int64, opened, closed string) {
	t.Helper()
	f := e.h.f
	tag, err := f.owner.Exec(context.Background(), `UPDATE live.claim_window_intervals SET opened_at=`+opened+`,closed_at=`+closed+`
		WHERE tenant_id=$1 AND store_id=$2 AND session_id=$3 AND generation=$4`, f.tenantA, f.storeA1, e.session, generation)
	if err != nil || tag.RowsAffected() != 1 {
		t.Fatalf("interval g%d not found/updated: %v", generation, err)
	}
}

// backdateOffer moves the offer activation far into the past: a comment whose occurred_at precedes
// activated_at is OFFER_INACTIVE (claims §2.3), which would mask the window/rate decisions under test
// when the test also backdates comment times.
func (e *mciEnv) backdateOffer(t *testing.T) {
	t.Helper()
	mustExec(t, e.h.f.owner, `UPDATE live.offers SET activated_at=clock_timestamp()-interval '12 hours' WHERE session_id=$1`, e.session)
}

func (e *mciEnv) stageAs(t *testing.T, text string) (mciSent, mciIntake) {
	t.Helper()
	s := e.postFB(t, "", "", text, mciAt(3*time.Second), nil)
	return s, e.fbIntake(t, s)
}

// TestMetaClaimsMCI06IntervalsGraceAndGenerations is the §5.3 step 2 decision table on the
// interval history: inside an interval (with the 60 s grace after its close) -> accepted with that
// interval's generation; before open, in the gap between generations, beyond grace, or in the
// future beyond +120 s -> DROPPED(window_closed) with no claims event. Times are controlled by
// backdating the staged row and the trigger-maintained intervals with owner SQL.
func TestMetaClaimsMCI06IntervalsGraceAndGenerations(t *testing.T) {
	e := mciSetup(t, mciOpts{})
	f := e.h.f
	// generation 1 -> closed, generation 2 -> open. Close/reopen must not merge generations.
	e.h.closeWindow(t, e.session)
	e.h.open(t, e.session, claims.MatchExact)
	if n := miCount(t, f.owner, `SELECT count(*) FROM live.claim_window_intervals WHERE session_id=$1`, e.session); n != 2 {
		t.Fatalf("close+reopen must leave two intervals (one per generation), got %d", n)
	}
	e.setInterval(t, 1, `clock_timestamp()-interval '2 hours'`, `clock_timestamp()-interval '1 hour'`)
	e.setInterval(t, 2, `clock_timestamp()-interval '30 minutes'`, `NULL`)
	e.backdateOffer(t)

	type want struct {
		name, occurred, received string
		state                    string
		generation               int64
	}
	cases := []want{
		{"inside g1, received within 60 s grace of its close", `clock_timestamp()-interval '90 minutes'`, `clock_timestamp()-interval '59 minutes 30 seconds'`, "APPLIED", 1},
		{"inside g1, received beyond the 60 s grace", `clock_timestamp()-interval '90 minutes'`, `clock_timestamp()-interval '58 minutes'`, "DROPPED", 0},
		{"before g1 opened", `clock_timestamp()-interval '125 minutes'`, `clock_timestamp()-interval '124 minutes'`, "DROPPED", 0},
		{"in the gap between the generations", `clock_timestamp()-interval '45 minutes'`, `clock_timestamp()-interval '44 minutes'`, "DROPPED", 0},
		{"inside the open g2", `clock_timestamp()-interval '10 minutes'`, `clock_timestamp()-interval '9 minutes'`, "APPLIED", 2},
		{"in the future beyond +120 s", `clock_timestamp()+interval '10 minutes'`, `clock_timestamp()`, "DROPPED", 0},
	}
	sent := make([]mciSent, len(cases))
	for i, c := range cases {
		s, row := e.stageAs(t, "A1")
		mciBackdate(t, f, row.ID, c.occurred, c.received)
		sent[i] = s
	}
	e.apply(t)
	accepted := int64(0)
	for i, c := range cases {
		row := e.fbIntake(t, sent[i])
		if row.State != c.state {
			t.Fatalf("%s: state=%s want %s (%+v)", c.name, row.State, c.state, row)
		}
		if c.state == "DROPPED" {
			if row.DropReason != "window_closed" || row.AppliedEvent != "" {
				t.Fatalf("%s: drop_reason=%q applied=%q", c.name, row.DropReason, row.AppliedEvent)
			}
			if miCount(t, f.owner, `SELECT count(*) FROM claims.events WHERE source_event_id=$1`, row.InboxEvent) != 0 {
				t.Fatalf("%s: WINDOW_CLOSED must not be persisted as a claims event", c.name)
			}
			continue
		}
		accepted++
		if ev := e.eventOf(t, row); ev.outcome != "ACCEPTED" || ev.generation != c.generation {
			t.Fatalf("%s: event %+v want ACCEPTED generation %d", c.name, ev, c.generation)
		}
	}
	if e.countEvents(t) != accepted {
		t.Fatalf("claims events=%d want %d", e.countEvents(t), accepted)
	}
}

// TestMetaClaimsMCI06CloseWhileApplyWaits: a window close that commits while an apply waits on the
// window lock is observed: the apply then sees the closed interval and drops (no acceptance after close).
func TestMetaClaimsMCI06CloseWhileApplyWaits(t *testing.T) {
	e := mciSetup(t, mciOpts{})
	f := e.h.f
	ctx := context.Background()
	s, row := e.stageAs(t, "A1")
	// The comment happened 2 s in the future relative to the close below, so it can only be
	// accepted if the apply ignored the close.
	mciBackdate(t, f, row.ID, `clock_timestamp()+interval '2 seconds'`, `clock_timestamp()`)

	closer, err := f.owner.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer closer.Rollback(ctx)
	if _, err := closer.Exec(ctx, `SELECT 1 FROM live.claim_windows WHERE tenant_id=$1 AND store_id=$2 AND session_id=$3 FOR UPDATE`, f.tenantA, f.storeA1, e.session); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := e.poller.ApplyOne(ctx)
		done <- err
	}()
	user := e.intakePool.Config().ConnConfig.User
	deadline := time.Now().Add(10 * time.Second)
	for miCount(t, f.owner, `SELECT count(*) FROM pg_stat_activity WHERE usename=$1 AND wait_event_type='Lock' AND state='active'`, user) < 1 {
		if time.Now().After(deadline) {
			t.Fatal("apply never blocked on the window lock")
		}
		time.Sleep(10 * time.Millisecond) // observes a PG lock wait
	}
	if _, err := closer.Exec(ctx, `UPDATE live.claim_windows SET state='CLOSED',closed_at=clock_timestamp(),version=version+1 WHERE tenant_id=$1 AND store_id=$2 AND session_id=$3`, f.tenantA, f.storeA1, e.session); err != nil {
		t.Fatalf("closing the window: %v", err)
	}
	if err := closer.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := waitError(t, done); err != nil {
		t.Fatalf("apply after the close: %v", err)
	}
	got := e.fbIntake(t, s)
	if got.State != "DROPPED" || got.DropReason != "window_closed" || e.countEvents(t) != 0 {
		t.Fatalf("a comment after the close was accepted: %+v events=%d", got, e.countEvents(t))
	}
}

// TestMetaClaimsMCI06InstagramUsesDeliveryTime (U7): an Instagram comment carries no creation
// time, so occurred_at is the entry.time of the delivery, even if the value carries another time.
func TestMetaClaimsMCI06InstagramUsesDeliveryTime(t *testing.T) {
	e := mciSetup(t, mciOpts{})
	delivered := time.Now().Add(3 * time.Second).Truncate(time.Second)
	s := e.postIG(t, "comments", "", "", "A1", &delivered, map[string]any{"created_time": delivered.Add(-24 * time.Hour).Unix(), "timestamp": delivered.Add(-48 * time.Hour).Unix()})
	row := e.igIntake(t, s)
	if !row.Occurred.Equal(delivered) {
		t.Fatalf("IG occurred_at=%v want the delivery entry.time %v", row.Occurred, delivered)
	}
	// No entry.time at all: nothing to decide a window on, so nothing is staged.
	n := e.postIG(t, "comments", "", "", "A1", nil, nil)
	e.noIntake(t, "instagram", e.igAsset, n.comment, "Instagram comment without any time")
}

// TestMetaClaimsMCI06RateBounds: per (session, actor) at most 10 ACCEPTED commands per rolling 60 s
// of event time, per session at most 5000 platform bundles; both are enforced exactly under
// concurrent applies (advisory locks of §4.2), the surplus is RATE_LIMITED with no bundle write; a
// backlog replay of legitimately spaced comments is not limited.
func TestMetaClaimsMCI06RateBounds(t *testing.T) {
	e := mciSetup(t, mciOpts{})
	f := e.h.f
	reasons := func() map[string]int64 {
		rows, err := f.owner.Query(context.Background(), `SELECT coalesce(reason,'ACCEPTED'),count(*) FROM claims.events WHERE session_id=$1 AND source_kind='meta' GROUP BY 1`, e.session)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		out := map[string]int64{}
		for rows.Next() {
			var r string
			var n int64
			if err := rows.Scan(&r, &n); err != nil {
				t.Fatal(err)
			}
			out[r] = n
		}
		return out
	}

	t.Run("10 per actor per minute under 12 concurrent applies", func(t *testing.T) {
		from := mciDigits(15)
		at := mciAt(3 * time.Second)
		for i := 0; i < 12; i++ {
			e.postFB(t, "", from, "A1", at, nil)
		}
		if leased := e.applyConcurrently(t, 12); leased != 12 {
			t.Fatalf("leased %d rows want 12", leased)
		}
		got := reasons()
		if got["ACCEPTED"] != 10 || got["RATE_LIMITED"] != 2 {
			t.Fatalf("actor bound: %v want 10 ACCEPTED and 2 RATE_LIMITED", got)
		}
		if n := miCount(t, f.owner, `SELECT count(*) FROM claims.bundles WHERE session_id=$1 AND actor_key=$2`, e.session, mciActorKey(e.actorRaw, "page", e.pageAsset, from)); n != 1 {
			t.Fatalf("one actor must own exactly one bundle, got %d", n)
		}
		// §4.4 clause 3 adds the claims §3.1 row `RATE_LIMITED | MATCH | set | N, flag | NULL`: offer and quantity
		// are recorded, bundle_id/line_version stay NULL (no bundle or line write).
		if n := miCount(t, f.owner, `SELECT count(*) FROM claims.events WHERE session_id=$1 AND reason='RATE_LIMITED' AND (bundle_id IS NOT NULL OR line_version IS NOT NULL
			OR offer_id IS NULL OR quantity IS NULL OR explicit_quantity IS NULL)`, e.session); n != 0 {
			t.Fatal("a RATE_LIMITED event is not the §3.1 row (offer and quantity set, bundle/line NULL)")
		}
	})

	t.Run("spaced backlog replay is not limited", func(t *testing.T) {
		e.setInterval(t, 1, `clock_timestamp()-interval '6 hours'`, `NULL`)
		e.backdateOffer(t)
		from := mciDigits(15)
		var rows []mciIntake
		for i := 0; i < 12; i++ {
			_, r := e.stageAs(t, "A1")
			_ = from
			rows = append(rows, r)
		}
		// One actor: rewrite the staged actor keys, then space the event times by 70 s (> 60 s).
		key := mciActorKey(e.actorRaw, "page", e.pageAsset, from)
		for i, r := range rows {
			mustExec(t, f.owner, `UPDATE claims.meta_intake SET actor_key=$2,occurred_at=clock_timestamp()-make_interval(secs=>$3),received_at=clock_timestamp() WHERE id=$1`, r.ID, key, float64(70*(i+1)))
		}
		before := reasons()
		e.apply(t)
		after := reasons()
		if after["ACCEPTED"]-before["ACCEPTED"] != 12 || after["RATE_LIMITED"] != before["RATE_LIMITED"] {
			t.Fatalf("backlog replay was rate limited: before=%v after=%v", before, after)
		}
	})

	t.Run("5000 platform bundles per session under 4 concurrent applies", func(t *testing.T) {
		mustExec(t, f.owner, `INSERT INTO claims.bundles(tenant_id,store_id,session_id,platform,actor_key)
			SELECT $1,$2,$3,'facebook',lpad(to_hex(g+1000000),64,'e') FROM generate_series(1,GREATEST(0,4998-(SELECT count(*) FROM claims.bundles WHERE session_id=$3 AND platform<>'manual'))::int) g`, f.tenantA, f.storeA1, e.session)
		if n := e.bundleCount(t); n != 4998 {
			t.Fatalf("test bug: %d bundles", n)
		}
		before := reasons()
		for i := 0; i < 4; i++ {
			e.postFB(t, "", "", "A1", mciAt(3*time.Second), nil)
		}
		e.applyConcurrently(t, 4)
		after := reasons()
		if after["ACCEPTED"]-before["ACCEPTED"] != 2 || after["RATE_LIMITED"]-before["RATE_LIMITED"] != 2 || e.bundleCount(t) != 5000 {
			t.Fatalf("session cap: before=%v after=%v bundles=%d want +2 ACCEPTED, +2 RATE_LIMITED, 5000 bundles", before, after, e.bundleCount(t))
		}
	})
}

// ---------------------------------------------------------------------------------------
// MCI09: lock-order workload, zero 40P01
// ---------------------------------------------------------------------------------------

func mciDeadlocks(t *testing.T, f *testFixture) int64 {
	t.Helper()
	return miCount(t, f.owner, `SELECT deadlocks FROM pg_stat_database WHERE datname=current_database()`)
}

// TestMetaClaimsMCI09LockOrderWorkload runs the §9 participants against each other on one
// session: consumer staging, window close/open, offer deactivate/activate, issue_link (rotation),
// buyer redeem, intake apply and (with private replies on) the operation plan. The gate is the
// database's own deadlock counter and 40P01 in any returned error. One deterministic
// interleaving first proves the workload really waits on locks (pg_stat_activity), no sleeps.
func TestMetaClaimsMCI09LockOrderWorkload(t *testing.T) {
	e := mciSetup(t, mciOpts{private: true})
	f, h := e.h.f, e.h
	ctx := context.Background()
	deadlocksBefore := mciDeadlocks(t, f)
	serverLog := lcServerLog(t, f) // PostgreSQL logs "deadlock detected" at once; the stats counter can lag

	// Seed bundles that carry a system link, so issue_link / redeem have targets.
	for i := 0; i < 4; i++ {
		e.postFB(t, "", "", "A1", mciAt(3*time.Second), nil)
	}
	e.apply(t)

	// Deterministic interleaving: an apply waits behind a held window lock together with a
	// window-state command and an offer command; all three are released in one commit.
	hold := h.holdMerchant(t, h.token, f.storeA1, func(tx pgx.Tx, s platform.Scope) error {
		_, err := tx.Exec(ctx, `SELECT 1 FROM live.claim_windows WHERE tenant_id=$1 AND store_id=$2 AND session_id=$3 FOR UPDATE`, s.TenantID, s.StoreID, e.session)
		return err
	})
	e.postFB(t, "", "", "A1", mciAt(3*time.Second), nil)
	applyDone := make(chan error, 1)
	go func() { _, err := e.poller.ApplyOne(ctx); applyDone <- err }()
	user := e.intakePool.Config().ConnConfig.User
	deadline := time.Now().Add(10 * time.Second)
	for miCount(t, f.owner, `SELECT count(*) FROM pg_stat_activity WHERE usename=$1 AND wait_event_type='Lock' AND state='active'`, user) < 1 {
		if time.Now().After(deadline) {
			t.Fatal("apply never blocked behind the held window lock")
		}
		time.Sleep(10 * time.Millisecond) // observes a PG lock wait
	}
	if err := hold.finish(t); err != nil {
		t.Fatal(err)
	}
	if err := waitError(t, applyDone); err != nil {
		t.Fatalf("apply behind a released lock: %v", err)
	}

	// Free-running workload.
	stop := make(chan struct{})
	var wg sync.WaitGroup
	errs := make(chan error, 256)
	report := func(who string, err error) {
		if err == nil {
			return
		}
		if mciSQLIs(err, "40P01") {
			errs <- fmt.Errorf("%s: deadlock 40P01: %w", who, err)
			return
		}
		// Business conflicts (version races, closed window, inactive offer) are expected noise.
		if !errors.Is(err, command.ErrConflict) && !errors.Is(err, command.ErrInvalid) && !errors.Is(err, command.ErrNotFound) && !mciSQLIs(err, "PT409", "PT404", "40001", "55P03") {
			errs <- fmt.Errorf("%s: unexpected error: %w", who, err)
		}
	}
	run := func(who string, iterations int, fn func(i int) error) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < iterations; i++ {
				select {
				case <-stop:
					return
				default:
				}
				report(who, fn(i))
			}
		}()
	}
	run("consumer-staging", 25, func(i int) error {
		raw := mciFBBody(e.pageAsset, e.postID, mciDigits(15)+"_"+mciDigits(10), mciDigits(15), "SENTINEL-NAME-"+t04Tag(), "A1", mciAt(3*time.Second), nil)
		batch, err := e.page.verifier.Verify(raw, miSignature(raw))
		if err != nil || len(batch.Events) != 1 {
			return err
		}
		status, body := miPost(t, e.page, raw)
		if status != 200 {
			return fmt.Errorf("webhook status %d %s", status, body)
		}
		return nil
	})
	run("window", 12, func(i int) error {
		board, err := h.getBoard(h.token, f.storeA1, e.session)
		if err != nil {
			return err
		}
		w := board.Window
		next := claims.WindowClosed
		if w.State == claims.WindowClosed {
			next = claims.WindowOpen
		}
		_, err = h.setWindow(h.token, f.storeA1, e.session, claims.WindowInput{ExpectedVersion: w.Version, State: next, MatchMode: w.MatchMode})
		return err
	})
	run("offer", 15, func(i int) error {
		board, err := h.getBoard(h.token, f.storeA1, e.session)
		if err != nil {
			return err
		}
		for _, o := range board.Offers {
			if o.ID == e.offer.ID {
				_, err := h.updateOffer(h.token, f.storeA1, t04Key("mci09-offer"), e.session, o.ID, claims.OfferUpdate{ExpectedVersion: o.Version, MaxQuantityPerClaim: o.MaxQuantityPerClaim, Active: !o.Active})
				return err
			}
		}
		return nil
	})
	run("issue-link", 10, func(i int) error {
		page, err := h.bundles(h.token, f.storeA1, e.session, pageAll)
		if err != nil {
			return err
		}
		for _, b := range page.Items {
			if b.Link.State == "ACTIVE" {
				_, err = h.issue(h.token, f.storeA1, t04Key("mci09-link"), e.session, b.ID, claims.LinkInput{ExpectedGeneration: b.Link.Generation})
				return err
			}
		}
		return nil
	})
	run("redeem", 6, func(i int) error {
		page, err := h.bundles(h.token, f.storeA1, e.session, pageAll)
		if err != nil {
			return err
		}
		for _, b := range page.Items {
			if b.Link.State != "ACTIVE" || b.Link.Generation != 1 || b.Bound {
				continue
			}
			var op string
			if err := f.owner.QueryRow(ctx, `SELECT o.id::text FROM integration.operations o WHERE o.request->>'bundle_id'=$1 AND o.action='meta.private_reply'`, b.ID).Scan(&op); err != nil {
				continue
			}
			tok, err := claims.SystemLinkToken(e.link, f.tenantA, f.storeA1, b.ID, op)
			if err != nil {
				return err
			}
			c, err := buyerIssue(t, h)
			if err != nil {
				return err
			}
			_, err = h.redeem(c, t04Key("mci09-redeem"), tok, b.Version)
			return err
		}
		return nil
	})
	run("intake-apply", 30, func(i int) error {
		_, err := e.poller.ApplyOne(ctx)
		return err
	})
	wg.Wait()
	close(stop)
	close(errs)
	for err := range errs {
		t.Error(err)
	}
	// Drain what is left so the last transactions also ran under contention-free conditions.
	e.applyConcurrently(t, 3)
	if log := serverLog(); strings.Contains(log, "deadlock detected") {
		t.Fatalf("the PostgreSQL server log reports a detected deadlock during the workload")
	}
	if d := mciDeadlocks(t, f) - deadlocksBefore; d != 0 {
		t.Fatalf("the database detected %d deadlock(s) during the workload (pg_stat_database.deadlocks)", d)
	}
}

func buyerIssue(t *testing.T, h *lcHarness) (buyer.Capability, error) {
	return h.service.IssueForTrustedStore(context.Background(), h.f.storeA1)
}
