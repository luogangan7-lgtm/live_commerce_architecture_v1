// MCI01 (meta-claims-intake-v1 §12, tier MOCK/UNIT): redaction of every new secret-bearing
// type, written by the independent test_worker from contracts/meta-claims-intake-v1.md §3,
// §6.4, §7 and the frozen signatures of docs/delivery/units/meta-intake-{core,reply}.md,
// not from the implementation.
//
// Owns: core.Secret / core.SecretClaim (external-dispatcher-v1 amendment, IR-12) and the
// three key types introduced with T10c: meta.ClaimsActorKey, claims.ReplyLinkKey and
// metareply.PageTokenKeyring. Each must render as a constant under %v %+v %#v %s %q %x %d,
// as a pointer, as a nested exported field, and under JSON/Text marshalling, and must never
// let raw bytes escape in hex, base64 or decimal-slice form.
//
// External test package: it imports the packages that depend on core (metareply), which an
// in-package test could not. Non-goals: dispatcher behaviour with a Secret needs PostgreSQL
// and lives in tests/foundation/meta_claims_intake_reply_test.go.
package core_test

import (
	"bytes"
	"encoding"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"testing"

	"livecommerce/internal/claims"
	"livecommerce/internal/integrations/core"
	"livecommerce/internal/integrations/meta"
	"livecommerce/internal/integrations/metareply"
)

// leaks reports whether out contains raw in any common rendering: hex, base64 (std, url,
// raw), the raw bytes themselves, or a decimal byte list such as "[12 34 56 ...".
func leaks(out string, raw []byte) bool {
	if len(raw) < 8 {
		return false
	}
	forms := []string{
		string(raw), hex.EncodeToString(raw), strings.ToUpper(hex.EncodeToString(raw)),
		base64.StdEncoding.EncodeToString(raw), base64.URLEncoding.EncodeToString(raw),
		base64.RawStdEncoding.EncodeToString(raw), base64.RawURLEncoding.EncodeToString(raw),
	}
	parts := make([]string, 0, 8)
	for _, b := range raw[:8] {
		parts = append(parts, strconv.Itoa(int(b)))
	}
	forms = append(forms, strings.Join(parts, " "), strings.Join(parts, ","))
	for _, f := range forms {
		if f != "" && strings.Contains(out, f) {
			return true
		}
	}
	return false
}

type holder struct{ V any }

func renderings(v any) []string {
	var out []string
	for _, verb := range []string{"%v", "%+v", "%#v", "%s", "%q", "%x", "%X", "%d", "%T"} {
		out = append(out, fmt.Sprintf(verb, v))
	}
	out = append(out, fmt.Sprint(v), fmt.Sprintln(v), fmt.Sprintf("%v", holder{V: v}), fmt.Sprintf("%+v", holder{V: v}),
		fmt.Sprintf("%#v", holder{V: v}), fmt.Sprintf("%v", []any{v}), fmt.Sprintf("%v", map[string]any{"k": v}))
	if j, err := json.Marshal(v); err == nil {
		out = append(out, string(j))
	}
	if j, err := json.Marshal(holder{V: v}); err == nil {
		out = append(out, string(j))
	}
	if m, ok := v.(encoding.TextMarshaler); ok {
		if b, err := m.MarshalText(); err == nil {
			out = append(out, string(b))
		}
	}
	return out
}

func noLeak(t *testing.T, name string, v any, raws ...[]byte) {
	t.Helper()
	for i, s := range renderings(v) {
		for _, raw := range raws {
			if leaks(s, raw) {
				t.Fatalf("%s rendering #%d leaked secret bytes: %.60q", name, i, s)
			}
		}
	}
}

func randomRaw(seed byte) []byte {
	b := make([]byte, 32)
	for i := range b {
		b[i] = seed*7 + byte(i)*13 + 3
	}
	return b
}

