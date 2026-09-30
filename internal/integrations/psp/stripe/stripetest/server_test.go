// Package stripetest owns fake Stripe wire self-tests.
// It never calls api.stripe.com or admits production credentials.
package stripetest

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"livecommerce/internal/integrations/psp/stripe"
)

func postCreate(t *testing.T, client *http.Client, endpoint, key string, values url.Values) (int, http.Header, []byte, error) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, endpoint+"/v1/checkout/sessions", bytes.NewBufferString(values.Encode()))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Idempotency-Key", key)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := client.Do(req)
	if err != nil {
		return 0, nil, nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	return resp.StatusCode, resp.Header, body, err
}

func testParams() url.Values {
	return url.Values{
		"mode": {"payment"}, "payment_method_types[0]": {"card"},
		"client_reference_id":                    {"0b5e3c1a-7d2f-4e8a-9c61-2f4d8e7a1b30"},
		"line_items[0][price_data][currency]":    {"usd"},
		"line_items[0][price_data][unit_amount]": {"500"},
		"expires_at":                             {"1790002400"},
		"metadata[lc_attempt]":                   {"0b5e3c1a-7d2f-4e8a-9c61-2f4d8e7a1b30"},
	}
}

func TestFakeStripeIdempotencyAndSignature(t *testing.T) {
	s := New("acct_FakeUnit1")
	defer s.Close()
	c := s.http.Client()
	p := testParams()
	status, _, first, err := postCreate(t, c, s.URL(), "same-key", p)
	if err != nil || status != 200 {
		t.Fatalf("first create status=%d err=%v", status, err)
	}
	status, h, replay, err := postCreate(t, c, s.URL(), "same-key", p)
	if err != nil || status != 200 || h.Get("Idempotent-Replayed") != "true" || !bytes.Equal(first, replay) || len(s.SessionIDs()) != 1 {
		t.Fatalf("same-key replay status=%d err=%v", status, err)
	}
	p.Set("line_items[0][price_data][unit_amount]", "600")
	status, _, body, err := postCreate(t, c, s.URL(), "same-key", p)
	if err != nil || status != 400 || !bytes.Contains(body, []byte("idempotency_error")) || len(s.SessionIDs()) != 1 {
		t.Fatalf("parameter mismatch status=%d err=%v", status, err)
	}
	secret := "whsec_fake_secret_not_real_0123456789"
	now := time.Unix(1790000000, 0)
	event := []byte(`{"id":"evt_fake1","object":"event","created":1790000000,"livemode":false,"type":"checkout.session.completed","data":{"object":{"object":"checkout.session","id":"cs_test_fake_1","client_reference_id":"0b5e3c1a-7d2f-4e8a-9c61-2f4d8e7a1b30","metadata":{"lc_attempt":"0b5e3c1a-7d2f-4e8a-9c61-2f4d8e7a1b30"}}}}`)
	verifier, err := stripe.NewWebhookVerifier(stripe.WebhookConfig{Secrets: []string{secret}, AccountID: "acct_FakeUnit1", Environment: "SANDBOX"})
	if err != nil {
		t.Fatal(err)
	}
	sig := SignWebhook(secret, event, now)
	got, err := verifier.Verify(event, sig, now)
	if err != nil || got.Malformed || got.ID != "evt_fake1" {
		t.Fatalf("signed event: malformed=%v err=%v", got.Malformed, err)
	}
	changed := append([]byte(nil), event...)
	changed[len(changed)-2] = 'x'
	if _, err = verifier.Verify(changed, sig, now); !errors.Is(err, stripe.ErrSignature) {
		t.Fatalf("modified raw body: %v", err)
	}
}

func TestFakeStripeCacheAndPreExecutionFaults(t *testing.T) {
	s := New("acct_FakeUnit1")
	defer s.Close()
	c := s.http.Client()
	p := testParams()
	s.SetNextFault(Fault{RateLimit: true})
	status, _, _, err := postCreate(t, c, s.URL(), "rate-key", p)
	if err != nil || status != 429 || len(s.SessionIDs()) != 0 {
		t.Fatalf("429 cached or executed: %d %v", status, err)
	}
	status, _, _, err = postCreate(t, c, s.URL(), "rate-key", p)
	if err != nil || status != 200 || len(s.SessionIDs()) != 1 {
		t.Fatalf("same key after 429: %d %v", status, err)
	}
	s.SetNextFault(Fault{Cached500: true})
	status, _, first, err := postCreate(t, c, s.URL(), "500-key", p)
	if err != nil || status != 500 {
		t.Fatalf("first 500: %d %v", status, err)
	}
	status, h, second, err := postCreate(t, c, s.URL(), "500-key", p)
	if err != nil || status != 500 || h.Get("Idempotent-Replayed") != "true" || !bytes.Equal(first, second) || len(s.SessionIDs()) != 2 {
		t.Fatalf("cached 500: %d %v", status, err)
	}
	s.SetNextFault(Fault{DropAfterExecute: true})
	c.CloseIdleConnections() // avoid net/http's transparent retry on a reused idempotent connection
	_, _, _, err = postCreate(t, c, s.URL(), "drop-key", p)
	if err == nil {
		t.Fatal("drop-after-execute returned a response")
	}
	status, h, _, err = postCreate(t, c, s.URL(), "drop-key", p)
	if err != nil || status != 200 || h.Get("Idempotent-Replayed") != "true" || len(s.SessionIDs()) != 3 {
		t.Fatalf("drop replay: %d %v", status, err)
	}
}

