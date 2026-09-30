package main

// SP15 no-PG half for cmd/stripe-admin (contracts/stripe-psp-v1.md §12/§13; frozen
// seam: func run(ctx, args, getenv, stdout) error in stripe-b1-ingress-assembly).
// The CLI is the only reader of STRIPE_SECRET_KEY and STRIPE_WEBHOOK_SECRET[_NEXT],
// and each subcommand reads only its own variables. The exact flag names of
// `method` are not frozen in the brief; a wrong guess degrades that case to the
// flag-parse path (still proving no leak) and is reported as an ambiguity.

import (
	"bytes"
	"context"
	"encoding/base64"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// Synthetic DSN sentinels live in their own constants so no source line looks like a
// credential to secret scanners (GitGuardian false positives 2026-09-29); they are test sentinels.
const (
	sp15DSNSentinel1 = "pw-sentinel"
)

const (
	sp15AdminDSN    = "postgres://registrar-sentinel:" + sp15DSNSentinel1 + "@127.0.0.1:1/x"
	sp15AdminSecret = "sk_" + "test_sentinel0123456789abcdef"
	sp15AdminWhsec  = "whsec_" + "sentinel0123456789abcdef"
	sp15AdminWhNext = "whsec_" + "sentinel_next_0123456789"
	sp15UUIDA       = "3f2b8c1e-0d4a-4b6f-9a7e-5c1d2e3f4a5b"
	sp15UUIDB       = "4a3c9d2f-1e5b-4c70-8b8f-6d2e3f4a5b6c"
	sp15UUIDC       = "5b4d0e3a-2f6c-4d81-9c90-7e3f4a5b6c7d"
)

func sp15AdminEnv() map[string]string {
	key := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{7}, 32))
	replay := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{8}, 32))
	ring := `[{"id":"k1","key_base64":"` + key + `"}]`
	return map[string]string{
		"COMMERCE_STRIPE_REGISTRAR_DATABASE_URL": sp15AdminDSN,
		"COMMERCE_ACCOUNT_ACTIVE_KEY_ID":         "k1", "COMMERCE_ACCOUNT_KEYS_JSON": ring, "COMMERCE_ACCOUNT_REPLAY_KEY": replay,
		"COMMERCE_STRIPE_WEBHOOK_ACTIVE_KEY_ID": "k1", "COMMERCE_STRIPE_WEBHOOK_KEYS_JSON": ring, "COMMERCE_STRIPE_WEBHOOK_REPLAY_KEY": replay,
		"STRIPE_SECRET_KEY": sp15AdminSecret, "STRIPE_ACCOUNT_ID": "acct_SentinelAcct1",
		"STRIPE_WEBHOOK_SECRET": sp15AdminWhsec, "STRIPE_WEBHOOK_SECRET_NEXT": sp15AdminWhNext,
	}
}

func sp15AdminCommon() []string {
	return []string{"--tenant", sp15UUIDA, "--store", sp15UUIDB, "--principal", sp15UUIDC}
}

func sp15AdminArgs(sub string, extra ...string) []string {
	return append(append([]string{sub}, sp15AdminCommon()...), extra...)
}

func sp15AdminCases() map[string][]string {
	return map[string][]string{
		"register": sp15AdminArgs("register"),
		"rotate":   sp15AdminArgs("rotate", "--connection", sp15UUIDA, "--expected-version", "1"),
		"webhook":  sp15AdminArgs("webhook", "--connection", sp15UUIDA, "--profile", "PROVIDER_MOCK", "--expected-version", "0", "--enabled=true"),
		"qualify": sp15AdminArgs("qualify", "--connection", sp15UUIDA, "--expected-version", "1", "--profile", "SANDBOX", "--currency", "TWD",
			"--amount-minor", "2500", "--return-url", "https://checkout.example.test/payment/return"),
		"method": sp15AdminArgs("method", "--market", sp15UUIDA, "--country", "TW", "--connection", sp15UUIDB, "--qualification", sp15UUIDC, "--expected-version", "0",
			"--enabled=true", "--visible=true", "--sort", "1", "--min-minor", "2500", "--max-minor", "99999900", "--name-hans", "Stripe", "--name-hant", "Stripe", "--name-en", "Stripe"),
	}
}

