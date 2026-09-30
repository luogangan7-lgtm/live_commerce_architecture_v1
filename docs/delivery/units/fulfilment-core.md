# Unit fulfilment-core — manual shipment SQL, merchant projection, shipment routes, CSV export

Role: commerce_worker (mid tier). Base = integrator-recorded SHA at dispatch (≥ `8f491dc`).
Worktree `.worktrees/fulfilment-core`, branch `unit/fulfilment-core`. No delegation or network.
Contract: `contracts/manual-fulfilment-v1.md` (FROZEN) incl. "Integrator rulings"; plus
`contracts/stripe-refund-v1.md` §7.1 (projection invariants + merchant read fields) and
`docs/delivery/units/refund-core.md` "Defaults adopted" D4–D6 (binding unless overridden).

**Goal:** a merchant with `fulfillment:write` records/corrects/voids one merchant-arranged shipment
per paid order; the buyer sees carrier + tracking; `orders:export` downloads unshipped orders as CSV.
**This unit is the single owner of the merchant order projection** (`identity.read_merchant_orders`
SQL + `internal/merchantorders/orders.go` decoders/validator) for both refunded and shipped states.

## Read (by section)
`docs/delivery/PROCESS.md`; fulfilment contract §0–§5, §7, rulings; refund contract §4.2 DDL, §4.3,
§4.5 rows 1–3, §7.1. Code by symbol: `migrations/0027_merchant_orders.sql`
(`read_merchant_orders`, `create_initial_store` auth pattern, `merchant_order_projection` policy),
`migrations/0018_payment_capture.sql` (`payment_work_items`, review policies),
`internal/merchantorders/orders.go` (`List`, `Get`, `read`, `exact`, `validSummary`, `decodeDetail`,
`validState`), `internal/httpapi/{orders,handler,claims}.go` (`scopedAs`, `claimsBody`),
`internal/checkout/checkout.go` (`Get`, `Order`), `internal/checkout/orders.go` (`ListOrders`).
The 0062 DDL/grants you depend on: refund contract §4.1–§4.5 + refund-core D4/D5 (frozen text; the
file lands at freeze F1).

## Defaults adopted (integrator may override)
- E1 0063 `CREATE OR REPLACE identity.read_merchant_orders` once, carrying **both** the refund §7.1
  fields (`payment_state` + `PARTIALLY_REFUNDED`/`REFUNDED` with precedence `NOT_STARTED >
  REVIEW_REQUIRED > REFUNDED > PARTIALLY_REFUNDED > CAPTURED > AUTHORIZED > PENDING`,
  `refunded_minor` = Σ SUCCEEDED-not-FAILED/CANCELED, `refund_pending_minor` = Σ held-not-succeeded)
  and the fulfilment fields (`shipped`/`unshipped` filters, `MERCHANT_SHIPPED`, detail `shipment`).
- E2 Permission probe for the UI (no frozen contract exposes grants): read-only
  `GET /v1/admin/stores/{store_id}/order-actions` → `{refund, fulfillment_write, orders_export}` booleans,
  `orders:read` required, computed with three `platform.RequirePermission` calls (no SQL change;
  `ErrForbidden` → false). The server stays the authority on every write.
- E3 `commerce_checkout_runtime` also gets `SELECT(tenant_id,store_id,owner_id,order_id,current_version)`
  on `manual_shipment_heads` + buyer owner-scope policy (contract lists only the versions columns).
- E4 `export_unshipped_orders` sets `app.tenant_id/store_id/principal_id` from its `resolve_access`
  result before the audit insert (round-3 P2); policy name `auth_export_audit`.
- E5 0063 ops policies for checkout_writer are the 0062 permissive + RESTRICTIVE pair (refund D5);
  0063 creates none of them again.
- E6 PUT success body = `ShipmentVersion` (merchant fields incl. note/void_reason/principal_id).

