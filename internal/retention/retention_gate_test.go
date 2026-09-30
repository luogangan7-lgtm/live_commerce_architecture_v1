// retention_gate_test.go is the independent CRP01 gate for package retention (U08, contract
// claims-retention-purge-v1 §5 + §7 row CRP01). Written from the contract and the FROZEN Go
// interface of docs/delivery/units/retention-core.md only; it drives the exported API and never
// reads unexported names. Evidence label: UNIT (no database is reached: pools are lazy and point
// at a closed loopback port, so a call that passes shape validation fails at connect time with a
// fixed error). The database halves of CRP01 (job loop stop rules, SQLSTATE -> error mapping,
// meta.SocialPeerKey == the consumer's stored peer_key) are asserted in tests/foundation
// CRP05/CRP06/CRP09 because they need PG (brief: "needs PG -> assert in CRP09").
package retention_test

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"

	"livecommerce/internal/integrations/meta"
	"livecommerce/internal/retention"
)

// Synthetic sentinels: none is a real id, key or credential (PROCESS §6).
const (
	crpKey      = "ab12cd34ef56ab12cd34ef56ab12cd34ef56ab12cd34ef56ab12cd34ef56ab12"
	crpPeer     = "cd34ef56ab12cd34ef56ab12cd34ef56ab12cd34ef56ab12cd34ef56ab12cd34"
	crpComment  = "918273645_5544332211"
	crpRequest  = "0f0e0d0c-0b0a-4908-8706-050403020100"
	crpTenant   = "11111111-1111-4111-8111-111111111111"
	crpStore    = "22222222-2222-4222-8222-222222222222"
	crpBundle   = "33333333-3333-4333-8333-333333333333"
	crpDSNToken = "dsn-sentinel-crp01-5e21"
)

func crpSentinels() []string {
	return []string{crpKey, crpPeer, crpComment, crpTenant, crpStore, crpBundle, crpDSNToken}
}

