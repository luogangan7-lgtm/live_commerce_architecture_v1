// Unit cases for the "claim-bundles" collection used by internal/claims.ListBundles
// (contracts/live-keyword-claims-v1.md §4.2, §10 integrator helper). Non-goal: SQL paging,
// which the independent real-PG gates cover.

package pagination

import (
	"encoding/base64"
	"errors"
	"testing"

	"livecommerce/internal/command"
)

func TestClaimBundlesCursorIsSessionBoundTwoKeyAndCanonical(t *testing.T) {
	session := "44444444-4444-4444-8444-444444444444"
	bundles := Binding{TenantID: binding.TenantID, StoreID: binding.StoreID, Collection: "claim-bundles", ParentID: session}
	stamp := "2026-09-28T01:02:03.000456Z"
	encoded, err := Encode(bundles, []string{stamp, key})
	if err != nil {
		t.Fatal(err)
	}
	limit, after, err := Decode(Request{Limit: 100, Cursor: encoded}, bundles, 2)
	if err != nil || limit != 100 || len(after) != 2 || after[0] != stamp || after[1] != key {
		t.Fatalf("claim-bundles roundtrip: limit=%d keys=%v err=%v", limit, after, err)
	}
	if limit, _, err := Decode(Request{}, bundles, 2); err != nil || limit != defaultLimit {
		t.Fatalf("default limit=%d err=%v", limit, err)
	}
	// Another session, store, tenant or collection never accepts this position.
	for _, other := range []Binding{
		{TenantID: binding.TenantID, StoreID: binding.StoreID, Collection: "claim-bundles", ParentID: key},
		{TenantID: binding.TenantID, StoreID: session, Collection: "claim-bundles", ParentID: session},
		{TenantID: session, StoreID: binding.StoreID, Collection: "claim-bundles", ParentID: session},
		{TenantID: binding.TenantID, StoreID: binding.StoreID, Collection: "live-sessions"},
	} {
		if _, _, err := Decode(Request{Cursor: encoded}, other, 2); !errors.Is(err, command.ErrInvalid) {
			t.Fatalf("foreign cursor accepted for %+v: %v", other, err)
		}
	}
	for _, bad := range []Binding{
		{TenantID: binding.TenantID, StoreID: binding.StoreID, Collection: "claim-bundles"},
		{TenantID: binding.TenantID, StoreID: binding.StoreID, Collection: "claim-bundles", ParentID: session, Filter: "amy"},
	} {
		if _, err := Encode(bad, []string{stamp, key}); !errors.Is(err, command.ErrInvalid) {
			t.Fatalf("invalid binding accepted (no session or a label filter): %+v %v", bad, err)
		}
	}
	for _, keys := range [][]string{{key}, {stamp, "not-a-uuid"}, {"2026-09-28T01:02:03Z", key}, {stamp, key, key}} {
		if _, err := Encode(bundles, keys); !errors.Is(err, command.ErrInvalid) {
			t.Fatalf("invalid keys accepted: %v %v", keys, err)
		}
	}
	if _, _, err := Decode(Request{Limit: 101}, bundles, 2); !errors.Is(err, command.ErrInvalid) {
		t.Fatalf("limit above 100 accepted: %v", err)
	}
	raw, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil {
		t.Fatal(err)
	}
	for _, malformed := range []string{encoded + "=", base64.RawURLEncoding.EncodeToString(append([]byte(" "), raw...))} {
		if _, _, err := Decode(Request{Cursor: malformed}, bundles, 2); !errors.Is(err, command.ErrInvalid) {
			t.Fatalf("non-canonical cursor accepted: %q %v", malformed, err)
		}
	}
}
