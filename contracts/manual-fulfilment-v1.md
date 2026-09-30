# Manual fulfilment v1 — merchant-arranged shipment, tracking and unshipped export

Status: **FROZEN 2026-09-29** after review rounds 1–3 (fresh reviewer round 3: PASS_WITH_P2). Was DRAFT (amended round 2). Round-1
adversarial review `output/contract-review/refund-fulfilment-round1.json` (BLOCK) is addressed by the
in-place edits marked "(A1)"; §9 maps each finding. The round-2 fresh review (one P1: schema USAGE
and RLS policies) is addressed by the edits marked "(A2)"; §10.
Evidence label for this file: DESIGN. Every MF gate below is NOT_RUN.

Implements the manual path of architecture §13.4 ("首发provider外保留手工发货/单号导入与面单重打，但手工动作同样留审计")
for R1. Builds on [merchant-orders-v1](merchant-orders-v1.md) (read authority, projection,
`internal/merchantorders`), [merchant-orders-ui-v1](merchant-orders-ui-v1.md) (inline detail row),
[buyer-order-history-v1](buyer-order-history-v1.md) (owned order reads),
[payment-capture-v1](payment-capture-v1.md) (`payment_work_items`, `PAID_ALLOCATION_FAILED`) and the
service-settings rule that merchant-arranged fulfilment never creates a provider operation
([merchant-service-settings-v1](merchant-service-settings-v1.md)). Architecture §12.1, §13.2–§13.4 and
invariants I01, I02, I04, I11, I13, I14, I24 are authoritative; where they conflict, this file is wrong.

## 0. Owner inputs, decisions and rulings needed

Owner inputs: R1 includes "merchant records carrier + tracking, buyer sees it" (PROCESS §1 R1-4).
Carrier contracts and label APIs are owner-only and out of R1. No owner input exists on export
columns, permission names or tracking-URL policy.

| # | Decision | Why / risk closed |
| --- | --- | --- |
| MD1 | **One merchant-arranged shipment per order** in v1: the whole order ships as one parcel record. Package split, partial shipment and merged packages (§13.2 `FulfillmentOrder → Package → PackageItem`) are R2. | Smallest correct model; the frozen order lines are not re-partitioned. |
| MD2 | **Execution is `MERCHANT_ARRANGED` only.** Recording a shipment creates no `integration.operations` row, no River job, no provider call, no label, no tracking poll. Carrier fields are merchant-supplied labels, not an adapter connection or capability claim. | Service-settings decision: merchant-arranged must not create provider operations or pretend pickup. |
| MD3 | **New fulfilment state `MERCHANT_SHIPPED`** on `checkout.orders.fulfillment_state`, meaning only "the merchant attests the parcel was dispatched with this carrier/tracking". It never means `HANDED_OVER`, `IN_TRANSIT` or `DELIVERED` (§13.4: a tracking number is not in-transit evidence; I13). | Keeps states honest; carrier tracking events are an R2 adapter concern. |
| MD4 | **Append-only versions + head CAS.** Every record, correction or void is a new immutable version row; the head points to the current version and is updated with `expected_version` CAS (the `service_heads` pattern). Nothing is overwritten. | §13.4 audit requirement; I14. |
| MD5 | **Void returns the order to `MANUAL_UNASSIGNED`** (mistaken shipment). A voided record stays in history. | Reversible merchant error without deleting evidence. |
| MD6 | **Eligibility to ship** (checked under the order lock): `commercial_state='CONFIRMED'`, `fulfillment_state='MANUAL_UNASSIGNED'`, a `payment_work_items` row with `state='READY'` for the order and a CAPTURED fact, and (A1) `NOT EXISTS payments.review_cases` for the work item's attempt with `reason <> 'PROVIDER_PRESENTMENT_DRIFT'` (the work item is written once at capture, so a later `REFUND_HISTORY`, `REFUND_UNRESOLVED`, `REFUND_CONFLICTING`, `REFUND_AMOUNT_MISMATCH` or dispute `CONFLICTING_REPORT` must block shipping by itself), and, once `stripe-refund-v1` exists, refund `held < captured` (a fully refunded or full-refund-in-flight order cannot be shipped; a partial refund can). `PAID_ALLOCATION_FAILED`, `CANCELLED`, `REVIEW_REQUIRED` work, DRAFT and AWAITING_PAYMENT are refused. | Never ship unpaid, unallocated or refunded goods (I24, capture-v1 "not a fake carrier job"). |
| MD7 | **Corrections** (carrier/tracking typo) are allowed only while the head is `SHIPPED`; void is allowed only while the head is `SHIPPED`. Refunds never change fulfilment state (stripe-refund RD6). | Clear single transition graph. |
| MD8 | **Separate permissions:** `fulfillment:write` to record/correct/void; `orders:export` to download recipient PII as CSV. Neither is implied by `orders:read`; neither is backfilled. (A1) 0063 adds no grant row; the store creator receives both only via `0065_owner_provisioning.sql` (stripe-refund-v1 §12, OP01). | Export is bulk PII (I11); write authority differs from read (merchant-orders-v1). |
| MD9 | **CSV export is generated on request, never stored,** capped, audited, formula-injection-safe, `no-store`. | "No automatic exports" (merchant-orders-v1) stays true: this is an explicit, permissioned, audited action. |
| MD10 | **No buyer message** is sent on shipment in v1; the buyer sees the state on the order page. | Messaging needs consent/window rules (§10) and a separate contract. |

