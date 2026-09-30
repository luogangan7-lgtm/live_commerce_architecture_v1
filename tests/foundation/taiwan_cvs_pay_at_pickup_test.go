package foundation_test

// TCV15 TestCvsPayAtPickupBegin, TCV16 TestCvsCollectionStatus, TCV17 TestCvsPayAtPickupRelease (contracts/taiwan-cvs-logistics-v1.md §10, §16.2-§16.5,
// §16.8, §6 last two rows, §7.4/§7.5). Prefix `tpp`. Tier REAL_PG + HTTP_PG (+ MOCK ECPay for TCV16/TCV17: the in-process dispatcher over the
// ecpaytest fake). Definers: checkout.begin_hold (pay_at_pickup branch), fulfillment.record_collection, fulfillment.ingest_ecpay_status,
// inventory.release_pay_at_pickup, fulfillment.request_cvs_shipment, fulfillment.order_money_shippable.
// Owner-pool writes (disclosed fixtures): identity grants (tcvEnv.member), aging checkout.orders.expires_at (the expiry-job case), and direct
// negative INSERTs into inventory.ledger inside rolled-back transactions (the ledger guards). Nothing else is fabricated.

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"

	"livecommerce/internal/checkout"
	"livecommerce/internal/integrations/shipping/ecpay/ecpaytest"
	"livecommerce/internal/storefront"
)

const tppName, tppPhone = "王小明", "0912345678"

// cvsSettings PUTs the store settings and tracks the version.
func (e *tcvEnv) cvsSettings(chains string, on bool, capTWD string, maxOpen int) {
	e.t.Helper()
	body := fmt.Sprintf(`{"expected_version":%d,"enabled_chains":%s,"pay_at_pickup_enabled":%v,"pay_at_pickup_max_twd":%s,"pay_at_pickup_max_open":%d}`, e.settingsVer, chains, on, capTWD, maxOpen)
	st, out, raw := e.mcall(e.token(), "PUT", "/v1/admin/stores/"+e.store()+"/logistics/cvs-settings", t04Key("tpp-settings"), body)
	if st != 200 {
		e.t.Fatalf("cvs-settings v%d: %d %s", e.settingsVer, st, raw)
	}
	if v, ok := out["version"].(float64); ok {
		e.settingsVer = int64(v)
	} else {
		e.settingsVer++
	}
}

const tcvAllChains = `["cvs_711","cvs_familymart","cvs_hilife","cvs_okmart"]`

// tppEntered enters a buyer store (buyer_entered mode: the store has no ECPay profile) and returns the pickup id.
func (e *tcvEnv) tppEntered(b *tcvBuyer, code string) string {
	e.t.Helper()
	res := b.enteredStore(code, "123456", "取貨門市", "台北市取貨路1號")
	if res.status != 201 {
		e.t.Fatalf("enter store: %d %s", res.status, res.body)
	}
	return tcvStr(tcvJSON(e.t, res.body), "pickup_id")
}

func (e *tcvEnv) tppPlace(b *tcvBuyer, code, pickup, name, phone string) (checkout.Result, error) {
	return e.tcbTry(b, "cvs_711", code, pickup, name, phone, "pay_at_pickup")
}

type tppBalance struct{ onHand, reserved, allocated int64 }

func (e *tcvEnv) tppBalance(sku string) tppBalance {
	var b tppBalance
	if err := e.p.f.owner.QueryRow(context.Background(), `SELECT on_hand,reserved,allocated FROM inventory.balances WHERE tenant_id=$1 AND store_id=$2 AND sku_id=$3`, e.tenant(), e.store(), sku).Scan(&b.onHand, &b.reserved, &b.allocated); err != nil {
		e.t.Fatal(err)
	}
	return b
}

