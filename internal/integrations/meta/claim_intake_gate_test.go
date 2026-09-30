// MCI01 (meta-claims-intake-v1 §12, tier MOCK/UNIT) for package meta, written by the
// independent test_worker from contracts/meta-claims-intake-v1.md §3 and the frozen
// signatures of docs/delivery/units/meta-intake-core.md, not from the implementation.
//
// Owns: the §3 qualification table (qualifyClaim over all seven kinds and every fail-closed
// rule that is decidable from the projected unit alone) and the ClaimActorKey vectors and key
// strictness. The rules that need the inbox event (NULL occurred_at, unbound object) and the
// cross-domain key separation live in the real-PG flow gates and in
// internal/claims/system_link_gate_test.go; formatting redaction lives in
// internal/integrations/core/secret_gate_test.go.
//
// Non-goals: no database, no network, no expectation read from claim_intake.go. Every id,
// name and text below is a synthetic sentinel.
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

const (
	mciAsset   = "111222333444"
	mciFrom    = "777888999"
	mciPostFB  = "111222333444_555666"
	mciComment = "555666_777001"
)

func mciKeyBytes(b byte) []byte { return bytes.Repeat([]byte{b}, 32) }

// mciUnit is the canonical single-unit payload the consumer holds: one webhook "change".
func mciUnit(field string, value map[string]any) []byte {
	return []byte(canonical(map[string]any{"field": field, "value": value}))
}

func mciPageValue(text string) map[string]any {
	return map[string]any{"item": "comment", "verb": "add", "post_id": mciPostFB, "comment_id": mciComment,
		"from": map[string]any{"id": mciFrom, "name": "SENTINEL-NAME-DO-NOT-STORE"}, "message": text, "created_time": 1700000000}
}

func mciIGValue(text string) map[string]any {
	return map[string]any{"id": "17900000000001", "text": text,
		"from":  map[string]any{"id": mciFrom, "username": "sentinel_user_do_not_store"},
		"media": map[string]any{"id": "17800000000002", "media_product_type": "FEED"}}
}

func mciWith(v map[string]any, edit func(map[string]any)) map[string]any {
	out := map[string]any{}
	for k, x := range v {
		out[k] = x
	}
	edit(out)
	return out
}

func mciWithout(v map[string]any, keys ...string) map[string]any {
	return mciWith(v, func(m map[string]any) {
		for _, k := range keys {
			delete(m, k)
		}
	})
}

func mciSender(id any) map[string]any { return map[string]any{"id": id, "name": "n"} }

