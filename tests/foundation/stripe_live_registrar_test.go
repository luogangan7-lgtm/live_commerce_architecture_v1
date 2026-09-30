package foundation_test

// SL03 (contracts/stripe-live-enable-v1.md §11 SL03, §3.4, §4, §6, §7): the LIVE registrar SQL definers through a real
// commerce_payment_registrar login, REAL_PG. Prefix `slr`. Also the shared LIVE store harness of SL02/SL04/SL05/SL09
// (`slrNew`, `slrLive`), because a LIVE store cannot come from a real LIVE flow in CI (no live key exists here).
//
// Written from the contract, not from the implementation: the assertions name the contract's SQLSTATE and codes
// (42501 grant predicate, 22023 `stripe_live_readiness_unknown` / `stripe_live_max_unruled`, PT409 replay/state).
// `LiveApprove`'s Stripe read (GET /v1/account) is SL01/SL08's; here the readiness jsonb is supplied by the test, and
// no mock transport ever admits a live key.
//
// OWNER-POOL DISCLOSURE (every insert/update as the migration owner, each logged as `OWNER-POOL:` in the run log):
//   - identity.store_grants payments:refund + orders:read for the fixture principal (0065 onboarding grants do not
//     apply to psSetup stores; same disclosure as rfx.grant);
//   - LIVE session/payment-intent pin (payments.stripe_sessions) and refund pin (payments.stripe_refunds): the worker
//     that pins them needs a live key; the guards allow NULL->value once;
//   - payments.provider_observations rows (the worker's report): the FACT itself is decided by the real definers
//     payments.apply_capture -> apply_stripe_observation / apply_stripe_refund as commerce_worker;
//   - payments.stripe_webhook_receipts ACCEPTED rows for the LIVE endpoint (the real ingress is SL05's);
//   - a PROVIDER_MOCK qualification on a LIVE connection and tamper/restore UPDATEs under
//     session_replication_role=replica (each restored before the next case) to isolate one canary predicate.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"

	"livecommerce/internal/checkout"
	"livecommerce/internal/command"
	"livecommerce/internal/httpapi"
	"livecommerce/internal/integrations/accounts"
	"livecommerce/internal/integrations/psp/stripe"
	"livecommerce/internal/integrations/psp/stripe/stripetest"
	"livecommerce/internal/payments/stripeadmin"
	"livecommerce/internal/payments/stripewebhook"
	"livecommerce/internal/platform"
)

// slrChecklist is the §9 code set, written from the contract table (not from stripeadmin.LiveChecklist).
var slrChecklist = []string{"account_active", "descriptor", "payout_bank", "rak_live", "webhook_live", "radar_default",
	"three_ds", "dispute_notice", "policy_pages", "managed_off", "canary_private"}

func slrCode(err error) string {
	var pe *pgconn.PgError
	if errors.As(err, &pe) {
		return pe.Code
	}
	return ""
}

func slrMsg(err error) string {
	var pe *pgconn.PgError
	if errors.As(err, &pe) {
		return pe.Message
	}
	if err != nil {
		return err.Error()
	}
	return ""
}

// slrWant fails unless err is a PostgreSQL error with the wanted SQLSTATE (and, when given, message fragment).
func slrWant(t *testing.T, what string, err error, code, fragment string) {
	t.Helper()
	if err == nil {
		t.Fatalf("%s: accepted, want SQLSTATE %s", what, code)
	}
	if slrCode(err) != code || (fragment != "" && !strings.Contains(slrMsg(err), fragment)) {
		t.Fatalf("%s: got %s %q, want SQLSTATE %s %q", what, slrCode(err), slrMsg(err), code, fragment)
	}
}

func slrRejected(t *testing.T, what string, err error) {
	t.Helper()
	if err == nil {
		t.Fatalf("%s: accepted, want rejection", what)
	}
	if slrCode(err) == "" {
		t.Fatalf("%s: not a database rejection: %v", what, err)
	}
}

func slrDisclose(t *testing.T, what string) { t.Helper(); t.Logf("OWNER-POOL: %s", what) }

// slrReplica runs one owner-pool statement with session_replication_role=replica (row triggers off, CHECKs on).
// Disclosed tamper fixture: every caller restores the row before the next case.
func slrReplica(t *testing.T, f *testFixture, sql string, args ...any) {
	t.Helper()
	slrDisclose(t, "replica-mode tamper/restore: "+strings.Fields(sql)[0]+" "+strings.Fields(sql)[1]+" "+strings.Fields(sql)[2])
	ctx := context.Background()
	tx, err := f.owner.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	if _, err = tx.Exec(ctx, `SET LOCAL session_replication_role = replica`); err != nil {
		t.Fatal(err)
	}
	if tag, err := tx.Exec(ctx, sql, args...); err != nil || tag.RowsAffected() == 0 {
		t.Fatalf("tamper %q: rows=%d err=%v", sql, tag.RowsAffected(), err)
	}
	if err = tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
}

// slrEnv is one TWD store with a real registrar login for the raw SQL definers, the Open registrar (mock transport,
// no pair) for the S8 Go paths, a LIVE-profile hosted starter and the merchant refund handler of a LIVE deployment.
type slrEnv struct {
	*sstEnv
	p                      psHarness
	pool                   *pgxpool.Pool
	scope                  stripeadmin.Scope
	conn, binding, account string
	handler                http.Handler
}

func slrNew(t *testing.T, base *testFixture) *slrEnv {
	t.Helper()
	ctx := context.Background()
	p := psSetupItemsOn(t, base, 1)
	e := sstNewEnv(t, p.f, sstKeyring(t, "sst_api", randomBytes(32)))
	pool, err := platform.OpenStripeRegistrarPool(ctx, sstLogin(t, p.f, "commerce_payment_registrar"))
	if err != nil {
		t.Fatalf("registrar pool: %v", err)
	}
	t.Cleanup(pool.Close)
	s := &slrEnv{sstEnv: e, p: p, pool: pool, scope: stripeadmin.Scope{TenantID: p.f.tenantA, StoreID: p.f.storeA1, PrincipalID: p.f.principalA}}
	s.grant(t, s.scope.PrincipalID, "payments:refund", "orders:read")
	jobs, err := river.NewClient(riverpgxv5.New(p.f.runtime), &river.Config{Schema: "river_payment"})
	if err != nil {
		t.Fatal(err)
	}
	// A LIVE deployment's merchant API: refund routes mounted for environment LIVE (contract §5.2 refund guard).
	s.handler = httpapi.NewHandler(p.f.runtime, httpapi.Options{RefundJobs: jobs, PaymentEnvironment: "LIVE"})
	return s
}

func (s *slrEnv) grant(t *testing.T, principal string, perms ...string) {
	t.Helper()
	slrDisclose(t, "identity.store_grants "+strings.Join(perms, ","))
	for _, perm := range perms {
		mustExec(t, s.f.owner, `INSERT INTO identity.store_grants(tenant_id,store_id,principal_id,permission) VALUES($1,$2,$3,$4) ON CONFLICT DO NOTHING`,
			s.scope.TenantID, s.scope.StoreID, principal, perm)
	}
}

// member adds a same-tenant principal holding exactly perms on the store.
func (s *slrEnv) member(t *testing.T, perms ...string) string {
	t.Helper()
	id := randomUUID()
	slrDisclose(t, "identity.principals/memberships/store_grants of an extra merchant principal")
	mustExec(t, s.f.owner, `INSERT INTO identity.principals(id) VALUES($1)`, id)
	mustExec(t, s.f.owner, `INSERT INTO identity.memberships(tenant_id,principal_id) VALUES($1,$2)`, s.scope.TenantID, id)
	s.grant(t, id, perms...)
	return id
}

func (s *slrEnv) audit(t *testing.T, action string) int {
	t.Helper()
	return countRows(t, s.f.owner, `SELECT count(*) FROM ops.audit_events WHERE tenant_id=$1 AND store_id=$2 AND action=$3`, s.scope.TenantID, s.scope.StoreID, action)
}

// registerAccount registers a Stripe account of environment env through the real definer (sealed bytes are random
// and never opened here).
func (s *slrEnv) registerAccount(t *testing.T, env string) error {
	t.Helper()
	s.conn, s.binding, s.account = randomUUID(), randomUUID(), "acct_"+strings.ToUpper(env[:1])+t04Tag()
	var got string
	return s.pool.QueryRow(context.Background(), `SELECT integration.register_stripe_account($1::uuid,$2::uuid,$3::uuid,$4::uuid,$5::uuid,$6,$7,'fixture_key',$8::bytea,$9::bytea)`,
		s.scope.TenantID, s.scope.StoreID, s.scope.PrincipalID, s.conn, s.binding, env, s.account, randomBytes(12), randomBytes(48)).Scan(&got)
}

