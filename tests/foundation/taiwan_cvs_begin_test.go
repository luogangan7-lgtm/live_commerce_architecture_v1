package foundation_test

// TCV04 TestCvsBeginGuards and TCV11 TestCvsOptions (contracts/taiwan-cvs-logistics-v1.md §10 TCV04 / TCV11, §4.3 checkout.begin_hold (a)-(d),
// §5.1, §12 kill switch, §16.1 mode, §16.5 store settings). Prefix `tcb`. Tier REAL_PG + HTTP_PG.
// Routes/definers: checkout.Service.Begin -> checkout.begin_hold (10 args), GET /v1/buyer/checkout-options -> fulfillment.read_cvs_offer,
// the merchant settings / ECPay routes to set the store up.
// Owner-pool writes (disclosed fixtures): profile ok_verified/hilife_verified flags (contract: "set only by TCV10/TCV07 evidence via the registrar"),
// qualified_credential_version NULL (a rotated-unqualified credential), pickup validity aged by 25 h. Each is restored or stays inside its store.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"testing"
	"time"

	"livecommerce/internal/buyerhttp"
	"livecommerce/internal/checkout"
	"livecommerce/internal/fulfillment"
	"livecommerce/internal/storefront"
)

// tcbHolds counts the stock holds and payment attempts an owner has: a refused Begin must leave both untouched.
func (e *tcvEnv) tcbEffects(owner string) (holds, attempts, orders int) {
	return e.count(`SELECT count(*) FROM inventory.reservations WHERE buyer_owner_id=$1`, owner),
		e.count(`SELECT count(*) FROM checkout.payment_attempts WHERE owner_id=$1`, owner),
		e.count(`SELECT count(*) FROM checkout.orders WHERE owner_id=$1`, owner)
}

// tcbTry runs the quote/destination/Begin chain for a pickup and returns the Begin error.
func (e *tcvEnv) tcbTry(b *tcvBuyer, kind, code, pickup, name, phone, paymentMode string) (checkout.Result, error) {
	dest, err := b.destination(kind, pickup, name, phone)
	if err != nil {
		return checkout.Result{}, fmt.Errorf("destination: %w", err)
	}
	quote, err := b.quote(code)
	if err != nil {
		return checkout.Result{}, fmt.Errorf("quote: %w", err)
	}
	return b.begin(dest, quote, e.svcVer[code], paymentMode)
}

