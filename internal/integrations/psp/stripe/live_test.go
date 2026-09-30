// live_test.go: SL01 (contracts/stripe-live-enable-v1.md §11), the UNIT gate for the LIVE
// key admission matrix, the LIVE probe rules and the readiness projection.
// Non-goals: no network, no SQL, no real key (every key is a split sentinel; a live-shaped
// literal must never exist in the tree, PROCESS §6). Tier: UNIT.
// Callers: go test ./internal/integrations/psp/stripe/... (SL01 red-then-green log in
// output/stripe-live-core/).

package stripe

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestStripeSL01LiveConfig(t *testing.T) {
	t.Run("admission matrix", testSL01Matrix)
	t.Run("live probe", testSL01Probe)
	t.Run("readiness projection", testSL01Readiness)
}

func testSL01Matrix(t *testing.T) {
	approved := LiveApproval{Enabled: true, Reference: "owner-chat:2026-09-30:stripe-live:shop"}
	cases := []struct {
		name     string
		cfg      Config
		mock     bool
		want     error
		wantCode string // "" = not asserted
	}{
		{"rk_live LIVE approved", Config{SecretKey: fakeRAKLiveKey, AccountID: fakeAccount, Environment: "LIVE", Live: approved}, false, nil, ""},
		// LR-3 / S2: the unrestricted key is refused before the pair is even looked at.
		{"sk_live LIVE approved", Config{SecretKey: fakeLiveKey, AccountID: fakeAccount, Environment: "LIVE", Live: approved}, false, ErrLiveRefused, "stripe_live_key_unrestricted"},
		{"sk_live LIVE no pair", Config{SecretKey: fakeLiveKey, AccountID: fakeAccount, Environment: "LIVE"}, false, ErrLiveRefused, "stripe_live_key_unrestricted"},
		{"rk_live LIVE no pair", Config{SecretKey: fakeRAKLiveKey, AccountID: fakeAccount, Environment: "LIVE"}, false, ErrLiveRefused, "stripe_live_refused"},
		{"rk_live LIVE flag without ref", Config{SecretKey: fakeRAKLiveKey, AccountID: fakeAccount, Environment: "LIVE", Live: LiveApproval{Enabled: true}}, false, ErrLiveRefused, "stripe_live_refused"},
		{"rk_live LIVE ref without flag", Config{SecretKey: fakeRAKLiveKey, AccountID: fakeAccount, Environment: "LIVE", Live: LiveApproval{Reference: "owner-chat:2026-09-30"}}, false, ErrLiveRefused, "stripe_live_refused"},
		{"rk_live LIVE short ref", Config{SecretKey: fakeRAKLiveKey, AccountID: fakeAccount, Environment: "LIVE", Live: LiveApproval{Enabled: true, Reference: "short"}}, false, ErrLiveRefused, "stripe_live_refused"},
		{"rk_live in SANDBOX", Config{SecretKey: fakeRAKLiveKey, AccountID: fakeAccount, Environment: "SANDBOX", Live: approved}, false, ErrLiveRefused, "stripe_key_mode_mismatch"},
		{"rk_live on mock", Config{SecretKey: fakeRAKLiveKey, AccountID: fakeAccount, Environment: "LIVE", Live: approved}, true, ErrLiveRefused, "stripe_key_mode_mismatch"},
		{"sk_live on mock", Config{SecretKey: fakeLiveKey, AccountID: fakeAccount, Environment: "LIVE", Live: approved}, true, ErrLiveRefused, "stripe_key_mode_mismatch"},
		{"sk_test in LIVE with pair", Config{SecretKey: fakeTestKey, AccountID: fakeAccount, Environment: "LIVE", Live: approved}, false, ErrLiveRefused, "stripe_key_mode_mismatch"},
		{"rk_test in LIVE with pair", Config{SecretKey: fakeRAKTestKey, AccountID: fakeAccount, Environment: "LIVE", Live: approved}, false, ErrLiveRefused, "stripe_key_mode_mismatch"},
		{"sk_test with live flag", Config{SecretKey: fakeTestKey, AccountID: fakeAccount, Environment: "SANDBOX", Live: LiveApproval{Enabled: true}}, false, ErrLiveRefused, "stripe_key_mode_mismatch"},
		{"rk_test with approval ref", Config{SecretKey: fakeRAKTestKey, AccountID: fakeAccount, Environment: "SANDBOX", Live: LiveApproval{Reference: "owner-chat:2026-09-30"}}, false, ErrLiveRefused, "stripe_key_mode_mismatch"},
		{"rk_test SANDBOX", Config{SecretKey: fakeRAKTestKey, AccountID: fakeAccount, Environment: "SANDBOX"}, false, nil, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var err error
			if tc.mock {
				_, err = NewWithMockTransport(tc.cfg, http.DefaultTransport)
			} else {
				_, err = New(tc.cfg)
			}
			if tc.want == nil && err != nil {
				t.Fatalf("want admitted, got %v", err)
			}
			if tc.want != nil && !errors.Is(err, tc.want) {
				t.Fatalf("want %v, got %v", tc.want, err)
			}
			if tc.wantCode != "" && (err == nil || err.Error() != "stripe: "+tc.wantCode) {
				t.Fatalf("want code %s, got %v", tc.wantCode, err)
			}
			if err != nil && containsAny(err.Error(), fakeTestKey, fakeLiveKey, fakeRAKLiveKey, fakeRAKTestKey) {
				t.Fatal("error leaks key")
			}
		})
	}
}

