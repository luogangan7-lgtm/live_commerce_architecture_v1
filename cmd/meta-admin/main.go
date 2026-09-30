package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"io"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"livecommerce/internal/command"
	"livecommerce/internal/integrations/metareply"
)

var (
	errUsage    = errors.New("meta_admin_usage")
	errConfig   = errors.New("meta_admin_config")
	errDatabase = errors.New("meta_admin_database")
	errRegister = errors.New("meta_admin_register_failed")
	errConflict = errors.New("meta_admin_version_conflict")
)

var (
	assetPattern = regexp.MustCompile(`^[0-9]{1,40}$`)
	scopePattern = regexp.MustCompile(`^[a-z_]{1,64}$`)
	proofPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)
)

// register is the database step, replaceable by tests.
var register = func(ctx context.Context, dsn string, keys *metareply.PageTokenKeyring, r metareply.Registration, token string) (int64, error) {
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return 0, errDatabase // parse errors can echo the DSN
	}
	cfg.MaxConns = 1
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return 0, errDatabase
	}
	defer pool.Close()
	// RegisterPageToken: seals the token, then integration.register_meta_page_token as the registrar login.
	v, err := metareply.RegisterPageToken(ctx, pool, keys, r, token)
	switch {
	case err == nil:
		return v, nil
	case errors.Is(err, command.ErrConflict):
		return 0, errConflict
	case errors.Is(err, command.ErrInvalid):
		return 0, errUsage
	default:
		return 0, errRegister
	}
}

func main() {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := run(ctx, os.Args[1:], os.Getenv, os.Stdout); err != nil {
		// Only fixed sentinel text can reach here: no driver, Graph or flag-value message.
		_, _ = io.WriteString(os.Stderr, err.Error()+"\n")
		os.Exit(1)
	}
}

// route and disable are the database steps of `route` / `route-disable`, replaceable by tests.
var route = func(ctx context.Context, dsn string, r metareply.RouteRegistration) (any, error) {
	var out metareply.RouteResult
	err := withPool(ctx, dsn, func(pool *pgxpool.Pool) (err error) {
		out, err = metareply.RegisterRoute(ctx, pool, r)
		return err
	})
	return out, err
}

var disable = func(ctx context.Context, dsn, routeID string, epoch int64) (any, error) {
	var next int64
	err := withPool(ctx, dsn, func(pool *pgxpool.Pool) (err error) {
		next, err = metareply.DisableRoute(ctx, pool, routeID, epoch)
		return err
	})
	return map[string]int64{"route_epoch": next}, err
}

// withPool opens a one-connection registrar pool and maps errors to the fixed sentinels.
func withPool(ctx context.Context, dsn string, fn func(*pgxpool.Pool) error) error {
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return errDatabase // parse errors can echo the DSN
	}
	cfg.MaxConns = 1
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return errDatabase
	}
	defer pool.Close()
	switch err := fn(pool); {
	case err == nil:
		return nil
	case errors.Is(err, command.ErrConflict):
		return errConflict
	case errors.Is(err, command.ErrInvalid):
		return errUsage
	default:
		return errRegister
	}
}

