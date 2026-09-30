package foundation_test

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/url"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"livecommerce/internal/checkout"
	"livecommerce/internal/fulfillment"
	"livecommerce/internal/platform"
	"livecommerce/internal/pricing"
	"livecommerce/internal/storefront"
)

// Keep the consumer-owned wire shape separate from the author's domain DTO.
type boptItem struct {
	MarketID          string `json:"market_id"`
	MarketCode        string `json:"market_code"`
	MarketName        string `json:"market_name"`
	Country           string `json:"country"`
	Currency          string `json:"currency"`
	DeliveryCode      string `json:"delivery_code"`
	Method            string `json:"method"`
	ServiceVersion    int64  `json:"service_version"`
	AllocationVersion int64  `json:"allocation_version"`
	DeliveryKind      string `json:"delivery_kind"`
	Mode              string `json:"mode"`
	NameHans          string `json:"name_hans"`
	NameHant          string `json:"name_hant"`
	NameEN            string `json:"name_en"`
	SortOrder         int    `json:"sort_order"`
}
type boptPage struct {
	Items      []boptItem `json:"items"`
	NextCursor string     `json:"next_cursor"`
}

func boptRead(t *testing.T, h bhHarness, query string) boptPage {
	t.Helper()
	path := "/v1/buyer/checkout-options"
	if query != "" {
		path += "?" + query
	}
	r := h.request(t, "GET", path, h.cap.Token, "", nil, nil)
	p := bhRead[boptPage](t, r, 200)
	var top map[string]json.RawMessage
	var rows struct {
		Items []map[string]json.RawMessage `json:"items"`
	}
	if json.Unmarshal(r.body, &top) != nil || json.Unmarshal(r.body, &rows) != nil || len(top) != 2 || top["items"] == nil || top["next_cursor"] == nil || p.Items == nil {
		t.Fatal("options page projection not exact/non-null")
	}
	want := []string{"allocation_version", "country", "currency", "delivery_code", "delivery_kind", "market_code", "market_id", "market_name", "method", "mode", "name_en", "name_hans", "name_hant", "service_version", "sort_order"}
	for _, row := range rows.Items {
		keys := make([]string, 0, len(row))
		for key := range row {
			keys = append(keys, key)
		}
		slices.Sort(keys)
		// taiwan-cvs-logistics-v1 §5.1: a home row is exactly the base projection; a CVS row adds a closed, mode-dependent set.
		// The store fixture is shared, so an unfiltered page also lists CVS rows other tests configured.
		var kind, selection string
		_ = json.Unmarshal(row["delivery_kind"], &kind)
		_ = json.Unmarshal(row["pickup_selection"], &selection)
		_, unavailable := row["available"]
		exact := want
		switch {
		case kind == "home":
		case selection == "buyer_entered":
			exact = append(slices.Clone(want), "payment_modes", "pickup_selection", "store_search_url")
		case selection == "ecpay_map" && unavailable:
			exact = append(slices.Clone(want), "available", "payment_modes", "pickup_selection", "reason")
		case selection == "ecpay_map":
			exact = append(slices.Clone(want), "payment_modes", "pickup_selection")
		default:
			t.Fatalf("CVS options row without a known pickup_selection: kind=%q selection=%q", kind, selection)
		}
		exact = slices.Clone(exact)
		slices.Sort(exact)
		if !slices.Equal(keys, exact) {
			t.Fatalf("options row contains extra or missing fields: kind=%q got=%v want=%v", kind, keys, exact)
		}
	}
	return p
}

func boptAdd(t *testing.T, h cqHarness, in fulfillment.ServiceInput, warehouses []string) {
	t.Helper()
	p := h.policy
	p.Country, p.Method, p.ExpectedVersion = in.Country, "delivery:"+in.Code, 0
	if _, err := h.setPolicy(p); err != nil {
		t.Fatal(err)
	}
	if _, err := dsSet(h, t04Key("options-service"), in); err != nil {
		t.Fatal(err)
	}
	if _, err := daSet(h, t04Key("options-allocation"), fulfillment.AllocationInput{MarketID: in.MarketID, Country: in.Country, Code: in.Code, ExpectedServiceVersion: 1, WarehouseIDs: warehouses}); err != nil {
		t.Fatal(err)
	}
}