// TestMetaClaimsMCI01Qualification is the §3 table: which kinds qualify, and every
// fail-closed rule. A qualified unit yields the object id, comment ref, sender id and the
// pure kw-v1 parse of the text; nothing else about the comment.
func TestMetaClaimsMCI01Qualification(t *testing.T) {
	type tc struct {
		name   string
		object string
		asset  string
		kind   string
		unit   []byte
		ok     bool
		obj    string
		ref    string
		from   string
		text   string // expected parse input when ok
	}
	page := func(name, kind string, v map[string]any, ok bool, text string) tc {
		return tc{name: name, object: "page", asset: mciAsset, kind: kind, unit: mciUnit("feed", v), ok: ok, obj: mciPostFB, ref: mciComment, from: mciFrom, text: text}
	}
	ig := func(name, kind, field string, v map[string]any, ok bool, text string) tc {
		return tc{name: name, object: "instagram", asset: mciAsset, kind: kind, unit: mciUnit(field, v), ok: ok, obj: "17800000000002", ref: "17900000000001", from: mciFrom, text: text}
	}
	long256 := strings.Repeat("x", 256)
	long257 := strings.Repeat("x", 257)
	cases := []tc{
		page("fb add qualifies", "page_comment_add", mciPageValue("A1"), true, "A1"),
		page("fb add with quantity", "page_comment_add", mciPageValue("A1+3"), true, "A1+3"),
		page("fb add non-keyword text still qualifies (NO_MATCH is counted)", "page_comment_add", mciPageValue("hello there, how much?"), true, "hello there, how much?"),
		page("fb add empty text qualifies as NO_MATCH", "page_comment_add", mciPageValue(""), true, ""),
		page("fb add full-width text is parsed by the pure grammar", "page_comment_add", mciPageValue("Ａ１＋２"), true, "Ａ１＋２"),
		page("fb add text of exactly 256 bytes qualifies", "page_comment_add", mciPageValue(long256), true, long256),
		page("fb add text over 256 bytes fails closed", "page_comment_add", mciPageValue(long257), false, ""),
		page("fb edit never qualifies", "page_comment_edit", mciWith(mciPageValue("A1"), func(m map[string]any) { m["verb"] = "edit" }), false, ""),
		page("fb remove never qualifies", "page_comment_remove", mciWith(mciPageValue("A1"), func(m map[string]any) { m["verb"] = "remove" }), false, ""),
		page("fb message kind never qualifies", "page_message", mciPageValue("A1"), false, ""),
		page("fb reply to a comment (parent_id present) fails closed", "page_comment_add", mciWith(mciPageValue("A1"), func(m map[string]any) { m["parent_id"] = "555666_999" }), false, ""),
		page("fb own comment (from.id equals asset) fails closed", "page_comment_add", mciWith(mciPageValue("A1"), func(m map[string]any) { m["from"] = mciSender(mciAsset) }), false, ""),
		page("fb missing from", "page_comment_add", mciWithout(mciPageValue("A1"), "from"), false, ""),
		page("fb missing from.id", "page_comment_add", mciWith(mciPageValue("A1"), func(m map[string]any) { m["from"] = map[string]any{"name": "n"} }), false, ""),
		page("fb empty from.id", "page_comment_add", mciWith(mciPageValue("A1"), func(m map[string]any) { m["from"] = mciSender("") }), false, ""),
		page("fb malformed from.id", "page_comment_add", mciWith(mciPageValue("A1"), func(m map[string]any) { m["from"] = mciSender("12x4") }), false, ""),
		page("fb numeric (non-string) from.id", "page_comment_add", mciWith(mciPageValue("A1"), func(m map[string]any) { m["from"] = mciSender(12345) }), false, ""),
		page("fb missing post_id", "page_comment_add", mciWithout(mciPageValue("A1"), "post_id"), false, ""),
		page("fb malformed post_id", "page_comment_add", mciWith(mciPageValue("A1"), func(m map[string]any) { m["post_id"] = "abc-123" }), false, ""),
		page("fb post_id over 80 chars", "page_comment_add", mciWith(mciPageValue("A1"), func(m map[string]any) { m["post_id"] = strings.Repeat("1", 81) }), false, ""),
		page("fb missing comment_id", "page_comment_add", mciWithout(mciPageValue("A1"), "comment_id"), false, ""),
		page("fb comment_id with letters (T07 accepts, claims must not)", "page_comment_add", mciWith(mciPageValue("A1"), func(m map[string]any) { m["comment_id"] = "abc" }), false, ""),
		page("fb comment_id with dot", "page_comment_add", mciWith(mciPageValue("A1"), func(m map[string]any) { m["comment_id"] = "1.2" }), false, ""),
		page("fb comment_id over 80 chars", "page_comment_add", mciWith(mciPageValue("A1"), func(m map[string]any) { m["comment_id"] = strings.Repeat("7", 81) }), false, ""),

		ig("ig comment qualifies", "instagram_comment", "comments", mciIGValue("A1"), true, "A1"),
		ig("ig live comment qualifies", "instagram_live_comment", "live_comments", mciIGValue("A1+2"), true, "A1+2"),
		ig("ig non-keyword text still qualifies", "instagram_comment", "comments", mciIGValue("nice!"), true, "nice!"),
		ig("ig text over 256 bytes fails closed", "instagram_comment", "comments", mciIGValue(long257), false, ""),
		ig("ig reply to a comment (parent_id present)", "instagram_comment", "comments", mciWith(mciIGValue("A1"), func(m map[string]any) { m["parent_id"] = "17900000000009" }), false, ""),
		ig("ig own comment (from.id equals asset)", "instagram_comment", "comments", mciWith(mciIGValue("A1"), func(m map[string]any) { m["from"] = mciSender(mciAsset) }), false, ""),
		ig("ig missing from.id", "instagram_comment", "comments", mciWith(mciIGValue("A1"), func(m map[string]any) { m["from"] = map[string]any{"username": "u"} }), false, ""),
		ig("ig malformed from.id", "instagram_comment", "comments", mciWith(mciIGValue("A1"), func(m map[string]any) { m["from"] = mciSender("x1") }), false, ""),
		ig("ig missing media", "instagram_comment", "comments", mciWithout(mciIGValue("A1"), "media"), false, ""),
		ig("ig missing media.id", "instagram_comment", "comments", mciWith(mciIGValue("A1"), func(m map[string]any) { m["media"] = map[string]any{"media_product_type": "FEED"} }), false, ""),
		ig("ig malformed media.id", "instagram_comment", "comments", mciWith(mciIGValue("A1"), func(m map[string]any) { m["media"] = map[string]any{"id": "m-1"} }), false, ""),
		ig("ig missing comment id", "instagram_comment", "comments", mciWithout(mciIGValue("A1"), "id"), false, ""),
		ig("ig malformed comment id", "instagram_comment", "comments", mciWith(mciIGValue("A1"), func(m map[string]any) { m["id"] = "ig-1" }), false, ""),
		ig("ig comment id over 80 chars", "instagram_comment", "comments", mciWith(mciIGValue("A1"), func(m map[string]any) { m["id"] = strings.Repeat("9", 81) }), false, ""),
		ig("ig message kind never qualifies", "instagram_message", "comments", mciIGValue("A1"), false, ""),

		{name: "page unit under an instagram kind", object: "page", asset: mciAsset, kind: "instagram_comment", unit: mciUnit("feed", mciPageValue("A1")), ok: false},
		{name: "instagram unit under a page kind", object: "instagram", asset: mciAsset, kind: "page_comment_add", unit: mciUnit("comments", mciIGValue("A1")), ok: false},
		{name: "instagram object with page kind and page unit", object: "instagram", asset: mciAsset, kind: "page_comment_add", unit: mciUnit("feed", mciPageValue("A1")), ok: false},
		{name: "unknown object", object: "threads", asset: mciAsset, kind: "page_comment_add", unit: mciUnit("feed", mciPageValue("A1")), ok: false},
		{name: "empty kind", object: "page", asset: mciAsset, kind: "", unit: mciUnit("feed", mciPageValue("A1")), ok: false},
		{name: "empty unit", object: "page", asset: mciAsset, kind: "page_comment_add", unit: nil, ok: false},
		{name: "not json", object: "page", asset: mciAsset, kind: "page_comment_add", unit: []byte("not json"), ok: false},
		{name: "json array", object: "page", asset: mciAsset, kind: "page_comment_add", unit: []byte(`[1,2]`), ok: false},
		{name: "value not an object", object: "page", asset: mciAsset, kind: "page_comment_add", unit: []byte(`{"field":"feed","value":"x"}`), ok: false},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			got, ok := qualifyClaim(c.object, c.asset, c.kind, c.unit)
			if ok != c.ok {
				t.Fatalf("qualifyClaim ok=%t want=%t", ok, c.ok)
			}
			if !ok {
				if got.ObjectID != "" || got.CommentRef != "" || got.FromID != "" {
					t.Fatal("a non-qualifying unit leaked candidate fields")
				}
				return
			}
			if got.ObjectID != c.obj || got.CommentRef != c.ref || got.FromID != c.from {
				t.Fatalf("candidate ids: object=%q ref=%q from=%q", got.ObjectID, got.CommentRef, got.FromID)
			}
			if want := grammar.Parse(c.text); got.Parsed != want {
				t.Fatalf("parse mismatch: kind=%s keyword=%q qty=%d explicit=%t; want kind=%s keyword=%q qty=%d explicit=%t",
					got.Parsed.Kind, got.Parsed.Keyword, got.Parsed.Quantity, got.Parsed.Explicit, want.Kind, want.Keyword, want.Quantity, want.Explicit)
			}
		})
	}
}

