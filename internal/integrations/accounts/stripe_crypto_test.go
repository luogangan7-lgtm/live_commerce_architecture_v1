package accounts

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
)

const (
	stripeTestKey     = "sk_" + "test_ABCDEFGHIJKLMNOP"
	stripeTestNextKey = "rk_" + "test_QRSTUVWXYZabcdef"
	stripeTestWhsec   = "whsec_" + "ABCDEFGHIJKLMNOP"
	stripeTestNextWh  = "whsec_" + "QRSTUVWXYZabcdef"
)

func stripeAPITestScope() StripeAPIScope {
	return StripeAPIScope{
		TenantID: "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa", StoreID: "bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb",
		ConnectionID: "cccccccc-cccc-cccc-cccc-cccccccccccc", Environment: "SANDBOX",
		AccountID: "acct_TestAccount123", CredentialVersion: 1,
	}
}

func stripeWebhookTestScope() StripeWebhookScope {
	a := stripeAPITestScope()
	return StripeWebhookScope{
		TenantID: a.TenantID, StoreID: a.StoreID, ConnectionID: a.ConnectionID,
		EndpointID: "dddddddd-dddd-dddd-dddd-dddddddddddd", Environment: a.Environment,
		AccountID: a.AccountID, Profile: "PROVIDER_MOCK", KeyVersion: 1,
	}
}

func TestStripeAPICustodyRoundTripScopeAndRotation(t *testing.T) {
	k := testKeyring(t)
	scope := stripeAPITestScope()
	want := StripeAPICredentials{stripeTestKey}
	id, nonce, ciphertext, err := k.SealStripeAPI(scope, want)
	if err != nil || id != "current" || len(nonce) != 12 || len(ciphertext) < 17 {
		t.Fatalf("seal failed: id=%q nonce=%d ciphertext=%d err=%v", id, len(nonce), len(ciphertext), err)
	}
	got, err := k.OpenStripeAPI(scope, id, nonce, ciphertext)
	if err != nil || got != want {
		t.Fatalf("round trip failed: result matched=%t err=%v", got == want, err)
	}
	_, secondNonce, secondCiphertext, err := k.SealStripeAPI(scope, want)
	if err != nil || bytes.Equal(nonce, secondNonce) || bytes.Equal(ciphertext, secondCiphertext) {
		t.Fatal("nonce or ciphertext repeated")
	}
	if bytes.Contains(ciphertext, []byte(want.SecretKey)) {
		t.Fatal("plaintext appeared in ciphertext")
	}

	for name, change := range map[string]func(*StripeAPIScope){
		"tenant":      func(s *StripeAPIScope) { s.TenantID = "eeeeeeee-eeee-eeee-eeee-eeeeeeeeeeee" },
		"store":       func(s *StripeAPIScope) { s.StoreID = "eeeeeeee-eeee-eeee-eeee-eeeeeeeeeeee" },
		"connection":  func(s *StripeAPIScope) { s.ConnectionID = "eeeeeeee-eeee-eeee-eeee-eeeeeeeeeeee" },
		"environment": func(s *StripeAPIScope) { s.Environment = "LIVE" },
		"account":     func(s *StripeAPIScope) { s.AccountID = "acct_OtherAccount123" },
		"version":     func(s *StripeAPIScope) { s.CredentialVersion++ },
	} {
		t.Run(name, func(t *testing.T) {
			changed := scope
			change(&changed)
			if _, err := k.OpenStripeAPI(changed, id, nonce, ciphertext); !errors.Is(err, errStripeMaterial) {
				t.Fatal("changed scope opened API material")
			}
		})
	}

	withoutOld, err := NewKeyring("old", map[string][]byte{"old": bytes.Repeat([]byte{0x71}, 32)}, bytes.Repeat([]byte{0xa4}, 32))
	if err != nil {
		t.Fatal(err)
	}
	oldID, oldNonce, oldCipher, err := withoutOld.SealStripeAPI(scope, want)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := k.OpenStripeAPI(scope, oldID, oldNonce, oldCipher); err != nil || got != want {
		t.Fatal("historical API key failed after active-key rotation")
	}
	if _, err := withoutOld.OpenStripeAPI(scope, id, nonce, ciphertext); !errors.Is(err, errStripeMaterial) {
		t.Fatal("missing encryption key accepted")
	}
}

