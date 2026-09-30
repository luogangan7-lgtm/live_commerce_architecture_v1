package accounts

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/json"
	"errors"
	"regexp"

	"livecommerce/internal/integrations/psp/stripe"
)

// Stripe material has its own wire format and AAD. PAYUNi's credential format
// remains independent, even when both use the same Keyring implementation.
var errStripeMaterial = errors.New("stripe material unavailable")

const maxStripeCiphertext = 8192

var stripeScopeUUID = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

type StripeAPIScope struct {
	TenantID, StoreID, ConnectionID, Environment, AccountID string
	CredentialVersion                                       int64
}

type StripeWebhookScope struct {
	TenantID, StoreID, ConnectionID, EndpointID, Environment, AccountID, Profile string
	KeyVersion                                                                   int64
}

type StripeAPICredentials struct{ SecretKey string }
type StripeWebhookSecrets struct{ CurrentSecret, NextSecret string }

func (StripeAPICredentials) String() string     { return "[redacted stripe API credentials]" }
func (c StripeAPICredentials) GoString() string { return c.String() }
func (StripeAPICredentials) MarshalJSON() ([]byte, error) {
	return []byte(`"[redacted stripe API credentials]"`), nil
}

func (StripeWebhookSecrets) String() string     { return "[redacted stripe webhook secrets]" }
func (s StripeWebhookSecrets) GoString() string { return s.String() }
func (StripeWebhookSecrets) MarshalJSON() ([]byte, error) {
	return []byte(`"[redacted stripe webhook secrets]"`), nil
}

type stripeAPIAAD struct {
	Purpose           string `json:"purpose"`
	Provider          string `json:"provider"`
	TenantID          string `json:"tenant_id"`
	StoreID           string `json:"store_id"`
	ConnectionID      string `json:"connection_id"`
	Environment       string `json:"environment"`
	AccountID         string `json:"account_id"`
	CredentialVersion int64  `json:"credential_version"`
}

type stripeWebhookAAD struct {
	Purpose      string `json:"purpose"`
	Provider     string `json:"provider"`
	TenantID     string `json:"tenant_id"`
	StoreID      string `json:"store_id"`
	ConnectionID string `json:"connection_id"`
	EndpointID   string `json:"endpoint_id"`
	Environment  string `json:"environment"`
	AccountID    string `json:"account_id"`
	Profile      string `json:"profile"`
	KeyVersion   int64  `json:"key_version"`
}

type stripeAPIWire struct {
	SecretKey string `json:"secret_key"`
}

type stripeWebhookWire struct {
	CurrentSecret string `json:"current_secret"`
	NextSecret    string `json:"next_secret,omitempty"`
}

func validStripeAPIScope(s StripeAPIScope) bool {
	return stripeScopeUUID.MatchString(s.TenantID) && stripeScopeUUID.MatchString(s.StoreID) &&
		stripeScopeUUID.MatchString(s.ConnectionID) && validStripeEnvironment(s.Environment) &&
		s.CredentialVersion > 0 && len(s.AccountID) >= 6 && len(s.AccountID) <= 64
}

func validStripeWebhookScope(s StripeWebhookScope) bool {
	return stripeScopeUUID.MatchString(s.TenantID) && stripeScopeUUID.MatchString(s.StoreID) &&
		stripeScopeUUID.MatchString(s.ConnectionID) && stripeScopeUUID.MatchString(s.EndpointID) &&
		validStripeEnvironment(s.Environment) && s.KeyVersion > 0 &&
		validStripeProfile(s.Environment, s.Profile) &&
		len(s.AccountID) >= 6 && len(s.AccountID) <= 64
}

// validStripeEnvironment: stripe-live-enable-v1 §5.2 admits LIVE next to SANDBOX in the
// keyring scope. Sealing a LIVE credential is custody, not permission to call Stripe: the
// flag+ref pair is enforced where a client is built (payments.NewLiveStripeRuntime,
// stripeadmin.OpenLive), never here.
func validStripeEnvironment(env string) bool { return env == "SANDBOX" || env == "LIVE" }

// validStripeProfile: a LIVE endpoint profile exists only for a LIVE account and vice versa
// (`(Environment=="LIVE") == (Profile=="LIVE")`; PROVIDER_MOCK/SANDBOX only for SANDBOX).
func validStripeProfile(env, profile string) bool {
	if env == "LIVE" {
		return profile == "LIVE"
	}
	return profile == "PROVIDER_MOCK" || profile == "SANDBOX"
}

// scopeCheckApproval satisfies stripe.admit's flag+ref shape so the KEY GRAMMAR of a LIVE
// credential (rk_live_ only, LD3) is checked at seal/open time. It is a syntax stand-in, not
// an approval: constructing a client does no I/O here and the value is never used to dial.
var scopeCheckApproval = stripe.LiveApproval{Enabled: true, Reference: "keyring-scope-check"}

func validStripeAPI(s StripeAPIScope, c StripeAPICredentials) bool {
	if !validStripeAPIScope(s) {
		return false
	}
	cfg := stripe.Config{SecretKey: c.SecretKey, AccountID: s.AccountID, Environment: s.Environment}
	if s.Environment == "LIVE" {
		cfg.Live = scopeCheckApproval
	}
	// Stage A owns the account and API-key grammar; constructing a client does no I/O.
	_, err := stripe.New(cfg)
	return err == nil
}

