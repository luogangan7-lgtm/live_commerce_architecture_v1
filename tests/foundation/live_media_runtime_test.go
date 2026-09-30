package foundation_test

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"livecommerce/internal/platform"
)

// lmwTLS creates a task-owned CA and endpoint-name certificate. The command
// must perform ordinary chain and hostname verification against this root.
func lmwTLS(t *testing.T, handler http.Handler) (*httptest.Server, string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "unit.livekit.cloud"},
		DNSNames: []string{"unit.livekit.cloud"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true, IsCA: true}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	priv, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: priv})
	pair, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		t.Fatal(err)
	}
	s := httptest.NewUnstartedServer(handler)
	s.TLS = &tls.Config{MinVersion: tls.VersionTLS12, Certificates: []tls.Certificate{pair}}
	s.StartTLS()
	t.Cleanup(s.Close)
	return s, string(certPEM)
}

func lmwEnvironment(h *lmeHarness, dial, ca string) []string {
	key := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{0x37}, 32))
	projects, _ := json.Marshal(map[string]any{"projects": []any{map[string]any{
		"project_id": "project_lma", "credential_version": 1, "endpoint": "https://unit.livekit.cloud",
		"api_key": "lme_test_key", "api_secret": strings.Repeat("s", 40),
		"stream_hosts": []string{"ingest.example.com"}, "mock_dial_address": dial, "mock_ca_pem": ca,
	}}})
	keys, _ := json.Marshal(map[string]any{"keys": []any{map[string]any{"id": "lma_key_1", "key_base64": key}}})
	return []string{
		"COMMERCE_MEDIA_WORKER_ENABLED=1",
		"COMMERCE_MEDIA_WORKER_DATABASE_URL=" + h.worker.Config().ConnString(),
		"COMMERCE_MEDIA_EXECUTOR_DATABASE_URL=" + h.executor.Config().ConnString(),
		"COMMERCE_MEDIA_WORKER_CONCURRENCY=1",
		"COMMERCE_MEDIA_MATERIAL_ACTIVE_KEY_ID=lma_key_1",
		"COMMERCE_MEDIA_MATERIAL_KEYS_JSON=" + string(keys),
		"COMMERCE_MEDIA_PROJECTS_JSON=" + string(projects),
	}
}

func lmwReplace(env []string, key, value string) []string {
	out := append([]string(nil), env...)
	for i, entry := range out {
		if strings.HasPrefix(entry, key+"=") {
			out[i] = key + "=" + value
			return out
		}
	}
	return append(out, key+"="+value)
}

func lmwEnvValue(env []string, key string) string {
	for _, entry := range env {
		if strings.HasPrefix(entry, key+"=") {
			return strings.TrimPrefix(entry, key+"=")
		}
	}
	return ""
}

func lmwProcess(t *testing.T, binary, name string, env []string) *mrProcess {
	t.Helper()
	return mrLaunch(t, binary, name, append(env, "HTTPS_PROXY=http://127.0.0.1:1", "HTTP_PROXY=http://127.0.0.1:1"))
}

