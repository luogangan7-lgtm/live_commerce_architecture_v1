package foundation_test

// SL04 REAL_PG half (contracts/stripe-live-enable-v1.md §11 SL04, §3.4 "re-created" list, §5.2 refund guard, brief ruling
// B5/S5). Prefix `slp`. The Go environment rules are internal/payments/stripe_live_sl04_test.go (same top-level name).
//
// For a LIVE kit and a SANDBOX (PROVIDER_MOCK) kit, on real definers:
//   - checkout observation (payments.apply_capture -> apply_stripe_observation): Livemode=true is admitted exactly for a
//     LIVE attempt and Livemode=false exactly for a SANDBOX attempt; the crossed report never creates a CAPTURED fact
//     and opens a PROVIDER_IDENTITY_MISMATCH review instead;
//   - refund apply (apply_stripe_refund): same rule, no SUCCEEDED fact from a crossed report;
//   - integration.record_stripe_charge_observation / record_stripe_refund_observation (worker lease): a crossed Livemode is
//     refused (22023 for the charge recorder) and the matching Livemode is not refused for that reason;
//   - merchant refund of an attempt of the other environment through the deployment's handler -> 422 not_refundable and
//     zero refund rows (S5), and the same request through the matching-environment handler succeeds (the refusal is the
//     guard, not the request).
// Owner-pool disclosure: as stripe_live_registrar_test.go (session/refund pins, observation rows); no fact, order or
// stock row is written by the test itself. No fake key is needed: nothing here calls Stripe.

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"

	"livecommerce/internal/httpapi"
)

type slpKit struct {
	name         string
	live         bool
	s            *slrEnv
	begin        func(psHarness) string
	right, wrong http.Handler
	profile      string
}

func slpHandler(t *testing.T, s *slrEnv, environment string) http.Handler {
	t.Helper()
	jobs, err := river.NewClient(riverpgxv5.New(s.p.f.runtime), &river.Config{Schema: "river_payment"})
	if err != nil {
		t.Fatal(err)
	}
	return httpapi.NewHandler(s.p.f.runtime, httpapi.Options{RefundJobs: jobs, PaymentEnvironment: environment})
}

func slpNewKit(t *testing.T, base *testFixture, live bool) *slpKit {
	t.Helper()
	k := &slpKit{live: live}
	if live {
		l := slrNew(t, base).live(t)
		k.name, k.s, k.profile = "LIVE", l.slrEnv, "LIVE"
		k.begin = func(p psHarness) string { return l.mustBegin(t, p) }
		k.right, k.wrong = slpHandler(t, k.s, "LIVE"), slpHandler(t, k.s, "SANDBOX")
		return k
	}
	s := slrNew(t, base)
	st := s.seed(t, s.p)
	k.name, k.s, k.profile = "SANDBOX", s, "PROVIDER_MOCK"
	k.begin = func(p psHarness) string {
		cur := st
		cur.p = p
		res, err := cur.begin(s.svc, t04Key("slp-start"), cur.input("zh-TW"))
		if err != nil {
			t.Fatalf("SANDBOX start: %v", err)
		}
		return res.AttemptID
	}
	k.right, k.wrong = slpHandler(t, s, "SANDBOX"), slpHandler(t, s, "LIVE")
	return k
}

func (k *slpKit) post(h http.Handler, p psHarness, amount, expected int64) (int, map[string]any) {
	body := strings.NewReader(`{"amount_minor":` + strconv.FormatInt(amount, 10) + `,"reason":"requested_by_customer","expected_refundable_minor":` + strconv.FormatInt(expected, 10) + `}`)
	req := httptest.NewRequest("POST", "/v1/admin/stores/"+p.f.storeA1+"/orders/"+p.hold.OrderID+"/refunds", body)
	req.Header.Set("Authorization", "Bearer "+p.f.tokens["a"])
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Idempotency-Key", "slp-"+t04Tag())
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	var out map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	return w.Code, out
}

func (k *slpKit) reviews(t *testing.T, attempt, reason string) int {
	t.Helper()
	return countRows(t, k.s.f.owner, `SELECT count(*) FROM payments.review_cases WHERE attempt_id=$1 AND reason=$2`, attempt, reason)
}

func (k *slpKit) facts(t *testing.T, attempt, kind string) int {
	t.Helper()
	return countRows(t, k.s.f.owner, `SELECT count(*) FROM payments.facts WHERE attempt_id=$1 AND kind=$2`, attempt, kind)
}

