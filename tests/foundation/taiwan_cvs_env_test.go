package foundation_test

// Shared environment of the Taiwan CVS gates TCV03-TCV06, TCV11, TCV14-TCV17 (contracts/taiwan-cvs-logistics-v1.md). Prefix `tcv`.
//
// It is built ONLY from public seams and the foundation harness (psSetup: a fresh TWD tenant/store with stock, market, policy and a
// home service; bhPublish: an ACTIVE storefront domain): the merchant handler (httpapi.NewHandler with Options.CVS), the buyer handler
// (buyerhttp.New over a checkout.Service with the CVS surface), the provider hooks (CVS.HooksHandler) and the independent ecpaytest fake
// behind ecpay.NewClient. Every ECPay answer is the fake's. Tier MOCK/HTTP_PG: real PG 18, real roles, real River rows.
//
// Owner-pool writes (disclosed fixtures): control.storefront_* rows (bhPublish), identity.store_grants/principals/sessions for member
// variants, and the aged timestamps / planted rows that individual gates list in their own header.

import (
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"

	"livecommerce/internal/buyer"
	"livecommerce/internal/buyerhttp"
	"livecommerce/internal/catalog"
	"livecommerce/internal/checkout"
	"livecommerce/internal/command"
	"livecommerce/internal/fulfillment"
	"livecommerce/internal/httpapi"
	integration "livecommerce/internal/integrations/core"
	"livecommerce/internal/integrations/shipping/ecpay"
	"livecommerce/internal/integrations/shipping/ecpay/ecpayroute"
	"livecommerce/internal/integrations/shipping/ecpay/ecpaytest"
	"livecommerce/internal/inventory"
	"livecommerce/internal/platform"
	"livecommerce/internal/storefront"
)

const tcvHooksOrigin = "https://hooks.tcv.example"

type tcvEnv struct {
	t           *testing.T
	p           psHarness
	fake        *ecpaytest.Fake
	keys        *ecpay.Keyring
	client      *ecpay.Client
	cvs         *fulfillment.CVS
	merchant    http.Handler
	svc         *checkout.Service
	bh          bhHarness
	hooks       http.Handler
	jobs        *river.Client[pgx.Tx]
	mk          ecpaytest.Merchant
	tag         string
	origin      string
	cfg         fulfillment.CVSConfig
	svcs        map[string]fulfillment.ServiceInput
	svcVer      map[string]int64
	r           *rfxEnv  // set when tcvOpts.stripe
	ro          rfxOrder // the rfx store (endpoint, secret) when tcvOpts.stripe
	queue       string   // dispatcher queue (startDispatcher)
	apiCode     string   // the API 7-ELEVEN service code of browser fixtures
	statusSeq   int
	keyringJSON string // the same keyring document, for the child process
	queueOver   string // startDispatcher listens on this queue instead of a fresh one
	rescueAfter time.Duration
	settingsVer int64 // last version of the store CVS settings written through cvsSettings
}

// tcvOpts tunes the environment.
type tcvOpts struct {
	ecpayDisabled bool    // CVS_ECPAY_ENABLED=0 (kill switch)
	payEnv        string  // payment environment; default SANDBOX
	stripe        bool    // build on the rfx Stripe environment (paid card orders through the real capture path)
	origin        string  // the storefront origin published for this store (default: a random https origin)
	share         *tcvEnv // reuse another env's fake, keyring and ECPay client (two stores served by one process)
}

func tcvKeyring(t *testing.T) (*ecpay.Keyring, string) {
	t.Helper()
	doc := fmt.Sprintf(`{"active":"k1","keys":[{"id":"k1","key_base64":%q}]}`, base64.StdEncoding.EncodeToString(randomBytes(32)))
	k, err := ecpay.LoadKeyring(func(name string) string {
		if name == "ECPAY_LOGISTICS_KEYRING" {
			return doc
		}
		return ""
	})
	if err != nil {
		t.Fatalf("keyring: %v", err)
	}
	return k, doc
}

