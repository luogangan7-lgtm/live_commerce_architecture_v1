package foundation_test

// Shared helpers of the manual-fulfilment gates (MF02–MF06). Prefix `mfx`. Built on the rfx harness:
// orders are paid through the real capture path; merchants are explicit-grant principals (see the
// rfx header for the disclosure); shipments are recorded through the real HTTP handler.
//
// Order factories for the refused source states (each a distinct store/tenant, all through product
// paths: unpaid checkout, expired-unpaid closure, late payment after closure). The only owner-pool
// fixtures are named at their call sites: srqReview (review cases), a READY->REVIEW_REQUIRED work item
// (no product path produces "CONFIRMED + REVIEW_REQUIRED work" on demand) and worker stop/start.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"livecommerce/internal/fulfillment"
	"livecommerce/internal/storefront"
)

func mfxPath(o rfxOrder) string {
	return "/v1/admin/stores/" + o.store() + "/orders/" + o.order + "/shipment"
}

func mfxJSONStr(s string) string {
	if s == "" {
		return "null"
	}
	b, _ := json.Marshal(s)
	return string(b)
}

// mfxShipBody renders the exact PUT body (all eight keys, nulls explicit). An empty string is null.
func mfxShipBody(expected int64, status, carrier, name, tracking, url, note, void string) string {
	return fmt.Sprintf(`{"expected_version":%d,"status":%q,"carrier_code":%s,"carrier_name":%s,"tracking_number":%s,"tracking_url":%s,"note":%s,"void_reason":%s}`,
		expected, status, mfxJSONStr(carrier), mfxJSONStr(name), mfxJSONStr(tracking), mfxJSONStr(url), mfxJSONStr(note), mfxJSONStr(void))
}

func mfxShip(expected int64, carrier, tracking string) string {
	return mfxShipBody(expected, "SHIPPED", carrier, "", tracking, "", "", "")
}

func mfxVoid(expected int64, reason string) string {
	return mfxShipBody(expected, "VOIDED", "", "", "", "", "", reason)
}

// mfxPut records/corrects/voids through PUT .../shipment and returns status, decoded body and the raw bytes.
func (e *rfxEnv) mfxPut(o rfxOrder, token, key, body string) (int, map[string]any, []byte) {
	hdr := map[string]string{}
	if key != "" {
		hdr["Idempotency-Key"] = key
	}
	status, raw, _ := e.call("PUT", mfxPath(o), token, hdr, body)
	return status, sraJSON(raw), raw
}

func (e *rfxEnv) mustShip(t *testing.T, o rfxOrder, expected int64, carrier, tracking string) map[string]any {
	t.Helper()
	status, out, raw := e.mfxPut(o, o.token(), t04Key("mfx-ship"), mfxShip(expected, carrier, tracking))
	if status != 200 {
		t.Fatalf("record shipment: %d %s", status, raw)
	}
	return out
}

// mfxOrdersState lists order ids of a store under a state filter.
func (e *rfxEnv) mfxOrdersState(t *testing.T, o rfxOrder, token, state string) (int, map[string]bool) {
	t.Helper()
	status, raw, _ := e.call("GET", "/v1/admin/stores/"+o.store()+"/orders?limit=100&state="+state, token, nil, "")
	ids := map[string]bool{}
	if status == 200 {
		var page struct {
			Items []map[string]any `json:"items"`
		}
		if err := json.Unmarshal(raw, &page); err != nil {
			t.Fatal(err)
		}
		for _, it := range page.Items {
			ids[it["order_id"].(string)] = true
		}
	}
	return status, ids
}

// mfxCSV downloads the unshipped export.
func (e *rfxEnv) mfxCSV(o rfxOrder, token string) (int, []byte, http.Header) {
	return e.call("GET", "/v1/admin/stores/"+o.store()+"/orders/unshipped.csv", token, nil, "")
}

func (e *rfxEnv) mfxVersions(t *testing.T, o rfxOrder) int {
	return e.count(t, `SELECT count(*) FROM fulfillment.manual_shipment_versions WHERE order_id=$1`, o.order)
}

func (e *rfxEnv) mfxAudit(t *testing.T, o rfxOrder, action string) int {
	return e.count(t, `SELECT count(*) FROM ops.audit_events WHERE tenant_id=$1 AND store_id=$2 AND action=$3`, o.s.p.f.tenantA, o.store(), action)
}

