// handler_test.go: unit tests for request-shape refusals, per-endpoint material handling,
// signature ordering and the no-ACK-before-commit rule, run against a fake store (MOCK tier).
// Non-goal: SQL, River and role behavior; those are the SP13 REAL_PG gates (NOT_RUN here).
// Callers: go test ./internal/payments/stripewebhook/
package stripewebhook

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"livecommerce/internal/integrations/accounts"
	"livecommerce/internal/integrations/psp/stripe"
	"livecommerce/internal/integrations/psp/stripe/stripetest"
)

const (
	endpointID = "7a1d2c3b-4e5f-4a6b-8c7d-9e0f1a2b3c4d"
	tenantID   = "11111111-1111-4111-8111-111111111111"
	storeID    = "22222222-2222-4222-8222-222222222222"
	connID     = "33333333-3333-4333-8333-333333333333"
	account    = "acct_1TestAccount000"
	secretA    = "whsec_" + "handlerTestSecretA_0123456789"
	secretB    = "whsec_" + "handlerTestSecretB_9876543210"
	eventBody  = `{"id":"evt_1","object":"event","type":"checkout.session.completed","livemode":false,"created":1789995000,` +
		`"api_version":"2026-08-26.dahlia","data":{"object":{"object":"checkout.session","id":"cs_test_1",` +
		`"client_reference_id":"","metadata":{}}}}`
)

type fakeStore struct {
	mu       sync.Mutex
	m        material
	found    bool
	matErr   error
	admitErr error
	admitted []stripe.Event
	block    chan struct{}
}

func (f *fakeStore) material(context.Context, string) (material, bool, error) {
	return f.m, f.found, f.matErr
}

func (f *fakeStore) admit(_ context.Context, _ string, _ material, ev stripe.Event) error {
	if f.block != nil {
		<-f.block
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.admitErr != nil {
		return f.admitErr
	}
	f.admitted = append(f.admitted, ev)
	return nil
}

func keyring(t *testing.T) *accounts.Keyring {
	t.Helper()
	k, err := accounts.NewKeyring("k1", map[string][]byte{"k1": bytes.Repeat([]byte{7}, 32)}, bytes.Repeat([]byte{9}, 32))
	if err != nil {
		t.Fatal(err)
	}
	return k
}

func fixture(t *testing.T, next string) (*handler, *fakeStore, time.Time) {
	t.Helper()
	keys := keyring(t)
	scope := accounts.StripeWebhookScope{TenantID: tenantID, StoreID: storeID, ConnectionID: connID,
		EndpointID: endpointID, Environment: "SANDBOX", AccountID: account, Profile: "SANDBOX", KeyVersion: 3}
	keyID, nonce, ct, err := keys.SealStripeWebhook(scope, accounts.StripeWebhookSecrets{CurrentSecret: secretA, NextSecret: next})
	if err != nil {
		t.Fatal(err)
	}
	fs := &fakeStore{found: true, m: material{TenantID: tenantID, StoreID: storeID, ConnectionID: connID,
		Environment: "SANDBOX", AccountID: account, Profile: "SANDBOX", KeyID: keyID, KeyVersion: 3, Nonce: nonce, Ciphertext: ct}}
	now := time.Unix(1789995000, 0)
	h, err := NewHandler(&Inbox{store: fs, keys: keys, profile: "SANDBOX", now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	return h.(*handler), fs, now
}

func post(h http.Handler, path, body string, hdr map[string]string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func expect(t *testing.T, rec *httptest.ResponseRecorder, status int, body string) {
	t.Helper()
	if rec.Code != status || rec.Body.String() != body || rec.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("got %d %q cache=%q; want %d %q", rec.Code, rec.Body.String(), rec.Header().Get("Cache-Control"), status, body)
	}
}

func TestRequestShapeRefusals(t *testing.T) {
	h, fs, now := fixture(t, "")
	sig := map[string]string{"Stripe-Signature": stripetest.SignWebhook(secretA, []byte(eventBody), now)}
	get := httptest.NewRecorder()
	h.ServeHTTP(get, httptest.NewRequest(http.MethodGet, routePrefix+endpointID, nil))
	expect(t, get, 405, `{"error":"method_not_allowed"}`)
	if get.Header().Get("Allow") != "POST" {
		t.Fatal("405 without Allow: POST")
	}
	for _, p := range []string{"/v1/stripe/webhook", "/v1/stripe/webhook/", routePrefix + strings.ToUpper(endpointID),
		routePrefix + endpointID + "/", routePrefix + "not-a-uuid", routePrefix + "%37a1d2c3b-4e5f-4a6b-8c7d-9e0f1a2b3c4d"} {
		expect(t, post(h, p, eventBody, sig), 404, `{"error":"not_found"}`)
	}
	expect(t, post(h, routePrefix+endpointID+"?", eventBody, sig), 400, `{"error":"invalid_request"}`)
	expect(t, post(h, routePrefix+endpointID+"?x=1", eventBody, sig), 400, `{"error":"invalid_request"}`)
	for _, hdr := range []map[string]string{{"Content-Type": "text/plain"}, {"Content-Type": "application/json; charset=latin1"},
		{"Content-Encoding": "gzip"}} {
		expect(t, post(h, routePrefix+endpointID, eventBody, hdr), 415, `{"error":"unsupported_media_type"}`)
	}
	expect(t, post(h, routePrefix+endpointID, strings.Repeat("x", maxBody+1), sig), 413, `{"error":"payload_too_large"}`)
	if len(fs.admitted) != 0 {
		t.Fatal("refused request reached admission")
	}
}

func TestBusyIsNonBlocking(t *testing.T) {
	h, fs, now := fixture(t, "")
	fs.block = make(chan struct{})
	sig := map[string]string{"Stripe-Signature": stripetest.SignWebhook(secretA, []byte(eventBody), now)}
	var wg sync.WaitGroup
	for i := 0; i < maxInFlight; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); post(h, routePrefix+endpointID, eventBody, sig) }()
	}
	deadline := time.Now().Add(3 * time.Second)
	for len(h.sem) < maxInFlight && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	rec := post(h, routePrefix+endpointID, eventBody, sig)
	expect(t, rec, 503, `{"error":"busy"}`)
	if rec.Header().Get("Retry-After") != "5" {
		t.Fatal("busy without Retry-After: 5")
	}
	close(fs.block)
	wg.Wait()
}

