package metareply

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"livecommerce/internal/command"
)

var (
	numericID  = regexp.MustCompile(`^[0-9]{1,40}$`)
	proofShape = regexp.MustCompile(`^[0-9a-f]{64}$`)
)

// RouteRegistration is one operator route activation (R1 ruling F2): the Meta app/object/asset of a
// webhook subscription mapped to a store. Proof is the operator's lowercase sha256 hex of the saved
// ownership evidence (meta-inbox-v1 activate_route); ExpectedEpoch 0 creates the route, >0 replaces it.
type RouteRegistration struct {
	TenantID, StoreID, PrincipalID string
	AppID, Object, AssetID         string // Object: page | instagram
	Proof                          string
	ProofExpires                   time.Time
	ExpectedEpoch                  int64
}

// RouteResult names the binding (for `meta-admin page-token --binding`) and the route epoch (for the
// next CAS replace/disable).
type RouteResult struct {
	BindingID      string `json:"binding_id"`
	BindingVersion int64  `json:"binding_version"`
	RouteID        string `json:"route_id"`
	RouteEpoch     int64  `json:"route_epoch"`
}

// RegisterRoute creates or reuses the store's Meta binding and activates the webhook route in ONE
// transaction on the registrar pool: integration.register_meta_binding (0066, definer
// commerce_integration_writer) then meta_inbox.activate_route (0028, definer commerce_meta_writer).
// EXECUTE on both is granted to commerce_meta_registrar only; that grant is the authority check.
func RegisterRoute(ctx context.Context, pool *pgxpool.Pool, r RouteRegistration) (RouteResult, error) {
	var out RouteResult
	if ctx == nil || pool == nil || !command.ValidID(r.TenantID) || !command.ValidID(r.StoreID) || !command.ValidID(r.PrincipalID) ||
		!numericID.MatchString(r.AppID) || !numericID.MatchString(r.AssetID) || (r.Object != "page" && r.Object != "instagram") ||
		!proofShape.MatchString(r.Proof) || r.ProofExpires.IsZero() || r.ExpectedEpoch < 0 || r.ExpectedEpoch >= 1<<62 {
		return out, command.ErrInvalid
	}
	provider := "facebook" // meta-inbox-v1: page -> facebook, instagram -> instagram
	if r.Object == "instagram" {
		provider = "instagram"
	}
	err := pgx.BeginTxFunc(ctx, pool, pgx.TxOptions{IsoLevel: pgx.ReadCommitted}, func(tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `SELECT binding_id::text, binding_version FROM integration.register_meta_binding($1::uuid,$2::uuid,$3::uuid,$4,$5)`,
			r.TenantID, r.StoreID, r.PrincipalID, provider, r.AssetID).Scan(&out.BindingID, &out.BindingVersion); err != nil {
			return err
		}
		return tx.QueryRow(ctx, `SELECT route_id::text, route_epoch FROM meta_inbox.activate_route($1,$2,$3,$4::uuid,$5::uuid,$6::uuid,$7,$8,$9,$10)`,
			r.AppID, r.Object, r.AssetID, r.TenantID, r.StoreID, out.BindingID, out.BindingVersion, r.Proof, r.ProofExpires, r.ExpectedEpoch).
			Scan(&out.RouteID, &out.RouteEpoch)
	})
	if err != nil {
		return RouteResult{}, registrarError("register meta route", err)
	}
	return out, nil
}

// DisableRoute CAS-disables a route (meta_inbox.disable_route, 0028) and returns the new epoch.
// Already-stored events are never changed; new webhook units for the asset quarantine.
func DisableRoute(ctx context.Context, pool *pgxpool.Pool, routeID string, expectedEpoch int64) (int64, error) {
	if ctx == nil || pool == nil || !command.ValidID(routeID) || expectedEpoch <= 0 || expectedEpoch >= 1<<62 {
		return 0, command.ErrInvalid
	}
	var epoch int64
	if err := pool.QueryRow(ctx, `SELECT meta_inbox.disable_route($1::uuid,$2)`, routeID, expectedEpoch).Scan(&epoch); err != nil {
		return 0, registrarError("disable meta route", err)
	}
	return epoch, nil
}

// registrarError maps the definers' fixed SQLSTATE classes; anything else (42501 scope denied, connection
// errors) is wrapped for the caller, which prints only a fixed code (never the driver message).
func registrarError(op string, err error) error {
	var pg *pgconn.PgError
	if errors.As(err, &pg) {
		switch pg.Code {
		case "22023":
			return command.ErrInvalid
		case "PT409", "40001", "23505":
			return command.ErrConflict
		}
	}
	return fmt.Errorf("%s: %w", op, err)
}
