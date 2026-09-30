// admin_gate_test.go is the independent CRP01 gate for cmd/retention-admin (U08, contract
// claims-retention-purge-v1 §5 + §7 row CRP01). Written from the contract §5 and the FROZEN block /
// defaults D6-D8 of docs/delivery/units/retention-core.md; the only implementation names used are
// the frozen test seam `run` and, isolated in crpSeams, the package variables that stand in for the
// database (so this gate needs no PG: evidence UNIT). Exit codes, stdout shape and stderr codes of
// a real database run are asserted again against a real binary in tests/foundation CRP06/CRP07.
package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"livecommerce/internal/platform"
	"livecommerce/internal/retention"
)

const (
	crpSender   = "918273645"
	crpComment  = "918273645_5544332211"
	crpRequest  = "0f0e0d0c-0b0a-4908-8706-050403020100"
	crpTenant   = "11111111-1111-4111-8111-111111111111"
	crpStore    = "22222222-2222-4222-8222-222222222222"
	crpBundle   = "33333333-3333-4333-8333-333333333333"
	crpDSNToken = "dsn-sentinel-crp01-cli-9b31"
	crpErrText  = "driver-text-sentinel-crp01-c77e"
	// Vectors computed once with python3 for K = bytes 0x01..0x20 (see internal/retention gate).
	crpKeyB64      = "AQIDBAUGBwgJCgsMDQ4PEBESExQVFhcYGRobHB0eHyA="
	crpActorPage   = "1d9b4d74a02d47ed6993ac8404c10fb01104dfa958ab2eb57c6b504760093ec3"
	crpActorIG     = "0051dceed4899959a25937f6235f28e1dfae073b4c751d71288b6b7ec4e1ce6c"
	crpPeerPage    = "275ab0ba43c50b2c173997c19a21bfb3f3ac8d7e24d1cc4bb58476b153471364" // app 123456789012345
	crpPeerPage111 = "971254134e46a8c604815419263750d35ddcaa675ae30489b501f8a44d702e00" // app 111
)

func crpDSN() string {
	u := url.URL{Scheme: "postgres", User: url.UserPassword("lc_retention_probe", crpDSNToken), Host: "127.0.0.1:1", Path: "/probe"}
	return u.String()
}

// crpRig replaces the database seams and records every call. Nothing opens a connection.
type crpRig struct {
	env            map[string]string
	opened         []string
	erased         []retention.Selector
	replayed       [][]retention.Tombstone
	policies       int
	runs           int
	eraseErr       error
	held           time.Time
	eraseCounts    retention.Counts
	statusOut      retention.Status
	stdout, stderr bytes.Buffer
}

func crpSeams(t *testing.T) *crpRig {
	t.Helper()
	r := &crpRig{env: map[string]string{}, eraseCounts: retention.Counts{"links": 2, "bundles": 1, "lines": 3}}
	openOperator = func(context.Context, string) (*pgxpool.Pool, error) {
		r.opened = append(r.opened, "operator")
		return nil, nil
	}
	openJob = func(context.Context, string) (*pgxpool.Pool, error) {
		r.opened = append(r.opened, "job")
		return nil, nil
	}
	doErase = func(_ context.Context, _ *pgxpool.Pool, s retention.Selector) (retention.Counts, retention.Held, error) {
		r.erased = append(r.erased, s)
		if r.eraseErr != nil {
			return nil, retention.Held{}, r.eraseErr
		}
		if !r.held.IsZero() {
			return nil, retention.Held{RetryAfter: r.held}, nil
		}
		return r.eraseCounts, retention.Held{}, nil
	}
	doStatus = func(context.Context, *pgxpool.Pool) (retention.Status, error) { return r.statusOut, nil }
	doRunOnce = func(context.Context, *pgxpool.Pool, int) (retention.Counts, error) {
		r.runs++
		return retention.Counts{"enforced": 1, "links": 4, "more": 0}, nil
	}
	doSetPolicy = func(_ context.Context, _ *pgxpool.Pool, v int64, _ retention.Policy) (int64, error) {
		r.policies++
		return v + 1, nil
	}
	doReplay = func(_ context.Context, _ *pgxpool.Pool, ts []retention.Tombstone) (int64, error) {
		r.replayed = append(r.replayed, ts)
		return int64(len(ts)), nil
	}
	t.Cleanup(func() {
		openOperator, openJob = platform.OpenRetentionOperatorPool, platform.OpenRetentionJobPool
		doStatus, doSetPolicy, doRunOnce, doErase, doReplay = retention.GetStatus, retention.SetPolicy, retention.RunOnce, retention.Erase, retention.Replay
	})
	r.statusOut = retention.Status{Policy: retention.Policy{Enforced: true, LinkDays: 7, IntakeDays: 30, ClaimsDays: 90, SocialDays: 30}, Version: 4, LastRunUnix: 1790000000}
	return r
}

