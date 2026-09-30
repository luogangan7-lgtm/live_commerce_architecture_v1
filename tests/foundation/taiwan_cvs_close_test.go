package foundation_test

// TCV18 TestCvsCloseDispatchWindow (contracts/taiwan-cvs-logistics-v1.md §4.3 load_cvs_create, §16.2 payable check; R2 close-out,
// open review P2s on migrations/0073). Prefix `tcl`. Tier REAL_PG + MOCK: real definers and the real merchant HTTP handler.
// Owner-pool writes (disclosed fixtures): planting a live DISPATCHING lease and putting the operation back to READY; a competing
// transaction that holds the order row lock the way request_stripe_refund does; cloning an operation row under another store's GUCs.
// Each of the three checks was red on 744a1d3 (no refusal, no blocking, cross-store insert accepted).

import (
	"bytes"
	"context"
	"testing"
	"time"
)

func TestCvsCloseDispatchWindow(t *testing.T) {
	e := tcvNew(t, tcvOpts{stripe: true})
	f := e.p.f
	ctx := context.Background()
	e.r.startWorker(t)
	e.startDispatcher()
	e.grantCreator("orders:read", "fulfillment:write", "integration:manage", "integration:read")
	e.connect("C2C")
	e.cvsSettings(tcvAllChains, true, "20000", 500)
	api711, _, _ := e.service("cvs_711", "API", 0)

	// card order with a label request that no dispatcher has claimed yet (READY)
	card, _ := e.cvsOrder(tcvOrderSpec{kind: "cvs_711", code: api711})
	st, out, raw := e.ship(e.token(), card, 0, "", false)
	if st != 202 {
		t.Fatalf("request: %d %s", st, raw)
	}
	opID := tcvStr(out, "operation_id")
	var attempt string
	var captured int64
	_ = f.owner.QueryRow(ctx, `SELECT a.id::text FROM checkout.payment_attempts a WHERE a.order_id=$1`, card).Scan(&attempt)
	_ = f.owner.QueryRow(ctx, `SELECT amount_minor FROM payments.facts WHERE attempt_id=$1 AND kind='CAPTURED'`, attempt).Scan(&captured)
	ro := rfxOrder{s: e.ro.s, attempt: attempt, order: card, endpoint: e.ro.endpoint, secret: e.ro.secret, captured: captured}
	token := bytes.Repeat([]byte{9}, 32)
	plant := func(set string, args ...any) {
		t.Helper()
		tx, err := f.owner.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := tx.Exec(ctx, `SET LOCAL session_replication_role=replica`); err != nil {
			t.Fatal(err)
		}
		if _, err := tx.Exec(ctx, `UPDATE integration.operations SET `+set+` WHERE id=$1`, append([]any{opID}, args...)...); err != nil {
			tx.Rollback(ctx)
			t.Fatalf("plant: %v", err)
		}
		if err := tx.Commit(ctx); err != nil {
			t.Fatal(err)
		}
	}
	refundRows := func() int {
		return e.count(`SELECT count(*) FROM payments.stripe_refunds WHERE order_id=$1`, card)
	}

	t.Run("a card refund is refused while the label operation is DISPATCHING (the Create may be on the wire)", func(t *testing.T) {
		plant(`state='DISPATCHING',generation=1,lease_mode='dispatch',lease_until=clock_timestamp()+interval '10 minutes',lease_token_hash=$2`, sha256Bytes(token))
		expected := e.r.refundable(t, ro)
		status, body := e.r.request(ro, ro.token(), "rfx-"+t04Tag(), rfxBody(captured, "requested_by_customer", expected))
		if status != 409 {
			t.Errorf("refund while DISPATCHING: want 409, got %d %v", status, body)
		}
		if n := refundRows(); n != 0 {
			t.Errorf("a refused refund left %d stripe_refunds rows", n)
		}
		// control: the same request is accepted once the operation is back to READY (refund before dispatch stops the label: TCV05)
		plant(`state='READY',generation=0,lease_mode='',lease_until=NULL,lease_token_hash=NULL`)
		expected = e.r.refundable(t, ro)
		if status, body = e.r.request(ro, ro.token(), "rfx-"+t04Tag(), rfxBody(captured, "requested_by_customer", expected)); status != 201 {
			t.Errorf("refund while the operation is READY: want 201, got %d %v", status, body)
		}
	})
}

