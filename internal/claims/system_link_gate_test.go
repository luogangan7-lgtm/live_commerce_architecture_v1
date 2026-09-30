// MCI01 (meta-claims-intake-v1 §12, tier MOCK/UNIT) for the reply-link derivation, written
// by the independent test_worker from contracts/meta-claims-intake-v1.md §3 and §6.2 and the
// frozen signatures of docs/delivery/units/meta-intake-reply.md, not from the implementation.
//
// Owns: ReplyLinkKey strictness and the key-id vector, the SystemLinkToken vector, and the
// domain-separation proof of §3 ("MCI01 proves that equal key bytes under the different
// domains give unrelated outputs"): the same 32 bytes used as the actor key, the label key,
// the reply-link key and as the input of the unkeyed social peer key must yield four
// pairwise unrelated values.
//
// This is an external test package so it may import the meta package for ClaimActorKey; it
// does not touch unexported claims code. Non-goals: no database, no network.
package claims_test

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"livecommerce/internal/claims"
	"livecommerce/internal/command"
	"livecommerce/internal/integrations/meta"
)

const (
	slTenant    = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"
	slStore     = "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb"
	slBundle    = "cccccccc-cccc-4ccc-8ccc-cccccccccccc"
	slOperation = "dddddddd-dddd-4ddd-8ddd-dddddddddddd"
)

func slBytes(seed byte) []byte {
	b := make([]byte, 32)
	for i := range b {
		b[i] = seed + byte(i)
	}
	return b
}

func slHMAC(key []byte, msg []byte) []byte {
	m := hmac.New(sha256.New, key)
	m.Write(msg)
	return m.Sum(nil)
}

func TestMetaClaimsMCI01ReplyLinkKeyStrictness(t *testing.T) {
	for _, bad := range [][]byte{nil, {}, slBytes(1)[:31], append(slBytes(1), 9), make([]byte, 32)} {
		if _, err := claims.NewReplyLinkKey(bad); !errors.Is(err, command.ErrInvalid) {
			t.Fatalf("NewReplyLinkKey(%d bytes, zero=%t) err=%v want command.ErrInvalid", len(bad), string(bad) == string(make([]byte, len(bad))), err)
		}
	}
	// A zero-value key never derives a token (an unconfigured claims-worker must not plan).
	var zero claims.ReplyLinkKey
	if _, err := claims.SystemLinkToken(zero, slTenant, slStore, slBundle, slOperation); !errors.Is(err, command.ErrInvalid) {
		t.Fatalf("zero key derived a token: %v", err)
	}
}

// TestMetaClaimsMCI01ReplyLinkKeyIDVector: the key id is a fingerprint, hex(HMAC(K,
// "meta-claim-link-key-id/v1"))[:16], so rotation between plan and dispatch changes it (§6.2).
func TestMetaClaimsMCI01ReplyLinkKeyIDVector(t *testing.T) {
	raw := slBytes(1)
	k, err := claims.NewReplyLinkKey(raw)
	if err != nil {
		t.Fatal(err)
	}
	want := hex.EncodeToString(slHMAC(raw, []byte("meta-claim-link-key-id/v1")))[:16]
	if got := k.ID(); got != want || len(got) != 16 {
		t.Fatalf("ID()=%q want %q", got, want)
	}
	other, _ := claims.NewReplyLinkKey(slBytes(2))
	if other.ID() == k.ID() {
		t.Fatal("two keys share a key id")
	}
	// The key is copied at construction.
	before := k.ID()
	for i := range raw {
		raw[i] = 0
	}
	if k.ID() != before {
		t.Fatal("NewReplyLinkKey kept a reference to the caller's slice")
	}
	// The id must not reveal the key: it is a 64-bit prefix of a MAC, never the key bytes.
	if strings.Contains(hex.EncodeToString(slBytes(1)), k.ID()) {
		t.Fatal("key id is a slice of the key")
	}
}

// TestMetaClaimsMCI01SystemLinkTokenVector: token = base64url_raw(HMAC-SHA256(K,
// JSON(["meta-claim-link/v1", tenant, store, bundle, operation]))), 43 chars, canonical.
func TestMetaClaimsMCI01SystemLinkTokenVector(t *testing.T) {
	raw := slBytes(7)
	k, _ := claims.NewReplyLinkKey(raw)
	msg, _ := json.Marshal([]string{"meta-claim-link/v1", slTenant, slStore, slBundle, slOperation})
	want := base64.RawURLEncoding.EncodeToString(slHMAC(raw, msg))
	tok, err := claims.SystemLinkToken(k, slTenant, slStore, slBundle, slOperation)
	if err != nil {
		t.Fatal(err)
	}
	if string(tok) != want || len(tok) != 43 {
		t.Fatalf("token does not match the §6.2 definition (len=%d)", len(tok))
	}
	if _, err := claims.ParseLinkToken(string(tok)); err != nil {
		t.Fatalf("derived token is not a canonical LinkToken: %v", err)
	}
	again, _ := claims.SystemLinkToken(k, slTenant, slStore, slBundle, slOperation)
	if again != tok {
		t.Fatal("derivation is not deterministic: dispatch could not re-derive the planned token")
	}
	// Every input matters.
	variants := map[string][4]string{
		"tenant":    {"aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaab", slStore, slBundle, slOperation},
		"store":     {slTenant, "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbc", slBundle, slOperation},
		"bundle":    {slTenant, slStore, "cccccccc-cccc-4ccc-8ccc-ccccccccccce", slOperation},
		"operation": {slTenant, slStore, slBundle, "dddddddd-dddd-4ddd-8ddd-dddddddddddf"},
	}
	for name, v := range variants {
		other, err := claims.SystemLinkToken(k, v[0], v[1], v[2], v[3])
		if err != nil || other == tok {
			t.Fatalf("changing %s did not change the token (err=%v)", name, err)
		}
	}
	otherKey, _ := claims.NewReplyLinkKey(slBytes(8))
	if o, _ := claims.SystemLinkToken(otherKey, slTenant, slStore, slBundle, slOperation); o == tok {
		t.Fatal("another key derived the same token")
	}
	// The token is redacted under every fmt verb and JSON (it is a bearer capability).
	for _, s := range []string{fmt.Sprintf("%v", tok), fmt.Sprintf("%+v", tok), fmt.Sprintf("%#v", tok), fmt.Sprintf("%s", tok), fmt.Sprint(tok)} {
		if strings.Contains(s, want) {
			t.Fatalf("token printed in clear: %q", s)
		}
	}
	if b, _ := json.Marshal(tok); strings.Contains(string(b), want) {
		t.Fatal("token marshalled in clear")
	}
}

