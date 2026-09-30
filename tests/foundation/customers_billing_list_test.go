package foundation_test

// CB03 (contracts/customers-billing-v1.md §0 CD1-CD3, §3.1 read_merchant_customers / read_finance_summary,
// §5 customers + finance rows, §7, §8; tier REAL_PG over the real capture and refund paths of the rfx harness).
// Written from the contract and the FROZEN blocks + D5/D6/D7/D13 of customers-core.md only. Helper prefix `cbl`.
// What it proves:
//   - two tenants, two stores, four owners; the store scope is the token's: a member sees only its store's
//     customers, another store's customer id is "not found" (indistinguishable), a member without
//     customers:read is forbidden;
//   - the forwarded link (G09 case 1): Alice's bundle redeemed by Bob's owner counts under Bob only, creates
//     no merge (customer count unchanged, Alice's owner has no claim);
//   - aggregates equal the facts: paid orders / captured from payments.facts, refunded from refund_facts with a
//     SUCCEEDED-then-FAILED refund excluded (also equal to the existing order projection's refunded_minor);
//   - no actor_key, capability session id, token hash or PSP reference in any output, and the list carries the
//     phone only as its last three digits;
//   - keyset paging is stable on equal timestamps, complete, and refuses a foreign or corrupted cursor;
//   - q: phone-digit suffix (separators ignored) and case-insensitive name prefix, bounds 1..40;
//   - finance (BD7): daily rows in Asia/Taipei, later-failed refunds excluded, totals, range bound 0..91,
//     CSV header/columns; the UTC+8 midnight boundary with aged facts.
// Disclosed owner-pool fixtures: order-snapshot destination names/phones (distinct search keys), grants of the
// merchant principals, claims.bundles.bound_at made equal (equal-timestamp keyset), and payments.facts /
// payments.refund_facts received_at aged with triggers off (session_replication_role=replica) for the
// midnight-boundary case only. Orders, payments and refunds come from the real paths.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"livecommerce/internal/buyer"
	"livecommerce/internal/claims"
	"livecommerce/internal/command"
	"livecommerce/internal/customers"
	"livecommerce/internal/pagination"
	"livecommerce/internal/platform"
	"livecommerce/internal/reporting"
)

type cblEnv struct {
	t   *testing.T
	e   *rfxEnv
	ctx context.Context
}

func (c *cblEnv) list(token, store string, req customers.ListRequest) (page pagination.Page[customers.Customer], err error) {
	err = platform.WithScope(c.ctx, c.e.f.runtime, token, store, "store:read", func(tx pgx.Tx, s platform.Scope) (e error) {
		page, e = customers.List(c.ctx, tx, s, token, req)
		return e
	})
	return page, err
}

func (c *cblEnv) get(token, store, id string) (d customers.Detail, err error) {
	err = platform.WithScope(c.ctx, c.e.f.runtime, token, store, "store:read", func(tx pgx.Tx, s platform.Scope) (e error) {
		d, e = customers.Get(c.ctx, tx, s, token, id)
		return e
	})
	return d, err
}

func (c *cblEnv) all(token, store, q string) []customers.Customer {
	c.t.Helper()
	var out []customers.Customer
	cursor := ""
	for i := 0; i < 50; i++ {
		p, err := c.list(token, store, customers.ListRequest{Page: pagination.Request{Limit: 100, Cursor: cursor}, Q: q})
		if err != nil {
			c.t.Fatalf("list: %v", err)
		}
		out = append(out, p.Items...)
		if p.NextCursor == "" {
			return out
		}
		cursor = p.NextCursor
	}
	c.t.Fatal("list did not terminate")
	return nil
}

func cblByID(list []customers.Customer, id string) *customers.Customer {
	for i := range list {
		if list[i].CustomerID == id {
			return &list[i]
		}
	}
	return nil
}

// cblClaims builds an lcHarness over the isolated database's store of o (real claims path).
func cblClaims(t *testing.T, o rfxOrder) *lcHarness {
	t.Helper()
	p := o.s.p
	f := p.f
	h := &lcHarness{cqHarness: p.cqHarness, ctx: context.Background()}
	h.actor, h.token = lcPrincipal(t, f, f.tenantA, []string{f.storeA1}, "store:read", "live:read", "live:manage")
	var err error
	if h.labels, err = claims.NewLabelKey(randomBytes(32)); err != nil {
		t.Fatal(err)
	}
	return h
}