Rejected alternatives:
- A mutable `tracking_number` column on `checkout.orders`: overwrites history, fails §13.4 audit.
- Creating an `integration.operations` row "for uniformity": would claim a provider action that never
  happens and could be picked up by dispatch paths.
- Mapping `MERCHANT_SHIPPED` to `IN_TRANSIT`/`DELIVERED`: false evidence (I13).
- Letting the buyer page fetch carrier tracking pages server-side: SSRF surface and unverified formats.
- Building carrier-specific CSV formats (7-ELEVEN/FamilyMart bulk upload) now: their formats are not
  verified in this repo; a generic column set ships first (ruling M-6).

### 0.1 Integrator ruling needed

- **M-1** Permission names `fulfillment:write`, `orders:export`; whether onboarding grants them to new
  store creators (draft: no; explicit provisioning).
- **M-2** Tracking URL policy. Draft: no built-in per-carrier URL templates until each template URL is
  verified with source + retrieval date (none verified in this unit); a merchant may supply an
  explicit `tracking_url` (https only, rules in §3.2) shown to the buyer with its host visible. Alternatives:
  host allowlist per known carrier, or no merchant URL at all.
- **M-3** Carrier code list and display names (draft §3.1). Confirm SF Express and Chunghwa Post
  naming for zh-Hant/zh-Hans/en, and whether Hi-Life/OK Mart CVS are needed for R1.
- **M-4** `shipped_on` merchant-attested date (draft: not collected; server `recorded_at` only).
- **M-5** Export row cap 1000 and "oldest paid first" order; paging beyond the cap (draft: none, the
  response flags truncation).
- **M-6** Export columns (draft §5.3); carrier-specific import formats deferred.
- **M-7** Tracking-number bulk import (§13.4 "单号导入"): draft defers to R2.
- **M-8** Whether a `MERCHANT_SHIPPED` order may still be refunded fully without any return flow
  (draft: yes; refund and return are independent, §12.3/I13).
- **M-9** Dependency on `stripe-refund-v1`: draft assumes 0062 lands before 0063 so the MD6 refund
  check can reference `payments.stripe_refunds`; if refunds slip, the check is added by 0062 instead.

## 1. Existing facts relied on (repository, read 2026-09-28 UTC)

| # | Fact | Source |
| --- | --- | --- |
| E1 | `checkout.orders.fulfillment_state ∈ {MANUAL_UNASSIGNED, CANCELLED, PAID_ALLOCATION_FAILED}`; `commerce_checkout_writer` holds `UPDATE(commercial_state,fulfillment_state,…)`. | `migrations/0013_buyer_checkout.sql:23,175`, `0018_payment_capture.sql:199-201` |
| E2 | `fulfillment.payment_work_items` is one immutable row per order, `state ∈ {READY, REVIEW_REQUIRED}`, FK to the CAPTURED fact; "not a carrier request or proof of shipment". | `0018_payment_capture.sql:102-110`, payment-capture-v1 |
| E3 | Merchant reads go through `identity.read_merchant_orders` (owner `commerce_auth`, `orders:read`, single snapshot, fresh final SQL auth); Go `merchantorders.List/Get`; detail carries frozen items, totals and destination incl. pickup code strings with leading zeroes. | `0027_merchant_orders.sql`, `internal/merchantorders/orders.go:101,150` |
| E4 | Buyer order reads use `commerce_checkout_runtime` under buyer RLS (`internal/checkout/orders.go` `ListOrders`/order GET). | `0013_buyer_checkout.sql:173`, `internal/checkout/orders.go:78-99` |
| E5 | Merchant writes use `ops.command_results(tenant,store,operation,idempotency_key)` and `ops.audit_events(action)`; `expected_version=0` creates, later writes require the current version. | `0002_catalog_inventory.sql:11`, `0001_foundation.sql:54`, merchant-settings-http-v1 |
| E6 | Latest permission vocabulary in the repo is 0033's list (adds `live:read`, `live:manage` without onboarding grant); 0062 adds `payments:refund` before this file's 0063 (M-9). | `0033_live_planning.sql:3-8`, stripe-refund-v1 §4.1 |
| E7 | (A1) `payments.apply_stripe_observation` (0061:752-780) flips a CONFIRMED order to `PAID_ALLOCATION_FAILED` on any replayed paid report when any review exists; stripe-refund-v1 §4.4 guards that branch with `v_new_capture OR v_closed_before`, so post-capture reviews never change `fulfillment_state` or the work item. | `0061_stripe_psp.sql:769-778`, stripe-refund-v1 §4.4 |
| E8 | (A1) `commerce_auth` holds only `SELECT(tenant_id,store_id,attempt_id)` on `payments.review_cases`. | `0027_merchant_orders.sql:64,70` |

