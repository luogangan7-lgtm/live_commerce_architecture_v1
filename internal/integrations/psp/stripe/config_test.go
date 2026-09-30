// config_test.go: SP01 (key/profile/flag matrix, WebhookConfig bounds, redaction) and
// the UNIT part of SP19 (LIVE refusal). Non-goal: no network and no SQL/CLI parts of SP19.
// Callers: go test ./internal/integrations/psp/stripe/...

package stripe

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
)

func TestStripeSP01Config(t *testing.T) {
	approved := LiveApproval{Enabled: true, Reference: "OWNER-APPROVAL:2026.09"}
	cases := []struct {
		name string
		cfg  Config
		mock bool
		want error
	}{
		{"sk_test sandbox", sandboxConfig(), false, nil},
		{"rk_test sandbox", Config{SecretKey: fakeRAKTestKey, AccountID: fakeAccount, Environment: "SANDBOX"}, false, nil},
		{"sk_test sandbox mock", sandboxConfig(), true, nil},
		{"live key in SANDBOX", Config{SecretKey: fakeLiveKey, AccountID: fakeAccount, Environment: "SANDBOX"}, false, ErrLiveRefused},
		{"live key on mock even when approved", Config{SecretKey: fakeLiveKey, AccountID: fakeAccount, Environment: "LIVE", Live: approved}, true, ErrLiveRefused},
		{"test key in LIVE", Config{SecretKey: fakeTestKey, AccountID: fakeAccount, Environment: "LIVE", Live: approved}, false, ErrLiveRefused},
		{"test key with live flag", Config{SecretKey: fakeTestKey, AccountID: fakeAccount, Environment: "SANDBOX", Live: LiveApproval{Enabled: true}}, false, ErrLiveRefused},
		{"test key with approval ref", Config{SecretKey: fakeTestKey, AccountID: fakeAccount, Environment: "SANDBOX", Live: LiveApproval{Reference: "OWNER-APPROVAL:2026"}}, false, ErrLiveRefused},
		{"live key LIVE no approval", Config{SecretKey: fakeLiveKey, AccountID: fakeAccount, Environment: "LIVE"}, false, ErrLiveRefused},
		{"live key LIVE flag without ref", Config{SecretKey: fakeLiveKey, AccountID: fakeAccount, Environment: "LIVE", Live: LiveApproval{Enabled: true}}, false, ErrLiveRefused},
		{"live key LIVE ref without flag", Config{SecretKey: fakeLiveKey, AccountID: fakeAccount, Environment: "LIVE", Live: LiveApproval{Reference: "OWNER-APPROVAL:2026"}}, false, ErrLiveRefused},
		{"live key LIVE short ref", Config{SecretKey: fakeLiveKey, AccountID: fakeAccount, Environment: "LIVE", Live: LiveApproval{Enabled: true, Reference: "short"}}, false, ErrLiveRefused},
		{"live key LIVE bad ref chars", Config{SecretKey: fakeLiveKey, AccountID: fakeAccount, Environment: "LIVE", Live: LiveApproval{Enabled: true, Reference: "owner approval ok"}}, false, ErrLiveRefused},
		{"live key LIVE approved", Config{SecretKey: fakeRAKLiveKey, AccountID: fakeAccount, Environment: "LIVE", Live: approved}, false, nil}, // stripe-live-core S2: was sk_live_; only rk_live_ is admitted now
		{"sk_live LIVE approved refused", Config{SecretKey: fakeLiveKey, AccountID: fakeAccount, Environment: "LIVE", Live: approved}, false, ErrLiveRefused},
		{"PROVIDER_MOCK env name", Config{SecretKey: fakeTestKey, AccountID: fakeAccount, Environment: "PROVIDER_MOCK"}, false, ErrInvalid},
		{"lowercase env", Config{SecretKey: fakeTestKey, AccountID: fakeAccount, Environment: "sandbox"}, false, ErrInvalid},
		{"empty key", Config{AccountID: fakeAccount, Environment: "SANDBOX"}, false, ErrInvalid},
		{"publishable key", Config{SecretKey: "pk_" + "test_FAKESENTINELKEY0000000000", AccountID: fakeAccount, Environment: "SANDBOX"}, false, ErrInvalid},
		{"key with whitespace", Config{SecretKey: " " + fakeTestKey, AccountID: fakeAccount, Environment: "SANDBOX"}, false, ErrInvalid},
		{"key trailing newline", Config{SecretKey: fakeTestKey + "\n", AccountID: fakeAccount, Environment: "SANDBOX"}, false, ErrInvalid},
		{"key too short", Config{SecretKey: "sk_test_short", AccountID: fakeAccount, Environment: "SANDBOX"}, false, ErrInvalid},
		{"key bad chars", Config{SecretKey: "sk_test_FAKE-SENTINEL-KEY-000000", AccountID: fakeAccount, Environment: "SANDBOX"}, false, ErrInvalid},
		{"bad account", Config{SecretKey: fakeTestKey, AccountID: "acct_", Environment: "SANDBOX"}, false, ErrInvalid},
		{"account not acct", Config{SecretKey: fakeTestKey, AccountID: "cus_123", Environment: "SANDBOX"}, false, ErrInvalid},
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
			if err != nil && containsAny(err.Error(), fakeTestKey, fakeLiveKey, fakeRAKTestKey) {
				t.Fatal("error leaks key")
			}
		})
	}
	if _, err := NewWithMockTransport(sandboxConfig(), nil); !errors.Is(err, ErrInvalid) {
		t.Fatalf("nil transport: %v", err)
	}

	// Refusal codes are fixed and grep-able.
	_, err := New(Config{SecretKey: fakeRAKLiveKey, AccountID: fakeAccount, Environment: "LIVE"}) // S2: rk_live_ reaches the pair check
	if err == nil || err.Error() != "stripe: stripe_live_refused" {
		t.Fatalf("live refusal code: %v", err)
	}
	_, err = New(Config{SecretKey: fakeTestKey, AccountID: fakeAccount, Environment: "LIVE"})
	if err == nil || err.Error() != "stripe: stripe_key_mode_mismatch" {
		t.Fatalf("mode mismatch code: %v", err)
	}

	t.Run("WebhookConfig bounds", func(t *testing.T) {
		bad := []WebhookConfig{
			{AccountID: fakeAccount, Environment: "SANDBOX"},
			{Secrets: []string{fakeWhsecA, fakeWhsecB, "whsec_" + "third_secret_value_000"}, AccountID: fakeAccount, Environment: "SANDBOX"},
			{Secrets: []string{fakeWhsecA, fakeWhsecA}, AccountID: fakeAccount, Environment: "SANDBOX"},
			{Secrets: []string{"whsec_short"}, AccountID: fakeAccount, Environment: "SANDBOX"},
			{Secrets: []string{"whsec_has space in secret 000"}, AccountID: fakeAccount, Environment: "SANDBOX"},
			{Secrets: []string{"sk_" + "test_not_a_webhook_secret0"}, AccountID: fakeAccount, Environment: "SANDBOX"},
			{Secrets: []string{"whsec_" + strings.Repeat("a", 250)}, AccountID: fakeAccount, Environment: "SANDBOX"},
			{Secrets: []string{fakeWhsecA}, AccountID: "bad", Environment: "SANDBOX"},
			{Secrets: []string{fakeWhsecA}, AccountID: fakeAccount, Environment: "PROVIDER_MOCK"},
		}
		for i, cfg := range bad {
			if _, err := NewWebhookVerifier(cfg); !errors.Is(err, ErrInvalid) {
				t.Fatalf("case %d admitted: %v", i, err)
			}
		}
		for _, cfg := range []WebhookConfig{
			{Secrets: []string{fakeWhsecA}, AccountID: fakeAccount, Environment: "SANDBOX"},
			{Secrets: []string{fakeWhsecA, fakeWhsecB}, AccountID: fakeAccount, Environment: "LIVE"},
			{Secrets: []string{"whsec_" + strings.Repeat("~", 249)}, AccountID: fakeAccount, Environment: "SANDBOX"},
		} {
			if _, err := NewWebhookVerifier(cfg); err != nil {
				t.Fatalf("valid webhook config refused: %v", err)
			}
		}
	})

	t.Run("redaction", func(t *testing.T) {
		client, err := NewWithMockTransport(sandboxConfig(), http.DefaultTransport)
		if err != nil {
			t.Fatal(err)
		}
		wcfg := WebhookConfig{Secrets: []string{fakeWhsecA, fakeWhsecB}, AccountID: fakeAccount, Environment: "SANDBOX"}
		verifier, err := NewWebhookVerifier(wcfg)
		if err != nil {
			t.Fatal(err)
		}
		sess := Session{ID: "cs_test_1", url: fxSessionURL}
		values := []any{sandboxConfig(), *client, client, sess, &sess, Event{ID: "evt_1"},
			CallMeta{HTTPStatus: 500, ErrorType: "api_error"}, wcfg, *verifier, verifier,
			struct{ C Config }{sandboxConfig()}, []Session{sess}}
		for _, v := range values {
			outs := []string{fmt.Sprintf("%v", v), fmt.Sprintf("%+v", v), fmt.Sprintf("%#v", v), fmt.Sprintf("%s", v)}
			j, err := json.Marshal(v)
			if err != nil {
				t.Fatalf("marshal %T: %v", v, err)
			}
			outs = append(outs, string(j))
			for _, o := range outs {
				if containsAny(o, fakeTestKey, fakeWhsecA, fakeWhsecB, fxSessionURL, "SENTINELURL") {
					t.Fatalf("%T leaks a secret or URL: %q", v, o)
				}
			}
		}
		if sess.URL() != fxSessionURL {
			t.Fatal("URL accessor")
		}
	})
}

