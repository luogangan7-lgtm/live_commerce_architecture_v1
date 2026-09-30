// T10 claims ingest and manual-entry gates (independent test_worker; contract
// contracts/live-keyword-claims-v1.md §2.3-§2.4, §3.1, §4.2 RecordManualClaim, §4.3
// Ingest, §5, §8, §9 KC06/KC07/KC08, §0.1 P2(b)-(e)).
//
// Owns: KC06 (every §2.4 ingest vector and S01-S05, the §3.1 event shape per reason,
// dedup on immutable facts, occurred_at bounds, validation, "claims touch nothing else"),
// KC07 (real parallel transactions: duplicate delivery, same-actor rapid set-quantity,
// the close-S1/open-S2 interleave, deactivate vs ingest, release vs redeem, two owners
// racing, cart lock ordering, a mixed workload with zero 40P01), KC08 (receipt replay,
// label_mac behaviour and a sentinel scan of the database and the PostgreSQL server log)
// and P2(b) label privacy, P2(c) reconstructible duplicates, P2(d) label normalization,
// P2(e) match_mode transitions.
//
// Non-goals: grammar vectors themselves (KC01), links/preview/redeem semantics beyond
// what the concurrency cases need (KC09-KC11), HTTP (KC13/KC14). Waits are observed in
// pg_stat_activity (lpWaitLock), never by sleeping.
//
// Ingest/IngestParsed are called directly in merchant transactions: the contract's only-caller rule is a
// source guard (KC15), not a runtime restriction on tests.
package foundation_test

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"os/exec"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"livecommerce/internal/buyer"
	"livecommerce/internal/claims"
	"livecommerce/internal/claims/grammar"
	"livecommerce/internal/command"
	"livecommerce/internal/live"
	"livecommerce/internal/pagination"
	"livecommerce/internal/platform"
	"livecommerce/internal/storefront"
)

func lcActor() string { return hex.EncodeToString(randomBytes(32)) }

// dbNow is the database clock in UTC with microsecond precision (IngestInput.OccurredAt).
func (h *lcHarness) dbNow(t *testing.T) time.Time {
	t.Helper()
	var now time.Time
	if err := h.f.owner.QueryRow(h.ctx, `SELECT clock_timestamp()`).Scan(&now); err != nil {
		t.Fatal(err)
	}
	return now.UTC().Truncate(time.Microsecond)
}

func (h *lcHarness) ingestIn(session, actor, label, text string, at time.Time) claims.IngestInput {
	return claims.IngestInput{TenantID: h.f.tenantA, StoreID: h.f.storeA1, SessionID: session, SourceKind: "manual", SourceEventID: randomUUID(),
		Platform: "manual", ActorKey: actor, ActorLabel: label, PrincipalID: h.actor, Text: text, OccurredAt: at}
}

func (h *lcHarness) ingest(in claims.IngestInput) (out claims.IngestResult, err error) {
	err = h.do(h.token, h.f.storeA1, func(tx pgx.Tx, _ platform.Scope) (e error) {
		out, e = claims.Ingest(h.ctx, tx, in)
		return e
	})
	return out, err
}

func (h *lcHarness) ingestParsed(in claims.IngestInput, p grammar.Result) (out claims.IngestResult, err error) {
	err = h.do(h.token, h.f.storeA1, func(tx pgx.Tx, _ platform.Scope) (e error) {
		out, e = claims.IngestParsed(h.ctx, tx, in, p)
		return e
	})
	return out, err
}

// lcEventRow is one persisted claims.events row with NULLs mapped to zero values.
type lcEventRow struct {
	session, source, sourceKind, platform, grammarVersion, kind, mode, outcome, reason, offer, bundle, principal string
	generation, quantity, lineVersion, previous                                                                  int64
	explicit                                                                                                     *bool
}

func lcEvent(t *testing.T, f *testFixture, id string) (e lcEventRow) {
	t.Helper()
	err := f.owner.QueryRow(context.Background(), `SELECT session_id::text,source_event_id::text,source_kind,platform,grammar_version,grammar_kind,match_mode,outcome,
		coalesce(reason,''),coalesce(offer_id::text,''),coalesce(bundle_id::text,''),coalesce(principal_id::text,''),window_generation,
		coalesce(quantity,0),coalesce(line_version,0),coalesce(previous_quantity,0),explicit_quantity FROM claims.events WHERE id=$1`, id).
		Scan(&e.session, &e.source, &e.sourceKind, &e.platform, &e.grammarVersion, &e.kind, &e.mode, &e.outcome, &e.reason, &e.offer, &e.bundle, &e.principal,
			&e.generation, &e.quantity, &e.lineVersion, &e.previous, &e.explicit)
	if err != nil {
		t.Fatalf("read event %s: %v", id, err)
	}
	return e
}

// lcWant is the expected §3.1 shape of one ingest outcome.
type lcWant struct {
	outcome    string
	reason     claims.Reason
	kind       string
	offer      *claims.Offer
	qty        int64 // 0 = NULL
	explicit   *bool // nil = NULL
	prev, line int64 // ACCEPTED only
	mode       string
	generation int64
	// ACCEPTED: the existing bundle id the result must name. REJECTED: any non-empty
	// value only states that the actor legitimately already owns a bundle; the result
	// BundleID stays "" (contract §4.3 IngestResult: "" unless ACCEPTED).
	bundleExpected string
}

func lcBool(b bool) *bool { return &b }

// checkIngest asserts the result and the persisted event against the §3.1 matrix.
func (h *lcHarness) checkIngest(t *testing.T, label string, in claims.IngestInput, r claims.IngestResult, err error, w lcWant) {
	t.Helper()
	if err != nil {
		t.Fatalf("%s: %v", label, err)
	}
	accepted := w.outcome == claims.OutcomeAccepted
	offerID, keyword := "", ""
	if w.offer != nil {
		offerID, keyword = w.offer.ID, w.offer.Keyword
	}
	if r.Outcome != w.outcome || r.Reason != w.reason || r.Duplicate || !command.ValidID(r.EventID) || r.SessionID != in.SessionID || r.GrammarVersion != "kw-v1" ||
		r.WindowGeneration != w.generation || r.OfferID != offerID || r.Keyword != keyword || r.Quantity != w.qty {
		t.Fatalf("%s: result %+v, want %+v", label, r, w)
	}
	if accepted != command.ValidID(r.BundleID) || (accepted && (r.LineVersion != w.line || r.PreviousQuantity != w.prev || r.BundleVersion < 1)) ||
		(!accepted && (r.LineVersion != 0 || r.PreviousQuantity != 0)) || (accepted && w.bundleExpected != "" && r.BundleID != w.bundleExpected) {
		t.Fatalf("%s: bundle/line fields %+v, want %+v", label, r, w)
	}
	e := lcEvent(t, h.f, r.EventID)
	reason := string(w.reason)
	bundle := ""
	if accepted {
		bundle = r.BundleID
	}
	if e.session != in.SessionID || e.source != in.SourceEventID || e.sourceKind != "manual" || e.platform != "manual" || e.grammarVersion != "kw-v1" ||
		e.kind != w.kind || e.mode != w.mode || e.outcome != w.outcome || e.reason != reason || e.offer != offerID || e.bundle != bundle || e.principal != h.actor ||
		e.generation != w.generation || e.quantity != w.qty || e.lineVersion != r.LineVersion || e.previous != r.PreviousQuantity ||
		(e.explicit == nil) != (w.explicit == nil) || (e.explicit != nil && *e.explicit != *w.explicit) {
		t.Fatalf("%s: persisted event %+v, want %+v", label, e, w)
	}
	if !accepted && countRows(t, h.f.owner, `SELECT count(*) FROM claims.bundles WHERE actor_key=$1`, in.ActorKey) != 0 && w.bundleExpected == "" {
		t.Fatalf("%s: a rejected first command created a bundle", label)
	}
}

