# Unit customers-billing-ui — admin customers/finance/billing pages, standing banner, Studio refusal, buyer privacy

Role: ui_worker (mid tier). Base `00c1d94`. Worktree `.worktrees/customers-billing-ui`, branch
`unit/customers-billing-ui`. No delegation, no new dependency, no lockfile change. May run in parallel with both cores
against the FROZEN blocks of `customers-core.md` and `billing-core.md` (DTOs, paths, error codes); real-chain check
after F2. Contract: `contracts/customers-billing-v1.md` §5 (merchant table, Buyer paragraph, Admin screens,
Storefront line), §0 CD4/CD5/CD7/BD4/BD5, §12 Q8–Q10.

## Read (by section)
PROCESS.md; contract sections above; `customers-core.md` + `billing-core.md` FROZEN blocks and D4/D5/D8–D13, B3.
Code by symbol: `apps/admin/components/{MerchantOrders.tsx (table, filter, states), OrderRefunds.tsx (native
<dialog> confirm, Idempotency-Key per dialog open), WorkspaceFrame.tsx, StudioClaims.tsx}`,
`apps/admin/lib/{orders-model,orders-request,orders-client,orders-copy,claims-copy}.ts`,
`apps/admin/app/api/stores/[store]/[...resource]/route.ts` (grammar, attachment pass-through branch ~246-260),
`apps/admin/proxy.ts` (locales); storefront `components/OrderFlow.tsx` (checkout form), `lib/buyer-server.ts`
(route table ~321, bodies ~589), `lib/{claim-contract,order-copy}.ts`, `app/[locale]/claim/page.tsx`.

## Defaults adopted
- U1 Audit-first (contract §5): no card grids; customers list reuses the orders table markup/classes; detail reuses
  the inline-detail section shape; banner is one shape reused on every page. No new colour tokens.
- U2 Erase = native `<dialog>`, typed `ERASE` enables the button, text states what is kept (orders, payments,
  shipments; the store may still contact about existing orders). One Idempotency-Key per dialog open, reused on retry.
- U3 Export = POST with Idempotency-Key, streamed download; 409 `export_too_large` shown as text, no retry loop.
- U4 Billing: plan list → POST checkout → `location.assign(url)`; `checkout=done` query shows "updating…" and re-GETs
  once; portal button POST → `location.assign(url)`; URLs never stored (state/localStorage) or logged.
- U5 Banner: GRACE (neutral warning) / RESTRICTED (warning) from `GET billing/standing`, links to `/billing`;
  `payment_pending` only on the billing page. UNBILLED/GOOD → nothing.
- U6 Studio: `billing_restricted` on window open → message + billing link in the existing Studio error slot.
- U7 Storefront checkout: two unticked boxes (marketing DM; ads personalization with the purchase-events disclosure
  "already-sent events cannot be recalled") + notice link carrying `LC_PRIVACY_POLICY_VERSION = "lc-2026-10"`; after
  the order is placed, one `PUT consents` per ticked box, `context:"checkout"`; a consent failure never blocks the order.
- U8 `/[locale]/privacy`: current consents with toggles (`context:"settings"`), "Download my data", "Erase" (typed
  `ERASE`); 410 `erased` → "your data was erased" state. `/[locale]/data-deletion`: static public instructions (remove
  the app in Facebook/Instagram settings; ask the store / use `/privacy` for store data); no API call.
- U9 Locales: admin zh-CN/zh-TW/en; storefront its existing locales; every new string in copy files.

## Build
Admin:
1. `lib/customers-request.ts`: `export function customersRoute(method: string, path: string): CustomersRouteKind | null`
   for exactly: `GET customers`, `GET customers/{uuid}`, `POST customers/{uuid}/consent-withdrawals|exports|erasure`,
   `GET finance/summary`, `GET finance/summary.csv`, `GET billing`, `GET billing/standing`, `POST billing/checkout`,
   `POST billing/portal`; plus `validCustomersRequest(kind, request)` (query grammar: `limit`, `after`, `q` ≤ 40;
   `from`/`to` dates; key required on the three customer POSTs, forbidden elsewhere). Tests in the new
   `tests/admin/customers-request.test.ts`.
2. `lib/customers-model.ts`, `lib/billing-model.ts`: strict parsers mirroring the frozen Go DTOs; unknown key =
   invalid response. `lib/{customers,billing}-client.ts`, `lib/{customers,billing}-copy.ts`.
