package metaads

// seal.go: the SEAL half of BISU token custody (meta-ads-v1 A-4, §4.1; ads-graph G1/G2). cmd/api holds
// only HPKE PUBLIC keys and can therefore encrypt a token it just received but never read one back.
// The OPEN half lives in subpackage tokenopen, imported only by cmd/ads-worker (MA11 static check).
//
// Suite (G2): DHKEM(X25519, HKDF-SHA256) / HKDF-SHA256 / AES-256-GCM via stdlib crypto/hpke, no
// dependency. RFC 9180: https://www.rfc-editor.org/rfc/rfc9180.html (retrieved 2026-09-30). The
// 32-byte encapsulated key goes to nonce/pending_enc, the AEAD ciphertext to ciphertext. `info`
// binds a ciphertext to (tenant, store, key id): a row copied to another store fails to open.

import (
	"bytes"
	"crypto/ecdh"
	"crypto/hpke"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"

	"livecommerce/internal/ads"
	"livecommerce/internal/command"
	"livecommerce/internal/integrations/meta"
)

const (
	// InfoDomain is the first element of the HPKE info array (contract §4.1).
	InfoDomain = "livecommerce/meta-ads-token/v1"

	envPublicKeys  = "COMMERCE_META_ADS_TOKEN_HPKE_PUBLIC_KEYS_JSON"
	envActiveKeyID = "COMMERCE_META_ADS_TOKEN_HPKE_ACTIVE_KEY_ID"

	// EncSize is the X25519 encapsulated-key length stored in nonce/pending_enc (0074 CHECK: 32).
	EncSize = 32
	// MaxTokenBytes bounds a plaintext token (visible ASCII); the ciphertext then fits 17..8192.
	MaxTokenBytes = 4096
)

var (
	// ErrSeal is every seal failure; it never carries key or token material.
	ErrSeal = errors.New("metaads: token seal failed")

	keyIDPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)
)

// HPKEInfo is the HPKE `info` of one sealed token: the JSON array
// ["livecommerce/meta-ads-token/v1", tenant, store, key_id] (contract §4.1). tokenopen calls this same
// function so seal and open cannot drift.
func HPKEInfo(tenantID, storeID, keyID string) ([]byte, error) {
	if !command.ValidID(tenantID) || !command.ValidID(storeID) || !keyIDPattern.MatchString(keyID) {
		return nil, ErrSeal
	}
	return json.Marshal([]string{InfoDomain, tenantID, storeID, keyID})
}

// ValidToken: 1..4096 visible ASCII bytes (Meta tokens are alphanumeric); no space or control.
func ValidToken(t []byte) bool {
	if len(t) < 1 || len(t) > MaxTokenBytes {
		return false
	}
	for _, b := range t {
		if b < 0x21 || b > 0x7e {
			return false
		}
	}
	return true
}

// SealKeys holds the HPKE public keys and the active key id. Public keys are not secret, but the
// type is redacted in every formatter anyway (no key material in logs).
type SealKeys struct {
	activeID string
	keys     map[string]hpke.PublicKey
}

func (SealKeys) String() string               { return "[redacted]" }
func (SealKeys) GoString() string             { return "[redacted]" }
func (SealKeys) Format(f fmt.State, _ rune)   { _, _ = f.Write([]byte("[redacted]")) }
func (SealKeys) MarshalJSON() ([]byte, error) { return []byte(`"[redacted]"`), nil }

// LoadSealKeys reads COMMERCE_META_ADS_TOKEN_HPKE_PUBLIC_KEYS_JSON
// `{"keys":[{"id":"…","public_key_base64":"<44-char std base64 of 32 bytes>"}]}` and
// COMMERCE_META_ADS_TOKEN_HPKE_ACTIVE_KEY_ID. Unknown members, duplicate object members (meta.ParseStrict)
// and duplicate ids are rejected; 1..16 keys; the active id must be one of them.
func LoadSealKeys(getenv func(string) string) (*SealKeys, error) {
	if getenv == nil {
		return nil, ErrConfig
	}
	raw := getenv(envPublicKeys)
	if len(raw) < 1 || len(raw) > 8192 {
		return nil, ErrConfig
	}
	if _, err := meta.ParseStrict([]byte(raw)); err != nil {
		return nil, ErrConfig
	}
	var doc struct {
		Keys []struct {
			ID  string `json:"id"`
			Key string `json:"public_key_base64"`
		} `json:"keys"`
	}
	dec := json.NewDecoder(bytes.NewReader([]byte(raw)))
	dec.DisallowUnknownFields()
	var extra json.RawMessage
	if err := dec.Decode(&doc); err != nil || dec.Decode(&extra) == nil || len(doc.Keys) < 1 || len(doc.Keys) > 16 {
		return nil, ErrConfig
	}
	activeID := getenv(envActiveKeyID)
	if !keyIDPattern.MatchString(activeID) {
		return nil, ErrConfig
	}
	out := &SealKeys{activeID: activeID, keys: make(map[string]hpke.PublicKey, len(doc.Keys))}
	for _, item := range doc.Keys {
		decoded, err := base64.StdEncoding.DecodeString(item.Key)
		if err != nil || len(item.Key) != 44 || len(decoded) != 32 || !keyIDPattern.MatchString(item.ID) {
			return nil, ErrConfig
		}
		if _, dup := out.keys[item.ID]; dup {
			return nil, ErrConfig
		}
		pk, err := hpke.DHKEM(ecdh.X25519()).NewPublicKey(decoded)
		if err != nil {
			return nil, ErrConfig
		}
		out.keys[item.ID] = pk
	}
	if _, ok := out.keys[activeID]; !ok {
		return nil, ErrConfig
	}
	return out, nil
}

// Seal encrypts token for (tenant, store) under the active key. The caller zeroes token afterwards;
// Seal keeps no reference to it.
func (k *SealKeys) Seal(info ads.SealInfo, token []byte) (ads.SealedToken, error) {
	if k == nil || !ValidToken(token) {
		return ads.SealedToken{}, ErrSeal
	}
	infoBytes, err := HPKEInfo(info.TenantID, info.StoreID, k.activeID)
	if err != nil {
		return ads.SealedToken{}, err
	}
	out, err := hpke.Seal(k.keys[k.activeID], hpke.HKDFSHA256(), hpke.AES256GCM(), infoBytes, token)
	if err != nil || len(out) <= EncSize {
		return ads.SealedToken{}, ErrSeal
	}
	return ads.SealedToken{KeyID: k.activeID, Enc: append([]byte(nil), out[:EncSize]...),
		Ciphertext: append([]byte(nil), out[EncSize:]...)}, nil
}
