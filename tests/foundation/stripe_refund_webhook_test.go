package foundation_test

// RF08 (contracts/stripe-refund-v1.md §9, §4.6, §7.3): refund/charge webhooks, HTTP_PG. Prefix `srh`.
//
// The REAL ingress handler (stripewebhook.NewInbox/NewHandler behind httptest over the
// commerce_stripe_ingress pool), the REAL payment worker (signal worker + refund worker), real PG 18
// and the stripetest fake, which renders refund.* / charge.refunded payloads from its live state and
// puts ARN / receipt / billing-email sentinels into them. Orders are paid through the real capture
// path. Owner-pool fixtures: swhTrigger commit hooks (SP13's technique, test-only DDL removed at
// cleanup), worker stop/start to hold a REQUESTED refund unsent, and the DB-wide sentinel scan.

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"

	"livecommerce/internal/integrations/psp/stripe/stripetest"
)

type srhReceipt struct {
	disposition, reason, objectType string
	refundID, attemptID             *string
	hasSignal                       bool
	redelivery                      int
}

func (e *rfxEnv) srhReceipt(t *testing.T, endpoint, eventID string) (srhReceipt, bool) {
	t.Helper()
	var r srhReceipt
	err := e.f.owner.QueryRow(context.Background(), `SELECT disposition,reason,coalesce(object_type,''),refund_id::text,attempt_id::text,signal_id IS NOT NULL,redelivery_count
	 FROM payments.stripe_webhook_receipts WHERE endpoint_id=$1::uuid AND event_id=$2`, endpoint, eventID).Scan(&r.disposition, &r.reason, &r.objectType, &r.refundID, &r.attemptID, &r.hasSignal, &r.redelivery)
	return r, err == nil
}

// srhPost sends one signed body once (no 503 retry: a 503 is itself an assertion) and returns status+body.
func (e *rfxEnv) srhPost(t *testing.T, endpoint, secret string, body []byte) (int, string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, e.hook.URL+"/v1/stripe/webhook/"+endpoint, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Stripe-Signature", stripetest.SignWebhook(secret, body, time.Now()))
	res, err := (&http.Client{Timeout: 15 * time.Second}).Do(req)
	if err != nil {
		t.Fatalf("webhook request: %v", err)
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(res.Body, 1<<16))
	return res.StatusCode, strings.TrimSpace(string(raw))
}

var srhEventSeq int

func srhEvent(kind string) string {
	srhEventSeq++
	return fmt.Sprintf("evt_srh%s_%s_%d", kind, t04Tag(), srhEventSeq)
}

func (e *rfxEnv) srhCounters(t *testing.T, o rfxOrder, refund string) (session, charge, refundSignals int) {
	t.Helper()
	if err := e.f.owner.QueryRow(context.Background(), `SELECT s.signal_count,s.charge_signal_count,coalesce((SELECT signal_count FROM payments.stripe_refunds WHERE id=NULLIF($2,'')::uuid),0)
	 FROM payments.stripe_sessions s WHERE s.attempt_id=$1`, o.attempt, refund).Scan(&session, &charge, &refundSignals); err != nil {
		t.Fatal(err)
	}
	return
}

// srhSignalLink returns the signal's refund_id and the River job's operation_id for one accepted event.
func (e *rfxEnv) srhSignalLink(t *testing.T, eventID string) (refund *string, jobOperation string, outcome *string) {
	t.Helper()
	if err := e.f.owner.QueryRow(context.Background(), `SELECT s.refund_id::text,j.args->>'operation_id',s.outcome FROM payments.stripe_webhook_receipts r
	 JOIN payments.stripe_signals s ON s.id=r.signal_id JOIN river_payment.river_job j ON j.id=s.job_id WHERE r.event_id=$1`, eventID).Scan(&refund, &jobOperation, &outcome); err != nil {
		t.Fatalf("signal link of %s: %v", eventID, err)
	}
	return
}

func (e *rfxEnv) srhAwaitOutcome(t *testing.T, o rfxOrder, refund, eventID string, want string) {
	t.Helper()
	// awaitRefund binds the refund id as $1; the condition must reference it (an unreferenced $1 has no
	// type and the query errors on every poll). Charge signals carry no refund id, so it is only typed.
	e.awaitRefund(t, "signal "+eventID+" consumed", refund, o.attempt, 45*time.Second, `SELECT EXISTS(SELECT 1 FROM payments.stripe_webhook_receipts r JOIN payments.stripe_signals s ON s.id=r.signal_id
	 WHERE $1::text IS NOT NULL AND r.event_id=$2 AND s.outcome=$3)`, eventID, want)
}

