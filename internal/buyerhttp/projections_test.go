package buyerhttp

import (
	"encoding/json"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"livecommerce/internal/checkout"
	"livecommerce/internal/fulfillment"
	"livecommerce/internal/pagination"
	"livecommerce/internal/pricing"
	"livecommerce/internal/storefront"
)

func assertExactKeys(t *testing.T, value any, want map[string][]string) []byte {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	var decoded any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	var walk func(string, any)
	walk = func(path string, node any) {
		t.Helper()
		switch current := node.(type) {
		case map[string]any:
			got := make([]string, 0, len(current))
			for key, child := range current {
				got = append(got, key)
				walk(path+"."+key, child)
			}
			sort.Strings(got)
			keys, ok := want[path]
			if !ok {
				t.Errorf("unexpected object at %s with keys %v", path, got)
				return
			}
			keys = append([]string(nil), keys...)
			sort.Strings(keys)
			if !reflect.DeepEqual(got, keys) {
				t.Errorf("%s keys = %v, want exactly %v", path, got, keys)
			}
			seen[path] = true
		case []any:
			for i, child := range current {
				walk(path+"["+strconv.Itoa(i)+"]", child)
			}
		}
	}
	walk("$", decoded)
	if len(seen) != len(want) {
		for path := range want {
			if !seen[path] {
				t.Errorf("expected object at %s was not visited", path)
			}
		}
	}
	return raw
}

func TestProjectCartExactKeysAndNonNullItems(t *testing.T) {
	got := projectCart(storefront.Cart{ID: "cart-1", Currency: "USD", Version: 3, Items: []storefront.Item{{SKUID: "sku-1", Quantity: 2}}})
	raw := assertExactKeys(t, got, map[string][]string{"$": {"id", "currency", "version", "items"}, "$.items[0]": {"sku_id", "quantity"}})
	if got.ID != "cart-1" || got.Currency != "USD" || got.Version != 3 || got.Items[0] != (cartItemResponse{SKUID: "sku-1", Quantity: 2}) || !strings.Contains(string(raw), `"items":[{"sku_id":"sku-1","quantity":2}]`) {
		t.Fatalf("cart projection lost values: %s", raw)
	}
	empty := projectCart(storefront.Cart{Currency: "USD", Items: nil})
	if empty.Items == nil {
		t.Fatal("empty cart items must be a non-nil slice")
	}
	if raw := assertExactKeys(t, empty, map[string][]string{"$": {"id", "currency", "version", "items"}}); !strings.Contains(string(raw), `"items":[]`) {
		t.Fatalf("empty items encoded as null: %s", raw)
	}
}