func TestCvsBeginGuards(t *testing.T) {
	e := tcvNew(t)
	f := e.p.f
	e.grantCreator("orders:read", "fulfillment:write")
	e.connect("C2C")
	api, _, _ := e.service("cvs_711", "API", 0)
	const name, phone = "王小明", "0912345678"

	t.Run("API service + verified pickup succeeds", func(t *testing.T) {
		b := e.newBuyer()
		_, pickup := e.verifiedPickup(b, api)
		res, err := e.tcbTry(b, "cvs_711", api, pickup, name, phone, "")
		if err != nil || res.OrderID == "" {
			t.Fatalf("Begin with an API service and a provider-verified pickup: %+v %v", res, err)
		}
		if n := e.count(`SELECT count(*) FROM checkout.orders WHERE id=$1 AND payment_mode='card' AND collection_state IS NULL`, res.OrderID); n != 1 {
			t.Error("a default Begin is a card order (payment_mode card, no collection_state)")
		}
	})

	t.Run("amount boundaries in TWD minor units (0061 x100 convention)", func(t *testing.T) {
		big := e.sku(200000, 30) // NT$2000 per unit
		s100 := e.sku(100, 5)    // NT$1
		s50 := e.sku(50, 5)      // NT$0.50: not a whole TWD
		cases := []struct {
			label string
			items []storefront.Item
			want  string // "" accepted, else refusal code
		}{
			{"2000000 minor = NT$20000 (GoodsAmount ceiling) accepted", []storefront.Item{{SKUID: big, Quantity: 10}}, ""},
			{"100 minor = NT$1 (floor) accepted", []storefront.Item{{SKUID: s100, Quantity: 1}}, ""},
			{"2000100 minor (> NT$20000) refused", []storefront.Item{{SKUID: big, Quantity: 10}, {SKUID: s100, Quantity: 1}}, "cvs_amount_exceeds"},
			{"50 minor (non-whole TWD) refused", []storefront.Item{{SKUID: s50, Quantity: 1}}, "cvs_amount_exceeds"},
			{"150 minor (non-whole TWD) refused", []storefront.Item{{SKUID: s100, Quantity: 1}, {SKUID: s50, Quantity: 1}}, "cvs_amount_exceeds"},
		}
		for _, c := range cases {
			b := e.newBuyer(c.items...)
			_, pickup := e.verifiedPickup(b, api)
			h0, a0, o0 := e.tcbEffects(b.cap.Scope.OwnerID)
			_, err := e.tcbTry(b, "cvs_711", api, pickup, name, phone, "")
			if c.want == "" {
				if err != nil {
					t.Errorf("%s: %v", c.label, err)
				}
				continue
			}
			tcvExpectRefusal(t, c.label, err, 422, c.want)
			if h, a, o := e.tcbEffects(b.cap.Scope.OwnerID); h != h0 || a != a0 || o != o0 {
				t.Errorf("%s: a refused Begin left holds/attempts/orders %d/%d/%d -> %d/%d/%d", c.label, h0, a0, o0, h, a, o)
			}
		}
	})

	t.Run("recipient rule (F5) refuses before any hold", func(t *testing.T) {
		for label, r := range map[string][2]string{
			"digit in name":  {"王小明1", phone},
			"one-char name":  {"王", phone},
			"landline phone": {name, "0212345678"},
			"short phone":    {name, "091234567"},
		} {
			b := e.newBuyer()
			_, pickup := e.verifiedPickup(b, api)
			h0, a0, o0 := e.tcbEffects(b.cap.Scope.OwnerID)
			_, err := e.tcbTry(b, "cvs_711", api, pickup, r[0], r[1], "")
			if err != nil && (func() bool { s, c := tcvRefusal(err); return s == 422 && c == "cvs_recipient_rejected" })() {
				// refused by Begin: fine
			} else if err != nil {
				t.Logf("%s refused earlier in the chain (destination/quote): %v", label, err)
			} else {
				t.Errorf("%s: Begin accepted a recipient the ECPay rule refuses", label)
			}
			if h, a, o := e.tcbEffects(b.cap.Scope.OwnerID); h != h0 || a != a0 || o != o0 {
				t.Errorf("%s: holds/attempts/orders changed %d/%d/%d -> %d/%d/%d", label, h0, a0, o0, h, a, o)
			}
		}
		// the accepted forms (normalised +886, spaces) still pass
		b := e.newBuyer()
		_, pickup := e.verifiedPickup(b, api)
		if _, err := e.tcbTry(b, "cvs_711", api, pickup, "王小明", "+886 912 345 678", ""); err != nil {
			t.Errorf("a +886 mobile must be accepted after normalisation: %v", err)
		}
	})

	t.Run("environment: LIVE deployment with a SANDBOX ECPay profile refuses, zero holds and payment attempts", func(t *testing.T) {
		b := e.newBuyer()
		_, pickup := e.verifiedPickup(b, api)
		bc, err := checkout.NewBuyerCVS(e.p.pool, e.keys, e.client, fulfillment.CVSConfig{ECPay: e.cfg.ECPay, PaymentEnvironment: "LIVE"})
		if err != nil {
			t.Fatal(err)
		}
		live := e.p.bcHarness.service.WithPaymentEnvironment("LIVE").WithBuyerCVS(bc)
		dest, err := b.destination("cvs_711", pickup, name, phone)
		if err != nil {
			t.Fatal(err)
		}
		quote, err := b.quote(api)
		if err != nil {
			t.Fatal(err)
		}
		h0, a0, o0 := e.tcbEffects(b.cap.Scope.OwnerID)
		_, err = live.Begin(context.Background(), b.cap.Token, e.store(), t04Key("tcb-live"), checkout.Input{QuoteID: quote.ID, DestinationID: dest.ID, CartVersion: b.cartVersion(), ServiceVersion: 1, AllocationVersion: 1})
		tcvExpectRefusal(t, "LIVE deployment, SANDBOX profile and pickup", err, 422, "cvs_environment_mismatch")
		if h, a, o := e.tcbEffects(b.cap.Scope.OwnerID); h != h0 || a != a0 || o != o0 {
			t.Errorf("a refused Begin left holds/attempts/orders %d/%d/%d -> %d/%d/%d", h0, a0, o0, h, a, o)
		}
		// the same buyer on the matching deployment still succeeds (the refusal was the environment, nothing else)
		if _, err := b.begin(dest, quote, 1, ""); err != nil {
			t.Errorf("same request on the SANDBOX deployment: %v", err)
		}
	})

	t.Run("disabled profile, unqualified credential, stale pickup: refused with zero holds", func(t *testing.T) {
		refuse := func(label string, setup func(), restore func()) {
			b := e.newBuyer()
			_, pickup := e.verifiedPickup(b, api)
			setup()
			defer restore()
			h0, a0, o0 := e.tcbEffects(b.cap.Scope.OwnerID)
			_, err := e.tcbTry(b, "cvs_711", api, pickup, name, phone, "")
			if err == nil {
				t.Errorf("%s: Begin must be refused", label)
				return
			}
			if !tcvRefusedWithStatus(err, 409, 422) {
				t.Errorf("%s: want a 409/422 refusal (PT409/PT422 per section 10), got %v", label, err)
			}
			if h, a, o := e.tcbEffects(b.cap.Scope.OwnerID); h != h0 || a != a0 || o != o0 {
				t.Errorf("%s: holds/attempts/orders %d/%d/%d -> %d/%d/%d", label, h0, a0, o0, h, a, o)
			}
		}
		profileVersion := func() int64 {
			_, out, raw := e.mcall(e.token(), "GET", "/v1/admin/stores/"+e.store()+"/logistics/ecpay", "", "")
			if v, ok := out["version"].(float64); ok {
				return int64(v)
			}
			t.Fatalf("profile: %s", raw)
			return 0
		}
		setEnabled := func(on bool) {
			st, _, raw := e.mcall(e.token(), "POST", "/v1/admin/stores/"+e.store()+"/logistics/ecpay/enabled", t04Key("tcb-enable"), fmt.Sprintf(`{"expected_version":%d,"enabled":%v}`, profileVersion(), on))
			if st != 200 {
				t.Fatalf("set enabled=%v: %d %s", on, st, raw)
			}
		}
		refuse("profile disabled", func() { setEnabled(false) }, func() { setEnabled(true) })
		var qv int64
		var qat time.Time
		refuse("rotated-unqualified credential", func() {
			if err := f.owner.QueryRow(context.Background(), `SELECT qualified_credential_version,qualified_at FROM integration.ecpay_logistics_profiles WHERE tenant_id=$1 AND store_id=$2 AND enabled`, f.tenantA, f.storeA1).Scan(&qv, &qat); err != nil {
				t.Fatal(err)
			}
			// disclosed owner-pool plant: a credential version the probe never qualified
			mustExec(t, f.owner, `UPDATE integration.ecpay_logistics_profiles SET qualified_credential_version=NULL,qualified_at=NULL WHERE tenant_id=$1 AND store_id=$2 AND enabled`, f.tenantA, f.storeA1)
		}, func() {
			mustExec(t, f.owner, `UPDATE integration.ecpay_logistics_profiles SET qualified_credential_version=$3,qualified_at=$4 WHERE tenant_id=$1 AND store_id=$2 AND enabled`, f.tenantA, f.storeA1, qv, qat)
		})
		// stale pickup: the directory validity is 24 h (§4.2); a pickup verified 25 hours ago is refused
		b := e.newBuyer()
		_, pickup := e.verifiedPickup(b, api)
		mustExec(t, f.owner, `UPDATE fulfillment.pickup_versions SET attested_at=attested_at-interval '25 hours',valid_until=valid_until-interval '25 hours' WHERE id=$1`, pickup)
		h0, a0, o0 := e.tcbEffects(b.cap.Scope.OwnerID)
		if _, err := e.tcbTry(b, "cvs_711", api, pickup, name, phone, ""); err == nil {
			t.Error("a 25-hour-old directory pickup must be refused")
		} else if !tcvRefusedWithStatus(err, 409, 422) {
			t.Errorf("stale pickup: want a 409/422 refusal, got %v", err)
		}
		if h, a, o := e.tcbEffects(b.cap.Scope.OwnerID); h != h0 || a != a0 || o != o0 {
			t.Errorf("stale pickup left rows: %d/%d/%d -> %d/%d/%d", h0, a0, o0, h, a, o)
		}
	})

	t.Run("regressions: MANUAL home and MANUAL_ATTESTED CVS still Begin", func(t *testing.T) {
		// home service from the harness (delivery `home`): the p.hold created by psSetup proves it; a second buyer repeats it
		e.topUp()
		hb := e.p.bcHarness
		hb.prepare(t, mustIssue(t, e.p.cqHarness.service, e.p.f.storeA1), []storefront.Item{{SKUID: e.p.stock.skus[0].ID, Quantity: 2}})
		if res, err := e.svc.Begin(context.Background(), hb.cap.Token, e.store(), t04Key("tcb-home"), hb.input); err != nil || res.OrderID == "" {
			t.Errorf("MANUAL home Begin: %+v %v", res, err)
		}
		// MANUAL_ATTESTED cvs_familymart (0012 attestation) + a MANUAL service: unchanged by the ECPay profile
		man, _, _ := e.service("cvs_familymart", "MANUAL", 0)
		b := e.newBuyer()
		pickup, err := bdAttest(b.h.cqHarness, t04Key("tcb-attest"), fulfillment.PickupInput{Kind: "cvs_familymart", Namespace: "fixture." + t04Tag(), Code: "017888", Name: "Synthetic pickup", Address: "Synthetic pickup address", EvidenceRef: "fixture-only", TTLSeconds: 3600})
		if err != nil {
			t.Fatal(err)
		}
		if res, err := e.tcbTry(b, "cvs_familymart", man, pickup.ID, "Synthetic Buyer", "+886900000001", ""); err != nil || res.OrderID == "" {
			t.Errorf("MANUAL_ATTESTED CVS Begin with a MANUAL service: %+v %v (contract: existing MANUAL regressions unchanged)", res, err)
		}
	})
}

