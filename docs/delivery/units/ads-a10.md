# Unit ads-a10 — dispatcher A-10 amendment (reconcile-with-secret, `DispatchRequest.Mode`, coded denials, queue-aware job insert)

Role: integration_worker (mid tier). Base SHA `00c1d94`. Worktree `.worktrees/ads-a10`, branch
`unit/ads-a10`. No delegation, no network, no Meta/Stripe key. **Wave 1, first to merge (F1):** every
other ads unit codes against the FROZEN block below; ads-graph's worker cannot run until this is merged.
Contract: `contracts/meta-ads-v1.md` (FROZEN) §3.1, §0.1 A-10, §6.1 header, MA11; `contracts/
external-dispatcher-v1.md` amendment note (line ~153). Rulings: `docs/delivery/units/r2-design-rulings.md` X2.

**Goal:** existing routes behave byte-identically; ads routes can (a) reconcile with a lease-fenced
secret, (b) see the claim mode in `Check`, (c) record a specific BLOCKED_POLICY code, (d) enqueue on
their own River queue/priority.

## Read (by section; `grep -n`, `sed -n`)
PROCESS.md; contract §3.1. Code: `internal/integrations/core/dispatcher.go` (`DispatchRequest` :29–43,
`DispatchRoute` :49–58, route validation :96–148, `runOperation` :171–316 esp. :226 Check, :236–240
denial, :267–291 secret path, `loadSecret` :321), `secret.go`, `jobs.go` (`InsertOperationJob`),
`service.go` (`ClaimResult.Mode`, `codePattern`). Existing tests: `grep -ln LoadSecret internal/integrations/core/*_test.go`.

## Defaults adopted (gaps the frozen text left open; integrator may override)
- A10-D1 `Mode` is `json:"-"` (ephemeral per claim; no existing marshalled bytes change). Set on the
  request passed to `Check` **and** to every callback.
- A10-D2 **Coded denial** (needed for the contract's `over_allowance`, `pause_requested`,
  `sandbox_account_required`, `billing_restricted`, `consent_withdrawn`, `capi_principal_revoked`,
  `preflight_stale`, `account_not_ready`; today every Check denial is recorded as `policy_denied`):
  `DenyPolicy(code)` returns a `PolicyDenial` that `errors.Is(_, ErrPolicyDenied)`; in dispatch mode the
  dispatcher records `BLOCKED_POLICY` with `PolicyDenial.Code` when it matches `codePattern`, else
  `policy_denied`. Bare `ErrPolicyDenied` keeps `policy_denied` (existing routes unchanged).
- A10-D3 **Queue-aware insert** (contract §6 "queue `ads`, priority lane for pause"; `InsertOperationJob`
  hard-codes queue `default`, which `cmd/claims-worker` works — an ads op there hits
  `errRouteMissing`): `InsertOperationJobOn` = same args/kind, explicit queue + priority, no other opts.
  `InsertOperationJob` unchanged.
- A10-D4 In reconcile mode with `ReconcileWithSecret`: loader `ErrPolicyDenied` →
  `completeAmbiguous("credential_unavailable")`; any other loader error/panic →
  `completeAmbiguous("secret_load_failed")`; secret zeroed after the callback (§3.1 verbatim).

## FROZEN Go interface (ads-core, ads-graph, ads-capi, ads-tests call exactly these)
```go
package core // internal/integrations/core
// DispatchRequest gains (A10-D1):
//     Mode string `json:"-"` // "dispatch" | "reconcile"; = claim.Mode; set before Check and before every callback
// DispatchRoute gains (§3.1):
//     ReconcileWithSecret func(context.Context, DispatchRequest, Secret) (Outcome, error)
// Validation: exactly one of Reconcile / ReconcileWithSecret; ReconcileWithSecret only with the
// LoadSecret+DispatchWithSecret pair; lease inequality (:107) unchanged.
type PolicyDenial struct{ Code string }                 // Error() "policy denied: "+Code; Is(ErrPolicyDenied) == true
func DenyPolicy(code string) error                      // PolicyDenial{code}; invalid code → recorded as "policy_denied"
func InsertOperationJobOn(ctx context.Context, jobs *river.Client[pgx.Tx], tx pgx.Tx, operationID, queue string, priority int) (int64, error)
// queue ^[a-z][a-z0-9_]{0,39}$, priority 1..4, else command.ErrInvalid
```

## Build
1. `dispatcher.go`: fields + validation + reconcile-secret branch + Mode + coded denial. Reuse
   `loadSecret`/`invokeLoad`/`invokeSecretOutcome`; no second transaction shape. Update the
   `DispatchRoute` doc comment (it currently says "LoadSecret runs in dispatch mode only").
2. `jobs.go`: `InsertOperationJobOn`, comment names the River guard that must admit the queue
   (ads-core `post_river/0015`).
3. Unit tests (`dispatcher_a10_test.go`, names must not start with `TestMetaAdsMA`): Mode per claim;
   reconcile-with-secret loads via reconcile claim; loader denial/other error codes in reconcile mode;
   Check never receives a Secret (static: `Check` signature; runtime: fake route asserts); coded vs bare
   denial; route validation matrix; `InsertOperationJobOn` validation; every existing core test unchanged.

## Write paths
`internal/integrations/core/{dispatcher.go,jobs.go,dispatcher_a10_test.go}`, `output/ads-a10/**`.
Forbidden: every other file (incl. `service.go`, migrations, contracts, other `*_test.go` — changing an
existing assertion = stop and escalate).

## PROCESS §5 (applies to every file you touch)
Package comment unchanged except the new capabilities in one line each; every retry/UNKNOWN branch says
why it must not retry; comments say why/what it touches. No hand-written dependency lists
(`scripts/dev/depmap.sh` regenerates `docs/engineering/dependency-map.md`; run it and include the diff).

## Verify
```sh
GOTOOLCHAIN=go1.27.1 go vet ./internal/integrations/... && gofmt -l internal
GOTOOLCHAIN=go1.27.1 go test -race -count=1 ./internal/integrations/...
LC_FOCUSED_TIMEOUT=1800s bash scripts/dev/test-focused.sh '^Test(T06|MetaClaims|ExternalOperation|Accounts)'   # regression, existing routes byte-identical
bash scripts/dev/depmap.sh && python3 scripts/check_packet.py
```
Logs → `/Volumes/data/live_commerce_architecture_v1/output/ads-a10/` (command, SHA, exit, PASS/FAIL/SKIP).

## Gates
Implementer: unit tests above (MOCK). Independent: **MA11** (ads-tests) — do not claim it.

## Integrator hooks
None in shared files. Integrator merges F1 before ads-graph runs; records A10-D2/D3 as rulings in
`contracts/external-dispatcher-v1.md` (contracts are integrator-only).

## Return
Commit SHA, model/reasoning, base, paths, commands + exits + counts, evidence paths, before/after of
each changed doc comment, risks, NOT_RUN. Any change to an existing route's recorded outcome = stop.
