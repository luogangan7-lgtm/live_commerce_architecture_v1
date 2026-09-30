package meta

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"livecommerce/internal/claims/grammar"
)

// Pure qualification, actor-key and redaction checks for the claims staging edge
// (meta-claims-intake-v1 §3; gate MCI01 slice). All ids and texts are synthetic.

const (
	qAsset = "100200300"
	qFrom  = "555666777"
)

func pageUnit(mutate func(value map[string]any)) []byte {
	value := map[string]any{"item": "comment", "verb": "add", "post_id": "100200300_9001", "comment_id": "9001_42",
		"from": map[string]any{"id": qFrom, "name": "Synthetic Person"}, "message": "A1+2", "created_time": json.Number("1790000000")}
	if mutate != nil {
		mutate(value)
	}
	return canonical(map[string]any{"field": "feed", "value": value})
}

func igUnit(field string, mutate func(value map[string]any)) []byte {
	value := map[string]any{"id": "17900000000000001", "text": "a1", "from": map[string]any{"id": qFrom, "username": "synthetic"},
		"media": map[string]any{"id": "17800000000000002", "media_product_type": "FEED"}}
	if mutate != nil {
		mutate(value)
	}
	return canonical(map[string]any{"field": field, "value": value})
}

func TestQualifyClaimTable(t *testing.T) {
	type tc struct {
		name, object, kind string
		unit               []byte
		ok                 bool
	}
	cases := []tc{
		{"page add", "page", "page_comment_add", pageUnit(nil), true},
		{"page top-level parent equals post", "page", "page_comment_add", pageUnit(func(v map[string]any) { v["parent_id"] = "100200300_9001" }), true},
		{"page empty parent", "page", "page_comment_add", pageUnit(func(v map[string]any) { v["parent_id"] = "" }), true},
		{"page edit never", "page", "page_comment_edit", pageUnit(func(v map[string]any) { v["verb"] = "edit" }), false},
		{"page remove never", "page", "page_comment_remove", pageUnit(func(v map[string]any) { v["verb"] = "remove" }), false},
		{"page reply", "page", "page_comment_add", pageUnit(func(v map[string]any) { v["parent_id"] = "9001_41" }), false},
		{"page non-string parent", "page", "page_comment_add", pageUnit(func(v map[string]any) { v["parent_id"] = nil }), false},
		{"page own comment", "page", "page_comment_add", pageUnit(func(v map[string]any) { v["from"] = map[string]any{"id": qAsset} }), false},
		{"page missing from.id", "page", "page_comment_add", pageUnit(func(v map[string]any) { v["from"] = map[string]any{"name": "x"} }), false},
		{"page no from", "page", "page_comment_add", pageUnit(func(v map[string]any) { delete(v, "from") }), false},
		{"page non-numeric from.id", "page", "page_comment_add", pageUnit(func(v map[string]any) { v["from"] = map[string]any{"id": "abc"} }), false},
		{"page missing post_id", "page", "page_comment_add", pageUnit(func(v map[string]any) { delete(v, "post_id") }), false},
		{"page malformed post_id", "page", "page_comment_add", pageUnit(func(v map[string]any) { v["post_id"] = "12-34" }), false},
		{"page missing comment id", "page", "page_comment_add", pageUnit(func(v map[string]any) { delete(v, "comment_id") }), false},
		{"page malformed comment id", "page", "page_comment_add", pageUnit(func(v map[string]any) { v["comment_id"] = "c.1" }), false},
		{"page text 256 bytes", "page", "page_comment_add", pageUnit(func(v map[string]any) { v["message"] = "A1" + strings.Repeat(" ", 254) }), true},
		{"page text 257 bytes", "page", "page_comment_add", pageUnit(func(v map[string]any) { v["message"] = "A1" + strings.Repeat(" ", 255) }), false},
		{"page non-string text", "page", "page_comment_add", pageUnit(func(v map[string]any) { v["message"] = json.Number("7") }), false},
		{"page no text is NO_MATCH", "page", "page_comment_add", pageUnit(func(v map[string]any) { delete(v, "message") }), true},
		{"page kind on instagram object", "instagram", "page_comment_add", pageUnit(nil), false},
		{"page wrong item", "page", "page_comment_add", pageUnit(func(v map[string]any) { v["item"] = "post" }), false},
		{"ig comment", "instagram", "instagram_comment", igUnit("comments", nil), true},
		{"ig live comment", "instagram", "instagram_live_comment", igUnit("live_comments", nil), true},
		{"ig field disagrees with kind", "instagram", "instagram_comment", igUnit("live_comments", nil), false},
		{"ig kind on page object", "page", "instagram_comment", igUnit("comments", nil), false},
		{"ig reply", "instagram", "instagram_comment", igUnit("comments", func(v map[string]any) { v["parent_id"] = "17900000000000000" }), false},
		{"ig own comment", "instagram", "instagram_comment", igUnit("comments", func(v map[string]any) { v["from"] = map[string]any{"id": qAsset} }), false},
		{"ig missing media", "instagram", "instagram_comment", igUnit("comments", func(v map[string]any) { delete(v, "media") }), false},
		{"ig missing media id", "instagram", "instagram_comment", igUnit("comments", func(v map[string]any) { v["media"] = map[string]any{} }), false},
		{"ig missing id", "instagram", "instagram_comment", igUnit("comments", func(v map[string]any) { delete(v, "id") }), false},
		{"quarantine kind", "page", "quarantine", pageUnit(nil), false},
		{"message kind", "page", "page_message", pageUnit(nil), false},
		{"not json", "page", "page_comment_add", []byte("{"), false},
		{"no value", "page", "page_comment_add", []byte(`{"field":"feed"}`), false},
	}
	for _, c := range cases {
		got, ok := qualifyClaim(c.object, qAsset, c.kind, c.unit)
		if ok != c.ok {
			t.Errorf("%s: qualifies=%t, want %t", c.name, ok, c.ok)
		}
		if !ok && got != (claimCandidate{}) {
			t.Errorf("%s: a rejected unit returned data", c.name)
		}
	}
	if _, ok := qualifyClaim("page", "not-digits", "page_comment_add", pageUnit(nil)); ok {
		t.Error("malformed asset id qualifies")
	}
	cand, ok := qualifyClaim("page", qAsset, "page_comment_add", pageUnit(nil))
	if !ok || cand.ObjectID != "100200300_9001" || cand.CommentRef != "9001_42" || cand.FromID != qFrom ||
		cand.Parsed.Kind != grammar.Match || cand.Parsed.Keyword != "A1" || cand.Parsed.Quantity != 2 || !cand.Parsed.Explicit {
		t.Fatalf("page candidate: %+v", cand)
	}
	cand, ok = qualifyClaim("instagram", qAsset, "instagram_live_comment", igUnit("live_comments", nil))
	if !ok || cand.ObjectID != "17800000000000002" || cand.CommentRef != "17900000000000001" || cand.Parsed.Keyword != "A1" || cand.Parsed.Quantity != 1 {
		t.Fatalf("instagram candidate: %+v", cand)
	}
	// The candidate never prints its content under any verb.
	for _, rendered := range []string{fmt.Sprint(cand), fmt.Sprintf("%+v", cand), fmt.Sprintf("%#v", cand)} {
		if strings.Contains(rendered, qFrom) || strings.Contains(rendered, cand.CommentRef) || !strings.Contains(rendered, "redacted") {
			t.Fatalf("candidate leaked: %s", rendered)
		}
	}
	if encoded, _ := json.Marshal(cand); strings.Contains(string(encoded), qFrom) {
		t.Fatal("candidate JSON leaked the sender id")
	}
}

