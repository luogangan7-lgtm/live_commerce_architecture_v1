// Package grammar owns the pure kw-v1 live-comment grammar (width map, trim, keyword, +N).
// It has no I/O, database, clock, logging or offer knowledge; callers resolve keywords.
//
// Contract: contracts/live-keyword-claims-v1.md §2 (FROZEN). Parse, NormalizeKeyword
// and NormalizeLabel are deterministic functions of their input and nothing else.
//
// Non-goals: no NFKC or locale-aware folding (NFKC would also accept ①, ², ﬁ, Roman
// numerals and the Kelvin sign), no unicode.ToUpper (ſ→S, ı→I, U+212A→K traps), no
// regexp (no backtracking), no substring/"contains" matching, no guessed SKU, no
// negation or question interpretation, and no knowledge of offers, windows or modes:
// the KEYWORD_QTY_ONLY rule is applied at ingest by package claims (§2.3).
//
// Stdlib only: unicode/utf8 decodes; the unicode category
// tables are consulted solely by NormalizeLabel to reject control (Cc) and format (Cf)
// characters, never for case mapping; fmt supplies the Formatter interface that keeps
// a Result redacted under every verb.
package grammar

import (
	"fmt"
	"unicode"
	"unicode/utf8"
)

// Version names this grammar in every persisted claim event (claims.events.grammar_version).
const Version = "kw-v1"

// Bounds fixed by the frozen grammar (§2.1 step 0, §2.2, §2.2 NormalizeLabel).
const (
	MaxTextBytes   = 256  // Parse short-circuits to NO_MATCH above this many bytes.
	MaxKeywordLen  = 16   // A canonical keyword is ^[A-Z0-9]{1,16}$.
	MaxQuantity    = 999  // "+N" accepts 1..999 written without a leading zero.
	MaxLabelRunes  = 60   // Manual actor labels are 1..60 code points after normalization.
	maxLabelBytes  = 1024 // Work bound for NormalizeLabel input (60 full-width runes plus padding).
	maxDigitsInQty = 3    // "+1000" and longer are INVALID_QUANTITY before any conversion.
)

// Kind is the grammar verdict for one comment.
type Kind string // "MATCH" | "NO_MATCH" | "INVALID_QUANTITY"

const (
	Match           Kind = "MATCH"
	NoMatch         Kind = "NO_MATCH"
	InvalidQuantity Kind = "INVALID_QUANTITY"
)

// Result is one parse. Keyword is the canonical head for MATCH and INVALID_QUANTITY
// and "" for NO_MATCH; Quantity is 1..999 for MATCH and 0 otherwise; Explicit is true
// only for a MATCH written with "+N".
//
// A keyword-shaped comment can be a phone number (0912345678), so every formatting
// path (String, GoString, Format, MarshalJSON) emits only Version and Kind.
type Result struct {
	Version  string // always Version
	Kind     Kind
	Keyword  string // canonical head; "" for NO_MATCH
	Quantity int64  // 1..999 for MATCH; 0 otherwise
	Explicit bool   // "+N" present (MATCH only)
}

// Parse applies kw-v1 to one comment. It never fails: anything outside the grammar is
// NO_MATCH, and an out-of-range "+N" on a keyword-shaped head is INVALID_QUANTITY.
// Pure; safe for concurrent use; linear in len(text) and bounded by MaxTextBytes.
func Parse(text string) Result {
	s, ok := canonical(text)
	if !ok {
		return Result{Version: Version, Kind: NoMatch}
	}
	head, tail, plus := cutPlus(s)
	if !isKeyword(head) {
		return Result{Version: Version, Kind: NoMatch}
	}
	if !plus {
		return Result{Version: Version, Kind: Match, Keyword: head, Quantity: 1}
	}
	if tail == "" || !allDigits(tail) {
		return Result{Version: Version, Kind: NoMatch}
	}
	// Length is checked before conversion, so no input can overflow.
	if len(tail) > maxDigitsInQty || tail[0] == '0' {
		return Result{Version: Version, Kind: InvalidQuantity, Keyword: head}
	}
	var quantity int64
	for i := 0; i < len(tail); i++ {
		quantity = quantity*10 + int64(tail[i]-'0')
	}
	return Result{Version: Version, Kind: Match, Keyword: head, Quantity: quantity, Explicit: true}
}

// NormalizeKeyword returns the canonical form stored in live.offers.keyword: steps 0–3
// of §2.1, then ^[A-Z0-9]{1,16}$ (so "+" is never part of a keyword). Pure.
func NormalizeKeyword(raw string) (string, bool) {
	s, ok := canonical(raw)
	if !ok || !isKeyword(s) {
		return "", false
	}
	return s, true
}

