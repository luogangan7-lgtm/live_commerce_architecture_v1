package ecpayroute

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"livecommerce/migrations"
)

// TestRouteSQLMatchesMigratedSchemaRealPG prepares the route's two SQL statements against the migrated schema
// (0073 load_cvs_create / finish_cvs_create). The unit tests use a fake queryRower, so only this catches a
// column or signature drift between cvs-core's SQL and this package (integrator R2 merge: merchant_trade_date
// vs trade_created_at). Run: bash scripts/dev/test-focused.sh '^TestRouteSQLMatches' ./internal/integrations/shipping/ecpay/ecpayroute
func TestRouteSQLMatchesMigratedSchemaRealPG(t *testing.T) {
	if os.Getenv("LC_TEST_DATABASE_ALLOWED") != "1" || os.Getenv("LC_TEST_DATABASE_URL") == "" {
		t.Skip("LC_TEST_DATABASE_ALLOWED=1 and LC_TEST_DATABASE_URL required; real-PG test NOT_RUN")
	}
	cfg, err := pgxpool.ParseConfig(os.Getenv("LC_TEST_DATABASE_URL"))
	if err != nil || cfg.ConnConfig.Host != "127.0.0.1" || cfg.ConnConfig.Database != "lc_foundation_test" {
		t.Fatal("refusing database outside 127.0.0.1/lc_foundation_test")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("open pool: %v", err)
	}
	defer pool.Close()
	if err := migrations.Apply(ctx, pool); err != nil {
		t.Fatalf("apply migrations: %v", err)
	}
	conn, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	defer conn.Release()
	for name, sql := range map[string]string{"loadSQL": loadSQL, "finishSQL": finishSQL} {
		desc, err := conn.Conn().Prepare(ctx, "", sql)
		if err != nil {
			t.Errorf("%s does not match the migrated schema: %v", name, err)
			continue
		}
		if name == "loadSQL" && len(desc.Fields) != 18 {
			t.Errorf("loadSQL returns %d columns, the Scan reads 18", len(desc.Fields))
		}
	}
}
