package metaads

// oauth_routes_test.go: OAuth.Connect against a fake Graph, seal-key loading, redaction and the route
// table. The seal/open round trip lives in tokenopen's tests (metaads must not import tokenopen).

import (
	"context"
	"crypto/ecdh"
	"crypto/hpke"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"livecommerce/internal/ads"
	"livecommerce/internal/integrations/core"
)

const (
	tenantID = "33333333-3333-4333-8333-333333333333"
	storeID  = "44444444-4444-4444-8444-444444444444"
	// appSecretSentinel is a synthetic value, neutrally named: leak assertions search for it.
	appSecretSentinel = "appSECRETsentinel0123456789abcdef"
	codeSentinel      = "authCODEsentinel0123"
)

func pubKeysEnv(t *testing.T) (func(string) string, hpke.PrivateKey) {
	t.Helper()
	priv, err := hpke.DHKEM(ecdh.X25519()).GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	doc, _ := json.Marshal(map[string]any{"keys": []map[string]string{
		{"id": "k1", "public_key_base64": base64.StdEncoding.EncodeToString(priv.PublicKey().Bytes())}}})
	env := map[string]string{envPublicKeys: string(doc), envActiveKeyID: "k1"}
	return func(k string) string { return env[k] }, priv
}

func TestLoadSealKeys(t *testing.T) {
	getenv, _ := pubKeysEnv(t)
	keys, err := LoadSealKeys(getenv)
	if err != nil {
		t.Fatal(err)
	}
	sealed, err := keys.Seal(ads.SealInfo{TenantID: tenantID, StoreID: storeID}, []byte("tok123"))
	if err != nil || sealed.KeyID != "k1" || len(sealed.Enc) != EncSize || len(sealed.Ciphertext) != len("tok123")+16 {
		t.Fatalf("sealed = %d/%d %v", len(sealed.Enc), len(sealed.Ciphertext), err)
	}
	if strings.Contains(fmt.Sprintf("%v %+v %#v %s", keys, keys, keys, keys), "k1") {
		t.Fatal("SealKeys formatter leaks")
	}
	if _, err := keys.Seal(ads.SealInfo{TenantID: "x", StoreID: storeID}, []byte("tok")); err == nil {
		t.Fatal("bad tenant sealed")
	}
	if _, err := keys.Seal(ads.SealInfo{TenantID: tenantID, StoreID: storeID}, []byte("has space")); err == nil {
		t.Fatal("bad token sealed")
	}
	valid := getenv(envPublicKeys)
	pub := base64.StdEncoding.EncodeToString(make([]byte, 32))
	for name, env := range map[string]map[string]string{
		"missing keys":     {envActiveKeyID: "k1"},
		"missing active":   {envPublicKeys: valid},
		"active not found": {envPublicKeys: valid, envActiveKeyID: "k2"},
		"unknown member":   {envPublicKeys: strings.Replace(valid, `"id"`, `"x":1,"id"`, 1), envActiveKeyID: "k1"},
		"duplicate member": {envPublicKeys: strings.Replace(valid, `"id":"k1"`, `"id":"k1","id":"k1"`, 1), envActiveKeyID: "k1"},
		"duplicate id":     {envPublicKeys: `{"keys":[{"id":"k1","public_key_base64":"` + pub + `"},{"id":"k1","public_key_base64":"` + pub + `"}]}`, envActiveKeyID: "k1"},
		"short key":        {envPublicKeys: `{"keys":[{"id":"k1","public_key_base64":"AAAA"}]}`, envActiveKeyID: "k1"},
		"trailing":         {envPublicKeys: valid + `{}`, envActiveKeyID: "k1"},
		"bad id":           {envPublicKeys: strings.ReplaceAll(valid, "k1", "k 1"), envActiveKeyID: "k 1"},
	} {
		env := env
		if _, err := LoadSealKeys(func(k string) string { return env[k] }); !errors.Is(err, ErrConfig) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if _, err := LoadSealKeys(nil); !errors.Is(err, ErrConfig) {
		t.Error("nil getenv")
	}
}

// oauthGraph is a fake Meta that serves the whole connect chain; hooks override single steps.
func oauthGraph(t *testing.T, override func(path string) (int, string, bool)) (*OAuth, *fake, func(string) string) {
	t.Helper()
	getenv, _ := pubKeysEnv(t)
	keys, err := LoadSealKeys(getenv)
	if err != nil {
		t.Fatal(err)
	}
	f := &fake{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		f.mu.Lock()
		f.calls = append(f.calls, call{method: r.Method, path: r.URL.Path, query: r.URL.RawQuery, auth: r.Header.Get("Authorization"), body: map[string]any{"len": len(raw)}})
		f.mu.Unlock()
		if override != nil {
			if code, body, ok := override(r.URL.Path); ok {
				w.WriteHeader(code)
				_, _ = io.WriteString(w, body)
				return
			}
		}
		switch r.URL.Path {
		case "/v26.0/oauth/access_token":
			_, _ = io.WriteString(w, `{"access_token":"bisuTOKEN0123456789","token_type":"bearer"}`)
		case "/v26.0/me":
			_, _ = io.WriteString(w, `{"client_business_id":"5550001","id":"9"}`)
		case "/v26.0/me/permissions":
			_, _ = io.WriteString(w, `{"data":[{"permission":"ads_management","status":"granted"},{"permission":"ads_read","status":"granted"},{"permission":"business_management","status":"declined"},{"permission":"BAD PERM","status":"granted"}]}`)
		case "/v26.0/me/adaccounts":
			if strings.Contains(r.URL.RawQuery, "after=P2") {
				_, _ = io.WriteString(w, `{"data":[{"account_id":"222","name":"Second","currency":"USD","timezone_name":"America/Los_Angeles","account_status":2}]}`)
				return
			}
			_, _ = io.WriteString(w, `{"data":[{"account_id":"111","name":"First 大梦\u0007","currency":"TWD","timezone_name":"Asia/Taipei","account_status":1},{"account_id":"bad id","name":"x"}],"paging":{"cursors":{"after":"P2"},"next":"https://graph.facebook.com/x"}}`)
		case "/v26.0/act_111/adspixels":
			_, _ = io.WriteString(w, `{"data":[{"id":"701","name":"Pixel A"}]}`)
		case "/v26.0/act_222/adspixels":
			_, _ = io.WriteString(w, `{"data":[{"id":"701","name":"Pixel A"},{"id":"702","name":"Pixel B"}]}`)
		default:
			w.WriteHeader(404)
		}
	}))
	t.Cleanup(srv.Close)
	o, err := NewOAuth(Config{GraphBaseURL: srv.URL, GraphVersion: "v26.0"},
		AppConfig{AppID: "4291253377792879", RedirectURI: "https://admin.example.test/api/ads/meta/callback", AppSecret: []byte(appSecretSentinel)}, keys)
	if err != nil {
		t.Fatal(err)
	}
	return o, f, getenv
}

