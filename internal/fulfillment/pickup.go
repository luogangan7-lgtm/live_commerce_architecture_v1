package fulfillment

import (
	"context"
	"errors"
	"math"
	"regexp"
	"time"

	"github.com/jackc/pgx/v5"

	"livecommerce/internal/buyer"
	"livecommerce/internal/command"
	"livecommerce/internal/platform"
)

var (
	pickupNamespacePattern = regexp.MustCompile(`^[a-z][a-z0-9_.:-]{0,63}$`)
	pickupCodePattern      = regexp.MustCompile(`^[A-Za-z0-9_-]{1,32}$`)
)

type PickupInput struct {
	Kind            string `json:"kind"`
	Namespace       string `json:"namespace"`
	Code            string `json:"code"`
	ExpectedVersion int64  `json:"expected_version"`
	Name            string `json:"name"`
	Address         string `json:"address"`
	EvidenceRef     string `json:"evidence_ref"`
	TTLSeconds      int64  `json:"ttl_seconds"`
}

// Pickup is the public projection of one immutable, merchant-attested source.
// The actor and evidence reference are deliberately absent.
type Pickup struct {
	ID               string    `json:"id"`
	Kind             string    `json:"kind"`
	Namespace        string    `json:"namespace"`
	Code             string    `json:"code"`
	Version          int64     `json:"version"`
	Country          string    `json:"country"`
	Name             string    `json:"name"`
	Address          string    `json:"address"`
	VerificationKind string    `json:"verification_kind"`
	AttestedAt       time.Time `json:"attested_at"`
	ValidUntil       time.Time `json:"valid_until"`
}

func AttestPickup(ctx context.Context, tx pgx.Tx, scope platform.Scope, token, key string, in PickupInput) (out Pickup, err error) {
	if !validPickupInput(in) {
		return out, command.ErrInvalid
	}
	if err = authorize(ctx, tx, scope, token, managePermission); err != nil {
		return out, err
	}
	request := struct {
		PrincipalID string `json:"principal_id"`
		PickupInput
	}{scope.PrincipalID, in}
	err = command.Run(ctx, tx, scope, "fulfillment.pickup.attest", key, request, &out, func() error {
		if lockErr := advisoryLock(ctx, tx, pickupLockKey(scope.TenantID, scope.StoreID, in.Kind, in.Namespace, in.Code)); lockErr != nil {
			return lockErr
		}
		var currentVersion int64
		lockErr := tx.QueryRow(ctx, `SELECT current_version FROM fulfillment.pickup_heads
			WHERE tenant_id=$1 AND store_id=$2 AND kind=$3 AND namespace=$4 AND code=$5 FOR UPDATE`,
			scope.TenantID, scope.StoreID, in.Kind, in.Namespace, in.Code).Scan(&currentVersion)
		exists := lockErr == nil
		if lockErr != nil && !errors.Is(lockErr, pgx.ErrNoRows) {
			return mapError(lockErr)
		}
		if exists != (in.ExpectedVersion > 0) || (exists && currentVersion != in.ExpectedVersion) {
			return command.ErrConflict
		}
		version := int64(1)
		if exists {
			version = currentVersion + 1
		}
		insertErr := tx.QueryRow(ctx, `WITH stamp AS MATERIALIZED (SELECT clock_timestamp() AS observed)
			INSERT INTO fulfillment.pickup_versions(
				tenant_id,store_id,kind,namespace,code,version,country,name,address,
				verification_kind,evidence_ref,principal_id,attested_at,valid_until)
			SELECT $1,$2,$3,$4,$5,$6,'TW',$7,$8,'MANUAL_ATTESTED',$9,$10,
				stamp.observed,stamp.observed+($11::bigint * interval '1 second') FROM stamp
			RETURNING id::text,kind,namespace,code,version,country,name,address,
				verification_kind,attested_at,valid_until`,
			scope.TenantID, scope.StoreID, in.Kind, in.Namespace, in.Code, version,
			in.Name, in.Address, in.EvidenceRef, scope.PrincipalID, in.TTLSeconds).Scan(
			&out.ID, &out.Kind, &out.Namespace, &out.Code, &out.Version, &out.Country,
			&out.Name, &out.Address, &out.VerificationKind, &out.AttestedAt, &out.ValidUntil)
		if insertErr != nil {
			return mapError(insertErr)
		}
		if exists {
			tag, updateErr := tx.Exec(ctx, `UPDATE fulfillment.pickup_heads
				SET current_version=$6,pickup_id=$7,enabled=true
				WHERE tenant_id=$1 AND store_id=$2 AND kind=$3 AND namespace=$4 AND code=$5 AND current_version=$8`,
				scope.TenantID, scope.StoreID, in.Kind, in.Namespace, in.Code, version, out.ID, currentVersion)
			if updateErr != nil {
				return mapError(updateErr)
			}
			if tag.RowsAffected() != 1 {
				return command.ErrConflict
			}
		} else {
			_, insertErr = tx.Exec(ctx, `INSERT INTO fulfillment.pickup_heads(
				tenant_id,store_id,kind,namespace,code,current_version,pickup_id,enabled)
				VALUES($1,$2,$3,$4,$5,$6,$7,true)`, scope.TenantID, scope.StoreID,
				in.Kind, in.Namespace, in.Code, version, out.ID)
			if insertErr != nil {
				return mapError(insertErr)
			}
		}
		return command.Audit(ctx, tx, scope, "fulfillment.pickup.attest")
	})
	return out, mapError(err)
}