// Fresh stores keep the old storeA2/storeB empty negative controls intact.
// Only synthetic configuration is created, through ordinary merchant APIs.
func boptOtherStore(t *testing.T, h bhHarness, tenant, token string) (store, market string) {
	t.Helper()
	f := *h.f
	f.tenantA, f.storeA1, f.tokens = tenant, randomUUID(), map[string]string{"a": token}
	digest := sha256.Sum256([]byte(token))
	if err := f.owner.QueryRow(context.Background(), `SELECT principal_id::text FROM identity.sessions WHERE token_hash=$1`, digest[:]).Scan(&f.principalA); err != nil {
		t.Fatal(err)
	}
	mustExec(t, f.owner, `INSERT INTO control.stores(tenant_id,id,name,currency) VALUES($1,$2,'Options isolated shop','USD')`, tenant, f.storeA1)
	mustExec(t, f.owner, `INSERT INTO identity.store_grants(tenant_id,store_id,principal_id,permission) VALUES($1,$2,$3,'pricing:write'),($1,$2,$3,'integration:manage'),($1,$2,$3,'store:read')`, tenant, f.storeA1, f.principalA)
	t.Cleanup(func() {
		mustExec(t, f.owner, `DELETE FROM identity.store_grants WHERE tenant_id=$1 AND store_id=$2 AND principal_id=$3`, tenant, f.storeA1, f.principalA)
	})
	m, err := createPricingMarket(context.Background(), &f, "options")
	if err != nil {
		t.Fatal(err)
	}
	other := cqHarness{f: &f, market: m, policy: h.policy}
	other.policy.MarketID = m.ID
	warehouse := randomUUID()
	mustExec(t, f.owner, `INSERT INTO inventory.warehouses(tenant_id,store_id,id,name) VALUES($1,$2,$3,'Options isolated warehouse')`, tenant, f.storeA1, warehouse)
	in := h.delivery
	in.MarketID, in.Code = m.ID, "isolated_"+t04Tag()
	boptAdd(t, other, in, []string{warehouse})
	return f.storeA1, m.ID
}

