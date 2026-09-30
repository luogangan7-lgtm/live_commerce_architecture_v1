# Meta ads v1 — merchant ad account connect, approved campaigns, insights, CAPI Purchase

Status: **v1 FROZEN 2026-09-30 (DESIGN; gates NOT_RUN)** — answers reviews `output/contract-review/r2-design-wave.json`
key `ads` (rounds 1–2) and `output/contract-review/r2-round3.json` key `ads` (round 3, applied per
`docs/delivery/units/r2-design-rulings.md` X1/X2/X6; ledgers at the end). Evidence label for this file: DESIGN. Every MA gate below is
NOT_RUN. Nothing here authorizes spend: LIVE activation stays refused until the operator flips it per
store after owner approval in chat (§0 AD9) and the owner approves each LIVE probe in chat (AGENTS.md
"禁止生产发布广告").

Covers architecture T15 (ad account / draft / budget approval / reporting) and the smallest
T16 slice (CAPI Purchase from verified payment facts, catalog id mapping). Architecture §14.4,
§15, §16 and invariants I06, I07, I12, I14, I20 and gates G09, G10 are authoritative; where they
conflict with this file, this file is wrong. Builds on
[external-operation-v1](external-operation-v1.md), [external-dispatcher-v1](external-dispatcher-v1.md)
(incl. the approved `LoadSecret` hook, meta-claims-intake §6.4, and the **A-10 amendment** of
§3.1, incl. `DispatchRequest.Mode` per ruling X2), [meta-claims-intake-v1](meta-claims-intake-v1.md) §7 (Meta token custody), and
[stripe-psp-v1](stripe-psp-v1.md)/[stripe-refund-v1](stripe-refund-v1.md) (payment facts, contract
style). Release: R3 per `docs/delivery/PROCESS.md` §1 (T15, T16). Upstream interfaces that must be
frozen before implementation: dispatcher A-10 (§3.1), T14 consent gate `customers.consent_allows`
([customers-billing-v1](customers-billing-v1.md) CD5, 0078), payments `CAPTURED` facts (0018/0061).

## 0. Owner inputs, decisions and rulings

Owner input recorded (chat, 2026-09-29): merchants stream live on FB/IG themselves; the platform
must connect to Meta advertising and manage merchants' ad campaigns. **O-C (binding):** the Meta app
is 「大梦」 app `4291253377792879`, attached to 香港大碗貿易有限公司's Business Portfolio; Daerdo's
portfolio `1299693313226979` and Daerdo's app are **not** used. **O-D:** production is operated by
Claude on the owner's server; the owner supplies secrets (app secret, token keys) as files, never in chat.

Project facts from Humaux memory (**re-verify before use**):
- 2026-09-29: app `4291253377792879` is in dev mode, `is_live=false`, has **no business portfolio
  attached, no privacy policy / terms / data-deletion URL, no permission requests**. Everything in §11
  starts from zero for this app.
- 2026-09-15/17 (recorded for the earlier Daerdo test setup, **not** for 大梦): sandbox ad account
  `act_27924149657285854` returned `promote_pages=[]`; campaign + ad set PAUSED CRUD passed; the only
  real `POST /act_{id}/adcreatives` failed HTTP 400 code 100 / subcode 1885183, root-caused to the app
  being in dev mode. Treated as a risk for 大梦 (U9), not as a fact about it. 大梦 needs its own
  sandbox ad account (F20: one per app).

| # | Decision | Why / risk closed |
| --- | --- | --- |
| AD1 | **Auth = Facebook Login for Business (FLfB) with a Business Integration System User (BISU) token** per merchant client business; `config_id` + `response_type=code` + server-side code exchange, all on app 大梦 (O-C). No merchant passwords, no manual token paste in product code, no Business Manager partner-request flow in v1. | F3/F4: Meta's preferred tech-provider auth; BISU "defaults to never expire" and is tied to the client portfolio, fitting automated reporting/CAPI. |
| AD2 | **Assets are bindings** (existing `integration.bindings`): provider `meta_ads` (asset = numeric ad account id, without `act_`) and `meta_dataset` (asset = dataset/pixel id). Page/IG identities reuse the existing `facebook`/`instagram` bindings. The BISU token is stored **per binding** by reusing `integration.meta_page_credentials`/`meta_page_heads` (0074 widens provider + nonce CHECKs), sealed with HPKE so only `cmd/ads-worker` can open it (A-4). | No second credential store. Asset change = new binding → old ops `STALE_BINDING` (G10). |
| AD3 | **The campaign is created `PAUSED`; ad set and ad are created `ACTIVE` under it**, so their `effective_status` is `CAMPAIGN_PAUSED` and nothing can deliver. The only spend-enabling call is `activate` = `POST /{campaign_id} status=ACTIVE`; pause = `POST /{campaign_id} status=PAUSED` (F9). Publish = ordered ops `create_campaign → create_adset → create_creative → create_ad → preflight_account → activate`. Each create is tagged `lc-<operation uuid>` in `name`; Reconcile lists the parent's children and pins the unique tag match. | F9/F22: status is per object and a child's own PAUSED status survives the parent going ACTIVE, so children must be ACTIVE for one campaign switch to start delivery. No idempotency key exists for creates; a duplicate object under a PAUSED campaign costs nothing. |
| AD4 | **Budget = one ad set with `lifetime_budget` + `end_time`** (one campaign, one ad set, one ad per draft). No daily budgets in v1. Campaign `spend_cap` is additionally set to the approved amount when it is ≥ Meta's minimum (F10), else omitted. | F11: a daily budget may be exceeded by up to 75 % per day; a lifetime budget is the tightest documented provider bound. §15.3: still not a real-time hard stop. |
| AD5 | **Approval before any spend.** A draft becomes activatable only with an `ads.draft_approvals` row by a principal holding `ads:approve`, freezing a SHA-256 of the canonical draft (objective, budget, currency, dates, creative source, targeting, binding ids+versions). Any edit after approval invalidates it → re-approval. Dispatcher `Check` of every create and of `activate` re-verifies the hash, `principal_holds(approver, ads:approve)`, AD6 and AD9; `activate` also requires a fresh preflight (§6.1). No network in any `Check`. | §15.2, G10 "草稿经审批才可发布", I07. |
| AD6 | **Internal allowance:** `ads.store_settings.max_active_budget_minor` in `allowance_currency` (operator CLI only, never the merchant) caps Σ approved lifetime budgets of the store's drafts, **except drafts whose campaign is confirmed non-spending**: every `activate` op of every attempt is absent or ended `BLOCKED_POLICY` (the dispatcher sets that state only before any provider call — Check denial, final gate, loader denial, `dispatcher.go:238/258/279`; the activate adapter never returns it), so a READY, DISPATCHING, UNKNOWN or SUCCEEDED activate always counts; or a `pause` op SUCCEEDED whose claim (first DISPATCHING event in `integration.operation_events`) came after every `activate` op of the draft had left READY/DISPATCHING; or `now > ends_at + 1 day`. The draft being checked always counts itself. Draft currency must equal `allowance_currency`. Checked at approve **and** at `activate` Check under a store row lock (`over_allowance`). The platform never pays for ads. | §15.3 "首版不替商家垫付广告费"; ENDED/REJECTED/FAILED with an unconfirmed pause still counts; two activates planned in one sweeper run cannot both pass (round-2 P1). Same test as §5.3. |
| AD7 | **Auto-pause, never auto-raise.** Ingesting an insights read (§6.2) plans a `pause` op when reported spend ≥ 100 % of the approved budget or the account is no longer ACTIVE. Nothing ever increases a budget or re-activates automatically. | §15.3. |
| AD8 | **CAPI Purchase only from a `payments.facts` `CAPTURED` row whose `environment` equals the store's ads environment**, only if `customers.consent_allows(tenant,store,owner,'ads_personalization','meta_ads')` is true both in the planning transaction and in dispatcher `Check` (I07, CD5; no copy of consent state here). `event_id = "lc-purchase-" + attempt uuid`; never resent after UNKNOWN (F15). Refunds send nothing (§15.4); reports net refunds locally. | §15.4, G09 "同意撤回后尚未发送CAPI任务阻止". |
| AD9 | **Environment:** `ads.store_settings.environment` ∈ `SANDBOX`, `LIVE`; default SANDBOX. `LIVE` is set only by the operator CLI after owner approval in chat. With `SANDBOX`, **every mutating meta_ads action (`create_*`, `activate`) is refused in Check** (BLOCKED_POLICY `sandbox_account_required`, zero HTTP) unless the bound ad account = `sandbox_ad_account`; `pause` and reads are always allowed. CAPI in SANDBOX always carries the store's `capi_test_event_code`. | AGENTS.md: no production ad publish without owner approval; no real PAUSED objects in a merchant account before LIVE. |
| AD10 | **Catalog = feed URL, no Catalog API.** The storefront exposes a CSV product feed on its verified host (`id` = variant public id); the merchant registers it in Commerce Manager as a scheduled feed. CAPI `contents[].id` uses the same id. No `catalog_management` permission. | F17: saves one Advanced-Access permission. |
| AD11 | **No token loader without an operation lease.** The BISU token reaches code only through the dispatcher's lease-fenced `LoadSecret`, in `dispatch` mode and (A-10) in `reconcile` mode. Every Meta read the system needs (account preflight, insights) is itself a read operation (`meta.ads.preflight_account`, `meta.ads.read_insights`). `Check` never receives a secret and never calls Meta. | Review P0: Check/Reconcile/insights had no legal token path. |

Explicitly out of v1 (each is a later amendment, not silently dropped): Meta Pixel on the
storefront, `fbc`/`fbclid` capture, customer-list custom audiences and lookalikes, Catalog API
sync, Advantage+ / dynamic creative, multiple ad sets, daily budgets, automated rules,
ad-level breakdowns, async insights jobs, Live-video-specific promotion (F12: no documented API),
lead ads, refund/negative CAPI events.

Rejected alternatives:
- Merchant pastes a user/system-user token: bypasses consent screen, un-auditable scopes, leaks tokens into chat.
- Business Manager partner access: manual, per-merchant BM admin work, not provable by API in the callback.
- Creating ACTIVE campaigns in one call: a timeout would leave a possibly spending ad with no tag reconcile.
- Creating all four objects PAUSED and activating each (round-0 draft): three non-atomic activations, no reconcile rule (review P1).
- Daily budget + our own poller as a hard stop: insights refresh every 15 min (F13) and daily budgets overshoot up to 75 %.
- Planning the CAPI op inside `payments.apply_capture`: amends a frozen money function; a bounded sweeper (§6.4) is enough.
- Resending CAPI with the same `event_id` after UNKNOWN: server↔server dedup is not documented (F15).
- A generic "boost live" API: none documented (F12).
- A network GET inside `Check` or a periodic job with its own token loader (round-0 draft): no lease fence, bypasses token custody (review P0).
- Daerdo's app / portfolio `1299693313226979`: excluded by owner decision O-C.

