# Unit stripe-b2-ui — Stripe buyer payment UI + BFF (R1-2)

Role: ui_worker (mid tier). Base = release branch tip carrying `contracts/stripe-buyer-ui-v1.md`
(FROZEN 2026-09-29; re-read its §4 steps 2-4 and §5 before coding). Worktree `.worktrees/stripe-b2-ui`, branch `unit/stripe-b2-ui`.
No delegation, no network, no Stripe key. Parallel with `stripe-b2-browser-tests` (disjoint paths;
that unit writes gates from the contract, not from this code).

**Goal:** a buyer of a Stripe store pays from the existing inline payment section: Pay → child tab →
`checkout.stripe.com` → neutral return → store tab shows the truth; Continue, Cancel and Refresh work.
PAYUNi stays byte- and pixel-identical.

## Read (by section; grep, `sed -n`)
`docs/delivery/PROCESS.md`; `contracts/stripe-buyer-ui-v1.md` (all, it is short);
`contracts/buyer-payment-ui-v1.md` "Controller ownership", "Native form and return";
`contracts/buyer-payment-public-v1.md` "Public routes", "Exact safe projections";
`contracts/stripe-psp-v1.md` §9.2, §9.3, §0.2 "Corrected amount table".
Code by symbol: `lib/payment-contract.ts` (all validators), `lib/buyer-server.ts` (`payment` route
regex ~L327, keyless `handoff` ~L794, `upstreamError`, payment validation ~L983),
`lib/buyer-client.ts` (`HANDOFF` ~L14, ~L385), `lib/order-payment.ts` (`PaymentMarker`, `parseMarker`,
`openPaymentDestination`, `payOrder`), `components/OrderPayment.tsx`, `components/OrderFlow.tsx`
(`OrderDetails` Refresh order ~L510; history reuses it via `OrderHistory.tsx` ~L147), `lib/payment-copy.ts`.
Go shapes to mirror (read only): `internal/checkout/payment_view.go` (`validPaymentViewFor`),
`internal/checkout/stripe.go` (`validStripeHandoff`, `PaymentSignal`).

## Write paths (only these)
`apps/storefront/lib/{payment-contract,buyer-server,buyer-client,order-payment,payment-copy}.ts`,
`apps/storefront/components/{OrderPayment,OrderFlow,OrderHistory}.tsx`,
`apps/storefront/tests/{payment-contract,buyer-payment-server,buyer-payment-client,order-payment}.test.mjs`
(add Stripe cases; never weaken or delete existing ones), `packages/i18n/**` only if a type genuinely
needs it (expected: untouched), `output/stripe-b2-ui/**` (main checkout path, PROCESS §4).
Forbidden: `next.config.ts`, `app/payment/return/**`, `lib/payment-return.ts`, `globals.css`,
any Go/SQL/contract/`tests/foundation`/`tests/storefront`, lockfiles, new dependencies.

## Build, in this order
1. `payment-contract.ts`: contract §3 exactly. Add `validPaymentSignal`; give `validPaymentPrepared`
   a third `method` parameter (update both callers). Keep PAYUNi branches textually unchanged.
2. `buyer-server.ts` + `buyer-client.ts`: contract §2 — refresh/cancel routes, keyless exception set
   `{handoff, refresh, cancel}`, nonretryable, prepare validated with the request's `method_code`.
3. `order-payment.ts`: widen `PaymentMarker.body.method_code`; `PaymentDestination.navigate(url)`
   (contract §4 step 4; call `owned()` before and set `used` before `location.replace`); in `payOrder`
   branch on method: PAYUNi path untouched; Stripe path = contract §4 steps 2–5 exactly: marker/view
   matrix in step 2 (a leftover `prepare` marker on a PENDING/AWAITING_PAYMENT view must NOT throw
   `uncertain`; PAYUNi/Stripe marker mismatch → `failed`), lock released after prepare and re-acquired
   for the handoff POST, re-GET before the POST and abort if `cancel_requested`. Export
   `requestPaymentSignal(context, orderID, "refresh"|"cancel"): Promise<PaymentSignal>` (keyless POST,
   one call, no retry, validates response). No URL ever passed to storage, console or an error message.
4. `OrderPayment.tsx`: contract §5 table and §6 polling (one `setTimeout` chain owned by the epoch,
   cleared on cleanup; visibility gate). Cancel button + `window.confirm` per Q1 ruling, disabled while Pay/Continue is in
   flight; when Cancel/Continue disappears, focus the status line (`tabIndex=-1`, §8). Stripe errors,
   budget and CLOSED/UNAVAILABLE map to `failed`/`creating`, never `uncertain` (§5). Register the refresh handler on a parent-provided ref only while
   the view is a non-terminal Stripe attempt; clear it on epoch change.
5. `OrderDetails` in `OrderFlow.tsx` (touch `OrderHistory.tsx` only if wiring needs it): pass the ref; Refresh order `onClick` awaits
   `paymentSignalRef.current?.()` (errors swallowed into the payment section's own message) before its
   existing refresh. No other layout change.
6. `payment-copy.ts`: contract §7 keys in all three locales (the `Copy` type enforces completeness).

**Comments (PROCESS §5):** every touched route/component file header names the BFF route(s) and Go
endpoint behind them; the navigate call site says why the URL is never stored (stripe-psp §9.2, D16);
the "one handoff POST per click" and "no POST from effects" branches say why.

## Verify (record command, SHA, exit code, counts → `output/stripe-b2-ui/`)
```sh
node --test --experimental-strip-types apps/storefront/tests/*.test.mjs   # all storefront Node suites
pnpm run typecheck:storefront
COMMERCE_BUYER_WEB_ENABLED=0 pnpm run build:storefront
git diff --stat <base> -- apps/storefront/next.config.ts apps/storefront/app apps/storefront/app/globals.css  # must be empty
```
Node tests must cover SU01–SU03 cases you can reach without a browser (author-side unit tests; the
independent SU gates belong to `stripe-b2-browser-tests`). Existing test count must not drop.
Do not run the SANDBOX browser gate; do not claim SU05–SU10.

## Non-goals
Method chooser (Q3), Stripe Embedded Checkout / Stripe.js, same-tab fallback, new CSS or layout,
merchant UI, refunds, return-page changes, rate limiting, Go changes. A needed Go/contract change =
stop and escalate.

## Return
Commit SHA, model/reasoning, base SHA, worktree, paths changed, commands + exit codes + PASS/FAIL/SKIP
counts, evidence paths, risks, NOT_RUN (SU05–SU10, SP18). Two failed fixes on one blocker → escalate.
