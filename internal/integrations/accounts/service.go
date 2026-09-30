// Package accounts owns the custody of merchant-owned provider credentials: sealed key material,
// account records and the hosted-payment configuration read. Configuration never implies the provider
// has approved or verified an account. It never logs or returns a plaintext key, never calls a provider
// (verification lives in the registrars and workers), and never derives merchant scope from a request.
package accounts

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"regexp"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"livecommerce/internal/command"
	"livecommerce/internal/integrations/core"
	"livecommerce/internal/platform"
)

const configuredUnverified = "CONFIGURED_UNVERIFIED"

var accountIDPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

type Service struct {
	keys     *Keyring
	bindings *core.Service
}

type CreateInput struct {
	Provider    string
	Environment string
	AccountID   string
	Credentials Credentials
}

type RotateInput struct {
	ConnectionID    string
	ExpectedVersion int64
	Credentials     Credentials
}

type Connection struct {
	ID                string    `json:"id"`
	Provider          string    `json:"provider"`
	Environment       string    `json:"environment"`
	AccountID         string    `json:"account_id"`
	BindingID         string    `json:"binding_id"`
	BindingVersion    int64     `json:"binding_version"`
	Enabled           bool      `json:"enabled"`
	CredentialVersion int64     `json:"credential_version"`
	KeyID             string    `json:"key_id"`
	State             string    `json:"state"`
	CreatedAt         time.Time `json:"created_at"`
	UpdatedAt         time.Time `json:"updated_at"`
}

func New(keys *Keyring, bindings *core.Service) (*Service, error) {
	if keys == nil || len(keys.keys) == 0 || len(keys.replayKey) != 32 || bindings == nil {
		return nil, command.ErrInvalid
	}
	return &Service{keys: keys, bindings: bindings}, nil
}

func (s *Service) Create(ctx context.Context, tx pgx.Tx, scope platform.Scope, token, key string, in CreateInput) (out Connection, err error) {
	if s == nil || s.keys == nil || s.bindings == nil || in.Provider != "payuni" ||
		!validEnvironment(in.Environment) || !validAccountID(in.AccountID) || !validCredentials(in.Credentials) {
		return out, command.ErrInvalid
	}
	if err = authorize(ctx, tx, scope, token, "integration:manage"); err != nil {
		return out, err
	}
	request := struct {
		PrincipalID string `json:"principal_id"`
		Provider    string `json:"provider"`
		Environment string `json:"environment"`
		AccountID   string `json:"account_id"`
		Fingerprint string `json:"credential_fingerprint"`
	}{scope.PrincipalID, in.Provider, in.Environment, in.AccountID, s.fingerprint(scope, in.Credentials)}
	err = command.Run(ctx, tx, scope, "integration.account.create", key, request, &out, func() error {
		if err := tx.QueryRow(ctx, `SELECT gen_random_uuid()::text`).Scan(&out.ID); err != nil {
			return err
		}
		assetID := in.Environment + ":" + in.AccountID
		binding, err := s.bindings.RegisterBinding(ctx, tx, scope, token, out.ID, in.Provider, assetID)
		if err != nil {
			return err
		}
		aad := newAAD(scope, out.ID, in.Provider, in.Environment, in.AccountID, 1)
		keyID, nonce, ciphertext, err := s.keys.seal(aad, in.Credentials)
		if err != nil {
			return err
		}
		out.Provider, out.Environment, out.AccountID = in.Provider, in.Environment, in.AccountID
		out.BindingID, out.BindingVersion, out.Enabled = binding.ID, binding.SemanticVersion, binding.Enabled
		out.CredentialVersion, out.KeyID, out.State = 1, keyID, configuredUnverified
		err = tx.QueryRow(ctx, `INSERT INTO integration.merchant_accounts
			(id,tenant_id,store_id,principal_id,provider,environment,account_id,binding_id,credential_version)
			VALUES($1,$2,$3,$4,$5,$6,$7,$8,1)
			RETURNING created_at,updated_at`, out.ID, scope.TenantID, scope.StoreID, scope.PrincipalID,
			in.Provider, in.Environment, in.AccountID, binding.ID).Scan(&out.CreatedAt, &out.UpdatedAt)
		if err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `INSERT INTO integration.account_credentials
			(tenant_id,store_id,connection_id,version,key_id,nonce,ciphertext,principal_id)
			VALUES($1,$2,$3,1,$4,$5,$6,$7)`, scope.TenantID, scope.StoreID, out.ID,
			keyID, nonce, ciphertext, scope.PrincipalID)
		if err != nil {
			return err
		}
		return command.Audit(ctx, tx, scope, "integration.account.created")
	})
	if err != nil {
		return Connection{}, mapError(err)
	}
	if err = authorize(ctx, tx, scope, token, "integration:manage"); err != nil {
		return Connection{}, err
	}
	return out, nil
}

