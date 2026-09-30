package foundation_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"livecommerce/internal/command"
	"livecommerce/internal/platform"
)

func hpDigest(t *testing.T, configURL, notifyURL string) []byte {
	t.Helper()
	raw, err := json.Marshal(struct {
		Version   string `json:"version"`
		ReturnURL string `json:"return_url"`
		NotifyURL string `json:"notify_url"`
	}{"payuni-hosted-v1", configURL, notifyURL})
	if err != nil {
		t.Fatal(err)
	}
	hash := sha256.Sum256(raw)
	return hash[:]
}

func hpSQLState(err error, want string) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == want
}

func TestBuyerPaymentHostedSQLAuthorityAndScope(t *testing.T) {
	h := hpSetup(t)
	result, err := h.begin(t04Key("hp-acl"))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	for _, sig := range []string{
		"checkout.load_hosted_material(bytea,uuid,uuid,text,text)",
		"checkout.save_hosted_page(bytea,uuid,uuid,text,text,bytea,jsonb)",
		"checkout.take_hosted_page(bytea,uuid,uuid,text,bytea)",
	} {
		var hostedExec, genericExec, merchantExec, workerExec, buyerExec, publicExec, definer, fixedPath bool
		err := h.f.owner.QueryRow(ctx, `SELECT
		 has_function_privilege('commerce_hosted_runtime',to_regprocedure($1),'EXECUTE'),
		 has_function_privilege('commerce_checkout_runtime',to_regprocedure($1),'EXECUTE'),
		 has_function_privilege('commerce_runtime',to_regprocedure($1),'EXECUTE'),
		 has_function_privilege('commerce_worker',to_regprocedure($1),'EXECUTE'),
		 has_function_privilege('commerce_buyer_runtime',to_regprocedure($1),'EXECUTE'),
		 has_function_privilege('public',to_regprocedure($1),'EXECUTE'),
		 p.prosecdef,p.proconfig=ARRAY['search_path=pg_catalog']::text[]
		 FROM pg_proc p WHERE p.oid=to_regprocedure($1)`, sig).
			Scan(&hostedExec, &genericExec, &merchantExec, &workerExec, &buyerExec, &publicExec, &definer, &fixedPath)
		if err != nil || !hostedExec || genericExec || merchantExec || workerExec || buyerExec || publicExec || !definer || !fixedPath {
			t.Fatalf("HP04 function grant/definer failure for %s: %v", sig, err)
		}
	}
	var hostedInherits, hostedCanSet, writerCanSet, hostedSelect, hostedUpdateForm, writerUpdateTime, writerUpdateForm, genericCipher bool
	err = h.f.owner.QueryRow(ctx, `SELECT
	 pg_has_role('commerce_hosted_runtime','commerce_checkout_runtime','USAGE'),
	 pg_has_role('commerce_hosted_runtime','commerce_checkout_runtime','SET'),
	 pg_has_role('commerce_hosted_runtime','commerce_checkout_writer','SET'),
	 has_table_privilege('commerce_hosted_runtime','checkout.hosted_payment_pages','SELECT'),
	 has_column_privilege('commerce_hosted_runtime','checkout.hosted_payment_pages','form','UPDATE'),
	 has_column_privilege('commerce_checkout_writer','checkout.hosted_payment_pages','handed_out_at','UPDATE'),
	 has_column_privilege('commerce_checkout_writer','checkout.hosted_payment_pages','form','UPDATE'),
	 has_column_privilege('commerce_checkout_runtime','integration.account_credentials','ciphertext','SELECT')`).
		Scan(&hostedInherits, &hostedCanSet, &writerCanSet, &hostedSelect, &hostedUpdateForm, &writerUpdateTime, &writerUpdateForm, &genericCipher)
	if err != nil || !hostedInherits || hostedCanSet || writerCanSet || hostedSelect || hostedUpdateForm || !writerUpdateTime || writerUpdateForm || genericCipher {
		t.Fatalf("HP04 role and column grants invalid: %v", err)
	}
	var tableOwner string
	if err := h.f.owner.QueryRow(ctx, `SELECT relowner::regrole::text FROM pg_class WHERE oid='checkout.hosted_payment_pages'::regclass`).Scan(&tableOwner); err != nil || tableOwner == "commerce_checkout_writer" {
		t.Fatalf("HP04 hosted table ownership invalid: %v", err)
	}
	if _, err := h.pool.Exec(ctx, `SET ROLE commerce_checkout_writer`); err == nil {
		t.Fatal("HP04 hosted login can SET ROLE private writer")
	}
	if err := platform.ValidateCheckoutPool(ctx, h.pool); err == nil {
		t.Fatal("HP04 generic checkout accepted hosted login")
	}
	if err := platform.ValidateHostedPool(ctx, h.psHarness.pool); err == nil {
		t.Fatal("HP04 hosted validator accepted ordinary checkout login")
	}
	var directForm []byte
	if err := h.pool.QueryRow(ctx, `SELECT form FROM checkout.hosted_payment_pages WHERE attempt_id=$1`, result.AttemptID).Scan(&directForm); !hpSQLState(err, "42501") {
		t.Fatal("HP04 hosted runtime received direct form table SELECT")
	}
	if _, err := h.psHarness.pool.Exec(ctx, `SELECT checkout.load_hosted_material($1::bytea,$2::uuid,$3::uuid,$4,$5)`, tokenHash(h.cap.Token), h.f.storeA1, h.hold.OrderID, "PROVIDER_MOCK", "zh-TW"); !hpSQLState(err, "42501") {
		t.Fatal("HP04 generic checkout could load hosted material")
	}
	form, _, _, _ := h.page(t, result.AttemptID)
	digest := hpDigest(t, h.config.ReturnURL, h.config.NotifyURL)
	if _, err := h.psHarness.pool.Exec(ctx, `SELECT checkout.save_hosted_page($1::bytea,$2::uuid,$3::uuid,$4,$5,$6::bytea,$7::jsonb)`, tokenHash(h.cap.Token), h.f.storeA1, h.hold.OrderID, "PROVIDER_MOCK", "zh-TW", digest, form); !hpSQLState(err, "42501") {
		t.Fatal("HP04 generic checkout could save copied hosted form")
	}
	if _, err := h.psHarness.pool.Exec(ctx, `SELECT checkout.take_hosted_page($1::bytea,$2::uuid,$3::uuid,$4,$5::bytea)`, tokenHash(h.cap.Token), h.f.storeA1, h.hold.OrderID, "PROVIDER_MOCK", digest); !hpSQLState(err, "42501") {
		t.Fatal("HP04 generic checkout could take hosted form")
	}
	if _, err := h.api.TakeHosted(ctx, h.cap.Token, randomUUID(), h.hold.OrderID); err == nil {
		t.Fatal("HP04 cross-store form release succeeded")
	}
	other := mustIssue(t, h.cqHarness.service, h.f.storeA1)
	if _, err := h.api.TakeHosted(ctx, other.Token, h.f.storeA1, h.hold.OrderID); err == nil {
		t.Fatal("HP04 cross-owner form release succeeded")
	}
	if _, err := h.api.TakeHosted(ctx, h.cap.Token, h.f.storeA1, randomUUID()); err == nil {
		t.Fatal("HP04 cross-order form release succeeded")
	}
	if _, err := h.api.TakeHosted(ctx, randomToken(), h.f.storeA1, h.hold.OrderID); err == nil {
		t.Fatal("HP04 forged capability released form")
	}
	tx, err := h.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `SELECT set_config('app.buyer_id',$1,true)`, randomUUID()); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `SELECT checkout.take_hosted_page($1::bytea,$2::uuid,$3::uuid,$4,$5::bytea)`, tokenHash(randomToken()), h.f.storeA1, h.hold.OrderID, "PROVIDER_MOCK", digest); err == nil {
		t.Fatal("HP04 forged GUC bypassed capability authority")
	}
	_, _, _, handed := h.page(t, result.AttemptID)
	if handed != nil {
		t.Fatal("HP04 denied requests consumed the one-shot handoff")
	}
}

