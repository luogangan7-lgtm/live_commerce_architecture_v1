# Unit refund-fulfilment-tests — independent gates RF01–RF12, MF01–MF08, OP01, RF11/MF07 browser

Role: test_worker, independent. Write from the contracts and the FROZEN blocks of
`refund-core.md`, `fulfilment-core.md`, `owner-provisioning.md` only; do not read or copy those
units' branches. Worktree `.worktrees/refund-fulfilment-tests`, branch `unit/refund-fulfilment-tests`.
Author against frozen signatures at dispatch; compile/run after the integrator merges the cores
(F2) and 0065. No owner secrets, no production DSNs, no LIVE; SANDBOX only with test keys.

## Read (by section)
PROCESS.md; `contracts/stripe-refund-v1.md` §3–§9, §12, both ruling sections; `contracts/
manual-fulfilment-v1.md` §2–§6, rulings; refund-core "Defaults adopted" D1–D8 and fulfilment-core
E1–E6 (the tested behaviour where the contract was silent). Reuse by symbol: `stripetest` (`New`,
`Transport`, `SetNextFault`, `SetState`, `RequireAPIKey`, `SignWebhook`), foundation fixtures
(`fixture(t)`, `mustExec`, owner/runtime pools; grep `tests/foundation/{stripe_flow,stripe_start,
merchant_orders,payment_capture,identity_integration}_test.go`), browser harnesses
`tests/foundation/browser_merchant_orders_ui_test.go` + `scripts/dev/test-local.sh` modes,
`tests/storefront/*.mjs`, SP18 harness from `stripe-b2-browser-tests` once merged.

## Tier rules
REAL_PG/HTTP_PG = PG 18 via `test-focused.sh`. MOCK = real PG + real River clients in-process +
`payments.NewStripeRuntime(…,"PROVIDER_MOCK", srv.Transport())` + `payments.NewPaymentWorkerClient`
+ `stripetest`. Paid orders come from the real capture path (SP08 flow), never fabricated facts;
controlled SQL faults and aged timestamps via the owner pool are disclosed per test in evidence.
Merchants: creator via `create_initial_store` (0065 grants) plus a second member granted only
`orders:read` (and variants) explicitly. SANDBOX: `t.Skip("NOT_RUN: …")` unless `STRIPE_SANDBOX=1`.

## Gates (top-level names exact; file prefixes avoid helper collisions)
- **RF01** `TestStripeRF01Wire` (`stripe_refund_wire_test.go`, `srw`; UNIT part + PG parity): golden
  body bytes via `stripetest` capture, exact sorted keys, forbidden keys → `ErrInvalid`, byte-identical
  resend, `RefundIdempotencyKey` format, §5.6 classification per call, list 10-page cap → uncertain,
  §3.1 reports ≤2048 bytes/no ARN/card/receipt URL, `RefundAmountOK` ↔ `payments.stripe_refund_amount_ok`
  parity over a generated table (TWD %100).
- **RF02** `TestStripeRF02WebhookProjection` (`srw`): refund/charge projections; other objects
  unchanged; add refund/charge cases to `tests/payments/stripe-webhook-vectors.json` and run
  `node scripts/dev/stripe-webhook-check.mjs` (Node half).
- **RF03** `TestStripeRF03Schema` (`stripe_refund_schema_test.go`, `srs`): every §9 RF03 clause incl.
  §4.5 matrix by name/role/cmd/qual from `pg_policies`/ACLs (names per refund-core D5), runtime pool
  validator positive + River DELETE/state-UPDATE negative, D2 prepare signature, D1 op birth columns,
  behaviour through real definers.
- **RF04** `TestStripeRF04Request` (`stripe_refund_request_test.go`, `srq`): §9 row, real
  two-transaction race witness (`pg_blocking_pids`), zero ledger/reservation/order/work-item deltas.
- **RF05–RF07** `TestStripeRF05HappyMock`, `TestStripeRF06Unknown`, `TestStripeRF07Lifecycle`
  (`stripe_refund_flow_test.go`, `srf`): §9 rows + D3 (second list call, then `REFUND_UNRESOLVED`);
  fake asserts ≤1 distinct create key per refund and the pre-send charge read before every first POST.
- **RF08** `TestStripeRF08Webhook` (`stripe_refund_webhook_test.go`, `srh`): §9 row + D2 cap in prepare;
  Dashboard refund → zero `POST /v1/refunds` (fake counter); log + DB-wide sentinel scan.
- **RF09** `TestStripeRF09AdminHTTP` (`stripe_refund_http_test.go`, `sra`) via
  `httpapi.NewHandler(pool, httpapi.Options{RefundJobs: …})`: exact keys/codes, permission matrix,
  refresh throttle, merchant list `payment_state` precedence + `refunded_minor`/`refund_pending_minor`
  (E1), `stripe_refund_id` merchant-only, buyer `refund` absent for PAYUNi and no ids (D8), PAYUNi
  bytes == `tests/payments/stripe-sp14-payuni-*.golden.json`. BFF Node cases in
  `tests/admin/refund-bff.test.ts`.
