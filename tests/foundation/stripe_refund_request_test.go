package foundation_test

// RF04 (contracts/stripe-refund-v1.md §9): the merchant refund REQUEST, REAL_PG through the real
// HTTP handler and the real River insert (no worker sends anything: the payment worker is stopped
// after the orders are paid, so every refund stays REQUESTED and the tests observe the request tx
// alone). Prefix `srq`. Written from §4.3/§4.4/§7.1/§8 and the refund-core defaults D1-D8.
//
// Owner-pool fixtures, named at their use: review_cases rows inserted with an existing
// observation hash (there is no product path that produces most §4.3 reasons on demand), and
// grants/sessions from the shared rfx helpers.

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

// srqReview inserts one review case for an attempt (owner fixture), reusing an observation the real
// capture path already recorded.
func srqReview(t *testing.T, e *rfxEnv, attempt, reason string) {
	t.Helper()
	mustExec(t, e.f.owner, `INSERT INTO payments.review_cases(tenant_id,store_id,attempt_id,reason,source_report_hash)
	 SELECT tenant_id,store_id,attempt_id,$2,report_hash FROM payments.provider_observations WHERE attempt_id=$1 ORDER BY received_at LIMIT 1`, attempt, reason)
}

func srqUnreview(t *testing.T, e *rfxEnv, attempt string) {
	t.Helper()
	mustExec(t, e.f.owner, `DELETE FROM payments.review_cases WHERE attempt_id=$1`, attempt)
}

type srqCounts struct{ refunds, ops, events, audits, results, jobs, signals int }