func run(ctx context.Context, args []string, getenv func(string) string, stdout io.Writer) error {
	if ctx == nil || getenv == nil || stdout == nil || len(args) < 1 {
		return errUsage
	}
	switch args[0] {
	case "page-token":
	case "route", "route-disable":
		return runRoute(ctx, args, getenv, stdout)
	case "ads-settings":
		return runAdsSettings(ctx, args, getenv, stdout)
	default:
		return errUsage
	}
	fs := flag.NewFlagSet("page-token", flag.ContinueOnError)
	fs.SetOutput(io.Discard) // flag errors echo the offending value; usage errors are fixed instead
	var r metareply.Registration
	var scopes string
	fs.StringVar(&r.TenantID, "tenant", "", "")
	fs.StringVar(&r.StoreID, "store", "", "")
	fs.StringVar(&r.PrincipalID, "principal", "", "")
	fs.StringVar(&r.BindingID, "binding", "", "")
	fs.StringVar(&r.Provider, "provider", "", "")
	fs.StringVar(&r.AssetID, "asset", "", "")
	fs.Int64Var(&r.ExpectedVersion, "expected-version", 0, "")
	fs.StringVar(&scopes, "scopes", "", "")
	if fs.Parse(args[1:]) != nil || fs.NArg() != 0 {
		return errUsage
	}
	r.Scopes = strings.Split(scopes, ",")
	valid := command.ValidID(r.TenantID) && command.ValidID(r.StoreID) && command.ValidID(r.PrincipalID) &&
		command.ValidID(r.BindingID) && (r.Provider == "facebook" || r.Provider == "instagram") &&
		assetPattern.MatchString(r.AssetID) && r.ExpectedVersion >= 0 && r.ExpectedVersion < 1<<62 &&
		len(r.Scopes) >= 1 && len(r.Scopes) <= 16
	for _, s := range r.Scopes {
		valid = valid && scopePattern.MatchString(s)
	}
	if !valid {
		return errUsage
	}
	// Everything the operation needs from the environment is validated before any connection opens.
	dsn := getenv("COMMERCE_META_REGISTRAR_DATABASE_URL")
	token := getenv("META_PAGE_ACCESS_TOKEN")
	if strings.TrimSpace(dsn) == "" || len(dsn) > 8192 || token == "" {
		return errConfig
	}
	keys, err := metareply.LoadPageTokenKeyring(getenv)
	if err != nil {
		return errConfig
	}
	version, err := register(ctx, dsn, keys, r, token)
	if err != nil {
		return err
	}
	out, err := json.Marshal(map[string]int64{"version": version})
	if err != nil {
		return errConfig
	}
	_, err = stdout.Write(append(out, '\n'))
	return err
}

// runRoute: `route` binds a Meta webhook asset to a store (R1 ruling F2); `route-disable` CAS-disables it.
// The operator attests ownership with --proof (sha256 hex of the saved evidence, docs/runbooks/deploy.md §6.3).
func runRoute(ctx context.Context, args []string, getenv func(string) string, stdout io.Writer) error {
	fs := flag.NewFlagSet(args[0], flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	var r metareply.RouteRegistration
	var expires, routeID string
	fs.StringVar(&r.TenantID, "tenant", "", "")
	fs.StringVar(&r.StoreID, "store", "", "")
	fs.StringVar(&r.PrincipalID, "principal", "", "")
	fs.StringVar(&r.AppID, "app", "", "")
	fs.StringVar(&r.Object, "object", "", "")
	fs.StringVar(&r.AssetID, "asset", "", "")
	fs.StringVar(&r.Proof, "proof", "", "")
	fs.StringVar(&expires, "proof-expires", "", "")
	fs.StringVar(&routeID, "route", "", "")
	fs.Int64Var(&r.ExpectedEpoch, "expected-epoch", -1, "")
	if fs.Parse(args[1:]) != nil || fs.NArg() != 0 {
		return errUsage
	}
	dsn := getenv("COMMERCE_META_REGISTRAR_DATABASE_URL")
	if strings.TrimSpace(dsn) == "" || len(dsn) > 8192 {
		return errConfig
	}
	var out any
	var err error
	if args[0] == "route-disable" {
		if !command.ValidID(routeID) || r.ExpectedEpoch <= 0 {
			return errUsage
		}
		out, err = disable(ctx, dsn, routeID, r.ExpectedEpoch)
	} else {
		t, perr := time.Parse(time.RFC3339, expires)
		if perr != nil || routeID != "" || !t.After(time.Now()) || !command.ValidID(r.TenantID) || !command.ValidID(r.StoreID) ||
			!command.ValidID(r.PrincipalID) || !assetPattern.MatchString(r.AppID) || !assetPattern.MatchString(r.AssetID) ||
			(r.Object != "page" && r.Object != "instagram") || !proofPattern.MatchString(r.Proof) || r.ExpectedEpoch < 0 {
			return errUsage
		}
		r.ProofExpires = t
		out, err = route(ctx, dsn, r)
	}
	if err != nil {
		return err
	}
	line, err := json.Marshal(out)
	if err != nil {
		return errConfig
	}
	_, err = stdout.Write(append(line, '\n'))
	return err
}
