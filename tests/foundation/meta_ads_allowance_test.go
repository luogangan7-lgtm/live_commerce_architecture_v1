package foundation_test

// MA02 allowance + billing halves (contracts/meta-ads-v1.md §9 row MA02 clauses on AD6 / §5.3 / §6.3 and BD5 / §5.2; ruling B9:
// billing_restricted is HTTP 402). Tier MOCK: real HTTP, real sweepers + dispatcher, real PG, fake Graph; the RESTRICTED store is
// built only through customers-billing's own definer (cbxRestrict / billing.apply_subscription as commerce_stripe_ingress).
// Disclosed owner-pool fixtures: aged draft times (replica mode), holding the store-settings row lock for the pg_blocking_pids
// witness, SET LOCAL ROLE probes. Written from the contract, not the implementation.
//
// Every "counts toward the allowance" assertion is made through the merchant's own approve call (§2 step 6: "check allowance under
// store lock, insert approval") with a second draft whose budget fits only if the first draft no longer counts: 409 over_allowance
// = counts, 200 = does not. Tables/functions touched: ads.* via HTTP + sweepers, billing.apply_subscription (ingress login).

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/riverqueue/river"

	"livecommerce/tests/ads/fakegraph"
)

// fitsOnlyIfReleased creates a draft whose budget fits the allowance only when the given draft budget no longer counts.
func (e *adsEnv) fitsOnlyIfReleased(held int64) string {
	return e.newDraft(adsDraftIn{Budget: e.opts.allowance - held + 100})
}

