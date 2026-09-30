# Stripe PSP v1 — Checkout Session lifecycle, webhook intake and deadline closure

Status: Stage A accepted separately; stage B consistency amendment §0.2 is
**FROZEN / IMPLEMENTATION_GATES_REQUIRED** on base `529cb0f` (2026-09-29).
Independent review of `80dea8e` closed the scope/dedupe/atomicity concerns; its remaining
row-lock-authority P1 was adjudicated by the integrator using the existing `0008` lock-only
column-grant/RLS pattern below. This is an interface decision, not a PostgreSQL test PASS.
§0.1 records the owner answers;
§0.2 reconciles their incomplete propagation into the original draft below.
Evidence label for this file: DESIGN; implementation/gate results live in delivery evidence,
not in this contract. No gate is proved merely by this file being frozen.
MOCK, SANDBOX, BROWSER and LIVE are separate evidence classes (架构 §28.1). A MOCK pass never
becomes SANDBOX evidence, and a SANDBOX pass never becomes LIVE evidence.

This file extends [payment-start](payment-start-v1.md), [payment-query](payment-query-v1.md),
[payment-capture](payment-capture-v1.md), [payment-hosted](payment-hosted-v1.md),
[payment-worker-runtime](payment-worker-runtime-v1.md),
[legacy-runtime-isolation](legacy-runtime-isolation-v1.md) and the buyer payment
[http](buyer-payment-http-v1.md)/[public](buyer-payment-public-v1.md)/[ui](buyer-payment-ui-v1.md)
contracts. Those contracts remain in force for PAYUNi. Architecture §2.2, §11.5, §12, §16 and §21.3,
plus invariants I01–I06, I11, I14, I16, I19, I20 and I24, are authoritative. Where they conflict
with this file, they win and this file is wrong.

## 0. Owner input, conflicts and decisions

**Owner decision 2026-09-28.** The orchestrating integrator recorded it; nobody re-confirmed it in
this session.
- The launch PSP is Stripe, using sandbox account `acct_1UJDb0RusP6Wwj7e` (livemode=false).
- The Go runtime reads `STRIPE_SECRET_KEY` (`sk_test_…`) and `STRIPE_WEBHOOK_SECRET` (`whsec_…`)
  from the environment.
- `sk_live_` is refused unless an explicit LIVE flag and owner approval are both present.

**Integrator constraints carried forward.**
- The 15-minute HOLD applies only before StartPayment.
- After StartPayment, the reservation is PAYMENT_PENDING and its deadline is the Stripe session
  `expires_at` (≥30 min).
- At the deadline, a worker calls `/expire` and then retrieves the session.
- Stock is released only after Stripe confirms the session is expired and unpaid.
- A payment that arrives after release goes to reallocation, or to PAID_ALLOCATION_FAILED plus a
  refund work item. It is never silently marked failed.

**Conflict found.** The env-key model conflicts with the established credential model.
[merchant-accounts-v1](merchant-accounts-v1.md) stores credentials per merchant in the database under
AEAD encryption. Architecture §2.2 forbids pooling merchant funds on one platform account.
D1 resolves this for exactly one store. Serving several merchants needs Stripe Connect, which
requires a new ADR.

| # | Decision | Why / risk closed |
| --- | --- | --- |
| D1 | **Custody `ENV_PLATFORM`**: one Stripe account per environment per deployment. That account is bound to **exactly one tenant/store** by an operator-only registrar (§13). A partial unique index on `(environment) WHERE provider='stripe'` enforces this, and merchants can never create or rebind the account (restrictive RLS). The Stripe secret is never stored in PG. PG stores only a keyed fingerprint per credential version. | Satisfies §2.2 **only if** the account is that store's merchant-of-record account (Q1). The index makes it impossible for one platform account to collect money for two stores. Multi-merchant = `stripe-connect-v1` + ADR. |
| D2 | **Reuse, not a parallel pipeline.** Stripe reuses the existing attempts, the `BUYER_PAYMENT_QUERY` operation family, `provider_observations`, `facts`, `review_cases`, `payment_work_items`, River kinds `payment_query_v1`/`payment_reconcile_v1`, the profile queues, the `river_payment` schema, `CaptureWorker` and `payments.apply_capture`. It adds provider value `stripe`, method code `stripe_checkout`, one River kind `payment_signal_v1` and three tables (§6.2). | Avoids a second trading engine (AGENTS.md). The PAYUNi code paths keep their frozen text: `apply_capture` is renamed, not retyped (§6.4). |
| D3 | **The worker creates sessions. The API never holds `sk_`.** No business transaction performs network I/O. | A crash in the API cannot leave an untracked remote session. The public-facing process carries a smaller secret blast radius. |
| D4 | **Fixed session timing.** `expires_at = date_trunc('second', attempt.created_at) + 40 min`. The first create must be sent before `created_at + 7 min`. Handoff stops at `expires_at − 5 min`. A never-disclosed-session closure waits until `expires_at + 15 min`. | Satisfies Stripe's rule of 30 min–24 h measured from Stripe's own creation time: the latest first send leaves ≥ 31.8 min, allowing ≤60 s clock skew and ≤10 s latency. Stock is held for at most about 40 min after payment start plus closure latency (Q2). |
| D5 | **Stock is released only by a `CLOSED_UNPAID` fact.** That fact requires one of: (a) Stripe retrieve shows `expired` + `unpaid` and the PaymentIntent has not succeeded; (b) the create was never sent; (c) the first send got a definitive rejection; or (d) the URL was never disclosed, DB now ≥ `expires_at + 15 min`, and a list scan finds no session. Timeout, 404, 5xx and an elapsed deadline alone never release stock. | Implements §11.5 and the owner rule "never release while the Stripe page can still take money". |
| D6 | **Card only, automatic capture.** Sessions set `payment_method_types[0]=card`. Status `paid` produces a `CAPTURED` fact. Stripe never produces an `AUTHORIZED` fact. Async events are processed defensively and lead to review (Q3). | Stripe's dashboard-dynamic methods could otherwise add delayed-notification methods. Their release semantics would need a PaymentIntent-level protocol, which is not designed here. |
| D7 | **The amount is pinned.** The session has one line item equal to the frozen order total. Promotion codes, discounts, automatic tax, shipping options, adjustable quantity, Managed Payments, Adaptive Pricing and after-expiration recovery are all **off**. Any money drift creates a review case. Nothing that violates I05 becomes a fact. | Managed Payments was ON by default on this account (2026-09-25 evidence). Adaptive Pricing showed USD. Recovery creates a *copy* session that could take money outside our attempt. |
| D8 | **A webhook is a wake-up signal, never financial authority.** Every financial transition comes from an authenticated retrieve followed by a PG apply. Raw webhook bodies are **not retained**. | Follows Stripe's fulfillment guidance to re-retrieve. Also avoids storing `customer_details` PII (§16.1: short retention is replaced by no retention). |
| D9 | Mandatory ownership, non-goal and safety comments (§15). | Reviewability. |
| D10 | Go standard library only. No `stripe-go` or any new dependency. The API version is pinned by constant. | Integrator-only `go.mod`. Gives deterministic form bytes for idempotent replays and no hidden SDK retries or key regeneration. |
| D11 | **One create Idempotency-Key per attempt, forever (I06).** Each expire call uses its own key, scoped to the claim generation (§11). | A different create key could create a second payable session. Expire cannot duplicate a money movement, and every expire is followed by a retrieve. |
| D12 | One attempt per order, keeping the existing `UNIQUE(tenant,store,order_id)`. A retry after closure means a new checkout and a new hold. | Keeps the existing identity model. §12.1 multi-attempt support is deferred. |
| D13 | Payment after closure produces `CAPTURED` + `fulfillment_state=PAID_ALLOCATION_FAILED` + a `REVIEW_REQUIRED` work item + review `CLOSURE_CONTRADICTED`. No automatic reallocation in v1 (Q5). | Keeps capture-v1's rule "never silently reopen a cancelled order" and its reallocation deferral. Under D5 this path is reachable only through a provider anomaly. |
| D14 | **LIVE is refused in three layers.** (1) SQL `CHECK` makes a `REAL_LIVE` qualification for `stripe_checkout` impossible. (2) Worker config needs a live key plus the flag and an approval reference. (3) API config needs the same. | I16/I17. LIVE needs a new migration, an amendment and owner approval. |
| D15 | **Closed currency allowlist: HKD, USD, TWD (whole dollars only), JPY.** Everything else is rejected (§4). | Stripe's special cases (ISK/UGX ×100, HUF/TWD payouts, 3-decimal currencies) were not fully verified. |
| D16 | **The Stripe handoff can be taken again** while the session is open and before the cutoff. PAYUNi keeps its one-shot form. | The Checkout Session is one payable identity. Showing its URL again to the same authenticated owner adds no capability. |

Rejected alternatives:
- **A Payment Link per SKU.** The link loses quantity, freight and current price, and it
  duplicates truth.
- **Synchronous create in the API.** The API would need the secret, and create would have two code
  paths.
- **The webhook payload as a fact.** The payload is a snapshot that arrives out of order and
  carries PII.
- **Parallel `stripe_*` attempt/fact tables.** A second engine.
- **Per-merchant secret keys stored in the keyring.** Stripe's multi-party path is Connect. Merchant
  secret-key custody is a liability.
- **The 24 h default expiry.** It holds stock far too long.
- **Releasing stock on timeout or 404.** Violates §11.5.

The Stripe MCP connection is operator tooling for docs lookup and read-only sandbox inspection. It
is not an application credential and not a gate executor. It MUST NOT create payments, refunds or
gate evidence. Only documentation search was used for this file.

### 0.1 Owner answers and integrator rulings (2026-09-28, binding)

Owner answers (AskUserQuestion, 2026-09-28):
- Q2: fixed 40-minute session and stock hold after payment start.
- Q3: card only at launch.
- Q5: payment after closure goes to manual refund work only (D13); never reopen the order.
- Q1/Q4: "merchants may choose platform collection or their own account; markets are mainly Taiwan
  and Singapore; TWD, HKD, SGD, MYR and USD are all needed; the integrator decides".

Integrator rulings:
- **Account model v1 = one Stripe account per store, direct charges, no Connect.** The store's
  account is registered by the operator registrar (§13). It may be the platform's own account (when
  the platform is the merchant of record for that store) or the merchant's own Stripe account (the
  merchant supplies a restricted key; stored encrypted like other merchant PSP credentials).
  "Platform collects on behalf of many merchants and pays them out" requires Stripe Connect,
  a money-flow and licensing review, and an ADR. It is **out of v1** (listed in §16).
- **Currency allowlist v1: TWD, HKD, SGD, MYR, USD.** Store currency equals Checkout currency.
  Settlement conversion to the account's default currency (HKD for the sandbox) is Stripe's.
  Reconciliation compares `amount_total`/`currency` in the store currency only. §4 minimums and
  steps apply per currency (TWD whole-dollar rule).
- The Q1 "exactly one store" blocker is lifted by the per-store account rule. SANDBOX tests bind
  `acct_1UJDb0RusP6Wwj7e` to one fixture store only.
- **Stage-A finding (SANDBOX, 2026-09-28):** Stripe accepted a 29-minute `expires_at`, so the
  "Stripe rejects < 30 min" fact in §1/§14 is not relied on. The 30-minute floor is **our local rule**
  (the adapter refuses < 30 min before any request); v1 always uses 40 minutes (Q2). The 29-minute
  probe runs only with `STRIPE_SANDBOX_EXPIRY_PROBE=1`.
- **Staging:** stage A = §5 wire adapter `internal/integrations/psp/stripe` (SP01–SP05 UNIT, SP16
  SANDBOX read/create/expire against the real sandbox). Stage B = §6–§13 persistence, workers,
  HTTP, registrar and SP06–SP15/SP17–SP21. Stage B starts only after stage A is accepted.
- Q6: Adaptive Pricing and Managed Payments are disabled per session (as drafted). Q7: the Stripe
  CLI is not admitted in v1, so SP17 stays NOT_RUN. Q8/Q9 are deploy-time owner inputs (listed in
  §16).

### 0.2 Stage-B consistency amendment (2026-09-29; frozen interfaces)

This section implements §0.1, rather than choosing a different account model.
It replaces the conflicting D1/D15, custody/registrar/worker/webhook clauses and test expectations
listed below. The historical ENV-only descriptions are **not a second supported runtime mode**.
In particular, the old §6.4 platform registrar names, §8 preconstructed-client constructor,
§9.1 global webhook route and §13 fingerprint-only storage are superseded, not worker APIs.
The per-unit delivery brief must freeze any Go constructor signatures before implementation.
PAYUNi behavior and already-applied migrations remain unchanged. No Connect, pooled funds,
new payment engine, new queue system or production permission is introduced.

#### Account identity and API-key custody

- Replace the environment-global Stripe account index with partial unique indexes on
  `(tenant_id,store_id,environment)` and `(environment,account_id)`, both WHERE provider='stripe'.
  Different stores may register different accounts in the same environment. One provider account
  cannot silently become the common collection account of unrelated stores.
- Retain `integration.account_credentials`' existing non-null `key_id`, `nonce`, `ciphertext`,
  immutable version rows and deferred current-version FK. **Do not add `ENV_PLATFORM`, nullable
  ciphertext or an env-key fingerprint column.** Ordinary merchant runtime INSERT into a Stripe
  account/credential remains forbidden; only the operator registrar may provision it in B1.
- Stripe API credential plaintext is a separately validated `stripe-api-v1` payload containing
  only `secret_key`. Reuse AES-256-GCM and the keyring, with a distinct payload/AAD format binding
  tenant, store, connection, provider, environment, account and credential version. Never shoehorn
  a Stripe key into PAYUNi `HashKey`/`HashIV`, or loosen PAYUNi's existing validator.
- `STRIPE_SECRET_KEY` and `STRIPE_ACCOUNT_ID` are registrar/probe inputs only. The registrar
  verifies the account using Stage A, encrypts before SQL, and records the exact frozen version.
  API and payment-worker do not read those global variables. No plaintext secret enters PG.
- Replace `stripe_worker_ready(environment,account,fingerprint)` with capability/role/queue
  validation at startup and this operation-scoped loader (0061, integration_writer, EXECUTE worker):

  `integration.load_stripe_credential(uuid,bigint,bytea,text) RETURNS TABLE
  (tenant_id uuid,store_id uuid,connection_id uuid,credential_version bigint,environment text,
  account_id text,key_id text,nonce bytea,ciphertext bytea)`.

  Arguments are attempt, lease generation, lease token, profile. It joins the account and exact
  `attempt.credential_version`, checks provider/binding/environment, and repeats the DB-clock
  lease fence after waits, as `load_payment_query` does. No current-head or process-env fallback.
  Only the worker may decrypt API material. Historical attempts keep their frozen credentials
  after rotation; if those credentials cease to work, retain UNKNOWN/stock, never substitute
  another merchant's key or a new create identity.
- Signal processing claims the operation **before** reading signal metadata. The post-River,
  integration_writer-owned, worker-EXECUTE-only loader is
  `integration.load_stripe_signal(operation uuid,generation bigint,token bytea,profile text,
  job_id bigint,signal_id uuid) RETURNS TABLE(source text,session_id text,
  created_at timestamptz,consumed_at timestamptz,db_now timestamptz)`.
  It reuses `require_stripe_query`, matches exact signal/job/attempt/tenant/store, River kind,
  immutable args and profile queue, then rechecks the lease against the DB clock. No direct
  worker table grant or unleased signal lookup is added. `signal_candidate` is a poller hint,
  never the authority for a signal job. A consumed row is returned for idempotent job completion.
  Stale/no-op/consumed jobs release their claim in the same transaction as any consume marker.
  After provider I/O, one transaction reloads the exact signal, inserts the reconcile job,
  consumes the signal, then calls `record_stripe_observation` **last** because that function
  completes the operation and clears its lease. Failure rolls all these changes back. This
  ordering supersedes the original pre-claim-read/post-record-consume wording in §8.