func TestCvsPayAtPickupBegin(t *testing.T) {
	e := tcvNew(t, tcvOpts{stripe: true})
	f := e.p.f
	ctx := context.Background()
	e.r.startWorker(t)
	e.grantCreator("orders:read", "fulfillment:write", "integration:manage")
	code, _, _ := e.service("cvs_711", "MANUAL", 0)
	e.cvsSettings(tcvAllChains, true, "20000", 500)
	sku := e.p.stock.skus[0].ID

	t.Run("Begin writes: CONFIRMED order, COMMITTED reservation, RESERVE+ALLOCATE ledger rows, no payment", func(t *testing.T) {
		b := e.newBuyer()
		pickup := e.tppEntered(b, code)
		before := e.tppBalance(sku)
		stripeBefore := e.count(`SELECT count(*) FROM payments.stripe_sessions`)
		stripeReqBefore := len(e.r.fake.Requests())
		res, err := e.tppPlace(b, code, pickup, tppName, tppPhone)
		if err != nil {
			t.Fatalf("pay-at-pickup Begin: %v", err)
		}
		if res.PaymentMode != "pay_at_pickup" || res.CommercialState != "CONFIRMED" {
			t.Errorf("result must carry payment_mode and commercial_state: %+v", res)
		}
		var commercial, fulfilment, mode string
		var collection *string
		if err := f.owner.QueryRow(ctx, `SELECT commercial_state,fulfillment_state,payment_mode,collection_state FROM checkout.orders WHERE id=$1`, res.OrderID).Scan(&commercial, &fulfilment, &mode, &collection); err != nil {
			t.Fatal(err)
		}
		if commercial != "CONFIRMED" || fulfilment != "MANUAL_UNASSIGNED" || mode != "pay_at_pickup" || collection == nil || *collection != "PENDING" {
			t.Errorf("order row: %s/%s/%s/%v", commercial, fulfilment, mode, collection)
		}
		if n := e.count(`SELECT count(*) FROM inventory.reservations WHERE id=$1 AND state='COMMITTED'`, res.ReservationID); n != 1 {
			t.Error("the reservation is COMMITTED (HELD -> COMMITTED, section 11.5) at placement")
		}
		if n := e.count(`SELECT count(*) FROM inventory.ledger WHERE reservation_id=$1 AND kind='RESERVE'`, res.ReservationID); n != 1 {
			t.Errorf("RESERVE rows: %d", n)
		}
		var actor, op, key string
		var dRes, dAlloc, dHand int64
		var attempt, factKind *string
		var n int
		rows, err := f.owner.Query(ctx, `SELECT actor_kind,operation,command_key,delta_reserved,delta_allocated,delta_on_hand,payment_attempt_id::text,payment_fact_kind FROM inventory.ledger WHERE reservation_id=$1 AND kind='ALLOCATE'`, res.ReservationID)
		if err != nil {
			t.Fatal(err)
		}
		for rows.Next() {
			n++
			_ = rows.Scan(&actor, &op, &key, &dRes, &dAlloc, &dHand, &attempt, &factKind)
		}
		rows.Close()
		if n != 1 || actor != "BUYER" || op != "checkout.pay_at_pickup.commit" || key != res.OrderID || dRes != -2 || dAlloc != 2 || dHand != 0 || attempt != nil || factKind != nil {
			t.Errorf("ALLOCATE rows=%d actor=%s op=%s key=%s deltas res/alloc/hand=%d/%d/%d attempt=%v fact=%v", n, actor, op, key, dRes, dAlloc, dHand, attempt, factKind)
		}
		after := e.tppBalance(sku)
		if after.reserved != before.reserved || after.allocated != before.allocated+2 || after.onHand != before.onHand {
			t.Errorf("balance %+v -> %+v (RESERVE then ALLOCATE nets reserved 0 and allocated +2)", before, after)
		}
		for _, action := range []string{"checkout.held", "checkout.pay_at_pickup_placed"} {
			if n := e.count(`SELECT count(*) FROM checkout.events WHERE order_id=$1 AND action=$2 AND actor_kind='BUYER'`, res.OrderID, action); n != 1 {
				t.Errorf("event %s: %d rows", action, n)
			}
		}
		if n := e.count(`SELECT count(*) FROM checkout.payment_attempts WHERE order_id=$1`, res.OrderID); n != 0 {
			t.Errorf("%d payment attempts for a pay-at-pickup order", n)
		}
		if got := e.count(`SELECT count(*) FROM payments.stripe_sessions`); got != stripeBefore {
			t.Errorf("Stripe sessions %d -> %d: pay-at-pickup never touches Stripe", stripeBefore, got)
		}
		if n := e.count(`SELECT count(*) FROM payments.stripe_sessions WHERE owner_id=$1`, b.cap.Scope.OwnerID); n != 0 {
			t.Errorf("%d payments.stripe_sessions rows for the buyer", n)
		}
		if got := len(e.r.fake.Requests()); got != stripeReqBefore {
			t.Errorf("Stripe requests %d -> %d: pay-at-pickup never calls Stripe", stripeReqBefore, got)
		}

		// start_payment (PayUni mock) and start_stripe_payment both refuse a non-DRAFT order
		if _, err := e.p.starter.StartPayment(ctx, b.cap.Token, e.store(), t04Key("tpp-start"), checkout.PaymentInput{OrderID: res.OrderID, MethodCode: "payuni_credit", MethodVersion: 1}); err == nil {
			t.Error("checkout.start_payment must refuse a pay-at-pickup (non-DRAFT) order")
		}
		s := e.ro.s
		s.p.hold = res
		if _, err := s.begin(e.r.svc, t04Key("tpp-stripe"), s.input("zh-TW")); err == nil {
			t.Error("checkout.start_stripe_payment must refuse a pay-at-pickup (non-DRAFT) order")
		}
		if n := e.count(`SELECT count(*) FROM checkout.payment_attempts WHERE order_id=$1`, res.OrderID); n != 0 {
			t.Errorf("a refused payment start left %d attempts", n)
		}
		if n := e.count(`SELECT count(*) FROM payments.stripe_sessions WHERE owner_id=$1`, b.cap.Scope.OwnerID); n != 0 {
			t.Errorf("a refused Stripe start left %d sessions", n)
		}

		// the expiry job reaches STALE and releases nothing (disclosed owner-pool fixture: created_at and expires_at move together so the job is due)
		mustExec(t, f.owner, `UPDATE checkout.orders SET created_at=created_at-interval '3 hours',expires_at=expires_at-interval '3 hours' WHERE id=$1`, res.OrderID)
		var disposition string
		if err := e.p.worker.QueryRow(ctx, `SELECT disposition FROM checkout.expire_held($1::uuid,$2::bigint)`, res.OrderID, res.Generation).Scan(&disposition); err != nil {
			t.Fatalf("expire_held: %v", err)
		}
		if disposition != "STALE" {
			t.Errorf("expiry of a CONFIRMED pay-at-pickup order: %s, want STALE", disposition)
		}
		if got := e.tppBalance(sku); got != after {
			t.Errorf("expiry released stock: %+v -> %+v", after, got)
		}
		if n := e.count(`SELECT count(*) FROM inventory.reservations WHERE id=$1 AND state='COMMITTED'`, res.ReservationID); n != 1 {
			t.Error("the reservation must stay COMMITTED after the expiry job")
		}
	})

	t.Run("amount cap: total = cap accepted, +NT$1 / non-whole / > NT$20000 refused (pay_at_pickup_amount_exceeds)", func(t *testing.T) {
		e.cvsSettings(tcvAllChains, true, "100", 500) // NT$100 cap = 10000 minor
		exact, over, half := e.sku(10000, 10), e.sku(10100, 10), e.sku(50, 10)
		try := func(label string, items []storefront.Item, wantErr string) {
			b := e.newBuyer(items...)
			pickup := e.tppEntered(b, code)
			h0, a0, o0 := e.tcbEffects(b.cap.Scope.OwnerID)
			_, err := e.tppPlace(b, code, pickup, tppName, tppPhone)
			if wantErr == "" {
				if err != nil {
					t.Errorf("%s: %v", label, err)
				}
				return
			}
			tcvExpectRefusal(t, label, err, 422, wantErr)
			if h, a, o := e.tcbEffects(b.cap.Scope.OwnerID); h != h0 || a != a0 || o != o0 {
				t.Errorf("%s: a refused Begin left holds/attempts/orders %d/%d/%d -> %d/%d/%d", label, h0, a0, o0, h, a, o)
			}
		}
		try("total = pay_at_pickup_max_twd x100", []storefront.Item{{SKUID: exact, Quantity: 1}}, "")
		try("cap + NT$1", []storefront.Item{{SKUID: over, Quantity: 1}}, "pay_at_pickup_amount_exceeds")
		try("non-whole TWD (NT$0.50)", []storefront.Item{{SKUID: half, Quantity: 1}}, "pay_at_pickup_amount_exceeds")
		e.cvsSettings(tcvAllChains, true, "20000", 500)
		big, one := e.sku(200000, 30), e.sku(100, 5)
		try("NT$20000 (F20 ceiling) accepted", []storefront.Item{{SKUID: big, Quantity: 10}}, "")
		try("NT$20001 refused whatever the store cap", []storefront.Item{{SKUID: big, Quantity: 10}, {SKUID: one, Quantity: 1}}, "pay_at_pickup_amount_exceeds")
	})

	t.Run("unavailable: setting off, chain not enabled, home destination -> pay_at_pickup_unavailable with zero holds", func(t *testing.T) {
		try := func(label string, place func(b *tcvBuyer) error) {
			b := e.newBuyer()
			h0, a0, o0 := e.tcbEffects(b.cap.Scope.OwnerID)
			err := place(b)
			tcvExpectRefusal(t, label, err, 422, "pay_at_pickup_unavailable")
			if h, a, o := e.tcbEffects(b.cap.Scope.OwnerID); h != h0 || a != a0 || o != o0 {
				t.Errorf("%s: holds/attempts/orders %d/%d/%d -> %d/%d/%d", label, h0, a0, o0, h, a, o)
			}
		}
		e.cvsSettings(tcvAllChains, false, "null", 500)
		try("pay-at-pickup disabled", func(b *tcvBuyer) error {
			_, err := e.tppPlace(b, code, e.tppEntered(b, code), tppName, tppPhone)
			return err
		})
		e.cvsSettings(`["cvs_familymart"]`, true, "20000", 500)
		try("destination chain not in enabled_chains", func(b *tcvBuyer) error {
			// the buyer cannot even enter a store for a disabled chain (record_buyer_cvs_store refuses); Begin must still refuse a pickup
			// entered while the chain was enabled: enter first, then narrow the settings
			e.cvsSettings(tcvAllChains, true, "20000", 500)
			pickup := e.tppEntered(b, code)
			e.cvsSettings(`["cvs_familymart"]`, true, "20000", 500)
			_, err := e.tppPlace(b, code, pickup, tppName, tppPhone)
			return err
		})
		e.cvsSettings(tcvAllChains, true, "20000", 500)
		try("home destination", func(b *tcvBuyer) error {
			in := b.h.input // newBuyer prepared a cart, a home destination and a home quote
			in.PaymentMode = "pay_at_pickup"
			_, err := e.svc.Begin(ctx, b.cap.Token, e.store(), t04Key("tpp-home"), in)
			return err
		})
	})

	t.Run("C3 recipient rule: golden table through SQL and through Begin", func(t *testing.T) {
		raw, err := os.ReadFile("../integrations/ecpay/testdata/recipient.json")
		if err != nil {
			t.Fatal(err)
		}
		var rows []struct {
			Why             string `json:"why"`
			Name            string `json:"name"`
			Phone           string `json:"phone"`
			OK              bool   `json:"ok"`
			PhoneNormalized string `json:"phone_normalized"`
		}
		if err := json.Unmarshal(raw, &rows); err != nil {
			t.Fatal(err)
		}
		for _, r := range rows {
			var got bool
			if err := f.owner.QueryRow(ctx, `SELECT fulfillment.ecpay_recipient_ok($1,$2)`, r.Name, r.Phone).Scan(&got); err != nil {
				t.Fatalf("ecpay_recipient_ok(%q,%q): %v", r.Name, r.Phone, err)
			}
			if got != r.OK {
				t.Errorf("SQL twin, %s: fulfillment.ecpay_recipient_ok(%q,%q)=%v want %v", r.Why, r.Name, r.Phone, got, r.OK)
			}
		}
		e.cvsSettings(tcvAllChains, true, "20000", 500)
		place := func(name, phone string) error {
			b := e.newBuyer()
			pickup := e.tppEntered(b, code)
			_, err := e.tppPlace(b, code, pickup, name, phone)
			return err
		}
		for _, ok := range [][2]string{{"王小明", "0912345678"}, {"王小明", "+886912345678"}, {"王小明", "09 1234-5678"}} {
			if err := place(ok[0], ok[1]); err != nil {
				t.Errorf("recipient %q %q must be accepted: %v", ok[0], ok[1], err)
			}
		}
		for label, bad := range map[string][2]string{"landline": {"王小明", "0212345678"}, "08 prefix": {"王小明", "0812345678"}, "name with digit": {"王小明1", "0912345678"}, "one-char name": {"王", "0912345678"},
			"emoji name": {"王小明😀", "0912345678"}, "short phone": {"王小明", "091234567"}} {
			b := e.newBuyer()
			pickup := e.tppEntered(b, code)
			h0, a0, o0 := e.tcbEffects(b.cap.Scope.OwnerID)
			_, err := e.tppPlace(b, code, pickup, bad[0], bad[1])
			if err == nil {
				t.Errorf("%s: a recipient outside the ECPay rule must be refused for pay-at-pickup", label)
			}
			if h, a, o := e.tcbEffects(b.cap.Scope.OwnerID); h != h0 || a != a0 || o != o0 {
				t.Errorf("%s: holds/attempts/orders %d/%d/%d -> %d/%d/%d", label, h0, a0, o0, h, a, o)
			}
		}
	})

	t.Run("shippability, manual shipment and refunds", func(t *testing.T) {
		b := e.newBuyer()
		res, err := e.tppPlace(b, code, e.tppEntered(b, code), tppName, tppPhone)
		if err != nil {
			t.Fatal(err)
		}
		var shippable bool
		if err := f.owner.QueryRow(ctx, `SELECT fulfillment.order_money_shippable($1::uuid,$2::uuid,$3::uuid)`, f.tenantA, f.storeA1, res.OrderID).Scan(&shippable); err != nil || !shippable {
			t.Errorf("order_money_shippable for a CONFIRMED PENDING pay-at-pickup order: %v %v", shippable, err)
		}
		// the Stripe refund request refuses (no CAPTURED attempt) and writes nothing
		refundsBefore := e.count(`SELECT count(*) FROM payments.stripe_refunds`)
		st, _, raw := e.mcall(e.token(), "POST", "/v1/admin/stores/"+e.store()+"/orders/"+res.OrderID+"/refunds", t04Key("tpp-refund"), rfxBody(100, "requested_by_customer", 100))
		if st < 400 {
			t.Errorf("a refund request for a pay-at-pickup order must be refused, got %d %s", st, raw)
		}
		if got := e.count(`SELECT count(*) FROM payments.stripe_refunds`); got != refundsBefore {
			t.Errorf("%d payments.stripe_refunds rows written for a pay-at-pickup order", got-refundsBefore)
		}
		// manual shipment (0063) is allowed
		st, _, raw = e.mcall(e.token(), "PUT", "/v1/admin/stores/"+e.store()+"/orders/"+res.OrderID+"/shipment", t04Key("tpp-manual"), mfxShip(0, "seven_eleven_cvs", "0012345678"))
		if st != 200 {
			t.Errorf("0063 manual shipment of a CONFIRMED pay-at-pickup order: %d %s", st, raw)
		}
		if n := e.count(`SELECT count(*) FROM checkout.orders WHERE id=$1 AND fulfillment_state='MERCHANT_SHIPPED'`, res.OrderID); n != 1 {
			t.Error("the order must be MERCHANT_SHIPPED after the manual record")
		}
	})

	t.Run("ledger guard: a BUYER ALLOCATE for a card order is refused (42501)", func(t *testing.T) {
		hold := e.ro.s.p.hold // the DRAFT card order of the rfx store
		var owner, session, wh, skuID string
		var qty int64
		if err := f.owner.QueryRow(ctx, `SELECT o.owner_id::text,o.creator_session_id::text,l.warehouse_id::text,l.sku_id::text,l.quantity FROM checkout.orders o JOIN inventory.reservation_lines l ON l.tenant_id=o.tenant_id AND l.store_id=o.store_id AND l.reservation_id=o.id WHERE o.id=$1`, hold.OrderID).
			Scan(&owner, &session, &wh, &skuID, &qty); err != nil {
			t.Fatalf("fixture order lines: %v", err)
		}
		tx, err := f.owner.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback(ctx)
		st, _ := tcsSub(ctx, tx, `INSERT INTO inventory.ledger(tenant_id,store_id,warehouse_id,sku_id,kind,delta_reserved,delta_allocated,operation,command_key,reservation_id,actor_kind,checkout_id,buyer_owner_id,buyer_session_id,principal_id)
		  VALUES($1::uuid,$2::uuid,$3::uuid,$4::uuid,'ALLOCATE',$5::bigint,$6::bigint,'checkout.pay_at_pickup.commit',$7::text,$7::uuid,'BUYER',$7::uuid,$8::uuid,$9::uuid,NULL)`, f.tenantA, f.storeA1, wh, skuID, -qty, qty, hold.OrderID, owner, session)
		if st != "42501" {
			t.Errorf("a BUYER ALLOCATE row for a card order: want 42501 from inventory.guard_pay_at_pickup_ledger, got %q", st)
		}
	})

	t.Run("open-order limit: max_open store-wide, one per owner, two concurrent Begins at max_open-1 yield one order (round 4)", func(t *testing.T) {
		// a fresh store keeps the open-order count exact
		e2 := tcvNew(t)
		e2.grantCreator("orders:read", "fulfillment:write", "integration:manage")
		code2, _, _ := e2.service("cvs_711", "MANUAL", 0)
		e2.cvsSettings(tcvAllChains, true, "20000", 2)
		a, b := e2.newBuyer(), e2.newBuyer()
		var resA checkout.Result
		var err error
		if resA, err = e2.tppPlace(a, code2, e2.tppEntered(a, code2), tppName, tppPhone); err != nil {
			t.Fatalf("first open order: %v", err)
		}
		if _, err = e2.tppPlace(b, code2, e2.tppEntered(b, code2), tppName, tppPhone); err != nil {
			t.Fatalf("second open order: %v", err)
		}
		c := e2.newBuyer()
		pickupC := e2.tppEntered(c, code2)
		h0, at0, o0 := e2.tcbEffects(c.cap.Scope.OwnerID)
		_, err = e2.tppPlace(c, code2, pickupC, tppName, tppPhone)
		tcvExpectRefusal(t, "the (max_open+1)th open order", err, 429, "pay_at_pickup_limit")
		if h, at, o := e2.tcbEffects(c.cap.Scope.OwnerID); h != h0 || at != at0 || o != o0 {
			t.Errorf("a refused order left rows %d/%d/%d -> %d/%d/%d", h0, at0, o0, h, at, o)
		}
		// one open order per buyer owner, whatever the store limit
		e2.cvsSettings(tcvAllChains, true, "20000", 500)
		a.recart()
		pickupA2 := e2.tppEntered(a, code2)
		_, err = e2.tppPlace(a, code2, pickupA2, tppName, tppPhone)
		tcvExpectRefusal(t, "a second open order of the same buyer", err, 429, "pay_at_pickup_limit")
		_ = resA
		// concurrency: at max_open-1 open orders two simultaneous Begins produce exactly one order
		e2.cvsSettings(tcvAllChains, true, "20000", 3) // open = 2 = max_open-1
		type ready struct {
			b     *tcvBuyer
			dest  storefront.Destination
			quote storefront.Quote
		}
		var racers []ready
		for i := 0; i < 2; i++ {
			rb := e2.newBuyer()
			pk := e2.tppEntered(rb, code2)
			dest, err := rb.destination("cvs_711", pk, tppName, tppPhone)
			if err != nil {
				t.Fatal(err)
			}
			quote, err := rb.quote(code2)
			if err != nil {
				t.Fatal(err)
			}
			racers = append(racers, ready{rb, dest, quote})
		}
		var wg sync.WaitGroup
		start := make(chan struct{})
		results := make([]error, len(racers))
		for i := range racers {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				<-start
				_, results[i] = racers[i].b.begin(racers[i].dest, racers[i].quote, e2.svcVer[code2], "pay_at_pickup")
			}(i)
		}
		close(start)
		wg.Wait()
		ok, limited := 0, 0
		for _, err := range results {
			if err == nil {
				ok++
			} else if s, c := tcvRefusal(err); s == 429 && c == "pay_at_pickup_limit" {
				limited++
			} else {
				t.Errorf("unexpected concurrent Begin result: %v", err)
			}
		}
		if ok != 1 || limited != 1 {
			t.Errorf("two concurrent Begins at max_open-1: %d succeeded, %d limited (want exactly one of each)", ok, limited)
		}
		if n := e2.count(`SELECT count(*) FROM checkout.orders WHERE store_id=$1 AND payment_mode='pay_at_pickup' AND collection_state='PENDING'`, e2.store()); n != 3 {
			t.Errorf("open orders after the race: %d, want exactly max_open=3", n)
		}
	})

	t.Run("over the real buyer HTTP route: coded refusals, never a retryable 503", func(t *testing.T) {
		// POST /v1/buyer/checkout is the route the storefront calls. Every CVS refusal of Begin is a definitive, coded 422/429 (section 16.2);
		// a 503 would tell the storefront "could not confirm, retry" for a request that was refused for good.
		e.cvsSettings(tcvAllChains, true, "100", 500) // NT$100 cap
		over := e.sku(10100, 10)
		b := e.newBuyer(storefront.Item{SKUID: over, Quantity: 1})
		pickup := e.tppEntered(b, code)
		dest, err := b.destination("cvs_711", pickup, tppName, tppPhone)
		if err != nil {
			t.Fatal(err)
		}
		quote, err := b.quote(code)
		if err != nil {
			t.Fatal(err)
		}
		body := map[string]any{"quote_id": quote.ID, "destination_id": dest.ID, "cart_version": b.cartVersion(), "service_version": 1, "allocation_version": 1, "payment_mode": "pay_at_pickup"}
		res := b.req("POST", "/v1/buyer/checkout", t04Key("tpp-http"), body, nil)
		if res.status != 422 || tcvStr(tcvJSON(t, res.body), "code") != "pay_at_pickup_amount_exceeds" {
			t.Errorf("over the store cap through HTTP: want 422 pay_at_pickup_amount_exceeds, got %d %s", res.status, res.body)
		}
		// the (max_open+1)th order: 429 with Retry-After
		e.cvsSettings(tcvAllChains, true, "20000", 500)
		open := e.count(`SELECT count(*) FROM checkout.orders WHERE store_id=$1 AND payment_mode='pay_at_pickup' AND collection_state='PENDING' AND fulfillment_state='MANUAL_UNASSIGNED'`, e.store())
		e.cvsSettings(tcvAllChains, true, "20000", open)
		b2 := e.newBuyer()
		pk2 := e.tppEntered(b2, code)
		d2, err := b2.destination("cvs_711", pk2, tppName, tppPhone)
		if err != nil {
			t.Fatal(err)
		}
		q2, err := b2.quote(code)
		if err != nil {
			t.Fatal(err)
		}
		res = b2.req("POST", "/v1/buyer/checkout", t04Key("tpp-http-limit"), map[string]any{"quote_id": q2.ID, "destination_id": d2.ID, "cart_version": b2.cartVersion(), "service_version": 1, "allocation_version": 1, "payment_mode": "pay_at_pickup"}, nil)
		if res.status != 429 || tcvStr(tcvJSON(t, res.body), "code") != "pay_at_pickup_limit" || res.header.Get("Retry-After") == "" {
			t.Errorf("at max_open through HTTP: want 429 pay_at_pickup_limit with Retry-After, got %d %s", res.status, res.body)
		}
		e.cvsSettings(tcvAllChains, true, "20000", 500)
	})

	t.Run("payment_mode is a closed vocabulary", func(t *testing.T) {
		b := e.newBuyer()
		pickup := e.tppEntered(b, code)
		h0, a0, o0 := e.tcbEffects(b.cap.Scope.OwnerID)
		if _, err := e.tcbTry(b, "cvs_711", code, pickup, tppName, tppPhone, "cash"); err == nil {
			t.Error("an unknown payment_mode must be refused")
		}
		if h, a, o := e.tcbEffects(b.cap.Scope.OwnerID); h != h0 || a != a0 || o != o0 {
			t.Errorf("unknown payment_mode left rows: %d/%d/%d -> %d/%d/%d", h0, a0, o0, h, a, o)
		}
	})
}

