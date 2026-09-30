# Stripe refund v1 — merchant-initiated full/partial refund of a captured Stripe payment

Status: **FROZEN 2026-09-29** after review rounds 1–3 (fresh reviewer round 3: PASS_WITH_P2). Was DRAFT (amended round 2). Round-1
adversarial review `output/contract-review/refund-fulfilment-round1.json` (BLOCK) is addressed by
the in-place edits marked "(A1)" and by §4.5–§4.7 and §12; see §13 for the finding map. The round-2
fresh review (one P1: schema USAGE and RLS policies missing from §4.5) is addressed by the edits
marked "(A2)"; see §14.
Evidence label for this file: DESIGN. Every RF gate below is NOT_RUN. Nothing here authorizes a
real refund: SANDBOX refunds are test-mode objects only, LIVE stays refused (I16/I17).

Extends [stripe-psp-v1](stripe-psp-v1.md) (§0.1/§0.2 binding; §0.2 overrides its older
single-account text), [payment-capture-v1](payment-capture-v1.md) and
[merchant-orders-v1](merchant-orders-v1.md). Architecture §11.5, §12.1, §12.3, §12.4 and invariants
I01, I02, I04, I05, I06, I13, I14, I18, I20, I23, I24 are authoritative; where they conflict with
this file, this file is wrong. Stage B1 (0061, post_river 0012) must be merged and frozen before
any implementation of this file starts (upstream-interface rule, AGENTS.md).

## 0. Owner inputs, decisions and rulings needed

Owner inputs already recorded: card only, automatic capture (stripe-psp D6/Q3); one Stripe account
per store, direct charges, no Connect (§0.1); late payment after closure is a manual-refund
obligation `CLOSURE_CONTRADICTED` + `REVIEW_REQUIRED` that "the later refund contract consumes"
(§0.2, D13). No owner input exists yet on refund permission, dual control or refund reasons.

| # | Decision | Why / risk closed |
| --- | --- | --- |
| RD1 | **Scope:** refund of one `stripe_checkout` attempt that has a `CAPTURED` fact, by amount, full or partial, many partial refunds allowed. PAYUNi refunds, disputes, payouts, balance transactions and reconciliation cases (§12.4) are out. | Smallest loop a merchant needs; PAYUNi exposes only its last refund record (capture-v1), so it needs its own contract. |
| RD2 | **Reuse, no parallel engine.** A refund is one `integration.operations` row (new actor family `PAYMENT_REFUND`, `provider='stripe'`, `action='stripe.refund'`), whose id equals the refund id, UNKNOWN from birth with reconcile leases, exactly like the Stripe checkout op. Observations reuse `payments.provider_observations`; review uses `payments.review_cases`; wake-ups reuse `payments.stripe_signals` + `payment_signal_v1`; reconcile reuses `payment_reconcile_v1` → `payments.apply_capture` dispatcher. New: two tables (§5.2) and one River kind `payment_refund_v1`. | AGENTS.md: no second trading engine. Leases/generations (I14) and the lease-fenced credential loader already exist. |
| RD3 | **Capacity under the order lock (§12.3).** `request_stripe_refund` locks the order row `FOR UPDATE` (the order is the fund bucket; same first lock as capture), then requires `held + amount ≤ captured`, where `captured` = the attempt's `CAPTURED` fact amount and `held` = Σ amount of this attempt's refunds that have **no** `FAILED`, `CANCELED` or `REJECTED` refund fact. REQUESTED, SUBMITTING, PENDING, UNKNOWN and SUCCEEDED all hold capacity. | Pending/unknown refunds occupy capacity and are never released by a timeout, so a second refund cannot be sent against the same money (§12.3). |
| RD4 | **Capacity is released only by provider or local proof:** a Stripe-retrieved `failed`/`canceled` status, a first-send definitive rejection (stripe-psp §5.6/§10 rules), or a LOCAL suppression before any send. Never by elapsed time, 404, 5xx or a missing webhook. | Mirrors stripe-psp D5 for stock. |
| RD5 | **One create Idempotency-Key per refund forever:** `lc:stripe:refund:v1:<refund uuid>`, same key and byte-identical body for every resend, only while `now < requested_at + 20 h` (below Stripe's ≥24 h key pruning, F4). After the window: list refunds of the PaymentIntent and match `metadata.lc_refund`; one match pins; zero matches stays UNKNOWN with review `REFUND_UNRESOLVED` and capacity held. | I06/I20. A new key after an uncertain send could create a second refund. |
| RD6 | **No stock, order or fulfilment side effect.** A refund never writes `inventory.ledger`, reservations, `commercial_state`, `fulfillment_state` or `payment_work_items`. Return, restock, cancellation and reshipment are separate actions (§12.3 last paragraph, I13). | Refund ≠ return ≠ restock. A full refund of an unshipped order leaves the allocation in place until a separate cancel/restock contract exists (ruling R-3). |
| RD7 | **Payment state is derived, never stored:** `REFUNDED` when Σ SUCCEEDED-and-not-FAILED/CANCELED = captured; `PARTIALLY_REFUNDED` when > 0. A PENDING refund does not change `payment_state` (request accepted ≠ buyer refunded, §12.3). | §12.1 state set; I13. |
| RD8 | **Webhook is a wake-up, never financial authority** (stripe-psp D8). Refund facts come only from an authenticated retrieve recorded by the worker and applied in PG. | Same as checkout. |
| RD9 | **External refunds are detected, not trusted.** (A1) Before every first send the refund worker retrieves the PaymentIntent with `expand[]=latest_charge` and records it through `integration.record_stripe_charge_observation` **under the refund-op lease** with `Via='presend'` and `RefundRef` set. That function, in the same transaction, compares `AmountRefunded` with Σ amount of this attempt's refunds that have `first_sent_at IS NOT NULL` and no `FAILED`/`CANCELED`/`REJECTED` fact; if `AmountRefunded` is larger it sets this refund's `suppressed_at`, inserts a LOCAL `unsent` refund observation plus its `payment_reconcile_v1` job (→ REJECTED `external_refund_detected`) and inserts review `REFUND_HISTORY` (existing reason). `mark_stripe_refund_sent` returns `SEND` only if a `presend` charge observation for this refund exists under the current op generation, recorded ≤ 60 s ago, that did not suppress, and no `REFUND_HISTORY` or `CONFLICTING_REPORT` review exists on the attempt. `charge.refunded` signals are recorded with `Via='retrieve'` and applied asynchronously by `apply_stripe_charge` (review only). New requests are refused while `REFUND_HISTORY` exists. | Prevents a double refund when the merchant also refunded in the Stripe Dashboard; the check and the suppression are synchronous with the send decision. |
| RD10 | **Separate permission `payments:refund`**, never implied by `orders:read` or owner status alone; not backfilled. (A1) 0062 adds no grant; the store creator receives it only through `0065_owner_provisioning.sql` (§12, ruling R-1). | §12.3: refund approval authority is independent of general support permission. |
| RD11 | **`REQUESTED → APPROVED` is collapsed** in v1: the requester holding `payments:refund` is the approver, recorded once in the same transaction. Dual control is a ruling (R-2). | Single-merchant R1; the audit row still names the principal. |
| RD12 | **Reasons:** `requested_by_customer` or `duplicate` only. `fraudulent` is not offered because Stripe then adds the card and email to Radar block lists (F-R2). | A merchant-side mis-click must not block a buyer; ruling R-4. |
| RD13 | **Credential:** (A1) every refund call (create, resend, retrieve, list, PI retrieve) uses the account's **current head** `integration.merchant_accounts.credential_version` read by `load_stripe_refund` at claim time; `merchant_accounts.account_id` must equal `stripe_refunds.account_id` (= the attempt's frozen account) and `VerifyAccount` must pass, else no call. `stripe_refunds.credential_version` records the head at request time for audit only. The version actually used is recorded as `KeyVersion` in every report. It never uses a different account's key. | A rotated-away key may be revoked, which would strand a SUBMITTING refund; the design assumes Stripe idempotency keys are scoped per account, not per API key (NOT verified; RF10 must prove a same-key replay under a rotated key on the same account returns the same `re_…`, otherwise resends stay on the request-time version and this row reverts). Ruling R-5 differs from §0.2's frozen-version rule for checkout. |

Rejected alternatives:
- Refunding through the Stripe Dashboard and importing results: no idempotency, no capacity lock, no audit.
- A mutable `refund.status` updated from webhook payloads: last-message-wins money (§12.2).
- Releasing capacity when an UNKNOWN refund times out: permits double refund (§12.3).
- Refund automatically restocking or cancelling the order: conflates I13 states.
- A new job family per event type, or a separate refund worker process: the payment worker already
  serves the profile queue and Stripe runtime.
- Reusing `payments.facts` for refunds: its PK is `(attempt, kind)`, so it cannot hold several partial
  refunds; §12.4 names `RefundFact` separately.

### 0.1 Integrator ruling needed (nothing below is assumed silently)

- **R-1** Permission name `payments:refund`; whether `create_initial_store` grants it to new store
  creators (default in this draft: **no**, explicit provisioning like `live:manage`).
- **R-2** Dual control (`REQUESTED → APPROVED` by a second principal) or single-actor v1 (draft: single).
- **R-3** Full refund of a CONFIRMED, unshipped order: leave allocation (draft) or add a separate
  merchant cancel-and-release operation in this unit.
- **R-4** Exclude `fraudulent` reason (draft: excluded).
- **R-5** Credential for refunds: current head of the same account (draft) vs the attempt's frozen
  version (§0.2 rule for checkout). The draft freezes the chosen version into the refund row.
- **R-6** Restricted-key permissions: stage-A SP16 records the least RAK set for Checkout; refunds need
  Refunds write and PaymentIntents/Charges read. Registrar qualification must prove it (RF10).
- **R-7** Which refunds are allowed when reviews exist (draft §4.2 table).
- **R-8** Long-tail failure detection: Stripe may fail a `succeeded` card refund up to ~30 days later
  (F-R3). Draft relies on `refund.failed` webhooks plus a merchant refresh; no 30-day poller. Accept?
