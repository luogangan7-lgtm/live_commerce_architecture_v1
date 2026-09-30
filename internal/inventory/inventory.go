// Package inventory owns ledger-backed physical inventory commands: warehouses, on-hand adjustment,
// reserve and release, and the pure allocation planner.
//
// It never writes a balance directly (every change is an append to inventory.ledger, whose trigger
// is the sole balance writer), never decides payment or refund policy, and never restocks on its own
// authority: callers supply the evidence-bearing command.
package inventory

import (
	"context"
	"errors"
	"sort"
	"strconv"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"livecommerce/internal/command"
	"livecommerce/internal/pagination"
	"livecommerce/internal/platform"
)

type Warehouse struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}
type Balance struct {
	WarehouseID string `json:"warehouse_id"`
	SKUID       string `json:"sku_id"`
	OnHand      int64  `json:"on_hand"`
	Reserved    int64  `json:"reserved"`
	Allocated   int64  `json:"allocated"`
	Unavailable int64  `json:"unavailable"`
	Version     int64  `json:"version"`
	Available   int64  `json:"available"`
}
type Adjustment struct {
	WarehouseID     string `json:"warehouse_id"`
	SKUID           string `json:"sku_id"`
	Delta           int64  `json:"delta"`
	ExpectedVersion int64  `json:"expected_version"`
	Reason          string `json:"reason"`
}
type Line struct {
	WarehouseID string `json:"warehouse_id"`
	SKUID       string `json:"sku_id"`
	Quantity    int64  `json:"quantity"`
}
type Reservation struct {
	ID        string    `json:"id"`
	State     string    `json:"state"`
	ExpiresAt time.Time `json:"expires_at"`
	Lines     []Line    `json:"lines"`
}

func CreateWarehouse(ctx context.Context, tx pgx.Tx, scope platform.Scope, key, name string) (out Warehouse, err error) {
	if !validScope(tx, scope) || utf8.RuneCountInString(name) < 1 || utf8.RuneCountInString(name) > 120 {
		return out, command.ErrInvalid
	}
	request := struct {
		Name string `json:"name"`
	}{name}
	err = command.Run(ctx, tx, scope, "inventory.warehouse.create", key, request, &out, func() error {
		err := tx.QueryRow(ctx, `INSERT INTO inventory.warehouses(tenant_id,store_id,name) VALUES($1,$2,$3) RETURNING id::text,name`, scope.TenantID, scope.StoreID, name).Scan(&out.ID, &out.Name)
		if err != nil {
			return err
		}
		return command.Audit(ctx, tx, scope, "inventory.warehouse.created")
	})
	return out, mapError(err)
}

func ListWarehouses(ctx context.Context, tx pgx.Tx, scope platform.Scope) ([]Warehouse, error) {
	page, err := ListWarehousesPage(ctx, tx, scope, pagination.Request{Limit: 100})
	return page.Items, err
}
func ListWarehousesPage(ctx context.Context, tx pgx.Tx, scope platform.Scope, request pagination.Request) (pagination.Page[Warehouse], error) {
	page := pagination.Page[Warehouse]{Items: make([]Warehouse, 0)}
	if !validScope(tx, scope) {
		return page, command.ErrInvalid
	}
	binding := pagination.Binding{TenantID: scope.TenantID, StoreID: scope.StoreID, Collection: "warehouses"}
	limit, after, err := pagination.Decode(request, binding, 1)
	if err != nil {
		return page, err
	}
	args := []any{scope.TenantID, scope.StoreID}
	if len(after) == 1 {
		args = append(args, after[0])
	}
	query := `SELECT id::text,name FROM inventory.warehouses WHERE tenant_id=$1 AND store_id=$2` + keysetID(after, 3) + ` ORDER BY id LIMIT $` + strconv.Itoa(len(args)+1)
	args = append(args, limit+1)
	rows, err := tx.Query(ctx, query, args...)
	if err != nil {
		return page, mapError(err)
	}
	defer rows.Close()
	for rows.Next() {
		var w Warehouse
		if err := rows.Scan(&w.ID, &w.Name); err != nil {
			return page, err
		}
		page.Items = append(page.Items, w)
	}
	if err := rows.Err(); err != nil {
		return page, mapError(err)
	}
	if len(page.Items) <= limit {
		return page, nil
	}
	page.Items = page.Items[:limit]
	page.NextCursor, err = pagination.Encode(binding, []string{page.Items[len(page.Items)-1].ID})
	return page, err
}

