package foundation_test

// MF03 (contracts/manual-fulfilment-v1.md §2, §6): the shipment state machine, REAL_PG through the real
// HTTP handler. Prefix `mff`. Orders are paid through the real capture path (rfx harness); every refused
// source state is produced by a product path except the one named owner fixture (work item
// REVIEW_REQUIRED) and the srqReview review fixtures.

import (
	"context"
	"fmt"
	"reflect"
	"sync"
	"testing"
	"time"
)

var mffDTOKeys = []string{"carrier_code", "carrier_name", "note", "principal_id", "recorded_at", "status", "tracking_number", "tracking_url", "version", "void_reason"}

// mffFingerprint is the rfx fingerprint without the orders row (a shipment legitimately updates it).
func mffFingerprint(t *testing.T, e *rfxEnv, o rfxOrder) map[string]string {
	fp := e.fingerprint(t, o)
	delete(fp, "orders")
	return fp
}

func mffAssertQuiet(t *testing.T, e *rfxEnv, o rfxOrder, before map[string]string, effects [4]int, what string) {
	t.Helper()
	after := mffFingerprint(t, e, o)
	for k, v := range before {
		if after[k] != v {
			t.Fatalf("%s changed %s (a manual shipment never touches stock, facts or work items)", what, k)
		}
	}
	if got := e.mfxSideEffects(t); got != effects {
		t.Fatalf("%s created provider-side rows (operations, River jobs, refunds, signals): %v -> %v (MD2)", what, effects, got)
	}
}

