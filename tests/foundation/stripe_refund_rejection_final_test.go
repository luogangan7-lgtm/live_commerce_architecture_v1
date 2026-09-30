package foundation_test

// RF06b (contracts/stripe-refund-v1.md RD4/RD9, review finding "first-send rejection then resend"): MOCK tier.
// Prefix `srf`.
//
// A recorded first-send rejection (400 / 401 / 403 before Stripe's idempotency layer) is FINAL:
//   - no later claim may POST the same key again, even while payment_reconcile_v1 has not yet applied
//     the REJECTED fact (a resend could create the refund after capacity was released);
//   - apply_stripe_refund never releases capacity from a rejection report once any resend happened.
//
// Disclosed controlled fixtures: (a) an owner-pool transaction holding SHARE ROW EXCLUSIVE on
// payments.refund_facts to delay the reconcile job's fact insert (reads stay open, so the refund worker
// and record_stripe_refund_observation run normally); (b) an owner-inserted rejection observation for a
// refund that really was resent, to replay a late first-send report through the real
// payments.apply_stripe_refund.

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"livecommerce/internal/integrations/psp/stripe/stripetest"
)

func TestStripeRF06RejectionIsFinal(t *testing.T) {
	e := srfNew(t)

	t.Run("first-send 400, reconcile delayed past the 5 s snooze: zero further POSTs, exactly one REJECTED fact", func(t *testing.T) {
		o := e.fresh(t)
		e.grant(t, o, "orders:read", "payments:refund")
		hold, err := e.f.owner.Begin(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		released := false
		release := func() {
			if !released {
				released = true
				_ = hold.Rollback(context.Background())
			}
		}
		defer release()
		if _, err := hold.Exec(context.Background(), `LOCK TABLE payments.refund_facts IN SHARE ROW EXCLUSIVE MODE`); err != nil {
			t.Fatal(err)
		}
		e.fake.SetNextFault(stripetest.Fault{Validation: true})
		id := e.mustRefund(t, o, 1000, "requested_by_customer")
		e.awaitRefund(t, "recorded Via=create rejection", id, o.attempt, 30*time.Second,
			`SELECT EXISTS(SELECT 1 FROM payments.provider_observations x WHERE x.report->>'RefundRef'=$1
			  AND x.report->>'Via'='create' AND x.report->>'ErrorClass'='rejected')`)
		if e.hasRefundFact(t, id, "REJECTED") {
			t.Fatal("fixture: the REJECTED fact must still be pending")
		}
		if n := e.keysOf(id); n != 1 {
			t.Fatalf("POSTs so far %d, want 1", n)
		}
		// Several claims after the 5 s snooze while the fact is still unapplied.
		deadline := time.Now().Add(9 * time.Second)
		for time.Now().Before(deadline) {
			e.wakeRefund(t, id, o.attempt)
			time.Sleep(300 * time.Millisecond)
		}
		if n := e.keysOf(id); n != 1 {
			t.Fatalf("the same key was POSTed %d times after a recorded first-send rejection", n)
		}
		if e.hasRefundFact(t, id, "REJECTED") {
			t.Fatal("fixture: the reconcile job was not delayed")
		}
		release()
		e.awaitRefundFact(t, id, o.attempt, "REJECTED")
		if n := e.count(t, `SELECT count(*) FROM payments.refund_facts WHERE refund_id=$1 AND kind='REJECTED'`, id); n != 1 {
			t.Fatalf("REJECTED facts %d, want exactly 1", n)
		}
		if n := e.keysOf(id); n != 1 || e.fake.RefundByRef(id) != "" {
			t.Fatalf("POSTs %d, provider refund %q: a rejected refund must never be created", n, e.fake.RefundByRef(id))
		}
		e.wantList(t, o, 0, 0, 2500)
	})

	t.Run("a rejection report of the first send never releases capacity once a resend happened", func(t *testing.T) {
		o := e.fresh(t)
		e.grant(t, o, "orders:read", "payments:refund")
		e.fake.SetNextFault(stripetest.Fault{Cached500NoSession: true})
		id := e.mustRefund(t, o, 1000, "requested_by_customer")
		e.awaitSend(t, id, o.attempt, 2) // first send and one same-key resend, both uncertain
		report, err := json.Marshal(map[string]any{"Provider": "stripe", "Object": "refund", "Via": "create",
			"RefundRef": id, "ErrorClass": "rejected", "SendCount": 1, "RefundID": ""})
		if err != nil {
			t.Fatal(err)
		}
		var hash []byte
		if err := e.f.owner.QueryRow(context.Background(), `
		 WITH src AS (SELECT tenant_id,store_id,attempt_id,execution_profile,environment,first_generation
		   FROM payments.provider_observations WHERE attempt_id=$1 ORDER BY received_at LIMIT 1)
		 INSERT INTO payments.provider_observations(tenant_id,store_id,attempt_id,source,execution_profile,environment,
		   first_generation,report,report_hash)
		 SELECT tenant_id,store_id,attempt_id,'QUERY',execution_profile,environment,first_generation,$2::jsonb,
		   sha256(convert_to($2::jsonb::text,'UTF8')) FROM src RETURNING report_hash`, o.attempt, string(report)).Scan(&hash); err != nil {
			t.Fatalf("fixture observation: %v", err)
		}
		if _, err := e.f.owner.Exec(context.Background(), `SELECT payments.apply_stripe_refund($1::uuid,$2::bytea)`, o.attempt, hash); err != nil {
			t.Fatalf("apply_stripe_refund: %v", err)
		}
		if e.hasRefundFact(t, id, "REJECTED") {
			t.Fatal("a first-send rejection report released capacity although the key was resent (RD4)")
		}
		e.wantList(t, o, 0, 1000, 1500)
	})
}
