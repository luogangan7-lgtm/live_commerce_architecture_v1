// Unit gate KC01 for the pure kw-v1 grammar (contracts/live-keyword-claims-v1.md §2, §9).
// Owns: the canonical-vector loader for tests/claims/kw-v1-vectors.json, FuzzParse, and
// NormalizeKeyword/NormalizeLabel/redaction unit cases. Non-goals: no database, offers,
// windows or ingest precedence (package claims and the independent KC06 gate own those).

package grammar

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"strings"
	"testing"
	"unicode/utf8"
)

// vectorFile mirrors tests/claims/kw-v1-vectors.json exactly; unknown keys fail the
// decode so a renamed field cannot silently drop vectors.
type vectorFile struct {
	GrammarVersion string            `json:"grammar_version"`
	Source         string            `json:"source"`
	Consumers      []string          `json:"consumers"`
	Format         map[string]string `json:"format"`
	Parse          []struct {
		ID         string `json:"id"`
		Input      string `json:"input"`
		InputHex   string `json:"input_hex"`
		PadToBytes int    `json:"pad_to_bytes"`
		Pad        string `json:"pad"`
		Expect     struct {
			Kind     Kind   `json:"kind"`
			Keyword  string `json:"keyword"`
			Quantity int64  `json:"quantity"`
			Explicit bool   `json:"explicit"`
		} `json:"expect"`
	} `json:"parse"`
	IngestOffers json.RawMessage `json:"ingest_offers"`
	Ingest       []struct {
		ID          string          `json:"id"`
		Mode        string          `json:"mode"`
		Text        string          `json:"text"`
		OfferState  string          `json:"offer_state"`
		Window      string          `json:"window"`
		GrammarKind Kind            `json:"grammar_kind"`
		Outcome     string          `json:"outcome"`
		Reason      string          `json:"reason"`
		Persisted   json.RawMessage `json:"persisted"`
	} `json:"ingest"`
	Sequences json.RawMessage `json:"sequences"`
}

