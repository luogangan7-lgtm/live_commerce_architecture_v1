# Unit stripe-b1-tests-b — independent Stripe B1 gates SP07–SP15, SP21

Role: test_worker, independent. Write from the contract and the FROZEN blocks of
`stripe-b1-start-http.md` / `stripe-b1-ingress-assembly.md` only; do not read or copy those units'
branches. Worktree `.worktrees/stripe-b1-tests-b`, branch `unit/stripe-b1-tests-b`, base `b563bb5`.
Author now against frozen signatures; compile/run after the integrator merges both units (and
`stripe-b1-pool-fix`). No real Stripe, no owner secrets, no production DSNs.

## Read (by section)
PROCESS.md; contract §0.2 ("Corrected amount table and acceptance deltas" overrides §14 rows),
§4, §6.5, §7, §8, §9, §10, §11, §12 (log-forbidden list), §13, §14 rows SP07–SP15/SP21. Reuse
`stripetest` (`New`, `Transport`, `SetNextFault`, `SetState`, `RequireAPIKey`, `CreateKeys`,
`SignWebhook`), foundation fixtures (`fixture(t)`, `mustExec`, owner/runtime pools; grep
`tests/foundation/{hosted_payment,payment_runtime,stripe_handoff,stripe_signal_loader}_test.go`).

## Tier ruling applied (integrator may override)
MOCK = real PG 18 via `test-focused.sh` + real River clients started in-process + the real
assembly functions the binaries call (`checkout.NewHostedPaymentService`, `buyerhttp.New`,
`stripewebhook.NewInbox/NewHandler` behind `httptest`, `payments.NewStripeRuntime(…,"PROVIDER_MOCK",
srv.Transport())` + `payments.NewPaymentWorkerClient`) + `stripetest`. The binaries refuse
PROVIDER_MOCK by design, so binary-level runs are limited to SP15 startup/SIGTERM. Deadline tests
age stored timestamps through the owner pool (disclose every aged column/trigger in evidence);
no sleeps beyond seconds. Every account/method/endpoint row is seeded through `stripeadmin`
(registrar login + `mockTransport`) or disclosed owner fixtures, never merchant runtime.

## Gates (top-level test names exact; prefixes avoid collisions)
- **SP07** `TestStripeSP07Start` (`tests/foundation/stripe_start_test.go`, `sst`): every §14 SP07
  clause via `BeginHosted` stripe_checkout: atomic rows (order, reservation, attempt, op,
  `stripe_sessions` frozen params/deadlines, receipt, events, `payment_query_v1` job at now);
  replay; changed locale/config/profile/method → 409; different keys ≠ second attempt; concurrent
  PAYUNi vs Stripe prepare → one winner; mixed-provider lock workload → zero 40P01; each admission
  drift (disabled/hidden/stale method, revoked/expired qualification, MOCK evidence on SANDBOX,
  wrong environment, currency/amount not admitted incl. JPY, hold expired during waits); fake
  request count 0; rotation fences new starts until requalified.
- **SP08–SP12** `TestStripeSP08HappyMock`, `SP09CreateUnknown`, `SP10Deadline`,
  `SP11BuyerSignals`, `SP12MoneyChecks` (`tests/foundation/stripe_flow_test.go`, `sfl`): §14 rows
  with §0.2 deltas — SP08 ≥2 Stripe stores/accounts + one PAYUNi store on one profile queue,
  `RequireAPIKey` per account, handoff REDIRECT repeated with the same URL, signed webhook through
  the handler → CAPTURED/COMMITTED/CONFIRMED/READY, URL purged, on_hand unchanged; forged
  cross-account sessions, signals and receipts fail. SP09 fake asserts ≤1 distinct create key per
  attempt and no POST after LOCAL `unsent`. SP10 stock restored exactly, old 15-min expiry job
  STALE. SP11 over HTTP (`buyerhttp` refresh/cancel): throttle 10 s/≤30, `scheduled=false` leaves
  no job, NOOP after terminal, stale dropped, busy lease snoozes. SP12 each §6.5 rule; late paid
  after closure → exactly one CLOSURE_CONTRADICTED+REVIEW_REQUIRED obligation,
  PAID_ALLOCATION_FAILED, zero allocation/stock movement; replay in any order converges.
- **SP13** `TestStripeSP13WebhookHTTP` (`stripe_webhook_http_test.go`, `swh`): every §9.1
  status/limit with the frozen codes; route only `/v1/stripe/webhook/{uuid}` (no global route);
  owner-injected blocking lock/trigger proves no ACK before COMMIT and commit failure → 503;
  signature/endpoint mismatch, stale key_version after a committed rotation, missing linkage →
  no receipt/ACK; duplicate id with changed body → DUPLICATE, no job; every ignore/quarantine
  reason; malformed → 200 once; 33rd concurrent → 503; binding disable / API rotation do not block
  historical endpoint admission, endpoint disable does (404); wrong-profile then correct-profile
  admits exactly one signal; log capture + DB-wide scan find no signature/body/whsec/URL/email/name
  sentinel. Cite, do not duplicate, the existing SQL-level SP13 tests.
