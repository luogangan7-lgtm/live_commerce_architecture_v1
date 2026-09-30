# Unit cvs-core — 0072/0073 + post-River 0017, CVS selection/shipment/pay-at-pickup Go, merchant + buyer HTTP

Role: commerce_worker (mid tier). Base SHA `00c1d94`, SHA recorded at dispatch. Worktree `.worktrees/cvs-core`,
branch `unit/cvs-core`. No delegation, no network, no ECPay key. **Wave 1**, parallel with cvs-ecpay and cvs-ui
against the FROZEN blocks here and in `cvs-ecpay.md`; Go that calls `ecpay.*` compiles after cvs-ecpay **F1e**
merges (do SQL first). Contract: `contracts/taiwan-cvs-logistics-v1.md` (v1 FROZEN 2026-09-30) incl. §14–§16
and `r2-design-rulings.md` C1–C4, X1, X5, X6, X8, X9. Write-set boundary as in `cvs-ecpay.md` header; never
touch 0070/0071/0074–0080, `post_river/0015–0016`, `internal/{ads,mail,customers,billing,integrations/core}`.

**Goal:** buyer picks a CVS store (ECPay map + directory verification, or buyer-entered), pays by card or
pay-at-pickup; merchant connects ECPay, sets chains/pay-at-pickup, requests one label per order through the
ledger/dispatcher, prints, abandons, records collection, cancels/restocks pay-at-pickup stock — all through
SECURITY DEFINER SQL with the §4.3 grant matrix.

## Read (by section; `grep -n` headings, `sed -n` ranges)
PROCESS.md; contract §0 (TD1–TD8, R-1..R-7), §3, §4 (all), §5, §6, §8, §9, §12, §16 (all), §10 rows (what
cvs-tests asserts). SQL by symbol: `0063_manual_fulfilment.sql` (`manual_shipment_eligible` :211-240,
`record_manual_shipment` :299-305 auth pattern, `export_unshipped_orders` :491, `read_merchant_orders` :543,
GUCs :330), `0013_*` (`guard_checkout_ledger` :129, writer grants :175/:205/:234, `expire_held` :569/:598),
`0061_stripe_psp.sql` (`ledger_checkout_actor` :142, events :188, `guard_payment_ledger` :167, identity index),
`0062_stripe_refund.sql` (`resolve_access` :260, command_results policies :263-272), `0064` :861-864 (job check),
`0008` :106-118 (`worker_binding_lock`, `claim_operation`), `0012` :82-98 (pickup grants), `post_river/0005:98`
(`begin_hold` 8-arg), `post_river/0014` (`external_operation_job_commit`), `migrate.go:120-190`.
Go: `internal/checkout/{checkout.go (Begin :103, begin_hold call :211, Get :233), options.go}`,
`internal/fulfillment/{service.go:279-293, pickup.go (AttestPickup lock order, :232)}`,
`internal/storefront/destination.go:313`, `internal/merchantorders/{orders.go (:248, :286, :333, :427-430), export.go}`,
`internal/httpapi/{shipments.go, refunds.go}` (route + classifier + `scopedAs` pattern),
`internal/buyerhttp/handler.go` (`matchRoute`, `allowed`, `keyFor`, `dispatch`; read only),
`internal/integrations/core/jobs.go` (`InsertOperationJob`), `cmd/api/{accounts.go:53-75 (insert-only River
client), buyer.go, buyer_payment.go:44}` (read only).

## Defaults adopted (P2s the frozen text left open; integrator may override)
- C1 R-6 post-River file = `migrations/post_river/0017_taiwan_cvs_begin_hold.sql` (0015 ads, 0016 stripe-live):
  DROP 8-arg `checkout.begin_hold`, CREATE the 10-arg one, same owner/grants (0013:611).
- C2 Trade no = `'LC' || rtrim(encode-base32(sha256(convert_to(p_operation::text,'UTF8'))))[:18]` (RFC 4648,
  upper, no padding; SQL helper `fulfillment.ecpay_trade_no(uuid)` IMMUTABLE); Go twin `ecpay.MerchantTradeNo`.
