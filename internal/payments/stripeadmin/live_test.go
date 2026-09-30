// live_test.go: MOCK-tier tests of the LIVE registrar steps (contracts/stripe-live-enable-v1.md §3.4, §5.2, S4,
// S7, S8) against a recording DB fake and a fake provider behind the package's unexported seam. A LIVE
// client can never be built over a mock transport (admit refuses it), so the seam still runs stripe.New's admission
// (no I/O) and only replaces the network calls.
// Non-goals: the SQL definers (SL02/SL03, independent unit), any real Stripe call, any real key (all keys are
// split sentinels). Callers: go test ./internal/payments/stripeadmin (names avoid the TestStripeSL prefix).

package stripeadmin

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"livecommerce/internal/integrations/accounts"
	"livecommerce/internal/integrations/psp/stripe"
)

const (
	liveRAK      = "rk_" + "live_LIVEREGISTRARSENTINEL000"
	liveSK       = "sk_" + "live_LIVEREGISTRARSENTINEL000"
	liveApproval = "55555555-5555-4555-8555-555555555555"
	liveAttempt  = "66666666-6666-4666-8666-666666666666"
	liveRefund   = "77777777-7777-4777-8777-777777777777"
	liveRef      = "owner-chat:2026-09-30:stripe-live:shop"
)

var livePair = stripe.LiveApproval{Enabled: true, Reference: liveRef}

// liveDB replays scripted answers in call order and records every statement.
type liveDB struct {
	calls   []call
	replies []any // []any = multi-column row; string/time.Time/int64 = single value; error = failure
}

type liveRow struct {
	reply any
}

func (r liveRow) Scan(dest ...any) error {
	if err, ok := r.reply.(error); ok {
		return err
	}
	vals, ok := r.reply.([]any)
	if !ok {
		vals = []any{r.reply}
	}
	for i, d := range dest {
		switch d := d.(type) {
		case *string:
			*d = vals[i].(string)
		case *[]byte:
			*d = vals[i].([]byte)
		case *int64:
			*d = vals[i].(int64)
		case *time.Time:
			*d = vals[i].(time.Time)
		default:
			panic("unsupported scan target")
		}
	}
	return nil
}

func (f *liveDB) QueryRow(_ context.Context, sql string, args ...any) pgx.Row {
	f.calls = append(f.calls, call{sql, args})
	if len(f.replies) == 0 {
		panic("unscripted SQL call: " + sql)
	}
	reply := f.replies[0]
	f.replies = f.replies[1:]
	return liveRow{reply}
}

// fakeProvider replaces the three network calls.
type fakeProvider struct {
	verifyErr    error
	readiness    stripe.Readiness
	readinessErr error
	session      string
	probeErr     error
	verified     int
	read         int
	probed       int
	cfg          stripe.Config
}

func (p *fakeProvider) VerifyAccount(context.Context) (stripe.CallMeta, error) {
	p.verified++
	return stripe.CallMeta{}, p.verifyErr
}

func (p *fakeProvider) AccountReadiness(context.Context) (stripe.Readiness, stripe.CallMeta, error) {
	p.read++
	return p.readiness, stripe.CallMeta{}, p.readinessErr
}

func (p *fakeProvider) ProbeCheckout(context.Context, string, string, int64, string) (string, stripe.CallMeta, error) {
	p.probed++
	return p.session, stripe.CallMeta{}, p.probeErr
}

func readyReadiness() stripe.Readiness {
	t, f, n0, n11 := true, false, 0, 11
	return stripe.Readiness{ChargesEnabled: &t, PayoutsEnabled: &t, DetailsSubmitted: &t, CVCRule: &t, AVSRule: &f,
		CurrentlyDueCount: &n0, DescriptorLength: &n11, PrefixLength: &n0}
}

func liveRegistrar(t *testing.T, db *liveDB, p *fakeProvider) *Registrar {
	t.Helper()
	r := newRegistrar(db, ring(t, 1), ring(t, 2))
	r.live = livePair
	r.newProvider = func(cfg stripe.Config) (provider, error) {
		p.cfg = cfg
		return p, nil
	}
	return r
}

