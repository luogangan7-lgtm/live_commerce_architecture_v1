package foundation_test

// TCV14 TestCvsBuyerEnteredStore (contracts/taiwan-cvs-logistics-v1.md §10 TCV14 REAL_PG half, §16.1, §5.1). Prefix `tce`. Tier REAL_PG + HTTP_PG.
// Routes/definers: POST /v1/buyer/cvs-stores (fulfillment.record_buyer_cvs_store), GET /v1/buyer/checkout-options (fulfillment.read_cvs_offer),
// checkout.begin_hold (cvs_source_mismatch), POST .../orders/{id}/cvs-shipment (request_cvs_shipment, no_cvs_destination), merchant order
// projection (pickup_source). The browser half of TCV14 is TCV08 (tests/admin/taiwan-cvs.spec.ts).
// Owner-pool writes (disclosed): identity grants (tcvEnv.grantCreator), nothing else.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"livecommerce/internal/buyer"
)

// tceOptions lists the checkout-options rows keyed by delivery kind.
func tceOptions(t *testing.T, b *tcvBuyer) map[string]map[string]any {
	t.Helper()
	r := b.req("GET", "/v1/buyer/checkout-options?market_id="+b.e.p.market.ID+"&country=TW", "", nil, nil)
	if r.status != 200 {
		t.Fatalf("checkout-options: %d %s", r.status, r.body)
	}
	var page struct {
		Items []map[string]any `json:"items"`
	}
	if err := json.Unmarshal(r.body, &page); err != nil {
		t.Fatalf("options JSON: %v %s", err, r.body)
	}
	out := map[string]map[string]any{}
	for _, it := range page.Items {
		out[tcvStr(it, "delivery_kind")] = it
	}
	return out
}

