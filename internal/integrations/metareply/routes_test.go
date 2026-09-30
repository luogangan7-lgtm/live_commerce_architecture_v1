package metareply

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"livecommerce/internal/claims"
	"livecommerce/internal/integrations/core"
)

const (
	opT     = "77777777-7777-4777-8777-777777777777"
	bundleT = "44444444-4444-4444-8444-444444444444"
)

type harness struct {
	t        *testing.T
	linkKey  claims.ReplyLinkKey
	keys     *PageTokenKeyring
	checkFn  checkFunc
	checked  atomic.Int32
	lastHash []byte
}

func newHarness(t *testing.T) *harness {
	k, _ := claims.NewReplyLinkKey(bytes32(5))
	return &harness{t: t, linkKey: k, keys: testKeyring(t)}
}

func (h *harness) routes(cfg Config) map[string]core.DispatchRoute {
	h.t.Helper()
	check := func(ctx context.Context, op string, hash []byte) (string, error) {
		h.checked.Add(1)
		h.lastHash = hash
		return h.checkFn(ctx, op, hash)
	}
	routes, err := newRoutes(check, h.linkKey, h.keys, cfg)
	if err != nil {
		h.t.Fatal(err)
	}
	out := map[string]core.DispatchRoute{}
	for _, r := range routes {
		out[r.Provider] = r
	}
	return out
}

func (h *harness) request(provider string, mutate func(map[string]any)) core.DispatchRequest {
	body := map[string]any{"v": 1, "platform": provider, "source_id": storeT, "asset_id": "1234567890", "comment_ref": "1111_2222",
		"bundle_id": bundleT, "session_id": storeT, "link_generation": 1, "link_key_id": h.linkKey.ID(), "locale": "en",
		"template": "claim-link/v1", "policy": "mpr-policy/v1", "message_type": "first_private_reply", "takeover_generation": 0,
		"origin_ref": storeT, "origin": "https://shop.example.com", "deadline_at": "2030-01-01T00:00:00Z", "live_media": false}
	if mutate != nil {
		mutate(body)
	}
	raw, _ := json.Marshal(body)
	return core.DispatchRequest{OperationID: opT, TenantID: tenantT, StoreID: storeT, PrincipalID: storeT, BindingID: bindingT,
		BindingVersion: 1, Provider: provider, ExternalAssetID: "1234567890", Purpose: "service", Action: "meta.private_reply",
		Request: raw, IdempotencyKey: "lc:" + opT}
}

func (h *harness) expectedToken() claims.LinkToken {
	tok, err := claims.SystemLinkToken(h.linkKey, tenantT, storeT, bundleT, opT)
	if err != nil {
		h.t.Fatal(err)
	}
	return tok
}

func TestRoutesConfigValidation(t *testing.T) {
	h := newHarness(t)
	h.checkFn = func(context.Context, string, []byte) (string, error) { return "OK", nil }
	ok := Config{GraphBaseURL: "https://graph.facebook.com", GraphVersion: "v23.0"}
	if got := h.routes(ok); len(got) != 2 || got["facebook"].Action != "meta.private_reply" || got["instagram"].Purpose != "service" {
		t.Fatalf("routes = %+v", got)
	}
	if got := h.routes(Config{GraphBaseURL: "http://127.0.0.1:8080", GraphVersion: "v9.10"}); len(got) != 2 {
		t.Fatal("loopback mock rejected")
	}
	for name, cfg := range map[string]Config{
		"no version":     {GraphBaseURL: "https://graph.facebook.com"},
		"bad version":    {GraphBaseURL: "https://graph.facebook.com", GraphVersion: "23.0"},
		"long version":   {GraphBaseURL: "https://graph.facebook.com", GraphVersion: "v1234.0"},
		"other host":     {GraphBaseURL: "https://graph.instagram.com", GraphVersion: "v23.0"},
		"http graph":     {GraphBaseURL: "http://graph.facebook.com", GraphVersion: "v23.0"},
		"trailing slash": {GraphBaseURL: "https://graph.facebook.com/", GraphVersion: "v23.0"},
		"localhost name": {GraphBaseURL: "http://localhost:8080", GraphVersion: "v23.0"},
		"empty base":     {GraphVersion: "v23.0"},
	} {
		if _, err := newRoutes(func(context.Context, string, []byte) (string, error) { return "OK", nil }, h.linkKey, h.keys, cfg); !errors.Is(err, ErrConfig) {
			t.Fatalf("%s accepted: %v", name, err)
		}
	}
	if _, err := newRoutes(nil, h.linkKey, h.keys, ok); err == nil {
		t.Fatal("nil check accepted")
	}
	if _, err := newRoutes(func(context.Context, string, []byte) (string, error) { return "OK", nil }, claims.ReplyLinkKey{}, h.keys, ok); err == nil {
		t.Fatal("zero link key accepted")
	}
	if _, err := newRoutes(func(context.Context, string, []byte) (string, error) { return "OK", nil }, h.linkKey, nil, ok); err == nil {
		t.Fatal("nil keyring accepted")
	}
	if _, err := Routes(nil, h.linkKey, h.keys, ok); !errors.Is(err, ErrConfig) {
		t.Fatalf("nil pool: %v", err)
	}
	// The routes must be acceptable to the dispatcher's own shape rules (secret pair, no Dispatch).
	for _, r := range h.routes(ok) {
		if r.Dispatch != nil || r.LoadSecret == nil || r.DispatchWithSecret == nil || r.Check == nil || r.Reconcile == nil {
			t.Fatalf("route shape: %+v", r)
		}
	}
}