func TestBuyerHTTPOptionsPaginationScopeAndNoWrites(t *testing.T) {
	h := bhSetup(t)
	second := h.delivery
	second.Code = "z_" + t04Tag()
	second.NameEN = "Second option"
	boptAdd(t, h.cqHarness, second, h.allocation.WarehouseIDs)
	hk := h.delivery
	hk.Country, hk.Code = "HK", "hk_"+t04Tag()
	boptAdd(t, h.cqHarness, hk, h.allocation.WarehouseIDs)
	otherStore, otherMarket := boptOtherStore(t, h, h.f.tenantA, h.f.tokens["a"])
	_, foreignMarket := boptOtherStore(t, h, h.f.tenantB, h.f.tokens["b"])
	before := h.facts(t)
	readFacts := func() []int {
		out := []int{}
		for _, table := range []string{"storefront.carts", "storefront.quotes", "storefront.events", "buyer.command_results", "storefront.destination_snapshots"} {
			out = append(out, countRows(t, h.f.owner, `SELECT count(*) FROM `+table+` WHERE owner_id=$1`, h.cap.Scope.OwnerID))
		}
		out = append(out, countRows(t, h.f.owner, `SELECT count(*) FROM inventory.ledger WHERE sku_id=ANY($1::uuid[])`, []string{h.stock.skus[0].ID, h.stock.skus[1].ID}))
		return out
	}
	beforeReads := readFacts()
	query := "market_id=" + h.market.ID + "&country=TW&limit=1"
	first := boptRead(t, h, query)
	if len(first.Items) != 1 || first.NextCursor == "" {
		t.Fatal("first bounded page missing continuation")
	}
	secondPage := boptRead(t, h, query+"&cursor="+url.QueryEscape(first.NextCursor))
	if len(secondPage.Items) != 1 || secondPage.NextCursor != "" || first.Items[0].DeliveryCode >= secondPage.Items[0].DeliveryCode {
		t.Fatal("keyset overlap/order/terminal cursor")
	}
	row := first.Items[0]
	if row.MarketID != h.market.ID || row.MarketCode != h.market.Code || row.MarketName != h.market.Name || row.Country != "TW" || row.Currency != "USD" || row.Method != "delivery:"+row.DeliveryCode || row.ServiceVersion != 1 || row.AllocationVersion != 1 || row.NameHans != h.delivery.NameHans || row.NameHant != h.delivery.NameHant || row.NameEN != h.delivery.NameEN || row.Mode != "MANUAL" || row.DeliveryKind != "home" {
		t.Fatal("display or consumer fields changed")
	}
	raw, err := base64.RawURLEncoding.DecodeString(first.NextCursor)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{h.f.tenantA, h.f.storeA1, h.cap.Token, h.cap.Scope.OwnerID, h.cap.Scope.SessionID} {
		if strings.Contains(string(raw), secret) {
			t.Fatal("cursor leaks scope or credential")
		}
	}
	if got := boptRead(t, h, "market_id="+h.market.ID); len(got.Items) != 3 {
		t.Fatal("market-only filter lost country")
	}
	if got := boptRead(t, h, "country=HK"); !slices.ContainsFunc(got.Items, func(o boptItem) bool { return o.MarketID == h.market.ID && o.Country == "HK" }) {
		t.Fatal("country-only filter lost candidate")
	}
	if got := boptRead(t, h, ""); len(got.Items) > 50 {
		t.Fatal("default page unbounded")
	}
	for _, id := range []string{otherMarket, foreignMarket, randomUUID()} {
		if len(boptRead(t, h, "market_id="+id).Items) != 0 {
			t.Fatal("foreign/missing market exposed")
		}
	}
	bhError(t, h.request(t, "GET", "/v1/buyer/checkout-options?market_id="+h.market.ID+"&country=HK&cursor="+url.QueryEscape(first.NextCursor), h.cap.Token, "", nil, nil), 422, "invalid_request")
	bhError(t, h.request(t, "GET", "/v1/buyer/catalog?cursor="+url.QueryEscape(first.NextCursor), h.cap.Token, "", nil, nil), 422, "invalid_request")
	other := h
	other.origin = "https://options-other.example"
	other.cap = mustIssue(t, h.cqHarness.service, otherStore)
	bhPublish(t, h.bcHarness, other.origin, h.f.tenantA, otherStore)
	if got := boptRead(t, other, "market_id="+otherMarket); len(got.Items) != 1 {
		t.Fatal("cross-store positive control failed")
	}
	if got := boptRead(t, other, "market_id="+h.market.ID); len(got.Items) != 0 {
		t.Fatal("reverse cross-store exposure")
	}
	bhError(t, other.request(t, "GET", "/v1/buyer/checkout-options?"+query+"&cursor="+url.QueryEscape(first.NextCursor), other.cap.Token, "", nil, nil), 422, "invalid_request")
	if h.facts(t) != before || !reflect.DeepEqual(readFacts(), beforeReads) {
		t.Fatal("discovery changed purchase facts or queued work")
	}
}

