package ads

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"

	"livecommerce/internal/integrations/core"
	"livecommerce/internal/platform"
	"livecommerce/migrations"
)

// pg_flow_test.go is the implementer's REAL_PG flow check of unit ads-core (not the independent MA gates): it applies every
// migration to the isolated PG, then walks connect -> bind -> draft -> approve -> publish -> chain -> activate Check -> pause
// through the Go service, the Checker and the sweeper transactions. Evidence tier: REAL_PG with no Meta (MOCK adapter absent).
// It skips unless LC_TEST_DATABASE_ALLOWED=1 (scripts/dev/test-focused.sh sets it). Non-goal: the frozen MA01-MA11 gates.

type adsFx struct {
	t                         *testing.T
	ctx                       context.Context
	owner                     *pgxpool.Pool
	runtime                   *pgxpool.Pool
	worker                    *pgxpool.Pool
	client                    *river.Client[pgx.Tx]
	tenant                    string
	store                     string
	token                     string
	hash                      []byte
	svc                       *Service
	principal                 string
	adBinding, datasetBinding string
}

func pgTestSkip(t *testing.T) string {
	t.Helper()
	if os.Getenv("LC_TEST_DATABASE_ALLOWED") != "1" || os.Getenv("LC_TEST_DATABASE_URL") == "" {
		t.Skip("LC_TEST_DATABASE_ALLOWED=1 and LC_TEST_DATABASE_URL are required; ads real-PG flow NOT_RUN")
	}
	return os.Getenv("LC_TEST_DATABASE_URL")
}

func randHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}

func (f *adsFx) must(sql string, args ...any) {
	f.t.Helper()
	if _, err := f.owner.Exec(f.ctx, sql, args...); err != nil {
		f.t.Fatalf("%s: %v", strings.Fields(sql)[0], err)
	}
}

func (f *adsFx) str(sql string, args ...any) string {
	f.t.Helper()
	var out string
	if err := f.owner.QueryRow(f.ctx, sql, args...).Scan(&out); err != nil {
		f.t.Fatalf("query: %v", err)
	}
	return out
}

var (
	fxOnce   sync.Once
	fxShared *adsFx
)

// sharedAdsFx builds the fixture once per test binary: roles and the migrated schema are process-wide in one PG.
func sharedAdsFx(t *testing.T) *adsFx {
	t.Helper()
	pgTestSkip(t)
	fxOnce.Do(func() { fxShared = newAdsFx(t) })
	if fxShared == nil {
		t.Fatal("ads fixture failed in an earlier test")
	}
	fxShared.t = t
	return fxShared
}

