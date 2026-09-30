// keyring_test.go: unit tests for the logistics keyring (E5) and the CVS_ECPAY_* config. Key bytes are
// generated per run from a counter pattern; no key-shaped literal exists in this file.

package ecpay

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func keyB64(seed byte) string {
	b := make([]byte, 32)
	for i := range b {
		b[i] = seed + byte(i)
	}
	return base64.StdEncoding.EncodeToString(b)
}

func ringJSON(active string, ids ...string) string {
	type k struct {
		ID  string `json:"id"`
		Key string `json:"key_base64"`
	}
	doc := struct {
		Active string `json:"active"`
		Keys   []k    `json:"keys"`
	}{Active: active}
	for i, id := range ids {
		doc.Keys = append(doc.Keys, k{id, keyB64(byte(i*40 + 1))})
	}
	b, _ := json.Marshal(doc)
	return string(b)
}

func envOf(v string) func(string) string {
	return func(k string) string {
		if k == envKeyring {
			return v
		}
		return ""
	}
}

func testScope() Scope {
	return Scope{
		TenantID: "11111111-1111-4111-8111-111111111111", StoreID: "22222222-2222-4222-8222-222222222222",
		ConnectionID: "33333333-3333-4333-8333-333333333333", MerchantID: "2000933", Environment: EnvSandbox, Version: 3,
	}
}

func TestKeyringSealOpen(t *testing.T) {
	k, err := LoadKeyring(envOf(ringJSON("k2", "k1", "k2")))
	if err != nil {
		t.Fatal(err)
	}
	s := testScope()
	id, nonce, ct, err := k.Seal(s, goodPayload())
	if err != nil || id != "k2" || len(nonce) != 12 {
		t.Fatalf("seal: %q %d %v", id, len(nonce), err)
	}
	if bytes.Contains(ct, []byte(tKey)) {
		t.Fatal("ciphertext contains the key in the clear")
	}
	got, err := k.Open(s, id, nonce, ct)
	if err != nil || got != goodPayload() {
		t.Fatalf("open: %+v %v", got, err)
	}
	// Nonces are fresh per seal.
	_, n2, _, _ := k.Seal(s, goodPayload())
	if bytes.Equal(nonce, n2) {
		t.Error("nonce reused")
	}
	// Every AAD field, and the key id, is bound.
	for name, mut := range map[string]func(*Scope){
		"tenant":      func(x *Scope) { x.TenantID = "99999999-9999-4999-8999-999999999999" },
		"store":       func(x *Scope) { x.StoreID = "99999999-9999-4999-8999-999999999999" },
		"connection":  func(x *Scope) { x.ConnectionID = "99999999-9999-4999-8999-999999999999" },
		"merchant":    func(x *Scope) { x.MerchantID = "2000132" },
		"environment": func(x *Scope) { x.Environment = EnvLive },
		"version":     func(x *Scope) { x.Version = 4 },
	} {
		w := s
		mut(&w)
		if _, err := k.Open(w, id, nonce, ct); !errors.Is(err, ErrSecret) {
			t.Errorf("wrong %s opened: %v", name, err)
		}
	}
	if _, err := k.Open(s, "k1", nonce, ct); !errors.Is(err, ErrSecret) {
		t.Error("wrong key id opened")
	}
	if _, err := k.Open(s, "nope", nonce, ct); !errors.Is(err, ErrSecret) {
		t.Error("unknown key id opened")
	}
	bad := append([]byte(nil), ct...)
	bad[0] ^= 1
	if _, err := k.Open(s, id, nonce, bad); !errors.Is(err, ErrSecret) {
		t.Error("tampered ciphertext opened")
	}
	if _, err := k.Open(s, id, nonce[:8], ct); !errors.Is(err, ErrSecret) {
		t.Error("short nonce opened")
	}
	// Rotation: a ciphertext sealed under k1 still opens while k1 stays in the ring.
	k1, _ := LoadKeyring(envOf(ringJSON("k1", "k1", "k2")))
	oid, on, oc, _ := k1.Seal(s, goodPayload())
	if _, err := k.Open(s, oid, on, oc); err != nil {
		t.Errorf("old key must still open after rotation: %v", err)
	}
}