func TestStripeWebhookCustodyPurposeAndScope(t *testing.T) {
	k := testKeyring(t)
	scope := stripeWebhookTestScope()
	apiScope := stripeAPITestScope()
	for _, want := range []StripeWebhookSecrets{{CurrentSecret: stripeTestWhsec},
		{CurrentSecret: stripeTestWhsec, NextSecret: stripeTestNextWh}} {
		id, nonce, ciphertext, err := k.SealStripeWebhook(scope, want)
		if err != nil {
			t.Fatal(err)
		}
		got, err := k.OpenStripeWebhook(scope, id, nonce, ciphertext)
		if err != nil || got != want {
			t.Fatalf("webhook round trip failed: result matched=%t err=%v", got == want, err)
		}
		if bytes.Contains(ciphertext, []byte(want.CurrentSecret)) ||
			(want.NextSecret != "" && bytes.Contains(ciphertext, []byte(want.NextSecret))) {
			t.Fatal("plaintext appeared in ciphertext")
		}
		if _, err := k.OpenStripeAPI(apiScope, id, nonce, ciphertext); !errors.Is(err, errStripeMaterial) {
			t.Fatal("webhook material opened as API material")
		}
	}

	id, nonce, ciphertext, err := k.SealStripeWebhook(scope, StripeWebhookSecrets{stripeTestWhsec, stripeTestNextWh})
	if err != nil {
		t.Fatal(err)
	}
	oldOnly, err := NewKeyring("old", map[string][]byte{"old": bytes.Repeat([]byte{0x71}, 32)}, bytes.Repeat([]byte{0xa4}, 32))
	if err != nil {
		t.Fatal(err)
	}
	oldID, oldNonce, oldCipher, err := oldOnly.SealStripeWebhook(scope, StripeWebhookSecrets{CurrentSecret: stripeTestWhsec})
	if err != nil {
		t.Fatal(err)
	}
	if got, err := k.OpenStripeWebhook(scope, oldID, oldNonce, oldCipher); err != nil || got.CurrentSecret != stripeTestWhsec {
		t.Fatal("historical webhook secret failed after active-key rotation")
	}
	if _, err := oldOnly.OpenStripeWebhook(scope, id, nonce, ciphertext); !errors.Is(err, errStripeMaterial) {
		t.Fatal("missing encryption key opened webhook material")
	}
	for name, change := range map[string]func(*StripeWebhookScope){
		"tenant":      func(s *StripeWebhookScope) { s.TenantID = "eeeeeeee-eeee-eeee-eeee-eeeeeeeeeeee" },
		"store":       func(s *StripeWebhookScope) { s.StoreID = "eeeeeeee-eeee-eeee-eeee-eeeeeeeeeeee" },
		"connection":  func(s *StripeWebhookScope) { s.ConnectionID = "eeeeeeee-eeee-eeee-eeee-eeeeeeeeeeee" },
		"endpoint":    func(s *StripeWebhookScope) { s.EndpointID = "eeeeeeee-eeee-eeee-eeee-eeeeeeeeeeee" },
		"environment": func(s *StripeWebhookScope) { s.Environment = "LIVE" },
		"account":     func(s *StripeWebhookScope) { s.AccountID = "acct_OtherAccount123" },
		"profile":     func(s *StripeWebhookScope) { s.Profile = "SANDBOX" },
		"version":     func(s *StripeWebhookScope) { s.KeyVersion++ },
	} {
		t.Run(name, func(t *testing.T) {
			changed := scope
			change(&changed)
			if _, err := k.OpenStripeWebhook(changed, id, nonce, ciphertext); !errors.Is(err, errStripeMaterial) {
				t.Fatal("changed scope opened webhook material")
			}
		})
	}

	apiID, apiNonce, apiCipher, err := k.SealStripeAPI(apiScope, StripeAPICredentials{stripeTestKey})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := k.OpenStripeWebhook(scope, apiID, apiNonce, apiCipher); !errors.Is(err, errStripeMaterial) {
		t.Fatal("API material opened as webhook material")
	}
	payuniAAD := testAAD()
	payuniAAD.AccountID = scope.AccountID
	payID, payNonce, payCipher, err := k.seal(payuniAAD, Credentials{"payuni-key", "payuni-iv"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := k.OpenStripeAPI(apiScope, payID, payNonce, payCipher); !errors.Is(err, errStripeMaterial) {
		t.Fatal("PAYUNi material opened as Stripe API material")
	}
	if _, err := k.open(payuniAAD, apiID, apiNonce, apiCipher); !errors.Is(err, errInvalidSecret) {
		t.Fatal("Stripe material opened as PAYUNi material")
	}
	if _, err := k.open(payuniAAD, id, nonce, ciphertext); !errors.Is(err, errInvalidSecret) {
		t.Fatal("Stripe webhook material opened as PAYUNi material")
	}
}

func TestStripeCustodyValidationAndBoundedOpen(t *testing.T) {
	k := testKeyring(t)
	a := stripeAPITestScope()
	w := stripeWebhookTestScope()
	for _, bad := range []string{"", "sk_" + "live_ABCDEFGHIJKLMNOP", "sk_test_short", "sk_" + "test_ABCDEFGHIJKLMNOP!",
		"sk_test_" + strings.Repeat("A", 241)} {
		if _, _, _, err := k.SealStripeAPI(a, StripeAPICredentials{bad}); !errors.Is(err, errStripeMaterial) {
			t.Fatal("invalid API secret accepted")
		}
	}
	for _, bad := range []StripeWebhookSecrets{{}, {NextSecret: stripeTestNextWh},
		{stripeTestWhsec, stripeTestWhsec}, {CurrentSecret: "whsec_short"},
		{CurrentSecret: "whsec_" + strings.Repeat("x", 250)}} {
		if _, _, _, err := k.SealStripeWebhook(w, bad); !errors.Is(err, errStripeMaterial) {
			t.Fatal("invalid webhook secrets accepted")
		}
	}
	badAPI := a
	badAPI.TenantID = "not-a-uuid"
	if _, _, _, err := k.SealStripeAPI(badAPI, StripeAPICredentials{stripeTestKey}); !errors.Is(err, errStripeMaterial) {
		t.Fatal("invalid API scope accepted")
	}
	badAPI = a
	badAPI.CredentialVersion = 0
	if _, _, _, err := k.SealStripeAPI(badAPI, StripeAPICredentials{stripeTestKey}); !errors.Is(err, errStripeMaterial) {
		t.Fatal("zero API version accepted")
	}
	badAPI = a
	badAPI.AccountID = "other-provider-account"
	if _, _, _, err := k.SealStripeAPI(badAPI, StripeAPICredentials{stripeTestKey}); !errors.Is(err, errStripeMaterial) {
		t.Fatal("invalid API account accepted")
	}
	badWebhook := w
	badWebhook.EndpointID = "not-a-uuid"
	if _, _, _, err := k.SealStripeWebhook(badWebhook, StripeWebhookSecrets{CurrentSecret: stripeTestWhsec}); !errors.Is(err, errStripeMaterial) {
		t.Fatal("invalid endpoint accepted")
	}
	badWebhook = w
	badWebhook.Profile = "LIVE"
	if _, _, _, err := k.SealStripeWebhook(badWebhook, StripeWebhookSecrets{CurrentSecret: stripeTestWhsec}); !errors.Is(err, errStripeMaterial) {
		t.Fatal("LIVE profile accepted")
	}
	badWebhook = w
	badWebhook.KeyVersion = 0
	if _, _, _, err := k.SealStripeWebhook(badWebhook, StripeWebhookSecrets{CurrentSecret: stripeTestWhsec}); !errors.Is(err, errStripeMaterial) {
		t.Fatal("zero webhook version accepted")
	}
	badWebhook = w
	badWebhook.AccountID = "other-provider-account"
	if _, _, _, err := k.SealStripeWebhook(badWebhook, StripeWebhookSecrets{CurrentSecret: stripeTestWhsec}); !errors.Is(err, errStripeMaterial) {
		t.Fatal("invalid webhook account accepted")
	}

	id, nonce, ciphertext, err := k.SealStripeAPI(a, StripeAPICredentials{stripeTestKey})
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		id     string
		nonce  []byte
		cipher []byte
	}{
		{"missing", nonce, ciphertext}, {id, nonce[:11], ciphertext},
		{id, nonce, ciphertext[:16]}, {id, nonce, make([]byte, maxStripeCiphertext+1)},
		{id, bytes.Repeat([]byte{0}, 12), ciphertext},
	} {
		if _, err := k.OpenStripeAPI(a, tc.id, tc.nonce, tc.cipher); !errors.Is(err, errStripeMaterial) {
			t.Fatal("bad encrypted material accepted")
		}
	}
	webID, webNonce, webCipher, err := k.SealStripeWebhook(w, StripeWebhookSecrets{CurrentSecret: stripeTestWhsec})
	if err != nil {
		t.Fatal(err)
	}
	webTampered := bytes.Clone(webCipher)
	webTampered[len(webTampered)-1] ^= 1
	if _, err := k.OpenStripeWebhook(w, webID, webNonce, webTampered); !errors.Is(err, errStripeMaterial) {
		t.Fatal("tampered webhook ciphertext accepted")
	}
	if _, _, _, err := (*Keyring)(nil).SealStripeAPI(a, StripeAPICredentials{stripeTestKey}); !errors.Is(err, errStripeMaterial) {
		t.Fatal("nil keyring accepted")
	}
	if _, err := (*Keyring)(nil).OpenStripeAPI(a, id, nonce, ciphertext); !errors.Is(err, errStripeMaterial) {
		t.Fatal("nil keyring opened material")
	}
}

func TestStripeCustodyWireStrictnessAndRedaction(t *testing.T) {
	k := testKeyring(t)
	a := stripeAPITestScope()
	w := stripeWebhookTestScope()
	for _, raw := range []string{
		`{}`, `{"secret_key":"` + stripeTestKey + `","unexpected":1}`,
		`{"secret_key":"` + stripeTestKey + `","secret_key":"` + stripeTestNextKey + `"}`,
		`{"secret_key":"sk_" + "live_ABCDEFGHIJKLMNOP"}`,
	} {
		id, nonce, cipher, err := k.sealStripe([]byte(raw), apiAssociated(a))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := k.OpenStripeAPI(a, id, nonce, cipher); !errors.Is(err, errStripeMaterial) {
			t.Fatal("invalid decrypted API payload accepted")
		}
	}
	for _, raw := range []string{`{}`, `{"current_secret":"` + stripeTestWhsec + `","unknown":1}`,
		`{"current_secret":"` + stripeTestWhsec + `","next_secret":"` + stripeTestWhsec + `"}`} {
		id, nonce, cipher, err := k.sealStripe([]byte(raw), webhookAssociated(w))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := k.OpenStripeWebhook(w, id, nonce, cipher); !errors.Is(err, errStripeMaterial) {
			t.Fatal("invalid decrypted webhook payload accepted")
		}
	}

	api := StripeAPICredentials{stripeTestKey}
	webhook := StripeWebhookSecrets{stripeTestWhsec, stripeTestNextWh}
	for _, value := range []any{api, webhook, struct{ Material StripeAPICredentials }{api},
		struct{ Material StripeWebhookSecrets }{webhook}} {
		encoded, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		for _, rendered := range []string{fmt.Sprintf("%v", value), fmt.Sprintf("%+v", value),
			fmt.Sprintf("%#v", value), string(encoded)} {
			if strings.Contains(rendered, stripeTestKey) || strings.Contains(rendered, stripeTestWhsec) ||
				strings.Contains(rendered, stripeTestNextWh) {
				t.Fatal("secret-bearing value rendered plaintext")
			}
		}
	}
}

func TestStripeCustodyAADDiscriminators(t *testing.T) {
	for _, tc := range []struct {
		associated []byte
		purpose    string
		fields     int
	}{
		{apiAssociated(stripeAPITestScope()), "stripe-api-v1", 8},
		{webhookAssociated(stripeWebhookTestScope()), "stripe-webhook-v1", 10},
	} {
		var fields map[string]any
		if err := json.Unmarshal(tc.associated, &fields); err != nil {
			t.Fatal(err)
		}
		if fields["purpose"] != tc.purpose || fields["provider"] != "stripe" || len(fields) != tc.fields {
			t.Fatal("Stripe AAD purpose, provider or exact scope fields changed")
		}
	}
}