func TestBuyerPaymentHostedForgedOuterFormRejected(t *testing.T) {
	h := hpSetup(t)
	result, err := h.begin(t04Key("hp-form-shape"))
	if err != nil {
		t.Fatal(err)
	}
	stored, _, _, _ := h.page(t, result.AttemptID)
	var original map[string]any
	if err := json.Unmarshal(stored, &original); err != nil {
		t.Fatal(err)
	}
	digest := hpDigest(t, h.config.ReturnURL, h.config.NotifyURL)
	for _, mutate := range []func(map[string]any){
		func(form map[string]any) { form["action"] = "https://attacker.example.test/api/upp" },
		func(form map[string]any) { form["fields"].(map[string]any)["MerID"] = "other_account" },
		func(form map[string]any) { form["fields"].(map[string]any)["Version"] = "1.0" },
		func(form map[string]any) { form["fields"].(map[string]any)["EncryptInfo"] = "bad" },
		func(form map[string]any) { form["fields"].(map[string]any)["HashInfo"] = "bad" },
		func(form map[string]any) { form["fields"].(map[string]any)["MerID"] = []string{"mock-account"} },
	} {
		var form map[string]any
		if err := json.Unmarshal(stored, &form); err != nil {
			t.Fatal(err)
		}
		mutate(form)
		bad, err := json.Marshal(form)
		if err != nil {
			t.Fatal(err)
		}
		_, err = h.pool.Exec(context.Background(), `SELECT checkout.save_hosted_page($1::bytea,$2::uuid,$3::uuid,$4,$5,$6::bytea,$7::jsonb)`,
			tokenHash(h.cap.Token), h.f.storeA1, h.hold.OrderID, "PROVIDER_MOCK", "zh-TW", digest, bad)
		if !hpSQLState(err, "22023") {
			t.Fatal("HP04 forged outer form was not rejected at shape/merchant boundary")
		}
	}
	still, _, _, _ := h.page(t, result.AttemptID)
	if !bytes.Equal(stored, still) || len(original) != 2 {
		t.Fatal("HP04 forged save mutated stored form")
	}
}