## 2. State machine

`checkout.orders.fulfillment_state` (commercial state untouched by this contract):

| From | Event | To | Guard |
| --- | --- | --- | --- |
| MANUAL_UNASSIGNED | record shipment (v1 SHIPPED) | MERCHANT_SHIPPED | MD6 eligibility |
| MERCHANT_SHIPPED | correct (vN SHIPPED) | MERCHANT_SHIPPED | head SHIPPED, `expected_version` = head |
| MERCHANT_SHIPPED | void (vN VOIDED) | MANUAL_UNASSIGNED | head SHIPPED, reason required |
| MANUAL_UNASSIGNED after a void | record again (vN+1 SHIPPED) | MERCHANT_SHIPPED | MD6 again |
| CANCELLED, PAID_ALLOCATION_FAILED, any non-CONFIRMED | any | unchanged | PT422 `not_shippable` |

Shipment version status is `SHIPPED` or `VOIDED`. The head's current version decides the state; the
order column is written in the same transaction and must always agree (a deferred constraint trigger
checks `head.status='SHIPPED' ⇔ fulfillment_state='MERCHANT_SHIPPED'`). (A1) The trigger function
`fulfillment.guard_manual_shipment_state()` is installed as DEFERRABLE INITIALLY DEFERRED constraint
triggers on **both** `checkout.orders` (`AFTER UPDATE OF fulfillment_state`) and
`fulfillment.manual_shipment_heads` (`AFTER INSERT OR UPDATE`); an order with no head must not be
`MERCHANT_SHIPPED`.

## 3. Data rules

### 3.1 Carrier

`carrier_code` ∈ {`seven_eleven_cvs` (7-ELEVEN 交貨便/取貨), `familymart_cvs` (全家 店到店),
`hilife_cvs` (萊爾富), `okmart_cvs` (OK mart), `sf_express` (順豐速運), `chunghwa_post` (中華郵政),
`other`} (A1, ruling M-3). `carrier_name` is free text, NFC,
1..80 characters, no control characters: **required** when `other`, optional display override
otherwise. A carrier code is a label, not an integration binding, capability or contract claim (MD2).
The buyer pickup destination (frozen at checkout) is not revalidated or rewritten by the carrier choice.

### 3.2 Tracking

- `tracking_number`: required for `SHIPPED`, trimmed, 1..64 characters, `^[A-Za-z0-9][A-Za-z0-9 -]{0,63}$`
  with no trailing space; stored exactly (no case folding; leading zeroes kept).
- `tracking_url` (optional, M-2): absolute `https://` URL ≤512 bytes, host made only of letter/digit/
  hyphen labels (no leading or trailing hyphen) with at least one dot and a last label that starts with a
  letter (so no IPv4-shaped host and nothing WHATWG `new URL()` would reject; amended 2026-09-29, S6), no
  userinfo, no explicit port, no fragment, no control/whitespace characters, parsed and re-serialized
  canonically by Go; SQL re-checks prefix and length. The buyer UI renders it as a link with
  `rel="noopener noreferrer nofollow"`, `target="_blank"`, showing the host text next to it. Never
  fetched by the server.
- Built-in URL templates: a Go table `carrierTrackingTemplates` keyed by carrier code, **empty in v1**;
  an entry may be added only with its docs URL and retrieval date (PROCESS §5), and the `{tracking}`
  placeholder is path-escaped. When both exist, the explicit `tracking_url` wins.
- `note` (optional, merchant-only, never shown to the buyer): 0..200 characters. `void_reason`
  ∈ {`wrong_order`, `wrong_tracking`, `not_dispatched`, `other`} required for VOIDED.

## 4. Persistence: `migrations/0063_manual_fulfilment.sql`