func tcvNew(t *testing.T, opts ...tcvOpts) *tcvEnv {
	t.Helper()
	var o tcvOpts
	if len(opts) > 0 {
		o = opts[0]
	}
	if o.payEnv == "" {
		o.payEnv = "SANDBOX"
	}
	ctx := context.Background()
	var p psHarness
	var rEnv *rfxEnv
	var rOrder rfxOrder
	if o.stripe {
		rEnv = rfxNew(t)
		rOrder = rEnv.storeFor(t)
		p = rOrder.s.p
	} else {
		p = psSetup(t)
	}
	e := &tcvEnv{r: rEnv, ro: rOrder, t: t, p: p, svcs: map[string]fulfillment.ServiceInput{}, svcVer: map[string]int64{}, fake: ecpaytest.New(), tag: t04Tag(), origin: "https://tcv-" + t04Tag() + ".example"}
	if o.origin != "" {
		e.origin = o.origin
	}
	var err error
	if o.share != nil {
		e.fake, e.keys, e.keyringJSON, e.client = o.share.fake, o.share.keys, o.share.keyringJSON, o.share.client
	} else {
		e.keys, e.keyringJSON = tcvKeyring(t)
		if e.client, err = ecpay.NewClient(ecpay.Environment(o.payEnv), e.fake.Transport()); err != nil {
			t.Fatal(err)
		}
	}
	e.cfg = fulfillment.CVSConfig{ECPay: ecpay.Config{Enabled: !o.ecpayDisabled, HooksOrigin: tcvHooksOrigin}, PaymentEnvironment: o.payEnv}
	if e.jobs, err = river.NewClient(riverpgxv5.New(p.f.runtime), &river.Config{Schema: "river"}); err != nil {
		t.Fatal(err)
	}
	if e.cvs, err = fulfillment.NewCVS(p.f.runtime, e.jobs, e.keys, e.client, e.cfg); err != nil {
		t.Fatalf("NewCVS: %v", err)
	}
	e.merchant = e.newMerchantHandler()
	e.hooks = e.cvs.HooksHandler()
	bc, err := checkout.NewBuyerCVS(p.pool, e.keys, e.client, e.cfg)
	if err != nil {
		t.Fatalf("NewBuyerCVS: %v", err)
	}
	e.svc = p.bcHarness.service.WithPaymentEnvironment(o.payEnv).WithBuyerCVS(bc)
	bffKey := base64.RawURLEncoding.EncodeToString(randomBytes(32))
	handler, err := buyerhttp.New(ctx, p.a.issuer, p.a.runtime, e.svc, bffKey, time.Hour)
	if err != nil {
		t.Fatalf("buyerhttp.New: %v", err)
	}
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	bhPublish(t, p.bcHarness, e.origin, p.f.tenantA, p.f.storeA1)
	e.bh = bhHarness{bcHarness: p.bcHarness, server: srv, key: bffKey, origin: e.origin}
	// the fake's directory: the documented stage stores (F5) plus a Hi-Life row (a shared fake already has them)
	if o.share != nil {
		return e
	}
	e.fake.SetStoreList("UNIMART", []ecpaytest.Store{{ID: "131386", Name: "Stage 7-ELEVEN", Addr: "Stage address 7", Phone: "0200000001"}, {ID: "000123", Name: "Leading zero store", Addr: "Zero address"},
		{ID: "1328", Name: "Four char store", Addr: "Four address"}, {ID: "1234567", Name: "Seven char store", Addr: "Seven address"}, {ID: "123456789", Name: "Nine char store", Addr: "Nine address"}})
	e.fake.SetStoreList("FAMI", []ecpaytest.Store{{ID: "006598", Name: "Stage FamilyMart", Addr: "Stage address F"}})
	e.fake.SetStoreList("HILIFE", []ecpaytest.Store{{ID: "2001", Name: "Stage Hi-Life", Addr: "Stage address H"}})
	return e
}

func (e *tcvEnv) store() string  { return e.p.f.storeA1 }
func (e *tcvEnv) tenant() string { return e.p.f.tenantA }
func (e *tcvEnv) token() string  { return e.p.f.tokens["a"] }

// mcall is one merchant HTTP request.
func (e *tcvEnv) mcall(token, method, path, key, body string) (int, map[string]any, []byte) {
	var r io.Reader
	if body != "" {
		r = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, path, r)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	if key != "" {
		req.Header.Set("Idempotency-Key", key)
	}
	w := httptest.NewRecorder()
	e.merchant.ServeHTTP(w, req)
	var out map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	return w.Code, out, w.Body.Bytes()
}

// member creates a merchant principal of the store's tenant with exactly perms (plus store:read) and returns its bearer token.
func (e *tcvEnv) member(perms ...string) (token, principal string) {
	e.t.Helper()
	f := e.p.f
	token, principal = randomToken(), randomUUID()
	mustExec(e.t, f.owner, `INSERT INTO identity.principals(id) VALUES($1)`, principal)
	mustExec(e.t, f.owner, `INSERT INTO identity.memberships(tenant_id,principal_id) VALUES($1,$2)`, f.tenantA, principal)
	for _, perm := range append([]string{"store:read"}, perms...) {
		mustExec(e.t, f.owner, `INSERT INTO identity.store_grants(tenant_id,store_id,principal_id,permission) VALUES($1,$2,$3,$4)`, f.tenantA, f.storeA1, principal, perm)
	}
	tx, err := f.owner.Begin(context.Background())
	if err != nil {
		e.t.Fatal(err)
	}
	if err := insertSession(context.Background(), tx, token, principal, "merchant", time.Now().Add(time.Hour), nil); err != nil {
		e.t.Fatal(err)
	}
	if err := tx.Commit(context.Background()); err != nil {
		e.t.Fatal(err)
	}
	return
}

// grantCreator gives the store creator (the fixture principal) more permissions (disclosed owner-pool fixture; psSetup grants integration:*
// and catalog/inventory/pricing only).
func (e *tcvEnv) grantCreator(perms ...string) {
	e.t.Helper()
	for _, perm := range perms {
		mustExec(e.t, e.p.f.owner, `INSERT INTO identity.store_grants(tenant_id,store_id,principal_id,permission) VALUES($1,$2,$3,$4) ON CONFLICT DO NOTHING`,
			e.p.f.tenantA, e.p.f.storeA1, e.p.f.principalA, perm)
	}
}

