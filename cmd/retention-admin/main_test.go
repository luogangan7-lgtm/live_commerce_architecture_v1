package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"livecommerce/internal/integrations/meta"
	"livecommerce/internal/platform"
	"livecommerce/internal/retention"
)

// Unit tests of the CLI: flag and stdin parsing tables, exit-code mapping, DSN admission rules
// and the "no identifier in any output" rule. The stubs replace the database seams; nothing here
// opens a connection.

const (
	senderSentinel  = "918273645"
	commentSentinel = "918273645_5544332211"
	dsnSentinel     = "dsn-sentinel-7f3a91c2"
	reqA            = "0f0e0d0c-0b0a-4908-8706-050403020100"
	tenantA         = "11111111-1111-4111-8111-111111111111"
	storeA          = "22222222-2222-4222-8222-222222222222"
	bundleA         = "33333333-3333-4333-8333-333333333333"
)

func actorEnv() string {
	raw := make([]byte, 32)
	for i := range raw {
		raw[i] = byte(i + 1)
	}
	return base64.StdEncoding.EncodeToString(raw)
}

func operatorDSN() string {
	u := url.URL{Scheme: "postgres", User: url.UserPassword("lc_op", dsnSentinel), Host: "synthetic.invalid", Path: "/db"}
	return u.String()
}

type harness struct {
	env             map[string]string
	stdout, stderr  bytes.Buffer
	opened          []string
	erased          []retention.Selector
	heldAt          time.Time
	eraseErr, stErr error
}

func newHarness(t *testing.T) *harness {
	h := &harness{env: map[string]string{}}
	openOperator = func(context.Context, string) (*pgxpool.Pool, error) {
		h.opened = append(h.opened, "operator")
		return nil, nil
	}
	openJob = func(context.Context, string) (*pgxpool.Pool, error) {
		h.opened = append(h.opened, "job")
		return nil, nil
	}
	doErase = func(_ context.Context, _ *pgxpool.Pool, s retention.Selector) (retention.Counts, retention.Held, error) {
		h.erased = append(h.erased, s)
		if h.eraseErr != nil {
			return nil, retention.Held{}, h.eraseErr
		}
		if !h.heldAt.IsZero() {
			return nil, retention.Held{RetryAfter: h.heldAt}, nil
		}
		return retention.Counts{"links": 2, "bundles": 1}, retention.Held{}, nil
	}
	doStatus = func(context.Context, *pgxpool.Pool) (retention.Status, error) {
		return retention.Status{Policy: retention.Policy{Enforced: true, LinkDays: 7, IntakeDays: 30, ClaimsDays: 90, SocialDays: 30},
			Version: 4, LastRunUnix: 1790000000, LastRunMore: false}, h.stErr
	}
	doRunOnce = func(context.Context, *pgxpool.Pool, int) (retention.Counts, error) {
		return retention.Counts{"busy": 1}, nil
	}
	doSetPolicy = func(_ context.Context, _ *pgxpool.Pool, v int64, _ retention.Policy) (int64, error) {
		return v + 1, nil
	}
	doReplay = func(_ context.Context, _ *pgxpool.Pool, ts []retention.Tombstone) (int64, error) {
		return int64(len(ts)), nil
	}
	t.Cleanup(func() {
		openOperator, openJob = platform.OpenRetentionOperatorPool, platform.OpenRetentionJobPool
		doStatus, doSetPolicy, doRunOnce, doErase, doReplay = retention.GetStatus, retention.SetPolicy, retention.RunOnce, retention.Erase, retention.Replay
	})
	return h
}

func (h *harness) do(args []string, stdin string) int {
	h.stdout.Reset()
	h.stderr.Reset()
	return run(context.Background(), args, strings.NewReader(stdin), &h.stdout, &h.stderr, func(k string) string { return h.env[k] })
}

func (h *harness) assertNoSecrets(t *testing.T, label string) {
	t.Helper()
	all := h.stdout.String() + h.stderr.String()
	for _, s := range []string{senderSentinel, commentSentinel, dsnSentinel, actorEnv()} {
		if strings.Contains(all, s) {
			t.Errorf("%s: output leaked %q: %q", label, s, all)
		}
	}
}