func TestFakeStripeAPIKeyIsolation(t *testing.T) {
	a, b := New("acct_FakeStoreA1"), New("acct_FakeStoreB1")
	defer a.Close()
	defer b.Close()
	keyA, keyB := "sk_test_"+strings.Repeat("A", 24), "sk_test_"+strings.Repeat("B", 24)
	if err := a.RequireAPIKey(keyA); err != nil {
		t.Fatal(err)
	}
	if err := b.RequireAPIKey(keyB); err != nil {
		t.Fatal(err)
	}
	call := func(s *Server, method, path, key string, body url.Values) (int, []byte) {
		t.Helper()
		var encoded string
		if body != nil {
			encoded = body.Encode()
		}
		req, err := http.NewRequest(method, s.URL()+path, strings.NewReader(encoded))
		if err != nil {
			t.Fatal(err)
		}
		if key != "" {
			req.Header.Set("Authorization", "Bearer "+key)
		}
		if method == http.MethodPost && path == "/v1/checkout/sessions" {
			req.Header.Set("Idempotency-Key", "isolated-create")
		}
		resp, err := s.http.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		out, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatal(err)
		}
		if bytes.Contains(out, []byte(keyA)) || bytes.Contains(out, []byte(keyB)) {
			t.Fatal("fake response exposed API key")
		}
		return resp.StatusCode, out
	}
	for _, v := range []struct {
		method, path, key string
		body              url.Values
	}{
		{http.MethodGet, "/v1/account", "", nil},
		{http.MethodGet, "/v1/account", keyB, nil},
		{http.MethodPost, "/v1/checkout/sessions", "", testParams()},
		{http.MethodPost, "/v1/checkout/sessions", keyB, testParams()},
	} {
		if code, _ := call(a, v.method, v.path, v.key, v.body); code != http.StatusUnauthorized {
			t.Fatalf("unauthorized %s %s status=%d", v.method, v.path, code)
		}
	}
	if ids := a.SessionIDs(); len(ids) != 0 || len(a.CreateKeys()) != 0 {
		t.Fatal("denied create touched session or idempotency state")
	}
	if code, _ := call(a, http.MethodGet, "/v1/account", keyA, nil); code != 200 {
		t.Fatalf("correct key account status=%d", code)
	}
	code, raw := call(a, http.MethodPost, "/v1/checkout/sessions", keyA, testParams())
	if code != 200 {
		t.Fatalf("correct key create status=%d", code)
	}
	var made struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(raw, &made); err != nil || made.ID == "" {
		t.Fatalf("created session shape: %v", err)
	}
	if len(a.SessionIDs()) != 1 || len(a.CreateKeys()) != 1 {
		t.Fatal("correct create was not recorded exactly once")
	}
	for _, v := range []struct{ method, path string }{
		{http.MethodGet, "/v1/checkout/sessions/" + made.ID},
		{http.MethodPost, "/v1/checkout/sessions/" + made.ID + "/expire"},
	} {
		if code, _ := call(a, v.method, v.path, keyB, nil); code != http.StatusUnauthorized {
			t.Fatalf("wrong key %s status=%d", v.method, code)
		}
	}
	code, raw = call(a, http.MethodGet, "/v1/checkout/sessions/"+made.ID, keyA, nil)
	if code != 200 || !bytes.Contains(raw, []byte(`"status":"open"`)) {
		t.Fatal("wrong-key expire mutated session state")
	}
	if got := a.Counts(); got.Accepted != 3 || got.Denied != 6 {
		t.Fatalf("auth count accepted=%d denied=%d", got.Accepted, got.Denied)
	}
	if code, _ := call(b, http.MethodGet, "/v1/account", keyA, nil); code != http.StatusUnauthorized {
		t.Fatalf("store A key admitted at B: %d", code)
	}
	if code, _ := call(b, http.MethodGet, "/v1/account", keyB, nil); code != 200 {
		t.Fatalf("store B key refused at B: %d", code)
	}
}

