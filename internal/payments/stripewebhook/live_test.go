// live_test.go: package-internal unit tests of LIVE admission at the webhook ingress
// (contracts/stripe-live-enable-v1.md §5.2, §5.4): NewInbox/NewHandler admit LIVE, an endpoint of another
// profile is the fixed 404 in both directions, and a correctly signed livemode=true event on a LIVE endpoint is
// admitted. Non-goals: SQL quarantine of livemode_mismatch (SL05, independent unit), River, real Stripe.
// Callers: go test ./internal/payments/stripewebhook (names avoid the TestStripeSL prefix owned by stripe-live-tests).

package stripewebhook

import (
	"context"
	"errors"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"livecommerce/internal/integrations/accounts"
	"livecommerce/internal/integrations/psp/stripe/stripetest"
)

const liveEventBody = `{"id":"evt_live_1","object":"event","type":"checkout.session.completed","livemode":true,"created":1789995000,` +
	`"api_version":"2026-08-26.dahlia","data":{"object":{"object":"checkout.session","id":"cs_live_1",` +
	`"client_reference_id":"","metadata":{}}}}`

// liveFixture is fixture() with a LIVE endpoint (environment LIVE, profile LIVE) and a LIVE inbox.
func liveFixture(t *testing.T, endpointProfile, inboxProfile string) (*handler, *fakeStore, time.Time) {
	t.Helper()
	keys := keyring(t)
	env := "LIVE"
	if endpointProfile != "LIVE" {
		env = "SANDBOX"
	}
	scope := accounts.StripeWebhookScope{TenantID: tenantID, StoreID: storeID, ConnectionID: connID,
		EndpointID: endpointID, Environment: env, AccountID: account, Profile: endpointProfile, KeyVersion: 3}
	keyID, nonce, ct, err := keys.SealStripeWebhook(scope, accounts.StripeWebhookSecrets{CurrentSecret: secretA})
	if err != nil {
		t.Fatal(err)
	}
	fs := &fakeStore{found: true, m: material{TenantID: tenantID, StoreID: storeID, ConnectionID: connID,
		Environment: env, AccountID: account, Profile: endpointProfile, KeyID: keyID, KeyVersion: 3, Nonce: nonce, Ciphertext: ct}}
	now := time.Unix(1789995000, 0)
	h, err := NewHandler(&Inbox{store: fs, keys: keys, profile: inboxProfile, now: func() time.Time { return now }})
	if err != nil {
		t.Fatalf("handler for %s: %v", inboxProfile, err)
	}
	return h.(*handler), fs, now
}

func TestNewHandlerAdmitsLiveAndOnlyKnownProfiles(t *testing.T) {
	for _, profile := range []string{"PROVIDER_MOCK", "SANDBOX", "LIVE"} {
		if _, err := NewHandler(&Inbox{store: &fakeStore{}, keys: keyring(t), profile: profile, now: time.Now}); err != nil {
			t.Fatalf("%s refused: %v", profile, err)
		}
	}
	for _, profile := range []string{"", "live", "PRODUCTION", "LIVE "} {
		if _, err := NewHandler(&Inbox{store: &fakeStore{}, keys: keyring(t), profile: profile, now: time.Now}); !errors.Is(err, ErrConfig) {
			t.Fatalf("%q admitted: %v", profile, err)
		}
	}
}

// NewInbox: LIVE must pass the config gate and stop at the pool authority check (ErrDatabase); an unknown
// profile is ErrConfig. The pool is lazy and points at a closed local port, so no connection succeeds.
func TestNewInboxLiveReachesPoolValidation(t *testing.T) {
	u := url.URL{Scheme: "postgres", User: url.User("live-test-operator"), Host: "127.0.0.1:1", Path: "/x"}
	pool, err := pgxpool.New(context.Background(), u.String())
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := NewInbox(ctx, pool, keyring(t), "LIVE"); !errors.Is(err, ErrDatabase) {
		t.Fatalf("LIVE profile: want the pool check (ErrDatabase), got %v", err)
	}
	if _, err := NewInbox(ctx, pool, keyring(t), "PRODUCTION"); !errors.Is(err, ErrConfig) {
		t.Fatalf("unknown profile: %v", err)
	}
}

func TestLiveEndpointOnLiveIngressAdmitsSignedLiveEvent(t *testing.T) {
	h, fs, now := liveFixture(t, "LIVE", "LIVE")
	sig := map[string]string{"Stripe-Signature": signWithA(now, liveEventBody)}
	expect(t, post(h, routePrefix+endpointID, liveEventBody, sig), 200, `{"received":true}`)
	if len(fs.admitted) != 1 || !fs.admitted[0].Livemode {
		t.Fatalf("admitted %+v", fs.admitted)
	}
	// A wrong signature is still refused before anything is admitted.
	bad := map[string]string{"Stripe-Signature": signWithA(now, strings.Replace(liveEventBody, "evt_live_1", "evt_live_2", 1))}
	rec := post(h, routePrefix+endpointID, liveEventBody, bad)
	if rec.Code != 400 || len(fs.admitted) != 1 {
		t.Fatalf("bad signature: %d admitted=%d", rec.Code, len(fs.admitted))
	}
}

func TestWrongProfileEndpointIsFixed404InBothDirections(t *testing.T) {
	for _, tc := range []struct{ endpointProfile, inboxProfile string }{{"SANDBOX", "LIVE"}, {"LIVE", "SANDBOX"}, {"LIVE", "PROVIDER_MOCK"}} {
		h, fs, now := liveFixture(t, tc.endpointProfile, tc.inboxProfile)
		body := eventBody
		if tc.endpointProfile == "LIVE" {
			body = liveEventBody
		}
		rec := post(h, routePrefix+endpointID, body, map[string]string{"Stripe-Signature": signWithA(now, body)})
		expect(t, rec, 404, `{"error":"not_found"}`)
		if len(fs.admitted) != 0 {
			t.Fatalf("%s endpoint admitted on %s ingress", tc.endpointProfile, tc.inboxProfile)
		}
	}
}

func signWithA(now time.Time, body string) string {
	return stripetest.SignWebhook(secretA, []byte(body), now)
}