func TestCheckMapping(t *testing.T) {
	h := newHarness(t)
	route := h.routes(Config{GraphBaseURL: "https://graph.facebook.com", GraphVersion: "v23.0"})["facebook"]
	run := func(mutate func(map[string]any)) error {
		return route.Check(context.Background(), h.request("facebook", mutate))
	}
	h.checkFn = func(context.Context, string, []byte) (string, error) { return "OK", nil }
	if err := run(nil); err != nil {
		t.Fatalf("OK denied: %v", err)
	}
	want := sha256.Sum256([]byte(h.expectedToken()))
	if string(h.lastHash) != string(want[:]) {
		t.Fatal("Check did not pass sha256 of the re-derived token")
	}
	for _, code := range []string{"deadline", "source_off", "principal_revoked", "link_invalid", "live_closed"} {
		h.checkFn = func(context.Context, string, []byte) (string, error) { return code, nil }
		if err := run(nil); !errors.Is(err, core.ErrPolicyDenied) || !strings.HasPrefix(err.Error(), code+":") {
			t.Fatalf("%s -> %v", code, err)
		}
	}
	h.checkFn = func(context.Context, string, []byte) (string, error) { return "mystery", nil }
	if err := run(nil); err == nil || errors.Is(err, core.ErrPolicyDenied) {
		t.Fatalf("unknown code must be a plain error: %v", err)
	}
	h.checkFn = func(context.Context, string, []byte) (string, error) { return "", errors.New("connection reset") }
	if err := run(nil); err == nil || errors.Is(err, core.ErrPolicyDenied) || strings.Contains(err.Error(), "connection") {
		t.Fatalf("infrastructure error must be a plain, opaque error: %v", err)
	}
	// Key rotation between plan and dispatch: deny without touching SQL.
	before := h.checked.Load()
	if err := run(func(m map[string]any) { m["link_key_id"] = "0000000000000000" }); !errors.Is(err, core.ErrPolicyDenied) {
		t.Fatalf("key id mismatch: %v", err)
	}
	if h.checked.Load() != before {
		t.Fatal("key id mismatch reached SQL")
	}
	for name, mutate := range map[string]func(map[string]any){
		"version":  func(m map[string]any) { m["v"] = 2 },
		"template": func(m map[string]any) { m["template"] = "other" },
		"asset":    func(m map[string]any) { m["asset_id"] = "999" },
		"bundle":   func(m map[string]any) { m["bundle_id"] = "x" },
	} {
		h.checkFn = func(context.Context, string, []byte) (string, error) { return "OK", nil }
		if err := run(mutate); err == nil || errors.Is(err, core.ErrPolicyDenied) {
			t.Fatalf("malformed request (%s) must be a plain error: %v", name, err)
		}
	}
}

type graphCall struct {
	path, query, auth, contentType string
	body                           map[string]any
	rawBody                        string
}