// storedReply seals a LIVE credential at version 2 exactly like Register/Rotate would and returns the four
// columns payments.stripe_registrar_credential answers.
func storedReply(t *testing.T, r *Registrar, key string) []any {
	t.Helper()
	keyID, nonce, ct, err := r.apiKeys.SealStripeAPI(accounts.StripeAPIScope{TenantID: tenant, StoreID: store,
		ConnectionID: conn, Environment: "LIVE", AccountID: acct, CredentialVersion: 2},
		accounts.StripeAPICredentials{SecretKey: key})
	if err != nil {
		t.Fatal(err)
	}
	return []any{acct, keyID, nonce, ct}
}

func liveInput() LiveApproveInput {
	return LiveApproveInput{ApprovalID: liveApproval, ConnectionID: conn, Currency: "TWD", ApprovalRef: liveRef,
		ApprovedAt: time.Date(2026, 9, 30, 8, 30, 0, 0, time.FixedZone("UTC+8", 8*3600)), ExpectedVersion: 2,
		CanaryMaxMinor: 5000, MaxMinor: 2000000, Checklist: append([]string{}, LiveChecklist...)}
}

func TestOpenLiveRefusesAnIncompletePairBeforeConnecting(t *testing.T) {
	for name, live := range map[string]stripe.LiveApproval{
		"zero": {}, "flag only": {Enabled: true}, "ref only": {Reference: liveRef},
		"short ref": {Enabled: true, Reference: "short"}, "spaces": {Enabled: true, Reference: "owner approval ok"},
	} {
		if _, err := OpenLive(context.Background(), "postgres://127.0.0.1:1/x", ring(t, 1), nil, live); !errors.Is(err, ErrConfig) {
			t.Fatalf("%s: %v", name, err)
		}
	}
	if _, err := OpenLive(nil, "x", nil, nil, livePair); !errors.Is(err, ErrConfig) { //nolint:staticcheck // nil ctx is the case under test
		t.Fatalf("nil ctx: %v", err)
	}
	// A valid pair reaches the pool opener, which fails masked on a closed port (S7: Open itself still refuses LIVE).
	if _, err := OpenLive(context.Background(), "postgres://127.0.0.1:1/x", ring(t, 1), nil, livePair); !errors.Is(err, ErrDatabase) {
		t.Fatalf("valid pair did not reach the database step: %v", err)
	}
}

func TestSandboxRegistrarStillRefusesLive(t *testing.T) {
	db := &liveDB{}
	r := newRegistrar(db, ring(t, 1), ring(t, 2))
	ctx := context.Background()
	if _, _, err := r.SetWebhookEndpoint(ctx, scope, EndpointInput{ConnectionID: conn, Profile: "LIVE", Enabled: true,
		Secrets: accounts.StripeWebhookSecrets{CurrentSecret: whsecA}}); !errors.Is(err, ErrConfig) {
		t.Fatalf("webhook LIVE on Open: %v", err)
	}
	if _, err := r.Qualify(ctx, scope, QualifyInput{ConnectionID: conn, Profile: "LIVE", ExpectedVersion: 1}); !errors.Is(err, ErrConfig) {
		t.Fatalf("qualify LIVE on Open: %v", err)
	}
	if _, err := r.LiveApprove(ctx, scope, liveInput()); !errors.Is(err, ErrConfig) {
		t.Fatalf("live-approve on Open: %v", err)
	}
	if _, err := r.LiveCanary(ctx, scope, liveApproval, liveAttempt, liveRefund); !errors.Is(err, ErrConfig) {
		t.Fatalf("live-canary on Open: %v", err)
	}
	if len(db.calls) != 0 {
		t.Fatalf("refusals reached SQL: %d", len(db.calls))
	}
	// A live key through the SANDBOX registrar is refused by the adapter (mode mismatch), before any SQL.
	srvless := newRegistrar(db, ring(t, 1), nil)
	if _, err := srvless.Register(ctx, scope, acct, liveRAK); !errors.Is(err, ErrRejected) {
		t.Fatalf("live key on Open: %v", err)
	}
	if len(db.calls) != 0 {
		t.Fatal("live key on Open reached SQL")
	}
}