func sp15Set(names ...string) map[string]bool {
	m := map[string]bool{}
	for _, n := range names {
		m[n] = true
	}
	return m
}

// each subcommand may read only its own variables (contract §12 table + brief env list).
var sp15AdminAllowed = map[string]map[string]bool{
	"register": sp15Set("COMMERCE_STRIPE_REGISTRAR_DATABASE_URL", "COMMERCE_ACCOUNT_ACTIVE_KEY_ID", "COMMERCE_ACCOUNT_KEYS_JSON", "COMMERCE_ACCOUNT_REPLAY_KEY", "STRIPE_SECRET_KEY", "STRIPE_ACCOUNT_ID"),
	"rotate":   sp15Set("COMMERCE_STRIPE_REGISTRAR_DATABASE_URL", "COMMERCE_ACCOUNT_ACTIVE_KEY_ID", "COMMERCE_ACCOUNT_KEYS_JSON", "COMMERCE_ACCOUNT_REPLAY_KEY", "STRIPE_SECRET_KEY", "STRIPE_ACCOUNT_ID"),
	"webhook":  sp15Set("COMMERCE_STRIPE_REGISTRAR_DATABASE_URL", "COMMERCE_STRIPE_WEBHOOK_ACTIVE_KEY_ID", "COMMERCE_STRIPE_WEBHOOK_KEYS_JSON", "COMMERCE_STRIPE_WEBHOOK_REPLAY_KEY", "STRIPE_WEBHOOK_SECRET", "STRIPE_WEBHOOK_SECRET_NEXT"),
	// qualify/method seal nothing, so they may need the registrar pool and (qualify SANDBOX) the API key + account only.
	// STRIPE_SANDBOX: rulings.md #7 "SANDBOX qualify runs with `STRIPE_SANDBOX=1`" (non-secret opt-in, fail closed).
	"qualify": sp15Set("COMMERCE_STRIPE_REGISTRAR_DATABASE_URL", "STRIPE_SANDBOX", "STRIPE_SECRET_KEY", "STRIPE_ACCOUNT_ID", "COMMERCE_ACCOUNT_ACTIVE_KEY_ID", "COMMERCE_ACCOUNT_KEYS_JSON", "COMMERCE_ACCOUNT_REPLAY_KEY",
		"COMMERCE_STRIPE_WEBHOOK_ACTIVE_KEY_ID", "COMMERCE_STRIPE_WEBHOOK_KEYS_JSON", "COMMERCE_STRIPE_WEBHOOK_REPLAY_KEY"),
	"method": sp15Set("COMMERCE_STRIPE_REGISTRAR_DATABASE_URL", "COMMERCE_ACCOUNT_ACTIVE_KEY_ID", "COMMERCE_ACCOUNT_KEYS_JSON", "COMMERCE_ACCOUNT_REPLAY_KEY",
		"COMMERCE_STRIPE_WEBHOOK_ACTIVE_KEY_ID", "COMMERCE_STRIPE_WEBHOOK_KEYS_JSON", "COMMERCE_STRIPE_WEBHOOK_REPLAY_KEY"),
}

