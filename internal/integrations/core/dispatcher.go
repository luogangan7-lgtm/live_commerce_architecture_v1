package core

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"

	"livecommerce/internal/command"
	"livecommerce/internal/platform"
)

var ErrPolicyDenied = errors.New("policy denied")

// PolicyDenial is a Check denial that names its BLOCKED_POLICY result code (A10-D2). It satisfies
// errors.Is(_, ErrPolicyDenied), so a route that returns it is denied exactly like a bare
// ErrPolicyDenied; only the recorded code differs. It is meaningful in dispatch mode only: a
// reconcile-mode denial is never BLOCKED_POLICY because an effect may already exist upstream.
type PolicyDenial struct{ Code string }

func (d PolicyDenial) Error() string { return "policy denied: " + d.Code }

// Is makes PolicyDenial a policy denial for errors.Is without wrapping the ErrPolicyDenied sentinel.
func (PolicyDenial) Is(target error) bool { return target == ErrPolicyDenied }

// DenyPolicy returns a policy denial recorded as BLOCKED_POLICY with code. A code that fails
// codePattern is recorded as "policy_denied" (the dispatcher never rejects a denial for its code).
func DenyPolicy(code string) error { return PolicyDenial{Code: code} }

// denialCode is the result code recorded for a Check denial: the PolicyDenial code when valid,
// else the historic "policy_denied" (bare ErrPolicyDenied keeps its byte-identical outcome).
func denialCode(err error) string {
	var denial PolicyDenial
	if errors.As(err, &denial) && codePattern.MatchString(denial.Code) {
		return denial.Code
	}
	return "policy_denied"
}

// secretCallback returns the callback that receives the loaded Secret for this claim mode, or nil
// when the route's mode has no secret path (plain Dispatch, plain Reconcile). LoadSecret runs
// exactly when this is non-nil, so a reconcile claim loads a credential only for routes that opted
// in with ReconcileWithSecret (A-10).
func secretCallback(route DispatchRoute, mode string) func(context.Context, DispatchRequest, Secret) (Outcome, error) {
	if route.LoadSecret == nil {
		return nil
	}
	if mode == "reconcile" {
		return route.ReconcileWithSecret
	}
	return route.DispatchWithSecret
}

// secretLoadFailure maps a failed LoadSecret to the recorded result. Dispatch mode: a policy denial
// means no credential and zero provider calls, so BLOCKED_POLICY is truthful (blocked=true).
// Reconcile mode (A-10): an effect may already exist upstream, so the operation stays UNKNOWN
// (blocked=false) and the same key is reconciled again, never re-POSTed. Any other loader error or
// panic is "secret_load_failed": the loader ran inside the lease, so only Reconcile follows.
func secretLoadFailure(mode string, loadErr error, panicked bool) (code string, blocked bool) {
	if loadErr != nil && !panicked && errors.Is(loadErr, ErrPolicyDenied) {
		return "credential_unavailable", mode == "dispatch"
	}
	return "secret_load_failed", false
}

var (
	errInvalidJob          = errors.New("external_operation_invalid_job")
	errOperationMissing    = errors.New("external_operation_missing")
	errRouteMissing        = errors.New("external_operation_route_missing")
	errDatabase            = errors.New("external_operation_database")
	errCompletionUncertain = errors.New("external_operation_completion_uncertain")
	errBindingBlocked      = errors.New("external_operation_binding_blocked")
	errBudgetExhausted     = errors.New("external_operation_reconcile_budget_exhausted")
)

type DispatchRequest struct {
	OperationID       string          `json:"operation_id"`
	TenantID          string          `json:"tenant_id"`
	StoreID           string          `json:"store_id"`
	PrincipalID       string          `json:"principal_id"`
	BindingID         string          `json:"binding_id"`
	BindingVersion    int64           `json:"binding_version"`
	Provider          string          `json:"provider"`
	ExternalAssetID   string          `json:"external_asset_id"`
	Purpose           string          `json:"purpose"`
	Action            string          `json:"action"`
	Request           json.RawMessage `json:"request"`
	ProviderReference string          `json:"provider_reference"`
	IdempotencyKey    string          `json:"idempotency_key"`
	// Mode is the claim mode, "dispatch" or "reconcile" (A10-D1). It is ephemeral per claim (never
	// marshalled, never stored) and is set before Check and before every callback, so a Check for a
	// query-only reconcile can skip dispatch-only rules. Routes that ignore it behave as before.
	Mode string `json:"-"`
}