func TestMaterialAndSignatureOrdering(t *testing.T) {
	h, fs, now := fixture(t, secretB)
	sigA := map[string]string{"Stripe-Signature": stripetest.SignWebhook(secretA, []byte(eventBody), now)}
	sigB := map[string]string{"Stripe-Signature": stripetest.SignWebhook(secretB, []byte(eventBody), now)}
	// Both the current and the next secret verify (rotation window).
	expect(t, post(h, routePrefix+endpointID, eventBody, sigA), 200, `{"received":true}`)
	expect(t, post(h, routePrefix+endpointID, eventBody, sigB), 200, `{"received":true}`)
	if len(fs.admitted) != 2 || fs.admitted[0].ID != "evt_1" {
		t.Fatalf("admitted %d events", len(fs.admitted))
	}
	fs.admitted = nil
	bad := map[string]string{"Stripe-Signature": stripetest.SignWebhook("whsec_"+"someOtherSecret_0123456789ab", []byte(eventBody), now)}
	expect(t, post(h, routePrefix+endpointID, eventBody, bad), 400, `{"error":"invalid_signature"}`)
	expect(t, post(h, routePrefix+endpointID, eventBody, nil), 400, `{"error":"invalid_signature"}`)
	stale := map[string]string{"Stripe-Signature": stripetest.SignWebhook(secretA, []byte(eventBody), now.Add(-10*time.Minute))}
	expect(t, post(h, routePrefix+endpointID, eventBody, stale), 400, `{"error":"invalid_signature"}`)
	if len(fs.admitted) != 0 {
		t.Fatal("unsigned or stale delivery admitted")
	}

	fs.found = false
	expect(t, post(h, routePrefix+endpointID, eventBody, sigA), 404, `{"error":"not_found"}`)
	fs.found = true
	fs.m.Profile = "PROVIDER_MOCK"
	expect(t, post(h, routePrefix+endpointID, eventBody, sigA), 404, `{"error":"not_found"}`)
	fs.m.Profile = "SANDBOX"
	fs.m.KeyVersion = 4 // AAD binds key_version: a mismatching envelope cannot be opened
	expect(t, post(h, routePrefix+endpointID, eventBody, sigA), 503, `{"error":"signing_unavailable"}`)
	fs.m.KeyVersion = 3
	fs.matErr = errors.New("db down")
	expect(t, post(h, routePrefix+endpointID, eventBody, sigA), 503, `{"error":"unavailable"}`)
}