func (r *crpRig) do(args []string, stdin string) int {
	r.stdout.Reset()
	r.stderr.Reset()
	return run(context.Background(), args, strings.NewReader(stdin), &r.stdout, &r.stderr, func(k string) string { return r.env[k] })
}

func (r *crpRig) withOperator() *crpRig {
	r.env["COMMERCE_RETENTION_OPERATOR_DATABASE_URL"] = crpDSN()
	return r
}

func (r *crpRig) withKey() *crpRig {
	r.env["COMMERCE_CLAIMS_ACTOR_KEY"] = crpKeyB64
	return r
}

func (r *crpRig) assertClean(t *testing.T, label string) {
	t.Helper()
	all := r.stdout.String() + r.stderr.String()
	for _, s := range []string{crpSender, crpComment, crpDSNToken, crpErrText, crpKeyB64, crpActorPage, crpActorIG, crpPeerPage, crpPeerPage111} {
		if strings.Contains(all, s) {
			t.Errorf("%s: output leaks %q: %q", label, s, all)
		}
	}
	// Selector (c) ids are the operator's own arguments; the CLI still never echoes tenant/store ids.
	for _, s := range []string{crpTenant, crpStore, crpBundle} {
		if strings.Contains(all, s) {
			t.Errorf("%s: output echoes an id %q: %q", label, s, all)
		}
	}
}

// crpUsage asserts exit 2, the one fixed stderr code, empty stdout, no connection and no definer call.
func (r *crpRig) crpUsage(t *testing.T, label string, args []string, stdin string) {
	t.Helper()
	before := len(r.opened) + len(r.erased) + len(r.replayed) + r.policies + r.runs
	code := r.do(args, stdin)
	if code != 2 || strings.TrimSpace(r.stderr.String()) != "retention_admin_usage" || r.stdout.Len() != 0 {
		t.Errorf("%s: exit=%d stderr=%q stdout=%q, want 2 / retention_admin_usage / empty", label, code, r.stderr.String(), r.stdout.String())
	}
	if after := len(r.opened) + len(r.erased) + len(r.replayed) + r.policies + r.runs; after != before {
		t.Errorf("%s: a usage error still reached the database seam", label)
	}
	r.assertClean(t, label)
}

func crpEraseArgs(extra ...string) []string {
	return append([]string{"erase", "--request", crpRequest}, extra...)
}