func TestLiveRegisterSealsEnvironmentLiveAndRefusesUnrestrictedKeys(t *testing.T) {
	db := &liveDB{}
	p := &fakeProvider{}
	r := liveRegistrar(t, db, p)
	// the definer echoes the connection id
	var connection string
	r.db = queryRowerFunc(func(sql string, args []any) any {
		connection = args[3].(string)
		db.calls = append(db.calls, call{sql, args})
		return connection
	})
	id, err := r.Register(context.Background(), scope, acct, liveRAK)
	if err != nil || id != connection {
		t.Fatalf("register LIVE: %q %v", id, err)
	}
	a := db.calls[0].args
	if a[5] != "LIVE" || a[6] != acct {
		t.Fatalf("register args: %v", a[5:7])
	}
	if p.cfg.Environment != "LIVE" || p.cfg.Live != livePair || p.verified != 1 {
		t.Fatalf("provider config/verify: %+v verified=%d", p.cfg.Environment, p.verified)
	}
	got, err := r.apiKeys.OpenStripeAPI(accounts.StripeAPIScope{TenantID: tenant, StoreID: store, ConnectionID: id,
		Environment: "LIVE", AccountID: acct, CredentialVersion: 1}, a[7].(string), a[8].([]byte), a[9].([]byte))
	if err != nil || got.SecretKey != liveRAK {
		t.Fatal("LIVE ciphertext does not open under the LIVE version-1 scope")
	}
	if _, err := r.apiKeys.OpenStripeAPI(accounts.StripeAPIScope{TenantID: tenant, StoreID: store, ConnectionID: id,
		Environment: "SANDBOX", AccountID: acct, CredentialVersion: 1}, a[7].(string), a[8].([]byte), a[9].([]byte)); err == nil {
		t.Fatal("AAD does not bind the environment")
	}
	calls := len(db.calls)
	for name, key := range map[string]string{"sk_live (LD3)": liveSK, "test key on a LIVE registrar": testKey} {
		if _, err := r.Register(context.Background(), scope, acct, key); !errors.Is(err, ErrRejected) {
			t.Fatalf("%s: %v", name, err)
		}
	}
	if len(db.calls) != calls {
		t.Fatal("a refused key reached SQL")
	}
}

// queryRowerFunc adapts a function to the registrar's DB seam for tests that need to echo arguments.
type queryRowerFunc func(sql string, args []any) any

func (f queryRowerFunc) QueryRow(_ context.Context, sql string, args ...any) pgx.Row {
	return liveRow{f(sql, args)}
}

func TestLiveQualifyUsesTheStoredKeyAndStoresLiveProbeEvidence(t *testing.T) {
	db := &liveDB{}
	p := &fakeProvider{session: "cs_live_a1B2c3"}
	r := liveRegistrar(t, db, p)
	db.replies = []any{storedReply(t, r, liveRAK)}
	// call 0 is the credential read (scripted); call 1, the qualification definer, echoes the minted id
	r.db = &sequenceDB{inner: db, echoIndex: 1, echoArg: 3}
	id, err := r.Qualify(context.Background(), scope, QualifyInput{ConnectionID: conn, Profile: "LIVE", Currency: "TWD",
		AmountMinor: 2500, ReturnURL: returnURL, ExpectedVersion: 2})
	if err != nil {
		t.Fatalf("qualify LIVE: %v", err)
	}
	if p.verified != 1 || p.probed != 1 || p.cfg.Environment != "LIVE" || p.cfg.Live != livePair {
		t.Fatalf("provider use: verified=%d probed=%d cfg=%s", p.verified, p.probed, p.cfg.Environment)
	}
	q := db.calls[1]
	if !strings.Contains(q.sql, "payments.qualify_stripe_method") || q.args[3] != id || q.args[6] != "LIVE" ||
		q.args[7] != "stripe-probe:cs_live_a1B2c3" {
		t.Fatalf("qualify SQL args: %v", q.args)
	}
	// An operator assertion that differs from the stored key is refused before the network.
	db2 := &liveDB{}
	p2 := &fakeProvider{}
	r2 := liveRegistrar(t, db2, p2)
	db2.replies = []any{storedReply(t, r2, liveRAK)}
	if _, err := r2.Qualify(context.Background(), scope, QualifyInput{ConnectionID: conn, Profile: "LIVE", ExpectedVersion: 2,
		SecretKey: "rk_" + "live_SOMEOTHERKEYSENTINEL00"}); !errors.Is(err, ErrRejected) || p2.verified != 0 {
		t.Fatalf("key assertion mismatch: %v verified=%d", err, p2.verified)
	}
}