func (s *slrEnv) registerLive(t *testing.T) {
	t.Helper()
	if err := s.registerAccount(t, "LIVE"); err != nil {
		t.Fatalf("register LIVE account: %v", err)
	}
}

type slrApproval struct {
	ID, Connection, Currency, Ref, Principal string
	ApprovedAt                               time.Time
	Canary, Max                              int64
	Checklist                                []string
	Readiness                                map[string]any
}

func slrReadiness() map[string]any {
	return map[string]any{"ChargesEnabled": true, "PayoutsEnabled": true, "DetailsSubmitted": true, "CurrentlyDueCount": 0,
		"DescriptorLength": 12, "PrefixLength": 6, "CVCRule": true, "AVSRule": false}
}

// approval is a valid TWD approval (canary cap 2x the TWD minimum, per-order max LQ3) approved a minute ago.
func (s *slrEnv) approval() slrApproval {
	return slrApproval{ID: randomUUID(), Connection: s.conn, Currency: "TWD", Ref: "owner-chat:2026-09-30:stripe-live:" + t04Tag(),
		Principal: s.scope.PrincipalID, ApprovedAt: time.Now().Add(-time.Minute).UTC(), Canary: 5000, Max: 2000000,
		Checklist: append([]string(nil), slrChecklist...), Readiness: slrReadiness()}
}

func (s *slrEnv) approve(a slrApproval) (string, error) {
	raw, err := json.Marshal(a.Readiness)
	if err != nil {
		return "", err
	}
	var got string
	err = s.pool.QueryRow(context.Background(), `SELECT payments.approve_stripe_live($1::uuid,$2::uuid,$3::uuid,$4::uuid,$5::uuid,$6,$7,$8::timestamptz,$9::bigint,$10::bigint,$11::text[],$12::jsonb)`,
		s.scope.TenantID, s.scope.StoreID, a.Principal, a.ID, a.Connection, a.Currency, a.Ref, a.ApprovedAt, a.Canary, a.Max, a.Checklist, string(raw)).Scan(&got)
	return got, err
}

func (s *slrEnv) mustApprove(t *testing.T) slrApproval {
	t.Helper()
	a := s.approval()
	if got, err := s.approve(a); err != nil || got != a.ID {
		t.Fatalf("approve: %q %v", got, err)
	}
	return a
}

func (s *slrEnv) approvals(t *testing.T) int {
	t.Helper()
	return countRows(t, s.f.owner, `SELECT count(*) FROM payments.stripe_live_approvals WHERE connection_id=$1`, s.conn)
}

func (s *slrEnv) qualify(t *testing.T, profile, evidence string, observed, expires time.Time, credentialVersion int64) (string, error) {
	t.Helper()
	id := randomUUID()
	var got string
	err := s.pool.QueryRow(context.Background(), `SELECT payments.qualify_stripe_method($1::uuid,$2::uuid,$3::uuid,$4::uuid,$5::uuid,$6::bigint,$7,$8,$9::timestamptz,$10::timestamptz)`,
		s.scope.TenantID, s.scope.StoreID, s.scope.PrincipalID, id, s.conn, credentialVersion, profile, evidence, observed, expires).Scan(&got)
	return id, err
}

func slrProbe() string { return "stripe-probe:cs_live_" + t04Tag() + t04Tag() }

func (s *slrEnv) mustQualify(t *testing.T) string {
	t.Helper()
	id, err := s.qualify(t, "LIVE", slrProbe(), time.Now().Add(-time.Second), time.Now().Add(time.Hour), 1)
	if err != nil {
		t.Fatalf("LIVE qualify: %v", err)
	}
	return id
}

// method calls payments.set_stripe_method on the fixture market (TW) and returns the new head version.
func (s *slrEnv) method(t *testing.T, market, country, qualification string, expected int64, enabled bool, min, max int64) (int64, error) {
	t.Helper()
	var v int64
	err := s.pool.QueryRow(context.Background(), `SELECT payments.set_stripe_method($1::uuid,$2::uuid,$3::uuid,$4::uuid,$5,$6::uuid,$7::uuid,$8::bigint,$9,true,1,$10::bigint,$11::bigint,'Stripe','Stripe','Stripe')`,
		s.scope.TenantID, s.scope.StoreID, s.scope.PrincipalID, market, country, s.conn, qualification, expected, enabled, min, max).Scan(&v)
	return v, err
}

func (s *slrEnv) twMethod(t *testing.T, qualification string, expected int64, enabled bool, max int64) (int64, error) {
	return s.method(t, s.p.market.ID, "TW", qualification, expected, enabled, 2500, max)
}

// slrLive is a LIVE store walked to CANARY through the real definers: LIVE account, LIVE endpoint, approval, REAL_LIVE
// qualification and an enabled method capped at the canary cap.
type slrLive struct {
	*slrEnv
	approval slrApproval
	qual     string
	endp     string
	secret   string
	regLive  *stripeadmin.Registrar
	version  int64
	starter  *checkout.HostedPaymentStarter
	hook     *httptest.Server
}

func (s *slrEnv) live(t *testing.T) *slrLive {
	t.Helper()
	s.registerLive(t)
	l := &slrLive{slrEnv: s}
	l.endpoint(t)
	l.approval = s.mustApprove(t)
	l.qual = s.mustQualify(t)
	v, err := s.twMethod(t, l.qual, 0, true, 5000)
	if err != nil {
		t.Fatalf("LIVE method at the canary cap: %v", err)
	}
	l.version = v
	l.starter = s.service(t, "LIVE", s.scfg)
	return l
}

// begin starts a LIVE Stripe attempt for the hold of p through the real hosted service (profile LIVE).
func (l *slrLive) begin(p psHarness) (checkout.PaymentResult, error) {
	return l.starter.BeginHosted(context.Background(), p.cap.Token, p.f.storeA1, t04Key("slr-start"),
		checkout.HostedInput{OrderID: p.hold.OrderID, MethodCode: "stripe_checkout", MethodVersion: l.version, Locale: "zh-TW"})
}

func (l *slrLive) mustBegin(t *testing.T, p psHarness) string {
	t.Helper()
	res, err := l.begin(p)
	if err != nil {
		t.Fatalf("LIVE start: %v", err)
	}
	return res.AttemptID
}

// newHold returns another buyer/order/hold of the same store (2 of its 10 units each).
func (l *slrLive) newHold(t *testing.T) psHarness { t.Helper(); return sstMoreHold(t, l.p) }

func (s *slrEnv) pin(t *testing.T, attempt string) (session, pi string) {
	t.Helper()
	slrDisclose(t, "payments.stripe_sessions session + payment intent pin")
	var environment string
	if err := s.f.owner.QueryRow(context.Background(), `SELECT environment FROM checkout.payment_attempts WHERE id=$1`, attempt).Scan(&environment); err != nil {
		t.Fatal(err)
	}
	mode := map[string]string{"LIVE": "live", "SANDBOX": "test"}[environment]
	session, pi = "cs_"+mode+"_"+t04Tag()+t04Tag(), "pi_"+mode+"_"+t04Tag()+t04Tag()
	mustExec(t, s.f.owner, `UPDATE integration.operations SET provider_reference=$2 WHERE id=$1`, attempt, session)
	mustExec(t, s.f.owner, `UPDATE payments.stripe_sessions SET create_first_sent_at=clock_timestamp(),create_last_sent_at=clock_timestamp(),
	 create_send_count=1,create_body_sha256=$2,session_id=$3,session_url=$4,payment_intent_id=$5,pinned_at=clock_timestamp() WHERE attempt_id=$1`,
		attempt, randomBytes(32), session, "https://checkout.stripe.com/c/pay/"+session, pi)
	return
}

// observe inserts the worker's report as an observation (owner pool) and returns its hash.
func (s *slrEnv) observe(t *testing.T, attempt string, report map[string]any) []byte {
	t.Helper()
	slrDisclose(t, "payments.provider_observations report")
	raw, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	var hash []byte
	if err := s.f.owner.QueryRow(context.Background(), `WITH a AS (SELECT tenant_id,store_id,id,execution_profile,environment FROM checkout.payment_attempts WHERE id=$1)
	 INSERT INTO payments.provider_observations(tenant_id,store_id,attempt_id,source,execution_profile,environment,first_generation,report,report_hash)
	 SELECT tenant_id,store_id,id,'QUERY',execution_profile,environment,2,$2::jsonb,sha256(convert_to($2::jsonb::text,'UTF8')) FROM a RETURNING report_hash`,
		attempt, string(raw)).Scan(&hash); err != nil {
		t.Fatalf("observation: %v", err)
	}
	return hash
}

// apply runs the real dispatcher payments.apply_capture as commerce_worker (the reconcile job's SQL).
func (s *slrEnv) apply(attempt string, hash []byte) error {
	_, err := s.p.worker.Exec(context.Background(), `SELECT payments.apply_capture($1::uuid,$2::bytea)`, attempt, hash)
	return err
}

