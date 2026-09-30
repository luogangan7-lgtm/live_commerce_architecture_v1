// Package fulfillment owns merchant delivery-service configuration revisions, per-market delivery
// allocation (which warehouses serve a country), pickup attestation (buyer-scoped read and lock) and the
// merchant side of Taiwan convenience-store shipping (taiwan-cvs-logistics-v1, cvs*.go): the ECPay logistics
// connection, chain and pay-at-pickup settings, one label request per order, shipment read, print form,
// abandon, collection record, pay-at-pickup cancel/restock and the two public provider hooks (map return,
// status). Every CVS write is one SECURITY DEFINER function of migrations/0073.
//
// It never commits the transaction of a caller that passes one in (the CVS methods open their own through
// platform.WithScope), never speaks the ECPay wire format (internal/integrations/shipping/ecpay does, with
// TLS to logistics(-stage).ecpay.com.tw only), never runs the dispatcher route, never writes a stock or
// ledger row itself (inventory.release_pay_at_pickup is the one audited writer, called through SQL), never
// holds pay-at-pickup money, and never returns or logs a credential, recipient field or trade number. Manual
// shipment recording lives in internal/merchantorders.
package fulfillment

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
	"livecommerce/internal/pricing"
)

const (
	managePermission = "integration:manage"
	readPermission   = "integration:read"
)