func TestMetaAdsMA02Allowance(t *testing.T) {
	t.Run("a draft that never had an activate does not count; an approved+published one waiting for preflight does not either", func(t *testing.T) {
		e := newAdsEnv(t, adsOpts{noWorker: true})
		a := e.newDraft(adsDraftIn{Budget: 900000})
		e.mustApprove(a)
		e.mustPublish(a)
		b := e.newDraft(adsDraftIn{Budget: 900000})
		if r := e.approve(b); r.Status != 200 {
			t.Fatalf("approve B while A has no activate op: %d %s (AD6: a campaign never activated is non-spending)", r.Status, r.Raw)
		}
	})

	t.Run("an activated draft counts; a second draft that does not fit is refused at approve with 409 over_allowance", func(t *testing.T) {
		e := newAdsEnv(t, adsOpts{})
		a := e.newDraft(adsDraftIn{Budget: 300000})
		e.mustApprove(a)
		e.mustPublish(a)
		e.driveTo(a, "activate", 1)
		b := e.fitsOnlyIfReleased(300000)
		if r := e.approve(b); r.Status != 409 {
			t.Fatalf("approve over the remaining allowance: %d %s", r.Status, r.Raw)
		}
		small := e.newDraft(adsDraftIn{Budget: e.opts.allowance - 300000})
		if r := e.approve(small); r.Status != 200 {
			t.Fatalf("a draft that exactly fits the remaining allowance was refused: %d %s (I05: minor units, same currency)", r.Status, r.Raw)
		}
	})

	t.Run("a denied activate (BLOCKED_POLICY, no spend) does not count", func(t *testing.T) {
		e := newAdsEnv(t, adsOpts{})
		a := e.readyToActivate(adsDraftIn{Budget: 300000})
		o := e.blockedActivate(a, func() { e.age(e.mustOp(a, "preflight", 1).ID, 11*time.Minute) })
		e.wantBlocked(o, "preflight_stale")
		b := e.fitsOnlyIfReleased(300000)
		if r := e.approve(b); r.Status != 200 {
			t.Fatalf("a BLOCKED_POLICY activate released nothing: approve %d %s", r.Status, r.Raw)
		}
	})

	t.Run("an UNKNOWN activate still counts (the effect may exist)", func(t *testing.T) {
		e := newAdsEnv(t, adsOpts{maxGen: 3})
		a := e.readyToActivate(adsDraftIn{Budget: 300000})
		e.g.Inject(fakegraph.Fault{Route: fakegraph.RouteStatusPost, Kind: fakegraph.FaultTimeout, Times: 20}) // no effect, no answer
		e.sweep("advance")
		e.settle()
		act := e.mustOp(a, "activate", 1)
		if act.State != "UNKNOWN" {
			t.Fatalf("activate %s/%s, want UNKNOWN", act.State, act.Code)
		}
		b := e.fitsOnlyIfReleased(300000)
		if r := e.approve(b); r.Status != 409 {
			t.Fatalf("UNKNOWN activate released the allowance: approve %d %s", r.Status, r.Raw)
		}
	})

	t.Run("ENDED with the pause UNKNOWN still counts, until ends_at + 1 day has passed", func(t *testing.T) {
		e := newAdsEnv(t, adsOpts{maxGen: 3})
		a := e.newDraft(adsDraftIn{Budget: 300000})
		e.mustApprove(a)
		e.mustPublish(a)
		e.driveTo(a, "activate", 1)
		e.g.Inject(fakegraph.Fault{Route: fakegraph.RouteStatusPost, Kind: fakegraph.FaultGraphError, Code: 4, HTTP: 429, Times: 60})
		if r := e.api("POST", "/drafts/"+a+"/end", e.token, adsKey(), nil); r.Status != 200 {
			t.Fatalf("end: %d %s", r.Status, r.Raw)
		}
		e.settle()
		if got := e.status(a); got != "ENDED" && got != "UNKNOWN" {
			t.Logf("status after end with an UNKNOWN pause: %s", got)
		}
		if p := e.mustOp(a, "pause", 1); p.State != "UNKNOWN" {
			t.Fatalf("pause %s/%s, want UNKNOWN", p.State, p.Code)
		}
		b := e.fitsOnlyIfReleased(300000)
		if r := e.approve(b); r.Status != 409 {
			t.Fatalf("ENDED draft with an unconfirmed pause released the allowance: %d %s", r.Status, r.Raw)
		}
		// now > ends_at + 1 day: the window is over (aged: owner SQL, replica mode)
		e.ownerReplica(`UPDATE ads.campaign_drafts SET starts_at=starts_at-interval '4 days',ends_at=ends_at-interval '4 days' WHERE id=$1`, a)
		if r := e.approve(b); r.Status != 200 {
			t.Fatalf("a draft past ends_at + 1 day still counts: approve %d %s", r.Status, r.Raw)
		}
	})

	t.Run("a merchant end of an ACTIVE draft plans a pause; once the pause is confirmed after the activate the draft stops counting", func(t *testing.T) {
		e := newAdsEnv(t, adsOpts{})
		a := e.newDraft(adsDraftIn{Budget: 300000})
		e.mustApprove(a)
		e.mustPublish(a)
		e.driveTo(a, "activate", 1)
		if r := e.api("POST", "/drafts/"+a+"/end", e.token, adsKey(), nil); r.Status != 200 {
			t.Fatalf("end: %d %s", r.Status, r.Raw)
		}
		e.settle()
		if p := e.mustOp(a, "pause", 1); p.State != "SUCCEEDED" {
			t.Fatalf("pause after end: %s/%s", p.State, p.Code)
		}
		b := e.fitsOnlyIfReleased(300000)
		if r := e.approve(b); r.Status != 200 {
			t.Fatalf("confirmed pause claimed after the activate did not release: %d %s", r.Status, r.Raw)
		}
		if got := e.status(a); got != "ENDED" {
			t.Errorf("status after end: %s", got)
		}
		// X7: an ended draft never re-activates
		for i := 0; i < 2; i++ {
			e.sweep("advance")
			e.settle()
		}
		if _, again := e.op(a, "activate", 2); again {
			t.Fatal("activate planned for an ended draft")
		}
	})

	t.Run("two ready drafts, room for one: exactly one activate is planned (approved_at order), the other never reaches Meta", func(t *testing.T) {
		e := newAdsEnv(t, adsOpts{allowance: 600000})
		first := e.newDraft(adsDraftIn{Budget: 300000})
		second := e.newDraft(adsDraftIn{Budget: 300000})
		e.mustApprove(second) // approved first: it owns the earlier approved_at
		time.Sleep(20 * time.Millisecond)
		e.mustApprove(first)
		e.setSettings("SANDBOX", e.account, 0, "TWD") // no room while the chains run: the sweeper cannot plan an activate yet
		e.mustPublish(first)
		e.mustPublish(second)
		for i := 0; i < 14; i++ {
			e.sweep("advance")
			e.settle()
			a, ok1 := e.op(first, "preflight", 1)
			b, ok2 := e.op(second, "preflight", 1)
			if ok1 && ok2 && a.State == "SUCCEEDED" && b.State == "SUCCEEDED" {
				break
			}
		}
		if _, planned := e.op(first, "activate", 1); planned {
			t.Fatal("activate planned with zero allowance")
		}
		if _, planned := e.op(second, "activate", 1); planned {
			t.Fatal("activate planned with zero allowance")
		}
		// operator grants room for exactly one of them; ONE sweeper run now sees both eligible
		e.setSettings("SANDBOX", e.account, 300000, "TWD")
		e.sweep("advance")
		e.settle()
		_, f1 := e.op(first, "activate", 1)
		_, f2 := e.op(second, "activate", 1)
		if f1 == f2 {
			t.Fatalf("activates planned: first=%v second=%v, want exactly one", f1, f2)
		}
		if !f2 {
			t.Fatal("the later-approved draft got the room (§6.3: drafts are processed in approved_at order)")
		}
		for i := 0; i < 3; i++ {
			e.sweep("advance")
			e.settle()
		}
		if _, planned := e.op(first, "activate", 1); planned {
			t.Fatal("the second draft's activate was planned once the first counts")
		}
		if n := e.g.Count(fakegraph.RouteStatusPost); n != 1 {
			t.Fatalf("%d status POSTs, want exactly one activation", n)
		}
		active := 0
		for _, c := range e.g.Objects("campaign") {
			if c.Status == "ACTIVE" {
				active++
			}
		}
		if active != 1 {
			t.Fatalf("%d ACTIVE campaigns for an allowance of one", active)
		}
	})

	t.Run("second activate is denied over_allowance while the first is DISPATCHING", func(t *testing.T) {
		e := newAdsEnv(t, adsOpts{allowance: 600000, callTimeout: 10 * time.Second})
		first := e.newDraft(adsDraftIn{Budget: 300000})
		second := e.newDraft(adsDraftIn{Budget: 300000})
		e.mustApprove(first)
		time.Sleep(20 * time.Millisecond)
		e.mustApprove(second)
		e.mustPublish(first)
		e.mustPublish(second)
		// creates + preflights of both, no activate yet: no room while they run
		e.setSettings("SANDBOX", e.account, 0, "TWD")
		for i := 0; i < 14; i++ {
			e.sweep("advance")
			e.settle()
			a, ok1 := e.op(first, "preflight", 1)
			b, ok2 := e.op(second, "preflight", 1)
			if ok1 && ok2 && a.State == "SUCCEEDED" && b.State == "SUCCEEDED" {
				break
			}
		}
		e.setSettings("SANDBOX", e.account, 600000, "TWD")
		e.pauseDispatch()
		e.sweep("advance") // room for both: both activates planned in one run, both READY
		a1, ok1 := e.op(first, "activate", 1)
		a2, ok2 := e.op(second, "activate", 1)
		if !ok1 || !ok2 {
			t.Fatalf("both activates should be planned with room for both: %v %v", ok1, ok2)
		}
		e.hold(a2.ID)
		hold, arrived := make(chan struct{}), make(chan struct{}, 1)
		e.g.Inject(fakegraph.Fault{Route: fakegraph.RouteStatusPost, Kind: fakegraph.FaultHold, Hold: hold, Arrived: arrived})
		e.resumeDispatch() // the first activate passes Check and its POST is now in flight at the fake
		select {
		case <-arrived:
		case <-time.After(20 * time.Second):
			t.Fatalf("first activate never in flight:%s", e.dump())
		}
		e.setSettings("SANDBOX", e.account, 300000, "TWD") // the operator lowers the allowance while the first is DISPATCHING
		e.release(a2.ID)
		e.awaitOp(a2.ID, 15*time.Second, "BLOCKED_POLICY")
		if o := e.mustOp(second, "activate", 1); o.Code != "over_allowance" {
			t.Fatalf("second activate blocked with %q, want over_allowance", o.Code)
		}
		if n := e.g.Count(fakegraph.RouteStatusPost); n != 1 {
			t.Fatalf("%d status POSTs while the first is in flight: the second reached Meta", n)
		}
		close(hold)
		e.awaitOp(a1.ID, 15*time.Second, "SUCCEEDED")
		e.settle()
		active := 0
		for _, c := range e.g.Objects("campaign") {
			if c.Status == "ACTIVE" {
				active++
			}
		}
		if active != 1 {
			t.Fatalf("%d ACTIVE campaigns after the race, want 1", active)
		}
		// liveness: the sweeper does not plan the second again while there is no room
		for i := 0; i < 2; i++ {
			e.sweep("advance")
			e.settle()
		}
		if _, again := e.op(second, "activate", 2); again {
			t.Fatal("second activate re-planned without room")
		}
	})

	t.Run("approve and the sweeper's activate planning serialize on the store row (pg_blocking_pids witness): never both counted over the allowance", func(t *testing.T) {
		e := newAdsEnv(t, adsOpts{allowance: 300000})
		a := e.readyToActivate(adsDraftIn{Budget: 300000})
		b := e.newDraft(adsDraftIn{Budget: 300000})
		tx, err := e.f.owner.Begin(e.ctx)
		if err != nil {
			t.Fatal(err)
		}
		var holder int
		if err := tx.QueryRow(e.ctx, `SELECT pg_backend_pid()`).Scan(&holder); err != nil {
			t.Fatal(err)
		}
		if _, err := tx.Exec(e.ctx, `SELECT 1 FROM ads.store_settings WHERE store_id=$1 FOR UPDATE`, e.store); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = tx.Rollback(e.ctx) }) // never strand the row lock (the store retirement needs it)
		waitBlocked := func(n int) {
			deadline := time.Now().Add(20 * time.Second)
			for {
				var blocked int
				if err := e.f.owner.QueryRow(e.ctx, `SELECT count(*) FROM pg_stat_activity a WHERE a.pid<>pg_backend_pid() AND a.wait_event_type='Lock' AND cardinality(pg_blocking_pids(a.pid))>0
					AND ($1=ANY(pg_blocking_pids(a.pid)) OR EXISTS (SELECT 1 FROM unnest(pg_blocking_pids(a.pid)) w WHERE $1=ANY(pg_blocking_pids(w))))`, holder).Scan(&blocked); err != nil {
					t.Fatal(err)
				}
				if blocked >= n {
					return
				}
				if time.Now().After(deadline) {
					t.Fatalf("expected %d backends waiting on the store row, saw %d", n, blocked)
				}
				time.Sleep(5 * time.Millisecond)
			}
		}
		// Merchant requests wait at most ~1 s and sweeper transactions 2 s for a lock (lock_timeout), so the two waits are staged:
		// the sweeper is provably waiting first, then the approve joins it (a second waiter queues behind the first waiter, transitively
		// behind the holder), and the row is released while both still wait.
		var wg sync.WaitGroup
		var approve adsResp
		var sweepErr error
		wg.Add(2)
		go func() { defer wg.Done(); sweepErr = e.sweepErr("advance") }()
		waitBlocked(1)
		go func() { defer wg.Done(); approve = e.approve(b) }()
		waitBlocked(2)
		if err := tx.Rollback(e.ctx); err != nil {
			t.Fatal(err)
		}
		wg.Wait()
		if sweepErr != nil {
			t.Fatal(sweepErr)
		}
		e.settle()
		for i := 0; i < 3; i++ {
			e.sweep("advance")
			e.settle()
		}
		_, aAct := e.op(a, "activate", 1)
		bOps := e.ops(b)
		bActivated := false
		for _, o := range bOps {
			bActivated = bActivated || (o.Kind == "activate" && o.State != "BLOCKED_POLICY")
		}
		if aAct && bActivated {
			t.Fatal("both drafts activated for an allowance of one (approve raced the planner)")
		}
		if approve.Status != 200 && !(approve.Status == 409 && aAct) {
			t.Fatalf("approve %d %s while A activate planned=%v", approve.Status, approve.Raw, aAct)
		}
		active := 0
		for _, c := range e.g.Objects("campaign") {
			if c.Status == "ACTIVE" {
				active++
			}
		}
		if active > 1 {
			t.Fatalf("%d ACTIVE campaigns", active)
		}
		t.Logf("EVIDENCE order: approve=%d, A activate planned=%v, B activated=%v, ACTIVE=%d", approve.Status, aAct, bActivated, active)
	})

	t.Run("mixed currency and non-whole TWD are refused before any row exists", func(t *testing.T) {
		e := newAdsEnv(t, adsOpts{noWorker: true})
		before := e.count(`SELECT count(*) FROM ads.campaign_drafts WHERE store_id=$1`, e.store)
		if r := e.api("POST", "/drafts", e.token, adsKey(), e.draftBody(adsDraftIn{Currency: "HKD", Budget: 5000})); r.Status != 422 {
			t.Errorf("HKD draft on a TWD store: %d %s", r.Status, r.Raw)
		}
		if r := e.api("POST", "/drafts", e.token, adsKey(), e.draftBody(adsDraftIn{Budget: 12345})); r.Status != 422 {
			t.Errorf("TWD 123.45: %d %s", r.Status, r.Raw)
		}
		if after := e.count(`SELECT count(*) FROM ads.campaign_drafts WHERE store_id=$1`, e.store); after != before {
			t.Errorf("%d rows created by refused drafts", after-before)
		}
	})
}

