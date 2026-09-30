// claims.go owns the buyer claim-link transport B1–B2 (contract
// contracts/live-keyword-claims-v1.md §7.2): GET /v1/buyer/claim-link previews a bundle
// and POST /v1/buyer/claim-link/redeem binds it and prefills the caller's own cart.
//
// Non-goals: no claims rule (internal/claims decides binding, CAS, availability and the
// cart merge), no merchant route (internal/httpapi M1–M7), no Quote, checkout, inventory
// or message effect, and no logging or echo of the link token.
//
// Each request runs in one commerce_buyer_runtime READ COMMITTED transaction (buyer.WithScope through
// scoped()) with the store already resolved from the published origin, never from input. The link token
// arrives only in the X-Commerce-Claim-Token header, exactly once; forbiddenInput (handler.go) rejects
// that header on every other route, and query strings are already rejected for both routes.

package buyerhttp

import (
	"context"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5"

	"livecommerce/internal/buyer"
	"livecommerce/internal/claims"
)

const (
	claimLinkPath    = "/v1/buyer/claim-link"
	claimRedeemPath  = "/v1/buyer/claim-link/redeem"
	claimTokenHeader = "X-Commerce-Claim-Token"
	// claimDerivedKeyPrefix must equal the nested cart.set key prefix that
	// claims.RedeemLink derives (internal/claims/buyer.go redeemKeyPrefix). PUT
	// /v1/buyer/cart rejects client keys with it (contract §5.6).
	claimDerivedKeyPrefix = "clm:"
)

// Frozen B1 projection (§7.2): no title, label, actor, platform, owner or principal.
type claimPreviewLineResponse struct {
	Keyword        string `json:"keyword"`
	SKUID          string `json:"sku_id"`
	SKUCode        string `json:"sku_code"`
	ProductName    string `json:"product_name"`
	Currency       string `json:"currency"`
	UnitPriceMinor int64  `json:"unit_price_minor"`
	Quantity       int64  `json:"quantity"`
	Pending        bool   `json:"pending"`
	Available      bool   `json:"available"`
}

type claimPreviewResponse struct {
	BundleVersion int64                      `json:"bundle_version"`
	Bound         bool                       `json:"bound"`
	ExpiresAt     time.Time                  `json:"expires_at"`
	Lines         []claimPreviewLineResponse `json:"lines"`
}

// Frozen B2 projection (§7.2): the existing cart projection plus applied/skipped lines.
type claimSkippedResponse struct {
	SKUID  string `json:"sku_id"`
	Reason string `json:"reason"`
}

type claimRedeemResponse struct {
	BundleVersion int64                  `json:"bundle_version"`
	Cart          cartResponse           `json:"cart"`
	Applied       []cartItemResponse     `json:"applied"`
	Skipped       []claimSkippedResponse `json:"skipped"`
}

// claimRequest serves B1 (GET preview, read-only) and B2 (POST redeem, keyed by the
// caller's Idempotency-Key through buyer.RunCommand). ServeHTTP has already checked the
// BFF key, bearer, origin, method and key presence; this validates the claim token header
// and body before opening the buyer transaction. Unknown, expired, rotated, other-owner
// and other-store links all surface as the same claims ErrNotFound (404).
func (h *handler) claimRequest(ctx context.Context, r *http.Request, kind routeKind, storeID, bearerToken, key string) (any, error) {
	link, err := claimToken(r)
	if err != nil {
		return nil, err
	}
	if kind == claimLinkRoute {
		preview, err := scoped(ctx, h.pool, bearerToken, storeID, func(c context.Context, tx pgx.Tx, s buyer.Scope) (claims.Preview, error) {
			return claims.PreviewLink(c, tx, s, link)
		})
		if err != nil {
			return nil, err
		}
		return projectClaimPreview(preview), nil
	}
	var in claims.RedeemInput
	if err := decodeJSON(r, &in); err != nil {
		return nil, err
	}
	redeemed, err := scoped(ctx, h.pool, bearerToken, storeID, func(c context.Context, tx pgx.Tx, s buyer.Scope) (claims.Redeemed, error) {
		return claims.RedeemLink(c, tx, s, key, link, in)
	})
	if err != nil {
		return nil, err
	}
	return projectClaimRedeem(redeemed), nil
}

// claimToken reads the one X-Commerce-Claim-Token header in its canonical 43-character
// form; missing, repeated or malformed is 422. The value is never logged or echoed.
func claimToken(r *http.Request) (claims.LinkToken, error) {
	value, one := oneHeader(r, claimTokenHeader)
	if !one {
		return "", responseError{http.StatusUnprocessableEntity, "invalid_request"}
	}
	token, err := claims.ParseLinkToken(value)
	if err != nil {
		return "", responseError{http.StatusUnprocessableEntity, "invalid_request"}
	}
	return token, nil
}

func projectClaimPreview(preview claims.Preview) claimPreviewResponse {
	out := claimPreviewResponse{BundleVersion: preview.BundleVersion, Bound: preview.Bound,
		ExpiresAt: preview.ExpiresAt.UTC(), Lines: make([]claimPreviewLineResponse, 0, len(preview.Lines))}
	for _, line := range preview.Lines {
		out.Lines = append(out.Lines, claimPreviewLineResponse{Keyword: line.Keyword, SKUID: line.SKUID,
			SKUCode: line.SKUCode, ProductName: line.ProductName, Currency: line.Currency,
			UnitPriceMinor: line.UnitPriceMinor, Quantity: line.Quantity, Pending: line.Pending, Available: line.Available})
	}
	return out
}

func projectClaimRedeem(redeemed claims.Redeemed) claimRedeemResponse {
	out := claimRedeemResponse{BundleVersion: redeemed.BundleVersion, Cart: projectCart(redeemed.Cart),
		Applied: make([]cartItemResponse, 0, len(redeemed.Applied)), Skipped: make([]claimSkippedResponse, 0, len(redeemed.Skipped))}
	for _, item := range redeemed.Applied {
		out.Applied = append(out.Applied, cartItemResponse{SKUID: item.SKUID, Quantity: item.Quantity})
	}
	for _, item := range redeemed.Skipped {
		out.Skipped = append(out.Skipped, claimSkippedResponse{SKUID: item.SKUID, Reason: item.Reason})
	}
	return out
}