```sql
-- permission vocabulary (A1): re-derive the CURRENT store_grants_permission_check via pg_get_constraintdef
-- (0062 list incl. payments:refund) + 'fulfillment:write','orders:export'; no grant rows (onboarding: 0065)
ALTER TABLE checkout.orders DROP CONSTRAINT orders_fulfillment_state_check;
ALTER TABLE checkout.orders ADD CONSTRAINT orders_fulfillment_state_check
 CHECK(fulfillment_state IN ('MANUAL_UNASSIGNED','CANCELLED','PAID_ALLOCATION_FAILED','MERCHANT_SHIPPED'));

CREATE TABLE fulfillment.manual_shipment_versions (
 tenant_id uuid NOT NULL, store_id uuid NOT NULL, owner_id uuid NOT NULL, order_id uuid NOT NULL,
 version bigint NOT NULL CHECK(version>0),
 status text NOT NULL CHECK(status IN ('SHIPPED','VOIDED')),
 carrier_code text NOT NULL CHECK(carrier_code IN ('seven_eleven_cvs','familymart_cvs','hilife_cvs','okmart_cvs',
  'sf_express','chunghwa_post','other')),                                        -- A1: M-3
 carrier_name text CHECK(carrier_name IS NULL OR (length(carrier_name) BETWEEN 1 AND 80 AND carrier_name !~ '[[:cntrl:]]')),
 tracking_number text NOT NULL CHECK(tracking_number ~ '^[A-Za-z0-9][A-Za-z0-9 -]{0,63}$' AND tracking_number !~ ' $'),
 tracking_url text CHECK(tracking_url IS NULL OR (octet_length(tracking_url)<=512 AND tracking_url ~ '^https://([a-zA-Z0-9]([a-zA-Z0-9-]*[a-zA-Z0-9])?\.)+[a-zA-Z]([a-zA-Z0-9-]*[a-zA-Z0-9])?([/?][!-~]*)?$')),
 note text CHECK(note IS NULL OR (length(note)<=200 AND note !~ '[[:cntrl:]]')),
 void_reason text CHECK(void_reason IN ('wrong_order','wrong_tracking','not_dispatched','other')),
 principal_id uuid NOT NULL, recorded_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 PRIMARY KEY(tenant_id,store_id,order_id,version),
 CHECK((status='VOIDED')=(void_reason IS NOT NULL)),
 CHECK(carrier_code<>'other' OR carrier_name IS NOT NULL),
 FOREIGN KEY(tenant_id,store_id,owner_id,order_id) REFERENCES checkout.orders(tenant_id,store_id,owner_id,id),
 FOREIGN KEY(tenant_id,principal_id) REFERENCES identity.memberships(tenant_id,principal_id)
);
CREATE TABLE fulfillment.manual_shipment_heads (
 tenant_id uuid NOT NULL, store_id uuid NOT NULL, owner_id uuid NOT NULL, order_id uuid NOT NULL,
 current_version bigint NOT NULL CHECK(current_version>0),
 updated_at timestamptz NOT NULL,
 PRIMARY KEY(tenant_id,store_id,order_id),
 FOREIGN KEY(tenant_id,store_id,order_id,current_version)
  REFERENCES fulfillment.manual_shipment_versions(tenant_id,store_id,order_id,version) DEFERRABLE INITIALLY DEFERRED
);
```

- A VOIDED version copies the carrier/tracking of the version it voids (so history reads without joins).
- Versions are append-only (no UPDATE/DELETE grant; trigger refuses both). Heads: only
  `UPDATE(current_version,updated_at)`, monotone +1 (trigger).
- Index `(tenant_id,store_id,order_id,version DESC)` is the PK; export uses the existing
  `checkout.orders(tenant_id,store_id,created_at DESC,id DESC)` index plus a partial index
  `ON checkout.orders(tenant_id,store_id,created_at,id) WHERE commercial_state='CONFIRMED' AND
  fulfillment_state='MANUAL_UNASSIGNED'`.
- **Roles/RLS:** both tables FORCE RLS, PUBLIC revoked. Owner of the write definer is
  `commerce_checkout_writer` (already the order-state writer): SELECT/INSERT on versions, SELECT/INSERT/
  `UPDATE(current_version,updated_at)` on heads, with explicit policies. `commerce_auth` (merchant read
  definer owner) gets column SELECT + SELECT policy. `commerce_checkout_runtime` (buyer) gets SELECT on
  `(tenant_id,store_id,owner_id,order_id,version,status,carrier_code,carrier_name,tracking_number,
  tracking_url,recorded_at)` only — never `note`, `void_reason` or `principal_id` — under the existing
  buyer owner-scope RLS pattern. `commerce_runtime` gets EXECUTE on the definers only; no table access.