func ListBalances(ctx context.Context, tx pgx.Tx, scope platform.Scope) ([]Balance, error) {
	page, err := ListBalancesPage(ctx, tx, scope, pagination.Request{Limit: 100})
	return page.Items, err
}
func ListBalancesPage(ctx context.Context, tx pgx.Tx, scope platform.Scope, request pagination.Request) (pagination.Page[Balance], error) {
	page := pagination.Page[Balance]{Items: make([]Balance, 0)}
	if !validScope(tx, scope) {
		return page, command.ErrInvalid
	}
	binding := pagination.Binding{TenantID: scope.TenantID, StoreID: scope.StoreID, Collection: "inventory"}
	limit, after, err := pagination.Decode(request, binding, 2)
	if err != nil {
		return page, err
	}
	args := []any{scope.TenantID, scope.StoreID}
	predicate := ""
	if len(after) == 2 {
		predicate = ` AND (warehouse_id,sku_id)>($3::uuid,$4::uuid)`
		args = append(args, after[0], after[1])
	}
	query := `SELECT warehouse_id::text,sku_id::text,on_hand,reserved,allocated,unavailable,version FROM inventory.balances WHERE tenant_id=$1 AND store_id=$2` + predicate + ` ORDER BY warehouse_id,sku_id LIMIT $` + strconv.Itoa(len(args)+1)
	args = append(args, limit+1)
	rows, err := tx.Query(ctx, query, args...)
	if err != nil {
		return page, mapError(err)
	}
	defer rows.Close()
	for rows.Next() {
		var b Balance
		if err := rows.Scan(balanceFields(&b)...); err != nil {
			return page, err
		}
		b.Available = b.OnHand - b.Reserved - b.Allocated - b.Unavailable
		page.Items = append(page.Items, b)
	}
	if err := rows.Err(); err != nil {
		return page, mapError(err)
	}
	if len(page.Items) <= limit {
		return page, nil
	}
	page.Items = page.Items[:limit]
	last := page.Items[len(page.Items)-1]
	page.NextCursor, err = pagination.Encode(binding, []string{last.WarehouseID, last.SKUID})
	return page, err
}

func keysetID(after []string, position int) string {
	if len(after) == 1 {
		return ` AND id>$` + strconv.Itoa(position) + `::uuid`
	}
	return ``
}

func AdjustOnHand(ctx context.Context, tx pgx.Tx, scope platform.Scope, key string, in Adjustment) (out Balance, err error) {
	if !validScope(tx, scope) || !validAdjustment(in) {
		return out, command.ErrInvalid
	}
	err = command.Run(ctx, tx, scope, "inventory.adjust", key, in, &out, func() error {
		if err := activeSKU(ctx, tx, scope, in.SKUID); err != nil {
			return err
		}
		if err := activeWarehouse(ctx, tx, scope, in.WarehouseID); err != nil {
			return err
		}
		// LOCK: missing balance creation must serialize before the privileged lock/read.
		lockKey := "balance|" + scope.TenantID + "|" + scope.StoreID + "|" + in.WarehouseID + "|" + in.SKUID
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, lockKey); err != nil {
			return err
		}
		current, found, err := lockBalance(ctx, tx, in.WarehouseID, in.SKUID)
		if err != nil {
			return err
		}
		if !found {
			if in.ExpectedVersion != 0 {
				return command.ErrConflict
			}
			if in.Delta < 0 {
				return command.ErrInsufficient
			}
		} else {
			if in.ExpectedVersion == 0 || current.Version != in.ExpectedVersion {
				return command.ErrConflict
			}
			if !canAdjust(current, in.Delta) {
				return command.ErrInsufficient
			}
		}
		if err := insertLedger(ctx, tx, scope, "ADJUST", in.WarehouseID, in.SKUID, in.Delta, 0, "inventory.adjust", key, "", in.Reason); err != nil {
			return err
		}
		out, _, err = readBalance(ctx, tx, scope, in.WarehouseID, in.SKUID)
		if err != nil {
			return err
		}
		return command.Audit(ctx, tx, scope, "inventory.adjusted")
	})
	return out, mapError(err)
}