- **R-9** Whether the merchant detail may show the Stripe refund id (`re_…`) for Dashboard tracing
  (draft: no, consistent with MOR "no PSP reference").
- **R-10** SANDBOX refund probe needs a paid test PaymentIntent. Draft: RF10 creates one with
  `payment_method=pm_card_visa` in test mode from the test harness only (not product code). Allowed?
- **R-11** Post-River file number: draft uses `migrations/post_river/0013_stripe_refund.sql`.
- **R-12** Webhook endpoint subscription: the registrar/runbook must add the four refund event types
  to each per-store endpoint (stripe-psp Q8 owner of endpoint creation still open).

## 1. Stripe facts relied on (WebFetch of docs.stripe.com, retrieved 2026-09-28 UTC)

| # | Fact | Source | Status |
| --- | --- | --- | --- |
| F-R1 | `POST /v1/refunds` needs a Charge or PaymentIntent. Optional `amount` is "a positive integer in the smallest currency unit" and "Can refund only up to the remaining, unrefunded amount of the charge"; omitted = full remaining. Partial refunds can repeat "until the entire charge has been refunded". Refunding an already-refunded charge, or more than is left, raises an error. | [create](https://docs.stripe.com/api/refunds/create) | VERIFIED |
| F-R2 | `reason` ∈ `duplicate`, `fraudulent`, `requested_by_customer`; `fraudulent` adds the card and email to block lists. `refund_application_fee`/`reverse_transfer` are Connect-only; `instructions_email`/`origin` are for non-card or customer-balance refunds. | create | VERIFIED |
| F-R3 | Refund `status` ∈ `pending`, `requires_action`, `succeeded`, `failed`, `canceled`. `failure_reason` ∈ `lost_or_stolen_card`, `expired_or_canceled_card`, `charge_for_pending_refund_disputed`, `insufficient_funds`, `declined`, `merchant_request`, `unknown`. `pending_reason` ∈ `processing`, `insufficient_funds`, `charge_pending`. A refund can fail after being submitted; the bank returns funds, which "can take up to 30 days". Cancellation is a type of failure and carries `failure_reason`. Card refund cancellation is Dashboard-only. | [object](https://docs.stripe.com/api/refunds/object), [refunds guide](https://docs.stripe.com/refunds) | VERIFIED |
| F-R4 | Refunds use available balance; if it is insufficient, card refunds are held `pending` until the balance suffices (other methods fail). `requires_action` applies to methods without native refund support (not card). | refunds guide | VERIFIED |
| F-R5 | Events: `refund.created`, `refund.updated` (incl. ARN reference), `refund.failed` (data.object = refund); `charge.refunded` (data.object = charge, "including partial refunds"). `charge.refund.updated` is deprecated. | [event types](https://docs.stripe.com/api/events/types), refunds guide | VERIFIED |
| F-R6 | `GET /v1/refunds?payment_intent=…` returns refunds newest first, `limit` 1..100, `starting_after`/`ending_before` cursors. `GET /v1/refunds/{id}` retrieves one. | [list](https://docs.stripe.com/api/refunds/list) | VERIFIED |
| F-R7 | Charge `amount_captured`, `amount_refunded` ("can be less than the amount … if a partial refund was issued"), `refunded` (true only when fully refunded), `disputed`. | [charge object](https://docs.stripe.com/api/charges/object) | VERIFIED |
| F-R8 | The customer typically sees a card refund "approximately 5-10 business days later"; early refunds may appear as a reversal instead. | refunds guide | VERIFIED |
| F4 | Idempotency semantics (param compare, cached 500, ≥24 h pruning, 429/most 400/401 before idempotency). | stripe-psp §1 | VERIFIED there, reused |

Not verified; the design treats each as possible: whether idempotency keys survive an API-key
rotation on the same account (RD13, RF10); whether `refund.created` fires before the create
response returns; the exact RAK permission names for refunds (R-6); whether a `pending` card refund
for insufficient balance has an expiry (docs imply `insufficient_funds` failure "crossed the pending
refund expiry window" without a duration).

## 2. Flow

```
Merchant admin ─POST /api/admin/.../orders/{id}/refunds (Idempotency-Key)─► BFF ─► API (runtime pool
  = platform.OpenPool, role commerce_runtime; River InsertTx into river_payment, grants §4.5)
  one tx, no I/O: auth(payments:refund) → lock order FOR UPDATE → CAPTURED fact + stripe_sessions pins
  → capacity check (RD3) → stripe_refunds row + op(UNKNOWN, stripe.refund) + audit + command result
  + River payment_refund_v1 (ScheduledAt=now) → COMMIT → 201 {state: REQUESTED}
Worker payment_refund_v1 ─claim refund op─► load_stripe_refund (lease-fenced, API material)
  first send only: GET /v1/payment_intents/{pi}?expand[]=latest_charge → record charge obs
     (Via=presend, same tx compares; A1) external refund → suppressed_at + LOCAL unsent → REJECTED
     (capacity released) + REFUND_HISTORY; clean → mark_sent may return SEND
  mark_stripe_refund_sent (commit) → POST /v1/refunds (key lc:stripe:refund:v1:<refund>)
  → record refund obs (pin re_… set-once) + payment_reconcile_v1 → snooze per status
CaptureWorker payment_reconcile_v1 ─► payments.apply_capture dispatcher (report Object=refund|charge)
  → payments.apply_stripe_refund: lock order → refund facts / reviews (idempotent)
Stripe ─refund.* / charge.refunded─► POST /v1/stripe/webhook/{endpoint_id} → receipt + signal
  (refund_id for refund objects; attempt-level for charge objects) → payment_signal_v1 → retrieve
```

## 3. Wire adapter additions (`internal/integrations/psp/stripe`, stage-A package)

Same package rules as stripe-psp §5.1 (stdlib only, pinned `Stripe-Version`, fixed errors, no
logging, docs URL + retrieval date on every wire constant, redacted `String()`).

- `CreateRefund(ctx, RefundParams) (Refund, CallMeta, error)`; `RefundParams{PaymentIntentID,
  AmountMinor, Currency, Reason, RefundRef, AttemptRef}`. The form body has **exactly** the keys
  `payment_intent`, `amount`, `reason`, `metadata[lc_refund]`, `metadata[lc_attempt]`, sorted and
  deterministic; any other key is `ErrInvalid`. `amount` always present (never "omit for full").
  `Idempotency-Key = RefundIdempotencyKey(refund)`.
- `RetrieveRefund(ctx, id)`, `ListRefunds(ctx, paymentIntentID, startingAfter)` (limit 100; ≤10 pages
  per call, more is `ErrUncertain`, never "not found"), `RetrievePaymentCharge(ctx, pi)` =
  `GET /v1/payment_intents/{pi}?expand[]=latest_charge` returning only the fields in §3.1.
- Classification reuses stripe-psp §5.6 unchanged; create meaning is "definitive only on the first
  send", identical to checkout create.
- Webhook verifier projection (§5.8) adds `data.object.payment_intent`, `data.object.metadata.lc_refund`
  and admits `data.object.object ∈ {checkout.session, refund, charge}`. Nothing else is read.

### 3.1 Observation projections (`payments.provider_observations.report`, ≤2048 bytes, exact keys)

Refund report: `Provider`="stripe", `Version`=1, `Object`="refund", `Via` ∈
{`create`,`retrieve`,`list`,`unsent`,`escalate`}, `AccountID`, `KeyVersion`, `RequestID`, `SendCount`,
`RefundRef` (our uuid), `AttemptRef`, `RefundID`, `Status`, `FailureReason`, `PendingReason`,
`Amount?`, `Currency`, `PaymentIntentID`, `Livemode`, `MetadataRefund`, `MetadataAttempt`,
`ErrorClass` ("" | "rejected"), `ErrorCode`, `HTTPStatus`, `ListMatchCount?`, `LocalReason`.

Charge report: `Provider`="stripe", `Version`=1, `Object`="charge", `Via` ∈ {`presend`,`retrieve`}
(A1), `AccountID`, `KeyVersion`, `RequestID`, `RefundRef` (A1: our refund uuid when `Via='presend'`,
else ""), `PaymentIntentID`, `ChargeID`, `Currency`, `AmountCaptured?`, `AmountRefunded?`, `Refunded`,
`Disputed`, `Livemode`, `LocalReason`.

Never stored: destination_details (ARN), card data, `receipt_url`, billing details, emails, error
messages, raw JSON. `provider_observations.first_generation` is the refund op's generation for
refund reports and the attempt op's generation for charge reports recorded by SignalWorker.

## 4. Persistence: `migrations/0062_stripe_refund.sql` + `migrations/post_river/0013_stripe_refund.sql`

### 4.1 Widened constraints (0062)

- `identity.store_grants` permission CHECK: re-derive the current `store_grants_permission_check` via
  `pg_get_constraintdef` (today the 0033 list) and add `payments:refund`. No grant rows (§12).
- `integration.operations.operation_actor_family`: re-derive from `pg_get_constraintdef` (keeping
  MERCHANT, BUYER_PAYMENT_QUERY incl. the 0061 Stripe disjunct, MEDIA_ATTEMPT) and add
  `(actor_kind='PAYMENT_REFUND' AND principal_id IS NOT NULL AND media_attempt_id IS NULL
  AND payment_attempt_id IS NOT NULL AND payment_attempt_id<>id AND buyer_owner_id IS NOT NULL
  AND buyer_session_id IS NOT NULL AND provider='stripe' AND action='stripe.refund'
  AND purpose='transactional' AND state NOT IN ('READY','DISPATCHING','BLOCKED_POLICY','STALE_BINDING')
  AND lease_mode<>'dispatch')`. Buyer owner/session are copied from the attempt so the existing
  composite `operation_payment_attempt_fk` is enforced (MATCH SIMPLE would skip it with NULLs).
- `payments.review_cases.reason` adds `REFUND_UNRESOLVED`, `REFUND_AMOUNT_MISMATCH`,
  `REFUND_CONFLICTING` (`REFUND_HISTORY` already exists and is reused for external refunds).
- `payments.stripe_signals`: add `refund_id uuid NULL` with composite FK (A1)
  `(tenant_id,store_id,attempt_id,refund_id) REFERENCES payments.stripe_refunds(tenant_id,store_id,attempt_id,id)`
  and `source` value `MERCHANT_REFRESH`; CHECK `(source='MERCHANT_REFRESH') <= (refund_id IS NOT NULL)`.
  `stripe_signals.attempt_id` is always the attempt. A signal with `refund_id` carries the **refund op
  id** as its job `operation_id`; one without keeps the checkout meaning (job `operation_id` = attempt).
- `payments.stripe_webhook_receipts`: `object_type` admits `refund`, `charge`; `reason` adds
  `unknown_refund`, `unknown_charge`; add nullable immutable `refund_id` (set only for `object_type='refund'`).
- (A1) `payments.stripe_sessions`: add `charge_signal_count integer NOT NULL DEFAULT 0 CHECK(charge_signal_count
  BETWEEN 0 AND 64)`, integration_writer `UPDATE(charge_signal_count)`. Refund traffic never touches
  the checkout `signal_count` (§7.3).
- (A1) `checkout.payment_attempts`: add `UNIQUE(tenant_id,store_id,owner_id,order_id,id)` (superset of
  the PK; target of the `stripe_refunds` order/attempt FK).
- `payments.provider_observations`: no DDL change; the refund/charge report shapes are validated by
  the new record definers (the checkout record function keeps rejecting them).

### 4.2 New tables

```sql
CREATE TABLE payments.stripe_refunds (
 tenant_id uuid NOT NULL, store_id uuid NOT NULL, id uuid PRIMARY KEY,           -- = operations.id
 attempt_id uuid NOT NULL, order_id uuid NOT NULL, owner_id uuid NOT NULL,
 principal_id uuid NOT NULL,                                                     -- requester = approver (RD11)
 environment text NOT NULL CHECK(environment='SANDBOX'),                          -- LIVE refused in v1
 account_id text NOT NULL CHECK(account_id ~ '^acct_[A-Za-z0-9]{1,59}$'),
 credential_version bigint NOT NULL CHECK(credential_version>0),                  -- R-5
 payment_intent_id text NOT NULL CHECK(payment_intent_id ~ '^[A-Za-z0-9_]{1,255}$'),
 currency text NOT NULL CHECK(currency ~ '^[A-Z]{3}$'),
 amount_minor bigint NOT NULL CHECK(amount_minor BETWEEN 1 AND 999999999999),
 reason text NOT NULL CHECK(reason IN ('requested_by_customer','duplicate')),
 create_params jsonb NOT NULL CHECK(jsonb_typeof(create_params)='object' AND octet_length(create_params::text)<=2048),
 requested_at timestamptz NOT NULL, resend_until timestamptz NOT NULL,
 first_sent_at timestamptz, last_sent_at timestamptz,
 send_count integer NOT NULL DEFAULT 0 CHECK(send_count BETWEEN 0 AND 200),
 body_sha256 bytea CHECK(body_sha256 IS NULL OR octet_length(body_sha256)=32),
 suppressed_at timestamptz,
 stripe_refund_id text UNIQUE CHECK(stripe_refund_id ~ '^[A-Za-z0-9_]{1,255}$'), pinned_at timestamptz,
 refresh_count integer NOT NULL DEFAULT 0 CHECK(refresh_count BETWEEN 0 AND 30), last_refresh_at timestamptz,
 signal_count integer NOT NULL DEFAULT 0 CHECK(signal_count BETWEEN 0 AND 64),
 CHECK(resend_until=requested_at+interval '20 hours'),
 CHECK((stripe_refund_id IS NULL)=(pinned_at IS NULL)),
 CHECK((first_sent_at IS NULL)=(send_count=0) AND (first_sent_at IS NULL)=(body_sha256 IS NULL)
   AND (first_sent_at IS NULL)=(last_sent_at IS NULL)),
 CHECK(suppressed_at IS NULL OR first_sent_at IS NULL),
 CHECK(first_sent_at IS NULL OR first_sent_at < resend_until - interval '1 hour'), -- A1: list closure never races a late first send
 CHECK(payments.stripe_refund_amount_ok(currency,amount_minor)),                  -- step rule below
 UNIQUE(tenant_id,store_id,attempt_id,id),                                         -- A1: composite FK target
 FOREIGN KEY(tenant_id,store_id,owner_id,order_id,attempt_id)                      -- A1: order tied to attempt
  REFERENCES checkout.payment_attempts(tenant_id,store_id,owner_id,order_id,id),
 FOREIGN KEY(tenant_id,store_id,id) REFERENCES integration.operations(tenant_id,store_id,id),
 FOREIGN KEY(tenant_id,principal_id) REFERENCES identity.memberships(tenant_id,principal_id)
);
CREATE TABLE payments.refund_facts (
 tenant_id uuid NOT NULL, store_id uuid NOT NULL, refund_id uuid NOT NULL,
 attempt_id uuid NOT NULL,
 FOREIGN KEY(tenant_id,store_id,attempt_id,refund_id)                              -- A1: same attempt
  REFERENCES payments.stripe_refunds(tenant_id,store_id,attempt_id,id),
 kind text NOT NULL CHECK(kind IN ('SUCCEEDED','FAILED','CANCELED','REJECTED')),
 amount_minor bigint NOT NULL CHECK(amount_minor>=0), currency text NOT NULL CHECK(currency ~ '^[A-Z]{3}$'),
 stripe_refund_id text CHECK(stripe_refund_id ~ '^[A-Za-z0-9_]{1,255}$'),
 failure_reason text CHECK(failure_reason IN ('lost_or_stolen_card','expired_or_canceled_card',
   'charge_for_pending_refund_disputed','insufficient_funds','declined','merchant_request','unknown',
   'first_send_rejected','external_refund_detected','send_window_closed')),         -- A1: last value
 source_report_hash bytea NOT NULL CHECK(octet_length(source_report_hash)=32),
 received_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 PRIMARY KEY(tenant_id,store_id,refund_id,kind),
 CHECK((kind='REJECTED')=(stripe_refund_id IS NULL)),
 FOREIGN KEY(tenant_id,store_id,attempt_id,source_report_hash)
  REFERENCES payments.provider_observations(tenant_id,store_id,attempt_id,report_hash)
);
```

- **Amount step:** TWD refunds must be multiples of 100 minor units (whole NT$, stripe-psp §4); other
  admitted currencies step 1. `payments.stripe_refund_amount_ok(currency, amount)` is IMMUTABLE and
  uses the §0.2 currency table without its minimum (no refund minimum is documented; Stripe's own 400
  on the first send is definitive). Go and SQL parity is RF01.
- **Set-once trigger** `payments.guard_stripe_refund()`: frozen columns never change; markers and
  pins only NULL→value; counters monotone; `stripe_refund_id` also becomes the op `provider_reference`.
- **Fact trigger** `payments.guard_refund_fact()`: `REJECTED` excludes every other kind and requires no
  pin; `SUCCEEDED`/`FAILED`/`CANCELED` require the pinned id; `FAILED` or `CANCELED` may follow
  `SUCCEEDED` (late failure, F-R3); `SUCCEEDED` after `FAILED`/`CANCELED` is refused (→ review
  `REFUND_CONFLICTING` instead); `SUCCEEDED.amount_minor` must equal the refund's amount (I05; a
  mismatch goes to review `REFUND_AMOUNT_MISMATCH`, no fact).
- Append-only facts; refunds updatable only through the guarded columns. Bounded: at most 20 refunds
  per attempt (I23), enforced in the request function.
- **Roles/RLS:** both tables FORCE RLS, PUBLIC revoked, no direct runtime login grants.
  `commerce_checkout_writer` owns request/apply definers: SELECT, INSERT on both tables,
  `UPDATE(refresh_count,last_refresh_at,signal_count)` on refunds. `commerce_integration_writer`: SELECT
  and `UPDATE(first_sent_at,last_sent_at,send_count,body_sha256,suppressed_at,stripe_refund_id,
  pinned_at,signal_count)` on refunds, and (A1) SELECT on `refund_facts` for the §RD9 pre-send
  comparison. `commerce_auth` gets column SELECT for the merchant read (§7).
  `commerce_checkout_runtime` gets SELECT on neither table; the buyer view reads through the existing
  `hosted_payment_view_v2` definer successor (§7.2). (A2) Because both tables FORCE RLS, every role
  above also needs an explicit policy (a missing policy returns zero rows without an error); the
  policies are in §4.5. Every other grant and policy change is listed exactly in §4.5 (A1).

### 4.3 Which captured payments may be refunded (R-7)

| Condition on the attempt | Allowed? |
| --- | --- |
| provider `stripe`, `CAPTURED` fact, pinned `payment_intent_id`, no review | yes, full or partial |
| review `PROVIDER_PRESENTMENT_DRIFT` only | yes (money matched, I05) |
| `CLOSURE_CONTRADICTED` / `PAID_ALLOCATION_FAILED` (late payment obligation, D13) | yes, **full remaining only**; this consumes the §0.2 manual-refund obligation. The review is not cleared; the read model shows `refunded_minor` = captured. |
| review `PROVIDER_AMOUNT_MISMATCH`, `PROVIDER_IDENTITY_MISMATCH`, `PROVIDER_SESSION_DUPLICATE`, `CONFLICTING_REPORT`, `REFUND_HISTORY`, `REFUND_UNRESOLVED`, `REFUND_AMOUNT_MISMATCH`, `REFUND_CONFLICTING`, or any other reason | no (`refund_blocked_review`); operator/Dashboard path, then manual resolution (NOT_IMPLEMENTED) |
| no `CAPTURED` fact, PAYUNi, LIVE environment | no |

(A1) **Combination rule (binding over the rows above):** a refund is allowed iff every review reason on
the attempt ∈ {`PROVIDER_PRESENTMENT_DRIFT`, `CLOSURE_CONTRADICTED`, `PAID_ALLOCATION_FAILED`}; it is
full-remaining-only iff `CLOSURE_CONTRADICTED` or `PAID_ALLOCATION_FAILED` is present; otherwise
`refund_blocked_review`. The request also refuses (`refund_blocked_review`) while any refund of the
attempt has `suppressed_at IS NOT NULL` without a `REJECTED` fact (suppression recorded, apply pending).

### 4.4 SQL entry points

| Function | Phase | Owner | EXECUTE | Contract |
| --- | --- | --- | --- | --- |
| `payments.request_stripe_refund(hash bytea, store uuid, order uuid, key text, request_hash bytea, amount bigint, reason text, expected_refundable bigint, refund uuid, job bigint) RETURNS jsonb` | post_river | checkout_writer | `commerce_runtime` | Merchant token auth with `payments:refund` using the 0027 fresh-final-auth pattern (READ COMMITTED, `clock_timestamp()` expiry; PT401/403/404 distinctions). Replays `ops.command_results` (`operation='payments.refund.request'`, I02). Locks order → reads attempt, CAPTURED fact, reviews, `stripe_sessions.payment_intent_id`, account/current credential. Requires §4.3, step rule, ≤20 refunds, `expected_refundable = captured − held` (else PT409 `refundable_changed`), `held + amount ≤ captured` (else PT422 `exceeds_refundable`). Verifies the exact `payment_refund_v1` job, inserts op (UNKNOWN, reconcile), refund row, operation event, `ops.audit_events` `payments.refund_requested`, command result. Returns `{refund_id,state,amount_minor,currency,refundable_minor}`. No provider I/O. (A1) After the first `resolve_access` and the order lock it sets `app.tenant_id`, `app.store_id`, `app.principal_id` from the auth result and `app.buyer_id`/`app.buyer_session_id` from the locked order's `owner_id`/`creator_session_id` (the `apply_stripe_observation` pattern) before reading `payment_attempts`, `stripe_sessions`, `merchant_accounts` or inserting. The op row copies `binding_id`, `binding_version`, `external_asset_id`, `buyer_owner_id`, `buyer_session_id` from the attempt's op, with `request = {"refund_id":…,"attempt_id":…}`, `request_hash = sha256(request::text)`, `job_id` = the verified job, `semantic_key` per §8. |
| `payments.request_stripe_refund_refresh(hash, store, order, refund, signal uuid, job bigint) RETURNS jsonb` | post_river | checkout_writer | runtime | `payments:refund`; same auth, fresh final auth and GUC rules as request (A1); throttled `now ≥ last_refresh_at+10 s`, ≤30; not after a terminal fact other than SUCCEEDED (late-failure check allowed). Inserts `stripe_signals(source MERCHANT_REFRESH, refund_id, attempt_id = the attempt)` whose job has `operation_id` = the refund id; `{scheduled:false}` means roll back the InsertTx. |
| (A1) `integration.require_stripe_refund(uuid,bigint,bytea,text) RETURNS payments.stripe_refunds` | 0062 | integration_writer | none | Private fence, the refund analogue of `require_stripe_query`: locks the binding `FOR SHARE` then the op `FOR UPDATE`; requires `actor_kind='PAYMENT_REFUND'`, `action='stripe.refund'`, the attempt's `execution_profile` = profile, `state='UNKNOWN'`, `lease_mode='reconcile'`, generation, unexpired lease and token hash. Used by every refund-op-leased function below. |
| `integration.load_stripe_refund(uuid,bigint,bytea,text) RETURNS TABLE(...)` | 0062 | integration_writer | worker | Fenced by `require_stripe_refund`; returns frozen refund, attempt scope, account, (A1) the account's **current head** credential (`merchant_accounts.credential_version` and that version's `key_id/nonce/ciphertext`, `merchant_accounts.account_id` = refund `account_id` else PT409), markers, pins, latest status, terminal flags, DB now. |
| `integration.mark_stripe_refund_sent(uuid,bigint,bytea,text,bytea) RETURNS text` | 0062 | integration_writer | worker | `SEND` only if first send, not suppressed, no REJECTED fact, `now < resend_until - interval '1 hour'` (A1), a `Via='presend'` charge observation for this refund exists with `first_generation` = current op generation and `received_at ≥ now - 60 s` that did not suppress, and no `REFUND_HISTORY`/`CONFLICTING_REPORT` review on the attempt (A1); `RESEND` (same body hash, `now < resend_until`); `CLOSED` otherwise. A never-sent refund that gets `CLOSED` because the send window passed is suppressed by the worker with a LOCAL `unsent` report (`LocalReason='send_window_closed'`) → REJECTED. Commits before the POST. |
| `integration.record_stripe_refund_observation(uuid,bigint,bytea,text,jsonb,bigint) RETURNS void` | post_river | integration_writer | worker | Exact §3.1 refund keys; identity (`MetadataRefund`=`RefundRef`=refund, `MetadataAttempt`=attempt, `PaymentIntentID`, `AccountID`, `Livemode=false`, non-empty `Currency`) or it raises and the op finishes UNKNOWN `stripe_refund_mismatch`; a non-empty `Currency` that differs from the request is recorded (not raised) and completes the op as `stripe_refund_mismatch` so apply step 4 opens `REFUND_AMOUNT_MISMATCH` (ruling 23, see the amendment at the end). Pins `stripe_refund_id` set-once; LOCAL `unsent` sets `suppressed_at`. Hash-dedup; verifies the `payment_reconcile_v1` job whose `operation_id` = the attempt; lease-fenced Complete UNKNOWN. |
| `integration.record_stripe_charge_observation(uuid,bigint,bytea,text,jsonb,bigint) RETURNS boolean` | post_river | integration_writer | worker | Charge report under either the refund op lease (`Via='presend'`, `RefundRef` = the leased refund, fenced by `require_stripe_refund`) or the checkout op lease (`Via='retrieve'`, `RefundRef=""`, fenced by `require_stripe_query`). Same validation style; hash-dedup; inserts the observation (`first_generation` = the leased op's generation) and verifies its `payment_reconcile_v1` job (`operation_id` = the attempt). (A1) For `presend` it then, in the same transaction, runs the RD9 comparison; on excess it sets this never-sent refund's `suppressed_at` and inserts review `REFUND_HISTORY` sourced from this observation, and returns `true` (suppressed); otherwise `false`. The worker then records the LOCAL `unsent` refund report (`LocalReason='external_refund'`) through `record_stripe_refund_observation` in its own transaction; a crash in between is recovered by §6 ("suppressed and never sent → record LOCAL unsent"). |
| `integration.finish_stripe_refund(uuid,bigint,bytea,text,text) RETURNS void` | 0062 | integration_writer | worker | Fixed codes: `stripe_refund_uncertain`, `stripe_retrieve_failed`, `stripe_rate_limited`, `stripe_timeout`, `stripe_panic`, `stripe_record_failed`, `stripe_refund_mismatch`, `stripe_idempotency_alarm`, `stripe_unavailable`, `stripe_budget_exhausted`, `stripe_refund_terminal`. Always UNKNOWN. |
| `payments.apply_capture(uuid,bytea)` dispatcher | 0062 | checkout_writer | worker (existing) | `CREATE OR REPLACE` of the 0061 dispatcher: additionally routes `report->>'Object'='refund'` to `payments.apply_stripe_refund` and `'charge'` to `payments.apply_stripe_charge`; the checkout routing is otherwise unchanged. |
| (A1) `payments.apply_stripe_observation(uuid,bytea)` | 0062 | checkout_writer | none | `CREATE OR REPLACE` of the 0061 body with **one** change: the post-capture review branch (`IF v_review THEN … UPDATE checkout.orders SET fulfillment_state='PAID_ALLOCATION_FAILED' … INSERT … payment_work_items … 'REVIEW_REQUIRED' … RETURN`) runs only when `v_new_capture OR v_closed_before`; otherwise the function falls through to the existing `IF NOT v_new_capture OR EXISTS(work item) THEN RETURN`. Consequence: a review inserted after capture (every `REFUND_%` reason, `REFUND_HISTORY`, and `CONFLICTING_REPORT`/`PROVIDER_PRESENTMENT_DRIFT` from a later report) never changes order or work-item state. RF12 records the sha256 of the new body and a diff against 0061 showing only this guard. |
| `payments.apply_stripe_refund(uuid,bytea)`, `payments.apply_stripe_charge(uuid,bytea)` | 0062 | checkout_writer | none | §6. |

**Post-River (0013):** `guard_payment_job_family` admits `payment_refund_v1` with args exactly
`{operation_id, version:1}`; `payment_job_queue` linkage `stripe_refunds.id::text=args.operation_id AND
operations.job_id=j.id`, queue from the attempt's `execution_profile`; `payment_signal_v1` linkage
accepts `s.refund_id::text=args.operation_id` when `refund_id` is set; kind lists in
`route_payment_queue_v1`, `payment_queue_ready` and `reject_legacy_family_job` include the new kind.
Old binaries still route correctly. (A1) `integration.payment_job_queue` resolves the attempt through
`payments.stripe_refunds` when `args->>'operation_id'` is a refund id (for `payment_refund_v1`, and for
`payment_signal_v1` whose signal row has `refund_id` = `args.operation_id` and `attempt_id` = the attempt);
the existing attempt-id branches are unchanged.

### 4.5 Grants and policies (A1; 0062 unless marked post_river)

All definers keep the 0027/0060 fresh-final-auth pattern (first `identity.resolve_access` before any
lock, a fresh `resolve_access` + `identity.sessions` expiry re-check after the last lock).

| Role | Change | Why |
| --- | --- | --- |
| `commerce_checkout_writer` | (A2) `GRANT USAGE ON SCHEMA identity, ops`. It holds USAGE only on buyer, control, storefront, catalog, pricing, fulfillment, inventory, river (0013:9), integration, payments (0016:84) and river_payment/river_expiry (post_river 0005:667); it owns checkout (0013:5). 0060:45 gave `commerce_claims_writer` the identity half for the same reason | without it every `identity.*` and `ops.*` reference in the merchant definers fails with 42501 |
| `commerce_checkout_writer` | `GRANT SELECT(token_hash,principal_id,audience,revoked_at,expires_at) ON identity.sessions`; `GRANT EXECUTE ON FUNCTION identity.resolve_access(bytea,uuid,text)` (with the USAGE row above, the 0060:45 `commerce_claims_writer` merchant-auth set) | merchant auth inside `request_stripe_refund(_refresh)` and `fulfillment.record_manual_shipment` |
| `commerce_checkout_writer` | `GRANT INSERT ON ops.command_results, ops.audit_events`; `GRANT SELECT ON ops.command_results`; RESTRICTIVE + permissive policies `FOR INSERT/SELECT TO commerce_checkout_writer` with `tenant_id`, `store_id` = `app.tenant_id`/`app.store_id` and `principal_id` = `app.principal_id` (the 0002 `command_actor` shape) | replay + audit rows for merchant commands |
| `commerce_checkout_writer` | new policies `checkout_refund_operation ON integration.operations` (ALL: `actor_kind='PAYMENT_REFUND'` AND tenant/store = GUCs AND `principal_id` = `app.principal_id`; SELECT also admits the attempt's `BUYER_PAYMENT_QUERY` op in scope, needed to copy its binding) and `checkout_refund_event ON integration.operation_events FOR INSERT` (op in scope with `actor_kind='PAYMENT_REFUND'`) | the existing `checkout_payment_operation`/`checkout_payment_event` admit only `BUYER_PAYMENT_QUERY` with buyer GUCs |
| `commerce_checkout_writer` | none on `stripe_sessions`, `stripe_signals`, `payment_attempts`: merchant definers set `app.buyer_id`/`app.buyer_session_id` from the locked order (§4.4) so the existing owner-scoped 0016/0061 policies apply | reuse, no widening |
| `commerce_checkout_writer` | (A2) policies `stripe_refund_checkout ON payments.stripe_refunds TO commerce_checkout_writer` and `refund_fact_checkout ON payments.refund_facts TO commerce_checkout_writer`, each `USING`/`WITH CHECK` `tenant_id`,`store_id` = `app.tenant_id`/`app.store_id` (the 0018 `private_writer` shape, 0018:116). The request, refresh, apply and buyer-view definers all set those GUCs before touching either table (as 0061 `apply_stripe_observation` and `hosted_payment_view_v2` already do) | FORCE RLS: without a policy the capacity Σ, replay lookup and buyer view read zero rows |
| `commerce_integration_writer` | SELECT on `payments.stripe_refunds`, `payments.refund_facts`; `INSERT ON payments.review_cases` with policy `WITH CHECK(reason='REFUND_HISTORY')`; SELECT on `payments.review_cases`; `UPDATE(charge_signal_count)` on `stripe_sessions`; the §4.2 `UPDATE(...)` column set on `stripe_refunds` (incl. `suppressed_at`, `signal_count`) | RD9 synchronous pre-send check; §7.3 caps |
| `commerce_integration_writer` | (A2) policies `stripe_refund_integration ON payments.stripe_refunds TO commerce_integration_writer USING(true) WITH CHECK(true)`, `refund_fact_integration ON payments.refund_facts FOR SELECT TO commerce_integration_writer USING(true)`, `review_integration_read ON payments.review_cases FOR SELECT TO commerce_integration_writer USING(true)` (the 0061 `stripe_*_integration` shape, 0061:393–412; its functions are fenced by op lease, not GUCs). `review_cases` is FORCE RLS since 0018:115 and its only SELECT policies are for checkout_writer, runtime (0018) and auth (0027:70) | without them `mark_stripe_refund_sent`'s "no `REFUND_HISTORY`/`CONFLICTING_REPORT`" check always passes (fails open), the RD9 Σ of sent refunds reads 0 so every second partial refund is suppressed with a sticky `REFUND_HISTORY`, and the webhook commit's `stripe_refunds … FOR UPDATE` for the `signal_count` cap finds no row |
| `commerce_runtime` (post_river) | `GRANT USAGE ON SCHEMA river_payment`; `GRANT SELECT, INSERT, UPDATE(kind) ON river_payment.river_job`; `GRANT USAGE ON SEQUENCE river_payment.river_job_id_seq` (the set `commerce_checkout_runtime` holds from post_river 0005) | merchant API pool (`platform.OpenPool`) InsertTx of `payment_refund_v1` and refresh `payment_signal_v1`; `reject_legacy_family_job` keeps payment kinds out of `river.river_job` |
| `commerce_auth` | column SELECT on `stripe_refunds(tenant_id,store_id,attempt_id,id,amount_minor,currency,reason,requested_at,stripe_refund_id,first_sent_at,suppressed_at)` and `refund_facts(tenant_id,store_id,refund_id,kind,received_at,failure_reason)` with `USING(true)` SELECT policies (the 0027 `merchant_order_projection` pattern) | merchant read §7.1 |

`internal/platform` pool validation: the `runtime` authority check must admit the new `river_payment`
privileges and still reject River lifecycle privileges (DELETE, UPDATE of state/attempt/args); RF03
proves both. RF03 and MF02 assert the exact privilege matrix above (each row positive and one
negative per role), including (A2) every schema USAGE row and every policy by name, role, command and
qual, read from `pg_namespace`/`pg_policies`, not inferred from a successful call.

### 4.6 Frozen Stripe B1 functions replaced (A1; post_river 0013 unless marked)

Refund and charge webhooks cannot pass the unchanged B1 ingress/consumer. The following are replaced;
each keeps its owner, ACL, `search_path` and every existing checkout behaviour byte-for-byte except the
stated delta (RF12 records old/new body hashes and the diff):

- `payments.stripe_webhook_prepare` (0062, DROP + CREATE because the return type changes): returns
  `(disposition, receipt_id, attempt_id, session_id, signal_id, refund_id uuid, object_type text)`;
  maps refund/charge objects per §7.3 and sets `receipts.refund_id`. Go ingress is updated in the same
  unit (commerce_worker) to pass `refund_id` into the job args.
- `payments.stripe_webhook_commit` and `payments.guard_stripe_receipt_link`: expected job
  `operation_id` = `coalesce(r.refund_id, r.attempt_id)`; the signal row inserted with
  `attempt_id = r.attempt_id`, `refund_id = r.refund_id`. Cap accounting: `object_type='refund'` →
  `stripe_refunds.signal_count` (≤64, locked `FOR UPDATE`); `object_type='charge'` →
  `stripe_sessions.charge_signal_count` (≤64); checkout sessions → `stripe_sessions.signal_count`
  unchanged. Over cap → the existing `signal_cap` IGNORED path.
- `integration.load_stripe_signal` (DROP + CREATE, same arguments; return type adds `object_type text`
  from the linked receipt (NULL for buyer/merchant refresh) and `refund_id uuid`) and
  `integration.consume_stripe_signal` (CREATE OR REPLACE, same signature): both dispatch the fence on
  the op's `actor_kind` — `PAYMENT_REFUND` → `require_stripe_refund` and `s.refund_id = p_operation`,
  job args `operation_id` = the refund id; `BUYER_PAYMENT_QUERY` → `require_stripe_query` (unchanged)
  and `s.refund_id IS NULL`. The job args shape `{operation_id, signal_id, version:1}` is unchanged, so
  the worker needs no discriminator: the generic `claim_operation` claims either op kind and the loader
  result tells the branch.
- SignalWorker: `refund_id` set → refund branch (§6); `object_type='charge'` → charge branch (§6),
  never `NOOP_TERMINAL`; otherwise today's checkout code path.
- `integration.claim_operation` is **not** replaced. (A1) A `blocked_binding` claim disposition for a
  `PAYMENT_REFUND` op (binding provider or asset changed) makes the worker record a LOCAL `escalate`
  refund report (`LocalReason='binding_changed'`) through `record_stripe_refund_observation`, which
  bypasses the lease requirement only for this code (it re-checks `result_code='binding_changed'` and
  `lease_until IS NULL` on the op); apply inserts review `REFUND_UNRESOLVED`, capacity stays held.

## 5. State machine

Derived from refund columns and facts, never one stored enum (like stripe-psp §7):

| State | Entered by | Exits |
| --- | --- | --- |
| REQUESTED (= §12.3 REQUESTED+APPROVED, RD11) | request tx | pre-send external refund → REJECTED; `mark_sent=SEND` → SUBMITTING |
| SUBMITTING (unknown) | send committed | 200 → pinned; first-send 4xx/401/403 → REJECTED; uncertain → same-key RESEND until `resend_until`, then list: match → pinned; none → UNKNOWN |
| PENDING (§12.3 ACKNOWLEDGED/PENDING) | pinned, status `pending`/`requires_action` | `succeeded` → SUCCEEDED; `failed` → FAILED; `canceled` → CANCELED; `requires_action` > 60 min → review `REFUND_CONFLICTING` (not expected for card) |
| SUCCEEDED | SUCCEEDED fact | late `failed`/`canceled` (webhook or refresh) → FAILED/CANCELED |
| FAILED / CANCELED | fact | terminal; capacity released |
| REJECTED | REJECTED fact (never reached Stripe, or definitive rejection) | terminal; capacity released |
| UNKNOWN | no pin after the resend window and list = 0, or budget exhausted before a terminal status | manual only (review `REFUND_UNRESOLVED`); capacity held |

**Effect table:**

| Event | Capacity | payment_state | Order / stock / fulfilment / work item |
| --- | --- | --- | --- |
| Request | held | unchanged | unchanged |
| SUCCEEDED | held (consumed) | PARTIALLY_REFUNDED or REFUNDED | unchanged |
| FAILED/CANCELED after SUCCEEDED | released | reverts | unchanged |
| REJECTED / FAILED / CANCELED before success | released | unchanged | unchanged |
| UNKNOWN / timeout / 5xx / missing webhook | **held** | unchanged | unchanged |
| External refund seen on the charge | n/a | REVIEW_REQUIRED (sticky `REFUND_HISTORY`) | unchanged; new refunds refused |

## 6. Worker and apply rules

**RefundWorker step** (inside `cmd/payment-worker`, same `StripeRuntime`, `payment_refund_v1` on the
profile queue; missing runtime → transient `stripe_unavailable` before Claim, as stripe-psp §8):

```
claim refund op → load_stripe_refund → build client from leased material → VerifyAccount(account)
if terminal fact (not SUCCEEDED): finish(stripe_refund_terminal); complete
if SUCCEEDED fact: finish(stripe_refund_terminal); complete   (late failure arrives via signal)
if not pinned:
  if never sent and suppressed_at set: record LOCAL unsent (LocalReason from the suppression); complete
  if never sent: RetrievePaymentCharge → record_stripe_charge_observation(Via=presend)   (A1)
                 true (suppressed) → record LOCAL unsent (external_refund); complete
                 false → mark_sent: SEND → POST /v1/refunds
                         CLOSED (window < resend_until-1h passed) → record LOCAL unsent
                         (send_window_closed) → REJECTED; complete
  else if now < resend_until: mark_sent=RESEND → POST (same key, same bytes)
  else: ListRefunds(pi) → match metadata.lc_refund: 1 → record via=list (pin); 0 → record via=list
        (ListMatchCount=0) → apply records REFUND_UNRESOLVED
  200 → record via=create (pin); ErrRejected/ErrAuthentication on first send → record rejected
  uncertain/409/429/idempotency_error → finish(code); snooze 5,15,45,120 s (cap 120)
else: RetrieveRefund → record via=retrieve
snooze: pending → 60 s for 1 h, then 15 min until op MaxAge 24 h, then finish(stripe_budget_exhausted)
        (PENDING kept; merchant refresh/webhook re-wake)
```

All HTTP calls in one claim share the existing `CallTimeout` budget (`call + 2*DB + 1 s < lease`).

**SignalWorker:** a signal with `refund_id` claims the refund op and retrieves that refund; an
attempt-level signal whose receipt `object_type='charge'` claims the checkout op (only after a terminal
CAPTURED fact) and records a charge report (`Via='retrieve'`). Staleness (10 min) and busy rules
unchanged. Loader/consumer per §4.6 (A1).

**`payments.apply_stripe_refund(attempt, hash)`** — lock order → refund row; never lock operations:
1. Identity guard (defensive; record already refused) → review `PROVIDER_IDENTITY_MISMATCH`, return.
2. `Via=create ∧ rejected ∧ SendCount=1 ∧ no pin` → REJECTED fact (`first_send_rejected`).
   LOCAL `unsent` with `suppressed_at` and never sent → REJECTED (`external_refund_detected` for
   `LocalReason='external_refund'`; `first_send_rejected` is **not** used; A1 adds failure_reason
   `send_window_closed` to the `refund_facts` CHECK for that LocalReason).
   LOCAL `escalate` `binding_changed` → review `REFUND_UNRESOLVED`; no fact (A1).
3. `Via=list ∧ ListMatchCount=0 ∧ now ≥ greatest(resend_until, last_sent_at + interval '15 minutes')`
   → review `REFUND_UNRESOLVED`; no fact (A1, mirrors checkout's `expires_at + 15 min` list closure).
   Earlier list-zero reports are no-ops; the worker lists again after the gap.
4. Money: `Currency ≠ refund.currency` or `Amount ≠ refund.amount` → review `REFUND_AMOUNT_MISMATCH`; no fact (I05).
5. `succeeded` → SUCCEEDED fact; if a FAILED/CANCELED fact exists: no-op when that fact's source
   observation `received_at` ≥ this report's `received_at` (stale report; Stripe only moves
   succeeded→failed), else review `REFUND_CONFLICTING` (A1);
   `failed` → FAILED fact with `FailureReason`; `canceled` → CANCELED fact.
6. Invariant re-check under the lock: Σ SUCCEEDED-not-failed ≤ captured, else review `REFUND_CONFLICTING`.
7. Anything else (`pending`, `requires_action`) → no-op.

**`payments.apply_stripe_charge(attempt, hash)`** — lock order: if `AmountRefunded` > Σ amount of this
attempt's refunds with `first_sent_at IS NOT NULL` that are **non-released as of this report** (A1: a
refund counts as released only if it has a FAILED/CANCELED/REJECTED fact whose source observation
`received_at` ≤ this charge observation's `received_at`) → review `REFUND_HISTORY`. It never suppresses
or touches refund rows (suppression is synchronous in `record_stripe_charge_observation`, RD9).
`AmountCaptured ≠ captured fact` → `CONFLICTING_REPORT`. `Disputed=true` → review `CONFLICTING_REPORT`
(disputes are out of scope; refunds are then blocked). Reviews written here never change order or
work-item state (§4.4 `apply_stripe_observation` guard). Every write is idempotent by PK; replay in any
order converges given the `received_at` comparisons above.

## 7. HTTP and UI surfaces

### 7.1 Merchant admin (Go private API; the admin BFF mirrors it under `/api/admin/`)

| Method/path | Input | Success |
| --- | --- | --- |
| POST `/v1/admin/stores/{store_id}/orders/{order_id}/refunds` | `Idempotency-Key` (required); body exactly `{amount_minor, reason, expected_refundable_minor}`; no query | 201 `{refund_id, state, amount_minor, currency, refundable_minor}` after COMMIT. 409 key conflict / `refundable_changed`; 422 `exceeds_refundable`, `amount_step`, `not_refundable`, `refund_blocked_review`, `refund_limit`; 403 missing `payments:refund`; 404 missing/other-store (indistinguishable). Currency is never accepted from the client. |
| GET `/v1/admin/stores/{store_id}/orders/{order_id}/refunds` | none | `{captured_minor, refunded_minor, pending_minor, refundable_minor, currency, items:[{refund_id, amount_minor, reason, state, requested_at, updated_at, failure_reason?, stripe_refund_id: string\|null}]}` (≤20; A1/R-9: `stripe_refund_id` merchant-only, null until pinned). Requires `orders:read`. |
| POST `.../refunds/{refund_id}/refresh` | no body, no key | `{refund_id, scheduled}`; `payments:refund`; throttled in SQL. |

Existing rules apply: 64 KiB JSON, unknown/duplicate fields rejected, private/no-store, fixed error
codes, no Stripe ids except the merchant refund id (R-9), no raw provider strings. Merchant-orders summary/detail (`identity.read_merchant_orders`
replaced in 0062, same single-snapshot and fresh-final-auth rules): `payment_state` adds
`PARTIALLY_REFUNDED`, `REFUNDED` with precedence `NOT_STARTED > REVIEW_REQUIRED > REFUNDED >
PARTIALLY_REFUNDED > CAPTURED > AUTHORIZED > PENDING`; summary adds `refunded_minor`, `refund_pending_minor`.

(A1) **merchant-orders-v1 projection invariants amended** (Go `internal/merchantorders/orders.go`
`validSummary`/decoders, owner commerce_worker; SQL projection must satisfy the same):
- `payment_state` admits `PARTIALLY_REFUNDED`, `REFUNDED`; `fulfillment_state` admits `MERCHANT_SHIPPED`
  (manual-fulfilment-v1); summary keys add `refunded_minor`, `refund_pending_minor` (detail keys too).
- `WorkState READY ⇒ CommercialState CONFIRMED ∧ PaymentState ∈ {CAPTURED, PARTIALLY_REFUNDED,
  REFUNDED, REVIEW_REQUIRED} ∧ FulfillmentState ∈ {MANUAL_UNASSIGNED, MERCHANT_SHIPPED}`.
- `MERCHANT_SHIPPED ⇒ WorkState READY ∧ CommercialState CONFIRMED`.
- `CONFIRMED ⇒ WorkState ≠ NONE ∧ PaymentState ∈ {CAPTURED, PARTIALLY_REFUNDED, REFUNDED, REVIEW_REQUIRED}`.
- Unchanged: `PAID_ALLOCATION_FAILED ⇒ REVIEW_REQUIRED` payment and work; `WorkState REVIEW_REQUIRED ⇒
  PaymentState REVIEW_REQUIRED`; `NOT_STARTED ⇒ WorkState NONE ∧ ¬TestMode`.

**Admin UI** (`merchant-orders-ui` amendment, ui_worker): inside the approved C inline detail row, a
"Refund" section shows captured / refunded / in progress / refundable amounts and the refund list with
state badges (REQUESTED, SUBMITTING, PENDING shown as "處理中/处理中/Processing"; SUCCEEDED; FAILED with
reason; UNKNOWN as "Needs support"). The action appears only with `payments:refund` and a refundable
amount; the dialog takes amount (default = refundable, formatted via the existing currency formatter,
TWD whole dollars) and reason, restates amount + currency for confirmation, and sends one idempotency
key per dialog open (kept for retries of that submission). No optimistic success; state comes from GET.

### 7.2 Buyer

`GET /v1/buyer/orders/{id}/payment` (via the `hosted_payment_view_v2` successor): `payment_state` adds
`PARTIALLY_REFUNDED`/`REFUNDED`; new `refund: null | {refunded_minor, pending_minor}` with no ids or
reasons. The storefront order page shows "Refund processing" when `pending_minor>0`, and "Refunded X —
your bank may take 5–10 business days" (F-R8) only for succeeded amounts. No buyer refund request
route in v1.

### 7.3 Webhook admission deltas (stripe-psp §0.2 prepare)

Subscribed types add `refund.created`, `refund.updated`, `refund.failed`, `charge.refunded`.
Mapping is scoped to the endpoint's account: `refund` object → by pinned `stripe_refund_id`, else by
`metadata.lc_refund` = a refund of an attempt on that account whose `metadata.lc_attempt` also matches;
none → `IGNORED unknown_refund` (a Dashboard refund; `charge.refunded` covers it). `charge` object →
by `payment_intent` = a pinned `stripe_sessions.payment_intent_id` on that account with a CAPTURED fact;
none → `IGNORED unknown_charge`. (A1) Signal caps: a refund-object receipt increments only
`stripe_refunds.signal_count` (≤64 per refund); a charge-object receipt increments only
`stripe_sessions.charge_signal_count` (≤64 per attempt); the checkout `stripe_sessions.signal_count` is
unchanged by refund traffic. Dedupe and ACK-after-commit are unchanged; the commit function and the
deferred receipt-link trigger are replaced as stated in §4.6 (the unchanged B1 versions would reject
every refund receipt at COMMIT).

## 8. Idempotency and uniqueness

| Scope | Key |
| --- | --- |
| Merchant request | `ops.command_results(tenant,store,'payments.refund.request',Idempotency-Key)` + `expected_refundable_minor` CAS |
| Stripe create | `lc:stripe:refund:v1:<refund uuid>`, every send, only before `requested_at+20h` |
| Stripe retrieve/list | GET, no key |
| Op | `semantic_key = payment.stripe.refund:<refund uuid>`; `UNIQUE(tenant,store,semantic_key)` |
| Facts | `(refund, kind)`; `stripe_refunds.stripe_refund_id` UNIQUE; op `provider_reference` |
| Webhook | stripe-psp §0.2 `(endpoint_id,event_id)` |

## 9. Test gates (tiers as stripe-psp §14: UNIT, REAL_PG, MOCK, HTTP_PG, SANDBOX, BROWSER)

| Gate | Test | Tier | Required |
| --- | --- | --- | --- |
| RF01 | `TestStripeRF01Wire` | UNIT | Golden refund body bytes, exact key set, forbidden keys (`charge`, `fraudulent`, `reverse_transfer`, `refund_application_fee`, `instructions_email`, `origin`) → `ErrInvalid`; resend byte-identical; key format; §5.6 classification for create/retrieve/list/PI; list 10-page cap → uncertain; §3.1 projections exact keys, ≤2048 bytes, no ARN/card/receipt URL; amount-step Go↔SQL parity (TWD %100). |
| RF02 | `TestStripeRF02WebhookProjection` + Node vectors | UNIT | refund/charge objects project `payment_intent`, `lc_refund`; other objects unchanged; strict JSON rules unchanged. |
| RF03 | `TestStripeRF03Schema` | REAL_PG | Fresh + populated upgrade from 0061; family CHECK keeps every prior family; new CHECKs negative; set-once and fact triggers (REJECTED exclusivity, SUCCEEDED→FAILED allowed, FAILED→SUCCEEDED refused, amount equality); FORCE RLS, column-privilege matrix, definer owner/`proconfig`/ACL, PUBLIC revoked; dispatcher checkout routing unchanged; post-River routing/guard/readiness include `payment_refund_v1`; LIVE environment rejected; (A1) 0062 adds the permission and no grant row (the onboarding grant exists only via 0065, OP01); §4.5 privilege matrix exactly (positive + one negative per role, incl. `commerce_runtime` River INSERT into `river_payment` allowed and River DELETE/state UPDATE refused, `internal/platform` runtime pool validator accepts it); composite FKs refuse a fact/signal of attempt X referencing a refund of attempt Y and a refund whose order is not the attempt's order; `first_sent_at < resend_until - 1h` CHECK negative; (A2) `has_schema_privilege('commerce_checkout_writer','identity'/'ops','USAGE')` true, each §4.5 policy present by name/role/cmd, and behaviour through the real definers (not as superuser): with a `REFUND_HISTORY` (and separately a `CONFLICTING_REPORT`) review on the attempt, `mark_stripe_refund_sent` returns `CLOSED`, not `SEND`; on a clean attempt with refund A sent (Stripe `amount_refunded` = A), the presend check for refund B does **not** suppress and B returns `SEND`; `request_stripe_refund` sees existing refunds in the capacity Σ; the webhook commit increments `stripe_refunds.signal_count`. |
| RF04 | `TestStripeRF04Request` | REAL_PG | Atomic request (op, refund, event, audit, command result, job); replay same key/body; key with different body 409; `expected_refundable` 409; two concurrent requests whose sum exceeds captured → exactly one wins (real two-transaction witness); pending/unknown refunds hold capacity; §4.3 matrix incl. late-payment full-only and (A1) the combinations `CLOSURE_CONTRADICTED + PROVIDER_PRESENTMENT_DRIFT` (full-only allowed) and `PAID_ALLOCATION_FAILED + REFUND_HISTORY` (refused); merchant-context request succeeds only through the §4.5 grants/GUCs (op, event, signal, command result, audit rows all written); PAYUNi/no-capture/other-store/revoked permission/expired session; 21st refund refused; **zero** changes to ledger, balances, reservations, order states, work items. |
| RF05 | `TestStripeRF05HappyMock` | MOCK | Real worker + `stripetest` fake: full refund → SUCCEEDED → REFUNDED; two partials → PARTIALLY_REFUNDED then REFUNDED; exactly one create key per refund; pre-send charge read occurs; stock and order unchanged; op ends `stripe_refund_terminal`. |
| RF06 | `TestStripeRF06Unknown` | MOCK | Drop-after-execute → same-key replay pins the same refund; cached 500 → resend → at `resend_until` list match pins / no match → UNKNOWN + `REFUND_UNRESOLVED`, capacity held, no second POST ever; first-send 400 → REJECTED + released; 400 after uncertain send → no release; 409/429 backoff; `idempotency_error` alarm; fake asserts ≤1 distinct key per refund; clock aging disclosed. |
| RF07 | `TestStripeRF07Lifecycle` | MOCK | pending (insufficient balance) → succeeded; succeeded → later `refund.failed` → FAILED fact, capacity released, payment_state reverts; canceled; failure reasons mapped; amount/currency drift → review, no fact (ruling 23: currency drift also ends the job `stripe_refund_mismatch`, no resend, capacity held); replay in any order converges, incl. (A1) a charge snapshot taken while A was SUCCEEDED applied after A's FAILED fact → no `REFUND_HISTORY`, and a stale `succeeded` report after FAILED → no `REFUND_CONFLICTING`; (A1) capture → ship → `REFUND_HISTORY` → replay the capture reconcile job: `fulfillment_state` and `work_state` unchanged; late first send (`now ≥ resend_until-1h`) → no POST, REJECTED `send_window_closed`; list=0 within 15 min of the last send → no `REFUND_UNRESOLVED`. |
| RF08 | `TestStripeRF08Webhook` | HTTP_PG | Four new types admitted and deduped; (A1) an accepted refund webhook COMMITs (deferred link trigger passes), is ACKed and its signal is loaded and consumed under the refund-op lease; a `charge.refunded` signal produces a charge observation and never `NOOP_TERMINAL`; refund/charge receipts leave `stripe_sessions.signal_count` unchanged and a late `refund.failed` after 20 partial refunds is still ACCEPTED; unknown refund/charge ignored; forged `lc_refund` of another account/store → ignored; Dashboard refund (charge `amount_refunded` > ours) → `REFUND_HISTORY`, new requests 422, a REQUESTED refund suppressed before send with **zero** `POST /v1/refunds` (fake request counter, A1); ACK only after commit; no body/signature/ARN in logs or rows. |
| RF09 | `TestStripeRF09AdminHTTP` + BFF Node tests | HTTP_PG + Node | Exact request/response keys and codes; permission matrix (`orders:read` alone cannot refund); strict body/query/method rules; refresh throttle; merchant read `payment_state` precedence and new fields; (A1) the merchant list/detail decode a refunded order, a `MERCHANT_SHIPPED` order and a READY order with a refund review; `stripe_refund_id` present for merchants only; buyer `refund` projection has no ids; PAYUNi responses byte-identical to golden files. |
| RF10 | `TestStripeRF10Sandbox` | **SANDBOX** | Refuses unless test key, `livemode=false`, registered sandbox account; (R-10) a test PaymentIntent with `pm_card_visa`; partial then full refund via the real adapter; same-key replay returns the same `re_…` with `Idempotent-Replayed`; changed params same key → `idempotency_error`; over-refund → 400; list by PI finds both; (A1) after rotating the store's test key, a same-key resend returns the same `re_…` (RD13 assumption; failure reverts RD13 to request-time key for resends); records the least RAK permission set. Test-mode only; SKIP is NOT_RUN. |
| RF11 | `refund-browser.spec.ts` | **BROWSER** | After SP18 (4242 payment): merchant with `payments:refund` refunds partially then fully in the admin page; buyer order page shows processing → refunded; merchant without permission sees no action; 3 locales, desktop + mobile Chromium; screenshots hashed. |
| RF12 | `TestStripeRF12Guards` + root | REVIEW + regression | D9/PROCESS §5 comments; (A1) old/new body sha256 + diff for every §4.4/§4.6 replaced B1 function shows only the stated delta; only `psp/stripe` dials Stripe; logs carry no key/whsec/body/ARN; full `go test -race ./...`, `go vet ./...`, `python3 scripts/check_packet.py`; every SP and prior gate unchanged; independent test_worker + security_reviewer verdicts. |

A gate that cannot fail is not a gate: each records one red run before its green run (PROCESS §2.4).

## 10. Ownership (PROCESS §2, stripe-psp §15 roles)

| Artifact | Owner |
| --- | --- |
| adapter additions + RF01/RF02 unit tests | integration_worker |
| `stripetest` refund endpoints, `tests/foundation/stripe_refund_*_test.go`, vectors, `refund-browser.spec.ts` | independent test_worker |
| `internal/payments/stripe_refund*.go`, SignalWorker branch, `internal/payments/stripewebhook` job-args change (§4.6), `internal/merchantorders` read extension incl. `orders.go` projection invariants (§7.1, A1) | commerce_worker |
| `internal/platform` runtime pool validator for the `river_payment` grants (§4.5) | integrator |
| `0062_stripe_refund.sql`, `post_river/0013_stripe_refund.sql`, `jobqueue`, admin HTTP mount, `core-openapi.json`, `tasks.json`, `sources.json` rows F-R1..F-R8, runbook webhook events | integrator |
| admin refund section, buyer refund display, BFF routes | ui_worker |

Sequence: B1 frozen and merged → freeze this file → 0062/post_river + RF03 (integrator) ‖ adapter RF01–02
→ commerce_worker RF04–RF08 → HTTP RF09 → SANDBOX RF10 → UI + RF11 → RF12 verdicts.
(A1) 0065 + OP01 (integrator) after 0062–0064 are merged.

## 11. Known limits / NOT_RUN

- Evidence: DESIGN only; RF01–RF12 NOT_RUN. SANDBOX/BROWSER NOT_RUN without test keys; LIVE NOT_APPLICABLE.
- Card refunds only; no `requires_action` flow, no refund cancellation (Dashboard-only for cards, F-R3).
- No disputes/chargebacks, no balance-transaction or payout reconciliation (§12.4), no fee accounting.
- Late refund failure up to ~30 days relies on webhooks and merchant refresh (R-8).
- UNKNOWN refunds and review cases need manual operator resolution; resolution UI NOT_IMPLEMENTED.
- A full refund does not cancel the order or release stock (R-3); no return/RMA (§13.4).
- No buyer notification message; the buyer sees state only on the order page.
- Stripe availability of balance: pending-for-balance refunds may stay PENDING past the 24 h poll budget.
- (A1) No per-principal refund amount limit (架构 §12.3 退款金额上限): `payments:refund` authorizes up to
  the full refundable amount. Owner ruling required before any non-owner principal is granted
  `payments:refund`.
- (A1) A refund whose first send would fall after `resend_until - 1 h` (19 h after request) is
  REJECTED `send_window_closed` without reaching Stripe; the merchant must request again.

## Integrator rulings (2026-09-29, binding; supersede the defaults in §0.1)

- R-1 Permission `payments:refund`. The store creator receives it for their own store through the
  owner-provisioning unit (0065, see below); no backfill of other principals.
- R-2 Single actor in v1 (no dual control). R-3 Cancel-and-release of a refunded unshipped order
  is a separate R1 follow-up unit, not part of this contract.
- R-4 The `fraudulent` reason is excluded. R-5 Current key version, frozen into the refund row.
  **Superseded by amendment (A1, RD13):** a frozen version can be revoked by a later rotation, which
  would make same-key resends, retrieves and lists return 401 and strand a SUBMITTING refund with
  capacity held. Calls use the account's current head of the same `account_id`; the request-time
  version is kept in the row for audit only.
- R-6 Registrar qualification must prove Refunds write + PaymentIntents/Charges read for a
  restricted key (extends SP21; RF gate owns the refund half).
- R-7 Draft §4.3 accepted. R-8 Webhook + merchant refresh only; no 30-day poller (documented limit).
- R-9 **Changed:** the merchant sees the Stripe refund id (`re_…`) for reconciliation with the
  Stripe Dashboard. It is not a secret; buyers never see it.
- R-10, R-11 (post-River 0013), R-12 accepted.
- Owner provisioning (applies to refund, fulfilment and live features): a new migration
  `0065_owner_provisioning.sql` makes `identity.create_initial_store` grant the creator
  `live:read`, `live:manage`, `payments:refund`, `fulfillment:write`, `orders:export` on the new
  store (in addition to today's set). Reason: 0033 deferred this "to an explicit provisioning
  decision"; without it an onboarded merchant cannot use live selling, refunds or shipping at all.
  No backfill for existing principals (no production data exists).

## 12. Owner provisioning migration `0065_owner_provisioning.sql` (A1; implements ruling R-1/M-1)

- Owner: integrator. Numbering: runs after 0062 (refund), 0063 (fulfilment) and 0064
  (meta-claims-intake), so every permission it grants already exists in `store_grants_permission_check`.
- Content: `CREATE OR REPLACE FUNCTION identity.create_initial_store(bytea,text,bytea,text,text,text,text)`
  — the 0027 body unchanged (same owner `commerce_identity_writer`, ACL, lock order, idempotent replay,
  audit `merchant.store_created`) except the grant array = the 0027 list
  (`store:read, audit:read, audit:write, catalog:read, catalog:write, inventory:read, inventory:write,
  inventory:reserve, pricing:read, pricing:write, integration:read, integration:manage, orders:read`)
  + `live:read`, `live:manage`, `payments:refund`, `fulfillment:write`, `orders:export`,
  `integration:execute` (SPEC CHANGE, ruling 24 of refund-fulfilment-rulings.md, 2026-09-29: six, not five
  — the claim-source definer `live.put_claim_source` requires `live:manage` + `integration:execute`, without
  which the owner cannot bind their own posts; this supersedes the earlier "creator is denied
  `integration:execute`" expectation of account_onboarding).
  No UPDATE/INSERT of existing `store_grants` rows (no backfill). `cmd/admin-fixture` is not changed;
  tests that need these permissions onboard through `create_initial_store` or grant explicitly.
- Gate **OP01** `TestOwnerProvisioningOP01` (REAL_PG): a fresh creator has exactly that set on the new
  store; an onboarding replay (same key) adds nothing; other members of the tenant get none; principals
  created before 0065 are not backfilled; the function body diff against 0027 is only the array.
- 0062 and 0063 themselves add **no** grant rows; RF03/MF02 assert that; OP01 owns the onboarding grant.

## 13. Round-1 review map (A1)

| Finding | Resolution |
| --- | --- |
| P1 refund signals cannot commit/consume | §4.1 signal/receipt deltas, §4.4 `payment_job_queue`, §4.6 replacements, RF08 |
| P1 checkout_writer grants / River pool | §4.4 GUC rule, §4.5 grant table, §2 pool, RF03/RF04 |
| P1 pre-send check async | RD9, §3.1 `Via=presend`+`RefundRef`, §4.4 record/mark_sent, §6, RF08 zero-POST |
| P1 refund reviews flip orders to PAID_ALLOCATION_FAILED | §4.4 `apply_stripe_observation` guard, RF07 replay case |
| P1 merchant projection validator | §7.1 invariants, §10 owner, RF09 |
| P1 permission migrations / 0065 | §4.1 `pg_get_constraintdef`, RD10, §12 + OP01 |
| P2 out-of-order apply | §6 apply step 5 and `apply_stripe_charge` `received_at` rule, RF07 |
| P2 early REFUND_UNRESOLVED | §4.2 CHECK, §4.4 mark_sent CLOSED, §6 step 3, RF07 |
| P2 signal cap crowding | §4.1 `charge_signal_count`, §4.6 commit, §7.3, RF08 |
| P2 composite FKs | §4.1 attempts UNIQUE, §4.2 DDL, RF03 |
| P2 op fields / blocked_binding / key rotation | §4.4 request row, §4.6 blocked_binding, RD13, R-5 superseded, RF10 |
| P2 combined reviews | §4.3 combination rule, RF04 |
| P2 R-9 not in body | §7.1 `stripe_refund_id` |
| P2 §12.3 amount limit | §11 |

## 14. Round-2 review map (A2)

| Finding | Resolution |
| --- | --- |
| P1 grants: checkout_writer lacks USAGE on `identity`/`ops`; no policies on `stripe_refunds`/`refund_facts` for checkout_writer or integration_writer; no integration_writer SELECT policy on `review_cases` (RD9 fails open) | Verified against 0013:5–9, 0016:84, 0060:45, 0018:113–128, 0027:56–70, 0061:393–427, post_river 0005:667. §4.2 RLS note, §4.5 rows (A2), RF03 behavioural cases. The `commerce_auth` ops half lives in manual-fulfilment-v1 §4 (0063), since only `export_unshipped_orders` writes ops rows as `commerce_auth` |

## Integrator rulings at freeze (2026-09-29)

- Approved: 0062 / post-River 0013 re-create the frozen B1 functions listed in §4.4/§4.6
  (`stripe_webhook_prepare`, `stripe_webhook_commit`, `guard_stripe_receipt_link`,
  `load_stripe_signal`, `consume_stripe_signal`, `payment_job_queue`, `apply_stripe_observation`)
  exactly as specified here; `contracts/stripe-psp-v1.md` gets a pointer note, and every B1 SP gate
  must stay green after 0062 (regression is part of RF gates).
- Approved: `commerce_runtime` gets SELECT/INSERT/UPDATE(kind) on `river_payment.river_job` +
  sequence USAGE (the `commerce_checkout_runtime` precedent), with the matching
  `internal/platform` runtime-pool validator change and T06/pool allowlist updates.
- Approved: `commerce_integration_writer` INSERT on `payments.review_cases` limited by policy to
  `reason='REFUND_HISTORY'`.
- Per-principal refund amount limit (架构 §12.3): none in v1 because only the store creator holds
  `payments:refund`; an owner ruling is required before any other principal receives it.
- `commerce_integration_writer` policy on `stripe_refunds` as USING(true) fenced by the operation
  lease, same shape as 0061: accepted.
- Round-3 P2s: fix during implementation (implementer must list how each was handled).

## Amendment (ruling 23, 2026-09-29): currency drift ends the job AND opens a review

§4.4 (`record_stripe_refund_observation`: `Currency` is part of the identity, a change finishes UNKNOWN `stripe_refund_mismatch`) and §6 step 4 (`Currency ≠ refund.currency` → review `REFUND_AMOUNT_MISMATCH`, no fact) conflicted; the integrator ruled BOTH apply. A refund report with a non-empty `Currency` different from the request is now recorded (identity is still proved by `MetadataRefund`, `MetadataAttempt`, `PaymentIntentID`, account and test mode; an empty `Currency` on a report that carries a `RefundID` still raises PT409), and `record_stripe_refund_observation` completes the operation UNKNOWN `stripe_refund_mismatch` instead of `stripe_refund_observed`; the worker then ends the River job (`JobCancel`, no poll, no resend). `apply_stripe_refund` step 4 runs unchanged and inserts `REFUND_AMOUNT_MISMATCH` with no fact (I05), so refundable capacity stays reserved and a human resolves it (Stripe refunds are always in the charge currency; a mismatch means a wrong match or corrupted data). Every other identity mismatch (metadata, PaymentIntent, account, livemode, pin change) still raises PT409 and finishes UNKNOWN `stripe_refund_mismatch` without a recorded observation. A same-currency amount drift is unchanged (review, no fact, job continues). RF07 asserts all three effects (review, op `stripe_refund_mismatch` with the job cancelled and `send_count` still 1, capacity held). Code: `migrations/post_river/0013_stripe_refund.sql` (`record_stripe_refund_observation`), `internal/payments/stripe_refund.go` (`recordAndSnooze`).
