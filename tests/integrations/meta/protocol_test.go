package meta_test

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"livecommerce/internal/integrations/meta"
)

const (
	testSecret  = "synthetic-meta-secret-not-a-real-key"
	testToken   = "synthetic-meta-verify-token"
	pageBody    = `{"object":"page","entry":[{"id":"100","time":1710000000,"changes":[{"field":"feed","value":{"item":"comment","verb":"add","comment_id":"C-1"}}]}]}`
	messageBody = `{"object":"page","entry":[{"id":"100","time":1710000000,"messaging":[{"sender":{"id":"200"},"recipient":{"id":"100"},"timestamp":1710000000123,"message":{"mid":"m-1","text":"hello"}}]}]}`
)

func config(object string) meta.Config {
	return meta.Config{AppID: "123456", Object: object, AppSecret: testSecret, VerifyToken: testToken}
}

func verifier(t *testing.T, c meta.Config) *meta.Verifier {
	t.Helper()
	v, err := meta.NewVerifier(c)
	if err != nil {
		t.Fatalf("new verifier: %v", err)
	}
	return v
}

func signature(secret string, raw []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write(raw)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

func verify(t *testing.T, c meta.Config, raw string) meta.Batch {
	t.Helper()
	b, err := verifier(t, c).Verify([]byte(raw), signature(c.AppSecret, []byte(raw)))
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	return b
}

func handler(t *testing.T, c meta.Config, commit func(context.Context, meta.Batch) error) http.Handler {
	t.Helper()
	h, err := meta.NewHandler(verifier(t, c), commit)
	if err != nil {
		t.Fatalf("new handler: %v", err)
	}
	return h
}

func post(h http.Handler, raw string, sig string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(http.MethodPost, "/webhook", strings.NewReader(raw))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("X-Hub-Signature-256", sig)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

type writeSpy struct {
	*httptest.ResponseRecorder
	writes atomic.Int32
}

func (s *writeSpy) WriteHeader(status int) {
	s.writes.Add(1)
	s.ResponseRecorder.WriteHeader(status)
}

func (s *writeSpy) Write(body []byte) (int, error) {
	s.writes.Add(1)
	return s.ResponseRecorder.Write(body)
}

func requireDigest(t *testing.T, value string) {
	t.Helper()
	if len(value) != 64 || value != strings.ToLower(value) {
		t.Fatalf("invalid lowercase digest %q", value)
	}
	if _, err := hex.DecodeString(value); err != nil {
		t.Fatalf("invalid hex digest: %v", err)
	}
}

func requireQuarantine(t *testing.T, e meta.Event) {
	t.Helper()
	if e.QuarantineReason == "" || len(e.Payload) == 0 {
		t.Fatalf("quarantine lost reason or payload: kind=%q", e.Kind)
	}
	requireDigest(t, e.Key)
	requireDigest(t, e.PayloadHash)
	requirePayloadHash(t, e)
}

func requirePayloadHash(t *testing.T, e meta.Event) {
	t.Helper()
	sum := sha256.Sum256(e.Payload)
	if e.PayloadHash != hex.EncodeToString(sum[:]) {
		t.Fatal("payload hash does not match canonical payload bytes")
	}
}

func TestMWP01RawSignatureUnicodeAndBodyHash(t *testing.T) {
	c := config("page")
	escaped := strings.Replace(pageBody, `"C-1"`, `"\u0043-1"`, 1)
	if escaped == pageBody {
		t.Fatal("fixture did not change bytes")
	}
	v := verifier(t, c)
	first, err := v.Verify([]byte(escaped), signature(c.AppSecret, []byte(escaped)))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := v.Verify([]byte(pageBody), signature(c.AppSecret, []byte(escaped))); err == nil {
		t.Fatal("semantic-equivalent bytes accepted under stale signature")
	}
	second := verify(t, c, pageBody)
	if len(first.Events) != 1 || len(second.Events) != 1 || first.Events[0].Key != second.Events[0].Key {
		t.Fatal("Unicode escape changed canonical event identity")
	}
	if first.BodyHash == second.BodyHash {
		t.Fatal("different raw bodies had one body hash")
	}
	sum := sha256.Sum256([]byte(escaped))
	if first.BodyHash != hex.EncodeToString(sum[:]) {
		t.Fatal("body hash was not over exact received bytes")
	}
	if _, err := v.Verify([]byte(pageBody), signature("wrong-but-long-secret-for-test", []byte(pageBody))); err == nil {
		t.Fatal("wrong app secret accepted")
	}
	if _, err := new(meta.Verifier).Verify([]byte(pageBody), signature("", []byte(pageBody))); err == nil {
		t.Fatal("zero-value verifier accepted empty-secret signature")
	}
	if w := post(handler(t, c, func(context.Context, meta.Batch) error { t.Fatal("callback on bad signature"); return nil }), "{", signature(c.AppSecret, []byte("{}"))); w.Code != http.StatusForbidden {
		t.Fatalf("signature must precede malformed JSON: status %d", w.Code)
	}
}

func TestMWP01ConfigIsolationAndRedaction(t *testing.T) {
	good := config("page")
	for _, tc := range []struct {
		name string
		edit func(*meta.Config)
	}{
		{"empty app", func(c *meta.Config) { c.AppID = "" }},
		{"unicode digit", func(c *meta.Config) { c.AppID = "１２３" }},
		{"app too long", func(c *meta.Config) { c.AppID = strings.Repeat("1", 41) }},
		{"object case", func(c *meta.Config) { c.Object = "Page" }},
		{"short secret", func(c *meta.Config) { c.AppSecret = "short" }},
		{"control secret", func(c *meta.Config) { c.AppSecret = strings.Repeat("s", 16) + "\n" }},
		{"long token", func(c *meta.Config) { c.VerifyToken = strings.Repeat("t", 513) }},
		{"control token", func(c *meta.Config) { c.VerifyToken = strings.Repeat("t", 16) + "\x00" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := good
			tc.edit(&c)
			if _, err := meta.NewVerifier(c); err == nil {
				t.Fatal("unsafe config accepted")
			}
		})
	}
	for _, c := range []meta.Config{
		{AppID: "1", Object: "page", AppSecret: strings.Repeat("s", 16), VerifyToken: strings.Repeat("t", 16)},
		{AppID: strings.Repeat("9", 40), Object: "instagram", AppSecret: strings.Repeat("s", 512), VerifyToken: strings.Repeat("t", 512)},
	} {
		_ = verifier(t, c)
	}
	b := verify(t, good, pageBody)
	if b.AppID != good.AppID || b.Object != good.Object {
		t.Fatal("batch authority not configured app/object")
	}
	for _, value := range []any{good, verifier(t, good), b, b.Events[0]} {
		for _, formatted := range []string{fmt.Sprintf("%v", value), fmt.Sprintf("%+v", value), fmt.Sprintf("%#v", value)} {
			for _, forbidden := range []string{testSecret, testToken, "C-1"} {
				if strings.Contains(formatted, forbidden) {
					t.Fatalf("formatting leaked %q", forbidden)
				}
			}
		}
		encoded, err := json.Marshal(value)
		if err != nil {
			t.Fatalf("redacted JSON marshal: %v", err)
		}
		for _, forbidden := range []string{testSecret, testToken, "C-1"} {
			if strings.Contains(string(encoded), forbidden) {
				t.Fatalf("JSON leaked %q", forbidden)
			}
		}
	}
	mismatch := verify(t, good, strings.Replace(pageBody, `"object":"page"`, `"object":"instagram"`, 1))
	if len(mismatch.Events) != 1 {
		t.Fatalf("object mismatch yielded %d events", len(mismatch.Events))
	}
	requireQuarantine(t, mismatch.Events[0])
	if mismatch.Events[0].Kind == "page_comment_add" {
		t.Fatal("foreign object was normalized")
	}
}

func TestMWP02StrictJSONAndPrecision(t *testing.T) {
	c := config("page")
	v := verifier(t, c)
	for _, tc := range []struct{ name, raw string }{
		{"duplicate root", `{"object":"page","object":"page","entry":[]}`},
		{"escaped root alias", `{"object":"page","\u006fbject":"page","entry":[]}`},
		{"nested duplicate", `{"object":"page","entry":[{"id":"100","changes":[{"field":"feed","value":{"item":"comment","item":"comment","verb":"add","comment_id":"C-1"}}]}]}`},
		{"nested escaped alias", `{"object":"page","entry":[{"id":"100","changes":[{"field":"feed","value":{"comment_id":"C-1","\u0063omment_id":"C-2","item":"comment","verb":"add"}}]}]}`},
		{"message escaped alias", strings.Replace(messageBody, `"mid":"m-1"`, `"mid":"m-1","\u006did":"m-2"`, 1)},
		{"trailing value", pageBody + `{}`},
		{"nonobject root", `[]`},
		{"invalid JSON", `{"object":`},
		{"invalid UTF-8", string([]byte{'{', '"', 'x', '"', ':', '"', 0xff, '"', '}'})},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := v.Verify([]byte(tc.raw), signature(c.AppSecret, []byte(tc.raw))); err == nil {
				t.Fatal("malformed signed JSON admitted")
			}
		})
	}
	precise := strings.Replace(pageBody, `"comment_id":"C-1"`, `"comment_id":"C-1","large":9007199254740993`, 1)
	b := verify(t, c, precise)
	if len(b.Events) != 1 || !strings.Contains(string(b.Events[0].Payload), `9007199254740993`) {
		t.Fatal("provider number lost precision")
	}
	callbackCount := 0
	h := handler(t, c, func(context.Context, meta.Batch) error { callbackCount++; return nil })
	for _, tc := range []struct{ name, text string }{
		{"lone high", `"\ud800"`},
		{"lone low", `"\udc00"`},
		{"two highs", `"\ud800\ud800"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw := strings.Replace(messageBody, `"hello"`, tc.text, 1)
			if _, err := v.Verify([]byte(raw), signature(c.AppSecret, []byte(raw))); err == nil {
				t.Error("unpaired UTF-16 escape admitted by Verifier")
			}
			w := post(h, raw, signature(c.AppSecret, []byte(raw)))
			if w.Code == 200 || w.Header().Get("Cache-Control") != "no-store" {
				t.Fatalf("unpaired UTF-16 escape HTTP status=%d", w.Code)
			}
		})
	}
	if callbackCount != 0 {
		t.Fatalf("unpaired UTF-16 reached callback %d times", callbackCount)
	}
	pair := verify(t, c, strings.Replace(messageBody, `"hello"`, `"\ud83d\ude00"`, 1)).Events[0]
	literal := verify(t, c, strings.Replace(messageBody, `"hello"`, `"😀"`, 1)).Events[0]
	if pair.Kind != "page_message" || pair.Key != literal.Key || pair.PayloadHash != literal.PayloadHash || !strings.Contains(string(pair.Payload), "😀") {
		t.Fatal("valid UTF-16 pair did not canonicalize to literal Unicode")
	}
	replacementEscape := verify(t, c, strings.Replace(messageBody, `"hello"`, `"\ufffd"`, 1)).Events[0]
	replacementLiteral := verify(t, c, strings.Replace(messageBody, `"hello"`, `"�"`, 1)).Events[0]
	if replacementEscape.PayloadHash != replacementLiteral.PayloadHash || replacementEscape.PayloadHash == pair.PayloadHash {
		t.Fatal("valid replacement character confused with emoji or its literal encoding")
	}
	literalBackslash := verify(t, c, strings.Replace(messageBody, `"hello"`, `"\\ud800"`, 1)).Events[0]
	if literalBackslash.Kind != "page_message" || literalBackslash.PayloadHash == replacementLiteral.PayloadHash {
		t.Fatal("literal backslash-u text rejected or collapsed to replacement character")
	}
}

func TestMWP02DepthBodyAndEmittedEventBounds(t *testing.T) {
	c := config("page")
	for _, tc := range []struct {
		depth int
		valid bool
	}{{63, true}, {64, false}} {
		raw := `{"object":"page","entry":[],"unknown":` + strings.Repeat("[", tc.depth) + `0` + strings.Repeat("]", tc.depth) + `}`
		_, err := verifier(t, c).Verify([]byte(raw), signature(c.AppSecret, []byte(raw)))
		if (err == nil) != tc.valid {
			t.Fatalf("root plus %d arrays: err %v", tc.depth, err)
		}
	}
	unit := `{"field":"feed","value":{"item":"comment","verb":"add","comment_id":"C-1"}}`
	makeBatch := func(n int, extra bool) string {
		entry := `{"id":"100","changes":[` + strings.TrimSuffix(strings.Repeat(unit+",", n), ",") + `]`
		if extra {
			entry += `,"standby":[]`
		}
		return `{"object":"page","entry":[` + entry + `}]}`
	}
	for _, tc := range []struct {
		n            int
		extra, valid bool
	}{{1000, false, true}, {1001, false, false}, {999, true, true}, {1000, true, false}} {
		raw := makeBatch(tc.n, tc.extra)
		b, err := verifier(t, c).Verify([]byte(raw), signature(c.AppSecret, []byte(raw)))
		if tc.valid {
			if err != nil || len(b.Events) != 1000 {
				t.Fatalf("%d units extra=%t: events=%d err=%v", tc.n, tc.extra, len(b.Events), err)
			}
		} else if err == nil {
			t.Fatalf("%d units extra=%t admitted", tc.n, tc.extra)
		}
	}
	eventCalls := 0
	eventHandler := handler(t, c, func(context.Context, meta.Batch) error { eventCalls++; return nil })
	for _, raw := range []string{makeBatch(1001, false), makeBatch(1000, true)} {
		w := post(eventHandler, raw, signature(c.AppSecret, []byte(raw)))
		if w.Code != http.StatusRequestEntityTooLarge || w.Header().Get("Cache-Control") != "no-store" {
			t.Fatalf("event overflow HTTP status=%d", w.Code)
		}
	}
	if eventCalls != 0 {
		t.Fatalf("partial oversized batch reached commit %d times", eventCalls)
	}
	const max = 1 << 20
	prefix := `{"object":"page","entry":[],"pad":"`
	suffix := `"}`
	makeSized := func(size int) string { return prefix + strings.Repeat("a", size-len(prefix)-len(suffix)) + suffix }
	called := 0
	h := handler(t, c, func(context.Context, meta.Batch) error { called++; return nil })
	for _, tc := range []struct{ size, status int }{{max, 200}, {max + 1, 413}} {
		raw := makeSized(tc.size)
		if len(raw) != tc.size {
			t.Fatal("bad size fixture")
		}
		w := post(h, raw, signature(c.AppSecret, []byte(raw)))
		if w.Code != tc.status {
			t.Fatalf("size %d: status %d", tc.size, w.Code)
		}
	}
	if called != 1 {
		t.Fatalf("over-limit callback count %d", called)
	}
}

func TestMWP03KnownSiblingsAndUnknownFields(t *testing.T) {
	c := config("page")
	raw := `{"object":"page","entry":[{"id":"100","time":1710000000,"changes":[{"field":"feed","value":{"item":"comment","verb":"add","comment_id":"C-1"}}],"messaging":[{"sender":{"id":"200"},"recipient":{"id":"100"},"message":{"mid":"m-1"}}],"standby":[{"message":{"mid":"unsupplied"}}]}]}`
	b := verify(t, c, raw)
	if len(b.Events) != 3 {
		t.Fatalf("known and unknown siblings emitted %d events", len(b.Events))
	}
	kinds := map[string]int{}
	for _, e := range b.Events {
		if e.QuarantineReason != "" {
			requireQuarantine(t, e)
			if !strings.Contains(string(e.Payload), "unsupplied") {
				t.Fatal("unknown sibling not preserved in quarantine")
			}
			kinds["quarantine"]++
		} else {
			kinds[e.Kind]++
		}
	}
	if kinds["page_comment_add"] != 1 || kinds["page_message"] != 1 || kinds["quarantine"] != 1 {
		t.Fatalf("missing event-bearing sibling: %v", kinds)
	}
	rootExtra := strings.Replace(pageBody, `"entry":`, `"extra":{"event":1},"entry":`, 1)
	if got := verify(t, c, rootExtra).Events; len(got) != 2 || !(got[0].Kind == "page_comment_add" && got[1].QuarantineReason != "" || got[1].Kind == "page_comment_add" && got[0].QuarantineReason != "") {
		t.Fatalf("unknown root sibling displaced known event: %d events", len(got))
	}
	caseVariant := strings.Replace(pageBody, `"id":"100"`, `"id":"100","ID":"999"`, 1)
	got := verify(t, c, caseVariant).Events
	if len(got) != 2 || !(got[0].Kind == "page_comment_add" && got[1].QuarantineReason != "" || got[1].Kind == "page_comment_add" && got[0].QuarantineReason != "") {
		t.Fatalf("case-variant entry key displaced known event: %d events", len(got))
	}
	multi := `{"object":"page","entry":[{"id":"100","changes":[{"field":"feed","value":{"item":"comment","verb":"add","comment_id":"C-1"}}]},{"id":"101","messaging":[{"sender":{"id":"200"},"recipient":{"id":"101"},"message":{"mid":"m-1"}}]}]}`
	entries := verify(t, c, multi).Events
	if len(entries) != 2 || entries[0].AssetID == entries[1].AssetID || entries[0].Kind != "page_comment_add" || entries[1].Kind != "page_message" {
		t.Fatalf("multiple entry[] units not independently accounted: %d events", len(entries))
	}
	t.Run("linear quarantine payload near body limit", func(t *testing.T) {
		unit := `{"field":"unsupported","value":{"blob":"` + strings.Repeat("x", 700) + `"}}`
		raw := `{"object":"page","entry":[{"id":"100","time":1710000000,"changes":[` + strings.TrimSuffix(strings.Repeat(unit+",", 1000), ",") + `]}]}`
		if len(raw) >= 1<<20 {
			t.Fatalf("input fixture exceeded 1 MiB: %d", len(raw))
		}
		batch := verify(t, c, raw)
		if len(batch.Events) != 1000 {
			t.Fatalf("unknown unit count=%d", len(batch.Events))
		}
		total := 0
		for _, event := range batch.Events {
			requireQuarantine(t, event)
			total += len(event.Payload)
		}
		if total >= 10<<20 {
			t.Fatalf("quadratic quarantine expansion: input=%d emitted=%d", len(raw), total)
		}
	})
	t.Run("huge invalid entry time is not copied per child", func(t *testing.T) {
		unit := `{"field":"unsupported"}`
		raw := `{"object":"page","entry":[{"id":"100","time":"` + strings.Repeat("T", 300000) + `","changes":[` + strings.TrimSuffix(strings.Repeat(unit+",", 100), ",") + `]}]}`
		if len(raw) >= 1<<20 {
			t.Fatalf("input fixture exceeded 1 MiB: %d", len(raw))
		}
		batch := verify(t, c, raw)
		if len(batch.Events) < 100 {
			t.Fatalf("unknown children vanished: %d", len(batch.Events))
		}
		total, whole := 0, 0
		for _, event := range batch.Events {
			requireQuarantine(t, event)
			total += len(event.Payload)
			if strings.Contains(string(event.Payload), strings.Repeat("T", 100)) {
				whole++
			}
		}
		if whole != 1 || total >= 10<<20 {
			t.Fatalf("invalid time expansion: whole=%d input=%d emitted=%d", whole, len(raw), total)
		}
	})
}

func TestMWP03PageInstagramAndQuarantineCases(t *testing.T) {
	for _, tc := range []struct {
		name, object, raw, kind string
		quarantine              bool
	}{
		{"Page add", "page", pageBody, "page_comment_add", false},
		{"Page numeric comment", "page", strings.Replace(pageBody, `"C-1"`, `123`, 1), "", true},
		{"IG comment", "instagram", `{"object":"instagram","entry":[{"id":"100","changes":[{"field":"comments","value":{"id":"123"}}]}]}`, "instagram_comment", false},
		{"IG live comment", "instagram", `{"object":"instagram","entry":[{"id":"100","changes":[{"field":"live_comments","value":{"id":"123"}}]}]}`, "instagram_live_comment", false},
		{"IG numeric ID", "instagram", `{"object":"instagram","entry":[{"id":"100","changes":[{"field":"comments","value":{"id":123}}]}]}`, "", true},
		{"Page unsupported IG field", "page", `{"object":"page","entry":[{"id":"100","changes":[{"field":"comments","value":{"id":"123"}}]}]}`, "", true},
		{"echo true", "page", strings.Replace(messageBody, `"message":{"mid":`, `"message":{"is_echo":true,"mid":`, 1), "", true},
		{"echo null", "page", strings.Replace(messageBody, `"message":{"mid":`, `"message":{"is_echo":null,"mid":`, 1), "", true},
		{"wrong recipient", "page", strings.Replace(messageBody, `"recipient":{"id":"100"}`, `"recipient":{"id":"300"}`, 1), "", true},
		{"missing sender", "page", strings.Replace(messageBody, `"sender":{"id":"200"},`, ``, 1), "", true},
		{"invalid asset", "page", strings.Replace(pageBody, `"id":"100"`, `"id":"not-numeric"`, 1), "", true},
		{"empty entry", "page", `{"object":"page","entry":[{"id":"100"}]}`, "", true},
		{"missing entries", "page", `{"object":"page"}`, "", true},
		{"case-only child", "page", strings.Replace(pageBody, `"changes":`, `"Changes":`, 1), "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := verify(t, config(tc.object), tc.raw)
			if tc.quarantine {
				if len(b.Events) == 0 {
					t.Fatal("invalid unit silently omitted")
				}
				for _, e := range b.Events {
					requireQuarantine(t, e)
					if e.Kind == "page_message" || e.Kind == "instagram_message" || strings.HasPrefix(e.Kind, "page_comment_") || strings.HasPrefix(e.Kind, "instagram_comment") {
						t.Fatalf("invalid unit recognized as %q", e.Kind)
					}
				}
			} else if len(b.Events) != 1 || b.Events[0].Kind != tc.kind || b.Events[0].QuarantineReason != "" {
				t.Fatalf("events=%d, wanted kind %q", len(b.Events), tc.kind)
			}
		})
	}
}

func TestMWP04StableMessageMIDAndConflictDigest(t *testing.T) {
	c := config("page")
	base := verify(t, c, messageBody)
	changed := verify(t, c, strings.Replace(messageBody, `"hello"`, `"changed text"`, 1))
	otherMID := verify(t, c, strings.Replace(messageBody, `"m-1"`, `"m-2"`, 1))
	rebatched := verify(t, c, strings.Replace(messageBody, `"time":1710000000`, `"time":1710001111`, 1))
	ig := config("instagram")
	igMessage := verify(t, ig, strings.Replace(messageBody, `"object":"page"`, `"object":"instagram"`, 1))
	for _, b := range []meta.Batch{base, changed, otherMID, rebatched} {
		if len(b.Events) != 1 || b.Events[0].Kind != "page_message" {
			t.Fatal("message fixture not recognized")
		}
		requireDigest(t, b.Events[0].Key)
		requireDigest(t, b.Events[0].PayloadHash)
	}
	a, b, d, r := base.Events[0], changed.Events[0], otherMID.Events[0], rebatched.Events[0]
	if string(a.Payload) != `{"message":{"mid":"m-1","text":"hello"},"recipient":{"id":"100"},"sender":{"id":"200"},"timestamp":1710000000123}` {
		t.Fatalf("message payload omitted fields or was not canonical: %s", a.Payload)
	}
	requirePayloadHash(t, a)
	requirePayloadHash(t, b)
	if a.Key != b.Key || a.PayloadHash == b.PayloadHash {
		t.Fatal("same MID changed payload must retain key and change digest")
	}
	if a.Key == d.Key || a.Key != r.Key || a.PayloadHash != r.PayloadHash {
		t.Fatal("MID or rebatching identity wrong")
	}
	if len(igMessage.Events) != 1 || igMessage.Events[0].Kind != "instagram_message" || igMessage.Events[0].Key == a.Key {
		t.Fatal("Page and Instagram message identity mixed")
	}
	ordered := strings.Replace(messageBody, `"sender":{"id":"200"},"recipient":{"id":"100"}`, `"recipient":{"id":"100"},"sender":{"id":"200"}`, 1)
	if e := verify(t, c, ordered).Events[0]; e.Key != a.Key || e.PayloadHash != a.PayloadHash {
		t.Fatal("member order changed canonical message")
	}
	duplicated := strings.Replace(messageBody, `"messaging":[`, `"messaging":[`+`{"sender":{"id":"200"},"recipient":{"id":"100"},"timestamp":1710000000123,"message":{"mid":"m-1","text":"hello"}},`, 1)
	dupes := verify(t, c, duplicated).Events
	if len(dupes) != 2 || dupes[0].Key != dupes[1].Key {
		t.Fatal("protocol deduped duplicate units before durable admission")
	}
}

func TestMWP04CommentTransitionsScopeAndOwnedBytes(t *testing.T) {
	c := config("page")
	base := verify(t, c, pageBody).Events[0]
	variants := []struct {
		name, raw string
		config    meta.Config
	}{
		{"id", strings.Replace(pageBody, `"C-1"`, `"C-2"`, 1), c},
		{"asset", strings.Replace(pageBody, `"id":"100"`, `"id":"101"`, 1), c},
		{"app", pageBody, meta.Config{AppID: "654321", Object: "page", AppSecret: testSecret, VerifyToken: testToken}},
		{"edit", strings.Replace(pageBody, `"verb":"add"`, `"verb":"edit"`, 1), c},
		{"remove", strings.Replace(pageBody, `"verb":"add"`, `"verb":"remove"`, 1), c},
		{"content", strings.Replace(pageBody, `"comment_id":"C-1"`, `"comment_id":"C-1","message":"changed"`, 1), c},
	}
	for _, tc := range variants {
		t.Run(tc.name, func(t *testing.T) {
			e := verify(t, tc.config, tc.raw).Events[0]
			if e.Key == base.Key {
				t.Fatal("distinct comment transition reused key")
			}
		})
	}
	if e := verify(t, c, strings.Replace(pageBody, `"time":1710000000`, `"time":1710001111`, 1)).Events[0]; e.Key != base.Key || e.PayloadHash != base.PayloadHash {
		t.Fatal("entry delivery time changed recognized comment identity")
	}
	raw := []byte(pageBody)
	v := verifier(t, c)
	batch, err := v.Verify(raw, signature(c.AppSecret, raw))
	if err != nil {
		t.Fatal(err)
	}
	want := string(batch.Events[0].Payload)
	for i := range raw {
		raw[i] = 'x'
	}
	if string(batch.Events[0].Payload) != want {
		t.Fatal("event payload aliases caller input")
	}
	q1 := verify(t, c, `{"object":"page","entry":[{"id":"100","time":1,"changes":[{"field":"unknown"}]}]}`).Events[0]
	q2 := verify(t, c, `{"object":"page","entry":[{"id":"100","time":2,"changes":[{"field":"unknown"}]}]}`).Events[0]
	requireQuarantine(t, q1)
	requireQuarantine(t, q2)
	if q1.Key == q2.Key {
		t.Fatal("quarantine discarded relevant entry time context")
	}
	// Timestamp fields are evidence only; an invalid present field cannot be
	// silently repaired from entry delivery time.
	valid := verify(t, c, messageBody).Events[0]
	if valid.OccurredAt == nil || valid.OccurredAt.UnixMilli() != 1710000000123 {
		t.Fatal("valid message milliseconds not preserved")
	}
	for _, timestamp := range []string{`null`, `-1`, `1.5`, `"1710000000123"`, `999999999999999999999999`} {
		raw := strings.Replace(messageBody, `"timestamp":1710000000123`, `"timestamp":`+timestamp, 1)
		e := verify(t, c, raw).Events[0]
		if e.Kind != "page_message" || e.OccurredAt != nil {
			t.Fatalf("invalid timestamp %s was repaired or rejected as message", timestamp)
		}
	}
	page := verify(t, c, strings.Replace(pageBody, `"comment_id":"C-1"`, `"comment_id":"C-1","created_time":1710000100`, 1)).Events[0]
	if page.OccurredAt == nil || page.OccurredAt.Unix() != 1710000100 {
		t.Fatal("Page created_time seconds ignored")
	}
	bad := verify(t, c, strings.Replace(pageBody, `"comment_id":"C-1"`, `"comment_id":"C-1","created_time":null`, 1)).Events[0]
	if bad.OccurredAt != nil {
		t.Fatal("invalid created_time silently replaced by entry time")
	}
}

func TestMWP05ChallengeAndMethodGuards(t *testing.T) {
	c := config("page")
	called := 0
	h := handler(t, c, func(context.Context, meta.Batch) error { called++; return nil })
	for _, tc := range []struct {
		name, url string
		ok        bool
	}{
		{"valid", "/webhook?hub.mode=subscribe&hub.verify_token=" + testToken + "&hub.challenge=AZ09._-", true},
		{"bad token", "/webhook?hub.mode=subscribe&hub.verify_token=wrong&hub.challenge=abc", false},
		{"duplicate mode", "/webhook?hub.mode=subscribe&hub.mode=subscribe&hub.verify_token=" + testToken + "&hub.challenge=abc", false},
		{"extra", "/webhook?hub.mode=subscribe&hub.verify_token=" + testToken + "&hub.challenge=abc&extra=1", false},
		{"meta underscore twins", "/webhook?hub.challenge=AZ09._-&hub.mode=subscribe&hub.verify_token=" + testToken + "&hub_challenge=AZ09._-&hub_mode=subscribe&hub_verify_token=" + testToken, true},
		{"twin mismatch", "/webhook?hub.mode=subscribe&hub.verify_token=" + testToken + "&hub.challenge=abc&hub_verify_token=wrong", false},
		{"twin only", "/webhook?hub_mode=subscribe&hub_verify_token=" + testToken + "&hub_challenge=abc", false},
		{"duplicate twin", "/webhook?hub.mode=subscribe&hub.verify_token=" + testToken + "&hub.challenge=abc&hub_challenge=abc&hub_challenge=abc", false},
		{"unsafe challenge", "/webhook?hub.mode=subscribe&hub.verify_token=" + testToken + "&hub.challenge=%3Cscript%3E", false},
		{"empty challenge", "/webhook?hub.mode=subscribe&hub.verify_token=" + testToken + "&hub.challenge=", false},
		{"long challenge", "/webhook?hub.mode=subscribe&hub.verify_token=" + testToken + "&hub.challenge=" + strings.Repeat("a", 201), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, tc.url, nil))
			if tc.ok {
				if w.Code != 200 || w.Body.String() != "AZ09._-" || !strings.HasPrefix(w.Header().Get("Content-Type"), "text/plain") {
					t.Fatalf("challenge response status=%d body=%q", w.Code, w.Body.String())
				}
			} else if w.Code == 200 || strings.Contains(w.Body.String(), testToken) {
				t.Fatalf("invalid challenge accepted/leaked, status=%d", w.Code)
			}
			if w.Header().Get("Cache-Control") != "no-store" {
				t.Fatal("challenge response cacheable")
			}
		})
	}
	for _, method := range []string{http.MethodHead, http.MethodPut, http.MethodDelete} {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest(method, "/webhook", nil))
		if w.Code != 405 || w.Header().Get("Allow") != "GET, POST" || w.Header().Get("Cache-Control") != "no-store" {
			t.Fatalf("method %s: status=%d allow=%q", method, w.Code, w.Header().Get("Allow"))
		}
	}
	if called != 0 {
		t.Fatal("GET/method guard invoked commit")
	}
	if _, err := meta.NewHandler(nil, func(context.Context, meta.Batch) error { return nil }); err == nil {
		t.Fatal("nil verifier accepted")
	}
	if _, err := meta.NewHandler(verifier(t, c), nil); err == nil {
		t.Fatal("nil commit accepted")
	}
	if _, err := meta.NewHandler(new(meta.Verifier), func(context.Context, meta.Batch) error { return nil }); err == nil {
		t.Fatal("zero-value verifier accepted by handler")
	}
}