func RevokePickup(ctx context.Context, tx pgx.Tx, scope platform.Scope, token, key, pickupID string) error {
	if !command.ValidID(pickupID) {
		return command.ErrInvalid
	}
	if err := authorize(ctx, tx, scope, token, managePermission); err != nil {
		return err
	}
	request := struct {
		PrincipalID string `json:"principal_id"`
		PickupID    string `json:"pickup_id"`
	}{scope.PrincipalID, pickupID}
	var result struct{}
	err := command.Run(ctx, tx, scope, "fulfillment.pickup.revoke", key, request, &result, func() error {
		var kind, namespace, code string
		var version int64
		readErr := tx.QueryRow(ctx, `SELECT kind,namespace,code,version FROM fulfillment.pickup_versions
			WHERE tenant_id=$1 AND store_id=$2 AND id=$3`, scope.TenantID, scope.StoreID, pickupID).
			Scan(&kind, &namespace, &code, &version)
		if readErr != nil {
			return mapError(readErr)
		}
		if lockErr := advisoryLock(ctx, tx, pickupLockKey(scope.TenantID, scope.StoreID, kind, namespace, code)); lockErr != nil {
			return lockErr
		}
		var currentVersion int64
		var currentID string
		var enabled bool
		lockErr := tx.QueryRow(ctx, `SELECT current_version,pickup_id::text,enabled FROM fulfillment.pickup_heads
			WHERE tenant_id=$1 AND store_id=$2 AND kind=$3 AND namespace=$4 AND code=$5 FOR UPDATE`,
			scope.TenantID, scope.StoreID, kind, namespace, code).Scan(&currentVersion, &currentID, &enabled)
		if lockErr != nil {
			return mapError(lockErr)
		}
		if currentVersion != version || currentID != pickupID {
			return command.ErrConflict
		}
		if enabled {
			tag, updateErr := tx.Exec(ctx, `UPDATE fulfillment.pickup_heads SET enabled=false
				WHERE tenant_id=$1 AND store_id=$2 AND kind=$3 AND namespace=$4 AND code=$5
				AND current_version=$6 AND pickup_id=$7`, scope.TenantID, scope.StoreID,
				kind, namespace, code, version, pickupID)
			if updateErr != nil {
				return mapError(updateErr)
			}
			if tag.RowsAffected() != 1 {
				return command.ErrConflict
			}
		}
		return command.Audit(ctx, tx, scope, "fulfillment.pickup.revoke")
	})
	return mapError(err)
}