// CRP01 row "selector parsing (exactly one; stdin strict JSON; argv never carries sender/comment id)":
// every malformed stdin is usage (exit 2) and touches nothing; exactly-one is enforced across the
// three selectors; no flag accepts a sender id, comment id or key.
func TestClaimsRetentionCRP01CLIStdinAndSelectors(t *testing.T) {
	r := crpSeams(t).withOperator().withKey()
	meta := []string{"--object", "page", "--asset", "5550001"}
	bundle := []string{"--tenant", crpTenant, "--store", crpStore, "--bundle", crpBundle}
	sender := `{"sender_id":"` + crpSender + `"}`
	comment := `{"comment_ref":"` + crpComment + `"}`
	// strict stdin
	for name, stdin := range map[string]string{
		"unknown key":                  `{"sender_id":"` + crpSender + `","extra":1}`,
		"unknown key alone":            `{"actor_key":"` + crpSender + `"}`,
		"both keys":                    `{"sender_id":"` + crpSender + `","comment_ref":"` + crpComment + `"}`,
		"trailing object":              sender + ` {}`,
		"trailing garbage":             sender + `x`,
		"trailing second selector":     sender + "\n" + comment,
		"empty without --bundle":       ``,
		"whitespace only w/o --bundle": " \n\t ",
		"json null":                    `null`,
		"json array":                   `[` + sender + `]`,
		"json string":                  `"` + crpSender + `"`,
		"sender is a number":           `{"sender_id":918273645}`,
		"sender null":                  `{"sender_id":null}`,
		"empty sender":                 `{"sender_id":""}`,
		"sender with letters":          `{"sender_id":"91827a645"}`,
		"sender 33 digits":             `{"sender_id":"` + strings.Repeat("9", 33) + `"}`,
		"sender with spaces":           `{"sender_id":" 918273645"}`,
		"comment with letters":         `{"comment_ref":"abc"}`,
		"comment 81 chars":             `{"comment_ref":"` + strings.Repeat("1", 81) + `"}`,
		"comment empty":                `{"comment_ref":""}`,
		"comment with dash":            `{"comment_ref":"12-34"}`,
		"truncated json":               `{"sender_id":"` + crpSender,
		"stdin over 4 KiB":             `{"sender_id":"` + crpSender + `"}` + strings.Repeat(" ", 4096),
		"stdin far over 4 KiB":         strings.Repeat("x", 1<<20),
	} {
		r.crpUsage(t, "stdin "+name, crpEraseArgs(meta...), stdin)
	}
	// exactly one selector: the (a) meta selector needs stdin, (c) needs none; mixes are refused.
	r.crpUsage(t, "bundle plus sender stdin (a+c)", crpEraseArgs(bundle...), sender)
	r.crpUsage(t, "bundle plus comment stdin (b+c)", crpEraseArgs(bundle...), comment)
	r.crpUsage(t, "object/asset plus bundle args", crpEraseArgs(append(append([]string{}, meta...), bundle...)...), sender)
	r.crpUsage(t, "no selector at all", crpEraseArgs(), "")
	r.crpUsage(t, "no request id", []string{"erase", "--object", "page", "--asset", "5550001"}, sender)
	r.crpUsage(t, "request not a uuid", []string{"erase", "--request", "42", "--object", "page", "--asset", "5550001"}, sender)
	r.crpUsage(t, "bundle without tenant", crpEraseArgs("--store", crpStore, "--bundle", crpBundle), "")
	r.crpUsage(t, "bundle without store", crpEraseArgs("--tenant", crpTenant, "--bundle", crpBundle), "")
	r.crpUsage(t, "bundle not a uuid", crpEraseArgs("--tenant", crpTenant, "--store", crpStore, "--bundle", "7"), "")
	r.crpUsage(t, "positional argument", crpEraseArgs(append(append([]string{}, meta...), "extra")...), sender)
	// D8 limits
	r.crpUsage(t, "object twitter", crpEraseArgs("--object", "twitter", "--asset", "5550001"), sender)
	r.crpUsage(t, "object missing", crpEraseArgs("--asset", "5550001"), sender)
	r.crpUsage(t, "asset not digits", crpEraseArgs("--object", "page", "--asset", "55x"), sender)
	r.crpUsage(t, "asset 33 digits", crpEraseArgs("--object", "page", "--asset", strings.Repeat("5", 33)), sender)
	r.crpUsage(t, "asset missing", crpEraseArgs("--object", "page"), sender)
	nine := append([]string{}, meta...)
	for i := 0; i < 9; i++ {
		nine = append(nine, "--app", fmt.Sprintf("%d", 100+i))
	}
	r.crpUsage(t, "9 --app values", crpEraseArgs(nine...), sender)
	r.crpUsage(t, "--app not digits", crpEraseArgs(append(append([]string{}, meta...), "--app", "12ab")...), sender)
	r.crpUsage(t, "--app on a comment selector", crpEraseArgs(append(append([]string{}, meta...), "--app", "111")...), comment)
	// The sender id, comment id and keys are never argv: no such flag exists, and a rejected flag does not echo its value.
	for _, flag := range []string{"--sender-id", "--sender", "--from-id", "--comment-ref", "--comment-id", "--comment", "--actor-key", "--peer-key", "--key"} {
		r.crpUsage(t, "argv "+flag, crpEraseArgs(append(append([]string{}, meta...), flag, crpSender)...), sender)
		r.crpUsage(t, "argv "+flag+" alone", crpEraseArgs(append(append([]string{}, meta...), flag+"="+crpComment)...), "")
	}
	if len(r.erased) != 0 || len(r.opened) != 0 {
		t.Fatalf("usage errors reached the database seam: opened=%v erased=%d", r.opened, len(r.erased))
	}
}

