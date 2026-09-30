// read.go serves the merchant customer list and detail (contract §5): one call to
// identity.read_merchant_customers (0078, customers:read), strict decoding of its projection, and the order
// summaries of the detail page through the existing internal/merchantorders projection.
//
// Non-goals: no write, no privacy action (privacy.go), no order rule (merchantorders.Get is the single order
// projection), never an actor_key, owner session id or PSP reference: the SQL does not select them.

package customers

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"livecommerce/internal/buyer"
	"livecommerce/internal/command"
	"livecommerce/internal/merchantorders"
	"livecommerce/internal/pagination"
	"livecommerce/internal/platform"
)

// List returns one page of customers, newest activity first (keyset on (last_activity_at, id), D5).
// Empty Q means no filter. The cursor is bound to tenant, store and the search text.
func List(ctx context.Context, tx pgx.Tx, scope platform.Scope, token string, in ListRequest) (pagination.Page[Customer], error) {
	empty := pagination.Page[Customer]{Items: []Customer{}}
	if tx == nil || !validAuthorityInput(scope, token) {
		return empty, command.ErrInvalid
	}
	q := ""
	filter := ""
	if in.Q != "" {
		var err error
		if q, err = NormalizeQuery(in.Q); err != nil {
			return empty, err
		}
		sum := sha256.Sum256([]byte(q))
		filter = hex.EncodeToString(sum[:])
	}
	binding := pagination.Binding{TenantID: scope.TenantID, StoreID: scope.StoreID, Collection: "customers", Filter: filter}
	limit, keys, err := pagination.Decode(in.Page, binding, 2)
	if err != nil {
		return empty, err
	}
	var afterTime, afterID any
	if len(keys) == 2 {
		afterTime, afterID = keys[0], keys[1]
	}
	var qArg any
	if q != "" {
		qArg = q
	}
	raw, err := read(ctx, tx, scope, token, nil, limit+1, afterTime, afterID, qArg)
	if err != nil {
		return empty, err
	}
	var objects []json.RawMessage
	if !bytes.HasPrefix(bytes.TrimSpace(raw), []byte("[")) || json.Unmarshal(raw, &objects) != nil || len(objects) > limit+1 {
		return empty, ErrUnavailable
	}
	for _, object := range objects {
		c, err := decodeCustomer(object)
		if err != nil {
			return empty, err
		}
		empty.Items = append(empty.Items, c)
	}
	if len(empty.Items) > limit {
		empty.Items = empty.Items[:limit]
		last := empty.Items[limit-1]
		if empty.NextCursor, err = pagination.Encode(binding, []string{last.LastActivityAt, last.CustomerID}); err != nil {
			return pagination.Page[Customer]{Items: []Customer{}}, ErrUnavailable
		}
	}
	return empty, nil
}

// Get returns one customer with its newest orders, claims, consent history and privacy actions. An owner with
// no order and no bound bundle, or another store's owner, is 404 (indistinguishable).
func Get(ctx context.Context, tx pgx.Tx, scope platform.Scope, token, customerID string) (Detail, error) {
	d, _, err := loadDetail(ctx, tx, scope, token, customerID)
	if err != nil {
		return Detail{}, err
	}
	ids := d.OrderIDs
	if len(ids) > detailOrders {
		ids = ids[:detailOrders]
	}
	out := Detail{Customer: d.Customer, Orders: make([]merchantorders.Summary, 0, len(ids)), Claims: d.Claims,
		ConsentHistory: d.ConsentHistory, PrivacyActions: d.PrivacyActions}
	for _, id := range ids {
		// merchantorders.Get: existing order projection; the detail page must show what the order page shows.
		order, err := merchantorders.Get(ctx, tx, scope, token, id)
		if err != nil {
			return Detail{}, err
		}
		if order.OrderID != id {
			return Detail{}, ErrUnavailable
		}
		out.Orders = append(out.Orders, order.Summary)
	}
	return out, nil
}