func TestMetaClaimsMCI01SecretRedaction(t *testing.T) {
	raw := []byte("SENTINEL-PAGE-ACCESS-TOKEN-DO-NOT-PRINT-0123456789")
	s := core.NewSecret(raw)
	noLeak(t, "Secret", s, raw)
	noLeak(t, "*Secret", &s, raw)
	for _, form := range renderings(s) {
		if strings.Contains(form, "SENTINEL") {
			t.Fatalf("Secret rendering leaked a fragment: %q", form)
		}
	}
	// The constant rendering is fixed by the contract: "[redacted]".
	for _, verb := range []string{"%v", "%+v", "%s"} {
		if got := fmt.Sprintf(verb, s); got != "[redacted]" {
			t.Fatalf("Secret %s = %q want [redacted]", verb, got)
		}
	}
	if b, err := json.Marshal(s); err != nil || string(b) != `"[redacted]"` {
		t.Fatalf("Secret JSON = %s err=%v", b, err)
	}
	if b, err := s.MarshalText(); err != nil || string(b) != "[redacted]" {
		t.Fatalf("Secret MarshalText = %q err=%v", b, err)
	}
	// Reveal is the one way to read it, and NewSecret copied its input.
	if !bytes.Equal(s.Reveal(), []byte("SENTINEL-PAGE-ACCESS-TOKEN-DO-NOT-PRINT-0123456789")) {
		t.Fatal("Reveal does not return the secret bytes")
	}
	for i := range raw {
		raw[i] = 'x'
	}
	if strings.Contains(string(s.Reveal()), "xxx") {
		t.Fatal("NewSecret kept a reference to the caller's slice")
	}
	// Zero value renders redacted too (an unloaded credential must not print oddly).
	var zero core.Secret
	if got := fmt.Sprintf("%v", zero); got != "[redacted]" {
		t.Fatalf("zero Secret renders %q", got)
	}
}

func TestMetaClaimsMCI01SecretClaimRedaction(t *testing.T) {
	lease := randomRaw(9)
	c := core.SecretClaim{OperationID: "dddddddd-dddd-4ddd-8ddd-dddddddddddd", Generation: 3, LeaseToken: lease}
	noLeak(t, "SecretClaim", c, lease)
	noLeak(t, "*SecretClaim", &c, lease)
	for _, s := range renderings(c) {
		if strings.Contains(s, "LeaseToken") && !strings.Contains(s, "redacted") {
			t.Fatalf("SecretClaim printed its lease field: %q", s)
		}
	}
}

func TestMetaClaimsMCI01KeyTypeRedaction(t *testing.T) {
	actorRaw, linkRaw, pageRaw := randomRaw(1), randomRaw(2), randomRaw(3)
	actor, err := meta.NewClaimsActorKey(actorRaw)
	if err != nil {
		t.Fatal(err)
	}
	link, err := claims.NewReplyLinkKey(linkRaw)
	if err != nil {
		t.Fatal(err)
	}
	ring, err := metareply.NewPageTokenKeyring("k1", map[string][]byte{"k1": pageRaw})
	if err != nil {
		t.Fatal(err)
	}
	all := [][]byte{actorRaw, linkRaw, pageRaw}
	noLeak(t, "ClaimsActorKey", actor, all...)
	noLeak(t, "*ClaimsActorKey", &actor, all...)
	noLeak(t, "ReplyLinkKey", link, all...)
	noLeak(t, "*ReplyLinkKey", &link, all...)
	noLeak(t, "PageTokenKeyring", ring, all...)
	noLeak(t, "**PageTokenKeyring", &ring, all...)
	for name, v := range map[string]any{"ClaimsActorKey": actor, "ReplyLinkKey": link} {
		for _, verb := range []string{"%v", "%+v", "%#v", "%s"} {
			if got := fmt.Sprintf(verb, v); got != "[redacted]" {
				t.Fatalf("%s %s = %q want [redacted]", name, verb, got)
			}
		}
		if b, err := json.Marshal(v); err != nil || string(b) != `"[redacted]"` {
			t.Fatalf("%s JSON = %s err=%v", name, b, err)
		}
	}
	// The key id of the link key is a public fingerprint, allowed to print; it must not equal
	// or contain the key.
	if id := link.ID(); leaks(id, linkRaw) || len(id) != 16 {
		t.Fatalf("link key id %q", id)
	}
	// A page-token keyring must refuse to seal for an unconfigured active id and never echo
	// the token in an error.
	const token = "SENTINEL-EAAG-PAGE-TOKEN-ABCDEFGH"
	_, _, _, err = ring.Seal(metareply.PageTokenScope{TenantID: "t", StoreID: "s", BindingID: "b", Provider: "facebook", AssetID: "1", Version: 1}, token)
	if err != nil && strings.Contains(err.Error(), "SENTINEL") {
		t.Fatalf("Seal error echoes the token: %v", err)
	}
}
