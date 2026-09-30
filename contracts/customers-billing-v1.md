# Customers, privacy and platform billing v1 — T14 + T17 + their T18 screens (R2)

Status: **v1 FROZEN 2026-09-30 (DESIGN; gates NOT_RUN)**. Round-1 review (`output/contract-review/r2-design-wave.json`,
key `customers-billing`, verdict BLOCK, 6 P1 + 7 P2), the round-2 cross-contract P1 (consent gate vs
meta-ads-v1 AD8/A-3) and the round-3 P1 + P2s (`output/contract-review/r2-round3.json`) are closed in place;
finding ledger §11, binding rulings §12.
Evidence label for this file: DESIGN. Every CB gate below is NOT_RUN. Nothing here authorizes a LIVE platform charge, a real buyer erasure or a message to a real user.

Authoritative: 架构 §14 (identity/transaction/touchpoint graphs, consent), §20.4 (usage ledger, arrears
never cut after-sales), §21.4 (deletion vs legal retention, restore replays tombstones), §2.2 (platform fee
collected separately from merchant funds), invariants I09, I11, I16, I22, I23; gates G05, G09.
Extends [merchant-orders-v1](merchant-orders-v1.md), [stripe-refund-v1](stripe-refund-v1.md) (§12 owner
provisioning pattern), [live-keyword-claims-v1](live-keyword-claims-v1.md) +
[meta-claims-intake-v1](meta-claims-intake-v1.md) (actor keys), [stripe-psp-v1](stripe-psp-v1.md)
(webhook verifier, D8 "webhook is a wake-up"). Where they conflict with this file, this file is wrong.
Migrations reserved: `0078_customers_privacy.sql`, `0079_platform_billing.sql`. No post-River file.

## 0. Owner inputs, decisions and rulings

Owner inputs recorded (chat, 2026-09-29): R2 must ship T14/T17/T18; merchants go live on FB/IG themselves
(LiveKit closed); the platform will later manage merchants' Meta ads (R3). Owner decisions O-A..O-D and the
integrator's accepted defaults (2026-09-29) are recorded in §12; only plan price and billing entity remain open (§10).

What exists today (read from migrations, not assumed): a "buyer" is `buyer.owners` — one anonymous
capability owner per store and browser, **no email, no verified phone, no account**. PII lives only in
`storefront.destination_snapshots` (recipient_name, phone, address), frozen into orders. Social identity is
`claims.bundles.actor_key` / `claims.meta_intake.actor_key` = keyed HMAC of the Meta sender id, bound to an
owner only when a claim link is redeemed (`bound_at`). A redeemed link proves who opened it, not who
commented (§14.2 Alice→Bob).

| # | Decision | Why / risk closed |
| --- | --- | --- |
| CD1 | **Customer = `buyer.owners` row.** No new customer table, no identity merge. The API calls it `customer_id` (= owner id). | Only per-store capability identity exists; merging needs verified contact evidence that the system does not collect (§14.1). I09. |
| CD2 | **No automatic identity edge** from claim-link redemption, same actor_key, same phone or same name. Two owners stay two customers. | §14.2: forwarded link ≠ same person; phone/name equality is not evidence of the same account (§14.1). |
| CD3 | **Customer list is a read-time projection** over orders, payment/refund facts and bound bundles. Actor keys, owner session ids and PSP refs are never returned. List shows latest recipient name + phone last 3 digits; full values stay on order detail (existing merchant-orders-v1 surface). | Smallest correct read; minimal PII in list views (I11). |
| CD4 | **Consent = append-only `customers.consent_events`**, current state = latest event per (owner, purpose, channel), absence = not granted. Purposes v1: `marketing_messages` (channel `meta_dm`), `ads_personalization` (channel `meta_ads`). Only the buyer can grant; buyer or merchant can withdraw; erasure withdraws all. Purchase never implies consent. Every consent writer locks the `buyer.owners` row FOR UPDATE first, so "latest event" = commit order. `source` and `policy_version` are set by the server (context → source; `policy_version` = deployed notice constant `LC_PRIVACY_POLICY_VERSION`), never taken from the request. | §14.4, §21.4, PDPA Art 20 (F-P1). Recipient = the store (row scope). Client cannot name a notice it never saw. |
| CD5 | **`customers.consent_allows(tenant,store,owner,purpose,channel)` is the only consent gate.** R2 has no marketing consumer; every R3 send/CAPI/audience planner must call it in the same transaction that plans the job (I22). **Consent recorded on an owner NEVER authorizes a send or an audience entry for any `actor_key`.** Any send or audience entry keyed by an `actor_key`/PSID (DMs, custom audiences built from social actors) additionally needs opt-in from that Meta actor (Meta's own marketing-message opt-in inside Messenger/IG, UNKNOWN §1), recorded with the actor as subject in its own contract; for these `consent_allows` is necessary, not sufficient. Owner-subject events built only from that owner's own storefront checkout (CAPI Purchase with the owner's own browser context, no `actor_key`) may rely on `consent_allows(...,'ads_personalization','meta_ads')` alone ([meta-ads-v1](meta-ads-v1.md) AD8). Owner-subject means identifiers the owner supplied about themselves; a recipient phone (`storefront.destination_snapshots.phone`, which may be a gift recipient's) may be used only when the checkout records that recipient = buyer, or CAPI omits `ph`. | One gate, no copy of consent state in R3 tables. The only owner→actor edge is a redeemed link, which is not identity proof (§14.2): Bob ticking the box after redeeming Alice's link must never authorize a DM to Alice (I07/I09, AGENTS.md "no skipped consent"). |
| CD6 | **Privacy actions are synchronous, one transaction each, logged in `customers.privacy_actions`.** EXPORT returns JSON in the response; ERASURE redacts in place. No request queue, no object storage. | Data per owner is bounded (≤ 200 orders enforced, §7); PDPA response deadlines (F-P1) are met by doing it immediately. |
| CD7 | **Erasure scope:** revoke consents, revoke capability sessions, deactivate owner, redact destination snapshots **not referenced by any order**, set the manual `label` of the owner's bound bundles to `erased-<bundle id, 32 hex>`. **Never touches `actor_key`, `claims.meta_intake`, `live.claim_sources` or `social.*`**: a redeemed link does not prove who commented (§14.2), so owner erasure never alters actor-level data. Actor-level (Meta user) deletion goes only through the data-deletion instructions page (F-P2) or a later verified callback; `social.messages`/`social.comment_events` retention is an open U08 gap (§9). **Orders, order snapshots, payment/refund facts and shipments are retained unchanged** (legal/financial retention, U08). Refused while the owner has an unexpired DRAFT/AWAITING_PAYMENT order, **any `payments.stripe_sessions` row with `expires_at > now()`** (sessions live 40 min, orders ≤ 15 min: 0061/0013 CHECKs), or a Stripe refund without a terminal fact. | §21.4 separates marketing/profile deletion from legal retention; never break an in-flight payment or refund (G05, I24 late capture). Rotating `actor_key` was rejected: `ClaimActorKey` is a deterministic HMAC of (object, asset, fromID) (`internal/integrations/meta/claim_intake.go:89`), so the next comment re-derives the old key; rotation de-identifies nobody and would rewrite another person's claim identity (0060 `UNIQUE(session_id,platform,actor_key)` then opens a second bundle). |
| CD8 | **Tombstone = the ERASURE row in `privacy_actions`;** `customers.replay_erasures()` re-applies CD7 idempotently. Keeping a copy of erased owner ids outside PG backups is T20 (§9). | §21.4 restore must replay deletions before marketing reopens. |
| BD1 | **Platform fee = Stripe Billing on the platform's own Stripe account** (never a merchant's account, no Connect): Stripe Checkout `mode=subscription` to start, Stripe customer portal for payment method, invoices and cancellation. **The platform billing account MUST differ from every `integration.merchant_accounts` Stripe `account_id`.** At startup the billing client calls `GET /v1/account`; if the id cannot be read or `billing.platform_account_conflict(id)` is true, billing stays disabled (503 `billing_unavailable`) and logs `billing_account_conflict`. `pin_customer` re-checks it (§3.2). | §2.2; stripe-psp D1 binds the per-environment account to one store as its merchant of record, so reusing it would mix platform fees with merchant sales. F-B4/F-B5 cover invoices and dunning without our own invoice UI. Owner question Q1. |
| BD2 | **Billing unit = store** (`client_reference_id` = store id, `subscription_data.metadata.lc_store` = store id). One non-terminal subscription per store, enforced by the §5 checkout guard (Stripe list + one open session + Dashboard "limit to one subscription"); a second non-terminal subscription observed for one store → `apply_subscription` returns `duplicate` + ops alert, no auto-cancel. **Trial only once:** Checkout sets `subscription_data.trial_period_days` only when the store has zero rows in `billing.subscriptions` (any status) and Stripe lists no prior subscription for the pinned customer. | First UI exposes one store (§2.1). Q3. The Dashboard limit does not count `trialing` (F-B12), so it alone cannot stop a double subscription during a trial; repeatable trials would let a store evade fees. |
| BD3 | **Local state = mirror of the retrieved Subscription only** (`billing.subscriptions`). Webhooks wake a retrieve (D8); no invoice mirror (portal shows invoices), no Stripe event table. | Smallest state that derives standing. |
| BD4 | **Standing (derived, never stored)** is computed only from subscriptions with status ∉ {`incomplete`, `incomplete_expired`}: `trialing`/`active` → GOOD; `past_due` → GRACE (banner only); `unpaid`/`canceled`/`paused` → RESTRICTED; no row left after the filter → UNBILLED (unrestricted in v1, Q4). Best standing over the remaining rows wins. `incomplete` only adds a "payment pending" banner and never improves standing. | `incomplete` lasts 23 h (F-B1); counting it as GRACE let canceled → new failing checkout → GRACE lift RESTRICTED every 23 h without paying. F-B1/F-B2: Stripe itself says revoke on `unpaid`/`canceled`. |
| BD5 | **RESTRICTED blocks only new growth actions:** opening a claim window (v1) and, in R3, creating/raising ads (R3 must call `billing.store_standing`, C-8). **Never** blocks: buyer checkout of existing carts, payment capture/apply, refunds, fulfilment, order reads/exports, privacy actions, an already-open window. Enforced by one trigger on `live.claim_windows` (§3.2), not by editing any existing function. | §20.4; owner request "never block buyer refunds". |
| BD6 | **Usage = derived counts, no usage ledger in v1:** paid orders (CAPTURED facts), claim windows opened (`live.claim_window_intervals`), Meta private replies sent (`integration.operations` action `meta.private_reply`, SUCCEEDED), members with grants. Plan limits are display-only (Q5). No Stripe meters. | Every metered thing already has a unique durable row; Stripe now steers new usage billing to Metronome (F-B7), so metered billing is deferred until overage pricing exists. |
| BD7 | **Finance view** = daily captured / refunded / net by currency and environment over ≤ 92 days, UTC+8 days (Q11), from `payments.facts` + `payments.refund_facts`. CSV via `orders:export`. Profit (COGS) deferred. | Merchant reconciliation need; no second ledger. |
| BD8 | **SANDBOX only in v1** (`environment='SANDBOX'` CHECK, keys `sk_test_`/`rk_test_`), like stripe-refund. LIVE is an amendment after owner approval + Q1 entity. | PROCESS §6. |