// sessionReport is the checkout observation of a LIVE session (livemode and money per the caller).
func (s *slrEnv) sessionReport(t *testing.T, attempt string, status, paymentStatus, piStatus string, livemode bool) map[string]any {
	t.Helper()
	var account, session, currency, profile string
	var expires, unit int64
	if err := s.f.owner.QueryRow(context.Background(), `SELECT s.account_id,coalesce(s.session_id,''),a.currency,a.execution_profile,extract(epoch FROM s.expires_at)::bigint,s.unit_amount
	 FROM payments.stripe_sessions s JOIN checkout.payment_attempts a ON a.id=s.attempt_id WHERE s.attempt_id=$1`, attempt).Scan(&account, &session, &currency, &profile, &expires, &unit); err != nil {
		t.Fatal(err)
	}
	received := int64(0)
	if paymentStatus == "paid" {
		received = unit
	}
	return map[string]any{"Provider": "stripe", "Version": 1, "Via": "retrieve", "AccountID": account, "SessionID": session, "ClientReferenceID": attempt,
		"MetadataAttempt": attempt, "MetadataProfile": profile, "Mode": "payment", "PaymentMethodTypes": []string{"card"}, "Livemode": livemode,
		"ExpiresAt": expires, "Status": status, "PaymentStatus": paymentStatus, "PaymentIntentStatus": piStatus, "Currency": currency,
		"AmountTotal": unit, "AmountSubtotal": unit, "AmountDiscount": 0, "AmountTax": 0, "AmountShipping": 0,
		"PaymentIntentAmountReceived": received, "PaymentIntentCurrency": currency, "PresentmentCurrency": "", "CurrencyConversion": false}
}

// capture drives one LIVE attempt to CAPTURED through the real definer (session pin + paid observation).
func (l *slrLive) capture(t *testing.T, attempt string) (session, pi string) {
	t.Helper()
	session, pi = l.pin(t, attempt)
	hash := l.observe(t, attempt, l.sessionReport(t, attempt, "complete", "paid", "succeeded", true))
	if err := l.apply(attempt, hash); err != nil {
		t.Fatalf("apply capture: %v", err)
	}
	if n := countRows(t, l.f.owner, `SELECT count(*) FROM payments.facts WHERE attempt_id=$1 AND kind='CAPTURED' AND environment='LIVE'`, attempt); n != 1 {
		t.Fatalf("LIVE CAPTURED facts=%d", n)
	}
	return
}

func (l *slrLive) call(method, path string, body string, key string) (int, map[string]any) {
	var r *strings.Reader
	if body != "" {
		r = strings.NewReader(body)
	} else {
		r = strings.NewReader("")
	}
	req := httptest.NewRequest(method, path, r)
	req.Header.Set("Authorization", "Bearer "+l.p.f.tokens["a"])
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	if key != "" {
		req.Header.Set("Idempotency-Key", key)
	}
	w := httptest.NewRecorder()
	l.handler.ServeHTTP(w, req)
	var out map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	return w.Code, out
}

// requestRefund posts a merchant refund through the LIVE-deployment handler (real request_stripe_refund definer).
func (l *slrLive) requestRefund(p psHarness, amount, expected int64) (int, map[string]any) {
	return l.call("POST", "/v1/admin/stores/"+p.f.storeA1+"/orders/"+p.hold.OrderID+"/refunds",
		fmt.Sprintf(`{"amount_minor":%d,"reason":"requested_by_customer","expected_refundable_minor":%d}`, amount, expected), "slr-"+t04Tag())
}

// pinRefund sets the Stripe refund id once (owner pool; the worker that pins it needs a live key).
func (s *slrEnv) pinRefund(t *testing.T, refund string, live bool) string {
	t.Helper()
	slrDisclose(t, "payments.stripe_refunds Stripe refund pin")
	re := "re_test_" + t04Tag()
	if live {
		re = "re_live_" + t04Tag()
	}
	mustExec(t, s.f.owner, `UPDATE payments.stripe_refunds SET stripe_refund_id=$2,pinned_at=clock_timestamp() WHERE id=$1 AND stripe_refund_id IS NULL`, refund, re)
	return re
}

// refundReport is the worker's refund observation (the 25 keys of the record definer), status and livemode per caller.
func (s *slrEnv) refundReport(t *testing.T, refund, attempt, pi, stripeRefund, status string, amount int64, livemode bool) map[string]any {
	t.Helper()
	var account string
	if err := s.f.owner.QueryRow(context.Background(), `SELECT account_id FROM payments.stripe_refunds WHERE id=$1`, refund).Scan(&account); err != nil {
		t.Fatal(err)
	}
	return map[string]any{"Provider": "stripe", "Version": 1, "Object": "refund", "Via": "retrieve", "AccountID": account,
		"KeyVersion": 1, "RequestID": "", "SendCount": 0, "RefundRef": refund, "AttemptRef": attempt, "RefundID": stripeRefund, "Status": status,
		"FailureReason": "", "PendingReason": "", "Amount": amount, "Currency": "TWD", "PaymentIntentID": pi, "Livemode": livemode,
		"MetadataRefund": refund, "MetadataAttempt": attempt, "ErrorClass": "", "ErrorCode": "", "HTTPStatus": 200, "ListMatchCount": nil, "LocalReason": ""}
}

// refundSucceeded pins the refund and applies a succeeded LIVE refund observation through the real definer.
func (l *slrLive) refundSucceeded(t *testing.T, refund, attempt, pi string, amount int64) string {
	t.Helper()
	re := l.pinRefund(t, refund, true)
	hash := l.observe(t, attempt, l.refundReport(t, refund, attempt, pi, re, "succeeded", amount, true))
	if err := l.apply(attempt, hash); err != nil {
		t.Fatalf("apply refund observation: %v", err)
	}
	if n := countRows(t, l.f.owner, `SELECT count(*) FROM payments.refund_facts WHERE refund_id=$1 AND kind='SUCCEEDED'`, refund); n != 1 {
		t.Fatalf("SUCCEEDED refund facts=%d", n)
	}
	return re
}

// endpoint registers the LIVE webhook endpoint through the real registrar (OpenLive: real sealing under the signing
// keyring, no network) and starts the real LIVE ingress handler behind httptest over the ingress login.
func (l *slrLive) endpoint(t *testing.T) {
	t.Helper()
	ctx := context.Background()
	reg, err := stripeadmin.OpenLive(ctx, sstLogin(t, l.f, "commerce_payment_registrar"), l.keys, l.signing,
		stripe.LiveApproval{Enabled: true, Reference: "owner-chat:2026-09-30:stripe-live:slr-fixture"})
	if err != nil {
		t.Fatalf("OpenLive: %v", err)
	}
	t.Cleanup(reg.Close)
	l.regLive = reg
	l.secret = swhSecret()
	var version int64
	if l.endp, version, err = reg.SetWebhookEndpoint(ctx, l.scope, stripeadmin.EndpointInput{ConnectionID: l.conn, Profile: "LIVE", Enabled: true,
		Secrets: accounts.StripeWebhookSecrets{CurrentSecret: l.secret}}); err != nil || version != 1 {
		t.Fatalf("LIVE webhook endpoint: %d %v", version, err)
	}
	pool, err := platform.OpenStripeIngressPool(ctx, sstLogin(t, l.f, "commerce_stripe_ingress"))
	if err != nil {
		t.Fatalf("ingress pool: %v", err)
	}
	t.Cleanup(pool.Close)
	inbox, err := stripewebhook.NewInbox(ctx, pool, l.signing, "LIVE")
	if err != nil {
		t.Fatalf("LIVE inbox: %v", err)
	}
	handler, err := stripewebhook.NewHandler(inbox)
	if err != nil {
		t.Fatal(err)
	}
	l.hook = httptest.NewServer(handler)
	t.Cleanup(l.hook.Close)
}

// deliver posts one correctly signed body to the LIVE endpoint through the real ingress and returns the status.
func (l *slrLive) deliver(t *testing.T, endpoint string, body []byte) int {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, l.hook.URL+"/v1/stripe/webhook/"+endpoint, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Stripe-Signature", stripetest.SignWebhook(l.secret, body, time.Now()))
	res, err := (&http.Client{Timeout: 15 * time.Second}).Do(req)
	if err != nil {
		t.Fatalf("webhook request: %v", err)
	}
	defer res.Body.Close()
	_, _ = io.Copy(io.Discard, res.Body)
	return res.StatusCode
}

// checkoutEvent delivers checkout.session.completed (livemode true) for the LIVE attempt's pinned session.
func (l *slrLive) checkoutEvent(t *testing.T, attempt, session string) {
	t.Helper()
	body := stripetest.EventBody(stripetest.EventOpts{ID: swhEventID("slrc"), Type: "checkout.session.completed", SessionID: session, ClientRef: attempt, Attempt: attempt, Livemode: true})
	if st := l.deliver(t, l.endp, body); st != 200 {
		t.Fatalf("LIVE checkout webhook answered %d", st)
	}
}

