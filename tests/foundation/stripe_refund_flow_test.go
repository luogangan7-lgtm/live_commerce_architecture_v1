package foundation_test

// RF05, RF06, RF07 (contracts/stripe-refund-v1.md §9): MOCK tier. Prefix `srf`.
//
// Real PG 18 (isolated container), real River, the SAME assembly functions the binaries call
// (payments.NewStripeRuntime + payments.NewPaymentWorkerClient, stripewebhook.NewInbox/NewHandler),
// the merchant HTTP handler, and the independent stripetest fake. Paid orders come from the real
// capture path (SP08 flow); nothing financial is fabricated.
//
// Disclosed controlled fixtures (evidence rule "clock aging disclosed"):
//   - ageRefund / srfSetTimes: owner-pool UPDATE of payments.stripe_refunds timestamps under
//     session_replication_role=replica so the 20 h resend window and the 15 min list gap can be
//     crossed without sleeping. The fake's own state is never aged (its refunds have no deadlines).
//   - owner-inserted review case is NOT used here; every review below is produced by the product.
//   - direct payments.apply_capture(attempt, hash) calls as the worker role replay an observation
//     the product already recorded (what a re-delivered reconcile job does), to prove
//     order-independent convergence (A1 received_at rules).
//
// No test sleeps for a Stripe deadline; waits are bounded polls that also make snoozed River jobs due.

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"livecommerce/internal/integrations/psp/stripe"
	"livecommerce/internal/integrations/psp/stripe/stripetest"
)

type srfEnv struct {
	*rfxEnv
	base rfxOrder
}

// srfNew builds the environment with one Stripe store, a registered webhook endpoint and a running
// payment worker, then pays the first order. Faults are set AFTER an order is paid (a checkout
// session create would otherwise consume a one-shot fault meant for the refund POST).
func srfNew(t *testing.T) *srfEnv {
	t.Helper()
	e := rfxNew(t)
	a := e.stripeStore(t)
	endpoint, secret := e.endpoint(t, a)
	e.startWorker(t)
	o := e.pay(t, a, endpoint, secret)
	e.grant(t, o, "orders:read", "payments:refund", "fulfillment:write")
	return &srfEnv{rfxEnv: e, base: o}
}

// fresh pays one more order of the same store and returns it.
func (e *srfEnv) fresh(t *testing.T) rfxOrder {
	t.Helper()
	return e.payMore(t, e.base)
}

// merchantOrder reads GET .../orders/{id} as a JSON object.
func (e *srfEnv) merchantOrder(t *testing.T, o rfxOrder) map[string]any {
	t.Helper()
	status, raw, _ := e.call("GET", "/v1/admin/stores/"+o.store()+"/orders/"+o.order, o.token(), nil, "")
	if status != 200 {
		t.Fatalf("merchant order detail answered %d: %s", status, raw)
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

// summary finds the order in the merchant list and returns its summary object.
func (e *srfEnv) summary(t *testing.T, o rfxOrder) map[string]any {
	t.Helper()
	status, raw, _ := e.call("GET", "/v1/admin/stores/"+o.store()+"/orders?limit=100", o.token(), nil, "")
	if status != 200 {
		t.Fatalf("merchant order list answered %d: %s", status, raw)
	}
	var page struct {
		Items []map[string]any `json:"items"`
	}
	if err := json.Unmarshal(raw, &page); err != nil {
		t.Fatal(err)
	}
	for _, it := range page.Items {
		if it["order_id"] == o.order {
			return it
		}
	}
	t.Fatalf("order %s not in the merchant list", o.order)
	return nil
}

func (e *srfEnv) wantMoney(t *testing.T, o rfxOrder, paymentState string, refunded, pending int64) {
	t.Helper()
	sum := e.summary(t, o)
	if sum["payment_state"] != paymentState || sum["refunded_minor"] != float64(refunded) || sum["refund_pending_minor"] != float64(pending) {
		t.Fatalf("merchant summary payment_state=%v refunded=%v pending=%v, want %s/%d/%d", sum["payment_state"], sum["refunded_minor"], sum["refund_pending_minor"], paymentState, refunded, pending)
	}
	det := e.merchantOrder(t, o)
	if det["payment_state"] != paymentState || det["refunded_minor"] != float64(refunded) || det["refund_pending_minor"] != float64(pending) {
		t.Fatalf("merchant detail payment_state=%v refunded=%v pending=%v, want %s/%d/%d", det["payment_state"], det["refunded_minor"], det["refund_pending_minor"], paymentState, refunded, pending)
	}
}

func (e *srfEnv) wantList(t *testing.T, o rfxOrder, refunded, pending, refundable int64) {
	t.Helper()
	l, _ := e.list(t, o, o.token())
	if l["captured_minor"] != float64(o.captured) || l["refunded_minor"] != float64(refunded) || l["pending_minor"] != float64(pending) || l["refundable_minor"] != float64(refundable) {
		t.Fatalf("refund list numbers: %v want refunded=%d pending=%d refundable=%d", l, refunded, pending, refundable)
	}
}

func (e *srfEnv) failureReason(t *testing.T, refund, kind string) string {
	t.Helper()
	var reason *string
	if err := e.f.owner.QueryRow(context.Background(), `SELECT failure_reason FROM payments.refund_facts WHERE refund_id=$1 AND kind=$2`, refund, kind).Scan(&reason); err != nil || reason == nil {
		t.Fatalf("fact %s of %s: %v", kind, refund, err)
	}
	return *reason
}

// keysOf returns the fake's create keys that belong to one refund.
func (e *srfEnv) keysOf(refund string) int { return e.keysOfRefund(refund) }

// assertSingleKeyPerRefund is the fake-side proof of "at most one distinct create key per refund forever":
// every create key the fake ever saw is the key of a refund an accepted request created (a key minted
// for anything else, or a second key for one refund, would show up as an unknown key), and no accepted
// refund is missing when it was sent.
func (e *srfEnv) assertSingleKeyPerRefund(t *testing.T, extra ...string) {
	t.Helper()
	known := map[string]bool{}
	e.mu.Lock()
	for _, r := range append(append([]string(nil), e.requested...), extra...) {
		known[stripe.RefundIdempotencyKey(r)] = true
	}
	e.mu.Unlock()
	for k := range e.distinctCreateKeys() {
		if !known[k] {
			t.Fatalf("create key %q does not belong to a requested refund (a new key was minted)", k)
		}
	}
}

// reports returns the parsed refund/charge observation reports of one attempt.
func (e *rfxEnv) reports(t *testing.T, attempt, object string) []map[string]any {
	t.Helper()
	rows, err := e.f.owner.Query(context.Background(), `SELECT report::text FROM payments.provider_observations WHERE attempt_id=$1 AND report->>'Object'=$2 ORDER BY received_at`, attempt, object)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []map[string]any
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			t.Fatal(err)
		}
		if len(raw) > 2048 {
			t.Fatalf("%s report is %d bytes, §3.1 caps it at 2048", object, len(raw))
		}
		for _, s := range []string{stripetest.SentinelARN, stripetest.SentinelReceiptURL, stripetest.SentinelEmail, "destination_details", "receipt_url"} {
			if strings.Contains(raw, s) {
				t.Fatalf("%s report stores %q: %s", object, s, raw)
			}
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(raw), &m); err != nil {
			t.Fatal(err)
		}
		out = append(out, m)
	}
	return out
}

