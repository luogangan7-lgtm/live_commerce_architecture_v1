// actions.go answers the admin UI's "which write buttons may I show" question (ruling E2): three
// permission probes inside the caller's already-scoped transaction. It is a display hint only; every
// write and the export still re-authorize in the database.

package merchantorders

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
	"livecommerce/internal/platform"
)

// OrderActions reports which order-side actions the principal holds on the store.
type OrderActions struct {
	Refund           bool `json:"refund"`
	FulfillmentWrite bool `json:"fulfillment_write"`
	OrdersExport     bool `json:"orders_export"`
}

// Actions maps ErrForbidden to false; any other failure (revoked session, unknown store) is returned.
func Actions(ctx context.Context, tx pgx.Tx, scope platform.Scope, token string) (OrderActions, error) {
	if tx == nil || !validAuthorityInput(scope, token) {
		return OrderActions{}, platform.ErrUnauthorized
	}
	var out OrderActions
	for permission, dest := range map[string]*bool{"payments:refund": &out.Refund,
		"fulfillment:write": &out.FulfillmentWrite, "orders:export": &out.OrdersExport} {
		switch err := platform.RequirePermission(ctx, tx, scope, token, permission); {
		case err == nil:
			*dest = true
		case errors.Is(err, platform.ErrForbidden):
		default:
			return OrderActions{}, mapError(err)
		}
	}
	return out, nil
}
