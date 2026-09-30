# T06 external-operation dispatcher v1

Status: INTERNAL_DISPATCHER_ACCEPTED, 2026-09-20, code baseline `8e7c4d8`.
Extends the accepted internal ledger in `external-operation-v1.md`; no new queue,
dependency or public endpoint. Independent bounded review, 123-test real-PG/race/vet
run, actual process crash recovery and browser regression are recorded in
[acceptance](../docs/implementation/2026-09-20-dispatcher-acceptance.md).
Real provider eligibility and full T06 remain separate gates.

## Code and authority boundary

`core.NewDispatcher(ctx, pool, routes, options)` validates the existing pool as
an ordinary, exact `commerce_worker` login using shared platform validation.
No owner, merchant, identity, buyer or mixed-authority pool is accepted. The
validator checks session_user authority and rejects session_user != current_user;
startup SET ROLE cannot disguise an owner login. It
returns a River worker for the existing `external_operation_v1` job, registered
with `river.AddWorker`; it owns neither the supplied pool nor River lifecycle.
There is no production command populated with a pretend provider adapter.

The constructor accepts a bounded `[]DispatchRoute` (1–64 entries), rejects
duplicate tuples, then builds its private copied map of exact provider/action/purpose tuples.
Each route has mandatory Check, Dispatch and Reconcile callbacks. These are
trusted server implementation, not scripts or URLs from a request/database.
Check validates typed frozen input and current provider authorization/purpose
eligibility on every attempt. Reconcile is query-only and may never issue the
original effect. Unknown provider or missing callback fails closed. No fallback
to another asset, provider, channel or tenant. A real provider adapter must have
separate policy, idempotency, timeout and privacy review before registration.

Callbacks receive a value snapshot of immutable operation fields, provider
reference and stable idempotency key derived from the operation UUID; never the
lease token/hash, tenant credentials, mutable configuration or caller-supplied
authority. Each callback receives its own copy of request bytes. Policy checks
cannot mutate the later dispatch payload. The operation UUID does not depend on
River attempt, generation, process, credential rotation or job retention.

## Attempt flow

1. Validate job version and operation ID. Read the persisted operation through
   worker authority; terminal duplicates succeed without callbacks. Missing route
   returns a fixed safe error without claiming; River's finite attempts then
   leave the READY operation visible for operator recovery, not a false success.
2. In a short bounded transaction call existing Claim, read frozen operation,
   then commit. A failed/uncertain commit MUST NOT call any callback. Busy jobs
   snooze; terminal jobs finish; blocked binding stops the queue job with the
   ledger's existing STALE_BINDING/UNKNOWN fact preserved.
3. Run Check under a bounded context. Immediately after Check, recheck current
   binding identity/version/enabled, generation/token and DB lease before I/O.
   All DB transactions have ended before callbacks. This is an observed gate,
   not an impossible promise to revoke an HTTP request already in flight.
4. Call Dispatch only for the committed `dispatch` claim; otherwise Reconcile.
   There is no UNKNOWN/ACKNOWLEDGED/expired-dispatch path back to Dispatch.
5. Complete in a separate bounded transaction using the existing SQL token and
   generation fence. Known success/failure is accepted only as the adapter's
   validated result; a network error, panic, malformed result or ambiguous timeout
   becomes UNKNOWN with a fixed code, never a fabricated remote rejection.
6. SUCCEEDED/FAILED_FINAL finish the River job. UNKNOWN/ACKNOWLEDGED snooze for
   query-only follow-up. A bounded number of claimed generations stops automatic
   reconciliation with UNKNOWN/ACKNOWLEDGED preserved and result/event code
   `reconcile_budget_exhausted` committed before cancelling the queue job. This
   code exposes manual-required state to the merchant ledger, without River read
   grants. Failed/uncertain Complete must retry/read back, not cancel blindly.
   Generation equal to the budget permits its last query; a terminal observed
   result still wins. An already exhausted result cancels without a new claim.
   A crash at the final generation may take one cleanup-only lease to persist
   exhaustion, but makes no further callback. Busy active leases are not cancelled
   as budget exhaustion. Queue completion/cancellation is not business success.

