package foundation_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"testing"
	"time"
)

type mrProcess struct {
	cmd     *exec.Cmd
	done    chan error
	logPath string
	exited  bool
}

func mrBuild(t *testing.T, packagePath, name string) string {
	t.Helper()
	binary := filepath.Join(t.TempDir(), name)
	cmd := exec.Command("go", "build", "-o", binary, packagePath)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build %s: %v %s", name, err, out)
	}
	return binary
}

func mrLaunch(t *testing.T, binary, name string, env []string) *mrProcess {
	t.Helper()
	path := filepath.Join(t.TempDir(), name+".log")
	logFile, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(binary)
	cmd.Env = append([]string{"PATH=" + os.Getenv("PATH")}, env...)
	cmd.Stdout, cmd.Stderr = logFile, logFile
	if err := cmd.Start(); err != nil {
		_ = logFile.Close()
		t.Fatal(err)
	}
	p := &mrProcess{cmd: cmd, done: make(chan error, 1), logPath: path}
	go func() {
		p.done <- cmd.Wait()
		_ = logFile.Close()
	}()
	t.Cleanup(func() {
		if !p.exited {
			_ = p.cmd.Process.Kill()
			select {
			case <-p.done:
			case <-time.After(5 * time.Second):
				t.Errorf("process PID %d did not exit on fixture cleanup", p.cmd.Process.Pid)
			}
			p.exited = true
		}
		if t.Failed() {
			log, err := os.ReadFile(p.logPath)
			if err != nil {
				t.Errorf("preserve %s log: %v", name, err)
				return
			}
			// Keep failure logs under the gitignored repo output/playwright (the
			// package runs from tests/foundation); no workstation-only path.
			dir, _ := filepath.Abs("../../output/playwright")
			preserved := filepath.Join(dir, "meta-runtime-process-"+name+"-"+t04Tag()+".log")
			if err := os.MkdirAll(dir, 0700); err != nil {
				t.Errorf("preserve %s log: %v", name, err)
			} else if err := os.WriteFile(preserved, log, 0600); err != nil {
				t.Errorf("preserve %s log: %v", name, err)
			} else {
				t.Logf("preserved process log: %s", preserved)
			}
		}
	})
	return p
}

func mrStop(t *testing.T, p *mrProcess, signal syscall.Signal, wantSuccess bool) {
	t.Helper()
	if err := p.cmd.Process.Signal(signal); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-p.done:
		p.exited = true
		if (err == nil) != wantSuccess {
			t.Fatalf("process PID %d signal %v exit=%v log=%s", p.cmd.Process.Pid, signal, err, p.logPath)
		}
	case <-time.After(8 * time.Second):
		t.Fatalf("process PID %d ignored %v log=%s", p.cmd.Process.Pid, signal, p.logPath)
	}
}

func mrReadyLog(t *testing.T, p *mrProcess, marker string) {
	t.Helper()
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case err := <-p.done:
			p.exited = true
			t.Fatalf("process exited before %s: %v log=%s", marker, err, p.logPath)
		default:
		}
		log, err := os.ReadFile(p.logPath)
		if err != nil {
			t.Fatal(err)
		}
		if bytes.Contains(log, []byte(marker)) {
			return
		}
		time.Sleep(20 * time.Millisecond) // readiness polling only, not causal ordering proof
	}
	t.Fatalf("missing %s marker: %s", marker, p.logPath)
}

func mrNamedDSN(t *testing.T, dsn, name string) string {
	t.Helper()
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatal(err)
	}
	q := u.Query()
	q.Set("application_name", name)
	u.RawQuery = q.Encode()
	return u.String()
}

// mrPoolCount is a single snapshot for running processes and positive
// controls. After the owning process exited or the pool closed, assert zero
// with waitPoolsGone (pg_teardown_test.go): backend exit is asynchronous.
func mrPoolCount(t *testing.T, f *testFixture, names ...string) int64 {
	t.Helper()
	return miCount(t, f.owner, `SELECT count(*) FROM pg_stat_activity WHERE datname='lc_foundation_test' AND application_name=ANY($1::text[])`, names)
}