// TestStripeSP19LiveRefusal (UNIT part): LIVE cannot be reached from config without the
// explicit approval pair, the mock transport never admits a live key, and no test in
// this package constructs an approved live client that performs a call.
func TestStripeSP19LiveRefusal(t *testing.T) {
	for _, cfg := range []Config{
		{SecretKey: fakeLiveKey, AccountID: fakeAccount, Environment: "LIVE"},
		{SecretKey: fakeLiveKey, AccountID: fakeAccount, Environment: "LIVE", Live: LiveApproval{Enabled: true}},
		{SecretKey: "rk_" + "live_FAKESENTINELKEY0000000000", AccountID: fakeAccount, Environment: "LIVE", Live: LiveApproval{Reference: "OWNER-APPROVAL:2026"}},
		{SecretKey: fakeLiveKey, AccountID: fakeAccount, Environment: "SANDBOX"},
	} {
		if _, err := New(cfg); !errors.Is(err, ErrLiveRefused) {
			t.Fatalf("live config admitted: %v", err)
		}
	}
	approved := Config{SecretKey: fakeLiveKey, AccountID: fakeAccount, Environment: "LIVE",
		Live: LiveApproval{Enabled: true, Reference: "OWNER-APPROVAL:2026.09"}}
	calls := 0
	rt := roundTripFunc(func(*http.Request) (*http.Response, error) { calls++; return nil, errors.New("unreachable") })
	if _, err := NewWithMockTransport(approved, rt); !errors.Is(err, ErrLiveRefused) {
		t.Fatalf("mock admitted live key: %v", err)
	}
	// A LIVE client (approved) still refuses a non-LIVE profile before any send.
	c := newClient(approved, false, rt)
	if _, _, err := c.CreateCheckoutSession(t.Context(), fixtureParams()); !errors.Is(err, ErrInvalid) || calls != 0 {
		t.Fatalf("LIVE client sent a SANDBOX profile: err=%v calls=%d", err, calls)
	}
}
