# Unit customers-core — 0078 customers/privacy SQL, consent gate, customers + finance Go, merchant + buyer routes

Role: commerce_worker (mid tier). Base `00c1d94`. Worktree `.worktrees/customers-core`, branch
`unit/customers-core`. No delegation, no network, no Stripe/Meta key. Parallel with `billing-core` (disjoint
paths; 0079 needs this unit's 0078 merged = F1 before its PG runs). Contract: `contracts/customers-billing-v1.md`
(FROZEN 2026-09-30) incl. §0.1 and §12; `r2-design-rulings.md` X4/X6. The "Defaults adopted" below bind unless the
integrator overrides.

**Goal:** T14 — customer list/detail as a read-time projection over `buyer.owners`, append-only consent with the one
gate `customers.consent_allows` (R3 CAPI consumes it), synchronous export/erasure with tombstone replay, and the
BD7 finance summary + CSV. No identity merge, no marketing send, no billing (billing-core).

## Read (by section; grep headings, `sed -n` ranges)
PROCESS.md; contract §0 (CD1–CD8 only), §0.1 C-1/C-2/C-3/C-6/C-7, §2, §3.1, §4 (consent/privacy), §5 (merchant
customers/finance rows + Buyer paragraph + export content), §6 rows 1–2, §7, §8 CB01–CB05/CB07/CB09 (what the gates
will assert), §9. By symbol: `0063_manual_fulfilment.sql` (`read_merchant_orders` GUC check ~525-528, export audit
pattern, `pg_get_constraintdef` re-derivation), `0013_*.sql` buyer definer `set_config` (~334-338),
`0006_buyer_capability.sql` (`buyer.owners`, `capability_sessions`, `capability_events`), `0060_live_claims.sql`
(`claims.bundles` ~94-115, label CHECK :99, UNIQUE :112, `claims.lines`), `0061_stripe_psp.sql`
(`payments.stripe_sessions` expiry CHECK), `0062_stripe_refund.sql` (`stripe_refunds`, `refund_facts`),
`0065_owner_provisioning.sql`; Go: `internal/merchantorders/orders.go` (`List`, `Get`, `Summary`, `Detail`, `mapError`),
`internal/httpapi/{handler.go (scopedAs, listRoute, bodyRoute), refunds.go, shipments.go}` (route + classifier
pattern, CSV export headers), `internal/buyerhttp/handler.go` (`matchRoute`, `allowed`, `keyFor`, `dispatch`,
`scoped`, `responseError`, `classify`), `internal/buyer/buyer.go` (`WithScope`, `Scope`, token sha256),
`internal/pagination`.

## Defaults adopted (P2s the frozen text left open)
- D1 C-1: `customer_id` (= owner id) only in the new customer endpoints; merchant order summaries unchanged.
- D2 C-2: privacy + consent in `internal/customers`; finance in `internal/reporting` (§9). C-6: one writer role.
- D3 C-3: PT410 → 410 `erased`, PT409 `erasure_blocked`/`idempotency_conflict` → 409 with that code.
- D4 `PrivacyPolicyVersion = "lc-2026-10"` is a **Go constant** (Q8), not env: the notice text and its version ship
  together; the storefront notice (UI unit) carries the same literal. Changing the notice = code change in both.
- D5 List paging: query `after` carries the opaque cursor returned as `next_cursor` (repo `pagination` encoding of
  `(last_activity_at,id)`); `limit` default 50, 1..100 (SQL `p_limit` = limit+1 probe, ≤ 101).
- D6 `q`: trimmed 1..40 chars else 422; if it is digits after removing spaces, `-`, `+` → phone-digits suffix over
  order destinations; else case-insensitive `recipient_name` prefix.
- D7 Row derivation: `first_seen_at` = `owners.created_at`; `last_activity_at` = greatest(latest order `created_at`,
  latest bundle `bound_at`); `display_name`/`phone_last3` from the latest order's destination (null for bundle-only
  customers); `currency` = latest order's currency and money sums cover only that currency (single-currency stores
  in v1; mixed-currency customers are a known limit); `platforms[]` = distinct bound bundle platforms, sorted.
- D8 Export doc `{"format":"lc.customer-export.v1","generated_at","store":{"name"},"customer_id","orders":[Detail],
  "consents":[ConsentEvent],"claims":[ClaimSummary],"privacy_actions":[PrivacyAction]}`; >200 orders or >1 MiB after
  marshal → 409 `export_too_large` and the tx rolls back (no EXPORT row). Buyer export: same envelope with the
  existing buyer order-history detail per order and no `customer_id`/principal ids.
- D9 Attachments: `Content-Type: application/json` / `text/csv; charset=utf-8`, `Content-Disposition: attachment;
  filename="customer-<id>.json"` / `"finance-<from>-<to>.csv"`, `Cache-Control: no-store`.
