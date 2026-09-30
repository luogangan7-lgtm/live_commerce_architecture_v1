package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
)

func workerTestEnv() map[string]string {
	return map[string]string{
		"COMMERCE_META_WORKER_ENABLED":        "1",
		"COMMERCE_META_WORKER_DATABASE_URL":   "postgres://synthetic.invalid/worker",
		"COMMERCE_META_CONSUMER_DATABASE_URL": "postgres://synthetic.invalid/consumer",
		"COMMERCE_META_PAYLOAD_ACTIVE_KEY_ID": "active",
		"COMMERCE_META_PAYLOAD_KEYS_JSON":     `{"keys":[{"id":"active","key_base64":"` + base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{5}, 32)) + `"}]}`,
	}
}

func TestMetaWorkerDisabledReadsOnlyFlag(t *testing.T) {
	for _, flagValue := range []string{"", "0"} {
		get := func(name string) string {
			if name != "COMMERCE_META_WORKER_ENABLED" {
				t.Fatal("disabled worker read authority setting")
			}
			return flagValue
		}
		c, err := loadConfig(get)
		if err != nil || c.enabled {
			t.Fatal("disabled worker changed")
		}
		if err := run(context.Background(), get); err != nil {
			t.Fatal("disabled worker opened dependency")
		}
	}
	for _, flagValue := range []string{"true", "2", " 1"} {
		if _, err := loadConfig(func(string) string { return flagValue }); !errors.Is(err, errWorkerConfig) {
			t.Fatal("noncanonical flag accepted")
		}
	}
}

func TestMetaWorkerConfigBoundsAndNoAppSecrets(t *testing.T) {
	values := workerTestEnv()
	get := func(name string) string {
		if name == "COMMERCE_META_APPS_JSON" {
			t.Fatal("worker read app secret configuration")
		}
		return values[name]
	}
	c, err := loadConfig(get)
	if err != nil || !c.enabled || c.concurrency != 4 || c.keys == nil {
		t.Fatal("valid worker configuration rejected")
	}
	for _, raw := range []string{"1", "16"} {
		values["COMMERCE_META_WORKER_CONCURRENCY"] = raw
		c, err := loadConfig(get)
		if err != nil || c.concurrency != 1 && raw == "1" || c.concurrency != 16 && raw == "16" {
			t.Fatal("concurrency boundary rejected")
		}
	}
	for _, raw := range []string{"0", "17", "01", "+1", "1 ", "1.0", "x"} {
		values["COMMERCE_META_WORKER_CONCURRENCY"] = raw
		if _, err := loadConfig(get); !errors.Is(err, errWorkerConfig) {
			t.Fatal("invalid concurrency accepted")
		}
	}
	values["COMMERCE_META_WORKER_CONCURRENCY"] = ""
	for _, name := range []string{"COMMERCE_META_WORKER_DATABASE_URL", "COMMERCE_META_CONSUMER_DATABASE_URL"} {
		original := values[name]
		for _, value := range []string{"", " ", strings.Repeat("x", 8193)} {
			values[name] = value
			if _, err := loadConfig(get); !errors.Is(err, errWorkerConfig) {
				t.Fatal("invalid database URL accepted")
			}
		}
		values[name] = original
	}
	if _, err := loadConfig(nil); !errors.Is(err, errWorkerConfig) {
		t.Fatal("nil environment accepted")
	}
	for _, rendered := range []string{fmt.Sprint(c), fmt.Sprintf("%+v", c), fmt.Sprintf("%#v", c)} {
		if strings.Contains(rendered, c.workerDSN) || strings.Contains(rendered, c.consumerDSN) || !strings.Contains(rendered, "redacted") {
			t.Fatal("worker config formatting leaked URL")
		}
	}
	encoded, err := json.Marshal(c)
	if err != nil || strings.Contains(string(encoded), c.workerDSN) || !strings.Contains(string(encoded), "redacted") {
		t.Fatal("worker config JSON leaked URL")
	}
}

func TestMetaWorkerClaimsActorKeyConfig(t *testing.T) {
	values := workerTestEnv()
	get := func(name string) string { return values[name] }
	// Unset: staging off, everything else unchanged.
	if c, err := loadConfig(get); err != nil || c.actor != (workerConfig{}).actor {
		t.Fatal("absent actor key must leave staging off")
	}
	good := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{9}, 32))
	values["COMMERCE_CLAIMS_ACTOR_KEY"] = good
	c, err := loadConfig(get)
	if err != nil || c.actor == (workerConfig{}).actor {
		t.Fatal("valid actor key rejected")
	}
	for _, rendered := range []string{fmt.Sprint(c), fmt.Sprintf("%+v", c), fmt.Sprintf("%#v", c)} {
		if strings.Contains(rendered, good) {
			t.Fatal("worker config leaked the actor key")
		}
	}
	for _, bad := range []string{"not base64", good[:43], base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{9}, 31)),
		base64.StdEncoding.EncodeToString(make([]byte, 32)), " " + good} {
		values["COMMERCE_CLAIMS_ACTOR_KEY"] = bad
		if _, err := loadConfig(get); !errors.Is(err, errWorkerConfig) {
			t.Fatalf("malformed actor key %q accepted", bad)
		}
	}
	// The key is read only when the worker is enabled.
	values["COMMERCE_META_WORKER_ENABLED"] = "0"
	if err := run(context.Background(), func(name string) string {
		if name != "COMMERCE_META_WORKER_ENABLED" {
			t.Fatal("disabled worker read the actor key")
		}
		return "0"
	}); err != nil {
		t.Fatal(err)
	}
}
