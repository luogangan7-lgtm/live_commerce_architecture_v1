// Claim-source real-PG gate CS01 (docs/delivery/units/claim-source.md, contracts/meta-claims-intake-v1.md §2,
// rulings h and 22): GET/PUT claim-source through the real admin handler on a real PostgreSQL.
//
// Owns: bind, edit, re-bind with CAS, deactivate, conflict across sessions, permission denial, idempotent
// replay, the input parser's HTTP outcome and the binding resolution errors. Evidence label REAL_PG (HTTP over
// httptest, no Meta call exists in R1).
//
// Every value is synthetic. Two fresh stores of the shared tenant isolate the gate from Meta bindings
// other gates leave enabled in store A1 (one binding per platform is a claim-source rule).
package foundation_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"livecommerce/internal/claims"
	"livecommerce/internal/httpapi"
)

type csEnv struct {
	*lcHarness
	m           miTest
	srv         *httptest.Server
	store       string // one Facebook binding + route, one Instagram binding + route
	bare        string // no Meta binding
	multi       string // two enabled Facebook bindings
	igOnly      string // one Instagram binding + route only
	asset       string // Facebook Page id of store
	igAsset     string
	igOnlyAsset string
	binding     string
	token       string // live:read, live:manage, integration:execute on all three stores
	sessions    []string
}

func csSetup(t *testing.T) *csEnv {
	t.Helper()
	h := lcSetup(t)
	f := h.f
	e := &csEnv{lcHarness: h, m: miSetup(t)}
	for _, dst := range []*string{&e.store, &e.bare, &e.multi, &e.igOnly} {
		*dst = randomUUID()
		mustExec(t, f.owner, `INSERT INTO control.stores(tenant_id,id,name,currency) VALUES($1,$2,'CS gate store','USD')`, f.tenantA, *dst)
	}
	stores := []string{e.store, e.bare, e.multi, e.igOnly}
	_, e.token = lcPrincipal(t, f, f.tenantA, stores, "store:read", "live:read", "live:manage", "integration:execute")
	t.Cleanup(func() {
		for _, s := range e.sessions {
			mustExec(t, f.owner, `DELETE FROM claims.meta_intake WHERE session_id=$1`, s)
			mustExec(t, f.owner, `DELETE FROM live.claim_sources WHERE session_id=$1`, s)
		}
	})
	e.asset, e.igAsset = miAsset(), miAsset()
	e.binding = miBinding(t, e.m, e.asset, "facebook", f.tenantA, e.store, f.principalA)
	miRoute(t, e.m, e.asset, f.tenantA, e.store, e.binding)
	igBinding := miBinding(t, e.m, e.igAsset, "instagram", f.tenantA, e.store, f.principalA)
	var route string
	var epoch int64
	if err := e.m.registrar.QueryRow(context.Background(), `SELECT * FROM meta_inbox.activate_route($1,'instagram',$2,$3,$4,$5,1,$6,$7,0)`,
		miApp, e.igAsset, f.tenantA, e.store, igBinding, strings.Repeat("a", 64), time.Now().Add(time.Hour)).Scan(&route, &epoch); err != nil {
		t.Fatal("instagram route", err)
	}
	igOnlyAsset := miAsset()
	igOnlyBinding := miBinding(t, e.m, igOnlyAsset, "instagram", f.tenantA, e.igOnly, f.principalA)
	if err := e.m.registrar.QueryRow(context.Background(), `SELECT * FROM meta_inbox.activate_route($1,'instagram',$2,$3,$4,$5,1,$6,$7,0)`,
		miApp, igOnlyAsset, f.tenantA, e.igOnly, igOnlyBinding, strings.Repeat("b", 64), time.Now().Add(time.Hour)).Scan(&route, &epoch); err != nil {
		t.Fatal("instagram-only route", err)
	}
	e.igOnlyAsset = igOnlyAsset
	miBinding(t, e.m, miAsset(), "facebook", f.tenantA, e.multi, f.principalA)
	miBinding(t, e.m, miAsset(), "facebook", f.tenantA, e.multi, f.principalA)
	labels, err := claims.NewLabelKey(randomBytes(32))
	if err != nil {
		t.Fatal(err)
	}
	e.srv = httptest.NewServer(httpapi.NewHandler(f.runtime, httpapi.Options{ClaimLabels: &labels}))
	t.Cleanup(e.srv.Close)
	return e
}

