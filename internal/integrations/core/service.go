// Package core owns the provider-neutral external-operation ledger boundary: the external_operation
// records, their River dispatcher and the sealed-secret handle that provider routes receive.
//
// It never calls a provider itself (routes registered by metareply, payments and others do), never
// commits caller-owned transactions, and never retries an UNKNOWN outcome with a new idempotency
// key.
package core

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/riverqueue/river"

	"livecommerce/internal/command"
	"livecommerce/internal/platform"
)

const (
	managePermission  = "integration:manage"
	executePermission = "integration:execute"
	readPermission    = "integration:read"
	maxRequestBytes   = 64 * 1024
)

var (
	providerPattern = regexp.MustCompile(`^[a-z][a-z0-9_]{0,39}$`)
	actionPattern   = regexp.MustCompile(`^[a-z][a-z0-9_.:]{0,79}$`)
	codePattern     = regexp.MustCompile(`^[A-Za-z0-9_.:-]{1,80}$`)
)

// Service reuses the process River client solely for transactional inserts.
type Service struct {
	jobs *river.Client[pgx.Tx]
}

func New(jobs *river.Client[pgx.Tx]) (*Service, error) {
	if jobs == nil {
		return nil, command.ErrInvalid
	}
	return &Service{jobs: jobs}, nil
}

type Binding struct {
	ID              string    `json:"binding_id"`
	Provider        string    `json:"provider"`
	ExternalAssetID string    `json:"external_asset_id"`
	SemanticVersion int64     `json:"semantic_version"`
	Enabled         bool      `json:"enabled"`
	CreatedAt       time.Time `json:"created_at"`
	UpdatedAt       time.Time `json:"updated_at"`
}

type PlanInput struct {
	BindingID              string          `json:"binding_id"`
	ExpectedBindingVersion int64           `json:"binding_version"`
	Purpose                string          `json:"purpose"`
	Action                 string          `json:"action"`
	Request                json.RawMessage `json:"request"`
}

type PlanResult struct {
	OperationID string `json:"operation_id"`
	JobID       int64  `json:"job_id"`
}

// Operation is a historical read projection. It intentionally excludes the
// worker lease-token digest and exposes no provider credential.
type Operation struct {
	ID                string          `json:"operation_id"`
	TenantID          string          `json:"tenant_id"`
	StoreID           string          `json:"store_id"`
	PrincipalID       string          `json:"principal_id"`
	BindingID         string          `json:"binding_id"`
	BindingVersion    int64           `json:"binding_version"`
	Provider          string          `json:"provider"`
	ExternalAssetID   string          `json:"external_asset_id"`
	Purpose           string          `json:"purpose"`
	Action            string          `json:"action"`
	SemanticKey       string          `json:"semantic_key"`
	Request           json.RawMessage `json:"request"`
	JobID             int64           `json:"job_id"`
	State             string          `json:"state"`
	Generation        int64           `json:"generation"`
	LeaseMode         string          `json:"lease_mode"`
	LeaseUntil        *time.Time      `json:"lease_until,omitempty"`
	ResultCode        string          `json:"result_code"`
	ProviderReference string          `json:"provider_reference"`
	CreatedAt         time.Time       `json:"created_at"`
	UpdatedAt         time.Time       `json:"updated_at"`
}

type ClaimResult struct {
	Disposition string `json:"disposition"`
	Generation  int64  `json:"generation"`
	Mode        string `json:"mode"`
	LeaseToken  []byte `json:"-"`
}

type Outcome struct {
	State             string `json:"state"`
	Code              string `json:"code"`
	ProviderReference string `json:"provider_reference"`
	// Detail is route-private data from the adapter to its own DispatchRoute.Finish (R-7a). Complete
	// ignores it and it is never marshalled; it must hold a comparable value (tests compare Outcomes).
	Detail any `json:"-"`
}

type externalOperationArgs struct {
	OperationID string `json:"operation_id"`
	Version     int    `json:"version"`
}

func (externalOperationArgs) Kind() string { return "external_operation_v1" }

