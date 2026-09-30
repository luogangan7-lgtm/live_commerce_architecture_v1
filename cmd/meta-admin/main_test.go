// main_test.go: usage, environment gating and secret-hygiene tests for the Meta registrar CLI (MOCK tier).
// Non-goal: the registry SQL (REAL_PG, tests/foundation) and any Graph call.
package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"strings"
	"testing"

	"livecommerce/internal/integrations/metareply"
)

// Synthetic DSN sentinels live in their own constants so no source line looks like a
// credential to secret scanners (GitGuardian false positives 2026-09-29); they are test sentinels.
const (
	dsnSentinel1 = "sentinel-operator-9f"
)

const fakeToken = "EAAB" + "cli-sentinel-page-token-0123456789"

func env() map[string]string {
	k := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{4}, 32))
	return map[string]string{
		"COMMERCE_META_REGISTRAR_DATABASE_URL":   "postgres://operator:" + dsnSentinel1 + "@127.0.0.1:1/lc",
		"META_PAGE_ACCESS_TOKEN":                 fakeToken,
		"COMMERCE_META_PAGE_TOKEN_ACTIVE_KEY_ID": "pt-1",
		"COMMERCE_META_PAGE_TOKEN_KEYS_JSON":     `{"keys":[{"id":"pt-1","key_base64":"` + k + `"}]}`,
	}
}

const ids = "--tenant 11111111-1111-4111-8111-111111111111 --store 22222222-2222-4222-8222-222222222222 " +
	"--principal 33333333-3333-4333-8333-333333333333 --binding 44444444-4444-4444-8444-444444444444 " +
	"--provider facebook --asset 1234567890 --expected-version 0 --scopes pages_messaging"

func do(t *testing.T, values map[string]string, line string) (string, error) {
	t.Helper()
	var out bytes.Buffer
	err := run(context.Background(), strings.Fields(line), func(n string) string { return values[n] }, &out)
	return out.String(), err
}

func TestUsageErrorsAreFixedAndPrintNothing(t *testing.T) {
	for _, line := range []string{"", "bogus", "page-token extra", "page-token --nope=EAAB_leak",
		"page-token --expected-version=abc", strings.Replace("page-token "+ids, "--provider facebook", "--provider tiktok", 1),
		strings.Replace("page-token "+ids, "--asset 1234567890", "--asset 12ab", 1),
		strings.Replace("page-token "+ids, "pages_messaging", "Pages-Messaging", 1),
		strings.Replace("page-token "+ids, "--expected-version 0", "--expected-version -1", 1),
		strings.Replace("page-token "+ids, "--tenant 11111111-1111-4111-8111-111111111111", "--tenant nope", 1)} {
		out, err := do(t, env(), line)
		if !errors.Is(err, errUsage) || out != "" || strings.Contains(err.Error(), "leak") || strings.Contains(err.Error(), "abc") {
			t.Fatalf("%q -> %q %v", line, out, err)
		}
	}
}

func TestEnvironmentGatesBeforeAnyConnection(t *testing.T) {
	saved := register
	defer func() { register = saved }()
	register = func(context.Context, string, *metareply.PageTokenKeyring, metareply.Registration, string) (int64, error) {
		t.Fatal("connected before the environment was valid")
		return 0, nil
	}
	for name, mutate := range map[string]func(map[string]string){
		"no dsn":     func(v map[string]string) { delete(v, "COMMERCE_META_REGISTRAR_DATABASE_URL") },
		"no token":   func(v map[string]string) { delete(v, "META_PAGE_ACCESS_TOKEN") },
		"no keyring": func(v map[string]string) { delete(v, "COMMERCE_META_PAGE_TOKEN_KEYS_JSON") },
		"bad active": func(v map[string]string) { v["COMMERCE_META_PAGE_TOKEN_ACTIVE_KEY_ID"] = "zz" },
	} {
		v := env()
		mutate(v)
		if out, err := do(t, v, "page-token "+ids); !errors.Is(err, errConfig) || out != "" {
			t.Fatalf("%s: %q %v", name, out, err)
		}
	}
}

func TestSuccessPrintsVersionOnly(t *testing.T) {
	saved := register
	defer func() { register = saved }()
	var got metareply.Registration
	var gotToken string
	register = func(_ context.Context, _ string, _ *metareply.PageTokenKeyring, r metareply.Registration, token string) (int64, error) {
		got, gotToken = r, token
		return 3, nil
	}
	out, err := do(t, env(), "page-token "+strings.Replace(strings.Replace(ids, "--expected-version 0", "--expected-version 2", 1), "pages_messaging", "pages_messaging,pages_read_engagement", 1))
	if err != nil || out != "{\"version\":3}\n" {
		t.Fatalf("out=%q err=%v", out, err)
	}
	if got.ExpectedVersion != 2 || len(got.Scopes) != 2 || got.Provider != "facebook" || got.AssetID != "1234567890" || gotToken != fakeToken {
		t.Fatalf("registration %+v", got)
	}
	if strings.Contains(out, fakeToken) {
		t.Fatal("token printed")
	}
}

