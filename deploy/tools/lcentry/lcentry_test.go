// File: deploy/tools/lcentry/lcentry_test.go
// Purpose: table tests for lcentry's *_FILE expansion and loopback probe
// (deploy-design §6). Run: go test -count=1 -cover ./deploy/tools/...
// Runs as/in: developer host / CI; no containers, no network beyond loopback
// httptest servers. Reads env/secrets: none (temp dirs only, fake values).
// Gate: deploy/scripts/smoke.sh S04 enforces coverage >= 90%.
// Status: MODEL_ONLY (unit level). Change rules: every rule in fileenv.go and
// probe.go needs a case here; never put real secrets in fixtures.

package main

import (
	"bytes"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// withRoot points secretsRoot at a fresh temp dir for one test.
func withRoot(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	old := secretsRoot
	secretsRoot = dir
	t.Cleanup(func() { secretsRoot = old })
	return dir
}

func writeSecret(t *testing.T, dir, name string, data []byte) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, data, 0o440); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestExpandFileEnv(t *testing.T) {
	dir := withRoot(t)
	dsn := writeSecret(t, dir, "dsn", []byte("postgres://fake\n"))
	crlf := writeSecret(t, dir, "crlf", []byte("value\r\n"))
	twoNL := writeSecret(t, dir, "twonl", []byte("value\n\n"))
	unset := writeSecret(t, dir, "unset", []byte("__UNSET__\n"))
	env, err := expandFileEnv([]string{
		"PATH=/bin",
		"DATABASE_URL_FILE=" + dsn,
		"COMMERCE_A_FILE=" + crlf,
		"COMMERCE_B_FILE=" + twoNL,
		"COMMERCE_C_FILE=" + unset,
		"FOO_FILE=/elsewhere/untouched",
		"NOEQUALS",
	})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"PATH=/bin", "FOO_FILE=/elsewhere/untouched", "NOEQUALS",
		"DATABASE_URL=postgres://fake", "COMMERCE_A=value", "COMMERCE_B=value\n"}
	if !slices.Equal(env, want) {
		t.Fatalf("env = %q, want %q", env, want)
	}
	for _, kv := range env {
		if strings.HasPrefix(kv, "COMMERCE_C") || strings.Contains(kv, "_FILE=/") && !strings.HasPrefix(kv, "FOO_FILE=") {
			t.Fatalf("unexpected entry %q", kv)
		}
	}
}

func TestExpandFileEnvErrors(t *testing.T) {
	dir := withRoot(t)
	good := writeSecret(t, dir, "good", []byte("x"))
	empty := writeSecret(t, dir, "empty", nil)
	onlyNL := writeSecret(t, dir, "onlynl", []byte("\n"))
	nul := writeSecret(t, dir, "nul", []byte("a\x00b"))
	big := writeSecret(t, dir, "big", bytes.Repeat([]byte("a"), maxSecretBytes+1))
	sub := filepath.Join(dir, "sub")
	if err := os.Mkdir(sub, 0o750); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "outside")
	if err := os.WriteFile(outside, []byte("x"), 0o440); err != nil {
		t.Fatal(err)
	}
	escape := filepath.Join(dir, "escape")
	if err := os.Symlink(outside, escape); err != nil {
		t.Fatal(err)
	}
	inside := filepath.Join(dir, "inside")
	if err := os.Symlink(good, inside); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name string
		env  []string
		want string
	}{
		{"both set", []string{"COMMERCE_X=1", "COMMERCE_X_FILE=" + good}, "both set"},
		{"relative", []string{"COMMERCE_X_FILE=run/secrets/x"}, "absolute"},
		{"outside root", []string{"COMMERCE_X_FILE=" + outside}, "under"},
		{"dotdot", []string{"COMMERCE_X_FILE=" + dir + "/../x"}, "under"},
		{"missing", []string{"COMMERCE_X_FILE=" + filepath.Join(dir, "nope")}, "not readable"},
		{"directory", []string{"COMMERCE_X_FILE=" + sub}, "regular"},
		{"empty", []string{"COMMERCE_X_FILE=" + empty}, "size"},
		{"only newline", []string{"COMMERCE_X_FILE=" + onlyNL}, "empty"},
		{"oversize", []string{"COMMERCE_X_FILE=" + big}, "size"},
		{"nul", []string{"COMMERCE_X_FILE=" + nul}, "NUL"},
		{"symlink escape", []string{"COMMERCE_X_FILE=" + escape}, "outside"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := expandFileEnv(tc.env)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want containing %q", err, tc.want)
			}
			if strings.Contains(err.Error(), "a\x00b") {
				t.Fatal("error leaks content")
			}
		})
	}
	env, err := expandFileEnv([]string{"COMMERCE_X_FILE=" + inside})
	if err != nil || !slices.Equal(env, []string{"COMMERCE_X=x"}) {
		t.Fatalf("symlink inside root: env=%q err=%v", env, err)
	}
}

