package foundation_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"livecommerce/internal/httpapi"
)

func TestStudioBackendSTU02HTTPRoutesAndStrictInput(t *testing.T) {
	h := lmpSetup(t, true)
	// RequestStop creates an execution-state row even before provider dispatch.
	// Remove only this test's row before the shared LMP fixture removes its job.
	t.Cleanup(func() {
		ctx := context.Background()
		for _, item := range []struct{ query, arg string }{
			{`DELETE FROM live.media_execution_state WHERE attempt_id IN (SELECT id FROM live.media_attempts WHERE session_id=$1)`, h.session},
			{`DELETE FROM ops.command_results WHERE principal_id=$1 AND operation='live.media.stop'`, h.lp.actor},
			{`DELETE FROM ops.audit_events WHERE principal_id=$1 AND action='live.media.stop.requested'`, h.lp.actor},
		} {
			if _, err := h.lp.f.owner.Exec(ctx, item.query, item.arg); err != nil {
				t.Errorf("Studio HTTP cleanup: %v", err)
			}
		}
	})
	base := "/v1/admin/stores/" + h.lp.f.storeA1 + "/live-sessions"
	active := httpapi.NewHandler(h.lp.f.runtime, httpapi.Options{Live: h.planner})
	request := func(handler http.Handler, method, path, token, key, body string, want int) map[string]any {
		t.Helper()
		var payload *strings.Reader
		if body != "" {
			payload = strings.NewReader(body)
		}
		var r *http.Request
		if payload == nil {
			r = httptest.NewRequest(method, path, nil)
		} else {
			r = httptest.NewRequest(method, path, payload)
		}
		r.Header.Set("Authorization", "Bearer "+token)
		if body != "" {
			r.Header.Set("Content-Type", "application/json")
		}
		if key != "" {
			r.Header.Set("Idempotency-Key", key)
		}
		r.Header.Set("X-Tenant-ID", h.lp.f.tenantB)
		r.Host = "attacker.invalid"
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		if w.Code != want {
			t.Fatalf("%s %s got=%d want=%d body=%s", method, path, w.Code, want, w.Body.String())
		}
		if got := w.Header().Get("Cache-Control"); !strings.Contains(got, "no-store") {
			t.Fatalf("cacheable %s %s: %q", method, path, got)
		}
		if bytes.Contains(w.Body.Bytes(), []byte("project_lma")) || bytes.Contains(w.Body.Bytes(), []byte("unit.livekit.cloud")) || bytes.Contains(w.Body.Bytes(), []byte("lma_key_1")) {
			t.Fatalf("secret or endpoint in HTTP response: %s", w.Body.String())
		}
		var out map[string]any
		if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
			t.Fatalf("non-JSON response %s %s: %v", method, path, err)
		}
		return out
	}
	defaultOff := httpapi.NewHandler(h.lp.f.runtime)
	for _, route := range []struct{ method, path string }{
		{"GET", base}, {"POST", base}, {"GET", base + "/" + h.session}, {"PATCH", base + "/" + h.session},
		{"POST", base + "/" + h.session + "/rehearsal/start"}, {"POST", base + "/" + h.session + "/rehearsal/stop"},
	} {
		request(defaultOff, route.method, route.path, h.lp.token, "", "", 404)
	}
	created := request(active, "POST", base, h.lp.token, t04Key("studio-http-create"), `{"title":"HTTP Studio","aspect_ratio":"16:9"}`, 200)
	id, ok := created["session_id"].(string)
	if !ok || id == "" || created["version"] != float64(1) {
		t.Fatalf("bad create: %v", created)
	}
	listed := request(active, "GET", base+"?limit=1", h.lp.token, "", "", 200)
	studioKeys(t, listed, "items", "next_cursor")
	items := listed["items"].([]any)
	if len(items) != 1 || items[0].(map[string]any)["session_id"] != id {
		t.Fatalf("list not current: %v", listed)
	}
	detail := request(active, "GET", base+"/"+id, h.lp.token, "", "", 200)
	studioKeys(t, detail, "draft", "prepared", "attempt", "can_manage", "media_enabled")
	if detail["prepared"] != nil || detail["attempt"] != nil || detail["media_enabled"] != true {
		t.Fatalf("unprepared draft falsely eligible: %v", detail)
	}
	changed := request(active, "PATCH", base+"/"+id, h.lp.token, t04Key("studio-http-edit"), `{"title":"Edited Studio","aspect_ratio":"16:9","expected_version":1}`, 200)
	if changed["version"] != float64(2) || changed["title"] != "Edited Studio" {
		t.Fatalf("edit not persisted: %v", changed)
	}
	request(active, "PATCH", base+"/"+id, h.lp.token, t04Key("studio-http-stale"), `{"title":"Stale Studio","aspect_ratio":"16:9","expected_version":1}`, 409)
	prepared := request(active, "GET", base+"/"+h.session, h.lp.token, "", "", 200)
	if prepared["prepared"] == nil || prepared["attempt"] != nil {
		t.Fatalf("trusted fixture not prepared: %v", prepared)
	}
	startPath := base + "/" + h.session + "/rehearsal/start"
	startBody := `{"authorization_id":"` + h.input.AuthorizationID + `","expected_session_version":1}`
	startKey := t04Key("studio-http-start")
	started := request(active, "POST", startPath, h.lp.token, startKey, startBody, 200)
	studioKeys(t, started, "session_id", "attempt_id", "state")
	if started["session_id"] != h.session || started["attempt_id"] == "" {
		t.Fatalf("unsafe or absent receipt: %v", started)
	}
	replay := request(active, "POST", startPath, h.lp.token, startKey, startBody, 200)
	if !reflect.DeepEqual(started, replay) {
		t.Fatalf("Start replay changed receipt: %v %v", started, replay)
	}
	stopPath := base + "/" + h.session + "/rehearsal/stop"
	stopped := request(active, "POST", stopPath, h.lp.token, t04Key("studio-http-stop"), `{"attempt_id":"`+started["attempt_id"].(string)+`"}`, 200)
	studioKeys(t, stopped, "session_id", "attempt_id", "state")
	if stopped["attempt_id"] != started["attempt_id"] {
		t.Fatalf("Stop targeted another attempt: %v", stopped)
	}
	for _, bad := range []struct {
		method, path, body string
		want               int
	}{
		{"GET", base + "?limit=01", "", 422},
		{"GET", base + "?li%6dit=1", "", 422},
		{"GET", base + "?limit=1&limit=2", "", 422},
		{"GET", base + "?bogus=1", "", 422},
		{"GET", base + "/" + id + "?cursor=x", "", 422},
		{"POST", base, `{"title":"a","title":"b","aspect_ratio":"16:9"}`, 400},
		{"POST", base, `{"title":"a","Title":"b","aspect_ratio":"16:9"}`, 400},
		{"POST", base, `{"TITLE":"a","aspect_ratio":"16:9"}`, 400},
		{"POST", base, `{"title":"a","aspect_ratio":"16:9","unexpected":1}`, 400},
		{"POST", base, `{"title":"a","aspect_ratio":"16:9"}{}`, 400},
		{"POST", startPath, `{"authorization_id":"` + h.input.AuthorizationID + `","authorization_id":"` + h.input.AuthorizationID + `","expected_session_version":1}`, 400},
		{"POST", stopPath, `{"attempt_id":"` + started["attempt_id"].(string) + `","unknown":true}`, 400},
	} {
		request(active, bad.method, bad.path, h.lp.token, t04Key("studio-invalid"), bad.body, bad.want)
	}
	request(active, "GET", base, h.lp.limitedToken, "", "", 403)
	request(active, "GET", "/v1/admin/stores/"+h.lp.f.storeA2+"/live-sessions/"+id, h.lp.token, "", "", 404)
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodDelete, base+"/"+id, nil)
	r.Header.Set("Authorization", "Bearer "+h.lp.token)
	active.ServeHTTP(w, r)
	if w.Code != http.StatusMethodNotAllowed || !strings.Contains(w.Header().Get("Cache-Control"), "no-store") {
		t.Fatalf("wrong-method Studio response cacheable or accepted: status=%d cache=%q", w.Code, w.Header().Get("Cache-Control"))
	}
}
