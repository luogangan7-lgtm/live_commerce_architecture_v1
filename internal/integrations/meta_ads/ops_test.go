package metaads

// ops_test.go: dispatch, classification, tag reconcile, reads, CAPI and host guard against an
// httptest fake Graph (MOCK evidence only: proves our request shapes and classification tables, not
// Meta's acceptance of them, which stays UNKNOWN until MA-S1).

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"

	"livecommerce/internal/integrations/core"
)

const (
	opID    = "11111111-1111-4111-8111-111111111111"
	draftID = "22222222-2222-4222-8222-222222222222"
	asset   = "123456789"
	// fakeToken is a synthetic bearer value: neutral name, no provider-shaped prefix.
	fakeToken = "tokSENTINEL0123456789"
)

type call struct {
	method, path, query, auth string
	body                      map[string]any
}

type fake struct {
	mu    sync.Mutex
	calls []call
}

func (f *fake) log() []call {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]call(nil), f.calls...)
}

// newFake starts a fake Graph whose handler answers every request; it returns a Client bound to it.
func newFake(t *testing.T, h func(c call, w http.ResponseWriter)) (*Client, *fake) {
	t.Helper()
	f := &fake{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		c := call{method: r.Method, path: r.URL.Path, query: r.URL.RawQuery, auth: r.Header.Get("Authorization")}
		if len(raw) > 0 {
			_ = json.Unmarshal(raw, &c.body)
		}
		f.mu.Lock()
		f.calls = append(f.calls, c)
		f.mu.Unlock()
		h(c, w)
	}))
	t.Cleanup(srv.Close)
	c, err := NewClient(Config{GraphBaseURL: srv.URL, GraphVersion: "v26.0", PartnerAgent: "lc-test"})
	if err != nil {
		t.Fatal(err)
	}
	return c, f
}

func reply200(body string) func(call, http.ResponseWriter) {
	return func(_ call, w http.ResponseWriter) { _, _ = io.WriteString(w, body) }
}

func status(code int, body string) func(call, http.ResponseWriter) {
	return func(_ call, w http.ResponseWriter) { w.WriteHeader(code); _, _ = io.WriteString(w, body) }
}

func dreq(action, body string) core.DispatchRequest {
	return core.DispatchRequest{OperationID: opID, Provider: "meta_ads", ExternalAssetID: asset, Purpose: "marketing",
		Action: action, Request: json.RawMessage(body), Mode: "dispatch"}
}

func tag() string { return "lc-" + opID }

const (
	campaignBody = `{"v":1,"draft_id":"` + draftID + `","attempt":1,"name":"lc-` + opID + `","objective":"OUTCOME_ENGAGEMENT","currency":"TWD","spend_cap_minor":0}`
	adsetBody    = `{"v":1,"draft_id":"` + draftID + `","attempt":1,"name":"lc-` + opID + `","campaign_id":"555","template":"BOOST_POST","currency":"TWD","lifetime_budget_minor":1234500,"start_time":"2026-10-01T00:00:00Z","end_time":"2026-10-08T00:00:00Z","countries":["TW"],"age_min":18,"age_max":65}`
	creativeBody = `{"v":1,"draft_id":"` + draftID + `","attempt":1,"name":"lc-` + opID + `","template":"BOOST_POST","page_id":"777","object_story_id":"777_888"}`
	adBody       = `{"v":1,"draft_id":"` + draftID + `","attempt":1,"name":"lc-` + opID + `","adset_id":"666","creative_id":"999"}`
	statusBody   = `{"v":1,"draft_id":"` + draftID + `","attempt":1,"seq":1,"campaign_id":"555"}`
)