var (
	codePattern    = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,39}$`)
	countryPattern = regexp.MustCompile(`^[A-Z]{2}$`)
)

type ServiceInput struct {
	MarketID        string `json:"market_id"`
	Country         string `json:"country"`
	Code            string `json:"code"`
	ExpectedVersion int64  `json:"expected_version"`
	PolicyVersion   int64  `json:"policy_version"`
	NameHans        string `json:"name_hans"`
	NameHant        string `json:"name_hant"`
	NameEN          string `json:"name_en"`
	DeliveryKind    string `json:"delivery_kind"`
	Mode            string `json:"mode"`
	Enabled         bool   `json:"enabled"`
	Visible         bool   `json:"visible"`
	SortOrder       int    `json:"sort_order"`
	BindingID       string `json:"binding_id,omitempty"`
	BindingVersion  int64  `json:"binding_version,omitempty"`
}

type Service struct {
	MarketID       string `json:"market_id"`
	Country        string `json:"country"`
	Code           string `json:"code"`
	Version        int64  `json:"version"`
	PolicyMethod   string `json:"policy_method"`
	PolicyVersion  int64  `json:"policy_version"`
	Currency       string `json:"currency"`
	NameHans       string `json:"name_hans"`
	NameHant       string `json:"name_hant"`
	NameEN         string `json:"name_en"`
	DeliveryKind   string `json:"delivery_kind"`
	Mode           string `json:"mode"`
	Enabled        bool   `json:"enabled"`
	Visible        bool   `json:"visible"`
	SortOrder      int    `json:"sort_order"`
	BindingID      string `json:"binding_id,omitempty"`
	BindingVersion int64  `json:"binding_version,omitempty"`
}

func SetService(ctx context.Context, tx pgx.Tx, scope platform.Scope, token, key string, in ServiceInput) (out Service, err error) {
	if !validServiceInput(in) {
		return out, command.ErrInvalid
	}
	if err = authorize(ctx, tx, scope, token, managePermission); err != nil {
		return out, err
	}
	request := struct {
		PrincipalID string `json:"principal_id"`
		ServiceInput
	}{scope.PrincipalID, in}
	err = command.Run(ctx, tx, scope, "fulfillment.service.set", key, request, &out, func() error {
		// Read before policy validation only to recognize the exact emergency-stop
		// transition. This is not a lock; the snapshot is checked again after the
		// policy -> service lock order has been acquired.
		before, exists, readErr := readCurrentService(ctx, tx, scope, in.MarketID, in.Country, in.Code)
		if readErr != nil {
			return readErr
		}
		stopOnly := exists && isStopOnly(before, in)

		market, lockErr := pricing.LockMarket(ctx, tx, scope.TenantID, scope.StoreID, in.MarketID)
		if lockErr != nil {
			return lockErr
		}
		method, _ := pricing.DeliveryMethod(in.Code)
		if stopOnly {
			if lockErr = lockPolicyHead(ctx, tx, scope, in, method); lockErr != nil {
				return lockErr
			}
		} else {
			if !market.Active {
				return command.ErrConflict
			}
			policy, policyErr := pricing.LockCurrent(ctx, tx, scope.TenantID, scope.StoreID, in.MarketID, in.Country, method)
			if errors.Is(policyErr, command.ErrNotFound) {
				return command.ErrConflict
			}
			if policyErr != nil {
				return policyErr
			}
			if policy.Version != in.PolicyVersion || policy.Currency != market.Currency {
				return command.ErrConflict
			}
		}

		if lockErr = advisoryLock(ctx, tx, serviceLockKey(scope, in)); lockErr != nil {
			return lockErr
		}
		current, currentExists, currentErr := lockCurrentService(ctx, tx, scope, in)
		if currentErr != nil {
			return currentErr
		}
		if currentExists != (in.ExpectedVersion > 0) || (currentExists && current.Version != in.ExpectedVersion) {
			return command.ErrConflict
		}
		if stopOnly && (!currentExists || !isStopOnly(current, in)) {
			return command.ErrConflict
		}

		if in.BindingID != "" {
			var bindingVersion int64
			if lockErr = tx.QueryRow(ctx, `SELECT semantic_version FROM integration.bindings
				WHERE tenant_id=$1 AND store_id=$2 AND id=$3 FOR SHARE`, scope.TenantID, scope.StoreID, in.BindingID).
				Scan(&bindingVersion); lockErr != nil {
				return mapError(lockErr)
			}
			if !stopOnly && bindingVersion != in.BindingVersion {
				return command.ErrConflict
			}
		}

		version := int64(1)
		if currentExists {
			version = current.Version + 1
		}
		out = Service{
			MarketID: in.MarketID, Country: in.Country, Code: in.Code, Version: version,
			PolicyMethod: method, PolicyVersion: in.PolicyVersion, Currency: market.Currency,
			NameHans: in.NameHans, NameHant: in.NameHant, NameEN: in.NameEN,
			DeliveryKind: in.DeliveryKind, Mode: in.Mode, Enabled: in.Enabled, Visible: in.Visible,
			SortOrder: in.SortOrder, BindingID: in.BindingID, BindingVersion: in.BindingVersion,
		}
		if _, insertErr := tx.Exec(ctx, `INSERT INTO fulfillment.service_versions(
			tenant_id,store_id,market_id,country,code,version,policy_version,currency,
			name_hans,name_hant,name_en,delivery_kind,mode,enabled,visible,sort_order,
			binding_id,binding_version,principal_id)
			VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,
			nullif($17::text,'')::uuid,nullif($18::bigint,0),$19)`,
			scope.TenantID, scope.StoreID, out.MarketID, out.Country, out.Code, out.Version,
			out.PolicyVersion, out.Currency, out.NameHans, out.NameHant, out.NameEN, out.DeliveryKind,
			out.Mode, out.Enabled, out.Visible, out.SortOrder, out.BindingID, out.BindingVersion, scope.PrincipalID); insertErr != nil {
			return mapError(insertErr)
		}
		if currentExists {
			tag, updateErr := tx.Exec(ctx, `UPDATE fulfillment.service_heads SET current_version=$6
				WHERE tenant_id=$1 AND store_id=$2 AND market_id=$3 AND country=$4 AND code=$5 AND current_version=$7`,
				scope.TenantID, scope.StoreID, in.MarketID, in.Country, in.Code, out.Version, current.Version)
			if updateErr != nil {
				return mapError(updateErr)
			}
			if tag.RowsAffected() != 1 {
				return command.ErrConflict
			}
		} else if _, insertErr := tx.Exec(ctx, `INSERT INTO fulfillment.service_heads(
			tenant_id,store_id,market_id,country,code,current_version) VALUES($1,$2,$3,$4,$5,$6)`,
			scope.TenantID, scope.StoreID, in.MarketID, in.Country, in.Code, out.Version); insertErr != nil {
			return mapError(insertErr)
		}
		return command.Audit(ctx, tx, scope, "fulfillment.service.set")
	})
	if err != nil {
		return Service{}, mapError(err)
	}
	// Both a new command and a saved replay may wait on locks. Re-resolve the
	// grant before returning so the caller rolls back if it was revoked meanwhile.
	if err = authorize(ctx, tx, scope, token, managePermission); err != nil {
		return Service{}, err
	}
	return out, nil
}

func GetService(ctx context.Context, tx pgx.Tx, scope platform.Scope, token, marketID, country, code string) (out Service, err error) {
	if !command.ValidID(marketID) || !countryPattern.MatchString(country) || !codePattern.MatchString(code) {
		return out, command.ErrInvalid
	}
	if err = authorize(ctx, tx, scope, token, readPermission); err != nil {
		return out, err
	}
	out, exists, err := readCurrentService(ctx, tx, scope, marketID, country, code)
	if err != nil {
		return Service{}, err
	}
	if !exists {
		return Service{}, command.ErrNotFound
	}
	if err = authorize(ctx, tx, scope, token, readPermission); err != nil {
		return Service{}, err
	}
	return out, nil
}

func readCurrentService(ctx context.Context, tx pgx.Tx, scope platform.Scope, marketID, country, code string) (Service, bool, error) {
	var out Service
	err := tx.QueryRow(ctx, `SELECT v.market_id::text,v.country,v.code,v.version,v.policy_method,v.policy_version,v.currency,
		v.name_hans,v.name_hant,v.name_en,v.delivery_kind,v.mode,v.enabled,v.visible,v.sort_order,
		coalesce(v.binding_id::text,''),coalesce(v.binding_version,0)
		FROM fulfillment.service_heads h JOIN fulfillment.service_versions v
		ON (v.tenant_id,v.store_id,v.market_id,v.country,v.code,v.version)=
		(h.tenant_id,h.store_id,h.market_id,h.country,h.code,h.current_version)
		WHERE h.tenant_id=$1 AND h.store_id=$2 AND h.market_id=$3 AND h.country=$4 AND h.code=$5`,
		scope.TenantID, scope.StoreID, marketID, country, code).Scan(
		&out.MarketID, &out.Country, &out.Code, &out.Version, &out.PolicyMethod, &out.PolicyVersion, &out.Currency,
		&out.NameHans, &out.NameHant, &out.NameEN, &out.DeliveryKind, &out.Mode, &out.Enabled, &out.Visible,
		&out.SortOrder, &out.BindingID, &out.BindingVersion)
	if errors.Is(err, pgx.ErrNoRows) {
		return Service{}, false, nil
	}
	return out, err == nil, mapError(err)
}

func lockCurrentService(ctx context.Context, tx pgx.Tx, scope platform.Scope, in ServiceInput) (Service, bool, error) {
	var version int64
	err := tx.QueryRow(ctx, `SELECT current_version FROM fulfillment.service_heads
		WHERE tenant_id=$1 AND store_id=$2 AND market_id=$3 AND country=$4 AND code=$5 FOR UPDATE`,
		scope.TenantID, scope.StoreID, in.MarketID, in.Country, in.Code).Scan(&version)
	if errors.Is(err, pgx.ErrNoRows) {
		return Service{}, false, nil
	}
	if err != nil {
		return Service{}, false, mapError(err)
	}
	var out Service
	err = tx.QueryRow(ctx, `SELECT market_id::text,country,code,version,policy_method,policy_version,currency,
		name_hans,name_hant,name_en,delivery_kind,mode,enabled,visible,sort_order,
		coalesce(binding_id::text,''),coalesce(binding_version,0)
		FROM fulfillment.service_versions
		WHERE tenant_id=$1 AND store_id=$2 AND market_id=$3 AND country=$4 AND code=$5 AND version=$6`,
		scope.TenantID, scope.StoreID, in.MarketID, in.Country, in.Code, version).Scan(
		&out.MarketID, &out.Country, &out.Code, &out.Version, &out.PolicyMethod, &out.PolicyVersion, &out.Currency,
		&out.NameHans, &out.NameHant, &out.NameEN, &out.DeliveryKind, &out.Mode, &out.Enabled, &out.Visible,
		&out.SortOrder, &out.BindingID, &out.BindingVersion)
	return out, err == nil, mapError(err)
}

func lockPolicyHead(ctx context.Context, tx pgx.Tx, scope platform.Scope, in ServiceInput, method string) error {
	var currentVersion int64
	err := tx.QueryRow(ctx, `SELECT current_version FROM pricing.policy_heads
		WHERE tenant_id=$1 AND store_id=$2 AND market_id=$3 AND country=$4 AND method=$5 FOR SHARE`,
		scope.TenantID, scope.StoreID, in.MarketID, in.Country, method).Scan(&currentVersion)
	return mapError(err)
}

func isStopOnly(old Service, in ServiceInput) bool {
	visibilityOK := old.Visible == in.Visible || (old.Visible && !in.Visible)
	return old.Enabled && !in.Enabled && visibilityOK && in.ExpectedVersion == old.Version &&
		in.PolicyVersion == old.PolicyVersion && in.NameHans == old.NameHans && in.NameHant == old.NameHant &&
		in.NameEN == old.NameEN && in.DeliveryKind == old.DeliveryKind && in.Mode == old.Mode &&
		in.SortOrder == old.SortOrder && in.BindingID == old.BindingID && in.BindingVersion == old.BindingVersion
}

func validServiceInput(in ServiceInput) bool {
	if !command.ValidID(in.MarketID) || !countryPattern.MatchString(in.Country) || !codePattern.MatchString(in.Code) ||
		in.ExpectedVersion < 0 || in.PolicyVersion < 1 || !printable(in.NameHans, 120) ||
		!printable(in.NameHant, 120) || !printable(in.NameEN, 120) || in.SortOrder < 0 || in.SortOrder > 1000 {
		return false
	}
	if in.DeliveryKind != "home" && !isCVSKind(in.DeliveryKind) {
		return false
	}
	if in.DeliveryKind != "home" && in.Country != "TW" {
		return false
	}
	if in.Mode != "MANUAL" && in.Mode != "API" {
		return false
	}
	bindingAbsent := in.BindingID == "" && in.BindingVersion == 0
	bindingPresent := command.ValidID(in.BindingID) && in.BindingVersion > 0
	if !bindingAbsent && !bindingPresent {
		return false
	}
	// R-2 (taiwan-cvs-logistics-v1 §4.1): an API service can be enabled only with a binding (and only for a CVS kind); the SQL trigger
	// guard_api_service_binding then requires it to belong to the store's qualified, enabled ecpay_logistics profile.
	return (in.Mode != "MANUAL" || bindingAbsent) && (in.Mode != "API" || !in.Enabled || (bindingPresent && isCVSKind(in.DeliveryKind)))
}

func printable(value string, max int) bool {
	if !utf8.ValidString(value) || strings.TrimSpace(value) == "" {
		return false
	}
	length := utf8.RuneCountInString(value)
	if length < 1 || length > max {
		return false
	}
	for _, r := range value {
		if !unicode.IsPrint(r) {
			return false
		}
	}
	return true
}

func authorize(ctx context.Context, tx pgx.Tx, scope platform.Scope, token, permission string) error {
	if err := validateTransactionScope(ctx, tx, scope); err != nil {
		return err
	}
	return platform.RequirePermission(ctx, tx, scope, token, permission)
}

func validateTransactionScope(ctx context.Context, tx pgx.Tx, scope platform.Scope) error {
	if tx == nil || !command.ValidID(scope.TenantID) || !command.ValidID(scope.StoreID) ||
		!command.ValidID(scope.PrincipalID) || scope.Revision < 1 {
		return command.ErrInvalid
	}
	var tenantID, storeID, principalID string
	err := tx.QueryRow(ctx, `SELECT coalesce(current_setting('app.tenant_id',true),''),
		coalesce(current_setting('app.store_id',true),''),coalesce(current_setting('app.principal_id',true),'')`).
		Scan(&tenantID, &storeID, &principalID)
	if err != nil {
		return err
	}
	if tenantID != scope.TenantID || storeID != scope.StoreID || principalID != scope.PrincipalID {
		return command.ErrInvalid
	}
	return nil
}

func serviceLockKey(scope platform.Scope, in ServiceInput) string {
	return "fulfillment.service|" + scope.TenantID + "|" + scope.StoreID + "|" + in.MarketID + "|" + in.Country + "|" + in.Code
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
