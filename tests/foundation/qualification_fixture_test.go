package foundation_test

// Fixture-only helpers for UPDATEs of payments.account_qualifications.
//
// Migration 0077 (contracts/stripe-live-enable-v1.md §3.1, payments.account_qualification_revoke_only) makes the table
// revoke-only for EVERY role, including the fixture owner: production has no path that expires, rewinds or re-versions a
// qualification. Tests that need a rewound or expired qualification run their UPDATE under
// session_replication_role=replica, which skips the (origin-mode) trigger. Only a superuser can set it, so no
// application role gains a bypass, and SL02 still proves the trigger by updating WITHOUT these helpers.

import (
	"context"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// qualExec is mustExec that lets a statement touching payments.account_qualifications skip its revoke-only trigger.
// Every other statement runs exactly as mustExec does.
func qualExec(t *testing.T, pool *pgxpool.Pool, query string, args ...any) {
	t.Helper()
	if !strings.Contains(query, "payments.account_qualifications") {
		mustExec(t, pool, query, args...)
		return
	}
	ctx := context.Background()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin fixture qualification update: %v", err)
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `SET LOCAL session_replication_role=replica`); err != nil {
		t.Fatalf("fixture qualification update needs a superuser owner: %v", err)
	}
	if _, err := tx.Exec(ctx, query, args...); err != nil {
		t.Fatalf("exec test setup: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit fixture qualification update: %v", err)
	}
}

// qualUpdateScan runs an UPDATE ... RETURNING on payments.account_qualifications under the same bypass and scans
// its single row into dest.
func qualUpdateScan(ctx context.Context, pool *pgxpool.Pool, query string, args []any, dest ...any) error {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `SET LOCAL session_replication_role=replica`); err != nil {
		return err
	}
	if err := tx.QueryRow(ctx, query, args...).Scan(dest...); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
