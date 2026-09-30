package foundation_test

// CB05 (contracts/customers-billing-v1.md §0 CD6-CD8, §3.1 erase_owner/apply_erasure/replay_erasures, §6 row 2,
// §8, §9; tier REAL_PG over the rfx harness, with a MODEL of restore on a second database of the same
// container). Written from the contract and the FROZEN blocks + D10/D11 of customers-core.md only. Helper
// prefix `cbe`. What it proves:
//   - erasure is refused (ErrErasureBlocked, nothing written) while the owner has an unexpired DRAFT/
//     AWAITING_PAYMENT order, ANY payments.stripe_sessions row with expires_at > now (also a paid order's),
//     or a Stripe refund without a terminal fact; each blocker is isolated so the others cannot mask it;
//   - success revokes every capability session (+ capability.revoked events), deactivates the owner, redacts
//     only snapshots no order references, relabels manual bound bundles to `erased-`+32 hex even when a
//     merchant label `erased-<first 8 hex>` already exists in the session, withdraws every granted pair;
//   - bundle actor_keys, claims.meta_intake, live.claim_sources and social.* are byte-identical before/after
//     and a second owner bound to the same actor's bundle in another session is unaffected (CD7);
//   - orders, payments, refunds, shipments and referenced snapshots are byte-identical;
//   - replay is idempotent; a buyer retry after erasure is ErrErased (410), with the same or a new key;
//   - buyer and merchant erasure both work; the merchant path is permission-gated and audited;
//   - RESTORE MODEL (G09 case 3, MODEL_ONLY until T20): dump pre-erasure, restore to a fresh database;
//     replay_erasures() without ids leaves consent granted (red), replay_erasures(ids) fixes it (green).
// Disclosed owner-pool fixtures: extra capability session, unreferenced destination snapshot, facebook
// bundles + claim source + meta_intake row (the Meta pipeline is not run here), stripe session aging (the
// harness's e.age), and pg_dump/pg_restore through the labelled test container.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"net/url"
	"os/exec"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"livecommerce/internal/buyer"
	"livecommerce/internal/claims"
	"livecommerce/internal/customers"
	"livecommerce/internal/platform"
)

type cbeEnv struct {
	t   *testing.T
	e   *rfxEnv
	ctx context.Context
}

func (c *cbeEnv) erase(token, store, customer, key string) (sum customers.ErasureSummary, err error) {
	err = platform.WithScope(c.ctx, c.e.f.runtime, token, store, "store:read", func(tx pgx.Tx, s platform.Scope) (e error) {
		sum, e = customers.Erase(c.ctx, tx, s, token, key, customer)
		return e
	})
	return sum, err
}

func (c *cbeEnv) buyerErase(o buyer.Capability, pool *pgxpool.Pool, store, key string) (sum customers.ErasureSummary, err error) {
	tx, err := pool.Begin(c.ctx)
	if err != nil {
		c.t.Fatal(err)
	}
	defer tx.Rollback(c.ctx)
	sum, err = customers.BuyerErase(c.ctx, tx, o.Token, store, key)
	if err == nil {
		err = tx.Commit(c.ctx)
	}
	return sum, err
}

func (c *cbeEnv) put(o rfxOrder, cp buyer.Capability, purpose, channel string) {
	c.t.Helper()
	err := buyer.WithScope(c.ctx, o.s.p.a.runtime, cp.Token, o.store(), func(ctx context.Context, tx pgx.Tx, s buyer.Scope) error {
		_, e := customers.BuyerSetConsent(ctx, tx, s.StoreID, cp.Token, t04Key("cbe-consent"), customers.ConsentInput{Purpose: purpose, Channel: channel, Granted: true, Context: "settings"})
		return e
	})
	if err != nil {
		c.t.Fatalf("consent: %v", err)
	}
}

// digest fingerprints whole tables (row text, ordered) of the schemas that erasure must never change.
func (c *cbeEnv) digest(schemas ...string) map[string]string {
	return lcDigest(c.t, c.e.f, schemas...)
}

// bundleDigest fingerprints claims.bundles without the manual label (the one column erasure may change).
func (c *cbeEnv) bundleDigest(where string, args ...any) string {
	c.t.Helper()
	var d string
	if err := c.e.f.owner.QueryRow(c.ctx, `SELECT count(*)::text||':'||coalesce(md5(string_agg((b.tenant_id,b.store_id,b.id,b.session_id,b.platform,b.actor_key,b.owner_id,b.bound_at,b.line_count,b.version,b.created_at)::text,E'\n' ORDER BY b.id)),'') FROM claims.bundles b WHERE `+where, args...).Scan(&d); err != nil {
		c.t.Fatal(err)
	}
	return d
}

