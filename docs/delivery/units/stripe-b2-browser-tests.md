# Unit stripe-b2-browser-tests — independent SP18 / SU browser gates (R1-2)

Role: test_worker (mid tier), independent of `stripe-b2-ui` (never read its diff before your gates
record a red run). Base = release tip after `contracts/stripe-buyer-ui-v1.md` is FROZEN. Worktree
`.worktrees/stripe-b2-browser-tests`, branch `unit/stripe-b2-browser-tests`. No delegation.
Network: only the harness's tunnel to Stripe hosts (below). Evidence class: SU05/SU07–SU09 BROWSER
(local mock/no provider), SU06 = SP18 **SANDBOX**. Never LIVE.

## Read (by section)
`docs/delivery/PROCESS.md`; `contracts/stripe-buyer-ui-v1.md` (all); `contracts/stripe-psp-v1.md`
§0.1, §0.2 "Corrected amount table", §7, §9.2, §9.3, §14 (SP18 row, tier table, parsing rule);
`contracts/buyer-payment-ui-v1.md` "Gates". Code by symbol: `tests/foundation/browser_payment_chain_test.go`
(whole, 157 lines: fixture, `/facts` control server, env to Node), `tests/storefront/buyer-payment-browser.mjs`
(`startNext`, TLS `edge`, CONNECT `proxy`, `context`, `makeOrder`, `storageSafe`), `scripts/dev/test-local.sh`
(`--browser-payment` blocks ~L73, ~L121, ~L203), `tests/foundation/stripe_flow_test.go` (`sflNew`,
`startWith`), `tests/foundation/stripe_registrar_test.go` (`sandbox_probe_against_real_stripe`),
`internal/payments/stripeadmin/registrar.go` (`Open`, `Register`, `Qualify`, `SetMethod`, inputs),
`internal/payments/stripe_runtime.go` (`NewStripeRuntime`).

## Write paths
`tests/foundation/browser_stripe_test.go` (build tag `browser`, `TestBrowserStripeCheckout`),
`tests/storefront/stripe-browser.mjs` (SP18 names `stripe-browser.spec.ts`; follow the repo's
Go-driven `.mjs` pattern instead and note it in evidence), `tests/storefront/payuni-ui-baseline.mjs`
(SU05 capture), `scripts/dev/test-local.sh` (new `--stripe-browser` mode only; integrator reviews),
`output/stripe-b2-browser-tests/**`. Do not edit other Stripe Go/test files, `apps/**`, contracts.

## Harness: `bash scripts/dev/test-local.sh --stripe-browser`
- Requires `STRIPE_BROWSER=1` and `STRIPE_SANDBOX=1`, else prints `NOT_RUN: SP18 …` and exits 2.
- Read only `STRIPE_SECRET_KEY` from secrets.env in a subshell (`STRIPE_SECRET_KEY=$(set +x; . file;
  printf %s "$STRIPE_SECRET_KEY")`, then export only that name); never `set -a` the file. The Go test
  builds the Node/Next env by stripping `STRIPE_*` and every name defined in secrets.env, and a unit
  assertion proves the Node env has no `STRIPE_`-prefixed key or `sk_test_`/`rk_test_` value
  (`browserEnvironment` in `tests/foundation/browser_identity_chain_test.go` copies `os.Environ()`, so
  the Go side must do the stripping; shell `unset` before spawning is not enough).
- Asserts `STRIPE_SECRET_KEY` matches `^(sk|rk)_test_` via `[[ =~ ]]`, never printing it or its length.
  `STRIPE_ACCOUNT_ID` defaults to `acct_1UJDb0RusP6Wwj7e` (not a secret; the file does not define it);
  any other value → exit 2 (matches SP16).
- Runs `go test -race -tags browser -count=1 -timeout=900s -json` as **separate `-run` calls**, each with its
  own 900 s budget: `'^TestBrowserStripeCheckout$/^SP18'` (6 SANDBOX runs), then `.../^SU07`, then
  `.../^SU09` (MOCK). Record each call's measured duration; if SP18 alone nears 900 s, split it per
  case. Parse JSON: any SKIP, zero cases, missing log → FAIL (§14 parsing rule).
- Same task-owned PG container and cleanup as `--browser-payment`; prints
  `SANDBOX checkout.stripe.com test mode; no live charge; SP17 webhook NOT_RUN`.

## Seeding (Go, in-process; secrets never cross to Node)
1. Fixture store as `hpSetup` + `bhPublish(... "https://buyer.example" ...)`; store/market currency
   HKD (sandbox default) with a product priced ≥ HK$4.00; if the fixture cannot use HKD, TWD with
   whole-dollar prices. Record which in evidence.
2. `stripeadmin.Open(ctx, sstLogin(t, f, "commerce_payment_registrar"), keys, signing)` — **no mock
   transport**. `Register(scope, account, secret)` → `Qualify({ConnectionID, AccountID, SecretKey,
   Profile:"SANDBOX", Currency, ReturnURL:"https://buyer.example/payment/return", ExpectedVersion:1,
   AmountMinor:<min>})` → `SetMethod({… Enabled:true, Visible:true, Min/Max per §0.2 table,
   NameHans/NameHant/NameEN synthetic})`. No PAYUNi method on this store (single-method UI, Q3).
