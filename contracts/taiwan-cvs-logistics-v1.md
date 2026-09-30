# Taiwan CVS logistics v1 — buyer store selection, ECPay shipment creation, code/label, status ingress

Status: **v1 FROZEN 2026-09-30 (DESIGN; gates NOT_RUN)** — drafted 2026-09-29, amended 2026-09-29 after the round-1 review
(`output/contract-review/r2-design-wave.json` key `cvs`, verdict BLOCK; resolution table in §14),
2026-09-30 after the round-2 review (4 P1, resolution in §14.1) and 2026-09-30 after the round-3 review
(`output/contract-review/r2-round3.json` key `cvs`, 4 P1; rulings X1/X5, resolution in §14.2; new §16 =
owner clarification C1–C4) and 2026-09-30 after the §16 verification review (3 P1, resolution in §14.3); frozen
per ruling X6; amendment X8/X9 (2026-09-30: §16.8 pay-at-pickup cancel/restock + TCV17, and the seven §16
verification P2s; resolution in §14.4) applied after its review (2 P1 + 2 P2, resolution in §14.5), DESIGN only. Evidence label for this
file: DESIGN. Every
TCV gate below is NOT_RUN. Nothing here authorizes a LIVE logistics order (a LIVE create buys a
label and ECPay deducts the fee from the merchant's ECPay balance, §1 F12): LIVE create stays refused
until the owner flips the flag in §12 (AGENTS.md: no label purchase without owner approval).

Owner decision (2026-09-29, chat): buyers must be able to pick a Taiwan convenience-store pickup at
checkout — 7-ELEVEN, FamilyMart, Hi-Life, OK mart — and shipments must be creatable. Card (Stripe) or
pay-at-pickup, and ECPay-map or buyer-entered store source, per owner clarification C1–C4 (2026-09-30),
§16.

Builds on, and where they conflict is subordinate to: architecture §13 (13.1 connection ≠ API key,
13.3 stable operation ID / UNKNOWN, 13.4 CREATED ≠ HANDED_OVER), invariants I02, I04, I06, I11, I13,
I14, I20, I24; [buyer-destination-v1](buyer-destination-v1.md) (pickup source + destination
snapshot), [delivery-service-v1](delivery-service-v1.md) (MANUAL|API service revisions),
[merchant-service-settings-v1](merchant-service-settings-v1.md) §3 (carrier-neutral pickup, adapter
vs merchant-arranged), [manual-fulfilment-v1](manual-fulfilment-v1.md) (MD1 one parcel per order,
MD6 eligibility, 0063), [external-operation-v1](external-operation-v1.md) +
[external-dispatcher-v1](external-dispatcher-v1.md) (Plan/Claim/Complete, UNKNOWN never redispatched),
[stripe-psp-v1](stripe-psp-v1.md) §0.2 (per-store account + AEAD credential custody, per-endpoint
webhook route), [buyer-checkout-options-v1](buyer-checkout-options-v1.md),
`docs/discovery/2026-09-20-shopline/06-台湾超商选店与跨境履约.md` (cross-border boundary).

## 0. Owner inputs, decisions and questions

Known owner/user facts (Humaux records 2026-09-19..29, not re-asked):
- Goods ship **from mainland China / overseas → Taiwan buyer → CVS pickup**; the carrier is not fixed
  (SF, Cainiao, forwarders). Store selection must not require a pre-chosen carrier (06 §1, §7).
- Merchants connect **their own** logistics/payment accounts (existing platform screenshots: ECPay
  logistics card with enable/visible toggles). "自主模式" is an opaque label, not a mode.
- COD is an independent capability, default false.

| # | Decision (DRAFT) | Why / risk closed |
| --- | --- | --- |
| TD1 | **Primary adapter: ECPay 物流整合API (v1, form POST + CheckMacValue MD5)**; carrier-neutral model unchanged (pickup source ≠ carrier capability). A second adapter (NewebPay/PAYUNi/forwarder API) is a new `provider` value + route, never a schema fork. | Only surveyed provider with public docs + public stage env for map, store list, create, print, query and status for 7-ELEVEN/FamilyMart/Hi-Life (§1, §2). v1 over v2 (全方位物流): v2 collects recipient PII on ECPay's page and its callbacks are AES-CBC without a MAC (F14); v1 keeps recipient capture in our destination snapshot. |
| TD2 | **BYO account per store** (`integration.merchant_accounts` provider `ecpay_logistics`), at most one per (store, environment). The store's connection is the directory source for the map even for MANUAL CVS services (TD6). | Matches "merchants connect their own accounts"; platform PlatformID program needs an ECPay partner contract (F11, Q1). |
| TD3 | **Store selection = ECPay e-map redirect → unsigned return → server-side directory verification via GetStoreList** (signed request over TLS). The trusted name/address come from the directory row, never from the browser POST. | Map return carries no CheckMacValue (F3); the browser can forge it. Forgery then only yields a real, existing store. |
| TD4 | **Pickup source reuses `fulfillment.pickup_versions/heads`** with a new `verification_kind='PROVIDER_DIRECTORY_VERIFIED'`, namespace `ecpay.<subtype>`, written only by a definer (never by merchant or buyer). Begin accepts both kinds. | No second destination model; existing FK/CAS/expiry/lock order are kept (buyer-destination-v1). |
| TD5 | **Shipment creation is a merchant action, per order, after payment** (MD6 eligibility), through the existing ledger/dispatcher: one `integration.operations` row (`ecpay_logistics` / `ecpay.cvs_create` / `transactional`), one `fulfillment.cvs_shipments` row. Never auto-created at payment. | ECPay C2C orders expire 5–7 calendar days after creation (F9); cross-border parcels routinely take longer to reach a Taiwan drop point. A LIVE create also costs money. |
| TD6 | Delivery kinds widen to `cvs_711`, `cvs_familymart`, `cvs_hilife`, `cvs_okmart`. `cvs_okmart` is **selectable only once TCV10 proves** an OK directory + map path (ECPay docs contradict each other, F5); until then OK services cannot be made available (§5.1) and the storefront shows OK mart as "即將開放" (§15, Q4). `cvs_hilife` follows the same gating via profile flag `hilife_verified` until TCV07 or TCV12 shows a Hi-Life Create accepted with a directory `StoreId` (store-code length UNKNOWN, F17). | Owner wants four chains; we must not ship an unverifiable store source. |
| TD7 | Status notifications are provider reports: CheckMacValue verified with the connection's keys, deduped, mapped by an exact code table; unknown codes are stored raw and change nothing. Reply `1|OK` only after COMMIT. | F7; I13 (created ≠ at store ≠ picked up). |
| TD8 | Buyer PII (recipient name/phone) never enters `integration.operations.request`, job args, receipts or logs; the worker loads it under lease (like `load_stripe_credential`). Status bodies (which echo recipient PII, F7) are stored only as sha256 + non-PII fields. | external-operation-v1 frozen-request rule; I11. |

Rejected alternatives:
- Trusting map POST fields (name/address/code) as verified: unsigned (F3).
- iframe/new-window map: ECPay forbids iframe; iOS must not open a new window (F4).
- Our own scraped store database: rot + no provider mapping (06 §2).
- Auto-create at payment: expiry window (F9) and label cost; also breaks cross-border timing.
- Retrying an UNKNOWN create under a new MerchantTradeNo: duplicate label (I06, §13.3).
- ECPay v2 as primary: MAC-less AES callbacks, recipient PII collected on ECPay page (F14).
- 7-ELEVEN 交貨便/MyShip direct: no public merchant API found (F16).
- Treating ECPay "跨境物流" as inbound Taiwan: it is Taiwan-outbound (06 §1, C08).

### 0.1 Open owner question (blocking before 0073 implementation; engineering proceeds on the default)

Q3–Q6 and Q8–Q10 of the first draft are now integrator-accepted defaults (§15); Q7 (one real
parcel) is an owner *approval* step, listed in §0.3, not a question.

| # | Question | Default until answered |
| --- | --- | --- |
| Q1/Q2 | **Which Taiwan entity/account holds the ECPay logistics contract?** ECPay membership needs a Taiwan individual (18+, no overseas individuals/phones) or a Taiwan-registered company (F13, UNVERIFIED wording), and C2C parcels must be dropped at a Taiwan store with the 寄件代碼 (B2C: delivered to a Taiwan DC). Options: (a) each merchant's own ECPay account held by a Taiwan person/company or forwarder acting for it (BYO); (b) one platform partner account (ECPay PlatformID programme). | **(a) BYO per store** (TD2). PlatformID only after a written ECPay partner agreement (settlement, fees, liability). Merchants without a Taiwan holder/drop-off path keep the MANUAL path (0063) and cannot use API create; if that is most merchants, the next adapter is a cross-border carrier API, not ECPay. |

### 0.2 Integrator rulings needed

- **R-1 Keyring scope.** ECPay uses one HashKey/HashIV pair for request MACs *and* callback
  verification (F6). The API process (GetStoreList verify, print form, status ingress, connect probe)
  and the worker (create/query) both need to decrypt it. Draft: one `ECPAY_LOGISTICS_KEYRING`,
  payload/AAD `ecpay-logistics-v1` bound to tenant/store/connection/environment/merchant_id/version;
  distinct from Stripe and PAYUNi keyrings. Stripe's API/worker split cannot be copied here.
- **R-2 Relax delivery-service "API Enabled=false"** (0010 CHECK `mode<>'API' OR NOT enabled`) to
  "API enabled ⇒ binding of a qualified, enabled `ecpay_logistics` connection of the same store"
  (§4.1). This is the "reviewed migration and real adapter capability gate" delivery-service-v1 asks for.
- **R-3 Pickup principal.** `pickup_versions.principal_id` must be a membership; draft uses the
  connection's `merchant_accounts.principal_id` for provider-verified rows (evidence_ref names the
  selection). Alternative: nullable principal + CHECK by kind.
- **R-4** New `checkout.orders.fulfillment_state` value `PROVIDER_LABEL_CREATED` (§6).
- **R-5** Permissions: reuse `integration:read` (GET connect card; exists since 0008), `integration:manage`
  (connect/rotate/enable), `fulfillment:write` (create/print/abandon), `orders:read` (view). No new
  permission, no backfill. CVS create does **not** go through `core.Service.Plan` (which authorizes
  `integration:execute`, service.go:188); it plans its operation inside `request_cvs_shipment` (§4.3,
  round 2), so a `fulfillment:write`-only member can request a label.
- **R-6** Migration split: 0072 schema/constraints/tables; 0073 definers/grants/policies + begin_hold
  and options replacement; plus one post_river file (next free number, integrator assigns; round 2) that
  holds only the `begin_hold` replacement (post_river 0005 re-creates the old signature after every main
  migration, §4.3). Round 3: no river grant to `commerce_checkout_writer` (migrate.go:180-184 revokes
  `river.river_job` and schema `river` from that role at the end of every Apply); job verification runs in
  `integration.plan_cvs_create`, owned by `commerce_integration_writer` (§4.3). No new job kind: the
  existing `external_operation_v1` kind and its commit-time link trigger (post_river 0014
  `external_operation_job_commit`) are reused.
- **R-7 Core dispatcher extension** (round 1, finding 3; owner: integrator). Verified in
  `internal/integrations/core/dispatcher.go` (2026-09-29): `DispatchRoute` has no hook that runs local SQL
  in the completion tx (`completeOperation` only calls `Service.Complete`), `Outcome` carries only
  `State/Code/ProviderReference`, and `LoadSecret` runs **only in dispatch mode**, so a `Reconcile` can
  never sign an ECPay query (Check and Reconcile never see a Secret). Two optional, backwards-compatible
  additions, both covered by the existing dispatcher suite plus one new test each:
  (a) `Outcome.Detail any` (`json:"-"`, ignored by `Complete`) and `DispatchRoute.Finish func(ctx,
  pgx.Tx, Operation, Outcome) error`, run inside `completeOperation`'s tx **before**
  `Service.Complete` (which runs last and clears the lease); a Finish error rolls the whole tx back and
  the dispatcher returns `errCompletionUncertain` (same as a Complete failure). **A Finish must be total
  over provider data** (round 2): an error is reserved for database faults, never for a value the provider
  sent — otherwise every reconcile repeats the same failure and the operation stays DISPATCHING forever
  (dispatcher.go:363-368; 0008 `claim_operation` turns a non-READY claim into reconcile).
  (b) `DispatchRoute.ReconcileWithSecret func(ctx, DispatchRequest, Secret) (Outcome, error)`: when set,
  `LoadSecret` also runs in reconcile mode (same lease-fenced tx), and a loader `ErrPolicyDenied` in
  reconcile mode completes `UNKNOWN` `credential_unavailable` (never BLOCKED_POLICY — a create may exist).
  Routes that set neither behave exactly as today.

### 0.3 Owner prerequisites (engineering cannot close these)

- **P1 Production host + public ingress.** Production server (O-D: operated by Claude, secrets as
  owner-supplied files) with the public HTTPS hooks host `LC_HOOKS_HOST` (`COMMERCE_CVS_HOOKS_ORIGIN`,
  §12) on xgdwm.com. If the host is proxied by
  Cloudflare, a rule must skip challenge/bot checks for `/v1/cvs/ecpay/status/*` and
  `/v1/cvs/ecpay/map-return/*`; whether Cloudflare challenges ECPay's `postgate` POSTs is **UNKNOWN** —
  verified by one signed MOCK post through Cloudflare before TCV13.
- **P2 ECPay LIVE member** (per Q1/Q2) with 物流 C2C enabled for each chain offered and a topped-up
  balance (F5: low balance ⇒ create refused).
- **P3** At least one pilot merchant confirms the Q1/Q2 Taiwan drop-off path before 0073
  implementation starts.
- **P4 Approval (not a question):** the owner approves the one real C2C parcel (TCV13, ~55–65 TWD, F12)
  in chat before it is created; only after TCV13 passes may `CVS_ECPAY_LIVE_CREATE=1` be set
  (AGENTS.md: no label purchase without owner approval).

## 1. External facts relied on (retrieved 2026-09-29 UTC unless noted)

Primary source: ECPay 物流整合API docs (official). Pages fetched with curl/WebFetch on 2026-09-29.

| # | Fact | Source |
| --- | --- | --- |
| F1 | Map: `POST https://logistics-stage.ecpay.com.tw/Express/map` (stage) / `https://logistics.ecpay.com.tw/Express/map` (prod), form-urlencoded. Request: `MerchantID` S(10), `MerchantTradeNo` S(20) unique alnum, `LogisticsType=CVS`, `LogisticsSubType`, `IsCollection` Y/N, `ServerReplyURL` S(200), `ExtraData` S(20), `Device` 0/1. **No CheckMacValue in request.** | https://developers.ecpay.com.tw/8795/ |
| F2 | Map subtypes: B2C `FAMI`, `UNIMART`, `UNIMARTFREEZE`, `HILIFE`; C2C `FAMIC2C`, `UNIMARTC2C`, `HILIFEC2C`. **`OKMARTC2C` not listed** for the map. | 8795 (zh), 22409 (en) |
| F3 | Map return fields: `MerchantID`, `MerchantTradeNo`, `LogisticsSubType`, `CVSStoreID` S(9), `CVSStoreName` S(10), `CVSAddress` S(60), `CVSTelephone` S(20) (not for UNIMART/UNIMARTC2C), `CVSOutSide` 0/1 (only UNIMART/UNIMARTC2C/FAMI/FAMIC2C), `ExtraData`. **No CheckMacValue in the return.** Return is HTML (browser auto-POST to ServerReplyURL). Stage returns a fixed store, no real map. | 8795 |
| F4 | Map: no iframe; on iOS do not open a new window; mobile needs cookies enabled. Error "找不到加密金鑰" = subtype not enabled for that merchant. | 8795 |
| F5 | Create `POST .../Express/Create` accepts C2C `FAMIC2C`, `UNIMARTC2C`, `HILIFEC2C`, **`OKMARTC2C`**; B2C `FAMI`, `UNIMART`, `UNIMARTFREEZE`, `HILIFE`. `GoodsAmount` 1..20000; `ReceiverStoreID` S(6); `ReceiverName` 4–10 chars (Chinese 2–5), no digits/symbols/emoji; `ReceiverCellPhone` 10 digits starting `09`; `SenderName` 4–10 chars; `SenderCellPhone` required for UNIMARTC2C/HILIFEC2C/OKMARTC2C; `GoodsName` required for those three, forbidden symbols listed; `ServerReplyURL` required; `CheckMacValue` required. Success body `1|k=v&…&CheckMacValue=…`, failure `0|message`. Save `AllPayLogisticsID`. B2C shipment number must be fetched by query (`ShipmentNo`). Low ECPay balance ⇒ create refused. B2C 7-ELEVEN/FamilyMart require prior 測標. `PlatformID` blank for ordinary merchants. Stage test stores: 7-ELEVEN 131386, 7-ELEVEN frozen 896539, FamilyMart 006598, OK 1328. The 物流方式一覽表 (7442) and GetStoreList `CvsType` omit OK. | https://developers.ecpay.com.tw/8809/, /7442/, /47496/ |
| F6 | CheckMacValue: sort all params A→Z, `HashKey=…&…&HashIV=…`, URL-encode (.NET-compatible table), lowercase, **MD5**, uppercase hex. Official worked example with the public C2C test keys yields `692FD6E2CDB539CCDB7206C76DC239AD`. With PlatformID, the platform's keys are used. | https://developers.ecpay.com.tw/7424/, /7400/ |
| F7 | Status notification: ECPay server POSTs form to the ServerReplyURL given at create: `MerchantID`, `MerchantTradeNo`, `RtnCode`, `RtnMsg`, `AllPayLogisticsID`, `LogisticsType`, `LogisticsSubType`, `GoodsAmount`, `UpdateStatusDate`, `ReceiverName/Phone/CellPhone/Email/Address` (PII), `CVSPaymentNo`, `CVSValidationNo`, `BookingNote`, `CheckMacValue`. Merchant must verify MAC, reply exactly `1|OK` (no HTML/spaces). Otherwise retried 3× then next day, for 3 days from the status date. **Stage has no simulated status notifications.** Not real-time; use query to back-fill. Common codes: to DC 7-11 2030 / FamilyMart+Hi-Life 3024; at store 7-11 B2C 2063, 7-11 C2C 2073, others 3018; picked up 7-11 2067, others 3022; 7-day unclaimed 7-11 2074, others 3020; 7-11 re-delivered to pickup store 2098, to sender store (C2C) 2099. OK mart column absent. | https://developers.ecpay.com.tw/7420/ |
| F8 | Code/label: 7-ELEVEN C2C code = `CVSPaymentNo` (8) + `CVSValidationNo` (4), printable at ibon or `…/Express/PrintUniMartC2COrderInfo`; FamilyMart C2C `CVSPaymentNo` at FamiPort or `…/PrintFAMIC2COrderInfo`; Hi-Life C2C at Life-ET or `…/PrintHILIFEC2COrderInfo`; B2C/home `…/helper/printTradeDocument`. All are form POSTs with CheckMacValue, return HTML, no iframe, batch ≤100 recommended. No OK mart print API page. | /7406/, /8848/, /8858/, /8875/ |
| F9 | Order validity (calendar days): C2C 7-ELEVEN 5, FamilyMart 6, Hi-Life 7, OK 7; B2C FamilyMart 6, 7-ELEVEN 5, Hi-Life 7. B2C DC receiving windows per chain. Store closure (關轉) is signalled by status + email; C2C store change via `…/Express/UpdateStoreInfo`; 7-ELEVEN C2C cancel via `…/Express/CancelC2COrder`. | /7444/, /8907/, /7412/ |
| F10 | Query `…/Helper/QueryLogisticsTradeInfo/V5` by `AllPayLogisticsID` or `MerchantTradeNo`, `TimeStamp` valid 3 min, CheckMacValue; response `k=v&…&CheckMacValue`, incl. `LogisticsStatus`, `CVSPaymentNo`, `CVSValidationNo`, `ShipmentNo`, `HandlingCharge`. Store list `…/Helper/GetStoreList` (`CvsType` All/FAMI/UNIMART/HILIFE/UNIMARTFREEZE, CheckMacValue) returns JSON `StoreId` S(10), `StoreName`, `StoreAddr`, `StorePhone`; refreshed daily at 20:00. | /7418/, /47496/ |
| F11 | Transport: TLS 1.2 only, https 443, FQDN not fixed IPs; callbacks come from `postgate(-stage).ecpay.com.tw`; too-fast calls get HTTP 403, wait 30 min; ServerReplyURL must be ASCII/punycode. Test merchants: B2C+home `2000132`, C2C `2000933` (keys published on the page; not copied here). | /7400/, /7398/ |
| F12 | Fees (list price, TWD, per parcel): C2C 7-ELEVEN 65, FamilyMart 65, Hi-Life 55; B2C 55 each; COD fee 0.75% (min 3). Size ≤45 cm longest side, L+W+H ≤105 cm; ≤10 kg (Hi-Life ≤5 kg). OK mart not in this table. | https://www.ecpay.com.tw/IntroTransport/Service_Fee |
| F13 | ECPay membership: 18+, no overseas individual registration, no overseas phone numbers; business members need Taiwan commercial registration. **Search-result summary only** (support.ecpay.com.tw/4862 timed out on direct fetch) — UNVERIFIED wording. | https://support.ecpay.com.tw/4862/ , https://www.ecpay.com.tw/About/ProvisionOnMember |
| F14 | ECPay 全方位物流 v2: JSON with AES-128-CBC/PKCS7 `Data`, 5-min timestamp, hosted "物流選擇頁" (`/Express/v2/RedirectToLogisticsSelection`) collecting recipient data, `CreateByTempTrade`; status notification is AES JSON, merchant replies RtnCode=1, retried every 60 min 3×/day; no MAC field documented; subtypes list omits OK. | https://developers.ecpay.com.tw/10112/, /10118/, /10127/, /10205/, /10210/ |
| F15 | NewebPay 藍新 store-to-store pages list 7-ELEVEN, FamilyMart, Hi-Life **and OK mart** size rules; merchant enables logistics in its member centre; map/create/notify API contract **UNKNOWN** (API download page is dynamic, not read). Fee figures (7-11 65, 全家 65, 萊爾富 55, OK 55, COD 0.75%) come from a search summary only — UNVERIFIED. | https://www.newebpay.com/website/Page/content/logistic_step_by_step |
| F16 | 7-ELEVEN 交貨便/MyShip public pages: consumer service for Taiwan residents; no public merchant developer API found. PAYUNi logistics: 7-ELEVEN C2C + T-cat home delivery per a third-party plugin doc (official PAYUNi logistics API not read — UNKNOWN). T-cat / HCT (新竹) are home-delivery carriers; no four-chain CVS pickup API found — UNKNOWN. | https://myship.7-11.com.tw/ , https://docs.wpbrewer.com/wpbr-payuni-shipping |
| F17 | `ReceiverStoreID` is documented as `String(6)` — a **maximum** length, not an exact one: the same page's stage store for OK is `1328` (4 chars). No Hi-Life stage store is listed on 8809, so the **Hi-Life store-code length is UNKNOWN** (a search snippet mentions a stage store `2001`, unverified). `GoodsAmount` is integer TWD 1..20000; outside ⇒ error `10500040`. Re-fetched 2026-09-29. | https://developers.ecpay.com.tw/8809/ |
| F18 | `GetStoreList` has **no store-ID filter and no pagination**; it returns the whole chain list per `CvsType` (`StoreId` S(10), `StoreName` S(40), `StoreAddr` S(100), `StorePhone` S(20)); no response-size limit documented; refreshed once daily at 20:00. A LIVE 7-ELEVEN list is several thousand rows (estimate ≥2 MiB UTF-8 JSON — UNKNOWN until TCV12 measures it). Re-fetched 2026-09-29. | https://developers.ecpay.com.tw/47496/ |
| F19 | Logistics status `300` = "訂單處理中(已收到訂單資料)" (order data received, nothing moved). **Search-result summary only** (full code table 物流狀態代碼一覽表 not read); the page 7420 lists no created-only or C2C-lapse code. UNVERIFIED until TCV07 records the stage Query `LogisticsStatus` right after a create. Searched 2026-09-29. | https://developers.ecpay.com.tw/?p=7440 , https://developers.ecpay.com.tw/7420/ |
| F20 | Create `IsCollection` = `N` 不代收貨款 (default) / `Y` 有代收貨款; `CollectionAmount` 代收金額 must equal `GoodsAmount` for `UNIMARTC2C`, `UNIMART`, `UNIMARTFREEZE`; `GoodsAmount` 1..20000. No separate CollectionAmount range is documented (the 1..20000 GoodsAmount range is applied). Re-fetched 2026-09-30. COD fee 0.75% (min 3) is charged to the merchant (F12). | https://developers.ecpay.com.tw/8809/ |
| F21 | Store numbers (店號) as shown by each chain's own store search, retrieved 2026-09-30: **7-ELEVEN 6 digits** (e-map "使用店號查詢 (例：6碼店號)", https://emap.pcsc.com.tw/); **FamilyMart 6 digits** ("共「6」碼", https://family.map.com.tw/famiport/storeNumberFreeze.aspx; consistent with ECPay stage store `006598`, F5); **OK mart 4 digits** ("店號」(共4碼)", https://www.okmart.com.tw/convenient_shopSearch; consistent with stage `1328`); **Hi-Life UNKNOWN** (official pages https://www.hilife.com.tw/storeInquiry_street.aspx and …/HI_Shipping/hilife/03-1_storeToStore.aspx state no length; search snippets unverified). Digits only for all four is an assumption from these pages' examples. | as cited |