// DispatchRoute sets either Dispatch, or the LoadSecret + DispatchWithSecret pair (both or
// neither; meta-claims-intake-v1 §6.4). LoadSecret runs after the final gate, in one
// DBTimeout-bounded dispatcher transaction that ends before the callback; its only allowed body is
// one lease-fenced SQL loader. It runs in dispatch mode, and in reconcile mode only when the route
// sets ReconcileWithSecret (meta-ads-v1 A-10), fenced on the reconcile claim. Exactly one of
// Reconcile / ReconcileWithSecret is set; ReconcileWithSecret needs the LoadSecret pair. Check and
// plain Reconcile never see a Secret.
//
// Finish (optional, taiwan-cvs-logistics-v1 R-7a) runs inside completeOperation's DBTimeout-bounded
// transaction before Service.Complete, on every completion path including BLOCKED_POLICY and
// UNKNOWN; its SecretClaim carries the lease fence and claim mode for a lease-fenced SQL writer.
// An error or panic rolls the whole transaction back and the dispatcher returns
// errCompletionUncertain (the operation stays DISPATCHING and the next claim reconciles), so a
// Finish must be total over provider data: an error is reserved for database faults.
type DispatchRoute struct {
	Provider            string
	Action              string
	Purpose             string
	Check               func(context.Context, DispatchRequest) error
	Dispatch            func(context.Context, DispatchRequest) (Outcome, error)
	LoadSecret          func(context.Context, pgx.Tx, SecretClaim) (Secret, error)
	DispatchWithSecret  func(context.Context, DispatchRequest, Secret) (Outcome, error)
	Reconcile           func(context.Context, DispatchRequest) (Outcome, error)
	ReconcileWithSecret func(context.Context, DispatchRequest, Secret) (Outcome, error)
	Finish              func(context.Context, pgx.Tx, SecretClaim, Outcome) error
}

type DispatcherOptions struct {
	LeaseSeconds   int
	DBTimeout      time.Duration
	CallTimeout    time.Duration
	RetryDelay     time.Duration
	MaxGenerations int64
}

func DefaultDispatcherOptions() DispatcherOptions {
	return DispatcherOptions{
		LeaseSeconds:   30,
		DBTimeout:      2 * time.Second,
		CallTimeout:    10 * time.Second,
		RetryDelay:     5 * time.Second,
		MaxGenerations: 10,
	}
}

type dispatchRouteKey struct {
	provider string
	action   string
	purpose  string
}

// Dispatcher is a River worker for external_operation_v1. The caller retains
// ownership of its pool and River client lifecycle.
type Dispatcher struct {
	river.WorkerDefaults[externalOperationArgs]
	pool    *pgxpool.Pool
	routes  map[dispatchRouteKey]DispatchRoute
	options DispatcherOptions
	service Service
}

var _ river.Worker[externalOperationArgs] = (*Dispatcher)(nil)

func NewDispatcher(ctx context.Context, pool *pgxpool.Pool, routes []DispatchRoute, options DispatcherOptions) (*Dispatcher, error) {
	if ctx == nil || !validDispatcherOptions(options) {
		return nil, errInvalidJob
	}
	compiled, err := compileDispatchRoutes(routes)
	if err != nil {
		return nil, err
	}
	lease := time.Duration(options.LeaseSeconds) * time.Second
	for _, route := range compiled {
		// The secret load adds one more DB-bounded step before the provider call.
		if route.LoadSecret != nil && options.CallTimeout+3*options.DBTimeout+time.Second >= lease {
			return nil, errInvalidJob
		}
	}
	if err := platform.ValidateWorkerPool(ctx, pool); err != nil {
		return nil, err
	}
	return &Dispatcher{pool: pool, routes: compiled, options: options}, nil
}