- `StripeRuntime` owns the scoped loader and shared keyring, not one process-global client/account.
  Build the Stage-A client from the leased material and verify `GET /v1/account` against that
  account before any checkout call. Verify plus checkout I/O share the existing claim deadline.
  Initial implementation need not cache verification; any later cache must be bounded and keyed
  by the entire scope/credential version. Observations record the credential version actually used.
- Registrar SQL names are `integration.register_stripe_account` and
  `integration.rotate_stripe_key` (not the obsolete `*_platform_*` names). Registration input:
  `(tenant uuid,store uuid,principal uuid,connection uuid,binding uuid,environment text,
  account text,key_id text,nonce bytea,ciphertext bytea) RETURNS uuid`.
  Rotation input: `(tenant uuid,store uuid,principal uuid,connection uuid,expected_version bigint,
  key_id text,nonce bytea,ciphertext bytea) RETURNS bigint`.
  Both validate owner membership/store, set scoped GUCs, and audit; registration atomically inserts
  binding/account/version 1, rotation locks the account and appends exactly expected_version+1.
  Rotation invalidates eligibility for new starts until requalification; it never rewrites attempts.
- The existing §13 qualification/method registrar operations use these typed SQL interfaces:
  `payments.qualify_stripe_method(tenant uuid,store uuid,principal uuid,qualification uuid,
  connection uuid,expected_credential_version bigint,profile text,evidence_ref text,
  observed_at timestamptz,expires_at timestamptz) RETURNS uuid` and
  `payments.set_stripe_method(tenant uuid,store uuid,principal uuid,market uuid,country text,
  connection uuid,qualification uuid,expected_version bigint,enabled boolean,visible boolean,
  sort integer,min bigint,max bigint,name_hans text,name_hant text,name_en text) RETURNS bigint`.
  Both are registry_writer-owned and EXECUTE registrar only, with owner/scope validation.
  Qualify captures the expected API credential version **before** the network probe and rejects
  a changed head after locking account/binding; an old-key probe cannot qualify a new key.
  **Amended 2026-09-29 (r1-final-rulings S4/S5):** the SANDBOX probe runs with the credential *stored*
  at `expected_version` (opened with the registrar's API keyring) against the connection's *registered*
  account, read through the registrar-only `payments.stripe_registrar_credential(tenant, store,
  principal, connection, expected_version)` (head version only; ciphertext envelope, no writes).
  `STRIPE_SECRET_KEY` / `STRIPE_ACCOUNT_ID` are optional operator assertions that must equal the
  stored key and registered account, else `rejected`. `rotate` binds the new envelope's AAD to the
  registered account from `payments.stripe_endpoint_account` and refuses an operator account or key
  that differs from it.
  It derives proof_class from PROVIDER_MOCK/SANDBOX (never LIVE), validates finite observation
  times not in the future and expiry in `(now, observed_at + 30 days]`, and records bounded
  evidence. It never silently substitutes the current version. Method updates use the existing
  method-head CAS/lock order, derive currency from the locked market and recheck the exact
  qualification/account/current credential/profile before enabling. No caller-supplied currency
  or separate method registry is introduced. Probe-vs-rotation rejection is part of SP21.

#### Per-account webhook routing and separate signing custody

- Public route is **`POST /v1/stripe/webhook/{endpoint_id}`**, where `endpoint_id` is a canonical
  UUID created by the registrar, not a secret or tenant authority. There is no global fallback route.
  Deploy routing must expose this exact route family. Existing §9.1 method/body/timeout/commit rules
  still apply. Never select a key using unsigned event fields or a supplied account/tenant header.
- Add `payments.stripe_webhook_endpoints` in 0061: `endpoint_id uuid PRIMARY KEY`, tenant/store/
  connection, environment/account, execution_profile, `enabled boolean`, `key_version bigint>0`,
  `key_id`, 12-byte nonce, bounded ciphertext (17..8192 bytes), and created/updated timestamps.
  Add a generated constant `provider='stripe'` and a composite FK to the exact account's
  tenant/store/id/provider/environment/account tuple (add that referenced UNIQUE key on
  `integration.merchant_accounts`), and UNIQUE
  `(tenant_id,store_id,connection_id,execution_profile)`. Profile compatibility is MOCK→SANDBOX
  or SANDBOX→SANDBOX; LIVE is rejected. No endpoint may be rebound to another account or scope.
- Signing ciphertext uses a separate `stripe-webhook-v1` payload/AAD binding endpoint, scope,
  account/profile and key_version. Payload contains `current_secret` and optional distinct
  `next_secret`; Stage-A webhook validation admits at most two. It never contains an API key.
  Updates increment key_version exactly once and are audited. This reuses AEAD machinery without
  letting the ingress role decrypt payment API credentials or the worker read signing credentials.
- `payments.stripe_webhook_material(uuid) RETURNS TABLE(tenant_id uuid,store_id uuid,
  connection_id uuid,environment text,account_id text,execution_profile text,key_version bigint,
  key_id text,nonce bytea,ciphertext bytea)` is integration_writer-owned, EXECUTE ingress only.
  It returns only an enabled endpoint with matching immutable account/binding identity. It must
  not depend on current `binding.enabled` or the current API credential head: historical payment
  reconciliation survives a merchant disabling new sales or rotating credentials. Missing/disabled
  endpoints return no row and a fixed HTTP 404. Decrypt/verifier failure has a fixed code and logs
  no key, signature or body. API has a separately configured signing-material keyring; its process
  need not load the payment API-key decryption keyring. Explicitly disabling the webhook endpoint
  fails closed; historical recovery then remains a poller/manual responsibility, never unpaid proof.
- `payments.stripe_webhook_prepare` arguments become `(endpoint uuid,key_version bigint,
  event_id text,event_type text,event_created bigint,api_version text,object_type text,
  session_id text,client_reference text,metadata_attempt text,account_present boolean,
  livemode boolean,probe boolean,malformed boolean,body_sha256 bytea,signed_at bigint)`.
  It resolves scope/account/profile from the locked endpoint and rejects a stale key_version
  (fixed retryable HTTP 503; no receipt/ACK). It must explicitly compare the verified `livemode`.
  Mapping and dedupe are scoped to that endpoint's account; unsigned fields cannot override it.
- Receipts add immutable `endpoint_id uuid NOT NULL REFERENCES payments.stripe_webhook_endpoints`.
  Replace the old account-wide event uniqueness with `(endpoint_id,event_id)` and the malformed
  partial unique index with `(endpoint_id,body_sha256) WHERE event_id IS NULL`. Advisory dedupe
  locks use the same endpoint/event or endpoint/body identity. A wrong-profile endpoint's ignored
  receipt must not consume delivery to the correct endpoint. Account/scope/profile still derive
  from the immutable endpoint; no alternate endpoint can create a cross-profile signal.
- Registrar endpoint function (registry_writer, EXECUTE registrar only):
  `payments.set_stripe_webhook_endpoint(tenant uuid,store uuid,principal uuid,connection uuid,
  endpoint uuid,profile text,expected_version bigint,enabled boolean,key_id text,nonce bytea,
  ciphertext bytea) RETURNS bigint`. expected_version=0 creates version 1; updates require the
  exact previous version. Disabled endpoints retain identity/audit and do not accept new ingress.

#### Receipt atomicity, effective SQL authority and late money

- `ACCEPT_PENDING` exists **only as a prepare return value**, never a stored disposition.
  Prepare preallocates `signal_id`, inserts an ACCEPTED receipt with scope/attempt/signal identity,
  and returns `(disposition,receipt_id,attempt_id,session_id,signal_id)`. All work is in one tx.
  The caller uses that signal_id for River InsertTx; `stripe_webhook_commit` inserts the reciprocal
  signal and validates job kind/args/queue/scope. It does not mutate receipt identity/disposition.
- A deferred receipt INSERT constraint trigger (post-River phase) re-reads the final row and
  rejects COMMIT of ACCEPTED without its exact reciprocal signal and linked River job. Follow
  `meta_event_commit`'s pattern. This closes prepare-without-commit, wrong signal/scope/job and
  independent-transaction holes. There is no ACK until the whole transaction commits.
  Malformed dedupe locks `(endpoint_id,body_sha256)`, not a NULL event-id lock. These endpoint-based
  keys also replace the stale §11 webhook key, without changing attempt/fact/stock idempotency.
- Receipt append-only and narrow redelivery UPDATE grants remain unchanged. There is no new
  `UPDATE(disposition,signal_id)` permission. Signal/receipt scope is reciprocal, not merely a
  UUID FK. Permanent dedupe and max signal count still apply under concurrent transactions.
- All new tables, including webhook endpoints, have FORCE RLS and no direct runtime login grants.
  Registry writer receives only the SELECT/INSERT and narrow UPDATEs needed for account, credential,
  endpoint, qualification/method, binding, owner-membership/store checks and audit. Add explicit
  policies for that definer role on each FORCE-RLS table; schema privileges alone are insufficient.
  Validation SELECT grants do not imply mutating membership/store permissions. Policies use the
  scope set by the validated definer; no new BYPASSRLS or schema-owner function is permitted.
  Registrar receives schema USAGE and EXECUTE on registrar functions only. Ingress gets only
  material/prepare/commit execution; worker gets only lease-fenced API-material execution.
  `commerce_integration_writer` receives SELECT with an explicit RLS SELECT policy and only
  `UPDATE(endpoint_id)` with an explicit FOR UPDATE policy `USING (true) WITH CHECK (false)`.
  This follows `0008_external_operations.sql`: PostgreSQL needs the column privilege for
  `SELECT ... FOR SHARE`, while the RLS check forbids even a no-op UPDATE. Prepare holds that
  row lock through receipt/signal/job commit; registrar rotation/disable takes the conflicting
  row lock before checking/incrementing key_version. No separate advisory endpoint-lock protocol
  is introduced. The registry writer alone may actually mutate endpoints. Existing immutable
  identity guards remain mandatory even for its narrow grants.
- `CLOSURE_CONTRADICTED` + `REVIEW_REQUIRED` is the B1 manual-refund obligation identity.
  No READY fulfillment consumer may select it. Late capture sets PAID_ALLOCATION_FAILED, records
  the obligation once, does not reopen an order or move stock, and does not call any refund API.
  The later refund contract consumes this explicit obligation; B1 does not claim refund execution.

#### Corrected amount table and acceptance deltas

SQL copies the accepted Stage-A `amount.go` table exactly: HKD 400..99999999 step1; USD and SGD
50..99999999 step1; MYR 200..99999999 step1; TWD 2500..99999900 step100 (TWD min 2500: Stripe SANDBOX rejected 100/1200, accepted 2500 (2026-09-29)). JPY and all other currencies
are rejected. These minimums are local prefilters, not guarantees for settlement conversion.
This replaces the old JPY row, D15 and every stale currency list/test vector below.
For rejected or NULL input, `stripe_amount_ok` returns false and `stripe_unit_amount` returns
SQL NULL (not an exception); accepted input returns the unchanged minor-unit integer.

- SP02: five-currency boundaries and SQL/Go parity; JPY is a negative, not a formatting gate.
- SP06: two stores/different accounts in one environment succeed; second account per store or
  same account across stores fails; unchanged PAYUNi crypto/CHECKs; DB_AEAD version immutability;
  effective registrar ACL/RLS, endpoint binding and deferred receipt/job reciprocity negatives.
- SP08: at least two Stripe stores/accounts plus PAYUNi share the profile queue without key/scope
  leakage; forged cross-account sessions, signals and receipts fail.
- SP12: late paid replay creates exactly one explicit manual-refund obligation, zero allocation.
- SP13: endpoint/signature mismatch, key-version rotation during admission, missing linkage and
  commit failure cannot ACK; no plaintext/body/signature in logs or persisted rows.
  Test effective-role FOR SHARE success and direct no-op/identity UPDATE rejection. With two
  real PG transactions, rotation/disable must wait for an admitted prepare transaction, and a
  prepare after a committed rotation must reject its old key_version without a receipt or ACK.
  Binding disable/API-key rotation do not suppress valid historical endpoint admission; endpoint
  disable does. A PAYUNi account cannot satisfy the endpoint's Stripe-specific composite FK.
  Wrong-profile delivery followed by correct-profile delivery admits exactly one correct signal;
  redelivery to the same endpoint is deduplicated even when its body metadata changes.
- SP15: API/worker ignore global Stripe credentials; worker scoped key admission and account
  mismatch fail closed before checkout I/O; exact-version rotation behavior; per-role secret
  separation; disabled startup reads no unrelated Stripe secrets. Same existing timeout budgets.
- SP21: operator provisions two distinct accounts/stores, rejects cross-store rebinding and
  secret-purpose/AAD swaps, rotates/requalifies without rewriting historical attempt credentials.
  ENV-only fingerprint/startup and global-account rejection assertions are superseded.
- SP16's 29-minute remote probe is observational only (§0.1); local adapter rejects it. CLI/SP17
  remain NOT_RUN unless explicitly admitted later. No SANDBOX gate becomes LIVE authorization.

Upgrade signals: add Connect only if platform collection for unrelated merchants is approved;
add a bounded credential-client cache only after measured verification overhead. Merchant
self-service Stripe UI, refund execution and live activation remain separate gated units.

## 1. Stripe facts relied on (retrieved 2026-09-28 via Stripe docs MCP + WebFetch of docs.stripe.com)

| # | Fact | Source | Status |
| --- | --- | --- | --- |
| F1 | `expires_at` "can be anywhere from 30 minutes to 24 hours after Checkout Session creation", default 24 h. `client_reference_id` ≤200 chars. `ui_mode` defaults to `hosted_page`. `locale` values include `zh`, `zh-TW`, `en`. Objects `adaptive_pricing`, `managed_payments` and `after_expiration` exist. | [create](https://docs.stripe.com/api/checkout/sessions/create) | VERIFIED. The child names `adaptive_pricing[enabled]` and `managed_payments[enabled]` are SOURCE_PARTIAL: the fetched reference collapsed them. SP16 must prove Stripe accepts them. |
| F2 | Expire is allowed only when the session is `open`. After it, the customer cannot complete. Stripe "returns an error if the Checkout Session has already expired or isn't in an expireable state". | [expire](https://docs.stripe.com/api/checkout/sessions/expire) | VERIFIED |
| F3 | `status`: `open` = processing not started; `complete` = processing may still be in progress; `expired` = no further processing. `payment_status`: `paid` = funds available; `unpaid`; `no_payment_required`. `url` is present only while the session is active. A Checkout PaymentIntent cannot be cancelled; expire the session instead. | [object](https://docs.stripe.com/api/checkout/sessions/object) | VERIFIED |
| F4 | `Idempotency-Key` is ≤255 chars. The result, including a 500, is saved once execution begins. Parameters are compared, and a mismatch errors. Validation failures and concurrent conflicts are not saved. Keys may be pruned after ≥24 h. 429 and most 400/401 responses come from layers that run before idempotency. A 500 is indeterminate and you should not switch keys. `Idempotent-Replayed: true` marks a replay. `Stripe-Should-Retry` is a hint. | [idempotent_requests](https://docs.stripe.com/api/idempotent_requests), [error-low-level](https://docs.stripe.com/error-low-level) | VERIFIED |
| F5 | `Stripe-Signature` carries `t=` plus one or more `v1=` values; a test `v0` is ignored, as is any non-`v1` scheme. Signature = HMAC-SHA256(secret, `t` + `.` + raw body), compared in constant time. The libraries' default tolerance is 5 min; use NTP. Each retried delivery gets a new timestamp and signature. During a secret roll (≤24 h) several `v1` values are sent. Duplicate deliveries happen: dedupe by event id. Return 2xx quickly. For `checkout.session.completed`, Checkout waits up to 10 s for the endpoint before redirecting. | [webhooks](https://docs.stripe.com/webhooks), [signature](https://docs.stripe.com/webhooks/signature), [fulfillment](https://docs.stripe.com/checkout/fulfillment.md?payment-ui=stripe-hosted) | VERIFIED |
| F6 | Fulfill once per session; fulfillment may be called concurrently. Retrieve the session and check `payment_status`. The landing page alone is insufficient. Delayed methods emit `checkout.session.async_payment_succeeded`/`_failed`. | fulfillment (as F5) | VERIFIED |
| F7 | Amounts are in the currency's minor unit; for a zero-decimal currency the amount is the charge (500 JPY → 500). ISK/UGX use a two-decimal representation ending in `00`. HUF and TWD accept two-decimal charges but zero-decimal payouts divisible by 100. Minimums depend on the settlement currency (HKD 4.00, USD 0.50, JPY 50; TWD not listed). Non-card payments allow 8 digits; cards allow up to 12 (Amex 9). | [currencies](https://docs.stripe.com/currencies) | VERIFIED. The full zero-decimal and 3-decimal lists were not rendered: SOURCE_PARTIAL, which is why D15 is a closed list. |
| F8 | Since `2025-03-31.basil`, a session's `currency`/`amount_total` stay in the integration currency, and the customer's choice goes to `presentment_details`. `currency_conversion` is legacy. Adaptive Pricing can be enabled or disabled per session. | [changelog basil](https://docs.stripe.com/changelog/basil/2025-03-31/add_presentment_details), [acacia param](https://docs.stripe.com/changelog/acacia/2024-11-20/adaptive-pricing-param) | VERIFIED |
| F9 | The current API version is `2026-08-26.dahlia`. `Stripe-Version` is sent per request. A webhook endpoint may pin its own version. IDs can be ≤255 chars, and prefixes may change. | [api-versions](https://docs.stripe.com/api-versions) | VERIFIED |
| F10 | List sessions supports a `created` interval, `limit` 1..100 and `starting_after`. Top-level v1 lists are immediately consistent. | [list](https://docs.stripe.com/api/checkout/sessions/list), [v2 overview](https://docs.stripe.com/api-v2-overview) | VERIFIED |
| F11 | Restricted keys use prefixes `rk_test_`/`rk_live_` and per-resource Read/Write. They are recommended over `sk_`. | [restricted keys](https://docs.stripe.com/keys/restricted-api-keys) | VERIFIED |
| F12 | `after_expiration.recovery` creates a **new session copy** of an expired one. | [abandoned carts](https://docs.stripe.com/payments/checkout/abandoned-carts) | VERIFIED, so it is forbidden |
| F13 | On this sandbox account, Managed Payments is on by default (it conflicted with `custom_text`), and Adaptive Pricing offered USD. | [2026-09-25 link verification](../docs/implementation/2026-09-25-stripe-sandbox-link-verification.md) | LOCAL EVIDENCE (a Payment Link, not a Session) |

Not verified; the design treats each as possible:
- Whether an expire that races an in-flight card confirmation errors or waits.
- Whether time-dependent validation of `expires_at` runs before the idempotency replay lookup (§10).
- The exact test-mode webhook retry schedule.
- The exact restricted-key permission names. SP16 records the actual least set.

## 2. Business boundary

**In scope:**
- One Stripe account per environment, bound to one store.
- Buyer prepare → worker create → re-takeable redirect handoff.
- Webhook intake as a signal.
- Poller retrieve.
- Expire at the deadline or on buyer cancel.
- Capture (allocate stock, confirm the order) or closure (release stock, cancel the order) through
  the existing single stock writer.
- Review cases for anomalies.
- An operator registrar with a SANDBOX probe.
- MOCK, SANDBOX and BROWSER test tiers.

**Out of scope (each is a separate contract):**
- Stripe Connect / multi-merchant.
- Refunds, disputes and chargebacks. No real refund may be issued, and no refund API is called.
- Balance-transaction and payout reconciliation (§12.4).
- Async payment methods and wallets beyond card.
- Automatic late reallocation.
- A merchant Stripe settings UI.
- Tax.
- LIVE admission.
- The PAYUNi expiry protocol. The generic closure fact added here can be reused for it later.

## 3. End-to-end flow

```
Buyer tab ─POST prepare{stripe_checkout}─► API(hosted pool) ─one tx, no I/O─► PG:
   lock order→reservation→market→head→account→binding→qualification;
   attempt + op(UNKNOWN, stripe.checkout_session) + stripe_sessions(frozen params, deadlines)
   + receipt + event + River payment_query_v1 (ScheduledAt=now); HELD→PAYMENT_PENDING
Worker payment_query_v1 ─claim─► mark_create_sent(commit) ─► POST /v1/checkout/sessions
   (Idempotency-Key lc:stripe:cs-create:v1:<attempt>) ─► record(obs via=create, pin id+url)
   + payment_reconcile_v1 ─► snooze to min(60 s, deadline)
Buyer tab ─POST handoff─► {REDIRECT, redirect_url} (repeatable until cutoff) ─child window─►
   checkout.stripe.com ─success/cancel─► neutral /payment/return (writes nothing)
Stripe ─POST /v1/stripe/webhook─► API(ingress pool): verify raw-body signature → one tx:
   receipt + stripe_signals + River payment_signal_v1 → commit → 200
Worker payment_signal_v1 | poller ─claim─► GET session?expand[]=payment_intent
   [deadline reached or buyer cancel, and open: POST .../expire → GET] ─► record obs + reconcile
CaptureWorker payment_reconcile_v1 ─► payments.apply_capture(dispatch) → apply_stripe_observation:
   CAPTURED (ALLOCATE, COMMITTED, CONFIRMED, work item) | CLOSED_UNPAID (RELEASE, RELEASED,
   CANCELLED) | review case | no-op
```

## 4. Currency, minor units and amount rules

The repository's `amount_minor` is in ISO 4217 minor units. Stripe's amount is in the currency's
minor unit with the special cases in F7. v1 admits only currencies where the two units are equal
and verified:

| Currency | ISO exponent | Stripe unit = repo minor | Local step | Local min | Local max | v1 |
| --- | --- | --- | --- | --- | --- | --- |
| HKD | 2 | yes | 1 | 400 (HK$4.00, F7) | 99,999,999 | admitted |
| USD | 2 | yes | 1 | 50 | 99,999,999 | admitted |
| TWD | 2 | yes (two-decimal charges) | **100** (whole NT$; payouts are zero-decimal, F7) | **2500** (NT$25; TWD min 2500: Stripe SANDBOX rejected 100/1200, accepted 2500 (2026-09-29)) | 99,999,900 | admitted |
| JPY | 0 | yes | 1 | 50 | 99,999,999 | admitted. SP02 must also pass the JPY storefront-formatting vector. |
| ISK, UGX | 0 | no (×100) | — | — | — | rejected |
| HUF | 2 | yes, but payouts differ | — | — | — | rejected |
| BHD/JOD/KWD/OMR/TND and all others | — | unverified | — | — | — | rejected |

- The same table exists as a Go table (`stripe.UnitAmount(currency, amountMinor) (int64, error)`)
  and as two IMMUTABLE SQL functions: `payments.stripe_amount_ok(text,bigint)` and
  `payments.stripe_unit_amount(text,bigint)`. SP02 asserts that Go and SQL agree on every vector.
- Stripe expects lowercase currency codes on the wire. PG stores uppercase.
- The local minimums are a prefilter, not Stripe's guarantee. The real minimum depends on the
  account's settlement currency after conversion. A Stripe 400 on the first send is a definitive
  rejection (§10).
- The 8-digit maximum is conservative; cards would allow more.
- The method revision `min/max_amount_minor` set by the registrar must lie inside these bounds.
- There is no rounding, truncation or re-pricing anywhere. A mismatch rejects.

## 5. Wire adapter `internal/integrations/psp/stripe`

### 5.1 Package rules

- Uses the standard library only. No SQL, River or `accounts` import.
- The base URL is fixed at `https://api.stripe.com`. No caller-supplied base URL, host, redirect or
  HTTP client.
- `NewWithMockTransport` accepts a `RoundTripper` and admits only `sk_test_`/`rk_test_` keys. It is
  referenced only from `_test.go` files and the PROVIDER_MOCK assembly. The deployable CLI cannot
  reach it (source guard SP20).
- HTTP client:
  - TLS ≥1.2 with certificate verification.
  - No cookies and no redirects: a 3xx response is `ErrUncertain`.
  - Each call is bounded by its context. All calls within one claim share `CallTimeout` (10 s).
  - Response body bound: 256 KiB.
- Headers on every request:
  - `Authorization: Bearer <key>`
  - `Stripe-Version: 2026-08-26.dahlia` (the `APIVersion` constant; changing it requires a contract
    amendment and a re-run of SP16/SP18)
  - `User-Agent: livecommerce-stripe/1`
  - on POST: `Content-Type: application/x-www-form-urlencoded`
- The package makes **no automatic retries**. Retry is a durable-orchestration decision (§10).
- `Config`, `Client`, `Session`, `Event` and `CallMeta` are redacted in `String`, `GoString`,
  `%+v` and `MarshalJSON`. They never expose the key, secret, session URL or error message.

### 5.2 Key admission and LIVE refusal (the worker, API and registrar share one parser)

| Key prefix | PROVIDER_MOCK | SANDBOX | LIVE |
| --- | --- | --- | --- |
| `sk_test_`, `rk_test_` | allowed (mock transport only) | allowed | **refused** `stripe_key_mode_mismatch` |
| `sk_live_`, `rk_live_` | **refused** | **refused** | allowed only if `COMMERCE_STRIPE_LIVE_ENABLED=1` **and** `COMMERCE_STRIPE_LIVE_APPROVAL_REF` matches `^[A-Za-z0-9._:-]{8,128}$`; else `stripe_live_refused` |

- Key syntax: `^(sk|rk)_(test|live)_[A-Za-z0-9]{16,240}$`, no surrounding whitespace.
- A live flag or approval reference present with a test key also fails. The two must match
  exactly.
- `rk_` is recommended. The least permission set is recorded by SP16; this file does not assert it.
- At startup, `VerifyAccount` calls `GET /v1/account`. The returned `id` must equal
  `STRIPE_ACCOUNT_ID`. Every session response must have `livemode == (environment=="LIVE")`.

### 5.3 Frozen Go API (to be frozen on acceptance)

```go
package stripe // internal/integrations/psp/stripe

const APIVersion = "2026-08-26.dahlia"

type Config struct {
	SecretKey   string       // sk_/rk_ test or live key; never logged or serialized
	AccountID   string       // acct_…; must equal GET /v1/account id
	Environment string       // "SANDBOX" | "LIVE" (PROVIDER_MOCK uses SANDBOX)
	Live        LiveApproval // zero value unless Environment=="LIVE"
}
type LiveApproval struct{ Enabled bool; Reference string }

func New(cfg Config) (*Client, error)
func NewWithMockTransport(cfg Config, rt http.RoundTripper) (*Client, error) // test keys only
func (c *Client) VerifyAccount(ctx context.Context) (CallMeta, error)
func (c *Client) CreateCheckoutSession(ctx context.Context, p CreateParams) (Session, CallMeta, error)
func (c *Client) RetrieveCheckoutSession(ctx context.Context, sessionID string) (Session, CallMeta, error)
func (c *Client) ExpireCheckoutSession(ctx context.Context, sessionID, idempotencyKey string) (Session, CallMeta, error)
func (c *Client) FindCheckoutSessions(ctx context.Context, attemptID string, createdFrom, createdTo int64) ([]Session, CallMeta, error)

type CreateParams struct{ Fields map[string]string } // exactly the §5.4 key set, loaded from PG
func EncodeCreateBody(p CreateParams) ([]byte, error)  // sorted keys, url.QueryEscape; deterministic
func CreateIdempotencyKey(attemptID string) string             // "lc:stripe:cs-create:v1:<attempt>"
func ExpireIdempotencyKey(attemptID string, generation int64) string // "lc:stripe:cs-expire:v1:<attempt>:<gen>"
func UnitAmount(currency string, amountMinor int64) (int64, error)  // §4 table

type CallMeta struct {
	HTTPStatus int; RequestID string; IdempotentReplayed bool; ShouldRetry *bool
	ErrorType, ErrorCode string // bounded ^[a-z_]{0,64}$; never the message
}
type Session struct { /* §5.7 projection fields + private url; redacted formatting */ }
func (s Session) URL() string // only for the pin write; never logged
func (s Session) Observation(via string, meta CallMeta, account string, keyVersion int64, sendCount int) Observation

type WebhookConfig struct {
	Secrets     []string // 1..2 distinct: STRIPE_WEBHOOK_SECRET [, STRIPE_WEBHOOK_SECRET_NEXT]
	AccountID   string
	Environment string   // SANDBOX ⇒ livemode=false; LIVE ⇒ true
}
type Event struct {
	ID, Type, APIVersion, ObjectType, SessionID, ClientReferenceID, MetadataAttempt string
	Livemode, AccountPresent, ProbeSession, Malformed bool
	Created, SignedAt int64
	BodySHA256 [32]byte
}
func NewWebhookVerifier(WebhookConfig) (*WebhookVerifier, error)
func (v *WebhookVerifier) Verify(raw []byte, signatureHeader string, now time.Time) (Event, error)

var (ErrInvalid, ErrLiveRefused, ErrSignature, ErrRejected, ErrAuthentication, ErrIdempotency,
	ErrConflict, ErrRateLimited, ErrNotOpen, ErrUncertain error)
```

Fingerprints for credential-version recording (§6.1) come from
`(*accounts.Keyring).StripeKeyFingerprint(environment, accountID, secretKey string) ([32]byte, error)`.
The value is HMAC-SHA256 under the existing 32-byte replay key over the compact JSON array
`["stripe-key-fingerprint-v1",environment,accountID,secretKey]`. It is not reversible without the
server key. `validAAD`/`Credentials` stay PAYUNi-only and are not modified.

### 5.4 Canonical create parameters (the complete key set; anything else is `ErrInvalid`)

The key set is built once, in SQL, by `checkout.start_stripe_payment`. It is stored in
`payments.stripe_sessions.create_params`. Every send posts byte-identical bodies (SP03). The SHA-256
of the first encoded body is pinned in `create_body_sha256`.

```
adaptive_pricing[enabled]=false                       managed_payments[enabled]=false
automatic_tax[enabled]=false                          metadata[lc_attempt]=<attempt uuid>
cancel_url=<COMMERCE_PAYMENT_RETURN_URL>              metadata[lc_order]=<order uuid>
client_reference_id=<attempt uuid>                    metadata[lc_profile]=<PROVIDER_MOCK|SANDBOX|LIVE>
expires_at=<unix seconds, D4>                         metadata[lc_v]=1
line_items[0][price_data][currency]=<lower ISO>       mode=payment
line_items[0][price_data][product_data][name]=Order <first 8 hex of order id, upper>
line_items[0][price_data][unit_amount]=<stripe_unit_amount>
line_items[0][quantity]=1                             payment_intent_data[metadata][lc_attempt]=<attempt>
locale=<zh-TW→zh-TW | zh-CN→zh | en→en>               payment_intent_data[metadata][lc_order]=<order>
payment_method_types[0]=card                          submit_type=pay
success_url=<COMMERCE_PAYMENT_RETURN_URL>             ui_mode=hosted_page
```

The following are never sent:
- `allow_promotion_codes`, `discounts`, `shipping_options`, `shipping_address_collection`
- `optional_items`, `adjustable_quantity`, `after_expiration`
- `customer`, `customer_email` (no buyer PII leaves our system)
- `phone_number_collection`, `custom_fields`, `custom_text`, `consent_collection`
- `invoice_creation`, `saved_payment_method_options`
- `payment_intent_data[capture_method]=manual`
- `{CHECKOUT_SESSION_ID}` in any URL

The product name contains no catalog text or PII.

### 5.5 Other calls

- **Retrieve:** `GET /v1/checkout/sessions/{id}?expand[]=payment_intent`. The PaymentIntent is used
  only for the §5.7 fields. Its `client_secret` and all other fields are discarded while parsing.
- **Expire:** `POST /v1/checkout/sessions/{id}/expire`, empty body, with
  `Idempotency-Key = ExpireIdempotencyKey(attempt, claimGeneration)`. A 400 whose type is
  `invalid_request_error` means the session is not open (`ErrNotOpen`); the caller always follows
  with a retrieve.
- **Find:** `GET /v1/checkout/sessions?created[gte]=A&created[lte]=B&limit=100[&starting_after=…]`.
  - At most 10 pages per call; more is `ErrUncertain`, never "not found".
  - A match requires `client_reference_id == metadata.lc_attempt == attempt`.
  - The window runs from `create_first_sent_at − 120 s` to `create_last_sent_at + 120 s`.

### 5.6 Response classification

| Response | Class | Create meaning | Retrieve / expire meaning |
| --- | --- | --- | --- |
| 200 with a session object that validates | ok | pin | observe |
| 400 `invalid_request_error` / 404 on create | `ErrRejected` | definitive only if it is the **first** send (§10) | retrieve 404: `ErrUncertain`; expire 400: `ErrNotOpen` |
| 400 `idempotency_error` | `ErrIdempotency` | frozen params changed: bug. UNKNOWN + alarm code, never a closure | — |
| 401 / 403 | `ErrAuthentication` | first send: definitive, nothing executed. Later send: uncertain (runs before idempotency, F4) | uncertain |
| 409 | `ErrConflict` | concurrent same key: retry the same key later | retry later |
| 429 | `ErrRateLimited` | retry the same key with backoff | backoff |
| 5xx, including replayed 500 (`Idempotent-Replayed:true`) | `ErrUncertain` | never a new key; resolve via same-key retry, then list (§10) | retry later |
| network error, timeout, cancel, 3xx, bad TLS, malformed/oversized/duplicate-key JSON, `object≠checkout.session`, missing required field | `ErrUncertain` | as above | retry later |

`Stripe-Should-Retry: false` stops **only** repeated sends of the same request. It never implies a
closure.

### 5.7 Observation projection (`payments.provider_observations.report`, provider `stripe`)

The projection is one flat JSON object. It has **exactly** these keys, always present; nullable
where marked `?`. It must be ≤2048 bytes (existing CHECK). The keys are:
- `Provider`="stripe", `Version`=1
- `Via` ∈ {`create`,`retrieve`,`expire`,`list`,`unsent`,`escalate`}
- `AccountID`, `KeyVersion` (int; 0 for LOCAL), `RequestID` (`^[A-Za-z0-9_]{0,64}$`)
- `SendCount` (int; create only, else 0)
- `SessionID`, `Status`, `PaymentStatus`, `Livemode`
- `Currency` (uppercase or "")
- `AmountTotal?`, `AmountSubtotal?`, `AmountDiscount?`, `AmountTax?`, `AmountShipping?`
- `PresentmentCurrency`, `PresentmentAmount?`, `CurrencyConversion` (bool)
- `ClientReferenceID`, `MetadataAttempt`, `MetadataProfile`
- `ExpiresAt?`, `Created?`, `Mode`, `PaymentMethodTypes` (array of strings)
- `PaymentIntentID`, `PaymentIntentStatus`, `PaymentIntentAmountReceived?`, `PaymentIntentCurrency`
- `ErrorClass` ("" | "rejected"), `ErrorCode`, `HTTPStatus`
- `ListMatchCount?`, `LocalReason`

The projection never contains the URL, customer details, email, card or wallet data, the error
message or raw JSON. Stripe IDs are validated only as `^[A-Za-z0-9_]{1,255}$`; per F9, prefixes are
not authority. `livemode` is. `Via` values `unsent` and `escalate` are written only with
`source='LOCAL'` (§6.1): these are local worker assessments, never provider claims.

### 5.8 Webhook verifier

1. Exactly one `Stripe-Signature` header, ≤2048 bytes, ≤16 comma-separated `k=v` elements.
2. Exactly one `t`: a decimal integer, 1..2^40.
3. At least one `v1`, each exactly 64 lowercase hex. Non-`v1` schemes are ignored (F5).
   A duplicated `t` rejects the header.
4. `|now − t| ≤ 300 s`, in both directions. Tolerance is fixed and never 0. NTP is a deployment
   prerequisite.
5. For each configured secret, compute HMAC-SHA256 over `t` + `.` + the unmodified raw bytes. Accept
   if any `v1` matches under `hmac.Equal`, after decoding the hex to fixed width.
6. Signature checks run before any JSON parsing or DB access. Any failure is `ErrSignature`.

After the signature passes, strict JSON is applied:
- Reject invalid UTF-8, duplicate keys at any depth, trailing data and depth >64.
- The root must have `object="event"`, and `id`, `type`, `livemode`, `created`.
- The projection reads only `data.object.{object,id,client_reference_id,metadata.lc_attempt,metadata.lc_probe}`,
  `api_version` and the presence of `account`.
- If the payload is authenticated but unusable, `Event{Malformed:true, BodySHA256, SignedAt}` is
  returned without error, so it can be quarantined (§9.1). It is never dropped.
- Event bytes are never logged or returned.

## 6. Persistence: `migrations/0061_stripe_psp.sql` + `migrations/post_river/0012_stripe_payment.sql`

- 0044–0059 are reserved for other lanes and 0060 is T10.
- Functions that reference `river_payment.river_job` must live in the post-River phase.
  `start_payment` already lives there, per legacy-runtime-isolation-v1. `0012` is the next free
  post-River number on `ce147fa`; the integrator renumbers at merge.
- Neither file edits an applied migration. Both run under the existing advisory lock and checksum
  ledger.
- The business file must apply cleanly on a DB that is fresh or populated at the latest version.

### 6.1 Widening existing constraints (0061)

- **`integration.merchant_accounts`**
  - `provider IN ('payuni','stripe')`.
  - `provider<>'stripe' OR account_id ~ '^acct_[A-Za-z0-9]{1,59}$'`.
  - `CREATE UNIQUE INDEX stripe_one_account_per_environment ON integration.merchant_accounts(environment) WHERE provider='stripe'` (D1).
  - RESTRICTIVE policy for `commerce_runtime` INSERT/UPDATE: `provider='payuni'`.
- **`integration.account_credentials`**
  - Add `custody text NOT NULL DEFAULT 'DB_AEAD' CHECK(custody IN ('DB_AEAD','ENV_PLATFORM'))`.
  - Add `key_fingerprint bytea`.
  - Drop NOT NULL on `nonce` and `ciphertext`.
  - `CHECK((custody='DB_AEAD') = (nonce IS NOT NULL AND ciphertext IS NOT NULL AND key_fingerprint IS NULL))`.
  - `CHECK(custody='DB_AEAD' OR (key_id='env' AND octet_length(key_fingerprint)=32))`.
  - Unique `(tenant_id,store_id,connection_id,key_fingerprint) WHERE custody='ENV_PLATFORM'`.
  - RESTRICTIVE policy for `commerce_runtime` INSERT: `custody='DB_AEAD'`.
  - The PAYUNi load, hosted and query definers already filter `provider='payuni'` and stay
    unchanged.
- **`payments.method_versions`**
  - Replace the single-value CHECKs with:
    `(provider='payuni' AND code IN (<5 existing>) AND country='TW' AND currency='TWD')
     OR (provider='stripe' AND code='stripe_checkout' AND country ~ '^[A-Z]{2}$' AND currency IN ('HKD','USD','TWD','JPY'))`.
  - The Go `SetMethod` allowlist is unchanged: merchants cannot write Stripe methods (§13).
- **`payments.account_qualifications`**
  - `code IN ('payuni_credit','stripe_checkout')`.
  - `CHECK(code<>'stripe_checkout' OR proof_class<>'REAL_LIVE')` (D14).
- **`checkout.payment_attempts`**
  - `method_code IN ('payuni_credit','stripe_checkout')`.
  - The currency/amount CHECK becomes
    `(method_code='payuni_credit' AND currency='TWD' AND amount_minor BETWEEN 100 AND 19999900 AND amount_minor%100=0)
     OR (method_code='stripe_checkout' AND payments.stripe_amount_ok(currency,amount_minor))`.
  - Everything else, including `state='PAYMENT_PENDING'` and `merchant_trade_no`, is unchanged.
- **`integration.operations`**
  - `provider_reference` length ≤255 (a relaxation).
  - `operation_actor_family` gains the disjunct
    `(actor_kind='BUYER_PAYMENT_QUERY' … AND provider='stripe' AND action='stripe.checkout_session' …)`.
    The integrator re-derives the full CHECK from `pg_get_constraintdef` at merge time, preserving
    MEDIA_ATTEMPT and any later family. SP06 asserts that every prior family still validates.
- **`payments.provider_observations`**
  - `source IN ('QUERY','LOCAL')`.
  - `CHECK(source='QUERY' OR report->>'Provider'='stripe')`.
- **`payments.facts`**
  - `kind IN ('AUTHORIZED','CAPTURED','CLOSED_UNPAID')`.
  - `CHECK((kind='CLOSED_UNPAID' AND amount_minor=0) OR (kind<>'CLOSED_UNPAID' AND amount_minor BETWEEN 1 AND 999999999999))`.
  - `currency ~ '^[A-Z]{3}$'`.
  - `provider_reference ~ '^[A-Za-z0-9_-]{1,255}$'`.
  - A trigger rejects `CLOSED_UNPAID` when an `AUTHORIZED` or `CAPTURED` fact exists for the
    attempt. The reverse is allowed and becomes a review (D13).
- **`payments.review_cases.reason`** adds:
  - `PROVIDER_AMOUNT_MISMATCH`
  - `PROVIDER_PRESENTMENT_DRIFT`
  - `PROVIDER_SESSION_DUPLICATE`
  - `PROVIDER_IDENTITY_MISMATCH`
  - `PROVIDER_EXPIRY_UNCONFIRMED`
  - `PROVIDER_ASYNC_PENDING`
  - `CLOSURE_CONTRADICTED`
  These names are provider-neutral so PAYUNi can reuse them.
- **`inventory.ledger`**
  - `ledger_checkout_actor` gains
    `(actor_kind='SYSTEM_PAYMENT' AND kind='RELEASE' AND payment_fact_kind='CLOSED_UNPAID' AND operation='checkout.payment.close' AND command_key=payment_attempt_id::text AND reservation_id=checkout_id AND principal_id IS NULL AND buyer_owner_id IS NOT NULL AND buyer_session_id IS NOT NULL)`.
  - `inventory.guard_payment_ledger()` is replaced with the identical CAPTURED branch plus a RELEASE
    branch. The RELEASE branch requires:
    - a `CLOSED_UNPAID` fact for the attempt;
    - no `CAPTURED` fact;
    - the order and provenance match;
    - `quantity = −delta_reserved` per reservation line.
- **`checkout.events`**
  - Action `checkout.payment_closed` with actor `SYSTEM_PAYMENT`.

### 6.2 New tables and why they are needed

Why three new tables:
- **`payments.stripe_sessions`.** Attempts are immutable initiation records, but Stripe needs mutable
  set-once pins and deadline state. `hosted_payment_pages` has a fixed 4-field PAYUNi form, a 60 s
  deadline CHECK and one-shot semantics, all incompatible (inventory §2).
- **`payments.stripe_webhook_receipts`.** Permanent event-id dedupe must be independent of River
  retention (§16.1). The Meta inbox tables are Meta-specific.
- **`payments.stripe_signals`.** The durable linkage row that River routing requires for a new job
  kind (the legacy-runtime-isolation pattern).

```sql
CREATE TABLE payments.stripe_sessions (
 tenant_id uuid NOT NULL, store_id uuid NOT NULL, owner_id uuid NOT NULL, attempt_id uuid PRIMARY KEY,
 environment text NOT NULL CHECK(environment IN ('SANDBOX','LIVE')),
 account_id text NOT NULL CHECK(account_id ~ '^acct_[A-Za-z0-9]{1,59}$'),
 locale text NOT NULL CHECK(locale IN ('zh-CN','zh-TW','en')),
 config_digest bytea NOT NULL CHECK(octet_length(config_digest)=32),
 unit_amount bigint NOT NULL CHECK(unit_amount BETWEEN 1 AND 99999999),
 create_params jsonb NOT NULL CHECK(jsonb_typeof(create_params)='object' AND octet_length(create_params::text)<=4096),
 attempt_created_at timestamptz NOT NULL,
 expires_at timestamptz NOT NULL, send_deadline timestamptz NOT NULL, handoff_cutoff timestamptz NOT NULL,
 create_first_sent_at timestamptz, create_last_sent_at timestamptz,
 create_send_count integer NOT NULL DEFAULT 0 CHECK(create_send_count BETWEEN 0 AND 200),
 create_body_sha256 bytea CHECK(create_body_sha256 IS NULL OR octet_length(create_body_sha256)=32),
 create_suppressed_at timestamptz,
 session_id text UNIQUE CHECK(session_id ~ '^[A-Za-z0-9_]{1,255}$'),
 session_url text CHECK(octet_length(session_url)<=4096 AND session_url ~ '^https://checkout\.stripe\.com/[!-~]+$'),
 payment_intent_id text CHECK(payment_intent_id ~ '^[A-Za-z0-9_]{1,255}$'),
 pinned_at timestamptz, url_purged_at timestamptz, first_handed_out_at timestamptz,
 cancel_requested_at timestamptz,
 refresh_count integer NOT NULL DEFAULT 0 CHECK(refresh_count BETWEEN 0 AND 30), last_refresh_at timestamptz,
 signal_count integer NOT NULL DEFAULT 0 CHECK(signal_count BETWEEN 0 AND 64),
 expire_calls integer NOT NULL DEFAULT 0 CHECK(expire_calls BETWEEN 0 AND 500), last_expire_at timestamptz,
 CHECK(expires_at=date_trunc('second',attempt_created_at)+interval '40 minutes'),
 CHECK(send_deadline=attempt_created_at+interval '7 minutes'),
 CHECK(handoff_cutoff=expires_at-interval '5 minutes'),
 CHECK((session_id IS NULL)=(pinned_at IS NULL)),
 CHECK(session_url IS NULL OR session_id IS NOT NULL), CHECK(url_purged_at IS NULL OR session_url IS NULL),
 CHECK((create_first_sent_at IS NULL)=(create_send_count=0)
   AND (create_first_sent_at IS NULL)=(create_body_sha256 IS NULL)
   AND (create_first_sent_at IS NULL)=(create_last_sent_at IS NULL)),
 CHECK(create_suppressed_at IS NULL OR create_first_sent_at IS NULL),
 CHECK(create_first_sent_at IS NULL OR create_first_sent_at<send_deadline),
 FOREIGN KEY(tenant_id,store_id,owner_id,attempt_id) REFERENCES checkout.payment_attempts(tenant_id,store_id,owner_id,id),
 FOREIGN KEY(tenant_id,store_id,attempt_id) REFERENCES integration.operations(tenant_id,store_id,id)
);
CREATE TABLE payments.stripe_webhook_receipts (
 id uuid PRIMARY KEY, environment text NOT NULL CHECK(environment IN ('SANDBOX','LIVE')),
 account_id text NOT NULL CHECK(account_id ~ '^acct_[A-Za-z0-9]{1,59}$'),
 event_id text CHECK(event_id ~ '^[A-Za-z0-9_]{1,255}$'),
 event_type text CHECK(event_type ~ '^[a-z0-9_.]{1,100}$'), event_created bigint,
 api_version text CHECK(api_version ~ '^[A-Za-z0-9._-]{0,64}$'),
 object_type text CHECK(object_type ~ '^[a-z0-9_.]{0,64}$'),
 session_id text CHECK(session_id ~ '^[A-Za-z0-9_]{1,255}$'),
 body_sha256 bytea NOT NULL CHECK(octet_length(body_sha256)=32), signed_at bigint NOT NULL CHECK(signed_at>0),
 disposition text NOT NULL CHECK(disposition IN ('ACCEPTED','IGNORED','QUARANTINED','MALFORMED')),
 reason text NOT NULL CHECK(reason IN ('accepted','unsubscribed_type','probe_session','connect_event',
  'livemode_mismatch','account_unregistered','object_mismatch','unknown_session','reference_mismatch',
  'profile_mismatch','signal_cap','malformed_json')),
 attempt_id uuid, tenant_id uuid, store_id uuid, signal_id uuid UNIQUE,
 redelivery_count integer NOT NULL DEFAULT 0 CHECK(redelivery_count BETWEEN 0 AND 100000),
 received_at timestamptz NOT NULL DEFAULT clock_timestamp(), last_redelivered_at timestamptz,
 CHECK((disposition='MALFORMED')=(event_id IS NULL)),
 CHECK((disposition='ACCEPTED')=(signal_id IS NOT NULL AND attempt_id IS NOT NULL)),
 CHECK((attempt_id IS NULL)=(tenant_id IS NULL) AND (attempt_id IS NULL)=(store_id IS NULL)),
 UNIQUE(environment,account_id,event_id)
);
CREATE UNIQUE INDEX stripe_malformed_body ON payments.stripe_webhook_receipts(environment,account_id,body_sha256)
 WHERE event_id IS NULL;
CREATE TABLE payments.stripe_signals (
 id uuid PRIMARY KEY, tenant_id uuid NOT NULL, store_id uuid NOT NULL, attempt_id uuid NOT NULL,
 source text NOT NULL CHECK(source IN ('STRIPE_WEBHOOK','BUYER_REFRESH','BUYER_CANCEL')),
 receipt_id uuid UNIQUE REFERENCES payments.stripe_webhook_receipts(id),
 session_id text CHECK(session_id ~ '^[A-Za-z0-9_]{1,255}$'),   -- webhook candidate only
 job_id bigint NOT NULL UNIQUE CHECK(job_id>0),
 created_at timestamptz NOT NULL DEFAULT clock_timestamp(), consumed_at timestamptz,
 outcome text CHECK(outcome IN ('OBSERVED','EXPIRE_REQUESTED','NOOP_TERMINAL','STALE_DROPPED')),
 CHECK((source='STRIPE_WEBHOOK')=(receipt_id IS NOT NULL)), CHECK((consumed_at IS NULL)=(outcome IS NULL)),
 FOREIGN KEY(tenant_id,store_id,attempt_id) REFERENCES integration.operations(tenant_id,store_id,id)
);
```

- **Set-once trigger** `payments.guard_stripe_session()` (BEFORE UPDATE):
  - Frozen columns never change.
  - Pins and markers may only go from NULL to a value.
  - `session_url` may go from a value to NULL only when `url_purged_at` is set in the same update.
  - Counters are monotone.
  - `stripe_sessions.session_id` must also be unique against `operations.provider_reference` of the
    same op. Both are written by `record_stripe_observation`.
- **Receipts are append-only** except `redelivery_count` and `last_redelivered_at`.
- **Retention:**
  - Receipts and signals: permanent minimal metadata, with no body, email or name.
  - `session_url` is purged when the attempt reaches a terminal fact.
  - Body purge policy: none, because no body is stored.
  - U08 may shorten retention of receipt IDs.

### 6.3 Roles, grants and RLS

- **New NOLOGIN INHERIT group `commerce_stripe_ingress`.**
  - The webhook ingress uses a dedicated LOGIN member created `WITH INHERIT TRUE, SET FALSE`.
  - Grants:
    - `USAGE` on schemas `payments` and `river_payment`;
    - `SELECT, INSERT` on `river_payment.river_job` and the matching sequence `USAGE`, which is what
      River `InsertTx` needs;
    - `EXECUTE` on the two webhook functions only.
  - `platform.OpenStripeIngressPool` / `ValidateStripeIngressPool` require:
    - `session_user = current_user = the DSN user`;
    - only this authority;
    - no SET on any group;
    - rejection of writer, worker, owner, hosted, merchant and Meta memberships in either direction.
- **New NOLOGIN group `commerce_payment_registrar`**, with a LOGIN member used only by
  `cmd/stripe-admin`. Its functions are owned by a new NOLOGIN, non-inheritable definer owner,
  `commerce_payment_registry_writer`. That owner has exactly:
  - INSERT on `integration.bindings`, `integration.merchant_accounts`,
    `integration.account_credentials`, `payments.account_qualifications`,
    `payments.method_versions` and `payments.method_heads`;
  - `UPDATE(current_version)` on `payments.method_heads`;
  - `UPDATE(credential_version, updated_at)` on `integration.merchant_accounts`;
  - INSERT on the audit table;
  - SELECT on `integration.account_credentials`, RLS-pinned to the registrar scope, used only by
    `payments.stripe_registrar_credential` (S4).
  Migrations provision no logins or passwords.
- **The three new tables** have FORCE RLS and PUBLIC revoked. No runtime login role gets any direct
  privilege.
  - `commerce_checkout_writer`:
    - `SELECT, INSERT` on `stripe_sessions`;
    - `UPDATE(first_handed_out_at, cancel_requested_at, refresh_count, last_refresh_at, signal_count)`;
    - `SELECT, INSERT` on `stripe_signals`.
    - Policies join the attempt with explicit tenant, store and owner GUCs, like `hosted_payment_pages`.
  - `commerce_integration_writer`:
    - `SELECT` and
      `UPDATE(create_first_sent_at, create_last_sent_at, create_send_count, create_body_sha256, create_suppressed_at, session_id, session_url, payment_intent_id, pinned_at, url_purged_at, expire_calls, last_expire_at, signal_count)`
      on `stripe_sessions`;
    - `SELECT, INSERT, UPDATE(redelivery_count, last_redelivered_at)` on receipts;
    - `SELECT, INSERT, UPDATE(consumed_at, outcome)` on signals.
    - Its policies are `USING(true)`. They are reachable only through lease-fenced or
      signature-fenced definers, as `payment_query_reader` is today.
  - `commerce_runtime` (merchant) receives nothing in v1. A merchant read model is follow-up work.
- **Every function** uses SECURITY DEFINER, `SET search_path=pg_catalog`, fully qualified names and
  revokes PUBLIC. The owner is never a login and never the schema or database owner.
  `pg_get_functiondef` / `proconfig` / ACL checks follow SP06.

### 6.4 SQL entry points

| Function | Phase | Owner | EXECUTE | Contract |
| --- | --- | --- | --- | --- |
| `payments.stripe_amount_ok(text,bigint) boolean`, `payments.stripe_unit_amount(text,bigint) bigint` | 0061 | migration owner (IMMUTABLE, not definer) | writer roles only | The §4 table. Used in CHECKs. |
| `checkout.start_stripe_payment(bytea,uuid,text,bytea,uuid,text,bigint,text,uuid,bigint,text,bytea,text) RETURNS jsonb` | post_river | checkout_writer | `commerce_hosted_runtime` | Parameters: hash, store, key, request_hash, order, method, version, profile, attempt, job, locale, config_digest, return_url. Same lock order and checks as `start_payment`, except: provider `stripe`, code `stripe_checkout`, `stripe_amount_ok`, `a.provider='stripe'`, and the qualification rules (MOCK ⇒ SANDBOX account; `REAL_LIVE` impossible). It verifies the exact `payment_query_v1` job, builds `create_params` and the deadlines, and inserts attempt, op (`action stripe.checkout_session`, `semantic_key payment.stripe:<attempt>`), `stripe_sessions`, the operation event, orders/reservations → AWAITING_PAYMENT/PAYMENT_PENDING, the checkout event and the receipt (operation `checkout.payment.start`, so the key namespace is shared with PAYUNi, I02). It rechecks capability and deadlines after all waits. It returns the same JSON keys as `start_payment`. |
| `checkout.take_stripe_handoff(bytea,uuid,uuid,text,bytea) RETURNS jsonb` | 0061 | checkout_writer | hosted_runtime | Parameters: hash, store, order, profile, stripe_config_digest. Locks the order first and never locks operations. Returns `{order_id, disposition, expires_at, redirect_url?}` (§9.2). Sets `first_handed_out_at` once. |
| `checkout.hosted_payment_view_v2(bytea,uuid,uuid,text,bytea,bytea) RETURNS jsonb` | 0061 | checkout_writer | hosted_runtime | Parameters: payuni_digest (nullable) and stripe_digest (nullable). A superset of v1 (§9.2). Once Go migrates, the existing v1 remains only for its own old tests. |
| `checkout.request_stripe_signal(bytea,uuid,uuid,text,bytea,text,uuid,bigint) RETURNS jsonb` | post_river | checkout_writer | hosted_runtime | Parameters: kind `REFRESH` or `CANCEL`, signal_id, job_id. REFRESH requires `now ≥ last_refresh_at+10 s` and fewer than 30 refreshes. CANCEL sets `cancel_requested_at` once. Neither applies once a terminal fact exists. Returns `{order_id, scheduled}`; `scheduled=false` means no job and the caller must roll back its InsertTx. |
| `payments.stripe_webhook_prepare(text,text,text,text,text,bigint,text,text,text,text,text,boolean,boolean,boolean,bytea,bigint) RETURNS TABLE(disposition text, receipt_id uuid, attempt_id uuid, session_id text)` | 0061 | integration_writer | ingress | Parameters: environment, account, profile, event_id, type, created, api_version, object, session, client_ref, meta_attempt, account_present, probe, malformed, body_sha256, signed_at. Mapping follows §9.1. Returns `DUPLICATE` \| `IGNORED` \| `QUARANTINED` \| `MALFORMED` \| `ACCEPT_PENDING`. |
| `payments.stripe_webhook_commit(uuid,uuid,bigint) RETURNS void` | post_river | integration_writer | ingress | Parameters: receipt, signal, job. Verifies the exact `payment_signal_v1` job args, inserts the signal, marks the receipt ACCEPTED and increments `signal_count` (≤64). |
| `integration.stripe_worker_ready(text,text,bytea) RETURNS bigint` | 0061 | integration_writer | `commerce_worker` | Parameters: environment, account, fingerprint. Returns the registered ENV_PLATFORM credential version, or NULL, in which case the worker refuses to start. |
| `integration.load_stripe_session(uuid,bigint,bytea,text) RETURNS jsonb` | 0061 | integration_writer | worker | Lease/token/profile fenced like `load_payment_query`. Returns frozen scope, account, credential version, `create_params`, deadlines, pins, markers, the latest observation summary (Status, PaymentStatus, first-seen of complete+unpaid), terminal-fact flags, signal session candidate and DB now. Returns no secrets. |
| `integration.mark_stripe_create_sent(uuid,bigint,bytea,text,bytea) RETURNS text` | 0061 | integration_writer | worker | Parameters: body_sha256. Locks the session row and returns `SEND` (first send, only if `now<send_deadline` and not suppressed or cancelled), `RESEND` (same body hash, `now<expires_at+15 min`) or `CLOSED`. Commits before the POST. |
| `integration.note_stripe_expire(uuid,bigint,bytea,text) RETURNS void` | 0061 | integration_writer | worker | Increments `expire_calls` and sets `last_expire_at` before each expire POST, so the call is auditable. |
| `integration.record_stripe_observation(uuid,bigint,bytea,text,jsonb,bigint,text) RETURNS void` | post_river | integration_writer | worker | Parameters: report, reconcile_job, session_url (NULL unless pinning). Validates the lease and the exact §5.7 keys and types. Every observation carrying a non-empty `SessionID` must have our identity (ClientReferenceID = MetadataAttempt = attempt, AccountID, Livemode, ExpiresAt, Mode, card-only). `list` with zero matches, `create` rejections and LOCAL observations carry an empty identity, and their DB preconditions are checked instead. Otherwise it raises, nothing is recorded, and the caller finishes UNKNOWN with `stripe_session_mismatch`. It pins `session_id` (op `provider_reference` too), `url` and `payment_intent_id` set-once. It records a *different* session with our identity as evidence for a duplicate review but never pins it. For LOCAL `unsent` it sets `create_suppressed_at`. It deduplicates by hash, verifies the `payment_reconcile_v1` job, then Completes UNKNOWN/`stripe_report_observed` with DB-clock lease fences. |
| `integration.finish_stripe_query(uuid,bigint,bytea,text,text) RETURNS void` | 0061 | integration_writer | worker | Fixed codes: `stripe_create_uncertain`, `stripe_retrieve_failed`, `stripe_rate_limited`, `stripe_timeout`, `stripe_panic`, `stripe_record_failed`, `stripe_session_mismatch`, `stripe_idempotency_alarm`, `stripe_unavailable`, `stripe_budget_exhausted`, `stripe_terminal_observed`. Always records UNKNOWN. `stripe_terminal_observed` requires a terminal fact and purges `session_url`. |
| `integration.consume_stripe_signal(uuid,uuid,bigint,bytea,text,text) RETURNS void` | 0061 | integration_writer | worker | Parameters: signal, op, gen, token, profile, outcome. |
| `payments.apply_capture(uuid,bytea) RETURNS void` | 0061 | checkout_writer | worker (existing grant) | **Dispatcher.** 0061 runs `ALTER FUNCTION payments.apply_capture(uuid,bytea) RENAME TO apply_capture_payuni_v1`, which keeps its body byte-identical, and revokes its EXECUTE. It then creates a new `apply_capture` that reads the op provider and calls `payments.apply_capture_payuni_v1` or `payments.apply_stripe_observation`. SP06 hashes the renamed body against the 0018 text. |
| `payments.apply_stripe_observation(uuid,bytea) RETURNS void` | 0061 | checkout_writer | none | Rules in §6.5. |
| `integration.register_stripe_platform_account`, `integration.rotate_stripe_platform_key`, `payments.qualify_stripe_method`, `payments.set_stripe_method` | 0061 | registry_writer | registrar | §13 |

**Post-River amendments (0012), each keeping the existing structure:**
- `integration.guard_payment_job_family` admits `payment_signal_v1` with args exactly
  `{operation_id, signal_id, version:1}`: canonical lowercase UUIDs and textual `1`.
- `integration.payment_job_queue` adds signal linkage:
  `stripe_signals.job_id=j.id AND s.attempt_id::text=args.operation_id AND s.id::text=args.signal_id`,
  with the queue taken from `attempt.execution_profile`.
- The reconcile linkage accepts `o.source IN ('QUERY','LOCAL')`.
- The kind lists in `route_payment_queue_v1`, `payment_queue_ready` (and its count) and
  `reject_legacy_family_job` include the new kind.
- No unique keys and no UPDATE trigger changes.
- Old binaries still route correctly.

### 6.5 `apply_stripe_observation` (ordered; review evaluation precedes any early return)

**Setup.**
- Locate the persisted observation by (attempt, hash), with provider `stripe`, and load the frozen
  attempt, op and `stripe_sessions` rows.
- Set GUCs from order provenance, never from the job.
- Lock order → reservation → globally sorted balances, the same order capture uses. Never lock
  operations or bindings.

**Rules, in order:**
1. **Identity guard.** A defensive path, since `record` already refuses these.
   - Any identity mismatch → review `PROVIDER_IDENTITY_MISMATCH`, then return.
2. **Duplicate session.**
   - `SessionID` non-empty and ≠ the pinned `session_id` → review `PROVIDER_SESSION_DUPLICATE`, then
     return. This never captures and never closes; a second session might hold money (§12.1
     multi-receipt).
3. **Money check** (when `Status∈{complete,expired}` or `PaymentStatus='paid'`). Any of these
   → review `PROVIDER_AMOUNT_MISMATCH`, then return with no fact (I05):
   - `Currency ≠ attempt.currency`;
   - `AmountTotal ≠ unit_amount`;
   - `AmountSubtotal ≠ AmountTotal`;
   - any discount, tax or shipping ≠ 0;
   - for paid, `PaymentIntentAmountReceived ≠ AmountTotal` or `PaymentIntentCurrency ≠ Currency`.
4. **Presentment drift.**
   - `PresentmentCurrency ∉ {"", Currency}`, or `CurrencyConversion`, or `PresentmentAmount`
     present with `≠ AmountTotal` → review `PROVIDER_PRESENTMENT_DRIFT`, then continue.
5. **LOCAL `escalate`.** DB state must agree:
   - `EXPIRY_UNCONFIRMED` needs `now ≥ expires_at+60 min` and a latest QUERY status of `open`;
   - `ASYNC_PENDING` needs complete+unpaid first seen ≥60 min ago.
   Then insert the matching review and return.
6. **Paid:** `Status='complete' ∧ PaymentStatus='paid' ∧ PaymentIntentStatus='succeeded'`.
   - Insert `CAPTURED` (amount = attempt amount, reference = session id). Idempotent by primary key.
   - If a `CLOSED_UNPAID` fact exists:
     - add review `CLOSURE_CONTRADICTED` and `PAID_ALLOCATION_FAILED`;
     - set `fulfillment_state=PAID_ALLOCATION_FAILED`;
     - create a `REVIEW_REQUIRED` work item;
     - no stock movement and no reopening (D13).
   - Else if any review case exists for the attempt → `REVIEW_REQUIRED` work item, no allocation
     (capture-v1 rule).
   - Else if the reservation is `PAYMENT_PENDING` and the order is `AWAITING_PAYMENT`:
     - ALLOCATE ledger per line;
     - reservation → `COMMITTED`;
     - order → `CONFIRMED`;
     - `READY` work item;
     - event `checkout.payment_captured`.
   - A paid session whose PaymentIntent is not `succeeded` → review `CONFLICTING_REPORT` and no fact.
7. **Closed.** Insert `CLOSED_UNPAID` when all of these hold: no AUTHORIZED/CAPTURED fact; no
   duplicate or amount review; and one of:
   - (a) `Status='expired' ∧ PaymentStatus='unpaid' ∧ PaymentIntentStatus ∈ {'','canceled','requires_payment_method'}`;
   - (b) `Via='create' ∧ ErrorClass='rejected' ∧ SendCount=1 ∧ no pinned session`;
   - (c) `Via='list' ∧ ListMatchCount=0 ∧ no pinned session ∧ create_first_sent_at IS NOT NULL ∧ now ≥ expires_at+15 min`;
   - (d) LOCAL `unsent ∧ create_suppressed_at IS NOT NULL ∧ create_first_sent_at IS NULL`.

   The fact's amount is 0, and its reference is the session id, or `merchant_trade_no` when no
   session exists. Then, if the reservation is `PAYMENT_PENDING` and the order is
   `AWAITING_PAYMENT`:
   - RELEASE ledger per line (SYSTEM_PAYMENT, `checkout.payment.close`);
   - reservation → `RELEASED`;
   - order → `CANCELLED` / `fulfillment_state CANCELLED`;
   - event `checkout.payment_closed`.

   Otherwise:
   - `expired` with `paid` or a succeeded PaymentIntent → review `CONFLICTING_REPORT`, no closure;
   - a create rejected with `SendCount>1` → no-op (the list path decides).
8. **Anything else** (`open`, complete+unpaid, `no_payment_required`) → no-op, except
   `no_payment_required` → review `CONFLICTING_REPORT`.

Every write is idempotent: facts are unique on (attempt, kind), review cases on (attempt, reason),
and work items on the order. Replaying any observation, in any order, converges.

## 7. State machines

**Stripe attempt sub-state.** Derived from `stripe_sessions` columns and facts, never stored as one
enum:

| State | Entered by | Exits |
| --- | --- | --- |
| CREATE_PENDING | start tx | SEND → CREATE_SENT. Send deadline or cancel with no send → LOCAL `unsent` → CLOSED_UNPAID. |
| CREATE_SENT (unknown) | `mark_create_sent` committed | 200 → OPEN (pinned). First-send 4xx → CLOSED_UNPAID. Uncertain → same-key RESEND until `expires_at+15m`, then list: match → pinned state; none → CLOSED_UNPAID. |
| OPEN | pin | paid → PAID. `now ≥ expires_at` or cancel → EXPIRE_REQUESTED. Webhook or refresh → re-observe. |
| EXPIRE_REQUESTED | expire POST attempted | retrieve `expired`+unpaid → CLOSED_UNPAID. `complete`+paid → PAID. Still `open` → retry each cycle; at `expires_at+60m` → review `PROVIDER_EXPIRY_UNCONFIRMED`, stock kept. |
| COMPLETE_UNPAID | retrieve | paid → PAID. After 60 min → review `PROVIDER_ASYNC_PENDING`. Stock kept; the budget bounds it. |
| PAID | CAPTURED fact | terminal; the poller ends |
| CLOSED_UNPAID | CLOSED_UNPAID fact | terminal; the poller ends. A late signal can still observe a contradiction (D13). |
| REVIEW | review case | manual (resolution UI NOT_IMPLEMENTED) |

**Reservation and order interaction** (§11.5; the existing single stock writer):

| Event | Reservation | Order | Facts | Stock |
| --- | --- | --- | --- | --- |
| Start (Stripe) | HELD→PAYMENT_PENDING | DRAFT→AWAITING_PAYMENT | — | reserved kept |
| Old 15-min expiry job | unchanged (STALE) | unchanged | — | kept |
| Timeout, 404, 5xx, UNKNOWN, elapsed deadline alone | unchanged | unchanged | — | **kept** |
| Paid, consistent, no review | →COMMITTED | →CONFIRMED | CAPTURED | reserved→allocated |
| Paid with review | unchanged | unchanged | CAPTURED + review | kept, REVIEW_REQUIRED work |
| Stripe-confirmed expired+unpaid, or provably never payable (D5) | →RELEASED | →CANCELLED | CLOSED_UNPAID | reserved→available |
| Paid after closure | RELEASED (unchanged) | CANCELLED; fulfillment PAID_ALLOCATION_FAILED | CAPTURED + reviews | none (I24) |

**Operation.**
- The op stays `UNKNOWN` with `reconcile` leases for its whole life. Every claim advances the
  generation (I14).
- After a terminal fact, the poller finishes with `stripe_terminal_observed` and its job completes.
- The op remains claimable by late signals, bounded by `signal_count ≤64` and
  `MaxGenerations 720`.
- Budget exhaustion (the existing `MaxAge 24h`, generations) before a terminal fact finishes
  `stripe_budget_exhausted`. Stock is kept for manual recovery.

## 8. River jobs and workers

| Kind | Args (exact) | Queue | Producer (same tx as) | Consumer |
| --- | --- | --- | --- | --- |
| `payment_query_v1` (existing) | `{operation_id,version:1}` | `payment_{mock,sandbox,live}_v1` by attempt profile | `start_stripe_payment`, `ScheduledAt=now` | `QueryWorker`: provider dispatch → Stripe step |
| `payment_signal_v1` (**new**) | `{operation_id,signal_id,version:1}` | same profile queue | `stripe_webhook_commit` / `request_stripe_signal` | `SignalWorker` |
| `payment_reconcile_v1` (existing) | `{operation_id,report_hash,version:1}` | same | `record_stripe_observation` | `CaptureWorker` → dispatcher |

All kinds use schema `river_payment`, with no unique keys. The routing, linkage and readiness audit
is extended in §6.4 post-River.

**QueryWorker Stripe step.**
- `validQueryOperation` accepts (`payuni`, `payuni.query`) or (`stripe`, `stripe.checkout_session`).
  A `stripe` op branches to `stripeStep` **before** any PAYUNi code runs. The PAYUNi path is
  unchanged.
- If this process has no `StripeRuntime`, it returns a transient error **before Claim**, so no
  generation is burned, with fixed code `stripe_unavailable`.
- Otherwise: read → Claim (commit) → `load_stripe_session` (commit), then:

```
if terminal fact: finish(stripe_terminal_observed); return nil (job completes)
if session not pinned:
   if never sent: if cancel_requested or now ≥ send_deadline → record LOCAL unsent → snooze 5s
                  else mark_create_sent=SEND → POST create
   else if now < expires_at+15m: mark_create_sent=RESEND → POST create (same key, same bytes)
   else: FindCheckoutSessions(window) → 1 match: record via=list(pin) ; 0: record via=list(ListMatchCount=0)
   200 → record via=create (pin id, url, PI) ; ErrRejected/ErrAuthentication → record via=create ErrorClass=rejected
   ErrUncertain/Conflict/RateLimited/Idempotency → finish(code) ; snooze backoff 5,15,45,120 s (cap 120)
else:
   s := Retrieve ; if s.open and (now ≥ expires_at or cancel_requested):
        note_expire ; Expire(key per generation) ; s = Retrieve
   record via=retrieve|expire
   if open and now ≥ expires_at+60m → record LOCAL escalate EXPIRY_UNCONFIRMED
   if complete+unpaid first seen ≥60m → record LOCAL escalate ASYNC_PENDING
snooze: open ∧ now<expires_at → min(60 s, expires_at−now+1 s); open ∧ past deadline → 30 s;
        complete/expired → 5 s (next run sees the terminal fact); complete+unpaid → 5 min;
        429 → 2^n s with jitter, cap 60 s
```

- All HTTP calls in one claim share `CallTimeout`, preserving the existing invariant
  `call + 2*DB + 1 s < lease`.
- On parent cancellation, snooze. On a timeout, finish `stripe_timeout`. A panic finishes
  `stripe_panic`. Nothing detaches external I/O.
- `KeyVersion` in each observation is the version returned by `stripe_worker_ready`, i.e. the
  secret actually used (§16.3, §21.3).

**SignalWorker** (new, same options):
- read op; if it is not a Stripe family op → cancel;
- Claim; if `busy`, snooze 2 s;
- load the exact job's signal under that lease using §0.2; if already consumed, release the
  claim and finish without I/O. If older than 10 min by the returned DB clock, consume
  `STALE_DROPPED` and complete UNKNOWN in one transaction; the poller remains the guaranteed path;
- retrieve the pinned session, or the signal's candidate session when none is pinned or when the
  candidate differs (duplicate evidence);
- for `BUYER_CANCEL` with an `open` session: `note_expire`, expire, retrieve;
- in one transaction reload the exact signal, insert the reconcile job, consume `OBSERVED` or
  `EXPIRE_REQUESTED`, and record last (which closes the lease); roll back all if any step fails;
- if there is a terminal fact and no candidate → `NOOP_TERMINAL`.

**CaptureWorker** is unchanged. Its dispatch happens in SQL.

**Assembly:**

```go
package payments
type StripeRuntime struct{ /* client, account, environment, keyVersion; redacted */ }
func NewStripeRuntime(ctx context.Context, pool *pgxpool.Pool, keys *accounts.Keyring,
	client *stripe.Client, profile string) (*StripeRuntime, error) // VerifyAccount + stripe_worker_ready
type WorkerConfig struct {
	Profile string; Concurrency int; Query QueryWorkerOptions
	Keys   *accounts.Keyring // required (PAYUNi material, Stripe fingerprint domain)
	Stripe *StripeRuntime    // nil ⇒ this process never claims Stripe operations
}
func NewPaymentWorkerClient(ctx context.Context, pool *pgxpool.Pool, c WorkerConfig) (*river.Client[pgx.Tx], error)
func NewSignalWorker(ctx context.Context, pool *pgxpool.Pool, s *StripeRuntime, profile string,
	o QueryWorkerOptions) (*SignalWorker, error)
// Existing NewWorkerClient(ctx,pool,keys,profile,concurrency,opts) == WorkerConfig{Stripe:nil}; PW gates unchanged.
```

In `cmd/payment-worker`:
- One process serves one profile queue and handles PAYUNi and Stripe jobs together, dispatching by
  the op's provider. This resolves inventory item 7.
- `StripeRuntime` is present only when `COMMERCE_STRIPE_ENABLED=1`. PROVIDER_MOCK remains test-only.
- The startup audit adds `stripe_worker_ready`, and `payment_queue_ready` now includes the new kind.

## 9. HTTP

### 9.1 Stripe webhook `POST /v1/stripe/webhook`

**Mounting.** The route is mounted on the API listener next to `/v1/meta`, with no auth other than
the signature. It is not a browser route: no CSRF or cookies. The deployment edge exposes exactly
this path over HTTPS (Stripe requires HTTPS for live). An optional edge allowlist of Stripe IPs is
deployment work, not a control this file relies on.

**Requests the handler rejects:**

| Condition | Response |
| --- | --- |
| Method other than POST | 405, `Allow: POST` |
| Any query string, including a bare `?` | 400 |
| `Content-Type` other than `application/json` (with optional `charset=utf-8`) | 415 |
| `Content-Encoding` other than absent or `identity` | 415 |
| Body over 256 KiB (read limit + 1 byte) | 413 |
| No or invalid signature (§5.8), including a timestamp outside tolerance | 400 |
| More than 32 in-flight admissions (non-blocking semaphore) | 503, `Retry-After: 5` |

The whole request has a 5 s deadline and the DB transaction 2 s. The 5 s applies to the body read as
well (per-request read deadline), and the admission slot is taken only after the bounded body has been
read, so a client that stalls a body can never hold a slot (amended 2026-09-29, r1-final-rulings S2).
All responses are
`Cache-Control: no-store`, with fixed bodies `{"received":true}` or `{"error":"<code>"}`.

**Admission.** One transaction, fed only by the verified `Event`:
1. `stripe_webhook_prepare(configured environment, configured account, configured profile, event…)`.
   - It takes an advisory lock on `(environment, account, event_id)`.
   - An existing event id means a redelivery: increment the count and return `DUPLICATE`. The
     body hash is **not** compared, because `pending_webhooks` changes between deliveries and the
     payload is not authority.
   - A malformed payload → `MALFORMED` receipt, deduplicated by body hash.
   - An `account` field present → `QUARANTINED connect_event`.
   - `livemode` ≠ the configured environment → `livemode_mismatch`.
   - The account is not the registered Stripe account for the environment → `account_unregistered`.
   - Type not in the four `checkout.session.{completed,async_payment_succeeded,async_payment_failed,expired}`
     → `IGNORED unsubscribed_type`.
   - `lc_probe` metadata → `IGNORED probe_session`.
   - `object≠checkout.session` → `object_mismatch`.
   - Mapping: by pinned `stripe_sessions.session_id` first; otherwise by
     `client_reference_id = metadata.lc_attempt` = a Stripe attempt of this account whose
     `session_id` is NULL or differs (the duplicate candidate). None found → `unknown_session`.
     Inconsistent references → `reference_mismatch`.
   - Attempt profile ≠ handler profile → `profile_mismatch`.
   - `signal_count ≥ 64` → `IGNORED signal_cap`.
   - Otherwise → `ACCEPT_PENDING` with the attempt and candidate session.
2. Only for `ACCEPT_PENDING`: generate a signal UUID, then run River `InsertTx` with
   `payment_signal_v1` on `jobqueue.ForProfile(attempt profile)`.
3. `stripe_webhook_commit(receipt, signal, job)`.
4. COMMIT.
5. **Only then** return 200.

Other outcomes:
- A DB failure or cancellation → 503. No early ACK and no goroutine ACK. Stripe retries.
- Every authenticated outcome (DUPLICATE, IGNORED, QUARANTINED, MALFORMED, ACCEPTED) returns 200
  after commit. None is silently dropped (§16.1).
- The event type is advisory only. All four types trigger the same retrieve.

**Construction.**
- `stripewebhook.NewInbox(ctx, ingressPool, profile)` validates the pool through
  `ValidateStripeIngressPool` and builds an insert-only River client for `river_payment`.
- `stripewebhook.NewHandler(verifier, inbox)` exposes no method that accepts an unverified event.
- The handler reads only `STRIPE_WEBHOOK_SECRET[_NEXT]`, `STRIPE_ACCOUNT_ID`,
  `COMMERCE_PAYMENT_PROFILE` and `COMMERCE_STRIPE_INGRESS_DATABASE_URL`.

### 9.2 Buyer routes (private Go; the public BFF mirrors them under `/api/buyer/`)

| Method/path | Input | Success |
| --- | --- | --- |
| GET `/v1/buyer/orders/{id}/payment` | unchanged | `OrderPayment` gains `cancel_requested` (bool). `methods` has 0..2 items (payuni_credit, stripe_checkout). `payment_state` adds `CLOSED_UNPAID`. Stripe `handoff_state` is one of `NONE`, `CREATING`, `READY`, `CLOSED`, `UNAVAILABLE`, and `handoff_expires_at` = the handoff cutoff. |
| POST `.../payment/prepare` | `method_code` ∈ {`payuni_credit`,`stripe_checkout`}; otherwise unchanged | unchanged projection (`currency` per §4, `amount_minor` per the admitted-currency rules) |
| POST `.../payment/handoff` | no body, no key | PAYUNi unchanged. Stripe: `{order_id, disposition, expires_at, redirect_url?}`, where `expires_at` is the handoff cutoff (`expires_at − 5 min`), never renewed. Disposition is `CREATING` (no URL yet; poll GET), `REDIRECT` (URL; **repeatable** until the cutoff; D16), `CLOSED` or `UNAVAILABLE` (binding disabled, qualification revoked, or profile/config mismatch) |
| POST `.../payment/refresh` (**new**) | no body, no key | `{order_id, scheduled}`. The DB throttles it (§6.4); it never triggers provider I/O inline. |
| POST `.../payment/cancel` (**new**) | no body, no key | `{order_id, scheduled}`. It is set-once. The effect is an expire and then a closure *after* Stripe confirms; stock is not released immediately. |

Go API additions in `checkout`:
- `NewHostedPaymentService(ctx, hostedPool, jobs, profile, keys, HostedProviders{PAYUNi *HostedConfig; Stripe *StripeHostedConfig})`.
  The existing `NewHostedPaymentStarter` wraps it for PAYUNi only.
- `StripeHostedConfig{ReturnURL}`, whose digest is
  `sha256(json{"version":"stripe-hosted-v1","return_url","api_version","session_ttl_seconds":2400,"send_window_seconds":420,"handoff_margin_seconds":300})`.
- `HostedHandoff.RedirectURL string \`json:"redirect_url,omitempty"\``.
- `RefreshPayment` and `CancelPayment` return `PaymentSignal{OrderID, Scheduled}`.
- The BeginHosted digest includes the Stripe config digest, so replays with a changed config
  conflict.

Public validator deltas (`apps/storefront/lib/payment-contract.ts`):
- `redirect_url` must match exactly `^https://checkout\.stripe\.com/[!-~]{1,4000}$`.
- The new enums and the `cancel_requested` field are added.
- `methods` allows ≤2 items.
- Refresh and cancel get the same keyless-POST exception as handoff, with CSRF, Origin and context
  checks kept.
- Refresh and cancel errors are nonretryable. A Stripe handoff may be re-requested **only by an
  explicit click**, never on mount, reload or poll.
- The UI opens `about:blank` synchronously on the click, sets `opener=null`, then calls
  `location.replace(redirect_url)`.
- The URL is never persisted, logged or placed in storage.

Per-tenant HTTP rate limiting remains the existing public-exposure prerequisite (buyer-http-v1).
The UI composition is a `buyer-payment-ui` amendment (ui_worker) and is not frozen here.

### 9.3 Return page

- `success_url` and `cancel_url` are both exactly `COMMERCE_PAYMENT_RETURN_URL`, the existing
  neutral central `/payment/return`. There is no query, no `{CHECKOUT_SESSION_ID}` and no tenant in
  the URL.
- The page stays GET/POST-neutral, no-store and tri-language. It writes nothing and infers nothing.
- The original store tab shows the truth, from GET `payment` polling with backoff up to 10 min.
  It offers "check now" (refresh) and "cancel payment".
- The redirect is never evidence of payment (F6).

## 10. Reconciliation and UNKNOWN handling

| Situation | Evidence available | Action | Stock |
| --- | --- | --- | --- |
| Create timeout, network error, 5xx or 409 | none | same key, same bytes, backoff, until `expires_at+15m` (a Stripe replay returns the cached result) | kept |
| Create replayed 500 (cached) | indeterminate (F4) | no new key. Wait; at `expires_at+15m` run a list scan. A match pins and continues normally. None → CLOSED_UNPAID (the URL was never disclosed and Stripe enforced the frozen `expires_at`). | kept until then |
| Create 4xx or auth failure on the **first** send | definitive: nothing executed before | record `rejected` → immediate closure | released |
| Create 4xx after an uncertain earlier send | not definitive: validation or auth can run before the idempotency lookup (F4) | wait for the list path | kept |
| `idempotency_error` | our bug | alarm code; no closure; stays UNKNOWN until budget, then manual | kept |
| Worker down past `send_deadline`, never sent | local DB proof | LOCAL `unsent` → closure | released |
| Retrieve failure or 404 on a pinned session | none | retry every cycle; never infer status | kept |
| Deadline reached, session `open` | retrieve | expire (key per generation) → retrieve; `expired`+unpaid → closure | released after confirmation |
| Expire error | — | retrieve decides; retry next cycle; `+60m` → review | kept |
| Paid at the last second | retrieve `complete`/`paid` | capture, even when the deadline has passed | allocated |
| Webhook missing or late | poller | the poller covers it (60 s while open) | per the above |
| Webhook before the pin (create was uncertain) | candidate session | the signal retrieves the candidate → pin → normal flow | per the above |
| Second session with our reference | retrieve evidence | review DUPLICATE; never capture or close from it | kept |
| Money, presentment or identity drift | retrieve | review (§6.5) | kept (drift: CAPTURED + REVIEW work) |

No path retries a create with a new key (I06, I20). No path treats 404, timeout or an elapsed
deadline as unpaid. No path writes a financial fact from webhook bytes.

## 11. Idempotency keys and uniqueness

| Scope | Key | Retention and justification |
| --- | --- | --- |
| Buyer prepare | existing `Idempotency-Key` → `checkout.command_results` (`checkout.payment.start`) | permanent; shared namespace with PAYUNi (I02) |
| Stripe create | `lc:stripe:cs-create:v1:<attempt uuid>` (≤255, no PII) | used for every send; the last send is before `expires_at+15m` (≤55 min), well inside Stripe's ≥24 h retention |
| Stripe expire | `lc:stripe:cs-expire:v1:<attempt>:<claim generation>` | a fresh key per claim is safe: expire cannot move money, only an `open` session is expired, and a retrieve always follows. A fixed key would pin a cached 500 forever. |
| Webhook | `UNIQUE(environment,account_id,event_id)`; malformed payloads by body hash | permanent |
| Signals / jobs | signal UUID; `stripe_signals.job_id` UNIQUE; routing linkage | permanent |
| Observations | existing `(attempt, report_hash)` | permanent |
| Business | facts `(attempt,kind)`; `stripe_sessions.session_id` UNIQUE; op `provider_reference` unique per binding; review `(attempt,reason)`; work item per order | permanent |

## 12. Secrets, configuration and redaction

| Variable | Process | Rule |
| --- | --- | --- |
| `COMMERCE_STRIPE_ENABLED` | payment-worker | empty/0/1; when disabled, reads nothing else |
| `STRIPE_SECRET_KEY` | payment-worker, stripe-admin **only** | §5.2. `cmd/api` never reads it; a source guard plus an env-sentinel test enforce this (SP15). |
| `STRIPE_ACCOUNT_ID` | worker, API (webhook), stripe-admin | `^acct_[A-Za-z0-9]{1,59}$`; must equal `GET /v1/account` and the registered row |
| `COMMERCE_STRIPE_LIVE_ENABLED` + `COMMERCE_STRIPE_LIVE_APPROVAL_REF` | all three | both or neither; LIVE only (§5.2). v1 SQL still blocks LIVE qualification. |
| `COMMERCE_STRIPE_WEBHOOK_ENABLED` | API | empty/0/1; when disabled, reads nothing else |
| `STRIPE_WEBHOOK_SECRET`, optional `STRIPE_WEBHOOK_SECRET_NEXT` | API **only** | `^whsec_[!-~]{16,249}$`; the two must differ; never in the worker |
| `COMMERCE_STRIPE_INGRESS_DATABASE_URL` | API | `OpenStripeIngressPool` |
| `COMMERCE_STRIPE_CHECKOUT_ENABLED` | API (buyer payment) | adds `stripe_checkout` to the hosted service; requires the existing `COMMERCE_PAYMENT_RETURN_URL` and `COMMERCE_PAYMENT_PROFILE` |
| `COMMERCE_STRIPE_REGISTRAR_DATABASE_URL` | stripe-admin | registrar pool |

**Rules for secrets:**
- Secrets come only from the environment or a secret manager. They never appear in PG, in a command
  receipt, in River args or in a URL.
- Errors are fixed codes. They never contain Stripe error messages, raw bodies, DSNs or key
  material.

**Logging may carry only:** fixed codes, internal UUIDs, durations and HTTP status classes.

**Logging must never carry (log-capture sentinel test SP13/SP15):**
- `Authorization`, the secret key, `whsec`, the `Stripe-Signature` header;
- raw webhook or API bodies;
- the session URL;
- `customer_details`, email, name, address;
- card or wallet data;
- `client_secret`;
- Stripe error messages.

Stripe event and session IDs are stored in PG, not logged. River's raw logger stays discarded
(worker runtime rule).

## 13. Operator registrar `cmd/stripe-admin` (the only Stripe account and method writer)

Subcommands:
- **`register --tenant --store --principal --market --country`**: reads `STRIPE_SECRET_KEY` and
  `STRIPE_ACCOUNT_ID` and runs `VerifyAccount`.
- **`rotate --connection --expected-version`**: runs after the env key changes.
- **`qualify`**: runs the probe.
- **`method --visible --sort --min --max`**: sets the `stripe_checkout` method revision.

The probe:
1. Creates one session with `metadata[lc_probe]=1` at the method minimum in the store currency,
   `expires_at` = now + 31 min.
2. Expires it immediately.
3. Retrieves it and requires `expired` + `unpaid` + `livemode=false`.
4. Issues `REAL_SANDBOX` qualification with `evidence_ref = stripe-probe:<session id>`, valid for 30
   days.

Registrar limits:
- **LIVE:** the CLI refuses, and the SQL CHECK (D14) blocks it independently.
- **Principal:** must be an existing owner membership of the tenant.
- **Audit:** every write records an audit row. The fingerprint is the only key-derived value stored.
- **Merchants:** `SetMethod` keeps rejecting Stripe codes. Merchant self-service for Stripe is out
  of scope, because binding the platform env key to an arbitrary store would pool funds (D1).

Tests seed the same rows through owner fixtures: PROVIDER_MOCK evidence for MOCK, and the registrar
path for SANDBOX (SP21).

## 14. Test tiers and gates

| Tier | Meaning | Required environment | When the environment is absent |
| --- | --- | --- | --- |
| UNIT | Go/Node, no PG | none | always runs |
| MOCK | real PG 18 + real River + real API/worker binaries + independent **fake Stripe HTTP server** (`stripetest`, test_worker-owned, written from the docs, not from the adapter): idempotency cache semantics (param compare, cached 500, uncached validation/409), fault hooks (drop-after-execute, 429, delay), session state control, webhook signer | Docker PG | always runs |
| SANDBOX | real `api.stripe.com`, test key, account `acct_1UJDb0RusP6Wwj7e`, livemode verified false before any write | `STRIPE_SANDBOX=1`, `STRIPE_SECRET_KEY` (test), `STRIPE_ACCOUNT_ID` | `t.Skip("NOT_RUN: …")`; the harness records **NOT_RUN**, never PASS |
| SANDBOX-WEBHOOK | SANDBOX plus real signed deliveries | `STRIPE_SANDBOX_WEBHOOK=1` + Stripe CLI `stripe listen --forward-to` (its own `whsec`, dev-tool dependency to be admitted) | NOT_RUN |
| BROWSER | Playwright Chromium, Next→Go→PG→real Stripe hosted page | `STRIPE_BROWSER=1` + SANDBOX variables | NOT_RUN |
| LIVE | — | forbidden in v1 | NOT_APPLICABLE; only the refusal is tested |

| Gate | Test | Tier | Required |
| --- | --- | --- | --- |
| SP01 | `TestStripeSP01Config` | UNIT | Key/profile/flag matrix (§5.2) incl. live key in SANDBOX, test key in LIVE, flag without ref; `WebhookConfig` bounds; redaction of Config/Client/Session/Event/CallMeta under `%v %+v %#v json` contains no key/whsec/URL sentinel |
| SP02 | `TestStripeSP02Currency` | UNIT + REAL_PG | §4 vectors (min/max/step, TWD %100, JPY, rejected ISK/UGX/HUF/BHD/lowercase/overflow); Go↔SQL `stripe_amount_ok`/`stripe_unit_amount` parity; JPY storefront formatting vector |
| SP03 | `TestStripeSP03CreateBody` | UNIT | Golden body bytes for a fixture attempt; sorted, deterministic; exact key set; forbidden params absent; `Stripe-Version`, `Idempotency-Key`, headers; resend byte-identical; any extra or missing key → `ErrInvalid` |
| SP04 | `TestStripeSP04Classify` | UNIT | Every §5.6 row incl. `Idempotent-Replayed` 500, `Stripe-Should-Retry`, 3xx refused, oversize/duplicate-key/malformed JSON, wrong `object`, livemode mismatch, TLS/redirect/cookie policy, context cancel |
| SP05 | `TestStripeSP05Webhook` + `node scripts/dev/stripe-webhook-check.mjs` | UNIT + cross-language | §5.8: valid, multi-`v1` roll, `v0` ignored, missing/duplicate `t`, bad hex/length, ±300 s boundary (300 ok, 301 reject, both directions), byte flip, second secret, header/element limits, `hmac.Equal` source guard, strict JSON after signature → `Malformed`; vectors independently verified with Node `crypto` |
| SP06 | `TestStripeSP06Schema` | REAL_PG | Fresh + populated-latest upgrade; repeat/checksum; every §6.1 CHECK negative (payuni rows still bound by PAYUNi rules); one-Stripe-account-per-environment; `REAL_LIVE` stripe qualification rejected; custody CHECK and restrictive runtime policies; set-once trigger; column-privilege matrix from `information_schema`; FORCE RLS; definer owner/`proconfig`/ACL; PUBLIC revoked; renamed `apply_capture_payuni_v1` body hash = 0018 text; the actor-family re-derivation keeps every prior family; post-River routing/guard/readiness/legacy exclusion include `payment_signal_v1`; `payment_queue_ready()` true |
| SP07 | `TestStripeSP07Start` | REAL_PG | Atomic start (order, reservation, attempt, op, `stripe_sessions` frozen params/deadlines, receipt, event, job at now); replay; changed locale/config/profile/method → 409; different keys ≠ second attempt; concurrent PAYUNi vs Stripe prepare on one order → one winner; mixed-provider lock workload → zero 40P01; admission drifts (disabled/hidden/stale method, revoked/expired qualification, MOCK evidence on SANDBOX, wrong environment, currency or amount not admitted, hold expired during waits); zero provider calls |
| SP08 | `TestStripeSP08HappyMock` | MOCK | Real worker creates exactly one fake session (one key) → pin → handoff REDIRECT repeated with the same URL → fake pays → signed webhook through the real API → signal → retrieve → CAPTURED, ALLOCATE, COMMITTED, CONFIRMED, READY work item, event; poller ends `stripe_terminal_observed`; URL purged; on_hand unchanged. Two tenants: tenant A uses Stripe and tenant B uses PAYUNi on the same profile queue, because the D1 index allows one Stripe account per environment. Forged cross-tenant signals and receipts are rejected. |
| SP09 | `TestStripeSP09CreateUnknown` | MOCK | Drop-after-execute → same-key replay pins the same session; cached 500 → wait → list match pins; cached 500 + no match → closure only at `expires_at+15m`; first-send 400 → immediate closure; 400 after an uncertain send → no closure until the list path; 409/429 backoff; `idempotency_error` alarm, no closure; worker down past `send_deadline` → LOCAL `unsent` closure and **no POST ever sent** afterward; the fake asserts ≤1 distinct create key per attempt; disclosed owner-fixture clock aging |
| SP10 | `TestStripeSP10Deadline` | MOCK | At `expires_at`: expire (key per generation) → retrieve expired → CLOSED_UNPAID, RELEASE per line, RELEASED, CANCELLED, event; available stock restored exactly; expire 400 (already expired) and 500 paths; paid in the last second → capture, not closure; still open at `+60m` → review, stock kept; expired+paid → CONFLICTING, no release; old 15-min expiry job stays STALE |
| SP11 | `TestStripeSP11BuyerSignals` | MOCK + HTTP_PG | Cancel before send / while unknown / while open / racing a payment; refresh throttle (10 s, ≤30) and `scheduled=false` with no job; signal after terminal → NOOP; stale signal dropped; lease busy → snooze without generation loss beyond one claim |
| SP12 | `TestStripeSP12MoneyChecks` | MOCK | Each §6.5 rule: amount/currency/subtotal/discount/tax/shipping/PI-amount mismatch → no CAPTURED fact (I05); presentment drift and legacy `currency_conversion` → CAPTURED + REVIEW work, no allocation; identity mismatch not recorded; duplicate session → review, never capture/close; `no_payment_required`; async events and complete+unpaid → escalation after 60 m; late paid after closure → CAPTURED + PAID_ALLOCATION_FAILED + CLOSURE_CONTRADICTED, no stock movement; replay in any order converges |
| SP13 | `TestStripeSP13WebhookHTTP` | HTTP_PG (real API binary) | Every §9.1 status/limit; blocked-commit hook proves no ACK before commit; commit failure → 503; duplicate id with a different body (pending_webhooks) → DUPLICATE, no job; every quarantine/ignore reason; malformed after signature → 200 MALFORMED once; concurrency cap 503; log capture and a DB-wide scan find no signature, body, secret, URL, email or name sentinel; no row stores a body |
| SP14 | `TestStripeSP14BuyerHTTP` + BFF Node tests | HTTP_PG + Node | Prepare/handoff/refresh/cancel/view exact keys and enums; handoff CREATING→REDIRECT (repeatable)→CLOSED/cutoff/UNAVAILABLE; foreign or other-store order 404; strict no-body/no-key; public validators accept only the Stripe URL pattern; **PAYUNi responses byte-identical to pre-change golden files**; BPH/BPT suites unchanged |
| SP15 | `TestStripeSP15Process` | process + PG | Disabled flags read nothing; every config negative; `GET /v1/account` mismatch (fake) and unregistered fingerprint refuse start; the API never reads `STRIPE_SECRET_KEY` (source guard + env sentinel); the worker never reads `whsec`; one worker process consumes PAYUNi (signed mock) and Stripe (fake) jobs on one profile queue; a Stripe job on a Stripe-disabled worker → no claim; SIGTERM graceful; ingress pool role validation, both directions |
| SP16 | `TestStripeSP16Sandbox` | **SANDBOX** | Refuses unless test key + `livemode=false` + account = `acct_1UJDb0RusP6Wwj7e`; `VerifyAccount`; create with frozen params (store currency, method min) → retrieve exact fields (adaptive pricing and managed payments disabled, presentment null, card only, amount pinned); same-key replay returns the same id with `Idempotent-Replayed`; changed-params same key → `idempotency_error`; expire → expired/unpaid; second expire → 400; list window finds it; below-minimum → first-send 400; `expires_at`=now+29 min → 400; records the least RAK permission set. Creates test objects only; no charge |
| SP17 | `TestStripeSP17SandboxWebhook` | SANDBOX-WEBHOOK | Real signed `checkout.session.expired` via the CLI forward → admission → signal → closure; redelivery dedupe |
| SP18 | `stripe-browser.spec.ts` | **BROWSER** | Pay → child window → hosted page → card `4242 4242 4242 4242`, any future expiry, any CVC and postal code → neutral return → original tab shows CAPTURED (poller; webhook if SP17 is available) → CONFIRMED; decline `4000 0000 0000 0002` → stays open → buyer cancel → closure; 3DS `4000 0027 6000 3184` → authenticate → paid; 3 locales, desktop + mobile Chromium; screenshots hashed; no URL in storage or logs |
| SP19 | `TestStripeSP19LiveRefusal` | UNIT + REAL_PG | LIVE is refused at config, CLI and SQL; no live network call is possible from tests |
| SP20 | `TestStripeSP20Guards` + root | REVIEW + regression | Package headers and §15 comments present; only `psp/stripe` dials `api.stripe.com`; no `stripe-go` import; `NewWithMockTransport` only in tests and the MOCK assembly; no forbidden log fields; full `go test -race ./...`, `go vet ./...`, `python3 scripts/check_packet.py`; every prior PS/PQ/CF/HP/BPH/BPT/BPU/PW/EW/LRI gate unchanged; independent test_worker + security_reviewer verdicts; author is not sole acceptor |
| SP21 | `TestStripeSP21Registrar` | REAL_PG (+ SANDBOX probe) | Register/rotate/qualify/method against owner-only roles; merchant cannot create or rebind a Stripe account or method; second store or second account in the same environment rejected; rotation blocks new starts until re-qualified; historical attempts keep processing; probe evidence format; LIVE refused |

**Commands** (record exit codes and commit SHA):
- `go test -count=1 ./internal/integrations/psp/stripe/...`
- `node scripts/dev/stripe-webhook-check.mjs tests/payments/stripe-webhook-vectors.json`
- `bash scripts/dev/test-local.sh --stripe` (integrator mode).
  - It first guards with `grep -q '^func TestStripeSP' tests/foundation/stripe_psp_test.go`.
  - It then runs `go test -race -count=1 -timeout=600s -json -run '^TestStripeSP' ./internal/payments/... ./internal/checkout/... ./tests/foundation`.
  - It prints `MOCK Stripe fake server; no api.stripe.com request; SANDBOX/BROWSER NOT_RUN`.
- `STRIPE_SANDBOX=1 … bash scripts/dev/test-local.sh --stripe-sandbox`. This mode **fails** if any
  SP16/SP21 test reports SKIP while the mode was requested.
- `--stripe-browser` for SP18.
- The full `bash scripts/dev/test-local.sh`, and `go vet ./...`.

**Parsing rule:** the harness parses `go test -json`. SKIP is NOT_RUN. Zero observed cases, missing
logs, cancelled runs and parse failures are never PASS (§28.1, I18).

## 15. Ownership, comments (D9) and sequencing

| Artifact | Owner |
| --- | --- |
| `internal/integrations/psp/stripe/**` (adapter, verifier, unit tests SP01–05) | integration_worker |
| `internal/integrations/psp/stripe/stripetest/**`, `tests/foundation/stripe_*_test.go`, `tests/payments/stripe-*.json`, `scripts/dev/stripe-webhook-check.mjs`, `apps/storefront/tests/stripe-browser.spec.ts` | independent test_worker |
| `internal/payments/stripe_*.go`, `internal/payments/stripewebhook/**`, `internal/checkout/stripe*.go`, `accounts.StripeKeyFingerprint` | commerce_worker |
| `migrations/0061_stripe_psp.sql`, `migrations/post_river/0012_stripe_payment.sql`, `internal/jobqueue`, `internal/platform` pool validators, `cmd/api` mount, `cmd/payment-worker` assembly, `cmd/stripe-admin`, `internal/buyerhttp` routes, `core-openapi.json`, `test-local.sh` modes, `tasks.json` T11 amendment, `sources.json` rows for F1–F12 | integrator |
| `apps/storefront` payment contract/BFF/UI (after the UI amendment) | ui_worker |

**D9 comment requirements** (SP20 source-guarded):
- Every Go file starts with an ownership line and a non-goal line. Example:
  `// Package stripe owns Stripe Checkout Session wire calls and webhook signature verification.`
  `// It never reads PG, decides payment state, releases stock or retries on its own.`
- Every Stripe wire constant or parameter carries a comment with its docs URL and the retrieval date
  `2026-09-28`, as in F1–F12.
- Every monetary comparison carries `// I05:` and the rule it enforces.
- Every UNKNOWN or retry branch explains why it retries the *same* key, or why it may not retry.
- Every stock transition carries `// §11.5:` and the evidence it requires.
- Each call across packages, and each SQL statement on another domain's table, names the table/role
  and why. Example: `payments.apply_capture // single stock writer; worker never writes ledger`.
- Every new table, column, function and role gets `COMMENT ON`, naming its owning package, the
  roles allowed to use it and its non-goals.

**Sequence:**
1. Owner answers to Q1–Q5, then an independent preflight, then FREEZE.
2. In parallel, with at most two initial writers:
   - integrator: 0061 + post_River, with SP06/SP02-SQL;
   - integration_worker: adapter + SP01–05.
   The test_worker starts `stripetest` and the vectors immediately.
3. commerce_worker: payments, checkout and webhook inbox, SP07–12. This starts only after 0061 and
   the §5.3 interface are frozen (AGENTS.md: no parallel upstream/downstream before the interface
   freezes).
4. integrator: HTTP, process and registrar, SP13–15/21.
5. SANDBOX SP16/17.
6. UI amendment, then SP18.
7. SP19/20, independent verdicts.

At most two targeted fixes per blocker, then escalate.

## 16. Known limits / NOT_RUN

- **Evidence.** Everything here is DESIGN. Every SP gate is NOT_RUN. SANDBOX, SANDBOX-WEBHOOK and
  BROWSER stay NOT_RUN without keys and the CLI. LIVE is NOT_APPLICABLE.
- **Single account.** There is one Stripe account per environment and one store (D1): no Connect,
  no multi-merchant, no merchant self-service. Key rotation is operator-run and requires
  re-qualification.
- **Payment methods.** Card only (wallets only where Stripe presents them as card). No Link, FPS,
  Alipay, WeChat Pay or bank methods. Async outcomes are review-only.
- **Currencies.** HKD, USD, TWD and JPY only. Minimums are local prefilters; Stripe's
  settlement-currency minimum decides.
- **Adaptive Pricing** is disabled. A buyer abroad pays in the store currency, possibly with issuer
  FX fees.
- **Stock hold.** After payment start, stock is held for up to about 40 minutes plus closure latency.
  If the create is uncertain, it is held until `expires_at + 15 min`. Buyer cancel shortens the hold
  only after Stripe confirms.
- **Retries.** One attempt per order. A retry means a new checkout.
- **Late payment.** No automatic reallocation. No refund execution: refunds, disputes and payouts
  are T11 follow-ups, and no real refund may be issued. Review resolution UI is not implemented.
- **Webhook evidence.** Raw webhook bodies are not retained. Forensics rely on receipts, Stripe
  Dashboard events (30-day API window) and our observations.
- **Clocks.** Both webhook tolerance and deadlines depend on NTP. Under skew, a `send_deadline` near
  30 min can produce a first-send 400, which closes the attempt definitively.
- **API version.** It is pinned. The webhook endpoint must be created with the same version (a
  runbook item). An upgrade needs an amendment and SP16/SP18 again.
- **Capacity and rate limits.** Stripe's rate limits (lower in test mode) bound worker concurrency.
  No capacity or latency figure is claimed. Polling costs about 40 retrieves per attempt, worst
  case.
- **Shared kind.** `payment_query_v1` is now shared by both providers. Regressions on the PAYUNi
  path are covered only by SP14/SP20 and the prior gates.
- **Browser coverage.** Chromium only.
- **Merchant views.** The merchant orders read model shows closure only as `CANCELLED`. A dedicated
  merchant payment view is follow-up work.

## 17. Open questions for the owner (blocking FREEZE unless marked)

- **Q1 (blocking).** Is `acct_1UJDb0RusP6Wwj7e` (香港大碗貿易有限公司) the merchant of record's own
  account, and is the launch exactly one store? If not, the design must use Connect plus an ADR
  instead of D1.
- **Q2 (blocking).** Accept the fixed 40-minute session and stock hold after payment start? The
  alternative is a per-store TTL between 30 minutes and 24 hours.
- **Q3 (blocking).** Card only at launch? HK buyers often expect FPS, Alipay or WeChat Pay, which
  are delayed or redirect methods and would need a PaymentIntent-level release protocol.
- **Q4 (blocking).** Which launch market and currency (HKD from the sandbox, or TWD)? This confirms
  the D15 allowlist and the method minimums.
- **Q5 (blocking).** For a payment after closure: manual refund work only (D13), or automatic
  reallocation when stock allows? The latter conflicts with capture-v1's "never reopen a cancelled
  order".
- **Q6.** Keep Adaptive Pricing and Managed Payments disabled per session? Managed Payments changes
  the merchant of record and tax.
- **Q7.** Approve the Stripe CLI as a dev and test tool for SP17 and SP18 webhook evidence?
- **Q8.** Public webhook exposure: the edge TLS path, optional Stripe IP allowlist, and who creates
  the webhook endpoint (pinned API version, four events).
- **Q9.** Retention period for webhook receipts and signals (U08).

## Amendment pointer (2026-09-29)

`contracts/stripe-refund-v1.md` (FROZEN 2026-09-29) re-creates, in `0062` / post-River `0013`,
these stage-B functions: `stripe_webhook_prepare`, `stripe_webhook_commit`,
`guard_stripe_receipt_link`, `load_stripe_signal`, `consume_stripe_signal`, `payment_job_queue`,
`apply_stripe_observation`. The refund contract's §4.4/§4.6 text governs their new bodies; this
file's SP gates remain required and must stay green.