## FROZEN Go interface
```go
package merchantorders
// Summary gains RefundedMinor int64 `json:"refunded_minor"`; RefundPendingMinor int64 `json:"refund_pending_minor"`
// Detail gains Shipment *Shipment `json:"shipment"` (null unless head SHIPPED)
// ListRequest.State additionally admits "shipped", "unshipped".
type Shipment struct{ Version int64 `json:"version"`; Status string `json:"status"`; CarrierCode string `json:"carrier_code"`
    CarrierName *string `json:"carrier_name"`; TrackingNumber string `json:"tracking_number"`; TrackingURL *string `json:"tracking_url"`; RecordedAt string `json:"recorded_at"` }
type ShipmentVersion struct{ Shipment; Note *string `json:"note"`; VoidReason *string `json:"void_reason"`
    PrincipalID string `json:"principal_id"` }
type ShipmentInput struct{ ExpectedVersion int64 `json:"expected_version"`; Status string `json:"status"`; CarrierCode *string `json:"carrier_code"`
    CarrierName *string `json:"carrier_name"`; TrackingNumber *string `json:"tracking_number"`; TrackingURL *string `json:"tracking_url"`
    Note *string `json:"note"`; VoidReason *string `json:"void_reason"` }
func NormalizeShipment(in ShipmentInput) (ShipmentInput, error) // §3 + void-body rule; pure (MF01)
func RecordShipment(ctx context.Context, tx pgx.Tx, scope platform.Scope, token, key, orderID string,
    in ShipmentInput) (ShipmentVersion, error)
func ShipmentHistory(ctx context.Context, tx pgx.Tx, scope platform.Scope, token, orderID string) ([]ShipmentVersion, error)
type ExportRow struct{ OrderID, CreatedAtUTC, ServiceCode, DestinationKind, RecipientName, Phone,
    Country, Region, City, PostalCode, Line1, Line2, PickupNamespace, PickupCode, PickupName,
    PickupAddress, Items string; TotalMinor int64; Currency string }
func WriteUnshippedCSV(w io.Writer, rows []ExportRow) error // BOM, CRLF, RFC 4180, §5.3 guard + phone; pure (MF01)
type Export struct{ Body []byte; Rows int; Truncated bool }
func ExportUnshipped(ctx context.Context, tx pgx.Tx, scope platform.Scope, token string) (Export, error) // asks 1001
type OrderActions struct{ Refund bool `json:"refund"`; FulfillmentWrite bool `json:"fulfillment_write"`; OrdersExport bool `json:"orders_export"` }
func Actions(ctx context.Context, tx pgx.Tx, scope platform.Scope, token string) (OrderActions, error)
var ErrVersionChanged, ErrNotShippable, ErrInvalidCarrier, ErrInvalidTracking, ErrInvalidURL,
    ErrVoidRequiresShipped, ErrInvalidVoid error // → 409 version_changed / 422 codes of §5.1

package checkout
type BuyerShipment struct{ Status string `json:"status"`; CarrierCode string `json:"carrier_code"`; CarrierName *string `json:"carrier_name"`
    TrackingNumber string `json:"tracking_number"`; TrackingURL *string `json:"tracking_url"`; RecordedAt time.Time `json:"recorded_at"` }
// Order gains Shipment *BuyerShipment `json:"shipment"` (always emitted; null unless head SHIPPED)

package httpapi // new file shipments.go; integrator mounts it unconditionally
func registerShipmentRoutes(mux *http.ServeMux, pool *pgxpool.Pool) // PUT shipment, GET history,
    // GET orders/unshipped.csv, GET order-actions (E2)
```
`refunds.go` in the same package belongs to refund-core: do not rename/re-sign the helpers it reuses
(`mapError`, `validAuthorityInput`, `exact`, `canonicalTime`, `money`, `currency`).

## Build
1. **0063** per §4/§4.1 + E1/E3/E4/E5: re-derive `store_grants_permission_check` from the live 0062
   constraint (adds `fulfillment:write`, `orders:export`; no grant rows), `orders_fulfillment_state_check`,
   versions/heads tables, set-once + monotone triggers, the deferred head⇔order constraint trigger on
   **both** tables, partial index, the three definers, `commerce_auth` `SELECT(reason)` on review_cases,
   `USAGE ON SCHEMA ops` + audit policy. MD6 predicate written once (SQL function or view used by
   record, `unshipped` filter and export). `COMMENT ON` everything.
2. **Projection** `orders.go`: §7.1 invariants verbatim (enums, `READY ⇒ …`, `MERCHANT_SHIPPED ⇒ …`,
   `CONFIRMED ⇒ …`, unchanged rules), new summary/detail keys through `exact`, `validState` filters.
3. **Services** `shipments.go`, `export.go`: §3 validation in Go (URL parsed + canonically
   re-serialized; `carrierTrackingTemplates` empty map with a PROCESS §5 comment), SQL replay/CAS
   mapping, CSV per §5.3 streamed from memory; logs only store UUID, row count, duration.
4. **Routes** `httpapi/shipments.go`: own classifier via `scopedAs`; PUT needs Idempotency-Key, strict
   body with explicit nulls, no query, HEAD rejected; CSV headers exactly §5.1 (filename, `no-store,
   private`, `X-Export-Truncated`); `fulfillment:write` / `orders:read` / `orders:export`+`orders:read`.
5. **Buyer**: `checkout.Get` reads the SHIPPED head in the same tx under buyer RLS; history state admits
   `MERCHANT_SHIPPED` wherever Go validates it.
6. Existing exact-key assertions that the frozen contracts change (MOR summary/detail keys, buyer
   order detail `shipment`): update only the expected key sets in `tests/foundation/{merchant_orders,
   buyer_http,buyer_order_history}_test.go`; list each diff in the return. No negative case removed.

## Write paths
`migrations/0063_manual_fulfilment.sql`, `internal/merchantorders/{orders.go,orders_test.go,shipments.go,
shipments_test.go,export.go,export_test.go,actions.go}`, `internal/httpapi/{shipments.go,shipments_test.go}`,
`internal/checkout/{checkout.go,orders.go,checkout_test.go}`, the three test files in Build 6,
`output/fulfilment-core/**`. Forbidden: 0062/0065, `refunds.go`, `payment_view.go`, `handler.go`,
`cmd/api/main.go`, contracts, `core-openapi.json`, `apps/**`, other `tests/**`, go.mod.

## Verify
```sh
GOTOOLCHAIN=go1.27.1 go vet ./... && gofmt -l internal cmd
GOTOOLCHAIN=go1.27.1 go test -race -count=1 ./internal/merchantorders ./internal/httpapi ./internal/checkout
LC_FOCUSED_TIMEOUT=1800s bash scripts/dev/test-focused.sh '^Test(MerchantOrders|BuyerOrder|BuyerHTTP|BuyerCheckout|PaymentCapture|StripeSP08)'
python3 scripts/check_packet.py
```
Logs → `output/fulfilment-core/`. MF01–MF08 belong to refund-fulfilment-tests.

## Order / Non-goals / Return
Author now, parallel with refund-core. PG runs only after **F1** (0062 + post_river 0013 merged): rebase;
0063 applies on top of 0062. Merge after refund-core, in one batch with the UI admin model change.
No carrier API, labels, tracking polls, buyer messages, bulk import (M-7), refund code, UI/BFF.
Return commit SHA, model/reasoning, base, paths, commands + exit codes + PASS/FAIL/SKIP counts,
evidence, E1–E6 handling, risks, NOT_RUN. Any deviation from the frozen signatures = stop and escalate.