3. API: `checkout.NewHostedPaymentService(ctx, hostedPool, jobs, "SANDBOX", keys, HostedProviders{PAYUNi:&cfg,
   Stripe:&StripeHostedConfig{ReturnURL}})` + `buyerhttp.New`. Worker: exactly `startWith`'s
   assembly with `Profile:"SANDBOX"` and `NewStripeRuntime(ctx, pool, keys, "SANDBOX")` (no transport).
4. `/facts` control (same key scheme) returns counts only: orders, attempts, stripe sessions,
   CAPTURED/CLOSED_UNPAID facts, reservation states, available/reserved stock, signals. Never a URL,
   session id, key or email.
5. Cleanup: expire any still-open test session via the worker's cancel path before teardown; record.

## Browser driver (`stripe-browser.mjs`, Playwright Chromium, headless)
- Reuse the edge/proxy pattern. CONNECT allowlist: `buyer.example:443` → local edge; hosts matching
  `^([a-z0-9-]+\.)*(stripe\.com|stripe\.network|stripecdn\.com):443$` → real tunnel; everything else
  refused and **recorded** (e.g. a hosted-page captcha provider host); the run fails only if the flow
  cannot complete. `payuni.com.tw` route → throw.
- Hosted page: fill card, expiry `12/34`, CVC `123`, synthetic name/email `gate+<n>@example.com`,
  postal code if shown; leave any "save my info" unchecked. Selectors are observed, not contracted;
  record them. **If a CAPTCHA or bot challenge appears: `t.Fatal("BLOCKED: captcha")`, recorded as NOT_RUN(BLOCKED), never a silent SKIP; never solve it.**
- After the child lands on `/payment/return`, assert neutral body, close it, click **Refresh order**
  in the store tab (sends the refresh signal), wait ≤ 90 s for the §5 terminal row.

| Case | Viewport / locale | Must observe |
| --- | --- | --- |
| SP18-A 4242 | desktop en; mobile zh-TW | CAPTURED + CONFIRMED text; facts CAPTURED=1, reservation COMMITTED, stock allocated |
| SP18-B decline `4000 0000 0000 0002` → Cancel | desktop zh-CN; mobile en | decline shown on Stripe; store tab still Continue+Cancel; confirm; `cancelling`; then CLOSED_UNPAID + CANCELLED; stock restored exactly |
| SP18-C 3DS `4000 0027 6000 3184` | desktop zh-TW; mobile zh-CN | authenticate "Complete" in the challenge frame → CAPTURED + CONFIRMED |
| SU07 | desktop en (no Stripe payment) | `window.open` → null (init script) ⇒ `blocked`, zero BFF calls; close child ⇒ Continue ⇒ same session count 1; double click + second tab ⇒ 1 prepare, 1 session; reload while CREATING ⇒ no POST in edge log; POST/GET `/payment/return` changes no facts |
| SU08 | all cases | localStorage/sessionStorage/IndexedDB/cookies, console, Next log, Go log, `browser.log`: no `checkout.stripe.com/`, `cs_test_`, `sk_test_`/`rk_test_`, synthetic email |
| SU09 | desktop + mobile | keyboard-only Pay/Continue/Cancel; `getByRole` names; one `role=status` change per state; screenshot of each §5 row × 3 locales, sha256 in `result.json`. SANDBOX cannot reach REVIEW_REQUIRED, UNAVAILABLE, `cutoff`, CREATING-budget or CLOSED_UNPAID screenshots: those rows run in a BROWSER(MOCK) assembly using `stripetest` plus a fixture clock, labelled MOCK in `result.json`. No AUTHORIZED row for Stripe |

Viewports: desktop 1440×900; mobile `devices["Pixel 7"]` at 390×844 (same as BPU02).

## SU05 PAYUNi unchanged
Run `bash scripts/dev/test-local.sh --browser-payment` on the base SHA and on the B2 merge candidate;
both PASS. `payuni-ui-baseline.mjs` captures the PAYUNi `order-payment` element (fresh, sending,
read-only; 3 locales; desktop+mobile) on both SHAs: normalized DOM (UUIDs/timestamps masked) equal,
and element screenshot sha256 equal, or a recorded, reviewed pixel diff of zero.

## Red before green
Record one red run per gate: e.g. SU06 against base (no Stripe UI) fails at Pay; SU08 with an injected
`localStorage.setItem` of the URL fails; SU05 with a one-character PAYUNi copy mutation fails.

## Return
Commit SHA, model/reasoning, base, paths, commands + exit codes + PASS/FAIL/SKIP/NOT_RUN counts,
evidence dir, Stripe selectors observed, risks (Stripe DOM drift, rate limits, CAPTCHA), NOT_RUN
(SP17, non-Chromium, real devices). Never print or attach secrets, session URLs or card-page HAR.