func TestCustomersBillingCB03List(t *testing.T) {
	e := rfxNew(t)
	c := &cblEnv{t: t, e: e, ctx: context.Background()}
	ctx := c.ctx
	stop := e.startWorker(t)
	defer stop()

	// ---- fixtures through the real paths ----
	a := e.stripeStore(t)
	endpoint, secret := e.endpoint(t, a)
	alice := e.pay(t, a, endpoint, secret)
	bob := e.payMore(t, alice)
	carolP := sstMoreHold(t, alice.s.p) // an order that was placed and never paid
	b := e.stripeStore(t)
	endpoint2, secret2 := e.endpoint(t, b)
	dave := e.pay(t, b, endpoint2, secret2)
	for _, o := range []rfxOrder{alice, bob, dave} {
		e.grant(t, o, "orders:read", "customers:read", "customers:privacy", "orders:export", "payments:refund")
	}
	ownerOf := func(o rfxOrder) string { return o.s.p.cap.Scope.OwnerID }
	aliceID, bobID, carolID, daveID := ownerOf(alice), ownerOf(bob), carolP.cap.Scope.OwnerID, ownerOf(dave)
	s1, s2 := alice.store(), dave.store()
	tok1, tok2 := alice.token(), dave.token()
	// distinct search keys (disclosed fixture: the list reads the frozen destination inside checkout.orders.snapshot)
	for id, kv := range map[string][2]string{aliceID: {"Alice Test", "0911000111"}, bobID: {"Bob Test", "0922000222"}, carolID: {"Carol Test", "0933000333"}, daveID: {"Dave Other", "0944000444"}} {
		mustExec(t, e.f.owner, `UPDATE checkout.orders SET snapshot=jsonb_set(jsonb_set(snapshot,'{destination,recipient_name}',to_jsonb($2::text)),'{destination,phone}',to_jsonb($3::text)) WHERE owner_id=$1`, id, kv[0], kv[1])
	}
	// Bob: one refund that succeeded and then failed (excluded), one that succeeded (counted)
	failed := e.mustRefund(t, bob, 1000, "requested_by_customer")
	e.awaitRefundFact(t, failed, bob.attempt, "SUCCEEDED")
	fakeID := e.fake.RefundByRef(failed)
	e.fake.SetRefundStatus(fakeID, "failed", "declined")
	if st := e.deliverRaw(t, bob.endpoint, bob.secret, e.fake.RefundEventBody("evt_cbl_failed_"+t04Tag(), "refund.failed", fakeID, false)); st != 200 {
		t.Fatalf("refund.failed webhook answered %d", st)
	}
	e.awaitRefundFact(t, failed, bob.attempt, "FAILED")
	kept := e.mustRefund(t, bob, 500, "requested_by_customer")
	e.awaitRefundFact(t, kept, bob.attempt, "SUCCEEDED")
	// claims: Alice-the-commenter's bundle is redeemed by BOB's owner (forwarded link, G09 case 1)
	h := cblClaims(t, alice)
	skus := lcSKUs(t, h.f, h.f.tenantA, h.f.storeA1, "TWD", 1)
	session := h.draft(t, s1)
	h.offer(t, session, "A1", skus[0], 5)
	h.open(t, session, claims.MatchExact)
	forwarded := h.accepted(t, session, "", "alice-the-commenter", "A1+2")
	link := h.link(t, session, forwarded.BundleID, 0, false)
	if _, err := h.redeem(bob.s.p.cap, t04Key("cbl-redeem"), link.Token, forwarded.BundleVersion); err != nil {
		t.Fatalf("Bob's owner redeeming the forwarded link: %v", err)
	}
	h.closeWindow(t, session) // one OPEN window per store: later sessions open their own
	// Alice grants marketing consent as a buyer (list shows it)
	c.grant(alice, "marketing_messages", "meta_dm")

	before := len(c.all(tok1, s1, ""))

	t.Run("scope: own store only, cross-store is not found, no permission is forbidden", func(t *testing.T) {
		l1, l2 := c.all(tok1, s1, ""), c.all(tok2, s2, "")
		ids1, ids2 := map[string]bool{}, map[string]bool{}
		for _, x := range l1 {
			ids1[x.CustomerID] = true
		}
		for _, x := range l2 {
			ids2[x.CustomerID] = true
		}
		if !ids1[aliceID] || !ids1[bobID] || !ids1[carolID] || ids1[daveID] || len(l1) != 3 {
			t.Fatalf("store 1 customers %v, want exactly alice, bob, carol", ids1)
		}
		if !ids2[daveID] || len(l2) != 1 {
			t.Fatalf("store 2 customers %v, want exactly dave", ids2)
		}
		if _, err := c.get(tok2, s2, aliceID); !cbxNotFound(err) {
			t.Errorf("another tenant's customer id: %v, want the not-found class", err)
		}
		if _, err := c.get(tok1, s1, randomUUID()); !cbxNotFound(err) {
			t.Errorf("unknown customer id: %v, want the not-found class", err)
		}
		_, e1 := c.get(tok2, s2, aliceID)
		_, e2 := c.get(tok1, s1, randomUUID())
		if fmt.Sprint(e1) != fmt.Sprint(e2) {
			t.Errorf("cross-store and unknown answers differ: %q vs %q", e1, e2)
		}
		noPerm, _ := e.member(t, alice, "orders:read")
		if _, err := c.list(noPerm, s1, customers.ListRequest{}); !errors.Is(err, platform.ErrForbidden) {
			t.Errorf("member without customers:read: %v, want ErrForbidden", err)
		}
		if _, err := c.list(tok2, s1, customers.ListRequest{}); err == nil {
			t.Error("another tenant's token listed store 1")
		}
	})

	t.Run("forwarded link counts under Bob only and creates no merge (G09 case 1)", func(t *testing.T) {
		l := c.all(tok1, s1, "")
		al, bo := cblByID(l, aliceID), cblByID(l, bobID)
		if len(l) != before || al == nil || bo == nil || al.CustomerID == bo.CustomerID {
			t.Fatalf("customers merged or lost: %d -> %d", before, len(l))
		}
		if bo.ClaimsCount != 1 || strings.Join(bo.Platforms, ",") != "manual" || al.ClaimsCount != 0 || len(al.Platforms) != 0 {
			t.Fatalf("claims: bob=%d %v alice=%d %v", bo.ClaimsCount, bo.Platforms, al.ClaimsCount, al.Platforms)
		}
		bd, err := c.get(tok1, s1, bobID)
		if err != nil || len(bd.Claims) != 1 || bd.Claims[0].SessionID != session || bd.Claims[0].Platform != "manual" || bd.Claims[0].BoundAt == "" || bd.Claims[0].LineCount < 1 {
			t.Fatalf("bob detail claims: %+v %v", bd.Claims, err)
		}
		ad, err := c.get(tok1, s1, aliceID)
		if err != nil || len(ad.Claims) != 0 || len(ad.Orders) != 1 || ad.Orders[0].OrderID != alice.order {
			t.Fatalf("alice detail: claims=%d orders=%+v %v", len(ad.Claims), ad.Orders, err)
		}
		for _, o := range bd.Orders {
			if o.OrderID == alice.order {
				t.Fatal("Bob's detail lists Alice's order: identities were merged")
			}
		}
	})

	t.Run("aggregates equal the facts (captured, refunded, later-failed refund excluded)", func(t *testing.T) {
		l := c.all(tok1, s1, "")
		type want struct{ orders, paid, captured, refunded int64 }
		for name, w := range map[string]struct {
			id string
			w  want
		}{"alice": {aliceID, want{1, 1, 2500, 0}}, "bob": {bobID, want{1, 1, 2500, 500}}, "carol": {carolID, want{1, 0, 0, 0}}} {
			cu := cblByID(l, w.id)
			if cu == nil {
				t.Fatalf("%s missing", name)
			}
			if cu.OrdersCount != w.w.orders || cu.PaidOrdersCount != w.w.paid || cu.CapturedMinor != w.w.captured || cu.RefundedMinor != w.w.refunded {
				t.Errorf("%s: orders=%d paid=%d captured=%d refunded=%d, want %+v", name, cu.OrdersCount, cu.PaidOrdersCount, cu.CapturedMinor, cu.RefundedMinor, w.w)
			}
			// independent oracle 1: the facts themselves
			var orders, paid, captured, refunded int64
			if err := e.f.owner.QueryRow(ctx, `
			 SELECT (SELECT count(*) FROM checkout.orders WHERE owner_id=$1),
			  (SELECT count(*) FROM checkout.orders o WHERE o.owner_id=$1 AND EXISTS(SELECT 1 FROM payments.facts f JOIN checkout.payment_attempts a ON a.id=f.attempt_id WHERE a.order_id=o.id AND f.kind='CAPTURED')),
			  coalesce((SELECT sum(f.amount_minor) FROM payments.facts f JOIN checkout.payment_attempts a ON a.id=f.attempt_id JOIN checkout.orders o ON o.id=a.order_id WHERE o.owner_id=$1 AND f.kind='CAPTURED'),0),
			  coalesce((SELECT sum(rf.amount_minor) FROM payments.refund_facts rf JOIN payments.stripe_refunds r ON r.id=rf.refund_id WHERE r.owner_id=$1 AND rf.kind='SUCCEEDED'
			    AND NOT EXISTS(SELECT 1 FROM payments.refund_facts x WHERE x.refund_id=rf.refund_id AND x.kind IN ('FAILED','CANCELED'))),0)`, w.id).Scan(&orders, &paid, &captured, &refunded); err != nil {
				t.Fatalf("oracle %s: %v", name, err)
			}
			if cu.OrdersCount != orders || cu.PaidOrdersCount != paid || cu.CapturedMinor != captured || cu.RefundedMinor != refunded {
				t.Errorf("%s list %d/%d/%d/%d != facts %d/%d/%d/%d", name, cu.OrdersCount, cu.PaidOrdersCount, cu.CapturedMinor, cu.RefundedMinor, orders, paid, captured, refunded)
			}
			// independent oracle 2: the existing merchant-order projection of the same orders
			d, err := c.get(tok1, s1, w.id)
			if err != nil {
				t.Fatal(err)
			}
			var projected int64
			for _, o := range d.Orders {
				projected += o.RefundedMinor
			}
			if projected != cu.RefundedMinor || int64(len(d.Orders)) != cu.OrdersCount {
				t.Errorf("%s: order projection refunded=%d orders=%d vs list refunded=%d orders=%d (export must equal the order page)", name, projected, len(d.Orders), cu.RefundedMinor, cu.OrdersCount)
			}
		}
		bo := cblByID(l, bobID)
		if bo.Currency == nil || *bo.Currency != "TWD" || !bo.Active {
			t.Errorf("bob currency/active: %v %v", bo.Currency, bo.Active)
		}
		if ca := cblByID(l, carolID); ca.Currency == nil || ca.DisplayName == nil || *ca.DisplayName != "Carol Test" || ca.PhoneLast3 == nil || *ca.PhoneLast3 != "333" {
			t.Errorf("carol display/phone: name=%v last3=%v currency=%v", ca.DisplayName, ca.PhoneLast3, ca.Currency)
		}
		if al := cblByID(l, aliceID); !al.Consents.MarketingMessages || al.Consents.AdsPersonalization {
			t.Errorf("alice consents: %+v", al.Consents)
		}
		// D7: first_seen_at = owners.created_at; last_activity_at = greatest(latest order, latest bound bundle)
		for _, id := range []string{aliceID, bobID} {
			cu := cblByID(l, id)
			var first, last string
			if err := e.f.owner.QueryRow(ctx, `SELECT to_char(o.created_at AT TIME ZONE 'UTC','YYYY-MM-DD"T"HH24:MI:SS.US"Z"'),
			  to_char(greatest((SELECT max(created_at) FROM checkout.orders WHERE owner_id=o.id),(SELECT max(bound_at) FROM claims.bundles WHERE owner_id=o.id)) AT TIME ZONE 'UTC','YYYY-MM-DD"T"HH24:MI:SS.US"Z"')
			  FROM buyer.owners o WHERE o.id=$1`, id).Scan(&first, &last); err != nil {
				t.Fatal(err)
			}
			if cu.FirstSeenAt != first || cu.LastActivityAt != last {
				t.Errorf("%s first/last %s/%s, want %s/%s (D7)", id, cu.FirstSeenAt, cu.LastActivityAt, first, last)
			}
		}
	})

	t.Run("no actor key, capability session, token hash or PSP reference in any output; phone is last 3 only", func(t *testing.T) {
		var sentinels []string
		for _, q := range []string{
			`SELECT actor_key FROM claims.bundles`,
			`SELECT id::text FROM buyer.capability_sessions`,
			`SELECT encode(token_hash,'hex') FROM buyer.capability_sessions`,
			`SELECT session_id FROM payments.stripe_sessions WHERE session_id IS NOT NULL`,
			`SELECT payment_intent_id FROM payments.stripe_sessions WHERE payment_intent_id IS NOT NULL`,
			`SELECT stripe_refund_id FROM payments.refund_facts WHERE stripe_refund_id IS NOT NULL`,
			`SELECT account_id FROM integration.merchant_accounts`,
			`SELECT provider_reference FROM payments.facts`,
			`SELECT semantic_key FROM integration.operations WHERE semantic_key IS NOT NULL`,
		} {
			rows, err := e.f.owner.Query(ctx, q)
			if err != nil {
				t.Fatalf("%s: %v", q, err)
			}
			for rows.Next() {
				var s string
				if err := rows.Scan(&s); err != nil {
					t.Fatal(err)
				}
				if len(s) >= 8 {
					sentinels = append(sentinels, s)
				}
			}
			rows.Close()
		}
		if len(sentinels) < 10 {
			t.Fatalf("only %d sentinels gathered: the scan would be vacuous", len(sentinels))
		}
		var outputs []string
		for _, id := range []string{aliceID, bobID, carolID} {
			d, err := c.get(tok1, s1, id)
			if err != nil {
				t.Fatal(err)
			}
			raw, _ := json.Marshal(d)
			outputs = append(outputs, string(raw))
		}
		raw, _ := json.Marshal(c.all(tok1, s1, ""))
		listOut := string(raw)
		outputs = append(outputs, listOut)
		// the raw definer output too (before the Go projection)
		var rawSQL string
		if err := platform.WithScope(ctx, e.f.runtime, tok1, s1, "store:read", func(tx pgx.Tx, sc platform.Scope) error {
			return tx.QueryRow(ctx, `SELECT identity.read_merchant_customers(sha256(convert_to($1,'UTF8')),$2::uuid,NULL,101,NULL,NULL,NULL)::text`, tok1, s1).Scan(&rawSQL)
		}); err != nil {
			t.Fatalf("raw definer: %v", err)
		}
		outputs = append(outputs, rawSQL)
		pat := regexp.MustCompile(`\b(cs|pi|re|acct|evt|whsec|sk|rk)_[A-Za-z0-9]{6,}`)
		for i, out := range outputs {
			for _, s := range sentinels {
				if strings.Contains(out, s) {
					t.Errorf("output %d leaks %q", i, s)
				}
			}
			if m := pat.FindString(out); m != "" {
				t.Errorf("output %d holds a PSP-shaped token %q", i, m)
			}
		}
		for _, phone := range []string{"0911000111", "0922000222", "0933000333", "911000111"} {
			if strings.Contains(listOut, phone) {
				t.Errorf("the list carries a full phone number %s (I11: last 3 digits only)", phone)
			}
		}
	})

	t.Run("keyset paging is stable on equal timestamps and refuses foreign cursors", func(t *testing.T) {
		// six bundle-only customers whose last_activity_at is identical (disclosed: bound_at set equal)
		var extra []string
		for i := 0; i < 6; i++ {
			s := h.draft(t, s1)
			h.offer(t, s, "A1", skus[0], 5)
			h.open(t, s, claims.MatchExact)
			r := h.accepted(t, s, "", fmt.Sprintf("tie-%d", i), "A1+1")
			l := h.link(t, s, r.BundleID, 0, false)
			cp := mustIssue(t, h.cqHarness.service, s1)
			if _, err := h.redeem(cp, t04Key("cbl-tie"), l.Token, r.BundleVersion); err != nil {
				t.Fatal(err)
			}
			extra = append(extra, cp.Scope.OwnerID)
			h.closeWindow(t, s)
		}
		mustExec(t, e.f.owner, `UPDATE claims.bundles SET bound_at=timestamptz '2026-03-01 10:00:00+00' WHERE owner_id=ANY($1::uuid[])`, extra)
		var ordered []string
		cursor := ""
		seen := map[string]int{}
		var pages int
		for {
			p, err := c.list(tok1, s1, customers.ListRequest{Page: pagination.Request{Limit: 2, Cursor: cursor}})
			if err != nil {
				t.Fatal(err)
			}
			pages++
			if len(p.Items) > 2 {
				t.Fatalf("page of %d for limit 2", len(p.Items))
			}
			for _, it := range p.Items {
				seen[it.CustomerID]++
				ordered = append(ordered, it.CustomerID)
			}
			if pages == 2 {
				// a customer with newer activity appears between page 2 and page 3: paging must neither repeat nor skip
				if _, err := c.list(tok1, s1, customers.ListRequest{Page: pagination.Request{Limit: 2, Cursor: p.NextCursor}}); err != nil {
					t.Fatal(err)
				}
			}
			if p.NextCursor == "" {
				break
			}
			cursor = p.NextCursor
			if pages > 20 {
				t.Fatal("paging did not terminate")
			}
		}
		total := 3 + len(extra)
		if len(seen) != total || len(ordered) != total {
			t.Fatalf("paged %d items (%d distinct), want %d each", len(ordered), len(seen), total)
		}
		for id, n := range seen {
			if n != 1 {
				t.Errorf("%s appeared %d times", id, n)
			}
		}
		full := c.all(tok1, s1, "")
		var byFull []string
		for _, x := range full {
			byFull = append(byFull, x.CustomerID)
		}
		if strings.Join(byFull, ",") != strings.Join(ordered, ",") {
			t.Errorf("limit-2 paging order differs from the single-page order")
		}
		// (last_activity_at DESC, id DESC): the six tied customers are ordered by id descending, contiguous
		var tied []string
		for _, x := range full {
			for _, id := range extra {
				if x.CustomerID == id {
					tied = append(tied, id)
				}
			}
		}
		want := append([]string(nil), extra...)
		sort.Sort(sort.Reverse(sort.StringSlice(want)))
		if strings.Join(tied, ",") != strings.Join(want, ",") {
			t.Errorf("tied customers order %v, want id DESC %v", tied, want)
		}
		for i := 1; i < len(full); i++ {
			if full[i-1].LastActivityAt < full[i].LastActivityAt {
				t.Errorf("last_activity_at not descending at %d: %s < %s", i, full[i-1].LastActivityAt, full[i].LastActivityAt)
			}
		}
		// a page walked while a newer customer is inserted between pages keeps its position
		first, err := c.list(tok1, s1, customers.ListRequest{Page: pagination.Request{Limit: 2}})
		if err != nil || first.NextCursor == "" {
			t.Fatalf("first page: %v %+v", err, first)
		}
		fresh := mustIssue(t, h.cqHarness.service, s1)
		s := h.draft(t, s1)
		h.offer(t, s, "A1", skus[0], 5)
		h.open(t, s, claims.MatchExact)
		r := h.accepted(t, s, "", "late", "A1+1")
		l := h.link(t, s, r.BundleID, 0, false)
		if _, err := h.redeem(fresh, t04Key("cbl-late"), l.Token, r.BundleVersion); err != nil {
			t.Fatal(err)
		}
		h.closeWindow(t, s)
		second, err := c.list(tok1, s1, customers.ListRequest{Page: pagination.Request{Limit: 2, Cursor: first.NextCursor}})
		if err != nil {
			t.Fatal(err)
		}
		for _, it := range second.Items {
			for _, prev := range first.Items {
				if it.CustomerID == prev.CustomerID {
					t.Errorf("an insert between pages repeated %s", it.CustomerID)
				}
			}
		}
		// cursors are bound: foreign store, tampered, and a filter change are refused
		for name, cur := range map[string]string{"garbage": "!!!", "truncated": first.NextCursor[:len(first.NextCursor)/2], "oversized": strings.Repeat("a", 2000)} {
			if _, err := c.list(tok1, s1, customers.ListRequest{Page: pagination.Request{Limit: 2, Cursor: cur}}); !errors.Is(err, command.ErrInvalid) {
				t.Errorf("%s cursor: %v, want ErrInvalid", name, err)
			}
		}
		if _, err := c.list(tok2, s2, customers.ListRequest{Page: pagination.Request{Limit: 2, Cursor: first.NextCursor}}); !errors.Is(err, command.ErrInvalid) {
			t.Errorf("a cursor from another store: %v, want ErrInvalid", err)
		}
		if _, err := c.list(tok1, s1, customers.ListRequest{Page: pagination.Request{Limit: 2, Cursor: first.NextCursor}, Q: "Alice"}); !errors.Is(err, command.ErrInvalid) {
			t.Errorf("a cursor reused under another q: %v, want ErrInvalid", err)
		}
		// limits: 0 = default 50, 101 refused
		if _, err := c.list(tok1, s1, customers.ListRequest{Page: pagination.Request{Limit: 101}}); !errors.Is(err, command.ErrInvalid) {
			t.Errorf("limit 101: %v, want ErrInvalid (list limit <= 100, §7)", err)
		}
		if p, err := c.list(tok1, s1, customers.ListRequest{}); err != nil || len(p.Items) == 0 {
			t.Errorf("default limit: %v", err)
		}
	})

	t.Run("q: phone-digit suffix, case-insensitive name prefix, bounds", func(t *testing.T) {
		ids := func(q string) string {
			var got []string
			for _, x := range c.all(tok1, s1, q) {
				got = append(got, x.CustomerID)
			}
			sort.Strings(got)
			return strings.Join(got, ",")
		}
		for name, tc := range map[string]struct{ q, want string }{
			"phone suffix":          {"222", bobID},
			"phone suffix 4":        {"0222", bobID},
			"full phone":            {"0922000222", bobID},
			"separators ignored":    {"0922-000-222", bobID},
			"plus and spaces":       {"+ 0922 000 222", bobID},
			"name prefix":           {"Ali", aliceID},
			"name prefix upper":     {"ALI", aliceID},
			"name prefix lower":     {"carol", carolID},
			"whole name":            {"Bob Test", bobID},
			"not a prefix":          {"Test", ""},
			"a middle digit run":    {"000", ""},
			"another store's phone": {"0944000444", ""},
			"another store's name":  {"Dave", ""},
		} {
			if got := ids(tc.q); got != tc.want {
				t.Errorf("%s (%q): %q, want %q", name, tc.q, got, tc.want)
			}
		}
		if ids("Dave") != "" || len(c.all(tok2, s2, "Dave")) != 1 {
			t.Error("q is scoped to the token's store")
		}
		for name, q := range map[string]string{"whitespace only": "   ", "41 chars": strings.Repeat("a", 41), "control char": "a\x00b", "tab inside": "a\tb"} {
			if _, err := c.list(tok1, s1, customers.ListRequest{Q: q}); !errors.Is(err, command.ErrInvalid) {
				t.Errorf("%s: %v, want ErrInvalid (q is 1..40 chars, D6)", name, err)
			}
		}
		if _, err := c.list(tok1, s1, customers.ListRequest{Q: strings.Repeat("a", 40)}); err != nil {
			t.Errorf("40 chars is allowed: %v", err)
		}
		// a % or _ is text, not a LIKE wildcard
		for _, q := range []string{"%", "_", "A%", "%ce"} {
			if got := ids(q); got != "" {
				t.Errorf("q=%q matched %q (LIKE metacharacters must be literal)", q, got)
			}
		}
	})

	t.Run("finance (BD7): Asia/Taipei days, later-failed refunds excluded, totals, bounds, CSV", func(t *testing.T) {
		fin := func(token, store, from, to string) (reporting.FinanceSummary, error) {
			var out reporting.FinanceSummary
			err := platform.WithScope(ctx, e.f.runtime, token, store, "store:read", func(tx pgx.Tx, s platform.Scope) (er error) {
				out, er = reporting.Finance(ctx, tx, s, token, from, to)
				return er
			})
			return out, err
		}
		today := time.Now().In(time.FixedZone("Asia/Taipei", 8*3600)).Format("2006-01-02")
		sum, err := fin(tok1, s1, today, today)
		if err != nil {
			t.Fatal(err)
		}
		if len(sum.Rows) != 1 || sum.Timezone != "Asia/Taipei" || sum.From != today || sum.To != today {
			t.Fatalf("finance summary: %+v", sum)
		}
		r := sum.Rows[0]
		if r.Day != today || r.Currency != "TWD" || r.Environment != "SANDBOX" || r.CapturedCount != 2 || r.CapturedMinor != 5000 || r.RefundedMinor != 500 || r.NetMinor != 4500 {
			t.Fatalf("row %+v, want 2 captured / 5000 / refunded 500 (the later-failed 1000 excluded) / net 4500", r)
		}
		if len(sum.Totals) != 1 || sum.Totals[0].Day != "" || sum.Totals[0].CapturedMinor != 5000 || sum.Totals[0].NetMinor != 4500 || sum.Totals[0].CapturedCount != 2 {
			t.Fatalf("totals %+v", sum.Totals)
		}
		// store scope
		s2sum, err := fin(tok2, s2, today, today)
		if err != nil || len(s2sum.Rows) != 1 || s2sum.Rows[0].CapturedMinor != 2500 || s2sum.Rows[0].RefundedMinor != 0 {
			t.Fatalf("store 2 finance: %+v %v", s2sum, err)
		}
		// bounds: 0..91 days
		if _, err := fin(tok1, s1, "2026-01-01", "2026-04-02"); err != nil { // 91 days
			t.Errorf("a 91-day range must be accepted: %v", err)
		}
		if _, err := fin(tok1, s1, "2026-01-01", "2026-04-03"); !errors.Is(err, command.ErrInvalid) { // 92 days
			t.Errorf("a 92-day range: %v, want ErrInvalid", err)
		}
		if _, err := fin(tok1, s1, "2026-02-01", "2026-01-01"); !errors.Is(err, command.ErrInvalid) {
			t.Errorf("to before from: %v, want ErrInvalid", err)
		}
		for _, bad := range [][2]string{{"", today}, {today, ""}, {"2026-1-1", today}, {today, "20260101"}, {"2026-02-30", today}} {
			if _, err := fin(tok1, s1, bad[0], bad[1]); !errors.Is(err, command.ErrInvalid) {
				t.Errorf("dates %q..%q: %v, want ErrInvalid", bad[0], bad[1], err)
			}
		}
		none, err := fin(tok1, s1, "2020-01-01", "2020-01-05")
		if err != nil || len(none.Rows) != 0 || len(none.Totals) != 0 {
			t.Errorf("an empty range: %+v %v", none, err)
		}
		// UTC+8 boundary (disclosed fixture): Alice's capture is 23:30 and Bob's 00:30 Taipei around 2026-01-16;
		// Bob's kept refund succeeds on 2026-01-17 01:00 Taipei, his later-failed refund "succeeded" on the 16th.
		age := func(q string, args ...any) {
			tx, err := e.f.owner.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback(ctx)
			if _, err := tx.Exec(ctx, `SET LOCAL session_replication_role = replica`); err != nil {
				t.Fatal(err)
			}
			if _, err := tx.Exec(ctx, q, args...); err != nil {
				t.Fatal(err)
			}
			if err := tx.Commit(ctx); err != nil {
				t.Fatal(err)
			}
		}
		age(`UPDATE payments.facts SET received_at=timestamptz '2026-01-15 15:30:00+00' WHERE attempt_id=$1 AND kind='CAPTURED'`, alice.attempt)
		age(`UPDATE payments.facts SET received_at=timestamptz '2026-01-15 16:30:00+00' WHERE attempt_id=$1 AND kind='CAPTURED'`, bob.attempt)
		age(`UPDATE payments.refund_facts SET received_at=timestamptz '2026-01-16 16:00:00+00' WHERE refund_id=$1 AND kind='SUCCEEDED'`, kept)
		age(`UPDATE payments.refund_facts SET received_at=timestamptz '2026-01-16 01:00:00+00' WHERE refund_id=$1 AND kind='SUCCEEDED'`, failed)
		edge, err := fin(tok1, s1, "2026-01-15", "2026-01-18")
		if err != nil {
			t.Fatal(err)
		}
		days := map[string]reporting.FinanceRow{}
		for _, r := range edge.Rows {
			days[r.Day] = r
		}
		if len(edge.Rows) != 3 || days["2026-01-15"].CapturedMinor != 2500 || days["2026-01-15"].CapturedCount != 1 || days["2026-01-15"].NetMinor != 2500 ||
			days["2026-01-16"].CapturedMinor != 2500 || days["2026-01-16"].RefundedMinor != 0 || days["2026-01-16"].NetMinor != 2500 ||
			days["2026-01-17"].CapturedCount != 0 || days["2026-01-17"].RefundedMinor != 500 || days["2026-01-17"].NetMinor != -500 {
			t.Fatalf("UTC+8 day boundaries: %+v (23:30 Taipei belongs to the 15th, 00:30 to the 16th, a refund counts on its SUCCEEDED day, a later-failed one never)", edge.Rows)
		}
		if len(edge.Totals) != 1 || edge.Totals[0].CapturedMinor != 5000 || edge.Totals[0].RefundedMinor != 500 || edge.Totals[0].NetMinor != 4500 {
			t.Fatalf("totals over the range: %+v", edge.Totals)
		}
		if one, err := fin(tok1, s1, "2026-01-15", "2026-01-15"); err != nil || len(one.Rows) != 1 || one.Rows[0].CapturedMinor != 2500 {
			t.Fatalf("a one-day range (from == to): %+v %v", one, err)
		}
		// CSV: header, columns, rows equal the summary, export needs orders:export and is audited
		csv := func(token string) (out []byte, err error) {
			err = platform.WithScope(ctx, e.f.runtime, token, s1, "store:read", func(tx pgx.Tx, s platform.Scope) (er error) {
				out, er = reporting.FinanceCSV(ctx, tx, s, token, "2026-01-15", "2026-01-18")
				return er
			})
			return out, err
		}
		auditsBefore := countRows(t, e.f.owner, `SELECT count(*) FROM ops.audit_events WHERE store_id=$1 AND action='finance.exported'`, s1)
		raw, err := csv(tok1)
		if err != nil {
			t.Fatalf("csv: %v", err)
		}
		lines := strings.Split(strings.TrimRight(string(raw), "\r\n"), "\n")
		if lines[0] != "day,currency,environment,captured_count,captured_minor,refunded_minor,net_minor" {
			t.Fatalf("csv header %q", lines[0])
		}
		csvDays := map[string]string{}
		for _, l := range lines[1:] {
			f := strings.Split(strings.TrimSpace(l), ",")
			if len(f) != 7 {
				t.Fatalf("csv row %q has %d columns", l, len(f))
			}
			csvDays[f[0]] = f[3] + "/" + f[4] + "/" + f[5] + "/" + f[6]
		}
		for day, r := range days {
			if got := csvDays[day]; got != fmt.Sprintf("%d/%d/%d/%d", r.CapturedCount, r.CapturedMinor, r.RefundedMinor, r.NetMinor) {
				t.Errorf("csv day %s = %q, summary says %+v", day, got, r)
			}
		}
		if got := countRows(t, e.f.owner, `SELECT count(*) FROM ops.audit_events WHERE store_id=$1 AND action='finance.exported'`, s1); got != auditsBefore+1 {
			t.Errorf("finance.exported audit rows %d -> %d, want +1", auditsBefore, got)
		}
		readOnly, _ := e.member(t, alice, "orders:read")
		if _, err := csv(readOnly); !errors.Is(err, platform.ErrForbidden) {
			t.Errorf("CSV without orders:export: %v, want ErrForbidden", err)
		}
		noRead, _ := e.member(t, alice, "customers:read")
		if _, err := fin(noRead, s1, today, today); !errors.Is(err, platform.ErrForbidden) {
			t.Errorf("finance without orders:read: %v, want ErrForbidden", err)
		}
	})

	// Lane-close review P2: buyer.issue_capability creates an owner per anonymous visitor, so a list that scans every
	// buyer.owners row is O(visitors) per page. The work counter is pg_stat_xact_user_tables on buyer.owners inside the
	// one transaction (tuples fetched by any scan, including inside the SECURITY DEFINER function): deterministic, unlike
	// a timing bound. 10k idle owners must not be read; the customers themselves still are.
	t.Run("list reads customers, not every visitor (10k idle owners)", func(t *testing.T) {
		before := c.all(tok1, s1, "")
		mustExec(t, e.f.owner, `INSERT INTO buyer.owners(tenant_id,store_id) SELECT st.tenant_id,st.id FROM control.stores st,generate_series(1,10000) WHERE st.id=$1`, s1)
		var oid uint32
		if err := e.f.owner.QueryRow(ctx, `SELECT 'buyer.owners'::regclass::oid`).Scan(&oid); err != nil {
			t.Fatal(err)
		}
		var work int64
		err := platform.WithScope(ctx, e.f.runtime, tok1, s1, "store:read", func(tx pgx.Tx, s platform.Scope) error {
			read := func() (n int64) {
				if err := tx.QueryRow(ctx, `SELECT coalesce(sum(seq_tup_read+idx_tup_fetch),0)::bigint FROM pg_stat_xact_user_tables WHERE relid=$1::oid`, oid).Scan(&n); err != nil {
					t.Fatal(err)
				}
				return n
			}
			start := read()
			if _, err := customers.List(ctx, tx, s, tok1, customers.ListRequest{Page: pagination.Request{Limit: 50}}); err != nil {
				return err
			}
			work = read() - start
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		if work >= 1000 {
			t.Errorf("list read %d buyer.owners tuples with 10000 idle owners in the store (want < 1000: customers only)", work)
		}
		if after := c.all(tok1, s1, ""); len(after) != len(before) {
			t.Errorf("idle owners changed the customer list: %d -> %d rows", len(before), len(after))
		}
	})
}

// grant records the buyer's own consent through the frozen Go API (real buyer pool and capability).
func (c *cblEnv) grant(o rfxOrder, purpose, channel string) {
	c.t.Helper()
	cp := o.s.p.cap
	err := buyer.WithScope(c.ctx, o.s.p.a.runtime, cp.Token, o.store(), func(ctx context.Context, tx pgx.Tx, s buyer.Scope) error {
		_, e := customers.BuyerSetConsent(ctx, tx, s.StoreID, cp.Token, t04Key("cbl-consent"), customers.ConsentInput{Purpose: purpose, Channel: channel, Granted: true, Context: "settings"})
		return e
	})
	if err != nil {
		c.t.Fatalf("buyer consent: %v", err)
	}
}
