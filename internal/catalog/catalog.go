// Package catalog owns the merchant-scoped catalog transaction slice: products, SKUs, price history,
// the wide product/SKU ledger read projection (contracts/admin-ledger-v1.md) and the purchase-entry
// read.
//
// It never writes stock (internal/inventory owns balances and the ledger), never opens a transaction
// (callers pass one from platform.WithScope), and never trusts a caller-supplied tenant or store.
package catalog

import (
	"context"
	"errors"
	"regexp"
	"strconv"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"livecommerce/internal/command"
	"livecommerce/internal/pagination"
	"livecommerce/internal/platform"
)

var (
	skuCodePattern = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,64}$`)
	countryPattern = regexp.MustCompile(`^[A-Z]{2}$`)
	hsPattern      = regexp.MustCompile(`^[0-9]{6,12}$`)
)

type Product struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description"`
	Status      string `json:"status"`
	Version     int64  `json:"version"`
}

type ProductInput struct {
	Name            string `json:"name"`
	Description     string `json:"description"`
	ExpectedVersion int64  `json:"expected_version"`
}

type SKU struct {
	ID            string `json:"id"`
	ProductID     string `json:"product_id"`
	Code          string `json:"code"`
	Status        string `json:"status"`
	Currency      string `json:"currency"`
	PriceMinor    int64  `json:"price_minor"`
	Version       int64  `json:"version"`
	WeightGrams   int64  `json:"weight_grams"`
	LengthMM      int64  `json:"length_mm"`
	WidthMM       int64  `json:"width_mm"`
	HeightMM      int64  `json:"height_mm"`
	OriginCountry string `json:"origin_country"`
	CustomsName   string `json:"customs_name"`
	HSCandidate   string `json:"hs_candidate"`
}

type SKUInput struct {
	ProductID       string `json:"product_id"`
	Code            string `json:"code"`
	PriceMinor      int64  `json:"price_minor"`
	WeightGrams     int64  `json:"weight_grams"`
	LengthMM        int64  `json:"length_mm"`
	WidthMM         int64  `json:"width_mm"`
	HeightMM        int64  `json:"height_mm"`
	OriginCountry   string `json:"origin_country"`
	CustomsName     string `json:"customs_name"`
	HSCandidate     string `json:"hs_candidate"`
	ExpectedVersion int64  `json:"expected_version"`
}

type PriceInput struct {
	PriceMinor      int64 `json:"price_minor"`
	ExpectedVersion int64 `json:"expected_version"`
}

func CreateProduct(ctx context.Context, tx pgx.Tx, scope platform.Scope, key string, in ProductInput) (out Product, err error) {
	if !validProductInput(in, false) {
		return out, command.ErrInvalid
	}
	err = command.Run(ctx, tx, scope, "catalog.product.create", key, in, &out, func() error {
		err := tx.QueryRow(ctx, `INSERT INTO catalog.products(tenant_id,store_id,name,description)
			VALUES($1,$2,$3,$4) RETURNING id::text,name,description,status,version`,
			scope.TenantID, scope.StoreID, in.Name, in.Description).Scan(&out.ID, &out.Name, &out.Description, &out.Status, &out.Version)
		if err != nil {
			return err
		}
		return command.Audit(ctx, tx, scope, "catalog.product.created")
	})
	return out, mapError(err)
}

func UpdateProduct(ctx context.Context, tx pgx.Tx, scope platform.Scope, key, id string, in ProductInput) (out Product, err error) {
	if !command.ValidID(id) || !validProductInput(in, true) {
		return out, command.ErrInvalid
	}
	request := struct {
		ID string `json:"id"`
		ProductInput
	}{id, in}
	err = command.Run(ctx, tx, scope, "catalog.product.update", key, request, &out, func() error {
		if err := lockProduct(ctx, tx, scope, id); err != nil {
			return err
		}
		err := tx.QueryRow(ctx, `UPDATE catalog.products SET name=$3,description=$4,version=version+1,updated_at=clock_timestamp()
			WHERE tenant_id=$1 AND store_id=$2 AND id=$5 AND version=$6
			RETURNING id::text,name,description,status,version`, scope.TenantID, scope.StoreID, in.Name, in.Description, id, in.ExpectedVersion).
			Scan(&out.ID, &out.Name, &out.Description, &out.Status, &out.Version)
		if err != nil {
			return err
		}
		return command.Audit(ctx, tx, scope, "catalog.product.updated")
	})
	return out, mapVersionError(err)
}

