package tokenopen

// keyring_test.go: seal (metaads.SealKeys) -> open (Keyring) round trip and every refusal. Keys are
// generated per test; nothing here is a real key or token.

import (
	"crypto/ecdh"
	"crypto/hpke"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"livecommerce/internal/ads"
	metaads "livecommerce/internal/integrations/meta_ads"
)

const (
	tenantID = "33333333-3333-4333-8333-333333333333"
	storeID  = "44444444-4444-4444-8444-444444444444"
	otherID  = "55555555-5555-4555-8555-555555555555"
)

type pair struct {
	priv hpke.PrivateKey
	pub  string // std base64
	prv  string // std base64
}

func newPair(t *testing.T) pair {
	t.Helper()
	priv, err := hpke.DHKEM(ecdh.X25519()).GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	raw, err := priv.Bytes()
	if err != nil {
		t.Fatal(err)
	}
	return pair{priv: priv, pub: base64.StdEncoding.EncodeToString(priv.PublicKey().Bytes()), prv: base64.StdEncoding.EncodeToString(raw)}
}

func env(t *testing.T, ids map[string]pair, active string) (seal, open func(string) string) {
	t.Helper()
	var pubs, prvs []map[string]string
	for id, p := range ids {
		pubs = append(pubs, map[string]string{"id": id, "public_key_base64": p.pub})
		prvs = append(prvs, map[string]string{"id": id, "private_key_base64": p.prv})
	}
	pubDoc, _ := json.Marshal(map[string]any{"keys": pubs})
	prvDoc, _ := json.Marshal(map[string]any{"keys": prvs})
	file := filepath.Join(t.TempDir(), "hpke-private.json")
	if err := os.WriteFile(file, prvDoc, 0o600); err != nil {
		t.Fatal(err)
	}
	sealEnv := map[string]string{"COMMERCE_META_ADS_TOKEN_HPKE_PUBLIC_KEYS_JSON": string(pubDoc), "COMMERCE_META_ADS_TOKEN_HPKE_ACTIVE_KEY_ID": active}
	openEnv := map[string]string{envPrivateKeys + "_FILE": file}
	return func(k string) string { return sealEnv[k] }, func(k string) string { return openEnv[k] }
}

func TestSealOpenRoundTrip(t *testing.T) {
	seal, open := env(t, map[string]pair{"k1": newPair(t), "k0": newPair(t)}, "k1")
	sk, err := metaads.LoadSealKeys(seal)
	if err != nil {
		t.Fatal(err)
	}
	ring, err := LoadKeyring(open)
	if err != nil {
		t.Fatal(err)
	}
	token := []byte("bisuTOKEN0123456789")
	sealed, err := sk.Seal(ads.SealInfo{TenantID: tenantID, StoreID: storeID}, token)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(sealed.Ciphertext), string(token)) || strings.Contains(string(sealed.Enc), string(token)) {
		t.Fatal("ciphertext contains the plaintext")
	}
	plain, err := ring.Open(tenantID, storeID, sealed.KeyID, sealed.Enc, sealed.Ciphertext)
	if err != nil || string(plain) != string(token) {
		t.Fatalf("open = %q,%v", plain, err)
	}
	clear(plain)

	tampered := append([]byte(nil), sealed.Ciphertext...)
	tampered[0] ^= 1
	badEnc := append([]byte(nil), sealed.Enc...)
	badEnc[0] ^= 1
	for name, tc := range map[string]struct {
		tenant, store, key string
		enc, ct            []byte
	}{
		"other tenant": {otherID, storeID, "k1", sealed.Enc, sealed.Ciphertext},
		"other store":  {tenantID, otherID, "k1", sealed.Enc, sealed.Ciphertext},
		"other key id": {tenantID, storeID, "k0", sealed.Enc, sealed.Ciphertext}, // info + key differ
		"unknown key":  {tenantID, storeID, "k9", sealed.Enc, sealed.Ciphertext},
		"tampered ct":  {tenantID, storeID, "k1", sealed.Enc, tampered},
		"tampered enc": {tenantID, storeID, "k1", badEnc, sealed.Ciphertext},
		"short enc":    {tenantID, storeID, "k1", sealed.Enc[:31], sealed.Ciphertext},
		"short ct":     {tenantID, storeID, "k1", sealed.Enc, sealed.Ciphertext[:16]},
		"long ct":      {tenantID, storeID, "k1", sealed.Enc, make([]byte, 8193)},
		"bad tenant":   {"x", storeID, "k1", sealed.Enc, sealed.Ciphertext},
	} {
		if plain, err := ring.Open(tc.tenant, tc.store, tc.key, tc.enc, tc.ct); !errors.Is(err, ErrOpen) || plain != nil {
			t.Errorf("%s: %q,%v", name, plain, err)
		}
	}
	var nilRing *Keyring
	if _, err := nilRing.Open(tenantID, storeID, "k1", sealed.Enc, sealed.Ciphertext); !errors.Is(err, ErrOpen) {
		t.Error("nil ring")
	}
	// Two seals of the same token differ (fresh encapsulation) and both open.
	again, _ := sk.Seal(ads.SealInfo{TenantID: tenantID, StoreID: storeID}, token)
	if string(again.Enc) == string(sealed.Enc) {
		t.Error("encapsulated key reused")
	}
}

