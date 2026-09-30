// buyer.go owns the buyer side of a claim link: PreviewLink (read-only) and RedeemLink
// (bind, then apply pending claim lines to the buyer's own cart) (contract §4.4, §6).
//
// Non-goals: no inventory reservation, Quote, checkout or order (inventory is touched only
// by a later BeginCheckout); no session title, label, actor key, platform, owner or
// principal in any buyer projection; no cart write except through storefront.SetCart.
//
// Lock order on redeem (§5.6): buyer-command receipt advisory → claims.bundles →
// claims.lines (inside redeem_link) → cart advisory (LockCartOwner) → nested cart.set
// receipt advisory (derived "clm:" key) → storefront.carts → catalog products → SKUs.

package claims

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"math"
	"sort"
	"time"

	"github.com/jackc/pgx/v5"

	"livecommerce/internal/buyer"
	"livecommerce/internal/command"
	"livecommerce/internal/storefront"
)

// Frozen buyer DTOs (contract §4.4, §7.2 B1–B2): no title, label, actor, platform, owner or principal.
type PreviewLine struct {
	Keyword        string `json:"keyword"`
	SKUID          string `json:"sku_id"`
	SKUCode        string `json:"sku_code"`
	ProductName    string `json:"product_name"`
	Currency       string `json:"currency"`
	UnitPriceMinor int64  `json:"unit_price_minor"` // display only; Quote remains the price authority
	Quantity       int64  `json:"quantity"`
	Pending        bool   `json:"pending"`
	Available      bool   `json:"available"`
}
type Preview struct {
	BundleVersion int64         `json:"bundle_version"`
	Bound         bool          `json:"bound"`
	ExpiresAt     time.Time     `json:"expires_at"`
	Lines         []PreviewLine `json:"lines"` // ORDER BY keyword
}
type RedeemInput struct {
	ExpectedBundleVersion int64 `json:"expected_bundle_version"`
}
type Skipped struct {
	SKUID  string `json:"sku_id"`
	Reason string `json:"reason"` // unavailable | offer_inactive
}
type Redeemed struct {
	BundleVersion int64             `json:"bundle_version"`
	Cart          storefront.Cart   `json:"cart"`
	Applied       []storefront.Item `json:"applied"`
	Skipped       []Skipped         `json:"skipped"`
}

// Skip reasons reported for pending lines that stay pending.
const (
	skipUnavailable   = "unavailable"
	skipOfferInactive = "offer_inactive"
)

// redeemKeyPrefix is the derived nested cart.set key prefix. buyerhttp rejects client
// Idempotency-Keys with this prefix on PUT /v1/buyer/cart, so a direct cart write and a
// redeem can never share a cart.set receipt key under opposite lock orders (§5.6).
const redeemKeyPrefix = "clm:"

// PreviewLink shows what a link would prefill, in a buyer.WithScope transaction. It calls
// claims.preview_link (no lock, no write) and enriches lines from buyer-readable catalog
// columns; Available = offer active ∧ SKU and product active ∧ SKU currency = store
// currency. Unknown, expired, rotated, other-owner, other-store and other-tenant links are
// all ErrNotFound. No receipt, event, lock or write. Called by B1.
func PreviewLink(ctx context.Context, tx pgx.Tx, s buyer.Scope, token LinkToken) (Preview, error) {
	if ctx == nil || tx == nil {
		return Preview{}, command.ErrInvalid
	}
	if _, err := ParseLinkToken(string(token)); err != nil {
		return Preview{}, err
	}
	if err := buyer.CheckScope(ctx, tx, s); err != nil {
		return Preview{}, err
	}
	// catalog.skus/products, control.stores: the buyer's own read grants (0007), no lock.
	rows, err := tx.Query(ctx, `SELECT p.bundle_version,p.bound,p.expires_at,p.keyword,p.sku_id::text,s.code,pr.name,
		s.currency,s.price_minor,p.quantity,p.pending,
		p.offer_active AND s.status='active' AND pr.status='active' AND s.currency=st.currency
		FROM claims.preview_link($1::bytea) p
		JOIN catalog.skus s ON s.tenant_id=$2 AND s.store_id=$3 AND s.id=p.sku_id
		JOIN catalog.products pr ON pr.tenant_id=s.tenant_id AND pr.store_id=s.store_id AND pr.id=s.product_id
		JOIN control.stores st ON st.tenant_id=s.tenant_id AND st.id=s.store_id
		ORDER BY p.keyword`, token.hash(), s.TenantID, s.StoreID)
	if err != nil {
		return Preview{}, mapError(err)
	}
	defer rows.Close()
	out := Preview{Lines: []PreviewLine{}}
	for rows.Next() {
		var line PreviewLine
		if err := rows.Scan(&out.BundleVersion, &out.Bound, &out.ExpiresAt, &line.Keyword, &line.SKUID, &line.SKUCode,
			&line.ProductName, &line.Currency, &line.UnitPriceMinor, &line.Quantity, &line.Pending, &line.Available); err != nil {
			return Preview{}, mapError(err)
		}
		out.Lines = append(out.Lines, line)
	}
	if err := rows.Err(); err != nil {
		return Preview{}, mapError(err)
	}
	if len(out.Lines) == 0 {
		return Preview{}, command.ErrNotFound
	}
	return out, nil
}

