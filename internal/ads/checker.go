package ads

import (
	"context"

	"github.com/jackc/pgx/v5/pgxpool"

	"livecommerce/internal/integrations/core"
)

// checker.go is the dispatcher Check of every meta_ads route (contract 6.1): PG only, no network, no secret. It asks one SQL
// definer (ads.check_create / check_activate / check_read, EXECUTE commerce_worker) and turns the returned BLOCKED_POLICY
// code into core.DenyPolicy, so the dispatcher records that exact code with zero provider calls. The rules live in SQL because
// they read the approval, the operation ledger and the allowance under the store lock; this file decides only which one applies.

// Checker is the PG-only Check for provider meta_ads. The CAPI Check is ads-capi's own route Check.
type Checker struct{ pool *pgxpool.Pool }

// NewChecker returns a Checker over the dispatcher's commerce_worker pool.
func NewChecker(pool *pgxpool.Pool) *Checker { return &Checker{pool: pool} }

// Check implements core.DispatchRoute.Check for meta_ads. Reconcile mode returns nil: a reconcile is query-only and has no
// effect, and a denial there would leave an UNKNOWN operation unreconcilable (ruling X2); the claim fence and the dispatcher's
// final gate still apply. Any action other than the eight meta_ads actions is denied unknown_action. A database error is
// returned as an error (never a denial): the dispatcher then records UNKNOWN policy_check_failed, because an unanswered Check
// must not read as permission and must not claim that no effect can exist.
func (c *Checker) Check(ctx context.Context, req core.DispatchRequest) error {
	if req.Mode == "reconcile" {
		return nil
	}
	if c == nil || c.pool == nil {
		return core.DenyPolicy("check_unavailable")
	}
	var fn string
	switch {
	case req.Provider != "meta_ads":
		return core.DenyPolicy("unknown_action")
	case req.Action == "meta.ads.create_campaign", req.Action == "meta.ads.create_adset", req.Action == "meta.ads.create_creative", req.Action == "meta.ads.create_ad":
		fn = "ads.check_create"
	case req.Action == "meta.ads.activate":
		fn = "ads.check_activate"
	case req.Action == "meta.ads.pause", req.Action == "meta.ads.preflight_account", req.Action == "meta.ads.read_insights":
		fn = "ads.check_read"
	default:
		return core.DenyPolicy("unknown_action")
	}
	var code string
	// fn is one of three constants above, never caller input; the operation id is a bind parameter.
	if err := c.pool.QueryRow(ctx, `SELECT `+fn+`($1::uuid)`, req.OperationID).Scan(&code); err != nil {
		return err
	}
	if code != "" {
		return core.DenyPolicy(code)
	}
	return nil
}
