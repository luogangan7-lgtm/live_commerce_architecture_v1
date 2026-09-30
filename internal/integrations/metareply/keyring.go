package metareply

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strconv"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"livecommerce/internal/command"
	"livecommerce/internal/integrations/core"
	"livecommerce/internal/integrations/meta"
)

// Page-token custody (meta-claims-intake-v1 §7). Payload {page_access_token}; AES-256-GCM with
// AAD JSON(["livecommerce/meta-page-token/v1", tenant, store, binding, provider, asset_id, version,
// key_id]). The keyring is separate from the Meta payload keyring and from payment keys.

const (
	aadDomain      = "livecommerce/meta-page-token/v1"
	envActiveKeyID = "COMMERCE_META_PAGE_TOKEN_ACTIVE_KEY_ID"
	envKeysJSON    = "COMMERCE_META_PAGE_TOKEN_KEYS_JSON"
	maxTokenBytes  = 4096
)

var (
	// ErrConfig is every keyring/config failure; it never carries key material.
	ErrConfig = errors.New("metareply: invalid configuration")
	// ErrSecret is every seal/open failure (wrong key, tampered AAD, bad token); no detail on purpose.
	ErrSecret = errors.New("metareply: page token unavailable")

	keyIDPattern  = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)
	assetPattern  = regexp.MustCompile(`^[0-9]{1,40}$`)
	scopePattern  = regexp.MustCompile(`^[a-z_]{1,64}$`)
	providerNames = map[string]bool{"facebook": true, "instagram": true}
)

// PageTokenKeyring holds the AES keys; every formatter is redacted.
type PageTokenKeyring struct {
	activeID string
	keys     map[string][]byte
}

func (PageTokenKeyring) String() string               { return "[redacted]" }
func (PageTokenKeyring) GoString() string             { return "[redacted]" }
func (PageTokenKeyring) Format(f fmt.State, _ rune)   { _, _ = f.Write([]byte("[redacted]")) }
func (PageTokenKeyring) MarshalJSON() ([]byte, error) { return []byte(`"[redacted]"`), nil }

// NewPageTokenKeyring copies 1..16 keys of exactly 32 bytes (no all-zero key, no duplicates); the
// active id must be one of them.
func NewPageTokenKeyring(activeID string, keys map[string][]byte) (*PageTokenKeyring, error) {
	if len(keys) < 1 || len(keys) > 16 || !keyIDPattern.MatchString(activeID) {
		return nil, ErrConfig
	}
	out := &PageTokenKeyring{activeID: activeID, keys: make(map[string][]byte, len(keys))}
	seen := map[string]bool{}
	for id, k := range keys {
		if !keyIDPattern.MatchString(id) || len(k) != 32 || bytes.Equal(k, make([]byte, 32)) || seen[string(k)] {
			return nil, ErrConfig
		}
		seen[string(k)] = true
		out.keys[id] = append([]byte(nil), k...)
	}
	if _, ok := out.keys[activeID]; !ok {
		return nil, ErrConfig
	}
	return out, nil
}

// LoadPageTokenKeyring reads COMMERCE_META_PAGE_TOKEN_ACTIVE_KEY_ID and
// COMMERCE_META_PAGE_TOKEN_KEYS_JSON, the payload-keyring format
// `{"keys":[{"id":"…","key_base64":"<44-char std base64 of 32 bytes>"}]}`. Unknown members are
// rejected, and so are duplicate object member names (meta.ParseStrict, ruling n) and duplicate key ids.
func LoadPageTokenKeyring(getenv func(string) string) (*PageTokenKeyring, error) {
	if getenv == nil {
		return nil, ErrConfig
	}
	raw := getenv(envKeysJSON)
	if len(raw) < 1 || len(raw) > 8192 {
		return nil, ErrConfig
	}
	if _, err := meta.ParseStrict([]byte(raw)); err != nil { // duplicate members would silently collapse in encoding/json
		return nil, ErrConfig
	}
	var doc struct {
		Keys []struct {
			ID  string `json:"id"`
			Key string `json:"key_base64"`
		} `json:"keys"`
	}
	dec := json.NewDecoder(bytes.NewReader([]byte(raw)))
	dec.DisallowUnknownFields()
	var extra json.RawMessage
	if err := dec.Decode(&doc); err != nil || dec.Decode(&extra) == nil {
		return nil, ErrConfig
	}
	keys := make(map[string][]byte, len(doc.Keys))
	for _, item := range doc.Keys {
		decoded, err := base64.StdEncoding.DecodeString(item.Key)
		if err != nil || len(item.Key) != 44 || len(decoded) != 32 || base64.StdEncoding.EncodeToString(decoded) != item.Key {
			return nil, ErrConfig
		}
		if _, dup := keys[item.ID]; dup {
			return nil, ErrConfig
		}
		keys[item.ID] = decoded
	}
	return NewPageTokenKeyring(getenv(envActiveKeyID), keys)
}

// PageTokenScope is everything the AAD binds a ciphertext to; Version is the credential version
// being written (expected head + 1) or read.
type PageTokenScope struct {
	TenantID, StoreID, BindingID, Provider, AssetID string
	Version                                         int64
}