func TestStripeRF08Webhook(t *testing.T) {
	logs := &swhLogBuffer{}
	oldSlog, oldLog := slog.Default(), log.Writer()
	slog.SetDefault(slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	log.SetOutput(logs)
	t.Cleanup(func() { slog.SetDefault(oldSlog); log.SetOutput(oldLog) })

	e := rfxNew(t)
	a, b := e.stripeStore(t), e.stripeStore(t)
	epA, secA := e.endpoint(t, a)
	epB, secB := e.endpoint(t, b)
	e.startWorker(t)
	oa, ob := e.pay(t, a, epA, secA), e.pay(t, b, epB, secB)
	e.grant(t, oa, "orders:read", "payments:refund")
	e.grant(t, ob, "orders:read", "payments:refund")
	refundA := e.mustRefund(t, oa, 1000, "requested_by_customer")
	refundB := e.mustRefund(t, ob, 1000, "requested_by_customer")
	e.awaitRefundFact(t, refundA, oa.attempt, "SUCCEEDED")
	e.awaitRefundFact(t, refundB, ob.attempt, "SUCCEEDED")
	fakeA := e.fake.RefundByRef(refundA)
	const ok = `{"received":true}`

	t.Run("the four new types are admitted, deduped, ACKed after commit and consumed under the refund-op lease", func(t *testing.T) {
		sessBefore, chargeBefore, refBefore := e.srhCounters(t, oa, refundA)
		postsBefore := e.fake.RefundPosts()
		ids := map[string]string{}
		for _, typ := range []string{"refund.created", "refund.updated", "refund.failed"} {
			id := srhEvent(strings.ReplaceAll(typ, ".", ""))
			ids[typ] = id
			if status, body := e.srhPost(t, epA, secA, e.fake.RefundEventBody(id, typ, fakeA, false)); status != 200 || body != ok {
				t.Fatalf("%s answered %d %q", typ, status, body)
			}
			rc, found := e.srhReceipt(t, epA, id)
			if !found || rc.disposition != "ACCEPTED" || rc.reason != "accepted" || rc.objectType != "refund" || rc.refundID == nil || *rc.refundID != refundA || rc.attemptID == nil || *rc.attemptID != oa.attempt || !rc.hasSignal {
				t.Fatalf("%s receipt: %+v found=%v", typ, rc, found)
			}
			refund, jobOp, _ := e.srhSignalLink(t, id)
			if refund == nil || *refund != refundA || jobOp != refundA {
				t.Fatalf("%s signal: refund_id=%v job operation_id=%s (the job must carry the REFUND id)", typ, refund, jobOp)
			}
			e.srhAwaitOutcome(t, oa, refundA, id, "OBSERVED")
		}
		chargeID := srhEvent("chargerefunded")
		chargesBefore := len(e.reports(t, oa.attempt, "charge"))
		if status, body := e.srhPost(t, epA, secA, e.fake.ChargeEventBody(chargeID, oa.pi, false)); status != 200 || body != ok {
			t.Fatalf("charge.refunded answered %d %q", status, body)
		}
		rc, found := e.srhReceipt(t, epA, chargeID)
		if !found || rc.disposition != "ACCEPTED" || rc.objectType != "charge" || rc.refundID != nil || rc.attemptID == nil || *rc.attemptID != oa.attempt {
			t.Fatalf("charge receipt: %+v found=%v", rc, found)
		}
		refund, jobOp, _ := e.srhSignalLink(t, chargeID)
		if refund != nil || jobOp != oa.attempt {
			t.Fatalf("charge signal is attempt-level: refund_id=%v job operation_id=%s", refund, jobOp)
		}
		// A charge signal produces a charge observation and never NOOP_TERMINAL.
		e.srhAwaitOutcome(t, oa, refundA, chargeID, "OBSERVED")
		if after := len(e.reports(t, oa.attempt, "charge")); after <= chargesBefore {
			t.Fatalf("charge.refunded produced no charge observation (%d -> %d)", chargesBefore, after)
		}
		var viaRetrieve int
		if err := e.f.owner.QueryRow(context.Background(), `SELECT count(*) FROM payments.provider_observations WHERE attempt_id=$1 AND report->>'Object'='charge' AND report->>'Via'='retrieve'`, oa.attempt).Scan(&viaRetrieve); err != nil || viaRetrieve < 1 {
			t.Fatalf("no Via=retrieve charge report: %d %v", viaRetrieve, err)
		}
		// Dedupe: the same event id again is ACKed without a second signal.
		signals := e.count(t, `SELECT count(*) FROM payments.stripe_signals WHERE attempt_id=$1`, oa.attempt)
		if status, body := e.srhPost(t, epA, secA, e.fake.RefundEventBody(ids["refund.updated"], "refund.updated", fakeA, false)); status != 200 || body != ok {
			t.Fatalf("redelivery answered %d %q", status, body)
		}
		if rc, _ := e.srhReceipt(t, epA, ids["refund.updated"]); rc.redelivery != 1 || e.count(t, `SELECT count(*) FROM payments.stripe_signals WHERE attempt_id=$1`, oa.attempt) != signals {
			t.Fatalf("dedupe: redelivery_count=%d", rc.redelivery)
		}
		// Caps: refund traffic touches only the refund's counter; charge traffic only the charge counter.
		sess, charge, ref := e.srhCounters(t, oa, refundA)
		if sess != sessBefore || charge != chargeBefore+1 || ref != refBefore+3 {
			t.Fatalf("counters session %d->%d (must not move), charge %d->%d (+1), refund %d->%d (+3)", sessBefore, sess, chargeBefore, charge, refBefore, ref)
		}
		if e.fake.RefundPosts() != postsBefore {
			t.Fatal("a webhook caused POST /v1/refunds (RD8: webhooks are wake-ups only)")
		}
	})

	t.Run("a late refund.failed after 20 partial refunds is still ACCEPTED; charge cap is independent", func(t *testing.T) {
		o := e.payMore(t, oa)
		e.grant(t, o, "orders:read", "payments:refund")
		var refunds []string
		for i := 0; i < 20; i++ {
			refunds = append(refunds, e.mustRefund(t, o, 100, "requested_by_customer"))
		}
		for _, id := range refunds {
			e.awaitRefundFact(t, id, o.attempt, "SUCCEEDED")
		}
		sessBefore, _, _ := e.srhCounters(t, o, "")
		// 20 refunds x 4 events = 80 accepted refund receipts on one attempt: more than the 64 the checkout
		// counter allows, all ACCEPTED because refund traffic has its own per-refund counter.
		for _, id := range refunds {
			fake := e.fake.RefundByRef(id)
			for _, typ := range []string{"refund.created", "refund.updated", "refund.updated", "refund.updated"} {
				eid := srhEvent("many")
				if status, body := e.srhPost(t, o.endpoint, o.secret, e.fake.RefundEventBody(eid, typ, fake, false)); status != 200 || body != ok {
					t.Fatalf("%s for %s answered %d %q", typ, id, status, body)
				}
				if rc, _ := e.srhReceipt(t, o.endpoint, eid); rc.disposition != "ACCEPTED" {
					t.Fatalf("refund receipt %s not ACCEPTED: %+v (reason %s)", eid, rc, rc.reason)
				}
			}
		}
		if sess, _, _ := e.srhCounters(t, o, ""); sess != sessBefore {
			t.Fatalf("refund traffic moved the checkout signal_count %d -> %d", sessBefore, sess)
		}
		// Exhaust the per-attempt charge cap (64): the 65th charge receipt is IGNORED signal_cap.
		accepted := 0
		for i := 0; i < 70; i++ {
			eid := srhEvent("charge")
			if status, body := e.srhPost(t, o.endpoint, o.secret, e.fake.ChargeEventBody(eid, o.pi, false)); status != 200 || body != ok {
				t.Fatalf("charge event %d answered %d %q", i, status, body)
			}
			rc, _ := e.srhReceipt(t, o.endpoint, eid)
			switch rc.disposition {
			case "ACCEPTED":
				accepted++
			case "IGNORED":
				if rc.reason != "signal_cap" {
					t.Fatalf("charge receipt ignored for %q", rc.reason)
				}
			default:
				t.Fatalf("charge receipt: %+v", rc)
			}
		}
		if _, charge, _ := e.srhCounters(t, o, ""); charge != 64 || accepted != 64 {
			t.Fatalf("charge_signal_count=%d accepted=%d, want 64/64", charge, accepted)
		}
		// The late failure of the last refund is still accepted and applied.
		last := refunds[len(refunds)-1]
		e.fake.SetRefundStatus(e.fake.RefundByRef(last), "failed", "declined")
		eid := srhEvent("late")
		if status, body := e.srhPost(t, o.endpoint, o.secret, e.fake.RefundEventBody(eid, "refund.failed", e.fake.RefundByRef(last), false)); status != 200 || body != ok {
			t.Fatalf("late refund.failed answered %d %q", status, body)
		}
		if rc, _ := e.srhReceipt(t, o.endpoint, eid); rc.disposition != "ACCEPTED" {
			t.Fatalf("late refund.failed after 20 refunds and a full charge cap was %s/%s", rc.disposition, rc.reason)
		}
		e.awaitRefundFact(t, last, o.attempt, "FAILED")
		if sess, _, _ := e.srhCounters(t, o, ""); sess != sessBefore {
			t.Fatalf("checkout signal_count moved: %d -> %d", sessBefore, sess)
		}
	})

	t.Run("unknown and forged refund/charge objects are ignored", func(t *testing.T) {
		o := e.payMore(t, oa)
		signalsBefore := e.count(t, `SELECT count(*) FROM payments.stripe_signals`)
		cases := []struct {
			name, endpoint, secret, reason, objectType string
			obj                                        map[string]any
			typ                                        string
		}{
			{"dashboard refund (no lc metadata, unknown id)", epA, secA, "unknown_refund", "refund", map[string]any{"id": "re_srh_dashboard", "object": "refund", "payment_intent": o.pi, "status": "succeeded", "amount": 200, "currency": "twd", "metadata": map[string]any{}}, "refund.created"},
			{"lc_refund of ANOTHER store's refund", epA, secA, "unknown_refund", "refund", map[string]any{"id": "re_srh_forged1", "object": "refund", "payment_intent": oa.pi, "status": "succeeded", "amount": 1000, "currency": "twd", "metadata": map[string]any{"lc_refund": refundB, "lc_attempt": ob.attempt}}, "refund.updated"},
			{"our lc_refund with a foreign lc_attempt", epA, secA, "unknown_refund", "refund", map[string]any{"id": "re_srh_forged2", "object": "refund", "payment_intent": oa.pi, "status": "succeeded", "amount": 1000, "currency": "twd", "metadata": map[string]any{"lc_refund": refundA, "lc_attempt": ob.attempt}}, "refund.updated"},
			{"our refund metadata delivered on the OTHER account's endpoint", epB, secB, "unknown_refund", "refund", map[string]any{"id": "re_srh_forged3", "object": "refund", "payment_intent": oa.pi, "status": "succeeded", "amount": 1000, "currency": "twd", "metadata": map[string]any{"lc_refund": refundA, "lc_attempt": oa.attempt}}, "refund.updated"},
			{"charge of an unknown PaymentIntent", epA, secA, "unknown_charge", "charge", map[string]any{"id": "ch_srh_unknown", "object": "charge", "payment_intent": "pi_srh_unknown", "amount_refunded": 100}, "charge.refunded"},
			{"charge of another account's PaymentIntent", epA, secA, "unknown_charge", "charge", map[string]any{"id": "ch_srh_foreign", "object": "charge", "payment_intent": ob.pi, "amount_refunded": 100}, "charge.refunded"},
		}
		for _, c := range cases {
			eid := srhEvent("ign")
			if status, body := e.srhPost(t, c.endpoint, c.secret, stripetest.RawEvent(eid, c.typ, false, c.obj)); status != 200 || body != ok {
				t.Fatalf("%s answered %d %q", c.name, status, body)
			}
			rc, found := e.srhReceipt(t, c.endpoint, eid)
			if !found || rc.disposition != "IGNORED" || rc.reason != c.reason || rc.objectType != c.objectType || rc.hasSignal || rc.refundID != nil {
				t.Fatalf("%s: receipt %+v found=%v, want IGNORED/%s", c.name, rc, found, c.reason)
			}
		}
		if after := e.count(t, `SELECT count(*) FROM payments.stripe_signals`); after != signalsBefore {
			t.Fatalf("ignored deliveries created %d signals", after-signalsBefore)
		}
		// Mapping by the pinned Stripe refund id works without any metadata.
		eid := srhEvent("pinned")
		obj := map[string]any{"id": fakeA, "object": "refund", "payment_intent": oa.pi, "status": "succeeded", "amount": 1000, "currency": "twd", "metadata": map[string]any{}}
		if status, _ := e.srhPost(t, epA, secA, stripetest.RawEvent(eid, "refund.updated", false, obj)); status != 200 {
			t.Fatalf("pinned-id event answered %d", status)
		}
		if rc, _ := e.srhReceipt(t, epA, eid); rc.disposition != "ACCEPTED" || rc.refundID == nil || *rc.refundID != refundA {
			t.Fatalf("pinned id mapping: %+v", rc)
		}
	})

	t.Run("Dashboard refund seen on the charge: REFUND_HISTORY, new requests refused", func(t *testing.T) {
		o := e.payMore(t, oa)
		e.grant(t, o, "orders:read", "payments:refund")
		e.fake.DashboardRefund(o.pi, 500)
		eid := srhEvent("dash")
		if status, body := e.srhPost(t, o.endpoint, o.secret, e.fake.ChargeEventBody(eid, o.pi, false)); status != 200 || body != ok {
			t.Fatalf("charge.refunded answered %d %q", status, body)
		}
		e.awaitRefund(t, "REFUND_HISTORY review", o.attempt, o.attempt, 45*time.Second, `SELECT EXISTS(SELECT 1 FROM payments.review_cases WHERE attempt_id=$1 AND reason='REFUND_HISTORY')`)
		status, out := e.request(o, o.token(), t04Key("srh-hist"), rfxBody(500, "requested_by_customer", 2500))
		if status != 422 || srqCode(out) != "refund_blocked_review" {
			t.Fatalf("request after an external refund: %d %v", status, out)
		}
		if sum := (&srfEnv{rfxEnv: e}).summary(t, o); sum["payment_state"] != "REVIEW_REQUIRED" {
			t.Fatalf("payment_state %v, want REVIEW_REQUIRED (sticky REFUND_HISTORY)", sum["payment_state"])
		}
		if e.count(t, `SELECT count(*) FROM fulfillment.payment_work_items WHERE attempt_id=$1 AND state='READY'`, o.attempt) != 1 {
			t.Fatal("REFUND_HISTORY changed the work item")
		}
	})

	t.Run("a REQUESTED refund is suppressed before send when the charge already has an external refund: zero POST", func(t *testing.T) {
		o := e.payMore(t, oa)
		e.grant(t, o, "orders:read", "payments:refund")
		e.stopAllWorkers() // hold the refund unsent
		restarted := false
		defer func() {
			if !restarted {
				e.startWorker(t)
			}
		}()
		id := e.mustRefund(t, o, 1000, "requested_by_customer")
		e.fake.DashboardRefund(o.pi, 500)
		posts := e.fake.RefundPosts()
		restarted = true
		e.startWorker(t)
		e.awaitRefundFact(t, id, o.attempt, "REJECTED")
		if r := (&srfEnv{rfxEnv: e}).failureReason(t, id, "REJECTED"); r != "external_refund_detected" {
			t.Fatalf("reason %q", r)
		}
		if e.fake.RefundPosts() != posts || e.fake.RefundByRef(id) != "" {
			t.Fatalf("a suppressed refund reached the provider (POSTs %d -> %d)", posts, e.fake.RefundPosts())
		}
		var suppressed, sent *time.Time
		if err := e.f.owner.QueryRow(context.Background(), `SELECT suppressed_at,first_sent_at FROM payments.stripe_refunds WHERE id=$1`, id).Scan(&suppressed, &sent); err != nil || suppressed == nil || sent != nil {
			t.Fatalf("suppressed_at=%v first_sent_at=%v err=%v", suppressed, sent, err)
		}
		if !e.hasAttemptReview(t, o.attempt, "REFUND_HISTORY") {
			t.Fatal("no REFUND_HISTORY review for the external refund")
		}
		if st := e.itemState(t, o, id); st != "REJECTED" {
			t.Fatalf("state %s", st)
		}
	})

	t.Run("no ACK before COMMIT; a commit-time failure is a 503 with nothing persisted", func(t *testing.T) {
		e.startWorker(t)
		swhTrigger(t, e.f, "CONSTRAINT TRIGGER", "srh_commit_probe", "AFTER INSERT", "payments.stripe_webhook_receipts", "DEFERRABLE INITIALLY DEFERRED",
			`IF NEW.event_id LIKE 'evt_srhslow_%' THEN PERFORM pg_sleep(1.5); END IF;
			 IF NEW.event_id LIKE 'evt_srhfail_%' THEN RAISE EXCEPTION 'srh injected commit failure' USING ERRCODE='XX000'; END IF;
			 RETURN NULL;`)
		slowID := "evt_srhslow_" + t04Tag()
		done := make(chan [2]any, 1)
		go func() {
			status, body := e.srhPost(t, epA, secA, e.fake.RefundEventBody(slowID, "refund.updated", fakeA, false))
			done <- [2]any{status, body}
		}()
		time.Sleep(700 * time.Millisecond)
		select {
		case r := <-done:
			t.Fatalf("ACK %v before COMMIT completed", r)
		default:
		}
		if _, found := e.srhReceipt(t, epA, slowID); found {
			t.Fatal("receipt visible before COMMIT")
		}
		if r := <-done; r[0] != 200 || r[1] != ok {
			t.Fatalf("slow commit answered %v", r)
		}
		if rc, found := e.srhReceipt(t, epA, slowID); !found || rc.disposition != "ACCEPTED" {
			t.Fatalf("ACKed refund event has no committed receipt: %+v", rc)
		}
		before := [3]int{e.count(t, `SELECT count(*) FROM payments.stripe_webhook_receipts`), e.count(t, `SELECT count(*) FROM payments.stripe_signals`), e.count(t, `SELECT count(*) FROM river_payment.river_job WHERE kind='payment_signal_v1'`)}
		_, _, refBefore := e.srhCounters(t, oa, refundA)
		failID := "evt_srhfail_" + t04Tag()
		body := e.fake.RefundEventBody(failID, "refund.updated", fakeA, false)
		if status, resp := e.srhPost(t, epA, secA, body); status != 503 || resp != `{"error":"unavailable"}` {
			t.Fatalf("commit failure answered %d %q", status, resp)
		}
		after := [3]int{e.count(t, `SELECT count(*) FROM payments.stripe_webhook_receipts`), e.count(t, `SELECT count(*) FROM payments.stripe_signals`), e.count(t, `SELECT count(*) FROM river_payment.river_job WHERE kind='payment_signal_v1'`)}
		if _, found := e.srhReceipt(t, epA, failID); found || after != before {
			t.Fatalf("commit failure persisted rows: %v -> %v", before, after)
		}
		if _, _, ref := e.srhCounters(t, oa, refundA); ref != refBefore {
			t.Fatalf("a 503 consumed refund signal capacity: %d -> %d", refBefore, ref)
		}
		swhDropTrigger(t, e.f, "srh_commit_probe", "payments.stripe_webhook_receipts")
		if status, resp := e.srhPost(t, epA, secA, body); status != 200 || resp != ok {
			t.Fatalf("redelivery after the failure answered %d %q", status, resp)
		}
		if rc, found := e.srhReceipt(t, epA, failID); !found || rc.disposition != "ACCEPTED" || rc.redelivery != 0 {
			t.Fatalf("redelivery: %+v", rc)
		}
	})

	t.Run("no body, signature, ARN, receipt URL or billing email in logs or rows", func(t *testing.T) {
		needles := []string{stripetest.SentinelARN, stripetest.SentinelReceiptURL, stripetest.SentinelEmail, secA, secB, a.secret, b.secret, "destination_details", "receipt_url", "billing_details"}
		if hits := swhScan(t, e.f, needles...); len(hits) != 0 {
			t.Fatalf("sensitive strings persisted in %v", hits)
		}
		out := logs.String()
		for _, n := range append(needles, "Stripe-Signature", "whsec_", "v1=") {
			if strings.Contains(out, n) {
				t.Fatalf("logs contain %q", n)
			}
		}
		sflNoLeakedSecrets(t, e.sflEnv, secA, secB, a.secret, b.secret)
	})
}
