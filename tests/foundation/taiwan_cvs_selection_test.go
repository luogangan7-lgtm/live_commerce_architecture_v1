package foundation_test

// TCV03 TestCvsSelectionFlow (contracts/taiwan-cvs-logistics-v1.md §10 TCV03, §4.2 pickup rows, §4.3 open/record_cvs_map_return/verify_cvs_selection/
// read_cvs_selection, §5.2 HTTP incl. the 303, §7.2 directory, F3/F17/X10). Prefix `tcl`. Tier REAL_PG + HTTP_PG (MOCK ECPay: the ecpaytest fake).
// Routes/definers: POST /v1/buyer/cvs-selections, POST /v1/cvs/ecpay/map-return/{id} (hooks handler, no cookie/BFF key), GET|POST
// /v1/buyer/cvs-selections/{id}[/verify]; fulfillment.open_cvs_selection, record_cvs_map_return, verify_cvs_selection, read_cvs_selection.
// Owner-pool writes (disclosed fixtures): aging a selection's created_at/expires_at by 20 minutes (the expiry case), aging a pickup version's
// validity (the not-reused-when-expiring case). Nothing is fabricated: every selection, store and pickup is created through the routes.

import (
	"context"
	"crypto/sha256"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"livecommerce/internal/checkout"
	"livecommerce/internal/integrations/shipping/ecpay/ecpaytest"
)

type tclSel struct {
	ID, ReturnPath string
	Fields         map[string]string
}

func (e *tcvEnv) tclOpen(b *tcvBuyer, code, returnPath string, edit func(*http.Request)) (tclSel, bhResponse) {
	e.t.Helper()
	res := b.req("POST", "/v1/buyer/cvs-selections", t04Key("tcl-sel"), map[string]any{"cart_version": b.cartVersion(), "market_id": e.p.market.ID, "service_code": code, "return_path": returnPath}, edit)
	if res.status != 201 {
		return tclSel{}, res
	}
	out := tcvJSON(e.t, res.body)
	return tclSel{ID: tcvStr(out, "selection_id"), ReturnPath: returnPath, Fields: tcvFormFields(out)}, res
}

func (e *tcvEnv) tclRow(id string) (state string, reject, returned *string, version int64) {
	e.t.Helper()
	if err := e.p.f.owner.QueryRow(context.Background(), `SELECT state,reject_code,returned_store_id,version FROM fulfillment.cvs_selections WHERE id=$1`, id).Scan(&state, &reject, &returned, &version); err != nil {
		e.t.Fatalf("selection row %s: %v", id, err)
	}
	return
}

func (e *tcvEnv) tclFinal(b *tcvBuyer, id string) map[string]any {
	e.t.Helper()
	st, view := e.verifySel(b, id)
	if st != 200 {
		e.t.Fatalf("verify %s: %d %v", id, st, view)
	}
	return view
}