// connect registers an ECPay connection through the real merchant route (the probe runs against the fake) and enables it.
// It returns the merchant id. mode is C2C or B2C.
func (e *tcvEnv) connect(mode string) string {
	e.t.Helper()
	// ECPay MerchantIDs are numeric (docs: 2000132, 2000933). Contract 4.1 admits [A-Za-z0-9]{1,10} in SQL, but the adapter's own
	// merchantIDRE is digits-only (finding F-MID), so a lettered id can never pass the probe: the fixtures use numeric ids.
	merchantID := fmt.Sprintf("2%09d", binary.BigEndian.Uint32(randomBytes(4))%1000000000)
	e.mk = ecpaytest.Merchant{ID: merchantID, Key: "k" + t04Tag() + "abcd", IV: "v" + t04Tag() + "wxyz"}
	e.fake.AddMerchant(e.mk)
	body := fmt.Sprintf(`{"expected_version":0,"environment":%q,"mode":%q,"merchant_id":%q,"hash_key":%q,"hash_iv":%q,"sender_name":"寄件人測試","sender_cell_phone":"0911222333"}`,
		e.cfg.PaymentEnvironment, mode, merchantID, e.mk.Key, e.mk.IV)
	status, out, raw := e.mcall(e.token(), "PUT", "/v1/admin/stores/"+e.store()+"/logistics/ecpay", "tcv-connect-"+t04Tag(), body)
	if status != 200 {
		e.t.Fatalf("connect ECPay: %d %s", status, raw)
	}
	version := int64(out["version"].(float64))
	body = fmt.Sprintf(`{"expected_version":%d,"enabled":true}`, version)
	if status, _, raw = e.mcall(e.token(), "POST", "/v1/admin/stores/"+e.store()+"/logistics/ecpay/enabled", "tcv-enable-"+t04Tag(), body); status != 200 {
		e.t.Fatalf("enable ECPay: %d %s", status, raw)
	}
	return merchantID
}

// service creates a delivery service `code` of the given kind/mode with a flat fee (TWD minor) and an allocation; API mode binds the
// store's ecpay_logistics binding. It returns the service code and versions.
func (e *tcvEnv) service(kind, mode string, feeMinor int64) (code string, serviceVersion, allocationVersion int64) {
	e.t.Helper()
	p := e.p
	// chain labels of cvs-ui U2, so the storefront delivery list can be told apart by chain in every locale
	hans := map[string]string{"cvs_711": "7-ELEVEN", "cvs_familymart": "全家", "cvs_hilife": "莱尔富", "cvs_okmart": "OK超商"}
	hant := map[string]string{"cvs_711": "7-ELEVEN", "cvs_familymart": "全家", "cvs_hilife": "萊爾富", "cvs_okmart": "OK超商"}
	en := map[string]string{"cvs_711": "7-ELEVEN", "cvs_familymart": "FamilyMart", "cvs_hilife": "Hi-Life", "cvs_okmart": "OK mart"}
	in := fulfillment.ServiceInput{MarketID: p.market.ID, Country: "TW", Code: "cvs" + t04Tag(), PolicyVersion: 1, NameHans: hans[kind], NameHant: hant[kind], NameEN: en[kind],
		DeliveryKind: kind, Mode: mode, Enabled: true, Visible: true}
	if mode == "API" {
		var binding string
		var bv int64
		if err := p.f.owner.QueryRow(context.Background(), `SELECT a.binding_id::text,b.semantic_version FROM integration.merchant_accounts a JOIN integration.bindings b ON b.tenant_id=a.tenant_id AND b.store_id=a.store_id AND b.id=a.binding_id
		  WHERE a.tenant_id=$1 AND a.store_id=$2 AND a.provider='ecpay_logistics'`, p.f.tenantA, p.f.storeA1).Scan(&binding, &bv); err != nil {
			e.t.Fatalf("ecpay binding: %v", err)
		}
		in.BindingID, in.BindingVersion = binding, bv
	}
	dsPolicy(e.t, p.cqHarness, in, 0, feeMinor, true)
	if _, err := dsSet(p.cqHarness, t04Key("tcv-service"), in); err != nil {
		e.t.Fatalf("set %s/%s service: %v", kind, mode, err)
	}
	if _, err := daSet(p.cqHarness, t04Key("tcv-allocation"), fulfillment.AllocationInput{MarketID: p.market.ID, Country: "TW", Code: in.Code, ExpectedServiceVersion: 1, WarehouseIDs: []string{p.stock.warehouse.ID}}); err != nil {
		e.t.Fatal(err)
	}
	e.svcs[in.Code], e.svcVer[in.Code] = in, 1
	return in.Code, 1, 1
}

// updateService changes a service through the real merchant path (version CAS) and returns its new version.
func (e *tcvEnv) updateService(code string, change func(*fulfillment.ServiceInput)) int64 {
	e.t.Helper()
	in := e.svcs[code]
	change(&in)
	in.ExpectedVersion = e.svcVer[code]
	out, err := dsSet(e.p.cqHarness, t04Key("tcv-service-update"), in)
	if err != nil {
		e.t.Fatalf("update service %s: %v", code, err)
	}
	in.ExpectedVersion = 0
	e.svcs[code], e.svcVer[code] = in, out.Version
	return out.Version
}