func TestBuyerPaymentHostedStorageFailureRollsBack(t *testing.T) {
	h := hpSetup(t)
	before := h.counts(t)
	name := "hp_reject_" + t04Tag()
	fn := pgx.Identifier{name}.Sanitize()
	trigger := pgx.Identifier{name + "_trigger"}.Sanitize()
	mustExec(t, h.f.owner, fmt.Sprintf(`CREATE FUNCTION public.%s() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'synthetic hosted storage failure' USING ERRCODE='PT409'; END $$`, fn))
	mustExec(t, h.f.owner, fmt.Sprintf(`CREATE TRIGGER %s BEFORE INSERT ON checkout.hosted_payment_pages FOR EACH ROW EXECUTE FUNCTION public.%s()`, trigger, fn))
	t.Cleanup(func() {
		mustExec(t, h.f.owner, fmt.Sprintf(`DROP TRIGGER IF EXISTS %s ON checkout.hosted_payment_pages`, trigger))
		mustExec(t, h.f.owner, fmt.Sprintf(`DROP FUNCTION IF EXISTS public.%s()`, fn))
	})
	if _, err := h.begin(t04Key("hp-storage-fail")); err == nil {
		t.Fatal("HP01 storage fault returned success")
	}
	if h.counts(t) != before {
		t.Fatal("HP01 storage fault committed partial payment/stock/job facts")
	}
}

func TestBuyerPaymentHostedDriftAndFinancialReviewDenyFirstTake(t *testing.T) {
	for name, change := range map[string]func(*testing.T, hpHarness, checkoutResult){
		"binding-disabled": func(t *testing.T, h hpHarness, _ checkoutResult) {
			mustExec(t, h.f.owner, `UPDATE integration.bindings SET enabled=false WHERE id=$1`, h.binding)
		},
		"method-hidden": func(t *testing.T, h hpHarness, _ checkoutResult) {
			mustExec(t, h.f.owner, `UPDATE payments.method_versions SET visible=false WHERE connection_id=$1`, h.account)
		},
		"qualification-revoked": func(t *testing.T, h hpHarness, _ checkoutResult) {
			qualExec(t, h.f.owner, `UPDATE payments.account_qualifications SET revoked_at=clock_timestamp() WHERE id=$1`, h.proof)
		},
		"credential-rotated": func(t *testing.T, h hpHarness, _ checkoutResult) {
			hpRotateFixtureHead(t, h)
		},
		"financial-authorized": func(t *testing.T, h hpHarness, result checkoutResult) {
			hpSeedReviewFact(t, h, result, false)
		},
		"review-case": func(t *testing.T, h hpHarness, result checkoutResult) {
			hpSeedReviewFact(t, h, result, true)
		},
	} {
		t.Run(name, func(t *testing.T) {
			h := hpSetup(t)
			result, err := h.begin(t04Key("hp-drift"))
			if err != nil {
				t.Fatal(err)
			}
			change(t, h, checkoutResult{result.AttemptID, result.AmountMinor})
			if _, err := h.take(); err == nil {
				t.Fatal("HP05/HP06 first handoff survived drift or financial review")
			}
			_, _, _, handed := h.page(t, result.AttemptID)
			if handed != nil {
				t.Fatal("HP05/HP06 denied first handoff consumed form")
			}
		})
	}
}