func (s *Service) RegisterBinding(ctx context.Context, tx pgx.Tx, scope platform.Scope, token, key, provider, assetID string) (out Binding, err error) {
	if s == nil || s.jobs == nil || !validProvider(provider) || !validAsset(assetID) {
		return out, command.ErrInvalid
	}
	if err = authorize(ctx, tx, scope, token, managePermission); err != nil {
		return out, err
	}
	request := struct {
		PrincipalID string `json:"principal_id"`
		Provider    string `json:"provider"`
		AssetID     string `json:"external_asset_id"`
	}{scope.PrincipalID, provider, assetID}
	err = command.Run(ctx, tx, scope, "integration.binding.register", key, request, &out, func() error {
		err := tx.QueryRow(ctx, `INSERT INTO integration.bindings(tenant_id,store_id,principal_id,provider,external_asset_id)
			VALUES($1,$2,$3,$4,$5)
			RETURNING id::text,provider,external_asset_id,semantic_version,enabled,created_at,updated_at`,
			scope.TenantID, scope.StoreID, scope.PrincipalID, provider, assetID).
			Scan(&out.ID, &out.Provider, &out.ExternalAssetID, &out.SemanticVersion, &out.Enabled, &out.CreatedAt, &out.UpdatedAt)
		if err != nil {
			return err
		}
		return command.Audit(ctx, tx, scope, "integration.binding.registered")
	})
	return out, mapError(err)
}

func (s *Service) SetBindingEnabled(ctx context.Context, tx pgx.Tx, scope platform.Scope, token, key, bindingID string, expectedVersion int64, enabled bool) (out Binding, err error) {
	if s == nil || s.jobs == nil || !command.ValidID(bindingID) || expectedVersion < 1 {
		return out, command.ErrInvalid
	}
	if err = authorize(ctx, tx, scope, token, managePermission); err != nil {
		return out, err
	}
	request := struct {
		PrincipalID     string `json:"principal_id"`
		BindingID       string `json:"binding_id"`
		ExpectedVersion int64  `json:"expected_version"`
		Enabled         bool   `json:"enabled"`
	}{scope.PrincipalID, bindingID, expectedVersion, enabled}
	err = command.Run(ctx, tx, scope, "integration.binding.enable", key, request, &out, func() error {
		err := tx.QueryRow(ctx, `UPDATE integration.bindings
			SET enabled=$5,semantic_version=semantic_version+1,updated_at=clock_timestamp()
			WHERE tenant_id=$1 AND store_id=$2 AND id=$3 AND semantic_version=$4 AND enabled<>$5
			RETURNING id::text,provider,external_asset_id,semantic_version,enabled,created_at,updated_at`,
			scope.TenantID, scope.StoreID, bindingID, expectedVersion, enabled).
			Scan(&out.ID, &out.Provider, &out.ExternalAssetID, &out.SemanticVersion, &out.Enabled, &out.CreatedAt, &out.UpdatedAt)
		if err != nil {
			return err
		}
		return command.Audit(ctx, tx, scope, "integration.binding.enabled")
	})
	return out, mapVersionError(err)
}

