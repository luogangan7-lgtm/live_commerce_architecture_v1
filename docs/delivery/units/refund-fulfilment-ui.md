# Unit refund-fulfilment-ui — admin refund + shipment sections, export, buyer refund/shipment display

Role: ui_worker (mid tier). Base = integrator-recorded SHA at dispatch. Worktree
`.worktrees/refund-fulfilment-ui`, branch `unit/refund-fulfilment-ui`. No delegation, no new
dependency, no lockfile change. Contracts: `contracts/stripe-refund-v1.md` §7.1 (admin UI paragraph),
§7.2; `contracts/manual-fulfilment-v1.md` §5.2, §5.4; `contracts/merchant-orders-ui-v1.md` (MOU,
approved C inline comp). These two contracts are the MOU amendment that lifts its "no shipping,
refunds, exports" and "no refunded/shipped label" limits — for exactly the surfaces below.
DTOs are frozen in `docs/delivery/units/refund-core.md` and `fulfilment-core.md` (FROZEN blocks).

## Read (by section)
PROCESS.md; the contract sections above; MOU "Scope and reuse", "Identity, transport and privacy",
"Acceptance gates" 5. Code by symbol: `apps/admin/components/MerchantOrders.tsx` (detail row,
filter, states), `apps/admin/lib/orders-{model,client,request,copy}.ts`,
`apps/admin/app/api/stores/[store]/[...resource]/route.ts` (resource grammar, method table,
Idempotency-Key handling of existing POST/PUT resources), `apps/admin/components/{WorkspaceFrame,Icon}.tsx`;
storefront (after stripe-b2-ui merges): `components/{OrderFlow,OrderHistory,OrderPayment}.tsx`,
`lib/{purchase,payment-contract,buyer-server,order-copy,history-copy,payment-copy}.ts`.

## Build — admin (can start at dispatch; apps/admin has no concurrent writer)
1. **Model/BFF (lands in the same integration batch as fulfilment-core, or the MOU gate breaks):**
   `orders-model.ts` parses the new summary keys, `PARTIALLY_REFUNDED`/`REFUNDED`,
   `MERCHANT_SHIPPED`, detail `shipment`, mirroring the §7.1 invariants exactly as Go does; filters
   add `shipped`, `unshipped`. BFF grammar adds exactly: `GET|POST orders/{uuid}/refunds`,
   `POST orders/{uuid}/refunds/{uuid}/refresh` (keyless, no body), `PUT orders/{uuid}/shipment`
   (Idempotency-Key required), `GET orders/{uuid}/shipment/history`, `GET orders/unshipped.csv`
   (streams the Go attachment through with its `Content-Type`, `Content-Disposition`, `Cache-Control`,
   `X-Export-Truncated`; never buffered to storage), `GET order-actions`. No generic proxying.
2. **Refund section** in the C inline detail row: captured / refunded / in progress / refundable,
   list with state badges (REQUESTED/SUBMITTING/PENDING → "处理中/處理中/Processing"; SUCCEEDED;
   FAILED/CANCELED with mapped reason; REJECTED; UNKNOWN → "需要支持/需要支援/Needs support"),
   `stripe_refund_id` shown for merchants (R-9). Action only when `order-actions.refund` ∧
   `refundable_minor>0`: amount (default = refundable, existing currency formatter, TWD whole
   dollars, step validated client-side as a hint only), reason (`requested_by_customer`, `duplicate`),
   confirmation restating amount + currency, one Idempotency-Key per dialog open reused on retry,
   `expected_refundable_minor` from the last GET. No optimistic state: re-GET after every response.
   Refresh button per non-terminal refund (throttle errors shown as retry-later).
3. **Shipment section**: with `order-actions.fulfillment_write` and an eligible order, form (carrier
   select with the seven §3.1 codes, name field required for `other`, tracking number, optional https
   URL, optional note) → "Mark shipped"; with a SHIPPED head, record + "Correct" + "Void" (reason
   select; void body sends carrier/tracking/note as `null`). `expected_version` from the detail.
   History = `<details>` disclosure (note/void reason/principal UUID visible to merchants only).
4. **List**: `shipped`/`unshipped` in the existing state filter; "Export unshipped (CSV)" only with
   `order-actions.orders_export`, plus the text-column import hint (§5.3) and truncation notice.
5. **Copy** zh-CN/zh-TW/en in `orders-copy.ts` for every string above, all error codes of refund §7.1
   and fulfilment §5.1, carrier names per Q3.