func actorKeyBytes(b byte) []byte { return bytes.Repeat([]byte{b}, 32) }

func TestClaimActorKeyVectors(t *testing.T) {
	raw := actorKeyBytes(7)
	k, err := NewClaimsActorKey(raw)
	if err != nil {
		t.Fatal(err)
	}
	mac := hmac.New(sha256.New, raw)
	mac.Write([]byte(`["meta-claim-actor/v1","page","100200300","555666777"]`))
	want := hex.EncodeToString(mac.Sum(nil))
	if got := ClaimActorKey(k, "page", qAsset, qFrom); got != want || len(got) != 64 {
		t.Fatalf("actor key %q, want independent HMAC %q", got, want)
	}
	// Per object, asset and sender they differ; an app id is in no input.
	seen := map[string]bool{want: true}
	for _, other := range [][3]string{{"instagram", qAsset, qFrom}, {"page", "100200301", qFrom}, {"page", qAsset, "555666778"}} {
		v := ClaimActorKey(k, other[0], other[1], other[2])
		if seen[v] {
			t.Fatalf("collision for %v", other)
		}
		seen[v] = true
	}
	// Domain separation with EQUAL key bytes: the unkeyed social peer key, the manual label MAC
	// shape and the reply-link derivation shape give unrelated outputs.
	peer := tupleHash("meta-social-peer/v1", "1", "page", qAsset, qFrom)
	labelMAC := hmac.New(sha256.New, raw)
	labelMAC.Write([]byte("claims.manual-label.v1|" + qAsset + "|" + qFrom))
	linkMAC := hmac.New(sha256.New, raw)
	linkMAC.Write(canonical([]string{"meta-claim-link/v1", qAsset, qFrom, "b", "o"}))
	for name, other := range map[string]string{"peer key": peer, "label MAC": hex.EncodeToString(labelMAC.Sum(nil)), "link derivation": hex.EncodeToString(linkMAC.Sum(nil))} {
		if other == want {
			t.Fatalf("actor key collides with %s", name)
		}
	}
	if ClaimActorKey(ClaimsActorKey{}, "page", qAsset, qFrom) != "" {
		t.Fatal("zero key must derive no actor")
	}
}

