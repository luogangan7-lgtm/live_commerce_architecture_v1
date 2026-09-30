// credentials.go owns the claims secrets and bearer values: the server-held manual-label
// HMAC key (LabelKey), the buyer claim-link token (LinkToken), the one-time IssuedLink
// result, and the constant redaction every input/credential type implements (§4, §8).
//
// Non-goals: no key loading or rotation (cmd/api reads COMMERCE_CLAIMS_LABEL_KEY; §10),
// no storage (the database only ever sees SHA-256 of a token and keyed label MACs inside
// receipt request digests), no HTTP header parsing.

package claims

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"time"

	"livecommerce/internal/command"
)

// redacted is the constant rendering of every redacted value under fmt and JSON.
const redacted = "[redacted]"

var redactedJSON = []byte(`"` + redacted + `"`)

const (
	labelKeyBytes   = 32
	linkTokenBytes  = 32
	linkTokenLength = 43 // raw (unpadded) base64url of 32 bytes
	labelMACDomain  = "claims.manual-label.v1|"
)

// LabelKey is the server-held HMAC-SHA256 key for manual-label MACs. It never enters the
// database, a receipt, an audit row or a log; the zero value is unusable.
type LabelKey struct {
	key [labelKeyBytes]byte
	set bool
}

// NewLabelKey accepts exactly 32 bytes (cmd/api decodes COMMERCE_CLAIMS_LABEL_KEY and
// checks it differs from every other configured key). Any other length is
// command.ErrInvalid. The input slice is copied.
func NewLabelKey(raw []byte) (LabelKey, error) {
	if len(raw) != labelKeyBytes {
		return LabelKey{}, command.ErrInvalid
	}
	var k LabelKey
	copy(k.key[:], raw)
	k.set = true
	return k, nil
}

// Redaction: every fmt verb and JSON render LabelKey as "[redacted]".
func (LabelKey) String() string               { return redacted }
func (LabelKey) GoString() string             { return redacted }
func (LabelKey) Format(f fmt.State, _ rune)   { _, _ = f.Write([]byte(redacted)) }
func (LabelKey) MarshalJSON() ([]byte, error) { return redactedJSON, nil }

// labelMAC is hex(HMAC-SHA256(key, "claims.manual-label.v1|tenant|store|session|label")).
// It is keyed, so the receipt request digest that contains it cannot be dictionary-tested
// for a display name without the server key (§4.2 RecordManualClaim). Pure.
func labelMAC(key LabelKey, tenantID, storeID, sessionID, label string) string {
	mac := hmac.New(sha256.New, key.key[:])
	_, _ = mac.Write([]byte(labelMACDomain + tenantID + "|" + storeID + "|" + sessionID + "|" + label))
	return hex.EncodeToString(mac.Sum(nil))
}

// LinkToken is the 256-bit bearer capability for one bundle's prefill (§6): 43 characters
// of raw base64url. Only its SHA-256 is stored. Every formatting path is redacted; the
// HTTP adapter reads the value with string(token) exactly once, after COMMIT.
type LinkToken string

// ParseLinkToken accepts only the canonical form: 43 characters that strictly decode to 32
// bytes and re-encode to the same text (no padding, no non-zero trailing bits, no
// standard-alphabet characters). Anything else is command.ErrInvalid.
func ParseLinkToken(raw string) (LinkToken, error) {
	if len(raw) != linkTokenLength {
		return "", command.ErrInvalid
	}
	decoded, err := base64.RawURLEncoding.Strict().DecodeString(raw)
	if err != nil || len(decoded) != linkTokenBytes || base64.RawURLEncoding.EncodeToString(decoded) != raw {
		return "", command.ErrInvalid
	}
	return LinkToken(raw), nil
}

// Redaction: every fmt verb and JSON render LinkToken as "[redacted]".
func (LinkToken) String() string               { return redacted }
func (LinkToken) GoString() string             { return redacted }
func (LinkToken) Format(f fmt.State, _ rune)   { _, _ = f.Write([]byte(redacted)) }
func (LinkToken) MarshalJSON() ([]byte, error) { return redactedJSON, nil }

// hash is the only form of a link token that reaches SQL.
func (t LinkToken) hash() []byte {
	sum := sha256.Sum256([]byte(t))
	return sum[:]
}

// newLinkToken draws 32 bytes from crypto/rand (which never fails on supported
// platforms since Go 1.24; it aborts the process instead of returning weak bytes).
func newLinkToken() LinkToken {
	raw := make([]byte, linkTokenBytes)
	_, _ = rand.Read(raw)
	return LinkToken(base64.RawURLEncoding.EncodeToString(raw))
}

// newActorKey is a fresh server-random manual actor key: 64 lowercase hex characters.
func newActorKey() string {
	raw := make([]byte, 32)
	_, _ = rand.Read(raw)
	return hex.EncodeToString(raw)
}

// IssuedLink is IssueLink's result. Token is non-empty only on the first execution and is
// never stored or marshalled; a replay returns Token "" and Replayed true.
type IssuedLink struct {
	Token      LinkToken // "" when Replayed; never stored or marshalled
	Generation int64
	ExpiresAt  time.Time
	Released   bool
	Replayed   bool
}

// Redaction: IssuedLink carries a fresh token, so it renders as "[redacted]" as a whole.
func (IssuedLink) String() string               { return redacted }
func (IssuedLink) GoString() string             { return redacted }
func (IssuedLink) Format(f fmt.State, _ rune)   { _, _ = f.Write([]byte(redacted)) }
func (IssuedLink) MarshalJSON() ([]byte, error) { return redactedJSON, nil }
