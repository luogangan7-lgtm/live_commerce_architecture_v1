# Stripe live enable v1 — per-store LIVE activation, canary, kill switch and monitoring

Status: **v1 FROZEN 2026-09-30 (DESIGN; gates NOT_RUN)** — round-1 review (`output/contract-review/r2-design-wave.json`
key `stripe-live`, verdict BLOCK, 8 P1 + 9 P2), round-2 finding (1 P1: ops-admin allowlist) and round-3
P2s (`output/contract-review/r2-round3.json`, PASS_WITH_P2) addressed in place. Evidence label for this
file: DESIGN. Every SL gate below is NOT_RUN. Nothing here moves real money: the only LIVE charge in
this contract is the owner's own canary (§10), performed and approved by the owner personally.

Amends [stripe-psp-v1](stripe-psp-v1.md) (§0.2 binding; D14, §5.2, §12, §13, SP19) and
[stripe-refund-v1](stripe-refund-v1.md) (LIVE refused in §4.2/§4.4, RF03). Architecture §2.2,
§11.5, §12, §16 and invariants I05, I06, I16, I17, I18, I20 are authoritative; where they conflict
with this file, this file is wrong. PAYUNi code is unchanged; in the LIVE deployment PAYUNi stays
unavailable until its own B3 LIVE approval (LD1). No Connect, pooled funds, new engine, new queue,
new dependency or new route is introduced.

## 0. Owner input, decisions and rulings

**Owner input (chat relayed by the workflow harness, 2026-09-29):** "激活 Stripe 正式收款：如果现在没有
什么问题，我可以切换或者给你正式的API." This is intent to go live, **not** the per-store approval
record required below. **Owner decision O-D (r2-design-rulings):** production is operated by Claude on
the owner's server; the owner supplies secrets as files; secrets never go through chat. So the live key
is not pasted in chat; the owner places it in a file on the server and Claude passes the file *path* to
`ops-admin.sh` (LD3).

Today LIVE is refused in four layers (stripe-psp D14): config (§5.2 `admit`), CLI
(`stripeadmin.environment="SANDBOX"`, `ProbeCheckout` refuses LIVE), deploy (`ops-admin.sh`, preflight
P06, compose) and SQL (`stripe_qualification_no_live_check`, `environment='SANDBOX'` in endpoint/refund
tables and in every Stripe registrar/start/refund definer). Inventory of every SANDBOX-only site is §5.3.