// TestLiveClaimsKC06Ingest is KC06 (§2.3 precedence, §2.4 ingest vectors, §3.1, §4.3).
func TestLiveClaimsKC06Ingest(t *testing.T) {
	h := lcSetup(t)
	f := h.f
	s := h.draft(t, f.storeA1)
	quiet := lcDigest(t, f, "inventory", "storefront", "checkout", "buyer", "social", "meta_inbox")
	eventCount := func() int { return countRows(t, f.owner, `SELECT count(*) FROM claims.events WHERE session_id=$1`, s) }

	// I08 before the window row exists: WINDOW_CLOSED, nothing persisted.
	in := h.ingestIn(s, lcActor(), "never-open", "A1", h.dbNow(t))
	if r, err := h.ingest(in); err != nil || r.Outcome != claims.OutcomeRejected || r.Reason != claims.ReasonWindowClosed || r.EventID != "" || r.WindowGeneration != 0 || r.BundleID != "" {
		t.Fatalf("I08 without window: %+v %v", r, err)
	}
	w := h.open(t, s, claims.MatchExact)
	a1 := h.offer(t, s, "A1", h.stock.skus[0].ID, 3)
	b2 := h.offer(t, s, "B2", h.stock.skus[1].ID, 999)
	gen := w.Generation
	t0, f0 := false, true
	vector := func(label, text string, want lcWant) claims.IngestResult {
		t.Helper()
		in := h.ingestIn(s, lcActor(), "v-"+t04Tag(), text, h.dbNow(t))
		want.generation = gen
		if want.mode == "" {
			want.mode = "EXACT"
		}
		r, err := h.ingest(in)
		h.checkIngest(t, label+" "+fmt.Sprintf("%q", text), in, r, err, want)
		return r
	}
	rej := func(reason claims.Reason, kind string, offer *claims.Offer, qty int64, explicit *bool) lcWant {
		return lcWant{outcome: claims.OutcomeRejected, reason: reason, kind: kind, offer: offer, qty: qty, explicit: explicit}
	}
	vector("I01", "A1", lcWant{outcome: claims.OutcomeAccepted, kind: "MATCH", offer: &a1, qty: 1, explicit: &t0, line: 1})
	vector("I01 width", "ａ１＋２", lcWant{outcome: claims.OutcomeAccepted, kind: "MATCH", offer: &a1, qty: 2, explicit: &f0, line: 1})
	vector("B2 max", "B2+999", lcWant{outcome: claims.OutcomeAccepted, kind: "MATCH", offer: &b2, qty: 999, explicit: &f0, line: 1})
	vector("I02", "A1+5", rej(claims.ReasonQuantityOverMax, "MATCH", &a1, 5, &f0))
	for _, text := range []string{"Z9", "0912345678", "a1x2", "ABCDEFGHIJKLMNOP"} {
		vector("I03", text, rej(claims.ReasonUnknownKeyword, "MATCH", nil, 0, nil))
	}
	vector("I03 invalid", "0912345678+0", rej(claims.ReasonUnknownKeyword, "INVALID_QUANTITY", nil, 0, nil))
	vector("I04", "A1+0", rej(claims.ReasonInvalidQuantity, "INVALID_QUANTITY", &a1, 0, nil))
	vector("I04 B2", "B2+1000", rej(claims.ReasonInvalidQuantity, "INVALID_QUANTITY", &b2, 0, nil))
	vector("I04 leading zero", "A1+02", rej(claims.ReasonInvalidQuantity, "INVALID_QUANTITY", &a1, 0, nil))
	for _, text := range []string{"不要A1", "A1 +2", "A1+", "A1 B2", "A1是不是红色", "A1+2 謝謝", "#A1", "A1​", "  "} {
		vector("I07", text, rej(claims.ReasonNoMatch, "NO_MATCH", nil, 0, nil))
	}
	vector("R07 long", "A1"+strings.Repeat(" ", 300), rej(claims.ReasonNoMatch, "NO_MATCH", nil, 0, nil))

	// S01 same actor A1 -> A1+3 -> A1+2 -> A1; S02 redelivery; changed facts 409.
	actor := lcActor()
	var sIn []claims.IngestInput
	var sOut []claims.IngestResult
	var bundle string
	for i, step := range []struct {
		text     string
		qty      int64
		explicit bool
		prev     int64
	}{{"A1", 1, false, 0}, {"A1+3", 3, true, 1}, {"A1+2", 2, true, 3}, {"A1", 1, false, 2}} {
		in := h.ingestIn(s, actor, "s01", step.text, h.dbNow(t))
		r, err := h.ingest(in)
		h.checkIngest(t, "S01 "+step.text, in, r, err, lcWant{outcome: claims.OutcomeAccepted, kind: "MATCH", offer: &a1, qty: step.qty, explicit: lcBool(step.explicit),
			prev: step.prev, line: int64(i + 1), mode: "EXACT", generation: gen, bundleExpected: bundle})
		if r.BundleVersion != int64(i+1) {
			t.Fatalf("S01 bundle version %d at step %d", r.BundleVersion, i+1)
		}
		bundle = r.BundleID
		sIn, sOut = append(sIn, in), append(sOut, r)
	}
	var lineQty, lineVersion int64
	lineOf := func() {
		t.Helper()
		if err := f.owner.QueryRow(h.ctx, `SELECT quantity,version FROM claims.lines WHERE bundle_id=$1 AND offer_id=$2`, bundle, a1.ID).Scan(&lineQty, &lineVersion); err != nil {
			t.Fatal(err)
		}
	}
	lineOf()
	if lineQty != 1 || lineVersion != 4 {
		t.Fatalf("S01 line %d v%d", lineQty, lineVersion)
	}
	before := eventCount()
	for _, redelivery := range []claims.IngestInput{sIn[1], func() claims.IngestInput { x := sIn[1]; x.Text = "Ａ１＋３"; return x }()} {
		dup, err := h.ingest(redelivery)
		want := sOut[1]
		want.Duplicate = true
		if err != nil || !reflect.DeepEqual(dup, want) {
			t.Fatalf("S02 redelivery %+v %v, want %+v", dup, err, want)
		}
	}
	lineOf()
	if lineQty != 1 || lineVersion != 4 || eventCount() != before {
		t.Fatal("S02 duplicate wrote")
	}
	for label, in := range map[string]claims.IngestInput{
		"text":     func() claims.IngestInput { x := sIn[1]; x.Text = "A1+2"; return x }(),
		"implicit": func() claims.IngestInput { x := sIn[0]; x.Text = "A1+1"; return x }(),
		"offer":    func() claims.IngestInput { x := sIn[1]; x.Text = "B2+3"; return x }(),
		"kind":     func() claims.IngestInput { x := sIn[1]; x.Text = "不要"; return x }(),
		"occurred": func() claims.IngestInput { x := sIn[1]; x.OccurredAt = x.OccurredAt.Add(time.Microsecond); return x }(),
		"session":  func() claims.IngestInput { x := sIn[1]; x.SessionID = h.draft(t, f.storeA1); return x }(),
		"actor":    func() claims.IngestInput { x := sIn[1]; x.ActorKey = lcActor(); return x }(),
	} {
		lcIs(t, lcErr(h.ingest(in)), command.ErrConflict, "duplicate source with changed "+label)
	}
	if eventCount() != before {
		t.Fatal("conflicting redeliveries wrote")
	}
	// Only immutable facts are compared: a NO_MATCH source redelivered by another actor
	// with other NO_MATCH text is the same fact (text is never stored, §4.3 step 2).
	noMatch := h.ingestIn(s, lcActor(), "nm", "不要A1", h.dbNow(t))
	first, err := h.ingest(noMatch)
	if err != nil {
		t.Fatal(err)
	}
	other := noMatch
	other.ActorKey, other.Text = lcActor(), "A1 謝謝"
	if dup, err := h.ingest(other); err != nil || !dup.Duplicate || dup.EventID != first.EventID || dup.Reason != claims.ReasonNoMatch {
		t.Fatalf("NO_MATCH redelivery %+v %v", dup, err)
	}
	// S03 unknown keyword -> CreateOffer -> redelivery stays a duplicate UNKNOWN_KEYWORD.
	z9In := h.ingestIn(s, lcActor(), "s03", "Z9+2", h.dbNow(t))
	z9, err := h.ingest(z9In)
	if err != nil || z9.Reason != claims.ReasonUnknownKeyword {
		t.Fatalf("S03 first %+v %v", z9, err)
	}
	h.offer(t, s, "Z9", lcSKUs(t, f, f.tenantA, f.storeA1, "USD", 1)[0], 5)
	n := eventCount()
	if dup, err := h.ingest(z9In); err != nil || !dup.Duplicate || dup.Reason != claims.ReasonUnknownKeyword || dup.OfferID != "" || dup.EventID != z9.EventID || eventCount() != n {
		t.Fatalf("S03 redelivery %+v %v", dup, err)
	}
	// S05 new manual actor via actor_label, then bundle_id with an empty label: one bundle, label unchanged.
	s05 := h.accepted(t, s, "", "@S05-"+strings.ToUpper(t04Tag()[:4]), "A1")
	s05b := h.accepted(t, s, s05.BundleID, "", "B2")
	var label string
	var lineCount int
	if err := f.owner.QueryRow(h.ctx, `SELECT label,line_count FROM claims.bundles WHERE id=$1`, s05.BundleID).Scan(&label, &lineCount); err != nil ||
		s05b.BundleID != s05.BundleID || !strings.HasPrefix(label, "s05-") || lineCount != 2 {
		t.Fatalf("S05 bundle label=%q lines=%d %v", label, lineCount, err)
	}

	// occurred_at bounds: before opened_at -> WINDOW_CLOSED (not persisted); +100 s ok; +125 s invalid.
	n = eventCount()
	early := h.ingestIn(s, lcActor(), "early", "A1", w.OpenedAt.UTC().Add(-time.Microsecond).Truncate(time.Microsecond))
	if r, err := h.ingest(early); err != nil || r.Reason != claims.ReasonWindowClosed || r.EventID != "" || eventCount() != n {
		t.Fatalf("occurred before opened_at %+v %v", r, err)
	}
	if r, err := h.ingest(h.ingestIn(s, lcActor(), "future-ok", "A1", h.dbNow(t).Add(100*time.Second))); err != nil || r.Outcome != claims.OutcomeAccepted {
		t.Fatalf("occurred +100s %+v %v", r, err)
	}
	lcIs(t, lcErr(h.ingest(h.ingestIn(s, lcActor(), "future-bad", "A1", h.dbNow(t).Add(125*time.Second)))), command.ErrInvalid, "occurred beyond +120s")

	// Validation -> ErrInvalid (§4.3 step 1), before any write.
	n = eventCount()
	base := h.ingestIn(s, lcActor(), "valid", "A1", h.dbNow(t))
	mut := func(fn func(*claims.IngestInput)) claims.IngestInput {
		x := base
		x.SourceEventID = randomUUID()
		fn(&x)
		return x
	}
	for label, in := range map[string]claims.IngestInput{
		"meta source": mut(func(x *claims.IngestInput) { x.SourceKind = "meta" }), "empty source": mut(func(x *claims.IngestInput) { x.SourceKind = "" }),
		"platform": mut(func(x *claims.IngestInput) { x.Platform = "facebook" }), "actor upper": mut(func(x *claims.IngestInput) { x.ActorKey = strings.ToUpper(x.ActorKey) }),
		"actor short": mut(func(x *claims.IngestInput) { x.ActorKey = x.ActorKey[:63] }), "tenant": mut(func(x *claims.IngestInput) { x.TenantID = f.tenantB }),
		"store": mut(func(x *claims.IngestInput) { x.StoreID = f.storeA2 }), "principal": mut(func(x *claims.IngestInput) { x.PrincipalID = randomUUID() }),
		"session": mut(func(x *claims.IngestInput) { x.SessionID = "not-a-uuid" }), "source id": mut(func(x *claims.IngestInput) { x.SourceEventID = "not-a-uuid" }),
		"zero time": mut(func(x *claims.IngestInput) { x.OccurredAt = time.Time{} }), "text 64KiB+1": mut(func(x *claims.IngestInput) { x.Text = strings.Repeat("A", 65537) }),
	} {
		lcIs(t, lcErr(h.ingest(in)), command.ErrInvalid, "Ingest "+label)
	}
	if r, err := h.ingest(mut(func(x *claims.IngestInput) { x.Text = "A1" + strings.Repeat(" ", 65534) })); err != nil || r.Reason != claims.ReasonNoMatch {
		t.Fatalf("64 KiB text is valid input and NO_MATCH: %+v %v", r, err)
	}
	n = eventCount()
	parsed := grammar.Parse("A1+2")
	for label, tc := range map[string]struct {
		in claims.IngestInput
		p  grammar.Result
	}{
		"text with parsed": {mut(func(x *claims.IngestInput) {}), parsed},
		"version":          {mut(func(x *claims.IngestInput) { x.Text = "" }), grammar.Result{Version: "kw-v2", Kind: grammar.Match, Keyword: "A1", Quantity: 1}},
		"zero quantity":    {mut(func(x *claims.IngestInput) { x.Text = "" }), grammar.Result{Version: grammar.Version, Kind: grammar.Match, Keyword: "A1"}},
		"quantity 1000":    {mut(func(x *claims.IngestInput) { x.Text = "" }), grammar.Result{Version: grammar.Version, Kind: grammar.Match, Keyword: "A1", Quantity: 1000, Explicit: true}},
		"lowercase":        {mut(func(x *claims.IngestInput) { x.Text = "" }), grammar.Result{Version: grammar.Version, Kind: grammar.Match, Keyword: "a1", Quantity: 1}},
		"no-match keyword": {mut(func(x *claims.IngestInput) { x.Text = "" }), grammar.Result{Version: grammar.Version, Kind: grammar.NoMatch, Keyword: "A1"}},
		"unknown kind":     {mut(func(x *claims.IngestInput) { x.Text = "" }), grammar.Result{Version: grammar.Version, Kind: "PARTIAL", Keyword: "A1", Quantity: 1}},
	} {
		lcIs(t, lcErr(h.ingestParsed(tc.in, tc.p)), command.ErrInvalid, "IngestParsed "+label)
	}
	if r, err := h.ingestParsed(mut(func(x *claims.IngestInput) { x.Text = "" }), parsed); err != nil || r.Outcome != claims.OutcomeAccepted || r.Quantity != 2 {
		t.Fatalf("IngestParsed positive control %+v %v", r, err)
	}
	// A fresh actor key: `base` already owns a bundle (positive control above), and an
	// existing bundle legitimately ignores ActorLabel (§4.3 step 6).
	lcIs(t, lcErr(h.ingest(mut(func(x *claims.IngestInput) { x.ActorKey = lcActor(); x.ActorLabel = "" }))), command.ErrInvalid, "new bundle without ActorLabel")
	rr, err := h.f.runtime.BeginTx(h.ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = rr.Exec(h.ctx, `SELECT set_config('app.tenant_id',$1,true),set_config('app.store_id',$2,true),set_config('app.principal_id',$3,true)`, f.tenantA, f.storeA1, h.actor); err != nil {
		t.Fatal(err)
	}
	_, err = claims.Ingest(h.ctx, rr, mut(func(x *claims.IngestInput) {}))
	_ = rr.Rollback(h.ctx)
	lcIs(t, err, command.ErrInvalid, "REPEATABLE READ ingest")
	if eventCount() != n+1 {
		t.Fatalf("invalid ingests wrote rows: %d -> %d", n, eventCount())
	}

	// I05 KEYWORD_QTY_ONLY (mode changes only while CLOSED; opening increments generation).
	w = h.open(t, s, claims.MatchKeywordQtyOnly)
	gen = w.Generation
	if gen != 2 {
		t.Fatalf("second opening generation %d", gen)
	}
	vector("I05", "A1", lcWant{outcome: claims.OutcomeRejected, reason: claims.ReasonQuantityRequired, kind: "MATCH", offer: &a1, qty: 1, explicit: &t0, mode: "KEYWORD_QTY_ONLY"})
	vector("I05", "A1+3", lcWant{outcome: claims.OutcomeAccepted, kind: "MATCH", offer: &a1, qty: 3, explicit: &f0, line: 1, mode: "KEYWORD_QTY_ONLY"})
	vector("I05 over max", "A1+4", lcWant{outcome: claims.OutcomeRejected, reason: claims.ReasonQuantityOverMax, kind: "MATCH", offer: &a1, qty: 4, explicit: &f0, mode: "KEYWORD_QTY_ONLY"})
	vector("I05 invalid first", "A1+0", lcWant{outcome: claims.OutcomeRejected, reason: claims.ReasonInvalidQuantity, kind: "INVALID_QUANTITY", offer: &a1, mode: "KEYWORD_QTY_ONLY"})
	// I06 inactive offer and occurred_at < activated_at.
	w = h.open(t, s, claims.MatchExact)
	gen = w.Generation
	inactive := h.setOffer(t, s, h.board(t, s).Offers[0], 3, false)
	vector("I06", "A1", rej(claims.ReasonOfferInactive, "MATCH", &inactive, 1, &t0))
	vector("I06 explicit", "A1+2", rej(claims.ReasonOfferInactive, "MATCH", &inactive, 2, &f0))
	vector("I06 invalid", "A1+0", rej(claims.ReasonOfferInactive, "INVALID_QUANTITY", &inactive, 0, nil))
	vector("I06 over max", "A1+9", rej(claims.ReasonOfferInactive, "MATCH", &inactive, 9, &f0))
	active := h.setOffer(t, s, inactive, 3, true)
	beforeActivation := active.ActivatedAt.UTC().Add(-time.Microsecond).Truncate(time.Microsecond)
	if !beforeActivation.After(*w.OpenedAt) {
		t.Fatalf("fixture timing: activated_at %v not after opened_at %v", active.ActivatedAt, w.OpenedAt)
	}
	lateIn := h.ingestIn(s, lcActor(), "pre-activation", "A1", beforeActivation)
	r, err := h.ingest(lateIn)
	h.checkIngest(t, "I06 occurred before activated_at", lateIn, r, err, lcWant{outcome: claims.OutcomeRejected, reason: claims.ReasonOfferInactive, kind: "MATCH", offer: &active, qty: 1, explicit: &t0, mode: "EXACT", generation: gen})
	vector("I06 reactivated", "A1", lcWant{outcome: claims.OutcomeAccepted, kind: "MATCH", offer: &active, qty: 1, explicit: &t0, line: 1})

	// Board stats: every persisted reason present, counts equal the current generation.
	board := h.board(t, s)
	var accepted int64
	if err := f.owner.QueryRow(h.ctx, `SELECT count(*) FROM claims.events WHERE session_id=$1 AND window_generation=$2 AND outcome='ACCEPTED'`, s, gen).Scan(&accepted); err != nil {
		t.Fatal(err)
	}
	if board.Stats.Generation != gen || board.Stats.Accepted != accepted || len(board.Stats.Rejected) != 7 {
		t.Fatalf("stats %+v (accepted %d)", board.Stats, accepted)
	}
	for _, reason := range []claims.Reason{claims.ReasonNoMatch, claims.ReasonUnknownKeyword, claims.ReasonOfferInactive, claims.ReasonInvalidQuantity,
		claims.ReasonQuantityRequired, claims.ReasonQuantityOverMax, claims.ReasonBundleLimit} {
		got, ok := board.Stats.Rejected[reason]
		if want := int64(countRows(t, f.owner, `SELECT count(*) FROM claims.events WHERE session_id=$1 AND window_generation=$2 AND reason=$3`, s, gen, string(reason))); !ok || got != want {
			t.Fatalf("stats %s=%d (present %t), want %d", reason, got, ok, want)
		}
	}

	// I08 after close; and R7: another session's OPEN window never admits this session.
	h.closeWindow(t, s)
	n = eventCount()
	if r, err := h.ingest(h.ingestIn(s, lcActor(), "closed", "A1", h.dbNow(t))); err != nil || r.Reason != claims.ReasonWindowClosed || r.EventID != "" || eventCount() != n {
		t.Fatalf("I08 after close %+v %v", r, err)
	}
	// A duplicate of an ACCEPTED source is answered from storage even after the close
	// (dedup precedes the window read, §4.3 steps 2-3).
	if dup, err := h.ingest(sIn[3]); err != nil || !dup.Duplicate || dup.EventID != sOut[3].EventID {
		t.Fatalf("duplicate after close %+v %v", dup, err)
	}
	s2 := h.draft(t, f.storeA1)
	h.open(t, s2, claims.MatchExact)
	if r, err := h.ingest(h.ingestIn(s, lcActor(), "other-open", "A1", h.dbNow(t))); err != nil || r.Reason != claims.ReasonWindowClosed || eventCount() != n {
		t.Fatalf("R7 other session open %+v %v", r, err)
	}
	h.closeWindow(t, s2)

	// S04 BUNDLE_LIMIT: 50 lines accepted, the 51st distinct offer is rejected; an
	// existing line of the full bundle still updates.
	sl := h.draft(t, f.storeA1)
	wl := h.open(t, sl, claims.MatchExact)
	skus := lcSKUs(t, f, f.tenantA, f.storeA1, "USD", 51)
	mustExec(t, f.owner, `INSERT INTO live.offers(tenant_id,store_id,session_id,keyword,sku_id,max_quantity_per_claim,principal_id)
		SELECT $1,$2,$3,'K'||lpad(i::text,4,'0'),sku,5,$4 FROM unnest($5::uuid[]) WITH ORDINALITY AS u(sku,i)`, f.tenantA, f.storeA1, sl, h.actor, skus)
	offers := map[string]claims.Offer{}
	for _, o := range h.board(t, sl).Offers {
		offers[o.Keyword] = o
	}
	big := lcActor()
	for i := 1; i <= 50; i++ {
		kw := fmt.Sprintf("K%04d", i)
		in := h.ingestIn(sl, big, "limit", kw, h.dbNow(t))
		if r, err := h.ingest(in); err != nil || r.Outcome != claims.OutcomeAccepted || r.LineVersion != 1 || r.BundleVersion != int64(i) {
			t.Fatalf("S04 line %d %+v %v", i, r, err)
		}
	}
	o51 := offers["K0051"]
	limitIn := h.ingestIn(sl, big, "", "K0051", h.dbNow(t))
	r, err = h.ingest(limitIn)
	h.checkIngest(t, "S04 51st", limitIn, r, err, lcWant{outcome: claims.OutcomeRejected, reason: claims.ReasonBundleLimit, kind: "MATCH", offer: &o51, qty: 1, explicit: &t0,
		mode: "EXACT", generation: wl.Generation, bundleExpected: "limit-bundle-exists"})
	o1 := offers["K0001"]
	updIn := h.ingestIn(sl, big, "", "K0001+2", h.dbNow(t))
	r, err = h.ingest(updIn)
	h.checkIngest(t, "S04 update in full bundle", updIn, r, err, lcWant{outcome: claims.OutcomeAccepted, kind: "MATCH", offer: &o1, qty: 2, explicit: &f0, prev: 1, line: 2, mode: "EXACT", generation: wl.Generation})
	if n := countRows(t, f.owner, `SELECT line_count FROM claims.bundles WHERE actor_key=$1`, big); n != 50 {
		t.Fatalf("S04 line_count %d", n)
	}
	h.closeWindow(t, sl)

	// Committed bundles always have >=1 line; line_count and version match the lines/events.
	if n := countRows(t, f.owner, `SELECT count(*) FROM claims.bundles b WHERE b.session_id=ANY($1) AND (
		b.line_count<>(SELECT count(*) FROM claims.lines l WHERE l.bundle_id=b.id) OR b.line_count<1
		OR b.version<>(SELECT count(*) FROM claims.events e WHERE e.bundle_id=b.id AND e.outcome='ACCEPTED'))`, []string{s, sl}); n != 0 {
		t.Fatalf("%d bundles violate the line/version bookkeeping", n)
	}
	lcSameDigest(t, "ingest touched a foreign schema", quiet, lcDigest(t, f, "inventory", "storefront", "checkout", "buyer", "social", "meta_inbox"))
}

// TestLiveClaimsKC07Concurrency is KC07 (§5: replay, absolute quantities, order, fences,
// binding, global lock order) with real parallel transactions.
func TestLiveClaimsKC07Concurrency(t *testing.T) {
	h := lcSetup(t)
	f := h.f
	s := h.draft(t, f.storeA1)
	h.open(t, s, claims.MatchExact)
	a1 := h.offer(t, s, "A1", h.stock.skus[0].ID, 5)
	b2 := h.offer(t, s, "B2", h.stock.skus[1].ID, 999)
	var wg sync.WaitGroup

	t.Run("duplicate-delivery-x20", func(t *testing.T) {
		in := h.ingestIn(s, lcActor(), "dup-20", "A1+2", h.dbNow(t))
		results, errs := make([]claims.IngestResult, 20), make([]error, 20)
		start := make(chan struct{})
		for i := range results {
			wg.Add(1)
			go func(i int) { defer wg.Done(); <-start; results[i], errs[i] = h.ingest(in) }(i)
		}
		close(start)
		wg.Wait()
		fresh := 0
		for i, r := range results {
			if errs[i] != nil {
				t.Fatalf("delivery %d: %v", i, errs[i])
			}
			if !r.Duplicate {
				fresh++
			}
			r.Duplicate = false
			first := results[0]
			first.Duplicate = false
			if !reflect.DeepEqual(r, first) {
				t.Fatalf("delivery %d result %+v differs from %+v", i, r, first)
			}
		}
		if fresh != 1 || countRows(t, f.owner, `SELECT count(*) FROM claims.events WHERE source_event_id=$1`, in.SourceEventID) != 1 ||
			countRows(t, f.owner, `SELECT version FROM claims.lines WHERE bundle_id=$1`, results[0].BundleID) != 1 {
			t.Fatalf("duplicate delivery wrote %d fresh results", fresh)
		}
	})

	t.Run("same-actor-rapid-set-quantity", func(t *testing.T) {
		base := h.accepted(t, s, "", "rapid", "B2")
		const n = 12
		results, errs := make([]claims.ManualClaimResult, n), make([]error, n)
		start := make(chan struct{})
		for i := 0; i < n; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				<-start
				results[i], errs[i] = h.manual(h.token, f.storeA1, t04Key("lc-rapid"), s, claims.ManualClaimInput{BundleID: base.BundleID, Text: fmt.Sprintf("B2+%d", i+2)})
			}(i)
		}
		close(start)
		wg.Wait()
		seen := map[int64]bool{}
		for i := range results {
			if errs[i] != nil || results[i].Outcome != claims.OutcomeAccepted || seen[results[i].LineVersion] {
				t.Fatalf("rapid set %d: %+v %v", i, results[i], errs[i])
			}
			seen[results[i].LineVersion] = true
		}
		var qty, version int64
		if err := f.owner.QueryRow(h.ctx, `SELECT quantity,version FROM claims.lines WHERE bundle_id=$1 AND offer_id=$2`, base.BundleID, b2.ID).Scan(&qty, &version); err != nil {
			t.Fatal(err)
		}
		var lastQty, broken int64
		if err := f.owner.QueryRow(h.ctx, `SELECT (SELECT quantity FROM claims.events WHERE bundle_id=$1 AND offer_id=$2 AND outcome='ACCEPTED' ORDER BY line_version DESC LIMIT 1),
			(SELECT count(*) FROM claims.events e JOIN claims.events p ON p.bundle_id=e.bundle_id AND p.offer_id=e.offer_id AND p.outcome='ACCEPTED' AND p.line_version=e.line_version-1
			 WHERE e.bundle_id=$1 AND e.offer_id=$2 AND e.outcome='ACCEPTED' AND e.previous_quantity IS DISTINCT FROM p.quantity)`, base.BundleID, b2.ID).Scan(&lastQty, &broken); err != nil {
			t.Fatal(err)
		}
		if version != n+1 || qty != lastQty || broken != 0 || countRows(t, f.owner, `SELECT version FROM claims.bundles WHERE id=$1`, base.BundleID) != n+1 {
			t.Fatalf("final line qty=%d v%d (last committed %d, broken chain %d)", qty, version, lastQty, broken)
		}
	})

	t.Run("close-s1-open-s2-interleave", func(t *testing.T) {
		other := h.draft(t, f.storeA1)
		key := t04Key("lc-interleave")
		lock, err := f.owner.Begin(h.ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer lock.Rollback(h.ctx)
		if _, err = lock.Exec(h.ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, "command|"+f.tenantA+"|"+f.storeA1+"|live.claim.manual|"+key); err != nil {
			t.Fatal(err)
		}
		events := countRows(t, f.owner, `SELECT count(*) FROM claims.events WHERE session_id=$1`, s)
		var got claims.ManualClaimResult
		pid, done := h.bgMerchant(t, h.token, f.storeA1, func(tx pgx.Tx, sc platform.Scope) (err error) {
			got, err = claims.RecordManualClaim(h.ctx, tx, sc, h.labels, h.token, key, s, claims.ManualClaimInput{ActorLabel: "interleave", Text: "A1"})
			return err
		})
		lpWaitLock(t, f, pid, true)
		h.closeWindow(t, s)
		h.open(t, other, claims.MatchExact)
		_ = lock.Rollback(h.ctx)
		if err := waitError(t, done); err != nil || got.Outcome != claims.OutcomeRejected || got.Reason != claims.ReasonWindowClosed {
			t.Fatalf("interleaved manual claim %+v %v", got, err)
		}
		if countRows(t, f.owner, `SELECT count(*) FROM claims.events WHERE session_id=$1`, other)+countRows(t, f.owner, `SELECT count(*) FROM claims.bundles WHERE session_id=$1`, other) != 0 ||
			countRows(t, f.owner, `SELECT count(*) FROM claims.events WHERE session_id=$1`, s) != events {
			t.Fatal("interleaved claim wrote to a session")
		}
		h.closeWindow(t, other)
		h.open(t, s, claims.MatchExact)
	})

	t.Run("deactivate-vs-ingest", func(t *testing.T) {
		base := h.accepted(t, s, "", "fence", "B2")
		current := func() claims.Offer {
			for _, o := range h.board(t, s).Offers {
				if o.ID == a1.ID {
					return o
				}
			}
			t.Fatal("offer A1 missing")
			return claims.Offer{}
		}
		// An in-flight ingest holds A1 FOR SHARE; the deactivation waits, then later ingests see it inactive.
		hold := h.holdMerchant(t, h.token, f.storeA1, func(tx pgx.Tx, sc platform.Scope) error {
			r, err := claims.RecordManualClaim(h.ctx, tx, sc, h.labels, h.token, t04Key("lc-fence"), s, claims.ManualClaimInput{BundleID: base.BundleID, Text: "A1"})
			if err == nil && r.Outcome != claims.OutcomeAccepted {
				err = fmt.Errorf("fenced claim %+v", r)
			}
			return err
		})
		o := current()
		pid, done := h.bgMerchant(t, h.token, f.storeA1, func(tx pgx.Tx, sc platform.Scope) error {
			_, err := claims.UpdateOffer(h.ctx, tx, sc, h.token, t04Key("lc-deactivate"), s, o.ID, claims.OfferUpdate{ExpectedVersion: o.Version, MaxQuantityPerClaim: o.MaxQuantityPerClaim, Active: false})
			return err
		})
		lpWaitLock(t, f, pid, false)
		if err := hold.finish(t); err != nil {
			t.Fatal(err)
		}
		if err := waitError(t, done); err != nil {
			t.Fatalf("deactivation after in-flight ingest: %v", err)
		}
		if r := h.claim(t, s, claims.ManualClaimInput{BundleID: base.BundleID, Text: "A1+2"}); r.Reason != claims.ReasonOfferInactive {
			t.Fatalf("ingest after deactivation %+v", r)
		}
		// Reverse: a held deactivation makes a concurrent ingest wait; the ingest then sees it inactive.
		o = h.setOffer(t, s, current(), 5, true)
		hold = h.holdMerchant(t, h.token, f.storeA1, func(tx pgx.Tx, sc platform.Scope) error {
			_, err := claims.UpdateOffer(h.ctx, tx, sc, h.token, t04Key("lc-deactivate"), s, o.ID, claims.OfferUpdate{ExpectedVersion: o.Version, MaxQuantityPerClaim: 5, Active: false})
			return err
		})
		var got claims.ManualClaimResult
		pid, done = h.bgMerchant(t, h.token, f.storeA1, func(tx pgx.Tx, sc platform.Scope) (err error) {
			got, err = claims.RecordManualClaim(h.ctx, tx, sc, h.labels, h.token, t04Key("lc-fence"), s, claims.ManualClaimInput{BundleID: base.BundleID, Text: "A1+3"})
			return err
		})
		lpWaitLock(t, f, pid, false)
		if err := hold.finish(t); err != nil {
			t.Fatal(err)
		}
		if err := waitError(t, done); err != nil || got.Reason != claims.ReasonOfferInactive {
			t.Fatalf("ingest waiting on deactivation %+v %v", got, err)
		}
		h.setOffer(t, s, current(), 5, true)
	})

	t.Run("owners-racing-to-redeem", func(t *testing.T) {
		r := h.accepted(t, s, "", "race", "A1+2")
		l := h.link(t, s, r.BundleID, 0, false)
		owner1, owner2 := mustIssue(t, h.service, f.storeA1), mustIssue(t, h.service, f.storeA1)
		hold := h.holdBuyer(t, owner1, func(ctx context.Context, tx pgx.Tx, sc buyer.Scope) error {
			_, err := claims.RedeemLink(ctx, tx, sc, t04Key("lc-redeem"), l.Token, claims.RedeemInput{ExpectedBundleVersion: r.BundleVersion})
			return err
		})
		loserKey := t04Key("lc-redeem")
		pid, done := h.bgBuyer(t, owner2, func(ctx context.Context, tx pgx.Tx, sc buyer.Scope) error {
			_, err := claims.RedeemLink(ctx, tx, sc, loserKey, l.Token, claims.RedeemInput{ExpectedBundleVersion: r.BundleVersion})
			return err
		})
		lpWaitLock(t, f, pid, false)
		if err := hold.finish(t); err != nil {
			t.Fatal(err)
		}
		lcIs(t, waitError(t, done), command.ErrNotFound, "second owner after the first bound")
		if lcOwner(t, f, r.BundleID) != owner1.Scope.OwnerID || h.cartOf(t, owner2).Version != 0 ||
			countRows(t, f.owner, `SELECT count(*) FROM buyer.command_results WHERE owner_id=$1`, owner2.Scope.OwnerID) != 0 {
			t.Fatal("losing owner wrote or the binding moved")
		}
		// Unsynchronized race on a fresh bundle: exactly one owner binds.
		r2 := h.accepted(t, s, "", "race-2", "A1")
		l2 := h.link(t, s, r2.BundleID, 0, false)
		owners := []buyer.Capability{mustIssue(t, h.service, f.storeA1), mustIssue(t, h.service, f.storeA1)}
		errs := make([]error, 2)
		start := make(chan struct{})
		for i := range owners {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				<-start
				_, errs[i] = h.redeem(owners[i], t04Key("lc-redeem"), l2.Token, r2.BundleVersion)
			}(i)
		}
		close(start)
		wg.Wait()
		winner := -1
		for i, err := range errs {
			if err == nil {
				winner = i
			} else if !errors.Is(err, command.ErrNotFound) {
				t.Fatalf("race error %v", err)
			}
		}
		if winner < 0 || errs[1-winner] == nil || lcOwner(t, f, r2.BundleID) != owners[winner].Scope.OwnerID {
			t.Fatalf("race outcome %v", errs)
		}
	})

	t.Run("release-vs-redeem", func(t *testing.T) {
		r := h.accepted(t, s, "", "release", "A1+2")
		l := h.link(t, s, r.BundleID, 0, false)
		owner1, owner2 := mustIssue(t, h.service, f.storeA1), mustIssue(t, h.service, f.storeA1)
		if _, err := h.redeem(owner1, t04Key("lc-redeem"), l.Token, r.BundleVersion); err != nil {
			t.Fatal(err)
		}
		// A held release+rotation makes an old-token redeem wait; afterwards the old token is gone.
		var rotated claims.IssuedLink
		hold := h.holdMerchant(t, h.token, f.storeA1, func(tx pgx.Tx, sc platform.Scope) (err error) {
			rotated, err = claims.IssueLink(h.ctx, tx, sc, h.token, t04Key("lc-link"), s, r.BundleID, claims.LinkInput{ExpectedGeneration: 1, ReleaseBinding: true})
			return err
		})
		pid, done := h.bgBuyer(t, owner1, func(ctx context.Context, tx pgx.Tx, sc buyer.Scope) error {
			_, err := claims.RedeemLink(ctx, tx, sc, t04Key("lc-redeem"), l.Token, claims.RedeemInput{ExpectedBundleVersion: r.BundleVersion})
			return err
		})
		lpWaitLock(t, f, pid, false)
		if err := hold.finish(t); err != nil || !rotated.Released {
			t.Fatalf("release: %+v %v", rotated.Released, err)
		}
		lcIs(t, waitError(t, done), command.ErrNotFound, "old-token redeem after release")
		if lcOwner(t, f, r.BundleID) != "" {
			t.Fatal("redeem re-bound a released bundle through the old token")
		}
		// A held redeem by the new owner makes the next release wait; the release then clears that binding.
		hold = h.holdBuyer(t, owner2, func(ctx context.Context, tx pgx.Tx, sc buyer.Scope) error {
			_, err := claims.RedeemLink(ctx, tx, sc, t04Key("lc-redeem"), rotated.Token, claims.RedeemInput{ExpectedBundleVersion: r.BundleVersion})
			return err
		})
		var again claims.IssuedLink
		pid, done = h.bgMerchant(t, h.token, f.storeA1, func(tx pgx.Tx, sc platform.Scope) (err error) {
			again, err = claims.IssueLink(h.ctx, tx, sc, h.token, t04Key("lc-link"), s, r.BundleID, claims.LinkInput{ExpectedGeneration: 2, ReleaseBinding: true})
			return err
		})
		lpWaitLock(t, f, pid, false)
		if err := hold.finish(t); err != nil {
			t.Fatal(err)
		}
		if err := waitError(t, done); err != nil || !again.Released || lcOwner(t, f, r.BundleID) != "" {
			t.Fatalf("release after held redeem: released=%t err=%v", again.Released, err)
		}
		if items := lcItems(h.cartOf(t, owner2).Items); items[h.stock.skus[0].ID] != 2 {
			t.Fatalf("released owner's cart must be untouched: %v", items)
		}
	})

	t.Run("cart-lock-ordering", func(t *testing.T) {
		r := h.accepted(t, s, "", "cart-order", "B2+2")
		l := h.link(t, s, r.BundleID, 0, false)
		owner := mustIssue(t, h.service, f.storeA1)
		hold := h.holdBuyer(t, owner, func(ctx context.Context, tx pgx.Tx, sc buyer.Scope) error {
			return storefront.LockCartOwner(ctx, tx, sc)
		})
		pid, done := h.bgBuyer(t, owner, func(ctx context.Context, tx pgx.Tx, sc buyer.Scope) error {
			_, err := claims.RedeemLink(ctx, tx, sc, t04Key("lc-redeem"), l.Token, claims.RedeemInput{ExpectedBundleVersion: r.BundleVersion})
			return err
		})
		lpWaitLock(t, f, pid, true)
		if err := hold.finish(t); err != nil {
			t.Fatal(err)
		}
		if err := waitError(t, done); err != nil || lcItems(h.cartOf(t, owner).Items)[h.stock.skus[1].ID] != 2 {
			t.Fatalf("redeem after the cart-owner lock: %v", err)
		}
		// Two bundles of one owner redeemed concurrently with the owner's own PUT cart: no
		// deadlock, only success or CAS conflict.
		x, y := h.accepted(t, s, "", "stress-x", "A1"), h.accepted(t, s, "", "stress-y", "B2")
		lx, ly := h.link(t, s, x.BundleID, 0, false), h.link(t, s, y.BundleID, 0, false)
		for round := 0; round < 6; round++ {
			rx := h.accepted(t, s, x.BundleID, "", fmt.Sprintf("A1+%d", round%4+1))
			ry := h.accepted(t, s, y.BundleID, "", fmt.Sprintf("B2+%d", round+1))
			cart := h.cartOf(t, owner)
			errs := make([]error, 3)
			start := make(chan struct{})
			ops := []func() error{
				func() error { _, err := h.redeem(owner, t04Key("lc-redeem"), lx.Token, rx.BundleVersion); return err },
				func() error { _, err := h.redeem(owner, t04Key("lc-redeem"), ly.Token, ry.BundleVersion); return err },
				func() error {
					_, err := h.putCart(owner, t04Key("lc-cart"), storefront.CartInput{ExpectedVersion: cart.Version, Items: []storefront.Item{{SKUID: h.stock.skus[0].ID, Quantity: int64(round + 1)}}})
					return err
				},
			}
			for i := range ops {
				wg.Add(1)
				go func(i int) { defer wg.Done(); <-start; errs[i] = ops[i]() }(i)
			}
			close(start)
			wg.Wait()
			for i, err := range errs {
				if err != nil && !errors.Is(err, command.ErrConflict) {
					t.Fatalf("round %d op %d: %v (sqlstate %s)", round, i, err, sqlState(err))
				}
			}
		}
	})

	t.Run("mixed-workload-zero-deadlocks", func(t *testing.T) {
		p := h.accepted(t, s, "", "mixed-p", "B2")
		lp := h.link(t, s, p.BundleID, 0, false)
		q := h.accepted(t, s, "", "mixed-q", "A1")
		h.link(t, s, q.BundleID, 0, false)
		owner := mustIssue(t, h.service, f.storeA1)
		if _, err := h.redeem(owner, t04Key("lc-redeem"), lp.Token, p.BundleVersion); err != nil {
			t.Fatal(err)
		}
		var mu sync.Mutex
		var deadlocks, timeouts int
		var unexpected []string
		record := func(worker string, err error) {
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil, errors.Is(err, command.ErrConflict), errors.Is(err, command.ErrNotFound), errors.Is(err, command.ErrInvalid):
			case sqlState(err) == "40P01":
				deadlocks++
			case sqlState(err) == "55P03" || sqlState(err) == "57014":
				timeouts++
			default:
				unexpected = append(unexpected, worker+": "+err.Error())
			}
		}
		const rounds = 6
		workers := map[string]func(int) error{
			"ingest": func(i int) error {
				_, err := h.manual(h.token, f.storeA1, t04Key("lc-mixed"), s, claims.ManualClaimInput{BundleID: p.BundleID, Text: fmt.Sprintf("B2+%d", i+1)})
				return err
			},
			"redeem": func(i int) error {
				pv, err := h.preview(owner, lp.Token)
				if err != nil {
					return err
				}
				_, err = h.redeem(owner, t04Key("lc-mixed"), lp.Token, pv.BundleVersion)
				return err
			},
			"issue": func(i int) error {
				page, err := h.bundles(h.token, f.storeA1, s, pageAll)
				if err != nil {
					return err
				}
				for _, b := range page.Items {
					if b.ID == q.BundleID {
						_, err = h.issue(h.token, f.storeA1, t04Key("lc-mixed"), s, q.BundleID, claims.LinkInput{ExpectedGeneration: b.Link.Generation})
					}
				}
				return err
			},
			"offer-update": func(i int) error {
				for _, o := range h.board(t, s).Offers {
					if o.ID == b2.ID {
						_, err := h.updateOffer(h.token, f.storeA1, t04Key("lc-mixed"), s, o.ID, claims.OfferUpdate{ExpectedVersion: o.Version, MaxQuantityPerClaim: int64(900 + i), Active: true})
						return err
					}
				}
				return nil
			},
			"window": func(i int) error {
				w := h.board(t, s).Window
				state := claims.WindowClosed
				if w.State == claims.WindowClosed {
					state = claims.WindowOpen
				}
				_, err := h.setWindow(h.token, f.storeA1, s, claims.WindowInput{ExpectedVersion: w.Version, State: state, MatchMode: w.MatchMode})
				return err
			},
			"update-draft": func(i int) error {
				return h.do(h.token, f.storeA1, func(tx pgx.Tx, sc platform.Scope) error {
					d, err := live.GetDraft(h.ctx, tx, sc, h.token, s)
					if err != nil {
						return err
					}
					_, err = live.UpdateDraft(h.ctx, tx, sc, h.token, t04Key("lc-mixed"), s, d.Version, live.DraftInput{Title: fmt.Sprintf("mixed %d", i), AspectRatio: "9:16"})
					return err
				})
			},
			"put-cart": func(i int) error {
				c := h.cartOf(t, owner)
				_, err := h.putCart(owner, t04Key("lc-mixed"), storefront.CartInput{ExpectedVersion: c.Version, Items: []storefront.Item{{SKUID: h.stock.skus[0].ID, Quantity: int64(i + 1)}, {SKUID: h.stock.skus[1].ID, Quantity: 1}}})
				return err
			},
			"quote": func(i int) error {
				c := h.cartOf(t, owner)
				_, err := cqBuyer(h.a.runtime, owner, func(ctx context.Context, tx pgx.Tx, sc buyer.Scope) (storefront.Quote, error) {
					return storefront.CreateQuote(ctx, tx, sc, t04Key("lc-mixed"), storefront.QuoteInput{CartVersion: c.Version, MarketID: h.market.ID, Country: "TW", Method: "cvs_711"})
				})
				return err
			},
		}
		start := make(chan struct{})
		for name, fn := range workers {
			wg.Add(1)
			go func(name string, fn func(int) error) {
				defer wg.Done()
				<-start
				for i := 0; i < rounds; i++ {
					record(name, fn(i))
				}
			}(name, fn)
		}
		close(start)
		wg.Wait()
		if timeouts > 0 {
			t.Logf("mixed workload: %d lock/statement timeouts (not a 40P01)", timeouts)
		}
		if deadlocks != 0 || len(unexpected) != 0 {
			t.Fatalf("mixed workload deadlocks=%d unexpected=%v", deadlocks, unexpected)
		}
		h.open(t, s, claims.MatchExact)
	})
}

