//go:build browser

package foundation_test

// MA09a (contracts/meta-ads-v1.md §9 row MA09, admin half; §2 flow steps 1-9, §5.1 statuses, §5.3 X7, §7, §12; ads-ui U1-U8;
// ruling O4 allowance off, O6, AD9 SANDBOX). Real browsers.
//
// Stack (label BROWSER, Meta = MOCK): an isolated PG 18, the real private Go API (identity + admin routes incl. the ads surface,
// ads.Service over the real metaads.OAuth against the fake Graph on loopback), the real ads worker (dispatcher with
// metaads.Routes + ads sweepers on River queue `ads`), the production admin Next build with a signed mock IdP. The Facebook
// Login dialog (https://www.facebook.com/...) is answered INSIDE the browser by page.route (a 302 to the app's own /api/ads/meta/callback);
// the code exchange, /me, permissions, ad accounts and datasets are answered by the fake Graph from the API process. Node never
// receives a database credential or a Meta token; a runner-only control listener registers OAuth codes, drives the sweepers, flips the
// operator settings and seeds insights.
//
// Spec: tests/admin/ads.spec.ts (Playwright; zh-TW + zh-CN + en, desktop 1586x992 + 390 px, screenshots hashed). Evidence:
// output/playwright/meta-ads/<timestamp>/. Not proven here (MOCK): Meta enum validity, review, delivery, real login, App Review.

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"livecommerce/internal/httpapi"
	"livecommerce/internal/identity"
	"livecommerce/internal/identityhttp"
	"livecommerce/internal/oidclogin"
	"livecommerce/internal/platform"
	"livecommerce/tests/ads/fakegraph"
)

func mabRequire(t *testing.T) {
	t.Helper()
	if os.Getenv("LC_BROWSER_META_ADS_ACCEPTANCE") != "1" || os.Getenv("LC_TEST_DATABASE_ALLOWED") != "1" {
		t.Fatal("use scripts/dev/test-local.sh --browser-meta-ads")
	}
}

// mabStartAdmin is brfStartAdmin over an arbitrary fixture with the ads service mounted.
func mabStartAdmin(t *testing.T, ctx context.Context, f *testFixture, principal, evidence string, options httpapi.Options) *brfStack {
	t.Helper()
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := listener.Addr().String()
	_ = listener.Close()
	_, port, _ := net.SplitHostPort(address)
	origin := "http://" + address
	idp := newBrowserIDP(t, origin+"/api/auth/callback")
	mustExec(t, f.owner, `INSERT INTO identity.external_identities(issuer,subject,principal_id) VALUES($1,'browser-subject',$2)`, idp.server.URL, principal)
	role := "mab_" + strings.ReplaceAll(randomUUID(), "-", "")
	password := randomToken()
	mustExec(t, f.owner, `CREATE ROLE `+pgx.Identifier{role}.Sanitize()+` LOGIN NOSUPERUSER NOBYPASSRLS NOCREATEDB NOCREATEROLE IN ROLE commerce_identity PASSWORD '`+password+`'`)
	t.Cleanup(func() { mustExec(t, f.owner, `DROP ROLE `+pgx.Identifier{role}.Sanitize()) })
	u, err := url.Parse(f.databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	u.User = url.UserPassword(role, password)
	authority, err := platform.OpenIdentityPool(ctx, u.String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(authority.Close)
	provider, err := oidclogin.New(ctx, oidclogin.Config{Issuer: idp.server.URL, ClientID: browserClientID, RedirectURL: idp.redirect, AllowLoopbackForTests: true})
	if err != nil {
		t.Fatal(err)
	}
	service, err := identity.New(authority, observedBrowserProvider{Provider: provider, t: t}, identity.Policy{ProviderKey: "browser-meta-ads-v1", SessionTTL: time.Hour, OnboardingEnabled: true, Currencies: []string{"TWD", "USD"}})
	if err != nil {
		t.Fatal(err)
	}
	bffKey := randomToken()
	private, err := identityhttp.NewHandler(service, bffKey)
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	mux.Handle("/v1/identity/", private)
	mux.Handle("/", httpapi.NewHandler(f.runtime, options))
	api := httptest.NewServer(mux)
	t.Cleanup(api.Close)
	nextLog := browserLog(t, filepath.Join(evidence, "next.log"))
	next := exec.CommandContext(ctx, "node", filepath.Join(root, "apps/admin/.next/standalone/apps/admin/server.js"))
	next.Dir = root
	next.Env = browserEnvironment(map[string]string{"HOSTNAME": "127.0.0.1", "PORT": port, "NODE_ENV": "production", "COMMERCE_IDENTITY_ENABLED": "1", "COMMERCE_IDENTITY_ALLOW_LOOPBACK_TESTS": "1",
		"COMMERCE_PUBLIC_ORIGIN": origin, "COMMERCE_API_ORIGIN": api.URL, "COMMERCE_OIDC_ISSUER": idp.server.URL, "COMMERCE_BFF_KEY": bffKey, "COMMERCE_ONBOARDING_ENABLED": "1", "COMMERCE_ONBOARDING_CURRENCIES": "TWD,USD"})
	next.Stdout, next.Stderr = nextLog, nextLog
	next.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := next.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = syscall.Kill(-next.Process.Pid, syscall.SIGKILL); _, _ = next.Process.Wait() })
	client := &http.Client{Timeout: time.Second}
	for attempt := 0; ; attempt++ {
		if response, err := client.Get(origin + "/api/stores"); err == nil {
			_ = response.Body.Close()
			if response.StatusCode == http.StatusUnauthorized {
				break
			}
		}
		if attempt > 100 {
			t.Fatalf("admin Next readiness failed; evidence=%s", evidence)
		}
		time.Sleep(100 * time.Millisecond)
	}
	return &brfStack{root: root, evidence: evidence, origin: origin, api: api}
}

