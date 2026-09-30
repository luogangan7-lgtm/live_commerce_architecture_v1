package main

import (
	"context"
	"encoding/base64"
	"errors"
	"testing"
)

func TestStudioBackendSTU05StartupAdmission(t *testing.T) {
	for _, value := range []string{"", "0"} {
		config, err := loadStudioConfig(func(string) string { return value }, false, "0.0.0.0:8080")
		if err != nil || config.enabled {
			t.Fatalf("default-off %q: %+v %v", value, config, err)
		}
		planner, err := buildStudioPlanner(context.Background(), nil, config)
		if err != nil || planner != nil {
			t.Fatalf("off touched DB: %v %v", planner, err)
		}
	}
	for _, value := range []string{"true", "false", "yes", "2", " 1", "1 ", "-1"} {
		_, err := loadStudioConfig(func(string) string { return value }, true, "127.0.0.1:8080")
		if !errors.Is(err, errStudioConfig) {
			t.Fatalf("noncanonical flag %q accepted: %v", value, err)
		}
	}
	for _, tc := range []struct {
		identity bool
		addr     string
	}{
		{false, "127.0.0.1:8080"},
		{true, "0.0.0.0:8080"},
		{true, "localhost:8080"},
		{true, "192.0.2.10:8080"},
	} {
		_, err := loadStudioConfig(func(string) string { return "1" }, tc.identity, tc.addr)
		if !errors.Is(err, errStudioConfig) {
			t.Fatalf("enabled without identity/loopback %+v: %v", tc, err)
		}
	}
	config, err := loadStudioConfig(func(string) string { return "1" }, true, "127.0.0.1:8080")
	if err != nil || !config.enabled {
		t.Fatalf("valid enabled config: %+v %v", config, err)
	}
	if planner, err := buildStudioPlanner(context.Background(), nil, config); planner != nil || !errors.Is(err, errStudioDatabase) {
		t.Fatalf("enabled nil pool false readiness: %v %v", planner, err)
	}
}

// R1 ruling G2: every COMMERCE_STUDIO_ENABLED x COMMERCE_STUDIO_MEDIA_ENABLED x COMMERCE_CLAIMS_ENABLED
// combination. Planning-only Studio never touches the media subsystem (a nil pool proves no
// live.media_plan_ready() query), media needs Studio, and claims need Studio only.
func TestStudioG2FlagMatrix(t *testing.T) {
	raw := make([]byte, 32)
	for i := range raw {
		raw[i] = byte(i + 7)
	}
	key := base64.RawURLEncoding.EncodeToString(raw)
	for _, tc := range []struct {
		studio, media, claims string
		configErr, claimsErr  bool
		wantMedia             bool
	}{
		{studio: "", media: "", claims: ""},
		{studio: "0", media: "0", claims: "0"},
		{studio: "1", media: "", claims: ""},
		{studio: "1", media: "0", claims: "0"},
		{studio: "1", media: "0", claims: "1"}, // R1 deploy shape
		{studio: "1", media: "1", claims: "0", wantMedia: true},
		{studio: "1", media: "1", claims: "1", wantMedia: true},
		{studio: "0", media: "0", claims: "1", claimsErr: true},
		{studio: "", media: "", claims: "1", claimsErr: true},
		{studio: "", media: "1", claims: "", configErr: true},
		{studio: "0", media: "1", claims: "1", configErr: true},
		{studio: "1", media: "true", claims: "", configErr: true},
		{studio: "1", media: " 1", claims: "", configErr: true},
	} {
		name := "studio=" + tc.studio + ",media=" + tc.media + ",claims=" + tc.claims
		env := map[string]string{"COMMERCE_STUDIO_ENABLED": tc.studio, "COMMERCE_STUDIO_MEDIA_ENABLED": tc.media,
			"COMMERCE_CLAIMS_ENABLED": tc.claims, "COMMERCE_CLAIMS_LABEL_KEY": key}
		getenv := func(name string) string { return env[name] }
		config, err := loadStudioConfig(getenv, true, "127.0.0.1:8080")
		if tc.configErr {
			if !errors.Is(err, errStudioConfig) {
				t.Fatalf("%s: accepted %+v %v", name, config, err)
			}
			continue
		}
		if err != nil || config.enabled != (tc.studio == "1") || config.media != tc.wantMedia {
			t.Fatalf("%s: %+v %v", name, config, err)
		}
		planner, err := buildStudioPlanner(context.Background(), nil, config)
		if tc.wantMedia {
			if planner != nil || !errors.Is(err, errStudioDatabase) {
				t.Fatalf("%s: media must require the media database readiness: %v %v", name, planner, err)
			}
		} else if planner != nil || err != nil {
			t.Fatalf("%s: media off touched the media subsystem: %v %v", name, planner, err)
		}
		claims, err := loadClaimsConfig(getenv, config.enabled)
		if tc.claimsErr {
			if !errors.Is(err, errClaimsConfig) {
				t.Fatalf("%s: claims without Studio accepted", name)
			}
			continue
		}
		if err != nil || (claims.labels != nil) != (tc.claims == "1") {
			t.Fatalf("%s: claims %v labels=%v", name, err, claims.labels != nil)
		}
	}
	// Media is still bound by Studio's identity/loopback boundary.
	if _, err := loadStudioConfig(func(name string) string { return "1" }, false, "127.0.0.1:8080"); !errors.Is(err, errStudioConfig) {
		t.Fatalf("media without identity accepted: %v", err)
	}
}