type checkoutResult struct {
	ID     string
	Amount int64
}

func hpSeedReviewFact(t *testing.T, h hpHarness, result checkoutResult, review bool) {
	t.Helper()
	report := []byte(`{"synthetic":"hosted-gate"}`)
	mustExec(t, h.f.owner, `INSERT INTO payments.provider_observations(tenant_id,store_id,attempt_id,source,execution_profile,environment,first_generation,report,report_hash)
	 SELECT tenant_id,store_id,id,'QUERY',execution_profile,environment,generation,$2::jsonb,sha256(convert_to($2::jsonb::text,'UTF8'))
	 FROM checkout.payment_attempts WHERE id=$1`, result.ID, report)
	if review {
		mustExec(t, h.f.owner, `INSERT INTO payments.review_cases(tenant_id,store_id,attempt_id,reason,source_report_hash)
		 SELECT tenant_id,store_id,id,'CONFLICTING_REPORT',sha256(convert_to($2::jsonb::text,'UTF8'))
		 FROM checkout.payment_attempts WHERE id=$1`, result.ID, report)
	} else {
		mustExec(t, h.f.owner, `INSERT INTO payments.facts(tenant_id,store_id,attempt_id,kind,amount_minor,currency,provider_reference,connection_id,execution_profile,environment,source_report_hash)
		 SELECT tenant_id,store_id,id,'AUTHORIZED',$3,currency,'hosted_fixture_reference',connection_id,execution_profile,environment,sha256(convert_to($2::jsonb::text,'UTF8'))
		 FROM checkout.payment_attempts WHERE id=$1`, result.ID, report, result.Amount)
	}
}

func TestBuyerPaymentHostedReplayAfterDisableAndRevokedCapability(t *testing.T) {
	h := hpSetup(t)
	result, err := h.begin(t04Key("hp-replay-disable"))
	if err != nil {
		t.Fatal(err)
	}
	first, err := h.take()
	if err != nil || hpDisposition(t, first) != "ISSUED" {
		t.Fatal("HP03 first handoff not issued")
	}
	mustExec(t, h.f.owner, `UPDATE integration.bindings SET enabled=false WHERE id=$1`, h.binding)
	replay, err := h.take()
	if err != nil || hpDisposition(t, replay) != "ALREADY_ISSUED" {
		t.Fatalf("HP03 no-form replay after disable: %v", err)
	}
	hpNoForm(t, replay)
	mustExec(t, h.f.owner, `UPDATE buyer.capability_sessions SET revoked_at=clock_timestamp() WHERE id=$1`, h.cap.Scope.SessionID)
	if _, err := h.take(); err == nil {
		t.Fatal("HP04 revoked capability replayed a handoff")
	}
	_, _, _, handed := h.page(t, result.AttemptID)
	if handed == nil {
		t.Fatal("HP03 first handoff timestamp disappeared")
	}
}

