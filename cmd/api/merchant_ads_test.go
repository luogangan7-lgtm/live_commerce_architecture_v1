// merchant_ads_test.go: configuration, secret hygiene and the MA11 static custody check for the
// merchant ads service builder (MOCK tier, no database round trip).
package main

import (
	"context"
	"crypto/ecdh"
	"crypto/hpke"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Synthetic values, neutrally named; the DSN is assembled with net/url (GitGuardian, 2026-09-29).
const (
	adsSecretSentinel = "adsAPPSECRETsentinel0123456789abcd"
	adsDSNSentinel    = "sentinel-adsapi-7c4"
)

func adsPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := (&url.URL{Scheme: "postgres", User: url.UserPassword("runtime", adsDSNSentinel), Host: "synthetic.invalid", Path: "/db"}).String()
	pool, err := pgxpool.New(context.Background(), dsn) // lazy: no connection is attempted
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func adsEnv(t *testing.T) map[string]string {
	t.Helper()
	priv, err := hpke.DHKEM(ecdh.X25519()).GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	keys, _ := json.Marshal(map[string]any{"keys": []map[string]string{{"id": "k1", "public_key_base64": base64.StdEncoding.EncodeToString(priv.PublicKey().Bytes())}}})
	secretFile := filepath.Join(t.TempDir(), "app-secret")
	if err := os.WriteFile(secretFile, []byte(adsSecretSentinel+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return map[string]string{
		"COMMERCE_META_ADS_APP_ID":                      "4291253377792879",
		"COMMERCE_META_ADS_APP_SECRET_FILE":             secretFile,
		"COMMERCE_META_ADS_CONFIG_ID":                   "123456789012345",
		"COMMERCE_META_ADS_REDIRECT_URI":                "https://admin.example.test/api/admin/ads/meta/callback",
		"COMMERCE_META_ADS_GRAPH_VERSION":               "v26.0",
		"COMMERCE_META_ADS_TOKEN_HPKE_PUBLIC_KEYS_JSON": string(keys),
		"COMMERCE_META_ADS_TOKEN_HPKE_ACTIVE_KEY_ID":    "k1",
	}
}

func adsGetenv(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

func TestMerchantAdsSurfaceOffReadsOnlyAppID(t *testing.T) {
	svc, err := newMerchantAds(nil, func(name string) string {
		if name != "COMMERCE_META_ADS_APP_ID" {
			t.Fatalf("surface-off read %s", name)
		}
		return ""
	})
	if svc != nil || err != nil {
		t.Fatalf("off = %v,%v", svc, err)
	}
}

func TestMerchantAdsBuildsWhenConfigured(t *testing.T) {
	svc, err := newMerchantAds(adsPool(t), adsGetenv(adsEnv(t)))
	if err != nil || svc == nil {
		t.Fatalf("svc = %v,%v", svc, err)
	}
}

// Container form: lcentry turns COMMERCE_META_ADS_APP_SECRET_FILE into COMMERCE_META_ADS_APP_SECRET.
func TestMerchantAdsAcceptsLcentryExpandedSecret(t *testing.T) {
	env := adsEnv(t)
	delete(env, "COMMERCE_META_ADS_APP_SECRET_FILE")
	env["COMMERCE_META_ADS_APP_SECRET"] = adsSecretSentinel
	if svc, err := newMerchantAds(adsPool(t), adsGetenv(env)); err != nil || svc == nil {
		t.Fatalf("inline secret: %v,%v", svc, err)
	}
	both := adsEnv(t)
	both["COMMERCE_META_ADS_APP_SECRET"] = adsSecretSentinel
	if svc, err := newMerchantAds(adsPool(t), adsGetenv(both)); svc != nil || !errors.Is(err, errMerchantAdsConfig) {
		t.Fatalf("both forms accepted: %v,%v", svc, err)
	}
}

func TestMerchantAdsEveryValueIsRequiredOnceEnabled(t *testing.T) {
	pool := adsPool(t)
	for name := range adsEnv(t) {
		if name == "COMMERCE_META_ADS_APP_ID" {
			continue
		}
		env := adsEnv(t)
		delete(env, name)
		svc, err := newMerchantAds(pool, adsGetenv(env))
		if svc != nil || !errors.Is(err, errMerchantAdsConfig) {
			t.Errorf("missing %s accepted: %v,%v", name, svc, err)
		}
	}
	for name, mut := range map[string]func(map[string]string){
		"non-numeric config id": func(m map[string]string) { m["COMMERCE_META_ADS_CONFIG_ID"] = "abc" },
		"http redirect":         func(m map[string]string) { m["COMMERCE_META_ADS_REDIRECT_URI"] = "http://admin.example.test/cb" },
		"bad graph version":     func(m map[string]string) { m["COMMERCE_META_ADS_GRAPH_VERSION"] = "26.0" },
		"non-numeric app id":    func(m map[string]string) { m["COMMERCE_META_ADS_APP_ID"] = "app" },
		"active key not found":  func(m map[string]string) { m["COMMERCE_META_ADS_TOKEN_HPKE_ACTIVE_KEY_ID"] = "k2" },
		"secret file is a dir":  func(m map[string]string) { m["COMMERCE_META_ADS_APP_SECRET_FILE"] = t.TempDir() },
		"secret with space":     func(m map[string]string) { os.WriteFile(m["COMMERCE_META_ADS_APP_SECRET_FILE"], []byte("a b"), 0o600) },
	} {
		env := adsEnv(t)
		mut(env)
		if svc, err := newMerchantAds(pool, adsGetenv(env)); svc != nil || !errors.Is(err, errMerchantAdsConfig) {
			t.Errorf("%s accepted: %v,%v", name, svc, err)
		}
	}
	if svc, err := newMerchantAds(nil, adsGetenv(adsEnv(t))); svc != nil || !errors.Is(err, errMerchantAdsConfig) {
		t.Errorf("nil pool accepted: %v,%v", svc, err)
	}
	if _, err := newMerchantAds(pool, nil); !errors.Is(err, errMerchantAdsConfig) {
		t.Error("nil getenv accepted")
	}
}

// Failure text is one fixed code: it never carries the secret, the file path or the DSN.
func TestMerchantAdsErrorsCarryNoSecret(t *testing.T) {
	env := adsEnv(t)
	env["COMMERCE_META_ADS_GRAPH_VERSION"] = "bad"
	_, err := newMerchantAds(adsPool(t), adsGetenv(env))
	for _, leak := range []string{adsSecretSentinel, env["COMMERCE_META_ADS_APP_SECRET_FILE"], adsDSNSentinel} {
		if err == nil || strings.Contains(err.Error(), leak) {
			t.Fatalf("error %v leaks %q", err, leak)
		}
	}
}

// MA11 (static): cmd/api's dependency graph must not contain the HPKE private-key loader, or the
// API process could read a token back. The positive control proves `go list` really saw the ads adapter.
func TestAPIBinaryHasNoTokenOpener(t *testing.T) {
	out, err := exec.Command("go", "list", "-deps", ".").CombinedOutput()
	if err != nil {
		t.Fatalf("go list failed (MA11 static check cannot run): %v", err)
	}
	deps := string(out)
	if !strings.Contains(deps, "livecommerce/internal/integrations/meta_ads\n") {
		t.Fatal("positive control failed: cmd/api does not depend on the ads adapter")
	}
	if strings.Contains(deps, "meta_ads/tokenopen") {
		t.Fatal("cmd/api depends on meta_ads/tokenopen: token custody broken (MA11)")
	}
}