- (A1) **Grants and policies** beyond the tables above (0063 unless noted): `commerce_checkout_writer`
  receives the merchant-auth and `ops` grants listed in stripe-refund-v1 §4.5 rows 1–3 (0062 creates
  them; 0063 relies on them and RF03/MF02 both assert them): (A2) `USAGE ON SCHEMA identity, ops`
  (0013:9/0016:84 grant neither), `SELECT(token_hash,principal_id,audience,
  revoked_at,expires_at) ON identity.sessions`, `EXECUTE identity.resolve_access(bytea,uuid,text)`,
  `SELECT, INSERT ON ops.command_results`, `INSERT ON ops.audit_events`, with RESTRICTIVE policies
  (tenant/store = `app.tenant_id`/`app.store_id`, `principal_id` = `app.principal_id`).
  `record_manual_shipment` sets `app.tenant_id`, `app.store_id`, `app.principal_id` from the auth
  result and `app.buyer_id` from the locked order's `owner_id` before reading `payment_work_items`,
  `payments.facts`, `payments.review_cases` (existing checkout_writer SELECT, 0018:126) and writing.
  `commerce_auth` gains `SELECT(reason) ON payments.review_cases` (E8; existing `merchant_order_projection`
  policy) for the MD6 predicate in `read_merchant_orders`/`export_unshipped_orders`, and
  `INSERT ON ops.audit_events` with policy `WITH CHECK(action='orders.export_unshipped' AND` tenant/store/
  principal = GUCs`)`, and (A2) `GRANT USAGE ON SCHEMA ops TO commerce_auth` (0063; it holds USAGE only
  on identity, control (0001:84) and checkout, payments, fulfillment (0027:56), so the export's audit
  insert would otherwise fail with 42501). No other role changes. MF02 asserts the exact matrix (positive and one negative
  per role).

### 4.1 SQL entry points (all SECURITY DEFINER, `search_path=pg_catalog`, PUBLIC revoked)

| Function | Owner | EXECUTE | Contract |
| --- | --- | --- | --- |
| `fulfillment.record_manual_shipment(hash bytea, store uuid, order uuid, key text, request_hash bytea, expected_version bigint, status text, carrier_code text, carrier_name text, tracking_number text, tracking_url text, note text, void_reason text) RETURNS jsonb` | checkout_writer | `commerce_runtime` | Merchant token auth (`fulfillment:write`) with the 0027 fresh-final-auth pattern after all locks (READ COMMITTED only; PT401/403/404 distinctions; missing and other-store orders indistinguishable). Replays `ops.command_results` (`operation='fulfillment.manual_shipment.record'`; same key + different hash → PT409). Lock order: order `FOR UPDATE` → work item → refund rows (if present) → head `FOR UPDATE`. `expected_version` must equal the head (0 = no head) else PT409 `version_changed`. Applies §2/MD6/MD7 guards; inserts version, upserts head, updates `fulfillment_state` and `updated_at`, inserts `ops.audit_events` action `fulfillment.shipment_recorded` / `fulfillment.shipment_corrected` / `fulfillment.shipment_voided`, and the command result. Returns the version DTO. No job, no operation, no provider call. |
| `identity.read_merchant_orders(...)` (replaced) | commerce_auth | runtime | Same signature and rules as 0027 (or its latest successor, incl. 0062 refund fields); `state` filter adds `shipped` meaning `fulfillment_state='MERCHANT_SHIPPED'`, and adds `unshipped` = exactly the MD6 predicate (CONFIRMED + MANUAL_UNASSIGNED + READY work + CAPTURED fact + no review other than `PROVIDER_PRESENTMENT_DRIFT` + refund `held < captured`; A1); summary `fulfillment_state` admits `MERCHANT_SHIPPED`; detail adds `shipment: null | {version,status,carrier_code,carrier_name,tracking_number,tracking_url,recorded_at}` from the current head in the same snapshot. |
| `fulfillment.read_manual_shipment_history(hash, store, order) RETURNS jsonb` | commerce_auth | runtime | `orders:read`; all versions ascending including `note`, `void_reason`, `principal_id` (principal as UUID only). |
| `identity.export_unshipped_orders(hash bytea, store uuid, row_limit integer) RETURNS jsonb` | commerce_auth | runtime | `orders:export` **and** `orders:read`; row_limit 1..1001 (Go asks 1001 to detect truncation); eligible orders per MD6 (the same predicate as `unshipped`, incl. the review and refund clauses; A1) ordered `created_at ASC, id ASC`; one snapshot; fresh final auth before returning; inserts `ops.audit_events` action `orders.export_unshipped` (commerce_auth receives INSERT on `ops.audit_events` only for this, via an explicit policy). Returns frozen destination and items per row. |

## 5. HTTP and UI

