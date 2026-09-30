// mac.go: CheckMacValue (F6), the recipient rule twin of fulfillment.ecpay_recipient_ok, the trade
// number derivation (E2) and the subtype tables. Pure functions, no I/O.
//
// External constants: CheckMacValue algorithm https://developers.ecpay.com.tw/7424/ and /7400/
// (retrieved 2026-09-29): sort parameters A-Z, "HashKey=..&k=v&..&HashIV=..", URL-encode with the
// .NET table, lowercase, MD5, uppercase hex. Recipient rules https://developers.ecpay.com.tw/8809/
// (F5, retrieved 2026-09-29/30).

package ecpay

import (
	"crypto/md5"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base32"
	"encoding/hex"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"unicode"
)

const macField = "CheckMacValue"

// encodeNET is the .NET-compatible URL encoding ECPay documents for the MAC: everything but
// [A-Za-z0-9] and "-_.!*()" is %xx (lowercase hex, per byte of the UTF-8 text) and a space is "+".
// Go's url.QueryEscape differs ("~" stays, "!*()" are escaped), so it is not used.
func encodeNET(s string) string {
	const hexd = "0123456789abcdef"
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'A' && c <= 'Z', c >= 'a' && c <= 'z', c >= '0' && c <= '9',
			c == '-', c == '_', c == '.', c == '!', c == '*', c == '(', c == ')':
			b.WriteByte(c)
		case c == ' ':
			b.WriteByte('+')
		default:
			b.WriteByte('%')
			b.WriteByte(hexd[c>>4])
			b.WriteByte(hexd[c&15])
		}
	}
	return b.String()
}

// macOrder selects the key ordering. The docs say "A to Z, first letter, then second letter ...",
// which every official SDK implements case-insensitively (ties by byte order); the contract text says
// "case-sensitive". They differ only for keys such as CVSPaymentNo vs CollectionAmount.
type macOrder int

const (
	orderFold macOrder = iota // case-insensitive, used to sign
	orderByte                 // plain byte order, accepted as a second candidate when verifying
)

func macDigest(params url.Values, key, iv string, order macOrder) string {
	keys := make([]string, 0, len(params))
	for k := range params {
		if k != macField {
			keys = append(keys, k)
		}
	}
	if order == orderFold {
		sort.Slice(keys, func(i, j int) bool {
			li, lj := strings.ToLower(keys[i]), strings.ToLower(keys[j])
			if li != lj {
				return li < lj
			}
			return keys[i] < keys[j]
		})
	} else {
		sort.Strings(keys)
	}
	var b strings.Builder
	b.WriteString("HashKey=" + key)
	for _, k := range keys {
		b.WriteString("&" + k + "=" + params.Get(k))
	}
	b.WriteString("&HashIV=" + iv)
	// MD5 is the vendor's keyed-envelope MAC (F6); accepted as provider authentication only.
	sum := md5.Sum([]byte(strings.ToLower(encodeNET(b.String()))))
	return strings.ToUpper(hex.EncodeToString(sum[:]))
}

// CheckMac computes CheckMacValue over every field of params except CheckMacValue itself (F6).
func CheckMac(params url.Values, hashKey, hashIV string) string {
	return macDigest(params, hashKey, hashIV, orderFold)
}

// VerifyMac reports whether params["CheckMacValue"] matches the MAC of every other field. The
// comparison is constant-time; both key orderings are evaluated (no short circuit) so a doc/contract
// ambiguity cannot silently reject every genuine provider message.
// ponytail: two candidates until TCV07 records which ordering ECPay uses for a mixed-case response.
func VerifyMac(params url.Values, hashKey, hashIV string) bool {
	got := strings.ToUpper(params.Get(macField))
	if got == "" {
		return false
	}
	a := subtle.ConstantTimeCompare([]byte(got), []byte(macDigest(params, hashKey, hashIV, orderFold)))
	b := subtle.ConstantTimeCompare([]byte(got), []byte(macDigest(params, hashKey, hashIV, orderByte)))
	return a|b == 1
}