- **SP14** `TestStripeSP14BuyerHTTP` (`stripe_buyer_http_test.go`, `sbh`): prepare/handoff/refresh/
  cancel/view exact keys and enums; CREATING→REDIRECT (repeatable)→CLOSED/cutoff/UNAVAILABLE;
  foreign/other-store 404; strict no-body/no-key; PAYUNi responses byte-identical to goldens
  `tests/payments/stripe-sp14-payuni-*.golden.json` **captured on base `b563bb5` before brief 1
  merges** (commit them first; never regenerate to pass). Existing `TestBuyerPayment*` stay green.
  BFF Node tests: NOT_RUN until the buyer-payment-ui amendment.
- **SP15** `TestStripeSP15Process` (`stripe_process_test.go`, `spr`, PG): one PROVIDER_MOCK
  worker assembly consumes signed-mock PAYUNi and fake Stripe jobs on one queue; Stripe-disabled
  assembly never claims a Stripe op (job retained); scoped key admission / `GET /v1/account`
  mismatch fails before any checkout request; exact-version rotation; built `cmd/payment-worker`
  binary (SANDBOX, `COMMERCE_STRIPE_ENABLED=1`) prints `payment_worker_ready`, exits 0 on SIGTERM.
  No-PG halves in `cmd/{api,payment-worker,stripe-admin}/stripe_sp15_test.go` (same top-level
  name per package): disabled flags read only their flag; every config negative; env-sentinel
  getenv proves API/worker never read `STRIPE_*`/whsec and admin reads only per-subcommand vars;
  source guard (no `STRIPE_SECRET_KEY` literal outside `cmd/stripe-admin`, psp/stripe, tests).
  Ingress role both directions: cite `TestStripeAuthority*`.
- **SP21** `TestStripeSP21Registrar` (`stripe_registrar_test.go`, `srg`): via `stripeadmin.Open`
  with a registrar login: two stores/accounts register; second account per store, same account
  across stores, cross-store rebinding, non-owner principal → `ErrRejected`; API/webhook ciphertext
  purpose/AAD swaps fail to open; rotate blocks new starts until requalified while historical
  attempts keep processing with their version; qualify with a stale expected version after rotation
  rejected; SANDBOX evidence format `stripe-probe:<id>`; LIVE refused (CLI + SQL). SANDBOX probe
  part: `t.Skip("NOT_RUN: …")` unless `STRIPE_SANDBOX=1`.

## Fake server
Extend `stripetest` only where a gate needs it (e.g. accept `metadata[lc_probe]` for
`ProbeCheckout`, per-account key isolation), written from Stripe docs + contract §1; keep its
package comment (PROCESS.md §5) and self-tests green.

## Write paths
`tests/foundation/stripe_{start,flow,webhook_http,buyer_http,process,registrar}_test.go`,
`cmd/{api,payment-worker,stripe-admin}/stripe_sp15_test.go`,
`internal/integrations/psp/stripe/stripetest/**`, `tests/payments/stripe-sp14-*.golden.json`,
`output/stripe-b1-tests-b/**`. Nothing else (not `tests/foundation/stripe_authority_test.go`).

## Verify
```sh
GOTOOLCHAIN=go1.27.1 go vet ./tests/foundation ./cmd/... ./internal/integrations/psp/stripe/...
GOTOOLCHAIN=go1.27.1 go test -race -count=1 -run 'SP15' ./cmd/api ./cmd/payment-worker ./cmd/stripe-admin
GOTOOLCHAIN=go1.27.1 go test -count=1 ./internal/integrations/psp/stripe/...
LC_FOCUSED_TIMEOUT=1800s bash scripts/dev/test-focused.sh '^TestStripeSP(07|08|09|10|11|12|13|14|15|21)'  # serialized machine-wide
```
Red proof per gate (PROCESS.md §2.4): after merge, one targeted mutation in the merged candidate
(scratch copy, reverted) per gate → `output/stripe-b1-tests-b/red-SPxx.log`; then green log.
Compile failure before merge is not a red run. Zero matched tests or SKIP is never PASS.

## Return
Files; per gate the assertion list mapped to §14/§0.2 text; red + green logs with command, SHA,
exit code, PASS/FAIL/SKIP counts; NOT_RUN (SANDBOX probe, BFF Node); every clause found
untestable or ambiguous in the contract or the frozen briefs.
