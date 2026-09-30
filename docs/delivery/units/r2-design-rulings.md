# R2 design integrator rulings (2026-09-29) — binding for the amendment round of
# merchant-password-auth-v1, taiwan-cvs-logistics-v1, meta-ads-v1, stripe-live-enable-v1,
# customers-billing-v1

Owner decisions already given (override any draft text):
- O-A Merchant login = email + password + emailed code; OIDC optional (supersedes merchant-identity-v1's
  "no password store" line for password principals).
- O-B Outbound email = generic SMTP adapter (Go stdlib net/smtp, implicit TLS 465, no new dependency);
  first configuration = QQ mailbox SMTP (smtp.qq.com:465) with an SMTP authorization code from env/secret
  file; Tencent Exmail (smtp.exmail.qq.com:465, sender @xgdwm.com + SPF/DKIM/DMARC on Cloudflare) is the
  recommended upgrade. Resend is NOT the default. Idempotency: SMTP has no idempotency key — a send is
  attempted once after commit; on an uncertain result the user presses resend (new code). Rate limits
  must fit a personal-mailbox daily cap (treat as UNKNOWN until measured; global cap configurable,
  default 200/day, fail closed with a clear 503 when exhausted).
- O-C Meta app = 「大梦」 app 4291253377792879, to be attached to 香港大碗貿易有限公司's Business
  Portfolio (not Daerdo's 1299693313226979). Business verification and App Review are owner steps.
- O-D Production is operated by Claude on the owner's server; the owner supplies secrets as files;
  secrets never go through chat.

Defaults the integrator accepts now (owner may revise before go-live; list them in the contract's
"Integrator rulings"):
- Auth: Q2 min password 12; Q3 HIBP k-anonymity check on, fail-open; Q4 code on every login, no
  remember-browser; Q5 reset signs in + revokes other sessions; Q6 admin host DNS-only (no CF proxy)
  until a proxy review; Q7 open sign-up (store creation still behind COMMERCE_ONBOARDING_ENABLED);
  Q8 180 d audit / 2 d throttle; Q9 6-digit code. R-1 accept golang.org/x/crypto (argon2id only);
  R-2 synchronous send after commit; R-3 privilege-controlled tables like 0004 (but fix the owner-role
  P1); R-4 OIDC optional when password login enabled; R-5 X-Commerce-Client-IP from Caddy only after the
  BFF key check; R-6 0071 released.
- CVS: Q3 store-to-store default, bulk later; Q6 merchant clicks per order; Q8 bulk
  create/print as follow-up contract; Q9 consent line under the chosen store; Q10 manual services also
  use the ECPay map when connected; Q4 run the OK mart probe first, hide OK mart as "coming soon" if it
  fails (no second provider in R2).
- CVS owner clarification (2026-09-30, supersedes Q5 "no COD in v1"): the chain is
  buyer picks a store by number/name -> the store's full address is backfilled into checkout ->
  merchant ships with that address -> for pay-at-pickup the carrier collects from the buyer.
  C1 Store source, two modes per store: (a) ECPAY_MAP when the store has an ECPay logistics connection
     (TD3 unchanged: e-map return + GetStoreList verification, address from the directory row);
     (b) MANUAL when it has none: buyer enters chain + store number + store name + store address, with
     links to each chain's official store search; server validates the code format per chain; the
     snapshot is labelled source=buyer_entered (never "verified"); the merchant sees that label at
     shipment. No scraping, no chain private endpoints (AGENTS.md).
  C2 Pay-at-pickup (超商取貨付款) is an order payment mode, not a Stripe payment: the order is created
     with payment_mode=pay_at_pickup, stock follows the normal HELD->COMMITTED path (§11.5) at order
     placement, no Stripe session. The platform never holds the money: with an ECPay connection the
     create sends IsCollection=Y + CollectionAmount=order total (I05: server-computed, TWD integer
     1..20000, F17); without one the merchant collects through their own channel (e.g. 7-11 賣貨便 /
     全家 好賣+). Merchant marks "collected" (audited) or the ECPay pickup status (2067/3022) does it.
     Unclaimed/returned (2074/3020) -> order returned, stock handling via the existing refund/return
     path, no money movement. Refunds on pay-at-pickup orders are offline records only.
  C3 Pickup-and-pay needs recipient real name (as on ID) + TW mobile (09xxxxxxxx) in the destination
     snapshot; buyer PII rules unchanged (no fixtures/logs).
  C4 Merchant-selectable per store: which chains, pay-at-pickup on/off, pay-at-pickup max amount.
- Ads: O4 per-store ceiling NT$0 (off) until the operator sets it; O6 purchase-events disclosure,
  unchecked by default; O7 test dataset in the 香港大碗 business; O8 automatic placements; O3 one App
  Review submission after sandbox gates. O2 is replaced by O-C (香港大碗 verification).
- Stripe live: LQ3 canary cap 2x currency minimum, per-order max NT$20,000 for 30 days; LQ4 Radar CVC
  rule on; LQ5 production LIVE-only with a separate staging deployment for SANDBOX; LQ8 approval
  reference format as drafted; LQ2 secrets typed by Claude on the server from owner-supplied files
  (O-D), never pasted in chat.
- Customers/billing: Q3 per store; Q4 pilot stores unrestricted (UNBILLED); Q5 limits shown, not
  blocking; Q6 arrears restricts new claim windows + new ads only; Q7 'unpaid' after retries; Q8 platform
  standard notice, no automatic purge until periods set; Q9 instructions page for Meta data deletion;
  Q10 buyer self-service erasure with typed confirmation; Q11 UTC+8 finance day; Q12 consent channels
  Messenger/IG DM + Meta ads personalization only.

Genuine owner inputs (keep as blocking owner questions with a default; the integrator will ask them):
- CVS Q1/Q2: ECPay logistics requires a Taiwan individual/company. Default (no answer needed to build):
  each merchant's own ECPay account (BYO, TD2); stores without one run MANUAL (C1b).
- Stripe live LQ1: first live store + confirm the live Stripe account holder; LQ6 statement descriptor
  text; LQ7 policy pages content owner.
- Billing Q1/Q2: platform billing entity + plan price/currency.
- Ads O5: budget for the owner's own LIVE ad test.

Amendment rules: fix every P0/P1 at the root with the reviewer's text or an equal-or-stronger fix;
verify each against the real code/SQL; rebut wrong findings with evidence; keep the defaults above.

## Round-3 integrator rulings (2026-09-30) — after two amend rounds (AGENTS.md: escalate, don't loop)
Findings: output/contract-review/r2-round3.json. stripe-live PASS_WITH_P2; four still BLOCK on P1.
- X1 Every round-3 P1 fix is accepted with the reviewer's exact text (auth ip-mail-unauth rows; CVS
  plan_cvs_create / apply_cvs_create_result / hooks-host routing; ads pause-vs-activate) and each
  contract applies its cheap round-3 P2s. No further full review round: the fixes are text given by the
  reviewer; only new material (CVS §16) gets one verification review.