func TestClaimsActorKeyConstructionAndLoad(t *testing.T) {
	for _, bad := range [][]byte{nil, {}, actorKeyBytes(1)[:31], append(actorKeyBytes(1), 1), make([]byte, 32)} {
		if _, err := NewClaimsActorKey(bad); !errors.Is(err, ErrRuntimeConfig) {
			t.Errorf("key of %d bytes accepted", len(bad))
		}
	}
	src := actorKeyBytes(3)
	k, err := NewClaimsActorKey(src)
	if err != nil {
		t.Fatal(err)
	}
	src[0] = 9 // the constructor copies
	if ClaimActorKey(k, "page", qAsset, qFrom) == ClaimActorKey(ClaimsActorKey{key: [32]byte{9}, set: true}, "page", qAsset, qFrom) {
		t.Fatal("constructor aliases the caller's slice")
	}
	good := base64.StdEncoding.EncodeToString(actorKeyBytes(3))
	env := map[string]string{}
	get := func(name string) string { return env[name] }
	if _, on, err := LoadClaimsActorKey(get); err != nil || on {
		t.Fatalf("unset must be off without error: %v %v", on, err)
	}
	env["COMMERCE_CLAIMS_ACTOR_KEY"] = good
	loaded, on, err := LoadClaimsActorKey(get)
	if err != nil || !on || ClaimActorKey(loaded, "page", qAsset, qFrom) != ClaimActorKey(mustKey(t, actorKeyBytes(3)), "page", qAsset, qFrom) {
		t.Fatalf("valid key: %v %v", on, err)
	}
	for _, bad := range []string{"%%%", good[:40], " " + good, good + "=", strings.ReplaceAll(good, "A", "-"),
		base64.StdEncoding.EncodeToString(actorKeyBytes(3)[:31]), base64.StdEncoding.EncodeToString(make([]byte, 32)),
		base64.RawStdEncoding.EncodeToString(actorKeyBytes(3))} {
		env["COMMERCE_CLAIMS_ACTOR_KEY"] = bad
		if _, on, err := LoadClaimsActorKey(get); !errors.Is(err, ErrRuntimeConfig) || on {
			t.Errorf("malformed key %q accepted (on=%t err=%v)", bad, on, err)
		}
	}
	if _, _, err := LoadClaimsActorKey(nil); !errors.Is(err, ErrRuntimeConfig) {
		t.Fatal("nil getenv accepted")
	}
	for _, rendered := range []string{fmt.Sprint(loaded), fmt.Sprintf("%+v", loaded), fmt.Sprintf("%#v", loaded), fmt.Sprintf("%s", loaded), fmt.Sprintf("%x", loaded)} {
		if rendered != "[redacted]" {
			t.Fatalf("key rendered %q", rendered)
		}
	}
	if encoded, err := json.Marshal(struct{ K ClaimsActorKey }{loaded}); err != nil || strings.Contains(string(encoded), good) || !strings.Contains(string(encoded), "redacted") {
		t.Fatalf("key JSON: %s %v", encoded, err)
	}
}

func mustKey(t *testing.T, raw []byte) ClaimsActorKey {
	t.Helper()
	k, err := NewClaimsActorKey(raw)
	if err != nil {
		t.Fatal(err)
	}
	return k
}
