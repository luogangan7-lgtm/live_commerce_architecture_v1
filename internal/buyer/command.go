package buyer

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"reflect"
	"regexp"

	"github.com/jackc/pgx/v5"
	"livecommerce/internal/command"
)

var commandKey = regexp.MustCompile(`^[A-Za-z0-9_.:-]{8,128}$`)
var commandOperation = regexp.MustCompile(`^[a-z][a-z0-9_.:]{0,79}$`)

// CheckScope binds a typed domain scope to the authenticated transaction. Domain
// arguments are not credentials. RLS independently checks owner/store visibility.
func CheckScope(ctx context.Context, tx pgx.Tx, s Scope) error {
	if tx == nil || !command.ValidID(s.TenantID) || !command.ValidID(s.StoreID) || !command.ValidID(s.OwnerID) || !command.ValidID(s.SessionID) {
		return command.ErrInvalid
	}
	var matches bool
	err := tx.QueryRow(ctx, `SELECT coalesce(current_setting('app.tenant_id',true)=$1
		AND current_setting('app.store_id',true)=$2 AND current_setting('app.buyer_id',true)=$3
		AND current_setting('app.buyer_session_id',true)=$4 AND current_setting('app.principal_id',true)='',false)`,
		s.TenantID, s.StoreID, s.OwnerID, s.SessionID).Scan(&matches)
	if err != nil {
		return err
	}
	if !matches {
		return command.ErrInvalid
	}
	return nil
}

// RunCommand keeps capability receipts separate from merchant membership-based
// receipts. Both use canonical JSON and transaction advisory locks; these keys
// additionally include owner and session. Caller must roll back every error.
func RunCommand(ctx context.Context, tx pgx.Tx, s Scope, operation, key string, request, result any, fn func() error) error {
	if fn == nil || result == nil || reflect.ValueOf(result).Kind() != reflect.Pointer || reflect.ValueOf(result).IsNil() ||
		!commandKey.MatchString(key) || !commandOperation.MatchString(operation) {
		return command.ErrInvalid
	}
	if err := CheckScope(ctx, tx, s); err != nil {
		return err
	}
	body, err := json.Marshal(request)
	if err != nil || len(body) > 65536 {
		return command.ErrInvalid
	}
	digest := sha256.Sum256(body)
	lockKey := "buyer-command|" + s.TenantID + "|" + s.StoreID + "|" + s.OwnerID + "|" + s.SessionID + "|" + operation + "|" + key
	var timeout string
	if err = tx.QueryRow(ctx, `SHOW lock_timeout`).Scan(&timeout); err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, `SELECT set_config('lock_timeout','0',true)`); err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, lockKey); err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, `SELECT set_config('lock_timeout',$1,true)`, timeout); err != nil {
		return err
	}
	var previousHash, previousJSON []byte
	err = tx.QueryRow(ctx, `SELECT request_hash,response FROM buyer.command_results
		WHERE tenant_id=$1 AND store_id=$2 AND owner_id=$3 AND session_id=$4 AND operation=$5 AND idempotency_key=$6`,
		s.TenantID, s.StoreID, s.OwnerID, s.SessionID, operation, key).Scan(&previousHash, &previousJSON)
	if err == nil {
		if !bytes.Equal(previousHash, digest[:]) {
			return command.ErrConflict
		}
		if err = json.Unmarshal(previousJSON, result); err != nil {
			return err
		}
		// Replay must equal the first result, which was scanned by pgx in time.Local.
		command.InLocalTime(result)
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
		return err
	}
	if len(encoded) > 1048576 {
		return command.ErrInvalid
	}
	_, err = tx.Exec(ctx, `INSERT INTO buyer.command_results(tenant_id,store_id,owner_id,session_id,operation,idempotency_key,request_hash,response)
		VALUES($1,$2,$3,$4,$5,$6,$7,$8)`, s.TenantID, s.StoreID, s.OwnerID, s.SessionID, operation, key, digest[:], encoded)
	return err
}