func TestCvsBuyerEnteredStore(t *testing.T) {
	e := tcvNew(t)
	f := e.p.f
	ctx := context.Background()
	kinds := []string{"cvs_711", "cvs_familymart", "cvs_hilife", "cvs_okmart"}
	svc := map[string]string{}
	for _, k := range kinds {
		svc[k], _, _ = e.service(k, "MANUAL", 0)
	}

	t.Run("options: buyer_entered mode with the four official search links", func(t *testing.T) {
		b := e.newBuyer()
		opts := tceOptions(t, b)
		want := map[string][]string{
			"cvs_711":        {"https://emap.pcsc.com.tw/"},
			"cvs_familymart": {"https://www.family.com.tw/Marketing/zh/Map", "https://family.map.com.tw/famiport/storeNumberFreeze.aspx"},
			"cvs_hilife":     {"https://www.hilife.com.tw/storeInquiry_street.aspx"},
			"cvs_okmart":     {"https://www.okmart.com.tw/convenient_shopSearch"},
		}
		for _, k := range kinds {
			row := opts[k]
			if row == nil {
				t.Errorf("no options row for %s", k)
				continue
			}
			if tcvStr(row, "pickup_selection") != "buyer_entered" {
				t.Errorf("%s pickup_selection=%q want buyer_entered (store has no ECPay profile)", k, tcvStr(row, "pickup_selection"))
			}
			ok := false
			for _, u := range want[k] {
				ok = ok || tcvStr(row, "store_search_url") == u
			}
			if !ok {
				t.Errorf("%s store_search_url=%q, want one of %v", k, tcvStr(row, "store_search_url"), want[k])
			}
			if av, present := row["available"]; present && av == false {
				t.Errorf("%s must be available in buyer_entered mode (ECPay chain gates do not apply): %v", k, row)
			}
		}
	})

	t.Run("per-chain store-code table (shared golden), leading zeros kept", func(t *testing.T) {
		raw, err := os.ReadFile("../integrations/ecpay/testdata/store_code.json")
		if err != nil {
			t.Fatal(err)
		}
		var rows []struct {
			Kind string `json:"kind"`
			Code string `json:"code"`
			OK   bool   `json:"ok"`
		}
		if err := json.Unmarshal(raw, &rows); err != nil {
			t.Fatal(err)
		}
		b := e.newBuyer()
		for _, r := range rows {
			code, known := svc[r.Kind]
			if !known {
				continue // bogus kind: no service exists; covered as service_unavailable below
			}
			res := b.enteredStore(code, r.Code, "測試門市", "台北市測試路1號")
			if r.OK {
				if res.status != 201 {
					t.Errorf("%s code %q: want 201, got %d %s", r.Kind, r.Code, res.status, res.body)
					continue
				}
				out := tcvJSON(t, res.body)
				if tcvStr(out, "code") != r.Code || tcvStr(out, "kind") != r.Kind || tcvStr(out, "source") != "buyer_entered" || tcvStr(out, "pickup_id") == "" {
					t.Errorf("%s %q projection: %v", r.Kind, r.Code, out)
				}
			} else if res.status != 422 || tcvStr(tcvJSON(t, res.body), "code") != "bad_store_code" {
				t.Errorf("%s code %q: want 422 bad_store_code, got %d %s", r.Kind, r.Code, res.status, res.body)
			}
		}
	})

	t.Run("SQL twin of the store-code table: the definer itself checks the format (Go validation bypassed)", func(t *testing.T) {
		raw, err := os.ReadFile("../integrations/ecpay/testdata/store_code.json")
		if err != nil {
			t.Fatal(err)
		}
		var rows []struct {
			Kind string `json:"kind"`
			Code string `json:"code"`
			OK   bool   `json:"ok"`
		}
		if err := json.Unmarshal(raw, &rows); err != nil {
			t.Fatal(err)
		}
		b := e.newBuyer()
		tokenHash := sha256.Sum256([]byte(b.cap.Token))
		for _, r := range rows {
			code, known := svc[r.Kind]
			if !known {
				continue
			}
			var sqlState string
			// buyer.WithScope on the checkout pool: exactly how BuyerCVS.EnterStore reaches the definer, minus its Go pre-validation
			err := buyer.WithScope(ctx, e.p.pool, b.cap.Token, e.store(), func(callCtx context.Context, tx pgx.Tx, _ buyer.Scope) error {
				var out []byte
				return tx.QueryRow(callCtx, `SELECT fulfillment.record_buyer_cvs_store($1,$2::uuid,$3,$4,$5::bigint,$6::uuid,$7,$8,$9,$10)`,
					tokenHash[:], e.store(), "tce-sql-"+t04Tag(), randomBytes(32), b.cartVersion(), e.p.market.ID, code, r.Code, "測試門市", "台北市測試路1號").Scan(&out)
			})
			sqlState = tcsErrMessage(err)
			if r.OK && sqlState != "" {
				t.Errorf("%s %q must be accepted by the definer, got %q", r.Kind, r.Code, sqlState)
			}
			if !r.OK && sqlState != "bad_store_code" {
				t.Errorf("%s %q must be refused by the definer with bad_store_code, got %q", r.Kind, r.Code, sqlState)
			}
		}
	})

	t.Run("name and address bounds, control characters, trimming", func(t *testing.T) {
		b := e.newBuyer()
		s711 := svc["cvs_711"]
		long := func(n int) string { return strings.Repeat("店", n) }
		cases := []struct {
			label, name, addr string
			status            int
			code              string
		}{
			{"name 1 char", "店", "台北市測試路1號", 201, ""},
			{"name 40 chars", long(40), "台北市測試路1號", 201, ""},
			{"name 41 chars", long(41), "台北市測試路1號", 422, "bad_store_name"},
			{"name empty", "", "台北市測試路1號", 422, "bad_store_name"},
			{"name blank", "   ", "台北市測試路1號", 422, "bad_store_name"},
			{"name control char", "店\x07店", "台北市測試路1號", 422, "bad_store_name"},
			{"name newline", "店\n店", "台北市測試路1號", 422, "bad_store_name"},
			{"address 5 chars", "測試門市", "台北市測試", 201, ""},
			{"address 4 chars", "測試門市", "台北市測", 422, "bad_store_address"},
			{"address 120 chars", "測試門市", long(120), 201, ""},
			{"address 121 chars", "測試門市", long(121), 422, "bad_store_address"},
			{"address control char", "測試門市", "台北市\x00測試路1號", 422, "bad_store_address"},
			{"address tab", "測試門市", "台北市\t測試路1號", 422, "bad_store_address"},
		}
		for _, c := range cases {
			res := b.enteredStore(s711, "123456", c.name, c.addr)
			if res.status != c.status || (c.code != "" && tcvStr(tcvJSON(t, res.body), "code") != c.code) {
				t.Errorf("%s: want %d %s, got %d %s", c.label, c.status, c.code, res.status, res.body)
			}
		}
		// trimmed: surrounding blanks are not part of the stored value
		// Contract 16.1 says "trimmed, 1..40 chars": either reading is safe (normalise then store, or refuse an untrimmed value), but a
		// value with surrounding blanks must never be stored. Recorded as an ambiguity in NOT_RUN.md.
		res := b.enteredStore(s711, "123456", "  修剪門市  ", "  台北市修剪路9號  ")
		switch res.status {
		case 201:
			out := tcvJSON(t, res.body)
			if tcvStr(out, "name") != "修剪門市" || tcvStr(out, "address") != "台北市修剪路9號" {
				t.Errorf("name/address must be stored trimmed: %v", out)
			}
		case 422:
			if c := tcvStr(tcvJSON(t, res.body), "code"); c != "bad_store_name" && c != "bad_store_address" {
				t.Errorf("an untrimmed value refused with an unexpected code: %s", res.body)
			}
		default:
			t.Fatalf("trim case: %d %s", res.status, res.body)
		}
		if n := e.count(`SELECT count(*) FROM fulfillment.pickup_versions WHERE namespace=$1 AND (name<>btrim(name) OR address<>btrim(address))`, "buyer."+b.cap.Scope.OwnerID); n != 0 {
			t.Errorf("%d stored versions carry surrounding blanks", n)
		}
	})

	t.Run("stored version: BUYER_ENTERED, NULL principal, buyer namespace, 24 h, reuse vs new version", func(t *testing.T) {
		b := e.newBuyer()
		s711 := svc["cvs_711"]
		key := t04Key("tce-version")
		first := b.enteredStoreKey(key, s711, "123456", "版本門市", "台北市版本路1號")
		if first.status != 201 {
			t.Fatalf("first: %d %s", first.status, first.body)
		}
		id1 := tcvStr(tcvJSON(t, first.body), "pickup_id")
		var vk, ns, evidence string
		var principalNull bool
		var version int64
		var attested, valid time.Time
		if err := f.owner.QueryRow(ctx, `SELECT verification_kind,namespace,evidence_ref,principal_id IS NULL,version,attested_at,valid_until FROM fulfillment.pickup_versions WHERE id=$1`, id1).
			Scan(&vk, &ns, &evidence, &principalNull, &version, &attested, &valid); err != nil {
			t.Fatal(err)
		}
		if vk != "BUYER_ENTERED" || !principalNull || ns != "buyer."+b.cap.Scope.OwnerID || version != 1 || valid.Sub(attested) != 24*time.Hour {
			t.Errorf("version row: kind=%s principalNULL=%v ns=%s version=%d validity=%v", vk, principalNull, ns, version, valid.Sub(attested))
		}
		if !strings.HasPrefix(evidence, "buyer-entry:") || len(strings.TrimPrefix(evidence, "buyer-entry:")) != 64 {
			t.Errorf("evidence_ref %q must be buyer-entry:<hex request hash>", evidence)
		}
		if _, err := hex.DecodeString(strings.TrimPrefix(evidence, "buyer-entry:")); err != nil {
			t.Errorf("evidence_ref hash not hex: %v", err)
		}
		// same key + same body replays the same answer and writes nothing
		versions := e.count(`SELECT count(*) FROM fulfillment.pickup_versions WHERE namespace=$1`, ns)
		if replay := b.enteredStoreKey(key, s711, "123456", "版本門市", "台北市版本路1號"); replay.status != 201 || string(replay.body) != string(first.body) {
			t.Errorf("replay: %d %s", replay.status, replay.body)
		}
		// same key + other body: conflict
		if other := b.enteredStoreKey(key, s711, "123456", "別的門市", "台北市版本路1號"); other.status != 409 {
			t.Errorf("same key, other body: want 409, got %d %s", other.status, other.body)
		}
		if got := e.count(`SELECT count(*) FROM fulfillment.pickup_versions WHERE namespace=$1`, ns); got != versions {
			t.Errorf("replay/conflict wrote versions: %d -> %d", versions, got)
		}
		// same store + same name/address under a NEW key reuses the head's current version (valid_until > now+1h)
		again := b.enteredStore(s711, "123456", "版本門市", "台北市版本路1號")
		if again.status != 201 || tcvStr(tcvJSON(t, again.body), "pickup_id") != id1 {
			t.Errorf("unchanged entry must reuse the current version: %d %s (want pickup %s)", again.status, again.body, id1)
		}
		// a changed name appends version 2 with head CAS
		changed := b.enteredStore(s711, "123456", "改名門市", "台北市版本路1號")
		if changed.status != 201 {
			t.Fatalf("changed: %d %s", changed.status, changed.body)
		}
		if id2 := tcvStr(tcvJSON(t, changed.body), "pickup_id"); id2 == id1 {
			t.Error("a changed name must append a new version")
		}
		var head int64
		if err := f.owner.QueryRow(ctx, `SELECT current_version FROM fulfillment.pickup_heads WHERE namespace=$1 AND code='123456' AND kind='cvs_711'`, ns).Scan(&head); err != nil || head != 2 {
			t.Errorf("head after a changed entry: version %d err %v, want 2", head, err)
		}
		// an entry about to expire (<1 h left) is not reused: age the head's version (owner-pool fixture, disclosed: only valid_until moves)
		mustExec(t, f.owner, `UPDATE fulfillment.pickup_versions SET attested_at=attested_at-interval '23 hours 30 minutes',valid_until=valid_until-interval '23 hours 30 minutes' WHERE namespace=$1 AND code='123456' AND version=2`, ns)
		aged := b.enteredStore(s711, "123456", "改名門市", "台北市版本路1號")
		if aged.status != 201 || tcvStr(tcvJSON(t, aged.body), "pickup_id") == tcvStr(tcvJSON(t, changed.body), "pickup_id") {
			t.Errorf("a version with <1 h validity left must not be reused: %d %s", aged.status, aged.body)
		}
	})

	t.Run("request guards: stale cart, unknown service, rate limit, idempotency header", func(t *testing.T) {
		b := e.newBuyer()
		s711 := svc["cvs_711"]
		stale := b.req("POST", "/v1/buyer/cvs-stores", t04Key("tce-stale"), map[string]any{"cart_version": b.cartVersion() + 5, "market_id": e.p.market.ID, "service_code": s711,
			"store_code": "123456", "store_name": "門市", "store_address": "台北市測試路1號"}, nil)
		if stale.status != 409 {
			t.Errorf("stale cart_version: want 409, got %d %s", stale.status, stale.body)
		}
		if res := b.enteredStore("nosuchservice", "123456", "門市", "台北市測試路1號"); res.status != 422 || tcvStr(tcvJSON(t, res.body), "code") != "service_unavailable" {
			t.Errorf("unknown service: want 422 service_unavailable, got %d %s", res.status, res.body)
		}
		if res := b.req("POST", "/v1/buyer/cvs-stores", "", map[string]any{"cart_version": b.cartVersion(), "market_id": e.p.market.ID, "service_code": s711,
			"store_code": "123456", "store_name": "門市", "store_address": "台北市測試路1號"}, nil); res.status < 400 {
			t.Errorf("a missing Idempotency-Key must be refused, got %d", res.status)
		}
		// exact body: an extra field is refused
		if res := b.req("POST", "/v1/buyer/cvs-stores", t04Key("tce-extra"), map[string]any{"cart_version": b.cartVersion(), "market_id": e.p.market.ID, "service_code": s711,
			"store_code": "123456", "store_name": "門市", "store_address": "台北市測試路1號", "recipient": "x"}, nil); res.status < 400 {
			t.Errorf("an unknown body field must be refused, got %d", res.status)
		}
	})

	t.Run("rate limit: 20 entries per owner per hour, then 429 with Retry-After (§16.1)", func(t *testing.T) {
		s711 := svc["cvs_711"]
		rl := e.newBuyer()
		var last bhResponse
		accepted := 0
		for i := 0; i < 24; i++ {
			last = rl.enteredStore(s711, "123456", fmt.Sprintf("限流門市%d", i), "台北市限流路1號")
			if last.status == 201 {
				accepted++
			} else {
				break
			}
		}
		if accepted != 20 || last.status != 429 {
			t.Errorf("rate limit: %d accepted then %d %s (want exactly 20 accepted, then 429)", accepted, last.status, last.body)
		}
		if last.status == 429 && last.header.Get("Retry-After") == "" {
			t.Error("a 429 must carry Retry-After")
		}
	})

	t.Run("buyer B cannot read or bump buyer A's head (both buyer roles)", func(t *testing.T) {
		a, other := e.newBuyer(), e.newBuyer()
		res := a.enteredStore(svc["cvs_711"], "654321", "甲的門市", "台北市甲路1號")
		if res.status != 201 {
			t.Fatalf("A: %d %s", res.status, res.body)
		}
		pickupA := tcvStr(tcvJSON(t, res.body), "pickup_id")
		nsA := "buyer." + a.cap.Scope.OwnerID
		for _, role := range []string{"commerce_buyer_runtime", "commerce_checkout_runtime"} {
			tcsAs(t, f, role, map[string]string{"app.tenant_id": f.tenantA, "app.store_id": f.storeA1, "app.buyer_id": other.cap.Scope.OwnerID, "app.buyer_session_id": other.cap.Scope.SessionID}, func(ctx context.Context, tx pgx.Tx) {
				var n int
				if err := tx.QueryRow(ctx, `SELECT count(*) FROM fulfillment.pickup_versions WHERE id=$1 OR namespace=$2`, pickupA, nsA).Scan(&n); err != nil || n != 0 {
					t.Errorf("%s as buyer B reads %d of buyer A's versions (err %v)", role, n, err)
				}
				if n, _ := func() (int64, string) {
					st, n := tcsSub(ctx, tx, `UPDATE fulfillment.pickup_heads SET current_version=current_version WHERE namespace=$1`, nsA)
					return n, st
				}(); n != 0 {
					t.Errorf("%s as buyer B updated %d of buyer A's heads", role, n)
				}
			})
		}
		// B's own entry of the same store is its own namespace and version
		resB := other.enteredStore(svc["cvs_711"], "654321", "甲的門市", "台北市甲路1號")
		if resB.status != 201 || tcvStr(tcvJSON(t, resB.body), "pickup_id") == pickupA {
			t.Errorf("B must get its own pickup row: %d %s", resB.status, resB.body)
		}
		if got := e.count(`SELECT count(DISTINCT namespace) FROM fulfillment.pickup_versions WHERE code='654321' AND namespace LIKE 'buyer.%'`); got < 2 {
			t.Errorf("two buyers, %d buyer namespaces", got)
		}
	})

	t.Run("pay-at-pickup order carries the buyer_entered label; never reused as verified", func(t *testing.T) {
		e.grantCreator("orders:read", "fulfillment:write", "integration:manage")
		// settings: pay-at-pickup on (TWD 20000 cap)
		status, _, raw := e.mcall(e.token(), "PUT", "/v1/admin/stores/"+e.store()+"/logistics/cvs-settings", t04Key("tce-settings"),
			`{"expected_version":0,"enabled_chains":["cvs_711","cvs_familymart","cvs_hilife","cvs_okmart"],"pay_at_pickup_enabled":true,"pay_at_pickup_max_twd":20000,"pay_at_pickup_max_open":20}`)
		if status != 200 {
			t.Fatalf("settings: %d %s", status, raw)
		}
		place := func(b *tcvBuyer) (orderID string, err error) {
			res := b.enteredStore(svc["cvs_711"], "123456", "標籤門市", "台北市標籤路1號")
			if res.status != 201 {
				t.Fatalf("enter store: %d %s", res.status, res.body)
			}
			dest, err := b.destination("cvs_711", tcvStr(tcvJSON(t, res.body), "pickup_id"), "王小明", "0912345678")
			if err != nil {
				t.Fatalf("destination: %v", err)
			}
			quote, err := b.quote(svc["cvs_711"])
			if err != nil {
				t.Fatalf("quote: %v", err)
			}
			hold, err := b.begin(dest, quote, 1, "pay_at_pickup")
			return hold.OrderID, err
		}
		firstBuyer, secondBuyer := e.newBuyer(), e.newBuyer()
		// second buyer enters its store BEFORE the ECPay connection exists (so its pickup is BUYER_ENTERED)
		res := secondBuyer.enteredStore(svc["cvs_711"], "123456", "標籤門市", "台北市標籤路1號")
		if res.status != 201 {
			t.Fatalf("second buyer store: %d %s", res.status, res.body)
		}
		secondPickup := tcvStr(tcvJSON(t, res.body), "pickup_id")
		order, err := place(firstBuyer)
		if err != nil {
			t.Fatalf("pay-at-pickup Begin with a buyer-entered store (no profile): %v", err)
		}
		st, detail, raw := e.mcall(e.token(), "GET", "/v1/admin/stores/"+e.store()+"/orders/"+order, "", "")
		if st != 200 || tcvStr(detail, "pickup_source") != "buyer_entered" {
			t.Errorf("merchant order detail must carry pickup_source=buyer_entered: %d %s", st, raw)
		}
		// connect ECPay: the store becomes ecpay_map
		e.connect("C2C")
		// (a) request_cvs_shipment for the buyer-entered order: no_cvs_destination
		st, _, raw = e.mcall(e.token(), "POST", "/v1/admin/stores/"+e.store()+"/orders/"+order+"/cvs-shipment", t04Key("tce-ship"), `{"expected_version":0}`)
		if st != 422 || tcvStr(tcvJSON(t, raw), "code") != "no_cvs_destination" {
			t.Errorf("request_cvs_shipment on a BUYER_ENTERED pickup: want 422 no_cvs_destination, got %d %s", st, raw)
		}
		if n := e.count(`SELECT count(*) FROM fulfillment.cvs_shipments WHERE order_id=$1`, order); n != 0 {
			t.Errorf("%d shipment rows for a refused request", n)
		}
		// (b) the second buyer's BUYER_ENTERED pickup cannot be Begun while the store has an enabled qualified profile
		if _, err := secondBuyer.destination("cvs_711", secondPickup, "王小明", "0912345678"); err != nil {
			t.Logf("destination with a buyer-entered pickup while ecpay_map: %v", err)
		}
		holdsBefore := e.count(`SELECT count(*) FROM inventory.reservations WHERE buyer_owner_id=$1`, secondBuyer.cap.Scope.OwnerID)
		dest, derr := secondBuyer.destination("cvs_711", secondPickup, "王小明", "0912345678")
		if derr == nil {
			quote, qerr := secondBuyer.quote(svc["cvs_711"])
			if qerr != nil {
				t.Fatalf("quote: %v", qerr)
			}
			_, berr := secondBuyer.begin(dest, quote, 1, "pay_at_pickup")
			tcvExpectRefusal(t, "Begin with BUYER_ENTERED while the store has an enabled qualified profile", berr, 422, "cvs_source_mismatch")
		}
		if got := e.count(`SELECT count(*) FROM inventory.reservations WHERE buyer_owner_id=$1`, secondBuyer.cap.Scope.OwnerID); got != holdsBefore {
			t.Errorf("a refused Begin left %d holds", got-holdsBefore)
		}
		// (c) record_buyer_cvs_store refuses once the store is ecpay_map
		late := e.newBuyer().enteredStore(svc["cvs_711"], "123456", "晚到門市", "台北市晚到路1號")
		if late.status != 422 || tcvStr(tcvJSON(t, late.body), "code") != "service_unavailable" {
			t.Errorf("entering a store while the store is ecpay_map: want 422 service_unavailable, got %d %s", late.status, late.body)
		}
	})
}

// tcsErrMessage returns the message of a PgError raised by a definer ("" for nil), or the error text otherwise.
func tcsErrMessage(err error) string {
	if err == nil {
		return ""
	}
	var pg *pgconn.PgError
	if errors.As(err, &pg) {
		return pg.Message
	}
	return err.Error()
}