func TestMWP05POSTTransportGates(t *testing.T) {
	c := config("page")
	called := 0
	h := handler(t, c, func(context.Context, meta.Batch) error { called++; return nil })
	for _, tc := range []struct {
		name, url, contentType, encoding string
		signatureHeaders                 []string
		want                             int
	}{
		{"accepted UTF-8", "/webhook", "application/json; charset=UTF-8", "identity", []string{signature(c.AppSecret, []byte(pageBody))}, 200},
		{"bare query", "/webhook?", "application/json", "", []string{signature(c.AppSecret, []byte(pageBody))}, 400},
		{"query", "/webhook?x=1", "application/json", "", []string{signature(c.AppSecret, []byte(pageBody))}, 400},
		{"wrong MIME", "/webhook", "text/plain", "", []string{signature(c.AppSecret, []byte(pageBody))}, 415},
		{"wrong charset", "/webhook", "application/json; charset=iso-8859-1", "", []string{signature(c.AppSecret, []byte(pageBody))}, 415},
		{"extra MIME parameter", "/webhook", "application/json; foo=bar", "", []string{signature(c.AppSecret, []byte(pageBody))}, 415},
		{"compressed", "/webhook", "application/json", "gzip", []string{signature(c.AppSecret, []byte(pageBody))}, 415},
		{"no signature", "/webhook", "application/json", "", nil, 403},
		{"sha1", "/webhook", "application/json", "", []string{"sha1=" + strings.Repeat("0", 40)}, 403},
		{"malformed signature", "/webhook", "application/json", "", []string{"sha256=not-hex"}, 403},
		{"duplicate signature", "/webhook", "application/json", "", []string{signature(c.AppSecret, []byte(pageBody)), signature(c.AppSecret, []byte(pageBody))}, 403},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodPost, tc.url, strings.NewReader(pageBody))
			r.Header.Set("Content-Type", tc.contentType)
			if tc.encoding != "" {
				r.Header.Set("Content-Encoding", tc.encoding)
			}
			for _, value := range tc.signatureHeaders {
				r.Header.Add("X-Hub-Signature-256", value)
			}
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			if w.Code != tc.want || w.Header().Get("Cache-Control") != "no-store" {
				t.Fatalf("status=%d cache=%q", w.Code, w.Header().Get("Cache-Control"))
			}
			if tc.want != 200 && strings.Contains(w.Body.String(), testSecret) {
				t.Fatal("error leaked secret")
			}
		})
	}
	if called != 1 {
		t.Fatalf("invalid transport invoked commit %d times", called-1)
	}
}

