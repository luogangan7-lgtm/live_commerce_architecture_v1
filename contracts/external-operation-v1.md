# T06 internal external-operation ledger v1

Status: INTERNAL_SLICE_ACCEPTED, 2026-09-20, code baseline `9bac8e4`. Contributes to I04/I06/I14/I20 and G02/G04; not a provider, public HTTP, payment or production gate. Independent bounded review and 106-test real-PG/race/vet evidence: [acceptance](../docs/implementation/2026-09-20-external-operation-acceptance.md). Full T06 remains IN_PROGRESS.

## Boundary and reuse

Go/pgx caller-owned transactions and the pinned River OSS client remain the only queue mechanism. No new dependency, queue engine, generic workflow DSL or secret store. `integration` is an internal module, not a microservice. The later runnable dispatcher is accepted separately in [external-dispatcher-v1.md](external-dispatcher-v1.md); the buyer checkout authority bridge remains unimplemented. A buyer is never impersonated as a merchant membership.

The initial producer is a trusted merchant-domain transaction with a server-resolved `platform.Scope`. A future checkout producer needs a separately reviewed buyer bridge; this slice gives buyer runtime/issuer no integration or River grants. Functions are not mounted on public routes. A typed Scope must match transaction-local tenant/store/principal, not just contain valid UUIDs.

## Data and permanent intent

- `integration.bindings`: internal UUID, tenant/store composite FK, provider identifier, external asset identifier, positive semantic version, enabled flag, creating merchant principal, DB timestamps. Provider/asset identity is immutable; an asset change requires a new binding. Enable/revoke changes increment semantic version. Secret renewal is not a semantic change and secrets are not stored here. This row is a binding reference, NOT evidence of provider permission or production eligibility.
- `integration.operations`: UUID; tenant/store/principal; binding ID and frozen semantic version; provider/asset snapshot; purpose (`transactional`, `service`, `marketing`); action; semantic key; SHA-256 of canonical frozen input; bounded JSON object; initial job ID; state, generation, lease mode/until; bounded result code and provider reference; created/updated DB timestamps. Immutable columns have no runtime/worker UPDATE grant. Composite binding/merchant FKs prevent cross-tenant/store assignment.
- `integration.operation_events`: append-only operation ID + tenant/store FK, generation, state/mode, bounded reason code, DB time. No raw provider errors, credential, address or message body.
- Semantic key is unique per tenant/store, independent of River retention or unique-job windows. Use domain-generated stable keys, not a new key for every HTTP attempt. The canonical input includes principal, binding/version, purpose, action, frozen request; a changed input is a conflict. Existing operation replay returns its original operation/job IDs, even after binding changes. This is historical readback, not dispatch permission.
- Frozen request is an internal object (at most 64 KiB serialized), containing domain IDs and the minimum immutable business values. No access tokens, raw address, unredacted conversation or authentication material. It is not an arbitrary public JSON endpoint. Future adapters own typed request schemas and privacy review.
- River job kind `external_operation_v1`; args EXACTLY `{operation_id, version: 1}`. No tenant authority or frozen payload in job args. The worker obtains scope from the operation row. The first operation, its event, receipt and `InsertTx` job commit or roll back together. No network call occurs in this transaction.

## Producer API

`RegisterBinding(ctx, tx, scope, token, key, provider, assetID)` creates version 1 enabled reference with command receipt/audit; it does not perform OAuth. `SetBindingEnabled(ctx, tx, scope, token, key, bindingID, expectedVersion, enabled)` uses CAS and bumps semantic version, including revoke/re-enable. Both call existing `platform.RequirePermission` for exact `integration:manage` inside the scoped transaction. There is no automatic existing-member grant/backfill. Test fixtures explicitly grant permissions; production authorization and UI remain separate work.