Explicit policy denial before first Dispatch records BLOCKED_POLICY with a
policy-denied code (no remote action occurred). Forward migration 0009 admits
this completion only with a valid `dispatch` lease and empty provider reference;
an expired dispatch/reconcile lease cannot erase a possible remote effect with
BLOCKED_POLICY. A denied Reconcile preserves
UNKNOWN, because a previous effect may exist. If shutdown cancels the context,
leave the persisted claim to expire and be reconciled; never detach provider I/O.
Callback errors and panic values are not propagated to River logs/job errors.
Only fixed machine codes escape the dispatcher. No raw request/DSN/token logs.

## Timing and limitations

Options bound DB timeout, total callback timeout, lease seconds, retry delay and
total claimed generations. One shared callback deadline covers Check, the final
DB gate and exactly one Dispatch/Reconcile (not separate full callback budgets).
Completion after callback timeout/panic uses a fresh bounded cleanup context;
parent shutdown cancellation instead leaves the lease to expire. Callback timeout plus two DB-write margins must fit
strictly inside the lease. Adapter HTTP clients must honor cancellation; fencing
cannot stop a non-cooperative adapter or remote in-flight action. No unsafe
goroutine timeout wrapper pretending to stop it. Global provider quotas, durable
Retry-After, inbox, UI manual recovery, lease renewal and retention rescue scanner
are not implemented by this slice and remain explicit follow-up gates.

River lifecycle uses existing Start/Stop/StopAndCancel. Crash recovery tests must
kill a real child process after a mock remote effect but before Complete, then
show a new worker queries the same frozen operation without repeating Dispatch.
Synthetic fixture clock/queue aging must be disclosed, not called elapsed time.

### Frozen Go surface

`DispatchRequest` contains OperationID, TenantID, StoreID, PrincipalID, BindingID,
BindingVersion, Provider, ExternalAssetID, Purpose, Action, Request (`json.RawMessage`),
ProviderReference and IdempotencyKey (`"lc:" + operation UUID`). No lease or job fields.
`DispatchRoute` contains Provider/Action/Purpose, `Check func(context.Context,
DispatchRequest) error`, and Dispatch/Reconcile with the same inputs and
`(Outcome, error)` returns. `ErrPolicyDenied` is the sole explicit denial marker.
Adapter outcomes cannot request BLOCKED_POLICY; only the dispatcher's own
pre-dispatch gate may produce it. Preserve a prior provider reference when an
ambiguous result has no replacement reference.

`DispatcherOptions` fields: LeaseSeconds int, DBTimeout/CallTimeout/RetryDelay
time.Duration, MaxGenerations int64. `DefaultDispatcherOptions()` returns
30 seconds / 2 seconds / 10 seconds / 5 seconds / 10 respectively. Require lease
5–300 seconds, DB timeout 100ms–5s, total call timeout 100ms–60s, retry delay
100ms–5m, generations 2–100, and CallTimeout + 2*DBTimeout + 1s < lease.
`NewDispatcher` returns `(*Dispatcher,error)`; unexported `runOperation(ctx,id)` is
called only by Work. No queue-bypassing manual execution API is introduced. Worker Timeout
is bounded/non-negative; registry and caller-owned pool are immutable/retained.

## Required gates

- Constructor rejects unsafe pools, duplicate/invalid route tuples, missing
  callbacks and options that can outlive the lease; no new dependency.
- Real River worker consumes a Plan-created job and persists operation/event
  plus River completion; callback sees committed claim from another connection.
- Busy/terminal duplicates do not repeat Dispatch. Uncommitted producer has no
  visible job. Policy denial and binding change during Check make zero calls.
- Frozen actor/asset/body/idempotency key preserved across attempts; deliberate
  Check mutation cannot change Dispatch. Exact tuple routing; no fallback.
- Error/panic/malformed adapter outcome persists UNKNOWN and only query follows.
  ACK is not success. Check denial after a possible effect does not erase it.
- Old generation, expired token and shutdown cannot overwrite a newer outcome.
  No open DB transaction during callback I/O; bounded cleanup and queue retries.