## 2. Provider comparison (why ECPay first, carrier-neutral kept)

| Provider | 7-11 | FamilyMart | Hi-Life | OK mart | Map + store list | Create + code/label | Status callback security | Public test env | Onboarding | Fit |
| --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- |
| **ECPay 物流整合 v1** | C2C+B2C | C2C+B2C | C2C+B2C (store-code length UNKNOWN, F17; gated like OK until proven) | create only (F5); no OK `CvsType` in the store list (F10/F18), so no directory verification ⇒ OK not offered in v1 unless TCV10 finds a path | yes (F1, F10) | yes (F5, F8) | MD5 keyed CheckMacValue (F6/F7) | yes, fixed stores, **no status sim** | TW individual/entity, self-serve C2C; B2C 測標 | **Primary** |
| ECPay v2 全方位 | yes | yes | yes | not listed | hosted page | yes | AES, no MAC (F14) | yes | same | rejected (TD1) |
| NewebPay 藍新 | C2C | C2C | C2C | C2C (F15) | UNKNOWN | UNKNOWN | UNKNOWN | UNKNOWN | TW member | not in R2 (§15 Q4); later OK-mart candidate |
| PAYUNi 統一金流 | C2C | UNKNOWN | no evidence | no evidence | map (plugin doc) | yes | UNKNOWN | UNKNOWN | TW merchant | no |
| 7-11 交貨便 official | — | — | — | — | consumer only | consumer only | — | — | — | no API (F16) |
| T-cat / HCT | home | — | — | — | — | home | — | — | contract | out of scope |

None of these covers the overseas first mile; ECPay covers the Taiwan last mile only (Q2).

## 3. End-to-end flow

```
Buyer (storefront)            Go API (commerce_runtime)            ECPay                 Worker (dispatcher)
1 choose "7-ELEVEN 取貨" ─▶ POST cvs-selections ──▶ selection OPEN (nonce, 15 min)
2 ◀── signed-less form {action=Express/map, fields}  (top-level auto-submit, no iframe / new window)
3 ───────────────────────────────────────────────────▶ e-map ──(browser auto-POST)──▶
4                         POST /v1/cvs/ecpay/map-return/{selection_id} → RETURNED
                          GetStoreList (cached, signed) → match code → VERIFIED + pickup source row
5 ◀── 303 <return_origin><return_path>?cvs_selection={id}   (both stored at open, §5.2)
6 POST cvs-selections/{id}/verify (buyer cookie) → {pickup_id, name, address}
7 SetDestination(kind, pickup_id, recipient) → quote → Begin (checks §5.2) → Stripe pay (unchanged),
  or payment_mode=pay_at_pickup: order CONFIRMED at Begin, no Stripe session (§16.2)
  (store without ECPay: steps 1-6 are replaced by the buyer-entered store form, §16.1)
Merchant (admin)
8 "建立超商寄件" ─▶ POST orders/{id}/cvs-shipment → River job + cvs_shipments REQUESTED + operation (one tx, §4.3)
9                                                       ◀── Express/Create ◀── Dispatch (loads PII under lease)
10                        Complete: CREATED (AllPayLogisticsID, 寄件代碼) + order PROVIDER_LABEL_CREATED
11 merchant/forwarder prints at ibon/FamiPort/Life-ET with the code, or "列印託運單" (signed form, new tab)
12                        POST /v1/cvs/ecpay/status/{endpoint_id} ◀── postgate (MAC) → event + state → "1|OK"
13 buyer order page shows 已建立寄件 / 已到物流中心 / 已到店 / 已取件 / 未取退回
```

## 4. Persistence: `migrations/0072_taiwan_cvs_logistics.sql` (schema)

Every widened CHECK is re-derived from `pg_get_constraintdef` of the current constraint (0063/0064
may have widened it after 0010/0012/0014/0061) and only extended; TCV02/TCV04 assert no earlier value is lost.

### 4.1 Widened constraints

| Object | Change |
| --- | --- |
| `fulfillment.pickup_versions.kind`, `storefront.destination_snapshots.kind` (+ its CVS branch CHECK), `fulfillment.service_versions.delivery_kind` | add `cvs_hilife`, `cvs_okmart`. The destination CVS branch becomes `kind IN (<4 cvs kinds>) AND country='TW' AND pickup_id IS NOT NULL AND <home fields empty>`. Legacy bare pricing method keys (0007) are **not** widened; new services use `delivery:<code>`. |
| `fulfillment.pickup_versions.verification_kind` | `IN ('MANUAL_ATTESTED','PROVIDER_DIRECTORY_VERIFIED','BUYER_ENTERED')` (`BUYER_ENTERED`: §16.1, C1b); default stays `MANUAL_ATTESTED`; `principal_id` becomes nullable with `CHECK((verification_kind='BUYER_ENTERED')=(principal_id IS NULL))` (a buyer has no membership). Merchant runtime INSERT policy gains `WITH CHECK (verification_kind='MANUAL_ATTESTED')` (a merchant can never claim directory verification). The provider namespace is also closed to merchants (round 1, verified against 0012:82-98, which grants `commerce_runtime` INSERT on both tables and UPDATE(current_version,pickup_id,enabled) on heads): RESTRICTIVE policies for `commerce_runtime` — `pickup_versions` INSERT `WITH CHECK(namespace !~ '^(ecpay|buyer)\.')`; `pickup_heads` INSERT `WITH CHECK(namespace !~ '^(ecpay|buyer)\.')` and UPDATE `USING(namespace !~ '^(ecpay|buyer)\.') WITH CHECK(namespace !~ '^(ecpay|buyer)\.')` (round 3: the `buyer.` namespace of §16.1 is closed to merchants too). |
| `checkout.orders` (§16.2, §16.4) | add `payment_mode text NOT NULL DEFAULT 'card' CHECK(payment_mode IN ('card','pay_at_pickup'))` and `collection_state text CHECK(collection_state IN ('PENDING','COLLECTED','RETURNED','REFUNDED_OFFLINE','CANCELLED','RESTOCKED'))` (`CANCELLED`/`RESTOCKED`: §16.8) with `CHECK((payment_mode='pay_at_pickup')=(collection_state IS NOT NULL))`; existing rows backfill `card`/NULL. |
| `inventory.ledger` `ledger_checkout_actor` (latest 0061:142, re-derived) + `checkout.events` `events_action_check`/`events_actor_action` (latest 0061:188, re-derived) | ledger: one more branch `actor_kind='BUYER' AND kind='ALLOCATE' AND principal_id IS NULL AND checkout_id IS NOT NULL AND buyer_owner_id IS NOT NULL AND buyer_session_id IS NOT NULL AND reservation_id=checkout_id AND payment_attempt_id IS NULL AND payment_fact_kind IS NULL AND operation='checkout.pay_at_pickup.commit' AND command_key=checkout_id::text` (§16.2; X9: the non-NULL checkout/buyer columns match every other non-MERCHANT branch, 0061:146-154, so a NULL checkout_id cannot pass the CHECK as NULL), plus the §16.8 `DEALLOCATE` kind, its MERCHANT branch and the `guard_checkout_ledger` DEALLOCATE branch; events: one action `checkout.pay_at_pickup_placed` with `actor_kind='BUYER'` (collection changes are audited in `ops`/audit and `cvs_shipment_events`, not `checkout.events`, whose actor kinds are BUYER/SYSTEM_EXPIRY/SYSTEM_PAYMENT only). |
| `fulfillment.service_versions` | drop `CHECK(mode<>'API' OR NOT enabled)`; add `CHECK(mode<>'API' OR NOT enabled OR binding_id IS NOT NULL)` plus constraint trigger `fulfillment.guard_api_service_binding()` (AFTER INSERT): an enabled API row's binding must belong to an `ecpay_logistics` account of the same tenant/store whose profile is `enabled` and `qualified_credential_version = account.credential_version`, and `delivery_kind` must be a CVS kind supported by the profile's mode (`cvs_okmart` only if `ok_verified`, `cvs_hilife` only if `hilife_verified`, §4.2). (R-2) |
| `integration.merchant_accounts.provider` | `IN ('payuni','stripe','ecpay_logistics')`; `CHECK(provider<>'ecpay_logistics' OR account_id ~ '^[A-Za-z0-9]{1,10}$')`; partial unique `(tenant_id,store_id,environment) WHERE provider='ecpay_logistics'` (TD2); global identity `CREATE UNIQUE INDEX ecpay_logistics_identity_unique ON integration.merchant_accounts(environment,account_id) WHERE provider='ecpay_logistics'` (same rule as 0061 `stripe_account_identity_unique`; one ECPay MerchantID serves one store — a multi-store merchant needs one ECPay account per store until the owner waives this). Merchant runtime INSERT/UPDATE policies stay `provider='payuni'`; ecpay rows only via the §4.3 definer. |
| `integration.operations.operation_actor_family` | (round 1: there is no provider/action/purpose CHECK; the real constraint is `operation_actor_family`, 0016 → 0035 → 0061 → 0062.) Its MERCHANT branch already admits any provider/action, so it is **not** widened. CVS create uses `actor_kind='MERCHANT'`. 0072 adds a separate narrowing CHECK `cvs_create_family CHECK(provider<>'ecpay_logistics' OR (action='ecpay.cvs_create' AND purpose='transactional' AND actor_kind='MERCHANT'))`. If a later migration must touch `operation_actor_family`, it re-derives it via `pg_get_constraintdef` with the 0062 shape assert. |
| `checkout.orders.fulfillment_state` | add `PROVIDER_LABEL_CREATED` (R-4). Deferred constraint trigger `fulfillment.guard_cvs_shipment_state()` on both `checkout.orders` (AFTER UPDATE OF fulfillment_state) and `fulfillment.cvs_shipments`: `fulfillment_state='PROVIDER_LABEL_CREATED' ⇔ shipment.state IN ('CREATED','AT_DC','AT_STORE','PICKED_UP','UNCLAIMED')`. |

### 4.2 New tables (all FORCE RLS, PUBLIC revoked, `COMMENT ON` per PROCESS §5)

```sql
CREATE TABLE integration.ecpay_logistics_profiles (           -- one per ecpay_logistics account
 tenant_id uuid NOT NULL, store_id uuid NOT NULL, connection_id uuid NOT NULL,
 mode text NOT NULL CHECK(mode IN ('C2C','B2C')),                -- ECPay application type (F5)
 endpoint_id uuid NOT NULL UNIQUE,                                -- status route id, not a secret
 enabled boolean NOT NULL,
 qualified_credential_version bigint,                             -- GetStoreList probe passed with this version
 qualified_at timestamptz, ok_verified boolean NOT NULL DEFAULT false,   -- set only by TCV10 evidence (registrar)
 hilife_verified boolean NOT NULL DEFAULT false,                  -- set only by TCV07/TCV12 evidence (registrar, F17)
 version bigint NOT NULL CHECK(version>0), updated_at timestamptz NOT NULL,
 PRIMARY KEY(tenant_id,store_id,connection_id),
 CHECK((qualified_credential_version IS NULL)=(qualified_at IS NULL)),
 FOREIGN KEY(tenant_id,store_id,connection_id) REFERENCES integration.merchant_accounts(tenant_id,store_id,id)
);
CREATE UNIQUE INDEX ecpay_one_enabled_profile ON integration.ecpay_logistics_profiles(tenant_id,store_id)
 WHERE enabled;                                                   -- no SANDBOX+LIVE ambiguity per store
CREATE TABLE fulfillment.cvs_selections (                       -- one buyer map round-trip
 tenant_id uuid NOT NULL, store_id uuid NOT NULL, owner_id uuid NOT NULL, id uuid NOT NULL,
 session_id uuid NOT NULL, cart_id uuid NOT NULL, cart_version bigint NOT NULL CHECK(cart_version>0),
 kind text NOT NULL CHECK(kind IN ('cvs_711','cvs_familymart','cvs_hilife','cvs_okmart')),
 connection_id uuid NOT NULL, credential_version bigint NOT NULL, logistics_subtype text NOT NULL
  CHECK(logistics_subtype IN ('UNIMARTC2C','FAMIC2C','HILIFEC2C','OKMARTC2C','UNIMART','FAMI','HILIFE')),
 nonce_sha256 bytea NOT NULL UNIQUE CHECK(octet_length(nonce_sha256)=32),   -- MerchantTradeNo digest
 state text NOT NULL CHECK(state IN ('OPEN','RETURNED','VERIFIED','REJECTED','EXPIRED')),
 returned_store_id text CHECK(returned_store_id ~ '^[A-Za-z0-9]{1,9}$'),
 returned_outside boolean, reject_code text CHECK(reject_code ~ '^[a-z_]{1,40}$'),
 pickup_id uuid, country text NOT NULL DEFAULT 'TW' CHECK(country='TW'),
 return_origin text NOT NULL,                                     -- the store's ACTIVE storefront_domains.origin (round 2)
 return_path text NOT NULL CHECK(return_path ~ '^/(zh-TW|zh-CN|en)/products/[A-Za-z0-9_-]{1,64}$'),  -- only route with checkout (OrderFlow)
 created_at timestamptz NOT NULL, expires_at timestamptz NOT NULL,
 updated_at timestamptz NOT NULL, version bigint NOT NULL CHECK(version>0),
 PRIMARY KEY(tenant_id,store_id,owner_id,id), UNIQUE(id),
 CHECK(expires_at>created_at AND expires_at<=created_at+interval '15 minutes'),
 CHECK((state='VERIFIED')=(pickup_id IS NOT NULL)), CHECK((state='REJECTED')=(reject_code IS NOT NULL)),
 FOREIGN KEY(tenant_id,store_id,owner_id,cart_id) REFERENCES storefront.carts(tenant_id,store_id,owner_id,id),
 FOREIGN KEY(tenant_id,store_id,owner_id,session_id) REFERENCES buyer.capability_sessions(tenant_id,store_id,owner_id,id),
 FOREIGN KEY(tenant_id,store_id,connection_id) REFERENCES integration.ecpay_logistics_profiles(tenant_id,store_id,connection_id),
 FOREIGN KEY(tenant_id,store_id,pickup_id,kind,country) REFERENCES fulfillment.pickup_versions(tenant_id,store_id,id,kind,country)  -- existing 0012 UNIQUE
);
CREATE TABLE fulfillment.cvs_shipments (                        -- MD1: at most one live shipment per order
 tenant_id uuid NOT NULL, store_id uuid NOT NULL, owner_id uuid NOT NULL, order_id uuid NOT NULL,
 attempt smallint NOT NULL CHECK(attempt BETWEEN 1 AND 5),       -- new attempt only after ABANDONED/FAILED
 connection_id uuid NOT NULL, credential_version bigint NOT NULL, environment text NOT NULL CHECK(environment IN ('SANDBOX','LIVE')),
 logistics_subtype text NOT NULL, receiver_store_id text NOT NULL CHECK(receiver_store_id ~ '^[A-Za-z0-9]{1,6}$'),  -- F17: S(6) is a maximum
 pickup_id uuid NOT NULL, goods_amount integer NOT NULL CHECK(goods_amount BETWEEN 1 AND 20000),  -- integer TWD (major units), F5/F17
 collection_amount integer CHECK(collection_amount BETWEEN 1 AND 20000),  -- §16.3: NULL = card order; set = pay-at-pickup, = order total TWD
 merchant_trade_no text NOT NULL CHECK(merchant_trade_no ~ '^LC[A-Z2-7]{18}$'),
 operation_id uuid NOT NULL UNIQUE,
 state text NOT NULL CHECK(state IN ('REQUESTED','CREATED','FAILED','UNKNOWN','AT_DC','AT_STORE',
   'PICKED_UP','UNCLAIMED','ABANDONED')),
 -- Provider codes (round 2): lengths/charsets are UNVERIFIED beyond F8 (7-11 payment no 8 + validation 4), so
 -- the CHECKs are generous display-safe bounds; a value outside them is stored NULL by the writer, never raised.
 provider_logistics_id text CHECK(provider_logistics_id ~ '^[0-9A-Za-z_-]{1,40}$'),
 cvs_payment_no text CHECK(cvs_payment_no ~ '^[0-9A-Za-z_-]{1,40}$'),
 cvs_validation_no text CHECK(cvs_validation_no ~ '^[0-9A-Za-z_-]{1,40}$'),
 shipment_no text CHECK(shipment_no ~ '^[0-9A-Za-z_-]{1,40}$'),   -- B2C, from query (F5)
 last_status_code text CHECK(last_status_code ~ '^[0-9]{1,8}$'), last_status_at timestamptz,
 result_code text CHECK(result_code ~ '^[a-z0-9_.]{1,80}$'),
 principal_id uuid NOT NULL, created_at timestamptz NOT NULL, updated_at timestamptz NOT NULL,
 version bigint NOT NULL CHECK(version>0),
 PRIMARY KEY(tenant_id,store_id,order_id,attempt),
 UNIQUE(connection_id,merchant_trade_no),
 CHECK(state NOT IN ('CREATED','AT_DC','AT_STORE','PICKED_UP','UNCLAIMED') OR provider_logistics_id IS NOT NULL),
 CHECK(collection_amount IS NULL OR collection_amount=goods_amount),   -- F20: CollectionAmount must equal GoodsAmount
 FOREIGN KEY(tenant_id,store_id,owner_id,order_id) REFERENCES checkout.orders(tenant_id,store_id,owner_id,id),
 FOREIGN KEY(tenant_id,store_id,connection_id) REFERENCES integration.ecpay_logistics_profiles(tenant_id,store_id,connection_id),
 FOREIGN KEY(tenant_id,principal_id) REFERENCES identity.memberships(tenant_id,principal_id)
);
CREATE UNIQUE INDEX cvs_shipments_one_live ON fulfillment.cvs_shipments(tenant_id,store_id,order_id)
 WHERE state NOT IN ('FAILED','ABANDONED');
CREATE TABLE fulfillment.cvs_shipment_events (                  -- append-only; status receipts + local transitions
 tenant_id uuid NOT NULL, store_id uuid NOT NULL, order_id uuid NOT NULL, attempt smallint NOT NULL,
 id uuid NOT NULL DEFAULT gen_random_uuid(),
 source text NOT NULL CHECK(source IN ('local','ecpay_status','ecpay_query')),
 event_code text CHECK(event_code ~ '^[a-z0-9_.]{1,80}$'),        -- local events, e.g. ecpay.code_nonconforming (round 2)
 body_sha256 bytea CHECK(body_sha256 IS NULL OR octet_length(body_sha256)=32),
 provider_code text CHECK(provider_code ~ '^[0-9]{1,8}$'),        -- RtnCode / LogisticsStatus; non-conforming ⇒ NULL (§7.5)
 provider_message text CHECK(char_length(provider_message)<=200 AND provider_message !~ '[[:cntrl:]]'),
 provider_updated_at text CHECK(provider_updated_at ~ '^[0-9]{4}/[0-9]{2}/[0-9]{2} [0-9]{2}:[0-9]{2}:[0-9]{2}$'),  -- non-conforming ⇒ NULL (§7.5)
 from_state text, to_state text, received_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 PRIMARY KEY(tenant_id,store_id,order_id,attempt,id),
 UNIQUE(tenant_id,store_id,order_id,attempt,body_sha256),         -- duplicate notification = no-op
 FOREIGN KEY(tenant_id,store_id,order_id,attempt) REFERENCES fulfillment.cvs_shipments(tenant_id,store_id,order_id,attempt)
);
```