var pageAll = pagination.Request{Limit: 100}

// lcServerLog locates the labelled PostgreSQL fixture container that serves this test
// database (pgfocus / test-local.sh start it with a livecommerce.fixture label) and
// returns a reader for its server log since now. PostgreSQL logs every ERROR with its
// DETAIL, so a label that reached a constraint violation would appear here (P2(b)).
func lcServerLog(t *testing.T, f *testFixture) func() string {
	t.Helper()
	u, err := url.Parse(f.databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command("docker", "ps", "--filter", "label=livecommerce.fixture", "--format", "{{.Names}}\t{{.Ports}}").Output()
	if err != nil {
		t.Fatalf("NOT_RUN: PostgreSQL server log capture needs docker: %v", err)
	}
	name := ""
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		parts := strings.SplitN(line, "\t", 2)
		if len(parts) == 2 && strings.Contains(parts[1], "127.0.0.1:"+u.Port()+"->5432/tcp") {
			name = parts[0]
		}
	}
	if name == "" {
		t.Fatalf("NOT_RUN: no labelled fixture container publishes port %s", u.Port())
	}
	since := time.Now().Add(-time.Second).UTC().Format(time.RFC3339Nano)
	return func() string {
		t.Helper()
		logs, err := exec.Command("docker", "logs", "--since", since, name).CombinedOutput()
		if err != nil {
			t.Fatalf("read PostgreSQL server log: %v", err)
		}
		return string(logs)
	}
}