func srfKeys(m map[string]any) (out []string) {
	for k := range m {
		out = append(out, k)
	}
	return
}

// assertReportShape checks exact key sets of §3.1: required keys present, only allowed keys present.
func srfAssertReportShape(t *testing.T, m map[string]any, required, optional []string) {
	t.Helper()
	allowed := map[string]bool{}
	for _, k := range required {
		allowed[k] = true
		if _, ok := m[k]; !ok {
			t.Fatalf("report lacks %q: %v", k, srfKeys(m))
		}
	}
	for _, k := range optional {
		allowed[k] = true
	}
	for k := range m {
		if !allowed[k] {
			t.Fatalf("report has unexpected key %q", k)
		}
	}
	if m["Provider"] != "stripe" || m["Version"] != float64(1) {
		t.Fatalf("report Provider/Version: %v %v", m["Provider"], m["Version"])
	}
}

var (
	srfRefundRequired = []string{"Provider", "Version", "Object", "Via", "AccountID", "KeyVersion", "RequestID", "SendCount", "RefundRef", "AttemptRef", "RefundID", "Status",
		"FailureReason", "PendingReason", "Currency", "PaymentIntentID", "Livemode", "MetadataRefund", "MetadataAttempt", "ErrorClass", "ErrorCode", "HTTPStatus", "LocalReason"}
	srfRefundOptional = []string{"Amount", "ListMatchCount"}
	srfChargeRequired = []string{"Provider", "Version", "Object", "Via", "AccountID", "KeyVersion", "RequestID", "RefundRef", "PaymentIntentID", "ChargeID", "Currency", "Refunded", "Disputed", "Livemode", "LocalReason"}
	srfChargeOptional = []string{"AmountCaptured", "AmountRefunded"}
)