// CRP01: the derived keys reach the definer call, the sender id never does; (b) and (c) pass
// through; a sender selector needs K_actor and fails closed without it.
func TestClaimsRetentionCRP01CLIDerivesKeysAndPassesSelectors(t *testing.T) {
	r := crpSeams(t).withOperator().withKey()
	if code := r.do(crpEraseArgs("--object", "page", "--asset", "5550001", "--app", "123456789012345", "--app", "111"), `{"sender_id":"`+crpSender+`"}`+"\n"); code != 0 {
		t.Fatalf("(a) exit=%d stderr=%q", code, r.stderr.String())
	}
	if len(r.erased) != 1 {
		t.Fatalf("(a) definer calls = %d", len(r.erased))
	}
	a := r.erased[0]
	if a.Request != crpRequest || a.Object != "page" || a.Asset != "5550001" || a.ActorKey != crpActorPage || a.CommentRef != "" || a.Bundle != "" || a.Tenant != "" || a.Store != "" {
		t.Errorf("(a) selector fields wrong (actor key vector, object, asset): request=%s object=%s asset=%s keyMatches=%t", a.Request, a.Object, a.Asset, a.ActorKey == crpActorPage)
	}
	if !reflect.DeepEqual(a.PeerKeys, []string{crpPeerPage, crpPeerPage111}) {
		t.Errorf("(a) peer keys != SocialPeerKey per --app in order (%d keys)", len(a.PeerKeys))
	}
	if strings.Contains(fmt.Sprintf("%#v|%v", reflect.ValueOf(a).Field(0), a.PeerKeys), crpSender) {
		t.Error("(a) the sender id is inside the selector")
	}
	for i := 0; i < reflect.ValueOf(a).NumField(); i++ {
		f := reflect.ValueOf(a).Field(i)
		if f.Kind() == reflect.String && strings.Contains(f.String(), crpSender) {
			t.Errorf("(a) selector field %d contains the sender id", i)
		}
	}
	first := strings.SplitN(r.stdout.String(), "\n", 2)[0]
	if first != "request="+crpRequest {
		t.Errorf("(a) first stdout line %q, want request=<uuid> (D6)", first)
	}
	r.assertClean(t, "erase a")

	// Instagram derives the other object's key.
	r.erased = nil
	if code := r.do(crpEraseArgs("--object", "instagram", "--asset", "5550001"), `{"sender_id":"`+crpSender+`"}`); code != 0 || len(r.erased) != 1 || r.erased[0].ActorKey != crpActorIG || len(r.erased[0].PeerKeys) != 0 {
		t.Errorf("(a instagram) exit=%d calls=%d", code, len(r.erased))
	}

	// (b): the comment ref passes through, no actor key is derived (K_actor not needed).
	noKey := crpSeams(t).withOperator()
	if code := noKey.do(crpEraseArgs("--object", "page", "--asset", "5550001"), `{"comment_ref":"`+crpComment+`"}`); code != 0 || len(noKey.erased) != 1 {
		t.Fatalf("(b) exit=%d stderr=%q", code, noKey.stderr.String())
	}
	b := noKey.erased[0]
	if b.CommentRef != crpComment || b.ActorKey != "" || len(b.PeerKeys) != 0 || b.Object != "page" || b.Asset != "5550001" {
		t.Errorf("(b) selector wrong: %+v", struct{ O, A string }{b.Object, b.Asset})
	}
	noKey.assertClean(t, "erase b")

	// (c): bundle selector with empty stdin; also works without K_actor.
	if code := noKey.do(crpEraseArgs("--tenant", crpTenant, "--store", crpStore, "--bundle", crpBundle), ""); code != 0 || len(noKey.erased) != 2 {
		t.Fatalf("(c) exit=%d stderr=%q", code, noKey.stderr.String())
	}
	c := noKey.erased[1]
	if c.Tenant != crpTenant || c.Store != crpStore || c.Bundle != crpBundle || c.ActorKey != "" || c.CommentRef != "" {
		t.Error("(c) selector wrong")
	}
	noKey.assertClean(t, "erase c")

	// A sender selector without K_actor fails closed (no derivation, nothing sent to the definer).
	before := len(noKey.erased)
	for name, env := range map[string]string{"unset": "", "not base64": "%%%not-base64%%%", "wrong length": base64.StdEncoding.EncodeToString([]byte("short")), "all zero": base64.StdEncoding.EncodeToString(make([]byte, 32))} {
		noKey.env["COMMERCE_CLAIMS_ACTOR_KEY"] = env
		code := noKey.do(crpEraseArgs("--object", "page", "--asset", "5550001"), `{"sender_id":"`+crpSender+`"}`)
		if code == 0 || len(noKey.erased) != before {
			t.Errorf("K_actor %s: exit=%d and %d definer calls", name, code, len(noKey.erased)-before)
		}
		noKey.assertClean(t, "K_actor "+name)
	}
}