// tppStatuses posts a sequence of signed status notifications for an order's trade and requires 1|OK for each.
func (e *tcvEnv) tppStatuses(endpoint, order string, codes ...string) {
	e.t.Helper()
	for _, c := range codes {
		w := e.postStatus(endpoint, e.statusFor(order, c))
		if w.Code != 200 || w.Body.String() != "1|OK" {
			e.t.Fatalf("status %s for %s: %d %q (want 200 1|OK)", c, order, w.Code, w.Body.String())
		}
	}
}

func (e *tcvEnv) collectionState(order string) string {
	var s *string
	if err := e.p.f.owner.QueryRow(context.Background(), `SELECT collection_state FROM checkout.orders WHERE id=$1`, order).Scan(&s); err != nil {
		e.t.Fatal(err)
	}
	if s == nil {
		return ""
	}
	return *s
}

func (e *tcvEnv) collectionPath(order string) string {
	return "/v1/admin/stores/" + e.store() + "/orders/" + order + "/collection"
}

func (e *tcvEnv) record(token, order, key, expected, state string) (int, map[string]any, []byte) {
	return e.mcall(token, "POST", e.collectionPath(order), key, fmt.Sprintf(`{"expected_state":%q,"state":%q}`, expected, state))
}

func (e *tcvEnv) audit(action string) int {
	return e.count(`SELECT count(*) FROM ops.audit_events WHERE tenant_id=$1 AND store_id=$2 AND action=$3`, e.tenant(), e.store(), action)
}