func validStripeWebhook(s StripeWebhookScope, secrets StripeWebhookSecrets) bool {
	if !validStripeWebhookScope(s) {
		return false
	}
	values := []string{secrets.CurrentSecret}
	if secrets.NextSecret != "" {
		values = append(values, secrets.NextSecret)
	}
	// Stage A owns signing-secret grammar and distinct-secret admission; no I/O.
	_, err := stripe.NewWebhookVerifier(stripe.WebhookConfig{
		Secrets: values, AccountID: s.AccountID, Environment: s.Environment,
	})
	return err == nil
}

func apiAssociated(s StripeAPIScope) []byte {
	b, _ := json.Marshal(stripeAPIAAD{"stripe-api-v1", "stripe", s.TenantID, s.StoreID,
		s.ConnectionID, s.Environment, s.AccountID, s.CredentialVersion})
	return b
}

func webhookAssociated(s StripeWebhookScope) []byte {
	b, _ := json.Marshal(stripeWebhookAAD{"stripe-webhook-v1", "stripe", s.TenantID, s.StoreID,
		s.ConnectionID, s.EndpointID, s.Environment, s.AccountID, s.Profile, s.KeyVersion})
	return b
}

func (k *Keyring) SealStripeAPI(scope StripeAPIScope, credentials StripeAPICredentials) (string, []byte, []byte, error) {
	if !validStripeAPI(scope, credentials) {
		return "", nil, nil, errStripeMaterial
	}
	plain, _ := json.Marshal(stripeAPIWire{SecretKey: credentials.SecretKey})
	defer clear(plain)
	return k.sealStripe(plain, apiAssociated(scope))
}

func (k *Keyring) OpenStripeAPI(scope StripeAPIScope, keyID string, nonce, ciphertext []byte) (StripeAPICredentials, error) {
	if !validStripeAPIScope(scope) {
		return StripeAPICredentials{}, errStripeMaterial
	}
	plain, err := k.openStripe(keyID, nonce, ciphertext, apiAssociated(scope))
	if err != nil {
		return StripeAPICredentials{}, errStripeMaterial
	}
	defer clear(plain)
	var wire stripeAPIWire
	if !strictStripeJSON(plain, &wire) {
		return StripeAPICredentials{}, errStripeMaterial
	}
	result := StripeAPICredentials{SecretKey: wire.SecretKey}
	if !validStripeAPI(scope, result) {
		return StripeAPICredentials{}, errStripeMaterial
	}
	return result, nil
}

func (k *Keyring) SealStripeWebhook(scope StripeWebhookScope, secrets StripeWebhookSecrets) (string, []byte, []byte, error) {
	if !validStripeWebhook(scope, secrets) {
		return "", nil, nil, errStripeMaterial
	}
	plain, _ := json.Marshal(stripeWebhookWire{secrets.CurrentSecret, secrets.NextSecret})
	defer clear(plain)
	return k.sealStripe(plain, webhookAssociated(scope))
}

func (k *Keyring) OpenStripeWebhook(scope StripeWebhookScope, keyID string, nonce, ciphertext []byte) (StripeWebhookSecrets, error) {
	if !validStripeWebhookScope(scope) {
		return StripeWebhookSecrets{}, errStripeMaterial
	}
	plain, err := k.openStripe(keyID, nonce, ciphertext, webhookAssociated(scope))
	if err != nil {
		return StripeWebhookSecrets{}, errStripeMaterial
	}
	defer clear(plain)
	var wire stripeWebhookWire
	if !strictStripeJSON(plain, &wire) {
		return StripeWebhookSecrets{}, errStripeMaterial
	}
	result := StripeWebhookSecrets{wire.CurrentSecret, wire.NextSecret}
	if !validStripeWebhook(scope, result) {
		return StripeWebhookSecrets{}, errStripeMaterial
	}
	return result, nil
}

func (k *Keyring) sealStripe(plain, associated []byte) (string, []byte, []byte, error) {
	if k == nil || !keyIDPattern.MatchString(k.activeID) || len(plain) == 0 ||
		len(plain)+16 > maxStripeCiphertext {
		return "", nil, nil, errStripeMaterial
	}
	key, ok := k.keys[k.activeID]
	if !ok || len(key) != 32 {
		return "", nil, nil, errStripeMaterial
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return "", nil, nil, errStripeMaterial
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", nil, nil, errStripeMaterial
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", nil, nil, errStripeMaterial
	}
	return k.activeID, nonce, gcm.Seal(nil, nonce, plain, associated), nil
}

func (k *Keyring) openStripe(keyID string, nonce, ciphertext, associated []byte) ([]byte, error) {
	if k == nil || !keyIDPattern.MatchString(keyID) || len(nonce) != 12 ||
		len(ciphertext) < 17 || len(ciphertext) > maxStripeCiphertext {
		return nil, errStripeMaterial
	}
	key, ok := k.keys[keyID]
	if !ok || len(key) != 32 {
		return nil, errStripeMaterial
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, errStripeMaterial
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, errStripeMaterial
	}
	plain, err := gcm.Open(nil, nonce, ciphertext, associated)
	if err != nil {
		return nil, errStripeMaterial
	}
	return plain, nil
}

func strictStripeJSON(plain []byte, target any) bool {
	if err := json.Unmarshal(plain, target); err != nil {
		return false
	}
	canonical, err := json.Marshal(target)
	return err == nil && bytes.Equal(plain, canonical)
}
