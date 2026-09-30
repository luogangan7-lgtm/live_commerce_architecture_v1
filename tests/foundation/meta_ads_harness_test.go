package foundation_test

// Shared MOCK harness of the independent meta-ads gates (contracts/meta-ads-v1.md §9 MA02..MA08, MA11). Written by the
// ads-tests author from the FROZEN contract and the frozen HTTP / Go / SQL signatures of ads-core, ads-graph and
// ads-capi; expected values are assertions of the contract, not of the implementation.
//
// What one adsEnv is (helper prefix `ade`/`adsEnv`): a fresh store of the shared fixture tenant; a creator principal and
// further members with explicit ads grants per case (A-1: no auto grants); the real merchant HTTP surface
// (httpapi.NewHandler + ads.Service over the real commerce_runtime pool, the real metaads.OAuth against the fake Graph);
// the real dispatcher with metaads.Routes and capiroute.Routes over a commerce_worker login, the real ads sweepers, all on
// ONE real River client working queue `ads` only; the fake Graph (tests/ads/fakegraph) on loopback as GraphBaseURL.
//
// Ops, approvals, facts and grants come from the real definers and the real HTTP/OAuth path. The owner pool (a fixture
// superuser) is used only for the disclosed things a runtime login can never do: synthetic Facebook Page identity
// binding, aged timestamps (session_replication_role=replica), out-of-band grant revocation, holding a River job so an
// operation stays READY, read-back of columns no runtime role may read, and the end-of-test retirement of the store's
// ads state so sweepers of later tests never see it (advance_candidates is global). Nothing here reaches a network.
//
// Evidence label: MOCK (REAL_PG + River + fake Graph).

import (
	"bytes"
	"context"
	"crypto/ecdh"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"

	"livecommerce/internal/ads"
	"livecommerce/internal/attribution"
	"livecommerce/internal/attribution/capiroute"
	"livecommerce/internal/httpapi"
	integration "livecommerce/internal/integrations/core"
	metaads "livecommerce/internal/integrations/meta_ads"
	"livecommerce/internal/integrations/meta_ads/tokenopen"
	"livecommerce/tests/ads/fakegraph"
)

const (
	adsApp          = "4291253377792879" // app 大梦 (O-C); an id, not a secret
	adsRedirect     = "https://admin.mock.example.test/api/ads/meta/callback"
	adsConfigID     = "1230000000001"
	adsVersion      = "v26.0"
	adsPartner      = "lc_ads_tests"
	adsKeyID        = "adstest_k1"
	adsExternalKey  = "synthetic capi external id sentinel for tests (63 bytes long).."
	adsPerms        = "ads:read,ads:manage,ads:approve,integration:manage"
	adsDefaultAllow = 1_000_000 // NT$10,000 in minor units
)

var adsAppSecret = "SENTINEL-APPSECRET-" + strings.Repeat("a1b2", 8)

// ---- HPKE key material (one pair per test binary; synthetic) -----------------------------------------------------

var (
	adsKeyOnce sync.Once
	adsPrivEnv map[string]string // the private-key env of the worker processes (child process helper)
	adsPubEnv  map[string]string
	adsRing    *tokenopen.Keyring
	adsKeyErr  error
)

func adsKeys(t *testing.T) (map[string]string, *tokenopen.Keyring) {
	t.Helper()
	adsKeyOnce.Do(func() {
		priv, err := ecdh.X25519().GenerateKey(rand.Reader)
		if err != nil {
			adsKeyErr = err
			return
		}
		pub := base64.StdEncoding.EncodeToString(priv.PublicKey().Bytes())
		prv := base64.StdEncoding.EncodeToString(priv.Bytes())
		adsPubEnv = map[string]string{
			"COMMERCE_META_ADS_TOKEN_HPKE_PUBLIC_KEYS_JSON": fmt.Sprintf(`{"keys":[{"id":%q,"public_key_base64":%q}]}`, adsKeyID, pub),
			"COMMERCE_META_ADS_TOKEN_HPKE_ACTIVE_KEY_ID":    adsKeyID,
		}
		env := map[string]string{"COMMERCE_META_ADS_TOKEN_HPKE_PRIVATE_KEYS": fmt.Sprintf(`{"keys":[{"id":%q,"private_key_base64":%q}]}`, adsKeyID, prv)}
		adsPrivEnv = env
		adsRing, adsKeyErr = tokenopen.LoadKeyring(func(k string) string { return env[k] })
	})
	if adsKeyErr != nil {
		t.Fatalf("synthetic HPKE keys: %v", adsKeyErr)
	}
	return adsPubEnv, adsRing
}

// ---- probe: what the dispatcher showed the Check / loader hooks ---------------------------------------------------

type adsCheckRec struct {
	Op, Action, Provider, Mode string
	GraphCalls                 int // fake Graph requests at entry of Check
}

type adsLoadRec struct {
	Claim   integration.SecretClaim
	Op      string
	OpState string // integration.operations.state read by the owner pool at loader entry
	Lease   string // lease_mode at loader entry
	InCheck bool   // a Check of the same op was still running (must never be true)
}

type adsProbe struct {
	mu      sync.Mutex
	checks  []adsCheckRec
	loads   []adsLoadRec
	inCheck map[string]int
	gate    map[string]chan struct{} // action -> release channel; the first matching Check blocks until closed
	entered map[string]chan struct{} // action -> closed when a gated Check has been entered
}

func (p *adsProbe) Checks() []adsCheckRec {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]adsCheckRec(nil), p.checks...)
}
func (p *adsProbe) Loads() []adsLoadRec {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]adsLoadRec(nil), p.loads...)
}

// ---- options ------------------------------------------------------------------------------------------------------

