// Package storefront owns buyer purchase intent and immutable price snapshots: the buyer catalog
// read, cart, delivery destination and quote/revalidation.
//
// It never reserves stock, accepts payment, resolves identity or calls providers.
package storefront

import (
	"context"
	"errors"
	"math"
	"sort"

	"github.com/jackc/pgx/v5" // All writes join the authenticated buyer transaction.
	"livecommerce/internal/buyer"
	"livecommerce/internal/command"
)

type Item struct {
	SKUID    string `json:"sku_id"`
	Quantity int64  `json:"quantity"`
}
type Cart struct {
	ID       string `json:"id"`
	Currency string `json:"currency"`
	Version  int64  `json:"version"`
	Items    []Item `json:"items"`
}
type CartInput struct {
	ExpectedVersion int64  `json:"expected_version"`
	Items           []Item `json:"items"`
}

func GetCart(ctx context.Context, tx pgx.Tx, s buyer.Scope) (Cart, error) {
	if err := buyer.CheckScope(ctx, tx, s); err != nil {
		return Cart{}, err
	}
	return readCart(ctx, tx, s, false)
}

func SetCart(ctx context.Context, tx pgx.Tx, s buyer.Scope, key string, in CartInput) (out Cart, err error) {
	if in.ExpectedVersion < 0 || in.ExpectedVersion == math.MaxInt64 {
		return out, command.ErrInvalid
	}
	in.Items, err = canonicalItems(in.Items)
	if err != nil {
		return out, err
	}
	err = buyer.RunCommand(ctx, tx, s, "cart.set", key, in, &out, func() error {
		// A missing row cannot be locked. Serialize only this owner's creation,
		// not the store; existing row locks protect readers and future checkout.
		if err := LockCartOwner(ctx, tx, s); err != nil {
			return err
		}
		current, err := readCart(ctx, tx, s, true)
		if err != nil {
			return err
		}
		if current.Version != in.ExpectedVersion {
			return command.ErrConflict
		}
		if _, err = lockCatalog(ctx, tx, s, current.Currency, in.Items); err != nil {
			return err
		}
		if current.ID == "" {
			err = tx.QueryRow(ctx, `INSERT INTO storefront.carts(tenant_id,store_id,owner_id,creator_session_id,currency)
				VALUES($1,$2,$3,$4,$5) RETURNING id::text`, s.TenantID, s.StoreID, s.OwnerID, s.SessionID, current.Currency).Scan(&current.ID)
			if err != nil {
				return err
			}
		}
		if _, err = tx.Exec(ctx, `DELETE FROM storefront.cart_lines WHERE tenant_id=$1 AND store_id=$2 AND owner_id=$3 AND cart_id=$4`, s.TenantID, s.StoreID, s.OwnerID, current.ID); err != nil {
			return err
		}
		for _, item := range in.Items {
			if _, err = tx.Exec(ctx, `INSERT INTO storefront.cart_lines(tenant_id,store_id,owner_id,cart_id,sku_id,quantity) VALUES($1,$2,$3,$4,$5,$6)`, s.TenantID, s.StoreID, s.OwnerID, current.ID, item.SKUID, item.Quantity); err != nil {
				return err
			}
		}
		err = tx.QueryRow(ctx, `UPDATE storefront.carts SET version=version+1 WHERE tenant_id=$1 AND store_id=$2 AND owner_id=$3 AND id=$4 RETURNING version`, s.TenantID, s.StoreID, s.OwnerID, current.ID).Scan(&current.Version)
		if err != nil {
			return err
		}
		current.Items = in.Items
		out = current
		return event(ctx, tx, s, current.ID, "", "cart.updated")
	})
	return out, err
}

// LockCartOwner takes the scoped owner's cart-writer advisory lock
// ("cart|tenant|store|owner", transaction-scoped, released at COMMIT/ROLLBACK) after
// buyer.CheckScope, in a buyer.WithScope transaction. SetCart takes it inside its receipt;
// claims.RedeemLink takes it before GetCart->SetCart so two same-owner writers serialize
// here instead of deadlocking on GetCart's FOR SHARE followed by SetCart's FOR UPDATE
// (contracts/live-keyword-claims-v1.md R2, §5.6). Advisory locks are re-entrant within a
// transaction, so SetCart re-taking it during a redeem never waits. Integrator-owned
// helper; it reads and writes no table.
func LockCartOwner(ctx context.Context, tx pgx.Tx, s buyer.Scope) error {
	if err := buyer.CheckScope(ctx, tx, s); err != nil {
		return err
	}
	_, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, "cart|"+s.TenantID+"|"+s.StoreID+"|"+s.OwnerID)
	return err
}