// sku creates a SKU with the given price (TWD minor) and stock and returns its id.
func (e *tcvEnv) sku(priceMinor, stock int64) string {
	e.t.Helper()
	p := e.p
	ctx := context.Background()
	tag := t04Tag()
	sku, err := t04Scoped(ctx, p.f, p.f.tokens["a"], p.f.storeA1, "catalog:write", func(tx pgx.Tx, s platform.Scope) (catalog.SKU, error) {
		return catalog.CreateSKU(ctx, tx, s, t04Key("tcv-sku"), catalog.SKUInput{ProductID: p.stock.product.ID, Code: "TCV-" + tag, PriceMinor: priceMinor, WeightGrams: 100, LengthMM: 10, WidthMM: 20, HeightMM: 30,
			OriginCountry: "TW", CustomsName: "test item", HSCandidate: "851840"})
	})
	if err != nil {
		e.t.Fatalf("create sku: %v", err)
	}
	if _, err = t04Scoped(ctx, p.f, p.f.tokens["a"], p.f.storeA1, "inventory:write", func(tx pgx.Tx, s platform.Scope) (inventory.Balance, error) {
		return inventory.AdjustOnHand(ctx, tx, s, t04Key("tcv-stock"), inventory.Adjustment{WarehouseID: p.stock.warehouse.ID, SKUID: sku.ID, Delta: stock, ExpectedVersion: 0, Reason: "tcv fixture stock"})
	}); err != nil {
		e.t.Fatalf("stock: %v", err)
	}
	return sku.ID
}

// topUp adds stock to the harness SKU (each order holds 2 units).
func (e *tcvEnv) topUp() {
	e.t.Helper()
	p := e.p
	var version int64
	if err := p.f.owner.QueryRow(context.Background(), `SELECT version FROM inventory.balances WHERE tenant_id=$1 AND store_id=$2 AND sku_id=$3`, p.f.tenantA, p.f.storeA1, p.stock.skus[0].ID).Scan(&version); err != nil {
		e.t.Fatal(err)
	}
	if _, err := t04Scoped(context.Background(), p.f, p.f.tokens["a"], p.f.storeA1, "inventory:write", func(tx pgx.Tx, s platform.Scope) (inventory.Balance, error) {
		return inventory.AdjustOnHand(context.Background(), tx, s, t04Key("tcv-topup"), inventory.Adjustment{WarehouseID: p.stock.warehouse.ID, SKUID: p.stock.skus[0].ID, Delta: 200, ExpectedVersion: version, Reason: "tcv top up"})
	}); err != nil {
		e.t.Fatal(err)
	}
}

// buyerCtx is one buyer (fresh capability => fresh owner and cart).
type tcvBuyer struct {
	e   *tcvEnv
	h   bcHarness
	cap buyer.Capability
}

// newBuyer issues a new buyer capability and fills its cart with items (sku id -> quantity; nil = the harness SKU x2).
func (e *tcvEnv) newBuyer(items ...storefront.Item) *tcvBuyer {
	e.t.Helper()
	e.topUp()
	if len(items) == 0 {
		items = []storefront.Item{{SKUID: e.p.stock.skus[0].ID, Quantity: 2}}
	}
	h := e.p.bcHarness
	h.prepare(e.t, mustIssue(e.t, e.p.cqHarness.service, e.p.f.storeA1), items)
	return &tcvBuyer{e: e, h: h, cap: h.cap}
}

func (b *tcvBuyer) cartVersion() int64 { return b.h.input.CartVersion }

// req is one buyer HTTP request through the real buyerhttp handler.
func (b *tcvBuyer) req(method, path, key string, input any, edit func(*http.Request)) bhResponse {
	return b.e.bh.request(b.e.t, method, path, b.cap.Token, key, input, edit)
}

// destination selects a pickup as the buyer's destination (CVS kinds need recipient name and phone that pass C3 for API/pay-at-pickup).
func (b *tcvBuyer) destination(kind, pickupID, name, phone string) (storefront.Destination, error) {
	dest, err := bdSet(b.h.cqHarness, t04Key("tcv-dest"), storefront.DestinationInput{ExpectedVersion: b.h.destination.Version, CartVersion: b.cartVersion(), Kind: kind, Country: "TW",
		RecipientName: name, Phone: phone, PickupID: pickupID})
	if err == nil {
		b.h.destination = dest
	}
	return dest, err
}

// quote quotes the cart with the delivery service code.
func (b *tcvBuyer) quote(code string) (storefront.Quote, error) {
	return cqBuyer(b.h.a.runtime, b.cap, func(ctx context.Context, tx pgx.Tx, s buyer.Scope) (storefront.Quote, error) {
		return storefront.CreateQuote(ctx, tx, s, t04Key("tcv-quote"), storefront.QuoteInput{CartVersion: b.cartVersion(), MarketID: b.e.p.market.ID, Country: "TW", Method: "delivery:" + code})
	})
}

// begin runs checkout.Begin through the environment's checkout service (payment environment + CVS surface).
func (b *tcvBuyer) begin(dest storefront.Destination, quote storefront.Quote, serviceVersion int64, paymentMode string) (checkout.Result, error) {
	in := checkout.Input{QuoteID: quote.ID, DestinationID: dest.ID, CartVersion: b.cartVersion(), ServiceVersion: serviceVersion, AllocationVersion: 1, PaymentMode: paymentMode}
	return b.e.svc.Begin(context.Background(), b.cap.Token, b.e.store(), t04Key("tcv-begin"), in)
}