func TestOAuthConnect(t *testing.T) {
	o, f, _ := oauthGraph(t, nil)
	res, err := o.Connect(context.Background(), codeSentinel, ads.SealInfo{TenantID: tenantID, StoreID: storeID})
	if err != nil {
		t.Fatal(err)
	}
	if res.ClientBusinessID != "5550001" || strings.Join(res.Scopes, ",") != "ads_management,ads_read" {
		t.Fatalf("res = %+v", res)
	}
	want := []ads.Pick{
		{Kind: "ad_account", ID: "111", Name: "First 大梦", Currency: "TWD", Timezone: "Asia/Taipei", AccountStatus: 1},
		{Kind: "ad_account", ID: "222", Name: "Second", Currency: "USD", Timezone: "America/Los_Angeles", AccountStatus: 2},
		{Kind: "dataset", ID: "701", Name: "Pixel A"},
		{Kind: "dataset", ID: "702", Name: "Pixel B"}, // 701 listed twice: deduped
	}
	if len(res.Picks) != len(want) {
		t.Fatalf("picks = %+v", res.Picks)
	}
	for i := range want {
		if res.Picks[i] != want[i] {
			t.Errorf("pick %d = %+v want %+v", i, res.Picks[i], want[i])
		}
	}
	if res.Token.KeyID != "k1" || len(res.Token.Enc) != 32 || len(res.Token.Ciphertext) != len("bisuTOKEN0123456789")+16 {
		t.Fatalf("token = %d/%d", len(res.Token.Enc), len(res.Token.Ciphertext))
	}
	// Order of calls (G6) and where secrets travel.
	var order []string
	for _, c := range f.log() {
		order = append(order, c.path)
		if strings.Contains(c.query, "bisuTOKEN") {
			t.Errorf("token in URL: %+v", c)
		}
		if c.path != "/v26.0/oauth/access_token" && c.auth != "Bearer bisuTOKEN0123456789" {
			t.Errorf("token not in header: %+v", c)
		}
	}
	wantOrder := "/v26.0/oauth/access_token /v26.0/me /v26.0/me/permissions /v26.0/me/adaccounts /v26.0/me/adaccounts /v26.0/act_111/adspixels /v26.0/act_222/adspixels"
	if got := strings.Join(order, " "); got != wantOrder {
		t.Fatalf("order:\n got %s\nwant %s", got, wantOrder)
	}
	exch := f.log()[0]
	for _, part := range []string{"client_id=4291253377792879", "client_secret=" + appSecretSentinel, "code=" + codeSentinel, "redirect_uri="} {
		if !strings.Contains(exch.query, part) {
			t.Errorf("exchange query lacks %q: %s", part, exch.query)
		}
	}
	// The plaintext token must not appear anywhere in the result (only its HPKE seal).
	blob, _ := json.Marshal(res)
	if strings.Contains(string(blob), "bisuTOKEN") {
		t.Fatal("plaintext token in result")
	}
}