### 0.1 Integrator rulings still needed (defaults are the draft; nothing assumed silently)

- **A-1** Permissions `ads:read`, `ads:manage`, `ads:approve`; granted to store creators by `create_initial_store`? Default: **no**, explicit provisioning.
- **A-2** Single actor may hold `ads:manage` and `ads:approve` (approve own draft). Default: yes in v1; audit names the principal.
- **A-3** Consent: reuse T14's `customers.consent_allows` with purpose `ads_personalization`/channel `meta_ads`. The grant to `commerce_ads_writer` now lives in this contract's own `0080_meta_capi.sql` (sorts after 0078). Remaining cross-contract touch: the T14 buyer consent handler calls `ads.put_capi_context` after an `ads_personalization` grant (one line, owned by T16's diff). Default: yes.
- **A-4** Token custody: `cmd/api` holds the Meta app secret (code exchange) and only the **HPKE public key** (stdlib `crypto/hpke`, X25519-HKDF-SHA256 / AES-256-GCM; Go 1.27.1 per `go.mod`, no new dependency); `cmd/ads-worker` alone holds the private key file (O-D). Default: yes. Alternative: symmetric keyring in both processes (accepted risk: `cmd/api` can decrypt).
- **A-5** Post-River migration number for the ads-worker River guards. Default: `post_river/0015_meta_ads_river.sql`.
- **A-6** Reuse `integration.meta_page_credentials` for ads tokens (widen provider + nonce CHECKs) vs a new table. Default: reuse (AD2).
- **A-7** CAPI UNKNOWN is never resent (AD8). Default: accept the lost-event risk.
- **A-8** `business_management` in the App Review request. Default: request it (F6); drop it if MA-S3 shows `/act_{id}/adspixels` + `ads_management` suffice.
- **A-9** Graph API version pin for ads: `v26.0` (F18), a config value like metareply's `GraphVersion`.
- **A-10** **Dispatcher amendment (must be approved and frozen before T15 starts; §3.1).** `LoadSecret` also runs in `reconcile` mode for routes that set `ReconcileWithSecret`, fenced on the reconcile claim (operation, generation, lease token); `Check` still never receives a secret; `DispatchRequest.Mode` lets ads/CAPI `Check` return nil in `reconcile` mode (ruling X2). Default: approve.
- **A-11** Migration numbers: `0074_meta_ads.sql`, `0075_meta_ads_insights.sql` (in this branch's 0060–0079 range) and `0080_meta_capi.sql` (outside it: first R3 number, required to sort after 0078). Default: integrator allocates 0080 to this contract.

## 1. Meta facts relied on (retrieved 2026-09-29 UTC unless marked 2026-09-30; WebFetch/WebSearch of developers.facebook.com unless noted)

| # | Fact | Source |
| --- | --- | --- |
| F1 | Access tiers: Limited Access (default, "heavily rate-limited", development) and Full Access (App Review, "lightly rate-limited"). "If your app is managing other people's ad accounts, you need advanced access to the `ads_read` and/or `ads_management` permissions." Full Access needs ≥500 successful Marketing API calls in 15 days and <15 % errors in the last 500 calls. Limited: 1 system user + 1 admin; Full: 10 system users. | <https://developers.facebook.com/documentation/ads-commerce/marketing-api/get-started/authorization> |
| F2 | 2026-05-04: AMSA renamed **Marketing API Access Tier** (Limited/Full); distinct from the `ads_management` permission; screen recordings no longer required for that submission. | <https://developers.meta.com/blog/updates-to-ads-management-standard-access-feature/> |
| F3 | FLfB: Business app type required; `config_id` replaces `scope`; BISU token needs `response_type=code` (+ `override_default_response_type=true`), exchanged server-side at `GET https://graph.facebook.com/<ver>/oauth/access_token?client_id&client_secret&code` (the documented example has no `redirect_uri`; the redirect URI "must match" the dashboard setting — re-checked by search 2026-09-30, direct fetch timed out); BISU "Defaults to never expire" and is "Associated with your business client's business portfolio"; `GET /me?fields=client_business_id` identifies the client; tech providers need Advanced Access via App Review; client revokes under Business Settings → Integrations → Connected apps. | <https://developers.facebook.com/documentation/facebook-login/facebook-login-for-business> |
| F4 | Use a user token for real-time user actions; a BISU token for "programmatic, automated actions on your business clients' assets". | same |
| F5 | Permissions reference: `ads_management` (deps `pages_read_engagement`, `pages_show_list`; Advanced), `ads_read` (Insights + server-side events; Advanced), `business_management` (Advanced), `pages_manage_ads`, `pages_show_list`, `pages_read_engagement`, `instagram_basic`, `catalog_management` (Advanced). Business Verification is required for apps requesting Advanced Access (search excerpt). | <https://developers.facebook.com/docs/permissions/> |
| F6 | FLfB CAPI template: BISU token; `ads_read` + `business_management` recommended baseline; datasets via `owned_pixels` / `client_pixels`. | <https://developers.facebook.com/documentation/facebook-login/facebook-login-for-business/conversions-api-integration-template/> |
| F7 | CAPI as a platform: App Review for Advanced access, Marketing API Access Tier, `ads_management`, `pages_read_engagement`, `ads_read`; `partner_agent` attributes events to the platform. | <https://developers.facebook.com/documentation/ads-commerce/conversions-api/set-up-conversions-api-as-a-platform> |
| F8 | Campaign create `POST /act_{id}/campaigns`: required `name`, `objective` (`OUTCOME_TRAFFIC`, `OUTCOME_AWARENESS`, `OUTCOME_ENGAGEMENT`, `OUTCOME_SALES`, …), `special_ad_categories`, `status` ∈ ACTIVE/PAUSED at creation; `execution_options=["validate_only"]`. | <https://developers.facebook.com/docs/marketing-api/reference/ad-account/campaigns/> |
| F9 | No idempotency key is documented for campaign creation; no name filter documented for `GET /act_{id}/campaigns` (reconcile lists and matches client-side). Campaign `status`: "If this status is `PAUSED`, all its active ad sets and ads will be paused and have an effective status `CAMPAIGN_PAUSED`" (re-fetched 2026-09-30). | <https://developers.facebook.com/docs/marketing-api/reference/ad-campaign-group/> |
| F10 | Campaign `spend_cap`: integer in currency offset, minimum USD 100 equivalent. Account `funding_source` absent → "no delivery"; `account_status` 1 = ACTIVE; `min_daily_budget`, `currency`, `timezone_name` readable. | <https://developers.facebook.com/docs/marketing-api/reference/ad-account/> |
| F11 | Daily budget: Meta may spend up to 75 % more on a day, ≤ 7× daily per week; lifetime budget: total stays within the budget (Help Center, search excerpt). | <https://www.facebook.com/business/help/190490051321426>, <https://www.facebook.com/business/help/1844835042445690> |
| F12 | Ad creative: `object_story_id` = `<page_id>_<post_id>`, `source_instagram_media_id` = existing IG post. **No documented API for promoting an in-progress Live video** (UNKNOWN). | <https://developers.facebook.com/docs/marketing-api/reference/ad-creative/> |
| F13 | Insights: "refresh every 15 minutes and do not change after 28 days of being reported"; metrics may update for a couple of days after an ad completes; throttle header `x-fb-ads-insights-throttle`, error code 4. | <https://developers.facebook.com/docs/marketing-api/insights/best-practices/> |
| F14 | CAPI server event fields; any event >7 days old rejects the whole request; `POST /{ver}/{PIXEL_ID}/events`; `test_event_code` for testing only. | <https://developers.facebook.com/docs/marketing-api/conversions-api/parameters/server-event>, <https://developers.facebook.com/documentation/ads-commerce/conversions-api/using-the-api> |
| F15 | Dedup: pixel `eventID`+`event` = CAPI `event_id`+`event_name`, within 48 h. Server↔server dedup **not documented** (UNKNOWN). | <https://developers.facebook.com/documentation/ads-commerce/conversions-api/deduplicate-pixel-and-server-events> |
| F16 | Customer info hashing (SHA-256; email trim+lowercase; phone digits with country code, `16505551212`). | <https://developers.facebook.com/docs/marketing-api/conversions-api/parameters/customer-information-parameters> |
| F17 | Commerce Manager scheduled data feeds from a URL (Help Center, search excerpt). Exact columns: re-verify at implementation. | <https://www.facebook.com/business/help/2284463181837648> |
| F18 | Latest Graph/Marketing API `v26.0` (2026-07-29); v25.0 available until 2028-07-29. | <https://developers.facebook.com/docs/graph-api/changelog/> |
| F19 | Currency offsets: **TWD offset 1** (budget integer = whole NT$), USD/HKD 100. | <https://developers.facebook.com/docs/marketing-api/currencies> |
| F20 | Sandbox ad account: one per app; no delivery, no spend; "Insights API is currently not supported"; API only. | <https://developers.facebook.com/blog/post/2023/06/21/marketing-api-sandbox-capability-now-re-enabled/> |
| F21 | BUC rate limits per ad account; header `X-Business-Use-Case-Usage` with `estimated_time_to_regain_access`; error 80004 (search excerpt). | <https://developers.facebook.com/docs/marketing-api/overview/rate-limiting/> |
| F22 | Ad set `status`/`configured_status`: "The status set at the ad set level. It can be different from the effective status due to its parent campaign"; `effective_status` includes `CAMPAIGN_PAUSED` (fetched 2026-09-30). | <https://developers.facebook.com/docs/marketing-api/reference/ad-campaign/> |

UNKNOWN (must be closed by a SANDBOX/LIVE gate, never guessed): U1 `special_ad_categories` value for
"none" in v26 (MA-S1); U2 valid `optimization_goal`/`billing_event` pairs per objective (MA-S1); U3
whether BISU tokens can create Page-post creatives without a Page token (MA-S2); U4 whether a finished
Live video's Page post is accepted as `object_story_id` (MA-S2); U5 whether a CAPI-only dataset is
enough for `OUTCOME_SALES` (deferred); U6 Taiwan PDPA legal basis for ad-measurement consent
(legal review, disclosure default in the rulings section); U7 whether the BISU token changes on
merchant re-login (MA-S3; safe either way: §4.1 `client_business_id` check); U8 whether a 4xx create
response can coexist with a created object (MA-S1); U9 whether creative/ad creation needs app 大梦 in
Live mode (earlier Daerdo sandbox run: code 100 / subcode 1885183 in dev mode; MA-S1); U10 whether the
BISU code exchange accepts/needs `redirect_uri` (MA-S3; we send it, identical to the dialog value).

## 2. Flow

1. **Connect** (merchant, `ads:manage` + `integration:manage`): `POST /v1/merchant/ads/meta/connect` returns the FLfB dialog URL (app 大梦) with `config_id`, `response_type=code`, `override_default_response_type=true`, `redirect_uri` = fixed admin callback, `state` = random 32 bytes (hash stored in `ads.oauth_states`, 10 min, single use, bound to principal+store).
2. **Callback** `GET /v1/merchant/ads/meta/callback?code&state` (excluded from URL/access logging; `code`/`state` redacted in every formatter): the session's principal+store must equal the state row's, else fixed code `state_mismatch`; consume state → exchange code (server-to-server, app secret from the owner-supplied secret file, `redirect_uri` identical to the dialog value, U10) → `GET /me?fields=client_business_id` → `GET /me/permissions` → `GET /me/adaccounts?fields=account_id,name,currency,timezone_name,account_status` and `GET /act_{id}/adspixels` → in one transaction the state row gets `client_business_id`, `pick_list` (ids + display names only), `scopes_attested` and the **HPKE-sealed** token (`pending_enc`, `pending_ciphertext`, `pending_key_id`). The plaintext token exists only in this request's memory and is zeroed after sealing; it never reaches the browser, logs or PG in clear.
3. **Bind** `POST /v1/merchant/ads/meta/bindings {state_id, ad_account_id, dataset_id?}`: `RegisterBinding` (`meta_ads` / `meta_dataset`) + `integration.register_meta_ads_token` per binding, which requires the asset ∈ the state's `pick_list` and copies the sealed token server-side (cmd/api never re-handles it). Pending ciphertext is deleted on bind or at `expires_at` (sweeper).
4. **Draft** (`ads:manage`): template `BOOST_POST` (existing FB Page post id or IG media id from existing `facebook`/`instagram` bindings) or `PRODUCT_TRAFFIC`. Budget = lifetime amount + start/end (≤ 30 days), targeting = countries (default `TW`) + age 18–65, placements automatic. Edits allowed only while not APPROVED.
5. **Validate** (sync, no spend): local rules only (§5.2).
6. **Approve** (`ads:approve`): `billing.store_standing` ≠ RESTRICTED (else 409 `billing_restricted`, §5.2), recompute canonical hash, check allowance under store lock, insert approval.
7. **Publish** (`ads:approve`, body `{publish_attempt}` CAS): standing ≠ RESTRICTED (else 409 `billing_restricted`); plans `create_campaign`; the advance sweeper (§6.3) plans each next step after the previous op SUCCEEDED, `preflight_account` right before `activate`. All ops: `purpose='marketing'`, actor MERCHANT, principal = approver.
8. **Run**: insights read ops (§6.2) feed `ads.insights_daily`; auto-pause (AD7); merchant can pause any time (priority lane).
9. **CAPI**: sweeper (§6.4) plans one `meta.capi.purchase` op per consented CAPTURED attempt of a store with CAPI enabled.

## 3. Wire adapter `internal/integrations/meta_ads` (+ `meta_catalog` feed writer in `internal/attribution`)

- Host `graph.facebook.com` only (constructor rejects others; tests use an `httptest` base URL flag like `metareply.Config`). Version from config (A-9).
- Every call bounded by the dispatcher `CallTimeout`; token only from `LoadSecret` (AD11); redacted formatters.
- **Money conversion (I05):** `metaBudget(currency, amount_minor)`: TWD → `amount_minor/100` only when `amount_minor % 100 == 0`, else `ErrNotWholeUnit` (never truncates; Meta offset 1, F19); USD/HKD → `amount_minor`; any other currency → `ErrUnsupportedCurrency`. `spendMinor(currency, s)`: Meta's decimal `spend` string (account currency) → exact decimal parse × 100 for TWD/USD/HKD; more than 2 fraction digits, sign, exponent or non-digits → error (op FAILED_FINAL `bad_spend`, never rounded).
- **Classification, creates** (`create_campaign|adset|creative|ad`): 2xx with an `id` → SUCCEEDED + provider_reference; HTTP 4xx with a parseable Graph `error` body → FAILED_FINAL `graph_<error.code>`, except codes 4/17/613/80004 → FAILED_FINAL `rate_limited` (retry = new publish attempt, §5.3); timeout, 5xx, transport error, unparseable body → UNKNOWN. U8 is closed by MA-S1 negative probes.
- **Classification, `activate`/`pause`:** 2xx `{"success":true}` → SUCCEEDED; **any other response → UNKNOWN**, reconciled by `GET /{campaign_id}?fields=status,effective_status`; never FAILED_FINAL (a rate-limited or rejected status POST does not prove the campaign's state).
- **Classification, reads** (`preflight_account`, `read_insights`): 2xx parsed → SUCCEEDED with the result in `provider_reference` (grammar below); 4xx with error body → FAILED_FINAL `graph_<code>`/`rate_limited` (a read has no effect; the planner plans the next seq); else UNKNOWN, and Reconcile simply repeats the read.
- **Read results** (`provider_reference`, ≤255 chars, `^v1(;[a-z]{2,3}=[A-Za-z0-9_./+-]{1,40}){1,9}$`): preflight `v1;st=<account_status>;cur=<ISO>;fund=<0|1>;tz=<timezone_name>`; insights (one day) `v1;es=<campaign effective_status>;sp=<spend decimal>;im=<int>;cl=<int>;pu=<int|na>;pv=<decimal|na>;cur=<ISO>;tz=<tz>`. `# ponytail: results ride in provider_reference (no schema change); add an operation result column if a read ever needs >255 chars.`
- **Reconcile** (query only, token via A-10): create ops: `GET /{parent}/{campaigns|adsets|ads}?fields=id,name&limit=100` (creatives under `act_{id}/adcreatives`), ≤10 pages, exact tag `lc-<op uuid>`: one match → SUCCEEDED; zero → stays UNKNOWN (never recreate); >1 → UNKNOWN `duplicate_remote_objects` + review. `activate`/`pause`: status GET matches target → SUCCEEDED, else UNKNOWN. Reads: repeat the read. CAPI: returns UNKNOWN unchanged (no query API; AD8) and needs no secret (plain `Reconcile`).
- CAPI: `POST /{pixel_id}/events` with exactly one event, `partner_agent` = config constant, `test_event_code` = store `capi_test_event_code` iff SANDBOX; 2xx with `events_received==1` → SUCCEEDED; 4xx with error body → FAILED_FINAL; else UNKNOWN.

### 3.1 A-10 dispatcher amendment (proposed text for external-dispatcher-v1; verified against `internal/integrations/core/dispatcher.go` lines 29–43, 45–58, 226, 236–240, 267–291 and `migrations/0008` operation CHECK)

Today `LoadSecret` runs only when `claim.Mode == "dispatch"` and `Reconcile(ctx, DispatchRequest)` gets no
secret; a reconcile claim already carries its own fence (`state='UNKNOWN'`, `lease_mode='reconcile'`,
generation, lease-token hash). Amendment:
- `DispatchRoute` gains optional `ReconcileWithSecret func(context.Context, DispatchRequest, Secret) (Outcome, error)`. Allowed only on routes with the `LoadSecret`+`DispatchWithSecret` pair; exactly one of `Reconcile`/`ReconcileWithSecret` is set (route validation fails otherwise).
- In `reconcile` mode, after the final gate, the dispatcher runs `LoadSecret` with `SecretClaim{operation, generation, lease token}` of the reconcile claim in the same bounded transaction as in dispatch mode, calls `ReconcileWithSecret`, then zeroes the secret. Loader `ErrPolicyDenied` in reconcile mode → stays UNKNOWN `credential_unavailable` (never BLOCKED_POLICY: an effect may exist); any other loader error → UNKNOWN `secret_load_failed`. The lease inequality is unchanged.
- `Check` never receives a secret (unchanged). Existing routes are unaffected.
- **Reconcile-mode `Check` (round 3, ruling X2).** The dispatcher calls `Check` for every claimed op in either mode (`dispatcher.go:226`), and a reconcile-mode denial goes to `completeAmbiguous('policy_check_failed')` (`:236–240`), so a dispatch-only rule (fresh preflight, standing, approver) would keep an UNKNOWN op unreconcilable. `DispatchRequest` (`:29–43`, no mode today) gains `Mode string` (`"dispatch"|"reconcile"`), set by the dispatcher from `claim.Mode` before `Check`; every ads/CAPI `Check` returns nil when `Mode="reconcile"` (Reconcile is query-only and has no effect; the claim fence and the final gate still apply). Existing routes ignore the field (no behaviour change). Amendment note recorded in external-dispatcher-v1.
- The ads SQL loader (§4.1) accepts exactly `(state='DISPATCHING', lease_mode='dispatch')` or `(state='UNKNOWN', lease_mode='reconcile')`, with equal generation, unexpired lease and matching lease-token hash.
- Gate MA11 (below).

## 4. Persistence: `0074_meta_ads.sql`, `0075_meta_ads_insights.sql`, `0080_meta_capi.sql` (A-11)

All new tables FORCE RLS, PUBLIC revoked, no direct runtime login grants, `COMMENT ON` per PROCESS §5; writes only through SECURITY DEFINER functions with `SET search_path=pg_catalog`; owners: `commerce_integration_writer` (credentials, operations), new NOLOGIN `commerce_ads_writer` (ads schema). Policy pattern = G/M GUC scope from 0064. Exact grants: §4.4.

### 4.1 0074 — widening + ads core

- `identity.store_grants` permission CHECK: re-derive via `pg_get_constraintdef`, add `ads:read`, `ads:manage`, `ads:approve` (no grant rows, A-1).
- `integration.meta_page_credentials`: drop/re-add `provider` CHECK → `IN ('facebook','instagram','meta_ads','meta_dataset')` and `nonce` CHECK → `(provider IN ('facebook','instagram') AND octet_length(nonce)=12) OR (provider IN ('meta_ads','meta_dataset') AND octet_length(nonce)=32)` (for ads rows `nonce` holds the HPKE encapsulated key). Ads payload = the BISU token; HPKE `info` = `["livecommerce/meta-ads-token/v1", tenant, store, key_id]`; keypair `COMMERCE_META_ADS_TOKEN_HPKE_KEYS` (private key file only in `cmd/ads-worker`, public key only in `cmd/api`), distinct from page-token and payment keyrings. `load_meta_page_token` is **not** changed: it already selects only operations with `action='meta.private_reply'` and requires the op's binding/provider (0064 lines 903–917), so it cannot return an ads row.
- `GRANT EXECUTE ON FUNCTION identity.principal_holds(uuid,uuid,uuid,text[]) TO commerce_ads_writer` (today only claims_writer and integration_writer, 0064:77).
- `integration.register_meta_ads_token(p_hash bytea, p_store uuid, p_state uuid, p_binding uuid, p_expected_version bigint) → bigint` (owner `commerce_integration_writer`, EXECUTE `commerce_runtime`): authenticates the merchant session hash via `identity.resolve_access` (0003; pattern of 0040), requires `integration:manage` + `ads:manage` (`principal_holds`), sets `app.*` GUCs from the result; requires the state row to belong to the same principal+store, `used_at` set and `now < expires_at`, pending ciphertext present, and the binding's provider/asset ∈ `pick_list`; scopes ⊇ {`ads_management`} (+`ads_read` for datasets); if the binding already has an `ads.connections` row, `client_business_id` must be equal (else `PT409 client_business_changed`); head CAS; copies `pending_key_id/enc/ciphertext` into a new credential version; writes `ads.connections`; audited `meta.ads_token_registered`. No caller-supplied principal.
- `integration.load_meta_ads_token(p_operation, p_generation, p_lease_token)`: clone of `load_meta_page_token` that requires `action LIKE 'meta.ads.%' OR action = 'meta.capi.purchase'`, binding provider ∈ {`meta_ads`,`meta_dataset`}, and the A-10 state/lease pairs; EXECUTE `commerce_worker`. It is the only reader of ads ciphertext.

```sql
CREATE SCHEMA ads;
CREATE TABLE ads.store_settings (               -- written only by definers: cmd/meta-admin (operator) and ads.set_capi (merchant)
 tenant_id uuid NOT NULL, store_id uuid NOT NULL, PRIMARY KEY(tenant_id,store_id),
 environment text NOT NULL DEFAULT 'SANDBOX' CHECK(environment IN ('SANDBOX','LIVE')),
 sandbox_ad_account text CHECK(sandbox_ad_account ~ '^[0-9]{1,40}$'),
 max_active_budget_minor bigint NOT NULL DEFAULT 0 CHECK(max_active_budget_minor BETWEEN 0 AND 100000000000),
 allowance_currency text NOT NULL DEFAULT 'TWD' CHECK(allowance_currency IN ('TWD','USD','HKD')),
 capi_enabled boolean NOT NULL DEFAULT false, capi_dataset_binding uuid,
 capi_enabled_by uuid,                                   -- principal of CAPI ops (§6.4)
 capi_test_event_code text CHECK(capi_test_event_code ~ '^[A-Z0-9]{4,20}$'),
 updated_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 FOREIGN KEY(tenant_id,store_id) REFERENCES control.stores(tenant_id,id),
 FOREIGN KEY(tenant_id,store_id,capi_dataset_binding) REFERENCES integration.bindings(tenant_id,store_id,id),
 FOREIGN KEY(tenant_id,capi_enabled_by) REFERENCES identity.memberships(tenant_id,principal_id),
 CHECK(NOT capi_enabled OR (capi_dataset_binding IS NOT NULL AND capi_enabled_by IS NOT NULL)),
 CHECK(NOT capi_enabled OR environment='LIVE' OR capi_test_event_code IS NOT NULL));
CREATE TABLE ads.oauth_states (
 id uuid PRIMARY KEY, tenant_id uuid NOT NULL, store_id uuid NOT NULL, principal_id uuid NOT NULL,
 state_hash bytea NOT NULL UNIQUE CHECK(octet_length(state_hash)=32),
 created_at timestamptz NOT NULL DEFAULT clock_timestamp(), used_at timestamptz,
 expires_at timestamptz NOT NULL,                                     -- created_at + 10 min
 client_business_id text CHECK(client_business_id ~ '^[0-9]{1,40}$'),
 pick_list jsonb CHECK(pick_list IS NULL OR (jsonb_typeof(pick_list)='array' AND octet_length(pick_list::text)<=16384)),
 scopes_attested text[],
 pending_key_id text CHECK(pending_key_id ~ '^[A-Za-z0-9_-]{1,64}$'),
 pending_enc bytea CHECK(octet_length(pending_enc)=32),
 pending_ciphertext bytea CHECK(octet_length(pending_ciphertext) BETWEEN 17 AND 8192),
 CHECK(used_at IS NULL OR used_at >= created_at), CHECK(expires_at > created_at),
 CHECK((pending_ciphertext IS NULL) = (pending_enc IS NULL) AND (pending_enc IS NULL) = (pending_key_id IS NULL)),
 FOREIGN KEY(tenant_id,principal_id) REFERENCES identity.memberships(tenant_id,principal_id));
CREATE TABLE ads.connections (                    -- one per ads binding; U7 guard
 tenant_id uuid NOT NULL, store_id uuid NOT NULL, binding_id uuid NOT NULL,
 client_business_id text NOT NULL CHECK(client_business_id ~ '^[0-9]{1,40}$'),
 oauth_state_id uuid NOT NULL REFERENCES ads.oauth_states(id), connected_by uuid NOT NULL,
 connected_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 PRIMARY KEY(tenant_id,store_id,binding_id),
 FOREIGN KEY(tenant_id,store_id,binding_id) REFERENCES integration.bindings(tenant_id,store_id,id));
CREATE TABLE ads.campaign_drafts (
 tenant_id uuid NOT NULL, store_id uuid NOT NULL, id uuid PRIMARY KEY,
 ad_binding_id uuid NOT NULL, ad_binding_version bigint NOT NULL,
 identity_binding_id uuid NOT NULL,                        -- facebook or instagram binding (Page/IG)
 template text NOT NULL CHECK(template IN ('BOOST_POST','PRODUCT_TRAFFIC')),
 source_ref text NOT NULL CHECK(source_ref ~ '^[0-9_]{1,80}$' OR source_ref ~ '^[0-9a-f-]{36}$'),
 currency text NOT NULL CHECK(currency IN ('TWD','USD','HKD')),
 lifetime_budget_minor bigint NOT NULL CHECK(lifetime_budget_minor > 0),
 starts_at timestamptz NOT NULL, ends_at timestamptz NOT NULL CHECK(ends_at > starts_at AND ends_at <= starts_at + interval '30 days'),
 countries text[] NOT NULL CHECK(cardinality(countries) BETWEEN 1 AND 10),
 age_min smallint NOT NULL CHECK(age_min BETWEEN 18 AND 65), age_max smallint NOT NULL CHECK(age_max BETWEEN age_min AND 65),
 revision integer NOT NULL DEFAULT 1 CHECK(revision BETWEEN 1 AND 1000),
 publish_attempt integer NOT NULL DEFAULT 0 CHECK(publish_attempt BETWEEN 0 AND 5),
 ended_at timestamptz, created_by uuid NOT NULL, created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 CHECK(currency<>'TWD' OR lifetime_budget_minor % 100 = 0),                    -- I05, F19
 UNIQUE(tenant_id,store_id,id),
 FOREIGN KEY(tenant_id,store_id,ad_binding_id) REFERENCES integration.bindings(tenant_id,store_id,id),
 FOREIGN KEY(tenant_id,store_id,identity_binding_id) REFERENCES integration.bindings(tenant_id,store_id,id));
CREATE TABLE ads.draft_approvals (                 -- append-only
 tenant_id uuid NOT NULL, store_id uuid NOT NULL, draft_id uuid NOT NULL, revision integer NOT NULL,
 draft_sha256 bytea NOT NULL CHECK(octet_length(draft_sha256)=32),
 principal_id uuid NOT NULL, approved_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 PRIMARY KEY(tenant_id,store_id,draft_id,revision),
 FOREIGN KEY(tenant_id,store_id,draft_id) REFERENCES ads.campaign_drafts(tenant_id,store_id,id));
CREATE TABLE ads.remote_objects (                  -- one row per publish step, pinned once
 tenant_id uuid NOT NULL, store_id uuid NOT NULL, draft_id uuid NOT NULL, publish_attempt integer NOT NULL,
 kind text NOT NULL CHECK(kind IN ('campaign','adset','creative','ad','preflight','activate','pause')),
 seq integer NOT NULL DEFAULT 1 CHECK(seq BETWEEN 1 AND 50),       -- preflight/activate/pause may repeat
 operation_id uuid NOT NULL UNIQUE, remote_id text CHECK(remote_id ~ '^[0-9]{1,40}$'),
 PRIMARY KEY(tenant_id,store_id,draft_id,publish_attempt,kind,seq),
 FOREIGN KEY(tenant_id,store_id,operation_id) REFERENCES integration.operations(tenant_id,store_id,id),
 FOREIGN KEY(tenant_id,store_id,draft_id) REFERENCES ads.campaign_drafts(tenant_id,store_id,id));
```

Set-once trigger on `remote_objects.remote_id` (NULL→value; copied from the op's `provider_reference` by the advance sweeper; `preflight` rows keep `remote_id` NULL, their result is the op's `provider_reference`). Draft frozen columns change only while no approval for the current revision exists; every edit bumps `revision`.

Merchant writes to `ads.store_settings` only via definer `ads.set_capi(p_hash, p_store, p_enabled, p_dataset_binding, p_test_event_code)` (`resolve_access` + `ads:manage`; updates `capi_enabled`, `capi_dataset_binding`, `capi_test_event_code`, and sets `capi_enabled_by` = the resolved principal). `environment`, `sandbox_ad_account`, `max_active_budget_minor`, `allowance_currency` change only via `cmd/meta-admin` definers.

### 4.2 0075 — insights

```sql
CREATE TABLE ads.insight_reads (                   -- one per read op; ingestion marker
 tenant_id uuid NOT NULL, store_id uuid NOT NULL, operation_id uuid PRIMARY KEY,
 draft_id uuid NOT NULL, day date NOT NULL, ingested_at timestamptz,
 FOREIGN KEY(tenant_id,store_id,operation_id) REFERENCES integration.operations(tenant_id,store_id,id),
 FOREIGN KEY(tenant_id,store_id,draft_id) REFERENCES ads.campaign_drafts(tenant_id,store_id,id));
CREATE TABLE ads.insights_daily (
 tenant_id uuid NOT NULL, store_id uuid NOT NULL, draft_id uuid NOT NULL, campaign_remote_id text NOT NULL,
 day date NOT NULL,                                   -- ad account timezone
 account_timezone text NOT NULL, currency text NOT NULL,
 spend_minor bigint NOT NULL CHECK(spend_minor >= 0), impressions bigint NOT NULL CHECK(impressions>=0),
 clicks bigint NOT NULL CHECK(clicks>=0), meta_purchases bigint, meta_purchase_value_minor bigint,  -- NULL = not reported
 effective_status text NOT NULL, source_operation_id uuid NOT NULL REFERENCES ads.insight_reads(operation_id),
 fetched_at timestamptz NOT NULL, final boolean NOT NULL DEFAULT false,   -- day ≤ today−28 (F13)
 PRIMARY KEY(tenant_id,store_id,campaign_remote_id,day),
 FOREIGN KEY(tenant_id,store_id,draft_id) REFERENCES ads.campaign_drafts(tenant_id,store_id,id));
```

Upsert only through `ads.put_insights_day(...)` (owner `commerce_ads_writer`, EXECUTE `commerce_worker`), which parses the op's `provider_reference` itself; rows with `final=true` are immutable.

### 4.3 0080 — CAPI (consent is T14's, A-3)

Sorts after `0078_customers_privacy.sql` and `0079_platform_billing.sql`, so both functions exist at create
time (grants to `commerce_ads_writer` cannot live in 0078/0079: the role is created in R3's 0074, customers-billing C-7).
This file contains:
- `GRANT USAGE ON SCHEMA customers, billing TO commerce_ads_writer`
- `GRANT EXECUTE ON FUNCTION customers.consent_allows(uuid,uuid,uuid,text,text) TO commerce_ads_writer`
- `GRANT EXECUTE ON FUNCTION billing.store_standing(uuid,uuid) TO commerce_ads_writer` (§5.2). Requires
  `store_standing` to be `SECURITY DEFINER` as customers-billing's functions are; if 0079 ships it as
  invoker, RLS on `billing.subscriptions` would hide rows and yield UNBILLED = fail open, so MA02 asserts a
  RESTRICTED fixture reads RESTRICTED **as `commerce_ads_writer`**. The ads definers that call it are
  plpgsql (name resolved at call time); ads-worker and the ads merchant routes start only when 0080 is
  applied (T15+T16 ship together in R3).
- `ads.capi_user_data(p_operation uuid, p_generation bigint, p_lease_token bytea) → (ph_e164 text,
  owner_id uuid, contents jsonb, value_minor bigint, currency text, event_source_url text, user_agent text)`
  (owner `commerce_ads_writer`, EXECUTE `commerce_worker`): same state/lease fence as
  `load_meta_ads_token` (§3.1), only for `action='meta.capi.purchase'`; resolves the op's `capi_events`
  row → attempt → order → buyer owner, destination phone, order lines. Needs column-level SELECT + a
  read policy for `commerce_ads_writer` on the attempt→order→owner/destination/line columns it reads
  (`checkout.orders(owner_id,destination_id)`, the payment attempt's `order_id`/`owner_id`,
  `storefront.destination_snapshots(phone)`, order-line variant id + quantity) and nothing else. The
  sweeper's owner/consent lookup (§6.4) uses the same column grants.
- `ads.feed_rows(p_origin text)` (owner `commerce_ads_writer`, EXECUTE `commerce_buyer_runtime`): calls
  `buyer.resolve_published_store(p_origin)` itself (the store is never taken from the caller) and returns
  only published products/variants of that store; column-level SELECT + read policy on the catalog
  columns F17 needs.

```sql
CREATE TABLE ads.capi_contexts (                   -- UA for website events (F14); purged after 8 days
 tenant_id uuid NOT NULL, store_id uuid NOT NULL, owner_id uuid NOT NULL,
 user_agent text NOT NULL CHECK(char_length(user_agent) BETWEEN 1 AND 512),
 captured_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 PRIMARY KEY(tenant_id,store_id,owner_id),
 FOREIGN KEY(tenant_id,store_id,owner_id) REFERENCES buyer.owners(tenant_id,store_id,id));
CREATE TABLE ads.capi_events (                     -- one per attempt, ever
 tenant_id uuid NOT NULL, store_id uuid NOT NULL, attempt_id uuid NOT NULL,
 operation_id uuid NOT NULL UNIQUE,
 event_id text NOT NULL UNIQUE CHECK(event_id ~ '^lc-purchase-[0-9a-f-]{36}$'),
 planned_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 fact_kind text GENERATED ALWAYS AS ('CAPTURED') STORED,
 PRIMARY KEY(tenant_id,store_id,attempt_id),
 FOREIGN KEY(tenant_id,store_id,attempt_id,fact_kind) REFERENCES payments.facts(tenant_id,store_id,attempt_id,kind),
 FOREIGN KEY(tenant_id,store_id,operation_id) REFERENCES integration.operations(tenant_id,store_id,id));
```

`ads.put_capi_context(p_hash, p_store, p_user_agent)` (owner `commerce_ads_writer`, EXECUTE
`commerce_buyer_runtime`, `buyer.resolve_scope` like `customers.buyer_set_consent`): upserts only
when `consent_allows(...,'ads_personalization','meta_ads')` is already true; the buyer consent PUT
handler calls it right after a grant (A-3 hook). Withdrawal or erasure: `consent_allows` turns false →
sweeper skips, `Check` → BLOCKED_POLICY `consent_withdrawn` with zero HTTP; the sweeper deletes the
context row. Already-sent events are not recalled (no documented CAPI delete; disclosed in consent copy).

### 4.4 Privilege delta (MA02 asserts equality against this table; nothing else changes)

| Role | Object | Privilege | Reason |
| --- | --- | --- | --- |
| commerce_ads_writer (new, NOLOGIN) | schema `ads`, all `ads.*` tables | USAGE; SELECT/INSERT/UPDATE/DELETE via RLS policy to itself only | owner of every ads definer |
| commerce_ads_writer | `identity.principal_holds(uuid,uuid,uuid,text[])` | EXECUTE | approve/Check re-authorization |
| commerce_ads_writer | `identity.resolve_access(bytea,uuid,text)` | EXECUTE | merchant definers (`ads.set_capi`, draft/approve/publish) |
| commerce_ads_writer | `integration.operations`, `integration.operation_events`, `integration.bindings`, `payments.facts`, `control.stores` | SELECT + a read policy per table limited to what the definers read (ops: `provider IN ('meta_ads','meta_dataset')`; events: EXISTS such an op — AD6 pause-claim order) | Check, sweepers, allowance. A grant without a policy reads zero rows under FORCE RLS → allowance sum empty = fail open; MA02 checks the policy exists |
| commerce_ads_writer | `integration.operations`, `integration.operation_events`; sequence `integration.operation_events_id_seq` | INSERT (+ INSERT policy `WITH CHECK (provider IN ('meta_ads','meta_dataset') AND (action LIKE 'meta.ads.%' OR action='meta.capi.purchase') AND actor_kind='MERCHANT' AND purpose='marketing')`; events policy = EXISTS such an op, 0035:116–121 pattern); USAGE on the sequence | op planning (publish, advance sweeper, insights planner, CAPI sweeper) |
| commerce_ads_writer | `customers.consent_allows(uuid,uuid,uuid,text,text)`, `billing.store_standing(uuid,uuid)`, schemas `customers`, `billing` (0080) | EXECUTE; USAGE | AD8; §5.2 billing standing |
| commerce_ads_writer | `buyer.resolve_published_store(text)`, schema `buyer` (0080) | EXECUTE; USAGE | `ads.feed_rows` |
| commerce_ads_writer | attempt→order→owner/destination/line columns and published-catalog columns listed in §4.3 (0080) | column SELECT + read policy | `ads.capi_user_data`, CAPI sweeper, `ads.feed_rows` |
| commerce_integration_writer | `identity.resolve_access(bytea,uuid,text)` | EXECUTE | `register_meta_ads_token` (today granted only to runtime, checkout/media/claims writers: 0003:45, 0035:163, 0060:322, 0062:260) |
| commerce_integration_writer | schema `ads`; `ads.oauth_states`, `ads.connections` | USAGE; SELECT, UPDATE / INSERT + RLS policies to itself (GUC scope set by the function from `resolve_access`) | `register_meta_ads_token` copies sealed token, writes connection |
| commerce_runtime, commerce_worker, commerce_buyer_runtime | schema `ads` | USAGE | calling the ads definers below |
| commerce_runtime | `integration.register_meta_ads_token(bytea,uuid,uuid,uuid,bigint)`, `ads.*` merchant definers | EXECUTE | merchant API |
| commerce_worker | `integration.load_meta_ads_token(uuid,bigint,bytea)`, `ads.capi_user_data(uuid,bigint,bytea)` (0080), `ads.check_*`, `ads.advance_*`, `ads.put_insights_day`, `ads.plan_capi_*` | EXECUTE | ads-worker dispatcher + sweepers |
| commerce_buyer_runtime | `ads.put_capi_context(bytea,uuid,text)`, `ads.feed_rows(text)` (0080) | EXECUTE | buyer consent hook; public feed |
| PUBLIC | every new function/table | none (REVOKE ALL) | default deny |

## 5. State machine and rules

### 5.1 Draft status (derived, never stored as a mutable column)

`DRAFT` (no approval for current revision) → `APPROVED` → `SUBMITTING` (any create/preflight op of current
attempt not terminal) → `REMOTE_PAUSED` (4 remote ids pinned, campaign never activated) → `ACTIVE`
(activate SUCCEEDED, no later pause SUCCEEDED) ⇄ `PAUSED`; `UNKNOWN` (any op UNKNOWN — "check Ads
Manager" + manual refresh + pause button); `FAILED` (a create op FAILED_FINAL; no activate was ever
planned in that attempt); `ENDED` (`ended_at` set after `ends_at` or merchant end; triggers a pause op;
counts toward allowance until AD6 says non-spending). `REJECTED` = Meta `effective_status` in
`DISAPPROVED`/`WITH_ISSUES` from the latest insights read (projection, not a local decision; still counts
toward allowance per AD6). Architecture §15.2 `VALIDATED`/`APPROVAL_REQUIRED` collapse into `DRAFT`.
**Derived status is display-only; no job selects drafts by it** (§6.2/§6.3 select by approvals, pinned
ids and op states).

### 5.2 Local validation (Go, also re-run in `Check`)

Budget ≥ max(`min_daily_budget` × days, 1 unit) from the latest preflight; TWD budget a whole NT$
(`% 100 == 0`); draft currency = store `allowance_currency` = account currency (preflight `cur`);
`starts_at ≥ now+10 min`; bindings enabled at the frozen versions and same store; BOOST_POST source
belongs to the identity binding's Page/IG (`page_id_` prefix for FB); PRODUCT_TRAFFIC product exists,
published, with an active storefront domain.

**Billing standing (BD5, accepted default Q6):** `billing.store_standing(tenant,store)` = RESTRICTED →
approve/publish 409 `billing_restricted`; `create_*`/`activate` Check → BLOCKED_POLICY `billing_restricted`
(zero HTTP). `pause`, `preflight_account`, `read_insights` and CAPI are never blocked by billing (BD5:
only new growth actions). GOOD/GRACE/UNBILLED pass (UNBILLED = pilot stores, Q4).

### 5.3 Rules

- Re-publish after FAILED creates a new attempt (`publish_attempt+1`, max 5) by definer CAS on the body's `publish_attempt` (409 `attempt_changed`), `Idempotency-Key` via `command.Run`. It is refused (409 `prior_attempt_not_paused`) unless every earlier attempt is non-spending by the AD6 test (every `activate` op absent or `BLOCKED_POLICY`, or a `pause` op SUCCEEDED whose claim (first DISPATCHING event in `integration.operation_events`) came after every `activate` op of the draft had left READY/DISPATCHING). Remote objects of a failed attempt stay under their PAUSED campaign (never deleted by us).
- Pause is always allowed (reduces spend) with any enabled or disabled-but-same-asset binding; its Check does not re-verify the principal. A pause op not SUCCEEDED within 15 min → the advance sweeper plans the next pause seq (≤ 50) and raises an ops alert; a revoked token → op BLOCKED_POLICY `credential_unavailable` at dispatch (or UNKNOWN in reconcile) and the UI shows the Ads Manager link.
- Binding change (different ad account) → unfinished ops `STALE_BINDING`/UNKNOWN per ledger; drafts on the old binding cannot be approved again.

## 6. Workers and jobs (`cmd/ads-worker`, new; queue `ads`, priority lane for pause)

### 6.1 Dispatcher routes (provider, action, purpose=`marketing`, actor MERCHANT)

`(meta_ads, meta.ads.create_campaign|create_adset|create_creative|create_ad|preflight_account|activate|pause|read_insights)`:
`LoadSecret` = `integration.load_meta_ads_token`, `DispatchWithSecret`, `ReconcileWithSecret` (A-10).
`(meta_dataset, meta.capi.purchase)`: `LoadSecret` + `DispatchWithSecret` + plain `Reconcile` (returns
UNKNOWN; no secret). **`Check` is PG-only** (no network, no secret). Check rules apply to dispatch mode only (`DispatchRequest.Mode`, §3.1):
- creates: approval hash current; `principal_holds(approver, ads:approve)`; AD9 sandbox rule; standing ≠ RESTRICTED (§5.2).
- `preflight_account`, `read_insights`, `pause`: binding same asset; nothing else (read-only or spend-reducing).
- `activate`: no `pause` op exists for this draft (any attempt, any state) and `ended_at IS NULL`, else BLOCKED_POLICY `pause_requested` (zero HTTP); AD5 hash + approver re-check, standing ≠ RESTRICTED (§5.2), AD6 allowance under store row lock (BLOCKED_POLICY `over_allowance`; the sweeper plans activate only per the §6.3 room test, Check re-verifies), AD9; the latest SUCCEEDED `preflight_account` op of this attempt must have `st=1`, `cur` = draft currency, `fund=1` (LIVE only) and `updated_at ≥ now−10 min`, else BLOCKED_POLICY `preflight_stale`/`account_not_ready` (zero HTTP; the sweeper then plans preflight seq+1 and activate seq+1).
- CAPI: consent active, store `capi_enabled`, dataset binding current, `principal_holds(capi_enabled_by, ads:manage)` else BLOCKED_POLICY `capi_principal_revoked`, fact environment = store environment, `event_time ≥ now−6 days`.
Frozen requests contain internal ids + the canonical draft (no tokens, no PII); CAPI user data is read
at dispatch only through the lease-fenced definer `ads.capi_user_data` (§4.3), hashed in memory, never
persisted.

### 6.2 Insights `ads_insights_plan_v1` (River periodic, daily 04:00 store-local + on-demand refresh ≤ 1/h) — plans reads, no network

Per draft whose campaign id is pinned (any derived status, incl. UNKNOWN) and `ends_at + 3 days ≥ today`:
plans one `meta.ads.read_insights` op per day in `[max(start, today−3), today]` (semantic key
`ads:ins:<draft>:<yyyy-mm-dd>:<yyyymmddhh plan hour>`, principal = the attempt's approver) + an
`ads.insight_reads` row, in one transaction. The op performs `GET /{campaign_id}/insights?level=campaign&fields=spend,impressions,clicks,actions,action_values&time_range={"since":day,"until":day}` and `GET /{campaign_id}?fields=effective_status`. The advance sweeper (§6.3) ingests SUCCEEDED reads via `ads.put_insights_day` (`spendMinor`, §3), sets `final` for days ≤ today−28, and plans auto-pause (AD7). Rate-limited reads (FAILED_FINAL `rate_limited`) are simply re-planned next hour. `# ponytail: one sync GET per day per op; switch to async report runs when a call exceeds CallTimeout`.

### 6.3 Advance sweeper `ads_publish_advance_v1` (periodic 30 s, ≤ 50 drafts per run)

For each draft that has a current approval, or a pinned campaign id with `ends_at + 3 days ≥ today`, or a
pinned campaign that AD6 does not yet confirm non-spending, or any non-terminal op — **regardless of derived
status (incl. UNKNOWN, PAUSED, REJECTED)**: pin remote ids from SUCCEEDED ops, plan the next step
(`ads:<draft>:<attempt>:<kind>:<seq>`, Plan idempotent by key), ingest insights reads, plan
pause retries (§5.3) and the ENDED pause. It never plans `activate` (any seq) for a draft that has any pause op or `ended_at`; a paused draft never re-activates; resuming = the merchant copies it into a new draft (new id, fresh approval and preflight; ruling X7). If an activate op reaches SUCCEEDED/UNKNOWN after the latest pause op was claimed, it plans pause seq+1. `activate` (any seq, incl. 1) is planned only when the AD6 sum, including activates already planned earlier in the same run/transaction, plus this draft ≤ `max_active_budget_minor`; drafts are processed in `approved_at` order. Holds no lock across I/O; no network. Planner definers take the
River job id inserted by the caller in the same transaction (pattern `integration.plan_claim_reply`, 0064:829).

### 6.4 CAPI sweeper `capi_purchase_sweep_v1` (periodic 5 min, ≤ 200 attempts)

Selects CAPTURED facts `f` in stores `s` with `capi_enabled`, **`f.environment = s.environment` and
`f.execution_profile <> 'PROVIDER_MOCK'`**, whose order's buyer owner has
`consent_allows(...,'ads_personalization','meta_ads')` and an `ads.capi_contexts` row captured ≤ 7 days
ago, without an `ads.capi_events` row, with `received_at ≥ now−6 days`; plans one op (actor MERCHANT,
principal = `s.capi_enabled_by`) + `capi_events` row in one transaction (consent checked in that
transaction, CD5). Event: `event_name=Purchase`, `event_time` = fact `received_at`,
`action_source=website`, `event_source_url` = storefront origin + `/orders`, `user_data`: `ph` (E.164,
SHA-256, omitted if not normalizable), `external_id` = SHA-256(hex(HMAC(store key, buyer_owner_id))),
`client_user_agent` from `ads.capi_contexts`; `custom_data`: `currency`, `value` = amount_minor/100
(exact decimal), `content_type=product`, `contents[{id: variant public id, quantity}]` (AD10). SANDBOX
stores always add `test_event_code`. Also deletes `ads.capi_contexts` rows older than 8 days or whose
consent is no longer allowed.

## 7. HTTP (Go private API; admin BFF mirrors under `/api/admin/`, buyer BFF under `/api/buyer/`)

| Route | Permission | Notes |
| --- | --- | --- |
| `POST /v1/merchant/ads/meta/connect` | `ads:manage`+`integration:manage` | returns dialog URL; command receipt |
| `GET /v1/merchant/ads/meta/callback` | session + state (principal+store must match, else `state_mismatch`) | excluded from URL logging; 303 to admin page; fixed error codes |
| `POST /v1/merchant/ads/meta/bindings` | same | `{state_id, ad_account_id, dataset_id?}` ∈ pick list, ≤10 min |
| `GET /v1/merchant/ads/drafts[/{id}]` | `ads:read` | derived status, remote ids, op states, Ads Manager deep link |
| `POST /v1/merchant/ads/drafts`, `PUT …/{id}` | `ads:manage` | `Idempotency-Key` (`command.Run`); PUT needs `If-Match: <revision>` |
| `POST …/{id}/approve` | `ads:approve` | body `{revision}`; 409 on revision/allowance/`billing_restricted` |
| `POST …/{id}/publish` | `ads:approve` | body `{publish_attempt}` CAS, `Idempotency-Key`; 409 `attempt_changed` / `prior_attempt_not_paused` / `billing_restricted` |
| `POST …/{id}/pause`, `…/end` | `ads:manage` / `ads:approve` | plans ops |
| `GET /v1/merchant/ads/report?from&to` | `ads:read` | three separate blocks (§15.5): orders net of refunds, Meta spend/impressions/clicks, Meta-reported purchases/value; each with window, timezone, `fetched_at` |
| `PUT /v1/merchant/ads/capi` | `ads:manage` | via `ads.set_capi` only |
| `GET /feeds/meta.csv` | public | on the verified storefront host, via `ads.feed_rows(origin)` (which calls `buyer.resolve_published_store`, 0020); no store parameter, no signature; only published products/variants; cache key = host+store+catalog revision |
| buyer consent PUT (customers-billing) | buyer | + calls `ads.put_capi_context` with the request `User-Agent` after an `ads_personalization` grant |

## 8. Idempotency and uniqueness

Ops: semantic keys §6.2/§6.3, `ads:capi:<attempt>`; remote creates: name tag `lc-<op uuid>` + reconcile
(AD3); status ops idempotent by nature and never FAILED_FINAL; reads repeatable; CAPI `event_id` stable
per attempt, one op per attempt, never resent after UNKNOWN; approval PK (draft, revision); publish
attempt CAS; OAuth state single-use. No op ever changes a budget of an existing remote object in v1.

## 9. Test gates (all NOT_RUN)

| Gate | Tier | Proves |
| --- | --- | --- |
| MA01 | UNIT | `metaBudget` TWD/USD/HKD/other, TWD `12345` → `ErrNotWholeUnit`; `spendMinor` vectors (`"0"`, `"12.3"`, `"12.34"`, `"12.345"`→error, `"-1"`→error, `"1e3"`→error); canonical draft hash; name tag; phone E.164 + F16 vectors; event_id; Graph error classification incl. activate/pause never FAILED_FINAL; `provider_reference` result grammar round-trip |
| MA02 | REAL_PG | privilege delta equals §4.4; RLS cross-store zero rows; approval needs `ads:approve`; edit after approval invalidates; allowance under concurrent approvals; ENDED with pause UNKNOWN still counts; two drafts whose activates are planned in one sweeper run: the second activate Check → BLOCKED_POLICY `over_allowance` while the first is DISPATCHING; activate UNKNOWN still counts; a pause created before the last activate does not release; RESTRICTED store (fixture read as `commerce_ads_writer`): approve/publish 409 `billing_restricted`, create/activate BLOCKED_POLICY zero HTTP, pause/insights/CAPI allowed; ads-writer read/insert policies on `integration.operations` present (removing one turns the gate red); mixed-currency draft refused; TWD non-whole budget refused by CHECK; merchant cannot change environment/allowance; pause SUCCEEDED while activate READY → activate BLOCKED_POLICY `pause_requested`; pause SUCCEEDED while activate DISPATCHING → allowance still counts the draft, and after the activate completes the sweeper plans pause seq+1; two over-allowance drafts ready together → exactly one activate planned; removing the `integration.operation_events` read policy turns the gate red |
| MA03 | REAL_PG | consent via `consent_allows`: none / withdrawn / erased → no CAPI op, context deleted; withdraw after plan → Check BLOCKED_POLICY `consent_withdrawn`, zero HTTP (G09); SANDBOX fact in a LIVE store → no op; `capi_enabled_by` revoked → `capi_principal_revoked` |
| MA04 | MOCK (fake Graph + REAL_PG) | full chain: campaign PAUSED + ad set/ad ACTIVE → preflight → activate; activate refused without approval, stale hash, over allowance, stale preflight, SANDBOX env with non-sandbox account (creates too; zero HTTP); pause allowed in SANDBOX |
| MA05 | MOCK | UNKNOWN at each create (timeout after remote create) → reconcile by tag pins one object, zero second POST; >1 match → review; rate-limited pause → UNKNOWN → reconcile → next seq, never terminal — the draft is UNKNOWN because of the pause op and the sweeper still plans pause seq+1 after 15 min; child-process kill after send; the pause-vs-activate race cases of MA02 through the mock dispatcher |
| MA06 | MOCK | binding re-pointed to another ad account → old ops STALE_BINDING/UNKNOWN, no call on the new account (G10); concurrent re-publish → one attempt, other 409 |
| MA07 | MOCK | insights reads: planning keys for UNKNOWN/PAUSED/REJECTED drafts too, ingestion of every SUCCEEDED read regardless of derived status (WITH_ISSUES still delivering → auto-pause fires), 28-day finalization, auto-pause at 100 % spend, rate-limited read re-planned, no auto budget change |
| MA08 | MOCK | CAPI body exact keys; user data only via `ads.capi_user_data` (stale generation / expired lease / non-CAPI op → refused; `commerce_worker` has no direct SELECT on `storefront.destination_snapshots`); `test_event_code` iff SANDBOX; hashed fields never in PG/logs (sentinel scan); UNKNOWN never resent |
| MA09 | BROWSER | admin connect (fake OAuth server), `state_mismatch`, pick outside list refused, draft→approve→publish→pause, report three blocks; buyer consent grant → context row, withdraw → none |
| MA11 | MOCK + REAL_PG | A-10: Reconcile of UNKNOWN `create_ad` and UNKNOWN `pause` loads the token via the reconcile claim; loader refuses a stale generation / expired lease / wrong token / page-token action; no loader call path from Check (static + runtime); `load_meta_page_token` still returns zero ads rows; `cmd/api` binary contains no HPKE private-key loader; UNKNOWN activate reconciled 11 min after its preflight, with the store RESTRICTED → Reconcile GET runs and pins SUCCEEDED; UNKNOWN create_ad reconciled after the approver lost ads:approve → pinned; `Mode` is `"dispatch"`/`"reconcile"` per claim and existing routes' tests pass unchanged |
| MA-S1 | SANDBOX | 大梦's own sandbox ad account: create campaign PAUSED, ad set/ad ACTIVE (effective `CAMPAIGN_PAUSED`, zero delivery), activate (ad effective ∈ {ACTIVE, IN_PROCESS, PENDING_REVIEW}), pause, reconcile list; closes U1/U2/U8/U9 |
| MA-S2 | SANDBOX | BOOST_POST with owner test Page post and IG media; closes U3/U4 |
| MA-S3 | SANDBOX | FLfB login on app 大梦 by an app-role user in dev mode → BISU token, `client_business_id`, `/me/permissions`, ad accounts, datasets; closes A-8/U7/U10 |
| MA-S4 | SANDBOX | CAPI Purchase with `test_event_code` on the test dataset in the 香港大碗 business → 200, visible in Test Events |
| MA-L1 | LIVE (owner-own) | owner's own funded ad account, budget = owner answer to O5, owner chat approval per activation; insights against real delivery |
| MA-L2 | LIVE (third-party) | a non-role merchant connects and publishes — only after App Review + 香港大碗 Business Verification + Access Tier (§11) |
| MA10 | REVIEW | independent test_worker + security_reviewer; full `go test -race ./...`, `go vet`, `python3 scripts/check_packet.py` |

What each tier cannot prove: MOCK — Meta enum validity, review/delivery, rate limits; SANDBOX —
insights (F20), delivery, ad review, spend behaviour, third-party permissions; LIVE owner-own —
third-party (non-role) access.

## 10. Ownership (PROCESS §2)

A-10 dispatcher change: `integration_worker` on `internal/integrations/core/**` as its own unit, frozen
and merged before T15. T15 `integration_worker`: `internal/ads/**`, `internal/integrations/meta_ads/**`,
`tests/ads/**`, `cmd/ads-worker/**`, migrations 0074–0075 (integrator merges). T16 `integration_worker`:
`internal/attribution/**` (CAPI sweeper, feed), 0080, the A-3 hook line. UI `ui_worker`: admin ads pages,
buyer consent control. Test author ≠ implementer; security_reviewer on token custody, OAuth callback,
consent.

## 11. What needs Meta App Review / owner action on app 大梦 `4291253377792879` (engineering cannot close)

| Item | Needed for | Evidence tier |
| --- | --- | --- |
| Attach app 大梦 to 香港大碗貿易有限公司's Business Portfolio (O-C); verify contact email | everything below | owner |
| **Business Verification** of the 香港大碗 portfolio | prerequisite for Advanced Access | owner |
| Privacy policy, terms, data-deletion URL, app icon; OAuth redirect URI on the production admin domain (`xgdwm.com` subdomain); app switched to Live mode (may also be needed for creatives, U9) | App Review + FLfB | owner + deploy |
| Business app use cases: **FLfB** + a configuration (BISU, assets: ad account, Page, IG, dataset) and Marketing API | connect flow | SANDBOX (role users, dev mode) → LIVE after review |
| 大梦's own sandbox ad account; test dataset in the 香港大碗 business (ruling O7) | MA-S1..S4 | owner |
| **Advanced Access (App Review)**, one submission after MA-S1..S4 (ruling O3): `ads_management`, `ads_read`, `pages_show_list`, `pages_read_engagement`, `pages_manage_ads`, `instagram_basic`, `business_management` (A-8); FLfB for tech providers | any non-role merchant | LIVE only |
| **Marketing API Access Tier: Full** (≥500 calls/15 d, <15 % errors, F1/F2) | production rate limits | LIVE; Limited suffices for SANDBOX/owner-own |
| Secret files on the server (O-D): app secret, HPKE keypair | runtime | owner supplies files |
| Not requested: `catalog_management` (AD10), `instagram_content_publish`, `pages_messaging` (other contracts) | — | — |

## 12. Known limits / NOT_RUN

- Budget is not a real-time hard stop (§15.3): lifetime budget + campaign spend cap are Meta-side bounds; our auto-pause lags ≥ 15 min (F13) plus the hourly/daily read cadence.
- No Live-video-specific promotion (F12); merchants boost the Live's post after it exists (U4) or use Ads Manager.
- Sandbox has no insights (F20): reporting is MOCK until MA-L1.
- CAPI UNKNOWN events may be lost (AD8); no pixel → lower match quality (upgrade signal: Event Match Quality < 6 → add pixel + `fbc`).
- Read results are limited to 255 chars (§3 ponytail).
- Retention class for `ads.insights_daily`/`ads.capi_events` and erasure of `ads.capi_contexts`: register with T14 (customers-billing CD7), blocks production mount.
- Every MA gate NOT_RUN.

## 13. Owner questions (genuine owner inputs only; each with the recommended default)

- **O5** Owner-own LIVE probe budget (MA-L1). Default: NT$300 lifetime, 1 day, boosting the owner's own Page post, each activation approved in chat.

## Round-1 review disposition (r2-design-wave.json `ads`)

| Finding | Disposition |
| --- | --- |
| P0 token path to Check/Reconcile/insights | Fixed at the root: AD11, A-10 (§3.1), preflight + insights as read ops, loader state/lease pairs, MA11. Verified: `dispatcher.go` runs `LoadSecret` only when `claim.Mode == "dispatch"`; reconcile claims already hold generation + lease token (0008 CHECK). |
| P1 PAUSED children never deliver | Fixed (AD3, MA-S1); F9/F22 re-fetched 2026-09-30. |
| P1 allowance release / currency | Fixed (AD6, `allowance_currency`, MA02). |
| P1 activate/pause classification, re-publish | Fixed (§3, §5.3, MA05/MA06). |
| P1 registrar principal / pick list / decrypt claim | Fixed (§4.1 hash-authenticated registrar, `oauth_states` pick list + sealed pending token, `state_mismatch`, A-4 HPKE). |
| P1 SANDBOX facts into LIVE dataset | Fixed (§6.4 environment + `PROVIDER_MOCK` filter, `capi_test_event_code`, MA03). |
| P1 0076 before 0078; `principal_holds` grant | Fixed: CAPI migration → `0080_meta_capi.sql` with the grant (A-11: 0080 is outside this branch's 0060–0079 range, integrator allocates); `principal_holds` grant in 0074 (0064:77 verified). |
| P1 CAPI op actor | Fixed (`capi_enabled_by`, §6.1/§6.4). Verified: `operation_actor_family` MERCHANT requires `principal_id IS NOT NULL` (0061:82). |
| P2 feed scope | Fixed (§7, `buyer.resolve_published_store`). |
| P2 merchant writes to store_settings | Fixed (`ads.set_capi`). |
| P2 TWD truncation | Fixed (CHECK + `ErrNotWholeUnit`, MA01). |
| P2 SANDBOX creates in real account | Fixed (AD9). |
| P2 callback logging / redirect_uri / client_business_id | Fixed (§2 step 2, U10, `ads.connections`). |
| P2 concurrent re-publish | Fixed (§5.3, §7). |
| P2 privilege-delta table | Fixed (§4.4). |
| P2 page loader lacks provider filter | **Rebutted in part:** `load_meta_page_token` already filters `action='meta.private_reply'` and the op's own binding/provider (0064:903–917), so it cannot return an ads row; no change to 0064's function. The ads loader gets the provider + action filter as asked. |
| P2 spend decimal parse | Fixed (`spendMinor`, MA01). |

## Round-2 review disposition (2026-09-30)

| Finding | Disposition |
| --- | --- |
| P1 AD6 frees allowance for DISPATCHING/UNKNOWN activates | Fixed with the reviewer's text, one correction: the dispatcher **claims** an op (DISPATCHING) *before* running `Check` (`dispatcher.go:192` claim, `:226` Check, `:238` BLOCKED_POLICY), so "never claimed for dispatch" is always false for a checked op; AD6 therefore uses the reviewer's parenthetical as the rule (every activate absent or ended `BLOCKED_POLICY`, which the dispatcher sets only pre-send at `:238/:258/:279`). READY activates also count. Self-count added; pause must be created after the last activate of any state; §5.3 uses the same test; MA02 extended. Liveness note: two concurrent activates may both block (fail closed); the sweeper re-plans only when the sum has room. |
| P1 jobs select drafts by derived status | Fixed as proposed (§5.1 display-only, §6.2 any pinned campaign, §6.3 approval/pinned/not-yet-non-spending/non-terminal op, MA05, MA07). |
| P1 billing RESTRICTED not enforced for ads | Fixed (§5.2, §2 steps 6–7, §6.1, §7, §4.3 grant in 0080 after 0079, §4.4, MA02). Verified `customers-billing-v1.md` BD5 (line 44), `store_standing` EXECUTE `commerce_auth` only (line 263), `USAGE ON SCHEMA billing` only billing_writer/auth (line 225) → schema USAGE added too. Added: MA02 reads RESTRICTED as `commerce_ads_writer` (invoker + RLS would fail open). |
| P1 §4.4 missing grants | Fixed (§4.4, §4.3). Verified each: (a) planners need INSERT + INSERT policy + sequence USAGE (0016:85–95, 0035:116–123); additionally the existing `SELECT` row had **no RLS read policy** — added (zero rows would make the allowance sum fail open). (b) `resolve_access` EXECUTE only at 0003:45, 0035:163, 0060:322, 0062:260 — added for integration_writer, plus schema `ads` USAGE + policies. (c) `resolve_published_store` EXECUTE only `commerce_buyer_issuer` (0020:85) — feed now goes through definer `ads.feed_rows(origin)` owned by `commerce_ads_writer` (store resolved inside, never from the caller). (d) `storefront.destination_snapshots` SELECT only `commerce_buyer_runtime` (0012:103) — lease-fenced `ads.capi_user_data`. Also added schema `ads` USAGE for every caller role. |
| P0 (round 1) token path | Still closed at the root (AD11, §3.1 A-10, MA11); the new `ads.capi_user_data` uses the same lease fence, and the billing check is PG-only in Check. |
| O-C retarget | Already applied in round 1 (§0, §11, rulings); Daerdo `1299693313226979` appears only as an explicit exclusion and as history for risk U9. |

## Round-3 review disposition (2026-09-30, `r2-round3.json` key `ads`; rulings X1/X2)

| Finding | Disposition |
| --- | --- |
| P1 pause vs activate race | Fixed with the reviewer's exact text (X1): §6.1 activate Check `pause_requested`, §6.3 no activate after any pause/`ended_at` + pause seq+1 after a late activate, AD6/§5.3 pause-claim order via `integration.operation_events` (0008:68; §4.4 read grant + policy added), MA02/MA05. Verified: pause runs in the priority lane (§2 step 8, §6 header). |
| P1 reconcile-mode Check | Fixed per X2 (additive `DispatchRequest.Mode`, §3.1, §6.1, MA11). Verified `dispatcher.go`: `DispatchRequest` has no mode (:29–43); `Check` runs for any claimed op (:226); reconcile-mode denial → `completeAmbiguous('policy_check_failed')` (:236–240); `LoadSecret` dispatch-only (:267). external-dispatcher-v1 amendment note added. |
| P2 same-run activates both blocked | Fixed with the reviewer's text (§6.3 room test incl. seq 1, `approved_at` order; MA02). |

## Integrator rulings (2026-09-29)

Recorded from `docs/delivery/units/r2-design-rulings.md` (binding; owner may revise before go-live):
- **O-C** Meta app = 大梦 `4291253377792879` under 香港大碗貿易有限公司's Business Portfolio; not Daerdo's `1299693313226979`. Business Verification and App Review are owner steps. Supersedes round-0 O1 (all use cases on app 大梦) and O2 (verify 香港大碗, not Daerdo).
- **O-D** Production operated by Claude on the owner's server; secrets (Meta app secret, HPKE key files) supplied as files, never in chat.
- **O3** One App Review submission after sandbox gates MA-S1..S4 pass.
- **O4** Per-store ad allowance default NT$0 (ads off) until the operator sets it (`max_active_budget_minor` default 0).
- **O6** Purchase-events disclosure: the `ads_personalization` checkbox copy names "sending purchase events to Meta for ad measurement", unchecked by default (U6 legal review still open).
- **O7** Test dataset for MA-S4 is created in the 香港大碗 business.
- **O8** Automatic placements (Meta chooses); no placement picker in v1.
- **Billing Q6** arrears (RESTRICTED) restricts new claim windows and new ads only: here approve/publish/create/activate (§5.2); pause, reads and CAPI never blocked. **Q4** pilot stores UNBILLED = unrestricted. **Q12** consent channel `meta_ads` / purpose `ads_personalization` is the CAPI gate (AD8).
- Still open with the integrator (not owner questions): A-1..A-11 in §0.1. Still open with the owner: O5 (§13).
- **Round-3 rulings (2026-09-30)** from `docs/delivery/units/r2-design-rulings.md` "Round-3 integrator rulings": X1 round-3 P1s applied with the reviewer's exact text + cheap P2; X2 reconcile-mode Check via additive `DispatchRequest.Mode`; X6 contract FROZEN at v1 2026-09-30, remaining owner questions keep their defaults and do not block implementation.


## Integrator amendment (2026-09-30, brief rulings B9, B12–B15 in docs/delivery/units/r2-design-rulings.md)
- `billing_restricted` on approve/publish is HTTP 402 (was 409), shared with customers-billing-v1.
- §4.4 adds: `ads.operator_set_settings` EXECUTE → commerce_meta_registrar; `payments.refund_facts` SELECT + read policy (store-scoped) → commerce_ads_writer. MA02 asserts both.
- A10-D2 (coded denials) and A10-D3 (queue-aware job insert) are recorded in external-dispatcher-v1.
- D2 read-result cap 200 chars; D5/D6/D9 accepted; ads-core mount and MA02 billing clauses wait for 0080.

## Integrator merge notes (2026-09-30, lane r2/ads: ads-core + ads-graph + ads-ui)
Recorded from the unit hooks; implemented in 0074/0075/post_river 0015 and asserted by MA02 (ads-tests).
- §4.4 extra rows: R-C commerce_ads_writer column SELECT + read policy on control.storefront_domains (ACTIVE),
  control.storefront_publications (published), catalog.products (active) for §5.2 PRODUCT_TRAFFIC validation and the
  D11 link freeze; R-E commerce_ads_writer river USAGE + river.river_job SELECT (planned-job verification, 0014 pattern);
  R-F commerce_integration_writer ads.connections UPDATE(token_version,oauth_state_id,connected_by,connected_at) +
  ads.oauth_states UPDATE(used_at) WITH CHECK (false); R-G EXECUTE of the merchant definers → commerce_runtime, the
  check/advance/insights/purge definers + integration.load_meta_ads_token → commerce_worker. R-D: operator audit lives
  in ads.operator_events (no principal for ops.audit_events).
- §4.1 drift: ads.connections carries `token_version bigint NOT NULL` (mirror of the head version, read by
  ads.connection_version) so commerce_runtime needs no grant on the token head table.
- §3 read-result grammar key length is {2,4} (the preflight key `fund` has 4). 0074 reads preflight keys by substring
  (accepts `fund`); 0075's insights parser keeps {2,3}, which fits every insights key (es,sp,im,cl,pu,pv,cur,tz).
- `metaads.Routes` takes the `metaads.TokenOpener` interface (not `*tokenopen.Keyring`) so cmd/api never links the
  HPKE private-key loader (MA11, `go list -deps ./cmd/api`).
- G3 secret files: lcentry expands `NAME_FILE` to `NAME`; `metaads.SecretFromEnv` accepts either (both = error).
  The public HPKE ring is derived from the private ring by deploy/scripts/secrets-init.sh (kinds hpke_private_ring /
  hpke_public_ring); preflight P03/P04/P05 check both and their id sets.
- §7 error body is the shared internal/httperror shape `{code,...}` (the ads-core brief's `{"error":code}` was wrong);
  OpenAPI for §7 lives in contracts/ads-openapi.json (core-openapi.json keeps its fixed operation count).
- Admin BFF transport: pause/end/connect are forwarded with no body and approve with no Idempotency-Key, as §7 freezes.
- B15 is enforced at deploy time: preflight P06 fails `COMMERCE_META_ADS_APP_ID` without migrations/0080 and
  without the `ads` profile (pause must always reach ads-worker).

## Lane close amendments (2026-09-30, r2/ads close pass; smallest notes, contract text above is otherwise unchanged)
- **§5.3 pause vs disabled binding (R2-ADS-PAUSE-1, a5ad682):** "Pause is always allowed ... disabled-but-same-asset binding" is narrowed to
  "with an ENABLED binding". The dispatcher's enabled/semantic_version gate is unchanged; trigger `0074 bindings_ads_disable_guard` refuses
  (PT409 `binding_in_use`, core maps it to a 409 conflict) disabling a `meta_ads` binding while any draft on it counts by the AD6 test.
  Today no HTTP route or admin page calls `core.Service.SetBindingEnabled`, so no merchant sees this refusal; the copy
  ("pause the campaign first, then disconnect") is added with the route that first exposes a disconnect.
  Operator path for a stuck draft (pause can never reach SUCCEEDED): docs/runbooks/deploy.md §6.4 items 6-7 (no override exists; the binding frees at
  `ends_at + 1 day` by design).
- **§5.3 publish after pause (X7 consequence):** `ads.publish_draft` refuses (422 `invalid_request`, zero ops) a draft that has any `pause` op,
  exactly like an ended draft. A re-publish could never activate (check_activate `pause_requested`), so the merchant copies the draft instead (X7).
- **§4.3 C4 event_time:** `event_time = floor(extract(epoch FROM received_at))` (the first 0080 cast rounded, up to 0.5 s ahead of the fact).
- **§3 creative request:** `page_id` is optional for a BOOST_POST of an Instagram media when the store has no Facebook Page binding
  (`ads.request_for` omits the key); a Facebook boost and PRODUCT_TRAFFIC still require it (the adapter refuses them locally, zero HTTP).
- **§4.3/§4.4 drift recorded, implementation is the authority:** 0080 reads `storefront.quotes(snapshot)` by `quote_id` (not
  `destination_id`/`destination_snapshots(phone)`); the §4.4 tables also list `river.river_job` SELECT + river USAGE (R-E), `control.storefront_domains`,
  `control.storefront_publications`, `catalog.products.status` (R-C), as the merge notes above. §6.4/MA08 `ph`: CAPI omits `ph` (0080 `ph_e164` is NULL, column not granted) until checkout records recipient = buyer, as customers-billing CD5 requires; MA08 asserts the omission.
- **§7 error key:** the wire key is `code` (shared `internal/httperror` shape), not `error`.
- **OAuth state (§2 step 1):** the state is an HMAC-SHA256 keyed by a key derived from the Meta app secret (server-side only) over
  tenant|store|principal|Idempotency-Key, so `ops.command_results` (which keeps the key) cannot be used to recompute a live state; only its SHA-256 is stored.
  Replaces the unkeyed SHA-256 derivation (r3 review P2); the state is still single-use, 10 min, bound to principal + store.
- **Provisioning (A-1):** `scripts/ops/grant-ads-permissions.sql` grants `ads:read`, `ads:manage`, `ads:approve` to one existing store creator
  (same safety checks as grant-r2-permissions.sql); deploy.md §6.4 item 6.
- **Known, tracked as NOT_RUN/BLOCKED until MA-S1 (tasks.json T15):** auto-pause (AD7) covers budget reached and DISAPPROVED/WITH_ISSUES only,
  not an ad account that is no longer ACTIVE; the min-daily-budget x days check of §5.2 is not enforced locally (Meta rejects the ad set,
  the attempt FAILS and can be re-published).
