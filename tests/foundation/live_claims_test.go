// T10 live keyword claims gates, written by the independent test_worker from
// contracts/live-keyword-claims-v1.md (FROZEN, with the §0.1 integrator rulings).
//
// Owns: the shared claims gate harness (lcHarness and its helpers) and gates KC02
// (public-API units: normalizers, token/key strictness, redaction), KC04 (offers) and
// KC05 (claim window). The other gates live beside this file in live_claims_*_test.go:
// schema/isolation (KC03, KC12, P2a, P2g), ingest/manual (KC06-KC08, P2b-P2e),
// link/preview/redeem (KC09-KC11, P2f), HTTP (KC13, KC14) and guards (KC15).
//
// Non-goals: no provider, Meta or browser coverage (KC16), no grammar-vector gate (KC01
// lives in internal/claims/grammar), and no expectation derived from internal/claims
// implementation bodies: every assertion cites a contract section. Tests never weaken a
// contract rule to pass; a failing assertion is a product finding.
//
// The owner (superuser) pool is used only for synthetic setup, fault injection and read-back of columns no
// runtime role may read (claims.links.token_hash, claims.bundles.owner_id), exactly as the contract's own
// KC rows describe.
//
// Isolation: every test owns fresh principals (live:* grants are explicit; there is no
// backfill, contract §12), fresh draft sessions and SKUs. The store-wide "one OPEN
// window" rule (§1) means claims tests run serially (no t.Parallel) and each harness
// closes its own OPEN windows in t.Cleanup. Evidence label: REAL_PG, MOCK manual ingress.
package foundation_test

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"livecommerce/internal/buyer"
	"livecommerce/internal/claims"
	"livecommerce/internal/claims/grammar"
	"livecommerce/internal/command"
	"livecommerce/internal/live"
	"livecommerce/internal/pagination"
	"livecommerce/internal/platform"
	"livecommerce/internal/storefront"
)

// lcHarness is the claims gate harness: cqSetup's store A1 (buyer pools, capability,
// stock, market) plus one merchant principal holding store:read, live:read and
// live:manage on stores A1 and A2, and a fresh server-held label key.
type lcHarness struct {
	cqHarness
	ctx          context.Context
	actor, token string
	labels       claims.LabelKey
}

// lcSetup builds the harness. Cleanup (via lcPrincipal) purges every session this actor
// created, so neither drafts nor OPEN windows leak into later tests on store A1.
func lcSetup(t *testing.T) *lcHarness {
	t.Helper()
	h := &lcHarness{cqHarness: cqSetup(t), ctx: context.Background()}
	h.actor, h.token = lcPrincipal(t, h.f, h.f.tenantA, []string{h.f.storeA1, h.f.storeA2}, "store:read", "live:read", "live:manage")
	var err error
	if h.labels, err = claims.NewLabelKey(randomBytes(32)); err != nil {
		t.Fatal(err)
	}
	return h
}

// lcPrincipal creates a merchant principal of tenant with permissions on every store
// and returns (principal id, merchant bearer token).
func lcPrincipal(t *testing.T, f *testFixture, tenant string, stores []string, permissions ...string) (string, string) {
	t.Helper()
	principal := randomUUID()
	mustExec(t, f.owner, `INSERT INTO identity.principals(id) VALUES($1)`, principal)
	mustExec(t, f.owner, `INSERT INTO identity.memberships(tenant_id,principal_id) VALUES($1,$2)`, tenant, principal)
	for _, store := range stores {
		mustExec(t, f.owner, `INSERT INTO identity.store_grants(tenant_id,store_id,principal_id,permission)
			SELECT $1,$2,$3,p FROM unnest($4::text[]) p`, tenant, store, principal, permissions)
	}
	t.Cleanup(func() { lcPurgeSessions(t, f, principal) })
	return principal, lcToken(t, f, principal)
}

// lcPurgeSessions deletes every live session principal created, with its claims rows
// (owner SQL). Store A1 is the package's shared fixture: leftover drafts leak into later
// store-wide reads (STU01 lists all drafts), leftover OPEN windows into the OPEN index.
func lcPurgeSessions(t *testing.T, f *testFixture, principal string) {
	t.Helper()
	const sessions = `SELECT id FROM live.sessions WHERE principal_id=$1`
	for _, q := range []string{
		`DELETE FROM claims.links WHERE bundle_id IN (SELECT id FROM claims.bundles WHERE session_id IN (` + sessions + `))`,
		`DELETE FROM claims.events WHERE session_id IN (` + sessions + `)`,
		`DELETE FROM claims.lines WHERE session_id IN (` + sessions + `)`,
		`DELETE FROM claims.bundles WHERE session_id IN (` + sessions + `)`,
		`DELETE FROM live.claim_windows WHERE session_id IN (` + sessions + `)`,
		`DELETE FROM live.offers WHERE session_id IN (` + sessions + `)`,
		`DELETE FROM live.programs WHERE session_id IN (` + sessions + `)`,
		`DELETE FROM live.sessions WHERE principal_id=$1`,
	} {
		mustExec(t, f.owner, q, principal)
	}
}

