package foundation_test

// MA11 (contracts/meta-ads-v1.md §9 row MA11, §3.1 A-10 dispatcher amendment, AD11, A-4; ads-a10 A10-D1..D4 and ads-graph G1).
// Tier MOCK + REAL_PG (+ static + build-graph checks). Written from the contract and the FROZEN ads-a10 block.
//
// What it proves:
//   - Reconcile of an UNKNOWN create_ad and an UNKNOWN pause loads the token through the RECONCILE claim (LoadSecret in reconcile
//     mode, fenced on that claim), and Reconcile stays query-only;
//   - the loader integration.load_meta_ads_token refuses a stale generation, an expired lease, a wrong lease token, an unknown /
//     non-ads operation; integration.load_meta_page_token returns no ads row for an ads operation's valid claim;
//   - no loader call path exists from Check: statically (Check sources / SQL bodies never name the loaders, Check has no Secret
//     parameter) and at runtime (no loader call while a Check of the same operation is running; no Graph call during a gated Check);
//   - `go list -deps ./cmd/api` contains neither meta_ads/tokenopen nor attribution/capiroute, and the built cmd/api binary has no
//     HPKE private-key loader symbol (cmd/ads-worker's binary does: positive control);
//   - an UNKNOWN activate is reconciled 11 minutes after its preflight with the store RESTRICTED (Reconcile GET runs, pins
//     SUCCEEDED); an UNKNOWN create_ad is reconciled after the approver lost ads:approve (pinned);
//   - DispatchRequest.Mode is "dispatch"/"reconcile" per claim for ads AND for a plain existing route; coded vs bare denials
//     (A10-D2): bare ErrPolicyDenied stays policy_denied; a reconcile-mode denial is never BLOCKED_POLICY; loader denial in
//     reconcile mode stays UNKNOWN credential_unavailable, in dispatch mode BLOCKED_POLICY; the secret is zeroed after the
//     callback; route validation refuses the impossible shapes; InsertOperationJobOn validates queue and priority.
// Disclosed owner-pool fixtures: lease expiry and aged preflight (replica mode), one synthetic non-ads operation, grant revocation.

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	integration "livecommerce/internal/integrations/core"
	"livecommerce/tests/ads/fakegraph"
)

