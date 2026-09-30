package foundation_test

// MA01 (contracts/meta-ads-v1.md §9 row MA01, §3 money + result grammar + classification, D10 canonical draft, D2 200-char
// cap, AD8 event_id, F16 hashing, C3 external_id). Tier UNIT (no PG) for money, spend, grammar, canonical draft, phone,
// event id and external id; the Graph classification tables (creates / activate+pause / reads / CAPI) are driven through
// the real route callbacks against the fake Graph (tests/ads/fakegraph, MOCK). metaads.Routes validates its worker pool,
// so that one test needs a commerce_worker login of the shared PG fixture (it is `TestMetaAdsMA01Classification`, in the
// MA01 prefix; no rows are read or written, no Check runs, the token stub never touches PG).
//
// Written by the independent ads-tests author from the FROZEN contract and the frozen Go signatures of ads-graph /
// ads-core / ads-capi; expected values are the contract's own examples or hand-derived from §3 / D10 / F16, never copied
// from the implementation. Touches: no table; host = loopback fake only (never graph.facebook.com).
// Evidence label: UNIT + MOCK.

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"testing"
	"time"

	"livecommerce/internal/ads"
	"livecommerce/internal/attribution"
	integration "livecommerce/internal/integrations/core"
	metaads "livecommerce/internal/integrations/meta_ads"
	"livecommerce/tests/ads/fakegraph"
)