// driveSafe alternates the advance sweeper and the dispatcher until nothing is left to plan or run (error-returning: it runs on
// the control listener's goroutines, where t.Fatal is not allowed).
func (e *adsEnv) driveSafe(rounds int) error {
	for i := 0; i < rounds; i++ {
		before := e.opCount()
		if err := e.sweepErr("advance"); err != nil {
			return err
		}
		if err := e.settleErr(); err != nil {
			return err
		}
		if e.opCount() == before && i > 0 {
			return nil
		}
	}
	return nil
}

func (e *adsEnv) opCount() int64 {
	var n int64
	_ = e.f.owner.QueryRow(e.ctx, `SELECT count(*) FROM integration.operations WHERE store_id=$1 AND provider IN ('meta_ads','meta_dataset')`, e.store).Scan(&n)
	return n
}

func TestBrowserMetaAds(t *testing.T) {
	mabRequire(t)
	ctx, cancel := context.WithTimeout(context.Background(), 24*time.Minute)
	defer cancel()
	e := newAdsEnv(t, adsOpts{noConnect: true})
	root, _ := filepath.Abs("../..")
	evidence := brfEvidence(t, root, "meta-ads")

	var codes int
	controlKey := randomToken()
	control := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Gate-Key") != controlKey {
			http.Error(w, "forbidden", 403)
			return
		}
		fail := func(err error) {
			http.Error(w, err.Error(), 500)
		}
		switch r.URL.Path {
		case "/oauth/code": // a fresh single-use authorization code that exchanges for the BISU token
			codes++
			code := fmt.Sprintf("SYNTH-CODE-%d-%s", codes, t04Tag())
			e.g.AddCode(code, e.botToken)
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]string{"code": code})
			return
		case "/oauth/expire":
			if _, err := e.f.owner.Exec(ctx, `UPDATE ads.oauth_states SET created_at=created_at-interval '2 hours',expires_at=expires_at-interval '2 hours' WHERE store_id=$1 AND used_at IS NULL`, e.store); err != nil {
				fail(err)
				return
			}
		case "/drive":
			if err := e.driveSafe(14); err != nil {
				fail(err)
				return
			}
		case "/sweep/insights":
			for _, k := range []string{"insights", "advance"} {
				if err := e.sweepErr(k); err != nil {
					fail(err)
					return
				}
				if err := e.settleErr(); err != nil {
					fail(err)
					return
				}
			}
		case "/seed/insights":
			var campaign string
			if err := e.f.owner.QueryRow(ctx, `SELECT r.remote_id FROM ads.remote_objects r WHERE r.store_id=$1 AND r.kind='campaign' AND r.remote_id IS NOT NULL ORDER BY r.publish_attempt LIMIT 1`, e.store).Scan(&campaign); err != nil {
				fail(err)
				return
			}
			for i := -2; i <= 0; i++ {
				e.g.SetInsights(campaign, taipeiDay(i), fakegraph.Insights{Spend: "12.30", Impressions: "1000", Clicks: "9", PurchaseCount: "2", PurchaseValue: "45.90"})
			}
			// disclosed fixture: two days of history before the reads (the draft starts in the future when the UI creates it);
			// pinned to Taipei day -2, not shifted by 2 days: the UI start is now+3h..5h, which crosses midnight after 21:00.
			if err := e.ownerReplicaBestEffort(adsStartDayMinus2+` WHERE store_id=$1 AND publish_attempt>0 AND ended_at IS NULL`, e.store); err != nil {
				fail(err)
				return
			}
		case "/allowance/off", "/allowance/on":
			amount := int64(0)
			if r.URL.Path == "/allowance/on" {
				amount = adsDefaultAllow
			}
			env := "SANDBOX"
			var cur string
			if err := e.f.owner.QueryRow(ctx, `SELECT environment FROM ads.store_settings WHERE store_id=$1`, e.store).Scan(&cur); err == nil {
				env = cur
			}
			if _, err := e.registrarPool.Exec(ctx, `SELECT ads.operator_set_settings($1::uuid,$2::uuid,$3,$4,$5::bigint,'TWD')`, e.tenant, e.store, env, e.account, amount); err != nil {
				fail(err)
				return
			}
		case "/env/live", "/env/sandbox":
			env := "SANDBOX"
			if r.URL.Path == "/env/live" {
				env = "LIVE"
			}
			if _, err := e.registrarPool.Exec(ctx, `SELECT ads.operator_set_settings($1::uuid,$2::uuid,$3,$4,$5::bigint,'TWD')`, e.tenant, e.store, env, e.account, int64(adsDefaultAllow)); err != nil {
				fail(err)
				return
			}
		case "/graph":
			type row struct{ ID, Name, Status, Effective string }
			violations := append([]string{}, e.g.Violations()...) // never JSON null
			out := map[string]any{"violations": violations, "oauth_exchanges": e.g.Count(fakegraph.RouteOAuthToken), "status_posts": e.g.Count(fakegraph.RouteStatusPost)}
			for _, kind := range []string{"campaign", "adset", "creative", "ad"} {
				var rows []row
				for _, o := range e.g.Objects(kind) {
					rows = append(rows, row{o.ID, o.Name, o.Status, e.g.EffectiveStatus(o.ID)})
				}
				out[kind] = rows
			}
			var budgets []string
			for _, r := range e.g.Requests() {
				if r.Route == fakegraph.RouteCreateAdset {
					budgets = append(budgets, string(r.Body))
				}
			}
			out["adset_bodies"] = budgets
			var tokens []string
			for _, r := range e.g.Requests() {
				tokens = append(tokens, r.TokenSource)
			}
			out["token_sources"] = tokens
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(out)
			return
		default:
			http.NotFound(w, r)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(control.Close)

	stack := mabStartAdmin(t, ctx, e.f, e.creator, evidence, httpapi.Options{SessionStoreList: true, Ads: e.svc})
	env := map[string]string{
		"LC_BROWSER_STORE": e.store, "LC_BROWSER_ACCOUNT": e.account, "LC_BROWSER_PAGE_ASSET": e.pageAsset, "LC_BROWSER_CLIENT_BUSINESS": e.clientBiz,
		"LC_BROWSER_CONTROL": control.URL, "LC_BROWSER_CONTROL_KEY": controlKey, "LC_BROWSER_APP_ID": adsApp, "LC_BROWSER_CONFIG_ID": adsConfigID,
		"LC_BROWSER_REDIRECT": adsRedirect, "LC_BROWSER_GRAPH_VERSION": adsVersion, "LC_BROWSER_SECRETS": mustJSON(t, []string{e.botToken, adsAppSecret}),
	}
	brfPlaywright(t, ctx, stack, []string{"ads.spec.ts"}, env)

	// ---- PG facts after the UI drove the real commands ----
	count := func(q string, args ...any) int { return countRows(t, e.f.owner, q, args...) }
	if n := count(`SELECT count(*) FROM ads.oauth_states WHERE store_id=$1 AND used_at IS NOT NULL AND pending_ciphertext IS NULL AND client_business_id=$2`, e.store, e.clientBiz); n < 1 {
		t.Errorf("MA09a: no consumed OAuth state with the client business and the pending ciphertext deleted after the bind (%d); evidence=%s", n, evidence)
	}
	if n := count(`SELECT count(*) FROM integration.bindings WHERE store_id=$1 AND provider='meta_ads' AND external_asset_id=$2 AND enabled`, e.store, e.account); n != 1 {
		t.Errorf("MA09a: %d meta_ads bindings for the picked account, want exactly 1", n)
	}
	if n := count(`SELECT count(*) FROM integration.meta_page_credentials c JOIN integration.bindings b ON b.id=c.binding_id WHERE b.store_id=$1 AND c.provider='meta_ads' AND octet_length(c.nonce)=32`, e.store); n < 1 {
		t.Errorf("MA09a: no HPKE-sealed credential row for the connected account")
	}
	if n := count(`SELECT count(*) FROM ads.connections WHERE store_id=$1 AND client_business_id=$2`, e.store, e.clientBiz); n != 1 {
		t.Errorf("MA09a: %d ads.connections rows, want 1", n)
	}
	var drafts, approved int
	_ = e.f.owner.QueryRow(ctx, `SELECT count(*),count(*) FILTER (WHERE EXISTS (SELECT 1 FROM ads.draft_approvals a WHERE a.draft_id=d.id AND a.revision=d.revision)) FROM ads.campaign_drafts d WHERE d.store_id=$1`, e.store).Scan(&drafts, &approved)
	if drafts < 2 || approved < 1 {
		t.Errorf("MA09a: %d drafts (%d approved): the UI must have created the draft and its copy", drafts, approved)
	}
	if n := count(`SELECT count(*) FROM ads.remote_objects r JOIN integration.operations o ON o.id=r.operation_id WHERE r.store_id=$1 AND r.kind='pause' AND o.state='SUCCEEDED'`, e.store); n < 1 {
		t.Errorf("MA09a: no SUCCEEDED pause from the UI")
	}
	if n := count(`SELECT count(*) FROM ads.remote_objects r JOIN integration.operations o ON o.id=r.operation_id WHERE r.store_id=$1 AND r.kind='activate' AND o.state='SUCCEEDED'`, e.store); n != 1 {
		t.Errorf("MA09a: %d SUCCEEDED activates, want exactly 1 (the copy was never published)", n)
	}
	for action, min := range map[string]int{"ads.meta.connect_started": 1, "ads.meta.connected": 1, "ads.meta.bound": 1, "ads.draft.approved": 1} {
		if n := count(`SELECT count(*) FROM ops.audit_events WHERE store_id=$1 AND action=$2 AND principal_id=$3`, e.store, action, e.creator); n < min {
			t.Errorf("MA09a: audit %s missing (%d)", action, n)
		}
	}
	if v := e.g.Violations(); len(v) != 0 {
		t.Errorf("MA09a: token->account isolation violations: %v", v)
	}
	for _, r := range e.g.Requests() {
		if r.TokenSource == "query" || strings.Contains(r.RawQuery, "access_token") && r.Route != fakegraph.RouteOAuthToken {
			t.Errorf("MA09a: token in a URL: %s", r.Path)
		}
	}
	// no plaintext BISU token or app secret in the tables the connect flow wrote
	for _, table := range []string{"ads.oauth_states", "ads.connections", "integration.meta_page_credentials", "ops.audit_events", "ops.command_results"} {
		for name, needle := range map[string]string{"BISU token": e.botToken, "app secret": adsAppSecret} {
			if n := count(`SELECT count(*) FROM `+table+` t WHERE t::text LIKE '%'||$1||'%'`, needle); n != 0 {
				t.Errorf("MA09a: %s in %s", name, table)
			}
		}
	}
	brfShots(t, evidence, 12)
	t.Logf("MA09a BROWSER (Meta = MOCK): admin half passed; evidence=%s", evidence)
}