- `cvs_shipments` is mutable only through the §4.3 definers with `expected_version` CAS
  (+1 per change, trigger enforced); identity columns (connection, credential_version, subtype,
  store, amount, collection_amount, trade no, operation) are immutable (trigger). Events are append-only.
- No recipient name/phone/address column anywhere in these tables (TD8); the worker reads them
  from the frozen order snapshot (`checkout.orders.snapshot->'destination'`).
- Credentials: `integration.account_credentials` unchanged; ciphertext payload `ecpay-logistics-v1`
  = JSON `{hash_key, hash_iv, sender_name, sender_cell_phone}` (sender data is merchant contact
  data required by F5; kept encrypted to avoid new PII columns). AAD binds tenant/store/connection/
  provider/environment/merchant_id/version (R-1).
- Pickup rows written by §4.3: `kind` from the selection, `namespace = 'ecpay.' || lower(environment)
  || '.' || lower(subtype)` (e.g. `ecpay.sandbox.unimartc2c`; stage and LIVE stores never share a head),
  `code` = `StoreId` exactly as the directory returns it (string, leading zeros kept), `name`/`address`
  from the directory row, `verification_kind='PROVIDER_DIRECTORY_VERIFIED'`, `evidence_ref =
  'ecpay-dir:' || selection_id`, `principal_id` per R-3, `valid_until = attested_at + 24 hours`
  (directory refreshes daily at 20:00, F10). The head's current version is reused (no new version)
  **only if** it has `verification_kind='PROVIDER_DIRECTORY_VERIFIED'` AND the same name and address AND
  `valid_until > now() + interval '1 hour'` AND the head is enabled; otherwise one new version is appended
  with head CAS (same advisory key and lock order as `AttestPickup`). Merchants cannot write `ecpay.*`
  heads at all (§4.1), so a merchant-attested row can never be reused as verified.

### 4.3 SQL entry points (`migrations/0073_taiwan_cvs_functions.sql`; SECURITY DEFINER, `search_path=pg_catalog`, PUBLIC revoked)

