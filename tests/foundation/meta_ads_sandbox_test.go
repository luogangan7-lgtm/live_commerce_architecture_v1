package foundation_test

// MA-S1..S4 (contracts/meta-ads-v1.md §9 rows MA-S1..MA-S4, §11, §13; SANDBOX tier, owner-scheduled). Skip-gated: every test is
// `t.Skip("NOT_RUN: ...")` unless META_ADS_SANDBOX=1 AND the owner-supplied inputs below exist; with the switch on and an input missing
// the test FAILS (BLOCKED), it never passes vacuously. They call graph.facebook.com through the same adapter routes as production
// (metaads.Routes DispatchWithSecret / ReconcileWithSecret, OAuth.Connect, Client.PostEvent) with the owner's own app 大梦 secrets and a
// sandbox ad account; they never run in CI and are NOT_RUN in this unit's evidence. Nothing here can spend: the sandbox ad account has no
// delivery (F20) and the only ACTIVE object is created under a PAUSED campaign before the single activation the sandbox allows.
//
// Owner inputs (files hold secrets; contents never echoed): META_ADS_SANDBOX_TOKEN_FILE (BISU token), META_ADS_SANDBOX_AD_ACCOUNT (numeric),
// META_ADS_SANDBOX_PAGE_ID + META_ADS_SANDBOX_POST_ID (an owner test Page post), META_ADS_SANDBOX_IG_MEDIA_ID + META_ADS_SANDBOX_IG_USER_ID,
// META_ADS_SANDBOX_CODE (a fresh FLfB authorization code from the owner's own login on app 大梦) with META_ADS_SANDBOX_APP_ID,
// META_ADS_SANDBOX_APP_SECRET_FILE, META_ADS_SANDBOX_REDIRECT_URI and META_ADS_SANDBOX_HPKE_PUBLIC_KEYS_JSON/_ACTIVE_KEY_ID,
// META_ADS_SANDBOX_DATASET_ID + META_ADS_SANDBOX_TEST_EVENT_CODE (a dataset in the 香港大碗 business).
// Evidence label when run: SANDBOX. U1/U2/U8/U9/U3/U4/U7/U10 stay UNKNOWN until an owner run closes them.

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"livecommerce/internal/ads"
	"livecommerce/internal/attribution"
	integration "livecommerce/internal/integrations/core"
	metaads "livecommerce/internal/integrations/meta_ads"
)

func sandboxGate(t *testing.T, names ...string) map[string]string {
	t.Helper()
	if os.Getenv("META_ADS_SANDBOX") != "1" {
		t.Skip("NOT_RUN: META_ADS_SANDBOX=1 and the owner's sandbox inputs are required (app 大梦 roles, sandbox ad account); never run in CI")
	}
	values := map[string]string{}
	for _, n := range names {
		v := os.Getenv(n)
		if v == "" {
			t.Fatalf("BLOCKED: %s is required when META_ADS_SANDBOX=1", n)
		}
		values[n] = v
	}
	return values
}

func sandboxToken(t *testing.T, path string) []byte {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil || len(strings.TrimSpace(string(raw))) == 0 {
		t.Fatalf("BLOCKED: token file unreadable or empty")
	}
	return []byte(strings.TrimSpace(string(raw)))
}

type sandboxRoutes map[string]integration.DispatchRoute

