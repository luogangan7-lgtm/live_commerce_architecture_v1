// T10 core smoke (commerce_worker, REAL_PG, MOCK manual ingress; no provider). It proves
// only that migrations/0060_live_claims.sql applies (the shared fixture runs
// migrations.Apply twice) and that one happy path works end to end through the frozen
// internal/claims API and the four claims definer functions. It is NOT a contract gate:
// KC02–KC15 are written independently by the test_worker from
// contracts/live-keyword-claims-v1.md.
//
// Isolation: its own merchant principal (live:read/live:manage granted explicitly; no
// backfill exists) and its own session; the store-wide "one OPEN window" rule means the
// window is closed at the end; t.Cleanup purges the session (lcPurgeSessions).

package foundation_test

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"livecommerce/internal/buyer"
	"livecommerce/internal/claims"
	"livecommerce/internal/command"
	"livecommerce/internal/live"
	"livecommerce/internal/pagination"
	"livecommerce/internal/platform"
	"livecommerce/internal/storefront"
)

func TestLiveClaimsCoreSmoke(t *testing.T) {
	h := cqSetup(t)
	f := h.f
	ctx := context.Background()
	var ledger int
	if err := f.owner.QueryRow(ctx, `SELECT count(*) FROM public.lc_schema_migrations WHERE version='0060_live_claims.sql'`).Scan(&ledger); err != nil || ledger != 1 {
		t.Fatalf("0060 not recorded exactly once after two Apply runs: %d %v", ledger, err)
	}

	actor, token := randomUUID(), randomToken()
	mustExec(t, f.owner, `INSERT INTO identity.principals(id) VALUES($1)`, actor)
	mustExec(t, f.owner, `INSERT INTO identity.memberships(tenant_id,principal_id) VALUES($1,$2)`, f.tenantA, actor)
	mustExec(t, f.owner, `INSERT INTO identity.store_grants(tenant_id,store_id,principal_id,permission)
		SELECT $1,$2,$3,p FROM unnest(ARRAY['store:read','live:read','live:manage']) p`, f.tenantA, f.storeA1, actor)
	tx, err := f.owner.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := insertSession(ctx, tx, token, actor, "merchant", time.Now().Add(time.Hour), nil); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	merchant := func(fn func(pgx.Tx, platform.Scope) error) error {
		return platform.WithScope(ctx, f.runtime, token, f.storeA1, "store:read", fn)
	}
	shopper := func(fn func(context.Context, pgx.Tx, buyer.Scope) error) error {
		return buyer.WithScope(ctx, h.a.runtime, h.cap.Token, f.storeA1, fn)
	}

	var draft live.Draft
	if err := merchant(func(tx pgx.Tx, s platform.Scope) (err error) {
		draft, err = live.CreateDraft(ctx, tx, s, token, t04Key("clm-draft"), live.DraftInput{Title: "claims smoke", AspectRatio: "9:16"})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { lcPurgeSessions(t, f, actor) })

	var window claims.Window
	var offer claims.Offer
	if err := merchant(func(tx pgx.Tx, s platform.Scope) (err error) {
		if window, err = claims.SetWindow(ctx, tx, s, token, t04Key("clm-open"), draft.ID,
			claims.WindowInput{ExpectedVersion: 0, State: claims.WindowOpen, MatchMode: claims.MatchExact}); err != nil {
			return err
		}
		offer, err = claims.CreateOffer(ctx, tx, s, token, t04Key("clm-offer"), draft.ID,
			claims.OfferInput{Keyword: "ａ１", SKUID: h.stock.skus[0].ID, MaxQuantityPerClaim: 5})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if window.State != claims.WindowOpen || window.Generation != 1 || window.Version != 1 || window.OpenedAt == nil ||
		offer.Keyword != "A1" || offer.SKUID != h.stock.skus[0].ID || !offer.Active || offer.Version != 1 || offer.SKUCode == "" {
		t.Fatalf("window=%+v offer=%+v", window, offer)
	}

	labels, err := claims.NewLabelKey(randomBytes(32))
	if err != nil {
		t.Fatal(err)
	}
	record := func(key string, in claims.ManualClaimInput) (out claims.ManualClaimResult, err error) {
		err = merchant(func(tx pgx.Tx, s platform.Scope) error {
			out, err = claims.RecordManualClaim(ctx, tx, s, labels, token, key, draft.ID, in)
			return err
		})
		return out, err
	}
	firstKey := t04Key("clm-manual")
	first, err := record(firstKey, claims.ManualClaimInput{ActorLabel: "@Amy ", Text: "A1+2"})
	if err != nil || first.Outcome != claims.OutcomeAccepted || first.Quantity != 2 || first.OfferID != offer.ID ||
		first.Keyword != "A1" || first.BundleVersion != 1 || first.LineVersion != 1 || first.PreviousQuantity != 0 {
		t.Fatalf("first manual claim: %+v %v", first, err)
	}
	if replay, err := record(firstKey, claims.ManualClaimInput{ActorLabel: "amy", Text: "A1+2"}); err != nil || replay != first {
		t.Fatalf("normalized-label replay: %+v %v", replay, err)
	}
	second, err := record(t04Key("clm-manual"), claims.ManualClaimInput{BundleID: first.BundleID, Text: "a1+3"})
	if err != nil || second.Outcome != claims.OutcomeAccepted || second.Quantity != 3 || second.PreviousQuantity != 2 ||
		second.BundleID != first.BundleID || second.BundleVersion != 2 || second.LineVersion != 2 {
		t.Fatalf("second manual claim: %+v %v", second, err)
	}
	rejected, err := record(t04Key("clm-manual"), claims.ManualClaimInput{BundleID: first.BundleID, Text: "不要A1"})
	if err != nil || rejected.Outcome != claims.OutcomeRejected || rejected.Reason != claims.ReasonNoMatch || rejected.BundleID != "" {
		t.Fatalf("NO_MATCH manual claim: %+v %v", rejected, err)
	}

	var board claims.Board
	var page pagination.Page[claims.Bundle]
	var link claims.IssuedLink
	linkKey := t04Key("clm-link")
	if err := merchant(func(tx pgx.Tx, s platform.Scope) (err error) {
		if board, err = claims.GetBoard(ctx, tx, s, token, draft.ID); err != nil {
			return err
		}
		if page, err = claims.ListBundles(ctx, tx, s, token, draft.ID, pagination.Request{}); err != nil {
			return err
		}
		link, err = claims.IssueLink(ctx, tx, s, token, linkKey, draft.ID, first.BundleID, claims.LinkInput{})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if board.Stats.Accepted != 2 || board.Stats.Rejected[claims.ReasonNoMatch] != 1 || len(board.Stats.Rejected) != 7 || len(board.Offers) != 1 {
		t.Fatalf("board: %+v", board)
	}
	if len(page.Items) != 1 || page.NextCursor != "" || page.Items[0].Label != "amy" || page.Items[0].Bound || page.Items[0].Link.State != "NONE" ||
		len(page.Items[0].Lines) != 1 || page.Items[0].Lines[0].Quantity != 3 || page.Items[0].Lines[0].Applied {
		t.Fatalf("bundles: %+v", page)
	}
	token1, err := claims.ParseLinkToken(string(link.Token))
	if err != nil || link.Replayed || link.Generation != 1 || link.Released || !link.ExpiresAt.After(time.Now().Add(71*time.Hour)) {
		t.Fatalf("issued link: gen=%d replayed=%t released=%t expires=%v err=%v", link.Generation, link.Replayed, link.Released, link.ExpiresAt, err)
	}

	var preview claims.Preview
	if err := shopper(func(ctx context.Context, tx pgx.Tx, s buyer.Scope) (err error) {
		preview, err = claims.PreviewLink(ctx, tx, s, token1)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if preview.BundleVersion != 2 || preview.Bound || len(preview.Lines) != 1 || preview.Lines[0].Keyword != "A1" ||
		preview.Lines[0].Quantity != 3 || !preview.Lines[0].Pending || !preview.Lines[0].Available || preview.Lines[0].Currency != "USD" {
		t.Fatalf("preview: %+v", preview)
	}
	redeemKey := t04Key("clm-redeem")
	redeem := func() (out claims.Redeemed, err error) {
		err = shopper(func(ctx context.Context, tx pgx.Tx, s buyer.Scope) error {
			out, err = claims.RedeemLink(ctx, tx, s, redeemKey, token1, claims.RedeemInput{ExpectedBundleVersion: 2})
			return err
		})
		return out, err
	}
	redeemed, err := redeem()
	want := []storefront.Item{{SKUID: h.stock.skus[0].ID, Quantity: 3}}
	if err != nil || redeemed.BundleVersion != 2 || !reflect.DeepEqual(redeemed.Applied, want) || len(redeemed.Skipped) != 0 ||
		!reflect.DeepEqual(redeemed.Cart.Items, want) {
		t.Fatalf("redeem: %+v %v", redeemed, err)
	}
	if replay, err := redeem(); err != nil || !reflect.DeepEqual(replay, redeemed) {
		t.Fatalf("redeem replay: %+v %v", replay, err)
	}
	if err := shopper(func(ctx context.Context, tx pgx.Tx, s buyer.Scope) (err error) {
		preview, err = claims.PreviewLink(ctx, tx, s, token1)
		return err
	}); err != nil || !preview.Bound || preview.Lines[0].Pending {
		t.Fatalf("preview after redeem: %+v %v", preview, err)
	}

	var replayed, rotated claims.IssuedLink
	var closedResult claims.ManualClaimResult
	if err := merchant(func(tx pgx.Tx, s platform.Scope) (err error) {
		if replayed, err = claims.IssueLink(ctx, tx, s, token, linkKey, draft.ID, first.BundleID, claims.LinkInput{}); err != nil {
			return err
		}
		// Release+rotate exercises the definer's ON CONFLICT update and binding release.
		if rotated, err = claims.IssueLink(ctx, tx, s, token, t04Key("clm-link"), draft.ID, first.BundleID,
			claims.LinkInput{ExpectedGeneration: 1, ReleaseBinding: true}); err != nil {
			return err
		}
		_, err = claims.SetWindow(ctx, tx, s, token, t04Key("clm-close"), draft.ID,
			claims.WindowInput{ExpectedVersion: window.Version, State: claims.WindowClosed, MatchMode: claims.MatchExact})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if replayed.Token != "" || !replayed.Replayed || replayed.Generation != 1 || !replayed.ExpiresAt.Equal(link.ExpiresAt) {
		t.Fatalf("link replay: gen=%d replayed=%t", replayed.Generation, replayed.Replayed)
	}
	token2, err := claims.ParseLinkToken(string(rotated.Token))
	if err != nil || rotated.Generation != 2 || !rotated.Released || rotated.Replayed || !rotated.ExpiresAt.After(link.ExpiresAt) {
		t.Fatalf("release+rotate: gen=%d released=%t replayed=%t err=%v", rotated.Generation, rotated.Released, rotated.Replayed, err)
	}
	if err := shopper(func(ctx context.Context, tx pgx.Tx, s buyer.Scope) error {
		_, err := claims.PreviewLink(ctx, tx, s, token1)
		return err
	}); !errors.Is(err, command.ErrNotFound) {
		t.Fatalf("rotated-out token must be not found: %v", err)
	}
	if err := shopper(func(ctx context.Context, tx pgx.Tx, s buyer.Scope) (err error) {
		preview, err = claims.PreviewLink(ctx, tx, s, token2)
		return err
	}); err != nil || preview.Bound || !preview.Lines[0].Pending {
		t.Fatalf("released bundle must be unbound with every line pending again: %+v %v", preview, err)
	}
	closedResult, err = record(t04Key("clm-manual"), claims.ManualClaimInput{BundleID: first.BundleID, Text: "A1"})
	if err != nil || closedResult.Outcome != claims.OutcomeRejected || closedResult.Reason != claims.ReasonWindowClosed {
		t.Fatalf("manual claim after close: %+v %v", closedResult, err)
	}
	var events int
	if err := f.owner.QueryRow(ctx, `SELECT count(*) FROM claims.events WHERE session_id=$1`, draft.ID).Scan(&events); err != nil || events != 3 {
		t.Fatalf("persisted events=%d err=%v (WINDOW_CLOSED must not persist)", events, err)
	}
}