func TestCreateRequestsAreExactAndPaused(t *testing.T) {
	c, f := newFake(t, reply200(`{"id":"424242"}`))
	secret := core.NewSecret([]byte(fakeToken))
	for _, tc := range []struct{ action, body, path string }{
		{ActionCreateCampaign, campaignBody, "/v26.0/act_123456789/campaigns"},
		{ActionCreateAdset, adsetBody, "/v26.0/act_123456789/adsets"},
		{ActionCreateCreative, creativeBody, "/v26.0/act_123456789/adcreatives"},
		{ActionCreateAd, adBody, "/v26.0/act_123456789/ads"},
	} {
		out, err := c.dispatch(context.Background(), dreq(tc.action, tc.body), secret)
		if err != nil || out != (core.Outcome{State: "SUCCEEDED", Code: "graph_created", ProviderReference: "424242"}) {
			t.Fatalf("%s = %+v,%v", tc.action, out, err)
		}
	}
	calls := f.log()
	if len(calls) != 4 {
		t.Fatalf("calls = %d", len(calls))
	}
	for i, want := range []string{"/v26.0/act_123456789/campaigns", "/v26.0/act_123456789/adsets", "/v26.0/act_123456789/adcreatives", "/v26.0/act_123456789/ads"} {
		if calls[i].method != "POST" || calls[i].path != want || calls[i].query != "" || calls[i].auth != "" {
			t.Fatalf("call %d = %+v (token must be in the body, never the URL/header)", i, calls[i])
		}
		if calls[i].body["access_token"] != fakeToken || calls[i].body["name"] != tag() {
			t.Fatalf("call %d body = %v", i, calls[i].body)
		}
	}
	camp := calls[0].body
	if camp["status"] != "PAUSED" || camp["objective"] != "OUTCOME_ENGAGEMENT" {
		t.Fatalf("campaign = %v", camp)
	}
	if cats, ok := camp["special_ad_categories"].([]any); !ok || len(cats) != 0 {
		t.Fatalf("special_ad_categories = %#v (must be [] not null)", camp["special_ad_categories"])
	}
	if _, has := camp["spend_cap"]; has {
		t.Fatal("spend_cap must be omitted when 0")
	}
	adset := calls[1].body
	// I05: TWD 1234500 minor is NT$12345, Meta offset 1.
	if adset["lifetime_budget"] != float64(12345) || adset["status"] != "ACTIVE" || adset["optimization_goal"] != "POST_ENGAGEMENT" ||
		adset["billing_event"] != "IMPRESSIONS" || adset["campaign_id"] != "555" {
		t.Fatalf("adset = %v", adset)
	}
	if calls[3].body["status"] != "ACTIVE" {
		t.Fatalf("ad = %v", calls[3].body)
	}
}

func TestCreateCampaignSpendCapAndTrafficGoal(t *testing.T) {
	c, f := newFake(t, reply200(`{"id":"1"}`))
	secret := core.NewSecret([]byte(fakeToken))
	body := strings.Replace(strings.Replace(campaignBody, `"currency":"TWD","spend_cap_minor":0`, `"currency":"USD","spend_cap_minor":10000`, 1), "OUTCOME_ENGAGEMENT", "OUTCOME_TRAFFIC", 1)
	if out, _ := c.dispatch(context.Background(), dreq(ActionCreateCampaign, body), secret); out.State != "SUCCEEDED" {
		t.Fatalf("out = %+v", out)
	}
	if f.log()[0].body["spend_cap"] != float64(10000) {
		t.Fatalf("spend_cap = %v", f.log()[0].body["spend_cap"])
	}
	traffic := strings.Replace(adsetBody, "BOOST_POST", "PRODUCT_TRAFFIC", 1)
	if out, _ := c.dispatch(context.Background(), dreq(ActionCreateAdset, traffic), secret); out.State != "SUCCEEDED" {
		t.Fatalf("out = %+v", out)
	}
	if got := f.log()[1].body; got["optimization_goal"] != "LINK_CLICKS" || got["billing_event"] != "IMPRESSIONS" {
		t.Fatalf("traffic goal = %v", got)
	}
}

func TestCreateCreativeShapes(t *testing.T) {
	c, f := newFake(t, reply200(`{"id":"1"}`))
	secret := core.NewSecret([]byte(fakeToken))
	ig := `{"v":1,"draft_id":"` + draftID + `","attempt":1,"name":"lc-` + opID + `","template":"BOOST_POST","page_id":"777","source_instagram_media_id":"901","instagram_user_id":"902"}`
	link := `{"v":1,"draft_id":"` + draftID + `","attempt":1,"name":"lc-` + opID + `","template":"PRODUCT_TRAFFIC","page_id":"777","link_url":"https://shop.example.test/products/abc"}`
	for _, body := range []string{ig, link} {
		if out, _ := c.dispatch(context.Background(), dreq(ActionCreateCreative, body), secret); out.State != "SUCCEEDED" {
			t.Fatalf("%s = %+v", body, out)
		}
	}
	calls := f.log()
	if calls[0].body["source_instagram_media_id"] != "901" || calls[0].body["instagram_user_id"] != "902" {
		t.Fatalf("ig = %v", calls[0].body)
	}
	spec, _ := calls[1].body["object_story_spec"].(map[string]any)
	if spec["page_id"] != "777" || spec["link_data"].(map[string]any)["link"] != "https://shop.example.test/products/abc" {
		t.Fatalf("link = %v", calls[1].body)
	}
}