// TestMetaClaimsMCI01QualifyNeverExposesText: the candidate carries a grammar verdict, and
// the grammar result formats without the head (frozen kw-v1 redaction), so text, name and
// username cannot escape through the candidate under any fmt verb.
func TestMetaClaimsMCI01QualifyNeverExposesText(t *testing.T) {
	got, ok := qualifyClaim("page", mciAsset, "page_comment_add", mciUnit("feed", mciPageValue("0912345678")))
	if !ok {
		t.Fatal("phone-shaped text must still qualify (it is only a keyword-shaped head)")
	}
	for _, s := range []string{fmtAll(got.Parsed), fmtAll(&got.Parsed)} {
		if strings.Contains(s, "0912345678") {
			t.Fatal("grammar result printed the comment head")
		}
	}
}

func fmtAll(v any) string {
	var b strings.Builder
	for _, f := range []string{"%v", "%+v", "%#v", "%s"} {
		b.WriteString(fmt.Sprintf(f, v))
		b.WriteByte('\n')
	}
	j, _ := json.Marshal(v)
	b.Write(j)
	return b.String()
}

// TestMetaClaimsMCI01ActorKeyVectors pins ClaimActorKey to the §3 definition, computed here
// independently: hex(HMAC-SHA256(K, JSON(["meta-claim-actor/v1", object, asset, from.id]))).
func TestMetaClaimsMCI01ActorKeyVectors(t *testing.T) {
	raw := make([]byte, 32)
	for i := range raw {
		raw[i] = byte(i + 1)
	}
	k, err := NewClaimsActorKey(raw)
	if err != nil {
		t.Fatalf("valid key refused: %v", err)
	}
	independent := func(key []byte, object, asset, from string) string {
		msg, _ := json.Marshal([]string{"meta-claim-actor/v1", object, asset, from})
		mac := hmac.New(sha256.New, key)
		mac.Write(msg)
		return hex.EncodeToString(mac.Sum(nil))
	}
	cases := [][3]string{{"page", "111", "777"}, {"instagram", "111", "777"}, {"page", "112", "777"}, {"page", "111", "778"},
		{"page", "1", "23"}, {"page", "12", "3"}, {"page", "", ""}, {"page", "111", strings.Repeat("9", 40)}}
	seen := map[string]string{}
	for _, c := range cases {
		got := ClaimActorKey(k, c[0], c[1], c[2])
		if want := independent(raw, c[0], c[1], c[2]); got != want {
			t.Fatalf("ClaimActorKey(%v)=%q want %q", c, got, want)
		}
		if len(got) != 64 || got != strings.ToLower(got) {
			t.Fatalf("not 64 lowercase hex: %q", got)
		}
		id := strings.Join(c[:], "|")
		if prev, dup := seen[got]; dup {
			t.Fatalf("collision between %s and %s", prev, id)
		}
		seen[got] = id
	}
	// The key is copied: mutating the caller's slice after construction changes nothing.
	before := ClaimActorKey(k, "page", "111", "777")
	for i := range raw {
		raw[i] = 0xEE
	}
	if ClaimActorKey(k, "page", "111", "777") != before {
		t.Fatal("NewClaimsActorKey kept a reference to the caller's slice")
	}
	// A different key gives an unrelated actor for the same person.
	other, _ := NewClaimsActorKey(mciKeyBytes(0x42))
	if ClaimActorKey(other, "page", "111", "777") == before {
		t.Fatal("two keys produced the same actor")
	}
	// Same platform person on the same asset and object is one actor regardless of app: the
	// function has no app parameter, so determinism is the whole property.
	if ClaimActorKey(k, "page", "111", "777") != ClaimActorKey(k, "page", "111", "777") {
		t.Fatal("ClaimActorKey is not deterministic")
	}
}