func validDispatcherOptions(options DispatcherOptions) bool {
	if options.LeaseSeconds < 5 || options.LeaseSeconds > 300 ||
		options.DBTimeout < 100*time.Millisecond || options.DBTimeout > 5*time.Second ||
		options.CallTimeout < 100*time.Millisecond || options.CallTimeout > 60*time.Second ||
		options.RetryDelay < 100*time.Millisecond || options.RetryDelay > 5*time.Minute ||
		options.MaxGenerations < 2 || options.MaxGenerations > 100 {
		return false
	}
	lease := time.Duration(options.LeaseSeconds) * time.Second
	return options.CallTimeout+2*options.DBTimeout+time.Second < lease
}

func compileDispatchRoutes(routes []DispatchRoute) (map[dispatchRouteKey]DispatchRoute, error) {
	if len(routes) < 1 || len(routes) > 64 {
		return nil, errInvalidJob
	}
	compiled := make(map[dispatchRouteKey]DispatchRoute, len(routes))
	for _, route := range routes {
		plain := route.Dispatch != nil && route.LoadSecret == nil && route.DispatchWithSecret == nil
		secret := route.Dispatch == nil && route.LoadSecret != nil && route.DispatchWithSecret != nil
		// A-10: exactly one reconcile callback; the secret variant needs the loader pair.
		reconcile := (route.Reconcile != nil) != (route.ReconcileWithSecret != nil) &&
			(route.ReconcileWithSecret == nil || secret)
		if !validProvider(route.Provider) || !actionPattern.MatchString(route.Action) || !validPurpose(route.Purpose) ||
			route.Check == nil || !(plain || secret) || !reconcile {
			return nil, errInvalidJob
		}
		key := dispatchRouteKey{provider: route.Provider, action: route.Action, purpose: route.Purpose}
		if _, exists := compiled[key]; exists {
			return nil, errInvalidJob
		}
		compiled[key] = route
	}
	return compiled, nil
}

func (d *Dispatcher) Timeout(*river.Job[externalOperationArgs]) time.Duration {
	if d == nil {
		return 0
	}
	return time.Duration(d.options.LeaseSeconds) * time.Second
}

func (d *Dispatcher) NextRetry(*river.Job[externalOperationArgs]) time.Time {
	if d == nil {
		return time.Time{}
	}
	return time.Now().UTC().Add(d.options.RetryDelay)
}

func (d *Dispatcher) Work(ctx context.Context, job *river.Job[externalOperationArgs]) error {
	if d == nil || job == nil || job.Args.Version != 1 || !command.ValidID(job.Args.OperationID) {
		return errInvalidJob
	}
	return d.runOperation(ctx, job.Args.OperationID)
}