func (s *Service) Rotate(ctx context.Context, tx pgx.Tx, scope platform.Scope, token, key string, in RotateInput) (out Connection, err error) {
	if s == nil || s.keys == nil || s.bindings == nil || !command.ValidID(in.ConnectionID) ||
		in.ExpectedVersion < 1 || in.ExpectedVersion == math.MaxInt64 || !validCredentials(in.Credentials) {
		return out, command.ErrInvalid
	}
	if err = authorize(ctx, tx, scope, token, "integration:manage"); err != nil {
		return out, err
	}
	request := struct {
		PrincipalID     string `json:"principal_id"`
		ConnectionID    string `json:"connection_id"`
		ExpectedVersion int64  `json:"expected_version"`
		Fingerprint     string `json:"credential_fingerprint"`
	}{scope.PrincipalID, in.ConnectionID, in.ExpectedVersion, s.fingerprint(scope, in.Credentials)}
	err = command.Run(ctx, tx, scope, "integration.account.rotate", key, request, &out, func() error {
		out.ID = in.ConnectionID
		err := tx.QueryRow(ctx, `SELECT provider,environment,account_id,binding_id::text,credential_version,created_at
			FROM integration.merchant_accounts WHERE tenant_id=$1 AND store_id=$2 AND id=$3 FOR UPDATE`,
			scope.TenantID, scope.StoreID, in.ConnectionID).Scan(&out.Provider, &out.Environment,
			&out.AccountID, &out.BindingID, &out.CredentialVersion, &out.CreatedAt)
		if err != nil {
			return err
		}
		if out.CredentialVersion != in.ExpectedVersion {
			return command.ErrConflict
		}
		// Hold the binding while reading its current control state; rotation
		// never changes its semantic version or enabled flag.
		err = tx.QueryRow(ctx, `SELECT semantic_version,enabled FROM integration.bindings
			WHERE tenant_id=$1 AND store_id=$2 AND id=$3 FOR SHARE`, scope.TenantID, scope.StoreID,
			out.BindingID).Scan(&out.BindingVersion, &out.Enabled)
		if err != nil {
			return err
		}
		out.CredentialVersion++
		keyID, nonce, ciphertext, err := s.keys.seal(newAAD(scope, out.ID, out.Provider, out.Environment,
			out.AccountID, out.CredentialVersion), in.Credentials)
		if err != nil {
			return err
		}
		out.KeyID, out.State = keyID, configuredUnverified
		_, err = tx.Exec(ctx, `INSERT INTO integration.account_credentials
			(tenant_id,store_id,connection_id,version,key_id,nonce,ciphertext,principal_id)
			VALUES($1,$2,$3,$4,$5,$6,$7,$8)`, scope.TenantID, scope.StoreID, out.ID,
			out.CredentialVersion, keyID, nonce, ciphertext, scope.PrincipalID)
		if err != nil {
			return err
		}
		err = tx.QueryRow(ctx, `UPDATE integration.merchant_accounts
			SET credential_version=$4,updated_at=clock_timestamp()
			WHERE tenant_id=$1 AND store_id=$2 AND id=$3 AND credential_version=$5
			RETURNING updated_at`, scope.TenantID, scope.StoreID, out.ID, out.CredentialVersion,
			in.ExpectedVersion).Scan(&out.UpdatedAt)
		if err != nil {
			return err
		}
		return command.Audit(ctx, tx, scope, "integration.account.rotated")
	})
	if err != nil {
		return Connection{}, mapVersionError(err)
	}
	if err = authorize(ctx, tx, scope, token, "integration:manage"); err != nil {
		return Connection{}, err
	}
	return out, nil
}

