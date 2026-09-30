# Unit cvs-tests — independent gates TCV01–TCV11, TCV14–TCV17 (+ TCV07/TCV10 SANDBOX, TCV08 BROWSER), ECPay fake

Role: test_worker, independent. Write from the contract and the FROZEN blocks/JSON of `cvs-ecpay.md` and
`cvs-core.md` only; do not read or copy those units' branches. Base SHA `00c1d94`, SHA recorded at dispatch.
Worktree `.worktrees/cvs-tests`, branch `unit/cvs-tests`. **Wave 1** author at dispatch; compile after cvs-ecpay
F1e, PG runs after cvs-core F1, flows after F2 + integrator mounts. No owner secrets, no production DSN, no LIVE;
ECPay stage keys only from env (`~/.config/livecommerce/secrets.env`), never in repo/logs/fixtures (the F6 vector's
published keys only as split literals, PROCESS §6).
Contract: `contracts/taiwan-cvs-logistics-v1.md` (v1 FROZEN 2026-09-30) §1 (F-facts the fake must honour), §4,
§5, §6, §7, §8, §9, §10 (every row is your spec), §12, §16 (all), §14 resolutions (what each gate re-proves).

## Read (by section)
PROCESS.md; the contract sections above; cvs-ecpay E1–E8 and cvs-core C1–C11 (the tested behaviour where the
contract was silent). Reuse by symbol: foundation fixtures (`fixture(t)`, `mustExec`, owner/runtime/checkout pool
logins, `platform.OpenCheckoutPool`; grep `tests/foundation/{manual_fulfilment_schema,manual_fulfilment_flow,
stripe_refund_request,owner_provisioning,external_operation_authority,local_recovery}_test.go`), paid orders via
the real capture path (SP08 flow, `stripetest`), River in-process clients, child-process worker pattern in
`local_recovery_test.go`, browser harness `tests/foundation/browser_refund_fulfilment_test.go` +
`scripts/dev/test-local.sh` mode blocks, `tests/storefront/*.mjs`, `playwright.config.ts`.

## Tier rules
REAL_PG/HTTP_PG = PG 18 via `test-focused.sh`, under the real role logins after a full Apply (never superuser
for the assertion; owner pool only for disclosed fault planting/aged timestamps, listed per test in evidence).
MOCK = real PG + real River + `core.NewDispatcher` with `ecpayroute.Routes(...)` + `ecpay.NewClient(env,
fake.Transport())`. Merchants: creator via `create_initial_store` plus members granted exactly `fulfillment:write`,
`orders:read`, `integration:manage`/`integration:read` variants. SANDBOX: `t.Skip("NOT_RUN: …")` unless
`ECPAY_LOGISTICS_SANDBOX=1` and the stage keys are set. Buyer PII in fixtures = synthetic sentinel names/phones only.