func (d *Dispatcher) runOperation(ctx context.Context, operationID string) error {
	if d == nil || d.pool == nil || !command.ValidID(operationID) {
		return errInvalidJob
	}
	operation, err := d.readOperation(ctx, operationID)
	if err != nil {
		return d.safeDatabaseError(ctx, err)
	}
	if terminalOperationState(operation.State) {
		return nil
	}
	if operation.Generation >= d.options.MaxGenerations && operation.ResultCode == "reconcile_budget_exhausted" && operation.LeaseUntil == nil {
		return river.JobCancel(errBudgetExhausted)
	}

	key := dispatchRouteKey{provider: operation.Provider, action: operation.Action, purpose: operation.Purpose}
	route, ok := d.routes[key]
	if !ok {
		return errRouteMissing
	}

	claim, operation, err := d.claimOperation(ctx, operationID)
	if err != nil {
		return d.safeDatabaseError(ctx, err)
	}
	switch claim.Disposition {
	case "busy":
		return river.JobSnooze(d.options.RetryDelay)
	case "terminal":
		if operation.State == "STALE_BINDING" {
			return river.JobCancel(errBindingBlocked)
		}
		return nil
	case "blocked_binding":
		return river.JobCancel(errBindingBlocked)
	case "claimed":
		if claim.Mode != "dispatch" && claim.Mode != "reconcile" {
			return errDatabase
		}
	default:
		return errDatabase
	}

	// A previous max-generation attempt may have died before persisting its
	// terminal queue disposition. This generation is cleanup-only.
	if operation.Generation > d.options.MaxGenerations {
		return d.completeAndFinish(ctx, operation, claim, Outcome{
			State:             "UNKNOWN",
			Code:              "reconcile_budget_exhausted",
			ProviderReference: operation.ProviderReference,
		}, true)
	}

	callCtx, cancel := context.WithTimeout(ctx, d.options.CallTimeout)
	defer cancel()
	checkErr, checkPanicked := invokeCheck(callCtx, route.Check, dispatchRequestMode(operation, claim.Mode))
	if ctx.Err() != nil {
		return river.JobSnooze(d.options.RetryDelay)
	}
	if callCtx.Err() != nil {
		return d.completeAmbiguous(ctx, operation, claim, "callback_timeout")
	}
	if checkPanicked {
		return d.completeAmbiguous(ctx, operation, claim, "callback_panic")
	}
	if checkErr != nil {
		if claim.Mode == "dispatch" && errors.Is(checkErr, ErrPolicyDenied) {
			// A10-D2: a coded denial records its own code; a bare ErrPolicyDenied stays "policy_denied".
			return d.completeAndFinish(ctx, operation, claim, Outcome{State: "BLOCKED_POLICY", Code: denialCode(checkErr)}, false)
		}
		return d.completeAmbiguous(ctx, operation, claim, "policy_check_failed")
	}

	gate, err := d.finalDispatchGate(callCtx, operation, claim)
	if ctx.Err() != nil {
		return river.JobSnooze(d.options.RetryDelay)
	}
	if err != nil {
		if callCtx.Err() != nil {
			return d.completeAmbiguous(ctx, operation, claim, "callback_timeout")
		}
		return d.completeAmbiguous(ctx, operation, claim, "dispatch_gate_failed")
	}
	if !gate.frozen || !gate.lease {
		return errCompletionUncertain
	}
	if !gate.binding {
		if claim.Mode == "dispatch" {
			return d.completeAndFinish(ctx, operation, claim, Outcome{State: "BLOCKED_POLICY", Code: "binding_changed"}, false)
		}
		return d.completeAmbiguous(ctx, operation, claim, "binding_changed")
	}

	request := dispatchRequestMode(operation, claim.Mode)
	var outcome Outcome
	var callbackErr error
	var callbackPanicked bool
	if withSecret := secretCallback(route, claim.Mode); withSecret != nil {
		secret, loadErr, loadPanicked := d.loadSecret(callCtx, route, operation, claim)
		if ctx.Err() != nil {
			secret.zero()
			return river.JobSnooze(d.options.RetryDelay)
		}
		if callCtx.Err() != nil {
			secret.zero()
			return d.completeAmbiguous(ctx, operation, claim, "callback_timeout")
		}
		if loadErr != nil || loadPanicked {
			// Dispatch denial: no credential means zero provider calls, so BLOCKED_POLICY is truthful.
			// Anything else (and every reconcile-mode failure) stays UNKNOWN: only Reconcile follows,
			// never a re-POST, because an effect may already exist upstream.
			code, blocked := secretLoadFailure(claim.Mode, loadErr, loadPanicked)
			if blocked {
				return d.completeAndFinish(ctx, operation, claim, Outcome{State: "BLOCKED_POLICY", Code: code}, false)
			}
			return d.completeAmbiguous(ctx, operation, claim, code)
		}
		outcome, callbackErr, callbackPanicked = invokeSecretOutcome(callCtx, withSecret, request, secret)
		secret.zero()
	} else if claim.Mode == "dispatch" {
		outcome, callbackErr, callbackPanicked = invokeOutcome(callCtx, route.Dispatch, request)
	} else {
		outcome, callbackErr, callbackPanicked = invokeOutcome(callCtx, route.Reconcile, request)
	}
	if ctx.Err() != nil {
		return river.JobSnooze(d.options.RetryDelay)
	}
	if callCtx.Err() != nil {
		return d.completeAmbiguous(ctx, operation, claim, "callback_timeout")
	}
	if callbackPanicked {
		return d.completeAmbiguous(ctx, operation, claim, "callback_panic")
	}
	if callbackErr != nil {
		return d.completeAmbiguous(ctx, operation, claim, "callback_failed")
	}
	if !validAdapterOutcome(outcome) {
		return d.completeAmbiguous(ctx, operation, claim, "adapter_result_invalid")
	}
	if (outcome.State == "UNKNOWN" || outcome.State == "ACKNOWLEDGED") && outcome.ProviderReference == "" {
		outcome.ProviderReference = operation.ProviderReference
	}
	exhausted := operation.Generation >= d.options.MaxGenerations &&
		(outcome.State == "UNKNOWN" || outcome.State == "ACKNOWLEDGED")
	if exhausted {
		outcome.Code = "reconcile_budget_exhausted"
	}
	return d.completeAndFinish(ctx, operation, claim, outcome, exhausted)
}