- X2 Ads reconcile-mode Check: take the additive option — `DispatchRequest.Mode` ("dispatch"|"reconcile")
  set by the dispatcher; ads/CAPI Check returns nil in reconcile mode; existing routes ignore it (no
  behaviour change). external-dispatcher-v1 gets a one-line amendment note.
- X3 Auth: R-2 deviation (sign-up/reset send detached after 202, login synchronous) accepted — it closes
  the timing oracle. §12 Q2 residual distributed lockout: default accepted (operator unlock runbook;
  CAPTCHA contract on first incident).
- X4 Billing P1: the claims production-mount purge blocker stays in force. Waiver W1: the pilot (owner's
  own store only) may run claims in production until U08 (retention purge + actor-level deletion of
  bundles.actor_key, bindings, claims.links hashes, claims.meta_intake, social.*) lands; U08 is an R2
  unit and must land before a second merchant is onboarded.
- X5 CVS C1–C4 go into the same contract as §16 (not a separate contract), before 0072 freezes.
- X6 After X1/X5, contracts are FROZEN at "v1 FROZEN 2026-09-30"; remaining owner questions keep
  their defaults and never block implementation.
- X7 Ads: a paused draft never re-activates. Resume = copy into a new draft (new id, fresh approval,
  preflight and allowance). Applied in meta-ads-v1 §6.3.
