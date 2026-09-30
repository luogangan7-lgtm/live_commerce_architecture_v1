package metareply

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"livecommerce/internal/command"
)

const (
	tenantT  = "11111111-1111-4111-8111-111111111111"
	storeT   = "22222222-2222-4222-8222-222222222222"
	bindingT = "66666666-6666-4666-8666-666666666666"
	fakeTok  = "EAAB" + "fake-page-token-0123456789"
)

func testKeyring(t *testing.T) *PageTokenKeyring {
	t.Helper()
	k, err := NewPageTokenKeyring("k1", map[string][]byte{"k1": bytes.Repeat([]byte{1}, 32), "k0": bytes.Repeat([]byte{2}, 32)})
	if err != nil {
		t.Fatal(err)
	}
	return k
}

func testScope() PageTokenScope {
	return PageTokenScope{TenantID: tenantT, StoreID: storeT, BindingID: bindingT, Provider: "facebook", AssetID: "1234567890", Version: 3}
}

func TestSealOpenRoundTripAndBinding(t *testing.T) {
	k := testKeyring(t)
	keyID, nonce, ct, err := k.Seal(testScope(), fakeTok)
	if err != nil || keyID != "k1" || len(nonce) != 12 || len(ct) < 17 || len(ct) > 8192 {
		t.Fatalf("seal: %v %s %d %d", err, keyID, len(nonce), len(ct))
	}
	if bytes.Contains(ct, []byte(fakeTok)) {
		t.Fatal("ciphertext contains the plaintext token")
	}
	got, err := k.Open(testScope(), keyID, nonce, ct)
	if err != nil || string(got.Reveal()) != fakeTok {
		t.Fatalf("open: %v", err)
	}
	// Every AAD member must be bound: changing any one must fail.
	mutations := map[string]func(*PageTokenScope){
		"tenant":   func(s *PageTokenScope) { s.TenantID = "99999999-9999-4999-8999-999999999999" },
		"store":    func(s *PageTokenScope) { s.StoreID = "99999999-9999-4999-8999-999999999999" },
		"binding":  func(s *PageTokenScope) { s.BindingID = "99999999-9999-4999-8999-999999999999" },
		"provider": func(s *PageTokenScope) { s.Provider = "instagram" },
		"asset":    func(s *PageTokenScope) { s.AssetID = "1234567891" },
		"version":  func(s *PageTokenScope) { s.Version = 4 },
	}
	for name, mutate := range mutations {
		s := testScope()
		mutate(&s)
		if _, err := k.Open(s, keyID, nonce, ct); !errors.Is(err, ErrSecret) {
			t.Fatalf("AAD member %s not bound: %v", name, err)
		}
	}
	if _, err := k.Open(testScope(), "k0", nonce, ct); !errors.Is(err, ErrSecret) {
		t.Fatalf("wrong key id opened: %v", err)
	}
	tampered := append([]byte(nil), ct...)
	tampered[0] ^= 1
	if _, err := k.Open(testScope(), keyID, nonce, tampered); !errors.Is(err, ErrSecret) {
		t.Fatalf("tampered ciphertext opened: %v", err)
	}
	// Rotation: a ring that still holds k1 but is active on k0 opens the old row.
	rotated, _ := NewPageTokenKeyring("k0", map[string][]byte{"k1": bytes.Repeat([]byte{1}, 32), "k0": bytes.Repeat([]byte{2}, 32)})
	if got, err := rotated.Open(testScope(), "k1", nonce, ct); err != nil || string(got.Reveal()) != fakeTok {
		t.Fatalf("rotation: %v", err)
	}
	if id, _, _, _ := rotated.Seal(testScope(), fakeTok); id != "k0" {
		t.Fatalf("seal used %s, want active k0", id)
	}
	// Fresh nonce per seal.
	_, n2, _, _ := k.Seal(testScope(), fakeTok)
	if bytes.Equal(nonce, n2) {
		t.Fatal("nonce reused")
	}
}

func TestSealRejectsBadInput(t *testing.T) {
	k := testKeyring(t)
	for name, tok := range map[string]string{"empty": "", "space": "a b", "control": "a\nb", "non-ascii": "toké", "huge": strings.Repeat("a", maxTokenBytes+1)} {
		if _, _, _, err := k.Seal(testScope(), tok); !errors.Is(err, ErrSecret) {
			t.Fatalf("%s token accepted: %v", name, err)
		}
	}
	for name, mutate := range map[string]func(*PageTokenScope){
		"provider":  func(s *PageTokenScope) { s.Provider = "whatsapp" },
		"asset":     func(s *PageTokenScope) { s.AssetID = "abc" },
		"version":   func(s *PageTokenScope) { s.Version = 0 },
		"tenant id": func(s *PageTokenScope) { s.TenantID = "nope" },
	} {
		s := testScope()
		mutate(&s)
		if _, _, _, err := k.Seal(s, fakeTok); !errors.Is(err, ErrSecret) {
			t.Fatalf("scope %s accepted: %v", name, err)
		}
	}
	var nilRing *PageTokenKeyring
	if _, _, _, err := nilRing.Seal(testScope(), fakeTok); err == nil {
		t.Fatal("nil ring sealed")
	}
}