// loadSecret runs route.LoadSecret inside one DBTimeout-bounded transaction that has ended when it
// returns. Any error leaves an empty Secret; a loader that panics or fails to end its
// transaction never yields a usable credential.
func (d *Dispatcher) loadSecret(ctx context.Context, route DispatchRoute, operation Operation, claim ClaimResult) (secret Secret, err error, panicked bool) {
	bounded, cancel := context.WithTimeout(ctx, d.options.DBTimeout)
	defer cancel()
	tx, err := d.pool.BeginTx(bounded, pgx.TxOptions{})
	if err != nil {
		return Secret{}, err, false
	}
	defer d.rollback(tx)
	secret, err, panicked = invokeLoad(bounded, route.LoadSecret, tx, SecretClaim{
		OperationID: operation.ID, Generation: claim.Generation, LeaseToken: append([]byte(nil), claim.LeaseToken...),
		Mode: claim.Mode,
	})
	if err == nil && !panicked {
		if err = tx.Commit(bounded); err != nil {
			secret.zero()
		}
	}
	if err != nil || panicked {
		secret.zero()
		return Secret{}, err, panicked
	}
	return secret, nil, false
}

func (d *Dispatcher) safeDatabaseError(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		return river.JobSnooze(d.options.RetryDelay)
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return errOperationMissing
	}
	return errDatabase
}

func (d *Dispatcher) completeAmbiguous(ctx context.Context, operation Operation, claim ClaimResult, code string) error {
	outcome := Outcome{State: "UNKNOWN", Code: code, ProviderReference: operation.ProviderReference}
	exhausted := operation.Generation >= d.options.MaxGenerations
	if exhausted {
		outcome.Code = "reconcile_budget_exhausted"
	}
	return d.completeAndFinish(ctx, operation, claim, outcome, exhausted)
}

func (d *Dispatcher) completeAndFinish(ctx context.Context, operation Operation, claim ClaimResult, outcome Outcome, exhausted bool) error {
	if ctx.Err() != nil {
		return river.JobSnooze(d.options.RetryDelay)
	}
	if err := d.completeOperation(ctx, operation, claim, outcome); err != nil {
		return errCompletionUncertain
	}
	if exhausted {
		return river.JobCancel(errBudgetExhausted)
	}
	switch outcome.State {
	case "SUCCEEDED", "FAILED_FINAL":
		return nil
	case "BLOCKED_POLICY":
		return nil
	case "UNKNOWN", "ACKNOWLEDGED":
		return river.JobSnooze(d.options.RetryDelay)
	default:
		return errCompletionUncertain
	}
}

func (d *Dispatcher) readOperation(ctx context.Context, operationID string) (Operation, error) {
	bounded, cancel := context.WithTimeout(ctx, d.options.DBTimeout)
	defer cancel()
	return scanOperation(d.pool.QueryRow(bounded, operationQuery, operationID))
}