func (e *csEnv) session(t *testing.T, store string) string {
	t.Helper()
	s := e.draftAs(t, e.token, store)
	e.sessions = append(e.sessions, s)
	return s
}

func (e *csEnv) call(t *testing.T, method, store, session, token, key, body string, spelling ...string) (int, map[string]any, []byte) {
	t.Helper()
	seg := "live-sessions"
	if len(spelling) > 0 {
		seg = spelling[0]
	}
	req, err := http.NewRequest(method, e.srv.URL+"/v1/admin/stores/"+store+"/"+seg+"/"+session+"/claim-source", bytes.NewReader([]byte(body)))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	if key != "" {
		req.Header.Set("Idempotency-Key", key)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var out map[string]any
	_ = json.Unmarshal(raw, &out)
	return resp.StatusCode, out, raw
}

func csBody(input string, private bool, locale string, active bool, expected int64) string {
	raw, _ := json.Marshal(map[string]any{"input": input, "private_reply": private, "reply_locale": locale, "active": active, "expected_version": expected})
	return string(raw)
}

// csBodyP is csBody plus the optional platform hint of ruling p.
func csBodyP(input string, private bool, locale string, active bool, expected int64, platform string) string {
	raw, _ := json.Marshal(map[string]any{"input": input, "private_reply": private, "reply_locale": locale, "active": active,
		"expected_version": expected, "platform": platform})
	return string(raw)
}

func csWant(t *testing.T, what string, status int, out map[string]any, raw []byte, wantStatus int, wantCode string) {
	t.Helper()
	if status != wantStatus || (wantCode != "" && out["code"] != wantCode) {
		t.Fatalf("%s: status=%d code=%v want %d %s body=%s", what, status, out["code"], wantStatus, wantCode, raw)
	}
}

func TestClaimSourceCS01(t *testing.T) {
	e := csSetup(t)
	f := e.f
	ctx := context.Background()
	s1 := e.session(t, e.store)
	post := "https://www.facebook.com/synthetic.page/posts/777"
	wantID := e.asset + "_777"
	key := func() string { return t04Key("cs-key") }

	// GET before any bind.
	// Ruling p: platforms lists the store's enabled Meta binding providers (both here, so the UI shows its select).
	status, out, raw := e.call(t, "GET", e.store, s1, e.token, "", "")
	if status != 200 || len(out) != 2 || out["source"] != nil || fmt.Sprint(out["platforms"]) != "[facebook instagram]" {
		t.Fatalf("empty GET: %d %s", status, raw)
	}
	if status, out, raw := e.call(t, "GET", e.igOnly, e.session(t, e.igOnly), e.token, "", ""); status != 200 || fmt.Sprint(out["platforms"]) != "[instagram]" {
		t.Fatalf("instagram-only GET: %d %s", status, raw)
	}
	if status, out, raw := e.call(t, "GET", e.bare, e.session(t, e.bare), e.token, "", ""); status != 200 || !bytes.Contains(raw, []byte(`"platforms":[]`)) || len(out) != 2 {
		t.Fatalf("no-binding GET: %d %s", status, raw)
	}

	// Bind (the session has no claim window row yet: the default CLOSED row is created).
	k1 := key()
	body := csBody(post, false, "zh-TW", true, 0)
	status, out, raw = e.call(t, "PUT", e.store, s1, e.token, k1, body)
	csWant(t, "bind", status, out, raw, 200, "")
	if out["platform"] != "facebook" || out["object"] != "page" || out["asset_id"] != e.asset || out["source_object_id"] != wantID ||
		out["version"] != float64(1) || out["active"] != true || out["private_reply"] != false || out["verified"] != false ||
		out["reply_locale"] != "zh-TW" || out["intake_count"] != float64(0) || out["intake_capped"] != float64(0) || len(out) != 13 {
		t.Fatalf("bound source: %s", raw)
	}
	firstID := out["id"].(string)
	if n := countRows(t, f.owner, `SELECT count(*) FROM live.claim_windows WHERE session_id=$1 AND state='CLOSED' AND generation=0`, s1); n != 1 {
		t.Fatalf("default window rows=%d", n)
	}
	// GET returns the same source in the envelope.
	status, out2, raw2 := e.call(t, "GET", e.store, s1, e.token, "", "")
	src, _ := out2["source"].(map[string]any)
	if status != 200 || src == nil || src["id"] != firstID || src["version"] != float64(1) {
		t.Fatalf("GET after bind: %d %s", status, raw2)
	}
	// Ruling o: live-sessions is the only spelling; the /live/sessions/ alias is gone.
	if status, _, raw3 := e.call(t, "GET", e.store, s1, e.token, "", "", "live/sessions"); status != 404 {
		t.Fatalf("alias GET: %d %s", status, raw3)
	}

	// Idempotent replay: same key + body returns the same bytes and writes nothing; same key, other body conflicts.
	status, _, replay := e.call(t, "PUT", e.store, s1, e.token, k1, body)
	if status != 200 || !bytes.Equal(replay, raw) {
		t.Fatalf("replay differs: %d %s vs %s", status, replay, raw)
	}
	if n := countRows(t, f.owner, `SELECT count(*) FROM live.claim_sources WHERE session_id=$1`, s1); n != 1 {
		t.Fatalf("rows after replay=%d", n)
	}
	status, out, raw = e.call(t, "PUT", e.store, s1, e.token, k1, csBody(post, false, "en", true, 0))
	csWant(t, "same key other body", status, out, raw, 409, "conflict")
	if n := countRows(t, f.owner, `SELECT count(*) FROM ops.audit_events WHERE tenant_id=$1 AND store_id=$2 AND action='live.claim_source.put'`, f.tenantA, e.store); n != 1 {
		t.Fatalf("audit rows=%d, want 1 (the replay writes none)", n)
	}

	// private_reply needs a current Page token: page_token_missing (ruling s, distinct from binding_missing)
	// until the registrar stores one.
	status, out, raw = e.call(t, "PUT", e.store, s1, e.token, key(), csBody(post, true, "zh-TW", true, 1))
	csWant(t, "private reply without token", status, out, raw, 409, "page_token_missing")
	regP, _ := lcPrincipal(t, f, f.tenantA, []string{e.store}, "store:read", "integration:manage")
	if err := e.m.registrar.QueryRow(ctx, `SELECT integration.register_meta_page_token($1,$2,$3,$4,'facebook',$5,0,'k1',$6,$7,ARRAY['pages_messaging'])`,
		f.tenantA, e.store, regP, e.binding, e.asset, randomBytes(12), randomBytes(48)).Scan(new(int64)); err != nil {
		t.Fatal("register token", err)
	}
	// Edit with CAS: stale version first, then the right one.
	status, out, raw = e.call(t, "PUT", e.store, s1, e.token, key(), csBody(post, true, "en", true, 0))
	csWant(t, "stale version", status, out, raw, 409, "version_changed")
	status, out, raw = e.call(t, "PUT", e.store, s1, e.token, key(), csBody(post, true, "en", true, 1))
	csWant(t, "edit", status, out, raw, 200, "")
	if out["id"] != firstID || out["version"] != float64(2) || out["private_reply"] != true || out["reply_locale"] != "en" {
		t.Fatalf("edited source: %s", raw)
	}

	// Re-bind to another post: the old row is deactivated, the new one is bound, one active source per session.
	status, out, raw = e.call(t, "PUT", e.store, s1, e.token, key(), csBody("https://facebook.com/synthetic.page/videos/888", true, "en", true, 2))
	csWant(t, "rebind", status, out, raw, 200, "")
	if out["source_object_id"] != e.asset+"_888" || out["version"] != float64(1) || out["id"] == firstID || out["active"] != true {
		t.Fatalf("rebound source: %s", raw)
	}
	secondID := out["id"].(string)
	if n := countRows(t, f.owner, `SELECT count(*) FROM live.claim_sources WHERE session_id=$1 AND active`, s1); n != 1 {
		t.Fatalf("active sources after rebind=%d", n)
	}
	if n := countRows(t, f.owner, `SELECT count(*) FROM live.claim_sources WHERE id=$1 AND NOT active AND version=3`, firstID); n != 1 {
		t.Fatal("the previous source was not deactivated with a version bump")
	}
	// A rebind with a stale version changes nothing.
	status, out, raw = e.call(t, "PUT", e.store, s1, e.token, key(), csBody(post, true, "en", true, 2))
	csWant(t, "stale rebind", status, out, raw, 409, "version_changed")
	if n := countRows(t, f.owner, `SELECT count(*) FROM live.claim_sources WHERE session_id=$1 AND active`, s1); n != 1 {
		t.Fatal("stale rebind changed the active source")
	}
	// Rebind back to the first post re-uses its row (id kept, version continues).
	status, out, raw = e.call(t, "PUT", e.store, s1, e.token, key(), csBody(post, false, "en", true, 1))
	csWant(t, "rebind back", status, out, raw, 200, "")
	if out["id"] != firstID || out["version"] != float64(4) || out["private_reply"] != false {
		t.Fatalf("re-activated source: %s", raw)
	}

	// Deactivate.
	status, out, raw = e.call(t, "PUT", e.store, s1, e.token, key(), csBody(post, false, "en", false, 4))
	csWant(t, "deactivate", status, out, raw, 200, "")
	if out["active"] != false || out["version"] != float64(5) {
		t.Fatalf("deactivated: %s", raw)
	}
	status, out2, raw2 = e.call(t, "GET", e.store, s1, e.token, "", "")
	if src, _ = out2["source"].(map[string]any); status != 200 || src == nil || src["active"] != false {
		t.Fatalf("GET after deactivate: %s", raw2)
	}

	// An active object never maps to two sessions: reactivate on s1, then s2 asks for the same post.
	status, out, raw = e.call(t, "PUT", e.store, s1, e.token, key(), csBody(post, false, "en", true, 5))
	csWant(t, "reactivate", status, out, raw, 200, "")
	s2 := e.session(t, e.store)
	status, out, raw = e.call(t, "PUT", e.store, s2, e.token, key(), csBody(post, false, "en", true, 0))
	csWant(t, "source conflict", status, out, raw, 409, "source_conflict")
	if n := countRows(t, f.owner, `SELECT count(*) FROM live.claim_sources WHERE session_id=$1`, s2); n != 0 {
		t.Fatal("conflicting bind left a row")
	}
	_ = secondID

	// Instagram: numeric media id only; a shortcode needs Graph and is refused.
	s3 := e.session(t, e.store)
	status, out, raw = e.call(t, "PUT", e.store, s3, e.token, key(), csBody("https://www.instagram.com/p/CxYz123AbC/", false, "en", true, 0))
	csWant(t, "instagram shortcode", status, out, raw, 422, "input_unresolvable")
	// A bare numeric id is ambiguous between the store's two Meta bindings.
	status, out, raw = e.call(t, "PUT", e.store, s3, e.token, key(), csBody("17900000000000001", false, "en", true, 0))
	csWant(t, "bare id with two platforms", status, out, raw, 409, "binding_ambiguous")
	// ... and an explicit Facebook URL selects the Facebook binding.
	status, out, raw = e.call(t, "PUT", e.store, s3, e.token, key(), csBody("facebook.com/x/posts/999", false, "en", true, 0))
	csWant(t, "facebook url selects binding", status, out, raw, 200, "")
	if out["platform"] != "facebook" {
		t.Fatalf("%s", raw)
	}
	// Ruling p: re-saving the stored Facebook source_object_id ("<page>_<post>") with its platform edits the same row.
	status, out, raw = e.call(t, "PUT", e.store, s3, e.token, key(), csBodyP(e.asset+"_999", false, "en", false, 1, "facebook"))
	csWant(t, "re-save stored facebook id", status, out, raw, 200, "")
	if out["source_object_id"] != e.asset+"_999" || out["version"] != float64(2) || out["active"] != false {
		t.Fatalf("re-saved source: %s", raw)
	}
	// Ruling p: the platform hint resolves a bare id on a two-platform store; a contradicting hint is input_invalid.
	s5 := e.session(t, e.store)
	status, out, raw = e.call(t, "PUT", e.store, s5, e.token, key(), csBodyP("https://facebook.com/x/posts/5", false, "en", true, 0, "instagram"))
	csWant(t, "contradicting platform", status, out, raw, 422, "input_invalid")
	status, out, raw = e.call(t, "PUT", e.store, s5, e.token, key(), csBodyP("17900000000000005", false, "en", true, 0, "tiktok"))
	csWant(t, "unknown platform", status, out, raw, 422, "invalid_request")
	status, out, raw = e.call(t, "PUT", e.store, s5, e.token, key(), csBodyP("17900000000000005", false, "en", true, 0, "instagram"))
	csWant(t, "bare id with instagram hint", status, out, raw, 200, "")
	if out["platform"] != "instagram" || out["object"] != "instagram" || out["asset_id"] != e.igAsset || out["source_object_id"] != "17900000000000005" {
		t.Fatalf("instagram source on a two-platform store: %s", raw)
	}
	// A store whose only Meta binding is Instagram takes the bare numeric media id as delivered (media.id).
	sIG := e.session(t, e.igOnly)
	status, out, raw = e.call(t, "PUT", e.igOnly, sIG, e.token, key(), csBody("17900000000000001", false, "zh-CN", true, 0))
	csWant(t, "instagram media id", status, out, raw, 200, "")
	if out["platform"] != "instagram" || out["object"] != "instagram" || out["asset_id"] != e.igOnlyAsset || out["source_object_id"] != "17900000000000001" || out["verified"] != true {
		t.Fatalf("instagram source: %s", raw)
	}
	status, out, raw = e.call(t, "PUT", e.igOnly, e.session(t, e.igOnly), e.token, key(), csBody("https://facebook.com/x/posts/1", false, "en", true, 0))
	csWant(t, "facebook url on an instagram-only store", status, out, raw, 409, "binding_missing")

	// Input errors.
	s4 := e.session(t, e.store)
	for input, want := range map[string]string{
		"https://fb.watch/abcDEF/":         "input_unresolvable",
		"not a link":                       "input_invalid",
		"https://evil.example.com/x/posts": "input_invalid",
		"http://facebook.com/x/posts/1":    "input_invalid",
	} {
		status, out, raw = e.call(t, "PUT", e.store, s4, e.token, key(), csBody(input, false, "en", true, 0))
		csWant(t, "input "+input, status, out, raw, 422, want)
	}
	if n := countRows(t, f.owner, `SELECT count(*) FROM live.claim_sources WHERE session_id=$1`, s4); n != 0 {
		t.Fatal("rejected input left a row")
	}

	// Binding resolution: no binding, several bindings.
	sBare, sMulti := e.session(t, e.bare), e.session(t, e.multi)
	status, out, raw = e.call(t, "PUT", e.bare, sBare, e.token, key(), csBody("1234", false, "en", true, 0))
	csWant(t, "no binding", status, out, raw, 409, "binding_missing")
	status, out, raw = e.call(t, "PUT", e.multi, sMulti, e.token, key(), csBody("https://facebook.com/x/posts/1", false, "en", true, 0))
	csWant(t, "two bindings", status, out, raw, 409, "binding_ambiguous")

	// A binding whose Page was never routed (meta_inbox.routes) is refused by the definer: binding_missing.
	miBinding(t, e.m, miAsset(), "facebook", f.tenantA, e.bare, f.principalA)
	status, out, raw = e.call(t, "PUT", e.bare, sBare, e.token, key(), csBody("1234", false, "en", true, 0))
	csWant(t, "binding without route", status, out, raw, 409, "binding_missing")

	// Permissions: each missing grant is 403, and nothing is written.
	_, noManage := lcPrincipal(t, f, f.tenantA, []string{e.store}, "store:read", "live:read", "integration:execute")
	_, noExecute := lcPrincipal(t, f, f.tenantA, []string{e.store}, "store:read", "live:read", "live:manage")
	_, noRead := lcPrincipal(t, f, f.tenantA, []string{e.store}, "store:read", "live:manage", "integration:execute")
	_, readOnly := lcPrincipal(t, f, f.tenantA, []string{e.store}, "store:read", "live:read")
	sDeny := e.session(t, e.store)
	before := countRows(t, f.owner, `SELECT count(*) FROM live.claim_sources WHERE tenant_id=$1`, f.tenantA)
	for name, token := range map[string]string{"no live:manage": noManage, "no integration:execute": noExecute, "read only": readOnly} {
		status, out, raw = e.call(t, "PUT", e.store, sDeny, token, key(), csBody(post+"9", false, "en", true, 0))
		csWant(t, "PUT "+name, status, out, raw, 403, "forbidden")
	}
	status, out, raw = e.call(t, "GET", e.store, sDeny, noRead, "", "")
	csWant(t, "GET without live:read", status, out, raw, 403, "forbidden")
	if status, _, raw = e.call(t, "GET", e.store, sDeny, readOnly, "", ""); status != 200 {
		t.Fatalf("a live:read principal must read: %d %s", status, raw)
	}
	if after := countRows(t, f.owner, `SELECT count(*) FROM live.claim_sources WHERE tenant_id=$1`, f.tenantA); after != before {
		t.Fatal("a denied PUT wrote a claim source")
	}
	// Unknown session and cross-store session.
	status, out, raw = e.call(t, "GET", e.store, randomUUID(), e.token, "", "")
	csWant(t, "unknown session", status, out, raw, 404, "not_found")
	status, out, raw = e.call(t, "PUT", e.bare, s1, e.token, key(), csBody("1", false, "en", true, 0))
	csWant(t, "session of another store", status, out, raw, 404, "not_found")
	status, out, raw = e.call(t, "GET", e.store, s1, "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA", "", "")
	csWant(t, "unknown bearer", status, out, raw, 401, "unauthorized")

	// One receipt and one audit row per applied PUT, none for failures or replays.
	if n := countRows(t, f.owner, `SELECT count(*) FROM ops.command_results WHERE tenant_id=$1 AND store_id=$2 AND operation='live.claim_source.put'`, f.tenantA, e.store); n < 7 {
		t.Fatalf("receipts=%d", n)
	}
}

// TestClaimSourceCS02ConcurrentPut: two or more PUTs on one session that hold the same expected_version must
// serialize (per-session advisory lock in PutClaimSource): exactly one applies, the rest get version_changed, and
// the session never ends with two active sources (the hidden one would keep feeding claims and private replies).
// Both starting points of the lost update are exercised: the session has no source but its claim window exists
// (ensureWindow writes nothing), and its latest source is inactive.
func TestClaimSourceCS02ConcurrentPut(t *testing.T) {
	e := csSetup(t)
	f := e.f
	const racers, rounds = 6, 4
	n0 := 0
	for _, start := range []string{"no_source", "inactive_source"} {
		for round := 0; round < rounds; round++ {
			s := e.session(t, e.store)
			// Bind then retire (or delete) a source so the window row exists and the session is not fresh.
			n0++
			seed := "facebook.com/x/posts/" + strconv.Itoa(1000+n0)
			status, out, raw := e.call(t, "PUT", e.store, s, e.token, t04Key("cs-key"), csBody(seed, false, "en", true, 0))
			csWant(t, start+" seed", status, out, raw, 200, "")
			expected := int64(0)
			if start == "inactive_source" {
				status, out, raw = e.call(t, "PUT", e.store, s, e.token, t04Key("cs-key"), csBody(seed, false, "en", false, 1))
				csWant(t, start+" seed off", status, out, raw, 200, "")
				expected = 2
			} else {
				mustExec(t, f.owner, `DELETE FROM live.claim_sources WHERE session_id=$1`, s)
			}
			codes := make([]string, racers)
			statuses := make([]int, racers)
			bodies := make([]map[string]any, racers)
			gate := make(chan struct{})
			var wg sync.WaitGroup
			for i := 0; i < racers; i++ {
				wg.Add(1)
				go func(i int) {
					defer wg.Done()
					<-gate
					post := "facebook.com/x/posts/" + strconv.Itoa(2000+n0*10+i)
					st, o, _ := e.call(t, "PUT", e.store, s, e.token, t04Key("cs-key"), csBody(post, false, "en", true, expected))
					statuses[i], bodies[i] = st, o
					codes[i], _ = o["code"].(string)
				}(i)
			}
			close(gate)
			wg.Wait()
			wins, winner := 0, ""
			for i := range statuses {
				switch {
				case statuses[i] == 200:
					wins++
					winner, _ = bodies[i]["id"].(string)
				case statuses[i] != 409 || codes[i] != "version_changed":
					t.Fatalf("%s round %d racer %d: status=%d code=%s", start, round, i, statuses[i], codes[i])
				}
			}
			if wins != 1 {
				t.Fatalf("%s round %d: %d PUTs applied, want exactly 1", start, round, wins)
			}
			if n := countRows(t, f.owner, `SELECT count(*) FROM live.claim_sources WHERE session_id=$1 AND active`, s); n != 1 {
				t.Fatalf("%s round %d: %d active sources, want 1", start, round, n)
			}
			status, out, raw = e.call(t, "GET", e.store, s, e.token, "", "")
			if src, _ := out["source"].(map[string]any); status != 200 || src == nil || src["id"] != winner {
				t.Fatalf("%s round %d: GET does not show the applied source: %s", start, round, raw)
			}
		}
	}
}