// refundEvent delivers refund.updated (livemode true) for the pinned LIVE refund.
func (l *slrLive) refundEvent(t *testing.T, refund, attempt, pi, stripeRefund string) {
	t.Helper()
	body := stripetest.RawEvent(swhEventID("slrr"), "refund.updated", true, map[string]any{"id": stripeRefund, "object": "refund", "amount": 2500, "currency": "twd",
		"payment_intent": pi, "status": "succeeded", "metadata": map[string]string{"lc_refund": refund, "lc_attempt": attempt}, "livemode": true})
	if st := l.deliver(t, l.endp, body); st != 200 {
		t.Fatalf("LIVE refund webhook answered %d", st)
	}
}

// canaryReady walks one LIVE order to the state record_stripe_live_canary accepts: captured, fully refunded, webhook
// evidence for both. It returns the attempt, refund and the ids the negatives tamper with.
type slrCanary struct {
	attempt, refund, order, session, pi, stripeRefund string
	hold                                              psHarness
}

func (l *slrLive) canaryReady(t *testing.T) slrCanary {
	t.Helper()
	c := slrCanary{hold: l.p, order: l.p.hold.OrderID}
	c.attempt = l.mustBegin(t, l.p)
	c.session, c.pi = l.capture(t, c.attempt)
	status, out := l.requestRefund(l.p, 2500, 2500)
	if status != 201 {
		t.Fatalf("LIVE merchant refund answered %d: %v", status, out)
	}
	c.refund, _ = out["refund_id"].(string)
	c.stripeRefund = l.refundSucceeded(t, c.refund, c.attempt, c.pi, 2500)
	l.checkoutEvent(t, c.attempt, c.session)
	l.refundEvent(t, c.refund, c.attempt, c.pi, c.stripeRefund)
	if n := countRows(t, l.f.owner, `SELECT count(*) FROM payments.stripe_webhook_receipts WHERE endpoint_id=$1 AND disposition='ACCEPTED' AND environment='LIVE' AND attempt_id=$2`, l.endp, c.attempt); n != 2 {
		t.Fatalf("ACCEPTED LIVE receipts (attempt + refund)=%d, want 2", n)
	}
	return c
}

func (l *slrLive) canary(a slrApproval, attempt, refund string) (time.Time, error) {
	var at time.Time
	err := l.pool.QueryRow(context.Background(), `SELECT payments.record_stripe_live_canary($1::uuid,$2::uuid,$3::uuid,$4::uuid,$5::uuid,$6::uuid)`,
		l.scope.TenantID, l.scope.StoreID, l.scope.PrincipalID, a.ID, attempt, refund).Scan(&at)
	return at, err
}

func (l *slrLive) revoke(a slrApproval, principal, ref string) (time.Time, error) {
	var at time.Time
	err := l.pool.QueryRow(context.Background(), `SELECT payments.revoke_stripe_live($1::uuid,$2::uuid,$3::uuid,$4::uuid,$5)`,
		l.scope.TenantID, l.scope.StoreID, principal, a.ID, ref).Scan(&at)
	return at, err
}

func slrRef(prefix string) string { return prefix + ":" + t04Tag() }

// ---------------------------------------------------------------------------------------------------------------------
// SL03
// ---------------------------------------------------------------------------------------------------------------------

func TestStripeSL03Registrar(t *testing.T) {
	// One labelled, isolated PG container: LIVE rows never reach the shared suite database.
	base := pwIsolatedFixture(t)
	t.Run("approve_rejections_replay_and_row", func(t *testing.T) { slrApproveGate(t, base) })
	t.Run("sandbox_account_cannot_be_approved", func(t *testing.T) {
		s := slrNew(t, base)
		if err := s.registerAccount(t, "SANDBOX"); err != nil {
			t.Fatal(err)
		}
		_, err := s.approve(s.approval())
		slrRejected(t, "approve on a SANDBOX account", err)
		if s.approvals(t) != 0 {
			t.Fatal("a SANDBOX account got an approval row")
		}
		// The reverse pairing of qualify: profile LIVE only on a LIVE account.
		_, err = s.qualify(t, "LIVE", slrProbe(), time.Now().Add(-time.Second), time.Now().Add(time.Hour), 1)
		slrRejected(t, "LIVE qualify on a SANDBOX account", err)
		if n := countRows(t, s.f.owner, `SELECT count(*) FROM payments.account_qualifications WHERE connection_id=$1`, s.conn); n != 0 {
			t.Fatalf("rejected qualification left %d rows", n)
		}
	})
	t.Run("endpoint_profile_and_account_environment_must_pair", func(t *testing.T) {
		sb := slrNew(t, base)
		if err := sb.registerAccount(t, "SANDBOX"); err != nil {
			t.Fatal(err)
		}
		lv := slrNew(t, base)
		lv.registerLive(t)
		set := func(s *slrEnv, profile string) error {
			var v int64
			return s.pool.QueryRow(context.Background(), `SELECT payments.set_stripe_webhook_endpoint($1::uuid,$2::uuid,$3::uuid,$4::uuid,$5::uuid,$6,0,true,'fixture_signing',$7::bytea,$8::bytea)`,
				s.scope.TenantID, s.scope.StoreID, s.scope.PrincipalID, s.conn, randomUUID(), profile, randomBytes(12), randomBytes(48)).Scan(&v)
		}
		for _, profile := range []string{"SANDBOX", "PROVIDER_MOCK"} {
			slrWant(t, profile+" endpoint on a LIVE account", set(lv, profile), "PT409", "")
		}
		slrWant(t, "LIVE endpoint on a SANDBOX account", set(sb, "LIVE"), "PT409", "")
		if err := set(sb, "PROVIDER_MOCK"); err != nil {
			t.Fatalf("PROVIDER_MOCK endpoint on a SANDBOX account: %v", err)
		}
		if err := set(lv, "LIVE"); err != nil {
			t.Fatalf("LIVE endpoint on a LIVE account (no approval needed, §2 step 2): %v", err)
		}
		if n := countRows(t, lv.f.owner, `SELECT count(*) FROM payments.stripe_webhook_endpoints WHERE connection_id=$1`, lv.conn); n != 1 {
			t.Fatalf("endpoints of the LIVE account=%d", n)
		}
	})
	t.Run("qualify_and_method_gates", func(t *testing.T) { slrQualifyMethodGate(t, base) })
	t.Run("canary_negatives_success_replay_and_caps", func(t *testing.T) { slrCanaryGate(t, base) })
	t.Run("revoke_reconcile_refund_and_kill_switch", func(t *testing.T) { slrRevokeGate(t, base) })
	t.Run("concurrent_qualify_and_revoke", func(t *testing.T) { slrConcurrentGate(t, base) })
	t.Run("disable_in_every_state", func(t *testing.T) { slrDisableGate(t, base) })
}

