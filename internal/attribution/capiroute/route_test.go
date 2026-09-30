package capiroute

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"

	"livecommerce/internal/attribution"
	"livecommerce/internal/integrations/core"
	metaads "livecommerce/internal/integrations/meta_ads"
)

// route_test.go: pure-logic and fake-Graph checks (no PG). Sentinels are neutral constants; no real token or PII.

const (
	testTenant  = "11111111-1111-4111-8111-111111111111"
	testStore   = "22222222-2222-4222-8222-222222222222"
	testOwner   = "33333333-3333-4333-8333-333333333333"
	testAttempt = "44444444-4444-4444-8444-444444444444"
	testSKU     = "55555555-5555-4555-8555-555555555555"
	tokenSent   = "tok-sentinel-not-a-secret"
	agentSent   = "Mozilla/5.0 (test-agent)"
)

var testKey = []byte("0123456789abcdef0123456789abcdef")

type fakeOpener struct {
	token []byte
	err   error
}

func (f fakeOpener) Open(_, _, _ string, _, _ []byte) ([]byte, error) {
	if f.err != nil {
		return nil, f.err
	}
	return append([]byte(nil), f.token...), nil
}

func goodToken() tokenRow {
	return tokenRow{tenant: testTenant, store: testStore, binding: "b", provider: "meta_dataset", asset: "31337", keyID: "k1",
		version: 1, nonce: []byte("n"), ciphertext: []byte("c"), scopes: []string{"ads_management", "ads_read", "extra"}}
}

func goodUser() userRow {
	return userRow{owner: testOwner, contents: []byte(`[{"id":"` + testSKU + `","quantity":2}]`), valueMinor: 123400, currency: "TWD",
		sourceURL: "https://shop.example.test/orders", agent: agentSent}
}

func testRoute(post poster) *route {
	return newRoute(post, fakeOpener{token: []byte(tokenSent)}, testKey, func(context.Context, string) (string, error) { return "", nil })
}

func mustAssemble(t *testing.T, r *route, tr tokenRow, ur userRow) packed {
	t.Helper()
	sec, err := r.assemble(tr, ur)
	if err != nil {
		t.Fatalf("assemble: %v", err)
	}
	var p packed
	if !decodeStrict(sec.Reveal(), &p) {
		t.Fatal("secret payload does not decode")
	}
	return p
}

func TestAssemblePacksHashedUserData(t *testing.T) {
	r := testRoute(nil)
	phone := "+886912345678"
	ur := goodUser()
	ur.phone = &phone
	p := mustAssemble(t, r, goodToken(), ur)
	wantPH, _ := attribution.HashPhone(phone)
	if p.PH != wantPH || p.ExternalID != attribution.ExternalID(testKey, testTenant, testStore, testOwner) ||
		string(p.Token) != tokenSent || p.Value != "1234.00" || p.Currency != "TWD" || len(p.Contents) != 1 {
		t.Fatalf("packed = %+v", p)
	}
	// The raw owner id and phone never enter the packed secret (only their hashes do).
	sec, _ := r.assemble(goodToken(), ur)
	for _, raw := range []string{testOwner, phone, "912345678"} {
		if bytes.Contains(sec.Reveal(), []byte(raw)) {
			t.Errorf("secret payload contains raw %q", raw)
		}
	}
	// Omitted or unusable phone => no ph key at all.
	bad := "0912345678"
	for name, ph := range map[string]*string{"nil": nil, "not e164": &bad} {
		u := goodUser()
		u.phone = ph
		if got := mustAssemble(t, r, goodToken(), u); got.PH != "" {
			t.Errorf("%s: ph = %q", name, got.PH)
		}
	}
}