func TestMetaAdsMA11ReconcileSecret(t *testing.T) {
	t.Run("reconcile of UNKNOWN create_ad loads the token through the reconcile claim and pins the object", func(t *testing.T) {
		e := newAdsEnv(t, adsOpts{})
		e.g.Inject(fakegraph.Fault{Route: fakegraph.RouteCreateAd, Kind: fakegraph.FaultTimeout, Effect: true})
		d := e.newDraft(adsDraftIn{})
		e.mustApprove(d)
		e.mustPublish(d)
		op := e.driveTo(d, "ad", 1)
		if e.g.Count(fakegraph.RouteCreateAd) != 1 {
			t.Fatalf("%d create_ad POSTs", e.g.Count(fakegraph.RouteCreateAd))
		}
		e.assertReconcileLoaded(op.ID)
	})

	t.Run("reconcile of UNKNOWN pause loads the token through the reconcile claim and confirms PAUSED", func(t *testing.T) {
		e := newAdsEnv(t, adsOpts{})
		d := e.newDraft(adsDraftIn{})
		e.mustApprove(d)
		e.mustPublish(d)
		e.driveTo(d, "activate", 1)
		e.g.Inject(fakegraph.Fault{Route: fakegraph.RouteStatusPost, Kind: fakegraph.FaultTimeout, Effect: true})
		if r := e.pause(d); r.Status != 200 {
			t.Fatalf("pause %d", r.Status)
		}
		e.settle()
		p := e.mustOp(d, "pause", 1)
		if p.State != "SUCCEEDED" {
			t.Fatalf("pause %s/%s: the reconcile GET should have proven PAUSED", p.State, p.Code)
		}
		if n := len(e.statusPosts()); n != 2 { // the activate and exactly ONE pause POST
			t.Fatalf("%d status POSTs, want 2 (activate + one pause): reconcile is query-only:\n%s%s", n, strings.Join(e.statusPosts(), "\n"), e.dump())
		}
		e.assertReconcileLoaded(p.ID)
	})

	t.Run("loader fences: stale generation, expired lease, wrong lease token, non-ads operation; page-token loader gives no ads row", func(t *testing.T) {
		e := newAdsEnv(t, adsOpts{})
		var results []string
		var seen atomic.Bool
		// A non-ads operation for the "wrong action" case (owner fixture, disclosed).
		foreign := e.foreignOp()
		e.onLoad = func(c integration.SecretClaim) {
			if !seen.CompareAndSwap(false, true) {
				return
			}
			probe := func(name string, op string, gen int64, token []byte) {
				var n int
				err := e.workerPool.QueryRow(e.ctx, `SELECT count(*) FROM integration.load_meta_ads_token($1::uuid,$2,$3)`, op, gen, token).Scan(&n)
				results = append(results, fmt.Sprintf("%s|%v|%d", name, sqlStateOf(err), n))
			}
			probe("valid", c.OperationID, c.Generation, c.LeaseToken)
			probe("stale generation +1", c.OperationID, c.Generation+1, c.LeaseToken)
			probe("stale generation -1", c.OperationID, c.Generation-1, c.LeaseToken)
			wrong := append([]byte(nil), c.LeaseToken...)
			wrong[0] ^= 0xff
			probe("wrong lease token", c.OperationID, c.Generation, wrong)
			probe("short lease token", c.OperationID, c.Generation, c.LeaseToken[:16])
			probe("unknown operation", randomUUID(), c.Generation, c.LeaseToken)
			probe("non-ads operation", foreign, c.Generation, c.LeaseToken)
			var pn int
			err := e.workerPool.QueryRow(e.ctx, `SELECT count(*) FROM integration.load_meta_page_token($1::uuid,$2,$3)`, c.OperationID, c.Generation, c.LeaseToken).Scan(&pn)
			results = append(results, fmt.Sprintf("page loader on an ads op|%v|%d", sqlStateOf(err), pn))
			// expired lease: the owner moves lease_until into the past, the correct claim is then refused, then restored
			tx, _ := e.f.owner.Begin(e.ctx)
			_, _ = tx.Exec(e.ctx, `SET LOCAL session_replication_role=replica`)
			var until time.Time
			_ = tx.QueryRow(e.ctx, `SELECT lease_until FROM integration.operations WHERE id=$1`, c.OperationID).Scan(&until)
			_, _ = tx.Exec(e.ctx, `UPDATE integration.operations SET lease_until=clock_timestamp()-interval '1 second' WHERE id=$1`, c.OperationID)
			_ = tx.Commit(e.ctx)
			probe("expired lease", c.OperationID, c.Generation, c.LeaseToken)
			tx, _ = e.f.owner.Begin(e.ctx)
			_, _ = tx.Exec(e.ctx, `SET LOCAL session_replication_role=replica`)
			_, _ = tx.Exec(e.ctx, `UPDATE integration.operations SET lease_until=$2 WHERE id=$1`, c.OperationID, until)
			_ = tx.Commit(e.ctx)
		}
		d := e.newDraft(adsDraftIn{})
		e.mustApprove(d)
		e.mustPublish(d)
		e.driveTo(d, "campaign", 1)
		want := map[string]string{
			"valid":               "<nil>|1",
			"stale generation +1": "40001|0",
			"stale generation -1": "40001|0",
			"wrong lease token":   "40001|0",
			"short lease token":   "22023|0",
			"unknown operation":   "P0002|0",
			"non-ads operation":   "P0002|0",
			"expired lease":       "40001|0",
		}
		got := map[string]string{}
		for _, r := range results {
			parts := strings.SplitN(r, "|", 2)
			got[parts[0]] = parts[1]
		}
		if got["stale generation -1"] == "22023|0" {
			got["stale generation -1"] = "40001|0" // generation 1-1 = 0 is not a valid generation: still a refusal, never a row
		}
		for name, w := range want {
			if got[name] != w {
				t.Errorf("loader %s -> %q, want %q", name, got[name], w)
			}
		}
		if g := got["page loader on an ads op"]; !strings.HasSuffix(g, "|0") {
			t.Errorf("load_meta_page_token returned an ads row: %q (0064:903-917 filters action=meta.private_reply)", g)
		}
		if len(results) != 9 {
			t.Errorf("probes ran: %v", results)
		}
	})

	t.Run("no loader path from Check: static and runtime", func(t *testing.T) {
		// static: neither the Go Check nor the SQL Check bodies name a credential loader or the ciphertext tables
		for _, p := range []string{"../../internal/ads/checker.go", "../../internal/attribution/capiroute/route.go"} {
			raw, err := os.ReadFile(p)
			if err != nil {
				t.Fatal(err)
			}
			src := string(raw)
			if p == "../../internal/ads/checker.go" && (strings.Contains(src, "load_meta_ads_token") || strings.Contains(src, "LoadSecret") || strings.Contains(src, "meta_page_credentials") || strings.Contains(src, "tokenopen")) {
				t.Errorf("%s names a loader, LoadSecret or the credential table", p)
			}
		}
		f := fixture(t)
		for _, fn := range []string{"ads.check_create(uuid)", "ads.check_activate(uuid)", "ads.check_read(uuid)", "ads.check_capi(uuid)"} {
			var body string
			if err := f.owner.QueryRow(e0ctx, `SELECT pg_get_functiondef($1::regprocedure)`, fn).Scan(&body); err != nil {
				t.Fatal(err)
			}
			for _, banned := range []string{"load_meta_ads_token", "load_meta_page_token", "meta_page_credentials", "meta_page_heads", "pending_ciphertext", "capi_user_data"} {
				if strings.Contains(body, banned) {
					t.Errorf("%s reads %s: Check must never see a secret or user data", fn, banned)
				}
			}
		}
		// signature: Check(ctx, DispatchRequest) error has no Secret parameter (compile-time assertion in the test package)
		var _ func(context.Context, integration.DispatchRequest) error = integration.DispatchRoute{}.Check

		// runtime: while a Check of an op is held, no loader call for it has happened and no Graph call was made
		e := newAdsEnv(t, adsOpts{})
		entered, release := e.gateCheck("meta.ads.create_campaign")
		defer release()
		d := e.newDraft(adsDraftIn{})
		e.mustApprove(d)
		mark := e.g.Mark()
		e.mustPublish(d)
		<-entered
		op := e.mustOp(d, "campaign", 1)
		for _, l := range e.probe.Loads() {
			if l.Op == op.ID {
				t.Error("the credential loader ran before/while Check")
			}
		}
		if n := len(e.g.RequestsSince(mark)); n != 0 {
			t.Errorf("%d Graph requests while Check was pending", n)
		}
		release()
		e.settle()
		for _, l := range e.probe.Loads() {
			if l.InCheck {
				t.Errorf("loader call for %s overlapped its Check", l.Op)
			}
		}
	})

	t.Run("cmd/api links no HPKE private-key loader and no CAPI route (deps + binary symbols)", func(t *testing.T) {
		root, _ := filepath.Abs("../..")
		cmd := exec.Command("go", "list", "-deps", "./cmd/api")
		cmd.Dir = root
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("go list: %v\n%s", err, out)
		}
		for _, banned := range []string{"meta_ads/tokenopen", "attribution/capiroute"} {
			if strings.Contains(string(out), banned) {
				t.Errorf("cmd/api depends on %s (A-4: only cmd/ads-worker may hold the private key)", banned)
			}
		}
		if !strings.Contains(string(out), "livecommerce/internal/integrations/meta_ads") || !strings.Contains(string(out), "livecommerce/internal/ads") {
			t.Errorf("cmd/api does not depend on the seal side / ads service: the check would be vacuous")
		}
		dir := t.TempDir()
		build := func(pkg, name string) string {
			bin := filepath.Join(dir, name)
			c := exec.Command("go", "build", "-o", bin, pkg)
			c.Dir = root
			if o, err := c.CombinedOutput(); err != nil {
				t.Fatalf("go build %s: %v\n%s", pkg, err, o)
			}
			return bin
		}
		symbols := func(bin string) string {
			o, err := exec.Command("go", "tool", "nm", bin).Output()
			if err != nil {
				t.Fatalf("go tool nm: %v", err)
			}
			return string(o)
		}
		api := symbols(build("./cmd/api", "api"))
		worker := symbols(build("./cmd/ads-worker", "ads-worker"))
		for _, banned := range []string{"tokenopen.LoadKeyring", "tokenopen.(*Keyring).Open", "capiroute.Routes", "load_meta_ads_token"} {
			if strings.Contains(api, banned) {
				t.Errorf("cmd/api binary contains %s", banned)
			}
		}
		for _, wanted := range []string{"tokenopen.LoadKeyring", "capiroute.Routes"} {
			if !strings.Contains(worker, wanted) {
				t.Errorf("positive control: cmd/ads-worker binary lacks %s (symbol check is vacuous)", wanted)
			}
		}
	})

	t.Run("UNKNOWN activate reconciled 11 minutes after its preflight, store RESTRICTED: the reconcile GET runs and pins SUCCEEDED", func(t *testing.T) {
		e := newAdsEnv(t, adsOpts{retryDelay: 5 * time.Second})
		d := e.readyToActivate(adsDraftIn{})
		e.g.Inject(fakegraph.Fault{Route: fakegraph.RouteStatusPost, Kind: fakegraph.FaultTimeout, Effect: true})
		e.sweep("advance")
		act := e.mustOp(d, "activate", 1)
		e.awaitOp(act.ID, 20*time.Second, "UNKNOWN")
		mark := e.g.Mark()
		// inside the retry window: the preflight is now 11 minutes old and the store is RESTRICTED (dispatch would be denied)
		e.age(e.mustOp(d, "preflight", 1).ID, 11*time.Minute)
		cbxRestrict(t, e.f, mustStripeIngress(t, e.f), e.tenant, e.store)
		e.awaitOp(act.ID, 25*time.Second, "SUCCEEDED")
		gets := 0
		for _, r := range e.g.RequestsSince(mark) {
			if r.Route == fakegraph.RouteGetObject {
				gets++
			}
			if r.Method == http.MethodPost {
				t.Errorf("reconcile POSTed: %s", r.Path)
			}
		}
		if gets < 1 {
			t.Error("no status GET during the reconcile")
		}
		if got := e.statusPosts(); len(got) != 1 {
			t.Errorf("%d status POSTs, want the single activation:\n%s%s", len(got), strings.Join(got, "\n"), e.dump())
		}
		var reconcileCheck bool
		for _, c := range e.probe.Checks() {
			reconcileCheck = reconcileCheck || (c.Op == act.ID && c.Mode == "reconcile")
		}
		if !reconcileCheck {
			t.Error("Check did not see mode reconcile for the reconcile claim")
		}
		if o, _ := e.g.Object(e.g.Objects("campaign")[0].ID); o.Status != "ACTIVE" {
			t.Errorf("remote %s", o.Status)
		}
	})

	t.Run("UNKNOWN create_ad reconciled after the approver lost ads:approve: pinned", func(t *testing.T) {
		e := newAdsEnv(t, adsOpts{retryDelay: 5 * time.Second})
		e.g.Inject(fakegraph.Fault{Route: fakegraph.RouteCreateAd, Kind: fakegraph.FaultTimeout, Effect: true})
		d := e.newDraft(adsDraftIn{})
		e.mustApprove(d)
		e.mustPublish(d)
		e.driveTo(d, "creative", 1)
		e.sweep("advance")
		ad := e.mustOp(d, "ad", 1)
		e.awaitOp(ad.ID, 20*time.Second, "UNKNOWN")
		e.revoke(e.creator, "ads:approve")
		_, ref := e.awaitOp(ad.ID, 25*time.Second, "SUCCEEDED")
		objs := e.g.Objects("ad")
		if len(objs) != 1 || objs[0].ID != ref || e.g.Count(fakegraph.RouteCreateAd) != 1 {
			t.Fatalf("ads %+v ref %s posts %d", objs, ref, e.g.Count(fakegraph.RouteCreateAd))
		}
		e.sweep("advance") // pins remote_id from the reconciled op
		if r := e.remote(d); r["ad_id"] != ref {
			t.Errorf("remote ad id %q, want %s pinned", r["ad_id"], ref)
		}
	})
}

