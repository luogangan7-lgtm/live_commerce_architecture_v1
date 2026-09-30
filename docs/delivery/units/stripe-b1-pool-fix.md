# Unit stripe-b1-pool-fix — make the Stripe ingress/registrar pool gates admit correct logins

Role: implementer. Worktree `.worktrees/stripe-b1-pool-fix`, branch `unit/stripe-b1-pool-fix`.
Source: independent review of 91d48ae + 285f865 (2026-09-29), verdict FAIL-closed.

## Defects to fix (root cause, not symptom)
- **P1-A** `internal/platform/stripe_runtime.go:128-131` rejects any EXECUTE outside
  `pg_*`/`information_schema`, and `has_function_privilege` counts PUBLIC. River v0.40 creates
  `river_job_state_in_bitmask(bit,river_job_state)` in every River schema (`river`, `river_meta`,
  `river_payment`, `river_expiry`, `river_media`) with PUBLIC EXECUTE → every correct Stripe login
  is rejected. Fix in `migrations/migrate.go` after the River upgrade loop: `REVOKE EXECUTE ... FROM
  PUBLIC` on that helper in every River schema (IMMUTABLE, not definer, used only by River's
  own index predicate). Do **not** loosen the scan.
- **P1-B** River `JobInsertFastMany` uses `ON CONFLICT (unique_key) DO UPDATE SET kind =
  EXCLUDED.kind`; PG checks `UPDATE(kind)` at executor start. `migrate.go` already grants
  `UPDATE(kind)` to `commerce_runtime`/`commerce_checkout_runtime` for this reason (grep
  `UPDATE (kind)`). Grant `UPDATE(kind)` on `river_payment.river_job` to `commerce_stripe_ingress`
  (in the file that owns that grant today — post_river/0012 or migrate.go, follow the existing
  pattern) and let the gate accept exactly column `kind` UPDATE while still rejecting table-level
  UPDATE, any other column, DELETE, TRUNCATE. Confirm the existing job-family guard trigger blocks
  kind rewrites; fix the misleading comment "exactly River InsertTx's job".
- **P2-C** `internal/platform/platform.go:~277` `canReachPrivileged` ignores ADMIN option. Reject
  any `pg_auth_members` row where `member = session_user` and `admin_option` is true, in the shared
  validator (applies to every mode — confirm old modes' legitimate logins have no ADMIN option by
  grepping the fixtures/runbooks that create logins).
- **P2-D** `stripe_runtime.go:86` ABI lookup via `oidvectortypes`/`format_type` depends on
  `search_path`. Match on `pronamespace`/`proname` + `proargtypes = ARRAY[...]::regtype[]::oidvector`
  using `pg_catalog.`-qualified types, like the media check near `platform.go:345`.

## Read
`docs/delivery/PROCESS.md`; `contracts/stripe-psp-v1.md` §0.2 + §6.3 (grep); the functions above.

## Write paths
`internal/platform/platform.go`, `internal/platform/stripe_runtime.go`, `migrations/migrate.go`,
`migrations/post_river/0012_stripe_payment.sql` (only the UPDATE(kind) grant, if that is where it
belongs), `output/stripe-b1-pool-fix/**`. No test files (the test author owns them).

## Verify
- `go build ./... && go vet ./...`
- Regression of every pool mode: `bash scripts/dev/test-focused.sh
  '^(TestStripe|TestMeta.*(Runtime|Isolation|Pool)|TestPayment(Runtime|Worker)|TestLiveMedia.*Pool|TestPlatform)'`
  → save log `output/stripe-b1-pool-fix/regression.log`; must be 0 FAIL and >0 PASS.

## Return
Diff summary per defect, commands + exit codes + counts, and any old-mode login you found that
has ADMIN option (must be reported, not silently allowed).