func (e *tcvEnv) events(order, whereExtra string, args ...any) int {
	return e.count(`SELECT count(*) FROM fulfillment.cvs_shipment_events WHERE order_id=$1 `+whereExtra, append([]any{order}, args...)...)
}

func TestCvsCollectionStatus(t *testing.T) {
	e := tcvNew(t, tcvOpts{stripe: true})
	f := e.p.f
	ctx := context.Background()
	e.r.startWorker(t)
	e.startDispatcher()
	e.grantCreator("orders:read", "fulfillment:write", "integration:manage", "integration:read")
	e.connect("C2C")
	mustExec(t, f.owner, `UPDATE integration.ecpay_logistics_profiles SET hilife_verified=true WHERE tenant_id=$1 AND store_id=$2 AND enabled`, f.tenantA, f.storeA1) // registrar-only in production (disclosed fixture)
	e.cvsSettings(tcvAllChains, true, "20000", 500)
	api711, _, _ := e.service("cvs_711", "API", 0)
	apiFami, _, _ := e.service("cvs_familymart", "API", 0)
	apiHilife, _, _ := e.service("cvs_hilife", "API", 0)
	endpoint := e.endpointID()
	pap := func(kind, code string) (string, *tcvBuyer) {
		return e.cvsOrder(tcvOrderSpec{kind: kind, code: code, paymentMode: "pay_at_pickup"})
	}
	shipCreated := func(order string) {
		st, _, raw := e.ship(e.token(), order, 0, "", true)
		if st != 202 {
			t.Fatalf("request shipment: %d %s", st, raw)
		}
		e.awaitShip(order, "CREATED")
	}

	t.Run("frozen collection_amount = goods_amount = total/100; Create carries IsCollection=Y + CollectionAmount (card: N)", func(t *testing.T) {
		order, _ := pap("cvs_711", api711)
		st, out, raw := e.ship(e.token(), order, 0, "", true)
		if st != 202 || tcvStr(out, "state") != "REQUESTED" {
			t.Fatalf("request for a CONFIRMED pay-at-pickup order with an ecpay_map pickup: %d %s (want 202 REQUESTED)", st, raw)
		}
		e.awaitShip(order, "CREATED")
		var goods, collection int
		if err := f.owner.QueryRow(ctx, `SELECT goods_amount,collection_amount FROM fulfillment.cvs_shipments WHERE order_id=$1`, order).Scan(&goods, &collection); err != nil || goods != 25 || collection != 25 {
			t.Errorf("frozen amounts goods=%d collection=%d err=%v (order total 2500 minor = NT$25)", goods, collection, err)
		}
		tr, _ := e.fake.Trade(e.tradeNo(order))
		if tr.IsCollection != "Y" || tr.CollectionAmount != "25" || tr.GoodsAmount != "25" {
			t.Errorf("the fake's Create received IsCollection=%q CollectionAmount=%q GoodsAmount=%q, want Y/25/25", tr.IsCollection, tr.CollectionAmount, tr.GoodsAmount)
		}
		if n := e.count(`SELECT count(*) FROM fulfillment.cvs_shipments WHERE order_id=$1 AND collection_amount IS NOT DISTINCT FROM goods_amount`, order); n != 1 {
			t.Error("collection_amount must equal goods_amount (F20)")
		}
		// a card order: IsCollection=N, no CollectionAmount, NULL collection_amount
		card, _ := e.cvsOrder(tcvOrderSpec{kind: "cvs_711", code: api711})
		shipCreated(card)
		tr, _ = e.fake.Trade(e.tradeNo(card))
		var cardCollection *int
		if err := f.owner.QueryRow(ctx, `SELECT collection_amount FROM fulfillment.cvs_shipments WHERE order_id=$1`, card).Scan(&cardCollection); err != nil || cardCollection != nil {
			t.Errorf("card order collection_amount %v err %v, want NULL", cardCollection, err)
		}
		if tr.IsCollection != "N" || tr.CollectionAmount != "" {
			t.Errorf("card order Create carried IsCollection=%q CollectionAmount=%q, want N and none", tr.IsCollection, tr.CollectionAmount)
		}
	})

	t.Run("signed pickup and return codes per subtype: state, collection_state, audit, no money rows", func(t *testing.T) {
		type sub struct {
			kind, code, atDC, atStore, picked, returned string
		}
		for _, c := range []sub{{"cvs_711", api711, "2030", "2073", "2067", "2074"}, {"cvs_familymart", apiFami, "3024", "3018", "3022", "3020"}, {"cvs_hilife", apiHilife, "3024", "3018", "3022", "3020"}} {
			// picked up
			order, _ := pap(c.kind, c.code)
			shipCreated(order)
			ledger0 := e.count(`SELECT count(*) FROM inventory.ledger WHERE reservation_id=$1`, order)
			e.tppStatuses(endpoint, order, c.atDC)
			if st, _, _ := e.shipState(order); st != "AT_DC" {
				t.Errorf("%s %s: state %s want AT_DC", c.kind, c.atDC, st)
			}
			e.tppStatuses(endpoint, order, c.atStore)
			if st, _, _ := e.shipState(order); st != "AT_STORE" || e.collectionState(order) != "PENDING" {
				t.Errorf("%s %s: state %s collection %s", c.kind, c.atStore, st, e.collectionState(order))
			}
			auditBefore := e.audit("fulfillment.collection_reported")
			pickedBody := e.statusFor(order, c.picked)
			if w := e.postStatus(endpoint, pickedBody); w.Code != 200 || w.Body.String() != "1|OK" {
				t.Fatalf("%s %s: %d %q", c.kind, c.picked, w.Code, w.Body.String())
			}
			if st, _, _ := e.shipState(order); st != "PICKED_UP" || e.collectionState(order) != "COLLECTED" {
				t.Errorf("%s %s: state %s collection %s, want PICKED_UP/COLLECTED", c.kind, c.picked, st, e.collectionState(order))
			}
			if got := e.audit("fulfillment.collection_reported"); got != auditBefore+1 {
				t.Errorf("%s: audit fulfillment.collection_reported %d -> %d", c.kind, auditBefore, got)
			}
			// a duplicate report is one transition (one event, still 1|OK)
			events := e.events(order, "AND provider_code=$2", c.picked)
			if w := e.postStatus(endpoint, pickedBody); w.Code != 200 || w.Body.String() != "1|OK" { // ECPay re-delivers the identical body
				t.Fatalf("%s duplicate delivery: %d %q", c.kind, w.Code, w.Body.String())
			}
			if got := e.events(order, "AND provider_code=$2", c.picked); got != events || e.audit("fulfillment.collection_reported") != auditBefore+1 {
				t.Errorf("%s: a duplicate report wrote a second event/audit (%d -> %d)", c.kind, events, got)
			}
			if got := e.count(`SELECT count(*) FROM inventory.ledger WHERE reservation_id=$1`, order); got != ledger0 {
				t.Errorf("%s: a collection wrote %d ledger row(s)", c.kind, got-ledger0)
			}
			// unclaimed / returned
			order2, _ := pap(c.kind, c.code)
			shipCreated(order2)
			e.tppStatuses(endpoint, order2, c.atDC, c.atStore)
			ledger2 := e.count(`SELECT count(*) FROM inventory.ledger WHERE reservation_id=$1`, order2)
			pay0, refunds0 := e.count(`SELECT count(*) FROM checkout.payment_attempts WHERE order_id=$1`, order2), e.count(`SELECT count(*) FROM payments.stripe_refunds`)
			e.tppStatuses(endpoint, order2, c.returned)
			if st, _, _ := e.shipState(order2); st != "UNCLAIMED" || e.collectionState(order2) != "RETURNED" {
				t.Errorf("%s %s: state %s collection %s, want UNCLAIMED/RETURNED", c.kind, c.returned, st, e.collectionState(order2))
			}
			if got := e.count(`SELECT count(*) FROM inventory.ledger WHERE reservation_id=$1`, order2); got != ledger2 {
				t.Errorf("%s: the return report wrote %d ledger row(s) (restock is the merchant's explicit action)", c.kind, got-ledger2)
			}
			if e.count(`SELECT count(*) FROM checkout.payment_attempts WHERE order_id=$1`, order2) != pay0 || e.count(`SELECT count(*) FROM payments.stripe_refunds`) != refunds0 {
				t.Errorf("%s: a return report created a payment attempt or refund row", c.kind)
			}
		}
		// recipient PII of the notifications is never stored (TD8)
		if n := e.count(`SELECT count(*) FROM fulfillment.cvs_shipment_events WHERE tenant_id=$1 AND (provider_message ILIKE '%SENTINELRECIPIENT%' OR event_code ILIKE '%SENTINELRECIPIENT%')`, e.tenant()); n != 0 {
			t.Errorf("%d event rows carry the recipient sentinel", n)
		}
	})

	t.Run("merchant collection action: permissions, states, replay, conflicts", func(t *testing.T) {
		readOnly, _ := e.member("orders:read")
		writer, _ := e.member("fulfillment:write", "orders:read")
		// a manually shipped pay-at-pickup order (0063 record) — buyer_entered-style order from the same store is fine: use an ECPay-verified one
		order, _ := pap("cvs_711", api711)
		if st, _, raw := e.record(e.token(), order, t04Key("tpp-early"), "PENDING", "collected"); st != 422 || tcvStr(tcvJSON(t, raw), "code") != "not_shipped" {
			t.Errorf("collected before shipping: want 422 not_shipped, got %d %s", st, raw)
		}
		st, _, raw := e.mcall(e.token(), "PUT", "/v1/admin/stores/"+e.store()+"/orders/"+order+"/shipment", t04Key("tpp-ms"), mfxShip(0, "seven_eleven_cvs", "0012345678"))
		if st != 200 {
			t.Fatalf("manual shipment: %d %s", st, raw)
		}
		if st, _, _ := e.record(readOnly, order, t04Key("tpp-ro"), "PENDING", "collected"); st != 403 {
			t.Errorf("orders:read only: want 403, got %d", st)
		}
		key := t04Key("tpp-collect")
		st, first, raw := e.record(writer, order, key, "PENDING", "collected")
		if st != 200 || e.collectionState(order) != "COLLECTED" || e.audit("fulfillment.collection_recorded") < 1 {
			t.Fatalf("collected: %d %s state=%s", st, raw, e.collectionState(order))
		}
		auditN := e.audit("fulfillment.collection_recorded")
		if st, again, _ := e.record(writer, order, key, "PENDING", "collected"); st != 200 || fmt.Sprint(again) != fmt.Sprint(first) || e.audit("fulfillment.collection_recorded") != auditN {
			t.Errorf("replay of the same key+body: %d %v (want the saved answer, no second audit)", st, again)
		}
		if st, _, raw := e.record(writer, order, key, "PENDING", "returned"); st != 409 || tcvStr(tcvJSON(t, raw), "code") != "idempotency_conflict" {
			t.Errorf("same key, other body: want 409 idempotency_conflict, got %d %s", st, raw)
		}
		if st, _, raw := e.record(writer, order, t04Key("tpp-stale"), "PENDING", "collected"); st != 409 || tcvStr(tcvJSON(t, raw), "code") != "collection_state_changed" {
			t.Errorf("stale expected_state: want 409 collection_state_changed, got %d %s", st, raw)
		}
		if st, _, raw := e.record(writer, order, t04Key("tpp-refoff"), "COLLECTED", "refunded_offline"); st != 200 || e.collectionState(order) != "REFUNDED_OFFLINE" {
			t.Errorf("refunded_offline from COLLECTED: %d %s state=%s", st, raw, e.collectionState(order))
		}
		// refunded_offline only from COLLECTED
		other, _ := pap("cvs_711", api711)
		if st, _, raw := e.record(writer, other, t04Key("tpp-refoff2"), "PENDING", "refunded_offline"); st == 200 {
			t.Errorf("refunded_offline from PENDING must be refused: %d %s", st, raw)
		}
		// card orders refuse the action
		card, _ := e.cvsOrder(tcvOrderSpec{kind: "cvs_711", code: api711})
		if st, _, raw := e.record(writer, card, t04Key("tpp-card"), "PENDING", "collected"); st != 422 || tcvStr(tcvJSON(t, raw), "code") != "not_pay_at_pickup" {
			t.Errorf("card order: want 422 not_pay_at_pickup, got %d %s", st, raw)
		}
		// a later ECPay report that conflicts with the merchant's record: event only + alert, the record stays
		conflict, _ := pap("cvs_711", api711)
		shipCreated(conflict)
		e.tppStatuses(endpoint, conflict, "2030", "2073")
		if st, _, raw := e.record(writer, conflict, t04Key("tpp-conf"), "PENDING", "collected"); st != 200 {
			t.Fatalf("merchant collected on an AT_STORE ECPay order: %d %s", st, raw)
		}
		e.tppStatuses(endpoint, conflict, "2074") // ECPay says unclaimed/returned
		if got := e.collectionState(conflict); got != "COLLECTED" {
			t.Errorf("a conflicting ECPay report changed the merchant's record: %s", got)
		}
		if n := e.events(conflict, "AND event_code LIKE '%collection_conflict'"); n < 1 {
			t.Error("a conflicting report must raise the collection_conflict alert (event)")
		}
	})

	t.Run("request with another environment's pickup namespace: cvs_environment_mismatch", func(t *testing.T) {
		order, _ := pap("cvs_711", api711)
		// disclosed plant: the order's pickup version moves to the LIVE namespace (a state Begin can no longer produce because it pins the
		// environment); FK triggers are off for this one statement so only the namespace column changes
		tx, err := f.owner.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := tx.Exec(ctx, `SET LOCAL session_replication_role=replica`); err != nil {
			t.Fatal(err)
		}
		tag, err := tx.Exec(ctx, `UPDATE fulfillment.pickup_versions v SET namespace='ecpay.live.unimartc2c' FROM checkout.orders o WHERE o.id=$1 AND v.id=(o.snapshot#>>'{destination,pickup,id}')::uuid`, order)
		if err != nil || tag.RowsAffected() != 1 {
			tx.Rollback(ctx)
			t.Skipf("NOT_RUN: cannot plant a LIVE-namespace pickup for the frozen order (rows=%d err=%v)", tag.RowsAffected(), err)
		}
		if err := tx.Commit(ctx); err != nil {
			t.Fatal(err)
		}
		st, _, raw := e.ship(e.token(), order, 0, "", false)
		if st != 422 || tcvStr(tcvJSON(t, raw), "code") != "cvs_environment_mismatch" {
			t.Errorf("a pickup namespace of another environment: want 422 cvs_environment_mismatch, got %d %s", st, raw)
		}
		if n := e.count(`SELECT count(*) FROM fulfillment.cvs_shipments WHERE order_id=$1`, order); n != 0 {
			t.Errorf("%d shipment rows for a refused request", n)
		}
	})
}