// lcFind returns every base table (of schemas, or of all non-system schemas when none
// are given) with a row whose text form contains needle. bytea renders as \x-hex in the
// row text, so a hex needle also finds a stored hash. where optionally restricts rows of
// a table (alias t) to this test's own owners/principals.
func lcFind(t *testing.T, f *testFixture, needle string, where map[string]string, schemas ...string) []string {
	t.Helper()
	query := `SELECT format('%I.%I',table_schema,table_name) FROM information_schema.tables WHERE table_type='BASE TABLE'
		AND table_schema NOT IN ('pg_catalog','information_schema') AND table_schema NOT LIKE 'pg\_%'`
	args := []any{}
	if len(schemas) > 0 {
		query += ` AND table_schema=ANY($1)`
		args = append(args, schemas)
	}
	var hits []string
	for _, table := range lcStrings(t, f.owner, query+` ORDER BY 1`, args...) {
		predicate := "true"
		if w, ok := where[table]; ok {
			predicate = w
		}
		var found bool
		if err := f.owner.QueryRow(context.Background(), `SELECT EXISTS(SELECT 1 FROM `+table+` t WHERE strpos(t::text,$1)>0 AND (`+predicate+`))`, needle).Scan(&found); err != nil {
			t.Fatalf("scan %s: %v", table, err)
		}
		if found {
			hits = append(hits, table)
		}
	}
	return hits
}