func TestDatabaseFailuresAreMasked(t *testing.T) {
	for _, line := range []string{"page-token " + ids} {
		out, err := do(t, env(), line)
		if !errors.Is(err, errRegister) || out != "" { // connection refused surfaces at the first query, masked
			t.Fatalf("%q: %q %v", line, out, err)
		}
		for _, banned := range []string{dsnSentinel1, "127.0.0.1", "operator", fakeToken} {
			if strings.Contains(err.Error(), banned) {
				t.Fatalf("error leaked %s", banned)
			}
		}
	}
	// A malformed DSN must not echo itself either.
	v := env()
	v["COMMERCE_META_REGISTRAR_DATABASE_URL"] = "postgres://operator:" + dsnSentinel1 + "@[bad"
	if _, err := do(t, v, "page-token "+ids); !errors.Is(err, errDatabase) || strings.Contains(err.Error(), dsnSentinel1) {
		t.Fatalf("parse error not masked: %v", err)
	}
}

const routeArgs = "route --tenant 11111111-1111-4111-8111-111111111111 --store 22222222-2222-4222-8222-222222222222 " +
	"--principal 33333333-3333-4333-8333-333333333333 --app 123456789012345 --object page --asset 1234567890 " +
	"--proof 0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef --proof-expires 2099-01-01T00:00:00Z --expected-epoch 0"

// F2: route / route-disable validate every flag before connecting, print only IDs/epochs, mask DB errors.
func TestRouteSubcommands(t *testing.T) {
	savedR, savedD := route, disable
	defer func() { route, disable = savedR, savedD }()
	var got metareply.RouteRegistration
	route = func(_ context.Context, _ string, r metareply.RouteRegistration) (any, error) {
		got = r
		return metareply.RouteResult{BindingID: "44444444-4444-4444-8444-444444444444", BindingVersion: 1, RouteID: "55555555-5555-4555-8555-555555555555", RouteEpoch: 1}, nil
	}
	disable = func(_ context.Context, _ string, id string, epoch int64) (any, error) {
		return map[string]int64{"route_epoch": epoch + 1}, nil
	}
	out, err := do(t, env(), routeArgs)
	if err != nil || out != `{"binding_id":"44444444-4444-4444-8444-444444444444","binding_version":1,"route_id":"55555555-5555-4555-8555-555555555555","route_epoch":1}`+"\n" ||
		got.Object != "page" || got.AppID != "123456789012345" || got.ExpectedEpoch != 0 || got.ProofExpires.Year() != 2099 {
		t.Fatalf("route: %q %v %+v", out, err, got)
	}
	if out, err := do(t, env(), "route-disable --route 55555555-5555-4555-8555-555555555555 --expected-epoch 1"); err != nil || out != `{"route_epoch":2}`+"\n" {
		t.Fatalf("disable: %q %v", out, err)
	}
	route = func(context.Context, string, metareply.RouteRegistration) (any, error) {
		t.Fatal("connected on invalid input")
		return nil, nil
	}
	disable = func(context.Context, string, string, int64) (any, error) {
		t.Fatal("connected on invalid input")
		return nil, nil
	}
	for _, line := range []string{
		strings.Replace(routeArgs, "--object page", "--object tiktok", 1),
		strings.Replace(routeArgs, "--proof 0123", "--proof ABCD", 1),
		strings.Replace(routeArgs, "2099-01-01T00:00:00Z", "2001-01-01T00:00:00Z", 1),
		strings.Replace(routeArgs, " --expected-epoch 0", "", 1),
		strings.Replace(routeArgs, "--app 123456789012345", "--app x1", 1),
		routeArgs + " --route 55555555-5555-4555-8555-555555555555",
		"route-disable --route 55555555-5555-4555-8555-555555555555 --expected-epoch 0",
		"route-disable --route nope --expected-epoch 1",
	} {
		if out, err := do(t, env(), line); !errors.Is(err, errUsage) || out != "" {
			t.Fatalf("%q -> %q %v", line, out, err)
		}
	}
	route, disable = savedR, savedD
	if out, err := do(t, env(), routeArgs); !errors.Is(err, errRegister) || out != "" || strings.Contains(err.Error(), dsnSentinel1) {
		t.Fatalf("db failure not masked: %q %v", out, err)
	}
}