func slrApproveGate(t *testing.T, base *testFixture) {
	s := slrNew(t, base)
	s.registerLive(t)
	before := s.audit(t, "stripe.live.approve")
	reject := func(name, code, fragment string, edit func(*slrApproval)) {
		t.Helper()
		a := s.approval()
		edit(&a)
		_, err := s.approve(a)
		if code == "" {
			slrRejected(t, name, err)
		} else {
			slrWant(t, name, err, code, fragment)
		}
		if n := s.approvals(t); n != 0 {
			t.Fatalf("%s: rejected approval left %d rows", name, n)
		}
		if got := s.audit(t, "stripe.live.approve"); got != before {
			t.Fatalf("%s: rejected approval wrote an audit row", name)
		}
	}
	// LD2 grant predicate: integration:manage AND payments:refund on the store (else 42501).
	noRefund := s.member(t, "integration:manage")
	reject("principal without payments:refund", "42501", "", func(a *slrApproval) { a.Principal = noRefund })
	noManage := s.member(t, "payments:refund")
	reject("principal without integration:manage", "42501", "", func(a *slrApproval) { a.Principal = noManage })
	stranger := randomUUID()
	mustExec(t, s.f.owner, `INSERT INTO identity.principals(id) VALUES($1)`, stranger)
	reject("principal that is no member", "42501", "", func(a *slrApproval) { a.Principal = stranger })
	// §9: every checklist code is required, including canary_private.
	for _, code := range slrChecklist {
		reject("checklist missing "+code, "22023", "", func(a *slrApproval) {
			var kept []string
			for _, c := range a.Checklist {
				if c != code {
					kept = append(kept, c)
				}
			}
			a.Checklist = kept
		})
	}
	reject("checklist with an unknown extra code", "22023", "", func(a *slrApproval) { a.Checklist = append(a.Checklist, "extra_code") })
	// LD8: readiness must hold; false/out-of-range values are refused.
	for _, k := range []string{"ChargesEnabled", "PayoutsEnabled", "DetailsSubmitted"} {
		reject("readiness "+k+"=false", "22023", "", func(a *slrApproval) { a.Readiness[k] = false })
	}
	reject("readiness CurrentlyDueCount=1", "22023", "", func(a *slrApproval) { a.Readiness["CurrentlyDueCount"] = 1 })
	for _, n := range []int{0, 4, 23} {
		reject(fmt.Sprintf("readiness DescriptorLength=%d", n), "22023", "", func(a *slrApproval) { a.Readiness["DescriptorLength"] = n })
	}
	for _, n := range []int{1, 11} {
		reject(fmt.Sprintf("readiness PrefixLength=%d", n), "22023", "", func(a *slrApproval) { a.Readiness["PrefixLength"] = n })
	}
	// LD8: a missing or JSON-null field fails closed with stripe_live_readiness_unknown, never read as 0/false.
	for k := range slrReadiness() {
		reject("readiness key missing: "+k, "22023", "stripe_live_readiness_unknown", func(a *slrApproval) { delete(a.Readiness, k) })
		reject("readiness key null: "+k, "22023", "stripe_live_readiness_unknown", func(a *slrApproval) { a.Readiness[k] = nil })
	}
	// approved_at: not in the future, not older than 30 days.
	reject("approved_at in the future", "22023", "", func(a *slrApproval) { a.ApprovedAt = time.Now().Add(time.Hour).UTC() })
	reject("approved_at 31 days old", "22023", "", func(a *slrApproval) { a.ApprovedAt = time.Now().Add(-31 * 24 * time.Hour).UTC() })
	// Caps: canary <= 2x stripe_min_minor(TWD)=5000 and stripe_amount_ok; max stripe_amount_ok and (LQ3) <= 2,000,000.
	reject("canary cap above 2x the currency minimum", "22023", "", func(a *slrApproval) { a.Canary = 5100 })
	reject("canary cap below the currency minimum", "22023", "", func(a *slrApproval) { a.Canary = 2400 })
	reject("canary cap not a whole TWD dollar", "22023", "", func(a *slrApproval) { a.Canary = 4999 })
	reject("max below the canary cap", "22023", "", func(a *slrApproval) { a.Max = 4900 })
	reject("TWD max 2,000,001 (LQ3)", "22023", "", func(a *slrApproval) { a.Max = 2000001 })
	reject("TWD max 2,000,100: amount_ok but above LQ3", "22023", "", func(a *slrApproval) { a.Max = 2000100 })
	reject("max not stripe_amount_ok", "22023", "", func(a *slrApproval) { a.Max = 2000050 })
	// Other currencies: HKD is a known Stripe currency whose per-order max the owner has not ruled; JPY is unknown.
	reject("non-TWD currency (HKD)", "22023", "stripe_live_max_unruled", func(a *slrApproval) { a.Currency, a.Canary, a.Max = "HKD", 800, 1600 })
	reject("unknown currency (JPY)", "22023", "", func(a *slrApproval) { a.Currency, a.Canary, a.Max = "JPY", 500, 1000 })
	reject("malformed currency", "22023", "", func(a *slrApproval) { a.Currency = "twd" })
	reject("malformed approval_ref", "22023", "", func(a *slrApproval) { a.Ref = "short" })
	reject("approval on another store's connection", "", "", func(a *slrApproval) { a.Connection = randomUUID() })

	// The boundaries are accepted (LQ3 max exactly 2,000,000, canary exactly 2x minimum).
	a := s.approval()
	if got, err := s.approve(a); err != nil || got != a.ID {
		t.Fatalf("valid approval: %q %v", got, err)
	}
	var by, ref, cur, env string
	var canary, max int64
	var checklist []string
	if err := s.f.owner.QueryRow(context.Background(), `SELECT approved_by::text,approval_ref,currency,environment,canary_max_minor,max_minor,checklist FROM payments.stripe_live_approvals WHERE id=$1`, a.ID).
		Scan(&by, &ref, &cur, &env, &canary, &max, &checklist); err != nil {
		t.Fatal(err)
	}
	if by != s.scope.PrincipalID || ref != a.Ref || cur != "TWD" || env != "LIVE" || canary != 5000 || max != 2000000 || strings.Join(checklist, ",") != strings.Join(sortedCopy(slrChecklist), ",") {
		t.Fatalf("stored approval by=%s ref=%s cur=%s env=%s canary=%d max=%d checklist=%v", by, ref, cur, env, canary, max, checklist)
	}
	if s.audit(t, "stripe.live.approve") != before+1 {
		t.Fatal("approve wrote no audit row")
	}
	// Replay: identical call -> same id, no second row; any other parameter under the same id -> PT409.
	if got, err := s.approve(a); err != nil || got != a.ID {
		t.Fatalf("identical replay: %q %v", got, err)
	}
	other := a
	other.Max = 1000000
	_, err := s.approve(other)
	slrWant(t, "same id, other max", err, "PT409", "")
	other = a
	other.Ref = slrRef("owner-chat:2026-09-30:other")
	_, err = s.approve(other)
	slrWant(t, "same id, other approval_ref", err, "PT409", "")
	// One active approval per connection.
	second := s.approval()
	_, err = s.approve(second)
	slrWant(t, "second active approval", err, "PT409", "")
	if s.approvals(t) != 1 {
		t.Fatalf("approvals=%d after replays", s.approvals(t))
	}
}

func sortedCopy(in []string) []string {
	out := append([]string(nil), in...)
	sort.Strings(out)
	return out
}

func slrQualifyMethodGate(t *testing.T, base *testFixture) {
	s := slrNew(t, base)
	s.registerLive(t)
	now := time.Now()
	// LD5: a LIVE qualification needs an active approval.
	_, err := s.qualify(t, "LIVE", slrProbe(), now.Add(-time.Second), now.Add(time.Hour), 1)
	slrWant(t, "LIVE qualify without approval", err, "PT409", "")
	a := s.mustApprove(t)
	// §3.4: PROVIDER_MOCK and SANDBOX profiles only on SANDBOX accounts, even with an approval present.
	for _, profile := range []string{"PROVIDER_MOCK", "SANDBOX"} {
		_, err = s.qualify(t, profile, "stripe-probe:cs_test_"+t04Tag(), now.Add(-time.Second), now.Add(time.Hour), 1)
		slrWant(t, profile+" qualify on a LIVE account", err, "PT409", "")
	}
	// Probe evidence must be a LIVE checkout session; expiry <= 30 days.
	for _, ref := range []string{"stripe-probe:cs_test_" + t04Tag(), "stripe-probe:" + t04Tag(), "cs_live_" + t04Tag(), ""} {
		_, err = s.qualify(t, "LIVE", ref, now.Add(-time.Second), now.Add(time.Hour), 1)
		slrRejected(t, "LIVE qualify with evidence "+ref, err)
	}
	_, err = s.qualify(t, "LIVE", slrProbe(), now.Add(-time.Second), now.Add(31*24*time.Hour), 1)
	slrWant(t, "LIVE qualify expiring after 30 days", err, "22023", "")
	if n := countRows(t, s.f.owner, `SELECT count(*) FROM payments.account_qualifications WHERE connection_id=$1`, s.conn); n != 0 {
		t.Fatalf("rejected qualifications left %d rows", n)
	}
	q := s.mustQualify(t)
	var proof, env string
	var approval *string
	if err := s.f.owner.QueryRow(context.Background(), `SELECT proof_class,environment,live_approval_id::text FROM payments.account_qualifications WHERE id=$1`, q).Scan(&proof, &env, &approval); err != nil {
		t.Fatal(err)
	}
	if proof != "REAL_LIVE" || env != "LIVE" || approval == nil || *approval != a.ID {
		t.Fatalf("qualification proof=%s env=%s approval=%v want REAL_LIVE/LIVE/%s", proof, env, approval, a.ID)
	}

	// LIVE method with a non-REAL_LIVE qualification is refused (owner-pool PROVIDER_MOCK qualification on the LIVE account).
	slrDisclose(t, "payments.account_qualifications PROVIDER_MOCK row on a LIVE connection")
	mock := randomUUID()
	mustExec(t, s.f.owner, `INSERT INTO payments.account_qualifications(id,tenant_id,store_id,connection_id,credential_version,environment,code,proof_class,evidence_ref,observed_at,expires_at)
	 VALUES($1,$2,$3,$4,1,'LIVE','stripe_checkout','PROVIDER_MOCK','slr fixture',clock_timestamp()-interval '1 second',clock_timestamp()+interval '1 hour')`, mock, s.scope.TenantID, s.scope.StoreID, s.conn)
	_, err = s.twMethod(t, mock, 0, true, 5000)
	slrWant(t, "LIVE method with a PROVIDER_MOCK qualification", err, "PT409", "")
	// Method in another currency than the approval. Every market's currency equals its store's (FK), and only TWD is
	// approvable, so the mismatch is reached by tampering the approval's currency (replica-mode UPDATE, restored).
	slrReplica(t, s.f, `UPDATE payments.stripe_live_approvals SET currency='HKD' WHERE id=$1`, a.ID)
	_, err = s.twMethod(t, q, 0, true, 5000)
	slrReplica(t, s.f, `UPDATE payments.stripe_live_approvals SET currency='TWD' WHERE id=$1`, a.ID)
	slrWant(t, "method in another currency than the approval", err, "PT409", "")
	// Caps before the canary: max <= canary_max_minor.
	_, err = s.twMethod(t, q, 0, true, 5100)
	slrWant(t, "method max above the canary cap", err, "PT409", "")
	v, err := s.twMethod(t, q, 0, true, 5000)
	if err != nil || v != 1 {
		t.Fatalf("method at the canary cap: %d %v", v, err)
	}
	// Registrar scope: a principal without integration:manage cannot set a method.
	_, err = s.pool.Exec(context.Background(), `SELECT payments.set_stripe_method($1::uuid,$2::uuid,$3::uuid,$4::uuid,'TW',$5::uuid,$6::uuid,1,true,true,1,2500,5000,'S','S','S')`,
		s.scope.TenantID, s.scope.StoreID, s.member(t, "payments:refund"), s.p.market.ID, s.conn, q)
	slrWant(t, "method by a principal without integration:manage", err, "42501", "")
}