Rejected alternatives:
- Identity graph with auto-merge by phone/actor_key/link: violates §14.2/I09; no verified contact exists.
- Consent boolean columns on owners: loses source/version/withdrawal history (§14.4).
- Async privacy request queue with RECEIVED/VERIFIED states: no deadline risk when execution is immediate; merchant verifies identity offline before pressing the button.
- Deleting orders/snapshots on erasure: destroys financial/legal records (§21.4).
- Local invoices table and our own dunning emails: duplicates Stripe portal + Smart Retries (F-B2, F-B5).
- Stripe Billing Meters / usage events table now: no overage price exists; F-B7.
- River job for billing sync: needs a post-River number and a worker kind for ≤ a few events per store per month; inline retrieve + Stripe's 3-day retries (F-B9) + refresh-on-read suffice (ponytail ceiling §9).
- Rotating `actor_key` on owner erasure (round-1 draft): deterministic HMAC re-derives it, and it rewrites another actor's claim identity (CD7).
- Treating checkout-box consent as authority to DM or target a Meta actor (CD5).
- Relying on the Dashboard "limit to one subscription" setting alone (does not count `trialing`, F-B12).
- Blocking buyer checkout or refunds on arrears: forbidden by §20.4.
- Platform collecting fees by deducting from merchant payouts: no Connect, merchant is MoR (§2.2).

### 0.1 Integrator rulings needed

- **C-1** Expose `customer_id` (= `buyer.owners.id`) in merchant APIs. merchant-orders-v1 forbids owner ids in
  order summaries; an owner id is not a credential (the capability token hash is). Draft: allow in the new
  customer endpoints only; order summaries unchanged.
- **C-2** tasks.json lists `internal/privacy/**`; draft puts privacy code in `internal/customers` (one package,
  one owner of consent + erasure). T18 write path `apps/admin/operations/**` does not match the app layout;
  draft uses `apps/admin/app/[locale]/{customers,finance,billing}` and `apps/storefront` privacy routes.
- **C-3** New SQLSTATEs `PT412` → HTTP 402 `billing_restricted`, `PT410` → HTTP 410 `erased` (buyer retry after erasure). Not PT402: it already means insufficient stock (0013:487, post_river/0005:279; Go `internal/checkout/checkout.go:354` → `command.ErrInsufficient`).
- **C-4** Platform webhook reuses the `commerce_stripe_ingress` login member (stripe-psp §0.2) with EXECUTE on
  `billing.apply_subscription` only (draft) vs a new login. `commerce_runtime` never gets `apply_subscription`; its
  refresh-on-read goes through `billing.refresh_subscription` (store-scoped, `billing:manage`).
- **C-5** `0079` is the **sixth version** of `identity.create_initial_store` (0004/0007/0019/0027/0065 → 0079); body
  diff vs 0065 = grant array only (+ 3 permissions). No automatic backfill. Stores created before 0079 (R1 staging or a
  pilot store) get the 3 permissions only through the owner-approved operator script of §9.
- **C-6** `commerce_privacy_writer` receives column grants on other domains' tables (§3.1 list). Alternative is
  one definer per owning role, which triples the functions; draft keeps one writer.
- **C-7** (with meta-ads-v1 A-3): 0078 does NOT grant to roles created in later-shipping files. `migrate.go`
  applies every missing version in `fs.Glob` (lexical) order with no out-of-order check (`migrate.go:75,195-215`),
  so an R2 database that already has 0078 would get R3's 0074 applied later, and a 0078
  `GRANT ... TO commerce_ads_writer` fails on a fresh R2 database where that role does not exist yet. The grant
  `GRANT EXECUTE ON FUNCTION customers.consent_allows(uuid,uuid,uuid,text,text) TO commerce_ads_writer` lives in the
  first R3 migration numbered after 0079, which lives in meta-ads `0080_meta_capi.sql` (A-11). 0074/0075 stay below
  0078 and must not reference any `customers.*`/`billing.*` object. Any R3 grant on `billing.store_standing` also goes
  in a file numbered ≥ 0080. R3-owned extension point: the buyer consent PUT (§5) calls meta-ads `ads.put_capi_context` after an
  `ads_personalization` grant; that call, its EXECUTE grant to `commerce_buyer_runtime` and its tests belong to
  meta-ads-v1 (R3), not to this contract; R2 ships the handler without it.
- **C-8** (BD5/Q6 ads half): meta-ads approve and publish (create or raise budget) call `billing.store_standing`
  and refuse RESTRICTED with `billing_restricted` (meta-ads-v1 §2 flow steps 6–7, currently HTTP 409 there vs 402 here;
  the integrator picks one status for the shared error name). The grant
  `GRANT EXECUTE ON FUNCTION billing.store_standing(uuid,uuid) TO commerce_ads_writer` lives in meta-ads
  `0080_meta_capi.sql` (≥ 0080). Gate is owned by meta-ads (MA02 reads a RESTRICTED fixture as `commerce_ads_writer`),
  which requires `store_standing` to be SECURITY DEFINER (§3.2).

## 1. External facts relied on (retrieved 2026-09-29 UTC)