func TestKeyringSealRefusals(t *testing.T) {
	k, _ := LoadKeyring(envOf(ringJSON("k1", "k1")))
	s := testScope()
	for name, mut := range map[string]func(*Scope, *Payload){
		"tenant not uuid": func(x *Scope, _ *Payload) { x.TenantID = "t" },
		"env":             func(x *Scope, _ *Payload) { x.Environment = "STAGING" },
		"version zero":    func(x *Scope, _ *Payload) { x.Version = 0 },
		"merchant":        func(x *Scope, _ *Payload) { x.MerchantID = "" },
		"no key":          func(_ *Scope, p *Payload) { p.HashKey = "" },
		"space in iv":     func(_ *Scope, p *Payload) { p.HashIV = "a b" },
		"sender name":     func(_ *Scope, p *Payload) { p.SenderName = "陳" },
		"sender phone":    func(_ *Scope, p *Payload) { p.SenderCellPhone = "12" },
	} {
		w, p := s, goodPayload()
		mut(&w, &p)
		if _, _, _, err := k.Seal(w, p); !errors.Is(err, ErrSecret) {
			t.Errorf("%s: err=%v", name, err)
		}
	}
	if _, _, _, err := (*Keyring)(nil).Seal(s, goodPayload()); !errors.Is(err, ErrSecret) {
		t.Error("nil keyring sealed")
	}
	// Empty sender fields are allowed at seal time (validated per subtype at create, F5).
	p := goodPayload()
	p.SenderName, p.SenderCellPhone = "", ""
	if _, _, _, err := k.Seal(s, p); err != nil {
		t.Errorf("empty sender: %v", err)
	}
}

func TestKeyringLoadRefusals(t *testing.T) {
	good := ringJSON("k1", "k1", "k2")
	zero := base64.StdEncoding.EncodeToString(make([]byte, 32))
	short := base64.StdEncoding.EncodeToString(make([]byte, 16))
	same := keyB64(1)
	bigJSON := `{"active":"k1","keys":[{"id":"k1","key_base64":"` + keyB64(1) + `"}],"pad":"` + strings.Repeat("x", maxKeyringJSON) + `"}`
	many := func(n int) string {
		var items []string
		for i := 0; i < n; i++ {
			b := make([]byte, 32)
			_, _ = rand.Read(b)
			items = append(items, `{"id":"k`+string(rune('a'+i))+`","key_base64":"`+base64.StdEncoding.EncodeToString(b)+`"}`)
		}
		return `{"active":"ka","keys":[` + strings.Join(items, ",") + `]}`
	}
	for name, v := range map[string]string{
		"empty":              "",
		"not json":           "nope",
		"unknown member":     strings.Replace(good, `"active"`, `"extra":1,"active"`, 1),
		"duplicate active":   strings.Replace(good, `{"active":"k1"`, `{"active":"k1","active":"k2"`, 1),
		"duplicate keys":     strings.Replace(good, `"keys":`, `"keys":[],"keys":`, 1),
		"duplicate nested":   strings.Replace(good, `"id":"k1"`, `"id":"k1","id":"k9"`, 1),
		"trailing data":      good + "{}",
		"active missing":     ringJSON("zz", "k1"),
		"active bad chars":   ringJSON("k 1", "k 1"),
		"no keys":            `{"active":"k1","keys":[]}`,
		"zero key":           `{"active":"k1","keys":[{"id":"k1","key_base64":"` + zero + `"}]}`,
		"short key":          `{"active":"k1","keys":[{"id":"k1","key_base64":"` + short + `"}]}`,
		"raw base64":         `{"active":"k1","keys":[{"id":"k1","key_base64":"` + strings.TrimRight(same, "=") + `"}]}`,
		"duplicate material": `{"active":"k1","keys":[{"id":"k1","key_base64":"` + same + `"},{"id":"k2","key_base64":"` + same + `"}]}`,
		"duplicate id":       `{"active":"k1","keys":[{"id":"k1","key_base64":"` + keyB64(1) + `"},{"id":"k1","key_base64":"` + keyB64(9) + `"}]}`,
		"oversize":           bigJSON,
		"17 keys":            many(17),
	} {
		k, err := LoadKeyring(envOf(v))
		if k != nil || !errors.Is(err, ErrConfig) {
			t.Errorf("%s: got %v %v", name, k, err)
		}
		if err != nil && strings.Contains(err.Error(), same) {
			t.Errorf("%s: error echoes key material", name)
		}
	}
	if _, err := LoadKeyring(nil); !errors.Is(err, ErrConfig) {
		t.Error("nil getenv")
	}
	if _, err := LoadKeyring(envOf(many(16))); err != nil {
		t.Errorf("16 keys must load: %v", err)
	}
}