// CRP01 row "exit-code mapping" (contract §5): 0 done, 2 usage, 3 held (retry_after RFC 3339 UTC),
// 4 not_found, 5 conflict/busy, 1 other with one fixed stderr code; nothing but fixed codes reaches stderr.
func TestClaimsRetentionCRP01CLIExitCodes(t *testing.T) {
	r := crpSeams(t).withOperator()
	args := crpEraseArgs("--tenant", crpTenant, "--store", crpStore, "--bundle", crpBundle)
	codes := regexp.MustCompile(`^retention_admin_[a-z_]+$`)
	cases := []struct {
		name string
		err  error
		want int
	}{
		{"usage", retention.ErrUsage, 2}, {"not_found", retention.ErrNotFound, 4}, {"conflict", retention.ErrConflict, 5},
		{"busy", retention.ErrBusy, 5}, {"other", errors.New(crpErrText + " " + crpDSNToken), 1},
		{"wrapped not_found", fmt.Errorf("wrap: %w", retention.ErrNotFound), 4},
	}
	seenStderr := map[string]string{}
	for _, c := range cases {
		r.eraseErr = c.err
		if got := r.do(args, ""); got != c.want {
			t.Errorf("%s: exit %d, want %d", c.name, got, c.want)
		}
		msg := strings.TrimSpace(r.stderr.String())
		if !codes.MatchString(msg) || strings.Count(r.stderr.String(), "\n") != 1 {
			t.Errorf("%s: stderr %q is not one fixed retention_admin_* code", c.name, r.stderr.String())
		}
		if r.stdout.Len() != 0 {
			t.Errorf("%s: failure wrote stdout %q", c.name, r.stdout.String())
		}
		seenStderr[c.name] = msg
		r.assertClean(t, c.name)
	}
	if seenStderr["not_found"] == seenStderr["conflict"] || seenStderr["usage"] == seenStderr["other"] || seenStderr["other"] == seenStderr["not_found"] {
		t.Errorf("distinct outcomes share a stderr code: %v", seenStderr)
	}
	// held: exit 3, retry_after RFC 3339 UTC on stdout, request id first, still nothing else.
	r.eraseErr = nil
	r.held = time.Date(2026, 10, 8, 3, 4, 5, 0, time.FixedZone("x", 8*3600)) // a non-UTC input must still print UTC
	if got := r.do(args, ""); got != 3 {
		t.Fatalf("held exit %d, want 3", got)
	}
	if want := "request=" + crpRequest + "\nretry_after=2026-10-07T19:04:05Z\n"; r.stdout.String() != want {
		t.Errorf("held stdout %q, want %q", r.stdout.String(), want)
	}
	if !codes.MatchString(strings.TrimSpace(r.stderr.String())) {
		t.Errorf("held stderr %q", r.stderr.String())
	}
	r.assertClean(t, "held")
	// done: exit 0, stderr empty, first line the request id then sorted numeric key=value lines only.
	r.held = time.Time{}
	if got := r.do(args, ""); got != 0 || r.stderr.Len() != 0 {
		t.Fatalf("done exit %d stderr %q", got, r.stderr.String())
	}
	lines := strings.Split(strings.TrimRight(r.stdout.String(), "\n"), "\n")
	if lines[0] != "request="+crpRequest {
		t.Errorf("done first line %q", lines[0])
	}
	num := regexp.MustCompile(`^[a-z_]+=[0-9]+$`)
	var keys []string
	for _, l := range lines[1:] {
		if !num.MatchString(l) {
			t.Errorf("done line %q is not key=number", l)
		}
		keys = append(keys, l)
	}
	sorted := append([]string{}, keys...)
	for i := range sorted {
		for j := i + 1; j < len(sorted); j++ {
			if sorted[j] < sorted[i] {
				sorted[i], sorted[j] = sorted[j], sorted[i]
			}
		}
	}
	if !reflect.DeepEqual(keys, sorted) || len(keys) != len(r.eraseCounts) {
		t.Errorf("done lines %v not the sorted counts %v", keys, r.eraseCounts)
	}
	r.assertClean(t, "done")
	// unknown subcommand / no subcommand.
	for _, a := range [][]string{{}, {"nope"}, {"erase", "--nope"}, {"ERASE"}} {
		r.crpUsage(t, fmt.Sprintf("args %v", a), a, "")
	}
}