// liveClient builds a LIVE-admitted client over a stub transport; New would dial the network.
func liveClient(t *testing.T, rt http.RoundTripper) *Client {
	t.Helper()
	cfg := Config{SecretKey: fakeRAKLiveKey, AccountID: fakeAccount, Environment: "LIVE",
		Live: LiveApproval{Enabled: true, Reference: "owner-chat:2026-09-30:stripe-live:shop"}}
	if err := admit(cfg, false); err != nil {
		t.Fatalf("live config not admitted: %v", err)
	}
	return newClient(cfg, false, rt)
}

func liveProbeSession(status string, livemode bool) string {
	s := strings.Replace(probeSession(status), "cs_test_probe1", "cs_live_probe1", 1)
	if livemode {
		s = strings.Replace(s, `"livemode":false`, `"livemode":true`, 1)
	}
	return s
}

func testSL01Probe(t *testing.T) {
	now := time.Unix(1789995000, 0)
	run := func(c *Client) (string, error) {
		id, _, err := c.probeCheckout(context.Background(), now, probeQualification, "HKD", 400, fxReturnURL)
		return id, err
	}
	stub := func(finalStatus string, livemode bool, seen *[]string) http.RoundTripper {
		return roundTripFunc(func(r *http.Request) (*http.Response, error) {
			*seen = append(*seen, r.Method+" "+r.URL.Path)
			reply := liveProbeSession("open", livemode)
			if r.Method == http.MethodGet || strings.HasSuffix(r.URL.Path, "/expire") {
				reply = liveProbeSession(finalStatus, livemode)
			}
			return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(reply)),
				Header: http.Header{"Content-Type": {"application/json"}}}, nil
		})
	}

	var seen []string
	if id, err := run(liveClient(t, stub("expired", true, &seen))); err != nil || id != "cs_live_probe1" || len(seen) != 3 {
		t.Fatalf("LIVE probe with livemode=true+expired+unpaid refused: id=%q err=%v calls=%v", id, err, seen)
	}
	// Still open after expire: not evidence.
	seen = nil
	if _, err := run(liveClient(t, stub("open", true, &seen))); !errors.Is(err, ErrRejected) {
		t.Fatalf("LIVE probe with an open session accepted: %v", err)
	}
	// livemode=false answered to a LIVE client (a SANDBOX object) is never LIVE evidence.
	seen = nil
	if _, err := run(liveClient(t, stub("expired", false, &seen))); err == nil {
		t.Fatal("LIVE probe accepted a livemode=false session")
	}
	// The converse: a SANDBOX client refuses livemode=true.
	seen = nil
	sb := stub("expired", true, &seen)
	c, err := NewWithMockTransport(sandboxConfig(), sb)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := run(c); err == nil {
		t.Fatal("SANDBOX probe accepted a livemode=true session")
	}
	// Input bounds are unchanged in LIVE (I05 amount, uuid, https return URL).
	seen = nil
	lc := liveClient(t, stub("expired", true, &seen))
	if _, _, err := lc.probeCheckout(context.Background(), now, probeQualification, "HKD", 399, fxReturnURL); !errors.Is(err, ErrInvalid) || len(seen) != 0 {
		t.Fatalf("LIVE probe accepted a bad amount: %v calls=%v", err, seen)
	}
}

const fullAccountBody = `{"id":"` + fakeAccount + `","object":"account","charges_enabled":true,"payouts_enabled":true,` +
	`"details_submitted":true,"email":"pii-sentinel@example.test","business_profile":{"name":"PII SENTINEL","support_email":"pii-sentinel@example.test"},` +
	`"requirements":{"currently_due":[],"disabled_reason":null},` +
	`"settings":{"payments":{"statement_descriptor":"DAMENG SHOP"},` +
	`"card_payments":{"statement_descriptor_prefix":"DAMENG","decline_on":{"cvc_failure":true,"avs_failure":false}},` +
	`"payouts":{"schedule":{"interval":"daily"}}}}`