func loadVectors(t *testing.T) vectorFile {
	t.Helper()
	raw, err := os.ReadFile("../../../tests/claims/kw-v1-vectors.json")
	if err != nil {
		t.Fatalf("read canonical vectors: %v", err)
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	var file vectorFile
	if err := decoder.Decode(&file); err != nil {
		t.Fatalf("decode canonical vectors: %v", err)
	}
	return file
}

// vectorInput reconstructs the exact bytes a vector describes (see the file's "format").
func vectorInput(t *testing.T, input, inputHex string, padTo int, pad string) string {
	t.Helper()
	if inputHex != "" {
		raw, err := hex.DecodeString(inputHex)
		if err != nil || input != "" {
			t.Fatalf("bad input_hex vector %q", inputHex)
		}
		return string(raw)
	}
	if padTo > 0 {
		if len(pad) != 1 || len(input) > padTo {
			t.Fatalf("bad padded vector %q", input)
		}
		return input + strings.Repeat(pad, padTo-len(input))
	}
	return input
}

// Every §2.4 Parse example, from the canonical file, plus the ingest texts' grammar kind.
func TestGrammarKC01Vectors(t *testing.T) {
	file := loadVectors(t)
	if file.GrammarVersion != Version {
		t.Fatalf("vector grammar %q != %q", file.GrammarVersion, Version)
	}
	ids := map[string]bool{}
	for _, v := range file.Parse {
		ids[v.ID] = true
		input := vectorInput(t, v.Input, v.InputHex, v.PadToBytes, v.Pad)
		got := Parse(input)
		want := Result{Version: Version, Kind: v.Expect.Kind, Keyword: v.Expect.Keyword, Quantity: v.Expect.Quantity, Explicit: v.Expect.Explicit}
		if got != want {
			t.Errorf("%s Parse(%q) = %#v kind=%s keyword=%q qty=%d explicit=%t; want kind=%s keyword=%q qty=%d explicit=%t",
				v.ID, input, got, got.Kind, got.Keyword, got.Quantity, got.Explicit, want.Kind, want.Keyword, want.Quantity, want.Explicit)
		}
	}
	for _, id := range []string{"G01", "G02", "G03", "G04", "G05", "G06", "R01", "R02", "R03", "R04", "R05", "R06", "R07", "R08", "R09"} {
		if !ids[id] {
			t.Errorf("canonical vector file lacks %s", id)
		}
	}
	for _, v := range file.Ingest {
		if got := Parse(v.Text).Kind; got != v.GrammarKind {
			t.Errorf("%s ingest text %q parses as %s, vector says %s", v.ID, v.Text, got, v.GrammarKind)
		}
	}
	if len(file.Ingest) != 12 || len(file.Parse) != 59 {
		t.Fatalf("vector file truncated: parse=%d ingest=%d", len(file.Parse), len(file.Ingest))
	}
}

func TestGrammarBoundaries(t *testing.T) {
	cases := []struct {
		in   string
		want Result
	}{
		{"A1" + strings.Repeat(" ", 254), Result{Version, Match, "A1", 1, false}}, // exactly 256 bytes
		{"A1" + strings.Repeat(" ", 255), Result{Version, NoMatch, "", 0, false}}, // 257 bytes
		{"A1+1", Result{Version, Match, "A1", 1, true}},
		{"A1+10", Result{Version, Match, "A1", 10, true}},
		{"A1+001", Result{Version, InvalidQuantity, "A1", 0, false}},
		{"\u00a0A1\u00a0", Result{Version, Match, "A1", 1, false}},
		{"A1\r\n", Result{Version, Match, "A1", 1, false}},
		{"A1\v", Result{Version, NoMatch, "", 0, false}}, // only four ASCII spaces are trimmed
		{"\uff20A1", Result{Version, NoMatch, "", 0, false}},
		{"A1+\uff19\uff19\uff19", Result{Version, Match, "A1", 999, true}},
	}
	for _, c := range cases {
		if got := Parse(c.in); got != c.want {
			t.Errorf("Parse(%q) kind=%s keyword=%q qty=%d explicit=%t", c.in, got.Kind, got.Keyword, got.Quantity, got.Explicit)
		}
	}
}

func TestNormalizeKeyword(t *testing.T) {
	for raw, want := range map[string]string{"a1": "A1", " \uff41\uff11 ": "A1", "101": "101", "ABCDEFGHIJKLMNOP": "ABCDEFGHIJKLMNOP", "a1x2": "A1X2"} {
		if got, ok := NormalizeKeyword(raw); !ok || got != want {
			t.Errorf("NormalizeKeyword(%q) = %q,%t want %q", raw, got, ok, want)
		}
	}
	for _, raw := range []string{"", " ", "A1+2", "A1+", "A 1", "ABCDEFGHIJKLMNOPQ", "\u212a1", "\u017f1", "A1\u200b", "#A1", string([]byte{0xff}), strings.Repeat(" ", 300) + "A1"} {
		if got, ok := NormalizeKeyword(raw); ok || got != "" {
			t.Errorf("NormalizeKeyword(%q) accepted %q", raw, got)
		}
	}
}

func TestNormalizeLabel(t *testing.T) {
	for raw, want := range map[string]string{
		"@Amy ":                 "amy",
		"amy":                   "amy",
		"@ Amy":                 "amy",
		" @Amy":                 "amy",
		"\uff20\uff21my":        "amy", // full-width @ and A
		"Amy   Lee":             "amy lee",
		"Amy\t\nLee":            "amy lee",
		"Amy\u3000Lee":          "amy lee",
		"\u738b\u5c0f\u660e":    "\u738b\u5c0f\u660e", // CJK name unchanged
		"\u00c5SA":              "\u00c5sa",           // ASCII-only lowercase, like the grammar's ASCII-only upper
		strings.Repeat("x", 60): strings.Repeat("x", 60),
	} {
		got, ok := NormalizeLabel(raw)
		if !ok || got != want {
			t.Errorf("NormalizeLabel(%q) = %q,%t want %q", raw, got, ok, want)
			continue
		}
		// P2(d): idempotent on every accepted value.
		if again, ok := NormalizeLabel(got); !ok || again != got {
			t.Errorf("NormalizeLabel not idempotent: %q -> %q -> %q,%t", raw, got, again, ok)
		}
	}
	for _, raw := range []string{"", " ", "@", "@ ", "@@amy", "@ @amy", strings.Repeat("x", 61), "amy\x00", "amy\x7f", "a\u0085b",
		"a\u200bb", "\ufeffamy", "a\u2028b", "a\u2029b", "a\u202eb", string([]byte{'a', 0xff}), strings.Repeat(" ", 1025)} {
		if got, ok := NormalizeLabel(raw); ok || got != "" {
			t.Errorf("NormalizeLabel(%q) accepted %q", raw, got)
		}
	}
	if a, _ := NormalizeLabel("@Amy "); a != "amy" {
		t.Fatalf("contract example `@Amy ` must equal `amy`, got %q", a)
	}
}

func TestResultRedaction(t *testing.T) {
	r := Parse("0912345678+5")
	if r.Keyword != "0912345678" || r.Quantity != 5 {
		t.Fatalf("sentinel parse changed: %+v", r.Keyword)
	}
	outputs := []string{fmt.Sprint(r), fmt.Sprintf("%v %+v %#v %s %q %x %X %d", r, r, r, r, r, r, r, r), r.String(), r.GoString()}
	encoded, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	outputs = append(outputs, string(encoded))
	wrapped, err := json.Marshal(struct{ R Result }{r})
	if err != nil {
		t.Fatal(err)
	}
	outputs = append(outputs, string(wrapped))
	for _, out := range outputs {
		if strings.Contains(out, "0912345678") || strings.Contains(out, hex.EncodeToString([]byte("0912345678"))) {
			t.Fatalf("redacted Result leaked: %s", out)
		}
	}
	if string(encoded) != `{"version":"kw-v1","kind":"MATCH"}` {
		t.Fatalf("Result JSON = %s", encoded)
	}
	forged := Result{Version: "0912345678", Kind: Kind("0912345678"), Keyword: "0912345678"}
	if out := fmt.Sprintf("%v", forged); strings.Contains(out, "0912345678") {
		t.Fatalf("forged Result fields leaked: %s", out)
	}
}

var keywordShape = regexp.MustCompile(`^[A-Z0-9]{1,16}$`)

// allowedMatchRune is every code point a MATCH may contain: ASCII keyword characters and
// '+', their full-width forms, and the trimmed whitespace. Anything else (ſ, ı, K, ①, ...)
// proves a non-table case or width mapping.
func allowedMatchRune(r rune) bool {
	switch {
	case r >= 'A' && r <= 'Z', r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '+':
		return true
	case r >= 0xFF21 && r <= 0xFF3A, r >= 0xFF41 && r <= 0xFF5A, r >= 0xFF10 && r <= 0xFF19, r == 0xFF0B:
		return true
	case r == ' ', r == '\t', r == '\n', r == '\r', r == 0x3000, r == 0x00A0:
		return true
	}
	return false
}

// FuzzParse is the KC01 property gate (run ≥60 s: go test -run='^$' -fuzz='^FuzzParse$'
// -fuzztime=60s ./internal/claims/grammar). Seeds are the canonical vectors.
func FuzzParse(f *testing.F) {
	for _, seed := range []string{"A1", "a1", "A1+2", "\uff41\uff11\uff0b\uff12", "A1+999", "A1+0", "A1+02", "0912345678", "\u4e0d\u8981A1", "A1 +2",
		"\u212a1", "\u017f1", "A1\u200b", "\u2460", "A1\ufe622", "a1x2", "", "   ", "A1+", "A1++2", "\tA1+3\n", string([]byte{0x41, 0x31, 0xff})} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, text string) {
		r := Parse(text)
		if again := Parse(text); again != r {
			t.Fatalf("non-deterministic Parse(%q)", text)
		}
		if r.Version != Version {
			t.Fatalf("version %q", r.Version)
		}
		if len(text) > MaxTextBytes && r.Kind != NoMatch {
			t.Fatalf("over-long input matched: %d bytes", len(text))
		}
		switch r.Kind {
		case Match:
			if !keywordShape.MatchString(r.Keyword) || r.Quantity < 1 || r.Quantity > MaxQuantity || (!r.Explicit && r.Quantity != 1) {
				t.Fatalf("invalid MATCH for %q: keyword=%q qty=%d explicit=%t", text, r.Keyword, r.Quantity, r.Explicit)
			}
			for _, c := range text {
				if !allowedMatchRune(c) || c == utf8.RuneError {
					t.Fatalf("MATCH from non-table rune %U in %q", c, text)
				}
			}
			canonicalText := r.Keyword
			if r.Explicit {
				canonicalText += fmt.Sprintf("+%d", r.Quantity)
			}
			if again := Parse(canonicalText); again != r {
				t.Fatalf("not idempotent on normalized input %q (from %q)", canonicalText, text)
			}
			if kw, ok := NormalizeKeyword(r.Keyword); !ok || kw != r.Keyword {
				t.Fatalf("MATCH keyword not canonical: %q", r.Keyword)
			}
		case InvalidQuantity:
			if !keywordShape.MatchString(r.Keyword) || r.Quantity != 0 || r.Explicit {
				t.Fatalf("invalid INVALID_QUANTITY for %q", text)
			}
			if again := Parse(r.Keyword + "+0"); again != r {
				t.Fatalf("INVALID_QUANTITY not idempotent for %q", text)
			}
		case NoMatch:
			if r.Keyword != "" || r.Quantity != 0 || r.Explicit {
				t.Fatalf("NO_MATCH carried data for %q", text)
			}
		default:
			t.Fatalf("unknown kind %q", r.Kind)
		}
		if label, ok := NormalizeLabel(text); ok {
			if again, ok := NormalizeLabel(label); !ok || again != label {
				t.Fatalf("NormalizeLabel not idempotent: %q -> %q -> %q", text, label, again)
			}
		}
	})
}