func TestBuyerHTTPOptionsCurrentEligibility(t *testing.T) {
	for _, change := range []string{"hidden", "disabled", "api-draft", "stale-policy", "disabled-policy", "empty-allocation", "inactive-warehouse", "inactive-market", "store-currency", "rename"} {
		t.Run(change, func(t *testing.T) {
			h := bhSetup(t)
			query := "market_id=" + h.market.ID
			if len(boptRead(t, h, query).Items) != 1 {
				t.Fatal("missing baseline choice")
			}
			in := h.delivery
			in.ExpectedVersion = 1
			switch change {
			case "hidden", "disabled", "api-draft", "rename":
				switch change {
				case "hidden":
					in.Visible = false
				case "disabled":
					in.Enabled = false
				case "api-draft":
					in.Enabled = false
					in.Mode = "API"
				case "rename":
					in.NameEN = "Renamed current option"
				}
				if _, err := dsSet(h.cqHarness, t04Key("options-change"), in); err != nil {
					t.Fatal(err)
				}
			case "stale-policy", "disabled-policy":
				dsPolicy(t, h.cqHarness, in, 1, 77, change == "stale-policy")
			case "empty-allocation":
				a := h.allocation
				a.ExpectedVersion = 1
				a.WarehouseIDs = nil
				if _, err := daSet(h.cqHarness, t04Key("options-clear"), a); err != nil {
					t.Fatal(err)
				}
			case "inactive-warehouse":
				// Disable just one of two assigned warehouses: EXISTS(active) is insufficient.
				mustExec(t, h.f.owner, `UPDATE inventory.warehouses SET active=false WHERE tenant_id=$1 AND store_id=$2 AND id=$3`, h.f.tenantA, h.f.storeA1, h.allocation.WarehouseIDs[0])
			case "inactive-market":
				_, err := pricingScoped(context.Background(), h.f, h.f.tokens["a"], h.f.storeA1, "pricing:write", func(tx pgx.Tx, s platform.Scope) (pricing.Market, error) {
					return pricing.SetMarketActive(context.Background(), tx, s, t04Key("options-market"), h.market.ID, h.market.Version, false)
				})
				if err != nil {
					t.Fatal(err)
				}
			case "store-currency":
				// Existing composite FKs reject drift even for the fixture owner;
				// never disable constraints to manufacture an impossible market.
				_, err := h.f.owner.Exec(context.Background(), `UPDATE control.stores SET currency='TWD' WHERE tenant_id=$1 AND id=$2`, h.f.tenantA, h.f.storeA1)
				daSQLState(t, err, "23503")
			}
			got := boptRead(t, h, query)
			if change == "rename" {
				if len(got.Items) != 1 || got.Items[0].ServiceVersion != 2 || got.Items[0].AllocationVersion != 1 || got.Items[0].NameEN != in.NameEN {
					t.Fatal("historical allocation provenance incorrectly blocks current service")
				}
			} else if change == "store-currency" {
				if len(got.Items) != 1 || got.Items[0].Currency != "USD" {
					t.Fatal("currency invariant failed to preserve current option")
				}
			} else if len(got.Items) != 0 {
				t.Fatal("ineligible configuration still exposed")
			}
			if change == "stale-policy" {
				in.PolicyVersion = 2
				if _, err := dsSet(h.cqHarness, t04Key("options-realign"), in); err != nil {
					t.Fatal(err)
				}
				if len(boptRead(t, h, query).Items) != 1 {
					t.Fatal("current policy/service restoration missing")
				}
			}
		})
	}
}

func TestBuyerHTTPOptionsDriveHomeCheckout(t *testing.T) {
	h := bhSetup(t)
	// The consumer does not copy market/method/version IDs from fixture config.
	var row boptItem
	query := ""
	for pages := 0; pages < 100; pages++ {
		page := boptRead(t, h, query)
		for _, candidate := range page.Items {
			if candidate.MarketCode == h.market.Code {
				row = candidate
				break
			}
		}
		if row.MarketID != "" || page.NextCursor == "" {
			break
		}
		query = "cursor=" + url.QueryEscape(page.NextCursor)
	}
	if row.MarketID == "" {
		t.Fatal("buyer could not discover expected named market")
	}
	c := bhRead[storefront.Cart](t, h.request(t, "GET", "/v1/buyer/cart", h.cap.Token, "", nil, nil), 200)
	q := bhRead[storefront.Quote](t, h.request(t, "POST", "/v1/buyer/quotes", h.cap.Token, t04Key("options-quote"), storefront.QuoteInput{CartVersion: c.Version, MarketID: row.MarketID, Country: row.Country, Method: row.Method}, nil), 200)
	address := bdHome(c)
	address.ExpectedVersion = h.destination.Version
	address.Country, address.Kind = row.Country, row.DeliveryKind
	d := bhRead[storefront.Destination](t, h.request(t, "PUT", "/v1/buyer/destination", h.cap.Token, t04Key("options-address"), address, nil), 200)
	in := checkout.Input{QuoteID: q.ID, DestinationID: d.ID, CartVersion: c.Version, ServiceVersion: row.ServiceVersion, AllocationVersion: row.AllocationVersion}
	receipt := bhRead[struct {
		OrderID   string    `json:"order_id"`
		ExpiresAt time.Time `json:"hold_expires_at"`
	}](t, h.request(t, "POST", "/v1/buyer/checkout", h.cap.Token, t04Key("options-checkout"), in, nil), 200)
	if receipt.OrderID == "" || !receipt.ExpiresAt.After(time.Now()) {
		t.Fatal("discovered config did not drive a hold")
	}
	bhRead[map[string]json.RawMessage](t, h.request(t, "GET", "/v1/buyer/orders/"+receipt.OrderID, h.cap.Token, "", nil, nil), 200)
}

