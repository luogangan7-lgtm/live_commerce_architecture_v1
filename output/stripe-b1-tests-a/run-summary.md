# Stripe B1 independent test candidate evidence

Base SHA: `529cb0fa18bed5476516ee34fd6154f89aeb5c2b`.
Role: independent `test_worker`; model `gpt-6-sol` high; branch `unit/stripe-b1-tests-a`.

| Tier | Command | Exit | Result |
| --- | --- | ---: | --- |
| REAL_PG | `bash scripts/dev/test-focused.sh '^TestStripeSP(06|02|19)'` | 1 | PASS=0, FAIL=3, SKIP=0; expected red on clean base without 0061 |
| UNIT | `go test -race -count=1 ./internal/integrations/psp/stripe/stripetest` | 0 | fake self-tests passed |
| UNIT | `go test -count=1 ./internal/integrations/psp/stripe/...` | 0 | adapter plus fake passed |
| STATIC | `go vet ./internal/integrations/psp/stripe/... ./tests/foundation/` | 0 | passed |
| COMPILE | `go test -run '^$' ./tests/foundation` | 0 | SP02/SP06/SP19 compiled |

The raw REAL_PG log is `red.log` beside this file. It records missing `payments.stripe_amount_ok`, migration `0061_stripe_psp.sql`, and the obsolete registrar signature in the pre-amendment SP19 draft; the obsolete registrar assertion was removed after this run. No further PG run was made pending the frozen §0.2 amendment.

Current SP06 is a provisional catalog and CHECK-negative skeleton. Two-store success and cross-binding rejection, complete effective RLS, all CHECK negatives, set-once trigger behavior, and queue guard behavior are NOT_RUN. SP19 covers only the SQL `REAL_LIVE` CHECK-negative. No SANDBOX, BROWSER or LIVE call was made.

## §0.2 behavioral candidate (2026-09-29)

New tests in `tests/foundation/stripe_schema_test.go` compile against the frozen interfaces:
`TestStripeSP06RegistrarIsolation`, `TestStripeSP06EndpointCustodyAndRLS`,
`TestStripeSP13EndpointPrepareLocksRotation`, and
`TestStripeSP13AcceptedReceiptNeedsCommittedLink`.
`go test -run '^$' ./tests/foundation`, `go vet ./tests/foundation`, and
`go test -count=1 ./internal/integrations/psp/stripe/stripetest` each exited 0.
REAL_PG execution of these new tests is **NOT_RUN** while the SQL writer owns the machinewide
PG slot. The accepted-receipt test exercises the deferred final-row invariant using an owner
fixture; the full prepare → River InsertTx → commit positive path is NOT_RUN. Historical
attempt processing, every §6.1 CHECK negative, and full queue guard routing remain NOT_RUN.

## SP06 test repair after root PG run (2026-09-29)

Root's independent second REAL_PG run is recorded at
`/Volumes/data/output/stripe-b1-root-schema-second.log`: 6 top-level PASS, 1 FAIL,
0 SKIP, exit 1. The remaining failure exposed test-fixture errors: PostgreSQL
normalizes interval literals in constraint text, and `LIKE INCLUDING CONSTRAINTS`
does not copy defaults. This candidate now copies DEFAULTS, proves a valid row
before each expected 23514 negative, and tests valid plus perturbed 40-minute
expiry, 7-minute send deadline, and 5-minute handoff cutoff behavior. It keeps
the no-PUBLIC-privilege assertions from commit `066d37e`.

`go test ./tests/foundation -run '^$' -count=1`, `go vet ./tests/foundation`,
and `git diff --check` exited 0. REAL_PG for this repair is **NOT_RUN** by the
test worker; root owns the machinewide PG slot. Pinned historical-session URL
handoff after API-key rotation is **NOT_RUN** here: the current fixture has no
session handoff setup, and that regression belongs to the later SP21 path.

## SP21 pinned historical handoff candidate (2026-09-29)

Root reports the preceding focused REAL_PG gate passed 7/7, with no failures or
skips. This candidate adds `TestStripeSP21PinnedHandoffAfterKeyRotation`:
registrar-created account/qualification/method, real scoped Stripe start and
River job, then an explicitly labeled owner-fixture pin of a synthetic open
Checkout Session. The hosted-role handoff must return the same URL before and
after registrar key rotation; attempt credential version stays 1, account head
becomes 2, and binding plus first-handoff timestamp remain unchanged. Profile,
config, forged-owner and revoked-qualification refusals are also asserted.
There is no provider call. Cutoff-time behavior remains **NOT_RUN** in this
targeted regression; the fixture cannot safely advance the DB clock.

`go test ./tests/foundation -run '^$' -count=1`, `go vet ./tests/foundation`,
and `git diff --check` exited 0. This new test's REAL_PG result is **NOT_RUN**
in the independent worktree, which has no SQL implementation; root owns the PG
slot and will run the combined candidate.

## SP11 exact signal-loader candidate (2026-09-29)

`TestStripeSP11SQLSignalLoader` now reuses the synthetic registrar/start fixture,
inserts two distinct `payment_signal_v1` jobs and BUYER signals in their own
transactions, then claims the Stripe operation before any worker signal read.
It asserts exact target rather than newest hint; mismatched job, signal,
operation, profile, NULL profile, token and generation are refused. Worker
direct-table SELECT and hosted-role function execution are denied. It also
checks River args/queue tampering is either blocked by its table guard or
refused by the loader, a consumed row remains readable for retry completion,
and a DB-expired lease cannot read material. Synthetic owner writes are
fixture setup only; no provider I/O occurs.

The new test's REAL_PG behavior is **NOT_RUN** in this worktree (SQL is on the
integrator branch, root holds the PG slot). The multi-statement
consume-plus-record atomic rollback case is **NOT_RUN** here; it belongs with
the runtime worker/reconcile test after its transaction interface exists.

## SP11/SP13 test-only repair after root combined PG (2026-09-29)

Root's combined log `/Volumes/data/output/stripe-b1-root-signal-loader-bundle.log`
records 7 top-level PASS, 2 FAIL, 0 SKIP, exit 1. Both failures were in
test mechanics: hosted-role `::regprocedure` resolution failed with 42501 on
the integration schema before the ACL predicate ran, and client cancellation
of a blocked endpoint rotation could race a successful server-side commit.
The test now resolves the loader OID through the owner fixture and asks both
roles `has_function_privilege(current_user, oid, 'EXECUTE')`. The endpoint
rotation test uses an explicit registrar transaction with server-side
`SET LOCAL lock_timeout='250ms'`, requires SQLSTATE 55P03, rolls back that
transaction before releasing ingress's lock, and verifies version 1 persisted
before asserting the subsequent 1→2 rotation. No security expectation or
timeout threshold was relaxed. The repaired candidate's REAL_PG result remains
**NOT_RUN** here; root owns the PG slot.
