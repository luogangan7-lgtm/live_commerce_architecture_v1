// claim_intake.go owns the consumer-side qualification of a Meta comment for keyword claims
// (meta-claims-intake-v1 §3): which of the already authenticated, decrypted units may claim,
// the keyed actor key, and the redacted key type that carries COMMERCE_CLAIMS_ACTOR_KEY.
//
// Non-goals: no SQL (consumer.go calls meta_inbox.stage_claim_intake with the result), no
// network, no River, no claims/live table access, no storage of comment text, from.name or
// username, and no Graph or private-reply code (internal/integrations/metareply).
// It imports only claims/grammar (Parse, a pure function) from claims, never internal/claims itself.
//
// Facts (retrieved 2026-09-28, https://developers.facebook.com/docs/graph-api/webhooks/reference/page/
// and .../reference/instagram/): Page feed comment value carries post_id, comment_id, parent_id,
// from{id}, message; IG comments/live_comments value carries id, text, from{id}, media{id},
// parent_id. U1/U8 of the contract stay UNKNOWN (LIVE probes MCI11).

package meta

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"regexp"

	"livecommerce/internal/claims/grammar"
)

const (
	claimsActorKeyEnv    = "COMMERCE_CLAIMS_ACTOR_KEY"
	claimsActorKeyDomain = "meta-claim-actor/v1"
	claimsRedacted       = "[redacted]"
)

// metaRef matches a platform comment or object id as stored in claims.meta_intake.
var metaRef = regexp.MustCompile(`^[0-9_]{1,80}$`)

// ClaimsActorKey is the server-held HMAC-SHA256 key (K_actor) of ClaimActorKey. It never enters
// the database, a log or an error; the zero value means "staging off".
type ClaimsActorKey struct {
	key [32]byte
	set bool
}

func (ClaimsActorKey) String() string               { return claimsRedacted }
func (ClaimsActorKey) GoString() string             { return claimsRedacted }
func (ClaimsActorKey) Format(f fmt.State, _ rune)   { _, _ = f.Write([]byte(claimsRedacted)) }
func (ClaimsActorKey) MarshalJSON() ([]byte, error) { return []byte(`"` + claimsRedacted + `"`), nil }

// NewClaimsActorKey accepts exactly 32 bytes that are not all zero (copied).
func NewClaimsActorKey(raw []byte) (ClaimsActorKey, error) {
	if len(raw) != 32 {
		return ClaimsActorKey{}, ErrRuntimeConfig
	}
	var k ClaimsActorKey
	copy(k.key[:], raw)
	if k.key == ([32]byte{}) {
		return ClaimsActorKey{}, ErrRuntimeConfig
	}
	k.set = true
	return k, nil
}

// LoadClaimsActorKey reads COMMERCE_CLAIMS_ACTOR_KEY (standard base64 of 32 bytes). Unset or
// empty returns (zero, false, nil): claim staging is off and the consumer is byte-identical to
// MC01-07. Anything else that is not exactly canonical base64 of a valid key is ErrRuntimeConfig.
func LoadClaimsActorKey(getenv func(string) string) (ClaimsActorKey, bool, error) {
	if getenv == nil {
		return ClaimsActorKey{}, false, ErrRuntimeConfig
	}
	encoded := getenv(claimsActorKeyEnv)
	if encoded == "" {
		return ClaimsActorKey{}, false, nil
	}
	decoded, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil || len(encoded) != 44 || base64.StdEncoding.EncodeToString(decoded) != encoded {
		return ClaimsActorKey{}, false, ErrRuntimeConfig
	}
	k, err := NewClaimsActorKey(decoded)
	if err != nil {
		return ClaimsActorKey{}, false, err
	}
	return k, true, nil
}

// ClaimActorKey is hex(HMAC-SHA256(k, json.Marshal([]string{"meta-claim-actor/v1", object,
// assetID, fromID}))): 64 lowercase hex characters, keyed because sender ids are enumerable and
// domain-separated from peer_key, the manual label MAC and the reply-link derivation. Per
// object/asset, never per app. The zero key returns "" (no actor can be derived).
func ClaimActorKey(k ClaimsActorKey, object, assetID, fromID string) string {
	if !k.set {
		return ""
	}
	mac := hmac.New(sha256.New, k.key[:])
	_, _ = mac.Write(canonical([]string{claimsActorKeyDomain, object, assetID, fromID}))
	return hex.EncodeToString(mac.Sum(nil))
}

// claimCandidate is the in-memory result of qualifyClaim. FromID is the raw sender id, used only
// to derive the actor key; it is never stored or logged. Parsed carries the comment head.
type claimCandidate struct {
	ObjectID, CommentRef, FromID string
	Parsed                       grammar.Result
}

func (claimCandidate) String() string     { return "meta.claimCandidate{redacted}" }
func (c claimCandidate) GoString() string { return c.String() }
func (claimCandidate) MarshalJSON() ([]byte, error) {
	return []byte(`"meta.claimCandidate{redacted}"`), nil
}

// qualifyClaim applies the §3 table to one decrypted, canonical change unit. Only page comment
// "add" and Instagram comments/live_comments qualify. It fails closed (false, and the social fact
// still commits) when: the kind is not qualifying or disagrees with the unit; from.id, the
// object id or the comment id is missing or malformed; from.id is the asset itself (the seller's
// own comment); the comment is a reply (parent_id present and, on a Page, different from the
// post id: Meta sends parent_id = post_id for a top-level Page comment, and no comment id can
// equal a post id, so this admits no reply); or the text is over 256 bytes (the grammar
// short-circuit). Pure; the text is parsed and dropped here.
func qualifyClaim(objectName, assetID, kind string, unit []byte) (claimCandidate, bool) {
	var none claimCandidate
	root, err := ParseStrict(unit)
	if err != nil || !digits(assetID) {
		return none, false
	}
	value, ok := object(root["value"])
	if !ok {
		return none, false
	}
	var objectID, commentRef, textKey string
	switch {
	case kind == "page_comment_add" && objectName == "page":
		if stringField(root, "field") != "feed" || stringField(value, "item") != "comment" || stringField(value, "verb") != "add" {
			return none, false
		}
		objectID, commentRef, textKey = stringField(value, "post_id"), stringField(value, "comment_id"), "message"
	case (kind == "instagram_comment" || kind == "instagram_live_comment") && objectName == "instagram":
		field := "comments"
		if kind == "instagram_live_comment" {
			field = "live_comments"
		}
		if stringField(root, "field") != field {
			return none, false
		}
		media, ok := object(value["media"])
		if !ok {
			return none, false
		}
		objectID, commentRef, textKey = stringField(media, "id"), stringField(value, "id"), "text"
	default:
		return none, false
	}
	from, ok := object(value["from"])
	if !ok {
		return none, false
	}
	fromID := stringField(from, "id")
	if !digits(fromID) || fromID == assetID || !metaRef.MatchString(objectID) || !metaRef.MatchString(commentRef) {
		return none, false
	}
	if raw, present := value["parent_id"]; present {
		parent, isString := raw.(string)
		if !isString || (parent != "" && !(objectName == "page" && parent == objectID)) {
			return none, false
		}
	}
	text := ""
	if raw, present := value[textKey]; present {
		s, isString := raw.(string)
		if !isString {
			return none, false
		}
		text = s
	}
	if len(text) > grammar.MaxTextBytes {
		return none, false
	}
	return claimCandidate{ObjectID: objectID, CommentRef: commentRef, FromID: fromID, Parsed: grammar.Parse(text)}, true
}