func TestStripeRF05HappyMock(t *testing.T) {
	e := srfNew(t)

	t.Run("full refund: REQUESTED -> SUCCEEDED -> REFUNDED, one key, pre-send read, no side effects", func(t *testing.T) {
		o := e.base
		before := e.fingerprint(t, o)
		stockBefore := e.stock(t, o.s) // store-wide balance + this order's state: relative, other orders share the SKU
		if stockBefore.order != "CONFIRMED" || stockBefore.reservation != "COMMITTED" {
			t.Fatalf("fixture: %+v", stockBefore)
		}
		id := e.mustRefund(t, o, o.captured, "requested_by_customer")
		e.awaitRefundFact(t, id, o.attempt, "SUCCEEDED")
		if st := e.itemState(t, o, id); st != "SUCCEEDED" {
			t.Fatalf("merchant state %s", st)
		}
		e.wantList(t, o, o.captured, 0, 0)
		e.wantMoney(t, o, "REFUNDED", o.captured, 0)
		// Provider side: exactly one refund, our metadata, one create key.
		fakeID := e.fake.RefundByRef(id)
		if fakeID == "" || len(e.fake.RefundIDs(o.pi)) != 1 {
			t.Fatalf("provider refunds: %v", e.fake.RefundIDs(o.pi))
		}
		if e.keysOf(id) < 1 {
			t.Fatal("no create with the refund key")
		}
		e.assertSingleKeyPerRefund(t)
		e.assertPreSendReads(t)
		var pinned, ref string
		var amount int64
		var cur string
		if err := e.f.owner.QueryRow(context.Background(), `SELECT r.stripe_refund_id,op.provider_reference,f.amount_minor,f.currency FROM payments.stripe_refunds r
		  JOIN integration.operations op ON op.id=r.id JOIN payments.refund_facts f ON f.refund_id=r.id AND f.kind='SUCCEEDED' WHERE r.id=$1`, id).Scan(&pinned, &ref, &amount, &cur); err != nil {
			t.Fatal(err)
		}
		if pinned != fakeID || ref != fakeID || amount != o.captured || cur != "TWD" {
			t.Fatalf("pin=%s op.provider_reference=%s fact=%d %s (fake %s)", pinned, ref, amount, cur, fakeID)
		}
		e.awaitRefundCode(t, id, o.attempt, "stripe_refund_terminal")
		// Stock and order state are untouched (RD6); the fingerprint covers ledger, balances,
		// reservations, order rows, work items and capture facts.
		e.assertUnchanged(t, o, before, "a full refund")
		e.wantStock(t, o.s, stockBefore)
		var commercial, fulfillment string
		if err := e.f.owner.QueryRow(context.Background(), `SELECT commercial_state,fulfillment_state FROM checkout.orders WHERE id=$1`, o.order).Scan(&commercial, &fulfillment); err != nil || commercial != "CONFIRMED" || fulfillment != "MANUAL_UNASSIGNED" {
			t.Fatalf("order state after refund: %s/%s %v", commercial, fulfillment, err)
		}
		if n := e.count(t, `SELECT count(*) FROM fulfillment.payment_work_items WHERE attempt_id=$1 AND state='READY'`, o.attempt); n != 1 {
			t.Fatalf("work item changed: %d", n)
		}
		// Buyer view: REFUNDED with amounts only.
		v := e.view(t, o.s)
		if v.PaymentState != "REFUNDED" || v.Refund == nil || v.Refund.RefundedMinor != o.captured || v.Refund.PendingMinor != 0 {
			t.Fatalf("buyer view: %+v refund=%+v", v, v.Refund)
		}
		raw, _ := json.Marshal(v)
		for _, s := range []string{fakeID, id, "re_test", "stripe_refund_id", "failure_reason"} {
			if strings.Contains(string(raw), s) {
				t.Fatalf("buyer view leaks %q: %s", s, raw)
			}
		}
		// Reports: exact §3.1 key sets, <=2048 bytes, no ARN/receipt/billing.
		refundReports := e.reports(t, o.attempt, "refund")
		if len(refundReports) == 0 {
			t.Fatal("no refund observation recorded")
		}
		sawCreate := false
		for _, r := range refundReports {
			srfAssertReportShape(t, r, srfRefundRequired, srfRefundOptional)
			if r["RefundRef"] != id || r["MetadataRefund"] != id || r["MetadataAttempt"] != o.attempt || r["PaymentIntentID"] != o.pi || r["Livemode"] != false || r["AccountID"] != o.s.account {
				t.Fatalf("refund report identity: %v", r)
			}
			if r["Via"] == "create" {
				sawCreate = true
				if r["Status"] != "succeeded" || r["RefundID"] != fakeID || r["SendCount"] != float64(1) {
					t.Fatalf("create report: %v", r)
				}
			}
		}
		if !sawCreate {
			t.Fatal("no Via=create refund report")
		}
		sawPresend := false
		for _, r := range e.reports(t, o.attempt, "charge") {
			srfAssertReportShape(t, r, srfChargeRequired, srfChargeOptional)
			if r["Via"] == "presend" {
				sawPresend = true
				if r["RefundRef"] != id || r["PaymentIntentID"] != o.pi {
					t.Fatalf("presend report: %v", r)
				}
			}
		}
		if !sawPresend {
			t.Fatal("no Via=presend charge report for the refund (RD9)")
		}
	})

	t.Run("two partial refunds: PARTIALLY_REFUNDED then REFUNDED", func(t *testing.T) {
		o := e.fresh(t)
		e.grant(t, o, "orders:read", "payments:refund")
		before := e.fingerprint(t, o)
		stockBefore := e.stock(t, o.s)
		first := e.mustRefund(t, o, 1000, "requested_by_customer")
		e.awaitRefundFact(t, first, o.attempt, "SUCCEEDED")
		e.wantMoney(t, o, "PARTIALLY_REFUNDED", 1000, 0)
		e.wantList(t, o, 1000, 0, 1500)
		v := e.view(t, o.s)
		if v.PaymentState != "PARTIALLY_REFUNDED" || v.Refund == nil || v.Refund.RefundedMinor != 1000 {
			t.Fatalf("buyer view after first partial: %+v %+v", v, v.Refund)
		}
		second := e.mustRefund(t, o, 1500, "duplicate")
		e.awaitRefundFact(t, second, o.attempt, "SUCCEEDED")
		e.wantMoney(t, o, "REFUNDED", 2500, 0)
		e.wantList(t, o, 2500, 0, 0)
		if first == second || e.keysOf(first) < 1 || e.keysOf(second) < 1 || stripeKey(first) == stripeKey(second) {
			t.Fatal("each refund needs its own key")
		}
		if len(e.fake.RefundIDs(o.pi)) != 2 {
			t.Fatalf("provider refunds: %v", e.fake.RefundIDs(o.pi))
		}
		// The second refund's presend check sees Stripe's amount_refunded (1000) = what we sent: not an
		// external refund, so no suppression and no REFUND_HISTORY.
		if e.hasAttemptReview(t, o.attempt, "REFUND_HISTORY") {
			t.Fatal("our own first refund was mistaken for an external refund")
		}
		e.assertSingleKeyPerRefund(t)
		e.assertPreSendReads(t)
		e.assertUnchanged(t, o, before, "two partial refunds")
		e.wantStock(t, o.s, stockBefore)
	})
}

func stripeKey(refund string) string { return stripe.RefundIdempotencyKey(refund) }