func (s PageTokenScope) aad(keyID string) ([]byte, error) {
	if !command.ValidID(s.TenantID) || !command.ValidID(s.StoreID) || !command.ValidID(s.BindingID) ||
		!providerNames[s.Provider] || !assetPattern.MatchString(s.AssetID) || s.Version < 1 || !keyIDPattern.MatchString(keyID) {
		return nil, ErrSecret
	}
	return json.Marshal([]string{aadDomain, s.TenantID, s.StoreID, s.BindingID, s.Provider, s.AssetID,
		strconv.FormatInt(s.Version, 10), keyID})
}

func newGCM(key []byte) (cipher.AEAD, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, ErrSecret
	}
	return cipher.NewGCM(block)
}

// validPageToken: 1..4096 visible ASCII bytes (Meta tokens are alphanumeric); no space or control.
func validPageToken(t string) bool {
	if len(t) < 1 || len(t) > maxTokenBytes {
		return false
	}
	for i := 0; i < len(t); i++ {
		if t[i] < 0x21 || t[i] > 0x7e {
			return false
		}
	}
	return true
}

// Seal encrypts pageToken under the active key for scope; the ciphertext is 17..8192 bytes.
func (k *PageTokenKeyring) Seal(s PageTokenScope, pageToken string) (keyID string, nonce, ciphertext []byte, err error) {
	if k == nil || !validPageToken(pageToken) {
		return "", nil, nil, ErrSecret
	}
	aad, err := s.aad(k.activeID)
	if err != nil {
		return "", nil, nil, err
	}
	gcm, err := newGCM(k.keys[k.activeID])
	if err != nil {
		return "", nil, nil, err
	}
	nonce = make([]byte, gcm.NonceSize())
	if _, err = rand.Read(nonce); err != nil {
		return "", nil, nil, ErrSecret
	}
	plain, _ := json.Marshal(struct {
		Token string `json:"page_access_token"`
	}{pageToken})
	ciphertext = gcm.Seal(nil, nonce, plain, aad)
	clear(plain)
	return k.activeID, nonce, ciphertext, nil
}

// Open decrypts a stored version; any mismatch (key id, scope, tamper, shape) is ErrSecret.
func (k *PageTokenKeyring) Open(s PageTokenScope, keyID string, nonce, ciphertext []byte) (core.Secret, error) {
	if k == nil || len(nonce) != 12 || len(ciphertext) < 17 || len(ciphertext) > 8192 {
		return core.Secret{}, ErrSecret
	}
	key, ok := k.keys[keyID]
	if !ok {
		return core.Secret{}, ErrSecret
	}
	aad, err := s.aad(keyID)
	if err != nil {
		return core.Secret{}, err
	}
	gcm, err := newGCM(key)
	if err != nil {
		return core.Secret{}, err
	}
	plain, err := gcm.Open(nil, nonce, ciphertext, aad)
	if err != nil {
		return core.Secret{}, ErrSecret
	}
	defer clear(plain)
	var doc struct {
		Token string `json:"page_access_token"`
	}
	dec := json.NewDecoder(bytes.NewReader(plain))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&doc); err != nil || !validPageToken(doc.Token) {
		return core.Secret{}, ErrSecret
	}
	return core.NewSecret([]byte(doc.Token)), nil
}

// Registration is one operator registration of a Page token version (cmd/meta-admin page-token).
// ExpectedVersion 0 creates the head; Scopes is the operator's attestation from the token debug
// output (§7), each `[a-z_]+`, 1..16.
type Registration struct {
	TenantID, StoreID, PrincipalID, BindingID, Provider, AssetID string
	ExpectedVersion                                              int64
	Scopes                                                       []string
}

// RegisterPageToken seals the token and calls integration.register_meta_page_token on the
// registrar pool (definer commerce_integration_writer; EXECUTE for commerce_meta_registrar only,
// which is the authority check). The plaintext never reaches SQL. It returns the new version.
func RegisterPageToken(ctx context.Context, pool *pgxpool.Pool, keys *PageTokenKeyring, r Registration, pageToken string) (int64, error) {
	if ctx == nil || pool == nil || keys == nil || !command.ValidID(r.PrincipalID) || r.ExpectedVersion < 0 ||
		r.ExpectedVersion >= 1<<62 || len(r.Scopes) < 1 || len(r.Scopes) > 16 {
		return 0, command.ErrInvalid
	}
	for _, sc := range r.Scopes {
		if !scopePattern.MatchString(sc) {
			return 0, command.ErrInvalid
		}
	}
	scope := PageTokenScope{TenantID: r.TenantID, StoreID: r.StoreID, BindingID: r.BindingID, Provider: r.Provider,
		AssetID: r.AssetID, Version: r.ExpectedVersion + 1}
	keyID, nonce, ciphertext, err := keys.Seal(scope, pageToken)
	if err != nil {
		return 0, command.ErrInvalid
	}
	var version int64
	err = pool.QueryRow(ctx, `SELECT integration.register_meta_page_token($1::uuid,$2::uuid,$3::uuid,$4::uuid,$5,$6,$7,$8,$9,$10,$11::text[])`,
		r.TenantID, r.StoreID, r.PrincipalID, r.BindingID, r.Provider, r.AssetID, r.ExpectedVersion, keyID, nonce, ciphertext, r.Scopes).Scan(&version)
	if err != nil {
		var pg *pgconn.PgError
		if errors.As(err, &pg) {
			switch pg.Code {
			case "22023":
				return 0, command.ErrInvalid
			case "PT409", "40001", "23505":
				return 0, command.ErrConflict
			}
		}
		return 0, fmt.Errorf("register meta page token: %w", err)
	}
	return version, nil
}