func TestSecretsRootMissing(t *testing.T) {
	dir := withRoot(t)
	p := writeSecret(t, dir, "s", []byte("x"))
	secretsRoot = filepath.Join(dir, "gone")
	if _, err := readSecretFile(filepath.Join(secretsRoot, "s")); err == nil {
		t.Fatal("expected error for missing file under missing root")
	}
	_ = p
}

type execRecord struct {
	argv0 string
	argv  []string
	env   []string
	err   error
	calls int
}

func (r *execRecord) exec(argv0 string, argv, env []string) error {
	r.calls++
	r.argv0, r.argv, r.env = argv0, argv, env
	return r.err
}

func TestRealMainRun(t *testing.T) {
	dir := withRoot(t)
	s := writeSecret(t, dir, "k", []byte("v\n"))
	var out bytes.Buffer
	rec := &execRecord{}
	code := realMain([]string{"run", "--", "/app/bin/api", "-x"}, []string{"COMMERCE_K_FILE=" + s}, rec.exec, &out)
	if code != exitOK || rec.calls != 1 || rec.argv0 != "/app/bin/api" || !slices.Equal(rec.argv, []string{"/app/bin/api", "-x"}) || !slices.Equal(rec.env, []string{"COMMERCE_K=v"}) {
		t.Fatalf("code=%d rec=%+v out=%q", code, rec, out.String())
	}
	rec = &execRecord{err: errors.New("boom")}
	if code := realMain([]string{"run", "--", "/nope"}, nil, rec.exec, &out); code != exitExec {
		t.Fatalf("exec failure code=%d", code)
	}
	for _, args := range [][]string{{}, {"bogus"}, {"run"}, {"run", "/app/bin/api"}, {"run", "--", "relative"}} {
		out.Reset()
		if code := realMain(args, nil, (&execRecord{}).exec, &out); code != exitConfig || out.Len() == 0 {
			t.Fatalf("args %q: code=%d out=%q", args, code, out.String())
		}
	}
	out.Reset()
	code = realMain([]string{"run", "--", "/app/bin/api"}, []string{"COMMERCE_K=1", "COMMERCE_K_FILE=" + s}, (&execRecord{}).exec, &out)
	if code != exitConfig || strings.Contains(out.String(), "v") && strings.Contains(out.String(), "=v") {
		t.Fatalf("conflict code=%d out=%q", code, out.String())
	}
}

func TestProbe(t *testing.T) {
	ok := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok")) }))
	defer ok.Close()
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusServiceUnavailable) }))
	defer bad.Close()
	redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "http://example.invalid/", http.StatusFound)
	}))
	defer redirect.Close()
	notFound := httptest.NewServer(http.NotFoundHandler())
	defer notFound.Close()
	closed := httptest.NewServer(http.NotFoundHandler())
	closedURL := closed.URL
	closed.Close()

	cases := []struct {
		args []string
		want int
		msg  string
	}{
		{[]string{ok.URL + "/readyz"}, exitOK, ""},
		{[]string{bad.URL + "/readyz"}, exitProbe, "status 503"},
		{[]string{redirect.URL + "/"}, exitOK, ""},
		{[]string{"-max-status", "299", redirect.URL + "/"}, exitProbe, "status 302"},
		{[]string{notFound.URL + "/"}, exitProbe, "status 404"},
		{[]string{"-max-status", "499", notFound.URL + "/"}, exitOK, ""},
		{[]string{"-timeout", "1s", closedURL + "/"}, exitProbe, "dial error"},
		{[]string{"http://10.0.0.1:8080/"}, exitConfig, "loopback"},
		{[]string{"http://localhost:8080/"}, exitConfig, "loopback"},
		{[]string{"https://127.0.0.1:8080/"}, exitConfig, "http://"},
		{[]string{"http://127.0.0.1/"}, exitConfig, "port"},
		{[]string{"http://user@127.0.0.1:1/"}, exitConfig, "http://"},
		{[]string{"::bad"}, exitConfig, "http://"},
		{[]string{}, exitConfig, "usage"},
		{[]string{"-nope", ok.URL}, exitConfig, "usage"},
		{[]string{"-max-status", "700", ok.URL}, exitConfig, "invalid flag"},
		{[]string{"-timeout", "0s", ok.URL}, exitConfig, "invalid flag"},
	}
	for _, tc := range cases {
		var out bytes.Buffer
		code := realMain(append([]string{"probe"}, tc.args...), nil, nil, &out)
		if code != tc.want || !strings.Contains(out.String(), tc.msg) {
			t.Errorf("probe %q: code=%d out=%q, want %d %q", tc.args, code, out.String(), tc.want, tc.msg)
		}
	}
	if u, err := loopbackURL("http://[::1]:8080/x"); err != nil || u != "http://[::1]:8080/x" {
		t.Fatalf("ipv6 loopback: %q %v", u, err)
	}
}