// TestLiveClaimsKC08ManualPrivacy is KC08 (§4.2 RecordManualClaim, §8 privacy).
func TestLiveClaimsKC08ManualPrivacy(t *testing.T) {
	h := lcSetup(t)
	f := h.f
	logs := lcServerLog(t, f)
	s := h.draft(t, f.storeA1)
	h.open(t, s, claims.MatchExact)
	h.offer(t, s, "A1", h.stock.skus[0].ID, 5)
	h.offer(t, s, "B2", h.stock.skus[1].ID, 999)
	tag := t04Tag()
	amy, bob := "amy-"+tag, "bob-"+tag

	key := t04Key("lc-manual")
	first, err := h.manual(h.token, f.storeA1, key, s, claims.ManualClaimInput{ActorLabel: "@Amy-" + strings.ToUpper(tag) + " ", Text: "A1+2"})
	if err != nil || first.Outcome != claims.OutcomeAccepted || first.Quantity != 2 || first.Keyword != "A1" || first.PreviousQuantity != 0 || first.BundleVersion != 1 || first.LineVersion != 1 {
		t.Fatalf("first manual claim %+v %v", first, err)
	}
	h.accepted(t, s, first.BundleID, "", "A1+4")
	h.accepted(t, s, first.BundleID, "", "B2")
	// Replay returns the original receipt after later commands; normalized label and an
	// equivalent parse replay; any other canonical request under the key is 409 (I02).
	for label, in := range map[string]claims.ManualClaimInput{
		"identical": {ActorLabel: "@Amy-" + strings.ToUpper(tag) + " ", Text: "A1+2"}, "normalized label": {ActorLabel: amy, Text: "A1+2"},
		"equivalent parse": {ActorLabel: amy, Text: "ａ１＋２"},
	} {
		if replay, err := h.manual(h.token, f.storeA1, key, s, in); err != nil || replay != first {
			t.Fatalf("replay (%s) %+v %v", label, replay, err)
		}
	}
	for label, in := range map[string]claims.ManualClaimInput{
		"other label": {ActorLabel: bob, Text: "A1+2"}, "other quantity": {ActorLabel: amy, Text: "A1+3"}, "other keyword": {ActorLabel: amy, Text: "B2+2"},
		"bundle instead of label": {BundleID: first.BundleID, Text: "A1+2"},
	} {
		lcIs(t, lcErr(h.manual(h.token, f.storeA1, key, s, in)), command.ErrConflict, "same key, "+label)
	}
	lcIs(t, lcErr(h.manual(h.token, f.storeA1, t04Key("lc-manual"), s, claims.ManualClaimInput{ActorLabel: "@" + amy, Text: "A1"})), command.ErrConflict, "new actor with a used label")
	second := h.accepted(t, s, "", bob, "A1")
	if second.BundleID == first.BundleID {
		t.Fatal("two labels must be two bundles")
	}
	// A rejected first comment of a new actor leaves no bundle and no label anywhere.
	cat := "cat-" + tag
	for _, text := range []string{"不要A1", "Z9", "A1+9"} {
		r := h.claim(t, s, claims.ManualClaimInput{ActorLabel: cat, Text: text})
		if r.Outcome != claims.OutcomeRejected || r.BundleID != "" {
			t.Fatalf("rejected first comment %q %+v", text, r)
		}
	}
	if hits := lcFind(t, f, cat, nil); len(hits) != 0 {
		t.Fatalf("label of a rejected first comment stored in %v", hits)
	}
	if r := h.accepted(t, s, "", cat, "A1"); r.BundleID == "" {
		t.Fatal("label must remain free after rejected comments")
	}
	// Input validation (§4.2, §4.5).
	otherSession := h.draft(t, f.storeA1)
	for label, tc := range map[string]struct {
		in   claims.ManualClaimInput
		want error
	}{
		"both":          {claims.ManualClaimInput{BundleID: first.BundleID, ActorLabel: "x", Text: "A1"}, command.ErrInvalid},
		"neither":       {claims.ManualClaimInput{Text: "A1"}, command.ErrInvalid},
		"empty text":    {claims.ManualClaimInput{BundleID: first.BundleID}, command.ErrInvalid},
		"257 bytes":     {claims.ManualClaimInput{BundleID: first.BundleID, Text: strings.Repeat("A", 257)}, command.ErrInvalid},
		"control":       {claims.ManualClaimInput{BundleID: first.BundleID, Text: "A1\n"}, command.ErrInvalid},
		"invalid utf8":  {claims.ManualClaimInput{BundleID: first.BundleID, Text: "A1\xff"}, command.ErrInvalid},
		"bad bundle id": {claims.ManualClaimInput{BundleID: "bundle", Text: "A1"}, command.ErrInvalid},
		"bad label":     {claims.ManualClaimInput{ActorLabel: "@", Text: "A1"}, command.ErrInvalid},
		"long label":    {claims.ManualClaimInput{ActorLabel: strings.Repeat("l", 61), Text: "A1"}, command.ErrInvalid},
		"unknown":       {claims.ManualClaimInput{BundleID: randomUUID(), Text: "A1"}, command.ErrNotFound},
	} {
		lcIs(t, lcErr(h.manual(h.token, f.storeA1, t04Key("lc-manual"), s, tc.in)), tc.want, "manual "+label)
	}
	lcIs(t, lcErr(h.manual(h.token, f.storeA1, t04Key("lc-manual"), otherSession, claims.ManualClaimInput{BundleID: first.BundleID, Text: "A1"})), command.ErrNotFound, "bundle of another session")
	_, readerToken := lcPrincipal(t, f, f.tenantA, []string{f.storeA1}, "store:read", "live:read")
	lcIs(t, lcErr(h.manual(readerToken, f.storeA1, t04Key("lc-manual"), s, claims.ManualClaimInput{BundleID: first.BundleID, Text: "A1"})), platform.ErrForbidden, "manual without live:manage")

	// Sentinel texts: never persisted (R5), in any claims/live/ops/buyer/storefront row.
	keywordShaped := "ZQ" + strings.ToUpper(tag[:10])
	texts := []string{"0912345678", "0912345678+0", "A1是不是红色", keywordShaped, keywordShaped + "+7"}
	for _, text := range texts {
		h.claim(t, s, claims.ManualClaimInput{BundleID: first.BundleID, Text: text})
	}
	own := map[string]string{
		"ops.command_results": "t.principal_id='" + h.actor + "'", "ops.audit_events": "t.principal_id='" + h.actor + "'",
	}
	for _, needle := range []string{"0912345678", "A1是不是红色", keywordShaped, lcSHA("0912345678"), lcSHA("A1是不是红色"), lcSHA(keywordShaped)} {
		if hits := lcFind(t, f, needle, own, "claims", "live", "ops"); len(hits) != 0 {
			t.Fatalf("comment text sentinel %q persisted in %v", needle, hits)
		}
	}
	for _, needle := range []string{keywordShaped, lcSHA(keywordShaped), lcSHA("0912345678")} {
		if hits := lcFind(t, f, needle, nil); len(hits) != 0 {
			t.Fatalf("unique text sentinel %q persisted in %v", needle, hits)
		}
	}
	// Label: only claims.bundles.label holds it; its unkeyed SHA-256 is nowhere.
	for _, label := range []string{amy, bob} {
		hits := lcFind(t, f, label, nil)
		if len(hits) != 1 || hits[0] != "claims.bundles" || countRows(t, f.owner, `SELECT count(*) FROM claims.bundles WHERE label=$1`, label) != 1 ||
			countRows(t, f.owner, `SELECT count(*) FROM claims.bundles b WHERE strpos(b::text,$1)>0`, label) != 1 {
			t.Fatalf("label %q found in %v", label, hits)
		}
		for _, digest := range []string{lcSHA(label), lcSHA("@" + label)} {
			if hits := lcFind(t, f, digest, nil); len(hits) != 0 {
				t.Fatalf("unkeyed label hash found in %v", hits)
			}
		}
	}
	server := logs()
	for _, needle := range append(texts, amy, bob, cat, strings.ToUpper(tag)) {
		if strings.Contains(server, needle) {
			t.Fatalf("PostgreSQL server log contains sentinel %q", needle)
		}
	}
}

