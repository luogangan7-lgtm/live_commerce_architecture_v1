//go:build browser

package foundation_test

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"livecommerce/internal/integrations/livekit"
	"livecommerce/internal/live"
)

func studioProcessFixture(t *testing.T) (*lmeHarness, string, *atomic.Bool) {
	t.Helper()
	h := &lmeHarness{lmpHarness: &lmpHarness{lmaHarness: lmaSetup(t)}}
	var stopsAllowed atomic.Bool
	server, ca := lmwTLS(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.TLS == nil || r.Host != "unit.livekit.cloud" {
			t.Error("unverified provider host")
		}
		switch r.URL.Path {
		case "/twirp/livekit.Egress/StartEgress":
			h.starts.Add(1)
			lmeReply(w, h.observation("EG_studio", "EGRESS_ACTIVE", 100, 110, 0))
		case "/twirp/livekit.Egress/ListEgress":
			h.lists.Add(1)
			lmeReply(w, `{"items":[`+h.observation("EG_studio", "EGRESS_ACTIVE", 100, 120, 0)+`]}`)
		case "/twirp/livekit.Egress/StopEgress":
			if !stopsAllowed.Load() {
				t.Error("provider Stop before authorized HTTP request")
			}
			h.stops.Add(1)
			lmeReply(w, h.observation("EG_studio", "EGRESS_COMPLETE", 100, 150, 140))
		default:
			t.Errorf("unexpected provider path %q", r.URL.Path)
			http.Error(w, "unknown", 500)
		}
	}))
	h.server = server
	// The task-owned CA and hostname are used even for material sealing; there
	// is no test-only SkipVerify path in this process chain.
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM([]byte(ca)) {
		t.Fatal("invalid fixture CA")
	}
	transport := &http.Transport{Proxy: nil, DisableKeepAlives: true, TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: roots},
		DialContext: func(ctx context.Context, _, target string) (net.Conn, error) {
			if target != "unit.livekit.cloud:443" {
				return nil, livekit.ErrInvalid
			}
			return (&net.Dialer{}).DialContext(ctx, "tcp", server.Listener.Addr().String())
		}}
	client, err := livekit.New(lmeConfig(), transport)
	if err != nil {
		t.Fatal(err)
	}
	h.keys, err = lmrKeys()
	if err != nil {
		t.Fatal(err)
	}
	h.specification = h.spec()
	h.streamURL = "rtmps://ingest.example.com/live/studio-secret-" + t04Tag()
	attemptID := h.specification["attempt_id"].(string)
	scope := livekit.MaterialScope{TenantID: h.lp.f.tenantA, StoreID: h.lp.f.storeA1, SessionID: h.session,
		AttemptID: attemptID, ProjectID: "project_lma", CredentialVersion: 1, MaterialVersion: 1}
	sealed, err := h.keys.Seal(scope, client, livekit.StartInput{RoomName: "lc_" + strings.ReplaceAll(attemptID, "-", ""), AspectRatio: "16:9", StreamURLs: []string{h.streamURL}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := lmaRegister(context.Background(), h.registrar, h.specification, sealed.Nonce, sealed.Ciphertext); err != nil {
		t.Fatal(err)
	}
	h.input = live.MediaStartInput{SessionID: h.session, AuthorizationID: h.specification["id"].(string), ExpectedSessionVersion: 1}
	h.planner = lmpPlanner(t, h.lp.f.runtime, "river_media")
	h.workerLogin, h.worker = lmaLogin(t, h.lp.f, "commerce_media_worker")
	h.executorLogin, h.executor = lmaLogin(t, h.lp.f, "commerce_media_executor")
	for _, item := range []struct{ role, login string }{{"commerce_media_worker", h.workerLogin}, {"commerce_media_executor", h.executorLogin}} {
		name := pgx.Identifier{item.login}.Sanitize()
		mustExec(t, h.lp.f.owner, "REVOKE "+item.role+" FROM "+name)
		mustExec(t, h.lp.f.owner, "GRANT "+item.role+" TO "+name+" WITH INHERIT TRUE, SET FALSE")
	}
	t.Cleanup(func() {
		if h.plan.AttemptID == "" {
			return
		}
		ctx := context.Background()
		tx, err := h.lp.f.owner.Begin(ctx)
		if err != nil {
			t.Error(err)
			return
		}
		defer tx.Rollback(ctx)
		if _, err := tx.Exec(ctx, `SET CONSTRAINTS ALL DEFERRED`); err != nil {
			t.Error(err)
			return
		}
		for _, item := range []struct {
			query string
			arg   any
		}{
			{`DELETE FROM live.media_observations WHERE attempt_id=$1`, h.plan.AttemptID},
			{`DELETE FROM live.media_execution_state WHERE attempt_id=$1`, h.plan.AttemptID},
			{`DELETE FROM river_media.river_job WHERE id=$1`, h.plan.JobID},
			{`DELETE FROM integration.operation_events WHERE operation_id=$1`, h.plan.OperationID},
			{`DELETE FROM integration.operations WHERE id=$1`, h.plan.OperationID},
			{`DELETE FROM live.media_attempts WHERE id=$1`, h.plan.AttemptID},
			{`DELETE FROM ops.command_results WHERE principal_id=$1 AND operation IN ('live.media.start','live.media.stop')`, h.lp.actor},
			{`DELETE FROM ops.audit_events WHERE principal_id=$1 AND action IN ('live.media.start.planned','live.media.stop.requested')`, h.lp.actor},
		} {
			if _, err := tx.Exec(ctx, item.query, item.arg); err != nil {
				t.Errorf("Studio chain cleanup: %v", err)
				return
			}
		}
		if err := tx.Commit(ctx); err != nil {
			t.Error(err)
		}
	})
	return h, ca, &stopsAllowed
}

func TestStudioBackendSTU03RealAPIWorkerRestart(t *testing.T) {
	// All databases, roles, ports, certificates and the IdP are disposable local fixtures.
	h, ca, stopsAllowed := studioProcessFixture(t)
	_, _, authority := identityFixture(t)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := listener.Addr().String()
	_ = listener.Close()
	origin := "http://" + addr
	idp := newBrowserIDP(t, origin+"/api/auth/callback")
	apiBinary := mrBuild(t, "../../cmd/api", "studio-api")
	workerBinary := mrBuild(t, "../../cmd/media-worker", "studio-media-worker")
	apiName := "studio_api_" + t04Tag()
	apiDSN := mrNamedDSN(t, h.lp.f.runtime.Config().ConnString(), apiName)
	apiEnv := []string{
		"LISTEN_ADDR=" + addr, "DATABASE_URL=" + apiDSN,
		"COMMERCE_STUDIO_ENABLED=1", "COMMERCE_STUDIO_MEDIA_ENABLED=1", "COMMERCE_IDENTITY_ENABLED=1", "COMMERCE_IDENTITY_ALLOW_LOOPBACK_TESTS=1",
		"COMMERCE_PUBLIC_ORIGIN=" + origin, "COMMERCE_IDENTITY_DATABASE_URL=" + authority.Config().ConnString(),
		"COMMERCE_BFF_KEY=" + randomToken(), "COMMERCE_OIDC_ISSUER=" + idp.server.URL,
		"COMMERCE_OIDC_CLIENT_ID=" + browserClientID, "COMMERCE_IDENTITY_PROVIDER_KEY=studio-process-mock-v1",
		"COMMERCE_SESSION_TTL=1h", "COMMERCE_ONBOARDING_ENABLED=0",
	}
	client := &http.Client{Timeout: 5 * time.Second}
	ready := func(p *mrProcess) {
		t.Helper()
		deadline := time.Now().Add(12 * time.Second)
		for time.Now().Before(deadline) {
			select {
			case err := <-p.done:
				p.exited = true
				t.Fatalf("API exited before readiness: %v log=%s", err, p.logPath)
			default:
			}
			resp, err := client.Get(origin + "/healthz")
			if err == nil {
				_ = resp.Body.Close()
				if resp.StatusCode == 200 {
					return
				}
			}
			time.Sleep(25 * time.Millisecond)
		}
		t.Fatalf("API not ready: %s", p.logPath)
	}
	request := func(method, path, key, body string, want int) map[string]any {
		t.Helper()
		var reader io.Reader
		if body != "" {
			reader = strings.NewReader(body)
		}
		r, err := http.NewRequest(method, origin+path, reader)
		if err != nil {
			t.Fatal(err)
		}
		r.Header.Set("Authorization", "Bearer "+h.lp.token)
		if body != "" {
			r.Header.Set("Content-Type", "application/json")
		}
		if key != "" {
			r.Header.Set("Idempotency-Key", key)
		}
		resp, err := client.Do(r)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		if err != nil {
			t.Fatal(err)
		}
		if resp.StatusCode != want || !strings.Contains(resp.Header.Get("Cache-Control"), "no-store") || bytes.Contains(raw, []byte(h.streamURL)) {
			t.Fatalf("API %s %s status=%d cache=%q body=%s", method, path, resp.StatusCode, resp.Header.Get("Cache-Control"), raw)
		}
		var out map[string]any
		if err := json.Unmarshal(raw, &out); err != nil {
			t.Fatal(err)
		}
		return out
	}
	base := "/v1/admin/stores/" + h.lp.f.storeA1 + "/live-sessions/" + h.session
	// The default-off binary has no Studio routes and must not require identity.
	off := mrLaunch(t, apiBinary, "studio-api-off", []string{"LISTEN_ADDR=" + addr, "DATABASE_URL=" + apiDSN, "COMMERCE_STUDIO_ENABLED=0"})
	ready(off)
	request("GET", base, "", "", 404)
	mrStop(t, off, syscall.SIGTERM, true)
	waitPoolsGone(t, h.lp.f, "default-off API pool remained", apiName)
	api := mrLaunch(t, apiBinary, "studio-api-enabled", apiEnv)
	ready(api)
	prepared := request("GET", base, "", "", 200)
	if prepared["prepared"] == nil || prepared["attempt"] != nil {
		t.Fatalf("API candidate missing: %v", prepared)
	}
	startKey := t04Key("studio-process-start")
	startPath := base + "/rehearsal/start"
	startBody := `{"authorization_id":"` + h.input.AuthorizationID + `","expected_session_version":1}`
	started := request("POST", startPath, startKey, startBody, 200)
	studioKeys(t, started, "session_id", "attempt_id", "state")
	if started["attempt_id"] != h.specification["attempt_id"] {
		t.Fatalf("wrong attempt: %v", started)
	}
	if replay := request("POST", startPath, startKey, startBody, 200); replay["attempt_id"] != started["attempt_id"] {
		t.Fatalf("start replay changed attempt: %v", replay)
	}
	h.plan.AttemptID = started["attempt_id"].(string)
	if err := h.lp.f.owner.QueryRow(context.Background(), `SELECT a.start_operation_id::text,o.job_id,a.room_name,a.program_id::text FROM live.media_attempts a JOIN integration.operations o ON o.id=a.start_operation_id WHERE a.id=$1`, h.plan.AttemptID).
		Scan(&h.plan.OperationID, &h.plan.JobID, &h.plan.RoomName, &h.plan.ProgramID); err != nil {
		t.Fatal(err)
	}
	h.plan.SessionID = h.session
	workerName, executorName := "studio_worker_"+t04Tag(), "studio_executor_"+t04Tag()
	workerEnv := lmwEnvironment(h, h.server.Listener.Addr().String(), ca)
	workerEnv = lmwReplace(lmwReplace(workerEnv, "COMMERCE_MEDIA_WORKER_DATABASE_URL", mrNamedDSN(t, h.worker.Config().ConnString(), workerName)), "COMMERCE_MEDIA_EXECUTOR_DATABASE_URL", mrNamedDSN(t, h.executor.Config().ConnString(), executorName))
	worker := lmwProcess(t, workerBinary, "studio-start", workerEnv)
	mrReadyLog(t, worker, "media_worker_ready")
	facts := h.await(t, 20*time.Second, func(f lmeFacts) bool { return f.observations >= 1 && f.egress == "EG_studio" })
	if facts.operation != "UNKNOWN" || facts.resource != "OBSERVED" || h.starts.Load() != 1 || h.stops.Load() != 0 {
		t.Fatalf("unsafe start: %+v starts=%d", facts, h.starts.Load())
	}
	seen := request("GET", base, "", "", 200)
	attempt := seen["attempt"].(map[string]any)
	studioKeys(t, attempt, "attempt_id", "environment", "operation_state", "resource_state", "transport_status", "cleanup_required", "stop_requested", "stop_wire_count", "escalated", "updated_at", "destinations")
	if seen["prepared"] != nil || attempt["attempt_id"] != h.plan.AttemptID || attempt["operation_state"] != "UNKNOWN" {
		t.Fatalf("API claimed false readiness: %v", seen)
	}
	mrStop(t, worker, syscall.SIGTERM, true)
	mrStop(t, api, syscall.SIGTERM, true)
	waitPoolsGone(t, h.lp.f, "shutdown leaked task-owned pools", apiName, workerName, executorName)
	api = mrLaunch(t, apiBinary, "studio-api-reloaded", apiEnv)
	ready(api)
	if reopened := request("GET", base, "", "", 200); reopened["attempt"].(map[string]any)["attempt_id"] != h.plan.AttemptID {
		t.Fatalf("reload lost attempt: %v", reopened)
	}
	stopsAllowed.Store(true)
	stopPath := base + "/rehearsal/stop"
	stopped := request("POST", stopPath, t04Key("studio-process-stop"), `{"attempt_id":"`+h.plan.AttemptID+`"}`, 200)
	studioKeys(t, stopped, "session_id", "attempt_id", "state")
	if stopped["attempt_id"] != h.plan.AttemptID {
		t.Fatalf("Stop targeted wrong attempt: %v", stopped)
	}
	worker = lmwProcess(t, workerBinary, "studio-stop", workerEnv)
	mrReadyLog(t, worker, "media_worker_ready")
	deadline := time.Now().Add(25 * time.Second)
	for time.Now().Before(deadline) {
		if state := lmrRead(t, h); state.resource == "TERMINAL" {
			break
		}
		time.Sleep(25 * time.Millisecond)
	}
	final := lmrRead(t, h)
	if final.resource != "TERMINAL" || final.count != 1 || h.starts.Load() != 1 || h.stops.Load() != 1 {
		t.Fatalf("Stop budget or duplicate Start: %+v starts=%d stops=%d", final, h.starts.Load(), h.stops.Load())
	}
	projected := request("GET", base, "", "", 200)
	terminal := projected["attempt"].(map[string]any)
	if terminal["attempt_id"] != h.plan.AttemptID || terminal["resource_state"] != "TERMINAL" || terminal["stop_wire_count"] != float64(1) {
		t.Fatalf("terminal projection: %v", projected)
	}
	for _, destination := range terminal["destinations"].([]any) {
		studioKeys(t, destination.(map[string]any), "ordinal", "provider")
	}
	mrStop(t, worker, syscall.SIGINT, true)
	mrStop(t, api, syscall.SIGTERM, true)
	waitPoolsGone(t, h.lp.f, "final shutdown leaked pools", apiName, workerName, executorName)
	for _, p := range []*mrProcess{off, api, worker} {
		log, err := os.ReadFile(p.logPath)
		if err != nil {
			t.Fatal(err)
		}
		for _, secret := range []string{h.lp.token, h.streamURL, h.lp.f.runtime.Config().ConnString()} {
			if bytes.Contains(log, []byte(secret)) {
				t.Fatalf("secret logged: %s", p.logPath)
			}
		}
	}
}