func TestProjectQuoteExactKeysSnapshotAndNonNullLines(t *testing.T) {
	created, expires := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC), time.Date(2026, 9, 1, 12, 1, 0, 0, time.UTC)
	quote := storefront.Quote{
		ID: "quote-1", CartID: "cart-1", Currency: "USD", CalculationVersion: "private-calculation-version",
		CartVersion: 4, MarketVersion: 9, CreatedAt: created, ExpiresAt: expires,
		Policy: pricing.Policy{MarketID: "market-1", Country: "TW", Method: "delivery-home", Version: 9, ShippingMinor: 50, TaxRateBPS: 500, QuoteTTLSeconds: 60, Enabled: true},
		Lines:  []storefront.QuoteLine{{SKUID: "sku-1", ProductID: "private-product-id", Code: "ITEM-1", Name: "Tea", Description: "Tea leaves", SKUVersion: 7, ProductVersion: 8, Quantity: 2, UnitPriceMinor: 1250, Amount: pricing.LineAmount{SubtotalMinor: 2500, TaxMinor: 125, TotalMinor: 2625}}},
		Amount: pricing.Calculation{SubtotalMinor: 2500, DiscountMinor: 0, ShippingMinor: 50, ShippingTaxMinor: 3, TaxMinor: 125, TotalMinor: 2678, Lines: []pricing.LineAmount{{SubtotalMinor: 2500}}},
	}
	got := projectQuote(quote)
	wantKeys := map[string][]string{
		"$":                 {"id", "cart_id", "cart_version", "market_id", "country", "method", "currency", "created_at", "expires_at", "lines", "amount"},
		"$.lines[0]":        {"sku_id", "code", "name", "description", "quantity", "unit_price_minor", "amount"},
		"$.lines[0].amount": {"subtotal_minor", "discount_minor", "tax_minor", "total_minor"},
		"$.amount":          {"subtotal_minor", "discount_minor", "shipping_minor", "shipping_tax_minor", "tax_minor", "total_minor"},
	}
	raw := assertExactKeys(t, got, wantKeys)
	if got.ID != quote.ID || got.MarketID != quote.Policy.MarketID || got.Country != quote.Policy.Country || got.Method != quote.Policy.Method || got.CreatedAt != created || got.ExpiresAt != expires || got.Lines[0].Name != "Tea" || got.Lines[0].UnitPriceMinor != 1250 || got.Amount.TotalMinor != 2678 {
		t.Fatalf("quote projection lost snapshot values: %+v", got)
	}
	for _, forbidden := range []string{"private-product-id", "private-calculation-version", "market_version", "sku_version", "product_version", "tax_rate_bps", "quote_ttl_seconds", "enabled"} {
		if strings.Contains(string(raw), forbidden) {
			t.Errorf("quote leaked %q: %s", forbidden, raw)
		}
	}
	empty := projectQuote(storefront.Quote{Policy: pricing.Policy{MarketID: "market-1", Country: "TW", Method: "cvs_711"}})
	if empty.Lines == nil {
		t.Fatal("empty quote lines must be non-nil")
	}
	if raw := assertExactKeys(t, empty, map[string][]string{
		"$":        {"id", "cart_id", "cart_version", "market_id", "country", "method", "currency", "created_at", "expires_at", "lines", "amount"},
		"$.amount": {"subtotal_minor", "discount_minor", "shipping_minor", "shipping_tax_minor", "tax_minor", "total_minor"},
	}); !strings.Contains(string(raw), `"lines":[]`) {
		t.Fatalf("empty lines encoded as null: %s", raw)
	}
}

func TestProjectDestinationExactKeysAndPickupAllowlist(t *testing.T) {
	destination := storefront.Destination{
		ID: "destination-1", Version: 2, CartID: "cart-1", CartVersion: 4, Kind: "cvs_familymart", Country: "TW",
		RecipientName: "Buyer Name", Phone: "+886900000001",
		HomeAddress: storefront.HomeAddress{Region: "north", City: "Taipei", PostalCode: "100", Line1: "1 Main St", Line2: "Unit 2"},
		SelectedAt:  time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC), ExpiresAt: time.Date(2026, 9, 1, 13, 0, 0, 0, time.UTC),
		Pickup: &fulfillment.Pickup{ID: "pickup-1", Kind: "cvs_familymart", Namespace: "fixture.local", Code: "017888", Version: 7, Country: "TW", Name: "Shop", Address: "2 Side St", VerificationKind: "MANUAL_ATTESTED", AttestedAt: time.Now(), ValidUntil: time.Now().Add(time.Hour)},
	}
	got := projectDestination(destination)
	want := map[string][]string{
		"$":              {"id", "version", "cart_id", "cart_version", "kind", "country", "recipient_name", "phone", "home_address", "pickup", "selected_at", "expires_at"},
		"$.home_address": {"region", "city", "postal_code", "line1", "line2"},
		"$.pickup":       {"id", "kind", "namespace", "code", "name", "address", "country"},
	}
	raw := assertExactKeys(t, got, want)
	if got.RecipientName != destination.RecipientName || got.Phone != destination.Phone || got.HomeAddress.Line1 != "1 Main St" || got.Pickup == nil || got.Pickup.Code != "017888" {
		t.Fatalf("destination projection lost buyer workflow values: %+v", got)
	}
	for _, forbidden := range []string{"verification_kind", "attested_at", "valid_until", "evidence_ref", "principal_id", "SYNTHETIC_SECRET"} {
		if strings.Contains(string(raw), forbidden) {
			t.Errorf("destination leaked %q", forbidden)
		}
	}
	withoutPickup := projectDestination(storefront.Destination{ID: "destination-2"})
	if withoutPickup.Pickup != nil {
		t.Fatal("absent pickup must remain omitted")
	}
	if raw := assertExactKeys(t, withoutPickup, map[string][]string{"$": {"id", "version", "cart_id", "cart_version", "kind", "country", "recipient_name", "phone", "home_address", "selected_at", "expires_at"}, "$.home_address": {"region", "city", "postal_code", "line1", "line2"}}); strings.Contains(string(raw), `"pickup"`) {
		t.Fatalf("absent pickup was serialized: %s", raw)
	}
}

