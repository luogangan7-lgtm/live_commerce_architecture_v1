// Command admin-fixture owns a disposable local fixture for the admin ledger UI: it migrates an
// empty lc_admin_fixture database on 127.0.0.1, seeds one store with nine sample SKUs and a dev
// session, writes a private 0600 env file for the dev Next adapter and serves the Go admin API on
// 127.0.0.1:18081.
//
// It never touches an existing shop or customer database (empty-database, loopback and login
// guards), never runs without COMMERCE_FIXTURE_ALLOWED=1, and never passes a credential through argv
// or env (the owner DSN arrives via a one-use 0600 file). Started by scripts/dev/admin-fixture.sh
// and by tests/foundation TestBrowserAdminLedgerFixtureChain; no deployment runs it.
package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"livecommerce/internal/catalog"
	"livecommerce/internal/httpapi"
	"livecommerce/internal/inventory"
	"livecommerce/internal/platform"
	"livecommerce/migrations"
)

type sample struct {
	code, name             string
	price, stock, reserved int64
}

var samples = []sample{
	{"HA-001-BE", "充电式助听器", 129000, 120, 8}, {"AC-002-BK", "便携收纳盒", 49000, 300, 12},
	{"CL-003-SET", "清洁护理套装", 35000, 200, 5}, {"ET-004-S", "耳塞（S 号）", 12000, 500, 20},
	{"ET-004-M", "耳塞（M 号）", 12000, 450, 18}, {"ET-004-L", "耳塞（L 号）", 12000, 320, 10},
	{"CB-005-TC", "充电线（Type-C）", 15000, 600, 15}, {"AC-006-GY", "收纳袋", 18000, 260, 6}, {"CL-007-BR", "清洁刷", 8000, 380, 12},
}