func lmwFail(t *testing.T, p *mrProcess, code string, forbidden ...string) {
	t.Helper()
	select {
	case err := <-p.done:
		p.exited = true
		if err == nil {
			t.Fatalf("invalid media worker exited zero: %s", p.logPath)
		}
	case <-time.After(14 * time.Second):
		t.Fatalf("media worker did not fail: %s", p.logPath)
	}
	log, err := os.ReadFile(p.logPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(log, []byte(code)) || bytes.Contains(log, []byte("media_worker_ready")) {
		t.Fatalf("wrong failure or false readiness: %s", p.logPath)
	}
	for _, secret := range forbidden {
		if secret != "" && bytes.Contains(log, []byte(secret)) {
			t.Fatalf("secret in log: %s", p.logPath)
		}
	}
}

func TestLiveMediaRuntimeLMW03CommandAuthorityAndPoolCleanup(t *testing.T) {
	h := lmrSetup(t, func(w http.ResponseWriter, _ *http.Request) { http.Error(w, "unused", 500) })
	tlsServer, ca := lmwTLS(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { http.Error(w, "unused", 500) }))
	binary := mrBuild(t, "../../cmd/media-worker", "media-worker-authority")
	base := lmwEnvironment(h, tlsServer.Listener.Addr().String(), ca)
	workerName, executorName := "lmw_worker_"+t04Tag(), "lmw_executor_"+t04Tag()
	workerDSN := mrNamedDSN(t, h.worker.Config().ConnString(), workerName)
	executorDSN := mrNamedDSN(t, h.executor.Config().ConnString(), executorName)
	base = lmwReplace(lmwReplace(base, "COMMERCE_MEDIA_WORKER_DATABASE_URL", workerDSN), "COMMERCE_MEDIA_EXECUTOR_DATABASE_URL", executorDSN)
	wrong := lmwReplace(base, "COMMERCE_MEDIA_EXECUTOR_DATABASE_URL", h.lp.f.runtime.Config().ConnString())
	lmwFail(t, lmwProcess(t, binary, "wrong-role", wrong), "media_worker_database_unavailable", workerDSN)
	waitPoolsGone(t, h.lp.f, "first media pool leaked after wrong second role", workerName)
	clone := mrFixture(t)
	cloneLogin, cloneExecutor := lmaLogin(t, clone, "commerce_media_executor")
	mustExec(t, clone.owner, "REVOKE commerce_media_executor FROM "+pgx.Identifier{cloneLogin}.Sanitize())
	mustExec(t, clone.owner, "GRANT commerce_media_executor TO "+pgx.Identifier{cloneLogin}.Sanitize()+" WITH INHERIT TRUE, SET FALSE")
	cloneDSN := mrNamedDSN(t, cloneExecutor.Config().ConnString(), executorName+"_clone")
	checkWorker, err := platform.OpenMediaWorkerPool(context.Background(), workerDSN)
	if err != nil {
		t.Fatal("individually valid worker pool denied", err)
	}
	checkWorker.Close()
	checkExecutor, err := platform.OpenMediaExecutorPool(context.Background(), cloneDSN)
	if err != nil {
		t.Fatal("individually valid clone executor pool denied", err)
	}
	checkExecutor.Close()
	cross := lmwReplace(base, "COMMERCE_MEDIA_EXECUTOR_DATABASE_URL", cloneDSN)
	lmwFail(t, lmwProcess(t, binary, "cross-db", cross), "media_worker_database_unavailable", workerDSN, cloneDSN)
	// checkWorker/checkExecutor above used these same application_names; their
	// Close and the process exit are both awaited as backend teardown.
	waitPoolsGone(t, h.lp.f, "cross-DB rejection leaked pools", workerName)
	waitPoolsGone(t, clone, "cross-DB rejection leaked pools", executorName+"_clone")
	// The same login acquiring both fixed authorities is forbidden before ready.
	mustExec(t, h.lp.f.owner, "GRANT commerce_media_executor TO "+pgx.Identifier{h.workerLogin}.Sanitize()+" WITH INHERIT TRUE, SET FALSE")
	t.Cleanup(func() {
		_, _ = h.lp.f.owner.Exec(context.Background(), "REVOKE commerce_media_executor FROM "+pgx.Identifier{h.workerLogin}.Sanitize())
	})
	mixed := lmwProcess(t, binary, "mixed-authority", base)
	lmwFail(t, mixed, "media_worker_database_unavailable", workerDSN, executorDSN)
	mustExec(t, h.lp.f.owner, "REVOKE commerce_media_executor FROM "+pgx.Identifier{h.workerLogin}.Sanitize())
	waitPoolsGone(t, h.lp.f, "mixed-authority rejection leaked pools", workerName, executorName)
	// A valid process must never report ready after a required guard drifts.
	_, err = h.lp.f.owner.Exec(context.Background(), `REVOKE EXECUTE ON FUNCTION live.media_worker_ready() FROM commerce_media_executor`)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = h.lp.f.owner.Exec(context.Background(), `GRANT EXECUTE ON FUNCTION live.media_worker_ready() TO commerce_media_executor`)
	})
	lmwFail(t, lmwProcess(t, binary, "drift", base), "media_worker_database_unavailable", workerDSN, executorDSN)
	waitPoolsGone(t, h.lp.f, "readiness drift leaked pools", workerName, executorName)
}