type adsOpts struct {
	noWorker    bool          // no River client: nothing dispatches, no sweeper runs
	live        bool          // store environment LIVE (default SANDBOX with sandbox_ad_account = the bound account)
	allowance   int64         // minor units (default adsDefaultAllow); -1 leaves the operator default (0 = ads off, O4)
	dataset     bool          // also bind a dataset (CAPI)
	maxGen      int64         // dispatcher MaxGenerations (default 6)
	noConnect   bool          // do not run the connect chain
	unfunded    bool          // account has no funding source
	retryDelay  time.Duration // dispatcher RetryDelay between an UNKNOWN and its reconcile claim (default 150ms)
	noDispatch  bool          // the parent works queue ads_sweep only: a child process (or e.startDispatch) dispatches
	origin      string        // storefront origin the CAPI env publishes (default synthetic; the buyer browser gate needs https://buyer.example)
	fx          *testFixture  // attach the ads surface to an existing store (phase B: the payment fixture's private tenant/store, principalA, tokens["a"])
	callTimeout time.Duration // dispatcher CallTimeout (default 700ms; race tests hold a Graph call in flight and need longer)
}

type adsEnv struct {
	t       *testing.T
	f       *testFixture
	ctx     context.Context
	opts    adsOpts
	g       *fakegraph.Server
	tenant  string
	store   string
	creator string // principal id
	token   string // creator merchant token
	handler http.Handler
	svc     *ads.Service

	account, clientBiz, pageAsset, pixel string
	botToken                             string // the BISU token the fake Graph hands out
	adBinding, dsBinding, idBinding      string
	workerPool, registrarPool            *pgxpool.Pool
	client                               *river.Client[pgx.Tx]
	sweepIns                             *river.Client[pgx.Tx] // insert-only, owner pool: sweeper jobs go to queue ads_sweep so `ads` can be paused
	cfg                                  metaads.Config
	probe                                *adsProbe
	routes                               []integration.DispatchRoute
	principals                           map[string]string // extra members: label -> token
	drafts                               []string
	dispatchOpts                         integration.DispatcherOptions
	onLoad                               func(integration.SecretClaim) // test hook: runs inside the loader wrapper before the real loader
}

type adsSweepArgs struct{ kind string }

func (a adsSweepArgs) Kind() string { return a.kind }

func adsDigits(n int) string { return mciDigits(n) }

// newAdsEnv builds one env. It registers every cleanup; the caller only uses it.
func newAdsEnv(t *testing.T, o adsOpts) *adsEnv {
	t.Helper()
	f := fixture(t)
	if o.fx != nil {
		f = o.fx
	}
	pubEnv, ring := adsKeys(t)
	ctx := context.Background()
	e := &adsEnv{t: t, f: f, ctx: ctx, opts: o, tenant: f.tenantA, principals: map[string]string{}, probe: &adsProbe{inCheck: map[string]int{}, gate: map[string]chan struct{}{}, entered: map[string]chan struct{}{}}}
	if o.allowance == 0 {
		o.allowance = adsDefaultAllow
	}
	e.opts.allowance = o.allowance
	e.g = fakegraph.New()
	t.Cleanup(e.g.Close)
	if o.fx != nil {
		e.tenant, e.store, e.creator, e.token = f.tenantA, f.storeA1, f.principalA, f.tokens["a"]
		mustExec(t, f.owner, `INSERT INTO identity.store_grants(tenant_id,store_id,principal_id,permission) SELECT $1,$2,$3,p FROM unnest(ARRAY['ads:read','ads:manage','ads:approve','integration:manage']) p ON CONFLICT DO NOTHING`, e.tenant, e.store, e.creator)
		t.Cleanup(e.retire)
	} else {
		e.store = cbxStore(t, f, e.tenant)
		t.Cleanup(e.retire) // runs after the River client stopped (registered later, so it runs earlier)
		e.creator, e.token = lcPrincipal(t, f, e.tenant, []string{e.store}, "store:read", "ads:read", "ads:manage", "ads:approve", "integration:manage")
	}
	e.account, e.clientBiz, e.pageAsset, e.pixel = "91"+adsDigits(11), "51"+adsDigits(11), "77"+adsDigits(11), "88"+adsDigits(11)
	e.botToken = "SENTINEL-EAAG-BISU-" + t04Tag() + t04Tag()
	e.g.AddAccount(fakegraph.Account{ID: e.account, Currency: "TWD", Timezone: "Asia/Taipei", Status: 1, Funded: !o.unfunded, MinDailyBudget: "100"})
	pixels := map[string][]fakegraph.Pixel{}
	if o.dataset {
		pixels[e.account] = []fakegraph.Pixel{{ID: e.pixel, Name: "Synthetic dataset"}}
	}
	e.g.Grant(e.botToken, e.clientBiz, []string{"ads_management", "ads_read", "business_management", "pages_show_list"}, []string{e.account}, pixels)
	e.g.ExpectApp(adsApp, adsAppSecret, adsRedirect)

	e.workerPool = miPool(t, f, "commerce_worker")
	e.registrarPool = miPool(t, f, "commerce_meta_registrar")

	// merchant HTTP surface: the real service + real OAuth against the fake Graph
	e.cfg = metaads.Config{GraphBaseURL: e.g.URL(), GraphVersion: adsVersion, PartnerAgent: adsPartner}
	seal, err := metaads.LoadSealKeys(func(k string) string { return pubEnv[k] })
	if err != nil {
		t.Fatalf("seal keys: %v", err)
	}
	oauth, err := metaads.NewOAuth(e.cfg, metaads.AppConfig{AppID: adsApp, RedirectURI: adsRedirect, AppSecret: []byte(adsAppSecret)}, seal)
	if err != nil {
		t.Fatalf("NewOAuth: %v", err)
	}
	jobs, err := river.NewClient[pgx.Tx](riverpgxv5.New(f.runtime), &river.Config{Schema: "river"})
	if err != nil {
		t.Fatal(err)
	}
	e.svc, err = ads.NewService(jobs, oauth.Connect, ads.DialogConfig{AppID: adsApp, ConfigID: adsConfigID, RedirectURI: adsRedirect, GraphVersion: adsVersion, StateKey: bytes.Repeat([]byte{7}, 32)})
	if err != nil {
		t.Fatalf("ads.NewService: %v", err)
	}
	e.handler = httpapi.NewHandler(f.runtime, httpapi.Options{Ads: e.svc})

	// Facebook Page identity binding (disclosed fixture: the page-token self-serve connect is a non-goal of ads-core)
	e.idBinding = randomUUID()
	mustExec(t, f.owner, `INSERT INTO integration.bindings(id,tenant_id,store_id,principal_id,provider,external_asset_id) VALUES($1,$2,$3,$4,'facebook',$5)`,
		e.idBinding, e.tenant, e.store, e.creator, e.pageAsset)

	// operator settings first (default env SANDBOX, allowance 0): only the operator CLI definer may write them
	e.startWorker(ring)
	if !o.noConnect {
		e.connect()
	}
	if o.allowance > 0 { // -1 leaves the operator default (SANDBOX, allowance 0 = ads off, O4)
		env := "SANDBOX"
		if o.live {
			env = "LIVE"
		}
		e.setSettings(env, e.account, o.allowance, "TWD")
	}
	return e
}