// crpLazyPool is a pool whose first connect fails at once (port 1 is closed). The DSN password is a
// sentinel so an error that echoed the DSN would be caught. Built with net/url (PROCESS §6).
func crpLazyPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	u := url.URL{Scheme: "postgres", User: url.UserPassword("lc_retention_probe", crpDSNToken), Host: "127.0.0.1:1", Path: "/probe", RawQuery: "sslmode=disable&connect_timeout=2"}
	pool, err := pgxpool.New(context.Background(), u.String())
	if err != nil {
		t.Fatalf("lazy pool: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func crpNoSentinel(t *testing.T, label, text string) {
	t.Helper()
	for _, s := range crpSentinels() {
		if strings.Contains(text, s) {
			t.Errorf("%s leaks %q: %q", label, s, text)
		}
	}
}

func crpFullSelectors() map[string]retention.Selector {
	return map[string]retention.Selector{
		"a": {Request: crpRequest, Object: "page", Asset: "5550001", ActorKey: crpKey, PeerKeys: []string{crpPeer, crpKey}},
		"b": {Request: crpRequest, Object: "instagram", Asset: "5550001", CommentRef: crpComment},
		"c": {Request: crpRequest, Tenant: crpTenant, Store: crpStore, Bundle: crpBundle},
	}
}

// CRP01 + §5 (redacted String/GoString/Format/MarshalJSON): every formatting path of Selector,
// including nested in slices, maps, structs, pointers, json and slog, prints no key, ref or id.
func TestClaimsRetentionCRP01SelectorRedaction(t *testing.T) {
	type wrapper struct {
		S retention.Selector
		P *retention.Selector
		L []retention.Selector
		M map[string]retention.Selector
	}
	for name, sel := range crpFullSelectors() {
		s := sel
		w := wrapper{S: s, P: &s, L: []retention.Selector{s}, M: map[string]retention.Selector{"k": s}}
		// fmt prefers Format over String/GoString, so a leak added to String() alone would hide behind
		// Format: call every redaction method directly (Stringer, GoStringer, json.Marshaler), on the
		// value and on the pointer.
		for _, v := range []any{s, &s} {
			st, ok1 := v.(fmt.Stringer)
			gs, ok2 := v.(fmt.GoStringer)
			jm, ok3 := v.(json.Marshaler)
			if !ok1 || !ok2 || !ok3 {
				t.Fatalf("%s: Selector lacks String/GoString/MarshalJSON (Stringer=%t GoStringer=%t Marshaler=%t)", name, ok1, ok2, ok3)
			}
			crpNoSentinel(t, name+" String()", st.String())
			crpNoSentinel(t, name+" GoString()", gs.GoString())
			raw, err := jm.MarshalJSON()
			if err != nil {
				t.Fatalf("%s: MarshalJSON: %v", name, err)
			}
			crpNoSentinel(t, name+" MarshalJSON()", string(raw))
		}
		for _, verb := range []string{"%v", "%+v", "%#v", "%s", "%q", "%x", "%d"} {
			crpNoSentinel(t, name+" Sprintf("+verb+") selector", fmt.Sprintf(verb, s))
			crpNoSentinel(t, name+" Sprintf("+verb+") *selector", fmt.Sprintf(verb, &s))
			crpNoSentinel(t, name+" Sprintf("+verb+") wrapper", fmt.Sprintf(verb, w))
		}
		crpNoSentinel(t, name+" Sprint", fmt.Sprint(s, &s, []any{s}))
		raw, err := json.Marshal(w)
		if err != nil {
			t.Fatalf("%s: json.Marshal(wrapper): %v", name, err)
		}
		crpNoSentinel(t, name+" json wrapper", string(raw))
		raw, err = json.Marshal(s)
		if err != nil {
			t.Fatalf("%s: json.Marshal(selector): %v", name, err)
		}
		crpNoSentinel(t, name+" json selector", string(raw))
		raw, err = json.MarshalIndent(map[string]any{"selector": s, "list": []retention.Selector{s}}, "", " ")
		if err != nil {
			t.Fatalf("%s: json.MarshalIndent: %v", name, err)
		}
		crpNoSentinel(t, name+" json indent", string(raw))
		var b strings.Builder
		log := slog.New(slog.NewTextHandler(&b, nil))
		log.Info("erase", "selector", s, "ptr", &s, slog.Any("list", []retention.Selector{s}))
		jb := strings.Builder{}
		slog.New(slog.NewJSONHandler(&jb, nil)).Info("erase", "selector", s, "ptr", &s)
		crpNoSentinel(t, name+" slog text", b.String())
		crpNoSentinel(t, name+" slog json", jb.String())
	}
}

// CRP01 + §5: Counts prints numbers only on every path, and its keys are the closed count set.
func TestClaimsRetentionCRP01CountsNumbersOnly(t *testing.T) {
	c := retention.Counts{"links": 3, "bundles": 2, "intake": 1, "operations": 4, "comment_events": 5,
		"messages": 6, "conversations": 7, "more": 1, "enforced": 1, "lines": 9, "replayed": 1}
	pair := regexp.MustCompile(`^[a-z_]+=[0-9]+$`)
	for _, verb := range []string{"%v", "%+v", "%#v", "%s"} {
		out := fmt.Sprintf(verb, c)
		for _, field := range strings.Fields(out) {
			if !pair.MatchString(field) {
				t.Errorf("Sprintf(%s) has a non key=number field %q in %q", verb, field, out)
			}
		}
		if got := len(strings.Fields(out)); got != len(c) {
			t.Errorf("Sprintf(%s) printed %d fields for %d counts: %q", verb, got, len(c), out)
		}
	}
	// Direct method calls (fmt prefers Format): String, GoString and MarshalJSON are numbers only too.
	direct := []string{c.String(), c.GoString()}
	for i, out := range direct {
		for _, field := range strings.Fields(out) {
			if !pair.MatchString(field) {
				t.Errorf("direct method %d has a non key=number field %q", i, field)
			}
		}
	}
	raw, err := json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	if mj, err := c.MarshalJSON(); err != nil || string(mj) != string(raw) {
		t.Errorf("MarshalJSON %s (%v) differs from json.Marshal %s", mj, err, raw)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil || len(m) != len(c) {
		t.Fatalf("json %s: %v", raw, err)
	}
	for k, v := range m {
		if _, isNumber := v.(float64); !isNumber {
			t.Errorf("json count %q is %T, want a number", k, v)
		}
	}
	// Counts nested in a struct or a log line stay numeric.
	crpNoSentinel(t, "counts", fmt.Sprintf("%v %+v %#v", c, struct{ C retention.Counts }{c}, []retention.Counts{c}))
	var b strings.Builder
	slog.New(slog.NewTextHandler(&b, nil)).Info("run", "counts", c)
	if !strings.Contains(b.String(), "links=3") {
		t.Errorf("slog counts line lost the numbers: %q", b.String())
	}
}

// CRP01 + §5/§3 (Erase shape = exactly one selector; nothing dialled for a bad shape): each invalid
// shape returns ErrUsage, and it does so without connecting (the lazy pool would report a different
// error if the call reached the network).
func TestClaimsRetentionCRP01SelectorShapes(t *testing.T) {
	pool := crpLazyPool(t)
	ctx := context.Background()
	full := crpFullSelectors()
	mix := func(parts ...string) retention.Selector {
		var s retention.Selector
		s.Request = crpRequest
		for _, p := range parts {
			x := full[p]
			switch p {
			case "a":
				s.Object, s.Asset, s.ActorKey, s.PeerKeys = x.Object, x.Asset, x.ActorKey, x.PeerKeys
			case "b":
				s.Object, s.Asset, s.CommentRef = x.Object, x.Asset, x.CommentRef
			case "c":
				s.Tenant, s.Store, s.Bundle = x.Tenant, x.Store, x.Bundle
			}
		}
		return s
	}
	base := func(mut func(*retention.Selector)) retention.Selector {
		s := full["a"]
		mut(&s)
		return s
	}
	bad := map[string]retention.Selector{
		"no selector at all": {Request: crpRequest},
		"a+b":                func() retention.Selector { s := mix("a", "b"); return s }(),
		"a+c":                mix("a", "c"),
		"b+c":                mix("b", "c"),
		"a+b+c":              mix("a", "b", "c"),
		"empty request":      base(func(s *retention.Selector) { s.Request = "" }),
		"request not a uuid": base(func(s *retention.Selector) { s.Request = "not-a-uuid" }),
		"request uppercase":  base(func(s *retention.Selector) { s.Request = strings.ToUpper(crpRequest) }),
		"key 63 chars":       base(func(s *retention.Selector) { s.ActorKey = crpKey[:63] }),
		"key 65 chars":       base(func(s *retention.Selector) { s.ActorKey = crpKey + "a" }),
		"key uppercase":      base(func(s *retention.Selector) { s.ActorKey = strings.ToUpper(crpKey) }),
		"key not hex":        base(func(s *retention.Selector) { s.ActorKey = strings.Repeat("g", 64) }),
		"9 peer keys": base(func(s *retention.Selector) {
			s.PeerKeys = make([]string, 9)
			for i := range s.PeerKeys {
				s.PeerKeys[i] = crpPeer
			}
		}),
		"peer key short":          base(func(s *retention.Selector) { s.PeerKeys = []string{crpPeer[:10]} }),
		"peer key uppercase":      base(func(s *retention.Selector) { s.PeerKeys = []string{strings.ToUpper(crpPeer)} }),
		"object twitter":          base(func(s *retention.Selector) { s.Object = "twitter" }),
		"object empty":            base(func(s *retention.Selector) { s.Object = "" }),
		"asset not digits":        base(func(s *retention.Selector) { s.Asset = "55x" }),
		"asset empty":             base(func(s *retention.Selector) { s.Asset = "" }),
		"comment ref letters":     {Request: crpRequest, Object: "page", Asset: "5550001", CommentRef: "abc"},
		"comment ref 81 chars":    {Request: crpRequest, Object: "page", Asset: "5550001", CommentRef: strings.Repeat("1", 81)},
		"comment ref with dash":   {Request: crpRequest, Object: "page", Asset: "5550001", CommentRef: "12-34"},
		"comment with peer keys":  {Request: crpRequest, Object: "page", Asset: "5550001", CommentRef: crpComment, PeerKeys: []string{crpPeer}},
		"bundle without tenant":   {Request: crpRequest, Store: crpStore, Bundle: crpBundle},
		"bundle without store":    {Request: crpRequest, Tenant: crpTenant, Bundle: crpBundle},
		"bundle not a uuid":       {Request: crpRequest, Tenant: crpTenant, Store: crpStore, Bundle: "7"},
		"tenant without a bundle": {Request: crpRequest, Tenant: crpTenant, Store: crpStore, Object: "page", Asset: "5550001", CommentRef: crpComment},
	}
	for name, s := range bad {
		_, held, err := retention.Erase(ctx, pool, s)
		if !errors.Is(err, retention.ErrUsage) || held.IsHeld() {
			t.Errorf("%s: Erase returned err=%v held=%v, want ErrUsage before any connection", name, err, held)
		}
	}
	if _, _, err := retention.Erase(ctx, nil, full["a"]); err == nil {
		t.Error("Erase on a nil pool succeeded")
	}
	// Valid shapes pass validation and only then fail at connect time with a fixed, redacted error.
	eight := full["a"]
	eight.PeerKeys = make([]string, 8)
	for i := range eight.PeerKeys {
		eight.PeerKeys[i] = crpPeer
	}
	noPeers := full["a"]
	noPeers.PeerKeys = nil
	for name, s := range map[string]retention.Selector{"a with 2 peers": full["a"], "a with 8 peers": eight, "a with none": noPeers, "b page": full["b"], "b instagram": full["b"], "c": full["c"]} {
		short, cancel := context.WithTimeout(ctx, 5*time.Second)
		_, _, err := retention.Erase(short, pool, s)
		cancel()
		if err == nil {
			t.Errorf("%s: Erase against a closed port succeeded", name)
			continue
		}
		if errors.Is(err, retention.ErrUsage) {
			t.Errorf("%s: a valid selector shape was rejected as usage", name)
		}
		crpNoSentinel(t, name+" error text", err.Error())
	}
}

// CRP01 + §5: argument shape checks of the remaining operator calls, and fixed error text.
func TestClaimsRetentionCRP01OperatorCallShapes(t *testing.T) {
	pool := crpLazyPool(t)
	ctx := context.Background()
	for _, limit := range []int{-1, 0, 1001, 100000} {
		if _, err := retention.RunOnce(ctx, pool, limit); !errors.Is(err, retention.ErrUsage) {
			t.Errorf("RunOnce(limit=%d) err=%v, want ErrUsage (definer range is 1..1000)", limit, err)
		}
	}
	for _, limit := range []int{1, 500, 1000} {
		short, cancel := context.WithTimeout(ctx, 5*time.Second)
		_, err := retention.RunOnce(short, pool, limit)
		cancel()
		if err == nil || errors.Is(err, retention.ErrUsage) {
			t.Errorf("RunOnce(limit=%d) err=%v, want a connect failure (valid range)", limit, err)
		} else {
			crpNoSentinel(t, "RunOnce error", err.Error())
		}
	}
	if _, err := retention.RunOnce(ctx, nil, 500); err == nil {
		t.Error("RunOnce(nil pool) succeeded")
	}
	for _, v := range []int64{-5, 0} {
		if _, err := retention.SetPolicy(ctx, pool, v, retention.Policy{LinkDays: 7, IntakeDays: 30, ClaimsDays: 90, SocialDays: 30}); !errors.Is(err, retention.ErrUsage) {
			t.Errorf("SetPolicy(expectedVersion=%d) err=%v, want ErrUsage (CAS version is > 0)", v, err)
		}
	}
	if _, err := retention.Replay(ctx, pool, make([]retention.Tombstone, 10001)); !errors.Is(err, retention.ErrUsage) {
		t.Errorf("Replay of 10001 tombstones err=%v, want ErrUsage (D7 cap 10000)", err)
	}
	if _, err := retention.GetStatus(ctx, nil); err == nil {
		t.Error("GetStatus(nil pool) succeeded")
	}
	// Fixed errors carry no driver text, DSN or selector.
	for name, err := range map[string]error{"usage": retention.ErrUsage, "not_found": retention.ErrNotFound, "conflict": retention.ErrConflict, "busy": retention.ErrBusy} {
		if err == nil || err.Error() == "" {
			t.Errorf("%s: exported error missing or empty", name)
			continue
		}
		crpNoSentinel(t, name+" fixed error", err.Error())
	}
	seen := map[string]string{}
	for name, err := range map[string]error{"usage": retention.ErrUsage, "not_found": retention.ErrNotFound, "conflict": retention.ErrConflict, "busy": retention.ErrBusy} {
		if other, dup := seen[err.Error()]; dup {
			t.Errorf("%s and %s share error text %q: a caller could not tell them apart", name, other, err.Error())
		}
		seen[err.Error()] = name
	}
	if errors.Is(retention.ErrNotFound, retention.ErrConflict) || errors.Is(retention.ErrUsage, retention.ErrBusy) {
		t.Error("distinct retention errors alias each other")
	}
}

// CRP01 + §5 (job): kind, uniqueness window, timeout, constructor guard; the worker and the
// periodic job are accepted by a River client (schedule + args valid for the pinned River).
func TestClaimsRetentionCRP01JobContract(t *testing.T) {
	if retention.JobKind != "claims_retention_v1" || (retention.JobArgs{}).Kind() != retention.JobKind {
		t.Fatalf("job kind %q / %q, want claims_retention_v1", retention.JobKind, (retention.JobArgs{}).Kind())
	}
	// RD3: one job per hour (unique by period) so a restart inside the hour inserts nothing.
	if got := (retention.JobArgs{}).InsertOpts().UniqueOpts.ByPeriod; got != time.Hour {
		t.Fatalf("UniqueOpts.ByPeriod = %v, want 1h", got)
	}
	if _, err := retention.NewWorker(nil); err == nil {
		t.Fatal("NewWorker(nil) accepted a nil pool")
	}
	pool := crpLazyPool(t)
	w, err := retention.NewWorker(pool)
	if err != nil || w == nil {
		t.Fatalf("NewWorker(pool) = %v, %v", w, err)
	}
	if d := w.Timeout(nil); d <= 0 || d > time.Hour {
		t.Fatalf("Timeout = %v, want a positive bound of at most one period", d)
	}
	crpNoSentinel(t, "worker formatting", fmt.Sprintf("%v %+v %#v", w, w, w))
	if pj := retention.PeriodicJob(); pj == nil {
		t.Fatal("PeriodicJob() is nil")
	}
	workers := river.NewWorkers()
	river.AddWorker(workers, w)
	client, err := river.NewClient(riverpgxv5.New(nil), &river.Config{Schema: "river", Workers: workers, PeriodicJobs: []*river.PeriodicJob{retention.PeriodicJob()}})
	if err != nil || client == nil {
		t.Fatalf("River rejected the retention worker/periodic job: %v", err)
	}
}

// crpActorKey is K_actor of the vectors: 32 bytes 0x01..0x20 (synthetic, never a real key).
func crpActorKey(t *testing.T) meta.ClaimsActorKey {
	t.Helper()
	raw := make([]byte, 32)
	for i := range raw {
		raw[i] = byte(i + 1)
	}
	k, err := meta.NewClaimsActorKey(raw)
	if err != nil {
		t.Fatal(err)
	}
	return k
}

// CRP01 + §7 ("SocialPeerKey ... and ClaimActorKey vectors unchanged"). The literals were computed
// once with python3 (json.dumps separators, hmac/hashlib), independent of Go; the formulas are those of
// contract §0 facts: actor = hex(HMAC(K,"meta-claim-actor/v1",object,asset,from.id)) and
// peer = unkeyed tuple hash of ("meta-social-peer/v1",app,object,asset,sender.id). The equality with
// what the consumer projection stores is CRP06 (REAL_PG).
func TestClaimsRetentionCRP01KeyVectors(t *testing.T) {
	k := crpActorKey(t)
	actor := []struct{ object, asset, from, want string }{
		{"page", "5550001", "918273645", "1d9b4d74a02d47ed6993ac8404c10fb01104dfa958ab2eb57c6b504760093ec3"},
		{"instagram", "5550001", "918273645", "0051dceed4899959a25937f6235f28e1dfae073b4c751d71288b6b7ec4e1ce6c"},
	}
	for _, v := range actor {
		if got := meta.ClaimActorKey(k, v.object, v.asset, v.from); got != v.want {
			t.Errorf("ClaimActorKey(%s,%s) vector drift: %s", v.object, v.asset, got)
		}
		// The same value from the contract formula, computed here.
		msg, _ := json.Marshal([]string{"meta-claim-actor/v1", v.object, v.asset, v.from})
		raw := make([]byte, 32)
		for i := range raw {
			raw[i] = byte(i + 1)
		}
		mac := hmac.New(sha256.New, raw)
		mac.Write(msg)
		if want := hex.EncodeToString(mac.Sum(nil)); v.want != want {
			t.Errorf("literal %s disagrees with the contract formula %s", v.want, want)
		}
	}
	if a, b := meta.ClaimActorKey(k, "page", "5550001", "918273645"), meta.ClaimActorKey(k, "page", "5550002", "918273645"); a == b {
		t.Error("actor key is not per asset")
	}
	if a, b := meta.ClaimActorKey(k, "page", "5550001", "918273645"), meta.ClaimActorKey(k, "page", "5550001", "918273646"); a == b {
		t.Error("actor key is not per sender")
	}
	peer := []struct{ app, object, asset, sender, want string }{
		{"123456789012345", "page", "5550001", "918273645", "275ab0ba43c50b2c173997c19a21bfb3f3ac8d7e24d1cc4bb58476b153471364"},
		{"123456789012345", "instagram", "5550001", "918273645", "d683c6a8a952eacb43a31dff62e0ab0c36afd8ccd8a6cd80d6e34c4230a9d785"},
		{"111", "page", "5550001", "918273645", "971254134e46a8c604815419263750d35ddcaa675ae30489b501f8a44d702e00"},
	}
	seen := map[string]bool{}
	for _, v := range peer {
		got := meta.SocialPeerKey(v.app, v.object, v.asset, v.sender)
		if got != v.want {
			t.Errorf("SocialPeerKey(%s,%s,%s) vector drift: %s", v.app, v.object, v.asset, got)
		}
		msg, _ := json.Marshal([]string{"meta-social-peer/v1", v.app, v.object, v.asset, v.sender})
		sum := sha256.Sum256(msg)
		if want := hex.EncodeToString(sum[:]); got != want {
			t.Errorf("SocialPeerKey %s disagrees with the contract formula %s", got, want)
		}
		if seen[got] {
			t.Errorf("SocialPeerKey collision across app/object: %s", got)
		}
		seen[got] = true
	}
	// The peer key is unkeyed and domain separated from the keyed actor key.
	if meta.SocialPeerKey("123456789012345", "page", "5550001", "918273645") == meta.ClaimActorKey(k, "page", "5550001", "918273645") {
		t.Error("peer key and actor key coincide")
	}
	// The zero key derives no actor (staging off): never a guessable value.
	if got := meta.ClaimActorKey(meta.ClaimsActorKey{}, "page", "5550001", "918273645"); got != "" {
		t.Errorf("zero K_actor derived %q", got)
	}
	// K_actor never prints.
	crpNoSentinel(t, "K_actor formatting", fmt.Sprintf("%v %+v %#v %s", k, k, k, k))
	if strings.Contains(fmt.Sprintf("%v", k), "0102030405") {
		t.Error("K_actor formatting shows key bytes")
	}
	if raw, err := json.Marshal(k); err != nil || strings.Contains(string(raw), "AQIDBAUG") {
		t.Errorf("K_actor json %s %v", raw, err)
	}
}