func (c *cbeEnv) ownerRow(owner string) (active bool, revokedAll bool, events int) {
	c.t.Helper()
	if err := c.e.f.owner.QueryRow(c.ctx, `SELECT o.active,
	  NOT EXISTS(SELECT 1 FROM buyer.capability_sessions s WHERE s.owner_id=o.id AND s.revoked_at IS NULL),
	  (SELECT count(*) FROM buyer.capability_events ev WHERE ev.owner_id=o.id AND ev.action='capability.revoked') FROM buyer.owners o WHERE o.id=$1`, owner).Scan(&active, &revokedAll, &events); err != nil {
		c.t.Fatal(err)
	}
	return
}

func TestCustomersBillingCB05Erasure(t *testing.T) {
	e := rfxNew(t)
	c := &cbeEnv{t: t, e: e, ctx: context.Background()}
	ctx := c.ctx
	stop := e.startWorker(t)
	defer stop()

	a := e.stripeStore(t)
	endpoint, secret := e.endpoint(t, a)
	sessionOwner := e.pay(t, a, endpoint, secret) // paid, unexpired Stripe session
	main := e.payMore(t, sessionOwner)            // the erased customer
	bystander := e.payMore(t, sessionOwner)
	e.ensureStock(t, sessionOwner)
	holdP := sstMoreHold(t, sessionOwner.s.p) // an order that is placed and unpaid
	for _, o := range []rfxOrder{sessionOwner, main, bystander} {
		e.grant(t, o, "orders:read", "customers:read", "customers:privacy", "payments:refund", "fulfillment:write")
	}
	store, tenant := main.store(), main.s.p.f.tenantA
	tok := main.token()
	mainID, sessID, byID, holdID := main.s.p.cap.Scope.OwnerID, sessionOwner.s.p.cap.Scope.OwnerID, bystander.s.p.cap.Scope.OwnerID, holdP.cap.Scope.OwnerID

	// consent for the erased customer and the bystander
	for _, id := range []struct {
		o  rfxOrder
		cp buyer.Capability
	}{{main, main.s.p.cap}, {bystander, bystander.s.p.cap}, {sessionOwner, sessionOwner.s.p.cap}} {
		c.put(id.o, id.cp, "marketing_messages", "meta_dm")
		c.put(id.o, id.cp, "ads_personalization", "meta_ads")
	}
	// a second capability session of the erased customer (disclosed fixture)
	extraHash := sha256.Sum256([]byte(randomToken()))
	mustExec(t, e.f.owner, `INSERT INTO buyer.capability_sessions(tenant_id,store_id,owner_id,token_hash,expires_at) VALUES($1,$2,$3,$4,now()+interval '1 hour')`, tenant, store, mainID, extraHash[:])
	// an unreferenced destination snapshot of the erased customer (disclosed fixture: copy of the order's snapshot, new version)
	mustExec(t, e.f.owner, `INSERT INTO storefront.destination_snapshots(tenant_id,store_id,owner_id,cart_id,cart_version,creator_session_id,version,kind,country,recipient_name,phone,region,city,postal_code,line1,line2,pickup_id,selected_at,expires_at)
	 SELECT tenant_id,store_id,owner_id,cart_id,cart_version,creator_session_id,version+50,kind,country,'Unreferenced Person','0912345678','Region','City','12345','Line 1','Line 2',pickup_id,selected_at,expires_at
	 FROM storefront.destination_snapshots WHERE owner_id=$1 ORDER BY version LIMIT 1`, mainID)
	unref := cbxOne(t, e.f, `SELECT id::text FROM storefront.destination_snapshots d WHERE d.owner_id=$1 AND NOT EXISTS(SELECT 1 FROM checkout.orders o WHERE o.destination_id=d.id)`, mainID)
	if unref == "" {
		t.Fatal("no unreferenced snapshot fixture")
	}

	// claims: main's manual bundle (label ivy) + a colliding merchant label, facebook bundles of one actor (main + bystander)
	h := cblClaims(t, sessionOwner)
	skus := lcSKUs(t, h.f, h.f.tenantA, h.f.storeA1, "TWD", 1)
	s1 := h.draft(t, store)
	h.offer(t, s1, "A1", skus[0], 5)
	h.open(t, s1, claims.MatchExact)
	ivy := h.accepted(t, s1, "", "ivy", "A1+1")
	hex8 := strings.ReplaceAll(ivy.BundleID, "-", "")[:8]
	collide := h.accepted(t, s1, "", "erased-"+hex8, "A1+1") // a merchant label that equals the 8-hex form
	if _, err := h.redeem(main.s.p.cap, t04Key("cbe-redeem"), h.link(t, s1, ivy.BundleID, 0, false).Token, ivy.BundleVersion); err != nil {
		t.Fatalf("main redeeming ivy: %v", err)
	}
	h.closeWindow(t, s1)
	s2 := h.draft(t, store)
	h.offer(t, s2, "A1", skus[0], 5)
	h.open(t, s2, claims.MatchExact)
	h.closeWindow(t, s2)
	actor := strings.Repeat("ab", 32)
	binding := randomUUID()
	mustExec(t, e.f.owner, `INSERT INTO integration.bindings(id,tenant_id,store_id,principal_id,provider,external_asset_id) VALUES($1,$2,$3,$4,'facebook','100000000000001')`, binding, tenant, store, h.actor)
	sourceID := randomUUID()
	mustExec(t, e.f.owner, `INSERT INTO live.claim_sources(id,tenant_id,store_id,session_id,platform,binding_id,binding_version,object,asset_id,source_object_id,principal_id)
	 VALUES($1,$2,$3,$4,'facebook',$5,1,'page','100000000000001','100000000000001_2000000001',$6)`, sourceID, tenant, store, s1, binding, h.actor)
	fbMain, fbBy := randomUUID(), randomUUID()
	mustExec(t, e.f.owner, `INSERT INTO claims.bundles(id,tenant_id,store_id,session_id,platform,actor_key,label,owner_id,bound_at) VALUES($1,$2,$3,$4,'facebook',$5,NULL,$6,now())`, fbMain, tenant, store, s1, actor, mainID)
	mustExec(t, e.f.owner, `INSERT INTO claims.bundles(id,tenant_id,store_id,session_id,platform,actor_key,label,owner_id,bound_at) VALUES($1,$2,$3,$4,'facebook',$5,NULL,$6,now())`, fbBy, tenant, store, s2, actor, byID)
	mustExec(t, e.f.owner, `INSERT INTO claims.meta_intake(tenant_id,store_id,inbox_event_id,source_id,session_id,platform,app_id,object,asset_id,comment_ref,live_media,actor_key,occurred_at,received_at,grammar_version,grammar_kind,not_before)
	 VALUES($1,$2,$3,$4,$5,'facebook','4291253377792879','page','100000000000001','2000000001_3000000001',false,$6,now(),now(),'kw-v1','NO_MATCH',now())`, tenant, store, randomUUID(), sourceID, s1, actor)
	t.Cleanup(func() { // lcPrincipal's purge deletes the sessions; the fixture rows that reference them go first
		mustExec(t, e.f.owner, `DELETE FROM claims.meta_intake WHERE source_id=$1`, sourceID)
		mustExec(t, e.f.owner, `DELETE FROM claims.bundles WHERE id=ANY($1::uuid[])`, []string{fbMain, fbBy})
		mustExec(t, e.f.owner, `DELETE FROM live.claim_sources WHERE id=$1`, sourceID)
	})
	bystanderBundleDigest := c.bundleDigest(`b.owner_id=$1`, byID)
	if bystanderBundleDigest == "0:" {
		t.Fatal("no bystander bundle")
	}

	// quiesce: every capture/reconcile job of the paid orders is done before the worker stops
	for _, o := range []rfxOrder{sessionOwner, main, bystander} {
		e.await(t, "query job completed", o.attempt, 45*time.Second, `SELECT NOT EXISTS(SELECT 1 FROM river_payment.river_job WHERE args->>'operation_id'=$1 AND state NOT IN ('completed','cancelled','discarded'))`)
	}
	// a refund held pending at the provider on the erased customer's order (needs the worker running)
	e.fake.HoldNextRefund("pending", "processing")
	refund := e.mustRefund(t, main, 1000, "requested_by_customer")
	e.awaitRefund(t, "refund pinned", refund, main.attempt, 45*time.Second, `SELECT stripe_refund_id IS NOT NULL FROM payments.stripe_refunds WHERE id=$1`)
	stop()
	wholeDB := []string{"buyer", "customers", "storefront", "claims", "live", "social", "checkout", "payments", "fulfillment", "inventory"}

	refused := func(name string, token, owner string, fromBuyer *buyer.Capability) {
		t.Helper()
		before := c.digest(wholeDB...)
		var err error
		if fromBuyer != nil {
			_, err = c.buyerErase(*fromBuyer, sessionOwner.s.p.a.runtime, store, t04Key("cbe-refused"))
		} else {
			_, err = c.erase(token, store, owner, t04Key("cbe-refused"))
		}
		if !errors.Is(err, customers.ErrErasureBlocked) {
			t.Fatalf("%s: %v, want ErrErasureBlocked", name, err)
		}
		lcSameDigest(t, name+": a refused erasure wrote something", before, c.digest(wholeDB...))
		if act, revoked, _ := c.ownerRow(owner); !act || revoked {
			t.Fatalf("%s: owner active=%v sessions all revoked=%v after a refusal", name, act, revoked)
		}
	}

	t.Run("refused: an unexpired DRAFT/AWAITING_PAYMENT order (merchant and buyer)", func(t *testing.T) {
		if n := e.count(t, `SELECT count(*) FROM checkout.orders WHERE owner_id=$1 AND commercial_state IN ('DRAFT','AWAITING_PAYMENT') AND expires_at>now()`, holdID); n != 1 {
			t.Fatalf("fixture: %d open orders for the hold owner", n)
		}
		if n := e.count(t, `SELECT count(*) FROM payments.stripe_sessions WHERE owner_id=$1`, holdID); n != 0 {
			t.Fatalf("fixture: the hold owner has %d stripe sessions (the order clause must be isolated)", n)
		}
		refused("merchant, open order", tok, holdID, nil)
		refused("buyer, open order", "", holdID, &holdP.cap)
	})

	t.Run("refused: any payments.stripe_sessions row with expires_at > now, even for a paid order", func(t *testing.T) {
		if n := e.count(t, `SELECT count(*) FROM checkout.orders WHERE owner_id=$1 AND commercial_state IN ('DRAFT','AWAITING_PAYMENT')`, sessID); n != 0 {
			t.Fatalf("fixture: the order of the session owner is still open (%d)", n)
		}
		if n := e.count(t, `SELECT count(*) FROM payments.stripe_sessions WHERE owner_id=$1 AND expires_at>now()`, sessID); n != 1 {
			t.Fatalf("fixture: %d unexpired sessions", n)
		}
		refused("merchant, unexpired session", tok, sessID, nil)
		// the same clause blocks the buyer; once the session expired (aged, the Stripe session of the fake too) nothing blocks
		refused("buyer, unexpired session", "", sessID, &sessionOwner.s.p.cap)
	})

	t.Run("refused: a Stripe refund without a terminal fact", func(t *testing.T) {
		e.age(t, main.attempt, 41*time.Minute)
		if n := e.count(t, `SELECT count(*) FROM payments.stripe_sessions WHERE owner_id=$1 AND expires_at>now()`, mainID); n != 0 {
			t.Fatalf("fixture: the session of the erased customer is still unexpired (%d)", n)
		}
		if n := e.count(t, `SELECT count(*) FROM checkout.orders WHERE owner_id=$1 AND commercial_state IN ('DRAFT','AWAITING_PAYMENT')`, mainID); n != 0 {
			t.Fatalf("fixture: an open order remains (%d)", n)
		}
		if e.hasRefundFact(t, refund, "SUCCEEDED") || e.hasRefundFact(t, refund, "FAILED") || e.hasRefundFact(t, refund, "CANCELED") {
			t.Fatal("fixture: the refund already has a terminal fact")
		}
		refused("merchant, non-terminal refund", tok, mainID, nil)
		// permission: customers:read alone cannot erase, and nothing is written
		before := c.digest(wholeDB...)
		readOnly, _ := e.member(t, main, "customers:read")
		if _, err := c.erase(readOnly, store, mainID, t04Key("cbe-noperm")); !errors.Is(err, platform.ErrForbidden) {
			t.Fatalf("member without customers:privacy: %v, want ErrForbidden", err)
		}
		lcSameDigest(t, "a forbidden erasure wrote something", before, c.digest(wholeDB...))
		// the refund reaches a terminal fact: the blocker is lifted
		stop2 := e.startWorker(t)
		e.fake.SetRefundStatus(e.fake.RefundByRef(refund), "succeeded", "")
		e.wakeRefund(t, refund, main.attempt)
		e.awaitRefundFact(t, refund, main.attempt, "SUCCEEDED")
		e.await(t, "refund jobs quiet", main.attempt, 45*time.Second, `SELECT NOT EXISTS(SELECT 1 FROM river_payment.river_job WHERE args->>'operation_id'=$1 AND state NOT IN ('completed','cancelled','discarded'))`)
		stop2()
	})

	// ---- the successful erasure of the main customer -------------------------------------------------
	e.age(t, sessionOwner.attempt, 41*time.Minute) // for the buyer-side erasure below
	var (
		ordersBefore = c.digest("checkout", "payments", "fulfillment", "inventory", "live", "social")
		metaBefore   = c.digest("social")
		actorBefore  = c.bundleDigest(`b.actor_key=$1 OR b.platform<>'manual'`, actor)
		manualBefore = c.bundleDigest(`b.platform='manual'`)
		intakeBefore = cbxOne(t, e.f, `SELECT md5(string_agg(m::text,E'\n' ORDER BY m.id)) FROM claims.meta_intake m`)
		sourceBefore = cbxOne(t, e.f, `SELECT md5(string_agg(s::text,E'\n' ORDER BY s.id)) FROM live.claim_sources s`)
		refBefore    = cbxOne(t, e.f, `SELECT md5(string_agg(d::text,E'\n' ORDER BY d.id)) FROM storefront.destination_snapshots d WHERE d.owner_id=$1 AND d.id<>$2::uuid`, mainID, unref)
		otherBefore  = cbxOne(t, e.f, `SELECT md5(string_agg(d::text,E'\n' ORDER BY d.id)) FROM storefront.destination_snapshots d WHERE d.owner_id<>$1`, mainID)
	)
	_ = metaBefore
	key := t04Key("cbe-erase")
	audits := e.count(t, `SELECT count(*) FROM ops.audit_events WHERE store_id=$1 AND action='customers.erased'`, store)
	sum, err := c.erase(tok, store, mainID, key)
	if err != nil {
		t.Fatalf("erase: %v", err)
	}

	t.Run("success: sessions revoked, owner inactive, consents withdrawn, summary counts (D10)", func(t *testing.T) {
		active, revoked, events := c.ownerRow(mainID)
		if active || !revoked || events != 2 {
			t.Fatalf("owner active=%v all sessions revoked=%v revoked events=%d (two sessions existed)", active, revoked, events)
		}
		if sum != (customers.ErasureSummary{ConsentsWithdrawn: 2, SessionsRevoked: 2, SnapshotsRedacted: 1, BundlesRelabelled: 1}) {
			t.Fatalf("summary %+v, want 2 consents, 2 sessions, 1 unreferenced snapshot, 1 bundle", sum)
		}
		for _, p := range [][2]string{{"marketing_messages", "meta_dm"}, {"ads_personalization", "meta_ads"}} {
			if e.count(t, `SELECT count(*) FROM customers.consent_events WHERE owner_id=$1 AND purpose=$2 AND source='erasure' AND NOT granted AND policy_version='erasure'`, mainID, p[0]) != 1 {
				t.Errorf("no erasure withdrawal row for %s", p[0])
			}
			if cbxAllows(t, e.f, tenant, store, mainID, p[0], p[1]) {
				t.Errorf("consent_allows true for %s after erasure", p[0])
			}
		}
		var via string
		var principal *string
		var stored string
		if err := e.f.owner.QueryRow(ctx, `SELECT via,principal_id::text,summary::text FROM customers.privacy_actions WHERE owner_id=$1 AND kind='ERASURE'`, mainID).Scan(&via, &principal, &stored); err != nil {
			t.Fatalf("ERASURE row: %v", err)
		}
		if via != "merchant" || principal == nil || *principal != main.s.p.f.principalA || len(stored) > 1024 {
			t.Fatalf("ERASURE row via=%s principal=%v summary=%s", via, principal, stored)
		}
		for _, k := range []string{"consents_withdrawn", "sessions_revoked", "snapshots_redacted", "bundles_relabelled"} {
			if !strings.Contains(stored, `"`+k+`"`) {
				t.Errorf("stored summary lacks %s: %s", k, stored)
			}
		}
		if got := e.count(t, `SELECT count(*) FROM ops.audit_events WHERE store_id=$1 AND action='customers.erased'`, store); got != audits+1 {
			t.Errorf("customers.erased audit rows %d -> %d", audits, got)
		}
	})

	t.Run("redacts only unreferenced snapshots; referenced ones stay byte-identical", func(t *testing.T) {
		var name, phone, region, city, postal, line1, line2 string
		if err := e.f.owner.QueryRow(ctx, `SELECT recipient_name,phone,region,city,postal_code,line1,line2 FROM storefront.destination_snapshots WHERE id=$1`, unref).Scan(&name, &phone, &region, &city, &postal, &line1, &line2); err != nil {
			t.Fatal(err)
		}
		if name != "[erased]" || phone != "000000" || region != "" || city != "[erased]" || postal != "" || line1 != "[erased]" || line2 != "" {
			t.Errorf("redaction of a home snapshot: %q %q %q %q %q %q %q", name, phone, region, city, postal, line1, line2)
		}
		if got := cbxOne(t, e.f, `SELECT md5(string_agg(d::text,E'\n' ORDER BY d.id)) FROM storefront.destination_snapshots d WHERE d.owner_id=$1 AND d.id<>$2::uuid`, mainID, unref); got != refBefore {
			t.Error("a snapshot referenced by an order changed (legal retention)")
		}
		if got := cbxOne(t, e.f, `SELECT md5(string_agg(d::text,E'\n' ORDER BY d.id)) FROM storefront.destination_snapshots d WHERE d.owner_id<>$1`, mainID); got != otherBefore {
			t.Error("another owner's snapshot changed")
		}
	})

	t.Run("relabels manual bound bundles to erased-+32 hex, also next to a colliding merchant label", func(t *testing.T) {
		want := "erased-" + strings.ReplaceAll(ivy.BundleID, "-", "")
		var label string
		if err := e.f.owner.QueryRow(ctx, `SELECT label FROM claims.bundles WHERE id=$1`, ivy.BundleID).Scan(&label); err != nil || label != want || len(label) != 39 {
			t.Fatalf("label %q (%v), want %q", label, err, want)
		}
		if !regexp.MustCompile(`^erased-[0-9a-f]{32}$`).MatchString(label) {
			t.Errorf("label shape %q", label)
		}
		var other string
		if err := e.f.owner.QueryRow(ctx, `SELECT label FROM claims.bundles WHERE id=$1`, collide.BundleID).Scan(&other); err != nil || other != "erased-"+hex8 {
			t.Errorf("the merchant's colliding label changed: %q %v", other, err)
		}
		if manualAfter := c.bundleDigest(`b.platform='manual'`); manualAfter != manualBefore {
			t.Errorf("a manual bundle column other than the label changed (bindings and actor keys are retained)")
		}
	})

	t.Run("actor-level data and other owners are untouched (CD7)", func(t *testing.T) {
		if got := c.bundleDigest(`b.actor_key=$1 OR b.platform<>'manual'`, actor); got != actorBefore {
			t.Error("bundles.actor_key rows changed")
		}
		if got := cbxOne(t, e.f, `SELECT md5(string_agg(m::text,E'\n' ORDER BY m.id)) FROM claims.meta_intake m`); got != intakeBefore || got == "" {
			t.Error("claims.meta_intake changed")
		}
		if got := cbxOne(t, e.f, `SELECT md5(string_agg(s::text,E'\n' ORDER BY s.id)) FROM live.claim_sources s`); got != sourceBefore || got == "" {
			t.Error("live.claim_sources changed")
		}
		if got := c.digest("social"); fmt.Sprint(got) != fmt.Sprint(metaBefore) {
			t.Error("social.* changed")
		}
		if got := c.bundleDigest(`b.owner_id=$1`, byID); got != bystanderBundleDigest {
			t.Error("the second owner bound to the same actor's bundle in another session changed")
		}
		if act, revoked, _ := c.ownerRow(byID); !act || revoked {
			t.Errorf("the bystander owner was deactivated or revoked (active=%v all revoked=%v)", act, revoked)
		}
		if !cbxAllows(t, e.f, tenant, store, byID, "marketing_messages", "meta_dm") {
			t.Error("the bystander's consent changed")
		}
		after := c.digest("checkout", "payments", "fulfillment", "inventory", "live", "social")
		lcSameDigest(t, "orders, payments, refunds, shipments and live data must stay byte-identical", ordersBefore, after)
		if n := e.count(t, `SELECT count(*) FROM checkout.orders WHERE owner_id=$1`, mainID); n != 1 {
			t.Errorf("the erased customer's order count is %d, want 1 (orders are retained)", n)
		}
		// the privileges and the function body agree: no write target among the actor-level tables
		body := cbxOne(t, e.f, `SELECT prosrc FROM pg_proc WHERE oid='customers.apply_erasure(uuid,uuid,uuid)'::regprocedure`)
		if regexp.MustCompile(`(?is)\b(update|insert\s+into|delete\s+from)\s+(claims\.meta_intake|live\.claim_sources|social\.[a-z_]+|claims\.links)`).MatchString(body) ||
			regexp.MustCompile(`(?is)\bset\b[^;]*\bactor_key\b`).MatchString(body) {
			t.Error("customers.apply_erasure writes an actor-level table or actor_key")
		}
	})

	t.Run("replay is idempotent; the tombstone is the ERASURE row", func(t *testing.T) {
		before := c.digest("buyer", "customers", "storefront", "claims")
		again, err := c.erase(tok, store, mainID, key)
		if err != nil || again != sum {
			t.Fatalf("replay with the same key: %+v %v, want the stored summary %+v", again, err, sum)
		}
		lcSameDigest(t, "an erasure replay changed state", before, c.digest("buyer", "customers", "storefront", "claims"))
		if n := e.count(t, `SELECT count(*) FROM customers.privacy_actions WHERE owner_id=$1 AND kind='ERASURE'`, mainID); n != 1 {
			t.Fatalf("%d ERASURE rows", n)
		}
		// a new key after the erasure never creates a second ERASURE (privacy_one_erasure)
		_, _ = c.erase(tok, store, mainID, t04Key("cbe-second"))
		if n := e.count(t, `SELECT count(*) FROM customers.privacy_actions WHERE owner_id=$1 AND kind='ERASURE'`, mainID); n != 1 {
			t.Fatalf("a second erasure key created %d ERASURE rows", n)
		}
		lcSameDigest(t, "a second-key erasure changed state", before, c.digest("buyer", "customers", "storefront", "claims"))
		// replay_erasures() re-applies every ERASURE row idempotently (ops/migration owner only)
		var n int
		if err := e.f.owner.QueryRow(ctx, `SELECT customers.replay_erasures()`).Scan(&n); err != nil || n < 1 {
			t.Fatalf("replay_erasures(): %d %v", n, err)
		}
		lcSameDigest(t, "replay_erasures() changed an already erased database", before, c.digest("buyer", "customers", "storefront", "claims"))
	})

	t.Run("buyer erasure (via buyer) and the retry after it is 410 erased", func(t *testing.T) {
		cp := sessionOwner.s.p.cap
		k := t04Key("cbe-buyer-erase")
		bsum, err := c.buyerErase(cp, sessionOwner.s.p.a.runtime, store, k)
		if err != nil {
			t.Fatalf("buyer erase: %v", err)
		}
		if bsum.ConsentsWithdrawn != 2 || bsum.SessionsRevoked < 1 || bsum.SnapshotsRedacted != 0 || bsum.BundlesRelabelled != 0 {
			t.Errorf("buyer summary %+v", bsum)
		}
		var via string
		if err := e.f.owner.QueryRow(ctx, `SELECT via FROM customers.privacy_actions WHERE owner_id=$1 AND kind='ERASURE'`, sessID).Scan(&via); err != nil || via != "buyer" {
			t.Errorf("ERASURE via=%q %v", via, err)
		}
		for name, key := range map[string]string{"same key": k, "new key": t04Key("cbe-buyer-retry")} {
			if _, err := c.buyerErase(cp, sessionOwner.s.p.a.runtime, store, key); !errors.Is(err, customers.ErrErased) {
				t.Errorf("buyer retry with the %s: %v, want ErrErased (PT410)", name, err)
			}
		}
		if err := buyer.WithScope(ctx, sessionOwner.s.p.a.runtime, cp.Token, store, func(context.Context, pgx.Tx, buyer.Scope) error { return nil }); err == nil {
			t.Error("a revoked capability still resolves")
		}
		// the merchant's replay still returns the stored summary and the customer stays erased
		if _, err := c.erase(tok, store, sessID, k); err != nil {
			t.Errorf("merchant replay of the buyer's erasure key: %v", err)
		}
		if act, revoked, _ := c.ownerRow(sessID); act || !revoked {
			t.Errorf("owner active=%v revoked=%v", act, revoked)
		}
	})

	t.Run("RESTORE MODEL (MODEL_ONLY until T20): replay_erasures without ids stays red, with the external ids goes green", func(t *testing.T) {
		fresh := mustIssue(t, main.s.p.cqHarness.service, store)
		frID := fresh.Scope.OwnerID
		c.put(main, fresh, "marketing_messages", "meta_dm")
		port := e.f.owner.Config().ConnConfig.Port
		ps, err := exec.Command("docker", "ps", "--format", "{{.Names}}|{{.Ports}}").Output()
		var name string
		for _, line := range strings.Split(strings.TrimSpace(string(ps)), "\n") {
			if n, ports, ok := strings.Cut(line, "|"); ok && strings.Contains(ports, fmt.Sprintf("127.0.0.1:%d->5432/tcp", port)) {
				name = n
			}
		}
		if err != nil || name == "" {
			t.Fatalf("cannot identify the test PG container for port %d: %v", port, err)
		}
		label, err := exec.Command("docker", "inspect", "-f", `{{index .Config.Labels "livecommerce.fixture"}}`, name).Output()
		if err != nil || strings.TrimSpace(string(label)) != name {
			t.Fatalf("refusing to touch an unlabelled container %s", name)
		}
		dump, err := exec.Command("docker", "exec", name, "pg_dump", "-U", "postgres", "-Fc", "lc_foundation_test").Output()
		if err != nil || len(dump) < 1000 {
			t.Fatalf("pg_dump: %d bytes %v", len(dump), err)
		}
		if _, err := c.erase(tok, store, frID, t04Key("cbe-restore-erase")); err != nil {
			t.Fatalf("erase of the restore subject: %v", err)
		}
		externalList := []string{frID} // the copy of erased owner ids kept outside the database backups (T20)
		if cbxAllows(t, e.f, tenant, store, frID, "marketing_messages", "meta_dm") {
			t.Fatal("live database still allows the erased subject")
		}
		db := "lc_restored_" + t04Tag()
		if out, err := exec.Command("docker", "exec", name, "createdb", "-U", "postgres", "-T", "template0", db).CombinedOutput(); err != nil {
			t.Fatalf("createdb: %v %s", err, out)
		}
		t.Cleanup(func() { _ = exec.Command("docker", "exec", name, "dropdb", "-U", "postgres", "--force", db).Run() })
		restore := exec.Command("docker", "exec", "-i", name, "pg_restore", "-U", "postgres", "--exit-on-error", "-d", db)
		restore.Stdin = bytes.NewReader(dump)
		if out, err := restore.CombinedOutput(); err != nil {
			t.Fatalf("pg_restore: %v %s", err, out)
		}
		u, err := url.Parse(e.f.databaseURL)
		if err != nil {
			t.Fatal(err)
		}
		u.Path = "/" + db
		restored, err := pgxpool.New(ctx, u.String())
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(restored.Close)
		state := func() (allowed, active bool, tombstones int, via string) {
			if err := restored.QueryRow(ctx, `SELECT customers.consent_allows($1,$2,$3,'marketing_messages','meta_dm'),
			  (SELECT active FROM buyer.owners WHERE id=$3),
			  (SELECT count(*) FROM customers.privacy_actions WHERE owner_id=$3 AND kind='ERASURE'),
			  coalesce((SELECT via FROM customers.privacy_actions WHERE owner_id=$3 AND kind='ERASURE'),'')`, tenant, store, frID).Scan(&allowed, &active, &tombstones, &via); err != nil {
				t.Fatal(err)
			}
			return
		}
		if allowed, active, tomb, _ := state(); !allowed || !active || tomb != 0 {
			t.Fatalf("the restored pre-erasure dump must show the subject granted and active: allowed=%v active=%v tombstones=%d", allowed, active, tomb)
		}
		// RED: without ids the replay only sees rows present in the dump, so the erasure is lost
		var n int
		if err := restored.QueryRow(ctx, `SELECT customers.replay_erasures()`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if allowed, active, tomb, _ := state(); !allowed || !active || tomb != 0 {
			t.Fatalf("RED expectation: replay_erasures() without ids must NOT restore an erasure made after the dump (allowed=%v active=%v tombstones=%d, replayed %d)", allowed, active, tomb, n)
		}
		// GREEN: the ids of the external list restore it
		if err := restored.QueryRow(ctx, `SELECT customers.replay_erasures($1::uuid[])`, externalList).Scan(&n); err != nil || n != 1 {
			t.Fatalf("replay_erasures(ids): %d %v", n, err)
		}
		allowed, active, tomb, via := state()
		if allowed || active || tomb != 1 || via != "restore" {
			t.Fatalf("after replay with ids: allowed=%v active=%v tombstones=%d via=%q, want false,false,1,restore", allowed, active, tomb, via)
		}
		var unrevoked int
		if err := restored.QueryRow(ctx, `SELECT count(*) FROM buyer.capability_sessions WHERE owner_id=$1 AND revoked_at IS NULL`, frID).Scan(&unrevoked); err != nil || unrevoked != 0 {
			t.Errorf("restored subject still has %d live capability sessions", unrevoked)
		}
		// idempotent: a second replay changes nothing
		if err := restored.QueryRow(ctx, `SELECT customers.replay_erasures($1::uuid[])`, externalList).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if _, _, tomb2, _ := state(); tomb2 != 1 {
			t.Errorf("a second replay made %d tombstones", tomb2)
		}
	})
}