// sequenceDB scripts replies like liveDB and echoes one argument of the call at echoIndex as the reply.
type sequenceDB struct {
	inner     *liveDB
	echoIndex int
	echoArg   int
	n         int
}

func (s *sequenceDB) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	i := s.n
	s.n++
	if i == s.echoIndex {
		s.inner.calls = append(s.inner.calls, call{sql, args})
		return liveRow{args[s.echoArg]}
	}
	return s.inner.QueryRow(ctx, sql, args...)
}

func TestLiveWebhookSealsLiveEnvironmentAndProfile(t *testing.T) {
	db := &liveDB{}
	r := liveRegistrar(t, db, &fakeProvider{})
	var endpoint string
	step := 0
	r.db = queryRowerFunc(func(sql string, args []any) any {
		db.calls = append(db.calls, call{sql, args})
		step++
		if step == 1 {
			return acct // payments.stripe_endpoint_account
		}
		endpoint = args[4].(string)
		return int64(1)
	})
	id, v, err := r.SetWebhookEndpoint(context.Background(), scope, EndpointInput{ConnectionID: conn, Profile: "LIVE",
		Enabled: true, Secrets: accounts.StripeWebhookSecrets{CurrentSecret: whsecA}})
	if err != nil || id != endpoint || v != 1 {
		t.Fatalf("webhook LIVE: %q %d %v", id, v, err)
	}
	a := db.calls[1].args
	if a[5] != "LIVE" {
		t.Fatalf("profile arg %v", a[5])
	}
	if _, err := r.signingKeys.OpenStripeWebhook(accounts.StripeWebhookScope{TenantID: tenant, StoreID: store, ConnectionID: conn,
		EndpointID: id, Environment: "LIVE", AccountID: acct, Profile: "LIVE", KeyVersion: 1}, a[8].(string), a[9].([]byte), a[10].([]byte)); err != nil {
		t.Fatal("LIVE webhook envelope does not open under the LIVE scope")
	}
	// A LIVE registrar never takes a SANDBOX or mock profile.
	for _, profile := range []string{"SANDBOX", "PROVIDER_MOCK"} {
		if _, _, err := r.SetWebhookEndpoint(context.Background(), scope, EndpointInput{ConnectionID: conn, Profile: profile,
			Secrets: accounts.StripeWebhookSecrets{CurrentSecret: whsecA}}); !errors.Is(err, ErrConfig) {
			t.Fatalf("%s on a LIVE registrar: %v", profile, err)
		}
	}
}

func TestLiveApproveReadsReadinessWithTheStoredKeyAndPassesExactJSON(t *testing.T) {
	db := &liveDB{}
	p := &fakeProvider{readiness: readyReadiness()}
	r := liveRegistrar(t, db, p)
	db.replies = []any{storedReply(t, r, liveRAK), liveApproval}
	id, err := r.LiveApprove(context.Background(), scope, liveInput())
	if err != nil || id != liveApproval {
		t.Fatalf("live approve: %q %v", id, err)
	}
	if p.read != 1 || p.cfg.Environment != "LIVE" || p.cfg.SecretKey != liveRAK || p.cfg.Live != livePair {
		t.Fatalf("provider: read=%d env=%s", p.read, p.cfg.Environment)
	}
	if len(db.calls) != 2 || !strings.Contains(db.calls[0].sql, "stripe_registrar_credential") ||
		!strings.Contains(db.calls[1].sql, "payments.approve_stripe_live") {
		t.Fatalf("SQL sequence: %d calls", len(db.calls))
	}
	a := db.calls[1].args
	if a[0] != tenant || a[1] != store || a[2] != principal || a[3] != liveApproval || a[4] != conn || a[5] != "TWD" ||
		a[6] != liveRef || a[8] != int64(5000) || a[9] != int64(2000000) {
		t.Fatalf("scalar args: %v", a)
	}
	if at := a[7].(time.Time); !at.Equal(liveInput().ApprovedAt) || at.Location() != time.UTC {
		t.Fatalf("approved_at %v", at)
	}
	if codes := a[10].([]string); strings.Join(codes, ",") != strings.Join(LiveChecklist, ",") {
		t.Fatalf("checklist %v", codes)
	}
	var readiness map[string]any
	if err := json.Unmarshal([]byte(a[11].(string)), &readiness); err != nil || len(readiness) != 8 ||
		a[11].(string) != `{"AVSRule":false,"CVCRule":true,"ChargesEnabled":true,"CurrentlyDueCount":0,"DescriptorLength":11,`+
			`"DetailsSubmitted":true,"PayoutsEnabled":true,"PrefixLength":0}` {
		t.Fatalf("readiness json %v %v", a[11], err)
	}
	// No key material or key text reaches SQL args.
	for _, v := range a {
		if s, ok := v.(string); ok && strings.Contains(s, liveRAK) {
			t.Fatal("key in SQL args")
		}
	}
}

