package foundation_test

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"
	integration "livecommerce/internal/integrations/core"
)

type sslSignalArgs struct {
	OperationID string `json:"operation_id"`
	SignalID    string `json:"signal_id"`
	Version     int    `json:"version"`
}

func (sslSignalArgs) Kind() string { return "payment_signal_v1" }

type sslSignalMaterial struct {
	source           string
	createdAt, dbNow time.Time
	sessionID        pgtype.Text
	consumedAt       pgtype.Timestamptz
}

// TestStripeSP11SQLSignalLoader proves a claimed worker reads only the exact
// signal linked to its River job, even when a newer signal exists for the op.
func TestStripeSP11SQLSignalLoader(t *testing.T) {
	h := sslStartStripe(t)
	ctx := context.Background()
	jobs, err := river.NewClient(riverpgxv5.New(h.p.f.owner), &river.Config{Schema: "river_payment"})
	if err != nil {
		t.Fatal(err)
	}
	first, second := randomUUID(), randomUUID()
	jobFor := func(signal, source string) int64 {
		t.Helper()
		tx, err := h.p.f.owner.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback(ctx)
		job, err := jobs.InsertTx(ctx, tx, sslSignalArgs{OperationID: h.attempt, SignalID: signal, Version: 1},
			&river.InsertOpts{Queue: "payment_mock_v1"})
		if err != nil {
			t.Fatal(err)
		}
		if _, err = tx.Exec(ctx, `INSERT INTO payments.stripe_signals
		 (id,tenant_id,store_id,attempt_id,source,job_id) VALUES ($1::uuid,$2::uuid,$3::uuid,$4::uuid,$5,$6)`,
			signal, h.p.f.tenantA, h.p.f.storeA1, h.attempt, source, job.Job.ID); err != nil {
			t.Fatal(err)
		}
		if err = tx.Commit(ctx); err != nil {
			t.Fatal(err)
		}
		return job.Job.ID
	}
	firstJob := jobFor(first, "BUYER_REFRESH")
	secondJob := jobFor(second, "BUYER_CANCEL")
	var queue string
	if err := h.p.f.owner.QueryRow(ctx, `SELECT queue FROM river_payment.river_job WHERE id=$1`, firstJob).Scan(&queue); err != nil || queue != "payment_mock_v1" {
		t.Fatalf("signal fixture routed to wrong queue: %q err=%v", queue, err)
	}
	if _, err := h.p.worker.Exec(ctx, `SELECT source FROM payments.stripe_signals LIMIT 1`); !stripeSQLState(err, "42501") {
		t.Fatalf("worker directly read signal table: %v", err)
	}
	var loaderOID uint32
	if err := h.p.f.owner.QueryRow(ctx, `SELECT 'integration.load_stripe_signal(uuid,bigint,bytea,text,bigint,uuid)'::regprocedure::oid`).Scan(&loaderOID); err != nil {
		t.Fatalf("resolve loader OID as fixture owner: %v", err)
	}
	var canExecute bool
	if err := h.p.worker.QueryRow(ctx, `SELECT has_function_privilege(current_user,$1::oid,'EXECUTE')`, loaderOID).Scan(&canExecute); err != nil || !canExecute {
		t.Fatalf("worker loader EXECUTE missing: %v", err)
	}
	if err := h.hosted.QueryRow(ctx, `SELECT has_function_privilege(current_user,$1::oid,'EXECUTE')`, loaderOID).Scan(&canExecute); err != nil || canExecute {
		t.Fatalf("hosted role unexpectedly allowed loader EXECUTE: %v", err)
	}

	claimTx, err := h.p.worker.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer claimTx.Rollback(ctx)
	claim, err := (&integration.Service{}).Claim(ctx, claimTx, h.attempt, 120)
	if err != nil || claim.Disposition != "claimed" {
		t.Fatalf("claim Stripe operation: disposition=%s err=%v", claim.Disposition, err)
	}
	if err := claimTx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	load := func(op string, generation int64, token []byte, profile any, job int64, signal string) (sslSignalMaterial, error) {
		var row sslSignalMaterial
		err := h.p.worker.QueryRow(ctx, `SELECT source,session_id,created_at,consumed_at,db_now
		 FROM integration.load_stripe_signal($1::uuid,$2::bigint,$3::bytea,$4::text,$5::bigint,$6::uuid)`,
			op, generation, token, profile, job, signal).
			Scan(&row.source, &row.sessionID, &row.createdAt, &row.consumedAt, &row.dbNow)
		return row, err
	}
	read := func(job int64, signal, wantSource string, wantConsumed bool) {
		t.Helper()
		row, err := load(h.attempt, claim.Generation, claim.LeaseToken, "PROVIDER_MOCK", job, signal)
		if err != nil || row.source != wantSource || row.sessionID.Valid || row.consumedAt.Valid != wantConsumed || row.createdAt.IsZero() || row.dbNow.IsZero() {
			t.Fatalf("exact signal source=%q consumed=%t err=%v", row.source, row.consumedAt.Valid, err)
		}
	}
	// The second row is newer; loading the first still returns its exact source.
	read(firstJob, first, "BUYER_REFRESH", false)
	read(secondJob, second, "BUYER_CANCEL", false)
	for _, v := range []struct {
		name, op, profile, signal string
		generation, job           int64
		token                     []byte
	}{
		{"wrong job", h.attempt, "PROVIDER_MOCK", first, claim.Generation, secondJob, claim.LeaseToken},
		{"wrong signal", h.attempt, "PROVIDER_MOCK", second, claim.Generation, firstJob, claim.LeaseToken},
		{"wrong operation", randomUUID(), "PROVIDER_MOCK", first, claim.Generation, firstJob, claim.LeaseToken},
		{"wrong profile", h.attempt, "SANDBOX", first, claim.Generation, firstJob, claim.LeaseToken},
		{"wrong token", h.attempt, "PROVIDER_MOCK", first, claim.Generation, firstJob, randomBytes(32)},
		{"wrong generation", h.attempt, "PROVIDER_MOCK", first, claim.Generation + 1, firstJob, claim.LeaseToken},
	} {
		t.Run(v.name, func(t *testing.T) {
			if _, err := load(v.op, v.generation, v.token, v.profile, v.job, v.signal); err == nil {
				t.Fatal("unbound signal material returned")
			}
		})
	}
	if _, err := load(h.attempt, claim.Generation, claim.LeaseToken, nil, firstJob, first); err == nil {
		t.Fatal("NULL profile returned signal material")
	}
	// A malformed River row must be refused either by its table guard at UPDATE
	// or by the loader if an owner fixture can commit the mutation.
	if _, err := h.p.f.owner.Exec(ctx, `UPDATE river_payment.river_job
	 SET args=jsonb_set(args,'{signal_id}',to_jsonb($2::text),false) WHERE id=$1`, secondJob, first); err == nil {
		if _, err := load(h.attempt, claim.Generation, claim.LeaseToken, "PROVIDER_MOCK", secondJob, second); err == nil {
			t.Fatal("wrong River signal args returned material")
		}
		mustExec(t, h.p.f.owner, `UPDATE river_payment.river_job
		 SET args=jsonb_set(args,'{signal_id}',to_jsonb($2::text),false) WHERE id=$1`, secondJob, second)
	} else {
		var linked string
		if readErr := h.p.f.owner.QueryRow(ctx, `SELECT args->>'signal_id' FROM river_payment.river_job WHERE id=$1`, secondJob).Scan(&linked); readErr != nil || linked != second {
			t.Fatalf("River args guard left bad linkage: %q err=%v", linked, readErr)
		}
	}
	if _, err := h.p.f.owner.Exec(ctx, `UPDATE river_payment.river_job SET queue='payment_sandbox_v1' WHERE id=$1`, secondJob); err == nil {
		if _, err := load(h.attempt, claim.Generation, claim.LeaseToken, "PROVIDER_MOCK", secondJob, second); err == nil {
			t.Fatal("wrong River queue returned material")
		}
		mustExec(t, h.p.f.owner, `UPDATE river_payment.river_job SET queue='payment_mock_v1' WHERE id=$1`, secondJob)
	} else {
		if readErr := h.p.f.owner.QueryRow(ctx, `SELECT queue FROM river_payment.river_job WHERE id=$1`, secondJob).Scan(&queue); readErr != nil || queue != "payment_mock_v1" {
			t.Fatalf("River queue guard left wrong route: %q err=%v", queue, readErr)
		}
	}
	var deniedSource string
	if err := h.hosted.QueryRow(ctx, `SELECT source FROM integration.load_stripe_signal($1::uuid,$2::bigint,$3::bytea,$4::text,$5::bigint,$6::uuid)`,
		h.attempt, claim.Generation, claim.LeaseToken, "PROVIDER_MOCK", firstJob, first).Scan(&deniedSource); !stripeSQLState(err, "42501") {
		t.Fatalf("hosted role executed worker loader: %v", err)
	}
	mustExec(t, h.p.f.owner, `UPDATE payments.stripe_signals
	 SET consumed_at=clock_timestamp(),outcome='OBSERVED' WHERE id=$1::uuid`, first)
	read(firstJob, first, "BUYER_REFRESH", true)
	mustExec(t, h.p.f.owner, `UPDATE integration.operations SET lease_until=clock_timestamp()-interval '1 second' WHERE id=$1::uuid`, h.attempt)
	if _, err := load(h.attempt, claim.Generation, claim.LeaseToken, "PROVIDER_MOCK", secondJob, second); err == nil {
		t.Fatal("late lease returned signal material")
	}
}