func readinessOf(t *testing.T, body string) (Readiness, error) {
	t.Helper()
	var path string
	rt := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		path = r.Method + " " + r.URL.Path
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body)),
			Header: http.Header{"Content-Type": {"application/json"}}}, nil
	})
	r, _, err := liveClient(t, rt).AccountReadiness(context.Background())
	if err == nil && path != "GET /v1/account" {
		t.Fatalf("unexpected request %q", path)
	}
	return r, err
}

func ptrInt(p *int) string {
	if p == nil {
		return "nil"
	}
	return fmt.Sprint(*p)
}

func ptrBool(p *bool) string {
	if p == nil {
		return "nil"
	}
	return fmt.Sprint(*p)
}

func testSL01Readiness(t *testing.T) {
	r, err := readinessOf(t, fullAccountBody)
	if err != nil {
		t.Fatal(err)
	}
	got := strings.Join([]string{ptrBool(r.ChargesEnabled), ptrBool(r.PayoutsEnabled), ptrBool(r.DetailsSubmitted),
		ptrBool(r.CVCRule), ptrBool(r.AVSRule), ptrInt(r.CurrentlyDueCount), ptrInt(r.DescriptorLength), ptrInt(r.PrefixLength)}, ",")
	if got != "true,true,true,true,false,0,11,6" {
		t.Fatalf("projection = %s", got)
	}
	j, err := r.JSON()
	if err != nil {
		t.Fatal(err)
	}
	want := `{"AVSRule":false,"CVCRule":true,"ChargesEnabled":true,"CurrentlyDueCount":0,"DescriptorLength":11,` +
		`"DetailsSubmitted":true,"PayoutsEnabled":true,"PrefixLength":6}`
	if string(j) != want {
		t.Fatalf("JSON = %s", j)
	}
	if strings.Contains(string(j), "SENTINEL") {
		t.Fatal("PII in readiness JSON")
	}

	// mutate: each row edits one listed path; want is the projection with that field.
	mut := func(from, to string) string {
		if !strings.Contains(fullAccountBody, from) {
			t.Fatalf("fixture drift: %q", from)
		}
		return strings.Replace(fullAccountBody, from, to, 1)
	}
	type row struct {
		name, body string
		check      func(Readiness) bool
	}
	rows := []row{
		{"charges_enabled false is false, not nil", mut(`"charges_enabled":true`, `"charges_enabled":false`),
			func(r Readiness) bool { return r.ChargesEnabled != nil && !*r.ChargesEnabled }},
		{"charges_enabled null is nil", mut(`"charges_enabled":true`, `"charges_enabled":null`),
			func(r Readiness) bool { return r.ChargesEnabled == nil }},
		{"charges_enabled absent is nil", mut(`"charges_enabled":true,`, ``),
			func(r Readiness) bool { return r.ChargesEnabled == nil }},
		{"payouts_enabled absent is nil", mut(`"payouts_enabled":true,`, ``),
			func(r Readiness) bool { return r.PayoutsEnabled == nil }},
		{"details_submitted null is nil", mut(`"details_submitted":true`, `"details_submitted":null`),
			func(r Readiness) bool { return r.DetailsSubmitted == nil }},
		{"requirements absent -> due count nil", mut(`"requirements":{"currently_due":[],"disabled_reason":null},`, ``),
			func(r Readiness) bool { return r.CurrentlyDueCount == nil }},
		{"requirements null -> due count nil", mut(`"requirements":{"currently_due":[],"disabled_reason":null}`, `"requirements":null`),
			func(r Readiness) bool { return r.CurrentlyDueCount == nil }},
		{"currently_due null -> nil", mut(`"currently_due":[]`, `"currently_due":null`),
			func(r Readiness) bool { return r.CurrentlyDueCount == nil }},
		{"currently_due 2 items -> 2", mut(`"currently_due":[]`, `"currently_due":["a","b"]`),
			func(r Readiness) bool { return r.CurrentlyDueCount != nil && *r.CurrentlyDueCount == 2 }},
		{"descriptor null -> nil", mut(`"statement_descriptor":"DAMENG SHOP"`, `"statement_descriptor":null`),
			func(r Readiness) bool { return r.DescriptorLength == nil }},
		{"descriptor counts runes", mut(`"statement_descriptor":"DAMENG SHOP"`, `"statement_descriptor":"大梦大梦大梦"`),
			func(r Readiness) bool { return r.DescriptorLength != nil && *r.DescriptorLength == 6 }},
		{"settings.payments absent -> descriptor nil", mut(`"payments":{"statement_descriptor":"DAMENG SHOP"},`, ``),
			func(r Readiness) bool { return r.DescriptorLength == nil && r.PrefixLength != nil }},
		{"prefix null in present card_payments -> 0", mut(`"statement_descriptor_prefix":"DAMENG"`, `"statement_descriptor_prefix":null`),
			func(r Readiness) bool { return r.PrefixLength != nil && *r.PrefixLength == 0 }},
		{"prefix empty in present card_payments -> 0", mut(`"statement_descriptor_prefix":"DAMENG"`, `"statement_descriptor_prefix":""`),
			func(r Readiness) bool { return r.PrefixLength != nil && *r.PrefixLength == 0 }},
		{"prefix key absent in present card_payments -> 0", mut(`"statement_descriptor_prefix":"DAMENG",`, ``),
			func(r Readiness) bool { return r.PrefixLength != nil && *r.PrefixLength == 0 }},
		{"card_payments absent -> prefix and decline_on nil", mut(`"card_payments":{"statement_descriptor_prefix":"DAMENG","decline_on":{"cvc_failure":true,"avs_failure":false}},`, ``),
			func(r Readiness) bool { return r.PrefixLength == nil && r.CVCRule == nil && r.AVSRule == nil }},
		{"settings absent -> all settings nil", mut(`,"settings":{"payments":{"statement_descriptor":"DAMENG SHOP"},"card_payments":{"statement_descriptor_prefix":"DAMENG","decline_on":{"cvc_failure":true,"avs_failure":false}},"payouts":{"schedule":{"interval":"daily"}}}`, ``),
			func(r Readiness) bool {
				return r.DescriptorLength == nil && r.PrefixLength == nil && r.CVCRule == nil && r.AVSRule == nil && r.ChargesEnabled != nil
			}},
		{"decline_on absent -> rules nil", mut(`,"decline_on":{"cvc_failure":true,"avs_failure":false}`, ``),
			func(r Readiness) bool { return r.CVCRule == nil && r.AVSRule == nil && r.PrefixLength != nil }},
		{"avs_failure null -> nil", mut(`"avs_failure":false`, `"avs_failure":null`),
			func(r Readiness) bool { return r.AVSRule == nil && r.CVCRule != nil }},
	}
	for _, tc := range rows {
		t.Run(tc.name, func(t *testing.T) {
			r, err := readinessOf(t, tc.body)
			if err != nil {
				t.Fatalf("unexpected error %v", err)
			}
			if !tc.check(r) {
				t.Fatalf("projection wrong: %s", fmt.Sprintf("%v", tc.name))
			}
			// Every row that leaves any nil must fail closed when serialised.
			if r.ChargesEnabled == nil || r.PayoutsEnabled == nil || r.DetailsSubmitted == nil || r.CVCRule == nil ||
				r.AVSRule == nil || r.CurrentlyDueCount == nil || r.DescriptorLength == nil || r.PrefixLength == nil {
				if _, err := r.JSON(); err == nil || err.Error() != "stripe: stripe_live_readiness_unknown" || !errors.Is(err, ErrUncertain) {
					t.Fatalf("nil field serialised: %v", err)
				}
			}
		})
	}

	// Wrong types are ErrUncertain, never a partial projection.
	for name, body := range map[string]string{
		"charges_enabled string": mut(`"charges_enabled":true`, `"charges_enabled":"yes"`),
		"due not array":          mut(`"currently_due":[]`, `"currently_due":{}`),
		"descriptor number":      mut(`"statement_descriptor":"DAMENG SHOP"`, `"statement_descriptor":5`),
		"settings number":        mut(`"settings":{`, `"settings":5,"x":{`),
		"cvc string":             mut(`"cvc_failure":true`, `"cvc_failure":"true"`),
		"not an account":         mut(`"object":"account"`, `"object":"person"`),
		"duplicate key":          mut(`"charges_enabled":true,`, `"charges_enabled":true,"charges_enabled":false,`),
		"not json":               `<html>`,
	} {
		if _, err := readinessOf(t, body); !errors.Is(err, ErrUncertain) {
			t.Fatalf("%s: want ErrUncertain, got %v", name, err)
		}
	}
	if _, err := readinessOf(t, mut(`"id":"`+fakeAccount+`"`, `"id":"acct_1OtherAccount000"`)); !errors.Is(err, ErrAuthentication) {
		t.Fatalf("other account: %v", err)
	}
	if _, _, err := (*Client)(nil).AccountReadiness(context.Background()); !errors.Is(err, ErrInvalid) {
		t.Fatalf("nil client: %v", err)
	}

	// Redaction: a Readiness never prints its values (or pointer addresses) on any path.
	for _, v := range []any{r, &r, []Readiness{r}, struct{ R Readiness }{r}} {
		outs := []string{fmt.Sprintf("%v", v), fmt.Sprintf("%+v", v), fmt.Sprintf("%#v", v), fmt.Sprintf("%s", v)}
		b, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		outs = append(outs, string(b))
		for _, o := range outs {
			if !strings.Contains(o, "stripe.Readiness{redacted}") || strings.Contains(o, "0x") || strings.Contains(o, "true") {
				t.Fatalf("%T not redacted: %q", v, o)
			}
		}
	}
}