// CRP01 row "status runs on the job DSN, every other subcommand given only the job DSN -> exit 2".
func TestClaimsRetentionCRP01CLIJobDSNOnlyStatus(t *testing.T) {
	r := crpSeams(t)
	r.env["COMMERCE_RETENTION_JOB_DATABASE_URL"] = crpDSN()
	if got := r.do([]string{"status"}, ""); got != 0 {
		t.Fatalf("status with only the job DSN: exit %d stderr %q", got, r.stderr.String())
	}
	if !reflect.DeepEqual(r.opened, []string{"job"}) {
		t.Errorf("status opened %v, want only the job pool", r.opened)
	}
	// D6 / contract §10(5): the smoke parses key=value lines; enforced=1 and last_run_unix present.
	out := r.stdout.String()
	for _, want := range []string{"enforced=1\n", "version=4\n", "link_days=7\n", "intake_days=30\n", "claims_days=90\n", "social_days=30\n", "last_run_unix=1790000000\n", "last_run_more=0\n"} {
		if !strings.Contains(out, want) {
			t.Errorf("status output lacks %q: %q", want, out)
		}
	}
	r.assertClean(t, "status job")
	r.opened = nil
	// every other subcommand refuses the job DSN with exit 2 and opens nothing.
	for name, args := range map[string][]string{
		"policy-set": {"policy-set", "--expected-version", "1", "--enforced=true", "--link-days", "7", "--intake-days", "30", "--claims-days", "90", "--social-days", "30"},
		"run":        {"run"},
		"run limit":  {"run", "--limit", "10"},
		"replay":     {"replay"},
		"erase c":    crpEraseArgs("--tenant", crpTenant, "--store", crpStore, "--bundle", crpBundle),
	} {
		r.crpUsage(t, name+" with only the job DSN", args, "")
	}
	// erase (a) with the job DSN and K_actor: still refused (no key derivation is reached first).
	r.withKey()
	r.crpUsage(t, "erase a with only the job DSN", crpEraseArgs("--object", "page", "--asset", "5550001"), `{"sender_id":"`+crpSender+`"}`)
	if len(r.opened) != 0 {
		t.Errorf("a refused subcommand opened %v", r.opened)
	}
	// operator DSN: every subcommand runs on the operator pool.
	op := crpSeams(t).withOperator()
	for name, args := range map[string][]string{
		"status": {"status"}, "run": {"run"},
		"policy-set": {"policy-set", "--expected-version", "1", "--enforced=false", "--link-days", "7", "--intake-days", "30", "--claims-days", "90", "--social-days", "30"},
		"replay":     {"replay"},
	} {
		op.opened = nil
		if got := op.do(args, ""); got != 0 || !reflect.DeepEqual(op.opened, []string{"operator"}) {
			t.Errorf("%s on the operator DSN: exit %d opened %v stderr %q", name, got, op.opened, op.stderr.String())
		}
		op.assertClean(t, name+" operator")
	}
	// no DSN at all: not a usage error, one fixed config failure, nothing opened.
	none := crpSeams(t)
	if got := none.do([]string{"status"}, ""); got == 0 || got == 2 || len(none.opened) != 0 {
		t.Errorf("status without any DSN: exit %d opened %v (want a non-usage failure)", got, none.opened)
	}
	none.assertClean(t, "no dsn")
}