// TestLiveClaimsP2bLabelPrivacy closes §0.1 P2(b): labels are validated in Go before SQL,
// duplicate labels map to ErrConflict without echo, and no label ever reaches the
// PostgreSQL server log through a constraint-violation DETAIL.
func TestLiveClaimsP2bLabelPrivacy(t *testing.T) {
	h := lcSetup(t)
	f := h.f
	logs := lcServerLog(t, f)
	s := h.draft(t, f.storeA1)
	h.open(t, s, claims.MatchExact)
	h.offer(t, s, "A1", h.stock.skus[0].ID, 5)
	sentinel := "p2b-" + t04Tag()
	h.accepted(t, s, "", sentinel, "A1")
	for _, label := range []string{sentinel, "@" + sentinel, "  " + strings.ToUpper(sentinel) + " ", "＠" + sentinel} {
		_, err := h.manual(h.token, f.storeA1, t04Key("lc-manual"), s, claims.ManualClaimInput{ActorLabel: label, Text: "A1"})
		lcIs(t, err, command.ErrConflict, "duplicate label")
		lcNotPG(t, err, "duplicate label")
		if strings.Contains(strings.ToLower(err.Error()), sentinel) {
			t.Fatalf("duplicate-label error echoes the label: %v", err)
		}
	}
	for _, label := range []string{sentinel + "x\x07", sentinel + "y​", strings.Repeat("z", 61) + sentinel, sentinel + "\xff", " @ "} {
		_, err := h.manual(h.token, f.storeA1, t04Key("lc-manual"), s, claims.ManualClaimInput{ActorLabel: label, Text: "A1"})
		lcIs(t, err, command.ErrInvalid, fmt.Sprintf("invalid label %q", label))
		lcNotPG(t, err, "invalid label")
	}
	// Direct Ingest with a non-canonical label for a new bundle: rejected in Go or stored
	// in canonical form, never a PostgreSQL constraint error.
	for i, label := range []string{" " + sentinel + "-raw ", sentinel + "-ctl\x07", "@@" + sentinel, strings.Repeat("w", 70) + sentinel} {
		r, err := h.ingest(h.ingestIn(s, lcActor(), label, "A1", h.dbNow(t)))
		lcNotPG(t, err, fmt.Sprintf("ingest label %d", i))
		if err != nil && !errors.Is(err, command.ErrInvalid) {
			t.Fatalf("ingest label %q: %v", label, err)
		}
		if err == nil && r.Outcome == claims.OutcomeAccepted {
			var stored string
			if e := f.owner.QueryRow(h.ctx, `SELECT label FROM claims.bundles WHERE id=$1`, r.BundleID).Scan(&stored); e != nil {
				t.Fatal(e)
			}
			if normalized, ok := grammar.NormalizeLabel(stored); !ok || normalized != stored {
				t.Fatalf("ingest stored a non-canonical label %q", stored)
			}
		}
	}
	// Concurrent new actors with one label: one wins, the rest ErrConflict (no raw 23505).
	racer := "p2b-race-" + t04Tag()
	errs := make([]error, 4)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := range errs {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			_, errs[i] = h.manual(h.token, f.storeA1, t04Key("lc-race"), s, claims.ManualClaimInput{ActorLabel: racer, Text: "A1"})
		}(i)
	}
	close(start)
	wg.Wait()
	for _, err := range errs {
		lcNotPG(t, err, "concurrent label")
	}
	if ok, conflicts := lcOutcomes(t, errs); ok != 1 || conflicts != 3 {
		t.Fatalf("concurrent label winners=%d conflicts=%d", ok, conflicts)
	}
	if server := logs(); strings.Contains(server, sentinel) || strings.Contains(server, racer) || strings.Contains(strings.ToLower(server), strings.ToLower(sentinel)) {
		t.Fatal("a manual label reached the PostgreSQL server log")
	}
}