func TestStripeRF06Unknown(t *testing.T) {
	e := srfNew(t)
	var refunds []string
	// scenario: pays a fresh order, arms the fault, requests 1000 and returns the refund and order.
	scenario := func(t *testing.T, fault *stripetest.Fault) (string, rfxOrder) {
		t.Helper()
		o := e.fresh(t)
		e.grant(t, o, "orders:read", "payments:refund")
		if fault != nil {
			e.fake.SetNextFault(*fault)
		}
		id := e.mustRefund(t, o, 1000, "requested_by_customer")
		refunds = append(refunds, id)
		return id, o
	}

	t.Run("drop after execute: the same-key replay pins the same refund", func(t *testing.T) {
		id, o := scenario(t, &stripetest.Fault{DropAfterExecute: true})
		e.awaitRefundFact(t, id, o.attempt, "SUCCEEDED")
		if got := e.fake.RefundIDs(o.pi); len(got) != 1 {
			t.Fatalf("provider refunds %v: a resend must replay, never create again", got)
		}
		if e.keysOf(id) < 2 {
			t.Fatalf("the dropped send was not followed by a same-key resend (%d POSTs)", e.keysOf(id))
		}
		var pinned string
		if err := e.f.owner.QueryRow(context.Background(), `SELECT stripe_refund_id FROM payments.stripe_refunds WHERE id=$1`, id).Scan(&pinned); err != nil || pinned != e.fake.RefundByRef(id) {
			t.Fatalf("pin %q vs provider %q: %v", pinned, e.fake.RefundByRef(id), err)
		}
	})

	t.Run("cached 500: resends replay the 500 until resend_until, then the list pins the executed refund", func(t *testing.T) {
		id, o := scenario(t, &stripetest.Fault{Cached500: true})
		e.awaitSend(t, id, o.attempt, 3) // the first POST plus at least two same-key resends that replay the cached 500
		var pin *string
		if err := e.f.owner.QueryRow(context.Background(), `SELECT stripe_refund_id FROM payments.stripe_refunds WHERE id=$1`, id).Scan(&pin); err != nil || pin != nil {
			t.Fatalf("a cached 500 must not pin: %v %v", pin, err)
		}
		if e.hasRefundFact(t, id, "SUCCEEDED") || e.hasRefundFact(t, id, "REJECTED") {
			t.Fatal("a fact exists while the send is unresolved")
		}
		e.wantList(t, o, 0, 1000, 1500) // held while unknown (RD4)
		postsBefore := e.fake.RefundPosts()
		e.ageRefund(t, id, 21*time.Hour) // past resend_until; disclosed fixture
		e.awaitRefundFact(t, id, o.attempt, "SUCCEEDED")
		if got := e.fake.RefundPosts(); got != postsBefore {
			t.Fatalf("POSTs after the resend window: %d -> %d (must list, never POST again)", postsBefore, got)
		}
		if got := e.fake.RefundIDs(o.pi); len(got) != 1 {
			t.Fatalf("provider refunds %v", got)
		}
		if st := e.itemState(t, o, id); st != "SUCCEEDED" {
			t.Fatalf("state %s", st)
		}
	})

	t.Run("cached 500 with nothing executed: list finds none, UNKNOWN + REFUND_UNRESOLVED, capacity held, no second POST", func(t *testing.T) {
		id, o := scenario(t, &stripetest.Fault{Cached500NoSession: true})
		e.awaitSend(t, id, o.attempt, 2)
		if e.fake.RefundByRef(id) != "" {
			t.Fatal("fixture: nothing may have executed")
		}
		postsBefore := e.fake.RefundPosts()
		e.ageRefund(t, id, 21*time.Hour) // last send is now > 15 min before now: the list gap is satisfied
		e.awaitRefund(t, "REFUND_UNRESOLVED review", id, o.attempt, 45*time.Second, `SELECT EXISTS(SELECT 1 FROM payments.review_cases c JOIN payments.stripe_refunds r ON r.attempt_id=c.attempt_id WHERE r.id=$1 AND c.reason='REFUND_UNRESOLVED')`)
		if got := e.fake.RefundPosts(); got != postsBefore {
			t.Fatalf("a second POST after the window: %d -> %d", postsBefore, got)
		}
		if n := e.count(t, `SELECT count(*) FROM payments.refund_facts WHERE refund_id=$1`, id); n != 0 {
			t.Fatalf("UNKNOWN wrote %d facts", n)
		}
		if st := e.itemState(t, o, id); st != "UNKNOWN" {
			t.Fatalf("merchant state %q, want UNKNOWN (\"Needs support\")", st)
		}
		e.wantList(t, o, 0, 1000, 1500) // capacity stays held: never released by a timeout (RD4)
		// The sticky review blocks any further refund of this attempt.
		status, out := e.request(o, o.token(), t04Key("srf-unres"), rfxBody(500, "requested_by_customer", 1500))
		if status != 422 || srqCode(out) != "refund_blocked_review" {
			t.Fatalf("request after REFUND_UNRESOLVED: %d %v", status, out)
		}
		e.wantMoney(t, o, "REVIEW_REQUIRED", 0, 1000)
	})

	t.Run("first-send 400: REJECTED and capacity released", func(t *testing.T) {
		id, o := scenario(t, &stripetest.Fault{Validation: true})
		e.awaitRefundFact(t, id, o.attempt, "REJECTED")
		if r := e.failureReason(t, id, "REJECTED"); r != "first_send_rejected" {
			t.Fatalf("reason %q", r)
		}
		if e.fake.RefundByRef(id) != "" {
			t.Fatal("a rejected create left a provider refund")
		}
		if st := e.itemState(t, o, id); st != "REJECTED" {
			t.Fatalf("state %s", st)
		}
		e.wantList(t, o, 0, 0, 2500)
		if e.hasAttemptReview(t, o.attempt, "REFUND_UNRESOLVED") {
			t.Fatal("a definitive first-send rejection opened a review")
		}
		// Released capacity is immediately usable again.
		if status, out := e.request(o, o.token(), t04Key("srf-retry"), rfxBody(2500, "requested_by_customer", 2500)); status != 201 {
			t.Fatalf("re-request after a rejection: %d %v", status, out)
		} else {
			refunds = append(refunds, out["refund_id"].(string))
			e.awaitRefundFact(t, out["refund_id"].(string), o.attempt, "SUCCEEDED")
		}
	})

	t.Run("400 after an earlier send does not release capacity", func(t *testing.T) {
		id, o := scenario(t, &stripetest.Fault{RateLimit: true}) // first send: 429, no execution
		e.awaitSend(t, id, o.attempt, 1)
		for e.fake.FaultPending() {
			time.Sleep(20 * time.Millisecond)
		}
		e.fake.SetNextFault(stripetest.Fault{Validation: true}) // the resend (same key) is answered 400
		postsAt := e.keysOf(id)
		deadline := time.Now().Add(40 * time.Second)
		for e.keysOf(id) < postsAt+1 || e.fake.FaultPending() {
			if time.Now().After(deadline) {
				t.Fatal("resend never reached the provider")
			}
			e.wakeRefund(t, id, o.attempt)
			time.Sleep(200 * time.Millisecond)
		}
		time.Sleep(500 * time.Millisecond)
		if e.hasRefundFact(t, id, "REJECTED") {
			t.Fatal("a 400 on a RESEND was treated as a definitive rejection (I06/I20: only the first send is definitive)")
		}
		e.wantList(t, o, 0, 1000, 1500)
		// The refund is still driven to completion with the same key afterwards.
		e.awaitRefundFact(t, id, o.attempt, "SUCCEEDED")
		if len(e.fake.RefundIDs(o.pi)) != 1 {
			t.Fatalf("provider refunds %v", e.fake.RefundIDs(o.pi))
		}
	})

	t.Run("429 backs off; 409 and idempotency_error keep the same key", func(t *testing.T) {
		o := e.fresh(t)
		e.grant(t, o, "orders:read", "payments:refund")
		e.fake.SetNextFault(stripetest.Fault{RateLimit: true})
		t0 := time.Now()
		id := e.mustRefund(t, o, 500, "requested_by_customer")
		refunds = append(refunds, id)
		// pollRefund always binds the refund id as $1, so the condition must consume it ("SELECT true" with
		// one argument is a pgx error and never becomes true). mark_sent SEND commits first_sent_at pre-POST.
		e.pollRefund(t, "first send", id, 20*time.Second, `SELECT first_sent_at IS NOT NULL FROM payments.stripe_refunds WHERE id=$1`)
		e.pollRefund(t, "operation event stripe_rate_limited", id, 30*time.Second, `SELECT EXISTS(SELECT 1 FROM integration.operation_events WHERE operation_id=$1 AND reason_code='stripe_rate_limited')`)
		first := e.keysOf(id)
		// No wake here: the natural backoff (5 s first step) is the assertion.
		deadline := time.Now().Add(40 * time.Second)
		for e.keysOf(id) == first {
			if time.Now().After(deadline) {
				t.Fatal("no retry after a 429")
			}
			time.Sleep(100 * time.Millisecond)
		}
		if gap := time.Since(t0); gap < 4*time.Second {
			t.Fatalf("retry after a 429 came after %v: backoff missing (5 s first step)", gap)
		}
		e.awaitRefundFact(t, id, o.attempt, "SUCCEEDED")

		e.fake.SetNextFault(stripetest.Fault{Conflict: true})
		id2 := e.mustRefund(t, o, 500, "requested_by_customer")
		refunds = append(refunds, id2)
		e.awaitRefundFact(t, id2, o.attempt, "SUCCEEDED")
		if e.keysOf(id2) < 2 {
			t.Fatal("409 was not retried with the same key")
		}

		e.fake.SetNextFault(stripetest.Fault{IdempotencyError: true})
		id3 := e.mustRefund(t, o, 500, "requested_by_customer")
		refunds = append(refunds, id3)
		e.awaitRefundCode(t, id3, o.attempt, "stripe_idempotency_alarm")
		if e.hasRefundFact(t, id3, "REJECTED") {
			t.Fatal("idempotency_error released capacity")
		}
		e.awaitRefundFact(t, id3, o.attempt, "SUCCEEDED") // alarm, then the same key succeeds
		if got := len(e.fake.RefundIDs(o.pi)); got != 3 {
			t.Fatalf("provider refunds: %d", got)
		}
	})

	t.Run("at most one distinct create key per refund, ever", func(t *testing.T) {
		e.assertSingleKeyPerRefund(t, refunds...)
		for _, id := range refunds {
			if e.keysOf(id) < 1 {
				t.Fatalf("refund %s never used its key", id)
			}
		}
		e.assertPreSendReads(t)
	})
}

