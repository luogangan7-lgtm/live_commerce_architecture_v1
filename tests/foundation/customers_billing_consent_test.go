package foundation_test

// CB04 (contracts/customers-billing-v1.md §0 CD4/CD5, §3.1 function rows, §6 rows 1-2, §8; tier REAL_PG plus
// one HTTP_PG subtest for the strict buyer body). Written from the contract and the FROZEN blocks of
// customers-core.md only. Helper prefix `cbc`. What it proves:
//   - buyer grant / replay / withdraw / re-grant; merchant withdraw; a merchant can never grant (CHECK, no
//     function, buyer function refuses merchant/erasure sources, runtime cannot execute it);
//   - every new key inserts a row even when the state is unchanged, so a stale retry can never re-grant:
//     grant(K, already granted) -> withdraw(K2) -> retry K returns the stored row and the state stays withdrawn;
//   - PT409 for the same key with a different purpose/granted/source/policy, and for an EXPORT key reused for
//     ERASURE (and the reverse);
//   - the owner row lock serializes the three writers: a lock holder blocks the call (pg_blocking_pids witness),
//     N same-key concurrent retries make exactly one row, alternating concurrent writers never fail;
//   - consent_allows exactly per the frozen SQL comment: latest event granted AND owner active; unknown owner,
//     other store, other tenant and invalid pair are false and never an error; works with no GUCs set and with
//     hostile GUCs; false after withdrawal and after erasure;
//   - cross-store isolation; request body `policy_version` / `source` rejected as unknown fields (D12).
// Disclosed owner-pool fixtures: buyer.owners.active flips and grants of the merchant principals (the store is
// a shared-fixture store, not created through create_initial_store; CB02 covers that path).

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"livecommerce/internal/buyer"
	"livecommerce/internal/claims"
	"livecommerce/internal/command"
	"livecommerce/internal/customers"
	"livecommerce/internal/platform"
)

const (
	cbcMM, cbcDM, cbcAP, cbcADS = "marketing_messages", "meta_dm", "ads_personalization", "meta_ads"
)

type cbcEnv struct {
	t         *testing.T
	h         bhHarness
	f         *testFixture
	principal string
	mtoken    string // customers:read + customers:privacy on A1 and A2
	rtoken    string // customers:read only on A1
	otoken    string // other tenant, customers:privacy on store B
}

func cbcSetup(t *testing.T) *cbcEnv {
	t.Helper()
	h := bhSetup(t)
	f := h.f
	c := &cbcEnv{t: t, h: h, f: f}
	c.principal, c.mtoken = lcPrincipal(t, f, f.tenantA, []string{f.storeA1, f.storeA2}, "store:read", "customers:read", "customers:privacy")
	_, c.rtoken = lcPrincipal(t, f, f.tenantA, []string{f.storeA1}, "store:read", "customers:read")
	_, c.otoken = lcPrincipal(t, f, f.tenantB, []string{f.storeB}, "store:read", "customers:read", "customers:privacy")
	return c
}

func (c *cbcEnv) newOwner() buyer.Capability {
	return mustIssue(c.t, c.h.cqHarness.service, c.f.storeA1)
}

func (c *cbcEnv) buyerTx(cp buyer.Capability, store string, fn func(context.Context, pgx.Tx, buyer.Scope) error) error {
	return buyer.WithScope(context.Background(), c.h.a.runtime, cp.Token, store, fn)
}

// put mirrors internal/buyerhttp.consentPut: one buyer-pool transaction WITHOUT buyer.WithScope (resolve_scope's
// FOR SHARE followed by the definer's FOR UPDATE would deadlock two concurrent requests of one buyer), same
// lock_timeout as buyerTransaction so a real deadlock still fails here.
func (c *cbcEnv) put(cp buyer.Capability, key string, in customers.ConsentInput) (res customers.ConsentResult, err error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	tx, err := c.h.a.runtime.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return res, err
	}
	defer tx.Rollback(context.Background())
	if _, err = tx.Exec(ctx, `SELECT set_config('lock_timeout','1s',true)`); err != nil {
		return res, err
	}
	if res, err = customers.BuyerSetConsent(ctx, tx, c.f.storeA1, cp.Token, key, in); err != nil {
		return res, err
	}
	return res, tx.Commit(ctx)
}

func (c *cbcEnv) withdraw(token, store, customer, key string, in customers.WithdrawInput) (res customers.ConsentResult, err error) {
	err = platform.WithScope(context.Background(), c.f.runtime, token, store, "store:read", func(tx pgx.Tx, s platform.Scope) (e error) {
		res, e = customers.WithdrawConsent(context.Background(), tx, s, token, key, customer, in)
		return e
	})
	return res, err
}

func (c *cbcEnv) allows(owner, purpose, channel string) bool {
	c.t.Helper()
	var ok bool
	if err := c.f.owner.QueryRow(context.Background(), `SELECT customers.consent_allows($1,$2,$3,$4,$5)`, c.f.tenantA, c.f.storeA1, owner, purpose, channel).Scan(&ok); err != nil {
		c.t.Fatalf("consent_allows: %v", err)
	}
	return ok
}

