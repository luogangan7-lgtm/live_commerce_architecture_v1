// main_test.go: configuration and secret-hygiene tests for cmd/claims-worker (MOCK tier, no database).
// Non-goal: the poller and dispatcher behavior (internal/claimsintake, internal/integrations/metareply,
// tests/foundation MCI gates).
package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"testing"
)

// Synthetic DSN sentinels live in their own constants so no source line looks like a
// credential to secret scanners (GitGuardian false positives 2026-09-29); they are test sentinels.
const (
	dsnSentinel1 = "sentinel-intake-7c1"
	dsnSentinel2 = "sentinel-worker-7c2"
	dsnSentinel3 = "sentinel-retention-7c3"
)

// syntheticDSN builds a test-only DSN with net/url so no source line is a credential-shaped
// literal (GitGuardian flagged every literal form of these sentinels, 2026-09-29).
func syntheticDSN(user, sentinel string) string {
	return (&url.URL{Scheme: "postgres", User: url.UserPassword(user, sentinel), Host: "synthetic.invalid", Path: "/db"}).String()
}

func b64(b byte) string { return base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{b}, 32)) }

func testEnv() map[string]string {
	return map[string]string{
		"COMMERCE_CLAIMS_WORKER_ENABLED":         "1",
		"COMMERCE_CLAIMS_INTAKE_DATABASE_URL":    syntheticDSN("intake", dsnSentinel1),
		"COMMERCE_WORKER_DATABASE_URL":           syntheticDSN("worker", dsnSentinel2),
		"COMMERCE_RETENTION_JOB_DATABASE_URL":    syntheticDSN("retention", dsnSentinel3),
		"COMMERCE_CLAIMS_REPLY_LINK_KEY":         b64(7),
		"COMMERCE_META_PAGE_TOKEN_ACTIVE_KEY_ID": "pt-1",
		"COMMERCE_META_PAGE_TOKEN_KEYS_JSON":     `{"keys":[{"id":"pt-1","key_base64":"` + b64(8) + `"}]}`,
		"COMMERCE_META_GRAPH_VERSION":            "v23.0",
	}
}

func TestDisabledReadsOnlyFlag(t *testing.T) {
	for _, v := range []string{"", "0"} {
		get := func(name string) string {
			if name != "COMMERCE_CLAIMS_WORKER_ENABLED" {
				t.Fatalf("disabled worker read %s", name)
			}
			return v
		}
		if c, err := loadConfig(get); err != nil || c.enabled {
			t.Fatal("disabled worker changed")
		}
		if err := run(context.Background(), get); err != nil {
			t.Fatal("disabled worker opened a dependency")
		}
	}
	for _, v := range []string{"true", "2", " 1"} {
		if _, err := loadConfig(func(string) string { return v }); !errors.Is(err, errWorkerConfig) {
			t.Fatalf("noncanonical flag %q accepted", v)
		}
	}
	if _, err := loadConfig(nil); !errors.Is(err, errWorkerConfig) {
		t.Fatal("nil environment accepted")
	}
}

// The worker must never learn the payload keyring, K_actor or Stripe variables (contract §5.3).
func TestOnlyDocumentedVariablesAreRead(t *testing.T) {
	values := testEnv()
	allowed := map[string]bool{}
	for name := range values {
		allowed[name] = true
	}
	allowed["COMMERCE_META_GRAPH_BASE_URL"], allowed["COMMERCE_META_GRAPH_AUTH_HEADER"] = true, true
	// ECPay CVS switches (taiwan-cvs-logistics-v1 §12): read on every start; the profile and keyring only when enabled.
	allowed["CVS_ECPAY_ENABLED"], allowed["CVS_ECPAY_LIVE_CREATE"], allowed["COMMERCE_CVS_HOOKS_ORIGIN"] = true, true, true
	c, err := loadConfig(func(name string) string {
		if !allowed[name] {
			t.Fatalf("read undocumented variable %s", name)
		}
		return values[name]
	})
	if err != nil || !c.enabled || c.graph.GraphBaseURL != "https://graph.facebook.com" || c.graph.AuthorizationHeader {
		t.Fatalf("valid configuration rejected or defaults wrong: %v", err)
	}
	values["COMMERCE_META_GRAPH_AUTH_HEADER"] = "1"
	values["COMMERCE_META_GRAPH_BASE_URL"] = "http://127.0.0.1:8123"
	if c, err = loadConfig(func(name string) string { return values[name] }); err != nil || !c.graph.AuthorizationHeader || c.graph.GraphBaseURL != "http://127.0.0.1:8123" {
		t.Fatalf("loopback MOCK configuration rejected: %v", err)
	}
}