// claimLine is one row of claims.redeem_link.
type claimLine struct {
	bundleID, offerID, skuID string
	quantity, version        int64
	pending, offerActive     bool
}

// RedeemLink binds the link's bundle to the caller (first owner wins) and applies its
// pending lines as absolute cart quantities, in a buyer.WithScope transaction under one
// buyer receipt ("claims.redeem", request {link_sha256, expected_bundle_version}). Pending
// lines whose offer is inactive or whose SKU/product is unavailable are skipped and stay
// pending; non-claim and already-applied lines are untouched. Nothing to apply → the
// current cart, no cart write. Errors: unknown/expired/rotated/other-owner link →
// ErrNotFound; bundle version changed, a pre-existing unavailable cart item, or a cart
// post-condition failure → ErrConflict; merged cart above SetCart's 50-SKU bound →
// ErrInvalid. Every error rolls back the whole command, including the binding. Same key +
// token + body replays the stored result. Called by B2.
func RedeemLink(ctx context.Context, tx pgx.Tx, s buyer.Scope, key string, token LinkToken, in RedeemInput) (Redeemed, error) {
	if ctx == nil || tx == nil || in.ExpectedBundleVersion < 1 || in.ExpectedBundleVersion == math.MaxInt64 {
		return Redeemed{}, command.ErrInvalid
	}
	if _, err := ParseLinkToken(string(token)); err != nil {
		return Redeemed{}, err
	}
	hash := token.hash()
	request := struct {
		LinkSHA256            string `json:"link_sha256"`
		ExpectedBundleVersion int64  `json:"expected_bundle_version"`
	}{hex.EncodeToString(hash), in.ExpectedBundleVersion}
	var out Redeemed
	// buyer.RunCommand: owner+session-scoped receipt in buyer.command_results (replay/409).
	err := buyer.RunCommand(ctx, tx, s, "claims.redeem", key, request, &out, func() error {
		bundleVersion, lines, err := redeemLines(ctx, tx, hash, in.ExpectedBundleVersion)
		if err != nil {
			return err
		}
		available, err := availableSKUs(ctx, tx, s, lines)
		if err != nil {
			return err
		}
		apply, skipped, err := splitPending(lines, available)
		if err != nil {
			return err
		}
		out = Redeemed{BundleVersion: bundleVersion, Applied: []storefront.Item{}, Skipped: skipped}
		if len(apply) == 0 {
			// storefront.GetCart: read-only projection of the caller's cart (no cart write).
			out.Cart, err = storefront.GetCart(ctx, tx, s)
			return err
		}
		// storefront (the cart owner package): serialize same-owner cart writers before the
		// read-modify-write; SetCart then revalidates and locks the whole catalog selection,
		// so a pre-existing unavailable item fails the redeem with ErrConflict.
		if err := storefront.LockCartOwner(ctx, tx, s); err != nil {
			return err
		}
		cart, err := storefront.GetCart(ctx, tx, s)
		if err != nil {
			return err
		}
		// storefront.SetCart is the only cart writer; claims never writes storefront tables.
		// Its nested cart.set receipt key is derived from the redeem key (never client input).
		derived := sha256.Sum256([]byte("claims.redeem|" + key))
		out.Cart, err = storefront.SetCart(ctx, tx, s, redeemKeyPrefix+hex.EncodeToString(derived[:])[:48],
			storefront.CartInput{ExpectedVersion: cart.Version, Items: mergeCart(cart.Items, apply)})
		if err != nil {
			return err
		}
		for _, line := range apply {
			if !cartHas(out.Cart.Items, line.skuID, line.quantity) {
				return command.ErrConflict
			}
			out.Applied = append(out.Applied, storefront.Item{SKUID: line.skuID, Quantity: line.quantity})
		}
		return markApplied(ctx, tx, apply)
	})
	if err != nil {
		return Redeemed{}, mapError(err)
	}
	return out, nil
}

// redeemLines calls claims.redeem_link (bind + CAS + line lock) and returns the bundle
// version with its lines ordered by offer. Zero rows is the uniform not-found; PT409
// (version changed) maps to ErrConflict through mapError.
func redeemLines(ctx context.Context, tx pgx.Tx, hash []byte, expected int64) (int64, []claimLine, error) {
	rows, err := tx.Query(ctx, `SELECT bundle_id::text,bundle_version,offer_id::text,sku_id::text,quantity,line_version,pending,offer_active
		FROM claims.redeem_link($1::bytea,$2::bigint)`, hash, expected)
	if err != nil {
		return 0, nil, mapError(err)
	}
	defer rows.Close()
	var bundleVersion int64
	var lines []claimLine
	for rows.Next() {
		var l claimLine
		if err := rows.Scan(&l.bundleID, &bundleVersion, &l.offerID, &l.skuID, &l.quantity, &l.version, &l.pending, &l.offerActive); err != nil {
			return 0, nil, mapError(err)
		}
		lines = append(lines, l)
	}
	if err := rows.Err(); err != nil {
		return 0, nil, mapError(err)
	}
	if len(lines) == 0 {
		return 0, nil, command.ErrNotFound
	}
	return bundleVersion, lines, nil
}