func crpWriteFile(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "tombstones.json")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// CRP01 + D7: `replay --tombstones-file` is a strict JSON array of full tuples (exactly the six keys).
func TestClaimsRetentionCRP01CLITombstoneFileStrictness(t *testing.T) {
	r := crpSeams(t).withOperator()
	hex64 := func(c string) string { return strings.Repeat(c, 64) }
	actor := fmt.Sprintf(`{"request_id":%q,"selector_digest":%q,"actor_digest":%q,"bundle_tenant":null,"bundle_store":null,"bundle_ref":null}`, crpRequest, hex64("a"), hex64("b"))
	bundle := fmt.Sprintf(`{"request_id":%q,"selector_digest":%q,"actor_digest":null,"bundle_tenant":%q,"bundle_store":%q,"bundle_ref":%q}`, "1f0e0d0c-0b0a-4908-8706-050403020100", hex64("c"), crpTenant, crpStore, crpBundle)
	// valid: one of each kind decodes into the tuple the definer replays.
	if code := r.do([]string{"replay", "--tombstones-file", crpWriteFile(t, "["+actor+","+bundle+"]")}, ""); code != 0 || len(r.replayed) != 1 {
		t.Fatalf("valid file: exit %d stderr %q replayed=%d", code, r.stderr.String(), len(r.replayed))
	}
	got := r.replayed[0]
	if len(got) != 2 || got[0].RequestID != crpRequest || len(got[0].SelectorDigest) != 32 || len(got[0].ActorDigest) != 32 || got[0].BundleRef != "" ||
		got[0].SelectorDigest[0] != 0xaa || got[0].ActorDigest[0] != 0xbb {
		t.Errorf("actor tuple decoded wrong: %+v", got[0])
	}
	if got[1].BundleTenant != crpTenant || got[1].BundleStore != crpStore || got[1].BundleRef != crpBundle || len(got[1].ActorDigest) != 0 || got[1].SelectorDigest[0] != 0xcc {
		t.Errorf("bundle tuple decoded wrong: %+v", got[1])
	}
	if !strings.Contains(r.stdout.String(), "tombstones=2") {
		t.Errorf("replay stdout %q", r.stdout.String())
	}
	// replay from the log: no file, definer called with nil (not an empty list).
	r.replayed = nil
	if code := r.do([]string{"replay"}, ""); code != 0 || len(r.replayed) != 1 || r.replayed[0] != nil {
		t.Errorf("replay from log: exit %d replayed=%v (want one call with a nil list)", code, r.replayed)
	}
	// strict rejections: usage, exit 2, nothing replayed.
	r.replayed = nil
	fixed := func(mut func(string) string) string { return "[" + mut(actor) + "]" }
	bad := map[string]string{
		"empty file":              ``,
		"not JSON":                `garbage`,
		"an object, not an array": actor,
		"null":                    `null`,
		"trailing data":           "[" + actor + "] x",
		"trailing array":          "[" + actor + "][" + actor + "]",
		"unknown key": fixed(func(s string) string {
			return strings.Replace(s, `"bundle_ref":null`, `"bundle_ref":null,"extra":1`, 1)
		}),
		"missing key":              fixed(func(s string) string { return strings.Replace(s, `,"bundle_ref":null`, ``, 1) }),
		"missing actor_digest":     fixed(func(s string) string { return strings.Replace(s, fmt.Sprintf(`"actor_digest":%q,`, hex64("b")), ``, 1) }),
		"request not a uuid":       fixed(func(s string) string { return strings.Replace(s, crpRequest, "not-a-uuid", 1) }),
		"selector digest short":    fixed(func(s string) string { return strings.Replace(s, hex64("a"), strings.Repeat("a", 63), 1) }),
		"selector digest upper":    fixed(func(s string) string { return strings.Replace(s, hex64("a"), strings.Repeat("A", 64), 1) }),
		"selector digest not hex":  fixed(func(s string) string { return strings.Replace(s, hex64("a"), strings.Repeat("g", 64), 1) }),
		"actor digest short":       fixed(func(s string) string { return strings.Replace(s, hex64("b"), strings.Repeat("b", 62), 1) }),
		"actor and bundle both":    "[" + strings.Replace(actor, `"bundle_tenant":null,"bundle_store":null,"bundle_ref":null`, fmt.Sprintf(`"bundle_tenant":%q,"bundle_store":%q,"bundle_ref":%q`, crpTenant, crpStore, crpBundle), 1) + "]",
		"neither actor nor bundle": "[" + strings.Replace(actor, fmt.Sprintf(`"actor_digest":%q`, hex64("b")), `"actor_digest":null`, 1) + "]",
		"bundle triple partial":    "[" + strings.Replace(bundle, fmt.Sprintf(`"bundle_store":%q`, crpStore), `"bundle_store":null`, 1) + "]",
		"bundle id not a uuid":     "[" + strings.Replace(bundle, crpBundle, "nope", 1) + "]",
		"selector digest number":   fixed(func(s string) string { return strings.Replace(s, fmt.Sprintf("%q", hex64("a")), `12`, 1) }),
	}
	for name, body := range bad {
		r.crpUsage(t, "tombstone file "+name, []string{"replay", "--tombstones-file", crpWriteFile(t, body)}, "")
	}
	// > 10 000 tuples and > 4 MiB files.
	many := "[" + strings.TrimSuffix(strings.Repeat(actor+",", 10001), ",") + "]"
	r.crpUsage(t, "10001 tuples", []string{"replay", "--tombstones-file", crpWriteFile(t, many)}, "")
	huge := "[" + actor + strings.Repeat(" ", (4<<20)+16) + "]"
	r.crpUsage(t, "file over 4 MiB", []string{"replay", "--tombstones-file", crpWriteFile(t, huge)}, "")
	r.crpUsage(t, "file missing", []string{"replay", "--tombstones-file", filepath.Join(t.TempDir(), "absent.json")}, "")
	r.crpUsage(t, "positional argument", []string{"replay", "stray"}, "")
	// exactly 10000 tuples is accepted.
	ten := "[" + strings.TrimSuffix(strings.Repeat(actor+",", 10000), ",") + "]"
	r.replayed = nil
	if code := r.do([]string{"replay", "--tombstones-file", crpWriteFile(t, ten)}, ""); code != 0 || len(r.replayed) != 1 || len(r.replayed[0]) != 10000 {
		t.Errorf("10000 tuples: exit %d", code)
	}
}