// ---- TCV17 ---------------------------------------------------------------------------------------------------------------

func (e *tcvEnv) release(token, order, key, action, expected string) (int, map[string]any, []byte) {
	return e.mcall(token, "POST", "/v1/admin/stores/"+e.store()+"/orders/"+order+"/pay-at-pickup-release", key, fmt.Sprintf(`{"action":%q,"expected_state":%q}`, action, expected))
}

type tppDealloc struct {
	allocated, onHand, reserved, unavailable      int64
	actor, op, key, reason, checkout, reservation string
	principal, owner, session                     string
}

func (e *tcvEnv) deallocRows(order string) []tppDealloc {
	e.t.Helper()
	rows, err := e.p.f.owner.Query(context.Background(), `SELECT delta_allocated,delta_on_hand,delta_reserved,delta_unavailable,actor_kind,operation,command_key,reason,checkout_id::text,reservation_id::text,
	  coalesce(principal_id::text,''),coalesce(buyer_owner_id::text,''),coalesce(buyer_session_id::text,'') FROM inventory.ledger WHERE reservation_id=$1 AND kind='DEALLOCATE' ORDER BY sku_id`, order)
	if err != nil {
		e.t.Fatal(err)
	}
	defer rows.Close()
	var out []tppDealloc
	for rows.Next() {
		var d tppDealloc
		if err := rows.Scan(&d.allocated, &d.onHand, &d.reserved, &d.unavailable, &d.actor, &d.op, &d.key, &d.reason, &d.checkout, &d.reservation, &d.principal, &d.owner, &d.session); err != nil {
			e.t.Fatal(err)
		}
		out = append(out, d)
	}
	return out
}