func TestAssembleRefusals(t *testing.T) {
	r := testRoute(nil)
	for name, mutate := range map[string]func(*tokenRow, *userRow){
		"page token provider": func(t *tokenRow, _ *userRow) { t.provider = "facebook" },
		"missing ads_read":    func(t *tokenRow, _ *userRow) { t.scopes = []string{"ads_management"} },
		"zero amount":         func(_ *tokenRow, u *userRow) { u.valueMinor = 0 },
		"bad currency":        func(_ *tokenRow, u *userRow) { u.currency = "twd" },
		"http source":         func(_ *tokenRow, u *userRow) { u.sourceURL = "http://shop.example.test/orders" },
		"empty agent":         func(_ *tokenRow, u *userRow) { u.agent = "" },
		"bad owner":           func(_ *tokenRow, u *userRow) { u.owner = "not-a-uuid" },
		"unknown content key": func(_ *tokenRow, u *userRow) { u.contents = []byte(`[{"id":"` + testSKU + `","quantity":1,"x":1}]`) },
		"zero quantity":       func(_ *tokenRow, u *userRow) { u.contents = []byte(`[{"id":"` + testSKU + `","quantity":0}]`) },
		"no contents":         func(_ *tokenRow, u *userRow) { u.contents = []byte(`[]`) },
	} {
		tr, ur := goodToken(), goodUser()
		mutate(&tr, &ur)
		if _, err := r.assemble(tr, ur); !errors.Is(err, core.ErrPolicyDenied) {
			t.Errorf("%s: err = %v want policy denial", name, err)
		}
	}
	// A failed open is a plain error that never carries the underlying message.
	failing := newRoute(nil, fakeOpener{err: errors.New("open failed key=" + tokenSent)}, testKey, nil)
	if _, err := failing.assemble(goodToken(), goodUser()); err == nil || errors.Is(err, core.ErrPolicyDenied) || strings.Contains(err.Error(), tokenSent) {
		t.Errorf("open failure err = %v", err)
	}
}

// keys returns the sorted key set of a JSON object.
func keys(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func TestBuildBodyExactKeys(t *testing.T) {
	cr := captureRequest{V: 1, AttemptID: testAttempt, EventID: attribution.EventID(testAttempt), EventTime: 1790000000, TestEventCode: "TEST1234"}
	p := packed{Token: []byte(tokenSent), PH: strings.Repeat("a", 64), ExternalID: strings.Repeat("b", 64), Agent: agentSent,
		Contents: []content{{ID: testSKU, Quantity: 2}}, Value: "1234.00", Currency: "TWD", SourceURL: "https://shop.example.test/orders"}
	raw, err := buildBody(cr, p)
	if err != nil {
		t.Fatal(err)
	}
	var top map[string]any
	if err = json.Unmarshal(raw, &top); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(keys(top), []string{"data", "test_event_code"}) {
		t.Fatalf("top keys = %v", keys(top))
	}
	ev := top["data"].([]any)[0].(map[string]any)
	want := []string{"action_source", "custom_data", "event_id", "event_name", "event_source_url", "event_time", "user_data"}
	if !reflect.DeepEqual(keys(ev), want) {
		t.Fatalf("event keys = %v", keys(ev))
	}
	if ev["event_name"] != "Purchase" || ev["action_source"] != "website" || ev["event_id"] != "lc-purchase-"+testAttempt ||
		ev["event_time"] != float64(1790000000) || ev["event_source_url"] != "https://shop.example.test/orders" {
		t.Fatalf("event = %v", ev)
	}
	if got := keys(ev["user_data"].(map[string]any)); !reflect.DeepEqual(got, []string{"client_user_agent", "external_id", "ph"}) {
		t.Fatalf("user_data keys = %v", got)
	}
	cd := ev["custom_data"].(map[string]any)
	if got := keys(cd); !reflect.DeepEqual(got, []string{"content_type", "contents", "currency", "value"}) || cd["value"] != 1234.0 ||
		cd["content_type"] != "product" || cd["currency"] != "TWD" {
		t.Fatalf("custom_data = %v", cd)
	}
	if !bytes.Contains(raw, []byte(`"value":1234.00`)) {
		t.Fatalf("value is not the exact decimal: %s", raw)
	}
	if bytes.Contains(raw, []byte(tokenSent)) || bytes.Contains(raw, []byte("partner_agent")) || bytes.Contains(raw, []byte("access_token")) {
		t.Fatalf("body must carry neither token nor partner_agent (PostEvent adds those): %s", raw)
	}
	// LIVE: no test_event_code; no phone: no ph key.
	cr.TestEventCode, p.PH = "", ""
	raw, _ = buildBody(cr, p)
	if bytes.Contains(raw, []byte("test_event_code")) || bytes.Contains(raw, []byte(`"ph"`)) {
		t.Fatalf("live body without phone: %s", raw)
	}
}

func TestDecimal(t *testing.T) {
	for minor, want := range map[int64]string{1: "0.01", 100: "1.00", 123456: "1234.56", 100000000000: "1000000000.00"} {
		if got, ok := decimal(minor); !ok || got != want {
			t.Errorf("decimal(%d) = %q,%v", minor, got, ok)
		}
	}
	for _, minor := range []int64{0, -1} {
		if _, ok := decimal(minor); ok {
			t.Errorf("decimal(%d) must be refused", minor)
		}
	}
}

// fakeGraph records every POST.
type fakeGraph struct {
	mu    sync.Mutex
	calls []map[string]any
	paths []string
	reply func(w http.ResponseWriter)
}

func (f *fakeGraph) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	raw, _ := io.ReadAll(r.Body)
	var body map[string]any
	_ = json.Unmarshal(raw, &body)
	f.mu.Lock()
	f.calls, f.paths = append(f.calls, body), append(f.paths, r.Method+" "+r.URL.Path)
	f.mu.Unlock()
	f.reply(w)
}