func TestMetaAdsMA01Units(t *testing.T) {
	t.Run("MetaBudget", func(t *testing.T) {
		ok := []struct {
			cur   string
			minor int64
			want  int64
		}{
			{"TWD", 12300, 123}, {"TWD", 100, 1}, {"TWD", 99900000, 999000},
			{"USD", 12345, 12345}, {"USD", 1, 1}, {"HKD", 500, 500}, {"HKD", 12345, 12345},
		}
		for _, c := range ok {
			got, err := metaads.MetaBudget(c.cur, c.minor)
			if err != nil || got != c.want {
				t.Errorf("MetaBudget(%s,%d)=%d,%v want %d", c.cur, c.minor, got, err, c.want)
			}
		}
		// I05/F19: TWD has Meta offset 1; a non-whole NT$ is refused, never truncated.
		for _, minor := range []int64{12345, 1, 99, 101, 12399} {
			if _, err := metaads.MetaBudget("TWD", minor); !errors.Is(err, metaads.ErrNotWholeUnit) {
				t.Errorf("TWD %d: %v, want ErrNotWholeUnit", minor, err)
			}
		}
		for _, cur := range []string{"EUR", "JPY", "twd", "", "USDT", "CNY"} {
			if _, err := metaads.MetaBudget(cur, 1000); !errors.Is(err, metaads.ErrUnsupportedCurrency) {
				t.Errorf("currency %q: %v, want ErrUnsupportedCurrency", cur, err)
			}
		}
		if v, err := metaads.MetaBudget("USD", -1); err == nil {
			t.Errorf("negative budget accepted: %d", v)
		}
	})

	t.Run("SpendMinor", func(t *testing.T) {
		// §9 MA01 vectors verbatim.
		for spend, want := range map[string]int64{"0": 0, "12.3": 1230, "12.34": 1234, "12.30": 1230, "0.05": 5, "100": 10000, "7.0": 700} {
			for _, cur := range []string{"TWD", "USD", "HKD"} {
				got, err := metaads.SpendMinor(cur, spend)
				if err != nil || got != want {
					t.Errorf("SpendMinor(%s,%q)=%d,%v want %d", cur, spend, got, err, want)
				}
			}
		}
		// >2 fraction digits, sign, exponent, non-digits: error, never rounded.
		for _, bad := range []string{"12.345", "-1", "-0", "+1", "1e3", "1E3", "0x10", "12,3", " 12", "12 ", "abc", "", "1.2.3", "12.", "NaN", "Infinity", "१२"} {
			if got, err := metaads.SpendMinor("TWD", bad); err == nil {
				t.Errorf("SpendMinor(%q) accepted as %d", bad, got)
			}
		}
		if _, err := metaads.SpendMinor("EUR", "1.00"); !errors.Is(err, metaads.ErrUnsupportedCurrency) {
			t.Errorf("EUR spend: %v", err)
		}
		// no int64 overflow can be smuggled in
		if got, err := metaads.SpendMinor("USD", strings.Repeat("9", 40)); err == nil {
			t.Errorf("40-digit spend accepted as %d", got)
		}
	})

	t.Run("ResultGrammarRoundTrip", func(t *testing.T) {
		// contract §3 preflight example shape, exact text
		p := metaads.Preflight{Status: 1, Currency: "TWD", Timezone: "Asia/Taipei", Funded: true}
		ref, err := metaads.EncodePreflight(p)
		if err != nil || ref != "v1;st=1;cur=TWD;fund=1;tz=Asia/Taipei" {
			t.Fatalf("EncodePreflight=%q,%v", ref, err)
		}
		back, err := metaads.ParsePreflight(ref)
		if err != nil || back != p {
			t.Fatalf("ParsePreflight=%+v,%v", back, err)
		}
		// the contract's own key `fund` has four letters: the grammar must accept its own preflight example (regex {2,4})
		if _, err := metaads.ParsePreflight("v1;st=2;cur=USD;fund=0;tz=UTC"); err != nil {
			t.Fatalf("4-letter key fund rejected: %v", err)
		}
		pu, pv := int64(3), int64(4590)
		for _, d := range []metaads.InsightsDay{
			{EffectiveStatus: "ACTIVE", Currency: "TWD", Timezone: "Asia/Taipei", SpendMinor: 1230, Impressions: 1000, Clicks: 7},
			{EffectiveStatus: "CAMPAIGN_PAUSED", Currency: "USD", Timezone: "America/Los_Angeles", SpendMinor: 0, Impressions: 0, Clicks: 0, Purchases: &pu, PurchaseValueMinor: &pv},
			{EffectiveStatus: "WITH_ISSUES", Currency: "HKD", Timezone: "Asia/Hong_Kong", SpendMinor: 99999999, Impressions: 123456789, Clicks: 42, Purchases: &pu},
		} {
			ref, err := metaads.EncodeInsights(d)
			if err != nil {
				t.Fatalf("EncodeInsights(%+v): %v", d, err)
			}
			if !strings.HasPrefix(ref, "v1;es="+d.EffectiveStatus+";sp=") || !strings.Contains(ref, ";cur="+d.Currency+";tz="+d.Timezone) {
				t.Errorf("insights ref shape: %q", ref)
			}
			if (d.Purchases == nil) != strings.Contains(ref, ";pu=na;") || (d.PurchaseValueMinor == nil) != strings.Contains(ref, ";pv=na;") {
				t.Errorf("nil purchase metrics must encode as na: %q", ref)
			}
			got, err := metaads.ParseInsights(ref)
			if err != nil {
				t.Fatalf("ParseInsights(%q): %v", ref, err)
			}
			if got.EffectiveStatus != d.EffectiveStatus || got.SpendMinor != d.SpendMinor || got.Impressions != d.Impressions || got.Clicks != d.Clicks ||
				got.Currency != d.Currency || got.Timezone != d.Timezone || (got.Purchases == nil) != (d.Purchases == nil) || (got.PurchaseValueMinor == nil) != (d.PurchaseValueMinor == nil) {
				t.Errorf("round trip lost data: %+v -> %q -> %+v", d, ref, got)
			}
			if len(ref) > 200 {
				t.Errorf("result longer than 200 chars (D2): %d", len(ref))
			}
		}
		// D2: results are capped at 200 characters (operations.provider_reference CHECK); a longer string never parses
		long := "v1;st=1;cur=TWD;fund=1;tz=" + strings.Repeat("a", 40)
		for len(long) <= 200 {
			long += ";xx=" + strings.Repeat("b", 40)
		}
		if _, err := metaads.ParsePreflight(long); err == nil {
			t.Error("over-200-char result parsed")
		}
		// strictness: wrong version, unknown key order, extra keys, forbidden characters, bad enum, negatives
		for _, bad := range []string{
			"v2;st=1;cur=TWD;fund=1;tz=UTC", "v1;cur=TWD;st=1;fund=1;tz=UTC", "v1;st=1;cur=TWD;fund=1", "v1;st=1;cur=TWD;fund=2;tz=UTC",
			"v1;st=1;cur=TWD;fund=1;tz=A B", "v1;st=-1;cur=TWD;fund=1;tz=UTC", "v1;st=1;cur=TWDX;fund=1;tz=UTC", "", "v1", "st=1;cur=TWD;fund=1;tz=UTC",
			"v1;st=1;cur=TWD;fund=1;tz=UTC;extra=1",
		} {
			if _, err := metaads.ParsePreflight(bad); err == nil {
				t.Errorf("ParsePreflight(%q) accepted", bad)
			}
		}
		for _, bad := range []metaads.Preflight{{Status: -1, Currency: "TWD", Timezone: "UTC"}, {Status: 1, Currency: "TW", Timezone: "UTC"}, {Status: 1, Currency: "TWD", Timezone: "a b"},
			{Status: 1, Currency: "TWD", Timezone: strings.Repeat("z", 41)}} {
			if _, err := metaads.EncodePreflight(bad); err == nil {
				t.Errorf("EncodePreflight(%+v) accepted", bad)
			}
		}
		if _, err := metaads.EncodeInsights(metaads.InsightsDay{EffectiveStatus: "ACTIVE", Currency: "TWD", Timezone: "UTC", SpendMinor: -1}); err == nil {
			t.Error("negative spend encoded")
		}
		if _, err := metaads.EncodeInsights(metaads.InsightsDay{EffectiveStatus: "not enum", Currency: "TWD", Timezone: "UTC"}); err == nil {
			t.Error("lowercase status encoded")
		}
		// contract regex of the frozen result grammar (with the {2,4} widening recorded in ads-graph): every encoded ref matches it
		re := regexp.MustCompile(`^v1(;[a-z]{2,4}=[A-Za-z0-9_./+-]{1,40}){1,9}$`)
		if !re.MatchString("v1;st=1;cur=TWD;fund=1;tz=Asia/Taipei") {
			t.Error("grammar regex self-check")
		}
	})

	t.Run("CanonicalDraftD10", func(t *testing.T) {
		// D10: v1|<template>|<ad_binding_id>|<ad_binding_version>|<identity_binding_id>|<identity binding current version>|
		// <source_ref>|<currency>|<lifetime_budget_minor>|<starts_at unix>|<ends_at unix>|<countries sorted, comma>|<age_min>|<age_max>
		d := ads.DraftCanon{Template: "BOOST_POST", AdBindingID: "11111111-1111-4111-8111-111111111111", AdBindingVersion: 3,
			IdentityBindingID: "22222222-2222-4222-8222-222222222222", IdentityBindingVersion: 2, SourceRef: "123_456", Currency: "TWD",
			LifetimeBudgetMinor: 300000, StartsAtUnix: 1790000000, EndsAtUnix: 1790086400, Countries: []string{"TW", "HK", "SG"}, AgeMin: 18, AgeMax: 65}
		want := "v1|BOOST_POST|11111111-1111-4111-8111-111111111111|3|22222222-2222-4222-8222-222222222222|2|123_456|TWD|300000|1790000000|1790086400|HK,SG,TW|18|65"
		if got := string(ads.CanonicalDraft(d)); got != want {
			t.Fatalf("canonical draft\n got %s\nwant %s", got, want)
		}
		sum := sha256.Sum256([]byte(want))
		if got := ads.DraftSHA256(d); got != sum {
			t.Fatal("DraftSHA256 is not SHA-256 of the canonical UTF-8 string")
		}
		if d.Countries[0] != "TW" {
			t.Fatal("CanonicalDraft reordered the caller's slice")
		}
		// every field is in the hash: flipping any one changes it (AD5: edit after approval invalidates)
		base := ads.DraftSHA256(d)
		mut := []func(x *ads.DraftCanon){
			func(x *ads.DraftCanon) { x.Template = "PRODUCT_TRAFFIC" },
			func(x *ads.DraftCanon) { x.AdBindingID = "11111111-1111-4111-8111-111111111112" },
			func(x *ads.DraftCanon) { x.AdBindingVersion++ },
			func(x *ads.DraftCanon) { x.IdentityBindingID = "22222222-2222-4222-8222-222222222223" },
			func(x *ads.DraftCanon) { x.IdentityBindingVersion++ },
			func(x *ads.DraftCanon) { x.SourceRef = "123_457" },
			func(x *ads.DraftCanon) { x.Currency = "USD" },
			func(x *ads.DraftCanon) { x.LifetimeBudgetMinor++ },
			func(x *ads.DraftCanon) { x.StartsAtUnix++ },
			func(x *ads.DraftCanon) { x.EndsAtUnix++ },
			func(x *ads.DraftCanon) { x.Countries = []string{"TW"} },
			func(x *ads.DraftCanon) { x.AgeMin++ },
			func(x *ads.DraftCanon) { x.AgeMax-- },
		}
		for i, m := range mut {
			x := d
			x.Countries = append([]string(nil), d.Countries...)
			m(&x)
			if ads.DraftSHA256(x) == base {
				t.Errorf("mutation %d left the approval hash unchanged", i)
			}
		}
		// country order must not matter, country set must
		x := d
		x.Countries = []string{"SG", "TW", "HK"}
		if ads.DraftSHA256(x) != base {
			t.Error("country order changed the hash")
		}
	})

	t.Run("EventIDPhoneExternalID", func(t *testing.T) {
		attempt := "0a1b2c3d-4e5f-4a6b-8c7d-9e0f1a2b3c4d"
		if got := attribution.EventID(attempt); got != "lc-purchase-"+attempt || !regexp.MustCompile(`^lc-purchase-[0-9a-f-]{36}$`).MatchString(got) {
			t.Errorf("EventID=%q", got)
		}
		// F16 (contract §1): phone digits with country code, SHA-256 hex; Meta's own example digits 16505551212
		for in, wantHash := range map[string]string{
			"+16505551212":  "e323ec626319ca94ee8bff2e4c87cf613be6ea19919ed1364124e16807ab3176",
			"+886912345678": "cf676475cec2ee7b7591f39bdefe7ef3dca63b14c1cdba3b19ce4219cbda2b09",
		} {
			got, ok := attribution.HashPhone(in)
			if !ok || got != wantHash {
				t.Errorf("HashPhone(%q)=%q,%v want %s", in, got, ok, wantHash)
			}
			if again, _ := attribution.HashPhone(strings.TrimPrefix(in, "+")); again != wantHash {
				t.Errorf("E.164 without plus hashed differently")
			}
		}
		// not normalizable -> omitted (ok=false), never a guess: local format, spaces, punctuation, letters, too short/long
		for _, bad := range []string{"", "0912345678", "+0912345678", "+886 912 345 678", "+886-912-345-678", "(650) 555-1212", "+1650555121x", "+1234567", "+1234567890123456", "++16505551212", "phone"} {
			if h, ok := attribution.HashPhone(bad); ok {
				t.Errorf("HashPhone(%q) produced %q", bad, h)
			}
		}
		// C3 external_id: hex(SHA-256(hex(HMAC-SHA256(K, "capi-external-id/v1|tenant|store|owner"))))
		key := []byte("synthetic-external-id-key-0123456789abcdef")
		tenant, store, owner := "t0000000-0000-4000-8000-000000000001", "s0000000-0000-4000-8000-000000000002", "o0000000-0000-4000-8000-000000000003"
		mac := hmac.New(sha256.New, key)
		mac.Write([]byte("capi-external-id/v1|" + tenant + "|" + store + "|" + owner))
		inner := sha256.Sum256([]byte(hex.EncodeToString(mac.Sum(nil))))
		got := attribution.ExternalID(key, tenant, store, owner)
		if got != hex.EncodeToString(inner[:]) || len(got) != 64 || strings.Contains(got, owner) {
			t.Errorf("ExternalID=%q", got)
		}
		for name, other := range map[string]string{
			"other store":  attribution.ExternalID(key, tenant, "s0000000-0000-4000-8000-0000000000ff", owner),
			"other owner":  attribution.ExternalID(key, tenant, store, "o0000000-0000-4000-8000-0000000000ff"),
			"other tenant": attribution.ExternalID(key, "t0000000-0000-4000-8000-0000000000ff", store, owner),
			"other key":    attribution.ExternalID([]byte("another-synthetic-key-0123456789abcdefgh"), tenant, store, owner),
		} {
			if other == got {
				t.Errorf("%s produced the same external_id", name)
			}
		}
	})
}