func TestCvsSelectionFlow(t *testing.T) {
	e := tcvNew(t)
	f := e.p.f
	ctx := context.Background()
	merchantID := e.connect("C2C")
	code, _, _ := e.service("cvs_711", "API", 0)
	const name, phone = "王小明", "0912345678"

	t.Run("open -> return -> verify -> pickup source -> destination -> Begin", func(t *testing.T) {
		b := e.newBuyer()
		sel, res := e.tclOpen(b, code, "/en/products/prod_flow", nil)
		if sel.ID == "" {
			t.Fatalf("open: %d %s", res.status, res.body)
		}
		fields := sel.Fields
		tradeNo := fields["MerchantTradeNo"]
		if fields["MerchantID"] != merchantID || fields["LogisticsType"] != "CVS" || fields["LogisticsSubType"] != "UNIMARTC2C" || fields["IsCollection"] != "N" ||
			fields["ServerReplyURL"] != tcvHooksOrigin+"/v1/cvs/ecpay/map-return/"+sel.ID || len(tradeNo) != 20 || fields["Device"] != "0" {
			t.Errorf("map form fields: %v", fields)
		}
		for _, c := range tradeNo {
			if !(c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c >= '0' && c <= '9') {
				t.Errorf("MerchantTradeNo %q must be 20 alphanumerics", tradeNo)
				break
			}
		}
		if action := tcvStr(tcvJSON(t, res.body), "form", "action"); action != "https://logistics-stage.ecpay.com.tw/Express/map" {
			t.Errorf("form action %q must be the fixed stage map URL", action)
		}
		var digest []byte
		if err := f.owner.QueryRow(ctx, `SELECT nonce_sha256 FROM fulfillment.cvs_selections WHERE id=$1`, sel.ID).Scan(&digest); err != nil {
			t.Fatal(err)
		}
		if want := sha256.Sum256([]byte(tradeNo)); string(digest) != string(want[:]) {
			t.Error("only sha256(MerchantTradeNo) is stored, and it must be the digest of the value handed to the browser")
		}
		if n := e.count(`SELECT count(*) FROM fulfillment.cvs_selections WHERE id=$1 AND state='OPEN'`, sel.ID); n != 1 {
			t.Error("a new selection is OPEN")
		}
		// B20 (Device=1 for mobile user agents) is decided by the buyer BFF, which is the only place a phone's user agent is visible: the private
		// Go route sees the BFF's own user agent. The mobile leg is asserted in the browser gate TCV08 (form field Device on a 390px phone).
		w := e.mapReturn(sel.ID, fields)
		wantLoc := e.origin + "/en/products/prod_flow?cvs_selection=" + sel.ID
		if w.Code != http.StatusSeeOther || w.Header().Get("Location") != wantLoc {
			t.Errorf("map return must 303 to the stored return_origin+return_path: %d %q want %q", w.Code, w.Header().Get("Location"), wantLoc)
		}
		if strings.Contains(w.Header().Get("Location"), "131386") || strings.Contains(w.Body.String(), "131386") || strings.Contains(w.Body.String(), "Stage") {
			t.Error("I11: no store fields in the Location or the body")
		}
		view := e.tclFinal(b, sel.ID)
		p := selPickup(view)
		if tcvStr(view, "state") != "VERIFIED" || p == nil || tcvStr(p, "code") != "131386" || tcvStr(p, "name") != "Stage 7-ELEVEN" || tcvStr(p, "address") != "Stage address 7" || tcvStr(p, "kind") != "cvs_711" {
			t.Fatalf("verify: %v", view)
		}
		var vk, ns, evidence string
		var hours float64
		if err := f.owner.QueryRow(ctx, `SELECT verification_kind,namespace,evidence_ref,extract(epoch FROM valid_until-attested_at)/3600 FROM fulfillment.pickup_versions WHERE id=$1`, selPickupID(view)).Scan(&vk, &ns, &evidence, &hours); err != nil {
			t.Fatal(err)
		}
		if vk != "PROVIDER_DIRECTORY_VERIFIED" || ns != "ecpay.sandbox.unimartc2c" || evidence != "ecpay-dir:"+sel.ID || hours != 24 {
			t.Errorf("pickup row: kind=%s namespace=%s evidence=%s validity=%vh", vk, ns, evidence, hours)
		}
		// destination -> Begin with the verified pickup
		if _, err := e.tcbTry(b, "cvs_711", code, selPickupID(view), name, phone, ""); err != nil {
			t.Errorf("SetDestination + Begin with the verified pickup: %v", err)
		}
	})

	t.Run("return check order: a wrong nonce writes nothing, however wrong the rest is", func(t *testing.T) {
		b := e.newBuyer()
		sel, _ := e.tclOpen(b, code, "/en/products/prod_nonce", nil)
		before, _, _, v0 := e.tclRow(sel.ID)
		if before != "OPEN" {
			t.Fatal("new selection must be OPEN")
		}
		for label, override := range map[string]map[string]string{
			"wrong nonce":                      {"MerchantTradeNo": "AAAAAAAAAAAAAAAAAAAA"},
			"wrong nonce + wrong merchant":     {"MerchantTradeNo": "AAAAAAAAAAAAAAAAAAAA", "MerchantID": "9999999999"},
			"wrong nonce + wrong subtype":      {"MerchantTradeNo": "AAAAAAAAAAAAAAAAAAAA", "LogisticsSubType": "FAMIC2C"},
			"wrong nonce + malformed store id": {"MerchantTradeNo": "AAAAAAAAAAAAAAAAAAAA", "CVSStoreID": "12-34-56-78-90"},
			"empty nonce":                      {"MerchantTradeNo": ""},
			"nonce of another selection's form": {"MerchantTradeNo": func() string {
				o, _ := e.tclOpen(e.newBuyer(), code, "/en/products/prod_x", nil)
				return o.Fields["MerchantTradeNo"]
			}()},
		} {
			w := e.mapReturnWith(sel.ID, sel.Fields, override)
			// 303 is the contract answer for a known selection (no oracle); a 400 for a value the transport itself refuses (empty nonce,
			// malformed store id) also writes nothing, which is what this case protects.
			if w.Code != http.StatusSeeOther && w.Code != http.StatusBadRequest {
				t.Errorf("%s: unexpected answer %d", label, w.Code)
			}
			if state, reject, returned, v := e.tclRow(sel.ID); state != "OPEN" || reject != nil || returned != nil || v != v0 {
				t.Errorf("%s changed the selection: state=%s reject=%v returned=%v version %d->%d (record_cvs_map_return step 2: ignored, no write)", label, state, reject, returned, v0, v)
			}
		}
		// the genuine return still works afterwards: nobody burned the selection
		if w := e.mapReturn(sel.ID, sel.Fields); w.Code != http.StatusSeeOther {
			t.Errorf("genuine return: %d", w.Code)
		}
		if view := e.tclFinal(b, sel.ID); tcvStr(view, "state") != "VERIFIED" {
			t.Errorf("selection after wrong-nonce noise + genuine return: %v", view)
		}
		// a repeat of the accepted return is ignored (no second write)
		_, _, _, v1 := e.tclRow(sel.ID)
		e.mapReturn(sel.ID, sel.Fields)
		if _, _, _, v2 := e.tclRow(sel.ID); v2 != v1 {
			t.Errorf("a repeated return changed the row: version %d -> %d", v1, v2)
		}
	})

	t.Run("rejections: expired, merchant/subtype mismatch, store id length, not in directory; zero pickup rows", func(t *testing.T) {
		pickupRows := func() int {
			return e.count(`SELECT count(*) FROM fulfillment.pickup_versions WHERE namespace LIKE 'ecpay.%' AND code IN ('1234567','123456789','12345','77777','999888')`)
		}
		reject := func(label, wantCode string, prep func(sel tclSel), override map[string]string, mapStore *ecpaytest.Store) {
			b := e.newBuyer()
			sel, res := e.tclOpen(b, code, "/en/products/prod_rej", nil)
			if sel.ID == "" {
				t.Fatalf("%s open: %d %s", label, res.status, res.body)
			}
			if prep != nil {
				prep(sel)
			}
			if mapStore != nil {
				e.fake.SetMapStore("UNIMARTC2C", *mapStore)
				defer e.fake.SetMapStore("UNIMARTC2C", ecpaytest.Store{ID: "131386", Name: "Stage 7-ELEVEN", Addr: "Stage address 7"})
			}
			rows := pickupRows()
			e.mapReturnWith(sel.ID, sel.Fields, override)
			view := e.tclFinal(b, sel.ID)
			state, reject, _, _ := e.tclRow(sel.ID)
			if tcvStr(view, "state") != "REJECTED" || state != "REJECTED" || reject == nil || *reject != wantCode || selPickup(view) != nil {
				t.Errorf("%s: want REJECTED %s, got view %v row %s/%v", label, wantCode, view, state, reject)
			}
			if got := pickupRows(); got != rows {
				t.Errorf("%s: a rejected selection wrote %d pickup row(s)", label, got-rows)
			}
		}
		reject("expired", "expired", func(sel tclSel) {
			// disclosed owner-pool fixture: only the two timestamps move (the DB clock decides expiry)
			mustExec(t, f.owner, `UPDATE fulfillment.cvs_selections SET created_at=created_at-interval '20 minutes',expires_at=expires_at-interval '20 minutes' WHERE id=$1`, sel.ID)
		}, nil, nil)
		reject("merchant mismatch", "merchant_mismatch", nil, map[string]string{"MerchantID": "9999999999"}, nil)
		reject("subtype mismatch", "subtype_mismatch", nil, map[string]string{"LogisticsSubType": "FAMIC2C"}, nil)
		reject("store id with a symbol", "bad_store_id", nil, map[string]string{"CVSStoreID": "12-345"}, nil)
		reject("store id longer than 9 chars", "bad_store_id", nil, map[string]string{"CVSStoreID": "1234567890"}, nil)
		reject("9-char store id (directory row, too long for ReceiverStoreID S(6))", "store_code_length", nil, nil, &ecpaytest.Store{ID: "123456789", Name: "Nine char store", Addr: "Nine address"})
		reject("7-char store id", "store_code_length", nil, nil, &ecpaytest.Store{ID: "1234567", Name: "Seven char store", Addr: "Seven address"})
		reject("5-char store id absent from the directory", "store_not_in_directory", nil, nil, &ecpaytest.Store{ID: "77777", Name: "Not listed", Addr: "Nowhere"})
		reject("directory miss with the stage store name", "store_not_in_directory", nil, nil, &ecpaytest.Store{ID: "999888", Name: "Stage 7-ELEVEN", Addr: "Stage address 7"})
		// a 4-char store id from the directory IS accepted (OK stage store shape, F17: S(6) is a maximum)
		b := e.newBuyer()
		sel, _ := e.tclOpen(b, code, "/en/products/prod_4", nil)
		e.fake.SetMapStore("UNIMARTC2C", ecpaytest.Store{ID: "1328", Name: "Four char store", Addr: "Four address"})
		e.mapReturn(sel.ID, sel.Fields)
		e.fake.SetMapStore("UNIMARTC2C", ecpaytest.Store{ID: "131386", Name: "Stage 7-ELEVEN", Addr: "Stage address 7"})
		if view := e.tclFinal(b, sel.ID); tcvStr(view, "state") != "VERIFIED" || tcvStr(selPickup(view), "code") != "1328" {
			t.Errorf("4-char store id 1328: %v", view)
		}
	})

	t.Run("forged name/address never reach the pickup row; leading zeros preserved", func(t *testing.T) {
		b := e.newBuyer()
		sel, _ := e.tclOpen(b, code, "/en/products/prod_forge", nil)
		e.fake.SetMapStore("UNIMARTC2C", ecpaytest.Store{ID: "000123", Name: "FORGED NAME", Addr: "FORGED ADDRESS 1 ROAD"})
		e.mapReturn(sel.ID, sel.Fields)
		e.fake.SetMapStore("UNIMARTC2C", ecpaytest.Store{ID: "131386", Name: "Stage 7-ELEVEN", Addr: "Stage address 7"})
		view := e.tclFinal(b, sel.ID)
		p := selPickup(view)
		if tcvStr(view, "state") != "VERIFIED" || tcvStr(p, "code") != "000123" || tcvStr(p, "name") != "Leading zero store" || tcvStr(p, "address") != "Zero address" {
			t.Errorf("the pickup row comes from the directory (name/address as ECPay's list returns them), never from the browser POST: %v", view)
		}
		var code2, name2, addr2 string
		if err := f.owner.QueryRow(ctx, `SELECT code,name,address FROM fulfillment.pickup_versions WHERE id=$1`, selPickupID(view)).Scan(&code2, &name2, &addr2); err != nil || code2 != "000123" || strings.Contains(name2+addr2, "FORGED") {
			t.Errorf("stored pickup: %q %q %q %v", code2, name2, addr2, err)
		}
	})

	t.Run("other owner or session cannot read or verify a selection", func(t *testing.T) {
		b, other := e.newBuyer(), e.newBuyer()
		sel, _ := e.tclOpen(b, code, "/en/products/prod_owner", nil)
		e.mapReturn(sel.ID, sel.Fields)
		if st, view := e.readSel(other, sel.ID); st != 404 {
			t.Errorf("GET by another owner: %d %v (indistinguishable not-found)", st, view)
		}
		if st, view := e.verifySel(other, sel.ID); st != 404 {
			t.Errorf("verify by another owner: %d %v", st, view)
		}
		if st, view := e.readSel(other, randomUUID()); st != 404 {
			t.Errorf("GET of a random id: %d %v", st, view)
		}
		if state, _, _, _ := e.tclRow(sel.ID); state != "RETURNED" && state != "VERIFIED" {
			t.Errorf("the other owner's verify must not change the selection: %s", state)
		}
	})

	t.Run("pickup reuse vs a new version; an expiring head is not reused", func(t *testing.T) {
		pickupOf := func() string { b := e.newBuyer(); _, p := e.verifiedPickup(b, code); return p }
		p1 := pickupOf()
		if p2 := pickupOf(); p2 != p1 {
			t.Errorf("same store, same directory row, head valid > 1 h: the current version is reused (%s vs %s)", p1, p2)
		}
		var head int64
		var nsHead string
		if err := f.owner.QueryRow(ctx, `SELECT h.current_version,h.namespace FROM fulfillment.pickup_heads h JOIN fulfillment.pickup_versions v ON v.id=$1 AND v.tenant_id=h.tenant_id AND v.store_id=h.store_id AND v.kind=h.kind AND v.namespace=h.namespace AND v.code=h.code`, p1).Scan(&head, &nsHead); err != nil || head != 1 {
			t.Fatalf("head after two identical verifications: %d %s %v", head, nsHead, err)
		}
		// directory name change (a fresh client models the next 20:00 refresh)
		e.fake.SetStoreList("UNIMART", []ecpaytest.Store{{ID: "131386", Name: "Renamed 7-ELEVEN", Addr: "Stage address 7"}, {ID: "000123", Name: "Leading zero store", Addr: "Zero address"},
			{ID: "1328", Name: "Four char store", Addr: "Four address"}, {ID: "1234567", Name: "Seven char store", Addr: "Seven address"}, {ID: "123456789", Name: "Nine char store", Addr: "Nine address"}})
		e.reclient()
		p3 := pickupOf()
		if p3 == p1 {
			t.Error("a changed directory name appends a new version")
		}
		if err := f.owner.QueryRow(ctx, `SELECT current_version FROM fulfillment.pickup_heads WHERE tenant_id=$1 AND store_id=$2 AND namespace=$3 AND code='131386'`, f.tenantA, f.storeA1, nsHead).Scan(&head); err != nil || head != 2 {
			t.Errorf("head after a name change: version %d err %v (want 2, head CAS)", head, err)
		}
		// a verified head with < 1 h left is never reused (disclosed owner-pool fixture: only the validity window moves)
		mustExec(t, f.owner, `UPDATE fulfillment.pickup_versions SET attested_at=attested_at-interval '23 hours 30 minutes',valid_until=valid_until-interval '23 hours 30 minutes' WHERE id=$1`, p3)
		if p4 := pickupOf(); p4 == p3 {
			t.Error("a directory pickup with < 1 h validity left must not be reused")
		}
		// stage store rows of the SANDBOX environment live in their own namespace, closed to merchants (TCV02 proves the closure)
		if nsHead != "ecpay.sandbox.unimartc2c" {
			t.Errorf("namespace %s", nsHead)
		}
	})

	t.Run("late or replayed return after a newer selection cannot change the destination (head CAS)", func(t *testing.T) {
		b := e.newBuyer()
		selA, _ := e.tclOpen(b, code, "/en/products/prod_cas", nil)
		e.mapReturn(selA.ID, selA.Fields)
		viewA := e.tclFinal(b, selA.ID)
		pickupA := selPickupID(viewA)
		destA, err := b.destination("cvs_711", pickupA, name, phone)
		if err != nil {
			t.Fatal(err)
		}
		// a newer selection for another store becomes the destination
		selB, _ := e.tclOpen(b, code, "/en/products/prod_cas", nil)
		e.fake.SetMapStore("UNIMARTC2C", ecpaytest.Store{ID: "000123", Name: "Leading zero store", Addr: "Zero address"})
		e.mapReturn(selB.ID, selB.Fields)
		e.fake.SetMapStore("UNIMARTC2C", ecpaytest.Store{ID: "131386", Name: "Stage 7-ELEVEN", Addr: "Stage address 7"})
		pickupB := selPickupID(e.tclFinal(b, selB.ID))
		destB, err := b.destination("cvs_711", pickupB, name, phone)
		if err != nil {
			t.Fatal(err)
		}
		headBefore := destB.Version
		// the late replay of A's browser return (same body) after B
		for i := 0; i < 2; i++ {
			e.mapReturn(selA.ID, selA.Fields)
		}
		var headVersion int64
		var headPickup string
		if err := f.owner.QueryRow(ctx, `SELECT h.current_version,s.pickup_id::text FROM storefront.destination_heads h JOIN storefront.destination_snapshots s ON s.tenant_id=h.tenant_id AND s.store_id=h.store_id AND s.owner_id=h.owner_id AND s.id=h.destination_id
		  WHERE h.owner_id=$1`, b.cap.Scope.OwnerID).Scan(&headVersion, &headPickup); err != nil {
			t.Fatal(err)
		}
		if headVersion != headBefore || headPickup != pickupB || destA.Version >= headVersion {
			t.Errorf("destination head after a late replay: version %d pickup %s (want version %d pickup %s)", headVersion, headPickup, headBefore, pickupB)
		}
		if state, _, _, _ := e.tclRow(selA.ID); state != "VERIFIED" {
			t.Errorf("replayed return changed selection A: %s", state)
		}
	})

	t.Run("return_path allowlist and the fixed return origin", func(t *testing.T) {
		b := e.newBuyer()
		for _, bad := range []string{"/zh-TW/claim", "/fr/products/x", "/en/products/", "/en/products/a b", "/en/products/x/y", "//evil.example/en/products/x", "https://evil.example/en/products/x", "/en/products/x?next=1", "/en/products/x#f", "en/products/x"} {
			res := b.openSelection(code, bad)
			if res.status != 422 || tcvStr(tcvJSON(t, res.body), "code") != "bad_return_path" {
				t.Errorf("return_path %q: want 422 bad_return_path, got %d %s", bad, res.status, res.body)
			}
		}
		for _, ok := range []string{"/zh-TW/products/prod_1", "/zh-CN/products/A-b_9", "/en/products/" + strings.Repeat("a", 64)} {
			if res := b.openSelection(code, ok); res.status != 201 {
				t.Errorf("return_path %q must be accepted: %d %s", ok, res.status, res.body)
			}
		}
		// return_origin is not a body field: an attempt to send one is refused, never honoured
		res := b.req("POST", "/v1/buyer/cvs-selections", t04Key("tcl-origin"), map[string]any{"cart_version": b.cartVersion(), "market_id": e.p.market.ID, "service_code": code,
			"return_path": "/en/products/x", "return_origin": "https://evil.example"}, nil)
		if res.status < 400 {
			t.Errorf("a body return_origin must be refused: %d %s", res.status, res.body)
		}
		// an origin that is not an ACTIVE domain of the store: refused by the definer (BuyerCVS.Open called with it directly)
		cv := e.svc.CVS()
		_, err := cv.Open(ctx, b.cap.Token, e.store(), t04Key("tcl-badorigin"), "https://not-active.example", false, checkout.SelectionOpenInput{CartVersion: b.cartVersion(), MarketID: e.p.market.ID, ServiceCode: code, ReturnPath: "/en/products/x"})
		tcvExpectRefusal(t, "origin not ACTIVE for the store", err, 422, "bad_return_origin")
		// replay of the same key: the nonce is not re-derivable
		key := t04Key("tcl-replay")
		first := b.req("POST", "/v1/buyer/cvs-selections", key, map[string]any{"cart_version": b.cartVersion(), "market_id": e.p.market.ID, "service_code": code, "return_path": "/en/products/x"}, nil)
		again := b.req("POST", "/v1/buyer/cvs-selections", key, map[string]any{"cart_version": b.cartVersion(), "market_id": e.p.market.ID, "service_code": code, "return_path": "/en/products/x"}, nil)
		if first.status != 201 || again.status != 409 || tcvStr(tcvJSON(t, again.body), "code") != "selection_replay_new_key" {
			t.Errorf("replay: first %d, again %d %s (want 409 selection_replay_new_key)", first.status, again.status, again.body)
		}
	})

	t.Run("hooks: unknown selection 404 without Location; oversize, duplicate keys and other content types refused", func(t *testing.T) {
		w := e.mapReturn(randomUUID(), map[string]string{"MerchantID": merchantID, "MerchantTradeNo": "AAAAAAAAAAAAAAAAAAAA", "LogisticsSubType": "UNIMARTC2C"})
		if w.Code != http.StatusNotFound || w.Header().Get("Location") != "" || !strings.HasPrefix(w.Header().Get("Content-Type"), "text/plain") {
			t.Errorf("unknown selection: %d Location=%q type=%q (want 404 text/plain, no redirect)", w.Code, w.Header().Get("Location"), w.Header().Get("Content-Type"))
		}
		b := e.newBuyer()
		sel, _ := e.tclOpen(b, code, "/en/products/prod_hook", nil)
		_, _, _, v0 := e.tclRow(sel.ID)
		post := func(body, ctype string) *httptest.ResponseRecorder {
			return e.hookRaw("/v1/cvs/ecpay/map-return/"+sel.ID, body, ctype)
		}
		huge := "MerchantID=" + merchantID + "&MerchantTradeNo=" + sel.Fields["MerchantTradeNo"] + "&LogisticsSubType=UNIMARTC2C&CVSStoreID=131386&ExtraData=" + strings.Repeat("x", 9000)
		if r := post(huge, "application/x-www-form-urlencoded"); r.Code < 400 {
			t.Errorf("a body over 8 KiB must be refused: %d", r.Code)
		}
		dup := "MerchantID=" + merchantID + "&MerchantTradeNo=" + sel.Fields["MerchantTradeNo"] + "&LogisticsSubType=UNIMARTC2C&CVSStoreID=131386&CVSStoreID=999999"
		if r := post(dup, "application/x-www-form-urlencoded"); r.Code < 400 {
			t.Errorf("duplicate fields must be rejected: %d", r.Code)
		}
		if r := post(`{"MerchantID":"1"}`, "application/json"); r.Code < 400 && r.Code != http.StatusSeeOther {
			t.Errorf("a JSON body: %d", r.Code)
		}
		if _, _, _, v := e.tclRow(sel.ID); v != v0 {
			t.Errorf("a refused body changed the selection (version %d -> %d)", v0, v)
		}
		// unknown extra fields are ignored, the genuine subset works
		ok := post("MerchantID="+merchantID+"&MerchantTradeNo="+sel.Fields["MerchantTradeNo"]+"&LogisticsSubType=UNIMARTC2C&CVSStoreID=131386&CVSStoreName=x&CVSAddress=y&CVSOutSide=0&Unknown=1", "application/x-www-form-urlencoded")
		if ok.Code != http.StatusSeeOther {
			t.Errorf("a valid subset with an unknown extra field: %d", ok.Code)
		}
	})

	t.Run("stores and environments", tclStoresAndEnvironments)

	t.Run("open-selection limit: 10 OPEN per owner and cart, then 429 (§4.3)", func(t *testing.T) {
		b := e.newBuyer()
		accepted := 0
		var last bhResponse
		for i := 0; i < 14; i++ {
			last = b.openSelection(code, "/en/products/prod_limit")
			if last.status != 201 {
				break
			}
			accepted++
		}
		if accepted != 10 || last.status != 429 {
			t.Errorf("%d accepted then %d %s (want exactly 10 OPEN, then 429)", accepted, last.status, last.body)
		}
		if last.status == 429 && last.header.Get("Retry-After") == "" {
			t.Error("a 429 must carry Retry-After")
		}
	})
}