func TestBuyerPaymentHostedQualificationExpiresDuringTakeWait(t *testing.T) {
	h := hpSetup(t)
	result, err := h.begin(t04Key("hp-wait"))
	if err != nil {
		t.Fatal(err)
	}
	// The first guard must wait on the order row. Expire the qualification while
	// it waits, then release the row; its database-clock final check must deny.
	tx, err := h.f.owner.Begin(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(context.Background())
	if _, err := tx.Exec(context.Background(), `SELECT id FROM checkout.orders WHERE id=$1 FOR UPDATE`, h.hold.OrderID); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { _, e := h.take(); done <- e }()
	hpAwaitOrderLock(t, h, tx.Conn().PgConn().PID())
	qualExec(t, h.f.owner, `UPDATE payments.account_qualifications SET expires_at=clock_timestamp()+interval '1 second' WHERE id=$1`, h.proof)
	time.Sleep(1200 * time.Millisecond)
	if err := tx.Commit(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err == nil {
		t.Fatal("HP05 qualification expiry during row wait released form")
	}
	_, _, _, handed := h.page(t, result.AttemptID)
	if handed != nil {
		t.Fatal("HP05 expired wait committed handoff timestamp")
	}
}

func TestBuyerPaymentHostedWriterCannotMutateFormOrClearHandoff(t *testing.T) {
	h := hpSetup(t)
	result, err := h.begin(t04Key("hp-immutable"))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	scope := func(tx pgx.Tx) {
		t.Helper()
		for setting, value := range map[string]string{
			"app.tenant_id": h.f.tenantA, "app.store_id": h.f.storeA1,
			"app.buyer_id": h.cap.Scope.OwnerID, "app.buyer_session_id": h.cap.Scope.SessionID,
		} {
			if _, err := tx.Exec(ctx, `SELECT set_config($1,$2,true)`, setting, value); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := tx.Exec(ctx, `SET LOCAL ROLE commerce_checkout_writer`); err != nil {
			t.Fatal(err)
		}
	}
	tx, err := h.f.owner.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	scope(tx)
	if _, err := tx.Exec(ctx, `UPDATE checkout.hosted_payment_pages SET form=form WHERE attempt_id=$1`, result.AttemptID); !hpSQLState(err, "42501") {
		t.Fatal("HP04 private writer could mutate frozen form")
	}
	if err := tx.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := h.take(); err != nil {
		t.Fatal(err)
	}
	tx, err = h.f.owner.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	scope(tx)
	tag, err := tx.Exec(ctx, `UPDATE checkout.hosted_payment_pages SET handed_out_at=NULL WHERE attempt_id=$1`, result.AttemptID)
	if err != nil || tag.RowsAffected() != 0 {
		t.Fatalf("HP04 one-way handoff timestamp reset: %v", err)
	}
	if err := tx.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	_, _, _, handed := h.page(t, result.AttemptID)
	if handed == nil {
		t.Fatal("HP04 handoff timestamp was cleared")
	}
}

func hpDelayPageWrite(t *testing.T, h hpHarness, event string) string {
	t.Helper()
	name := "hp_delay_" + t04Tag()
	fn := pgx.Identifier{name}.Sanitize()
	trigger := pgx.Identifier{name + "_trigger"}.Sanitize()
	sequenceName := name + "_seq"
	sequence := pgx.Identifier{sequenceName}.Sanitize()
	mustExec(t, h.f.owner, fmt.Sprintf(`CREATE SEQUENCE public.%s`, sequence))
	mustExec(t, h.f.owner, fmt.Sprintf(`CREATE FUNCTION public.%s() RETURNS trigger LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog AS $$ BEGIN PERFORM nextval('public.%s'::regclass); PERFORM pg_sleep(2.5); RETURN NEW; END $$`, fn, sequenceName))
	mustExec(t, h.f.owner, fmt.Sprintf(`CREATE TRIGGER %s BEFORE %s ON checkout.hosted_payment_pages FOR EACH ROW EXECUTE FUNCTION public.%s()`, trigger, event, fn))
	t.Cleanup(func() {
		mustExec(t, h.f.owner, fmt.Sprintf(`DROP TRIGGER IF EXISTS %s ON checkout.hosted_payment_pages`, trigger))
		mustExec(t, h.f.owner, fmt.Sprintf(`DROP FUNCTION IF EXISTS public.%s()`, fn))
		mustExec(t, h.f.owner, fmt.Sprintf(`DROP SEQUENCE IF EXISTS public.%s`, sequence))
	})
	return sequenceName
}

func hpAssertDelayedWriteEntered(t *testing.T, h hpHarness, sequenceName string) {
	t.Helper()
	var entered bool
	if err := h.f.owner.QueryRow(context.Background(), `SELECT is_called FROM public.`+pgx.Identifier{sequenceName}.Sanitize()).Scan(&entered); err != nil || !entered {
		t.Fatalf("HP05 delayed write trigger was never entered: %v", err)
	}
}

func hpAwaitOrderLock(t *testing.T, h hpHarness, blockerPID uint32) {
	t.Helper()
	ctx := context.Background()
	login := h.pool.Config().ConnConfig.User
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		var waiting bool
		err := h.f.owner.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_stat_activity
		 WHERE datname=current_database() AND usename=$1 AND state='active'
		 AND query LIKE '%checkout.take_hosted_page%' AND wait_event_type='Lock'
		 AND $2::int=ANY(pg_blocking_pids(pid)))`, login, int32(blockerPID)).Scan(&waiting)
		if err != nil {
			t.Fatal(err)
		}
		if waiting {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("HP04/HP05 take never reached the held order lock")
}

func TestBuyerPaymentHostedQualificationExpiresDuringFinalWrites(t *testing.T) {
	t.Run("prepare-insert", func(t *testing.T) {
		h := hpSetup(t)
		before := h.counts(t)
		sequence := hpDelayPageWrite(t, h, "INSERT")
		qualExec(t, h.f.owner, `UPDATE payments.account_qualifications SET expires_at=clock_timestamp()+interval '2 seconds' WHERE id=$1`, h.proof)
		if _, err := h.begin(t04Key("hp-expire-save")); !errors.Is(err, command.ErrConflict) {
			t.Fatalf("HP05 late insert did not return conflict: %v", err)
		}
		hpAssertDelayedWriteEntered(t, h, sequence)
		if h.counts(t) != before {
			t.Fatal("HP05 late preparation failure committed partial payment facts")
		}
	})
	t.Run("handoff-update", func(t *testing.T) {
		h := hpSetup(t)
		result, err := h.begin(t04Key("hp-expire-take"))
		if err != nil {
			t.Fatal(err)
		}
		sequence := hpDelayPageWrite(t, h, "UPDATE OF handed_out_at")
		qualExec(t, h.f.owner, `UPDATE payments.account_qualifications SET expires_at=clock_timestamp()+interval '2 seconds' WHERE id=$1`, h.proof)
		if _, err := h.take(); !errors.Is(err, command.ErrConflict) {
			t.Fatalf("HP05 late handoff update did not return conflict: %v", err)
		}
		hpAssertDelayedWriteEntered(t, h, sequence)
		_, _, _, handed := h.page(t, result.AttemptID)
		if handed != nil {
			t.Fatal("HP05 late take failure committed handoff timestamp")
		}
	})
}

func TestBuyerPaymentHostedCapabilityRevokedDuringTakeWait(t *testing.T) {
	h := hpSetup(t)
	result, err := h.begin(t04Key("hp-cap-wait"))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	tx, err := h.f.owner.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `SELECT id FROM checkout.orders WHERE id=$1 FOR UPDATE`, h.hold.OrderID); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { _, e := h.take(); done <- e }()
	hpAwaitOrderLock(t, h, tx.Conn().PgConn().PID())
	mustExec(t, h.f.owner, `UPDATE buyer.capability_sessions SET revoked_at=clock_timestamp() WHERE id=$1`, h.cap.Scope.SessionID)
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err == nil {
		t.Fatal("HP04 capability revoked during order wait released form")
	}
	_, _, _, handed := h.page(t, result.AttemptID)
	if handed != nil {
		t.Fatal("HP04 revoked capability committed handoff timestamp")
	}
}

func TestBuyerPaymentHostedLocalDeadlineFence(t *testing.T) {
	h := hpSetup(t)
	result, err := h.begin(t04Key("hp-deadline"))
	if err != nil {
		t.Fatal(err)
	}
	// Test-only owner time shift exercises the same database-clock comparison as
	// an elapsed minute, without adding a minute to every full suite run.
	mustExec(t, h.f.owner, `UPDATE checkout.payment_attempts SET created_at=clock_timestamp()-interval '61 seconds' WHERE id=$1`, result.AttemptID)
	if _, err := h.take(); err == nil {
		t.Fatal("HP05 expired local release window emitted a form")
	}
	_, _, _, handed := h.page(t, result.AttemptID)
	if handed != nil {
		t.Fatal("HP05 deadline denial consumed the handoff")
	}
}