func Reserve(ctx context.Context, tx pgx.Tx, scope platform.Scope, key string, lines []Line) (out Reservation, err error) {
	canonical, err := canonicalLines(lines)
	if err != nil {
		return out, err
	}
	if !validScope(tx, scope) {
		return out, command.ErrInvalid
	}
	request := struct {
		Lines []Line `json:"lines"`
	}{canonical}
	err = command.Run(ctx, tx, scope, "inventory.reserve", key, request, &out, func() error {
		if err := lockCatalogForLines(ctx, tx, scope, canonical); err != nil {
			return err
		}
		for _, line := range canonical {
			if err := activeWarehouse(ctx, tx, scope, line.WarehouseID); err != nil {
				return err
			}
		}
		// Header is created before balance locks; any later error aborts the caller transaction.
		if err := tx.QueryRow(ctx, `INSERT INTO inventory.reservations(tenant_id,store_id,state) VALUES($1,$2,'HELD') RETURNING id::text,state,expires_at`, scope.TenantID, scope.StoreID).Scan(&out.ID, &out.State, &out.ExpiresAt); err != nil {
			return err
		}
		for _, line := range canonical {
			balance, found, err := lockBalance(ctx, tx, line.WarehouseID, line.SKUID)
			if err != nil {
				return err
			}
			if !found {
				return command.ErrNotFound
			}
			if balance.Available < line.Quantity {
				return command.ErrInsufficient
			}
		}
		for _, line := range canonical {
			if _, err := tx.Exec(ctx, `INSERT INTO inventory.reservation_lines(tenant_id,store_id,reservation_id,warehouse_id,sku_id,quantity) VALUES($1,$2,$3,$4,$5,$6)`, scope.TenantID, scope.StoreID, out.ID, line.WarehouseID, line.SKUID, line.Quantity); err != nil {
				return err
			}
			if err := insertLedger(ctx, tx, scope, "RESERVE", line.WarehouseID, line.SKUID, 0, line.Quantity, "inventory.reserve", key, out.ID, ""); err != nil {
				return err
			}
		}
		out.Lines = canonical
		return command.Audit(ctx, tx, scope, "inventory.reserved")
	})
	return out, mapError(err)
}

func ReleaseReservation(ctx context.Context, tx pgx.Tx, scope platform.Scope, key, id string, expire bool) (out Reservation, err error) {
	if !validScope(tx, scope) || !command.ValidID(id) {
		return out, command.ErrInvalid
	}
	operation := "inventory.release"
	if expire {
		operation = "inventory.expire"
	}
	request := struct {
		ID     string `json:"id"`
		Expire bool   `json:"expire"`
	}{id, expire}
	err = command.Run(ctx, tx, scope, operation, key, request, &out, func() error {
		var expired bool
		// LOCK: header first, then sorted immutable lines/balances. Pending/committed stay payment-owned.
		err := tx.QueryRow(ctx, `SELECT id::text,state,expires_at,clock_timestamp()>=expires_at FROM inventory.reservations WHERE tenant_id=$1 AND store_id=$2 AND id=$3 FOR UPDATE`, scope.TenantID, scope.StoreID, id).Scan(&out.ID, &out.State, &out.ExpiresAt, &expired)
		if err != nil {
			return err
		}
		lines, err := reservationLines(ctx, tx, scope, id)
		if err != nil {
			return err
		}
		out.Lines = lines
		if out.State == "RELEASED" || out.State == "EXPIRED" {
			return nil
		}
		if out.State != "HELD" {
			return command.ErrConflict
		}
		if expire && !expired {
			return command.ErrConflict
		}
		for _, line := range lines {
			balance, found, err := lockBalance(ctx, tx, line.WarehouseID, line.SKUID)
			if err != nil {
				return err
			}
			if !found || balance.Reserved < line.Quantity {
				return command.ErrConflict
			}
		}
		for _, line := range lines {
			if err := insertLedger(ctx, tx, scope, "RELEASE", line.WarehouseID, line.SKUID, 0, -line.Quantity, operation, key, id, ""); err != nil {
				return err
			}
		}
		if expire {
			out.State = "EXPIRED"
		} else {
			out.State = "RELEASED"
		}
		if _, err := tx.Exec(ctx, `UPDATE inventory.reservations SET state=$4 WHERE tenant_id=$1 AND store_id=$2 AND id=$3`, scope.TenantID, scope.StoreID, id, out.State); err != nil {
			return err
		}
		if expire {
			return command.Audit(ctx, tx, scope, "inventory.expired")
		}
		return command.Audit(ctx, tx, scope, "inventory.released")
	})
	return out, mapError(err)
}

