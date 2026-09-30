package merchantorders

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"livecommerce/internal/command"
	"livecommerce/internal/pagination"
	"livecommerce/internal/platform"
)

const orderID = "33333333-3333-4333-8333-333333333333"
const token = "merchant-test-token-0123456789-abcdef"

var scope = platform.Scope{TenantID: "11111111-1111-4111-8111-111111111111", StoreID: "22222222-2222-4222-8222-222222222222", PrincipalID: "44444444-4444-4444-8444-444444444444", Revision: 2}

func summary() map[string]any {
	return map[string]any{"order_id": orderID, "created_at": "2026-09-25T04:05:06.123456Z", "updated_at": "2026-09-25T04:06:06.123456Z", "currency": "TWD", "total_minor": 110, "commercial_state": "CONFIRMED", "fulfillment_state": "MANUAL_UNASSIGNED", "payment_state": "CAPTURED", "test_mode": true, "work_state": "READY", "refunded_minor": 0, "refund_pending_minor": 0,
		"pickup_source": nil, "payment_mode": "card", "collection_state": nil}
}

func detail() map[string]any {
	v := summary()
	v["country"] = "TW"
	v["service_code"] = "manual_home"
	v["items"] = []any{map[string]any{"sku_id": "55555555-5555-4555-8555-555555555555", "code": "SKU", "name": "Tea", "quantity": 1, "unit_price_minor": 100,
		"amount": map[string]any{"subtotal_minor": 100, "discount_minor": 0, "tax_minor": 5, "total_minor": 100}}}
	v["totals"] = map[string]any{"subtotal_minor": 100, "discount_minor": 0, "shipping_minor": 10, "shipping_tax_minor": 1, "tax_minor": 6, "total_minor": 110}
	v["destination"] = map[string]any{"kind": "home", "country": "TW", "recipient_name": "Buyer", "phone": "+886900000001", "home_address": map[string]any{"region": "", "city": "Taipei", "postal_code": "", "line1": "3 Main St", "line2": ""}, "pickup": nil}
	v["shipment"] = nil
	return v
}

func raw(v any) []byte { b, _ := json.Marshal(v); return b }