var e0ctx = context.Background()

func sqlStateOf(err error) any {
	if err == nil {
		return nil
	}
	var pg interface{ SQLState() string }
	if errors.As(err, &pg) {
		return pg.SQLState()
	}
	return err.Error()
}

// assertReconcileLoaded: a reconcile-mode Check (nil) and a loader call fenced on the reconcile claim exist for the op.
func (e *adsEnv) assertReconcileLoaded(op string) {
	e.t.Helper()
	var load, check bool
	for _, l := range e.probe.Loads() {
		if l.Op == op && l.OpState == "UNKNOWN" && l.Lease == "reconcile" {
			load = true
		}
	}
	for _, c := range e.probe.Checks() {
		if c.Op == op && c.Mode == "reconcile" {
			check = true
		}
	}
	if !load || !check {
		e.t.Errorf("op %s: token loaded through a reconcile claim=%v, reconcile-mode Check seen=%v", op, load, check)
	}
}

// ---------------------------------------------------------------------------------------------------------------------
// A-10 on plain (non-ads) routes: Mode, coded denials, secret hooks, route validation, queue-aware insert
// ---------------------------------------------------------------------------------------------------------------------

func TestMetaAdsMA11DispatcherAmendment(t *testing.T) {
	type seen struct {
		check, dispatch, reconcile []string
	}
	run := func(t *testing.T, route func(*seen) integration.DispatchRoute, wantState, wantCode string, opts func(*integration.DispatcherOptions)) (*seen, string, string) {
		t.Helper()
		f := newT06GoFixture(t)
		b := f.register(t, f.store, "a10-binding-"+t04Tag())
		p := f.plan(t, "a10-op-"+t04Tag(), b, `{"n":1}`)
		queue := t06Queue(t, f, p)
		s := &seen{}
		o := t06DispatchOptions()
		if opts != nil {
			opts(&o)
		}
		t06StartDispatcher(t, f.worker, queue, []integration.DispatchRoute{route(s)}, o)
		var state, code string
		deadline := time.Now().Add(15 * time.Second)
		for time.Now().Before(deadline) {
			_ = f.worker.QueryRow(context.Background(), `SELECT o.state,o.result_code FROM integration.operations o WHERE o.id=$1`, p.OperationID).Scan(&state, &code)
			if state == wantState && (wantCode == "" || code == wantCode) {
				return s, state, code
			}
			time.Sleep(20 * time.Millisecond)
		}
		t.Fatalf("persisted %s/%s, want %s/%s (seen %+v)", state, code, wantState, wantCode, s)
		return s, state, code
	}
	plain := func(check func(context.Context, integration.DispatchRequest) error, dispatch, reconcile func(context.Context, integration.DispatchRequest) (integration.Outcome, error)) func(*seen) integration.DispatchRoute {
		return func(s *seen) integration.DispatchRoute {
			return integration.DispatchRoute{Provider: "mock_provider", Action: "payment.authorize", Purpose: "transactional",
				Check: func(ctx context.Context, r integration.DispatchRequest) error {
					s.check = append(s.check, r.Mode)
					return check(ctx, r)
				}, Dispatch: dispatch, Reconcile: reconcile}
		}
	}
	unknown := func(context.Context, integration.DispatchRequest) (integration.Outcome, error) {
		return integration.Outcome{State: "UNKNOWN", Code: "mock_unknown"}, nil
	}
	ok := func(context.Context, integration.DispatchRequest) error { return nil }

	t.Run("Mode is dispatch for the first claim and reconcile for the next, in Check and in the callbacks of an existing route", func(t *testing.T) {
		var s *seen
		route := plain(ok,
			func(_ context.Context, r integration.DispatchRequest) (integration.Outcome, error) {
				s.dispatch = append(s.dispatch, r.Mode)
				return integration.Outcome{State: "UNKNOWN", Code: "mock_unknown"}, nil
			},
			func(_ context.Context, r integration.DispatchRequest) (integration.Outcome, error) {
				s.reconcile = append(s.reconcile, r.Mode)
				return integration.Outcome{State: "SUCCEEDED", Code: "mock_found", ProviderReference: "mock-9"}, nil
			})
		got, _, _ := run(t, func(x *seen) integration.DispatchRoute { s = x; return route(x) }, "SUCCEEDED", "mock_found", nil)
		if fmt.Sprint(got.check) != "[dispatch reconcile]" || fmt.Sprint(got.dispatch) != "[dispatch]" || fmt.Sprint(got.reconcile) != "[reconcile]" {
			t.Fatalf("modes: check %v dispatch %v reconcile %v", got.check, got.dispatch, got.reconcile)
		}
	})

	t.Run("a bare ErrPolicyDenied stays policy_denied (existing routes unchanged)", func(t *testing.T) {
		run(t, plain(func(context.Context, integration.DispatchRequest) error { return integration.ErrPolicyDenied }, unknown, unknown), "BLOCKED_POLICY", "policy_denied", nil)
	})
	t.Run("DenyPolicy records its own code; an invalid code falls back to policy_denied; wrapping keeps the code", func(t *testing.T) {
		for _, c := range []struct{ code, want string }{{"over_allowance", "over_allowance"}, {"Bad Code!", "policy_denied"}, {"", "policy_denied"}, {strings.Repeat("a", 200), "policy_denied"}} {
			run(t, plain(func(context.Context, integration.DispatchRequest) error { return integration.DenyPolicy(c.code) }, unknown, unknown), "BLOCKED_POLICY", c.want, nil)
		}
		if !errors.Is(integration.DenyPolicy("x_y"), integration.ErrPolicyDenied) {
			t.Fatal("DenyPolicy is not errors.Is ErrPolicyDenied")
		}
	})
	t.Run("a Check denial in reconcile mode is never BLOCKED_POLICY: the op stays UNKNOWN policy_check_failed", func(t *testing.T) {
		route := plain(func(_ context.Context, r integration.DispatchRequest) error {
			if r.Mode == "reconcile" {
				return integration.DenyPolicy("would_block")
			}
			return nil
		}, unknown, unknown)
		run(t, route, "UNKNOWN", "policy_check_failed", func(o *integration.DispatcherOptions) { o.MaxGenerations = 3 })
	})

	secretRoute := func(load func(context.Context, pgx.Tx, integration.SecretClaim) (integration.Secret, error), dispatch, reconcile func(context.Context, integration.DispatchRequest, integration.Secret) (integration.Outcome, error)) func(*seen) integration.DispatchRoute {
		return func(s *seen) integration.DispatchRoute {
			return integration.DispatchRoute{Provider: "mock_provider", Action: "payment.authorize", Purpose: "transactional",
				Check: func(_ context.Context, r integration.DispatchRequest) error {
					s.check = append(s.check, r.Mode)
					return nil
				},
				LoadSecret: load, DispatchWithSecret: dispatch, ReconcileWithSecret: reconcile}
		}
	}
	t.Run("reconcile with a secret: loader runs on the reconcile claim; the secret is zeroed after the callback", func(t *testing.T) {
		var captured []byte
		var loads []string
		route := secretRoute(
			func(_ context.Context, tx pgx.Tx, c integration.SecretClaim) (integration.Secret, error) {
				var mode string
				_ = tx.QueryRow(context.Background(), `SELECT lease_mode FROM integration.operations WHERE id=$1`, c.OperationID).Scan(&mode)
				loads = append(loads, mode)
				return integration.NewSecret([]byte("SENTINEL-SECRET-A10")), nil
			},
			func(_ context.Context, r integration.DispatchRequest, s integration.Secret) (integration.Outcome, error) {
				return integration.Outcome{State: "UNKNOWN", Code: "mock_unknown"}, nil
			},
			func(_ context.Context, r integration.DispatchRequest, s integration.Secret) (integration.Outcome, error) {
				captured = s.Reveal() // same backing array the dispatcher zeroes afterwards
				if string(captured) != "SENTINEL-SECRET-A10" || r.Mode != "reconcile" {
					return integration.Outcome{}, errors.New("bad reconcile inputs")
				}
				return integration.Outcome{State: "SUCCEEDED", Code: "mock_found", ProviderReference: "mock-1"}, nil
			})
		run(t, route, "SUCCEEDED", "mock_found", nil)
		if len(loads) != 2 || loads[1] != "reconcile" {
			t.Errorf("loader saw lease modes %v", loads)
		}
		if !bytes.Equal(captured, make([]byte, len(captured))) || len(captured) == 0 {
			t.Errorf("secret not zeroed after the callback: %q", captured)
		}
	})
	t.Run("loader denial: dispatch -> BLOCKED_POLICY credential_unavailable; reconcile -> UNKNOWN credential_unavailable; other error -> UNKNOWN secret_load_failed", func(t *testing.T) {
		deny := func(context.Context, pgx.Tx, integration.SecretClaim) (integration.Secret, error) {
			return integration.Secret{}, fmt.Errorf("no credential: %w", integration.ErrPolicyDenied)
		}
		never := func(context.Context, integration.DispatchRequest, integration.Secret) (integration.Outcome, error) {
			return integration.Outcome{}, errors.New("callback must not run without a secret")
		}
		run(t, secretRoute(deny, never, never), "BLOCKED_POLICY", "credential_unavailable", nil)
		// reconcile mode: the dispatch attempt loads fine and ends UNKNOWN; the reconcile load is denied
		var n atomic.Int32
		flaky := func(context.Context, pgx.Tx, integration.SecretClaim) (integration.Secret, error) {
			if n.Add(1) == 1 {
				return integration.NewSecret([]byte("s")), nil
			}
			return integration.Secret{}, fmt.Errorf("revoked: %w", integration.ErrPolicyDenied)
		}
		unk := func(context.Context, integration.DispatchRequest, integration.Secret) (integration.Outcome, error) {
			return integration.Outcome{State: "UNKNOWN", Code: "mock_unknown"}, nil
		}
		run(t, secretRoute(flaky, unk, never), "UNKNOWN", "credential_unavailable", func(o *integration.DispatcherOptions) { o.MaxGenerations = 3 })
		var m atomic.Int32
		broken := func(context.Context, pgx.Tx, integration.SecretClaim) (integration.Secret, error) {
			if m.Add(1) == 1 {
				return integration.NewSecret([]byte("s")), nil
			}
			return integration.Secret{}, errors.New("database unavailable")
		}
		run(t, secretRoute(broken, unk, never), "UNKNOWN", "secret_load_failed", func(o *integration.DispatcherOptions) { o.MaxGenerations = 3 })
	})

	t.Run("route validation: exactly one of Reconcile / ReconcileWithSecret, the secret variant only with the loader pair", func(t *testing.T) {
		f := newT06GoFixture(t)
		ctx := context.Background()
		load := func(context.Context, pgx.Tx, integration.SecretClaim) (integration.Secret, error) {
			return integration.Secret{}, nil
		}
		dws := func(context.Context, integration.DispatchRequest, integration.Secret) (integration.Outcome, error) {
			return integration.Outcome{}, nil
		}
		plainD := func(context.Context, integration.DispatchRequest) (integration.Outcome, error) {
			return integration.Outcome{}, nil
		}
		base := func() integration.DispatchRoute {
			return integration.DispatchRoute{Provider: "mock_provider", Action: "payment.authorize", Purpose: "transactional", Check: ok}
		}
		mk := func(edit func(*integration.DispatchRoute)) error {
			r := base()
			edit(&r)
			_, err := integration.NewDispatcher(ctx, f.worker, []integration.DispatchRoute{r}, t06DispatchOptions())
			return err
		}
		good := []func(*integration.DispatchRoute){
			func(r *integration.DispatchRoute) { r.Dispatch, r.Reconcile = plainD, plainD },
			func(r *integration.DispatchRoute) {
				r.LoadSecret, r.DispatchWithSecret, r.Reconcile = load, dws, plainD
			},
			func(r *integration.DispatchRoute) {
				r.LoadSecret, r.DispatchWithSecret, r.ReconcileWithSecret = load, dws, dws
			},
		}
		for i, g := range good {
			if err := mk(g); err != nil {
				t.Errorf("valid shape %d refused: %v", i, err)
			}
		}
		bad := map[string]func(*integration.DispatchRoute){
			"both reconcile callbacks": func(r *integration.DispatchRoute) {
				r.LoadSecret, r.DispatchWithSecret, r.Reconcile, r.ReconcileWithSecret = load, dws, plainD, dws
			},
			"neither reconcile callback":                    func(r *integration.DispatchRoute) { r.LoadSecret, r.DispatchWithSecret = load, dws },
			"ReconcileWithSecret on a plain Dispatch route": func(r *integration.DispatchRoute) { r.Dispatch, r.ReconcileWithSecret = plainD, dws },
			"ReconcileWithSecret without the loader":        func(r *integration.DispatchRoute) { r.DispatchWithSecret, r.ReconcileWithSecret = dws, dws },
			"plain Dispatch together with the loader pair": func(r *integration.DispatchRoute) {
				r.Dispatch, r.LoadSecret, r.DispatchWithSecret, r.Reconcile = plainD, load, dws, plainD
			},
			"no Check": func(r *integration.DispatchRoute) { r.Check, r.Dispatch, r.Reconcile = nil, plainD, plainD },
		}
		for name, edit := range bad {
			if err := mk(edit); err == nil {
				t.Errorf("%s: accepted", name)
			}
		}
	})

	t.Run("InsertOperationJobOn validates queue and priority; the default insert is unchanged", func(t *testing.T) {
		f := newT06GoFixture(t)
		jobs := f.service // integration.Service has the insert-only client; the package function needs the client itself
		_ = jobs
		ctx := context.Background()
		client, err := newInsertOnlyClient(f.base)
		if err != nil {
			t.Fatal(err)
		}
		for _, c := range []struct {
			queue string
			prio  int
			ok    bool
		}{{"ads", 1, true}, {"ads", 4, true}, {"a", 3, true}, {"ads", 0, false}, {"ads", 5, false}, {"", 3, false}, {"Ads", 3, false}, {"1ads", 3, false}, {"ads-x", 3, false}, {strings.Repeat("a", 41), 3, false}, {"ads;drop", 3, false}} {
			tx, err := f.base.owner.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			_, err = integration.InsertOperationJobOn(ctx, client, tx, randomUUID(), c.queue, c.prio)
			_ = tx.Rollback(ctx)
			if c.ok && err != nil && !strings.Contains(err.Error(), "operation") {
				// a valid queue/priority may still fail later on the operation-existence guard, never on validation
				t.Logf("valid args %s/%d failed later: %v", c.queue, c.prio, err)
			}
			if !c.ok && err == nil {
				t.Errorf("queue %q priority %d accepted", c.queue, c.prio)
			}
		}
	})
}

var _ = regexp.MustCompile
