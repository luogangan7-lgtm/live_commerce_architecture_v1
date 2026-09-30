# Unit cvs-ui — storefront CVS picker / buyer-entered store / pay-at-pickup, buyer order states; admin ECPay + CVS settings cards, order CVS section

Role: ui_worker (mid tier). Base SHA `00c1d94`, SHA recorded at dispatch. Worktree `.worktrees/cvs-ui`, branch
`unit/cvs-ui`. No delegation, no new dependency, no lockfile change. **Wave 1**, parallel with cvs-core against
its FROZEN interface + FROZEN JSON (`docs/delivery/units/cvs-core.md`); real-chain checks after cvs-core F2 is
mounted. Contract: `contracts/taiwan-cvs-logistics-v1.md` (v1 FROZEN 2026-09-30) §5, §8, §16.1 (UI + links +
labels), §16.2 (labels), §16.4 (buyer copy), §16.5 (UI), §16.8 (UI), §15 Q4/Q6/Q9/Q10, §13; plus
`contracts/merchant-orders-ui-v1.md` (MOU, approved C inline comp) "Scope and reuse", "Identity, transport and
privacy", "Acceptance gates" 5 — the CVS contract lifts MOU limits for exactly the surfaces below.

## Read (by section)
PROCESS.md; the sections above. Code by symbol: storefront `components/{OrderFlow.tsx (delivery list, recipient
form, Begin), ProductPurchase.tsx (products/[productID] route, query handling), OrderHistory.tsx, OrderPayment.tsx}`,
`lib/{purchase.ts (kinds, options/Begin validators), buyer-server.ts (route table, keyed POST shape, cookie
`__Host-commerce_buyer` SameSite=Lax :20/:781), buyer-client.ts, order-copy.ts, history-copy.ts, purchase-copy.ts}`,
`apps/storefront/next.config.ts:21-43` (CSP; read only); admin `components/{MerchantOrders.tsx, OrderShipment.tsx
(0063 section pattern), OrderRefunds.tsx (<dialog> confirm, key per dialog open), SettingsWizard.tsx (delivery-service
editor, kinds), WorkspaceFrame.tsx (read only)}`, `lib/{orders-model,orders-request,orders-client,orders-copy,
settings-model,settings-client,settings-copy}.ts`, `apps/admin/app/api/stores/[store]/[...resource]/route.ts`
(grammar via `orderActionRoute`; read only).

## Build — storefront
1. **Validators** (`purchase.ts`): kinds `cvs_hilife`/`cvs_okmart`; options row `pickup_selection`, `payment_modes`,
   `store_search_url`, `{available:false, reason}`; Begin body `payment_mode`, result `payment_mode`/`commercial_state`;
   order `payment_mode`, `collection_state`, `cvs_shipment` (null | object). Mirror the FROZEN JSON exactly; unknown
   keys rejected as today. **Merges in the same integration batch as cvs-core** (else existing buyer flows break).
2. **BFF** (`buyer-server.ts`): exactly `POST cvs-selections` (key), `GET cvs-selections/{uuid}`, `POST
   cvs-selections/{uuid}/verify` (keyless, no body), `POST cvs-stores` (key). No generic proxying.
   customers-billing-ui's privacy hook also lands in this file — integrator merges both.
3. **Picker** (`CvsPickup.tsx`, rendered by `OrderFlow.tsx`): four chains, text labels (U2); unavailable →
   disabled + "即將開放 / 即将开放 / Coming soon". `ecpay_map`: save the checkout draft (cart, recipient inputs) in
   the existing session state, then build a hidden `<form method=post action=form.action>` and submit it in the
   **same tab** (no iframe, no `target`, no `window.open`). On `?cvs_selection=` return: call verify, show the
   store card (name, address, code, 離島 badge when `outside`), "更換門市", the Q9 consent line (U4); `RETURNED` +
   `retry_after_s` → retry-later notice. Missing cookie in Messenger/IG in-app browser → existing recovery path
   ("在瀏覽器中繼續"). `buyer_entered`: code/name/address fields with per-chain hints, the official search link
   (new tab, `rel="noopener noreferrer"`, constant with URL + 2026-09-30 comment; FamilyMart: check
   `family.com.tw/Marketing/zh/Map` once in a browser and record the result, else use the `family.map.com.tw` page),
   card label "你填寫的門市" (never "已驗證"). 422 codes map to field errors.
