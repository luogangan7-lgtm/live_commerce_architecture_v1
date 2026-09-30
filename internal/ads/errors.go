package ads

import (
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"livecommerce/internal/command"
	"livecommerce/internal/platform"
)

// errors.go maps every refusal (local validation, SQL definer, driver) to one of the frozen error codes of
// docs/delivery/units/ads-core.md so the HTTP layer never returns a driver message. It touches no table.

// Refusal is one frozen domain refusal: an HTTP status and the frozen error code the body carries.
type Refusal struct {
	Status int
	Code   string
}

func (r *Refusal) Error() string { return "ads refusal: " + r.Code }

// refusal builds the Refusal for a frozen code with its status. Statuses follow the ADnnn SQLSTATE the
// migration raises (nnn = HTTP status); billing_restricted is 402 everywhere (ruling B9).
func refusal(code string) *Refusal { return &Refusal{Status: statusOf(code), Code: code} }

var (
	frozenStatus = map[string]int{
		"state_mismatch": http.StatusConflict, "state_expired": http.StatusGone, "meta_connect_failed": http.StatusBadGateway,
		"not_in_pick_list": http.StatusUnprocessableEntity, "client_business_changed": http.StatusConflict,
		"revision_changed": http.StatusConflict, "over_allowance": http.StatusConflict, "billing_restricted": http.StatusPaymentRequired,
		"attempt_changed": http.StatusConflict, "prior_attempt_not_paused": http.StatusConflict, "draft_approved": http.StatusConflict,
		"budget_below_minimum": http.StatusUnprocessableEntity, "not_whole_unit": http.StatusUnprocessableEntity,
		"currency_mismatch": http.StatusUnprocessableEntity, "starts_too_soon": http.StatusUnprocessableEntity,
		"binding_disabled": http.StatusConflict, "source_not_owned": http.StatusUnprocessableEntity,
		"product_not_published": http.StatusUnprocessableEntity, "forbidden": http.StatusForbidden,
		"not_found": http.StatusNotFound, "invalid_request": http.StatusUnprocessableEntity,
		"unauthorized": http.StatusUnauthorized, "conflict": http.StatusConflict,
	}
)

func statusOf(code string) int {
	if s, ok := frozenStatus[code]; ok {
		return s
	}
	return http.StatusConflict
}

// mapError converts a definer/driver error into a Refusal, a platform/command sentinel, or leaves an unknown
// error untouched for the caller's 503 classification. SQLSTATE ADnnn carries MESSAGE = a frozen code; only a
// code in the frozen table is echoed (anything else becomes "conflict"), never the driver text.
func mapError(err error) error {
	if err == nil {
		return nil
	}
	var refused *Refusal
	if errors.As(err, &refused) || errors.Is(err, command.ErrInvalid) || errors.Is(err, command.ErrConflict) ||
		errors.Is(err, command.ErrNotFound) || errors.Is(err, platform.ErrUnauthorized) ||
		errors.Is(err, platform.ErrForbidden) || errors.Is(err, platform.ErrScopeNotFound) || errors.Is(err, ErrConnectFailed) {
		return err
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return command.ErrNotFound
	}
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		return err
	}
	if strings.HasPrefix(pgErr.Code, "AD") && len(pgErr.Code) == 5 {
		if _, numeric := strconv.Atoi(pgErr.Code[2:]); numeric == nil {
			code := pgErr.Message
			if _, ok := frozenStatus[code]; !ok {
				code = "conflict"
			}
			return refusal(code)
		}
	}
	switch pgErr.Code {
	case "40001", "23505", "PT409":
		return command.ErrConflict
	case "23503", "P0002":
		return command.ErrNotFound
	case "22001", "22007", "22008", "22023", "22P02", "23514":
		return command.ErrInvalid
	}
	return err
}