func slrCanaryGate(t *testing.T, base *testFixture) {
	l := slrNew(t, base).live(t)
	c := l.canaryReady(t)
	f := l.f
	// Every negative tampers exactly one canary predicate (replica-mode UPDATE, restored right after), so each
	// rejection can only come from that predicate; the untampered fixture is accepted at the end.
	type tamper struct {
		name, apply, restore   string
		applyArgs, restoreArgs []any
	}
	// A second, already revoked approval of the same connection: the "other approval" of one negative.
	slrDisclose(t, "payments.stripe_live_approvals extra revoked approval row of the same connection")
	other := randomUUID()
	mustExec(t, f.owner, `INSERT INTO payments.stripe_live_approvals(id,tenant_id,store_id,connection_id,account_id,currency,approved_by,approval_ref,approved_at,canary_max_minor,max_minor,checklist,account_readiness,revoked_at,revoked_by,revoke_ref,recorded_at)
	 SELECT $1,tenant_id,store_id,connection_id,account_id,currency,approved_by,'owner-chat:other-approval',approved_at-interval '1 day',canary_max_minor,max_minor,checklist,account_readiness,now(),approved_by,'owner-chat:other-revoke',recorded_at-interval '1 day'
	 FROM payments.stripe_live_approvals WHERE id=$2`, other, l.approval.ID)
	xa, xr, xe := randomUUID(), randomUUID(), randomUUID()
	neg := []tamper{
		{"SANDBOX attempt", `UPDATE checkout.payment_attempts SET environment='SANDBOX',execution_profile='SANDBOX' WHERE id=$1`,
			`UPDATE checkout.payment_attempts SET environment='LIVE',execution_profile='LIVE' WHERE id=$1`, []any{c.attempt}, []any{c.attempt}},
		{"attempt of another connection", `UPDATE checkout.payment_attempts SET connection_id=gen_random_uuid() WHERE id=$1`,
			`UPDATE checkout.payment_attempts SET connection_id=$2 WHERE id=$1`, []any{c.attempt}, []any{c.attempt, l.conn}},
		{"attempt in another currency", `UPDATE checkout.payment_attempts SET currency='HKD' WHERE id=$1`,
			`UPDATE checkout.payment_attempts SET currency='TWD' WHERE id=$1`, []any{c.attempt}, []any{c.attempt}},
		{"attempt created before the approval", `UPDATE checkout.payment_attempts SET created_at=created_at-interval '1 day' WHERE id=$1`,
			`UPDATE checkout.payment_attempts SET created_at=created_at+interval '1 day' WHERE id=$1`, []any{c.attempt}, []any{c.attempt}},
		{"attempt under another approval's qualification", `UPDATE payments.account_qualifications SET live_approval_id=$2 WHERE id=$1`,
			`UPDATE payments.account_qualifications SET live_approval_id=$2 WHERE id=$1`, []any{l.qual, other}, []any{l.qual, l.approval.ID}},
		{"no CAPTURED fact", `UPDATE payments.facts SET kind='AUTHORIZED' WHERE attempt_id=$1`,
			`UPDATE payments.facts SET kind='CAPTURED' WHERE attempt_id=$1`, []any{c.attempt}, []any{c.attempt}},
		{"captured amount above the canary cap", `UPDATE payments.facts SET amount_minor=5100 WHERE attempt_id=$1`,
			`UPDATE payments.facts SET amount_minor=2500 WHERE attempt_id=$1`, []any{c.attempt}, []any{c.attempt}},
		{"partial refund", `UPDATE payments.refund_facts SET amount_minor=1000 WHERE refund_id=$1`,
			`UPDATE payments.refund_facts SET amount_minor=2500 WHERE refund_id=$1`, []any{c.refund}, []any{c.refund}},
		{"refund without a SUCCEEDED fact", `UPDATE payments.refund_facts SET kind='FAILED',failure_reason='declined' WHERE refund_id=$1`,
			`UPDATE payments.refund_facts SET kind='SUCCEEDED',failure_reason=NULL WHERE refund_id=$1`, []any{c.refund}, []any{c.refund}},
		{"SANDBOX refund", `UPDATE payments.stripe_refunds SET environment='SANDBOX' WHERE id=$1`,
			`UPDATE payments.stripe_refunds SET environment='LIVE' WHERE id=$1`, []any{c.refund}, []any{c.refund}},
		{"refund of another attempt", `UPDATE payments.stripe_refunds SET attempt_id=$2 WHERE id=$1`,
			`UPDATE payments.stripe_refunds SET attempt_id=$2 WHERE id=$1`, []any{c.refund, randomUUID()}, []any{c.refund, c.attempt}},
		{"receipt endpoint is not a LIVE endpoint", `UPDATE payments.stripe_webhook_endpoints SET environment='SANDBOX',execution_profile='SANDBOX' WHERE endpoint_id=$1`,
			`UPDATE payments.stripe_webhook_endpoints SET environment='LIVE',execution_profile='LIVE' WHERE endpoint_id=$1`, []any{l.endp}, []any{l.endp}},
		{"no LIVE attempt receipt", `UPDATE payments.stripe_webhook_receipts SET attempt_id=$2 WHERE attempt_id=$1 AND refund_id IS NULL`,
			`UPDATE payments.stripe_webhook_receipts SET attempt_id=$1 WHERE attempt_id=$2 AND refund_id IS NULL`, []any{c.attempt, xa}, []any{c.attempt, xa}},
		{"no LIVE refund receipt", `UPDATE payments.stripe_webhook_receipts SET refund_id=$2 WHERE refund_id=$1`,
			`UPDATE payments.stripe_webhook_receipts SET refund_id=$1 WHERE refund_id=$2`, []any{c.refund, xr}, []any{c.refund, xr}},
		{"receipts not on a LIVE endpoint of this connection", `UPDATE payments.stripe_webhook_receipts SET endpoint_id=$2 WHERE endpoint_id=$1`,
			`UPDATE payments.stripe_webhook_receipts SET endpoint_id=$1 WHERE endpoint_id=$2`, []any{l.endp, xe}, []any{l.endp, xe}},
	}
	for _, n := range neg {
		n := n
		t.Run("negative/"+n.name, func(t *testing.T) {
			slrReplica(t, f, n.apply, n.applyArgs...)
			_, err := l.canary(l.approval, c.attempt, c.refund)
			slrReplica(t, f, n.restore, n.restoreArgs...)
			slrWant(t, n.name, err, "PT409", "")
			if got := countRows(t, f.owner, `SELECT count(*) FROM payments.stripe_live_approvals WHERE id=$1 AND canary_verified_at IS NOT NULL`, l.approval.ID); got != 0 {
				t.Fatalf("%s: rejected canary was recorded", n.name)
			}
		})
	}
	// Unknown attempt/refund ids and a review case on the attempt (a real review reason) are refused too.
	_, err := l.canary(l.approval, randomUUID(), c.refund)
	slrWant(t, "unknown attempt", err, "PT409", "")
	_, err = l.canary(l.approval, c.attempt, randomUUID())
	slrWant(t, "unknown refund", err, "PT409", "")
	t.Run("negative/any review case on the attempt", func(t *testing.T) {
		slrDisclose(t, "payments.review_cases row (REFUND_HISTORY) and its removal")
		var hash []byte
		if err := f.owner.QueryRow(context.Background(), `SELECT report_hash FROM payments.provider_observations WHERE attempt_id=$1 ORDER BY received_at LIMIT 1`, c.attempt).Scan(&hash); err != nil {
			t.Fatal(err)
		}
		mustExec(t, f.owner, `INSERT INTO payments.review_cases(tenant_id,store_id,attempt_id,reason,source_report_hash) VALUES($1,$2,$3,'REFUND_HISTORY',$4)`, l.scope.TenantID, l.scope.StoreID, c.attempt, hash)
		_, err := l.canary(l.approval, c.attempt, c.refund)
		slrReplica(t, f, `DELETE FROM payments.review_cases WHERE attempt_id=$1`, c.attempt)
		slrWant(t, "review case on the attempt", err, "PT409", "")
	})
	// An approval that is revoked cannot record a canary (checked on a second approval below via revoke gate).

	// The untampered fixture is accepted: canary_verified_at set once, audit row written.
	before := l.audit(t, "stripe.live.canary")
	at, err := l.canary(l.approval, c.attempt, c.refund)
	if err != nil || at.IsZero() {
		t.Fatalf("valid canary: %v %v", at, err)
	}
	if l.audit(t, "stripe.live.canary") != before+1 {
		t.Fatal("canary wrote no audit row")
	}
	again, err := l.canary(l.approval, c.attempt, c.refund)
	if err != nil || !again.Equal(at) {
		t.Fatalf("identical replay: %v %v want %v", again, err, at)
	}
	_, err = l.canary(l.approval, randomUUID(), c.refund)
	slrWant(t, "other attempt after the canary", err, "PT409", "")
	_, err = l.canary(l.approval, c.attempt, randomUUID())
	slrWant(t, "other refund after the canary", err, "PT409", "")
	// Method caps after the canary: max <= max_minor (2,000,000), the canary cap no longer applies.
	v, err := l.twMethod(t, l.qual, l.version, true, 2000000)
	if err != nil {
		t.Fatalf("method at max_minor after the canary: %v", err)
	}
	_, err = l.twMethod(t, l.qual, v, true, 2000100)
	slrWant(t, "method above max_minor", err, "PT409", "")
}