func TestSealKeyRotationOpensOldRows(t *testing.T) {
	old, cur := newPair(t), newPair(t)
	sealOld, _ := env(t, map[string]pair{"old": old}, "old")
	sealCur, openBoth := env(t, map[string]pair{"cur": cur, "old": old}, "cur")
	skOld, _ := metaads.LoadSealKeys(sealOld)
	skCur, _ := metaads.LoadSealKeys(sealCur)
	ring, err := LoadKeyring(openBoth)
	if err != nil {
		t.Fatal(err)
	}
	for _, sk := range []*metaads.SealKeys{skOld, skCur} {
		s, err := sk.Seal(ads.SealInfo{TenantID: tenantID, StoreID: storeID}, []byte("tok"))
		if err != nil {
			t.Fatal(err)
		}
		if p, err := ring.Open(tenantID, storeID, s.KeyID, s.Enc, s.Ciphertext); err != nil || string(p) != "tok" {
			t.Fatalf("%s: %q,%v", s.KeyID, p, err)
		}
	}
}

// In a container lcentry expands NAME_FILE into NAME=<contents>; that form must work, and setting both
// forms is refused.
func TestLoadKeyringInlineForm(t *testing.T) {
	p := newPair(t)
	doc := fmt.Sprintf(`{"keys":[{"id":"k1","private_key_base64":%q}]}`+"\n", p.prv)
	if _, err := LoadKeyring(func(k string) string {
		if k == envPrivateKeys {
			return doc
		}
		return ""
	}); err != nil {
		t.Fatalf("inline form: %v", err)
	}
	if _, err := LoadKeyring(func(k string) string {
		if k == envPrivateKeys {
			return doc
		}
		if k == envPrivateKeys+"_FILE" {
			return "/x"
		}
		return ""
	}); !errors.Is(err, ErrConfig) {
		t.Fatalf("both forms accepted: %v", err)
	}
}

func TestLoadKeyringRefusals(t *testing.T) {
	good := newPair(t)
	write := func(content string) func(string) string {
		f := filepath.Join(t.TempDir(), "k.json")
		if err := os.WriteFile(f, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		return func(k string) string {
			if k == envPrivateKeys+"_FILE" {
				return f
			}
			return ""
		}
	}
	entry := func(id, key string) string { return fmt.Sprintf(`{"id":%q,"private_key_base64":%q}`, id, key) }
	zero := base64.StdEncoding.EncodeToString(make([]byte, 32))
	dir := t.TempDir()
	for name, getenv := range map[string]func(string) string{
		"unset":            func(string) string { return "" },
		"missing file":     func(string) string { return filepath.Join(dir, "nope") },
		"directory":        func(string) string { return dir },
		"empty file":       write(""),
		"not json":         write("x"),
		"no keys":          write(`{"keys":[]}`),
		"unknown member":   write(`{"keys":[` + entry("k1", good.prv) + `],"x":1}`),
		"duplicate member": write(`{"keys":[{"id":"k1","id":"k2","private_key_base64":"` + good.prv + `"}]}`),
		"duplicate id":     write(`{"keys":[` + entry("k1", good.prv) + `,` + entry("k1", newPair(t).prv) + `]}`),
		"short key":        write(`{"keys":[` + entry("k1", "AAAA") + `]}`),
		"all-zero key":     write(`{"keys":[` + entry("k1", zero) + `]}`),
		"bad id":           write(`{"keys":[` + entry("k 1", good.prv) + `]}`),
		"trailing":         write(`{"keys":[` + entry("k1", good.prv) + `]}{}`),
		"oversize":         write(strings.Repeat(" ", 9000)),
	} {
		if r, err := LoadKeyring(getenv); !errors.Is(err, ErrConfig) || r != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
	if _, err := LoadKeyring(nil); !errors.Is(err, ErrConfig) {
		t.Error("nil getenv")
	}
	ring, err := LoadKeyring(write(`{"keys":[` + entry("k1", good.prv) + `]}`))
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{fmt.Sprintf("%v", ring), fmt.Sprintf("%+v", *ring), fmt.Sprintf("%#v", ring), fmt.Sprintf("%s", ring)} {
		if strings.Contains(s, "k1") || strings.Contains(s, good.prv) {
			t.Errorf("formatter leaks: %s", s)
		}
	}
	if b, _ := json.Marshal(ring); strings.Contains(string(b), "k1") {
		t.Error("json leaks")
	}
}