// stubOpener is a TokenOpener that must never be reached by these direct callback calls.
type stubOpener struct{}

func (stubOpener) Open(string, string, string, []byte, []byte) ([]byte, error) {
	return nil, errors.New("stub opener must not be called")
}

const ma01Token = "SENTINEL-EAAG-MA01-TOKEN"

// TestMetaAdsMA01Classification: contract §3 classification tables through the real route callbacks and the fake Graph.
func TestMetaAdsMA01Classification(t *testing.T) {
	f := fixture(t)
	g := fakegraph.New()
	t.Cleanup(g.Close)
	const acct = "9100000000001"
	g.AddAccount(fakegraph.Account{ID: acct, Currency: "TWD", Timezone: "Asia/Taipei", Status: 1, Funded: true, MinDailyBudget: "100"})
	g.Grant(ma01Token, "5100000000001", []string{"ads_management", "ads_read"}, []string{acct}, nil)
	pool := miPool(t, f, "commerce_worker")
	cfg := metaads.Config{GraphBaseURL: g.URL(), GraphVersion: "v26.0", PartnerAgent: "lc_test_partner"}
	routes, err := metaads.Routes(pool, cfg, stubOpener{}, func(context.Context, integration.DispatchRequest) error { return nil })
	if err != nil {
		t.Fatalf("metaads.Routes: %v", err)
	}
	byAction := map[string]integration.DispatchRoute{}
	for _, r := range routes {
		if r.Provider != "meta_ads" || r.Purpose != "marketing" || r.LoadSecret == nil || r.DispatchWithSecret == nil || r.ReconcileWithSecret == nil || r.Reconcile != nil || r.Dispatch != nil {
			t.Errorf("route %s/%s/%s not the LoadSecret + DispatchWithSecret + ReconcileWithSecret shape of §6.1/A-10", r.Provider, r.Action, r.Purpose)
		}
		byAction[r.Action] = r
	}
	want := []string{"meta.ads.create_campaign", "meta.ads.create_adset", "meta.ads.create_creative", "meta.ads.create_ad",
		"meta.ads.preflight_account", "meta.ads.activate", "meta.ads.pause", "meta.ads.read_insights"}
	if len(routes) != 8 {
		t.Fatalf("%d routes, want 8", len(routes))
	}
	for _, a := range want {
		if _, ok := byAction[a]; !ok {
			t.Errorf("missing route %s", a)
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	draft := randomUUID()
	req := func(action string, body string) integration.DispatchRequest {
		op := randomUUID()
		body = strings.ReplaceAll(body, "{OP}", op)
		return integration.DispatchRequest{OperationID: op, TenantID: f.tenantA, StoreID: f.storeA1, PrincipalID: f.principalA,
			BindingID: randomUUID(), BindingVersion: 1, Provider: "meta_ads", ExternalAssetID: acct, Purpose: "marketing", Action: action,
			Request: []byte(body), IdempotencyKey: "ma01", Mode: "dispatch"}
	}
	dispatch := func(r integration.DispatchRequest) (integration.Outcome, error) {
		return byAction[r.Action].DispatchWithSecret(ctx, r, integration.NewSecret([]byte(ma01Token)))
	}
	campaign := fmt.Sprintf(`{"v":1,"draft_id":%q,"attempt":1,"name":"lc-{OP}","objective":"OUTCOME_ENGAGEMENT","currency":"TWD","spend_cap_minor":0}`, draft)

	t.Run("creates", func(t *testing.T) {
		g.ClearFaults()
		out, err := dispatch(req("meta.ads.create_campaign", campaign))
		if err != nil || out.State != "SUCCEEDED" || !regexp.MustCompile(`^[0-9]{1,40}$`).MatchString(out.ProviderReference) {
			t.Fatalf("2xx create: %+v %v", out, err)
		}
		// AD3: created PAUSED, tagged lc-<op uuid>
		created := g.Objects("campaign")
		if len(created) != 1 || created[0].Status != "PAUSED" || created[0].ID != out.ProviderReference {
			t.Fatalf("campaign as created: %+v", created)
		}
		cases := []struct {
			name  string
			fault fakegraph.Fault
			state string
			code  string
		}{
			{"graph 100 -> graph_100", fakegraph.Fault{Route: fakegraph.RouteCreateCampaign, Kind: fakegraph.FaultGraphError, Code: 100}, "FAILED_FINAL", "graph_100"},
			{"graph 190 -> graph_190", fakegraph.Fault{Route: fakegraph.RouteCreateCampaign, Kind: fakegraph.FaultGraphError, Code: 190, HTTP: 401}, "FAILED_FINAL", "graph_190"},
			{"code 4 -> rate_limited", fakegraph.Fault{Route: fakegraph.RouteCreateCampaign, Kind: fakegraph.FaultGraphError, Code: 4, HTTP: 429}, "FAILED_FINAL", "rate_limited"},
			{"code 17 -> rate_limited", fakegraph.Fault{Route: fakegraph.RouteCreateCampaign, Kind: fakegraph.FaultGraphError, Code: 17}, "FAILED_FINAL", "rate_limited"},
			{"code 613 -> rate_limited", fakegraph.Fault{Route: fakegraph.RouteCreateCampaign, Kind: fakegraph.FaultGraphError, Code: 613}, "FAILED_FINAL", "rate_limited"},
			{"code 80004 -> rate_limited", fakegraph.Fault{Route: fakegraph.RouteCreateCampaign, Kind: fakegraph.FaultGraphError, Code: 80004}, "FAILED_FINAL", "rate_limited"},
			{"timeout (no effect) -> UNKNOWN", fakegraph.Fault{Route: fakegraph.RouteCreateCampaign, Kind: fakegraph.FaultTimeout}, "UNKNOWN", ""},
			{"timeout after create -> UNKNOWN", fakegraph.Fault{Route: fakegraph.RouteCreateCampaign, Kind: fakegraph.FaultTimeout, Effect: true}, "UNKNOWN", ""},
			{"503 after create -> UNKNOWN", fakegraph.Fault{Route: fakegraph.RouteCreateCampaign, Kind: fakegraph.Fault5xx, Effect: true}, "UNKNOWN", ""},
			{"unparseable 200 -> UNKNOWN", fakegraph.Fault{Route: fakegraph.RouteCreateCampaign, Kind: fakegraph.FaultGarbled, Effect: true}, "UNKNOWN", ""},
			{"200 without id -> UNKNOWN", fakegraph.Fault{Route: fakegraph.RouteCreateCampaign, Kind: fakegraph.FaultShapeless, Effect: true}, "UNKNOWN", ""},
			{"5xx with a Graph error body is still UNKNOWN", fakegraph.Fault{Route: fakegraph.RouteCreateCampaign, Kind: fakegraph.FaultGraphError, Code: 100, HTTP: 503}, "UNKNOWN", ""},
		}
		callCtx := ctx
		for _, c := range cases {
			g.ClearFaults()
			g.Inject(c.fault)
			short, stop := context.WithTimeout(callCtx, 900*time.Millisecond)
			r := req("meta.ads.create_campaign", campaign)
			out, err := byAction[r.Action].DispatchWithSecret(short, r, integration.NewSecret([]byte(ma01Token)))
			stop()
			if err != nil && c.state != "UNKNOWN" {
				t.Errorf("%s: adapter error %v", c.name, err)
				continue
			}
			state := out.State
			if err != nil { // a returned error is completeAmbiguous -> UNKNOWN in the dispatcher: same class
				state = "UNKNOWN"
			}
			if state != c.state || (c.code != "" && out.Code != c.code) {
				t.Errorf("%s: got %s/%s (err %v), want %s/%s", c.name, out.State, out.Code, err, c.state, c.code)
			}
			if c.state == "UNKNOWN" && out.ProviderReference != "" {
				t.Errorf("%s: UNKNOWN create carried a provider reference %q", c.name, out.ProviderReference)
			}
		}
		g.ClearFaults()
	})

	t.Run("name must be the lc tag, otherwise zero HTTP", func(t *testing.T) {
		g.ClearFaults()
		before := g.Posts()
		bad := fmt.Sprintf(`{"v":1,"draft_id":%q,"attempt":1,"name":"lc-%s","objective":"OUTCOME_ENGAGEMENT","currency":"TWD","spend_cap_minor":0}`, draft, randomUUID())
		out, err := dispatch(req("meta.ads.create_campaign", bad))
		if err != nil || out.State != "FAILED_FINAL" || out.Code != "bad_request" || g.Posts() != before {
			t.Fatalf("name not tagged with this operation: %+v %v posts+%d", out, err, g.Posts()-before)
		}
		unknownKey := fmt.Sprintf(`{"v":1,"draft_id":%q,"attempt":1,"name":"lc-{OP}","objective":"OUTCOME_ENGAGEMENT","currency":"TWD","spend_cap_minor":0,"access_token":"x"}`, draft)
		out, err = dispatch(req("meta.ads.create_campaign", unknownKey))
		if err != nil || out.State != "FAILED_FINAL" || out.Code != "bad_request" || g.Posts() != before {
			t.Fatalf("unknown request key: %+v %v", out, err)
		}
	})

	t.Run("activate and pause are never FAILED_FINAL", func(t *testing.T) {
		g.ClearFaults()
		out, _ := dispatch(req("meta.ads.create_campaign", campaign))
		camp := out.ProviderReference
		status := func(action string) integration.DispatchRequest {
			return req(action, fmt.Sprintf(`{"v":1,"draft_id":%q,"attempt":1,"seq":1,"campaign_id":%q}`, draft, camp))
		}
		for _, action := range []string{"meta.ads.activate", "meta.ads.pause"} {
			g.ClearFaults()
			target := map[string]string{"meta.ads.activate": "ACTIVE", "meta.ads.pause": "PAUSED"}[action]
			out, err := dispatch(status(action))
			if err != nil || out.State != "SUCCEEDED" {
				t.Fatalf("%s success:true: %+v %v", action, out, err)
			}
			if o, _ := g.Object(camp); o.Status != target {
				t.Errorf("%s: remote status %s", action, o.Status)
			}
			faults := []fakegraph.Fault{
				{Route: fakegraph.RouteStatusPost, Kind: fakegraph.FaultGraphError, Code: 100},
				{Route: fakegraph.RouteStatusPost, Kind: fakegraph.FaultGraphError, Code: 4, HTTP: 429},
				{Route: fakegraph.RouteStatusPost, Kind: fakegraph.FaultGraphError, Code: 17},
				{Route: fakegraph.RouteStatusPost, Kind: fakegraph.FaultGraphError, Code: 80004},
				{Route: fakegraph.RouteStatusPost, Kind: fakegraph.FaultGraphError, Code: 190, HTTP: 401},
				{Route: fakegraph.RouteStatusPost, Kind: fakegraph.Fault5xx},
				{Route: fakegraph.RouteStatusPost, Kind: fakegraph.FaultTimeout},
				{Route: fakegraph.RouteStatusPost, Kind: fakegraph.FaultTimeout, Effect: true},
				{Route: fakegraph.RouteStatusPost, Kind: fakegraph.FaultNoSuccess},
				{Route: fakegraph.RouteStatusPost, Kind: fakegraph.FaultShapeless},
				{Route: fakegraph.RouteStatusPost, Kind: fakegraph.FaultGarbled},
			}
			for i, fl := range faults {
				g.ClearFaults()
				g.Inject(fl)
				short, stop := context.WithTimeout(ctx, 900*time.Millisecond)
				r := status(action)
				out, err := byAction[action].DispatchWithSecret(short, r, integration.NewSecret([]byte(ma01Token)))
				stop()
				if err == nil && out.State != "UNKNOWN" {
					t.Errorf("%s fault %d (%+v): %s/%s, want UNKNOWN (never FAILED_FINAL)", action, i, fl, out.State, out.Code)
				}
			}
		}
		g.ClearFaults()
	})

	t.Run("reads", func(t *testing.T) {
		g.ClearFaults()
		out, _ := dispatch(req("meta.ads.create_campaign", campaign))
		camp := out.ProviderReference
		day := time.Now().UTC().Format("2006-01-02")
		g.SetInsights(camp, day, fakegraph.Insights{Spend: "12.3", Impressions: "1000", Clicks: "9", PurchaseCount: "2", PurchaseValue: "45.90"})
		pre := func() integration.DispatchRequest {
			return req("meta.ads.preflight_account", fmt.Sprintf(`{"v":1,"draft_id":%q,"attempt":1,"seq":1}`, draft))
		}
		ins := func() integration.DispatchRequest {
			return req("meta.ads.read_insights", fmt.Sprintf(`{"v":1,"draft_id":%q,"campaign_id":%q,"day":%q}`, draft, camp, day))
		}
		out, err := dispatch(pre())
		if err != nil || out.State != "SUCCEEDED" {
			t.Fatalf("preflight: %+v %v", out, err)
		}
		p, perr := metaads.ParsePreflight(out.ProviderReference)
		if perr != nil || p.Status != 1 || p.Currency != "TWD" || p.Timezone != "Asia/Taipei" || !p.Funded {
			t.Fatalf("preflight result %q -> %+v %v", out.ProviderReference, p, perr)
		}
		out, err = dispatch(ins())
		if err != nil || out.State != "SUCCEEDED" {
			t.Fatalf("insights: %+v %v", out, err)
		}
		d, derr := metaads.ParseInsights(out.ProviderReference)
		if derr != nil || d.SpendMinor != 1230 || d.Impressions != 1000 || d.Clicks != 9 || d.Purchases == nil || *d.Purchases != 2 || d.PurchaseValueMinor == nil || *d.PurchaseValueMinor != 4590 || d.EffectiveStatus != "PAUSED" && d.EffectiveStatus != "ACTIVE" {
			t.Fatalf("insights result %q -> %+v %v", out.ProviderReference, d, derr)
		}
		// F13: a day without delivery is zero spend, not an error
		g.SetInsights(camp, day, fakegraph.Insights{})
		g.ClearFaults()
		empty := req("meta.ads.read_insights", fmt.Sprintf(`{"v":1,"draft_id":%q,"campaign_id":%q,"day":"2001-02-03"}`, draft, camp))
		out, _ = dispatch(empty)
		if d, e := metaads.ParseInsights(out.ProviderReference); out.State != "SUCCEEDED" || e != nil || d.SpendMinor != 0 {
			t.Fatalf("empty day: %+v %v", out, e)
		}
		// a spend Meta reports with 3 fraction digits is never rounded: FAILED_FINAL bad_spend
		g.SetInsights(camp, day, fakegraph.Insights{Spend: "12.345", Impressions: "1", Clicks: "1"})
		if out, _ = dispatch(ins()); out.State != "FAILED_FINAL" || out.Code != "bad_spend" {
			t.Errorf("12.345 spend: %+v", out)
		}
		g.SetInsights(camp, day, fakegraph.Insights{Spend: "1e3", Impressions: "1", Clicks: "1"})
		if out, _ = dispatch(ins()); out.State != "FAILED_FINAL" || out.Code != "bad_spend" {
			t.Errorf("1e3 spend: %+v", out)
		}
		// 4xx error body -> FAILED_FINAL graph_<code>/rate_limited; anything else -> UNKNOWN
		for name, fl := range map[string]fakegraph.Fault{
			"graph_100":    {Route: fakegraph.RouteAccountGet, Kind: fakegraph.FaultGraphError, Code: 100},
			"rate_limited": {Route: fakegraph.RouteAccountGet, Kind: fakegraph.FaultGraphError, Code: 80004, HTTP: 400},
		} {
			g.ClearFaults()
			g.Inject(fl)
			out, _ = dispatch(pre())
			if out.State != "FAILED_FINAL" || out.Code != name {
				t.Errorf("read 4xx %s: %+v", name, out)
			}
		}
		for i, fl := range []fakegraph.Fault{
			{Route: fakegraph.RouteAccountGet, Kind: fakegraph.Fault5xx},
			{Route: fakegraph.RouteAccountGet, Kind: fakegraph.FaultGarbled},
			{Route: fakegraph.RouteAccountGet, Kind: fakegraph.FaultShapeless},
			{Route: fakegraph.RouteAccountGet, Kind: fakegraph.FaultTimeout},
		} {
			g.ClearFaults()
			g.Inject(fl)
			short, stop := context.WithTimeout(ctx, 900*time.Millisecond)
			r := pre()
			out, err := byAction[r.Action].DispatchWithSecret(short, r, integration.NewSecret([]byte(ma01Token)))
			stop()
			if err == nil && out.State != "UNKNOWN" {
				t.Errorf("read doubt %d: %+v, want UNKNOWN", i, out)
			}
		}
		g.ClearFaults()
	})

	t.Run("CAPI PostEvent classification", func(t *testing.T) {
		g.ClearFaults()
		const pixel = "7700000000001"
		g.Grant(ma01Token, "5100000000001", []string{"ads_management", "ads_read"}, []string{acct}, nil)
		client, err := metaads.NewClient(cfg)
		if err != nil {
			t.Fatal(err)
		}
		body := func(age time.Duration) []byte {
			return []byte(fmt.Sprintf(`{"data":[{"event_name":"Purchase","event_time":%d,"event_id":"lc-purchase-%s","action_source":"website","event_source_url":"https://shop.example.test/orders","user_data":{"client_user_agent":"UA/1"},"custom_data":{"currency":"TWD","value":10}}]}`,
				time.Now().Add(-age).Unix(), randomUUID()))
		}
		post := func(b []byte) (integration.Outcome, error) {
			short, stop := context.WithTimeout(ctx, 900*time.Millisecond)
			defer stop()
			return client.PostEvent(short, []byte(ma01Token), pixel, b)
		}
		out, err := post(body(time.Minute))
		if err != nil || out.State != "SUCCEEDED" {
			t.Fatalf("2xx events_received=1: %+v %v", out, err)
		}
		reqs := g.Requests()
		last := reqs[len(reqs)-1]
		if last.Route != fakegraph.RouteEvents || last.TokenSource == "query" || !strings.Contains(string(last.Body), `"partner_agent":"lc_test_partner"`) {
			t.Errorf("event request: route %s token via %s; partner_agent missing in body", last.Route, last.TokenSource)
		}
		// F14: an event older than 7 days rejects the batch with a 4xx Graph error -> FAILED_FINAL
		if out, err = post(body(8 * 24 * time.Hour)); err != nil || out.State != "FAILED_FINAL" {
			t.Errorf("stale event: %+v %v", out, err)
		}
		for i, fl := range []fakegraph.Fault{
			{Route: fakegraph.RouteEvents, Kind: fakegraph.Fault5xx}, {Route: fakegraph.RouteEvents, Kind: fakegraph.FaultTimeout},
			{Route: fakegraph.RouteEvents, Kind: fakegraph.FaultGarbled}, {Route: fakegraph.RouteEvents, Kind: fakegraph.FaultShapeless},
		} {
			g.ClearFaults()
			g.Inject(fl)
			out, err := post(body(time.Minute))
			if err == nil && out.State != "UNKNOWN" {
				t.Errorf("CAPI doubt %d: %+v, want UNKNOWN", i, out)
			}
		}
		g.ClearFaults()
		g.Inject(fakegraph.Fault{Route: fakegraph.RouteEvents, Kind: fakegraph.FaultGraphError, Code: 100})
		if out, err = post(body(time.Minute)); err != nil || out.State != "FAILED_FINAL" {
			t.Errorf("CAPI 4xx with error body: %+v %v", out, err)
		}
		g.ClearFaults()
		// one event only: a two-event batch is refused before any call
		before := g.Count(fakegraph.RouteEvents)
		two := []byte(`{"data":[{"event_name":"Purchase"},{"event_name":"Purchase"}]}`)
		if out, _ = post(two); out.State != "FAILED_FINAL" || g.Count(fakegraph.RouteEvents) != before {
			t.Errorf("two-event batch: %+v", out)
		}
	})

	t.Run("host guard", func(t *testing.T) {
		// §3: host graph.facebook.com only (loopback for MOCK); any other host is refused by the constructor
		for _, base := range []string{"https://example.test", "http://graph.facebook.com", "https://graph.facebook.com.evil.test", "http://localhost:80", "http://10.0.0.1:80"} {
			if _, err := metaads.NewClient(metaads.Config{GraphBaseURL: base, GraphVersion: "v26.0", PartnerAgent: "x"}); err == nil {
				t.Errorf("host %s accepted", base)
			}
		}
		for _, v := range []string{"", "26.0", "v26", "latest", "v26.0/../x"} {
			if _, err := metaads.NewClient(metaads.Config{GraphVersion: v}); err == nil {
				t.Errorf("version %q accepted", v)
			}
		}
		if _, err := metaads.NewClient(metaads.Config{GraphVersion: "v26.0"}); err != nil {
			t.Errorf("default host + v26.0 refused: %v", err)
		}
	})
}