// A read lock prevents a READ COMMITTED header/line tear across two statements.
// The absent GET has no row insertion, receipt, event or other durable write.
func readCart(ctx context.Context, tx pgx.Tx, s buyer.Scope, write bool) (out Cart, err error) {
	out.Items = []Item{}
	lock := " FOR SHARE"
	if write {
		lock = " FOR UPDATE"
	}
	err = tx.QueryRow(ctx, `SELECT id::text,currency,version FROM storefront.carts WHERE tenant_id=$1 AND store_id=$2 AND owner_id=$3`+lock, s.TenantID, s.StoreID, s.OwnerID).Scan(&out.ID, &out.Currency, &out.Version)
	if errors.Is(err, pgx.ErrNoRows) {
		err = tx.QueryRow(ctx, `SELECT currency FROM control.stores WHERE tenant_id=$1 AND id=$2`, s.TenantID, s.StoreID).Scan(&out.Currency)
		return out, notFound(err)
	}
	if err != nil {
		return out, err
	}
	rows, err := tx.Query(ctx, `SELECT sku_id::text,quantity FROM storefront.cart_lines WHERE tenant_id=$1 AND store_id=$2 AND owner_id=$3 AND cart_id=$4 ORDER BY sku_id`, s.TenantID, s.StoreID, s.OwnerID, out.ID)
	if err != nil {
		return out, err
	}
	defer rows.Close()
	for rows.Next() {
		var item Item
		if err = rows.Scan(&item.SKUID, &item.Quantity); err != nil {
			return out, err
		}
		out.Items = append(out.Items, item)
	}
	return out, rows.Err()
}

func canonicalItems(items []Item) ([]Item, error) {
	if len(items) > 50 {
		return nil, command.ErrInvalid
	}
	out := append([]Item{}, items...)
	sort.Slice(out, func(i, j int) bool { return out[i].SKUID < out[j].SKUID })
	for i, item := range out {
		if !command.ValidID(item.SKUID) || item.Quantity < 1 || item.Quantity > command.MaxQuantity || (i > 0 && out[i-1].SKUID == item.SKUID) {
			return nil, command.ErrInvalid
		}
	}
	return out, nil
}

// Lock order follows inventory: all products, then all SKUs. Product association
// is immutable in catalog commands; still recheck after locking for fail-closed
// behavior if a future importer changes that rule.
func lockCatalog(ctx context.Context, tx pgx.Tx, s buyer.Scope, currency string, items []Item) ([]QuoteLine, error) {
	products := map[string]QuoteLine{}
	parents := map[string]string{}
	for _, item := range items {
		var id string
		if err := tx.QueryRow(ctx, `SELECT product_id::text FROM catalog.skus WHERE tenant_id=$1 AND store_id=$2 AND id=$3`, s.TenantID, s.StoreID, item.SKUID).Scan(&id); err != nil {
			return nil, notFound(err)
		}
		parents[item.SKUID] = id
		products[id] = QuoteLine{}
	}
	ids := make([]string, 0, len(products))
	for id := range products {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		p := QuoteLine{ProductID: id}
		var status string
		err := tx.QueryRow(ctx, `SELECT name,description,version,status FROM catalog.products WHERE tenant_id=$1 AND store_id=$2 AND id=$3 FOR SHARE`, s.TenantID, s.StoreID, id).Scan(&p.Name, &p.Description, &p.ProductVersion, &status)
		if err != nil {
			return nil, notFound(err)
		}
		if status != "active" {
			return nil, command.ErrConflict
		}
		products[id] = p
	}
	lines := make([]QuoteLine, 0, len(items))
	for _, item := range items {
		line := products[parents[item.SKUID]]
		line.SKUID = item.SKUID
		line.Quantity = item.Quantity
		var actualParent, status, actualCurrency string
		err := tx.QueryRow(ctx, `SELECT product_id::text,code,version,price_minor,status,currency FROM catalog.skus WHERE tenant_id=$1 AND store_id=$2 AND id=$3 FOR SHARE`, s.TenantID, s.StoreID, item.SKUID).Scan(&actualParent, &line.Code, &line.SKUVersion, &line.UnitPriceMinor, &status, &actualCurrency)
		if err != nil {
			return nil, notFound(err)
		}
		if status != "active" || actualParent != line.ProductID || actualCurrency != currency {
			return nil, command.ErrConflict
		}
		lines = append(lines, line)
	}
	return lines, nil
}

func event(ctx context.Context, tx pgx.Tx, s buyer.Scope, cart, quote, action string) error {
	_, err := tx.Exec(ctx, `INSERT INTO storefront.events(tenant_id,store_id,owner_id,session_id,cart_id,quote_id,action) VALUES($1,$2,$3,$4,$5,nullif($6,'')::uuid,$7)`, s.TenantID, s.StoreID, s.OwnerID, s.SessionID, cart, quote, action)
	return err
}
func notFound(err error) error {
	if errors.Is(err, pgx.ErrNoRows) {
		return command.ErrNotFound
	}
	return err
}