### 5.1 Merchant admin routes (Go private API; admin BFF mirrors under `/api/admin/`)

| Method/path | Input | Success |
| --- | --- | --- |
| PUT `/v1/admin/stores/{store_id}/orders/{order_id}/shipment` | `Idempotency-Key` required; body exactly `{expected_version, status, carrier_code, carrier_name, tracking_number, tracking_url, note, void_reason}` (nullable fields explicit `null`); no query | 200 version DTO after COMMIT. 409 key conflict / `version_changed`; 422 `not_shippable`, `invalid_carrier`, `invalid_tracking`, `invalid_url`, `void_requires_shipped`; 403 missing `fulfillment:write`; 404 missing/other-store. |
| GET `/v1/admin/stores/{store_id}/orders/{order_id}/shipment/history` | none | `{items:[version…]}` (merchant-only fields included) |
| GET `/v1/admin/stores/{store_id}/orders/unshipped.csv` | no query | `200 text/csv; charset=utf-8`, `Content-Disposition: attachment; filename="unshipped-<store8>-<UTC yyyymmddHHMM>.csv"`, `Cache-Control: no-store, private`, header `X-Export-Truncated: true|false`. |

Existing merchant HTTP rules apply (64 KiB JSON, unknown/duplicate fields rejected, HEAD rejected on
writes, fixed error envelope, tokens from cookie transport only). The PUT is the single command route;
correction and void are distinct `status`/version transitions of it, not separate endpoints.

(A1) **Void body:** for `status="VOIDED"` the body must have `carrier_code`, `carrier_name`,
`tracking_number`, `tracking_url` and `note` all `null`, and `void_reason` non-null; the server copies
the carrier and tracking fields from the head version. Any non-null carrier/tracking/note value → 422
`invalid_void`. For `status="SHIPPED"`, `void_reason` must be `null` (else 422 `invalid_void`). The
request hash is over the exact submitted body, so a void replay is byte-stable.

(A1) **merchant-orders-v1 projection invariants** are amended as stated in stripe-refund-v1 §7.1
(`fulfillment_state` admits `MERCHANT_SHIPPED`; `WorkState READY ⇒ … FulfillmentState ∈
{MANUAL_UNASSIGNED, MERCHANT_SHIPPED}`; `MERCHANT_SHIPPED ⇒ WorkState READY ∧ CONFIRMED`); the Go
validator in `internal/merchantorders/orders.go` is changed by commerce_worker, not bypassed.

### 5.2 Buyer surface

The existing owned order detail `GET /v1/buyer/orders/{id}` adds
`shipment: null | {status:"SHIPPED", carrier_code, carrier_name, tracking_number, tracking_url, recorded_at}`
— only when the head is SHIPPED (a voided head returns `null`). History list `fulfillment_state`
admits `MERCHANT_SHIPPED`. The storefront order page shows "Shipped by the seller" (三語), carrier
display name, tracking number with a copy button, and the link per §3.2; it never says "in transit"
or "delivered". No other buyer fields or routes change; recipient PII rules unchanged.

### 5.3 CSV export

- UTF-8 with BOM (for spreadsheet tools used in Taiwan), CRLF line ends, RFC 4180 quoting.
- Columns (M-6): `order_id, created_at_utc, service_code, destination_kind, recipient_name, phone,
  country, region, city, postal_code, line1, line2, pickup_namespace, pickup_code, pickup_name,
  pickup_address, items, total_minor, currency`. `items` = `code×quantity` joined with `; `.
  Pickup codes are written as text; leading zeroes are kept in the file bytes. Spreadsheet users must
  import those columns as text (the UI says so next to the export button); a double-clicked file may
  still drop them — that is a documented limit, not a guarantee (A1).
- `phone` (A1): digits only; a leading `+886` becomes `0` (Taiwan national form, e.g. `0912345678`);
  any other country code is exported as its digits without `+`. The frozen destination value is not
  changed; this is export formatting only.
- Formula-injection guard (A1): any cell whose first non-space character is `=`, `+`, `-` or `@`, or
  whose first character is TAB, CR or LF, is prefixed with `'`.
- At most 1000 rows; more sets `X-Export-Truncated: true` (M-5). The file is streamed from memory,
  never written to disk, cache, logs, object storage or browser storage; the BFF passes it through as
  an attachment. Logs record only store UUID, row count and duration.

### 5.4 Admin UI (`merchant-orders-ui` amendment, ui_worker)

