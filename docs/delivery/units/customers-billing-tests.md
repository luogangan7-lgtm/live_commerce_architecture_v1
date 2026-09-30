# Unit customers-billing-tests — independent gates CB01–CB11, platform Stripe Billing fake

Role: test_worker (mid tier), independent. Write from `contracts/customers-billing-v1.md` and the FROZEN blocks +
defaults of `customers-core.md`, `billing-core.md`, `customers-billing-ui.md` only; never read or copy those units'
branches. Base `00c1d94`. Worktree `.worktrees/customers-billing-tests`, branch `unit/customers-billing-tests`.
Author at dispatch against frozen signatures; compile/run after F1 (0078) / F2 (cores + hooks merged). No owner
secrets, no production DSN, no LIVE; SANDBOX only with the platform test key (O-D).

## Read (by section)
PROCESS.md; contract §0 (CD1–CD8, BD1–BD8), §0.1, §1, §3–§8, §9 (U08/W1 lines), §12; the three briefs' frozen
blocks + D1–D14, B1–B12, U1–U9. Reuse by symbol: foundation fixtures (`fixture(t)`, `mustExec`, owner/runtime/buyer
pools; grep `tests/foundation/{owner_provisioning,manual_fulfilment_schema,stripe_refund_schema,
stripe_refund_request,buyer_checkout,live_claims_schema,identity_integration}_test.go`), paid orders via the real
capture path (SP08 flow) and refunds via refund-core fixtures, `stripetest.SignWebhook` (read-only import), browser
harness `tests/foundation/browser_refund_fulfilment_test.go` + `tests/admin/refund.spec.ts` shape.

## Tier rules
REAL_PG/HTTP_PG = PG 18 via `test-focused.sh`; roles are the real logins (CB06 window open **as the real
`commerce_runtime` login**). MOCK = real PG + `billing.New(…, fake.Transport())` + `billing.WebhookHandler` + the fake
below. Merchants: creator via `create_initial_store` (0079 grants) + a second member with explicit narrower grants.
Aged timestamps/controlled SQL faults via the owner pool, disclosed per test in evidence. SANDBOX:
`t.Skip("NOT_RUN: …")` unless `STRIPE_BILLING_SANDBOX=1`.

## Fake server (default T1: not `stripetest`)
`stripetest` sits under `internal/integrations/psp/stripe/**`, owned by stripe-live-enable this release, so CB08's
fake is a new package `internal/billing/billingtest` (package comment per PROCESS §5; written from Stripe docs F-B4..
F-B13, never from the adapter): `GET /v1/account`, `POST /v1/customers` (idempotency cache), `POST
/v1/checkout/sessions` + `/{id}/expire`, `POST /v1/billing_portal/sessions`, `GET /v1/subscriptions/{id}` + list
`?customer&status=all`, `GET /v1/prices/{id}`; per-request capture (method, path, sorted form body, headers),
fault injection (5xx, timeout), event builder signed with `stripetest.SignWebhook`; self-test file.

## Gates (top-level names exact; files `tests/foundation/customers_billing_<x>_test.go`, helper prefix per file)
- **CB01** `TestCustomersBillingCB01Unit` (`unit`, no PG): `billing.StandingOf` all 8 statuses + none + mixed +
  `incomplete`-only → UNBILLED; `customers.CurrentConsents` ordering/withdrawal table; export constants. Tier note
  (T2): PT412→402 and export schema/bounds are asserted through real handlers in CB06/CB09 (they need PG).
- **CB02** `TestCustomersBillingCB02Schema` (`cbs`): §8 row; owners, `proconfig`, ACLs, schema USAGE, FORCE RLS +
  policy names/quals from `pg_policies`, column-privilege matrix from `information_schema`; no R3 role referenced;
  CHECK contains the 3 values once (re-derivation idempotent); `create_initial_store` body diff vs 0065 = array only.
- **CB03** `TestCustomersBillingCB03List` (`cbl`): §8 row incl. forwarded link (G09 case 1), aggregates = facts,
  sentinel scan for actor_key/session id/PSP ref in every output, keyset on equal timestamps, `q` (D6).
- **CB04** `TestCustomersBillingCB04Consent` (`cbc`): §8 row incl. K/K2 stale-retry case, PT409s, owner-lock
  serialization witness (`pg_blocking_pids`), unknown body keys rejected, and `consent_allows` exactly per the frozen
  SQL comment (unknown owner/other store/invalid pair → false; works with no GUCs set).