func graphServer(t *testing.T, status int, response string, calls *[]graphCall) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var body map[string]any
		_ = json.Unmarshal(raw, &body)
		*calls = append(*calls, graphCall{path: r.URL.Path, query: r.URL.RawQuery, auth: r.Header.Get("Authorization"),
			contentType: r.Header.Get("Content-Type"), body: body, rawBody: string(raw)})
		w.WriteHeader(status)
		_, _ = io.WriteString(w, response)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestDispatchSuccessBodyAndTokenPlacement(t *testing.T) {
	for _, header := range []bool{false, true} {
		var calls []graphCall
		srv := graphServer(t, 200, `{"recipient_id":"555","message_id":"m_abc123"}`, &calls)
		h := newHarness(t)
		route := h.routes(Config{GraphBaseURL: srv.URL, GraphVersion: "v23.0", AuthorizationHeader: header})["instagram"]
		out, err := route.DispatchWithSecret(context.Background(), h.request("instagram", nil), core.NewSecret([]byte(fakeTok)))
		if err != nil || out != (core.Outcome{State: "SUCCEEDED", Code: "graph_sent", ProviderReference: "m_abc123"}) {
			t.Fatalf("header=%v out=%+v err=%v", header, out, err)
		}
		if len(calls) != 1 {
			t.Fatalf("calls = %d", len(calls))
		}
		c := calls[0]
		if c.path != "/v23.0/1234567890/messages" || c.query != "" || c.contentType != "application/json" {
			t.Fatalf("call = %+v", c)
		}
		recipient := c.body["recipient"].(map[string]any)
		message := c.body["message"].(map[string]any)
		text := message["text"].(string)
		wantText, _ := RenderClaimLink("en", "https://shop.example.com", h.expectedToken())
		if recipient["comment_id"] != "1111_2222" || text != wantText || len(recipient) != 1 || len(message) != 1 {
			t.Fatalf("body = %s", c.rawBody)
		}
		if header {
			if c.auth != "Bearer "+fakeTok || c.body["access_token"] != nil || strings.Contains(c.rawBody, fakeTok) {
				t.Fatalf("header mode: auth=%q body=%s", c.auth, c.rawBody)
			}
		} else if c.auth != "" || c.body["access_token"] != fakeTok || strings.Contains(c.path+c.query, fakeTok) {
			t.Fatalf("body mode: auth=%q body=%s", c.auth, c.rawBody)
		}
	}
}

func TestDispatchNeverRepostsOnDoubt(t *testing.T) {
	huge := `{"message_id":"m1","pad":"` + strings.Repeat("a", 70<<10) + `"}`
	cases := map[string]struct {
		status   int
		response string
	}{
		"400":            {400, `{"error":{"code":100}}`},
		"400 with id":    {400, `{"message_id":"m1"}`},
		"500 with id":    {500, `{"message_id":"m1"}`},
		"300 with id":    {300, `{"message_id":"m1"}`},
		"401":            {401, `{"error":{"code":190}}`},
		"403":            {403, ``},
		"429":            {429, `{}`},
		"500":            {500, `boom`},
		"503":            {503, ``},
		"302":            {302, ``},
		"200 garbled":    {200, `not json`},
		"200 empty":      {200, ``},
		"200 no id":      {200, `{"recipient_id":"1"}`},
		"200 empty id":   {200, `{"message_id":""}`},
		"200 numeric id": {200, `{"message_id":5}`},
		"200 long id":    {200, `{"message_id":"` + strings.Repeat("a", 201) + `"}`},
		"200 ctrl id":    {200, `{"message_id":"a\nb"}`},
		"200 oversize":   {200, huge},
		"201 ok shape":   {201, `{"message_id":"m1"}`},
	}
	for name, c := range cases {
		var calls []graphCall
		srv := graphServer(t, c.status, c.response, &calls)
		h := newHarness(t)
		route := h.routes(Config{GraphBaseURL: srv.URL, GraphVersion: "v23.0"})["facebook"]
		out, err := route.DispatchWithSecret(context.Background(), h.request("facebook", nil), core.NewSecret([]byte(fakeTok)))
		if len(calls) != 1 {
			t.Fatalf("%s: %d POSTs", name, len(calls))
		}
		if name == "201 ok shape" {
			if err != nil || out.State != "SUCCEEDED" {
				t.Fatalf("%s: %+v %v", name, out, err)
			}
			continue
		}
		if err != nil || out != (core.Outcome{State: "UNKNOWN", Code: "graph_unconfirmed"}) {
			t.Fatalf("%s: %+v %v", name, out, err)
		}
	}
}