// tcbBuyerHandler serves the buyer routes over a checkout service (same wiring as tcvNew) and returns a request function.
func (e *tcvEnv) tcbServe(svc *checkout.Service) bhHarness {
	e.t.Helper()
	handler, err := buyerhttp.New(context.Background(), e.p.a.issuer, e.p.a.runtime, svc, e.bh.key, time.Hour)
	if err != nil {
		e.t.Fatal(err)
	}
	srv := httptest.NewServer(handler)
	e.t.Cleanup(srv.Close)
	h := e.bh
	h.server = srv
	return h
}

func tcbRows(t *testing.T, raw []byte) map[string]map[string]any {
	t.Helper()
	var page struct {
		Items []map[string]any `json:"items"`
	}
	if err := json.Unmarshal(raw, &page); err != nil {
		t.Fatalf("options JSON: %v %s", err, raw)
	}
	out := map[string]map[string]any{}
	for _, it := range page.Items {
		out[tcvStr(it, "delivery_kind")] = it
	}
	return out
}

func tcbOptions(t *testing.T, h bhHarness, token, market string) map[string]map[string]any {
	t.Helper()
	r := h.request(t, "GET", "/v1/buyer/checkout-options?market_id="+market+"&country=TW", token, "", nil, nil)
	if r.status != 200 {
		t.Fatalf("checkout-options: %d %s", r.status, r.body)
	}
	return tcbRows(t, r.body)
}