func TestLiveApproveRefusalsWriteNothing(t *testing.T) {
	run := func(mutate func(*LiveApproveInput), p *fakeProvider, wantSQL int, want error) {
		t.Helper()
		db := &liveDB{}
		r := liveRegistrar(t, db, p)
		db.replies = []any{storedReply(t, r, liveRAK), liveApproval}
		in := liveInput()
		if mutate != nil {
			mutate(&in)
		}
		if _, err := r.LiveApprove(context.Background(), scope, in); !errors.Is(err, want) {
			t.Fatalf("want %v, got %v", want, err)
		}
		if len(db.calls) != wantSQL {
			t.Fatalf("SQL calls = %d, want %d (%v)", len(db.calls), wantSQL, want)
		}
		for _, c := range db.calls {
			if strings.Contains(c.sql, "approve_stripe_live") && want != nil {
				t.Fatal("approval SQL reached on a refusal")
			}
		}
	}
	good := func() *fakeProvider { return &fakeProvider{readiness: readyReadiness()} }
	// input refused before any SQL or provider call
	run(func(in *LiveApproveInput) { in.Checklist = in.Checklist[1:] }, good(), 0, ErrRejected)
	run(func(in *LiveApproveInput) { in.Checklist = append(in.Checklist, "descriptor") }, good(), 0, ErrRejected)
	run(func(in *LiveApproveInput) { in.Checklist = append(in.Checklist[:10], "not_a_code") }, good(), 0, ErrRejected)
	run(func(in *LiveApproveInput) { in.Currency = "twd" }, good(), 0, ErrRejected)
	run(func(in *LiveApproveInput) { in.ApprovalRef = "short" }, good(), 0, ErrRejected)
	run(func(in *LiveApproveInput) { in.ApprovedAt = time.Time{} }, good(), 0, ErrRejected)
	run(func(in *LiveApproveInput) { in.CanaryMaxMinor = 0 }, good(), 0, ErrRejected)
	run(func(in *LiveApproveInput) { in.MaxMinor = 1000 }, good(), 0, ErrRejected)
	run(func(in *LiveApproveInput) { in.ApprovalID = "nope" }, good(), 0, ErrConfig)
	run(func(in *LiveApproveInput) { in.ConnectionID = "nope" }, good(), 0, ErrConfig)
	run(func(in *LiveApproveInput) { in.ExpectedVersion = 0 }, good(), 0, ErrConfig)
	// after the credential read (1 SQL call) but before the approval SQL
	absent := readyReadiness()
	absent.PrefixLength = nil // LD8: an absent field is never read as 0
	run(nil, &fakeProvider{readiness: absent}, 1, ErrRejected)
	run(nil, &fakeProvider{readinessErr: stripe.ErrAuthentication}, 1, ErrRejected)
	run(nil, &fakeProvider{readinessErr: stripe.ErrUncertain}, 1, ErrProvider)
	// a non-LIVE key can never be the stored key: the adapter refuses it (mode mismatch) before the provider call
	db := &liveDB{}
	p := &fakeProvider{readiness: readyReadiness()}
	r := liveRegistrar(t, db, p)
	keyID, nonce, ct, err := r.apiKeys.SealStripeAPI(accounts.StripeAPIScope{TenantID: tenant, StoreID: store, ConnectionID: conn,
		Environment: "SANDBOX", AccountID: acct, CredentialVersion: 2}, accounts.StripeAPICredentials{SecretKey: testKey})
	if err != nil {
		t.Fatal(err)
	}
	db.replies = []any{[]any{acct, keyID, nonce, ct}}
	if _, err := r.LiveApprove(context.Background(), scope, liveInput()); !errors.Is(err, ErrConfig) || p.read != 0 {
		t.Fatalf("SANDBOX-sealed key opened by the LIVE registrar: %v read=%d", err, p.read)
	}
	// the definer answering another id is a database inconsistency, not success
	db3 := &liveDB{}
	r3 := liveRegistrar(t, db3, &fakeProvider{readiness: readyReadiness()})
	db3.replies = []any{storedReply(t, r3, liveRAK), "99999999-9999-4999-8999-999999999999"}
	if _, err := r3.LiveApprove(context.Background(), scope, liveInput()); !errors.Is(err, ErrDatabase) {
		t.Fatalf("mismatched id: %v", err)
	}
}