func newFake(t *testing.T, reply func(http.ResponseWriter)) (*route, *fakeGraph) {
	t.Helper()
	f := &fakeGraph{reply: reply}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	client, err := metaads.NewClient(metaads.Config{GraphBaseURL: srv.URL, GraphVersion: "v26.0", PartnerAgent: "lc-test"})
	if err != nil {
		t.Fatal(err)
	}
	return testRoute(client), f
}

func dreq(request string) core.DispatchRequest {
	return core.DispatchRequest{OperationID: "66666666-6666-4666-8666-666666666666", TenantID: testTenant, StoreID: testStore,
		Provider: "meta_dataset", Action: "meta.capi.purchase", Purpose: "marketing", ExternalAssetID: "31337", Request: json.RawMessage(request), Mode: "dispatch"}
}

func goodRequest(extra string) string {
	return `{"v":1,"attempt_id":"` + testAttempt + `","event_id":"lc-purchase-` + testAttempt + `","event_time":1790000000` + extra + `}`
}

func TestDispatchPostsOneEventAndClassifies(t *testing.T) {
	ok := func(w http.ResponseWriter) {
		_, _ = w.Write([]byte(`{"events_received":1,"messages":[],"fbtrace_id":"AbC_123"}`))
	}
	r, f := newFake(t, ok)
	sec, err := r.assemble(goodToken(), goodUser())
	if err != nil {
		t.Fatal(err)
	}
	out, err := r.dispatch(context.Background(), dreq(goodRequest(`,"test_event_code":"TEST1234"`)), sec)
	if err != nil || out != (core.Outcome{State: "SUCCEEDED", Code: "graph_received", ProviderReference: "AbC_123"}) {
		t.Fatalf("out = %+v,%v", out, err)
	}
	if len(f.calls) != 1 || f.paths[0] != "POST /v26.0/31337/events" {
		t.Fatalf("calls = %v %v", f.paths, f.calls)
	}
	c := f.calls[0]
	if c["partner_agent"] != "lc-test" || c["access_token"] != tokenSent || c["test_event_code"] != "TEST1234" || len(c["data"].([]any)) != 1 {
		t.Fatalf("body = %v", c)
	}
	// UNKNOWN classes: one call, never a second.
	for name, h := range map[string]func(http.ResponseWriter){
		"5xx":      func(w http.ResponseWriter) { w.WriteHeader(500) },
		"received": func(w http.ResponseWriter) { _, _ = w.Write([]byte(`{"events_received":0}`)) },
	} {
		r, f := newFake(t, h)
		sec, _ := r.assemble(goodToken(), goodUser())
		out, err := r.dispatch(context.Background(), dreq(goodRequest("")), sec)
		if err != nil || out.State != "UNKNOWN" || len(f.calls) != 1 {
			t.Errorf("%s: out=%+v err=%v calls=%d", name, out, err, len(f.calls))
		}
		// The dispatcher never dispatches an UNKNOWN operation again: reconcile is the only path and it posts nothing.
		rec, err := r.reconcile(context.Background(), dreq(goodRequest("")))
		if err != nil || rec.State != "UNKNOWN" || len(f.calls) != 1 {
			t.Errorf("%s: reconcile = %+v err=%v calls=%d", name, rec, err, len(f.calls))
		}
	}
	// A Graph 4xx error body ends FAILED_FINAL.
	r, _ = newFake(t, func(w http.ResponseWriter) { w.WriteHeader(400); _, _ = w.Write([]byte(`{"error":{"code":100}}`)) })
	sec, _ = r.assemble(goodToken(), goodUser())
	if out, _ := r.dispatch(context.Background(), dreq(goodRequest("")), sec); out != (core.Outcome{State: "FAILED_FINAL", Code: "graph_100"}) {
		t.Fatalf("4xx = %+v", out)
	}
}

