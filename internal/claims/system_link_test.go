// Pure unit tests for system_link.go: key validation, redaction, fingerprint and token vector.
// Non-goals: no database (MCI gates in tests/ own issue_system_link / check_meta_reply).

package claims

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

	"livecommerce/internal/command"
)

const (
	bundleA = "44444444-4444-4444-8444-444444444444"
	opA     = "55555555-5555-4555-8555-555555555555"
)

func testReplyKey(t *testing.T, fill byte) ReplyLinkKey {
	t.Helper()
	k, err := NewReplyLinkKey(bytes.Repeat([]byte{fill}, 32))
	if err != nil {
		t.Fatal(err)
	}
	return k
}

func TestReplyLinkKeyValidationAndRedaction(t *testing.T) {
	for name, raw := range map[string][]byte{"nil": nil, "31": make([]byte, 31), "33": bytes.Repeat([]byte{1}, 33), "zero": make([]byte, 32)} {
		if _, err := NewReplyLinkKey(raw); !errors.Is(err, command.ErrInvalid) {
			t.Fatalf("%s accepted: %v", name, err)
		}
	}
	raw := bytes.Repeat([]byte{7}, 32)
	k, err := NewReplyLinkKey(raw)
	if err != nil {
		t.Fatal(err)
	}
	before := k.ID()
	raw[0] = 9 // the key must have been copied
	if k.ID() != before {
		t.Fatal("key aliases the caller slice")
	}
	hexKey := hex.EncodeToString(bytes.Repeat([]byte{7}, 32))
	for _, out := range []string{fmt.Sprintf("%v %+v %#v %s %x %d", k, k, k, k, k, k), fmt.Sprint(&k)} {
		if strings.Contains(out, hexKey) || strings.Contains(out, "0707") {
			t.Fatalf("key leaked: %s", out)
		}
	}
	if j, _ := json.Marshal(k); string(j) != `"[redacted]"` {
		t.Fatalf("json = %s", j)
	}
	if (ReplyLinkKey{}).ID() != "" {
		t.Fatal("zero key has an id")
	}
}

func TestSystemLinkTokenVector(t *testing.T) {
	k := testReplyKey(t, 7)
	mac := hmac.New(sha256.New, bytes.Repeat([]byte{7}, 32))
	mac.Write([]byte(`["meta-claim-link/v1","` + tenantA + `","` + storeA + `","` + bundleA + `","` + opA + `"]`))
	want := base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
	got, err := SystemLinkToken(k, tenantA, storeA, bundleA, opA)
	if err != nil || string(got) != want {
		t.Fatalf("token mismatch: %v", err)
	}
	if _, err := ParseLinkToken(string(got)); err != nil {
		t.Fatalf("token is not a LinkToken: %v", err)
	}
	idMac := hmac.New(sha256.New, bytes.Repeat([]byte{7}, 32))
	idMac.Write([]byte("meta-claim-link-key-id/v1"))
	if k.ID() != hex.EncodeToString(idMac.Sum(nil))[:16] || len(k.ID()) != 16 {
		t.Fatalf("id = %s", k.ID())
	}
}

func TestSystemLinkTokenSeparationAndInvalid(t *testing.T) {
	k := testReplyKey(t, 7)
	base, _ := SystemLinkToken(k, tenantA, storeA, bundleA, opA)
	other, _ := SystemLinkToken(testReplyKey(t, 8), tenantA, storeA, bundleA, opA)
	swapped, _ := SystemLinkToken(k, tenantA, storeA, opA, bundleA)
	next, _ := SystemLinkToken(k, tenantA, storeA, bundleA, sessionA)
	if base == other || base == swapped || base == next || k.ID() == testReplyKey(t, 8).ID() {
		t.Fatal("token or key id did not separate key/operand changes")
	}
	if again, _ := SystemLinkToken(k, tenantA, storeA, bundleA, opA); again != base {
		t.Fatal("token not deterministic")
	}
	for _, bad := range []string{"", "x", "AAAAAAAA-AAAA-4AAA-8AAA-AAAAAAAAAAAA", opA + " ", "55555555555545558555555555555555"} {
		if _, err := SystemLinkToken(k, tenantA, storeA, bundleA, bad); !errors.Is(err, command.ErrInvalid) {
			t.Fatalf("non-canonical id %q accepted: %v", bad, err)
		}
	}
	if _, err := SystemLinkToken(ReplyLinkKey{}, tenantA, storeA, bundleA, opA); !errors.Is(err, command.ErrInvalid) {
		t.Fatalf("zero key accepted: %v", err)
	}
	if strings.Contains(fmt.Sprint(base), string(base)) {
		t.Fatal("token leaked through fmt")
	}
}