func TestProjectCheckoutReceiptOnly(t *testing.T) {
	expires := time.Date(2026, 9, 1, 12, 15, 0, 0, time.UTC)
	result := checkout.Result{OrderID: "order-1", ReservationID: "private-reservation", Generation: 8, ExpiresAt: expires, JobID: 42,
		PaymentMode: "pay_at_pickup", CommercialState: "CONFIRMED"}
	got := projectCheckout(result)
	// taiwan-cvs-logistics-v1 §16.2: the receipt also names the payment mode and the state at placement; never ids or the job.
	raw := assertExactKeys(t, got, map[string][]string{"$": {"order_id", "hold_expires_at", "payment_mode", "commercial_state"}})
	if got.OrderID != result.OrderID || !got.HoldExpiresAt.Equal(expires) || got.PaymentMode != "pay_at_pickup" || got.CommercialState != "CONFIRMED" {
		t.Fatalf("receipt lost values: %+v", got)
	}
	for _, forbidden := range []string{"private-reservation", "reservation_id", "generation", "job_id", "42"} {
		if strings.Contains(string(raw), forbidden) {
			t.Errorf("checkout response leaked %q: %s", forbidden, raw)
		}
	}
}

func TestProjectOrderSummaryExactKeys(t *testing.T) {
	order := checkout.OrderSummary{OrderID: "order-id", CreatedAt: time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC), CartID: "cart-id", CartVersion: 3, CommercialState: "DRAFT", FulfillmentState: "MANUAL_UNASSIGNED", Currency: "USD", TotalMinor: 1234}
	got := projectOrders(pagination.Page[checkout.OrderSummary]{Items: []checkout.OrderSummary{order}, NextCursor: "opaque"})
	assertExactKeys(t, got, map[string][]string{
		"$":          {"items", "next_cursor"},
		"$.items[0]": {"order_id", "created_at", "cart_id", "cart_version", "commercial_state", "fulfillment_state", "currency", "total_minor"},
	})
	if got.Items[0].TotalMinor != 1234 || got.Items[0].CartVersion != 3 || got.NextCursor != "opaque" {
		t.Fatal("summary lost frozen fields")
	}
	empty := projectOrders(pagination.Page[checkout.OrderSummary]{})
	if empty.Items == nil || len(empty.Items) != 0 || empty.NextCursor != "" {
		t.Fatal("empty history must be []")
	}
}