func newAdsFx(t *testing.T) *adsFx {
	dsn := pgTestSkip(t)
	ctx := context.Background()
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil || cfg.ConnConfig.Host != "127.0.0.1" || cfg.ConnConfig.Database != "lc_foundation_test" {
		t.Fatal("refusing database outside 127.0.0.1/lc_foundation_test")
	}
	owner, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	// Pools live until the test binary exits: the fixture is shared by every test of the file.
	if err = migrations.Apply(ctx, owner); err != nil {
		t.Fatalf("migrations: %v", err)
	}
	if err = migrations.Apply(ctx, owner); err != nil {
		t.Fatalf("second migrations apply: %v", err)
	}
	f := &adsFx{t: t, ctx: ctx, owner: owner}
	login := func(name, role string) *pgxpool.Pool {
		secret := randHex(16)
		f.must(`CREATE ROLE ` + name + ` LOGIN NOSUPERUSER NOBYPASSRLS NOCREATEDB NOCREATEROLE IN ROLE ` + role + ` PASSWORD '` + secret + `'`)
		u := *(&url.URL{Scheme: "postgres", Host: cfg.ConnConfig.Host + ":" + strconv.Itoa(int(cfg.ConnConfig.Port)), Path: "/lc_foundation_test", RawQuery: "sslmode=disable"})
		u.User = url.UserPassword(name, secret)
		pool, perr := pgxpool.New(ctx, u.String())
		if perr != nil {
			t.Fatal(perr)
		}
		return pool
	}
	f.runtime = login("ads_test_runtime", "commerce_runtime")
	f.worker = login("ads_test_worker", "commerce_worker")
	// Billing standing is the real 0079 billing.store_standing with 0080's grant to commerce_ads_writer (the pre-merge
	// stub is gone): a store without subscriptions is UNBILLED (pilot, Q4), so approve/publish are not restricted.
	f.tenant, f.store, f.principal = newUUID(f), newUUID(f), newUUID(f)
	f.must(`INSERT INTO control.tenants(id,name) VALUES($1,'ads-tenant')`, f.tenant)
	f.must(`INSERT INTO control.stores(tenant_id,id,name,currency) VALUES($1,$2,'ads-store','TWD')`, f.tenant, f.store)
	f.must(`INSERT INTO identity.principals(id) VALUES($1)`, f.principal)
	f.must(`INSERT INTO identity.memberships(tenant_id,principal_id) VALUES($1,$2)`, f.tenant, f.principal)
	for _, p := range []string{"store:read", "integration:manage", "ads:read", "ads:manage", "ads:approve"} {
		f.must(`INSERT INTO identity.store_grants(tenant_id,store_id,principal_id,permission) VALUES($1,$2,$3,$4)`, f.tenant, f.store, f.principal, p)
	}
	f.token = randHex(32)
	sum := sha256.Sum256([]byte(f.token))
	f.hash = sum[:]
	f.must(`INSERT INTO identity.sessions(id,token_hash,principal_id,audience,expires_at) VALUES($1,$2,$3,'merchant',now()+interval '1 hour')`,
		newUUID(f), f.hash, f.principal)
	f.client, err = river.NewClient(riverpgxv5.New(f.runtime), &river.Config{Schema: "river"})
	if err != nil {
		t.Fatal(err)
	}
	connect := func(_ context.Context, code string, seal SealInfo) (ConnectResult, error) {
		if code != "fake-code" || seal.StoreID != f.store || seal.TenantID != f.tenant {
			return ConnectResult{}, errors.New("unexpected")
		}
		return ConnectResult{ClientBusinessID: "555000111", Scopes: []string{"ads_management", "ads_read", "pages_show_list"},
			Picks: []Pick{{Kind: "ad_account", ID: "9001", Name: "Acct", Currency: "TWD", Timezone: "Asia/Taipei", AccountStatus: 1},
				{Kind: "dataset", ID: "7001", Name: "Pixel"}},
			Token: SealedToken{KeyID: "k1", Enc: make([]byte, 32), Ciphertext: make([]byte, 48)}}, nil
	}
	f.svc, err = NewService(f.client, connect, DialogConfig{AppID: "4291253377792879", ConfigID: "123456789", RedirectURI: "https://admin.example.test/api/admin/ads/meta/callback", GraphVersion: "v26.0", StateKey: bytes.Repeat([]byte{7}, 32)})
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func newUUID(f *adsFx) string { return f.str(`SELECT gen_random_uuid()::text`) }

// scoped runs fn in a runtime-login transaction with the fixture principal's scope (platform.WithScope).
func (f *adsFx) scoped(perm string, fn func(pgx.Tx, platform.Scope) error) error {
	return platform.WithScope(f.ctx, f.runtime, f.token, f.store, perm, fn)
}

func TestAdsCoreRealPGConnectAndBind(t *testing.T) {
	f := sharedAdsFx(t)
	var start ConnectStart
	if err := f.scoped("ads:manage", func(tx pgx.Tx, s platform.Scope) (err error) {
		start, err = f.svc.Connect(f.ctx, tx, s, f.token, "connect-key-0001")
		return err
	}); err != nil {
		t.Fatalf("connect: %v", err)
	}
	u, err := url.Parse(start.DialogURL)
	if err != nil || u.Host != "www.facebook.com" || u.Path != "/v26.0/dialog/oauth" || u.Query().Get("config_id") != "123456789" ||
		u.Query().Get("response_type") != "code" || u.Query().Get("override_default_response_type") != "true" {
		t.Fatalf("dialog url: %v %v", start.DialogURL, err)
	}
	// Replay returns the same dialog URL without the receipt holding the state.
	var again ConnectStart
	if err = f.scoped("ads:manage", func(tx pgx.Tx, s platform.Scope) (e error) {
		again, e = f.svc.Connect(f.ctx, tx, s, f.token, "connect-key-0001")
		return e
	}); err != nil || again.DialogURL != start.DialogURL || again.StateID != start.StateID {
		t.Fatalf("replay: %v %+v", err, again)
	}
	state := u.Query().Get("state")
	// A wrong state is refused with the fixed code; the right one connects.
	if _, err = f.svc.Callback(f.ctx, f.runtime, f.token, f.store, "fake-code", strings.Repeat("A", 43)); !isRefusal(err, "state_mismatch") {
		t.Fatalf("wrong state: %v", err)
	}
	stateID, err := f.svc.Callback(f.ctx, f.runtime, f.token, f.store, "fake-code", state)
	if err != nil || stateID != start.StateID {
		t.Fatalf("callback: %v %s", err, stateID)
	}
	if _, err = f.svc.Callback(f.ctx, f.runtime, f.token, f.store, "fake-code", state); !isRefusal(err, "state_mismatch") {
		t.Fatalf("state replay: %v", err)
	}
	var picks []byte
	if err = f.scoped("ads:manage", func(tx pgx.Tx, s platform.Scope) error {
		var e error
		picks, e = f.svc.GetState(f.ctx, tx, s, f.token, stateID)
		return e
	}); err != nil || !strings.Contains(string(picks), `"9001"`) || strings.Contains(string(picks), "ciphertext") {
		t.Fatalf("get state: %v %s", err, picks)
	}
	// Bind outside the pick list is refused; inside it registers both bindings and the sealed copies.
	err = f.scoped("ads:manage", func(tx pgx.Tx, s platform.Scope) error {
		_, e := f.svc.Bind(f.ctx, tx, s, f.token, "bind-key-000001", BindInput{StateID: stateID, AdAccountID: "424242"})
		return e
	})
	if !isRefusal(err, "not_in_pick_list") {
		t.Fatalf("outside pick list: %v", err)
	}
	var bound BindResult
	if err = f.scoped("ads:manage", func(tx pgx.Tx, s platform.Scope) (e error) {
		bound, e = f.svc.Bind(f.ctx, tx, s, f.token, "bind-key-000002", BindInput{StateID: stateID, AdAccountID: "9001", DatasetID: "7001"})
		return e
	}); err != nil || bound.AdBindingID == "" || bound.DatasetBindingID == "" {
		t.Fatalf("bind: %v %+v", err, bound)
	}
	f.adBinding, f.datasetBinding = bound.AdBindingID, bound.DatasetBindingID
	if n := f.str(`SELECT count(*)::text FROM integration.meta_page_credentials WHERE provider IN ('meta_ads','meta_dataset') AND octet_length(nonce)=32`); n != "2" {
		t.Fatalf("credential rows %s", n)
	}
	if n := f.str(`SELECT count(*)::text FROM ads.oauth_states WHERE pending_ciphertext IS NOT NULL`); n != "0" {
		t.Fatalf("pending token not deleted: %s", n)
	}
	if n := f.str(`SELECT count(*)::text FROM ads.connections WHERE token_version=1`); n != "2" {
		t.Fatalf("connections %s", n)
	}
	// Settings default: SANDBOX, allowance 0.
	var settings []byte
	if err = f.scoped("ads:read", func(tx pgx.Tx, s platform.Scope) (e error) {
		settings, e = f.svc.GetSettings(f.ctx, tx, s, f.token)
		return e
	}); err != nil || !strings.Contains(string(settings), `"environment": "SANDBOX"`) || !strings.Contains(string(settings), `"max_active_budget_minor": 0`) {
		t.Fatalf("settings: %v %s", err, settings)
	}
}

func isRefusal(err error, code string) bool {
	var r *Refusal
	return errors.As(err, &r) && r.Code == code
}

// operatorSet calls ads.operator_set_settings as the commerce_meta_registrar role (its only EXECUTE holder).
func (f *adsFx) operatorSet(env, sandbox string, budget int64) {
	f.t.Helper()
	tx, err := f.owner.Begin(f.ctx)
	if err != nil {
		f.t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(f.ctx) }()
	if _, err = tx.Exec(f.ctx, `SET LOCAL ROLE commerce_meta_registrar`); err != nil {
		f.t.Fatal(err)
	}
	if _, err = tx.Exec(f.ctx, `SELECT ads.operator_set_settings($1::uuid,$2::uuid,$3,$4,$5,NULL)`, f.tenant, f.store, env, sandbox, budget); err != nil {
		f.t.Fatalf("operator_set_settings: %v", err)
	}
	if err = tx.Commit(f.ctx); err != nil {
		f.t.Fatal(err)
	}
}

// finish simulates the dispatcher: claim (DISPATCHING event) then complete an operation as SUCCEEDED with a reference.
func (f *adsFx) finish(op, state, reference string) {
	f.t.Helper()
	f.must(`INSERT INTO integration.operation_events(tenant_id,store_id,operation_id,generation,state,mode,reason_code)
		VALUES($1,$2,$3,1,'DISPATCHING','dispatch','dispatch_claimed')`, f.tenant, f.store, op)
	f.must(`UPDATE integration.operations SET state=$2,generation=1,provider_reference=$3,updated_at=clock_timestamp() WHERE id=$1`, op, state, reference)
	f.must(`INSERT INTO integration.operation_events(tenant_id,store_id,operation_id,generation,state,mode,reason_code)
		VALUES($1,$2,$3,1,$4,'','done')`, f.tenant, f.store, op, state)
}

func (f *adsFx) opOf(draft, kind string, seq int) string {
	f.t.Helper()
	return f.str(`SELECT operation_id::text FROM ads.remote_objects WHERE draft_id=$1 AND kind=$2 AND seq=$3 ORDER BY publish_attempt DESC LIMIT 1`, draft, kind, seq)
}

func (f *adsFx) advance(w *advanceWorker, draft string) {
	f.t.Helper()
	if err := w.advanceOne(f.ctx, f.client, draft); err != nil {
		f.t.Fatalf("advance: %v", err)
	}
}

func (f *adsFx) draftField(draft, key string) string {
	f.t.Helper()
	var raw json.RawMessage
	if err := f.scoped("ads:read", func(tx pgx.Tx, s platform.Scope) (e error) {
		raw, e = f.svc.GetDraft(f.ctx, tx, s, f.token, draft)
		return e
	}); err != nil {
		f.t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		f.t.Fatal(err)
	}
	if v, ok := m[key]; ok {
		return strings.Trim(strings.TrimSpace(jsonString(v)), `"`)
	}
	return ""
}

func jsonString(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

func denied(err error, code string) bool {
	var d core.PolicyDenial
	return errors.As(err, &d) && d.Code == code && errors.Is(err, core.ErrPolicyDenied)
}

func TestAdsCoreRealPGDraftLifecycle(t *testing.T) {
	f := sharedAdsFx(t)
	if f.adBinding == "" {
		t.Skip("connect step did not run in this selection")
	}
	fbBinding := newUUID(f)
	f.must(`INSERT INTO integration.bindings(id,tenant_id,store_id,principal_id,provider,external_asset_id) VALUES($1,$2,$3,$4,'facebook','111')`,
		fbBinding, f.tenant, f.store, f.principal)
	now := time.Now().UTC()
	in := DraftInput{AdBindingID: f.adBinding, IdentityBindingID: fbBinding, Template: "BOOST_POST", SourceRef: "111_222", Currency: "TWD",
		LifetimeBudgetMinor: 300000, StartsAt: now.Add(11 * time.Minute), EndsAt: now.Add(49 * time.Hour), Countries: []string{"TW", "HK"}, AgeMin: 18, AgeMax: 65}
	if err := ValidateInput(DraftInput{LifetimeBudgetMinor: 12345}, "", now); err == nil {
		t.Fatal("empty input accepted")
	}
	bad := in
	bad.LifetimeBudgetMinor = 12345
	if err := ValidateInput(bad, "", now); !isRefusal(err, "not_whole_unit") {
		t.Fatalf("12345 TWD: %v", err)
	}
	var draft string
	create := func(key string, input DraftInput) (json.RawMessage, error) {
		var raw json.RawMessage
		err := f.scoped("ads:manage", func(tx pgx.Tx, s platform.Scope) (e error) {
			raw, e = f.svc.CreateDraft(f.ctx, tx, s, f.token, key, input)
			return e
		})
		return raw, err
	}
	raw, err := create("draft-key-000001", in)
	if err != nil {
		t.Fatalf("create draft: %v", err)
	}
	var created struct{ ID, Status string }
	if err = json.Unmarshal(raw, &created); err != nil || created.Status != "DRAFT" {
		t.Fatalf("created: %v %s", err, raw)
	}
	draft = created.ID
	// Foreign source (another Page's post) and currency mismatch are refused by SQL.
	foreign := in
	foreign.SourceRef = "999_222"
	if _, err = create("draft-key-000002", foreign); !isRefusal(err, "source_not_owned") {
		t.Fatalf("foreign source: %v", err)
	}
	usd := in
	usd.Currency = "USD"
	if _, err = create("draft-key-000003", usd); !isRefusal(err, "currency_mismatch") {
		t.Fatalf("usd: %v", err)
	}
	approve := func(rev int) error {
		return f.scoped("ads:approve", func(tx pgx.Tx, s platform.Scope) error {
			_, e := f.svc.Approve(f.ctx, tx, s, f.token, draft, rev)
			return e
		})
	}
	if err = approve(1); !isRefusal(err, "over_allowance") {
		t.Fatalf("approve with allowance 0: %v", err)
	}
	if err = approve(2); !isRefusal(err, "revision_changed") {
		t.Fatalf("stale revision: %v", err)
	}
	f.operatorSet("SANDBOX", "9001", 1_000_000)
	if err = approve(1); err != nil {
		t.Fatalf("approve: %v", err)
	}
	if err = approve(1); err != nil {
		t.Fatalf("approve replay: %v", err)
	}
	if st := f.draftField(draft, "status"); st != "APPROVED" {
		t.Fatalf("status %s", st)
	}
	err = f.scoped("ads:manage", func(tx pgx.Tx, s platform.Scope) error {
		_, e := f.svc.UpdateDraft(f.ctx, tx, s, f.token, "draft-key-000004", draft, 1, in)
		return e
	})
	if !isRefusal(err, "draft_approved") {
		t.Fatalf("edit after approval: %v", err)
	}
	// Canonical hash: Go and SQL agree byte for byte.
	canon := f.str(`SELECT ads.canonical_draft($1::uuid)`, draft)
	want := string(CanonicalDraft(DraftCanon{Template: "BOOST_POST", AdBindingID: f.adBinding, AdBindingVersion: 1, IdentityBindingID: fbBinding,
		IdentityBindingVersion: 1, SourceRef: "111_222", Currency: "TWD", LifetimeBudgetMinor: 300000,
		StartsAtUnix: in.StartsAt.Truncate(time.Second).Unix(), EndsAtUnix: in.EndsAt.Truncate(time.Second).Unix(),
		Countries: []string{"TW", "HK"}, AgeMin: 18, AgeMax: 65}))
	if canon != want {
		t.Fatalf("canonical mismatch\nsql=%s\ngo =%s", canon, want)
	}
	// Publish: CAS, lane, Check.
	publish := func(key string, attempt int) error {
		return f.scoped("ads:approve", func(tx pgx.Tx, s platform.Scope) error {
			_, e := f.svc.Publish(f.ctx, tx, s, f.token, key, draft, attempt)
			return e
		})
	}
	if err = publish("publish-key-0001", 0); err != nil {
		t.Fatalf("publish: %v", err)
	}
	if err = publish("publish-key-0002", 0); !isRefusal(err, "attempt_changed") {
		t.Fatalf("concurrent re-publish: %v", err)
	}
	campaignOp := f.opOf(draft, "campaign", 1)
	if q := f.str(`SELECT r.queue||':'||r.priority::text FROM river.river_job r JOIN integration.operations o ON o.job_id=r.id WHERE o.id=$1`, campaignOp); q != "ads:3" {
		t.Fatalf("job lane %s", q)
	}
	checker := NewChecker(f.worker)
	req := func(op, action string) core.DispatchRequest {
		return core.DispatchRequest{OperationID: op, Provider: "meta_ads", Action: action, Purpose: "marketing", Mode: "dispatch"}
	}
	if err = checker.Check(f.ctx, req(campaignOp, "meta.ads.create_campaign")); err != nil {
		t.Fatalf("check create: %v", err)
	}
	f.operatorSet("SANDBOX", "1", 1_000_000)
	if err = checker.Check(f.ctx, req(campaignOp, "meta.ads.create_campaign")); !denied(err, "sandbox_account_required") {
		t.Fatalf("sandbox rule: %v", err)
	}
	if err = checker.Check(f.ctx, core.DispatchRequest{OperationID: campaignOp, Provider: "meta_ads", Action: "meta.ads.create_campaign", Mode: "reconcile"}); err != nil {
		t.Fatalf("reconcile-mode check must pass: %v", err)
	}
	if err = checker.Check(f.ctx, req(campaignOp, "meta.ads.delete_everything")); !denied(err, "unknown_action") {
		t.Fatalf("unknown action: %v", err)
	}
	f.operatorSet("SANDBOX", "9001", 1_000_000)
	// The sweeper chain: adset, creative, ad, preflight, activate (never before the previous is SUCCEEDED and pinned).
	w := &advanceWorker{pool: f.worker}
	f.advance(w, draft)
	if n := f.str(`SELECT count(*)::text FROM ads.remote_objects WHERE draft_id=$1 AND kind<>'campaign'`, draft); n != "0" {
		t.Fatalf("planned before campaign succeeded: %s", n)
	}
	f.finish(campaignOp, "SUCCEEDED", "1001")
	for i, step := range []struct{ kind, ref string }{{"adset", "1002"}, {"creative", "1003"}, {"ad", "1004"}} {
		f.advance(w, draft) // pins the previous id and plans this step
		op := f.opOf(draft, step.kind, 1)
		if q := f.str(`SELECT o.action||':'||r.queue||':'||r.priority::text FROM integration.operations o JOIN river.river_job r ON r.id=o.job_id WHERE o.id=$1`, op); !strings.HasSuffix(q, ":ads:3") {
			t.Fatalf("step %d lane %s", i, q)
		}
		f.finish(op, "SUCCEEDED", step.ref)
	}
	f.advance(w, draft)
	preflightOp := f.opOf(draft, "preflight", 1)
	f.finish(preflightOp, "SUCCEEDED", "v1;st=1;cur=TWD;fund=0;tz=Asia/Taipei")
	f.advance(w, draft)
	activateOp := f.opOf(draft, "activate", 1)
	if st := f.draftField(draft, "status"); st != "SUBMITTING" {
		t.Fatalf("status before activate completes: %s", st)
	}
	var req1 struct {
		CampaignID string `json:"campaign_id"`
		Attempt    int    `json:"attempt"`
	}
	if err = json.Unmarshal([]byte(f.str(`SELECT request::text FROM integration.operations WHERE id=$1`, activateOp)), &req1); err != nil || req1.CampaignID != "1001" || req1.Attempt != 1 {
		t.Fatalf("activate request: %v %+v", err, req1)
	}
	if err = checker.Check(f.ctx, req(activateOp, "meta.ads.activate")); err != nil {
		t.Fatalf("check activate: %v", err)
	}
	f.operatorSet("SANDBOX", "9001", 100)
	if err = checker.Check(f.ctx, req(activateOp, "meta.ads.activate")); !denied(err, "over_allowance") {
		t.Fatalf("allowance: %v", err)
	}
	f.operatorSet("SANDBOX", "9001", 1_000_000)
	// Pause is always allowed and forbids any later activation.
	pause := func(key string) error {
		return f.scoped("ads:manage", func(tx pgx.Tx, s platform.Scope) error {
			_, e := f.svc.Pause(f.ctx, tx, s, f.token, key, draft)
			return e
		})
	}
	if err = pause("pause-key-000001"); err != nil {
		t.Fatalf("pause: %v", err)
	}
	pauseOp := f.opOf(draft, "pause", 1)
	if q := f.str(`SELECT r.queue||':'||r.priority::text FROM river.river_job r JOIN integration.operations o ON o.job_id=r.id WHERE o.id=$1`, pauseOp); q != "ads:1" {
		t.Fatalf("pause lane %s", q)
	}
	if err = checker.Check(f.ctx, req(activateOp, "meta.ads.activate")); !denied(err, "pause_requested") {
		t.Fatalf("activate after pause: %v", err)
	}
	if err = checker.Check(f.ctx, req(pauseOp, "meta.ads.pause")); err != nil {
		t.Fatalf("pause check: %v", err)
	}
	// Late activate: the pause was claimed, then an activate completed after it: the sweeper plans pause seq 2.
	f.must(`INSERT INTO integration.operation_events(tenant_id,store_id,operation_id,generation,state,mode,reason_code)
		VALUES($1,$2,$3,1,'DISPATCHING','dispatch','dispatch_claimed')`, f.tenant, f.store, pauseOp)
	time.Sleep(20 * time.Millisecond)
	f.finish(activateOp, "SUCCEEDED", "")
	f.advance(w, draft)
	if n := f.str(`SELECT count(*)::text FROM ads.remote_objects WHERE draft_id=$1 AND kind='pause'`, draft); n != "2" {
		t.Fatalf("late activate did not plan pause seq 2 (pauses=%s)", n)
	}
	// Insights: plan, ingest exactly, auto-pause condition, report.
	iw := &insightsWorker{pool: f.worker}
	if err = iw.planOne(f.ctx, f.client, draft); err != nil {
		t.Fatalf("insights plan: %v", err)
	}
	read := f.str(`SELECT operation_id::text FROM ads.insight_reads WHERE draft_id=$1 ORDER BY day LIMIT 1`, draft)
	if read == "" {
		t.Fatal("no read planned")
	}
	f.finish(read, "SUCCEEDED", "v1;es=ACTIVE;sp=12.34;im=100;cl=5;pu=na;pv=na;cur=TWD;tz=Asia/Taipei")
	if ok := f.str(`SELECT ads.put_insights_day($1::uuid)::text`, read); ok != "true" {
		t.Fatalf("ingest: %s", ok)
	}
	if v := f.str(`SELECT spend_minor::text FROM ads.insights_daily WHERE draft_id=$1 LIMIT 1`, draft); v != "1234" {
		t.Fatalf("spend minor %s", v)
	}
	// AD6: a pause SUCCEEDED claimed after the activate left DISPATCHING releases the allowance; the activate ended first.
	if c := f.str(`SELECT ads.draft_counts($1::uuid)::text`, draft); c != "true" {
		t.Fatalf("draft must still count before a later pause succeeds: %s", c)
	}
	pause2 := f.opOf(draft, "pause", 2)
	f.finish(pause2, "SUCCEEDED", "")
	if c := f.str(`SELECT ads.draft_counts($1::uuid)::text`, draft); c != "false" {
		t.Fatalf("draft must stop counting after a later pause succeeded: %s", c)
	}
	if due := f.str(`SELECT ads.auto_pause_due($1::uuid,'1001')::text`, draft); due != "false" {
		t.Fatalf("auto pause too early: %s", due)
	}
	var report json.RawMessage
	day := f.str(`SELECT day::text FROM ads.insights_daily WHERE draft_id=$1 LIMIT 1`, draft) // store-local (Asia/Taipei) date of the read
	if err = f.scoped("ads:read", func(tx pgx.Tx, s platform.Scope) (e error) {
		report, e = f.svc.Report(f.ctx, tx, s, f.token, day, day)
		return e
	}); err != nil || !strings.Contains(string(report), `"spend_minor": 1234`) || !strings.Contains(string(report), `"card_payments_only"`) {
		t.Fatalf("report: %v %s", err, report)
	}
	// End: marks the draft ended; a pause op is already in flight, so nothing more is planned (pause_view path).
	var ended json.RawMessage
	if err = f.scoped("ads:approve", func(tx pgx.Tx, s platform.Scope) (e error) {
		ended, e = f.svc.End(f.ctx, tx, s, f.token, "end-key-0000001", draft)
		return e
	}); err != nil || f.draftField(draft, "status") != "ENDED" || !strings.Contains(string(ended), `"ended_at"`) {
		t.Fatalf("end: %v %s", err, ended)
	}
	// CAPI switch: a dataset binding needs a Test Events code while the store is in SANDBOX.
	setCapi := func(key string, in CapiInput) (json.RawMessage, error) {
		var out json.RawMessage
		err := f.scoped("ads:manage", func(tx pgx.Tx, s platform.Scope) (e error) {
			out, e = f.svc.SetCapi(f.ctx, tx, s, f.token, key, in)
			return e
		})
		return out, err
	}
	if _, err = setCapi("capi-key-0000001", CapiInput{Enabled: true, DatasetBindingID: f.datasetBinding}); !isRefusal(err, "invalid_request") {
		t.Fatalf("sandbox capi without test code: %v", err)
	}
	if out, e := setCapi("capi-key-0000002", CapiInput{Enabled: true, DatasetBindingID: f.datasetBinding, TestEventCode: "TEST1234"}); e != nil ||
		!strings.Contains(string(out), `"enabled": true`) {
		t.Fatalf("capi: %v %s", e, out)
	}
	if n := f.str(`SELECT (capi_enabled_by=$1::uuid)::text FROM ads.store_settings WHERE store_id=$2`, f.principal, f.store); n != "true" {
		t.Fatalf("capi_enabled_by not the resolved principal: %s", n)
	}
	// River guard: a wrong priority on the ads lane is refused for the runtime login.
	err = f.scoped("ads:manage", func(tx pgx.Tx, s platform.Scope) error {
		_, e := tx.Exec(f.ctx, `INSERT INTO river.river_job(kind,queue,args,priority,state,max_attempts) VALUES('external_operation_v1','ads',
			jsonb_build_object('operation_id',gen_random_uuid()::text,'version',1),2,'available',25)`)
		return e
	})
	if err == nil {
		t.Fatal("ads lane accepted priority 2")
	}
	// A merchant cannot change the environment or allowance: no path exists (no runtime grant on ads tables).
	if _, err = f.runtime.Exec(f.ctx, `UPDATE ads.store_settings SET max_active_budget_minor=1`); err == nil {
		t.Fatal("runtime updated ads.store_settings directly")
	}
	if _, err = f.runtime.Exec(f.ctx, `SELECT ads.operator_set_settings($1::uuid,$2::uuid,'LIVE',NULL,NULL,NULL)`, f.tenant, f.store); err == nil {
		t.Fatal("runtime executed the operator definer")
	}
}

// fakePool is a never-used pool handle for registration tests (pgxpool.New does not dial until first use).
func fakePool() *pgxpool.Pool {
	pool, err := pgxpool.New(context.Background(), "postgres://user@127.0.0.1:1/none?sslmode=disable&pool_max_conns=1")
	if err != nil {
		panic(err)
	}
	return pool
}

// TestAdsCoreRealPGAdvanceBeyondFirstPage (review R1 P1): ads.advance_candidates is global and a published draft stays a
// candidate for days, so more than one page of candidates must all be advanced in a run. 51 published drafts (plus the
// lifecycle test's leftovers) each get their adset planned after the campaign SUCCEEDED; before the fix only the first 50 by
// approved_at were ever looked at.
func TestAdsCoreRealPGAdvanceBeyondFirstPage(t *testing.T) {
	f := sharedAdsFx(t)
	if f.adBinding == "" {
		t.Skip("connect step did not run in this selection")
	}
	fbBinding := newUUID(f)
	f.must(`INSERT INTO integration.bindings(id,tenant_id,store_id,principal_id,provider,external_asset_id) VALUES($1,$2,$3,$4,'facebook','111')`,
		fbBinding, f.tenant, f.store, f.principal)
	f.operatorSet("SANDBOX", "9001", 1_000_000_000)
	now := time.Now().UTC()
	const n = advancePage + 1
	drafts := make([]string, 0, n)
	for i := 0; i < n; i++ {
		in := DraftInput{AdBindingID: f.adBinding, IdentityBindingID: fbBinding, Template: "BOOST_POST", SourceRef: "111_9" + strconv.Itoa(i), Currency: "TWD",
			LifetimeBudgetMinor: 300000, StartsAt: now.Add(11 * time.Minute), EndsAt: now.Add(49 * time.Hour), Countries: []string{"TW"}, AgeMin: 18, AgeMax: 65}
		var created struct{ ID string }
		if err := f.scoped("ads:manage", func(tx pgx.Tx, s platform.Scope) error {
			raw, e := f.svc.CreateDraft(f.ctx, tx, s, f.token, "page-draft-key-"+strconv.Itoa(1000+i), in)
			if e == nil {
				e = json.Unmarshal(raw, &created)
			}
			return e
		}); err != nil {
			t.Fatalf("create %d: %v", i, err)
		}
		if err := f.scoped("ads:approve", func(tx pgx.Tx, s platform.Scope) error {
			if _, e := f.svc.Approve(f.ctx, tx, s, f.token, created.ID, 1); e != nil {
				return e
			}
			_, e := f.svc.Publish(f.ctx, tx, s, f.token, "page-publish-key-"+strconv.Itoa(1000+i), created.ID, 0)
			return e
		}); err != nil {
			t.Fatalf("approve/publish %d: %v", i, err)
		}
		f.finish(f.opOf(created.ID, "campaign", 1), "SUCCEEDED", strconv.Itoa(5000+i))
		drafts = append(drafts, created.ID)
	}
	w := &advanceWorker{pool: f.worker}
	if err := w.advanceAll(f.ctx, f.client); err != nil {
		t.Fatalf("advanceAll: %v", err)
	}
	for i, d := range drafts {
		if c := f.str(`SELECT count(*)::text FROM ads.remote_objects WHERE draft_id=$1 AND kind='adset'`, d); c != "1" {
			t.Fatalf("draft %d of %d (approved position beyond one page) has %s adset ops planned, want 1", i+1, n, c)
		}
	}
	// The SQL contract itself: offset pages are disjoint and their union covers more than one page.
	seen := map[string]bool{}
	for off := 0; ; off += advancePage {
		rows, err := f.worker.Query(f.ctx, `SELECT d::text FROM ads.advance_candidates($1,$2) d`, advancePage, off)
		if err != nil {
			t.Fatal(err)
		}
		k := 0
		for rows.Next() {
			var id string
			if err = rows.Scan(&id); err != nil {
				t.Fatal(err)
			}
			if seen[id] {
				t.Fatalf("draft %s on two pages", id)
			}
			seen[id] = true
			k++
		}
		rows.Close()
		if k < advancePage {
			break
		}
	}
	if len(seen) < n {
		t.Fatalf("pages cover %d drafts, want at least %d", len(seen), n)
	}
	if _, err := f.worker.Exec(f.ctx, `SELECT ads.advance_candidates(10,-1)`); err == nil {
		t.Fatal("negative offset accepted")
	}
}