func ArchiveProduct(ctx context.Context, tx pgx.Tx, scope platform.Scope, key, id string, expectedVersion int64) (out Product, err error) {
	if !command.ValidID(id) || expectedVersion < 1 {
		return out, command.ErrInvalid
	}
	request := struct {
		ID              string `json:"id"`
		ExpectedVersion int64  `json:"expected_version"`
	}{id, expectedVersion}
	err = command.Run(ctx, tx, scope, "catalog.product.archive", key, request, &out, func() error {
		// LOCK: catalog archive and inventory readers acquire product before SKU.
		if err := lockProduct(ctx, tx, scope, id); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `SELECT id FROM catalog.skus WHERE tenant_id=$1 AND store_id=$2 AND product_id=$3 ORDER BY id FOR UPDATE`, scope.TenantID, scope.StoreID, id); err != nil {
			return err
		}
		err := tx.QueryRow(ctx, `UPDATE catalog.products SET status='archived',version=version+1,updated_at=clock_timestamp()
			WHERE tenant_id=$1 AND store_id=$2 AND id=$3 AND version=$4
			RETURNING id::text,name,description,status,version`, scope.TenantID, scope.StoreID, id, expectedVersion).
			Scan(&out.ID, &out.Name, &out.Description, &out.Status, &out.Version)
		if err != nil {
			return err
		}
		return command.Audit(ctx, tx, scope, "catalog.product.archived")
	})
	return out, mapVersionError(err)
}

func ListProducts(ctx context.Context, tx pgx.Tx, scope platform.Scope) ([]Product, error) {
	page, err := ListProductsPage(ctx, tx, scope, pagination.Request{Limit: 100})
	return page.Items, err
}