func (s *Service) Get(ctx context.Context, tx pgx.Tx, scope platform.Scope, token, connectionID string) (out Connection, err error) {
	if s == nil || s.keys == nil || s.bindings == nil || !command.ValidID(connectionID) {
		return out, command.ErrInvalid
	}
	if err = authorize(ctx, tx, scope, token, "integration:read"); err != nil {
		return out, err
	}
	err = tx.QueryRow(ctx, `SELECT a.id::text,a.provider,a.environment,a.account_id,a.binding_id::text,
		b.semantic_version,b.enabled,a.credential_version,c.key_id,a.created_at,a.updated_at
		FROM integration.merchant_accounts a
		JOIN integration.bindings b ON b.tenant_id=a.tenant_id AND b.store_id=a.store_id AND b.id=a.binding_id
		JOIN integration.account_credentials c ON c.tenant_id=a.tenant_id AND c.store_id=a.store_id
		 AND c.connection_id=a.id AND c.version=a.credential_version
		WHERE a.tenant_id=$1 AND a.store_id=$2 AND a.id=$3
		FOR SHARE OF a,b`, scope.TenantID, scope.StoreID, connectionID).Scan(&out.ID, &out.Provider,
		&out.Environment, &out.AccountID, &out.BindingID, &out.BindingVersion, &out.Enabled,
		&out.CredentialVersion, &out.KeyID, &out.CreatedAt, &out.UpdatedAt)
	if err != nil {
		return Connection{}, mapError(err)
	}
	if err = authorize(ctx, tx, scope, token, "integration:read"); err != nil {
		return Connection{}, err
	}
	out.State = configuredUnverified
	return out, nil
}

func newAAD(scope platform.Scope, id, provider, environment, accountID string, version int64) credentialAAD {
	return credentialAAD{1, scope.TenantID, scope.StoreID, id, provider, environment, accountID, version}
}

func validEnvironment(value string) bool { return value == "SANDBOX" || value == "LIVE" }
func validAccountID(value string) bool   { return accountIDPattern.MatchString(value) }

// Only the keyed digest reaches command.Run. The independent replay key stays
// stable as encryption keys rotate, preserving the permanent idempotency key.
func (s *Service) fingerprint(scope platform.Scope, c Credentials) string {
	plain, _ := json.Marshal(struct {
		Domain   string `json:"domain"`
		TenantID string `json:"tenant_id"`
		StoreID  string `json:"store_id"`
		HashKey  string `json:"hash_key"`
		HashIV   string `json:"hash_iv"`
	}{"merchant-account-credential-replay-v1", scope.TenantID, scope.StoreID, c.HashKey, c.HashIV})
	mac := hmac.New(sha256.New, s.keys.replayKey)
	_, _ = mac.Write(plain)
	return fmt.Sprintf("%x", mac.Sum(nil))
}

func authorize(ctx context.Context, tx pgx.Tx, scope platform.Scope, token, permission string) error {
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
	return platform.RequirePermission(ctx, tx, scope, token, permission)
}

func mapVersionError(err error) error {
	if errors.Is(err, pgx.ErrNoRows) {
		return command.ErrConflict
	}
	return mapError(err)
}

func mapError(err error) error {
	if err == nil || errors.Is(err, command.ErrInvalid) || errors.Is(err, command.ErrConflict) ||
		errors.Is(err, command.ErrNotFound) {
		return err
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return command.ErrNotFound
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		switch pgErr.Code {
		case "40001", "23505":
			return command.ErrConflict
		case "23503", "P0002":
			return command.ErrNotFound
		case "22001", "22007", "22008", "22023", "22P02", "23514":
			return command.ErrInvalid
		}
	}
	return err
}