func ReadPickup(ctx context.Context, tx pgx.Tx, scope buyer.Scope, id string) (Pickup, error) {
	if err := buyer.CheckScope(ctx, tx, scope); err != nil {
		return Pickup{}, err
	}
	if !command.ValidID(id) {
		return Pickup{}, command.ErrInvalid
	}
	return readPickup(ctx, tx, scope, id)
}

func LockPickup(ctx context.Context, tx pgx.Tx, scope buyer.Scope, id string) (Pickup, error) {
	if err := buyer.CheckScope(ctx, tx, scope); err != nil {
		return Pickup{}, err
	}
	if !command.ValidID(id) {
		return Pickup{}, command.ErrInvalid
	}
	pickup, err := readPickup(ctx, tx, scope, id)
	if err != nil {
		return Pickup{}, err
	}
	var currentVersion int64
	var currentID string
	var enabled bool
	err = tx.QueryRow(ctx, `SELECT current_version,pickup_id::text,enabled FROM fulfillment.pickup_heads
		WHERE tenant_id=$1 AND store_id=$2 AND kind=$3 AND namespace=$4 AND code=$5 FOR SHARE`,
		scope.TenantID, scope.StoreID, pickup.Kind, pickup.Namespace, pickup.Code).
		Scan(&currentVersion, &currentID, &enabled)
	if err != nil {
		return Pickup{}, mapError(err)
	}
	if !enabled || currentVersion != pickup.Version || currentID != pickup.ID {
		return Pickup{}, command.ErrConflict
	}
	var current bool
	err = tx.QueryRow(ctx, `WITH stamp AS MATERIALIZED (SELECT clock_timestamp() AS observed)
		SELECT v.attested_at<=stamp.observed AND v.valid_until>stamp.observed
		FROM fulfillment.pickup_versions v CROSS JOIN stamp
		WHERE v.tenant_id=$1 AND v.store_id=$2 AND v.id=$3`,
		scope.TenantID, scope.StoreID, id).Scan(&current)
	if err != nil {
		return Pickup{}, mapError(err)
	}
	if !current {
		return Pickup{}, command.ErrConflict
	}
	return pickup, nil
}

func readPickup(ctx context.Context, tx pgx.Tx, scope buyer.Scope, id string) (Pickup, error) {
	var out Pickup
	err := tx.QueryRow(ctx, `SELECT id::text,kind,namespace,code,version,country,name,address,
		verification_kind,attested_at,valid_until FROM fulfillment.pickup_versions
		WHERE tenant_id=$1 AND store_id=$2 AND id=$3`, scope.TenantID, scope.StoreID, id).Scan(
		&out.ID, &out.Kind, &out.Namespace, &out.Code, &out.Version, &out.Country,
		&out.Name, &out.Address, &out.VerificationKind, &out.AttestedAt, &out.ValidUntil)
	return out, mapError(err)
}

func validPickupInput(in PickupInput) bool {
	if !isCVSKind(in.Kind) {
		return false
	}
	return pickupNamespacePattern.MatchString(in.Namespace) && pickupCodePattern.MatchString(in.Code) &&
		in.ExpectedVersion >= 0 && in.ExpectedVersion < math.MaxInt64 &&
		printable(in.Name, 120) && printable(in.Address, 400) && printable(in.EvidenceRef, 240) &&
		in.TTLSeconds >= 1 && in.TTLSeconds <= 604800
}

func pickupLockKey(tenantID, storeID, kind, namespace, code string) string {
	return "fulfillment.pickup|" + tenantID + "|" + storeID + "|" + kind + "|" + namespace + "|" + code
}