func TestBuyerHTTPOptionsCVSIsConfigurationOnly(t *testing.T) {
	h := bhSetup(t)
	for _, kind := range []string{"cvs_711", "cvs_familymart"} {
		in := h.delivery
		in.Code, in.DeliveryKind = kind+"_"+t04Tag(), kind
		boptAdd(t, h.cqHarness, in, h.allocation.WarehouseIDs)
	}
	page := boptRead(t, h, "market_id="+h.market.ID+"&country=TW")
	if len(page.Items) != 3 {
		t.Fatal("configured CVS choices missing")
	}
	for _, kind := range []string{"cvs_711", "cvs_familymart"} {
		if !slices.ContainsFunc(page.Items, func(row boptItem) bool { return row.DeliveryKind == kind && row.Country == "TW" }) {
			t.Fatal("CVS kind/country changed")
		}
		// Discovery must not manufacture a trusted pickup source for a CVS choice.
		in := storefront.DestinationInput{ExpectedVersion: h.destination.Version, CartVersion: h.input.CartVersion,
			Kind: kind, Country: "TW", RecipientName: "Synthetic Buyer", Phone: "+886 900 000 000"}
		bhError(t, h.request(t, "PUT", "/v1/buyer/destination", h.cap.Token, t04Key("options-cvs-no-pickup"), in, nil), 422, "invalid_request")
	}
}

func TestBuyerHTTPOptionsStrictQueryAndAuthority(t *testing.T) {
	h := bhSetup(t)
	path := "/v1/buyer/checkout-options"
	for _, query := range []string{"limit=0", "limit=101", "limit=-1", "limit=+1", "limit=1.5", "limit=1&limit=2", "limit=", "country=tw", "country=TWN", "country=", "market_id=bad", "market_id=", "cursor=bad", "cursor=", "cursor=" + strings.Repeat("A", 1025), "tenant_id=" + h.f.tenantA, "limit=1;country=TW", "limit=1&&country=TW", "&limit=1", "limit=1&", "x=%GG", strings.Repeat("a", 2049)} {
		bhError(t, h.request(t, "GET", path+"?"+query, h.cap.Token, "", nil, nil), 422, "invalid_request")
	}
	bhError(t, h.request(t, "GET", path+"?", h.cap.Token, "", nil, nil), 403, "forbidden")
	bhError(t, h.request(t, "GET", path, h.cap.Token, t04Key("read-idem"), nil, nil), 422, "invalid_request")
	bhError(t, h.request(t, "GET", path, h.cap.Token, "", struct{}{}, nil), 422, "invalid_request")
	bhError(t, h.request(t, "POST", path, h.cap.Token, "", nil, nil), 405, "method_not_allowed")
	bhError(t, h.request(t, "GET", path+"/", h.cap.Token, "", nil, nil), 404, "not_found")
	bhError(t, h.request(t, "GET", path, "", "", nil, nil), 401, "unauthorized")
	bhError(t, h.request(t, "GET", path, h.cap.Token, "", nil, func(r *http.Request) { r.Header.Del("X-Commerce-Buyer-BFF-Key") }), 401, "unauthorized")
	for _, header := range []string{"Cookie", "Origin", "X-Tenant-ID", "X-Store-ID"} {
		bhError(t, h.request(t, "GET", path, h.cap.Token, "", nil, func(r *http.Request) { r.Header.Set(header, "forged") }), 403, "forbidden")
	}
	bhError(t, h.request(t, "GET", path, h.cap.Token, "", nil, func(r *http.Request) { r.Header.Set("X-Commerce-Storefront-Origin", "https://unknown.example") }), 404, "not_found")
	mustExec(t, h.f.owner, `UPDATE control.storefront_publications SET published=false WHERE tenant_id=$1 AND store_id=$2`, h.f.tenantA, h.f.storeA1)
	bhError(t, h.request(t, "GET", path, h.cap.Token, "", nil, nil), 404, "not_found")
	mustExec(t, h.f.owner, `UPDATE control.storefront_publications SET published=true WHERE tenant_id=$1 AND store_id=$2`, h.f.tenantA, h.f.storeA1)
	if r := h.request(t, "DELETE", "/v1/buyer/session", h.cap.Token, "", nil, nil); r.status != 204 {
		t.Fatal("revoke failed")
	}
	bhError(t, h.request(t, "GET", path, h.cap.Token, "", nil, nil), 401, "unauthorized")
}