func TestLiveMediaRuntimeLMW04And05ProcessStartStopRestart(t *testing.T) {
	var h *lmeHarness
	var stopAfterRestart atomic.Bool
	var starts, stops atomic.Int32
	server, ca := lmwTLS(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Host != "unit.livekit.cloud" || r.TLS == nil {
			t.Error("unpinned provider transport")
		}
		switch r.URL.Path {
		case "/twirp/livekit.Egress/StartEgress":
			starts.Add(1)
			lmeReply(w, h.observation("EG_lmw", "EGRESS_ACTIVE", 100, 110, 0))
		case "/twirp/livekit.Egress/ListEgress":
			lmeReply(w, `{"items":[`+h.observation("EG_lmw", "EGRESS_ACTIVE", 100, 120, 0)+`]}`)
		case "/twirp/livekit.Egress/StopEgress":
			stops.Add(1)
			if !stopAfterRestart.Load() {
				t.Error("Stop during process shutdown")
			}
			lmeReply(w, h.observation("EG_lmw", "EGRESS_COMPLETE", 100, 150, 140))
		default:
			http.Error(w, "unknown", 500)
		}
	}))
	h = lmrSetup(t, func(w http.ResponseWriter, _ *http.Request) { http.Error(w, "unused", 500) })
	var foreignID int64
	if err := h.lp.f.runtime.QueryRow(context.Background(), `INSERT INTO river.river_job(kind,queue,args,max_attempts) VALUES('foreign_lmw_probe','default','{}',1) RETURNING id`).Scan(&foreignID); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = h.lp.f.owner.Exec(context.Background(), `DELETE FROM river.river_job WHERE id=$1`, foreignID)
	})
	var foreignBefore []byte
	if err := h.lp.f.owner.QueryRow(context.Background(), `SELECT to_jsonb(j) FROM river.river_job j WHERE id=$1`, foreignID).Scan(&foreignBefore); err != nil {
		t.Fatal(err)
	}
	binary := mrBuild(t, "../../cmd/media-worker", "media-worker-lifecycle")
	env := lmwEnvironment(h, server.Listener.Addr().String(), ca)
	workerName, executorName := "lmw_positive_worker_"+t04Tag(), "lmw_positive_executor_"+t04Tag()
	env = lmwReplace(lmwReplace(env, "COMMERCE_MEDIA_WORKER_DATABASE_URL", mrNamedDSN(t, h.worker.Config().ConnString(), workerName)), "COMMERCE_MEDIA_EXECUTOR_DATABASE_URL", mrNamedDSN(t, h.executor.Config().ConnString(), executorName))
	p := lmwProcess(t, binary, "start", env)
	mrReadyLog(t, p, "media_worker_ready")
	f := h.await(t, 20*time.Second, func(f lmeFacts) bool { return f.observations >= 1 && f.egress == "EG_lmw" })
	if f.operation != "UNKNOWN" || f.resource != "OBSERVED" || f.status != "EGRESS_ACTIVE" || starts.Load() != 1 || stops.Load() != 0 {
		t.Fatalf("Start was not durably observed: %+v", f)
	}
	mrStop(t, p, syscall.SIGTERM, true)
	waitPoolsGone(t, h.lp.f, "shutdown leaked media pool or issued provider Stop", workerName, executorName)
	if stops.Load() != 0 {
		t.Fatal("shutdown leaked media pool or issued provider Stop")
	}
	// Persisted Start must survive command restart without another Start.
	before := h.facts(t)
	if before.egress != "EG_lmw" {
		t.Fatal("attempt identity lost")
	}
	lmrStop(t, h, t04Key("lmw-stop"))
	stopAfterRestart.Store(true)
	restart := lmwProcess(t, binary, "restart", env)
	mrReadyLog(t, restart, "media_worker_ready")
	deadline := time.Now().Add(25 * time.Second)
	for time.Now().Before(deadline) {
		state := lmrRead(t, h)
		if state.resource == "TERMINAL" {
			break
		}
		time.Sleep(25 * time.Millisecond)
	}
	state := lmrRead(t, h)
	if state.resource != "TERMINAL" || state.count != 1 || starts.Load() != 1 || stops.Load() != 1 {
		t.Fatalf("Stop terminal evidence/budget: %+v", state)
	}
	mrStop(t, restart, syscall.SIGINT, true)
	waitPoolsGone(t, h.lp.f, "restart shutdown leaked pools", workerName, executorName)
	var foreignAfter []byte
	if err := h.lp.f.owner.QueryRow(context.Background(), `SELECT to_jsonb(j) FROM river.river_job j WHERE id=$1`, foreignID).Scan(&foreignAfter); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(foreignAfter, foreignBefore) {
		t.Fatal("media worker changed nonempty foreign job row")
	}
	if got := h.facts(t); got.egress != "EG_lmw" {
		t.Fatalf("restart changed attempt: %+v", got)
	}
}