func (c *cbcEnv) rowCount(owner string) (n int) {
	c.t.Helper()
	if err := c.f.owner.QueryRow(context.Background(), `SELECT count(*) FROM customers.consent_events WHERE owner_id=$1`, owner).Scan(&n); err != nil {
		c.t.Fatal(err)
	}
	return n
}

func (c *cbcEnv) row(owner, key string) (source, policy string, granted bool, principal *string, n int) {
	c.t.Helper()
	kid, err := customers.KeyUUID(key)
	if err != nil {
		c.t.Fatal(err)
	}
	if err := c.f.owner.QueryRow(context.Background(), `SELECT count(*) FROM customers.consent_events WHERE owner_id=$1 AND request_key=$2`, owner, kid).Scan(&n); err != nil {
		c.t.Fatal(err)
	}
	if n > 0 {
		if err := c.f.owner.QueryRow(context.Background(), `SELECT source,policy_version,granted,principal_id::text FROM customers.consent_events WHERE owner_id=$1 AND request_key=$2 ORDER BY occurred_at DESC,id DESC LIMIT 1`, owner, kid).
			Scan(&source, &policy, &granted, &principal); err != nil {
			c.t.Fatal(err)
		}
	}
	return
}

// cbxNotFound: an unknown customer, another store's customer and another tenant's token all surface as one
// "not found" class (the HTTP layer renders it 404, CB09), never as forbidden.
func cbxNotFound(err error) bool {
	return errors.Is(err, platform.ErrScopeNotFound) || errors.Is(err, command.ErrNotFound)
}

func cbcCode(err error) string {
	var pg *pgconn.PgError
	if errors.As(err, &pg) {
		return pg.Code
	}
	return ""
}

// cbcHold locks the owner row from a separate connection and returns the holder's pid and its release.
func (c *cbcEnv) cbcHold(owner string) (pid int, release func()) {
	c.t.Helper()
	ctx := context.Background()
	tx, err := c.f.owner.Begin(ctx)
	if err != nil {
		c.t.Fatal(err)
	}
	if err := tx.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&pid); err != nil {
		c.t.Fatal(err)
	}
	var id string
	if err := tx.QueryRow(ctx, `SELECT id::text FROM buyer.owners WHERE id=$1 FOR SHARE`, owner).Scan(&id); err != nil {
		c.t.Fatal(err)
	}
	released := false
	release = func() {
		if !released {
			released = true
			_ = tx.Rollback(ctx)
		}
	}
	c.t.Cleanup(release)
	return pid, release
}

// awaitBlocked waits until backend pid is blocked by holder; a call that finishes first did not take the lock.
func (c *cbcEnv) awaitBlocked(holder, pid int, fn string, done <-chan error) {
	c.t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case err := <-done:
			c.t.Fatalf("%s finished (%v) while another transaction held FOR SHARE on the owner row: it does not take the owner lock (CD4)", fn, err)
		default:
		}
		var blocked bool
		if err := c.f.owner.QueryRow(context.Background(), `SELECT $1=ANY(pg_blocking_pids($2))`, holder, pid).Scan(&blocked); err != nil {
			c.t.Fatal(err)
		}
		if blocked {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	c.t.Fatalf("%s never blocked on the held owner row within 15 s", fn)
}

// bundleCustomer makes a customer out of a fresh owner without an order: a manual claim bundle redeemed by it.
func (c *cbcEnv) bundleCustomer() buyer.Capability {
	c.t.Helper()
	f := c.f
	h := &lcHarness{cqHarness: c.h.cqHarness, ctx: context.Background()}
	h.actor, h.token = lcPrincipal(c.t, f, f.tenantA, []string{f.storeA1}, "store:read", "live:read", "live:manage")
	var err error
	if h.labels, err = claims.NewLabelKey(randomBytes(32)); err != nil {
		c.t.Fatal(err)
	}
	s := h.draft(c.t, f.storeA1)
	h.offer(c.t, s, "A1", c.h.stock.skus[0].ID, 5)
	h.open(c.t, s, claims.MatchExact)
	r := h.accepted(c.t, s, "", "cbc-"+t04Tag(), "A1+1")
	cp := c.newOwner()
	if _, err := h.redeem(cp, t04Key("cbc-bundle"), h.link(c.t, s, r.BundleID, 0, false).Token, r.BundleVersion); err != nil {
		c.t.Fatalf("redeem: %v", err)
	}
	h.closeWindow(c.t, s)
	return cp
}

