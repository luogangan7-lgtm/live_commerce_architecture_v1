package main

import (
	"encoding/base64"
	"encoding/hex"
	"errors"
	"strings"
	"testing"
)

// KC13 startup admission: the API refuses to start with claims enabled unless Studio is
// enabled and COMMERCE_CLAIMS_LABEL_KEY is a canonical 32-byte key no other secret reuses.
func TestClaimsConfigStartupAdmission(t *testing.T) {
	raw := make([]byte, 32)
	for i := range raw {
		raw[i] = byte(i + 1)
	}
	key := base64.RawURLEncoding.EncodeToString(raw)
	for _, value := range []string{"", "0"} {
		config, err := loadClaimsConfig(func(name string) string {
			if name != "COMMERCE_CLAIMS_ENABLED" {
				t.Fatalf("disabled claims read %s", name)
			}
			return value
		}, true)
		if err != nil || config.labels != nil {
			t.Fatalf("disabled %q: %v", value, err)
		}
	}
	env := func(overrides map[string]string) func(string) string {
		return func(name string) string {
			if value, ok := overrides[name]; ok {
				return value
			}
			return map[string]string{"COMMERCE_CLAIMS_ENABLED": "1", "COMMERCE_CLAIMS_LABEL_KEY": key}[name]
		}
	}
	config, err := loadClaimsConfig(env(nil), true)
	if err != nil || config.labels == nil {
		t.Fatalf("valid enabled config rejected: %v", err)
	}
	if _, err := loadClaimsConfig(env(nil), false); !errors.Is(err, errClaimsConfig) {
		t.Fatal("claims enabled without Studio")
	}
	if _, err := loadClaimsConfig(nil, true); !errors.Is(err, errClaimsConfig) {
		t.Fatal("nil getenv accepted")
	}
	for name, overrides := range map[string]map[string]string{
		"noncanonical flag":   {"COMMERCE_CLAIMS_ENABLED": "true"},
		"missing key":         {"COMMERCE_CLAIMS_LABEL_KEY": ""},
		"short key":           {"COMMERCE_CLAIMS_LABEL_KEY": key[:42]},
		"padded key":          {"COMMERCE_CLAIMS_LABEL_KEY": key + "="},
		"standard alphabet":   {"COMMERCE_CLAIMS_LABEL_KEY": strings.Repeat("+", 42) + "A"},
		"nonzero tail bits":   {"COMMERCE_CLAIMS_LABEL_KEY": strings.Repeat("A", 42) + "B"},
		"reuses BFF key":      {"COMMERCE_BFF_KEY": key},
		"reuses buyer key":    {"COMMERCE_BUYER_BFF_KEY": key},
		"reuses replay key":   {"COMMERCE_ACCOUNT_REPLAY_KEY": key},
		"reuses OIDC secret":  {"COMMERCE_OIDC_CLIENT_SECRET": key},
		"in account keyring":  {"COMMERCE_ACCOUNT_KEYS_JSON": `{"k1":"` + base64.StdEncoding.EncodeToString(raw) + `"}`},
		"in meta payload":     {"COMMERCE_META_PAYLOAD_KEYS_JSON": `{"k1":"` + base64.URLEncoding.EncodeToString(raw) + `"}`},
		"in media material":   {"COMMERCE_MEDIA_MATERIAL_KEYS_JSON": `{"k1":"` + hex.EncodeToString(raw) + `"}`},
		"in meta app secrets": {"COMMERCE_META_APPS_JSON": `[{"secret":"` + key + `"}]`},
	} {
		if _, err := loadClaimsConfig(env(overrides), true); !errors.Is(err, errClaimsConfig) {
			t.Fatalf("%s: accepted (%v)", name, err)
		}
	}
	other := base64.RawURLEncoding.EncodeToString(make([]byte, 32))
	if _, err := loadClaimsConfig(env(map[string]string{"COMMERCE_BFF_KEY": other, "COMMERCE_ACCOUNT_KEYS_JSON": `{"k1":"` + other + `"}`}), true); err != nil {
		t.Fatalf("distinct keys rejected: %v", err)
	}
	if strings.Contains(config.labels.String()+config.labels.GoString(), key) {
		t.Fatal("label key formatted")
	}
}
