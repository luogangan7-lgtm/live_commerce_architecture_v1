package foundation_test

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"
	"github.com/riverqueue/river/rivermigrate"
	"livecommerce/internal/integrations/meta"
)

type mcEvent struct {
	id, key, hash, app, object, asset, route string
	epoch, job                               int64
	plain                                    []byte
}

// Start at the actual checked migration ledger through 0028. The existing
// worker fixture installs latest immediately, so it cannot model this upgrade.
func mcPre0029Fixture(t *testing.T) *testFixture {
	t.Helper()
	fixture(t) // the same explicit real-PG permission gate as the other fixtures
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	name := "lc-meta-upgrade-" + t04Tag()
	password := hex.EncodeToString(randomBytes(24))
	_, err := exec.CommandContext(ctx, "docker", "run", "-d", "--pull=never", "--name", name,
		"--label", "livecommerce.fixture="+name, "--memory=512m", "--cpus=1", "--pids-limit=128",
		"--tmpfs", "/var/lib/postgresql:rw,size=268435456", "-e", "POSTGRES_PASSWORD="+password,
		"-e", "POSTGRES_DB=lc_foundation_test", "-p", "127.0.0.1::5432",
		"postgres@sha256:4ef4dbc939d61acea57712655ddb4b4ab27419c913f94cca0cd57cb3ea3c2280",
		"-c", "shared_buffers=32MB", "-c", "max_connections=60").CombinedOutput()
	if err != nil {
		t.Fatalf("start labelled old-schema PG: %v", err)
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
			t.Errorf("remove test PG %s: %v: %s", name, err, out)
		}
	})
	portOutput, err := exec.CommandContext(ctx, "docker", "port", name, "5432/tcp").Output()
	if err != nil || !strings.HasPrefix(strings.TrimSpace(string(portOutput)), "127.0.0.1:") {
		t.Fatal("old-schema PG did not bind loopback")
	}
	port := strings.TrimPrefix(strings.TrimSpace(string(portOutput)), "127.0.0.1:")
	if _, err := strconv.Atoi(port); err != nil {
		t.Fatal("old-schema PG returned invalid port")
	}
	u := &url.URL{Scheme: "postgres", User: url.UserPassword("postgres", password), Host: "127.0.0.1:" + port, Path: "/lc_foundation_test", RawQuery: "sslmode=disable"}
	owner, err := pgxpool.New(ctx, u.String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(owner.Close)
	for i := 0; i < 50; i++ {
		probe, stop := context.WithTimeout(ctx, 500*time.Millisecond)
		err = owner.Ping(probe)
		stop()
		if err == nil {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	if err != nil {
		t.Fatalf("old-schema PG did not become ready: %v", err)
	}
	// Mirror Apply's business, upstream River, then post-River phases, stopping
	// before 0029. Keep the original bytes and checksums, not a hand-written
	// approximation of the 0028 schema.
	oldVersions, err := filepath.Glob("../../migrations/[0-9][0-9][0-9][0-9]_*.sql")
	if err != nil || len(oldVersions) < 30 || filepath.Base(oldVersions[27]) != "0028_meta_inbox.sql" || filepath.Base(oldVersions[28]) != "0029_meta_social_consumer.sql" || filepath.Base(oldVersions[29]) != "0030_meta_runtime.sql" {
		t.Fatalf("unexpected numbered migrations: count=%d err=%v", len(oldVersions), err)
	}
	tx, err := owner.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `CREATE TABLE public.lc_schema_migrations(version text PRIMARY KEY,checksum text NOT NULL,applied_at timestamptz NOT NULL DEFAULT now())`); err != nil {
		_ = tx.Rollback(ctx)
		t.Fatal(err)
	}
	for _, path := range oldVersions[:28] {
		body, err := os.ReadFile(path)
		if err != nil {
			_ = tx.Rollback(ctx)
			t.Fatal(err)
		}
		if _, err := tx.Exec(ctx, string(body)); err != nil {
			_ = tx.Rollback(ctx)
			t.Fatalf("historical migration %s: %v", filepath.Base(path), err)
		}
		checksum := fmt.Sprintf("%x", sha256.Sum256(body))
		if _, err := tx.Exec(ctx, `INSERT INTO public.lc_schema_migrations(version,checksum) VALUES($1,$2)`, filepath.Base(path), checksum); err != nil {
			_ = tx.Rollback(ctx)
			t.Fatal(err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	upstream, err := rivermigrate.New(riverpgxv5.New(owner), &rivermigrate.Config{Schema: "river", Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := upstream.Migrate(ctx, rivermigrate.DirectionUp, nil); err != nil {
		t.Fatal(err)
	}
	// Apply installs these River privileges between upstream and post-River SQL.
	mustExec(t, owner, `GRANT SELECT,INSERT,UPDATE(kind) ON river.river_job TO commerce_runtime;
	 GRANT USAGE ON SEQUENCE river.river_job_id_seq TO commerce_runtime;
	 GRANT SELECT,INSERT,UPDATE(kind) ON river.river_job TO commerce_checkout_runtime;
	 GRANT USAGE ON SEQUENCE river.river_job_id_seq TO commerce_checkout_runtime;
	 GRANT SELECT ON river.river_job TO commerce_checkout_writer,commerce_integration_writer;
	 GRANT USAGE ON SCHEMA river TO commerce_worker;
	 GRANT SELECT,INSERT,UPDATE,DELETE ON ALL TABLES IN SCHEMA river TO commerce_worker;
	 REVOKE ALL ON river.river_migration FROM commerce_worker;
	 GRANT USAGE,SELECT ON ALL SEQUENCES IN SCHEMA river TO commerce_worker`)
	postVersions, err := filepath.Glob("../../migrations/post_river/[0-9][0-9][0-9][0-9]_*.sql")
	if err != nil || len(postVersions) < 3 || filepath.Base(postVersions[0]) != "0001_payment_queue.sql" || filepath.Base(postVersions[1]) != "0002_checkout_expiry_queue.sql" || filepath.Base(postVersions[2]) != "0003_meta_inbox_queue.sql" {
		t.Fatalf("unexpected post-River migrations: count=%d err=%v", len(postVersions), err)
	}
	postTx, err := owner.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range postVersions[:3] {
		body, err := os.ReadFile(path)
		if err != nil {
			_ = postTx.Rollback(ctx)
			t.Fatal(err)
		}
		version := "post_river/" + filepath.Base(path)
		if _, err := postTx.Exec(ctx, string(body)); err != nil {
			_ = postTx.Rollback(ctx)
			t.Fatalf("historical migration %s: %v", version, err)
		}
		checksum := fmt.Sprintf("%x", sha256.Sum256(body))
		if _, err := postTx.Exec(ctx, `INSERT INTO public.lc_schema_migrations(version,checksum) VALUES($1,$2)`, version, checksum); err != nil {
			_ = postTx.Rollback(ctx)
			t.Fatal(err)
		}
	}
	if err := postTx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	f := &testFixture{owner: owner, databaseURL: u.String(), tenantA: randomUUID(), tenantB: randomUUID(), storeA1: randomUUID(), storeA2: randomUUID(), storeB: randomUUID(), principalA: randomUUID(), tokens: map[string]string{"a": randomToken(), "a2": randomToken(), "b": randomToken(), "expired": randomToken(), "revoked": randomToken(), "buyer": randomToken(), "revoked_grant": randomToken()}}
	if err := f.seed(ctx); err != nil {
		t.Fatal(err)
	}
	return f
}

func mcPost(t *testing.T, m miTest, asset string, raw []byte) mcEvent {
	return mcPostApp(t, m, asset, miApp, raw)
}

func mcPostApp(t *testing.T, m miTest, asset, app string, raw []byte) mcEvent {
	t.Helper()
	batch, err := m.verifier.Verify(raw, miSignature(raw))
	if err != nil || len(batch.Events) != 1 || batch.Events[0].QuarantineReason != "" {
		t.Fatal("invalid one-event consumer fixture", err)
	}
	code, body := miPost(t, m, raw)
	miStatus(t, code, body, 200)
	e := mcEvent{key: batch.Events[0].Key, hash: batch.Events[0].PayloadHash, app: app, object: batch.Object, asset: asset, plain: batch.Events[0].Payload}
	if err := m.f.owner.QueryRow(context.Background(), `SELECT id::text,route_id::text,route_epoch,job_id FROM meta_inbox.events WHERE app_id=$1 AND object=$2 AND event_key=$3 AND is_primary`, app, e.object, e.key).Scan(&e.id, &e.route, &e.epoch, &e.job); err != nil {
		t.Fatal("missing admitted consumer event", err)
	}
	return e
}

func mcRunning(t *testing.T, m miTest, e mcEvent, attempt int) {
	t.Helper()
	mustExec(t, m.f.owner, `UPDATE `+mcJobTable(t, m.f)+` SET state='running',attempt=$2,attempted_at=clock_timestamp() WHERE id=$1`, e.job, attempt)
}

func mcJobTable(t *testing.T, f *testFixture) string {
	t.Helper()
	var modern bool
	if err := f.owner.QueryRow(context.Background(), `SELECT to_regclass('river_meta.river_job') IS NOT NULL`).Scan(&modern); err != nil {
		t.Fatal(err)
	}
	if modern {
		return "river_meta.river_job"
	}
	return "river.river_job"
}

func mcKeys(t *testing.T, m miTest) *meta.PayloadKeyring {
	t.Helper()
	keys, err := meta.NewPayloadKeyring(miKeyID, map[string][]byte{miKeyID: m.key})
	if err != nil {
		t.Fatal(err)
	}
	return keys
}

func mcSubject() string { sum := sha256.Sum256(randomBytes(32)); return hex.EncodeToString(sum[:]) }

func mcConsumer(t *testing.T, m miTest) *pgxpool.Pool {
	t.Helper()
	return miPool(t, m.f, "commerce_meta_consumer")
}

func mcAwait(t *testing.T, m miTest, e mcEvent) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		var state string
		var reason *string
		if err := m.f.owner.QueryRow(context.Background(), `SELECT j.state,e.terminal_reason FROM `+mcJobTable(t, m.f)+` j JOIN meta_inbox.events e ON e.job_id=j.id WHERE j.id=$1`, e.job).Scan(&state, &reason); err != nil {
			t.Fatal(err)
		}
		if state == "completed" && reason != nil && *reason == "processed" {
			return
		}
		if state == "cancelled" || state == "discarded" {
			t.Fatalf("real River job ended %s without processed fact", state)
		}
		time.Sleep(20 * time.Millisecond) // Polls completion only; no race conclusion depends on this delay.
	}
	t.Fatal("actual River consumer did not complete")
}

func TestMetaConsumerPopulated0028Upgrade(t *testing.T) {
	f := mcPre0029Fixture(t)
	ctx := context.Background()
	var oldLedger int
	var oldSocial bool
	if err := f.owner.QueryRow(ctx, `SELECT (SELECT count(*) FROM public.lc_schema_migrations),
	 to_regclass('social.messages') IS NOT NULL`).Scan(&oldLedger, &oldSocial); err != nil {
		t.Fatal(err)
	}
	if oldLedger != 31 || oldSocial {
		t.Fatalf("fixture is not 0028 plus three post-River migrations: ledger=%d social=%v", oldLedger, oldSocial)
	}
	m := mcOldSetup(t, f, "page")
	keys := mcKeys(t, m)
	asset := miAsset()
	binding := miBinding(t, m, asset, "facebook", f.tenantA, f.storeA1, f.principalA)
	route, _ := miRoute(t, m, asset, f.tenantA, f.storeA1, binding)
	e := mcOldPost(t, m, asset, miMessage(asset, "m."+randomUUID(), "legacy-0028-message"))
	const receipt = `SELECT jsonb_build_object(
	 'binding',(SELECT to_jsonb(b) FROM integration.bindings b WHERE b.id=$3),
	 'route',(SELECT to_jsonb(r) FROM meta_inbox.routes r WHERE r.id=$2),
	 'batch',(SELECT to_jsonb(b) FROM meta_inbox.batches b JOIN meta_inbox.batch_events x ON x.batch_id=b.id WHERE x.event_id=$1),
	 'batch_event',(SELECT to_jsonb(x) FROM meta_inbox.batch_events x WHERE x.event_id=$1),
	 'event',(SELECT to_jsonb(e) FROM meta_inbox.events e WHERE e.id=$1),
	 'raw',(SELECT to_jsonb(rb) FROM meta_private.raw_bodies rb JOIN meta_inbox.batch_events x ON x.batch_id=rb.batch_id WHERE x.event_id=$1),
	 'body',(SELECT to_jsonb(eb) FROM meta_private.event_bodies eb WHERE eb.event_id=$1),
	 'job',(SELECT to_jsonb(j) FROM river.river_job j WHERE j.id=$4))::text`
	var before, after string
	if err := f.owner.QueryRow(ctx, receipt, e.id, route, binding, e.job).Scan(&before); err != nil {
		t.Fatal(err)
	}
	for _, row := range []string{"binding", "route", "batch", "batch_event", "event", "raw", "body", "job"} {
		if strings.Contains(before, `"`+row+`": null`) {
			t.Fatalf("0028 fixture lacks populated %s row", row)
		}
	}
	mcApplyHistorical(t, f, "0029_meta_social_consumer.sql", "0030_meta_runtime.sql")
	mcApplyHistorical(t, f, "0029_meta_social_consumer.sql", "0030_meta_runtime.sql")
	if err := f.owner.QueryRow(ctx, receipt, e.id, route, binding, e.job).Scan(&after); err != nil {
		t.Fatal(err)
	}
	if after != before {
		t.Fatal("upgrade or repeat Apply changed the existing binding, route, receipt, body or River job")
	}
	var ledger int
	var hasSocial bool
	if err := f.owner.QueryRow(ctx, `SELECT (SELECT count(*) FROM public.lc_schema_migrations),
	 to_regclass('social.messages') IS NOT NULL`).Scan(&ledger, &hasSocial); err != nil {
		t.Fatal(err)
	}
	if ledger != oldLedger+2 || !hasSocial {
		t.Fatalf("0029/0030 ledger or social schema missing after repeat Apply: ledger=%d social=%v", ledger, hasSocial)
	}
	consumer := mcConsumer(t, m)
	w, err := meta.NewConsumerWorker(ctx, consumer, keys)
	if err != nil {
		t.Fatal(err)
	}
	workers := river.NewWorkers()
	river.AddWorker(workers, w)
	workerPool := miPool(t, f, "commerce_worker")
	client, err := river.NewClient(riverpgxv5.New(workerPool), &river.Config{Schema: "river", Workers: workers, Queues: map[string]river.QueueConfig{"meta_inbox": {MaxWorkers: 1}}, Logger: slog.New(slog.NewTextHandler(io.Discard, nil)), JobTimeout: 15 * time.Second, RescueStuckJobsAfter: 30 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	if err := client.Start(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		stop, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := client.StopAndCancel(stop); err != nil {
			t.Error("River stop", err)
		}
	})
	mcAwait(t, m, e)
	if miCount(t, f.owner, `SELECT count(*) FROM social.messages WHERE event_id=$1`, e.id) != 1 ||
		miCount(t, f.owner, `SELECT count(*) FROM meta_inbox.audit_events WHERE event_id=$1 AND action='processed'`, e.id) != 1 ||
		miCount(t, f.owner, `SELECT count(*) FROM meta_private.event_bodies WHERE event_id=$1`, e.id) != 1 {
		t.Fatal("upgraded legacy receipt was not consumed exactly once with source body retained")
	}
}

func TestMetaConsumerAuthority(t *testing.T) {
	m := miSetup(t)
	ctx := context.Background()
	keys := mcKeys(t, m)
	consumer := mcConsumer(t, m)
	if _, err := meta.NewConsumerWorker(ctx, consumer, keys); err != nil {
		t.Fatal("dedicated consumer refused", err)
	}
	for name, p := range map[string]*pgxpool.Pool{"owner": m.f.owner, "runtime": m.f.runtime, "ingress": m.ingress, "registrar": m.registrar, "curator": m.curator, "ordinary worker": miPool(t, m.f, "commerce_worker")} {
		t.Run(name, func(t *testing.T) {
			if w, err := meta.NewConsumerWorker(ctx, p, keys); err == nil || w != nil {
				t.Fatal("non-consumer authority accepted")
			}
		})
	}
	for _, role := range []string{"commerce_runtime", "commerce_meta_ingress"} {
		t.Run("mixed "+role, func(t *testing.T) {
			dsn := miRole(t, m.f, "commerce_meta_consumer")
			u, err := url.Parse(dsn)
			if err != nil {
				t.Fatal(err)
			}
			name := pgx.Identifier{u.User.Username()}.Sanitize()
			mustExec(t, m.f.owner, `GRANT `+pgx.Identifier{role}.Sanitize()+` TO `+name+` WITH INHERIT TRUE, SET FALSE`)
			t.Cleanup(func() { mustExec(t, m.f.owner, `REVOKE `+pgx.Identifier{role}.Sanitize()+` FROM `+name) })
			p, err := pgxpool.New(ctx, dsn)
			if err != nil {
				t.Fatal(err)
			}
			defer p.Close()
			if w, err := meta.NewConsumerWorker(ctx, p, keys); err == nil || w != nil {
				t.Fatal("mixed consumer accepted")
			}
			if _, err := p.Exec(ctx, `SELECT * FROM meta_inbox.load_social_event($1::uuid,1,1)`, randomUUID()); miSQLState(err) != "42501" {
				t.Fatalf("mixed SQLSTATE=%s", miSQLState(err))
			}
		})
	}
	// The validator must reject the reverse direction too: ingress acquiring
	// consumer authority cannot keep accepting signed deliveries.
	ingressDSN := miRole(t, m.f, "commerce_meta_ingress")
	ingressURL, err := url.Parse(ingressDSN)
	if err != nil {
		t.Fatal(err)
	}
	ingressName := pgx.Identifier{ingressURL.User.Username()}.Sanitize()
	mustExec(t, m.f.owner, `GRANT commerce_meta_consumer TO `+ingressName+` WITH INHERIT TRUE, SET FALSE`)
	t.Cleanup(func() { mustExec(t, m.f.owner, `REVOKE commerce_meta_consumer FROM `+ingressName) })
	mixedIngress, err := pgxpool.New(ctx, ingressDSN)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(mixedIngress.Close)
	if inbox, err := meta.NewInbox(ctx, mixedIngress, keys); err == nil || inbox != nil {
		t.Fatal("reverse mixed ingress accepted")
	}
	if _, err := mixedIngress.Exec(ctx, `SELECT * FROM meta_inbox.begin_batch($1,'page',$2,1)`, miApp, strings.Repeat("e", 64)); miSQLState(err) != "42501" {
		t.Fatalf("reverse mixed ingress SQLSTATE=%s", miSQLState(err))
	}
	for _, extra := range []string{"pg_read_all_data", "commerce_meta_consumer_settable"} {
		t.Run(extra, func(t *testing.T) {
			dsn := miRole(t, m.f, "commerce_meta_consumer")
			u, err := url.Parse(dsn)
			if err != nil {
				t.Fatal(err)
			}
			name := pgx.Identifier{u.User.Username()}.Sanitize()
			if extra == "pg_read_all_data" {
				mustExec(t, m.f.owner, `GRANT pg_read_all_data TO `+name)
			} else {
				mustExec(t, m.f.owner, `GRANT commerce_meta_consumer TO `+name+` WITH INHERIT TRUE, SET TRUE`)
			}
			p, err := pgxpool.New(ctx, dsn)
			if err != nil {
				t.Fatal(err)
			}
			defer p.Close()
			if w, err := meta.NewConsumerWorker(ctx, p, keys); err == nil || w != nil {
				t.Fatal("system/SET-capable consumer accepted")
			}
			if _, err := p.Exec(ctx, `SELECT * FROM meta_inbox.load_social_event($1::uuid,1,1)`, randomUUID()); miSQLState(err) != "42501" {
				t.Fatalf("system/SET SQLSTATE=%s", miSQLState(err))
			}
			if extra == "pg_read_all_data" {
				mustExec(t, m.f.owner, `REVOKE pg_read_all_data FROM `+name)
			}
		})
	}
	t.Run("inherited custom owner", func(t *testing.T) {
		dsn := miRole(t, m.f, "commerce_meta_consumer")
		u, err := url.Parse(dsn)
		if err != nil {
			t.Fatal(err)
		}
		login := pgx.Identifier{u.User.Username()}.Sanitize()
		ownerRole := "mc_owner_" + strings.ReplaceAll(randomUUID(), "-", "")
		table := "mc_owned_" + strings.ReplaceAll(randomUUID(), "-", "")
		quotedOwner := pgx.Identifier{ownerRole}.Sanitize()
		quotedTable := pgx.Identifier{"public", table}.Sanitize()
		mustExec(t, m.f.owner, `CREATE ROLE `+quotedOwner+` NOLOGIN NOSUPERUSER NOBYPASSRLS NOCREATEDB NOCREATEROLE NOREPLICATION`)
		mustExec(t, m.f.owner, `CREATE TABLE `+quotedTable+` (id integer)`)
		mustExec(t, m.f.owner, `ALTER TABLE `+quotedTable+` OWNER TO `+quotedOwner)
		t.Cleanup(func() {
			mustExec(t, m.f.owner, `REVOKE `+quotedOwner+` FROM `+login)
			mustExec(t, m.f.owner, `DROP TABLE `+quotedTable)
			mustExec(t, m.f.owner, `DROP ROLE `+quotedOwner)
		})
		mustExec(t, m.f.owner, `GRANT `+quotedOwner+` TO `+login+` WITH INHERIT TRUE, SET FALSE`)
		p, err := pgxpool.New(ctx, dsn)
		if err != nil {
			t.Fatal(err)
		}
		defer p.Close()
		if w, err := meta.NewConsumerWorker(ctx, p, keys); err == nil || w != nil {
			t.Error("custom owner inherited without SET accepted by Go")
		}
		if _, err := p.Exec(ctx, `SELECT * FROM meta_inbox.load_social_event($1::uuid,1,1)`, randomUUID()); miSQLState(err) != "42501" {
			t.Fatalf("custom owner SQLSTATE=%s", miSQLState(err))
		}
	})
	if _, err := consumer.Exec(ctx, `SET ROLE commerce_meta_consumer`); miSQLState(err) != "42501" {
		t.Fatalf("SET ROLE consumer SQLSTATE=%s", miSQLState(err))
	}
	for _, table := range []string{"social.conversations", "social.messages", "social.comment_events", "meta_private.event_bodies", "meta_private.raw_bodies", "meta_private.quarantine_bodies"} {
		var count int64
		err := consumer.QueryRow(ctx, `SELECT count(*) FROM `+table).Scan(&count)
		if miSQLState(err) != "42501" {
			t.Fatalf("consumer read %s SQLSTATE=%s", table, miSQLState(err))
		}
	}
	if _, err := consumer.Exec(ctx, `INSERT INTO social.conversations(tenant_id,store_id,app_id,object,asset_id,peer_key) VALUES($1,$2,$3,'page','1',$4)`, m.f.tenantA, m.f.storeA1, miApp, strings.Repeat("a", 64)); miSQLState(err) != "42501" {
		t.Fatalf("consumer direct insert SQLSTATE=%s", miSQLState(err))
	}
}

func TestMetaConsumerRiverPageAndInstagramFacts(t *testing.T) {
	m := miSetup(t)
	ctx := context.Background()
	consumer := mcConsumer(t, m)
	w, err := meta.NewConsumerWorker(ctx, consumer, mcKeys(t, m))
	if err != nil {
		t.Fatal(err)
	}
	workerPool := miPool(t, m.f, "commerce_meta_worker")
	workers := river.NewWorkers()
	river.AddWorker(workers, w)
	client, err := river.NewClient(riverpgxv5.New(workerPool), &river.Config{Schema: "river_meta", Workers: workers, Queues: map[string]river.QueueConfig{"meta_inbox": {MaxWorkers: 2}}, Logger: slog.New(slog.NewTextHandler(io.Discard, nil)), JobTimeout: 15 * time.Second, RescueStuckJobsAfter: 30 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	var events []mcEvent
	for _, object := range []string{"page", "instagram"} {
		local := m
		if object == "instagram" {
			local = miInstagram(t, m)
		}
		asset := miAsset()
		provider := "facebook"
		if object == "instagram" {
			provider = "instagram"
		}
		binding := miBinding(t, local, asset, provider, m.f.tenantA, m.f.storeA1, m.f.principalA)
		if object == "page" {
			miRoute(t, local, asset, m.f.tenantA, m.f.storeA1, binding)
		} else {
			miInstagramRoute(t, local, asset, binding)
		}
		var message, comment []byte
		if object == "page" {
			message = miMessage(asset, "m."+randomUUID(), "synthetic-page-消息")
			comment = []byte(fmt.Sprintf(`{"object":"page","entry":[{"id":%q,"changes":[{"field":"feed","value":{"item":"comment","verb":"add","comment_id":%q,"message":"synthetic-comment"}}]}]}`, asset, miAsset()))
		} else {
			message = []byte(fmt.Sprintf(`{"object":"instagram","entry":[{"id":%q,"messaging":[{"sender":{"id":"4"},"recipient":{"id":%q},"message":{"mid":%q,"text":"synthetic-IG"}}]}]}`, asset, asset, "m."+randomUUID()))
			comment = []byte(fmt.Sprintf(`{"object":"instagram","entry":[{"id":%q,"changes":[{"field":"comments","value":{"id":%q,"text":"synthetic-IG-comment"}}]}]}`, asset, miAsset()))
		}
		events = append(events, mcPost(t, local, asset, message), mcPost(t, local, asset, comment))
	}
	// Same provider peer under a second store and tenant must remain scoped.
	otherAsset := miAsset()
	otherBinding := miBinding(t, m, otherAsset, "facebook", m.f.tenantA, m.f.storeA2, m.f.principalA)
	miRoute(t, m, otherAsset, m.f.tenantA, m.f.storeA2, otherBinding)
	events = append(events, mcPost(t, m, otherAsset, miMessage(otherAsset, "m."+randomUUID(), "other-store")))
	var principalB string
	if err := m.f.owner.QueryRow(ctx, `SELECT principal_id::text FROM identity.memberships WHERE tenant_id=$1 LIMIT 1`, m.f.tenantB).Scan(&principalB); err != nil {
		t.Fatal(err)
	}
	thirdAsset := miAsset()
	thirdBinding := miBinding(t, m, thirdAsset, "facebook", m.f.tenantB, m.f.storeB, principalB)
	miRoute(t, m, thirdAsset, m.f.tenantB, m.f.storeB, thirdBinding)
	events = append(events, mcPost(t, m, thirdAsset, miMessage(thirdAsset, "m."+randomUUID(), "other-tenant")))
	// A distinct app has its own classifier and registration authority.
	altApp := "987654321012345"
	altAsset := miAsset()
	altBinding := miBinding(t, m, altAsset, "facebook", m.f.tenantA, m.f.storeA1, m.f.principalA)
	var altRoute string
	var altEpoch int64
	if err := m.registrar.QueryRow(ctx, `SELECT * FROM meta_inbox.activate_route($1,'page',$2,$3,$4,$5,1,$6,$7,0)`, altApp, altAsset, m.f.tenantA, m.f.storeA1, altBinding, strings.Repeat("f", 64), time.Now().Add(time.Hour)).Scan(&altRoute, &altEpoch); err != nil {
		t.Fatal(err)
	}
	alt := m
	alt.verifier, err = meta.NewVerifier(meta.Config{AppID: altApp, Object: "page", AppSecret: miSecret, VerifyToken: "meta-inbox-verify-token"})
	if err != nil {
		t.Fatal(err)
	}
	altInbox, err := meta.NewInbox(ctx, m.ingress, mcKeys(t, m))
	if err != nil {
		t.Fatal(err)
	}
	alt.handler, err = meta.NewInboxHandler(alt.verifier, altInbox)
	if err != nil {
		t.Fatal(err)
	}
	events = append(events, mcPostApp(t, alt, altAsset, altApp, miMessage(altAsset, "m."+randomUUID(), "other-app")))
	domainTables := []string{"identity.sessions", "checkout.orders", "checkout.payment_attempts", "integration.operations"}
	baseline := make(map[string]int64, len(domainTables))
	for _, table := range domainTables {
		baseline[table] = miCount(t, m.f.owner, `SELECT count(*) FROM `+table)
	}
	if err := client.Start(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		stop, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := client.StopAndCancel(stop); err != nil {
			t.Error("River stop", err)
		}
	})
	for _, e := range events {
		mcAwait(t, m, e)
	}
	if n := miCount(t, m.f.owner, `SELECT count(*) FROM social.messages WHERE event_id=ANY($1::uuid[])`, []string{events[0].id, events[2].id, events[4].id, events[5].id, events[6].id}); n != 5 {
		t.Fatalf("message facts=%d", n)
	}
	if n := miCount(t, m.f.owner, `SELECT count(*) FROM social.comment_events WHERE event_id=ANY($1::uuid[])`, []string{events[1].id, events[3].id}); n != 2 {
		t.Fatalf("comment facts=%d", n)
	}
	for _, e := range events {
		var key string
		var nonce, ciphertext, sourceNonce, sourceCipher []byte
		var tenant, store string
		q := `SELECT s.key_id,s.nonce,s.ciphertext,e.tenant_id::text,e.store_id::text,b.nonce,b.ciphertext FROM social.messages s JOIN meta_inbox.events e ON e.id=s.event_id JOIN meta_private.event_bodies b ON b.event_id=e.id WHERE s.event_id=$1`
		if e.id == events[1].id || e.id == events[3].id {
			q = `SELECT s.key_id,s.nonce,s.ciphertext,e.tenant_id::text,e.store_id::text,b.nonce,b.ciphertext FROM social.comment_events s JOIN meta_inbox.events e ON e.id=s.event_id JOIN meta_private.event_bodies b ON b.event_id=e.id WHERE s.event_id=$1`
		}
		if err := m.f.owner.QueryRow(ctx, q, e.id).Scan(&key, &nonce, &ciphertext, &tenant, &store, &sourceNonce, &sourceCipher); err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(nonce, sourceNonce) || !bytes.Equal(ciphertext, sourceCipher) || !bytes.Equal(miDecrypt(t, m.key, "event", e.id, e.app, e.object, "", e.key, e.hash, tenant, store, e.route, e.epoch, key, nonce, ciphertext), e.plain) {
			t.Fatal("social copy differs from authenticated source")
		}
	}
	for _, table := range domainTables {
		if n := miCount(t, m.f.owner, `SELECT count(*) FROM `+table); n != baseline[table] {
			t.Fatalf("consumer wrote unrelated domain %s: %d to %d", table, baseline[table], n)
		}
	}
	for i, want := range map[int]struct{ tenant, store, app, object, asset string }{0: {m.f.tenantA, m.f.storeA1, miApp, "page", events[0].asset}, 2: {m.f.tenantA, m.f.storeA1, miApp, "instagram", events[2].asset}, 4: {m.f.tenantA, m.f.storeA2, miApp, "page", otherAsset}, 5: {m.f.tenantB, m.f.storeB, miApp, "page", thirdAsset}, 6: {m.f.tenantA, m.f.storeA1, altApp, "page", altAsset}} {
		e := events[i]
		if n := miCount(t, m.f.owner, `SELECT count(*) FROM social.messages s JOIN social.conversations c ON c.id=s.conversation_id WHERE s.event_id=$1 AND c.tenant_id=$2 AND c.store_id=$3 AND c.app_id=$4 AND c.object=$5 AND c.asset_id=$6`, e.id, want.tenant, want.store, want.app, want.object, want.asset); n != 1 {
			t.Fatalf("event %d crossed scope", i)
		}
	}
}

func TestMetaConsumerSQLIdentityAndRollback(t *testing.T) {
	m := miSetup(t)
	ctx := context.Background()
	asset := miAsset()
	b := miBinding(t, m, asset, "facebook", m.f.tenantA, m.f.storeA1, m.f.principalA)
	miRoute(t, m, asset, m.f.tenantA, m.f.storeA1, b)
	e := mcPost(t, m, asset, miMessage(asset, "m."+randomUUID(), "rollback"))
	mcRunning(t, m, e, 1)
	p := mcConsumer(t, m)
	for _, tc := range []struct {
		job             int64
		attempt         int
		family, subject string
	}{{e.job, 0, "message", strings.Repeat("a", 64)}, {e.job + 1, 1, "message", strings.Repeat("a", 64)}, {e.job, 1, "unknown", strings.Repeat("a", 64)}, {e.job, 1, "comment", strings.Repeat("a", 64)}, {e.job, 1, "message", "bad"}, {e.job, 1, "", strings.Repeat("a", 64)}} {
		_, err := p.Exec(ctx, `SELECT meta_inbox.finish_social_event($1::uuid,$2::bigint,$3::integer,$4::text,$5::text)`, e.id, tc.job, tc.attempt, tc.family, tc.subject)
		if miSQLState(err) != "22023" {
			t.Fatalf("invalid finish SQLSTATE=%s", miSQLState(err))
		}
	}
	if _, err := p.Exec(ctx, `SELECT meta_inbox.finish_social_event($1::uuid,$2::bigint,1,NULL,$3)`, e.id, e.job, strings.Repeat("a", 64)); miSQLState(err) != "22023" {
		t.Fatalf("NULL family SQLSTATE=%s", miSQLState(err))
	}
	var outcome string
	if err := p.QueryRow(ctx, `SELECT outcome FROM meta_inbox.load_social_event($1::uuid,$2::bigint,$3::integer)`, e.id, e.job, 1).Scan(&outcome); err != nil || outcome != "READY" {
		t.Fatalf("load outcome=%s err=%v", outcome, err)
	}
	for _, target := range []string{"social.conversations", "social.messages", "meta_inbox.events", "meta_inbox.audit_events"} {
		t.Run("rollback "+target, func(t *testing.T) {
			mustExec(t, m.f.owner, `CREATE FUNCTION public.mc_consumer_fail() RETURNS trigger LANGUAGE plpgsql AS $$BEGIN RAISE EXCEPTION 'synthetic fault' USING ERRCODE='P0001'; END$$`)
			// A test-local AFTER trigger proves each downstream write aborts the entire projection.
			mustExec(t, m.f.owner, `CREATE TRIGGER mc_fault AFTER INSERT OR UPDATE ON `+target+` FOR EACH ROW EXECUTE FUNCTION public.mc_consumer_fail()`)
			t.Cleanup(func() {
				mustExec(t, m.f.owner, `DROP TRIGGER mc_fault ON `+target)
				mustExec(t, m.f.owner, `DROP FUNCTION public.mc_consumer_fail()`)
			})
			tx, err := p.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			_, err = tx.Exec(ctx, `SELECT meta_inbox.finish_social_event($1::uuid,$2::bigint,1,'message',$3)`, e.id, e.job, strings.Repeat("a", 64))
			if err == nil {
				err = tx.Commit(ctx)
			} else {
				_ = tx.Rollback(ctx)
			}
			if miSQLState(err) != "P0001" {
				t.Fatalf("fault SQLSTATE=%s", miSQLState(err))
			}
			if miCount(t, m.f.owner, `SELECT count(*) FROM social.messages WHERE event_id=$1`, e.id) != 0 || miCount(t, m.f.owner, `SELECT count(*) FROM social.conversations WHERE peer_key=$1`, strings.Repeat("a", 64)) != 0 || miCount(t, m.f.owner, `SELECT count(*) FROM meta_inbox.events WHERE id=$1 AND terminal_reason IS NOT NULL`, e.id) != 0 || miCount(t, m.f.owner, `SELECT count(*) FROM meta_inbox.audit_events WHERE event_id=$1 AND action='processed'`, e.id) != 0 {
				t.Fatal("partial projection or sequence survived fault")
			}
		})
	}
	// Existing conversation update is a distinct failure site from its first insert.
	subject := mcSubject()
	warm := mcPost(t, m, asset, miMessage(asset, "m."+randomUUID(), "sequence-baseline"))
	mcRunning(t, m, warm, 1)
	mcFinish(t, p, warm, 1, subject)
	mustExec(t, m.f.owner, `CREATE FUNCTION public.mc_sequence_fail() RETURNS trigger LANGUAGE plpgsql AS $$BEGIN RAISE EXCEPTION 'synthetic sequence fault' USING ERRCODE='P0001'; END$$`)
	mustExec(t, m.f.owner, `CREATE TRIGGER mc_sequence_fault AFTER UPDATE ON social.conversations FOR EACH ROW EXECUTE FUNCTION public.mc_sequence_fail()`)
	defer func() {
		mustExec(t, m.f.owner, `DROP TRIGGER mc_sequence_fault ON social.conversations`)
		mustExec(t, m.f.owner, `DROP FUNCTION public.mc_sequence_fail()`)
	}()
	_, seqErr := p.Exec(ctx, `SELECT meta_inbox.finish_social_event($1::uuid,$2::bigint,1,'message',$3)`, e.id, e.job, subject)
	if miSQLState(seqErr) != "P0001" {
		t.Fatalf("existing conversation sequence fault SQLSTATE=%s", miSQLState(seqErr))
	}
	if miCount(t, m.f.owner, `SELECT next_seq FROM social.conversations WHERE peer_key=$1`, subject) != 1 ||
		miCount(t, m.f.owner, `SELECT count(*) FROM social.messages WHERE event_id=$1`, e.id) != 0 ||
		miCount(t, m.f.owner, `SELECT count(*) FROM meta_inbox.events WHERE id=$1 AND terminal_reason IS NOT NULL`, e.id) != 0 ||
		miCount(t, m.f.owner, `SELECT count(*) FROM meta_inbox.audit_events WHERE event_id=$1 AND action='processed'`, e.id) != 0 {
		t.Fatal("sequence fault left partial fact or advanced next_seq")
	}
}

func TestMetaConsumerCommentRollbackAtEveryWrite(t *testing.T) {
	m := miSetup(t)
	ctx := context.Background()
	asset := miAsset()
	binding := miBinding(t, m, asset, "facebook", m.f.tenantA, m.f.storeA1, m.f.principalA)
	miRoute(t, m, asset, m.f.tenantA, m.f.storeA1, binding)
	raw := []byte(fmt.Sprintf(`{"object":"page","entry":[{"id":%q,"changes":[{"field":"feed","value":{"item":"comment","verb":"add","comment_id":%q,"message":"rollback-comment"}}]}]}`, asset, miAsset()))
	e := mcPost(t, m, asset, raw)
	mcRunning(t, m, e, 1)
	p := mcConsumer(t, m)
	for _, target := range []string{"social.comment_events", "meta_inbox.events", "meta_inbox.audit_events"} {
		t.Run(target, func(t *testing.T) {
			mustExec(t, m.f.owner, `CREATE FUNCTION public.mc_comment_fail() RETURNS trigger LANGUAGE plpgsql AS $$BEGIN RAISE EXCEPTION 'synthetic comment fault' USING ERRCODE='P0001'; END$$`)
			mustExec(t, m.f.owner, `CREATE TRIGGER mc_comment_fault AFTER INSERT OR UPDATE ON `+target+` FOR EACH ROW EXECUTE FUNCTION public.mc_comment_fail()`)
			t.Cleanup(func() {
				mustExec(t, m.f.owner, `DROP TRIGGER mc_comment_fault ON `+target)
				mustExec(t, m.f.owner, `DROP FUNCTION public.mc_comment_fail()`)
			})
			tx, err := p.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			_, err = tx.Exec(ctx, `SELECT meta_inbox.finish_social_event($1::uuid,$2::bigint,1,'comment',$3)`, e.id, e.job, strings.Repeat("b", 64))
			if err == nil {
				err = tx.Commit(ctx)
			} else {
				_ = tx.Rollback(ctx)
			}
			if miSQLState(err) != "P0001" {
				t.Fatalf("comment fault SQLSTATE=%s", miSQLState(err))
			}
			if miCount(t, m.f.owner, `SELECT count(*) FROM social.comment_events WHERE event_id=$1`, e.id) != 0 || miCount(t, m.f.owner, `SELECT count(*) FROM meta_inbox.events WHERE id=$1 AND terminal_reason IS NOT NULL`, e.id) != 0 || miCount(t, m.f.owner, `SELECT count(*) FROM meta_inbox.audit_events WHERE event_id=$1 AND action='processed'`, e.id) != 0 {
				t.Fatal("comment partial projection survived fault")
			}
		})
	}
}

func mcFinish(t *testing.T, p *pgxpool.Pool, e mcEvent, attempt int, subject string) {
	t.Helper()
	if _, err := p.Exec(context.Background(), `SELECT meta_inbox.finish_social_event($1::uuid,$2::bigint,$3::integer,'message',$4)`, e.id, e.job, attempt, subject); err != nil {
		t.Fatal("finish synthetic running attempt", err)
	}
}

func TestMetaConsumerReverseSequenceReplayAndPurge(t *testing.T) {
	m := miSetup(t)
	ctx := context.Background()
	asset := miAsset()
	binding := miBinding(t, m, asset, "facebook", m.f.tenantA, m.f.storeA1, m.f.principalA)
	miRoute(t, m, asset, m.f.tenantA, m.f.storeA1, binding)
	p := mcConsumer(t, m)
	subject := mcSubject()
	first := mcPost(t, m, asset, miMessage(asset, "m."+randomUUID(), "received-first"))
	second := mcPost(t, m, asset, []byte(strings.Replace(string(miMessage(asset, "m."+randomUUID(), "received-second")), `"time":123`, `"time":124`, 1)))
	mcRunning(t, m, first, 1)
	mcRunning(t, m, second, 1)
	mcFinish(t, p, second, 1, subject)
	mcFinish(t, p, first, 1, subject)
	var seqFirst, seqSecond, next int64
	var occurredFirst, occurredSecond, receivedFirst, receivedSecond bool
	if err := m.f.owner.QueryRow(ctx, `SELECT a.server_seq,b.server_seq,c.next_seq,
	 a.occurred_at IS NOT DISTINCT FROM ea.occurred_at,b.occurred_at IS NOT DISTINCT FROM eb.occurred_at,
	 a.received_at=ea.created_at,b.received_at=eb.created_at
	 FROM social.messages a JOIN social.messages b ON true JOIN social.conversations c ON c.id=a.conversation_id
	 JOIN meta_inbox.events ea ON ea.id=a.event_id JOIN meta_inbox.events eb ON eb.id=b.event_id
	 WHERE a.event_id=$1 AND b.event_id=$2 AND a.conversation_id=b.conversation_id`, first.id, second.id).Scan(&seqFirst, &seqSecond, &next, &occurredFirst, &occurredSecond, &receivedFirst, &receivedSecond); err != nil {
		t.Fatal(err)
	}
	if seqFirst != 2 || seqSecond != 1 || next != 2 || !occurredFirst || !occurredSecond || !receivedFirst || !receivedSecond {
		t.Fatal("materialization order or original source timestamps changed")
	}
	// A lost success response can replay against the same fact without taking a sequence.
	mcFinish(t, p, first, 1, subject)
	if n := miCount(t, m.f.owner, `SELECT next_seq FROM social.conversations WHERE peer_key=$1`, subject); n != 2 {
		t.Fatal("replay advanced conversation sequence")
	}
	mustExec(t, m.f.owner, `UPDATE river_meta.river_job SET state='completed',finalized_at=clock_timestamp() WHERE id=$1`, first.job)
	mustExec(t, m.f.owner, `UPDATE meta_private.event_bodies SET expires_at=clock_timestamp()-interval '1 hour' WHERE event_id=$1`, first.id)
	var purged int
	if err := m.curator.QueryRow(ctx, `SELECT meta_inbox.purge_expired(10)`).Scan(&purged); err != nil {
		t.Fatal(err)
	}
	if purged < 1 || miCount(t, m.f.owner, `SELECT count(*) FROM meta_private.event_bodies WHERE event_id=$1`, first.id) != 0 {
		t.Fatal("processed source body did not purge")
	}
	var outcome string
	var exposed int
	if err := p.QueryRow(ctx, `SELECT outcome,num_nonnulls(app_id,object,asset_id,kind,event_key,payload_hash,tenant_id,store_id,route_id,route_epoch,key_id,nonce,ciphertext) FROM meta_inbox.load_social_event($1::uuid,$2::bigint,1)`, first.id, first.job).Scan(&outcome, &exposed); err != nil || outcome != "ALREADY" || exposed != 0 {
		t.Fatalf("post-purge historical outcome=%s exposed fields=%d err=%v", outcome, exposed, err)
	}
	if miCount(t, m.f.owner, `SELECT count(*) FROM social.messages WHERE event_id=$1`, first.id) != 1 || miCount(t, m.f.owner, `SELECT next_seq FROM social.conversations WHERE peer_key=$1`, subject) != 2 {
		t.Fatal("source purge changed social history")
	}
}

func TestMetaConsumerConcurrentDifferentEventsSamePeer(t *testing.T) {
	m := miSetup(t)
	ctx := context.Background()
	asset := miAsset()
	binding := miBinding(t, m, asset, "facebook", m.f.tenantA, m.f.storeA1, m.f.principalA)
	miRoute(t, m, asset, m.f.tenantA, m.f.storeA1, binding)
	first := mcPost(t, m, asset, miMessage(asset, "m."+randomUUID(), "concurrent-first"))
	second := mcPost(t, m, asset, miMessage(asset, "m."+randomUUID(), "concurrent-second"))
	mcRunning(t, m, first, 1)
	mcRunning(t, m, second, 1)
	p := mcConsumer(t, m)
	subject := mcSubject()
	tx, err := p.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `SELECT meta_inbox.finish_social_event($1::uuid,$2::bigint,1,'message',$3)`, first.id, first.job, subject); err != nil {
		_ = tx.Rollback(ctx)
		t.Fatal(err)
	}
	conn, err := p.Acquire(ctx)
	if err != nil {
		_ = tx.Rollback(ctx)
		t.Fatal(err)
	}
	defer conn.Release()
	marker := "mc_peer_" + strings.ReplaceAll(randomUUID(), "-", "")
	if _, err := conn.Exec(ctx, `SET application_name='`+marker+`'`); err != nil {
		_ = tx.Rollback(ctx)
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, e := conn.Exec(ctx, `SELECT meta_inbox.finish_social_event($1::uuid,$2::bigint,1,'message',$3)`, second.id, second.job, subject)
		done <- e
	}()
	mcWaitLock(t, m, marker)
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal("second peer projection", err)
	}
	var firstSeq, secondSeq, next int64
	var firstSource, secondSource bool
	if err := m.f.owner.QueryRow(ctx, `SELECT a.server_seq,b.server_seq,c.next_seq,a.received_at=ea.created_at,b.received_at=eb.created_at FROM social.messages a JOIN social.messages b ON a.conversation_id=b.conversation_id JOIN social.conversations c ON c.id=a.conversation_id JOIN meta_inbox.events ea ON ea.id=a.event_id JOIN meta_inbox.events eb ON eb.id=b.event_id WHERE a.event_id=$1 AND b.event_id=$2`, first.id, second.id).Scan(&firstSeq, &secondSeq, &next, &firstSource, &secondSource); err != nil {
		t.Fatal(err)
	}
	if firstSeq != 1 || secondSeq != 2 || next != 2 || !firstSource || !secondSource {
		t.Fatal("concurrent same-peer materialization lost ordering or source time")
	}
}

func TestMetaConsumerSameEventLockAndFinalAuthority(t *testing.T) {
	m := miSetup(t)
	ctx := context.Background()
	asset := miAsset()
	binding := miBinding(t, m, asset, "facebook", m.f.tenantA, m.f.storeA1, m.f.principalA)
	miRoute(t, m, asset, m.f.tenantA, m.f.storeA1, binding)
	var guardDef, authorityDef, enabled, triggerFunction string
	var deferrable, initiallyDeferred bool
	if err := m.f.owner.QueryRow(ctx, `SELECT pg_get_functiondef('meta_inbox.guard_social_insert()'::regprocedure)`).Scan(&guardDef); err != nil {
		t.Fatal(err)
	}
	if err := m.f.owner.QueryRow(ctx, `SELECT pg_get_functiondef('meta_inbox.require_authority(text)'::regprocedure)`).Scan(&authorityDef); err != nil {
		t.Fatal(err)
	}
	if err := m.f.owner.QueryRow(ctx, `SELECT tgenabled::text,tgdeferrable,tginitdeferred,tgfoid::regprocedure::text FROM pg_trigger WHERE tgrelid='social.messages'::regclass AND tgname='social_message_commit'`).Scan(&enabled, &deferrable, &initiallyDeferred, &triggerFunction); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(guardDef, "meta_inbox.require_authority('commerce_meta_consumer')") || !strings.Contains(authorityDef, "pg_has_role(session_user,r.oid,'MEMBER')") || enabled != "O" || !deferrable || !initiallyDeferred || triggerFunction != "meta_inbox.guard_social_insert()" {
		t.Fatalf("runtime guard definition/trigger mismatch: auth=%v member=%v enabled=%s deferred=%v/%v function=%s", strings.Contains(guardDef, "meta_inbox.require_authority('commerce_meta_consumer')"), strings.Contains(authorityDef, "pg_has_role(session_user,r.oid,'MEMBER')"), enabled, deferrable, initiallyDeferred, triggerFunction)
	}
	e := mcPost(t, m, asset, miMessage(asset, "m."+randomUUID(), "lock-race"))
	mcRunning(t, m, e, 1)
	p := mcConsumer(t, m)
	subject := mcSubject()
	tx, err := p.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var outcome string
	if err := tx.QueryRow(ctx, `SELECT outcome FROM meta_inbox.load_social_event($1::uuid,$2::bigint,1)`, e.id, e.job).Scan(&outcome); err != nil || outcome != "READY" {
		_ = tx.Rollback(ctx)
		t.Fatalf("first load=%s err=%v", outcome, err)
	}
	conn, err := p.Acquire(ctx)
	if err != nil {
		_ = tx.Rollback(ctx)
		t.Fatal(err)
	}
	defer conn.Release()
	marker := "mc_same_" + strings.ReplaceAll(randomUUID(), "-", "")
	if _, err := conn.Exec(ctx, `SET application_name=`+"'"+marker+"'"); err != nil {
		_ = tx.Rollback(ctx)
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, e := conn.Exec(ctx, `SELECT meta_inbox.finish_social_event($1::uuid,$2::bigint,1,'message',$3)`, e.id, e.job, subject)
		done <- e
	}()
	mcWaitLock(t, m, marker)
	if _, err := tx.Exec(ctx, `SELECT meta_inbox.finish_social_event($1::uuid,$2::bigint,1,'message',$3)`, e.id, e.job, subject); err != nil {
		_ = tx.Rollback(ctx)
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal("serialized duplicate", err)
	}
	if miCount(t, m.f.owner, `SELECT count(*) FROM social.messages WHERE event_id=$1`, e.id) != 1 || miCount(t, m.f.owner, `SELECT next_seq FROM social.conversations WHERE peer_key=$1`, subject) != 1 {
		t.Fatal("same-event race duplicated fact or sequence")
	}
	// Change role membership after finish, before the deferred COMMIT guard.
	e2 := mcPost(t, m, asset, miMessage(asset, "m."+randomUUID(), "commit-authority"))
	mcRunning(t, m, e2, 1)
	tx2, err := p.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx2.Exec(ctx, `SELECT meta_inbox.finish_social_event($1::uuid,$2::bigint,1,'message',$3)`, e2.id, e2.job, subject); err != nil {
		_ = tx2.Rollback(ctx)
		t.Fatal(err)
	}
	role := pgx.Identifier{p.Config().ConnConfig.User}.Sanitize()
	mustExec(t, m.f.owner, `GRANT commerce_runtime TO `+role+` WITH INHERIT TRUE, SET FALSE`)
	defer mustExec(t, m.f.owner, `REVOKE commerce_runtime FROM `+role)
	if err := tx2.Commit(ctx); miSQLState(err) != "42501" {
		t.Fatalf("mixed authority at final COMMIT SQLSTATE=%s err=%v", miSQLState(err), err)
	}
	if miCount(t, m.f.owner, `SELECT count(*) FROM social.messages WHERE event_id=$1`, e2.id) != 0 ||
		miCount(t, m.f.owner, `SELECT count(*) FROM social.comment_events WHERE event_id=$1`, e2.id) != 0 ||
		miCount(t, m.f.owner, `SELECT count(*) FROM meta_inbox.events WHERE id=$1 AND terminal_reason IS NOT NULL`, e2.id) != 0 ||
		miCount(t, m.f.owner, `SELECT count(*) FROM meta_inbox.audit_events WHERE event_id=$1 AND action='processed'`, e2.id) != 0 ||
		miCount(t, m.f.owner, `SELECT next_seq FROM social.conversations WHERE peer_key=$1`, subject) != 1 {
		t.Fatal("authority change left fact, terminal, audit or sequence")
	}
}

func TestMetaConsumerCommitGuardActuallyFires(t *testing.T) {
	m := miSetup(t)
	ctx := context.Background()
	asset := miAsset()
	binding := miBinding(t, m, asset, "facebook", m.f.tenantA, m.f.storeA1, m.f.principalA)
	miRoute(t, m, asset, m.f.tenantA, m.f.storeA1, binding)
	e := mcPost(t, m, asset, miMessage(asset, "m."+randomUUID(), "guard-probe"))
	mcRunning(t, m, e, 1)
	p := mcConsumer(t, m)
	tx, err := p.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `SELECT meta_inbox.finish_social_event($1::uuid,$2::bigint,1,'message',$3)`, e.id, e.job, strings.Repeat("e", 64)); err != nil {
		_ = tx.Rollback(ctx)
		t.Fatal(err)
	}
	// Delete only the short-retention source copy after finish. The deferred
	// guard must verify source lineage anew at COMMIT and roll every fact back.
	mustExec(t, m.f.owner, `DELETE FROM meta_private.event_bodies WHERE event_id=$1`, e.id)
	err = tx.Commit(ctx)
	if miSQLState(err) != "22023" {
		t.Fatalf("source removed before COMMIT escaped deferred guard: SQLSTATE=%s err=%v", miSQLState(err), err)
	}
	if miCount(t, m.f.owner, `SELECT count(*) FROM social.messages WHERE event_id=$1`, e.id) != 0 || miCount(t, m.f.owner, `SELECT count(*) FROM meta_inbox.events WHERE id=$1 AND terminal_reason IS NOT NULL`, e.id) != 0 {
		t.Fatal("deferred guard failure left partial projection")
	}
}

func TestMetaConsumerWarmedFinalRoleChanges(t *testing.T) {
	m := miSetup(t)
	ctx := context.Background()
	for _, mode := range []string{"revoke-consumer", "indirect-runtime", "make-settable"} {
		t.Run(mode, func(t *testing.T) {
			dsn := miRole(t, m.f, "commerce_meta_consumer")
			u, err := url.Parse(dsn)
			if err != nil {
				t.Fatal(err)
			}
			login := pgx.Identifier{u.User.Username()}.Sanitize()
			p, err := pgxpool.New(ctx, dsn)
			if err != nil {
				t.Fatal(err)
			}
			defer p.Close()
			conn, err := p.Acquire(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Release()
			asset := miAsset()
			binding := miBinding(t, m, asset, "facebook", m.f.tenantA, m.f.storeA1, m.f.principalA)
			miRoute(t, m, asset, m.f.tenantA, m.f.storeA1, binding)
			subject := mcSubject()
			warm := mcPost(t, m, asset, miMessage(asset, "m."+randomUUID(), "warm-session"))
			mcRunning(t, m, warm, 1)
			warmTx, err := conn.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := warmTx.Exec(ctx, `SELECT meta_inbox.finish_social_event($1::uuid,$2::bigint,1,'message',$3)`, warm.id, warm.job, subject); err != nil {
				_ = warmTx.Rollback(ctx)
				t.Fatal(err)
			}
			if err := warmTx.Commit(ctx); err != nil {
				t.Fatal(err)
			}
			e := mcPost(t, m, asset, miMessage(asset, "m."+randomUUID(), "role-mutation"))
			mcRunning(t, m, e, 1)
			tx, err := conn.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := tx.Exec(ctx, `SELECT meta_inbox.finish_social_event($1::uuid,$2::bigint,1,'message',$3)`, e.id, e.job, subject); err != nil {
				_ = tx.Rollback(ctx)
				t.Fatal(err)
			}
			switch mode {
			case "revoke-consumer":
				mustExec(t, m.f.owner, `REVOKE commerce_meta_consumer FROM `+login)
			case "make-settable":
				mustExec(t, m.f.owner, `GRANT commerce_meta_consumer TO `+login+` WITH INHERIT TRUE, SET TRUE`)
			case "indirect-runtime":
				bridge := "mc_bridge_" + strings.ReplaceAll(randomUUID(), "-", "")
				quoted := pgx.Identifier{bridge}.Sanitize()
				mustExec(t, m.f.owner, `CREATE ROLE `+quoted+` NOLOGIN NOSUPERUSER NOBYPASSRLS NOCREATEDB NOCREATEROLE NOREPLICATION`)
				mustExec(t, m.f.owner, `GRANT commerce_runtime TO `+quoted+` WITH INHERIT TRUE, SET FALSE`)
				mustExec(t, m.f.owner, `GRANT `+quoted+` TO `+login+` WITH INHERIT TRUE, SET FALSE`)
				t.Cleanup(func() {
					mustExec(t, m.f.owner, `REVOKE `+quoted+` FROM `+login)
					mustExec(t, m.f.owner, `REVOKE commerce_runtime FROM `+quoted)
					mustExec(t, m.f.owner, `DROP ROLE `+quoted)
				})
			}
			if err := tx.Commit(ctx); miSQLState(err) != "42501" {
				t.Fatalf("warmed %s COMMIT SQLSTATE=%s err=%v", mode, miSQLState(err), err)
			}
			if miCount(t, m.f.owner, `SELECT count(*) FROM social.messages WHERE event_id=$1`, e.id) != 0 || miCount(t, m.f.owner, `SELECT count(*) FROM meta_inbox.events WHERE id=$1 AND terminal_reason IS NOT NULL`, e.id) != 0 || miCount(t, m.f.owner, `SELECT count(*) FROM meta_inbox.audit_events WHERE event_id=$1 AND action='processed'`, e.id) != 0 || miCount(t, m.f.owner, `SELECT next_seq FROM social.conversations WHERE peer_key=$1`, subject) != 1 {
				t.Fatal("warmed role mutation left fact, terminal, audit or sequence")
			}
		})
	}
}

func TestMetaConsumerFinalCommitDatabaseOwner(t *testing.T) {
	m := miSetup(t)
	ctx := context.Background()
	asset := miAsset()
	binding := miBinding(t, m, asset, "facebook", m.f.tenantA, m.f.storeA1, m.f.principalA)
	miRoute(t, m, asset, m.f.tenantA, m.f.storeA1, binding)
	e := mcPost(t, m, asset, miMessage(asset, "m."+randomUUID(), "database-owner"))
	mcRunning(t, m, e, 1)
	dsn := miRole(t, m.f, "commerce_meta_consumer")
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatal(err)
	}
	login := pgx.Identifier{u.User.Username()}.Sanitize()
	p, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	tx, err := p.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `SELECT meta_inbox.finish_social_event($1::uuid,$2::bigint,1,'message',$3)`, e.id, e.job, strings.Repeat("f", 64)); err != nil {
		_ = tx.Rollback(ctx)
		t.Fatal(err)
	}
	mustExec(t, m.f.owner, `ALTER DATABASE lc_foundation_test OWNER TO `+login)
	defer mustExec(t, m.f.owner, `ALTER DATABASE lc_foundation_test OWNER TO postgres`)
	if err := tx.Commit(ctx); miSQLState(err) != "42501" {
		t.Fatalf("database owner at final COMMIT SQLSTATE=%s err=%v", miSQLState(err), err)
	}
	if miCount(t, m.f.owner, `SELECT count(*) FROM social.messages WHERE event_id=$1`, e.id) != 0 ||
		miCount(t, m.f.owner, `SELECT count(*) FROM social.comment_events WHERE event_id=$1`, e.id) != 0 ||
		miCount(t, m.f.owner, `SELECT count(*) FROM social.conversations WHERE peer_key=$1`, strings.Repeat("f", 64)) != 0 ||
		miCount(t, m.f.owner, `SELECT count(*) FROM meta_inbox.events WHERE id=$1 AND terminal_reason IS NOT NULL`, e.id) != 0 ||
		miCount(t, m.f.owner, `SELECT count(*) FROM meta_inbox.audit_events WHERE event_id=$1 AND action='processed'`, e.id) != 0 {
		t.Fatal("database ownership change left fact, terminal, audit or conversation")
	}
}

func mcWaitLock(t *testing.T, m miTest, marker string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if miCount(t, m.f.owner, `SELECT count(*) FROM pg_stat_activity WHERE application_name=$1 AND wait_event_type='Lock'`, marker) == 1 {
			return
		}
		time.Sleep(10 * time.Millisecond) // Observes a PG lock state, not an elapsed-time race assumption.
	}
	t.Fatal("competing consumer never observed blocked on PostgreSQL lock")
}

func TestMetaConsumerRouteBindingAndProofWaitFences(t *testing.T) {
	m := miSetup(t)
	ctx := context.Background()
	p := mcConsumer(t, m)
	for _, mode := range []string{"route-disabled", "binding-version", "proof-clock"} {
		t.Run(mode, func(t *testing.T) {
			asset := miAsset()
			binding := miBinding(t, m, asset, "facebook", m.f.tenantA, m.f.storeA1, m.f.principalA)
			route, _ := miRoute(t, m, asset, m.f.tenantA, m.f.storeA1, binding)
			e := mcPost(t, m, asset, miMessage(asset, "m."+randomUUID(), "fenced"))
			mcRunning(t, m, e, 1)
			ownerTx, err := m.f.owner.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			switch mode {
			case "route-disabled":
				_, err = ownerTx.Exec(ctx, `UPDATE meta_inbox.routes SET enabled=false WHERE id=$1`, route)
			case "binding-version":
				_, err = ownerTx.Exec(ctx, `UPDATE integration.bindings SET semantic_version=semantic_version+1 WHERE id=$1`, binding)
			case "proof-clock":
				_, err = ownerTx.Exec(ctx, `UPDATE meta_inbox.routes SET proof_expires=clock_timestamp()+interval '100 milliseconds' WHERE id=$1`, route)
			}
			if err != nil {
				_ = ownerTx.Rollback(ctx)
				t.Fatal(err)
			}
			conn, err := p.Acquire(ctx)
			if err != nil {
				_ = ownerTx.Rollback(ctx)
				t.Fatal(err)
			}
			marker := "mc_fence_" + strings.ReplaceAll(randomUUID(), "-", "")
			if _, err := conn.Exec(ctx, `SET application_name='`+marker+`'`); err != nil {
				conn.Release()
				_ = ownerTx.Rollback(ctx)
				t.Fatal(err)
			}
			result := make(chan struct {
				outcome string
				err     error
			}, 1)
			go func() {
				var outcome string
				err := conn.QueryRow(ctx, `SELECT outcome FROM meta_inbox.load_social_event($1::uuid,$2::bigint,1)`, e.id, e.job).Scan(&outcome)
				result <- struct {
					outcome string
					err     error
				}{outcome, err}
			}()
			mcWaitLock(t, m, marker)
			if mode == "proof-clock" {
				deadline := time.Now().Add(2 * time.Second)
				for {
					var expired bool
					if err := ownerTx.QueryRow(ctx, `SELECT clock_timestamp()>proof_expires FROM meta_inbox.routes WHERE id=$1`, route).Scan(&expired); err != nil {
						t.Fatal(err)
					}
					if expired {
						break
					}
					if time.Now().After(deadline) {
						t.Fatal("database clock never crossed proof expiry")
					}
				}
			}
			if err := ownerTx.Commit(ctx); err != nil {
				conn.Release()
				t.Fatal(err)
			}
			r := <-result
			conn.Release()
			if r.err != nil || r.outcome != "STALE" {
				t.Fatalf("post-wait outcome=%s err=%v", r.outcome, r.err)
			}
			if miCount(t, m.f.owner, `SELECT count(*) FROM social.messages WHERE event_id=$1`, e.id) != 0 || miCount(t, m.f.owner, `SELECT count(*) FROM meta_inbox.events WHERE id=$1 AND terminal_reason IS NOT NULL`, e.id) != 0 {
				t.Fatal("stale route/binding/proof materialized")
			}
		})
	}
}

func TestMetaConsumerRescuedAttemptAfterJobLockWait(t *testing.T) {
	m := miSetup(t)
	ctx := context.Background()
	asset := miAsset()
	binding := miBinding(t, m, asset, "facebook", m.f.tenantA, m.f.storeA1, m.f.principalA)
	miRoute(t, m, asset, m.f.tenantA, m.f.storeA1, binding)
	e := mcPost(t, m, asset, miMessage(asset, "m."+randomUUID(), "rescue-fence"))
	mcRunning(t, m, e, 1)
	p := mcConsumer(t, m)
	ownerTx, err := m.f.owner.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ownerTx.Exec(ctx, `UPDATE river_meta.river_job SET attempt=2 WHERE id=$1`, e.job); err != nil {
		_ = ownerTx.Rollback(ctx)
		t.Fatal(err)
	}
	conn, err := p.Acquire(ctx)
	if err != nil {
		_ = ownerTx.Rollback(ctx)
		t.Fatal(err)
	}
	marker := "mc_rescue_" + strings.ReplaceAll(randomUUID(), "-", "")
	if _, err := conn.Exec(ctx, `SET application_name='`+marker+`'`); err != nil {
		conn.Release()
		_ = ownerTx.Rollback(ctx)
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		var outcome string
		done <- conn.QueryRow(ctx, `SELECT outcome FROM meta_inbox.load_social_event($1::uuid,$2::bigint,1)`, e.id, e.job).Scan(&outcome)
	}()
	mcWaitLock(t, m, marker)
	if err := ownerTx.Commit(ctx); err != nil {
		conn.Release()
		t.Fatal(err)
	}
	err = <-done
	conn.Release()
	if miSQLState(err) != "22023" {
		t.Fatalf("rescued attempt after observed River row lock SQLSTATE=%s err=%v", miSQLState(err), err)
	}
	if miCount(t, m.f.owner, `SELECT count(*) FROM social.messages WHERE event_id=$1`, e.id) != 0 || miCount(t, m.f.owner, `SELECT count(*) FROM meta_inbox.events WHERE id=$1 AND terminal_reason IS NOT NULL`, e.id) != 0 {
		t.Fatal("rescued attempt materialized")
	}
}

func TestMetaConsumerProofExpiresBetweenFinishAndCommit(t *testing.T) {
	m := miSetup(t)
	ctx := context.Background()
	asset := miAsset()
	binding := miBinding(t, m, asset, "facebook", m.f.tenantA, m.f.storeA1, m.f.principalA)
	route, _ := miRoute(t, m, asset, m.f.tenantA, m.f.storeA1, binding)
	e := mcPost(t, m, asset, miMessage(asset, "m."+randomUUID(), "commit-proof-clock"))
	mcRunning(t, m, e, 1)
	p := mcConsumer(t, m)
	mustExec(t, m.f.owner, `UPDATE meta_inbox.routes SET proof_expires=clock_timestamp()+interval '1 second' WHERE id=$1`, route)
	tx, err := p.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var proofCurrent bool
	if err := m.f.owner.QueryRow(ctx, `SELECT clock_timestamp()<proof_expires FROM meta_inbox.routes WHERE id=$1`, route).Scan(&proofCurrent); err != nil {
		t.Fatal(err)
	}
	if !proofCurrent {
		_ = tx.Rollback(ctx)
		t.Fatal("proof expired before finish; fixture did not exercise COMMIT fence")
	}
	if _, err := tx.Exec(ctx, `SELECT meta_inbox.finish_social_event($1::uuid,$2::bigint,1,'message',$3)`, e.id, e.job, mcSubject()); err != nil {
		_ = tx.Rollback(ctx)
		t.Fatal("finish before proof deadline", err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for {
		var expired bool
		if err := m.f.owner.QueryRow(ctx, `SELECT clock_timestamp()>proof_expires FROM meta_inbox.routes WHERE id=$1`, route).Scan(&expired); err != nil {
			_ = tx.Rollback(ctx)
			t.Fatal(err)
		}
		if expired {
			break
		}
		if time.Now().After(deadline) {
			_ = tx.Rollback(ctx)
			t.Fatal("PG clock did not cross proof deadline")
		}
	}
	if err := tx.Commit(ctx); miSQLState(err) != "PT409" {
		t.Fatalf("expired proof at final COMMIT SQLSTATE=%s err=%v", miSQLState(err), err)
	}
	if miCount(t, m.f.owner, `SELECT count(*) FROM social.messages WHERE event_id=$1`, e.id) != 0 || miCount(t, m.f.owner, `SELECT count(*) FROM meta_inbox.events WHERE id=$1 AND terminal_reason IS NOT NULL`, e.id) != 0 || miCount(t, m.f.owner, `SELECT count(*) FROM meta_inbox.audit_events WHERE event_id=$1 AND action='processed'`, e.id) != 0 {
		t.Fatal("expired proof left projection")
	}
}

func TestMetaConsumerReviewedAndStaleNeverProcess(t *testing.T) {
	m := miSetup(t)
	ctx := context.Background()
	p := mcConsumer(t, m)
	for _, mode := range []string{"reviewed", "stale"} {
		t.Run(mode, func(t *testing.T) {
			asset := miAsset()
			binding := miBinding(t, m, asset, "facebook", m.f.tenantA, m.f.storeA1, m.f.principalA)
			route, _ := miRoute(t, m, asset, m.f.tenantA, m.f.storeA1, binding)
			e := mcPost(t, m, asset, miMessage(asset, "m."+randomUUID(), mode))
			mcRunning(t, m, e, 1)
			if mode == "reviewed" {
				if _, err := m.curator.Exec(ctx, `SELECT meta_inbox.record_terminal($1,'retention_discarded',$2)`, e.id, strings.Repeat("a", 64)); err != nil {
					t.Fatal(err)
				}
			} else {
				mustExec(t, m.f.owner, `UPDATE meta_inbox.routes SET enabled=false WHERE id=$1`, route)
			}
			var outcome string
			var exposed int
			if err := p.QueryRow(ctx, `SELECT outcome,num_nonnulls(app_id,object,asset_id,kind,event_key,payload_hash,tenant_id,store_id,route_id,route_epoch,key_id,nonce,ciphertext) FROM meta_inbox.load_social_event($1::uuid,$2::bigint,1)`, e.id, e.job).Scan(&outcome, &exposed); err != nil {
				t.Fatal(err)
			}
			want := "STALE"
			if mode == "reviewed" {
				want = "REVIEWED"
			}
			if outcome != want || exposed != 0 {
				t.Fatalf("nonREADY outcome=%s exposed-columns=%d", outcome, exposed)
			}
			if _, err := p.Exec(ctx, `SELECT meta_inbox.finish_social_event($1::uuid,$2::bigint,1,'message',$3)`, e.id, e.job, mcSubject()); miSQLState(err) != "PT409" {
				t.Fatalf("nonREADY finish SQLSTATE=%s", miSQLState(err))
			}
			if miCount(t, m.f.owner, `SELECT count(*) FROM social.messages WHERE event_id=$1`, e.id) != 0 || miCount(t, m.f.owner, `SELECT count(*) FROM meta_inbox.events WHERE id=$1 AND terminal_reason='processed'`, e.id) != 0 || miCount(t, m.f.owner, `SELECT count(*) FROM meta_inbox.audit_events WHERE event_id=$1 AND action='processed'`, e.id) != 0 {
				t.Fatal("reviewed/stale materialized")
			}
		})
	}
}

func TestMetaConsumerCryptoFailureKeepsPendingBody(t *testing.T) {
	m := miSetup(t)
	ctx := context.Background()
	workerPool := miPool(t, m.f, "commerce_meta_worker")
	consumer := mcConsumer(t, m)
	for _, mode := range []string{"missing-key", "tampered-tag", "invalid-classifier"} {
		t.Run(mode, func(t *testing.T) {
			asset := miAsset()
			binding := miBinding(t, m, asset, "facebook", m.f.tenantA, m.f.storeA1, m.f.principalA)
			miRoute(t, m, asset, m.f.tenantA, m.f.storeA1, binding)
			e := mcPost(t, m, asset, miMessage(asset, "m."+randomUUID(), "crypto-negative"))
			keys := mcKeys(t, m)
			if mode == "missing-key" {
				var err error
				keys, err = meta.NewPayloadKeyring("other_test_key", map[string][]byte{"other_test_key": randomBytes(32)})
				if err != nil {
					t.Fatal(err)
				}
			} else if mode == "tampered-tag" {
				var cipher []byte
				if err := m.f.owner.QueryRow(ctx, `SELECT ciphertext FROM meta_private.event_bodies WHERE event_id=$1`, e.id).Scan(&cipher); err != nil {
					t.Fatal(err)
				}
				cipher[len(cipher)-1] ^= 1
				mustExec(t, m.f.owner, `UPDATE meta_private.event_bodies SET ciphertext=$2 WHERE event_id=$1`, e.id, cipher)
			} else {
				// Synthetic storage corruption: the AEAD and digest remain valid, but
				// the strict Meta classifier must reject this non-event plaintext.
				plain := []byte(`{"not":"a Meta event"}`)
				sum := sha256.Sum256(plain)
				hash := hex.EncodeToString(sum[:])
				nonce := randomBytes(12)
				aad, err := json.Marshal([]any{"livecommerce/meta-payload/v1", "event", e.id, e.app, e.object, "", e.key, hash, m.f.tenantA, m.f.storeA1, e.route, e.epoch, miKeyID})
				if err != nil {
					t.Fatal(err)
				}
				block, err := aes.NewCipher(m.key)
				if err != nil {
					t.Fatal(err)
				}
				aead, err := cipher.NewGCM(block)
				if err != nil {
					t.Fatal(err)
				}
				sealed := aead.Seal(nil, nonce, plain, aad)
				mustExec(t, m.f.owner, `UPDATE meta_inbox.events SET payload_hash=$2 WHERE id=$1`, e.id, hash)
				mustExec(t, m.f.owner, `UPDATE meta_private.event_bodies SET nonce=$2,ciphertext=$3 WHERE event_id=$1`, e.id, nonce, sealed)
			}
			w, err := meta.NewConsumerWorker(ctx, consumer, keys)
			if err != nil {
				t.Fatal(err)
			}
			workers := river.NewWorkers()
			river.AddWorker(workers, w)
			client, err := river.NewClient(riverpgxv5.New(workerPool), &river.Config{Schema: "river_meta", Workers: workers, Queues: map[string]river.QueueConfig{"meta_inbox": {MaxWorkers: 1}}, Logger: slog.New(slog.NewTextHandler(io.Discard, nil)), JobTimeout: 15 * time.Second, RescueStuckJobsAfter: 30 * time.Second})
			if err != nil {
				t.Fatal(err)
			}
			if err := client.Start(ctx); err != nil {
				t.Fatal(err)
			}
			deadline := time.Now().Add(10 * time.Second)
			observed := false
			for time.Now().Before(deadline) {
				var state string
				var attempt int
				if err := m.f.owner.QueryRow(ctx, `SELECT state,attempt FROM river_meta.river_job WHERE id=$1`, e.job).Scan(&state, &attempt); err != nil {
					t.Fatal(err)
				}
				if state == "retryable" && attempt >= 1 {
					observed = true
					break
				}
				if state == "completed" || state == "cancelled" || state == "discarded" {
					t.Fatalf("crypto failure completed/cancelled River job: %s", state)
				}
				time.Sleep(20 * time.Millisecond)
			}
			stop, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			err = client.StopAndCancel(stop)
			cancel()
			if err != nil {
				t.Fatal(err)
			}
			if !observed {
				t.Fatal("crypto failure did not produce retryable River attempt")
			}
			mustExec(t, m.f.owner, `UPDATE meta_private.event_bodies SET expires_at=clock_timestamp()-interval '1 hour' WHERE event_id=$1`, e.id)
			var purged int
			if err := m.curator.QueryRow(ctx, `SELECT meta_inbox.purge_expired(10)`).Scan(&purged); err != nil {
				t.Fatal(err)
			}
			if miCount(t, m.f.owner, `SELECT count(*) FROM meta_private.event_bodies WHERE event_id=$1`, e.id) != 1 || miCount(t, m.f.owner, `SELECT count(*) FROM social.messages WHERE event_id=$1`, e.id) != 0 || miCount(t, m.f.owner, `SELECT count(*) FROM meta_inbox.events WHERE id=$1 AND terminal_reason IS NOT NULL`, e.id) != 0 {
				t.Fatal("failed crypto consumed/purged pending source")
			}
		})
	}
}