func TestFakeStripeMultiAccountScopeAuditAndFaults(t *testing.T) {
	s := New("acct_FakeDefault1")
	defer s.Close()
	keyA, keyB := "sk_test_"+strings.Repeat("A", 24), "sk_test_"+strings.Repeat("B", 24)
	if err := s.AddAccount("acct_FakeStoreA1", keyA); err != nil {
		t.Fatal(err)
	}
	if err := s.AddAccount("acct_FakeStoreB1", keyB); err != nil {
		t.Fatal(err)
	}
	call := func(method, path, key string, body url.Values, idem string) (int, http.Header, []byte) {
		t.Helper()
		req, err := http.NewRequest(method, s.URL()+path, strings.NewReader(body.Encode()))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer "+key)
		if idem != "" {
			req.Header.Set("Idempotency-Key", idem)
		}
		resp, err := s.http.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		out, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, resp.Header, out
	}
	var acct struct{ ID string }
	for key, want := range map[string]string{keyA: "acct_FakeStoreA1", keyB: "acct_FakeStoreB1"} {
		code, _, out := call(http.MethodGet, "/v1/account", key, nil, "")
		if code != 200 || json.Unmarshal(out, &acct) != nil || acct.ID != want {
			t.Fatalf("account for key: %d %s", code, out)
		}
	}
	// One Idempotency-Key value used by two accounts must create two sessions.
	var idA, idB struct{ ID string }
	code, _, out := call(http.MethodPost, "/v1/checkout/sessions", keyA, testParams(), "shared-key")
	if code != 200 || json.Unmarshal(out, &idA) != nil {
		t.Fatalf("create A: %d", code)
	}
	code, h, out := call(http.MethodPost, "/v1/checkout/sessions", keyB, testParams(), "shared-key")
	if code != 200 || h.Get("Idempotent-Replayed") != "" || json.Unmarshal(out, &idB) != nil || idA.ID == idB.ID {
		t.Fatalf("idempotency cache leaked across accounts: %d", code)
	}
	if code, _, _ := call(http.MethodGet, "/v1/checkout/sessions/"+idA.ID, keyB, nil, ""); code != 404 {
		t.Fatalf("foreign session visible: %d", code)
	}
	if code, _, _ := call(http.MethodPost, "/v1/checkout/sessions/"+idA.ID+"/expire", keyB, nil, "x"); code != 404 {
		t.Fatalf("foreign session expirable: %d", code)
	}
	if code, _, _ := call(http.MethodGet, "/v1/checkout/sessions/"+idA.ID, keyA, nil, ""); code != 200 {
		t.Fatalf("own session hidden: %d", code)
	}
	if code, _, _ := call(http.MethodGet, "/v1/account", "sk_test_"+strings.Repeat("C", 24), nil, ""); code != 401 {
		t.Fatalf("unknown key admitted: %d", code)
	}
	// FailNext is consumed once, per operation, without executing the call.
	s.FailNext("retrieve", 500)
	if code, _, _ := call(http.MethodGet, "/v1/checkout/sessions/"+idA.ID, keyA, nil, ""); code != 500 {
		t.Fatalf("queued retrieve failure: %d", code)
	}
	if code, _, _ := call(http.MethodGet, "/v1/checkout/sessions/"+idA.ID, keyA, nil, ""); code != 200 {
		t.Fatalf("failure not consumed: %d", code)
	}
	s.SetNextFault(Fault{IdempotencyError: true})
	if code, _, out := call(http.MethodPost, "/v1/checkout/sessions", keyA, testParams(), "fresh"); code != 400 || !bytes.Contains(out, []byte("idempotency_error")) {
		t.Fatalf("idempotency fault: %d %s", code, out)
	}
	// Probe sessions need no attempt reference and echo metadata.
	probe := testParams()
	probe.Del("client_reference_id")
	if code, _, _ := call(http.MethodPost, "/v1/checkout/sessions", keyA, probe, "probe-1"); code != 400 {
		t.Fatalf("non-probe without reference accepted: %d", code)
	}
	probe.Set("metadata[lc_probe]", "1")
	code, _, out = call(http.MethodPost, "/v1/checkout/sessions", keyA, probe, "probe-2")
	if code != 200 || !bytes.Contains(out, []byte(`"lc_probe":"1"`)) {
		t.Fatalf("probe create: %d %s", code, out)
	}
	log := s.Requests()
	if len(log) == 0 || log[0].Account == "" || log[0].KeyFingerprint != KeyFingerprint(keyA) && log[0].KeyFingerprint != KeyFingerprint(keyB) {
		t.Fatal("request log empty or key fingerprint missing")
	}
	s.RevokeKey(keyA)
	if code, _, _ := call(http.MethodGet, "/v1/account", keyA, nil, ""); code != 401 {
		t.Fatalf("revoked key admitted: %d", code)
	}
	if code, _, _ := call(http.MethodGet, "/v1/account", keyB, nil, ""); code != 200 {
		t.Fatalf("other key affected by revocation: %d", code)
	}
	var creates int
	for _, r := range log {
		if r.Method == http.MethodPost && r.Path == "/v1/checkout/sessions" && r.IdempotencyKey == "shared-key" {
			creates++
		}
	}
	if creates != 2 {
		t.Fatalf("request log lost keyed creates: %d", creates)
	}
	body := EventBody(EventOpts{ID: "evt_fake_body1", SessionID: "cs_test_x1", ClientRef: "r", Attempt: "a", Created: 1790000000, Probe: true, Connect: true})
	if !bytes.Contains(body, []byte(`"lc_probe":"1"`)) || !bytes.Contains(body, []byte(`"account":"acct_ConnectedElsewhere1"`)) || bytes.Contains(body, []byte(`sk_`)) {
		t.Fatalf("event body: %s", body)
	}
}