func TestStripeSL04LiveShape(t *testing.T) {
	base := pwIsolatedFixture(t) // LIVE rows stay out of the shared suite database
	t.Run("live_kit", func(t *testing.T) { slpRun(t, slpNewKit(t, base, true)) })
	t.Run("sandbox_kit", func(t *testing.T) { slpRun(t, slpNewKit(t, base, false)) })
	t.Run("refund_routes_are_not_mounted_for_an_unknown_environment", func(t *testing.T) {
		k := slpNewKit(t, base, true)
		unknown := slpHandler(t, k.s, "STAGING")
		status, _ := k.post(unknown, k.s.p, 2500, 2500)
		if status != 404 {
			t.Fatalf("refund route under environment STAGING answered %d, want 404 (not mounted)", status)
		}
	})
}

func slpRun(t *testing.T, k *slpKit) {
	ctx := context.Background()
	s := k.s
	live := k.live
	holds := []psHarness{s.p, sstMoreHold(t, s.p), sstMoreHold(t, s.p)}
	a1, a2, a3 := k.begin(holds[0]), k.begin(holds[1]), k.begin(holds[2])

	// --- checkout observation apply: crossed Livemode never captures; matching Livemode captures -----------------------------------
	_, _ = s.pin(t, a2)
	crossed := s.observe(t, a2, s.sessionReport(t, a2, "complete", "paid", "succeeded", !live))
	if err := s.apply(a2, crossed); err != nil {
		t.Fatalf("%s: crossed checkout observation must be a review, not an error: %v", k.name, err)
	}
	if k.facts(t, a2, "CAPTURED") != 0 || k.reviews(t, a2, "PROVIDER_IDENTITY_MISMATCH") != 1 {
		t.Fatalf("%s: crossed Livemode: CAPTURED=%d PROVIDER_IDENTITY_MISMATCH=%d, want 0/1", k.name, k.facts(t, a2, "CAPTURED"), k.reviews(t, a2, "PROVIDER_IDENTITY_MISMATCH"))
	}
	session1, pi1 := s.pin(t, a1)
	_ = session1
	matching := s.observe(t, a1, s.sessionReport(t, a1, "complete", "paid", "succeeded", live))
	if err := s.apply(a1, matching); err != nil {
		t.Fatalf("%s: matching checkout observation: %v", k.name, err)
	}
	if k.facts(t, a1, "CAPTURED") != 1 || k.reviews(t, a1, "PROVIDER_IDENTITY_MISMATCH") != 0 {
		t.Fatalf("%s: matching Livemode: CAPTURED=%d mismatch=%d, want 1/0", k.name, k.facts(t, a1, "CAPTURED"), k.reviews(t, a1, "PROVIDER_IDENTITY_MISMATCH"))
	}

	// --- refund apply -------------------------------------------------------------------------------------------------
	status, out := k.post(k.right, holds[0], 2500, 2500)
	if status != 201 {
		t.Fatalf("%s: refund through the matching handler answered %d: %v", k.name, status, out)
	}
	r1, _ := out["refund_id"].(string)
	re1 := s.pinRefund(t, r1, live)
	crossedRefund := s.observe(t, a1, s.refundReport(t, r1, a1, pi1, re1, "succeeded", 2500, !live))
	if err := s.apply(a1, crossedRefund); err != nil {
		t.Fatalf("%s: crossed refund observation must be a review, not an error: %v", k.name, err)
	}
	if n := countRows(t, s.f.owner, `SELECT count(*) FROM payments.refund_facts WHERE refund_id=$1`, r1); n != 0 || k.reviews(t, a1, "PROVIDER_IDENTITY_MISMATCH") != 1 {
		t.Fatalf("%s: crossed refund Livemode: facts=%d PROVIDER_IDENTITY_MISMATCH=%d, want 0/1", k.name, n, k.reviews(t, a1, "PROVIDER_IDENTITY_MISMATCH"))
	}
	matchingRefund := s.observe(t, a1, s.refundReport(t, r1, a1, pi1, re1, "succeeded", 2500, live))
	if err := s.apply(a1, matchingRefund); err != nil {
		t.Fatalf("%s: matching refund observation: %v", k.name, err)
	}
	if n := countRows(t, s.f.owner, `SELECT count(*) FROM payments.refund_facts WHERE refund_id=$1 AND kind='SUCCEEDED'`, r1); n != 1 {
		t.Fatalf("%s: matching refund Livemode did not produce the SUCCEEDED fact (%d)", k.name, n)
	}

	// --- worker recorders under a lease ----------------------------------------------------------------------------------------
	_, pi3 := s.pin(t, a3)
	hash := s.observe(t, a3, s.sessionReport(t, a3, "complete", "paid", "succeeded", live))
	if err := s.apply(a3, hash); err != nil {
		t.Fatal(err)
	}
	token := make([]byte, 32)
	if _, err := rand.Read(token); err != nil {
		t.Fatal(err)
	}
	var disposition string
	var generation int64
	if err := s.p.worker.QueryRow(ctx, `SELECT disposition,generation FROM integration.claim_operation($1::uuid,60,$2::bytea)`, a3, token).Scan(&disposition, &generation); err != nil || disposition != "claimed" {
		t.Fatalf("%s: claim of the attempt operation: %q %v", k.name, disposition, err)
	}
	var account string
	if err := s.f.owner.QueryRow(ctx, `SELECT account_id FROM payments.stripe_sessions WHERE attempt_id=$1`, a3).Scan(&account); err != nil {
		t.Fatal(err)
	}
	charge := func(livemode bool) error {
		report := map[string]any{"Provider": "stripe", "Version": 1, "Object": "charge", "Via": "retrieve", "AccountID": account, "KeyVersion": 1, "RequestID": "",
			"RefundRef": "", "PaymentIntentID": pi3, "ChargeID": "ch_slp1", "Currency": "TWD", "AmountCaptured": 2500, "AmountRefunded": 0, "Refunded": false,
			"Disputed": false, "Livemode": livemode, "LocalReason": ""}
		raw, _ := json.Marshal(report)
		_, err := s.p.worker.Exec(ctx, `SELECT integration.record_stripe_charge_observation($1::uuid,$2::bigint,$3::bytea,$4,$5::jsonb,1)`, a3, generation, token, k.profile, string(raw))
		return err
	}
	slrWant(t, k.name+" charge observation with a crossed Livemode", charge(!live), "22023", "")
	if err := charge(live); err != nil && slrCode(err) == "22023" {
		t.Fatalf("%s: charge observation with the matching Livemode was refused as a value error: %v", k.name, err)
	}

	// refund recorder: claim the refund operation, then compare the crossed and matching outcomes (they must differ:
	// Livemode is a gate, not a cosmetic field).
	status, out = k.post(k.right, holds[2], 1000, 2500)
	if status != 201 {
		t.Fatalf("%s: second refund answered %d: %v", k.name, status, out)
	}
	r3, _ := out["refund_id"].(string)
	rtoken := make([]byte, 32)
	if _, err := rand.Read(rtoken); err != nil {
		t.Fatal(err)
	}
	var rgen int64
	if err := s.p.worker.QueryRow(ctx, `SELECT disposition,generation FROM integration.claim_operation($1::uuid,60,$2::bytea)`, r3, rtoken).Scan(&disposition, &rgen); err != nil || disposition != "claimed" {
		t.Fatalf("%s: claim of the refund operation: %q %v", k.name, disposition, err)
	}
	record := func(livemode bool) error {
		rep := s.refundReport(t, r3, a3, pi3, "re_slp_"+t04Tag(), "pending", 1000, livemode)
		raw, _ := json.Marshal(rep)
		_, err := s.p.worker.Exec(ctx, `SELECT integration.record_stripe_refund_observation($1::uuid,$2::bigint,$3::bytea,$4,$5::jsonb,1)`, r3, rgen, rtoken, k.profile, string(raw))
		return err
	}
	bad, good := record(!live), record(live)
	if bad == nil {
		t.Fatalf("%s: refund observation with a crossed Livemode was accepted", k.name)
	}
	slrWant(t, k.name+" refund observation with a crossed Livemode", bad, "PT409", "")
	if good != nil && slrMsg(good) == slrMsg(bad) {
		t.Fatalf("%s: crossed and matching Livemode fail identically (%v): Livemode is not checked", k.name, bad)
	}

	// --- S5: the refund route of a deployment refuses an attempt of the other environment -----------------------------------------------
	before := countRows(t, s.f.owner, `SELECT count(*) FROM payments.stripe_refunds WHERE attempt_id=$1`, a3)
	status, out = k.post(k.wrong, holds[2], 500, 1500)
	if status != 422 || out["code"] != "not_refundable" {
		t.Fatalf("%s: refund through the other environment's handler answered %d %v, want 422 not_refundable", k.name, status, out)
	}
	if after := countRows(t, s.f.owner, `SELECT count(*) FROM payments.stripe_refunds WHERE attempt_id=$1`, a3); after != before {
		t.Fatalf("%s: the refused refund left rows: %d -> %d", k.name, before, after)
	}
	if status, out = k.post(k.right, holds[2], 500, 1500); status != 201 {
		t.Fatalf("%s: the same request through the matching handler answered %d %v", k.name, status, out)
	}
	if after := countRows(t, s.f.owner, `SELECT count(*) FROM payments.stripe_refunds WHERE attempt_id=$1`, a3); after != before+1 {
		t.Fatalf("%s: matching request created %d rows", k.name, after-before)
	}
}