func slrRevokeGate(t *testing.T, base *testFixture) {
	l := slrNew(t, base).live(t)
	ctx := context.Background()
	// Three attempts exist before the kill switch: one paid later, one closed unpaid later, one to refund later.
	holdPaid, holdClosed := l.p, l.newHold(t)
	paid := l.mustBegin(t, holdPaid)
	closed := l.mustBegin(t, holdClosed)
	// A captured order whose refund is requested after the revoke.
	holdRefund := l.newHold(t)
	refundAttempt := l.mustBegin(t, holdRefund)
	_, refundPI := l.capture(t, refundAttempt)

	// The buyer-facing view (hosted_payment_view_v2, LIVE profile): the LIVE method is offered while the store is in CANARY,
	// never by a SANDBOX-profile service, and disappears with the revoke.
	viewHold := l.newHold(t)
	offers := func(svc *checkout.HostedPaymentStarter) bool {
		t.Helper()
		v, err := svc.PaymentView(ctx, viewHold.cap.Token, viewHold.f.storeA1, viewHold.hold.OrderID)
		if err != nil {
			t.Fatalf("payment view: %v", err)
		}
		for _, m := range v.Methods {
			if m.Code == "stripe_checkout" {
				return true
			}
		}
		return false
	}
	sandboxService := l.service(t, "SANDBOX", l.scfg)
	if !offers(l.starter) {
		t.Fatal("the LIVE view does not offer the LIVE method of a store in CANARY")
	}
	if offers(sandboxService) {
		t.Fatal("a SANDBOX-profile service offers a LIVE-account method")
	}

	// S8: the Open registrar (no pair) revokes through Go too; the raw definer is used for replay checks.
	ref := slrRef("owner-chat:revoke")
	at, err := l.reg.LiveRevoke(ctx, l.scope, l.approval.ID, ref)
	if err != nil || at.IsZero() {
		t.Fatalf("LiveRevoke through the no-pair registrar: %v %v", at, err)
	}
	if got := countRows(t, l.f.owner, `SELECT count(*) FROM payments.account_qualifications WHERE live_approval_id=$1 AND revoked_at IS NULL`, l.approval.ID); got != 0 {
		t.Fatalf("%d LIVE qualifications survive the revoke", got)
	}
	if l.audit(t, "stripe.live.revoke") != 1 {
		t.Fatal("revoke wrote no audit row")
	}
	// Replay: same ref + principal -> stored time; other ref or other principal on a revoked approval -> PT409.
	again, err := l.revoke(l.approval, l.scope.PrincipalID, ref)
	if err != nil || !again.Equal(at) {
		t.Fatalf("revoke replay: %v %v want %v", again, err, at)
	}
	_, err = l.revoke(l.approval, l.scope.PrincipalID, slrRef("owner-chat:other"))
	slrWant(t, "revoke replay with another ref", err, "PT409", "")
	other := l.member(t, "integration:manage")
	_, err = l.revoke(l.approval, other, ref)
	slrWant(t, "revoke replay by another principal", err, "PT409", "")
	// Any principal passing the registrar scope may revoke (the kill switch never needs the owner): a fresh approval
	// revoked by a principal without payments:refund.
	// The next start is refused (PT409 -> command.ErrConflict) although the method row is still enabled.
	if offers(l.starter) {
		t.Fatal("the LIVE view still offers the method after the revoke")
	}
	_, err = l.begin(viewHold)
	if !errors.Is(err, command.ErrConflict) {
		t.Fatalf("start after revoke: %v, want command.ErrConflict (PT409)", err)
	}
	// In-flight attempts still reconcile: a paid observation captures, an expired/unpaid one closes.
	sess, pi := l.pin(t, paid)
	_ = sess
	_ = pi
	hash := l.observe(t, paid, l.sessionReport(t, paid, "complete", "paid", "succeeded", true))
	if err := l.apply(paid, hash); err != nil {
		t.Fatalf("reconcile after revoke (capture): %v", err)
	}
	if countRows(t, l.f.owner, `SELECT count(*) FROM payments.facts WHERE attempt_id=$1 AND kind='CAPTURED'`, paid) != 1 {
		t.Fatal("in-flight attempt did not reach CAPTURED after the revoke")
	}
	l.pin(t, closed)
	hash = l.observe(t, closed, l.sessionReport(t, closed, "expired", "unpaid", "canceled", true))
	if err := l.apply(closed, hash); err != nil {
		t.Fatalf("reconcile after revoke (close): %v", err)
	}
	if countRows(t, l.f.owner, `SELECT count(*) FROM payments.facts WHERE attempt_id=$1 AND kind='CLOSED_UNPAID'`, closed) != 1 {
		t.Fatal("in-flight attempt did not reach CLOSED_UNPAID after the revoke")
	}
	// LD7: a captured LIVE payment of a revoked store is still refundable.
	status, out := l.requestRefund(holdRefund, 2500, 2500)
	if status != 201 {
		t.Fatalf("refund on a revoked store answered %d: %v", status, out)
	}
	_ = refundPI
	// The kill switch also works through disable; re-enable after revoke is refused (needs a new approval).
	if _, err := l.reg.SetMethod(ctx, l.scope, stripeadmin.MethodInput{MarketID: l.p.market.ID, Country: "TW", ConnectionID: l.conn, QualificationID: l.qual,
		ExpectedVersion: l.version, Enabled: false, Visible: true, Sort: 1, MinMinor: 2500, MaxMinor: 5000, NameHans: "Stripe", NameHant: "Stripe", NameEN: "Stripe"}); err != nil {
		t.Fatalf("SetMethod(Enabled=false) after revoke through the no-pair registrar: %v", err)
	}
	_, err = l.twMethod(t, l.qual, l.version+1, true, 5000)
	slrWant(t, "re-enable after revoke", err, "PT409", "")
	// A revoked approval cannot record a canary.
	_, err = l.canary(l.approval, refundAttempt, randomUUID())
	slrWant(t, "canary on a revoked approval", err, "PT409", "")
}