func TestMWP05CommitBlocksACKAndSanitizesFailure(t *testing.T) {
	c := config("page")
	spy := &writeSpy{ResponseRecorder: httptest.NewRecorder()}
	spyEntered := make(chan int32, 1)
	spyRelease := make(chan struct{})
	spyReleased := false
	defer func() {
		if !spyReleased {
			close(spyRelease)
		}
	}()
	spyHandler := handler(t, c, func(context.Context, meta.Batch) error {
		spyEntered <- spy.writes.Load()
		<-spyRelease
		return nil
	})
	spyReq := httptest.NewRequest(http.MethodPost, "/webhook", strings.NewReader(pageBody))
	spyReq.Header.Set("Content-Type", "application/json")
	spyReq.Header.Set("X-Hub-Signature-256", signature(c.AppSecret, []byte(pageBody)))
	spyDone := make(chan struct{})
	go func() {
		spyHandler.ServeHTTP(spy, spyReq)
		close(spyDone)
	}()
	select {
	case before := <-spyEntered:
		if before != 0 || spy.writes.Load() != 0 {
			t.Fatal("response writer touched before commit completed")
		}
	case <-spyDone:
		t.Fatal("handler returned before blocked callback completed")
	case <-time.After(3 * time.Second):
		t.Fatal("writer-spy callback not entered")
	}
	close(spyRelease)
	spyReleased = true
	select {
	case <-spyDone:
		if spy.Code != 200 || spy.Body.String() != "EVENT_RECEIVED" {
			t.Fatalf("writer-spy success status=%d body=%q", spy.Code, spy.Body.String())
		}
	case <-time.After(3 * time.Second):
		t.Fatal("writer-spy handler did not return after release")
	}
	entered := make(chan meta.Batch, 1)
	release := make(chan struct{})
	var calls atomic.Int32
	h := handler(t, c, func(_ context.Context, b meta.Batch) error {
		calls.Add(1)
		entered <- b
		<-release
		return nil
	})
	server := httptest.NewServer(h)
	defer server.Close()
	released := false
	defer func() {
		if !released {
			close(release)
		}
	}()
	result := make(chan *http.Response, 1)
	errResult := make(chan error, 1)
	go func() {
		req, err := http.NewRequest(http.MethodPost, server.URL+"/webhook", strings.NewReader(pageBody))
		if err != nil {
			errResult <- err
			return
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Hub-Signature-256", signature(c.AppSecret, []byte(pageBody)))
		resp, err := server.Client().Do(req)
		if err != nil {
			errResult <- err
			return
		}
		result <- resp
	}()
	select {
	case b := <-entered:
		if len(b.Events) != 1 || b.Events[0].Kind != "page_comment_add" {
			t.Fatal("commit did not receive complete batch")
		}
	case err := <-errResult:
		t.Fatalf("request before callback: %v", err)
	case <-time.After(3 * time.Second):
		t.Fatal("commit callback not entered")
	}
	// Causal barrier: callback has entered but cannot return until release closes.
	select {
	case resp := <-result:
		resp.Body.Close()
		t.Fatal("ACK sent while durable callback was blocked")
	case err := <-errResult:
		t.Fatalf("request failed while callback blocked: %v", err)
	case <-time.After(75 * time.Millisecond):
	}
	close(release)
	released = true
	select {
	case resp := <-result:
		defer resp.Body.Close()
		if resp.StatusCode != 200 || resp.Header.Get("Cache-Control") != "no-store" {
			t.Fatalf("commit success status=%d", resp.StatusCode)
		}
		body, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatal(err)
		}
		if string(body) != "EVENT_RECEIVED" {
			t.Fatalf("success body=%q", body)
		}
		if calls.Load() != 1 {
			t.Fatalf("commit invoked %d times", calls.Load())
		}
	case err := <-errResult:
		t.Fatal(err)
	case <-time.After(3 * time.Second):
		t.Fatal("no ACK after commit release")
	}
	for _, failure := range []error{errors.New("sensitive commit failure"), context.Canceled} {
		calls := 0
		failHandler := handler(t, c, func(context.Context, meta.Batch) error { calls++; return failure })
		w := post(failHandler, pageBody, signature(c.AppSecret, []byte(pageBody)))
		if w.Code != 503 || calls != 1 || w.Header().Get("Cache-Control") != "no-store" || strings.Contains(w.Body.String(), failure.Error()) {
			t.Fatalf("failure status=%d calls=%d body=%q", w.Code, calls, w.Body.String())
		}
	}
}