- C3 `load_cvs_create` mode = caller's `SecretClaim.Mode` and must equal `operations.lease_mode`; policy
  refusals RAISE `PT409`, lease-fence failures `40001` (cvs-ecpay E4).
- C4 Merchant projections: 0073 CREATE OR REPLACE `identity.read_merchant_orders` and
  `identity.export_unshipped_orders`, re-derived from `pg_get_functiondef` of the live definition; the only body
  delta = summary/detail keys `pickup_source` (`ecpay_directory|buyer_entered|merchant_attested`), `payment_mode`,
  `collection_state`; list filter `cvs_pending` (PROVIDER_LABEL_CREATED ∧ current attempt CREATED); export
  appends one last CSV column `pickup_source`. `fulfillment_state` admits `PROVIDER_LABEL_CREATED` everywhere Go
  validates it (merchantorders :286/:333).
- C5 `fulfillment.ecpay_validity_days(subtype)` (UNIMART*=5, FAMI*=6, HILIFE*/OKMARTC2C=7, F9) and
  `fulfillment.ecpay_created_only_codes()` = `{'300'}` (F19) as IMMUTABLE SQL functions (evidence-only edits).
- C6 "Merchant alert" = `cvs_shipment_events` row `event_code='alert.duplicate_label_risk'|'alert.collection_conflict'`
  (+ audit row); GET `cvs-shipment` exposes `alerts` (distinct codes). A collection conflict on an order with no
  shipment row → audit only.
- C7 Command-result operations: `fulfillment.cvs_selection.open`, `fulfillment.cvs_store.enter`,
  `fulfillment.cvs_shipment.request`, `fulfillment.cvs_shipment.abandon`, `fulfillment.collection.record`,
  `fulfillment.cvs_settings.set`, `inventory.pay_at_pickup.release`, `integration.ecpay_logistics.register`,
  `integration.ecpay_logistics.enabled`. SQLSTATE → HTTP: PT403 403, PT409 409, PT422 422, PT429 429 (+Retry-After
  60), PT2RP → `read_cvs_shipment_command` replay, 22023 → 400; message = the §-named code.
- C8 Settings GET without a row → `{version:0, <§16.5 defaults>}`; PUT `expected_version=0` inserts.
- C9 Deployment payment environment: `buildCVS` receives the already-validated `COMMERCE_PAYMENT_PROFILE`
  (integrator passes it; `PROVIDER_MOCK`→`SANDBOX`); empty (buyer payment off) ⇒ `""`, which every CVS definer
  refuses for ECPay paths while MANUAL/buyer-entered keep working. `CVS_ECPAY_ENABLED=1` with `""` = startup error.
- C10 Map-return inline verify uses `ecpay.Client.CachedStore` only; a miss leaves RETURNED for the buyer's `verify`.
- C11 `print-form` body exactly `{thermal}`; `abandon` body exactly `{expected_version, i_checked_ecpay_backend}`;
  `collection` `state ∈ {collected, returned, refunded_offline}`.