// An Instagram-only store has no Page binding: ads.request_for omits page_id for BOOST_POST of an
// Instagram media (0074), and create_creative must still go out (r3 review P2; before the fix it was
// refused locally after the campaign and ad set already existed).
func TestCreateCreativeInstagramOnlyNoPage(t *testing.T) {
	c, f := newFake(t, reply200(`{"id":"1"}`))
	secret := core.NewSecret([]byte(fakeToken))
	ig := `{"v":1,"draft_id":"` + draftID + `","attempt":1,"name":"lc-` + opID + `","template":"BOOST_POST","source_instagram_media_id":"901","instagram_user_id":"902"}`
	if out, _ := c.dispatch(context.Background(), dreq(ActionCreateCreative, ig), secret); out.State != "SUCCEEDED" {
		t.Fatalf("ig-only = %+v", out)
	}
	if b := f.log()[0].body; b["source_instagram_media_id"] != "901" || b["instagram_user_id"] != "902" {
		t.Fatalf("ig-only body = %v", b)
	}
	// A Facebook boost and a product-traffic creative still need the Page id.
	for _, body := range []string{
		`{"v":1,"draft_id":"` + draftID + `","attempt":1,"name":"lc-` + opID + `","template":"BOOST_POST","object_story_id":"777_888"}`,
		`{"v":1,"draft_id":"` + draftID + `","attempt":1,"name":"lc-` + opID + `","template":"PRODUCT_TRAFFIC","link_url":"https://shop.example.test/p/a"}`,
	} {
		if out, _ := c.dispatch(context.Background(), dreq(ActionCreateCreative, body), secret); out != badRequest {
			t.Errorf("page-less %s = %+v want bad_request", body, out)
		}
	}
}

func TestBadRequestsNeverReachGraph(t *testing.T) {
	c, f := newFake(t, reply200(`{"id":"1"}`))
	secret := core.NewSecret([]byte(fakeToken))
	mut := func(body, from, to string) string { return strings.Replace(body, from, to, 1) }
	for name, tc := range map[string]struct{ action, body string }{
		"unknown key":           {ActionCreateCampaign, mut(campaignBody, `"v":1`, `"v":1,"extra":1`)},
		"wrong version":         {ActionCreateCampaign, mut(campaignBody, `"v":1`, `"v":2`)},
		"name is not the tag":   {ActionCreateCampaign, mut(campaignBody, `"name":"lc-`+opID+`"`, `"name":"other"`)},
		"unknown objective":     {ActionCreateCampaign, mut(campaignBody, "OUTCOME_ENGAGEMENT", "OUTCOME_SALES")},
		"unsupported currency":  {ActionCreateCampaign, mut(campaignBody, `"TWD"`, `"JPY"`)},
		"TWD not whole unit":    {ActionCreateAdset, mut(adsetBody, "1234500", "1234501")},
		"zero budget":           {ActionCreateAdset, mut(adsetBody, "1234500", "0")},
		"age order":             {ActionCreateAdset, mut(adsetBody, `"age_min":18`, `"age_min":66`)},
		"end before start":      {ActionCreateAdset, mut(adsetBody, "2026-10-08T00", "2026-09-01T00")},
		"bad country":           {ActionCreateAdset, mut(adsetBody, `"TW"`, `"tw"`)},
		"unknown template":      {ActionCreateAdset, mut(adsetBody, "BOOST_POST", "OTHER")},
		"bad campaign id":       {ActionCreateAdset, mut(adsetBody, `"555"`, `"5/5"`)},
		"creative: both modes":  {ActionCreateCreative, mut(creativeBody, `"object_story_id"`, `"source_instagram_media_id":"1","object_story_id"`)},
		"creative: no source":   {ActionCreateCreative, mut(creativeBody, `,"object_story_id":"777_888"`, ``)},
		"creative: bad story":   {ActionCreateCreative, mut(creativeBody, "777_888", "777")},
		"creative: http link":   {ActionCreateCreative, `{"v":1,"draft_id":"` + draftID + `","attempt":1,"name":"lc-` + opID + `","template":"PRODUCT_TRAFFIC","page_id":"777","link_url":"http://x.test/p"}`},
		"ad: bad ids":           {ActionCreateAd, mut(adBody, `"999"`, `"9 9"`)},
		"attempt 0":             {ActionCreateAd, mut(adBody, `"attempt":1`, `"attempt":0`)},
		"activate: no campaign": {ActionActivate, mut(statusBody, `"campaign_id":"555"`, `"campaign_id":""`)},
		"pause: seq 0":          {ActionPause, mut(statusBody, `"seq":1`, `"seq":0`)},
		"read: bad day":         {ActionReadInsights, `{"v":1,"draft_id":"` + draftID + `","campaign_id":"5","day":"2026-13-40"}`},
		"unknown action":        {"meta.ads.delete", statusBody},
	} {
		out, err := c.dispatch(context.Background(), dreq(tc.action, tc.body), secret)
		if err != nil || out != badRequest {
			t.Errorf("%s = %+v,%v want FAILED_FINAL bad_request", name, out, err)
		}
	}
	// Wrong provider / asset / empty token also refuse before any call.
	bad := dreq(ActionCreateCampaign, campaignBody)
	bad.Provider = "meta_dataset"
	if out, _ := c.dispatch(context.Background(), bad, secret); out != badRequest {
		t.Error("provider not checked")
	}
	bad = dreq(ActionCreateCampaign, campaignBody)
	bad.ExternalAssetID = "act_1"
	if out, _ := c.dispatch(context.Background(), bad, secret); out != badRequest {
		t.Error("asset not checked")
	}
	if out, _ := c.dispatch(context.Background(), dreq(ActionCreateCampaign, campaignBody), core.NewSecret(nil)); out != badRequest {
		t.Error("empty token not refused")
	}
	if n := len(f.log()); n != 0 {
		t.Fatalf("bad requests made %d Graph calls", n)
	}
}