func TestProjectOrderExactDisplayAndDraftOnlyExpiry(t *testing.T) {
	expires := time.Date(2026, 9, 1, 12, 15, 0, 0, time.UTC)
	order := checkout.Order{
		Result:          checkout.Result{OrderID: "order-1", ReservationID: "private-reservation", Generation: 8, ExpiresAt: expires, JobID: 42},
		CommercialState: "DRAFT", FulfillmentState: "MANUAL_UNASSIGNED",
		Snapshot: checkout.Snapshot{
			Quote: storefront.Quote{ID: "private-quote-id", CartID: "order-cart-id", CartVersion: 7, MarketVersion: 5, Currency: "USD", Policy: pricing.Policy{MarketID: "private-market-id", Version: 6},
				Lines:  []storefront.QuoteLine{{SKUID: "sku-1", ProductID: "private-product-id", Code: "ITEM-1", Name: "Tea", Description: "Tea leaves", SKUVersion: 3, ProductVersion: 4, Quantity: 2, UnitPriceMinor: 1250, Amount: pricing.LineAmount{SubtotalMinor: 2500, TaxMinor: 125, TotalMinor: 2625}}},
				Amount: pricing.Calculation{SubtotalMinor: 2500, ShippingMinor: 50, ShippingTaxMinor: 3, TaxMinor: 125, TotalMinor: 2678, Lines: []pricing.LineAmount{{SubtotalMinor: 2500}}}},
			Destination: storefront.Destination{ID: "private-destination-id", Version: 3, CartID: "private-cart-id", CartVersion: 4, Kind: "cvs_familymart", Country: "TW", RecipientName: "Buyer Name", Phone: "+886900000001", HomeAddress: storefront.HomeAddress{City: "Taipei", Line1: "3 Main St"}, Pickup: &fulfillment.Pickup{ID: "private-pickup-id", Kind: "cvs_familymart", Namespace: "fixture.local", Code: "017888", Name: "Shop", Address: "4 Side St", Country: "TW", VerificationKind: "private-proof", Version: 9}},
			Service:     fulfillment.Service{MarketID: "private-market-id", Country: "TW", Code: "home_standard", Version: 2, PolicyMethod: "delivery:home_standard", PolicyVersion: 3, Currency: "USD", NameHans: "配送", NameHant: "配送", NameEN: "Standard delivery", DeliveryKind: "home", Mode: "MANUAL", BindingID: "private-provider-binding", BindingVersion: 11},
			Allocation:  fulfillment.Allocation{MarketID: "private-market-id", Country: "TW", Code: "home_standard", Version: 4, ServiceVersion: 2, WarehouseIDs: []string{"private-warehouse-id"}},
		},
	}
	got := projectOrder(order)
	want := map[string][]string{
		"$":                                   {"order_id", "cart_id", "cart_version", "commercial_state", "fulfillment_state", "hold_expires_at", "snapshot", "shipment", "payment_mode", "collection_state", "cvs_shipment"},
		"$.snapshot":                          {"quote", "destination", "service"},
		"$.snapshot.quote":                    {"currency", "lines", "amount"},
		"$.snapshot.quote.lines[0]":           {"sku_id", "code", "name", "description", "quantity", "unit_price_minor", "amount"},
		"$.snapshot.quote.lines[0].amount":    {"subtotal_minor", "discount_minor", "tax_minor", "total_minor"},
		"$.snapshot.quote.amount":             {"subtotal_minor", "discount_minor", "shipping_minor", "shipping_tax_minor", "tax_minor", "total_minor"},
		"$.snapshot.destination":              {"kind", "country", "recipient_name", "phone", "home_address", "pickup"},
		"$.snapshot.destination.home_address": {"region", "city", "postal_code", "line1", "line2"},
		"$.snapshot.destination.pickup":       {"kind", "namespace", "code", "name", "address", "country"},
		"$.snapshot.service":                  {"code", "name_hans", "name_hant", "name_en", "delivery_kind", "mode"},
	}
	raw := assertExactKeys(t, got, want)
	if got.CartID != "order-cart-id" || got.CartVersion != 7 {
		t.Fatal("order cart provenance must come from immutable quote")
	}
	if got.OrderID != order.OrderID || got.HoldExpiresAt == nil || !got.HoldExpiresAt.Equal(expires) || got.Snapshot.Quote.Lines[0].UnitPriceMinor != 1250 || got.Snapshot.Quote.Amount.TotalMinor != 2678 || got.Snapshot.Destination.RecipientName != "Buyer Name" || got.Snapshot.Destination.Pickup == nil || got.Snapshot.Destination.Pickup.Code != "017888" || got.Snapshot.Service.NameEN != "Standard delivery" {
		t.Fatalf("order display lost required values: %+v", got)
	}
	for _, forbidden := range []string{"private-reservation", "generation", "job_id", "warehouse_ids", "private-warehouse-id", "binding_id", "private-provider-binding", "allocation", "private-pickup-id", "private-quote-id", "private-cart-id", "private-market-id", "product_id", "sku_version", "product_version", "verification_kind", "private-proof"} {
		if strings.Contains(string(raw), forbidden) {
			t.Errorf("order response leaked %q: %s", forbidden, raw)
		}
	}
	order.CommercialState = "PAID"
	paid := projectOrder(order)
	if paid.HoldExpiresAt != nil {
		t.Fatal("non-DRAFT order retained a hold expiry")
	}
	if raw := assertExactKeys(t, paid, map[string][]string{
		"$":                                   {"order_id", "cart_id", "cart_version", "commercial_state", "fulfillment_state", "snapshot", "shipment", "payment_mode", "collection_state", "cvs_shipment"},
		"$.snapshot":                          {"quote", "destination", "service"},
		"$.snapshot.quote":                    {"currency", "lines", "amount"},
		"$.snapshot.quote.lines[0]":           {"sku_id", "code", "name", "description", "quantity", "unit_price_minor", "amount"},
		"$.snapshot.quote.lines[0].amount":    {"subtotal_minor", "discount_minor", "tax_minor", "total_minor"},
		"$.snapshot.quote.amount":             {"subtotal_minor", "discount_minor", "shipping_minor", "shipping_tax_minor", "tax_minor", "total_minor"},
		"$.snapshot.destination":              {"kind", "country", "recipient_name", "phone", "home_address", "pickup"},
		"$.snapshot.destination.home_address": {"region", "city", "postal_code", "line1", "line2"},
		"$.snapshot.destination.pickup":       {"kind", "namespace", "code", "name", "address", "country"},
		"$.snapshot.service":                  {"code", "name_hans", "name_hant", "name_en", "delivery_kind", "mode"},
	}); strings.Contains(string(raw), `"hold_expires_at"`) {
		t.Fatalf("non-DRAFT hold expiry serialized: %s", raw)
	}
	order.Snapshot.Destination.Pickup = nil
	order.Snapshot.Quote.Lines = nil
	withoutOptionals := projectOrder(order)
	if withoutOptionals.Snapshot.Quote.Lines == nil || withoutOptionals.Snapshot.Destination.Pickup != nil {
		t.Fatal("empty order quote lines must be [] and absent pickup must remain omitted")
	}
	if raw := assertExactKeys(t, withoutOptionals, map[string][]string{
		"$":                                   {"order_id", "cart_id", "cart_version", "commercial_state", "fulfillment_state", "snapshot", "shipment", "payment_mode", "collection_state", "cvs_shipment"},
		"$.snapshot":                          {"quote", "destination", "service"},
		"$.snapshot.quote":                    {"currency", "lines", "amount"},
		"$.snapshot.quote.amount":             {"subtotal_minor", "discount_minor", "shipping_minor", "shipping_tax_minor", "tax_minor", "total_minor"},
		"$.snapshot.destination":              {"kind", "country", "recipient_name", "phone", "home_address"},
		"$.snapshot.destination.home_address": {"region", "city", "postal_code", "line1", "line2"},
		"$.snapshot.service":                  {"code", "name_hans", "name_hant", "name_en", "delivery_kind", "mode"},
	}); strings.Contains(string(raw), `"hold_expires_at"`) || !strings.Contains(string(raw), `"lines":[]`) {
		t.Fatalf("empty display projection not canonical: %s", raw)
	}
}