func TestMetaClaimsMCI01ActorKeyStrictness(t *testing.T) {
	for _, bad := range [][]byte{nil, {}, mciKeyBytes(1)[:31], append(mciKeyBytes(1), 1), make([]byte, 32)} {
		if _, err := NewClaimsActorKey(bad); err == nil {
			t.Fatalf("NewClaimsActorKey accepted a %d-byte key (all-zero=%t)", len(bad), bytes.Equal(bad, make([]byte, len(bad))))
		}
	}
	// A zero-value key never produces an actor (an unconfigured worker must not stage).
	var zero ClaimsActorKey
	if got := ClaimActorKey(zero, "page", "111", "777"); got != "" {
		t.Fatalf("zero key produced %q", got)
	}
}

func TestMetaClaimsMCI01LoadClaimsActorKey(t *testing.T) {
	env := func(v string, set bool) func(string) string {
		return func(name string) string {
			if name == "COMMERCE_CLAIMS_ACTOR_KEY" && set {
				return v
			}
			return ""
		}
	}
	// Unset and empty both mean "staging off", without error.
	for _, set := range []bool{false, true} {
		k, on, err := LoadClaimsActorKey(env("", set))
		if err != nil || on || ClaimActorKey(k, "page", "1", "2") != "" {
			t.Fatalf("empty (set=%t): on=%t err=%v", set, on, err)
		}
	}
	raw := mciKeyBytes(0x5A)
	k, on, err := LoadClaimsActorKey(env(base64.StdEncoding.EncodeToString(raw), true))
	if err != nil || !on {
		t.Fatalf("valid key: on=%t err=%v", on, err)
	}
	want, _ := NewClaimsActorKey(raw)
	if ClaimActorKey(k, "page", "1", "2") != ClaimActorKey(want, "page", "1", "2") {
		t.Fatal("loaded key differs from NewClaimsActorKey of the same bytes")
	}
	urlSafe := bytes.Repeat([]byte{0xfb}, 32) // std base64 contains '+' and '/' characters
	if std := base64.StdEncoding.EncodeToString(urlSafe); !strings.ContainsAny(std, "+/") {
		t.Fatal("test vector does not exercise the std alphabet")
	}
	for name, v := range map[string]string{
		"not base64":     "!!!!not-base64!!!!",
		"31 bytes":       base64.StdEncoding.EncodeToString(raw[:31]),
		"33 bytes":       base64.StdEncoding.EncodeToString(append(mciKeyBytes(1), 1)),
		"all zero":       base64.StdEncoding.EncodeToString(make([]byte, 32)),
		"url alphabet":   base64.URLEncoding.EncodeToString(urlSafe),
		"raw no padding": base64.RawStdEncoding.EncodeToString(raw),
	} {
		_, on, err := LoadClaimsActorKey(env(v, true))
		if err == nil || on {
			t.Fatalf("%s: accepted (on=%t)", name, on)
		}
		if name == "not base64" || name == "31 bytes" || name == "33 bytes" {
			if !errors.Is(err, ErrRuntimeConfig) {
				t.Fatalf("%s: error is not ErrRuntimeConfig: %v", name, err)
			}
		}
	}
}