func TestCreateClassificationTable(t *testing.T) {
	secret := core.NewSecret([]byte(fakeToken))
	errBody := func(code int) string {
		return `{"error":{"message":"m","type":"OAuthException","code":` + itoa(code) + `}}`
	}
	for name, tc := range map[string]struct {
		h    func(call, http.ResponseWriter)
		want core.Outcome
	}{
		"2xx with id":            {reply200(`{"id":"77"}`), core.Outcome{State: "SUCCEEDED", Code: "graph_created", ProviderReference: "77"}},
		"2xx without id":         {reply200(`{}`), unconfirmed()},
		"2xx non-numeric id":     {reply200(`{"id":"act_1"}`), unconfirmed()},
		"2xx unparseable":        {reply200(`not json`), unconfirmed()},
		"400 code 100":           {status(400, errBody(100)), failedFinal("graph_100")},
		"400 code 190":           {status(400, errBody(190)), failedFinal("graph_190")},
		"403 code 200":           {status(403, errBody(200)), failedFinal("graph_200")},
		"rate limit 4":           {status(400, errBody(4)), failedFinal("rate_limited")},
		"rate limit 17":          {status(400, errBody(17)), failedFinal("rate_limited")},
		"rate limit 613":         {status(400, errBody(613)), failedFinal("rate_limited")},
		"rate limit 80004 (429)": {status(429, errBody(80004)), failedFinal("rate_limited")},
		"4xx unparseable body":   {status(400, `oops`), unconfirmed()},
		"4xx without code":       {status(400, `{"error":{}}`), unconfirmed()},
		"5xx":                    {status(500, `{}`), unconfirmed()},
		"5xx with graph error":   {status(500, errBody(100)), unconfirmed()},
		"3xx":                    {status(302, ``), unconfirmed()},
	} {
		c, f := newFake(t, tc.h)
		out, err := c.dispatch(context.Background(), dreq(ActionCreateCampaign, campaignBody), secret)
		if err != nil || out != tc.want {
			t.Errorf("%s = %+v,%v want %+v", name, out, err, tc.want)
		}
		if n := len(f.log()); n != 1 {
			t.Errorf("%s made %d POSTs; an unconfirmed create must never be re-POSTed", name, n)
		}
	}
	// Transport failure (closed server) is UNKNOWN, not FAILED_FINAL.
	c, _ := newFake(t, reply200(`{}`))
	c.g.base = "http://127.0.0.1:1"
	if out, err := c.dispatch(context.Background(), dreq(ActionCreateCampaign, campaignBody), secret); err != nil || out != unconfirmed() {
		t.Errorf("transport = %+v,%v", out, err)
	}
}

func itoa(i int) string { return strconv.Itoa(i) }

