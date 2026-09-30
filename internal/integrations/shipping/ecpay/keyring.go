// keyring.go: custody of the merchant's ECPay logistics credential (R-1, E5). The payload
// {hash_key,hash_iv,sender_name,sender_cell_phone} is sealed with AES-256-GCM under a keyring loaded
// from ECPAY_LOGISTICS_KEYRING; the AAD binds a ciphertext to its tenant, store, connection,
// environment, MerchantID and credential version, so a row copied to another scope, or an old version
// replayed as a new one, fails to open. The keyring is separate from the Stripe, Meta and PAYUNi
// keyrings. Shape copied from internal/integrations/metareply/keyring.go; errors never echo key
// material or env values.

package ecpay

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"regexp"
	"strconv"
	"strings"
)

const (
	envKeyring     = "ECPAY_LOGISTICS_KEYRING"
	aadPurpose     = "ecpay-logistics-v1"
	aadProvider    = "ecpay_logistics"
	maxKeyringJSON = 8 << 10
)

var (
	keyIDRE = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)
	uuidRE  = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
)

// Keyring holds the AES keys; every formatter is redacted.
type Keyring struct {
	active string
	keys   map[string][]byte
}

func (Keyring) String() string   { return redacted }
func (Keyring) GoString() string { return redacted }
func (Keyring) Format(f fmt.State, _ rune) {
	_, _ = f.Write([]byte(redacted))
}
func (Keyring) MarshalJSON() ([]byte, error) { return []byte(`"` + redacted + `"`), nil }

// noDuplicateMembers walks the JSON tokens and refuses a repeated object member name at any depth:
// encoding/json would silently keep the last one, so two "keys" arrays could hide a key.
func noDuplicateMembers(raw []byte) bool {
	dec := json.NewDecoder(bytes.NewReader(raw))
	type frame struct {
		obj  bool
		seen map[string]bool
		key  bool // next string token in an object is a member name
	}
	var stack []*frame
	for {
		t, err := dec.Token()
		if err == io.EOF {
			return len(stack) == 0
		}
		if err != nil {
			return false
		}
		if d, ok := t.(json.Delim); ok {
			switch d {
			case '{':
				stack = append(stack, &frame{obj: true, seen: map[string]bool{}, key: true})
			case '[':
				stack = append(stack, &frame{})
			default:
				stack = stack[:len(stack)-1]
				if n := len(stack); n > 0 && stack[n-1].obj {
					stack[n-1].key = true
				}
			}
			continue
		}
		if n := len(stack); n > 0 && stack[n-1].obj {
			top := stack[n-1]
			if top.key {
				name, _ := t.(string)
				if top.seen[name] {
					return false
				}
				top.seen[name] = true
				top.key = false
			} else {
				top.key = true
			}
		}
	}
}

// LoadKeyring reads ECPAY_LOGISTICS_KEYRING: JSON {"active":"<id>","keys":[{"id":"..","key_base64":
// ".."}]}, at most 8 KiB, 1..16 keys of exactly 32 bytes (standard base64, 44 chars, no all-zero and no
// duplicate key), unknown or duplicate members refused, the active id present. Every failure is
// ErrConfig.
func LoadKeyring(getenv func(string) string) (*Keyring, error) {
	if getenv == nil {
		return nil, ErrConfig
	}
	raw := getenv(envKeyring)
	if len(raw) < 1 || len(raw) > maxKeyringJSON || !noDuplicateMembers([]byte(raw)) {
		return nil, ErrConfig
	}
	var doc struct {
		Active string `json:"active"`
		Keys   []struct {
			ID  string `json:"id"`
			Key string `json:"key_base64"`
		} `json:"keys"`
	}
	dec := json.NewDecoder(strings.NewReader(raw))
	dec.DisallowUnknownFields()
	var extra json.RawMessage
	if err := dec.Decode(&doc); err != nil || dec.Decode(&extra) != io.EOF {
		return nil, ErrConfig
	}
	if len(doc.Keys) < 1 || len(doc.Keys) > 16 || !keyIDRE.MatchString(doc.Active) {
		return nil, ErrConfig
	}
	k := &Keyring{active: doc.Active, keys: make(map[string][]byte, len(doc.Keys))}
	seen := map[string]bool{}
	for _, item := range doc.Keys {
		b, err := base64.StdEncoding.DecodeString(item.Key)
		if err != nil || len(item.Key) != 44 || len(b) != 32 || bytes.Equal(b, make([]byte, 32)) ||
			!keyIDRE.MatchString(item.ID) || seen[string(b)] {
			return nil, ErrConfig
		}
		if _, dup := k.keys[item.ID]; dup {
			return nil, ErrConfig
		}
		seen[string(b)] = true
		k.keys[item.ID] = b
	}
	if _, ok := k.keys[k.active]; !ok {
		return nil, ErrConfig
	}
	return k, nil
}