// enteredStore posts a buyer-entered store (§16.1) and returns the HTTP response.
func (b *tcvBuyer) enteredStore(serviceCode, storeCode, name, address string) bhResponse {
	return b.enteredStoreKey(t04Key("tcv-store"), serviceCode, storeCode, name, address)
}

func (b *tcvBuyer) enteredStoreKey(key, serviceCode, storeCode, name, address string) bhResponse {
	return b.req("POST", "/v1/buyer/cvs-stores", key, map[string]any{"cart_version": b.cartVersion(), "market_id": b.e.p.market.ID, "service_code": serviceCode,
		"store_code": storeCode, "store_name": name, "store_address": address}, nil)
}

// openSelection opens an ECPay map selection (§5.2) for the product page path.
func (b *tcvBuyer) openSelection(code, returnPath string) bhResponse {
	return b.req("POST", "/v1/buyer/cvs-selections", t04Key("tcv-sel"), map[string]any{"cart_version": b.cartVersion(), "market_id": b.e.p.market.ID, "service_code": code, "return_path": returnPath}, nil)
}

// hook posts a form to the provider-hooks handler (no cookies, no BFF key: the unauthenticated public routes).
func (e *tcvEnv) hook(path string, form url.Values, edit func(*http.Request)) *httptest.ResponseRecorder {
	req := httptest.NewRequest("POST", path, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if edit != nil {
		edit(req)
	}
	w := httptest.NewRecorder()
	e.hooks.ServeHTTP(w, req)
	return w
}

// mapReturn plays the browser: the fake's stage map returns its fixed store for the selection's form, posted to the hooks handler.
func (e *tcvEnv) mapReturn(selectionID string, form map[string]string) *httptest.ResponseRecorder {
	v := url.Values{}
	for k, val := range form {
		v.Set(k, val)
	}
	return e.hook("/v1/cvs/ecpay/map-return/"+selectionID, e.fake.MapReturn(v), nil)
}

func tcvJSON(t *testing.T, raw []byte) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("not JSON: %v: %s", err, raw)
	}
	return out
}

func tcvStr(m map[string]any, keys ...string) string {
	var cur any = m
	for _, k := range keys {
		mm, ok := cur.(map[string]any)
		if !ok {
			return ""
		}
		cur = mm[k]
	}
	s, _ := cur.(string)
	return s
}

func tcvFormFields(m map[string]any) map[string]string {
	out := map[string]string{}
	if fields, ok := m["form"].(map[string]any)["fields"].(map[string]any); ok {
		for k, v := range fields {
			out[k], _ = v.(string)
		}
	}
	return out
}

// count runs a count query on the owner pool.
func (e *tcvEnv) count(q string, args ...any) int { return countRows(e.t, e.p.f.owner, q, args...) }

// raise runs stmt on a fresh transaction of the owner pool and returns its SQLSTATE (used for disclosed fault planting checks).
func tcvExecState(ctx context.Context, tx pgx.Tx, stmt string, args ...any) string {
	st, _ := tcsSub(ctx, tx, stmt, args...)
	return st
}

// tcvRefusal extracts the coded refusal (HTTP status, contract code) of a checkout/CVS error.
func tcvRefusal(err error) (int, string) {
	var r *fulfillment.CVSError
	if errors.As(err, &r) {
		return r.Status, r.Code
	}
	if err == nil {
		return 0, ""
	}
	return -1, err.Error()
}

// tcvExpectRefusal fails the test unless err is the coded refusal (status, code).
func tcvExpectRefusal(t *testing.T, label string, err error, status int, code string) {
	t.Helper()
	if s, c := tcvRefusal(err); s != status || c != code {
		t.Errorf("%s: want %d %s, got %d %q (%v)", label, status, code, s, c, err)
	}
}

// selPickup returns the pickup projection of a selection view (nil unless VERIFIED).
func selPickup(m map[string]any) map[string]any {
	p, _ := m["pickup"].(map[string]any)
	return p
}

func selPickupID(m map[string]any) string {
	p := selPickup(m)
	if p == nil {
		return ""
	}
	if id := tcvStr(p, "pickup_id"); id != "" {
		return id
	}
	return tcvStr(p, "id")
}

// openSel opens a selection through the buyer route and returns its id and the map form fields.
func (e *tcvEnv) openSel(b *tcvBuyer, code string) (string, map[string]string) {
	e.t.Helper()
	res := b.openSelection(code, "/en/products/prod_"+t04Tag())
	if res.status != 201 {
		e.t.Fatalf("open selection: %d %s", res.status, res.body)
	}
	out := tcvJSON(e.t, res.body)
	return tcvStr(out, "selection_id"), tcvFormFields(out)
}

// verifySel calls POST verify (keyless, bodiless) and returns the projection.
func (e *tcvEnv) verifySel(b *tcvBuyer, id string) (int, map[string]any) {
	e.t.Helper()
	res := b.req("POST", "/v1/buyer/cvs-selections/"+id+"/verify", "", nil, nil)
	return res.status, tcvJSON(e.t, res.body)
}