func TestStripeSP15Process(t *testing.T) {
	ctx := context.Background()
	sentinels := []string{"registrar-sentinel", "pw-sentinel", sp15AdminSecret, sp15AdminWhsec, sp15AdminWhNext, "acct_SentinelAcct1"}

	t.Run("no_args_and_unknown_subcommand_read_nothing_and_print_nothing", func(t *testing.T) {
		for name, args := range map[string][]string{"none": nil, "unknown": {"bogus"}, "help_flag": {"--help"}, "empty": {""}} {
			var out bytes.Buffer
			read := []string{}
			err := run(ctx, args, func(n string) string { read = append(read, n); return sp15AdminEnv()[n] }, &out)
			if err == nil || len(read) != 0 || out.Len() != 0 {
				t.Fatalf("%s: err=%v reads=%v stdout=%q", name, err, read, out.String())
			}
		}
	})

	t.Run("each_subcommand_reads_only_its_own_variables_and_leaks_nothing", func(t *testing.T) {
		for name, args := range sp15AdminCases() {
			var out bytes.Buffer
			read := []string{}
			env := sp15AdminEnv()
			err := run(ctx, args, func(n string) string { read = append(read, n); return env[n] }, &out)
			if err == nil { // the registrar DSN points at a closed port: success is impossible
				t.Fatalf("%s: run succeeded without a database", name)
			}
			for _, n := range read {
				if !sp15AdminAllowed[name][n] {
					t.Fatalf("%s read %q", name, n)
				}
			}
			if out.Len() != 0 {
				t.Fatalf("%s wrote to stdout on failure: %q", name, out.String())
			}
			for _, s := range sentinels {
				if strings.Contains(err.Error(), s) {
					t.Fatalf("%s leaked a secret in its error", name)
				}
			}
			if strings.Contains(err.Error(), "postgres://") {
				t.Fatalf("%s leaked the DSN", name)
			}
		}
	})

	t.Run("register_and_rotate_never_read_signing_material_and_webhook_never_reads_api_keys", func(t *testing.T) {
		env := sp15AdminEnv()
		for _, tc := range []struct {
			cmd       string
			forbidden []string
		}{
			{"register", []string{"STRIPE_WEBHOOK_SECRET", "STRIPE_WEBHOOK_SECRET_NEXT"}},
			{"rotate", []string{"STRIPE_WEBHOOK_SECRET", "STRIPE_WEBHOOK_SECRET_NEXT"}},
			{"webhook", []string{"STRIPE_SECRET_KEY", "STRIPE_ACCOUNT_ID"}},
			{"method", []string{"STRIPE_SECRET_KEY", "STRIPE_ACCOUNT_ID", "STRIPE_WEBHOOK_SECRET", "STRIPE_WEBHOOK_SECRET_NEXT"}},
			{"qualify", []string{"STRIPE_WEBHOOK_SECRET", "STRIPE_WEBHOOK_SECRET_NEXT"}},
		} {
			read := []string{}
			_ = run(ctx, sp15AdminCases()[tc.cmd], func(n string) string { read = append(read, n); return env[n] }, &bytes.Buffer{})
			for _, n := range read {
				for _, f := range tc.forbidden {
					if n == f {
						t.Fatalf("%s read %s", tc.cmd, f)
					}
				}
			}
		}
	})

	t.Run("live_is_refused_by_the_cli", func(t *testing.T) {
		env := sp15AdminEnv()
		env["STRIPE_SECRET_KEY"] = "sk_" + "live_sentinel0123456789abcdef"
		var out bytes.Buffer
		if err := run(ctx, sp15AdminCases()["register"], func(n string) string { return env[n] }, &out); err == nil || out.Len() != 0 || strings.Contains(err.Error(), "sk_live_") {
			t.Fatalf("register with a live key: %v %q", err, out.String())
		}
		swap := func(args []string, from, to string) []string {
			out := append([]string{}, args...)
			for i, a := range out {
				if a == from {
					out[i] = to
				}
			}
			return out
		}
		for name, args := range map[string][]string{
			"qualify_live": swap(sp15AdminCases()["qualify"], "SANDBOX", "LIVE"),
			"webhook_live": swap(sp15AdminCases()["webhook"], "PROVIDER_MOCK", "LIVE"),
		} {
			out.Reset()
			if err := run(ctx, args, func(n string) string { return sp15AdminEnv()[n] }, &out); err == nil || out.Len() != 0 {
				t.Fatalf("%s: %v %q", name, err, out.String())
			}
		}
	})

	t.Run("bad_flags_and_ids_fail_without_leaking", func(t *testing.T) {
		env := sp15AdminEnv()
		for name, args := range map[string][]string{
			"missing_tenant":     {"register", "--store", sp15UUIDB, "--principal", sp15UUIDC},
			"bad_uuid":           {"register", "--tenant", "not-a-uuid", "--store", sp15UUIDB, "--principal", sp15UUIDC},
			"uppercase_uuid":     {"register", "--tenant", strings.ToUpper(sp15UUIDA), "--store", sp15UUIDB, "--principal", sp15UUIDC},
			"unknown_flag":       sp15AdminArgs("register", "--password", "sentinel-flag-value"),
			"negative_version":   sp15AdminArgs("rotate", "--connection", sp15UUIDA, "--expected-version", "-1"),
			"non_numeric":        sp15AdminArgs("rotate", "--connection", sp15UUIDA, "--expected-version", "one"),
			"positional_garbage": sp15AdminArgs("register", "extra-positional"),
		} {
			var out bytes.Buffer
			err := run(ctx, args, func(n string) string { return env[n] }, &out)
			if err == nil || out.Len() != 0 || strings.Contains(err.Error(), "sentinel") || strings.Contains(err.Error(), "postgres://") {
				t.Fatalf("%s: err=%v stdout=%q", name, err, out.String())
			}
		}
	})

	t.Run("source_guard_stripe_secrets_only_in_the_registrar_and_the_adapter", func(t *testing.T) {
		// String literals in production Go files only (a comment naming a variable reads nothing).
		root, err := filepath.Abs("../..")
		if err != nil {
			t.Fatal(err)
		}
		envName := regexp.MustCompile(`^STRIPE_[A-Z0-9_]+$`)
		type rule struct {
			name    string
			match   func(string) bool
			allowed []string // repo-relative directories that may hold such a literal
		}
		rules := []rule{
			{"a STRIPE_* environment name", envName.MatchString, []string{"cmd/stripe-admin", "internal/integrations/psp/stripe"}},
			{"COMMERCE_STRIPE_REGISTRAR_DATABASE_URL", func(v string) bool { return v == "COMMERCE_STRIPE_REGISTRAR_DATABASE_URL" }, []string{"cmd/stripe-admin"}},
			{"a signing-keyring name", func(v string) bool {
				return strings.HasPrefix(v, "COMMERCE_STRIPE_WEBHOOK_") && v != "COMMERCE_STRIPE_WEBHOOK_ENABLED"
			},
				[]string{"cmd/api", "cmd/stripe-admin"}},
		}
		checked := 0
		fset := token.NewFileSet()
		err = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			rel, _ := filepath.Rel(root, path)
			if d.IsDir() {
				switch d.Name() {
				case ".git", "node_modules", "output", ".worktrees", "apps", "docs", "contracts":
					return filepath.SkipDir
				}
				return nil
			}
			if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			file, err := parser.ParseFile(fset, path, nil, 0)
			if err != nil {
				return err
			}
			checked++
			ast.Inspect(file, func(n ast.Node) bool {
				lit, ok := n.(*ast.BasicLit)
				if !ok || lit.Kind != token.STRING {
					return true
				}
				value, err := strconv.Unquote(lit.Value)
				if err != nil {
					return true
				}
				for _, r := range rules {
					if !r.match(value) {
						continue
					}
					allowed := false
					for _, dir := range r.allowed {
						allowed = allowed || strings.HasPrefix(rel, dir+string(filepath.Separator))
					}
					if !allowed {
						t.Errorf("%s holds %s outside %v", rel, r.name, r.allowed)
					}
				}
				return true
			})
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		if checked < 50 {
			t.Fatalf("source guard walked only %d files; wrong root?", checked)
		}
	})
}