// sweepErr is sweep for goroutines (t.Fatal is not allowed there).
func (e *adsEnv) sweepErr(kind string) error {
	full := map[string]string{"advance": "ads_publish_advance_v1", "insights": "ads_insights_plan_v1", "purge": "ads_oauth_purge_v1", "capi": "capi_purchase_sweep_v1"}[kind]
	res, err := e.sweepIns.Insert(e.ctx, adsSweepArgs{kind: full}, &river.InsertOpts{Queue: "ads_sweep", Priority: 3})
	if err != nil {
		return err
	}
	deadline := time.Now().Add(45 * time.Second)
	for time.Now().Before(deadline) {
		var st string
		if err := e.f.owner.QueryRow(e.ctx, `SELECT state::text FROM river.river_job WHERE id=$1`, res.Job.ID).Scan(&st); err != nil {
			return err
		}
		switch st {
		case "completed":
			return nil
		case "discarded", "cancelled":
			return fmt.Errorf("%s sweeper ended %s", full, st)
		}
		time.Sleep(25 * time.Millisecond)
	}
	return fmt.Errorf("%s sweeper did not finish", full)
}

// ---------------------------------------------------------------------------------------------------------------------

// asWriterStanding reads billing.store_standing as commerce_ads_writer (SET LOCAL ROLE from the fixture superuser).
func (e *adsEnv) standingAsWriter() string {
	e.t.Helper()
	tx, err := e.f.owner.Begin(e.ctx)
	if err != nil {
		e.t.Fatal(err)
	}
	defer tx.Rollback(e.ctx)
	if _, err := tx.Exec(e.ctx, `SET LOCAL ROLE commerce_ads_writer`); err != nil {
		e.t.Fatal(err)
	}
	var s string
	if err := tx.QueryRow(e.ctx, `SELECT billing.store_standing($1,$2)`, e.tenant, e.store).Scan(&s); err != nil {
		e.t.Fatalf("billing.store_standing as commerce_ads_writer: %v", err)
	}
	return s
}