// srfSetTimes sets the four linked timestamps of one refund (owner fixture, replica role) so a test can
// place "last send" arbitrarily close to resend_until. Disclosed like ageRefund.
func srfSetTimes(t *testing.T, e *rfxEnv, refund, requestedAgo, lastSentBeforeResendUntil string) {
	t.Helper()
	ctx := context.Background()
	tx, err := e.f.owner.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `SET LOCAL session_replication_role = replica`); err != nil {
		t.Fatal(err)
	}
	// One instant for every column: clock_timestamp() differs per call, which breaks the §4.2 CHECK
	// resend_until = requested_at + 20 hours by microseconds.
	if _, err := tx.Exec(ctx, `UPDATE payments.stripe_refunds SET requested_at=statement_timestamp()-$2::interval,
	 resend_until=statement_timestamp()-$2::interval+interval '20 hours',
	 first_sent_at=statement_timestamp()-$2::interval+interval '1 minute',
	 last_sent_at=statement_timestamp()-$2::interval+interval '20 hours'-$3::interval WHERE id=$1`, refund, requestedAgo, lastSentBeforeResendUntil); err != nil {
		t.Fatalf("set times: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestStripeRF07Lifecycle(t *testing.T) {
	e := srfNew(t)
	ctx := context.Background()

	t.Run("pending (insufficient balance) then succeeded", func(t *testing.T) {
		o := e.fresh(t)
		e.grant(t, o, "orders:read", "payments:refund")
		e.fake.HoldNextRefund("pending", "insufficient_funds")
		id := e.mustRefund(t, o, 1000, "requested_by_customer")
		e.awaitRefund(t, "refund pinned", id, o.attempt, 45*time.Second, `SELECT stripe_refund_id IS NOT NULL FROM payments.stripe_refunds WHERE id=$1`)
		if st := e.itemState(t, o, id); st != "PENDING" {
			t.Fatalf("state %s, want PENDING", st)
		}
		// A pending refund holds capacity and does NOT change payment_state (RD7).
		e.wantList(t, o, 0, 1000, 1500)
		e.wantMoney(t, o, "CAPTURED", 0, 1000)
		if e.hasRefundFact(t, id, "SUCCEEDED") {
			t.Fatal("pending produced a SUCCEEDED fact")
		}
		if v := e.view(t, o.s); v.PaymentState != "CAPTURED" || v.Refund == nil || v.Refund.PendingMinor != 1000 || v.Refund.RefundedMinor != 0 {
			t.Fatalf("buyer view while pending: %+v %+v", v, v.Refund)
		}
		e.fake.SetRefundStatus(e.fake.RefundByRef(id), "succeeded", "")
		e.awaitRefundFact(t, id, o.attempt, "SUCCEEDED")
		e.wantMoney(t, o, "PARTIALLY_REFUNDED", 1000, 0)
	})

	t.Run("succeeded then a later refund.failed: FAILED fact, capacity released, state reverts", func(t *testing.T) {
		o := e.fresh(t)
		e.grant(t, o, "orders:read", "payments:refund")
		id := e.mustRefund(t, o, 1000, "requested_by_customer")
		e.awaitRefundFact(t, id, o.attempt, "SUCCEEDED")
		e.wantMoney(t, o, "PARTIALLY_REFUNDED", 1000, 0)
		posts := e.fake.RefundPosts()
		fakeID := e.fake.RefundByRef(id)
		e.fake.SetRefundStatus(fakeID, "failed", "declined")
		body := e.fake.RefundEventBody("evt_srf_failed_"+t04Tag(), "refund.failed", fakeID, false)
		if status := e.deliverRaw(t, o.endpoint, o.secret, body); status != 200 {
			t.Fatalf("refund.failed webhook answered %d", status)
		}
		e.awaitRefundFact(t, id, o.attempt, "FAILED")
		if r := e.failureReason(t, id, "FAILED"); r != "declined" {
			t.Fatalf("failure reason %q", r)
		}
		if !e.hasRefundFact(t, id, "SUCCEEDED") {
			t.Fatal("the SUCCEEDED fact must stay (append-only)")
		}
		if st := e.itemState(t, o, id); st != "FAILED" {
			t.Fatalf("state %s", st)
		}
		e.wantList(t, o, 0, 0, 2500)
		e.wantMoney(t, o, "CAPTURED", 0, 0)
		if e.fake.RefundPosts() != posts {
			t.Fatal("a webhook caused a POST /v1/refunds")
		}
		if e.hasAttemptReview(t, o.attempt, "REFUND_CONFLICTING") || e.hasAttemptReview(t, o.attempt, "REFUND_HISTORY") {
			t.Fatal("a late failure opened a review")
		}
		// The released amount is refundable again.
		if status, out := e.request(o, o.token(), t04Key("srf-again"), rfxBody(2500, "requested_by_customer", 2500)); status != 201 {
			t.Fatalf("re-request after failure: %d %v", status, out)
		}
	})

	t.Run("canceled before success releases capacity", func(t *testing.T) {
		o := e.fresh(t)
		e.grant(t, o, "orders:read", "payments:refund")
		e.fake.HoldNextRefund("pending", "processing")
		id := e.mustRefund(t, o, 1000, "requested_by_customer")
		e.awaitRefund(t, "refund pinned", id, o.attempt, 45*time.Second, `SELECT stripe_refund_id IS NOT NULL FROM payments.stripe_refunds WHERE id=$1`)
		e.fake.SetRefundStatus(e.fake.RefundByRef(id), "canceled", "merchant_request")
		e.awaitRefundFact(t, id, o.attempt, "CANCELED")
		if r := e.failureReason(t, id, "CANCELED"); r != "merchant_request" {
			t.Fatalf("reason %q", r)
		}
		if st := e.itemState(t, o, id); st != "CANCELED" {
			t.Fatalf("state %s", st)
		}
		e.wantList(t, o, 0, 0, 2500)
		e.wantMoney(t, o, "CAPTURED", 0, 0)
	})

	t.Run("every failure reason is mapped", func(t *testing.T) {
		o := e.fresh(t)
		e.grant(t, o, "orders:read", "payments:refund")
		for _, reason := range []string{"lost_or_stolen_card", "expired_or_canceled_card", "charge_for_pending_refund_disputed", "insufficient_funds", "declined", "merchant_request", "unknown"} {
			e.fake.HoldNextRefund("pending", "processing")
			id := e.mustRefund(t, o, 100, "requested_by_customer")
			e.awaitRefund(t, "refund pinned", id, o.attempt, 45*time.Second, `SELECT stripe_refund_id IS NOT NULL FROM payments.stripe_refunds WHERE id=$1`)
			e.fake.SetRefundStatus(e.fake.RefundByRef(id), "failed", reason)
			e.awaitRefundFact(t, id, o.attempt, "FAILED")
			if got := e.failureReason(t, id, "FAILED"); got != reason {
				t.Fatalf("failure_reason %q stored as %q", reason, got)
			}
			l, _ := e.list(t, o, o.token())
			for _, it := range l["items"].([]any) {
				m := it.(map[string]any)
				if m["refund_id"] == id && m["failure_reason"] != reason {
					t.Fatalf("merchant list failure_reason %v for %s", m["failure_reason"], reason)
				}
			}
		}
		e.wantList(t, o, 0, 0, 2500)
	})

	t.Run("amount and currency drift: review, no fact", func(t *testing.T) {
		for name, patch := range map[string]map[string]any{"amount": {"amount": 900}, "currency": {"currency": "usd"}} {
			o := e.fresh(t)
			e.grant(t, o, "orders:read", "payments:refund")
			e.fake.HoldNextRefund("pending", "processing")
			id := e.mustRefund(t, o, 1000, "requested_by_customer")
			e.awaitRefund(t, "refund pinned", id, o.attempt, 45*time.Second, `SELECT stripe_refund_id IS NOT NULL FROM payments.stripe_refunds WHERE id=$1`)
			e.fake.PatchRefund(e.fake.RefundByRef(id), patch)
			e.fake.SetRefundStatus(e.fake.RefundByRef(id), "succeeded", "")
			e.awaitRefund(t, "REFUND_AMOUNT_MISMATCH review", id, o.attempt, 45*time.Second, `SELECT EXISTS(SELECT 1 FROM payments.review_cases c JOIN payments.stripe_refunds r ON r.attempt_id=c.attempt_id WHERE r.id=$1 AND c.reason='REFUND_AMOUNT_MISMATCH')`)
			if n := e.count(t, `SELECT count(*) FROM payments.refund_facts WHERE refund_id=$1`, id); n != 0 {
				t.Fatalf("%s drift produced %d facts (I05)", name, n)
			}
			e.wantMoney(t, o, "REVIEW_REQUIRED", 0, 1000)
			if name == "currency" {
				// Ruling 23: BOTH effects. The job ENDS as stripe_refund_mismatch (op result_code + River job
				// cancelled, so no poll and no resend) AND the review above is open; capacity stays reserved
				// (wantMoney: pending 1000, nothing refunded).
				e.awaitRefund(t, "op ended stripe_refund_mismatch", id, o.attempt, 45*time.Second, `SELECT EXISTS(SELECT 1 FROM integration.operations WHERE id=$1 AND state='UNKNOWN' AND result_code='stripe_refund_mismatch')`)
				e.awaitRefund(t, "refund job cancelled", id, o.attempt, 45*time.Second, `SELECT EXISTS(SELECT 1 FROM river_payment.river_job WHERE kind='payment_refund_v1' AND args->>'operation_id'=$1 AND state='cancelled')
				  AND NOT EXISTS(SELECT 1 FROM river_payment.river_job WHERE kind='payment_refund_v1' AND args->>'operation_id'=$1 AND state IN ('available','scheduled','retryable','running'))`)
				if n := e.count(t, `SELECT send_count FROM payments.stripe_refunds WHERE id=$1`, id); n != 1 {
					t.Fatalf("currency drift resent the create key: send_count %d", n)
				}
			}
		}
	})

	t.Run("replay in any order converges (A1 received_at rules)", func(t *testing.T) {
		o := e.fresh(t)
		e.grant(t, o, "orders:read", "payments:refund")
		id := e.mustRefund(t, o, 1000, "requested_by_customer")
		e.awaitRefundFact(t, id, o.attempt, "SUCCEEDED")
		fakeID := e.fake.RefundByRef(id)
		// A charge snapshot taken while A is SUCCEEDED (amount_refunded = 1000): trigger charge.refunded.
		chargeBody := e.fake.ChargeEventBody("evt_srf_charge_"+t04Tag(), o.pi, false)
		if status := e.deliverRaw(t, o.endpoint, o.secret, chargeBody); status != 200 {
			t.Fatalf("charge.refunded answered %d", status)
		}
		var chargeHash []byte
		e.awaitRefund(t, "charge snapshot", id, o.attempt, 45*time.Second, `SELECT EXISTS(SELECT 1 FROM payments.provider_observations WHERE attempt_id=(SELECT attempt_id FROM payments.stripe_refunds WHERE id=$1)
		  AND report->>'Object'='charge' AND report->>'Via'='retrieve' AND (report->>'AmountRefunded')::bigint=1000)`)
		if err := e.f.owner.QueryRow(ctx, `SELECT report_hash FROM payments.provider_observations WHERE attempt_id=$1 AND report->>'Object'='charge' AND report->>'Via'='retrieve' ORDER BY received_at DESC LIMIT 1`, o.attempt).Scan(&chargeHash); err != nil {
			t.Fatal(err)
		}
		var succeededHash []byte
		if err := e.f.owner.QueryRow(ctx, `SELECT report_hash FROM payments.provider_observations WHERE attempt_id=$1 AND report->>'Object'='refund' AND report->>'Status'='succeeded' ORDER BY received_at LIMIT 1`, o.attempt).Scan(&succeededHash); err != nil {
			t.Fatal(err)
		}
		// Now A fails at the provider and the FAILED fact lands AFTER the snapshot above.
		e.fake.SetRefundStatus(fakeID, "failed", "declined")
		if status := e.deliverRaw(t, o.endpoint, o.secret, e.fake.RefundEventBody("evt_srf_f2_"+t04Tag(), "refund.failed", fakeID, false)); status != 200 {
			t.Fatalf("refund.failed answered %d", status)
		}
		e.awaitRefundFact(t, id, o.attempt, "FAILED")
		state := func() string {
			var s string
			if err := e.f.owner.QueryRow(ctx, `SELECT coalesce((SELECT string_agg(f::text,'|' ORDER BY f::text) FROM payments.refund_facts f WHERE f.attempt_id=$1),'')||'#'||
			 coalesce((SELECT string_agg(c.reason,',' ORDER BY c.reason) FROM payments.review_cases c WHERE c.attempt_id=$1),'')`, o.attempt).Scan(&s); err != nil {
				t.Fatal(err)
			}
			return s
		}
		settled := state()
		apply := func(hash []byte) {
			t.Helper()
			if _, err := e.pool.Exec(ctx, `SELECT payments.apply_capture($1::uuid,$2::bytea)`, o.attempt, hash); err != nil {
				t.Fatalf("apply_capture replay: %v", err)
			}
		}
		// The snapshot showed 1000 refunded while A was live; A's FAILED fact is newer than the snapshot,
		// so A is NOT released "as of this report": 1000 <= 1000, no REFUND_HISTORY.
		apply(chargeHash)
		// A stale `succeeded` report replayed after the FAILED fact: no REFUND_CONFLICTING.
		apply(succeededHash)
		if e.hasAttemptReview(t, o.attempt, "REFUND_HISTORY") || e.hasAttemptReview(t, o.attempt, "REFUND_CONFLICTING") {
			t.Fatalf("out-of-order replay opened a review: %s", state())
		}
		// Replay every observation of the attempt in reverse order, twice: the end state does not move.
		rows, err := e.f.owner.Query(ctx, `SELECT report_hash FROM payments.provider_observations WHERE attempt_id=$1 ORDER BY received_at DESC, report_hash`, o.attempt)
		if err != nil {
			t.Fatal(err)
		}
		var hashes [][]byte
		for rows.Next() {
			var h []byte
			if err := rows.Scan(&h); err != nil {
				t.Fatal(err)
			}
			hashes = append(hashes, h)
		}
		rows.Close()
		for pass := 0; pass < 2; pass++ {
			for _, h := range hashes {
				apply(h)
			}
		}
		if after := state(); after != settled {
			t.Fatalf("replay did not converge:\n before %s\n after  %s", settled, after)
		}
	})

	t.Run("a review after capture and shipment never changes order or work state (A1 guard)", func(t *testing.T) {
		o := e.fresh(t)
		e.grant(t, o, "orders:read", "payments:refund", "fulfillment:write")
		// Ship it (manual fulfilment): CONFIRMED + MERCHANT_SHIPPED + READY work item.
		body := `{"expected_version":0,"status":"SHIPPED","carrier_code":"sf_express","carrier_name":null,"tracking_number":"SF0001","tracking_url":null,"note":null,"void_reason":null}`
		status, raw, _ := e.call("PUT", "/v1/admin/stores/"+o.store()+"/orders/"+o.order+"/shipment", o.token(), map[string]string{"Idempotency-Key": t04Key("srf-ship")}, body)
		if status != 200 {
			t.Fatalf("ship: %d %s", status, raw)
		}
		// A Dashboard refund appears on the charge; a charge.refunded webhook makes the product see it.
		e.fake.DashboardRefund(o.pi, 500)
		if s := e.deliverRaw(t, o.endpoint, o.secret, e.fake.ChargeEventBody("evt_srf_dash_"+t04Tag(), o.pi, false)); s != 200 {
			t.Fatalf("charge.refunded answered %d", s)
		}
		e.awaitRefund(t, "REFUND_HISTORY review", o.attempt, o.attempt, 45*time.Second, `SELECT EXISTS(SELECT 1 FROM payments.review_cases WHERE attempt_id=$1 AND reason='REFUND_HISTORY')`)
		snapshot := func() string {
			var s string
			if err := e.f.owner.QueryRow(ctx, `SELECT o.fulfillment_state||'/'||o.commercial_state||'/'||w.state FROM checkout.orders o JOIN fulfillment.payment_work_items w ON w.order_id=o.id WHERE o.id=$1`, o.order).Scan(&s); err != nil {
				t.Fatal(err)
			}
			return s
		}
		if got := snapshot(); got != "MERCHANT_SHIPPED/CONFIRMED/READY" {
			t.Fatalf("after REFUND_HISTORY: %s", got)
		}
		// Replay the capture reconcile observation: the B1 body would flip the order to PAID_ALLOCATION_FAILED.
		var captureHash []byte
		if err := e.f.owner.QueryRow(ctx, `SELECT f.source_report_hash FROM payments.facts f WHERE f.attempt_id=$1 AND f.kind='CAPTURED'`, o.attempt).Scan(&captureHash); err != nil {
			t.Fatal(err)
		}
		if _, err := e.pool.Exec(ctx, `SELECT payments.apply_capture($1::uuid,$2::bytea)`, o.attempt, captureHash); err != nil {
			t.Fatalf("replay capture: %v", err)
		}
		if got := snapshot(); got != "MERCHANT_SHIPPED/CONFIRMED/READY" {
			t.Fatalf("replaying the capture reconcile flipped order/work state: %s", got)
		}
	})

	t.Run("late first send (now >= resend_until-1h): no POST, REJECTED send_window_closed", func(t *testing.T) {
		o := e.fresh(t)
		e.grant(t, o, "orders:read", "payments:refund")
		// Hold the refund unsent: stop the worker, request, age past 19 h, restart the worker.
		e.stopAllWorkers()
		restarted := false
		defer func() {
			if !restarted {
				e.startWorker(t) // later subtests need a worker even when this one failed early
			}
		}()
		id := e.mustRefund(t, o, 1000, "requested_by_customer")
		e.ageRefund(t, id, 19*time.Hour+30*time.Minute)
		posts := e.fake.RefundPosts()
		restarted = true
		e.startWorker(t)
		e.awaitRefundFact(t, id, o.attempt, "REJECTED")
		if r := e.failureReason(t, id, "REJECTED"); r != "send_window_closed" {
			t.Fatalf("reason %q", r)
		}
		if e.fake.RefundPosts() != posts || e.fake.RefundByRef(id) != "" {
			t.Fatalf("a late first send reached the provider (POSTs %d -> %d)", posts, e.fake.RefundPosts())
		}
		e.wantList(t, o, 0, 0, 2500)
	})

	t.Run("list finds none within 15 min of the last send: snooze and list again, no REFUND_UNRESOLVED yet", func(t *testing.T) {
		o := e.fresh(t)
		e.grant(t, o, "orders:read", "payments:refund")
		e.fake.SetNextFault(stripetest.Fault{Cached500NoSession: true})
		id := e.mustRefund(t, o, 1000, "requested_by_customer")
		deadline := time.Now().Add(40 * time.Second)
		for e.keysOf(id) < 1 {
			if time.Now().After(deadline) {
				t.Fatal("never sent")
			}
			time.Sleep(100 * time.Millisecond)
		}
		e.awaitRefundCode(t, id, o.attempt, "stripe_refund_uncertain")
		// requested 20 h 1 min ago: the window closed 1 min ago; the last send was 10 s before resend_until.
		srfSetTimes(t, e.rfxEnv, id, "20 hours 1 minute", "10 seconds")
		lists := func() (n int) {
			for _, c := range e.refundCalls() {
				if c.kind == "list" {
					n++
				}
			}
			return
		}
		listsBefore := lists()
		deadline = time.Now().Add(40 * time.Second)
		for lists() == listsBefore {
			if time.Now().After(deadline) {
				t.Fatal("worker did not list after the window closed")
			}
			e.wakeRefund(t, id, o.attempt)
			time.Sleep(200 * time.Millisecond)
		}
		time.Sleep(1500 * time.Millisecond) // let the worker record the zero-match report and snooze
		if e.hasAttemptReview(t, o.attempt, "REFUND_UNRESOLVED") {
			t.Fatal("REFUND_UNRESOLVED opened within 15 min of the last send (D3)")
		}
		e.pollRefund(t, "a refund job snoozed ~15 min out (D3)", id, 10*time.Second, `SELECT EXISTS(SELECT 1 FROM river_payment.river_job WHERE kind='payment_refund_v1' AND args->>'operation_id'=$1
		  AND state IN ('scheduled','retryable') AND scheduled_at > clock_timestamp()+interval '10 minutes')`)
		// Twenty minutes later the same list-zero is conclusive.
		e.ageRefund(t, id, 20*time.Minute)
		e.awaitRefund(t, "REFUND_UNRESOLVED review", id, o.attempt, 45*time.Second, `SELECT EXISTS(SELECT 1 FROM payments.review_cases c JOIN payments.stripe_refunds r ON r.attempt_id=c.attempt_id WHERE r.id=$1 AND c.reason='REFUND_UNRESOLVED')`)
	})
}
