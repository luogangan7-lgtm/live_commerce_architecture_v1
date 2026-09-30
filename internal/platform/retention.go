package platform

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Retention logins (contracts/claims-retention-purge-v1.md §4/§5, U08). Two dedicated logins, each with
// exactly one authority: lc_retention_job (member of commerce_retention_job; the claims-worker
// retention pool, EXECUTE run_retention/retention_status only) and lc_retention_operator (member of
// commerce_retention_operator; cmd/retention-admin only, created by the runbook after owner approval, never
// by deploy/postgres/logins.tsv). A login that is also any other authority, owns objects or can SET ROLE
// to one is rejected, and every other pool validator rejects a login that can reach either role or
// their definer owner commerce_retention_writer (validatePoolAuthority: exactly-one rule + owner
// reachability). Parse/connection errors, which can echo a DSN, stay private.

// OpenRetentionJobPool opens the retention-job pool (used by cmd/claims-worker and by
// `retention-admin status` on a deploy host).
func OpenRetentionJobPool(ctx context.Context, dsn string) (*pgxpool.Pool, error) {
	return openRetentionPool(ctx, dsn, "retention_job", "retention job database unavailable")
}

// OpenRetentionOperatorPool opens the operator pool (used only by cmd/retention-admin).
func OpenRetentionOperatorPool(ctx context.Context, dsn string) (*pgxpool.Pool, error) {
	return openRetentionPool(ctx, dsn, "retention_operator", "retention operator database unavailable")
}

func openRetentionPool(ctx context.Context, dsn, authority, failure string) (*pgxpool.Pool, error) {
	if ctx == nil || len(dsn) > 8192 {
		return nil, errors.New(failure)
	}
	pool, err := openPool(ctx, dsn, authority)
	if err != nil {
		return nil, errors.New(failure)
	}
	return pool, nil
}

// ValidateRetentionJobPool borrows, but never closes, the caller's pool and applies the same
// admission as OpenRetentionJobPool.
func ValidateRetentionJobPool(ctx context.Context, pool *pgxpool.Pool) error {
	return validateRetentionPool(ctx, pool, "retention_job", "retention job database unavailable")
}

// ValidateRetentionOperatorPool borrows, but never closes, the caller's pool and applies the same
// admission as OpenRetentionOperatorPool.
func ValidateRetentionOperatorPool(ctx context.Context, pool *pgxpool.Pool) error {
	return validateRetentionPool(ctx, pool, "retention_operator", "retention operator database unavailable")
}

func validateRetentionPool(ctx context.Context, pool *pgxpool.Pool, authority, failure string) error {
	if ctx == nil || pool == nil {
		return errors.New(failure)
	}
	bounded, cancel := context.WithTimeout(ctx, startupTimeout)
	defer cancel()
	if err := validatePoolAuthority(bounded, pool, authority); err != nil {
		return errors.New(failure)
	}
	return nil
}