`Plan(ctx, tx, scope, token, key, input)` returns immutable `{operation_id, job_id}`. Require exact `integration:execute` with `platform.RequirePermission`. Input includes binding ID, expected binding version, action, purpose, request object. Validate scope, identifiers, size and canonical JSON before writes. Lock command key, then binding FOR SHARE; active/version must match for a new operation. Replays reuse the command receipt and permanent operation key. Caller rolls back on every error, including River insertion/event/audit/receipt failure. Existing `command.Run` is reused; no separate public idempotency framework.

`Get(ctx, tx, scope, token, operationID)` requires exact `integration:read` and is store-scoped historical readback; another tenant/store sees not found. Actor is provenance; an authorized same-store merchant can read. The public DTO/privacy policy is not defined here.

## Worker lease and state API

`commerce_worker` is an ordinary NOLOGIN role, not inherited by merchant, identity or buyer roles. A separately provisioned login passes `platform.OpenWorkerPool`'s exact-one-authority and privilege/object-owner checks. It can read integration projections and operate River's own schema; business state/event changes are EXECUTE-only through fixed `integration.claim_operation` and `integration.complete_operation`. A non-login/non-inherited `commerce_integration_writer` owns these SECURITY DEFINER functions with fixed pg_catalog search_path and no PUBLIC execution. No worker direct UPDATE/INSERT/DELETE on business tables, identity/capability authority, or catalog/inventory mutation. No database-owner connection may run a worker.

Worker functions accept caller-owned short transactions. Database EXECUTE grants verify worker authority. First perform an unlocked locator read of immutable binding ID, then lock binding FOR SHARE → operation FOR UPDATE and revalidate the composite association. Never hold locks across network I/O. Fixed SQL predicates use `clock_timestamp()` AFTER lock acquisition, not application time or transaction-start `now()`.

`Claim(ctx, tx, operationID, leaseSeconds)` permits lease 5–300 seconds. Go generates a crypto/rand 32-byte lease token, passes it to SQL (only SHA-256 is stored), and returns the raw token only with a committed-claim result to its caller, never a public DTO/log/job. Disposition, mode, generation and this owner token fence completion; reading generation/hash does not let a different claimant complete.

1. Active lease: `busy`, no write/no dispatch permission.
2. Terminal SUCCEEDED/FAILED_FINAL/CANCELLED/BLOCKED_POLICY/STALE_BINDING: `terminal`, no write.
3. READY with enabled binding and exact semantic version/provider/asset: increment generation; state DISPATCHING; mode `dispatch`; DB lease. This permits only an adapter's subsequent current policy check, NOT an unconditional external call.
4. Expired DISPATCHING, UNKNOWN or ACKNOWLEDGED: increment generation; state UNKNOWN; mode `reconcile`; new lease. Never return dispatch, even when the earlier process may have died before sending.
5. Disabled/changed binding: only a never-dispatched READY becomes terminal STALE_BINDING. An expired dispatch/UNKNOWN/ACKNOWLEDGED becomes or remains UNKNOWN with disposition `blocked_binding`, clears lease and prevents calls using the replacement account. Preserve the possible remote side effect. Repeated unchanged blocked-binding reads need not append events. A future authorized reconciliation route must query the frozen asset; no silent new-account fallback.

`Complete(ctx, tx, operationID, generation, leaseToken, outcome)` locks binding then operation, requires matching token digest AND generation AND unexpired lease, then atomically records state/event and clears lease. Base migration 0008 outcomes: SUCCEEDED, FAILED_FINAL, UNKNOWN, ACKNOWLEDGED; migration 0009 additionally accepts BLOCKED_POLICY only under the restricted dispatcher condition below. Result code is machine identifier 1–80 chars; provider reference <=200 chars, no arbitrary error text. Binding change during work does NOT erase an observed remote success: record the result for the frozen action plus event reason `completed_binding_changed`. Unknown stays UNKNOWN. Stale generation/expired lease returns conflict and writes nothing; an outcome arriving after lease loss needs future reconciliation, never a blind redispatch.