func TestEraseUsageTable(t *testing.T) {
	h := newHarness(t)
	h.env[envOperatorDSN] = operatorDSN()
	h.env["COMMERCE_CLAIMS_ACTOR_KEY"] = actorEnv()
	base := []string{"erase", "--request", reqA, "--object", "page", "--asset", "55"}
	sender := `{"sender_id":"` + senderSentinel + `"}`
	comment := `{"comment_ref":"` + commentSentinel + `"}`
	with := func(extra ...string) []string { return append(append([]string{}, base...), extra...) }
	bundle := []string{"erase", "--request", reqA, "--tenant", tenantA, "--store", storeA, "--bundle", bundleA}
	cases := []struct {
		name  string
		args  []string
		stdin string
		want  int
	}{
		{"sender ok", base, sender, 0},
		{"sender with apps", with("--app", "111", "--app", "222"), sender, 0},
		{"comment ok", base, comment, 0},
		{"bundle ok, empty stdin", bundle, "", 0},
		{"bundle ok, blank stdin", bundle, " \n", 0},
		{"bundle with stdin", bundle, sender, 2},
		{"no stdin without bundle", base, "", 2},
		{"both fields", base, `{"sender_id":"1","comment_ref":"1_2"}`, 2},
		{"unknown field", base, `{"sender_id":"1","extra":1}`, 2},
		{"trailing object", base, sender + `{"sender_id":"2"}`, 2},
		{"trailing garbage", base, sender + ` x`, 2},
		{"null sender", base, `{"sender_id":null}`, 2},
		{"array", base, `[1]`, 2},
		{"sender not digits", base, `{"sender_id":"12a"}`, 2},
		{"sender too long", base, `{"sender_id":"` + strings.Repeat("1", 33) + `"}`, 2},
		{"comment bad chars", base, `{"comment_ref":"1-2"}`, 2},
		{"oversize stdin", base, `{"sender_id":"1"}` + strings.Repeat(" ", 5000), 2},
		{"bad request", []string{"erase", "--request", "nope", "--object", "page", "--asset", "55"}, sender, 2},
		{"no request", []string{"erase", "--object", "page", "--asset", "55"}, sender, 2},
		{"bad object", []string{"erase", "--request", reqA, "--object", "facebook", "--asset", "55"}, sender, 2},
		{"bad asset", []string{"erase", "--request", reqA, "--object", "page", "--asset", "5x"}, sender, 2},
		{"asset too long", []string{"erase", "--request", reqA, "--object", "page", "--asset", strings.Repeat("5", 33)}, sender, 2},
		{"nine apps", with(strings.Fields("--app 1 --app 2 --app 3 --app 4 --app 5 --app 6 --app 7 --app 8 --app 9")...), sender, 2},
		{"bad app", with("--app", "x"), sender, 2},
		{"app with comment", with("--app", "1"), comment, 2},
		{"tenant without bundle", with("--tenant", tenantA), sender, 2},
		{"bundle with object", append(append([]string{}, bundle...), "--object", "page"), "", 2},
		{"bundle without tenant", []string{"erase", "--request", reqA, "--store", storeA, "--bundle", bundleA}, "", 2},
		{"unknown flag", with("--sender", senderSentinel), sender, 2},
		{"positional", with("extra"), sender, 2},
		{"sender id in argv is not a flag", with("--sender-id", senderSentinel), "", 2},
	}
	for _, c := range cases {
		h.erased = nil
		if got := h.do(c.args, c.stdin); got != c.want {
			t.Errorf("%s: exit %d want %d (stderr %q)", c.name, got, c.want, h.stderr.String())
		}
		h.assertNoSecrets(t, c.name)
		if c.want == 2 && len(h.erased) != 0 {
			t.Errorf("%s: erase ran on a usage error", c.name)
		}
	}
}

func TestEraseDerivesKeysAndKeepsTheSenderOutOfTheSelector(t *testing.T) {
	h := newHarness(t)
	h.env[envOperatorDSN] = operatorDSN()
	h.env["COMMERCE_CLAIMS_ACTOR_KEY"] = actorEnv()
	if code := h.do([]string{"erase", "--request", reqA, "--object", "page", "--asset", "55", "--app", "111", "--app", "222"}, `{"sender_id":"`+senderSentinel+`"}`); code != 0 {
		t.Fatalf("exit %d %q", code, h.stderr.String())
	}
	if len(h.erased) != 1 {
		t.Fatal("erase not called once")
	}
	s := h.erased[0]
	k, ok, err := meta.LoadClaimsActorKey(func(string) string { return actorEnv() })
	if !ok || err != nil {
		t.Fatal("key")
	}
	if s.ActorKey != meta.ClaimActorKey(k, "page", "55", senderSentinel) {
		t.Fatal("actor key is not meta.ClaimActorKey")
	}
	if len(s.PeerKeys) != 2 || s.PeerKeys[0] != meta.SocialPeerKey("111", "page", "55", senderSentinel) || s.PeerKeys[1] != meta.SocialPeerKey("222", "page", "55", senderSentinel) {
		t.Fatal("peer keys are not meta.SocialPeerKey per --app")
	}
	if s.CommentRef != "" || s.Bundle != "" || s.Request != reqA {
		t.Fatalf("selector %+v", s)
	}
	out := h.stdout.String()
	if !strings.HasPrefix(out, "request="+reqA+"\n") || !strings.Contains(out, "bundles=1\n") || !strings.Contains(out, "links=2\n") {
		t.Fatalf("stdout %q", out)
	}
	if strings.Index(out, "bundles=1") > strings.Index(out, "links=2") {
		t.Fatalf("counts not sorted: %q", out)
	}
	h.assertNoSecrets(t, "erase")
}