func TestCustomersBillingCB04Consent(t *testing.T) {
	c := cbcSetup(t)
	f := c.f
	ctx := context.Background()
	in := func(purpose, channel string, granted bool, context string) customers.ConsentInput {
		return customers.ConsentInput{Purpose: purpose, Channel: channel, Granted: granted, Context: context}
	}

	t.Run("buyer grant, replay, withdraw, re-grant; source and policy are server-set", func(t *testing.T) {
		cp := c.newOwner()
		owner := cp.Scope.OwnerID
		if c.allows(owner, cbcMM, cbcDM) {
			t.Fatal("absence must be not granted")
		}
		k1 := t04Key("cbc-grant")
		g, err := c.put(cp, k1, in(cbcMM, cbcDM, true, "checkout"))
		if err != nil || !g.Granted || g.Purpose != cbcMM || g.Channel != cbcDM || g.OccurredAt == "" {
			t.Fatalf("grant: %+v %v", g, err)
		}
		if src, pol, granted, principal, n := c.row(owner, k1); n != 1 || src != "buyer_checkout" || pol != customers.PrivacyPolicyVersion || !granted || principal != nil {
			t.Fatalf("stored row source=%s policy=%s granted=%v principal=%v n=%d (context checkout -> buyer_checkout, policy = server constant)", src, pol, granted, principal, n)
		}
		if !c.allows(owner, cbcMM, cbcDM) || c.allows(owner, cbcAP, cbcADS) {
			t.Fatal("marketing granted, ads personalization must stay not granted (independent purposes)")
		}
		again, err := c.put(cp, k1, in(cbcMM, cbcDM, true, "checkout"))
		if err != nil || again != g || c.rowCount(owner) != 1 {
			t.Fatalf("replay must return the stored result and add no row: %+v vs %+v err=%v rows=%d", again, g, err, c.rowCount(owner))
		}
		var priv customers.BuyerPrivacy
		if err := c.buyerTx(cp, f.storeA1, func(ctx context.Context, tx pgx.Tx, s buyer.Scope) (e error) {
			priv, e = customers.ReadBuyerPrivacy(ctx, tx, s, cp.Token)
			return e
		}); err != nil || !priv.Consents.MarketingMessages || priv.Consents.AdsPersonalization || priv.Erased {
			t.Fatalf("buyer privacy view: %+v %v", priv, err)
		}
		k2 := t04Key("cbc-withdraw")
		w, err := c.put(cp, k2, in(cbcMM, cbcDM, false, "settings"))
		if err != nil || w.Granted {
			t.Fatalf("withdraw: %+v %v", w, err)
		}
		if src, _, _, _, _ := c.row(owner, k2); src != "buyer_settings" {
			t.Fatalf("context settings -> buyer_settings, got %s", src)
		}
		if c.allows(owner, cbcMM, cbcDM) {
			t.Fatal("consent_allows must be false after the buyer's withdrawal")
		}
		if r, err := c.put(cp, t04Key("cbc-regrant"), in(cbcMM, cbcDM, true, "settings")); err != nil || !r.Granted || !c.allows(owner, cbcMM, cbcDM) {
			t.Fatalf("a buyer may grant again after withdrawing: %+v %v", r, err)
		}
		if _, err := c.put(cp, t04Key("cbc-ads"), in(cbcAP, cbcADS, true, "settings")); err != nil || !c.allows(owner, cbcAP, cbcADS) || !c.allows(owner, cbcMM, cbcDM) {
			t.Fatalf("ads grant must not disturb marketing: %v", err)
		}
	})

	t.Run("K/K2 stale retry: an unchanged-state write still records its key and a retry can never re-grant", func(t *testing.T) {
		cp := c.newOwner()
		owner := cp.Scope.OwnerID
		if _, err := c.put(cp, t04Key("cbc-k0"), in(cbcMM, cbcDM, true, "checkout")); err != nil {
			t.Fatal(err)
		}
		k := t04Key("cbc-K")
		stored, err := c.put(cp, k, in(cbcMM, cbcDM, true, "checkout")) // already granted: state unchanged
		if err != nil || !stored.Granted {
			t.Fatalf("grant of an already granted pair: %+v %v", stored, err)
		}
		if _, _, _, _, n := c.row(owner, k); n != 1 {
			t.Fatalf("an unchanged-state write must still insert a row for its key, got %d (I02)", n)
		}
		if _, err := c.put(cp, t04Key("cbc-K2"), in(cbcMM, cbcDM, false, "settings")); err != nil {
			t.Fatal(err)
		}
		rows := c.rowCount(owner)
		retry, err := c.put(cp, k, in(cbcMM, cbcDM, true, "checkout"))
		if err != nil || retry != stored {
			t.Fatalf("retry of K must return the stored row %+v, got %+v (%v)", stored, retry, err)
		}
		if c.rowCount(owner) != rows || c.allows(owner, cbcMM, cbcDM) {
			t.Fatalf("the retry re-granted: rows %d -> %d, allows=%v", rows, c.rowCount(owner), c.allows(owner, cbcMM, cbcDM))
		}
	})

	t.Run("PT409: same key with a different purpose, granted, source or policy", func(t *testing.T) {
		cp := c.newOwner()
		hash := sha256.Sum256([]byte(cp.Token))
		key := randomUUID()
		call := func(purpose, channel string, granted bool, source, policy, k string) error {
			var raw []byte
			return c.h.a.runtime.QueryRow(ctx, `SELECT customers.buyer_set_consent($1,$2::uuid,$3,$4,$5,$6,$7,$8::uuid)`, hash[:], f.storeA1, purpose, channel, granted, source, policy, k).Scan(&raw)
		}
		if err := call(cbcMM, cbcDM, true, "buyer_checkout", "lc-2026-10", key); err != nil {
			t.Fatalf("first call: %v", err)
		}
		if err := call(cbcMM, cbcDM, true, "buyer_checkout", "lc-2026-10", key); err != nil {
			t.Fatalf("identical replay must succeed: %v", err)
		}
		for name, args := range map[string][]any{
			"different granted": {cbcMM, cbcDM, false, "buyer_checkout", "lc-2026-10"},
			"different purpose": {cbcAP, cbcADS, true, "buyer_checkout", "lc-2026-10"},
			"different source":  {cbcMM, cbcDM, true, "buyer_settings", "lc-2026-10"},
			"different policy":  {cbcMM, cbcDM, true, "buyer_checkout", "lc-2026-11"},
		} {
			err := call(args[0].(string), args[1].(string), args[2].(bool), args[3].(string), args[4].(string), key)
			if cbcCode(err) != "PT409" || !strings.Contains(err.Error(), "idempotency_conflict") {
				t.Errorf("%s: %v, want PT409 idempotency_conflict", name, err)
			}
		}
		if n := c.rowCount(cp.Scope.OwnerID); n != 1 {
			t.Errorf("conflicting retries must not insert: %d rows", n)
		}
		// through the Go API the same conflict is the frozen sentinel
		k := t04Key("cbc-go409")
		if _, err := c.put(cp, k, in(cbcMM, cbcDM, false, "settings")); err != nil {
			t.Fatal(err)
		}
		if _, err := c.put(cp, k, in(cbcMM, cbcDM, true, "settings")); !errors.Is(err, customers.ErrIdempotencyConflict) {
			t.Errorf("Go API: %v, want ErrIdempotencyConflict", err)
		}
		if _, err := c.put(cp, k, in(cbcMM, cbcDM, false, "checkout")); !errors.Is(err, customers.ErrIdempotencyConflict) {
			t.Errorf("Go API (different context): %v, want ErrIdempotencyConflict", err)
		}
	})

	t.Run("an EXPORT key reused for ERASURE (and the reverse) is PT409", func(t *testing.T) {
		export := func(token, store, customer, key string) (err error) {
			return platform.WithScope(ctx, f.runtime, token, store, "store:read", func(tx pgx.Tx, s platform.Scope) error {
				_, e := customers.Export(ctx, tx, s, token, key, customer)
				return e
			})
		}
		erase := func(token, store, customer, key string) (err error) {
			return platform.WithScope(ctx, f.runtime, token, store, "store:read", func(tx pgx.Tx, s platform.Scope) error {
				_, e := customers.Erase(ctx, tx, s, token, key, customer)
				return e
			})
		}
		a, b := c.bundleCustomer(), c.bundleCustomer() // customers = owners with an order or a bound bundle (§3.1 read)
		k := t04Key("cbc-exp-then-erase")
		if err := export(c.mtoken, f.storeA1, a.Scope.OwnerID, k); err != nil {
			t.Fatalf("export: %v", err)
		}
		if err := erase(c.mtoken, f.storeA1, a.Scope.OwnerID, k); !errors.Is(err, customers.ErrIdempotencyConflict) {
			t.Fatalf("erasure under an EXPORT key: %v, want ErrIdempotencyConflict", err)
		}
		var erasures int
		var active bool
		if err := f.owner.QueryRow(ctx, `SELECT (SELECT count(*) FROM customers.privacy_actions WHERE owner_id=$1 AND kind='ERASURE'),(SELECT active FROM buyer.owners WHERE id=$1)`, a.Scope.OwnerID).Scan(&erasures, &active); err != nil || erasures != 0 || !active {
			t.Fatalf("the refused erasure left effects: erasures=%d active=%v %v", erasures, active, err)
		}
		if err := export(c.mtoken, f.storeA1, a.Scope.OwnerID, k); err != nil {
			t.Fatalf("export replay: %v", err)
		}
		k2 := t04Key("cbc-erase-then-exp")
		if err := erase(c.mtoken, f.storeA1, b.Scope.OwnerID, k2); err != nil {
			t.Fatalf("erase: %v", err)
		}
		if err := export(c.mtoken, f.storeA1, b.Scope.OwnerID, k2); !errors.Is(err, customers.ErrIdempotencyConflict) {
			t.Fatalf("export under an ERASURE key: %v, want ErrIdempotencyConflict", err)
		}
		if err := erase(c.mtoken, f.storeA1, b.Scope.OwnerID, k2); err != nil {
			t.Fatalf("erasure replay with its own key must return the stored summary: %v", err)
		}
	})

	t.Run("merchant withdraw: recorded, audited, permission-gated, store-scoped", func(t *testing.T) {
		cp := c.newOwner()
		owner := cp.Scope.OwnerID
		if _, err := c.put(cp, t04Key("cbc-m-grant"), in(cbcAP, cbcADS, true, "settings")); err != nil {
			t.Fatal(err)
		}
		k := t04Key("cbc-m-withdraw")
		w, err := c.withdraw(c.mtoken, f.storeA1, owner, k, customers.WithdrawInput{Purpose: cbcAP, Channel: cbcADS})
		if err != nil || w.Granted || w.Purpose != cbcAP || w.Channel != cbcADS {
			t.Fatalf("withdraw: %+v %v", w, err)
		}
		src, _, granted, principal, n := c.row(owner, k)
		if n != 1 || src != "merchant_recorded" || granted || principal == nil || *principal != c.principal {
			t.Fatalf("merchant row: src=%s granted=%v principal=%v n=%d", src, granted, principal, n)
		}
		if c.allows(owner, cbcAP, cbcADS) {
			t.Fatal("consent_allows must be false after the merchant's withdrawal")
		}
		var audits int
		if err := f.owner.QueryRow(ctx, `SELECT count(*) FROM ops.audit_events WHERE tenant_id=$1 AND store_id=$2 AND action='customers.consent_withdrawn' AND principal_id=$3`, f.tenantA, f.storeA1, c.principal).Scan(&audits); err != nil || audits < 1 {
			t.Fatalf("audit customers.consent_withdrawn missing: %d %v", audits, err)
		}
		if r, err := c.withdraw(c.mtoken, f.storeA1, owner, k, customers.WithdrawInput{Purpose: cbcAP, Channel: cbcADS}); err != nil || r != w {
			t.Fatalf("withdraw replay: %+v %v", r, err)
		}
		if _, _, _, _, n := c.row(owner, k); n != 1 {
			t.Fatalf("withdraw replay inserted %d rows", n)
		}
		if _, err := c.withdraw(c.mtoken, f.storeA1, owner, k, customers.WithdrawInput{Purpose: cbcMM, Channel: cbcDM}); !errors.Is(err, customers.ErrIdempotencyConflict) {
			t.Errorf("same key, different pair: %v, want ErrIdempotencyConflict", err)
		}
		// a withdrawal of a never-granted pair is still one recorded row (always insert)
		fresh := c.newOwner()
		if _, err := c.withdraw(c.mtoken, f.storeA1, fresh.Scope.OwnerID, t04Key("cbc-m-never"), customers.WithdrawInput{Purpose: cbcMM, Channel: cbcDM}); err != nil || c.rowCount(fresh.Scope.OwnerID) != 1 {
			t.Errorf("withdrawal of a never-granted pair: %v rows=%d", err, c.rowCount(fresh.Scope.OwnerID))
		}
		if _, err := c.withdraw(c.mtoken, f.storeA1, owner, t04Key("cbc-m-badpair"), customers.WithdrawInput{Purpose: cbcMM, Channel: cbcADS}); !errors.Is(err, command.ErrInvalid) {
			t.Errorf("invalid pair: %v, want ErrInvalid", err)
		}
		// customers:read alone cannot withdraw
		before := c.rowCount(owner)
		if _, err := c.withdraw(c.rtoken, f.storeA1, owner, t04Key("cbc-m-noperm"), customers.WithdrawInput{Purpose: cbcMM, Channel: cbcDM}); !errors.Is(err, platform.ErrForbidden) {
			t.Errorf("read-only member: %v, want ErrForbidden", err)
		}
		if c.rowCount(owner) != before {
			t.Error("a refused withdrawal inserted a row")
		}
		// another store/tenant sees an unknown customer, not a forbidden one
		for name, tok := range map[string][2]string{"same tenant other store": {c.mtoken, f.storeA2}, "other tenant": {c.otoken, f.storeB}} {
			_, err := c.withdraw(tok[0], tok[1], owner, t04Key("cbc-m-cross"), customers.WithdrawInput{Purpose: cbcMM, Channel: cbcDM})
			if !cbxNotFound(err) {
				t.Errorf("%s: %v, want the not-found class (indistinguishable from a missing customer)", name, err)
			}
		}
		if c.rowCount(owner) != before {
			t.Error("a cross-store withdrawal inserted a row")
		}
	})

	t.Run("a merchant can never grant", func(t *testing.T) {
		// catalog: no function callable by the merchant runtime accepts a granted flag, and the buyer function is not callable by it
		for _, sig := range e2(c, `SELECT p.oid::regprocedure::text FROM pg_proc p JOIN pg_namespace n ON n.oid=p.pronamespace
		 WHERE n.nspname='customers' AND has_function_privilege('commerce_runtime',p.oid,'EXECUTE')`) {
			if strings.Contains(sig, "boolean") {
				t.Errorf("merchant-callable function %s takes a boolean (a grant path)", sig)
			}
		}
		cp := c.newOwner()
		hash := sha256.Sum256([]byte(cp.Token))
		var raw []byte
		err := f.runtime.QueryRow(ctx, `SELECT customers.buyer_set_consent($1,$2::uuid,$3,$4,true,'buyer_checkout','lc-2026-10',$5::uuid)`, hash[:], f.storeA1, cbcMM, cbcDM, randomUUID()).Scan(&raw)
		if cbcCode(err) != "42501" {
			t.Errorf("commerce_runtime calling buyer_set_consent: %v, want 42501 permission denied", err)
		}
		for _, source := range []string{"merchant_recorded", "erasure", "api"} {
			err := c.h.a.runtime.QueryRow(ctx, `SELECT customers.buyer_set_consent($1,$2::uuid,$3,$4,true,$5,'lc-2026-10',$6::uuid)`, hash[:], f.storeA1, cbcMM, cbcDM, source, randomUUID()).Scan(&raw)
			if err == nil {
				t.Errorf("the buyer function accepted source %s with granted=true", source)
			}
		}
		if n := c.rowCount(cp.Scope.OwnerID); n != 0 {
			t.Errorf("refused grants left %d rows", n)
		}
		// a stranger's capability cannot write another store's owner: the capability is scoped to its store
		if err := c.buyerTx(cp, f.storeA2, func(context.Context, pgx.Tx, buyer.Scope) error { return nil }); err == nil {
			t.Error("a store-A1 capability resolved in store A2")
		}
	})

	t.Run("consent_allows exactly per the frozen comment", func(t *testing.T) {
		cp := c.newOwner()
		owner := cp.Scope.OwnerID
		if _, err := c.put(cp, t04Key("cbc-allow"), in(cbcMM, cbcDM, true, "checkout")); err != nil {
			t.Fatal(err)
		}
		type q struct{ tenant, store, owner, purpose, channel string }
		other := mustIssue(t, c.h.cqHarness.service, f.storeA2)
		cases := map[string]struct {
			q    q
			want bool
		}{
			"granted pair":                            {q{f.tenantA, f.storeA1, owner, cbcMM, cbcDM}, true},
			"other pair of the same owner":            {q{f.tenantA, f.storeA1, owner, cbcAP, cbcADS}, false},
			"invalid pair (purpose/channel mismatch)": {q{f.tenantA, f.storeA1, owner, cbcMM, cbcADS}, false},
			"invalid purpose":                         {q{f.tenantA, f.storeA1, owner, "sms", cbcDM}, false},
			"unknown owner":                           {q{f.tenantA, f.storeA1, randomUUID(), cbcMM, cbcDM}, false},
			"owner asked in another store":            {q{f.tenantA, f.storeA2, owner, cbcMM, cbcDM}, false},
			"owner asked in another tenant":           {q{f.tenantB, f.storeB, owner, cbcMM, cbcDM}, false},
			"store of another owner":                  {q{f.tenantA, f.storeA2, other.Scope.OwnerID, cbcMM, cbcDM}, false},
		}
		login := c.authLogin()
		for name, tc := range cases {
			var got bool
			if err := login.QueryRow(ctx, `SELECT customers.consent_allows($1,$2,$3,$4,$5)`, tc.q.tenant, tc.q.store, tc.q.owner, tc.q.purpose, tc.q.channel).Scan(&got); err != nil {
				t.Errorf("%s: consent_allows must never raise: %v", name, err)
				continue
			}
			if got != tc.want {
				t.Errorf("%s: %v, want %v", name, got, tc.want)
			}
		}
		// works with no GUCs at all (fresh connection) and with hostile GUCs naming another store
		var withGUC bool
		tx, err := login.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback(ctx)
		if _, err := tx.Exec(ctx, `SELECT set_config('app.tenant_id',$1,true),set_config('app.store_id',$2,true),set_config('app.principal_id',$3,true)`, f.tenantB, f.storeB, randomUUID()); err != nil {
			t.Fatal(err)
		}
		if err := tx.QueryRow(ctx, `SELECT customers.consent_allows($1,$2,$3,$4,$5)`, f.tenantA, f.storeA1, owner, cbcMM, cbcDM).Scan(&withGUC); err != nil || !withGUC {
			t.Errorf("consent_allows under hostile GUCs: %v %v (it reads by its own arguments, so it works from any GUC state)", withGUC, err)
		}
		_ = tx.Rollback(ctx)
		// latest event decides: withdrawal -> false; then an inactive owner is false even with a granted latest event
		if _, err := c.put(cp, t04Key("cbc-allow-w"), in(cbcMM, cbcDM, false, "settings")); err != nil || c.allows(owner, cbcMM, cbcDM) {
			t.Fatalf("after withdrawal: %v", err)
		}
		if _, err := c.put(cp, t04Key("cbc-allow-g"), in(cbcMM, cbcDM, true, "settings")); err != nil || !c.allows(owner, cbcMM, cbcDM) {
			t.Fatalf("after re-grant: %v", err)
		}
		mustExec(t, f.owner, `UPDATE buyer.owners SET active=false WHERE id=$1`, owner) // fixture: an inactive owner with a granted latest event
		if c.allows(owner, cbcMM, cbcDM) {
			t.Error("consent_allows must require the owner to be active")
		}
		mustExec(t, f.owner, `UPDATE buyer.owners SET active=true WHERE id=$1`, owner)
	})

	t.Run("erasure withdraws every pair; nothing is granted afterwards", func(t *testing.T) {
		cp := c.newOwner()
		owner := cp.Scope.OwnerID
		for _, p := range [][2]string{{cbcMM, cbcDM}, {cbcAP, cbcADS}} {
			if _, err := c.put(cp, t04Key("cbc-er-"+p[0][:4]), in(p[0], p[1], true, "settings")); err != nil {
				t.Fatal(err)
			}
		}
		if err := platform.WithScope(ctx, f.runtime, c.mtoken, f.storeA1, "store:read", func(tx pgx.Tx, s platform.Scope) error {
			_, e := customers.Erase(ctx, tx, s, c.mtoken, t04Key("cbc-erase"), owner)
			return e
		}); err != nil {
			t.Fatalf("erase: %v", err)
		}
		if c.allows(owner, cbcMM, cbcDM) || c.allows(owner, cbcAP, cbcADS) {
			t.Fatal("consent_allows must be false for both pairs after erasure")
		}
		var erasureRows int
		if err := f.owner.QueryRow(ctx, `SELECT count(*) FROM customers.consent_events WHERE owner_id=$1 AND source='erasure' AND NOT granted`, owner).Scan(&erasureRows); err != nil || erasureRows != 2 {
			t.Fatalf("erasure must record one withdrawal row per granted pair, got %d %v", erasureRows, err)
		}
		if _, err := c.put(cp, t04Key("cbc-er-regrant"), in(cbcMM, cbcDM, true, "settings")); err == nil {
			t.Fatal("a capability revoked by erasure re-granted consent")
		}
		if c.allows(owner, cbcMM, cbcDM) {
			t.Fatal("consent came back after erasure")
		}
	})

	t.Run("owner lock: a held owner row blocks the three writers (pg_blocking_pids witness)", func(t *testing.T) {
		// The holder takes FOR SHARE on the owner row: buyer.resolve_scope (FOR SHARE) passes, so the blocked
		// backend is waiting inside the customers definer, whose FOR UPDATE conflicts with it (CD4).
		type call struct {
			fn  string
			run func(cp buyer.Capability, owner string, report func(pgx.Tx)) error
		}
		calls := []call{
			{"buyer_set_consent", func(cp buyer.Capability, owner string, report func(pgx.Tx)) error {
				return c.buyerTx(cp, f.storeA1, func(ctx context.Context, tx pgx.Tx, s buyer.Scope) error {
					report(tx)
					_, e := customers.BuyerSetConsent(ctx, tx, s.StoreID, cp.Token, t04Key("cbc-lock-b"), in(cbcMM, cbcDM, true, "settings"))
					return e
				})
			}},
			{"merchant_withdraw_consent", func(cp buyer.Capability, owner string, report func(pgx.Tx)) error {
				return platform.WithScope(ctx, f.runtime, c.mtoken, f.storeA1, "store:read", func(tx pgx.Tx, s platform.Scope) error {
					report(tx)
					_, e := customers.WithdrawConsent(ctx, tx, s, c.mtoken, t04Key("cbc-lock-m"), owner, customers.WithdrawInput{Purpose: cbcMM, Channel: cbcDM})
					return e
				})
			}},
			{"erase_owner", func(cp buyer.Capability, owner string, report func(pgx.Tx)) error {
				return platform.WithScope(ctx, f.runtime, c.mtoken, f.storeA1, "store:read", func(tx pgx.Tx, s platform.Scope) error {
					report(tx)
					_, e := customers.Erase(ctx, tx, s, c.mtoken, t04Key("cbc-lock-e"), owner)
					return e
				})
			}},
		}
		for _, cl := range calls {
			cp := c.newOwner()
			holder, release := c.cbcHold(cp.Scope.OwnerID)
			pids, done := make(chan int, 1), make(chan error, 1)
			report := func(tx pgx.Tx) {
				var pid int
				if err := tx.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&pid); err != nil {
					pid = -1
				}
				pids <- pid
			}
			go func() { done <- cl.run(cp, cp.Scope.OwnerID, report) }()
			var pid int
			select {
			case pid = <-pids:
			case err := <-done:
				t.Fatalf("%s ended before reaching the database: %v", cl.fn, err)
			case <-time.After(15 * time.Second):
				t.Fatalf("%s never started", cl.fn)
			}
			c.awaitBlocked(holder, pid, cl.fn, done)
			release()
			select {
			case err := <-done:
				if err != nil {
					t.Errorf("%s after the lock was released: %v", cl.fn, err)
				}
			case <-time.After(20 * time.Second):
				t.Fatalf("%s did not finish after the owner lock was released", cl.fn)
			}
		}
	})

	t.Run("concurrency: one key makes one row; alternating writers never fail", func(t *testing.T) {
		cp := c.newOwner()
		owner := cp.Scope.OwnerID
		same := t04Key("cbc-conc-same")
		var wg sync.WaitGroup
		results := make([]customers.ConsentResult, 12)
		errs := make([]error, 12)
		for i := range results {
			wg.Add(1)
			go func() {
				defer wg.Done()
				results[i], errs[i] = c.put(cp, same, in(cbcMM, cbcDM, true, "settings"))
			}()
		}
		wg.Wait()
		failed := 0
		for i := range results {
			if errs[i] != nil || results[i] != results[0] {
				failed++
				t.Errorf("concurrent same-key call %d: %+v %v (want every call to return the one stored row)", i, results[i], errs[i])
			}
		}
		if failed > 0 {
			t.Errorf("%d of %d concurrent same-key requests of ONE buyer failed instead of serializing behind the owner lock (CD4)", failed, len(results))
		}
		if _, _, _, _, n := c.row(owner, same); n > 1 {
			t.Errorf("same-key concurrency produced %d rows (I02: one key, one row)", n)
		}
		errs = make([]error, 24)
		for i := range errs {
			wg.Add(1)
			go func() {
				defer wg.Done()
				_, errs[i] = c.put(cp, t04Key("cbc-conc-alt"), in(cbcMM, cbcDM, i%2 == 0, "settings"))
			}()
		}
		wg.Wait()
		for i, err := range errs {
			if err != nil {
				t.Errorf("alternating writer %d: %v", i, err)
			}
		}
		if n := c.rowCount(owner); n != 25 && failed == 0 {
			t.Errorf("rows=%d, want 1+24", n)
		}
		var latest bool
		if err := f.owner.QueryRow(ctx, `SELECT granted FROM customers.consent_events WHERE owner_id=$1 AND purpose=$2 ORDER BY occurred_at DESC,id DESC LIMIT 1`, owner, cbcMM).Scan(&latest); err != nil || latest != c.allows(owner, cbcMM, cbcDM) {
			t.Errorf("consent_allows disagrees with the latest event: latest=%v %v", latest, err)
		}
	})

	t.Run("request body policy_version / source are rejected as unknown fields (D12)", func(t *testing.T) {
		issued := bhRead[struct {
			Token string `json:"token"`
		}](t, c.h.request(t, "POST", "/v1/buyer/session", "", "", struct{}{}, nil), 200)
		token := issued.Token
		var owner string
		if err := buyer.WithScope(ctx, c.h.a.runtime, token, f.storeA1, func(_ context.Context, _ pgx.Tx, s buyer.Scope) error { owner = s.OwnerID; return nil }); err != nil {
			t.Fatal(err)
		}
		good := map[string]any{"purpose": cbcMM, "channel": cbcDM, "granted": true, "context": "checkout"}
		for _, extra := range []string{"policy_version", "source", "occurred_at", "owner_id", "principal_id", "granted_by"} {
			body := map[string]any{}
			for k, v := range good {
				body[k] = v
			}
			body[extra] = "x"
			r := c.h.request(t, "PUT", "/v1/buyer/consents", token, t04Key("cbc-http-extra"), body, nil)
			if r.status != 422 {
				t.Errorf("extra key %q: HTTP %d (%s), want 422", extra, r.status, r.body)
			}
		}
		if n := c.rowCount(owner); n != 0 {
			t.Fatalf("rejected bodies left %d consent rows", n)
		}
		ok := c.h.request(t, "PUT", "/v1/buyer/consents", token, t04Key("cbc-http-ok"), good, nil)
		var res map[string]any
		if ok.status != 200 || json.Unmarshal(ok.body, &res) != nil || len(res) != 4 || res["granted"] != true || res["purpose"] != cbcMM || res["channel"] != cbcDM || res["occurred_at"] == nil {
			t.Fatalf("valid body: HTTP %d %s", ok.status, ok.body)
		}
		var src, pol string
		if err := f.owner.QueryRow(ctx, `SELECT source,policy_version FROM customers.consent_events WHERE owner_id=$1`, owner).Scan(&src, &pol); err != nil || src != "buyer_checkout" || pol != "lc-2026-10" {
			t.Fatalf("stored source=%s policy=%s %v (server-set from context and constant)", src, pol, err)
		}
		bad := map[string]any{"purpose": cbcMM, "channel": cbcDM, "granted": true, "context": "admin"}
		if r := c.h.request(t, "PUT", "/v1/buyer/consents", token, t04Key("cbc-http-ctx"), bad, nil); r.status != 422 {
			t.Errorf("context outside checkout|settings: HTTP %d, want 422", r.status)
		}
	})
}

// e2 lists the first column of a catalog query through the owner pool.
func e2(c *cbcEnv, q string) []string {
	return cbsEnv{t: c.t, f: c.f}.list(q)
}

// authLogin opens a plain login that is a member of commerce_auth only (the only EXECUTE grantee of consent_allows).
func (c *cbcEnv) authLogin() *pgxpool.Pool {
	c.t.Helper()
	return cbxLogin(c.t, c.f, "commerce_auth")
}
