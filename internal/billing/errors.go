// errors.go: the fixed sentinels of the billing surface and the PG error mapping. Errors never carry
// Stripe messages, response bodies, URLs, keys or driver details (I11, contract §12).

package billing

import (
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"livecommerce/internal/command"
	"livecommerce/internal/platform"
)

var (
	// ErrUnavailable: billing is disabled (config, BD1 account conflict) or Stripe could not be reached
	// or refused a call; HTTP 503 billing_unavailable. The webhook answers 503 for the same reason so
	// Stripe retries (F-B9).
	ErrUnavailable = errors.New("billing: unavailable")
	// ErrSubscriptionExists: the store already has a non-terminal subscription; HTTP 409.
	ErrSubscriptionExists = errors.New("billing: subscription exists")
	// ErrNoCustomer: no Stripe customer is pinned to the store yet (portal); HTTP 409 no_billing_customer.
	ErrNoCustomer = errors.New("billing: no billing customer")
	// ErrUnknownPrice: price_id is not one of the configured LC_BILLING_PRICE_IDS; HTTP 422.
	ErrUnknownPrice = errors.New("billing: unknown price")
	// ErrConfig: a billing environment value is unusable. Fixed text: the value is never echoed.
	ErrConfig = errors.New("billing: invalid configuration")
)

// mapError translates the PT4xx / constraint SQLSTATEs of the 0079 definers into the shared sentinels
// (the internal/claims mapError table). "billing_account_conflict" (BD1) means billing must stay off, so
// it is ErrUnavailable, not a generic conflict. Lock/deadlock/timeout errors and anything unknown are
// returned unchanged for the HTTP layer's 503 mapping.
func mapError(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return command.ErrNotFound
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		switch pgErr.Code {
		case "PT400", "22023", "22P02", "23514":
			return command.ErrInvalid
		case "PT409":
			if pgErr.Message == "billing_account_conflict" {
				return ErrUnavailable
			}
			return command.ErrConflict
		case "23505":
			return command.ErrConflict
		case "PT401":
			return platform.ErrUnauthorized
		case "PT403":
			return platform.ErrForbidden
		case "PT404":
			return platform.ErrScopeNotFound
		}
	}
	return err
}