func ListProductsPage(ctx context.Context, tx pgx.Tx, scope platform.Scope, request pagination.Request) (pagination.Page[Product], error) {
	page := pagination.Page[Product]{Items: make([]Product, 0)}
	if !validScope(tx, scope) {
		return page, command.ErrInvalid
	}
	binding := pagination.Binding{TenantID: scope.TenantID, StoreID: scope.StoreID, Collection: "products"}
	limit, after, err := pagination.Decode(request, binding, 1)
	if err != nil {
		return page, err
	}
	args := []any{scope.TenantID, scope.StoreID}
	if len(after) == 1 {
		args = append(args, after[0])
	}
	query := `SELECT id::text,name,description,status,version FROM catalog.products WHERE tenant_id=$1 AND store_id=$2` + keysetID(after, 3) + ` ORDER BY id LIMIT $` + strconv.Itoa(len(args)+1)
	args = append(args, limit+1)
	rows, err := tx.Query(ctx, query, args...)
	if err != nil {
		return page, mapError(err)
	}
	defer rows.Close()
	for rows.Next() {
		var p Product
		if err := rows.Scan(&p.ID, &p.Name, &p.Description, &p.Status, &p.Version); err != nil {
			return page, err
		}
		page.Items = append(page.Items, p)
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

func CreateSKU(ctx context.Context, tx pgx.Tx, scope platform.Scope, key string, in SKUInput) (out SKU, err error) {
	if !validSKUInput(in, false) {
		return out, command.ErrInvalid
	}
	err = command.Run(ctx, tx, scope, "catalog.sku.create", key, in, &out, func() error {
		if err := activeProduct(ctx, tx, scope, in.ProductID); err != nil {
			return err
		}
		currency, err := storeCurrency(ctx, tx, scope)
		if err != nil {
			return err
		}
		err = tx.QueryRow(ctx, `INSERT INTO catalog.skus(tenant_id,store_id,product_id,code,currency,price_minor,weight_grams,length_mm,width_mm,height_mm,origin_country,customs_name,hs_candidate)
			VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13)
			RETURNING id::text,product_id::text,code,status,currency,price_minor,version,weight_grams,length_mm,width_mm,height_mm,origin_country,customs_name,hs_candidate`,
			scope.TenantID, scope.StoreID, in.ProductID, in.Code, currency, in.PriceMinor, in.WeightGrams, in.LengthMM, in.WidthMM, in.HeightMM, in.OriginCountry, in.CustomsName, in.HSCandidate).Scan(skuFields(&out)...)
		if err != nil {
			return err
		}
		if err := appendPriceHistory(ctx, tx, scope, out); err != nil {
			return err
		}
		return command.Audit(ctx, tx, scope, "catalog.sku.created")
	})
	return out, mapError(err)
}

func UpdateSKU(ctx context.Context, tx pgx.Tx, scope platform.Scope, key, id string, in SKUInput) (out SKU, err error) {
	if !command.ValidID(id) || !validSKUInput(in, true) {
		return out, command.ErrInvalid
	}
	request := struct {
		ID string `json:"id"`
		SKUInput
	}{id, in}
	err = command.Run(ctx, tx, scope, "catalog.sku.update", key, request, &out, func() error {
		current, err := lockSKU(ctx, tx, scope, id)
		if err != nil {
			return err
		}
		if current.ProductID != in.ProductID || current.PriceMinor != in.PriceMinor || current.Version != in.ExpectedVersion {
			return command.ErrConflict
		}
		if current.Status != "active" {
			return command.ErrConflict
		}
		err = tx.QueryRow(ctx, `UPDATE catalog.skus SET code=$3,weight_grams=$4,length_mm=$5,width_mm=$6,height_mm=$7,origin_country=$8,customs_name=$9,hs_candidate=$10,version=version+1,updated_at=clock_timestamp()
			WHERE tenant_id=$1 AND store_id=$2 AND id=$11 AND version=$12
			RETURNING id::text,product_id::text,code,status,currency,price_minor,version,weight_grams,length_mm,width_mm,height_mm,origin_country,customs_name,hs_candidate`,
			scope.TenantID, scope.StoreID, in.Code, in.WeightGrams, in.LengthMM, in.WidthMM, in.HeightMM, in.OriginCountry, in.CustomsName, in.HSCandidate, id, in.ExpectedVersion).Scan(skuFields(&out)...)
		if err != nil {
			return err
		}
		return command.Audit(ctx, tx, scope, "catalog.sku.updated")
	})
	return out, mapVersionError(err)
}

func SetSKUPrice(ctx context.Context, tx pgx.Tx, scope platform.Scope, key, id string, in PriceInput) (out SKU, err error) {
	if !command.ValidID(id) || in.PriceMinor < 0 || in.PriceMinor > command.MaxMoney || in.ExpectedVersion < 1 {
		return out, command.ErrInvalid
	}
	request := struct {
		ID string `json:"id"`
		PriceInput
	}{id, in}
	err = command.Run(ctx, tx, scope, "catalog.sku.set_price", key, request, &out, func() error {
		current, err := lockSKU(ctx, tx, scope, id)
		if err != nil {
			return err
		}
		if current.Version != in.ExpectedVersion || current.Status != "active" {
			return command.ErrConflict
		}
		if err := tx.QueryRow(ctx, `UPDATE catalog.skus SET price_minor=$3,version=version+1,updated_at=clock_timestamp()
			WHERE tenant_id=$1 AND store_id=$2 AND id=$4 AND version=$5
			RETURNING id::text,product_id::text,code,status,currency,price_minor,version,weight_grams,length_mm,width_mm,height_mm,origin_country,customs_name,hs_candidate`, scope.TenantID, scope.StoreID, in.PriceMinor, id, in.ExpectedVersion).Scan(skuFields(&out)...); err != nil {
			return err
		}
		if err := appendPriceHistory(ctx, tx, scope, out); err != nil {
			return err
		}
		return command.Audit(ctx, tx, scope, "catalog.sku.price_changed")
	})
	return out, mapVersionError(err)
}

func ArchiveSKU(ctx context.Context, tx pgx.Tx, scope platform.Scope, key, id string, expectedVersion int64) (out SKU, err error) {
	if !command.ValidID(id) || expectedVersion < 1 {
		return out, command.ErrInvalid
	}
	request := struct {
		ID              string `json:"id"`
		ExpectedVersion int64  `json:"expected_version"`
	}{id, expectedVersion}
	err = command.Run(ctx, tx, scope, "catalog.sku.archive", key, request, &out, func() error {
		current, err := lockSKU(ctx, tx, scope, id)
		if err != nil {
			return err
		}
		if current.Version != expectedVersion {
			return command.ErrConflict
		}
		err = tx.QueryRow(ctx, `UPDATE catalog.skus SET status='archived',version=version+1,updated_at=clock_timestamp()
			WHERE tenant_id=$1 AND store_id=$2 AND id=$3 AND version=$4
			RETURNING id::text,product_id::text,code,status,currency,price_minor,version,weight_grams,length_mm,width_mm,height_mm,origin_country,customs_name,hs_candidate`, scope.TenantID, scope.StoreID, id, expectedVersion).Scan(skuFields(&out)...)
		if err != nil {
			return err
		}
		return command.Audit(ctx, tx, scope, "catalog.sku.archived")
	})
	return out, mapVersionError(err)
}

func ListSKUs(ctx context.Context, tx pgx.Tx, scope platform.Scope, productID string) ([]SKU, error) {
	page, err := ListSKUsPage(ctx, tx, scope, productID, pagination.Request{Limit: 100})
	return page.Items, err
}

func ListSKUsPage(ctx context.Context, tx pgx.Tx, scope platform.Scope, productID string, request pagination.Request) (pagination.Page[SKU], error) {
	page := pagination.Page[SKU]{Items: make([]SKU, 0)}
	if !validScope(tx, scope) || !command.ValidID(productID) {
		return page, command.ErrInvalid
	}
	// A read-only parent visibility check must not hold a row write lock until
	// the caller's transaction ends. Mutation helpers retain their own locks.
	var visible bool
	if err := tx.QueryRow(ctx, `SELECT true FROM catalog.products WHERE tenant_id=$1 AND store_id=$2 AND id=$3`, scope.TenantID, scope.StoreID, productID).Scan(&visible); err != nil {
		return page, mapError(err)
	}
	binding := pagination.Binding{TenantID: scope.TenantID, StoreID: scope.StoreID, Collection: "skus", ParentID: productID}
	limit, after, err := pagination.Decode(request, binding, 1)
	if err != nil {
		return page, err
	}
	query := `SELECT id::text,product_id::text,code,status,currency,price_minor,version,weight_grams,length_mm,width_mm,height_mm,origin_country,customs_name,hs_candidate FROM catalog.skus WHERE tenant_id=$1 AND store_id=$2 AND product_id=$3` + keysetID(after, 4) + ` ORDER BY id LIMIT $` + placeholder(4+len(after))
	args := []any{scope.TenantID, scope.StoreID, productID}
	if len(after) == 1 {
		args = append(args, after[0])
	}
	args = append(args, limit+1)
	rows, err := tx.Query(ctx, query, args...)
	if err != nil {
		return page, mapError(err)
	}
	defer rows.Close()
	for rows.Next() {
		var sku SKU
		if err := rows.Scan(skuFields(&sku)...); err != nil {
			return page, err
		}
		page.Items = append(page.Items, sku)
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

func keysetID(after []string, position int) string {
	if len(after) == 1 {
		return ` AND id>$` + strconv.Itoa(position) + `::uuid`
	}
	return ``
}
func placeholder(number int) string { return strconv.Itoa(number) }

func lockProduct(ctx context.Context, tx pgx.Tx, scope platform.Scope, id string) error {
	var ignored string
	err := tx.QueryRow(ctx, `SELECT id::text FROM catalog.products WHERE tenant_id=$1 AND store_id=$2 AND id=$3 FOR UPDATE`, scope.TenantID, scope.StoreID, id).Scan(&ignored)
	return mapError(err)
}

func activeProduct(ctx context.Context, tx pgx.Tx, scope platform.Scope, id string) error {
	var status string
	err := tx.QueryRow(ctx, `SELECT status FROM catalog.products WHERE tenant_id=$1 AND store_id=$2 AND id=$3 FOR UPDATE`, scope.TenantID, scope.StoreID, id).Scan(&status)
	if err != nil {
		return mapError(err)
	}
	if status != "active" {
		return command.ErrConflict
	}
	return nil
}

func lockSKU(ctx context.Context, tx pgx.Tx, scope platform.Scope, id string) (SKU, error) {
	var sku SKU
	// LOCK: product first prevents an archive racing a SKU mutation or inventory read.
	var productID string
	err := tx.QueryRow(ctx, `SELECT product_id::text FROM catalog.skus WHERE tenant_id=$1 AND store_id=$2 AND id=$3`, scope.TenantID, scope.StoreID, id).Scan(&productID)
	if err != nil {
		return sku, mapError(err)
	}
	if err = lockProduct(ctx, tx, scope, productID); err != nil {
		return sku, err
	}
	err = tx.QueryRow(ctx, `SELECT id::text,product_id::text,code,status,currency,price_minor,version,weight_grams,length_mm,width_mm,height_mm,origin_country,customs_name,hs_candidate FROM catalog.skus WHERE tenant_id=$1 AND store_id=$2 AND id=$3 FOR UPDATE`, scope.TenantID, scope.StoreID, id).Scan(skuFields(&sku)...)
	return sku, mapError(err)
}

func storeCurrency(ctx context.Context, tx pgx.Tx, scope platform.Scope) (string, error) {
	var c string
	err := tx.QueryRow(ctx, `SELECT currency FROM control.stores WHERE tenant_id=$1 AND id=$2`, scope.TenantID, scope.StoreID).Scan(&c)
	return c, mapError(err)
}

// appendPriceHistory keeps the create price (version 1) and later price changes
// in the same append-only stream, before the command result/audit can commit.
func appendPriceHistory(ctx context.Context, tx pgx.Tx, scope platform.Scope, sku SKU) error {
	_, err := tx.Exec(ctx, `INSERT INTO catalog.price_history(tenant_id,store_id,sku_id,version,price_minor,currency,principal_id)
		VALUES($1,$2,$3,$4,$5,$6,$7)`, scope.TenantID, scope.StoreID, sku.ID, sku.Version, sku.PriceMinor, sku.Currency, scope.PrincipalID)
	return err
}
func skuFields(s *SKU) []any {
	return []any{&s.ID, &s.ProductID, &s.Code, &s.Status, &s.Currency, &s.PriceMinor, &s.Version, &s.WeightGrams, &s.LengthMM, &s.WidthMM, &s.HeightMM, &s.OriginCountry, &s.CustomsName, &s.HSCandidate}
}
func validScope(tx pgx.Tx, s platform.Scope) bool {
	return tx != nil && command.ValidID(s.TenantID) && command.ValidID(s.StoreID) && command.ValidID(s.PrincipalID)
}
func validProductInput(in ProductInput, updating bool) bool {
	return utf8.RuneCountInString(in.Name) >= 1 && utf8.RuneCountInString(in.Name) <= 120 && utf8.RuneCountInString(in.Description) <= 8000 && (!updating || in.ExpectedVersion >= 1)
}
func validSKUInput(in SKUInput, updating bool) bool {
	return command.ValidID(in.ProductID) && skuCodePattern.MatchString(in.Code) && in.PriceMinor >= 0 && in.PriceMinor <= command.MaxMoney && in.WeightGrams >= 0 && in.WeightGrams <= command.MaxQuantity && in.LengthMM >= 0 && in.LengthMM <= 1_000_000 && in.WidthMM >= 0 && in.WidthMM <= 1_000_000 && in.HeightMM >= 0 && in.HeightMM <= 1_000_000 && (in.OriginCountry == "" || countryPattern.MatchString(in.OriginCountry)) && utf8.RuneCountInString(in.CustomsName) <= 240 && (in.HSCandidate == "" || hsPattern.MatchString(in.HSCandidate)) && (!updating || in.ExpectedVersion >= 1)
}
func mapVersionError(err error) error {
	if errors.Is(err, pgx.ErrNoRows) {
		return command.ErrConflict
	}
	return mapError(err)
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