// availableSKUs reports, for the SKUs of pending lines, whether SKU and product are active
// and priced in the store currency. It reads catalog.skus/products and control.stores
// with the buyer's own grants and takes no lock (SetCart locks what it writes).
func availableSKUs(ctx context.Context, tx pgx.Tx, s buyer.Scope, lines []claimLine) (map[string]bool, error) {
	var skus []string
	for _, line := range lines {
		if line.pending {
			skus = append(skus, line.skuID)
		}
	}
	available := map[string]bool{}
	if len(skus) == 0 {
		return available, nil
	}
	rows, err := tx.Query(ctx, `SELECT s.id::text,s.status='active' AND p.status='active' AND s.currency=st.currency
		FROM catalog.skus s
		JOIN catalog.products p ON p.tenant_id=s.tenant_id AND p.store_id=s.store_id AND p.id=s.product_id
		JOIN control.stores st ON st.tenant_id=s.tenant_id AND st.id=s.store_id
		WHERE s.tenant_id=$1 AND s.store_id=$2 AND s.id=ANY($3::uuid[])`, s.TenantID, s.StoreID, skus)
	if err != nil {
		return nil, mapError(err)
	}
	defer rows.Close()
	for rows.Next() {
		var sku string
		var ok bool
		if err := rows.Scan(&sku, &ok); err != nil {
			return nil, mapError(err)
		}
		available[sku] = ok
	}
	if err := rows.Err(); err != nil {
		return nil, mapError(err)
	}
	return available, nil
}

// splitPending decides, per pending line, apply or skip (offer_inactive before
// unavailable); skipped lines stay pending. live_offer_active_sku leaves at most one
// applicable line per SKU, which is asserted here (a second one is ErrConflict). Output
// is ordered by SKU. Pure.
func splitPending(lines []claimLine, available map[string]bool) ([]claimLine, []Skipped, error) {
	apply, skipped := []claimLine{}, []Skipped{}
	seen := map[string]bool{}
	for _, line := range lines {
		switch {
		case !line.pending:
		case !line.offerActive:
			skipped = append(skipped, Skipped{SKUID: line.skuID, Reason: skipOfferInactive})
		case !available[line.skuID]:
			skipped = append(skipped, Skipped{SKUID: line.skuID, Reason: skipUnavailable})
		case seen[line.skuID]:
			return nil, nil, command.ErrConflict
		default:
			seen[line.skuID] = true
			apply = append(apply, line)
		}
	}
	sort.Slice(apply, func(i, j int) bool { return apply[i].skuID < apply[j].skuID })
	sort.SliceStable(skipped, func(i, j int) bool { return skipped[i].SKUID < skipped[j].SKUID })
	return apply, skipped, nil
}

// mergeCart overrides the cart quantity of each applied SKU with its absolute claim target
// and keeps every other cart line unchanged. SetCart canonicalizes order and bounds. Pure.
func mergeCart(items []storefront.Item, apply []claimLine) []storefront.Item {
	merged := append([]storefront.Item{}, items...)
	for _, line := range apply {
		found := false
		for i := range merged {
			if merged[i].SKUID == line.skuID {
				merged[i].Quantity, found = line.quantity, true
			}
		}
		if !found {
			merged = append(merged, storefront.Item{SKUID: line.skuID, Quantity: line.quantity})
		}
	}
	return merged
}

func cartHas(items []storefront.Item, skuID string, quantity int64) bool {
	for _, item := range items {
		if item.SKUID == skuID {
			return item.Quantity == quantity
		}
	}
	return false
}

// markApplied records, via claims.mark_applied, exactly the line versions just written to
// the cart; a count mismatch raises PT409 (→ ErrConflict) and rolls the redeem back.
func markApplied(ctx context.Context, tx pgx.Tx, apply []claimLine) error {
	offers, versions := make([]string, len(apply)), make([]int64, len(apply))
	for i, line := range apply {
		offers[i], versions[i] = line.offerID, line.version
	}
	var count int
	if err := tx.QueryRow(ctx, `SELECT claims.mark_applied($1::uuid,$2::uuid[],$3::bigint[])`,
		apply[0].bundleID, offers, versions).Scan(&count); err != nil {
		return mapError(err)
	}
	if count != len(apply) {
		return command.ErrConflict
	}
	return nil
}
