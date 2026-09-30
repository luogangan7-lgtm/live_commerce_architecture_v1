# Unit stripe-b1-authority-tests — independent Stripe ingress/registrar authority matrix

Role: independent test author (do not read `unit/stripe-b1-pool-fix` or its worktree; write from
the contract and the public Go API). Worktree `.worktrees/stripe-b1-authority-tests`, branch
`unit/stripe-b1-authority-tests`.

## Goal
`tests/foundation/stripe_authority_test.go` (REAL_PG) — gate the two pool constructors in
`internal/platform/stripe_runtime.go` (`OpenStripeIngressPool`, `OpenStripeRegistrarPool`, and
`ValidateStripeIngressPool` if exported) against `contracts/stripe-psp-v1.md` §0.2/§6.3:

Positive controls (must PASS after the pool-fix unit merges; these are the tests that were
missing, which is why two fail-closed P1s went unnoticed):
- A fresh LOGIN that inherits only `commerce_stripe_ingress` (INHERIT TRUE, SET FALSE, no ADMIN)
  opens the ingress pool, and through it can run the real River InsertTx of a
  `payment_signal_v1` job into `river_payment` (use the same River client/insert path production
  ingress uses — grep `payment_signal_v1` in internal/ for it) plus the §6.4 prepare/commit
  functions it is granted.
- Same for `commerce_payment_registrar` with the registry functions (`register_stripe_account`
  etc. — grep 0061 for the exact names).

Negative matrix (each must be rejected at pool open, with the fixed masked error): extra
membership in owner / worker / merchant runtime / registry writer / integration writer / any
Meta or media role / the other Stripe role; SET TRUE; ADMIN option on its own role; ADMIN on the
owner role (INHERIT FALSE, SET FALSE); BYPASSRLS; CREATEROLE; a direct table grant; a table-level
UPDATE or DELETE on `river_payment.river_job`; EXECUTE granted to PUBLIC on a non-catalog
function. Plus: old pool modes (grep existing `Open*Pool` tests) still reject a login that holds
either Stripe role.

Every login/role you create uses random names and is dropped in `t.Cleanup` (owner SQL).

## Read
`docs/delivery/PROCESS.md`; contract §0.2, §6.3, §9.1 (grep); `internal/platform/stripe_runtime.go`
exported API only (`grep -n '^func [A-Z]'`); an existing pool test for fixture style
(`grep -ln 'Open.*Pool' tests/foundation/*.go | head -3`).

## Write paths
`tests/foundation/stripe_authority_test.go`, `output/stripe-b1-authority-tests/**`.

## Verify
`go vet ./tests/foundation/` clean. Run `bash scripts/dev/test-focused.sh '^TestStripeAuthority'`
on your branch (pool-fix NOT merged): the positive controls are EXPECTED to FAIL — save that as
`output/stripe-b1-authority-tests/red.log` (the "can fail" proof). The integrator reruns after merge.

## Return
Test names, matrix rows → assertion mapping, red-run counts, anything untestable.
