package pagination

import (
	"strings"
	"testing"
)

// The customers collection (internal/customers.List, D5) pages by (last_activity_at, id) like merchant-orders;
// its cursor binds tenant, store and the search text (Filter empty or a sha256 hex digest).
func TestCustomersCollectionCursor(t *testing.T) {
	const tenant, store, id = "11111111-1111-4111-8111-111111111111", "22222222-2222-4222-8222-222222222222", "33333333-3333-4333-8333-333333333333"
	const at = "2026-09-30T04:05:06.123456Z"
	for _, filter := range []string{"", strings.Repeat("a", 64)} {
		binding := Binding{TenantID: tenant, StoreID: store, Collection: "customers", Filter: filter}
		cursor, err := Encode(binding, []string{at, id})
		if err != nil {
			t.Fatalf("filter %q: %v", filter, err)
		}
		limit, keys, err := Decode(Request{Cursor: cursor}, binding, 2)
		if err != nil || limit != defaultLimit || len(keys) != 2 || keys[0] != at || keys[1] != id {
			t.Fatalf("filter %q: %d %v %v", filter, limit, keys, err)
		}
		// A cursor never crosses a search text, a store or a collection.
		other := binding
		other.Filter = strings.Repeat("b", 64)
		if _, _, err := Decode(Request{Cursor: cursor}, other, 2); err == nil {
			t.Fatal("cursor accepted for another search text")
		}
		other = binding
		other.StoreID = tenant
		if _, _, err := Decode(Request{Cursor: cursor}, other, 2); err == nil {
			t.Fatal("cursor accepted for another store")
		}
		other = binding
		other.Collection = "merchant-orders"
		other.Filter = "all"
		if _, _, err := Decode(Request{Cursor: cursor}, other, 2); err == nil {
			t.Fatal("customers cursor accepted for merchant-orders")
		}
	}
	for _, bad := range []Binding{
		{TenantID: tenant, StoreID: store, Collection: "customers", Filter: "all"},
		{TenantID: tenant, StoreID: store, Collection: "customers", ParentID: id},
	} {
		if _, err := Encode(bad, []string{at, id}); err == nil {
			t.Fatalf("invalid binding %+v accepted", bad)
		}
	}
	binding := Binding{TenantID: tenant, StoreID: store, Collection: "customers"}
	for _, keys := range [][]string{{at}, {"2026-09-30T04:05:06Z", id}, {at, "nope"}, {at, id, id}} {
		if _, err := Encode(binding, keys); err == nil {
			t.Fatalf("invalid keys %v accepted", keys)
		}
	}
}
