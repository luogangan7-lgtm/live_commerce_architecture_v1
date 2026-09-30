package fulfillment

import (
	"encoding/json"
	"math"
	"strings"
	"testing"
)

func TestValidPickupInput(t *testing.T) {
	t.Parallel()
	base := PickupInput{
		Kind: "cvs_711", Namespace: "merchant:tw.v1", Code: "00000123",
		Name: "門市", Address: "台北市信義區 1 號", EvidenceRef: "review:42", TTLSeconds: 604800,
	}
	if !validPickupInput(base) {
		t.Fatal("valid leading-zero source rejected")
	}
	// The manual attestation path serves all four chains (taiwan-cvs-logistics-v1 TD6); only 'home' is not a pickup kind.
	for _, kind := range []string{"cvs_familymart", "cvs_hilife", "cvs_okmart"} {
		in := base
		in.Kind = kind
		if !validPickupInput(in) {
			t.Fatalf("manual pickup of kind %s rejected", kind)
		}
	}
	cases := []struct {
		name string
		edit func(*PickupInput)
	}{
		{"kind", func(v *PickupInput) { v.Kind = "home" }},
		{"namespace", func(v *PickupInput) { v.Namespace = "Merchant" }},
		{"namespace length", func(v *PickupInput) { v.Namespace = "a" + strings.Repeat("b", 64) }},
		{"code", func(v *PickupInput) { v.Code = "001.23" }},
		{"code length", func(v *PickupInput) { v.Code = strings.Repeat("0", 33) }},
		{"negative version", func(v *PickupInput) { v.ExpectedVersion = -1 }},
		{"max version", func(v *PickupInput) { v.ExpectedVersion = math.MaxInt64 }},
		{"blank name", func(v *PickupInput) { v.Name = " " }},
		{"control address", func(v *PickupInput) { v.Address = "x\n" }},
		{"invalid UTF8", func(v *PickupInput) { v.EvidenceRef = string([]byte{0xff}) }},
		{"long address", func(v *PickupInput) { v.Address = strings.Repeat("a", 401) }},
		{"zero TTL", func(v *PickupInput) { v.TTLSeconds = 0 }},
		{"excess TTL", func(v *PickupInput) { v.TTLSeconds = 604801 }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			v := base
			tc.edit(&v)
			if validPickupInput(v) {
				t.Fatal("invalid source accepted")
			}
		})
	}
}

func TestPickupProjectionAndLockKey(t *testing.T) {
	t.Parallel()
	p := Pickup{Code: "00000123", VerificationKind: "MANUAL_ATTESTED"}
	encoded, err := json.Marshal(p)
	if err != nil || !strings.Contains(string(encoded), `"code":"00000123"`) ||
		strings.Contains(string(encoded), "evidence_ref") || strings.Contains(string(encoded), "principal_id") {
		t.Fatalf("unsafe projection: %s err=%v", encoded, err)
	}
	a := pickupLockKey(testID, testID, "cvs_711", "source_a", "00000123")
	b := pickupLockKey(testID, testID, "cvs_711", "source_b", "00000123")
	if a == b {
		t.Fatal("namespaces share a lock key")
	}
}
