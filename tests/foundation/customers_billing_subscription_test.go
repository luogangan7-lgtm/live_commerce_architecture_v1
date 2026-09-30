package foundation_test

// CB06 (contracts/customers-billing-v1.md §0 BD1-BD5, §3.2 function rows, §4, §8; tier REAL_PG with the real
// logins: the claim window is opened AS the real commerce_runtime login, apply_subscription is called as a
// commerce_stripe_ingress member, standing is read as a commerce_auth member). Written from the contract and the
// FROZEN block of billing-core.md only. Helper prefix `cbb`. What it proves:
//   - apply_subscription: unknown customer, metadata mismatch and NULL metadata (no write), stale and equal
//     retrieved_at, future retrieved_at and livemode -> PT400, canceled terminal, a second non-terminal
//     subscription -> `duplicate` but mirrored, incomplete_expired never a duplicate;
//   - refresh_subscription: another store's customer -> PT404, billing:manage required, same rules as apply;
//   - the runtime login has no EXECUTE on apply_subscription / store_standing; commerce_auth has store_standing;
//   - standing matrix (BD4): every status alone, canceled + incomplete -> RESTRICTED, incomplete alone ->
//     UNBILLED, best remaining status wins, no row -> UNBILLED; identity.read_billing_standing / read_billing;
//   - claim windows opened as the real runtime login are refused under RESTRICTED with PT412 (never a permission
//     error, also for a brand-new window), allowed under GOOD / GRACE / UNBILLED; an already-open window stays
//     open and keeps accepting claims; closing is never blocked; a new paid subscription lifts the restriction;
//   - platform_account_conflict is true for a registered PSP account id (any environment) and malformed ids;
//     pin_customer refuses a conflicting account, is set-once per (store, env) and audited;
//   - record_checkout_session keeps one open session per store.
// Disclosed owner-pool fixtures: extra stores of the tenant, cbxPin rows for matrix stores (the real
// pin_customer path is exercised on the main store), a registered stripe merchant account row (the registrar is
// not run), aged open_checkout_expires_at.

import (
	"context"
	"crypto/sha256"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"livecommerce/internal/billing"
	"livecommerce/internal/claims"
	"livecommerce/internal/platform"
)

type cbbEnv struct {
	t     *testing.T
	p     psHarness
	f     *testFixture
	ctx   context.Context
	store string
	mgr   string // billing:manage on the store
	plain string // store:read only
}

func (c *cbbEnv) rt(token string, fn func(tx pgx.Tx, hash []byte) error) error {
	h := sha256.Sum256([]byte(token))
	return platform.WithScope(c.ctx, c.f.runtime, token, c.store, "store:read", func(tx pgx.Tx, _ platform.Scope) error { return fn(tx, h[:]) })
}

func cbbSetup(t *testing.T) *cbbEnv {
	t.Helper()
	p := psSetup(t)
	f := p.f
	c := &cbbEnv{t: t, p: p, f: f, ctx: context.Background(), store: f.storeA1}
	_, c.mgr = lcPrincipal(t, f, f.tenantA, []string{f.storeA1}, "store:read", "billing:manage")
	_, c.plain = lcPrincipal(t, f, f.tenantA, []string{f.storeA1}, "store:read")
	return c
}

func (c *cbbEnv) standing(tenant, store string) string {
	c.t.Helper()
	auth := cbxLogin(c.t, c.f, "commerce_auth")
	var s string
	if err := auth.QueryRow(c.ctx, `SELECT billing.store_standing($1,$2)`, tenant, store).Scan(&s); err != nil {
		c.t.Fatalf("store_standing: %v", err)
	}
	return s
}

func (c *cbbEnv) rows(store string) int {
	return countRows(c.t, c.f.owner, `SELECT count(*) FROM billing.subscriptions WHERE store_id=$1`, store)
}