func tcbModes(row map[string]any) []string {
	var out []string
	if list, ok := row["payment_modes"].([]any); ok {
		for _, v := range list {
			s, _ := v.(string)
			out = append(out, s)
		}
	}
	return out
}

func tcbAvailable(row map[string]any) bool {
	if row == nil {
		return false
	}
	av, present := row["available"]
	return !present || av == true
}

func TestCvsOptions(t *testing.T) {
	e := tcvNew(t)
	f := e.p.f
	e.grantCreator("orders:read", "fulfillment:write")
	kinds := []string{"cvs_711", "cvs_familymart", "cvs_hilife", "cvs_okmart"}
	svc := map[string]string{}
	for _, k := range kinds {
		svc[k], _, _ = e.service(k, "MANUAL", 0)
	}
	b := e.newBuyer()
	market := e.p.market.ID
	opts := func() map[string]map[string]any { return tcbOptions(t, e.bh, b.cap.Token, market) }
	settings := func(version int64, chains string, payAtPickup bool, capTWD string) {
		t.Helper()
		body := fmt.Sprintf(`{"expected_version":%d,"enabled_chains":%s,"pay_at_pickup_enabled":%v,"pay_at_pickup_max_twd":%s,"pay_at_pickup_max_open":20}`, version, chains, payAtPickup, capTWD)
		st, _, raw := e.mcall(e.token(), "PUT", "/v1/admin/stores/"+e.store()+"/logistics/cvs-settings", t04Key("tcb-settings"), body)
		if st != 200 {
			t.Fatalf("cvs-settings: %d %s", st, raw)
		}
	}
	all := `["cvs_711","cvs_familymart","cvs_hilife","cvs_okmart"]`

	t.Run("no profile: buyer_entered rows, payment_modes card only", func(t *testing.T) {
		rows := opts()
		for _, k := range kinds {
			r := rows[k]
			if r == nil || tcvStr(r, "pickup_selection") != "buyer_entered" || tcvStr(r, "store_search_url") == "" || !tcbAvailable(r) {
				t.Errorf("%s: %v", k, r)
			}
			if m := tcbModes(r); len(m) != 1 || m[0] != "card" {
				t.Errorf("%s payment_modes %v, want [card] (pay-at-pickup off by default)", k, m)
			}
		}
	})

	t.Run("payment_modes includes pay_at_pickup only when enabled; only enabled_chains are listed", func(t *testing.T) {
		settings(0, all, true, "20000")
		for _, k := range kinds {
			if m := tcbModes(opts()[k]); len(m) != 2 || m[0] != "card" || m[1] != "pay_at_pickup" {
				t.Errorf("%s payment_modes %v, want [card pay_at_pickup]", k, m)
			}
		}
		settings(1, `["cvs_711","cvs_okmart"]`, true, "20000")
		rows := opts()
		if rows["cvs_711"] == nil || rows["cvs_okmart"] == nil || rows["cvs_familymart"] != nil || rows["cvs_hilife"] != nil {
			t.Errorf("only chains in enabled_chains are listed, got kinds %v", func() (ks []string) {
				for k := range rows {
					ks = append(ks, k)
				}
				return
			}())
		}
		settings(2, all, false, "null")
		for _, k := range kinds {
			if m := tcbModes(opts()[k]); len(m) != 1 || m[0] != "card" {
				t.Errorf("%s after pay-at-pickup off: %v", k, m)
			}
		}
	})

	t.Run("hidden / disabled service variants are not listed", func(t *testing.T) {
		v := e.updateService(svc["cvs_familymart"], func(in *fulfillment.ServiceInput) { in.Visible = false })
		if tcbOptions(t, e.bh, b.cap.Token, market)["cvs_familymart"] != nil {
			t.Error("a hidden (visible=false) service must not be listed")
		}
		e.updateService(svc["cvs_familymart"], func(in *fulfillment.ServiceInput) { in.Visible, in.Enabled = true, false })
		if tcbOptions(t, e.bh, b.cap.Token, market)["cvs_familymart"] != nil {
			t.Error("a disabled service must not be listed")
		}
		e.updateService(svc["cvs_familymart"], func(in *fulfillment.ServiceInput) { in.Enabled = true })
		if r := tcbOptions(t, e.bh, b.cap.Token, market)["cvs_familymart"]; r == nil || !tcbAvailable(r) {
			t.Errorf("re-enabled service must be listed again (version %d): %v", v, r)
		}
	})

	t.Run("enabled qualified profile: ecpay_map rows; OK and Hi-Life coming_soon until verified", func(t *testing.T) {
		e.connect("C2C")
		rows := opts()
		for _, k := range []string{"cvs_711", "cvs_familymart"} {
			if r := rows[k]; r == nil || tcvStr(r, "pickup_selection") != "ecpay_map" || !tcbAvailable(r) {
				t.Errorf("%s with an enabled qualified profile: %v", k, r)
			}
		}
		for _, k := range []string{"cvs_okmart", "cvs_hilife"} {
			r := rows[k]
			if r == nil || r["available"] != false || tcvStr(r, "reason") != "coming_soon" {
				t.Errorf("%s must be {available:false, reason:coming_soon} until verified: %v", k, r)
			}
			if tcvStr(r, "pickup_selection") == "buyer_entered" {
				t.Errorf("%s: an unverified chain of an ecpay_map store is never buyer_entered", k)
			}
		}
		// evidence flags (registrar-only in production; owner-pool fixture here, disclosed)
		mustExec(t, f.owner, `UPDATE integration.ecpay_logistics_profiles SET ok_verified=true WHERE tenant_id=$1 AND store_id=$2 AND enabled`, f.tenantA, f.storeA1)
		rows = opts()
		if r := rows["cvs_okmart"]; r == nil || !tcbAvailable(r) || tcvStr(r, "pickup_selection") != "ecpay_map" {
			t.Errorf("cvs_okmart after ok_verified: %v", r)
		}
		if r := rows["cvs_hilife"]; r == nil || r["available"] != false {
			t.Errorf("cvs_hilife stays coming_soon while hilife_verified=false: %v", r)
		}
		mustExec(t, f.owner, `UPDATE integration.ecpay_logistics_profiles SET hilife_verified=true WHERE tenant_id=$1 AND store_id=$2 AND enabled`, f.tenantA, f.storeA1)
		if r := opts()["cvs_hilife"]; r == nil || !tcbAvailable(r) || tcvStr(r, "pickup_selection") != "ecpay_map" {
			t.Errorf("cvs_hilife after hilife_verified: %v", r)
		}
		mustExec(t, f.owner, `UPDATE integration.ecpay_logistics_profiles SET ok_verified=false,hilife_verified=false WHERE tenant_id=$1 AND store_id=$2 AND enabled`, f.tenantA, f.storeA1)
	})

	t.Run("CVS_ECPAY_ENABLED=0 with an enabled profile: temporarily_unavailable, never buyer_entered (X9)", func(t *testing.T) {
		cfg := e.cfg
		cfg.ECPay.Enabled = false
		bc, err := checkout.NewBuyerCVS(e.p.pool, nil, nil, cfg)
		if err != nil {
			t.Fatal(err)
		}
		off := e.tcbServe(e.p.bcHarness.service.WithPaymentEnvironment("SANDBOX").WithBuyerCVS(bc))
		rows := tcbOptions(t, off, b.cap.Token, market)
		for _, k := range kinds {
			r := rows[k]
			if r == nil {
				continue // a chain hidden for other reasons is not asserted here
			}
			if r["available"] != false || tcvStr(r, "reason") != "temporarily_unavailable" || tcvStr(r, "pickup_selection") == "buyer_entered" {
				t.Errorf("%s with the kill switch off: %v", k, r)
			}
		}
		if rows["cvs_711"] == nil {
			t.Error("cvs_711 must still be listed (as unavailable) with the kill switch off")
		}
	})

	t.Run("disabling the profile falls back to buyer_entered (X9: mode is SQL-only)", func(t *testing.T) {
		_, out, _ := e.mcall(e.token(), "GET", "/v1/admin/stores/"+e.store()+"/logistics/ecpay", "", "")
		st, _, raw := e.mcall(e.token(), "POST", "/v1/admin/stores/"+e.store()+"/logistics/ecpay/enabled", t04Key("tcb-off"), fmt.Sprintf(`{"expected_version":%d,"enabled":false}`, int64(out["version"].(float64))))
		if st != 200 {
			t.Fatalf("disable: %d %s", st, raw)
		}
		for _, k := range []string{"cvs_711", "cvs_familymart"} {
			if r := opts()[k]; r == nil || tcvStr(r, "pickup_selection") != "buyer_entered" || !tcbAvailable(r) {
				t.Errorf("%s after disabling the profile: %v", k, r)
			}
		}
	})

}