// CRP01: policy-set carries every value explicitly and a CAS version; run has a bounded --limit.
func TestClaimsRetentionCRP01CLIPolicyAndRunFlags(t *testing.T) {
	r := crpSeams(t).withOperator()
	ok := []string{"policy-set", "--expected-version", "3", "--enforced=true", "--link-days", "7", "--intake-days", "30", "--claims-days", "90", "--social-days", "30"}
	if code := r.do(ok, ""); code != 0 || r.policies != 1 || strings.TrimSpace(r.stdout.String()) != "version=4" {
		t.Fatalf("valid policy-set: exit %d policies=%d stdout=%q", code, r.policies, r.stdout.String())
	}
	// A change of policy is never a default: without a CAS version it is usage.
	r.crpUsage(t, "policy-set without expected-version", []string{"policy-set", "--enforced=true", "--link-days", "7", "--intake-days", "30", "--claims-days", "90", "--social-days", "30"}, "")
	r.crpUsage(t, "policy-set expected-version 0", append(append([]string{}, ok[:2]...), append([]string{"0"}, ok[3:]...)...), "")
	r.crpUsage(t, "policy-set positional", append(append([]string{}, ok...), "x"), "")
	r.crpUsage(t, "policy-set non-numeric days", []string{"policy-set", "--expected-version", "3", "--enforced=true", "--link-days", "seven", "--intake-days", "30", "--claims-days", "90", "--social-days", "30"}, "")
	for _, limit := range []string{"0", "-1", "1001", "abc"} {
		r.crpUsage(t, "run --limit "+limit, []string{"run", "--limit", limit}, "")
	}
	for _, limit := range []string{"1", "500", "1000"} {
		before := r.runs
		if code := r.do([]string{"run", "--limit", limit}, ""); code != 0 || r.runs != before+1 {
			t.Errorf("run --limit %s: exit %d", limit, code)
		}
	}
	if code := r.do([]string{"run"}, ""); code != 0 {
		t.Errorf("run without --limit: exit %d", code)
	}
	if !strings.Contains(r.stdout.String(), "links=4") || strings.Contains(r.stdout.String(), " ") {
		t.Errorf("run stdout %q is not numeric key=value lines", r.stdout.String())
	}
	r.assertClean(t, "run")
	r.crpUsage(t, "status positional", []string{"status", "extra"}, "")
	r.crpUsage(t, "status unknown flag", []string{"status", "--verbose"}, "")
}