func (e *tcvEnv) readSel(b *tcvBuyer, id string) (int, map[string]any) {
	e.t.Helper()
	res := b.req("GET", "/v1/buyer/cvs-selections/"+id, "", nil, nil)
	return res.status, tcvJSON(e.t, res.body)
}

// verifiedPickup runs the whole map round trip for the buyer against the fake's fixed stage store and returns the VERIFIED selection
// id and pickup id.
func (e *tcvEnv) verifiedPickup(b *tcvBuyer, code string) (selection, pickup string) {
	e.t.Helper()
	id, fields := e.openSel(b, code)
	if w := e.mapReturn(id, fields); w.Code != http.StatusSeeOther {
		e.t.Fatalf("map return: %d %s", w.Code, w.Body.String())
	}
	st, view := e.verifySel(b, id)
	if st != 200 || tcvStr(view, "state") != "VERIFIED" || selPickupID(view) == "" {
		e.t.Fatalf("verify: %d %v", st, view)
	}
	return id, selPickupID(view)
}

// tcvRefusedWithStatus reports whether err is a refusal with one of the HTTP statuses: a coded CVS refusal, or the shared
// conflict/invalid sentinels the buyer layer maps to 409/422 when the SQL message is outside the code grammar.
func tcvRefusedWithStatus(err error, statuses ...int) bool {
	if err == nil {
		return false
	}
	got := -1
	if s, _ := tcvRefusal(err); s > 0 {
		got = s
	} else if errors.Is(err, command.ErrConflict) {
		got = 409
	} else if errors.Is(err, command.ErrInvalid) {
		got = 422
	}
	for _, s := range statuses {
		if got == s {
			return true
		}
	}
	return false
}

// mapReturnWith is mapReturn with field overrides (a forged or mismatching return: the map return has no MAC, F3).
func (e *tcvEnv) mapReturnWith(selectionID string, form map[string]string, override map[string]string) *httptest.ResponseRecorder {
	v := url.Values{}
	for k, val := range form {
		v.Set(k, val)
	}
	ret := e.fake.MapReturn(v)
	for k, val := range override {
		ret.Set(k, val)
	}
	return e.hook("/v1/cvs/ecpay/map-return/"+selectionID, ret, nil)
}

// reclient replaces the ECPay client (and everything holding it) with a fresh one: an empty in-process directory cache, as after a
// process restart or the next 20:00 refresh.
func (e *tcvEnv) reclient() {
	e.t.Helper()
	var err error
	if e.client, err = ecpay.NewClient(ecpay.Environment(e.cfg.PaymentEnvironment), e.fake.Transport()); err != nil {
		e.t.Fatal(err)
	}
	if e.cvs, err = fulfillment.NewCVS(e.p.f.runtime, e.jobs, e.keys, e.client, e.cfg); err != nil {
		e.t.Fatal(err)
	}
	e.merchant = e.newMerchantHandler()
	e.hooks = e.cvs.HooksHandler()
	bc, err := checkout.NewBuyerCVS(e.p.pool, e.keys, e.client, e.cfg)
	if err != nil {
		e.t.Fatal(err)
	}
	e.svc = e.p.bcHarness.service.WithPaymentEnvironment(e.cfg.PaymentEnvironment).WithBuyerCVS(bc)
	e.bh = e.tcbServe(e.svc)
}

// hookRaw posts an arbitrary body to the hooks handler.
func (e *tcvEnv) hookRaw(path, body, contentType string) *httptest.ResponseRecorder {
	req := httptest.NewRequest("POST", path, strings.NewReader(body))
	req.Header.Set("Content-Type", contentType)
	w := httptest.NewRecorder()
	e.hooks.ServeHTTP(w, req)
	return w
}

func urlValues(m map[string]string) url.Values {
	v := url.Values{}
	for k, val := range m {
		v.Set(k, val)
	}
	return v
}

// fulfillmentCfg is the environment's CVS config with another payment environment.
func fulfillmentCfg(e *tcvEnv, payEnv string) fulfillment.CVSConfig {
	c := e.cfg
	c.PaymentEnvironment = payEnv
	return c
}

// newMerchantHandler is the merchant handler with the CVS routes and the refund routes (an insert-only river_payment client, as cmd/api).
func (e *tcvEnv) newMerchantHandler() http.Handler {
	e.t.Helper()
	refunds, err := river.NewClient(riverpgxv5.New(e.p.f.runtime), &river.Config{Schema: "river_payment"})
	if err != nil {
		e.t.Fatal(err)
	}
	return httpapi.NewHandler(e.p.f.runtime, httpapi.Options{CVS: e.cvs, RefundJobs: refunds})
}

// ---- the real ecpay.cvs_create route, in process --------------------------------------------------------------------------

// startDispatcher runs the real dispatcher with ecpayroute.Routes over the fake, on a private River queue (jobs are moved to it by route()),
// the same shape cmd/worker assembles. The worker pool is a real commerce_worker login.
func (e *tcvEnv) startDispatcher() { e.startDispatcherWith(nil) }