| # | Fact | Source | Status |
| --- | --- | --- | --- |
| F-B1 | Subscription statuses: `trialing`, `active`, `incomplete` (23 h to pay first invoice), `incomplete_expired` ("don't bill customers"), `past_due`, `canceled` (terminal), `unpaid`, `paused` (trial ended without payment method). `active` "doesn't indicate that all outstanding invoices … have been paid". | [subscriptions overview](https://docs.stripe.com/billing/subscriptions/overview) | VERIFIED |
| F-B2 | After retries on `past_due`, Dashboard failed-payment settings move the subscription to `canceled`, `unpaid` or leave `past_due`. "Revoke access to your product when the subscription is `unpaid`"; revoke on `canceled`/`unpaid`. Paying the latest invoice returns it to `active`. | overview; [subscription webhooks](https://docs.stripe.com/billing/subscriptions/webhooks) | VERIFIED |
| F-B3 | Events: `customer.subscription.created/updated/deleted/paused/resumed`, `invoice.paid` (provision when subscription `active`), `invoice.payment_failed`, `invoice.payment_action_required`, `invoice.finalization_failed`. Invoice→subscription on API ≥ `2025-03-31.basil` is `invoice.parent.subscription_details.subscription`. | subscription webhooks | VERIFIED |
| F-B4 | `POST /v1/checkout/sessions`: `mode=subscription` needs recurring `line_items`; `client_reference_id` ≤ 200 chars; with `customer` set, a Customer without email gets the email typed in Checkout; `expires_at` 30 min–24 h (default 24 h); `subscription_data` passes params to the subscription. | [create session](https://docs.stripe.com/api/checkout/sessions/create) | VERIFIED |
| F-B5 | Customer portal: update payment methods, update/cancel subscriptions, "Pay, download, and view current and past invoices"; zh-Hant-TW supported; session expires 5 min if unused, 1 h after last activity; cannot be iframed. | [customer portal](https://docs.stripe.com/customer-management) | VERIFIED |
| F-B6 | `POST /v1/billing_portal/sessions` takes `customer`, `return_url` (optional `configuration`, `flow_data`, `locale`) and returns `url`. | [portal session](https://docs.stripe.com/api/customer_portal/sessions/create) | VERIFIED |
| F-B7 | Billing Meters (`/v1/billing/meter_events`) process asynchronously; Stripe recommends Metronome for new usage-based integrations. | [recording usage](https://docs.stripe.com/billing/subscriptions/usage-based/recording-usage) | VERIFIED |
| F-B8 | Stripe account countries (Asia-Pacific): AU, HK, IN (preview), ID (preview), JP, MY, NZ, SG, TH. **Taiwan not listed.** | [stripe.com/global](https://stripe.com/global) | VERIFIED |
| F-B9 | Invoice finalization waits for a 2xx on `invoice.created` up to 72 h; live webhooks retry up to 3 days, sandbox three times over a few hours. | subscription webhooks | VERIFIED |
| F-B10 | Billing period fields live on subscription items (`items.data[].current_period_start/end`) in current API versions. | overview (field link anchor only) | SOURCE_PARTIAL — CB08 must assert against the API version pinned by stripe-psp |
| F-B12 | "Limit customers to one subscription" counts only statuses `active`, `past_due`, `unpaid`, `paused` (**not `trialing`**); detection by the Checkout `customer` or email; needs the no-code portal with its login link kept enabled plus the Checkout settings toggle. | [limit subscriptions](https://docs.stripe.com/payments/checkout/limit-subscriptions.md?payment-ui=stripe-hosted) | VERIFIED 2026-09-29 |
| F-B13 | Test clocks: create a Customer with `test_clock=<clock>`; an existing customer can be attached only if its clock `frozen_time` is not in the past and the account has no Billing Automations; a customer cannot leave a clock; list endpoints omit test-clock objects unless filtered (e.g. by customer). | [test clocks API](https://docs.stripe.com/billing/testing/test-clocks/api-advanced-usage.md) | VERIFIED 2026-09-29 |
| F-P1 | Taiwan PDPA: Art 3 rights (review, copy, correct, cease processing, erase) cannot be waived; Art 11 erase when the purpose ends unless required for duties; Art 13 answers in 15 days (review/copy) or 30 days (correction/cessation/erasure), each extendable; Art 20 stop marketing on objection and offer a way to refuse at the first marketing contact. Page shows last amendment 2025-11-11. | [PDPA (English)](https://law.moj.gov.tw/ENG/LawClass/LawAll.aspx?pcode=I0050021) | VERIFIED (tool summary of the English text; exact paragraphs to be rechecked by legal review U08) |
| F-P2 | Meta: every app that accesses user data must provide a data deletion callback **or** a help page with deletion instructions; the callback gets a `signed_request` with an app-scoped `user_id` and returns `{url, confirmation_code}`. | [data deletion callback](https://developers.facebook.com/docs/development/create-an-app/app-dashboard/data-deletion-callback/) | VERIFIED |

UNKNOWN (not guessed): Taiwan retention period for commercial records; Singapore/HK privacy rules; whether
Meta requires its own opt-in for marketing DMs (R3, CD5 assumes it does until verified).

## 2. Flows

```
Buyer checkout page (two unticked boxes) ─after order placed─► PUT /v1/buyer/consents {context:"checkout"} ─►
  customers.buyer_set_consent → consent_events (source buyer_checkout, server policy version)
Buyer /privacy ─► GET/PUT consents, POST export (JSON attachment), POST erasure (typed confirm)
Merchant /customers ─► list/detail (identity.read_merchant_customers, customers:read)
  ─► withdraw consent / export / erase (customers:privacy; audit row)
Merchant /billing ─► GET status (refresh-on-read if synced > 10 min) ─► POST checkout → Stripe list-by-customer +
  guard (§5) → Stripe Checkout (platform account) ─► success_url back to /billing ─► POST portal → Stripe portal
Stripe (platform) ─webhook─► POST /v1/platform/stripe/webhook: verify signature (existing WebhookVerifier,
  platform secret) → resolve subscription id → GET /v1/subscriptions/{id} (no DB tx open) →
  billing.apply_subscription (one tx) → 200; retrieve error → 503 so Stripe retries (F-B9)
Studio "open claim window" ─► UPDATE live.claim_windows state OPEN ─► trigger billing.guard_window_open
  → RESTRICTED → PT412 → 402 billing_restricted
```

## 3. Persistence

### 3.1 `0078_customers_privacy.sql`

Preconditions (fail the migration otherwise): 0060, 0064, 0065 present; `store_grants_permission_check`
re-derived via `pg_get_constraintdef` (0063 pattern) and extended with `customers:read`, `customers:privacy`.
No grant rows.

```sql
CREATE ROLE commerce_privacy_writer NOLOGIN NOSUPERUSER NOBYPASSRLS NOCREATEDB NOCREATEROLE NOREPLICATION;
CREATE SCHEMA customers; REVOKE ALL ON SCHEMA customers FROM PUBLIC;
GRANT USAGE ON SCHEMA customers TO commerce_privacy_writer, commerce_auth;

CREATE TABLE customers.consent_events (
 tenant_id uuid NOT NULL, store_id uuid NOT NULL, owner_id uuid NOT NULL, id uuid NOT NULL DEFAULT gen_random_uuid(),
 purpose text NOT NULL CHECK (purpose IN ('marketing_messages','ads_personalization')),
 channel text NOT NULL,
 granted boolean NOT NULL,
 source text NOT NULL CHECK (source IN ('buyer_checkout','buyer_settings','merchant_recorded','erasure')),
 policy_version text NOT NULL CHECK (policy_version ~ '^[a-z0-9][a-z0-9._-]{0,39}$'),
 principal_id uuid,                                   -- merchant_recorded only
 request_key uuid NOT NULL,                           -- caller idempotency key
 occurred_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 PRIMARY KEY (tenant_id,store_id,id),
 UNIQUE (tenant_id,store_id,owner_id,request_key,purpose,channel),   -- erasure writes one row per pair under its key
 FOREIGN KEY (tenant_id,store_id,owner_id) REFERENCES buyer.owners(tenant_id,store_id,id),
 FOREIGN KEY (tenant_id,principal_id) REFERENCES identity.memberships(tenant_id,principal_id),
 CHECK ((purpose='marketing_messages' AND channel='meta_dm') OR (purpose='ads_personalization' AND channel='meta_ads')),
 CHECK (NOT granted OR source IN ('buyer_checkout','buyer_settings')),     -- only the buyer grants
 CHECK ((source='merchant_recorded')=(principal_id IS NOT NULL)));
CREATE UNIQUE INDEX consent_one_key ON customers.consent_events(tenant_id,store_id,owner_id,request_key)
 WHERE source<>'erasure';                             -- one buyer/merchant request = one row, always recorded (I02)
CREATE INDEX consent_current ON customers.consent_events(tenant_id,store_id,owner_id,purpose,channel,occurred_at DESC,id DESC);

CREATE TABLE customers.privacy_actions (
 tenant_id uuid NOT NULL, store_id uuid NOT NULL, id uuid NOT NULL DEFAULT gen_random_uuid(),
 owner_id uuid NOT NULL,
 kind text NOT NULL CHECK (kind IN ('EXPORT','ERASURE')),
 via text NOT NULL CHECK (via IN ('buyer','merchant','restore')),      -- restore: tombstone re-inserted by replay_erasures
 principal_id uuid,
 request_key uuid NOT NULL,
 summary jsonb NOT NULL CHECK (jsonb_typeof(summary)='object' AND octet_length(summary::text)<=1024), -- counts only, no PII
 completed_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 PRIMARY KEY (tenant_id,store_id,id),
 UNIQUE (tenant_id,store_id,owner_id,request_key),
 FOREIGN KEY (tenant_id,store_id,owner_id) REFERENCES buyer.owners(tenant_id,store_id,id),
 FOREIGN KEY (tenant_id,principal_id) REFERENCES identity.memberships(tenant_id,principal_id),
 CHECK ((via='merchant')=(principal_id IS NOT NULL)));
CREATE UNIQUE INDEX privacy_one_erasure ON customers.privacy_actions(tenant_id,store_id,owner_id) WHERE kind='ERASURE';
CREATE INDEX orders_by_owner ON checkout.orders(tenant_id,store_id,owner_id,created_at DESC,id DESC);
```

Both tables FORCE RLS, PUBLIC revoked, append-only (no UPDATE/DELETE grant to anyone). `COMMENT ON` each
table/column/function per PROCESS §5.

Functions (all `SECURITY DEFINER SET search_path=pg_catalog`, PUBLIC revoked):

| Function | Owner | EXECUTE to | Behavior |
| --- | --- | --- | --- |
| `identity.read_merchant_customers(p_hash,p_store,p_customer,p_limit,p_after_ts,p_after_id,p_q)` | commerce_auth | commerce_runtime | `resolve_access(...,'customers:read')` + GUC check exactly as `read_merchant_orders`. List (limit 1..101, keyset on `(last_activity_at,id)` DESC) or detail (`p_customer`, limit 1). Owners with ≥ 1 order or ≥ 1 bound bundle. `p_q` 1..40 chars: phone digits suffix or case-insensitive name prefix over order destinations. One statement snapshot. |
| `customers.consent_allows(p_tenant,p_store,p_owner,p_purpose,p_channel) → boolean` | commerce_privacy_writer | commerce_auth (R3 grants its planners in its own migration after 0079, C-7) | Latest event granted AND owner active. STABLE. |
| `customers.buyer_set_consent(p_hash,p_store,p_purpose,p_channel,p_granted,p_source,p_policy,p_key)` | commerce_privacy_writer | commerce_buyer_runtime | `buyer.resolve_scope`; owner active; `p_source` ∈ buyer_checkout/buyer_settings and `p_policy` are passed by Go from the server context/constant (CD4), never from the body. Lock `buyer.owners` row FOR UPDATE; look up `(owner,p_key)` (non-erasure rows): found and (purpose,channel,granted,source,policy) equal → return the stored row; found and different → PT409 `idempotency_conflict`; else **always insert** (also when the state is unchanged), so every key is recorded and a stale retry can never re-grant. |
| `customers.merchant_withdraw_consent(p_hash,p_store,p_customer,p_purpose,p_channel,p_key)` | commerce_privacy_writer | commerce_runtime | `customers:privacy`; same lock + key rule as `buyer_set_consent`; always inserts granted=false `merchant_recorded`; audit `customers.consent_withdrawn`. |
| `customers.record_export(p_hash,p_store,p_customer,p_via,p_key,p_summary)` | commerce_privacy_writer | commerce_runtime, commerce_buyer_runtime | Auth per `p_via` (buyer: `resolve_scope` and owner must equal scope owner; merchant: `customers:privacy`); key rule: `(owner,p_key)` exists with kind EXPORT → return it; exists with another kind → PT409 `idempotency_conflict`; else insert EXPORT row; merchant audit `customers.exported`. Called in the same transaction as the export reads. |
| `customers.erase_owner(p_hash,p_store,p_customer,p_via,p_key) → jsonb` | commerce_privacy_writer | commerce_runtime, commerce_buyer_runtime | Buyer via: if the capability hash resolves to a revoked session whose owner has an ERASURE row → PT410 `erased` (a buyer retry after success; the stored summary is for merchant replay only). Otherwise auth as above; lock `buyer.owners` row FOR UPDATE first; `(owner,p_key)` exists with kind ERASURE → return stored summary; with another kind → PT409 `idempotency_conflict`; refuse PT409 `erasure_blocked` (CD7); then `customers.apply_erasure`; insert ERASURE row; merchant audit `customers.erased`. |
| `customers.apply_erasure(p_tenant,p_store,p_owner) → jsonb` | commerce_privacy_writer | none (internal) | CD7 steps, idempotent: withdraw every granted pair (source `erasure`, policy `erasure`, request_key = the ERASURE row id); `capability_sessions.revoked_at=now()` where NULL + `capability.revoked` events; `owners.active=false`; unreferenced snapshots → `recipient_name='[erased]'`, `phone='000000'`, region/city/postal/line1/line2 → `''`, except `kind='home'` city/line1 → `'[erased]'` (existing CHECKs); bound bundles with a non-NULL label → `label='erased-'||replace(id::text,'-','')` (39 chars, within 0060's 1..60 CHECK; the full id keeps it unique under 0060 `claims_bundle_label` UNIQUE(tenant,store,session,label), which an 8-hex prefix or a merchant-typed `erased-xxxxxxxx` label could violate and fail the erasure tx on every retry). **No write to `actor_key`, `claims.meta_intake`, `live.claim_sources` or `social.*`.** Returns counts. |
| `customers.replay_erasures(p_owners uuid[] DEFAULT NULL) → integer` | commerce_privacy_writer | none (migration owner/ops only) | Runs `apply_erasure` for every ERASURE row, or for the given owner ids after a restore (§9), inserting the missing ERASURE tombstone (`via='restore'`, `request_key` = uuid from the external list entry). Without ids it cannot restore erasures made after the dump; only the ids path is the restore procedure. |
| `identity.read_finance_summary(p_hash,p_store,p_from date,p_to date)` | commerce_auth | commerce_runtime | `orders:read`; `p_to-p_from` 0..91; days in `Asia/Taipei`; rows `(day,currency,environment,captured_count,captured_minor,refunded_minor,net_minor)`; refund counted on its SUCCEEDED day unless a later FAILED/CANCELED fact exists. |
| `identity.export_finance_summary(...)` | commerce_auth | commerce_runtime | Same rows; `orders:export` AND `orders:read`; audit `finance.exported` (0063 export pattern). |

Grants for `commerce_privacy_writer` (column-level, each with a FORCE-RLS policy
`privacy_scoped ... TO commerce_privacy_writer USING (tenant_id=GUC tenant AND store_id=GUC store)`). Merchant
definers verify `current_setting('app.tenant_id'/'app.store_id'/'app.principal_id')` equals the `resolve_access`
result exactly as `identity.read_merchant_orders` (0063) and refuse PT403 otherwise; buyer definers follow the
existing buyer-definer pattern (0013 `set_config(...,true)` after `resolve_scope`); no new merchant definer calls
`set_config`:
`customers.*` SELECT, INSERT; `buyer.owners` SELECT, UPDATE(active); `buyer.capability_sessions` SELECT,
UPDATE(revoked_at); `buyer.capability_events` INSERT; `storefront.destination_snapshots` SELECT,
UPDATE(recipient_name,phone,region,city,postal_code,line1,line2); `checkout.orders` SELECT(tenant_id,store_id,
owner_id,id,destination_id,commercial_state,expires_at); `claims.bundles` SELECT(tenant_id,store_id,id,owner_id,
bound_at,label), UPDATE(label); `payments.stripe_sessions` SELECT(tenant_id,store_id,owner_id,expires_at);
`payments.stripe_refunds` SELECT(tenant_id,
store_id,id,owner_id); `payments.refund_facts` SELECT(tenant_id,store_id,refund_id,kind); `ops.audit_events`
INSERT (policy limited to the three `customers.*` actions); EXECUTE on `identity.resolve_access`,
`buyer.resolve_scope`. `commerce_auth` gets SELECT on `customers.*` + policies for its reads.
No login role reads `customers.*` directly.
Amendment (2026-09-30, lane close; reviewer P2 "USING(true) vs GUC-scoped"): the policy text above is the rule for every
WRITE and for reads on other domains' tables. Three reads are deliberately `USING (true)`, restricted to
`commerce_privacy_writer` (migration 0078 header comment): SELECT on `customers.consent_events` /
`customers.privacy_actions` (`customers.consent_allows` must answer from any GUC state, e.g. the R3 sweeper) and on
`buyer.owners` / `buyer.capability_sessions` (the buyer erasure retry must find an already revoked session before any
scope exists to answer 410 `erased`). The tenant/store/owner filter is explicit inside each definer (CB02 asserts the
policy names and quals from `pg_policies`); no login role can reach these tables, only the definers.

### 3.2 `0079_platform_billing.sql`

Preconditions: 0078 present; permission CHECK re-derived and extended with `billing:manage`;
`identity.create_initial_store` replaced (sixth version, C-5) with the 0065 body byte for byte except the grant
array + `customers:read`, `customers:privacy`, `billing:manage`.

```sql
CREATE ROLE commerce_billing_writer NOLOGIN NOSUPERUSER NOBYPASSRLS NOCREATEDB NOCREATEROLE NOREPLICATION;
CREATE SCHEMA billing; REVOKE ALL ON SCHEMA billing FROM PUBLIC;
GRANT USAGE ON SCHEMA billing TO commerce_billing_writer, commerce_auth;

CREATE TABLE billing.store_customers (
 tenant_id uuid NOT NULL, store_id uuid NOT NULL,
 environment text NOT NULL CHECK (environment='SANDBOX'),                 -- BD8
 stripe_customer_id text NOT NULL UNIQUE CHECK (stripe_customer_id ~ '^cus_[A-Za-z0-9]{1,64}$'),
 platform_account_id text NOT NULL CHECK (platform_account_id ~ '^acct_[A-Za-z0-9]{1,59}$'),  -- BD1, from GET /v1/account
 open_checkout_session_id text CHECK (open_checkout_session_id ~ '^cs_[A-Za-z0-9_]{1,255}$'),
 open_checkout_expires_at timestamptz,
 created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 PRIMARY KEY (tenant_id,store_id,environment),
 CHECK ((open_checkout_session_id IS NULL)=(open_checkout_expires_at IS NULL)),
 UNIQUE (tenant_id,store_id,stripe_customer_id),
 FOREIGN KEY (tenant_id,store_id) REFERENCES control.stores(tenant_id,id));

CREATE TABLE billing.subscriptions (
 stripe_subscription_id text PRIMARY KEY CHECK (stripe_subscription_id ~ '^sub_[A-Za-z0-9]{1,64}$'),
 tenant_id uuid NOT NULL, store_id uuid NOT NULL, stripe_customer_id text NOT NULL,
 status text NOT NULL CHECK (status IN ('incomplete','incomplete_expired','trialing','active','past_due','canceled','unpaid','paused')),
 price_id text NOT NULL CHECK (price_id ~ '^price_[A-Za-z0-9]{1,64}$'),
 current_period_start timestamptz, current_period_end timestamptz,
 cancel_at_period_end boolean NOT NULL,
 stripe_created_at timestamptz NOT NULL,
 retrieved_at timestamptz NOT NULL,                                        -- monotone (§4)
 updated_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 FOREIGN KEY (tenant_id,store_id,stripe_customer_id) REFERENCES billing.store_customers(tenant_id,store_id,stripe_customer_id),
 CHECK ((current_period_start IS NULL)=(current_period_end IS NULL)),
 CHECK (current_period_end IS NULL OR current_period_end>current_period_start));
CREATE INDEX subscriptions_store ON billing.subscriptions(tenant_id,store_id);
```

Both FORCE RLS, PUBLIC revoked; `commerce_billing_writer` SELECT/INSERT/UPDATE and `commerce_auth` SELECT,
each with an explicit GUC-scoped policy (the trigger and `apply_subscription` read by store id they resolved
themselves, so their policy is `USING (true)` restricted to that owner role). Set-once trigger: `tenant_id`, `store_id`, `stripe_customer_id`,
`stripe_created_at` immutable; `status='canceled'` is terminal (F-B1).

| Function | Owner | EXECUTE to | Behavior |
| --- | --- | --- | --- |
| `billing.store_standing(p_tenant,p_store) → text` | commerce_billing_writer | commerce_auth only (R3 `commerce_ads_writer` in meta-ads 0080, C-8); runtime uses `identity.read_billing_standing` | BD4 mapping; STABLE; `SECURITY DEFINER SET search_path=pg_catalog`, PUBLIC revoked (an invoker would see no rows under FORCE RLS and fail open to UNBILLED). |
| `billing.guard_window_open()` trigger function; trigger `billing_guard_window_open` `BEFORE INSERT OR UPDATE OF state ON live.claim_windows FOR EACH ROW WHEN (NEW.state='OPEN')`, created in 0079 | commerce_billing_writer | trigger only (`SECURITY DEFINER SET search_path=pg_catalog`, EXECUTE revoked from PUBLIC) | Fires when `TG_OP='INSERT' OR OLD.state<>'OPEN'`; RESTRICTED → `RAISE ... ERRCODE='PT412'`. Only new opens; an OPEN window is never closed by billing. Definer because `commerce_runtime` writes `live.claim_windows` directly (0060 GRANT UPDATE(state,…)) and must never get SELECT on `billing.*`. |
| `billing.platform_account_conflict(p_account text) → boolean` | commerce_billing_writer (SELECT(provider,account_id) on `integration.merchant_accounts` + read policy) | commerce_runtime, commerce_stripe_ingress | True when `p_account` equals any `provider='stripe'` `account_id` (any environment) or is malformed (BD1). |
| `identity.read_billing(p_hash,p_store) → jsonb` | commerce_auth | commerce_runtime | `billing:manage`; standing, subscriptions (status, price_id, period, cancel flag, retrieved_at), pinned customer presence, usage counts for the current period (BD6; UNBILLED → calendar month Asia/Taipei). |
| `identity.read_billing_standing(p_hash,p_store) → text` | commerce_auth | commerce_runtime | `store:read`; standing only (banner for every member). |
| `billing.pin_customer(p_hash,p_store,p_env,p_customer,p_account)` | commerce_billing_writer | commerce_runtime | `billing:manage` + GUC check (0063 pattern); `platform_account_conflict(p_account)` → PT409 `billing_account_conflict`; set-once per (store, env); same id replay OK, different id PT409. Audit `billing.customer_pinned`. |
| `billing.apply_subscription(p_env,p_customer,p_subscription,p_status,p_price,p_ps,p_pe,p_cancel_end,p_created,p_retrieved_at,p_meta_store uuid,p_livemode boolean) → text` | commerce_billing_writer | **commerce_stripe_ingress only** (C-4) | PT400 when `p_livemode` (BD8) or `p_retrieved_at > clock_timestamp()+interval '5 seconds'`. Resolve store **only** through `store_customers.stripe_customer_id`; unknown → `'unknown_customer'` (no write). `p_meta_store` NULL (e.g. Dashboard-created) or ≠ resolved store → `'mismatch'` (no write, error log + ops alert). A second subscription with status ∉ {canceled, incomplete_expired} for the store while another non-terminal one exists → still mirrored, returns `'duplicate'` + ops alert (no auto-cancel, BD2). Upsert; update only if `p_retrieved_at > retrieved_at`, else `'stale'`. Returns `'applied'`. |
| `billing.refresh_subscription(p_hash,p_store, <same Stripe fields as apply_subscription>) → text` | commerce_billing_writer | commerce_runtime | `billing:manage` + GUC check; the resolved customer's store must equal `p_store` (else PT404); then the `apply_subscription` body. Used by refresh-on-read and the checkout pre-check (§5). |
| `billing.record_checkout_session(p_hash,p_store,p_env,p_session,p_expires) → text` | commerce_billing_writer | commerce_runtime | `billing:manage` + GUC check; lock `store_customers` row FOR UPDATE; if another open session with `open_checkout_expires_at > now()` is stored, returns its id (caller expires it) and stores the new one; else stores and returns NULL. `p_expires ≤ now()+31 min`. |

## 4. State machines

Standing (derived, BD4):

```
UNBILLED ──checkout completes──► GOOD (trialing|active)
GOOD ──invoice fails──► GRACE (past_due)  ──retries exhausted──► RESTRICTED (unpaid|canceled per Q7)
GRACE/RESTRICTED(unpaid) ──latest invoice paid (portal)──► GOOD
RESTRICTED(canceled) ──new checkout, paid──► GOOD (canceled is terminal per subscription; a new sub row is added;
                      no trial on it (BD2); while it is `incomplete` standing stays RESTRICTED (BD4))
```

Consent per (owner, purpose, channel): `absent(=no) → granted (buyer) → withdrawn (buyer|merchant|erasure) →
granted (buyer only, not after erasure because the owner is inactive)`.

Privacy actions: none (synchronous rows). Erasure is one-way per owner (`privacy_one_erasure`).

## 5. HTTP and UI surfaces (T18)

Merchant (Go under `/v1/admin/stores/{store_id}/…`, admin BFF mirrors under `/api/stores/[store]/…` via
the existing allowlist proxy; same session/CSRF/Origin rules as orders; bodies strict and bounded):

| Method + path | Permission | Result |
| --- | --- | --- |
| `GET customers?limit&after&q` | customers:read | list rows: `customer_id, first_seen_at, last_activity_at, display_name, phone_last3, orders_count, paid_orders_count, captured_minor, refunded_minor, currency, claims_count, platforms[], consents{marketing_messages,ads_personalization}, active` |
| `GET customers/{id}` | customers:read + orders:read (amended, R1 review P1-2) | row + `orders[]` (merchant-orders-v1 Summary shape, newest 50) + `claims[]` (`session_id, platform, bound_at, line_count`) + consent history + privacy actions |
| `POST customers/{id}/consent-withdrawals` `{purpose,channel}` + Idempotency-Key | customers:privacy | 201 |
| `POST customers/{id}/exports` + Idempotency-Key | customers:privacy + customers:read + orders:read (amended, R1 review P1-2) | 200 `application/json` attachment `lc.customer-export.v1` (≤ 1 MiB, ≤ 200 orders else 409 `export_too_large`), `Cache-Control: no-store` |
| `POST customers/{id}/erasure` `{confirm:"ERASE"}` + Idempotency-Key | customers:privacy | 200 summary counts; 409 `erasure_blocked` |
| `GET finance/summary?from&to` / `GET finance/summary.csv?…` | orders:read / +orders:export | daily rows + totals |
| `GET billing` | billing:manage | §3.2 `read_billing` + `plans[]` (configured price ids with name/amount/currency/interval from Stripe, 10-min process cache) + `stale` flag |
| `GET billing/standing` | store:read | `{standing}` |
| `POST billing/checkout` `{price_id}` | billing:manage | 200 `{url}`; 409 `subscription_exists`; 503 `billing_unavailable` (BD1 conflict) — steps below |
| `POST billing/portal` | billing:manage | 200 `{url}`; 409 `no_billing_customer` |

`POST billing/checkout` steps (no DB transaction open during Stripe calls): (1) `GET /v1/subscriptions?customer=<pinned>
&status=all` and apply every result through `billing.refresh_subscription`; (2) 409 `subscription_exists` if any
status ∉ {canceled, incomplete_expired}; (3) trial per BD2; create the session with `expires_at` = now+30 min (F-B4
minimum) and `subscription_data.metadata.lc_store`, **without** a Stripe Idempotency-Key (a wall-clock `expires_at`
would change the params on retry and Stripe rejects a reused key with different params); (4) `billing.record_checkout_session`; if it returns an older
unexpired session id, `POST /v1/checkout/sessions/{id}/expire` it (own idempotency key per session id). Two tabs, or a
retry, therefore leave at most one open session; a session whose create response was lost is never returned to anyone
and expires within 30 min. Operator prerequisite (§10): Dashboard
"limit customers to one subscription" + portal login link enabled (F-B12), which also covers the gap outside trials.

Amendment (R1 review P1-2): detail and export return order summaries/details, which carry order and destination PII,
through the one existing projection (`merchantorders.Get`, gated by `orders:read`). A principal therefore needs
`orders:read` in addition to the row permission; detail needs `customers:read`+`orders:read`, export needs
`customers:privacy`+`customers:read`+`orders:read` (export builds on the detail read). This is fail-closed and keeps a
single order-PII gate; the consent-withdrawal and erasure rows are unchanged (no order PII returned).
Amendment (2026-09-30, lane close; reviewer P2): the LIST row is not PII-free by design. Under `customers:read` alone it
returns minimized destination PII taken from the latest order destination: `display_name` (recipient name) and
`phone_last3` (last three phone digits), and `q` matches the recipient-name prefix or the phone-digit suffix (D6). That
minimum is what makes a customer recognizable in the list; the full destination, address, items and payment facts
stay behind `orders:read` (detail/export). Asserted in CB09Permissions (list with `customers:read` alone: 200, `phone_last3`
has at most 3 digits, no other destination field).

Export content: store name, customer_id, orders (existing merchant order Detail shape, produced by the
existing projection per order id), consent history, claims summary (platform + time, never actor_key),
privacy actions. Buyer export uses the existing buyer order-history detail shape instead.

Buyer (storefront host-resolved store, capability cookie; `/v1/buyer/…`):
`GET privacy` (consents + whether an erasure exists), `PUT consents` `{purpose,channel,granted,context:"checkout"|"settings"}`
+ Idempotency-Key (server maps context → source and sets `policy_version` from `LC_PRIVACY_POLICY_VERSION`, CD4;
R3 extension point after a committed `ads_personalization` grant: `ads.put_capi_context`, owned by meta-ads-v1, C-7),
`POST privacy/export`, `POST privacy/erasure` `{confirm:"ERASE"}`. A buyer retry after a completed erasure gets
410 `erased` (capability revoked; the stored summary is only for merchant replay).
Erasure revokes the caller's capability; the response tells the buyer their order records are kept for
legal retention and that the store can still contact them about existing orders.

Platform: `POST /v1/platform/stripe/webhook` — body ≤ 64 KiB, existing `WebhookVerifier` with
`LC_PLATFORM_STRIPE_WEBHOOK_SECRET`; handled types: `customer.subscription.{created,updated,deleted,paused,resumed}`,
`invoice.{paid,payment_failed}`; others 200 ignored. Keys only from env (`LC_PLATFORM_STRIPE_SECRET_KEY`
must start `sk_test_`/`rk_test_`, `LC_BILLING_PRICE_IDS` comma list; production values come from owner-supplied
secret files, never chat, O-D). Events with `livemode=true` are rejected (BD8). Portal/Checkout URLs are bearer links:
never logged, never stored (I11).

Admin screens (`apps/admin/app/[locale]/…`, each file names its BFF route and Go endpoint, PROCESS §5):
- `customers`: search box + one table (reuse the orders table component; no card grid); row → detail.
- `customers/[customer]`: header facts, orders table (links to existing order detail), claims list, consent
  rows with "Record withdrawal", privacy section: "Download data" and "Erase" (typed `ERASE` confirm dialog
  stating what is kept).
- `finance`: date range (native `<input type="date">`), daily table + totals row, CSV link, test-mode badge.
- `billing`: standing banner, plan choices → Stripe Checkout redirect, "Manage payment & invoices" → portal,
  usage-this-period table.
- Layout: GRACE/RESTRICTED banner (from `billing/standing`) on every page, one shape, linking to billing.
- Studio: `billing_restricted` on window open shows the reason + billing link.
Storefront: checkout page two unticked checkboxes + privacy notice link (policy version constant);
`/[locale]/privacy` buyer page; `/[locale]/data-deletion` public instructions page (Meta App Review URL, F-P2).
Audit-first for UI: no new summary-card grids; reuse the existing table, dialog and banner components.

## 6. Idempotency and uniqueness

| Operation | Key | Rule |
| --- | --- | --- |
| Consent write | `(owner, request_key)` (non-erasure rows) | same key + same normalized body → 200 stored result; same key + different body → 409 `idempotency_conflict` (I02); every new key inserts a row even when state is unchanged |
| Export / erasure | `(owner, request_key)`; one ERASURE per owner | same key + same kind → stored result (erasure: stored summary to the merchant; buyer retry → 410); same key + different kind → 409 `idempotency_conflict` |
| Stripe customer create | `lc:billing:customer:v1:<store>:<env>` | then `pin_customer` set-once |
| Stripe checkout session | none (body carries wall-clock `expires_at`) | every call creates a new session; `record_checkout_session` + expire keep at most one open session per store |
| Checkout session expire | `lc:billing:expire:v1:<session id>` | expire cannot move money |
| Portal session | none (no state, 5-min bearer URL) | new URL per click |
| Subscription apply | `stripe_subscription_id` + monotone `retrieved_at` | out-of-order webhooks cannot regress state |

## 7. Bounds (I23)

List limit ≤ 100; `q` ≤ 40 chars; detail orders ≤ 50; export ≤ 200 orders and ≤ 1 MiB; finance range ≤ 92
days; webhook body ≤ 64 KiB; Stripe calls 10 s timeout, no retry inside a request (webhook relies on Stripe
retries; merchant sees an error and retries; checkout retries rely on §5 step 4); plan cache 10 min, ≤ 10 price ids.

## 8. Test gates

| Gate | Tier | Proves |
| --- | --- | --- |
| CB01 | UNIT | BD4 mapping for all 8 statuses + none + mixed rows; consent current-state derivation; export JSON schema/bounds; PT412→402 `billing_restricted` mapping (PT402 keeps meaning insufficient stock). |
| CB02 | REAL_PG | Exact owners, `search_path`, ACLs, schema USAGE, FORCE RLS + policies for every new object; PUBLIC/buyer/worker roles cannot read `customers.*`/`billing.*`; permission CHECK contains the 3 new values; fresh creator gets them once, replay adds nothing, other members and earlier principals get nothing, `create_initial_store` body diff vs 0065 = array only. |
| CB03 | REAL_PG | Two tenants, two stores, three owners. Forwarded link (Alice's bundle redeemed by Bob's owner) counts under Bob only and creates no merge (G09 case 1). Aggregates equal facts (captured, refunded, later-failed refund excluded). No actor_key/session id/PSP ref in any output. Keyset stable on equal timestamps. `q` filter. |
| CB04 | REAL_PG | Buyer grant/withdraw, merchant withdraw, merchant grant impossible (CHECK + no function), `consent_allows` false after withdrawal and after erasure, cross-store isolation, idempotent replay. Same key + different `granted` → PT409; unchanged-state write still records its key, so grant(K, already granted) → withdraw(K2) → retry K returns stored row and state stays withdrawn; privacy_actions EXPORT key reused for ERASURE → PT409; concurrent grant/withdraw on one owner serialize (owner lock). Request body `policy_version`/`source` rejected as unknown fields. |
| CB05 | REAL_PG | Erasure refused with open order / unexpired `payments.stripe_sessions` row / non-terminal refund; success revokes sessions, deactivates owner, redacts only unreferenced snapshots, relabels manual bundles to `erased-`+32 hex (a pre-existing merchant label `erased-<first 8 hex of the bundle id>` in the same session does not fail the erasure); bundle `actor_key`s, `claims.meta_intake` and `social.*` rows byte-identical before/after, and a second owner who redeemed the same actor's link is unaffected; orders/facts/snapshots referenced by orders byte-identical; replay idempotent; buyer retry after erasure → 410. Restore model (G09 case 3; MODEL of restore until T20): dump pre-erasure DB, restore to a fresh DB; red run = `replay_erasures()` without ids leaves consent granted; green = `replay_erasures(ids)` from the external list → consent false, owner inactive, ERASURE tombstone (`via='restore'`) inserted. |
| CB06 | REAL_PG | `apply_subscription`: unknown customer, metadata mismatch and NULL metadata, stale `retrieved_at`, future `retrieved_at` + `livemode` → PT400, canceled terminal, second non-terminal sub → `duplicate`; `refresh_subscription` for another store's customer → PT404; runtime has no EXECUTE on `apply_subscription`/`store_standing`. Standing: canceled + incomplete → RESTRICTED; incomplete alone → UNBILLED. Claim window open **as the real `commerce_runtime` login** refused under RESTRICTED (PT412, not a permission error), allowed under GOOD/GRACE/UNBILLED, already-open window stays open. `platform_account_conflict` true for a registered PSP account id. |
| CB07 | REAL_PG | Under RESTRICTED: buyer checkout of an existing cart, payment apply, refund request, shipment record, merchant order read/export and privacy actions all succeed. Catalog check: no function in schemas `checkout`, `payments`, `fulfillment`, `buyer`, `storefront` references `billing.` (`pg_proc.prosrc`). |
| CB08 | MOCK | Existing `stripetest` fake extended: account/customer/checkout/expire/portal/subscription retrieve+list; `GET /v1/account` id equal to a registered PSP account → billing disabled, `billing_account_conflict` logged; two checkouts with different keys in parallel → one open session (older expired); prior subscription on the customer → no `trial_period_days` in the create body; livemode event rejected; customer-create and expire idempotency keys stable and bodies byte-identical on retry; checkout create sends no Idempotency-Key and a retry leaves one open session (older expired); forged/old signature 400; `invoice.*` resolves the subscription via `parent.subscription_details`; retrieve failure → 503; period fields per F-B10 on the pinned API version; portal/checkout URLs absent from logs. |
| CB09 | HTTP_PG | Every endpoint in §5: permissions, 404 indistinguishable from other store, strict bodies, Idempotency-Key required where §5 lists it, export/CSV headers (`no-store`, attachment), no secrets in errors. |
| CB10 | SANDBOX | Platform sandbox account (separate from every PSP account): the test creates the Customer via the API with `test_clock` (F-B13), then calls `pin_customer`; webhooks via `stripe listen --forward-to` to the local server; checkout with 4242 → `active` → GOOD; switch default payment method to a test card that attaches but fails charges, advance the clock → `past_due` GRACE → `unpaid` RESTRICTED → pay in portal → GOOD. Prerequisites: platform sandbox secret in env (O-D), Dashboard failed-payment setting `unpaid` (Q7), no Billing Automations on the account (F-B13). |
| CB11 | BROWSER | Playwright: admin customers list/detail/withdraw/export/erase, finance range + CSV, billing banner/subscribe redirect (mock) / portal, studio refusal message; storefront checkout checkboxes, privacy page, public data-deletion page; zh-TW + en. |
| CB12 | LIVE | NOT_RUN. Requires Q1 entity, live keys and owner approval in chat. |

Each gate records one red run before green (PROCESS §2.4). Test author ≠ implementer.

## 9. Ownership, NOT_RUN and deferred scope

| Piece | Role | Write paths |
| --- | --- | --- |
| 0078, 0079 SQL | commerce_worker; integrator merges | `migrations/0078_customers_privacy.sql`, `migrations/0079_platform_billing.sql` |
| Customers/privacy Go + HTTP | commerce_worker | `internal/customers/**` (C-2), route wiring in `internal/httpapi` |
| Billing Go (Stripe platform client reusing `internal/integrations/psp/stripe` verifier/strict JSON) | integration_worker | `internal/billing/**` |
| Finance summary | commerce_worker | `internal/reporting/**` |
| Admin + storefront screens | ui_worker | §5 routes, BFF allowlist entries |
| CB gates | test_worker | package tests + `tests/browser/**` |
| Review | security_reviewer | privacy writer grants, erasure SQL, webhook |

Pre-0079 stores (C-5): owner-approved operator script `scripts/ops/grant-r2-permissions.sql` grants
`customers:read`, `customers:privacy`, `billing:manage` to the store creator only, writes audit
`store.permissions_granted`, is idempotent, and is never run automatically (not a migration).

All CB gates NOT_RUN. Deferred, stated explicitly:
- Identity merge / verified buyer contact (email or phone OTP), touchpoint graph, segments/RFM → needs a verified-contact contract; R3 for ad touchpoints.
- Any marketing send, CAPI, audience export → R3. CAPI → R3 (meta-ads-v1), calls `consent_allows` in the planning tx; every send/audience planner likewise calls `consent_allows` (CD5); actor-keyed sends/audiences additionally need actor-level opt-in recorded with the Meta actor as subject. Cancel queued jobs on withdrawal (I22).
- Retention of `social.messages` / `social.comment_events` ciphertext and `claims.meta_intake.comment_ref` (0029/0064 have no expiry) and actor-level deletion → U08 (an R2 unit, ruling X4); not solved by owner erasure (CD7).
- Claims production-mount blocker (live-keyword-claims-v1 §8/§11.4, meta-claims-intake-v1 §1/§8) is NOT lifted by this contract. CB05 covers only `bundles.label` on owner erasure. `bundles.actor_key`, bindings (`owner_id`,`bound_at`), `claims.links` token hashes, `claims.meta_intake` (`actor_key`,`comment_ref`) and `social.*` still need the U08 retention purge job plus actor-level deletion, each in its own contract with its own gate. Until then claims production mount stays blocked, unless the integrator records a waiver.
- **Waiver W1** (integrator, r2-design-rulings.md Round-3 X4): the pilot (owner's own store only) may run claims in production until U08 lands. U08 (retention purge + actor-level deletion of `bundles.actor_key`, bindings, `claims.links` hashes, `claims.meta_intake`, `social.*`) is an R2 unit and must land before a second merchant is onboarded; W1 lapses at that point.
- Meta data deletion **callback** (signed_request, app-scoped id cannot be mapped to page-scoped actor keys without Facebook Login); v1 ships the instructions page (F-P2).
- Legal-retention purge of order PII → after U08 sets periods.
- Copy of erased owner ids outside DB backups and the restore-replay drill → T20 (no production host yet).
- Usage metering ledger, overage pricing, Stripe meters/Metronome, Stripe Tax on platform fees, invoice mirror.
- Tenant-level billing across several stores; platform security suspension state; platform admin UI (plans are Stripe Prices listed in env).
- Profit/COGS reporting (R18 part); pending privacy request tracking with PDPA deadlines (merchant handles offline in v1).
- ponytail ceilings: customer list and usage are read-time aggregates (materialize when p95 > 300 ms at a measured store size); billing sync relies on webhooks + refresh-on-read (add a daily reconcile job when stores > ~100 or a missed webhook is observed).

## 10. Owner questions (genuine owner inputs only; each has a default)

1. **Platform billing entity and account (Q1).** Stripe accounts cannot be opened in Taiwan (F-B8), and the
   platform account must not be the account bound to a store for buyer payments (BD1, stripe-psp D1). Is the Stripe
   account you plan to switch to live the same one that will take buyer payments? Default: open a **separate**
   platform Stripe account under a Hong Kong entity (香港大碗貿易有限公司 is the entity already used for Meta, O-C)
   for merchant fees; the buyer-payment account stays the store's own.
2. **Plans and prices (Q2).** Default: one monthly plan, 14-day trial once per store (BD2), card collected in
   Checkout; you set price and currency in the Stripe Dashboard and send the price id (not a secret).

Operator prerequisites on the platform Stripe account (not questions; done by the operator per O-D before CB10/LIVE):
customer portal activated with allowed products and its login link kept enabled; Checkout setting "limit customers
to one subscription" on (F-B12); failed-payment setting → `unpaid` (Q7); platform webhook endpoint + signing secret
(`LC_PLATFORM_STRIPE_WEBHOOK_SECRET`) and API key delivered as files (O-D); no Billing Automations on the sandbox
(F-B13).

## 11. Round-1 finding ledger (review key `customers-billing`)

| # | Sev | Finding | Result |
| --- | --- | --- | --- |
| 1 | P1 | Erasure rotated actor_key / meta_intake | Fixed (CD7, §3.1 grants/apply_erasure, CB05). Verified: `ClaimActorKey` deterministic HMAC (`claim_intake.go:89`), 0060 `UNIQUE(...,session_id,platform,actor_key)`, 0029 `social.*` ciphertext, 0064 `comment_ref`. |
| 2 | P1 | Owner consent could authorize DM to another actor | Fixed (CD5 rule, §9). Not raised as owner question: the safe default is the only compliant one; R3 decides the mechanism. |
| 3 | P1 | Consent/privacy idempotency and ordering (I02/I22) | Fixed (CD4 lock, `consent_one_key`, function rules, §6, CB04). |
| 4 | P1 | Double subscription via two tabs / missed webhook | Fixed (§5 checkout steps, `record_checkout_session`, `duplicate`, CB08). Fact re-verified: F-B12 (trialing not counted). |
| 5 | P1 | Standing bypass via `incomplete`, repeatable trials | Fixed (BD4 filter, BD2 trial-once, §4, CB06/CB08). |
| 6 | P1 | Platform billing account may equal the store PSP account | Fixed (BD1, `platform_account_conflict` over `integration.merchant_accounts` (0014/0061), `store_customers.platform_account_id`, Q1 rewritten, operator prerequisites). |
| 7 | P2 | `apply_subscription` metadata/future/livemode/grants | Fixed (§3.2 signature, ingress-only, `refresh_subscription`, `store_standing` to commerce_auth only). |
| 8 | P2 | Trigger must be SECURITY DEFINER | Fixed (§3.2 row; verified 0060:240-241 runtime writes `live.claim_windows` directly). CB06 runs as real runtime login. |
| 9 | P2 | Definers set GUCs | Fixed (§3.1): merchant definers verify as 0063:525-528; buyer definers keep the 0013 pattern (which does call `set_config`, verified 0013:334-338). |
| 10 | P2 | Client-supplied policy_version/source | Fixed (CD4, §5 body). |
| 11 | P2 | Erasure between order expiry and session expiry | Fixed (CD7 refusal on `payments.stripe_sessions.expires_at`, verified 0061 CHECK `+40 min` vs 0013 `≤15 min`; 410 `erased`). Used the session expiry alone (no "terminal fact" join): after `expires_at` Stripe cannot capture a new payment, so it is the simpler sufficient bound. |
| 12 | P2 | C-5 version count; pre-0079 stores | Fixed (C-5 = sixth version, verified CREATE [OR REPLACE] in 0004/0007/0019/0027/0065; §9 operator script). |
| 13a | P2 | CB05 replay without ids cannot fail | Fixed (red/green ids path, tombstone `via='restore'`). |
| 13b | P2 | CB09 tier HTTP_PG "not allowed" | **Rebutted**: HTTP_PG is an established tier in frozen contracts (manual-fulfilment-v1 §6 "tiers: UNIT, REAL_PG, HTTP_PG, BROWSER"; merchant-password-auth-v1 §9). Evidence *labels* (PROCESS §4) are a separate axis. Kept. |
| 13c | P2 | CB10 test clock only at creation | **Partly rebutted**: an existing customer can be attached with limits (F-B13). Adopted the reviewer's create-with-`test_clock` path anyway (no Automations/backdating constraints); fact now VERIFIED. |
| — | P2 (self) | Rejected-alternatives cited nonexistent F-B11 | Fixed → F-B9. |
| R2-1 | P1 | CD5/§9 required actor-level opt-in for CAPI, contradicting meta-ads-v1 AD8; A-3 grant to `commerce_ads_writer` unanswered and order-unsafe; `ads.put_capi_context` hook unmentioned | Fixed with the reviewer's text (CD5, §9, C-7, §3.1 EXECUTE column, §5 hook). Verified: meta-ads §6.4 CAPI `user_data` = destination phone hash + HMAC(owner id) + UA from `ads.capi_contexts` keyed by owner, no `actor_key`; `migrate.go:75` `fs.Glob` + `applyVersions` fill missing lower versions after higher applied ones; `commerce_ads_writer` is created only in meta-ads 0074. |
| R2-2 | P1 (round 3) | T14 did not say the claims production-mount blocker stays in force | Claims production-mount blocker (live-keyword-claims-v1 §8/§11.4, meta-claims-intake-v1 §1/§8) is NOT lifted by this contract. CB05 covers only `bundles.label` on owner erasure. `bundles.actor_key`, bindings (`owner_id`,`bound_at`), `claims.links` token hashes, `claims.meta_intake` (`actor_key`,`comment_ref`) and `social.*` still need the U08 retention purge job plus actor-level deletion, each in its own contract with its own gate. Until then claims production mount stays blocked, unless the integrator records a waiver. Waiver W1 recorded (X4, §9): owner's own pilot store only; U08 is an R2 unit, lands before a second merchant. Verified: live-keyword-claims-v1:920-922, §11.4 (line 1039); meta-claims-intake-v1:86, 640, 734. |
| R3-P2 | P2 ×6 (round 3) | C-7 stale renumbering; ads half of Q6 unenforced; checkout `expires_at` vs Stripe idempotency; PT402 reuse; recipient phone in CAPI; 8-hex erased label collision | Fixed: C-7 (verified meta-ads A-11, §4.3 line 271); C-8 + `store_standing` definer (meta-ads already calls it at approve/publish with 409 — status mismatch left to integrator); §5 step 3/§6/§7/CB08 drop the Stripe key for session create; PT412 (verified PT402 = insufficient stock at 0013:487, post_river/0005:279, `checkout.go:354`; PT412 unused); CD5 recipient = buyer or omit `ph` (to mirror in meta-ads); full-hex label (verified 0060:99 CHECK 1..60, :112 UNIQUE index) + CB05. |

## 12. Integrator rulings (2026-09-29)

Binding owner decisions (`docs/delivery/units/r2-design-rulings.md`) as they apply here:
- **Round-3 rulings** (`docs/delivery/units/r2-design-rulings.md` §Round-3 X1, X4, X6): the round-3 P1 applied with the
  reviewer's exact text (§9, §11 R2-2); claims production-mount blocker stays, waiver W1 for the owner's own pilot
  store, U08 is an R2 unit before a second merchant (§9); contract FROZEN 2026-09-30, open owner questions keep their defaults.
- **O-A/O-B**: not used by this contract (no merchant login or email send here; buyer has no verified email).
- **O-C**: Meta app 4291253377792879 under 香港大碗貿易有限公司; its App Review needs the public
  `/[locale]/data-deletion` instructions page (F-P2, Q9) shipped here. Default entity for Q1 follows O-C.
- **O-D**: Claude operates production; platform Stripe key and webhook secret come from owner-supplied files into
  env on the server, never through chat; SANDBOX only in v1 (BD8).

Accepted defaults (owner may revise before go-live):
- **Q3** billing unit = per store. **Q4** stores with no subscription (pilot) unrestricted (UNBILLED).
- **Q5** plan limits shown, never blocking. **Q6** arrears (RESTRICTED) blocks only new claim windows (v1) and new
  ads (R3); never refunds, fulfilment, buyer checkout, order reads/exports, privacy actions.
- **Q7** Stripe failed-payment setting → `unpaid` after retries (operator prerequisite §10).
- **Q8** platform standard privacy notice (`LC_PRIVACY_POLICY_VERSION`, first value `lc-2026-10`); no automatic purge
  until retention periods are set (U08).
- **Q9** Meta data deletion = public instructions page, no callback in v1.
- **Q10** buyer self-service erasure allowed with typed `ERASE` confirmation.
- **Q11** finance day boundary UTC+8 (`Asia/Taipei`).
- **Q12** consent channels v1 = Messenger/IG DM marketing + Meta ads personalization only (per CD5: necessary, not
  sufficient, for any actor-keyed R3 send/audience; sufficient alone for owner-subject CAPI Purchase, meta-ads AD8).

Open owner inputs: Q1 (platform billing entity/account), Q2 (plan price/currency) — §10. Integrator rulings still
needed from the integrator (not the owner): C-1, C-2, C-3, C-4, C-6, C-7, C-8 (§0.1; C-7/C-8 default = R3 grants in meta-ads
`0080_meta_capi.sql`); C-5 is a correction, not a choice.

## Amendment by claims-retention-purge-v1 (integrator, 2026-09-30, U08 merge)

Recorded from `contracts/claims-retention-purge-v1.md` §6 (FROZEN 2026-09-30); that file is the source of the rows.

- Clause 7: the §9 U08 bullets point to claims-retention-purge-v1; CD7 unchanged (owner erasure still never touches
  actor data). Note: C2 clears bindings after `claims_days`, so the CB03 projection loses old claims.
- IR-U1 (ruling B23): 0071 `bundle_label_reserved` admits `label = 'erased-'||replace(id::text,'-','')` (a bundle's
  own id only), so CD7's `customers.apply_erasure` relabel does not hit 23514.