func TestLiveMediaRuntimeLMW05MissingFrozenProjectRetainsLiability(t *testing.T) {
	var h *lmeHarness
	var starts, stops atomic.Int32
	server, ca := lmwTLS(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/twirp/livekit.Egress/StartEgress" {
			starts.Add(1)
			t.Error("replacement Start after missing project")
		}
		if r.URL.Path == "/twirp/livekit.Egress/StopEgress" {
			stops.Add(1)
			lmeReply(w, h.observation("EG_lmw_missing", "EGRESS_COMPLETE", 100, 150, 140))
			return
		}
		lmeReply(w, `{"items":[`+h.observation("EG_lmw_missing", "EGRESS_ACTIVE", 100, 120, 0)+`]}`)
	}))
	h = lmrSetup(t, func(w http.ResponseWriter, _ *http.Request) { http.Error(w, "unused", 500) })
	lmrStarted(t, h, "EG_lmw_missing")
	lmrStop(t, h, t04Key("lmw-missing-stop"))
	binary := mrBuild(t, "../../cmd/media-worker", "media-worker-missing")
	env := lmwEnvironment(h, server.Listener.Addr().String(), ca)
	var projects map[string]any
	if err := json.Unmarshal([]byte(lmwEnvValue(env, "COMMERCE_MEDIA_PROJECTS_JSON")), &projects); err != nil {
		t.Fatal(err)
	}
	projects["projects"].([]any)[0].(map[string]any)["credential_version"] = 2
	badJSON, _ := json.Marshal(projects)
	missing := lmwReplace(env, "COMMERCE_MEDIA_PROJECTS_JSON", string(badJSON))
	p := lmwProcess(t, binary, "missing-project", missing)
	mrReadyLog(t, p, "media_worker_ready")
	h.await(t, 15*time.Second, func(f lmeFacts) bool { return f.result == "credential_unavailable" })
	mrStop(t, p, syscall.SIGTERM, true)
	before := lmrRead(t, h)
	if before.count != 0 || before.resource == "TERMINAL" || starts.Load() != 0 || stops.Load() != 0 {
		t.Fatalf("missing project consumed stop budget or closed liability: %+v", before)
	}
	good := lmwProcess(t, binary, "restored-project", env)
	mrReadyLog(t, good, "media_worker_ready")
	deadline := time.Now().Add(25 * time.Second)
	for time.Now().Before(deadline) {
		if lmrRead(t, h).resource == "TERMINAL" {
			break
		}
		time.Sleep(25 * time.Millisecond)
	}
	after := lmrRead(t, h)
	if after.resource != "TERMINAL" || after.count != 1 || starts.Load() != 0 || stops.Load() != 1 {
		t.Fatalf("restored original project did not resume: %+v", after)
	}
	mrStop(t, good, syscall.SIGTERM, true)
}

func TestLiveMediaRuntimeLMW05MissingOldKeyThenResume(t *testing.T) {
	var h *lmeHarness
	var starts, stops atomic.Int32
	server, ca := lmwTLS(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/twirp/livekit.Egress/StartEgress":
			starts.Add(1)
			lmeReply(w, h.observation("EG_lmw_key", "EGRESS_ACTIVE", 100, 110, 0))
		case "/twirp/livekit.Egress/ListEgress":
			lmeReply(w, `{"items":[`+h.observation("EG_lmw_key", "EGRESS_ACTIVE", 100, 120, 0)+`]}`)
		case "/twirp/livekit.Egress/StopEgress":
			stops.Add(1)
			http.Error(w, "unexpected Stop", 500)
		default:
			http.Error(w, "unexpected", 500)
		}
	}))
	h = lmrSetup(t, func(w http.ResponseWriter, _ *http.Request) { http.Error(w, "unused", 500) })
	binary := mrBuild(t, "../../cmd/media-worker", "media-worker-old-key")
	env := lmwEnvironment(h, server.Listener.Addr().String(), ca)
	other := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{0x48}, 32))
	missing := lmwReplace(env, "COMMERCE_MEDIA_MATERIAL_ACTIVE_KEY_ID", "new_only")
	missing = lmwReplace(missing, "COMMERCE_MEDIA_MATERIAL_KEYS_JSON", `{"keys":[{"id":"new_only","key_base64":"`+other+`"}]}`)
	bad := lmwProcess(t, binary, "missing-old-key", missing)
	mrReadyLog(t, bad, "media_worker_ready")
	h.await(t, 15*time.Second, func(f lmeFacts) bool { return f.result == "material_invalid" })
	mrStop(t, bad, syscall.SIGTERM, true)
	if f := h.facts(t); f.reserved || f.observations != 0 || starts.Load() != 0 || stops.Load() != 0 {
		t.Fatalf("missing key made provider side effect: %+v", f)
	}
	good := lmwProcess(t, binary, "restored-old-key", env)
	mrReadyLog(t, good, "media_worker_ready")
	f := h.await(t, 20*time.Second, func(f lmeFacts) bool { return f.observations >= 1 && f.egress == "EG_lmw_key" })
	if f.resource != "OBSERVED" || starts.Load() != 1 || stops.Load() != 0 {
		t.Fatalf("original attempt not resumed exactly once: %+v start=%d stop=%d", f, starts.Load(), stops.Load())
	}
	mrStop(t, good, syscall.SIGTERM, true)
}