func TestCustomersBillingCB06Subscription(t *testing.T) {
	c := cbbSetup(t)
	f, ctx := c.f, c.ctx
	ing := cbxLogin(t, f, "commerce_stripe_ingress")
	auth := cbxLogin(t, f, "commerce_auth")
	store, tenant := c.store, f.tenantA

	t.Run("platform_account_conflict: registered PSP ids (any environment) and malformed ids", func(t *testing.T) {
		other := cbxStore(t, f, tenant)
		cbxRegisterPSP(t, f, tenant, store, f.principalA, "SANDBOX", "acct_PspOne000001")
		cbxRegisterPSP(t, f, tenant, other, f.principalA, "LIVE", "acct_PspLive000001")
		for _, login := range []struct {
			name string
			q    func(string) (bool, error)
		}{
			{"commerce_runtime", func(a string) (b bool, err error) {
				err = f.runtime.QueryRow(ctx, `SELECT billing.platform_account_conflict($1)`, a).Scan(&b)
				return
			}},
			{"commerce_stripe_ingress", func(a string) (b bool, err error) {
				err = ing.QueryRow(ctx, `SELECT billing.platform_account_conflict($1)`, a).Scan(&b)
				return
			}},
		} {
			for id, want := range map[string]bool{
				"acct_PspOne000001":     true,  // registered, SANDBOX
				"acct_PspLive000001":    true,  // registered, LIVE (any environment)
				"acct_Platform0001":     false, // well-formed, not registered
				"acct_Unregistered99":   false,
				"":                      true, // malformed
				"bad":                   true,
				"acct_":                 true,
				"ACCT_x1":               true,
				"cus_notanaccount":      true,
				strings.Repeat("a", 90): true,
			} {
				got, err := login.q(id)
				if err != nil || got != want {
					t.Errorf("%s: platform_account_conflict(%q) = %v %v, want %v", login.name, id, got, err, want)
				}
			}
		}
	})

	t.Run("pin_customer: conflict refused, set-once, audited, gated", func(t *testing.T) {
		pin := func(token, env, customer, account string) error {
			return c.rt(token, func(tx pgx.Tx, h []byte) error {
				var out any
				return tx.QueryRow(ctx, `SELECT billing.pin_customer($1,$2,$3,$4,$5)`, h, store, env, customer, account).Scan(&out)
			})
		}
		if err := pin(c.mgr, "SANDBOX", "cus_Cb06Pin0001", "acct_PspOne000001"); cbcCode(err) != "PT409" || !strings.Contains(err.Error(), "billing_account_conflict") {
			t.Fatalf("pinning the PSP account as the platform account: %v, want PT409 billing_account_conflict", err)
		}
		if n := countRows(t, f.owner, `SELECT count(*) FROM billing.store_customers WHERE store_id=$1`, store); n != 0 {
			t.Fatalf("a refused pin left %d rows", n)
		}
		if err := pin(c.plain, "SANDBOX", "cus_Cb06Pin0001", "acct_Platform0001"); cbcCode(err) != "PT403" {
			t.Errorf("without billing:manage: %v, want PT403", err)
		}
		if err := pin(c.mgr, "LIVE", "cus_Cb06Pin0001", "acct_Platform0001"); err == nil {
			t.Error("a LIVE pin was accepted (BD8: SANDBOX only)")
		}
		for name, args := range map[string][2]string{"malformed customer": {"customer_1", "acct_Platform0001"}, "malformed account": {"cus_Cb06Pin0001", "platform"}} {
			if err := pin(c.mgr, "SANDBOX", args[0], args[1]); err == nil {
				t.Errorf("%s was accepted", name)
			}
		}
		audits := countRows(t, f.owner, `SELECT count(*) FROM ops.audit_events WHERE store_id=$1 AND action='billing.customer_pinned'`, store)
		if err := pin(c.mgr, "SANDBOX", "cus_Cb06Pin0001", "acct_Platform0001"); err != nil {
			t.Fatalf("pin: %v", err)
		}
		if got := countRows(t, f.owner, `SELECT count(*) FROM ops.audit_events WHERE store_id=$1 AND action='billing.customer_pinned'`, store); got != audits+1 {
			t.Errorf("billing.customer_pinned audit rows %d -> %d, want +1", audits, got)
		}
		if err := pin(c.mgr, "SANDBOX", "cus_Cb06Pin0001", "acct_Platform0001"); err != nil {
			t.Errorf("the same customer id replays: %v", err)
		}
		if err := pin(c.mgr, "SANDBOX", "cus_Cb06Pin0002", "acct_Platform0001"); cbcCode(err) != "PT409" {
			t.Errorf("a different customer id for the same (store, env): %v, want PT409 (set-once)", err)
		}
		if n := countRows(t, f.owner, `SELECT count(*) FROM billing.store_customers WHERE store_id=$1`, store); n != 1 {
			t.Errorf("%d pinned customers", n)
		}
	})

	customer := cbxOne(t, f, `SELECT stripe_customer_id FROM billing.store_customers WHERE store_id=$1`, store)
	other := cbxStore(t, f, tenant)
	otherCustomer := cbxPin(t, f, tenant, other)

	t.Run("apply_subscription: guards, stale, duplicate, canceled terminal", func(t *testing.T) {
		if got := c.standing(tenant, store); got != "UNBILLED" {
			t.Fatalf("a pinned customer without subscriptions is %s, want UNBILLED (Q4)", got)
		}
		unknown := cbxSubFor("cus_Unknown0001", store, "active")
		if res, err := cbxApply(ing, unknown); err != nil || res != "unknown_customer" || c.rows(store) != 0 {
			t.Errorf("unknown customer: %q %v rows=%d", res, err, c.rows(store))
		}
		mismatch := cbxSubFor(customer, other, "active") // metadata names another store
		if res, err := cbxApply(ing, mismatch); err != nil || res != "mismatch" || c.rows(store) != 0 {
			t.Errorf("metadata of another store: %q %v rows=%d", res, err, c.rows(store))
		}
		nometa := cbxSubFor(customer, store, "active")
		nometa.MetaStore = nil
		if res, err := cbxApply(ing, nometa); err != nil || res != "mismatch" || c.rows(store) != 0 {
			t.Errorf("NULL metadata (Dashboard-created): %q %v rows=%d", res, err, c.rows(store))
		}
		live := cbxSubFor(customer, store, "active")
		live.Livemode = true
		if _, err := cbxApply(ing, live); cbcCode(err) != "PT400" {
			t.Errorf("livemode: %v, want PT400 (BD8)", err)
		}
		future := cbxSubFor(customer, store, "active")
		future.Retrieved = time.Now().Add(time.Hour)
		if _, err := cbxApply(ing, future); cbcCode(err) != "PT400" {
			t.Errorf("retrieved_at in the future: %v, want PT400", err)
		}
		if c.rows(store) != 0 {
			t.Fatal("a refused call wrote a row")
		}
		first := cbxSubFor(customer, store, "trialing")
		first.Retrieved = time.Now().Add(-time.Minute).UTC()
		if res, err := cbxApply(ing, first); err != nil || res != "applied" {
			t.Fatalf("first apply: %q %v", res, err)
		}
		var status, price string
		var ps, pe *time.Time
		var cancel bool
		if err := f.owner.QueryRow(ctx, `SELECT status,price_id,current_period_start,current_period_end,cancel_at_period_end FROM billing.subscriptions WHERE stripe_subscription_id=$1`, first.ID).Scan(&status, &price, &ps, &pe, &cancel); err != nil ||
			status != "trialing" || price != "price_Test0001" || ps == nil || pe == nil || !pe.After(*ps) || cancel {
			t.Fatalf("mirrored row: %s %s %v %v %v %v", status, price, ps, pe, cancel, err)
		}
		if got := c.standing(tenant, store); got != "GOOD" {
			t.Fatalf("trialing: %s, want GOOD", got)
		}
		older := first
		older.Status, older.Retrieved = "canceled", first.Retrieved.Add(-time.Second)
		if res, err := cbxApply(ing, older); err != nil || res != "stale" {
			t.Errorf("older retrieved_at: %q %v, want stale", res, err)
		}
		same := first
		same.Status = "past_due"
		if res, err := cbxApply(ing, same); err != nil || res != "stale" {
			t.Errorf("equal retrieved_at: %q %v, want stale (only a strictly newer retrieval updates)", res, err)
		}
		if cbxOne(t, f, `SELECT status FROM billing.subscriptions WHERE stripe_subscription_id=$1`, first.ID) != "trialing" {
			t.Error("a stale apply changed the row")
		}
		newer := first
		newer.Status, newer.Retrieved, newer.CancelEnd = "active", time.Now().UTC(), true
		if res, err := cbxApply(ing, newer); err != nil || res != "applied" {
			t.Fatalf("newer: %q %v", res, err)
		}
		if cbxOne(t, f, `SELECT status||':'||cancel_at_period_end::text FROM billing.subscriptions WHERE stripe_subscription_id=$1`, first.ID) != "active:true" {
			t.Error("the newer retrieval did not update status and cancel_at_period_end")
		}
		// a second non-terminal subscription is mirrored and reported as duplicate, never auto-cancelled
		dup := cbxSubFor(customer, store, "active")
		if res, err := cbxApply(ing, dup); err != nil || res != "duplicate" {
			t.Errorf("second non-terminal subscription: %q %v, want duplicate", res, err)
		}
		if c.rows(store) != 2 || cbxOne(t, f, `SELECT status FROM billing.subscriptions WHERE stripe_subscription_id=$1`, first.ID) != "active" {
			t.Errorf("duplicate handling: rows=%d, the first subscription must stay untouched", c.rows(store))
		}
		// an incomplete_expired (terminal) one next to a live one is not a duplicate
		expired := cbxSubFor(customer, store, "incomplete_expired")
		if res, err := cbxApply(ing, expired); err != nil || res != "applied" {
			t.Errorf("incomplete_expired next to an active one: %q %v, want applied", res, err)
		}
		// canceled is terminal: a later, newer retrieval cannot resurrect it
		cancelled := cbxSubFor(customer, store, "canceled")
		cancelled.Retrieved = time.Now().Add(time.Second).UTC()
		if _, err := cbxApply(ing, cancelled); err != nil {
			t.Fatalf("cancel: %v", err)
		}
		revive := cancelled
		revive.Status, revive.Retrieved = "active", cancelled.Retrieved.Add(500*time.Millisecond)
		_, _ = cbxApply(ing, revive) // the outcome (error or no-op) is the definer's choice; the state is the contract's
		if got := cbxOne(t, f, `SELECT status FROM billing.subscriptions WHERE stripe_subscription_id=$1`, cancelled.ID); got != "canceled" {
			t.Errorf("a canceled subscription came back as %s (F-B1: terminal)", got)
		}
		// set-once identity: the same subscription id cannot move to another store's customer
		moved := cbxSubFor(otherCustomer, other, "active")
		moved.ID = first.ID
		moved.Retrieved = time.Now().Add(time.Second).UTC()
		res, err := cbxApply(ing, moved)
		if err == nil && res == "applied" {
			t.Error("a subscription id was re-applied under another store's customer")
		}
		if cbxOne(t, f, `SELECT store_id::text FROM billing.subscriptions WHERE stripe_subscription_id=$1`, first.ID) != store {
			t.Error("a subscription moved store")
		}
	})

	t.Run("refresh_subscription: store-scoped, billing:manage, same rules as apply", func(t *testing.T) {
		refresh := func(token string, s cbxSub, forStore string) (res string, err error) {
			return res, platform.WithScope(ctx, f.runtime, token, store, "store:read", func(tx pgx.Tx, _ platform.Scope) error {
				h := sha256.Sum256([]byte(token))
				return tx.QueryRow(ctx, `SELECT billing.refresh_subscription($1,$2::uuid,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13::uuid,$14)`,
					h[:], forStore, s.Env, s.Customer, s.ID, s.Status, s.Price, s.PS, s.PE, s.CancelEnd, s.Created, s.Retrieved, s.MetaStore, s.Livemode).Scan(&res)
			})
		}
		mine := cbxSubFor(customer, store, "past_due")
		mine.Retrieved = time.Now().Add(2 * time.Second).UTC()
		if _, err := refresh(c.plain, mine, store); cbcCode(err) != "PT403" {
			t.Errorf("without billing:manage: %v, want PT403", err)
		}
		foreign := cbxSubFor(otherCustomer, other, "active")
		if _, err := refresh(c.mgr, foreign, store); cbcCode(err) != "PT404" {
			t.Errorf("another store's customer: %v, want PT404", err)
		}
		if cbxOne(t, f, `SELECT count(*)::text FROM billing.subscriptions WHERE store_id=$1`, other) != "0" {
			t.Error("a refused refresh wrote a row")
		}
		mm := cbxSubFor(customer, other, "active")
		if res, err := refresh(c.mgr, mm, store); err != nil || res != "mismatch" {
			t.Errorf("metadata mismatch: %q %v", res, err)
		}
		if res, err := refresh(c.mgr, mine, store); err != nil || (res != "applied" && res != "duplicate") {
			t.Errorf("own customer: %q %v", res, err)
		}
		live := cbxSubFor(customer, store, "active")
		live.Livemode = true
		if _, err := refresh(c.mgr, live, store); cbcCode(err) != "PT400" {
			t.Errorf("livemode via refresh: %v, want PT400", err)
		}
		// the runtime login cannot apply directly (ingress-only, C-4) nor read the standing function
		s := cbxSubFor(customer, store, "active")
		if _, err := cbxApply(f.runtime, s); cbcCode(err) != "42501" {
			t.Errorf("commerce_runtime calling apply_subscription: %v, want 42501", err)
		}
		var st string
		if err := f.runtime.QueryRow(ctx, `SELECT billing.store_standing($1,$2)`, tenant, store).Scan(&st); cbcCode(err) != "42501" {
			t.Errorf("commerce_runtime calling store_standing: %v, want 42501", err)
		}
		if _, err := cbxApply(auth, s); cbcCode(err) != "42501" {
			t.Errorf("commerce_auth calling apply_subscription: %v, want 42501 (ingress only)", err)
		}
	})

	t.Run("standing matrix (BD4) through the real apply path", func(t *testing.T) {
		type step struct{ status string }
		cases := []struct {
			name     string
			statuses []string
			want     billing.Standing
		}{
			{"trialing", []string{"trialing"}, billing.Good}, {"active", []string{"active"}, billing.Good},
			{"past_due", []string{"past_due"}, billing.Grace},
			{"unpaid", []string{"unpaid"}, billing.Restricted}, {"canceled", []string{"canceled"}, billing.Restricted}, {"paused", []string{"paused"}, billing.Restricted},
			{"incomplete alone", []string{"incomplete"}, billing.Unbilled}, {"incomplete_expired alone", []string{"incomplete_expired"}, billing.Unbilled},
			{"canceled + incomplete (the 23 h lift)", []string{"canceled", "incomplete"}, billing.Restricted},
			{"incomplete + canceled (other order)", []string{"incomplete", "canceled"}, billing.Restricted},
			{"unpaid + incomplete", []string{"unpaid", "incomplete"}, billing.Restricted},
			{"incomplete + incomplete_expired", []string{"incomplete", "incomplete_expired"}, billing.Unbilled},
			{"canceled + active", []string{"canceled", "active"}, billing.Good},
			{"past_due + unpaid", []string{"past_due", "unpaid"}, billing.Grace},
			{"trialing + past_due", []string{"trialing", "past_due"}, billing.Good},
			{"unpaid + canceled + paused", []string{"unpaid", "canceled", "paused"}, billing.Restricted},
			{"incomplete + past_due", []string{"incomplete", "past_due"}, billing.Grace},
		}
		var ids []string
		for _, tc := range cases {
			st := cbxStore(t, f, tenant)
			cus := cbxPin(t, f, tenant, st)
			for _, status := range tc.statuses {
				_, _ = cbxApply(ing, cbxSubFor(cus, st, status)) // 'duplicate' is a valid outcome that still mirrors
			}
			if got := c.standing(tenant, st); got != string(tc.want) {
				t.Errorf("%s: store_standing = %s, want %s", tc.name, got, tc.want)
			}
			// the merchant-facing reads agree (any member for the banner)
			ids = append(ids, st)
			tok := lcTokenFor(t, f, tenant, st)
			var banner string
			if err := platform.WithScope(ctx, f.runtime, tok, st, "store:read", func(tx pgx.Tx, _ platform.Scope) error {
				h := sha256.Sum256([]byte(tok))
				return tx.QueryRow(ctx, `SELECT identity.read_billing_standing($1,$2)`, h[:], st).Scan(&banner)
			}); err != nil || banner != string(tc.want) {
				t.Errorf("%s: read_billing_standing = %q %v, want %s", tc.name, banner, err, tc.want)
			}
		}
		if got := c.standing(tenant, cbxStore(t, f, tenant)); got != "UNBILLED" {
			t.Errorf("a store without any billing row: %s, want UNBILLED", got)
		}
		_ = ids
	})

	t.Run("read_billing: billing:manage, the mirrored rows and BD6 usage", func(t *testing.T) {
		var raw string
		read := func(token string) error {
			return c.rt(token, func(tx pgx.Tx, h []byte) error {
				return tx.QueryRow(ctx, `SELECT identity.read_billing($1,$2)::text`, h, store).Scan(&raw)
			})
		}
		if err := read(c.plain); cbcCode(err) != "PT403" {
			t.Errorf("read_billing without billing:manage: %v, want PT403", err)
		}
		if err := read(c.mgr); err != nil {
			t.Fatalf("read_billing: %v", err)
		}
		for _, k := range []string{"standing", "subscriptions", "status", "price_id", "retrieved_at", "cancel_at_period_end", "paid_orders", "claim_windows_opened", "private_replies_sent", "members"} {
			if !strings.Contains(raw, `"`+k+`"`) {
				t.Errorf("read_billing lacks %q: %s", k, raw)
			}
		}
		// note: the definer returns the pinned customer id to the runtime (the refresh needs it); the HTTP projection
		// hides it and CB09 scans every response for Stripe ids
	})

	t.Run("record_checkout_session: one open session per store", func(t *testing.T) {
		record := func(token, session string, expires time.Time) (res *string, err error) {
			err = c.rt(token, func(tx pgx.Tx, h []byte) error {
				return tx.QueryRow(ctx, `SELECT billing.record_checkout_session($1,$2,'SANDBOX',$3,$4)`, h, store, session, expires).Scan(&res)
			})
			return
		}
		if _, err := record(c.plain, "cs_test_a1", time.Now().Add(30*time.Minute)); cbcCode(err) != "PT403" {
			t.Errorf("without billing:manage: %v, want PT403", err)
		}
		if _, err := record(c.mgr, "cs_test_far", time.Now().Add(2*time.Hour)); err == nil {
			t.Error("p_expires beyond now()+31 min was accepted")
		}
		if _, err := record(c.mgr, "notasession", time.Now().Add(30*time.Minute)); err == nil {
			t.Error("a malformed session id was accepted")
		}
		if res, err := record(c.mgr, "cs_test_first", time.Now().Add(30*time.Minute)); err != nil || res != nil {
			t.Fatalf("first session: %v %v, want NULL", res, err)
		}
		res, err := record(c.mgr, "cs_test_second", time.Now().Add(30*time.Minute))
		if err != nil || res == nil || *res != "cs_test_first" {
			t.Fatalf("second session must return the older unexpired one for the caller to expire: %v %v", res, err)
		}
		if got := cbxOne(t, f, `SELECT open_checkout_session_id FROM billing.store_customers WHERE store_id=$1`, store); got != "cs_test_second" {
			t.Errorf("stored open session %q, want the newest", got)
		}
		mustExec(t, f.owner, `UPDATE billing.store_customers SET open_checkout_expires_at=now()-interval '1 minute' WHERE store_id=$1`, store)
		if res, err := record(c.mgr, "cs_test_third", time.Now().Add(30*time.Minute)); err != nil || res != nil {
			t.Errorf("after the stored session expired: %v %v, want NULL", res, err)
		}
		unpinned := cbxStore(t, f, tenant)
		tok := lcTokenFor(t, f, tenant, unpinned, "billing:manage")
		if err := platform.WithScope(ctx, f.runtime, tok, unpinned, "store:read", func(tx pgx.Tx, _ platform.Scope) error {
			h := sha256.Sum256([]byte(tok))
			var out *string
			return tx.QueryRow(ctx, `SELECT billing.record_checkout_session($1,$2,'SANDBOX','cs_test_x',$3)`, h[:], unpinned, time.Now().Add(30*time.Minute)).Scan(&out)
		}); err == nil {
			t.Error("a session was recorded for a store without a pinned customer")
		}
	})

	t.Run("claim windows opened as the real runtime login: refused only under RESTRICTED (BD5)", func(t *testing.T) {
		wp := psSetup(t) // a fresh store with stock, so its standing history is its own
		wf := wp.f
		ws, wt := wf.storeA1, wf.tenantA
		h := &lcHarness{cqHarness: wp.cqHarness, ctx: ctx}
		h.actor, h.token = lcPrincipal(t, wf, wt, []string{ws}, "store:read", "live:read", "live:manage")
		var err error
		if h.labels, err = claims.NewLabelKey(randomBytes(32)); err != nil {
			t.Fatal(err)
		}
		wcust := cbxPin(t, wf, wt, ws)
		wc := &cbbEnv{t: t, p: wp, f: wf, ctx: ctx, store: ws}
		open := func(session string) error {
			_, err := h.setWindow(h.token, ws, session, claims.WindowInput{ExpectedVersion: h.board(t, session).Window.Version, State: claims.WindowOpen, MatchMode: claims.MatchExact})
			return err
		}
		newSession := func() string {
			s := h.draft(t, ws)
			h.offer(t, s, "A1", wp.stock.skus[0].ID, 5)
			return s
		}
		s1 := newSession()
		if wc.standing(wt, ws) != "UNBILLED" {
			t.Fatalf("fixture standing %s", wc.standing(wt, ws))
		}
		if err := open(s1); err != nil { // UNBILLED
			t.Fatalf("UNBILLED must open: %v", err)
		}
		h.closeWindow(t, s1)
		sub := cbxSubFor(wcust, ws, "active")
		sub.Retrieved = time.Now().Add(-10 * time.Second).UTC()
		if res, err := cbxApply(ing, sub); err != nil || res != "applied" {
			t.Fatalf("active: %q %v", res, err)
		}
		if err := open(s1); err != nil {
			t.Fatalf("GOOD must open: %v", err)
		}
		h.closeWindow(t, s1)
		sub.Status, sub.Retrieved = "past_due", time.Now().Add(-5*time.Second).UTC()
		if res, err := cbxApply(ing, sub); err != nil || res != "applied" {
			t.Fatalf("past_due: %q %v", res, err)
		}
		if got := wc.standing(wt, ws); got != "GRACE" {
			t.Fatalf("standing %s, want GRACE", got)
		}
		if err := open(s1); err != nil {
			t.Fatalf("GRACE must open (banner only): %v", err)
		}
		// an already-open window survives the store becoming RESTRICTED and keeps accepting claims; closing is free
		sub.Status, sub.Retrieved = "canceled", time.Now().Add(-2*time.Second).UTC()
		if res, err := cbxApply(ing, sub); err != nil || res != "applied" {
			t.Fatalf("canceled: %q %v", res, err)
		}
		if got := wc.standing(wt, ws); got != "RESTRICTED" {
			t.Fatalf("standing %s, want RESTRICTED", got)
		}
		if w := h.board(t, s1).Window; w.State != claims.WindowOpen {
			t.Fatalf("an already-open window was closed by billing: %s", w.State)
		}
		if r := h.claim(t, s1, claims.ManualClaimInput{ActorLabel: "still-open", Text: "A1+1"}); r.Outcome != claims.OutcomeAccepted {
			t.Fatalf("a manual claim on the already-open window under RESTRICTED: %+v", r)
		}
		h.closeWindow(t, s1)
		if w := h.board(t, s1).Window; w.State != claims.WindowClosed {
			t.Fatalf("closing under RESTRICTED: %s", w.State)
		}
		// opening again is refused with PT412, mapped to the billing sentinel, never a permission error
		err = open(s1)
		if !errors.Is(err, claims.ErrBillingRestricted) || errors.Is(err, platform.ErrForbidden) {
			t.Fatalf("re-opening under RESTRICTED: %v, want claims.ErrBillingRestricted (PT412 -> 402 billing_restricted), not a permission error", err)
		}
		rawErr := platform.WithScope(ctx, wf.runtime, h.token, ws, "store:read", func(tx pgx.Tx, _ platform.Scope) error {
			_, e := tx.Exec(ctx, `UPDATE live.claim_windows SET state='OPEN' WHERE tenant_id=$1 AND store_id=$2 AND session_id=$3`, wt, ws, s1)
			return e
		})
		if cbcCode(rawErr) != "PT412" {
			t.Errorf("the raw UPDATE as the runtime login: %v, want SQLSTATE PT412 (not 42501, not a CHECK error)", rawErr)
		}
		// a brand-new session's first window (the INSERT path of the trigger) is refused too
		s2 := newSession()
		if err = open(s2); !errors.Is(err, claims.ErrBillingRestricted) {
			t.Fatalf("first OPEN of a new window under RESTRICTED: %v, want ErrBillingRestricted", err)
		}
		if w := h.board(t, s2).Window; w.State == claims.WindowOpen {
			t.Fatal("the refused window is open")
		}
		// a new paid subscription lifts it (canceled is terminal, the new row is added, no duplicate)
		next := cbxSubFor(wcust, ws, "active")
		if res, err := cbxApply(ing, next); err != nil || res != "applied" {
			t.Fatalf("new subscription after cancel: %q %v, want applied", res, err)
		}
		if err := open(s2); err != nil {
			t.Fatalf("after a new active subscription: %v", err)
		}
		h.closeWindow(t, s2)
		// incomplete never lifts: cancel again, add an incomplete one
		next.Status, next.Retrieved = "canceled", time.Now().Add(time.Second).UTC()
		if _, err := cbxApply(ing, next); err != nil {
			t.Fatal(err)
		}
		if res, err := cbxApply(ing, cbxSubFor(wcust, ws, "incomplete")); err != nil || res != "applied" {
			t.Fatalf("incomplete: %q %v", res, err)
		}
		if err := open(s2); !errors.Is(err, claims.ErrBillingRestricted) {
			t.Errorf("canceled + incomplete must still refuse: %v", err)
		}
	})
}

// lcTokenFor makes a merchant with store:read (plus perms) on one store of tenant and returns its bearer token.
func lcTokenFor(t *testing.T, f *testFixture, tenant, store string, perms ...string) string {
	t.Helper()
	_, token := lcPrincipal(t, f, tenant, []string{store}, append([]string{"store:read"}, perms...)...)
	return token
}