// TestLiveClaimsP2cDuplicateReconstructible closes §0.1 P2(c): a redelivered source
// returns exactly the stored result (including the bundle version it produced, which
// cannot be re-derived after later commands) with Duplicate=true and writes nothing.
func TestLiveClaimsP2cDuplicateReconstructible(t *testing.T) {
	h := lcSetup(t)
	f := h.f
	s := h.draft(t, f.storeA1)
	h.open(t, s, claims.MatchExact)
	h.offer(t, s, "A1", h.stock.skus[0].ID, 9)
	h.offer(t, s, "B2", h.stock.skus[1].ID, 9)
	actor := lcActor()
	var inputs []claims.IngestInput
	var outputs []claims.IngestResult
	for _, text := range []string{"A1", "A1+3", "B2+2", "A1+5", "A1+50", "Z9", "不要"} {
		in := h.ingestIn(s, actor, "p2c", text, h.dbNow(t))
		r, err := h.ingest(in)
		if err != nil {
			t.Fatal(err)
		}
		inputs, outputs = append(inputs, in), append(outputs, r)
	}
	h.accepted(t, s, outputs[0].BundleID, "", "B2+7") // later commands move every version
	snapshot := lcDigest(t, f, "claims")
	for i, in := range inputs {
		dup, err := h.ingest(in)
		want := outputs[i]
		want.Duplicate = true
		if err != nil || !reflect.DeepEqual(dup, want) {
			t.Fatalf("redelivery %d (%s): %+v %v, want %+v", i, in.Text, dup, err, want)
		}
	}
	if outputs[1].BundleVersion != 2 || outputs[3].BundleVersion != 4 || outputs[3].PreviousQuantity != 3 {
		t.Fatalf("fixture versions %+v", outputs)
	}
	lcSameDigest(t, "duplicate ingest wrote", snapshot, lcDigest(t, f, "claims"))
}