3. Pages `app/[locale]/{customers,customers/[customer],finance,billing}/page.tsx`; components `Customers.tsx`,
   `CustomerDetail.tsx`, `Finance.tsx` (native `<input type="date">`, CSV link, test-mode badge), `Billing.tsx`,
   `BillingBanner.tsx`.
4. Studio: `StudioClaims.tsx` + `claims-copy.ts` (U6 only).
Storefront:
5. `lib/privacy-contract.ts`: `export const privacyRoutes` (entries in `buyer-server.ts` route-table shape for
   `privacy` GET, `consents` PUT, `privacy/export` POST, `privacy/erasure` POST), `privacyBodies`, response validators,
   `export const LC_PRIVACY_POLICY_VERSION`; `lib/privacy-copy.ts`.
6. `components/ConsentChoices.tsx` (exports the component + `submitCheckoutConsents(orderContext, choices)`),
   `components/PrivacyCenter.tsx`, pages `app/[locale]/{privacy,data-deletion}/page.tsx`.

## Comments (PROCESS §5)
Every new route/component/lib file starts with a comment naming the BFF route(s) it calls and the Go endpoint behind
them (e.g. `// BFF /api/stores/[store]/customers/{id}/erasure → Go POST /v1/admin/stores/{id}/customers/{id}/erasure
(internal/httpapi/customers.go)`). Money shown with the existing formatter; no client-side money or consent authority.

## Write paths
Admin: `apps/admin/app/[locale]/{customers,finance,billing}/**`, `apps/admin/components/{Customers,CustomerDetail,
Finance,Billing,BillingBanner}.tsx`, `apps/admin/components/customers.css` (only if existing classes cannot cover it),
`apps/admin/components/StudioClaims.tsx`, `apps/admin/lib/{customers,billing}-{model,request,client,copy}.ts`,
`apps/admin/lib/claims-copy.ts`, `tests/admin/{customers-request,customers-model,billing-model}.test.ts`.
Storefront: `apps/storefront/lib/{privacy-contract,privacy-copy}.ts`, `apps/storefront/components/{ConsentChoices,
PrivacyCenter}.tsx`, `apps/storefront/app/[locale]/{privacy,data-deletion}/**`,
`apps/storefront/tests/privacy-contract.test.mjs`. `output/customers-billing-ui/**`.
Forbidden: `route.ts`, `WorkspaceFrame.tsx`, `buyer-server.ts`, `OrderFlow.tsx`, `globals.css`, `next.config.ts`,
`proxy.ts`, Go, SQL, contracts, `tests/foundation/**`, `*.spec.ts`, lockfiles, packages.

## Verify
```sh
pnpm typecheck:admin && pnpm build:admin && pnpm typecheck:storefront && pnpm build:storefront
node --test --experimental-strip-types tests/admin/customers-request.test.ts tests/admin/customers-model.test.ts tests/admin/billing-model.test.ts
node --test apps/storefront/tests/privacy-contract.test.mjs
bash scripts/dev/test-local.sh --browser-merchant-orders-ui   # regression after F2 (one browser run machine-wide)
```
Screenshots 1586×992 + 390px × locales → `output/customers-billing-ui/` for the independent visual reviewer.

## Integrator hooks (integrator only; unit ships `output/customers-billing-ui/integrator-hooks.patch`)
- `apps/admin/app/api/stores/[store]/[...resource]/route.ts`: import `customersRoute`/`validCustomersRequest`, admit
  those resources (session auth required, no fixture), and route export/CSV through the existing attachment
  pass-through (add `Content-Disposition` to the kept headers; never buffer).
- `apps/admin/components/WorkspaceFrame.tsx`: nav entries customers, finance, billing; mount `<BillingBanner>`.
- `apps/storefront/lib/buyer-server.ts`: spread `privacyRoutes`/`privacyBodies`; export attachment pass-through;
  clear the capability cookie on erasure 200/410. CVS UI also edits this file.
- `apps/storefront/components/OrderFlow.tsx`: render `<ConsentChoices>` in the checkout form and call
  `submitCheckoutConsents` after the order is placed (≤ 5 lines). CVS UI also edits this file.

## Gates
Implementer: typecheck/build + model/request unit tests. Independent: CB11 (`customers-billing-tests`), CB09 BFF
cases; visual review of screenshots. Do not claim them.

## Order / Non-goals / Return
Author at dispatch; integrate after F2 of both cores. Non-goals: grant UI, marketing sends, invoice UI (portal),
Meta deletion callback, plan admin. Return commit SHA, model/reasoning, base, paths, commands + exits + counts,
screenshots path, U1–U9 as applied, risks, NOT_RUN.