// TestBrowserMetaAdsConsent is MA09b: buyer consent -> CAPI browser context, through the production storefront Next build, the real
// buyerhttp handler and PG (tests/storefront/ads-consent.mjs). The runner-only control listener answers the questions only Go may
// answer (the context row, its stored user agent, consent_allows) and runs the real CAPI sweeper.
func TestBrowserMetaAdsConsent(t *testing.T) {
	mabRequire(t)
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Minute)
	defer cancel()
	const storefrontOrigin = "https://buyer.example"
	c := newCapiEnv(t, adsOpts{origin: storefrontOrigin})
	root, _ := filepath.Abs("../..")
	evidence := brfEvidence(t, root, "meta-ads-consent")
	controlKey := randomToken()
	control := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Gate-Key") != controlKey {
			http.Error(w, "forbidden", 403)
			return
		}
		rows := func() (n int, ua string) {
			_ = c.f.owner.QueryRow(ctx, `SELECT count(*),coalesce(max(user_agent),'') FROM ads.capi_contexts WHERE store_id=$1`, c.store).Scan(&n, &ua)
			return
		}
		switch r.URL.Path {
		case "/context/none":
			if n, _ := rows(); n != 0 {
				http.Error(w, fmt.Sprintf("%d CAPI context rows, want none", n), http.StatusConflict)
				return
			}
		case "/context/one":
			n, ua := rows()
			if n != 1 {
				http.Error(w, fmt.Sprintf("%d CAPI context rows, want exactly one", n), http.StatusConflict)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]string{"user_agent": ua})
			return
		case "/sweep":
			if err := c.sweepErr("capi"); err != nil {
				http.Error(w, err.Error(), 500)
				return
			}
			if err := c.settleErr(); err != nil {
				http.Error(w, err.Error(), 500)
				return
			}
		case "/consent/denied":
			var owner string
			if err := c.f.owner.QueryRow(ctx, `SELECT owner_id::text FROM customers.consent_events WHERE store_id=$1 AND purpose='ads_personalization' ORDER BY occurred_at DESC,id DESC LIMIT 1`, c.store).Scan(&owner); err != nil {
				http.Error(w, err.Error(), 500)
				return
			}
			var allowed bool
			var ops int
			_ = c.f.owner.QueryRow(ctx, `SELECT customers.consent_allows($1,$2,$3,'ads_personalization','meta_ads')`, c.tenant, c.store, owner).Scan(&allowed)
			_ = c.f.owner.QueryRow(ctx, `SELECT count(*) FROM ads.capi_events WHERE store_id=$1`, c.store).Scan(&ops)
			if allowed || ops != 0 {
				http.Error(w, fmt.Sprintf("consent_allows=%v capi_events=%d after the withdrawal", allowed, ops), http.StatusConflict)
				return
			}
		default:
			http.NotFound(w, r)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(control.Close)
	values := map[string]string{"COMMERCE_BUYER_WEB_ENABLED": "1", "COMMERCE_BUYER_API_ORIGIN": c.bh.server.URL, "COMMERCE_BUYER_DEMO_LABEL": "1", "COMMERCE_BUYER_BFF_KEY": c.bh.key,
		"COMMERCE_BUYER_COOKIE_KEY": base64.RawURLEncoding.EncodeToString(randomBytes(32)), "COMMERCE_BUYER_SESSION_TTL": "3600",
		"LC_AC_EVIDENCE": evidence, "LC_AC_ORIGIN": storefrontOrigin, "LC_AC_CONTROL": control.URL, "LC_AC_CONTROL_KEY": controlKey}
	cmd := exec.CommandContext(ctx, "node", "tests/storefront/ads-consent.mjs")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	defer func() {
		if cmd.Process != nil {
			_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		}
	}()
	cmd.Dir = root
	cmd.Env = browserEnvironment(values)
	log := browserLog(t, filepath.Join(evidence, "ads-consent.mjs.log"))
	cmd.Stdout, cmd.Stderr = log, log
	if err := cmd.Run(); err != nil {
		t.Fatalf("MA09b storefront browser gate failed: %v; evidence=%s", err, evidence)
	}
	if n := countRows(t, c.f.owner, `SELECT count(*) FROM ads.capi_events WHERE store_id=$1`, c.store); n != 0 {
		t.Errorf("MA09b: %d CAPI events planned by a consent-only gate", n)
	}
	if n := len(c.events()); n != 0 {
		t.Errorf("MA09b: %d events reached Meta from a consent-only gate", n)
	}
	t.Logf("MA09b BROWSER: buyer consent -> CAPI context passed; evidence=%s", evidence)
}
