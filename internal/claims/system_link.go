// system_link.go owns the server-derived claim-link token of the first Meta private reply
// (meta-claims-intake-v1 §6.2): ReplyLinkKey (K_link) and SystemLinkToken, a pure HMAC of the
// bundle and its reply operation. The token is never stored or logged; only sha256(token)
// reaches SQL (claims.issue_system_link / claims.check_meta_reply), and the adapter re-derives
// the same token at Check and Dispatch time.
//
// Non-goals: no key loading or rotation (cmd/claims-worker reads COMMERCE_CLAIMS_REPLY_LINK_KEY;
// a key change makes ID() differ, which Check turns into a policy denial), no storage.

package claims

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"

	"livecommerce/internal/command"
)

const (
	replyLinkKeyBytes  = 32
	systemLinkDomain   = "meta-claim-link/v1"
	replyLinkKeyIDText = "meta-claim-link-key-id/v1"
)

// ReplyLinkKey is the server-held HMAC-SHA256 key K_link; the zero value is unusable.
type ReplyLinkKey struct {
	key [replyLinkKeyBytes]byte
	set bool
}

// NewReplyLinkKey accepts exactly 32 bytes that are not all zero; the input is copied.
func NewReplyLinkKey(raw []byte) (ReplyLinkKey, error) {
	var k ReplyLinkKey
	if len(raw) != replyLinkKeyBytes {
		return ReplyLinkKey{}, command.ErrInvalid
	}
	var acc byte
	for _, b := range raw {
		acc |= b
	}
	if acc == 0 {
		return ReplyLinkKey{}, command.ErrInvalid
	}
	copy(k.key[:], raw)
	k.set = true
	return k, nil
}

// Redaction: every fmt verb and JSON render ReplyLinkKey as "[redacted]".
func (ReplyLinkKey) String() string               { return redacted }
func (ReplyLinkKey) GoString() string             { return redacted }
func (ReplyLinkKey) Format(f fmt.State, _ rune)   { _, _ = f.Write([]byte(redacted)) }
func (ReplyLinkKey) MarshalJSON() ([]byte, error) { return redactedJSON, nil }

// ID is the key fingerprint stored in the operation request (hex of the first 8 MAC bytes); ""
// for an unusable key. It reveals nothing about the key and lets Check detect a key change.
func (k ReplyLinkKey) ID() string {
	if !k.set {
		return ""
	}
	mac := hmac.New(sha256.New, k.key[:])
	_, _ = mac.Write([]byte(replyLinkKeyIDText))
	return hex.EncodeToString(mac.Sum(nil))[:16]
}

// SystemLinkToken is base64url_raw(HMAC-SHA256(K_link, JSON(["meta-claim-link/v1", tenant, store,
// bundle, operation]))): 43 characters in the LinkToken format. The JSON array is an
// unambiguous tuple encoding. A zero key or a non-canonical UUID is command.ErrInvalid. Pure.
func SystemLinkToken(k ReplyLinkKey, tenantID, storeID, bundleID, operationID string) (LinkToken, error) {
	if !k.set || !command.ValidID(tenantID) || !command.ValidID(storeID) || !command.ValidID(bundleID) || !command.ValidID(operationID) {
		return "", command.ErrInvalid
	}
	tuple, err := json.Marshal([]string{systemLinkDomain, tenantID, storeID, bundleID, operationID})
	if err != nil {
		return "", command.ErrInvalid
	}
	mac := hmac.New(sha256.New, k.key[:])
	_, _ = mac.Write(tuple)
	return ParseLinkToken(base64.RawURLEncoding.EncodeToString(mac.Sum(nil)))
}