// lcToken issues one more merchant session for principal (so a test may revoke it).
func lcToken(t *testing.T, f *testFixture, principal string) string {
	t.Helper()
	token := randomToken()
	tx, err := f.owner.Begin(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(context.Background())
	if err = insertSession(context.Background(), tx, token, principal, "merchant", time.Now().Add(time.Hour), nil); err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(context.Background()); err != nil {
		t.Fatal(err)
	}
	return token
}

// lcSKUs inserts one active product with n active SKUs (owner SQL, synthetic catalog
// data only) and returns their ids in creation order.
func lcSKUs(t *testing.T, f *testFixture, tenant, store, currency string, n int) []string {
	t.Helper()
	product, tag := randomUUID(), t04Tag()
	mustExec(t, f.owner, `INSERT INTO catalog.products(tenant_id,store_id,id,name) VALUES($1,$2,$3,$4)`, tenant, store, product, "claims gate "+tag)
	mustExec(t, f.owner, `INSERT INTO catalog.skus(tenant_id,store_id,product_id,code,currency,price_minor)
		SELECT $1,$2,$3,$4||'-'||lpad(g::text,4,'0'),$5,1000+g FROM generate_series(1,$6::int) g`, tenant, store, product, "LC-"+tag, currency, n)
	rows, err := f.owner.Query(context.Background(), `SELECT id::text FROM catalog.skus WHERE product_id=$1 ORDER BY code`, product)
	if err != nil {
		t.Fatal(err)
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil || len(ids) != n {
		t.Fatalf("synthetic SKUs: %d %v", len(ids), err)
	}
	return ids
}

func (h *lcHarness) do(token, store string, fn func(pgx.Tx, platform.Scope) error) error {
	return platform.WithScope(h.ctx, h.f.runtime, token, store, "store:read", fn)
}

// draft creates a live DRAFT session owned by the harness actor (windows may open on a
// DRAFT session until T08 defines LIVE, §0.1).
func (h *lcHarness) draft(t *testing.T, store string) string {
	t.Helper()
	return h.draftAs(t, h.token, store)
}

// draftAs creates a DRAFT session with any merchant token holding live:manage (KC12 uses
// another tenant's principal).
func (h *lcHarness) draftAs(t *testing.T, token, store string) string {
	t.Helper()
	var d live.Draft
	err := h.do(token, store, func(tx pgx.Tx, s platform.Scope) (err error) {
		d, err = live.CreateDraft(h.ctx, tx, s, token, t04Key("lc-draft"), live.DraftInput{Title: "claims gate " + t04Tag(), AspectRatio: "9:16"})
		return err
	})
	if err != nil {
		t.Fatalf("create draft in %s: %v", store, err)
	}
	return d.ID
}

func (h *lcHarness) setWindow(token, store, session string, in claims.WindowInput) (out claims.Window, err error) {
	err = h.do(token, store, func(tx pgx.Tx, s platform.Scope) (e error) {
		out, e = claims.SetWindow(h.ctx, tx, s, token, t04Key("lc-window"), session, in)
		return e
	})
	return out, err
}

func (h *lcHarness) mustWindow(t *testing.T, session string, version int64, state string, mode claims.MatchMode) claims.Window {
	t.Helper()
	w, err := h.setWindow(h.token, h.f.storeA1, session, claims.WindowInput{ExpectedVersion: version, State: state, MatchMode: mode})
	if err != nil {
		t.Fatalf("SetWindow %s/%s from v%d: %v", state, mode, version, err)
	}
	return w
}

func (h *lcHarness) getBoard(token, store, session string) (out claims.Board, err error) {
	err = h.do(token, store, func(tx pgx.Tx, s platform.Scope) (e error) {
		out, e = claims.GetBoard(h.ctx, tx, s, token, session)
		return e
	})
	return out, err
}

func (h *lcHarness) board(t *testing.T, session string) claims.Board {
	t.Helper()
	b, err := h.getBoard(h.token, h.f.storeA1, session)
	if err != nil {
		t.Fatalf("GetBoard: %v", err)
	}
	return b
}

// open moves the store-A1 session's window to OPEN in mode (a mode change goes through
// CLOSED first: mode changes only while CLOSED, §1 / P2(e)).
func (h *lcHarness) open(t *testing.T, session string, mode claims.MatchMode) claims.Window {
	t.Helper()
	w := h.board(t, session).Window
	if w.State == claims.WindowOpen {
		if w.MatchMode == mode {
			return w
		}
		w = h.mustWindow(t, session, w.Version, claims.WindowClosed, w.MatchMode)
	}
	if w.Version > 0 && w.MatchMode != mode {
		w = h.mustWindow(t, session, w.Version, claims.WindowClosed, mode)
	}
	return h.mustWindow(t, session, w.Version, claims.WindowOpen, mode)
}

func (h *lcHarness) closeWindow(t *testing.T, session string) claims.Window {
	t.Helper()
	w := h.board(t, session).Window
	if w.State != claims.WindowOpen {
		return w
	}
	return h.mustWindow(t, session, w.Version, claims.WindowClosed, w.MatchMode)
}

func (h *lcHarness) createOffer(token, store, key, session string, in claims.OfferInput) (out claims.Offer, err error) {
	err = h.do(token, store, func(tx pgx.Tx, s platform.Scope) (e error) {
		out, e = claims.CreateOffer(h.ctx, tx, s, token, key, session, in)
		return e
	})
	return out, err
}

func (h *lcHarness) offer(t *testing.T, session, keyword, sku string, max int64) claims.Offer {
	t.Helper()
	o, err := h.createOffer(h.token, h.f.storeA1, t04Key("lc-offer"), session, claims.OfferInput{Keyword: keyword, SKUID: sku, MaxQuantityPerClaim: max})
	if err != nil {
		t.Fatalf("CreateOffer %s: %v", keyword, err)
	}
	return o
}

func (h *lcHarness) updateOffer(token, store, key, session, id string, in claims.OfferUpdate) (out claims.Offer, err error) {
	err = h.do(token, store, func(tx pgx.Tx, s platform.Scope) (e error) {
		out, e = claims.UpdateOffer(h.ctx, tx, s, token, key, session, id, in)
		return e
	})
	return out, err
}

func (h *lcHarness) setOffer(t *testing.T, session string, o claims.Offer, max int64, active bool) claims.Offer {
	t.Helper()
	out, err := h.updateOffer(h.token, h.f.storeA1, t04Key("lc-offer-set"), session, o.ID, claims.OfferUpdate{ExpectedVersion: o.Version, MaxQuantityPerClaim: max, Active: active})
	if err != nil {
		t.Fatalf("UpdateOffer %s active=%t: %v", o.Keyword, active, err)
	}
	return out
}

func (h *lcHarness) manual(token, store, key, session string, in claims.ManualClaimInput) (out claims.ManualClaimResult, err error) {
	err = h.do(token, store, func(tx pgx.Tx, s platform.Scope) (e error) {
		out, e = claims.RecordManualClaim(h.ctx, tx, s, h.labels, token, key, session, in)
		return e
	})
	return out, err
}

func (h *lcHarness) claim(t *testing.T, session string, in claims.ManualClaimInput) claims.ManualClaimResult {
	t.Helper()
	r, err := h.manual(h.token, h.f.storeA1, t04Key("lc-manual"), session, in)
	if err != nil {
		t.Fatalf("RecordManualClaim: %v", err)
	}
	return r
}

// accepted records a manual claim that must be ACCEPTED (bundle "" = new actor label).
func (h *lcHarness) accepted(t *testing.T, session, bundle, label, text string) claims.ManualClaimResult {
	t.Helper()
	r := h.claim(t, session, claims.ManualClaimInput{BundleID: bundle, ActorLabel: label, Text: text})
	if r.Outcome != claims.OutcomeAccepted || !command.ValidID(r.BundleID) {
		t.Fatalf("manual claim %q not accepted: %+v", text, r)
	}
	return r
}

func (h *lcHarness) issue(token, store, key, session, bundle string, in claims.LinkInput) (out claims.IssuedLink, err error) {
	err = h.do(token, store, func(tx pgx.Tx, s platform.Scope) (e error) {
		out, e = claims.IssueLink(h.ctx, tx, s, token, key, session, bundle, in)
		return e
	})
	return out, err
}

// link issues or rotates a store-A1 link and checks the first-execution token shape (§6).
func (h *lcHarness) link(t *testing.T, session, bundle string, generation int64, release bool) claims.IssuedLink {
	t.Helper()
	l, err := h.issue(h.token, h.f.storeA1, t04Key("lc-link"), session, bundle, claims.LinkInput{ExpectedGeneration: generation, ReleaseBinding: release})
	if err != nil {
		t.Fatalf("IssueLink gen=%d release=%t: %v", generation, release, err)
	}
	if _, perr := claims.ParseLinkToken(string(l.Token)); perr != nil || len(l.Token) != 43 || l.Replayed || l.Generation != generation+1 {
		t.Fatalf("issued link: len=%d gen=%d replayed=%t parse=%v", len(l.Token), l.Generation, l.Replayed, perr)
	}
	return l
}

func (h *lcHarness) bundles(token, store, session string, page pagination.Request) (out pagination.Page[claims.Bundle], err error) {
	err = h.do(token, store, func(tx pgx.Tx, s platform.Scope) (e error) {
		out, e = claims.ListBundles(h.ctx, tx, s, token, session, page)
		return e
	})
	return out, err
}

func (h *lcHarness) preview(c buyer.Capability, token claims.LinkToken) (claims.Preview, error) {
	return cqBuyer(h.a.runtime, c, func(ctx context.Context, tx pgx.Tx, s buyer.Scope) (claims.Preview, error) {
		return claims.PreviewLink(ctx, tx, s, token)
	})
}

func (h *lcHarness) redeem(c buyer.Capability, key string, token claims.LinkToken, version int64) (claims.Redeemed, error) {
	return cqBuyer(h.a.runtime, c, func(ctx context.Context, tx pgx.Tx, s buyer.Scope) (claims.Redeemed, error) {
		return claims.RedeemLink(ctx, tx, s, key, token, claims.RedeemInput{ExpectedBundleVersion: version})
	})
}

func (h *lcHarness) cartOf(t *testing.T, c buyer.Capability) storefront.Cart {
	t.Helper()
	cart, err := cqBuyer(h.a.runtime, c, func(ctx context.Context, tx pgx.Tx, s buyer.Scope) (storefront.Cart, error) {
		return storefront.GetCart(ctx, tx, s)
	})
	if err != nil {
		t.Fatalf("GetCart: %v", err)
	}
	return cart
}

func (h *lcHarness) putCart(c buyer.Capability, key string, in storefront.CartInput) (storefront.Cart, error) {
	return cqBuyer(h.a.runtime, c, func(ctx context.Context, tx pgx.Tx, s buyer.Scope) (storefront.Cart, error) {
		return storefront.SetCart(ctx, tx, s, key, in)
	})
}

// bgMerchant runs op in its own merchant transaction on a goroutine and returns the
// backend pid first, so the caller can observe a lock wait in pg_stat_activity
// (lpWaitLock) instead of sleeping.
func (h *lcHarness) bgMerchant(t *testing.T, token, store string, op func(pgx.Tx, platform.Scope) error) (int, <-chan error) {
	t.Helper()
	pids, done := make(chan int, 1), make(chan error, 1)
	go func() {
		done <- h.do(token, store, func(tx pgx.Tx, s platform.Scope) error {
			var pid int
			if err := tx.QueryRow(h.ctx, `SELECT pg_backend_pid()`).Scan(&pid); err != nil {
				return err
			}
			pids <- pid
			return op(tx, s)
		})
	}()
	return lcPID(t, pids, done)
}

// bgBuyer is bgMerchant for a buyer.WithScope transaction.
func (h *lcHarness) bgBuyer(t *testing.T, c buyer.Capability, op func(context.Context, pgx.Tx, buyer.Scope) error) (int, <-chan error) {
	t.Helper()
	pids, done := make(chan int, 1), make(chan error, 1)
	go func() {
		done <- buyer.WithScope(h.ctx, h.a.runtime, c.Token, c.Scope.StoreID, func(ctx context.Context, tx pgx.Tx, s buyer.Scope) error {
			var pid int
			if err := tx.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&pid); err != nil {
				return err
			}
			pids <- pid
			return op(ctx, tx, s)
		})
	}()
	return lcPID(t, pids, done)
}

func lcPID(t *testing.T, pids <-chan int, done <-chan error) (int, <-chan error) {
	t.Helper()
	select {
	case pid := <-pids:
		return pid, done
	case err := <-done:
		t.Fatalf("background transaction ended before reporting its pid: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("background transaction did not start")
	}
	return 0, done
}

// lcHold is an open transaction that already did its work and holds its locks until
// finish (commit) is called; t.Cleanup always finishes it.
type lcHold struct {
	release  chan struct{}
	done     chan error
	once     sync.Once
	finished bool
	result   error
}

func (x *lcHold) finish(t *testing.T) error {
	t.Helper()
	x.once.Do(func() { close(x.release) })
	if !x.finished {
		x.result, x.finished = waitError(t, x.done), true
	}
	return x.result
}

func (h *lcHarness) hold(t *testing.T, run func(fn func() error) error, op func() error) *lcHold {
	t.Helper()
	x := &lcHold{release: make(chan struct{}), done: make(chan error, 1)}
	ready := make(chan error, 1)
	go func() {
		x.done <- run(func() error {
			if err := op(); err != nil {
				ready <- err
				return err
			}
			ready <- nil
			<-x.release
			return nil
		})
	}()
	select {
	case err := <-ready:
		if err != nil {
			t.Fatalf("held transaction work failed: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("held transaction did not become ready")
	}
	t.Cleanup(func() {
		x.once.Do(func() { close(x.release) })
		if !x.finished {
			select {
			case <-x.done:
			case <-time.After(6 * time.Second):
				t.Error("held transaction did not end")
			}
		}
	})
	return x
}

func (h *lcHarness) holdMerchant(t *testing.T, token, store string, op func(pgx.Tx, platform.Scope) error) *lcHold {
	t.Helper()
	var tx pgx.Tx
	var scope platform.Scope
	return h.hold(t, func(fn func() error) error {
		return h.do(token, store, func(inner pgx.Tx, s platform.Scope) error { tx, scope = inner, s; return fn() })
	}, func() error { return op(tx, scope) })
}

func (h *lcHarness) holdBuyer(t *testing.T, c buyer.Capability, op func(context.Context, pgx.Tx, buyer.Scope) error) *lcHold {
	t.Helper()
	var tx pgx.Tx
	var scope buyer.Scope
	var ctx context.Context
	return h.hold(t, func(fn func() error) error {
		return buyer.WithScope(h.ctx, h.a.runtime, c.Token, c.Scope.StoreID, func(inner context.Context, itx pgx.Tx, s buyer.Scope) error {
			ctx, tx, scope = inner, itx, s
			return fn()
		})
	}, func() error { return op(ctx, tx, scope) })
}

// lcOwner reads claims.bundles.owner_id (no runtime role may read it, §3.2).
func lcOwner(t *testing.T, f *testFixture, bundle string) string {
	t.Helper()
	var owner string
	if err := f.owner.QueryRow(context.Background(), `SELECT coalesce(owner_id::text,'') FROM claims.bundles WHERE id=$1`, bundle).Scan(&owner); err != nil {
		t.Fatalf("read binding: %v", err)
	}
	return owner
}

type lcLinkRow struct {
	hash             []byte
	generation       int64
	issued, expires  time.Time
	exactly72, found bool
}

// lcLinkState reads the stored link row of a bundle (owner SQL; token_hash is writer-only).
func lcLinkState(t *testing.T, f *testFixture, bundle string) (row lcLinkRow) {
	t.Helper()
	err := f.owner.QueryRow(context.Background(), `SELECT token_hash,generation,issued_at,expires_at,expires_at-issued_at=interval '72 hours'
		FROM claims.links WHERE bundle_id=$1`, bundle).Scan(&row.hash, &row.generation, &row.issued, &row.expires, &row.exactly72)
	if errors.Is(err, pgx.ErrNoRows) {
		return row
	}
	if err != nil {
		t.Fatalf("read link: %v", err)
	}
	row.found = true
	return row
}

// lcDigest fingerprints the full content of every base table in schemas, so a gate can
// prove that claims code wrote nothing there (§1: claims never touch inventory, carts...).
func lcDigest(t *testing.T, f *testFixture, schemas ...string) map[string]string {
	t.Helper()
	rows, err := f.owner.Query(context.Background(), `SELECT format('%I.%I',table_schema,table_name) FROM information_schema.tables
		WHERE table_schema=ANY($1) AND table_type='BASE TABLE' ORDER BY 1`, schemas)
	if err != nil {
		t.Fatal(err)
	}
	tables, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil || len(tables) == 0 {
		t.Fatalf("digest tables: %v %v", tables, err)
	}
	out := map[string]string{}
	for _, table := range tables {
		var digest string
		if err := f.owner.QueryRow(context.Background(), `SELECT count(*)::text||':'||coalesce(md5(string_agg(t::text,E'\n' ORDER BY t::text)),'') FROM `+table+` t`).Scan(&digest); err != nil {
			t.Fatalf("digest %s: %v", table, err)
		}
		out[table] = digest
	}
	return out
}

func lcSameDigest(t *testing.T, label string, before, after map[string]string) {
	t.Helper()
	for table, digest := range before {
		if after[table] != digest {
			t.Fatalf("%s: %s changed (%s -> %s)", label, table, digest, after[table])
		}
	}
	if len(after) != len(before) {
		t.Fatalf("%s: table set changed", label)
	}
}

func lcIs(t *testing.T, err, want error, label string) {
	t.Helper()
	if !errors.Is(err, want) {
		t.Fatalf("%s: err=%v, want %v", label, err, want)
	}
}

// lcNotPG asserts an error carries no PostgreSQL error (whose message/DETAIL could echo
// personal data, P2(b)).
func lcNotPG(t *testing.T, err error, label string) {
	t.Helper()
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		t.Fatalf("%s: raw PostgreSQL error %s escaped the claims error mapping", label, pgErr.Code)
	}
}

func lcItems(items []storefront.Item) map[string]int64 {
	out := map[string]int64{}
	for _, item := range items {
		out[item.SKUID] = item.Quantity
	}
	return out
}

func lcSkipped(skipped []claims.Skipped) []claims.Skipped {
	out := append([]claims.Skipped(nil), skipped...)
	sort.Slice(out, func(i, j int) bool { return out[i].SKUID+out[i].Reason < out[j].SKUID+out[j].Reason })
	return out
}

func lcSHA(s string) string { sum := sha256.Sum256([]byte(s)); return hex.EncodeToString(sum[:]) }

// TestLiveClaimsKC02Units is KC02 over the exported API only (UNIT; no database):
// NormalizeKeyword/NormalizeLabel vectors (§2.1-§2.2, P2(d)), ParseLinkToken strictness
// (§4.2, §6), NewLabelKey size (§4.2) and redaction of every input/credential type (§4,
// §8). The label_mac scope separation is not observable through the exported API and
// is covered by KC08's receipt behaviour (same key + "@Amy "/"amy" replays; another
// label conflicts).
func TestLiveClaimsKC02Units(t *testing.T) {
	for raw, want := range map[string]string{
		"A1": "A1", "a1": "A1", "ａ１": "A1", "  a1\t": "A1", "\nA1\r": "A1", "Ａ１７": "A17", "101": "101",
		"ABCDEFGHIJKLMNOP": "ABCDEFGHIJKLMNOP", "a1x2": "A1X2", "A1\u3000": "A1", "\u00a0b2": "B2",
	} {
		if got, ok := grammar.NormalizeKeyword(raw); !ok || got != want {
			t.Fatalf("NormalizeKeyword(%q)=%q,%t want %q", raw, got, ok, want)
		}
	}
	for _, raw := range []string{"", "   ", "A1+2", "A1+", "+2", "A 1", "ABCDEFGHIJKLMNOPQ", "\u017f1", "\u01311", "\u212a1", "A1\u200b",
		"\ufeffA1", "①", "A²", "A-1", "#A1", "A1?", "不要", "A1👍", "A1\xff"} {
		if got, ok := grammar.NormalizeKeyword(raw); ok {
			t.Fatalf("NormalizeKeyword(%q) accepted %q", raw, got)
		}
	}
	sixty := strings.Repeat("a", 60)
	labels := map[string]string{
		"@Amy ": "amy", "amy": "amy", "@ Amy": "amy", "  @Amy  ": "amy", "  Amy   Chen  ": "amy chen", "Amy\t\tChen": "amy chen",
		"ＡＭＹ": "amy", "＠Amy": "amy", "Amy\u3000Chen": "amy chen", "Amy\u00a0Chen": "amy chen", "陳小美": "陳小美", sixty: sixty, "@" + sixty: sixty,
	}
	for raw, want := range labels {
		got, ok := grammar.NormalizeLabel(raw)
		if !ok || got != want {
			t.Fatalf("NormalizeLabel(%q)=%q,%t want %q", raw, got, ok, want)
		}
	}
	for _, raw := range []string{"", "   ", "@", "@ ", " @ ", strings.Repeat("a", 61), "a\x07b", "a\x00b", "a\u200bb", "a\u200eb", "a\ufeffb", "amy\xff"} {
		if got, ok := grammar.NormalizeLabel(raw); ok {
			t.Fatalf("NormalizeLabel(%q) accepted %q", raw, got)
		}
	}
	// P2(d): idempotent on every accepted output, including @-prefixed tricks.
	for _, raw := range []string{"@Amy ", "@@amy", "@ @amy", " @ amy", "@＠amy", "  Amy   Chen  ", "陳小美", "@" + sixty, "@a"} {
		if y, ok := grammar.NormalizeLabel(raw); ok {
			if again, ok2 := grammar.NormalizeLabel(y); !ok2 || again != y {
				t.Fatalf("NormalizeLabel not idempotent: %q -> %q -> %q,%t", raw, y, again, ok2)
			}
		}
	}

	valid := base64.RawURLEncoding.EncodeToString(randomBytes(32))
	if tok, err := claims.ParseLinkToken(valid); err != nil || string(tok) != valid {
		t.Fatalf("canonical token rejected: %v", err)
	}
	const alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_"
	last := strings.IndexByte(alphabet, valid[42])
	nonCanonical := valid[:42] + string(alphabet[last|1]) // non-zero trailing bits decode to the same bytes
	for name, raw := range map[string]string{
		"empty": "", "short": valid[:42], "long": valid + "A", "padded": valid + "=", "space": " " + valid[1:], "trailing-bits": nonCanonical,
		"std-plus": "+" + valid[1:], "std-slash": valid[:20] + "/" + valid[21:], "std-padded": base64.StdEncoding.EncodeToString(randomBytes(32)),
		"newline": valid[:42] + "\n", "unicode": valid[:41] + "é",
	} {
		if _, err := claims.ParseLinkToken(raw); !errors.Is(err, command.ErrInvalid) {
			t.Fatalf("ParseLinkToken %s accepted or wrong error: %v", name, err)
		}
	}
	for _, n := range []int{0, 1, 31, 33, 64} {
		if _, err := claims.NewLabelKey(randomBytes(n)); !errors.Is(err, command.ErrInvalid) {
			t.Fatalf("NewLabelKey(%d bytes): %v", n, err)
		}
	}
	if _, err := claims.NewLabelKey(nil); !errors.Is(err, command.ErrInvalid) {
		t.Fatalf("NewLabelKey(nil): %v", err)
	}

	// Redaction (§4, §8): every fmt verb and JSON of every listed type hides its secrets.
	raw := randomBytes(32)
	key, err := claims.NewLabelKey(raw)
	if err != nil {
		t.Fatal(err)
	}
	actorKey := hex.EncodeToString(randomBytes(32))
	label := "sentinel-label-" + t04Tag()
	text := "A1是不是红色 0912345678"
	tok := claims.LinkToken(valid)
	secrets := []string{"0912345678", "A1是不是红色", label, actorKey, valid, hex.EncodeToString(raw), base64.StdEncoding.EncodeToString(raw),
		base64.RawURLEncoding.EncodeToString(raw), strings.Trim(fmt.Sprint(raw), "[]")}
	render := func(name string, v any) []string {
		body, err := json.Marshal(v)
		if err != nil {
			t.Fatalf("%s json: %v", name, err)
		}
		return []string{fmt.Sprintf("%v", v), fmt.Sprintf("%+v", v), fmt.Sprintf("%#v", v), string(body),
			fmt.Sprintf("%v", []any{v}), fmt.Sprintf("%+v", struct{ V any }{v})}
	}
	values := map[string]any{
		"grammar.Result":   grammar.Parse("0912345678"),
		"IngestInput":      claims.IngestInput{TenantID: randomUUID(), StoreID: randomUUID(), SessionID: randomUUID(), SourceKind: "manual", SourceEventID: randomUUID(), Platform: "manual", ActorKey: actorKey, ActorLabel: label, PrincipalID: randomUUID(), Text: text, OccurredAt: time.Now()},
		"ManualClaimInput": claims.ManualClaimInput{ActorLabel: label, Text: text},
		"LabelKey":         key,
		"LinkToken":        tok,
		"IssuedLink":       claims.IssuedLink{Token: tok, Generation: 1, ExpiresAt: time.Now()},
	}
	if r := grammar.Parse("0912345678"); r.Kind != grammar.Match || r.Keyword != "0912345678" {
		t.Fatalf("redaction fixture must be a phone-shaped MATCH: %v", r.Kind)
	}
	for name, v := range values {
		for _, out := range render(name, v) {
			for _, secret := range secrets {
				if strings.Contains(out, secret) {
					t.Fatalf("%s rendering leaked a sentinel: %q", name, out)
				}
			}
		}
	}
	// Constant "[redacted]" String/GoString/MarshalJSON for the listed input/credential types.
	type redacted interface {
		String() string
		GoString() string
		MarshalJSON() ([]byte, error)
	}
	for name, v := range values {
		if name == "grammar.Result" {
			continue // redacted to Version and Kind only (§4.1)
		}
		r, ok := v.(redacted)
		if !ok {
			t.Fatalf("%s lacks String/GoString/MarshalJSON", name)
		}
		body, err := r.MarshalJSON()
		if r.String() != "[redacted]" || r.GoString() != "[redacted]" || err != nil || string(body) != `"[redacted]"` {
			t.Fatalf("%s is not constant [redacted]: %q %q %s %v", name, r.String(), r.GoString(), body, err)
		}
	}
	if out := fmt.Sprintf("%v", grammar.Parse("0912345678")); !strings.Contains(out, "MATCH") {
		t.Fatalf("grammar.Result must still show its Kind: %q", out)
	}
	// Redaction never affects decoding of the M5 body (§4.2 json tags).
	var decoded claims.ManualClaimInput
	if err := json.Unmarshal([]byte(`{"bundle_id":"","actor_label":"@Amy ","text":"A1+2"}`), &decoded); err != nil || decoded.ActorLabel != "@Amy " || decoded.Text != "A1+2" {
		t.Fatalf("ManualClaimInput decode: %v", err)
	}
}

// TestLiveClaimsKC04Offers is KC04 (§4.2 CreateOffer/UpdateOffer rules, §1 offer model).
func TestLiveClaimsKC04Offers(t *testing.T) {
	h := lcSetup(t)
	f := h.f
	s1, s2 := h.draft(t, f.storeA1), h.draft(t, f.storeA1)
	sku0, sku1 := h.stock.skus[0], h.stock.skus[1]

	// Create: replay (canonical keyword before hashing), changed body 409.
	key := t04Key("lc-offer")
	first, err := h.createOffer(h.token, f.storeA1, key, s1, claims.OfferInput{Keyword: "ａ１", SKUID: sku0.ID, MaxQuantityPerClaim: 3})
	if err != nil {
		t.Fatal(err)
	}
	if !command.ValidID(first.ID) || first.SessionID != s1 || first.Keyword != "A1" || first.SKUID != sku0.ID || first.SKUCode != sku0.Code ||
		first.ProductName != h.stock.product.Name || first.MaxQuantityPerClaim != 3 || !first.Active || first.Version != 1 || first.ActivatedAt.IsZero() || first.UpdatedAt.IsZero() {
		t.Fatalf("created offer %+v", first)
	}
	for _, kw := range []string{"a1", "A1", " A1 ", "ａ１"} {
		replay, err := h.createOffer(h.token, f.storeA1, key, s1, claims.OfferInput{Keyword: kw, SKUID: sku0.ID, MaxQuantityPerClaim: 3})
		if err != nil || !reflect.DeepEqual(replay, first) {
			t.Fatalf("canonical replay %q: %+v %v", kw, replay, err)
		}
	}
	for name, in := range map[string]claims.OfferInput{
		"max":     {Keyword: "A1", SKUID: sku0.ID, MaxQuantityPerClaim: 4},
		"keyword": {Keyword: "A2", SKUID: sku0.ID, MaxQuantityPerClaim: 3},
		"sku":     {Keyword: "A1", SKUID: sku1.ID, MaxQuantityPerClaim: 3},
	} {
		if _, err := h.createOffer(h.token, f.storeA1, key, s1, in); !errors.Is(err, command.ErrConflict) {
			t.Fatalf("changed create body (%s) under one key: %v", name, err)
		}
	}
	if _, err := h.createOffer(h.token, f.storeA1, key, s1, claims.OfferInput{Keyword: "A1", SKUID: sku0.ID, MaxQuantityPerClaim: 3}); err != nil {
		t.Fatalf("replay after conflicts: %v", err)
	}
	if n := countRows(t, f.owner, `SELECT count(*) FROM live.offers WHERE session_id=$1`, s1); n != 1 {
		t.Fatalf("replays created %d offers", n)
	}
	// Duplicate keyword; second active offer on one SKU (create); same keyword in another session.
	lcIs(t, lcErr(h.createOffer(h.token, f.storeA1, t04Key("lc-offer"), s1, claims.OfferInput{Keyword: "a1", SKUID: sku1.ID, MaxQuantityPerClaim: 3})), command.ErrConflict, "duplicate keyword")
	lcIs(t, lcErr(h.createOffer(h.token, f.storeA1, t04Key("lc-offer"), s1, claims.OfferInput{Keyword: "B1", SKUID: sku0.ID, MaxQuantityPerClaim: 3})), command.ErrConflict, "second active offer on SKU")
	other := h.offer(t, s2, "A1", sku0.ID, 3)
	if other.ID == first.ID || other.SessionID != s2 {
		t.Fatalf("other-session offer %+v", other)
	}
	// Invalid inputs are ErrInvalid before any write (§4.5).
	for name, in := range map[string]claims.OfferInput{
		"plus": {Keyword: "A1+2", SKUID: sku1.ID, MaxQuantityPerClaim: 1}, "empty": {SKUID: sku1.ID, MaxQuantityPerClaim: 1},
		"17": {Keyword: strings.Repeat("K", 17), SKUID: sku1.ID, MaxQuantityPerClaim: 1}, "kelvin": {Keyword: "\u212a1", SKUID: sku1.ID, MaxQuantityPerClaim: 1},
		"max0": {Keyword: "C1", SKUID: sku1.ID, MaxQuantityPerClaim: 0}, "max1000": {Keyword: "C1", SKUID: sku1.ID, MaxQuantityPerClaim: 1000},
		"sku": {Keyword: "C1", SKUID: "not-a-uuid", MaxQuantityPerClaim: 1}, "sku-upper": {Keyword: "C1", SKUID: strings.ToUpper(sku1.ID), MaxQuantityPerClaim: 1},
	} {
		lcIs(t, lcErr(h.createOffer(h.token, f.storeA1, t04Key("lc-offer-bad"), s1, in)), command.ErrInvalid, "invalid offer "+name)
	}
	lcIs(t, lcErr(h.createOffer(h.token, f.storeA1, t04Key("lc-offer-bad"), "not-a-session", claims.OfferInput{Keyword: "C1", SKUID: sku1.ID, MaxQuantityPerClaim: 1})), command.ErrInvalid, "invalid session id")
	lcIs(t, lcErr(h.createOffer(h.token, f.storeA1, t04Key("lc-offer"), randomUUID(), claims.OfferInput{Keyword: "C1", SKUID: sku1.ID, MaxQuantityPerClaim: 1})), command.ErrNotFound, "missing session")
	// SKU missing 404 / cross-store 404 / archived SKU, archived product, foreign currency 409.
	archived := lcSKUs(t, f, f.tenantA, f.storeA1, "USD", 2)
	mustExec(t, f.owner, `UPDATE catalog.skus SET status='archived' WHERE id=$1`, archived[0])
	archivedProduct := lcSKUs(t, f, f.tenantA, f.storeA1, "USD", 1)
	mustExec(t, f.owner, `UPDATE catalog.products SET status='archived' WHERE id=(SELECT product_id FROM catalog.skus WHERE id=$1)`, archivedProduct[0])
	foreignCurrency := lcSKUs(t, f, f.tenantA, f.storeA1, "TWD", 1)
	crossStore := lcSKUs(t, f, f.tenantA, f.storeA2, "USD", 1)
	crossTenant := lcSKUs(t, f, f.tenantB, f.storeB, "TWD", 1)
	for name, tc := range map[string]struct {
		sku  string
		want error
	}{
		"missing": {randomUUID(), command.ErrNotFound}, "cross-store": {crossStore[0], command.ErrNotFound}, "cross-tenant": {crossTenant[0], command.ErrNotFound},
		"archived-sku": {archived[0], command.ErrConflict}, "archived-product": {archivedProduct[0], command.ErrConflict}, "foreign-currency": {foreignCurrency[0], command.ErrConflict},
	} {
		lcIs(t, lcErr(h.createOffer(h.token, f.storeA1, t04Key("lc-offer"), s1, claims.OfferInput{Keyword: "S1", SKUID: tc.sku, MaxQuantityPerClaim: 1})), tc.want, "offer SKU "+name)
	}

	// Update: replay, changed body 409, stale CAS 409, concurrent CAS one winner.
	upKey := t04Key("lc-offer-up")
	up := claims.OfferUpdate{ExpectedVersion: 1, MaxQuantityPerClaim: 5, Active: true}
	u1, err := h.updateOffer(h.token, f.storeA1, upKey, s1, first.ID, up)
	if err != nil || u1.Version != 2 || u1.MaxQuantityPerClaim != 5 || !u1.Active || !u1.ActivatedAt.Equal(first.ActivatedAt) || u1.Keyword != "A1" || u1.SKUID != sku0.ID {
		t.Fatalf("update %+v %v", u1, err)
	}
	if replay, err := h.updateOffer(h.token, f.storeA1, upKey, s1, first.ID, up); err != nil || !reflect.DeepEqual(replay, u1) {
		t.Fatalf("update replay %+v %v", replay, err)
	}
	up.MaxQuantityPerClaim = 6
	lcIs(t, lcErr(h.updateOffer(h.token, f.storeA1, upKey, s1, first.ID, up)), command.ErrConflict, "changed update body")
	lcIs(t, lcErr(h.updateOffer(h.token, f.storeA1, t04Key("lc-offer-up"), s1, first.ID, claims.OfferUpdate{ExpectedVersion: 1, MaxQuantityPerClaim: 7, Active: true})), command.ErrConflict, "stale offer version")
	lcIs(t, lcErr(h.updateOffer(h.token, f.storeA1, t04Key("lc-offer-up"), s1, first.ID, claims.OfferUpdate{ExpectedVersion: 2, MaxQuantityPerClaim: 0, Active: true})), command.ErrInvalid, "update max 0")
	lcIs(t, lcErr(h.updateOffer(h.token, f.storeA1, t04Key("lc-offer-up"), s2, first.ID, claims.OfferUpdate{ExpectedVersion: 2, MaxQuantityPerClaim: 2, Active: true})), command.ErrNotFound, "offer of another session")
	cas := make([]error, 4)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := range cas {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			_, cas[i] = h.updateOffer(h.token, f.storeA1, t04Key("lc-offer-cas"), s1, first.ID, claims.OfferUpdate{ExpectedVersion: 2, MaxQuantityPerClaim: int64(10 + i), Active: true})
		}(i)
	}
	close(start)
	wg.Wait()
	if ok, conflicts := lcOutcomes(t, cas); ok != 1 || conflicts != len(cas)-1 {
		t.Fatalf("offer CAS race winners=%d conflicts=%d", ok, conflicts)
	}
	current := h.board(t, s1).Offers[0]
	if current.Version != 3 {
		t.Fatalf("offer after CAS race %+v", current)
	}

	// Reactivation conflicts while another active offer holds the SKU; activated_at moves.
	inactive := h.setOffer(t, s1, current, current.MaxQuantityPerClaim, false)
	holder := h.offer(t, s1, "B1", sku0.ID, 2) // allowed: A1 no longer active on sku0
	lcIs(t, lcErr(h.updateOffer(h.token, f.storeA1, t04Key("lc-offer-up"), s1, inactive.ID, claims.OfferUpdate{ExpectedVersion: inactive.Version, MaxQuantityPerClaim: 3, Active: true})), command.ErrConflict, "reactivation while SKU held")
	h.setOffer(t, s1, holder, 2, false)
	reactivated := h.setOffer(t, s1, inactive, 3, true)
	if !reactivated.ActivatedAt.After(first.ActivatedAt) || !reactivated.Active {
		t.Fatalf("reactivation must move activated_at: %v -> %v", first.ActivatedAt, reactivated.ActivatedAt)
	}
	// Keyword and SKU are immutable (no UPDATE grant, §3.2) -> 42501.
	for _, q := range []string{`UPDATE live.offers SET keyword='Z1' WHERE id=$1`, `UPDATE live.offers SET sku_id=sku_id WHERE id=$1`,
		`UPDATE live.offers SET session_id=session_id WHERE id=$1`, `DELETE FROM live.offers WHERE id=$1`} {
		err := h.do(h.token, f.storeA1, func(tx pgx.Tx, _ platform.Scope) error { _, e := tx.Exec(h.ctx, q, first.ID); return e })
		requirePGCode(t, err, "42501", q)
	}

	// Missing live:manage (read-only principal).
	_, readerToken := lcPrincipal(t, f, f.tenantA, []string{f.storeA1}, "store:read", "live:read")
	lcIs(t, lcErr(h.createOffer(readerToken, f.storeA1, t04Key("lc-offer"), s1, claims.OfferInput{Keyword: "R1", SKUID: sku1.ID, MaxQuantityPerClaim: 1})), platform.ErrForbidden, "create without live:manage")
	lcIs(t, lcErr(h.updateOffer(readerToken, f.storeA1, t04Key("lc-offer"), s1, first.ID, claims.OfferUpdate{ExpectedVersion: reactivated.Version, MaxQuantityPerClaim: 1, Active: true})), platform.ErrForbidden, "update without live:manage")
	if _, err := h.getBoard(readerToken, f.storeA1, s1); err != nil {
		t.Fatalf("live:read board: %v", err)
	}

	// Typo recovery (§12): A11->X deactivated, A1->X created, A1 accepted, redeem applies
	// only the active offer's line and reports the inactive one as skipped.
	s3 := h.draft(t, f.storeA1)
	x := lcSKUs(t, f, f.tenantA, f.storeA1, "USD", 1)[0]
	h.open(t, s3, claims.MatchExact)
	typo := h.offer(t, s3, "A11", x, 5)
	onTypo := h.accepted(t, s3, "", "typo-buyer", "A11+2")
	h.setOffer(t, s3, typo, 5, false)
	fixed := h.offer(t, s3, "A1", x, 5)
	onFixed := h.accepted(t, s3, onTypo.BundleID, "", "A1+3")
	if onFixed.OfferID != fixed.ID || onFixed.Quantity != 3 || onFixed.BundleID != onTypo.BundleID {
		t.Fatalf("typo recovery claim %+v", onFixed)
	}
	l := h.link(t, s3, onTypo.BundleID, 0, false)
	c := mustIssue(t, h.service, f.storeA1)
	redeemed, err := h.redeem(c, t04Key("lc-redeem"), l.Token, onFixed.BundleVersion)
	if err != nil || !reflect.DeepEqual(lcItems(redeemed.Applied), map[string]int64{x: 3}) ||
		!reflect.DeepEqual(lcSkipped(redeemed.Skipped), []claims.Skipped{{SKUID: x, Reason: "offer_inactive"}}) || !reflect.DeepEqual(lcItems(redeemed.Cart.Items), map[string]int64{x: 3}) {
		t.Fatalf("typo recovery redeem %+v %v", redeemed, err)
	}
	h.closeWindow(t, s3)

	// 201st offer under concurrency: 196 existing + 5 concurrent creates -> 4 win, 1 409.
	s4 := h.draft(t, f.storeA1)
	many := lcSKUs(t, f, f.tenantA, f.storeA1, "USD", 202)
	mustExec(t, f.owner, `INSERT INTO live.offers(tenant_id,store_id,session_id,keyword,sku_id,max_quantity_per_claim,principal_id)
		SELECT $1,$2,$3,'K'||lpad(i::text,4,'0'),sku,1,$4 FROM unnest($5::uuid[]) WITH ORDINALITY AS u(sku,i)`, f.tenantA, f.storeA1, s4, h.actor, many[:196])
	racers := make([]error, 5)
	start = make(chan struct{})
	for i := range racers {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			_, racers[i] = h.createOffer(h.token, f.storeA1, t04Key("lc-offer-cap"), s4, claims.OfferInput{Keyword: fmt.Sprintf("CAP%d", i), SKUID: many[196+i], MaxQuantityPerClaim: 1})
		}(i)
	}
	close(start)
	wg.Wait()
	if ok, conflicts := lcOutcomes(t, racers); ok != 4 || conflicts != 1 {
		t.Fatalf("offer cap race winners=%d conflicts=%d", ok, conflicts)
	}
	lcIs(t, lcErr(h.createOffer(h.token, f.storeA1, t04Key("lc-offer-cap"), s4, claims.OfferInput{Keyword: "CAPX", SKUID: many[201], MaxQuantityPerClaim: 1})), command.ErrConflict, "201st offer")
	if n := countRows(t, f.owner, `SELECT count(*) FROM live.offers WHERE session_id=$1`, s4); n != 200 {
		t.Fatalf("offers per session=%d, want cap 200", n)
	}

	// Create while another transaction holds the session FOR SHARE (media/Studio readers)
	// succeeds within the inherited lock_timeout: no live.sessions row lock (§4.2).
	s5 := h.draft(t, f.storeA1)
	extra := lcSKUs(t, f, f.tenantA, f.storeA1, "USD", 3)
	share, err := f.owner.Begin(h.ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = share.Exec(h.ctx, `SELECT 1 FROM live.sessions WHERE id=$1 FOR SHARE`, s5); err != nil {
		t.Fatal(err)
	}
	if _, err := h.createOffer(h.token, f.storeA1, t04Key("lc-offer-share"), s5, claims.OfferInput{Keyword: "SH1", SKUID: extra[0], MaxQuantityPerClaim: 1}); err != nil {
		t.Fatalf("create blocked by session FOR SHARE: %v", err)
	}
	_ = share.Rollback(h.ctx)

	// Revoked token after the receipt lock wait -> ErrUnauthorized, nothing written.
	_, waitToken := lcPrincipal(t, f, f.tenantA, []string{f.storeA1}, "store:read", "live:read", "live:manage")
	waitKey := t04Key("lc-offer-wait")
	lock, err := f.owner.Begin(h.ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Rollback(h.ctx)
	if _, err = lock.Exec(h.ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, "command|"+f.tenantA+"|"+f.storeA1+"|live.claim.offer.create|"+waitKey); err != nil {
		t.Fatal(err)
	}
	pid, done := h.bgMerchant(t, waitToken, f.storeA1, func(tx pgx.Tx, s platform.Scope) error {
		_, err := claims.CreateOffer(h.ctx, tx, s, waitToken, waitKey, s5, claims.OfferInput{Keyword: "WT1", SKUID: extra[1], MaxQuantityPerClaim: 1})
		return err
	})
	lpWaitLock(t, f, pid, true)
	mustExec(t, f.owner, `UPDATE identity.sessions SET revoked_at=clock_timestamp() WHERE token_hash=$1`, tokenHash(waitToken))
	_ = lock.Rollback(h.ctx)
	lcIs(t, waitError(t, done), platform.ErrUnauthorized, "revoked during receipt wait")
	if n := countRows(t, f.owner, `SELECT count(*) FROM live.offers WHERE session_id=$1 AND keyword='WT1'`, s5); n != 0 {
		t.Fatal("revoked create wrote an offer")
	}
	// Permission removed while a replay waits -> the replay is refused too (§4.2
	// "again after command.Run returns (including replay)").
	replayPrincipal, replayToken := lcPrincipal(t, f, f.tenantA, []string{f.storeA1}, "store:read", "live:read", "live:manage")
	replayKey := t04Key("lc-offer-replay")
	replayIn := claims.OfferInput{Keyword: "RP1", SKUID: extra[2], MaxQuantityPerClaim: 1}
	if _, err := h.createOffer(replayToken, f.storeA1, replayKey, s5, replayIn); err != nil {
		t.Fatal(err)
	}
	lock2, err := f.owner.Begin(h.ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer lock2.Rollback(h.ctx)
	if _, err = lock2.Exec(h.ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, "command|"+f.tenantA+"|"+f.storeA1+"|live.claim.offer.create|"+replayKey); err != nil {
		t.Fatal(err)
	}
	pid, done = h.bgMerchant(t, replayToken, f.storeA1, func(tx pgx.Tx, s platform.Scope) error {
		_, err := claims.CreateOffer(h.ctx, tx, s, replayToken, replayKey, s5, replayIn)
		return err
	})
	lpWaitLock(t, f, pid, true)
	mustExec(t, f.owner, `DELETE FROM identity.store_grants WHERE principal_id=$1 AND permission='live:manage'`, replayPrincipal)
	_ = lock2.Rollback(h.ctx)
	lcIs(t, waitError(t, done), platform.ErrForbidden, "replay after live:manage removed")

	// Audit failure rolls back offer and receipt (§4.2: command.Run + command.Audit in one tx).
	fn := "lc_audit_fail_" + t04Tag()
	ident := pgx.Identifier{"ops", fn}.Sanitize()
	mustExec(t, f.owner, fmt.Sprintf(`CREATE FUNCTION %s() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'synthetic audit failure' USING ERRCODE='P0001'; END $$`, ident))
	mustExec(t, f.owner, fmt.Sprintf(`CREATE TRIGGER %s BEFORE INSERT ON ops.audit_events FOR EACH ROW WHEN (NEW.action='live.claim.offer.created' AND NEW.principal_id='%s'::uuid) EXECUTE FUNCTION %s()`,
		pgx.Identifier{fn}.Sanitize(), h.actor, ident))
	t.Cleanup(func() {
		mustExec(t, f.owner, fmt.Sprintf(`DROP TRIGGER IF EXISTS %s ON ops.audit_events`, pgx.Identifier{fn}.Sanitize()))
		mustExec(t, f.owner, `DROP FUNCTION IF EXISTS `+ident+`()`)
	})
	auditKey := t04Key("lc-offer-audit")
	auditIn := claims.OfferInput{Keyword: "AU1", SKUID: extra[1], MaxQuantityPerClaim: 1}
	_, err = h.createOffer(h.token, f.storeA1, auditKey, s5, auditIn)
	requirePGCode(t, err, "P0001", "audit failure")
	if countRows(t, f.owner, `SELECT count(*) FROM live.offers WHERE session_id=$1 AND keyword='AU1'`, s5) != 0 ||
		countRows(t, f.owner, `SELECT count(*) FROM ops.command_results WHERE idempotency_key=$1`, auditKey) != 0 {
		t.Fatal("audit failure left an offer or a receipt")
	}
	mustExec(t, f.owner, fmt.Sprintf(`DROP TRIGGER %s ON ops.audit_events`, pgx.Identifier{fn}.Sanitize()))
	if _, err := h.createOffer(h.token, f.storeA1, auditKey, s5, auditIn); err != nil {
		t.Fatalf("same key after audit recovery: %v", err)
	}
	if n := countRows(t, f.owner, `SELECT count(*) FROM ops.audit_events WHERE principal_id=$1 AND action='live.claim.offer.created'`, h.actor); n < 6 {
		t.Fatalf("offer creations must be audited, got %d", n)
	}
}

func lcErr[T any](_ T, err error) error { return err }

// lcOutcomes counts nil and ErrConflict results and fails on any other error.
func lcOutcomes(t *testing.T, errs []error) (ok, conflicts int) {
	t.Helper()
	for _, err := range errs {
		switch {
		case err == nil:
			ok++
		case errors.Is(err, command.ErrConflict):
			conflicts++
		default:
			t.Fatalf("unexpected race error: %v (sqlstate %s)", err, sqlState(err))
		}
	}
	return ok, conflicts
}

// TestLiveClaimsKC05Window is KC05 (§4.2 SetWindow rules, §1 window model, §5.4 fence).
func TestLiveClaimsKC05Window(t *testing.T) {
	h := lcSetup(t)
	f := h.f
	s := h.draft(t, f.storeA1)
	audits := func(action string) int {
		return countRows(t, f.owner, `SELECT count(*) FROM ops.audit_events WHERE principal_id=$1 AND action=$2`, h.actor, "live.claim.window."+action)
	}
	// No row yet: reported CLOSED/EXACT version 0.
	if w := h.board(t, s).Window; w.SessionID != s || w.State != claims.WindowClosed || w.MatchMode != claims.MatchExact || w.Version != 0 || w.Generation != 0 || w.OpenedAt != nil || w.ClosedAt != nil {
		t.Fatalf("absent window %+v", w)
	}
	for name, in := range map[string]claims.WindowInput{
		"state": {State: "PAUSED", MatchMode: claims.MatchExact}, "mode": {State: claims.WindowClosed, MatchMode: "CONTAINS"},
		"version": {ExpectedVersion: -1, State: claims.WindowClosed, MatchMode: claims.MatchExact}, "empty": {},
	} {
		lcIs(t, lcErr(h.setWindow(h.token, f.storeA1, s, in)), command.ErrInvalid, "invalid window "+name)
	}
	lcIs(t, lcErr(h.setWindow(h.token, f.storeA1, randomUUID(), claims.WindowInput{State: claims.WindowClosed, MatchMode: claims.MatchExact})), command.ErrNotFound, "missing session")
	// expected 0 + CLOSED inserts generation 0 at version 1 in the requested mode.
	w := h.mustWindow(t, s, 0, claims.WindowClosed, claims.MatchKeywordQtyOnly)
	if w.State != claims.WindowClosed || w.MatchMode != claims.MatchKeywordQtyOnly || w.Generation != 0 || w.Version != 1 || w.OpenedAt != nil || w.ClosedAt != nil {
		t.Fatalf("inserted CLOSED %+v", w)
	}
	lcIs(t, lcErr(h.setWindow(h.token, f.storeA1, s, claims.WindowInput{ExpectedVersion: 0, State: claims.WindowClosed, MatchMode: claims.MatchExact})), command.ErrConflict, "second insert")
	lcIs(t, lcErr(h.setWindow(h.token, f.storeA1, s, claims.WindowInput{ExpectedVersion: 1, State: claims.WindowClosed, MatchMode: claims.MatchKeywordQtyOnly})), command.ErrConflict, "identical CLOSED->CLOSED")
	w = h.mustWindow(t, s, 1, claims.WindowClosed, claims.MatchExact) // mode_set
	if w.Version != 2 || w.MatchMode != claims.MatchExact || w.Generation != 0 {
		t.Fatalf("mode_set %+v", w)
	}
	lcIs(t, lcErr(h.setWindow(h.token, f.storeA1, s, claims.WindowInput{ExpectedVersion: 2, State: claims.WindowOpen, MatchMode: claims.MatchKeywordQtyOnly})), command.ErrConflict, "open with a different mode")
	w = h.mustWindow(t, s, 2, claims.WindowOpen, claims.MatchExact)
	if w.State != claims.WindowOpen || w.Generation != 1 || w.Version != 3 || w.OpenedAt == nil || w.ClosedAt != nil {
		t.Fatalf("CLOSED->OPEN %+v", w)
	}
	opened := *w.OpenedAt
	lcIs(t, lcErr(h.setWindow(h.token, f.storeA1, s, claims.WindowInput{ExpectedVersion: 3, State: claims.WindowOpen, MatchMode: claims.MatchExact})), command.ErrConflict, "OPEN->OPEN")
	lcIs(t, lcErr(h.setWindow(h.token, f.storeA1, s, claims.WindowInput{ExpectedVersion: 3, State: claims.WindowOpen, MatchMode: claims.MatchKeywordQtyOnly})), command.ErrConflict, "mode change while OPEN")
	lcIs(t, lcErr(h.setWindow(h.token, f.storeA1, s, claims.WindowInput{ExpectedVersion: 3, State: claims.WindowClosed, MatchMode: claims.MatchKeywordQtyOnly})), command.ErrConflict, "close with a different mode")
	lcIs(t, lcErr(h.setWindow(h.token, f.storeA1, s, claims.WindowInput{ExpectedVersion: 2, State: claims.WindowClosed, MatchMode: claims.MatchExact})), command.ErrConflict, "stale version")
	// A second OPEN window in the store is 23505 -> ErrConflict.
	other := h.draft(t, f.storeA1)
	lcIs(t, lcErr(h.setWindow(h.token, f.storeA1, other, claims.WindowInput{State: claims.WindowOpen, MatchMode: claims.MatchExact})), command.ErrConflict, "second OPEN window in store")
	if h.board(t, other).Window.Version != 0 {
		t.Fatal("rejected open left a window row")
	}
	w = h.mustWindow(t, s, 3, claims.WindowClosed, claims.MatchExact)
	if w.State != claims.WindowClosed || w.Generation != 1 || w.OpenedAt == nil || !w.OpenedAt.Equal(opened) || w.ClosedAt == nil || w.ClosedAt.Before(opened) || w.MatchMode != claims.MatchExact {
		t.Fatalf("OPEN->CLOSED %+v", w)
	}
	for want := int64(2); want <= 3; want++ {
		w = h.mustWindow(t, s, w.Version, claims.WindowOpen, claims.MatchExact)
		if w.Generation != want || w.ClosedAt != nil || !w.OpenedAt.After(opened) {
			t.Fatalf("reopen generation %+v want %d", w, want)
		}
		w = h.mustWindow(t, s, w.Version, claims.WindowClosed, claims.MatchExact)
	}
	// One audit row per successful SetWindow (8 here); the contract does not name the
	// action of the initial CLOSED insert, so only opened/closed are counted exactly.
	total := countRows(t, f.owner, `SELECT count(*) FROM ops.audit_events WHERE principal_id=$1 AND action LIKE 'live.claim.window.%'`, h.actor)
	if audits("opened") != 3 || audits("closed") != 3 || audits("mode_set") < 1 || total != 8 {
		t.Fatalf("window audits opened=%d closed=%d mode_set=%d total=%d", audits("opened"), audits("closed"), audits("mode_set"), total)
	}
	// Missing live:manage.
	_, readerToken := lcPrincipal(t, f, f.tenantA, []string{f.storeA1}, "store:read", "live:read")
	lcIs(t, lcErr(h.setWindow(readerToken, f.storeA1, s, claims.WindowInput{ExpectedVersion: w.Version, State: claims.WindowOpen, MatchMode: claims.MatchExact})), platform.ErrForbidden, "window without live:manage")

	// Two sessions opened concurrently in one store -> exactly one 409.
	a, b := h.draft(t, f.storeA1), h.draft(t, f.storeA1)
	results := make([]error, 2)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i, session := range []string{a, b} {
		wg.Add(1)
		go func(i int, session string) {
			defer wg.Done()
			<-start
			_, results[i] = h.setWindow(h.token, f.storeA1, session, claims.WindowInput{State: claims.WindowOpen, MatchMode: claims.MatchExact})
		}(i, session)
	}
	close(start)
	wg.Wait()
	if ok, conflicts := lcOutcomes(t, results); ok != 1 || conflicts != 1 {
		t.Fatalf("concurrent opens winners=%d conflicts=%d", ok, conflicts)
	}
	winner := a
	if results[0] != nil {
		winner = b
	}
	h.closeWindow(t, winner)

	// Close waits for an in-flight ingest holding the window FOR SHARE (observed in
	// pg_stat_activity), then any later ingest sees CLOSED and persists nothing.
	fence := h.draft(t, f.storeA1)
	fw := h.open(t, fence, claims.MatchExact)
	h.offer(t, fence, "A1", h.stock.skus[0].ID, 5)
	inflight := h.holdMerchant(t, h.token, f.storeA1, func(tx pgx.Tx, sc platform.Scope) error {
		r, err := claims.RecordManualClaim(h.ctx, tx, sc, h.labels, h.token, t04Key("lc-inflight"), fence, claims.ManualClaimInput{ActorLabel: "inflight", Text: "A1"})
		if err == nil && r.Outcome != claims.OutcomeAccepted {
			err = fmt.Errorf("in-flight claim not accepted: %+v", r)
		}
		return err
	})
	pid, done := h.bgMerchant(t, h.token, f.storeA1, func(tx pgx.Tx, sc platform.Scope) error {
		_, err := claims.SetWindow(h.ctx, tx, sc, h.token, t04Key("lc-close"), fence, claims.WindowInput{ExpectedVersion: fw.Version, State: claims.WindowClosed, MatchMode: claims.MatchExact})
		return err
	})
	lpWaitLock(t, f, pid, false)
	if err := inflight.finish(t); err != nil {
		t.Fatalf("in-flight ingest: %v", err)
	}
	if err := waitError(t, done); err != nil {
		t.Fatalf("close after in-flight ingest: %v", err)
	}
	events := countRows(t, f.owner, `SELECT count(*) FROM claims.events WHERE session_id=$1`, fence)
	bundlesBefore := countRows(t, f.owner, `SELECT count(*) FROM claims.bundles WHERE session_id=$1`, fence)
	if events != 1 || bundlesBefore != 1 {
		t.Fatalf("in-flight ingest must commit before close: events=%d bundles=%d", events, bundlesBefore)
	}
	late := h.claim(t, fence, claims.ManualClaimInput{ActorLabel: "late", Text: "A1"})
	if late.Outcome != claims.OutcomeRejected || late.Reason != claims.ReasonWindowClosed || late.BundleID != "" || late.OfferID != "" {
		t.Fatalf("ingest after close %+v", late)
	}
	if countRows(t, f.owner, `SELECT count(*) FROM claims.events WHERE session_id=$1`, fence) != events ||
		countRows(t, f.owner, `SELECT count(*) FROM claims.bundles WHERE session_id=$1`, fence) != bundlesBefore {
		t.Fatal("WINDOW_CLOSED persisted a row")
	}
}
