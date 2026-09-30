// Command migrate owns applying livecommerce/migrations (embedded, checksummed,
// forward-only) once and exits. It is the only production caller of
// migrations.Apply.
//
// Runs as: the one-shot "migrate" job in deploy/compose.yml before any API or
// worker starts. It must never run inside an API or worker process.
// Env: COMMERCE_MIGRATE_DATABASE_URL, the migration-owner DSN. In the
// reference deployment that is the bootstrap superuser over the PostgreSQL
// unix socket. Never pass an API/worker DSN.
// Exit: 0 applied or no-op; 1 failed; 2 invalid config; 75 another migration
// holds advisory lock 718020260920 (retry later, never in a loop).
// Logs: fixed tokens only (migrate_applied, migrate_failed code=<SQLSTATE>,
// migrate_busy, migrate_invalid_config). No DSN and no driver message text,
// because messages may contain row values.
package main

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgconn"  // PgError: report only the SQLSTATE, never message text
	"github.com/jackc/pgx/v5/pgxpool" // owner pool handed to migrations.Apply
	"livecommerce/migrations"         // embedded, checksummed, forward-only SQL
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	code := run(ctx, os.Getenv)
	stop()
	os.Exit(code)
}

func run(ctx context.Context, getenv func(string) string) int {
	dsn := getenv("COMMERCE_MIGRATE_DATABASE_URL")
	if strings.TrimSpace(dsn) == "" || len(dsn) > 8192 {
		slog.Error("migrate_invalid_config")
		return 2
	}
	config, err := pgxpool.ParseConfig(dsn)
	dsn = ""
	if err != nil {
		slog.Error("migrate_invalid_config")
		return 2
	}
	config.MaxConns = 4
	start, cancel := context.WithTimeout(ctx, 15*time.Second)
	pool, err := pgxpool.NewWithConfig(start, config)
	cancel()
	if err != nil {
		slog.Error("migrate_database_unavailable")
		return 1
	}
	defer pool.Close()
	switch err := migrations.Apply(ctx, pool); {
	case err == nil:
		slog.Info("migrate_applied")
		return 0
	case errors.Is(err, migrations.ErrMigrationBusy):
		slog.Error("migrate_busy")
		return 75
	default:
		var pgErr *pgconn.PgError
		code := "none"
		if errors.As(err, &pgErr) {
			code = pgErr.Code
		}
		slog.Error("migrate_failed", "sqlstate", code)
		return 1
	}
}