func TestMetaClaimsMCI01SystemLinkTokenRejectsNonCanonicalIDs(t *testing.T) {
	k, _ := claims.NewReplyLinkKey(slBytes(3))
	bad := []string{"", "not-a-uuid", strings.ToUpper(slBundle), strings.ReplaceAll(slBundle, "-", ""), slBundle + " ", "{" + slBundle + "}",
		"cccccccc-cccc-4ccc-8ccc-ccccccccccc", "cccccccc-cccc-4ccc-8ccc-cccccccccccc0"}
	for _, id := range bad {
		for pos, args := range [][4]string{{id, slStore, slBundle, slOperation}, {slTenant, id, slBundle, slOperation}, {slTenant, slStore, id, slOperation}, {slTenant, slStore, slBundle, id}} {
			if tok, err := claims.SystemLinkToken(k, args[0], args[1], args[2], args[3]); !errors.Is(err, command.ErrInvalid) || tok != "" {
				t.Fatalf("id %q at position %d accepted: tok=%d err=%v", id, pos, len(tok), err)
			}
		}
	}
}

// TestMetaClaimsMCI01KeyDomainSeparation is the H3 proof: one byte string configured as
// every secret must not let any process correlate identities across domains. The four values
// are computed from the same bytes and the same nominal inputs and must be pairwise unrelated
// (no equality, no prefix/suffix relation).
func TestMetaClaimsMCI01KeyDomainSeparation(t *testing.T) {
	same := slBytes(0x21)
	actorKey, err := meta.NewClaimsActorKey(same)
	if err != nil {
		t.Fatal(err)
	}
	linkKey, err := claims.NewReplyLinkKey(same)
	if err != nil {
		t.Fatal(err)
	}
	const object, asset, from = "page", "111222333", "777888"
	actor := meta.ClaimActorKey(actorKey, object, asset, from)

	// Unkeyed social peer key (meta-social-peer/v1): hex(SHA-256(JSON(tuple))). It takes no
	// key, so the property is that the actor key cannot be derived from it or equal it.
	peerMsg, _ := json.Marshal([]string{"meta-social-peer/v1", "123456789012345", object, asset, from})
	peerSum := sha256.Sum256(peerMsg)
	peer := hex.EncodeToString(peerSum[:])

	// Manual label MAC (claims frozen §4.2): hex(HMAC(K, "claims.manual-label.v1|tenant|store|session|label")).
	label := hex.EncodeToString(slHMAC(same, []byte("claims.manual-label.v1|"+slTenant+"|"+slStore+"|"+slBundle+"|"+from)))

	tok, err := claims.SystemLinkToken(linkKey, slTenant, slStore, slBundle, slOperation)
	if err != nil {
		t.Fatal(err)
	}
	linkHex := hex.EncodeToString([]byte(base64ToBytes(t, string(tok))))
	keyID := linkKey.ID()

	values := map[string]string{"actor_key": actor, "peer_key": peer, "label_mac": label, "link_token(hex)": linkHex}
	names := []string{"actor_key", "peer_key", "label_mac", "link_token(hex)"}
	for i := 0; i < len(names); i++ {
		v := values[names[i]]
		if len(v) != 64 {
			t.Fatalf("%s is not 64 hex chars: %d", names[i], len(v))
		}
		for j := i + 1; j < len(names); j++ {
			w := values[names[j]]
			if v == w || v[:16] == w[:16] || v[48:] == w[48:] {
				t.Fatalf("%s and %s are related under equal key bytes", names[i], names[j])
			}
		}
		if strings.HasPrefix(v, keyID) {
			t.Fatalf("%s starts with the reply-link key fingerprint", names[i])
		}
	}
	// The actor key is HMAC-keyed under its own domain, so it is not the SHA-256 of any of
	// the other domains' inputs either.
	for name, msg := range map[string][]byte{"tuple": peerMsg, "actor-tuple": mustJSON([]string{"meta-claim-actor/v1", object, asset, from})} {
		sum := sha256.Sum256(msg)
		if hex.EncodeToString(sum[:]) == actor {
			t.Fatalf("actor key equals an unkeyed hash of %s", name)
		}
	}
	// The raw key bytes never appear as any output.
	for name, v := range values {
		if strings.Contains(v, hex.EncodeToString(same)) {
			t.Fatalf("%s contains the raw key", name)
		}
	}
}

func mustJSON(v any) []byte { b, _ := json.Marshal(v); return b }

func base64ToBytes(t *testing.T, s string) []byte {
	t.Helper()
	b, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		t.Fatal(err)
	}
	return b
}