// TestLiveClaimsP2dLabelNormalization closes §0.1 P2(d) end to end: "@" is stripped
// before the final trim, the result is idempotent, and the stored label is canonical.
func TestLiveClaimsP2dLabelNormalization(t *testing.T) {
	for raw, want := range map[string]string{"@ Amy": "amy", " @Amy ": "amy", "@　Amy　": "amy", "@ \tAmy  Chen": "amy chen"} {
		got, ok := grammar.NormalizeLabel(raw)
		if !ok || got != want {
			t.Fatalf("NormalizeLabel(%q)=%q,%t want %q", raw, got, ok, want)
		}
		if again, ok := grammar.NormalizeLabel(got); !ok || again != got {
			t.Fatalf("NormalizeLabel not idempotent on %q", got)
		}
	}
	h := lcSetup(t)
	f := h.f
	s := h.draft(t, f.storeA1)
	h.open(t, s, claims.MatchExact)
	h.offer(t, s, "A1", h.stock.skus[0].ID, 5)
	tag := t04Tag()
	key := t04Key("lc-manual")
	first, err := h.manual(h.token, f.storeA1, key, s, claims.ManualClaimInput{ActorLabel: "@ Amy " + strings.ToUpper(tag), Text: "A1"})
	if err != nil || first.Outcome != claims.OutcomeAccepted {
		t.Fatalf("label with '@ ' prefix: %+v %v", first, err)
	}
	var stored string
	if err := f.owner.QueryRow(h.ctx, `SELECT label FROM claims.bundles WHERE id=$1`, first.BundleID).Scan(&stored); err != nil || stored != "amy "+tag {
		t.Fatalf("stored label %q %v", stored, err)
	}
	if replay, err := h.manual(h.token, f.storeA1, key, s, claims.ManualClaimInput{ActorLabel: stored, Text: "A1"}); err != nil || replay != first {
		t.Fatalf("canonical label replays the same receipt: %+v %v", replay, err)
	}
	lcIs(t, lcErr(h.manual(h.token, f.storeA1, t04Key("lc-manual"), s, claims.ManualClaimInput{ActorLabel: "  @AMY  " + tag + " ", Text: "A1"})), command.ErrConflict, "same canonical label, new actor")
	for _, raw := range []string{"@@" + tag, "@ @" + tag, "＠＠" + tag} {
		r, err := h.manual(h.token, f.storeA1, t04Key("lc-manual"), s, claims.ManualClaimInput{ActorLabel: raw, Text: "A1"})
		lcNotPG(t, err, raw)
		if err != nil && !errors.Is(err, command.ErrInvalid) && !errors.Is(err, command.ErrConflict) {
			t.Fatalf("label %q: %v", raw, err)
		}
		if err == nil {
			if e := f.owner.QueryRow(h.ctx, `SELECT label FROM claims.bundles WHERE id=$1`, r.BundleID).Scan(&stored); e != nil {
				t.Fatal(e)
			}
			if again, ok := grammar.NormalizeLabel(stored); !ok || again != stored {
				t.Fatalf("stored label %q is not a fixed point", stored)
			}
		}
	}
}

// TestLiveClaimsP2eModeTransitions closes §0.1 P2(e): match_mode changes only while
// CLOSED; a transition that also changes the mode is ErrConflict and changes nothing.
func TestLiveClaimsP2eModeTransitions(t *testing.T) {
	h := lcSetup(t)
	f := h.f
	s := h.draft(t, f.storeA1)
	h.offer(t, s, "A1", h.stock.skus[0].ID, 5)
	w := h.mustWindow(t, s, 0, claims.WindowClosed, claims.MatchExact)
	unchanged := func(label string) {
		t.Helper()
		lcIs(t, lcErr(h.setWindow(h.token, f.storeA1, s, claims.WindowInput{ExpectedVersion: w.Version, State: labelState(label), MatchMode: labelMode(label)})), command.ErrConflict, label)
		if got := h.board(t, s).Window; !reflect.DeepEqual(got, w) {
			t.Fatalf("%s changed the window: %+v -> %+v", label, w, got)
		}
	}
	unchanged("OPEN/KEYWORD_QTY_ONLY from CLOSED/EXACT")
	w = h.mustWindow(t, s, w.Version, claims.WindowOpen, claims.MatchExact)
	unchanged("CLOSED/KEYWORD_QTY_ONLY from OPEN/EXACT")
	unchanged("OPEN/KEYWORD_QTY_ONLY from OPEN/EXACT")
	w = h.mustWindow(t, s, w.Version, claims.WindowClosed, claims.MatchExact)
	w = h.mustWindow(t, s, w.Version, claims.WindowClosed, claims.MatchKeywordQtyOnly)
	unchanged("OPEN/EXACT from CLOSED/KEYWORD_QTY_ONLY")
	w = h.mustWindow(t, s, w.Version, claims.WindowOpen, claims.MatchKeywordQtyOnly)
	if w.Generation != 2 || w.MatchMode != claims.MatchKeywordQtyOnly {
		t.Fatalf("reopened in the new mode %+v", w)
	}
	if r := h.claim(t, s, claims.ManualClaimInput{ActorLabel: "p2e", Text: "A1"}); r.Reason != claims.ReasonQuantityRequired {
		t.Fatalf("OPEN window mode not applied: %+v", r)
	}
	unchanged("CLOSED/EXACT from OPEN/KEYWORD_QTY_ONLY")
}

func labelState(label string) string { return strings.SplitN(label, "/", 2)[0] }

func labelMode(label string) claims.MatchMode {
	return claims.MatchMode(strings.SplitN(strings.SplitN(label, "/", 2)[1], " ", 2)[0])
}
