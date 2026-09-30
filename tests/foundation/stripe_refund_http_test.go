package foundation_test

// RF09 (contracts/stripe-refund-v1.md §7.1, §7.2, §9): the merchant refund HTTP surface and the
// buyer projection, HTTP_PG. Prefix `sra`. The BFF half is tests/admin/refund-bff.test.ts.
//
// The handler is httpapi.NewHandler(pool, httpapi.Options{RefundJobs: ...}) over the runtime pool,
// exactly the assembly cmd/api builds; the buyer routes are buyerhttp.New over the same database.
// Orders are paid through the real capture path (rfx harness); the payment worker is stopped while
// a REQUESTED refund is inspected. PAYUNi bytes are compared with the SP14 golden files through the
// SP14 harness itself (sbhPayuniFlow), so a golden is never regenerated here.

import (
	"context"
	"encoding/json"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"livecommerce/internal/checkout"
)

func sraKeys(m map[string]any) []string {
	var k []string
	for name := range m {
		k = append(k, name)
	}
	sort.Strings(k)
	return k
}

func sraJSON(raw []byte) map[string]any {
	var out map[string]any
	_ = json.Unmarshal(raw, &out)
	return out
}

func sraTime(t *testing.T, v any) time.Time {
	t.Helper()
	s, _ := v.(string)
	ts, err := time.Parse(time.RFC3339Nano, s)
	if err != nil || !strings.HasSuffix(s, "Z") {
		t.Fatalf("timestamp %v is not canonical RFC 3339 UTC: %v", v, err)
	}
	return ts
}