- **CB05** `TestCustomersBillingCB05Erasure` (`cbe`): §8 row incl. `erased-`+32 hex with a pre-existing
  `erased-<8 hex>` label, actor-level rows byte-identical, buyer retry → 410 (D11), restore MODEL red (no ids) / green
  (ids, `via='restore'`) on an isolated fresh DB.
- **CB06** `TestCustomersBillingCB06Subscription` (`cbb`): §8 row; window open as real runtime login; standing
  matrix; `platform_account_conflict`; runtime lacks EXECUTE on `apply_subscription`/`store_standing`.
- **CB07** `TestCustomersBillingCB07Restricted` (`cbr`): §8 row; each after-sales action through its real path;
  `pg_proc.prosrc` catalog check over the five schemas.
- **CB08** `TestCustomersBillingCB08Mock` (`cbm`): §8 row + B4–B9 (body bytes, keys, no checkout key, one open session
  after parallel/retry, trial omitted with prior sub, livemode 400, forged/old signature 400, invoice parent
  resolution, retrieve failure 503, period fields, URLs absent from logs, `billing_ops_alert` on mismatch/duplicate).
- **CB09** `TestCustomersBillingCB09HTTP` (`cbh`) via the merged `httpapi.NewHandler` + buyer handler: every §5 row;
  permission matrix, cross-store 404 indistinguishable, strict bodies, key rules, attachment headers (D9), error codes
  (D3, B2 402), no secret/URL in errors. BFF Node cases in `tests/admin/customers-bff.test.ts`.
- **CB10** `TestCustomersBillingCB10Sandbox`: §8 row (test clock via API, `stripe listen` forward); key-gated.
- **CB11** browser: `tests/admin/customers-billing.spec.ts` + `tests/storefront/privacy-buyer.mjs`, driven by
  `tests/foundation/browser_customers_billing_test.go` (`//go:build browser`, `TestBrowserCustomersBilling`): §8 row
  + U2/U4/U6/U7/U8; zh-TW + en, desktop + 390px Chromium, screenshots hashed; billing redirect against the MOCK fake
  (labelled MOCK).
- **CB12** LIVE: NOT_RUN (Q1 entity, live keys, owner approval).

## Write paths
`tests/foundation/customers_billing_*_test.go`, `tests/foundation/browser_customers_billing_test.go`,
`internal/billing/billingtest/**`, `tests/admin/{customers-billing.spec.ts,customers-bff.test.ts}`,
`tests/storefront/privacy-buyer.mjs`, `output/customers-billing-tests/**`. Nothing else (not the cores' unit tests,
not `apps/**`, not `scripts/dev/test-local.sh`, not contracts).

## Verify
```sh
GOTOOLCHAIN=go1.27.1 go vet ./tests/foundation ./internal/billing/billingtest && GOTOOLCHAIN=go1.27.1 go test -count=1 ./internal/billing/billingtest
LC_FOCUSED_TIMEOUT=2400s bash scripts/dev/test-focused.sh '^TestCustomersBillingCB(0[1-9])'
bash scripts/dev/test-local.sh --browser-customers-billing     # after the integrator adds the mode (hook)
STRIPE_BILLING_SANDBOX=1 bash scripts/dev/test-focused.sh '^TestCustomersBillingCB10'   # platform test key only; else NOT_RUN
```
Red proof per gate (PROCESS §2.4): after merge, one targeted mutation of the merged candidate in a scratch copy
(reverted) → `output/customers-billing-tests/red-<gate>.log`, then green. Suggested reds: CB02 drop one policy; CB04
remove the owner `FOR UPDATE`; CB05 8-hex label; CB06 guard trigger invoker; CB07 add a `billing.` reference to a
checkout function; CB08 send an Idempotency-Key on checkout create. Compile failure, zero matched tests or SKIP is
never PASS.

## Integrator hooks (integrator only; unit ships `output/customers-billing-tests/test-local-mode.patch`)
- `scripts/dev/test-local.sh`: mode `--browser-customers-billing` (other R2 test units add modes too).
- CI workflow: include the new browser mode where the other browser modes run.

## Order
Author at dispatch (writer slot per AGENTS.md budget). PG gates need F1; HTTP/MOCK/browser need F2 of both cores +
UI integration; CB10 needs the operator prerequisites of contract §10.

## Return
Files; per gate the assertion list mapped to contract/brief-default text; red + green logs with command, SHA, exit
code, PASS/FAIL/SKIP counts; NOT_RUN (CB10 without key, CB12); every clause found untestable or ambiguous in the
contract or the frozen briefs (escalate, do not guess).