No automatic UNKNOWN→READY, no same-action redispatch, no retry under another semantic key, no cancellation-as-remote-reversal claim. ACKNOWLEDGED is not success. Query attempts can repeat under fresh leases but only query/reconcile, never execute. An adapter that proves an action was not performed and wants retry must add a reviewed policy/transition; v1 does not guess it.

Dispatcher extension: forward migration `0009_dispatch_policy_outcome.sql` also permits
BLOCKED_POLICY only with a valid `dispatch` lease and empty provider reference.
It does not allow reconciliation to erase a possible remote effect. Migration 0008
and the historical 106-test ledger acceptance remain unchanged; see the dispatcher
contract for callback ownership, timing, durable budget and later acceptance.

## Locking and recovery limits

- Producer lock order: existing command advisory key → binding row → allocate operation UUID → River job → operation insert → event/audit/receipt. The job cannot become visible before commit. Replay is read-only except repairing a missing command receipt from the immutable operation.
- Worker: binding FOR SHARE → operation FOR UPDATE. Binding mutation locks binding only. No worker takes producer command locks.
- All operation result writes compare generation; lease expiry alone rejects a result even before a successor claims. Claim/complete commit outcomes may be unknown: reread state/generation using the same operation ID. Never call an adapter from an uncommitted claim.
- River is at-least-once. Neither operation fencing nor Go cancellation prevents an old in-flight remote action. Stable provider idempotency keys and current adapter policy checks remain mandatory at the later dispatcher/provider boundary.

## Acceptance gates

Real isolated PostgreSQL, ordinary runtime + worker logins (not only mocks):

1. Successful Plan creates exactly one operation/job/event/receipt; rollback, forced River insertion failure and final audit failure leave zero facts.
2. Concurrent same-key Plan yields identical IDs and one job; changed body/actor/binding conflicts; permanent operation still deduplicates if its command receipt is absent. Job args contain exactly two safe fields.
3. Tenant/store/binding FK and RLS isolation; explicit integration permissions required; buyer/issuer cannot read/mutate integration; merchant cannot change operation state/generation or River execution state; worker cannot directly update any business state/intent/event or issue identities. Mixed-role/owner/superuser pools rejected.
4. Concurrent Claim gives exactly one dispatch lease; live duplicate is busy. Expired dispatch claim only reconciles. UNKNOWN/ACKNOWLEDGED never redispatch. Terminal duplicate does nothing.
5. Wrong owner token, old generation and elapsed lease cannot complete; latest valid completion writes one event. Binding revoke before READY blocks dispatch; post-dispatch binding change preserves known outcome/UNKNOWN and cannot redirect to a new asset.
6. Forced event insertion failure rolls back claim/completion. Transaction/lock cancellation does not leak scope or orphan partial state.
7. Full existing Go real-PG race + vet suite remains passing. Review by an agent other than the implementation author.

A real River probe worker using the ordinary restricted worker login has started, consumed its exact fixture job, persisted `completed`, and stopped under the test harness. This proves queue lifecycle privileges only, not an external-operation dispatcher.

Later internal dispatcher acceptance at `8e7c4d8` adds actual River consumption,
mock callback dispatch/query policy, bounded deadlines/retries, shutdown and real
cross-process crash recovery; [123-test evidence](../docs/implementation/2026-09-20-dispatcher-acceptance.md).
This supersedes the earlier dispatcher NOT_RUN status, not the historical probe's scope.

Still NOT_RUN: production adapters/eligibility/credentials and provider sandbox/live,
global quotas/durable Retry-After, inbox/webhook ingress, authorized cancellation/requeue
UI, buyer checkout, full global gates and full T06.

## Amendment by claims-retention-purge-v1 (integrator, 2026-09-30, U08 merge)

Recorded from `contracts/claims-retention-purge-v1.md` §6 (FROZEN 2026-09-30); that file is the source of the rows.

- Clause 6 (IR-3): a terminal `meta.private_reply` operation's `request.comment_ref` and `semantic_key` may be redacted
  by U08 only (`commerce_retention_writer`, `semantic_key LIKE 'mpr-%'`); `request_hash` stays the hash of the original
  request.