func TestDispatchRedirectNotFollowed(t *testing.T) {
	var hits atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { hits.Add(1) }))
	defer target.Close()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusTemporaryRedirect)
	}))
	defer srv.Close()
	h := newHarness(t)
	// Even an injected client that follows redirects must be overridden.
	route := h.routes(Config{GraphBaseURL: srv.URL, GraphVersion: "v23.0", HTTPClient: &http.Client{}})["facebook"]
	out, err := route.DispatchWithSecret(context.Background(), h.request("facebook", nil), core.NewSecret([]byte(fakeTok)))
	if err != nil || out.Code != "graph_unconfirmed" || hits.Load() != 0 {
		t.Fatalf("redirect: %+v %v hits=%d", out, err, hits.Load())
	}
}

func TestDispatchTimeoutIsUnknown(t *testing.T) {
	release := make(chan struct{})
	var count atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		count.Add(1)
		<-release
	}))
	defer srv.Close()
	defer close(release)
	h := newHarness(t)
	route := h.routes(Config{GraphBaseURL: srv.URL, GraphVersion: "v23.0"})["facebook"]
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	out, err := route.DispatchWithSecret(ctx, h.request("facebook", nil), core.NewSecret([]byte(fakeTok)))
	if err != nil || out != (core.Outcome{State: "UNKNOWN", Code: "graph_unconfirmed"}) || count.Load() != 1 {
		t.Fatalf("timeout: %+v %v calls=%d", out, err, count.Load())
	}
}

func TestDispatchRefusesBeforeAnyCall(t *testing.T) {
	var calls []graphCall
	srv := graphServer(t, 200, `{"message_id":"m"}`, &calls)
	h := newHarness(t)
	route := h.routes(Config{GraphBaseURL: srv.URL, GraphVersion: "v23.0"})["facebook"]
	secret := core.NewSecret([]byte(fakeTok))
	for name, mutate := range map[string]func(map[string]any){
		"key id":       func(m map[string]any) { m["link_key_id"] = "0000000000000000" },
		"locale":       func(m map[string]any) { m["locale"] = "fr" },
		"origin":       func(m map[string]any) { m["origin"] = "http://evil.example.com" },
		"comment ref":  func(m map[string]any) { m["comment_ref"] = "" },
		"comment ctrl": func(m map[string]any) { m["comment_ref"] = "a\nb" },
		"template":     func(m map[string]any) { m["template"] = "x" },
		"message type": func(m map[string]any) { m["message_type"] = "x" },
	} {
		out, err := route.DispatchWithSecret(context.Background(), h.request("facebook", mutate), secret)
		if err == nil || out != (core.Outcome{}) {
			t.Fatalf("%s: %+v %v", name, out, err)
		}
		if strings.Contains(err.Error(), fakeTok) || strings.Contains(err.Error(), string(h.expectedToken())) {
			t.Fatalf("%s: error leaks a credential", name)
		}
	}
	if _, err := route.DispatchWithSecret(context.Background(), h.request("facebook", nil), core.Secret{}); err == nil {
		t.Fatal("empty secret dispatched")
	}
	if len(calls) != 0 {
		t.Fatalf("%d calls before validation passed", len(calls))
	}
}

func TestReconcileNeverDispatches(t *testing.T) {
	var calls []graphCall
	srv := graphServer(t, 200, `{"message_id":"m"}`, &calls)
	h := newHarness(t)
	route := h.routes(Config{GraphBaseURL: srv.URL, GraphVersion: "v23.0"})["facebook"]
	out, err := route.Reconcile(context.Background(), h.request("facebook", nil))
	if err != nil || out != (core.Outcome{State: "UNKNOWN", Code: "reconcile_unproven"}) || len(calls) != 0 {
		t.Fatalf("reconcile: %+v %v calls=%d", out, err, len(calls))
	}
}

// fakeTx serves exactly one load_meta_page_token row (or none) through pgx.Tx.QueryRow.
type fakeTx struct {
	pgx.Tx
	row    []any // nil = no rows; error = query failure
	err    error
	gotSQL string
	gotArg []any
}

type fakeRow struct {
	values []any
	err    error
}