// startWorker starts the ONE River client (queue ads only): dispatcher + ads sweepers + CAPI sweeper.
func (e *adsEnv) startWorker(ring *tokenopen.Keyring) {
	t := e.t
	if e.opts.noWorker {
		return
	}
	checker := ads.NewChecker(e.workerPool)
	check := func(ctx context.Context, req integration.DispatchRequest) error {
		p := e.probe
		p.mu.Lock()
		p.checks = append(p.checks, adsCheckRec{Op: req.OperationID, Action: req.Action, Provider: req.Provider, Mode: req.Mode, GraphCalls: e.g.Count("")})
		p.inCheck[req.OperationID]++
		gate := p.gate[req.Action]
		entered := p.entered[req.Action]
		if gate != nil {
			delete(p.gate, req.Action)
		}
		p.mu.Unlock()
		defer func() { p.mu.Lock(); p.inCheck[req.OperationID]--; p.mu.Unlock() }()
		if gate != nil {
			close(entered)
			select {
			case <-gate:
			case <-ctx.Done():
			}
		}
		if req.Provider == "meta_dataset" { // CAPI's own Check is inside the capi route; this wrapper is for meta_ads only
			return nil
		}
		return checker.Check(ctx, req)
	}
	routes, err := metaads.Routes(e.workerPool, e.cfg, ring, check)
	if err != nil {
		t.Fatalf("metaads.Routes: %v", err)
	}
	capi, err := capiroute.Routes(e.workerPool, e.cfg, ring, []byte(adsExternalKey))
	if err != nil {
		t.Fatalf("capiroute.Routes: %v", err)
	}
	all := append(routes, capi...)
	for i := range all {
		r := all[i]
		if r.Provider == "meta_ads" {
			origLoad := r.LoadSecret
			r.LoadSecret = func(ctx context.Context, tx pgx.Tx, c integration.SecretClaim) (integration.Secret, error) {
				e.recordLoad(c)
				return origLoad(ctx, tx, c)
			}
		} else {
			origLoad, origCheck := r.LoadSecret, r.Check
			r.LoadSecret = func(ctx context.Context, tx pgx.Tx, c integration.SecretClaim) (integration.Secret, error) {
				e.recordLoad(c)
				return origLoad(ctx, tx, c)
			}
			r.Check = func(ctx context.Context, req integration.DispatchRequest) error {
				p := e.probe
				p.mu.Lock()
				p.checks = append(p.checks, adsCheckRec{Op: req.OperationID, Action: req.Action, Provider: req.Provider, Mode: req.Mode, GraphCalls: e.g.Count("")})
				p.inCheck[req.OperationID]++
				p.mu.Unlock()
				defer func() { p.mu.Lock(); p.inCheck[req.OperationID]--; p.mu.Unlock() }()
				return origCheck(ctx, req)
			}
		}
		all[i] = r
	}
	e.routes = all
	e.dispatchOpts = t06DispatchOptions()
	e.dispatchOpts.CallTimeout = 700 * time.Millisecond
	if e.opts.callTimeout > 0 {
		e.dispatchOpts.CallTimeout = e.opts.callTimeout
		if e.opts.callTimeout > 15*time.Second {
			e.dispatchOpts.LeaseSeconds = 90
		}
	}
	e.dispatchOpts.RetryDelay = 150 * time.Millisecond
	if e.opts.retryDelay > 0 {
		e.dispatchOpts.RetryDelay = e.opts.retryDelay
	}
	e.dispatchOpts.MaxGenerations = 6
	if e.opts.maxGen > 0 {
		e.dispatchOpts.MaxGenerations = e.opts.maxGen
	}
	worker, err := integration.NewDispatcher(e.ctx, e.workerPool, all, e.dispatchOpts)
	if err != nil {
		t.Fatalf("NewDispatcher: %v", err)
	}
	workers := river.NewWorkers()
	river.AddWorker(workers, worker)
	if err := ads.AddWorkers(workers, e.workerPool); err != nil {
		t.Fatal(err)
	}
	if err := attribution.AddWorkers(workers, e.workerPool); err != nil {
		t.Fatal(err)
	}
	// a previous test may have left queue ads paused (River persists it): the owner clears it
	_, _ = e.f.owner.Exec(e.ctx, `UPDATE river.river_queue SET paused_at=NULL WHERE name IN ('ads','ads_sweep')`)
	// Neutralize jobs a previous test left on the shared queue: their operations belong to another store and fake Graph.
	e.cancelLeftoverJobs()
	e.client, err = river.NewClient(riverpgxv5.New(e.workerPool), &river.Config{Schema: "river", Workers: workers,
		Queues: e.queues(), JobTimeout: e.dispatchOpts.CallTimeout + 20*time.Second, RescueStuckJobsAfter: e.dispatchOpts.CallTimeout + 30*time.Second,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err != nil {
		t.Fatal(err)
	}
	if e.sweepIns, err = river.NewClient[pgx.Tx](riverpgxv5.New(e.f.owner), &river.Config{Schema: "river"}); err != nil {
		t.Fatal(err)
	}
	if err := e.client.Start(e.ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
		defer cancel()
		if err := e.client.StopAndCancel(ctx); err != nil {
			t.Error("ads River client did not stop")
		}
	})
}

func (e *adsEnv) queues() map[string]river.QueueConfig {
	q := map[string]river.QueueConfig{"ads_sweep": {MaxWorkers: 2}}
	if !e.opts.noDispatch {
		q["ads"] = river.QueueConfig{MaxWorkers: 4}
	}
	return q
}

// startDispatch adds a second River client that works queue ads only (a parent started with noDispatch has none), sharing the same
// routes and options; River rescues jobs a killed process left running.
func (e *adsEnv) startDispatch() {
	t := e.t
	t.Helper()
	worker, err := integration.NewDispatcher(e.ctx, e.workerPool, e.routes, e.dispatchOpts)
	if err != nil {
		t.Fatal(err)
	}
	workers := river.NewWorkers()
	river.AddWorker(workers, worker)
	client, err := river.NewClient(riverpgxv5.New(e.workerPool), &river.Config{Schema: "river", Workers: workers, Queues: map[string]river.QueueConfig{"ads": {MaxWorkers: 4}},
		JobTimeout: 20 * time.Second, RescueStuckJobsAfter: 30 * time.Second, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err != nil {
		t.Fatal(err)
	}
	if err := client.Start(e.ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
		defer cancel()
		_ = client.StopAndCancel(ctx)
	})
}

func (e *adsEnv) recordLoad(c integration.SecretClaim) {
	if e.onLoad != nil {
		e.onLoad(c)
	}
	rec := adsLoadRec{Claim: c, Op: c.OperationID}
	_ = e.f.owner.QueryRow(e.ctx, `SELECT state,lease_mode FROM integration.operations WHERE id=$1`, c.OperationID).Scan(&rec.OpState, &rec.Lease)
	e.probe.mu.Lock()
	rec.InCheck = e.probe.inCheck[c.OperationID] > 0
	e.probe.loads = append(e.probe.loads, rec)
	e.probe.mu.Unlock()
}

// cancelLeftoverJobs cancels every not-yet-running ads-lane job (owner SQL: River state is not identity).
func (e *adsEnv) cancelLeftoverJobs() {
	if _, err := e.f.owner.Exec(e.ctx, `UPDATE river.river_job SET state='cancelled',finalized_at=now() WHERE queue='ads' AND state IN ('available','scheduled','retryable')`); err != nil {
		e.t.Logf("cancel leftover ads jobs: %v", err)
	}
}

// retire removes this env's store from every global ads sweeper (advance_candidates / insights_candidates / CAPI sweeper):
// drafts move into the past, unfinished operations end FAILED_FINAL, jobs are cancelled, CAPI is switched off. Owner SQL,
// triggers off (fixture superuser); never touches another store.
func (e *adsEnv) retire() {
	ctx := context.Background()
	tx, err := e.f.owner.Begin(ctx)
	if err != nil {
		e.t.Logf("retire: %v", err)
		return
	}
	defer tx.Rollback(ctx)
	for _, q := range []string{
		`SET LOCAL session_replication_role=replica`,
		`UPDATE river.river_job SET state='cancelled',finalized_at=now() WHERE queue='ads' AND state IN ('available','scheduled','retryable','running')
		   AND args->>'operation_id' IN (SELECT id::text FROM integration.operations WHERE store_id=$1)`,
		`UPDATE integration.operations SET state='FAILED_FINAL',generation=greatest(generation,1),lease_mode='',lease_until=NULL,lease_token_hash=NULL,result_code='test_retired'
		   WHERE store_id=$1 AND provider IN ('meta_ads','meta_dataset') AND state IN ('READY','DISPATCHING','UNKNOWN','ACKNOWLEDGED')`,
		// pin unpinned remote ids (set-once NULL->value) and take the drafts out of every global sweeper: candidates need publish_attempt>0
		`UPDATE ads.remote_objects r SET remote_id=o.provider_reference FROM integration.operations o WHERE o.id=r.operation_id AND r.store_id=$1 AND r.remote_id IS NULL AND r.kind IN ('campaign','adset','creative','ad') AND o.state='SUCCEEDED' AND o.provider_reference ~ '^[0-9]{1,40}$'`,
		`UPDATE ads.campaign_drafts SET starts_at=starts_at-interval '200 days',ends_at=ends_at-interval '200 days',publish_attempt=0 WHERE store_id=$1`,
		`UPDATE ads.store_settings SET capi_enabled=false,capi_dataset_binding=NULL,capi_enabled_by=NULL WHERE store_id=$1`,
	} {
		var err error
		if strings.Contains(q, "$1") {
			_, err = tx.Exec(ctx, q, e.store)
		} else {
			_, err = tx.Exec(ctx, q)
		}
		if err != nil {
			e.t.Logf("retire (%.50s): %v", q, err)
			return
		}
	}
	_ = tx.Commit(ctx)
}

// ---- operator + owner fixtures ------------------------------------------------------------------------------------

// setSettings is the operator CLI definer (D7), executed by the registrar login exactly as cmd/meta-admin does.
func (e *adsEnv) setSettings(environment, sandboxAccount string, allowance int64, currency string) {
	e.t.Helper()
	var sb any
	if sandboxAccount != "" {
		sb = sandboxAccount
	}
	if _, err := e.registrarPool.Exec(e.ctx, `SELECT ads.operator_set_settings($1::uuid,$2::uuid,$3,$4,$5::bigint,$6)`, e.tenant, e.store, environment, sb, allowance, currency); err != nil {
		e.t.Fatalf("operator_set_settings: %v", err)
	}
}

// principal creates a member of the store with exactly these permissions (A-1: nothing is granted automatically).
func (e *adsEnv) principal(label string, perms ...string) string {
	e.t.Helper()
	if tok, ok := e.principals[label]; ok {
		return tok
	}
	_, tok := lcPrincipal(e.t, e.f, e.tenant, []string{e.store}, append([]string{"store:read"}, perms...)...)
	e.principals[label] = tok
	return tok
}

// principalID returns the principal id behind a merchant token (owner read-back of identity.sessions).
func (e *adsEnv) principalID(token string) string {
	var id string
	if err := e.f.owner.QueryRow(e.ctx, `SELECT principal_id::text FROM identity.sessions WHERE token_hash=$1`, tokenHash(token)).Scan(&id); err != nil {
		e.t.Fatalf("principal of token: %v", err)
	}
	return id
}

func (e *adsEnv) revoke(principal, permission string) {
	e.t.Helper()
	mustExec(e.t, e.f.owner, `DELETE FROM identity.store_grants WHERE tenant_id=$1 AND store_id=$2 AND principal_id=$3 AND permission=$4`, e.tenant, e.store, principal, permission)
}

func (e *adsEnv) grant(principal string, permissions ...string) {
	e.t.Helper()
	mustExec(e.t, e.f.owner, `INSERT INTO identity.store_grants(tenant_id,store_id,principal_id,permission) SELECT $1,$2,$3,p FROM unnest($4::text[]) p ON CONFLICT DO NOTHING`, e.tenant, e.store, principal, permissions)
}

// ---- HTTP ----------------------------------------------------------------------------------------------------------

type adsResp struct {
	Status int
	Hdr    http.Header
	Raw    []byte
	JSON   map[string]any
}

// code is the error code the merchant would see: the platform envelope's `code`, or the frozen brief's `error` key.
func (r adsResp) code() string {
	if s, ok := r.JSON["code"].(string); ok {
		return s
	}
	s, _ := r.JSON["error"].(string)
	return s
}
func (r adsResp) str(k string) string {
	s, _ := r.JSON[k].(string)
	return s
}

func (e *adsEnv) api(method, path, token string, hdr map[string]string, body any) adsResp {
	e.t.Helper()
	return e.apiOn(e.store, method, path, token, hdr, body)
}

func (e *adsEnv) apiOn(store, method, path, token string, hdr map[string]string, body any) adsResp {
	e.t.Helper()
	var rd io.Reader
	switch b := body.(type) {
	case nil:
	case string:
		rd = strings.NewReader(b)
	case []byte:
		rd = bytes.NewReader(b)
	default:
		raw, err := json.Marshal(b)
		if err != nil {
			e.t.Fatal(err)
		}
		rd = bytes.NewReader(raw)
	}
	req := httptest.NewRequest(method, "/v1/admin/stores/"+store+"/ads"+path, rd)
	if rd != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	e.handler.ServeHTTP(w, req)
	out := adsResp{Status: w.Code, Hdr: w.Header(), Raw: w.Body.Bytes()}
	_ = json.Unmarshal(out.Raw, &out.JSON)
	return out
}

func adsKey() map[string]string { return map[string]string{"Idempotency-Key": t04Key("ads")} }

// connect runs the real chain: POST connect -> (browser at the fake dialog) -> callback with a fresh code -> pick list ->
// bind the ad account (and the dataset when opts.dataset).
func (e *adsEnv) connect() {
	t := e.t
	t.Helper()
	c := e.api("POST", "/meta/connect", e.token, adsKey(), nil)
	if c.Status != 201 {
		t.Fatalf("connect: %d %s", c.Status, c.Raw)
	}
	stateID := c.str("state_id")
	dialog, err := url.Parse(c.str("dialog_url"))
	if err != nil {
		t.Fatal(err)
	}
	state := dialog.Query().Get("state")
	code := "SYNTH-CODE-" + t04Tag() + t04Tag()
	e.g.AddCode(code, e.botToken)
	cb := e.api("GET", "/meta/callback?code="+url.QueryEscape(code)+"&state="+url.QueryEscape(state), e.token, nil, nil)
	if cb.Status != 200 || cb.str("state_id") != stateID {
		t.Fatalf("callback: %d %s", cb.Status, cb.Raw)
	}
	body := map[string]any{"state_id": stateID, "ad_account_id": e.account}
	if e.opts.dataset {
		body["dataset_id"] = e.pixel
	}
	b := e.api("POST", "/meta/bindings", e.token, adsKey(), body)
	if b.Status != 201 {
		t.Fatalf("bindings: %d %s", b.Status, b.Raw)
	}
	e.adBinding = b.str("ad_binding_id")
	e.dsBinding = b.str("dataset_binding_id")
}

// ---- drafts ---------------------------------------------------------------------------------------------------------

type adsDraftIn struct {
	Budget   int64
	Currency string
	Start    time.Time
	End      time.Time
	Source   string
}

func (e *adsEnv) draftBody(in adsDraftIn) map[string]any {
	if in.Budget == 0 {
		in.Budget = 300000
	}
	if in.Currency == "" {
		in.Currency = "TWD"
	}
	if in.Start.IsZero() {
		in.Start = time.Now().UTC().Add(2 * time.Hour)
	}
	if in.End.IsZero() {
		in.End = in.Start.Add(24 * time.Hour)
	}
	if in.Source == "" {
		in.Source = e.pageAsset + "_" + adsDigits(12)
	}
	return map[string]any{"ad_binding_id": e.adBinding, "identity_binding_id": e.idBinding, "template": "BOOST_POST", "source_ref": in.Source,
		"currency": in.Currency, "lifetime_budget_minor": in.Budget, "starts_at": in.Start.Format(time.RFC3339), "ends_at": in.End.Format(time.RFC3339),
		"countries": []string{"TW"}, "age_min": 18, "age_max": 65}
}

// newDraft creates a draft and returns its id.
func (e *adsEnv) newDraft(in adsDraftIn) string {
	e.t.Helper()
	r := e.api("POST", "/drafts", e.token, adsKey(), e.draftBody(in))
	if r.Status != 201 {
		e.t.Fatalf("create draft: %d %s", r.Status, r.Raw)
	}
	id := r.str("id")
	e.drafts = append(e.drafts, id)
	return id
}

func (e *adsEnv) draft(id string) adsResp {
	e.t.Helper()
	r := e.api("GET", "/drafts/"+id, e.token, nil, nil)
	if r.Status != 200 {
		e.t.Fatalf("get draft: %d %s", r.Status, r.Raw)
	}
	return r
}

func (e *adsEnv) approve(id string) adsResp {
	e.t.Helper()
	rev := int(e.draft(id).JSON["revision"].(float64))
	return e.api("POST", "/drafts/"+id+"/approve", e.token, nil, map[string]any{"revision": rev})
}

func (e *adsEnv) mustApprove(id string) {
	e.t.Helper()
	if r := e.approve(id); r.Status != 200 {
		e.t.Fatalf("approve: %d %s", r.Status, r.Raw)
	}
}

func (e *adsEnv) publish(id string) adsResp {
	e.t.Helper()
	attempt := int(e.draft(id).JSON["publish_attempt"].(float64))
	return e.api("POST", "/drafts/"+id+"/publish", e.token, adsKey(), map[string]any{"publish_attempt": attempt})
}

func (e *adsEnv) mustPublish(id string) {
	e.t.Helper()
	if r := e.publish(id); r.Status != 200 {
		e.t.Fatalf("publish: %d %s", r.Status, r.Raw)
	}
}

func (e *adsEnv) pause(id string) adsResp {
	e.t.Helper()
	return e.api("POST", "/drafts/"+id+"/pause", e.token, adsKey(), nil)
}

func (e *adsEnv) status(id string) string {
	s, _ := e.draft(id).JSON["status"].(string)
	return s
}

// ---- operations ------------------------------------------------------------------------------------------------------

type adsOp struct {
	ID, Kind, Action, State, Code, Ref, JobState, Queue string
	Seq, Attempt, Priority                              int
	Job                                                 int64
	Generation                                          int64
}

const adsOpSQL = `SELECT o.id::text,r.kind,o.action,o.state,o.result_code,o.provider_reference,coalesce(j.state::text,''),coalesce(j.queue,''),
	r.seq,r.publish_attempt,coalesce(j.priority,0),o.job_id,o.generation
	FROM ads.remote_objects r JOIN integration.operations o ON o.id=r.operation_id LEFT JOIN river.river_job j ON j.id=o.job_id
	WHERE r.draft_id=$1 ORDER BY r.publish_attempt,array_position(ARRAY['campaign','adset','creative','ad','preflight','activate','pause'],r.kind),r.seq`

func (e *adsEnv) ops(draft string) []adsOp {
	e.t.Helper()
	rows, err := e.f.owner.Query(e.ctx, adsOpSQL, draft)
	if err != nil {
		e.t.Fatalf("ops: %v", err)
	}
	defer rows.Close()
	var out []adsOp
	for rows.Next() {
		var o adsOp
		if err := rows.Scan(&o.ID, &o.Kind, &o.Action, &o.State, &o.Code, &o.Ref, &o.JobState, &o.Queue, &o.Seq, &o.Attempt, &o.Priority, &o.Job, &o.Generation); err != nil {
			e.t.Fatal(err)
		}
		out = append(out, o)
	}
	return out
}

// op returns the (last attempt, given seq) operation of a kind; ok=false when none.
func (e *adsEnv) op(draft, kind string, seq int) (adsOp, bool) {
	var found adsOp
	ok := false
	for _, o := range e.ops(draft) {
		if o.Kind == kind && o.Seq == seq {
			found, ok = o, true
		}
	}
	return found, ok
}

func (e *adsEnv) mustOp(draft, kind string, seq int) adsOp {
	e.t.Helper()
	o, ok := e.op(draft, kind, seq)
	if !ok {
		e.t.Fatalf("no %s seq %d op for draft %s (ops: %+v)", kind, seq, draft, e.ops(draft))
	}
	return o
}

func (e *adsEnv) opRow(id string) (state, code, ref, job string) {
	e.t.Helper()
	if err := e.f.owner.QueryRow(e.ctx, `SELECT o.state,o.result_code,o.provider_reference,coalesce(j.state::text,'') FROM integration.operations o LEFT JOIN river.river_job j ON j.id=o.job_id WHERE o.id=$1`, id).Scan(&state, &code, &ref, &job); err != nil {
		e.t.Fatalf("op row: %v", err)
	}
	return
}

// awaitOp polls persisted state (no race conclusion depends on the delay) until the op has state (any of), then returns code/ref.
func (e *adsEnv) awaitOp(id string, budget time.Duration, states ...string) (code, ref string) {
	e.t.Helper()
	deadline := time.Now().Add(budget)
	var s, c, r, j string
	for time.Now().Before(deadline) {
		s, c, r, j = e.opRow(id)
		for _, w := range states {
			if s == w {
				return c, r
			}
		}
		time.Sleep(25 * time.Millisecond)
	}
	e.t.Fatalf("op %s persisted %s/%s/job=%s, want one of %v", id, s, c, j, states)
	return
}

// hold keeps an operation's River job away from the dispatcher (scheduled far in the future); release makes it available.
func (e *adsEnv) hold(id string) {
	e.t.Helper()
	tag, err := e.f.owner.Exec(e.ctx, `UPDATE river.river_job SET state='scheduled',scheduled_at=now()+interval '2 hours' WHERE id=(SELECT job_id FROM integration.operations WHERE id=$1) AND state IN ('available','scheduled','retryable')`, id)
	if err != nil || tag.RowsAffected() != 1 {
		e.t.Fatalf("hold %s: %v rows=%d", id, err, tag.RowsAffected())
	}
}

func (e *adsEnv) release(id string) {
	e.t.Helper()
	tag, err := e.f.owner.Exec(e.ctx, `UPDATE river.river_job SET state='available',scheduled_at=now() WHERE id=(SELECT job_id FROM integration.operations WHERE id=$1) AND state='scheduled'`, id)
	if err != nil || tag.RowsAffected() != 1 {
		e.t.Fatalf("release %s: %v rows=%d", id, err, tag.RowsAffected())
	}
}

// holdAll holds every ops job of the store that is available right now and returns their ids.
func (e *adsEnv) holdNew(before map[string]bool) []string {
	e.t.Helper()
	rows, err := e.f.owner.Query(e.ctx, `SELECT id::text FROM integration.operations WHERE store_id=$1 AND provider IN ('meta_ads','meta_dataset') AND state='READY'`, e.store)
	if err != nil {
		e.t.Fatal(err)
	}
	var ids []string
	for rows.Next() {
		var id string
		_ = rows.Scan(&id)
		if !before[id] {
			ids = append(ids, id)
		}
	}
	rows.Close()
	for _, id := range ids {
		e.hold(id)
	}
	return ids
}

// gateCheck makes the next Check of `action` block (op stays DISPATCHING) until the returned release func is called.
func (e *adsEnv) gateCheck(action string) (entered <-chan struct{}, release func()) {
	p := e.probe
	gate, ent := make(chan struct{}), make(chan struct{})
	p.mu.Lock()
	p.gate[action], p.entered[action] = gate, ent
	p.mu.Unlock()
	var once sync.Once
	return ent, func() { once.Do(func() { close(gate) }) }
}

// sweep inserts one job of a periodic kind on queue ads (worker login, args {}, exactly what River's periodic scheduler
// inserts) and waits until the real worker finished it.
func (e *adsEnv) sweep(kind string) {
	e.t.Helper()
	full := map[string]string{"advance": "ads_publish_advance_v1", "insights": "ads_insights_plan_v1", "purge": "ads_oauth_purge_v1", "capi": "capi_purchase_sweep_v1"}[kind]
	if full == "" || e.client == nil {
		e.t.Fatalf("sweep(%q) without a worker", kind)
	}
	// Owner-inserted on queue ads_sweep (a fixture superuser passes the ads job guard): same worker code as the periodic
	// job, but pauseDispatch() can hold queue `ads` while sweepers still run.
	res, err := e.sweepIns.Insert(e.ctx, adsSweepArgs{kind: full}, &river.InsertOpts{Queue: "ads_sweep", Priority: 3})
	if err != nil {
		e.t.Fatalf("insert %s sweeper job: %v", full, err)
	}
	deadline := time.Now().Add(45 * time.Second)
	for time.Now().Before(deadline) {
		var st string
		if err := e.f.owner.QueryRow(e.ctx, `SELECT state::text FROM river.river_job WHERE id=$1`, res.Job.ID).Scan(&st); err != nil {
			e.t.Fatal(err)
		}
		switch st {
		case "completed":
			return
		case "discarded", "cancelled":
			e.t.Fatalf("%s sweeper job ended %s", full, st)
		}
		time.Sleep(25 * time.Millisecond)
	}
	e.t.Fatalf("%s sweeper job did not finish", full)
}

// pauseDispatch stops River from fetching queue `ads` (operations stay READY, sweepers still run); resumeDispatch undoes it.
// QueuePause only writes river_queue.paused_at and notifies: the producer stops fetching asynchronously, so both helpers
// wait for this client's own queue event before returning — otherwise a job planned right after the call can still be
// fetched (MA06 flaked in the full suite on exactly that window).
func (e *adsEnv) pauseDispatch() {
	e.t.Helper()
	e.setDispatch(river.EventKindQueuePaused, e.client.QueuePause)
	e.t.Cleanup(func() {
		_, _ = e.f.owner.Exec(context.Background(), `UPDATE river.river_queue SET paused_at=NULL WHERE name='ads'`)
	})
}

func (e *adsEnv) resumeDispatch() {
	e.t.Helper()
	e.setDispatch(river.EventKindQueueResumed, e.client.QueueResume)
}

func (e *adsEnv) setDispatch(kind river.EventKind, change func(context.Context, string, *river.QueuePauseOpts) error) {
	e.t.Helper()
	var already bool
	if err := e.f.owner.QueryRow(e.ctx, `SELECT COALESCE((SELECT paused_at IS NOT NULL FROM river.river_queue WHERE name='ads'),false)`).Scan(&already); err != nil {
		e.t.Fatal(err)
	}
	events, cancel := e.client.Subscribe(kind)
	defer cancel()
	if err := change(e.ctx, "ads", nil); err != nil {
		e.t.Fatalf("%s queue ads: %v", kind, err)
	}
	if already == (kind == river.EventKindQueuePaused) {
		return // no state change: the producer emits no event
	}
	timeout := time.After(30 * time.Second)
	for {
		select {
		case ev := <-events:
			if ev != nil && ev.Queue != nil && ev.Queue.Name == "ads" {
				return
			}
		case <-timeout:
			e.t.Fatalf("queue ads producer did not confirm %s", kind)
		}
	}
}

// settleErr waits until no operation of this store is READY/DISPATCHING with a runnable job (held jobs and UNKNOWN operations
// whose budget is exhausted do not count), i.e. until the dispatcher went idle. Error-returning so goroutines can use it.
func (e *adsEnv) settleErr() error {
	deadline := time.Now().Add(45 * time.Second)
	quiet := 0
	for time.Now().Before(deadline) {
		var n int
		if err := e.f.owner.QueryRow(e.ctx, `SELECT count(*) FROM integration.operations o JOIN river.river_job j ON j.id=o.job_id
			WHERE o.store_id=$1 AND o.provider IN ('meta_ads','meta_dataset') AND o.state IN ('READY','DISPATCHING','UNKNOWN','ACKNOWLEDGED')
			AND (j.state IN ('available','running','retryable') OR (j.state='scheduled' AND j.scheduled_at<now()+interval '1 minute'))`, e.store).Scan(&n); err != nil {
			return err
		}
		if n == 0 {
			quiet++
			if quiet >= 3 {
				return nil
			}
		} else {
			quiet = 0
		}
		time.Sleep(40 * time.Millisecond)
	}
	return fmt.Errorf("dispatcher did not go idle: %s", e.dumpSafe())
}

func (e *adsEnv) settle() {
	e.t.Helper()
	if err := e.settleErr(); err != nil {
		e.t.Fatal(err)
	}
}

func (e *adsEnv) dumpSafe() string { return "(see ops)" }

func (e *adsEnv) dump() string {
	var b strings.Builder
	for _, d := range e.drafts {
		for _, o := range e.ops(d) {
			fmt.Fprintf(&b, "\n  %s draft=%.8s %s#%d attempt=%d state=%s code=%s job=%s gen=%d", o.ID[:8], d, o.Kind, o.Seq, o.Attempt, o.State, o.Code, o.JobState, o.Generation)
		}
	}
	return b.String()
}

// drive alternates the advance sweeper and the dispatcher until done() holds (bounded).
func (e *adsEnv) drive(draft string, rounds int, done func() bool) bool {
	e.t.Helper()
	for i := 0; i < rounds; i++ {
		e.sweep("advance")
		e.settle()
		if done() {
			return true
		}
	}
	return false
}

// chainTo drives the publish chain until the named op kind exists in a SUCCEEDED state.
func (e *adsEnv) driveTo(draft, kind string, seq int) adsOp {
	e.t.Helper()
	ok := e.drive(draft, 14, func() bool { o, f := e.op(draft, kind, seq); return f && o.State == "SUCCEEDED" })
	if !ok {
		e.t.Fatalf("draft %s never reached %s#%d SUCCEEDED:%s", draft, kind, seq, e.dump())
	}
	return e.mustOp(draft, kind, seq)
}

// age moves an operation's timestamps (row + events) into the past (owner SQL, triggers off; disclosed per test).
func (e *adsEnv) age(op string, by time.Duration) {
	e.t.Helper()
	tx, err := e.f.owner.Begin(e.ctx)
	if err != nil {
		e.t.Fatal(err)
	}
	defer tx.Rollback(e.ctx)
	secs := fmt.Sprintf("%d seconds", int64(by.Seconds()))
	for _, q := range []string{
		`SET LOCAL session_replication_role=replica`,
		`UPDATE integration.operations SET created_at=created_at-$2::interval,updated_at=updated_at-$2::interval WHERE id=$1`,
		`UPDATE integration.operation_events SET created_at=created_at-$2::interval WHERE operation_id=$1`,
	} {
		if strings.Contains(q, "$1") {
			_, err = tx.Exec(e.ctx, q, op, secs)
		} else {
			_, err = tx.Exec(e.ctx, q)
		}
		if err != nil {
			e.t.Fatalf("age: %v", err)
		}
	}
	if err := tx.Commit(e.ctx); err != nil {
		e.t.Fatal(err)
	}
}

func (e *adsEnv) count(q string, args ...any) int64 { return miCount(e.t, e.f.owner, q, args...) }

// remote returns the campaign/adset/creative/ad remote ids of a draft (from the API view).
func (e *adsEnv) remote(draft string) map[string]string {
	e.t.Helper()
	out := map[string]string{}
	if m, ok := e.draft(draft).JSON["remote"].(map[string]any); ok {
		for k, v := range m {
			out[k], _ = v.(string)
		}
	}
	return out
}

// newInsertOnlyClient is a River client on the runtime pool that only inserts (queue validation tests).
func newInsertOnlyClient(f *testFixture) (*river.Client[pgx.Tx], error) {
	return river.NewClient[pgx.Tx](riverpgxv5.New(f.runtime), &river.Config{Schema: "river"})
}

// foreignOp inserts one synthetic NON-ads operation (provider mock_provider, action payment.authorize) with an initial event (owner
// fixture, disclosed) so "an ads role sees zero rows / a loader refuses it" assertions have something to refuse. Removed at cleanup.
func (e *adsEnv) foreignOp() string {
	e.t.Helper()
	op, binding := randomUUID(), randomUUID()
	mustExec(e.t, e.f.owner, `INSERT INTO integration.bindings(id,tenant_id,store_id,principal_id,provider,external_asset_id) VALUES($1,$2,$3,$4,'mock_provider',$5)`, binding, e.tenant, e.store, e.creator, "asset-"+t04Tag())
	mustExec(e.t, e.f.owner, `INSERT INTO integration.operations(id,tenant_id,store_id,principal_id,binding_id,binding_version,provider,external_asset_id,purpose,action,semantic_key,request_hash,request,job_id)
		SELECT $1,$2,$3,$4,b.id,b.semantic_version,'mock_provider',b.external_asset_id,'transactional','payment.authorize',$5,sha256('x'::bytea),'{}'::jsonb,1 FROM integration.bindings b WHERE b.id=$6`,
		op, e.tenant, e.store, e.creator, "foreign-"+t04Tag()+"-key", binding)
	mustExec(e.t, e.f.owner, `INSERT INTO integration.operation_events(tenant_id,store_id,operation_id,generation,state,mode,reason_code) VALUES($1,$2,$3,0,'READY','','operation_planned')`, e.tenant, e.store, op)
	e.t.Cleanup(func() {
		_ = e.ownerReplicaBestEffort(`DELETE FROM integration.operation_events WHERE operation_id=$1`, op)
		_ = e.ownerReplicaBestEffort(`DELETE FROM integration.operations WHERE id=$1`, op)
		_ = e.ownerReplicaBestEffort(`DELETE FROM integration.bindings WHERE id=$1`, binding)
	})
	return op
}