func TestActivatePauseNeverFailedFinal(t *testing.T) {
	secret := core.NewSecret([]byte(fakeToken))
	errBody := `{"error":{"code":100,"message":"m"}}`
	for _, action := range []string{ActionActivate, ActionPause} {
		for name, tc := range map[string]struct {
			h    func(call, http.ResponseWriter)
			want core.Outcome
		}{
			"success true":    {reply200(`{"success":true}`), core.Outcome{State: "SUCCEEDED", Code: "graph_success"}},
			"success false":   {reply200(`{"success":false}`), unconfirmed()},
			"2xx other":       {reply200(`{"id":"1"}`), unconfirmed()},
			"4xx graph error": {status(400, errBody), unconfirmed()},
			"rate limited":    {status(429, `{"error":{"code":80004}}`), unconfirmed()},
			"code 613":        {status(400, `{"error":{"code":613}}`), unconfirmed()},
			"5xx":             {status(503, ``), unconfirmed()},
			"unparseable":     {reply200(`<html>`), unconfirmed()},
		} {
			c, f := newFake(t, tc.h)
			out, err := c.dispatch(context.Background(), dreq(action, statusBody), secret)
			if err != nil || out != tc.want || out.State == "FAILED_FINAL" {
				t.Errorf("%s/%s = %+v,%v want %+v", action, name, out, err, tc.want)
			}
			calls := f.log()
			want := "ACTIVE"
			if action == ActionPause {
				want = "PAUSED"
			}
			if len(calls) != 1 || calls[0].path != "/v26.0/555" || calls[0].body["status"] != want || calls[0].body["access_token"] != fakeToken {
				t.Errorf("%s/%s calls = %+v", action, name, calls)
			}
		}
	}
}

func TestReconcileCreateByTag(t *testing.T) {
	secret := core.NewSecret([]byte(fakeToken))
	page := func(after string, next bool, items ...string) string {
		var parts []string
		for _, i := range items {
			parts = append(parts, i)
		}
		doc := `{"data":[` + strings.Join(parts, ",") + `]`
		if next {
			doc += `,"paging":{"cursors":{"after":"` + after + `"},"next":"https://graph.facebook.com/next"}`
		}
		return doc + `}`
	}
	mine := `{"id":"9001","name":"` + tag() + `"}`
	other := `{"id":"1","name":"lc-someone-else"}`
	for name, tc := range map[string]struct {
		h     func(call, http.ResponseWriter)
		want  core.Outcome
		calls int
	}{
		"one match on page 1": {reply200(page("", false, other, mine)), core.Outcome{State: "SUCCEEDED", Code: "reconciled_by_tag", ProviderReference: "9001"}, 1},
		"match on page 2": {func(c call, w http.ResponseWriter) {
			if strings.Contains(c.query, "after=CUR1") {
				_, _ = io.WriteString(w, page("", false, mine))
				return
			}
			_, _ = io.WriteString(w, page("CUR1", true, other))
		}, core.Outcome{State: "SUCCEEDED", Code: "reconciled_by_tag", ProviderReference: "9001"}, 2},
		"none":       {reply200(page("", false, other)), unknown("reconcile_unproven"), 1},
		"duplicates": {reply200(page("", false, mine, `{"id":"9002","name":"`+tag()+`"}`)), unknown("duplicate_remote_objects"), 1},
		"duplicate across pages": {func(c call, w http.ResponseWriter) {
			if strings.Contains(c.query, "after=CUR1") {
				_, _ = io.WriteString(w, page("", false, `{"id":"9002","name":"`+tag()+`"}`))
				return
			}
			_, _ = io.WriteString(w, page("CUR1", true, mine))
		}, unknown("duplicate_remote_objects"), 2},
		"page cap 10": {func(c call, w http.ResponseWriter) { _, _ = io.WriteString(w, page("CURX", true, other)) }, unknown("reconcile_unproven"), 10},
		"graph 4xx":   {status(400, `{"error":{"code":190}}`), unconfirmed(), 1}, // never FAILED_FINAL in reconcile
		"unparseable": {reply200(`nope`), unconfirmed(), 1},
	} {
		c, f := newFake(t, tc.h)
		out, err := c.reconcile(context.Background(), dreq(ActionCreateCampaign, campaignBody), secret)
		if err != nil || out != tc.want {
			t.Errorf("%s = %+v,%v want %+v", name, out, err, tc.want)
		}
		calls := f.log()
		if len(calls) != tc.calls {
			t.Errorf("%s made %d calls want %d", name, len(calls), tc.calls)
		}
		for _, cl := range calls {
			if cl.method != "GET" || cl.body != nil || cl.auth != "Bearer "+fakeToken || cl.path != "/v26.0/act_123456789/campaigns" {
				t.Errorf("%s: reconcile must only GET with the header token: %+v", name, cl)
			}
		}
	}
	// Listing parents per action.
	for action, want := range map[string]struct{ body, path string }{
		ActionCreateAdset:    {adsetBody, "/v26.0/555/adsets"},
		ActionCreateCreative: {creativeBody, "/v26.0/act_123456789/adcreatives"},
		ActionCreateAd:       {adBody, "/v26.0/666/ads"},
	} {
		c, f := newFake(t, reply200(page("", false, mine)))
		out, _ := c.reconcile(context.Background(), dreq(action, want.body), secret)
		if out.State != "SUCCEEDED" || f.log()[0].path != want.path {
			t.Errorf("%s: %+v %s", action, out, f.log()[0].path)
		}
	}
}