func TestNoACKWithoutCommitAndMalformedType(t *testing.T) {
	h, fs, now := fixture(t, "")
	sig := map[string]string{"Stripe-Signature": stripetest.SignWebhook(secretA, []byte(eventBody), now)}
	fs.admitErr = errors.New("commit failed")
	expect(t, post(h, routePrefix+endpointID, eventBody, sig), 503, `{"error":"unavailable"}`)
	fs.admitErr = nil
	long := strings.Replace(eventBody, "checkout.session.completed", "a."+strings.Repeat("b", 110), 1)
	expect(t, post(h, routePrefix+endpointID, long,
		map[string]string{"Stripe-Signature": stripetest.SignWebhook(secretA, []byte(long), now)}), 200, `{"received":true}`)
	if len(fs.admitted) != 1 || !fs.admitted[0].Malformed {
		t.Fatal("over-long type was not quarantined as malformed")
	}
}

func TestLogsCarryNoSecretsBodyOrSignature(t *testing.T) {
	var logs bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	defer slog.SetDefault(prev)
	h, fs, now := fixture(t, "")
	header := stripetest.SignWebhook(secretA, []byte(eventBody), now)
	fs.admitErr = errors.New("boom " + secretA)
	post(h, routePrefix+endpointID, eventBody, map[string]string{"Stripe-Signature": header})
	post(h, routePrefix+endpointID, eventBody, map[string]string{"Stripe-Signature": "t=1,v1=" + strings.Repeat("0", 64)})
	out := logs.String()
	for _, banned := range []string{"whsec", header, "v1=", "evt_1", "cs_test_1", "checkout.session"} {
		if strings.Contains(out, banned) {
			t.Fatalf("log leaked %q: %s", banned, out)
		}
	}
	if !strings.Contains(out, endpointID) {
		t.Fatal("log lacks endpoint id")
	}
}

func TestNewHandlerRejectsIncompleteInbox(t *testing.T) {
	if _, err := NewHandler(nil); !errors.Is(err, ErrConfig) {
		t.Fatal("nil inbox accepted")
	}
	if _, err := NewHandler(&Inbox{profile: "LIVE"}); !errors.Is(err, ErrConfig) {
		t.Fatal("incomplete inbox accepted")
	}
	if _, err := NewInbox(context.Background(), nil, keyring(t), "SANDBOX"); !errors.Is(err, ErrConfig) {
		t.Fatal("nil pool accepted")
	}
}

// S2: a stalled body must not hold an admission slot or outlive the per-request read deadline.
func TestStalledBodiesDoNotExhaustAdmission(t *testing.T) {
	h, _, now := fixture(t, "")
	h.budget = 400 * time.Millisecond
	srv := httptest.NewServer(h)
	defer srv.Close()
	stalled := func() net.Conn {
		c, err := net.Dial("tcp", srv.Listener.Addr().String())
		if err != nil {
			t.Fatal(err)
		}
		// Valid headers for an unauthenticated request, declared body never sent.
		fmt.Fprintf(c, "POST %s%s HTTP/1.1\r\nHost: x\r\nContent-Type: application/json\r\nContent-Length: 4096\r\n\r\n", routePrefix, endpointID)
		return c
	}
	var conns []net.Conn
	for i := 0; i < maxInFlight+8; i++ {
		conns = append(conns, stalled())
	}
	defer func() {
		for _, c := range conns {
			_ = c.Close()
		}
	}()
	time.Sleep(100 * time.Millisecond) // let the handlers reach the body read
	req, _ := http.NewRequest(http.MethodPost, srv.URL+routePrefix+endpointID, strings.NewReader(eventBody))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Stripe-Signature", stripetest.SignWebhook(secretA, []byte(eventBody), now))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("a fully sent signed delivery got %d while %d bodies were stalled", resp.StatusCode, len(conns))
	}
	// The read deadline releases each stalled request (400 invalid_request) well before ReadTimeout.
	_ = conns[0].SetReadDeadline(time.Now().Add(3 * time.Second))
	st, err := bufio.NewReader(conns[0]).ReadString('\n')
	if err != nil || !strings.Contains(st, "400") {
		t.Fatalf("stalled body not cut by the read deadline: %q %v", st, err)
	}
}