// NormalizeLabel canonicalizes a merchant-typed manual actor label (claims.bundles.label):
// width map (§2.1 step 1), trim the ends, strip one leading "@", trim again, collapse
// interior whitespace runs to one U+0020, ASCII-lowercase, then require 1..60 code points,
// no control or format character and no remaining leading "@". Whitespace means the same
// four ASCII characters as §2.1 step 2 (after the width map).
//
// Stripping "@" before the final trim makes "@ Amy" equal "amy", and rejecting a second
// leading "@" makes the function idempotent: NormalizeLabel(x) == (y, true) implies
// NormalizeLabel(y) == (y, true) (contract §0.1 P2(d)).
//
// Every accepted value satisfies the claims.bundles.label CHECK (btrim, 1..60, no
// [[:cntrl:]]), so SQL never raises 23514 whose DETAIL would copy this personal data into
// the PostgreSQL server log (P2(b)). U+2028/U+2029 are rejected with Cc and Cf because
// glibc's cntrl class, which PostgreSQL's [[:cntrl:]] may use, includes them. Pure.
func NormalizeLabel(raw string) (string, bool) {
	if len(raw) > maxLabelBytes || !utf8.ValidString(raw) {
		return "", false
	}
	s := trimSpace(widthMap(raw))
	if len(s) > 0 && s[0] == '@' {
		s = trimSpace(s[1:])
	}
	out := make([]byte, 0, len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case isSpace(c):
			if !isSpace(s[i-1]) { // trimmed, so a space is never at index 0
				out = append(out, ' ')
			}
		case c >= 'A' && c <= 'Z':
			out = append(out, c+'a'-'A')
		default:
			out = append(out, c)
		}
	}
	count := 0
	for _, r := range string(out) {
		if unicode.Is(unicode.Cc, r) || unicode.Is(unicode.Cf, r) || r == 0x2028 || r == 0x2029 {
			return "", false
		}
		count++
	}
	if count < 1 || count > MaxLabelRunes || out[0] == '@' {
		return "", false
	}
	return string(out), true
}

// canonical is §2.1 steps 0–3: bound and validate, width map, trim ends, ASCII upper.
func canonical(text string) (string, bool) {
	if len(text) > MaxTextBytes || !utf8.ValidString(text) {
		return "", false
	}
	s := []byte(trimSpace(widthMap(text)))
	for i, c := range s {
		if c >= 'a' && c <= 'z' {
			s[i] = c - ('a' - 'A')
		}
	}
	return string(s), true
}

// widthMap is §2.1 step 1: the explicit full-width table, nothing else.
func widthMap(text string) string {
	out := make([]byte, 0, len(text))
	for _, r := range text {
		switch {
		case r >= 0xFF01 && r <= 0xFF5E: // full-width ASCII, including ０-９ Ａ-Ｚ ａ-ｚ ＋ ＠
			r -= 0xFEE0
		case r == 0x3000 || r == 0x00A0: // ideographic space, no-break space
			r = ' '
		}
		out = utf8.AppendRune(out, r)
	}
	return string(out)
}

// trimSpace is §2.1 step 2: only U+0020, U+0009, U+000A, U+000D, and only at the ends.
func trimSpace(s string) string {
	start, end := 0, len(s)
	for start < end && isSpace(s[start]) {
		start++
	}
	for end > start && isSpace(s[end-1]) {
		end--
	}
	return s[start:end]
}

func isSpace(c byte) bool { return c == ' ' || c == '\t' || c == '\n' || c == '\r' }

// cutPlus splits at the first '+'. '+' is ASCII, so a byte scan is UTF-8 safe.
func cutPlus(s string) (head, tail string, found bool) {
	for i := 0; i < len(s); i++ {
		if s[i] == '+' {
			return s[:i], s[i+1:], true
		}
	}
	return s, "", false
}

// isKeyword reports ^[A-Z0-9]{1,16}$ without regexp.
func isKeyword(s string) bool {
	if len(s) < 1 || len(s) > MaxKeywordLen {
		return false
	}
	for i := 0; i < len(s); i++ {
		if !(s[i] >= 'A' && s[i] <= 'Z' || s[i] >= '0' && s[i] <= '9') {
			return false
		}
	}
	return true
}

func allDigits(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

// redacted renders only fields that cannot carry comment text. A Version or Kind that is
// not one of the fixed constants is replaced, so a hand-built Result cannot smuggle text.
func (r Result) redacted() (version, kind string) {
	version, kind = "invalid", "INVALID"
	if r.Version == Version {
		version = Version
	}
	if r.Kind == Match || r.Kind == NoMatch || r.Kind == InvalidQuantity {
		kind = string(r.Kind)
	}
	return version, kind
}

// String never includes Keyword or Quantity (redacted type, contract §4.1).
func (r Result) String() string {
	version, kind := r.redacted()
	return "grammar.Result{Version:" + version + " Kind:" + kind + "}"
}

// GoString keeps %#v redacted.
func (r Result) GoString() string { return r.String() }

// Format keeps every fmt verb (including %d and %x) redacted.
func (r Result) Format(f fmt.State, _ rune) { _, _ = f.Write([]byte(r.String())) }

// MarshalJSON emits only version and kind; both are fixed ASCII tokens, so no escaping.
func (r Result) MarshalJSON() ([]byte, error) {
	version, kind := r.redacted()
	return []byte(`{"version":"` + version + `","kind":"` + kind + `"}`), nil
}