func keysJSON(entries ...[2]string) string {
	items := make([]map[string]string, 0, len(entries))
	for _, e := range entries {
		items = append(items, map[string]string{"id": e[0], "key_base64": e[1]})
	}
	b, _ := json.Marshal(map[string]any{"keys": items})
	return string(b)
}

func TestLoadPageTokenKeyringFromEnv(t *testing.T) {
	k1 := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{1}, 32))
	k2 := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{2}, 32))
	env := func(active, raw string) func(string) string {
		return func(name string) string {
			switch name {
			case "COMMERCE_META_PAGE_TOKEN_ACTIVE_KEY_ID":
				return active
			case "COMMERCE_META_PAGE_TOKEN_KEYS_JSON":
				return raw
			}
			return ""
		}
	}
	ring, err := LoadPageTokenKeyring(env("k1", keysJSON([2]string{"k1", k1}, [2]string{"k2", k2})))
	if err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := ring.Seal(testScope(), fakeTok); err != nil {
		t.Fatal(err)
	}
	bad := map[string]func(string) string{
		"nil getenv":        nil,
		"empty":             env("k1", ""),
		"unknown active":    env("zz", keysJSON([2]string{"k1", k1})),
		"missing active":    env("", keysJSON([2]string{"k1", k1})),
		"duplicate id":      env("k1", keysJSON([2]string{"k1", k1}, [2]string{"k1", k2})),
		"duplicate key":     env("k1", keysJSON([2]string{"k1", k1}, [2]string{"k2", k1})),
		"short key":         env("k1", keysJSON([2]string{"k1", base64.StdEncoding.EncodeToString(make([]byte, 16))})),
		"zero key":          env("k1", keysJSON([2]string{"k1", base64.StdEncoding.EncodeToString(make([]byte, 32))})),
		"url-safe base64":   env("k1", keysJSON([2]string{"k1", strings.NewReplacer("+", "-", "/", "_").Replace(base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{0xfb}, 32)))})),
		"unknown member":    env("k1", `{"keys":[{"id":"k1","key_base64":"`+k1+`","x":1}]}`),
		"unknown top":       env("k1", `{"keys":[{"id":"k1","key_base64":"`+k1+`"}],"x":1}`),
		"duplicate member":  env("k1", `{"keys":[{"id":"k1","id":"k1","key_base64":"`+k1+`"}]}`),
		"escaped member":    env("k1", `{"keys":[{"id":"k1","\u0069d":"zz","key_base64":"`+k1+`"}]}`),
		"duplicate keys":    env("k1", `{"keys":[{"id":"k1","key_base64":"`+k1+`"}],"keys":[{"id":"k1","key_base64":"`+k1+`"}]}`),
		"trailing":          env("k1", keysJSON([2]string{"k1", k1})+` {}`),
		"bad id":            env("k 1", keysJSON([2]string{"k 1", k1})),
		"not json":          env("k1", "nope"),
		"empty key list":    env("k1", `{"keys":[]}`),
		"oversize document": env("k1", strings.Repeat(" ", 8193)),
	}
	for name, getenv := range bad {
		if _, err := LoadPageTokenKeyring(getenv); !errors.Is(err, ErrConfig) {
			t.Fatalf("%s accepted: %v", name, err)
		}
	}
}

func TestKeyringRedactionAndCopy(t *testing.T) {
	raw := bytes.Repeat([]byte{1}, 32)
	k, _ := NewPageTokenKeyring("k1", map[string][]byte{"k1": raw})
	raw[0] = 9
	_, nonce, ct, _ := k.Seal(testScope(), fakeTok)
	if _, err := k.Open(testScope(), "k1", nonce, ct); err != nil {
		t.Fatalf("keyring aliases the caller key: %v", err)
	}
	out := fmt.Sprintf("%v %+v %#v %s", k, k, k, k) + fmt.Sprint(*k)
	if strings.Contains(out, "\x01\x01") {
		t.Fatal("key bytes leaked")
	}
	if j, _ := json.Marshal(k); string(j) != `"[redacted]"` {
		t.Fatalf("json = %s", j)
	}
	for _, c := range []struct {
		active string
		keys   map[string][]byte
	}{{"", nil}, {"k1", map[string][]byte{}}, {"k1", map[string][]byte{"k1": make([]byte, 31)}}, {"k9", map[string][]byte{"k1": bytes.Repeat([]byte{1}, 32)}}} {
		if _, err := NewPageTokenKeyring(c.active, c.keys); !errors.Is(err, ErrConfig) {
			t.Fatalf("bad keyring accepted: %q", c.active)
		}
	}
}

func TestRegisterPageTokenValidatesBeforeSQL(t *testing.T) {
	k := testKeyring(t)
	ok := Registration{TenantID: tenantT, StoreID: storeT, PrincipalID: storeT, BindingID: bindingT, Provider: "facebook", AssetID: "1", Scopes: []string{"pages_messaging"}}
	// A nil pool is rejected before any SQL; every invalid field is command.ErrInvalid.
	if _, err := RegisterPageToken(context.Background(), nil, k, ok, fakeTok); !errors.Is(err, command.ErrInvalid) {
		t.Fatalf("nil pool: %v", err)
	}
}