// startDispatcherWith is startDispatcher with dispatcher option overrides (small call timeouts and budgets make the UNKNOWN paths quick).
func (e *tcvEnv) startDispatcherWith(tune func(*integration.DispatcherOptions)) {
	e.t.Helper()
	ctx := context.Background()
	routes, err := ecpayroute.Routes(e.p.worker, e.keys, e.client, e.cfg.ECPay)
	if err != nil {
		e.t.Fatalf("ecpayroute.Routes: %v", err)
	}
	opts := integration.DefaultDispatcherOptions()
	opts.RetryDelay = 100 * time.Millisecond
	if tune != nil {
		tune(&opts)
	}
	disp, err := integration.NewDispatcher(ctx, e.p.worker, routes, opts)
	if err != nil {
		e.t.Fatalf("dispatcher: %v", err)
	}
	e.queue = "cvs_" + strings.ReplaceAll(randomUUID(), "-", "")
	if e.queueOver != "" {
		e.queue = e.queueOver
	}
	rescue := 30 * time.Second
	if e.rescueAfter > 0 {
		rescue = e.rescueAfter
	}
	workers := river.NewWorkers()
	river.AddWorker(workers, disp)
	client, err := river.NewClient(riverpgxv5.New(e.p.worker), &river.Config{Schema: "river", Workers: workers, Queues: map[string]river.QueueConfig{e.queue: {MaxWorkers: 2}},
		JobTimeout: 20 * time.Second, RescueStuckJobsAfter: rescue, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err != nil {
		e.t.Fatal(err)
	}
	if err := client.Start(ctx); err != nil {
		e.t.Fatal(err)
	}
	e.t.Cleanup(func() {
		c, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = client.StopAndCancel(c)
	})
}

// route moves the River job of an operation onto the private queue so the in-process dispatcher (and only it) picks it up.
func (e *tcvEnv) route(operationID string) {
	e.t.Helper()
	tag, err := e.p.f.owner.Exec(context.Background(), `UPDATE river.river_job SET queue=$1 WHERE id=(SELECT job_id FROM integration.operations WHERE id=$2::uuid) AND state='available'`, e.queue, operationID)
	if err != nil || tag.RowsAffected() != 1 {
		e.t.Fatalf("route the job of operation %s to the dispatcher queue: rows=%v err=%v", operationID, tag.RowsAffected(), err)
	}
}

func (e *tcvEnv) shipPath(order string) string {
	return "/v1/admin/stores/" + e.store() + "/orders/" + order + "/cvs-shipment"
}

// ship requests a label as token; on 202 with routeJob the job is handed to the dispatcher.
func (e *tcvEnv) ship(token, order string, expected int64, key string, routeJob bool) (int, map[string]any, []byte) {
	e.t.Helper()
	if key == "" {
		key = t04Key("tcv-ship")
	}
	st, out, raw := e.mcall(token, "POST", e.shipPath(order), key, fmt.Sprintf(`{"expected_version":%d}`, expected))
	if st == 202 && routeJob {
		e.route(tcvStr(out, "operation_id"))
	}
	return st, out, raw
}

// shipState is the latest attempt of an order: state, attempt, version ("" when none).
func (e *tcvEnv) shipState(order string) (string, int, int64) {
	var state string
	var attempt int
	var version int64
	err := e.p.f.owner.QueryRow(context.Background(), `SELECT state,attempt,version FROM fulfillment.cvs_shipments WHERE order_id=$1 ORDER BY attempt DESC LIMIT 1`, order).Scan(&state, &attempt, &version)
	if err == pgx.ErrNoRows {
		return "", 0, 0
	}
	if err != nil {
		e.t.Fatal(err)
	}
	return state, attempt, version
}

func (e *tcvEnv) awaitShip(order, want string) {
	e.t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for {
		state, _, _ := e.shipState(order)
		if state == want {
			return
		}
		if time.Now().After(deadline) {
			e.t.Fatalf("shipment of %s: state %q after 30s, want %s", order, state, want)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// tradeNo is the frozen MerchantTradeNo of the latest attempt.
func (e *tcvEnv) tradeNo(order string) string {
	var tn string
	if err := e.p.f.owner.QueryRow(context.Background(), `SELECT merchant_trade_no FROM fulfillment.cvs_shipments WHERE order_id=$1 ORDER BY attempt DESC LIMIT 1`, order).Scan(&tn); err != nil {
		e.t.Fatal(err)
	}
	return tn
}

// endpointID reads the status route id from the merchant profile projection.
func (e *tcvEnv) endpointID() string {
	e.t.Helper()
	st, out, raw := e.mcall(e.token(), "GET", "/v1/admin/stores/"+e.store()+"/logistics/ecpay", "", "")
	if st != 200 {
		e.t.Fatalf("profile: %d %s", st, raw)
	}
	url := tcvStr(out, "status_url")
	i := strings.LastIndex(url, "/")
	if i < 0 || !strings.Contains(url, "/v1/cvs/ecpay/status/") {
		e.t.Fatalf("status_url %q", url)
	}
	return url[i+1:]
}

// postStatus signs fields with the store's ECPay keys and POSTs them to the status hook.
func (e *tcvEnv) postStatus(endpoint string, fields url.Values) *httptest.ResponseRecorder {
	return e.hook("/v1/cvs/ecpay/status/"+endpoint, ecpaytest.SignStatus(e.mk, fields), nil)
}

// statusFor builds an F7 status notification for the recorded trade of an order.
func (e *tcvEnv) statusFor(order, rtnCode string) url.Values {
	e.t.Helper()
	tr, ok := e.fake.Trade(e.tradeNo(order))
	if !ok {
		e.t.Fatalf("the fake has no trade for %s (was a Create sent?)", order)
	}
	// every notification carries its own UpdateStatusDate (a re-delivered identical body is a duplicate, deduped by design)
	e.statusSeq++
	return ecpaytest.StatusFields(tr, rtnCode, time.Now().Add(time.Duration(e.statusSeq)*time.Second).Format("2006/01/02 15:04:05"), "SENTINELRECIPIENT")
}

// recart bumps the buyer's cart (same owner, new cart version), so the same owner can attempt another order.
func (b *tcvBuyer) recart() {
	b.e.t.Helper()
	b.e.topUp()
	c, err := b.h.cart(t04Key("tcv-recart"), storefront.CartInput{ExpectedVersion: b.cartVersion(), Items: []storefront.Item{{SKUID: b.e.p.stock.skus[0].ID, Quantity: 2}}})
	if err != nil {
		b.e.t.Fatalf("recart: %v", err)
	}
	b.h.input.CartVersion = c.Version
}

// tcvOrderSpec describes one CVS order placed through the real buyer path.
type tcvOrderSpec struct {
	kind, code  string // delivery kind and service code
	paymentMode string // "" (card, paid through the rfx capture path) or "pay_at_pickup"
	items       []storefront.Item
}

// cvsOrder places an order to an ECPay-verified pickup (selection flow against the fake). A card order is paid through the real capture path
// (needs tcvOpts.stripe and a running rfx worker). It returns the order id and the buyer.
func (e *tcvEnv) cvsOrder(s tcvOrderSpec) (string, *tcvBuyer) {
	e.t.Helper()
	b := e.newBuyer(s.items...)
	_, pickup := e.verifiedPickup(b, s.code)
	res, err := e.tcbTry(b, s.kind, s.code, pickup, "王小明", "0912345678", s.paymentMode)
	if err != nil {
		e.t.Fatalf("place %s order: %v", s.kind, err)
	}
	if s.paymentMode == "" {
		e.payHold(res, b)
	}
	return res.OrderID, b
}

// payHold pays a DRAFT hold through the real Stripe capture path of the rfx store (e.r.startWorker must be running).
func (e *tcvEnv) payHold(hold checkout.Result, b *tcvBuyer) rfxOrder {
	e.t.Helper()
	if e.r == nil {
		e.t.Fatal("payHold needs tcvOpts.stripe")
	}
	s := e.ro.s
	s.p.hold = hold
	s.p.cap = b.cap // the hosted payment start authenticates as the order's own buyer
	return e.r.pay(e.t, s, e.ro.endpoint, e.ro.secret)
}

// abandon abandons the latest attempt with the acknowledgement. While the dispatcher still owns the reconcile budget the route answers 409
// reconcile_in_progress (an UNKNOWN attempt is abandonable only after the budget is exhausted or the 1-hour settle rule); it polls for that.
func (e *tcvEnv) abandon(order string) (int, map[string]any, []byte) {
	e.t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for {
		_, _, version := e.shipState(order)
		st, out, raw := e.mcall(e.token(), "POST", e.shipPath(order)+"/abandon", t04Key("tcv-abandon"), fmt.Sprintf(`{"expected_version":%d,"i_checked_ecpay_backend":true}`, version))
		if st != 409 || (tcvStr(out, "code") != "reconcile_in_progress" && tcvStr(out, "code") != "version_changed") || time.Now().After(deadline) {
			return st, out, raw
		}
		time.Sleep(200 * time.Millisecond)
	}
}

func (e *tcvEnv) awaitShipWithin(order, want string, d time.Duration) {
	e.t.Helper()
	deadline := time.Now().Add(d)
	for {
		state, _, _ := e.shipState(order)
		if state == want {
			return
		}
		if time.Now().After(deadline) {
			e.t.Fatalf("shipment of %s: state %q after %v, want %s", order, state, d, want)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// newFakeServer serves the fake over real HTTP (out-of-process clients).
func newFakeServer(f *ecpaytest.Fake) *httptest.Server { return httptest.NewServer(f.Handler()) }

// newHTTPServer serves a handler on a loopback listener for out-of-process clients (the browser gates).
func newHTTPServer(t *testing.T, h http.Handler) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return srv
}

// routeNewJobs keeps moving the River jobs of this store's new ECPay create operations onto the dispatcher queue (for gates where a browser, not the
// test, requests the label). It stops with the test.
func (e *tcvEnv) routeNewJobs() {
	ctx, cancel := context.WithCancel(context.Background())
	e.t.Cleanup(cancel)
	go func() {
		for ctx.Err() == nil {
			_, _ = e.p.f.owner.Exec(ctx, `UPDATE river.river_job SET queue=$1 WHERE state='available' AND queue<>$1 AND kind='external_operation_v1'
			  AND id IN (SELECT job_id FROM integration.operations WHERE tenant_id=$2 AND provider='ecpay_logistics' AND state='READY')`, e.queue, e.tenant())
			time.Sleep(100 * time.Millisecond)
		}
	}()
}