func (e *rfxEnv) mfxFulfilmentState(t *testing.T, o rfxOrder) string {
	t.Helper()
	var s string
	if err := e.f.owner.QueryRow(context.Background(), `SELECT fulfillment_state FROM checkout.orders WHERE id=$1`, o.order).Scan(&s); err != nil {
		t.Fatal(err)
	}
	return s
}

// mfxAnyRefundRows counts every provider-side effect table a shipment must never touch.
func (e *rfxEnv) mfxSideEffects(t *testing.T) [4]int {
	t.Helper()
	var c [4]int
	if err := e.f.owner.QueryRow(context.Background(), `SELECT
	 (SELECT count(*) FROM integration.operations),
	 (SELECT count(*) FROM river_payment.river_job)+(SELECT count(*) FROM river.river_job)+(SELECT count(*) FROM river_expiry.river_job),
	 (SELECT count(*) FROM payments.stripe_refunds),
	 (SELECT count(*) FROM payments.stripe_signals)`).Scan(&c[0], &c[1], &c[2], &c[3]); err != nil {
		t.Fatal(err)
	}
	return c
}

// mfxCVSOrder adds a paid PICKUP order (frozen pickup code with leading zeroes) to the store of base:
// the delivery service is switched to cvs_familymart once per store (bcCVS's steps), a new buyer
// selects an attested pickup, checkout freezes it, and the order is paid through the real capture path.
func (e *rfxEnv) mfxCVSOrder(t *testing.T, base rfxOrder, code string, converted *bool) rfxOrder {
	t.Helper()
	e.ensureStock(t, base)
	s := base.s
	q := s.p
	q.bcHarness.prepare(t, mustIssue(t, q.cqHarness.service, q.f.storeA1), []storefront.Item{{SKUID: q.stock.skus[0].ID, Quantity: 2}})
	in := q.delivery
	if !*converted {
		in.ExpectedVersion = 1
		in.DeliveryKind = "cvs_familymart"
		if _, err := dsSet(q.cqHarness, t04Key("mfx-cvs-service"), in); err != nil {
			t.Fatalf("switch the delivery service to a pickup kind: %v", err)
		}
		*converted = true
	}
	pickup, err := bdAttest(q.cqHarness, t04Key("mfx-pickup"), fulfillment.PickupInput{Kind: "cvs_familymart", Namespace: "fixture." + t04Tag(), Code: code, Name: "Synthetic convenience store", Address: "Synthetic convenience address", EvidenceRef: "synthetic only", TTLSeconds: 3600})
	if err != nil {
		t.Fatal(err)
	}
	dest, err := bdSet(q.cqHarness, t04Key("mfx-cvs-select"), storefront.DestinationInput{ExpectedVersion: q.destination.Version, CartVersion: q.bcHarness.input.CartVersion, Kind: "cvs_familymart", Country: "TW", RecipientName: "Synthetic Recipient", Phone: "+886900000002", PickupID: pickup.ID})
	if err != nil {
		t.Fatal(err)
	}
	q.destination = dest
	q.bcHarness.input.DestinationID = dest.ID
	q.bcHarness.input.ServiceVersion = 2
	hold, err := q.bcHarness.begin(t04Key("mfx-cvs-hold"))
	if err != nil {
		t.Fatal(err)
	}
	q.hold = hold
	s.p = q
	return e.pay(t, s, base.endpoint, base.secret)
}

// ---- refused source states, each in its own store -------------------------------------------

type mfxStates struct {
	draft, awaiting, cancelled, allocFailed, reviewWork, fullRefund, refundInFlight, refundHistory, conflicting rfxOrder
}

// storeFor returns a fresh store with a webhook endpoint and every permission of the fulfilment gates.
func (e *rfxEnv) storeFor(t *testing.T) rfxOrder {
	t.Helper()
	s := e.stripeStore(t)
	endpoint, secret := e.endpoint(t, s)
	o := rfxOrder{s: s, order: s.p.hold.OrderID, endpoint: endpoint, secret: secret}
	e.grant(t, o, "orders:read", "payments:refund", "fulfillment:write", "orders:export")
	return o
}

// moreOrder adds a buyer/order to the store of base (unpaid: a DRAFT hold) and tops the stock up when needed. One store
// per gate keeps the number of connection pools under the focused PG's max_connections=60: every store the harness
// seeds opens several pools that live until the test ends.
func (e *rfxEnv) moreOrder(t *testing.T, base rfxOrder) rfxOrder {
	t.Helper()
	e.ensureStock(t, base)
	s := base.s
	s.p = sstMoreHold(t, base.s.p)
	return rfxOrder{s: s, order: s.p.hold.OrderID, endpoint: base.endpoint, secret: base.secret}
}