// loadDetail reads and validates the detail row; total is the number of order ids the database returned
// (at most MaxExportOrders+1, so callers can tell "over the export cap" without an unbounded read).
func loadDetail(ctx context.Context, tx pgx.Tx, scope platform.Scope, token, customerID string) (detailWire, int, error) {
	if tx == nil || !validAuthorityInput(scope, token) || !command.ValidID(customerID) {
		return detailWire{}, 0, command.ErrInvalid
	}
	raw, err := read(ctx, tx, scope, token, customerID, 1, nil, nil, nil)
	if err != nil {
		return detailWire{}, 0, err
	}
	var objects []json.RawMessage
	if json.Unmarshal(raw, &objects) != nil || len(objects) > 1 {
		return detailWire{}, 0, ErrUnavailable
	}
	if len(objects) == 0 {
		return detailWire{}, 0, command.ErrNotFound
	}
	d, err := decodeDetail(objects[0])
	if err != nil || d.CustomerID != customerID {
		return detailWire{}, 0, ErrUnavailable
	}
	return d, len(d.OrderIDs), nil
}

func read(ctx context.Context, tx pgx.Tx, scope platform.Scope, token string, customerID any, limit int, afterTime, afterID, q any) ([]byte, error) {
	hash := sha256.Sum256([]byte(token))
	var raw []byte
	// identity.read_merchant_customers: customers:read projection over buyer.owners (0078); the definer re-verifies
	// the GUCs WithScope set and fences authority again after its reads.
	err := tx.QueryRow(ctx, `SELECT identity.read_merchant_customers($1,$2::uuid,$3::uuid,$4,$5::timestamptz,$6::uuid,$7)`,
		hash[:], scope.StoreID, customerID, limit, afterTime, afterID, q).Scan(&raw)
	if err != nil {
		return nil, mapMerchantError(err)
	}
	// Second fence with the original Go Scope (merchantorders.read pattern).
	if err := platform.RequirePermission(ctx, tx, scope, token, "customers:read"); err != nil {
		return nil, mapMerchantError(err)
	}
	if len(raw) == 0 {
		return nil, ErrUnavailable
	}
	return raw, nil
}

// mapMerchantError turns a database or platform error into a stable sentinel; raw pgconn messages can carry
// customer values and never leave this package.
func mapMerchantError(err error) error {
	var pg *pgconn.PgError
	if errors.As(err, &pg) {
		switch pg.Code {
		case "PT400":
			return command.ErrInvalid
		case "PT401":
			return platform.ErrUnauthorized
		case "PT403":
			return platform.ErrForbidden
		case "PT404":
			return platform.ErrScopeNotFound
		case "PT409":
			return mapConflict(pg.Message)
		case "PT410":
			return ErrErased
		case "PT503":
			return ErrUnavailable
		case "40001", "40P01", "55P03", "57014", "23505":
			return err // the HTTP layer maps deadlocks/lock timeouts to a retry and unique races to conflict
		}
		return ErrUnavailable
	}
	if errors.Is(err, platform.ErrUnauthorized) || errors.Is(err, platform.ErrForbidden) ||
		errors.Is(err, platform.ErrScopeNotFound) || errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return err
	}
	return ErrUnavailable
}

// mapBuyerError is mapMerchantError for buyer definers: PT401 is a bad or revoked capability.
func mapBuyerError(err error) error {
	var pg *pgconn.PgError
	if errors.As(err, &pg) {
		switch pg.Code {
		case "PT400":
			return command.ErrInvalid
		case "PT401":
			return buyer.ErrUnauthorized
		case "PT409":
			return mapConflict(pg.Message)
		case "PT410":
			return ErrErased
		case "40001", "40P01", "55P03", "57014", "23505":
			return err
		}
		return ErrUnavailable
	}
	if errors.Is(err, buyer.ErrUnauthorized) || errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return err
	}
	return ErrUnavailable
}

// mapConflict distinguishes the two PT409 messages the 0078 definers raise (D3).
func mapConflict(message string) error {
	if message == "erasure_blocked" {
		return ErrErasureBlocked
	}
	return ErrIdempotencyConflict
}

func validAuthorityInput(scope platform.Scope, token string) bool {
	return command.ValidID(scope.TenantID) && command.ValidID(scope.StoreID) && command.ValidID(scope.PrincipalID) &&
		scope.Revision > 0 && len(token) >= 32 && len(token) <= 512
}