func TestStrictProjectionAndInclusiveAmount(t *testing.T) {
	v, err := decodeDetail(raw(detail()))
	if err != nil || v.TotalMinor != 110 || v.Destination.Pickup != nil {
		t.Fatalf("valid inclusive detail: %+v %v", v, err)
	}
	for name, mutate := range map[string]func(map[string]any){
		"missing scalar":               func(v map[string]any) { delete(v, "test_mode") },
		"null scalar":                  func(v map[string]any) { v["total_minor"] = nil },
		"secret field":                 func(v map[string]any) { v["owner_id"] = orderID },
		"invalid time":                 func(v map[string]any) { v["created_at"] = "2026-09-25T04:05:06Z" },
		"state":                        func(v map[string]any) { v["payment_state"] = "PAID" },
		"work without attempt":         func(v map[string]any) { v["payment_state"] = "NOT_STARTED"; v["test_mode"] = false },
		"allocation failure is review": func(v map[string]any) { v["fulfillment_state"] = "PAID_ALLOCATION_FAILED" },
		"service code":                 func(v map[string]any) { v["service_code"] = "bad/code" },
		"phone":                        func(v map[string]any) { v["destination"].(map[string]any)["phone"] = "NOT-PHONE" },
		"bad total":                    func(v map[string]any) { v["totals"].(map[string]any)["total_minor"] = 116 },
		"missing amount": func(v map[string]any) {
			delete(v["items"].([]any)[0].(map[string]any)["amount"].(map[string]any), "tax_minor")
		},
		"home pickup": func(v map[string]any) { v["destination"].(map[string]any)["pickup"] = map[string]any{} },
	} {
		t.Run(name, func(t *testing.T) {
			v := detail()
			mutate(v)
			if _, err := decodeDetail(raw(v)); !errors.Is(err, ErrUnavailable) {
				t.Fatalf("accepted: %v", err)
			}
		})
	}
	// Exclusive tax remains valid: the line and whole-order totals include tax.
	v2 := detail()
	v2["items"].([]any)[0].(map[string]any)["amount"].(map[string]any)["total_minor"] = 105
	v2["totals"].(map[string]any)["total_minor"] = 116
	v2["total_minor"] = 116
	if _, err := decodeDetail(raw(v2)); err != nil {
		t.Fatalf("valid exclusive detail: %v", err)
	}
	// Frozen pickup codes are strings, including leading zeroes.
	v3 := detail()
	d := v3["destination"].(map[string]any)
	d["kind"] = "cvs_711"
	d["home_address"] = map[string]any{"region": "", "city": "", "postal_code": "", "line1": "", "line2": ""}
	d["pickup"] = map[string]any{"kind": "cvs_711", "namespace": "fixture.local", "code": "000123", "name": "Shop", "address": "2 St", "verification_kind": "MANUAL_ATTESTED"}
	v3["pickup_source"] = "merchant_attested"
	if got, err := decodeDetail(raw(v3)); err != nil || got.Destination.Pickup.Code != "000123" {
		t.Fatalf("pickup: %+v %v", got.Destination.Pickup, err)
	}
	// taiwan-cvs-logistics-v1 C4: every pickup kind is admitted only with its matching label, all four chains decode.
	for _, tc := range []struct{ kind, verification, source string }{
		{"cvs_711", "PROVIDER_DIRECTORY_VERIFIED", "ecpay_directory"}, {"cvs_familymart", "BUYER_ENTERED", "buyer_entered"},
		{"cvs_hilife", "MANUAL_ATTESTED", "merchant_attested"}, {"cvs_okmart", "BUYER_ENTERED", "buyer_entered"}} {
		vk := detail()
		dk := vk["destination"].(map[string]any)
		dk["kind"] = tc.kind
		dk["home_address"] = map[string]any{"region": "", "city": "", "postal_code": "", "line1": "", "line2": ""}
		dk["pickup"] = map[string]any{"kind": tc.kind, "namespace": "fixture.local", "code": "000123", "name": "Shop", "address": "2 St", "verification_kind": tc.verification}
		vk["pickup_source"] = tc.source
		if _, err := decodeDetail(raw(vk)); err != nil {
			t.Fatalf("%s/%s: %v", tc.kind, tc.verification, err)
		}
		vk["pickup_source"] = "merchant_attested"
		if tc.source != "merchant_attested" {
			if _, err := decodeDetail(raw(vk)); !errors.Is(err, ErrUnavailable) {
				t.Fatalf("%s: mismatched label accepted: %v", tc.kind, err)
			}
		}
	}
	// A pay_at_pickup order is CONFIRMED with no payment attempt (payment NOT_STARTED, work NONE) and a collection state.
	vp := detail()
	vp["payment_state"], vp["work_state"], vp["test_mode"], vp["payment_mode"], vp["collection_state"] = "NOT_STARTED", "NONE", false, "pay_at_pickup", "PENDING"
	if _, err := decodeDetail(raw(vp)); err != nil {
		t.Fatalf("pay_at_pickup detail: %v", err)
	}
	for name, mutate := range map[string]func(map[string]any){
		"card with collection state":  func(v map[string]any) { v["collection_state"] = "PENDING" },
		"pay_at_pickup without state": func(v map[string]any) { v["collection_state"] = nil },
		"unknown collection state":    func(v map[string]any) { v["collection_state"] = "PAID" },
		"pay_at_pickup with capture":  func(v map[string]any) { v["payment_state"] = "CAPTURED" },
		"unknown payment mode":        func(v map[string]any) { v["payment_mode"] = "cash" },
	} {
		vx := detail()
		if name != "card with collection state" && name != "unknown payment mode" {
			vx["payment_state"], vx["work_state"], vx["test_mode"], vx["payment_mode"], vx["collection_state"] = "NOT_STARTED", "NONE", false, "pay_at_pickup", "PENDING"
		}
		mutate(vx)
		if _, err := decodeDetail(raw(vx)); !errors.Is(err, ErrUnavailable) {
			t.Fatalf("%s accepted: %v", name, err)
		}
	}
	// PROVIDER_LABEL_CREATED needs a CONFIRMED card order with READY work, exactly like MERCHANT_SHIPPED.
	vl := detail()
	vl["fulfillment_state"] = "PROVIDER_LABEL_CREATED"
	if _, err := decodeDetail(raw(vl)); err != nil {
		t.Fatalf("label created: %v", err)
	}
	d["home_address"].(map[string]any)["city"] = "Taipei"
	if _, err := decodeDetail(raw(v3)); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("nonempty pickup address accepted: %v", err)
	}
}

