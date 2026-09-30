# Unit stripe-b1-sql — Stripe persistence (migrations 0061 + post-River 0012)

Role: implementer (integrator-owned artifact, delegated; integrator reviews and merges).
Worktree: `.worktrees/stripe-b1-sql`, branch `unit/stripe-b1-sql`.

## Goal
Implement exactly `contracts/stripe-psp-v1.md` §6 (6.1–6.5), the SQL parts of §7 (state
machines as CHECKs/transitions), the job kinds and routing of §8 that live in SQL/post-River,
§11 uniqueness, and `COMMENT ON` per §15 D9. Stage A (`internal/integrations/psp/stripe`) is
frozen and merged; do not change it.

## Read (in order; sections only)
1. `docs/delivery/PROCESS.md`
2. `contracts/stripe-psp-v1.md` §0.1, §4, §6, §7, §8, §11, §15
3. Pattern references (read by grep, not whole files): the newest payment migrations
   (`grep -ln 'payments\.' migrations/*.sql | tail -3`), `migrations/post_river/*.sql` for the
   routing/guard/readiness pattern, `migrations/migrate.go` for ordering/checksums.

## Write paths (only these)
- `migrations/0061_stripe_psp.sql`
- `migrations/post_river/0012_stripe_payment.sql`
- `internal/jobqueue/**` only if §8 requires registering a new job kind there
- `output/stripe-b1-sql/**` (evidence, gitignored)

## Must hold
- Every existing PAYUNi row/constraint keeps its current meaning (§6.1 widening, not rewrite).
  The renamed `apply_capture_payuni_v1` body must be byte-identical to its 0018 text (SP06).
- RLS FORCE, restrictive runtime policies, definer functions with pinned `search_path`, PUBLIC
  revoked, column privileges exactly as §6.3.
- LIVE qualification for stripe rejected in SQL (SP19 SQL part).
- `COMMENT ON` for every new table/column/function/role (PROCESS.md §5).

## Verify before returning
- `GOTOOLCHAIN=go1.27.1 go build ./... && go vet ./migrations/...`
- Migrations apply on a fresh DB and are idempotent under the runner: run
  `bash scripts/dev/test-focused.sh '^(TestPaymentQueue|TestPaymentCapture|TestPaymentQuery|TestPaymentStart|TestHostedPayment|TestBuyerPayment|TestMigrat)'`
  — every pre-existing payment gate must stay green (regression), and the run must show the
  new migrations applied (no checksum errors).
- Save the log to `output/stripe-b1-sql/regression.log`.

## Non-goals
Go business logic (`internal/payments`, `internal/checkout`), HTTP, workers, UI, SP06 test file
(the independent test author writes it). Do not edit tests to make them pass.

## Return
Changed files; commands + exit codes + PASS/FAIL/SKIP counts; any contract ambiguity you had
to resolve (quote the line, state your reading) — the integrator rules on those.