func TestConfigLoad(t *testing.T) {
	env := func(m map[string]string) func(string) string { return func(k string) string { return m[k] } }
	ok := map[string]string{"CVS_ECPAY_ENABLED": "1", "COMMERCE_CVS_HOOKS_ORIGIN": "https://hooks.example.test"}
	cases := []struct {
		name string
		env  map[string]string
		want Config
		err  bool
	}{
		{"defaults off", nil, Config{}, false},
		{"enabled", ok, Config{Enabled: true, HooksOrigin: "https://hooks.example.test"}, false},
		{"trailing slash normalised", map[string]string{"CVS_ECPAY_ENABLED": "1", "COMMERCE_CVS_HOOKS_ORIGIN": "https://hooks.example.test/"}, Config{Enabled: true, HooksOrigin: "https://hooks.example.test"}, false},
		{"port kept", map[string]string{"CVS_ECPAY_ENABLED": "1", "COMMERCE_CVS_HOOKS_ORIGIN": "https://hooks.example.test:8443"}, Config{Enabled: true, HooksOrigin: "https://hooks.example.test:8443"}, false},
		{"live create", map[string]string{"CVS_ECPAY_ENABLED": "1", "CVS_ECPAY_LIVE_CREATE": "1", "COMMERCE_CVS_HOOKS_ORIGIN": "https://h.example.test"}, Config{Enabled: true, LiveCreate: true, HooksOrigin: "https://h.example.test"}, false},
		{"disabled ignores bad origin", map[string]string{"COMMERCE_CVS_HOOKS_ORIGIN": "http://nope"}, Config{}, false},
		{"enabled no origin", map[string]string{"CVS_ECPAY_ENABLED": "1"}, Config{}, true},
		{"http origin", map[string]string{"CVS_ECPAY_ENABLED": "1", "COMMERCE_CVS_HOOKS_ORIGIN": "http://hooks.example.test"}, Config{}, true},
		{"origin with path", map[string]string{"CVS_ECPAY_ENABLED": "1", "COMMERCE_CVS_HOOKS_ORIGIN": "https://hooks.example.test/v1"}, Config{}, true},
		{"origin with query", map[string]string{"CVS_ECPAY_ENABLED": "1", "COMMERCE_CVS_HOOKS_ORIGIN": "https://hooks.example.test?x=1"}, Config{}, true},
		{"origin with userinfo", map[string]string{"CVS_ECPAY_ENABLED": "1", "COMMERCE_CVS_HOOKS_ORIGIN": "https://u@hooks.example.test"}, Config{}, true},
		{"non-ascii origin", map[string]string{"CVS_ECPAY_ENABLED": "1", "COMMERCE_CVS_HOOKS_ORIGIN": "https://例え.test"}, Config{}, true},
		{"bad enabled flag", map[string]string{"CVS_ECPAY_ENABLED": "yes"}, Config{}, true},
		{"bad live flag", map[string]string{"CVS_ECPAY_LIVE_CREATE": "true"}, Config{}, true},
	}
	for _, c := range cases {
		got, err := LoadConfig(env(c.env))
		if (err != nil) != c.err || got != c.want {
			t.Errorf("%s: got %+v err=%v", c.name, got, err)
		}
	}
	if _, err := LoadConfig(nil); !errors.Is(err, ErrConfig) {
		t.Error("nil getenv")
	}
}