func (s *Service) Plan(ctx context.Context, tx pgx.Tx, scope platform.Scope, token, key string, in PlanInput) (out PlanResult, err error) {
	if s == nil || s.jobs == nil || !command.ValidID(in.BindingID) || in.ExpectedBindingVersion < 1 ||
		!validPurpose(in.Purpose) || !actionPattern.MatchString(in.Action) {
		return out, command.ErrInvalid
	}
	requestObject, requestJSON, err := canonicalObject(in.Request)
	if err != nil {
		return out, err
	}
	if err = authorize(ctx, tx, scope, token, executePermission); err != nil {
		return out, err
	}
	canonical := struct {
		PrincipalID    string         `json:"principal_id"`
		BindingID      string         `json:"binding_id"`
		BindingVersion int64          `json:"binding_version"`
		Purpose        string         `json:"purpose"`
		Action         string         `json:"action"`
		Request        map[string]any `json:"request"`
	}{scope.PrincipalID, in.BindingID, in.ExpectedBindingVersion, in.Purpose, in.Action, requestObject}
	canonicalJSON, err := json.Marshal(canonical)
	if err != nil || len(canonicalJSON) > maxRequestBytes {
		return out, command.ErrInvalid
	}
	requestHash := sha256.Sum256(canonicalJSON)

	err = command.Run(ctx, tx, scope, "integration.operation.plan", key, canonical, &out, func() error {
		var existingHash []byte
		err := tx.QueryRow(ctx, `SELECT id::text,job_id,request_hash FROM integration.operations
			WHERE tenant_id=$1 AND store_id=$2 AND semantic_key=$3`, scope.TenantID, scope.StoreID, key).
			Scan(&out.OperationID, &out.JobID, &existingHash)
		if err == nil {
			if !bytes.Equal(existingHash, requestHash[:]) {
				return command.ErrConflict
			}
			return nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}

		var provider, assetID string
		var version int64
		var enabled bool
		err = tx.QueryRow(ctx, `SELECT provider,external_asset_id,semantic_version,enabled
			FROM integration.bindings WHERE tenant_id=$1 AND store_id=$2 AND id=$3 FOR SHARE`,
			scope.TenantID, scope.StoreID, in.BindingID).Scan(&provider, &assetID, &version, &enabled)
		if err != nil {
			return err
		}
		if !enabled || version != in.ExpectedBindingVersion {
			return command.ErrConflict
		}
		if err = tx.QueryRow(ctx, `SELECT gen_random_uuid()::text`).Scan(&out.OperationID); err != nil {
			return err
		}
		job, err := s.jobs.InsertTx(ctx, tx, externalOperationArgs{OperationID: out.OperationID, Version: 1}, nil)
		if err != nil {
			return err
		}
		out.JobID = job.Job.ID
		_, err = tx.Exec(ctx, `INSERT INTO integration.operations(
			tenant_id,store_id,id,principal_id,binding_id,binding_version,provider,external_asset_id,
			purpose,action,semantic_key,request_hash,request,job_id)
			VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14)`,
			scope.TenantID, scope.StoreID, out.OperationID, scope.PrincipalID, in.BindingID,
			in.ExpectedBindingVersion, provider, assetID, in.Purpose, in.Action, key, requestHash[:], requestJSON, out.JobID)
		if err != nil {
			return err
		}
		if _, err = tx.Exec(ctx, `INSERT INTO integration.operation_events(
			tenant_id,store_id,operation_id,generation,state,mode,reason_code)
			VALUES($1,$2,$3,0,'READY','','operation_planned')`, scope.TenantID, scope.StoreID, out.OperationID); err != nil {
			return err
		}
		return command.Audit(ctx, tx, scope, "integration.operation.planned")
	})
	return out, mapError(err)
}

func (s *Service) Get(ctx context.Context, tx pgx.Tx, scope platform.Scope, token, operationID string) (out Operation, err error) {
	if s == nil || s.jobs == nil || !command.ValidID(operationID) {
		return out, command.ErrInvalid
	}
	if err = authorize(ctx, tx, scope, token, readPermission); err != nil {
		return out, err
	}
	err = tx.QueryRow(ctx, `SELECT id::text,tenant_id::text,store_id::text,principal_id::text,binding_id::text,
		binding_version,provider,external_asset_id,purpose,action,semantic_key,request,job_id,state,generation,
		lease_mode,lease_until,result_code,provider_reference,created_at,updated_at
		FROM integration.operations WHERE tenant_id=$1 AND store_id=$2 AND id=$3 AND actor_kind='MERCHANT'`,
		scope.TenantID, scope.StoreID, operationID).Scan(
		&out.ID, &out.TenantID, &out.StoreID, &out.PrincipalID, &out.BindingID,
		&out.BindingVersion, &out.Provider, &out.ExternalAssetID, &out.Purpose, &out.Action,
		&out.SemanticKey, &out.Request, &out.JobID, &out.State, &out.Generation,
		&out.LeaseMode, &out.LeaseUntil, &out.ResultCode, &out.ProviderReference, &out.CreatedAt, &out.UpdatedAt)
	return out, mapError(err)
}