func TestEraseSenderNeedsTheActorKey(t *testing.T) {
	h := newHarness(t)
	h.env[envOperatorDSN] = operatorDSN()
	args := []string{"erase", "--request", reqA, "--object", "page", "--asset", "55"}
	if code := h.do(args, `{"sender_id":"1"}`); code != 1 || strings.TrimSpace(h.stderr.String()) != codeConfig {
		t.Fatalf("missing key: exit %d %q", code, h.stderr.String())
	}
	h.env["COMMERCE_CLAIMS_ACTOR_KEY"] = "not-base64"
	if code := h.do(args, `{"sender_id":"1"}`); code != 1 || strings.TrimSpace(h.stderr.String()) != codeConfig {
		t.Fatalf("bad key: exit %d %q", code, h.stderr.String())
	}
	// The comment and bundle selectors never need it.
	if code := h.do(args, `{"comment_ref":"1_2"}`); code != 0 {
		t.Fatalf("comment selector: exit %d", code)
	}
	h.assertNoSecrets(t, "actor key")
}

func TestExitCodes(t *testing.T) {
	h := newHarness(t)
	h.env[envOperatorDSN] = operatorDSN()
	args := []string{"erase", "--request", reqA, "--tenant", tenantA, "--store", storeA, "--bundle", bundleA}
	cases := []struct {
		err  error
		code int
		msg  string
	}{
		{retention.ErrUsage, 2, codeUsage}, {retention.ErrNotFound, 4, codeNotFound}, {retention.ErrConflict, 5, codeConflict},
		{retention.ErrBusy, 5, codeBusy}, {errors.New("driver text " + dsnSentinel), 1, codeFailed},
	}
	for _, c := range cases {
		h.eraseErr = c.err
		if got := h.do(args, ""); got != c.code || strings.TrimSpace(h.stderr.String()) != c.msg {
			t.Errorf("%v: exit %d stderr %q, want %d %s", c.err, got, h.stderr.String(), c.code, c.msg)
		}
		h.assertNoSecrets(t, c.msg)
	}
	h.eraseErr = nil
	h.heldAt = time.Date(2026, 10, 8, 3, 4, 5, 0, time.UTC)
	if got := h.do(args, ""); got != 3 || strings.TrimSpace(h.stderr.String()) != codeHeld {
		t.Fatalf("held: exit %d stderr %q", got, h.stderr.String())
	}
	if want := "request=" + reqA + "\nretry_after=2026-10-08T03:04:05Z\n"; h.stdout.String() != want {
		t.Fatalf("held stdout %q", h.stdout.String())
	}
}