func tclStoresAndEnvironments(t *testing.T) {
	// A second store proves per-store return hosts (X9/round 2), and a LIVE deployment proves environment-scoped namespaces.
	ctx := context.Background()
	e1 := tcvNew(t)
	merchant1 := e1.connect("C2C")
	code1, _, _ := e1.service("cvs_711", "API", 0)
	e2 := tcvNew(t)
	merchant2 := e2.connect("C2C")
	code2, _, _ := e2.service("cvs_711", "API", 0)
	if merchant1 == merchant2 || e1.origin == e2.origin {
		t.Fatal("fixture: two stores need two merchant ids and two hosts")
	}

	t.Run("two stores on two ACTIVE domains: each 303 lands on its own store's origin", func(t *testing.T) {
		b1, b2 := e1.newBuyer(), e2.newBuyer()
		s1, _ := e1.tclOpen(b1, code1, "/en/products/prod_one", nil)
		s2, _ := e2.tclOpen(b2, code2, "/zh-TW/products/prod_two", nil)
		// one process serves both stores: env1's hooks handler answers both selections
		w1 := e1.mapReturn(s1.ID, s1.Fields)
		w2 := e1.hook("/v1/cvs/ecpay/map-return/"+s2.ID, e2.fake.MapReturn(urlValues(s2.Fields)), nil)
		if w1.Header().Get("Location") != e1.origin+"/en/products/prod_one?cvs_selection="+s1.ID {
			t.Errorf("store 1 return: %d %q", w1.Code, w1.Header().Get("Location"))
		}
		if w2.Header().Get("Location") != e2.origin+"/zh-TW/products/prod_two?cvs_selection="+s2.ID || w2.Code != http.StatusSeeOther {
			t.Errorf("store 2 return: %d %q (want its own origin %s)", w2.Code, w2.Header().Get("Location"), e2.origin)
		}
		// a selection is invisible to the other store's buyer
		if st, _ := e1.readSel(b1, s2.ID); st != 404 {
			t.Errorf("store 1 buyer reading store 2's selection: %d", st)
		}
	})

	t.Run("open with a profile whose environment differs from the deployment: service_unavailable", func(t *testing.T) {
		b := e1.newBuyer()
		bc, err := checkout.NewBuyerCVS(e1.p.pool, e1.keys, e1.client, fulfillmentCfg(e1, "LIVE"))
		if err != nil {
			t.Fatal(err)
		}
		_, err = bc.Open(ctx, b.cap.Token, e1.store(), t04Key("tcl-env"), e1.origin, false, checkout.SelectionOpenInput{CartVersion: b.cartVersion(), MarketID: e1.p.market.ID, ServiceCode: code1, ReturnPath: "/en/products/x"})
		tcvExpectRefusal(t, "LIVE deployment, SANDBOX profile", err, 422, "service_unavailable")
	})

	t.Run("SANDBOX and LIVE selections produce different namespaces", func(t *testing.T) {
		live := tcvNew(t, tcvOpts{payEnv: "LIVE"})
		live.connect("C2C")
		liveCode, _, _ := live.service("cvs_711", "API", 0)
		bl := live.newBuyer()
		_, livePickup := live.verifiedPickup(bl, liveCode)
		bs := e1.newBuyer()
		_, sandboxPickup := e1.verifiedPickup(bs, code1)
		var nsLive, nsSandbox string
		if err := e1.p.f.owner.QueryRow(ctx, `SELECT namespace FROM fulfillment.pickup_versions WHERE id=$1`, livePickup).Scan(&nsLive); err != nil {
			t.Fatal(err)
		}
		if err := e1.p.f.owner.QueryRow(ctx, `SELECT namespace FROM fulfillment.pickup_versions WHERE id=$1`, sandboxPickup).Scan(&nsSandbox); err != nil {
			t.Fatal(err)
		}
		if nsLive != "ecpay.live.unimartc2c" || nsSandbox != "ecpay.sandbox.unimartc2c" || nsLive == nsSandbox {
			t.Errorf("namespaces: live %q sandbox %q (stage and LIVE stores never share a head)", nsLive, nsSandbox)
		}
		if act := tcvStr(tcvJSON(t, bl.openSelection(liveCode, "/en/products/x").body), "form", "action"); act != "https://logistics.ecpay.com.tw/Express/map" {
			t.Errorf("LIVE form action %q", act)
		}
	})
}
