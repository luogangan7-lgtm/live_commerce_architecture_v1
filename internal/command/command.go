// Package command owns scoped replay records and small transaction primitives.
// It never opens/commits a transaction: platform.WithScope must roll back every
// returned error. Domain code passes validated canonical inputs, not raw HTTP.
package command

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"regexp"

	"github.com/jackc/pgx/v5" // Explicit caller-owned PG transaction; no ORM/pool.
	"livecommerce/internal/platform"
)

var (
	ErrInvalid      = errors.New("invalid request")
	ErrConflict     = errors.New("conflicting request or version")
	ErrNotFound     = errors.New("resource not found")
	ErrInsufficient = errors.New("insufficient available inventory")
	keyPattern      = regexp.MustCompile(`^[A-Za-z0-9_.:-]{8,128}$`)
	opPattern       = regexp.MustCompile(`^[a-z][a-z0-9_.:]{0,79}$`)
	idPattern       = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
)

// Run serializes one scoped command. Only successful, fully serialized results
// are saved; there is no durable "pending" placeholder to strand on crashes.
func Run(ctx context.Context, tx pgx.Tx, scope platform.Scope, operation, key string, request, result any, fn func() error) error {
	if tx == nil || fn == nil || !resultPointer(result) || !keyPattern.MatchString(key) || !opPattern.MatchString(operation) ||
		!ValidID(scope.TenantID) || !ValidID(scope.StoreID) || !ValidID(scope.PrincipalID) {
		return ErrInvalid
	}
	body, err := json.Marshal(request)
	if err != nil {
		return fmt.Errorf("encode command: %w", ErrInvalid)
	}
	if len(body) > 65536 {
		return ErrInvalid
	}
	digest := sha256.Sum256(body)
	// LOCK: stable scope/operation/key, never payload. Hash collisions only cause
	// harmless extra serialization; equality is checked using the full SQL key.
	lockKey := "command|" + scope.TenantID + "|" + scope.StoreID + "|" + operation + "|" + key
	// A valid duplicate may outlive the short row-lock timeout. Only this advisory
	// wait uses the enclosing request/statement deadline; restore row policy next.
	var rowLockTimeout string
	if err = tx.QueryRow(ctx, `SHOW lock_timeout`).Scan(&rowLockTimeout); err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, `SELECT set_config('lock_timeout','0',true)`); err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, lockKey); err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, `SELECT set_config('lock_timeout',$1,true)`, rowLockTimeout); err != nil {
		return err
	}
	var previousHash, previousJSON []byte
	err = tx.QueryRow(ctx, `SELECT request_hash,response FROM ops.command_results
		WHERE tenant_id=$1 AND store_id=$2 AND operation=$3 AND idempotency_key=$4`,
		scope.TenantID, scope.StoreID, operation, key).Scan(&previousHash, &previousJSON)
	if err == nil {
		if !bytes.Equal(previousHash, digest[:]) {
			return ErrConflict
		}
		if err = json.Unmarshal(previousJSON, result); err != nil {
			return fmt.Errorf("decode saved command: %w", err)
		}
		// Replay must equal the first result, which was scanned by pgx in time.Local.
		InLocalTime(result)
		return nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	if err = fn(); err != nil {
		return err
	}
	encoded, err := json.Marshal(result)
	if err != nil {
		return fmt.Errorf("encode command result: %w", err)
	}
	_, err = tx.Exec(ctx, `INSERT INTO ops.command_results(tenant_id,store_id,operation,idempotency_key,request_hash,response,principal_id)
		VALUES($1,$2,$3,$4,$5,$6,$7)`, scope.TenantID, scope.StoreID, operation, key, digest[:], encoded, scope.PrincipalID)
	return err
}

func resultPointer(result any) bool {
	if result == nil {
		return false
	}
	v := reflect.ValueOf(result)
	return v.Kind() == reflect.Pointer && !v.IsNil()
}

const MaxMoney int64 = 1_000_000_000_000
const MaxQuantity int64 = 1_000_000_000

// CheckMoney checks before multiplication, preventing wraparound in quotes.
func CheckMoney(unit, quantity int64) (int64, error) {
	if unit < 0 || unit > MaxMoney || quantity < 1 || quantity > MaxQuantity || unit > MaxMoney/quantity {
		return 0, ErrInvalid
	}
	return unit * quantity, nil
}

func ValidID(id string) bool { return idPattern.MatchString(id) }

// Audit is part of the same transaction as the command, never a best-effort log.
func Audit(ctx context.Context, tx pgx.Tx, scope platform.Scope, action string) error {
	if !opPattern.MatchString(action) {
		return ErrInvalid
	}
	_, err := tx.Exec(ctx, `INSERT INTO ops.audit_events(tenant_id,store_id,principal_id,action) VALUES($1,$2,$3,$4)`,
		scope.TenantID, scope.StoreID, scope.PrincipalID, action)
	return err
}