func TestDSNAdmission(t *testing.T) {
	h := newHarness(t)
	jobOnly := map[string]string{envJobDSN: operatorDSN()}
	for _, args := range [][]string{
		{"policy-set", "--expected-version", "1", "--enforced=true", "--link-days", "7", "--intake-days", "30", "--claims-days", "90", "--social-days", "30"},
		{"run"}, {"replay"}, {"erase", "--request", reqA, "--tenant", tenantA, "--store", storeA, "--bundle", bundleA},
	} {
		h.env = map[string]string{envJobDSN: jobOnly[envJobDSN]}
		h.opened = nil
		if got := h.do(args, ""); got != 2 || len(h.opened) != 0 {
			t.Errorf("%s with only the job DSN: exit %d opened %v, want 2 and no connection", args[0], got, h.opened)
		}
		h.assertNoSecrets(t, args[0])
	}
	// status runs on the job DSN alone, and on the operator DSN when both are set.
	h.env = map[string]string{envJobDSN: operatorDSN()}
	h.opened = nil
	if got := h.do([]string{"status"}, ""); got != 0 || len(h.opened) != 1 || h.opened[0] != "job" {
		t.Fatalf("status on job DSN: exit %d opened %v", got, h.opened)
	}
	want := "enforced=1\nversion=4\nlink_days=7\nintake_days=30\nclaims_days=90\nsocial_days=30\nlast_run_unix=1790000000\nlast_run_more=0\n"
	if h.stdout.String() != want {
		t.Fatalf("status stdout %q", h.stdout.String())
	}
	h.env[envOperatorDSN] = operatorDSN()
	h.opened = nil
	if got := h.do([]string{"status"}, ""); got != 0 || len(h.opened) != 1 || h.opened[0] != "operator" {
		t.Fatalf("status prefers operator: exit %d opened %v", got, h.opened)
	}
	// No DSN at all is a config error (exit 1), not usage.
	h.env = map[string]string{}
	if got := h.do([]string{"status"}, ""); got != 1 || strings.TrimSpace(h.stderr.String()) != codeConfig {
		t.Fatalf("no DSN: exit %d %q", got, h.stderr.String())
	}
	// An Open failure is one fixed code; the pool error text (which can echo a DSN) never surfaces.
	openOperator = func(context.Context, string) (*pgxpool.Pool, error) { return nil, errors.New("dial " + dsnSentinel) }
	h.env = map[string]string{envOperatorDSN: operatorDSN()}
	if got := h.do([]string{"status"}, ""); got != 1 || strings.TrimSpace(h.stderr.String()) != codeDatabase {
		t.Fatalf("open failure: exit %d %q", got, h.stderr.String())
	}
	h.assertNoSecrets(t, "open failure")
}

func TestPolicySetTable(t *testing.T) {
	h := newHarness(t)
	h.env[envOperatorDSN] = operatorDSN()
	full := []string{"policy-set", "--expected-version", "3", "--enforced=true", "--link-days", "7", "--intake-days", "30", "--claims-days", "90", "--social-days", "30"}
	if code := h.do(full, ""); code != 0 || h.stdout.String() != "version=4\n" {
		t.Fatalf("ok: exit %d %q", code, h.stdout.String())
	}
	swap := func(flag, value string) []string {
		out := append([]string{}, full...)
		for i := range out {
			if out[i] == flag {
				out[i+1] = value
			}
		}
		return out
	}
	bad := map[string][]string{
		"missing flag":     full[:len(full)-2],
		"version zero":     swap("--expected-version", "0"),
		"link 0":           swap("--link-days", "0"),
		"link 366":         swap("--link-days", "366"),
		"intake 7":         swap("--intake-days", "7"),
		"claims 7":         swap("--claims-days", "7"),
		"social 7":         swap("--social-days", "7"),
		"social > intake":  swap("--social-days", "31"),
		"not a number":     swap("--link-days", "x"),
		"positional":       append(append([]string{}, full...), "x"),
		"enforced garbage": {"policy-set", "--expected-version", "3", "--enforced=maybe", "--link-days", "7", "--intake-days", "30", "--claims-days", "90", "--social-days", "30"},
	}
	for name, args := range bad {
		if code := h.do(args, ""); code != 2 {
			t.Errorf("%s: exit %d want 2", name, code)
		}
	}
}

func TestRunAndUnknownSubcommand(t *testing.T) {
	h := newHarness(t)
	h.env[envOperatorDSN] = operatorDSN()
	if code := h.do([]string{"run"}, ""); code != 0 || h.stdout.String() != "busy=1\n" {
		t.Fatalf("run: exit %d %q", code, h.stdout.String())
	}
	for _, args := range [][]string{{}, {"nope"}, {"run", "--limit", "0"}, {"run", "--limit", "1001"}, {"run", "extra"}, {"status", "extra"}} {
		if code := h.do(args, ""); code != 2 {
			t.Errorf("%v: exit %d want 2", args, code)
		}
	}
}