func (e *tcvEnv) providerRows() [4]int {
	return [4]int{e.count(`SELECT count(*) FROM integration.operations`), e.count(`SELECT count(*) FROM river.river_job`),
		e.count(`SELECT count(*) FROM payments.stripe_refunds`), e.count(`SELECT count(*) FROM checkout.payment_attempts`)}
}

func TestCvsPayAtPickupRelease(t *testing.T) {
	e := tcvNew(t, tcvOpts{stripe: true})
	f := e.p.f
	ctx := context.Background()
	e.r.startWorker(t)
	e.startDispatcher()
	e.grantCreator("orders:read", "fulfillment:write", "integration:manage", "integration:read")
	man, _, _ := e.service("cvs_711", "MANUAL", 0)
	e.cvsSettings(tcvAllChains, true, "20000", 500)
	sku := e.p.stock.skus[0].ID
	writer, writerPrincipal := e.member("fulfillment:write", "orders:read")
	readOnly, _ := e.member("orders:read")
	entered := func() (string, *tcvBuyer, checkout.Result) {
		b := e.newBuyer()
		res, err := e.tppPlace(b, man, e.tppEntered(b, man), tppName, tppPhone)
		if err != nil {
			t.Fatalf("place a pay-at-pickup order: %v", err)
		}
		return res.OrderID, b, res
	}
	orderMoney := func(order string) (commercial, fulfilment, collection, reservation string) {
		if err := f.owner.QueryRow(ctx, `SELECT o.commercial_state,o.fulfillment_state,coalesce(o.collection_state,''),r.state FROM checkout.orders o JOIN inventory.reservations r ON r.tenant_id=o.tenant_id AND r.store_id=o.store_id AND r.id=o.id WHERE o.id=$1`, order).Scan(&commercial, &fulfilment, &collection, &reservation); err != nil {
			t.Fatal(err)
		}
		return
	}

	t.Run("cancel a PENDING unshipped order: states, DEALLOCATE rows, balance, audit, no provider rows; replay and conflicts", func(t *testing.T) {
		balBefore := e.tppBalance(sku)
		order, b, _ := entered()
		balPlaced := e.tppBalance(sku)
		if balPlaced.allocated != balBefore.allocated+2 {
			t.Fatalf("placement should allocate 2: %+v -> %+v", balBefore, balPlaced)
		}
		rowsBefore := e.providerRows()
		key := t04Key("tpp-cancel")
		st, out, raw := e.release(writer, order, key, "cancel", "PENDING")
		if st != 200 || tcvStr(out, "order_id") != order || tcvStr(out, "collection_state") != "CANCELLED" || tcvStr(out, "commercial_state") != "CANCELLED" || out["released_lines"] != float64(1) {
			t.Fatalf("cancel: %d %s", st, raw)
		}
		c, fu, col, res := orderMoney(order)
		if c != "CANCELLED" || fu != "CANCELLED" || col != "CANCELLED" || res != "RELEASED" {
			t.Errorf("states commercial=%s fulfillment=%s collection=%s reservation=%s", c, fu, col, res)
		}
		rows := e.deallocRows(order)
		if len(rows) != 1 {
			t.Fatalf("DEALLOCATE rows: %d, want one per line", len(rows))
		}
		d := rows[0]
		if d.allocated != -2 || d.onHand != 0 || d.reserved != 0 || d.unavailable != 0 || d.actor != "MERCHANT" || d.principal != writerPrincipal || d.checkout != order || d.reservation != order ||
			d.owner != b.cap.Scope.OwnerID || d.session != b.cap.Scope.SessionID || d.op != "fulfillment.pay_at_pickup.cancel" || d.key != order || d.reason != "PENDING" {
			t.Errorf("DEALLOCATE row: %+v", d)
		}
		// on_hand differs from balBefore only by the fixture's own top-up inside newBuyer; allocated and reserved must be back to pre-Begin
		if after := e.tppBalance(sku); after.allocated != balBefore.allocated || after.reserved != balBefore.reserved || after.onHand != balPlaced.onHand {
			t.Errorf("balance after cancel %+v, want allocated/reserved of the pre-Begin value %+v (on_hand %d)", after, balBefore, balPlaced.onHand)
		}
		if e.audit("fulfillment.pay_at_pickup_cancelled") < 1 {
			t.Error("audit fulfillment.pay_at_pickup_cancelled missing")
		}
		if got := e.providerRows(); got != rowsBefore {
			t.Errorf("payment/refund/operation/River rows %v -> %v: a cancel writes none", rowsBefore, got)
		}
		// replay: same key + same body -> the saved answer, zero extra ledger rows
		ledger := e.count(`SELECT count(*) FROM inventory.ledger WHERE reservation_id=$1`, order)
		if st2, again, _ := e.release(writer, order, key, "cancel", "PENDING"); st2 != 200 || fmt.Sprint(again) != fmt.Sprint(out) || e.count(`SELECT count(*) FROM inventory.ledger WHERE reservation_id=$1`, order) != ledger {
			t.Errorf("replay: %d %v", st2, again)
		}
		if st2, _, raw2 := e.release(writer, order, key, "restock", "RETURNED"); st2 != 409 || tcvStr(tcvJSON(t, raw2), "code") != "idempotency_conflict" {
			t.Errorf("same key, other body: want 409 idempotency_conflict, got %d %s", st2, raw2)
		}
		if st2, _, raw2 := e.release(writer, order, t04Key("tpp-cancel2"), "cancel", "PENDING"); st2 != 409 || tcvStr(tcvJSON(t, raw2), "code") != "collection_state_changed" {
			t.Errorf("a new key on a released order: want 409 collection_state_changed, got %d %s", st2, raw2)
		}
	})

	t.Run("the freed slot lets a Begin at max_open succeed", func(t *testing.T) {
		open := e.count(`SELECT count(*) FROM checkout.orders WHERE store_id=$1 AND payment_mode='pay_at_pickup' AND collection_state='PENDING' AND fulfillment_state='MANUAL_UNASSIGNED'`, e.store())
		e.cvsSettings(tcvAllChains, true, "20000", open+1)
		victim, _, _ := entered() // fills the store to max_open
		b := e.newBuyer()
		pickup := e.tppEntered(b, man)
		if _, err := e.tppPlace(b, man, pickup, tppName, tppPhone); err == nil {
			t.Fatal("the store is at pay_at_pickup_max_open: a further order must be refused")
		} else {
			tcvExpectRefusal(t, "at max_open", err, 429, "pay_at_pickup_limit")
		}
		if st, _, raw := e.release(writer, victim, t04Key("tpp-free"), "cancel", "PENDING"); st != 200 {
			t.Fatalf("cancel: %d %s", st, raw)
		}
		if _, err := e.tppPlace(b, man, pickup, tppName, tppPhone); err != nil {
			t.Errorf("after the cancel the slot is free: %v", err)
		}
		e.cvsSettings(tcvAllChains, true, "20000", 500)
	})

	t.Run("refusals: card order, orders:read only, revoked member replay, stale state, manual shipment", func(t *testing.T) {
		// card order (RD6: card orders keep the no-cancel rule)
		card := e.payHoldFor(t, e.newBuyer())
		ledger := e.count(`SELECT count(*) FROM inventory.ledger WHERE reservation_id=$1 AND kind='DEALLOCATE'`, card)
		if st, _, raw := e.release(writer, card, t04Key("tpp-card"), "cancel", "PENDING"); st != 422 || tcvStr(tcvJSON(t, raw), "code") != "not_pay_at_pickup" {
			t.Errorf("card order: want 422 not_pay_at_pickup, got %d %s", st, raw)
		}
		if got := e.count(`SELECT count(*) FROM inventory.ledger WHERE reservation_id=$1 AND kind='DEALLOCATE'`, card); got != ledger {
			t.Errorf("a refused card cancel wrote %d ledger rows", got-ledger)
		}
		order, _, _ := entered()
		if st, _, _ := e.release(readOnly, order, t04Key("tpp-ro"), "cancel", "PENDING"); st != 403 {
			t.Errorf("orders:read only: want 403, got %d", st)
		}
		// section 16.8: expected_state must equal the current state, else 409 collection_state_changed. A cancel carrying RETURNED is both a
		// stale state and not the value a cancel takes; the contract text is silent on which wins (recorded in NOT_RUN.md), so 409 and 422 are
		// both accepted, and nothing may be written.
		if st, _, raw := e.release(writer, order, t04Key("tpp-stale"), "cancel", "RETURNED"); st != 409 && st != 422 {
			t.Errorf("expected_state RETURNED on a PENDING order: want 409 collection_state_changed (or 422), got %d %s", st, raw)
		}
		if e.collectionState(order) != "PENDING" {
			t.Error("a refused cancel changed the order")
		}
		if st, _, raw := e.release(writer, order, t04Key("tpp-bad-action"), "refund", "PENDING"); st < 400 {
			t.Errorf("an unknown action must be refused: %d %s", st, raw)
		}
		// a revoked member replaying a valid key gets 403, not the saved answer
		tmp, tmpPrincipal := e.member("fulfillment:write", "orders:read")
		key := t04Key("tpp-revoked")
		if st, _, raw := e.release(tmp, order, key, "cancel", "PENDING"); st != 200 {
			t.Fatalf("cancel by the temporary member: %d %s", st, raw)
		}
		mustExec(t, f.owner, `DELETE FROM identity.store_grants WHERE tenant_id=$1 AND store_id=$2 AND principal_id=$3 AND permission='fulfillment:write'`, f.tenantA, f.storeA1, tmpPrincipal)
		if st, _, raw := e.release(tmp, order, key, "cancel", "PENDING"); st != 403 {
			t.Errorf("a revoked member replaying a valid key: want 403, got %d %s", st, raw)
		}
		// 0063 manual shipment: not cancellable (the parcel left the merchant)
		shipped, _, _ := entered()
		if st, _, raw := e.mcall(e.token(), "PUT", "/v1/admin/stores/"+e.store()+"/orders/"+shipped+"/shipment", t04Key("tpp-ms"), mfxShip(0, "seven_eleven_cvs", "0012345678")); st != 200 {
			t.Fatalf("manual shipment: %d %s", st, raw)
		}
		if st, _, raw := e.release(writer, shipped, t04Key("tpp-shipped"), "cancel", "PENDING"); st != 422 || tcvStr(tcvJSON(t, raw), "code") != "not_cancellable" {
			t.Errorf("manually shipped order: want 422 not_cancellable, got %d %s", st, raw)
		}
		if n := len(e.deallocRows(shipped)); n != 0 {
			t.Errorf("a refused cancel wrote %d DEALLOCATE rows", n)
		}
	})

	t.Run("restock: merchant returned -> RESTOCKED; PENDING/COLLECTED refused; replay", func(t *testing.T) {
		order, b, _ := entered()
		if st, _, raw := e.mcall(e.token(), "PUT", "/v1/admin/stores/"+e.store()+"/orders/"+order+"/shipment", t04Key("tpp-ms2"), mfxShip(0, "seven_eleven_cvs", "0012345679")); st != 200 {
			t.Fatalf("manual shipment: %d %s", st, raw)
		}
		if st, _, raw := e.release(writer, order, t04Key("tpp-rs-pending"), "restock", "RETURNED"); st != 409 || tcvStr(tcvJSON(t, raw), "code") != "collection_state_changed" {
			t.Errorf("restock of a PENDING order: want 409 collection_state_changed, got %d %s", st, raw)
		}
		if st, _, raw := e.record(writer, order, t04Key("tpp-ret"), "PENDING", "returned"); st != 200 {
			t.Fatalf("merchant returned: %d %s", st, raw)
		}
		balBefore := e.tppBalance(sku)
		key := t04Key("tpp-restock")
		st, out, raw := e.release(writer, order, key, "restock", "RETURNED")
		if st != 200 || tcvStr(out, "collection_state") != "RESTOCKED" || e.collectionState(order) != "RESTOCKED" {
			t.Fatalf("restock: %d %s", st, raw)
		}
		rows := e.deallocRows(order)
		if len(rows) != 1 || rows[0].op != "fulfillment.pay_at_pickup.restock" || rows[0].reason != "RETURNED" || rows[0].allocated != -2 || rows[0].principal != writerPrincipal || rows[0].owner != b.cap.Scope.OwnerID {
			t.Errorf("restock ledger rows: %+v", rows)
		}
		if after := e.tppBalance(sku); after.allocated != balBefore.allocated-2 {
			t.Errorf("balance %+v -> %+v: restock releases the order's allocation", balBefore, after)
		}
		if c, fu, _, _ := orderMoney(order); c != "CONFIRMED" || fu != "MERCHANT_SHIPPED" {
			t.Errorf("restock keeps commercial/fulfillment states (0063 guards): %s/%s", c, fu)
		}
		if e.audit("fulfillment.pay_at_pickup_restocked") < 1 {
			t.Error("audit fulfillment.pay_at_pickup_restocked missing")
		}
		if st2, again, _ := e.release(writer, order, key, "restock", "RETURNED"); st2 != 200 || fmt.Sprint(again) != fmt.Sprint(out) || len(e.deallocRows(order)) != 1 {
			t.Errorf("restock replay: %d %v", st2, again)
		}
		// COLLECTED cannot be restocked
		collected, _, _ := entered()
		e.mcall(e.token(), "PUT", "/v1/admin/stores/"+e.store()+"/orders/"+collected+"/shipment", t04Key("tpp-ms3"), mfxShip(0, "seven_eleven_cvs", "0012345680"))
		if st, _, _ := e.record(writer, collected, t04Key("tpp-col"), "PENDING", "collected"); st != 200 {
			t.Fatal("collected")
		}
		if st, _, _ := e.release(writer, collected, t04Key("tpp-rs-col"), "restock", "COLLECTED"); st == 200 {
			t.Error("restock of a COLLECTED order must be refused")
		}
	})

	t.Run("two concurrent releases of one order: exactly one", func(t *testing.T) {
		order, _, _ := entered()
		var wg sync.WaitGroup
		codes := make([]int, 2)
		start := make(chan struct{})
		for i := range codes {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				<-start
				codes[i], _, _ = e.release(writer, order, t04Key(fmt.Sprintf("tpp-race-%d", i)), "cancel", "PENDING")
			}(i)
		}
		close(start)
		wg.Wait()
		ok := 0
		for _, c := range codes {
			if c == 200 {
				ok++
			} else if c != 409 {
				t.Errorf("unexpected status %d", c)
			}
		}
		if ok != 1 || len(e.deallocRows(order)) != 1 {
			t.Errorf("concurrent cancels: statuses %v, %d DEALLOCATE rows (want one 200 and one row set)", codes, len(e.deallocRows(order)))
		}
	})

	t.Run("direct ledger writes are refused; guards hold", func(t *testing.T) {
		order, _, res := entered()
		var owner, session, wh string
		var qty int64
		if err := f.owner.QueryRow(ctx, `SELECT o.owner_id::text,o.creator_session_id::text,l.warehouse_id::text,l.quantity FROM checkout.orders o JOIN inventory.reservation_lines l ON l.tenant_id=o.tenant_id AND l.store_id=o.store_id AND l.reservation_id=o.id WHERE o.id=$1`, order).Scan(&owner, &session, &wh, &qty); err != nil {
			t.Fatal(err)
		}
		insert := func(qtyDelta int64, op string) string {
			return fmt.Sprintf(`INSERT INTO inventory.ledger(tenant_id,store_id,warehouse_id,sku_id,kind,delta_allocated,operation,command_key,reservation_id,actor_kind,checkout_id,buyer_owner_id,buyer_session_id,principal_id,reason)
			  VALUES('%s','%s','%s','%s','DEALLOCATE',%d,'%s','%s','%s','MERCHANT','%s','%s','%s','%s','PENDING')`, f.tenantA, f.storeA1, wh, sku, qtyDelta, op, order, res.ReservationID, order, owner, session, writerPrincipal)
		}
		gucs := map[string]string{"app.tenant_id": f.tenantA, "app.store_id": f.storeA1, "app.principal_id": writerPrincipal, "app.buyer_id": owner, "app.buyer_session_id": session}
		asWriter := func(stmt string, g map[string]string) string {
			var st string
			tx, err := f.owner.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback(ctx)
			if _, err := tx.Exec(ctx, `SET LOCAL ROLE commerce_checkout_writer`); err != nil {
				t.Fatalf("set role: %v", err)
			}
			for k, v := range g {
				if _, err := tx.Exec(ctx, `SELECT set_config($1,$2,true)`, k, v); err != nil {
					t.Fatal(err)
				}
			}
			st, _ = tcsSub(ctx, tx, stmt)
			return st
		}
		// commerce_runtime cannot write DEALLOCATE rows at all
		tcsAs(t, f, "commerce_runtime", map[string]string{"app.tenant_id": f.tenantA, "app.store_id": f.storeA1, "app.principal_id": writerPrincipal}, func(ctx context.Context, tx pgx.Tx) {
			if st, _ := tcsSub(ctx, tx, insert(-int64(qty), "fulfillment.pay_at_pickup.cancel")); st == "" {
				t.Error("commerce_runtime inserted a DEALLOCATE row directly")
			}
			if st, _ := tcsSub(ctx, tx, fmt.Sprintf(`INSERT INTO inventory.ledger(tenant_id,store_id,warehouse_id,sku_id,kind,delta_on_hand,operation,command_key,actor_kind,principal_id,checkout_id,reason)
			  VALUES('%s','%s','%s','%s','ADJUST',1,'x','k','MERCHANT','%s','%s','r')`, f.tenantA, f.storeA1, wh, sku, writerPrincipal, order)); st == "" {
				t.Error("a MERCHANT non-DEALLOCATE row carrying checkout_id must stay refused (0013)")
			}
		})
		// the checkout writer without the order state (PENDING): refused by inventory.guard_pay_at_pickup_ledger
		if st := asWriter(insert(-int64(qty), "fulfillment.pay_at_pickup.cancel"), gucs); st != "42501" {
			t.Errorf("DEALLOCATE while the order is PENDING: want 42501, got %q", st)
		}
		// plant CANCELLED without ledger rows (disclosed owner-pool fixture; triggers off for these two statements) to isolate the other guards
		ptx, err := f.owner.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		mustExecTx := func(q string) {
			if _, err := ptx.Exec(ctx, q, order); err != nil {
				ptx.Rollback(ctx)
				t.Fatalf("plant: %v", err)
			}
		}
		if _, err := ptx.Exec(ctx, `SET LOCAL session_replication_role=replica`); err != nil {
			t.Fatal(err)
		}
		mustExecTx(`UPDATE checkout.orders SET collection_state='CANCELLED' WHERE id=$1`)
		mustExecTx(`UPDATE inventory.reservations SET state='RELEASED' WHERE id=$1`)
		defer ptx.Rollback(ctx)
		// (the planted state lives only in ptx; run the guard checks inside it through the same connection)
		set := func(g map[string]string) {
			for k, v := range g {
				if _, err := ptx.Exec(ctx, `SELECT set_config($1,$2,true)`, k, v); err != nil {
					t.Fatal(err)
				}
			}
		}
		try := func(stmt string, g map[string]string) string {
			sp, _ := ptx.Begin(ctx)
			if _, err := sp.Exec(ctx, `SET LOCAL session_replication_role=origin`); err != nil {
				t.Fatal(err)
			}
			if _, err := sp.Exec(ctx, `SET LOCAL ROLE commerce_checkout_writer`); err != nil {
				sp.Rollback(ctx)
				t.Fatalf("set role: %v", err)
			}
			for k, v := range g {
				_, _ = sp.Exec(ctx, `SELECT set_config($1,$2,true)`, k, v)
			}
			_, err := sp.Exec(ctx, stmt)
			sp.Rollback(ctx)
			return sqlState(err)
		}
		_ = set
		other := randomUUID()
		bad := map[string]string{"app.tenant_id": f.tenantA, "app.store_id": f.storeA1, "app.principal_id": other, "app.buyer_id": owner, "app.buyer_session_id": session}
		if st := try(insert(-int64(qty), "fulfillment.pay_at_pickup.cancel"), bad); st != "42501" {
			t.Errorf("MERCHANT DEALLOCATE whose principal differs from app.principal_id: want 42501, got %q", st)
		}
		badBuyer := map[string]string{"app.tenant_id": f.tenantA, "app.store_id": f.storeA1, "app.principal_id": writerPrincipal, "app.buyer_id": other, "app.buyer_session_id": session}
		if st := try(insert(-int64(qty), "fulfillment.pay_at_pickup.cancel"), badBuyer); st != "42501" {
			t.Errorf("MERCHANT DEALLOCATE whose buyer GUC differs: want 42501, got %q", st)
		}
		if st := try(insert(-int64(qty)+1, "fulfillment.pay_at_pickup.cancel"), gucs); st != "42501" {
			t.Errorf("DEALLOCATE with a wrong quantity: want 42501, got %q", st)
		}
	})

	t.Run("ECPay attempts: in flight, CREATED, 2098 re-delivery, ABANDONED late report, 3020 restock", func(t *testing.T) {
		e.connect("C2C")
		api, _, _ := e.service("cvs_711", "API", 0)
		apiFami, _, _ := e.service("cvs_familymart", "API", 0)
		endpoint := e.endpointID()
		place := func(kind, code string) string {
			order, _ := e.cvsOrder(tcvOrderSpec{kind: kind, code: code, paymentMode: "pay_at_pickup"})
			return order
		}
		created := func(order string) {
			if st, _, raw := e.ship(e.token(), order, 0, "", true); st != 202 {
				t.Fatalf("request shipment: %d %s", st, raw)
			}
			e.awaitShip(order, "CREATED")
		}
		mustRelease := func(order, action, expected string) (int, map[string]any, []byte) {
			return e.release(writer, order, t04Key("tpp-e-"+action), action, expected)
		}
		// REQUESTED (the job is not routed to any worker): may already be at ECPay
		o1 := place("cvs_711", api)
		if st, _, raw := e.ship(e.token(), o1, 0, "", false); st != 202 {
			t.Fatalf("request: %d %s", st, raw)
		}
		if st, _, raw := mustRelease(o1, "cancel", "PENDING"); st != 409 || tcvStr(tcvJSON(t, raw), "code") != "cvs_attempt_in_flight" {
			t.Errorf("REQUESTED attempt: want 409 cvs_attempt_in_flight, got %d %s", st, raw)
		}
		if n := len(e.deallocRows(o1)); n != 0 || e.collectionState(o1) != "PENDING" {
			t.Errorf("a refused cancel changed the order (%d DEALLOCATE rows, state %s)", n, e.collectionState(o1))
		}
		// UNKNOWN (403 rate limit at ECPay: nothing recorded there)
		e.fake.SetCreateMode(ecpaytest.Create403, "")
		o2 := place("cvs_711", api)
		if st, _, raw := e.ship(e.token(), o2, 0, "", true); st != 202 {
			t.Fatalf("request: %d %s", st, raw)
		}
		e.awaitShip(o2, "UNKNOWN")
		e.fake.SetCreateMode(ecpaytest.CreateOK, "")
		if st, _, raw := mustRelease(o2, "cancel", "PENDING"); st != 409 || tcvStr(tcvJSON(t, raw), "code") != "cvs_attempt_in_flight" {
			t.Errorf("UNKNOWN attempt: want 409 cvs_attempt_in_flight, got %d %s", st, raw)
		}
		// the merchant abandons it (acknowledgement; ECPay shows nothing): ABANDONED counts as not handed over, so the cancel now works
		tn := e.tradeNo(o2)
		if st, _, raw := e.abandon(o2); st != 200 {
			t.Fatalf("abandon: %d %s", st, raw)
		}
		if st, _, raw := mustRelease(o2, "cancel", "PENDING"); st != 200 {
			t.Fatalf("cancel after abandon: %d %s", st, raw)
		}
		ledger := e.count(`SELECT count(*) FROM inventory.ledger WHERE reservation_id=$1`, o2)
		late := ecpaytest.StatusFields(ecpaytest.Trade{MerchantID: e.mk.ID, TradeNo: tn, LogisticsID: "8000001", SubType: "UNIMARTC2C", GoodsAmount: "25", PaymentNo: "1"}, "2030", "2026/09/30 10:00:00", "SENTINELRECIPIENT")
		if w := e.postStatus(endpoint, late); w.Code != 200 || w.Body.String() != "1|OK" {
			t.Errorf("a late report for the abandoned attempt of a CANCELLED order: %d %q (always ACK)", w.Code, w.Body.String())
		}
		if st, _, _ := e.shipState(o2); st != "ABANDONED" || e.collectionState(o2) != "CANCELLED" || e.count(`SELECT count(*) FROM inventory.ledger WHERE reservation_id=$1`, o2) != ledger {
			t.Errorf("late report changed state %s / collection %s / ledger", st, e.collectionState(o2))
		}
		if e.events(o2, "AND event_code LIKE '%duplicate_label_risk'") < 1 {
			t.Error("a late report for an ABANDONED attempt must raise duplicate_label_risk")
		}
		// CREATED: handed to the provider
		o3 := place("cvs_711", api)
		created(o3)
		if st, _, raw := mustRelease(o3, "cancel", "PENDING"); st != 422 || tcvStr(tcvJSON(t, raw), "code") != "not_cancellable" {
			t.Errorf("CREATED shipment: want 422 not_cancellable, got %d %s", st, raw)
		}
		// 2074 -> 2098 -> restock refused; 2074 again -> restock allowed; a later 2067 conflicts with the merchant's record
		o4 := place("cvs_711", api)
		created(o4)
		e.tppStatuses(endpoint, o4, "2030", "2073", "2074")
		if e.collectionState(o4) != "RETURNED" {
			t.Fatalf("2074 must set RETURNED, got %s", e.collectionState(o4))
		}
		e.tppStatuses(endpoint, o4, "2098")
		if st, _, _ := e.shipState(o4); st != "AT_STORE" {
			t.Errorf("2098 re-delivery to the pickup store: state %s, want AT_STORE", st)
		}
		if st, _, raw := mustRelease(o4, "restock", "RETURNED"); st != 409 || tcvStr(tcvJSON(t, raw), "code") != "parcel_not_returned" {
			t.Errorf("restock while the parcel is back at the store: want 409 parcel_not_returned, got %d %s", st, raw)
		}
		e.tppStatuses(endpoint, o4, "2074")
		if st, out, raw := mustRelease(o4, "restock", "RETURNED"); st != 200 || tcvStr(out, "collection_state") != "RESTOCKED" {
			t.Fatalf("restock after 2074: %d %s", st, raw)
		}
		e.tppStatuses(endpoint, o4, "2067")
		if e.collectionState(o4) != "RESTOCKED" || e.events(o4, "AND event_code LIKE '%collection_conflict'") < 1 {
			t.Errorf("a signed 2067 on a RESTOCKED order: collection %s, conflict events %d (want RESTOCKED + collection_conflict)", e.collectionState(o4), e.events(o4, "AND event_code LIKE '%collection_conflict'"))
		}
		// FamilyMart 3020 -> RETURNED -> restock
		o5 := place("cvs_familymart", apiFami)
		created(o5)
		e.tppStatuses(endpoint, o5, "3024", "3018", "3020")
		if st, out, raw := mustRelease(o5, "restock", "RETURNED"); st != 200 || tcvStr(out, "collection_state") != "RESTOCKED" {
			t.Errorf("restock after 3020: %d %s", st, raw)
		}
	})
}

// payHoldFor places a card order for the buyer on the harness home service (paid through the real capture path) and returns its id.
func (e *tcvEnv) payHoldFor(t *testing.T, b *tcvBuyer) string {
	t.Helper()
	res, err := e.svc.Begin(context.Background(), b.cap.Token, e.store(), t04Key("tpp-card"), b.h.input)
	if err != nil {
		t.Fatalf("card Begin: %v", err)
	}
	return e.payHold(res, b).order
}