- D10 Erasure summary (≤ 1024 bytes, counts only): `{"consents_withdrawn","sessions_revoked","snapshots_redacted",
  "bundles_relabelled"}`.
- D11 Buyer **erasure** does not use `buyer.WithScope` (a revoked capability must reach `erase_owner` to get PT410):
  it opens a buyer-pool tx and calls the definer with the token hash. Buyer GET/PUT/export use `buyer.WithScope`.
- D12 Consent PUT body strict `{purpose,channel,granted,context}`; `context` checkout→`buyer_checkout`,
  settings→`buyer_settings`; any other key (incl. `source`, `policy_version`) → 422. Response 200
  `{purpose,channel,granted,occurred_at}`; merchant withdrawal 201 same shape.
- D13 Finance: `from`/`to` required `YYYY-MM-DD`, `to-from` 0..91 else 422; CSV columns
  `day,currency,environment,captured_count,captured_minor,refunded_minor,net_minor` (integers, minor units), header row.
- D14 If `read_merchant_customers` needs a read `commerce_auth` does not hold (e.g. `claims.lines` count), add the
  column grant + policy in 0078 and list it in the return (CB02 asserts the exact set).

## FROZEN interface (billing-core, meta-ads R3, UI and tests call exactly these)
SQL (0078; R3 `0080_meta_capi.sql` grants EXECUTE to `commerce_ads_writer` — never in 0078, C-7):
```sql
customers.consent_allows(p_tenant uuid, p_store uuid, p_owner uuid, p_purpose text, p_channel text) RETURNS boolean
  -- STABLE SECURITY DEFINER SET search_path=pg_catalog; owner commerce_privacy_writer; EXECUTE commerce_auth only.
  -- true iff the latest (occurred_at DESC, id DESC) event for (tenant,store,owner,purpose,channel) is granted AND
  -- owners.active. Unknown owner / other store / invalid pair → false, never an error. Reads by its own arguments
  -- (policy USING(true) restricted to the owner role), so it works from any GUC state (R3 sweeper, dispatcher Check).
-- Valid pairs only: ('marketing_messages','meta_dm'), ('ads_personalization','meta_ads'). CAPI uses the second.
```
```go
package customers // internal/customers
const PrivacyPolicyVersion = "lc-2026-10"
const PurposeMarketingMessages, ChannelMetaDM, PurposeAdsPersonalization, ChannelMetaAds = "marketing_messages", "meta_dm", "ads_personalization", "meta_ads"
const ExportFormat = "lc.customer-export.v1"; const MaxExportBytes, MaxExportOrders = 1 << 20, 200
type Consents struct{ MarketingMessages bool `json:"marketing_messages"`; AdsPersonalization bool `json:"ads_personalization"` }
type Customer struct{ CustomerID string `json:"customer_id"`; FirstSeenAt string `json:"first_seen_at"`; LastActivityAt string `json:"last_activity_at"`
    DisplayName *string `json:"display_name"`; PhoneLast3 *string `json:"phone_last3"`; OrdersCount int64 `json:"orders_count"`; PaidOrdersCount int64 `json:"paid_orders_count"`
    CapturedMinor int64 `json:"captured_minor"`; RefundedMinor int64 `json:"refunded_minor"`; Currency *string `json:"currency"`; ClaimsCount int64 `json:"claims_count"`
    Platforms []string `json:"platforms"`; Consents Consents `json:"consents"`; Active bool `json:"active"` }
type ClaimSummary struct{ SessionID string `json:"session_id"`; Platform string `json:"platform"`; BoundAt string `json:"bound_at"`; LineCount int64 `json:"line_count"` }
type ConsentEvent struct{ Purpose string `json:"purpose"`; Channel string `json:"channel"`; Granted bool `json:"granted"`; Source string `json:"source"`
    PolicyVersion string `json:"policy_version"`; OccurredAt string `json:"occurred_at"` }
type PrivacyAction struct{ Kind string `json:"kind"`; Via string `json:"via"`; CompletedAt string `json:"completed_at"`; Summary json.RawMessage `json:"summary"` }
type Detail struct{ Customer; Orders []merchantorders.Summary `json:"orders"`; Claims []ClaimSummary `json:"claims"`
    ConsentHistory []ConsentEvent `json:"consent_history"`; PrivacyActions []PrivacyAction `json:"privacy_actions"` }
type ListRequest struct{ Page pagination.Request; Q string }
type ConsentInput struct{ Purpose string `json:"purpose"`; Channel string `json:"channel"`; Granted bool `json:"granted"`; Context string `json:"context"` }
type WithdrawInput struct{ Purpose string `json:"purpose"`; Channel string `json:"channel"` }
type ConsentResult struct{ Purpose string `json:"purpose"`; Channel string `json:"channel"`; Granted bool `json:"granted"`; OccurredAt string `json:"occurred_at"` }
type BuyerPrivacy struct{ Consents Consents `json:"consents"`; Erased bool `json:"erased"` }
type ErasureSummary struct{ ConsentsWithdrawn int64 `json:"consents_withdrawn"`; SessionsRevoked int64 `json:"sessions_revoked"`
    SnapshotsRedacted int64 `json:"snapshots_redacted"`; BundlesRelabelled int64 `json:"bundles_relabelled"` }
func CurrentConsents(history []ConsentEvent) Consents // pure CD4 derivation (CB01); Detail uses it
func List(ctx context.Context, tx pgx.Tx, scope platform.Scope, token string, in ListRequest) (pagination.Page[Customer], error)
func Get(ctx context.Context, tx pgx.Tx, scope platform.Scope, token, customerID string) (Detail, error)
func WithdrawConsent(ctx context.Context, tx pgx.Tx, scope platform.Scope, token, key, customerID string, in WithdrawInput) (ConsentResult, error)
func Export(ctx context.Context, tx pgx.Tx, scope platform.Scope, token, key, customerID string) ([]byte, error)
func Erase(ctx context.Context, tx pgx.Tx, scope platform.Scope, token, key, customerID string) (ErasureSummary, error)
func ReadBuyerPrivacy(ctx context.Context, tx pgx.Tx, s buyer.Scope, token string) (BuyerPrivacy, error)
func BuyerSetConsent(ctx context.Context, tx pgx.Tx, s buyer.Scope, token, key string, in ConsentInput) (ConsentResult, error)
func BuyerExport(ctx context.Context, tx pgx.Tx, s buyer.Scope, token, key string) ([]byte, error)
func BuyerErase(ctx context.Context, tx pgx.Tx, token, storeID, key string) (ErasureSummary, error) // D11: no WithScope
var ErrIdempotencyConflict, ErrErasureBlocked, ErrErased, ErrExportTooLarge error

package reporting // internal/reporting
type FinanceRow struct{ Day string `json:"day"`; Currency string `json:"currency"`; Environment string `json:"environment"`; CapturedCount int64 `json:"captured_count"`
    CapturedMinor int64 `json:"captured_minor"`; RefundedMinor int64 `json:"refunded_minor"`; NetMinor int64 `json:"net_minor"` }
type FinanceSummary struct{ From string `json:"from"`; To string `json:"to"`; Timezone string `json:"timezone"`; Rows []FinanceRow `json:"rows"`; Totals []FinanceRow `json:"totals"` } // totals: Day=""
func Finance(ctx context.Context, tx pgx.Tx, scope platform.Scope, token, from, to string) (FinanceSummary, error)
func FinanceCSV(ctx context.Context, tx pgx.Tx, scope platform.Scope, token, from, to string) ([]byte, error) // export_finance_summary (audit)

func registerCustomerRoutes(mux *http.ServeMux, pool *pgxpool.Pool) // internal/httpapi/customers.go: 5 §5 customer rows
func registerFinanceRoutes(mux *http.ServeMux, pool *pgxpool.Pool)  // internal/httpapi/finance.go: summary + summary.csv
// internal/buyerhttp/privacy.go — paths + handlers the integrator dispatches to (same signature each):
const privacyPath, consentsPath, privacyExportPath, privacyErasurePath = "/v1/buyer/privacy", "/v1/buyer/consents", "/v1/buyer/privacy/export", "/v1/buyer/privacy/erasure"
func (h *handler) privacyGet|consentPut|privacyExport|privacyErase(ctx context.Context, w http.ResponseWriter, r *http.Request, storeID, token, key string) error
```
`consentPut` carries the A-3 seam comment exactly: `// A-3 (meta-ads-v1, R3): ads.put_capi_context(hash, store, User-Agent)
goes here, same tx, iff in.Purpose==PurposeAdsPersonalization && in.Granted.` R2 ships no call (C-7); meta-ads adds the line.
Errors map through `responseError{status,code}` inside privacy.go (no edit to `classify`).