func (r fakeRow) Scan(dest ...any) error {
	if r.err != nil {
		return r.err
	}
	if len(dest) != len(r.values) {
		return fmt.Errorf("scan width %d != %d", len(dest), len(r.values))
	}
	for i, v := range r.values {
		switch d := dest[i].(type) {
		case *string:
			*d = v.(string)
		case *int64:
			*d = v.(int64)
		case *[]byte:
			*d = v.([]byte)
		case *[]string:
			*d = v.([]string)
		default:
			return fmt.Errorf("unexpected dest %T", d)
		}
	}
	return nil
}

func (f *fakeTx) QueryRow(_ context.Context, sql string, args ...any) pgx.Row {
	f.gotSQL, f.gotArg = sql, args
	if f.err != nil {
		return fakeRow{err: f.err}
	}
	if f.row == nil {
		return fakeRow{err: pgx.ErrNoRows}
	}
	return fakeRow{values: f.row}
}

func TestLoadSecretScopesAndOpen(t *testing.T) {
	h := newHarness(t)
	routes := h.routes(Config{GraphBaseURL: "https://graph.facebook.com", GraphVersion: "v23.0"})
	claim := core.SecretClaim{OperationID: opT, Generation: 2, LeaseToken: bytes32(9)}
	sealed := func(provider string, version int64) (keyID string, nonce, ct []byte) {
		id, n, c, err := h.keys.Seal(PageTokenScope{TenantID: tenantT, StoreID: storeT, BindingID: bindingT, Provider: provider, AssetID: "1234567890", Version: version}, fakeTok)
		if err != nil {
			t.Fatal(err)
		}
		return id, n, c
	}
	row := func(provider string, version int64, scopes []string) []any {
		id, n, c := sealed(provider, version)
		return []any{tenantT, storeT, bindingT, provider, "1234567890", version, id, n, c, scopes}
	}

	// facebook: pages_messaging required.
	tx := &fakeTx{row: row("facebook", 4, []string{"pages_messaging", "extra"})}
	s, err := routes["facebook"].LoadSecret(context.Background(), tx, claim)
	if err != nil || string(s.Reveal()) != fakeTok {
		t.Fatalf("facebook load: %v", err)
	}
	if !strings.Contains(tx.gotSQL, "integration.load_meta_page_token") || len(tx.gotArg) != 3 || tx.gotArg[0] != opT || tx.gotArg[1] != int64(2) {
		t.Fatalf("loader call: %s %v", tx.gotSQL, tx.gotArg)
	}
	// instagram needs both scopes.
	if _, err := routes["instagram"].LoadSecret(context.Background(), &fakeTx{row: row("instagram", 1, []string{"instagram_manage_comments", "pages_read_engagement"})}, claim); err != nil {
		t.Fatalf("instagram load: %v", err)
	}
	denied := map[string]*fakeTx{
		"no row":               {},
		"fb missing scope":     {row: row("facebook", 1, []string{"pages_read_engagement"})},
		"fb empty scopes":      {row: row("facebook", 1, nil)},
		"ig one scope":         {row: row("instagram", 1, []string{"instagram_manage_comments"})},
		"ig wrong scope":       {row: row("instagram", 1, []string{"pages_messaging"})},
		"provider mismatch fb": {row: row("instagram", 1, []string{"pages_messaging", "instagram_manage_comments", "pages_read_engagement"})},
	}
	for name, tx := range denied {
		route := routes["facebook"]
		if strings.HasPrefix(name, "ig") {
			route = routes["instagram"]
		}
		if _, err := route.LoadSecret(context.Background(), tx, claim); !errors.Is(err, core.ErrPolicyDenied) {
			t.Fatalf("%s: %v", name, err)
		}
	}
	// A loader failure or an undecryptable row is a plain error (UNKNOWN secret_load_failed), not a denial.
	for name, tx := range map[string]*fakeTx{
		"sql error":   {err: errors.New("40001 lease conflict")},
		"wrong ver":   {row: func() []any { r := row("facebook", 4, []string{"pages_messaging"}); r[5] = int64(5); return r }()},
		"wrong asset": {row: func() []any { r := row("facebook", 4, []string{"pages_messaging"}); r[4] = "999"; return r }()},
	} {
		if _, err := routes["facebook"].LoadSecret(context.Background(), tx, claim); err == nil || errors.Is(err, core.ErrPolicyDenied) {
			t.Fatalf("%s: %v", name, err)
		}
	}
}