## Build — storefront (only after stripe-b2-ui is merged; other agents own apps/storefront until then)
6. Payment view validator admits `PARTIALLY_REFUNDED`/`REFUNDED` and optional `refund`
   (`{refunded_minor,pending_minor}`, absent ≡ null). Order page: "Refund processing" when
   `pending_minor>0`; "Refunded X — your bank may take 5–10 business days" only for succeeded amounts.
7. Order detail validator admits `shipment` (null | SHIPPED object) and history `MERCHANT_SHIPPED`.
   Show "Shipped by the seller", carrier display name, tracking number with copy button, and the
   link (`rel="noopener noreferrer nofollow"`, `target="_blank"`, host text visible). Never "in transit"
   or "delivered". Copy in the storefront copy files, 3 locales.

Every new/edited component or route file starts with the PROCESS §5 comment (BFF route → Go endpoint).

## Owner questions (default ships unless the owner answers otherwise)
- Q1 No new comp for the refund/shipment sections: they reuse the C inline row's section, badge,
  button and form tokens; independent visual review judges screenshots against C. Default: yes.
- Q2 Refund confirmation uses the native `<dialog>` with a two-step confirm (amount + currency
  restated); shipment/void forms are inline in the detail row. Default: yes.
- Q3 Carrier display names — en: 7-ELEVEN, FamilyMart, Hi-Life, OK mart, SF Express, Chunghwa Post,
  Other; zh-TW: 7-ELEVEN 交貨便, 全家 店到店, 萊爾富, OK mart, 順豐速運, 中華郵政, 其他;
  zh-CN: 7-ELEVEN 交货便, 全家 店到店, 莱尔富, OK mart, 顺丰速运, 中华邮政, 其他.
- Q4 Badge tones: processing = neutral, succeeded = existing success tone, failed/unknown/needs
  support = existing warning tone; no new colour tokens.
- Q5 Export button sits in the list toolbar right of the filter as a secondary button (not a KPI/CTA).
- Q6 Buyer tracking link opens in a new tab and shows the host (e.g. `(t.cat.com.tw)`) after the link text.

## Write paths
Admin: `apps/admin/components/{MerchantOrders.tsx,OrderRefunds.tsx,OrderShipment.tsx}`,
`apps/admin/lib/orders-{model,client,request,copy}.ts`, `apps/admin/app/api/stores/[store]/[...resource]/route.ts`,
`tests/admin/{orders-model,orders-request}.test.ts` (add cases; never weaken existing ones).
Storefront (phase 2): `apps/storefront/components/{OrderFlow,OrderHistory,OrderPayment}.tsx`,
`apps/storefront/lib/{purchase,payment-contract,buyer-server,order-copy,history-copy,payment-copy}.ts`,
`apps/storefront/tests/{purchase,payment-contract,buyer-server,order-payment}.test.mjs`.
`output/refund-fulfilment-ui/**`. Forbidden: Go, SQL, contracts, `tests/foundation/**`,
`tests/storefront/**`, `*.spec.ts` gates, `globals.css`, `next.config.ts`, lockfiles, new packages.

## Verify
```sh
pnpm typecheck:admin && pnpm build:admin
node --test --experimental-strip-types tests/admin/orders-model.test.ts tests/admin/orders-request.test.ts
pnpm typecheck:storefront && pnpm build:storefront && node --test apps/storefront/tests/*.test.mjs   # phase 2
bash scripts/dev/test-local.sh --browser-merchant-orders-ui   # MOU regression after F2 (one browser run machine-wide)
bash scripts/dev/test-local.sh --browser-merchant-orders-bff
```
Record command, SHA, exit, counts → `output/refund-fulfilment-ui/`; screenshots 1586×992 + 390px × 3
locales for the independent visual reviewer. RF11/MF07 are refund-fulfilment-tests' gates.

## Order
Admin phase: author at dispatch against the frozen DTOs; real-chain check after F2 (both cores merged
+ mounted). Storefront phase: after stripe-b2-ui merges. Model/validator changes merge in the same
integration batch as fulfilment-core (admin) and refund-core/fulfilment-core (storefront).

## Non-goals / Return
No buyer refund request, no carrier tracking fetch, no bulk import, no grant UI, no client-side money
authority. Return commit SHA, model/reasoning, base, paths, commands + exits + counts, screenshots
path, answers assumed for Q1–Q6, risks, NOT_RUN.
