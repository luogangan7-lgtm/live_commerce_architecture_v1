// link.go owns IssueLink: issue, rotate, or release+rotate a bundle's buyer claim link
// (contract §4.2 IssueLink, §6).
//
// Non-goals: no link delivery (the operator copies it; T10c/T07 own messaging), no HTTP
// response (the adapter writes the token only after platform.WithScope commits), no Go
// lock or write on claims.bundles/claims.links: commerce_runtime has no such grant.

package claims

import (
	"context"
	"crypto/sha256"
	"errors"
	"math"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"

	"livecommerce/internal/command"
	"livecommerce/internal/platform"
)

// LinkInput is the M7 body.
type LinkInput struct {
	ExpectedGeneration int64 `json:"expected_generation"` // 0 = no link yet
	ReleaseBinding     bool  `json:"release_binding"`
}

// linkReceipt is the token-free replay record stored by command.Run (§0 R3).
type linkReceipt struct {
	BundleID   string    `json:"bundle_id"`
	Generation int64     `json:"generation"`
	ExpiresAt  time.Time `json:"expires_at"`
	Released   bool      `json:"released"`
}

// IssueLink creates or rotates the bundle's link (live:manage) with receipt
// live.claim.link.issue and audit live.claim.link.issued (+ live.claim.binding.released).
// The 32-byte token is generated inside the receipt closure and exists only in the
// returned IssuedLink of the first execution; the receipt, audit and database see only
// SHA-256 values. A replay returns the same generation/expiry with Token "" and
// Replayed true. Errors: zero rows → ErrNotFound; PT409 (generation moved) →
// ErrConflict; PT401/PT403/PT404 → platform.ErrUnauthorized/ErrForbidden/ErrScopeNotFound.
// Caller (M7) must write the token to the HTTP body only after COMMIT is acknowledged.
func IssueLink(ctx context.Context, tx pgx.Tx, scope platform.Scope, token, key, sessionID, bundleID string, in LinkInput) (IssuedLink, error) {
	if !command.ValidID(sessionID) || !command.ValidID(bundleID) || in.ExpectedGeneration < 0 || in.ExpectedGeneration == math.MaxInt64 {
		return IssuedLink{}, command.ErrInvalid
	}
	if err := authorize(ctx, tx, scope, token, managePermission); err != nil {
		return IssuedLink{}, err
	}
	request := struct {
		PrincipalID        string `json:"principal_id"`
		SessionID          string `json:"session_id"`
		BundleID           string `json:"bundle_id"`
		ExpectedGeneration int64  `json:"expected_generation"`
		ReleaseBinding     bool   `json:"release_binding"`
	}{scope.PrincipalID, sessionID, bundleID, in.ExpectedGeneration, in.ReleaseBinding}
	var receipt linkReceipt
	var fresh LinkToken // set only when this call executed the closure (not a replay)
	err := command.Run(ctx, tx, scope, "live.claim.link.issue", key, request, &receipt, func() error {
		if err := authorize(ctx, tx, scope, token, managePermission); err != nil {
			return err
		}
		candidate := newLinkToken()
		authHash := sha256.Sum256([]byte(token))
		// The definer compares app.authz_revision with its own resolve_access
		// (live.request_media_stop pattern).
		if _, err := tx.Exec(ctx, `SELECT set_config('app.authz_revision',$1,true)`, strconv.FormatInt(scope.Revision, 10)); err != nil {
			return err
		}
		err := tx.QueryRow(ctx, `SELECT generation,expires_at,released
			FROM claims.issue_link($1::bytea,$2::uuid,$3::uuid,$4::uuid,$5::bigint,$6::bytea,$7::boolean)`,
			authHash[:], scope.StoreID, sessionID, bundleID, in.ExpectedGeneration, candidate.hash(), in.ReleaseBinding).
			Scan(&receipt.Generation, &receipt.ExpiresAt, &receipt.Released)
		if errors.Is(err, pgx.ErrNoRows) {
			return command.ErrNotFound
		}
		if err != nil {
			return mapError(err)
		}
		receipt.BundleID = bundleID
		if err := command.Audit(ctx, tx, scope, "live.claim.link.issued"); err != nil {
			return err
		}
		if receipt.Released {
			if err := command.Audit(ctx, tx, scope, "live.claim.binding.released"); err != nil {
				return err
			}
		}
		fresh = candidate
		return nil
	})
	if err != nil {
		return IssuedLink{}, mapError(err)
	}
	if err := authorize(ctx, tx, scope, token, managePermission); err != nil {
		return IssuedLink{}, err
	}
	return IssuedLink{Token: fresh, Generation: receipt.Generation, ExpiresAt: receipt.ExpiresAt,
		Released: receipt.Released, Replayed: fresh == ""}, nil
}