| Function | Owner | EXECUTE | Contract |
| --- | --- | --- | --- |
| `integration.register_ecpay_logistics(hash bytea, store uuid, key text, request_hash bytea, expected_version bigint, environment text, merchant_id text, mode text, key_id text, nonce bytea, ciphertext bytea, probe_ok boolean, p_payment_environment text) RETURNS jsonb` | integration_writer | commerce_runtime | Merchant token, `integration:manage`, fresh final auth (0027 pattern). **Environment pin (round 2):** `environment` must equal `p_payment_environment` — the deployment payment environment Go derives from `COMMERCE_PAYMENT_PROFILE` (cmd/api/buyer_payment.go:44; `PROVIDER_MOCK`⇒`SANDBOX`) — else PT422 `ecpay_environment_not_allowed` (HTTP 422, nothing written), so a LIVE deployment can never hold a SANDBOX (public stage merchant `2000933`, F11) profile. `expected_version=0` inserts binding + account + credential v1 + profile (new `endpoint_id`), else appends credential `version+1` (rotation) and bumps profile. `probe_ok` must be true (Go ran GetStoreList with the plaintext before calling; §7.3) → `qualified_*` = new version. LIVE rows allowed (read-only probe), LIVE *create* is gated in Go (§12). Command result + audit `logistics.ecpay.connected/rotated`. Never returns key material. |
| `integration.set_ecpay_logistics_enabled(hash, store, key, request_hash, expected_version, enabled)` | integration_writer | runtime | Disable stops new selections/creates; in-flight status ingress and query continue (merchant-service-settings §4). |
| `fulfillment.open_cvs_selection(buyer_hash bytea, store uuid, key text, request_hash bytea, cart_version bigint, market uuid, service_code text, nonce_sha256 bytea, return_origin text, return_path text, p_payment_environment text) RETURNS jsonb` | checkout_writer | `commerce_checkout_runtime` (buyer route, checkout pool; see pool note below) | Buyer scope (buyer.WithScope pattern, before replay). `return_origin` = the storefront origin Go already authenticated for this buyer request (`X-Commerce-Storefront-Origin` after the BFF-key check, internal/buyerhttp/handler.go:372-380, resolved by `buyer.resolve_published_store`); the definer requires it to equal an ACTIVE `control.storefront_domains.origin` of this store (published), else PT422 `bad_return_origin`. The profile's environment must equal `p_payment_environment` (same derivation as register) else PT422 `service_unavailable` (round 2). Locks cart FOR SHARE (current, nonempty), service head → current enabled+visible CVS service in TW; the store's **single** enabled, qualified profile (unique index `ecpay_one_enabled_profile`; `cvs_okmart` requires `ok_verified`, `cvs_hilife` requires `hilife_verified`); maps kind+mode → subtype (`cvs_711`→UNIMARTC2C/UNIMART, `cvs_familymart`→FAMIC2C/FAMI, `cvs_hilife`→HILIFEC2C/HILIFE, `cvs_okmart`→OKMARTC2C, C2C only). `return_path` must match the table CHECK (Go validates first; never read from the map-return body) else PT422 `bad_return_path`. Inserts OPEN row, `expires_at = now+15 min`. Returns `{selection_id, subtype, merchant_id, environment, expires_at}`. Rate limit: ≤10 OPEN per owner/cart (PT429). |
| `fulfillment.record_cvs_map_return(selection uuid, nonce_sha256 bytea, merchant_id text, subtype text, store_id text, outside text) RETURNS jsonb` | checkout_writer | runtime (unauthenticated route) | **Check order is part of the contract** (round 1): (1) lock selection FOR UPDATE; unknown selection → `ignored`. (2) If `state<>'OPEN'` or the nonce digest differs (constant-time compare of the 32-byte digests) → return `ignored`, **no write** — whoever only knows a `selection_id` (it is in the storefront URL and ECPay's ServerReplyURL) cannot change or burn the selection. A repeat of the already-accepted return (state RETURNED, same digest, same store id) is also `ignored`. (3) Only after the nonce matches: expired (DB clock) → REJECTED `expired`; `merchant_id ≠ account.account_id` → `merchant_mismatch`; subtype ≠ selection's → `subtype_mismatch`; store id not `^[A-Za-z0-9]{1,9}$` → `bad_store_id`. (4) Else stores RETURNED. `reject_code` values: `expired`, `merchant_mismatch`, `subtype_mismatch`, `bad_store_id` (no `nonce_mismatch`). |
| `fulfillment.verify_cvs_selection(selection uuid, store_id text, directory_hit boolean, dir_name text, dir_address text, fetched_at timestamptz) RETURNS jsonb` | checkout_writer | `commerce_runtime` (map-return route) + `commerce_checkout_runtime` (buyer verify route, only after `read_cvs_selection` proved owner/session/cart) | Called by Go after the GetStoreList lookup (outside any tx). Locks selection; requires RETURNED and same `returned_store_id`. `directory_hit=false` → REJECTED `store_not_in_directory`. Store code for create is at most 6 alnum (F17: `ReceiverStoreID` S(6) is a maximum; OK stage store `1328` has 4); a longer code → REJECTED `store_code_length`. Writes/reuses the pickup source (§4.2, environment-scoped namespace) and sets VERIFIED + `pickup_id`. |
| `fulfillment.read_cvs_selection(buyer_hash, store, selection) RETURNS jsonb` | checkout_writer | `commerce_checkout_runtime` | Owner + session + cart must match the buyer scope; returns state, reject_code, and for VERIFIED the pickup projection (id, kind, code, name, address, outside). Other owners: indistinguishable not-found. |
| `checkout.begin_hold(<current 8 args>, p_payment_environment text, p_payment_mode text)` (replaced; 10 args after round 3, §16.2; re-derive from the latest definition — 0013 as replaced by `post_river/0005:98`). **Lives in the new post_river file, not 0073** (round 2, found while verifying: migrate.go applies every main migration, then River, then post_river, so on a fresh install `post_river/0005`'s `CREATE OR REPLACE` would re-create the old 8-arg function after 0073). That file DROPs the 8-arg signature and creates the 10-arg one with the same owner/grants; `internal/checkout/checkout.go:211` passes the deployment payment environment. TCV02/TCV04 assert exactly one `checkout.begin_hold` exists. | unchanged owner | unchanged | Only deltas: (a) accept `verification_kind IN ('MANUAL_ATTESTED','PROVIDER_DIRECTORY_VERIFIED')`, and `BUYER_ENTERED` under the §16.1 rules; (b) accept `v_service.mode='API'` when enabled+visible and the bound profile is enabled/qualified for the destination kind (same predicate as §4.1 trigger); (c) for API services and for any `PROVIDER_DIRECTORY_VERIFIED` pickup: quote currency must be `TWD`; **amount in TWD minor units (0061 convention, ×100)**: `items_subtotal_minor % 100 = 0 AND items_subtotal_minor BETWEEN 100 AND 2000000` (= GoodsAmount 1..20000 TWD, F5/F17; 7-ELEVEN also refuses goods > 20000 TWD) → else PT422 `cvs_amount_exceeds`; recipient passes `fulfillment.ecpay_recipient_ok(name, phone)` → else PT422 `cvs_recipient_rejected`; the pickup namespace environment/subtype must equal the store's single enabled profile's environment and a subtype of its `mode` → else PT422 `cvs_environment_mismatch`. **The ECPay environment must equal the deployment payment environment** (round 2): a deployment runs exactly one payment profile, `COMMERCE_PAYMENT_PROFILE` (cmd/api/buyer_payment.go:44; production = LIVE per stripe-live-enable LD1/LQ5; `PROVIDER_MOCK`⇒`SANDBOX`), which Go passes to `register_ecpay_logistics`, `open_cvs_selection` and `begin_hold` as `p_payment_environment`. Register refuses any other environment (422 `ecpay_environment_not_allowed`), open refuses a profile of another environment (422 `service_unavailable`), and Begin refuses a pickup namespace or profile of another environment (PT422 `cvs_environment_mismatch`) before any hold or payment attempt. The request-time check against the CAPTURED payment fact (`request_cvs_shipment`) stays as a second guard. Lock order unchanged (pickup head → destination head → service head → profile FOR SHARE → allocation…). (d) the `p_payment_mode` branch, CVS store settings and C3 recipient rule of §16.2/§16.5. |
| `fulfillment.ecpay_recipient_ok(name text, phone text) RETURNS boolean` IMMUTABLE | fulfillment owner | runtime, checkout_writer, worker | Name: no digits/ASCII symbols/emoji, width 4..10 where CJK/full-width = 2 (F5). Phone: digits only after removing spaces/hyphens/parens; `+8869xxxxxxxx` normalised to `09xxxxxxxx`; must match `^09[0-9]{8}$`. Go mirrors it (UNIT golden table shared via `testdata`). |
| `fulfillment.request_cvs_shipment(hash bytea, store uuid, order uuid, key text, request_hash bytea, expected_version bigint, p_operation uuid, p_job bigint) RETURNS jsonb` | checkout_writer | runtime | `fulfillment:write` via `identity.resolve_access` before any lock (sets tenant/store/principal GUCs; §16.8 Route pattern). Replay check first after that: a `ops.command_results` hit for (`fulfillment.cvs_shipment.request`, key) — same or different request hash — RAISEs SQLSTATE `PT2RP` (round 2). Lock order: order FOR UPDATE → work item → refund rows → manual head (must be absent or VOIDED) → cvs shipments of the order. First **settles** a latest attempt that is still REQUESTED/UNKNOWN from its operation (`fulfillment.settle_cvs_attempt`, below). Then the latest attempt must be absent, FAILED or ABANDONED (`expected_version` = that row's version, 0 = none). Eligibility = the replaced `fulfillment.manual_shipment_eligible` (below) **and** order destination is a CVS kind with a `PROVIDER_DIRECTORY_VERIFIED` pickup whose namespace environment+subtype match the store's single enabled/qualified profile **and**, for a `card` order, profile environment = the environment of the order's CAPTURED payment fact (`payments.facts.environment`; a `PROVIDER_MOCK` execution profile counts as SANDBOX); for a `pay_at_pickup` order (no payment fact exists: `checkout.start_payment` post_river/0005:372 and `start_stripe_payment` post_river/0012:171 refuse a non-DRAFT order), the pickup-namespace environment check above is the environment guard (it was pinned to `p_payment_environment` at Begin, §4.3 (c)) → else PT422 `cvs_environment_mismatch` (a buyer who paid LIVE never gets a stage label; round 4, R4-1). Freezes connection, credential_version, environment, subtype, `receiver_store_id`, `goods_amount = items_subtotal_minor / 100` (integer TWD, 1..20000, 0061 convention) for a card order, or for a `pay_at_pickup` order `collection_amount = goods_amount = orders.total_minor / 100` (§16.3; F20), `merchant_trade_no = 'LC' || base32(sha256(p_operation))[:18]`, `operation_id = p_operation`. **Planning (round 2, rewritten round 3):** Go generates `p_operation` (crypto/rand UUIDv4), calls River `InsertTx(externalOperationArgs{OperationID:p_operation,Version:1})` as `commerce_runtime`, then calls this definer in the same READ COMMITTED tx (`core.Service.Plan` is not used: service.go:179-260 authorizes `integration:execute` and derives the operation id itself). Planning is delegated to `integration.plan_cvs_create(p_tenant uuid, p_store uuid, p_order uuid, p_attempt smallint, p_principal uuid, p_connection uuid, p_operation uuid, p_job bigint) RETURNS void`. It is SECURITY DEFINER, `search_path=pg_catalog`, owned by commerce_integration_writer, with EXECUTE granted only to commerce_checkout_writer, and is called by request_cvs_shipment after its auth and eligibility checks. It verifies the job exactly as 0064:861-864 does, locks the binding FOR SHARE through the existing worker_binding_lock (0008:108/118), and inserts the operation plus the READY event through a new policy `cvs_create_insert` FOR INSERT TO commerce_integration_writer WITH CHECK(provider='ecpay_logistics' AND action='ecpay.cvs_create' AND purpose='transactional' AND actor_kind='MERCHANT'). commerce_checkout_writer gets no river privilege, no INSERT on operations/events and no bindings policy. It keeps one SELECT policy `checkout_cvs_operation_read` on integration.operations: USING(app tenant/store GUCs AND provider='ecpay_logistics' AND action='ecpay.cvs_create'), with no principal clause. Operation fields written by `plan_cvs_create` (unchanged from round 2): `actor_kind='MERCHANT'`, `principal_id = p_principal`, `binding_id`/`binding_version` = the connection's `merchant_accounts.binding_id` and its `bindings.semantic_version` (binding must be enabled, else 22023), `provider='ecpay_logistics'`, `external_asset_id` from the binding, `action='ecpay.cvs_create'`, `purpose='transactional'`, `semantic_key = 'cvs:'||order||':'||attempt` (matches 0008 `^[A-Za-z0-9_.:-]{8,128}$`), `request = {order_id, attempt}` only (TD8), `request_hash = sha256(request::text)`, `job_id = p_job`, READY `operation_events` row (generation 0, `operation_planned`). A job from another tx or with other args → 22023. request_cvs_shipment then writes the shipment, command result and audit `fulfillment.cvs_shipment_requested`. No `integration:execute` is required. On `PT2RP` Go rolls back (dropping its job; post_river 0014's deferred `external_operation_job_commit` would in any case refuse to commit a job without its operation) and answers from the stored result via `fulfillment.read_cvs_shipment_command` (below). Returns `{attempt, state:'REQUESTED', operation_id}`. |
| `fulfillment.read_cvs_shipment_command(hash bytea, store uuid, key text, request_hash bytea) RETURNS jsonb` | checkout_writer | runtime | Writes no shipment, operation, job or command row. Same merchant auth + `fulfillment:write`; returns the stored `command_results` body for the key, or PT409 `idempotency_conflict` when the stored request hash differs. |
| `fulfillment.manual_shipment_eligible(tenant, store, order)` (REPLACED in 0073, re-derived from 0063) | unchanged (checkout_writer, INVOKER) | unchanged | Body = 0063 body with the payment/review/refund clauses moved verbatim into `fulfillment.order_money_shippable(tenant,store,order)` (INVOKER, same owner/grants; round 3: those card clauses plus `payment_mode='card'`, OR the pay-at-pickup branch of §16.2) plus one extra clause: `AND NOT EXISTS(SELECT 1 FROM fulfillment.cvs_shipments c WHERE c.tenant_id=o.tenant_id AND c.store_id=o.store_id AND c.order_id=o.id AND c.state NOT IN ('FAILED','ABANDONED'))`. So `record_manual_shipment` (unchanged) refuses a manual record while an ECPay attempt is REQUESTED/UNKNOWN/CREATED… (round 1, finding 1; PT422 `not_shippable`), and `request_cvs_shipment` uses the same predicate (its own one-live check stays as a second guard). `commerce_auth` gains SELECT(tenant_id,store_id,order_id,state) on `cvs_shipments` via the 0027 merchant-projection policy so the INVOKER call still works for projections. |
| `fulfillment.cvs_order_payable(tenant, store, order) RETURNS boolean` | checkout_writer (SECURITY DEFINER) | integration_writer only | Sets `app.tenant_id`/`app.store_id` tx-locally from its arguments, returns `fulfillment.order_money_shippable(...)` (the MD6 refund + review + captured-payment clauses, no fulfillment_state/live-shipment clauses), then resets them to `''`. Called only from `load_cvs_create` in dispatch mode, which derives tenant/store/order from the lease-fenced operation row. Boolean only. |
| `fulfillment.settle_cvs_attempt(tenant, store, order) RETURNS void` | checkout_writer (INVOKER helper, not granted) | — | Closes the gaps no dispatcher completion reaches (verified 0008 `claim_operation`: a binding change writes `STALE_BINDING` or `UNKNOWN binding_changed` inside the claim, never via Complete). For the latest attempt in REQUESTED or UNKNOWN, reads its operation: `STALE_BINDING`/`BLOCKED_POLICY`/`CANCELLED` → shipment FAILED `ecpay.not_sent.<result_code>` (nothing was sent; a new attempt is safe); `UNKNOWN` with `lease_until IS NULL` and `result_code IN ('binding_changed','reconcile_budget_exhausted')` → shipment UNKNOWN (unchanged if already); **an operation in `DISPATCHING`/`UNKNOWN` whose `lease_until < now() - interval '1 hour'`** (a completion that keeps failing, or a job River discarded) → shipment UNKNOWN (abandonable with acknowledgement; round 2). Called by `request_cvs_shipment`, `abandon_cvs_shipment` and `read_cvs_shipment`'s VOLATILE merchant variant. Idempotent. |
| `integration.load_cvs_create(operation uuid, generation bigint, lease_token bytea, mode text) RETURNS TABLE(...)` | integration_writer | commerce_worker | The route's `LoadSecret` body (only place for a lease-fenced SQL loader; `Check` has no tx and never sees a Secret — `DispatchRoute` comment, 2026-09-29). Lease fence (token digest + generation + unexpired, DB clock, after waits) like `load_stripe_credential`. **Dispatch mode**: raises `ErrPolicyDenied`-mapped SQLSTATE (→ BLOCKED_POLICY, zero requests) unless the shipment is still REQUESTED, the profile is enabled with the frozen credential version, and `fulfillment.cvs_order_payable(...)` is true (round 1, finding 2: a refund or review committed after the request stops the label purchase). **Reconcile mode** (R-7b): only the lease fence and frozen credential; no payable check (the create may exist). Returns the frozen shipment fields (incl. `collection_amount`, §16.3), recipient name/phone from `checkout.orders.snapshot->'destination'`, merchant_id, key_id/nonce/ciphertext of the **frozen** credential version, and the status `endpoint_id`. No current-head fallback. A refund landing between this load and the ECPay call is a known sub-second window; the merchant sees the §6 refunded-order warning. |
| `integration.finish_cvs_create(operation, generation, lease_token, outcome text, outcome_code text, logistics_id text, payment_no text, validation_no text, shipment_no text, status_code text)` | integration_writer | commerce_worker | Called by the route's `Finish` hook (R-7a) inside the dispatcher's completion tx, **before** `integration.complete_operation` (which runs last in the same tx and clears the lease); codes come from the adapter-private `Outcome.Detail` struct. Lease-fenced. After the lease fence it calls `fulfillment.apply_cvs_create_result(p_tenant,p_store,p_order,p_attempt,p_operation,outcome,outcome_code,logistics_id,payment_no,validation_no,shipment_no,status_code)`. That function is SECURITY DEFINER, owned by commerce_checkout_writer, with EXECUTE granted only to commerce_integration_writer. It performs the row lock, the NULL-normalisation plus `ecpay.code_nonconforming` event, the state mapping and the order column; finish_cvs_create writes no fulfillment table itself (round 3). **Finish never fails on provider data** (round 2; R-7a): `apply_cvs_create_result` locks the shipment row FOR UPDATE and takes no caller version; any code (`logistics_id` excepted, see adapter rule) not matching its column CHECK is stored NULL, plus one local event `event_code='ecpay.code_nonconforming'` with `body_sha256 = sha256(<field name> || ':' || <raw value>)` only (raw value never stored or logged); `status_code` not `^[0-9]{1,8}$` → NULL. The adapter classifies SUCCEEDED only when `AllPayLogisticsID` matches the column CHECK `^[0-9A-Za-z_-]{1,40}$`, else UNKNOWN, so Finish never sees a non-conforming id. A NULL `payment_no` makes `print_available=false` and the admin shows "代碼請至綠界後台查看". Mapping: SUCCEEDED → CREATED (+codes), order `PROVIDER_LABEL_CREATED`, local event; FAILED_FINAL → FAILED (order stays `MANUAL_UNASSIGNED`); UNKNOWN (incl. `reconcile_budget_exhausted`, `callback_timeout`, `secret_load_failed`) → UNKNOWN; BLOCKED_POLICY (`policy_denied`, `binding_changed`, `credential_unavailable`) and CANCELLED → FAILED with `result_code = 'ecpay.not_sent.' || outcome_code` (zero requests were sent). Paths that bypass Complete (claim-side `STALE_BINDING`) are settled by `settle_cvs_attempt`. Idempotent: an attempt already CREATED (or later) by status ingress or an earlier finish is a no-op; an attempt already FAILED/ABANDONED (e.g. abandoned after the 1-hour settle rule) → event only + merchant alert `duplicate_label_risk`, never a state change; a later query result may move UNKNOWN → CREATED, or → FAILED only when ECPay's query proves the trade does not exist (only if TCV06/TCV07 show a distinct not-found code; else stays UNKNOWN). |
| `fulfillment.ingest_ecpay_status(endpoint uuid, body_sha256 bytea, merchant_id text, merchant_trade_no text, logistics_id text, rtn_code text, rtn_msg text, update_date text, payment_no text, validation_no text) RETURNS text` | checkout_writer | runtime | Called after Go verified CheckMacValue with the endpoint's connection keys. Resolves the shipment by `(connection_id, merchant_trade_no)` and requires `provider_logistics_id` equal (or fills it when the shipment is UNKNOWN and the report proves the trade). Returns `retry` when the attempt is still REQUESTED (Go answers 503 without `1|OK`; ECPay retries, and the create's own Finish decides first). Normalises before insert: `rtn_code` not `^[0-9]{1,8}$` → NULL; `update_date` not matching the CHECK → NULL; `rtn_msg` control chars stripped and truncated to 200 chars (a vendor variant never aborts the tx and loses the report). Inserts event (dup `body_sha256` → no-op, still ACK). Applies §6 mapping with version CAS; unknown code or non-monotone jump → event only. FAILED/ABANDONED attempt → event only + merchant alert `duplicate_label_risk` (never a state change, never a unique-index violation). Unknown trade → `ignored` (ACK anyway, nothing written except a bounded counter log line). |
| `fulfillment.abandon_cvs_shipment(hash, store, order, key, request_hash, expected_version, query_status text, query_at timestamptz)` | checkout_writer | runtime | `fulfillment:write`; runs `settle_cvs_attempt` first. **CREATED**: allowed only when DB now > `created_at + validity(subtype)` (F9: 5/6/7 days) AND no status event beyond creation AND a MAC-verified `QueryLogisticsTradeInfo/V5` made by Go (merchant key loader, below) with `query_at` within the last 10 minutes (DB clock) returned a `LogisticsStatus` in the created-only set (`fulfillment.ecpay_created_only_codes`, initially `{'300'}` per F19, extended only with TCV07/TCV12 evidence) → else PT409 `ecpay_shows_movement` (status reports are not real-time, F7; the parcel may already be dropped). **UNKNOWN** after `reconcile_budget_exhausted`/`binding_changed` or the 1-hour settle rule: requires `i_checked_ecpay_backend=true`; if a query is possible it is run, and a MAC-verified found trade (id matching the column CHECK) is applied as UNKNOWN → CREATED (+codes, `ecpay_query` event) and answered 409 `ecpay_trade_found` — a returned outcome, not a RAISE, so the CREATED write commits (round 2: never leaves a bought label stuck in UNKNOWN). Sets ABANDONED, order back to `MANUAL_UNASSIGNED`, audit. The definer itself never calls ECPay. |
| API-side key loaders (round 1, finding 8; R-1): `integration.load_ecpay_key_for_status(endpoint uuid)`, `integration.load_ecpay_key_for_selection(selection uuid)` (only OPEN/RETURNED, unexpired selections), `integration.load_ecpay_key_for_merchant(hash bytea, store uuid)` (`fulfillment:write`, fresh auth; print form + abandon query) | integration_writer | `commerce_runtime`; `load_ecpay_key_for_selection` also `commerce_checkout_runtime` (buyer verify retry, after `read_cvs_selection`) | Each returns `{connection_id, merchant_id, environment, credential_version, key_id, nonce, ciphertext}` of the **current** credential of the one connection reachable from its argument (status: the endpoint's connection, any enabled state), never another tenant's; decryption happens in Go with `ECPAY_LOGISTICS_KEYRING` (AAD binds tenant/store/connection/environment/merchant_id/version). Purpose-scoped loaders instead of one generic `(connection, purpose)` loader so an unauthenticated route can only reach the connection its own URL names. |
| `fulfillment.read_cvs_shipment(hash, store, order)` (merchant) / `fulfillment.read_buyer_cvs_shipment(buyer_hash bytea, store uuid, order uuid) RETURNS jsonb` STABLE (buyer, scope from `buyer.resolve_scope`, called by the buyer order GET `checkout.Get`, handler.go:629) | checkout_writer (both) | `commerce_runtime` (merchant) / `commerce_checkout_runtime` (buyer; no table grant to that role) | Merchant: all attempts, codes, events (no PII). Buyer: current attempt `{state, chain, store_name, store_code, updated_at}` only; never trade no, codes or provider IDs. |

Grants/policies reuse the 0063/0062 matrix pattern: `commerce_checkout_writer` already has
identity/ops USAGE + session/audit/command grants (stripe-refund-v1 §4.5); add SELECT/INSERT/
column-UPDATE on the new fulfillment tables and SELECT on profiles; `commerce_integration_writer`
SELECT on profiles/shipments + credentials (existing); commerce_integration_writer has no INSERT/UPDATE on any fulfillment.* table (TCV02 negative); `commerce_auth` column SELECT for reads;
`commerce_worker` EXECUTE only on `load_cvs_create` and `finish_cvs_create`; no direct table grant to
runtime, worker or buyer roles.

Added in round 1 (verified: 0013:293-303 gives `commerce_checkout_writer` only the lock policy
`checkout_writer_lock … WITH CHECK(false)` and UPDATE(current_version) on `pickup_heads`, and no INSERT
on `pickup_versions`/`pickup_heads`, so `verify_cvs_selection` would fail at runtime):
- `commerce_checkout_writer`: INSERT on `fulfillment.pickup_versions` and `fulfillment.pickup_heads`,
  UPDATE(current_version,pickup_id) on `pickup_heads`, each through new policies scoped by
  `app.tenant_id`/`app.store_id` AND `namespace ~ '^ecpay\.'` AND (versions) `verification_kind =
  'PROVIDER_DIRECTORY_VERIFIED'`; the existing `checkout_writer_lock` policy stays for non-ecpay heads.
- `commerce_integration_writer`: column SELECT on `checkout.orders(tenant_id,store_id,owner_id,id,
  currency,snapshot)` through a policy `EXISTS(cvs_shipments c … AND c.state IN ('REQUESTED','UNKNOWN'))`
  (the recipient is read from the frozen order snapshot, never from `storefront.destination_snapshots`);
  EXECUTE on `fulfillment.cvs_order_payable`.
- `commerce_runtime`: EXECUTE on the three API-side key loaders; the RESTRICTIVE `ecpay.*` namespace
  policies of §4.1.

Round 2's grant bullets are replaced in round 3 (finding 1: migrate.go:180-184 revokes `river.river_job`
and schema `river` from `commerce_checkout_writer` at the end of every Apply; a principal-scoped operation
policy hid member A's attempt from member B; `FOR SHARE` on `bindings` needs a column UPDATE grant plus
a lock policy, 0008:108/118):
- `commerce_checkout_writer`: EXECUTE on `integration.plan_cvs_create`; one SELECT policy
  `checkout_cvs_operation_read` on `integration.operations` (§4.3 text, no principal clause) so
  `settle_cvs_attempt` and abandon see any member's attempt. No river privilege, no INSERT on
  operations/events, no bindings policy.
- `commerce_integration_writer`: policy `cvs_create_insert` FOR INSERT on `integration.operations` (§4.3
  text); existing `worker_event_insert` / `worker_binding_lock` / `GRANT UPDATE(id)` (0008:106-117) cover
  the READY event and the binding lock; SELECT on `river.river_job` already exists (migrate.go:154,
  post_river 0014:19); EXECUTE on `fulfillment.apply_cvs_create_result`. No INSERT/UPDATE on any
  `fulfillment.*` table.
- No `integration:execute` and no `core.Service.Plan` call on this path.
TCV02 asserts the exact matrix (every grant/policy above) with one negative per role, including a
`commerce_runtime` INSERT of an `ecpay.*` or `buyer.*` pickup version or head (refused); after Apply,
has_table_privilege('commerce_checkout_writer','river.river_job','SELECT') is false; and
`commerce_integration_writer` has no INSERT/UPDATE on any `fulfillment.*` table.

§16 (round 4, R4-2; verified: 0027:57-59 gives `commerce_auth` only a column list on `checkout.orders`, and
`fulfillment.manual_shipment_eligible` is INVOKER inside `identity.export_unshipped_orders` (0063:491) and
`identity.read_merchant_orders` (0063:543); 0013:175 limits `commerce_checkout_writer` UPDATE to
`commercial_state,fulfillment_state,generation,expires_at,updated_at`; options run on the checkout pool,
`internal/checkout/options.go:108` via `checkout.New(checkoutPool)` (cmd/api/buyer.go:91,114), whose login
holds exactly one authority, `commerce_checkout_runtime` (internal/platform/platform.go:303-316)):
- `GRANT SELECT(payment_mode,collection_state) ON checkout.orders TO commerce_auth`;
  `GRANT UPDATE(collection_state) ON checkout.orders TO commerce_checkout_writer`.
- `fulfillment.cvs_store_settings`: `GRANT SELECT,INSERT,UPDATE(enabled_chains,pay_at_pickup_enabled,
  pay_at_pickup_max_twd,pay_at_pickup_max_open,version,updated_at) TO commerce_checkout_writer` with policies
  `cvs_settings_rw` FOR SELECT/INSERT/UPDATE TO commerce_checkout_writer USING/WITH CHECK(tenant_id=app.tenant_id
  AND store_id=app.store_id) (the UPDATE policy also covers `begin_hold` FOR SHARE/FOR UPDATE); no grant to
  `commerce_runtime`, `commerce_checkout_runtime` or buyer roles.
- Options read through `fulfillment.read_cvs_offer(p_hash bytea, p_store uuid) RETURNS TABLE(pickup_selection
  text, enabled_chains text[], pay_at_pickup_enabled boolean, pay_at_pickup_max_twd integer, ok_verified
  boolean, hilife_verified boolean)` STABLE SECURITY DEFINER, `search_path=pg_catalog`, owner
  `commerce_checkout_writer`, EXECUTE **`commerce_checkout_runtime`** only (the reviewer's text said
  `commerce_runtime`; the options pool is not a `commerce_runtime` member, so that grant would 42501 —
  stronger fix), scope from `buyer.resolve_scope(p_hash,p_store)` (EXECUTE already granted to
  checkout_writer, 0013:11; never caller GUCs).
TCV02 adds these rows to the exact matrix.

Pool per route (X9, verified 2026-09-30): each login holds exactly one authority (platform.go:303-316).
Merchant/admin and the two unauthenticated ECPay routes run on the main pool (`platform.OpenPool` =
`commerce_runtime`, cmd/api/main.go:65). Buyer routes use two pools (cmd/api/buyer.go:86,91):
cart/quotes/destination/catalog on the buyer pool (`commerce_buyer_runtime`, `scoped(ctx,h.pool,…)`,
handler.go:548-612), options/Begin/orders on the checkout pool (`commerce_checkout_runtime`, `h.checkout`,
handler.go:528,538,622,629). The new buyer CVS routes (§5.2 `cvs-selections*`, §16.1 `cvs-stores`) are served
by `checkout.Service` on the checkout pool, so `open_cvs_selection`, `read_cvs_selection`,
`record_buyer_cvs_store`, `read_cvs_offer`, `read_buyer_cvs_shipment`, and (buyer verify) `verify_cvs_selection` +
`load_ecpay_key_for_selection` are EXECUTE `commerce_checkout_runtime`; none is granted to
`commerce_buyer_runtime`, and the buyer-only ones are not granted to `commerce_runtime`. `begin_hold` keeps its
0013:611 grant (`commerce_checkout_runtime`). TCV02 asserts this EXECUTE matrix per function, and runs each buyer
definer from a real `platform.OpenCheckoutPool` login (not superuser).

## 5. Buyer surfaces

### 5.1 Options (amends buyer-checkout-options-v1 "MANUAL, no binding")

`internal/checkout/options.go` accepts the four CVS kinds and, in addition to MANUAL rows, API rows
that satisfy the §4.1 predicate. With an enabled qualified profile the row carries `pickup_selection:"ecpay_map"`; without one it carries `pickup_selection:"buyer_entered"` (C1b). A CVS row is listed only when its kind is in the store's `enabled_chains` (§16.5, C4). Profile and settings are read only through `fulfillment.read_cvs_offer` (§4.3; round 4), never by direct table access.
In `ecpay_map` mode `cvs_okmart` is listed only when `ok_verified` and `cvs_hilife` only when `hilife_verified` (TD6); an
unverified chain the store configured is returned as `{available:false, reason:"coming_soon"}` and the
storefront shows it disabled with "即將開放" (§15, Q4). In `buyer_entered` mode these ECPay gates do not
apply (no ECPay create; the merchant ships through its own channel, §16.1). Each CVS row also carries
`payment_modes` (`["card"]`, plus `"pay_at_pickup"` when §16.5 enables it) and, for `buyer_entered`, the
chain's official store-search URL (§16.1).

### 5.2 HTTP (private Go under `/v1/buyer/` — the store is resolved from the BFF-authenticated `X-Commerce-Storefront-Origin`, handler.go:372-380, as for every buyer route; round 2 corrected the `/stores/{store_id}` prefix; the public BFF mirrors under `/api/buyer/`)

| Method/path | Input | Success / errors |
| --- | --- | --- |
| POST `/cvs-selections` | `Idempotency-Key`; body exactly `{cart_version, market_id, service_code, return_path}` (`return_path` = the product page the buyer checks out on, allowlisted `^/(zh-TW|zh-CN|en)/products/[A-Za-z0-9_-]{1,64}$` — checkout (`OrderFlow`) exists only under `products/[productID]` (ProductPurchase.tsx); `/claim` is excluded because its link token lives only in the URL fragment and is dropped on load and on `pagehide` (ClaimLink.tsx:120-146), so a return there always renders not-found; anything else → 422 `bad_return_path`. A later claim-page checkout adds its path by amendment). `return_origin` is **not** a body field: Go takes the authenticated `X-Commerce-Storefront-Origin` (§4.3 open_cvs_selection) → 422 `bad_return_origin` | 201 `{selection_id, expires_at, form:{action, fields:{MerchantID, MerchantTradeNo, LogisticsType:"CVS", LogisticsSubType, IsCollection:"N", ServerReplyURL, Device}}}`. `MerchantTradeNo` = 20 chars crypto/rand `[A-Za-z0-9]` generated in Go, only its sha256 stored; `ServerReplyURL = <COMMERCE_CVS_HOOKS_ORIGIN>/v1/cvs/ecpay/map-return/{selection_id}`; `action` is the fixed stage/prod map URL by environment (never from input). 409 cart/version, 422 `service_unavailable`, 429. Replay returns 409 `selection_replay_new_key` (the nonce is not re-derivable; the client opens a new selection). |
| POST `/cvs-selections/{id}/verify` | none | 200 read projection. If RETURNED, Go retries the directory lookup (§7.2) then `verify_cvs_selection`; directory fetch failure → 200 `{state:"RETURNED", retry_after_s}`. |
| GET `/cvs-selections/{id}` | none | 200 read projection. |

Public provider return (no cookie, no auth; not under `/api/buyer`):
`POST /v1/cvs/ecpay/map-return/{selection_id}` — form-urlencoded ≤ 8 KiB, fields must be a subset
of F3 names (unknown fields ignored, duplicates → reject), `MerchantTradeNo` hashed and passed to
`record_cvs_map_return`, then (if RETURNED) the directory verification runs inline with a 5 s budget.
For a known selection it always answers `303 See Other` to `<return_origin><return_path>?cvs_selection={id}`
with the selection's **stored** `return_origin` and `return_path` (round 2: stores are served on per-store
hosts resolved against `control.storefront_domains`, and the buyer cookie is `__Host-commerce_buyer`
bound to that exact host, buyer-server.ts:20 — one global origin would land on the wrong host with no
cookie and no store; neither value is ever taken from the map-return body). The cookie is sent on this
cross-site top-level GET because it is `SameSite=Lax` (buyer-server.ts:781); TCV08 fails if that changes.
Unknown selection → `404 text/plain`, no redirect. No body echo, no store fields in the Location (I11). Origin/Referer are not used for authentication. The map-return
record step follows the §4.3 check order (nonce first; a wrong nonce writes nothing).

Storefront UI (ui_worker): delivery list shows the four chains with logos-free text labels (三語);
"選擇門市" builds a hidden form and submits it in the **same tab** (no iframe, no `target`), after
saving the checkout draft (cart, recipient inputs) in the existing session state. On return it
calls `verify`, shows a read-only store card (name, address, code, 離島 badge when `outside`) with
"更換門市" and the Q9 consent line; recipient name/mobile inputs show the ECPay rules inline (F5) and
the server's 422 codes map to field errors. Messenger/Instagram in-app browsers: if the return lands
without the buyer cookie, the page shows "在瀏覽器中繼續" with the existing recovery path; the selection
stays VERIFIED and can be confirmed after recovery within its 15 min.

### 5.3 Buyer order page

`GET /v1/buyer/orders/{id}` adds `cvs_shipment: null | {state, chain, store_name, store_code, updated_at}`
for the current attempt. Copy: CREATED "賣家已建立超商寄件", AT_DC "已到物流中心", AT_STORE "已到店，請取貨",
PICKED_UP "已取貨", UNCLAIMED "逾期未取，退回中"; REQUESTED/UNKNOWN/FAILED/ABANDONED show "處理中" (never an
error detail). Never "delivered" before PICKED_UP. ECPay sends the arrival SMS (F7 flow), not us.

## 6. State machine (`fulfillment.cvs_shipments.state`, order column in brackets)

| From | Event | To [order] | Guard |
| --- | --- | --- | --- |
| — / FAILED / ABANDONED | merchant request | REQUESTED [MANUAL_UNASSIGNED] | §4.3 request guards, attempt ≤5 |
| REQUESTED | create `1|…` MAC ok + AllPayLogisticsID | CREATED [PROVIDER_LABEL_CREATED] | lease fence |
| REQUESTED | `0|msg` | FAILED [MANUAL_UNASSIGNED] | result_code bounded (§7.4) |
| REQUESTED | timeout / 5xx / 403 / MAC bad / malformed | UNKNOWN [MANUAL_UNASSIGNED] | never redispatch |
| REQUESTED | BLOCKED_POLICY / CANCELLED (Finish) or STALE_BINDING (settle) | FAILED `ecpay.not_sent.<code>` [MANUAL_UNASSIGNED] | zero requests sent; new attempt allowed |
| REQUESTED | status notification | unchanged; 503 without `1|OK` (ECPay retries) | create's Finish decides first |
| UNKNOWN | query finds trade (MAC ok) | CREATED (+codes) [PROVIDER_LABEL_CREATED] | same MerchantTradeNo |
| UNKNOWN | budget exhausted | UNKNOWN (manual) | merchant may ABANDON with acknowledgement |
| REQUESTED/UNKNOWN | operation DISPATCHING/UNKNOWN with `lease_until < now()-1 h` (settle) | UNKNOWN (manual) | round 2; a later Finish on an ABANDONED attempt = event + `duplicate_label_risk` |
| REQUESTED/UNKNOWN | create/query success whose payment/validation/shipment no is outside its column CHECK | CREATED [PROVIDER_LABEL_CREATED], that code NULL + local event `ecpay.code_nonconforming` | Finish never raises on provider data (R-7a); `print_available=false` |
| CREATED | status 2030 / 3024 | AT_DC | subtype column matches |
| CREATED, AT_DC | status 2063 (UNIMART) / 2073 (UNIMARTC2C) / 3018 (FAMI*, HILIFE*) | AT_STORE | |
| AT_STORE | 2067 (7-11) / 3022 (others) | PICKED_UP | terminal for v1 |
| AT_STORE | 2074 (7-11) / 3020 (others) | UNCLAIMED | returns/RMA out of scope |
| AT_STORE/UNCLAIMED | 2098 (re-delivered to pickup store) | AT_STORE | |
| CREATED | lapsed (F9) + merchant abandon | ABANDONED [MANUAL_UNASSIGNED] | no status beyond creation AND fresh (≤10 min) MAC-verified query in the created-only set (§4.3) |
| UNKNOWN | budget exhausted / binding changed / stale lease + merchant abandon | ABANDONED [MANUAL_UNASSIGNED] | `i_checked_ecpay_backend=true`; a query that finds the trade applies CREATED instead (409 `ecpay_trade_found`) |
| FAILED / ABANDONED | any status or query result | unchanged (event only) + merchant alert `duplicate_label_risk` | always ACK `1|OK`; never touches the one-live index |
| any | other/unknown code, OK mart code, backwards jump | unchanged (event only) | |
| AT_STORE (pay_at_pickup order) | 2067 / 3022 | PICKED_UP; order `collection_state` PENDING→COLLECTED (audited) | §16.4; conflicting merchant record → event + `collection_conflict` |
| AT_STORE (pay_at_pickup order) | 2074 / 3020 | UNCLAIMED; order `collection_state` PENDING→RETURNED (audited) | §16.4; no ledger/payment/refund row |

`record_manual_shipment` is blocked for every live attempt: `PROVIDER_LABEL_CREATED` fails its MD6
`MANUAL_UNASSIGNED` clause, and REQUESTED/UNKNOWN (order still `MANUAL_UNASSIGNED`) fail the new
no-live-cvs-shipment clause of the replaced `manual_shipment_eligible` (§4.3), so an order cannot be both
manually shipped and ECPay-shipped (I13). Refunds never change these states
(stripe-refund RD6); a refunded order cannot get a *new* request (MD6 refund clause), a REQUESTED one is
not sent (dispatch-time `cvs_order_payable`, §4.3), an existing CREATED shipment is shown to the merchant with a "refunded — do not drop the parcel" warning.
OK mart codes: none documented (F7) → OKMARTC2C shipments stay CREATED until TCV10 supplies a
mapping; the merchant sees raw events.

## 7. Wire adapter `internal/integrations/shipping/ecpay` (stdlib only; no SQL, no River)

### 7.1 Package rules
- Endpoints are constants per environment (F1, F5, F8, F10) with docs URL + 2026-09-29 comment.
  `http.Client` with TLS ≥1.2, 10 s timeout, no redirects followed, response body cap 256 KiB **except
  GetStoreList: 32 MiB, 30 s, streamed `encoding/json` decode straight into `map[StoreId]Store`** (F18: the
  whole chain list, no filter or paging).
- `CheckMac(params url.Values, key, iv string) string` implements F6 exactly (sort by key,
  case-sensitive as documented, .NET encode table incl. `-_.!*()` restored, lowercase, MD5, upper).
  Verify uses `subtle.ConstantTimeCompare` on the uppercase hex. MD5 is the vendor's MAC; never reuse
  it for our storage.
- Fields are validated against F5 lengths/charset before sending; `GoodsName` is a fixed per-store
  ASCII-safe string (default `"商品"`; forbidden symbols F5), never product titles (PII-free, symbol-safe).
- No logging of request/response bodies, keys, recipient data, trade numbers or codes.

### 7.2 Directory lookup
`StoreDirectory(ctx, env, cvsType, creds) (map[string]Store, error)` calls GetStoreList with the calling
connection's keys and caches in-process **keyed by (environment, CvsType)** until the next 20:00
Asia/Taipei (F10) — the list is the chain's store list, not merchant data, so at most 2×5 entries exist
and more stores never thrash it (round 1, finding 6). At most one fetch per key per 10 minutes;
concurrent misses coalesce (single-flight, stdlib `sync`); a failed fetch is remembered for the rest of
that 10 minutes and `verify` answers `{state:"RETURNED", retry_after_s}` without refetching (F11 403 ⇒
30 min ban). The inline map-return budget uses the cache only; a miss defers to the buyer's `/verify`.
Assumption checked by TCV07: stage merchants `2000132` and `2000933` return identical UNIMART/FAMI lists;
if they differ the key becomes (connection, CvsType) with an LRU of 2 × enabled profiles × 4.
`// ponytail: per-process cache; move to a PG table if multi-instance cold fetch latency matters.`
Lookup key = exact `StoreId` string; the map's 9-char `CVSStoreID` must equal a directory `StoreId`
exactly (no trimming/padding). CvsType mapping: UNIMART* → `UNIMART`, FAMI* → `FAMI`, HILIFE* →
`HILIFE`; OKMARTC2C has no CvsType (F10) → `ErrNoDirectory` unless TCV10 proves otherwise.

### 7.3 Connect probe
Before `register_ecpay_logistics`, Go calls GetStoreList with the submitted plaintext
(`CvsType=UNIMART`, 10 s). `RtnCode=1` ⇒ probe_ok. Any other outcome ⇒ 422
`ecpay_probe_failed` (no row written). Read-only; allowed for LIVE credentials.

### 7.4 Dispatcher route (`ecpay_logistics` / `ecpay.cvs_create` / `transactional`)
- **Check** (no tx, no Secret — `DispatchRoute` contract): request shape `{order_id, attempt}` only;
  `CVS_ECPAY_ENABLED=1`. Everything that needs SQL moves to LoadSecret.
- **LoadSecret** (dispatch mode, and reconcile mode via R-7b): `load_cvs_create` (lease-fenced) — in
  dispatch mode it refuses unless the shipment is still REQUESTED, the profile is enabled with the frozen
  credential version and `cvs_order_payable` holds; Go then decrypts and refuses LIVE unless
  `CVS_ECPAY_LIVE_CREATE=1` and refuses a recipient failing `ecpay_recipient_ok`. Every refusal returns
  `ErrPolicyDenied` ⇒ BLOCKED_POLICY `credential_unavailable`, zero requests; `Finish` maps it to FAILED
  `ecpay.not_sent.credential_unavailable` and the merchant's next request shows the precise reason
  (`not_shippable`, `connection_unavailable`, …) from `request_cvs_shipment`.
- **Finish** (R-7a): `finish_cvs_create` with the codes from `Outcome.Detail`, in the completion tx
  before Complete.
- **Dispatch:** `Express/Create` with `MerchantTradeNo` (frozen), `MerchantTradeDate` (frozen at
  request time, Asia/Taipei, `yyyy/MM/dd HH:mm:ss`), `LogisticsType=CVS`, subtype, `GoodsAmount`,
  `IsCollection=Y` and `CollectionAmount` = frozen order total in integer TWD 1..20000 iff the order payment_mode is pay_at_pickup, else N (§16.3, F20), `GoodsName`, sender fields from the credential payload, receiver name/phone,
  `ReceiverStoreID`, `ServerReplyURL=<COMMERCE_CVS_HOOKS_ORIGIN>/v1/cvs/ecpay/status/{endpoint_id}`,
  `PlatformID=""`, CheckMacValue. Classify: body `1|…` with valid MAC and `AllPayLogisticsID` ⇒
  SUCCEEDED (reference = AllPayLogisticsID); body `0|…` ⇒ FAILED_FINAL with `result_code`
  `ecpay.rejected` (message text is not stored; the merchant is told to check the ECPay backend —
  common causes: balance too low, subtype not enabled, store closed); everything else ⇒ UNKNOWN.
- **Reconcile** (`ReconcileWithSecret`, R-7b — the key is needed to sign the query):
  `QueryLogisticsTradeInfo/V5` by frozen `MerchantTradeNo` (`TimeStamp` now, 3 min window); MAC-verified response with `AllPayLogisticsID` ⇒ SUCCEEDED with codes (B2C: also
  `ShipmentNo`); otherwise stay UNKNOWN. 403 ⇒ UNKNOWN (30 min back-off is longer than the
  dispatcher's max retry delay; budget exhaustion → manual, documented limit).
- Duplicate-create behaviour for a reused MerchantTradeNo is **UNKNOWN** (ECPay says it must be
  unique, F5, but does not document the error); v1 never sends a second create for one operation.

### 7.5 Status ingress `POST /v1/cvs/ecpay/status/{endpoint_id}`
Form ≤ 16 KiB, duplicate keys → 400. The MAC is computed over **every received field** except
CheckMacValue; only the F7 fields are persisted (unknown fields are MAC-covered, then dropped). Load endpoint
material (profile enabled or disabled — historical shipments keep reporting; missing endpoint → 404),
load the key with `load_ecpay_key_for_status`, decrypt with the logistics keyring, verify
CheckMacValue; `MerchantID` must equal the account. Failure ⇒ `400 text/plain "0|verify"` (ECPay
retries; nothing stored). Success ⇒ `ingest_ecpay_status` in one tx; after COMMIT reply exactly
`200 text/plain` body `1|OK`; result `retry` (attempt still REQUESTED) ⇒ `503` with no `1|OK`. Body sha256 is computed over the raw bytes; recipient fields are
discarded after MAC verification. Per-endpoint concurrency cap 4 and body read timeout 5 s
(the Stripe P2 "ingress can be exhausted" finding applies).

## 8. Merchant admin surfaces (admin BFF mirrors under `/api/admin/`)

| Method/path | Permission | Contract |
| --- | --- | --- |
| GET `/v1/admin/stores/{store_id}/logistics/ecpay` | integration:read | `{environment, mode, merchant_id, version, enabled, qualified_at, ok_verified, status_url}` or 404; never keys/sender data. |
| PUT same | integration:manage | `Idempotency-Key`; body exactly `{expected_version, environment, mode, merchant_id, hash_key, hash_iv, sender_name, sender_cell_phone}`; §7.3 probe; 200 GET projection. 422 `ecpay_probe_failed`, `invalid_sender`, `ecpay_environment_not_allowed` (environment ≠ deployment payment environment, round 2; checked before the probe). Keys are write-only in the UI (never re-displayed). |
| POST `…/logistics/ecpay/enabled` | integration:manage | `{expected_version, enabled}`. |
| POST `/v1/admin/stores/{store_id}/orders/{order_id}/cvs-shipment` | fulfillment:write | `Idempotency-Key`; `{expected_version}`; 202 `{attempt, state:"REQUESTED", operation_id}`. 422 `not_shippable`, `no_cvs_destination`, `connection_unavailable`, `cvs_recipient_rejected`. |
| GET `…/cvs-shipment` | orders:read | all attempts + events (no PII); for CREATED: `code` (7-11: payment_no+validation_no; others: payment_no), `print_available`. |
| POST `…/cvs-shipment/print-form` | fulfillment:write | 200 `{action, fields}` signed form for F8 print URL of the subtype (`PrintMode=2` A6 when the merchant picks thermal). No state change; audit `fulfillment.cvs_label_printed`. OK mart: 422 `print_unsupported` (use code at store). |
| POST `…/cvs-shipment/abandon` | fulfillment:write | `{expected_version, i_checked_ecpay_backend}`; Go first runs a MAC-verified Query/V5 with the merchant key loader and passes `query_status`/`query_at` to the definer (§4.3). 409 `ecpay_shows_movement`. |
| GET / PUT `/v1/admin/stores/{store_id}/logistics/cvs-settings` | integration:read / integration:manage | §16.5 store settings; PUT `Idempotency-Key`, body exactly `{expected_version, enabled_chains, pay_at_pickup_enabled, pay_at_pickup_max_twd, pay_at_pickup_max_open}`; 409 version; 422 `invalid_settings`. |
| POST `/v1/admin/stores/{store_id}/orders/{order_id}/collection` | fulfillment:write | §16.4 manual `collected` / `returned` / `refunded_offline`; `Idempotency-Key`; body exactly `{expected_state, state}`; 422 `not_pay_at_pickup`, `not_shipped`; 409 `collection_state_changed`. |
| POST `/v1/admin/stores/{store_id}/orders/{order_id}/pay-at-pickup-release` | fulfillment:write | §16.8 `cancel` (PENDING, unshipped) / `restock` (RETURNED); `Idempotency-Key`; body exactly `{action, expected_state}`; 200 `{order_id, collection_state, commercial_state, released_lines}`; 422 `not_pay_at_pickup`, `not_cancellable`; 409 `collection_state_changed`, `cvs_attempt_in_flight`. |

Admin UI (ui_worker, inside the existing inline order detail row, next to the 0063 manual section):
Settings → 物流 → "綠界物流" card (connect/rotate form, mode, environment badge, enable toggle,
status URL shown read-only); delivery-service editor allows Mode "API（綠界）" for CVS kinds when
the card is qualified. Order detail: "建立超商寄件" (confirm dialog: "綠界將從你的綠界帳戶餘額扣運費；
請在 N 天內交寄" with N from F9), code with copy button, "列印託運單" (opens a new top-level tab that
auto-submits the returned form — merchant side only; buyer map never uses a new window), status
timeline, UNKNOWN/FAILED banners with "改用手動出貨" (abandon → 0063 form). Filter `cvs_pending`
(PROVIDER_LABEL_CREATED ∧ state CREATED) for the forwarder's daily drop list.

## 9. Idempotency and uniqueness

- Selections: HTTP `Idempotency-Key` via `ops.command_results` (`fulfillment.cvs_selection.open`);
  nonce unique by digest; map return idempotent per selection state.
- Shipment request: `ops.command_results` (`fulfillment.cvs_shipment.request`, I02); one live
  shipment per order (partial unique index); operation semantic key `cvs:<order_id>:<attempt>` set by the
  definer (not the HTTP key; `core.Service.Plan` is not used). A replayed key RAISEs `PT2RP`, Go rolls back
  its River job and answers from `read_cvs_shipment_command` ⇒ zero extra `river_job` rows (round 2).
- Provider idempotency: `MerchantTradeNo` derived from the operation UUID, frozen, unique per
  connection (DB UNIQUE); UNKNOWN never redispatched (I06); new attempt = new operation + new trade no,
  only after FAILED or merchant ABANDON.
- Status: dedupe by `(shipment, body_sha256)`; ECPay retries re-deliver identical bodies.
- Connection: one per (store, environment) for `ecpay_logistics`; rotation appends a credential
  version; historical shipments use their frozen version (query/status survive rotation only while
  the old keys still verify — ECPay has one active key pair, so after rotation old-version status MAC
  fails → documented limit: rotate only when no shipment is in CREATED..UNCLAIMED, UI warns).

## 10. Test gates (tiers: UNIT, REAL_PG, MOCK, HTTP_PG, SANDBOX, BROWSER, LIVE)

| Gate | Test | Tier | Required |
| --- | --- | --- | --- |
| TCV01 | `TestEcpayMac` | UNIT | F6 official vector (published test keys written split, PROCESS §6) reproduces `692FD6E2…239AD`; .NET encode table incl. `!*()-_.` and space→`+`; constant-time verify; tampered field/key fails; recipient rule table (CJK width, emoji, digits, `+8869…`→`09…`, landline rejected); trade-no derivation `^LC[A-Z2-7]{18}$`. |
| TCV02 | `TestTaiwanCvsSchema` | REAL_PG | Fresh + populated upgrade over 0063–0066; widened CHECKs keep every prior value (re-derived); merchant cannot insert `PROVIDER_DIRECTORY_VERIFIED`; API service enable refused without qualified same-store profile (trigger), allowed with; `cvs_okmart` refused unless `ok_verified`; deferred order⇔shipment trigger from both sides; one-live-shipment index; immutability triggers; exact grant matrix + `pg_policies` + definer owner/`proconfig`/ACL; no PII columns (catalog scan); merchant-plant negative: `commerce_runtime` INSERT of an `ecpay.*` pickup version/head and UPDATE of an `ecpay.*` head refused; `ecpay_one_enabled_profile`, `ecpay_logistics_identity_unique` and `cvs_create_family` enforced; `receiver_store_id` accepts 4-char `1328`, refuses 7 chars; **round 4:** a merchant export (`identity.export_unshipped_orders`) and `identity.read_merchant_orders` under the real `commerce_auth` role after 0072 succeed for a card order and for a pay_at_pickup order; `commerce_runtime` and `commerce_checkout_runtime` SELECT on `fulfillment.cvs_store_settings` and `integration.ecpay_logistics_profiles` is refused; `fulfillment.read_cvs_offer` runs on the checkout pool; **X9:** after a full Apply `has_table_privilege('commerce_checkout_writer','river.river_job','SELECT')` is false; `commerce_integration_writer` has no INSERT/UPDATE on any `fulfillment.*` table; exactly one `checkout.begin_hold`; the §4.3 pool-note EXECUTE matrix (buyer definers `commerce_checkout_runtime` only, run from a real checkout-pool login); `buyer_entered_own` hides buyer A's `buyer.*` rows from buyer B under both buyer roles; the BUYER ALLOCATE branch refuses NULL `checkout_id`/buyer columns; §16.8 DEALLOCATE CHECK/branch/guard/unique index present; `guard_checkout_ledger` DEALLOCATE branch present. |
| TCV03 | `TestCvsSelectionFlow` | REAL_PG + HTTP_PG | open → return → verify (fake directory) → pickup source → SetDestination → Begin; replayed/late return after a newer selection cannot change the destination (head CAS); wrong nonce on OPEN = ignored; expired, merchant/subtype mismatch, 9-char or 5-char store id, store not in directory → REJECTED with zero pickup rows; other owner/session cannot read/verify; forged name/address in the POST never reach the pickup row; leading zeros preserved; pickup reuse vs new version on name change; `record_cvs_map_return` reachable without cookies; wrong nonce on OPEN with a mismatching merchant/subtype/store id → `ignored`, row unchanged (check order); 4-char store id accepted, 7-char → `store_code_length`; merchant-planted `MANUAL_ATTESTED` row under the ecpay namespace impossible, and an expiring (<1 h) verified head is not reused; 303 goes to the stored `return_origin` + `return_path`; `bad_return_path` (incl. `/zh-TW/claim`) → 422; **two stores on two ACTIVE domains**: each selection's 303 lands on its own store's origin, an origin not ACTIVE for the store → `bad_return_origin`, unknown selection → 404 without `Location`; open with a profile whose environment ≠ `p_payment_environment` → `service_unavailable`; SANDBOX and LIVE selections produce different namespaces. |
| TCV04 | `TestCvsBeginGuards` | REAL_PG | Begin with API service + verified pickup succeeds; amount boundaries in TWD minor units: 2000000 accepted, 2000100 and 50 (non-whole TWD) refused; pickup namespace environment ≠ profile environment → `cvs_environment_mismatch`; **LIVE payment-profile deployment + SANDBOX ECPay profile** (planted by migration owner, since register refuses it with `ecpay_environment_not_allowed`) ⇒ `cvs_environment_mismatch`, zero holds, zero payment attempts; exactly one `checkout.begin_hold` signature after a fresh install and after an upgrade (post_river ordering); recipient rule / non-TWD / disabled profile / rotated-unqualified credential / stale pickup (24 h) → PT422/PT409 with zero holds; existing MANUAL home + MANUAL_ATTESTED CVS regressions unchanged. |
| TCV05 | `TestCvsShipmentLifecycle` | REAL_PG + MOCK (fake ECPay HTTP server) | request → job + operation + shipment in one tx (job xmin/args verified; a job from another tx or with other args → 22023); replayed Idempotency-Key ⇒ same body and **zero extra `river_job` rows**; a member with only `fulfillment:write` (no `integration:execute`) can request; operation `semantic_key = cvs:<order>:<attempt>`; fake Create returning a 41-char `CVSPaymentNo` ⇒ CREATED with NULL payment_no + `ecpay.code_nonconforming` event, operation SUCCEEDED (and a 16-char one is stored); an `AllPayLogisticsID` outside the CHECK ⇒ UNKNOWN, never a Finish error; operation left DISPATCHING with `lease_until` > 1 h old ⇒ settle → shipment UNKNOWN, abandon with acknowledgement, later Finish ⇒ event + `duplicate_label_risk` only; UNKNOWN abandon whose query finds the trade ⇒ CREATED, 409 `ecpay_trade_found`; SUCCEEDED/FAILED/UNKNOWN classification incl. bad MAC, `0|`, 403, timeout, malformed; UNKNOWN → query-only reconcile (never a second Create, counted on the fake); real child-process kill after the fake records the create, restart → one create, query recovers CREATED; LIVE without flag → BLOCKED_POLICY, zero requests, shipment FAILED `ecpay.not_sent.*` and a new attempt allowed; refund committed after the request but before dispatch → zero creates on the fake; binding changed before dispatch (claim-side STALE_BINDING) → settled FAILED on the next request; budget exhausted → UNKNOWN; manual record while REQUESTED or UNKNOWN → PT422 `not_shippable`; SANDBOX logistics profile with a LIVE captured payment → `cvs_environment_mismatch`; reconcile signs Query/V5 with the loaded key (R-7b); refunded/unpaid/manual-shipped/other-store orders refused; two concurrent requests → one shipment; abandon rules (lapse window by subtype, fresh created-only query required, stale/moved query → PT409, UNKNOWN acknowledgement); PII absent from `integration.operations`, job args, events, logs (sentinel scan); **round 3:** member B settles and abandons member A's attempt; a REAL_PG run of Finish (`finish_cvs_create` → `apply_cvs_create_result`) under the real roles (`commerce_worker` login, not superuser) for SUCCEEDED/FAILED_FINAL/UNKNOWN/BLOCKED_POLICY; request under the real `commerce_runtime`/`commerce_checkout_writer` roles after a full Apply (post_river revokes in force). |
| TCV06 | `TestCvsStatusIngress` | MOCK + HTTP_PG | Signed fixtures for every F7 code per subtype → exact transitions; duplicate body → one event, `1|OK`; bad MAC / wrong MerchantID / unknown endpoint → no rows, no `1|OK`; unknown code and backwards jump → event only; reply sent only after COMMIT (fault injection); status for a REQUESTED attempt → 503, no row; status for an ABANDONED attempt while attempt 2 is live → one event, no state change, `duplicate_label_risk` alert, `1|OK`; non-conforming `UpdateStatusDate`/`RtnCode` and a 500-char `RtnMsg` with control chars → stored NULL/truncated, `1|OK`; an extra unknown field is MAC-covered (tampering it fails); body > 16 KiB, duplicate keys, slow body → rejected; recipient fields never persisted. |
| TCV07 | `ecpay_sandbox_test.go` (`-tags sandbox`, env `ECPAY_LOGISTICS_SANDBOX=1`) | SANDBOX | Stage C2C `2000933` + B2C `2000132` (keys from env): GetStoreList returns the documented stage stores (both stage merchants' lists compared — §7.2 cache-key assumption); Express/map form accepted (stage fixed store) for UNIMARTC2C/FAMIC2C/HILIFEC2C; Create with stage stores 131386/006598 (+ Hi-Life if documented) returns MAC-valid `1|…`; Query/V5 by MerchantTradeNo returns the same AllPayLogisticsID; print form returns HTML; the Query `LogisticsStatus` right after create is recorded (F19 created-only set); a Hi-Life create with a directory `StoreId` accepted ⇒ evidence for `hilife_verified`; evidence under `output/taiwan-cvs/`. Status notification is **NOT_RUN in SANDBOX** (F7: stage has no simulation). |
| TCV08 | `taiwan-cvs.spec.ts` | BROWSER (SANDBOX) | Buyer: pick each enabled chain, same-tab map round-trip against the stage map (fixed store), store card, change store, recipient errors, pay with Stripe 4242, order page states (seeded via signed MOCK status posts); merchant: connect card (probe via fake in CI, stage in SANDBOX run), create, code copy, print opens new tab, abandon; desktop + mobile Chromium + WebKit (iOS same-tab rule); 3 locales; no iframe in DOM; **two stores on two hosts**: the map return lands on the originating store's host with the `__Host-commerce_buyer` cookie present and the store card shown. **Deploy smoke (round 3):** `/v1/cvs/ecpay/map-return/*` and `/v1/cvs/ecpay/status/*` reach Go on `LC_HOOKS_HOST`, and any other `/v1/cvs/*` path there, or on `LC_API_HOST`, returns 404. |
| TCV09 | `TestCvsNoIframeAndPrivacy` + review | REVIEW + regression | CSP `frame-src` unchanged (no ECPay frame); no PII/keys in logs/URLs (I11); PROCESS §5 comments; full `go test -race ./...`, `go vet ./...`, `python3 scripts/check_packet.py`, MF/RF/SP/BPH suites unchanged; independent test_worker + security_reviewer verdicts. |
| TCV10 | `ecpay_okmart_probe_test.go` | SANDBOX | Record OK mart evidence. **Expected FAIL on the directory leg** (GetStoreList has no OK `CvsType`, F10/F18, checked 2026-09-29), so the Q4 default (OK shown disabled "即將開放", no second provider in R2, §15) is the plan. Probes run only to record evidence: Express/map with `OKMARTC2C`, Create with stage store `1328`, print, query. `ok_verified` may be set only if a later ECPay doc/API adds an OK directory and this gate is re-run green. |
| TCV11 | `TestCvsOptions` | REAL_PG + HTTP_PG | With an enabled qualified profile, CVS rows carry `pickup_selection:"ecpay_map"` (OK hidden/`coming_soon` until `ok_verified`, Hi-Life until `hilife_verified`); without one, MANUAL CVS rows carry `pickup_selection:"buyer_entered"` plus the chain search URL; only kinds in `enabled_chains` are listed; `payment_modes` includes `"pay_at_pickup"` only when enabled; hidden/disabled variants follow the merchant-service-settings three-axis rule; `CVS_ECPAY_ENABLED=0` with an enabled profile → rows `{available:false, reason:"temporarily_unavailable"}`, never `buyer_entered` (X9). |
| TCV12 | LIVE directory probe | LIVE (read-only) | Owner's real ECPay account: connect probe (GetStoreList) passes; the full LIVE UNIMART, FAMI and HILIFE lists parse under the 32 MiB cap (row count and byte size recorded as evidence); map with a real store on a phone returns a `StoreId` (length recorded) present in the directory for 7-11/FamilyMart/Hi-Life. No create. Requires owner-provided credentials via env; owner approval not needed for read-only (no label bought) — still announced in chat. |
| TCV13 | LIVE one parcel | LIVE (owner-approved, §0.3 P4) | One real C2C shipment: create → code → drop → status notifications 2030/3024 → at store → picked up, all MAC-verified at the production status URL. Only then `CVS_ECPAY_LIVE_CREATE=1`. |
| TCV14 | `TestCvsBuyerEnteredStore` + `taiwan-cvs.spec.ts` (buyer_entered) | REAL_PG + BROWSER | Store without a profile: options carry `pickup_selection:"buyer_entered"` and the four official search links; per-chain code regexes (§16.1: 7-11 `123456` ok / `12345` and `1234567` refused; FamilyMart `006598` ok, leading zero kept; OK `1328` ok / `13280` refused; Hi-Life 3..8 digits); name/address bounds and control chars refused; version written with `BUYER_ENTERED`, NULL principal, namespace `buyer.<owner_id>`; buyer B cannot bump or read buyer A's head (under `commerce_buyer_runtime` and `commerce_checkout_runtime`); merchant `commerce_runtime` cannot write `buyer.*` or `BUYER_ENTERED`; a `BUYER_ENTERED` pickup is never reused as verified: Begin with it while the store has an enabled qualified profile → PT422 `cvs_source_mismatch`, `request_cvs_shipment` → 422 `no_cvs_destination`; merchant order detail shows the `buyer_entered` label; browser: desktop + mobile, 3 locales, links open the chain page in a new tab (`rel=noopener`), field errors from 422 codes. |
| TCV15 | `TestCvsPayAtPickupBegin` | REAL_PG + HTTP_PG | `payment_mode=pay_at_pickup` Begin: order `CONFIRMED`, reservation `COMMITTED`, RESERVE + ALLOCATE ledger rows (BUYER, `checkout.pay_at_pickup.commit`), `collection_state='PENDING'`, **zero `checkout.payment_attempts`, zero Stripe sessions on the fake**, `start_payment` and `start_stripe_payment` refused (PT409), zero `payments.stripe_sessions` rows; expiry job reaches STALE (no release); amount cap: total = `pay_at_pickup_max_twd`×100 accepted, +100 refused `pay_at_pickup_amount_exceeds`, non-whole TWD refused, >20000 TWD refused; disabled setting / chain not in `enabled_chains` / home destination / non-TWD → PT422 `pay_at_pickup_unavailable` with zero holds; C3 recipient (`0912345678` ok, `+886912345678` normalised, landline/`08…` refused; name with digits refused); `order_money_shippable` true for a CONFIRMED pay_at_pickup order, manual shipment (0063) allowed, Stripe refund request refused by the existing stripe-refund-v1 request definer (no CAPTURED attempt) with zero `payments.stripe_refunds` rows; ledger guard refuses a BUYER ALLOCATE for a card order; card Begin regressions unchanged; **round 4:** the (max_open+1)th open order gets PT429 `pay_at_pickup_limit` with zero holds, a second open order by the same owner gets PT429, and two concurrent Begins at max_open−1 produce exactly one order. |
| TCV16 | `TestCvsCollectionStatus` | MOCK + HTTP_PG | pay_at_pickup shipment: frozen `collection_amount = goods_amount = total/100`, fake Create receives `IsCollection=Y` + `CollectionAmount` (card order: `N`, no CollectionAmount); **round 4:** `request_cvs_shipment` for a CONFIRMED pay_at_pickup order with an ecpay_map pickup returns 202 REQUESTED, and a pickup namespace from another environment returns 422 `cvs_environment_mismatch`; signed 2067 (UNIMARTC2C) and 3022 (FAMIC2C/HILIFEC2C) → PICKED_UP + `collection_state='COLLECTED'` + audit; 2074/3020 → UNCLAIMED + `RETURNED`, no ledger row, no payment/refund row; duplicate report → one transition; merchant `collected` action (fulfillment:write; `orders:read` only → 403) on a manually shipped order → COLLECTED + audit, replay same body, conflicting later report → event only + alert `collection_conflict`; `refunded_offline` only from COLLECTED; card orders refuse the action (422 `not_pay_at_pickup`). |
| TCV17 | `TestCvsPayAtPickupRelease` | REAL_PG + HTTP_PG | §16.8, under the real `commerce_runtime` login after a full Apply: `cancel` of a PENDING unshipped pay_at_pickup order → `commercial_state`/`fulfillment_state` `CANCELLED`, `collection_state='CANCELLED'`, reservation `RELEASED`, one `DEALLOCATE` row per line (`delta_allocated=-quantity`, MERCHANT, principal, `operation`/`command_key`/`reason` as §16.8), balance `allocated` back to its pre-Begin value, audit row, zero payment/refund/`integration.operations`/`river_job` rows; the freed slot lets a Begin at `pay_at_pickup_max_open` succeed; replay same key+body → same body and zero extra ledger rows, other body → PT409 `idempotency_conflict`, a new key on a released order → PT409 `collection_state_changed`; refused: REQUESTED/UNKNOWN CVS attempt (409 `cvs_attempt_in_flight`), CREATED or later (422 `not_cancellable`), 0063 manual shipment (422 `not_cancellable`), card order (422 `not_pay_at_pickup`, zero ledger rows; RD6), `orders:read` only (403), a revoked member replaying a valid key (403); a MERCHANT DEALLOCATE whose principal or buyer GUC differs → 42501; a MERCHANT non-DEALLOCATE row with `checkout_id` → 42501 (0013 regression); ECPay 2074 → 2098 → restock refused (409 `parcel_not_returned`); a later status/finish for an ABANDONED attempt on a CANCELLED order → event + `duplicate_label_risk`, no state or ledger change; `restock` of a RETURNED order (ECPay 3020 and merchant `returned`) → `RESTOCKED` + DEALLOCATE rows, restock of PENDING/COLLECTED refused; two concurrent releases → one; direct DEALLOCATE INSERT by `commerce_runtime`, by `commerce_checkout_writer` without the order state, with a wrong quantity, or a second one for the same line → refused; a later signed 2067 on a RESTOCKED order → event + `collection_conflict`, no state change; card cancel regressions (RD6) unchanged. |

Each gate records one red run before its green run (PROCESS §2.4).

## 11. Ownership and sequence

| Artifact | Owner |
| --- | --- |
| `0072_taiwan_cvs_logistics.sql`, `0073_taiwan_cvs_functions.sql`, the post_river file (R-6), route mounts, `core-openapi.json`, `tasks.json` (T13) | integrator |
| `deploy/caddy/Caddyfile` (hooks-host CVS routes), `deploy/compose.yml` + `deploy/env/api.env.example` (`COMMERCE_CVS_HOOKS_ORIGIN`), §12 | integrator |
| `internal/integrations/shipping/ecpay` (wire, MAC, directory cache, dispatcher route) | integration_worker |
| `internal/fulfillment` selection/shipment Go, `internal/checkout` options/begin deltas, buyer/merchant HTTP handlers | commerce_worker |
| storefront CVS picker + order page, admin connect card + shipment section, BFF routes | ui_worker |
| `tests/foundation/taiwan_cvs_*`, `tests/integrations/ecpay_*`, `taiwan-cvs.spec.ts` | independent test_worker |
| MAC/ingress/PII review | security_reviewer |
| `internal/integrations/core` R-7 (`Outcome.Detail`, `DispatchRoute.Finish`, `ReconcileWithSecret`) + its dispatcher tests | integrator |

Sequence: review + freeze this file → R-7 core change (+ dispatcher tests) → 0072 + TCV02 → wire package + TCV01/TCV07/TCV10 (can run in
parallel with 0073 once 0072 is frozen) → 0073 + TCV03–TCV06/TCV11/TCV14–TCV17 → UI + TCV08 → TCV09 verdicts →
owner TCV12 → owner-approved TCV13. The manual path (0063) stays live throughout.

## 12. Configuration and secrets

`ECPAY_LOGISTICS_KEYRING` (AEAD keyring, R-1; supplied on the server as an owner-provided file, O-D —
never through chat), `COMMERCE_CVS_HOOKS_ORIGIN` (round 3, replaces `PUBLIC_API_ORIGIN`, which exists
nowhere in deploy/, cmd/ or internal/; used for both ServerReplyURLs — must be reachable from
`postgate.ecpay.com.tw` through any Cloudflare proxy, F11, §0.3 P1), set in deploy/compose.yml api env to
`https://${LC_HOOKS_HOST:?set LC_HOOKS_HOST}` — the same pattern as COMMERCE_PAYMENT_NOTIFY_URL at
compose.yml:156 — and added to deploy/env/api.env.example; a startup config error when unset while
CVS_ECPAY_ENABLED=1. deploy/caddy/Caddyfile gains, under `{$LC_HOOKS_HOST}` (where the BFF headers are
already stripped, Caddyfile:138-140), before its final `handle { respond 404 }`:
`handle /v1/cvs/ecpay/map-return/* { reverse_proxy 127.0.0.1:8080 }` and
`handle /v1/cvs/ecpay/status/* { reverse_proxy 127.0.0.1:8080 }`. `{$LC_API_HOST}` stays default-deny
(Caddyfile:121-128). The Caddyfile, compose.yml and api.env.example belong to the integrator (§11).
`CVS_ECPAY_ENABLED` (0/1, default 0: ECPay routes 404, and CVS rows of a store with an enabled profile are
returned `{available:false, reason:"temporarily_unavailable"}`; the mode itself is SQL-only (§16.1), so to fall
back to `buyer_entered` the merchant disables the profile (`set_ecpay_logistics_enabled`); `=1` requires buyer
payment enabled with `COMMERCE_PAYMENT_PROFILE` set, else startup config error — the profile is the
`p_payment_environment` source, §4.3),
`CVS_ECPAY_LIVE_CREATE` (0/1, default 0; owner-only, §0.3 P4). Sandbox test keys only in
`~/.config/livecommerce/secrets.env`; never in repo, logs, fixtures (split literals only for the
official F6 vector), commits or agent replies.

## 13. Known limits / NOT_RUN

- Evidence: DESIGN only; TCV01–TCV17 NOT_RUN (round-1, round-2, round-3 and X8/X9 amendments are also DESIGN; no code or SQL ran; code/SQL references were read, not executed). SANDBOX cannot exercise status notifications (F7) or
  a real map (stage fixed store, F3); those are MOCK until TCV12/TCV13 (LIVE).
- **Cross-border first mile is not covered**: ECPay moves the parcel only from a Taiwan store/DC to
  the pickup store (Q2). Overseas pickup, customs, forwarder handoff remain merchant-arranged.
- OK mart depends on TCV10 (F2/F5/F10 contradict); no OK status code mapping.
- One parcel per order (MD1); no split/merge; no bulk create/print (Q8); no home-delivery COD (the
  rulings allow CVS pay-at-pickup only, §16); no B2C frozen
  (`UNIMARTFREEZE`); no store change after creation (`UpdateStoreInfo`), no 7-11 C2C cancel API, no
  returns/RMA/unclaimed return handling beyond displaying UNCLAIMED and the pay-at-pickup restock (§16.8); no home
  delivery via ECPay.
- Pay-at-pickup cancel/restock (§16.8) is merchant-initiated, whole-order only: no buyer cancel, no automatic
  timeout cancel, no per-line cancel/restock, no automatic restock on 2074/3020. Card orders keep the no-cancel
  rule (stripe-refund-v1 RD6).
- MD5 keyed MAC is the vendor's scheme (weak hash, keyed prefix/suffix); accepted as provider
  authentication, not reused elsewhere. Callbacks from ECPay are not IP-allowlisted (IPs not fixed, F11).
- Directory freshness: 24 h pickup validity vs daily 20:00 refresh; a store closing inside that window
  is caught only at create (`0|…`) or by status (關轉) — merchant re-coordinates with the buyer.
- ECPay 403 rate limit (30 min) can exhaust the reconcile budget → manual UNKNOWN (§7.4).
- Hi-Life store-code length and the created-only status code set are UNKNOWN/UNVERIFIED (F17, F19)
  until TCV07/TCV12; Hi-Life stays hidden ("即將開放") until `hilife_verified`.
- A refund committed in the sub-second window between `load_cvs_create` and the ECPay call still buys
  the label; the merchant sees the refunded-order warning and does not drop the parcel.
- A directory row is shared per (environment, CvsType) in-process; LIVE list size is estimated, not
  measured (F18) until TCV12.
- Credential rotation while shipments are in flight breaks status MAC verification for them (§9).
- One ECPay environment per deployment (= payment profile): staging runs SANDBOX, production LIVE only
  (LQ5); a merchant cannot test stage ECPay on production.
- A non-conforming provider code is stored NULL (merchant reads it in the ECPay backend); the
  code-column bounds are generous but UNVERIFIED until TCV07/TCV12 record real values.
- The storefront return is limited to product pages; a claim-page checkout needs an amendment.
- F13/F15/F16 are partially UNVERIFIED (search summaries or third-party docs); do not quote them to
  merchants as provider terms.

## 14. Round-1 review resolution (2026-09-29)

Source: `output/contract-review/r2-design-wave.json` → `cvs.review.findings` (verdict BLOCK). Each
finding was checked against the code/SQL named; all 10 P1 and 7 P2 are accepted (none rebutted in
substance). Corrections to the reviewer's proposed text are marked "adjusted".

| # | Sev | Verified against | Resolution (section) |
| --- | --- | --- | --- |
| 1 | P1 | 0063:211-237 `manual_shipment_eligible` only checks `MANUAL_UNASSIGNED`, so REQUESTED/UNKNOWN pass | Replaced `manual_shipment_eligible` with a no-live-cvs-shipment clause (§4.3, §6); TCV05 negative. |
| 2 | P1 | §7.4 draft re-checked only state/profile/flag | Dispatch-time `cvs_order_payable` in `load_cvs_create` → BLOCKED_POLICY, zero sends (§4.3, §7.4); TCV05. Adjusted: the check lives in LoadSecret, not Check — `DispatchRoute.Check` has no tx (dispatcher.go:44-57). |
| 3 | P1 | dispatcher.go:363-425 (no finish hook; `Outcome` = State/Code/ProviderReference); 0008 `claim_operation` writes STALE_BINDING itself; LoadSecret dispatch-only | R-7 core extension (`Outcome.Detail`, `Finish`, `ReconcileWithSecret`), full outcome mapping, `settle_cvs_attempt` for claim-side terminals (§0.2, §4.3, §6, §11). Adjusted: reviewer's hook alone misses claim-side STALE_BINDING and unsigned reconcile. |
| 4 | P1 | 0061:5-12 TWD minor = ×100, `%100=0` | Minor-unit bounds 100..2000000, `goods_amount = minor/100` (§4.3 begin_hold, request); TCV04 boundaries. |
| 5 | P1 | 8809 re-fetched: `ReceiverStoreID` String(6), OK stage `1328` | CHECK `{1,6}`, reject only >6, Hi-Life gated by `hilife_verified`, F17 (§1, TD6, §4.2, §4.3). |
| 6 | P1 | 47496 re-fetched: no filter/paging, S(40)/S(100) fields | 32 MiB streamed cap for GetStoreList; cache keyed (environment, CvsType), 10-min fetch throttle, cache-only inline path; TCV12 size evidence (§7.1, §7.2, F18). Adjusted: shared key instead of LRU sizing (bounded by construction; TCV07 checks the assumption). |
| 7 | P1 | 0012:82-98 runtime INSERT/UPDATE on pickup tables | RESTRICTIVE `ecpay.*` namespace policies + verified-only reuse rule (§4.1, §4.2); TCV02/TCV03 negatives. |
| 8 | P1 | 0013:293-303 writer has lock-only policy, no INSERT | Writer INSERT/UPDATE policies scoped to `ecpay.*`; integration_writer order-snapshot read; three purpose-scoped API key loaders (§4.3). Adjusted: purpose-scoped loaders instead of one generic `(connection, purpose)` loader; recipient read from `checkout.orders.snapshot`, not `destination_snapshots`. |
| 9 | P1 | TD2 index allows SANDBOX+LIVE; 0016/0061 create the payment attempt after Begin | Environment in namespace, `ecpay_one_enabled_profile`, Begin checks pickup env = profile env, request checks profile env = captured payment env (§4.2, §4.3); TCV04/TCV05. Adjusted: the payment environment is unknown at Begin, so that half moves to request time. |
| 10 | P1 | §4.3 abandon never queried; one-live index would abort a late status | Abandon of CREATED needs a fresh created-only Query; FAILED/ABANDONED status = event + alert, always ACK (§4.3, §6, §8); TCV06. Created-only set `{'300'}` is F19 (UNVERIFIED) and evidence-extended. |
| 11 | P2 | draft §4.3 text (order unspecified; `selection_id` is in the ServerReplyURL and storefront URL) | Nonce-first check order, `nonce_mismatch` removed (§4.3, §5.2); TCV03. |
| 12 | P2 | 0016:71, 0035:62, 0061:81, 0062:45-63 — only `operation_actor_family` exists; MERCHANT branch admits any provider | Row rewritten; separate `cvs_create_family` CHECK (§4.1). |
| 13 | P2 | draft §7.5/§4.2 CHECKs (strict format would abort the tx ⇒ no ACK) | REQUESTED ⇒ 503; normalise code/date/message; MAC over all fields, persist F7 only (§4.2, §4.3, §7.5); TCV06. |
| 14 | P2 | `apps/storefront/app/[locale]` has only `claim`, `products/[productID]`; locales are `zh-TW`/`zh-CN`/`en` (lib/claim-copy.ts) | Stored allowlisted `return_path` (§4.2, §5.2). Adjusted: reviewer's regex used `zh-Hant/zh-Hans`, which the storefront does not use. |
| 15 | P2 | 0061:32 `stripe_account_identity_unique` | `ecpay_logistics_identity_unique` (§4.1). |
| 16 | P2 | 47496/8795 CvsType and map subtypes omit OK | TCV10 restated as expected FAIL on the directory leg; Q4 default is the plan (§10, §15). |
| 17 | P2 | 0008/0027 permission list has `integration:read` | §0.3 owner prerequisites; R-5 adds `integration:read` (§0.2, §0.3). |

Found while verifying (not in the review): `LoadSecret` runs only in dispatch mode, so the draft's
signed Query/V5 reconcile could never run — fixed by R-7b. `cvs_selections` FK now targets the existing
0012 `UNIQUE(tenant_id,store_id,id,kind,country)` instead of a new unique.

### 14.1 Round-2 review resolution (2026-09-30)

Source: the round-2 review of this file (4 P1). All four accepted after verification; one factual
detail of finding 1 corrected ("adjusted").

| # | Sev | Verified against | Resolution (section) |
| --- | --- | --- | --- |
| R2-1 | P1 | `core.Service.Plan` service.go:179-260 (authorizes `integration:execute` at :188, semantic_key = HTTP key, operation id generated after `InsertTx`); 0064:829-884 `plan_claim_reply` (job verified by kind/queue/unique_key/args/`xmin`, :861-863); 0016:85 + 0062:281 checkout_writer operation policies | `request_cvs_shipment` takes `p_operation`/`p_job`, verifies the job, inserts operation + READY event itself; policy-scoped INSERT; `PT2RP` replay + `read_cvs_shipment_command`; R-5, R-6 (§0.2, §4.3, §9); TCV05. Adjusted: an orphan job could not actually commit — post_river 0014 `external_operation_job_commit` (deferred) refuses a job without its operation — so the defect was every replay failing at COMMIT (500), not a committed orphan; fix unchanged. |
| R2-2 | P1 | handler.go:377 origin header; `buyer.resolve_published_store` 0020:51-74; buyer-server.ts:20 `__Host-` cookie, :781 `SameSite=Lax`; ClaimLink.tsx:120-146 fragment dropped; `OrderFlow` only via ProductPurchase; no `STOREFRONT_ORIGIN` in deploy/, cmd/, internal/, apps/ | `return_origin` stored at open (must be the store's ACTIVE domain), return_path products-only, 303 to stored origin, unknown → 404; `STOREFRONT_ORIGIN` deleted (§4.2, §4.3, §5.2, §12); TCV03/TCV08 two-store cases. Also corrected the §5.2 route prefix (buyer routes are `/v1/buyer/...`, store from origin). |
| R2-3 | P1 | cmd/api/buyer_payment.go:44 (one `COMMERCE_PAYMENT_PROFILE` per deployment); deploy/scripts/preflight.sh P06/P08 | `p_payment_environment` pinned at register/open/Begin; request-time CAPTURED check kept as second guard (§4.3, §12, §15 LQ5); TCV04. |
| R2-4 | P1 | dispatcher.go:363-368 (`completeAndFinish` → `errCompletionUncertain`), :413-425; 0008:143-164 (expired lease re-claimed as reconcile); settle needed `lease_until IS NULL` | Finish total over provider data (NULL + `ecpay.code_nonconforming`), adapter id rule, settle rule for leases stale > 1 h, Finish on ABANDONED = alert only, UNKNOWN abandon with found trade → CREATED (§0.2 R-7a, §4.2, §4.3, §6); TCV05. Stronger than proposed: code CHECKs widened to `^[0-9A-Za-z_-]{1,40}$` so a longer FamilyMart/Hi-Life number is kept rather than lost (the reviewer's 16-char case now stores; the NULL path is tested with 41 chars). |

Found while verifying (not in the review): `checkout.begin_hold`'s latest body is in `post_river/0005`,
and migrate.go applies post_river after all main migrations, so the replacement must live in the new
post_river file (DROP old 8-arg + CREATE the new one — 10-arg since round 3, §16.2); TCV02/TCV04 assert a single signature.

### 14.2 Round-3 review resolution (2026-09-30; rulings X1, X5)

Source: `output/contract-review/r2-round3.json` → `cvs` (verdict BLOCK, 4 P1, no P2). X1: fixes 1–3 applied
with the reviewer's exact text; X5: finding 4 implemented as §16 in this contract. Re-verified before applying.

| # | Sev | Verified against | Resolution (section) |
| --- | --- | --- | --- |
| R3-1 | P1 | migrate.go:180-184 (end-of-Apply `REVOKE ALL ON river.river_job` / `ON SCHEMA river` FROM `commerce_checkout_writer`); migrate.go:154 + post_river 0014:19 (integration_writer SELECT on `river.river_job`); 0064:861-864 job check; 0008:106-117 (`worker_binding_lock`, `GRANT UPDATE(id)`, `worker_event_insert`) | `integration.plan_cvs_create` (integration_writer-owned), `cvs_create_insert` policy, `checkout_cvs_operation_read` without principal clause; river GRANT deleted from R-6 (§0.2, §4.3); TCV02 river negative; TCV05 member B settles/abandons A's attempt. |
| R3-2 | P1 | §4.3 grant paragraph (integration_writer: SELECT only on shipments, nothing on events) | `fulfillment.apply_cvs_create_result` (checkout_writer-owned, EXECUTE only integration_writer) does all fulfillment writes; TCV02 negative; TCV05 REAL_PG Finish under real roles (§4.3). |
| R3-3 | P1 | deploy/caddy/Caddyfile:121-128 (`LC_API_HOST` 404 except /healthz), :132-156 (hooks host, BFF headers stripped at :138-140); compose.yml:156 `COMMERCE_PAYMENT_NOTIFY_URL`; no `PUBLIC_API_ORIGIN` in deploy/, cmd/, internal/ | `COMMERCE_CVS_HOOKS_ORIGIN` + two Caddy `handle` routes on the hooks host, integrator-owned (§0.3 P1, §5.2, §7.4, §11, §12); TCV08 deploy-smoke routing. |
| R3-4 | P1 | r2-design-rulings.md C1–C4 (2026-09-30) | Header, §5.1, §7.4, §13, §15 rewritten per the reviewer's text; §16 (a)–(f) added. Adjusted/stronger: buyer-entered namespace is per buyer owner (`buyer.<owner_id>`), not `buyer.<kind>` — kind is already in the head key, and a shared head would let one buyer's entry move the head and fail another buyer's Begin (post_river/0005:193-196 requires head = source); settings live in a per-store table (a MANUAL store has no ECPay profile); the "existing return path" for stock does not exist (§16.4). |

### 14.3 §16 verification review resolution (2026-09-30; rulings X1, X6)

Source: round-3 verification findings on §16 (key `cvs`, 3 P1). All three re-verified against the code and
applied; one strengthened.

| # | Sev | Verified against | Resolution (section) |
| --- | --- | --- | --- |
| R4-1 | P1 | post_river/0005:372 and post_river/0012:171 (`commercial_state<>'DRAFT'` → PT409): a pay_at_pickup order never has a payment attempt/fact, so the CAPTURED-fact equality made every ECPay create 422 | §4.3 `request_cvs_shipment`: CAPTURED-fact check for `card` only; `pay_at_pickup` relies on the Begin-pinned pickup-namespace environment; TCV16 202 + cross-environment 422. |
| R4-2 | P1 | 0027:57-59 (commerce_auth column list), 0063:211-240 (`manual_shipment_eligible` INVOKER) used at 0063:491/543; 0013:175 (writer UPDATE columns); §16.5 table had no grants/policies; options.go:108 runs on the checkout pool (cmd/api/buyer.go:91,114; platform.go:303-316 one authority) | §4.3 grant paragraph "§16 (round 4)": column SELECT/UPDATE grants, `cvs_settings_rw`, `fulfillment.read_cvs_offer`; §5.1; TCV02. **Stronger:** `read_cvs_offer` EXECUTE is `commerce_checkout_runtime`, not the reviewer's `commerce_runtime` (the options login is not a `commerce_runtime` member, so that grant would fail with 42501); negatives cover both runtime roles. |
| R4-3 | P1 | buyer-session-registration-v1 (anonymous bootstrap); 0013:569-570 (`expire_held` STALE for non-DRAFT); 0013:164 `writer_access` USING(true) (store-wide count visible to the definer) | §16.5 `pay_at_pickup_max_open` (table, PUT, definer, UI); §16.2 settings FOR UPDATE + PT429 `pay_at_pickup_limit` (store count and one per owner) + partial index; §16.7; TCV15. |

### 14.4 Amendment X8/X9 resolution (2026-09-30; rulings X8, X9)

Source: `docs/delivery/units/r2-design-rulings.md` X8/X9; the seven P2s are the §16 verification review's
findings (key `cvs`). All re-verified against code/SQL; none rebutted.

| # | Sev | Verified against | Resolution (section) |
| --- | --- | --- | --- |
| X8 | ruling | 0013:569-570 (`expire_held` STALE for non-DRAFT), 0013:598 (expiry cancel pair), 0018:155-163 (no kind lowers `allocated`), 0063:330 (GUCs from the order), 0013:205/234 (writer reservation policy/grant) | §16.8 cancel/restock through `inventory.release_pay_at_pickup` (new `DEALLOCATE` kind, MERCHANT branch, guard, once-per-line index); §4.1; §8; §16.4; §16.7; §13; TCV17. |
| X9-1 | P2 | §5.1/§16.1 vs TCV11 | TCV11 replaced with the reviewer's text plus the kill-switch case. |
| X9-2 | P2 | MG01/MG02 had no row in §10 | Every MG01 → TCV02/TCV04, MG02 → TCV02; TCV02 gains the river/integration_writer/single-`begin_hold` negatives. |
| X9-3 | P2 | 0012 `scoped_read` (store-wide for `commerce_buyer_runtime`); 0013:196-203 `checkout_runtime_read` (store-wide for `commerce_checkout_runtime`) | §16.1 RESTRICTIVE `buyer_entered_own`. **Stronger:** on both buyer roles, not only `commerce_buyer_runtime`. |
| X9-4 | P2 | Go flag vs SQL profile check disagreed | §16.1 mode SQL-only; §12 flag=0 → `temporarily_unavailable`; TCV11. |
| X9-5 | P2 | 0061:146-154 (other non-MERCHANT branches) | §4.1 BUYER/ALLOCATE branch adds `checkout_id`/buyer columns NOT NULL; TCV02. |
| X9-6 | P2 | post_river/0012:134,171 | §16.2 cites `start_stripe_payment`; TCV15 refuses both, zero `stripe_sessions`. |
| X9-7 | P2 | cmd/api/buyer.go:86,91; handler.go:528-629 (buyer pool vs checkout pool); platform.go:58-65,303-316 (one authority per login); cmd/api/main.go:65 | §4.3 pool note; buyer CVS definers EXECUTE `commerce_checkout_runtime` (`open_cvs_selection`, `read_cvs_selection`, `record_buyer_cvs_store`, buyer-path `verify_cvs_selection` + `load_ecpay_key_for_selection`, new `read_buyer_cvs_shipment`); merchant/unauthenticated definers stay `commerce_runtime`; TCV02 runs them from a real checkout-pool login. |

### 14.5 Amendment X8/X9 review resolution (2026-09-30)

All four findings re-verified against SQL; none rebutted.

| # | Sev | Verified against | Resolution (section) |
| --- | --- | --- | --- |
| A1 | P1 | 0013:129-158 `guard_checkout_ledger` (only definition; MERCHANT rows need NULL checkout/buyer columns, 42501) | §16.8 Schema: DEALLOCATE branch in `guard_checkout_ledger`; §4.1; TCV02; TCV17 negatives. |
| A2 | P1 | 0063:266-267 (`merchant_access_denied` `commerce_auth`-only, never granted); 0062:260 (`resolve_access` for checkout_writer); 0062:263-272 (command_results policies need tenant/store/principal GUCs); 0002:162-165 (`apply_ledger` principal check) | §16.8 Route + writer order (resolve_access → GUCs → order lock → buyer GUCs → replay); same auth text in §16.4, §16.5 and the §4.3 `request_cvs_shipment` row (same defect); TCV17 revoked-member replay → 403. |
| A3 | P2 | §6 AT_STORE/UNCLAIMED + 2098 → AT_STORE | §16.8 restock refused while a shipment is CREATED/AT_DC/AT_STORE (409 `parcel_not_returned`); TCV17. |
| A4 | P2 | X8 text vs §6 ABANDONED paths (CREATED lapse + fresh query; UNKNOWN + acknowledgement) | §16.8 cancel: ABANDONED accepted as not handed over — **recorded deviation from X8's literal text**; later status/finish on CANCELLED → event + `duplicate_label_risk`; TCV17. |

### 14.6 R2 close-out amendment notes (2026-09-30; lane `cvs`, branch r2/cvs; implementation test findings and review P2s)

Smallest amendments where the code had to deviate from, or go beyond, the frozen text. Gates: TCV18 (`TestCvsCloseDispatchWindow`,
`TestCvsCloseLoadWaitsForOrderLock`, `TestCvsCloseOperationInsertScope`) and the two unit tests named below.

| # | Amendment | Why |
| --- | --- | --- |
| N1 | §4.3 `load_cvs_create` (dispatch mode): the payable check now runs after `checkout.orders ... FOR SHARE` (inside `fulfillment.cvs_order_payable`), so it waits for a refund that already holds the order row (RD3: `request_stripe_refund` locks the order first). New trigger `payments.guard_refund_cvs_dispatch` (BEFORE INSERT on `payments.stripe_refunds`) refuses a card refund with `PT409 cvs_attempt_in_flight` (HTTP 409 `conflict`) while the order's CVS shipment is REQUESTED and its operation is DISPATCHING (the Create may be on the wire). A READY (unclaimed) operation stays refundable: that refund is what stops the dispatch (TCV05). | Review P2: a full refund that committed between the worker's load and ECPay's answer still got a label (no transaction is open in that window). stripe-refund-v1 RD6 (refunds never change fulfilment state) is untouched: the refusal writes nothing and is transient. |
| N2 | §4.3 `cvs_create_insert` policy adds `tenant_id`/`store_id` = the caller's `app.*` GUCs, like every other integration_writer insert policy. | Review P2 (defence in depth; `plan_cvs_create` sets both GUCs before its insert). |
| N3 | §7.5 applies to the map-return hook too: per-selection cap 4 and a global cap 16 concurrent map-return transactions, answered `503 busy` (retryable, nothing recorded), taken after the body is read. | Review P2: the hook is public and unsigned; each well-formed POST opens a `FOR UPDATE` transaction. |
| N4 | §4.3 `register_ecpay_logistics` input: `merchant_id` is digits only at the Go boundary (`fulfillment.ecpayMerchantID` = `ecpay.merchantIDRE`, ECPay's ids are numeric); the SQL CHECKs keep the wider alnum set (a superset, never reached by a lettered id). | Test finding: a lettered id passed Connect validation and failed later as `ecpay_probe_failed`. |
| N5 | §12 wiring: `COMMERCE_CVS_HOOKS_ORIGIN`, `CVS_ECPAY_ENABLED` and `CVS_ECPAY_LIVE_CREATE` are set only in `deploy/compose.yml` (api and claims-worker), not in `api.env.example`: preflight P06 rejects wiring keys in the env example. One `compose.env` switch `LC_CVS_ECPAY_ENABLED` drives `CVS_ECPAY_ENABLED` on both services; the claims-worker is pinned to `COMMERCE_PAYMENT_PROFILE=SANDBOX` and `CVS_ECPAY_LIVE_CREATE=0`, so LIVE create needs an owner-approved edit of those compose lines (as §0.3 P4 already requires). | Integrator merge finding (97981f0): the original §12 text failed the preflight knob allowlist and duplicated a YAML key. |

## 15. Integrator rulings (2026-09-29)

From `docs/delivery/units/r2-design-rulings.md` (binding; the owner may revise before go-live). Question
numbers are kept from the first draft so review references still resolve.

Round-3 rulings: `docs/delivery/units/r2-design-rulings.md` "Round-3 integrator rulings (2026-09-30)" X1
(reviewer's fix text accepted), X5 (C1–C4 as §16) and X6 (frozen at "v1 FROZEN 2026-09-30"; owner questions
keep their defaults and never block implementation) apply to this contract (§14.2, §14.3).

| Ref | Accepted default | Where applied |
| --- | --- | --- |
| Q3 | Store-to-store (C2C) is the default mode; bulk/B2C later on the same code path (`mode`). | TD5, §4.2 profile `mode`, §8 |
| Q4 | Run the OK mart probe (TCV10) first; if it fails (expected, F18), OK mart is shown disabled as "即將開放". **No second provider in R2.** | TD6, §5.1, TCV10 |
| C1 | Owner clarification 2026-09-30 (supersedes Q5 "no COD in v1"): two store sources per store — ECPAY_MAP with an ECPay connection, else MANUAL buyer-entered (`BUYER_ENTERED`, never "verified"). | §16.1, §5.1, §4.1 |
| C2 | Pay-at-pickup is an order payment mode (`payment_mode='pay_at_pickup'`), HELD→COMMITTED at placement, no Stripe session; ECPay `IsCollection=Y` + `CollectionAmount`; collected/returned by status or merchant action; offline refunds only. | §16.2–§16.4, §7.4 |
| C3 | Pickup-and-pay recipient: real name (as on ID) + TW mobile `^09[0-9]{8}$`. | §16.2, §4.3 `ecpay_recipient_ok` |
| C4 | Per-store settings: `enabled_chains`, `pay_at_pickup_enabled`, `pay_at_pickup_max_twd`. | §16.5, §5.1 |
| X5 | C1–C4 live in this contract as §16 (not a separate contract), before 0072 freezes. | §16 |
| Q6 | The merchant clicks "建立超商寄件" per order; never auto-created at payment. | TD5, §8 |
| Q8 | Bulk create / bulk print is a follow-up contract (`taiwan-cvs-bulk-v1`). | §13 |
| Q9 | One consent line under the chosen store card (3 locales), no separate checkbox. | §5.2 UI |
| Q10 | MANUAL CVS services also use the ECPay map when the store has a qualified connection. | §5.1 |
| O-D | Production is operated by Claude on the owner's server; `ECPAY_LOGISTICS_KEYRING` and ECPay keys arrive as owner-supplied files, never through chat. | §0.3 P1, §12 |
| LQ5 | Production is LIVE-only; SANDBOX runs in a separate staging deployment. Applied here as: the ECPay environment is pinned to the deployment payment environment at register/open/Begin (round 2), with the request-time CAPTURED check as a second guard. | §4.3, §12 |

Still pending an integrator ruling (engineering proceeds on the draft text): R-1 (single keyring for API
and worker), R-2 (API service enable via binding trigger), R-3 (pickup principal), R-4
(`PROVIDER_LABEL_CREATED`), R-6 (0072/0073 split + one post_river file), R-7 (core dispatcher extension,
incl. "Finish is total over provider data"). The auth rulings R-1..R-6 in the rulings file are
merchant-password-auth's, not these.

Open owner question (the only one; blocking before 0073 implementation, engineering proceeds on the
default): **Q1/Q2** — which Taiwan entity/account holds the ECPay logistics contract (each merchant's own,
or a platform partner account)? Default: (a) BYO per store (§0.1). Owner approval step (not a question):
§0.3 P4 (the one real TCV13 parcel).

## 16. Owner clarification C1–C4 (2026-09-30; ruling X5) — store source, pay-at-pickup, recipient, store settings

Source: `docs/delivery/units/r2-design-rulings.md` "CVS owner clarification", supersedes Q5. Scope is CVS
only: the rulings give no home-delivery COD, so none is added. Evidence: DESIGN; TCV14–TCV17 NOT_RUN. All
SQL below is in 0072 (schema) / 0073 (functions) / the R-6 post_river file (`begin_hold`) with `COMMENT ON`
per PROCESS §5.

### 16.1 Store source (C1; spec (a))

- **Mode per store** is decided only in SQL (X9): `ecpay_map` iff the store has its single enabled, qualified
  profile (§4.2); else `buyer_entered`. `CVS_ECPAY_ENABLED` never changes the mode (§12). `ecpay_map` is §3–§5.2 unchanged (TD3).
- **Buyer route** (private Go, BFF mirror `/api/buyer/`): POST `/v1/buyer/cvs-stores`, `Idempotency-Key`,
  body exactly `{cart_version, market_id, service_code, store_code, store_name, store_address}` → 201
  `{pickup_id, kind, code, name, address, source:"buyer_entered"}`; then the normal SetDestination. Errors:
  409 cart/version, 422 `service_unavailable`, `bad_store_code`, `bad_store_name`, `bad_store_address`, 429.
- **Definer** `fulfillment.record_buyer_cvs_store(buyer_hash bytea, store uuid, key text, request_hash bytea,
  cart_version bigint, market uuid, service_code text, store_code text, store_name text, store_address text)
  RETURNS jsonb`, owner `commerce_checkout_writer`, EXECUTE `commerce_checkout_runtime` (checkout pool, §4.3 pool note; X9). Buyer scope (buyer.WithScope
  pattern, before replay); command result `fulfillment.cvs_store.enter`; ≤20 per owner/cart per hour (PT429).
  Locks cart FOR SHARE (current, nonempty) → service head → current enabled+visible **MANUAL** CVS service
  in TW whose kind ∈ `enabled_chains` (§16.5) → refuses (PT422 `service_unavailable`) when the store has an
  enabled qualified profile (that store is `ecpay_map`). Validation (Go mirrors it; golden table in
  `testdata` shared with the SQL test, like `ecpay_recipient_ok`):

  | kind | `store_code` regex | Basis (F21, retrieved 2026-09-30) |
  | --- | --- | --- |
  | `cvs_711` | `^[0-9]{6}$` | VERIFIED — https://emap.pcsc.com.tw/ "6碼店號" |
  | `cvs_familymart` | `^[0-9]{6}$` | VERIFIED — https://family.map.com.tw/famiport/storeNumberFreeze.aspx "共「6」碼" |
  | `cvs_okmart` | `^[0-9]{4}$` | VERIFIED — https://www.okmart.com.tw/convenient_shopSearch "店號」(共4碼)" |
  | `cvs_hilife` | `^[0-9]{3,8}$` | **UNKNOWN** — no length on the official pages; widen/narrow only with evidence |

  `store_name`: trimmed, 1..40 chars, no control chars; `store_address`: trimmed, 5..120 chars, no control
  chars. Codes stay strings (leading zeros kept).
- **Pickup row:** `verification_kind='BUYER_ENTERED'`, `principal_id NULL` (§4.1), namespace
  `'buyer.' || owner_id` (per buyer owner; §14.2 R3-4), `code`/`name`/`address` as validated, `evidence_ref =
  'buyer-entry:' || encode(request_hash,'hex')`, `valid_until = attested_at + 24 hours`. Reuse of the head's
  current version only when it is `BUYER_ENTERED` with the same name/address, enabled, and `valid_until >
  now() + 1 hour`; else one new version with head CAS (same advisory key and lock order as `AttestPickup`).
- **Grants:** `commerce_checkout_writer` INSERT on `pickup_versions`/`pickup_heads` and
  UPDATE(current_version,pickup_id) on heads through new policies scoped by app tenant/store GUCs AND
  `namespace = 'buyer.' || current_setting('app.buyer_id')` AND (versions) `verification_kind='BUYER_ENTERED'`;
  `commerce_runtime` is closed out by the RESTRICTIVE `^(ecpay|buyer)\.` policies (§4.1). Reads (X9; 0012 `scoped_read`
  and 0013:196-203 `checkout_runtime_read` are store-wide): RESTRICTIVE policy `buyer_entered_own` FOR SELECT TO
  `commerce_buyer_runtime`, `commerce_checkout_runtime` on `pickup_versions` and `pickup_heads`
  `USING(namespace !~ '^buyer\.' OR namespace = 'buyer.' || current_setting('app.buyer_id',true))`, so buyer B
  never reads buyer A's owner id or entered store; merchants (`commerce_runtime`) and definers still see them.
- **Never reused as verified:** the `buyer.` namespace cannot hold `PROVIDER_DIRECTORY_VERIFIED` (policy);
  the §4.2 reuse rule requires `PROVIDER_DIRECTORY_VERIFIED`; `request_cvs_shipment` requires a
  `PROVIDER_DIRECTORY_VERIFIED` pickup (→ 422 `no_cvs_destination`); Begin accepts `BUYER_ENTERED` only with a
  MANUAL service and only while the store has no enabled qualified profile, else PT422 `cvs_source_mismatch`.
- **Official store-search links** shown next to the code field (constants in the storefront with URL +
  2026-09-30 comment; open in a new tab, `rel="noopener noreferrer"`; we never fetch or scrape them, no
  chain private endpoint — AGENTS.md): 7-ELEVEN https://emap.pcsc.com.tw/ ; FamilyMart
  https://www.family.com.tw/Marketing/zh/Map (returned HTTP 403 to our fetcher 2026-09-30; browser
  reachability UNVERIFIED — the UI author checks it once and records it, else uses the family.map.com.tw
  page above); Hi-Life https://www.hilife.com.tw/storeInquiry_street.aspx ; OK mart
  https://www.okmart.com.tw/convenient_shopSearch.
- **Labels:** buyer store card "你填寫的門市" (never "已驗證"). Merchant projections (`read_merchant_orders`,
  admin order detail, `export_unshipped_orders`, `read_cvs_shipment`) add `pickup_source:
  "ecpay_directory"|"buyer_entered"|"merchant_attested"`; the admin shows `buyer_entered` as
  "買家自填門市（未驗證，出貨前請至超商官網核對）". ECPay chain gates (`ok_verified`, `hilife_verified`) do not apply
  in this mode (no ECPay create).

### 16.2 Payment mode (C2, C3; spec (b))

- `checkout.orders.payment_mode IN ('card','pay_at_pickup')` + `collection_state` (§4.1). The buyer Begin
  body gains `payment_mode` (default `"card"`); Go passes it as `p_payment_mode` to the 10-arg
  `checkout.begin_hold` (R-6 post_river file; current 8-arg body is `post_river/0005:98`).
- **`card`:** unchanged (then `checkout.start_payment`, Stripe).
- **`pay_at_pickup` branch** of `begin_hold`, after all existing checks, same lock order plus
  `fulfillment.cvs_store_settings` FOR UPDATE after the service head (serialises pay-at-pickup placements per
  store; a missing row means pay-at-pickup off, so every placement locks a row). Guards (any failure ⇒ zero holds):
  destination is a CVS kind in `enabled_chains` and `pay_at_pickup_enabled` → else PT422
  `pay_at_pickup_unavailable`; currency `TWD` and (I05: `total_minor` from the server quote, never the
  client) `total_minor % 100 = 0 AND total_minor BETWEEN 100 AND least(pay_at_pickup_max_twd,20000)*100`
  → else PT422 `pay_at_pickup_amount_exceeds`; **C3** `fulfillment.ecpay_recipient_ok(recipient_name,
  phone)` (real name, no digits/symbols, width 4..10; mobile normalised to `^09[0-9]{8}$`) for every
  pay_at_pickup order, either store source → else PT422 `cvs_recipient_rejected`; refuse PT429
  `pay_at_pickup_limit` with zero holds when the store already has ≥ `pay_at_pickup_max_open` orders with
  `payment_mode='pay_at_pickup' AND collection_state='PENDING' AND fulfillment_state='MANUAL_UNASSIGNED'`,
  or when this buyer owner already has one such order (round 4, R4-3; partial index
  `orders_pay_at_pickup_open(tenant_id,store_id,owner_id)` on that predicate in 0072). The UI labels the field
  "取件人真實姓名（與證件相同）" and "手機 09xxxxxxxx".
- Writes (one tx): order inserted with `commercial_state='CONFIRMED'`, `payment_mode='pay_at_pickup'`,
  `collection_state='PENDING'`; reservation inserted `HELD` with the RESERVE lines exactly as today, then per
  line (sorted, after `inventory.lock_balance`) one ALLOCATE ledger row (`actor_kind='BUYER'`,
  `operation='checkout.pay_at_pickup.commit'`, `command_key = order id`, no payment attempt/fact) and the
  reservation → `COMMITTED` (HELD→COMMITTED, §11.5); events `checkout.held` + `checkout.pay_at_pickup_placed`.
  Result adds `payment_mode` and `commercial_state`. **No payment attempt and no Stripe session:**
  `checkout.start_payment` (`post_river/0005:371`) and the live Stripe entry `checkout.start_stripe_payment`
  (`post_river/0012:171`) both refuse a non-DRAFT order. The expiry job `begin_hold`
  requires is still inserted; `checkout.expire_held` returns STALE for a non-DRAFT order (`0013:569`), so
  it never releases the stock.
- New trigger `inventory.guard_pay_at_pickup_ledger()` (mirrors `inventory.guard_payment_ledger`,
  0061:167): a BUYER ALLOCATE row requires its order to be `pay_at_pickup` and its quantity to equal the
  reservation line, else 42501. catalog-inventory-v1's "payment owns pending/committed" is amended: the
  pay-at-pickup Begin also commits (no reservation-transition trigger exists, grep 2026-09-30).
- **Shippability:** `fulfillment.order_money_shippable` = the card clauses (0063:214-236) AND
  `payment_mode='card'`, OR `payment_mode='pay_at_pickup' AND commercial_state='CONFIRMED' AND
  collection_state='PENDING'` AND no `checkout.payment_attempts` row. So 0063 manual shipment, the export,
  `request_cvs_shipment` and dispatch-time `cvs_order_payable` work unchanged.
- **Refunds:** the Stripe refund request refuses (no CAPTURED attempt); money is returned offline and
  recorded by §16.4 `refunded_offline`. The platform never holds or moves pay-at-pickup money.

### 16.3 ECPay collection (C2; spec (c))

- `cvs_shipments.collection_amount integer NULL CHECK(1..20000)` (§4.2), frozen by `request_cvs_shipment`:
  pay_at_pickup ⇒ `collection_amount = goods_amount = orders.total_minor/100` (F20: must equal GoodsAmount
  for 7-ELEVEN subtypes; applied to all); card ⇒ NULL. Immutable.
- `load_cvs_create` returns it; §7.4 sends `IsCollection=Y` + `CollectionAmount` iff set, else `N`.
- The map form keeps `IsCollection:"N"` (store selection precedes the payment choice). Whether ECPay's
  e-map filters stores by `IsCollection` is **UNKNOWN**; TCV07 records one stage map with `Y`.
- Collected money is settled by ECPay to the merchant's own ECPay account (BYO, TD2); COD fee 0.75% (min 3,
  F12) is the merchant's. Without ECPay (`buyer_entered`) there is no create: the merchant ships and
  collects through its own channel (e.g. 7-11 賣貨便 / 全家 好賣+), records the parcel with 0063, and marks
  the collection with §16.4.

### 16.4 Collected / returned (C2; spec (d))

- `collection_state`: `PENDING` → `COLLECTED` | `RETURNED` | `CANCELLED` (§16.8); `COLLECTED` → `REFUNDED_OFFLINE`;
  `RETURNED` → `RESTOCKED` (§16.8).
- **ECPay status** (inside `ingest_ecpay_status`, same tx, after the §6 transition; pay_at_pickup orders
  only): 2067 (7-11) / 3022 (others) → PICKED_UP and PENDING→COLLECTED; 2074 / 3020 → UNCLAIMED and
  PENDING→RETURNED; audit `fulfillment.collection_reported` (source `ecpay_status`, event id). A report that
  conflicts with a state already recorded by the merchant → event only + merchant alert
  `collection_conflict`.
- **Returned stock** (X8 supersedes the round-3 "allocation stays" text): the status report writes no ledger,
  payment or refund row; once the parcel is back the merchant presses restock, which releases the order's
  allocation through the single §16.8 definer. No automatic restock (the parcel may still be in transit).
- **Manual action** (spec: manual `collected`): POST `/v1/admin/stores/{store_id}/orders/{order_id}/collection`
  (§8), permission `fulfillment:write`. Definer `fulfillment.record_collection(hash bytea, store uuid, order
  uuid, key text, request_hash bytea, expected_state text, state text) RETURNS jsonb`, owner
  `commerce_checkout_writer`, EXECUTE `commerce_runtime`; authorization by `identity.resolve_access(p_hash,p_store,'fulfillment:write')` before any lock (0063:299-305 `record_manual_shipment` pattern; EXECUTE for `commerce_checkout_writer` 0062:260; `identity.merchant_access_denied` is `commerce_auth`-only, 0063:266-267), which then sets `app.tenant_id`, `app.store_id` and `app.principal_id` tx-locally from its result; command result `fulfillment.collection.record` (replay same hash → stored body, other hash →
  PT409 `idempotency_conflict`); order FOR UPDATE; not pay_at_pickup → 422 `not_pay_at_pickup`;
  `collected`/`returned` need `fulfillment_state IN ('MERCHANT_SHIPPED','PROVIDER_LABEL_CREATED')` (else 422
  `not_shipped`) and `collection_state = expected_state = 'PENDING'`; `refunded_offline` needs `COLLECTED`;
  otherwise PT409 `collection_state_changed`. Audit `fulfillment.collection_recorded` (principal, from, to).
  No money movement.
- Buyer order page: COLLECTED "已取貨付款", RETURNED "未取貨，已退回"; pending pay-at-pickup orders show
  "取貨時付款 NT$<total>".

### 16.5 Store settings (C4; spec (e))

Per store, not per ECPay profile (a `buyer_entered` store has no profile):

```sql
CREATE TABLE fulfillment.cvs_store_settings (                   -- FORCE RLS, PUBLIC revoked
 tenant_id uuid NOT NULL, store_id uuid NOT NULL,
 enabled_chains text[] NOT NULL DEFAULT '{cvs_711,cvs_familymart,cvs_hilife,cvs_okmart}'
  CHECK(enabled_chains <@ ARRAY['cvs_711','cvs_familymart','cvs_hilife','cvs_okmart']::text[]),
 pay_at_pickup_enabled boolean NOT NULL DEFAULT false,
 pay_at_pickup_max_twd integer CHECK(pay_at_pickup_max_twd BETWEEN 1 AND 20000),
 pay_at_pickup_max_open integer NOT NULL DEFAULT 20 CHECK(pay_at_pickup_max_open BETWEEN 1 AND 500),
 version bigint NOT NULL CHECK(version>0), updated_at timestamptz NOT NULL,
 PRIMARY KEY(tenant_id,store_id),
 CHECK(NOT pay_at_pickup_enabled OR pay_at_pickup_max_twd IS NOT NULL),
 FOREIGN KEY(tenant_id,store_id) REFERENCES control.stores(tenant_id,id)
);
```

No row = defaults (all four chains, still subject to §5.1 gates; pay-at-pickup off). A chain is offered
only when it is in `enabled_chains` **and** has an enabled+visible service. Writer: `fulfillment.
set_cvs_store_settings(hash bytea, store uuid, key text, request_hash bytea, expected_version bigint,
enabled_chains text[], pay_at_pickup_enabled boolean, pay_at_pickup_max_twd integer, pay_at_pickup_max_open
integer) RETURNS jsonb` (the admin PUT body carries `pay_at_pickup_max_open` too), owner
`commerce_checkout_writer`, EXECUTE `commerce_runtime`, permission `integration:manage` (R-5: no new
permission), authorization by `identity.resolve_access(p_hash,p_store,'integration:manage')` before any lock (0063:299-305 `record_manual_shipment` pattern; EXECUTE for `commerce_checkout_writer` 0062:260; `identity.merchant_access_denied` is `commerce_auth`-only, 0063:266-267), which then sets `app.tenant_id`, `app.store_id` and `app.principal_id` tx-locally from its result, version CAS (0 = insert), command result + audit
`fulfillment.cvs_settings_changed`. Read by options (via `read_cvs_offer`, §4.3), `record_buyer_cvs_store` and
`begin_hold` (FOR UPDATE, §16.2);
a change never touches placed orders. Admin UI: Settings → 物流 → "超商取貨" card (chain checkboxes,
取貨付款 toggle, 上限 NT$ 1..20000, 未取貨付款訂單上限 1..500), next to the ECPay card.

### 16.6 Gates (spec (f))

TCV14 (buyer_entered, REAL_PG + BROWSER), TCV15 (pay-at-pickup Begin: zero Stripe sessions, amount cap,
C3), TCV16 (collection status mapping + manual action), TCV17 (§16.8 cancel/restock) in §10; TCV02 adds the §4.1/§16.5 schema items
(new CHECKs keep every prior value, `principal_id` NULL only for `BUYER_ENTERED`, settings CHECKs, ledger
branch + guard, `collection_amount = goods_amount`). Each records one red run before green.

### 16.7 Known limits

- A `buyer_entered` store is format-checked only; it may not exist or may be closed. The merchant sees the
  label and verifies before shipping.
- Hi-Life store-code length UNKNOWN (3..8 digits accepted, F21).
- Returned pay-at-pickup parcels restock only when the merchant presses restock (§16.8), never automatically.
- An unshipped pay-at-pickup order keeps its allocation until the merchant cancels it (§16.8); there is no
  automatic timeout, so `pay_at_pickup_max_open` still bounds what unpaid anonymous orders lock meanwhile.
- Pay-at-pickup is capped at 20 000 TWD (F20) and the store maximum; no home-delivery COD.
- Whether the ECPay map filters stores by `IsCollection` is UNKNOWN (§16.3).

### 16.8 Pay-at-pickup cancel and restock (ruling X8; supersedes the "allocation stays" text of §16.4/§16.7)

Why: a pay-at-pickup Begin commits stock with no payment (§16.2) and `checkout.expire_held` never releases a
non-DRAFT order (0013:569), so without an exit an anonymous junk order holds its stock and one
`pay_at_pickup_max_open` slot forever. Evidence: DESIGN; TCV17 NOT_RUN.

- **Route** (merchant API, main pool = `commerce_runtime`, cmd/api/main.go:65): POST
  `/v1/admin/stores/{store_id}/orders/{order_id}/pay-at-pickup-release` (§8), permission `fulfillment:write`,
  authorization by `identity.resolve_access(p_hash,p_store,'fulfillment:write')` before any lock (0063:299-305 `record_manual_shipment` pattern; EXECUTE for `commerce_checkout_writer` 0062:260; `identity.merchant_access_denied` is `commerce_auth`-only, 0063:266-267), which then sets `app.tenant_id`, `app.store_id` and `app.principal_id` tx-locally from its result, `Idempotency-Key`, body exactly
  `{action, expected_state}`, `action IN ('cancel','restock')`.
  - `cancel`: order `payment_mode='pay_at_pickup'`, `commercial_state='CONFIRMED'`, `collection_state='PENDING'`,
    `fulfillment_state='MANUAL_UNASSIGNED'` (0063 MERCHANT_SHIPPED or PROVIDER_LABEL_CREATED → 422
    `not_cancellable`), and not handed to a provider: after `fulfillment.settle_cvs_attempt`, every
    `fulfillment.cvs_shipments` row of the order is `FAILED` or `ABANDONED` (REQUESTED/UNKNOWN may already be at
    ECPay → 409 `cvs_attempt_in_flight`; CREATED or later → 422 `not_cancellable`). ABANDONED counts as not handed
    over because §6 reaches it only after a fresh MAC-verified created-only query or the `i_checked_ecpay_backend`
    acknowledgement (deviation from X8's literal text, recorded in §14.5); a later status or finish for such an
    attempt on a CANCELLED order → event + `duplicate_label_risk`, no state or ledger change. Writes
    `commercial_state='CANCELLED'`, `fulfillment_state='CANCELLED'` (the expiry pair, 0013:598) and
    `collection_state='CANCELLED'`. A cancelled order is no longer `order_money_shippable` (needs CONFIRMED), so
    `request_cvs_shipment` and dispatch-time `cvs_order_payable` refuse it.
  - `restock`: `collection_state='RETURNED'` (ECPay 2074/3020 or the merchant's §16.4 `returned`), pressed once
    the parcel is back, and no `fulfillment.cvs_shipments` row of the order is in CREATED/AT_DC/AT_STORE (the latest
    live shipment, if any, is UNCLAIMED; a §6 2098 re-delivery puts it back to AT_STORE), else 409
    `parcel_not_returned`. Writes `collection_state='RESTOCKED'`; commercial/fulfillment states and the shipment
    history stay (so `guard_cvs_shipment_state` and 0063's "MERCHANT_SHIPPED only on CONFIRMED" hold).
  - `expected_state` must equal the current `collection_state` (`PENDING` for cancel, `RETURNED` for restock),
    else PT409 `collection_state_changed`. A card order → 422 `not_pay_at_pickup`: card orders keep the
    no-cancel rule (stripe-refund-v1 RD6).
- **One audited stock writer:** `inventory.release_pay_at_pickup(hash bytea, store uuid, order uuid, key text,
  request_hash bytea, action text, expected_state text) RETURNS jsonb`, SECURITY DEFINER, `search_path=pg_catalog`,
  PUBLIC revoked, owner `commerce_checkout_writer`, EXECUTE `commerce_runtime` only, `COMMENT ON` per PROCESS §5.
  It is the only writer of `DEALLOCATE` ledger rows. One tx: resolve_access + tenant/store/principal GUCs
  (Route bullet) → order FOR UPDATE → `app.buyer_id`/`app.buyer_session_id` set from the order row (0063:330
  pattern) → replay check (`ops.command_results` operation `inventory.pay_at_pickup.release`: same hash → stored
  body; other hash → PT409 `idempotency_conflict`; the SELECT policy needs the tenant/store GUCs, 0062:270-272) →
  guards above → order update → reservation (`id` = order id) FOR UPDATE,
  must be `COMMITTED` (else PT409 `collection_state_changed`) → `RELEASED` → per reservation line, sorted by
  (warehouse, sku), after `inventory.lock_balance`, one ledger row `kind='DEALLOCATE'`, `delta_allocated =
  -quantity`, `delta_on_hand = delta_reserved = 0`, `actor_kind='MERCHANT'`, `principal_id` = the member,
  `checkout_id = reservation_id = order id`, buyer owner/session from the order, `operation =
  'fulfillment.pay_at_pickup.cancel'|'fulfillment.pay_at_pickup.restock'`, `command_key = order id::text`,
  `reason` = the from-state (`PENDING`|`RETURNED`) → audit `fulfillment.pay_at_pickup_cancelled` /
  `fulfillment.pay_at_pickup_restocked` (principal, order, from, to, line count) → command result. §11.5
  evidence = order id + collection_state + actor, all on the ledger row. Result `{order_id, collection_state,
  commercial_state, released_lines}`. No `payments.*`, refund, `integration.operations` or River row.
- **Why releasing the allocation is the whole restock:** shipping never decrements `on_hand` or `allocated`
  (0063 writes no ledger row; `allocated` only grows through ALLOCATE, 0018:163), so moving the order's units
  from allocated back to available is the restock. A damaged parcel is then written off with the existing
  audited ADJUST.
- **Schema (0072, each constraint re-derived via `pg_get_constraintdef`):** `ledger_kind_check` + `DEALLOCATE`;
  `ledger_check` branch `kind='DEALLOCATE' AND delta_on_hand=0 AND delta_reserved=0 AND delta_allocated<0 AND
  delta_unavailable=0 AND reservation_id IS NOT NULL`; `ledger_checkout_actor` branch `actor_kind='MERCHANT' AND
  kind='DEALLOCATE' AND principal_id IS NOT NULL AND checkout_id IS NOT NULL AND buyer_owner_id IS NOT NULL AND
  buyer_session_id IS NOT NULL AND reservation_id=checkout_id AND payment_attempt_id IS NULL AND payment_fact_kind
  IS NULL AND operation IN ('fulfillment.pay_at_pickup.cancel','fulfillment.pay_at_pickup.restock') AND
  command_key=checkout_id::text` (the existing MERCHANT branch keeps `checkout_id IS NULL`, and
  `ledger_merchant_fence`, 0013:121, keeps `commerce_runtime` off checkout rows); unique index
  `ledger_pay_at_pickup_release_once ON inventory.ledger(tenant_id,store_id,checkout_id,warehouse_id,sku_id)
  WHERE kind='DEALLOCATE'`; `inventory.guard_pay_at_pickup_ledger()` (§16.2) extended: a DEALLOCATE row requires
  its order `pay_at_pickup` with `collection_state='CANCELLED'` (cancel operation) or `'RESTOCKED'` (restock
  operation), its reservation `RELEASED`, a BUYER ALLOCATE row for the same line, and `-delta_allocated` = the
  reservation line quantity, else 42501. Policy `checkout_writer_pay_at_pickup_release` FOR INSERT ON
  `inventory.ledger` TO `commerce_checkout_writer` WITH CHECK(tenant/store GUCs AND `actor_kind='MERCHANT'` AND
  `kind='DEALLOCATE'` AND `principal_id IS NOT NULL` AND buyer owner/session = the `app.buyer_*` GUCs) — the
  existing `checkout_writer_ledger` admits no MERCHANT row. `inventory.guard_checkout_ledger()` (0013:129, BEFORE
  INSERT; CREATE OR REPLACE re-derived from 0013, same owner `commerce_inventory_writer`, PUBLIC revoked) gains one
  branch, checked before the existing MERCHANT branch: `IF NEW.actor_kind='MERCHANT' AND NEW.kind='DEALLOCATE' THEN`
  require `NEW.principal_id = nullif(current_setting('app.principal_id',true),'')::uuid AND NEW.checkout_id =
  NEW.reservation_id AND NEW.buyer_owner_id = nullif(current_setting('app.buyer_id',true),'')::uuid AND
  NEW.buyer_session_id = nullif(current_setting('app.buyer_session_id',true),'')::uuid`, else 42501. Every other
  MERCHANT row keeps the 0013 rule `checkout_id/buyer_owner_id/buyer_session_id IS NULL`; the reservation-match
  block is unchanged. (Without this branch 0013:134-137 refuses every DEALLOCATE row.) The reservation update uses the existing
  `checkout_writer_order_stock` policy (0013:205) and UPDATE(state) grant (0013:234); order columns use 0013:175
  plus the §4.3 round-4 `UPDATE(collection_state)`. TCV02 adds all of these to the matrix.
- **Slots:** the §16.2 open-order count only matches `collection_state='PENDING'`, so a cancel frees its slot.
- **Later reports:** an ECPay report on a RESTOCKED order is "a state already recorded by the merchant" →
  event + `collection_conflict` alert (§16.4), never a state change. A cancelled order has no live shipment.
- **UI:** admin order detail "取消訂單並釋放庫存" (PENDING, unshipped; confirm dialog) and "已收回包裹，恢復庫存"
  (RETURNED); merchant projections carry `collection_state`; buyer order page CANCELLED "賣家已取消訂單",
  RESTOCKED "未取貨，已退回".
- **Gate:** TCV17 (§10).