type fakeTx struct {
	pgx.Tx
	calls      int
	projection []byte
	err        error
	args       []any
}
type fakeRow struct{ scan func(...any) error }

func (r fakeRow) Scan(dest ...any) error { return r.scan(dest...) }
func (t *fakeTx) QueryRow(_ context.Context, query string, args ...any) pgx.Row {
	t.calls++
	if t.calls == 1 {
		t.args = args
		return fakeRow{func(dest ...any) error {
			if t.err != nil {
				return t.err
			}
			*dest[0].(*[]byte) = t.projection
			return nil
		}}
	}
	if !strings.Contains(query, "resolve_access") {
		panic("unexpected authorization query")
	}
	return fakeRow{func(dest ...any) error {
		*dest[0].(*string) = "ok"
		*dest[1].(*string) = scope.TenantID
		*dest[2].(*string) = scope.PrincipalID
		*dest[3].(*int64) = scope.Revision
		return nil
	}}
}

func TestListSQLBindingsAndErrorClasses(t *testing.T) {
	tx := &fakeTx{projection: raw([]any{summary()})}
	page, err := List(context.Background(), tx, scope, token, ListRequest{})
	if err != nil || len(page.Items) != 1 || page.NextCursor != "" || tx.calls != 2 {
		t.Fatalf("page=%+v calls=%d err=%v", page, tx.calls, err)
	}
	if tx.args[3] != 51 || tx.args[4] != nil || tx.args[5] != nil || tx.args[6] != "all" {
		t.Fatalf("SQL args: %v", tx.args[3:])
	}
	wantHash := sha256.Sum256([]byte(token))
	if got := tx.args[0].([]byte); string(got) != string(wantHash[:]) {
		t.Fatal("unhashed token binding")
	}
	bind := pagination.Binding{TenantID: scope.TenantID, StoreID: scope.StoreID, Collection: "merchant-orders", Filter: "DRAFT"}
	cursor, _ := pagination.Encode(bind, []string{"2026-09-25T04:05:06.123456Z", orderID})
	tx = &fakeTx{projection: raw([]any{})}
	_, err = List(context.Background(), tx, scope, token, ListRequest{State: "DRAFT", Page: pagination.Request{Limit: 2, Cursor: cursor}})
	if err != nil || tx.args[3] != 3 || tx.args[4] != "2026-09-25T04:05:06.123456Z" || tx.args[5] != orderID {
		t.Fatalf("cursor args=%v err=%v", tx.args[3:], err)
	}
	for code, expected := range map[string]error{"PT401": platform.ErrUnauthorized, "PT403": platform.ErrForbidden, "PT404": platform.ErrScopeNotFound, "XX001": ErrUnavailable} {
		tx = &fakeTx{err: &pgconn.PgError{Code: code}}
		_, err := Get(context.Background(), tx, scope, token, orderID)
		if !errors.Is(err, expected) {
			t.Fatalf("%s => %v", code, err)
		}
	}
	if _, err := Get(context.Background(), &fakeTx{}, scope, token, "bad-id"); !errors.Is(err, command.ErrInvalid) {
		t.Fatalf("bad id: %v", err)
	}
}