var phoneRE = regexp.MustCompile(`^09[0-9]{8}$`)

// normalizePhone is the phone half of fulfillment.ecpay_recipient_ok: spaces, hyphens and
// parentheses are removed, "+8869xxxxxxxx" becomes "09xxxxxxxx", and the result must be
// 09 + 8 ASCII digits (landlines and overseas numbers are refused, F5/F13).
func normalizePhone(phone string) (string, bool) {
	s := strings.Map(func(r rune) rune {
		switch r {
		case ' ', '-', '(', ')':
			return -1
		}
		return r
	}, phone)
	if rest, ok := strings.CutPrefix(s, "+886"); ok && len(rest) == 9 && rest[0] == '9' {
		s = "0" + rest
	}
	return s, phoneRE.MatchString(s)
}

// wide reports the East Asian wide/full-width blocks that count as width 2 (F5: Chinese 2-5 chars).
// A stdlib approximation of the SQL twin's rule; the shared golden table pins the behaviour.
func wide(r rune) bool {
	switch {
	case r >= 0x1100 && r <= 0x11FF, r >= 0x2E80 && r <= 0x303E, r >= 0x3041 && r <= 0x33FF,
		r >= 0x3400 && r <= 0x4DBF, r >= 0x4E00 && r <= 0x9FFF, r >= 0xA000 && r <= 0xA4CF,
		r >= 0xAC00 && r <= 0xD7A3, r >= 0xF900 && r <= 0xFAFF, r >= 0xFE30 && r <= 0xFE6F,
		r >= 0xFF01 && r <= 0xFF60, r >= 0xFFE0 && r <= 0xFFE6, r >= 0x20000 && r <= 0x3FFFD:
		return true
	}
	return false
}

// nameOK: letters only (no digits, ASCII symbols, spaces or emoji), display width 4..10 with
// CJK/full-width = 2 (F5). It also validates SenderName (same 4-10 rule).
func nameOK(name string) bool {
	w := 0
	for _, r := range name {
		if !unicode.IsLetter(r) {
			return false
		}
		if wide(r) {
			w += 2
		} else {
			w++
		}
	}
	return w >= 4 && w <= 10
}

// RecipientOK is the Go twin of fulfillment.ecpay_recipient_ok(name, phone): the name rule of F5 and
// the phone rule of normalizePhone. It returns the normalised 09xxxxxxxx phone.
func RecipientOK(name, phone string) (normalizedPhone string, ok bool) {
	p, pok := normalizePhone(phone)
	if !pok || !nameOK(name) {
		return "", false
	}
	return p, true
}

// MerchantTradeNo is the E2 trade number: "LC" + base32(sha256(lowercase operation uuid text))[:18]
// (RFC 4648 upper, no padding), 20 chars of [A-Z2-7]; the Go twin of fulfillment.ecpay_trade_no
// (cvs-core C2). ECPay requires it unique per merchant (F5); it is frozen per operation and reused,
// never regenerated, by Query.
func MerchantTradeNo(operationID string) string {
	sum := sha256.Sum256([]byte(strings.ToLower(operationID)))
	return "LC" + base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(sum[:])[:18]
}

// subtype tables (F2/F5). UNIMARTFREEZE is outside the v1 subtype CHECK (§13) and is ErrInvalid.
var subTypes = map[string]string{
	"UNIMARTC2C": "UNIMART", "UNIMART": "UNIMART",
	"FAMIC2C": "FAMI", "FAMI": "FAMI",
	"HILIFEC2C": "HILIFE", "HILIFE": "HILIFE",
	"OKMARTC2C": "",
}

// CVSType maps a logistics subtype to its GetStoreList CvsType (§7.2). OKMARTC2C has none (F10).
func CVSType(subType string) (string, error) {
	t, ok := subTypes[subType]
	switch {
	case !ok:
		return "", ErrInvalid
	case t == "":
		return "", ErrNoDirectory
	}
	return t, nil
}

func isC2C(subType string) bool { return strings.HasSuffix(subType, "C2C") }