## Build
1. **SQL** `0078_customers_privacy.sql`: §3.1 exactly (preconditions, role, schema, tables, indexes, FORCE RLS,
   append-only, functions, column grants + `privacy_scoped` policies, merchant definers verify GUCs like 0063 and
   never `set_config`; buyer definers follow 0013). Permission CHECK re-derived with `pg_get_constraintdef` and
   extended **only if absent** (R3 0074 may apply after 0079, C-7); no grant rows; no reference to any R3 role.
   `apply_erasure` label = `'erased-'||replace(id::text,'-','')`; no write to `actor_key`, `claims.meta_intake`,
   `live.claim_sources`, `social.*`. `replay_erasures(ids)` inserts `via='restore'` tombstones. `COMMENT ON` all.
2. **Go** `internal/customers` (+`doc.go`), `internal/reporting` (+`doc.go`): frozen block; export built in the
   same tx as `record_export`; reuse `merchantorders.Get` for order detail, buyer history detail for buyer export.
3. **HTTP** `httpapi/customers.go`, `httpapi/finance.go` (own classifier via `scopedAs`; POSTs need
   Idempotency-Key, strict bounded bodies, erasure body exactly `{"confirm":"ERASE"}`), `buyerhttp/privacy.go`.
4. **Integrator hand-off** (§ Integrator hooks) as `output/customers-core/integrator-hooks.patch`. Hand step 1 first = F1.