func TestParseTombstones(t *testing.T) {
	d64 := strings.Repeat("ab", 32)
	actor := `{"request_id":"` + reqA + `","selector_digest":"` + d64 + `","actor_digest":"` + d64 + `","bundle_tenant":null,"bundle_store":null,"bundle_ref":null}`
	bundle := `{"request_id":"` + reqA + `","selector_digest":"` + d64 + `","actor_digest":null,"bundle_tenant":"` + tenantA + `","bundle_store":"` + storeA + `","bundle_ref":"` + bundleA + `"}`
	ts, err := parseTombstones([]byte("[" + actor + "," + bundle + "]"))
	if err != nil || len(ts) != 2 || len(ts[0].ActorDigest) != 32 || ts[0].BundleRef != "" || ts[1].BundleRef != bundleA || ts[1].ActorDigest != nil {
		t.Fatalf("good: %+v %v", ts, err)
	}
	if ts, err := parseTombstones([]byte("[]")); err != nil || ts == nil || len(ts) != 0 {
		t.Fatalf("empty array is a valid empty list: %v %v", ts, err)
	}
	bad := map[string]string{
		"not array":        actor,
		"null":             `null`,
		"trailing":         "[" + actor + "] x",
		"unknown key":      strings.Replace(actor, `"bundle_ref":null`, `"bundle_ref":null,"x":1`, 1),
		"missing key":      strings.Replace(actor, `,"bundle_ref":null`, ``, 1),
		"renamed key":      strings.Replace(actor, `"bundle_ref"`, `"bundle_x"`, 1),
		"both handles":     strings.Replace(bundle, `"actor_digest":null`, `"actor_digest":"`+d64+`"`, 1),
		"no handle":        strings.Replace(actor, `"actor_digest":"`+d64+`"`, `"actor_digest":null`, 1),
		"partial triple":   strings.Replace(bundle, `"bundle_store":"`+storeA+`"`, `"bundle_store":null`, 1),
		"short digest":     strings.Replace(actor, `"selector_digest":"`+d64+`"`, `"selector_digest":"abcd"`, 1),
		"upper digest":     strings.Replace(actor, `"selector_digest":"`+d64+`"`, `"selector_digest":"`+strings.ToUpper(d64)+`"`, 1),
		"bad request":      strings.Replace(actor, reqA, "nope", 1),
		"bad bundle uuid":  strings.Replace(bundle, bundleA, "nope", 1),
		"string not array": `"x"`,
	}
	for name, raw := range bad {
		if _, err := parseTombstones([]byte(raw)); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
	var many strings.Builder
	many.WriteByte('[')
	for i := 0; i < maxTombstones+1; i++ {
		if i > 0 {
			many.WriteByte(',')
		}
		many.WriteString(actor)
	}
	many.WriteByte(']')
	if _, err := parseTombstones([]byte(many.String())); err == nil {
		t.Error("more than 10000 tombstones accepted")
	}
}

func TestReplayFileHandling(t *testing.T) {
	h := newHarness(t)
	h.env[envOperatorDSN] = operatorDSN()
	if code := h.do([]string{"replay"}, ""); code != 0 || h.stdout.String() != "tombstones=0\n" {
		t.Fatalf("replay log: exit %d %q", code, h.stdout.String())
	}
	dir := t.TempDir()
	d64 := strings.Repeat("cd", 32)
	good := filepath.Join(dir, "good.json")
	if err := os.WriteFile(good, []byte(`[{"request_id":"`+reqA+`","selector_digest":"`+d64+`","actor_digest":"`+d64+`","bundle_tenant":null,"bundle_store":null,"bundle_ref":null}]`), 0o600); err != nil {
		t.Fatal(err)
	}
	if code := h.do([]string{"replay", "--tombstones-file", good}, ""); code != 0 || h.stdout.String() != "tombstones=1\n" {
		t.Fatalf("replay file: exit %d %q", code, h.stdout.String())
	}
	huge := filepath.Join(dir, "huge.json")
	if err := os.WriteFile(huge, append([]byte("["), bytes.Repeat([]byte(" "), maxFile+1)...), 0o600); err != nil {
		t.Fatal(err)
	}
	for name, path := range map[string]string{"missing": filepath.Join(dir, "nope.json"), "huge": huge} {
		if code := h.do([]string{"replay", "--tombstones-file", path}, ""); code != 2 {
			t.Errorf("%s file: exit %d want 2", name, code)
		}
		if strings.Contains(h.stderr.String(), dir) {
			t.Errorf("%s: stderr echoes the path", name)
		}
	}
}

func TestWriteFailureIsFixed(t *testing.T) {
	h := newHarness(t)
	h.env[envOperatorDSN] = operatorDSN()
	var stderr bytes.Buffer
	code := run(context.Background(), []string{"status"}, strings.NewReader(""), failWriter{}, &stderr, func(k string) string { return h.env[k] })
	if code != 1 || strings.TrimSpace(stderr.String()) != codeOutput {
		t.Fatalf("exit %d %q", code, stderr.String())
	}
}

type failWriter struct{}

func (failWriter) Write([]byte) (int, error) { return 0, io.ErrClosedPipe }