func lockCatalogForLines(ctx context.Context, tx pgx.Tx, scope platform.Scope, lines []Line) error {
	type pair struct{ product, sku string }
	pairs := make([]pair, 0, len(lines))
	seen := map[string]bool{}
	for _, line := range lines {
		var product string
		err := tx.QueryRow(ctx, `SELECT product_id::text FROM catalog.skus WHERE tenant_id=$1 AND store_id=$2 AND id=$3`, scope.TenantID, scope.StoreID, line.SKUID).Scan(&product)
		if err != nil {
			return mapError(err)
		}
		k := product + line.SKUID
		if !seen[k] {
			seen[k] = true
			pairs = append(pairs, pair{product, line.SKUID})
		}
	}
	sort.Slice(pairs, func(i, j int) bool {
		if pairs[i].product == pairs[j].product {
			return pairs[i].sku < pairs[j].sku
		}
		return pairs[i].product < pairs[j].product
	})
	products := make([]string, 0, len(pairs))
	for _, pair := range pairs {
		if len(products) == 0 || products[len(products)-1] != pair.product {
			products = append(products, pair.product)
		}
	}
	// LOCK: every product lock precedes every SKU lock; both sets are globally sorted.
	for _, product := range products {
		var productStatus string
		if err := tx.QueryRow(ctx, `SELECT status FROM catalog.products WHERE tenant_id=$1 AND store_id=$2 AND id=$3 FOR SHARE`, scope.TenantID, scope.StoreID, product).Scan(&productStatus); err != nil {
			return mapError(err)
		}
		if productStatus != "active" {
			return command.ErrConflict
		}
	}
	for _, pair := range pairs {
		var skuStatus string
		if err := tx.QueryRow(ctx, `SELECT status FROM catalog.skus WHERE tenant_id=$1 AND store_id=$2 AND id=$3 FOR SHARE`, scope.TenantID, scope.StoreID, pair.sku).Scan(&skuStatus); err != nil {
			return mapError(err)
		}
		if skuStatus != "active" {
			return command.ErrConflict
		}
	}
	return nil
}

func activeSKU(ctx context.Context, tx pgx.Tx, scope platform.Scope, skuID string) error {
	return lockCatalogForLines(ctx, tx, scope, []Line{{SKUID: skuID}})
}
func activeWarehouse(ctx context.Context, tx pgx.Tx, scope platform.Scope, id string) error {
	var active bool
	err := tx.QueryRow(ctx, `SELECT active FROM inventory.warehouses WHERE tenant_id=$1 AND store_id=$2 AND id=$3`, scope.TenantID, scope.StoreID, id).Scan(&active)
	if err != nil {
		return mapError(err)
	}
	if !active {
		return command.ErrConflict
	}
	return nil
}

