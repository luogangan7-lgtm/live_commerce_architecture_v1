// ads_test.go: usage, environment gating and secret-hygiene tests of the `ads-settings` subcommand (MOCK tier).
// Non-goal: the ads.operator_set_settings SQL (REAL_PG, exercised in internal/ads and by the independent MA02 gate).
package main

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

const adsIDs = "--tenant 11111111-1111-4111-8111-111111111111 --store 22222222-2222-4222-8222-222222222222"

func adsEnv() map[string]string {
	return map[string]string{"COMMERCE_META_REGISTRAR_DATABASE_URL": "postgres://operator:" + dsnSentinel1 + "@127.0.0.1:1/lc"}
}

func TestAdsSettingsUsageErrorsAreFixedAndPrintNothing(t *testing.T) {
	for _, line := range []string{
		"ads-settings",           // no ids
		"ads-settings " + adsIDs, // nothing to change
		"ads-settings --tenant nope --store 22222222-2222-4222-8222-222222222222 --environment LIVE",
		"ads-settings " + adsIDs + " --environment PRODUCTION",
		"ads-settings " + adsIDs + " --environment live",
		"ads-settings " + adsIDs + " --sandbox-ad-account act_123",
		"ads-settings " + adsIDs + " --max-active-budget-minor -1",
		"ads-settings " + adsIDs + " --max-active-budget-minor 100000000001",
		"ads-settings " + adsIDs + " --max-active-budget-minor abc",
		"ads-settings " + adsIDs + " --allowance-currency EUR",
		"ads-settings " + adsIDs + " --environment LIVE extra",
		"ads-settings " + adsIDs + " --nope=leak-value",
	} {
		out, err := do(t, adsEnv(), line)
		if !errors.Is(err, errUsage) || out != "" || strings.Contains(err.Error(), "leak-value") || strings.Contains(err.Error(), "abc") {
			t.Fatalf("%q -> %q %v", line, out, err)
		}
	}
}

func TestAdsSettingsEnvironmentGatesBeforeAnyConnection(t *testing.T) {
	saved := setAdsSettings
	defer func() { setAdsSettings = saved }()
	setAdsSettings = func(context.Context, string, adsSettings) (time.Time, error) {
		t.Fatal("connected before the environment was validated")
		return time.Time{}, nil
	}
	for _, dsn := range []string{"", "  ", strings.Repeat("x", 9000)} {
		v := adsEnv()
		v["COMMERCE_META_REGISTRAR_DATABASE_URL"] = dsn
		if _, err := do(t, v, "ads-settings "+adsIDs+" --environment SANDBOX"); !errors.Is(err, errConfig) {
			t.Fatalf("dsn %q -> %v", dsn[:min(len(dsn), 5)], err)
		}
	}
}

func TestAdsSettingsPassesOnlyTheGivenFlagsAndPrintsTimestampOnly(t *testing.T) {
	saved := setAdsSettings
	defer func() { setAdsSettings = saved }()
	var got adsSettings
	at := time.Date(2026, 10, 1, 8, 0, 0, 0, time.UTC)
	setAdsSettings = func(_ context.Context, _ string, s adsSettings) (time.Time, error) { got = s; return at, nil }
	out, err := do(t, adsEnv(), "ads-settings "+adsIDs+" --environment LIVE --max-active-budget-minor 0 --sandbox-ad-account")
	// A trailing valueless flag is a usage error, not a silent clear.
	if !errors.Is(err, errUsage) || out != "" {
		t.Fatalf("valueless flag: %q %v", out, err)
	}
	out, err = do(t, adsEnv(), "ads-settings "+adsIDs+" --environment LIVE --max-active-budget-minor 500000 --allowance-currency TWD")
	if err != nil || out != "{\"updated_at\":\"2026-10-01T08:00:00Z\"}\n" {
		t.Fatalf("out=%q err=%v", out, err)
	}
	if got.Environment == nil || *got.Environment != "LIVE" || got.MaxActiveBudgetMinor == nil || *got.MaxActiveBudgetMinor != 500000 ||
		got.AllowanceCurrency == nil || *got.AllowanceCurrency != "TWD" || got.SandboxAdAccount != nil {
		t.Fatalf("parsed %+v", got)
	}
	// An explicit empty sandbox account clears it (pointer to "").
	if _, err = do(t, adsEnv(), "ads-settings "+adsIDs+" --sandbox-ad-account="); err != nil || got.SandboxAdAccount == nil || *got.SandboxAdAccount != "" {
		t.Fatalf("clear: %v %+v", err, got)
	}
	// Zero allowance switches ads off and is valid.
	if _, err = do(t, adsEnv(), "ads-settings "+adsIDs+" --max-active-budget-minor 0"); err != nil || got.MaxActiveBudgetMinor == nil || *got.MaxActiveBudgetMinor != 0 {
		t.Fatalf("zero allowance: %v", err)
	}
}

func TestAdsSettingsDatabaseFailuresAreMasked(t *testing.T) {
	out, err := do(t, adsEnv(), "ads-settings "+adsIDs+" --environment SANDBOX")
	if !errors.Is(err, errRegister) || out != "" { // connection refused surfaces at the first query, masked
		t.Fatalf("%q %v", out, err)
	}
	for _, banned := range []string{dsnSentinel1, "127.0.0.1", "operator"} {
		if strings.Contains(err.Error(), banned) {
			t.Fatalf("error leaked %s", banned)
		}
	}
	v := adsEnv()
	v["COMMERCE_META_REGISTRAR_DATABASE_URL"] = "postgres://operator:" + dsnSentinel1 + "@[bad"
	if _, err = do(t, v, "ads-settings "+adsIDs+" --environment SANDBOX"); !errors.Is(err, errDatabase) || strings.Contains(err.Error(), dsnSentinel1) {
		t.Fatalf("parse error not masked: %v", err)
	}
}
