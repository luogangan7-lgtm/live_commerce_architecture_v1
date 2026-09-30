// Package pricing owns merchant market policy writes (markets and their currency) and the pure quote
// money calculation.
//
// It never reads a client-supplied amount, never touches stock or payment state, and never rounds
// money outside Calculate.
package pricing

import (
	"context"
	"errors"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"livecommerce/internal/command"
	"livecommerce/internal/platform"
)

var (
	marketCodePattern = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,39}$`)
	countryPattern    = regexp.MustCompile(`^[A-Z]{2}$`)
	currencyPattern   = regexp.MustCompile(`^[A-Z]{3}$`)
)

type Market struct {
	ID       string `json:"id"`
	Code     string `json:"code"`
	Name     string `json:"name"`
	Currency string `json:"currency"`
	Version  int64  `json:"version"`
	Active   bool   `json:"active"`
}

type MarketInput struct {
	Code     string `json:"code"`
	Name     string `json:"name"`
	Currency string `json:"currency"`
}

type Policy struct {
	MarketID        string `json:"market_id"`
	Country         string `json:"country"`
	Method          string `json:"method"`
	Currency        string `json:"currency"`
	ShippingMode    string `json:"shipping_mode"`
	TaxMode         string `json:"tax_mode"`
	TaxBasis        string `json:"tax_basis"`
	Version         int64  `json:"version"`
	ShippingMinor   int64  `json:"shipping_minor"`
	TaxRateBPS      int64  `json:"tax_rate_bps"`
	QuoteTTLSeconds int64  `json:"quote_ttl_seconds"`
	Enabled         bool   `json:"enabled"`
}

type PolicyInput struct {
	MarketID         string `json:"market_id"`
	Country          string `json:"country"`
	Method           string `json:"method"`
	Currency         string `json:"currency"`
	ShippingMode     string `json:"shipping_mode"`
	TaxMode          string `json:"tax_mode"`
	TaxBasis         string `json:"tax_basis"`
	ExpectedVersion  int64  `json:"expected_version"`
	ShippingMinor    *int64 `json:"shipping_minor"`
	TaxRateBPS       *int64 `json:"tax_rate_bps"`
	QuoteTTLSeconds  int64  `json:"quote_ttl_seconds"`
	Enabled          bool   `json:"enabled"`
	ConfigurationRef string `json:"configuration_ref"`
}

type AmountLine struct {
	UnitPriceMinor int64 `json:"unit_price_minor"`
	Quantity       int64 `json:"quantity"`
}

type LineAmount struct {
	SubtotalMinor int64 `json:"subtotal_minor"`
	DiscountMinor int64 `json:"discount_minor"`
	TaxMinor      int64 `json:"tax_minor"`
	TotalMinor    int64 `json:"total_minor"`
}

type Calculation struct {
	SubtotalMinor    int64        `json:"subtotal_minor"`
	DiscountMinor    int64        `json:"discount_minor"`
	ShippingMinor    int64        `json:"shipping_minor"`
	ShippingTaxMinor int64        `json:"shipping_tax_minor"`
	TaxMinor         int64        `json:"tax_minor"`
	TotalMinor       int64        `json:"total_minor"`
	Lines            []LineAmount `json:"lines"`
}

func CreateMarket(ctx context.Context, tx pgx.Tx, scope platform.Scope, key string, in MarketInput) (out Market, err error) {
	if !validScope(tx, scope) || !marketCodePattern.MatchString(in.Code) || !printable(in.Name, 120) || !currencyPattern.MatchString(in.Currency) {
		return out, command.ErrInvalid
	}
	request := struct {
		PrincipalID string `json:"principal_id"`
		MarketInput
	}{scope.PrincipalID, in}
	err = command.Run(ctx, tx, scope, "pricing.market.create", key, request, &out, func() error {
		var storeCurrency string
		if err := tx.QueryRow(ctx, `SELECT currency FROM control.stores WHERE tenant_id=$1 AND id=$2`, scope.TenantID, scope.StoreID).Scan(&storeCurrency); err != nil {
			return mapError(err)
		}
		if storeCurrency != in.Currency {
			return command.ErrInvalid
		}
		err := tx.QueryRow(ctx, `INSERT INTO pricing.markets(tenant_id,store_id,code,name,currency,principal_id)
			VALUES($1,$2,$3,$4,$5,$6) RETURNING id::text,code,name,currency,version,active`,
			scope.TenantID, scope.StoreID, in.Code, in.Name, in.Currency, scope.PrincipalID).
			Scan(&out.ID, &out.Code, &out.Name, &out.Currency, &out.Version, &out.Active)
		if err != nil {
			return mapError(err)
		}
		return command.Audit(ctx, tx, scope, "pricing.market.created")
	})
	return out, mapError(err)
}

func SetMarketActive(ctx context.Context, tx pgx.Tx, scope platform.Scope, key, id string, expectedVersion int64, active bool) (out Market, err error) {
	if !validScope(tx, scope) || !command.ValidID(id) || expectedVersion < 1 {
		return out, command.ErrInvalid
	}
	request := struct {
		PrincipalID     string `json:"principal_id"`
		ID              string `json:"id"`
		ExpectedVersion int64  `json:"expected_version"`
		Active          bool   `json:"active"`
	}{scope.PrincipalID, id, expectedVersion, active}
	err = command.Run(ctx, tx, scope, "pricing.market.active", key, request, &out, func() error {
		var current Market
		err := tx.QueryRow(ctx, `SELECT id::text,code,name,currency,version,active FROM pricing.markets
			WHERE tenant_id=$1 AND store_id=$2 AND id=$3 FOR UPDATE`, scope.TenantID, scope.StoreID, id).
			Scan(&current.ID, &current.Code, &current.Name, &current.Currency, &current.Version, &current.Active)
		if err != nil {
			return mapError(err)
		}
		if current.Version != expectedVersion {
			return command.ErrConflict
		}
		err = tx.QueryRow(ctx, `UPDATE pricing.markets SET active=$4,version=version+1
			WHERE tenant_id=$1 AND store_id=$2 AND id=$3 AND version=$5
			RETURNING id::text,code,name,currency,version,active`, scope.TenantID, scope.StoreID, id, active, expectedVersion).
			Scan(&out.ID, &out.Code, &out.Name, &out.Currency, &out.Version, &out.Active)
		if err != nil {
			return mapVersionError(err)
		}
		return command.Audit(ctx, tx, scope, "pricing.market.active_changed")
	})
	return out, mapError(err)
}

func LockMarket(ctx context.Context, tx pgx.Tx, tenantID, storeID, id string) (out Market, err error) {
	if tx == nil || !command.ValidID(tenantID) || !command.ValidID(storeID) || !command.ValidID(id) {
		return out, command.ErrInvalid
	}
	err = tx.QueryRow(ctx, `SELECT id::text,code,name,currency,version,active FROM pricing.markets
		WHERE tenant_id=$1 AND store_id=$2 AND id=$3 FOR SHARE`, tenantID, storeID, id).
		Scan(&out.ID, &out.Code, &out.Name, &out.Currency, &out.Version, &out.Active)
	return out, mapError(err)
}

func SetPolicy(ctx context.Context, tx pgx.Tx, scope platform.Scope, key string, in PolicyInput) (out Policy, err error) {
	if !validScope(tx, scope) || !validPolicyInput(in) {
		return out, command.ErrInvalid
	}
	request := struct {
		PrincipalID string `json:"principal_id"`
		PolicyInput
	}{scope.PrincipalID, in}
	err = command.Run(ctx, tx, scope, "pricing.policy.set", key, request, &out, func() error {
		market, err := LockMarket(ctx, tx, scope.TenantID, scope.StoreID, in.MarketID)
		if err != nil {
			return err
		}
		if market.Currency != in.Currency {
			return command.ErrInvalid
		}
		if err = advisoryLock(ctx, tx, "pricing.policy|"+scope.TenantID+"|"+scope.StoreID+"|"+in.MarketID+"|"+in.Country+"|"+in.Method); err != nil {
			return err
		}
		var current int64
		err = tx.QueryRow(ctx, `SELECT current_version FROM pricing.policy_heads
			WHERE tenant_id=$1 AND store_id=$2 AND market_id=$3 AND country=$4 AND method=$5 FOR UPDATE`,
			scope.TenantID, scope.StoreID, in.MarketID, in.Country, in.Method).Scan(&current)
		switch {
		case errors.Is(err, pgx.ErrNoRows) && in.ExpectedVersion == 0:
			current = 0
		case err != nil:
			return mapError(err)
		case current != in.ExpectedVersion:
			return command.ErrConflict
		}
		out = Policy{MarketID: in.MarketID, Country: in.Country, Method: in.Method, Currency: in.Currency,
			ShippingMode: in.ShippingMode, TaxMode: in.TaxMode, TaxBasis: in.TaxBasis, Version: current + 1,
			ShippingMinor: *in.ShippingMinor, TaxRateBPS: *in.TaxRateBPS, QuoteTTLSeconds: in.QuoteTTLSeconds, Enabled: in.Enabled}
		_, err = tx.Exec(ctx, `INSERT INTO pricing.policy_versions(tenant_id,store_id,market_id,country,method,version,currency,
			shipping_mode,shipping_minor,tax_mode,tax_basis,tax_rate_bps,quote_ttl_seconds,enabled,configuration_ref,principal_id)
			VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16)`, scope.TenantID, scope.StoreID,
			out.MarketID, out.Country, out.Method, out.Version, out.Currency, out.ShippingMode, out.ShippingMinor,
			out.TaxMode, out.TaxBasis, out.TaxRateBPS, out.QuoteTTLSeconds, out.Enabled, in.ConfigurationRef, scope.PrincipalID)
		if err != nil {
			return mapError(err)
		}
		if current == 0 {
			_, err = tx.Exec(ctx, `INSERT INTO pricing.policy_heads(tenant_id,store_id,market_id,country,method,current_version)
				VALUES($1,$2,$3,$4,$5,$6)`, scope.TenantID, scope.StoreID, in.MarketID, in.Country, in.Method, out.Version)
		} else {
			var tag pgconn.CommandTag
			tag, err = tx.Exec(ctx, `UPDATE pricing.policy_heads SET current_version=$6
				WHERE tenant_id=$1 AND store_id=$2 AND market_id=$3 AND country=$4 AND method=$5 AND current_version=$7`,
				scope.TenantID, scope.StoreID, in.MarketID, in.Country, in.Method, out.Version, current)
			if err == nil && tag.RowsAffected() != 1 {
				return command.ErrConflict
			}
		}
		if err != nil {
			return mapError(err)
		}
		return command.Audit(ctx, tx, scope, "pricing.policy.set")
	})
	return out, mapError(err)
}

func LockCurrent(ctx context.Context, tx pgx.Tx, tenantID, storeID, marketID, country, method string) (out Policy, err error) {
	if tx == nil || !command.ValidID(tenantID) || !command.ValidID(storeID) || !command.ValidID(marketID) ||
		!countryPattern.MatchString(country) || !ValidMethod(method) {
		return out, command.ErrInvalid
	}
	var currentVersion int64
	err = tx.QueryRow(ctx, `SELECT current_version FROM pricing.policy_heads
		WHERE tenant_id=$1 AND store_id=$2 AND market_id=$3 AND country=$4 AND method=$5 FOR SHARE`,
		tenantID, storeID, marketID, country, method).Scan(&currentVersion)
	if err != nil {
		return out, mapError(err)
	}
	err = tx.QueryRow(ctx, `SELECT market_id::text,country,method,currency,shipping_mode,tax_mode,tax_basis,
		version,shipping_minor,tax_rate_bps,quote_ttl_seconds,enabled FROM pricing.policy_versions
		WHERE tenant_id=$1 AND store_id=$2 AND market_id=$3 AND country=$4 AND method=$5 AND version=$6`,
		tenantID, storeID, marketID, country, method, currentVersion).Scan(
		&out.MarketID, &out.Country, &out.Method, &out.Currency, &out.ShippingMode, &out.TaxMode, &out.TaxBasis,
		&out.Version, &out.ShippingMinor, &out.TaxRateBPS, &out.QuoteTTLSeconds, &out.Enabled)
	if err != nil {
		return out, mapError(err)
	}
	if !out.Enabled {
		return Policy{}, command.ErrNotFound
	}
	return out, nil
}

func Calculate(policy Policy, lines []AmountLine) (Calculation, error) {
	result := Calculation{ShippingMinor: policy.ShippingMinor, Lines: make([]LineAmount, 0, len(lines))}
	if !validPolicy(policy) || len(lines) == 0 || len(lines) > 50 {
		return Calculation{}, command.ErrInvalid
	}
	for _, line := range lines {
		subtotal, err := command.CheckMoney(line.UnitPriceMinor, line.Quantity)
		if err != nil {
			return Calculation{}, err
		}
		tax := taxMinor(subtotal, policy.TaxRateBPS, policy.TaxMode)
		lineTotal := subtotal
		if policy.TaxMode == "exclusive" {
			lineTotal, err = addMoney(subtotal, tax)
			if err != nil {
				return Calculation{}, err
			}
		}
		result.SubtotalMinor, err = addMoney(result.SubtotalMinor, subtotal)
		if err != nil {
			return Calculation{}, err
		}
		result.TaxMinor, err = addMoney(result.TaxMinor, tax)
		if err != nil {
			return Calculation{}, err
		}
		result.Lines = append(result.Lines, LineAmount{SubtotalMinor: subtotal, TaxMinor: tax, TotalMinor: lineTotal})
	}
	if policy.TaxBasis == "goods_and_shipping" {
		result.ShippingTaxMinor = taxMinor(policy.ShippingMinor, policy.TaxRateBPS, policy.TaxMode)
	}
	var err error
	result.TaxMinor, err = addMoney(result.TaxMinor, result.ShippingTaxMinor)
	if err != nil {
		return Calculation{}, err
	}
	total, err := addMoney(result.SubtotalMinor, result.ShippingMinor)
	if err != nil {
		return Calculation{}, err
	}
	if policy.TaxMode == "exclusive" {
		if total, err = addMoney(total, result.TaxMinor); err != nil {
			return Calculation{}, err
		}
	}
	result.TotalMinor = total
	return result, nil
}

func validPolicyInput(in PolicyInput) bool {
	return command.ValidID(in.MarketID) && in.ExpectedVersion >= 0 && in.ShippingMinor != nil && in.TaxRateBPS != nil &&
		currencyPattern.MatchString(in.Currency) && countryPattern.MatchString(in.Country) && ValidMethod(in.Method) &&
		in.ShippingMode == "country_flat" && validTax(in.TaxMode, in.TaxBasis, *in.TaxRateBPS) &&
		*in.ShippingMinor >= 0 && *in.ShippingMinor <= command.MaxMoney && in.QuoteTTLSeconds >= 60 && in.QuoteTTLSeconds <= 1800 &&
		printable(in.ConfigurationRef, 240)
}

func validPolicy(p Policy) bool {
	return command.ValidID(p.MarketID) && p.Version > 0 && p.Enabled && currencyPattern.MatchString(p.Currency) &&
		countryPattern.MatchString(p.Country) && ValidMethod(p.Method) && p.ShippingMode == "country_flat" &&
		p.ShippingMinor >= 0 && p.ShippingMinor <= command.MaxMoney && validTax(p.TaxMode, p.TaxBasis, p.TaxRateBPS) &&
		p.QuoteTTLSeconds >= 60 && p.QuoteTTLSeconds <= 1800
}

func validTax(mode, basis string, bps int64) bool {
	return (basis == "goods" || basis == "goods_and_shipping") && bps >= 0 && bps <= 10000 &&
		(mode == "exclusive" || mode == "inclusive" || mode == "none") && (mode != "none" || bps == 0)
}

// ValidMethod reports whether method is a supported legacy or delivery-service key.
func ValidMethod(method string) bool {
	if method == "home" || method == "cvs_711" || method == "cvs_familymart" {
		return true
	}
	code, ok := strings.CutPrefix(method, "delivery:")
	return ok && marketCodePattern.MatchString(code)
}

// DeliveryMethod returns the reserved pricing key for one delivery service.
func DeliveryMethod(code string) (string, error) {
	if !marketCodePattern.MatchString(code) {
		return "", command.ErrInvalid
	}
	return "delivery:" + code, nil
}

func printable(value string, max int) bool {
	length := utf8.RuneCountInString(value)
	if length < 1 || length > max || !utf8.ValidString(value) {
		return false
	}
	for _, r := range value {
		if !unicode.IsPrint(r) {
			return false
		}
	}
	return true
}

func validScope(tx pgx.Tx, scope platform.Scope) bool {
	return tx != nil && command.ValidID(scope.TenantID) && command.ValidID(scope.StoreID) && command.ValidID(scope.PrincipalID)
}

func advisoryLock(ctx context.Context, tx pgx.Tx, key string) error {
	var lockTimeout string
	if err := tx.QueryRow(ctx, `SHOW lock_timeout`).Scan(&lockTimeout); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `SELECT set_config('lock_timeout','0',true)`); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, key); err != nil {
		return err
	}
	_, err := tx.Exec(ctx, `SELECT set_config('lock_timeout',$1,true)`, lockTimeout)
	return err
}

func taxMinor(base, bps int64, mode string) int64 {
	if mode == "none" || bps == 0 {
		return 0
	}
	if mode == "inclusive" {
		net := (base*10000 + (10000+bps)/2) / (10000 + bps)
		return base - net
	}
	return (base*bps + 5000) / 10000
}

func addMoney(a, b int64) (int64, error) {
	if a < 0 || b < 0 || a > command.MaxMoney-b {
		return 0, command.ErrInvalid
	}
	return a + b, nil
}

func mapVersionError(err error) error {
	if errors.Is(err, pgx.ErrNoRows) {
		return command.ErrConflict
	}
	return mapError(err)
}

func mapError(err error) error {
	if err == nil || errors.Is(err, command.ErrInvalid) || errors.Is(err, command.ErrConflict) || errors.Is(err, command.ErrNotFound) {
		return err
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return command.ErrNotFound
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		switch pgErr.Code {
		case "23505", "40001":
			return command.ErrConflict
		case "23503", "23514", "22P02":
			return command.ErrInvalid
		}
	}
	return err
}