- X8 CVS pay-at-pickup cancel/restock (supersedes §16.4/§16.7 "allocation stays until the cancel/restock
  contract"): without it anonymous junk orders lock stock and the max_open slots with no way out.
  Add to taiwan-cvs-logistics-v1 §16.8: merchant (fulfillment:write) cancels a pay-at-pickup order that
  is PENDING collection and not handed to a provider (no ECPay create SUCCEEDED/UNKNOWN), and marks a
  RETURNED order "restocked"; both release the order's COMMITTED allocation through one audited
  inventory definer (§11.5 evidence: order id + collection_state + actor), idempotent, no money rows.
  Card orders keep the existing no-cancel rule (stripe-refund-v1 RD6).
- X9 The seven CVS verification P2s (TCV11 vs §16.1, MG01/MG02 undefined, buyer-entered rows readable
  store-wide, kill-switch mode mismatch, BUYER/ALLOCATE ledger CHECK, start_stripe_payment citation,
  EXECUTE grants vs the checkout pool) are fixed in the contract before implementation.
- X10 E-map probe 2026-09-30 (evidence output/cvs-emap-probe/, SANDBOX + LIVE read-only):
  7-11 emap.pcsc.com.tw with an unregistered eshopid -> error E0014; FamilyMart mfme.map.com.tw with an
  unregistered cvsname -> retrieve.aspx "無來源網頁資料" (no callback). Chain e-maps only call back to
  registered e-shops (SHOPLINE uses its own registration). ECPay stage Express/map (public C2C test
  merchant) called back our ServerReplyURL for UNIMARTC2C and FAMIC2C with CVSStoreID/Name/Address.
  Implementation must: trim trailing spaces (7-11 values are space-padded), keep the raw address and
  display it as returned (FamilyMart returns full-width digits/dash), accept 6-digit store ids with
  leading zeros as strings. Our ECPay map path is TD3; direct chain registration is not planned.

## Brief rulings (2026-09-30) — answers to the brief authors' open questions; bind every R2 unit
Auth: B1 deactivation = ops-admin.log + session.revoked events (no 0070 CHECK change). B2 PA01/PA02 stay
with the implementers; auth-tests adds one independent black-box assertion each. B3 the lane integrator
lands F0 (x/crypto + frozen internal/mail/message.go) before dispatch. B4 preflight rule numbers are
assigned by the integrator at the final merge; units name rules by purpose.
Stripe-live: B5 accept `identity.merchant_refund_environment` (S5). B6 use test-focused.sh regexes, no
new test-local modes. B7 legal pages cover the owner's single store (W1); per-store policies are a later
contract before merchant #2. B8 the footer data-deletion link merges only together with
customers-billing-ui (lane order).
Customers/billing: B9 `billing_restricted` is HTTP 402 everywhere (meta-ads approve/publish change 409→402).
B10 CB08 fake lives in internal/billing/billingtest. B11 accept LC_BILLING_ENABLED, LC_BILLING_RETURN_ORIGIN.
Ads: B12 add §4.4 rows `ads.operator_set_settings` (EXECUTE commerce_meta_registrar) and
payments.refund_facts SELECT + read policy for commerce_ads_writer; MA02 asserts them. B13 record
A10-D2/D3 in external-dispatcher-v1. B14 accept D2 (200-char cap), D5/D6/D9. B15 MA02 billing clauses
and the ads-core mount wait for ads-capi (0080).
CVS: B16 package path = contract §7 `internal/integrations/shipping/ecpay`. B17 accept E1 (Finish takes
SecretClaim, SecretClaim.Mode). B18 order: ads-a10 merges, then the CVS lane F0 (R-7a), then cvs-ecpay.
B19 accept C4 (pickup_source appended as the last CSV column; MF06 golden updated in the same change,
not weakened). B20 ECPay map `Device`: the buyer BFF sends 1 for mobile user agents, 0 otherwise (buyers
come from FB/IG on phones). B21 accept A4 (ABANDONED attempts may be cancelled). B22 cvs-core proves
with a REAL_PG test that the integration_writer definers can run resolve_access / command_results.
Retention: B23 accept IR-U1 (D4 CHECK widening for 'erased-'). B24 accept D1/D2. B25 lc_retention_operator
is provisioned only by the runbook after owner approval, never via logins.tsv. B26 a pre-existing
reserved-pattern label stops 0071 with 55000 (fresh pilot DB; no automatic relabel). B27 record the
meta_inbox.lock_purgeable grant in meta-claims-intake-v1's amendment notes.
- X11 Owner correction (2026-09-30): there is NO Taiwan sender; all parcels ship from mainland China to
  Taiwan through cross-border logistics. The pilot merchant's default store source is buyer_entered
  (C1b) with official store-search links; the ECPay map/adapter stays as an optional integration for
  Taiwan-based merchants and is not a pilot prerequisite. Pay-at-pickup (C2) unchanged: the
  cross-border carrier's Taiwan last mile collects. Auto-backfill for the pilot waits on a registered
  store-picker usable by a non-Taiwan entity (research in progress; likely the cross-border carrier).

R2-ADS-PAUSE-1 (round-2 review P1, meta-ads-v1 §5.3): "pause is always allowed" is narrowed to "with an enabled binding". The dispatcher's
enabled/semantic_version gate is unchanged; instead `0074` trigger `integration.bindings.bindings_ads_disable_guard` refuses (PT409
`binding_in_use`, core maps to ErrConflict) disabling a `meta_ads` binding while any draft on it counts by the AD6 test. The merchant
pauses first; a SUCCEEDED pause makes the draft non-counting and the disconnect then succeeds. Gate: MA06 "a binding with a spending campaign cannot be disabled".

## Integration rulings (2026-10-01)
- I1 Ratify stripe-live-enable-v1 amendment 2026-09-30b (W11a settled-op exclusion, refund refresh
  environment guard, P06 rule, rotate command): verified by the lane verifier against 0008/0061/0062.
- I2 0070 was edited in place for the auth F1 throttle fix; allowed because no environment has applied
  0070 (pilot host runs 0066). From the first R2 deploy on, identity.auth_throttle_hit changes only by a
  new migration.
- I3 R2 release gate round 3 at b031285: foundation 1472 pass / 0 fail / 10 skip (SANDBOX/LIVE
  prerequisites), 20 browser modes green, release-gate 0 FAIL. Remaining NOT_RUN: WebKit (install needs
  owner approval), Stripe SANDBOX gates (run separately with STRIPE_BROWSER=1 STRIPE_SANDBOX=1),
  platform-billing SANDBOX CB10, PA12/PA13 live mail, LG01 with LC_LEGAL_REQUIRE_FINAL=1 (owner policy
  text pending).