func TestLiveCanaryAndRevokeCallTheirDefinersAndMapErrors(t *testing.T) {
	at := time.Date(2026, 9, 30, 1, 2, 3, 0, time.UTC)
	db := &liveDB{replies: []any{at}}
	r := liveRegistrar(t, db, &fakeProvider{})
	got, err := r.LiveCanary(context.Background(), scope, liveApproval, liveAttempt, liveRefund)
	if err != nil || !got.Equal(at) {
		t.Fatalf("canary: %v %v", got, err)
	}
	if c := db.calls[0]; !strings.Contains(c.sql, "payments.record_stripe_live_canary") || len(c.args) != 6 ||
		c.args[3] != liveApproval || c.args[4] != liveAttempt || c.args[5] != liveRefund {
		t.Fatalf("canary SQL: %v", c)
	}
	for _, bad := range [][3]string{{"x", liveAttempt, liveRefund}, {liveApproval, "x", liveRefund}, {liveApproval, liveAttempt, "x"}} {
		if _, err := r.LiveCanary(context.Background(), scope, bad[0], bad[1], bad[2]); !errors.Is(err, ErrConfig) {
			t.Fatalf("bad ids %v: %v", bad, err)
		}
	}

	// S8: revoke works on a registrar from Open (no pair) and needs no keyring.
	sandbox := newRegistrar(db, nil, nil)
	db.replies = []any{at}
	got, err = sandbox.LiveRevoke(context.Background(), scope, liveApproval, "owner-chat:2026-09-30:revoke")
	if err != nil || !got.Equal(at) {
		t.Fatalf("revoke on Open: %v %v", got, err)
	}
	if c := db.calls[len(db.calls)-1]; !strings.Contains(c.sql, "payments.revoke_stripe_live") || len(c.args) != 5 || c.args[4] != "owner-chat:2026-09-30:revoke" {
		t.Fatalf("revoke SQL: %v", c)
	}
	calls := len(db.calls)
	if _, err := sandbox.LiveRevoke(context.Background(), scope, liveApproval, "short"); !errors.Is(err, ErrRejected) {
		t.Fatalf("short revoke ref: %v", err)
	}
	if _, err := sandbox.LiveRevoke(context.Background(), scope, "x", "owner-chat:2026-09-30:revoke"); !errors.Is(err, ErrConfig) {
		t.Fatalf("bad approval id: %v", err)
	}
	if len(db.calls) != calls {
		t.Fatal("a refused revoke reached SQL")
	}
	// SQLSTATE mapping: PT409 / 22023 / 42501 are rejections; anything else is ErrDatabase.
	for _, tc := range []struct {
		err  error
		want error
	}{{pgErr("PT409"), ErrRejected}, {pgErr("22023"), ErrRejected}, {pgErr("42501"), ErrRejected}, {pgErr("XX000"), ErrDatabase}} {
		db.replies = []any{tc.err}
		if _, err := sandbox.LiveRevoke(context.Background(), scope, liveApproval, "owner-chat:2026-09-30:revoke"); !errors.Is(err, tc.want) {
			t.Fatalf("%v -> %v, want %v", tc.err, err, tc.want)
		}
	}
}

func pgErr(code string) error {
	return &pgconn.PgError{Code: code, Message: "secret detail " + testKey}
}