- Real child process termination after mock effect, then recovery: one dispatch,
  query-only reconciliation, same immutable UUID/key, final local persisted fact.
- Full real-PG/race/vet regression plus independent review; production adapters,
  exact-once remote effects, global quotas and full T06 not implied by these gates.

## Amendment proposal (meta-claims-intake-v1 round 1, 2026-09-29; APPROVED by integrator 2026-09-29 for meta-claims-intake-v1)

Proposed by the `meta-claims-intake-v1` contract author; approved by the integrator (IR-12
there, 2026-09-29). Nothing above changes until the meta-claims-intake unit implements it. Full text: `meta-claims-intake-v1.md` §6.4.
Summary: `DispatchRoute` gains an optional pair `LoadSecret(ctx, pgx.Tx, SecretClaim)
(Secret, error)` + `DispatchWithSecret(ctx, DispatchRequest, Secret)`. In `dispatch` mode only,
after the final gate, the dispatcher runs `LoadSecret` in one bounded transaction that ends
before the call; `SecretClaim` (operation, generation, lease token) reaches only `LoadSecret`,
whose sole body is a lease-fenced SQL loader. `Secret` is redacted in every formatter, zeroed
after use, and never passed to Check or Reconcile. `ErrPolicyDenied` from `LoadSecret` →
BLOCKED_POLICY `credential_unavailable`; any other error → UNKNOWN `secret_load_failed` with
zero calls. Lease inequality becomes `CallTimeout + 3*DBTimeout + 1s < lease`. Existing routes
are unaffected. Added gate: a secret is never observable by Check/Reconcile, and a loader
failure makes zero provider calls.

## Amendment note (meta-ads-v1 round 3, 2026-09-30; ruling X2 in `docs/delivery/units/r2-design-rulings.md`)

Additive, lands with the meta-ads A-10 unit: `DispatchRequest` gains `Mode string` (`"dispatch"|"reconcile"`),
set by the dispatcher from `claim.Mode` before `Check` (today `Check` runs for every claimed op, `dispatcher.go:226`,
and a reconcile-mode denial is `completeAmbiguous('policy_check_failed')`, `:236–240`). Ads/CAPI `Check` returns nil in
`reconcile` mode; existing routes ignore the field (no behaviour change). Full text: `meta-ads-v1.md` §3.1.

- 2026-09-30 (meta-ads brief ruling B13): A10-D2 Check denials carry a stable code recorded on the op; A10-D3 job insert names the route's queue. Additive; existing routes unchanged.
- Integrator rulings, merged 2026-09-30 (unit ads-a10, `1bf3535`; `internal/integrations/core`), additive, existing routes byte-identical:
  - A10-D1 `DispatchRequest.Mode` is `json:"-"` (ephemeral per claim, never marshalled); set on the request given to `Check` and to every callback.
  - A10-D2 `DenyPolicy(code)` returns `PolicyDenial{Code}` (`errors.Is(_, ErrPolicyDenied)`); in dispatch mode it records BLOCKED_POLICY with that code when it matches `codePattern`, else `policy_denied`. A bare `ErrPolicyDenied` keeps `policy_denied`. Reconcile-mode Check denials stay UNKNOWN `policy_check_failed` (unchanged).
  - A10-D3 `InsertOperationJobOn(ctx, jobs, tx, operationID, queue, priority)`: same args/kind as `InsertOperationJob`, explicit queue `^[a-z][a-z0-9_]{0,39}$` and priority 1..4, else `command.ErrInvalid`. `InsertOperationJob` stays on queue `default`. The `river_job` guard must admit the queue (ads-core `post_river/0015`).
  - A10-D4 reconcile with `ReconcileWithSecret`: loader `ErrPolicyDenied` → UNKNOWN `credential_unavailable`; other loader error/panic → UNKNOWN `secret_load_failed`; the secret is zeroed after the callback. Route validation: exactly one of `Reconcile`/`ReconcileWithSecret`, the latter only with the `LoadSecret`+`DispatchWithSecret` pair.
