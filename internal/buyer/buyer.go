// Package buyer owns the anonymous buyer capability boundary: issuing short-lived opaque capability
// tokens on the issuer pool and scoping every buyer transaction to one (tenant, store, owner,
// session) with a replay-safe command record.
//
// It never identifies a person, holds merchant authority or PII, or trusts a tenant or store id from
// a request; merchant scope is internal/platform. Only Service.New touches the issuer pool, so an
// issuer pool cannot reach buyer callbacks.
package buyer

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

var (
	ErrInvalid      = errors.New("invalid buyer capability request")
	ErrUnauthorized = errors.New("unauthorized")
	ErrRateLimited  = errors.New("buyer capability admission limited")
	errUnavailable  = errors.New("buyer capability unavailable")
)

const (
	requestTimeout = 5 * time.Second
	lockTimeout    = time.Second
	cleanupTimeout = 2 * time.Second
	minimumTTL     = time.Minute
	maximumTTL     = 30 * 24 * time.Hour
)

// Scope is buyer authority derived by the database, not merchant authority.
type Scope struct {
	TenantID  string
	StoreID   string
	OwnerID   string
	SessionID string
}

type Capability struct {
	Token     string `json:"-"`
	Scope     Scope
	ExpiresAt time.Time
}

// Service owns only the issuer pool. Runtime resolution is deliberately a
// package function so an issuer pool cannot accidentally reach buyer callbacks.
type Service struct {
	issuerPool *pgxpool.Pool
	ttlSeconds int64
}

func New(issuerPool *pgxpool.Pool, ttl time.Duration) (*Service, error) {
	if issuerPool == nil || ttl < minimumTTL || ttl > maximumTTL || ttl%time.Second != 0 {
		return nil, ErrInvalid
	}
	return &Service{issuerPool: issuerPool, ttlSeconds: int64(ttl / time.Second)}, nil
}

func (s *Service) IssueForTrustedStore(ctx context.Context, storeID string) (Capability, error) {
	if s == nil || s.issuerPool == nil || !validUUID(storeID) {
		return Capability{}, ErrInvalid
	}
	token := randomToken()
	hash := sha256.Sum256([]byte(token))
	callCtx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()

	var capability Capability
	err := s.issuerPool.QueryRow(callCtx, `SELECT tenant_id::text,store_id::text,owner_id::text,session_id::text,expires_at
		FROM buyer.issue_capability($1::uuid,$2,$3)`, storeID, hash[:], s.ttlSeconds).
		Scan(&capability.Scope.TenantID, &capability.Scope.StoreID, &capability.Scope.OwnerID, &capability.Scope.SessionID, &capability.ExpiresAt)
	if err != nil {
		return Capability{}, translate(callCtx, err)
	}
	capability.Token = token
	return capability, nil
}

func (s *Service) Revoke(ctx context.Context, token, storeID string) error {
	if s == nil || s.issuerPool == nil || !validToken(token) || !validUUID(storeID) {
		return ErrInvalid
	}
	hash := sha256.Sum256([]byte(token))
	callCtx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	tx, err := s.issuerPool.Begin(callCtx)
	if err != nil {
		return translate(callCtx, err)
	}
	defer rollback(tx)
	// Revocation must wait behind an already-authorized buyer transaction. The
	// 5s request deadline bounds that wait; the 1s runtime row-lock limit does not.
	if _, err = tx.Exec(callCtx, `SELECT
		set_config('statement_timeout',$1,true),
		set_config('lock_timeout','0',true),
		set_config('idle_in_transaction_session_timeout',$1,true)`, requestTimeout.String()); err != nil {
		return translate(callCtx, err)
	}
	if _, err = tx.Exec(callCtx, `SELECT buyer.revoke_capability($1,$2::uuid)`, hash[:], storeID); err != nil {
		return translate(callCtx, err)
	}
	if err = tx.Commit(callCtx); err != nil {
		return translate(callCtx, err)
	}
	return nil
}

// WithScope resolves one opaque capability and runs fn in that same bounded
// transaction. Only a separately validated buyer or internal checkout pool
// belongs here; the caller must validate its pool authority before use.
func WithScope(ctx context.Context, buyerPool *pgxpool.Pool, token, storeID string, fn func(context.Context, pgx.Tx, Scope) error) (err error) {
	if buyerPool == nil || fn == nil || !validToken(token) || !validUUID(storeID) {
		return ErrUnauthorized
	}
	scopeCtx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	tx, err := buyerPool.BeginTx(scopeCtx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return translate(scopeCtx, err)
	}
	defer func() {
		if recovered := recover(); recovered != nil {
			rollback(tx)
			panic(recovered)
		}
		if err != nil {
			rollback(tx)
		}
	}()

	if _, err = tx.Exec(scopeCtx, `SELECT
		set_config('statement_timeout',$1,true),
		set_config('lock_timeout',$2,true),
		set_config('idle_in_transaction_session_timeout',$3,true)`, requestTimeout.String(), lockTimeout.String(), requestTimeout.String()); err != nil {
		return translate(scopeCtx, err)
	}
	hash := sha256.Sum256([]byte(token))
	var scope Scope
	err = tx.QueryRow(scopeCtx, `SELECT tenant_id::text,store_id::text,owner_id::text,session_id::text
		FROM buyer.resolve_scope($1,$2::uuid)`, hash[:], storeID).
		Scan(&scope.TenantID, &scope.StoreID, &scope.OwnerID, &scope.SessionID)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrUnauthorized
	}
	if err != nil {
		return translate(scopeCtx, err)
	}
	if scope.StoreID != storeID || !validUUID(scope.TenantID) || !validUUID(scope.OwnerID) || !validUUID(scope.SessionID) {
		return ErrUnauthorized
	}
	if _, err = tx.Exec(scopeCtx, `SELECT
		set_config('app.tenant_id',$1,true),
		set_config('app.store_id',$2,true),
		set_config('app.buyer_id',$3,true),
		set_config('app.buyer_session_id',$4,true),
		set_config('app.principal_id','',true)`, scope.TenantID, scope.StoreID, scope.OwnerID, scope.SessionID); err != nil {
		return translate(scopeCtx, err)
	}
	if err = fn(scopeCtx, tx, scope); err != nil {
		return err
	}
	if err = tx.Commit(scopeCtx); err != nil {
		return translate(scopeCtx, err)
	}
	return nil
}

func rollback(tx pgx.Tx) {
	ctx, cancel := context.WithTimeout(context.Background(), cleanupTimeout)
	defer cancel()
	_ = tx.Rollback(ctx)
}

func translate(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		switch pgErr.Code {
		case "PT400":
			return ErrInvalid
		case "PT401":
			return ErrUnauthorized
		case "PT429":
			return ErrRateLimited
		}
	}
	return errUnavailable
}

func randomToken() string {
	value := make([]byte, 32)
	_, _ = rand.Read(value)
	return base64.RawURLEncoding.EncodeToString(value)
}

func validToken(value string) bool {
	decoded, err := base64.RawURLEncoding.Strict().DecodeString(value)
	return len(value) == 43 && err == nil && len(decoded) == 32
}

func validUUID(value string) bool {
	if len(value) != 36 || value != strings.ToLower(value) {
		return false
	}
	for index, character := range value {
		switch index {
		case 8, 13, 18, 23:
			if character != '-' {
				return false
			}
		default:
			if !(character >= '0' && character <= '9' || character >= 'a' && character <= 'f') {
				return false
			}
		}
	}
	return true
}