4. **Payment mode**: radio "信用卡" / "取貨付款" only when `payment_modes` has it; pay-at-pickup labels the
   recipient fields "取件人真實姓名（與證件相同）" and "手機 09xxxxxxxx"; on success no Stripe step — show the placed
   order. `pay_at_pickup_limit`/`pay_at_pickup_amount_exceeds`/`pay_at_pickup_unavailable` → inline messages.
5. **Order page** (`OrderHistory.tsx`/`OrderPayment.tsx` + copy): §5.3 states (never "delivered" before PICKED_UP;
   REQUESTED/UNKNOWN/FAILED/ABANDONED → "處理中"), §16.4 COLLECTED/RETURNED/pending "取貨時付款 NT$<total>", §16.8
   CANCELLED "賣家已取消訂單" / RESTOCKED "未取貨，已退回". 3 locales.

## Build — admin
6. **Model/grammar**: `orders-model.ts` parses `PROVIDER_LABEL_CREATED`, `pickup_source`, `payment_mode`,
   `collection_state`, new kinds/verification kinds, filter `cvs_pending` (same batch as cvs-core, or the MOU gate
   breaks). `orders-request.ts` grammar adds exactly `GET|POST orders/{uuid}/cvs-shipment`, `POST
   orders/{uuid}/cvs-shipment/{print-form|abandon}`, `POST orders/{uuid}/{collection|pay-at-pickup-release}`;
   keys per §8. New `logistics-{model,request,client,copy}.ts` with exported `logisticsRoute(method, path)` for
   `GET|PUT logistics/ecpay`, `POST logistics/ecpay/enabled`, `GET|PUT logistics/cvs-settings`.
7. **Order CVS section** (`OrderCvsShipment.tsx`, in the C inline detail row next to the 0063 section):
   "建立超商寄件" only with `order-actions.fulfillment_write` ∧ CVS destination with `pickup_source=ecpay_directory`;
   `<dialog>` confirm "綠界將從你的綠界帳戶餘額扣運費；請在 N 天內交寄" (N = `validity_days`); code with copy button;
   "列印託運單" → `cvs-print` page in a new top-level tab that POSTs `print-form` and auto-submits the returned
   form (merchant side only); timeline of events; UNKNOWN/FAILED banners + "改用手動出貨" (abandon with the
   `i_checked_ecpay_backend` checkbox for UNKNOWN); `alerts` shown as warnings; refunded order + CREATED → "已退款，
   請勿交寄". `buyer_entered` label "買家自填門市（未驗證，出貨前請至超商官網核對）". Pay-at-pickup: "已收款" /
   "已退回" / "已線下退款" actions (`collection`), "取消訂單並釋放庫存" (PENDING, unshipped) and "已收回包裹，恢復庫存"
   (RETURNED), each with `<dialog>` confirm; one Idempotency-Key per dialog open reused on retry; re-GET after every
   response (no optimistic state). Filter `cvs_pending` in the existing state filter.
8. **Settings** (`LogisticsSettings.tsx` mounted in `SettingsWizard.tsx` → 物流): "綠界物流" card (connect/rotate
   form, keys write-only and never re-displayed, mode, environment badge, enable toggle, status URL read-only,
   rotation warning while shipments are live, §9) and "超商取貨" card (chain checkboxes, 取貨付款 toggle, 上限 NT$
   1..20000, 未取貨付款訂單上限 1..500). GET 403 → card hidden; PUT 403 → no-permission notice. Delivery-service
   editor: Mode "API（綠界）" for CVS kinds only when the ECPay card is qualified; new kinds listed.
9. **Copy** zh-CN/zh-TW/en for every string and every §5.2/§8/§16 error code.

Every new/edited component or route file starts with the PROCESS §5 comment (BFF route → Go endpoint).

## Owner questions (default ships unless the owner answers otherwise)
- U1 No new comp: reuse the C inline row section/badge/button/form tokens; independent visual review vs C.
- U2 Chain labels — en: 7-ELEVEN, FamilyMart, Hi-Life, OK mart; zh-TW: 7-ELEVEN, 全家, 萊爾富, OK超商; zh-CN: 7-ELEVEN, 全家, 莱尔富, OK超商.
- U3 Unverified chains (OK until TCV10, Hi-Life until `hilife_verified`) shown disabled "即將開放".
- U4 Q9 consent line (one line, no checkbox) — zh-TW "取件人姓名與手機將提供給物流商與門市，用於通知與取貨。", zh-CN
  "取件人姓名与手机将提供给物流商与门店，用于通知与取货。", en "The recipient's name and mobile are shared with the
  carrier and store for pickup notices."