func srqCount(t *testing.T, e *rfxEnv, o rfxOrder) srqCounts {
	t.Helper()
	var c srqCounts
	err := e.f.owner.QueryRow(context.Background(), `SELECT
	 (SELECT count(*) FROM payments.stripe_refunds WHERE order_id=$1),
	 (SELECT count(*) FROM integration.operations WHERE actor_kind='PAYMENT_REFUND' AND payment_attempt_id=$2),
	 (SELECT count(*) FROM integration.operation_events ev JOIN integration.operations op ON op.id=ev.operation_id WHERE op.actor_kind='PAYMENT_REFUND' AND op.payment_attempt_id=$2),
	 (SELECT count(*) FROM ops.audit_events WHERE tenant_id=$3 AND store_id=$4 AND action='payments.refund_requested'),
	 (SELECT count(*) FROM ops.command_results WHERE tenant_id=$3 AND store_id=$4 AND operation='payments.refund.request'),
	 (SELECT count(*) FROM river_payment.river_job WHERE kind='payment_refund_v1'),
	 (SELECT count(*) FROM payments.stripe_signals WHERE attempt_id=$2 AND refund_id IS NOT NULL)`,
		o.order, o.attempt, o.s.p.f.tenantA, o.store()).Scan(&c.refunds, &c.ops, &c.events, &c.audits, &c.results, &c.jobs, &c.signals)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func srqCode(out map[string]any) string {
	c, _ := out["code"].(string)
	return c
}

func TestStripeRF04Request(t *testing.T) {
	e := rfxNew(t)
	a := e.stripeStore(t)
	endpoint, secret := e.endpoint(t, a)
	payuni := e.addPAYUNi(t)
	stop := e.startWorker(t)

	main := e.pay(t, a, endpoint, secret)
	race := e.payMore(t, main)
	matrix := e.payMore(t, main)
	limit := e.payMore(t, main)
	late1 := e.payMore(t, main)
	late2 := e.payMore(t, main)
	if main.captured != 2500 {
		t.Fatalf("fixture captured %d, the tests below assume TWD 2500", main.captured)
	}
	payuni.pcAwaitCapture(t)
	payuniOrder := rfxOrder{s: sstStore{p: payuni.psHarness}, order: payuni.hold.OrderID}
	// An order that was started but never paid: no CAPTURED fact.
	unpaidStore := e.stripeStore(t)
	unpaidAttempt := e.attempt(t, unpaidStore)
	unpaid := rfxOrder{s: unpaidStore, attempt: unpaidAttempt.AttemptID, order: unpaidStore.p.hold.OrderID}
	// Wait until the payment worker has finished every capture job before it is stopped, so no
	// late capture/poll write can be mistaken for a refund side effect.
	for _, o := range []rfxOrder{main, race, matrix, limit, late1, late2} {
		e.await(t, "query job completed", o.attempt, 45*time.Second, `SELECT NOT EXISTS(SELECT 1 FROM river_payment.river_job WHERE args->>'operation_id'=$1 AND state NOT IN ('completed','cancelled','discarded'))`)
	}
	stop()

	for _, o := range []rfxOrder{main, race, matrix, limit, late1, late2} {
		e.grant(t, o, "orders:read", "payments:refund")
	}
	e.grant(t, unpaid, "orders:read", "payments:refund")
	e.grant(t, payuniOrder, "orders:read", "payments:refund")
	readerToken, _ := e.member(t, main, "orders:read")
	noPermToken, _ := e.member(t, main, "catalog:read")

	// ---- atomic request -------------------------------------------------------------------
	t.Run("atomic request writes op, refund, event, audit, command result and job", func(t *testing.T) {
		before := srqCount(t, e, main)
		fp := e.fingerprint(t, main)
		key := t04Key("srq-atomic")
		status, out := e.request(main, main.token(), key, rfxBody(1000, "requested_by_customer", 2500))
		if status != 201 {
			t.Fatalf("request answered %d: %v", status, out)
		}
		if len(out) != 5 || out["state"] != "REQUESTED" || out["amount_minor"] != float64(1000) || out["currency"] != "TWD" || out["refundable_minor"] != float64(1500) {
			t.Fatalf("response keys/values: %v", out)
		}
		id := out["refund_id"].(string)
		after := srqCount(t, e, main)
		want := srqCounts{before.refunds + 1, before.ops + 1, before.events + 1, before.audits + 1, before.results + 1, before.jobs + 1, before.signals}
		if after.refunds != want.refunds || after.ops != want.ops || after.audits != want.audits || after.results != want.results || after.jobs != want.jobs || after.signals != want.signals || after.events < want.events {
			t.Fatalf("row deltas (refunds,ops,events,audits,results,jobs,signals) before=%+v after=%+v", before, after)
		}
		var (
			principal, account, pi, cur, reason, env, opKind, opProvider, opAction, opState, lease, semantic, reqRefund, reqAttempt string
			amount, generation, opJob                                                                                               int64
			sent, suppressed, pinned                                                                                                *time.Time
			sendCount                                                                                                               int
			resendOK                                                                                                                bool
			paramLen                                                                                                                int
			hashOK, bindingSame                                                                                                     bool
		)
		err := e.f.owner.QueryRow(context.Background(), `SELECT r.principal_id::text,r.account_id,r.payment_intent_id,r.currency,r.reason,r.environment,r.amount_minor,
		  r.first_sent_at,r.suppressed_at,r.pinned_at,r.send_count,(r.resend_until=r.requested_at+interval '20 hours'),octet_length(r.create_params::text),
		  op.actor_kind,op.provider,op.action,op.state,op.lease_mode,op.generation,op.semantic_key,op.request->>'refund_id',op.request->>'attempt_id',
		  op.job_id,(op.request_hash=sha256(convert_to(op.request::text,'UTF8'))),
		  (op.binding_id,op.binding_version,op.external_asset_id,op.buyer_owner_id,op.buyer_session_id) IS NOT DISTINCT FROM (a.binding_id,a.binding_version,a.external_asset_id,a.buyer_owner_id,a.buyer_session_id)
		 FROM payments.stripe_refunds r JOIN integration.operations op ON op.id=r.id JOIN integration.operations a ON a.id=r.attempt_id WHERE r.id=$1`, id).
			Scan(&principal, &account, &pi, &cur, &reason, &env, &amount, &sent, &suppressed, &pinned, &sendCount, &resendOK, &paramLen,
				&opKind, &opProvider, &opAction, &opState, &lease, &generation, &semantic, &reqRefund, &reqAttempt, &opJob, &hashOK, &bindingSame)
		if err != nil {
			t.Fatalf("read back refund %s: %v", id, err)
		}
		if principal != main.s.p.f.principalA || account != main.s.account || pi != main.pi || cur != "TWD" || reason != "requested_by_customer" || env != "SANDBOX" || amount != 1000 {
			t.Fatalf("refund row: principal=%s account=%s pi=%s cur=%s reason=%s env=%s amount=%d", principal, account, pi, cur, reason, env, amount)
		}
		if sent != nil || suppressed != nil || pinned != nil || sendCount != 0 || !resendOK || paramLen == 0 || paramLen > 2048 {
			t.Fatalf("refund row markers: sent=%v suppressed=%v pinned=%v send_count=%d resendOK=%v params=%d", sent, suppressed, pinned, sendCount, resendOK, paramLen)
		}
		// D1: born UNKNOWN, generation 1, no lease, like the checkout operation.
		if opKind != "PAYMENT_REFUND" || opProvider != "stripe" || opAction != "stripe.refund" || opState != "UNKNOWN" || generation != 1 || lease != "" ||
			semantic != "payment.stripe.refund:"+id || reqRefund != id || reqAttempt != main.attempt || !hashOK || !bindingSame {
			t.Fatalf("operation row: %s/%s/%s state=%s gen=%d lease=%q semantic=%s req=%s/%s hashOK=%v bindingSame=%v", opKind, opProvider, opAction, opState, generation, lease, semantic, reqRefund, reqAttempt, hashOK, bindingSame)
		}
		var leaseUntil *time.Time
		if err := e.f.owner.QueryRow(context.Background(), `SELECT lease_until FROM integration.operations WHERE id=$1`, id).Scan(&leaseUntil); err != nil || leaseUntil != nil {
			t.Fatalf("D1: the refund op is born without a lease: %v %v", leaseUntil, err)
		}
		// The verified River job: kind, exact args, linked from the op.
		var jobKind, args string
		if err := e.f.owner.QueryRow(context.Background(), `SELECT kind,args::text FROM river_payment.river_job WHERE id=$1`, opJob).Scan(&jobKind, &args); err != nil {
			t.Fatalf("op.job_id does not reference a River job: %v", err)
		}
		var decoded map[string]any
		_ = json.Unmarshal([]byte(args), &decoded)
		if jobKind != "payment_refund_v1" || len(decoded) != 2 || decoded["operation_id"] != id || decoded["version"] != float64(1) {
			t.Fatalf("job %s args %s", jobKind, args)
		}
		e.assertUnchanged(t, main, fp, "the refund request")
		if e.count(t, `SELECT count(*) FROM payments.refund_facts WHERE refund_id=$1`, id) != 0 {
			t.Fatal("a request wrote a refund fact")
		}
		// Replay: same key + same body returns the same refund and writes nothing.
		status, replay := e.request(main, main.token(), key, rfxBody(1000, "requested_by_customer", 2500))
		if (status != 200 && status != 201) || replay["refund_id"] != id || replay["amount_minor"] != float64(1000) {
			t.Fatalf("replay: %d %v", status, replay)
		}
		if again := srqCount(t, e, main); again != after {
			t.Fatalf("replay wrote rows: %+v -> %+v", after, again)
		}
		// Same key, different body: 409, nothing written.
		if status, out := e.request(main, main.token(), key, rfxBody(900, "requested_by_customer", 2500)); status != 409 {
			t.Fatalf("key reuse with a different body: %d %v", status, out)
		}
		if again := srqCount(t, e, main); again != after {
			t.Fatalf("conflicting replay wrote rows: %+v -> %+v", after, again)
		}
	})

	t.Run("expected_refundable CAS and capacity", func(t *testing.T) {
		fp := e.fingerprint(t, main)
		base := srqCount(t, e, main)
		// 1000 is held: refundable is 1500. A stale expectation is refundable_changed (409).
		status, out := e.request(main, main.token(), t04Key("srq-cas"), rfxBody(500, "requested_by_customer", 2500))
		if status != 409 || srqCode(out) != "refundable_changed" {
			t.Fatalf("stale expected_refundable: %d %v", status, out)
		}
		// Step rule: TWD is whole dollars (multiples of 100).
		status, out = e.request(main, main.token(), t04Key("srq-step"), rfxBody(150, "requested_by_customer", 1500))
		if status != 422 || srqCode(out) != "amount_step" {
			t.Fatalf("amount step: %d %v", status, out)
		}
		// held + amount > captured.
		status, out = e.request(main, main.token(), t04Key("srq-over"), rfxBody(1600, "requested_by_customer", 1500))
		if status != 422 || srqCode(out) != "exceeds_refundable" {
			t.Fatalf("exceeds_refundable: %d %v", status, out)
		}
		// RD12: fraudulent is not offered.
		if status, out = e.request(main, main.token(), t04Key("srq-fraud"), rfxBody(100, "fraudulent", 1500)); status != 422 {
			t.Fatalf("fraudulent reason: %d %v", status, out)
		}
		if again := srqCount(t, e, main); again != base {
			t.Fatalf("a refused request wrote rows: %+v -> %+v", base, again)
		}
		// A REQUESTED (never sent, pending/unknown alike) refund holds capacity: take the remainder,
		// then even 100 is refused and refundable is 0.
		status, out = e.request(main, main.token(), t04Key("srq-rest"), rfxBody(1500, "duplicate", 1500))
		if status != 201 || out["refundable_minor"] != float64(0) {
			t.Fatalf("remainder: %d %v", status, out)
		}
		status, out = e.request(main, main.token(), t04Key("srq-none"), rfxBody(100, "requested_by_customer", 0))
		if status != 422 || srqCode(out) != "exceeds_refundable" {
			t.Fatalf("no capacity left: %d %v", status, out)
		}
		l, _ := e.list(t, main, main.token())
		if l["captured_minor"] != float64(2500) || l["refundable_minor"] != float64(0) || l["pending_minor"] != float64(2500) || l["refunded_minor"] != float64(0) {
			t.Fatalf("list capacity numbers: %v", l)
		}
		e.assertUnchanged(t, main, fp, "refund requests")
	})

	t.Run("two concurrent requests whose sum exceeds captured: exactly one wins (real two-transaction witness)", func(t *testing.T) {
		fp := e.fingerprint(t, race)
		ctx := context.Background()
		// The witness: this owner transaction holds the order row lock; both requests must queue on it
		// (RD3: the order row is the first lock of the request), which pg_blocking_pids proves.
		holder, err := e.f.owner.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer holder.Rollback(ctx)
		var pid int
		if err := holder.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&pid); err != nil {
			t.Fatal(err)
		}
		if _, err := holder.Exec(ctx, `SELECT 1 FROM checkout.orders WHERE id=$1 FOR UPDATE`, race.order); err != nil {
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
				status, out := e.request(race, race.token(), t04Key(fmt.Sprintf("srq-race-%d", i)), rfxBody(1500, "requested_by_customer", 2500))
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
				t.Fatalf("only %d of 2 requests queued behind the order lock: the request does not lock the order first", blocked)
			}
			time.Sleep(20 * time.Millisecond) // lock_timeout is 1 s: release promptly
		}
		if err := holder.Commit(ctx); err != nil {
			t.Fatal(err)
		}
		wg.Wait()
		close(results)
		var won, lost int
		for r := range results {
			switch {
			case r.status == 201:
				won++
			case (r.status == 409 && srqCode(r.out) == "refundable_changed") || (r.status == 422 && srqCode(r.out) == "exceeds_refundable"):
				lost++
			default:
				t.Fatalf("unexpected race outcome %d %v", r.status, r.out)
			}
		}
		if won != 1 || lost != 1 {
			t.Fatalf("race: won=%d lost=%d, want exactly one winner (I01/I05: refunds never exceed captured)", won, lost)
		}
		if n := e.count(t, `SELECT count(*) FROM payments.stripe_refunds WHERE order_id=$1`, race.order); n != 1 {
			t.Fatalf("race left %d refund rows", n)
		}
		if held := e.count(t, `SELECT coalesce(sum(amount_minor),0)::bigint FROM payments.stripe_refunds WHERE order_id=$1`, race.order); held != 1500 {
			t.Fatalf("held=%d", held)
		}
		e.assertUnchanged(t, race, fp, "the concurrent requests")
	})

	t.Run("4.3 matrix incl. the A1 combinations", func(t *testing.T) {
		fp := e.fingerprint(t, matrix)
		// try issues one request under exactly the given review reasons; accepted requests hold
		// capacity for the rest of the sequence, so amounts are chosen against the remaining amount.
		try := func(reasons []string, amount int64, wantStatus int, wantCode string) {
			t.Helper()
			srqUnreview(t, e, matrix.attempt)
			for _, r := range reasons {
				srqReview(t, e, matrix.attempt, r)
			}
			status, out := e.request(matrix, matrix.token(), t04Key("srq-matrix"), rfxBody(amount, "requested_by_customer", e.refundable(t, matrix)))
			if status != wantStatus || (wantCode != "" && srqCode(out) != wantCode) {
				t.Fatalf("reviews %v amount %d: got %d %v, want %d %s", reasons, amount, status, out, wantStatus, wantCode)
			}
		}
		try(nil, 500, 201, "")                                    // held 500
		try([]string{"PROVIDER_PRESENTMENT_DRIFT"}, 500, 201, "") // money matched (I05): partial allowed; held 1000
		// remaining is now 1500. Full-remaining-only reviews refuse a partial.
		try([]string{"CLOSURE_CONTRADICTED"}, 500, 422, "")
		try([]string{"PAID_ALLOCATION_FAILED"}, 500, 422, "")
		try([]string{"CLOSURE_CONTRADICTED", "PROVIDER_PRESENTMENT_DRIFT"}, 500, 422, "") // A1 combination
		// Blocking combinations refuse even the full remaining amount.
		try([]string{"PAID_ALLOCATION_FAILED", "REFUND_HISTORY"}, 1500, 422, "refund_blocked_review")
		try([]string{"CLOSURE_CONTRADICTED", "CONFLICTING_REPORT"}, 1500, 422, "refund_blocked_review")
		// Every other reason blocks.
		for _, r := range []string{"PROVIDER_AMOUNT_MISMATCH", "PROVIDER_IDENTITY_MISMATCH", "PROVIDER_SESSION_DUPLICATE", "CONFLICTING_REPORT",
			"REFUND_HISTORY", "REFUND_UNRESOLVED", "REFUND_AMOUNT_MISMATCH", "REFUND_CONFLICTING", "CAPTURE_EVIDENCE_INCOMPLETE", "PROVIDER_EXPIRY_UNCONFIRMED", "PROVIDER_ASYNC_PENDING"} {
			try([]string{r}, 500, 422, "refund_blocked_review")
		}
		// CLOSURE_CONTRADICTED + PROVIDER_PRESENTMENT_DRIFT: the full remaining amount is allowed.
		try([]string{"CLOSURE_CONTRADICTED", "PROVIDER_PRESENTMENT_DRIFT"}, 1500, 201, "")
		if e.refundable(t, matrix) != 0 {
			t.Fatal("full remaining refund did not take all capacity")
		}
		srqUnreview(t, e, matrix.attempt)
		// Late-payment obligations alone: a partial is refused, the full amount is accepted.
		for _, c := range []struct {
			o      rfxOrder
			reason string
		}{{late1, "CLOSURE_CONTRADICTED"}, {late2, "PAID_ALLOCATION_FAILED"}} {
			srqReview(t, e, c.o.attempt, c.reason)
			if status, out := e.request(c.o, c.o.token(), t04Key("srq-late"), rfxBody(500, "requested_by_customer", 2500)); status != 422 {
				t.Fatalf("%s partial: %d %v", c.reason, status, out)
			}
			if status, out := e.request(c.o, c.o.token(), t04Key("srq-late"), rfxBody(2500, "requested_by_customer", 2500)); status != 201 {
				t.Fatalf("%s full remaining: %d %v", c.reason, status, out)
			}
			// The review is NOT cleared by the request (§4.3: consumes the obligation, keeps the case).
			if !e.hasAttemptReview(t, c.o.attempt, c.reason) {
				t.Fatalf("%s review was cleared by a refund request", c.reason)
			}
		}
		// The fingerprint covers ledger, reservations, order state and work items: none of this moved them.
		e.assertUnchanged(t, matrix, fp, "matrix requests")
	})

	t.Run("A1: a suppression recorded but not yet applied blocks new requests", func(t *testing.T) {
		var refund string
		if err := e.f.owner.QueryRow(context.Background(), `SELECT id::text FROM payments.stripe_refunds WHERE order_id=$1`, race.order).Scan(&refund); err != nil {
			t.Fatal(err)
		}
		// Owner fixture: record a suppression without its REJECTED fact (the state between
		// record_stripe_charge_observation and apply_stripe_refund).
		mustExec(t, e.f.owner, `UPDATE payments.stripe_refunds SET suppressed_at=clock_timestamp() WHERE id=$1`, refund)
		status, out := e.request(race, race.token(), t04Key("srq-suppr"), rfxBody(500, "requested_by_customer", e.refundable(t, race)))
		if status != 422 || srqCode(out) != "refund_blocked_review" {
			t.Fatalf("suppression pending apply must block: %d %v", status, out)
		}
	})

	t.Run("not refundable: no capture, PAYUNi", func(t *testing.T) {
		for name, o := range map[string]rfxOrder{"never paid": unpaid, "PAYUNi": payuniOrder} {
			status, out := e.request(o, o.token(), t04Key("srq-nr"), rfxBody(100, "requested_by_customer", 0))
			if status != 422 || srqCode(out) != "not_refundable" {
				t.Fatalf("%s: %d %v", name, status, out)
			}
			if n := e.count(t, `SELECT count(*) FROM payments.stripe_refunds WHERE order_id=$1`, o.order); n != 0 {
				t.Fatalf("%s: %d refund rows", name, n)
			}
		}
	})

	t.Run("authority: 401, 403, 404 and store isolation", func(t *testing.T) {
		before := srqCount(t, e, main)
		body := rfxBody(100, "requested_by_customer", e.refundable(t, limit))
		cases := []struct {
			name, token string
			want        int
			path        rfxOrder
		}{
			{"no bearer token", "", 401, limit},
			{"expired session", e.f.tokens["expired"], 401, limit},
			{"revoked session", e.f.tokens["revoked"], 401, limit},
			{"orders:read alone cannot refund", readerToken, 403, limit},
			{"no orders grant at all", noPermToken, 403, limit},
		}
		for _, c := range cases {
			if status, out := e.request(c.path, c.token, t04Key("srq-auth"), body); status != c.want {
				t.Fatalf("%s: got %d %v want %d", c.name, status, out, c.want)
			}
		}
		// Another store's order id under our store is indistinguishable from a missing order (404).
		foreign := rfxOrder{s: main.s, order: unpaid.order}
		s1, o1 := e.request(foreign, main.token(), t04Key("srq-404"), body)
		missing := rfxOrder{s: main.s, order: randomUUID()}
		s2, o2 := e.request(missing, main.token(), t04Key("srq-404"), body)
		if s1 != 404 || s2 != 404 || srqCode(o1) != srqCode(o2) || o1["message"] != o2["message"] {
			t.Fatalf("other-store vs missing order: %d %v / %d %v", s1, o1, s2, o2)
		}
		// A member of a different store cannot reach our store at all.
		other := rfxOrder{s: unpaid.s, order: main.order}
		if status, _ := e.request(other, unpaid.token(), t04Key("srq-x"), body); status != 404 {
			t.Fatalf("cross-store order under the other store's path: %d", status)
		}
		// Revoking the grant takes effect on the next request (fresh final auth).
		mustExec(t, e.f.owner, `DELETE FROM identity.store_grants WHERE store_id=$1 AND principal_id=$2 AND permission='payments:refund'`, main.store(), main.s.p.f.principalA)
		if status, out := e.request(main, main.token(), t04Key("srq-revoked"), body); status != 403 {
			t.Fatalf("revoked payments:refund: %d %v", status, out)
		}
		e.grant(t, main, "payments:refund")
		if after := srqCount(t, e, main); after != before {
			t.Fatalf("refused authority cases wrote rows: %+v -> %+v", before, after)
		}
	})

	t.Run("the 21st refund is refused", func(t *testing.T) {
		fp := e.fingerprint(t, limit)
		for i := 1; i <= 20; i++ {
			status, out := e.request(limit, limit.token(), t04Key(fmt.Sprintf("srq-lim-%d", i)), rfxBody(100, "requested_by_customer", 2500-int64(i-1)*100))
			if status != 201 {
				t.Fatalf("refund %d of 20: %d %v", i, status, out)
			}
		}
		status, out := e.request(limit, limit.token(), t04Key("srq-lim-21"), rfxBody(100, "requested_by_customer", 500))
		if status != 422 || srqCode(out) != "refund_limit" {
			t.Fatalf("21st refund (I23): %d %v", status, out)
		}
		if n := e.count(t, `SELECT count(*) FROM payments.stripe_refunds WHERE order_id=$1`, limit.order); n != 20 {
			t.Fatalf("refund rows = %d", n)
		}
		e.assertUnchanged(t, limit, fp, "20 refund requests")
	})

	t.Run("refresh writes a MERCHANT_REFRESH signal and a job, no facts", func(t *testing.T) {
		var refund string
		if err := e.f.owner.QueryRow(context.Background(), `SELECT id::text FROM payments.stripe_refunds WHERE order_id=$1 ORDER BY requested_at LIMIT 1`, main.order).Scan(&refund); err != nil {
			t.Fatal(err)
		}
		status, raw, _ := e.call("POST", rfxRefundsPath(main)+"/"+refund+"/refresh", main.token(), nil, "")
		var out map[string]any
		_ = json.Unmarshal(raw, &out)
		if status != 200 && status != 202 || out["refund_id"] != refund || out["scheduled"] != true || len(out) != 2 {
			t.Fatalf("refresh: %d %s", status, raw)
		}
		var source string
		var attempt string
		if err := e.f.owner.QueryRow(context.Background(), `SELECT source,attempt_id::text FROM payments.stripe_signals WHERE refund_id=$1`, refund).Scan(&source, &attempt); err != nil || source != "MERCHANT_REFRESH" || attempt != main.attempt {
			t.Fatalf("signal row: source=%q attempt=%q err=%v", source, attempt, err)
		}
		if n := e.count(t, `SELECT count(*) FROM river_payment.river_job WHERE kind='payment_signal_v1' AND args->>'operation_id'=$1`, refund); n != 1 {
			t.Fatalf("refresh jobs = %d (job operation_id must be the refund id)", n)
		}
		if n := e.count(t, `SELECT count(*) FROM payments.refund_facts WHERE refund_id=$1`, refund); n != 0 {
			t.Fatal("refresh wrote a fact")
		}
		if strings.Contains(string(raw), "stripe") {
			t.Fatalf("refresh leaks provider data: %s", raw)
		}
	})
}