Inside the approved C inline detail row: a "Shipment" section. With `fulfillment:write` and an
eligible order it shows a form (carrier select with the §3.1 names, carrier name when `other`,
tracking number, optional tracking URL, optional note) and a "Mark shipped" action; with a SHIPPED head
it shows the record plus "Correct" and "Void" (void asks for a reason); history is a disclosure list.
Orders list gains filters `unshipped` and `shipped`, and an "Export unshipped (CSV)" button only with
`orders:export`. One idempotency key per dialog submission; no optimistic state; after success the
row re-reads the detail. Existing MOU rules (locale routes, keyboard access, empty/loading/error
states) apply.

## 6. Test gates (tiers: UNIT, REAL_PG, HTTP_PG, BROWSER)

| Gate | Test | Tier | Required |
| --- | --- | --- | --- |
| MF01 | `TestManualFulfilmentMF01Validation` | UNIT | Carrier/name/tracking/URL/note/void rules incl. unicode, control chars, trailing space, leading zeroes, `https` only, userinfo/port/fragment rejected, canonical re-serialization; empty template table; CSV encoder: BOM, quoting, CRLF, every injection prefix incl. (A1) leading-space-then-`=`/`+`/`-`/`@` and leading LF, phone `+886…` → `0…` without guard prefix, 1000/1001 truncation flag; (A1) all seven carrier codes; void-body rule (`invalid_void`). |
| MF02 | `TestManualFulfilmentMF02Schema` | REAL_PG | Fresh + populated upgrade (after 0062); CHECK negatives; append-only versions (UPDATE/DELETE refused for every role); head monotone; deferred head⇔order-state agreement trigger fires from both `checkout.orders` and heads (A1: a direct order update to `MERCHANT_SHIPPED` without head, and a head flip without order update, both fail at COMMIT); FORCE RLS, column grants (buyer cannot read note/void_reason/principal), definer owner/`proconfig`/ACL, PUBLIC revoked; (A1) the permission CHECK after 0063 still contains `payments:refund` and every earlier value (re-derived, not rebuilt from 0033); 0063 adds no grant row (onboarding grant only via 0065/OP01); the §4 grant matrix exactly; (A2) `has_schema_privilege` USAGE true for `commerce_checkout_writer` on identity/ops and `commerce_auth` on ops, every §4 policy present by name/role/cmd in `pg_policies`; `record_manual_shipment` succeeds through the real definer as a merchant (command result + audit rows written) and `export_unshipped_orders` writes its `orders.export_unshipped` audit row; merchant read function keeps 0027 auth behaviour. |
| MF03 | `TestManualFulfilmentMF03Transitions` | REAL_PG | Real CAPTURED/READY order (via existing capture path, no fabricated facts): record → MERCHANT_SHIPPED; correct → v2; void → MANUAL_UNASSIGNED; re-record → v4; every refused source state (DRAFT, AWAITING_PAYMENT, CANCELLED, PAID_ALLOCATION_FAILED, REVIEW_REQUIRED work, full refund held/succeeded, (A1) READY work + `REFUND_HISTORY` review, READY work + `CONFLICTING_REPORT` review) → 422 `not_shippable` with zero rows, and each is absent from the `unshipped` filter and the export; partial refund still ships; replay same key; key+different body 409; stale `expected_version` 409; two concurrent records → exactly one version 1; zero changes to ledger, balances, reservations, facts, work items, operations, River jobs; exactly one audit row per accepted command. |
| MF04 | `TestManualFulfilmentMF04Authority` | REAL_PG | Two tenants, two stores, two buyers: other-store order indistinguishable 404; `orders:read` alone cannot write or export; `orders:export` alone cannot export (needs read) or write; revoked session/grant during a blocked lock (pg_blocking_pids witness) is rejected by the fresh final auth with no row written. |
| MF05 | `TestManualFulfilmentMF05HTTP` | HTTP_PG | Exact PUT/GET/CSV keys, codes and headers (`no-store`, attachment, truncation flag); strict body/query/method rules; merchant list filters `shipped`/`unshipped`; (A1) the merchant list and detail decode a `MERCHANT_SHIPPED` order, a refunded shipped order and a READY order with a refund review (no projection error); void body with non-null carrier fields → 422 `invalid_void`; buyer detail `shipment` only for SHIPPED head, no merchant-only fields; buyer history state; no PII in logs (log-capture sentinel). |
| MF06 | `TestManualFulfilmentMF06Export` | REAL_PG + HTTP_PG | Eligibility exactly MD6; frozen destination/pickup values after catalog/address edits; oldest-first; 1000 cap; audit row per export; nothing persisted (DB-wide scan finds no CSV bytes). |
| MF07 | `manual-fulfilment.spec.ts` | BROWSER | Merchant marks a paid order shipped (7-ELEVEN CVS, leading-zero tracking; (A1) the carrier select lists all seven codes incl. 萊爾富 and OK mart), corrects, voids, re-ships; buyer order page shows carrier + tracking + link host and never "delivered"; export downloads a CSV that opens with the expected header; merchant without `fulfillment:write` sees no action; 3 locales, desktop + mobile Chromium; screenshots hashed. |
| MF08 | `TestManualFulfilmentMF08Guards` + root | REVIEW + regression | PROCESS §5 comments/`COMMENT ON`; no provider dial, job or operation from `fulfillment` code (source guard); full `go test -race ./...`, `go vet ./...`, `python3 scripts/check_packet.py`; MOR/MOU/BPH/CF and RF gates unchanged; independent test_worker + security_reviewer verdicts. |