- U5 Map uses the desktop form (`Device=0`, cvs-ecpay E6); WebKit mobile result recorded by TCV08.
- U6 Print opens a new tab route `apps/admin/app/[locale]/orders/cvs-print/page.tsx`; no inline script (CSP): the
  page's client component submits via `form.requestSubmit()`.

## Write paths
Storefront: `apps/storefront/components/{OrderFlow.tsx,ProductPurchase.tsx,OrderHistory.tsx,OrderPayment.tsx,CvsPickup.tsx}`,
`apps/storefront/lib/{purchase.ts,buyer-server.ts,buyer-client.ts,order-copy.ts,history-copy.ts,purchase-copy.ts,cvs-contract.ts,cvs-copy.ts}`,
`apps/storefront/tests/{purchase,buyer-server,cvs-contract}.test.mjs`.
Admin: `apps/admin/components/{MerchantOrders.tsx,OrderCvsShipment.tsx,LogisticsSettings.tsx,SettingsWizard.tsx}`,
`apps/admin/lib/{orders-model,orders-request,orders-client,orders-copy,settings-model,settings-client,settings-copy}.ts`,
`apps/admin/lib/logistics-{model,request,client,copy}.ts`, `apps/admin/app/[locale]/orders/cvs-print/**`,
`tests/admin/{orders-model,orders-request,logistics-model,logistics-request}.test.ts` (add cases; never weaken),
`output/cvs-ui/**`. customers-billing-ui's hooks also edit `buyer-server.ts`/`OrderFlow.tsx`: integrator merges.
Forbidden: Go, SQL, contracts, `[...resource]/route.ts`, `WorkspaceFrame.tsx`, `next.config.ts`, `globals.css`,
`proxy.ts`, `tests/foundation/**`, `tests/storefront/**`, `*.spec.ts`, lockfiles, packages, other R2 units' pages.

## Verify
```sh
pnpm typecheck:admin && pnpm build:admin && pnpm typecheck:storefront && pnpm build:storefront
node --test --experimental-strip-types tests/admin/orders-model.test.ts tests/admin/orders-request.test.ts tests/admin/logistics-model.test.ts tests/admin/logistics-request.test.ts
node --test apps/storefront/tests/*.test.mjs
bash scripts/dev/test-local.sh --browser-merchant-orders-ui    # MOU regression after F2 (one browser run machine-wide)
bash scripts/dev/test-local.sh --browser-order                 # buyer checkout regression after F2
```
Record command, SHA, exit, counts → `/Volumes/data/live_commerce_architecture_v1/output/cvs-ui/`; screenshots
1586×992 + 390px × 3 locales (picker, store card, buyer-entered form, order states, admin CVS section, both
settings cards) for the independent visual reviewer.

## Gates
Implementer: typecheck/build + model/request unit tests, each new validator case with one red run. Independent
(do not claim): TCV08 + TCV14 browser halves (cvs-tests), visual review.

## Integrator hooks (integrator only; unit ships `output/cvs-ui/integrator-hooks.patch`)
- `apps/admin/app/api/stores/[store]/[...resource]/route.ts`: import `logisticsRoute`, admit those resources
  (session auth, strict bodies, Idempotency-Key on PUTs); order CVS resources already flow through
  `orderActionRoute` — add a keyless-with-body kind only if the grammar needs it.
- `apps/storefront/next.config.ts`: CSP `form-action` += `https://logistics-stage.ecpay.com.tw/Express/map
  https://logistics.ecpay.com.tw/Express/map` (§5.2 same-tab POST); `frame-src` unchanged (TCV09).
- No nav change (settings and orders pages exist).

## Order / Non-goals / Return
Author at dispatch; validators merge with cvs-core F2; real-chain + browser regressions after the integrator mounts.
Non-goals: bulk create/print, tracking fetch, store change after create, buyer cancel, grant UI, client-side money
authority. Return commit SHA, model/reasoning, base, paths, commands + exits + counts, screenshots path, U1–U6 as
applied, FamilyMart link check result, risks, NOT_RUN.
