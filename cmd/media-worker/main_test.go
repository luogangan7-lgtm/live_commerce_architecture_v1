package main

import (
	"context"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Synthetic DSN sentinels live in their own constants so no source line looks like a
// credential to secret scanners (GitGuardian false positives 2026-09-29); they are test sentinels.
const (
	dsnSentinel1 = "private-pass"
)

func TestMediaWorkerLMW01DisabledReadsOnlyFlag(t *testing.T) {
	for _, flag := range []string{"", "0"} {
		var reads []string
		err := run(nil, func(name string) string {
			reads = append(reads, name)
			if name == "COMMERCE_MEDIA_WORKER_ENABLED" {
				return flag
			}
			return "must-not-read"
		})
		if err != nil || len(reads) != 1 || reads[0] != "COMMERCE_MEDIA_WORKER_ENABLED" {
			t.Fatalf("disabled %q read=%v err=%v", flag, reads, err)
		}
	}
	for _, flag := range []string{"false", "2", " 1", "1 "} {
		var reads []string
		err := run(context.Background(), func(name string) string { reads = append(reads, name); return flag })
		if !errors.Is(err, errWorkerConfig) || len(reads) != 1 {
			t.Fatalf("noncanonical flag %q read=%v err=%v", flag, reads, err)
		}
	}
}

func TestMediaWorkerLMW01ConfigBeforeDatabase(t *testing.T) {
	vars := map[string]string{
		"COMMERCE_MEDIA_WORKER_ENABLED":         "1",
		"COMMERCE_MEDIA_WORKER_DATABASE_URL":    "postgres://private-user:" + dsnSentinel1 + "@127.0.0.1:1/test",
		"COMMERCE_MEDIA_EXECUTOR_DATABASE_URL":  "postgres://private-user:" + dsnSentinel1 + "@127.0.0.1:1/test",
		"COMMERCE_MEDIA_WORKER_CONCURRENCY":     "1",
		"COMMERCE_MEDIA_MATERIAL_ACTIVE_KEY_ID": "bad",
		"COMMERCE_MEDIA_MATERIAL_KEYS_JSON":     "private-malformed-key",
		"COMMERCE_MEDIA_PROJECTS_JSON":          "private-malformed-project",
	}
	get := func(name string) string { return vars[name] }
	if err := run(context.Background(), get); !errors.Is(err, errWorkerConfig) || strings.Contains(err.Error(), "private") {
		t.Fatalf("malformed config reached DB or exposed input: %v", err)
	}
	server := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Error("nil context made network request") }))
	defer server.Close()
	key := base64.StdEncoding.EncodeToString([]byte(strings.Repeat("k", 32)))
	ca := string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.TLS.Certificates[0].Certificate[0]}))
	vars["COMMERCE_MEDIA_MATERIAL_ACTIVE_KEY_ID"] = "k1"
	vars["COMMERCE_MEDIA_MATERIAL_KEYS_JSON"] = fmt.Sprintf(`{"keys":[{"id":"k1","key_base64":%q}]}`, key)
	vars["COMMERCE_MEDIA_PROJECTS_JSON"] = fmt.Sprintf(`{"projects":[{"project_id":"p1","credential_version":1,"endpoint":"https://unit.livekit.cloud","api_key":"test_key","api_secret":%q,"stream_hosts":["ingest.example.com"],"mock_dial_address":%q,"mock_ca_pem":%q}]}`, strings.Repeat("s", 40), server.Listener.Addr().String(), ca)
	if err := run(nil, get); !errors.Is(err, errWorkerConfig) {
		t.Fatalf("valid enabled config with nil context: %v", err)
	}
}