Each gate records one red run before its green run (PROCESS §2.4).

## 7. Ownership and sequence

| Artifact | Owner |
| --- | --- |
| `internal/merchantorders` shipment/export additions incl. `orders.go` projection invariants (A1), `internal/checkout` buyer projection | commerce_worker |
| `0063_manual_fulfilment.sql`, admin HTTP mount, `core-openapi.json`, `tasks.json` | integrator |
| `tests/foundation/manual_fulfilment_*_test.go`, `manual-fulfilment.spec.ts` | independent test_worker |
| admin shipment section, export button, storefront shipment display, BFF routes | ui_worker |

Sequence: freeze this file → 0063 + MF02 (integrator; after 0062) → commerce_worker MF01/MF03–MF06
→ UI + MF07 → MF08 verdicts. This unit may run in parallel with refund implementation only after both
contracts' SQL interfaces are frozen (the MD6 refund check reads `payments.stripe_refunds`).

## 8. Known limits / NOT_RUN

- Evidence: DESIGN only; MF01–MF08 NOT_RUN.
- One parcel per order; no split/merge/partial shipment, no package items (§13.2) — R2.
- No carrier API, label purchase, pickup booking, tracking events, delivery confirmation, RMA or
  returns (§13.3/§13.4) — T13/R2. `MERCHANT_SHIPPED` is merchant attestation only.
- No built-in carrier tracking URLs until verified (M-2); no tracking import (M-7).
- No buyer notification on shipment (MD10).
- Export capped at 1000 rows without paging (M-5); generic columns, not carrier upload formats (M-6).
- Cash-on-delivery is not modelled; a CVS carrier label here never implies COD collection.
- (A1) CSV leading zeroes survive only when the spreadsheet imports the column as text (§5.3).

## Integrator rulings (2026-09-29, binding; supersede the defaults in §0.1)

- M-1 Names accepted; creator grant via `0065_owner_provisioning.sql` (see stripe-refund-v1 rulings).
- M-2 Merchant-supplied https tracking URL; no built-in templates until verified.
- M-3 **Changed:** carrier codes add `hilife_cvs` (萊爾富) and `okmart_cvs` (OK mart) — the launch
  market is Taiwan and buyers pick among the four CVS chains.
- M-4 Server time only. M-5 1000-row cap with a truncation flag. M-6 Generic CSV columns.
- M-7 Bulk tracking-number CSV import is deferred **but is the first R1 follow-up** (live sessions
  produce many orders at once); design it as `manual-fulfilment-import-v1` after MF gates pass.
- M-8 Accepted. M-9 Migration 0062 (refund) before 0063 (fulfilment).

## 9. Round-1 review map (A1)

| Finding | Resolution |
| --- | --- |
| P1 checkout_writer grants (record_manual_shipment) | §4 grants bullet (reuses stripe-refund-v1 §4.5 rows 1–3 after A2), MF02 |
| P1 refund reviews flip orders to PAID_ALLOCATION_FAILED / trigger table unspecified | E7 (fix lives in stripe-refund-v1 §4.4), §2 trigger on both tables, MF02 |
| P1 merchant projection validator | §5.1 invariants note, §7 owner, MF05 |
| P1 MD6 ignores post-capture reviews | MD6, §4.1 `unshipped`/export predicate, E8 grant, MF03 |
| P1 permission migrations / 0065 | §4 SQL comment, MD8, MF02; 0065 defined in stripe-refund-v1 §12 (OP01) |
| P2 carrier codes (M-3) | §3.1, §4 CHECK, MF01 |
| P2 CSV guard / phone / leading zeroes | §5.3, §8, MF01 |
| P2 void body | §5.1, MF01/MF05 |

## 10. Round-2 review map (A2)

| Finding | Resolution |
| --- | --- |
| P1 schema USAGE: checkout_writer lacks identity/ops (record_manual_shipment 42501); commerce_auth lacks ops (export audit insert 42501) | Verified against 0001:84, 0013:9, 0016:84, 0027:56, 0060:45. §4 grants bullet (A2), stripe-refund-v1 §4.5 USAGE row, MF02 |