func TestConfigRejections(t *testing.T) {
	bad := map[string]func(map[string]string){
		"missing intake dsn":    func(v map[string]string) { delete(v, "COMMERCE_CLAIMS_INTAKE_DATABASE_URL") },
		"blank worker dsn":      func(v map[string]string) { v["COMMERCE_WORKER_DATABASE_URL"] = "  " },
		"missing retention dsn": func(v map[string]string) { delete(v, "COMMERCE_RETENTION_JOB_DATABASE_URL") },
		"huge dsn":              func(v map[string]string) { v["COMMERCE_WORKER_DATABASE_URL"] = strings.Repeat("x", 8193) },
		"missing link key":      func(v map[string]string) { delete(v, "COMMERCE_CLAIMS_REPLY_LINK_KEY") },
		"short link key": func(v map[string]string) {
			v["COMMERCE_CLAIMS_REPLY_LINK_KEY"] = base64.StdEncoding.EncodeToString([]byte("short"))
		},
		"zero link key": func(v map[string]string) {
			v["COMMERCE_CLAIMS_REPLY_LINK_KEY"] = base64.StdEncoding.EncodeToString(make([]byte, 32))
		},
		"link key not base64": func(v map[string]string) { v["COMMERCE_CLAIMS_REPLY_LINK_KEY"] = "!!!" },
		"no page keyring":     func(v map[string]string) { delete(v, "COMMERCE_META_PAGE_TOKEN_KEYS_JSON") },
		"unknown active key":  func(v map[string]string) { v["COMMERCE_META_PAGE_TOKEN_ACTIVE_KEY_ID"] = "nope" },
		"no graph version":    func(v map[string]string) { delete(v, "COMMERCE_META_GRAPH_VERSION") },
		"bad graph version":   func(v map[string]string) { v["COMMERCE_META_GRAPH_VERSION"] = "23.0" },
		"foreign graph host":  func(v map[string]string) { v["COMMERCE_META_GRAPH_BASE_URL"] = "https://graph.example.com" },
		"non-loopback http":   func(v map[string]string) { v["COMMERCE_META_GRAPH_BASE_URL"] = "http://10.0.0.1:80" },
		"bad auth header":     func(v map[string]string) { v["COMMERCE_META_GRAPH_AUTH_HEADER"] = "yes" },
	}
	for name, mutate := range bad {
		v := testEnv()
		mutate(v)
		if _, err := loadConfig(func(n string) string { return v[n] }); !errors.Is(err, errWorkerConfig) {
			t.Errorf("%s accepted: %v", name, err)
		}
	}
}

func TestConfigNeverRendersSecrets(t *testing.T) {
	v := testEnv()
	c, err := loadConfig(func(n string) string { return v[n] })
	if err != nil {
		t.Fatal(err)
	}
	blob, _ := json.Marshal(c)
	for _, rendered := range []string{fmt.Sprint(c), fmt.Sprintf("%+v", c), fmt.Sprintf("%#v", c), string(blob)} {
		for _, secret := range []string{"sentinel-intake-7c1", "sentinel-worker-7c2", dsnSentinel3, v["COMMERCE_CLAIMS_REPLY_LINK_KEY"], b64(8)} {
			if strings.Contains(rendered, secret) {
				t.Fatalf("config rendering leaked a secret: %s", rendered)
			}
		}
		if !strings.Contains(rendered, "redacted") {
			t.Fatal("config rendering is not redacted")
		}
	}
}

// A valid environment reaches the database step and fails with one fixed error, no DSN text.
func TestValidEnvironmentFailsClosedWithoutLeaking(t *testing.T) {
	v := testEnv()
	err := run(context.Background(), func(n string) string { return v[n] })
	if !errors.Is(err, errWorkerDatabase) {
		t.Fatalf("got %v", err)
	}
	for _, banned := range []string{dsnSentinel1, dsnSentinel2, "synthetic.invalid", "intake", "postgres://"} {
		if strings.Contains(err.Error(), banned) {
			t.Fatalf("error leaked %q", banned)
		}
	}
}