func TestMetaAdsMA02Billing(t *testing.T) {
	t.Run("standing is read correctly AS commerce_ads_writer: RESTRICTED is not fail-open UNBILLED", func(t *testing.T) {
		e := newAdsEnv(t, adsOpts{noWorker: true})
		if got := e.standingAsWriter(); got != "UNBILLED" {
			t.Fatalf("fresh store standing %s, want UNBILLED (pilot store, Q4)", got)
		}
		ingress := mustStripeIngress(t, e.f)
		customer := cbxPin(t, e.f, e.tenant, e.store)
		for status, want := range map[string]string{"active": "GOOD", "past_due": "GRACE"} {
			s := cbxSubFor(customer, e.store, status)
			if res, err := cbxApply(ingress, s); err != nil || res != "applied" {
				t.Fatalf("apply %s: %q %v", status, res, err)
			}
			if got := e.standingAsWriter(); got != want {
				t.Fatalf("standing after %s subscription: %s, want %s", status, got, want)
			}
			// GOOD / GRACE / UNBILLED pass: approve works
			d := e.newDraft(adsDraftIn{})
			if r := e.approve(d); r.Status != 200 {
				t.Errorf("approve with standing %s: %d %s", want, r.Status, r.Raw)
			}
			// a canceled subscription replaces the standing for the next round
			if _, err := e.f.owner.Exec(e.ctx, `DELETE FROM billing.subscriptions WHERE store_id=$1`, e.store); err != nil {
				t.Fatalf("reset subscriptions: %v", err)
			}
		}
		if _, err := cbxApply(ingress, cbxSubFor(customer, e.store, "canceled")); err != nil {
			t.Fatal(err)
		}
		if got := e.standingAsWriter(); got != "RESTRICTED" {
			t.Fatalf("standing after canceled subscription read as commerce_ads_writer: %s, want RESTRICTED (a policy-less read would yield UNBILLED = fail open)", got)
		}
	})

	t.Run("RESTRICTED: approve and publish 402 billing_restricted", func(t *testing.T) {
		e := newAdsEnv(t, adsOpts{noWorker: true})
		draftA := e.newDraft(adsDraftIn{})
		e.mustApprove(draftA) // approved while UNBILLED
		draftB := e.newDraft(adsDraftIn{})
		cbxRestrict(t, e.f, mustStripeIngress(t, e.f), e.tenant, e.store)
		if r := e.approve(draftB); r.Status != 402 {
			t.Fatalf("approve on a RESTRICTED store: %d %s", r.Status, r.Raw)
		}
		attempt := int(e.draft(draftA).JSON["publish_attempt"].(float64))
		r := e.api("POST", "/drafts/"+draftA+"/publish", e.token, adsKey(), map[string]any{"publish_attempt": attempt})
		if r.Status != 402 {
			t.Fatalf("publish on a RESTRICTED store: %d %s", r.Status, r.Raw)
		}
		if len(e.ops(draftA)) != 0 {
			t.Fatal("ops planned by a refused publish")
		}
	})

	t.Run("RESTRICTED: create and activate are BLOCKED_POLICY billing_restricted with zero HTTP", func(t *testing.T) {
		e := newAdsEnv(t, adsOpts{})
		ingress := mustStripeIngress(t, e.f)
		// create: restrict between publish and the create's Check
		d := e.newDraft(adsDraftIn{})
		e.mustApprove(d)
		entered, release := e.gateCheck("meta.ads.create_campaign")
		defer release()
		mark := e.g.Mark()
		e.mustPublish(d)
		<-entered
		cbxRestrict(t, e.f, ingress, e.tenant, e.store)
		release()
		e.settle()
		e.wantBlocked(e.mustOp(d, "campaign", 1), "billing_restricted")
		if n := len(e.g.RequestsSince(mark)); n != 0 {
			t.Fatalf("%d Graph requests for a denied create", n)
		}
	})
	t.Run("RESTRICTED: activate", func(t *testing.T) {
		e := newAdsEnv(t, adsOpts{})
		d := e.readyToActivate(adsDraftIn{})
		posts := e.g.Count(fakegraph.RouteStatusPost)
		o := e.blockedActivate(d, func() { cbxRestrict(t, e.f, mustStripeIngress(t, e.f), e.tenant, e.store) })
		e.wantBlocked(o, "billing_restricted")
		if e.g.Count(fakegraph.RouteStatusPost) != posts {
			t.Fatal("a status POST reached Meta for a RESTRICTED activate")
		}
	})

	t.Run("RESTRICTED: pause and insights reads are never blocked", func(t *testing.T) {
		e := newAdsEnv(t, adsOpts{})
		d, camp := e.activeDraft(300000)
		cbxRestrict(t, e.f, mustStripeIngress(t, e.f), e.tenant, e.store)
		e.g.SetInsights(camp, taipeiDay(0), fakegraph.Insights{Spend: "3.00", Impressions: "3", Clicks: "1"})
		e.sweep("insights")
		e.settle()
		reads := e.reads(d)
		if len(reads) == 0 {
			t.Fatal("no reads planned for a RESTRICTED store")
		}
		for _, r := range reads {
			if r.State != "SUCCEEDED" {
				t.Errorf("read %s %s/%s blocked by billing (BD5: only growth actions)", r.Day, r.State, r.Code)
			}
		}
		if r := e.pause(d); r.Status != 200 {
			t.Fatalf("pause on a RESTRICTED store: %d %s", r.Status, r.Raw)
		}
		e.settle()
		if p := e.mustOp(d, "pause", 1); p.State != "SUCCEEDED" {
			t.Fatalf("pause %s/%s on a RESTRICTED store", p.State, p.Code)
		}
		if o, _ := e.g.Object(camp); o.Status != "PAUSED" {
			t.Fatalf("remote %s", o.Status)
		}
		e.sweep("advance") // ingestion is never blocked either
		if strings.TrimSpace(fmt.Sprint(len(e.daily(d)))) == "0" {
			t.Error("insights not ingested for a RESTRICTED store")
		}
	})
}

var _ = context.Background