func TestListFailsWholePage(t *testing.T) {
	bad := summary()
	bad["payment_state"] = nil
	tx := &fakeTx{projection: raw([]any{summary(), bad})}
	if _, err := List(context.Background(), tx, scope, token, ListRequest{Page: pagination.Request{Limit: 1}}); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("partial page: %v", err)
	}
	if tx.calls != 2 {
		t.Fatalf("missing final fence: %d", tx.calls)
	}
}

func TestGetExactDetailAndMissingOrder(t *testing.T) {
	tx := &fakeTx{projection: raw([]any{detail()})}
	got, err := Get(context.Background(), tx, scope, token, orderID)
	if err != nil || got.OrderID != orderID || tx.args[2] != orderID || tx.args[3] != 1 || tx.args[4] != nil || tx.args[5] != nil || tx.args[6] != "all" {
		t.Fatalf("detail=%+v args=%v err=%v", got, tx.args, err)
	}
	tx = &fakeTx{projection: raw([]any{})}
	if _, err := Get(context.Background(), tx, scope, token, orderID); !errors.Is(err, command.ErrNotFound) || tx.calls != 2 {
		t.Fatalf("missing detail/fence: err=%v calls=%d", err, tx.calls)
	}
}

func shipment() map[string]any {
	return map[string]any{"version": 1, "status": "SHIPPED", "carrier_code": "seven_eleven_cvs", "carrier_name": nil,
		"tracking_number": "0012345678", "tracking_url": nil, "recorded_at": "2026-09-25T05:06:07.123456Z"}
}