- **RF10** `TestStripeRF10Sandbox` (`stripe_refund_sandbox_test.go`): §9 row incl. rotated-key same-key
  replay (RD13) and least RAK set; test PaymentIntent with `pm_card_visa` from the harness only (R-10).
- **RF12** `TestStripeRF12Guards` (`stripe_refund_guards_test.go`): old/new body sha256 + diff of every
  replaced B1 function shows only the stated delta; source guard (only `psp/stripe` dials Stripe); log
  forbidden-list scan. Full-suite regression and reviewer verdicts are the integrator's run.
- **MF01** `TestManualFulfilmentMF01Validation` (`manual_fulfilment_unit_test.go`, no PG) through
  `merchantorders.NormalizeShipment` and `WriteUnshippedCSV`: every §6 MF01 clause.
- **MF02–MF06** `TestManualFulfilmentMF02Schema`, `MF03Transitions`, `MF04Authority`, `MF05HTTP`,
  `MF06Export` (`manual_fulfilment_{schema,flow,http}_test.go`, prefix `mfs`/`mff`/`mfh`): §6 rows +
  E2 (`order-actions` booleans per grant), E3 (buyer head columns only), E4 (export audit GUCs),
  MD6 predicate identical across record / `unshipped` filter / export.
- **MF08** `TestManualFulfilmentMF08Guards`: `COMMENT ON` presence for 0063 objects; no provider dial,
  River insert or `integration.operations` write reachable from `merchantorders` shipment/export code.
- **OP01** `TestOwnerProvisioningOP01` (`owner_provisioning_test.go`): §12 exactly (set, replay,
  other members, pre-0065 principal not backfilled, body diff only the array).
- **Browser MF07** `tests/admin/manual-fulfilment.spec.ts` + storefront `tests/storefront/shipment-buyer.mjs`,
  driven by `tests/foundation/browser_refund_fulfilment_test.go` (`//go:build browser`,
  `TestBrowserManualFulfilment`) and a new `test-local.sh --browser-refund-fulfilment` mode: §6 MF07.
- **Browser RF11** `tests/admin/refund.spec.ts` + `tests/storefront/refund-buyer.mjs`,
  `TestBrowserRefund`: (a) MOCK variant — order paid through the fake Stripe + real worker, refunds
  through the fake; labeled MOCK; (b) SANDBOX variant after SP18 (4242) when `STRIPE_BROWSER=1` and
  `STRIPE_SANDBOX=1`, else NOT_RUN. Both: partial then full refund, buyer processing → refunded,
  member without `payments:refund` sees no action; 3 locales, desktop + 390px Chromium, screenshots hashed.

## Fake server
Extend `stripetest` for `/v1/refunds` (create/retrieve/list), `GET /v1/payment_intents/{id}?expand[]=
latest_charge`, refund/charge webhook events, pending/failed/canceled transitions, per-account key
isolation; written from Stripe docs + contract §1; keep its package comment and self-tests green.

## Write paths
`tests/foundation/{stripe_refund_*,manual_fulfilment_*,owner_provisioning,browser_refund_fulfilment}_test.go`,
`internal/integrations/psp/stripe/stripetest/**`, `tests/payments/stripe-webhook-vectors.json`,
`scripts/dev/stripe-webhook-check.mjs` (new cases only), `tests/admin/{refund,manual-fulfilment}.spec.ts`,
`tests/admin/refund-bff.test.ts`, `tests/storefront/{refund-buyer,shipment-buyer}.mjs`,
`scripts/dev/test-local.sh` (new mode only; integrator reviews), `output/refund-fulfilment-tests/**`.
Nothing else (not the cores' unit tests, not `apps/**`, not contracts).

## Verify
```sh
GOTOOLCHAIN=go1.27.1 go vet ./tests/foundation ./internal/integrations/psp/stripe/...
GOTOOLCHAIN=go1.27.1 go test -count=1 ./internal/integrations/psp/stripe/... && node scripts/dev/stripe-webhook-check.mjs
LC_FOCUSED_TIMEOUT=2400s bash scripts/dev/test-focused.sh '^Test(StripeRF(0[1-9]|12)|ManualFulfilmentMF0[1-68]|OwnerProvisioningOP01)'
bash scripts/dev/test-local.sh --browser-refund-fulfilment        # MF07 + RF11 MOCK
STRIPE_SANDBOX=1 bash scripts/dev/test-focused.sh '^TestStripeRF10'   # owner test key only; else NOT_RUN
```
Red proof per gate (PROCESS §2.4): after merge, one targeted mutation of the merged candidate in a
scratch copy (reverted) per gate → `output/refund-fulfilment-tests/red-<gate>.log`, then green log.
Compile failure is not a red run; zero matched tests or SKIP is never PASS.

## Order
Author at dispatch (third writer once a slot frees). Runs need F2 (refund-core + fulfilment-core
merged and mounted), OP01 needs 0065, storefront browser halves need stripe-b2-ui + the UI unit,
RF11 SANDBOX needs SP18 green.

## Return
Files; per gate the assertion list mapped to contract/§-default text; red + green logs with command,
SHA, exit code, PASS/FAIL/SKIP counts; NOT_RUN (RF10/RF11-SANDBOX without keys); every clause found
untestable or ambiguous in the contracts or the frozen briefs.