## Fake ECPay (`internal/integrations/shipping/ecpay/ecpaytest/**`)
Written from ECPay docs + contract §1 (not from cvs-ecpay's code): `New()`, `Transport()`, per-merchant keys,
map (fixed stage store), GetStoreList per `CvsType` (settable list; 2 MiB+ variant), Create (`1|…` MAC-valid, `0|msg`,
403, timeout, bad MAC, malformed, 41-char `CVSPaymentNo`, non-conforming `AllPayLogisticsID`), Query/V5 (found /
not-found / `LogisticsStatus` settable), print (HTML), `SignStatus(fields)` for status posts, counters: creates per
`MerchantTradeNo`, distinct trade nos, received `IsCollection`/`CollectionAmount`. Package comment + self-tests.

## Gates (top-level names exact; file prefixes avoid helper collisions)
- **TCV01** `TestEcpayMac` (`tests/integrations/ecpay/ecpay_mac_test.go`, UNIT): §10 row; F6 vector
  `692FD6E2…239AD`; .NET encode table; constant-time verify; tampered field/key fails; recipient + store-code golden
  tables in `tests/integrations/ecpay/testdata/{recipient,store_code}.json` (shared with TCV14/TCV15 SQL legs);
  trade no `^LC[A-Z2-7]{18}$` and Go ≡ SQL `fulfillment.ecpay_trade_no` over generated UUIDs (C2/E2).
- **TCV02** `TestTaiwanCvsSchema` (`taiwan_cvs_schema_test.go`, `tcs`): every §10 TCV02 clause incl. exact grant +
  `pg_policies` matrix (§4.3 + round-4 + X9 pool note), definer owner/`proconfig`/ACL, exactly one `begin_hold`
  after fresh install and after upgrade over 0063–0066 (C1 ordering), river negative for checkout_writer, no PII
  columns, DEALLOCATE CHECK/branch/guard/unique index, `COMMENT ON` presence for every 0072/0073/0017 object.
- **TCV03** `TestCvsSelectionFlow` (`taiwan_cvs_selection_test.go`, `tcl`), **TCV04** `TestCvsBeginGuards`
  (`taiwan_cvs_begin_test.go`, `tcb`), **TCV11** `TestCvsOptions` (same file), **TCV14** `TestCvsBuyerEnteredStore`
  (`taiwan_cvs_buyer_entered_test.go`, `tce`): §10 rows; two stores on two ACTIVE domains; kill-switch case (X9).
- **TCV05** `TestCvsShipmentLifecycle` (`taiwan_cvs_shipment_test.go`, `tsh`): §10 row incl. zero extra `river_job`
  on replay, `fulfillment:write`-only member, settle 1-hour rule, `ecpay_trade_found`, never a second Create on
  UNKNOWN (fake counter), real child-process kill + restart = one create, LIVE without flag, refund-before-dispatch,
  STALE_BINDING settle, member B settles/abandons A's attempt, Finish under the real roles, PII sentinel scan of
  operations/job args/events/logs.
- **TCV06** `TestCvsStatusIngress` (`taiwan_cvs_status_test.go`, `tst`): §10 row via the mounted hooks handler;
  `1|OK` only after COMMIT (fault injection), 503 for REQUESTED, size/dup/slow-body rejects, recipient never stored.
- **TCV15** `TestCvsPayAtPickupBegin`, **TCV16** `TestCvsCollectionStatus`, **TCV17** `TestCvsPayAtPickupRelease`
  (`taiwan_cvs_pay_at_pickup_test.go`, `tpp`): §10 rows incl. concurrency witnesses (`pg_blocking_pids`),
  ledger deltas exact, zero Stripe sessions/attempts/refund rows, revoked-member replay 403.
- **TCV09** `TestCvsNoIframeAndPrivacy` (`taiwan_cvs_guards_test.go`): CSP `frame-src` unchanged in both
  `next.config.ts`; source guard (only `ecpay` dials `*.ecpay.com.tw`; no `iframe`/`target=`/`window.open` in the
  picker); log/URL forbidden-list scan (keys, recipient sentinels, trade nos). Reviewer verdicts are not yours.
- **TCV07** `ecpay_sandbox_test.go` (`-tags sandbox`), **TCV10** `ecpay_okmart_probe_test.go` (`-tags sandbox`,
  expected FAIL on the directory leg, recorded as evidence) in `tests/integrations/ecpay/`; evidence →
  `output/taiwan-cvs/`. Status notification leg NOT_RUN in SANDBOX (F7).
- **TCV08** BROWSER: `tests/admin/taiwan-cvs.spec.ts` + `tests/storefront/cvs-buyer.mjs`, driven by
  `tests/foundation/browser_taiwan_cvs_test.go` (`//go:build browser`, `TestBrowserTaiwanCvs`) and a new
  `test-local.sh --browser-cvs` mode: MOCK variant (fake map/Create, signed MOCK status posts) always; SANDBOX
  variant (stage map + Stripe 4242) only with `ECPAY_LOGISTICS_SANDBOX=1 STRIPE_SANDBOX=1 STRIPE_BROWSER=1`, else
  NOT_RUN. Desktop + 390px Chromium; WebKit only if already installed locally (installing browsers = a download →
  NOT_RUN + ask). 3 locales, no iframe in DOM, two stores on two hosts with the `__Host-` cookie present after
  return, TCV14 buyer_entered UI half, screenshots hashed.

## Write paths
`tests/foundation/{taiwan_cvs_*,browser_taiwan_cvs}_test.go`, `tests/integrations/ecpay/**`,
`internal/integrations/shipping/ecpay/ecpaytest/**`, `tests/admin/taiwan-cvs.spec.ts`,
`tests/storefront/cvs-buyer.mjs`, `scripts/dev/test-local.sh` (new mode only; integrator reviews),
`output/cvs-tests/**`, `output/taiwan-cvs/**`. Nothing else (not the units' own tests, not `apps/**`, not contracts,
not existing tests — a needed change to an existing assertion goes to the integrator as a before/after).

## PROCESS §5
Test files start with a comment naming the gate, contract rows and the definers/routes exercised; fault planting
comments say which owner-pool write and why; wire constants in the fake carry docs URL + retrieval date.

## Verify
```sh
GOTOOLCHAIN=go1.27.1 go vet ./tests/... ./internal/integrations/shipping/...
GOTOOLCHAIN=go1.27.1 go test -count=1 ./internal/integrations/shipping/ecpay/ecpaytest ./tests/integrations/ecpay   # TCV01
LC_FOCUSED_TIMEOUT=2400s bash scripts/dev/test-focused.sh '^Test(TaiwanCvsSchema|Cvs(SelectionFlow|BeginGuards|ShipmentLifecycle|StatusIngress|Options|BuyerEnteredStore|PayAtPickupBegin|CollectionStatus|PayAtPickupRelease|NoIframeAndPrivacy))$'
bash scripts/dev/test-local.sh --browser-cvs                                            # TCV08 MOCK
ECPAY_LOGISTICS_SANDBOX=1 GOTOOLCHAIN=go1.27.1 go test -tags sandbox -count=1 ./tests/integrations/ecpay   # TCV07/TCV10; else NOT_RUN
```
Red proof per gate (PROCESS §2.4): after merge, one targeted mutation of the merged candidate in a scratch copy
(reverted) per gate → `output/cvs-tests/red-<gate>.log`, then the green log. Compile failure is not a red run;
zero matched tests or SKIP is never PASS.

## Integrator hooks
`test-local.sh` mode review/merge (auth-, ads-, customers-billing-tests also add modes); TCV08 deploy-smoke leg
(`/v1/cvs/ecpay/{map-return,status}/*` reach Go on `LC_HOOKS_HOST`, other `/v1/cvs/*` and `LC_API_HOST` → 404)
goes into the integrator's deploy smoke script; `contracts/tasks.json` T13 evidence.

## NOT_RUN / Return
NOT_RUN: TCV07/TCV10 without stage keys, TCV08 SANDBOX/WebKit variants, SANDBOX status leg (F7), TCV12/TCV13
(owner LIVE). Return files; per gate the assertion list mapped to contract/§-default text; red + green logs with
command, SHA, exit code, PASS/FAIL/SKIP counts; NOT_RUN; every clause found untestable or ambiguous in the contract
or the frozen briefs.