func uuid() string {
	b := make([]byte, 16)
	rand.Read(b)
	b[6] = b[6]&15 | 64
	b[8] = b[8]&63 | 128
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[:4], b[4:6], b[6:8], b[8:10], b[10:])
}
func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "admin fixture failed (credentials withheld):", err)
		os.Exit(1)
	}
}
func run() error {
	if os.Getenv("COMMERCE_FIXTURE_ALLOWED") != "1" {
		return errors.New("explicit fixture permission required")
	}
	envPath := os.Getenv("FIXTURE_ENV_PATH")
	info, err := os.Lstat(envPath)
	if err != nil || !filepath.IsAbs(envPath) || !info.Mode().IsRegular() || info.Mode().Perm() != 0600 || info.Size() != 0 {
		return errors.New("empty private env file required")
	}
	ownerDSN, err := consumeOwnerFile(os.Getenv("FIXTURE_OWNER_FILE"), envPath)
	if err != nil {
		return err
	}
	config, err := pgxpool.ParseConfig(ownerDSN)
	ownerDSN = ""
	if err != nil || !validFixtureConfig(config) {
		return errors.New("refusing nonfixture database")
	}
	listener, err := net.Listen("tcp", "127.0.0.1:18081")
	if err != nil {
		return errors.New("fixture port unavailable")
	}
	defer listener.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	owner, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		return errors.New("owner pool unavailable")
	}
	defer func() {
		if owner != nil {
			owner.Close()
		}
	}()
	empty, err := emptyFixtureDatabase(ctx, owner)
	if err != nil || !empty {
		return errors.New("fixture database must be empty")
	}
	if err = migrations.Apply(ctx, owner); err != nil {
		return errors.New("fixture migration failed")
	}
	password := uuid()
	token := uuid() + uuid()
	tenant, store, principal := uuid(), uuid(), uuid()
	// Password is generated hex/UUID, not external SQL input. The ordinary role
	// deliberately uses the same restricted runtime role as the application.
	_, err = owner.Exec(ctx, `CREATE ROLE fixture_api LOGIN NOSUPERUSER NOBYPASSRLS NOCREATEDB NOCREATEROLE IN ROLE commerce_runtime PASSWORD '`+password+`'`)
	if err != nil {
		return errors.New("fixture runtime role failed")
	}
	tx, err := owner.Begin(ctx)
	if err != nil {
		return errors.New("seed transaction failed")
	}
	defer tx.Rollback(context.Background())
	statements := []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO control.tenants(id,name) VALUES($1,'isolated-ui-fixture')`, []any{tenant}},
		{`INSERT INTO control.stores(tenant_id,id,name,currency) VALUES($1,$2,'助听器配件示例店','TWD')`, []any{tenant, store}},
		{`INSERT INTO identity.principals(id) VALUES($1)`, []any{principal}},
		{`INSERT INTO identity.memberships(tenant_id,principal_id) VALUES($1,$2)`, []any{tenant, principal}},
		{`INSERT INTO identity.store_grants(tenant_id,store_id,principal_id,permission) SELECT $1,$2,$3,permission FROM unnest(ARRAY['store:read','catalog:read','catalog:write','inventory:read','inventory:write']) AS permission`, []any{tenant, store, principal}},
	}
	for _, s := range statements {
		if _, err = tx.Exec(ctx, s.sql, s.args...); err != nil {
			return errors.New("identity seed failed")
		}
	}
	hash := sha256.Sum256([]byte(token))
	_, err = tx.Exec(ctx, `INSERT INTO identity.sessions(id,token_hash,principal_id,audience,expires_at) VALUES($1,$2,$3,'merchant',$4)`, uuid(), hash[:], principal, time.Now().Add(4*time.Hour))
	if err != nil {
		return errors.New("session seed failed")
	}
	if err = tx.Commit(ctx); err != nil {
		return errors.New("seed commit failed")
	}
	owner.Close()
	owner = nil
	// pgx ConnString retains the original parse string after config field edits;
	// construct an explicit fixture-only DSN so runtime never inherits postgres.
	runtimeDSN := fmt.Sprintf("postgres://fixture_api:%s@127.0.0.1:%d/lc_admin_fixture?sslmode=disable", password, config.ConnConfig.Port)
	config = nil
	pool, err := platform.OpenPool(ctx, runtimeDSN)
	if err != nil {
		return errors.New("runtime pool failed")
	}
	defer pool.Close()
	var warehouse inventory.Warehouse
	err = platform.WithScope(ctx, pool, token, store, "inventory:write", func(tx pgx.Tx, s platform.Scope) error {
		var e error
		warehouse, e = inventory.CreateWarehouse(ctx, tx, s, "fixture-warehouse", "台北示例仓")
		return e
	})
	if err != nil {
		return errors.New("warehouse seed failed")
	}
	for i, item := range samples {
		var sku catalog.SKU
		err = platform.WithScope(ctx, pool, token, store, "catalog:write", func(tx pgx.Tx, s platform.Scope) error {
			product, e := catalog.CreateProduct(ctx, tx, s, fmt.Sprintf("fixture-product-%d", i), catalog.ProductInput{Name: item.name, Description: "本地隔离演示数据，非真实在售产品"})
			if e != nil {
				return e
			}
			sku, e = catalog.CreateSKU(ctx, tx, s, fmt.Sprintf("fixture-sku-%d", i), catalog.SKUInput{ProductID: product.ID, Code: item.code, PriceMinor: item.price})
			return e
		})
		if err != nil {
			return errors.New("catalog seed failed")
		}
		err = platform.WithScope(ctx, pool, token, store, "inventory:write", func(tx pgx.Tx, s platform.Scope) error {
			_, e := inventory.AdjustOnHand(ctx, tx, s, fmt.Sprintf("fixture-stock-%d", i), inventory.Adjustment{WarehouseID: warehouse.ID, SKUID: sku.ID, Delta: item.stock, Reason: "isolated fixture seed"})
			if e != nil {
				return e
			}
			_, e = inventory.Reserve(ctx, tx, s, fmt.Sprintf("fixture-reserve-%d", i), []inventory.Line{{WarehouseID: warehouse.ID, SKUID: sku.ID, Quantity: item.reserved}})
			return e
		})
		if err != nil {
			return errors.New("inventory seed failed")
		}
	}
	env := fmt.Sprintf("COMMERCE_FIXTURE_ENABLED=1\nCOMMERCE_API_ORIGIN=http://127.0.0.1:18081\nCOMMERCE_FIXTURE_STORE_ID=%s\nCOMMERCE_FIXTURE_TOKEN=%s\n", store, token)
	if err = os.WriteFile(envPath, []byte(env), 0600); err != nil {
		return errors.New("fixture environment write failed")
	}
	server := &http.Server{Handler: httpapi.NewHandler(pool), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 10 * time.Second, WriteTimeout: 15 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 16 << 10}
	stopCtx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	errs := make(chan error, 1)
	go func() { errs <- server.Serve(listener) }()
	fmt.Printf("Admin fixture ready: http://127.0.0.1:18081\nPrivate env file: %s\n", envPath)
	select {
	case err = <-errs:
		if !errors.Is(err, http.ErrServerClosed) {
			return errors.New("fixture server stopped")
		}
	case <-stopCtx.Done():
		shutdown, done := context.WithTimeout(context.Background(), 10*time.Second)
		defer done()
		return server.Shutdown(shutdown)
	}
	return nil
}

// This is an owned disposable credential file, not an arbitrary cleanup target.
// Never pass an owner DSN in argv/env: macOS retains exec-time environment.
func consumeOwnerFile(path, envPath string) (string, error) {
	if !filepath.IsAbs(path) || filepath.Base(path) != "owner.dsn" || filepath.Dir(path) != filepath.Dir(envPath) {
		return "", errors.New("private owner file required")
	}
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0600 || info.Size() == 0 || info.Size() > 4096 {
		return "", errors.New("private owner file required")
	}
	value, err := os.ReadFile(path)
	if err != nil {
		return "", errors.New("private owner file unreadable")
	}
	if err = os.Remove(path); err != nil {
		return "", errors.New("private owner file cleanup failed")
	}
	return string(value), nil
}

func validFixtureConfig(config *pgxpool.Config) bool {
	return config != nil && config.ConnConfig.Host == "127.0.0.1" && config.ConnConfig.Database == "lc_admin_fixture" && config.ConnConfig.User == "postgres" && len(config.ConnConfig.Fallbacks) == 0
}

func emptyFixtureDatabase(ctx context.Context, pool *pgxpool.Pool) (bool, error) {
	var empty bool
	err := pool.QueryRow(ctx, `SELECT
 NOT EXISTS (SELECT 1 FROM pg_namespace WHERE nspname NOT LIKE 'pg_%' AND nspname NOT IN ('public','information_schema'))
 AND NOT EXISTS (SELECT 1 FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace WHERE n.nspname='public')
 AND NOT EXISTS (SELECT 1 FROM pg_proc p JOIN pg_namespace n ON n.oid=p.pronamespace WHERE n.nspname='public')
 AND NOT EXISTS (SELECT 1 FROM pg_type t JOIN pg_namespace n ON n.oid=t.typnamespace WHERE n.nspname='public')`).Scan(&empty)
	return empty, err
}
