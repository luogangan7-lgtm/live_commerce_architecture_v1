package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"io"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"livecommerce/internal/command"
)

// ads.go is the `ads-settings` subcommand (meta-ads-v1 4.1, unit ads-core D7 / ruling B12): the operator's only way to change a
// store's ads environment, sandbox ad account, active-budget allowance and its currency. It calls the SQL definer
// ads.operator_set_settings as the commerce_meta_registrar login (EXECUTE granted there only) and prints the resulting updated_at.
// It never prints a DSN, driver message or flag value, never calls Meta, and never runs in the API or a worker. The authority is
// the SQL EXECUTE grant of the login named by the DSN, not this command; LIVE is set only after the owner approves in chat
// (AGENTS.md), which this command cannot verify.

// adsSettings is the parsed subcommand. A nil pointer means "keep the current value"; SandboxAdAccount "" clears it.
type adsSettings struct {
	TenantID, StoreID    string
	Environment          *string
	SandboxAdAccount     *string
	MaxActiveBudgetMinor *int64
	AllowanceCurrency    *string
}

// setAdsSettings is the database step of `ads-settings`, replaceable by tests.
var setAdsSettings = func(ctx context.Context, dsn string, s adsSettings) (time.Time, error) {
	var at time.Time
	err := withPool(ctx, dsn, func(pool *pgxpool.Pool) error {
		// ads.operator_set_settings: owner commerce_ads_writer, EXECUTE commerce_meta_registrar; audited in ads.operator_events.
		err := pool.QueryRow(ctx, `SELECT ads.operator_set_settings($1::uuid,$2::uuid,$3,$4,$5,$6)`, s.TenantID, s.StoreID,
			s.Environment, s.SandboxAdAccount, s.MaxActiveBudgetMinor, s.AllowanceCurrency).Scan(&at)
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) {
			switch pgErr.Code {
			case "22023", "P0002": // invalid value or unknown store: a usage error, not a database fault
				return command.ErrInvalid
			case "23514": // allowance currency in use / CHECK: a conflict with existing state
				return command.ErrConflict
			}
		}
		return err
	})
	return at, err
}

// runAdsSettings parses and validates every flag before any connection opens.
func runAdsSettings(ctx context.Context, args []string, getenv func(string) string, stdout io.Writer) error {
	fs := flag.NewFlagSet("ads-settings", flag.ContinueOnError)
	fs.SetOutput(io.Discard) // flag errors echo the offending value; usage errors are fixed instead
	var s adsSettings
	var environment, sandbox, currency string
	var budget int64
	fs.StringVar(&s.TenantID, "tenant", "", "")
	fs.StringVar(&s.StoreID, "store", "", "")
	fs.StringVar(&environment, "environment", "", "")
	fs.StringVar(&sandbox, "sandbox-ad-account", "", "")
	fs.Int64Var(&budget, "max-active-budget-minor", -1, "")
	fs.StringVar(&currency, "allowance-currency", "", "")
	if fs.Parse(args[1:]) != nil || fs.NArg() != 0 || !command.ValidID(s.TenantID) || !command.ValidID(s.StoreID) {
		return errUsage
	}
	changes := 0
	fs.Visit(func(f *flag.Flag) {
		switch f.Name {
		case "environment":
			s.Environment = &environment
		case "sandbox-ad-account":
			s.SandboxAdAccount = &sandbox
		case "max-active-budget-minor":
			s.MaxActiveBudgetMinor = &budget
		case "allowance-currency":
			s.AllowanceCurrency = &currency
		default:
			return
		}
		changes++
	})
	// I05: the allowance is minor units of one currency; the SQL CHECK repeats these bounds.
	if changes == 0 ||
		(s.Environment != nil && environment != "SANDBOX" && environment != "LIVE") ||
		(s.SandboxAdAccount != nil && sandbox != "" && !assetPattern.MatchString(sandbox)) ||
		(s.MaxActiveBudgetMinor != nil && (budget < 0 || budget > 100_000_000_000)) ||
		(s.AllowanceCurrency != nil && currency != "TWD" && currency != "USD" && currency != "HKD") {
		return errUsage
	}
	dsn := getenv("COMMERCE_META_REGISTRAR_DATABASE_URL")
	if strings.TrimSpace(dsn) == "" || len(dsn) > 8192 {
		return errConfig
	}
	at, err := setAdsSettings(ctx, dsn, s)
	if err != nil {
		return err
	}
	line, err := json.Marshal(map[string]string{"updated_at": at.UTC().Format(time.RFC3339Nano)})
	if err != nil {
		return errConfig
	}
	_, err = stdout.Write(append(line, '\n'))
	return err
}