## Comments (PROCESS §5, reviewer checks)
Package comments: `customers` "owns consent and owner-level privacy actions; It never merges identities, sends
marketing or touches actor-level data (U08)"; `reporting` "owns read-only finance aggregates; It never writes".
Every call into another domain's SQL names it and why (`// customers.erase_owner: CD7 refusal + redaction, one tx`,
`// merchantorders.Get: existing projection, export must equal the order page`). Money sums carry `// I05:`.
Every table/column/function/role in 0078 has `COMMENT ON` (owning package, allowed roles, non-goals). No hand-written
depends-on lists (`dependency-map.md` is generated; the integrator regenerates it).

## Write paths
`migrations/0078_customers_privacy.sql`, `internal/customers/**`, `internal/reporting/**`,
`internal/httpapi/{customers.go,customers_test.go,finance.go,finance_test.go}`,
`internal/buyerhttp/{privacy.go,privacy_test.go}`, `output/customers-core/**`.
Forbidden: `internal/httpapi/handler.go`, `internal/buyerhttp/handler.go`, `internal/merchantorders/**`,
`internal/billing/**`, 0079, `tests/**`, `apps/**`, `cmd/**`, contracts, OpenAPI, go.mod/go.sum.

## Verify (unit tests only; names must not start with `TestCustomersBillingCB`)
```sh
GOTOOLCHAIN=go1.27.1 go vet ./... && gofmt -l internal cmd && bash scripts/dev/check-pkgdocs.sh
GOTOOLCHAIN=go1.27.1 go test -race -count=1 ./internal/customers/... ./internal/reporting/... ./internal/httpapi ./internal/buyerhttp
LC_FOCUSED_TIMEOUT=1800s bash scripts/dev/test-focused.sh '^Test(T0[46]|Buyer(Capability|Checkout|OrderHistory|Retirement)|MerchantOrders|Identity|OwnerProvisioning|Migration|LiveClaims)'
python3 scripts/check_packet.py
```
Unit tests: `CurrentConsents` table, q/limit/date parsing, strict bodies, error → status/code map, export size cap.
A regression that fails only because an existing test asserts an exact privilege/function set is **not** fixed here:
list test name + assertion + before/after for the integrator. Logs → `output/customers-core/`.

## Integrator hooks (only the integrator edits these; unit ships the patch)
- `internal/httpapi/handler.go`: `registerCustomerRoutes(mux, pool)` and `registerFinanceRoutes(mux, pool)` after
  `registerShipmentRoutes`.
- `internal/buyerhttp/handler.go`: four `routeKind`s + `matchRoute` cases for the four paths; `allowed`: privacy GET,
  consents PUT, export POST, erasure POST; key required on PUT/POSTs (`keyFor` write rule); `dispatch` cases → the
  privacy.go methods. CVS also edits this file — integrator merges both.
- `contracts/core-openapi.json` paths; `contracts/tasks.json` T14 evidence; exact-set privilege tests in
  `tests/foundation` (T06/KC03/identity) updated to frozen values only; `scripts/dev/depmap.sh` regen.

## Gates
Implementer: unit tests above. Independent (`customers-billing-tests`, each red-then-green): CB01 (consent part),
CB02 (0078 half), CB03, CB04, CB05, CB07 (privacy under RESTRICTED), CB09 (customers/finance/buyer rows), CB11
(customers/finance/privacy pages). Do not claim them.

## Order / Non-goals / Return
Dispatch now; F1 = 0078 merged (unblocks billing-core and test PG runs); F2 = Go + hooks merged. Non-goals: billing,
UI, CAPI call (R3), actor-level deletion/U08 (0071), Meta deletion callback, identity merge, async privacy queue.
NOT_RUN: CB05 restore drill beyond the MODEL (T20). Return commit SHA, model/reasoning, base, paths, commands + exit
codes + PASS/FAIL/SKIP counts, evidence paths, D1–D14 handling, exact-set test deltas, risks, NOT_RUN. Any deviation
from the frozen block = stop and escalate.