// mfxRefusedStates builds one order per source state that MD6 refuses, all in the store of base (base's own hold is
// the DRAFT one). The worker must be running.
func (e *rfxEnv) mfxRefusedStates(t *testing.T, base rfxOrder, refunds bool) mfxStates {
	t.Helper()
	var st mfxStates
	st.draft = base
	// AWAITING_PAYMENT: a started, pinned, unpaid attempt.
	st.awaiting = e.moreOrder(t, base)
	res, _ := e.pinned(t, st.awaiting.s)
	st.awaiting.attempt = res.AttemptID
	// CANCELLED: the session expires unpaid and the poller closes the attempt.
	st.cancelled = e.moreOrder(t, base)
	res, session := e.pinned(t, st.cancelled.s)
	st.cancelled.attempt, st.cancelled.session = res.AttemptID, session
	e.fake.SetState(session, "expired", "unpaid")
	e.awaitFact(t, res.AttemptID, "CLOSED_UNPAID")
	// PAID_ALLOCATION_FAILED + REVIEW_REQUIRED work: a payment that arrives after the closure (§0.2 late payment).
	st.allocFailed = e.moreOrder(t, base)
	res, session = e.pinned(t, st.allocFailed.s)
	st.allocFailed.attempt, st.allocFailed.session = res.AttemptID, session
	e.fake.SetState(session, "expired", "unpaid")
	e.awaitFact(t, res.AttemptID, "CLOSED_UNPAID")
	e.fake.SetState(session, "complete", "paid")
	if status := e.deliver(t, base.endpoint, base.secret, sflEvent(res.AttemptID, session)); status != 200 {
		t.Fatalf("late-payment webhook answered %d", status)
	}
	e.awaitFact(t, res.AttemptID, "CAPTURED")
	e.awaitReview(t, res.AttemptID, "PAID_ALLOCATION_FAILED")
	// CONFIRMED + work REVIEW_REQUIRED (owner fixture: no product path yields it on demand).
	st.reviewWork = e.payMore(t, base)
	mustExec(t, e.f.owner, `UPDATE fulfillment.payment_work_items SET state='REVIEW_REQUIRED' WHERE order_id=$1`, st.reviewWork.order)
	// READY work + a review that appeared after capture.
	st.refundHistory = e.payMore(t, base)
	srqReview(t, e, st.refundHistory.attempt, "REFUND_HISTORY")
	st.conflicting = e.payMore(t, base)
	srqReview(t, e, st.conflicting.attempt, "CONFLICTING_REPORT")
	if refunds {
		st.fullRefund = e.payMore(t, base)
		id := e.mustRefund(t, st.fullRefund, st.fullRefund.captured, "requested_by_customer")
		e.awaitRefundFact(t, id, st.fullRefund.attempt, "SUCCEEDED")
		st.refundInFlight = e.payMore(t, base)
		e.fake.HoldNextRefund("pending", "processing")
		id = e.mustRefund(t, st.refundInFlight, st.refundInFlight.captured, "requested_by_customer")
		e.awaitRefund(t, "refund pinned", id, st.refundInFlight.attempt, 45*time.Second, `SELECT stripe_refund_id IS NOT NULL FROM payments.stripe_refunds WHERE id=$1`)
	}
	return st
}

// payInStore pays the hold order that storeFor prepared (same store, same endpoint).
func (e *rfxEnv) payInStore(t *testing.T, o rfxOrder) rfxOrder {
	t.Helper()
	paid := e.pay(t, o.s, o.endpoint, o.secret)
	return paid
}

func (st mfxStates) all() map[string]rfxOrder {
	m := map[string]rfxOrder{"draft": st.draft, "awaiting_payment": st.awaiting, "cancelled": st.cancelled, "paid_allocation_failed": st.allocFailed,
		"work REVIEW_REQUIRED": st.reviewWork, "READY + REFUND_HISTORY": st.refundHistory, "READY + CONFLICTING_REPORT": st.conflicting}
	if st.fullRefund.order != "" {
		m["full refund succeeded"], m["full refund in flight"] = st.fullRefund, st.refundInFlight
	}
	return m
}

func mfxContains(raw []byte, needles ...string) bool {
	for _, n := range needles {
		if strings.Contains(string(raw), n) {
			return true
		}
	}
	return false
}