## FROZEN interface (cvs-ui, cvs-tests and the integrator call exactly these)
```go
package fulfillment // internal/fulfillment/cvs*.go (main pool = commerce_runtime)
type CVSConfig struct{ ECPay ecpay.Config; PaymentEnvironment string }        // "SANDBOX"|"LIVE"|""
type CVS struct{ /* unexported */ }
func NewCVS(pool *pgxpool.Pool, jobs *river.Client[pgx.Tx], keys *ecpay.Keyring, client *ecpay.Client, cfg CVSConfig) (*CVS, error) // jobs: insert-only, Schema "river"
func (c *CVS) HooksHandler() http.Handler // POST /v1/cvs/ecpay/map-return/{selection_id}, /v1/cvs/ecpay/status/{endpoint_id}; 404 when !cfg.ECPay.Enabled
func ValidBuyerStoreCode(kind, code string) bool                               // §16.1 table; twin of the SQL check

package checkout // checkout pool = commerce_checkout_runtime
// Input gains PaymentMode string `json:"payment_mode"` ("" ≡ "card"); Result gains PaymentMode, CommercialState.
// Order (Get) gains PaymentMode string `json:"payment_mode"`, CollectionState *string `json:"collection_state"`,
//   CVSShipment *BuyerCVSShipment `json:"cvs_shipment"`.
type BuyerCVSShipment struct{ State, Chain, StoreName, StoreCode string; UpdatedAt time.Time } // json snake_case
func (s *Service) WithPaymentEnvironment(env string) *Service                  // copy; Begin passes it as p_payment_environment
type BuyerCVS struct{ /* unexported */ }
func NewBuyerCVS(checkoutPool *pgxpool.Pool, keys *ecpay.Keyring, client *ecpay.Client, cfg fulfillment.CVSConfig) (*BuyerCVS, error)

package httpapi   // internal/httpapi/cvs.go
func registerCVSRoutes(mux *http.ServeMux, pool *pgxpool.Pool, cvs *fulfillment.CVS) // §8 table; integrator mounts iff non-nil
package buyerhttp // internal/buyerhttp/cvs.go — same shape as paymentRequest (payment.go:95); integrator adds the routeKinds
const ( routeCVSSelectionOpen routeKind = 200 + iota; routeCVSSelectionGet; routeCVSSelectionVerify; routeCVSStoreEnter ) // in cvs.go; clear of handler.go's iota block
func (h *handler) cvsRequest(ctx context.Context, r *http.Request, selected route, storeID, token, key string) (any, error)
package main      // cmd/api/cvs.go
type cvsParts struct{ Merchant *fulfillment.CVS; Buyer *checkout.BuyerCVS; Hooks http.Handler; PaymentEnvironment string }
func buildCVS(ctx context.Context, getenv func(string) string, pool, checkoutPool *pgxpool.Pool, paymentProfile string) (cvsParts, error)
```
(`cvsRequest` switches on the route kind; the integrator's hook only adds `matchRoute`/`allowed`/`keyFor`/`dispatch` cases.)

FROZEN JSON (snake_case, exact keys; absent ≠ null only where stated):
- Options CVS row adds `pickup_selection: "ecpay_map"|"buyer_entered"`, `payment_modes: ["card"(,"pay_at_pickup")]`,
  `store_search_url` (buyer_entered only), and unavailable rows `{available:false, reason:"coming_soon"|"temporarily_unavailable"}`.
- `POST /v1/buyer/cvs-selections` 201 `{selection_id, expires_at, form:{action, fields}}`; GET / verify 200
  `{selection_id, state, reject_code, retry_after_s, pickup}` with `pickup = null | {pickup_id, kind, code, name, address, outside}`.
- `POST /v1/buyer/cvs-stores` 201 `{pickup_id, kind, code, name, address, source:"buyer_entered"}`.
- `GET …/logistics/ecpay` and PUT 200: `{environment, mode, merchant_id, version, enabled, qualified_at, ok_verified, status_url}`.
- `GET|PUT …/logistics/cvs-settings`: `{version, enabled_chains, pay_at_pickup_enabled, pay_at_pickup_max_twd, pay_at_pickup_max_open}`.
- `GET …/orders/{id}/cvs-shipment` 200 `{current_attempt, expected_version, validity_days, attempts:[{attempt, state,
  subtype, environment, receiver_store_id, goods_amount, collection_amount, provider_logistics_id, code, print_available,
  result_code, last_status_code, last_status_at, alerts, created_at, updated_at, version, events:[{source, event_code,
  provider_code, provider_message, from_state, to_state, received_at}]}]}` (no PII, no trade no).
- `POST …/cvs-shipment` 202 `{attempt, state:"REQUESTED", operation_id}`; `print-form` 200 `{action, fields}`;
  `abandon` 200 = the GET projection; `collection` 200 `{order_id, collection_state}`; `pay-at-pickup-release` 200
  `{order_id, collection_state, commercial_state, released_lines}`.
- Merchant order summary/detail: + `pickup_source`, `payment_mode`, `collection_state` (C4).

## Build
1. **SQL 0072** §4.1, §4.2, §16.5 tables, §16.8 schema; every widened CHECK re-derived via `pg_get_constraintdef`
   and only extended; `COMMENT ON` every object (owner package, roles, non-goals). **0073** every §4.3/§16
   definer + C2/C5 helpers + the grant/policy matrix exactly (incl. X9 pool note, round-4 grants,
   `buyer_entered_own`, `cvs_create_insert`, `checkout_cvs_operation_read`); `post_river/0017` (C1).
   Hand 0072/0073/0017 to the integrator first = **F1** (unblocks cvs-ecpay step 3 and cvs-tests PG runs).
2. **Allowlists** (step 2 files only): T06 approved `integration` functions +8 (`register_ecpay_logistics`,
   `set_ecpay_logistics_enabled`, `plan_cvs_create`, `load_cvs_create`, `finish_cvs_create`, three
   `load_ecpay_key_for_*`), MF06 export header (+`pickup_source`), merchant-order key goldens (C4). Change existing
   assertions only to frozen values; list every before/after; never loosen a negative. Hand with F1.
3. **Go widening:** kinds `cvs_hilife`/`cvs_okmart` and verification kinds in `fulfillment/{service,pickup}.go`,
   `storefront/destination.go`, `merchantorders/orders.go`; `PROVIDER_LABEL_CREATED`; projection keys (C4).
4. **Checkout:** `options.go` via `read_cvs_offer` only; Begin `payment_mode` + `p_payment_environment`;
   `Get` adds `cvs_shipment` via `read_buyer_cvs_shipment`; `checkout/cvs.go` BuyerCVS (open → form with
   crypto/rand 20-char trade no, only its sha256 stored; verify with directory lookup outside any tx; buyer-entered).
5. **Merchant** `fulfillment/cvs.go`: connect (C9 env check before probe; `ecpay.Probe` then `Seal` then definer;
   keys never returned/logged), enable, settings, request (generate UUIDv4 → `core.InsertOperationJob` → definer in
   one READ COMMITTED tx; PT2RP ⇒ rollback + `read_cvs_shipment_command`), read, print-form (merchant key loader,
   audit), abandon (Query/V5 first, pass `query_status/query_at`), collection, release; `httpapi/cvs.go` strict
   bodies, Idempotency-Key where §8 says, no query strings.
6. **Hooks** (`HooksHandler`): map-return §5.2 (≤8 KiB, 303 to stored origin+path, unknown → 404 no Location);
   status §7.5 (≤16 KiB, 5 s body timeout, per-endpoint concurrency 4, MAC over all fields, `1|OK` only after
   COMMIT, `retry` → 503). No body echo, no PII in logs/URLs.

## Write paths
`migrations/{0072_taiwan_cvs_logistics.sql,0073_taiwan_cvs_functions.sql}`, `migrations/post_river/0017_taiwan_cvs_begin_hold.sql`,
`internal/fulfillment/{cvs*.go,service.go,pickup.go,*_test.go for those}`, `internal/storefront/{destination.go,destination_test.go}`,
`internal/checkout/{checkout.go,options.go,cvs.go,cvs_test.go,checkout_test.go,options_test.go}`,
`internal/merchantorders/{orders.go,export.go,orders_test.go,export_test.go}`, `internal/httpapi/{cvs.go,cvs_test.go}`,
`internal/buyerhttp/{cvs.go,cvs_test.go}`, `cmd/api/{cvs.go,cvs_test.go}`,
`tests/foundation/{external_operation_authority,manual_fulfilment_schema,manual_fulfilment_http,manual_fulfilment_env}_test.go` (step 2 only:
frozen values, rows merged by the integrator with ads-core/customers-core), `output/cvs-core/**`.
Forbidden: `internal/httpapi/handler.go`, `internal/buyerhttp/handler.go`, `cmd/api/{main,buyer,buyer_payment}.go`,
`internal/merchantorders/refunds.go`, `internal/checkout/hosted.go`, `internal/integrations/**`, `internal/platform/**`,
other `tests/**`, `apps/**`, contracts, `core-openapi.json`, deploy, go.mod/go.sum.

## PROCESS §5 (every file you touch)
`doc.go`/package comment updated (owns / never); cross-domain SQL call sites name the definer + why
(`// inventory.release_pay_at_pickup: single DEALLOCATE writer (§16.8)`); money comparisons `// I05:`
(TWD ×100, server quote only); stock transitions `// §11.5:` + evidence (order id + collection_state + actor);
every UNKNOWN/retry branch says why no retry; every new SQL object `COMMENT ON`. Run `scripts/dev/depmap.sh`.

## Verify
```sh
GOTOOLCHAIN=go1.27.1 go vet ./... && gofmt -l internal cmd
GOTOOLCHAIN=go1.27.1 go test -race -count=1 ./internal/fulfillment/... ./internal/checkout ./internal/storefront ./internal/merchantorders ./internal/httpapi ./internal/buyerhttp ./cmd/api
LC_FOCUSED_TIMEOUT=2400s bash scripts/dev/test-focused.sh '^Test(T0[46]|ManualFulfilmentMF0[2-68]|StripeRF0[3-9]|StripeSP|BuyerPayment|MerchantOrders|Checkout|Migration|OwnerProvisioning|Pool)'
python3 scripts/check_packet.py && bash scripts/dev/depmap.sh
```
Logs → `/Volumes/data/live_commerce_architecture_v1/output/cvs-core/` (command, SHA, exit, PASS/FAIL/SKIP).

## Gates
Implementer: Go unit tests for pure logic (store-code table, recipient mirror, HTTP body strictness, error
mapping), each with one red run; regression suites above green. Independent (do not claim): TCV02–TCV06,
TCV11, TCV14–TCV17 REAL_PG/HTTP_PG, TCV08 BROWSER, TCV09 regression — cvs-tests; TCV09 verdicts —
security_reviewer + integrator.

## Integrator hooks (integrator only; unit ships `output/cvs-core/integrator-hooks.patch`)
- `internal/httpapi/handler.go`: `Options.CVS *fulfillment.CVS`; `registerCVSRoutes(mux, pool, configured.CVS)` after `registerShipmentRoutes`.
- `internal/buyerhttp/handler.go`: routeKinds + `matchRoute` for `cvs-selections` (POST, key), `cvs-selections/{uuid}`
  (GET), `cvs-selections/{uuid}/verify` (POST, keyless, no body), `cvs-stores` (POST, key); `New` gains the
  `*checkout.BuyerCVS`. customers-core also edits this file — integrator merges both.
- `cmd/api/main.go` + `cmd/api/buyer.go`: `buildCVS(ctx, getenv, pool, checkoutPool, buyerPaymentConfig.profile)`;
  `mux.Handle("/v1/cvs/ecpay/", parts.Hooks)`; `checkoutService.WithPaymentEnvironment(parts.PaymentEnvironment)`.
- `deploy/caddy/Caddyfile` (two hooks-host `handle`s, §12), `deploy/compose.yml` + `deploy/env/api.env.example`
  (`COMMERCE_CVS_HOOKS_ORIGIN`, `CVS_ECPAY_ENABLED`, `CVS_ECPAY_LIVE_CREATE`, keyring), `contracts/core-openapi.json`
  §5.2/§8/§16 paths, `contracts/tasks.json` T13 evidence, `docs/engineering/dependency-map.md` regen.
- Merge the step-2 test rows with ads-core (F1c) and customers-core rows; review every before/after.

## Order / NOT_RUN / Return
F1 (SQL) → Go against F1e → full merge F2 with cvs-ecpay → integrator mounts. NOT_RUN: all SANDBOX/LIVE, TCV08.
Non-goals: bulk create/print, B2C frozen, store change, 7-11 cancel API, home COD, buyer cancel, auto restock.
Return commit SHAs (F1, full), model/reasoning, base, paths, commands + exits + counts, evidence, C1–C11 as
applied, every before/after of changed assertions, risks, NOT_RUN. Any deviation from frozen signatures = stop.