func TestManualFulfilmentMF03Transitions(t *testing.T) {
	e := rfxNew(t)
	e.startWorker(t)
	base := e.storeFor(t) // one store for the whole gate (connection budget); base's own hold is the DRAFT order
	main := e.payMore(t, base)
	race := e.payMore(t, base)
	partial := e.payMore(t, base)
	st := e.mfxRefusedStates(t, base, true)
	// the worker keeps polling: wait for every capture job to settle so background writes cannot look like effects
	for _, o := range []rfxOrder{main, race, partial} {
		e.await(t, "capture jobs settled", o.attempt, 45*time.Second, `SELECT NOT EXISTS(SELECT 1 FROM river_payment.river_job WHERE args->>'operation_id'=$1 AND state NOT IN ('completed','cancelled','discarded'))`)
	}

	t.Run("record, correct, void, re-record", func(t *testing.T) {
		fp := mffFingerprint(t, e, main)
		effects := e.mfxSideEffects(t)
		put := func(key, body string) (int, map[string]any, []byte) { return e.mfxPut(main, main.token(), key, body) }

		key1 := t04Key("mff-v1")
		body1 := mfxShipBody(0, "SHIPPED", "seven_eleven_cvs", "", "0012345678", "https://track.example.com/t?n=0012345678", "left at the counter", "")
		status, v1, raw1 := put(key1, body1)
		if status != 200 || !reflect.DeepEqual(sraKeys(v1), mffDTOKeys) {
			t.Fatalf("record: %d %s", status, raw1)
		}
		if v1["version"] != float64(1) || v1["status"] != "SHIPPED" || v1["carrier_code"] != "seven_eleven_cvs" || v1["tracking_number"] != "0012345678" || v1["carrier_name"] != nil ||
			v1["tracking_url"] != "https://track.example.com/t?n=0012345678" || v1["note"] != "left at the counter" || v1["void_reason"] != nil || v1["principal_id"] != main.s.p.f.principalA {
			t.Fatalf("v1 DTO: %v", v1)
		}
		sraTime(t, v1["recorded_at"])
		if got := e.mfxFulfilmentState(t, main); got != "MERCHANT_SHIPPED" {
			t.Fatalf("fulfillment_state %s", got)
		}
		var commercial string
		var head int64
		if err := e.f.owner.QueryRow(context.Background(), `SELECT o.commercial_state,h.current_version FROM checkout.orders o JOIN fulfillment.manual_shipment_heads h ON h.order_id=o.id WHERE o.id=$1`, main.order).Scan(&commercial, &head); err != nil || commercial != "CONFIRMED" || head != 1 {
			t.Fatalf("commercial=%s head=%d err=%v", commercial, head, err)
		}
		if e.mfxAudit(t, main, "fulfillment.shipment_recorded") != 1 || e.count(t, `SELECT count(*) FROM ops.command_results WHERE tenant_id=$1 AND store_id=$2 AND operation='fulfillment.manual_shipment.record'`, main.s.p.f.tenantA, main.store()) != 1 {
			t.Fatal("record must write exactly one audit row and one command result")
		}
		// replay: same key + same body -> the saved answer, nothing new
		status, replay, _ := put(key1, body1)
		if status != 200 || !reflect.DeepEqual(replay, v1) || e.mfxVersions(t, main) != 1 || e.mfxAudit(t, main, "fulfillment.shipment_recorded") != 1 {
			t.Fatalf("replay: %d %v", status, replay)
		}
		if status, out, _ := put(key1, mfxShip(0, "seven_eleven_cvs", "9999")); status != 409 {
			t.Fatalf("same key, different body: %d %v", status, out)
		}
		// stale expected_version
		if status, out, _ := put(t04Key("mff-stale"), mfxShip(0, "sf_express", "SF1")); status != 409 || srqCode(out) != "version_changed" {
			t.Fatalf("stale expected_version: %d %v", status, out)
		}
		if e.mfxVersions(t, main) != 1 {
			t.Fatal("a refused command wrote a version")
		}
		// correction while the head is SHIPPED
		status, v2, raw2 := put(t04Key("mff-v2"), mfxShip(1, "sf_express", "SF7777"))
		if status != 200 || v2["version"] != float64(2) || v2["status"] != "SHIPPED" || v2["carrier_code"] != "sf_express" || v2["tracking_number"] != "SF7777" || v2["tracking_url"] != nil {
			t.Fatalf("correct: %d %s", status, raw2)
		}
		if e.mfxFulfilmentState(t, main) != "MERCHANT_SHIPPED" || e.mfxAudit(t, main, "fulfillment.shipment_corrected") != 1 {
			t.Fatal("correction changed the state or wrote no corrected audit row")
		}
		// void: returns the order to MANUAL_UNASSIGNED, the voided version copies the carrier/tracking it voids
		status, v3, raw3 := put(t04Key("mff-v3"), mfxVoid(2, "wrong_tracking"))
		if status != 200 || v3["version"] != float64(3) || v3["status"] != "VOIDED" || v3["void_reason"] != "wrong_tracking" || v3["carrier_code"] != "sf_express" || v3["tracking_number"] != "SF7777" {
			t.Fatalf("void: %d %s", status, raw3)
		}
		if e.mfxFulfilmentState(t, main) != "MANUAL_UNASSIGNED" || e.mfxAudit(t, main, "fulfillment.shipment_voided") != 1 {
			t.Fatal("void must return the order to MANUAL_UNASSIGNED with a voided audit row")
		}
		// void only while the head is SHIPPED
		if status, out, _ := put(t04Key("mff-void2"), mfxVoid(3, "other")); status != 422 || srqCode(out) != "void_requires_shipped" {
			t.Fatalf("void of a voided head: %d %v", status, out)
		}
		// record again after a void: v4, MD6 again
		status, v4, raw4 := put(t04Key("mff-v4"), mfxShip(3, "familymart_cvs", "FM-0001"))
		if status != 200 || v4["version"] != float64(4) || v4["status"] != "SHIPPED" {
			t.Fatalf("re-record: %d %s", status, raw4)
		}
		if e.mfxFulfilmentState(t, main) != "MERCHANT_SHIPPED" {
			t.Fatal("re-record did not ship again")
		}
		// the shipped state shows in the merchant list/detail; the filters follow the head
		if _, ids := e.mfxOrdersState(t, main, main.token(), "shipped"); !ids[main.order] {
			t.Fatal("state=shipped lacks the shipped order")
		}
		if _, ids := e.mfxOrdersState(t, main, main.token(), "unshipped"); ids[main.order] {
			t.Fatal("state=unshipped lists a shipped order")
		}
		// history: all versions, ascending, merchant-only fields included
		status, raw, _ := e.call("GET", mfxPath(main)+"/history", main.token(), nil, "")
		hist := sraJSON(raw)
		items, _ := hist["items"].([]any)
		if status != 200 || len(hist) != 1 || len(items) != 4 {
			t.Fatalf("history: %d %s", status, raw)
		}
		for i, it := range items {
			m := it.(map[string]any)
			if m["version"] != float64(i+1) || !reflect.DeepEqual(sraKeys(m), mffDTOKeys) {
				t.Fatalf("history item %d: %v", i, m)
			}
		}
		if items[0].(map[string]any)["note"] != "left at the counter" || items[2].(map[string]any)["void_reason"] != "wrong_tracking" || items[0].(map[string]any)["principal_id"] != main.s.p.f.principalA {
			t.Fatalf("history lost merchant-only fields: %v", items)
		}
		// exactly one audit row per accepted command; nothing else moved
		if n := e.mfxAudit(t, main, "fulfillment.shipment_recorded") + e.mfxAudit(t, main, "fulfillment.shipment_corrected") + e.mfxAudit(t, main, "fulfillment.shipment_voided"); n != 4 || e.mfxVersions(t, main) != 4 {
			t.Fatalf("audit rows %d for %d versions", n, e.mfxVersions(t, main))
		}
		mffAssertQuiet(t, e, main, fp, effects, "the shipment lifecycle")
		// a shipped order may still be refunded (M-8) and a refund never changes fulfilment state (RD6)
		id := e.mustRefund(t, main, main.captured, "requested_by_customer")
		e.awaitRefundFact(t, id, main.attempt, "SUCCEEDED")
		if got := e.mfxFulfilmentState(t, main); got != "MERCHANT_SHIPPED" {
			t.Fatalf("a full refund changed fulfilment state to %s", got)
		}
	})

	t.Run("every refused source state: 422 not_shippable, zero rows, absent from unshipped and export", func(t *testing.T) {
		for name, o := range st.all() {
			effects := e.mfxSideEffects(t)
			fp := mffFingerprint(t, e, o)
			// Audit rows are counted per store and every refused order shares the gate's one store
			// (connection budget), so the invariant is "no new row", not "zero rows in the store".
			auditBefore := e.mfxAudit(t, o, "fulfillment.shipment_recorded")
			status, out, raw := e.mfxPut(o, o.token(), t04Key("mff-refused"), mfxShip(0, "sf_express", "SF0001"))
			if status != 422 || srqCode(out) != "not_shippable" {
				t.Fatalf("%s: %d %s (want 422 not_shippable)", name, status, raw)
			}
			if e.mfxVersions(t, o) != 0 || e.count(t, `SELECT count(*) FROM fulfillment.manual_shipment_heads WHERE order_id=$1`, o.order) != 0 || e.mfxAudit(t, o, "fulfillment.shipment_recorded") != auditBefore {
				t.Fatalf("%s: a refused command left a version, head or audit row", name)
			}
			if got := e.mfxFulfilmentState(t, o); got == "MERCHANT_SHIPPED" {
				t.Fatalf("%s: became MERCHANT_SHIPPED", name)
			}
			if status, ids := e.mfxOrdersState(t, o, o.token(), "unshipped"); status != 200 || ids[o.order] {
				t.Fatalf("%s: state=unshipped answered %d and lists the order: %v", name, status, ids[o.order])
			}
			status, csv, _ := e.mfxCSV(o, o.token())
			if status != 200 || mfxContains(csv, o.order) {
				t.Fatalf("%s: export answered %d or contains the order", name, status)
			}
			mffAssertQuiet(t, e, o, fp, effects, name)
		}
		// the same payload succeeds where MD6 holds: control that the refusals are about state, not the body
		status, _, raw := e.mfxPut(race, race.token(), t04Key("mff-control"), mfxShip(0, "sf_express", "SF0001"))
		if status != 200 {
			t.Fatalf("control shipment refused: %d %s", status, raw)
		}
	})

	t.Run("a partial refund still ships", func(t *testing.T) {
		id := e.mustRefund(t, partial, 500, "requested_by_customer")
		e.awaitRefundFact(t, id, partial.attempt, "SUCCEEDED")
		if _, ids := e.mfxOrdersState(t, partial, partial.token(), "unshipped"); !ids[partial.order] {
			t.Fatal("a partially refunded order is missing from state=unshipped")
		}
		if status, _, raw := e.mfxPut(partial, partial.token(), t04Key("mff-partial"), mfxShip(0, "chunghwa_post", "RA123456789TW")); status != 200 {
			t.Fatalf("partially refunded order: %d %s", status, raw)
		}
		if _, ids := e.mfxOrdersState(t, partial, partial.token(), "unshipped"); ids[partial.order] {
			t.Fatal("shipped partially refunded order still unshipped")
		}
	})

	t.Run("two concurrent records: exactly one version 1 (real two-transaction witness)", func(t *testing.T) {
		o := e.payMore(t, base)
		auditBefore := e.mfxAudit(t, o, "fulfillment.shipment_recorded") // store-scoped: compare the delta
		ctx := context.Background()
		holder, err := e.f.owner.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer holder.Rollback(ctx)
		var pid int
		if err := holder.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&pid); err != nil {
			t.Fatal(err)
		}
		if _, err := holder.Exec(ctx, `SELECT 1 FROM checkout.orders WHERE id=$1 FOR UPDATE`, o.order); err != nil {
			t.Fatal(err)
		}
		type result struct {
			status int
			out    map[string]any
		}
		results := make(chan result, 2)
		var wg sync.WaitGroup
		for i := 0; i < 2; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				status, out, _ := e.mfxPut(o, o.token(), t04Key(fmt.Sprintf("mff-race-%d", i)), mfxShip(0, "sf_express", fmt.Sprintf("SFRACE%d", i)))
				results <- result{status, out}
			}(i)
		}
		deadline := time.Now().Add(20 * time.Second)
		for {
			blocked := e.queuedBehind(t, pid)
			if blocked >= 2 {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("only %d of 2 records queued behind the order lock", blocked)
			}
			time.Sleep(20 * time.Millisecond) // lock_timeout is 1 s: release promptly
		}
		if err := holder.Commit(ctx); err != nil {
			t.Fatal(err)
		}
		wg.Wait()
		close(results)
		won, lost := 0, 0
		for r := range results {
			switch {
			case r.status == 200:
				won++
			case r.status == 409 && srqCode(r.out) == "version_changed":
				lost++
			default:
				t.Fatalf("race outcome %d %v", r.status, r.out)
			}
		}
		if won != 1 || lost != 1 || e.mfxVersions(t, o) != 1 || e.mfxAudit(t, o, "fulfillment.shipment_recorded") != auditBefore+1 {
			t.Fatalf("won=%d lost=%d versions=%d", won, lost, e.mfxVersions(t, o))
		}
	})

	t.Run("void of an order that was never shipped", func(t *testing.T) {
		o := e.payMore(t, base)
		if status, out, _ := e.mfxPut(o, o.token(), t04Key("mff-void0"), mfxVoid(0, "wrong_order")); status != 422 || srqCode(out) != "void_requires_shipped" {
			t.Fatalf("void without a head: %d %v", status, out)
		}
		if e.mfxVersions(t, o) != 0 {
			t.Fatal("void wrote a version")
		}
	})
}