func mrHTTP(t *testing.T, client *http.Client, method, target string, raw []byte, signature string) (int, string) {
	t.Helper()
	req, err := http.NewRequest(method, target, bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	if method == http.MethodPost {
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Hub-Signature-256", signature)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 512))
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, string(data)
}

func mrStoredEvent(t *testing.T, m miTest, raw []byte) mcEvent {
	t.Helper()
	batch, err := m.verifier.Verify(raw, miSignature(raw))
	if err != nil || len(batch.Events) != 1 {
		t.Fatalf("invalid signed fixture: %v", err)
	}
	e := mcEvent{key: batch.Events[0].Key, hash: batch.Events[0].PayloadHash, object: batch.Object, asset: batch.Events[0].AssetID, app: batch.AppID, plain: batch.Events[0].Payload}
	if err := m.f.owner.QueryRow(context.Background(), `SELECT id::text,route_id::text,route_epoch,job_id FROM meta_inbox.events WHERE app_id=$1 AND object=$2 AND event_key=$3 AND is_primary`, e.app, e.object, e.key).Scan(&e.id, &e.route, &e.epoch, &e.job); err != nil {
		t.Fatal("signed API receipt missing", err)
	}
	return e
}

func mrLogNoSecrets(t *testing.T, p *mrProcess, forbidden ...string) {
	t.Helper()
	log, err := os.ReadFile(p.logPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range forbidden {
		if secret != "" && bytes.Contains(log, []byte(secret)) {
			t.Fatalf("process log exposed secret: %s", p.logPath)
		}
	}
}

func mrFailsBeforeReady(t *testing.T, p *mrProcess, marker string, forbidden ...string) {
	t.Helper()
	select {
	case err := <-p.done:
		p.exited = true
		if err == nil {
			t.Fatalf("invalid process exited successfully: %s", p.logPath)
		}
	case <-time.After(14 * time.Second):
		t.Fatalf("invalid process did not fail within startup/rollback envelope: %s", p.logPath)
	}
	log, err := os.ReadFile(p.logPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(log, []byte(marker)) || bytes.Contains(log, []byte("meta_worker_ready")) {
		t.Fatalf("invalid process used wrong result code or reached worker readiness: %s", p.logPath)
	}
	mrLogNoSecrets(t, p, forbidden...)
}

func TestMetaRuntimeBinaryPreflightFailureClosesPools(t *testing.T) {
	f := mrFixture(t)
	clone := mrFixture(t) // independent PG with the same database name and schema
	apiBinary := mrBuild(t, "../../cmd/api", "meta-api-preflight")
	workerBinary := mrBuild(t, "../../cmd/meta-worker", "meta-worker-preflight")
	keyJSON := fmt.Sprintf(`{"keys":[{"id":%q,"key_base64":%q}]}`, miKeyID, base64.StdEncoding.EncodeToString(randomBytes(32)))
	appsJSON := fmt.Sprintf(`{"apps":[{"app_id":%q,"object":"page","app_secret":%q,"verify_token":"meta-inbox-verify-token"}]}`, miApp, miSecret)
	mainName, ingressName := "mr_bad_main_"+t04Tag(), "mr_bad_ingress_"+t04Tag()
	workerName, consumerName := "mr_bad_worker_"+t04Tag(), "mr_bad_consumer_"+t04Tag()
	mainDSN := mrNamedDSN(t, f.runtime.Config().ConnString(), mainName)
	otherIngress := mrNamedDSN(t, miRole(t, clone, "commerce_meta_ingress"), ingressName)
	workerDSN := mrNamedDSN(t, miRole(t, f, "commerce_meta_worker"), workerName)
	otherConsumer := mrNamedDSN(t, miRole(t, clone, "commerce_meta_consumer"), consumerName)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := listener.Addr().String()
	_ = listener.Close()
	apiEnv := []string{"LISTEN_ADDR=" + addr, "DATABASE_URL=" + mainDSN, "COMMERCE_META_WEBHOOK_ENABLED=1", "COMMERCE_META_INGRESS_DATABASE_URL=" + otherIngress, "COMMERCE_META_APPS_JSON=" + appsJSON, "COMMERCE_META_PAYLOAD_ACTIVE_KEY_ID=" + miKeyID, "COMMERCE_META_PAYLOAD_KEYS_JSON=" + keyJSON}
	api := mrLaunch(t, apiBinary, "api-clone-reject", apiEnv)
	mrFailsBeforeReady(t, api, "api stopped", mainDSN, otherIngress, keyJSON, appsJSON, miSecret)
	waitPoolsGone(t, f, "split-DB API assembly leaked opened pools", mainName)
	waitPoolsGone(t, clone, "split-DB API assembly leaked opened pools", ingressName)
	client := &http.Client{Timeout: time.Second}
	if resp, err := client.Get("http://" + addr + "/healthz"); err == nil {
		_ = resp.Body.Close()
		t.Fatal("split-DB API listened before preflight")
	}
	workerEnv := []string{"COMMERCE_META_WORKER_ENABLED=1", "COMMERCE_META_WORKER_DATABASE_URL=" + workerDSN, "COMMERCE_META_CONSUMER_DATABASE_URL=" + otherConsumer, "COMMERCE_META_WORKER_CONCURRENCY=1", "COMMERCE_META_PAYLOAD_ACTIVE_KEY_ID=" + miKeyID, "COMMERCE_META_PAYLOAD_KEYS_JSON=" + keyJSON}
	worker := mrLaunch(t, workerBinary, "worker-clone-reject", workerEnv)
	mrFailsBeforeReady(t, worker, "meta_worker_database_unavailable", workerDSN, otherConsumer, keyJSON)
	waitPoolsGone(t, f, "split-DB worker leaked pools or started queue", workerName)
	waitPoolsGone(t, clone, "split-DB worker leaked pools or started queue", consumerName)
	if miCount(t, f.owner, `SELECT count(*) FROM river_meta.river_queue WHERE name='meta_inbox'`) != 0 {
		t.Fatal("split-DB worker leaked pools or started queue")
	}
	// A wrong-role second pool exercises cleanup after the first pool opened.
	wrongConsumer := mrNamedDSN(t, miRole(t, f, "commerce_meta_ingress"), consumerName+"_wrong")
	partialEnv := append([]string(nil), workerEnv...)
	for i, entry := range partialEnv {
		if strings.HasPrefix(entry, "COMMERCE_META_CONSUMER_DATABASE_URL=") {
			partialEnv[i] = "COMMERCE_META_CONSUMER_DATABASE_URL=" + wrongConsumer
		}
	}
	partial := mrLaunch(t, workerBinary, "worker-partial-reject", partialEnv)
	mrFailsBeforeReady(t, partial, "meta_worker_database_unavailable", workerDSN, wrongConsumer, keyJSON)
	waitPoolsGone(t, f, "partial worker assembly left pool connections", workerName, consumerName+"_wrong")
	disabled := exec.Command(workerBinary)
	disabled.Env = []string{"PATH=" + os.Getenv("PATH"), "COMMERCE_META_WORKER_ENABLED=0", "COMMERCE_META_WORKER_DATABASE_URL=invalid", "COMMERCE_META_CONSUMER_DATABASE_URL=invalid", "COMMERCE_META_PAYLOAD_KEYS_JSON=invalid"}
	if out, err := disabled.CombinedOutput(); err != nil || len(out) != 0 {
		t.Fatalf("disabled binary opened resources or read disabled secrets: %v %q", err, out)
	}
}

func TestMetaRuntimeRealAPIBinariesPageInstagramRestart(t *testing.T) {
	f := mrFixture(t)
	m := mrSetup(t, f)
	ctx := context.Background()
	pageAsset, igAsset := miAsset(), miAsset()
	pageBinding := miBinding(t, m, pageAsset, "facebook", f.tenantA, f.storeA1, f.principalA)
	miRoute(t, m, pageAsset, f.tenantA, f.storeA1, pageBinding)
	ig := miInstagram(t, m)
	igBinding := miBinding(t, ig, igAsset, "instagram", f.tenantA, f.storeA1, f.principalA)
	miInstagramRoute(t, ig, igAsset, igBinding)
	apiBinary := mrBuild(t, "../../cmd/api", "meta-api")
	workerBinary := mrBuild(t, "../../cmd/meta-worker", "meta-worker")
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := listener.Addr().String()
	_ = listener.Close()
	origin := "http://" + addr
	keyJSON := fmt.Sprintf(`{"keys":[{"id":%q,"key_base64":%q}]}`, miKeyID, base64.StdEncoding.EncodeToString(m.key))
	appsJSON := fmt.Sprintf(`{"apps":[{"app_id":%q,"object":"page","app_secret":%q,"verify_token":"meta-inbox-verify-token"},{"app_id":%q,"object":"instagram","app_secret":%q,"verify_token":"meta-inbox-verify-token"}]}`, miApp, miSecret, miApp, miSecret)
	apiMainName, apiIngressName := "mr_api_main_"+t04Tag(), "mr_api_ingress_"+t04Tag()
	workerName, consumerName := "mr_worker_"+t04Tag(), "mr_consumer_"+t04Tag()
	mainDSN := mrNamedDSN(t, f.runtime.Config().ConnString(), apiMainName)
	ingressDSN := mrNamedDSN(t, miRole(t, f, "commerce_meta_ingress"), apiIngressName)
	workerDSN := mrNamedDSN(t, miRole(t, f, "commerce_meta_worker"), workerName)
	consumerDSN := mrNamedDSN(t, miRole(t, f, "commerce_meta_consumer"), consumerName)
	apiEnv := []string{"LISTEN_ADDR=" + addr, "DATABASE_URL=" + mainDSN, "COMMERCE_META_WEBHOOK_ENABLED=1", "COMMERCE_META_INGRESS_DATABASE_URL=" + ingressDSN, "COMMERCE_META_APPS_JSON=" + appsJSON, "COMMERCE_META_PAYLOAD_ACTIVE_KEY_ID=" + miKeyID, "COMMERCE_META_PAYLOAD_KEYS_JSON=" + keyJSON}
	workerEnv := []string{"COMMERCE_META_WORKER_ENABLED=1", "COMMERCE_META_WORKER_DATABASE_URL=" + workerDSN, "COMMERCE_META_CONSUMER_DATABASE_URL=" + consumerDSN, "COMMERCE_META_WORKER_CONCURRENCY=1", "COMMERCE_META_PAYLOAD_ACTIVE_KEY_ID=" + miKeyID, "COMMERCE_META_PAYLOAD_KEYS_JSON=" + keyJSON}
	api := mrLaunch(t, apiBinary, "api", apiEnv)
	client := &http.Client{Timeout: 5 * time.Second, CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }}
	deadline := time.Now().Add(8 * time.Second)
	for {
		select {
		case err := <-api.done:
			api.exited = true
			t.Fatalf("API exited before healthz: %v log=%s", err, api.logPath)
		default:
		}
		resp, err := client.Get(origin + "/healthz")
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				break
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("API healthz timeout: %s", api.logPath)
		}
		time.Sleep(20 * time.Millisecond)
	}
	if mrPoolCount(t, f, apiMainName, apiIngressName) < 2 {
		t.Fatal("API did not hold separate main and ingress pools")
	}
	pagePath := origin + "/v1/meta/webhooks/" + miApp + "/page"
	igPath := origin + "/v1/meta/webhooks/" + miApp + "/instagram"
	if status, body := mrHTTP(t, client, http.MethodGet, pagePath+"?hub.mode=subscribe&hub.verify_token=meta-inbox-verify-token&hub.challenge=challenge123", nil, ""); status != 200 || body != "challenge123" {
		t.Fatalf("real API verification status=%d body=%q", status, body)
	}
	pageMessage := miMessage(pageAsset, "m."+randomUUID(), "runtime-page-message")
	igMessage := []byte(fmt.Sprintf(`{"object":"instagram","entry":[{"id":%q,"messaging":[{"sender":{"id":"4"},"recipient":{"id":%q},"message":{"mid":%q,"text":"runtime-ig-message"}}]}]}`, igAsset, igAsset, "m."+randomUUID()))
	pageComment := []byte(fmt.Sprintf(`{"object":"page","entry":[{"id":%q,"changes":[{"field":"feed","value":{"item":"comment","verb":"add","comment_id":%q,"message":"runtime-page-comment"}}]}]}`, pageAsset, miAsset()))
	igComment := []byte(fmt.Sprintf(`{"object":"instagram","entry":[{"id":%q,"changes":[{"field":"comments","value":{"id":%q,"text":"runtime-ig-comment"}}]}]}`, igAsset, miAsset()))
	for _, item := range []struct {
		target string
		raw    []byte
	}{{pagePath, pageMessage}, {igPath, igMessage}, {pagePath, pageComment}, {igPath, igComment}} {
		if status, body := mrHTTP(t, client, http.MethodPost, item.target, item.raw, miSignature(item.raw)); status != 200 || body != "EVENT_RECEIVED" {
			t.Fatalf("signed API admission status=%d body=%q", status, body)
		}
	}
	if status, body := mrHTTP(t, client, http.MethodPost, pagePath, pageMessage, miSignature(pageMessage)); status != 200 || body != "EVENT_RECEIVED" {
		t.Fatalf("duplicate API delivery status=%d body=%q", status, body)
	}
	unknown := miMessage(miAsset(), "m."+randomUUID(), "unknown-asset")
	if status, body := mrHTTP(t, client, http.MethodPost, pagePath, unknown, miSignature(unknown)); status != 200 || body != "EVENT_RECEIVED" {
		t.Fatalf("unknown asset quarantine status=%d body=%q", status, body)
	}
	var events []mcEvent
	for _, item := range []struct {
		local miTest
		raw   []byte
	}{{m, pageMessage}, {ig, igMessage}, {m, pageComment}, {ig, igComment}} {
		events = append(events, mrStoredEvent(t, item.local, item.raw))
	}
	beforeRejected := miCount(t, f.owner, `SELECT count(*) FROM meta_inbox.events`)
	badRaw := miMessage(pageAsset, "m."+randomUUID(), "must-not-admit")
	for _, reject := range []struct {
		method, target, signature string
		want                      int
	}{
		{http.MethodPost, pagePath, "sha256=" + strings.Repeat("0", 64), http.StatusForbidden},
		{http.MethodPost, origin + "/v1/meta/webhooks/" + miApp + "/unknown", miSignature(badRaw), http.StatusNotFound},
		{http.MethodPut, pagePath, miSignature(badRaw), http.StatusMethodNotAllowed},
		{http.MethodPost, origin + "/v1/meta/webhooks/" + miApp + "/page/", miSignature(badRaw), http.StatusNotFound},
	} {
		status, _ := mrHTTP(t, client, reject.method, reject.target, badRaw, reject.signature)
		if status != reject.want {
			t.Fatalf("API negative method/path/signature status=%d want=%d", status, reject.want)
		}
	}
	for _, alias := range []string{
		origin + "/v1/meta/webhooks/" + miApp + "/%70age",
		origin + "/v1//meta/webhooks/" + miApp + "/page",
		origin + "/x/../v1/meta/webhooks/" + miApp + "/page",
	} {
		req, err := http.NewRequest(http.MethodPost, alias, bytes.NewReader(badRaw))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Hub-Signature-256", miSignature(badRaw))
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound || resp.Header.Get("Location") != "" {
			t.Fatalf("alias path accepted or redirected: status=%d location=%q", resp.StatusCode, resp.Header.Get("Location"))
		}
	}
	if miCount(t, f.owner, `SELECT count(*) FROM meta_inbox.events`) != beforeRejected {
		t.Fatal("rejected HTTP request admitted a receipt")
	}
	if miCount(t, f.owner, `SELECT count(*) FROM social.messages`) != 0 || miCount(t, f.owner, `SELECT count(*) FROM social.comment_events`) != 0 ||
		miCount(t, f.owner, `SELECT count(*) FROM river_meta.river_job WHERE kind='meta_inbox_v1' AND attempt=0 AND state='available'`) != 4 ||
		miCount(t, f.owner, `SELECT count(*) FROM meta_inbox.events WHERE disposition='QUARANTINED' AND job_id IS NULL`) != 1 {
		t.Fatal("API consumed a job, duplicated a fact, or routed an unknown asset")
	}
	expiry := ewSetup(t, f, 1)
	payment := pqSetupItemsOn(t, f, nil, false, 1)
	if pwQueueIn(t, f.owner, "river_expiry.river_job", expiry.hold.JobID) != "checkout_expiry_v1" || pwQueueIn(t, f.owner, "river_payment.river_job", payment.result.JobID) != "payment_mock_v1" {
		t.Fatal("unrelated producer jobs have wrong queues")
	}
	// Keep this legitimate payment job scheduled and make it due before the
	// Meta worker starts. Queue-scoped fetch must not hide cross-queue River
	// maintenance: a scheduled-to-available promotion is an MR04 violation.
	result, err := f.owner.Exec(ctx, `UPDATE river_payment.river_job SET scheduled_at=clock_timestamp()-interval '1 second' WHERE id=$1 AND state='scheduled'`, payment.result.JobID)
	if err != nil || result.RowsAffected() != 1 {
		t.Fatalf("prepare due payment job: affected=%d err=%v", result.RowsAffected(), err)
	}
	var defaultJob int64
	if err := f.owner.QueryRow(ctx, `INSERT INTO river.river_job(kind,args,queue,max_attempts) VALUES('mr_unrelated_v1','{}','default',2) RETURNING id`).Scan(&defaultJob); err != nil {
		t.Fatal(err)
	}
	unrelated := []struct {
		table  string
		id     int64
		before string
	}{
		{table: "river_expiry.river_job", id: expiry.hold.JobID},
		{table: "river_payment.river_job", id: payment.result.JobID},
		{table: "river.river_job", id: defaultJob},
	}
	for i := range unrelated {
		var before string
		if err := f.owner.QueryRow(ctx, `SELECT to_jsonb(j)::text FROM `+unrelated[i].table+` j WHERE id=$1`, unrelated[i].id).Scan(&before); err != nil {
			t.Fatal(err)
		}
		unrelated[i].before = before
	}
	worker := mrLaunch(t, workerBinary, "worker-correct", workerEnv)
	mrReadyLog(t, worker, "meta_worker_ready")
	for _, e := range events {
		mcAwait(t, m, e)
	}
	if miCount(t, f.owner, `SELECT count(*) FROM social.messages WHERE event_id=ANY($1::uuid[])`, []string{events[0].id, events[1].id}) != 2 ||
		miCount(t, f.owner, `SELECT count(*) FROM social.comment_events WHERE event_id=ANY($1::uuid[])`, []string{events[2].id, events[3].id}) != 2 ||
		miCount(t, f.owner, `SELECT count(*) FROM social.messages WHERE event_id=ANY($1::uuid[]) AND tenant_id=$2 AND store_id=$3`, []string{events[0].id, events[1].id}, f.tenantA, f.storeA1) != 2 ||
		miCount(t, f.owner, `SELECT count(*) FROM social.comment_events WHERE event_id=ANY($1::uuid[]) AND tenant_id=$2 AND store_id=$3`, []string{events[2].id, events[3].id}, f.tenantA, f.storeA1) != 2 ||
		miCount(t, f.owner, `SELECT count(*) FROM meta_inbox.audit_events WHERE event_id=ANY($1::uuid[]) AND action='processed'`, []string{events[0].id, events[1].id, events[2].id, events[3].id}) != 4 {
		t.Fatal("real worker did not materialize Page/IG messages and comments exactly once")
	}
	// Observe the actual unrelated row transition (or lack thereof) across
	// a bounded maintenance window, not a fixed sleep as causal evidence.
	unrelatedDeadline := time.Now().Add(12 * time.Second)
	for time.Now().Before(unrelatedDeadline) {
		var current string
		if err := f.owner.QueryRow(ctx, `SELECT to_jsonb(j)::text FROM river_payment.river_job j WHERE id=$1`, payment.result.JobID).Scan(&current); err != nil {
			t.Fatal(err)
		}
		if current != unrelated[1].before {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	for _, item := range unrelated {
		var after string
		if err := f.owner.QueryRow(ctx, `SELECT to_jsonb(j)::text FROM `+item.table+` j WHERE id=$1`, item.id).Scan(&after); err != nil || after != item.before {
			var oldFields, newFields map[string]json.RawMessage
			_ = json.Unmarshal([]byte(item.before), &oldFields)
			_ = json.Unmarshal([]byte(after), &newFields)
			changed := make([]string, 0)
			for key, oldValue := range oldFields {
				if !bytes.Equal(oldValue, newFields[key]) {
					changed = append(changed, key)
				}
			}
			sort.Strings(changed)
			t.Fatalf("Meta worker changed unrelated %s job id=%d: err=%v changed_fields=%v state_before=%s state_after=%s attempt_before=%s attempt_after=%s", item.table, item.id, err, changed, oldFields["state"], newFields["state"], oldFields["attempt"], newFields["attempt"])
		}
	}
	mrStop(t, worker, syscall.SIGTERM, true)
	waitPoolsGone(t, f, "SIGTERM worker left ordinary or consumer DB pool", workerName, consumerName)
	// The API keeps the old encryption key. Restart worker without that key:
	// the attempt may run, but no terminal/processed fact may be committed.
	pendingRaw := miMessage(pageAsset, "m."+randomUUID(), "pending-missing-key")
	if status, body := mrHTTP(t, client, http.MethodPost, pagePath, pendingRaw, miSignature(pendingRaw)); status != 200 || body != "EVENT_RECEIVED" {
		t.Fatalf("pending admission status=%d body=%q", status, body)
	}
	pending := mrStoredEvent(t, m, pendingRaw)
	wrongJSON := fmt.Sprintf(`{"keys":[{"id":"wrong_key","key_base64":%q}]}`, base64.StdEncoding.EncodeToString(randomBytes(32)))
	wrongEnv := append([]string(nil), workerEnv...)
	for i, entry := range wrongEnv {
		if strings.HasPrefix(entry, "COMMERCE_META_PAYLOAD_ACTIVE_KEY_ID=") {
			wrongEnv[i] = "COMMERCE_META_PAYLOAD_ACTIVE_KEY_ID=wrong_key"
		}
		if strings.HasPrefix(entry, "COMMERCE_META_PAYLOAD_KEYS_JSON=") {
			wrongEnv[i] = "COMMERCE_META_PAYLOAD_KEYS_JSON=" + wrongJSON
		}
	}
	missing := mrLaunch(t, workerBinary, "worker-missing-key", wrongEnv)
	mrReadyLog(t, missing, "meta_worker_ready")
	deadline = time.Now().Add(8 * time.Second)
	var state string
	var attempt int
	for time.Now().Before(deadline) {
		if err := f.owner.QueryRow(ctx, `SELECT state,attempt FROM river_meta.river_job WHERE id=$1`, pending.job).Scan(&state, &attempt); err != nil {
			t.Fatal(err)
		}
		if state == "retryable" && attempt > 0 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if state != "retryable" || attempt == 0 ||
		miCount(t, f.owner, `SELECT count(*) FROM social.messages WHERE event_id=$1`, pending.id) != 0 ||
		miCount(t, f.owner, `SELECT count(*) FROM meta_inbox.events WHERE id=$1 AND terminal_reason IS NOT NULL`, pending.id) != 0 ||
		miCount(t, f.owner, `SELECT count(*) FROM meta_private.event_bodies WHERE event_id=$1`, pending.id) != 1 {
		t.Fatal("missing key did not leave retryable encrypted pending source")
	}
	mrStop(t, missing, syscall.SIGTERM, true)
	waitPoolsGone(t, f, "missing-key worker leaked DB pools", workerName, consumerName)
	mustExec(t, f.owner, `UPDATE river_meta.river_job SET scheduled_at=clock_timestamp()-interval '1 second' WHERE id=$1 AND state IN ('retryable','available')`, pending.job)
	restarted := mrLaunch(t, workerBinary, "worker-restarted", workerEnv)
	mrReadyLog(t, restarted, "meta_worker_ready")
	mcAwait(t, m, pending)
	if miCount(t, f.owner, `SELECT count(*) FROM social.messages WHERE event_id=$1`, pending.id) != 1 ||
		miCount(t, f.owner, `SELECT count(*) FROM meta_inbox.audit_events WHERE event_id=$1 AND action='processed'`, pending.id) != 1 {
		t.Fatal("restarted worker did not process retained ciphertext once")
	}
	mrStop(t, restarted, syscall.SIGTERM, true)
	mrStop(t, api, syscall.SIGTERM, true)
	waitPoolsGone(t, f, "API/worker signal cleanup left PG connections", workerName, consumerName, apiMainName, apiIngressName)
	for _, p := range []*mrProcess{api, worker, missing, restarted} {
		mrLogNoSecrets(t, p, miSecret, miKeyID+`","key_base64"`, pageAsset, "runtime-page-message", keyJSON, appsJSON, ingressDSN, workerDSN, consumerDSN)
	}
}