func TestReconcileStatusGET(t *testing.T) {
	secret := core.NewSecret([]byte(fakeToken))
	for _, tc := range []struct {
		action, body string
		want         core.Outcome
	}{
		{ActionActivate, `{"status":"ACTIVE","effective_status":"ACTIVE"}`, core.Outcome{State: "SUCCEEDED", Code: "reconciled_status"}},
		{ActionActivate, `{"status":"PAUSED"}`, unknown("status_unmatched")},
		{ActionPause, `{"status":"PAUSED","effective_status":"PAUSED"}`, core.Outcome{State: "SUCCEEDED", Code: "reconciled_status"}},
		{ActionPause, `{"status":"ACTIVE"}`, unknown("status_unmatched")},
	} {
		c, f := newFake(t, reply200(tc.body))
		out, err := c.reconcile(context.Background(), dreq(tc.action, statusBody), secret)
		if err != nil || out != tc.want {
			t.Errorf("%s %s = %+v,%v", tc.action, tc.body, out, err)
		}
		cl := f.log()[0]
		if cl.method != "GET" || cl.path != "/v26.0/555" || !strings.Contains(cl.query, "fields=status%2Ceffective_status") {
			t.Errorf("call = %+v", cl)
		}
	}
	c, _ := newFake(t, status(400, `{"error":{"code":100}}`))
	if out, _ := c.reconcile(context.Background(), dreq(ActionPause, statusBody), secret); out != unconfirmed() {
		t.Errorf("rejected GET = %+v (must stay UNKNOWN)", out)
	}
}

func TestReadPreflight(t *testing.T) {
	secret := core.NewSecret([]byte(fakeToken))
	body := `{"v":1,"draft_id":"` + draftID + `","attempt":1,"seq":2}`
	c, f := newFake(t, reply200(`{"account_status":1,"currency":"TWD","timezone_name":"Asia/Taipei","funding_source":"4001","id":"act_123456789"}`))
	out, err := c.dispatch(context.Background(), dreq(ActionPreflight, body), secret)
	if err != nil || out != (core.Outcome{State: "SUCCEEDED", Code: "graph_read", ProviderReference: "v1;st=1;cur=TWD;fund=1;tz=Asia/Taipei"}) {
		t.Fatalf("out = %+v,%v", out, err)
	}
	if cl := f.log()[0]; cl.method != "GET" || cl.path != "/v26.0/act_123456789" || cl.auth != "Bearer "+fakeToken || strings.Contains(cl.query, fakeToken) {
		t.Fatalf("call = %+v", cl)
	}
	// Unfunded: funding_source absent (F10) -> fund=0.
	c, _ = newFake(t, reply200(`{"account_status":1,"currency":"TWD","timezone_name":"Asia/Taipei"}`))
	if out, _ := c.dispatch(context.Background(), dreq(ActionPreflight, body), secret); !strings.Contains(out.ProviderReference, ";fund=0;") {
		t.Fatalf("out = %+v", out)
	}
	c, _ = newFake(t, status(400, `{"error":{"code":190}}`))
	if out, _ := c.dispatch(context.Background(), dreq(ActionPreflight, body), secret); out != failedFinal("graph_190") {
		t.Fatalf("read 4xx = %+v", out)
	}
	c, _ = newFake(t, reply200(`{"currency":"TWD"}`))
	if out, _ := c.dispatch(context.Background(), dreq(ActionPreflight, body), secret); out != unconfirmed() {
		t.Fatalf("missing account_status = %+v", out)
	}
	c, _ = newFake(t, reply200(`{"account_status":1,"currency":"twd","timezone_name":"Asia/Taipei"}`))
	if out, _ := c.dispatch(context.Background(), dreq(ActionPreflight, body), secret); out != failedFinal("bad_result") {
		t.Fatalf("bad currency = %+v", out)
	}
	// Reconcile of a read repeats the read.
	c, f = newFake(t, reply200(`{"account_status":2,"currency":"USD","timezone_name":"America/Los_Angeles","funding_source":"1"}`))
	out, _ = c.reconcile(context.Background(), dreq(ActionPreflight, body), secret)
	if out.State != "SUCCEEDED" || len(f.log()) != 1 || f.log()[0].method != "GET" {
		t.Fatalf("reconcile read = %+v", out)
	}
}