| # | Decision | Why / risk closed |
| --- | --- | --- |
| LD1 | **LIVE is enabled per store, inside a LIVE deployment.** A deployment runs one payment profile (`COMMERCE_PAYMENT_PROFILE`, ingress profile = endpoint profile, deploy.md §6.1). Production = `LIVE`; SANDBOX testing lives in a separate staging deployment. In the LIVE deployment a store offers Stripe only when it has an active approval, a REAL_LIVE qualification and an enabled method (§4). PAYUNi methods stay unavailable in the LIVE deployment until PAYUNi B3 LIVE approval (no REAL_LIVE PAYUNi qualification exists; no application role can issue one, 0016). **Cutover precondition:** zero non-terminal SANDBOX Stripe attempts and refunds (runbook SQL count: `checkout.payment_attempts` with `method_code='stripe_checkout' AND environment='SANDBOX'` not in a terminal state, and `payments.stripe_refunds` with `environment='SANDBOX'` without a terminal refund fact); `payments-sandbox` is kept until drained, as deploy.md Phase B already requires. | No mixed-profile API process; reuses the existing single-profile ingress/queue routing; no stranded SANDBOX attempts. |
| LD2 | **Owner approval is a DB row, not only an env var.** New `payments.stripe_live_approvals` (§3.2) records the approving principal, when (approval time + recorded time), what (store, connection, account, currency, canary cap, per-order cap, attested checklist) and the pointer `approval_ref` to the owner's written approval. A REAL_LIVE Stripe qualification must reference an active approval by composite FK. **"Owner" is defined, not assumed:** `approved_by` must be an active member holding store grants `integration:manage` **and** `payments:refund` on the store (the 0065 store-creator set). The DB cannot prove the owner consented; consent is evidenced only by `approval_ref`, which points to the owner's written message. The approval row is operator-attested (label DESIGN until SL-LIVE01). | The env flag is deployment-wide and leaves no audit of *which store* the owner approved; `identity.memberships` has no role column (0001:28-34), so "owner" must be a grant predicate. |
| LD3 | **Live key custody (O-D):** only a **restricted** key `rk_live_…` is admitted in LIVE (`sk_live_` refused, code `stripe_live_key_unrestricted`). The owner writes the key (and later the live `whsec_`) into a file on the server; Claude runs `STRIPE_SECRET_KEY_FILE=<path> deploy/scripts/ops-admin.sh stripe-admin register --environment LIVE …` (and `STRIPE_WEBHOOK_SECRET_FILE` for `webhook`). `ops-admin.sh` reads the file itself (§5.2); the value is sealed with the existing API keyring (§0.2 `stripe-api-v1`). Claude never reads, prints, greps or copies the file contents and never runs the script with `-x`; the value never passes through chat, repo, CI, agent context, logs or URLs. After the CLI prints the registered ids, the transport file is removed (a step named in the owner's approval message). | L2/L3: RAK limits blast radius; Stripe says never share keys over chat; the `ops-admin.sh` prompt needs a TTY (`[[ -t 0 ]]`, ops-admin.sh:60), which an agent session on the server does not have. |
| LD4 | **Separate live webhook endpoint and signing secret** per store connection, registered with the existing `stripe-admin webhook --profile LIVE`; same route family `POST /v1/stripe/webhook/{endpoint_id}`, same 8 events, API version `2026-08-26.dahlia`. | L4: each endpoint and each mode has its own `whsec_`. |
| LD5 | **Staged enablement enforced in SQL:** approval → LIVE probe qualification (create+expire, no charge) → method visible with `max ≤ canary_max_minor` → owner canary (real charge + full refund) verified by SQL → method `max ≤ max_minor`. | **CANARY exposes Stripe to every buyer of the store for orders ≤ `canary_max_minor`; exposure is bounded by the cap, not by buyer identity** (`start_stripe_payment` checks only `enabled`, `visible` and `total ≤ max_amount_minor`, post_river/0012:182-184). The owner accepts this by attesting `canary_private` (§9). The per-order max above the cap opens only after one real payment, webhook and refund round-trip succeeded on that exact account/endpoint/approval. |
| LD6 | **Kill switch = stop new starts, never stop reconciliation.** Per store: `method --enabled=false` (always allowed, §3.4) or `live-revoke` (revokes the approval and its REAL_LIVE qualifications; start already rejects a revoked qualification). Platform-wide: the **existing** `COMMERCE_STRIPE_CHECKOUT_ENABLED` (cmd/api/buyer_payment.go:64) gets its own compose source `LC_STRIPE_CHECKOUT_ENABLED`, split from `LC_STRIPE_ENABLED` (worker + webhook keep running). | Stopping the worker/webhook would strand PAYMENT_PENDING stock and UNKNOWN refunds (stripe-psp D5, refund RD3/RD4). |
| LD7 | **Refunds work in LIVE for every captured LIVE payment**, independent of approval/method state. Refund still needs `payments:refund` (RD10). The per-store approval is the owner's approval for merchant-initiated LIVE refunds on that store. Agents, CI and test harnesses never call LIVE refund paths; the only agent-visible LIVE refund is the owner's canary. | A revoked store must still be able to refund money it already took; AGENTS.md forbids real refunds without owner approval. |
| LD8 | **Account readiness is read from Stripe where Stripe reports it.** `live-approve` calls `GET /v1/account` with the stored key and requires `charges_enabled`, `payouts_enabled`, `details_submitted`, empty `requirements.currently_due`, a static descriptor of 5–22 chars and a card prefix length of 0 or 2–10 (L7, L12). **A missing or null readiness field fails closed** with `stripe_live_readiness_unknown`; it is never read as 0/false→ok. Radar rules are not in the API; `decline_on.{cvc,avs}_failure` are recorded as-is, not required. Items Stripe cannot report (policy pages, dispute handling, Radar rules, canary exposure) are operator attestations listed by code. | I17: reduce self-attestation to what can be checked, and never read an absent field as a pass. |
| LD9 | **Monitoring reuses `deploy/scripts/watchdog.sh`** as check W11 (counts only). No new service, dashboard or table. | Existing alert path (`LC_ALERT_WEBHOOK_URL`, cron). |

Rejected alternatives:
- **Flip the env flag and drop the SQL CHECK.** No per-store record, no canary gate; violates I16/I17.
- **`sk_live_` accepted in LIVE.** Unrestricted key; L3 recommends RAK.
- **Owner pastes the live key into chat/MCP for an agent to register.** Key in agent context; O-D and
  AGENTS.md forbid. Replaced by the file-path route (LD3).
- **Automated LIVE tests in CI.** Real money and a live key in CI. LIVE evidence is owner-run only.
- **Canary via Stripe Dashboard refund.** Bypasses our refund path; it would open `REFUND_HISTORY` (RD9)
  and prove nothing about our LIVE refund code.
- **Mixed SANDBOX+LIVE stores in one API process.** Needs per-endpoint queue routing; not needed (LD1).
- **Stopping `payment-worker-live` as the kill switch.** Strands stock and money states (LD6).
- **A separate LIVE qualification table or approval-state column on `merchant_accounts`.** Qualifications
  already carry revocation and start already checks it; one small append-only table suffices.
- **A separate revocation table (old LR-2 option b).** Needs start to join it; the revoke-only trigger +
  policy on `account_qualifications` (§3.3) keeps start unchanged.
- **Restricting CANARY to the owner's buyer identity.** Needs a buyer allowlist in the start definer;
  the cap already bounds exposure and the owner attests the store is unannounced (`canary_private`).

## 1. Stripe facts relied on (WebFetch of docs.stripe.com / support.stripe.com, retrieved 2026-09-29)

| # | Fact | Source | Status |
| --- | --- | --- | --- |
| L1 | Go-live: "Your Stripe account can have both test and live webhook endpoints … make sure you've defined live endpoints"; rotate keys before going live; no keys in code; test objects are not usable in live mode. | [go-live checklist](https://docs.stripe.com/get-started/checklist/go-live) | VERIFIED |
| L2 | Sandbox and live each have their own keys; objects in one mode are not accessible in the other. Live keys start `rk_live_`/`sk_live_`. A live RAK you create yourself can't be revealed after you've seen it once. Access policies (IP/CIDR or ASN/country) are recommended on all live keys and replace IP restrictions. Dashboard **Rotate key** keeps old and new keys valid for up to 7 days unless Expiration = **Now** ("the old key is deleted"); **Expire key** stops the key immediately; rotation is the documented response to a compromised key. Keys must not be shared "over email, chat, or other unencrypted channels". | [API keys](https://docs.stripe.com/keys) (re-fetched 2026-09-29) | VERIFIED |
| L3 | A RAK has per-resource None/Read/Write; GET needs read, POST/DELETE need write; missing permission → invalid request error naming the permission; create and test the RAK in a sandbox first, then create the live RAK with matching permissions; one RAK per service. | [restricted keys](https://docs.stripe.com/keys/restricted-api-keys) | VERIFIED. **Exact permission names for `GET /v1/account`, Checkout Sessions, PaymentIntents (+`expand latest_charge`), Refunds: UNKNOWN** (recorded by SL08). v1 deviates from "one RAK per service" (§13). |
| L4 | Each endpoint has a unique `whsec_`; "If you use the same endpoint for both test and live API keys, the secret is different for each one". Live endpoints must be HTTPS (TLS 1.2/1.3). Live retries "for up to three days with an exponential back off"; sandbox retries three times over a few hours. Secret roll keeps both valid ≤24 h unless rolled with immediate expiry. IP allowlisting of Stripe webhook IPs is recommended. Max 16 endpoints per account. Dashboard resend ≤15 days, CLI ≤30 days. | [webhooks](https://docs.stripe.com/webhooks) | VERIFIED |
| L5 | "All transactions are screened using Radar's default rules": `:risk_level: = 'highest'` block; `'elevated'` → review by default. Review-rule matches are still processed and go to the review queue. CVC/postal-code block rules must be enabled explicitly. Stripe triggers 3DS on issuer soft declines and where regulation requires, even with Radar disabled. Radar rules are Dashboard configuration; they are not fields of the Account object. | [Radar rules](https://docs.stripe.com/radar/rules) | VERIFIED |
| L6 | Checkout Sessions: "Stripe manages 3D Secure authentication, and other required customer actions." | [Checkout Sessions API](https://docs.stripe.com/payments/checkout-sessions) | VERIFIED (SP18 3DS card PASS in SANDBOX) |
| L7 | Statement descriptor: Latin only, 5–22 chars, ≥1 letter, no `< > \ ' " *`, reflects the DBA; card prefix ("shortened descriptor") 2–10 chars; set in Dashboard; per-charge suffix via `payment_intent_data.statement_descriptor_suffix`. **If no prefix is set, Stripe uses the static descriptor as the card prefix and truncates it to 10 characters.** | [statement descriptors](https://docs.stripe.com/get-started/account/statement-descriptors) (re-fetched 2026-09-29) | VERIFIED. v1 sends no suffix (D7 exact key set). What a card statement actually shows is **UNKNOWN** until SL-LIVE01 reads the charge's `calculated_statement_descriptor` (as a length and a boolean "equals the configured prefix/descriptor" only). |
| L8 | Live use requires business verification (KYC); customers see business name, URL, support email/phone/address and descriptor; origin country cannot change after activation. | [set up your account](https://docs.stripe.com/get-started/account/activate) | VERIFIED |
| L9 | Website must show business name, goods description, customer service contact, return policy (physical goods), "Refund and dispute policy", cancellation policy if applicable; a full social-profile URL is acceptable as the website. | [website FAQ](https://support.stripe.com/questions/business-website-for-account-activation-faq), [website checklist](https://docs.stripe.com/get-started/checklist/website) | VERIFIED |
| L10 | Dispute response window "usually 7 to 21 days"; no response = automatic loss; notified by email, Dashboard and `charge.dispute.created`; amount + dispute fee debited; counter fee returned only on a win. | [respond to disputes](https://docs.stripe.com/disputes/responding) | VERIFIED |
| L11 | First payout "typically … within 7–14 days" after the first live payment; HK and SG initial settlement 7 calendar days; the bank account must support debits (negative balance). Default payout schedule for HK: **UNKNOWN**. | [payouts](https://docs.stripe.com/payouts) | PARTIAL |
| L12 | Account object fields `charges_enabled`, `payouts_enabled`, `details_submitted`, `requirements.currently_due`, `requirements.disabled_reason`, `settings.payments.statement_descriptor`, `settings.card_payments.statement_descriptor_prefix`, `settings.card_payments.decline_on.{cvc_failure,avs_failure}` (an account setting, separate from Radar rules), `settings.payouts.schedule`. | [account object](https://docs.stripe.com/api/accounts/object) | VERIFIED (field list). Whether an own (non-Connect) account read with a RAK returns `requirements` and `settings`: **UNKNOWN** (SL08); absent → `stripe_live_readiness_unknown` (LD8). |
| L13 | Live account `acct_1UJDadRzKpmj4jFL` (香港大碗貿易有限公司, livemode=true) listed by the Stripe MCP on 2026-09-25. | Humaux memory `07aba442…` (superseded note) | LOCAL, UNVERIFIED this session; `register` re-reads the account id from Stripe (VerifyAccount). |

## 2. Flow (operator runbook order; each step is a separate command and audit row)

```
owner: Stripe Dashboard live mode — activation, descriptor (+ card prefix), payout bank, Radar,
       access policy, create rk_live_ with the SL08 permission list, create live webhook endpoint
       (8 events, dahlia, URL https://<LC_HOOKS_HOST>/v1/stripe/webhook/<endpoint uuid>)
owner: put rk_live_ and whsec_ each in its own file on the server (owner-only dir, outside the repo);
       owner writes each file as the uid that will run ops-admin.sh (or chowns it to that uid),
       mode 0400/0600; send the written approval (approval_ref) naming store, currency, caps and
       "remove transport files after registration"
Claude on the server (O-D; LIVE flag+ref pair in the ops env; never reads the files;
every `stripe-admin …` line below runs as `deploy/scripts/ops-admin.sh stripe-admin …`, the only
production entry, compose.yml:33-34, §5.2):
 0 cutover precondition SQL (LD1) = 0 rows; deploy LIVE profile
 1 STRIPE_SECRET_KEY_FILE=… ops-admin.sh stripe-admin register --environment LIVE
                                               → connection (VerifyAccount, livemode); rm transport file
 2 STRIPE_WEBHOOK_SECRET_FILE=… ops-admin.sh stripe-admin webhook --profile LIVE
                                               → endpoint, whsec sealed (signing keyring); rm file
 3 ops-admin.sh stripe-admin live-approve --approval <uuid> --currency TWD ...
                                               → GET /v1/account readiness + approval row
 4 stripe-admin qualify  --profile LIVE        → probe create+expire+retrieve, REAL_LIVE qual
 5 stripe-admin method   --enabled --visible --max ≤ canary_max_minor
 6 owner: real order in the store, pays with own card (≤ canary cap), waits CONFIRMED;
          merchant admin refund (full) → SUCCEEDED; waits for the refund webhook
 7 ops-admin.sh stripe-admin live-canary --approval --attempt --refund → SQL verifies, canary_verified_at
 8 stripe-admin method   --max ≤ max_minor     → store open to buyers
kill:  ops-admin.sh stripe-admin method --enabled=false | ops-admin.sh stripe-admin live-revoke
       (both without the pair) | LC_STRIPE_CHECKOUT_ENABLED=0
```

## 3. Persistence: `migrations/0077_stripe_live_enable.sql` + post-River file (number: LR-1)

Neither file edits an applied migration. Every re-created function keeps its exact signature, owner
and grants; bodies change only as listed. Integrator re-derives each widened CHECK from
`pg_get_constraintdef` at merge (as 0061/0062 did).

### 3.1 Widened constraints and guards (0077)

| Object | Today | After |
| --- | --- | --- |
| `payments.account_qualifications` | `stripe_qualification_no_live_check`: no REAL_LIVE Stripe | add `live_approval_id uuid NULL`; replace CHECK with `code<>'stripe_checkout' OR proof_class<>'REAL_LIVE' OR live_approval_id IS NOT NULL`; `CHECK(live_approval_id IS NULL OR (code='stripe_checkout' AND proof_class='REAL_LIVE'))`; composite FK `(tenant_id,store_id,live_approval_id,connection_id)` → `stripe_live_approvals(tenant_id,store_id,id,connection_id)` |
| `payments.account_qualifications` (new trigger) | no UPDATE path (only `UPDATE(id)` for row locks, 0061:204, 0016:113) | `payments.account_qualification_revoke_only` BEFORE UPDATE FOR EACH ROW: rejects (`PT409`) any change except `OLD.revoked_at IS NULL AND NEW.revoked_at IS NOT NULL; the trigger then sets NEW.revoked_at := clock_timestamp() (the supplied value is ignored)` on rows with `code='stripe_checkout' AND live_approval_id IS NOT NULL`; `id` and every other column must be unchanged (`NEW IS NOT DISTINCT FROM OLD` apart from `revoked_at`). Applies to every role, including `commerce_checkout_writer`. |
| `payments.stripe_webhook_endpoints` | `environment='SANDBOX'`, `execution_profile IN (PROVIDER_MOCK,SANDBOX)` | `environment IN ('SANDBOX','LIVE')`, `execution_profile IN ('PROVIDER_MOCK','SANDBOX','LIVE')`, `CHECK((environment='LIVE')=(execution_profile='LIVE'))` |
| `payments.stripe_refunds` | `environment='SANDBOX'` (0062:91) | `environment IN ('SANDBOX','LIVE')` |

New helper next to `payments.stripe_amount_ok` (same closed table, IMMUTABLE, `search_path=pg_catalog`,
EXECUTE registry_writer): `payments.stripe_min_minor(text) RETURNS bigint` → HKD 400, USD 50, SGD 50,
MYR 200, TWD 2500, else NULL. SL02 asserts `stripe_amount_ok(c, stripe_min_minor(c))` and
`NOT stripe_amount_ok(c, stripe_min_minor(c)-1)` for every currency, so the two cannot drift.

SP19's existing REAL_PG fixture (REAL_LIVE Stripe qualification without approval) must still fail.

### 3.2 New table `payments.stripe_live_approvals` (0077)

```
id uuid PRIMARY KEY, tenant_id, store_id, connection_id uuid NOT NULL,
account_id text NOT NULL CHECK(account_id ~ '^acct_[A-Za-z0-9]{1,59}$'),
environment text GENERATED ALWAYS AS ('LIVE') STORED, provider text GENERATED ALWAYS AS ('stripe') STORED,
currency text NOT NULL CHECK(currency ~ '^[A-Z]{3}$'),
approved_by uuid NOT NULL,                      -- LD2 grant predicate; FK identity.memberships(tenant_id,principal_id)
approval_ref text NOT NULL CHECK(approval_ref ~ '^[A-Za-z0-9._:-]{8,128}$'),
approved_at timestamptz NOT NULL,               -- owner's stated approval time, ≤ recorded_at
recorded_at timestamptz NOT NULL DEFAULT clock_timestamp(),
canary_max_minor bigint NOT NULL CHECK(canary_max_minor>0),
max_minor bigint NOT NULL CHECK(max_minor>=canary_max_minor),
checklist text[] NOT NULL,                      -- exactly the §9 attestation codes, sorted, no duplicates
account_readiness jsonb NOT NULL,               -- exact keys: ChargesEnabled, PayoutsEnabled, DetailsSubmitted,
                                                --   CurrentlyDueCount, DescriptorLength, PrefixLength, CVCRule, AVSRule
                                                --   (bools/ints only; no nulls)
canary_attempt_id uuid, canary_refund_id uuid, canary_verified_at timestamptz,   -- set-once together
revoked_at timestamptz, revoked_by uuid, revoke_ref text CHECK(revoke_ref ~ '^[A-Za-z0-9._:-]{8,128}$'),
UNIQUE(tenant_id,store_id,id,connection_id),
FOREIGN KEY(tenant_id,approved_by) REFERENCES identity.memberships(tenant_id,principal_id),
FOREIGN KEY(tenant_id,revoked_by)  REFERENCES identity.memberships(tenant_id,principal_id),
FOREIGN KEY(tenant_id,store_id,connection_id,provider,environment,account_id)
  REFERENCES integration.merchant_accounts(tenant_id,store_id,id,provider,environment,account_id),
CHECK((canary_attempt_id IS NULL)=(canary_refund_id IS NULL) AND (canary_attempt_id IS NULL)=(canary_verified_at IS NULL)),
CHECK((revoked_at IS NULL)=(revoked_by IS NULL) AND (revoked_at IS NULL)=(revoke_ref IS NULL))
CREATE UNIQUE INDEX stripe_live_one_active ON payments.stripe_live_approvals(connection_id) WHERE revoked_at IS NULL;
```

Trigger: every column immutable except the canary triple (NULL → value once) and the revoke triple
(NULL → value once). No DELETE grant to anyone. `COMMENT ON` names owner package `payments/stripeadmin`,
roles and non-goals (no money movement, no merchant access, not proof of owner consent — see LD2).
FORCE RLS, PUBLIC revoked.

### 3.3 Roles, grants and RLS (reuse; no new role)

- `commerce_payment_registry_writer` (existing definer owner): INSERT, SELECT and
  `UPDATE(canary_attempt_id,canary_refund_id,canary_verified_at,revoked_at,revoked_by,revoke_ref)` on
  `stripe_live_approvals`; `UPDATE(revoked_at)` on `payments.account_qualifications`; SELECT on
  `checkout.payment_attempts`, `payments.facts`, `payments.stripe_refunds`, `payments.refund_facts`,
  `payments.review_cases`, `payments.stripe_webhook_receipts` (canary check only). Each with an explicit
  RLS policy `TO commerce_payment_registry_writer` scoped by `app.tenant_id`/`app.store_id` (0061
  pattern); UPDATE policies `WITH CHECK` the same scope.
- Qualification revocation path (replaces old LR-2): the existing `stripe_registry_qualification_lock`
  policy (`WITH CHECK(false)`) **stays** for every other row. New permissive policy
  `stripe_registry_qualification_revoke` FOR UPDATE TO `commerce_payment_registry_writer`
  `USING(scope AND code='stripe_checkout' AND live_approval_id IS NOT NULL) WITH CHECK(same)`. Because
  the role also holds `UPDATE(id)` (0061:204), the policy alone would allow changing `id` or clearing
  `revoked_at`; the `account_qualification_revoke_only` trigger (§3.1) is what forbids that, for
  PAYUNi rows and SANDBOX Stripe rows as well.
- `commerce_payment_registrar` (CLI login group): EXECUTE on the three new functions only.
- `commerce_integration_writer`, `commerce_checkout_writer`, ingress, worker, `commerce_runtime`: **no**
  privilege on `stripe_live_approvals`. Runtime admission never reads it; it reads the qualification.

### 3.4 SQL entry points

New (0077, registry_writer-owned, SECURITY DEFINER, `search_path=pg_catalog`, EXECUTE registrar only,
`require_stripe_registrar_scope` + store validation and one `ops.audit_events` row each):

| Function | Contract |
| --- | --- |
| `payments.approve_stripe_live(tenant,store,principal,approval uuid,connection uuid,currency text,approval_ref text,approved_at timestamptz,canary_max bigint,max bigint,checklist text[],readiness jsonb) RETURNS uuid` | `approved_by = principal`, which must hold `integration:manage` (scope check) **and** `payments:refund` on the store, else 42501. Account is `provider='stripe'`, `environment='LIVE'`, binding in scope; `approved_at ≤ now`, not > 30 days old; checklist = exact §9 code set; readiness exact keys, no JSON null, and `ChargesEnabled∧PayoutsEnabled∧DetailsSubmitted∧CurrentlyDueCount=0∧DescriptorLength∈[5,22]∧(PrefixLength=0∨PrefixLength∈[2,10])`; a missing/null key → 22023 `stripe_live_readiness_unknown`. Caps: `stripe_min_minor(currency) IS NOT NULL`, `stripe_amount_ok(currency,canary_max)`, `canary_max ≤ 2 × stripe_min_minor(currency)`, `stripe_amount_ok(currency,max)`, and (LQ3) `max ≤ 2,000,000 when currency='TWD'`; any other currency is refused (22023 `stripe_live_max_unruled`) until the owner rules its per-order max. Replay with same id and identical row → same id; otherwise PT409. Audit `stripe.live.approve`. |
| `payments.record_stripe_live_canary(tenant,store,principal,approval,attempt,refund) RETURNS timestamptz` | Approval active (FOR UPDATE), canary unset. Attempt: `method_code='stripe_checkout'`, `environment='LIVE'`, same connection, `currency = approval.currency`, `created_at > recorded_at`, and `attempt.qualification_id` references a qualification with `live_approval_id = approval`; CAPTURED fact with `amount_minor ≤ canary_max_minor`; the refund belongs to that attempt, `stripe_refunds.environment='LIVE'`, with a SUCCEEDED refund fact and Σ SUCCEEDED = captured (full refund); no `review_cases` row on the attempt; ≥1 ACCEPTED `stripe_webhook_receipts` row for the attempt **and** ≥1 ACCEPTED receipt with `refund_id = refund`, both on a LIVE endpoint of this connection. Sets the triple; identical replay returns the stored time; other attempt/refund → PT409. Audit `stripe.live.canary`. |
| `payments.revoke_stripe_live(tenant,store,principal,approval,revoke_ref) RETURNS timestamptz` | Takes the approval `FOR UPDATE` first; then, in a **separate statement** (READ COMMITTED sees qualifications committed while it waited), sets `revoked_at=now` on every non-revoked qualification with `live_approval_id=approval`, and sets the revoke triple, in one tx. Replay with identical `revoke_ref` and principal → stored `revoked_at`; different ref or principal on an already-revoked approval → PT409. Any principal passing the registrar scope may revoke (kill switch must not need the owner). Audit `stripe.live.revoke`. |

**Lock order (all LIVE registrar paths):** account → binding → approval → qualification → method head.
`qualify_stripe_method(LIVE)` and `set_stripe_method(LIVE, enabled)` take the approval `FOR SHARE` and
recheck `revoked_at IS NULL` under that lock; revoke's `FOR UPDATE` therefore waits for them, and a
qualification inserted concurrently is either seen and revoked, or refused. Start never reads
approvals; it relies on the qualification's `revoked_at`.

Re-created 0061/0062 registrar/definer functions (0077; same signatures and owners; body deltas only):
- `integration.register_stripe_account`, `integration.rotate_stripe_key`, `payments.stripe_endpoint_account`,
  `payments.stripe_registrar_credential`: `environment IN ('SANDBOX','LIVE')` (LIVE rows untouched by
  SANDBOX callers: environment comes from the registered account, never a second input).
- `payments.set_stripe_webhook_endpoint`: profile LIVE only for a LIVE account; SANDBOX/PROVIDER_MOCK only
  for a SANDBOX account.
- `payments.qualify_stripe_method`: gains **no** new parameter; requires
  `(acct.environment='LIVE') = (p_profile='LIVE')` (PROVIDER_MOCK and SANDBOX profiles only on SANDBOX
  accounts; the 0016:13 CHECK alone would allow PROVIDER_MOCK on LIVE); profile `LIVE` requires an active
  approval for the connection (FOR SHARE, above) and `evidence_ref ~ '^stripe-probe:cs_live_[A-Za-z0-9_]{1,245}$'`;
  inserts `environment=acct.environment`, `proof_class='REAL_LIVE'`, `live_approval_id` = that approval,
  expiry ≤ 30 days (same as SANDBOX).
- `payments.set_stripe_method` (0061:1570-1630):
  - **`p_enabled=false`** skips the qualification validity, approval and cap checks; it still requires
    registrar scope, the market, the account/binding and the CAS on `method_heads`, and stores the
    current `p_qualification` (which must still belong to the connection; `method_admission_reference`
    only binds enabled rows). So disable works after `live-revoke`, qualification expiry and key rotate.
  - `q.proof_class='REAL_LIVE'` is removed from the rejection list; `q.environment=acct.environment`
    replaces `q.environment='SANDBOX'`; the inserted row uses `acct.environment` instead of the literal
    `'SANDBOX'`.
  - For a LIVE account with `p_enabled=true`: `q.proof_class='REAL_LIVE'`, approval active (FOR SHARE),
    `market.currency = approval.currency`, `p_max ≤ canary_max_minor` while `canary_verified_at IS NULL`,
    else `p_max ≤ max_minor`.
- `payments.apply_stripe_refund` (0062:941, owner `commerce_checkout_writer`, kept): `Livemode` must equal
  `to_jsonb(a.environment='LIVE')`, replacing `Livemode IS DISTINCT FROM 'false'` (0062:981).
- `checkout.hosted_payment_view_v2` (0062 body): the `p_profile IN ('PROVIDER_MOCK','SANDBOX')` Stripe
  branch admits `LIVE` with `acct.environment=a.environment` and `proof_class='REAL_'||environment`.

Re-created post-River functions (LR-1; same signatures and owners; body deltas only):
- `checkout.start_stripe_payment`: profile `LIVE` admitted; `a.environment=m.environment=<profile env>`;
  `q.proof_class = CASE PROVIDER_MOCK → PROVIDER_MOCK, SANDBOX → REAL_SANDBOX, LIVE → REAL_LIVE`. The
  existing `q.revoked_at IS NOT NULL` → PT409 check is the kill-switch hook (unchanged text).
- `checkout.request_stripe_signal`: profile LIVE admitted.
- `integration.record_stripe_refund_observation`, `integration.record_stripe_charge_observation`
  (post_river/0013:427, 533): `Livemode` must equal `to_jsonb(attempt.environment='LIVE')` (the rule
  0012:461 already uses for checkout observations), replacing `Livemode<>'false'`.
- `payments.request_stripe_refund`: `a.environment IN ('SANDBOX','LIVE')` (0013:673); **no** dependency on
  approval, qualification or method state (LD7).

## 4. Per-store enablement state (derived, never stored as a status)

| State | Derived from | New starts |
| --- | --- | --- |
| NONE | no LIVE Stripe account | none |
| REGISTERED | LIVE account + credential | none (no qualification) |
| APPROVED | active approval | none |
| QUALIFIED | REAL_LIVE qualification, not revoked/expired, `live_approval_id` active | none until method |
| CANARY | method enabled+visible, `max ≤ canary_max_minor` | yes, **any buyer** of the store, ≤ canary cap (LD5, `canary_private`) |
| OPEN | `canary_verified_at` set, method max raised | yes, ≤ `max_minor` |
| PAUSED | method disabled (allowed from every state, incl. after revoke/expiry/rotate) | none; in-flight reconcile, refunds allowed |
| REVOKED | approval revoked (qualifications revoked) | none; in-flight reconcile, refunds allowed; re-enable = new approval + qualify + canary |

Key rotation (`rotate`) keeps the approval; existing rule applies (new starts blocked until re-qualified).
Qualification expiry (30 days) needs a re-probe (`qualify --profile LIVE`, no charge), not a new canary.

## 5. Config, CLI, deploy and Go deltas

### 5.1 Config (§5.2 amended)

| Key | SANDBOX | LIVE |
| --- | --- | --- |
| `sk_test_`/`rk_test_` | allowed | refused `stripe_key_mode_mismatch` |
| `rk_live_` | refused | allowed only with `COMMERCE_STRIPE_LIVE_ENABLED=1` and valid `COMMERCE_STRIPE_LIVE_APPROVAL_REF`, else `stripe_live_refused` |
| `sk_live_` | refused | **refused** `stripe_live_key_unrestricted` (new; LR-3) |

Mock transport never admits a live key (unchanged). `ProbeCheckout` in LIVE is allowed only on a
LIVE-admitted client and requires `expired`+`unpaid`+`livemode=true` (probe.go:46 and :76 change).

### 5.2 Processes and deploy

- `cmd/payment-worker`: `COMMERCE_STRIPE_ENABLED=1` with profile `LIVE` requires the flag+ref pair
  (both or neither, as §12); `NewStripeRuntime` admits `LIVE` without mock transport;
  `validStripeSnapshot` requires `Environment` = profile's environment; `stripeSessionIdentity` requires
  `session.Livemode == (s.Environment=="LIVE")`; refund loader (`stripe_refund.go`) same.
- `cmd/api`: webhook ingress admits profile `LIVE` with the flag+ref pair; `stripewebhook` handler/inbox
  admit `LIVE`; `checkout/hosted.go` Stripe branch admits `LIVE`; `buyer_payment.go:65` admits
  `COMMERCE_STRIPE_CHECKOUT_ENABLED=1` on profile `LIVE` only with the pair. `cmd/api` still never reads
  `STRIPE_SECRET_KEY` (SP15). The merchant refund path (`internal/merchantorders/refunds.go`, before
  `payments.request_stripe_refund`, which has no profile input) refuses any attempt whose environment
  differs from the deployment profile's environment with the existing `not_refundable` error
  (`ErrNotRefundable`, httpapi/refunds.go:142), so a pre-cutover SANDBOX capture can never create a
  refund the LIVE worker refuses (`validStripeSnapshot`) and W11 never sees. The refresh route
  (`POST …/refunds/{refund_id}/refresh`) applies the same guard before its River insert
  (`merchantorders.RefreshRefundIn`, amendment 2026-09-30b).
- `internal/integrations/accounts/stripe_crypto.go`: `validStripeAPIScope` and `validStripeWebhookScope`
  admit `Environment IN (SANDBOX, LIVE)`; webhook scope requires `(Environment=="LIVE") == (Profile=="LIVE")`.
- `deploy/compose.yml`: the **existing** `COMMERCE_STRIPE_CHECKOUT_ENABLED` gets the separate source
  `${LC_STRIPE_CHECKOUT_ENABLED:-${LC_STRIPE_ENABLED:-0}}`; compose wires `COMMERCE_STRIPE_ENABLED`,
  `COMMERCE_STRIPE_LIVE_ENABLED` and `COMMERCE_STRIPE_LIVE_APPROVAL_REF` into `api` and
  `payment-worker-live` (today payment-worker-live never receives them). `LC_STRIPE_ENABLED=1` +
  `LC_STRIPE_CHECKOUT_ENABLED=0` keeps webhook and worker running.
- `deploy/scripts/preflight.sh` P06 (lines 336-338): `LC_STRIPE_ENABLED` requires either
  profile=SANDBOX + `payments-sandbox`, or profile=LIVE + `payments-live` + the flag+ref pair.
- `deploy/scripts/ops-admin.sh`: admits `--profile LIVE`/`--environment LIVE` only when the flag+ref pair
  is set (today refused unconditionally, lines 46-50); key regex admits `rk_live_` only in that case,
  never `sk_live_`. **O-D file input:** `need NAME secret …` first honours `NAME_FILE`: absolute path,
  regular file, not a symlink, owned by the invoking uid, mode 0400 or 0600, one line; read with
  `IFS= read -r v <"$file"`; the value is never echoed and every error names the variable only.
  The TTY prompt stays for humans.
  **Subcommand allowlist (ops-admin.sh:41, and the `usage()`/header text at :7,:33)** adds
  `stripe-admin:live-approve | stripe-admin:live-canary | stripe-admin:live-revoke`; any other
  subcommand still goes to `usage()`. None of the three takes a secret input, so none gets a `need`
  arm (ops-admin.sh:77-100): `live-approve` unseals the **stored** `rk_live_` with the API keyring that
  compose already mounts into the stripe-admin container (compose.yml:426-428; same path as SANDBOX
  `qualify`, cmd/stripe-admin/main.go:162-174), so ops-admin forwards no key and no keyring.
  `live-approve` and `live-canary` require the flag+ref pair (ops-admin refuses first, the CLI refuses
  in code); ops-admin forwards `COMMERCE_STRIPE_LIVE_ENABLED` and `COMMERCE_STRIPE_LIVE_APPROVAL_REF` by
  NAME (`-e NAME`, like `need`) to every stripe-admin run when set, because compose.yml:423-431 does not
  wire them into stripe-admin. **Host source of the pair:** compose.env keys `LC_STRIPE_LIVE_ENABLED` and
  `LC_STRIPE_LIVE_APPROVAL_REF` (loaded by `lc_load_env`, ops-admin.sh:54); ops-admin maps them to
  `COMMERCE_STRIPE_LIVE_ENABLED`/`COMMERCE_STRIPE_LIVE_APPROVAL_REF` before forwarding, and compose wires
  the same keys into `api` and `payment-worker-live` (`${LC_STRIPE_LIVE_ENABLED:-0}`,
  `${LC_STRIPE_LIVE_APPROVAL_REF:-}`); preflight P06 reads the pair from those keys. **`live-revoke` and `method --enabled=false` are admitted without the
  pair** (ops-admin and CLI), so the per-store kill switch never depends on the deploy env.
- `cmd/stripe-admin`: `register --environment SANDBOX|LIVE` (default SANDBOX, existing behavior);
  LIVE requires the flag+ref pair in the ops env and an `rk_live_` key (main.go:163 and
  `stripeadmin/registrar.go:39,327,377,387` change). New subcommands `live-approve` (`--currency`),
  `live-canary`, `live-revoke` (flags as §3.4; `--attest` comma list of §9 codes). `live-approve` reads
  readiness via `GET /v1/account` with the **stored** key (S4 pattern) and emits only ids/booleans.
  Output stays one JSON line of ids and versions.
- `stripe.Client.VerifyAccount` gains an optional readiness projection (the L12 booleans, due count,
  descriptor and prefix lengths, decline_on booleans), each field tri-state (present/absent) so absence
  maps to `stripe_live_readiness_unknown`; no other field is parsed or stored (no PII: business profile,
  email and addresses are ignored).

### 5.3 SANDBOX-only sites that change (inventory, 2026-09-29, base `ac3e3a9`, re-grepped after review)

SQL 0061: lines 71, 349/351, 1374, 1410, 1437/1444, 1485, 1510, 1534/1544/1551/1554, 1592/1603/1620.
SQL 0062: 91, 981, 1211–1236. post_river 0012: 149, 189, 200, 281. post_river 0013: 427, 533, 673.
Go: `stripe/config.go` admit, `stripe/probe.go:46,76`, `accounts/stripe_crypto.go:83,90-91`,
`payments/stripe_runtime.go:48,106`, `payments/stripe_query.go:236`, `payments/stripe_refund.go:116,122`,
`stripewebhook/handler.go:46`, `stripewebhook/inbox.go:64`, `checkout/hosted.go:84`,
`cmd/payment-worker/main.go:69`, `cmd/api/stripe_webhook.go:71`, `cmd/api/buyer_payment.go:65`,
`cmd/stripe-admin/main.go:163`, `stripeadmin/registrar.go:39,327,377,387`.
Already LIVE-aware (no change): `stripe/client.go:250-258` `profileAllowed`, `stripe/params.go:75`,
0062:345/364/718/741, post_river 0012:461/553, 0013:349/504.
Deploy: `deploy/scripts/preflight.sh:336-338` (P06), `deploy/scripts/ops-admin.sh:7,33,41,46-50,78-79,91,106` (+ `_FILE`, allowlist, pair forwarding),
`deploy/compose.yml:176-180` (api) and `:328-330` + payment-worker-live block, `deploy/README.md` rule 20
("One Stripe switch" → two switches), `deploy/env/api.env.example:33`, `docs/runbooks/deploy.md` §5/§6.1
and Phase B ("Stripe 在 LIVE 下被代码拒绝"), `merchant-onboarding.md` O4.

### 5.4 HTTP

No new route. Live endpoint uses `POST /v1/stripe/webhook/{endpoint_id}`; on the LIVE ingress a SANDBOX
endpoint id returns the existing fixed 404 and vice versa; a verified event with `livemode=false` on a
LIVE endpoint is the existing quarantine `livemode_mismatch`. Buyer and merchant routes unchanged.

## 6. Idempotency

| Scope | Key | Rule |
| --- | --- | --- |
| Approval | operator-minted `--approval` UUID | identical replay → same id; different params → PT409; one active per connection |
| LIVE probe | `lc:stripe:probe:v1:<qualification>` (existing) | a failed probe mints a new qualification id (existing S4 rule) |
| Canary record | approval id (set-once triple) | identical replay → stored time; other attempt/refund → PT409 |
| Revoke | approval id (set-once triple) | identical `revoke_ref` + principal → stored time; different ref or principal on a revoked approval → PT409 |
| Charges/refunds | unchanged stripe-psp §11 / refund RD5 | unchanged |

## 7. Kill switch

| Scope | Command | Effect | Not affected |
| --- | --- | --- | --- |
| One store, reversible | `ops-admin.sh stripe-admin method --enabled=false` (no pair needed) | next `start_stripe_payment` PT409 (method), UI shows unavailable; succeeds in every state incl. after revoke/expiry/rotate | handoff of open sessions until cutoff (D16), worker, webhook, refunds |
| One store, hard | `ops-admin.sh stripe-admin live-revoke` (no pair needed, §5.2) | qualifications revoked → start PT409; re-enable needs new approval + qualify + canary | same |
| Platform | `LC_STRIPE_CHECKOUT_ENABLED=0` + `deploy.sh` (api restart) | Stripe removed from the hosted service for all stores | worker, webhook, refunds keep running |
| API key compromise | Stripe Dashboard → API keys → the RAK → **Expire key** (or **Rotate key** with Expiration = **Now**); never use the 7-day grace period for a compromise (L2); then owner writes the new `rk_live_` to a file and Claude runs `STRIPE_SECRET_KEY_FILE=<path> STRIPE_ACCOUNT_ID=<acct_…> ops-admin.sh stripe-admin rotate --environment LIVE --tenant <t> --store <s> --principal <p> --connection <c> --expected-version <n>` (without `--environment LIVE` the CLI defaults to SANDBOX and refuses the `rk_live_` key; ops-admin now says so before any container starts) | all Stripe I/O for that account fails auth → attempts stay UNKNOWN (stock kept) until the new key is registered and re-qualified | — |
| Webhook secret compromise | Dashboard → the live endpoint → **Roll secret** with immediate expiry; then `STRIPE_WEBHOOK_SECRET_FILE=… ops-admin.sh stripe-admin webhook --profile LIVE` | events signed with the old secret are rejected; Stripe retries for up to 3 days (L4) | worker polling |

Open sessions are not force-expired (they close at `expires_at`, ≤40 min, D4). A forced expire-all is
out of scope (§13).

## 8. Monitoring (watchdog W11, counts only, `environment='LIVE'`)

| Signal | Threshold (default) | Source |
| --- | --- | --- |
| Stripe `integration.operations` in `UNKNOWN` older than 60 min, **excluding settled ones** (`result_code` `stripe_terminal_observed` / `stripe_refund_terminal`, see amendment 2026-09-30b) | > 0 | operations (`state`, `created_at`, `result_code`) |
| `payments.review_cases` on LIVE attempts created in the last 24 h | > 0 | review_cases |
| `payment_work_items` in `REVIEW_REQUIRED` (incl. `CLOSURE_CONTRADICTED`) | > 0 | work items |
| LIVE refunds without a terminal fact older than 24 h, or review `REFUND_UNRESOLVED`/`REFUND_HISTORY` | > 0 | stripe_refunds, refund_facts, review_cases |
| LIVE webhook receipts quarantined/ignored in the last hour | > 5 | stripe_webhook_receipts |
| LIVE endpoint disabled while an OPEN/CANARY method exists | > 0 | endpoints, method heads |

W11 prints only the failing check id and counts; POSTs ids to `LC_ALERT_WEBHOOK_URL` like W1–W10.
Stripe-side queues are **not** visible to us: Radar review queue, disputes and payout failures are
watched in the Stripe Dashboard/email by the owner (§9 D-codes). A dispute on a captured payment
surfaces in our system only when a refund pre-send check sees `Disputed=true` (refund RD9 →
`CONFLICTING_REPORT`).

## 9. Go-live checklist (codes stored in `checklist`; `live-approve` refuses a missing code)

| Code | Item | Verified by |
| --- | --- | --- |
| `account_active` | Business verified; `charges_enabled`, `payouts_enabled`, `details_submitted`, no `currently_due` (L8, L12) | API (LD8); absent field → `stripe_live_readiness_unknown` |
| `descriptor` | Static descriptor 5–22 Latin chars, reflects DBA; card prefix 2–10 chars set, or owner accepts the static descriptor truncated to 10 chars on card statements (L7) | API lengths + owner attests text; card text UNKNOWN until SL-LIVE01 |
| `payout_bank` | Payout bank account added, supports debits; first payout 7–14 days expected (L11) | API `payouts_enabled` + attest |
| `rak_live` | `rk_live_` created with exactly the SL08 permission list, access policy set to the server egress IPs (L2, L3) | attest (Dashboard only) |
| `webhook_live` | Live endpoint: URL with endpoint uuid, `2026-08-26.dahlia`, the 8 events, secret registered via `webhook --profile LIVE` (L1, L4) | SQL (endpoint row) + canary receipts |
| `radar_default` | Radar default rules on; no allow rules; CVC rule on (ruling LQ4) (L5) | attest only (Radar rules are not in the API); `CVCRule`/`AVSRule` record `decline_on` as-is |
| `three_ds` | Nothing to configure: Checkout handles 3DS (L6); SP18 3DS SANDBOX PASS | attest |
| `dispute_notice` | Owner receives dispute emails; knows 7–21 day window and fees; disputes are handled in Stripe Dashboard (L10) | attest |
| `policy_pages` | Public page (storefront or business social profile) with contact, refund & dispute, return, shipping, cancellation policies, prices with currency (L9) | attest; URL recorded in the owner's approval message referenced by `approval_ref` (not stored in DB) |
| `managed_off` | Managed Payments / Adaptive Pricing stay off per session (D7) | SP16-style create body (unchanged) |
| `canary_private` | Store not yet announced; no public links shared; owner accepts that real buyers may pay ≤ canary cap during CANARY (LD5) | attest |

## 10. Canary (LIVE, owner-run)

1. Store in CANARY state (method `max ≤ canary_max_minor`, default 2 × currency minimum, e.g. TWD 5000
   minor = NT$50, HKD 800 = HK$8), unannounced (`canary_private`).
2. Owner, as a buyer with his own card, orders one item priced ≤ cap through the real storefront; pays
   on the Stripe hosted page; waits for order CONFIRMED (poller or webhook).
3. Owner, as merchant with `payments:refund`, refunds the full amount in merchant admin; waits for
   SUCCEEDED and the refund webhook receipt.
4. Claude runs `live-canary`; SQL verifies §3.4. Evidence file
   `output/stripe-live/<store>/canary.txt`: approval id, attempt id, refund id, verified time, commit
   SHA, `calculated_statement_descriptor` length + match boolean — no card data, no email, no session URL.
5. Failure at any step: `method --enabled=false`, keep the store in CANARY, diagnose; two targeted
   fixes max, then escalate (AGENTS.md). The Stripe fee on the canary charge is not refunded by Stripe
   (UNKNOWN amount; owner accepts).

## 11. Test gates (tiers as stripe-psp §14; LIVE = owner-run, never CI)

| Gate | Test | Tier | Required |
| --- | --- | --- | --- |
| SL01 | `TestStripeSL01LiveConfig` | UNIT | §5.1 matrix both directions; `sk_live_` in LIVE refused; flag without ref, ref without flag, test key with flag, live key on mock/SANDBOX refused; LIVE probe requires live client and `livemode=true`; `validStripeAPIScope`/`validStripeWebhookScope` LIVE rules; readiness projection parses only listed fields, reports absence (not false/0), and redacts under `%v %+v %#v json` |
| SL02 | `TestStripeSL02Schema` | REAL_PG | 0077 on fresh and populated-latest DB; repeat/checksum; SP19 REAL_LIVE-without-approval insert still rejected; approval set-once/immutable triggers; one active approval per connection; FKs (approval→LIVE account; approved_by/revoked_by→memberships; qualification→approval same connection); `stripe_min_minor` ↔ `stripe_amount_ok` agreement per currency; **direct UPDATE of qualification `id`, `revoked_at`→NULL, or any other column rejected by trigger (Stripe LIVE, SANDBOX and PAYUNi rows)**; endpoint env/profile CHECK; refund env widened; FORCE RLS, column-privilege matrix from `information_schema`, definer owner/`proconfig`/ACL, PUBLIC revoked; no runtime/worker/ingress privilege on approvals |
| SL03 | `TestStripeSL03Registrar` | REAL_PG | approve: principal without `payments:refund`, SANDBOX account, missing checklist code (incl. `canary_private`), readiness false, readiness key missing/null (`stripe_live_readiness_unknown`), `approved_at` future/old, cap > 2×`stripe_min_minor`, cap/max not `stripe_amount_ok`, TWD `max` = 2,000,001 minor (LQ3), non-TWD currency (`stripe_live_max_unruled`), unknown currency all rejected; replay idempotent; LIVE qualify without approval rejected; PROVIDER_MOCK/SANDBOX qualify on a LIVE account rejected; LIVE method with a non-REAL_LIVE qualification rejected; method in another currency than the approval rejected; method max caps before/after canary; canary negatives (SANDBOX attempt, other connection, other currency, attempt before approval, attempt under another approval's qualification, no CAPTURED, amount > cap, partial refund, refund not SUCCEEDED, SANDBOX refund, any review case, no LIVE attempt receipt, no LIVE refund receipt) each rejected; revoke → next start PT409, in-flight attempt still reconciles to CAPTURED/CLOSED_UNPAID, refund on a revoked store still accepted; revoke replay same ref → stored time, other ref/principal → PT409; **concurrent qualify + revoke (two sessions) leaves no active REAL_LIVE qualification**; `enabled=false` succeeds in every state, explicitly after revoke, after expiry and after rotate |
| SL04 | `TestStripeSL04LiveShape` | UNIT + REAL_PG | Checkout/refund/charge observation and apply definers accept `Livemode=true` exactly for LIVE attempts and reject each mismatch; Go `stripeSessionIdentity`/snapshot/refund loader env rules; merchant refund on a SANDBOX attempt in a LIVE-profile API (and the reverse) → `not_refundable`, no refund row. (No end-to-end MOCK LIVE run: the mock transport never admits a live key by design.) |
| SL05 | `TestStripeSL05LiveIngress` | HTTP_PG | LIVE ingress: LIVE endpoint admitted; SANDBOX endpoint 404; `livemode=false` event quarantined; SANDBOX ingress rejects LIVE endpoint; no secret/body/signature in logs (SP13 sentinels) |
| SL06 | `TestStripeSL06LiveProcess` | process + PG + shell | worker/api LIVE refuse start without the pair; `LC_STRIPE_CHECKOUT_ENABLED=0` removes Stripe from the hosted service while a MOCK in-flight attempt still closes via worker+webhook; API never reads `STRIPE_SECRET_KEY`; **preflight P06 both directions** (SANDBOX+payments-sandbox pass, LIVE+payments-live+pair pass, LIVE without pair fail, mixed fail); **ops-admin `_FILE`**: symlink, group/world-readable, wrong owner, multi-line and `sk_live_` files refused; LIVE refused without the pair; the sentinel value never appears in stdout/stderr; **ops-admin allowlist**: `live-approve`/`live-canary`/`live-revoke` admitted, `live-approve`/`live-canary` refused without the pair, `live-revoke` and `method --enabled=false` admitted without the pair, the pair reaches the one-shot container (stub `docker compose` records `-e` names only), `LC_STRIPE_LIVE_ENABLED`/`LC_STRIPE_LIVE_APPROVAL_REF` in compose.env are mapped to `COMMERCE_STRIPE_LIVE_*` before forwarding, an unknown subcommand still refused by `usage()` |
| SL07 | regression | MOCK + REAL_PG + SANDBOX + BROWSER | SP01–SP21 and RF01–RF12 unchanged and green; SANDBOX SP16/RF10/SP21 and SP18 re-run green (functions re-created); SP19 still proves refusal without approval at config, CLI and SQL |
| SL08 | `TestStripeSL08RestrictedKey` | SANDBOX | With an `rk_test_` key holding only the candidate set (Checkout Sessions write, PaymentIntents read, Charges read, Refunds write, account read — names UNKNOWN until run): VerifyAccount+readiness (records which readiness fields a RAK receives), SP16 create/retrieve/expire/list, RF10 refund create/retrieve/list, PI retrieve with `latest_charge`. Records the exact least permission names, and whether account read is needed by any runtime (worker/refund) path, in `output/stripe-live/rak-permissions.txt`; a 403 adds one permission and reruns; SKIP = NOT_RUN |
| SL09 | `watchdog W11` | REAL_PG + shell | each §8 signal red on a seeded fixture, green when clear; output has counts/ids only |
| SL-LIVE01 | owner canary §10 | **LIVE** | `live-canary` returns a time; evidence file; NOT_RUN until the owner performs it |
| SL-LIVE02 | owner kill drill | **LIVE** | after SL-LIVE01: `method --enabled=false` → storefront shows Stripe unavailable for that store; re-enable; NOT_RUN until owner |

Commands: existing `bash scripts/dev/test-local.sh --stripe` / `--stripe-sandbox` / `--stripe-browser`
gain `-run '^TestStripe(SP|SL)'`; `--stripe-sandbox` fails when SL08 SKIPs in that mode. The harness
refuses to start if any `*_live_*` key is present in the test environment (new guard; CI step "No
key-shaped secret literals" unchanged).

## 12. Ownership (PROCESS §2)

| Artifact | Owner |
| --- | --- |
| `migrations/0077_stripe_live_enable.sql`, post-River file (LR-1), `cmd/stripe-admin`, `cmd/api`/`cmd/payment-worker` config, compose/preflight/`ops-admin.sh`/`watchdog.sh` W11, runbooks | integrator |
| `internal/integrations/psp/stripe/**` (config, probe, readiness projection, SL01), `internal/integrations/accounts/stripe_crypto.go` | integration_worker |
| `internal/payments/stripe_*.go`, `internal/payments/stripewebhook/**`, `internal/payments/stripeadmin/**`, `internal/checkout/hosted.go` | commerce_worker |
| `tests/foundation/stripe_live_*_test.go` (SL02–SL06, SL08, SL09) | independent test_worker |
| review of the unit diff (money/secrets) | security_reviewer (≠ author) |
| SL-LIVE01/02 purchase and refund, §9 attestations, Dashboard configuration, writing secret files, written approval | **owner** |
| running `ops-admin.sh` / `stripe-admin` on the production server with owner-supplied secret files (O-D) | Claude (operator), never reading the files |

## 13. Known limits / NOT_RUN

- Evidence: DESIGN; SL01–SL09 NOT_RUN; SL-LIVE01/02 NOT_RUN (owner). LIVE end-to-end is proven only by
  the owner canary; there is no automated LIVE test and no MOCK run with live-mode keys.
- **Capability label (I17):** after SL-LIVE01, Stripe LIVE is labelled `LIVE (owner canary)` for that
  store only; it is not `production_supported` until a non-developer merchant completes a LIVE order and
  refund.
- CANARY is open to any buyer of the store up to the cap (LD5); only the owner's attestation keeps the
  store unannounced.
- One RAK shared by worker and registrar: deviation from L3's one-per-service. Upgrade: a separate
  read-only registrar RAK once SL08 shows account read is needed only by `live-approve`.
- The approval row proves the operator recorded an approval under a principal with the owner grant set;
  owner consent itself lives in the message `approval_ref` points to (LD2).
- One Stripe account per store, direct charges; a store run by another merchant needs that merchant's
  own account (their `rk_live_`) or `stripe-connect-v1` + ADR (LQ1).
- No production host yet (owner is buying one); SL-LIVE01 needs the deploy + `xgdwm.com` hooks host
  with valid TLS (L4) first.
- No policy pages exist in `apps/storefront` today (only product/claim/payment routes); `policy_pages`
  must be satisfied by a separate UI unit or by the owner's social profile (LQ7).
- Disputes, Radar reviews, early fraud warnings and payout failures are not ingested; the owner handles
  them in Stripe. `charge.dispute.created` is not subscribed (event allowlist unchanged).
- Least RAK permission names and RAK visibility of readiness fields UNKNOWN until SL08; access-policy IPs
  depend on the server.
- TWD live minimum is only SANDBOX-observed (2500); the canary uses the store currency and may reveal a
  different live minimum (then SP02 + `stripe_min_minor` table amendment).
- Card statement text UNKNOWN until SL-LIVE01 (L7); HK default payout schedule and fees UNKNOWN (L11);
  the canary's Stripe fee is not refunded.
- Open sessions are not force-expired on kill; they close within ≤40 min (D4).

## 14. Owner questions (genuine owner inputs; each has a default)

- **LQ1 (blocking).** Which store goes live first, and is the live Stripe account holder (L13,
  香港大碗貿易有限公司) that store's merchant of record? *Default:* one store, your own shop, sold under
  that company; other merchants later with their own Stripe accounts.
- **LQ6.** Statement descriptor text (5–22 Latin chars) and card prefix (2–10 chars). *Default:* your
  DBA in Latin letters as the descriptor, e.g. the brand on `xgdwm.com`, and the same brand ≤10 chars as
  the card prefix; supply the exact text.
- **LQ7 (blocking for `policy_pages`).** Who owns the refund/return/shipping/contact policy content and
  where does it live? *Default:* you supply the text; one static policies page on `xgdwm.com` built as a
  small UI unit, linked from the storefront footer and set as the Stripe business website.

## 15. Integrator rulings (2026-09-29)

Owner decision applied: **O-D** — production is operated by Claude on the owner's server; the owner
supplies secrets as files; secrets never go through chat (LD3, §2, §5.2 `_FILE`, §7, §12).

Defaults accepted by the integrator (owner may revise before go-live):
- **LQ2** Secrets are entered by Claude on the server from owner-supplied files (O-D), never pasted in
  chat; Claude passes paths, never reads contents; transport files removed after registration.
- **LQ3** Canary cap = 2 × currency minimum (`stripe_min_minor`: NT$50 / HK$8); per-order max NT$20,000
  (TWD 2,000,000 minor) for the first 30 days, enforced by `approve_stripe_live` (§3.4, SL03); raising it
  later needs a contract amendment of that constant plus a new approval.
- **LQ4** Radar CVC rule on; no allow rules; default elevated-risk review handled in Stripe.
- **LQ5** Production runs LIVE only; SANDBOX lives in a separate staging deployment (LD1).
- **LQ8** `approval_ref` = `owner-chat:<YYYY-MM-DD>:stripe-live:<store-slug>`, pointing to the owner's
  written "approve LIVE for store X, currency C, caps Y/Z" message, recorded in Humaux.

Round-1 review resolution (`stripe-live`): all 8 P1 and 9 P2 accepted and fixed in place (LD1, LD2,
LD3, LD5–LD8, §1 L2/L7/L12, §3.1–§3.4, §4, §5.2–§5.3, §6, §7, §9, §11, §13). One partial rebuttal: for
the approval currency check the reviewer offered `NOT stripe_amount_ok(currency, canary_max/2 - 1)`; that
test is vacuous for TWD because `stripe_amount_ok` also requires `p_minor%100=0` (0061:12), so 2499 is
always "not ok". The reviewer's alternative, a `payments.stripe_min_minor(text)` helper with the same
closed table, is adopted instead (§3.1).

Round-2 review resolution (`stripe-live`, 1 P1): accepted. `ops-admin.sh:41` allowlist gains
`live-approve`/`live-canary`/`live-revoke` (§2, §5.2, §5.3, §7, SL06). One correction to the suggested
text: ops-admin forwards **no** API keyring for `live-approve`; compose.yml:426-428 already mounts it
into every stripe-admin run (the same way SANDBOX `qualify` gets it). Added the gap the fix exposed:
the flag+ref pair is not wired into the stripe-admin container (compose.yml:423-431), so ops-admin
forwards it by name; `live-revoke` and `method --enabled=false` never require it.

Round-3 (`output/contract-review/r2-round3.json` key `stripe-live`, PASS_WITH_P2, no P1): per
`docs/delivery/units/r2-design-rulings.md` Round-3 rulings X1/X6, all five P2s applied in place (§3.1
revoke trigger sets `clock_timestamp()` itself; §3.4 LQ3 max enforced; §2 transport-file uid; §5.2
refund environment guard; §5.2 `LC_STRIPE_LIVE_*` → `COMMERCE_STRIPE_LIVE_*` mapping; SL03/SL04/SL06);
contract frozen as v1 2026-09-30. Reviewer's "existing fixed 409" for the refund guard corrected: the
existing refusal is PT422 `not_refundable` (post_river/0013:673-676 → refunds.go:255).

Rulings still needed from the integrator (not owner inputs):
- **LR-1** Post-River migration number for the re-created start/signal/observation/refund functions.
  *Default:* `post_river/0016_stripe_live.sql` (0015 is the default proposed by `meta-ads-v1` A-5).
- **LR-3** Accept `sk_live_` refusal in LIVE (§5.1) as a Stage-A parser change (SP01 vectors change).
  *Default:* accept.
- (Old LR-2 is resolved: revoke-only trigger + scoped policy on `account_qualifications`, §3.1/§3.3.)

Amendment 2026-09-30b (r2 close-out of lane `stripe-live`, integrator-side; no contract semantics widened):
- §8 row 1 (W11a): `finish_stripe_query` (0061) and `finish_stripe_refund` (0062) complete every *settled*
  operation in state `UNKNOWN` with `result_code` `stripe_terminal_observed` / `stripe_refund_terminal`, and only
  once the terminal `payments.facts` / `refund_facts` row exists. The literal §8 text therefore fired on every
  paid or refunded LIVE order 60 min after success. Those two codes are excluded; a stuck operation (any other
  code) still counts. Gate: SL09 subtest `W11a_a_settled_LIVE_payment_and_refund_are_not_trouble`.
- §5.2 / §7.1: the refund refresh route takes the same deployment-environment guard as the refund POST.
- §7 API-key-compromise row: the rotate command names `--environment LIVE` and the scope flags; `ops-admin.sh`
  refuses register/rotate on a LIVE-pair host without it (`stripe_live_environment_required`). Gate: SL06.
- Preflight P06: `LC_STRIPE_CHECKOUT_ENABLED=1` requires `LC_STRIPE_ENABLED=1` (checkout without worker/webhook
  strands held stock). Gate: SL06 `shell_preflight_P06`.