// aad is the canonical additional data: a struct marshals with fixed member order, so the bytes are
// deterministic. The scope must be complete and well formed or sealing/opening is refused.
func (s Scope) aad() ([]byte, error) {
	if !uuidRE.MatchString(s.TenantID) || !uuidRE.MatchString(s.StoreID) || !uuidRE.MatchString(s.ConnectionID) ||
		!merchantIDRE.MatchString(s.MerchantID) || (s.Environment != EnvSandbox && s.Environment != EnvLive) || s.Version < 1 {
		return nil, ErrSecret
	}
	return json.Marshal(struct {
		Purpose      string `json:"purpose"`
		TenantID     string `json:"tenant_id"`
		StoreID      string `json:"store_id"`
		ConnectionID string `json:"connection_id"`
		Provider     string `json:"provider"`
		Environment  string `json:"environment"`
		MerchantID   string `json:"merchant_id"`
		Version      string `json:"version"`
	}{aadPurpose, s.TenantID, s.StoreID, s.ConnectionID, aadProvider, string(s.Environment), s.MerchantID,
		strconv.FormatInt(s.Version, 10)})
}

// visible reports 1..max visible ASCII bytes (HashKey/HashIV are 16-char alphanumerics).
func visible(s string, max int) bool {
	if len(s) < 1 || len(s) > max {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < 0x21 || s[i] > 0x7e {
			return false
		}
	}
	return true
}

// payloadOK bounds the payload; sender fields may be empty (the create validates them per subtype,
// F5) but a non-empty one must already satisfy its rule.
func payloadOK(p Payload) bool {
	if !visible(p.HashKey, 64) || !visible(p.HashIV, 64) || len(p.SenderName) > 64 || len(p.SenderCellPhone) > 32 {
		return false
	}
	if p.SenderName != "" && !nameOK(p.SenderName) {
		return false
	}
	if p.SenderCellPhone != "" {
		if _, ok := normalizePhone(p.SenderCellPhone); !ok {
			return false
		}
	}
	return true
}

type payloadJSON struct {
	HashKey         string `json:"hash_key"`
	HashIV          string `json:"hash_iv"`
	SenderName      string `json:"sender_name"`
	SenderCellPhone string `json:"sender_cell_phone"`
}

func gcmFor(key []byte) (cipher.AEAD, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, ErrSecret
	}
	return cipher.NewGCM(block)
}

// Seal encrypts p under the active key for scope s (Version = the credential version being written).
func (k *Keyring) Seal(s Scope, p Payload) (keyID string, nonce, ciphertext []byte, err error) {
	if k == nil || !payloadOK(p) {
		return "", nil, nil, ErrSecret
	}
	aad, err := s.aad()
	if err != nil {
		return "", nil, nil, err
	}
	gcm, err := gcmFor(k.keys[k.active])
	if err != nil {
		return "", nil, nil, err
	}
	nonce = make([]byte, gcm.NonceSize())
	if _, err = rand.Read(nonce); err != nil {
		return "", nil, nil, ErrSecret
	}
	plain, _ := json.Marshal(payloadJSON{p.HashKey, p.HashIV, p.SenderName, p.SenderCellPhone})
	ciphertext = gcm.Seal(nil, nonce, plain, aad)
	clear(plain)
	return k.active, nonce, ciphertext, nil
}

// Open decrypts a stored version. Any mismatch (unknown key id, wrong scope or version, tampering,
// malformed payload) is ErrSecret with no detail.
func (k *Keyring) Open(s Scope, keyID string, nonce, ciphertext []byte) (Payload, error) {
	if k == nil || len(nonce) != 12 || len(ciphertext) < 17 || len(ciphertext) > 8192 {
		return Payload{}, ErrSecret
	}
	key, ok := k.keys[keyID]
	if !ok {
		return Payload{}, ErrSecret
	}
	aad, err := s.aad()
	if err != nil {
		return Payload{}, err
	}
	gcm, err := gcmFor(key)
	if err != nil {
		return Payload{}, err
	}
	plain, err := gcm.Open(nil, nonce, ciphertext, aad)
	if err != nil {
		return Payload{}, ErrSecret
	}
	defer clear(plain)
	var doc payloadJSON
	dec := json.NewDecoder(bytes.NewReader(plain))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&doc); err != nil {
		return Payload{}, ErrSecret
	}
	p := Payload{HashKey: doc.HashKey, HashIV: doc.HashIV, SenderName: doc.SenderName, SenderCellPhone: doc.SenderCellPhone}
	if !payloadOK(p) {
		return Payload{}, ErrSecret
	}
	return p, nil
}
