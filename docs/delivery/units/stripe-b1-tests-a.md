# Unit stripe-b1-tests-a — independent Stripe fake server + schema gates

Role: independent test author (must NOT read or copy the implementation branch
`unit/stripe-b1-sql`; write from the contract).
Worktree: `.worktrees/stripe-b1-tests-a`, branch `unit/stripe-b1-tests-a`.

## Goal
1. `internal/integrations/psp/stripe/stripetest/` — the MOCK-tier fake Stripe HTTP server of
   `contracts/stripe-psp-v1.md` §14 (row MOCK): Checkout Session create/retrieve/expire/list,
   idempotency-cache semantics (param compare → `idempotency_error`, cached 500 replay,
   uncached validation/409), fault hooks (drop-after-execute, 429, delay), session state
   control (paid/expired/complete+unpaid/async), webhook signer (Stripe-Signature `t=,v1=`
   over `t.body` with a test whsec), `GET /v1/account`. Written from Stripe docs + contract §1,
   not from `internal/integrations/psp/stripe` code. Package comment per PROCESS.md §5.
2. `tests/foundation/stripe_schema_test.go` — gates **SP06** and the SQL half of **SP02**
   (`stripe_amount_ok`/`stripe_unit_amount` Go↔SQL parity) and the SQL half of **SP19**, exactly
   as the §14 table rows describe. Use existing foundation helpers (`fixture(t)`, `mustExec`,
   owner/runtime pools) — grep `tests/foundation/payment_*_test.go` for the pattern.
3. Unit self-test for the fake: `stripetest` must be exercised by a small `_test.go` proving
   idempotency replay, the param-mismatch error and signature verification with the frozen
   adapter's verifier (`stripe.VerifyWebhook` or whatever §5.8 names).

## Read
1. `docs/delivery/PROCESS.md`
2. `contracts/stripe-psp-v1.md` §1, §4, §5.4–5.8, §6 (to know what SP06 asserts), §14, §15
3. `tests/payments/stripe-webhook-vectors.json` (reuse vectors)

## Write paths
`internal/integrations/psp/stripe/stripetest/**`, `tests/foundation/stripe_schema_test.go`,
`tests/payments/stripe-*.json` (new files only), `output/stripe-b1-tests-a/**`.

## Verify before returning
- `go vet ./internal/integrations/psp/stripe/... ./tests/foundation/` and
  `go test -count=1 ./internal/integrations/psp/stripe/...` green.
- The SP06/SP02/SP19 foundation tests compile. They are EXPECTED to fail now (0061 is not
  merged into your branch) — run them once with `bash scripts/dev/test-focused.sh
  '^TestStripeSP(06|02|19)'` and save the red log to `output/stripe-b1-tests-a/red.log`; that is
  your "can fail" proof. Do not stub the schema to make them pass.

## Return
Files; commands + exit codes; list of every SP06 assertion you implemented mapped to the §14
row text; anything in the contract you found untestable or ambiguous.
