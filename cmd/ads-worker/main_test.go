// main_test.go: configuration and secret-hygiene tests for cmd/ads-worker (MOCK tier, no database).
// Non-goal: the dispatcher, Check and sweeper behaviour (internal/integrations/core, internal/ads,
// tests/foundation MA gates).
package main

import (
	"context"
	"crypto/ecdh"
	"crypto/hpke"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// dsnSentinel is a synthetic test value in its own neutrally named constant; the DSN is assembled
// with net/url so no source line is a credential-shaped literal (GitGuardian, 2026-09-29).
const dsnSentinel = "sentinel-adsworker-7c3"

func syntheticDSN() string {
	return (&url.URL{Scheme: "postgres", User: url.UserPassword("worker", dsnSentinel), Host: "synthetic.invalid", Path: "/db"}).String()
}

func keyFile(t *testing.T) string {
	t.Helper()
	priv, err := hpke.DHKEM(ecdh.X25519()).GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := priv.Bytes()
	doc, _ := json.Marshal(map[string]any{"keys": []map[string]string{{"id": "k1", "private_key_base64": base64.StdEncoding.EncodeToString(raw)}}})
	f := filepath.Join(t.TempDir(), "hpke.json")
	if err := os.WriteFile(f, doc, 0o600); err != nil {
		t.Fatal(err)
	}
	return f
}

func capiKeyFile(t *testing.T) string {
	t.Helper()
	f := filepath.Join(t.TempDir(), "capi-key")
	if err := os.WriteFile(f, []byte("capi-external-id-key-fixture-32byte!"), 0o600); err != nil {
		t.Fatal(err)
	}
	return f
}

func testEnv(t *testing.T) map[string]string {
	return map[string]string{
		"COMMERCE_ADS_WORKER_DATABASE_URL":               syntheticDSN(),
		"COMMERCE_META_ADS_GRAPH_VERSION":                "v26.0",
		"COMMERCE_META_ADS_TOKEN_HPKE_PRIVATE_KEYS_FILE": keyFile(t),
		"COMMERCE_META_ADS_PARTNER_AGENT":                "lc-ads",
		"COMMERCE_CAPI_EXTERNAL_ID_KEY_FILE":             capiKeyFile(t),
	}
}

func getenvOf(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

func TestLoadConfigOK(t *testing.T) {
	c, err := loadConfig(getenvOf(testEnv(t)))
	if err != nil || c.keys == nil || c.graph.GraphBaseURL != "https://graph.facebook.com" || c.graph.GraphVersion != "v26.0" || c.graph.PartnerAgent != "lc-ads" {
		t.Fatalf("config = %+v,%v", c.graph, err)
	}
	for _, s := range []string{fmt.Sprintf("%v", c), fmt.Sprintf("%+v", c), fmt.Sprintf("%#v", c)} {
		if strings.Contains(s, dsnSentinel) || strings.Contains(s, "synthetic.invalid") {
			t.Errorf("config formatter leaks: %s", s)
		}
	}
	if b, _ := json.Marshal(c); strings.Contains(string(b), dsnSentinel) {
		t.Error("config json leaks")
	}
}

// Every variable is required: dropping or corrupting any one refuses to start with the one fixed code.
func TestEveryVariableIsRequired(t *testing.T) {
	for name := range testEnv(t) {
		env := testEnv(t)
		delete(env, name)
		if _, err := loadConfig(getenvOf(env)); !errors.Is(err, errWorkerConfig) {
			t.Errorf("missing %s accepted: %v", name, err)
		}
	}
	for name, mut := range map[string]func(map[string]string){
		"blank dsn":             func(m map[string]string) { m["COMMERCE_ADS_WORKER_DATABASE_URL"] = "   " },
		"bad version":           func(m map[string]string) { m["COMMERCE_META_ADS_GRAPH_VERSION"] = "26" },
		"bad partner":           func(m map[string]string) { m["COMMERCE_META_ADS_PARTNER_AGENT"] = "bad agent" },
		"missing file":          func(m map[string]string) { m["COMMERCE_META_ADS_TOKEN_HPKE_PRIVATE_KEYS_FILE"] += ".nope" },
		"missing capi key file": func(m map[string]string) { m["COMMERCE_CAPI_EXTERNAL_ID_KEY_FILE"] += ".nope" },
	} {
		env := testEnv(t)
		mut(env)
		if _, err := loadConfig(getenvOf(env)); !errors.Is(err, errWorkerConfig) {
			t.Errorf("%s accepted: %v", name, err)
		}
	}
	if _, err := loadConfig(nil); !errors.Is(err, errWorkerConfig) {
		t.Error("nil environment accepted")
	}
}

// The worker must read only the four documented names (never the app secret, Stripe, page-token or
// claims variables) and must not accept a Graph base URL override.
func TestOnlyDocumentedVariablesAreRead(t *testing.T) {
	values := testEnv(t)
	allowed := map[string]bool{"COMMERCE_META_ADS_TOKEN_HPKE_PRIVATE_KEYS": true, "COMMERCE_CAPI_EXTERNAL_ID_KEY": true} // lcentry-expanded forms of the _FILE variables
	for name := range values {
		allowed[name] = true
	}
	if _, err := loadConfig(func(name string) string {
		if !allowed[name] {
			t.Fatalf("read undocumented variable %s", name)
		}
		return values[name]
	}); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"COMMERCE_META_ADS_APP_SECRET_FILE", "COMMERCE_META_GRAPH_BASE_URL", "COMMERCE_META_PAGE_TOKEN_KEYS_JSON"} {
		if allowed[name] {
			t.Errorf("%s must not be an ads-worker variable", name)
		}
	}
}

func TestRunRefusesBadConfigBeforeAnyDependency(t *testing.T) {
	if err := run(context.Background(), getenvOf(map[string]string{})); !errors.Is(err, errWorkerConfig) {
		t.Fatalf("run = %v", err)
	}
	env := testEnv(t)
	if err := run(nil, getenvOf(env)); !errors.Is(err, errWorkerConfig) { //nolint:staticcheck // nil ctx is the case under test
		t.Fatalf("nil ctx = %v", err)
	}
}

func TestDispatcherOptionsKeepLeaseInequality(t *testing.T) {
	o := dispatcherOptions()
	lease := float64(o.LeaseSeconds)
	if o.CallTimeout.Seconds()+3*o.DBTimeout.Seconds()+1 >= lease {
		t.Fatalf("options violate the loader lease inequality: %+v", o)
	}
	if queueAds != "ads" {
		t.Fatal("queue name is frozen by post_river/0015")
	}
}
