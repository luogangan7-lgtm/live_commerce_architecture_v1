package foundation_test

// S1 (r1-final-rulings S1; contracts/stripe-psp-v1.md §10, stripe-refund-v1 RD4): MOCK tier, REAL_PG.
// The checkout create path must give a recorded first-send rejection (400/401/403 before Stripe's
// idempotency layer) the same finality the refund path has:
//   - no later claim POSTs the same key again while payment_reconcile_v1 has not applied CLOSED_UNPAID;
//   - a first-send rejection report is never applied once the key was resent (create_send_count>1).
//
// Disclosed controlled fixtures: (a) an owner-pool transaction holding SHARE ROW EXCLUSIVE on
// payments.facts to delay the reconcile job's fact insert (reads stay open, so the worker and
// record_stripe_observation run normally); (b) an owner-inserted rejection observation for an attempt
// that really was resent, replayed through the real payments.apply_stripe_observation.

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"livecommerce/internal/integrations/psp/stripe"
	"livecommerce/internal/integrations/psp/stripe/stripetest"
)

func TestStripeS1CreateRejectionIsFinal(t *testing.T) {
	e := sflNew(t, nil)

	t.Run("first-send 400, reconcile delayed past the 5 s snooze: zero further POSTs, one CLOSED_UNPAID", func(t *testing.T) {
		s := e.stripeStore(t)
		e.fake.SetNextFault(stripetest.Fault{Validation: true})
		res := e.attempt(t, s)
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
		if _, err := hold.Exec(context.Background(), `LOCK TABLE payments.facts IN SHARE ROW EXCLUSIVE MODE`); err != nil {
			t.Fatal(err)
		}
		e.start(t, true)
		e.await(t, "recorded Via=create rejection", res.AttemptID, 30*time.Second,
			`SELECT EXISTS(SELECT 1 FROM payments.provider_observations x WHERE x.attempt_id=$1
			  AND x.report->>'Via'='create' AND x.report->>'ErrorClass'='rejected')`)
		if e.has(t, res.AttemptID, "CLOSED_UNPAID") {
			t.Fatal("fixture: CLOSED_UNPAID must still be pending")
		}
		deadline := time.Now().Add(9 * time.Second) // several claims after the 5 s snooze
		for time.Now().Before(deadline) {
			sstWake(t, e.f, res.AttemptID)
			time.Sleep(300 * time.Millisecond)
		}
		if n := e.sends(res.AttemptID); n != 1 {
			t.Fatalf("the same create key was POSTed %d times after a recorded first-send rejection", n)
		}
		release()
		e.awaitFact(t, res.AttemptID, "CLOSED_UNPAID")
		if n := e.sends(res.AttemptID); n != 1 || e.fake.SessionByReference(res.AttemptID) != "" {
			t.Fatalf("POSTs %d, provider session %q: a rejected create must never execute", n, e.fake.SessionByReference(res.AttemptID))
		}
		e.wantStock(t, s, sflReleased)
	})

	t.Run("a first-send rejection report never closes once the key was resent", func(t *testing.T) {
		s := e.stripeStore(t)
		e.fake.SetNextFault(stripetest.Fault{Cached500NoSession: true})
		res := e.attempt(t, s)
		e.await(t, "create sent x2", res.AttemptID, 45*time.Second,
			`SELECT create_send_count>=2 FROM payments.stripe_sessions WHERE attempt_id=$1`)
		var account string
		if err := e.f.owner.QueryRow(context.Background(),
			`SELECT account_id FROM payments.stripe_sessions WHERE attempt_id=$1`, res.AttemptID).Scan(&account); err != nil {
			t.Fatal(err)
		}
		o := (stripe.Session{}).Observation("create", stripe.CallMeta{HTTPStatus: 400}, account, 1, 1)
		o.ErrorClass = "rejected"
		report, err := json.Marshal(o)
		if err != nil {
			t.Fatal(err)
		}
		var hash []byte
		if err := e.f.owner.QueryRow(context.Background(), `
		 WITH src AS (SELECT tenant_id,store_id,id AS attempt_id,execution_profile,environment
		   FROM checkout.payment_attempts WHERE id=$1)
		 INSERT INTO payments.provider_observations(tenant_id,store_id,attempt_id,source,execution_profile,environment,
		   first_generation,report,report_hash)
		 SELECT tenant_id,store_id,attempt_id,'QUERY',execution_profile,environment,2,$2::jsonb,
		   sha256(convert_to($2::jsonb::text,'UTF8')) FROM src RETURNING report_hash`, res.AttemptID, string(report)).Scan(&hash); err != nil {
			t.Fatalf("fixture observation: %v", err)
		}
		if _, err := e.f.owner.Exec(context.Background(), `SELECT payments.apply_stripe_observation($1::uuid,$2::bytea)`, res.AttemptID, hash); err != nil {
			t.Fatalf("apply_stripe_observation: %v", err)
		}
		if e.has(t, res.AttemptID, "CLOSED_UNPAID") {
			t.Fatal("a first-send rejection report closed the attempt although the key was resent (RD4)")
		}
		e.wantStock(t, s, sflPending)
	})
}