func sandboxRoutesFor(t *testing.T) sandboxRoutes {
	t.Helper()
	f := fixture(t)
	pool := miPool(t, f, "commerce_worker")
	routes, err := metaads.Routes(pool, metaads.Config{GraphVersion: "v26.0", PartnerAgent: "lc_sandbox_gate"}, stubOpener{}, func(context.Context, integration.DispatchRequest) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	out := sandboxRoutes{}
	for _, r := range routes {
		out[r.Action] = r
	}
	return out
}

func (r sandboxRoutes) call(t *testing.T, action, account string, token []byte, body string, reconcile bool) integration.Outcome {
	t.Helper()
	op := randomUUID()
	req := integration.DispatchRequest{OperationID: op, TenantID: randomUUID(), StoreID: randomUUID(), PrincipalID: randomUUID(), BindingID: randomUUID(), BindingVersion: 1,
		Provider: "meta_ads", ExternalAssetID: account, Purpose: "marketing", Action: action, Request: []byte(strings.ReplaceAll(body, "{OP}", op)), IdempotencyKey: "sandbox", Mode: "dispatch"}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	fn := r[action].DispatchWithSecret
	if reconcile {
		fn, req.Mode = r[action].ReconcileWithSecret, "reconcile"
	}
	out, err := fn(ctx, req, integration.NewSecret(token))
	if err != nil {
		t.Fatalf("%s: %v", action, err)
	}
	return out
}

func TestMetaAdsSandboxS1(t *testing.T) {
	in := sandboxGate(t, "META_ADS_SANDBOX_TOKEN_FILE", "META_ADS_SANDBOX_AD_ACCOUNT", "META_ADS_SANDBOX_PAGE_ID", "META_ADS_SANDBOX_POST_ID")
	token := sandboxToken(t, in["META_ADS_SANDBOX_TOKEN_FILE"])
	account, page, post := in["META_ADS_SANDBOX_AD_ACCOUNT"], in["META_ADS_SANDBOX_PAGE_ID"], in["META_ADS_SANDBOX_POST_ID"]
	r := sandboxRoutesFor(t)
	draft := randomUUID()
	start, end := time.Now().UTC().Add(2*time.Hour).Format(time.RFC3339), time.Now().UTC().Add(26*time.Hour).Format(time.RFC3339)
	camp := r.call(t, "meta.ads.create_campaign", account, token, fmt.Sprintf(`{"v":1,"draft_id":%q,"attempt":1,"name":"lc-{OP}","objective":"OUTCOME_ENGAGEMENT","currency":"TWD","spend_cap_minor":0}`, draft), false)
	if camp.State != "SUCCEEDED" {
		t.Fatalf("U1/U8 create campaign PAUSED: %+v", camp)
	}
	adset := r.call(t, "meta.ads.create_adset", account, token, fmt.Sprintf(`{"v":1,"draft_id":%q,"attempt":1,"name":"lc-{OP}","campaign_id":%q,"template":"BOOST_POST","currency":"TWD","lifetime_budget_minor":300000,"start_time":%q,"end_time":%q,"countries":["TW"],"age_min":18,"age_max":65}`, draft, camp.ProviderReference, start, end), false)
	if adset.State != "SUCCEEDED" {
		t.Fatalf("U2 create ad set (optimization_goal/billing_event pair): %+v", adset)
	}
	creative := r.call(t, "meta.ads.create_creative", account, token, fmt.Sprintf(`{"v":1,"draft_id":%q,"attempt":1,"name":"lc-{OP}","template":"BOOST_POST","page_id":%q,"object_story_id":%q}`, draft, page, page+"_"+post), false)
	if creative.State != "SUCCEEDED" {
		t.Fatalf("U9 create creative (subcode 1885183 means the app is still in dev mode): %+v", creative)
	}
	ad := r.call(t, "meta.ads.create_ad", account, token, fmt.Sprintf(`{"v":1,"draft_id":%q,"attempt":1,"name":"lc-{OP}","adset_id":%q,"creative_id":%q}`, draft, adset.ProviderReference, creative.ProviderReference), false)
	if ad.State != "SUCCEEDED" {
		t.Fatalf("create ad: %+v", ad)
	}
	status := fmt.Sprintf(`{"v":1,"draft_id":%q,"attempt":1,"seq":1,"campaign_id":%q}`, draft, camp.ProviderReference)
	if out := r.call(t, "meta.ads.activate", account, token, status, false); out.State != "SUCCEEDED" {
		t.Fatalf("activate: %+v", out)
	}
	if out := r.call(t, "meta.ads.activate", account, token, status, true); out.State != "SUCCEEDED" {
		t.Fatalf("reconcile of the activation by status GET: %+v", out)
	}
	if out := r.call(t, "meta.ads.pause", account, token, strings.Replace(status, `"seq":1`, `"seq":2`, 1), false); out.State != "SUCCEEDED" {
		t.Fatalf("pause: %+v", out)
	}
	t.Logf("SANDBOX S1 created campaign %s / ad set %s / creative %s / ad %s and paused it; closes U1/U2/U8/U9 only when this run is owner-approved evidence", camp.ProviderReference, adset.ProviderReference, creative.ProviderReference, ad.ProviderReference)
}

func TestMetaAdsSandboxS2(t *testing.T) {
	in := sandboxGate(t, "META_ADS_SANDBOX_TOKEN_FILE", "META_ADS_SANDBOX_AD_ACCOUNT", "META_ADS_SANDBOX_PAGE_ID", "META_ADS_SANDBOX_POST_ID", "META_ADS_SANDBOX_IG_MEDIA_ID", "META_ADS_SANDBOX_IG_USER_ID")
	token := sandboxToken(t, in["META_ADS_SANDBOX_TOKEN_FILE"])
	account, page := in["META_ADS_SANDBOX_AD_ACCOUNT"], in["META_ADS_SANDBOX_PAGE_ID"]
	r := sandboxRoutesFor(t)
	draft := randomUUID()
	fb := r.call(t, "meta.ads.create_creative", account, token, fmt.Sprintf(`{"v":1,"draft_id":%q,"attempt":1,"name":"lc-{OP}","template":"BOOST_POST","page_id":%q,"object_story_id":%q}`, draft, page, page+"_"+in["META_ADS_SANDBOX_POST_ID"]), false)
	ig := r.call(t, "meta.ads.create_creative", account, token, fmt.Sprintf(`{"v":1,"draft_id":%q,"attempt":1,"name":"lc-{OP}","template":"BOOST_POST","page_id":%q,"source_instagram_media_id":%q,"instagram_user_id":%q}`, draft, page, in["META_ADS_SANDBOX_IG_MEDIA_ID"], in["META_ADS_SANDBOX_IG_USER_ID"]), false)
	if fb.State != "SUCCEEDED" || ig.State != "SUCCEEDED" {
		t.Fatalf("U3/U4 BOOST_POST creatives: facebook %+v instagram %+v", fb, ig)
	}
}

func TestMetaAdsSandboxS3(t *testing.T) {
	in := sandboxGate(t, "META_ADS_SANDBOX_CODE", "META_ADS_SANDBOX_APP_ID", "META_ADS_SANDBOX_APP_SECRET_FILE", "META_ADS_SANDBOX_REDIRECT_URI", "META_ADS_SANDBOX_HPKE_PUBLIC_KEYS_JSON", "META_ADS_SANDBOX_HPKE_ACTIVE_KEY_ID")
	secret := sandboxToken(t, in["META_ADS_SANDBOX_APP_SECRET_FILE"])
	keys, err := metaads.LoadSealKeys(func(k string) string {
		return map[string]string{"COMMERCE_META_ADS_TOKEN_HPKE_PUBLIC_KEYS_JSON": in["META_ADS_SANDBOX_HPKE_PUBLIC_KEYS_JSON"], "COMMERCE_META_ADS_TOKEN_HPKE_ACTIVE_KEY_ID": in["META_ADS_SANDBOX_HPKE_ACTIVE_KEY_ID"]}[k]
	})
	if err != nil {
		t.Fatalf("BLOCKED: seal keys: %v", err)
	}
	oauth, err := metaads.NewOAuth(metaads.Config{GraphVersion: "v26.0"}, metaads.AppConfig{AppID: in["META_ADS_SANDBOX_APP_ID"], RedirectURI: in["META_ADS_SANDBOX_REDIRECT_URI"], AppSecret: secret}, keys)
	if err != nil {
		t.Fatal(err)
	}
	res, err := oauth.Connect(context.Background(), in["META_ADS_SANDBOX_CODE"], ads.SealInfo{TenantID: randomUUID(), StoreID: randomUUID()})
	if err != nil {
		t.Fatalf("U7/U10 FLfB code exchange + /me + permissions + accounts: %v", err)
	}
	if res.ClientBusinessID == "" || len(res.Picks) == 0 || len(res.Token.Ciphertext) == 0 {
		t.Fatalf("connect result incomplete (client business %q, %d picks)", res.ClientBusinessID, len(res.Picks))
	}
	t.Logf("SANDBOX S3: client_business_id present, %d picks, scopes %v (A-8: is business_management needed?)", len(res.Picks), res.Scopes)
}

func TestMetaAdsSandboxS4(t *testing.T) {
	in := sandboxGate(t, "META_ADS_SANDBOX_TOKEN_FILE", "META_ADS_SANDBOX_DATASET_ID", "META_ADS_SANDBOX_TEST_EVENT_CODE")
	token := sandboxToken(t, in["META_ADS_SANDBOX_TOKEN_FILE"])
	client, err := metaads.NewClient(metaads.Config{GraphVersion: "v26.0", PartnerAgent: "lc_sandbox_gate"})
	if err != nil {
		t.Fatal(err)
	}
	body := fmt.Sprintf(`{"data":[{"event_name":"Purchase","event_time":%d,"event_id":%q,"action_source":"website","event_source_url":"https://shop.example.test/orders","user_data":{"client_user_agent":"lc-sandbox-gate/1"},"custom_data":{"currency":"TWD","value":10}}],"test_event_code":%q}`,
		time.Now().Unix(), attribution.EventID(randomUUID()), in["META_ADS_SANDBOX_TEST_EVENT_CODE"])
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	out, err := client.PostEvent(ctx, token, in["META_ADS_SANDBOX_DATASET_ID"], []byte(body))
	if err != nil || out.State != "SUCCEEDED" {
		t.Fatalf("CAPI Purchase with test_event_code: %+v %v", out, err)
	}
}