func TestOAuthConnectFailsClosed(t *testing.T) {
	info := ads.SealInfo{TenantID: tenantID, StoreID: storeID}
	for name, path := range map[string]string{
		"exchange": "/v26.0/oauth/access_token", "me": "/v26.0/me", "permissions": "/v26.0/me/permissions",
		"adaccounts": "/v26.0/me/adaccounts", "adspixels": "/v26.0/act_111/adspixels",
	} {
		for kind, resp := range map[string]struct {
			code int
			body string
		}{"http 500": {500, `{}`}, "graph error": {400, `{"error":{"code":190,"message":"` + appSecretSentinel + `"}}`}, "garbage": {200, `not json`}} {
			path, resp := path, resp
			o, _, _ := oauthGraph(t, func(p string) (int, string, bool) { return resp.code, resp.body, p == path })
			res, err := o.Connect(context.Background(), codeSentinel, info)
			if !errors.Is(err, ads.ErrConnectFailed) || len(res.Picks) != 0 || len(res.Token.Ciphertext) != 0 || res.ClientBusinessID != "" {
				t.Errorf("%s/%s: res=%+v err=%v (no partial result allowed)", name, kind, res, err)
			}
			if err != nil && (strings.Contains(err.Error(), appSecretSentinel) || strings.Contains(err.Error(), codeSentinel)) {
				t.Errorf("%s/%s: error leaks a secret: %v", name, kind, err)
			}
		}
	}
	// Semantically bad but well-formed bodies also fail.
	for name, tc := range map[string]struct{ path, body string }{
		"empty access_token":   {"/v26.0/oauth/access_token", `{"access_token":""}`},
		"token with space":     {"/v26.0/oauth/access_token", `{"access_token":"a b"}`},
		"non-numeric business": {"/v26.0/me", `{"client_business_id":"abc"}`},
		"missing business":     {"/v26.0/me", `{}`},
	} {
		tc := tc
		o, _, _ := oauthGraph(t, func(p string) (int, string, bool) { return 200, tc.body, p == tc.path })
		if _, err := o.Connect(context.Background(), codeSentinel, info); !errors.Is(err, ads.ErrConnectFailed) {
			t.Errorf("%s: %v", name, err)
		}
	}
	// Bad inputs never reach Graph.
	o, f, _ := oauthGraph(t, nil)
	for name, tc := range map[string]struct {
		code string
		info ads.SealInfo
	}{
		"empty code": {"", info}, "space in code": {"a b", info}, "bad tenant": {codeSentinel, ads.SealInfo{TenantID: "x", StoreID: storeID}},
		"bad store": {codeSentinel, ads.SealInfo{TenantID: tenantID, StoreID: ""}}, "long code": {strings.Repeat("a", 2049), info},
	} {
		if _, err := o.Connect(context.Background(), tc.code, tc.info); !errors.Is(err, ads.ErrConnectFailed) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if len(f.log()) != 0 {
		t.Fatal("invalid input reached Graph")
	}
	var nilOAuth *OAuth
	if _, err := nilOAuth.Connect(context.Background(), codeSentinel, info); !errors.Is(err, ads.ErrConnectFailed) {
		t.Error("nil OAuth")
	}
}

func TestOAuthAccountCapAndRedaction(t *testing.T) {
	// 30 accounts on one page: only the first 20 get picks and a pixel query.
	var items []string
	for i := 1; i <= 30; i++ {
		items = append(items, fmt.Sprintf(`{"account_id":"%d","name":"A%d","currency":"TWD","timezone_name":"Asia/Taipei","account_status":1}`, 1000+i, i))
	}
	o, f, _ := oauthGraph(t, func(p string) (int, string, bool) {
		switch {
		case p == "/v26.0/me/adaccounts":
			return 200, `{"data":[` + strings.Join(items, ",") + `]}`, true
		case strings.HasSuffix(p, "/adspixels"):
			return 200, `{"data":[]}`, true
		}
		return 0, "", false
	})
	res, err := o.Connect(context.Background(), codeSentinel, ads.SealInfo{TenantID: tenantID, StoreID: storeID})
	if err != nil || len(res.Picks) != maxAccounts {
		t.Fatalf("picks = %d,%v", len(res.Picks), err)
	}
	pixels := 0
	for _, c := range f.log() {
		if strings.HasSuffix(c.path, "/adspixels") {
			pixels++
		}
	}
	if pixels != maxAccounts {
		t.Fatalf("pixel queries = %d", pixels)
	}
	for _, v := range []any{o, *o, o.app, AppConfig{AppSecret: []byte(appSecretSentinel)}} {
		for _, s := range []string{fmt.Sprintf("%v", v), fmt.Sprintf("%+v", v), fmt.Sprintf("%#v", v), fmt.Sprintf("%s", v)} {
			if strings.Contains(s, appSecretSentinel) || strings.Contains(s, "4291253377792879") {
				t.Errorf("formatter leaks: %s", s)
			}
		}
		if b, err := json.Marshal(v); err != nil || strings.Contains(string(b), appSecretSentinel) {
			t.Errorf("json leaks: %s %v", b, err)
		}
	}
	secretCfg := AppConfig{AppSecret: []byte(appSecretSentinel)}
	if b, _ := secretCfg.MarshalText(); strings.Contains(string(b), appSecretSentinel) {
		t.Error("MarshalText leaks")
	}
	// Oversized pick lists are trimmed from the end (datasets first) to stay inside the 16 KiB CHECK.
	big := make([]ads.Pick, 0, 400)
	for i := 0; i < 400; i++ {
		big = append(big, ads.Pick{Kind: "dataset", ID: fmt.Sprint(100000 + i), Name: strings.Repeat("大", pickNameRunes)})
	}
	fit := fitPicks(big)
	if raw, _ := json.Marshal(fit); len(raw) > pickListBudget || len(fit) == 0 || len(fit) == len(big) {
		t.Fatalf("fit = %d picks, %d bytes", len(fit), len(raw))
	}
}

func TestNewOAuthValidation(t *testing.T) {
	getenv, _ := pubKeysEnv(t)
	keys, _ := LoadSealKeys(getenv)
	cfg := Config{GraphVersion: "v26.0"}
	good := AppConfig{AppID: "4291253377792879", RedirectURI: "https://admin.example.test/cb", AppSecret: []byte(appSecretSentinel)}
	if _, err := NewOAuth(cfg, good, keys); err != nil {
		t.Fatal(err)
	}
	for name, mut := range map[string]func(*AppConfig){
		"app id":        func(a *AppConfig) { a.AppID = "abc" },
		"empty secret":  func(a *AppConfig) { a.AppSecret = nil },
		"http redirect": func(a *AppConfig) { a.RedirectURI = "http://admin.example.test/cb" },
		"userinfo":      func(a *AppConfig) { a.RedirectURI = "https://u:p@admin.example.test/cb" },
		"fragment":      func(a *AppConfig) { a.RedirectURI = "https://admin.example.test/cb#x" },
		"no host":       func(a *AppConfig) { a.RedirectURI = "https:///cb" },
	} {
		a := good
		mut(&a)
		if _, err := NewOAuth(cfg, a, keys); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
	if _, err := NewOAuth(cfg, good, nil); err == nil {
		t.Error("nil keys accepted")
	}
	if _, err := NewOAuth(Config{GraphBaseURL: "https://evil.example.test", GraphVersion: "v26.0"}, good, keys); err == nil {
		t.Error("bad host accepted")
	}
	// The secret is copied: mutating the caller's slice does not change the client.
	secret := []byte(appSecretSentinel)
	a := good
	a.AppSecret = secret
	o, _ := NewOAuth(cfg, a, keys)
	secret[0] = 'X'
	if o.app.AppSecret[0] == 'X' {
		t.Error("secret aliased")
	}
}

type fakeOpener struct {
	plain []byte
	err   error
	got   [3]string
}

func (f *fakeOpener) Open(tenant, store, keyID string, enc, ct []byte) ([]byte, error) {
	f.got = [3]string{tenant, store, keyID}
	return append([]byte(nil), f.plain...), f.err
}

func TestOpenRow(t *testing.T) {
	row := tokenRow{tenant: tenantID, store: storeID, provider: ProviderAds, keyID: "k1", scopes: []string{"ads_management", "pages_show_list"}}
	op := &fakeOpener{plain: []byte("bisuTOKEN")}
	secret, err := openRow(ProviderAds, op, row)
	if err != nil || string(secret.Reveal()) != "bisuTOKEN" || op.got != [3]string{tenantID, storeID, "k1"} {
		t.Fatalf("secret,err = %v %v %v", secret, err, op.got)
	}
	// Denials are policy denials (dispatch: BLOCKED_POLICY zero HTTP; reconcile: credential_unavailable).
	for name, r := range map[string]tokenRow{
		"page token row": {tenant: tenantID, store: storeID, provider: "facebook", keyID: "k1", scopes: row.scopes},
		"dataset row":    {tenant: tenantID, store: storeID, provider: ProviderDataset, keyID: "k1", scopes: row.scopes},
		"missing scope":  {tenant: tenantID, store: storeID, provider: ProviderAds, keyID: "k1", scopes: []string{"ads_read"}},
		"no scopes":      {tenant: tenantID, store: storeID, provider: ProviderAds, keyID: "k1"},
	} {
		if _, err := openRow(ProviderAds, op, r); !errors.Is(err, core.ErrPolicyDenied) {
			t.Errorf("%s: %v", name, err)
		}
	}
	// Dataset needs both scopes.
	oneScope := tokenRow{provider: ProviderDataset, scopes: []string{"ads_management"}}
	if _, err := openRow(ProviderDataset, op, oneScope); !errors.Is(err, core.ErrPolicyDenied) {
		t.Error("dataset with one scope accepted")
	}
	// An open failure is a plain error (dispatcher: secret_load_failed), never a policy denial, and
	// its text carries nothing from the key ring.
	bad := &fakeOpener{err: errors.New("hpke: secret detail")}
	if _, err := openRow(ProviderAds, bad, row); err == nil || errors.Is(err, core.ErrPolicyDenied) || strings.Contains(err.Error(), "secret detail") {
		t.Errorf("open failure = %v", err)
	}
	if _, err := openRow(ProviderAds, nil, row); !errors.Is(err, core.ErrPolicyDenied) {
		t.Error("nil opener")
	}
}

func TestRoutesTable(t *testing.T) {
	check := func(context.Context, core.DispatchRequest) error { return nil }
	routes, err := newRoutes(Config{GraphVersion: "v26.0"}, &fakeOpener{}, check)
	if err != nil || len(routes) != 8 {
		t.Fatalf("routes = %d,%v", len(routes), err)
	}
	seen := map[string]bool{}
	for _, r := range routes {
		seen[r.Action] = true
		if r.Provider != "meta_ads" || r.Purpose != "marketing" || r.Check == nil || r.LoadSecret == nil ||
			r.DispatchWithSecret == nil || r.ReconcileWithSecret == nil || r.Dispatch != nil || r.Reconcile != nil {
			t.Errorf("route %s malformed: %+v", r.Action, r)
		}
	}
	for _, a := range []string{"meta.ads.create_campaign", "meta.ads.create_adset", "meta.ads.create_creative", "meta.ads.create_ad",
		"meta.ads.preflight_account", "meta.ads.activate", "meta.ads.pause", "meta.ads.read_insights"} {
		if !seen[a] {
			t.Errorf("missing route %s", a)
		}
	}
	for name, tc := range map[string]struct {
		cfg   Config
		keys  TokenOpener
		check func(context.Context, core.DispatchRequest) error
	}{
		"nil keys": {Config{GraphVersion: "v26.0"}, nil, check}, "nil check": {Config{GraphVersion: "v26.0"}, &fakeOpener{}, nil},
		"bad host":   {Config{GraphBaseURL: "https://evil.example.test", GraphVersion: "v26.0"}, &fakeOpener{}, check},
		"no version": {Config{}, &fakeOpener{}, check},
	} {
		if _, err := newRoutes(tc.cfg, tc.keys, tc.check); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
	if _, err := Routes(nil, Config{GraphVersion: "v26.0"}, &fakeOpener{}, check); !errors.Is(err, ErrConfig) {
		t.Error("nil pool")
	}
}