func TestFakeStripePatchAndInject(t *testing.T) {
	s := New("acct_FakePatch1")
	defer s.Close()
	c := s.http.Client()
	status, _, out, err := postCreate(t, c, s.URL(), "patch-key", testParams())
	var created struct{ ID string }
	if err != nil || status != 200 || json.Unmarshal(out, &created) != nil {
		t.Fatalf("create: %d %v", status, err)
	}
	s.SetNextFault(Fault{RateLimit: true})
	if !s.FaultPending() {
		t.Fatal("fault not pending")
	}
	if status, _, _, err := postCreate(t, c, s.URL(), "consume", testParams()); err != nil || status != 429 || s.FaultPending() {
		t.Fatalf("fault consumption: %d %v", status, err)
	}
	s.FailNext("list", 500)
	s.ClearFailures()
	if s.failed("list") != 0 {
		t.Fatal("ClearFailures")
	}
	if !s.Patch(created.ID, map[string]any{"amount_total": 1, "presentment_details": map[string]any{"presentment_currency": "eur", "presentment_amount": 9}, "url": nil}) || s.Patch("cs_missing", nil) {
		t.Fatal("patch admission")
	}
	get := func() map[string]any {
		resp, err := c.Get(s.URL() + "/v1/checkout/sessions/" + created.ID)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var m map[string]any
		if err := json.NewDecoder(resp.Body).Decode(&m); err != nil {
			t.Fatal(err)
		}
		return m
	}
	m := get()
	if m["amount_total"].(float64) != 1 || m["amount_subtotal"].(float64) != 500 || m["url"] != nil || m["presentment_details"] == nil {
		t.Fatalf("overlay not applied: %v", m)
	}
	if s.SessionByReference("0b5e3c1a-7d2f-4e8a-9c61-2f4d8e7a1b30") != created.ID || s.SessionByReference("nobody") != "" {
		t.Fatal("SessionByReference")
	}
	dup := s.Inject("acct_FakePatch1", testParams())
	if dup == "" || dup == created.ID || len(s.SessionIDs()) != 2 || len(s.CreateKeys()) != 2 { // the 429 attempt logged its key but created nothing
		t.Fatalf("inject: %q", dup)
	}
}

func TestFakeStripeCached500NoSessionAndAgeSession(t *testing.T) {
	s := New("acct_FakeAge1")
	defer s.Close()
	c := s.http.Client()
	s.SetNextFault(Fault{Cached500NoSession: true})
	status, _, _, err := postCreate(t, c, s.URL(), "nosession", testParams())
	if err != nil || status != 500 || len(s.SessionIDs()) != 0 {
		t.Fatalf("500 without session: %d %v %d", status, err, len(s.SessionIDs()))
	}
	status, h, _, err := postCreate(t, c, s.URL(), "nosession", testParams())
	if err != nil || status != 500 || h.Get("Idempotent-Replayed") != "true" || len(s.SessionIDs()) != 0 {
		t.Fatalf("cached 500 replay: %d %v", status, err)
	}
	status, _, out, err := postCreate(t, c, s.URL(), "real", testParams())
	var created struct {
		ID      string
		Created int64
		Expires int64 `json:"expires_at"`
	}
	if err != nil || status != 200 || json.Unmarshal(out, &created) != nil {
		t.Fatalf("create: %d %v", status, err)
	}
	if !s.AgeSession(created.ID, time.Hour) || s.AgeSession("cs_missing", time.Hour) {
		t.Fatal("age admission")
	}
	resp, err := c.Get(s.URL() + "/v1/checkout/sessions/" + created.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var aged struct {
		Created int64
		Expires int64 `json:"expires_at"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&aged); err != nil || aged.Expires != created.Expires-3600 || aged.Created != created.Created-3600 {
		t.Fatalf("aged session: %+v vs %+v", aged, created)
	}
}