func TestDispatchBadRequestNeverReachesGraph(t *testing.T) {
	r, f := newFake(t, func(w http.ResponseWriter) { _, _ = w.Write([]byte(`{"events_received":1}`)) })
	sec, _ := r.assemble(goodToken(), goodUser())
	for name, req := range map[string]string{
		"unknown key":    goodRequest(`,"x":1`),
		"wrong event id": `{"v":1,"attempt_id":"` + testAttempt + `","event_id":"lc-purchase-other","event_time":1}`,
		"bad attempt":    `{"v":1,"attempt_id":"nope","event_id":"lc-purchase-nope","event_time":1}`,
		"v2":             `{"v":2,"attempt_id":"` + testAttempt + `","event_id":"lc-purchase-` + testAttempt + `","event_time":1}`,
		"zero time":      `{"v":1,"attempt_id":"` + testAttempt + `","event_id":"lc-purchase-` + testAttempt + `","event_time":0}`,
		"bad test code":  goodRequest(`,"test_event_code":"bad code"`),
		"not json":       `x`,
	} {
		if out, err := r.dispatch(context.Background(), dreq(req), sec); err != nil || out != badRequest {
			t.Errorf("%s: out=%+v err=%v", name, out, err)
		}
	}
	if out, _ := r.dispatch(context.Background(), dreq(goodRequest("")), core.NewSecret([]byte(`{"token":"","x":1}`))); out != badRequest {
		t.Errorf("bad secret = %+v", out)
	}
	if len(f.calls) != 0 {
		t.Fatalf("bad requests reached Graph: %v", f.calls)
	}
}

func TestCheckCodesAndReconcileMode(t *testing.T) {
	var asked int
	code, askErr := "", error(nil)
	r := newRoute(nil, nil, testKey, func(context.Context, string) (string, error) { asked++; return code, askErr })
	req := dreq(goodRequest(""))
	if err := r.check(context.Background(), req); err != nil {
		t.Fatalf("allowed = %v", err)
	}
	for _, c := range []string{"consent_withdrawn", "capi_disabled", "dataset_binding_changed", "capi_principal_revoked", "environment_mismatch", "event_too_old"} {
		code = c
		err := r.check(context.Background(), req)
		var denial core.PolicyDenial
		if !errors.Is(err, core.ErrPolicyDenied) || !errors.As(err, &denial) || denial.Code != c {
			t.Errorf("%s: err = %v", c, err)
		}
	}
	// A database error is an error, never a denial (dispatcher: UNKNOWN policy_check_failed, not BLOCKED_POLICY).
	code, askErr = "", errors.New("db down")
	if err := r.check(context.Background(), req); err == nil || errors.Is(err, core.ErrPolicyDenied) {
		t.Errorf("db error = %v", err)
	}
	// Reconcile mode: nil, and the database is not even asked.
	before := asked
	req.Mode = "reconcile"
	if err := r.check(context.Background(), req); err != nil || asked != before {
		t.Errorf("reconcile mode err=%v asked=%d", err, asked-before)
	}
	// Another action is refused before any question.
	req.Mode, req.Action = "dispatch", "meta.ads.activate"
	if err := r.check(context.Background(), req); !errors.Is(err, core.ErrPolicyDenied) || asked != before {
		t.Errorf("wrong action err=%v", err)
	}
}

func TestRoutesRejectsBadConfigBeforeAnyPool(t *testing.T) {
	cfg := metaads.Config{GraphVersion: "v26.0", PartnerAgent: "lc-test"}
	if _, err := Routes(nil, cfg, nil, testKey); !errors.Is(err, ErrConfig) {
		t.Fatalf("nil pool = %v", err)
	}
}

func TestRouteShape(t *testing.T) {
	dr := testRoute(nil).dispatchRoute()
	if dr.Provider != "meta_dataset" || dr.Action != "meta.capi.purchase" || dr.Purpose != "marketing" || dr.Check == nil ||
		dr.LoadSecret == nil || dr.DispatchWithSecret == nil || dr.Reconcile == nil || dr.Dispatch != nil || dr.ReconcileWithSecret != nil {
		t.Fatalf("route = %+v", dr)
	}
}