func TestStripeRF09AdminHTTP(t *testing.T) {
	e := rfxNew(t)
	f := &srfEnv{rfxEnv: e}
	a := e.stripeStore(t)
	endpoint, secret := e.endpoint(t, a)
	stop := e.startWorker(t)
	o := e.pay(t, a, endpoint, secret)
	shipped := e.payMore(t, o)
	reviewed := e.payMore(t, o)
	for _, x := range []rfxOrder{o, shipped, reviewed} {
		e.grant(t, x, "orders:read", "payments:refund", "fulfillment:write")
	}
	readerToken, _ := e.member(t, o, "orders:read")
	refundOnlyToken, _ := e.member(t, o, "payments:refund")
	bothToken, _ := e.member(t, o, "orders:read", "payments:refund")
	nothingToken, _ := e.member(t, o, "catalog:read")
	stop() // REQUESTED refunds stay unsent while the wire shape is inspected

	var first string
	t.Run("wire shape and codes", func(t *testing.T) {
		status, out := e.request(o, o.token(), t04Key("sra-1"), rfxBody(1000, "requested_by_customer", 2500))
		if status != 201 || !reflect.DeepEqual(sraKeys(out), []string{"amount_minor", "currency", "refund_id", "refundable_minor", "state"}) || out["state"] != "REQUESTED" {
			t.Fatalf("POST: %d %v", status, out)
		}
		first = out["refund_id"].(string)
		list, raw := e.list(t, o, o.token())
		if !reflect.DeepEqual(sraKeys(list), []string{"captured_minor", "currency", "items", "pending_minor", "refundable_minor", "refunded_minor"}) {
			t.Fatalf("list keys: %v", sraKeys(list))
		}
		items := list["items"].([]any)
		if len(items) != 1 {
			t.Fatalf("items: %s", raw)
		}
		item := items[0].(map[string]any)
		// failure_reason is omitted unless failed/canceled; stripe_refund_id is present and null until pinned.
		if !reflect.DeepEqual(sraKeys(item), []string{"amount_minor", "reason", "refund_id", "requested_at", "state", "stripe_refund_id", "updated_at"}) ||
			item["state"] != "REQUESTED" || item["stripe_refund_id"] != nil || item["reason"] != "requested_by_customer" || item["amount_minor"] != float64(1000) {
			t.Fatalf("REQUESTED item: %v", item)
		}
		if req, upd := sraTime(t, item["requested_at"]), sraTime(t, item["updated_at"]); upd.Before(req) {
			t.Fatal("updated_at precedes requested_at")
		}
		if list["currency"] != "TWD" || list["captured_minor"] != float64(2500) || list["pending_minor"] != float64(1000) || list["refundable_minor"] != float64(1500) || list["refunded_minor"] != float64(0) {
			t.Fatalf("numbers: %v", list)
		}
		for _, k := range []string{"Cache-Control"} {
			_, _, h := e.call("GET", rfxRefundsPath(o), o.token(), nil, "")
			if !strings.Contains(h.Get(k), "no-store") {
				t.Fatalf("%s = %q", k, h.Get(k))
			}
		}
		e.startWorker(t)
		e.awaitRefundFact(t, first, o.attempt, "SUCCEEDED")
		list, _ = e.list(t, o, o.token())
		item = list["items"].([]any)[0].(map[string]any)
		// R-9: the merchant sees the Stripe refund id; nothing else provider-shaped.
		if item["state"] != "SUCCEEDED" || item["stripe_refund_id"] != e.fake.RefundByRef(first) || item["failure_reason"] != nil {
			t.Fatalf("SUCCEEDED item: %v", item)
		}
		// A failed refund carries failure_reason.
		e.fake.HoldNextRefund("pending", "processing")
		failing := e.mustRefund(t, o, 500, "duplicate")
		e.awaitRefund(t, "pinned", failing, o.attempt, 45*time.Second, `SELECT stripe_refund_id IS NOT NULL FROM payments.stripe_refunds WHERE id=$1`)
		e.fake.SetRefundStatus(e.fake.RefundByRef(failing), "failed", "insufficient_funds")
		e.awaitRefundFact(t, failing, o.attempt, "FAILED")
		list, _ = e.list(t, o, o.token())
		var failed map[string]any
		for _, it := range list["items"].([]any) {
			if m := it.(map[string]any); m["refund_id"] == failing {
				failed = m
			}
		}
		if failed == nil || failed["state"] != "FAILED" || failed["failure_reason"] != "insufficient_funds" || failed["reason"] != "duplicate" {
			t.Fatalf("FAILED item: %v", failed)
		}
		// Items are bounded (<=20) and the list is deterministic newest-first or oldest-first, but stable.
		if n := len(list["items"].([]any)); n != 2 {
			t.Fatalf("items = %d", n)
		}
		e.stopAllWorkers()
	})

	t.Run("permission matrix", func(t *testing.T) {
		// POST needs payments:refund, GET needs orders:read; neither implies the other.
		for _, c := range []struct {
			name, token string
			post, get   int
		}{
			{"orders:read only", readerToken, 403, 200},
			{"payments:refund only", refundOnlyToken, 201, 403},
			{"both", bothToken, 201, 200},
			{"neither", nothingToken, 403, 403},
			{"anonymous", "", 401, 401},
			{"creator", o.token(), 201, 200},
		} {
			status, out := e.request(o, c.token, t04Key("sra-perm"), rfxBody(100, "requested_by_customer", e.refundable(t, o)))
			if status != c.post {
				t.Fatalf("%s POST: %d %v want %d", c.name, status, out, c.post)
			}
			if gs, raw, _ := e.call("GET", rfxRefundsPath(o), c.token, nil, ""); gs != c.get {
				t.Fatalf("%s GET: %d %s want %d", c.name, gs, raw, c.get)
			}
		}
		// The refresh route needs payments:refund too.
		refund := first
		if s, _, _ := e.call("POST", rfxRefundsPath(o)+"/"+refund+"/refresh", readerToken, nil, ""); s != 403 {
			t.Fatalf("refresh with orders:read only: %d", s)
		}
		// Unknown methods on the routes.
		for _, m := range []string{"PUT", "PATCH", "DELETE"} {
			if s, _, _ := e.call(m, rfxRefundsPath(o), o.token(), map[string]string{"Idempotency-Key": t04Key("sra-m")}, `{}`); s != 405 {
				t.Fatalf("%s on /refunds: %d", m, s)
			}
		}
		if s, _, _ := e.call("GET", rfxRefundsPath(o)+"/"+first+"/refresh", o.token(), nil, ""); s != 405 {
			t.Fatalf("GET on refresh: %d", s)
		}
	})

	t.Run("strict transport rules write nothing", func(t *testing.T) {
		before := srqCount(t, e, shipped)
		key := func() map[string]string { return map[string]string{"Idempotency-Key": t04Key("sra-strict")} }
		post := func(name, body string, hdr map[string]string, path string) {
			t.Helper()
			status, raw, _ := e.call("POST", path, shipped.token(), hdr, body)
			if status < 400 || status >= 500 {
				t.Fatalf("%s: HTTP %d %s (want a 4xx and no refund)", name, status, raw)
			}
		}
		p := rfxRefundsPath(shipped)
		good := rfxBody(100, "requested_by_customer", 2500)
		post("extra field", `{"amount_minor":100,"reason":"requested_by_customer","expected_refundable_minor":2500,"note":"x"}`, key(), p)
		post("client-supplied currency", `{"amount_minor":100,"reason":"requested_by_customer","expected_refundable_minor":2500,"currency":"USD"}`, key(), p)
		post("missing amount", `{"reason":"requested_by_customer","expected_refundable_minor":2500}`, key(), p)
		post("missing reason", `{"amount_minor":100,"expected_refundable_minor":2500}`, key(), p)
		post("missing expected", `{"amount_minor":100,"reason":"requested_by_customer"}`, key(), p)
		post("duplicate key", `{"amount_minor":100,"amount_minor":200,"reason":"requested_by_customer","expected_refundable_minor":2500}`, key(), p)
		post("string amount", `{"amount_minor":"100","reason":"requested_by_customer","expected_refundable_minor":2500}`, key(), p)
		post("fractional amount", `{"amount_minor":100.5,"reason":"requested_by_customer","expected_refundable_minor":2500}`, key(), p)
		post("negative amount", `{"amount_minor":-100,"reason":"requested_by_customer","expected_refundable_minor":2500}`, key(), p)
		post("zero amount", `{"amount_minor":0,"reason":"requested_by_customer","expected_refundable_minor":2500}`, key(), p)
		post("null reason", `{"amount_minor":100,"reason":null,"expected_refundable_minor":2500}`, key(), p)
		post("unknown reason", `{"amount_minor":100,"reason":"fraudulent","expected_refundable_minor":2500}`, key(), p)
		post("trailing data", good+`{}`, key(), p)
		post("array body", `[]`, key(), p)
		post("empty body", ``, key(), p)
		post("null body", `null`, key(), p)
		post("invalid UTF-8", "{\"amount_minor\":100,\"reason\":\"requested_by_customer\",\"expected_refundable_minor\":2500,\"x\":\"\xff\"}", key(), p)
		post("over 64 KiB", `{"amount_minor":100,"reason":"`+strings.Repeat("a", 64<<10)+`","expected_refundable_minor":2500}`, key(), p)
		post("missing Idempotency-Key", good, nil, p)
		post("empty Idempotency-Key", good, map[string]string{"Idempotency-Key": ""}, p)
		post("query string", good, key(), p+"?x=1")
		post("wrong media type", good, map[string]string{"Idempotency-Key": t04Key("sra-mt"), "Content-Type": "text/plain"}, p)
		post("malformed order id", good, key(), "/v1/admin/stores/"+shipped.store()+"/orders/not-a-uuid/refunds")
		if after := srqCount(t, e, shipped); after != before {
			t.Fatalf("a refused transport case wrote rows: %+v -> %+v", before, after)
		}
		// GET rules: no query, no body, no Idempotency-Key.
		for name, run := range map[string]func() int{
			"GET with a query": func() int { s, _, _ := e.call("GET", p+"?limit=1", shipped.token(), nil, ""); return s },
			"GET with a key":   func() int { s, _, _ := e.call("GET", p, shipped.token(), key(), ""); return s },
			"GET with a body":  func() int { s, _, _ := e.call("GET", p, shipped.token(), nil, "{}"); return s },
		} {
			if s := run(); s < 400 || s >= 500 {
				t.Fatalf("%s: %d", name, s)
			}
		}
	})

	t.Run("refresh throttle", func(t *testing.T) {
		ctx := context.Background()
		refresh := func(refund string) (int, map[string]any) {
			s, raw, _ := e.call("POST", rfxRefundsPath(o)+"/"+refund+"/refresh", o.token(), nil, "")
			return s, sraJSON(raw)
		}
		signals := func() int {
			return e.count(t, `SELECT count(*) FROM payments.stripe_signals WHERE refund_id=$1`, first)
		}
		jobs := func() int {
			return e.count(t, `SELECT count(*) FROM river_payment.river_job WHERE kind='payment_signal_v1' AND args->>'operation_id'=$1`, first)
		}
		s, out := refresh(first) // SUCCEEDED: a late-failure check is allowed
		if s != 200 || out["scheduled"] != true || out["refund_id"] != first || len(out) != 2 {
			t.Fatalf("first refresh: %d %v", s, out)
		}
		sig, job := signals(), jobs()
		// Immediately again: throttled in SQL (>= 10 s), and the River insert is rolled back with it.
		s, out = refresh(first)
		if s != 200 || out["scheduled"] != false || signals() != sig || jobs() != job {
			t.Fatalf("throttled refresh: %d %v signals %d->%d jobs %d->%d", s, out, sig, signals(), job, jobs())
		}
		e.ageRefund(t, first, 11*time.Second)
		if s, out = refresh(first); s != 200 || out["scheduled"] != true {
			t.Fatalf("refresh after the throttle window: %d %v", s, out)
		}
		// Cap of 30 refreshes per refund.
		tx, err := e.f.owner.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = tx.Exec(ctx, `SET LOCAL session_replication_role=replica`); err != nil {
			t.Fatal(err)
		}
		if _, err = tx.Exec(ctx, `UPDATE payments.stripe_refunds SET refresh_count=30,last_refresh_at=clock_timestamp()-interval '1 hour' WHERE id=$1`, first); err != nil {
			t.Fatal(err)
		}
		if err = tx.Commit(ctx); err != nil {
			t.Fatal(err)
		}
		if s, out = refresh(first); s != 200 || out["scheduled"] != false {
			t.Fatalf("refresh beyond the cap: %d %v", s, out)
		}
		// After a terminal fact other than SUCCEEDED refresh is refused (scheduled=false).
		var failed string
		if err := e.f.owner.QueryRow(ctx, `SELECT refund_id::text FROM payments.refund_facts WHERE attempt_id=$1 AND kind='FAILED'`, o.attempt).Scan(&failed); err != nil {
			t.Fatal(err)
		}
		if s, out = refresh(failed); s != 200 || out["scheduled"] != false {
			t.Fatalf("refresh of a FAILED refund: %d %v", s, out)
		}
		// Unknown refund and another order's refund are indistinguishable 404s.
		s1, _ := refresh(randomUUID())
		s2, _, _ := e.call("POST", rfxRefundsPath(shipped)+"/"+first+"/refresh", shipped.token(), nil, "")
		if s1 != 404 || s2 != 404 {
			t.Fatalf("unknown/foreign refund refresh: %d %d", s1, s2)
		}
		// No body, no key on refresh.
		if s, _, _ := e.call("POST", rfxRefundsPath(o)+"/"+first+"/refresh", o.token(), nil, `{}`); s < 400 || s >= 500 {
			t.Fatalf("refresh with a body: %d", s)
		}
		if s, _, _ := e.call("POST", rfxRefundsPath(o)+"/"+first+"/refresh", o.token(), map[string]string{"Idempotency-Key": t04Key("sra-r")}, ""); s < 400 || s >= 500 {
			t.Fatalf("refresh with an Idempotency-Key: %d", s)
		}
	})

	t.Run("merchant projection decodes refunded, shipped and reviewed orders", func(t *testing.T) {
		e.startWorker(t)
		// (1) a fully refunded order.
		full := e.mustRefund(t, shipped, shipped.captured, "requested_by_customer")
		e.awaitRefundFact(t, full, shipped.attempt, "SUCCEEDED")
		f.wantMoney(t, shipped, "REFUNDED", shipped.captured, 0)
		// (2) a MERCHANT_SHIPPED order that is also fully refunded (refund never changes fulfilment, RD6/M-8).
		// The order was refunded first, so shipping is refused (full refund held): use the review order for shipping.
		shipBody := `{"expected_version":0,"status":"SHIPPED","carrier_code":"familymart_cvs","carrier_name":null,"tracking_number":"0099887766","tracking_url":null,"note":null,"void_reason":null}`
		if s, raw, _ := e.call("PUT", "/v1/admin/stores/"+reviewed.store()+"/orders/"+reviewed.order+"/shipment", reviewed.token(), map[string]string{"Idempotency-Key": t04Key("sra-ship")}, shipBody); s != 200 {
			t.Fatalf("ship: %d %s", s, raw)
		}
		sum := f.summary(t, reviewed)
		if sum["fulfillment_state"] != "MERCHANT_SHIPPED" || sum["payment_state"] != "CAPTURED" || sum["work_state"] != "READY" || sum["refunded_minor"] != float64(0) {
			t.Fatalf("shipped summary: %v", sum)
		}
		// (3) shipped + partial refund: still decodes (a partial refund never blocks the projection).
		part := e.mustRefund(t, reviewed, 500, "requested_by_customer")
		e.awaitRefundFact(t, part, reviewed.attempt, "SUCCEEDED")
		f.wantMoney(t, reviewed, "PARTIALLY_REFUNDED", 500, 0)
		if det := f.merchantOrder(t, reviewed); det["fulfillment_state"] != "MERCHANT_SHIPPED" {
			t.Fatalf("shipped+refunded detail: %v", det["fulfillment_state"])
		}
		// (4) a READY order with a refund review: REVIEW_REQUIRED outranks REFUNDED/PARTIALLY_REFUNDED (precedence).
		srqReview(t, e, reviewed.attempt, "REFUND_HISTORY")
		f.wantMoney(t, reviewed, "REVIEW_REQUIRED", 500, 0)
		if det := f.merchantOrder(t, reviewed); det["work_state"] != "READY" {
			t.Fatalf("work_state %v: a post-capture review must not touch it", det["work_state"])
		}
		srqUnreview(t, e, reviewed.attempt)
		srqReview(t, e, shipped.attempt, "CONFLICTING_REPORT")
		f.wantMoney(t, shipped, "REVIEW_REQUIRED", shipped.captured, 0) // REVIEW_REQUIRED > REFUNDED
		srqUnreview(t, e, shipped.attempt)
		f.wantMoney(t, shipped, "REFUNDED", shipped.captured, 0)
		// The list page as a whole still decodes with all of these states side by side.
		status, raw, _ := e.call("GET", "/v1/admin/stores/"+o.store()+"/orders?limit=100", o.token(), nil, "")
		if status != 200 {
			t.Fatalf("list with mixed states: %d %s", status, raw)
		}
		// filters unchanged for the old values
		if status, _, _ := e.call("GET", "/v1/admin/stores/"+o.store()+"/orders?state=CONFIRMED", o.token(), nil, ""); status != 200 {
			t.Fatalf("state=CONFIRMED: %d", status)
		}
	})

	t.Run("buyer projection has amounts only", func(t *testing.T) {
		srv := e.serveHTTP(t, o.s)
		path := "/v1/buyer/orders/" + o.order + "/payment"
		want := append(append([]string{}, sbhViewKeys...), "refund")
		sort.Strings(want)
		view := bphRaw(t, srv.request(t, "GET", path, o.s.p.cap.Token, "", nil, nil), 200, want)
		var refund map[string]json.RawMessage
		if err := json.Unmarshal(view["refund"], &refund); err != nil || !reflect.DeepEqual(bpKeys(refund), []string{"pending_minor", "refunded_minor"}) {
			t.Fatalf("buyer refund object: %s", view["refund"])
		}
		body := string(view["refund"])
		for _, s := range []string{"re_", "stripe", "reason", "failure", first} {
			if strings.Contains(body, s) {
				t.Fatalf("buyer refund object leaks %q: %s", s, body)
			}
		}
		state := strings.Trim(string(view["payment_state"]), `"`)
		if state != "PARTIALLY_REFUNDED" && state != "REFUNDED" && state != "CAPTURED" {
			t.Fatalf("buyer payment_state %q", state)
		}
		// D8: an order without any refund activity carries no `refund` key at all (absent, not null).
		none := e.payMore(t, o)
		// Same store as o (one store per gate): reuse srv; a second serveHTTP would re-insert the store's
		// storefront publication (duplicate storefront_publications_pkey).
		plain := bphRaw(t, srv.request(t, "GET", "/v1/buyer/orders/"+none.order+"/payment", none.s.p.cap.Token, "", nil, nil), 200, nil)
		if _, has := plain["refund"]; has {
			t.Fatalf("refund key present without refund activity: %s", plain["refund"])
		}
		if v := e.view(t, none.s); v.Refund != nil {
			t.Fatalf("service view without refunds has Refund=%+v", v.Refund)
		}
	})

	t.Run("PAYUNi responses are byte-identical to the SP14 golden files", func(t *testing.T) {
		h := hpSetup(t)
		sbhPayuniFlow(t, h, sbhServe(t, h, h.api.(*checkout.HostedPaymentStarter), true))
	})
}