// A refund in flight (order row locked FOR UPDATE, as request_stripe_refund does first) must be waited for by the dispatch-time
// payable check, so a refund that commits between the claim and the load is seen and stops the label purchase.
func TestCvsCloseLoadWaitsForOrderLock(t *testing.T) {
	e := tcvNew(t, tcvOpts{stripe: true})
	f := e.p.f
	ctx := context.Background()
	e.r.startWorker(t)
	e.startDispatcher()
	e.grantCreator("orders:read", "fulfillment:write", "integration:manage", "integration:read")
	e.connect("C2C")
	e.cvsSettings(tcvAllChains, true, "20000", 500)
	api711, _, _ := e.service("cvs_711", "API", 0)
	card, _ := e.cvsOrder(tcvOrderSpec{kind: "cvs_711", code: api711})
	st, out, raw := e.ship(e.token(), card, 0, "", false)
	if st != 202 {
		t.Fatalf("request: %d %s", st, raw)
	}
	opID := tcvStr(out, "operation_id")
	token := bytes.Repeat([]byte{8}, 32)
	tx, err := f.owner.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = tx.Exec(ctx, `SET LOCAL session_replication_role=replica`)
	if _, err := tx.Exec(ctx, `UPDATE integration.operations SET state='DISPATCHING',generation=1,lease_mode='dispatch',lease_until=clock_timestamp()+interval '10 minutes',lease_token_hash=$2 WHERE id=$1`, opID, sha256Bytes(token)); err != nil {
		tx.Rollback(ctx)
		t.Fatalf("plant the lease: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}

	lock, err := f.owner.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	released := false
	defer func() {
		if !released {
			lock.Rollback(ctx)
		}
	}()
	if _, err := lock.Exec(ctx, `SELECT 1 FROM checkout.orders WHERE id=$1 FOR UPDATE`, card); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		var n int
		done <- e.p.worker.QueryRow(ctx, `SELECT count(*) FROM integration.load_cvs_create($1::uuid,1,$2,'dispatch')`, opID, token).Scan(&n)
	}()
	select {
	case err := <-done:
		t.Fatalf("load_cvs_create returned while a refund held the order lock (err=%v): the payable check cannot see a refund that commits next", err)
	case <-time.After(1500 * time.Millisecond):
	}
	if err := lock.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	released = true
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("load_cvs_create after the lock was released: %v", err)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("load_cvs_create still blocked after the order lock was released")
	}
}

// The integration_writer insert policy for ecpay.cvs_create operations is bound to the caller's store GUCs like every other
// integration_writer insert policy in 0073 (defence in depth; plan_cvs_create is the only inserter today).
func TestCvsCloseOperationInsertScope(t *testing.T) {
	e := tcvNew(t)
	f := e.p.f
	ctx := context.Background()
	e.grantCreator("orders:read", "fulfillment:write", "integration:manage", "integration:read")
	e.connect("C2C")
	e.cvsSettings(tcvAllChains, true, "20000", 500)
	api711, _, _ := e.service("cvs_711", "API", 0)
	order, _ := e.cvsOrder(tcvOrderSpec{kind: "cvs_711", code: api711, paymentMode: "pay_at_pickup"})
	st, out, raw := e.ship(e.token(), order, 0, "", false)
	if st != 202 {
		t.Fatalf("request: %d %s", st, raw)
	}
	var tenant, store, principal, binding, asset string
	var bver, job int64
	if err := f.owner.QueryRow(ctx, `SELECT tenant_id::text,store_id::text,principal_id::text,binding_id::text,binding_version,external_asset_id,job_id FROM integration.operations WHERE id=$1`,
		tcvStr(out, "operation_id")).Scan(&tenant, &store, &principal, &binding, &bver, &asset, &job); err != nil {
		t.Fatal(err)
	}
	insert := func(guc string) error {
		tx, err := f.owner.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback(ctx)
		if _, err := tx.Exec(ctx, `SET LOCAL ROLE commerce_integration_writer`); err != nil {
			t.Fatal(err)
		}
		if _, err := tx.Exec(ctx, `SELECT set_config('app.tenant_id',$1,true),set_config('app.store_id',$2,true)`, tenant, guc); err != nil {
			t.Fatal(err)
		}
		id := randomUUID()
		_, err = tx.Exec(ctx, `INSERT INTO integration.operations(tenant_id,store_id,id,principal_id,binding_id,binding_version,provider,external_asset_id,purpose,action,semantic_key,request_hash,request,job_id)
			VALUES($1,$2,$3,$4,$5,$6,'ecpay_logistics',$7,'transactional','ecpay.cvs_create',$8,$9,'{}'::jsonb,$10)`,
			tenant, store, id, principal, binding, bver, asset, "tcl:"+id, randomBytes(32), job)
		return err
	}
	if err := insert(store); err != nil {
		t.Fatalf("control: an insert under the row's own store GUCs: %v", err)
	}
	if err := insert(randomUUID()); sqlState(err) != "42501" {
		t.Errorf("an insert for store %s under another store's GUCs: want 42501, got %v", store, err)
	}
}