func TestReadInsights(t *testing.T) {
	secret := core.NewSecret([]byte(fakeToken))
	body := `{"v":1,"draft_id":"` + draftID + `","campaign_id":"555","day":"2026-10-02"}`
	handler := func(insights string) func(call, http.ResponseWriter) {
		return func(c call, w http.ResponseWriter) {
			switch {
			case strings.HasSuffix(c.path, "/insights"):
				_, _ = io.WriteString(w, insights)
			case c.path == "/v26.0/act_123456789":
				_, _ = io.WriteString(w, `{"currency":"TWD","timezone_name":"Asia/Taipei","id":"act_123456789"}`)
			default:
				_, _ = io.WriteString(w, `{"effective_status":"ACTIVE","id":"555"}`)
			}
		}
	}
	c, f := newFake(t, handler(`{"data":[{"spend":"12.3","impressions":"900","clicks":"14","actions":[{"action_type":"link_click","value":"14"},{"action_type":"omni_purchase","value":"2"}],"action_values":[{"action_type":"omni_purchase","value":"1999.5"}]}]}`))
	out, err := c.dispatch(context.Background(), dreq(ActionReadInsights, body), secret)
	want := "v1;es=ACTIVE;sp=12.30;im=900;cl=14;pu=2;pv=1999.50;cur=TWD;tz=Asia/Taipei"
	if err != nil || out != (core.Outcome{State: "SUCCEEDED", Code: "graph_read", ProviderReference: want}) {
		t.Fatalf("out = %+v,%v", out, err)
	}
	calls := f.log()
	if len(calls) != 3 {
		t.Fatalf("calls = %d", len(calls))
	}
	ins := calls[2]
	if ins.path != "/v26.0/555/insights" || !strings.Contains(ins.query, "time_range=") || !strings.Contains(ins.query, "2026-10-02") {
		t.Fatalf("insights call = %+v", ins)
	}
	// Nothing delivered: no data row -> zeros and "na" purchases.
	c, _ = newFake(t, handler(`{"data":[]}`))
	out, _ = c.dispatch(context.Background(), dreq(ActionReadInsights, body), secret)
	if out.ProviderReference != "v1;es=ACTIVE;sp=0.00;im=0;cl=0;pu=na;pv=na;cur=TWD;tz=Asia/Taipei" {
		t.Fatalf("empty = %+v", out)
	}
	// Spend with more than 2 fraction digits is never rounded.
	c, _ = newFake(t, handler(`{"data":[{"spend":"12.345","impressions":"1","clicks":"1"}]}`))
	if out, _ := c.dispatch(context.Background(), dreq(ActionReadInsights, body), secret); out != failedFinal("bad_spend") {
		t.Fatalf("bad spend = %+v", out)
	}
	c, _ = newFake(t, handler(`{"data":[{"spend":"1","impressions":"x","clicks":"1"}]}`))
	if out, _ := c.dispatch(context.Background(), dreq(ActionReadInsights, body), secret); out != failedFinal("bad_result") {
		t.Fatalf("bad impressions = %+v", out)
	}
	c, _ = newFake(t, handler(`{"data":[{},{}]}`))
	if out, _ := c.dispatch(context.Background(), dreq(ActionReadInsights, body), secret); out != unconfirmed() {
		t.Fatalf("two rows = %+v", out)
	}
	// Rate limited read is a rejected read: FAILED_FINAL rate_limited, the planner plans the next seq.
	c, _ = newFake(t, status(429, `{"error":{"code":80004}}`))
	if out, _ := c.dispatch(context.Background(), dreq(ActionReadInsights, body), secret); out != failedFinal("rate_limited") {
		t.Fatalf("rate limited = %+v", out)
	}
}