func lockBalance(ctx context.Context, tx pgx.Tx, warehouseID, skuID string) (Balance, bool, error) {
	var b Balance
	err := tx.QueryRow(ctx, `SELECT warehouse_id::text,sku_id::text,on_hand,reserved,allocated,unavailable,version FROM inventory.lock_balance($1::uuid,$2::uuid)`, warehouseID, skuID).Scan(balanceFields(&b)...)
	if errors.Is(err, pgx.ErrNoRows) {
		return b, false, nil
	}
	if err != nil {
		return b, false, mapError(err)
	}
	b.Available = b.OnHand - b.Reserved - b.Allocated - b.Unavailable
	return b, true, nil
}
func readBalance(ctx context.Context, tx pgx.Tx, scope platform.Scope, warehouseID, skuID string) (Balance, bool, error) {
	var b Balance
	err := tx.QueryRow(ctx, `SELECT warehouse_id::text,sku_id::text,on_hand,reserved,allocated,unavailable,version FROM inventory.balances WHERE tenant_id=$1 AND store_id=$2 AND warehouse_id=$3 AND sku_id=$4`, scope.TenantID, scope.StoreID, warehouseID, skuID).Scan(balanceFields(&b)...)
	if errors.Is(err, pgx.ErrNoRows) {
		return b, false, nil
	}
	if err != nil {
		return b, false, mapError(err)
	}
	b.Available = b.OnHand - b.Reserved - b.Allocated - b.Unavailable
	return b, true, nil
}
func reservationLines(ctx context.Context, tx pgx.Tx, scope platform.Scope, id string) ([]Line, error) {
	rows, err := tx.Query(ctx, `SELECT warehouse_id::text,sku_id::text,quantity FROM inventory.reservation_lines WHERE tenant_id=$1 AND store_id=$2 AND reservation_id=$3 ORDER BY warehouse_id,sku_id`, scope.TenantID, scope.StoreID, id)
	if err != nil {
		return nil, mapError(err)
	}
	defer rows.Close()
	out := make([]Line, 0)
	for rows.Next() {
		var line Line
		if err := rows.Scan(&line.WarehouseID, &line.SKUID, &line.Quantity); err != nil {
			return nil, err
		}
		out = append(out, line)
	}
	return out, mapError(rows.Err())
}
func insertLedger(ctx context.Context, tx pgx.Tx, scope platform.Scope, kind, warehouseID, skuID string, onHand, reserved int64, operation, key, reservationID, reason string) error {
	_, err := tx.Exec(ctx, `INSERT INTO inventory.ledger(tenant_id,store_id,warehouse_id,sku_id,kind,delta_on_hand,delta_reserved,delta_allocated,delta_unavailable,operation,command_key,reservation_id,reason,principal_id) VALUES($1,$2,$3,$4,$5,$6,$7,0,0,$8,$9,NULLIF($10,'')::uuid,$11,$12)`, scope.TenantID, scope.StoreID, warehouseID, skuID, kind, onHand, reserved, operation, key, reservationID, reason, scope.PrincipalID)
	return err
}
func balanceFields(b *Balance) []any {
	return []any{&b.WarehouseID, &b.SKUID, &b.OnHand, &b.Reserved, &b.Allocated, &b.Unavailable, &b.Version}
}
func canAdjust(b Balance, delta int64) bool {
	if delta > 0 {
		return b.OnHand <= command.MaxQuantity-delta
	}
	return delta >= -(b.OnHand - b.Reserved - b.Allocated - b.Unavailable)
}
func canonicalLines(raw []Line) ([]Line, error) {
	if len(raw) < 1 || len(raw) > 50 {
		return nil, command.ErrInvalid
	}
	merged := make(map[string]Line, len(raw))
	for _, line := range raw {
		if !command.ValidID(line.WarehouseID) || !command.ValidID(line.SKUID) || line.Quantity < 1 || line.Quantity > command.MaxQuantity {
			return nil, command.ErrInvalid
		}
		k := line.WarehouseID + line.SKUID
		old := merged[k]
		if old.Quantity > command.MaxQuantity-line.Quantity {
			return nil, command.ErrInvalid
		}
		line.Quantity += old.Quantity
		merged[k] = line
	}
	out := make([]Line, 0, len(merged))
	for _, line := range merged {
		out = append(out, line)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].WarehouseID == out[j].WarehouseID {
			return out[i].SKUID < out[j].SKUID
		}
		return out[i].WarehouseID < out[j].WarehouseID
	})
	return out, nil
}
func validAdjustment(in Adjustment) bool {
	return command.ValidID(in.WarehouseID) && command.ValidID(in.SKUID) && in.Delta >= -command.MaxQuantity && in.Delta <= command.MaxQuantity && in.Delta != 0 && in.ExpectedVersion >= 0 && utf8.RuneCountInString(in.Reason) >= 1 && utf8.RuneCountInString(in.Reason) <= 240
}
func validScope(tx pgx.Tx, s platform.Scope) bool {
	return tx != nil && command.ValidID(s.TenantID) && command.ValidID(s.StoreID) && command.ValidID(s.PrincipalID)
}
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
		case "23505":
			return command.ErrConflict
		case "23503":
			return command.ErrNotFound
		case "22001", "22007", "22008", "22023", "22P02", "23514":
			return command.ErrInvalid
		}
	}
	return err
}