func (s *Service) Claim(ctx context.Context, tx pgx.Tx, operationID string, leaseSeconds int) (out ClaimResult, err error) {
	if s == nil || tx == nil || !command.ValidID(operationID) || leaseSeconds < 5 || leaseSeconds > 300 {
		return out, command.ErrInvalid
	}
	token := make([]byte, 32)
	if _, err = rand.Read(token); err != nil {
		return out, fmt.Errorf("generate lease token: %w", err)
	}
	err = tx.QueryRow(ctx, `SELECT disposition,generation,mode FROM integration.claim_operation($1,$2,$3)`,
		operationID, leaseSeconds, token).Scan(&out.Disposition, &out.Generation, &out.Mode)
	if err != nil {
		return ClaimResult{}, mapError(err)
	}
	if out.Disposition == "claimed" {
		out.LeaseToken = token
	}
	return out, nil
}

func (s *Service) Complete(ctx context.Context, tx pgx.Tx, operationID string, generation int64, leaseToken []byte, outcome Outcome) error {
	if s == nil || tx == nil || !command.ValidID(operationID) || generation < 1 || len(leaseToken) != 32 ||
		!validOutcome(outcome) {
		return command.ErrInvalid
	}
	_, err := tx.Exec(ctx, `SELECT integration.complete_operation($1,$2,$3,$4,$5,$6)`,
		operationID, generation, leaseToken, outcome.State, outcome.Code, outcome.ProviderReference)
	return mapError(err)
}

func authorize(ctx context.Context, tx pgx.Tx, scope platform.Scope, token, permission string) error {
	if err := validateTransactionScope(ctx, tx, scope); err != nil {
		return err
	}
	return platform.RequirePermission(ctx, tx, scope, token, permission)
}

func validateTransactionScope(ctx context.Context, tx pgx.Tx, scope platform.Scope) error {
	if tx == nil || !command.ValidID(scope.TenantID) || !command.ValidID(scope.StoreID) || !command.ValidID(scope.PrincipalID) || scope.Revision < 1 {
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

func canonicalObject(raw json.RawMessage) (map[string]any, []byte, error) {
	if len(raw) == 0 || len(raw) > maxRequestBytes || !utf8.Valid(raw) {
		return nil, nil, command.ErrInvalid
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var object map[string]any
	if err := decoder.Decode(&object); err != nil || object == nil {
		return nil, nil, command.ErrInvalid
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return nil, nil, command.ErrInvalid
	}
	canonical, err := json.Marshal(object)
	if err != nil || len(canonical) > maxRequestBytes {
		return nil, nil, command.ErrInvalid
	}
	return object, canonical, nil
}

func validProvider(value string) bool { return providerPattern.MatchString(value) }

func validAsset(value string) bool {
	if !utf8.ValidString(value) || utf8.RuneCountInString(value) < 1 || utf8.RuneCountInString(value) > 200 {
		return false
	}
	for _, r := range value {
		if unicode.IsControl(r) {
			return false
		}
	}
	return true
}

func validPurpose(value string) bool {
	return value == "transactional" || value == "service" || value == "marketing"
}

func validOutcome(out Outcome) bool {
	validState := out.State == "SUCCEEDED" || out.State == "FAILED_FINAL" || out.State == "UNKNOWN" || out.State == "ACKNOWLEDGED" || out.State == "BLOCKED_POLICY"
	return validState && codePattern.MatchString(out.Code) && validReference(out.ProviderReference)
}

func validReference(value string) bool {
	if !utf8.ValidString(value) || utf8.RuneCountInString(value) > 200 {
		return false
	}
	for _, r := range value {
		if unicode.IsControl(r) {
			return false
		}
	}
	return true
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
		case "40001", "23505", "PT409":
			return command.ErrConflict
		case "23503", "P0002":
			return command.ErrNotFound
		case "22001", "22007", "22008", "22023", "22P02", "23514":
			return command.ErrInvalid
		}
	}
	return err
}