func TestPostEvent(t *testing.T) {
	event := []byte(`{"data":[{"event_name":"Purchase","event_id":"lc-purchase-x"}],"test_event_code":"TEST1234"}`)
	c, f := newFake(t, reply200(`{"events_received":1,"messages":[],"fbtrace_id":"AbC_123"}`))
	out, err := c.PostEvent(context.Background(), []byte(fakeToken), "31337", event)
	if err != nil || out != (core.Outcome{State: "SUCCEEDED", Code: "graph_received", ProviderReference: "AbC_123"}) {
		t.Fatalf("out = %+v,%v", out, err)
	}
	cl := f.log()[0]
	if cl.method != "POST" || cl.path != "/v26.0/31337/events" || cl.query != "" || cl.body["partner_agent"] != "lc-test" ||
		cl.body["access_token"] != fakeToken || cl.body["test_event_code"] != "TEST1234" {
		t.Fatalf("call = %+v", cl)
	}
	for name, tc := range map[string]struct {
		h    func(call, http.ResponseWriter)
		want core.Outcome
	}{
		"received 0":       {reply200(`{"events_received":0}`), unconfirmed()},
		"received 2":       {reply200(`{"events_received":2}`), unconfirmed()},
		"2xx unparseable":  {reply200(`x`), unconfirmed()},
		"4xx error":        {status(400, `{"error":{"code":100}}`), failedFinal("graph_100")},
		"4xx rate limited": {status(400, `{"error":{"code":17}}`), failedFinal("rate_limited")},
		"5xx":              {status(500, `{}`), unconfirmed()},
		"4xx unparseable":  {status(400, `x`), unconfirmed()},
	} {
		c, f := newFake(t, tc.h)
		out, err := c.PostEvent(context.Background(), []byte(fakeToken), "31337", event)
		if err != nil || out != tc.want || len(f.log()) != 1 {
			t.Errorf("%s = %+v,%v calls=%d want %+v", name, out, err, len(f.log()), tc.want)
		}
	}
	// Bad arguments never reach Graph.
	c, f = newFake(t, reply200(`{}`))
	for name, args := range map[string]struct {
		token []byte
		pixel string
		body  string
	}{
		"empty token": {nil, "1", string(event)}, "bad pixel": {[]byte(fakeToken), "1/2", string(event)},
		"not json": {[]byte(fakeToken), "1", `x`}, "no data": {[]byte(fakeToken), "1", `{}`},
		"two events": {[]byte(fakeToken), "1", `{"data":[{},{}]}`},
	} {
		if out, err := c.PostEvent(context.Background(), args.token, args.pixel, []byte(args.body)); err != nil || out != badRequest {
			t.Errorf("%s = %+v,%v", name, out, err)
		}
	}
	if len(f.log()) != 0 {
		t.Fatal("bad arguments reached Graph")
	}
	noPartner, _ := NewClient(Config{GraphVersion: "v26.0"})
	if _, err := noPartner.PostEvent(context.Background(), []byte(fakeToken), "1", event); err == nil {
		t.Fatal("PostEvent without PartnerAgent must be a configuration error")
	}
}

func TestHostGuardAndRedirects(t *testing.T) {
	for _, cfg := range []Config{
		{GraphBaseURL: "https://evil.example.test", GraphVersion: "v26.0"},
		{GraphBaseURL: "http://graph.facebook.com", GraphVersion: "v26.0"},
		{GraphBaseURL: "https://graph.facebook.com/", GraphVersion: "v26.0"},
		{GraphBaseURL: "https://graph.facebook.com.evil.test", GraphVersion: "v26.0"},
		{GraphBaseURL: "http://localhost:8080", GraphVersion: "v26.0"},
		{GraphBaseURL: "http://127.0.0.1:8080/x", GraphVersion: "v26.0"},
		{GraphBaseURL: "http://127.0.0.1.evil.test:80", GraphVersion: "v26.0"},
		{GraphBaseURL: GraphHost}, {GraphBaseURL: GraphHost, GraphVersion: "26.0"}, {GraphBaseURL: GraphHost, GraphVersion: "v26"},
		{GraphBaseURL: GraphHost, GraphVersion: "v26.0", PartnerAgent: "bad agent"},
	} {
		if _, err := NewClient(cfg); err == nil {
			t.Errorf("accepted %+v", cfg)
		}
	}
	for _, cfg := range []Config{{GraphVersion: "v26.0"}, {GraphBaseURL: GraphHost, GraphVersion: "v26.0", PartnerAgent: "lc"}, {GraphBaseURL: "http://127.0.0.1:9", GraphVersion: "v26.0"}} {
		if _, err := NewClient(cfg); err != nil {
			t.Errorf("rejected %+v", cfg)
		}
	}
	// A redirect is never followed: the token-bearing POST must not be replayed elsewhere.
	var hits int
	target := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { hits++ }))
	defer target.Close()
	c, _ := newFake(t, func(_ call, w http.ResponseWriter) { w.Header().Set("Location", target.URL); w.WriteHeader(307) })
	out, _ := c.dispatch(context.Background(), dreq(ActionCreateCampaign, campaignBody), core.NewSecret([]byte(fakeToken)))
	if out != unconfirmed() || hits != 0 {
		t.Fatalf("redirect: out=%+v hits=%d", out, hits)
	}
	// Oversized response bodies are unusable (UNKNOWN), not buffered without bound.
	c, _ = newFake(t, func(_ call, w http.ResponseWriter) {
		_, _ = w.Write([]byte(`{"id":"1","pad":"` + strings.Repeat("a", maxBody) + `"}`))
	})
	if out, _ := c.dispatch(context.Background(), dreq(ActionCreateCampaign, campaignBody), core.NewSecret([]byte(fakeToken))); out != unconfirmed() {
		t.Fatalf("oversize = %+v", out)
	}
}
