package platform

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5/pgxpool"
)

// OpenClaimsIntakePool opens the dedicated commerce_claims_intake login (meta-claims-intake-v1
// §4.1): the claims intake worker's only SQL authority for applying claims.meta_intake rows,
// planning the first private reply and inserting external_operation_v1 River jobs. Exactly one
// authority: a login that is also a merchant, worker, Meta or payment role, that owns objects or
// that can SET ROLE to one is rejected, and every other pool validator rejects a login that can
// reach commerce_claims_intake. Parse/connection errors (which may include a DSN) stay private.
func OpenClaimsIntakePool(ctx context.Context, dsn string) (*pgxpool.Pool, error) {
	if ctx == nil || len(dsn) > 8192 {
		return nil, errors.New("claims intake database unavailable")
	}
	pool, err := openPool(ctx, dsn, "claims_intake")
	if err != nil {
		return nil, errors.New("claims intake database unavailable")
	}
	return pool, nil
}

// ValidateClaimsIntakePool borrows, but never closes, the caller's pool and applies the same
// admission as OpenClaimsIntakePool (as ValidateMetaConsumerPool does for the Meta consumer).
func ValidateClaimsIntakePool(ctx context.Context, pool *pgxpool.Pool) error {
	if ctx == nil || pool == nil {
		return errors.New("claims intake database unavailable")
	}
	bounded, cancel := context.WithTimeout(ctx, startupTimeout)
	defer cancel()
	if err := validatePoolAuthority(bounded, pool, "claims_intake"); err != nil {
		return errors.New("claims intake database unavailable")
	}
	return nil
}