func (d *Dispatcher) claimOperation(ctx context.Context, operationID string) (ClaimResult, Operation, error) {
	bounded, cancel := context.WithTimeout(ctx, d.options.DBTimeout)
	defer cancel()
	tx, err := d.pool.BeginTx(bounded, pgx.TxOptions{})
	if err != nil {
		return ClaimResult{}, Operation{}, err
	}
	defer d.rollback(tx)
	claim, err := d.service.Claim(bounded, tx, operationID, d.options.LeaseSeconds)
	if err != nil {
		return ClaimResult{}, Operation{}, err
	}
	operation, err := scanOperation(tx.QueryRow(bounded, operationQuery, operationID))
	if err != nil {
		return ClaimResult{}, Operation{}, err
	}
	if err := tx.Commit(bounded); err != nil {
		return ClaimResult{}, Operation{}, err
	}
	return claim, operation, nil
}

func (d *Dispatcher) completeOperation(ctx context.Context, operation Operation, claim ClaimResult, outcome Outcome) error {
	bounded, cancel := context.WithTimeout(ctx, d.options.DBTimeout)
	defer cancel()
	tx, err := d.pool.BeginTx(bounded, pgx.TxOptions{})
	if err != nil {
		return err
	}
	defer d.rollback(tx)
	// R-7a: the route's local finish commits atomically with the completion or not at all.
	route := d.routes[dispatchRouteKey{provider: operation.Provider, action: operation.Action, purpose: operation.Purpose}]
	if route.Finish != nil {
		if err := invokeFinish(bounded, route.Finish, tx, SecretClaim{
			OperationID: operation.ID, Generation: claim.Generation, LeaseToken: append([]byte(nil), claim.LeaseToken...),
			Mode: claim.Mode,
		}, outcome); err != nil {
			return err
		}
	}
	if err := d.service.Complete(bounded, tx, operation.ID, claim.Generation, claim.LeaseToken, outcome); err != nil {
		return err
	}
	return tx.Commit(bounded)
}

func (d *Dispatcher) rollback(tx pgx.Tx) {
	cleanup, cancel := context.WithTimeout(context.Background(), d.options.DBTimeout)
	defer cancel()
	_ = tx.Rollback(cleanup)
}

type dispatchGate struct {
	frozen  bool
	binding bool
	lease   bool
}

func (d *Dispatcher) finalDispatchGate(ctx context.Context, operation Operation, claim ClaimResult) (dispatchGate, error) {
	bounded, cancel := context.WithTimeout(ctx, d.options.DBTimeout)
	defer cancel()
	var gate dispatchGate
	err := d.pool.QueryRow(bounded, `SELECT
		o.binding_id=$2 AND o.binding_version=$3 AND o.provider=$4 AND o.external_asset_id=$5,
		b.enabled AND b.semantic_version=o.binding_version AND b.provider=o.provider AND b.external_asset_id=o.external_asset_id,
		o.generation=$6 AND o.lease_mode=$7 AND o.lease_until>clock_timestamp()
		  AND o.lease_token_hash=sha256($8) AND o.state=CASE WHEN $7='dispatch' THEN 'DISPATCHING' ELSE 'UNKNOWN' END
		FROM integration.operations o
		JOIN integration.bindings b ON b.id=o.binding_id AND b.tenant_id=o.tenant_id AND b.store_id=o.store_id
		WHERE o.id=$1 AND o.actor_kind='MERCHANT'`, operation.ID, operation.BindingID, operation.BindingVersion, operation.Provider,
		operation.ExternalAssetID, claim.Generation, claim.Mode, claim.LeaseToken).Scan(&gate.frozen, &gate.binding, &gate.lease)
	return gate, err
}

const operationQuery = `SELECT id::text,tenant_id::text,store_id::text,principal_id::text,binding_id::text,
	binding_version,provider,external_asset_id,purpose,action,request,state,generation,lease_mode,lease_until,
	result_code,provider_reference FROM integration.operations WHERE id=$1 AND actor_kind='MERCHANT'`