func slrConcurrentGate(t *testing.T, base *testFixture) {
	ctx := context.Background()
	t.Run("qualify_holds_the_approval_share_lock_revoke_waits_then_revokes_it", func(t *testing.T) {
		s := slrNew(t, base)
		s.registerLive(t)
		a := s.mustApprove(t)
		tx, err := s.pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback(ctx)
		var holderPID int
		if err := tx.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&holderPID); err != nil {
			t.Fatal(err)
		}
		qual := randomUUID()
		if _, err := tx.Exec(ctx, `SELECT payments.qualify_stripe_method($1::uuid,$2::uuid,$3::uuid,$4::uuid,$5::uuid,1,'LIVE',$6,clock_timestamp()-interval '1 second',clock_timestamp()+interval '1 hour')`,
			s.scope.TenantID, s.scope.StoreID, s.scope.PrincipalID, qual, s.conn, slrProbe()); err != nil {
			t.Fatalf("session 1 qualify: %v", err)
		}
		done := make(chan error, 1)
		go func() {
			_, err := (&slrLive{slrEnv: s}).revoke(a, s.scope.PrincipalID, slrRef("owner-chat:revoke"))
			done <- err
		}()
		// pg_blocking_pids witness: the revoke backend waits on session 1 (holder of the FOR SHARE approval lock).
		deadline := time.Now().Add(15 * time.Second)
		witnessed := false
		for time.Now().Before(deadline) && !witnessed {
			var n int
			if err := s.f.owner.QueryRow(ctx, `SELECT count(*) FROM pg_stat_activity w WHERE w.query LIKE '%payments.revoke_stripe_live%' AND w.pid<>pg_backend_pid() AND $1=ANY(pg_blocking_pids(w.pid))`, holderPID).Scan(&n); err != nil {
				t.Fatal(err)
			}
			witnessed = n > 0
			if !witnessed {
				time.Sleep(100 * time.Millisecond)
			}
		}
		if !witnessed {
			t.Fatal("revoke was not blocked by the in-flight qualify (no pg_blocking_pids witness)")
		}
		select {
		case err := <-done:
			t.Fatalf("revoke finished while the qualify was uncommitted: %v", err)
		default:
		}
		if err := tx.Commit(ctx); err != nil {
			t.Fatal(err)
		}
		select {
		case err := <-done:
			if err != nil {
				t.Fatalf("revoke after the qualify committed: %v", err)
			}
		case <-time.After(15 * time.Second):
			t.Fatal("revoke did not finish after session 1 committed")
		}
		if n := countRows(t, s.f.owner, `SELECT count(*) FROM payments.account_qualifications WHERE connection_id=$1 AND proof_class='REAL_LIVE' AND revoked_at IS NULL`, s.conn); n != 0 {
			t.Fatalf("%d active REAL_LIVE qualifications after qualify+revoke", n)
		}
		if n := countRows(t, s.f.owner, `SELECT count(*) FROM payments.account_qualifications WHERE id=$1 AND revoked_at IS NOT NULL`, qual); n != 1 {
			t.Fatal("the concurrently inserted qualification was not revoked")
		}
	})
	t.Run("revoke_holds_the_lock_qualify_waits_then_is_refused", func(t *testing.T) {
		s := slrNew(t, base)
		s.registerLive(t)
		a := s.mustApprove(t)
		tx, err := s.pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback(ctx)
		var holderPID int
		if err := tx.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&holderPID); err != nil {
			t.Fatal(err)
		}
		if _, err := tx.Exec(ctx, `SELECT payments.revoke_stripe_live($1::uuid,$2::uuid,$3::uuid,$4::uuid,$5)`, s.scope.TenantID, s.scope.StoreID, s.scope.PrincipalID, a.ID, slrRef("owner-chat:revoke")); err != nil {
			t.Fatalf("session 1 revoke: %v", err)
		}
		done := make(chan error, 1)
		go func() {
			_, err := s.qualify(t, "LIVE", slrProbe(), time.Now().Add(-time.Second), time.Now().Add(time.Hour), 1)
			done <- err
		}()
		deadline := time.Now().Add(15 * time.Second)
		witnessed := false
		for time.Now().Before(deadline) && !witnessed {
			var n int
			if err := s.f.owner.QueryRow(ctx, `SELECT count(*) FROM pg_stat_activity w WHERE w.query LIKE '%payments.qualify_stripe_method%' AND w.pid<>pg_backend_pid() AND $1=ANY(pg_blocking_pids(w.pid))`, holderPID).Scan(&n); err != nil {
				t.Fatal(err)
			}
			witnessed = n > 0
			if !witnessed {
				time.Sleep(100 * time.Millisecond)
			}
		}
		if !witnessed {
			t.Fatal("qualify was not blocked by the in-flight revoke (no pg_blocking_pids witness)")
		}
		if err := tx.Commit(ctx); err != nil {
			t.Fatal(err)
		}
		select {
		case err := <-done:
			slrWant(t, "qualify after the revoke committed", err, "PT409", "")
		case <-time.After(15 * time.Second):
			t.Fatal("qualify did not finish after the revoke committed")
		}
		if n := countRows(t, s.f.owner, `SELECT count(*) FROM payments.account_qualifications WHERE connection_id=$1 AND revoked_at IS NULL`, s.conn); n != 0 {
			t.Fatalf("%d active qualifications after revoke+qualify", n)
		}
	})
}

// slrDisableGate: `enabled=false` succeeds in every state (contract §3.4/§7): after revoke, after qualification expiry
// and after a key rotation, from a SQL definer call.
func slrDisableGate(t *testing.T, base *testFixture) {
	ctx := context.Background()
	t.Run("after_revoke", func(t *testing.T) {
		l := slrNew(t, base).live(t)
		if _, err := l.revoke(l.approval, l.scope.PrincipalID, slrRef("owner-chat:revoke")); err != nil {
			t.Fatal(err)
		}
		v, err := l.twMethod(t, l.qual, l.version, false, 5000)
		if err != nil || v != l.version+1 {
			t.Fatalf("disable after revoke: %d %v", v, err)
		}
	})
	t.Run("after_qualification_expiry", func(t *testing.T) {
		l := slrNew(t, base).live(t)
		slrReplica(t, l.f, `UPDATE payments.account_qualifications SET observed_at=observed_at-interval '2 hours',expires_at=clock_timestamp()-interval '1 hour' WHERE id=$1`, l.qual)
		_, err := l.twMethod(t, l.qual, l.version, true, 5000)
		slrWant(t, "enable with an expired qualification", err, "PT409", "")
		v, err := l.twMethod(t, l.qual, l.version, false, 5000)
		if err != nil || v != l.version+1 {
			t.Fatalf("disable after expiry: %d %v", v, err)
		}
	})
	t.Run("after_key_rotation", func(t *testing.T) {
		l := slrNew(t, base).live(t)
		rotate := func(expected int64) int64 {
			var head int64
			if err := l.pool.QueryRow(ctx, `SELECT integration.rotate_stripe_key($1::uuid,$2::uuid,$3::uuid,$4::uuid,$5,'fixture_key2',$6::bytea,$7::bytea)`,
				l.scope.TenantID, l.scope.StoreID, l.scope.PrincipalID, l.conn, expected, randomBytes(12), randomBytes(48)).Scan(&head); err != nil || head != expected+1 {
				t.Fatalf("rotate: %d %v", head, err)
			}
			return head
		}
		head := rotate(1)
		// §4: rotation keeps the approval; new starts are blocked until the new head is re-qualified.
		if n := countRows(t, l.f.owner, `SELECT count(*) FROM payments.stripe_live_approvals WHERE id=$1 AND revoked_at IS NULL`, l.approval.ID); n != 1 {
			t.Fatal("rotation revoked the approval")
		}
		if _, err := l.begin(l.newHold(t)); !errors.Is(err, command.ErrConflict) {
			t.Fatalf("start with a qualification of the rotated-out credential: %v, want command.ErrConflict", err)
		}
		_, err := l.twMethod(t, l.qual, l.version, true, 5000)
		slrWant(t, "enable with a qualification of the rotated-out credential", err, "PT409", "")
		// Re-qualify the new head (no new approval, no new canary) and the store starts again.
		q2, err := l.qualify(t, "LIVE", slrProbe(), time.Now().Add(-time.Second), time.Now().Add(time.Hour), head)
		if err != nil {
			t.Fatalf("re-qualify after rotation: %v", err)
		}
		v, err := l.twMethod(t, q2, l.version, true, 5000)
		if err != nil {
			t.Fatalf("enable with the re-qualified head: %v", err)
		}
		l.version, l.qual = v, q2
		if _, err := l.begin(l.newHold(t)); err != nil {
			t.Fatalf("start after re-qualification: %v", err)
		}
		// Disable still works after a further rotation (the kill switch never depends on the key).
		rotate(head)
		v, err = l.twMethod(t, l.qual, l.version, false, 5000)
		if err != nil || v != l.version+1 {
			t.Fatalf("disable after rotation: %d %v", v, err)
		}
	})
	t.Run("disable_needs_only_scope_and_the_head_cas", func(t *testing.T) {
		l := slrNew(t, base).live(t)
		_, err := l.twMethod(t, l.qual, l.version+5, false, 5000)
		slrWant(t, "disable with a stale head version", err, "PT409", "")
		if _, err := l.pool.Exec(ctx, `SELECT payments.set_stripe_method($1::uuid,$2::uuid,$3::uuid,$4::uuid,'TW',$5::uuid,$6::uuid,$7,false,true,1,2500,5000,'S','S','S')`,
			l.scope.TenantID, l.scope.StoreID, l.member(t, "payments:refund"), l.p.market.ID, l.conn, l.qual, l.version); err == nil {
			t.Fatal("disable by a principal outside the registrar scope was accepted")
		}
	})
}