// stripe-refund-v1 §7.1 and manual-fulfilment-v1 §5.1 projection invariants.
func TestProjectionInvariantsRefundAndShipment(t *testing.T) {
	shipped := func(v map[string]any) { v["fulfillment_state"] = "MERCHANT_SHIPPED"; v["shipment"] = shipment() }
	ok := map[string]func(map[string]any){
		"shipped order with head": shipped,
		"shipped, url and name set": func(v map[string]any) {
			shipped(v)
			v["shipment"].(map[string]any)["tracking_url"] = "https://track.example.com/x?id=1"
			v["shipment"].(map[string]any)["carrier_name"] = "Local Courier"
			v["shipment"].(map[string]any)["carrier_code"] = "other"
		},
		"partial refund": func(v map[string]any) {
			v["payment_state"], v["refunded_minor"], v["refund_pending_minor"] = "PARTIALLY_REFUNDED", 10, 20
		},
		"full refund": func(v map[string]any) { v["payment_state"], v["refunded_minor"] = "REFUNDED", 110 },
		"refunded and shipped": func(v map[string]any) {
			shipped(v)
			v["payment_state"], v["refunded_minor"] = "REFUNDED", 110
		},
		"refund in flight keeps CAPTURED": func(v map[string]any) { v["refund_pending_minor"] = 110 },
		"READY work with a post-capture review": func(v map[string]any) {
			v["payment_state"] = "REVIEW_REQUIRED"
		},
		"refunded late payment": func(v map[string]any) {
			v["payment_state"], v["refunded_minor"] = "REVIEW_REQUIRED", 110
		},
	}
	for name, mutate := range ok {
		t.Run("accept "+name, func(t *testing.T) {
			v := detail()
			mutate(v)
			if _, err := decodeDetail(raw(v)); err != nil {
				t.Fatalf("rejected: %v", err)
			}
		})
	}
	bad := map[string]func(map[string]any){
		"shipped without head":       func(v map[string]any) { v["fulfillment_state"] = "MERCHANT_SHIPPED" },
		"head without shipped state": func(v map[string]any) { v["shipment"] = shipment() },
		"voided head shown": func(v map[string]any) {
			shipped(v)
			v["shipment"].(map[string]any)["status"] = "VOIDED"
		},
		"shipped needs READY work": func(v map[string]any) {
			shipped(v)
			v["work_state"], v["payment_state"] = "REVIEW_REQUIRED", "REVIEW_REQUIRED"
		},
		"shipment missing key":   func(v map[string]any) { shipped(v); delete(v["shipment"].(map[string]any), "recorded_at") },
		"shipment extra key":     func(v map[string]any) { shipped(v); v["shipment"].(map[string]any)["note"] = "x" },
		"shipment null tracking": func(v map[string]any) { shipped(v); v["shipment"].(map[string]any)["tracking_number"] = nil },
		"shipment bad carrier":   func(v map[string]any) { shipped(v); v["shipment"].(map[string]any)["carrier_code"] = "dhl" },
		"other without name": func(v map[string]any) {
			shipped(v)
			v["shipment"].(map[string]any)["carrier_code"] = "other"
		},
		"shipment http url": func(v map[string]any) {
			shipped(v)
			v["shipment"].(map[string]any)["tracking_url"] = "http://track.example.com/x"
		},
		"shipment missing key in detail": func(v map[string]any) { delete(v, "shipment") },
		"missing refund key":             func(v map[string]any) { delete(v, "refunded_minor") },
		"null refund":                    func(v map[string]any) { v["refund_pending_minor"] = nil },
		"refund above total": func(v map[string]any) {
			v["payment_state"], v["refunded_minor"], v["refund_pending_minor"] = "PARTIALLY_REFUNDED", 100, 20
		},
		"captured with refunded amount": func(v map[string]any) { v["refunded_minor"] = 10 },
		"partially refunded at total":   func(v map[string]any) { v["payment_state"], v["refunded_minor"] = "PARTIALLY_REFUNDED", 110 },
		"refunded below total":          func(v map[string]any) { v["payment_state"], v["refunded_minor"] = "REFUNDED", 10 },
		"pending without capture": func(v map[string]any) {
			v["payment_state"], v["work_state"], v["commercial_state"] = "PENDING", "NONE", "AWAITING_PAYMENT"
			v["refund_pending_minor"] = 10
		},
		"READY on cancelled fulfilment":   func(v map[string]any) { v["fulfillment_state"] = "CANCELLED" },
		"confirmed without capture state": func(v map[string]any) { v["payment_state"] = "AUTHORIZED" },
	}
	for name, mutate := range bad {
		t.Run("reject "+name, func(t *testing.T) {
			v := detail()
			mutate(v)
			if _, err := decodeDetail(raw(v)); !errors.Is(err, ErrUnavailable) {
				t.Fatalf("accepted: %v", err)
			}
		})
	}
	// The summary key set is exact too (list rows carry no shipment key).
	s := summary()
	s["shipment"] = nil
	if _, err := decodeSummary(raw(s)); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("summary accepted a shipment key: %v", err)
	}
	if got, err := decodeDetail(raw(func() map[string]any { v := detail(); shipped(v); return v }())); err != nil || got.Shipment == nil ||
		got.Shipment.TrackingNumber != "0012345678" || got.Shipment.CarrierName != nil {
		t.Fatalf("shipment fields: %+v %v", got.Shipment, err)
	}
}

func TestListStateFiltersShippedAndUnshipped(t *testing.T) {
	shippedRow := summary()
	shippedRow["fulfillment_state"] = "MERCHANT_SHIPPED"
	for _, tc := range []struct {
		state string
		rows  []any
		ok    bool
	}{
		{"shipped", []any{shippedRow}, true},
		{"shipped", []any{summary()}, false},
		{"unshipped", []any{summary()}, true},
		{"unshipped", []any{shippedRow}, false},
		{"CONFIRMED", []any{shippedRow}, true},
		{"DRAFT", []any{summary()}, false},
		{"bogus", []any{summary()}, false},
	} {
		tx := &fakeTx{projection: raw(tc.rows)}
		_, err := List(context.Background(), tx, scope, token, ListRequest{State: tc.state})
		if tc.ok != (err == nil) {
			t.Fatalf("state %s rows=%s: err=%v", tc.state, raw(tc.rows), err)
		}
		if tc.state == "bogus" && tx.calls != 0 {
			t.Fatal("invalid state reached the database")
		}
		if tc.ok && tx.args[6] != tc.state {
			t.Fatalf("state binding %v", tx.args[6])
		}
	}
}