type rowScanner interface {
	Scan(...any) error
}

func scanOperation(row rowScanner) (operation Operation, err error) {
	err = row.Scan(&operation.ID, &operation.TenantID, &operation.StoreID, &operation.PrincipalID, &operation.BindingID,
		&operation.BindingVersion, &operation.Provider, &operation.ExternalAssetID, &operation.Purpose, &operation.Action,
		&operation.Request, &operation.State, &operation.Generation, &operation.LeaseMode, &operation.LeaseUntil,
		&operation.ResultCode, &operation.ProviderReference)
	return operation, err
}

// dispatchRequestMode is dispatchRequest with the claim mode set (A10-D1); Check and the callback
// each get their own copy so neither can mutate what the other sees.
func dispatchRequestMode(operation Operation, mode string) DispatchRequest {
	request := dispatchRequest(operation)
	request.Mode = mode
	return request
}

func dispatchRequest(operation Operation) DispatchRequest {
	return DispatchRequest{
		OperationID: operation.ID, TenantID: operation.TenantID, StoreID: operation.StoreID,
		PrincipalID: operation.PrincipalID, BindingID: operation.BindingID, BindingVersion: operation.BindingVersion,
		Provider: operation.Provider, ExternalAssetID: operation.ExternalAssetID, Purpose: operation.Purpose,
		Action: operation.Action, Request: append(json.RawMessage(nil), operation.Request...),
		ProviderReference: operation.ProviderReference, IdempotencyKey: "lc:" + operation.ID,
	}
}

func invokeCheck(ctx context.Context, callback func(context.Context, DispatchRequest) error, request DispatchRequest) (err error, panicked bool) {
	defer func() {
		if recover() != nil {
			err, panicked = nil, true
		}
	}()
	return callback(ctx, request), false
}

func invokeOutcome(ctx context.Context, callback func(context.Context, DispatchRequest) (Outcome, error), request DispatchRequest) (outcome Outcome, err error, panicked bool) {
	defer func() {
		if recover() != nil {
			outcome, err, panicked = Outcome{}, nil, true
		}
	}()
	outcome, err = callback(ctx, request)
	return outcome, err, false
}

func invokeLoad(ctx context.Context, callback func(context.Context, pgx.Tx, SecretClaim) (Secret, error), tx pgx.Tx, claim SecretClaim) (secret Secret, err error, panicked bool) {
	defer func() {
		if recover() != nil {
			secret, err, panicked = Secret{}, nil, true
		}
	}()
	secret, err = callback(ctx, tx, claim)
	return secret, err, false
}

// errFinishPanicked replaces a Finish panic so no panic value reaches River's job errors.
var errFinishPanicked = errors.New("external_operation_finish_panicked")

func invokeFinish(ctx context.Context, callback func(context.Context, pgx.Tx, SecretClaim, Outcome) error, tx pgx.Tx, claim SecretClaim, outcome Outcome) (err error) {
	defer func() {
		if recover() != nil {
			err = errFinishPanicked
		}
	}()
	return callback(ctx, tx, claim, outcome)
}

func invokeSecretOutcome(ctx context.Context, callback func(context.Context, DispatchRequest, Secret) (Outcome, error), request DispatchRequest, secret Secret) (outcome Outcome, err error, panicked bool) {
	defer func() {
		if recover() != nil {
			outcome, err, panicked = Outcome{}, nil, true
		}
	}()
	outcome, err = callback(ctx, request, secret)
	return outcome, err, false
}

func validAdapterOutcome(outcome Outcome) bool {
	return (outcome.State == "SUCCEEDED" || outcome.State == "FAILED_FINAL" || outcome.State == "UNKNOWN" || outcome.State == "ACKNOWLEDGED") &&
		codePattern.MatchString(outcome.Code) && validReference(outcome.ProviderReference)
}

func terminalOperationState(state string) bool {
	return state == "SUCCEEDED" || state == "FAILED_FINAL" || state == "CANCELLED" || state == "BLOCKED_POLICY" || state == "STALE_BINDING"
}
